package app

// Retention enforcement in the Shell composition. The durable compositions
// run one sweeper at startup and on a fixed interval. It discovers ownership
// scopes from the cross-scope controller's execution read (tenant and project),
// then applies installation.RetentionPolicy through the PostgreSQL retention
// adapter.
//
// Reconstructing the scope set from durable rows each pass means a restarted
// controller resumes sweeping without any in-memory state. A scope with no
// execution is simply not seen; it has no execution-derived evidence or audit
// row to expire, and a later execution makes it visible. A scan or sweep
// failure is logged by fault class and retried on the next interval, never
// turned into a partial guess.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	// DefaultRetentionInterval is the composition sweep cadence. Retention
	// windows are measured in days, so one pass per day bounds the work without
	// delaying a deadline by more than one interval.
	DefaultRetentionInterval = 24 * time.Hour
	// retentionScopePage bounds one controller execution page.
	retentionScopePage = store.MaxQueryLimit
)

// retentionSweeper is the pure installation.RetentionSweeper surface the loop
// consumes. It is an interface so the composition loop is testable without a
// database.
type retentionSweeper interface {
	Sweep(context.Context, []installation.RetentionScope, time.Time) (installation.RetentionSweepResult, error)
}

// retentionScopeReader lists the ownership scopes that may hold retained rows.
type retentionScopeReader interface {
	scopes(context.Context) ([]installation.RetentionScope, error)
}

// retainedCommitReader returns the commit names carried by the retained
// evidence rows in one project scope. The retention store implements it; the
// loop never invents a window of its own.
type retainedCommitReader interface {
	RetainedCommits(context.Context, installation.RetentionScope, time.Time) ([]string, error)
}

// retainedCommitKeeper reconciles one tenant's managed keep refs with the
// authoritative tenant-wide retained set.
type retainedCommitKeeper interface {
	KeepRefs(context.Context, string, []string) error
}

// controllerRetentionScopes derives the sweep scope set from the cross-scope
// controller's execution read. The controller may read executions for every
// tenant without widening the runtime role.
type controllerRetentionScopes struct {
	controller store.Controller
	// tenants are the explicitly configured production tenants. They let the
	// sweep reach tenant-wide audit rows even before a tenant's first execution
	// makes it discoverable through the controller read.
	tenants []id.TenantID
	// projects are the explicitly configured model-gateway project scopes. They
	// let the sweep reach raw-content captures written by non-execution
	// model.invoke calls before a project runs its first execution.
	projects []tenancy.Scope
}

func (source controllerRetentionScopes) scopes(ctx context.Context) ([]installation.RetentionScope, error) {
	if source.controller == nil {
		return nil, fault.New(fault.InternalFailure, "retention scope reader requires a controller")
	}
	seen := make(map[installation.RetentionScope]bool)
	scopes := make([]installation.RetentionScope, 0)
	add := func(tenant, project string) {
		if tenant == "" {
			// A row without a tenant cannot be swept safely. The controller
			// never produces one; skip rather than invent a scope.
			return
		}
		for _, candidate := range []installation.RetentionScope{
			{TenantID: tenant, ProjectID: project},
			{TenantID: tenant},
		} {
			if seen[candidate] {
				continue
			}
			seen[candidate] = true
			scopes = append(scopes, candidate)
		}
	}
	for _, tenant := range source.tenants {
		add(string(tenant), "")
	}
	for _, project := range source.projects {
		if project.ProjectScoped() {
			add(string(project.TenantID), string(project.ProjectID))
		}
	}
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := source.controller.Scan(ctx, store.ControllerScan{
			Table:   "executions",
			OrderBy: []store.Order{{Column: "id"}},
			After:   cursor,
			Limit:   retentionScopePage})
		if err != nil {
			return nil, err
		}
		for _, row := range page.Rows {
			tenant, _ := row.Values["tenant_id"].(string)
			project, _ := row.Values["project_id"].(string)
			add(tenant, project)
		}
		if !page.HasMore {
			break
		}
		if page.Next == "" || page.Next == cursor {
			return nil, fault.New(fault.InternalFailure, "retention scope scan returned a non-advancing cursor")
		}
		cursor = page.Next
	}
	sort.Slice(scopes, func(left, right int) bool {
		if scopes[left].TenantID != scopes[right].TenantID {
			return scopes[left].TenantID < scopes[right].TenantID
		}
		return scopes[left].ProjectID < scopes[right].ProjectID
	})
	return scopes, nil
}

// retentionLoop owns the startup and periodic retention sweep.
type retentionLoop struct {
	sweeper  retentionSweeper
	scopes   retentionScopeReader
	commits  retainedCommitReader
	keepRefs retainedCommitKeeper
	clock    Clock
	interval time.Duration
	logger   *slog.Logger
}

type retentionLoopConfig struct {
	Sweeper  retentionSweeper
	Scopes   retentionScopeReader
	Commits  retainedCommitReader
	KeepRefs retainedCommitKeeper
	Clock    Clock
	Interval time.Duration
	Logger   *slog.Logger
}

func newRetentionLoop(config retentionLoopConfig) (*retentionLoop, error) {
	switch {
	case config.Sweeper == nil:
		return nil, fault.New(fault.InvalidInput, "retention loop requires a sweeper")
	case config.Scopes == nil:
		return nil, fault.New(fault.InvalidInput, "retention loop requires a scope reader")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "retention loop requires a clock")
	case config.KeepRefs != nil && config.Commits == nil:
		return nil, fault.Wrap(fault.InvalidInput,
			"retention loop keep refs require retained commits", ErrKeepRefsWithoutRetainedCommits)
	case config.Interval <= 0:
		return nil, fault.New(fault.InvalidInput, "retention loop requires a positive interval")
	case config.Logger == nil:
		return nil, fault.New(fault.InvalidInput, "retention loop requires a logger")
	}
	return &retentionLoop{
		sweeper:  config.Sweeper,
		scopes:   config.Scopes,
		commits:  config.Commits,
		keepRefs: config.KeepRefs,
		clock:    config.Clock,
		interval: config.Interval,
		logger:   config.Logger,
	}, nil
}

// run sweeps once at startup and then on the configured cadence until
// cancellation. A failed pass is logged by class and retried; it never stops
// the loop or another scope's progress.
func (loop *retentionLoop) run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := loop.sweep(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			loop.logger.Warn("retention sweep failed", "class", loopFailureDetail(err))
		}
		if err := loop.clock.Sleep(ctx, loop.interval); err != nil {
			return err
		}
	}
}

func (loop *retentionLoop) sweep(ctx context.Context) error {
	now := loop.clock.Now()
	if now.IsZero() {
		return fault.New(fault.InternalFailure, "retention clock returned zero time")
	}
	scopes, err := loop.scopes.scopes(ctx)
	if err != nil {
		return err
	}
	if len(scopes) == 0 {
		return nil
	}
	_, err = loop.sweeper.Sweep(ctx, scopes, now)
	keepErr := loop.reconcileKeepRefs(ctx, scopes, now)
	return errors.Join(err, keepErr)
}

// reconcileKeepRefs keeps the commits named by retained executions before a
// garbage collection can prune them. It reads the authoritative retained
// commits from the same retention store the sweep deletes through and
// aggregates a whole tenant before reconciling, so a commit retained by one
// project is never released because another project's set omitted it. A failed
// read skips that tenant's sweep entirely: uncertainty keeps refs, it never
// releases them.
func (loop *retentionLoop) reconcileKeepRefs(ctx context.Context, scopes []installation.RetentionScope, now time.Time) error {
	if loop.commits == nil || loop.keepRefs == nil {
		return nil
	}
	retained := make(map[string]map[string]bool)
	failed := make(map[string]bool)
	var failures []error
	for _, scope := range scopes {
		if scope.ProjectID == "" {
			// Evidence is project-scoped; the tenant-wide scope exists for
			// audit rows and has no commit evidence of its own.
			continue
		}
		commits, err := loop.commits.RetainedCommits(ctx, scope, now)
		if err != nil {
			failed[scope.TenantID] = true
			failures = append(failures, fmt.Errorf("retained commits in %s/%s: %w", scope.TenantID, scope.ProjectID, err))
			continue
		}
		set := retained[scope.TenantID]
		if set == nil {
			set = make(map[string]bool)
			retained[scope.TenantID] = set
		}
		for _, commit := range commits {
			set[commit] = true
		}
	}
	tenants := make([]string, 0, len(retained))
	for tenant := range retained {
		tenants = append(tenants, tenant)
	}
	sort.Strings(tenants)
	for _, tenant := range tenants {
		if failed[tenant] {
			continue
		}
		commits := make([]string, 0, len(retained[tenant]))
		for commit := range retained[tenant] {
			commits = append(commits, commit)
		}
		sort.Strings(commits)
		if err := loop.keepRefs.KeepRefs(ctx, tenant, commits); err != nil {
			failures = append(failures, fmt.Errorf("keep refs in %s: %w", tenant, err))
		}
	}
	return errors.Join(failures...)
}

// configuredRetentionTenants returns the production tenant identities the
// composition already knows, so a tenant with audit rows but no execution yet
// is still reachable by the sweep. The durable test profile has no fixed tenant
// list (humans arrive through OIDC), so it relies on execution discovery alone.
func configuredRetentionTenants(config Config) []id.TenantID {
	if config.Production == nil {
		return nil
	}
	if len(config.Production.Tenants) > 0 {
		tenants := make([]id.TenantID, 0, len(config.Production.Tenants))
		for _, tenant := range config.Production.Tenants {
			if tenant.TenantID != "" {
				tenants = append(tenants, tenant.TenantID)
			}
		}
		return tenants
	}
	if config.WorkspacePVC != nil && config.WorkspacePVC.TenantID != "" {
		return []id.TenantID{config.WorkspacePVC.TenantID}
	}
	return nil
}

// hasRecordedModelContent reports whether any configured model opts into raw
// content capture, enabling the raw_content retention family on the sweeper.
func hasRecordedModelContent(models *ModelsConfig) bool {
	if models == nil {
		return false
	}
	for _, model := range models.Models {
		if model.RecordContent {
			return true
		}
	}
	return false
}

// configuredRetentionProjects returns the distinct project scopes declared by
// configured models so raw-content rows from non-execution invocations are
// swept even before a project has an execution row.
func configuredRetentionProjects(config Config) []tenancy.Scope {
	if config.Models == nil || len(config.Models.Models) == 0 {
		return nil
	}
	projects := make([]tenancy.Scope, 0, len(config.Models.Models))
	seen := make(map[tenancy.Scope]bool, len(config.Models.Models))
	for _, model := range config.Models.Models {
		if !model.Scope.ProjectScoped() || seen[model.Scope] {
			continue
		}
		seen[model.Scope] = true
		projects = append(projects, model.Scope)
	}
	return projects
}

package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/store"
)

// requiredDatabaseRoles are the cluster-level roles the reviewed migrations
// create and a database dump references in object ownership and grants. They
// are cluster objects, not database objects: pg_dump records owners and ACLs
// but never creates roles, so a target must already carry them. `kodama
// migrate` is that bootstrap step (migration 0001). Restore refuses
// before pg_restore rather than restoring a database the runtime roles cannot
// use.
func requiredDatabaseRoles() []string {
	return []string{
		store.DefaultOwnerRole,
		store.DefaultRuntimeRole,
		store.ControllerRole,
		store.PublisherRole,
		store.AuthRole,
	}
}

// roleProbeQuery asks one target database which required roles exist. It is a
// read-only precondition check; the names come from the declaration, not
// caller input.
var roleProbeQuery = buildRoleProbeQuery()

func buildRoleProbeQuery() string {
	roles := requiredDatabaseRoles()
	quoted := make([]string, 0, len(roles))
	for _, role := range roles {
		quoted = append(quoted, "'"+role+"'")
	}
	return "SELECT rolname FROM pg_roles WHERE rolname IN (" +
		strings.Join(quoted, ", ") + ") ORDER BY rolname"
}

// RestoreConfig binds one restore driver to its container command executor, its
// host file boundary, the exact artifact destinations and the explicit
// workload identities reconciliation observes.
type RestoreConfig struct {
	Executor  Executor
	Files     Files
	Container string
	Database  string
	Root      string
	Targets   []RestoreTarget
	Workloads []WorkloadIdentity
	Observer  Observer
	Services  []string
}

// RestoreDriver reads one backup's preflight evidence, performs one fenced
// restore and reconciles explicit workload identities. It never enables
// dispatch.
type RestoreDriver struct {
	executor  Executor
	files     Files
	container string
	database  string
	root      string
	targets   []RestoreTarget
	workloads []WorkloadIdentity
	observer  Observer
	services  []string

	mu    sync.Mutex
	armed *armedRestore
}

// armedRestore is the one preflighted identity a Restore may consume. It is
// consumed before any effect, so a retry can never replay the restore.
type armedRestore struct {
	identity string
	evidence installation.BackupEvidence
}

var _ interface {
	Preflight(context.Context, installation.BackupIdentity) (installation.BackupEvidence, error)
	Restore(context.Context, installation.RestorePlan) (installation.RestoreResult, error)
	Reconcile(context.Context) (installation.ReconcileResult, error)
} = (*RestoreDriver)(nil)

// NewRestoreDriver validates one restore configuration. It contacts no
// database, broker or filesystem.
func NewRestoreDriver(config RestoreConfig) (*RestoreDriver, error) {
	if config.Executor == nil {
		return nil, fault.New(fault.InvalidInput, "restore driver requires an executor")
	}
	if strings.TrimSpace(config.Container) == "" {
		return nil, fault.New(fault.InvalidInput, "restore driver requires a container")
	}
	if strings.TrimSpace(config.Database) == "" {
		return nil, fault.New(fault.InvalidInput, "restore driver requires a database name")
	}
	if strings.TrimSpace(config.Root) == "" || !filepath.IsAbs(config.Root) {
		return nil, fault.New(fault.InvalidInput, "restore driver requires an absolute backup root")
	}
	targets := make([]RestoreTarget, 0, len(config.Targets))
	targetNames := make(map[string]bool, len(config.Targets))
	for _, target := range config.Targets {
		if !validName(target.Name) {
			return nil, fault.New(fault.InvalidInput, "restore target name is not a single path component")
		}
		if strings.TrimSpace(target.Root) == "" {
			return nil, fault.New(fault.InvalidInput, "restore target root is empty")
		}
		if targetNames[target.Name] {
			return nil, fault.New(fault.InvalidInput, "restore target name is duplicated")
		}
		targetNames[target.Name] = true
		targets = append(targets, target)
	}
	workloads := make([]WorkloadIdentity, 0, len(config.Workloads))
	for _, workload := range config.Workloads {
		if strings.TrimSpace(workload.ID) == "" {
			return nil, fault.New(fault.InvalidInput, "restore workload identity is empty")
		}
		workloads = append(workloads, workload)
	}
	if len(workloads) > 0 && config.Observer == nil {
		return nil, fault.New(fault.InvalidInput, "restore driver requires an observer for explicit workload identities")
	}
	files := config.Files
	if files == nil {
		files = OSFiles{}
	}
	return &RestoreDriver{
		executor:  config.Executor,
		files:     files,
		container: config.Container,
		database:  config.Database,
		root:      filepath.Clean(config.Root),
		targets:   targets,
		workloads: workloads,
		observer:  config.Observer,
		services:  copyStrings(config.Services),
	}, nil
}

// Preflight reads and validates the actual artifacts and binds exactly one
// complete identity whose keys are present. A set without key material is
// reported but never armed. A different identity cannot retarget an armed
// restore.
func (d *RestoreDriver) Preflight(ctx context.Context, identity installation.BackupIdentity) (installation.BackupEvidence, error) {
	if err := ctx.Err(); err != nil {
		return installation.BackupEvidence{}, err
	}
	if strings.TrimSpace(identity.ID) == "" {
		return installation.BackupEvidence{}, fault.New(fault.InvalidInput, "backup identity is empty")
	}
	if !validName(identity.ID) {
		return installation.BackupEvidence{}, fault.New(fault.InvalidInput, "backup identity is not a single path component")
	}
	evidence, err := d.loadEvidence(ctx, identity.ID)
	if err != nil {
		return installation.BackupEvidence{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.armed != nil {
		if d.armed.identity == identity.ID {
			return d.armed.evidence, nil
		}
		return installation.BackupEvidence{}, fault.New(fault.VersionConflict, "a different backup identity is already armed")
	}
	if evidence.KeysPresent {
		d.armed = &armedRestore{identity: identity.ID, evidence: evidence}
	}
	return evidence, nil
}

// loadEvidence reads the manifest and validates the artifacts it names.
func (d *RestoreDriver) loadEvidence(ctx context.Context, id string) (installation.BackupEvidence, error) {
	setDir := filepath.Join(d.root, id)
	data, err := d.files.ReadFile(ctx, filepath.Join(setDir, "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return installation.BackupEvidence{}, fault.New(fault.NotFound, "backup set has no manifest")
		}
		return installation.BackupEvidence{}, fault.Wrap(fault.DependencyUnavailable, "backup manifest is unreadable", err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return installation.BackupEvidence{}, fault.Wrap(fault.InternalFailure, "backup manifest is not decodable", err)
	}
	if manifest.Format != manifestFormat {
		return installation.BackupEvidence{}, fault.New(fault.InternalFailure, "backup manifest format is not supported")
	}
	if manifest.Identity != id {
		return installation.BackupEvidence{}, fault.New(fault.InternalFailure, "backup manifest names another identity")
	}

	dump, err := d.files.Stat(ctx, filepath.Join(setDir, "database.dump"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return installation.BackupEvidence{}, fault.New(fault.NotFound, "backup set has no database dump")
		}
		return installation.BackupEvidence{}, fault.Wrap(fault.DependencyUnavailable, "backup database dump is unreachable", err)
	}
	if dump.Kind != FileRegular {
		return installation.BackupEvidence{}, fault.New(fault.InternalFailure, "backup database dump is not a regular file")
	}
	if dump.Size != int64(manifest.DatabaseBytes) {
		return installation.BackupEvidence{}, fault.New(fault.InternalFailure, "backup database dump size does not match the manifest")
	}
	for name := range manifest.Sources {
		if !validName(name) {
			return installation.BackupEvidence{}, fault.New(fault.InternalFailure, "backup manifest source name is invalid")
		}
		info, err := d.files.Stat(ctx, filepath.Join(setDir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return installation.BackupEvidence{}, fault.New(fault.NotFound, "backup source is missing")
			}
			return installation.BackupEvidence{}, fault.Wrap(fault.DependencyUnavailable, "backup source is unreachable", err)
		}
		if info.Kind != FileDirectory {
			return installation.BackupEvidence{}, fault.New(fault.InternalFailure, "backup source is not a directory")
		}
	}
	// The configured targets must name exactly the durable families this set
	// captured. A missing target would silently skip state; an extra target has
	// no captured source. Refuse before the binding is armed or any effect runs.
	if err := d.validateTargets(manifest.Sources); err != nil {
		return installation.BackupEvidence{}, err
	}
	return installation.BackupEvidence{
		Identity:          installation.BackupIdentity{ID: manifest.Identity},
		Scope:             manifest.Scope.installationScope(),
		KeysPresent:       manifest.Sources["keys"] > 0,
		Preserved:         manifest.Preserved.installationState(),
		MissingWorkspaces: copyStrings(manifest.Workspaces),
	}, nil
}

// validateTargets requires the configured restore targets to be the same set
// as the durable sources the manifest captured. Without this check a target
// list missing a family would report success while silently skipping it, and a
// target with no captured source would fail only after pg_restore --clean ran.
func (d *RestoreDriver) validateTargets(sources map[string]int) error {
	if len(sources) != len(d.targets) {
		return fault.New(fault.InvalidInput, "restore targets do not match the backup sources")
	}
	targets := make(map[string]bool, len(d.targets))
	for _, target := range d.targets {
		targets[target.Name] = true
	}
	for name := range sources {
		if !targets[name] {
			return fault.New(fault.InvalidInput, "backup source has no configured restore target")
		}
	}
	return nil
}

// Restore consumes the armed binding before any effect, checks every target is
// empty, runs exactly one pg_restore and copies each captured source. It never
// trusts caller booleans, never retries and never enables dispatch.
func (d *RestoreDriver) Restore(ctx context.Context, plan installation.RestorePlan) (installation.RestoreResult, error) {
	if err := ctx.Err(); err != nil {
		return installation.RestoreResult{}, err
	}
	if plan.EnableDispatch {
		return installation.RestoreResult{}, fault.New(fault.InvalidInput, "restore never enables dispatch")
	}
	d.mu.Lock()
	armed := d.armed
	d.armed = nil
	d.mu.Unlock()
	if armed == nil {
		return installation.RestoreResult{}, fault.New(fault.InvalidInput, "restore requires an armed preflight")
	}

	// Refuse before any effect when the target cannot receive the dump's
	// ownership and grants. A missing role would make pg_restore fail midway
	// (after --clean began) or, worse, leave the restored database unusable.
	if err := d.verifyTargetRoles(ctx); err != nil {
		return installation.RestoreResult{}, err
	}

	setDir := filepath.Join(d.root, armed.identity)
	for _, target := range d.targets {
		if err := ctx.Err(); err != nil {
			return installation.RestoreResult{}, err
		}
		if err := d.files.EnsureEmpty(ctx, target.Root); err != nil {
			return installation.RestoreResult{}, err
		}
	}

	dump, err := d.files.ReadFile(ctx, filepath.Join(setDir, "database.dump"))
	if err != nil {
		return installation.RestoreResult{}, fault.Wrap(fault.DependencyUnavailable, "backup database dump is unreadable", err)
	}
	if err := ctx.Err(); err != nil {
		return installation.RestoreResult{}, err
	}
	result, err := d.executor.Exec(ctx, ExecRequest{
		Container: d.container,
		Argv:      []string{"pg_restore", "--clean", "--if-exists", "--dbname", d.database},
		Stdin:     dump,
	})
	if err != nil {
		return installation.RestoreResult{}, fault.Wrap(fault.UncertainExternalEffect, "database restore outcome is unknown", err)
	}
	if result.ExitCode != 0 {
		return installation.RestoreResult{}, fault.New(fault.UncertainExternalEffect, "database restore reported a non-zero exit")
	}

	for _, target := range d.targets {
		if err := ctx.Err(); err != nil {
			return installation.RestoreResult{}, err
		}
		if err := d.files.Restore(ctx, filepath.Join(setDir, target.Name), target.Root); err != nil {
			return installation.RestoreResult{}, err
		}
	}
	return installation.RestoreResult{
		Fenced:                  false,
		Reconciled:              false,
		Detail:                  fmt.Sprintf("restored database and %d durable sources from backup %s", len(d.targets), armed.identity),
		Preserved:               armed.evidence.Preserved,
		Services:                copyStrings(d.services),
		UnrecoverableWorkspaces: copyStrings(armed.evidence.MissingWorkspaces),
	}, nil
}

// verifyTargetRoles refuses when the target lacks a role the dump's ownership
// or ACLs reference. Reading pg_roles is not an effect, and a non-zero psql
// exit is a dependency failure rather than a partially restored database.
func (d *RestoreDriver) verifyTargetRoles(ctx context.Context) error {
	result, err := d.executor.Exec(ctx, ExecRequest{
		Container: d.container,
		Argv: []string{
			"psql", "--no-psqlrc", "--no-align", "--tuples-only",
			"--dbname", d.database, "--command", roleProbeQuery,
		},
	})
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "database role check failed", err)
	}
	if result.ExitCode != 0 {
		return fault.New(fault.DependencyUnavailable, "database role check reported a non-zero exit")
	}
	present := make(map[string]bool)
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			present[name] = true
		}
	}
	missing := make([]string, 0)
	for _, role := range requiredDatabaseRoles() {
		if !present[role] {
			missing = append(missing, role)
		}
	}
	if len(missing) > 0 {
		return fault.New(fault.InvalidInput,
			"restore target is missing required database role(s): "+strings.Join(missing, ", "))
	}
	return nil
}

// Reconcile observes only the explicit workload identities. A running, unknown
// or unobservable effect is unresolved and blocks dispatch; reconciliation
// never launches or replays anything.
func (d *RestoreDriver) Reconcile(ctx context.Context) (installation.ReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return installation.ReconcileResult{}, err
	}
	result := installation.ReconcileResult{Services: copyStrings(d.services)}
	unresolved := make([]string, 0)
	for _, workload := range d.workloads {
		if err := ctx.Err(); err != nil {
			return installation.ReconcileResult{}, err
		}
		observation, err := d.observer.Observe(ctx, workload)
		if err != nil {
			unresolved = append(unresolved, workload.ID)
			continue
		}
		switch observation.State {
		case ObservationStopped, ObservationAbsent:
			// Settled.
		default:
			unresolved = append(unresolved, workload.ID)
		}
	}
	result.Unresolved = unresolved
	result.Reconciled = len(unresolved) == 0
	if result.Reconciled {
		result.Detail = fmt.Sprintf("observed %d workload identities with no unresolved external effects", len(d.workloads))
	} else {
		result.Detail = fmt.Sprintf("%d workload identities remain unresolved", len(unresolved))
	}
	return result, nil
}

package work

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// State strings the enablement guards assert.
const (
	serviceStateEnabled   = "enabled"
	executionStateRunning = "running"
)

// authorityResolver maps the current-authority reads a commit unit carries to
// in-transaction guarded locks. The capability already evaluated the reads; the
// adapter re-evaluates the same current fact inside the transaction and pins
// the exact row and version it observed, so a membership that was revoked or
// demoted, a service that was disabled or a versioned row that changed after
// the unit was built cannot authorize the commit.
type authorityResolver struct {
	executor store.Executor
	scanner  store.Scanner
}

// authorityContext carries the implicit row identity a commit unit supplies to
// its authority reads. The real task service addresses the task it mutates
// through the unit rather than through each task_version read, and a
// pipeline-apply unit that creates a new pipeline has no row to name yet. Both
// are legitimate service-emitted shapes; any other unresolved target remains
// invalid_input.
type authorityContext struct {
	TaskID      id.TaskID
	NewPipeline bool
}

// authorityLocks translates every current-authority read into a read-only
// guarded lock in the same store.Unit. A check that cannot be mapped is
// invalid_input, never a silent skip.
func authorityLocks(ctx context.Context, executor store.Executor, scanner store.Scanner, reads []authorize.AuthorityRead, target authorityContext) ([]store.Lock, error) {
	resolver := authorityResolver{executor: executor, scanner: scanner}
	return resolver.locks(ctx, reads, target)
}

// locks maps every read, merging duplicate targets so one row is pinned once.
func (resolver authorityResolver) locks(ctx context.Context, reads []authorize.AuthorityRead, target authorityContext) ([]store.Lock, error) {
	locks := make([]store.Lock, 0, len(reads))
	index := make(map[string]int, len(reads))
	add := func(lock store.Lock) {
		// An absence guard names its unique key rather than a row id, and a
		// changed observed version is a different pin. Both are part of the
		// dedup identity so a same-row pin at a different version is never
		// silently merged into the earlier one (that would mask a conflict).
		key := lock.Table + "\x00"
		if lock.ExpectAbsent {
			key += fmt.Sprintf("absent\x00%v", lock.Key)
		} else {
			key += fmt.Sprintf("%v\x00%d", lock.IDs, lock.ExpectVersion)
		}
		if position, ok := index[key]; ok {
			existing := locks[position]
			if existing.ExpectVersion == 0 {
				existing.ExpectVersion = lock.ExpectVersion
			}
			if existing.Expect == nil {
				existing.Expect = lock.Expect
			}
			if existing.Deadline == nil {
				existing.Deadline = lock.Deadline
			}
			if lock.TenantWide {
				existing.TenantWide = true
			}
			locks[position] = existing
			return
		}
		index[key] = len(locks)
		locks = append(locks, lock)
	}
	for _, read := range reads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !read.Check.Valid() {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q is not frozen", read.Check))
		}
		if err := read.Scope.Validate(); err != nil {
			return nil, err
		}
		switch read.Check {
		case authorize.CheckMembership:
			fact, pins, err := resolver.membershipFactWithLocks(ctx, read)
			if err != nil {
				return nil, err
			}
			if !fact.Allowed {
				return nil, fault.New(fault.Forbidden, "the current membership does not grant the required role")
			}
			for _, lock := range pins {
				add(lock)
			}
		case authorize.CheckPushServiceAdmin:
			fact, pins, err := resolver.membershipFactWithLocks(ctx, read)
			if err != nil {
				return nil, err
			}
			if !fact.Allowed {
				return nil, fault.New(fault.Forbidden, "the caller does not currently hold Admin in scope")
			}
			for _, lock := range pins {
				add(lock)
			}
		case authorize.CheckPrincipalEnabled:
			lock, found, err := resolver.principalEnabledLock(ctx, read)
			if err != nil {
				return nil, err
			}
			if found {
				add(lock)
			}
		case authorize.CheckServiceEnabled:
			if read.TargetID == "" {
				// With no explicit target the check applies to the acting
				// principal: a service must be enabled, while a human or an
				// execution has no service enablement fact and adds no lock. A
				// principal that resolves to no committed row is refused.
				fact, lock, err := resolver.actingServiceEnabledLock(ctx, read)
				if err != nil {
					return nil, err
				}
				if !fact.Allowed {
					return nil, fault.New(fault.Forbidden, "the acting principal is not an enabled service")
				}
				if lock != nil {
					add(*lock)
				}
				continue
			}
			row, found, tenantWide, err := resolver.readNullableScope(ctx, read.Scope, "services", read.TargetID)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fault.New(fault.VersionConflict, "the current service is missing or out of scope")
			}
			add(store.Lock{
				Table:         "services",
				IDs:           []string{read.TargetID},
				ExpectVersion: versionValue(row, "version"),
				Expect:        map[string]any{"state": serviceStateEnabled},
				TenantWide:    tenantWide,
			})
		case authorize.CheckExecutionRunning:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "an execution authority read must name the execution row")
			}
			row, err := resolver.executor.Read(ctx, read.Scope, "executions", read.TargetID)
			if err != nil {
				return nil, err
			}
			add(store.Lock{
				Table:         "executions",
				IDs:           []string{read.TargetID},
				ExpectVersion: versionValue(row, "version"),
				Expect:        map[string]any{"state": executionStateRunning},
			})
		case authorize.CheckActionDeadline:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "an action deadline read must name the action row")
			}
			row, err := resolver.executor.Read(ctx, read.Scope, "actions", read.TargetID)
			if err != nil {
				return nil, err
			}
			add(store.Lock{
				Table:         "actions",
				IDs:           []string{read.TargetID},
				ExpectVersion: versionValue(row, "version"),
				Deadline:      &store.DeadlineGuard{Column: "deadline"},
			})
		case authorize.CheckBindingVersion:
			lock, err := versionedAuthorityLock(read, "resource_bindings", "version")
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckTaskVersion:
			lock, pinned, err := taskVersionAuthorityLock(read, target)
			if err != nil {
				return nil, err
			}
			if pinned {
				add(lock)
			}
		case authorize.CheckExecutionVersion:
			lock, err := versionedAuthorityLock(read, "executions", "version")
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckPipelineVersion:
			if target.NewPipeline && read.TargetID == "" && read.ExpectedVersion == 0 {
				// A pipeline-apply unit that creates a new pipeline names no
				// prior row: this same unit inserts it at version 1, so there
				// is no version to pin and the insert is the guard.
				continue
			}
			lock, err := versionedAuthorityLock(read, "pipelines", "version")
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckResourceOwnership:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "a resource ownership read must name the resource row")
			}
			// Ownership is the row's immutable tenant/project scope, so a
			// scoped existence lock is the guard.
			add(store.Lock{Table: "resources", IDs: []string{read.TargetID}})
		case authorize.CheckFailedTaskOperatorStart:
			pins, err := resolver.failedTaskOperatorStartLocks(ctx, read, target)
			if err != nil {
				return nil, err
			}
			for _, lock := range pins {
				add(lock)
			}
		default:
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q has no store mapping", read.Check))
		}
	}
	return locks, nil
}

// taskVersionAuthorityLock resolves the task row a task_version read addresses.
// The real task service names the task through the unit, not the read, so an
// empty TargetID resolves against the unit's task. A zero ExpectedVersion is
// the service's explicit no-lifecycle-precondition shape for content and
// comment edits; those units guard the row with their own content_version write
// expectation, so no lock is added. A non-zero version pins exactly the
// lifecycle_version the service observed.
func taskVersionAuthorityLock(read authorize.AuthorityRead, target authorityContext) (store.Lock, bool, error) {
	identity := read.TargetID
	if identity == "" {
		identity = string(target.TaskID)
	}
	if identity == "" {
		return store.Lock{}, false, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q must name its target row", read.Check))
	}
	if read.ExpectedVersion == 0 {
		return store.Lock{}, false, nil
	}
	return store.Lock{Table: "tasks", IDs: []string{identity}, Expect: map[string]any{"lifecycle_version": read.ExpectedVersion}}, true, nil
}

// versionedAuthorityLock builds the guarded lock of one versioned authority
// check. The lock asserts the version the capability observed, never whatever
// version happens to exist at adapter time.
func versionedAuthorityLock(read authorize.AuthorityRead, table, column string) (store.Lock, error) {
	if read.TargetID == "" {
		return store.Lock{}, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q must name its target row", read.Check))
	}
	if read.ExpectedVersion == 0 {
		return store.Lock{}, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q must carry its expected version", read.Check))
	}
	lock := store.Lock{Table: table, IDs: []string{read.TargetID}}
	if column == "version" {
		lock.ExpectVersion = read.ExpectedVersion
	} else {
		lock.Expect = map[string]any{column: read.ExpectedVersion}
	}
	return lock, nil
}

// membershipFactWithLocks evaluates the caller's current membership union and
// returns every pinned row as read-only guarded locks. Under the union a
// project-scoped caller reads BOTH the tenant-wide row of its tenant and the
// exact-project row and evaluates their union
// (authorize.EvaluateMembership -> tenancy.EffectiveRoles); a project row can
// only add roles and never shadows a tenant-wide grant. A tenant-scoped caller
// reads and pins only the tenant-wide row, so a project-only membership grants
// no tenant-wide right.
//
// The pins the fact captured are translated one-for-one: a present row becomes
// an identity+version lock (the plain store.Lock{IDs, ExpectVersion} shape,
// with TenantWide for the tenant-wide row) and an absent row becomes the
// absence guard on (tenant_id, project_id, principal_id). The store applies
// every pin inside the commit transaction before any write, so a concurrent
// grant, revoke, role change or creation of either row is a version_conflict
// and the unit commits nothing. A read that arrives with pins from an earlier
// observation is confirmed against the rows read here through
// authorize.ConfirmMembership, so the unit carries exactly those pins; a read
// without pins is evaluated through authorize.EvaluateMembership, which derives
// the same pins. Either way the store's guards carry those pins into the
// transaction.
//
// An execution principal holds no membership roles: its grant is the registry's
// execution permission and its enablement is a running execution row in the
// same scope, so it adds no membership lock. The unit's own
// execution_version and execution_running guards remain authoritative for the
// exact execution the unit observed. A service principal is evaluated through
// its service row because service roles live there, not on membership rows.
func (resolver authorityResolver) membershipFactWithLocks(ctx context.Context, read authorize.AuthorityRead) (authorize.AuthorityFact, []store.Lock, error) {
	fact := authorityFact(read)
	// The conditional push-service Admin grant is the same union with Admin
	// required and no execution permission; authorize.MembershipRead is its
	// single definition, so this path also answers a CheckPushServiceAdmin
	// read. The returned fact keeps the check the caller asked for.
	evaluated := authorize.MembershipRead(read)
	if evaluated.Scope.ProjectScoped() {
		row, found, err := resolver.findExecutionByPrincipal(ctx, evaluated.Scope, evaluated.PrincipalID)
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		if found {
			// Execution authority is never a membership row. The registry's
			// execution permission is the grant, and only a currently running
			// execution in this scope enables it. An unset or unknown permission
			// is not a grant and fails closed.
			if !evaluated.Required.Execution.Valid() || evaluated.Required.Execution == authorize.ExecNone {
				fact.Detail = "the operation declares no execution permission"
				return fact, nil, nil
			}
			if textValue(row, "state") != executionStateRunning {
				fact.Detail = "the execution principal is not running"
				return fact, nil, nil
			}
			fact.Version = versionValue(row, "version")
			fact.Allowed = true
			fact.Detail = "the running execution grants the registry's execution permission"
			return fact, nil, nil
		}
	}
	row, found, tenantWide, err := resolver.readNullableScope(ctx, evaluated.Scope, "services", string(evaluated.PrincipalID))
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	if found {
		roles, err := decodeAuthorityRoles(bytesValue(row, "roles"))
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		version := versionValue(row, "version")
		state := textValue(row, "state")
		fact.Version = version
		// A service never gains Admin: tenancy.AssignableServiceRoles drops it
		// (and every unknown value) before matching the required service roles,
		// so even a corrupt services row cannot administer.
		fact.Allowed = state == serviceStateEnabled && hasAnyAuthorityRole(tenancy.AssignableServiceRoles(roles), evaluated.Required.ServiceRoles)
		lock := store.Lock{
			Table:         "services",
			IDs:           []string{string(evaluated.PrincipalID)},
			ExpectVersion: version,
			Expect:        map[string]any{"state": serviceStateEnabled},
			TenantWide:    tenantWide,
		}
		return fact, []store.Lock{lock}, nil
	}
	rows, err := resolver.membershipRows(ctx, evaluated.Scope, evaluated.PrincipalID)
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	// A read that already carries the pins an earlier observation captured is
	// confirmed against the rows read now, and those same pins travel into the
	// unit. Any difference of identity, version or absence is a
	// version_conflict and a union that no longer grants the role is forbidden.
	if len(evaluated.Targets) > 0 {
		if err := authorize.ConfirmMembership(evaluated, false, rows); err != nil {
			return fact, nil, err
		}
		pins, err := membershipLocks(evaluated.Scope, evaluated.PrincipalID, evaluated.Targets)
		if err != nil {
			return fact, nil, err
		}
		fact.Allowed = true
		return fact, pins, nil
	}
	membershipFact, err := authorize.EvaluateMembership(evaluated, false, rows)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.Forbidden {
			fact.Allowed = false
			fact.Detail = membershipFact.Detail
			return fact, nil, nil
		}
		return fact, nil, err
	}
	pins, err := membershipLocks(evaluated.Scope, evaluated.PrincipalID, membershipFact.Targets)
	if err != nil {
		return fact, nil, err
	}
	fact.Version = membershipFact.Version
	fact.Allowed = true
	fact.Detail = membershipFact.Detail
	return fact, pins, nil
}

// membershipRows reads the complete applicable membership observation of one
// principal. The tenant-wide row always belongs to the read's tenant. The exact
// project row is read only for a project-scoped read, so a tenant-scoped read
// never observes - or pins - a project row.
func (resolver authorityResolver) membershipRows(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRows, error) {
	tenantWide, err := resolver.membershipRow(ctx, tenancy.Scope{TenantID: scope.TenantID}, principal)
	if err != nil {
		return authorize.MembershipRows{}, err
	}
	rows := authorize.MembershipRows{TenantWide: tenantWide}
	if scope.ProjectScoped() {
		project, err := resolver.membershipRow(ctx, scope, principal)
		if err != nil {
			return authorize.MembershipRows{}, err
		}
		rows.Project = project
	}
	return rows, nil
}

// membershipRow reads at most one membership row for one principal in one
// scope. The scanner applies the scope predicate, so a project-scoped read
// returns the exact project row and a tenant-scoped read returns the
// tenant-wide row (project_id IS NULL). No row is an absent observation.
func (resolver authorityResolver) membershipRow(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRow, error) {
	page, err := resolver.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   "memberships",
		Equals:  map[string]any{"principal_id": string(principal)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	if len(page.Rows) == 0 {
		return authorize.MembershipRow{}, nil
	}
	row := page.Rows[0]
	roles, err := decodeAuthorityRoles(bytesValue(row, "roles"))
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	return authorize.MembershipRow{
		RowID: authorityRowID(row),
		Membership: &tenancy.Membership{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(textValue(row, "tenant_id")),
				ProjectID: id.ProjectID(textValue(row, "project_id")),
			},
			PrincipalID: principal,
			Roles:       roles,
			Version:     versionValue(row, "version"),
		},
	}, nil
}

// membershipValues extracts the neutral membership rows from an observation.
// An absent row contributes nil so the union and pin builders treat it as no
// contribution.
func membershipValues(rows authorize.MembershipRows) (*tenancy.Membership, *tenancy.Membership) {
	var tenantWide, project *tenancy.Membership
	if rows.TenantWide.Membership != nil {
		tenantWide = rows.TenantWide.Membership
	}
	if rows.Project.Membership != nil {
		project = rows.Project.Membership
	}
	return tenantWide, project
}

// membershipLocks translates the pins the MembershipTargets builder
// produced into the store guards of the same commit unit. A present pin keeps
// its observed memberships.id identity AND version (identity alone or version
// alone is never row identity); an absence pin becomes the absence guard
// keyed by (tenant_id, project_id, principal_id) with an explicit nil project
// for a tenant-wide row. A malformed pin is refused rather than matched as a
// wildcard.
func membershipLocks(scope tenancy.Scope, principal id.PrincipalID, targets []authorize.AuthorityTarget) ([]store.Lock, error) {
	locks := make([]store.Lock, 0, len(targets))
	for index, target := range targets {
		if target.Scope.TenantID != scope.TenantID {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("membership pin %d names another tenant", index))
		}
		if target.Absent {
			if target.RowID != "" || target.ExpectedVersion != 0 {
				return nil, fault.New(fault.InvalidInput, fmt.Sprintf("membership pin %d is an absence pin carrying a row identity or version", index))
			}
			project := any(nil)
			if target.Scope.ProjectScoped() {
				project = string(target.Scope.ProjectID)
			}
			locks = append(locks, store.Lock{
				Table:        "memberships",
				ExpectAbsent: true,
				Key: map[string]any{
					"tenant_id":    string(scope.TenantID),
					"project_id":   project,
					"principal_id": string(principal),
				},
			})
			continue
		}
		if target.RowID == "" || target.ExpectedVersion == 0 {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("membership pin %d is not a present row identity with a version", index))
		}
		locks = append(locks, store.Lock{
			Table:         "memberships",
			IDs:           []string{target.RowID},
			ExpectVersion: target.ExpectedVersion,
			TenantWide:    target.Scope.TenantScoped(),
		})
	}
	return locks, nil
}

// principalEnabledLock re-reads the enablement fact of one principal by its
// committed kind. A service's enablement is its state, an execution's is its
// running state and a human's is the presence of the scoped membership row. A
// human enablement is an authentication-boundary fact, so a principal that
// resolves to no row yields no lock; every sensitive operation additionally
// carries a membership check.
func (resolver authorityResolver) principalEnabledLock(ctx context.Context, read authorize.AuthorityRead) (store.Lock, bool, error) {
	if read.Scope.ProjectScoped() {
		row, found, err := resolver.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID)
		if err != nil {
			return store.Lock{}, false, err
		}
		if found {
			return store.Lock{
				Table:         "executions",
				IDs:           []string{authorityRowID(row)},
				ExpectVersion: versionValue(row, "version"),
				Expect:        map[string]any{"state": executionStateRunning},
			}, true, nil
		}
	}
	row, found, tenantWide, err := resolver.readNullableScope(ctx, read.Scope, "services", string(read.PrincipalID))
	if err != nil {
		return store.Lock{}, false, err
	}
	if found {
		return store.Lock{
			Table:         "services",
			IDs:           []string{string(read.PrincipalID)},
			ExpectVersion: versionValue(row, "version"),
			Expect:        map[string]any{"state": serviceStateEnabled},
			TenantWide:    tenantWide,
		}, true, nil
	}
	membership, found, tenantWide, err := resolver.findMembership(ctx, read.Scope, read.PrincipalID)
	if err != nil {
		return store.Lock{}, false, err
	}
	if !found {
		return store.Lock{}, false, nil
	}
	return store.Lock{
		Table:         "memberships",
		IDs:           []string{authorityRowID(membership)},
		ExpectVersion: versionValue(membership, "version"),
		TenantWide:    tenantWide,
	}, true, nil
}

// actingServiceEnabledLock evaluates a service_enabled read that names no
// explicit target. The check applies to the acting principal: a service must be
// enabled, a human membership or an execution has no service enablement fact
// and is not applicable, and a principal that resolves to no committed row is
// refused.
func (resolver authorityResolver) actingServiceEnabledLock(ctx context.Context, read authorize.AuthorityRead) (authorize.AuthorityFact, *store.Lock, error) {
	fact := authorityFact(read)
	if read.Scope.ProjectScoped() {
		row, found, err := resolver.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID)
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		if found {
			fact.Version = versionValue(row, "version")
			fact.Allowed = true
			fact.Detail = "an execution principal has no service enablement fact"
			return fact, nil, nil
		}
	}
	row, found, tenantWide, err := resolver.readNullableScope(ctx, read.Scope, "services", string(read.PrincipalID))
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	if found {
		state := textValue(row, "state")
		version := versionValue(row, "version")
		fact.Version = version
		fact.Allowed = state == serviceStateEnabled
		if !fact.Allowed {
			fact.Detail = "the acting service is disabled"
		}
		lock := store.Lock{
			Table:         "services",
			IDs:           []string{string(read.PrincipalID)},
			ExpectVersion: version,
			Expect:        map[string]any{"state": serviceStateEnabled},
			TenantWide:    tenantWide,
		}
		return fact, &lock, nil
	}
	membership, found, _, err := resolver.findMembership(ctx, read.Scope, read.PrincipalID)
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	if found {
		fact.Version = versionValue(membership, "version")
		fact.Allowed = true
		fact.Detail = "a human principal has no service enablement fact"
		return fact, nil, nil
	}
	fact.Detail = "no committed membership, service or execution row in scope"
	return fact, nil, nil
}

// findMembership returns one scoped membership row of one principal for the
// presence-only enablement checks. It is not the authority union: role
// evaluation reads both rows through membershipRows/membershipRow. A
// project-scoped lookup prefers the exact project row and falls back to the
// tenant-wide row; a tenant-scoped lookup reads only tenant-wide grants. Both
// shapes are enough to prove "a membership exists in scope", and every
// sensitive operation additionally carries the union membership check.
func (resolver authorityResolver) findMembership(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (store.Row, bool, bool, error) {
	candidates := []struct {
		scope      tenancy.Scope
		tenantWide bool
	}{{scope: scope}}
	if scope.ProjectScoped() {
		candidates = append(candidates, struct {
			scope      tenancy.Scope
			tenantWide bool
		}{scope: tenancy.Scope{TenantID: scope.TenantID}, tenantWide: true})
	}
	for _, candidate := range candidates {
		page, err := resolver.scanner.Query(ctx, candidate.scope, store.ScanQuery{
			Table:   "memberships",
			Equals:  map[string]any{"principal_id": string(principal)},
			OrderBy: []store.Order{{Column: "id"}},
			Limit:   1,
		})
		if err != nil {
			return store.Row{}, false, false, err
		}
		if len(page.Rows) > 0 {
			return page.Rows[0], true, candidate.tenantWide, nil
		}
	}
	return store.Row{}, false, false, nil
}

// findExecutionByPrincipal returns the execution row of one execution
// principal. Executions are project-scoped, so a tenant-scoped read is refused.
func (resolver authorityResolver) findExecutionByPrincipal(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (store.Row, bool, error) {
	if scope.TenantScoped() {
		return store.Row{}, false, nil
	}
	page, err := resolver.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   "executions",
		Equals:  map[string]any{"principal_id": string(principal)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		return store.Row{}, false, err
	}
	if len(page.Rows) == 0 {
		return store.Row{}, false, nil
	}
	return page.Rows[0], true, nil
}

// readNullableScope reads one row from a table whose project column is
// nullable. A project-scoped caller first reads the exact project row and then
// the tenant-wide row, reporting which predicate matched.
func (resolver authorityResolver) readNullableScope(ctx context.Context, scope tenancy.Scope, table, identity string) (store.Row, bool, bool, error) {
	row, err := resolver.executor.Read(ctx, scope, table, identity)
	if err == nil {
		return row, true, false, nil
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		return store.Row{}, false, false, err
	}
	if !scope.ProjectScoped() {
		return store.Row{}, false, false, nil
	}
	row, err = resolver.executor.Read(ctx, tenancy.Scope{TenantID: scope.TenantID}, table, identity)
	if err == nil {
		return row, true, true, nil
	}
	if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
		return store.Row{}, false, false, nil
	}
	return store.Row{}, false, false, err
}

// authorityFact builds the identity-only fact a lock evaluation starts from.
func authorityFact(read authorize.AuthorityRead) authorize.AuthorityFact {
	return authorize.AuthorityFact{
		Check:       read.Check,
		Scope:       read.Scope,
		PrincipalID: read.PrincipalID,
		TargetID:    read.TargetID,
	}
}

// authorityRowID returns the stored identity of one row.
func authorityRowID(row store.Row) string {
	return textValue(row, "id")
}

// decodeAuthorityRoles decodes the roles JSON column of a membership or service
// row.
func decodeAuthorityRoles(raw []byte) ([]tenancy.Role, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var roles []tenancy.Role
	if err := json.Unmarshal(raw, &roles); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "membership or service roles are malformed", err)
	}
	return roles, nil
}

// hasAnyAuthorityRole reports whether roles contains at least one required role.
func hasAnyAuthorityRole(roles, required []tenancy.Role) bool {
	for _, wanted := range required {
		if hasAuthorityRole(roles, wanted) {
			return true
		}
	}
	return false
}

// hasAuthorityRole reports whether roles contains one role.
func hasAuthorityRole(roles []tenancy.Role, wanted tenancy.Role) bool {
	return slices.Contains(roles, wanted)
}

// failedTaskOperatorStartLocks re-evaluates the conditional operator retry
// inside the start transaction and returns the pins of the two rows it read.
//
// The operator role holds task.start only for an OPEN task whose CURRENT
// execution is FAILED. The read names the observed current execution in
// TargetID and its observed version in ExpectedVersion. The store therefore
// re-reads the task state and that execution row atomically with the commit and
// fails closed:
//
//   - no execution, a non-open task, a non-failed execution or a missing
//     execution row refuses forbidden;
//   - a moved current execution pointer or a moved execution version refuses
//     version_conflict;
//   - a present execution row with no observed version is invalid_input, so an
//     unversioned observation can never become an unguarded pin.
//
// The returned pins assert the exact rows this read observed, so a concurrent
// transition of either row is a version_conflict and the unit commits nothing.
func (resolver authorityResolver) failedTaskOperatorStartLocks(ctx context.Context, read authorize.AuthorityRead, target authorityContext) ([]store.Lock, error) {
	if target.TaskID == "" {
		return nil, fault.New(fault.InvalidInput, "an operator start read must name its task")
	}
	taskRow, err := resolver.executor.Read(ctx, read.Scope, "tasks", string(target.TaskID))
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return nil, fault.New(fault.VersionConflict, "the task row is missing")
		}
		return nil, err
	}
	if textValue(taskRow, "state") != string(task.StateOpen) {
		return nil, fault.New(fault.Forbidden, "the operator retry requires an open task")
	}
	if read.TargetID == "" {
		return nil, fault.New(fault.Forbidden, "the operator retry requires a current execution")
	}
	if textValue(taskRow, "current_execution_id") != read.TargetID {
		return nil, fault.New(fault.VersionConflict, "the task current execution changed")
	}
	executionRow, err := resolver.executor.Read(ctx, read.Scope, "executions", read.TargetID)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return nil, fault.New(fault.Forbidden, "the task current execution is missing")
		}
		return nil, err
	}
	if textValue(executionRow, "state") != string(kernel.ExecutionFailed) {
		return nil, fault.New(fault.Forbidden, "the task current execution is not failed")
	}
	observed := versionValue(executionRow, "version")
	if observed == 0 {
		return nil, fault.New(fault.InvalidInput, "the task current execution row has no version")
	}
	if read.ExpectedVersion == 0 {
		return nil, fault.New(fault.InvalidInput, "the operator start read must carry the observed execution version")
	}
	if observed != read.ExpectedVersion {
		return nil, fault.New(fault.VersionConflict, "the task current execution version changed")
	}
	return []store.Lock{
		{
			Table: "tasks",
			IDs:   []string{string(target.TaskID)},
			// The tasks row is versioned by lifecycle_version, not by a
			// generic version column; the unit's own task lock also pins it.
			Expect: map[string]any{
				"state":             string(task.StateOpen),
				"lifecycle_version": versionValue(taskRow, "lifecycle_version"),
			},
		},
		{
			Table:         "executions",
			IDs:           []string{read.TargetID},
			ExpectVersion: observed,
			Expect:        map[string]any{"state": string(kernel.ExecutionFailed)},
		},
	}, nil
}

// requireOperatorStartCondition refuses a task.start unit whose actor holds
// only the conditional operator grant but which omits the
// failed_task_operator_start read.
//
// The registry admits the operator role at the grant stage, so a commit path
// that forgets the conditional read would otherwise start an operator's retry
// without the failed-task authority. This guard re-reads the actor's current
// roles inside the transaction and fails closed, so an operator-only unit can
// never commit without the check. An engineer (or engineer-and-operator) actor
// and a service actor with the engineer role are unchanged.
//
// The guard applies to every unit that carries a task.start membership read,
// including the task.reply unit that carries the initiator's start authority, so
// a reply cannot create a fresh execution for an operator-only initiator. It
// reads the task.start membership read itself rather than the last read's
// operation, because a mixed unit (a reply) appends further reads after it.
func requireOperatorStartCondition(ctx context.Context, executor store.Executor, scanner store.Scanner, scope tenancy.Scope, reads []authorize.AuthorityRead) error {
	resolver := authorityResolver{executor: executor, scanner: scanner}
	return resolver.requireOperatorStartCondition(ctx, scope, reads)
}

func (resolver authorityResolver) requireOperatorStartCondition(ctx context.Context, scope tenancy.Scope, reads []authorize.AuthorityRead) error {
	declaresStart := false
	hasConditional := false
	actor := id.PrincipalID("")
	for _, read := range reads {
		if read.Operation != "task.start" {
			continue
		}
		declaresStart = true
		if read.Check == authorize.CheckFailedTaskOperatorStart {
			hasConditional = true
		}
		if read.Check == authorize.CheckMembership && read.PrincipalID != "" {
			actor = read.PrincipalID
		}
	}
	if !declaresStart || hasConditional {
		return nil
	}
	if actor == "" {
		return fault.New(fault.Forbidden, "a task.start unit carries no initiating principal")
	}
	roles, err := resolver.actorRoles(ctx, scope, actor)
	if err != nil {
		return err
	}
	if hasAuthorityRole(roles, tenancy.RoleOperator) && !hasAuthorityRole(roles, tenancy.RoleEngineer) {
		return fault.New(fault.Forbidden, "an operator-only task.start requires the failed_task_operator_start check")
	}
	return nil
}

// withoutFailedTaskOperatorStart drops the conditional failed-task admission
// read for a receipt replay. The ordinary membership read stays, so the commit
// boundary still re-evaluates the actor's current authority.
func withoutFailedTaskOperatorStart(reads []authorize.AuthorityRead) []authorize.AuthorityRead {
	filtered := make([]authorize.AuthorityRead, 0, len(reads))
	for _, read := range reads {
		if read.Check == authorize.CheckFailedTaskOperatorStart {
			continue
		}
		filtered = append(filtered, read)
	}
	return filtered
}

// actorRoles reads the current roles of one acting principal: a service row
// when the identity is a service, otherwise the project/tenant membership
// union. An identity with no committed row has no roles.
func (resolver authorityResolver) actorRoles(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) ([]tenancy.Role, error) {
	row, found, _, err := resolver.readNullableScope(ctx, scope, "services", string(principal))
	if err != nil {
		return nil, err
	}
	if found {
		roles, err := decodeAuthorityRoles(bytesValue(row, "roles"))
		if err != nil {
			return nil, err
		}
		return tenancy.AssignableServiceRoles(roles), nil
	}
	rows, err := resolver.membershipRows(ctx, scope, principal)
	if err != nil {
		return nil, err
	}
	tenantWide, project := membershipValues(rows)
	roles, ok := tenancy.EffectiveRoles(scope, tenantWide, project)
	if !ok {
		return nil, nil
	}
	return roles, nil
}

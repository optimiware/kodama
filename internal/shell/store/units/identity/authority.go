package identity

import (
	"context"
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// authorityLocks translates the current-authority reads a commit unit carries
// into read-only guarded locks inside the same store.Unit. A guarded lock names
// exactly one row and asserts its observed version (and, for enablement checks,
// its state) so a membership, service or execution that was revoked, disabled
// or changed after the caller read it cannot authorize the commit.
//
// A membership or conditional Admin check re-evaluates the current fact
// AuthorityReader returns, including the required roles the read recorded, and
// refuses with forbidden when that fact no longer holds. Under the membership
// union the human membership fact is the exact-project plus tenant-wide union,
// so the check re-reads BOTH rows through authorize.ConfirmMembership and pins
// them as a present-row id+version lock or an absence guard. Role
// membership inside a JSON column cannot be expressed as an equality guard, so
// ConfirmMembership's re-evaluation plus those pins are the atomic re-read
// inside the transaction.
func (a *Adapter) authorityLocks(ctx context.Context, reads []authorize.AuthorityRead) ([]store.Lock, error) {
	locks := make([]store.Lock, 0, len(reads))
	for _, read := range reads {
		if !read.Check.Valid() {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q is not frozen", read.Check))
		}
		if err := read.Scope.Validate(); err != nil {
			return nil, err
		}
		switch read.Check {
		case authorize.CheckMembership, authorize.CheckPushServiceAdmin:
			fact, membershipLocks, err := a.membershipAuthority(ctx, read)
			if err != nil {
				return nil, err
			}
			if !fact.Allowed {
				if read.Check == authorize.CheckPushServiceAdmin {
					return nil, fault.New(fault.Forbidden, "the caller does not currently hold Admin in scope")
				}
				return nil, fault.New(fault.Forbidden, "the current membership does not grant the required role")
			}
			locks = append(locks, membershipLocks...)
		case authorize.CheckPrincipalEnabled:
			lock, found, err := a.principalEnabledLock(ctx, read)
			if err != nil {
				return nil, err
			}
			if found {
				locks = append(locks, lock)
			}
		case authorize.CheckServiceEnabled:
			if read.TargetID == "" {
				// With no explicit target the check applies to the acting
				// principal: a service must be enabled, while a human or an
				// execution has no service enablement fact to assert and adds
				// no lock. A principal that resolves to no committed row is
				// still refused.
				fact, lock, err := a.actingServiceEnabledFact(ctx, read, authorityFact(read))
				if err != nil {
					return nil, err
				}
				if !fact.Allowed {
					return nil, fault.New(fault.Forbidden, "the acting principal is not an enabled service")
				}
				if lock != nil {
					locks = append(locks, *lock)
				}
				continue
			}
			row, found, tenantWide, err := a.readNullableScope(ctx, read.Scope, "services", read.TargetID)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fault.New(fault.VersionConflict, "the current service is missing or out of scope")
			}
			version, err := rowUint(row, "version")
			if err != nil {
				return nil, err
			}
			locks = append(locks, store.Lock{
				Table:         "services",
				IDs:           []string{read.TargetID},
				ExpectVersion: version,
				Expect:        map[string]any{"state": string(serviceStateEnabled)},
				TenantWide:    tenantWide,
			})
		case authorize.CheckExecutionRunning:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "an execution authority read must name the execution row")
			}
			row, err := a.executor.Read(ctx, read.Scope, "executions", read.TargetID)
			if err != nil {
				return nil, err
			}
			version, err := rowUint(row, "version")
			if err != nil {
				return nil, err
			}
			locks = append(locks, store.Lock{
				Table:         "executions",
				IDs:           []string{read.TargetID},
				ExpectVersion: version,
				Expect:        map[string]any{"state": string(executionStateRunning)},
			})
		case authorize.CheckActionDeadline:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "an action deadline read must name the action row")
			}
			row, err := a.executor.Read(ctx, read.Scope, "actions", read.TargetID)
			if err != nil {
				return nil, err
			}
			version, err := rowUint(row, "version")
			if err != nil {
				return nil, err
			}
			locks = append(locks, store.Lock{
				Table:         "actions",
				IDs:           []string{read.TargetID},
				ExpectVersion: version,
				Deadline:      &store.DeadlineGuard{Column: "deadline"},
			})
		case authorize.CheckBindingVersion:
			lock, err := versionedLock(read, "resource_bindings", "version")
			if err != nil {
				return nil, err
			}
			locks = append(locks, lock)
		case authorize.CheckTaskVersion:
			lock, err := versionedLock(read, "tasks", "lifecycle_version")
			if err != nil {
				return nil, err
			}
			locks = append(locks, lock)
		case authorize.CheckExecutionVersion:
			lock, err := versionedLock(read, "executions", "version")
			if err != nil {
				return nil, err
			}
			locks = append(locks, lock)
		case authorize.CheckPipelineVersion:
			lock, err := versionedLock(read, "pipelines", "version")
			if err != nil {
				return nil, err
			}
			locks = append(locks, lock)
		case authorize.CheckResourceOwnership:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "a resource ownership read must name the resource row")
			}
			locks = append(locks, store.Lock{Table: "resources", IDs: []string{read.TargetID}})
		case authorize.CheckFailedTaskOperatorStart:
			fact, pins, err := a.failedTaskOperatorStartFact(ctx, read)
			if err != nil {
				return nil, err
			}
			if !fact.Allowed {
				return nil, fault.New(fault.Forbidden, "the task is not open with a failed current execution")
			}
			if read.ExpectedVersion != 0 && fact.Version != read.ExpectedVersion {
				return nil, fault.New(fault.VersionConflict, "the task current execution version changed")
			}
			locks = append(locks, pins...)
		default:
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q has no store mapping", read.Check))
		}
	}
	return locks, nil
}

// versionedLock builds the guarded lock of one versioned authority check.
func versionedLock(read authorize.AuthorityRead, table, column string) (store.Lock, error) {
	if read.TargetID == "" {
		return store.Lock{}, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q must name its target row", read.Check))
	}
	if read.ExpectedVersion == 0 {
		return store.Lock{}, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q must carry its expected version", read.Check))
	}
	lock := store.Lock{Table: table, IDs: []string{read.TargetID}, ExpectVersion: read.ExpectedVersion}
	if column != "version" {
		lock.ExpectVersion = 0
		lock.Expect = map[string]any{column: read.ExpectedVersion}
	}
	return lock, nil
}

// principalEnabledLock re-reads the enablement fact of one principal. A
// service's enablement is its state, an execution's is its running state and a
// human's is the presence of the scoped membership. A human enablement is an
// authentication-boundary fact, not a committed store row, so a principal that
// resolves to no row yields no lock; every sensitive operation additionally
// carries a membership check.
func (a *Adapter) principalEnabledLock(ctx context.Context, read authorize.AuthorityRead) (store.Lock, bool, error) {
	if read.Scope.ProjectScoped() {
		row, found, err := a.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID)
		if err != nil {
			return store.Lock{}, false, err
		}
		if found {
			version, err := rowUint(row, "version")
			if err != nil {
				return store.Lock{}, false, err
			}
			return store.Lock{
				Table:         "executions",
				IDs:           []string{mustRowID(row)},
				ExpectVersion: version,
				Expect:        map[string]any{"state": string(executionStateRunning)},
			}, true, nil
		}
	}
	row, found, tenantWide, err := a.readNullableScope(ctx, read.Scope, "services", string(read.PrincipalID))
	if err != nil {
		return store.Lock{}, false, err
	}
	if found {
		version, err := rowUint(row, "version")
		if err != nil {
			return store.Lock{}, false, err
		}
		return store.Lock{
			Table:         "services",
			IDs:           []string{string(read.PrincipalID)},
			ExpectVersion: version,
			Expect:        map[string]any{"state": string(serviceStateEnabled)},
			TenantWide:    tenantWide,
		}, true, nil
	}
	membership, found, tenantWide, err := a.findMembership(ctx, read.Scope, read.PrincipalID)
	if err != nil {
		return store.Lock{}, false, err
	}
	if !found {
		return store.Lock{}, false, nil
	}
	version, err := rowUint(membership, "version")
	if err != nil {
		return store.Lock{}, false, err
	}
	return store.Lock{
		Table:         "memberships",
		IDs:           []string{mustRowID(membership)},
		ExpectVersion: version,
		TenantWide:    tenantWide,
	}, true, nil
}

// findMembership returns the presence of one scoped membership row of one
// principal. It is the enablement/presence probe only: a project-scoped lookup
// prefers the exact project row and uses the tenant-wide row when no project
// row exists. It is deliberately NOT the membership authority read, which must
// observe and pin BOTH rows through readMembershipRows and the union.
func (a *Adapter) findMembership(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (store.Row, bool, bool, error) {
	scopes := []struct {
		scope      tenancy.Scope
		tenantWide bool
	}{{scope: scope}}
	if scope.ProjectScoped() {
		scopes = append(scopes, struct {
			scope      tenancy.Scope
			tenantWide bool
		}{scope: tenancy.Scope{TenantID: scope.TenantID}, tenantWide: true})
	}
	for _, candidate := range scopes {
		page, err := a.scanner.Query(ctx, candidate.scope, store.ScanQuery{
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
func (a *Adapter) findExecutionByPrincipal(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (store.Row, bool, error) {
	if scope.TenantScoped() {
		return store.Row{}, false, nil
	}
	page, err := a.scanner.Query(ctx, scope, store.ScanQuery{
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
// the tenant-wide row, reporting which predicate matched so the lock can carry
// the same TenantWide target.
func (a *Adapter) readNullableScope(ctx context.Context, scope tenancy.Scope, table, identity string) (store.Row, bool, bool, error) {
	row, err := a.executor.Read(ctx, scope, table, identity)
	if err == nil {
		return row, true, false, nil
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		return store.Row{}, false, false, err
	}
	if !scope.ProjectScoped() {
		return store.Row{}, false, false, nil
	}
	row, err = a.executor.Read(ctx, tenancy.Scope{TenantID: scope.TenantID}, table, identity)
	if err == nil {
		return row, true, true, nil
	}
	if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
		return store.Row{}, false, false, nil
	}
	return store.Row{}, false, false, err
}

// mustRowID returns the stored identity of one row. A row read through the
// seam always carries it.
func mustRowID(row store.Row) string {
	identity, err := rowString(row, "id")
	if err != nil {
		return ""
	}
	return identity
}

// serviceStateEnabled and executionStateRunning are the declared state strings
// the enablement guards assert.
const (
	serviceStateEnabled   = "enabled"
	executionStateRunning = "running"
)

// Read returns one current-authority fact per supplied read, in input order.
// It always observes committed state through the scoped seams and never caches
// a previous answer.
func (a *Adapter) Read(ctx context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		fact, err := a.readFact(ctx, read)
		if err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, nil
}

// readFact evaluates one authority read against committed state.
func (a *Adapter) readFact(ctx context.Context, read authorize.AuthorityRead) (authorize.AuthorityFact, error) {
	if !read.Check.Valid() {
		return authorize.AuthorityFact{}, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q is not frozen", read.Check))
	}
	if err := read.Scope.Validate(); err != nil {
		return authorize.AuthorityFact{}, err
	}
	fact := authorize.AuthorityFact{
		Check:       read.Check,
		Scope:       read.Scope,
		PrincipalID: read.PrincipalID,
		TargetID:    read.TargetID,
	}
	switch read.Check {
	case authorize.CheckMembership, authorize.CheckPushServiceAdmin:
		fact, _, err := a.membershipAuthority(ctx, read)
		return fact, err
	case authorize.CheckPrincipalEnabled:
		return a.principalEnabledFact(ctx, read, fact)
	case authorize.CheckServiceEnabled:
		if read.TargetID == "" {
			fact, _, err := a.actingServiceEnabledFact(ctx, read, fact)
			return fact, err
		}
		row, found, _, err := a.readNullableScope(ctx, read.Scope, "services", read.TargetID)
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		if !found {
			fact.Detail = "service is missing or out of scope"
			return fact, nil
		}
		state, err := rowString(row, "state")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		version, err := rowUint(row, "version")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		fact.Version = version
		fact.Allowed = state == serviceStateEnabled
		return fact, nil
	case authorize.CheckExecutionRunning:
		if read.TargetID == "" {
			return authorize.AuthorityFact{}, fault.New(fault.InvalidInput, "an execution authority read must name the execution row")
		}
		row, err := a.executor.Read(ctx, read.Scope, "executions", read.TargetID)
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		state, err := rowString(row, "state")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		version, err := rowUint(row, "version")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		fact.Version = version
		fact.Allowed = state == executionStateRunning
		return fact, nil
	case authorize.CheckActionDeadline:
		if read.TargetID == "" {
			return authorize.AuthorityFact{}, fault.New(fault.InvalidInput, "an action deadline read must name the action row")
		}
		row, err := a.executor.Read(ctx, read.Scope, "actions", read.TargetID)
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		version, err := rowUint(row, "version")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		deadline, err := rowNullableTime(row, "deadline")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		fact.Version = version
		fact.Allowed = deadline != nil && deadline.After(a.clock.Now())
		return fact, nil
	case authorize.CheckBindingVersion:
		return a.versionedFact(ctx, read, fact, "resource_bindings", "version")
	case authorize.CheckTaskVersion:
		return a.versionedFact(ctx, read, fact, "tasks", "lifecycle_version")
	case authorize.CheckExecutionVersion:
		return a.versionedFact(ctx, read, fact, "executions", "version")
	case authorize.CheckPipelineVersion:
		return a.versionedFact(ctx, read, fact, "pipelines", "version")
	case authorize.CheckResourceOwnership:
		if read.TargetID == "" {
			return authorize.AuthorityFact{}, fault.New(fault.InvalidInput, "a resource ownership read must name the resource row")
		}
		row, err := a.executor.Read(ctx, read.Scope, resourcesTable, read.TargetID)
		if err != nil {
			// resource.binding.register's ownership read is satisfied by
			// ABSENCE because the register decision rule replaces the
			// executor read and creates the row at commit. update, delete,
			// read and report keep the existing-row rule and report
			// not_found for an absent row.
			if read.Operation == registerBindingOperation {
				if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
					fact.Allowed = true
					fact.Version = 0
					return fact, nil
				}
			}
			return authorize.AuthorityFact{}, err
		}
		version, err := rowUint(row, "version")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		fact.Version = version
		fact.Allowed = true
		return fact, nil
	case authorize.CheckFailedTaskOperatorStart:
		fact, _, err := a.failedTaskOperatorStartFact(ctx, read)
		return fact, err
	default:
		return authorize.AuthorityFact{}, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q has no store mapping", read.Check))
	}
}

// actingServiceEnabledFact evaluates a service_enabled read that names no
// explicit target. The check applies to the acting principal: a service must be
// enabled, a human membership or an execution has no service enablement fact
// and is not applicable, and a principal that resolves to no committed row is
// refused.
func (a *Adapter) actingServiceEnabledFact(ctx context.Context, read authorize.AuthorityRead, fact authorize.AuthorityFact) (authorize.AuthorityFact, *store.Lock, error) {
	if read.Scope.ProjectScoped() {
		row, found, err := a.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID)
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		if found {
			version, err := rowUint(row, "version")
			if err != nil {
				return authorize.AuthorityFact{}, nil, err
			}
			fact.Version = version
			fact.Allowed = true
			fact.Detail = "an execution principal has no service enablement fact"
			return fact, nil, nil
		}
	}
	row, found, tenantWide, err := a.readNullableScope(ctx, read.Scope, "services", string(read.PrincipalID))
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	if found {
		state, err := rowString(row, "state")
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		version, err := rowUint(row, "version")
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		fact.Version = version
		fact.Allowed = state == serviceStateEnabled
		if !fact.Allowed {
			fact.Detail = "the acting service is disabled"
		}
		lock := store.Lock{
			Table:         "services",
			IDs:           []string{string(read.PrincipalID)},
			ExpectVersion: version,
			Expect:        map[string]any{"state": string(serviceStateEnabled)},
			TenantWide:    tenantWide,
		}
		return fact, &lock, nil
	}
	membership, found, _, err := a.findMembership(ctx, read.Scope, read.PrincipalID)
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	if found {
		version, err := rowUint(membership, "version")
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		fact.Version = version
		fact.Allowed = true
		fact.Detail = "a human principal has no service enablement fact"
		return fact, nil, nil
	}
	fact.Detail = "no committed membership, service or execution row in scope"
	return fact, nil, nil
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

// principalEnabledFact evaluates one principal's current enablement.
//
// An AuthorityRead carries only the scope the check applies to, never the
// actor's own scope. tenant.create therefore names the NEW tenant's scope, so a
// human actor's membership must resolve there for the fact to hold; the adapter
// cannot infer a different actor scope from the read and must not widen the
// lookup. A caller that needs the actor's own tenant asserted has to build the
// read in that scope.
func (a *Adapter) principalEnabledFact(ctx context.Context, read authorize.AuthorityRead, fact authorize.AuthorityFact) (authorize.AuthorityFact, error) {
	if read.Scope.ProjectScoped() {
		row, found, err := a.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID)
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		if found {
			state, err := rowString(row, "state")
			if err != nil {
				return authorize.AuthorityFact{}, err
			}
			version, err := rowUint(row, "version")
			if err != nil {
				return authorize.AuthorityFact{}, err
			}
			fact.Version = version
			fact.Allowed = state == executionStateRunning
			return fact, nil
		}
	}
	row, found, _, err := a.readNullableScope(ctx, read.Scope, "services", string(read.PrincipalID))
	if err != nil {
		return authorize.AuthorityFact{}, err
	}
	if found {
		state, err := rowString(row, "state")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		version, err := rowUint(row, "version")
		if err != nil {
			return authorize.AuthorityFact{}, err
		}
		fact.Version = version
		fact.Allowed = state == serviceStateEnabled
		return fact, nil
	}
	membership, found, _, err := a.findMembership(ctx, read.Scope, read.PrincipalID)
	if err != nil {
		return authorize.AuthorityFact{}, err
	}
	if !found {
		fact.Detail = "no committed membership, service or execution row in scope"
		return fact, nil
	}
	version, err := rowUint(membership, "version")
	if err != nil {
		return authorize.AuthorityFact{}, err
	}
	fact.Version = version
	fact.Allowed = true
	return fact, nil
}

// versionedFact evaluates one versioned authority check. Allowed reports
// whether the observed version equals the expected version, so a stale read is
// refused by authorize.Confirm as forbidden during preflight. That is
// deliberate and kept: a preflight cannot distinguish "someone else advanced
// the row" from "the caller's own earlier commit advanced it", and the
// commit-boundary guarded lock is what reports version_conflict with nothing
// persisted. A read with no expected version only reports the observed version.
func (a *Adapter) versionedFact(ctx context.Context, read authorize.AuthorityRead, fact authorize.AuthorityFact, table, column string) (authorize.AuthorityFact, error) {
	if read.TargetID == "" {
		return authorize.AuthorityFact{}, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q must name its target row", read.Check))
	}
	row, err := a.executor.Read(ctx, read.Scope, table, read.TargetID)
	if err != nil {
		return authorize.AuthorityFact{}, err
	}
	version, err := rowUint(row, column)
	if err != nil {
		return authorize.AuthorityFact{}, err
	}
	fact.Version = version
	if read.ExpectedVersion != 0 {
		fact.Allowed = version == read.ExpectedVersion
	} else {
		fact.Allowed = true
	}
	return fact, nil
}

// decodeRolesFromRow decodes the roles JSON column of a membership or service
// row.
func decodeRolesFromRow(row store.Row) ([]tenancy.Role, error) {
	data, err := rowBytes(row, "roles")
	if err != nil {
		return nil, err
	}
	return decodeRoles(data)
}

// hasAnyRole reports whether roles contains at least one required role.
func hasAnyRole(roles, required []tenancy.Role) bool {
	for _, wanted := range required {
		if hasRole(roles, wanted) {
			return true
		}
	}
	return false
}

// hasRole reports whether roles contains one role.
func hasRole(roles []tenancy.Role, wanted tenancy.Role) bool {
	return slices.Contains(roles, wanted)
}

// taskStateOpen and executionStateFailed are the declared state strings the
// conditional operator retry asserts.
const (
	taskStateOpen        = "open"
	executionStateFailed = "failed"
)

// failedTaskOperatorStartFact re-reads the task state and the named current
// execution for the conditional operator task.start grant. The read names the
// observed execution in TargetID; the task is resolved through the execution's
// task_id column. Only an open task whose current execution is failed is
// allowed. The observed execution version is returned so the caller's
// ExpectedVersion pin is enforced by Confirm or by the commit lock.
func (a *Adapter) failedTaskOperatorStartFact(ctx context.Context, read authorize.AuthorityRead) (authorize.AuthorityFact, []store.Lock, error) {
	fact := authorityFact(read)
	if read.TargetID == "" {
		fact.Detail = "the task has no current execution"
		return fact, nil, nil
	}
	execution, err := a.executor.Read(ctx, read.Scope, "executions", read.TargetID)
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	taskID, err := rowNullableString(execution, "task_id")
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	if taskID == "" {
		fact.Detail = "the execution names no task"
		return fact, nil, nil
	}
	taskRow, err := a.executor.Read(ctx, read.Scope, "tasks", taskID)
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	version, err := rowUint(execution, "version")
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	executionState, err := rowString(execution, "state")
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	taskState, err := rowString(taskRow, "state")
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	current, err := rowNullableString(taskRow, "current_execution_id")
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	lifecycle, err := rowUint(taskRow, "lifecycle_version")
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	fact.Version = version
	fact.Allowed = taskState == taskStateOpen && current == read.TargetID && executionState == executionStateFailed
	if !fact.Allowed {
		fact.Detail = "the task is not open with a failed current execution"
		return fact, nil, nil
	}
	return fact, []store.Lock{
		{
			Table:  "tasks",
			IDs:    []string{taskID},
			Expect: map[string]any{"state": taskStateOpen, "lifecycle_version": lifecycle},
		},
		{
			Table:         "executions",
			IDs:           []string{read.TargetID},
			ExpectVersion: version,
			Expect:        map[string]any{"state": executionStateFailed},
		},
	}, nil
}

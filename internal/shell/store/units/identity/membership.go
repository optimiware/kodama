package identity

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Compile-time pin: the consumer-owned membership reader is implemented by the
// identity store adapter.
var _ authorize.MembershipReader = (*Adapter)(nil)

// ReadMembership observes the membership rows that apply to one principal in
// one scope. A project-scoped read returns the exact-project row and the
// tenant-wide row; a tenant-scoped read returns only the tenant-wide row. Each
// present row carries its committed memberships.id identity and version, and an
// absent row carries no identity. Reading is not authority: the commit unit
// re-reads both rows, compares the confirmed pins and re-evaluates the union.
func (a *Adapter) ReadMembership(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRows, error) {
	if err := scope.Validate(); err != nil {
		return authorize.MembershipRows{}, err
	}
	if _, err := id.ParsePrincipalID(string(principal)); err != nil {
		return authorize.MembershipRows{}, fault.Wrap(fault.InvalidInput, "membership reader principal id is invalid", err)
	}
	return a.readMembershipRows(ctx, scope, principal)
}

// readMembershipRows performs the two scoped reads of the union. The tenant
// scope reads only tenant-wide rows and the project scope reads only exact
// project rows, so neither query can observe a row outside the requested
// scope. The canonical row order makes a duplicate row deterministic.
func (a *Adapter) readMembershipRows(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRows, error) {
	rows := authorize.MembershipRows{}
	tenantRow, tenantWide, found, err := a.membershipRowInScope(ctx, tenancy.Scope{TenantID: scope.TenantID}, principal)
	if err != nil {
		return authorize.MembershipRows{}, err
	}
	if found {
		rows.TenantWide = authorize.MembershipRow{RowID: mustRowID(tenantRow), Membership: &tenantWide}
	}
	if !scope.ProjectScoped() {
		return rows, nil
	}
	projectRow, project, found, err := a.membershipRowInScope(ctx, scope, principal)
	if err != nil {
		return authorize.MembershipRows{}, err
	}
	if found {
		rows.Project = authorize.MembershipRow{RowID: mustRowID(projectRow), Membership: &project}
	}
	return rows, nil
}

// membershipRowInScope reads the single membership row of one principal in the
// supplied scope. The scoped scanner never widens to the tenant when a project
// scope is requested, so an exact-project read cannot silently fall back.
func (a *Adapter) membershipRowInScope(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (store.Row, tenancy.Membership, bool, error) {
	page, err := a.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   "memberships",
		Equals:  map[string]any{"principal_id": string(principal)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		return store.Row{}, tenancy.Membership{}, false, err
	}
	if len(page.Rows) == 0 {
		return store.Row{}, tenancy.Membership{}, false, nil
	}
	membership, err := decodeMembership(page.Rows[0])
	if err != nil {
		return store.Row{}, tenancy.Membership{}, false, err
	}
	return page.Rows[0], membership, true, nil
}

// findExactMembership returns the membership row that belongs to exactly the
// supplied scope. A grant or revoke mutates that row and nothing else; the
// union read is never used to pick a row to write, because a tenant-wide row
// must never be updated or deleted as if it were the project grant.
func (a *Adapter) findExactMembership(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (store.Row, bool, error) {
	row, _, found, err := a.membershipRowInScope(ctx, scope, principal)
	if err != nil {
		return store.Row{}, false, err
	}
	return row, found, nil
}

// membershipAbsenceLock is the create-if-absent guard of a first grant. It names
// the registered (tenant_id, project_id, principal_id) unique key, with an
// explicit NULL project_id for a tenant-wide row.
func membershipAbsenceLock(scope tenancy.Scope, principal id.PrincipalID) store.Lock {
	var project any
	if scope.ProjectScoped() {
		project = string(scope.ProjectID)
	}
	return store.Lock{
		Table:        "memberships",
		ExpectAbsent: true,
		Key: map[string]any{
			"tenant_id":    string(scope.TenantID),
			"project_id":   project,
			"principal_id": string(principal),
		},
	}
}

// membershipAuthority evaluates the current membership fact of one read and
// returns the read-only guards that pin the rows it observed. An execution
// principal keeps its earlier running-execution fact; a service principal is
// represented by its service row, whose roles never include Admin; a human
// principal is the union of the tenant-wide and exact-project rows through
// authorize.EvaluateMembership, re-read and confirmed through
// authorize.ConfirmMembership. The returned guards are present-row id+version
// locks or absence guards, never a single tenant-wide wildcard.
func (a *Adapter) membershipAuthority(ctx context.Context, read authorize.AuthorityRead) (authorize.AuthorityFact, []store.Lock, error) {
	base := authorityFact(read)
	// authorize.MembershipRead is the one definition of the grant a
	// membership-family read requires; the fact keeps the check the caller
	// asked for.
	evaluated := authorize.MembershipRead(read)
	if read.Scope.ProjectScoped() {
		if execution, found, err := a.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID); err != nil {
			return base, nil, err
		} else if found {
			return a.executionMembershipFact(evaluated, base, execution)
		}
	}
	if serviceRow, found, tenantWide, err := a.readNullableScope(ctx, read.Scope, "services", string(read.PrincipalID)); err != nil {
		return base, nil, err
	} else if found {
		return a.serviceMembershipFact(read, base, serviceRow, tenantWide)
	}
	return a.humanMembershipFact(ctx, read, base)
}

// executionMembershipFact applies the execution-principal rule: a running
// execution holds no membership row and is admitted by the execution permission
// that travels with it. No lock is added here; the commit unit keeps the
// execution guards it lists.
func (a *Adapter) executionMembershipFact(read authorize.AuthorityRead, fact authorize.AuthorityFact, row store.Row) (authorize.AuthorityFact, []store.Lock, error) {
	state, err := rowString(row, "state")
	if err != nil {
		return fact, nil, err
	}
	version, err := rowUint(row, "version")
	if err != nil {
		return fact, nil, err
	}
	fact.Version = version
	fact.Allowed = read.Required.Execution.Valid() &&
		read.Required.Execution != authorize.ExecNone &&
		state == executionStateRunning
	switch {
	case !read.Required.Execution.Valid() || read.Required.Execution == authorize.ExecNone:
		fact.Detail = "the operation grants no execution permission"
	case state != executionStateRunning:
		fact.Detail = "the execution is not running"
	default:
		fact.Detail = "a running execution principal is authorized by its registry execution permission"
	}
	return fact, nil, nil
}

// serviceMembershipFact evaluates a service principal from its service row. The
// service role set always excludes Admin, which is never service-assignable,
// even when a corrupt row carries it. The guard pins the exact service row at
// its observed version and enabled state.
func (a *Adapter) serviceMembershipFact(read authorize.AuthorityRead, fact authorize.AuthorityFact, row store.Row, tenantWide bool) (authorize.AuthorityFact, []store.Lock, error) {
	roles, err := decodeRolesFromRow(row)
	if err != nil {
		return fact, nil, err
	}
	roles = tenancy.AssignableServiceRoles(roles)
	version, err := rowUint(row, "version")
	if err != nil {
		return fact, nil, err
	}
	state, err := rowString(row, "state")
	if err != nil {
		return fact, nil, err
	}
	required := authorize.MembershipRead(read).Required.ServiceRoles
	fact.Version = version
	fact.Allowed = state == serviceStateEnabled && hasAnyRole(roles, required)
	switch {
	case state != serviceStateEnabled:
		fact.Detail = "the service is not enabled"
	case !hasAnyRole(roles, required):
		fact.Detail = "the service does not hold the required role"
	case read.Check == authorize.CheckPushServiceAdmin:
		// A service never holds Admin, so this branch is unreachable for a
		// correct row; keep the message precise if it is ever reached.
		fact.Detail = "Admin is never service-assignable"
	}
	if !fact.Allowed {
		return fact, nil, nil
	}
	lock := store.Lock{
		Table:         "services",
		IDs:           []string{string(read.PrincipalID)},
		ExpectVersion: version,
		Expect:        map[string]any{"state": string(serviceStateEnabled)},
		TenantWide:    tenantWide,
	}
	return fact, []store.Lock{lock}, nil
}

// humanMembershipFact evaluates the union of the human's tenant-wide and
// exact-project rows, then re-reads both rows and confirms the observed pins
// through authorize.ConfirmMembership before translating them into guards. A
// union that no longer grants the required role is a fact with Allowed false; a
// pin change is returned as a refusal.
func (a *Adapter) humanMembershipFact(ctx context.Context, read authorize.AuthorityRead, fact authorize.AuthorityFact) (authorize.AuthorityFact, []store.Lock, error) {
	evalRead := authorize.MembershipRead(read)
	rows, err := a.readMembershipRows(ctx, evalRead.Scope, evalRead.PrincipalID)
	if err != nil {
		return fact, nil, err
	}
	fact.Version = membershipVersion(rows, evalRead.Scope)
	evaluated, err := authorize.EvaluateMembership(evalRead, false, rows)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.Forbidden {
			fact.Allowed = false
			fact.Detail = evaluated.Detail
			return fact, nil, nil
		}
		return fact, nil, err
	}
	pinned := evaluated.PinnedRead()
	if len(read.Targets) > 0 {
		// The capability already confirmed the union and attached its pins to
		// the commit-unit read. Those pins are the ones the atomic boundary must
		// compare, never a freshly derived set that would silently accept a
		// change between the capability's confirmation and this re-read.
		pinned = evalRead
		pinned.Targets = append([]authorize.AuthorityTarget(nil), read.Targets...)
	}
	confirmed, err := a.readMembershipRows(ctx, pinned.Scope, pinned.PrincipalID)
	if err != nil {
		return fact, nil, err
	}
	if err := authorize.ConfirmMembership(pinned, false, confirmed); err != nil {
		return fact, nil, err
	}
	guards, err := membershipLocks(evalRead, pinned.Targets)
	if err != nil {
		return fact, nil, err
	}
	fact.Allowed = evaluated.Allowed
	fact.Detail = evaluated.Detail
	return fact, guards, nil
}

// membershipVersion reports the observed version a membership fact carries for
// diagnostics: the exact-project row when the read is project-scoped and the
// row exists, otherwise the tenant-wide row. The union itself is pinned per row
// through Targets, not through this single field.
func membershipVersion(rows authorize.MembershipRows, scope tenancy.Scope) uint64 {
	if scope.ProjectScoped() && rows.Project.Membership != nil {
		return rows.Project.Membership.Version
	}
	if rows.TenantWide.Membership != nil {
		return rows.TenantWide.Membership.Version
	}
	return 0
}

// membershipLocks translates the observed targets of one membership fact into
// the commit unit's read-only guards. A present target pins the exact row id and
// version; an absent target pins the registered unique key. A single
// TenantWide membership lock is never produced.
func membershipLocks(read authorize.AuthorityRead, targets []authorize.AuthorityTarget) ([]store.Lock, error) {
	locks := make([]store.Lock, 0, len(targets))
	for _, target := range targets {
		if target.Absent {
			if read.Scope.TenantScoped() && target.Scope.ProjectScoped() {
				return nil, fault.New(fault.InvalidInput, "a tenant-scoped membership read observed a project target")
			}
			locks = append(locks, membershipAbsenceLock(target.Scope, read.PrincipalID))
			continue
		}
		if target.RowID == "" {
			return nil, fault.New(fault.InvalidInput, "a present membership target carries no observed row identity")
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

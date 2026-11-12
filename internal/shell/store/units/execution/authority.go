package executionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Store is the membership reader the execution adapter confirms against. The
// capability services may also use it to pin both rows into an authority read.
var _ authorize.MembershipReader = (*Store)(nil)

// State strings the enablement guards assert.
const (
	serviceStateEnabled   = "enabled"
	executionStateRunning = "running"
)

// authorityLocks maps every current-authority read a commit unit carries to an
// in-transaction guarded lock. A guarded lock names exactly one row and asserts
// the fact the capability read, so a membership that was revoked or demoted, a
// service that was disabled, or an action whose deadline passed after the unit
// was built cannot authorize the commit.
//
// A check that cannot be mapped to a durable row is invalid_input, never a
// silent skip. Membership is confirmed against the UNION of the tenant-wide
// and exact-project rows (including the required roles the read recorded) and
// refuses with forbidden when the current union no longer grants it; its pins
// lock both rows, by identity AND version when present and by unique-key
// absence when absent. The conditional push-admin check evaluates the same
// two-row membership union with an Admin requirement. The enablement, running
// and deadline checks are asserted by their guard and refuse with
// version_conflict or deadline_expired.
func (s *Store) authorityLocks(ctx context.Context, reads []authorize.AuthorityRead) ([]store.Lock, error) {
	locks := make([]store.Lock, 0, len(reads))
	index := make(map[string]int, len(reads))
	add := func(lock store.Lock) {
		key := lockIdentity(lock)
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
			membership, err := s.membershipLocks(ctx, read)
			if err != nil {
				return nil, err
			}
			for _, lock := range membership {
				add(lock)
			}
		case authorize.CheckPushServiceAdmin:
			membership, err := s.membershipLocks(ctx, authorize.MembershipRead(read))
			if err != nil {
				return nil, err
			}
			for _, lock := range membership {
				add(lock)
			}
		case authorize.CheckPrincipalEnabled:
			lock, found, err := s.principalEnabledLock(ctx, read)
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
				// execution has no service enablement fact and is not
				// applicable. A principal that resolves to no committed row is
				// refused.
				fact, lock, err := s.actingServiceEnabledLock(ctx, read)
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
			row, found, tenantWide, err := s.readNullableScope(ctx, read.Scope, tableServices, read.TargetID)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fault.New(fault.VersionConflict, "the current service is missing or out of scope")
			}
			version, err := rowUint64(row.Values, "version")
			if err != nil {
				return nil, err
			}
			add(store.Lock{
				Table:         tableServices,
				IDs:           []string{read.TargetID},
				ExpectVersion: version,
				Expect:        map[string]any{"state": serviceStateEnabled},
				TenantWide:    tenantWide,
			})
		case authorize.CheckExecutionRunning:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "an execution authority read must name the execution row")
			}
			row, err := s.executor.Read(ctx, read.Scope, tableExecutions, read.TargetID)
			if err != nil {
				return nil, err
			}
			version, err := rowUint64(row.Values, "version")
			if err != nil {
				return nil, err
			}
			add(store.Lock{
				Table:         tableExecutions,
				IDs:           []string{read.TargetID},
				ExpectVersion: version,
				Expect:        map[string]any{"state": executionStateRunning},
			})
		case authorize.CheckActionDeadline:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "an action deadline read must name the action row")
			}
			row, err := s.executor.Read(ctx, read.Scope, tableActions, read.TargetID)
			if err != nil {
				return nil, err
			}
			version, err := rowUint64(row.Values, "version")
			if err != nil {
				return nil, err
			}
			add(store.Lock{
				Table:         tableActions,
				IDs:           []string{read.TargetID},
				ExpectVersion: version,
				Deadline:      &store.DeadlineGuard{Column: "deadline"},
			})
		case authorize.CheckBindingVersion:
			lock, err := versionedLock(read, tableBindings, "version")
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckTaskVersion:
			lock, err := versionedLock(read, tableTasks, "lifecycle_version")
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckExecutionVersion:
			lock, err := versionedLock(read, tableExecutions, "version")
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckPipelineVersion:
			lock, err := versionedLock(read, tablePipelines, "version")
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckResourceOwnership:
			if read.TargetID == "" {
				return nil, fault.New(fault.InvalidInput, "a resource ownership read must name the resource row")
			}
			// Ownership is the row's immutable tenant/project scope, so a
			// scoped existence lock is the guard: a row outside the unit scope
			// or removed before the commit is refused.
			add(store.Lock{Table: tableResources, IDs: []string{read.TargetID}})
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
	lock := store.Lock{Table: table, IDs: []string{read.TargetID}}
	if column == "version" {
		lock.ExpectVersion = read.ExpectedVersion
	} else {
		lock.Expect = map[string]any{column: read.ExpectedVersion}
	}
	return lock, nil
}

// membershipLocks confirms one membership authority read against the committed
// UNION of the tenant-wide and exact-project rows and returns the locks that
// pin it. A read that already carries observed targets is confirmed against
// them; a read from a caller that did not pin is evaluated now and pinned from
// that observation, so the transaction locks the exact rows and versions the
// evaluation saw.
//
// A project row can only add roles and never shadows the tenant-wide grant.
// An execution principal holds no membership roles, so a membership read for
// one refuses with forbidden on every port. A service principal is evaluated
// through its service row rather than a human membership.
func (s *Store) membershipLocks(ctx context.Context, read authorize.AuthorityRead) ([]store.Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if read.Scope.ProjectScoped() {
		if _, found, err := s.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID); err != nil {
			return nil, err
		} else if found {
			// An execution principal holds no membership roles. The registry
			// grants executions ExecNone on every membership-bearing operation,
			// so a membership read for one is always a refusal, never a silent
			// pass with no lock.
			return nil, fault.New(fault.Forbidden, "an execution principal holds no membership roles")
		}
	}
	row, found, tenantWide, err := s.readNullableScope(ctx, read.Scope, tableServices, string(read.PrincipalID))
	if err != nil {
		return nil, err
	}
	if found {
		roles, err := decodeRolesFromRow(row.Values)
		if err != nil {
			return nil, err
		}
		version, err := rowUint64(row.Values, "version")
		if err != nil {
			return nil, err
		}
		state, err := rowString(row.Values, "state")
		if err != nil {
			return nil, err
		}
		if state != serviceStateEnabled || !hasAnyRole(tenancy.AssignableServiceRoles(roles), read.Required.ServiceRoles) {
			return nil, fault.New(fault.Forbidden, "the acting service does not hold the required role")
		}
		return []store.Lock{{
			Table:         tableServices,
			IDs:           []string{string(read.PrincipalID)},
			ExpectVersion: version,
			Expect:        map[string]any{"state": serviceStateEnabled},
			TenantWide:    tenantWide,
		}}, nil
	}

	rows, err := s.ReadMembership(ctx, read.Scope, read.PrincipalID)
	if err != nil {
		return nil, err
	}
	targets := read.Targets
	if len(targets) > 0 {
		// The caller pinned both rows when it confirmed the fact. The unit must
		// still compare the current rows with those pins and re-evaluate the
		// union: matching versions alone are never durable permission.
		if err := authorize.ConfirmMembership(read, false, rows); err != nil {
			return nil, err
		}
	} else {
		fact, err := authorize.EvaluateMembership(read, false, rows)
		if err != nil {
			return nil, err
		}
		targets = fact.Targets
	}
	return membershipTargetLocks(read, targets)
}

// membershipTargetLocks maps the observed membership pins to guarded locks. A
// present row is locked by its memberships.id identity AND version; an absent
// row is guarded by the registered unique key of its scope and principal, so a
// row created before the commit is a version_conflict.
func membershipTargetLocks(read authorize.AuthorityRead, targets []authorize.AuthorityTarget) ([]store.Lock, error) {
	locks := make([]store.Lock, 0, len(targets))
	for _, target := range targets {
		if target.Absent {
			locks = append(locks, store.Lock{
				Table:        tableMemberships,
				ExpectAbsent: true,
				Key:          membershipAbsenceKey(target.Scope, read.PrincipalID),
			})
			continue
		}
		if target.RowID == "" {
			return nil, fault.New(fault.InvalidInput, "a present membership pin carries no row identity")
		}
		if target.ExpectedVersion == 0 {
			return nil, fault.New(fault.InvalidInput, "a present membership pin carries no observed version")
		}
		locks = append(locks, store.Lock{
			Table:         tableMemberships,
			IDs:           []string{target.RowID},
			ExpectVersion: target.ExpectedVersion,
			TenantWide:    read.Scope.ProjectScoped() && target.Scope.TenantScoped(),
		})
	}
	return locks, nil
}

// membershipAbsenceKey builds the registered (tenant_id, project_id,
// principal_id) unique-key identity of an absent membership row. A tenant-wide
// row carries an explicit nil project_id, never an omitted column.
func membershipAbsenceKey(scope tenancy.Scope, principal id.PrincipalID) map[string]any {
	key := map[string]any{
		"tenant_id":    string(scope.TenantID),
		"project_id":   nil,
		"principal_id": string(principal),
	}
	if scope.ProjectScoped() {
		key["project_id"] = string(scope.ProjectID)
	}
	return key
}

// ReadMembership observes both applicable membership rows of one principal for
// a project-scoped read and only the tenant-wide row for a tenant-scoped read.
// Each present row carries its memberships.id identity and version; a missing
// row is reported as an absent MembershipRow.
func (s *Store) ReadMembership(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRows, error) {
	if err := ctx.Err(); err != nil {
		return authorize.MembershipRows{}, err
	}
	if err := scope.Validate(); err != nil {
		return authorize.MembershipRows{}, err
	}
	if _, err := id.ParsePrincipalID(string(principal)); err != nil {
		return authorize.MembershipRows{}, fault.Wrap(fault.InvalidInput, "membership principal id is invalid", err)
	}
	tenantRow, err := s.readMembershipRow(ctx, tenancy.Scope{TenantID: scope.TenantID}, principal)
	if err != nil {
		return authorize.MembershipRows{}, err
	}
	rows := authorize.MembershipRows{TenantWide: tenantRow}
	if scope.ProjectScoped() {
		projectRow, err := s.readMembershipRow(ctx, scope, principal)
		if err != nil {
			return authorize.MembershipRows{}, err
		}
		rows.Project = projectRow
	}
	return rows, nil
}

// readMembershipRow reads at most one committed membership row of one
// principal in exactly one scope. The scanner's scope predicate selects the
// tenant-wide row (project_id IS NULL) or the exact project row.
func (s *Store) readMembershipRow(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRow, error) {
	page, err := s.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   tableMemberships,
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
	rowID, err := rowString(row.Values, "id")
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	if rowID == "" {
		return authorize.MembershipRow{}, fault.New(fault.InternalFailure, "a committed membership row carries no identity")
	}
	roles, err := decodeRolesFromRow(row.Values)
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	version, err := rowUint64(row.Values, "version")
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	membership := tenancy.Membership{
		Scope:       scope,
		PrincipalID: principal,
		Roles:       roles,
		Version:     version,
	}
	if err := membership.Validate(); err != nil {
		return authorize.MembershipRow{}, fault.Wrap(fault.InternalFailure, "a committed membership row is invalid", err)
	}
	return authorize.MembershipRow{RowID: rowID, Membership: &membership}, nil
}

// lockIdentity names one planned lock for duplicate merging. An absence guard
// has no row id, so it is keyed by its canonical unique key; a present lock is
// keyed by its table and row identity.
func lockIdentity(lock store.Lock) string {
	if lock.ExpectAbsent {
		columns, _ := store.AbsenceKeyColumns(lock.Table)
		parts := make([]string, 0, len(columns)+2)
		parts = append(parts, lock.Table, "absent")
		for _, name := range columns {
			parts = append(parts, fmt.Sprintf("%v", lock.Key[name]))
		}
		return strings.Join(parts, "\x00")
	}
	return lock.Table + "\x00" + strings.Join(lock.IDs, "\x00")
}

// principalEnabledLock re-reads the enablement fact of one principal by its
// committed kind. A service's enablement is its state, an execution's is its
// running state and a human's is the presence of the scoped membership row.
// A human enablement is an authentication-boundary fact, so a principal that
// resolves to no row yields no lock; every sensitive operation additionally
// carries a membership check.
func (s *Store) principalEnabledLock(ctx context.Context, read authorize.AuthorityRead) (store.Lock, bool, error) {
	if read.Scope.ProjectScoped() {
		row, found, err := s.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID)
		if err != nil {
			return store.Lock{}, false, err
		}
		if found {
			version, err := rowUint64(row.Values, "version")
			if err != nil {
				return store.Lock{}, false, err
			}
			return store.Lock{
				Table:         tableExecutions,
				IDs:           []string{mustRowID(row)},
				ExpectVersion: version,
				Expect:        map[string]any{"state": executionStateRunning},
			}, true, nil
		}
	}
	row, found, tenantWide, err := s.readNullableScope(ctx, read.Scope, tableServices, string(read.PrincipalID))
	if err != nil {
		return store.Lock{}, false, err
	}
	if found {
		version, err := rowUint64(row.Values, "version")
		if err != nil {
			return store.Lock{}, false, err
		}
		return store.Lock{
			Table:         tableServices,
			IDs:           []string{string(read.PrincipalID)},
			ExpectVersion: version,
			Expect:        map[string]any{"state": serviceStateEnabled},
			TenantWide:    tenantWide,
		}, true, nil
	}
	membership, found, tenantWide, err := s.findMembership(ctx, read.Scope, read.PrincipalID)
	if err != nil {
		return store.Lock{}, false, err
	}
	if !found {
		return store.Lock{}, false, nil
	}
	version, err := rowUint64(membership.Values, "version")
	if err != nil {
		return store.Lock{}, false, err
	}
	return store.Lock{
		Table:         tableMemberships,
		IDs:           []string{mustRowID(membership)},
		ExpectVersion: version,
		TenantWide:    tenantWide,
	}, true, nil
}

// actingServiceEnabledLock evaluates a service_enabled read that names no
// explicit target. The check applies to the acting principal: a service must be
// enabled, a human membership or an execution has no service enablement fact
// and is not applicable, and a principal that resolves to no committed row is
// refused.
func (s *Store) actingServiceEnabledLock(ctx context.Context, read authorize.AuthorityRead) (authorize.AuthorityFact, *store.Lock, error) {
	fact := authorityFact(read)
	if read.Scope.ProjectScoped() {
		row, found, err := s.findExecutionByPrincipal(ctx, read.Scope, read.PrincipalID)
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		if found {
			version, err := rowUint64(row.Values, "version")
			if err != nil {
				return authorize.AuthorityFact{}, nil, err
			}
			fact.Version = version
			fact.Allowed = true
			fact.Detail = "an execution principal has no service enablement fact"
			return fact, nil, nil
		}
	}
	row, found, tenantWide, err := s.readNullableScope(ctx, read.Scope, tableServices, string(read.PrincipalID))
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	if found {
		state, err := rowString(row.Values, "state")
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		version, err := rowUint64(row.Values, "version")
		if err != nil {
			return authorize.AuthorityFact{}, nil, err
		}
		fact.Version = version
		fact.Allowed = state == serviceStateEnabled
		if !fact.Allowed {
			fact.Detail = "the acting service is disabled"
		}
		lock := store.Lock{
			Table:         tableServices,
			IDs:           []string{string(read.PrincipalID)},
			ExpectVersion: version,
			Expect:        map[string]any{"state": serviceStateEnabled},
			TenantWide:    tenantWide,
		}
		return fact, &lock, nil
	}
	membership, found, _, err := s.findMembership(ctx, read.Scope, read.PrincipalID)
	if err != nil {
		return authorize.AuthorityFact{}, nil, err
	}
	if found {
		version, err := rowUint64(membership.Values, "version")
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

// findMembership returns one scoped membership row of one principal for the
// single-row enablement facts. A project-scoped lookup prefers
// the exact project grant and falls back to the tenant-wide grant that covers
// the project; a tenant-scoped lookup reads only tenant-wide grants, matching
// the scanner's scope predicate. The membership and push-admin authority checks
// use ReadMembership instead, which observes the UNION of both rows.
func (s *Store) findMembership(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (store.Row, bool, bool, error) {
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
		page, err := s.scanner.Query(ctx, candidate.scope, store.ScanQuery{
			Table:   tableMemberships,
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
func (s *Store) findExecutionByPrincipal(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (store.Row, bool, error) {
	if scope.TenantScoped() {
		return store.Row{}, false, nil
	}
	page, err := s.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   tableExecutions,
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
func (s *Store) readNullableScope(ctx context.Context, scope tenancy.Scope, table, identity string) (store.Row, bool, bool, error) {
	row, err := s.executor.Read(ctx, scope, table, identity)
	if err == nil {
		return row, true, false, nil
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		return store.Row{}, false, false, err
	}
	if !scope.ProjectScoped() {
		return store.Row{}, false, false, nil
	}
	row, err = s.executor.Read(ctx, tenancy.Scope{TenantID: scope.TenantID}, table, identity)
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

// decodeRolesFromRow decodes the roles JSON column of a membership or service
// row.
func decodeRolesFromRow(values map[string]any) ([]tenancy.Role, error) {
	data, err := rowBytes(values, "roles")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var roles []tenancy.Role
	if err := json.Unmarshal(data, &roles); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "membership or service roles are malformed", err)
	}
	return roles, nil
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

// mustRowID returns the stored identity of one row. A row read through the
// seam always carries it.
func mustRowID(row store.Row) string {
	identity, err := rowString(row.Values, "id")
	if err != nil {
		return ""
	}
	return identity
}

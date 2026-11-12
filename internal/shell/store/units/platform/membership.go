package platform

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Membership-union authority at the platform commit boundary.
//
// Every membership or service-role authority read observes BOTH applicable
// rows: the tenant-wide row of the caller's tenant and the exact-project row of
// a project-scoped caller. The effective grant is their union, so a project row
// can only ADD roles and never shadows a tenant-wide grant. A tenant-scoped read
// observes only the tenant-wide row, so a project-only membership grants no
// tenant-wide right.
//
// The adapter evaluates the CURRENT rows through authorize.EvaluateMembership
// (the same pure union the capability confirmed with) and pins every observed
// row: a present row by its observed row identity AND version, an absent
// memberships row by the absence guard on the declared unique key
// (tenant_id, project_id, principal_id). When the capability recorded a pin set
// on AuthorityRead.Targets, authorize.ConfirmMembership compares it with the
// rows re-read here first, so a revoke, re-grant, role change or the creation
// of an absent row between confirmation and commit is version_conflict before
// any write. Version equality is never permission: the union is re-evaluated on
// these rows inside the same adapter decision, and a matching pin set that no
// longer grants the required role is forbidden.
//
// A service principal uses the same union over its two service rows with
// tenancy.EffectiveServiceRoles, which never yields Admin: a service never
// gains Admin even from a corrupt row. The declared services unique key
// (tenant_id, id) allows at most one service row per principal, so an absent
// service row cannot be created for a principal whose sibling row applies; the
// absence guard is registered for memberships only and a service row is
// therefore pinned whenever it exists.

// membershipFamily is the durable row family that carries one principal's
// membership authority.
type membershipFamily int

const (
	membershipFamilyMissing membershipFamily = iota
	membershipFamilyExecution
	membershipFamilyService
	membershipFamilyHuman
)

// observedPrincipal is one principal's observed applicable rows in one unit
// scope. Exactly one family is selected, service before human, and an execution
// principal holds no membership row.
type observedPrincipal struct {
	family     membershipFamily
	project    *store.Row
	tenantWide *store.Row
}

// membershipUnionLocks maps one membership authority read onto the union of the
// principal's both rows.
func membershipUnionLocks(
	ctx context.Context,
	executor store.Executor,
	scanner store.Scanner,
	scope tenancy.Scope,
	read authorize.AuthorityRead,
) ([]store.Lock, error) {
	if read.PrincipalID == "" {
		return nil, fault.New(fault.InvalidInput, "membership authority read has no principal")
	}
	observed, err := observePrincipalRows(ctx, executor, scanner, scope, read.PrincipalID)
	if err != nil {
		return nil, err
	}
	switch observed.family {
	case membershipFamilyExecution:
		// An execution principal holds no membership row and no membership
		// roles, so it can never hold the Admin the conditional push-service
		// check requires: that read is a refusal, never "not applicable".
		if read.Check == authorize.CheckPushServiceAdmin {
			return nil, fault.New(fault.Forbidden, "an execution principal never holds Admin")
		}
		// For a plain membership read its own authority is the separate
		// execution_running and action_deadline guards the unit lists. The
		// membership read is not applicable and contributes no union lock.
		return nil, nil
	case membershipFamilyService:
		return unionLocks(read, observed, scope, true)
	case membershipFamilyHuman:
		return unionLocks(read, observed, scope, false)
	default:
		return nil, fault.New(fault.Forbidden, "the current principal has no membership, service or execution row in scope")
	}
}

// serviceEnabledUnionLocks maps one service-enablement read onto the union of
// the service's both rows. The target service is the read's explicit target
// when it names one, otherwise its principal.
func serviceEnabledUnionLocks(
	ctx context.Context,
	executor store.Executor,
	scope tenancy.Scope,
	read authorize.AuthorityRead,
) ([]store.Lock, error) {
	principal := read.PrincipalID
	if read.TargetID != "" {
		principal = id.PrincipalID(read.TargetID)
	}
	if principal == "" {
		return nil, fault.New(fault.InvalidInput, "service_enabled authority read has no service")
	}
	observed, err := observeServiceRows(ctx, executor, scope, principal)
	if err != nil {
		return nil, err
	}
	if observed.project == nil && observed.tenantWide == nil {
		return nil, fault.New(fault.VersionConflict, "the current service is missing or out of scope")
	}
	derived := read
	derived.PrincipalID = principal
	return unionLocks(derived, observed, scope, true)
}

// unionLocks evaluates the union of the observed rows against the roles
// the read requires and returns the pins as store locks. service selects the
// service-role union and the failure mode of a missing row.
func unionLocks(
	read authorize.AuthorityRead,
	observed observedPrincipal,
	scope tenancy.Scope,
	service bool,
) ([]store.Lock, error) {
	if service {
		for _, row := range []*store.Row{observed.project, observed.tenantWide} {
			if row == nil {
				continue
			}
			if err := requireRowState(*row, enabledServiceState); err != nil {
				return nil, err
			}
		}
	}
	rows, err := observed.membershipRows(scope, read.PrincipalID)
	if err != nil {
		return nil, err
	}
	fact, err := membershipUnionFact(read, service, rows)
	if err != nil {
		return nil, err
	}
	roleValues, err := observed.roleValues()
	if err != nil {
		return nil, err
	}
	return membershipTargetLocks(read, service, fact.Targets, roleValues)
}

// membershipUnionFact evaluates the union of the observed rows. When the read
// carries a confirmed pin set, ConfirmMembership refuses any change of either
// row first (version_conflict) and then the current union's refusal
// (forbidden); otherwise the same union evaluation produces the fact and its
// pins.
func membershipUnionFact(
	read authorize.AuthorityRead,
	service bool,
	rows authorize.MembershipRows,
) (authorize.MembershipFact, error) {
	derived := authorize.MembershipRead(read)
	if len(read.Targets) > 0 {
		if err := authorize.ConfirmMembership(derived, service, rows); err != nil {
			return authorize.MembershipFact{}, err
		}
	}
	return authorize.EvaluateMembership(derived, service, rows)
}

// membershipTargetLocks maps the fact's pins onto store locks. A present pin
// carries the observed row identity together with its version (and, for a
// service row, the enabled-state guard). An absent memberships row becomes the
// absence guard naming the declared unique key, with an explicit NULL project
// id for the tenant-wide row.
func membershipTargetLocks(
	read authorize.AuthorityRead,
	service bool,
	targets []authorize.AuthorityTarget,
	rolesByRow map[string][]byte,
) ([]store.Lock, error) {
	table := membershipsTable
	if service {
		table = servicesTable
	}
	legacy := -1
	if len(read.Targets) == 0 && read.ExpectedVersion != 0 {
		// A read without a pin set carries only the single row version it
		// observed. Binding it to that same row keeps that guard without
		// weakening the identity pin the union adds for both rows.
		legacy = legacyVersionTarget(read, targets)
	}
	locks := make([]store.Lock, 0, len(targets))
	for index, target := range targets {
		if target.Absent {
			if service {
				// Only memberships registers an absence-guardable unique key.
				// The services unique key (tenant_id, id) makes a second row
				// for one principal impossible, so an absent service row cannot
				// be filled while a sibling applies; the union itself refuses
				// when neither row exists.
				continue
			}
			key := map[string]any{
				tenantIDColumn:  string(target.Scope.TenantID),
				projectIDColumn: nil,
				principalColumn: string(read.PrincipalID),
			}
			if target.Scope.ProjectScoped() {
				key[projectIDColumn] = string(target.Scope.ProjectID)
			}
			locks = append(locks, store.Lock{
				Table:        membershipsTable,
				ExpectAbsent: true,
				Key:          key,
			})
			continue
		}
		if target.RowID == "" {
			// A present pin without its observed identity is never a wildcard.
			return nil, fault.New(fault.VersionConflict, "a present membership authority target carries no row identity")
		}
		version := target.ExpectedVersion
		if index == legacy {
			version = read.ExpectedVersion
		}
		if version == 0 {
			return nil, fault.New(fault.InternalFailure, fmt.Sprintf("%s row has no observed version", table))
		}
		lock := store.Lock{
			Table:         table,
			IDs:           []string{target.RowID},
			ExpectVersion: version,
			TenantWide:    target.Scope.TenantScoped(),
		}
		// Guard the exact roles bytes the union evaluated as well as the
		// version, so a same-version role rewrite between this decision and the
		// transaction cannot slip past the identity+version pin.
		roles, ok := rolesByRow[target.RowID]
		if !ok {
			return nil, fault.New(fault.InternalFailure, fmt.Sprintf("observed %s row %q carries no roles", table, target.RowID))
		}
		lock.Expect = map[string]any{rolesColumn: roles}
		if service {
			lock.Expect[stateColumn] = enabledServiceState
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

// roleValues returns the exact stored roles bytes of every observed row, keyed
// by row identity. The commit unit guards them together with version so the
// roles the union evaluated cannot change underneath it.
func (observed observedPrincipal) roleValues() (map[string][]byte, error) {
	roles := make(map[string][]byte, 2)
	for _, row := range []*store.Row{observed.project, observed.tenantWide} {
		if row == nil {
			continue
		}
		identity, err := textValue(*row, idColumn)
		if err != nil {
			return nil, err
		}
		encoded, err := bytesValue(*row, rolesColumn)
		if err != nil {
			return nil, err
		}
		roles[identity] = encoded
	}
	return roles, nil
}

// legacyVersionTarget names the target a single-row read's
// ExpectedVersion belongs to: the exact-project target when that row applies,
// otherwise the tenant-wide target.
func legacyVersionTarget(read authorize.AuthorityRead, targets []authorize.AuthorityTarget) int {
	if read.Scope.ProjectScoped() && len(targets) == 2 && !targets[0].Absent {
		return 0
	}
	if len(targets) == 0 {
		return -1
	}
	return len(targets) - 1
}

// observePrincipalRows resolves the row family that currently represents one
// principal and reads both of its applicable rows.
func observePrincipalRows(
	ctx context.Context,
	executor store.Executor,
	scanner store.Scanner,
	scope tenancy.Scope,
	principal id.PrincipalID,
) (observedPrincipal, error) {
	if scope.ProjectScoped() {
		row, found, err := findExecutionByPrincipal(ctx, scanner, scope, principal)
		if err != nil {
			return observedPrincipal{}, err
		}
		if found {
			return observedPrincipal{family: membershipFamilyExecution, project: &row}, nil
		}
	}
	service, err := observeServiceRows(ctx, executor, scope, principal)
	if err != nil {
		return observedPrincipal{}, err
	}
	if service.project != nil || service.tenantWide != nil {
		service.family = membershipFamilyService
		return service, nil
	}
	human, err := observeMembershipRows(ctx, scanner, scope, principal)
	if err != nil {
		return observedPrincipal{}, err
	}
	if human.project != nil || human.tenantWide != nil {
		human.family = membershipFamilyHuman
		return human, nil
	}
	return observedPrincipal{family: membershipFamilyMissing}, nil
}

// observeServiceRows reads the service row of the exact project and of the
// tenant that owns it. A service row is addressed by its identity, which is the
// service principal id.
func observeServiceRows(
	ctx context.Context,
	executor store.Executor,
	scope tenancy.Scope,
	principal id.PrincipalID,
) (observedPrincipal, error) {
	observed := observedPrincipal{}
	if scope.ProjectScoped() {
		row, err := readOptionalRow(ctx, executor, scope, servicesTable, string(principal))
		if err != nil {
			return observedPrincipal{}, err
		}
		observed.project = row
	}
	row, err := readOptionalRow(ctx, executor, tenancy.Scope{TenantID: scope.TenantID}, servicesTable, string(principal))
	if err != nil {
		return observedPrincipal{}, err
	}
	observed.tenantWide = row
	return observed, nil
}

// observeMembershipRows reads the human membership row of the exact project and
// of the tenant that owns it. Membership rows are addressed by their principal
// in the scope, because the observed row identity is what the union pins.
func observeMembershipRows(
	ctx context.Context,
	scanner store.Scanner,
	scope tenancy.Scope,
	principal id.PrincipalID,
) (observedPrincipal, error) {
	if scanner == nil {
		return observedPrincipal{}, fault.New(fault.InternalFailure, "principal resolution requires a scanner")
	}
	observed := observedPrincipal{}
	if scope.ProjectScoped() {
		row, err := findMembershipRow(ctx, scanner, scope, principal)
		if err != nil {
			return observedPrincipal{}, err
		}
		observed.project = row
	}
	row, err := findMembershipRow(ctx, scanner, tenancy.Scope{TenantID: scope.TenantID}, principal)
	if err != nil {
		return observedPrincipal{}, err
	}
	observed.tenantWide = row
	return observed, nil
}

// findMembershipRow returns the one membership row of a principal in one exact
// scope, or nil when the row is absent.
func findMembershipRow(
	ctx context.Context,
	scanner store.Scanner,
	scope tenancy.Scope,
	principal id.PrincipalID,
) (*store.Row, error) {
	page, err := scanner.Query(ctx, scope, store.ScanQuery{
		Table:   membershipsTable,
		Equals:  map[string]any{principalColumn: string(principal)},
		OrderBy: []store.Order{{Column: idColumn}},
		Limit:   1,
	})
	if err != nil {
		return nil, err
	}
	if len(page.Rows) == 0 {
		return nil, nil
	}
	row := page.Rows[0]
	return &row, nil
}

// readOptionalRow reads one scoped row and reports a missing row as nil.
func readOptionalRow(
	ctx context.Context,
	executor store.Executor,
	scope tenancy.Scope,
	table string,
	identity string,
) (*store.Row, error) {
	row, err := readRow(ctx, executor, scope, table, identity)
	if err == nil {
		return &row, nil
	}
	if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
		return nil, nil
	}
	return nil, err
}

// membershipRows maps the observed rows onto the neutral union input. A
// tenant-wide row is carried at the tenant scope, an exact-project row at the
// project scope, and an absent row as a nil Membership.
func (observed observedPrincipal) membershipRows(scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRows, error) {
	project, err := observedMembershipRow(observed.project, scope, principal)
	if err != nil {
		return authorize.MembershipRows{}, err
	}
	tenantWide, err := observedMembershipRow(observed.tenantWide, tenancy.Scope{TenantID: scope.TenantID}, principal)
	if err != nil {
		return authorize.MembershipRows{}, err
	}
	return authorize.MembershipRows{TenantWide: tenantWide, Project: project}, nil
}

// observedMembershipRow converts one durable row into the neutral membership
// value the pure union consumes, keeping the observed row identity for the pin.
func observedMembershipRow(row *store.Row, scope tenancy.Scope, principal id.PrincipalID) (authorize.MembershipRow, error) {
	if row == nil {
		return authorize.MembershipRow{}, nil
	}
	identity, err := textValue(*row, idColumn)
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	if identity == "" {
		return authorize.MembershipRow{}, fault.New(fault.InternalFailure, fmt.Sprintf("%s row has no usable identity", row.Table))
	}
	roles, err := decodeRowRoles(*row)
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	version, err := confirmedVersion(*row)
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	return authorize.MembershipRow{
		RowID: identity,
		Membership: &tenancy.Membership{
			Scope:       scope,
			PrincipalID: principal,
			Roles:       roles,
			Version:     version,
		},
	}, nil
}

// confirmedVersion returns the current version of a row the union pins.
func confirmedVersion(row store.Row) (uint64, error) {
	value, present := row.Values[versionColumn]
	if !present || value == nil {
		return 0, missingColumn(row.Table, versionColumn)
	}
	switch typed := value.(type) {
	case uint64:
		return typed, nil
	case int64:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, fmt.Sprintf("%s row column %q is negative", row.Table, versionColumn))
		}
		return uint64(typed), nil
	default:
		return 0, corruptRow(row.Table, versionColumn, value)
	}
}

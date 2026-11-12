package authorize_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Membership-union authority vectors.
//
// A project-scoped membership authority read observes BOTH the tenant-wide row
// and the exact-project row. The effective grant is their union
// (tenancy.EffectiveRoles / EffectiveServiceRoles); a project row can only add
// roles and never shadows the tenant-wide grant. The resulting fact pins both
// rows by observed row identity AND version (or deliberate absence) through
// MembershipTargets, project row first and tenant-wide row second. The commit
// boundary compares the confirmed pins with those the fact captured and
// re-evaluates the current union, refusing version_conflict on any pin change
// and forbidden when the current union no longer grants the required role.

const membershipPrincipal = id.PrincipalID("principal-bob")

// membershipRow builds one observed membership row. An empty rowID is an
// absent row; a present row carries its memberships.id identity plus its
// current version and roles.
func membershipRow(scope tenancy.Scope, rowID string, version uint64, roles ...tenancy.Role) authorize.MembershipRow {
	return membershipRowFor(membershipPrincipal, scope, rowID, version, roles...)
}

func membershipRowFor(principal id.PrincipalID, scope tenancy.Scope, rowID string, version uint64, roles ...tenancy.Role) authorize.MembershipRow {
	if rowID == "" {
		return authorize.MembershipRow{}
	}
	return authorize.MembershipRow{
		RowID: rowID,
		Membership: &tenancy.Membership{
			Scope:       scope,
			PrincipalID: principal,
			Roles:       append([]tenancy.Role(nil), roles...),
			Version:     version,
		},
	}
}

// humanMembershipRead builds one human membership authority read.
func humanMembershipRead(scope tenancy.Scope, roles ...tenancy.Role) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: membershipPrincipal,
		Required:    authorize.Requires{HumanRoles: append([]tenancy.Role(nil), roles...)},
	}
}

// serviceMembershipRead builds one service membership authority read.
func serviceMembershipRead(scope tenancy.Scope, roles ...tenancy.Role) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: membershipPrincipal,
		Required:    authorize.Requires{ServiceRoles: append([]tenancy.Role(nil), roles...)},
	}
}

// TestMembershipUnionGrantsBothRows pins the union: Bob is tenant-wide Engineer
// and project Reviewer, so the engineer-only task.create grant and a
// reviewer-only grant both pass, and neither row narrows the other.
func TestMembershipUnionGrantsBothRows(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
		Project:    membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleReviewer),
	}

	engineer := operationNamed(t, "task.create").Requires
	fact, err := authorize.EvaluateMembership(authorize.AuthorityRead{
		Operation:   "task.create",
		Required:    engineer,
		Check:       authorize.CheckMembership,
		Scope:       projectScope(),
		PrincipalID: membershipPrincipal,
	}, false, rows)
	requireNoError(t, err)
	if !fact.Allowed {
		t.Fatalf("tenant-wide Engineer + project Reviewer did not grant task.create: %+v", fact)
	}

	reviewerOnly := humanMembershipRead(projectScope(), tenancy.RoleReviewer)
	fact, err = authorize.EvaluateMembership(reviewerOnly, false, rows)
	requireNoError(t, err)
	if !fact.Allowed {
		t.Fatalf("project Reviewer did not grant a reviewer-only operation: %+v", fact)
	}

	// Carol's tenant-wide Admin survives a project Viewer row.
	carolTenant := membershipRowFor("principal-carol", tenantScope(), "row-tenant-carol", 2, tenancy.RoleAdmin)
	carolProject := membershipRowFor("principal-carol", projectScope(), "row-project-carol", 5, tenancy.RoleViewer)
	carolRows := authorize.MembershipRows{TenantWide: carolTenant, Project: carolProject}
	carolRead := humanMembershipRead(projectScope(), tenancy.RoleAdmin)
	carolRead.PrincipalID = "principal-carol"
	fact, err = authorize.EvaluateMembership(carolRead, false, carolRows)
	requireNoError(t, err)
	if !fact.Allowed {
		t.Fatalf("tenant-wide Admin + project Viewer lost the Admin grant: %+v", fact)
	}
}

// TestMembershipProjectOnlyRowGrantsNoTenantRight pins that a tenant-scoped
// read never consults the project row.
func TestMembershipProjectOnlyRowGrantsNoTenantRight(t *testing.T) {
	rows := authorize.MembershipRows{
		Project: membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleAdmin),
	}
	read := humanMembershipRead(tenantScope(), tenancy.RoleAdmin)
	fact, err := authorize.EvaluateMembership(read, false, rows)
	wantFaultClass(t, err, fault.Forbidden)
	if fact.Allowed {
		t.Fatalf("project-only membership granted a tenant-scoped right: %+v", fact)
	}
	if len(fact.Targets) != 1 || fact.Targets[0].Scope != tenantScope() || !fact.Targets[0].Absent {
		t.Fatalf("tenant-scoped fact targets = %+v; want only the absent tenant-wide pin", fact.Targets)
	}
}

// TestMembershipTenantScopeIgnoresProjectRow pins that a project row cannot add
// or shadow anything in a tenant-scoped read.
func TestMembershipTenantScopeIgnoresProjectRow(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 2, tenancy.RoleEngineer),
		Project:    membershipRow(projectScope(), "row-project-bob", 5, tenancy.RoleAdmin),
	}
	fact, err := authorize.EvaluateMembership(humanMembershipRead(tenantScope(), tenancy.RoleAdmin), false, rows)
	wantFaultClass(t, err, fault.Forbidden)
	if len(fact.Targets) != 1 {
		t.Fatalf("tenant-scoped read pinned %d targets; want only the tenant-wide row", len(fact.Targets))
	}
	if fact.Targets[0].RowID != "row-tenant-bob" || fact.Targets[0].ExpectedVersion != 2 {
		t.Fatalf("tenant target = %+v; want the observed tenant-wide row", fact.Targets[0])
	}

	fact, err = authorize.EvaluateMembership(humanMembershipRead(tenantScope(), tenancy.RoleEngineer), false, rows)
	requireNoError(t, err)
	if !fact.Allowed {
		t.Fatalf("tenant-wide Engineer did not grant a tenant-scoped engineer operation: %+v", fact)
	}
}

// TestMembershipServiceNeverGainsAdmin pins the service variant: Admin is
// removed from the effective set even from a corrupt row.
func TestMembershipServiceNeverGainsAdmin(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-svc", 3, tenancy.RoleAdmin, tenancy.RoleEngineer),
		Project:    membershipRow(projectScope(), "row-project-svc", 7, tenancy.RoleReviewer),
	}
	fact, err := authorize.EvaluateMembership(serviceMembershipRead(projectScope(), tenancy.RoleAdmin), true, rows)
	wantFaultClass(t, err, fault.Forbidden)
	if fact.Allowed {
		t.Fatalf("a service gained Admin from a corrupt row: %+v", fact)
	}

	fact, err = authorize.EvaluateMembership(serviceMembershipRead(projectScope(), tenancy.RoleEngineer), true, rows)
	requireNoError(t, err)
	if !fact.Allowed {
		t.Fatalf("service did not receive its engineer grant through the union: %+v", fact)
	}
}

// TestMembershipBothAbsentIsForbidden pins the no-membership refusal.
func TestMembershipBothAbsentIsForbidden(t *testing.T) {
	fact, err := authorize.EvaluateMembership(humanMembershipRead(projectScope(), tenancy.RoleViewer), false, authorize.MembershipRows{})
	wantFaultClass(t, err, fault.Forbidden)
	if fact.Allowed {
		t.Fatalf("no membership granted a right: %+v", fact)
	}
	if len(fact.Targets) != 2 || !fact.Targets[0].Absent || !fact.Targets[1].Absent {
		t.Fatalf("targets = %+v; want two explicit absence pins", fact.Targets)
	}

	// The fact carries the observed pins on the read the consumer stores, and
	// confirming the same absent rows re-evaluates the union and refuses.
	pinned := fact.PinnedRead()
	if len(pinned.Targets) != 2 || !pinned.Targets[0].Absent || !pinned.Targets[1].Absent {
		t.Fatalf("PinnedRead targets = %+v; want two explicit absence pins", pinned.Targets)
	}
	err = authorize.ConfirmMembership(pinned, false, authorize.MembershipRows{})
	wantFaultClass(t, err, fault.Forbidden)
}

// TestMembershipForeignScopeRowsIgnored pins that a row outside the read's
// scope is treated as absent and never contributes roles or a pin.
func TestMembershipForeignScopeRowsIgnored(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
		Project:    membershipRow(otherProjectScope(), "row-foreign", 1, tenancy.RoleAdmin),
	}
	fact, err := authorize.EvaluateMembership(humanMembershipRead(projectScope(), tenancy.RoleAdmin), false, rows)
	wantFaultClass(t, err, fault.Forbidden)
	if fact.Targets[0].Scope != projectScope() || !fact.Targets[0].Absent || fact.Targets[0].RowID != "" {
		t.Fatalf("foreign project row became the project pin: %+v", fact.Targets[0])
	}

	// A foreign tenant row in the tenant-wide slot is ignored too.
	rows.TenantWide = membershipRow(tenancy.Scope{TenantID: otherTenant}, "row-other-tenant", 1, tenancy.RoleAdmin)
	rows.Project = membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleReviewer)
	fact, err = authorize.EvaluateMembership(humanMembershipRead(projectScope(), tenancy.RoleAdmin), false, rows)
	wantFaultClass(t, err, fault.Forbidden)
	if !fact.Targets[1].Absent {
		t.Fatalf("foreign tenant row became the tenant-wide pin: %+v", fact.Targets[1])
	}
}

// TestMembershipEmptyRoleRowIsMemberWithoutRights pins that an applicable row
// with no roles is a membership (not a no-membership refusal) that grants
// nothing.
func TestMembershipEmptyRoleRowIsMemberWithoutRights(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 3),
	}
	fact, err := authorize.EvaluateMembership(humanMembershipRead(projectScope(), tenancy.RoleViewer), false, rows)
	wantFaultClass(t, err, fault.Forbidden)
	if fact.Allowed {
		t.Fatalf("empty-role membership granted a right: %+v", fact)
	}
	if fact.Targets[1].Absent || fact.Targets[1].RowID != "row-tenant-bob" {
		t.Fatalf("tenant-wide pin = %+v; want the observed empty-role row", fact.Targets[1])
	}
}

// TestMembershipFactPinsBothRowsExactly pins the target list: project first,
// tenant-wide second, each carrying the observed identity and version or an
// explicit absence.
func TestMembershipFactPinsBothRowsExactly(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
		Project:    membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleReviewer),
	}
	read := humanMembershipRead(projectScope(), tenancy.RoleEngineer)
	fact, err := authorize.EvaluateMembership(read, false, rows)
	requireNoError(t, err)
	if !fact.Allowed {
		t.Fatalf("union did not grant the engineer role: %+v", fact)
	}
	want := []authorize.AuthorityTarget{
		{Scope: projectScope(), RowID: "row-project-bob", ExpectedVersion: 9},
		{Scope: tenantScope(), RowID: "row-tenant-bob", ExpectedVersion: 4},
	}
	if !reflect.DeepEqual(fact.Targets, want) {
		t.Fatalf("targets = %#v; want %#v", fact.Targets, want)
	}

	// An absent project row is an explicit absence pin, and the tenant-wide
	// row keeps its own identity and version.
	rows.Project = authorize.MembershipRow{}
	fact, err = authorize.EvaluateMembership(read, false, rows)
	requireNoError(t, err)
	want[0] = authorize.AuthorityTarget{Scope: projectScope(), Absent: true}
	if !reflect.DeepEqual(fact.Targets, want) {
		t.Fatalf("absence targets = %#v; want %#v", fact.Targets, want)
	}

	// A present applicable row without its observed identity cannot be pinned
	// as a wildcard.
	invalid := authorize.MembershipRows{
		TenantWide: authorize.MembershipRow{
			Membership: &tenancy.Membership{
				Scope:       tenantScope(),
				PrincipalID: membershipPrincipal,
				Roles:       []tenancy.Role{tenancy.RoleEngineer},
				Version:     4,
			},
		},
	}
	_, err = authorize.EvaluateMembership(read, false, invalid)
	wantFaultClass(t, err, fault.InternalFailure)
}

// membershipCommitFixture observes the two rows, evaluates the fact and pins
// the read exactly as a consumer would before the commit boundary.
func membershipCommitFixture(t *testing.T, rows authorize.MembershipRows) (authorize.AuthorityRead, authorize.MembershipFact) {
	t.Helper()
	read := humanMembershipRead(projectScope(), tenancy.RoleEngineer)
	fact, err := authorize.EvaluateMembership(read, false, rows)
	requireNoError(t, err)
	if !fact.Allowed {
		t.Fatalf("fixture fact is not allowed: %+v", fact)
	}
	return fact.PinnedRead(), fact
}

// TestConfirmMembershipAcceptsUnchangedRows proves an unchanged pin set and a
// still-granting union commit.
func TestConfirmMembershipAcceptsUnchangedRows(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
		Project:    membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleReviewer),
	}
	read, _ := membershipCommitFixture(t, rows)
	requireNoError(t, authorize.ConfirmMembership(read, false, rows))
}

// TestConfirmMembershipRefusesTargetChanges sweeps every pin change: a bumped
// row, a deleted and recreated row identity at the same version, an absence
// that became a row, a row that became absent, and a tenant-row change while
// the project row is unchanged.
func TestConfirmMembershipRefusesTargetChanges(t *testing.T) {
	baseRows := func() authorize.MembershipRows {
		return authorize.MembershipRows{
			TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
			Project:    membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleReviewer),
		}
	}

	cases := []struct {
		name    string
		mutate  func(*authorize.MembershipRows)
		wantErr fault.Class
	}{
		{
			name: "tenant row bumped",
			mutate: func(rows *authorize.MembershipRows) {
				rows.TenantWide = membershipRow(tenantScope(), "row-tenant-bob", 5, tenancy.RoleEngineer)
			},
			wantErr: fault.VersionConflict,
		},
		{
			name: "project row bumped",
			mutate: func(rows *authorize.MembershipRows) {
				rows.Project = membershipRow(projectScope(), "row-project-bob", 10, tenancy.RoleReviewer)
			},
			wantErr: fault.VersionConflict,
		},
		{
			name: "project row deleted and recreated at the same version",
			mutate: func(rows *authorize.MembershipRows) {
				rows.Project = membershipRow(projectScope(), "row-project-recreated", 9, tenancy.RoleReviewer)
			},
			wantErr: fault.VersionConflict,
		},
		{
			name: "tenant row deleted and recreated at the same version",
			mutate: func(rows *authorize.MembershipRows) {
				rows.TenantWide = membershipRow(tenantScope(), "row-tenant-recreated", 4, tenancy.RoleEngineer)
			},
			wantErr: fault.VersionConflict,
		},
		{
			name: "tenant row changed while the project row is unchanged",
			mutate: func(rows *authorize.MembershipRows) {
				rows.TenantWide = membershipRow(tenantScope(), "row-tenant-bob", 5, tenancy.RoleEngineer)
			},
			wantErr: fault.VersionConflict,
		},
		{
			name: "present row became absent",
			mutate: func(rows *authorize.MembershipRows) {
				rows.Project = authorize.MembershipRow{}
			},
			wantErr: fault.VersionConflict,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			read, _ := membershipCommitFixture(t, baseRows())
			confirmed := baseRows()
			testCase.mutate(&confirmed)
			err := authorize.ConfirmMembership(read, false, confirmed)
			wantFaultClass(t, err, testCase.wantErr)
		})
	}

	// A pinned absence that became a row is a version_conflict.
	absentRows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
	}
	read, _ := membershipCommitFixture(t, absentRows)
	confirmed := absentRows
	confirmed.Project = membershipRow(projectScope(), "row-project-new", 1, tenancy.RoleReviewer)
	wantFaultClass(t, authorize.ConfirmMembership(read, false, confirmed), fault.VersionConflict)
}

// TestConfirmMembershipRefusesSameVersionRoleRewrite pins the second guard: no
// pin changed, but the current union no longer grants the required role, so the
// commit is forbidden rather than accepted on version equality.
func TestConfirmMembershipRefusesSameVersionRoleRewrite(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
		Project:    membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleReviewer),
	}
	read, _ := membershipCommitFixture(t, rows)

	rewritten := rows
	rewritten.TenantWide = membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleViewer)
	rewritten.Project = membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleViewer)
	wantFaultClass(t, authorize.ConfirmMembership(read, false, rewritten), fault.Forbidden)
}

// TestConfirmMembershipRequiresObservedTargets pins the fail-closed rule: a
// membership read without the observed pins cannot be confirmed against a
// version alone.
func TestConfirmMembershipRequiresObservedTargets(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
	}
	read := humanMembershipRead(projectScope(), tenancy.RoleEngineer)
	wantFaultClass(t, authorize.ConfirmMembership(read, false, rows), fault.InvalidInput)
}

// TestMembershipEvaluationIsDeterministicAndReadOnly pins that repeated
// evaluation returns the same fact and never mutates the observed rows.
func TestMembershipEvaluationIsDeterministicAndReadOnly(t *testing.T) {
	rows := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
		Project:    membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleReviewer),
	}
	before := authorize.MembershipRows{
		TenantWide: membershipRow(tenantScope(), "row-tenant-bob", 4, tenancy.RoleEngineer),
		Project:    membershipRow(projectScope(), "row-project-bob", 9, tenancy.RoleReviewer),
	}
	read := humanMembershipRead(projectScope(), tenancy.RoleEngineer)

	first, err := authorize.EvaluateMembership(read, false, rows)
	requireNoError(t, err)
	for iteration := range 10 {
		next, err := authorize.EvaluateMembership(read, false, rows)
		requireNoError(t, err)
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("evaluation %d differs:\n got %#v\nwant %#v", iteration, next, first)
		}
	}
	if !reflect.DeepEqual(rows, before) {
		t.Fatalf("evaluation mutated the observed rows:\n got %#v\nwant %#v", rows, before)
	}
	if len(read.Targets) != 0 {
		t.Fatalf("evaluation mutated the read targets: %+v", read.Targets)
	}
}

// TestMembershipReadPushAdminRequiresOnlyAdmin pins the single definition of
// the conditional push-service Admin read: it is the membership union with
// Admin required for a human, Admin (never assignable) for a service, and no
// execution permission, whatever grant the operation itself declares.
func TestMembershipReadPushAdminRequiresOnlyAdmin(t *testing.T) {
	read := authorize.AuthorityRead{
		Operation: "pipeline.update",
		Required: authorize.Requires{
			HumanRoles:   []tenancy.Role{tenancy.RoleEngineer},
			ServiceRoles: []tenancy.Role{tenancy.RoleEngineer},
			Execution:    authorize.ExecOwnTaskContent,
		},
		Check:       authorize.CheckPushServiceAdmin,
		Scope:       tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"},
		PrincipalID: "principal-a",
	}
	evaluated := authorize.MembershipRead(read)
	if evaluated.Check != authorize.CheckMembership {
		t.Errorf("check = %q; want membership", evaluated.Check)
	}
	if !slices.Equal(evaluated.Required.HumanRoles, []tenancy.Role{tenancy.RoleAdmin}) ||
		!slices.Equal(evaluated.Required.ServiceRoles, []tenancy.Role{tenancy.RoleAdmin}) {
		t.Errorf("required roles = %+v; want Admin only", evaluated.Required)
	}
	if evaluated.Required.Execution != authorize.ExecNone {
		t.Errorf("execution permission = %q; want none", evaluated.Required.Execution)
	}
	if read.Check != authorize.CheckPushServiceAdmin || read.Required.Execution != authorize.ExecOwnTaskContent ||
		read.Required.HumanRoles[0] != tenancy.RoleEngineer {
		t.Errorf("the caller's read was mutated: %+v", read)
	}

	membership := read
	membership.Check = authorize.CheckMembership
	if kept := authorize.MembershipRead(membership); kept.Required.Execution != authorize.ExecOwnTaskContent ||
		!slices.Equal(kept.Required.HumanRoles, []tenancy.Role{tenancy.RoleEngineer}) {
		t.Errorf("a plain membership read lost its own grant: %+v", kept.Required)
	}
}

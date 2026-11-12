package domaincontract

// Authority-target vectors: both membership rows are pinned.
//
// A project-scoped `membership` authority check resolves the union of two
// rows, so a single lock is not enough: a concurrent grant or revoke of EITHER
// the exact-project row or the tenant-wide row between confirmation and commit
// must be a version_conflict, including the creation of a row that was ABSENT
// at confirmation.
//
// The authority read therefore carries a target list:
//
//	authorize.AuthorityTarget{
//	    Scope           tenancy.Scope // exact project, or tenant-wide (no project)
//	    RowID           string        // observed memberships.id; empty when Absent
//	    ExpectedVersion uint64        // observed version; zero when Absent
//	    Absent          bool          // the row was observed absent
//	}
//
//	authorize.AuthorityRead.Targets []authorize.AuthorityTarget // json "targets,omitempty"
//
// and every membership adapter must build it with the single declared builder:
//
//	authorize.MembershipTargets(scope tenancy.Scope,
//	    tenantWide, project *tenancy.Membership,
//	    tenantWideRowID, projectRowID string) []authorize.AuthorityTarget
//
// A project-scoped read produces TWO targets, project row first and tenant-wide
// row second. A tenant-scoped read produces ONE target (tenant-wide); the
// project row is never pinned. Version equality alone is not row identity: a
// revoke deletes the row and a re-grant creates a NEW row identity with its own
// version (normally 1 again), so a pin that carried only the version would
// accept a recreated row. A present pin therefore carries the observed row
// identity AND version together; absence pins carry neither.
//
// The reference in-memory store below is a contract model, not an adapter. It
// proves the declared shape is sufficient to enforce the both-row rule; the
// production guards are not exercised here.

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestAuthorityReadDeclaresMembershipTargets pins the exact declaration shape.
// A consumer must be able to read `Targets` from an authority read, and each
// target must carry the scope, the observed version and the
// deliberate-absence bit.
func TestAuthorityReadDeclaresMembershipTargets(t *testing.T) {
	readType := reflect.TypeOf(authorize.AuthorityRead{})
	targetsField, ok := readType.FieldByName("Targets")
	if !ok {
		t.Fatalf("authorize.AuthorityRead does not declare Targets; a project-scoped membership read must pin both rows")
	}
	wantTargets := reflect.TypeOf([]authorize.AuthorityTarget(nil))
	if targetsField.Type != wantTargets {
		t.Fatalf("AuthorityRead.Targets type = %v; want %v", targetsField.Type, wantTargets)
	}
	if got := targetsField.Tag.Get("json"); got != "targets,omitempty" {
		t.Fatalf("AuthorityRead.Targets json tag = %q; want %q", got, "targets,omitempty")
	}

	targetType := reflect.TypeOf(authorize.AuthorityTarget{})
	fields := []struct {
		name string
		typ  reflect.Type
		tag  string
	}{
		{name: "Scope", typ: reflect.TypeOf(tenancy.Scope{}), tag: "scope"},
		{name: "RowID", typ: reflect.TypeOf(""), tag: "row_id,omitempty"},
		{name: "ExpectedVersion", typ: reflect.TypeOf(uint64(0)), tag: "expected_version,omitempty"},
		{name: "Absent", typ: reflect.TypeOf(false), tag: "absent,omitempty"},
	}
	for _, want := range fields {
		field, ok := targetType.FieldByName(want.name)
		if !ok {
			t.Errorf("authorize.AuthorityTarget does not declare %s", want.name)
			continue
		}
		if field.Type != want.typ {
			t.Errorf("AuthorityTarget.%s type = %v; want %v", want.name, field.Type, want.typ)
		}
		if got := field.Tag.Get("json"); got != want.tag {
			t.Errorf("AuthorityTarget.%s json tag = %q; want %q", want.name, got, want.tag)
		}
	}
}

// TestMembershipTargetsPinBothRows pins the single builder: a project-scoped
// call yields the exact-project target and the tenant-wide target in that
// order; a tenant-scoped call yields only the tenant-wide target.
func TestMembershipTargetsPinBothRows(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	bobTenant := member(tenant, "principal-bob", 4, tenancy.RoleEngineer)
	bobProject := member(project, "principal-bob", 9, tenancy.RoleReviewer)

	targets := authorize.MembershipTargets(project, bobTenant, bobProject, "row-tenant-bob", "row-project-bob")
	if len(targets) != 2 {
		t.Fatalf("project-scoped MembershipTargets returned %d targets; want 2 (project row + tenant-wide row)", len(targets))
	}
	if targets[0].Scope != project {
		t.Errorf("first target scope = %+v; want the exact project %+v", targets[0].Scope, project)
	}
	if targets[0].Absent || targets[0].ExpectedVersion != bobProject.Version || targets[0].RowID != "row-project-bob" {
		t.Errorf("project target = %+v; want present at row id %q version %d", targets[0], "row-project-bob", bobProject.Version)
	}
	if targets[1].Scope != tenant {
		t.Errorf("second target scope = %+v; want the tenant-wide scope %+v", targets[1].Scope, tenant)
	}
	if targets[1].Absent || targets[1].ExpectedVersion != bobTenant.Version || targets[1].RowID != "row-tenant-bob" {
		t.Errorf("tenant-wide target = %+v; want present at row id %q version %d", targets[1], "row-tenant-bob", bobTenant.Version)
	}

	tenantTargets := authorize.MembershipTargets(tenant, bobTenant, bobProject, "row-tenant-bob", "row-project-bob")
	if len(tenantTargets) != 1 {
		t.Fatalf("tenant-scoped MembershipTargets returned %d targets; want only the tenant-wide row", len(tenantTargets))
	}
	if tenantTargets[0].Scope != tenant || tenantTargets[0].Absent || tenantTargets[0].ExpectedVersion != bobTenant.Version || tenantTargets[0].RowID != "row-tenant-bob" {
		t.Fatalf("tenant-scoped target = %+v; want the tenant-wide row %q at version %d", tenantTargets[0], "row-tenant-bob", bobTenant.Version)
	}
}

// TestMembershipTargetsMarkAbsence pins that an absent applicable row is an
// explicit absence pin, and that a row outside the requested scope is not
// silently used as the target.
func TestMembershipTargetsMarkAbsence(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	targets := authorize.MembershipTargets(project, nil, member(project, "principal-1", 3, tenancy.RoleEngineer), "", "row-project-1")
	if len(targets) != 2 {
		t.Fatalf("targets = %d; want 2", len(targets))
	}
	if !targets[1].Absent || targets[1].ExpectedVersion != 0 || targets[1].RowID != "" || targets[1].Scope != tenant {
		t.Fatalf("absent tenant-wide target = %+v; want Absent with no row id at the tenant scope", targets[1])
	}
	if targets[0].Absent || targets[0].RowID != "row-project-1" {
		t.Fatalf("project target = %+v; want the observed row id", targets[0])
	}

	targets = authorize.MembershipTargets(project, member(tenant, "principal-1", 4, tenancy.RoleEngineer), nil, "row-tenant-1", "")
	if len(targets) != 2 {
		t.Fatalf("targets = %d; want 2", len(targets))
	}
	if !targets[0].Absent || targets[0].ExpectedVersion != 0 || targets[0].RowID != "" || targets[0].Scope != project {
		t.Fatalf("absent project target = %+v; want Absent with no row id at the project scope", targets[0])
	}
	if targets[1].Absent || targets[1].RowID != "row-tenant-1" {
		t.Fatalf("tenant-wide target = %+v; want the observed row id", targets[1])
	}

	// A membership row from another project is not the exact-project target.
	targets = authorize.MembershipTargets(project, nil, member(otherProjectScope(), "principal-1", 5, tenancy.RoleAdmin), "", "row-other-project")
	if !targets[0].Absent || targets[0].RowID != "" {
		t.Fatalf("a row for another project was pinned as the project target: %+v", targets[0])
	}

	// A project-scoped row passed as the tenant-wide row is not a tenant-wide
	// target.
	targets = authorize.MembershipTargets(project, member(project, "principal-1", 6, tenancy.RoleAdmin), nil, "row-wrong-scope", "")
	if !targets[1].Absent || targets[1].RowID != "" {
		t.Fatalf("a project-scoped row was pinned as the tenant-wide target: %+v", targets[1])
	}
}

// membershipRecord is one stored membership row: its memberships.id identity
// plus the current grant value.
type membershipRecord struct {
	ID         string
	Membership *tenancy.Membership
}

// membershipFixture is the reference in-memory store. It holds at most one row
// per (scope, principal), matching the memberships uniqueness key, and every
// row keeps its own identity.
type membershipFixture struct {
	rows map[string]membershipRecord
}

func newMembershipFixture() *membershipFixture {
	return &membershipFixture{rows: make(map[string]membershipRecord)}
}

func fixtureKey(scope tenancy.Scope, principal string) string {
	return string(scope.TenantID) + "\x00" + string(scope.ProjectID) + "\x00" + principal
}

// put stores a row under a deterministic per-(scope, principal, version)
// identity. Tests that exercise identity reuse call putID explicitly.
func (fixture *membershipFixture) put(row *tenancy.Membership) {
	fixture.putID(fmt.Sprintf("row-%s-%s-%s-%d", row.Scope.TenantID, row.Scope.ProjectID, row.PrincipalID, row.Version), row)
}

// putID stores a row under an explicit memberships.id identity.
func (fixture *membershipFixture) putID(id string, row *tenancy.Membership) {
	fixture.rows[fixtureKey(row.Scope, string(row.PrincipalID))] = membershipRecord{ID: id, Membership: row}
}

func (fixture *membershipFixture) delete(scope tenancy.Scope, principal string) {
	delete(fixture.rows, fixtureKey(scope, principal))
}

func (fixture *membershipFixture) bump(scope tenancy.Scope, principal string, roles ...tenancy.Role) {
	key := fixtureKey(scope, principal)
	if record, ok := fixture.rows[key]; ok {
		updated := *record.Membership
		updated.Version++
		updated.Roles = append([]tenancy.Role(nil), roles...)
		fixture.rows[key] = membershipRecord{ID: record.ID, Membership: &updated}
	}
}

func (fixture *membershipFixture) record(scope tenancy.Scope, principal string) (membershipRecord, bool) {
	record, ok := fixture.rows[fixtureKey(scope, principal)]
	return record, ok
}

func (fixture *membershipFixture) row(scope tenancy.Scope, principal string) *tenancy.Membership {
	record, ok := fixture.record(scope, principal)
	if !ok {
		return nil
	}
	return record.Membership
}

// targets observes the two rows the way a capability does at confirmation and
// returns the read's pins, carrying each observed row identity.
func (fixture *membershipFixture) targets(scope tenancy.Scope, principal string) []authorize.AuthorityTarget {
	tenantRecord, tenantFound := fixture.record(tenancy.Scope{TenantID: scope.TenantID}, principal)
	tenantWide, tenantWideID := (*tenancy.Membership)(nil), ""
	if tenantFound {
		tenantWide, tenantWideID = tenantRecord.Membership, tenantRecord.ID
	}
	var projectRow *tenancy.Membership
	projectRowID := ""
	if scope.ProjectScoped() {
		if projectRecord, found := fixture.record(scope, principal); found {
			projectRow, projectRowID = projectRecord.Membership, projectRecord.ID
		}
	}
	return authorize.MembershipTargets(scope, tenantWide, projectRow, tenantWideID, projectRowID)
}

// commit re-reads the pinned targets and then the effective role union. A
// pinned row that disappeared, a pinned absence that became a row, or a pinned
// row identity OR version that moved is a version_conflict: a revoked row can
// be re-granted with a NEW identity at the same version, so version equality
// alone must never satisfy the pin. Only an unchanged target set reaches the
// role decision, which re-evaluates the union on the current rows and refuses
// with forbidden when it no longer grants a required role (a same-identity,
// same-version role change is still refused here).
func (fixture *membershipFixture) commit(scope tenancy.Scope, principal string, required []tenancy.Role, targets []authorize.AuthorityTarget) error {
	for _, target := range targets {
		record, found := fixture.record(target.Scope, principal)
		switch {
		case target.Absent && found:
			return fault.New(fault.VersionConflict, "a membership row that was absent at confirmation now exists")
		case !target.Absent && !found:
			return fault.New(fault.VersionConflict, "a membership row pinned at confirmation is missing")
		case !target.Absent && target.RowID == "":
			return fault.New(fault.VersionConflict, "a present membership pin carries no row identity")
		case !target.Absent && record.ID != target.RowID:
			return fault.New(fault.VersionConflict, "a membership pin names a different row identity")
		case !target.Absent && record.Membership.Version != target.ExpectedVersion:
			return fault.New(fault.VersionConflict, "a membership row changed version after confirmation")
		}
	}
	tenantWide := fixture.row(tenancy.Scope{TenantID: scope.TenantID}, principal)
	var projectRow *tenancy.Membership
	if scope.ProjectScoped() {
		projectRow = fixture.row(scope, principal)
	}
	roles, ok := tenancy.EffectiveRoles(scope, tenantWide, projectRow)
	if !ok {
		return fault.New(fault.Forbidden, "no membership is present in scope")
	}
	for _, want := range required {
		for _, held := range roles {
			if want == held {
				return nil
			}
		}
	}
	return fault.New(fault.Forbidden, "the effective membership no longer grants the required role")
}

// TestMembershipCommitPinsBothRows exercises the both-row settlement rule:
// grant, revoke or role change of EITHER row after confirmation is a
// version_conflict, an absence pin catches a row created after confirmation,
// and only an unchanged target set reaches the role-union decision.
func TestMembershipCommitPinsBothRows(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	bobTenant := member(tenant, "principal-bob", 4, tenancy.RoleEngineer)
	bobProject := member(project, "principal-bob", 9, tenancy.RoleReviewer)
	carolTenant := member(tenant, "principal-carol", 2, tenancy.RoleAdmin)
	carolProject := member(project, "principal-carol", 5, tenancy.RoleViewer)

	cases := []struct {
		name      string
		scope     tenancy.Scope
		principal string
		initial   []*tenancy.Membership
		required  []tenancy.Role
		mutate    func(*membershipFixture)
		wantClass fault.Class // empty means the commit is accepted
	}{
		{
			name:      "both rows unchanged grant the union",
			scope:     project,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobTenant, bobProject},
			required:  []tenancy.Role{tenancy.RoleReviewer},
		},
		{
			name:      "Carol keeps the tenant-wide admin contribution",
			scope:     project,
			principal: "principal-carol",
			initial:   []*tenancy.Membership{carolTenant, carolProject},
			required:  []tenancy.Role{tenancy.RoleAdmin},
		},
		{
			name:      "tenant-wide row revoked after confirmation",
			scope:     project,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobTenant, bobProject},
			required:  []tenancy.Role{tenancy.RoleEngineer},
			mutate: func(fixture *membershipFixture) {
				fixture.delete(tenant, "principal-bob")
			},
			wantClass: fault.VersionConflict,
		},
		{
			name:      "project row revoked after confirmation",
			scope:     project,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobTenant, bobProject},
			required:  []tenancy.Role{tenancy.RoleEngineer},
			mutate: func(fixture *membershipFixture) {
				fixture.delete(project, "principal-bob")
			},
			wantClass: fault.VersionConflict,
		},
		{
			name:      "tenant-wide role change after confirmation",
			scope:     project,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobTenant, bobProject},
			required:  []tenancy.Role{tenancy.RoleEngineer},
			mutate: func(fixture *membershipFixture) {
				fixture.bump(tenant, "principal-bob", tenancy.RoleEngineer, tenancy.RoleAdmin)
			},
			wantClass: fault.VersionConflict,
		},
		{
			name:      "project role change after confirmation",
			scope:     project,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobTenant, bobProject},
			required:  []tenancy.Role{tenancy.RoleReviewer},
			mutate: func(fixture *membershipFixture) {
				fixture.bump(project, "principal-bob", tenancy.RoleViewer)
			},
			wantClass: fault.VersionConflict,
		},
		{
			name:      "tenant-wide row created after an absence pin",
			scope:     project,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobProject},
			required:  []tenancy.Role{tenancy.RoleReviewer},
			mutate: func(fixture *membershipFixture) {
				fixture.put(member(tenant, "principal-bob", 1, tenancy.RoleAdmin))
			},
			wantClass: fault.VersionConflict,
		},
		{
			name:      "project row created after an absence pin",
			scope:     project,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobTenant},
			required:  []tenancy.Role{tenancy.RoleEngineer},
			mutate: func(fixture *membershipFixture) {
				fixture.put(member(project, "principal-bob", 1, tenancy.RoleAdmin))
			},
			wantClass: fault.VersionConflict,
		},
		{
			name:      "no membership at confirmation or commit is forbidden",
			scope:     project,
			principal: "principal-nobody",
			required:  []tenancy.Role{tenancy.RoleViewer},
			wantClass: fault.Forbidden,
		},
		{
			name:      "tenant-scoped read does not pin or observe the project row",
			scope:     tenant,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobTenant},
			required:  []tenancy.Role{tenancy.RoleEngineer},
			mutate: func(fixture *membershipFixture) {
				fixture.put(member(project, "principal-bob", 1, tenancy.RoleAdmin))
			},
		},
		{
			name:      "a project-only membership grants no tenant-scoped right",
			scope:     tenant,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobProject},
			required:  []tenancy.Role{tenancy.RoleReviewer},
			wantClass: fault.Forbidden,
		},
		{
			name:      "an empty project row does not remove the tenant-wide contribution",
			scope:     project,
			principal: "principal-bob",
			initial:   []*tenancy.Membership{bobTenant, member(project, "principal-bob", 2)},
			required:  []tenancy.Role{tenancy.RoleEngineer},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newMembershipFixture()
			for _, row := range testCase.initial {
				fixture.put(row)
			}
			targets := fixture.targets(testCase.scope, testCase.principal)
			if testCase.scope.ProjectScoped() && len(targets) != 2 {
				t.Fatalf("project-scoped read pinned %d targets; want both rows", len(targets))
			}
			if testCase.scope.TenantScoped() && len(targets) != 1 {
				t.Fatalf("tenant-scoped read pinned %d targets; want only the tenant-wide row", len(targets))
			}
			if testCase.mutate != nil {
				testCase.mutate(fixture)
			}
			err := fixture.commit(testCase.scope, testCase.principal, testCase.required, targets)
			if testCase.wantClass == "" {
				if err != nil {
					t.Fatalf("commit refused an unchanged target set: %v", err)
				}
				return
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != testCase.wantClass {
				t.Fatalf("commit error = %v (class %q, ok %v); want class %q", err, class, ok, testCase.wantClass)
			}
		})
	}
}

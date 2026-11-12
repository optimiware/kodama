package domaincontract

// Regrant and identity vectors: a membership pin must carry the observed row
// identity, not only its version.
//
// An AuthorityTarget carrying only (scope, principal, ExpectedVersion|Absent)
// would not be a row pin: a revoke deletes the row and a re-grant creates a
// NEW membership row identity whose version normally starts again at 1, so a
// version-only pin would accept a row that has nothing to do with the one
// observed at confirmation. The reference fixture below proves the required
// guards:
//
//   - a present pin matches row identity AND version together; a recreated row
//     at the same version is a version_conflict because its identity differs;
//   - the commit unit re-reads both rows and re-evaluates the effective union
//     on the CURRENT rows, refusing forbidden when it no longer grants the
//     required role even when identity and version still match;
//   - a pinned absence that became a row and then disappeared again is not
//     enough for the membership decision: the current union grants nothing;
//   - pins are scoped to the read's principal and scope, so an unrelated row
//     change does not fail the commit.
//
// These are contract-model vectors, not adapter tests: the store adapters own
// the real lock/absence guard.

import (
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestMembershipCommitRefusesRecreatedRowAtSameVersion is the ABA case: the
// observed row is revoked and re-granted with a different role set at the same
// version under a new identity. A version-only pin would pass; the identity pin
// must refuse.
func TestMembershipCommitRefusesRecreatedRowAtSameVersion(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	cases := []struct {
		name      string
		scope     tenancy.Scope
		rowScope  tenancy.Scope
		rowID     string
		roles     []tenancy.Role
		required  []tenancy.Role
		regranted []tenancy.Role
	}{
		{
			name:      "tenant-wide row revoked and re-granted at version one",
			scope:     project,
			rowScope:  tenant,
			rowID:     "row-tenant-bob",
			roles:     []tenancy.Role{tenancy.RoleEngineer},
			required:  []tenancy.Role{tenancy.RoleEngineer},
			regranted: []tenancy.Role{tenancy.RoleViewer},
		},
		{
			name:      "tenant-wide row re-granted with the same roles but a new identity",
			scope:     project,
			rowScope:  tenant,
			rowID:     "row-tenant-bob",
			roles:     []tenancy.Role{tenancy.RoleEngineer},
			required:  []tenancy.Role{tenancy.RoleEngineer},
			regranted: []tenancy.Role{tenancy.RoleEngineer},
		},
		{
			name:      "project row revoked and re-granted at version one",
			scope:     project,
			rowScope:  project,
			rowID:     "row-project-bob",
			roles:     []tenancy.Role{tenancy.RoleReviewer},
			required:  []tenancy.Role{tenancy.RoleReviewer},
			regranted: []tenancy.Role{tenancy.RoleViewer},
		},
		{
			name:      "project row re-granted with the same roles but a new identity",
			scope:     project,
			rowScope:  project,
			rowID:     "row-project-bob",
			roles:     []tenancy.Role{tenancy.RoleReviewer},
			required:  []tenancy.Role{tenancy.RoleReviewer},
			regranted: []tenancy.Role{tenancy.RoleReviewer},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newMembershipFixture()
			fixture.putID(testCase.rowID, member(testCase.rowScope, "principal-bob", 1, testCase.roles...))
			targets := fixture.targets(testCase.scope, "principal-bob")
			fixture.delete(testCase.rowScope, "principal-bob")
			fixture.putID("row-regranted", member(testCase.rowScope, "principal-bob", 1, testCase.regranted...))

			requireClass(t, fixture.commit(testCase.scope, "principal-bob", testCase.required, targets), fault.VersionConflict)
		})
	}
}

// TestMembershipCommitReevaluatesCurrentRolesEvenWhenPinsMatch pins the second
// guard: a contrived write that replaces the role set on the SAME row identity
// at the SAME version passes the pin, so the commit unit must re-evaluate the
// union on the current rows and refuse.
func TestMembershipCommitReevaluatesCurrentRolesEvenWhenPinsMatch(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	fixture := newMembershipFixture()
	fixture.putID("row-tenant-bob", member(tenant, "principal-bob", 1, tenancy.RoleEngineer))
	targets := fixture.targets(project, "principal-bob")

	// Same identity, same version, changed roles: no version conflict is
	// observable, so only re-evaluation can refuse the stale Engineer grant.
	fixture.putID("row-tenant-bob", member(tenant, "principal-bob", 1, tenancy.RoleViewer))

	requireClass(t, fixture.commit(project, "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, targets), fault.Forbidden)
}

// TestMembershipCommitAcceptsAnUnchangedPin guards against a blanket refusal:
// unchanged identity, version and roles commit successfully.
func TestMembershipCommitAcceptsAnUnchangedPin(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	fixture := newMembershipFixture()
	fixture.putID("row-tenant-bob", member(tenant, "principal-bob", 1, tenancy.RoleEngineer))
	targets := fixture.targets(project, "principal-bob")

	if err := fixture.commit(project, "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, targets); err != nil {
		t.Fatalf("commit refused an unchanged membership pin: %v", err)
	}
}

// TestMembershipCommitReevaluatesAnAbsencePinThatReturnsToAbsence sweeps the
// absent -> created -> deleted sequence: the absence pin is satisfied again,
// but the current union has no membership, so the decision must be refused.
func TestMembershipCommitReevaluatesAnAbsencePinThatReturnsToAbsence(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	fixture := newMembershipFixture()
	targets := fixture.targets(project, "principal-bob")
	fixture.putID("row-tenant-bob", member(tenant, "principal-bob", 1, tenancy.RoleEngineer))
	fixture.delete(tenant, "principal-bob")

	requireClass(t, fixture.commit(project, "principal-bob", []tenancy.Role{tenancy.RoleViewer}, targets), fault.Forbidden)
}

// TestMembershipPinsFollowTheReadPrincipal proves the pins are keyed to the
// read's principal: changing another principal's row is not a conflict.
func TestMembershipPinsFollowTheReadPrincipal(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	fixture := newMembershipFixture()
	fixture.putID("row-tenant-bob", member(tenant, "principal-bob", 1, tenancy.RoleEngineer))
	fixture.putID("row-tenant-alice", member(tenant, "principal-alice", 1, tenancy.RoleAdmin))
	targets := fixture.targets(project, "principal-bob")
	fixture.bump(tenant, "principal-alice", tenancy.RoleViewer)

	if err := fixture.commit(project, "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, targets); err != nil {
		t.Fatalf("another principal's membership change failed the read's pin: %v", err)
	}
}

// TestTenantScopedPinIgnoresARecreatedProjectRow sweeps the scope split: a
// tenant-scoped read pins only the tenant-wide row, so deleting and recreating
// the project row under a new identity does not affect it.
func TestTenantScopedPinIgnoresARecreatedProjectRow(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	fixture := newMembershipFixture()
	fixture.putID("row-tenant-bob", member(tenant, "principal-bob", 1, tenancy.RoleEngineer))
	targets := fixture.targets(tenant, "principal-bob")
	fixture.putID("row-project-old", member(project, "principal-bob", 1, tenancy.RoleViewer))
	fixture.delete(project, "principal-bob")
	fixture.putID("row-project-new", member(project, "principal-bob", 1, tenancy.RoleAdmin))

	if err := fixture.commit(tenant, "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, targets); err != nil {
		t.Fatalf("tenant-scoped commit observed a project row: %v", err)
	}
}

// TestMembershipTargetsPresentPinRequiresRowIdentity pins the fail-closed
// shape: the builder carries an empty identity when the caller supplies none,
// and the commit unit must refuse a present target without an identity rather
// than treating it as a wildcard.
func TestMembershipTargetsPresentPinRequiresRowIdentity(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	targets := authorize.MembershipTargets(project, member(tenant, "principal-bob", 1, tenancy.RoleEngineer), nil, "", "")
	if len(targets) != 2 || targets[1].Absent || targets[1].RowID != "" {
		t.Fatalf("targets = %+v; want a present tenant-wide target with no identity", targets)
	}

	fixture := newMembershipFixture()
	fixture.putID("row-tenant-bob", member(tenant, "principal-bob", 1, tenancy.RoleEngineer))
	requireClass(t, fixture.commit(project, "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, targets), fault.VersionConflict)
}

// requireClass asserts the fault class of a reference commit decision.
func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("commit error = %v (class %q, ok %v); want class %q", err, class, ok, want)
	}
}

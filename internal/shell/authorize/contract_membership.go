package authorize

import "github.com/optimiware/kodama/internal/shell/tenancy"

// Membership authority targets.
//
// Under the membership union (tenancy.EffectiveRoles) a project-scoped
// membership check reads BOTH the tenant-wide row and the exact project row.
// Pinning one row is therefore not sufficient: a concurrent grant, revoke or
// role change of EITHER row between confirmation and commit must be a
// version_conflict, including the creation of a row that was absent when the
// fact was read. A version alone is not row identity, because a revoke deletes
// the row and a re-grant creates a new row identity at a restarted version;
// every present pin therefore carries the observed row identity as well.
// Even a matching identity and version is not durable permission: the commit
// unit re-reads both rows and re-evaluates EffectiveRoles against the required
// role inside the unit.
//
// AuthorityTarget is one row pin in an AuthorityRead.
//
// RowID is the observed memberships.id identity of a present row. It is
// required together with ExpectedVersion: a revoke deletes the row and a later
// re-grant creates a NEW row identity whose version normally starts at 1
// again, so a version-only pin would accept a recreated row that has nothing
// to do with the observed grant (ABA). A present pin therefore matches row
// identity AND version together (the store.Lock{IDs, ExpectVersion} shape);
// ExpectedVersion alone is never row identity. RowID is empty only when Absent
// is true.
//
// Absent is the deliberate absence pin: the atomically re-read row must still
// be absent, so a row created after confirmation is a version_conflict.
// ExpectedVersion is the observed version of a present row and zero for an
// absence pin. A present target with an empty RowID is invalid; the commit
// unit must refuse it rather than treat it as a wildcard.
type AuthorityTarget struct {
	Scope           tenancy.Scope `json:"scope"`
	RowID           string        `json:"row_id,omitempty"`
	ExpectedVersion uint64        `json:"expected_version,omitempty"`
	Absent          bool          `json:"absent,omitempty"`
}

// MembershipTargets is the single builder every membership authority adapter
// uses to pin the rows a membership read observed.
//
// A project-scoped call returns the exact-project target first and the
// tenant-wide target second, so the commit unit refuses a change to either
// row. A tenant-scoped call returns only the tenant-wide target; the project
// row is never pinned or observed, so a project-only membership grants no
// tenant-wide right. A nil or inapplicable row becomes an absence pin at that
// target's scope with an empty RowID.
//
// tenantWideRowID and projectRowID are the observed memberships.id identities
// of the corresponding rows. A present row MUST carry its non-empty identity;
// an empty identity on a present row is an invalid pin and the commit unit
// must refuse it. The rows themselves stay the neutral tenancy.Membership
// values, so the neutral tenancy package does not learn a store identity.
func MembershipTargets(scope tenancy.Scope, tenantWide, project *tenancy.Membership, tenantWideRowID, projectRowID string) []AuthorityTarget {
	tenantTarget := AuthorityTarget{Scope: tenancy.Scope{TenantID: scope.TenantID}}
	if membershipTenantWideApplies(scope, tenantWide) {
		tenantTarget.RowID = tenantWideRowID
		tenantTarget.ExpectedVersion = tenantWide.Version
	} else {
		tenantTarget.Absent = true
	}
	if !scope.ProjectScoped() {
		return []AuthorityTarget{tenantTarget}
	}
	projectTarget := AuthorityTarget{Scope: scope}
	if membershipProjectApplies(scope, project) {
		projectTarget.RowID = projectRowID
		projectTarget.ExpectedVersion = project.Version
	} else {
		projectTarget.Absent = true
	}
	return []AuthorityTarget{projectTarget, tenantTarget}
}

// membershipTenantWideApplies mirrors the tenancy applicability rule: only a
// tenant-wide row of the caller's tenant is the tenant-wide contribution.
func membershipTenantWideApplies(scope tenancy.Scope, row *tenancy.Membership) bool {
	return row != nil && row.Scope.TenantScoped() && row.Scope.TenantID == scope.TenantID
}

// membershipProjectApplies mirrors the tenancy applicability rule: only the
// exact project row of a project-scoped caller is the project contribution.
func membershipProjectApplies(scope tenancy.Scope, row *tenancy.Membership) bool {
	return scope.ProjectScoped() && row != nil && row.Scope == scope
}

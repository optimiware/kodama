package authorize

import (
	"context"
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Membership-union authority facts.
//
// A project-scoped membership authority decision observes TWO rows: the
// tenant-wide row of the caller's tenant and the row of the exact project. The
// effective grant is their union (tenancy.EffectiveRoles for a human,
// tenancy.EffectiveServiceRoles for a service), so a project row can only add
// roles and never shadows a tenant-wide grant. A tenant-scoped decision reads
// only the tenant-wide row; a project-only membership therefore grants no
// tenant-scoped right.
//
// The capability-side fact captures the observed pins of both rows through the
// declared MembershipTargets builder: the exact-project target first and the
// tenant-wide target second, each present at the observed memberships.id
// identity AND version, or explicitly absent. A tenant-scoped fact pins only
// the tenant-wide row. The pins travel on AuthorityRead.Targets; the atomic
// commit boundary compares the freshly observed pins with the fact's pins and
// refuses version_conflict on ANY difference of identity, version or absence,
// then re-evaluates the current union and refuses forbidden when it no longer
// grants the operation's required role. Matching pins are never durable
// permission.

// MembershipRow is one membership row exactly as a scoped read observed it.
//
// A nil Membership means the row is absent and carries no identity. A present
// row MUST carry its observed memberships.id identity in RowID together with
// its current Version: a revoked row is deleted and a re-grant creates a NEW
// identity, so a version alone is not row identity. A present applicable row
// with an empty RowID is an invalid observation and is refused rather than
// pinned as a wildcard.
type MembershipRow struct {
	RowID      string
	Membership *tenancy.Membership
}

// MembershipRows is the complete observation of one principal's applicable
// membership rows in one scope. TenantWide is always the tenant-wide row of the
// scope's tenant. Project is the exact-project row and is ignored by a
// tenant-scoped decision.
type MembershipRows struct {
	TenantWide MembershipRow
	Project    MembershipRow
}

// MembershipReader is the consumer-owned, side-effect-free reader a capability
// uses to observe BOTH membership rows before it builds a membership authority
// fact. The identity store adapter implements it over committed state: for a
// project-scoped read it returns the exact-project row and the tenant-wide row,
// each with its memberships.id identity and version, or a nil Membership when
// the row is absent. It never returns a row outside the supplied scope or for
// another principal. A tenant-scoped read returns only the tenant-wide row.
//
// Reading is not authority. The commit unit must re-read both rows, compare the
// confirmed pins with the fact's pins and re-evaluate the union inside the
// atomic unit.
type MembershipReader interface {
	ReadMembership(context.Context, tenancy.Scope, id.PrincipalID) (MembershipRows, error)
}

// MembershipFact is the current membership fact of one principal in one scope.
// It extends the single-fact shape with the operation grant context and the
// observed row pins, so a commit unit can compare the confirmed rows with the
// fact instead of trusting a version alone.
//
// Targets is never empty: a project-scoped fact pins the exact-project row and
// the tenant-wide row (project first), a tenant-scoped fact pins only the
// tenant-wide row. Version on the embedded AuthorityFact is not meaningful for
// a membership union; each target carries its own observed version.
type MembershipFact struct {
	AuthorityFact
	Required Requires          `json:"required,omitempty"`
	Service  bool              `json:"service,omitempty"`
	Targets  []AuthorityTarget `json:"targets,omitempty"`

	read AuthorityRead
}

// PinnedRead returns the authority read this fact was evaluated from with
// Targets replaced by the observed pins. The caller stores that read on its
// commit unit, so the atomic boundary can confirm the fact against the rows it
// re-reads.
func (fact MembershipFact) PinnedRead() AuthorityRead {
	read := fact.read
	read.Targets = append([]AuthorityTarget(nil), fact.Targets...)
	return read
}

// EvaluateMembership evaluates the union of the observed rows against the
// grant set the read's operation requires and returns the fact with both row
// pins populated. service selects the service union and the operation's
// ServiceRoles; otherwise the human union and HumanRoles apply.
//
// A structurally invalid read or an applicable present row without its
// observed identity is invalid_input/internal_failure. A well-formed read whose
// union grants nothing (neither row applies, or the effective roles do not
// include a required role) is a forbidden refusal: the fact is returned for
// diagnostics with Allowed false, and the error class is fault.Forbidden. A
// service never gains Admin, even from a corrupt row.
func EvaluateMembership(read AuthorityRead, service bool, rows MembershipRows) (MembershipFact, error) {
	fact, err := evaluateMembership(read, service, rows)
	if err != nil {
		return fact, err
	}
	if !fact.Allowed {
		return fact, fault.New(fault.Forbidden, fact.Detail)
	}
	return fact, nil
}

// MembershipRead normalizes one membership-family authority read to the
// membership check the union evaluator expects. The conditional push-service
// Admin grant is the same union with Admin as its whole requirement, so a copy
// of that rule in an adapter cannot drift from the union's own definition: a
// human must hold Admin in the effective membership set, a service can never
// hold it, and no execution permission satisfies it whatever the operation
// itself grants an execution. Every other read keeps its own grant.
func MembershipRead(read AuthorityRead) AuthorityRead {
	evaluated := read
	evaluated.Check = CheckMembership
	if read.Check == CheckPushServiceAdmin {
		evaluated.Required = Requires{
			HumanRoles:   []tenancy.Role{tenancy.RoleAdmin},
			ServiceRoles: []tenancy.Role{tenancy.RoleAdmin},
			Execution:    ExecNone,
		}
	}
	return evaluated
}

// ConfirmMembership is the atomic-boundary half of the membership fact. It
// re-reads the confirmed rows the store returned inside the commit unit,
// derives their pins through MembershipTargets, and compares them with the pins
// the read captured at confirmation. Any difference of row identity, version
// or absence is a version_conflict, including a row deleted and re-created at
// the same version. Only an unchanged pin set reaches the union re-evaluation
// against the read's required roles, which refuses forbidden when the current
// rows no longer grant it. A membership read without observed pins is
// invalid_input and is never accepted on a version alone.
func ConfirmMembership(read AuthorityRead, service bool, confirmed MembershipRows) error {
	if len(read.Targets) == 0 {
		return fault.New(fault.InvalidInput, "a membership authority read carries no observed row targets")
	}
	current, err := evaluateMembership(read, service, confirmed)
	if err != nil {
		return err
	}
	if err := compareMembershipTargets(read.Targets, current.Targets); err != nil {
		return err
	}
	if !current.Allowed {
		return fault.New(fault.Forbidden, current.Detail)
	}
	return nil
}

// evaluateMembership builds the fact without translating a refusal into an
// error, so Confirm can distinguish a pin change (version_conflict) from a
// current union that no longer grants the role (forbidden).
func evaluateMembership(read AuthorityRead, service bool, rows MembershipRows) (MembershipFact, error) {
	if read.Check != CheckMembership {
		return MembershipFact{}, fault.New(fault.InvalidInput, fmt.Sprintf("authority check %q is not a membership check", read.Check))
	}
	if err := read.Scope.Validate(); err != nil {
		return MembershipFact{}, err
	}
	if _, err := id.ParsePrincipalID(string(read.PrincipalID)); err != nil {
		return MembershipFact{}, fault.Wrap(fault.InvalidInput, "membership authority principal id is invalid", err)
	}

	tenantRow, err := observedMembershipRow(read.Scope, read.PrincipalID, rows.TenantWide, true)
	if err != nil {
		return MembershipFact{}, err
	}
	projectRow := MembershipRow{}
	if read.Scope.ProjectScoped() {
		projectRow, err = observedMembershipRow(read.Scope, read.PrincipalID, rows.Project, false)
		if err != nil {
			return MembershipFact{}, err
		}
	}

	var tenantWide, project *tenancy.Membership
	if tenantRow.Membership != nil {
		tenantWide = tenantRow.Membership
	}
	if projectRow.Membership != nil {
		project = projectRow.Membership
	}

	var roles []tenancy.Role
	var ok bool
	if service {
		roles, ok = tenancy.EffectiveServiceRoles(read.Scope, tenantWide, project)
	} else {
		roles, ok = tenancy.EffectiveRoles(read.Scope, tenantWide, project)
	}
	required := read.Required.HumanRoles
	if service {
		required = read.Required.ServiceRoles
	}

	fact := MembershipFact{
		Check:       CheckMembership,
		Scope:       read.Scope,
		PrincipalID: read.PrincipalID,
		TargetID:    read.TargetID,
		Required:    read.Required,
		Service:     service,
		Targets:     MembershipTargets(read.Scope, tenantWide, project, tenantRow.RowID, projectRow.RowID),
		read:        read,
	}
	switch {
	case !ok:
		fact.Detail = "no applicable membership row exists in scope"
	case !anyRequiredRole(roles, required):
		fact.Detail = "the effective membership does not grant the required role"
	default:
		fact.Allowed = true
	}
	return fact, nil
}

// observedMembershipRow applies the union's applicability rule to one observed
// row. A row for another principal, another tenant or another project is not a
// contribution and is not pinned: it is treated as absent. An applicable
// present row without its observed identity is an invalid observation that must
// never be pinned as a wildcard.
func observedMembershipRow(scope tenancy.Scope, principal id.PrincipalID, row MembershipRow, tenantWide bool) (MembershipRow, error) {
	if row.Membership == nil {
		return MembershipRow{}, nil
	}
	if row.Membership.PrincipalID != principal {
		return MembershipRow{}, nil
	}
	applies := false
	if tenantWide {
		applies = row.Membership.Scope.TenantScoped() && row.Membership.Scope.TenantID == scope.TenantID
	} else {
		applies = scope.ProjectScoped() && row.Membership.Scope == scope
	}
	if !applies {
		return MembershipRow{}, nil
	}
	if row.RowID == "" {
		return MembershipRow{}, fault.New(fault.InternalFailure, "an applicable membership row carries no observed identity")
	}
	return row, nil
}

// anyRequiredRole reports whether the effective union holds at least one of the
// operation's required roles.
func anyRequiredRole(current, required []tenancy.Role) bool {
	for _, wanted := range required {
		if slices.Contains(current, wanted) {
			return true
		}
	}
	return false
}

// compareMembershipTargets refuses any difference between the pins a fact
// captured and the pins derived from the rows a commit unit re-read. A present
// pin without a row identity is invalid and is refused rather than matched as a
// wildcard.
func compareMembershipTargets(expected, confirmed []AuthorityTarget) error {
	if len(expected) != len(confirmed) {
		return fault.New(fault.VersionConflict, fmt.Sprintf("membership authority pins changed: %d observed targets, want %d", len(confirmed), len(expected)))
	}
	for index := range expected {
		want, got := expected[index], confirmed[index]
		if !want.Absent && want.RowID == "" {
			return fault.New(fault.VersionConflict, fmt.Sprintf("membership authority target %d is a present pin without a row identity", index))
		}
		if want.Scope != got.Scope || want.Absent != got.Absent || want.RowID != got.RowID || want.ExpectedVersion != got.ExpectedVersion {
			return fault.New(fault.VersionConflict, fmt.Sprintf("membership authority target %d changed: observed scope %+v row %q version %d absent %t; want scope %+v row %q version %d absent %t", index, got.Scope, got.RowID, got.ExpectedVersion, got.Absent, want.Scope, want.RowID, want.ExpectedVersion, want.Absent))
		}
	}
	return nil
}

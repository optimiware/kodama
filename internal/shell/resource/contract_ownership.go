package resource

// Relational resource-row ownership.
//
// The declared registry row requires `membership` and `resource_ownership`, and
// the `resource_ownership` read is the executor read of the `resources` row:
// absent means NotFound. Registration is the operation that creates that row,
// so it follows its own rule:
//
//   - resource.binding.register CREATES the `resources` row when it is absent
//     (identity = ResourceID, owner = the committing scope, version 1) in the
//     same atomic unit, and its `resource_ownership` authority read is
//     satisfied by ABSENCE: the executor read of the `resources` row is
//     replaced for register by the RegisterResourceRow decision rule, so an
//     absent row is not a not_found refusal at the authority stage;
//   - the insert is a CONDITIONAL unique-id insert inside the unit. When two
//     scopes both read the row absent, the first committer wins; the loser's
//     insert hits the primary-key guard and the unit re-reads the row in the
//     same transaction and re-applies the rule with
//     ResolveRegisterConflict. A same-scope loser reuses the winner row
//     (existing, false), a foreign-scope loser gets the uniform not_found
//     class, and the insert conflict is never surfaced as
//     conflict/version_conflict for the resources row;
//   - a row owned by another scope is refused as the uniform not_found class,
//     so registration is no existence oracle;
//   - update, delete, read and report keep requiring an existing row owned by
//     exactly this scope; absent and foreign are both not_found.
//
// These are declarations plus the reference rule; the store adapter applies
// them inside its transaction.

import (
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ResourceRow is the relational ownership row of one registered resource. Its
// identity is the stable ResourceID a binding links to; its owner is the
// committing scope and its version starts at 1.
type ResourceRow struct {
	ID      ResourceID    `json:"id"`
	Scope   tenancy.Scope `json:"scope"`
	Version uint64        `json:"version"`
}

// RegisterResourceRow applies the resource.binding.register ownership rule to
// the currently read row. An absent row is created with the resource identity,
// the committing scope and version 1; an existing row owned by exactly this
// scope is reused unchanged; a row owned by any other scope is the uniform
// not_found class and is not modified. The bool reports whether the caller must
// insert the returned row.
func RegisterResourceRow(scope tenancy.Scope, resourceID ResourceID, existing *ResourceRow) (ResourceRow, bool, error) {
	if err := validateOwnershipInputs(scope, resourceID); err != nil {
		return ResourceRow{}, false, err
	}
	if existing == nil {
		return ResourceRow{ID: resourceID, Scope: scope, Version: 1}, true, nil
	}
	if err := validateResourceRow(resourceID, *existing); err != nil {
		return ResourceRow{}, false, err
	}
	if existing.Scope != scope {
		return ResourceRow{}, false, fault.New(fault.NotFound, "resource is not visible in this scope")
	}
	return *existing, false, nil
}

// ResolveRegisterConflict re-applies the register decision rule to the winner
// row the unit re-read after its conditional unique-id insert lost a race. The
// first committer's row is the winner: a same-scope loser reuses it and reports
// (existing, false), while a foreign-scope loser gets the uniform not_found
// class. The insert conflict is never surfaced as conflict/version_conflict for
// the resources row; an absent re-read (the winner rolled back) is retried as a
// conditional insert instead of calling this helper.
func ResolveRegisterConflict(scope tenancy.Scope, resourceID ResourceID, winner ResourceRow) (ResourceRow, bool, error) {
	return RegisterResourceRow(scope, resourceID, &winner)
}

// RequireOwnedResourceRow applies the existing-row rule for update, delete,
// read and report. Only a row owned by exactly this scope is visible; an absent
// or foreign row is the same not_found class, so the read is not an existence
// oracle.
func RequireOwnedResourceRow(scope tenancy.Scope, resourceID ResourceID, existing *ResourceRow) (ResourceRow, error) {
	if err := validateOwnershipInputs(scope, resourceID); err != nil {
		return ResourceRow{}, err
	}
	if existing == nil {
		return ResourceRow{}, fault.New(fault.NotFound, "resource is not visible in this scope")
	}
	if err := validateResourceRow(resourceID, *existing); err != nil {
		return ResourceRow{}, err
	}
	if existing.Scope != scope {
		return ResourceRow{}, fault.New(fault.NotFound, "resource is not visible in this scope")
	}
	return *existing, nil
}

// validateOwnershipInputs checks the committing scope and resource identity
// before any ownership decision.
func validateOwnershipInputs(scope tenancy.Scope, resourceID ResourceID) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if _, err := ParseResourceID(string(resourceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "resource row id is invalid", err)
	}
	return nil
}

// validateResourceRow refuses a read row that does not describe this resource.
func validateResourceRow(resourceID ResourceID, row ResourceRow) error {
	if row.ID != resourceID {
		return fault.New(fault.InvalidInput, fmt.Sprintf("resource row id %q does not match %q", row.ID, resourceID))
	}
	if row.Version == 0 {
		return fault.New(fault.InvalidInput, "resource row version is zero")
	}
	return row.Scope.Validate()
}

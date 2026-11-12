// Package tenancyadmin declares the consumer-facing tenant, project and
// membership administration commit units and query seam.
//
// The neutral tenancy package owns the shared values but cannot import
// authorize or event: both import tenancy, so doing so would create a package
// cycle. This capability-local subpackage carries the complete commit payloads
// the administration service and the persistence adapters consume. It is not a
// universal contracts or repository package: every value belongs to tenancy
// administration and every mutation is one named atomic commit unit with
// current-authority reads.
//
// A unit always carries the owner scope, the expected version where the target
// is versioned, the next value, the receipt, audit and event publication
// intent, the current-authority reads that must be re-read inside the
// transaction and the retryable post-commit effects. A read query carries only
// the caller scope, filters and stable pagination; it never carries a durable
// permission.
package tenancyadmin

import (
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// MaxMembershipPageSize is the largest accepted membership page size. A page is
// stable and cursor based; the adapter refuses a non-positive or oversized
// limit instead of silently clamping it.
const MaxMembershipPageSize = 200

// TenantCreateUnit is the complete atomic tenant-creation commit unit. The
// created tenant starts at version 1 and the initial encrypted derivation key
// is version 1; there is no prior version to compare. Key is the structured
// tenants.derivation_key object and never carries plaintext key material.
type TenantCreateUnit struct {
	Tenant     tenancy.Tenant            `json:"tenant"`
	Key        token.TenantKey           `json:"key"`
	At         time.Time                 `json:"at"`
	Receipt    receipt.Record            `json:"receipt"`
	Audit      []event.Audit             `json:"audit"`
	Events     []event.Envelope          `json:"events"`
	Authority  []authorize.AuthorityRead `json:"authority"`
	PostCommit []event.PostCommitEffect  `json:"post_commit"`
}

// Validate checks the tenant, the initial version-1 derivation key, the key
// tenant link and the commit time. A tenant creation without a complete
// encrypted key is refused rather than committing a NULL or plaintext key.
func (unit TenantCreateUnit) Validate() error {
	if err := unit.Tenant.Validate(); err != nil {
		return err
	}
	if unit.Tenant.Version != 1 {
		return fault.New(fault.InvalidInput, "tenant creation must start at version 1")
	}
	if unit.Key.TenantID != unit.Tenant.ID {
		return fault.New(fault.InvalidInput, "tenant creation key tenant id does not match the tenant")
	}
	if unit.Key.Version != 1 {
		return fault.New(fault.InvalidInput, "tenant creation key must start at version 1")
	}
	if err := unit.Key.Validate(); err != nil {
		return err
	}
	if unit.At.IsZero() {
		return fault.New(fault.InvalidInput, "tenant creation time is zero")
	}
	return nil
}

// TenantCreateResult is the committed tenant-creation result.
type TenantCreateResult struct {
	TenantID id.TenantID `json:"tenant_id"`
	Version  uint64      `json:"version"`
	Replayed bool        `json:"replayed"`
}

// TenantKeyRotationUnit is the complete atomic tenant derivation-key rotation
// commit unit. It advances the tenant row version and the derivation-key
// version together, re-reads the supplied current authority inside the
// transaction and commits the receipt, audit and event intent with the key
// replacement. PostCommit carries the credential-refresh effects for the
// tenant's mounted credentials.
//
// It deliberately has no principal, execution or service target: rotating the
// tenant derivation key is not a per-principal credential mutation, and the
// per-principal pair remains token.MutationUnit.
type TenantKeyRotationUnit struct {
	Tenant          tenancy.Tenant            `json:"tenant"`
	Key             token.TenantKeyWrite      `json:"key"`
	ExpectedVersion uint64                    `json:"expected_version"`
	At              time.Time                 `json:"at"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// Validate checks the tenant, the expected tenant version, the key write and
// the commit time. The next tenant and key versions must each advance the
// expected version by exactly one, so a stale rotation is refused.
func (unit TenantKeyRotationUnit) Validate() error {
	if err := unit.Tenant.Validate(); err != nil {
		return err
	}
	if unit.ExpectedVersion == 0 {
		return fault.New(fault.InvalidInput, "tenant key rotation expected version is zero")
	}
	if unit.Tenant.Version != unit.ExpectedVersion+1 {
		return fault.New(fault.InvalidInput, "tenant key rotation does not advance the tenant version")
	}
	if unit.Key.TenantID != unit.Tenant.ID {
		return fault.New(fault.InvalidInput, "tenant key rotation key tenant id does not match the tenant")
	}
	if err := unit.Key.Validate(); err != nil {
		return err
	}
	if unit.At.IsZero() {
		return fault.New(fault.InvalidInput, "tenant key rotation time is zero")
	}
	return nil
}

// TenantKeyRotationResult is the committed tenant-key rotation result.
type TenantKeyRotationResult struct {
	TenantVersion uint64 `json:"tenant_version"`
	KeyVersion    uint64 `json:"key_version"`
	Replayed      bool   `json:"replayed"`
}

// ProjectCreateUnit is the complete atomic project-creation commit unit.
type ProjectCreateUnit struct {
	Project    tenancy.Project           `json:"project"`
	At         time.Time                 `json:"at"`
	Receipt    receipt.Record            `json:"receipt"`
	Audit      []event.Audit             `json:"audit"`
	Events     []event.Envelope          `json:"events"`
	Authority  []authorize.AuthorityRead `json:"authority"`
	PostCommit []event.PostCommitEffect  `json:"post_commit"`
}

// ProjectCreateResult is the committed project-creation result.
type ProjectCreateResult struct {
	ProjectID id.ProjectID `json:"project_id"`
	Version   uint64       `json:"version"`
	Replayed  bool         `json:"replayed"`
}

// ProjectUpdateUnit is the complete atomic project-update commit unit. The
// adapter compares ExpectedVersion and re-reads the supplied current authority
// inside the same transaction as the project write, receipt, audit and events.
type ProjectUpdateUnit struct {
	Owner           tenancy.Scope             `json:"owner"`
	ProjectID       id.ProjectID              `json:"project_id"`
	ExpectedVersion uint64                    `json:"expected_version"`
	Project         tenancy.Project           `json:"project"`
	At              time.Time                 `json:"at"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// ProjectUpdateResult is the committed project-update result.
type ProjectUpdateResult struct {
	Version  uint64 `json:"version"`
	Replayed bool   `json:"replayed"`
}

// MembershipGrantUnit is the complete atomic membership-grant commit unit.
//
// A first grant (ExpectedVersion zero) allocates a NEW memberships row identity
// from the store's ID source; it never reuses the identity of a revoked row,
// and the new row's version starts at 1. A grant for an existing row replaces
// the roles and advances the version of that same row, guarded by
// ExpectedVersion. Membership versions are per row lifetime, not a global
// monotonic sequence across a revoke/re-grant: because a revoked identity is
// never reused, the row identity in
// authorize.AuthorityTarget.RowID defeats the ABA case where a recreated row
// happens to carry the same version. Grant and revoke take the same
// per-(scope, principal) serialization/absence guard as a membership authority
// pin.
type MembershipGrantUnit struct {
	Owner           tenancy.Scope             `json:"owner"`
	Membership      tenancy.Membership        `json:"membership"`
	ExpectedVersion uint64                    `json:"expected_version"`
	At              time.Time                 `json:"at"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// MembershipGrantResult is the committed membership-grant result.
type MembershipGrantResult struct {
	Version  uint64 `json:"version"`
	Replayed bool   `json:"replayed"`
}

// MembershipRevokeUnit is the complete atomic membership-revocation commit
// unit. Revocation deletes the current relational grant after guarding its
// expected version; a caller that only narrows roles issues a grant with the
// reduced role set instead. A deleted row identity is never reused, so a later
// grant creates a different row even when its version starts over at 1.
type MembershipRevokeUnit struct {
	Owner           tenancy.Scope             `json:"owner"`
	PrincipalID     id.PrincipalID            `json:"principal_id"`
	ExpectedVersion uint64                    `json:"expected_version"`
	At              time.Time                 `json:"at"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
	PostCommit      []event.PostCommitEffect  `json:"post_commit"`
}

// MembershipRevokeResult is the committed membership-revocation result.
type MembershipRevokeResult struct {
	Version  uint64 `json:"version"`
	Replayed bool   `json:"replayed"`
}

// MembershipQuery is one scoped membership listing. PrincipalID optionally
// narrows the listing to one principal; Cursor is the opaque stable
// continuation returned by the previous page.
type MembershipQuery struct {
	Scope       tenancy.Scope  `json:"scope"`
	PrincipalID id.PrincipalID `json:"principal_id,omitempty"`
	Cursor      string         `json:"cursor,omitempty"`
	Limit       int            `json:"limit"`
}

// Validate checks the scope, optional principal, cursor and bounded page size.
func (query MembershipQuery) Validate() error {
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	if query.PrincipalID != "" {
		if _, err := id.ParsePrincipalID(string(query.PrincipalID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "membership query principal id is invalid", err)
		}
	}
	if query.Limit <= 0 || query.Limit > MaxMembershipPageSize {
		return fault.New(fault.InvalidInput, "membership query limit is outside the allowed page bounds")
	}
	return nil
}

// MembershipPage is one stable membership page.
type MembershipPage struct {
	Memberships []tenancy.Membership `json:"memberships"`
	NextCursor  string               `json:"next_cursor,omitempty"`
}

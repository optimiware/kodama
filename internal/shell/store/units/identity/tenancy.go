package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
)

// Compile-time pins: every tenancy administration port is implemented by one
// adapter.
var (
	_ tenancyadmin.TenantCreatePort      = (*Adapter)(nil)
	_ tenancyadmin.TenantKeyRotationPort = (*Adapter)(nil)
	_ tenancyadmin.ProjectCreatePort     = (*Adapter)(nil)
	_ tenancyadmin.ProjectUpdatePort     = (*Adapter)(nil)
	_ tenancyadmin.MembershipGrantPort   = (*Adapter)(nil)
	_ tenancyadmin.MembershipRevokePort  = (*Adapter)(nil)
	_ tenancyadmin.MembershipQueryPort   = (*Adapter)(nil)
)

// CreateTenant commits one tenant row together with its initial encrypted
// derivation-key object. The tenant starts at version 1; there is no prior
// version to compare.
func (a *Adapter) CreateTenant(ctx context.Context, unit tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
	if err := unit.Validate(); err != nil {
		return tenancyadmin.TenantCreateResult{}, err
	}
	key, err := encodeJSON(unit.Key)
	if err != nil {
		return tenancyadmin.TenantCreateResult{}, err
	}
	scope := tenancy.Scope{TenantID: unit.Tenant.ID}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return tenancyadmin.TenantCreateResult{}, err
	}
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:  "tenants",
			ID:     string(unit.Tenant.ID),
			Insert: true,
			Values: map[string]any{
				"name":           unit.Tenant.Name,
				"version":        int64(unit.Tenant.Version),
				"derivation_key": key,
				"created_at":     unit.At,
			},
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, scope, commit)
	if err != nil {
		return tenancyadmin.TenantCreateResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[tenancyadmin.TenantCreateResult](result); ok {
		if err != nil {
			return tenancyadmin.TenantCreateResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return tenancyadmin.TenantCreateResult{TenantID: unit.Tenant.ID, Version: unit.Tenant.Version}, nil
}

// RotateTenantKey replaces the tenant derivation-key object and advances the
// tenant version in one guarded commit.
func (a *Adapter) RotateTenantKey(ctx context.Context, unit tenancyadmin.TenantKeyRotationUnit) (tenancyadmin.TenantKeyRotationResult, error) {
	if err := unit.Validate(); err != nil {
		return tenancyadmin.TenantKeyRotationResult{}, err
	}
	key, err := encodeJSON(unit.Key.Key)
	if err != nil {
		return tenancyadmin.TenantKeyRotationResult{}, err
	}
	scope := tenancy.Scope{TenantID: unit.Tenant.ID}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return tenancyadmin.TenantKeyRotationResult{}, err
	}
	locks = append(locks, store.Lock{
		Table:         "tenants",
		IDs:           []string{string(unit.Tenant.ID)},
		ExpectVersion: unit.ExpectedVersion,
	})
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:         "tenants",
			ID:            string(unit.Tenant.ID),
			ExpectVersion: unit.ExpectedVersion,
			Values: map[string]any{
				"name":           unit.Tenant.Name,
				"derivation_key": key,
			},
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, scope, commit)
	if err != nil {
		return tenancyadmin.TenantKeyRotationResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[tenancyadmin.TenantKeyRotationResult](result); ok {
		if err != nil {
			return tenancyadmin.TenantKeyRotationResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return tenancyadmin.TenantKeyRotationResult{
		TenantVersion: unit.Tenant.Version,
		KeyVersion:    unit.Key.Key.Version,
	}, nil
}

// CreateProject commits one project row in the owner tenant.
func (a *Adapter) CreateProject(ctx context.Context, unit tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
	if err := unit.Project.Validate(); err != nil {
		return tenancyadmin.ProjectCreateResult{}, err
	}
	if unit.Project.Version != 1 {
		return tenancyadmin.ProjectCreateResult{}, fault.New(fault.InvalidInput, "project creation must start at version 1")
	}
	if err := requireCommitTime(unit.At); err != nil {
		return tenancyadmin.ProjectCreateResult{}, err
	}
	scope := tenancy.Scope{TenantID: unit.Project.TenantID}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return tenancyadmin.ProjectCreateResult{}, err
	}
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:  "projects",
			ID:     string(unit.Project.ID),
			Insert: true,
			Values: map[string]any{
				"name":       unit.Project.Name,
				"version":    int64(unit.Project.Version),
				"created_at": unit.At,
			},
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, scope, commit)
	if err != nil {
		return tenancyadmin.ProjectCreateResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[tenancyadmin.ProjectCreateResult](result); ok {
		if err != nil {
			return tenancyadmin.ProjectCreateResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return tenancyadmin.ProjectCreateResult{ProjectID: unit.Project.ID, Version: unit.Project.Version}, nil
}

// UpdateProject replaces one project value under its expected version.
func (a *Adapter) UpdateProject(ctx context.Context, unit tenancyadmin.ProjectUpdateUnit) (tenancyadmin.ProjectUpdateResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return tenancyadmin.ProjectUpdateResult{}, err
	}
	if err := unit.Project.Validate(); err != nil {
		return tenancyadmin.ProjectUpdateResult{}, err
	}
	if unit.ExpectedVersion == 0 {
		return tenancyadmin.ProjectUpdateResult{}, fault.New(fault.InvalidInput, "project update expected version is zero")
	}
	if unit.Project.TenantID != unit.Owner.TenantID {
		return tenancyadmin.ProjectUpdateResult{}, fault.New(fault.InvalidInput, "project update tenant does not match the owner scope")
	}
	if err := requireUnitVersion(unit.ExpectedVersion, unit.Project.Version); err != nil {
		return tenancyadmin.ProjectUpdateResult{}, err
	}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return tenancyadmin.ProjectUpdateResult{}, err
	}
	locks = append(locks, store.Lock{
		Table:         "projects",
		IDs:           []string{string(unit.ProjectID)},
		ExpectVersion: unit.ExpectedVersion,
	})
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:         "projects",
			ID:            string(unit.ProjectID),
			ExpectVersion: unit.ExpectedVersion,
			Values:        map[string]any{"name": unit.Project.Name},
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return tenancyadmin.ProjectUpdateResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[tenancyadmin.ProjectUpdateResult](result); ok {
		if err != nil {
			return tenancyadmin.ProjectUpdateResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return tenancyadmin.ProjectUpdateResult{Version: unit.Project.Version}, nil
}

// GrantMembership inserts a first grant or replaces the roles of the existing
// scoped grant under its expected version.
func (a *Adapter) GrantMembership(ctx context.Context, unit tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	if err := unit.Membership.Validate(); err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	if unit.Membership.Scope != unit.Owner {
		return tenancyadmin.MembershipGrantResult{}, fault.New(fault.InvalidInput, "membership grant scope does not match the owner scope")
	}
	if err := requireUnitVersion(unit.ExpectedVersion, unit.Membership.Version); err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	roles, err := encodeRoles(unit.Membership.Roles)
	if err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	var write store.Write
	if unit.ExpectedVersion == 0 {
		if err := requireCommitTime(unit.At); err != nil {
			return tenancyadmin.MembershipGrantResult{}, err
		}
		rowID, err := a.newRowID()
		if err != nil {
			return tenancyadmin.MembershipGrantResult{}, err
		}
		// A first grant is create-if-absent: the same commit unit carries the
		// absence guard on the membership unique key and the insert of a NEW
		// row identity. Version alone is not the target identity, so the guard,
		// not the optimistic version, serializes two concurrent first grants.
		locks = append(locks, membershipAbsenceLock(unit.Owner, unit.Membership.PrincipalID))
		write = store.Write{
			Table:  "memberships",
			ID:     rowID,
			Insert: true,
			Values: map[string]any{
				"principal_id": string(unit.Membership.PrincipalID),
				"roles":        roles,
				"version":      int64(unit.Membership.Version),
				"created_at":   unit.At,
			},
		}
	} else {
		existing, found, err := a.findExactMembership(ctx, unit.Owner, unit.Membership.PrincipalID)
		if err != nil {
			return tenancyadmin.MembershipGrantResult{}, err
		}
		if !found {
			return tenancyadmin.MembershipGrantResult{}, fault.New(fault.VersionConflict, "the membership to grant is missing or out of scope")
		}
		write = store.Write{
			Table:         "memberships",
			ID:            mustRowID(existing),
			ExpectVersion: unit.ExpectedVersion,
			Values:        map[string]any{"roles": roles},
		}
		locks = append(locks, store.Lock{
			Table:         "memberships",
			IDs:           []string{write.ID},
			ExpectVersion: unit.ExpectedVersion,
		})
	}
	commit := store.Unit{
		Locks:   locks,
		Writes:  []store.Write{write},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[tenancyadmin.MembershipGrantResult](result); ok {
		if err != nil {
			return tenancyadmin.MembershipGrantResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return tenancyadmin.MembershipGrantResult{Version: unit.Membership.Version}, nil
}

// RevokeMembership removes the scoped grant under its expected version.
func (a *Adapter) RevokeMembership(ctx context.Context, unit tenancyadmin.MembershipRevokeUnit) (tenancyadmin.MembershipRevokeResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return tenancyadmin.MembershipRevokeResult{}, err
	}
	if _, err := id.ParsePrincipalID(string(unit.PrincipalID)); err != nil {
		return tenancyadmin.MembershipRevokeResult{}, err
	}
	if unit.ExpectedVersion == 0 {
		return tenancyadmin.MembershipRevokeResult{}, fault.New(fault.InvalidInput, "membership revocation expected version is zero")
	}
	if err := requireCommitTime(unit.At); err != nil {
		return tenancyadmin.MembershipRevokeResult{}, err
	}
	existing, found, err := a.findExactMembership(ctx, unit.Owner, unit.PrincipalID)
	if err != nil {
		return tenancyadmin.MembershipRevokeResult{}, err
	}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return tenancyadmin.MembershipRevokeResult{}, err
	}
	commit := store.Unit{
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	if found {
		rowID := mustRowID(existing)
		locks = append(locks, store.Lock{
			Table:         "memberships",
			IDs:           []string{rowID},
			ExpectVersion: unit.ExpectedVersion,
		})
		commit.Locks = locks
		commit.Writes = []store.Write{{
			Table:         "memberships",
			ID:            rowID,
			Delete:        true,
			ExpectVersion: unit.ExpectedVersion,
		}}
	} else {
		// The unit itself removes the row, so a retry after a successful
		// commit can no longer find it. Submit the receipt with a guarded lock
		// instead of a pre-read failure: an equal receipt replays the original
		// result without touching the row, while a genuinely missing row fails
		// the guard with version_conflict. The probe identity is derived from
		// the revocation target so it cannot name the row this unit would have
		// deleted, and it additionally asserts the principal it belongs to.
		commit.Locks = append(locks, store.Lock{
			Table:         "memberships",
			IDs:           []string{revokeProbeID(unit.Owner, unit.PrincipalID, unit.ExpectedVersion)},
			ExpectVersion: unit.ExpectedVersion,
			Expect:        map[string]any{"principal_id": string(unit.PrincipalID)},
		})
	}
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return tenancyadmin.MembershipRevokeResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[tenancyadmin.MembershipRevokeResult](result); ok {
		if err != nil {
			return tenancyadmin.MembershipRevokeResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return tenancyadmin.MembershipRevokeResult{Version: unit.ExpectedVersion + 1}, nil
}

// ListMemberships returns one stably paginated membership page inside the
// caller's scope. A tenant scope lists tenant-wide grants; a project scope
// lists the exact project grants.
func (a *Adapter) ListMemberships(ctx context.Context, query tenancyadmin.MembershipQuery) (tenancyadmin.MembershipPage, error) {
	if err := query.Validate(); err != nil {
		return tenancyadmin.MembershipPage{}, err
	}
	equals := map[string]any{}
	if query.PrincipalID != "" {
		equals["principal_id"] = string(query.PrincipalID)
	}
	page, err := a.scanner.Query(ctx, query.Scope, store.ScanQuery{
		Table:   "memberships",
		Equals:  equals,
		OrderBy: []store.Order{{Column: "id"}},
		After:   query.Cursor,
		Limit:   query.Limit,
	})
	if err != nil {
		return tenancyadmin.MembershipPage{}, err
	}
	memberships := make([]tenancy.Membership, 0, len(page.Rows))
	for _, row := range page.Rows {
		membership, err := decodeMembership(row)
		if err != nil {
			return tenancyadmin.MembershipPage{}, err
		}
		memberships = append(memberships, membership)
	}
	return tenancyadmin.MembershipPage{Memberships: memberships, NextCursor: page.Next}, nil
}

// revokeProbeID derives the synthetic membership identity a not-found revoke
// guards. It is a deterministic digest of the revocation target, never an
// allocated row identity, so it names no row this unit could delete.
func revokeProbeID(scope tenancy.Scope, principal id.PrincipalID, expected uint64) string {
	material := string(scope.TenantID) + "\x00" + string(scope.ProjectID) + "\x00" + string(principal) + "\x00" + strconv.FormatUint(expected, 10)
	digest := sha256.Sum256([]byte(material))
	return "membership-revoke-" + hex.EncodeToString(digest[:])
}

// decodeMembership rebuilds one membership value from its committed row.
func decodeMembership(row store.Row) (tenancy.Membership, error) {
	scope, err := decodeScope(row)
	if err != nil {
		return tenancy.Membership{}, err
	}
	principal, err := rowString(row, "principal_id")
	if err != nil {
		return tenancy.Membership{}, err
	}
	roles, err := decodeRolesFromRow(row)
	if err != nil {
		return tenancy.Membership{}, err
	}
	version, err := rowUint(row, "version")
	if err != nil {
		return tenancy.Membership{}, err
	}
	return tenancy.Membership{
		Scope:       scope,
		PrincipalID: id.PrincipalID(principal),
		Roles:       roles,
		Version:     version,
	}, nil
}

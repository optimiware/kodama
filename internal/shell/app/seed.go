package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// seedFirstAdmin performs the explicit first-administrator seed through the
// store seam. It creates the bootstrap tenant, project and memberships
// idempotently and never calls a capability service.
func seedFirstAdmin(
	ctx context.Context,
	executor store.Executor,
	scanner store.Scanner,
	ids *bridges.IDs,
	cipher secretCipher,
	installationID string,
	keyVersion secret.KeyVersion,
	bootstrap *Bootstrap,
	clock Clock,
) error {
	if bootstrap == nil {
		return nil
	}
	if err := bootstrap.validate(); err != nil {
		return err
	}
	now := clock.Now()
	roles, err := bootstrap.roles()
	if err != nil {
		return err
	}
	tenantScope := tenancy.Scope{TenantID: bootstrap.TenantID}

	if _, err := executor.Read(ctx, tenantScope, "tenants", string(bootstrap.TenantID)); err != nil {
		if !isNotFound(err) {
			return err
		}
		key, err := (tenantKeySource{
			cipher:     cipher,
			keyVersion: keyVersion,
			associated: secret.AssociatedData{
				InstallationID: installationID,
				SecretID:       secret.SecretID("tenant-derivation-key"),
				Name:           id.SecretName("TENANT_DERIVATION_KEY"),
				KeyVersion:     keyVersion,
			},
		}).NewTenantKey(ctx, bootstrap.TenantID, 1)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return fault.Wrap(fault.InternalFailure, "bootstrap tenant key could not be encoded", err)
		}
		if _, err := executor.Execute(ctx, tenantScope, store.Unit{Writes: []store.Write{{
			Table:  "tenants",
			ID:     string(bootstrap.TenantID),
			Insert: true,
			Values: map[string]any{
				"name":           bootstrap.TenantName,
				"version":        uint64(1),
				"derivation_key": encoded,
				"created_at":     now,
			},
		}}}); err != nil {
			return err
		}
	}

	if _, err := executor.Read(ctx, tenantScope, "projects", string(bootstrap.ProjectID)); err != nil {
		if !isNotFound(err) {
			return err
		}
		if _, err := executor.Execute(ctx, tenantScope, store.Unit{Writes: []store.Write{{
			Table:  "projects",
			ID:     string(bootstrap.ProjectID),
			Insert: true,
			Values: map[string]any{
				"name":       bootstrap.ProjectName,
				"version":    uint64(1),
				"created_at": now,
			},
		}}}); err != nil {
			return err
		}
	}

	// Only the tenant-wide administration grant is seeded. The project
	// membership is deliberately left to the public membership.grant operation,
	// so the first-admin seed never pre-empts a service-owned mutation.
	return seedMembership(ctx, executor, scanner, ids, tenantScope, bootstrap.AdminPrincipal, roles, now)
}

func seedMembership(
	ctx context.Context,
	executor store.Executor,
	scanner store.Scanner,
	ids *bridges.IDs,
	scope tenancy.Scope,
	principal id.PrincipalID,
	roles []tenancy.Role,
	now time.Time,
) error {
	page, err := scanner.Query(ctx, scope, store.ScanQuery{
		Table:   "memberships",
		Equals:  map[string]any{"principal_id": string(principal)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		return err
	}
	if len(page.Rows) > 0 {
		return nil
	}
	encoded, err := json.Marshal(roles)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "bootstrap membership roles could not be encoded", err)
	}
	rowID, err := ids.NewRowID()
	if err != nil {
		return err
	}
	_, err = executor.Execute(ctx, scope, store.Unit{Writes: []store.Write{{
		Table:  "memberships",
		ID:     rowID,
		Insert: true,
		Values: map[string]any{
			"principal_id": string(principal),
			"roles":        encoded,
			"version":      uint64(1),
			"created_at":   now,
		},
	}}})
	return err
}

func (bootstrap *Bootstrap) validate() error {
	if _, err := id.ParseTenantID(string(bootstrap.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "bootstrap tenant id is invalid", err)
	}
	if bootstrap.TenantName == "" {
		return fault.New(fault.InvalidInput, "bootstrap tenant name is empty")
	}
	if _, err := id.ParseProjectID(string(bootstrap.ProjectID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "bootstrap project id is invalid", err)
	}
	if bootstrap.ProjectName == "" {
		return fault.New(fault.InvalidInput, "bootstrap project name is empty")
	}
	if _, err := id.ParsePrincipalID(string(bootstrap.AdminPrincipal)); err != nil {
		return fault.Wrap(fault.InvalidInput, "bootstrap admin principal id is invalid", err)
	}
	_, err := bootstrap.roles()
	return err
}

func (bootstrap *Bootstrap) roles() ([]tenancy.Role, error) {
	names := bootstrap.AdminRoles
	if len(names) == 0 {
		names = []string{string(tenancy.RoleAdmin)}
	}
	roles := make([]tenancy.Role, len(names))
	for index, name := range names {
		role := tenancy.Role(name)
		if !role.Valid() {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("bootstrap admin role %q is not frozen", name))
		}
		roles[index] = role
	}
	return roles, nil
}

func isNotFound(err error) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == fault.NotFound
}

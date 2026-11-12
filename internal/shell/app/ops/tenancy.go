package opsapp

import (
	"context"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// tenancyHandlers binds the tenant, project and membership administration
// operations.
func tenancyHandlers(deps Dependencies) map[authorize.OperationName]ops.Handler {
	return map[authorize.OperationName]ops.Handler{
		"tenant.create": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TenantCreateRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			tenantID := id.TenantID(request.TenantID)
			key, err := deps.TenantKeys.NewTenantKey(ctx, tenantID, 1)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tenancy.CreateTenant(ctx, tenancyadmin.TenantCreateCommand{
				Actor:     call.Principal,
				TenantID:  tenantID,
				Name:      request.Name,
				Key:       key,
				CallerKey: call.Request.IdempotencyKey,
				Content:   receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.TenantCreateResult{TenantID: string(committed.TenantID), Version: committed.Version})
		}),
		"tenant.rotate_key": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TenantRotateKeyRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			tenantID := id.TenantID(request.TenantID)
			key, err := deps.TenantKeys.NewTenantKey(ctx, tenantID, version+1)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tenancy.RotateTenantKey(ctx, tenancyadmin.TenantKeyRotationCommand{
				Actor:           call.Principal,
				TenantID:        tenantID,
				Name:            request.Name,
				ExpectedVersion: version,
				Key:             key,
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.TenantRotateKeyResult{
				TenantVersion: committed.TenantVersion,
				KeyVersion:    committed.KeyVersion,
			})
		}),
		"project.create": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ProjectCreateRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tenancy.CreateProject(ctx, tenancyadmin.ProjectCreateCommand{
				Actor:     call.Principal,
				ProjectID: id.ProjectID(request.ProjectID),
				Name:      request.Name,
				CallerKey: call.Request.IdempotencyKey,
				Content:   receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ProjectCreateResult{ProjectID: string(committed.ProjectID), Version: committed.Version})
		}),
		"project.update": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ProjectUpdateRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tenancy.UpdateProject(ctx, tenancyadmin.ProjectUpdateCommand{
				Actor:           call.Principal,
				ProjectID:       id.ProjectID(request.ProjectID),
				ExpectedVersion: version,
				Name:            request.Name,
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ProjectUpdateResult{Version: committed.Version})
		}),
		"membership.grant": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.MembershipGrantRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			scope, err := membershipScope(call, request.ProjectID)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tenancy.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
				Actor: call.Principal,
				Membership: tenancy.Membership{
					Scope:       scope,
					PrincipalID: id.PrincipalID(request.PrincipalID),
					Roles:       rolesOf(request.Roles),
				},
				ExpectedVersion: expectedVersion(call),
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.MembershipGrantResult{Version: committed.Version})
		}),
		"membership.revoke": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.MembershipRevokeRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			scope, err := membershipScope(call, request.ProjectID)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tenancy.RevokeMembership(ctx, tenancyadmin.MembershipRevokeCommand{
				Actor:           call.Principal,
				Scope:           scope,
				PrincipalID:     id.PrincipalID(request.PrincipalID),
				ExpectedVersion: version,
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.MembershipRevokeResult{Version: committed.Version})
		}),
		"membership.list": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.MembershipListRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			scope, err := membershipScope(call, request.ProjectID)
			if err != nil {
				return api.Result{}, err
			}
			page := request.Page
			if page == nil {
				page = &api.Page{}
			}
			committed, err := deps.Tenancy.ListMemberships(ctx, tenancyadmin.MembershipListCommand{
				Actor:  call.Principal,
				Scope:  scope,
				Cursor: page.Cursor,
				Limit:  page.EffectiveLimit(),
			})
			if err != nil {
				return api.Result{}, err
			}
			result := api.MembershipListResult{NextCursor: committed.NextCursor}
			for _, membership := range committed.Memberships {
				result.Memberships = append(result.Memberships, api.MembershipView{
					PrincipalID: string(membership.PrincipalID),
					Roles:       roleNames(membership.Roles),
					Version:     membership.Version,
				})
			}
			return respond(call, result)
		}),
	}
}

// membershipScope builds the membership row scope from the tenant the caller is
// authorized in and the project named by the route (or project_id for MCP). The
// registry scope of the membership family is tenant, so the resolver hands the
// handler a tenant-only scope; using it as the row scope would silently create,
// read or revoke tenant-wide memberships instead of project ones.
//
// A project-scoped principal can never address another project: the refusal is
// the same uniform not_found the scope resolver uses, so no lookup reveals that
// the foreign project exists.
func membershipScope(call ops.AuthorizedCall, projectID string) (tenancy.Scope, error) {
	scope := tenancy.Scope{TenantID: call.Scope.TenantID, ProjectID: id.ProjectID(projectID)}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, err
	}
	if call.Principal.Scope.ProjectScoped() && call.Principal.Scope.ProjectID != scope.ProjectID {
		return tenancy.Scope{}, fault.New(fault.NotFound, "project is not visible")
	}
	return scope, nil
}

// rolesOf converts public role names into domain roles.
func rolesOf(names []string) []tenancy.Role {
	if names == nil {
		return nil
	}
	roles := make([]tenancy.Role, len(names))
	for index, name := range names {
		roles[index] = tenancy.Role(name)
	}
	return roles
}

// roleNames converts domain roles into public role names.
func roleNames(roles []tenancy.Role) []string {
	if roles == nil {
		return nil
	}
	names := make([]string, len(roles))
	for index, role := range roles {
		names[index] = string(role)
	}
	return names
}

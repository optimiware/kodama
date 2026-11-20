package opsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/receipt"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodema/internal/shell/tenancy/admin"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
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
			if request.TenantID == "" {
				request.TenantID = string(call.Scope.TenantID)
			}
			var copiedMembers []tenancy.Membership
			if request.CopyMembersFrom != "" {
				if deps.Scanner != nil {
					foundSource, err := projectExistsInTenant(ctx, deps.Scanner, call.Scope, request.CopyMembersFrom)
					if err != nil {
						return api.Result{}, err
					}
					if !foundSource {
						return api.Result{}, fault.New(fault.NotFound, "source project to copy members from was not found")
					}
				}
				sourceScope := tenancy.Scope{
					TenantID:  call.Scope.TenantID,
					ProjectID: id.ProjectID(request.CopyMembersFrom),
				}
				copiedMembers, err = listAllProjectMemberships(ctx, deps.Tenancy, call.Principal, sourceScope)
				if err != nil {
					return api.Result{}, err
				}
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
			if err := grantInitialProjectMemberships(ctx, deps.Tenancy, call, committed.ProjectID, request, copiedMembers); err != nil {
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
		"project.list": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ProjectListRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			scanScope := call.Scope
			if call.Principal.Scope.ProjectScoped() {
				scanScope = call.Principal.Scope
			}
			if err := confirmReadAuthority(ctx, deps, call, scanScope); err != nil {
				return api.Result{}, err
			}
			scanner, err := requireScanner(deps)
			if err != nil {
				return api.Result{}, err
			}
			page := pageOrDefault(request.Page)
			committed, err := scanner.Query(ctx, scanScope, store.ScanQuery{
				Table:   "projects",
				OrderBy: []store.Order{{Column: "id"}},
				After:   page.Cursor,
				Limit:   page.EffectiveLimit(),
			})
			if err != nil {
				return api.Result{}, err
			}
			projects := make([]api.ProjectView, 0, len(committed.Rows))
			for _, row := range committed.Rows {
				projectID, err := scanRowString(row, "id")
				if err != nil {
					return api.Result{}, err
				}
				name, err := scanRowString(row, "name")
				if err != nil {
					return api.Result{}, err
				}
				version, err := scanRowUint(row, "version")
				if err != nil {
					return api.Result{}, err
				}
				projects = append(projects, api.ProjectView{
					ProjectID: projectID,
					Name:      name,
					Version:   version,
				})
			}
			return respond(call, api.ProjectListResult{
				Projects:   projects,
				NextCursor: committed.Next,
			})
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
		"principal.whoami": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			if _, err := decodeRequest[api.PrincipalWhoamiRequest](call); err != nil {
				return api.Result{}, err
			}
			roles, memberships, err := whoamiMembershipsAndRoles(ctx, deps, call)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.PrincipalWhoamiResult{
				PrincipalID: string(call.Principal.ID),
				Kind:        whoamiKind(call.Principal.Kind),
				TenantID:    string(call.Principal.Scope.TenantID),
				ProjectID:   string(call.Principal.Scope.ProjectID),
				Roles:       roles,
				Memberships: memberships,
			})
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

// defaultCreatorProjectRoles is the initial project role set granted to the
// authenticated tenant-admin creator of a new project when neither copied
// memberships nor explicit creator_roles override it.
var defaultCreatorProjectRoles = []tenancy.Role{
	tenancy.RoleAdmin,
	tenancy.RoleEngineer,
	tenancy.RoleReviewer,
	tenancy.RoleOperator,
	tenancy.RoleViewer,
}

func projectExistsInTenant(ctx context.Context, scanner store.Scanner, tenantScope tenancy.Scope, projectID string) (bool, error) {
	page, err := scanner.Query(ctx, tenantScope, store.ScanQuery{
		Table:   "projects",
		Equals:  map[string]any{"id": projectID},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		return false, err
	}
	return len(page.Rows) > 0, nil
}

func listAllProjectMemberships(
	ctx context.Context,
	tenancyService TenancyAdmin,
	actor authorize.Principal,
	scope tenancy.Scope,
) ([]tenancy.Membership, error) {
	var all []tenancy.Membership
	cursor := ""
	for {
		page, err := tenancyService.ListMemberships(ctx, tenancyadmin.MembershipListCommand{
			Actor:  actor,
			Scope:  scope,
			Cursor: cursor,
			Limit:  tenancyadmin.MaxMembershipPageSize,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Memberships...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}

// grantInitialProjectMemberships initializes project-scoped memberships for a
// newly created project: any memberships copied from copy_members_from, plus
// the authenticated creator's membership (defaulting to all human roles unless
// already copied or overridden via creator_roles). Every grant goes through
// TenancyAdmin.GrantMembership with deterministic derived caller keys so
// authority is re-checked in-commit and retries converge cleanly.
func grantInitialProjectMemberships(
	ctx context.Context,
	tenancyService TenancyAdmin,
	call ops.AuthorizedCall,
	projectID id.ProjectID,
	request api.ProjectCreateRequest,
	copied []tenancy.Membership,
) error {
	grants := make(map[id.PrincipalID][]tenancy.Role, len(copied)+1)
	for _, membership := range copied {
		if len(membership.Roles) == 0 {
			continue
		}
		grants[membership.PrincipalID] = append([]tenancy.Role(nil), membership.Roles...)
	}
	if call.Principal.Kind == identity.KindHuman && call.Principal.ID != "" {
		switch {
		case request.CreatorRoles != nil:
			if len(request.CreatorRoles) > 0 {
				grants[call.Principal.ID] = rolesOf(request.CreatorRoles)
			}
		default:
			if _, exists := grants[call.Principal.ID]; !exists {
				grants[call.Principal.ID] = append([]tenancy.Role(nil), defaultCreatorProjectRoles...)
			}
		}
	}
	if len(grants) == 0 {
		return nil
	}

	principals := make([]string, 0, len(grants))
	for principalID := range grants {
		principals = append(principals, string(principalID))
	}
	sort.Strings(principals)

	projectScope := tenancy.Scope{TenantID: call.Scope.TenantID, ProjectID: projectID}
	for _, principal := range principals {
		principalID := id.PrincipalID(principal)
		roles := grants[principalID]
		grantRequest := api.MembershipGrantRequest{
			ProjectID:   string(projectID),
			PrincipalID: principal,
			Roles:       roleNames(roles),
		}
		encoded, err := json.Marshal(struct {
			Request api.MembershipGrantRequest `json:"request"`
		}{Request: grantRequest})
		if err != nil {
			encoded = []byte(`{"request":null}`)
		}
		_, err = tenancyService.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
			Actor: call.Principal,
			Membership: tenancy.Membership{
				Scope:       projectScope,
				PrincipalID: principalID,
				Roles:       roles,
			},
			ExpectedVersion: 0,
			CallerKey:       projectInitMemberKey(call.Request.IdempotencyKey, projectID, principalID),
			Content:         receipt.Content{MediaType: api.MediaType, Bytes: encoded},
		})
		if err != nil {
			if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
				continue
			}
			return err
		}
	}
	return nil
}

func projectInitMemberKey(callerKey string, projectID id.ProjectID, principalID id.PrincipalID) string {
	sum := sha256.Sum256([]byte(callerKey + "\x00" + string(projectID) + "\x00" + string(principalID)))
	return "project-init-member-" + hex.EncodeToString(sum[:16])
}

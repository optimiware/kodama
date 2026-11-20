package opsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/receipt"
	"github.com/optimiware/kodema/internal/shell/resource"
	"github.com/optimiware/kodema/internal/shell/service"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
)

const defaultGitEnforcingService = "git"

var (
	defaultGitBindingOperations = []resource.Operation{
		resource.OperationGitRead,
		resource.OperationGitRefUpdate,
	}
	defaultGitReportableEvents = []string{
		"git.ref.updated",
	}
)

// repositoryHandlers binds the high-level Git repository onboarding and listing
// operations while keeping Git storage ownership and access rules in Userland.
func repositoryHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"repository.create": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.RepositoryCreateRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			if deps.Repositories == nil {
				return api.Result{}, fault.New(fault.DependencyUnavailable,
					"git repository provisioning is not configured on this server")
			}

			defaultBranch := strings.TrimSpace(request.DefaultBranch)
			if defaultBranch == "" {
				defaultBranch = api.DefaultRepositoryBranch
			}
			resourceID := strings.TrimSpace(request.ResourceID)
			if resourceID == "" {
				resourceID = api.DeriveRepositoryResourceID(request.Name)
			}
			bindingID := strings.TrimSpace(request.BindingID)
			if bindingID == "" {
				bindingID = resourceID
			}
			enforcingService := strings.TrimSpace(request.EnforcingService)
			if enforcingService == "" {
				enforcingService = strings.TrimSpace(deps.Repositories.DefaultServiceID())
			}
			if enforcingService == "" {
				enforcingService = defaultGitEnforcingService
			}
			trustedOrigins := request.TrustedOrigins
			if trustedOrigins == nil {
				trustedOrigins = deps.Repositories.DefaultTrustedOrigins()
			}
			if trustedOrigins == nil {
				trustedOrigins = []string{}
			} else {
				trustedOrigins = append([]string(nil), trustedOrigins...)
			}
			metadata := cloneMetadata(request.Metadata)
			if metadata == nil {
				metadata = make(map[string]string, 3)
			}
			metadata["kind"] = "git"
			metadata["repository"] = request.Name
			metadata["default_branch"] = defaultBranch
			if len(metadata) > api.MaxMetadataEntries {
				return api.Result{}, fault.New(fault.InvalidInput,
					"repository metadata exceeds the maximum allowed entries")
			}

			canonical := api.RepositoryCreateRequest{
				ProjectID:        string(call.Scope.ProjectID),
				Name:             request.Name,
				DefaultBranch:    defaultBranch,
				ResourceID:       resourceID,
				BindingID:        bindingID,
				EnforcingService: enforcingService,
				TrustedOrigins:   trustedOrigins,
				Metadata:         metadata,
			}

			committed, err := deps.Bindings.RegisterBinding(ctx, resource.RegisterBindingCommand{
				Actor:            call.Principal,
				Scope:            call.Scope,
				BindingID:        resource.BindingID(bindingID),
				ResourceID:       resource.ResourceID(resourceID),
				EnforcingService: service.ServiceID(enforcingService),
				TrustedOrigins:   trustedOrigins,
				Operations:       append([]resource.Operation(nil), defaultGitBindingOperations...),
				ReportableEvents: append([]string(nil), defaultGitReportableEvents...),
				Metadata:         cloneMetadata(metadata),
				CallerKey:        call.Request.IdempotencyKey,
				Content:          receiptContent(call, canonical),
			})
			if err != nil {
				return api.Result{}, err
			}

			provisioned, err := deps.Repositories.ProvisionRepository(ctx, RepositoryProvisionRequest{
				TenantID:      call.Scope.TenantID,
				ProjectID:     call.Scope.ProjectID,
				Repository:    request.Name,
				ResourceID:    resourceID,
				DefaultBranch: defaultBranch,
			})
			if err != nil {
				if !committed.Replayed && committed.Version == 1 && isPermanentRepositoryProvisionFailure(err) {
					compensateFailedRepositoryBinding(ctx, deps.Bindings, call, resource.BindingID(bindingID), committed.Version)
				}
				return api.Result{}, err
			}
			cloneURL := strings.TrimSpace(provisioned.CloneURL)
			if cloneURL == "" {
				cloneURL = deps.Repositories.CloneURL(request.Name)
			}
			return respond(call, api.RepositoryCreateResult{
				Name:             request.Name,
				ResourceID:       resourceID,
				BindingID:        bindingID,
				EnforcingService: enforcingService,
				DefaultBranch:    defaultBranch,
				CloneURL:         cloneURL,
				TrustedOrigins:   trustedOrigins,
				Version:          committed.Version,
				Created:          provisioned.Created,
				Metadata:         metadata,
			})
		}),
		"repository.list": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.RepositoryListRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			if err := confirmReadAuthority(ctx, deps, call, call.Scope); err != nil {
				return api.Result{}, err
			}
			scanner, err := requireScanner(deps)
			if err != nil {
				return api.Result{}, err
			}
			page := pageOrDefault(request.Page)
			wantLimit := page.EffectiveLimit()

			repositories := make([]api.RepositoryView, 0, wantLimit)
			after := page.Cursor
			var nextCursor string
			batchLimit := store.MaxQueryLimit
			for {
				committed, err := scanner.Query(ctx, call.Scope, store.ScanQuery{
					Table:   "resource_bindings",
					OrderBy: []store.Order{{Column: "id"}},
					After:   after,
					Limit:   batchLimit,
				})
				if err != nil {
					return api.Result{}, err
				}
				kept := len(repositories)
				reread := false
				foundExtra := false
				for index, row := range committed.Rows {
					binding, err := decodeBindingRow(row)
					if err != nil {
						return api.Result{}, err
					}
					if !bindingMatchesKind(binding, "git") {
						continue
					}
					if len(repositories) < wantLimit {
						repositories = append(repositories, repositoryViewFromBinding(binding, deps.Repositories))
						if len(repositories) < wantLimit {
							continue
						}
						if index < len(committed.Rows)-1 {
							repositories = repositories[:kept]
							batchLimit = index + 1
							reread = true
							break
						}
						nextCursor = committed.Next
					} else {
						foundExtra = true
						break
					}
				}
				if foundExtra {
					break
				}
				if reread {
					continue
				}
				batchLimit = store.MaxQueryLimit
				if !committed.HasMore {
					nextCursor = ""
					break
				}
				after = committed.Next
			}
			return respond(call, api.RepositoryListResult{
				Repositories: repositories,
				NextCursor:   nextCursor,
			})
		}),
	}
}

func repositoryViewFromBinding(binding api.ResourceBindingView, provisioner RepositoryProvisioner) api.RepositoryView {
	name := ""
	defaultBranch := ""
	if binding.Metadata != nil {
		name = strings.TrimSpace(binding.Metadata["repository"])
		defaultBranch = strings.TrimSpace(binding.Metadata["default_branch"])
	}
	if name == "" {
		if binding.ResourceID == "git-admin" {
			name = "admin"
		} else {
			name = binding.ResourceID
		}
	}
	cloneURL := ""
	if provisioner != nil {
		cloneURL = provisioner.CloneURL(name)
	}
	return api.RepositoryView{
		Name:             name,
		ResourceID:       binding.ResourceID,
		BindingID:        binding.BindingID,
		EnforcingService: binding.EnforcingService,
		DefaultBranch:    defaultBranch,
		CloneURL:         cloneURL,
		TrustedOrigins:   binding.TrustedOrigins,
		Version:          binding.Version,
		Metadata:         binding.Metadata,
	}
}

func isPermanentRepositoryProvisionFailure(err error) bool {
	class, ok := fault.ClassOf(err)
	if !ok {
		return false
	}
	switch class {
	case fault.InvalidInput, fault.VersionConflict, fault.Forbidden, fault.NotFound:
		return true
	default:
		return false
	}
}

func compensateFailedRepositoryBinding(
	ctx context.Context,
	bindings BindingAdmin,
	call ops.AuthorizedCall,
	bindingID resource.BindingID,
	version uint64,
) {
	deleteRequest := api.ResourceBindingDeleteRequest{BindingID: string(bindingID)}
	encoded, err := json.Marshal(struct {
		Request         api.ResourceBindingDeleteRequest `json:"request"`
		ExpectedVersion uint64                           `json:"expected_version"`
	}{Request: deleteRequest, ExpectedVersion: version})
	if err != nil {
		encoded = []byte(`{"request":null}`)
	}
	_, _ = bindings.DeleteBinding(ctx, resource.DeleteBindingCommand{
		Actor:           call.Principal,
		Scope:           call.Scope,
		BindingID:       bindingID,
		ExpectedVersion: version,
		CallerKey:       repositoryRollbackKey(call.Request.IdempotencyKey, bindingID),
		Content:         receipt.Content{MediaType: api.MediaType, Bytes: encoded},
	})
}

func repositoryRollbackKey(callerKey string, bindingID resource.BindingID) string {
	sum := sha256.Sum256([]byte(callerKey + "\x00" + string(bindingID)))
	return "repo-rollback-binding-" + hex.EncodeToString(sum[:16])
}

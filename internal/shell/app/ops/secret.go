package opsapp

import (
	"context"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// secretHandlers binds secret metadata, write and delete.
func secretHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"secret.metadata.read": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.SecretMetadataReadRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			if request.SecretID == id.SecretTokenName {
				return api.Result{}, fault.New(fault.InvalidInput, "reserved secret name KODAMA_TOKEN cannot be read")
			}
			name, nameErr := id.ParseSecretName(request.SecretID)
			if nameErr != nil {
				if _, idErr := secret.ParseSecretID(request.SecretID); idErr != nil {
					return api.Result{}, idErr
				}
			}
			if deps.Authority != nil {
				reads := make([]authorize.AuthorityRead, 0, len(call.Operation.Authority))
				for _, check := range call.Operation.Authority {
					reads = append(reads, authorize.AuthorityRead{
						Operation:   call.Operation.Name,
						Required:    call.Operation.Requires,
						Check:       check,
						Scope:       call.Scope,
						PrincipalID: call.Principal.ID,
					})
				}
				if len(reads) > 0 {
					facts, err := deps.Authority.Read(ctx, reads)
					if err != nil {
						return api.Result{}, err
					}
					if err := authorize.Confirm(reads, facts); err != nil {
						return api.Result{}, err
					}
				}
			}
			var metadata secret.Metadata
			if nameErr == nil {
				metadata, err = deps.Secrets.GetByName(ctx, call.Scope, name)
			} else {
				metadata, err = deps.Secrets.Get(ctx, call.Scope, secret.SecretID(request.SecretID))
			}
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.SecretMetadataReadResult{
				SecretID:  string(metadata.ID),
				Name:      string(metadata.Name),
				Version:   metadata.Version,
				UpdatedAt: metadata.UpdatedAt,
			})
		}),
		"secret.metadata.list": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.SecretMetadataListRequest](call)
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
			committed, err := scanner.Query(ctx, call.Scope, store.ScanQuery{
				Table:   "secrets",
				OrderBy: []store.Order{{Column: "id"}},
				After:   page.Cursor,
				Limit:   page.EffectiveLimit(),
			})
			if err != nil {
				return api.Result{}, err
			}
			secrets := make([]api.SecretMetadataView, 0, len(committed.Rows))
			for _, row := range committed.Rows {
				secretID, err := scanRowString(row, "id")
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
				updatedAt, err := scanRowTime(row, "updated_at")
				if err != nil {
					return api.Result{}, err
				}
				secrets = append(secrets, api.SecretMetadataView{
					SecretID:  secretID,
					Name:      name,
					Version:   version,
					UpdatedAt: updatedAt,
				})
			}
			return respond(call, api.SecretMetadataListResult{
				Secrets:    secrets,
				NextCursor: committed.Next,
			})
		}),
		"secret.write": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.SecretWriteRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			metadata, err := deps.Secrets.Put(ctx, secret.PutCommand{
				Scope:           call.Scope,
				Name:            id.SecretName(request.SecretID),
				Value:           request.Value,
				ExpectedVersion: expectedVersion(call),
				Actor:           call.Principal.ID,
				ActorKind:       call.Principal.Kind,
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.SecretWriteResult{
				SecretID: string(metadata.ID),
				Name:     string(metadata.Name),
				Version:  metadata.Version,
			})
		}),
		"secret.delete": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.SecretDeleteRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			if err := deps.Secrets.Delete(ctx, secret.DeleteCommand{
				Scope:           call.Scope,
				Name:            id.SecretName(request.SecretID),
				ExpectedVersion: version,
				Actor:           call.Principal.ID,
				ActorKind:       call.Principal.Kind,
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			}); err != nil {
				return api.Result{}, err
			}
			return respond(call, api.SecretDeleteResult{Version: version + 1})
		}),
	}
}

// credentialTTL converts the public TTL seconds into a positive duration.
func credentialTTL(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

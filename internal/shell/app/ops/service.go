package opsapp

import (
	"context"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// serviceHandlers binds service-account administration and credential
// lifecycle.
func serviceHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"service.create": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ServiceCreateRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Services.Create(ctx, service.CreateCommand{
				Actor:     call.Principal,
				ID:        service.ServiceID(request.ServiceID),
				Scope:     call.Scope,
				State:     service.StateEnabled,
				Roles:     rolesOf(request.Roles),
				CallerKey: call.Request.IdempotencyKey,
				Content:   receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ServiceCreateResult{
				ServiceID: string(committed.ServiceID),
				Version:   committed.Version,
			})
		}),
		"service.update": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ServiceUpdateRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Services.Update(ctx, service.UpdateCommand{
				Actor:           call.Principal,
				Scope:           call.Scope,
				ServiceID:       service.ServiceID(request.ServiceID),
				ExpectedVersion: version,
				Roles:           rolesOf(request.Roles),
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ServiceUpdateResult{Version: committed.Version})
		}),
		"service.enable": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ServiceEnableRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := setServiceStatus(ctx, deps, call, request.ServiceID, receiptContent(call, request), true)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ServiceEnableResult{
				State:              string(committed.State),
				Version:            committed.Version,
				RevokedCredentials: uint64(committed.RevokedCredentials),
			})
		}),
		"service.disable": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ServiceDisableRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := setServiceStatus(ctx, deps, call, request.ServiceID, receiptContent(call, request), false)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ServiceDisableResult{
				State:              string(committed.State),
				Version:            committed.Version,
				RevokedCredentials: uint64(committed.RevokedCredentials),
			})
		}),
		"service.credential.issue": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ServiceCredentialIssueRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := issueCredential(ctx, deps, call, request.ServiceID, receiptContent(call, request), request.TTLSeconds, false)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ServiceCredentialIssueResult{
				State:   string(committed.State),
				Version: committed.Version,
				Token:   tokenOf(committed),
			})
		}),
		"service.credential.rotate": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ServiceCredentialRotateRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := issueCredential(ctx, deps, call, request.ServiceID, receiptContent(call, request), request.TTLSeconds, true)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ServiceCredentialRotateResult{
				State:   string(committed.State),
				Version: committed.Version,
				Token:   tokenOf(committed),
			})
		}),
		"service.credential.revoke": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ServiceCredentialRevokeRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Credentials.Revoke(ctx, token.ServiceCredentialRevokeCommand{
				Actor:           call.Principal,
				Scope:           call.Scope,
				ServiceID:       service.ServiceID(request.ServiceID),
				ExpectedVersion: version,
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			invalidated := committed.Invalidated
			return respond(call, api.ServiceCredentialRevokeResult{
				Version:     committed.Mutation.Version,
				Invalidated: &invalidated,
			})
		}),
	}
}

// setServiceStatus applies one enablement or disablement at the envelope's
// expected version.
func setServiceStatus(
	ctx context.Context,
	deps Dependencies,
	call ops.AuthorizedCall,
	serviceID string,
	content receipt.Content,
	enable bool,
) (service.StatusResult, error) {
	version, err := requiredVersion(call)
	if err != nil {
		return service.StatusResult{}, err
	}
	command := service.StatusCommand{
		Actor:           call.Principal,
		Scope:           call.Scope,
		ServiceID:       service.ServiceID(serviceID),
		ExpectedVersion: version,
		CallerKey:       call.Request.IdempotencyKey,
		Content:         content,
	}
	if enable {
		return deps.Services.Enable(ctx, command)
	}
	return deps.Services.Disable(ctx, command)
}

// issueCredential applies one issue or rotation at the envelope's expected
// version.
func issueCredential(
	ctx context.Context,
	deps Dependencies,
	call ops.AuthorizedCall,
	serviceID string,
	content receipt.Content,
	ttlSeconds int,
	rotate bool,
) (token.IssueResult, error) {
	version, err := requiredVersion(call)
	if err != nil {
		return token.IssueResult{}, err
	}
	command := token.ServiceCredentialCommand{
		Actor:           call.Principal,
		Scope:           call.Scope,
		ServiceID:       service.ServiceID(serviceID),
		TTL:             credentialTTL(ttlSeconds),
		ExpectedVersion: version,
		CallerKey:       call.Request.IdempotencyKey,
		Content:         content,
	}
	if rotate {
		return deps.Credentials.Rotate(ctx, command)
	}
	return deps.Credentials.Issue(ctx, command)
}

// tokenOf returns the one-time token of a fresh issue/rotation, or nil when a
// replay produced no plaintext. Only these two operations may expose a token.
func tokenOf(result token.IssueResult) *string {
	if result.Token == "" {
		return nil
	}
	token := result.Token
	return &token
}

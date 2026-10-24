package app

// The native Git push trigger (T-036, decision O-03), joined by T-039.
//
// AssembleTriggers is a pure composition over an injected gitwebhook.Service,
// so route registration, edge throttling and the construction guard are
// unit-tested with the module's in-memory store and port doubles. NewCore
// (T-101) builds that service from T-100's production adapters (the PostgreSQL
// trigger store and admission ledger, the secrets-backed hook secret, the core's
// run planner and the real Git ref reader) through NewGitTriggers when the
// installation selected the trigger (git-triggers.json); otherwise it leaves
// the route unregistered and records `git-webhooks` as disabled, so a hook is
// answered by the deny-by-default router, never by an allow-all handler. Real
// Git and durable admission against the dependency services are T-038's
// `native_hook_roundtrip`.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook"
	gitwebhookhttp "github.com/optimiware/kodama/internal/integrations/gitwebhook/http"
)

// GitHookAction names the edge throttle bucket of unauthenticated hook traffic.
const GitHookAction httpapi.Action = "git_hook.receive"

// TriggerInputs are the collaborators of the trigger slice.
type TriggerInputs struct {
	Env Env
	Log *slog.Logger
	// Service is the trigger use-case layer (Receive; Install and SetEnabled are
	// the Go-level management seam, exposed as TriggerSlice.Service).
	Service *gitwebhook.Service
	// Adapters lists the adapters below the service (trigger store, authorizer,
	// secrets, definitions, repositories, Git reader, admission) for the
	// construction guard: production refuses a development or test one.
	Adapters []any
	// RateLimitPerMinute throttles unauthenticated traffic per remote host. The
	// signature is the only credential, so this is a flood bound and never an
	// authentication step. It must be positive: there is no unlimited default.
	RateLimitPerMinute int
}

// TriggerSlice is the assembled slice.
type TriggerSlice struct {
	// Pattern is the process handler pattern of the hook endpoint
	// (gitwebhookhttp.Pattern: POST /api/v1/git-hooks/{triggerId}).
	Pattern string
	// Handler serves the endpoint behind the edge throttle. It is mounted beside
	// the public router, not inside it: no bearer authentication applies.
	Handler http.Handler
	// Service is the management seam for installing and toggling triggers.
	Service *gitwebhook.Service
	Routes  []string
}

// AssembleTriggers validates the inputs and builds the slice. A missing service
// or limit is a construction error.
func AssembleTriggers(in TriggerInputs) (*TriggerSlice, error) {
	if !in.Env.Valid() {
		return nil, ErrEnvUnknown
	}
	switch {
	case in.Service == nil:
		return nil, fmt.Errorf("%w: the Git trigger service is required", execution.ErrNotConfigured)
	case in.RateLimitPerMinute < 1:
		return nil, errors.New("app: the Git hook rate limit must be positive (there is no unlimited default)")
	}
	for _, a := range append([]any{in.Service}, in.Adapters...) {
		if a == nil {
			continue
		}
		if _, err := Admit(in.Env, PortSource, a); err != nil {
			return nil, err
		}
	}
	mux := http.NewServeMux()
	gitwebhookhttp.New(in.Service).Register(mux)
	limiter := newRateLimiter(in.RateLimitPerMinute, nil)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		decision, err := limiter.Allow(r.Context(), httpapi.RateKey{Principal: httpapi.Principal{ID: "hook-source:" + host}, Action: GitHookAction})
		switch {
		case err != nil:
			httpapi.WriteError(w, r, httpapi.UnavailableError(0, err))
			return
		case !decision.Allowed:
			httpapi.WriteError(w, r, httpapi.RateLimitedError(decision.RetryAfter))
			return
		}
		mux.ServeHTTP(w, r)
	})
	return &TriggerSlice{Pattern: gitwebhookhttp.Pattern, Handler: handler, Service: in.Service, Routes: []string{"git-webhooks"}}, nil
}

// TriggerAuthorizer implements gitwebhook.Authorizer over the selected identity
// authority, with current state on every call: installing or toggling a trigger
// needs project:settings, and a delivery starts a run only while its installing
// principal still holds run:manage. The hook secret is never an identity, and a
// principal that is not a current user of the project holds nothing.
type TriggerAuthorizer struct{ authority identity.Authority }

var _ gitwebhook.Authorizer = (*TriggerAuthorizer)(nil)

// NewTriggerAuthorizer requires the authority; there is no default grant.
func NewTriggerAuthorizer(a identity.Authority) (*TriggerAuthorizer, error) {
	if a == nil {
		return nil, errors.New("app: an identity authority is required for Git triggers")
	}
	return &TriggerAuthorizer{authority: a}, nil
}

// Authorize implements gitwebhook.Authorizer. An unknown action is denied; a
// hidden resource is not found; an outage is returned as such (never access).
func (t *TriggerAuthorizer) Authorize(ctx context.Context, principal string, scope domain.Scope, action gitwebhook.Action) error {
	var perm identity.Permission
	switch action {
	case gitwebhook.ActionInstall:
		perm = identity.PermProjectSettings
	case gitwebhook.ActionRun:
		perm = identity.PermRunManage
	default:
		return gitwebhook.ErrDenied
	}
	err := t.authority.Authorize(ctx, identity.Actor{ID: principal, Type: identity.ActorUser}, identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, perm)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return gitwebhook.ErrNotFound
	case errors.Is(err, identity.ErrDenied):
		return gitwebhook.ErrDenied
	}
	return err
}

package runtimeauth

import (
	"context"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/state"
)

// Resolver adapts the runtime-authority service to the state handler's binding
// resolver. It derives the attempt from the request path and the scope/alias
// only from the verified runtime token and its pinned context.
type Authority interface {
	Authorize(ctx context.Context, req runtimeauth.AuthorizeRequest) (runtimeauth.Authorization, error)
}

type Resolver struct {
	Authority Authority
}

// ResolveBinding implements state.BindingResolver.
func (r Resolver) ResolveBinding(ctx context.Context, req *http.Request, attempt, alias string) (state.Scope, state.Binding, error) {
	return r.resolve(ctx, req, attempt, alias)
}

// ReauthorizeBinding checks the same authority again after the body is read.
func (r Resolver) ReauthorizeBinding(ctx context.Context, req *http.Request, attempt, alias string) error {
	_, _, err := r.resolve(ctx, req, attempt, alias)
	return err
}

func (r Resolver) resolve(ctx context.Context, req *http.Request, attempt, alias string) (state.Scope, state.Binding, error) {
	if r.Authority == nil || attempt == "" || alias == "" {
		return state.Scope{}, state.Binding{}, state.ErrDenied
	}
	action := runtimeauth.ActionStateRead
	if req.Method == http.MethodPut || req.Method == http.MethodDelete {
		action = runtimeauth.ActionStateWrite
	}
	token := bearer(req)
	if token == "" {
		return state.Scope{}, state.Binding{}, state.ErrDenied
	}
	auth, err := r.Authority.Authorize(ctx, runtimeauth.AuthorizeRequest{
		AttemptID: domain.AttemptID(attempt), Bearer: token, Action: action,
		// The state route is versioned independently of the runtime context
		// family. Current v1alpha3 runs use the run_shared v2 context, so the
		// authority decision must be made against that wire.
		ContextVersion: runtimeauth.WireV2,
	})
	if err != nil {
		return state.Scope{}, state.Binding{}, state.ErrDenied
	}
	for _, b := range auth.StateBindings {
		if b.Name != alias {
			continue
		}
		return state.Scope{TenantID: auth.Scope.TenantID, ProjectID: auth.Scope.ProjectID},
			state.Binding{Namespace: b.Namespace, Prefix: b.Prefix, Read: true, Write: b.Write}, nil
	}
	return state.Scope{}, state.Binding{}, state.ErrDenied
}

func bearer(req *http.Request) string {
	h := req.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

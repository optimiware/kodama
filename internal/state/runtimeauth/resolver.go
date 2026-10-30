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
type Resolver struct {
	Authority *runtimeauth.Service
}

// ResolveBinding implements state.BindingResolver.
func (r Resolver) ResolveBinding(ctx context.Context, req *http.Request, attempt, alias string) (state.Scope, state.Binding, error) {
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

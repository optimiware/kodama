package gitauthhttp

import (
	"context"
	"errors"
	"net/http"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
)

// ActionExchange is the authorization action of the credential exchange.
const ActionExchange httpapi.Action = "git_credential.exchange"

// Path is the route of the credential exchange, relative to the API prefix.
const Path = "/tenants/{tenantId}/git-credentials"

// Rule is the coarse route gate the composition registers for ActionExchange:
// source:read at tenant scope or at any project of the tenant, which keeps
// unrelated callers out and hides the tenant from non-members. The use case
// repeats the check at the requested project, and requires the engineer role
// for a seed, through the same identity authority.
func Rule() identityhttp.Rule {
	return identityhttp.Rule{Level: identityhttp.LevelAnyScope, Any: []identity.Permission{gitauth.PermRead}}
}

// Exchanger is the use case the handler needs; *gitauth.Service implements it.
type Exchanger interface {
	Exchange(ctx context.Context, tenant domain.TenantID, user identity.UserID, req gitv1.CredentialRequest) (gitv1.CredentialResponse, error)
}

// Handler serves the credential exchange.
type Handler struct{ svc Exchanger }

// NewHandler requires the use case.
func NewHandler(svc Exchanger) (*Handler, error) {
	if svc == nil {
		return nil, errors.New("gitauthhttp: a credential exchange service is required")
	}
	return &Handler{svc: svc}, nil
}

// RegisterRoutes contributes exactly the exchange route. The router
// authenticates, throttles and authorizes before the handler runs.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	return rt.Handle(httpapi.Route{Method: http.MethodPost, Path: Path, Action: ActionExchange, Handler: h.exchange})
}

func (h *Handler) exchange(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	// The contract decoder is strict: an unknown, misspelled, duplicate or
	// omitted member (seed included) is refused, and the message never echoes the
	// body.
	req, err := gitv1.DecodeRequest[gitv1.CredentialRequest](body)
	if err != nil {
		httpapi.WriteError(w, r, httpapi.BadRequestError("the request body is not valid for this operation"))
		return
	}
	// The principal is the router's verified identity; neither the user nor the
	// tenant is ever read from the body.
	resp, err := h.svc.Exchange(r.Context(), c.Target.TenantID, identity.UserID(c.Principal.ID), req)
	if err != nil {
		httpapi.WriteError(w, r, mapError(err))
		return
	}
	out, err := httpapi.MarshalJSON(r, resp)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// mapError renders a use-case error. Business refusals are the contract's codes
// with their constant, client-safe messages and statuses (gitv1.Status);
// dependency failures are a retryable 503; anything else is the router's fixed
// 500.
func mapError(err error) error {
	var ce *gitv1.ContractError
	if errors.As(err, &ce) {
		if status, ok := gitv1.Status(ce.Code); ok {
			return httpapi.NewError(status, ce.Code, ce.Message)
		}
		return httpapi.InternalServerError(err)
	}
	if errors.Is(err, gitauth.ErrUnavailable) {
		return httpapi.UnavailableError(time.Second, err)
	}
	return err
}

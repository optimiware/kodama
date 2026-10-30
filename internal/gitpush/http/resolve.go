package gitpushhttp

import (
	"context"
	"errors"
	"net/http"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/httpapi"
)

// ActionResolve is the authorization action of the operator resolution of an
// unknown push entry. The composition maps it to the project's administrators
// (the admin role: it gains no push or review authority from it); the use case
// asks its own gitpush.ResolveAuthorizer again with current state.
const ActionResolve httpapi.Action = "git_push.resolve"

// ResolveRequest is the body of POST .../pushes/{pushId}/resolutions: the
// unknown entry, what the operator found after inspecting the repository and
// why. The wire shape is owned here until T-158 lists it in api/git-pushes.yaml.
type ResolveRequest struct {
	Ref string `json:"ref"`
	// Resolution is "applied" or "refused".
	Resolution gitv1.EntryState `json:"resolution"`
	Reason     string           `json:"reason"`
}

// Validate checks the shape; the use case checks the substance.
func (r *ResolveRequest) Validate() error {
	if err := gitv1.ValidateRefName(r.Ref); err != nil {
		return err
	}
	if r.Resolution != gitv1.EntryApplied && r.Resolution != gitv1.EntryRefused {
		return errors.New("resolution: must be applied or refused")
	}
	if r.Reason == "" || len(r.Reason) > gitpush.MaxResolutionReason {
		return errors.New("reason: required, at most 1024 bytes")
	}
	return nil
}

// Resolver is the consumer-owned port of the resolution route;
// *gitpush.Service implements it.
type Resolver interface {
	ResolveUnknown(ctx context.Context, cmd gitpush.ResolveCommand) (gitpush.Resolved, error)
}

// WithResolver mounts the operator resolution route on RegisterRoutes. Without
// it the route does not exist.
func (h *Handler) WithResolver(r Resolver) *Handler {
	h.resolver = r
	return h
}

func (h *Handler) registerResolve(rt *httpapi.Router) error {
	if h.resolver == nil {
		return nil
	}
	return rt.Handle(httpapi.Route{Method: http.MethodPost, Path: projectBase + "/{id}/pushes/{pushId}/resolutions", Action: ActionResolve,
		ResourceKind: resourceKind, ResourceParam: "id", Handler: h.resolve})
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		fail(w, r, err)
		return
	}
	req, err := gitv1.DecodeRequest[ResolveRequest](body)
	if err != nil {
		fail(w, r, err)
		return
	}
	pushID := r.PathValue("pushId")
	if !validID(pushID) {
		fail(w, r, httpapi.NotFoundError())
		return
	}
	out, err := h.resolver.ResolveUnknown(h.ctx(r, c), gitpush.ResolveCommand{
		Principal: c.Principal.ID, Scope: scope, RepositoryID: r.PathValue("id"), PushID: pushID,
		Ref: req.Ref, Resolution: req.Resolution, Reason: req.Reason, Expected: expected,
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = httpapi.SetETag(w, domain.Version(out.Version))
	httpapi.WriteJSON(w, r, http.StatusOK, out.Record)
}

package artifactshttp

import (
	"context"
	"errors"
	"net/http"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

var errConfig = errors.New("artifactshttp: a service and actions are required")

// PublicService is what the public route needs from the module.
type PublicService interface {
	Metadata(ctx context.Context, caller artifacts.Caller, scope domain.Scope, id domain.ArtifactID, download bool) (artifacts.View, error)
}

// Public serves GET /tenants/{tenantId}/projects/{projectId}/artifacts/{id}.
type Public struct {
	svc     PublicService
	actions Actions
}

// NewPublic requires the service.
func NewPublic(svc PublicService, actions Actions) (*Public, error) {
	if svc == nil || actions.Read == "" {
		return nil, errConfig
	}
	return &Public{svc: svc, actions: actions}, nil
}

// RegisterRoutes registers the public metadata route on a router whose prefix
// is "/api/v1". The router authenticates, throttles and authorizes the tenant,
// project and artifact reference before the handler runs.
func (h *Public) RegisterRoutes(rt *httpapi.Router) error {
	return rt.Handle(httpapi.Route{
		Method: http.MethodGet, Path: "/tenants/{tenantId}/projects/{projectId}/artifacts/{id}", Action: h.actions.Read,
		ResourceKind: ResourceArtifact, ResourceParam: "id", Handler: h.get,
	})
}

func (h *Public) get(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		httpapi.WriteError(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	download, err := downloadParam(r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	v, err := h.svc.Metadata(r.Context(), caller(c), scope, domain.ArtifactID(r.PathValue("id")), download)
	if err != nil {
		httpapi.WriteError(w, r, translate(err))
		return
	}
	body := toDTO(v)
	if err := httpapi.SetETag(w, v.Artifact.Version()); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, body)
}

// downloadParam accepts only `download=true|false`, once. Anything else is
// rejected so a typo can never silently skip an authorization-relevant flag.
func downloadParam(r *http.Request) (bool, error) {
	q := r.URL.Query()
	for k := range q {
		if k != "download" {
			return false, httpapi.BadRequestError("unsupported query parameter")
		}
	}
	vals := q["download"]
	switch {
	case len(vals) == 0:
		return false, nil
	case len(vals) > 1:
		return false, httpapi.BadRequestError("download must be given once")
	}
	switch vals[0] {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, httpapi.BadRequestError("download must be true or false")
}

package workhttp

import (
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
)

const projectBase = "/tenants/{tenantId}/projects/{projectId}/work-items"

// RegisterRoutes contributes exactly the work routes: list/create, read/update,
// history and transitions. It registers no DELETE, workspace, dependency,
// finding or generic candidate route. Every route carries an authorization
// action; the router authenticates, throttles and authorizes before a handler
// runs.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: projectBase, Action: ActionList, Handler: h.list},
		{Method: http.MethodPost, Path: projectBase, Action: ActionCreate, Handler: h.create},
		{Method: http.MethodGet, Path: projectBase + "/{id}", Action: ActionRead, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.get},
		{Method: http.MethodPatch, Path: projectBase + "/{id}", Action: ActionUpdate, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.patch},
		{Method: http.MethodGet, Path: projectBase + "/{id}/history", Action: ActionHistory, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.history},
		{Method: http.MethodPost, Path: projectBase + "/{id}/transitions", Action: ActionTransition, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.transition},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func microTime(us int64) time.Time { return time.UnixMicro(us).UTC() }

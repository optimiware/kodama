package dependencieshttp

import (
	"net/http"

	"github.com/optimiware/kodama/internal/httpapi"
)

const workItems = "/tenants/{tenantId}/projects/{projectId}/work-items/{id}/dependencies"

// RegisterRoutes contributes exactly the dependency routes of the contract:
// the GET and POST collection and the DELETE member. It registers no PATCH, no
// separate graph resource and no cross-project route. Every route carries an
// authorization action naming the dependent work item as its resource; the
// router authenticates, throttles and authorizes before a handler runs, and
// the use cases authorize again with the fine-grained permission.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: workItems, Action: ActionList, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.list},
		{Method: http.MethodPost, Path: workItems, Action: ActionAdd, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.add},
		{Method: http.MethodDelete, Path: workItems + "/{dependencyId}", Action: ActionRemove, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.remove},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

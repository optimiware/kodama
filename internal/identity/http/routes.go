package identityhttp

import (
	"net/http"

	"github.com/optimiware/kodama/internal/httpapi"
)

const (
	tenantBase  = "/tenants/{tenantId}"
	projectBase = tenantBase + "/projects"
)

// RegisterRoutes contributes exactly the identity routes and no others: no
// bootstrap, token-issuance, workload-identity or platform route exists here.
// Every route carries an authorization action; the router authenticates,
// throttles and authorizes before a handler runs.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: "/me", Action: ActionMe, Handler: h.me},
		{Method: http.MethodGet, Path: "/tenants", Action: ActionTenantList, Handler: h.tenants},
		{Method: http.MethodGet, Path: tenantBase + "/memberships", Action: ActionMembershipList, Handler: h.listMemberships},
		{Method: http.MethodPost, Path: tenantBase + "/memberships", Action: ActionMembershipCreate, Handler: h.createMembership},
		{Method: http.MethodPatch, Path: tenantBase + "/memberships/{id}", Action: ActionMembershipUpdate, ResourceKind: resourceMembership, ResourceParam: "id", Handler: h.updateMembership},
		{Method: http.MethodDelete, Path: tenantBase + "/memberships/{id}", Action: ActionMembershipDelete, ResourceKind: resourceMembership, ResourceParam: "id", Handler: h.deleteMembership},
		{Method: http.MethodGet, Path: projectBase, Action: ActionProjectList, Handler: h.listProjects},
		{Method: http.MethodPost, Path: projectBase, Action: ActionProjectCreate, Handler: h.createProject},
		{Method: http.MethodGet, Path: projectBase + "/{projectId}", Action: ActionProjectRead, Handler: h.getProject},
		{Method: http.MethodPatch, Path: projectBase + "/{projectId}", Action: ActionProjectUpdate, Handler: h.updateProject},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

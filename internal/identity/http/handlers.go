package identityhttp

import (
	"errors"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
)

const (
	resourceMembership = "membership"
	listSort           = "id"
)

// Handler serves the identity routes.
type Handler struct {
	svc   *identity.Service
	codec *httpapi.CursorCodec
}

// NewHandler requires the use-case service and the cursor codec.
func NewHandler(svc *identity.Service, codec *httpapi.CursorCodec) (*Handler, error) {
	if svc == nil || codec == nil {
		return nil, errors.New("identityhttp: a service and a cursor codec are required")
	}
	return &Handler{svc: svc, codec: codec}, nil
}

// caller is the router's verified principal; neither it nor any scope is ever
// read from the request body or query.
func caller(c *httpapi.Call) identity.Caller {
	return identity.Caller{UserID: identity.UserID(c.Principal.ID), RequestID: c.RequestID}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapError(err))
}

// cursorScope binds every list cursor to the verified caller, in addition to the
// tenant, endpoint, filters and sort: a cursor copied to another principal is
// invalid, and a page is authorized again whoever presents it.
func cursorScope(c *httpapi.Call, filters map[string][]string) httpapi.CursorScope {
	f := map[string][]string{"principal": {c.Principal.ID}}
	for k, v := range filters {
		f[k] = v
	}
	return c.CursorScope(f, listSort)
}

// afterID decodes a cursor position into the last ID of the previous page.
func afterID(page httpapi.PageRequest) (string, error) {
	if page.Position == nil {
		return "", nil
	}
	id := string(page.Position)
	if domain.ProjectID(id).Validate() != nil { // the opaque-ID syntax
		return "", httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request")
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any, version domain.Version, location string) {
	body, err := httpapi.MarshalJSON(r, v)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	if version != 0 {
		if err := httpapi.SetETag(w, version); err != nil {
			httpapi.WriteError(w, r, httpapi.InternalServerError(err))
			return
		}
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	if location != "" {
		hd.Set("Location", location)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func tenantID(c *httpapi.Call) domain.TenantID { return c.Target.TenantID }

// ---- principal level ------------------------------------------------------

func (h *Handler) me(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	if r.URL.Query().Has("limit") || r.URL.Query().Has("cursor") {
		h.fail(w, r, httpapi.BadRequestError("this route takes no query parameters"))
		return
	}
	u, err := h.svc.Me(r.Context(), caller(c))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, meDTO(u), 0, "")
}

func (h *Handler) tenants(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	cs := cursorScope(c, nil)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	after, err := afterID(page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rows, err := h.svc.Tenants(r.Context(), caller(c), after, page.FetchLimit())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]Tenant, 0, len(rows))
	for _, t := range rows {
		dtos = append(dtos, tenantDTO(t))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d Tenant) ([]byte, error) { return []byte(d.Id), nil })
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

// ---- memberships ----------------------------------------------------------

func (h *Handler) listMemberships(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	if err := httpapi.CheckQuery(r, "projectId", "userId"); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd := identity.ListBindingsCommand{Caller: caller(c), Tenant: tenantID(c)}
	filters := map[string][]string{}
	for _, name := range []string{"projectId", "userId"} {
		vs := r.URL.Query()[name]
		switch len(vs) {
		case 0:
			continue
		case 1:
		default:
			h.fail(w, r, httpapi.BadRequestError(name+" must be given once"))
			return
		}
		if domain.ProjectID(vs[0]).Validate() != nil {
			h.fail(w, r, httpapi.BadRequestError(name+" is not a valid identifier"))
			return
		}
		filters[name] = vs
		if name == "projectId" {
			cmd.Project = domain.ProjectID(vs[0])
		} else {
			cmd.User = identity.UserID(vs[0])
		}
	}
	cs := cursorScope(c, filters)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	after, err := afterID(page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cmd.After, cmd.Limit = identity.BindingID(after), page.FetchLimit()
	rows, err := h.svc.ListBindings(r.Context(), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]Membership, 0, len(rows))
	for _, b := range rows {
		dtos = append(dtos, bindingDTO(b))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d Membership) ([]byte, error) { return []byte(d.Id), nil })
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) createMembership(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	var body CreateMembership
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd := identity.GrantCommand{Caller: caller(c), Tenant: tenantID(c), User: identity.UserID(body.UserId), Role: identity.Role(body.Role)}
	if body.ProjectId != nil {
		cmd.Project = domain.ProjectID(*body.ProjectId)
	}
	b, err := h.svc.Grant(r.Context(), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, bindingDTO(b), b.Version, strings.TrimSuffix(r.URL.Path, "/")+"/"+string(b.ID))
}

func (h *Handler) updateMembership(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var body UpdateMembership
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	b, err := h.svc.ChangeRole(r.Context(), identity.ChangeRoleCommand{
		Caller: caller(c), Tenant: tenantID(c), ID: identity.BindingID(r.PathValue("id")), Expected: expected, Role: identity.Role(body.Role),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, bindingDTO(b), b.Version, "")
}

func (h *Handler) deleteMembership(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if _, err := h.svc.Revoke(r.Context(), identity.RevokeCommand{
		Caller: caller(c), Tenant: tenantID(c), ID: identity.BindingID(r.PathValue("id")), Expected: expected,
	}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// ---- projects -------------------------------------------------------------

func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	cs := cursorScope(c, nil)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	after, err := afterID(page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rows, err := h.svc.ListProjects(r.Context(), caller(c), tenantID(c), domain.ProjectID(after), page.FetchLimit())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]Project, 0, len(rows))
	for _, p := range rows {
		dtos = append(dtos, projectDTO(p))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d Project) ([]byte, error) { return []byte(d.Id), nil })
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) getProject(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	v, err := h.svc.GetProject(r.Context(), caller(c), scope)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, projectDTO(v), v.Project.Version, "")
}

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	var body CreateProject
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd := identity.CreateProjectCommand{Caller: caller(c), Tenant: tenantID(c), Key: body.Key, Settings: rawSettings(body.Settings)}
	if body.DisplayName != nil {
		cmd.DisplayName = *body.DisplayName
	}
	if body.ActiveWipLimit != nil {
		cmd.ActiveWIPLimit = *body.ActiveWipLimit
		if cmd.ActiveWIPLimit == 0 {
			h.fail(w, r, httpapi.BadRequestError("activeWipLimit must be between 1 and 100000"))
			return
		}
	}
	v, err := h.svc.CreateProject(r.Context(), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, projectDTO(v), v.Project.Version, strings.TrimSuffix(r.URL.Path, "/")+"/"+string(v.Project.Scope.ProjectID))
}

func (h *Handler) updateProject(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var body UpdateProject
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd := identity.UpdateProjectCommand{
		Caller: caller(c), Scope: scope, Expected: expected,
		Patch: identity.ProjectPatch{DisplayName: body.DisplayName, Settings: rawSettings(body.Settings), ActiveWIPLimit: body.ActiveWipLimit},
	}
	if rb := body.RoleBindings; rb != nil {
		if rb.Grant != nil {
			for _, g := range *rb.Grant {
				cmd.Grant = append(cmd.Grant, identity.BindingGrant{User: identity.UserID(g.UserId), Role: identity.Role(g.Role)})
			}
		}
		if rb.Revoke != nil {
			for _, id := range *rb.Revoke {
				cmd.Revoke = append(cmd.Revoke, identity.BindingID(id))
			}
		}
	}
	v, err := h.svc.UpdateProject(r.Context(), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, projectDTO(v), v.Project.Version, "")
}

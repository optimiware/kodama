// Package dependencieshttp is the HTTP adapter of the business dependency graph. It
// reuses the shared router, strict JSON decoding, ETags, scoped cursors and error
// envelopes of internal/httpapi, and contains no persistence code.
package dependencieshttp

import (
	"errors"
	"net/http"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
)

// Authorization actions registered with the router for each route.
const (
	ActionList   httpapi.Action = "work_item.dependency.list"
	ActionAdd    httpapi.Action = "work_item.dependency.add"
	ActionRemove httpapi.Action = "work_item.dependency.remove"
)

const resourceKind = "work_item"

// listSort names the ordering bound into the cursor.
const listSort = "prerequisite_id"

// Handler serves the dependency routes.
type Handler struct {
	svc   *dependencies.Service
	codec *httpapi.CursorCodec
}

// NewHandler requires the use-case service and the cursor codec.
func NewHandler(svc *dependencies.Service, codec *httpapi.CursorCodec) (*Handler, error) {
	if svc == nil || codec == nil {
		return nil, errors.New("dependencieshttp: a service and a cursor codec are required")
	}
	return &Handler{svc: svc, codec: codec}, nil
}

func (h *Handler) caller(c *httpapi.Call) work.Caller {
	// The principal is the router's verified identity; neither it nor the actor
	// type is ever read from the request body.
	return work.Caller{PrincipalID: c.Principal.ID, RequestID: c.RequestID}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapError(err))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	// The cursor scope binds tenant, project, endpoint and every concrete path
	// value, so a cursor issued for one dependent is refused for another.
	cs := c.CursorScope(map[string][]string{}, listSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cmd := dependencies.ListCommand{Scope: scope, Caller: h.caller(c), Dependent: domain.WorkItemID(r.PathValue("id")), Limit: page.FetchLimit()}
	if page.Position != nil {
		after := domain.WorkItemID(page.Position)
		if after.Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		cmd.After = after
	}
	res, err := h.svc.List(r.Context(), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]Dependency, 0, len(res.Items))
	for _, p := range res.Items {
		dtos = append(dtos, dependencyDTO(p))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d Dependency) ([]byte, error) { return []byte(d.DependencyId), nil })
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, res.OwnerVersion); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	var body AddDependency
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.Add(r.Context(), dependencies.AddCommand{
		Scope: scope, Caller: h.caller(c), Dependent: domain.WorkItemID(r.PathValue("id")),
		Prerequisite: domain.WorkItemID(body.DependencyId), Expected: expected,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, out.Owner.Version); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	status := http.StatusOK
	if out.Changed {
		status = http.StatusCreated
	}
	httpapi.WriteJSON(w, r, status, changeDTO(out))
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	out, err := h.svc.Remove(r.Context(), dependencies.RemoveCommand{
		Scope: scope, Caller: h.caller(c), Dependent: domain.WorkItemID(r.PathValue("id")),
		Prerequisite: domain.WorkItemID(r.PathValue("dependencyId")), Expected: expected,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, out.Owner.Version); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	hd := w.Header()
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNoContent)
}

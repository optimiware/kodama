// Package workhttp is the HTTP adapter of the work-management module. It reuses
// the shared router, strict JSON decoding, ETags, scoped cursors and error
// envelopes of internal/httpapi, and contains no persistence code.
package workhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
)

// Authorization actions registered with the router for each route.
const (
	ActionList       httpapi.Action = "work_item.list"
	ActionCreate     httpapi.Action = "work_item.create"
	ActionRead       httpapi.Action = "work_item.read"
	ActionUpdate     httpapi.Action = "work_item.update"
	ActionHistory    httpapi.Action = "work_item.history"
	ActionTransition httpapi.Action = "work_item.transition"
)

const resourceKind = "work_item"

// Handler serves the work routes.
type Handler struct {
	svc   *work.Service
	codec *httpapi.CursorCodec
}

// NewHandler requires the use-case service and the cursor codec.
func NewHandler(svc *work.Service, codec *httpapi.CursorCodec) (*Handler, error) {
	if svc == nil || codec == nil {
		return nil, errors.New("workhttp: a service and a cursor codec are required")
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

func writeItem(w http.ResponseWriter, r *http.Request, status int, it work.Item) {
	body, err := httpapi.MarshalJSON(r, itemDTO(it))
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, it.Version); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	if status == http.StatusCreated {
		hd.Set("Location", strings.TrimSuffix(r.URL.Path, "/")+"/"+string(it.ID))
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	var body CreateWorkItem
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	it, err := h.svc.Create(r.Context(), createDomain(body, scope, h.caller(c)))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeItem(w, r, http.StatusCreated, it)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	it, err := h.svc.Get(r.Context(), scope, h.caller(c), domain.WorkItemID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeItem(w, r, http.StatusOK, it)
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	var body UpdateWorkItem
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	patch, err := patchDomain(body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	it, err := h.svc.Edit(r.Context(), work.EditCommand{
		Scope: scope, Caller: h.caller(c), ID: domain.WorkItemID(r.PathValue("id")), Expected: expected, Patch: patch,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeItem(w, r, http.StatusOK, it)
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	var body TransitionRequest
	if err := httpapi.DecodeJSON(w, r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	it, err := h.svc.Transition(r.Context(), work.TransitionCommand{
		Scope: scope, Caller: h.caller(c), ID: domain.WorkItemID(r.PathValue("id")), Expected: expected, Request: transitionDomain(body),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeItem(w, r, http.StatusOK, it)
}

// listFilters parses and normalizes the filters. Normalization (sorted, unique)
// makes the cursor scope independent of parameter order.
func listFilters(r *http.Request) (work.ListQuery, map[string][]string, error) {
	var q work.ListQuery
	norm := map[string][]string{}
	vals := r.URL.Query()
	for _, s := range uniqueSorted(vals["state"]) {
		st := work.State(s)
		if !st.Valid() {
			return q, nil, httpapi.BadRequestError("state must be one of backlog, ready, active, review, done, cancelled")
		}
		q.States = append(q.States, st)
		norm["state"] = append(norm["state"], s)
	}
	for _, k := range uniqueSorted(vals["kind"]) {
		kd := work.Kind(k)
		if !kd.Valid() {
			return q, nil, httpapi.BadRequestError("kind must be one of epic, story, task, defect")
		}
		q.Kinds = append(q.Kinds, kd)
		norm["kind"] = append(norm["kind"], k)
	}
	single := func(name string) (string, bool, error) {
		vs := vals[name]
		switch len(vs) {
		case 0:
			return "", false, nil
		case 1:
			return vs[0], true, nil
		}
		return "", false, httpapi.BadRequestError(name + " must be given once")
	}
	if v, ok, err := single("assigneeId"); err != nil {
		return q, nil, err
	} else if ok {
		if domain.WorkItemID(v).Validate() != nil { // the opaque-ID syntax
			return q, nil, httpapi.BadRequestError("assigneeId is not a valid identifier")
		}
		q.AssigneeID = v
		norm["assigneeId"] = []string{v}
	}
	if v, ok, err := single("parentId"); err != nil {
		return q, nil, err
	} else if ok {
		if domain.WorkItemID(v).Validate() != nil {
			return q, nil, httpapi.BadRequestError("parentId is not a valid identifier")
		}
		q.ParentID = domain.WorkItemID(v)
		norm["parentId"] = []string{v}
	}
	if v, ok, err := single("blocked"); err != nil {
		return q, nil, err
	} else if ok {
		b, perr := strconv.ParseBool(v)
		if perr != nil || (v != "true" && v != "false") {
			return q, nil, httpapi.BadRequestError("blocked must be true or false")
		}
		q.Blocked = &b
		norm["blocked"] = []string{v}
	}
	return q, norm, nil
}

func uniqueSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

const listSort = "created_at,id"

type listPosition struct {
	T  int64  `json:"t"` // creation time in microseconds
	ID string `json:"id"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	if err := httpapi.CheckQuery(r, "state", "kind", "assigneeId", "parentId", "blocked"); err != nil {
		h.fail(w, r, err)
		return
	}
	q, norm, err := listFilters(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cs := c.CursorScope(norm, listSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if page.Position != nil {
		var pos listPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || domain.WorkItemID(pos.ID).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		q.After = &work.Position{CreatedAt: microTime(pos.T), ID: domain.WorkItemID(pos.ID)}
	}
	q.Limit = page.FetchLimit()
	items, err := h.svc.List(r.Context(), scope, h.caller(c), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]WorkItem, 0, len(items))
	for _, it := range items {
		dtos = append(dtos, itemDTO(it))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d WorkItem) ([]byte, error) {
		return json.Marshal(listPosition{T: d.CreatedAt.UnixMicro(), ID: d.Id})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	cs := c.CursorScope(map[string][]string{}, "version")
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := work.HistoryQuery{Limit: page.FetchLimit()}
	if page.Position != nil {
		v, perr := strconv.ParseInt(string(page.Position), 10, 64)
		if perr != nil || domain.Version(v).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		q.AfterVersion = domain.Version(v)
	}
	entries, err := h.svc.History(r.Context(), scope, h.caller(c), domain.WorkItemID(r.PathValue("id")), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]HistoryEntry, 0, len(entries))
	for _, e := range entries {
		dtos = append(dtos, historyDTO(e))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d HistoryEntry) ([]byte, error) {
		return []byte(strconv.FormatInt(d.Version, 10)), nil
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

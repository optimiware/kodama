// Package pipelinehttp is the HTTP adapter of the pipeline-definition module.
// It reuses the shared router, bounded body reading, ETags, scoped cursors and
// error envelopes of internal/httpapi and contains no persistence or
// validation logic: every rule comes from the shared internal/pipeline library
// that the local CLI uses as well.
package pipelinehttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Authorization actions registered with the router for each route. They match
// the permissions the use cases ask the pipeline Authority about.
const (
	ActionList     httpapi.Action = httpapi.Action(pipeline.PermList)
	ActionRegister httpapi.Action = httpapi.Action(pipeline.PermRegister)
	ActionRead     httpapi.Action = httpapi.Action(pipeline.PermRead)
)

const (
	resourceKind = "pipeline_definition"
	projectBase  = "/tenants/{tenantId}/projects/{projectId}/pipeline-definitions"
	listSort     = "created_at,id"
)

// Handler serves the pipeline-definition routes.
type Handler struct {
	svc   *pipeline.Service
	codec *httpapi.CursorCodec
}

// NewHandler requires the use-case service and the cursor codec.
func NewHandler(svc *pipeline.Service, codec *httpapi.CursorCodec) (*Handler, error) {
	if svc == nil || codec == nil {
		return nil, errors.New("pipelinehttp: a service and a cursor codec are required")
	}
	return &Handler{svc: svc, codec: codec}, nil
}

// RegisterRoutes contributes exactly the definition routes: list, register and
// read. It registers no PUT, PATCH, DELETE or validation-only route, because a
// definition version is immutable and registration is the only way to validate
// through the API. Every route carries an authorization action; the router
// authenticates, throttles and authorizes before a handler runs.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: projectBase, Action: ActionList, Handler: h.list},
		{Method: http.MethodPost, Path: projectBase, Action: ActionRegister, Handler: h.register},
		{Method: http.MethodGet, Path: projectBase + "/{id}", Action: ActionRead, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.get},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) caller(c *httpapi.Call) pipeline.Caller {
	// The principal is the router's verified identity; neither it nor the actor
	// type is ever read from the request body.
	return pipeline.Caller{PrincipalID: c.Principal.ID, RequestID: c.RequestID}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapError(err))
}

func (h *Handler) scope(w http.ResponseWriter, r *http.Request, c *httpapi.Call) (domain.Scope, bool) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return domain.Scope{}, false
	}
	return scope, true
}

func writeDefinition(w http.ResponseWriter, r *http.Request, status int, d pipeline.Definition) {
	body, err := httpapi.MarshalJSON(r, definitionDTO(d))
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, d.Version); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodPost {
		hd.Set("Location", strings.TrimSuffix(r.URL.Path, "/")+"/"+string(d.ID))
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// register validates and registers the submitted JSON document: 201 for a new
// version, 200 for identical existing content. The body is handed to the shared
// parser unmodified so duplicate keys and number precision are judged by the
// same code as the CLI.
func (h *Handler) register(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.svc.Register(r.Context(), pipeline.RegisterCommand{
		Scope: scope, Caller: h.caller(c), Format: pipeline.FormatJSON, Body: body,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	writeDefinition(w, r, status, res.Definition)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), scope, h.caller(c), domain.DefinitionID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeDefinition(w, r, http.StatusOK, d)
}

type listPosition struct {
	T  int64  `json:"t"` // creation time in microseconds
	ID string `json:"id"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r, "name"); err != nil {
		h.fail(w, r, err)
		return
	}
	var q pipeline.ListQuery
	filters := map[string][]string{}
	if names := r.URL.Query()["name"]; len(names) > 0 {
		if len(names) != 1 {
			h.fail(w, r, httpapi.BadRequestError("name must be given once"))
			return
		}
		q.Name = names[0]
		filters["name"] = []string{q.Name}
	}
	cs := c.CursorScope(filters, listSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if page.Position != nil {
		var pos listPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || domain.DefinitionID(pos.ID).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		q.After = &pipeline.Position{CreatedAt: microTime(pos.T), ID: domain.DefinitionID(pos.ID)}
	}
	q.Limit = page.FetchLimit()
	defs, err := h.svc.List(r.Context(), scope, h.caller(c), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]PipelineDefinition, 0, len(defs))
	for _, d := range defs {
		dtos = append(dtos, definitionDTO(d))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d PipelineDefinition) ([]byte, error) {
		return json.Marshal(listPosition{T: d.CreatedAt.UnixMicro(), ID: d.Id})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

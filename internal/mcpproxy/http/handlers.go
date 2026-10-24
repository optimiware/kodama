// Package mcpproxyhttp is the HTTP adapter of the registration management API of
// the MCP proxy. It reuses the shared router, bounded body reading, ETags,
// scoped cursors and error envelopes of internal/httpapi and holds no
// persistence or validation logic: every rule comes from internal/mcpproxy. The
// workload-facing MCP endpoint is a different adapter (internal/mcpproxy/protocol).
package mcpproxyhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// Authorization actions registered with the router. They match the permissions
// the use cases ask the HumanAuthority about; the identity bridge maps them to
// project:read (list) and project:settings plus credential:manage (mutation).
const (
	ActionList   httpapi.Action = httpapi.Action(mcpproxy.PermList)
	ActionMutate httpapi.Action = httpapi.Action(mcpproxy.PermMutate)
)

const (
	collection = "/tenants/{tenantId}/projects/{projectId}/mcp-servers"
	resource   = collection + "/{id}"
	listSort   = "id"
)

// Manager is the consumer-owned port of the handlers; *mcpproxy.Service
// satisfies it.
type Manager interface {
	Create(ctx context.Context, scope domain.Scope, c mcpproxy.ManageCaller, d mcpproxy.Draft) (mcpproxy.Registration, error)
	Edit(ctx context.Context, scope domain.Scope, c mcpproxy.ManageCaller, id mcpproxy.RegistrationID, expected domain.Version, d mcpproxy.Draft) (mcpproxy.Registration, error)
	Delete(ctx context.Context, scope domain.Scope, c mcpproxy.ManageCaller, id mcpproxy.RegistrationID, expected domain.Version) error
	Get(ctx context.Context, scope domain.Scope, c mcpproxy.ManageCaller, id mcpproxy.RegistrationID) (mcpproxy.Registration, error)
	List(ctx context.Context, scope domain.Scope, c mcpproxy.ManageCaller, after mcpproxy.RegistrationID, limit int) ([]mcpproxy.Registration, error)
}

var _ Manager = (*mcpproxy.Service)(nil)

// Handler serves the management routes.
type Handler struct {
	svc   Manager
	codec *httpapi.CursorCodec
}

// NewHandler requires the use-case service and the cursor codec.
func NewHandler(svc Manager, codec *httpapi.CursorCodec) (*Handler, error) {
	if svc == nil || codec == nil {
		return nil, errors.New("mcpproxyhttp: a service and a cursor codec are required")
	}
	return &Handler{svc: svc, codec: codec}, nil
}

// RegisterRoutes contributes exactly the documented routes: list and create, edit
// and delete. There is no GET-by-id, PUT or runtime route here.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: collection, Action: ActionList, Handler: h.list},
		{Method: http.MethodPost, Path: collection, Action: ActionMutate, Handler: h.create},
		{Method: http.MethodPatch, Path: resource, Action: ActionMutate, ResourceKind: "mcp_server", ResourceParam: "id", Handler: h.update},
		{Method: http.MethodDelete, Path: resource, Action: ActionMutate, ResourceKind: "mcp_server", ResourceParam: "id", Handler: h.remove},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) caller(c *httpapi.Call) mcpproxy.ManageCaller {
	// The principal is the router's verified identity, never a body field.
	return mcpproxy.ManageCaller{PrincipalID: c.Principal.ID, RequestID: c.RequestID}
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

func writeServer(w http.ResponseWriter, r *http.Request, status int, reg mcpproxy.Registration) {
	body, err := httpapi.MarshalJSON(r, serverDTO(reg))
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, reg.Version); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := rejectDuplicateKeys(body); err != nil {
		h.fail(w, r, err)
		return
	}
	var in McpServerDraft
	if err := httpapi.DecodeJSONBytes(body, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	reg, err := h.svc.Create(r.Context(), scope, h.caller(c), draftFrom(in))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeServer(w, r, http.StatusCreated, reg)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := rejectDuplicateKeys(body); err != nil {
		h.fail(w, r, err)
		return
	}
	var in McpServerPatch
	if err := httpapi.DecodeJSONBytes(body, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	if in.Endpoint == nil && in.Credential == nil && in.Tools == nil {
		h.fail(w, r, httpapi.BadRequestError("the request body is not valid for this operation"))
		return
	}
	id := mcpproxy.RegistrationID(r.PathValue("id"))
	cur, err := h.svc.Get(r.Context(), scope, h.caller(c), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The merged draft is validated, and the expected version is compared in the
	// write itself, so a concurrent change cannot be lost.
	d := mcpproxy.Draft{Endpoint: cur.Endpoint, Secret: cur.Secret, Tools: cur.Tools}
	if in.Endpoint != nil {
		d.Endpoint = *in.Endpoint
	}
	if in.Credential != nil {
		d.Secret = mcpproxy.SecretRef{Name: in.Credential.Name, Version: int64(in.Credential.Version)}
	}
	if in.Tools != nil {
		d.Tools = *in.Tools
	}
	reg, err := h.svc.Edit(r.Context(), scope, h.caller(c), id, expected, d)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeServer(w, r, http.StatusOK, reg)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.svc.Delete(r.Context(), scope, h.caller(c), mcpproxy.RegistrationID(r.PathValue("id")), expected); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

type listPosition struct {
	ID string `json:"id"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	cs := c.CursorScope(map[string][]string{}, listSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var after mcpproxy.RegistrationID
	if page.Position != nil {
		var pos listPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || mcpproxy.RegistrationID(pos.ID).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		after = mcpproxy.RegistrationID(pos.ID)
	}
	regs, err := h.svc.List(r.Context(), scope, h.caller(c), after, page.FetchLimit())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]McpServer, 0, len(regs))
	for _, reg := range regs {
		dtos = append(dtos, serverDTO(reg))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d McpServer) ([]byte, error) {
		return json.Marshal(listPosition{ID: string(d.Id)})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

// rejectDuplicateKeys refuses an object that repeats a key at any depth:
// encoding/json keeps the last value silently, so the registration that would be
// stored could differ from what a reviewer read.
func rejectDuplicateKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := k.(string)
				if seen[key] {
					return errDuplicate
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token()
		return err
	}
	if err := walk(); err != nil {
		if errors.Is(err, errDuplicate) {
			return httpapi.BadRequestError("the request body repeats a key")
		}
		return httpapi.BadRequestError("the request body is not valid for this operation")
	}
	return nil
}

var errDuplicate = errors.New("duplicate key")

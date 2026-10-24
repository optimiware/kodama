// Package agentshttp is the HTTP adapter of the agent-profile module. It reuses
// the shared router, bounded body reading, ETags, scoped cursors and error
// envelopes of internal/httpapi and contains no persistence or validation logic:
// every rule comes from internal/agents. There is no update, delete, session,
// continuation or transcript route.
package agentshttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Authorization actions registered with the router for each route. They match
// the permissions the use cases ask the agents Authority about.
const (
	ActionList     httpapi.Action = httpapi.Action(agents.PermList)
	ActionRegister httpapi.Action = httpapi.Action(agents.PermRegister)
)

const (
	projectBase = "/tenants/{tenantId}/projects/{projectId}/agent-profiles"
	listSort    = "created_at,id"
)

// Handler serves the profile routes.
type Handler struct {
	svc   *agents.Service
	codec *httpapi.CursorCodec
}

// NewHandler requires the use-case service and the cursor codec.
func NewHandler(svc *agents.Service, codec *httpapi.CursorCodec) (*Handler, error) {
	if svc == nil || codec == nil {
		return nil, errors.New("agentshttp: a service and a cursor codec are required")
	}
	return &Handler{svc: svc, codec: codec}, nil
}

// RegisterRoutes contributes exactly the documented routes: list and register.
// It registers no GET-by-id, PUT, PATCH or DELETE route, because a profile
// version is immutable and addressed by its name@version reference. Every route
// carries an authorization action; the router authenticates, throttles and
// authorizes before a handler runs.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: projectBase, Action: ActionList, Handler: h.list},
		{Method: http.MethodPost, Path: projectBase, Action: ActionRegister, Handler: h.register},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) caller(c *httpapi.Call) agents.Caller {
	// The principal is the router's verified identity; neither it nor the actor
	// type is ever read from the request body.
	return agents.Caller{PrincipalID: c.Principal.ID, RequestID: c.RequestID}
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

func writeProfile(w http.ResponseWriter, r *http.Request, status int, p agents.ProfileVersion) {
	body, err := httpapi.MarshalJSON(r, profileDTO(p))
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, p.Version); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Location", strings.TrimSuffix(r.URL.Path, "/")+"?name="+p.Name)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// register decodes the submitted JSON profile strictly (an unknown field, a
// repeated key or trailing data is 400 before any state exists) and registers
// it: 201 for a new version, 200 for identical existing content.
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
	if err := rejectDuplicateKeys(body); err != nil {
		h.fail(w, r, err)
		return
	}
	draft, err := agents.DecodeDraft(body)
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the request body is not a valid agent profile document"))
		return
	}
	res, err := h.svc.Register(r.Context(), agents.RegisterCommand{Scope: scope, Caller: h.caller(c), Draft: draft})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	writeProfile(w, r, status, res.Profile)
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
	var q agents.ListQuery
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
		if err := json.Unmarshal(page.Position, &pos); err != nil || agents.ProfileID(pos.ID).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		q.After = &agents.Position{CreatedAt: microTime(pos.T), ID: agents.ProfileID(pos.ID)}
	}
	q.Limit = page.FetchLimit()
	versions, err := h.svc.List(r.Context(), scope, h.caller(c), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]AgentProfile, 0, len(versions))
	for _, p := range versions {
		dtos = append(dtos, profileDTO(p))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(d AgentProfile) ([]byte, error) {
		return json.Marshal(listPosition{T: d.CreatedAt.UnixMicro(), ID: string(d.Id)})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

// rejectDuplicateKeys refuses an object that repeats a key at any depth:
// encoding/json keeps the last value silently, so the profile that would be
// registered could differ from what a reviewer read.
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

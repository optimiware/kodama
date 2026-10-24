// Package gateshttp is the HTTP adapter of the gate module: list, read and
// decide for human gates. It uses net/http through the shared router
// (authentication, rate limiting, authorization, bounded bodies, ETags, scoped
// cursors, error envelopes) and contains no gate logic: every rule comes from
// the gate use cases and the transaction that records the decision. There is no
// route that creates a gate, changes a subject or names an actor.
package gateshttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Authorization actions registered with the router for each route. The use
// cases still check evidence:read and gate:decide through the identity
// authority, so a route authorizer that is more permissive grants nothing.
const (
	ActionList   httpapi.Action = "gate.list"
	ActionRead   httpapi.Action = "gate.read"
	ActionDecide httpapi.Action = "gate.decide"
)

const (
	resourceKind = "gate"
	projectBase  = "/tenants/{tenantId}/projects/{projectId}/gates"
	listSort     = "created_at,id"
)

// Decider joins a decision to the keyed (idempotent) transaction. It is
// implemented by gates/postgres.Keyed.
type Decider interface {
	DecideKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
		plan *gates.DecisionPlan, render func(*gates.Gate) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *gates.Gate, error)
}

// Config wires a Handler. Every member but Clock is required.
type Config struct {
	Service *gates.Service
	Decider Decider
	Cursors *httpapi.CursorCodec
	Clock   func() time.Time
}

// Handler serves the gate routes.
type Handler struct {
	svc     *gates.Service
	decider Decider
	codec   *httpapi.CursorCodec
	now     func() time.Time
}

// NewHandler requires every collaborator.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Service == nil || cfg.Decider == nil || cfg.Cursors == nil {
		return nil, errors.New("gateshttp: a service, a keyed decider and a cursor codec are required")
	}
	h := &Handler{svc: cfg.Service, decider: cfg.Decider, codec: cfg.Cursors, now: cfg.Clock}
	if h.now == nil {
		h.now = func() time.Time { return time.Now().UTC() }
	}
	return h, nil
}

// RegisterRoutes contributes exactly list, read and decide.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: projectBase, Action: ActionList, Handler: h.list},
		{Method: http.MethodGet, Path: projectBase + "/{id}", Action: ActionRead, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.get},
		{Method: http.MethodPost, Path: projectBase + "/{id}/decisions", Action: ActionDecide, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.decide},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) caller(c *httpapi.Call) gates.Caller {
	// The principal is the router's verified identity; neither it nor the actor
	// type is ever read from the request body.
	return gates.Caller{PrincipalID: c.Principal.ID, RequestID: c.RequestID}
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

func (h *Handler) get(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	g, err := h.svc.Get(r.Context(), h.caller(c), scope, gates.ID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, g.Version); err != nil {
		h.fail(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, gateDTO(g, h.now()))
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
	if err := httpapi.CheckQuery(r, "status", "runId"); err != nil {
		h.fail(w, r, err)
		return
	}
	var q gates.ListQuery
	filters := map[string][]string{}
	if sts := r.URL.Query()["status"]; len(sts) > 0 {
		seen := map[string]bool{}
		for _, s := range sts {
			switch gates.Status(s) {
			case gates.StatusPending, gates.StatusApproved, gates.StatusRejected:
			default:
				h.fail(w, r, httpapi.BadRequestError("status is not a gate status"))
				return
			}
			if !seen[s] {
				seen[s] = true
				q.Statuses = append(q.Statuses, gates.Status(s))
			}
		}
		for i := 1; i < len(q.Statuses); i++ {
			for j := i; j > 0 && q.Statuses[j] < q.Statuses[j-1]; j-- {
				q.Statuses[j], q.Statuses[j-1] = q.Statuses[j-1], q.Statuses[j]
			}
		}
		for _, s := range q.Statuses {
			filters["status"] = append(filters["status"], string(s))
		}
	}
	if runs := r.URL.Query()["runId"]; len(runs) > 0 {
		if len(runs) != 1 || domain.RunID(runs[0]).Validate() != nil {
			h.fail(w, r, httpapi.BadRequestError("runId must be one valid identifier"))
			return
		}
		q.RunID = domain.RunID(runs[0])
		filters["runId"] = []string{runs[0]}
	}
	cs := c.CursorScope(filters, listSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if page.Position != nil {
		var pos listPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || gates.ID(pos.ID).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		q.After = &gates.Position{CreatedAt: time.UnixMicro(pos.T).UTC(), ID: gates.ID(pos.ID)}
	}
	q.Limit = page.FetchLimit()
	rows, err := h.svc.List(r.Context(), h.caller(c), scope, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.now()
	type row struct {
		dto     Gate
		created time.Time
	}
	dtos := make([]row, 0, len(rows))
	for _, g := range rows {
		dtos = append(dtos, row{dto: gateDTO(g, now), created: g.CreatedAt})
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(x row) ([]byte, error) {
		return json.Marshal(listPosition{T: x.created.UnixMicro(), ID: x.dto.Id})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]Gate, 0, len(out.Items))
	for _, x := range out.Items {
		items = append(items, x.dto)
	}
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.ListPage[Gate]{Items: items, NextCursor: out.NextCursor})
}

// decide records the one decision. The body is read unmodified so the
// idempotency request hash covers its exact bytes, then decoded strictly. The
// version, the digest and the reason come from the request; the actor and the
// subject never do.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id, err := c.Identity(r, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	version, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := rejectDuplicateKeys(body); err != nil {
		h.fail(w, r, err)
		return
	}
	var req GateDecisionRequest
	if err := httpapi.DecodeJSONBytes(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	caller := h.caller(c)
	ctx := r.Context()
	gateID := gates.ID(r.PathValue("id"))
	plan, err := h.svc.PrepareDecision(ctx, caller, scope, gateID, gates.DecideCommand{
		Decision: gates.Decision(req.Decision), Reason: req.Reason, ExpectedVersion: version, SubjectDigest: req.SubjectDigest,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// A replay is authorized again, with the current permission, before the
	// stored response is released: a revoked reviewer gets nothing back.
	authorizeReplay := func(ctx context.Context, ref httpapi.Reference) error {
		if err := h.svc.Authorize(ctx, caller, scope, gates.PermDecide); err != nil {
			return mapError(err)
		}
		return c.AuthorizeReference(ctx, ref)
	}
	render := func(g *gates.Gate) (httpapi.Outcome, error) {
		hd, err := etagHeader(g)
		if err != nil {
			return httpapi.Outcome{}, httpapi.InternalServerError(err)
		}
		resp, err := httpapi.NewStoredResponse(r, http.StatusOK, gateDTO(g, h.now()), hd)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		return httpapi.Outcome{Response: resp, Target: httpapi.Reference{Kind: resourceKind, ID: string(g.ID)}}, nil
	}
	resp, replayed, _, err := h.decider.DecideKeyed(ctx, id, authorizeReplay, plan, render)
	if err != nil {
		h.fail(w, r, h.svc.AfterRefusal(ctx, plan, err))
		return
	}
	httpapi.WriteStored(w, r, resp, replayed)
}

// rejectDuplicateKeys refuses an object that repeats a key at any depth:
// encoding/json keeps the last value silently, which would make the request the
// idempotency hash covers differ from the request that is executed.
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

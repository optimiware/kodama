// Package executionhttp is the HTTP adapter of the execution module: create,
// list, get, steps and cancel for pipeline runs. It uses net/http through the
// shared router (authentication, rate limiting, authorization, bounded bodies,
// ETags, scoped cursors, error envelopes) and contains no orchestration logic:
// every rule comes from the execution use cases. Events, logs and runtime
// routes belong to their own modules.
package executionhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Authorization actions registered with the router for each route. The use cases
// still check run:read / run:manage through the identity authority, so a route
// authorizer that is more permissive grants nothing.
const (
	ActionList   httpapi.Action = "run.list"
	ActionCreate httpapi.Action = "run.create"
	ActionRead   httpapi.Action = "run.read"
	ActionSteps  httpapi.Action = "run.steps"
	ActionCancel httpapi.Action = "run.cancel"
)

const (
	resourceKind = "run"
	projectBase  = "/tenants/{tenantId}/projects/{projectId}/runs"
	listSort     = "created_at,id"
	stepSort     = "ordinal"
)

// Creator joins run creation to the keyed (idempotent) create transaction. It is
// implemented by execution/postgres.Keyed.
type Creator interface {
	CreateKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
		plan *execution.AdmissionPlan, render func(*execution.Run) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *execution.Run, error)
}

// Config wires a Handler. Every member is required.
type Config struct {
	Service *execution.Service
	Creator Creator
	Cursors *httpapi.CursorCodec
}

// Handler serves the run routes.
type Handler struct {
	svc     *execution.Service
	creator Creator
	codec   *httpapi.CursorCodec
}

// NewHandler requires every collaborator.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Service == nil || cfg.Creator == nil || cfg.Cursors == nil {
		return nil, errors.New("executionhttp: a service, a keyed creator and a cursor codec are required")
	}
	return &Handler{svc: cfg.Service, creator: cfg.Creator, codec: cfg.Cursors}, nil
}

// RegisterRoutes contributes exactly create, list, get, steps and cancel. A rerun
// is another POST /runs, not a route; there is no PUT, PATCH or DELETE because a
// run's pins are immutable.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: projectBase, Action: ActionList, Handler: h.list},
		{Method: http.MethodPost, Path: projectBase, Action: ActionCreate, Handler: h.create},
		{Method: http.MethodPost, Path: projectBase + "/project", Action: ActionCreate, Handler: h.createProject},
		{Method: http.MethodGet, Path: projectBase + "/{id}", Action: ActionRead, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.get},
		{Method: http.MethodGet, Path: projectBase + "/{id}/steps", Action: ActionSteps, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.steps},
		{Method: http.MethodPost, Path: projectBase + "/{id}/cancel", Action: ActionCancel, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.cancel},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) caller(c *httpapi.Call) execution.Caller {
	// The principal is the router's verified identity; neither it nor the actor
	// type is ever read from the request body.
	return execution.Caller{PrincipalID: c.Principal.ID, RequestID: c.RequestID}
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

func (h *Handler) ctx(r *http.Request, c *httpapi.Call) context.Context {
	return execution.WithRequestID(r.Context(), c.RequestID)
}

func runHeaders(r *http.Request, run *execution.Run) (map[string]string, error) {
	tag, err := httpapi.FormatETag(run.Version)
	if err != nil {
		return nil, err
	}
	hd := map[string]string{"ETag": tag}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs") {
		hd["Location"] = strings.TrimSuffix(r.URL.Path, "/") + "/" + string(run.ID)
	}
	return hd, nil
}

func writeRun(w http.ResponseWriter, r *http.Request, status int, run *execution.Run) {
	body, err := httpapi.MarshalJSON(r, runDTO(run))
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	hd, err := runHeaders(r, run)
	if err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	for k, v := range hd {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// create admits a run. The body is read unmodified so the idempotency request
// hash covers its exact bytes, then decoded strictly: an unknown field, a
// duplicate-key surprise or a non-scalar input is 400 before any state exists.
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
	id, err := c.Identity(r, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := rejectDuplicateKeys(body); err != nil {
		h.fail(w, r, err)
		return
	}
	var req CreateRunRequest
	if err := httpapi.DecodeJSONBytes(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd := execution.AdmitCommand{
		Scope: scope, WorkItemID: domain.WorkItemID(req.WorkItemId), DefinitionID: domain.DefinitionID(req.DefinitionId),
		ContentRevision: domain.ContentRevision(req.ContentRevision), SourceCommit: req.SourceCommit,
	}
	if req.Inputs != nil {
		cmd.Inputs = *req.Inputs
	}
	ctx := h.ctx(r, c)
	plan, err := h.svc.Prepare(ctx, h.caller(c), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	render := func(run *execution.Run) (httpapi.Outcome, error) {
		hd, err := runHeaders(r, run)
		if err != nil {
			return httpapi.Outcome{}, httpapi.InternalServerError(err)
		}
		resp, err := httpapi.NewStoredResponse(r, http.StatusCreated, runDTO(run), hd)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		return httpapi.Outcome{Response: resp, Target: httpapi.Reference{Kind: resourceKind, ID: string(run.ID)}}, nil
	}
	resp, replayed, run, err := h.creator.CreateKeyed(ctx, id, c.AuthorizeReference, plan, render)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, resp, replayed)
	if replayed || run == nil {
		return
	}
	// The run is committed with its retained-source intent. Confirm the remote
	// reference now; a failure leaves the run pending and reconciliation repeats
	// it, so the response above stays truthful.
	if _, err := h.svc.ConfirmSource(context.WithoutCancel(ctx), run.Scope, run.ID); err != nil {
		_ = err // logged by the service's reconciler; never surfaced to the creator
	}
}

// createProject admits a source-free project run. It is a separate route because
// it has no work item, content revision or source commit to hash into the run
// schema; the definition and inputs are still pinned through ProjectAdmitCommand.
func (h *Handler) createProject(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	if err := rejectDuplicateKeys(body); err != nil {
		h.fail(w, r, err)
		return
	}
	var req struct {
		DefinitionID domain.DefinitionID        `json:"definitionId"`
		Inputs       map[string]runtimev1.Value `json:"inputs,omitempty"`
	}
	if err := httpapi.DecodeJSONBytes(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd := execution.ProjectAdmitCommand{Scope: scope, DefinitionID: req.DefinitionID, Inputs: req.Inputs}
	ctx := h.ctx(r, c)
	plan, err := h.svc.PrepareProject(ctx, h.caller(c), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	render := func(run *execution.Run) (httpapi.Outcome, error) {
		hd, err := runHeaders(r, run)
		if err != nil {
			return httpapi.Outcome{}, httpapi.InternalServerError(err)
		}
		resp, err := httpapi.NewStoredResponse(r, http.StatusCreated, runDTO(run), hd)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		return httpapi.Outcome{Response: resp, Target: httpapi.Reference{Kind: resourceKind, ID: string(run.ID)}}, nil
	}
	resp, replayed, _, err := h.creator.CreateKeyed(ctx, id, c.AuthorizeReference, plan, render)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, resp, replayed)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	agg, err := h.svc.Get(h.ctx(r, c), h.caller(c), scope, domain.RunID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeRun(w, r, http.StatusOK, agg.Run)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	run, err := h.svc.Cancel(h.ctx(r, c), h.caller(c), scope, domain.RunID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeRun(w, r, http.StatusAccepted, run)
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
	if err := httpapi.CheckQuery(r, "status", "workItemId"); err != nil {
		h.fail(w, r, err)
		return
	}
	var q execution.ListQuery
	filters := map[string][]string{}
	if sts := r.URL.Query()["status"]; len(sts) > 0 {
		seen := map[string]bool{}
		for _, s := range sts {
			switch execution.RunStatus(s) {
			case execution.RunQueued, execution.RunRunning, execution.RunSucceeded, execution.RunFailed, execution.RunCancelled:
			default:
				h.fail(w, r, httpapi.BadRequestError("status is not a run status"))
				return
			}
			if !seen[s] {
				seen[s] = true
				q.Statuses = append(q.Statuses, execution.RunStatus(s))
			}
		}
		sortStrings(q.Statuses)
		for _, s := range q.Statuses {
			filters["status"] = append(filters["status"], string(s))
		}
	}
	if items := r.URL.Query()["workItemId"]; len(items) > 0 {
		if len(items) != 1 || domain.WorkItemID(items[0]).Validate() != nil {
			h.fail(w, r, httpapi.BadRequestError("workItemId must be one valid identifier"))
			return
		}
		q.WorkItemID = domain.WorkItemID(items[0])
		filters["workItemId"] = []string{items[0]}
	}
	cs := c.CursorScope(filters, listSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if page.Position != nil {
		var pos listPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || domain.RunID(pos.ID).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		q.After = &execution.Position{CreatedAt: time.UnixMicro(pos.T).UTC(), ID: domain.RunID(pos.ID)}
	}
	q.Limit = page.FetchLimit()
	runs, err := h.svc.List(h.ctx(r, c), h.caller(c), scope, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dtos := make([]runPosition, 0, len(runs))
	for _, run := range runs {
		dtos = append(dtos, runPosition{dto: runDTO(run), created: run.CreatedAt})
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, dtos, func(p runPosition) ([]byte, error) {
		return json.Marshal(listPosition{T: p.created.UnixMicro(), ID: p.dto.Id})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]Run, 0, len(out.Items))
	for _, p := range out.Items {
		items = append(items, p.dto)
	}
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.ListPage[Run]{Items: items, NextCursor: out.NextCursor})
}

type runPosition struct {
	dto     Run
	created time.Time
}

type stepPosition struct {
	O int `json:"o"`
}

func (h *Handler) steps(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	cs := c.CursorScope(nil, stepSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	after := -1
	if page.Position != nil {
		var pos stepPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || pos.O < 0 {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		after = pos.O
	}
	agg, err := h.svc.Get(h.ctx(r, c), h.caller(c), scope, domain.RunID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type row struct {
		ordinal int
		dto     Step
	}
	var rows []row
	for _, s := range agg.Steps {
		if s.Ordinal > after && len(rows) < page.FetchLimit() {
			rows = append(rows, row{ordinal: s.Ordinal, dto: stepDTO(agg, s)})
		}
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, rows, func(x row) ([]byte, error) { return json.Marshal(stepPosition{O: x.ordinal}) })
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]Step, 0, len(out.Items))
	for _, x := range out.Items {
		items = append(items, x.dto)
	}
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.ListPage[Step]{Items: items, NextCursor: out.NextCursor})
}

func sortStrings(s []execution.RunStatus) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
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

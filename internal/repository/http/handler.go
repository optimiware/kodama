// Package repositoryhttp is the HTTP adapter of the repository module: the
// repository resource of the Git service contract (docs/contracts/git-service.md)
// registered through httpapi.Router: list, create (idempotent, keyed), read,
// patch, archive, restore and the refs read. It reuses the shared router,
// strict JSON decoding, ETags, scoped cursors and error envelopes of
// internal/httpapi, and the wire shapes of contracts/git/v1 (decoded and
// validated by that contract, never redefined here). It contains no
// persistence code and registers no route for push records, credentials or the
// Git endpoint.
package repositoryhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/repository"
)

// Authorization actions registered with each route. The use cases authorize
// again with the finer repository.Action of what they change; ActionPatch is
// the coarse route gate of PATCH, which an engineer (branch and policy) or an
// admin (policy) may pass.
const (
	ActionList    httpapi.Action = httpapi.Action(repository.ActionList)
	ActionCreate  httpapi.Action = httpapi.Action(repository.ActionCreate)
	ActionRead    httpapi.Action = httpapi.Action(repository.ActionRead)
	ActionPatch   httpapi.Action = "repository.patch"
	ActionArchive httpapi.Action = httpapi.Action(repository.ActionArchive)
	ActionRestore httpapi.Action = httpapi.Action(repository.ActionRestore)
	ActionRefs    httpapi.Action = "repository.read_refs"
)

const (
	resourceKind = "repository"
	projectBase  = "/tenants/{tenantId}/projects/{projectId}/repositories"
	listSort     = "created_at,id"
	refsSort     = "ref"
)

// Service is the consumer-owned port the handlers use; *repository.Service
// implements it.
type Service interface {
	Prepare(ctx context.Context, actor repository.Actor, scope domain.Scope, req gitv1.CreateRepositoryRequest) (*repository.CreatePlan, error)
	Provision(ctx context.Context, scope domain.Scope, id repository.ID) (*repository.Repository, error)
	Get(ctx context.Context, actor repository.Actor, scope domain.Scope, id repository.ID) (*repository.Repository, error)
	List(ctx context.Context, actor repository.Actor, scope domain.Scope, q repository.ListQuery) ([]*repository.Repository, error)
	Update(ctx context.Context, c repository.UpdateCommand) (*repository.Repository, error)
	Archive(ctx context.Context, actor repository.Actor, scope domain.Scope, id repository.ID, expected domain.Version) (*repository.Repository, error)
	Restore(ctx context.Context, actor repository.Actor, scope domain.Scope, id repository.ID, expected domain.Version) (*repository.Repository, error)
	ReadRefs(ctx context.Context, actor repository.Actor, scope domain.Scope, id repository.ID, q repository.RefsQuery) (gitv1.RefsResponse, error)
	CloneURL(ctx context.Context, r *repository.Repository) (string, error)
}

// Creator joins repository creation to the keyed (idempotent) create
// transaction. It is implemented by repository/postgres.Keyed.
type Creator interface {
	CreateKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
		plan *repository.CreatePlan, render func(r *repository.Repository, created bool) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *repository.Repository, error)
}

// Config wires a Handler. Every member is required.
type Config struct {
	Service Service
	Creator Creator
	Cursors *httpapi.CursorCodec
}

// Handler serves the repository routes.
type Handler struct {
	svc     Service
	creator Creator
	codec   *httpapi.CursorCodec
}

// NewHandler requires every collaborator.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Service == nil || cfg.Creator == nil || cfg.Cursors == nil {
		return nil, errors.New("repositoryhttp: a service, a keyed creator and a cursor codec are required")
	}
	return &Handler{svc: cfg.Service, creator: cfg.Creator, codec: cfg.Cursors}, nil
}

// RegisterRoutes contributes exactly the repository resource: list, create,
// read, patch, archive, restore and refs. It registers no DELETE, push-record,
// credential or Git-transport route. Every route carries an authorization
// action; the router authenticates, throttles and authorizes before a handler
// runs.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: projectBase, Action: ActionList, Handler: h.list},
		{Method: http.MethodPost, Path: projectBase, Action: ActionCreate, Handler: h.create},
		{Method: http.MethodGet, Path: projectBase + "/{id}", Action: ActionRead, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.get},
		{Method: http.MethodPatch, Path: projectBase + "/{id}", Action: ActionPatch, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.patch},
		{Method: http.MethodPost, Path: projectBase + "/{id}/archive", Action: ActionArchive, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.archive},
		{Method: http.MethodPost, Path: projectBase + "/{id}/restore", Action: ActionRestore, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.restore},
		{Method: http.MethodGet, Path: projectBase + "/{id}/refs", Action: ActionRefs, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.refs},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

// actorOf is the router's verified principal as a person; neither it nor its
// type is ever read from the request body.
func actorOf(c *httpapi.Call) repository.Actor {
	return repository.Actor{Type: repository.ActorUser, ID: c.Principal.ID}
}

func (h *Handler) ctx(r *http.Request, c *httpapi.Call) context.Context {
	return repository.WithRequestID(r.Context(), c.RequestID)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapError(err))
}

func (h *Handler) scope(w http.ResponseWriter, r *http.Request, c *httpapi.Call) (domain.Scope, bool) {
	scope, err := c.Scope()
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError("the tenant or project identifier is not valid"))
		return domain.Scope{}, false
	}
	return scope, true
}

// emptyBody accepts no body or exactly `{}`: these operations take no input,
// and a typo must not be silently dropped.
func emptyBody(w http.ResponseWriter, r *http.Request) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return httpapi.NewError(http.StatusRequestEntityTooLarge, domain.CodeInvalidRequest, "the request body is too large")
		}
		return httpapi.BadRequestError("the request body could not be read")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var v struct{} // strict: any member is an error, never silently dropped
	return httpapi.DecodeJSONBytes(b, &v)
}

// headers are the response headers worth replaying: the ETag, and the location
// of a newly reserved repository.
func headers(r *http.Request, repo *repository.Repository, created bool) (map[string]string, error) {
	etag, err := httpapi.FormatETag(repo.Version)
	if err != nil {
		return nil, httpapi.InternalServerError(err)
	}
	h := map[string]string{"ETag": etag}
	if created {
		h["Location"] = strings.TrimSuffix(r.URL.Path, "/") + "/" + string(repo.ID)
	}
	return h, nil
}

func (h *Handler) writeRepository(w http.ResponseWriter, r *http.Request, c *httpapi.Call, status int, repo *repository.Repository) {
	res, err := h.describe(h.ctx(r, c), repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := httpapi.SetETag(w, repo.Version); err != nil {
		h.fail(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, status, res)
}

func (h *Handler) describe(ctx context.Context, repo *repository.Repository) (gitv1.Repository, error) {
	url, err := h.svc.CloneURL(ctx, repo)
	if err != nil {
		return gitv1.Repository{}, err
	}
	return repository.Representation(repo, url)
}

// create reserves a repository under the request's idempotency identity. The
// body is read unmodified so the idempotency request hash covers its exact
// bytes, then decoded strictly by the contract: an unknown, misspelled or
// duplicate member is 400 before any state exists. The worker is called after
// the reservation committed, outside any transaction.
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
	req, err := gitv1.DecodeRequest[gitv1.CreateRepositoryRequest](body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ctx := h.ctx(r, c)
	plan, err := h.svc.Prepare(ctx, actorOf(c), scope, req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The clone URL depends on the project and the name only, which an existing
	// equivalent repository shares, so it is resolved before the transaction.
	url, err := h.svc.CloneURL(ctx, plan.Repository)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	render := func(repo *repository.Repository, created bool) (httpapi.Outcome, error) {
		res, err := repository.Representation(repo, url)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		hd, err := headers(r, repo, created)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		resp, err := httpapi.NewStoredResponse(r, status, res, hd)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		return httpapi.Outcome{Response: resp, Target: httpapi.Reference{Kind: resourceKind, ID: string(repo.ID)}}, nil
	}
	resp, replayed, repo, err := h.creator.CreateKeyed(ctx, id, c.AuthorizeReference, plan, render)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, resp, replayed)
	if replayed || repo == nil || repo.Status != gitv1.StatusProvisioning {
		return
	}
	// The reservation is committed. Initialize the repository now; a failure
	// leaves it provisioning with a reason and the reconciler repeats the
	// (idempotent) initialization, so the response above stays truthful.
	if _, err := h.svc.Provision(context.WithoutCancel(ctx), scope, repo.ID); err != nil {
		_ = err // recorded as the repository's reason; never surfaced to the creator
	}
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	repo, err := h.svc.Get(h.ctx(r, c), actorOf(c), scope, repository.ID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeRepository(w, r, c, http.StatusOK, repo)
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	req, err := gitv1.DecodeRequest[gitv1.PatchRepositoryRequest](body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	repo, err := h.svc.Update(h.ctx(r, c), repository.UpdateCommand{
		Actor: actorOf(c), Scope: scope, ID: repository.ID(r.PathValue("id")), Expected: expected, Patch: req,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeRepository(w, r, c, http.StatusOK, repo)
}

// transition runs archive or restore: If-Match and no body.
func (h *Handler) transition(w http.ResponseWriter, r *http.Request, c *httpapi.Call,
	do func(ctx context.Context, actor repository.Actor, scope domain.Scope, id repository.ID, expected domain.Version) (*repository.Repository, error)) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err == nil {
		err = emptyBody(w, r)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	repo, err := do(h.ctx(r, c), actorOf(c), scope, repository.ID(r.PathValue("id")), expected)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeRepository(w, r, c, http.StatusOK, repo)
}

func (h *Handler) archive(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	h.transition(w, r, c, h.svc.Archive)
}

func (h *Handler) restore(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	h.transition(w, r, c, h.svc.Restore)
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
	q := repository.ListQuery{Limit: page.FetchLimit()}
	if page.Position != nil {
		var pos listPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || repository.ID(pos.ID).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		q.After = &repository.Position{CreatedAt: time.UnixMicro(pos.T).UTC(), ID: repository.ID(pos.ID)}
	}
	ctx := h.ctx(r, c)
	repos, err := h.svc.List(ctx, actorOf(c), scope, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type row struct {
		res gitv1.Repository
		at  time.Time
	}
	rows := make([]row, 0, len(repos))
	for _, repo := range repos {
		res, err := h.describe(ctx, repo)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		rows = append(rows, row{res: res, at: repo.CreatedAt})
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, rows, func(d row) ([]byte, error) {
		return json.Marshal(listPosition{T: d.at.UnixMicro(), ID: d.res.ID})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]gitv1.Repository, 0, len(out.Items))
	for _, it := range out.Items {
		items = append(items, it.res)
	}
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.ListPage[gitv1.Repository]{Items: items, NextCursor: out.NextCursor})
}

func (h *Handler) refs(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r, "prefix"); err != nil {
		h.fail(w, r, err)
		return
	}
	prefix := gitv1.HeadsPrefix
	switch vs := r.URL.Query()["prefix"]; len(vs) {
	case 0:
	case 1:
		prefix = vs[0]
	default:
		h.fail(w, r, httpapi.BadRequestError("prefix must be given once"))
		return
	}
	if prefix != gitv1.HeadsPrefix && prefix != gitv1.TagsPrefix {
		h.fail(w, r, httpapi.BadRequestError("prefix must be refs/heads/ or refs/tags/"))
		return
	}
	cs := c.CursorScope(map[string][]string{"prefix": {prefix}}, refsSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := repository.RefsQuery{Prefix: prefix, Limit: page.Limit}
	if page.Position != nil {
		cur := string(page.Position)
		q.Cursor = &cur
	}
	resp, err := h.svc.ReadRefs(h.ctx(r, c), actorOf(c), scope, repository.ID(r.PathValue("id")), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gitv1.RefsResponse{Refs: resp.Refs, Empty: resp.Empty}
	if out.Refs == nil {
		out.Refs = []gitv1.RefTip{}
	}
	if resp.NextCursor != nil {
		tok, err := h.codec.Encode(cs, []byte(*resp.NextCursor))
		if err != nil {
			h.fail(w, r, httpapi.InternalServerError(err))
			return
		}
		out.NextCursor = &tok
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

// messages are the client-safe texts of the module's failures. Nothing about a
// foreign resource, a worker or an internal cause reaches the client.
var messages = map[domain.ErrorCode]string{
	domain.CodeNotFound:           "the resource was not found",
	domain.CodeForbidden:          "the caller may not perform this operation",
	domain.CodeVersionConflict:    "the repository changed; read it again and retry",
	gitv1.CodeRepositoryNameTaken: "the name is already used by another repository of the project",
	gitv1.CodeRepositoryNotReady:  "the repository is not ready",
	gitv1.CodeRepositoryArchived:  "the repository is archived",
	gitv1.CodeHostingMismatch:     "this repository is hosted externally and is read-only here",
	domain.CodeUnavailable:        "a dependency is unavailable; retry",
}

// mapError renders a repository or contract error as an API error.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	var ce *gitv1.ContractError
	if errors.As(err, &ce) {
		if status, ok := gitv1.Status(ce.Code); ok {
			return httpapi.NewError(status, ce.Code, ce.Message)
		}
		return err
	}
	code, ok := repository.ContractCode(err)
	if !ok {
		return err // unclassified: the router renders a fixed 500
	}
	status, _ := gitv1.Status(code)
	switch code {
	case domain.CodeUnavailable:
		return httpapi.UnavailableError(time.Second, err)
	case domain.CodeInvalidRequest:
		return httpapi.NewError(status, code, clientMessage(err))
	}
	msg, ok := messages[code]
	if !ok {
		return err
	}
	return httpapi.NewError(status, code, msg)
}

// clientMessage extracts the validation text after the shared sentinel, for
// example "invalid value: name: does not match ..." -> "name: does not match
// ...". Wrapped infrastructure causes are never included.
func clientMessage(err error) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, repository.ErrInvalidValue.Error()+": "); ok {
		msg = rest
	}
	if i := strings.Index(msg, ": repository: "); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 512 || msg == "" {
		return "the request could not be processed"
	}
	return msg
}

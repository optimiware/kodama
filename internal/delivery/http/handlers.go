// Package deliveryhttp is the HTTP adapter of the delivery module: the public
// environment and deployment routes, and (in runtime.go) the separately
// mountable runtime routes a deployment script calls. It reuses the shared
// router, bounded body reading, strict JSON, quoted-version ETags, scoped
// cursors, idempotency and error envelopes of internal/httpapi, and holds no
// persistence or business rule: every rule comes from internal/delivery.
//
// DTOs are generated from api/delivery.yaml (delivery.gen.go) and mapped by
// dto.go; no aggregate is serialized directly. The router authenticates,
// throttles and authorizes the tenant, project and resource reference before a
// handler runs, and the use cases check the current permission again.
package deliveryhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Resource kinds passed to the router's Authorizer.
const (
	ResourceEnvironment = "environment"
	ResourceDeployment  = "deployment"
	ResourceAttempt     = "attempt"
)

// Actions are the authorization actions of the public routes. The composition
// root maps them to its policy; DefaultActions documents the identity
// permission each one stands for. The use cases check the same permissions
// again through delivery.Access, so a mis-mapped router cannot widen access.
type Actions struct {
	EnvironmentList, EnvironmentRead, EnvironmentCreate, EnvironmentUpdate httpapi.Action
	DeploymentList, DeploymentRead, DeploymentCreate, DeploymentCleanup    httpapi.Action
}

// DefaultActions returns the documented action identifiers. Reads map to the
// identity permission run:read; creation, edits and cleanup to deployment:manage.
func DefaultActions() Actions {
	return Actions{
		EnvironmentList: "environment.list", EnvironmentRead: "environment.read",
		EnvironmentCreate: "environment.create", EnvironmentUpdate: "environment.update",
		DeploymentList: "deployment.list", DeploymentRead: "deployment.read",
		DeploymentCreate: "deployment.create", DeploymentCleanup: "deployment.cleanup",
	}
}

func (a Actions) valid() bool {
	for _, v := range []httpapi.Action{a.EnvironmentList, a.EnvironmentRead, a.EnvironmentCreate, a.EnvironmentUpdate,
		a.DeploymentList, a.DeploymentRead, a.DeploymentCreate, a.DeploymentCleanup} {
		if v == "" {
			return false
		}
	}
	return true
}

// RouteInfo describes one route for the route inventory.
type RouteInfo struct {
	Method string
	// Path is relative to the router's prefix: "/api/v1" for the public routes
	// and "/runtime/v1" for the runtime routes.
	Path   string
	Action httpapi.Action
}

const (
	projectBase  = "/tenants/{tenantId}/projects/{projectId}"
	environments = projectBase + "/environments"
	deployments  = projectBase + "/deployments"
	envSort      = "name"
	depSort      = "id"
)

// PublicRoutes lists the public routes with the given actions: the exported
// inventory the OpenAPI contract test compares with api/delivery.yaml.
func PublicRoutes(a Actions) []RouteInfo {
	return []RouteInfo{
		{http.MethodGet, environments, a.EnvironmentList},
		{http.MethodPost, environments, a.EnvironmentCreate},
		{http.MethodGet, environments + "/{id}", a.EnvironmentRead},
		{http.MethodPatch, environments + "/{id}", a.EnvironmentUpdate},
		{http.MethodGet, deployments, a.DeploymentList},
		{http.MethodPost, deployments, a.DeploymentCreate},
		{http.MethodGet, deployments + "/{id}", a.DeploymentRead},
		{http.MethodPost, deployments + "/{id}/cleanup", a.DeploymentCleanup},
	}
}

// Service is the consumer-owned port of the public handlers; *delivery.Service
// satisfies it.
type Service interface {
	CreateEnvironment(ctx context.Context, c delivery.Caller, scope domain.Scope, in delivery.EnvironmentInput) (delivery.Environment, error)
	GetEnvironment(ctx context.Context, c delivery.Caller, scope domain.Scope, id domain.EnvironmentID) (delivery.Environment, error)
	ListEnvironments(ctx context.Context, c delivery.Caller, scope domain.Scope, afterName string, limit int) ([]delivery.Environment, error)
	UpdateEnvironment(ctx context.Context, c delivery.Caller, scope domain.Scope, id domain.EnvironmentID, expected domain.Version, edit delivery.EnvironmentEdit) (delivery.Environment, error)

	GetDeployment(ctx context.Context, c delivery.Caller, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error)
	ListDeployments(ctx context.Context, c delivery.Caller, scope domain.Scope, f delivery.DeploymentFilter, afterID delivery.DeploymentID, limit int) ([]delivery.Deployment, error)
	RequestCleanupAt(ctx context.Context, c delivery.Caller, scope domain.Scope, id delivery.DeploymentID, expected domain.Version) (delivery.Receipt, error)

	PlanRegistrationForAttempt(ctx context.Context, c delivery.Caller, scope domain.Scope, attempt domain.AttemptID, req delivery.RegistrationRequest) (delivery.RegistrationPlan, error)
	CompleteRegistration(ctx context.Context, plan delivery.RegistrationPlan, res delivery.RegisterResult, commitErr error) (delivery.Receipt, error)
	CompleteReplay(ctx context.Context, plan delivery.RegistrationPlan)
	Receipt(d delivery.Deployment) delivery.Receipt
}

var _ Service = (*delivery.Service)(nil)

// Registrar commits a planned registration inside the HTTP foundation's keyed
// (idempotent) create transaction; *postgres.Keyed satisfies it. The deployment,
// its history, audit entry, event and the stored response commit together, and a
// replay never runs the registration.
type Registrar interface {
	RegisterKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
		cmd delivery.RegisterCommand, render func(delivery.RegisterResult) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, delivery.RegisterResult, error)
}

// Handler serves the public routes.
type Handler struct {
	svc     Service
	keyed   Registrar
	codec   *httpapi.CursorCodec
	actions Actions
}

// NewHandler requires the use-case service, the keyed registrar, the cursor
// codec and the router actions.
func NewHandler(svc Service, keyed Registrar, codec *httpapi.CursorCodec, actions Actions) (*Handler, error) {
	if svc == nil || keyed == nil || codec == nil || !actions.valid() {
		return nil, errors.New("deliveryhttp: a service, a keyed registrar, a cursor codec and every action are required")
	}
	return &Handler{svc: svc, keyed: keyed, codec: codec, actions: actions}, nil
}

// RegisterRoutes registers the public routes on a router whose prefix is
// "/api/v1". There is no environment deletion, deployment mutation or settled
// outcome route: outcomes are reported by the runtime routes only.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	handlers := map[string]httpapi.Handler{
		http.MethodGet + " " + environments:                   h.listEnvironments,
		http.MethodPost + " " + environments:                  h.createEnvironment,
		http.MethodGet + " " + environments + "/{id}":         h.getEnvironment,
		http.MethodPatch + " " + environments + "/{id}":       h.updateEnvironment,
		http.MethodGet + " " + deployments:                    h.listDeployments,
		http.MethodPost + " " + deployments:                   h.createDeployment,
		http.MethodGet + " " + deployments + "/{id}":          h.getDeployment,
		http.MethodPost + " " + deployments + "/{id}/cleanup": h.requestCleanup,
	}
	for _, info := range PublicRoutes(h.actions) {
		route := httpapi.Route{Method: info.Method, Path: info.Path, Action: info.Action, Handler: handlers[info.Method+" "+info.Path]}
		switch {
		case strings.HasPrefix(info.Path, environments+"/"):
			route.ResourceKind, route.ResourceParam = ResourceEnvironment, "id"
		case strings.HasPrefix(info.Path, deployments+"/"):
			route.ResourceKind, route.ResourceParam = ResourceDeployment, "id"
		}
		if err := rt.Handle(route); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) caller(c *httpapi.Call) delivery.Caller {
	// The principal is the router's verified identity, never a body field.
	return delivery.Caller{Actor: delivery.Actor{Type: delivery.ActorUser, ID: c.Principal.ID}, RequestID: c.RequestID}
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

func writeWithETag(w http.ResponseWriter, r *http.Request, status int, v domain.Version, body any, location string) {
	if err := httpapi.SetETag(w, v); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	if location != "" {
		w.Header().Set("Location", location)
	}
	httpapi.WriteJSON(w, r, status, body)
}

func location(r *http.Request, id string) string {
	return strings.TrimSuffix(r.URL.Path, "/") + "/" + id
}

// ---- environments ----------------------------------------------------------

type environmentPosition struct {
	Name string `json:"n"`
}

func (h *Handler) listEnvironments(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r); err != nil {
		h.fail(w, r, err)
		return
	}
	cs := c.CursorScope(map[string][]string{}, envSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var after string
	if page.Position != nil {
		var pos environmentPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || pos.Name == "" {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		after = pos.Name
	}
	envs, err := h.svc.ListEnvironments(r.Context(), h.caller(c), scope, after, page.FetchLimit())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]Environment, 0, len(envs))
	for _, e := range envs {
		items = append(items, environmentDTO(e))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, items, func(e Environment) ([]byte, error) {
		return json.Marshal(environmentPosition{Name: e.Name})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) createEnvironment(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req EnvironmentCreate
	if err := decodeStrict(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	env, err := h.svc.CreateEnvironment(r.Context(), h.caller(c), scope, environmentInput(req))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusCreated, env.Version(), environmentDTO(env), location(r, string(env.ID())))
}

func (h *Handler) getEnvironment(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	env, err := h.svc.GetEnvironment(r.Context(), h.caller(c), scope, domain.EnvironmentID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusOK, env.Version(), environmentDTO(env), "")
}

func (h *Handler) updateEnvironment(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	var req EnvironmentEdit
	if err := decodeStrict(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.Target == nil && req.Namespace == nil && req.Kubeconfig == nil && req.Secrets == nil && req.Config == nil && req.PreviewTtlSeconds == nil {
		h.fail(w, r, httpapi.BadRequestError("an edit changes at least one member"))
		return
	}
	env, err := h.svc.UpdateEnvironment(r.Context(), h.caller(c), scope, domain.EnvironmentID(r.PathValue("id")), expected, environmentEdit(req))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusOK, env.Version(), environmentDTO(env), "")
}

// ---- deployments -----------------------------------------------------------

type deploymentPosition struct {
	ID string `json:"id"`
}

func (h *Handler) listDeployments(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r, "environmentId", "status"); err != nil {
		h.fail(w, r, err)
		return
	}
	var f delivery.DeploymentFilter
	filters := map[string][]string{}
	q := r.URL.Query()
	for name, set := range map[string]func(string){
		"environmentId": func(v string) { f.Environment = domain.EnvironmentID(v) },
		"status":        func(v string) { f.Status = delivery.Status(v) },
	} {
		vals := q[name]
		if len(vals) == 0 {
			continue
		}
		if len(vals) != 1 || vals[0] == "" {
			h.fail(w, r, httpapi.BadRequestError(name+" must be given once"))
			return
		}
		set(vals[0])
		filters[name] = []string{vals[0]}
	}
	cs := c.CursorScope(filters, depSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var after delivery.DeploymentID
	if page.Position != nil {
		var pos deploymentPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || delivery.DeploymentID(pos.ID).Validate() != nil {
			h.fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		after = delivery.DeploymentID(pos.ID)
	}
	deps, err := h.svc.ListDeployments(r.Context(), h.caller(c), scope, f, after, page.FetchLimit())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]Deployment, 0, len(deps))
	for _, d := range deps {
		items = append(items, deploymentDTO(d))
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, items, func(d Deployment) ([]byte, error) {
		return json.Marshal(deploymentPosition{ID: d.Id})
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) getDeployment(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	d, err := h.svc.GetDeployment(r.Context(), h.caller(c), scope, delivery.DeploymentID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusOK, d.Version(), deploymentDTO(d), "")
}

func (h *Handler) requestCleanup(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The transition takes no body; a body is a client that believes it is
	// sending something this route would ignore.
	if hasBody(w, r) {
		h.fail(w, r, httpapi.BadRequestError("this request takes no body"))
		return
	}
	rc, err := h.svc.RequestCleanupAt(r.Context(), h.caller(c), scope, delivery.DeploymentID(r.PathValue("id")), expected)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeWithETag(w, r, http.StatusOK, rc.Deployment.Version(), deploymentDTO(rc.Deployment), "")
}

// hasBody reports whether the request carries any body byte.
func hasBody(w http.ResponseWriter, r *http.Request) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	return len(b) != 0 || err != nil
}

// createDeployment registers a deployment against an existing attempt's pinned
// context. Order: authorize the human caller, read the attempt's immutable
// context and retained configuration, verify the image's provenance and protect
// it from retention (all outside the keyed transaction, so no network or
// cross-owner call runs inside it); then commit the record, history, audit,
// event and the stored response together. A replay releases the stored response
// only after authorization is checked again, and the preparation pin it re-took
// is reconciled with the authoritative record.
func (h *Handler) createDeployment(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
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
	var req DeploymentCreate
	if err := decodeStrict(body, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	attempt := domain.AttemptID(req.AttemptId)
	if err := attempt.Validate(); err != nil {
		h.fail(w, r, httpapi.BadRequestError("the attempt identifier is not valid"))
		return
	}
	ctx := r.Context()
	plan, err := h.svc.PlanRegistrationForAttempt(ctx, h.caller(c), scope, attempt, registrationRequest(req.Source, req.Image))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	render := func(res delivery.RegisterResult) (httpapi.Outcome, error) {
		rc := h.svc.Receipt(res.Deployment)
		status := http.StatusOK
		if res.Created {
			status = http.StatusCreated
		}
		tag, err := httpapi.FormatETag(res.Deployment.Version())
		if err != nil {
			return httpapi.Outcome{}, httpapi.InternalServerError(err)
		}
		stored, err := httpapi.NewStoredResponse(r, status, receiptDTO(rc, res.Created),
			map[string]string{"ETag": tag, "Location": location(r, string(res.Deployment.ID()))})
		if err != nil {
			return httpapi.Outcome{}, err
		}
		return httpapi.Outcome{Response: stored, Target: httpapi.Reference{Kind: ResourceDeployment, ID: string(res.Deployment.ID())}}, nil
	}
	stored, replayed, res, err := h.keyed.RegisterKeyed(ctx, id, func(ctx context.Context, ref httpapi.Reference) error {
		return c.AuthorizeReference(ctx, ref)
	}, plan.Command, render)
	if err != nil {
		// An unknown commit outcome keeps the preparation pin (conservative); a
		// definite refusal reconciles it against the authoritative record.
		_, _ = h.svc.CompleteRegistration(ctx, plan, delivery.RegisterResult{}, commitError(err))
		h.fail(w, r, err)
		return
	}
	if replayed {
		h.svc.CompleteReplay(ctx, plan)
	} else {
		_, _ = h.svc.CompleteRegistration(ctx, plan, res, nil)
	}
	httpapi.WriteStored(w, r, stored, replayed)
}

// commitError classifies a failed keyed create for retention reconciliation. A
// server-side or unknown failure may have committed, so it is ErrUnavailable
// (the pin stays); anything else is a definite refusal that committed nothing.
func commitError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) && api.Status >= 500 {
		return delivery.Fail(delivery.ErrUnavailable, err, "the commit outcome is unknown")
	}
	if errors.Is(err, delivery.ErrUnavailable) {
		return err
	}
	if api != nil {
		return delivery.Fail(delivery.ErrConflict, err, "refused")
	}
	return err
}

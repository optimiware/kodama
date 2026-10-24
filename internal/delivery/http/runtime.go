package deliveryhttp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// RuntimeActions are the router actions of the runtime routes: the
// deployment.call interface of an attempt, an audience distinct from human
// access. The composition root maps them to its runtime policy.
type RuntimeActions struct {
	Register, Lookup, Read, Outcome, CleanupResult httpapi.Action
}

// DefaultRuntimeActions returns the documented action identifiers.
func DefaultRuntimeActions() RuntimeActions {
	return RuntimeActions{
		Register: "deployment.runtime.register", Lookup: "deployment.runtime.lookup", Read: "deployment.runtime.read",
		Outcome: "deployment.runtime.outcome", CleanupResult: "deployment.runtime.cleanup_result",
	}
}

func (a RuntimeActions) valid() bool {
	for _, v := range []httpapi.Action{a.Register, a.Lookup, a.Read, a.Outcome, a.CleanupResult} {
		if v == "" {
			return false
		}
	}
	return true
}

const (
	attemptBase = "/attempts/{attemptId}/deployments"
	maxBearer   = 1100
)

// RuntimeRoutes lists the runtime routes with the given actions, relative to
// the "/runtime/v1" prefix.
func RuntimeRoutes(a RuntimeActions) []RouteInfo {
	return []RouteInfo{
		{http.MethodPost, attemptBase, a.Register},
		{http.MethodGet, attemptBase + "/by-action/{actionKey}", a.Lookup},
		{http.MethodGet, attemptBase + "/{id}", a.Read},
		{http.MethodPost, attemptBase + "/{id}/outcome", a.Outcome},
		{http.MethodPost, attemptBase + "/{id}/cleanup-result", a.CleanupResult},
	}
}

// RuntimeService is the consumer-owned port of the runtime handlers;
// *delivery.Service satisfies it. Each method authenticates deployment.call for
// the route's attempt and checks the attempt's immutable pinned context itself.
type RuntimeService interface {
	RegisterRuntime(ctx context.Context, attempt domain.AttemptID, bearer string, req delivery.RegistrationRequest) (delivery.Receipt, error)
	LookupByAction(ctx context.Context, attempt domain.AttemptID, bearer, actionKey string) (delivery.Receipt, error)
	GetRuntime(ctx context.Context, attempt domain.AttemptID, bearer string, id delivery.DeploymentID) (delivery.Receipt, error)
	ReportOutcome(ctx context.Context, attempt domain.AttemptID, bearer string, id delivery.DeploymentID, rep delivery.OutcomeReport) (delivery.Receipt, error)
	ReportCleanup(ctx context.Context, attempt domain.AttemptID, bearer string, id delivery.DeploymentID, rep delivery.CleanupReport) (delivery.Receipt, error)
}

var _ RuntimeService = (*delivery.Service)(nil)

// Runtime serves the runtime routes. It is mounted separately from the public
// Handler, on a router whose prefix is "/runtime/v1" and whose Authenticator
// accepts attempt bearer tokens: a different audience from a human access
// token, with no tenant or project in its paths. The tenant and project come
// from the verified grant, never from the router target or the request. Every
// route forwards the bearer to the service, which authenticates it again for
// deployment.call, binds the session so the store repeats the check under lock
// in the mutating transaction, and reads the attempt's immutable pinned
// context.
type Runtime struct {
	svc     RuntimeService
	actions RuntimeActions
}

// NewRuntime requires the service and every action.
func NewRuntime(svc RuntimeService, actions RuntimeActions) (*Runtime, error) {
	if svc == nil || !actions.valid() {
		return nil, errors.New("deliveryhttp: a runtime service and every runtime action are required")
	}
	return &Runtime{svc: svc, actions: actions}, nil
}

// RegisterRoutes registers the runtime routes.
func (h *Runtime) RegisterRoutes(rt *httpapi.Router) error {
	handlers := map[string]httpapi.Handler{
		http.MethodPost + " " + attemptBase:                           h.register,
		http.MethodGet + " " + attemptBase + "/by-action/{actionKey}": h.lookup,
		http.MethodGet + " " + attemptBase + "/{id}":                  h.read,
		http.MethodPost + " " + attemptBase + "/{id}/outcome":         h.outcome,
		http.MethodPost + " " + attemptBase + "/{id}/cleanup-result":  h.cleanupResult,
	}
	for _, info := range RuntimeRoutes(h.actions) {
		if err := rt.Handle(httpapi.Route{Method: info.Method, Path: info.Path, Action: info.Action,
			ResourceKind: ResourceAttempt, ResourceParam: "attemptId", Handler: handlers[info.Method+" "+info.Path]}); err != nil {
			return err
		}
	}
	return nil
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapError(err))
}

// bearer extracts the attempt token. A missing, repeated, malformed or
// oversized Authorization header is an invalid credential.
func bearer(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maxBearer {
		return "", delivery.Fail(delivery.ErrUnauthenticated, nil, "no usable bearer token")
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", delivery.Fail(delivery.ErrUnauthenticated, nil, "no usable bearer token")
	}
	return token, nil
}

// begin validates the route's attempt and extracts the bearer.
func begin(r *http.Request) (domain.AttemptID, string, error) {
	// Nothing on the runtime routes is selected by a query parameter.
	if len(r.URL.Query()) != 0 {
		return "", "", httpapi.BadRequestError("this request takes no query parameters")
	}
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		return "", "", httpapi.BadRequestError("the attempt identifier is not valid")
	}
	tok, err := bearer(r)
	if err != nil {
		return "", "", err
	}
	return attempt, tok, nil
}

func deploymentID(r *http.Request) (delivery.DeploymentID, error) {
	id := delivery.DeploymentID(r.PathValue("id"))
	if err := id.Validate(); err != nil {
		return "", httpapi.BadRequestError("the deployment identifier is not valid")
	}
	return id, nil
}

func writeReceipt(w http.ResponseWriter, r *http.Request, status int, rc delivery.Receipt, created bool) {
	writeWithETag(w, r, status, rc.Deployment.Version(), receiptDTO(rc, created), "")
}

func (h *Runtime) register(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
	attempt, tok, err := begin(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		fail(w, r, err)
		return
	}
	var req RuntimeDeploymentRegister
	if err := decodeStrict(body, &req); err != nil {
		fail(w, r, err)
		return
	}
	rc, err := h.svc.RegisterRuntime(r.Context(), attempt, tok, registrationRequest(req.Source, req.Image))
	if err != nil {
		fail(w, r, err)
		return
	}
	status := http.StatusOK
	if rc.Created {
		status = http.StatusCreated
	}
	writeReceipt(w, r, status, rc, rc.Created)
}

func (h *Runtime) lookup(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
	attempt, tok, err := begin(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	rc, err := h.svc.LookupByAction(r.Context(), attempt, tok, r.PathValue("actionKey"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeReceipt(w, r, http.StatusOK, rc, false)
}

func (h *Runtime) read(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
	attempt, tok, err := begin(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	id, err := deploymentID(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	rc, err := h.svc.GetRuntime(r.Context(), attempt, tok, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeReceipt(w, r, http.StatusOK, rc, false)
}

func (h *Runtime) outcome(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
	attempt, tok, err := begin(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	id, err := deploymentID(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		fail(w, r, err)
		return
	}
	var req OutcomeReport
	if err := decodeStrict(body, &req); err != nil {
		fail(w, r, err)
		return
	}
	rc, err := h.svc.ReportOutcome(r.Context(), attempt, tok, id, outcomeReport(req))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeReceipt(w, r, http.StatusOK, rc, false)
}

func (h *Runtime) cleanupResult(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
	attempt, tok, err := begin(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	id, err := deploymentID(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		fail(w, r, err)
		return
	}
	var req CleanupReport
	if err := decodeStrict(body, &req); err != nil {
		fail(w, r, err)
		return
	}
	rc, err := h.svc.ReportCleanup(r.Context(), attempt, tok, id, cleanupReport(req))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeReceipt(w, r, http.StatusOK, rc, false)
}

// Package logshttp is the HTTP adapter of the log module: the one protected
// route that streams a bounded log of one attempt as NDJSON.
//
// The route is contributed to the shared protected router, which authenticates,
// throttles and authorizes the tenant, project and run before the handler runs;
// the module authorizes again, against current membership, for every call. The
// query accepts only attemptId, tail, follow and source, each at most once. No
// namespace, Pod, container, selector or backend URL is accepted. Errors found
// before output starts use the standard error envelope; after the first record
// a failure is reported only as a classified stop reason in the final summary.
package logshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/logs"
)

// ActionLogs is the authorization action of the route. The composition root
// maps it through RoutePermissions.
const ActionLogs httpapi.Action = "run.logs"

// ResourceRun is the resource kind passed to the Authorizer.
const ResourceRun = "run"

// RoutePermissions is the route-action mapping onto existing permissions: the
// coarse route gate needs run:read; the module's authority adapter then requires
// both run:read and evidence:read on every call. No permission is added.
func RoutePermissions() map[httpapi.Action][]identity.Permission {
	return map[httpapi.Action][]identity.Permission{ActionLogs: {identity.PermRunRead}}
}

// Opener is what the route needs from the module. *logs.Service satisfies it.
type Opener interface {
	Open(ctx context.Context, req logs.Request) (*logs.Session, error)
}

// Handler serves GET /tenants/{tenantId}/projects/{projectId}/runs/{id}/logs.
type Handler struct {
	svc          Opener
	writeTimeout time.Duration
}

// NewHandler requires the service and the bound on one blocked write to the
// client (the slow-consumer limit).
func NewHandler(svc Opener, writeTimeout time.Duration) (*Handler, error) {
	if svc == nil || writeTimeout <= 0 {
		return nil, errors.New("logshttp: a service and a positive write timeout are required")
	}
	return &Handler{svc: svc, writeTimeout: writeTimeout}, nil
}

// RegisterRoutes registers the protected route on a router whose prefix is
// "/api/v1".
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	return rt.Handle(httpapi.Route{
		Method: http.MethodGet, Path: "/tenants/{tenantId}/projects/{projectId}/runs/{id}/logs", Action: ActionLogs,
		ResourceKind: ResourceRun, ResourceParam: "id", Handler: h.logs,
	})
}

func (h *Handler) logs(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		httpapi.WriteError(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	req, err := parseQuery(r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	req.Principal, req.Scope, req.Run = c.Principal.ID, scope, domain.RunID(r.PathValue("id"))

	sess, err := h.svc.Open(r.Context(), req)
	if err != nil {
		httpapi.WriteError(w, r, translate(err))
		return
	}
	// From here the session owns the upstream stream and the stream slot, and
	// Run closes it on every path.
	sink := &ndjson{w: w, rc: http.NewResponseController(w), timeout: h.writeTimeout, follow: req.Follow}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/x-ndjson")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if err := sess.Run(r.Context(), sink); err != nil {
		// The client is gone or too slow, or the response is already broken:
		// there is nothing safe left to send. Abort so the connection is not reused.
		panic(http.ErrAbortHandler)
	}
}

// parseQuery accepts only the documented parameters, each once, with strict
// syntax; anything else is 400 so a typo can never silently widen a read.
func parseQuery(r *http.Request) (logs.Request, error) {
	var req logs.Request
	q := r.URL.Query()
	for k, vs := range q {
		switch k {
		case "attemptId", "tail", "follow", "source":
		default:
			return req, httpapi.BadRequestError("unsupported query parameter")
		}
		if len(vs) != 1 {
			return req, httpapi.BadRequestError("each query parameter may be given once")
		}
	}
	if v, ok := q["attemptId"]; ok {
		id := domain.AttemptID(v[0])
		if id.Validate() != nil {
			return req, httpapi.BadRequestError("attemptId is not valid")
		}
		req.Attempt = id
	}
	if v, ok := q["tail"]; ok {
		n, err := strconv.Atoi(v[0])
		if err != nil || strconv.Itoa(n) != v[0] || n < 1 {
			return req, httpapi.BadRequestError("tail must be a positive integer")
		}
		req.Tail = &n
	}
	if v, ok := q["follow"]; ok {
		switch v[0] {
		case "true":
			req.Follow = true
		case "false":
		default:
			return req, httpapi.BadRequestError("follow must be true or false")
		}
	}
	if v, ok := q["source"]; ok {
		req.Mode = logs.Mode(v[0])
		if !req.Mode.Valid() {
			return req, httpapi.BadRequestError("source must be auto, live, wrapper or controller")
		}
	}
	return req, nil
}

// ndjson writes bounded records and enforces the slow-consumer limit: every
// write must complete within the timeout or the read ends.
type ndjson struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	follow  bool
}

func (s *ndjson) write(v any, flush bool) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	if flush {
		if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	return nil
}

func (s *ndjson) Begin(m logs.Meta) error { return s.write(toMeta(m), true) }
func (s *ndjson) Chunk(c logs.Chunk) error {
	// A follow flushes every line so a tail is live; a bounded read lets the
	// server buffer fill and flush.
	return s.write(toChunk(c), s.follow)
}
func (s *ndjson) End(sum logs.Summary) error { return s.write(toSummary(sum), true) }

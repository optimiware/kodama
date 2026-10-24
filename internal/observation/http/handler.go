// Package observationhttp is the HTTP adapter of the observation module: the
// project and run event routes, serving server-sent events or a JSON
// snapshot-plus-cursor from the same cursor scope. It reuses the shared router,
// scoped cursors and error envelopes of internal/httpapi and contains no
// persistence code.
package observationhttp

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/observation"
)

const (
	tenantBase  = "/tenants/{tenantId}"
	projectBase = tenantBase + "/projects/{projectId}"

	resourceRun = "run"

	// LastEventIDHeader is the SSE resume header.
	LastEventIDHeader = "Last-Event-ID"
)

// HandlerOptions bounds a response. Every value is explicit; the composition
// root supplies deployment values.
type HandlerOptions struct {
	// WriteTimeout is the deadline of every write to the client. A reader that
	// does not accept data within it is disconnected as a slow client. It
	// replaces any server-wide WriteTimeout for these responses.
	WriteTimeout time.Duration
	// MaxBufferedBytes bounds the frames held before they are written.
	MaxBufferedBytes int
	// Logger receives stream-ending records (types only, never payloads); nil
	// discards them.
	Logger *slog.Logger
}

func (o HandlerOptions) validate() error {
	if o.WriteTimeout <= 0 || o.MaxBufferedBytes < 1 {
		return errors.New("observationhttp: the write timeout and the buffer bound must be positive")
	}
	return nil
}

// Handler serves the observation routes.
type Handler struct {
	svc   *observation.Service
	codec *httpapi.CursorCodec
	opts  HandlerOptions
}

// NewHandler requires the use-case service, the cursor codec and valid bounds.
func NewHandler(svc *observation.Service, codec *httpapi.CursorCodec, opts HandlerOptions) (*Handler, error) {
	if svc == nil || codec == nil {
		return nil, errors.New("observationhttp: a service and a cursor codec are required")
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{svc: svc, codec: codec, opts: opts}, nil
}

// RegisterRoutes contributes exactly the two event routes. Every route carries an
// authorization action; the router authenticates, throttles and authorizes
// before a handler runs.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: projectBase + "/events", Action: ActionProjectEvents, Handler: h.projectEvents},
		{Method: http.MethodGet, Path: projectBase + "/runs/{id}/events", Action: ActionRunEvents,
			ResourceKind: resourceRun, ResourceParam: "id", Handler: h.runEvents},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapError(err))
}

func (h *Handler) projectEvents(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	h.serve(w, r, c, "")
}

func (h *Handler) runEvents(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	h.serve(w, r, c, domain.RunID(c.Target.Resource.ID))
}

// representation is the negotiated response format.
type representation int

const (
	reprSSE representation = iota
	reprJSON
)

// negotiate selects the representation from Accept. An exact text/event-stream
// wins, then an exact application/json; a missing header or a wildcard selects
// the event stream; anything else is refused.
func negotiate(r *http.Request) (representation, error) {
	values := r.Header.Values("Accept")
	if len(values) == 0 {
		return reprSSE, nil
	}
	var sse, js, wild bool
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			if q, ok := params["q"]; ok {
				if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
					continue
				}
			}
			switch mt {
			case "text/event-stream":
				sse = true
			case "application/json":
				js = true
			case "*/*", "text/*", "application/*":
				wild = true
			}
		}
	}
	switch {
	case sse:
		return reprSSE, nil
	case js:
		return reprJSON, nil
	case wild:
		return reprSSE, nil
	}
	return 0, httpapi.BadRequestError("Accept must allow text/event-stream or application/json")
}

// parseTypes validates the query: only the repeatable type filter is accepted.
func parseTypes(r *http.Request) ([]string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, httpapi.BadRequestError("the query string is not valid")
	}
	for k := range q {
		if k != "type" {
			return nil, httpapi.BadRequestError("unsupported query parameter " + strconv.Quote(k))
		}
	}
	return q["type"], nil
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, c *httpapi.Call, run domain.RunID) {
	types, err := parseTypes(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	scope, err := observation.NewScope(c.Target.TenantID, c.Target.ProjectID, run, types)
	if err != nil {
		h.fail(w, r, httpapi.BadRequestError(scopeMessage(err)))
		return
	}
	repr, err := negotiate(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cs := cursorScope(c, scope)
	actor := observation.Actor{ID: c.Principal.ID}

	headers := r.Header.Values(LastEventIDHeader)
	if len(headers) > 1 {
		h.fail(w, r, invalidCursor()) // ambiguous resume input
		return
	}
	if repr == reprJSON {
		if len(headers) > 0 {
			h.fail(w, r, httpapi.BadRequestError("Last-Event-ID resumes an event stream; request a snapshot without it"))
			return
		}
		h.snapshot(w, r, actor, scope, cs)
		return
	}

	var resumeAt *int64
	if len(headers) == 1 {
		pos, err := resume(h.codec, cs, headers[0])
		if err != nil {
			h.fail(w, r, err)
			return
		}
		resumeAt = &pos
	}
	if err := probeStreaming(w, h.opts.WriteTimeout); err != nil {
		h.fail(w, r, httpapi.InternalServerError(err))
		return
	}
	stream, err := h.svc.Open(r.Context(), observation.Request{Actor: actor, Scope: scope, Resume: resumeAt})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.stream(w, r, stream, cs)
}

func scopeMessage(err error) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, observation.ErrInvalid.Error()+": "); ok && len(rest) <= 256 {
		return rest
	}
	return "the request is not valid for this stream"
}

func (h *Handler) snapshot(w http.ResponseWriter, r *http.Request, actor observation.Actor, scope observation.Scope, cs httpapi.CursorScope) {
	snap, err := h.svc.Snapshot(r.Context(), actor, scope)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cur, err := issue(h.codec, cs, snap.Position)
	if err != nil {
		h.fail(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, snapshotDTO(cur, snap))
}

// stream commits the event-stream response and runs the session. After the
// headers are committed a failure can only close the stream: the connection is
// ended and the client resumes from the last cursor it received.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, stream *observation.Stream, cs httpapi.CursorScope) {
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sink := newSink(w, h.codec, cs, h.opts)
	if err := sink.flushHeaders(); err != nil {
		h.end(r, err)
		return
	}
	h.end(r, stream.Run(r.Context(), sink))
}

// end records why a stream ended. Cancellation is the normal end of a stream.
func (h *Handler) end(r *http.Request, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	h.opts.Logger.LogAttrs(r.Context(), slog.LevelInfo, "observation stream ended",
		slog.String("requestId", httpapi.RequestIDFrom(r.Context())),
		slog.String("reason", reason(err)))
}

func reason(err error) string {
	switch {
	case errors.Is(err, observation.ErrSlowClient):
		return "slow_client"
	case errors.Is(err, observation.ErrAccessRevoked):
		return "access_revoked"
	case errors.Is(err, observation.ErrCursorExpired):
		return "cursor_expired"
	case errors.Is(err, observation.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	}
	return "error"
}

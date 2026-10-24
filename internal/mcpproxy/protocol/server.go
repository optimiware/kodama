package protocol

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// RoutePattern is the module-owned runtime MCP endpoint, relative to the
// server root. It is separate from the frozen runtime API and from the human
// API: it accepts only an attempt token.
const RoutePattern = "/internal/mcp/v1/attempts/{attemptId}/servers/{registrationId}"

// Runtime is the consumer-owned port of the handler; *mcpproxy.Service
// satisfies it.
type Runtime interface {
	Authenticate(ctx context.Context, bearer string, attempt domain.AttemptID) (mcpproxy.Caller, error)
	OpenSession(ctx context.Context, c mcpproxy.Caller, reg mcpproxy.RegistrationID) (mcpproxy.SessionID, error)
	Session(ctx context.Context, c mcpproxy.Caller, reg mcpproxy.RegistrationID, id mcpproxy.SessionID) error
	Discover(ctx context.Context, c mcpproxy.Caller, reg mcpproxy.RegistrationID, id mcpproxy.SessionID) ([]mcpproxy.RemoteTool, error)
	Invoke(ctx context.Context, c mcpproxy.Caller, reg mcpproxy.RegistrationID, id mcpproxy.SessionID, req mcpproxy.InvokeRequest) (mcpproxy.RemoteResult, error)
	CloseSession(ctx context.Context, c mcpproxy.Caller, reg mcpproxy.RegistrationID, id mcpproxy.SessionID) error
	Abandon(ctx context.Context, c mcpproxy.Caller, reg mcpproxy.RegistrationID, id mcpproxy.SessionID)
}

var _ Runtime = (*mcpproxy.Service)(nil)

// ServerConfig configures the workload-facing handler.
type ServerConfig struct {
	Runtime Runtime
	// IdleTimeout closes SDK sessions with no request for this long.
	IdleTimeout time.Duration
	// MaxRequestBytes bounds every request body.
	MaxRequestBytes int64
	Logger          *slog.Logger
}

// MaxHeaderBytes is for the composition root's http.Server.
const MaxHeaderBytes = 16 << 10

type handler struct {
	cfg ServerConfig
	sdk *mcp.StreamableHTTPHandler
	log *slog.Logger
}

type ctxKey int

const sessionKey ctxKey = 1

type opening struct {
	id  mcpproxy.SessionID
	reg mcpproxy.RegistrationID
	c   mcpproxy.Caller
}

// NewHandler returns the handler for RoutePattern. Authorization happens before
// the SDK sees any request, and again in every tools method: the SDK never
// decides who may use a session.
func NewHandler(cfg ServerConfig) (http.Handler, error) {
	if cfg.Runtime == nil || cfg.IdleTimeout <= 0 || cfg.MaxRequestBytes <= 0 {
		return nil, errors.New("protocol: a runtime, an idle timeout and a request bound are required")
	}
	if err := VerifyCompatibility(); err != nil {
		return nil, err
	}
	h := &handler{cfg: cfg, log: cfg.Logger}
	if h.log == nil {
		h.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h.sdk = mcp.NewStreamableHTTPHandler(h.serverFor, &mcp.StreamableHTTPOptions{
		SessionTimeout:      cfg.IdleTimeout,
		JSONResponse:        true,
		MaxRequestBodyBytes: cfg.MaxRequestBytes,
	})
	mux := http.NewServeMux()
	for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		mux.HandleFunc(m+" "+RoutePattern, h.serve)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeError(w, http.StatusNotFound, "not_found") })
	return mux, nil
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Bearer realm="kodama-mcp"`)
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
}

// fail renders a classified failure with constant text only.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mcpproxy.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "invalid_credential")
	case errors.Is(err, mcpproxy.ErrDenied):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, mcpproxy.ErrNotFound), errors.Is(err, mcpproxy.ErrSessionInvalid), errors.Is(err, mcpproxy.ErrBindingInvalid), errors.Is(err, mcpproxy.ErrToolHidden):
		writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, mcpproxy.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, mcpproxy.ErrProtocol):
		writeError(w, http.StatusBadGateway, "unsupported_protocol")
	case errors.Is(err, mcpproxy.ErrDestination), errors.Is(err, mcpproxy.ErrCredential):
		writeError(w, http.StatusBadGateway, "remote_unavailable")
	default:
		writeError(w, http.StatusServiceUnavailable, "unavailable")
	}
}

// bearer extracts the single Authorization bearer.
func bearer(h http.Header) (string, error) {
	vs := h.Values("Authorization")
	if len(vs) != 1 || len(vs[0]) > 1100 {
		return "", mcpproxy.ErrUnauthenticated
	}
	scheme, tok, ok := strings.Cut(vs[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", mcpproxy.ErrUnauthenticated
	}
	return tok, nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	reg := mcpproxy.RegistrationID(r.PathValue("registrationId"))
	if attempt.Validate() != nil || reg.Validate() != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	tok, err := bearer(r.Header)
	if err != nil {
		fail(w, err)
		return
	}
	// Every request, whatever its MCP method, authenticates and asks for the
	// tool.invoke action before the SDK is involved.
	caller, err := h.cfg.Runtime.Authenticate(r.Context(), tok, attempt)
	if err != nil {
		fail(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxRequestBytes)

	sid := r.Header.Get("Mcp-Session-Id")
	if sid != "" {
		id := mcpproxy.SessionID(sid)
		if err := h.cfg.Runtime.Session(r.Context(), caller, reg, id); err != nil {
			// Copied, expired, closed, process-lost and foreign sessions are all
			// the same not-found. A dependency that gave no answer is not a
			// verdict about the session: it is unavailable and may be retried.
			if errors.Is(err, mcpproxy.ErrUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "unavailable")
				return
			}
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		h.sdk.ServeHTTP(sw, r)
		if r.Method == http.MethodDelete && sw.status >= 200 && sw.status < 300 {
			_ = h.cfg.Runtime.CloseSession(context.WithoutCancel(r.Context()), caller, reg, id)
		}
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// Only an initialize request may open a session; peek at it before any
	// remote connection is made.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	var probe struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(body, &probe) != nil || probe.Method != "initialize" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	id, err := h.cfg.Runtime.OpenSession(r.Context(), caller, reg)
	if err != nil {
		fail(w, err)
		return
	}
	ctx := context.WithValue(r.Context(), sessionKey, opening{id: id, reg: reg, c: caller})
	sw := &statusWriter{ResponseWriter: w}
	h.sdk.ServeHTTP(sw, r.WithContext(ctx))
	if sw.status == 0 || sw.status >= 400 {
		h.cfg.Runtime.Abandon(context.WithoutCancel(r.Context()), caller, reg, id)
	}
}

// serverFor builds the SDK server of one new session. Its session ID is the
// proxy-local one; the remote server's own never reaches the workload.
func (h *handler) serverFor(r *http.Request) *mcp.Server {
	o, ok := r.Context().Value(sessionKey).(opening)
	if !ok {
		return nil
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "kodama-mcp-proxy", Version: "1"}, &mcp.ServerOptions{
		SupportedProtocolVersions: []string{ProtocolRevision},
		// Only tools are advertised. Resources, prompts, sampling and
		// elicitation could bypass tool authorization and are not relayed.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		GetSessionID: func() string { return string(o.id) },
	})
	srv.AddReceivingMiddleware(h.guard(o))
	return srv
}

func rpcError(code int64, msg string) error { return &jsonrpc.Error{Code: code, Message: msg} }

// guard routes the only two methods that reach the remote and rejects every
// other method before it can be answered.
func (h *handler) guard(o opening) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch {
			case method == "initialize", method == "ping", strings.HasPrefix(method, "notifications/"):
				return next(ctx, method, req)
			case method == "tools/list":
				return h.list(ctx, o, req)
			case method == "tools/call":
				return h.call(ctx, o, req)
			}
			return nil, rpcError(jsonrpc.CodeMethodNotFound, "method not found")
		}
	}
}

// authorize repeats authentication and the complete caller-tuple check for the
// request being served, using the bearer of that very request.
func (h *handler) authorize(ctx context.Context, o opening, req mcp.Request) (mcpproxy.Caller, error) {
	extra := req.GetExtra()
	if extra == nil {
		return mcpproxy.Caller{}, mcpproxy.ErrUnauthenticated
	}
	tok, err := bearer(extra.Header)
	if err != nil {
		return mcpproxy.Caller{}, err
	}
	c, err := h.cfg.Runtime.Authenticate(ctx, tok, o.c.AttemptID)
	if err != nil {
		return mcpproxy.Caller{}, err
	}
	if sid := req.GetSession().ID(); sid != string(o.id) {
		return mcpproxy.Caller{}, mcpproxy.ErrSessionInvalid
	}
	if err := h.cfg.Runtime.Session(ctx, c, o.reg, o.id); err != nil {
		return mcpproxy.Caller{}, err
	}
	return c, nil
}

func protoError(err error) error {
	switch {
	case errors.Is(err, mcpproxy.ErrUnauthenticated), errors.Is(err, mcpproxy.ErrDenied):
		return rpcError(jsonrpc.CodeInvalidRequest, "not authorized")
	case errors.Is(err, mcpproxy.ErrToolHidden), errors.Is(err, mcpproxy.ErrNotFound), errors.Is(err, mcpproxy.ErrSessionInvalid), errors.Is(err, mcpproxy.ErrBindingInvalid),
		errors.Is(err, mcpproxy.ErrCredential):
		// A session whose credential ended is gone, like any other ended session.
		return rpcError(jsonrpc.CodeInvalidParams, "unknown tool or session")
	case errors.Is(err, mcpproxy.ErrTooLarge), errors.Is(err, mcpproxy.ErrInvalid):
		return rpcError(jsonrpc.CodeInvalidParams, "invalid request")
	}
	return rpcError(jsonrpc.CodeInternalError, "the proxy could not complete the request")
}

func (h *handler) list(ctx context.Context, o opening, req mcp.Request) (mcp.Result, error) {
	c, err := h.authorize(ctx, o, req)
	if err != nil {
		return nil, protoError(err)
	}
	if p, ok := req.GetParams().(*mcp.ListToolsParams); ok && p != nil && p.Cursor != "" {
		// The proxy never issues a cursor: the listing is complete and filtered.
		return nil, rpcError(jsonrpc.CodeInvalidParams, "invalid cursor")
	}
	tools, err := h.cfg.Runtime.Discover(ctx, c, o.reg, o.id)
	if err != nil {
		return nil, protoError(err)
	}
	out := &mcp.ListToolsResult{Tools: make([]*mcp.Tool, 0, len(tools))}
	for _, t := range tools {
		tool := &mcp.Tool{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: rawOrObject(t.InputSchema)}
		if len(t.OutputSchema) > 0 {
			tool.OutputSchema = t.OutputSchema
		}
		out.Tools = append(out.Tools, tool)
	}
	return out, nil
}

func rawOrObject(b json.RawMessage) any {
	if len(b) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

func newRequestID() mcpproxy.RequestID {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return mcpproxy.RequestID("c" + hex.EncodeToString(b[:]))
}

func (h *handler) call(ctx context.Context, o opening, req mcp.Request) (mcp.Result, error) {
	c, err := h.authorize(ctx, o, req)
	if err != nil {
		return nil, protoError(err)
	}
	p, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || p == nil {
		return nil, rpcError(jsonrpc.CodeInvalidParams, "invalid request")
	}
	id := newRequestID()
	res, err := h.cfg.Runtime.Invoke(ctx, c, o.reg, o.id, mcpproxy.InvokeRequest{ID: id, Tool: p.Name, Arguments: p.Arguments})
	if err != nil {
		var oe *mcpproxy.OutcomeError
		switch {
		case errors.As(err, &oe) && errors.Is(err, mcpproxy.ErrOutcomeUnknown):
			return toolError("The tool call's outcome is unknown: the remote effect may or may not have happened. It was not retried.", string(oe.RequestID), "unknown", oe.Code), nil
		case errors.As(err, &oe) && errors.Is(err, mcpproxy.ErrRemoteFailed):
			return toolError("The remote server reported an error for this tool call.", string(oe.RequestID), "failed", oe.Code), nil
		case errors.As(err, &oe) && errors.Is(err, mcpproxy.ErrDuplicate):
			return toolError("This request was already admitted and is not dispatched again.", string(oe.RequestID), string(oe.State), oe.Code), nil
		case errors.Is(err, mcpproxy.ErrLimitReached):
			return toolError("The run's tool call limit was reached.", string(id), "rejected", "limit_reached"), nil
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, err
		}
		return nil, protoError(err)
	}
	var out mcp.CallToolResult
	if err := json.Unmarshal(res.JSON, &out); err != nil {
		return nil, rpcError(jsonrpc.CodeInternalError, "the proxy could not read the remote reply")
	}
	return &out, nil
}

// toolError is the classified, content-free error result the model sees. It
// carries safe correlation metadata and claims nothing about rollback.
func toolError(text, requestID, outcome, code string) *mcp.CallToolResult {
	res := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
	res.Meta = mcp.Meta{"kodama/requestId": requestID, "kodama/outcome": outcome, "kodama/code": code}
	return res
}

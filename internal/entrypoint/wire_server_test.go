package entrypoint

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// wireServer is a minimal runtime API of ONE route family over a source-free
// attempt that runs script. It answers only the routes of its wire (every other
// path is a 404) and records every request path, so a test sees which wire the
// entrypoint spoke. answer makes it reply with the other version's context
// ("v1ctx", "v2ctx") or the version-mismatch error ("mismatch").
type wireServer struct {
	t      *testing.T
	srv    *httptest.Server
	wire   string
	script string
	answer string
	// The contexts are built once: admission and GET /context must agree.
	v1ctx runtimev1.Context
	v2ctx runtimev2.ContextV2

	mu     sync.Mutex
	seen   []string
	result *runtimev1.Result // the last result submitted
}

func newWireServer(t *testing.T, wire, script string) *wireServer {
	t.Helper()
	w := &wireServer{t: t, wire: wire, script: script}
	w.v1ctx, w.v2ctx = unitContext(script), sharedContext(t, script, freeWorkspace())
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *wireServer) URL() string { return w.srv.URL }

func (w *wireServer) paths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

func (w *wireServer) reply(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

func (w *wireServer) fail(rw http.ResponseWriter, status int, code domain.ErrorCode) {
	w.reply(rw, status, runtimev2.ErrorResponse{Error: runtimev2.ErrorBody{Code: code, Message: string(code), RequestID: "req_wire", Details: map[string]any{}}})
}

func (w *wireServer) serve(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	w.seen = append(w.seen, r.URL.Path)
	w.mu.Unlock()
	prefix := "/runtime/" + w.wire + "/attempts/att_unit01"
	route, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		w.fail(rw, http.StatusNotFound, domain.CodeNotFound)
		return
	}
	if w.answer == "mismatch" && (route == "/admit" || route == "/context") {
		w.fail(rw, http.StatusConflict, runtimev2.CodeContextVersionMismatch)
		return
	}
	token := tokenInfo("rt_wire_token_0123456789abcdef", time.Hour, 2*time.Hour)
	v1ctx, v2ctx := w.v1ctx, w.v2ctx
	switch route {
	case "/admit":
		switch {
		case w.answer == "v1ctx" || (w.answer == "" && w.wire == "v1"):
			w.reply(rw, http.StatusOK, runtimev1.AdmitResponse{Token: token, Context: v1ctx})
		default:
			w.reply(rw, http.StatusOK, runtimev2.AdmitResponseV2{Token: token, Context: v2ctx})
		}
	case "/context":
		if w.answer == "v1ctx" || (w.answer == "" && w.wire == "v1") {
			w.reply(rw, http.StatusOK, v1ctx)
		} else {
			w.reply(rw, http.StatusOK, v2ctx)
		}
	case "/token":
		w.reply(rw, http.StatusOK, runtimev1.TokenResponse{Token: token})
	case "/begin":
		var req runtimev2.BeginRequest
		if json.Unmarshal(body, &req) != nil || req.Workspace != nil {
			w.fail(rw, http.StatusBadRequest, domain.CodeInvalidRequest)
			return
		}
		w.reply(rw, http.StatusOK, runtimev2.BeginResponse{AttemptID: "att_unit01", BeganAt: timestamp(0), WorkspaceState: runtimev2.StateNone})
	case "/result":
		var q runtimev1.ResultRequest
		if json.Unmarshal(body, &q) != nil {
			w.fail(rw, http.StatusBadRequest, domain.CodeInvalidRequest)
			return
		}
		w.mu.Lock()
		w.result = &q.Result
		w.mu.Unlock()
		status := runtimev1.AttemptFailed
		if q.Result.Outcome == runtimev1.OutcomeCompleted {
			status = runtimev1.AttemptSucceeded
		}
		w.reply(rw, http.StatusOK, runtimev1.ResultResponse{AttemptID: "att_unit01", ResultHash: q.ResultHash, AttemptStatus: status, AcceptedAt: timestamp(0)})
	default:
		w.fail(rw, http.StatusNotFound, domain.CodeNotFound)
	}
}

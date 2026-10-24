package run_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/root"
	clirun "github.com/optimiware/kodama/internal/cli/run"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
	base    = "/api/v1/tenants/" + tenant + "/projects/" + project
	runID   = "run_0123456789abcdef01234567"
	commit  = "1111111111111111111111111111111111111111"
)

type tokens struct{}

func (tokens) TokenSource(root.EffectiveContext) (client.AccessTokenSource, error) {
	return clienttest.Tokens("tok-1"), nil
}

type result struct {
	code root.ExitCode
	out  string
	err  string
}

// sleeps records backoff delays instead of waiting for them.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeps) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.d)
}

func newRunner(t *testing.T, out, errb *bytes.Buffer, opts clirun.WatchOptions) *root.Runner {
	t.Helper()
	contrib, err := clirun.NewCommands(clirun.Config{Tokens: tokens{}, AllowInsecureLoopback: true, Watch: opts})
	if err != nil {
		t.Fatal(err)
	}
	r, err := root.NewRunner(root.RunnerConfig{Streams: root.Streams{Out: out, Err: errb}, Tokens: tokens{}, AllowInsecureLoopback: true}, contrib)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func runCtx(ctx context.Context, t *testing.T, srv *clienttest.Server, opts clirun.WatchOptions, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	r := newRunner(t, &out, &errb, opts)
	full := append([]string{}, args[:2]...)
	full = append(full, "--factory", srv.URL, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(ctx, full)
	return result{code, out.String(), errb.String()}
}

func runCLI(t *testing.T, srv *clienttest.Server, args ...string) result {
	t.Helper()
	return runCtx(context.Background(), t, srv, clirun.WatchOptions{}, args...)
}

func runDoc(status string, version int) map[string]any {
	return map[string]any{
		"id": runID, "purpose": "work", "workItemId": "wi_1042", "definitionId": "pd_1",
		"definitionDigest": "sha256:" + strings.Repeat("2", 64), "contentRevision": 2, "sourceCommit": commit,
		"status": status, "reason": "created", "cancelRequestedAt": nil, "failureReason": nil,
		"inputSnapshot": map[string]any{"digest": "sha256:" + strings.Repeat("3", 64), "inputs": []any{}, "secrets": []any{}},
		"requestedBy":   map[string]any{"type": "user", "id": "usr_1"}, "sourceRetention": "confirmed", "version": version,
		"createdAt": "2026-10-19T10:05:00Z", "startedAt": nil, "finishedAt": nil,
	}
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not JSON: %v: %q", err, s)
	}
	return m
}

func errorOf(t *testing.T, s string) map[string]any {
	t.Helper()
	e, _ := decodeJSON(t, s)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error envelope: %q", s)
	}
	return e
}

// sseWriter writes frames to a streaming response.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func startSSE(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	f.Flush()
	return &sseWriter{w, f}
}

func (s *sseWriter) raw(text string) {
	_, _ = s.w.Write([]byte(text))
	s.f.Flush()
}

func (s *sseWriter) frame(event, id string, data any) {
	b, _ := json.Marshal(data)
	var sb strings.Builder
	fmt.Fprintf(&sb, "event: %s\n", event)
	if id != "" {
		fmt.Fprintf(&sb, "id: %s\n", id)
	}
	fmt.Fprintf(&sb, "data: %s\n\n", b)
	s.raw(sb.String())
}

func (s *sseWriter) snapshot(cursor, state string, version int) {
	s.frame("snapshot", cursor, map[string]any{"cursor": cursor, "resources": []any{
		map[string]any{"type": "run", "id": runID, "version": version, "state": state},
	}})
}

var eventSeq int

func runEvent(id, to string, version int) map[string]any {
	return map[string]any{
		"id": id, "schemaVersion": 1, "type": "run.state_changed", "occurredAt": "2026-10-19T10:05:00Z",
		"tenantId": tenant, "projectId": project, "resource": map[string]any{"type": "run", "id": runID, "version": version},
		"actor": map[string]any{"type": "service", "id": "svc"}, "correlationId": "req_1", "causationId": nil,
		"data": map[string]any{"from": "running", "to": to, "reason": "x"},
	}
}

func (s *sseWriter) runEvent(id, cursor, to string, version int) {
	s.frame("run.state_changed", cursor, runEvent(id, to, version))
}

func (s *sseWriter) heartbeat() { s.raw(": heartbeat\n\n") }

// factory is a scriptable fake of the run, event and snapshot routes.
type factory struct {
	t  *testing.T
	mu sync.Mutex
	// runGet answers GET /runs/{id}; calls counts reads.
	runGet  func(n int) (status int, body any)
	runGets int
	// events handles one stream connection (n counts from 1).
	events func(n int, w http.ResponseWriter, r *http.Request)
	// snapshot handles the application/json representation.
	snapshot func(w http.ResponseWriter, r *http.Request)

	conns   int
	lastIDs []string
	snaps   int
	accepts []string
}

func (f *factory) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == base+"/runs/"+runID+"/events":
			f.mu.Lock()
			acc := r.Header.Get("Accept")
			f.accepts = append(f.accepts, acc)
			if acc == "application/json" {
				f.snaps++
				f.mu.Unlock()
				f.snapshot(w, r)
				return
			}
			f.conns++
			n := f.conns
			f.lastIDs = append(f.lastIDs, r.Header.Get("Last-Event-ID"))
			f.mu.Unlock()
			f.events(n, w, r)
		case r.URL.Path == base+"/runs/"+runID && r.Method == "GET":
			f.mu.Lock()
			f.runGets++
			n := f.runGets
			f.mu.Unlock()
			status, body := f.runGet(n)
			clienttest.WriteJSON(w, status, body, nil)
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func (f *factory) connections() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns, append([]string(nil), f.lastIDs...)
}

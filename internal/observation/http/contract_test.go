package observationhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/observation"
	obshttp "github.com/optimiware/kodama/internal/observation/http"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
)

const (
	observationYAML = "../../../api/observation.yaml"
	commonYAML      = "../../../api/common.yaml"

	tenant, project = "t_acme", "p_inventory"
)

// --- OpenAPI loading (same approach as the identity contract test) ---

func loadYAML(t testing.TB, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func translate(n any) {
	switch v := n.(type) {
	case map[string]any:
		if nb, _ := v["nullable"].(bool); nb {
			if ty, ok := v["type"].(string); ok {
				v["type"] = []any{ty, "null"}
			}
		}
		delete(v, "nullable")
		for _, c := range v {
			translate(c)
		}
	case []any:
		for _, c := range v {
			translate(c)
		}
	}
}

type spec struct {
	raw map[string]any
	c   *jsonschema.Compiler
}

func loadSpec(t testing.TB) *spec {
	t.Helper()
	c := jsonschema.NewCompiler()
	var out map[string]any
	for name, path := range map[string]string{"common.yaml": commonYAML, "observation.yaml": observationYAML} {
		raw := loadYAML(t, path)
		translate(raw)
		js, _ := json.Marshal(raw)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(name, doc); err != nil {
			t.Fatal(err)
		}
		if name == "observation.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("observation.yaml#/components/schemas/" + schema)
	if err != nil {
		t.Fatalf("schema %s: %v", schema, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return sch.Validate(v)
}

func (s *spec) validateCommon(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("common.yaml#/components/schemas/" + schema)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return sch.Validate(v)
}

func dig(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

func strictDecode(doc []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// --- harness ---

type allowAll struct{}

func (allowAll) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

type bearer struct{}

func (bearer) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	_, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || tok == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

type rig struct {
	log   *ot.MemLog
	state *ot.MemState
	srv   *httptest.Server
	h     http.Handler
	codec *httpapi.CursorCodec
	opts  obshttp.HandlerOptions
}

func newRig(t *testing.T, access ot.AccessFunc) *rig { return newRigWith(t, nil, access) }

func newRigWith(t *testing.T, mutate func(*obshttp.HandlerOptions), accessFns ...ot.AccessFunc) *rig {
	t.Helper()
	var access ot.AccessFunc
	if len(accessFns) > 0 {
		access = accessFns[0]
	}
	r := &rig{log: &ot.MemLog{}, state: &ot.MemState{}}
	if access == nil {
		access = func(context.Context, observation.Actor, observation.Scope, observation.Family) error { return nil }
	}
	svc, err := observation.NewService(observation.Config{Log: r.log, State: r.state, Access: access, Options: observation.Options{
		PageSize: 3, MaxPageBytes: 1 << 20, PollInterval: 5 * time.Millisecond, HeartbeatInterval: 40 * time.Millisecond,
		AccessInterval: 20 * time.Millisecond, MaxSnapshotResources: 50,
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.codec, err = httpapi.NewCursorCodec(ot.CursorKey)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: bearer{}, Authorizer: allowAll{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	r.opts = obshttp.HandlerOptions{WriteTimeout: 300 * time.Millisecond, MaxBufferedBytes: 4096}
	if mutate != nil {
		mutate(&r.opts)
	}
	h, err := obshttp.NewHandler(svc, r.codec, r.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	r.h = rt
	r.srv = httptest.NewServer(rt)
	t.Cleanup(func() { r.srv.CloseClientConnections(); r.srv.Close() })
	return r
}

func (r *rig) projectURL() string { return ot.ProjectURL(r.srv.URL, tenant, project) }
func (r *rig) runURL(run string) string {
	return ot.RunURL(r.srv.URL, tenant, project, run)
}

func workEv(id string) eventsv1.Envelope {
	return ot.WorkEvent(tenant, project, id, "wi_"+id, 1)
}

// --- tests ---

// Test_http_contract: the published OpenAPI examples, the generated DTOs, the
// SSE framing, the header validation, the error envelopes, the unsupported
// streaming writers and cursor tampering agree. No external services.
func Test_http_contract(t *testing.T) {
	sp := loadSpec(t)

	t.Run("published examples validate against their schemas and generated types", func(t *testing.T) {
		resp := dig(sp.raw, "components", "responses", "Observation", "content")
		snap, _ := json.Marshal(dig(resp, "application/json", "examples", "projectSnapshot", "value"))
		if err := sp.validate(t, "Snapshot", snap); err != nil {
			t.Fatalf("snapshot example: %v", err)
		}
		var dto obshttp.Snapshot
		if err := strictDecode(snap, &dto); err != nil || len(dto.Resources) != 3 {
			t.Fatalf("generated Snapshot cannot decode the example: %v", err)
		}
		cur := dig(resp, "text/event-stream", "examples", "snapshotThenEvents", "value").(string)
		frames := ot.ParseFrames(cur)
		if len(frames) != 4 {
			t.Fatalf("stream example has %d frames", len(frames))
		}
		for _, f := range frames {
			switch f.Event {
			case "snapshot":
				if err := sp.validate(t, "Snapshot", []byte(f.Data)); err != nil {
					t.Errorf("snapshot frame: %v", err)
				}
				if err := strictDecode([]byte(f.Data), new(obshttp.Snapshot)); err != nil {
					t.Error(err)
				}
			case "checkpoint":
				if err := sp.validate(t, "Checkpoint", []byte(f.Data)); err != nil {
					t.Errorf("checkpoint frame: %v", err)
				}
				if err := strictDecode([]byte(f.Data), new(obshttp.Checkpoint)); err != nil {
					t.Error(err)
				}
			case "":
				if f.Comment != "heartbeat" {
					t.Errorf("unexpected comment frame %+v", f)
				}
			default:
				if err := sp.validate(t, "ObservedEvent", []byte(f.Data)); err != nil {
					t.Errorf("event frame %s: %v", f.Event, err)
				}
				if err := strictDecode([]byte(f.Data), new(obshttp.ObservedEvent)); err != nil {
					t.Errorf("generated ObservedEvent: %v", err)
				}
				if _, err := eventsv1.Decode([]byte(f.Data)); err != nil {
					t.Errorf("the example event is not a valid catalog envelope: %v", err)
				}
			}
		}
		resync := dig(resp, "text/event-stream", "examples", "resyncRequired", "value").(string)
		rf := ot.ParseFrames(resync)
		if len(rf) != 1 || rf[0].Event != "resync_required" || rf[0].ID != "" {
			t.Fatalf("resync example = %+v", rf)
		}
		if err := sp.validate(t, "ResyncRequired", []byte(rf[0].Data)); err != nil {
			t.Fatal(err)
		}
		gone, _ := json.Marshal(dig(sp.raw, "components", "responses", "CursorExpired", "content", "application/json", "example"))
		if err := sp.validateCommon(t, "ErrorEnvelope", gone); err != nil {
			t.Fatalf("410 example: %v", err)
		}
	})

	t.Run("documented routes are exactly the registered routes", func(t *testing.T) {
		var documented []string
		for path, item := range sp.raw["paths"].(map[string]any) {
			for method := range item.(map[string]any) {
				if method == "get" {
					documented = append(documented, "GET /api/v1"+path)
				}
			}
		}
		sort.Strings(documented)
		want := []string{
			"GET /api/v1/tenants/{tenantId}/projects/{projectId}/events",
			"GET /api/v1/tenants/{tenantId}/projects/{projectId}/runs/{id}/events",
		}
		if fmt.Sprint(documented) != fmt.Sprint(want) {
			t.Fatalf("documented %v, want %v", documented, want)
		}
		r := newRig(t, nil)
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req, _ := http.NewRequest(method, r.projectURL(), nil)
			req.Header.Set("Authorization", "Bearer usr_1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s answered %d", method, resp.StatusCode)
			}
		}
	})

	t.Run("documented event types are exactly the observable projection", func(t *testing.T) {
		var spec []string
		for _, v := range dig(sp.raw, "components", "schemas", "ObservableEventType", "enum").([]any) {
			spec = append(spec, v.(string))
		}
		sort.Strings(spec)
		if fmt.Sprint(spec) != fmt.Sprint(observation.SupportedTypes()) {
			t.Fatalf("spec %v != projection %v", spec, observation.SupportedTypes())
		}
	})

	t.Run("SSE framing, headers and cursors", func(t *testing.T) {
		r := newRig(t, nil)
		r.state.Position = 0
		r.state.Resources = []observation.Resource{{Type: observation.FamilyWork, ID: "wi_1", Version: 1, State: "backlog", Kind: "story"}}
		r.log.Add(workEv("e1"))
		r.log.Add(workEv("e2"))
		s := ot.Connect(t, r.projectURL(), "usr_1", nil)
		if s.Resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", s.Resp.StatusCode, s.Body)
		}
		h := s.Resp.Header
		if !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") || h.Get("Cache-Control") != "no-store" ||
			h.Get("X-Accel-Buffering") != "no" || !strings.HasPrefix(h.Get("X-Request-ID"), "req_") {
			t.Fatalf("headers = %v", h)
		}
		frames := s.NextN(t, 3, 2*time.Second)
		if frames[0].Event != "snapshot" || frames[1].Event != eventsv1.TypeWorkItemTransitioned || frames[2].Event != eventsv1.TypeWorkItemTransitioned {
			t.Fatalf("frames = %+v", frames)
		}
		if err := sp.validate(t, "Snapshot", []byte(frames[0].Data)); err != nil {
			t.Fatalf("snapshot frame: %v", err)
		}
		var snap obshttp.Snapshot
		if err := strictDecode([]byte(frames[0].Data), &snap); err != nil || snap.Cursor != frames[0].ID {
			t.Fatalf("snapshot frame id %q vs cursor %q (%v)", frames[0].ID, snap.Cursor, err)
		}
		for _, f := range frames[1:] {
			if err := sp.validate(t, "ObservedEvent", []byte(f.Data)); err != nil {
				t.Errorf("event frame: %v", err)
			}
			if _, err := eventsv1.Decode([]byte(f.Data)); err != nil {
				t.Errorf("event frame is not a catalog envelope: %v", err)
			}
			if f.ID == "" || f.ID == frames[0].ID {
				t.Errorf("event frames must carry their own cursor: %+v", f)
			}
		}
		// Idle: heartbeats arrive.
		sawHeartbeat := false
		deadline := time.After(2 * time.Second)
		for !sawHeartbeat {
			select {
			case f := <-s.Frames:
				sawHeartbeat = f.IsHeartbeat()
			case <-deadline:
				t.Fatal("no heartbeat on an idle stream")
			}
		}

		// Resume exactly after the first event with its id.
		s2 := ot.Connect(t, r.projectURL(), "usr_1", map[string]string{"Last-Event-ID": frames[1].ID})
		got := s2.NextN(t, 1, 2*time.Second)
		if got[0].Event != eventsv1.TypeWorkItemTransitioned || got[0].ID != frames[2].ID {
			t.Fatalf("resumed with %+v, want the second event %+v", got[0], frames[2])
		}
		// No snapshot is emitted on resume.
		if f, ok := s2.Next(150 * time.Millisecond); ok {
			t.Fatalf("unexpected frame after the resume: %+v", f)
		}
	})

	t.Run("JSON snapshot carries a usable cursor", func(t *testing.T) {
		r := newRig(t, nil)
		r.state.Position = 2
		r.state.Resources = []observation.Resource{
			{Type: observation.FamilyRun, ID: "run_1", Version: 3, State: "running"},
			{Type: observation.FamilyStepAttempt, ID: "att_1", Version: 2, State: "running", RunID: "run_1", StepID: "build"},
		}
		r.log.Add(workEv("e1"))
		r.log.Add(workEv("e2"))
		r.log.Add(workEv("e3"))
		code, hdr, body := ot.GetJSON(t, r.projectURL(), "usr_1", nil)
		if code != 200 || hdr.Get("Cache-Control") != "no-store" || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
			t.Fatalf("status %d headers %v: %s", code, hdr, body)
		}
		if err := sp.validate(t, "Snapshot", body); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		var snap obshttp.Snapshot
		if err := strictDecode(body, &snap); err != nil {
			t.Fatal(err)
		}
		if snap.Resources[1].RunId == nil || *snap.Resources[1].RunId != "run_1" {
			t.Fatalf("resources = %+v", snap.Resources)
		}
		s := ot.Connect(t, r.projectURL(), "usr_1", map[string]string{"Last-Event-ID": snap.Cursor})
		fr := s.NextN(t, 1, 2*time.Second)
		if !strings.HasPrefix(fr[0].ID, "") || fr[0].Event != eventsv1.TypeWorkItemTransitioned {
			t.Fatalf("frames = %+v", fr)
		}
		var env eventsv1.Envelope
		if err := json.Unmarshal([]byte(fr[0].Data), &env); err != nil || env.ID != "e3" {
			t.Fatalf("the snapshot cursor must continue with the third event, got %q (%v)", env.ID, err)
		}
	})

	t.Run("request validation", func(t *testing.T) {
		r := newRig(t, nil)
		r.log.Add(workEv("e1"))
		good := r.projectURL()
		cur := mintCursor(t, r, "", nil, 1)
		cases := []struct {
			name    string
			url     string
			headers map[string]string
			status  int
			code    string
		}{
			{"unknown query member", good + "?limit=5", nil, 400, "invalid_request"},
			{"cursor query member is not a resume input", good + "?cursor=" + cur, nil, 400, "invalid_request"},
			{"unsupported event type", good + "?type=gate.requested", nil, 400, "invalid_request"},
			{"empty event type", good + "?type=", nil, 400, "invalid_request"},
			{"malformed query", good + "?type=%zz", nil, 400, "invalid_request"},
			{"unacceptable Accept", good, map[string]string{"Accept": "text/html"}, 400, "invalid_request"},
			{"malformed cursor", good, map[string]string{"Last-Event-ID": "not-a-cursor"}, 400, "invalid_cursor"},
			{"empty cursor", good, map[string]string{"Last-Event-ID": " "}, 400, "invalid_cursor"},
			{"cursor beyond the head", good, map[string]string{"Last-Event-ID": mintCursor(t, r, "", nil, 99)}, 400, "invalid_cursor"},
			{"cursor of another run", r.runURL("run_1"), map[string]string{"Last-Event-ID": cur}, 400, "invalid_cursor"},
			{"cursor with other filters", good + "?type=run.created", map[string]string{"Last-Event-ID": cur}, 400, "invalid_cursor"},
			{"cursor for another endpoint", r.runURL("run_1"), map[string]string{"Last-Event-ID": mintCursor(t, r, "run_2", nil, 1)}, 400, "invalid_cursor"},
		}
		for _, c := range cases {
			s := ot.Connect(t, c.url, "usr_1", c.headers)
			if s.Resp.StatusCode != c.status || ot.ErrorCode(s.Body) != c.code {
				t.Errorf("%s: %d %s, want %d %s", c.name, s.Resp.StatusCode, s.Body, c.status, c.code)
				continue
			}
			var env httpapi.ErrorEnvelope
			if err := strictDecode(s.Body, &env); err != nil {
				t.Errorf("%s: not the common error envelope: %v", c.name, err)
			}
			if err := sp.validateCommon(t, "ErrorEnvelope", s.Body); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
		}
		// The snapshot representation does not resume.
		code, _, body := ot.GetJSON(t, good, "usr_1", map[string]string{"Last-Event-ID": cur})
		if code != 400 || ot.ErrorCode(body) != "invalid_request" {
			t.Errorf("JSON + Last-Event-ID: %d %s", code, body)
		}
		// Two resume values are ambiguous.
		req, _ := http.NewRequest(http.MethodGet, good, nil)
		req.Header.Set("Authorization", "Bearer usr_1")
		req.Header.Add("Last-Event-ID", cur)
		req.Header.Add("Last-Event-ID", cur)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || ot.ErrorCode(b) != "invalid_cursor" {
			t.Errorf("two Last-Event-ID headers: %d %s", resp.StatusCode, b)
		}
		// Credentials are required.
		if code, _, _ := ot.GetJSON(t, good, "", nil); code != 401 {
			t.Errorf("unauthenticated: %d", code)
		}
	})

	t.Run("cursor tampering is generic", func(t *testing.T) {
		r := newRig(t, nil)
		r.log.Add(workEv("e1"))
		cur := mintCursor(t, r, "", nil, 1)
		body, sig, _ := strings.Cut(cur, ".")
		for name, tampered := range map[string]string{
			"flipped signature": body + "." + flip(sig),
			"flipped payload":   flip(body) + "." + sig,
			"truncated":         cur[:len(cur)-3],
			"extra segment":     cur + ".x",
		} {
			s := ot.Connect(t, r.projectURL(), "usr_1", map[string]string{"Last-Event-ID": tampered})
			if s.Resp.StatusCode != 400 || ot.ErrorCode(s.Body) != "invalid_cursor" {
				t.Errorf("%s: %d %s", name, s.Resp.StatusCode, s.Body)
			}
		}
	})

	t.Run("error envelopes before the response is committed", func(t *testing.T) {
		boom := errors.New("secret database detail")
		cases := []struct {
			name   string
			setup  func(r *rig)
			resume bool
			status int
			code   string
		}{
			{"hidden run", func(r *rig) { r.state.VerifyErr = fmt.Errorf("%w: x", observation.ErrNotFound) }, true, 404, "not_found"},
			{"cursor expired", func(r *rig) {
				r.log.Add(workEv("a"))
				r.log.Add(workEv("b"))
				r.log.Prune(2)
			}, true, 410, "cursor_expired"},
			{"snapshot too large", func(r *rig) { r.state.SnapshotErr = observation.ErrSnapshotTooLarge }, false, 503, "snapshot_too_large"},
			{"binding unavailable", func(r *rig) { r.state.SnapshotErr = observation.Unavailablef(boom, "binding") }, false, 503, "unavailable"},
			{"log unavailable", func(r *rig) { r.log.Fail = observation.Unavailablef(boom, "db") }, true, 503, "unavailable"},
		}
		for _, c := range cases {
			r := newRig(t, nil)
			c.setup(r)
			var hdr map[string]string
			if c.resume {
				hdr = map[string]string{"Last-Event-ID": mintCursor(t, r, "", nil, 0)}
			}
			s := ot.Connect(t, r.projectURL(), "usr_1", hdr)
			if s.Resp.StatusCode != c.status || ot.ErrorCode(s.Body) != c.code {
				t.Errorf("%s: %d %s, want %d %s", c.name, s.Resp.StatusCode, s.Body, c.status, c.code)
			}
			if strings.Contains(string(s.Body), "secret") {
				t.Errorf("%s leaked internals: %s", c.name, s.Body)
			}
			if err := sp.validateCommon(t, "ErrorEnvelope", s.Body); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			if c.status == 503 && s.Resp.Header.Get("Retry-After") == "" && c.code == "unavailable" {
				t.Errorf("%s: 503 unavailable needs Retry-After", c.name)
			}
		}
		// The same expiry through the JSON snapshot path is not an error: it
		// issues a fresh cursor.
		r := newRig(t, nil)
		r.log.Add(workEv("a"))
		r.log.Add(workEv("b"))
		r.log.Prune(2)
		r.state.Position = 2
		code, _, body := ot.GetJSON(t, r.projectURL(), "usr_1", nil)
		if code != 200 {
			t.Fatalf("snapshot after expiry: %d %s", code, body)
		}
	})

	t.Run("denied and hidden callers", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err    error
			status int
			code   string
		}{
			"denied": {observation.ErrDenied, 403, "forbidden"},
			"hidden": {observation.ErrHidden, 404, "not_found"},
			"outage": {errors.New("authority down"), 503, "unavailable"},
		} {
			r := newRig(t, func(context.Context, observation.Actor, observation.Scope, observation.Family) error { return tc.err })
			s := ot.Connect(t, r.projectURL(), "usr_1", nil)
			if s.Resp.StatusCode != tc.status || ot.ErrorCode(s.Body) != tc.code {
				t.Errorf("%s: %d %s", name, s.Resp.StatusCode, s.Body)
			}
		}
	})

	t.Run("resync frame is the last frame and has no id", func(t *testing.T) {
		r := newRig(t, nil)
		for i := 0; i < 8; i++ {
			r.log.Add(workEv(fmt.Sprint("e", i)))
		}
		r.log.BeforeRead = func(n int) {
			if n == 2 {
				r.log.Prune(7) // retention overtakes the stream between its first and second page
			}
		}
		s := ot.Connect(t, r.projectURL(), "usr_1", map[string]string{"Last-Event-ID": mintCursor(t, r, "", nil, 0)})
		var last ot.Frame
		for {
			f, ok := s.Next(2 * time.Second)
			if !ok {
				break
			}
			last = f
		}
		if last.Event != "resync_required" || last.ID != "" {
			t.Fatalf("last frame = %+v", last)
		}
		if err := sp.validate(t, "ResyncRequired", []byte(last.Data)); err != nil {
			t.Fatal(err)
		}
	})
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

// mintCursor issues a cursor the way the handler does for the project stream, or
// for the run stream when run is set. Position encoding is part of the contract
// under test: the codec's module-owned position is version 1 plus the big-endian
// sequence.
func mintCursor(t *testing.T, r *rig, run string, types []string, seq int64) string {
	t.Helper()
	scope, err := observation.NewScope(tenant, project, runID(run), types)
	if err != nil {
		t.Fatal(err)
	}
	pattern := "GET /api/v1/tenants/{tenantId}/projects/{projectId}/events"
	values := []string{tenant, project}
	if run != "" {
		pattern = "GET /api/v1/tenants/{tenantId}/projects/{projectId}/runs/{id}/events"
		values = append(values, run)
	}
	pos := make([]byte, 9)
	pos[0] = 1
	for i := 0; i < 8; i++ {
		pos[8-i] = byte(seq >> (8 * i))
	}
	tok, err := r.codec.Encode(httpapi.CursorScope{
		TenantID: tenant, ProjectID: project, Endpoint: pattern, PathValues: values,
		Filters: scope.Filters(), Sort: "sequence",
	}, pos)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// --- unsupported and failing response writers ---

func runID(s string) domain.RunID { return domain.RunID(s) }

// deadlineOnly supports write deadlines but cannot flush.
type deadlineOnly struct{ http.ResponseWriter }

func (deadlineOnly) SetWriteDeadline(time.Time) error { return nil }

// stalledWriter accepts the head and a little data, then behaves like a socket
// whose peer stopped reading: writes fail with the deadline error.
type stalledWriter struct {
	hdr     http.Header
	status  int
	written int
	limit   int
}

func (w *stalledWriter) Header() http.Header              { return w.hdr }
func (w *stalledWriter) WriteHeader(code int)             { w.status = code }
func (w *stalledWriter) Flush()                           {}
func (w *stalledWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *stalledWriter) Write(b []byte) (int, error) {
	if w.written+len(b) > w.limit {
		return 0, os.ErrDeadlineExceeded
	}
	w.written += len(b)
	return len(b), nil
}

func serveDirect(t *testing.T, r *rig, w http.ResponseWriter, resume string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, strings.TrimPrefix(r.projectURL(), r.srv.URL), nil)
	req.Header.Set("Authorization", "Bearer usr_1")
	if resume != "" {
		req.Header.Set("Last-Event-ID", resume)
	}
	r.h.ServeHTTP(w, req)
}

func Test_http_contract_writers(t *testing.T) {
	t.Run("a writer without deadlines is refused before committing", func(t *testing.T) {
		r := newRig(t, nil)
		rec := httptest.NewRecorder()
		serveDirect(t, r, rec, "")
		if rec.Code != 500 || ot.ErrorCode(rec.Body.Bytes()) != "internal_error" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
			t.Fatal("a refused stream must not declare an event stream")
		}
	})
	t.Run("a writer without flush is refused before committing", func(t *testing.T) {
		r := newRig(t, nil)
		rec := httptest.NewRecorder()
		serveDirect(t, r, deadlineOnly{rec}, "")
		if rec.Code != 500 || ot.ErrorCode(rec.Body.Bytes()) != "internal_error" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a stalled writer ends the stream as a slow client", func(t *testing.T) {
		var logs bytes.Buffer
		r := newRigWith(t, func(o *obshttp.HandlerOptions) { o.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
		for i := 0; i < 20; i++ {
			r.log.Add(workEv(fmt.Sprint("slow", i)))
		}
		w := &stalledWriter{hdr: http.Header{}, limit: 600}
		done := make(chan struct{})
		go func() {
			serveDirect(t, r, w, mintCursor(t, r, "", nil, 0))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("the handler did not release a stalled writer")
		}
		if w.status != 200 || !strings.Contains(logs.String(), "reason=slow_client") {
			t.Fatalf("status %d logs %q", w.status, logs.String())
		}
	})
}

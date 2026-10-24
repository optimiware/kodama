package entrypoint

// Retained-log upload: bounded/redacted bytes, the completeness sidecar bound
// to the acknowledged artifact, replay after lost responses, and interruption.
// Component tests only: the runtime API and object store are test doubles, so
// they establish nothing about real reservation/completion idempotency (T-064).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

const (
	logSecret    = "pw-9f8e7d6c5b4a-sECRET"
	logSecretEnv = "db-pass"
)

// secretUnit is a unit whose command can see one env secret, the runtime token
// file and the admission credential file.
func secretUnit(t *testing.T, script string) *unit {
	t.Helper()
	u := newUnit(t, "")
	script = strings.NewReplacer("@TOKEN@", u.cfg.TokenFile, "@ADMISSION@", u.cfg.AdmissionCredentialFile).Replace(script)
	u.rt.ctx = unitContext(script)
	b := secretBinding(logSecretEnv, "secret://project/db-pass", runtimev1.DeliveryEnv)
	u.rt.ctx.Secrets = []runtimev1.SecretBinding{b}
	u.rt.resolve = func(r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
		return runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
			ValueBase64: base64.StdEncoding.EncodeToString([]byte(logSecret))}, nil
	}
	return u
}

// reservedID is the ID the default fake runtime gave the nth reservation.
func reservedID(n int) domain.ArtifactID { return domain.ArtifactID(fmt.Sprintf("art_unit%03d", n+1)) }

// diagnostic returns the bytes the fake object store holds for the artifact
// reserved under name, and the reservation request.
func (u *unit) diagnostic(name string) ([]byte, runtimev1.ArtifactReserveRequest, domain.ArtifactID, bool) {
	u.rt.mu.Lock()
	defer u.rt.mu.Unlock()
	for i, q := range u.rt.reserved {
		if q.Name == name {
			return u.up.object(reservedID(i)), q, reservedID(i), true
		}
	}
	return nil, runtimev1.ArtifactReserveRequest{}, "", false
}

func sidecarOf(t *testing.T, u *unit) (contract.Metadata, domain.ArtifactID) {
	t.Helper()
	data, _, _, ok := u.diagnostic("command.log.meta.json")
	if !ok {
		t.Fatalf("no sidecar was uploaded; calls %v", u.rt.calls)
	}
	m, err := contract.Decode(data)
	if err != nil {
		t.Fatalf("sidecar does not decode: %v\n%s", err, data)
	}
	return m, ""
}

// wantBoundSidecar proves the sidecar binds the uploaded command.log by ID,
// digest and size, and returns it.
func wantBoundSidecar(t *testing.T, u *unit, c contract.Completeness, reasons ...contract.Reason) contract.Metadata {
	t.Helper()
	m, _ := sidecarOf(t, u)
	logBytes, req, id, ok := u.diagnostic("command.log")
	if !ok {
		t.Fatal("sidecar without a command.log")
	}
	if got := digestOfBytes(logBytes); got != req.Digest || int64(len(logBytes)) != *req.SizeBytes {
		t.Fatalf("declared digest/size %s/%d do not match the stored bytes %s/%d", req.Digest, *req.SizeBytes, got, len(logBytes))
	}
	if err := m.Bind(id, string(req.Digest), int64(len(logBytes))); err != nil {
		t.Fatalf("sidecar does not bind the stored log: %v", err)
	}
	if m.Source != contract.SourceWrapper || m.LogName != "command.log" || m.Completeness != c {
		t.Fatalf("sidecar = %+v, want wrapper/command.log/%s", m, c)
	}
	if got := append([]contract.Reason{}, m.Reasons...); fmt.Sprint(got) != fmt.Sprint(append([]contract.Reason{}, reasons...)) {
		t.Fatalf("reasons = %v, want %v", got, reasons)
	}
	return m
}

func indexOf(calls []string, op string, from int) int {
	for i := from; i < len(calls); i++ {
		if calls[i] == op {
			return i
		}
	}
	return -1
}

func Test_wrapper_log_upload(t *testing.T) {
	split := `p="$DB_PASS"; head="${p%%????}"; tail="${p#"$head"}"`
	t.Run("bounded redacted bytes, bound sidecar, then the result", func(t *testing.T) {
		u := secretUnit(t, split+`
printf 'start %s' "$head"; sleep 0.1; printf '%s end\n' "$tail"
echo "token=$(cat @TOKEN@) admission=$(cat @ADMISSION@)"
echo err-line >&2
echo "plain output"`)
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		if r.Outcome != runtimev1.OutcomeCompleted || len(r.DiagnosticArtifactIDs) != 2 {
			t.Fatalf("result = %+v\n%s", r, u.logs.String())
		}
		logBytes, _, id, _ := u.diagnostic("command.log")
		got := string(logBytes)
		for _, leak := range []string{logSecret, "SECRET", "rt_unit_token", unitAdmission} {
			if strings.Contains(got, leak) {
				t.Fatalf("retained log leaks %q:\n%s", leak, got)
			}
		}
		for _, want := range []string{"start [REDACTED] end", "token=[REDACTED] admission=[REDACTED]", "err-line", "plain output"} {
			if !strings.Contains(got, want) {
				t.Fatalf("retained log lacks %q:\n%s", want, got)
			}
		}
		wantBoundSidecar(t, u, contract.Complete)
		if r.DiagnosticArtifactIDs[0] != id {
			t.Fatalf("diagnostics = %v, command.log is %s", r.DiagnosticArtifactIDs, id)
		}
		// reserve/complete of the log, then of the sidecar, then the result.
		want := []string{"reserveArtifact", "completeArtifact", "reserveArtifact", "completeArtifact", "submitResult"}
		if got := u.rt.calls[indexOf(u.rt.calls, "reserveArtifact", 0):]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("call order = %v, want %v", got, want)
		}
		if q := u.rt.reserved[1]; q.Name != "command.log.meta.json" || q.Role != runtimev1.RoleDiagnostic || *q.MediaType != contract.MediaType {
			t.Fatalf("sidecar reservation = %+v", q)
		}
		if entries, _ := os.ReadDir(u.cfg.ScratchDir); len(entries) != 0 {
			t.Fatalf("scratch files left behind: %v", entries)
		}
	})
	t.Run("a failed command is retained completely and says so", func(t *testing.T) {
		u := secretUnit(t, `echo "boom $DB_PASS"; exit 3`)
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		wantReason(t, r, ReasonExitNonzero)
		if len(r.DiagnosticArtifactIDs) != 3 {
			t.Fatalf("diagnostics = %v", r.DiagnosticArtifactIDs)
		}
		m := wantBoundSidecar(t, u, contract.Complete) // completeness is independent of the outcome
		_, _, logID, _ := u.diagnostic("command.log")
		_, _, sideID, _ := u.diagnostic("command.log.meta.json")
		_, _, notesID, _ := u.diagnostic("entrypoint.log")
		if fmt.Sprint(r.DiagnosticArtifactIDs) != fmt.Sprint([]domain.ArtifactID{logID, sideID, notesID}) || m.Log.ArtifactID != logID {
			t.Fatalf("diagnostics = %v", r.DiagnosticArtifactIDs)
		}
		notes, _, _, _ := u.diagnostic("entrypoint.log")
		if !strings.Contains(string(notes), "status 3") || strings.Contains(string(notes), logSecret) {
			t.Fatalf("notes = %q", notes)
		}
		if b, _, _, _ := u.diagnostic("command.log"); strings.Contains(string(b), logSecret) || !strings.Contains(string(b), "boom [REDACTED]") {
			t.Fatalf("log = %q", b)
		}
	})
	t.Run("a runaway command stays within the aggregate diagnostic budget", func(t *testing.T) {
		u := secretUnit(t, `head -c 300000 /dev/zero | tr '\0' 'z'; echo; exit 1`)
		u.rt.ctx.Limits.MaxDiagnosticBytes = 4096
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		var total int64
		for _, q := range u.rt.reserved {
			total += *q.SizeBytes
		}
		if total > 4096 {
			t.Fatalf("diagnostics total %d bytes, budget 4096: %+v", total, u.rt.reserved)
		}
		wantBoundSidecar(t, u, contract.Partial, contract.ReasonTruncated)
		if b, _, _, _ := u.diagnostic("command.log"); !strings.HasSuffix(string(b), truncationMarker) {
			t.Fatalf("a truncated log carries no marker")
		}
		if len(r.DiagnosticArtifactIDs) < 2 {
			t.Fatalf("diagnostics = %v", r.DiagnosticArtifactIDs)
		}
	})
	t.Run("a budget too small for the sidecar leaves completeness unknown", func(t *testing.T) {
		u := secretUnit(t, `head -c 5000 /dev/zero | tr '\0' 'q'`)
		u.rt.ctx.Limits.MaxDiagnosticBytes = 100
		u.wantExit(u.run(), ExitOK)
		var total int64
		for _, q := range u.rt.reserved {
			total += *q.SizeBytes
		}
		if total > 100 {
			t.Fatalf("diagnostics total %d bytes, budget 100", total)
		}
		if _, _, _, ok := u.diagnostic("command.log.meta.json"); ok {
			t.Fatal("a sidecar was uploaded beyond the budget")
		}
		if len(u.lastResult().DiagnosticArtifactIDs) != 1 {
			t.Fatalf("diagnostics = %v", u.lastResult().DiagnosticArtifactIDs)
		}
	})
	t.Run("empty output is observed and declared complete when it fits", func(t *testing.T) {
		u := secretUnit(t, `exit 0`)
		u.wantExit(u.run(), ExitOK)
		if b, req, _, ok := u.diagnostic("command.log"); !ok || len(b) != 0 || *req.SizeBytes != 0 {
			t.Fatalf("empty log = %q %+v", b, req)
		}
		wantBoundSidecar(t, u, contract.Complete)
		u = secretUnit(t, `exit 0`)
		u.rt.ctx.Limits.MaxDiagnosticBytes = 1
		u.wantExit(u.run(), ExitOK)
		if u.rt.count("reserveArtifact") != 0 {
			t.Fatalf("a budget with no room performed an upload: %+v", u.rt.reserved)
		}
	})
	t.Run("a command that cannot start uploads no log", func(t *testing.T) {
		u := secretUnit(t, ``)
		u.rt.ctx.Command.Command = []string{"/nonexistent/binary"}
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonCommandStartFailed)
		if _, _, _, ok := u.diagnostic("command.log"); ok {
			t.Fatal("a command.log was uploaded for a command that never ran")
		}
	})
}

// ---------------------------------------------------------------------------
// A stateful runtime API and object store behind http.RoundTripper, so the
// real Client (retries, replayable bodies) drives the log/sidecar uploads.

type srvArtifact struct {
	id       domain.ArtifactID
	req      runtimev1.ArtifactReserveRequest
	bodies   [][]byte
	complete bool
}

type logServer struct {
	mu      sync.Mutex
	ctx     runtimev1.Context
	byKey   map[string]*srvArtifact
	byID    map[domain.ArtifactID]*srvArtifact
	events  []string
	counts  map[string]int
	results []runtimev1.ResultRequest
	bad     []string
	// lose reports whether the response of the nth (op, name) call, whose
	// effect has already happened, is lost on the way back.
	lose func(op, name string, n int) bool
}

func newLogServer(c runtimev1.Context) *logServer {
	return &logServer{ctx: c, byKey: map[string]*srvArtifact{}, byID: map[domain.ArtifactID]*srvArtifact{}, counts: map[string]int{}}
}

func (s *logServer) reply(req *http.Request, status int, v any) (*http.Response, error) {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)), Request: req}, nil
}

func (s *logServer) fail(req *http.Request, status int, code domain.ErrorCode) (*http.Response, error) {
	return s.reply(req, status, runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: string(code), RequestID: "req_1"}})
}

func (s *logServer) maybeLose(op, name string) error {
	s.counts[op+":"+name]++
	if s.lose != nil && s.lose(op, name, s.counts[op+":"+name]) {
		return errors.New("connection reset by peer")
	}
	return nil
}

func (s *logServer) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	path := req.URL.Path
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/admit"):
		return s.reply(req, 200, runtimev1.AdmitResponse{Token: tokenInfo("rt_unit_token_0123456789abcdef", time.Hour, 2*time.Hour), Context: s.ctx})
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/context"):
		return s.reply(req, 200, s.ctx)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/secrets/resolve"):
		var rq runtimev1.SecretResolveRequest
		if err := json.Unmarshal(body, &rq); err != nil || len(s.ctx.Secrets) != 1 || s.ctx.Secrets[0].Ref != rq.Ref {
			return s.fail(req, 403, domain.CodeForbidden)
		}
		b := s.ctx.Secrets[0]
		return s.reply(req, 200, runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
			ValueBase64: base64.StdEncoding.EncodeToString([]byte(logSecret))})
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/artifacts"):
		var rq runtimev1.ArtifactReserveRequest
		if err := json.Unmarshal(body, &rq); err != nil {
			return s.fail(req, 400, domain.CodeInvalidRequest)
		}
		key := fmt.Sprintf("%s|%s|%s|%d", rq.Role, rq.Name, rq.Digest, *rq.SizeBytes)
		a := s.byKey[key]
		status := 200
		if a == nil {
			a = &srvArtifact{id: domain.ArtifactID(fmt.Sprintf("art_srv%03d", len(s.byKey)+1)), req: rq}
			s.byKey[key], s.byID[a.id] = a, a
			status = 201
		}
		s.events = append(s.events, "reserve:"+rq.Name)
		if err := s.maybeLose("reserve", rq.Name); err != nil {
			return nil, err
		}
		return s.reply(req, status, runtimev1.ArtifactReserveResponse{ArtifactID: a.id, State: runtimev1.StateReserved, Upload: &runtimev1.Upload{
			Method: "PUT", URL: "https://objects.example.invalid/" + string(a.id), Headers: map[string]string{}, ExpiresAt: timestamp(time.Hour)}})
	case req.Method == http.MethodPut && req.URL.Host == "objects.example.invalid":
		a := s.byID[domain.ArtifactID(strings.TrimPrefix(path, "/"))]
		if a == nil {
			return s.fail(req, 404, domain.CodeNotFound)
		}
		a.bodies = append(a.bodies, body)
		s.events = append(s.events, "put:"+a.req.Name)
		if err := s.maybeLose("put", a.req.Name); err != nil {
			return nil, err
		}
		return s.reply(req, 200, struct{}{})
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/complete"):
		id := domain.ArtifactID(strings.TrimSuffix(strings.TrimPrefix(path[strings.Index(path, "/artifacts/"):], "/artifacts/"), "/complete"))
		a := s.byID[id]
		var rq runtimev1.ArtifactCompleteRequest
		if a == nil || len(a.bodies) == 0 || json.Unmarshal(body, &rq) != nil {
			return s.fail(req, 404, domain.CodeNotFound)
		}
		sum := sha256.Sum256(a.bodies[len(a.bodies)-1])
		if rq.Digest != runtimev1.Digest("sha256:"+hex.EncodeToString(sum[:])) || rq.SizeBytes != int64(len(a.bodies[len(a.bodies)-1])) {
			s.bad = append(s.bad, "completion does not match the stored object of "+a.req.Name)
			return s.fail(req, 422, runtimev1.CodeOutputInvalid)
		}
		a.complete = true
		s.events = append(s.events, "complete:"+a.req.Name)
		if err := s.maybeLose("complete", a.req.Name); err != nil {
			return nil, err
		}
		return s.reply(req, 200, runtimev1.ArtifactCompleteResponse{ArtifactID: id, State: runtimev1.StateUsable, Digest: rq.Digest, SizeBytes: rq.SizeBytes})
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/result"):
		var rq runtimev1.ResultRequest
		if err := json.Unmarshal(body, &rq); err != nil {
			return s.fail(req, 400, domain.CodeInvalidRequest)
		}
		for _, id := range rq.Result.DiagnosticArtifactIDs {
			if a := s.byID[id]; a == nil || !a.complete {
				s.bad = append(s.bad, "result cites an artifact that is not complete: "+string(id))
			}
		}
		s.results = append(s.results, rq)
		s.events = append(s.events, "result")
		return s.reply(req, 200, runtimev1.ResultResponse{AttemptID: s.ctx.AttemptID, ResultHash: rq.ResultHash, AttemptStatus: runtimev1.AttemptSucceeded, AcceptedAt: timestamp(0)})
	}
	return s.fail(req, 404, domain.CodeNotFound)
}

func (s *logServer) named(name string) *srvArtifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *srvArtifact
	for _, a := range s.byID {
		if a.req.Name == name {
			if found != nil {
				return nil // more than one artifact under the name
			}
			found = a
		}
	}
	return found
}

func Test_log_upload_retry(t *testing.T) {
	cases := []struct {
		name string
		lose func(op, name string, n int) bool
		// ops that must have been repeated
		repeated []string
	}{
		{"nothing lost", func(string, string, int) bool { return false }, nil},
		{"lost reservation response", func(op, name string, n int) bool { return op == "reserve" && name == "command.log" && n == 1 }, []string{"reserve:command.log"}},
		{"lost PUT response", func(op, name string, n int) bool { return op == "put" && name == "command.log" && n == 1 }, []string{"put:command.log"}},
		{"lost completion response", func(op, name string, n int) bool { return op == "complete" && name == "command.log" && n == 1 }, []string{"complete:command.log"}},
		{"lost sidecar reservation response", func(op, name string, n int) bool { return op == "reserve" && name == "command.log.meta.json" && n <= 2 }, []string{"reserve:command.log.meta.json"}},
		{"lost sidecar PUT response", func(op, name string, n int) bool { return op == "put" && name == "command.log.meta.json" && n == 1 }, []string{"put:command.log.meta.json"}},
		{"lost sidecar completion response", func(op, name string, n int) bool {
			return op == "complete" && name == "command.log.meta.json" && n == 1
		}, []string{"complete:command.log.meta.json"}},
		{"everything lost once", func(op, name string, n int) bool { return n == 1 }, []string{"reserve:command.log", "put:command.log", "complete:command.log", "reserve:command.log.meta.json", "put:command.log.meta.json", "complete:command.log.meta.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := secretUnit(t, `echo "retry me $DB_PASS"`)
			srv := newLogServer(u.rt.ctx)
			srv.lose = tc.lose
			client, err := NewClient("https://runtime.example.invalid", u.rt.ctx.AttemptID, "", RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}, nil)
			if err != nil {
				t.Fatal(err)
			}
			client.http = &http.Client{Transport: srv}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if code := NewStep(u.cfg, client, client).Run(ctx); code != ExitOK {
				t.Fatalf("exit = %d\n%s\nevents %v", code, u.logs.String(), srv.events)
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if len(srv.bad) != 0 {
				t.Fatalf("protocol violations: %v", srv.bad)
			}
			if len(srv.results) != 1 {
				t.Fatalf("results submitted = %d", len(srv.results))
			}
			// Replays reuse the same declaration: one artifact per name, both usable.
			names := map[string]int{}
			var logArt, sideArt *srvArtifact
			for _, a := range srv.byID {
				names[a.req.Name]++
				if !a.complete {
					t.Fatalf("artifact %s was never completed", a.req.Name)
				}
				switch a.req.Name {
				case "command.log":
					logArt = a
				case "command.log.meta.json":
					sideArt = a
				}
			}
			if names["command.log"] != 1 || names["command.log.meta.json"] != 1 || len(srv.byID) != 2 {
				t.Fatalf("artifact identity is not stable: %v", names)
			}
			for _, a := range []*srvArtifact{logArt, sideArt} {
				for _, b := range a.bodies {
					if !bytes.Equal(b, a.bodies[0]) {
						t.Fatalf("%s: a replay sent different bytes", a.req.Name)
					}
				}
			}
			for _, want := range tc.repeated {
				n := 0
				for _, e := range srv.events {
					if e == want {
						n++
					}
				}
				if n < 2 {
					t.Fatalf("%s was not replayed: events %v", want, srv.events)
				}
			}
			// The sidecar binds the (stable) log artifact and the stored bytes.
			m, err := contract.Decode(sideArt.bodies[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Bind(logArt.id, string(logArt.req.Digest), int64(len(logArt.bodies[0]))); err != nil || m.Completeness != contract.Complete {
				t.Fatalf("sidecar = %+v (%v)", m, err)
			}
			if strings.Contains(string(logArt.bodies[0]), logSecret) {
				t.Fatal("the uploaded log leaks the secret")
			}
			// Only after both artifacts were complete did the result cite them,
			// each exactly once and in order.
			ids := srv.results[0].Result.DiagnosticArtifactIDs
			if fmt.Sprint(ids) != fmt.Sprint([]domain.ArtifactID{logArt.id, sideArt.id}) {
				t.Fatalf("result cites %v, want [%s %s]", ids, logArt.id, sideArt.id)
			}
			last := -1
			for i, e := range srv.events {
				if e == "complete:command.log" || e == "complete:command.log.meta.json" {
					last = i
				}
			}
			if srv.events[len(srv.events)-1] != "result" || last < 0 || last > len(srv.events)-2 {
				t.Fatalf("result was not last: %v", srv.events)
			}
		})
	}
	t.Run("retry exhaustion leaves no reference to an unverified artifact", func(t *testing.T) {
		u := secretUnit(t, `echo hi`)
		srv := newLogServer(u.rt.ctx)
		srv.lose = func(op, name string, n int) bool { return op == "complete" && name == "command.log" }
		client, _ := NewClient("https://runtime.example.invalid", u.rt.ctx.AttemptID, "", RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, nil)
		client.http = &http.Client{Transport: srv}
		if code := NewStep(u.cfg, client, client).Run(context.Background()); code != ExitOK {
			t.Fatalf("exit = %d\n%s", code, u.logs.String())
		}
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if len(srv.results) != 1 || len(srv.results[0].Result.DiagnosticArtifactIDs) != 0 || len(srv.bad) != 0 {
			t.Fatalf("results = %+v, violations %v", srv.results, srv.bad)
		}
		for _, e := range srv.events {
			if e == "reserve:command.log.meta.json" {
				t.Fatal("a sidecar was uploaded for a log whose completion is unknown")
			}
		}
	})
}

// hookUploader runs hook for every upload before storing it.
type hookUploader struct {
	inner *fakeUploader
	mu    sync.Mutex
	n     int
	hook  func(n int, ctx context.Context) error
}

func (h *hookUploader) Upload(ctx context.Context, target runtimev1.Upload, body io.ReaderAt, size int64) error {
	h.mu.Lock()
	h.n++
	n := h.n
	h.mu.Unlock()
	if h.hook != nil {
		if err := h.hook(n, ctx); err != nil {
			return err
		}
	}
	return h.inner.Upload(ctx, target, body, size)
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func Test_log_upload_interruption(t *testing.T) {
	t.Run("cancel during capture: tree stopped, partial interrupted evidence, no leaked tail", func(t *testing.T) {
		u := secretUnit(t, `echo begin
(sleep 60 & echo $! > "$KODAMA_WORKSPACE/child.pid")
printf 'pw-9f8e7d6c5b'
: > "$KODAMA_WORKSPACE/ready"
sleep 60`)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		step := u.build()
		go func() { done <- step.Run(ctx) }()
		ready := filepath.Join(u.cfg.WorkspaceDir, "ready")
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the command never became ready\n%s", u.logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		start := time.Now()
		cancel()
		select {
		case code := <-done:
			u.wantExit(code, ExitOK)
		case <-time.After(20 * time.Second):
			t.Fatal("Run did not finish after the interrupt")
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("finalization after the interrupt took %v", d)
		}
		wantReason(t, u.lastResult(), ReasonInterrupted)
		data, err := os.ReadFile(filepath.Join(u.cfg.WorkspaceDir, "child.pid"))
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		fmt.Sscan(strings.TrimSpace(string(data)), &pid)
		waitDead(t, pid)
		logBytes, _, _, _ := u.diagnostic("command.log")
		if !strings.Contains(string(logBytes), "begin") || strings.Contains(string(logBytes), "pw-9f8e") {
			t.Fatalf("retained log = %q", logBytes)
		}
		wantBoundSidecar(t, u, contract.Partial, contract.ReasonInterrupted)
		if r := u.lastResult(); len(r.DiagnosticArtifactIDs) < 2 {
			t.Fatalf("diagnostics = %v", r.DiagnosticArtifactIDs)
		}
	})
	t.Run("timeout is partial evidence too", func(t *testing.T) {
		u := secretUnit(t, `echo before; sleep 60`)
		u.rt.ctx.Limits.TimeoutSeconds = 1
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonTimeout)
		wantBoundSidecar(t, u, contract.Partial, contract.ReasonInterrupted)
	})
	t.Run("cancel during the log upload: nothing usable, nothing complete, no result", func(t *testing.T) {
		u := secretUnit(t, `echo hi`)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		u.up = nil
		fake := &fakeUploader{}
		up := &hookUploader{inner: fake, hook: func(n int, c context.Context) error {
			cancel()
			<-c.Done()
			return c.Err()
		}}
		u.step = NewStep(u.cfg, u.rt, up)
		u.wantExit(u.step.Run(ctx), ExitFinalization)
		if u.rt.count("completeArtifact") != 0 || u.rt.count("submitResult") != 0 || len(u.rt.reserved) != 1 {
			t.Fatalf("calls = %v reserved = %+v", u.rt.calls, u.rt.reserved)
		}
		if entries, _ := os.ReadDir(u.cfg.ScratchDir); len(entries) != 0 {
			t.Fatalf("scratch files left behind: %v", entries)
		}
	})
	t.Run("cancel during the sidecar upload: the log is not claimed complete", func(t *testing.T) {
		u := secretUnit(t, `echo hi`)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fake := &fakeUploader{}
		up := &hookUploader{inner: fake, hook: func(n int, c context.Context) error {
			if n == 2 {
				cancel()
				<-c.Done()
				return c.Err()
			}
			return nil
		}}
		u.step = NewStep(u.cfg, u.rt, up)
		u.wantExit(u.step.Run(ctx), ExitFinalization)
		if u.rt.count("completeArtifact") != 1 || u.rt.count("submitResult") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
		if _, ok := fake.objects["https://objects.example.invalid/"+string(reservedID(1))]; ok {
			t.Fatal("the interrupted sidecar was stored")
		}
	})
	t.Run("finalization after an interrupt is bounded", func(t *testing.T) {
		u := secretUnit(t, `echo started; : > "$KODAMA_WORKSPACE/ready"; sleep 60`)
		u.cfg.InterruptTimeout = 300 * time.Millisecond
		up := &hookUploader{inner: &fakeUploader{}, hook: func(n int, c context.Context) error {
			<-c.Done() // a stalled object store
			return c.Err()
		}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		u.step = NewStep(u.cfg, u.rt, up)
		done := make(chan int, 1)
		go func() { done <- u.step.Run(ctx) }()
		for i := 0; ; i++ {
			if _, err := os.Stat(filepath.Join(u.cfg.WorkspaceDir, "ready")); err == nil {
				break
			}
			if i > 2000 {
				t.Fatal("the command never became ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
		start := time.Now()
		cancel()
		select {
		case code := <-done:
			u.wantExit(code, ExitFinalization)
		case <-time.After(20 * time.Second):
			t.Fatal("finalization was not bounded")
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("took %v", d)
		}
		if u.rt.count("completeArtifact") != 0 || u.rt.count("submitResult") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
	})
}

// ---------------------------------------------------------------------------

func TestLogUploadFailurePaths(t *testing.T) {
	scratchClean := func(t *testing.T, u *unit) {
		t.Helper()
		if entries, _ := os.ReadDir(u.cfg.ScratchDir); len(entries) != 0 {
			t.Fatalf("scratch files left behind: %v", entries)
		}
	}
	t.Run("a completion with the wrong digest, ID or state is never cited", func(t *testing.T) {
		for name, resp := range map[string]func(id domain.ArtifactID, r runtimev1.ArtifactCompleteRequest) runtimev1.ArtifactCompleteResponse{
			"digest": func(id domain.ArtifactID, r runtimev1.ArtifactCompleteRequest) runtimev1.ArtifactCompleteResponse {
				return runtimev1.ArtifactCompleteResponse{ArtifactID: id, State: runtimev1.StateUsable, Digest: runtimev1.Digest("sha256:" + strings.Repeat("0", 64)), SizeBytes: r.SizeBytes}
			},
			"id": func(id domain.ArtifactID, r runtimev1.ArtifactCompleteRequest) runtimev1.ArtifactCompleteResponse {
				return runtimev1.ArtifactCompleteResponse{ArtifactID: "art_other", State: runtimev1.StateUsable, Digest: r.Digest, SizeBytes: r.SizeBytes}
			},
			"state": func(id domain.ArtifactID, r runtimev1.ArtifactCompleteRequest) runtimev1.ArtifactCompleteResponse {
				return runtimev1.ArtifactCompleteResponse{ArtifactID: id, State: runtimev1.StateReserved, Digest: r.Digest, SizeBytes: r.SizeBytes}
			},
		} {
			u := secretUnit(t, `echo hi`)
			u.rt.complete = func(id domain.ArtifactID, r runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
				return resp(id, r), nil
			}
			u.wantExit(u.run(), ExitOK)
			if r := u.lastResult(); len(r.DiagnosticArtifactIDs) != 0 || len(u.rt.reserved) != 1 {
				t.Fatalf("%s: diagnostics %v, reservations %+v", name, r.DiagnosticArtifactIDs, u.rt.reserved)
			}
			scratchClean(t, u)
		}
	})
	t.Run("a missing object (completion refused) leaves the log out of the result", func(t *testing.T) {
		u := secretUnit(t, `echo hi`)
		u.rt.complete = func(domain.ArtifactID, runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
			return runtimev1.ArtifactCompleteResponse{}, apiErr("completeArtifact", 404, domain.CodeNotFound)
		}
		u.wantExit(u.run(), ExitOK)
		if r := u.lastResult(); r.Outcome != runtimev1.OutcomeCompleted || len(r.DiagnosticArtifactIDs) != 0 {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("terminal conflict on the log reservation is best effort", func(t *testing.T) {
		u := secretUnit(t, `echo hi`)
		u.rt.reserve = func(runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
			return runtimev1.ArtifactReserveResponse{}, apiErr("reserveArtifact", 409, domain.CodeIdempotencyConflict)
		}
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		if r.Outcome != runtimev1.OutcomeCompleted || len(r.DiagnosticArtifactIDs) != 0 || len(u.rt.reserved) != 1 {
			t.Fatalf("result = %+v, reservations %+v", r, u.rt.reserved)
		}
	})
	t.Run("sidecar failures keep the verified log and mean unknown completeness", func(t *testing.T) {
		for name, mutate := range map[string]func(u *unit){
			"reserve refused": func(u *unit) {
				u.rt.reserve = func(q runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
					if q.Name == "command.log.meta.json" {
						return runtimev1.ArtifactReserveResponse{}, apiErr("reserveArtifact", 409, domain.CodeIdempotencyConflict)
					}
					return runtimev1.ArtifactReserveResponse{ArtifactID: "art_log00001", State: runtimev1.StateReserved, Upload: &runtimev1.Upload{
						Method: "PUT", URL: "https://objects.example.invalid/art_log00001", Headers: map[string]string{}, ExpiresAt: timestamp(time.Hour)}}, nil
				}
			},
			"upload exhausted": func(u *unit) {
				n := 0
				u.step = NewStep(u.cfg, u.rt, &hookUploader{inner: u.up, hook: func(int, context.Context) error {
					if n++; n == 2 {
						return &TransportError{Op: "upload", Err: errors.New("reset")}
					}
					return nil
				}})
			},
			"completion unknown": func(u *unit) {
				u.rt.complete = func(id domain.ArtifactID, r runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
					if id == reservedID(1) {
						return runtimev1.ArtifactCompleteResponse{}, &TransportError{Op: "completeArtifact", Err: errors.New("reset")}
					}
					return runtimev1.ArtifactCompleteResponse{ArtifactID: id, State: runtimev1.StateUsable, Digest: r.Digest, SizeBytes: r.SizeBytes}, nil
				}
			},
		} {
			u := secretUnit(t, `echo hi`)
			mutate(u)
			step := u.step
			if step == nil {
				step = u.build()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			u.wantExit(step.Run(ctx), ExitOK)
			cancel()
			r := u.lastResult()
			if len(r.DiagnosticArtifactIDs) != 1 || r.Outcome != runtimev1.OutcomeCompleted {
				t.Fatalf("%s: result = %+v\n%s", name, r, u.logs.String())
			}
			scratchClean(t, u)
		}
	})
	t.Run("authority denial while reserving the sidecar stops finalization", func(t *testing.T) {
		u := secretUnit(t, `echo hi`)
		u.rt.reserve = func(q runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
			if q.Name == "command.log.meta.json" {
				return runtimev1.ArtifactReserveResponse{}, apiErr("reserveArtifact", 403, runtimev1.CodeAttemptRevoked)
			}
			return runtimev1.ArtifactReserveResponse{ArtifactID: "art_log00001", State: runtimev1.StateReserved, Upload: &runtimev1.Upload{
				Method: "PUT", URL: "https://objects.example.invalid/art_log00001", Headers: map[string]string{}, ExpiresAt: timestamp(time.Hour)}}, nil
		}
		u.wantExit(u.run(), ExitRevoked)
		if u.rt.count("submitResult") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
		scratchClean(t, u)
	})
	t.Run("an unusable redaction set withholds the log and the notes", func(t *testing.T) {
		a, u := newResultAttempt(t, nil)
		for i := 0; i < 300; i++ {
			a.needles = append(a.needles, []byte(fmt.Sprintf("secret-%03d", i)))
		}
		raw := filepath.Join(u.cfg.ScratchDir, "raw.log")
		if err := os.WriteFile(raw, []byte("contains secret-001\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ab := a.uploadCommandLog(raw, captureEvidence{}); ab != nil {
			t.Fatal(ab)
		}
		a.note("contains secret-001")
		if code := a.finalize(failed(ReasonExitNonzero, nil)); code != ExitOK {
			t.Fatalf("exit %d", code)
		}
		for _, q := range u.rt.reserved {
			t.Fatalf("an upload happened with an unusable redaction set: %+v", q)
		}
		os.Remove(raw)
		scratchClean(t, u)
	})
	t.Run("zero budget uploads nothing", func(t *testing.T) {
		a, u := newResultAttempt(t, nil)
		a.c.Limits.MaxDiagnosticBytes = 0
		raw := filepath.Join(u.cfg.ScratchDir, "raw.log")
		os.WriteFile(raw, []byte("some output\n"), 0o600)
		defer os.Remove(raw)
		if ab := a.uploadCommandLog(raw, captureEvidence{}); ab != nil {
			t.Fatal(ab)
		}
		a.note("a note")
		a.uploadNotes()
		if len(u.rt.reserved) != 0 {
			t.Fatalf("reservations = %+v", u.rt.reserved)
		}
	})
	t.Run("notes are bounded by what the log and sidecar left", func(t *testing.T) {
		u := secretUnit(t, `head -c 100000 /dev/zero | tr '\0' 'n'; exit 2`)
		u.rt.ctx.Limits.MaxDiagnosticBytes = 2000
		u.wantExit(u.run(), ExitOK)
		var total int64
		for _, q := range u.rt.reserved {
			total += *q.SizeBytes
		}
		if total > 2000 {
			t.Fatalf("diagnostics total %d, budget 2000: %+v", total, u.rt.reserved)
		}
		scratchClean(t, u)
	})
}

func TestLogContractFixtures(t *testing.T) {
	raw, err := os.ReadFile("../logs/testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name     string `json:"name"`
			Artifact struct {
				ID        domain.ArtifactID `json:"id"`
				Digest    string            `json:"digest"`
				SizeBytes int64             `json:"sizeBytes"`
			} `json:"artifact"`
			Metadata     json.RawMessage `json:"metadata"`
			Decode       string          `json:"decode"`
			Bind         string          `json:"bind"`
			Completeness string          `json:"completeness"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Cases) == 0 {
		t.Fatalf("fixtures: %v", err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if string(c.Metadata) == "null" {
				if got, _ := contract.Unverified(); got == contract.Complete {
					t.Fatal("a log without a sidecar must not be complete")
				}
				return
			}
			m, err := contract.Decode(c.Metadata)
			if c.Decode != "ok" {
				if !errors.Is(err, contract.ErrInvalid) {
					t.Fatalf("decode = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			berr := m.Bind(c.Artifact.ID, c.Artifact.Digest, c.Artifact.SizeBytes)
			if c.Bind == "binding" {
				if !errors.Is(berr, contract.ErrBinding) {
					t.Fatalf("bind = %v, want ErrBinding", berr)
				}
				return
			}
			if berr != nil {
				t.Fatalf("bind: %v", berr)
			}
			if m.Source != contract.SourceWrapper {
				return // controller statements are T-064's to produce
			}
			// The entrypoint produces the frozen canonical encoding for the same
			// declaration.
			fl := &frozenLog{size: c.Artifact.SizeBytes, completeness: m.Completeness, reasons: m.Reasons}
			got, err := commandLogMetadata(c.Artifact.ID, runtimev1.Digest(c.Artifact.Digest), fl)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := contract.Encode(m)
			if !bytes.Equal(got, want) {
				t.Fatalf("entrypoint sidecar = %s\nfixture canonical  = %s", got, want)
			}
		})
	}
	t.Run("a sidecar copied onto another artifact is refused", func(t *testing.T) {
		fl := &frozenLog{size: 10, completeness: contract.Complete}
		b, err := commandLogMetadata("art_one", digestOfBytes([]byte("0123456789")), fl)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := contract.Decode(b)
		if err := m.Bind("art_two", string(digestOfBytes([]byte("0123456789"))), 10); !errors.Is(err, contract.ErrBinding) {
			t.Fatalf("bind = %v", err)
		}
		if err := m.Bind("art_one", string(digestOfBytes([]byte("replaced!!!"))), 10); !errors.Is(err, contract.ErrBinding) {
			t.Fatalf("bind = %v", err)
		}
	})
}

// A diagnostic note can embed command-controlled text (an undeclared manifest
// output key). The injected secret's backing array is zeroed when the command
// phase returns, so the notes must be redacted from an independent snapshot.
func TestWrapperLogUploadRedactsNotesAfterSecretCleanup(t *testing.T) {
	u := secretUnit(t, `printf '{"outcome":"completed","outputs":{"key-%s":"x"}}' "$DB_PASS" > "$KODAMA_RESULTS_FILE"`)
	u.wantExit(u.run(), ExitOK)
	notes, _, _, ok := u.diagnostic("entrypoint.log")
	if !ok {
		t.Fatalf("no entrypoint.log was uploaded; calls %v", u.rt.calls)
	}
	got := string(notes)
	if strings.Contains(got, logSecret) {
		t.Fatalf("entrypoint.log leaks the injected secret:\n%s", got)
	}
	if !strings.Contains(got, "undeclared or a file output") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("entrypoint.log lacks the redacted diagnostic note:\n%s", got)
	}
	if b, _, _, ok := u.diagnostic("command.log"); ok && strings.Contains(string(b), logSecret) {
		t.Fatalf("command.log leaks the injected secret:\n%s", b)
	}
}

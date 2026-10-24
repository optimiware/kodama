package entrypoint

// Container-log mirror (T-078): ordinary output reaches the container log
// stream after redaction, within the retained bound, without changing the
// retained log, the sidecar, the result or process-tree handling. The sink is
// a test double; the real Pod/node/controller fallback join is T-064.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// failingSink rejects every write after the first n bytes.
type failingSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
	ok  int
}

func (s *failingSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() >= s.ok {
		return 0, errors.New("sink: broken pipe")
	}
	return s.buf.Write(p)
}

// gateSink blocks every write until released.
type gateSink struct {
	open chan struct{}
	once sync.Once
	buf  safeBuf
}

func newGateSink(t *testing.T) *gateSink {
	g := &gateSink{open: make(chan struct{})}
	t.Cleanup(g.release)
	return g
}
func (g *gateSink) release() { g.once.Do(func() { close(g.open) }) }
func (g *gateSink) Write(p []byte) (int, error) {
	<-g.open
	return g.buf.Write(p)
}

// needleSet is a growable redaction set with a generation, like the Step's.
type needleSet struct {
	mu   sync.Mutex
	vals [][]byte
	gen  uint64
}

func (n *needleSet) add(v string) {
	n.mu.Lock()
	n.vals = append(n.vals, []byte(v))
	n.gen++
	n.mu.Unlock()
}
func (n *needleSet) get() ([][]byte, uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([][]byte{}, n.vals...), n.gen
}

func testMirror(t *testing.T, sink *safeBuf, capacity int64, needles ...string) (*containerMirror, *needleSet) {
	t.Helper()
	ns := &needleSet{}
	for _, v := range needles {
		ns.add(v)
	}
	return newContainerMirror(sink, capacity, ns.get, slog.New(slog.NewTextHandler(&safeBuf{}, nil))), ns
}

func mirrorBytes(u *unit) string { return u.cfg.ContainerLog.(*safeBuf).String() }

func mirrorUnit(t *testing.T, script string) *unit {
	t.Helper()
	u := secretUnit(t, script)
	u.cfg.ContainerLog = &safeBuf{}
	return u
}

func Test_container_log_mirror(t *testing.T) {
	t.Run("ordinary stdout and stderr reach the container log and equal the retained log", func(t *testing.T) {
		u := mirrorUnit(t, `echo out-1; echo err-1 >&2; echo out-1; echo out-1; printf 'no newline at end'`)
		u.wantExit(u.run(), ExitOK)
		got := mirrorBytes(u)
		retained, _, _, _ := u.diagnostic("command.log")
		want := "out-1\nerr-1\nout-1\nout-1\nno newline at end"
		if string(retained) != want {
			t.Fatalf("retained log changed: %q", retained)
		}
		// The mirror adds only a closing newline after an unterminated last line,
		// and never suppresses repeated lines.
		if got != want+"\n" || strings.Count(got, "out-1\n") != 3 {
			t.Fatalf("mirror = %q, want %q", got, want+"\n")
		}
		wantBoundSidecar(t, u, contract.Complete)
		if r := u.lastResult(); r.Outcome != runtimev1.OutcomeCompleted || len(r.DiagnosticArtifactIDs) != 2 {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("no sink, no mirror, and the retained artifacts are identical", func(t *testing.T) {
		with := mirrorUnit(t, `echo same`)
		with.wantExit(with.run(), ExitOK)
		without := secretUnit(t, `echo same`)
		without.wantExit(without.run(), ExitOK)
		a, ra, _, _ := with.diagnostic("command.log")
		b, rb, _, _ := without.diagnostic("command.log")
		sa, _, _, _ := with.diagnostic("command.log.meta.json")
		sb, _, _, _ := without.diagnostic("command.log.meta.json")
		if !bytes.Equal(a, b) || ra.Digest != rb.Digest || len(sa) != len(sb) {
			t.Fatalf("the mirror changed retained evidence: %q vs %q", a, b)
		}
		if !bytes.Equal(mustMeta(t, sa), mustMeta(t, sb)) {
			t.Fatal("the sidecar differs")
		}
	})
	t.Run("a failing sink never changes capture, sidecar or result", func(t *testing.T) {
		u := secretUnit(t, `i=0; while [ $i -lt 2000 ]; do echo "line $i"; i=$((i+1)); done; exit 4`)
		sink := &failingSink{ok: 100}
		u.cfg.ContainerLog = sink
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonExitNonzero)
		retained, _, _, _ := u.diagnostic("command.log")
		if !strings.HasSuffix(string(retained), "line 1999\n") {
			t.Fatalf("retained log was affected by the sink: %d bytes", len(retained))
		}
		wantBoundSidecar(t, u, contract.Complete)
		if !strings.Contains(u.logs.String(), "container log mirror is partial") {
			t.Fatalf("a failed sink must be reported:\n%s", u.logs.String())
		}
	})
	t.Run("a stalled sink does not block capture, the process or finalization", func(t *testing.T) {
		u := secretUnit(t, `i=0; while [ $i -lt 60000 ]; do echo "stalled line $i"; i=$((i+1)); done`)
		u.rt.ctx.Limits.MaxDiagnosticBytes = 4 << 20
		gate := newGateSink(t)
		u.cfg.ContainerLog = gate
		began := time.Now()
		u.wantExit(u.run(), ExitOK)
		// Everything after the command is bounded by the kill grace (200ms).
		if d := time.Since(began); d > 10*time.Second {
			t.Fatalf("run took %v with a stalled sink", d)
		}
		retained, _, _, _ := u.diagnostic("command.log")
		if !strings.HasSuffix(string(retained), "stalled line 59999\n") {
			t.Fatalf("retained log is incomplete: %d bytes", len(retained))
		}
		wantBoundSidecar(t, u, contract.Complete)
		if !strings.Contains(u.logs.String(), `"abandoned":true`) {
			t.Fatalf("the abandoned sink was not reported:\n%s", u.logs.String())
		}
		gate.release() // late writes after Close are discarded
	})
	t.Run("timeout stops the tree, flushes what was seen and says nothing more", func(t *testing.T) {
		u := mirrorUnit(t, `echo before-timeout
(sleep 60 & echo $! > "$KODAMA_WORKSPACE/child.pid")
printf 'unfinished'
sleep 60`)
		u.rt.ctx.Limits.TimeoutSeconds = 1
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonTimeout)
		if got := mirrorBytes(u); got != "before-timeout\nunfinished\n" {
			t.Fatalf("mirror = %q", got)
		}
		pid := readPID(t, filepath.Join(u.cfg.WorkspaceDir, "child.pid"))
		waitDead(t, pid)
		wantBoundSidecar(t, u, contract.Partial, contract.ReasonInterrupted)
	})
	t.Run("interruption stops the tree and finalizes with the mirror attached", func(t *testing.T) {
		u := mirrorUnit(t, `echo begin
(sleep 60 & echo $! > "$KODAMA_WORKSPACE/child.pid")
: > "$KODAMA_WORKSPACE/ready"
sleep 60`)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		step := u.build()
		go func() { done <- step.Run(ctx) }()
		readPID(t, filepath.Join(u.cfg.WorkspaceDir, "child.pid"))
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(u.cfg.WorkspaceDir, "ready")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the command never became ready")
			}
		}
		cancel()
		select {
		case code := <-done:
			u.wantExit(code, ExitOK)
		case <-time.After(20 * time.Second):
			t.Fatal("Run did not finish after the interrupt")
		}
		wantReason(t, u.lastResult(), ReasonInterrupted)
		waitDead(t, readPID(t, filepath.Join(u.cfg.WorkspaceDir, "child.pid")))
		if got := mirrorBytes(u); got != "begin\n" {
			t.Fatalf("mirror = %q", got)
		}
		wantBoundSidecar(t, u, contract.Partial, contract.ReasonInterrupted)
	})
	t.Run("one stream: command lines and entrypoint log lines never interleave mid-line", func(t *testing.T) {
		u := secretUnit(t, `i=0; while [ $i -lt 300 ]; do printf 'cmd-line-%03d part-a ' $i; sleep 0.001; echo part-b; i=$((i+1)); done`)
		buf := &safeBuf{}
		stream := newContainerStream(buf)
		u.cfg.ContainerLog = stream
		u.cfg.Logger = slog.New(slog.NewJSONHandler(stream, &slog.HandlerOptions{Level: slog.LevelDebug}))
		u.wantExit(u.run(), ExitOK)
		stream.Close(10 * time.Second)
		cmdLines, jsonLines := 0, 0
		for _, l := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
			switch {
			case strings.HasPrefix(l, "{"):
				var v map[string]any
				if json.Unmarshal([]byte(l), &v) != nil {
					t.Fatalf("garbled entrypoint log line %q", l)
				}
				jsonLines++
			case strings.HasPrefix(l, "cmd-line-") && strings.HasSuffix(l, " part-a part-b"):
				cmdLines++
			default:
				t.Fatalf("garbled line %q", l)
			}
		}
		if cmdLines != 300 || jsonLines == 0 {
			t.Fatalf("command lines %d, log lines %d", cmdLines, jsonLines)
		}
	})
	t.Run("the CLI writes the mirror to its stderr next to its own log lines", runMirrorCLI)
	t.Run("the mirror carries no completeness claim of its own", func(t *testing.T) {
		u := mirrorUnit(t, `echo hi`)
		u.wantExit(u.run(), ExitOK)
		for _, q := range u.rt.reserved {
			if strings.Contains(q.Name, "mirror") || strings.Contains(q.Name, "container") {
				t.Fatalf("the mirror was registered as an artifact: %+v", q)
			}
		}
		if len(u.rt.reserved) != 2 {
			t.Fatalf("diagnostics = %d, want log and sidecar", len(u.rt.reserved))
		}
	})
}

// Run (the CLI) mirrors the command's output to its stderr, which is the
// container log stream, next to its own structured log lines.
func runMirrorCLI(t *testing.T) {
	ctxDoc := unitContext(`echo "cli out $DB_PASS"; echo cli-err >&2`)
	b := secretBinding(logSecretEnv, "secret://project/db-pass", runtimev1.DeliveryEnv)
	ctxDoc.Secrets = []runtimev1.SecretBinding{b}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var v any
		switch {
		case strings.HasSuffix(r.URL.Path, "/admit"):
			v = runtimev1.AdmitResponse{Token: tokenInfo("rt_unit_token_0123456789abcdef", time.Hour, 2*time.Hour), Context: ctxDoc}
		case strings.HasSuffix(r.URL.Path, "/context"):
			v = ctxDoc
		case strings.HasSuffix(r.URL.Path, "/secrets/resolve"):
			v = runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
				ValueBase64: base64.StdEncoding.EncodeToString([]byte(logSecret))}
		case strings.HasSuffix(r.URL.Path, "/result"):
			var rq runtimev1.ResultRequest
			_ = json.NewDecoder(r.Body).Decode(&rq)
			v = runtimev1.ResultResponse{AttemptID: ctxDoc.AttemptID, ResultHash: rq.ResultHash, AttemptStatus: runtimev1.AttemptSucceeded, AcceptedAt: timestamp(0)}
		default: // diagnostics cannot be uploaded: the mirror is what remains
			w.WriteHeader(http.StatusForbidden)
			v = runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: domain.CodeForbidden, Message: "no", RequestID: "r1"}}
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(srv.Close)
	var out, errb safeBuf
	if code := Run(context.Background(), bootstrapDir(t, srv.URL), noEnv, &out, &errb); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	got := errb.String()
	if !strings.Contains(got, "cli out [REDACTED]\n") || !strings.Contains(got, "cli-err\n") || !strings.Contains(got, `"msg":"launching command"`) {
		t.Fatalf("stderr = %s", got)
	}
	if strings.Contains(got, logSecret) || strings.Contains(got, unitAdmission) || out.String() != "" {
		t.Fatalf("leak or stdout write: stderr %s stdout %q", got, out.String())
	}
}

func mustMeta(t *testing.T, b []byte) []byte {
	t.Helper()
	m, err := contract.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	m.Log = contract.Ref{} // artifact IDs and digests are checked separately
	out, _ := json.Marshal(m)
	return out
}

func Test_container_log_redaction(t *testing.T) {
	t.Run("env secret, file secret, admission credential, token and renewed token never reach the sink", func(t *testing.T) {
		const fileSecret = "file-secret-value-5544-zzz"
		u := mirrorUnit(t, `p="$DB_PASS"; head="${p%%????}"; tail="${p#"$head"}"
printf 'env %s' "$head"; sleep 0.1; printf '%s\n' "$tail"
echo "file=$(cat "$KODAMA_SECRETS_DIR/file-key")"
echo "admission=$(cat @ADMISSION@) token=$(cat @TOKEN@)"
n=0
while [ "$(cat @TOKEN@)" != "rt_unit_renewed_0123456789abcdef" ] && [ $n -lt 500 ]; do sleep 0.02; n=$((n+1)); done
echo "renewed=$(cat @TOKEN@)"
echo plain-text`)
		fb := secretBinding("file-key", "secret://project/file-key", runtimev1.DeliveryFile)
		eb := u.rt.ctx.Secrets[0]
		u.rt.ctx.Secrets = []runtimev1.SecretBinding{eb, fb}
		u.rt.resolve = func(r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
			v := logSecret
			if r.Ref == fb.Ref {
				v = fileSecret
			}
			b := eb
			if r.Ref == fb.Ref {
				b = fb
			}
			return runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
				ValueBase64: base64.StdEncoding.EncodeToString([]byte(v))}, nil
		}
		u.rt.admit = func(runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
			return runtimev1.AdmitResponse{Token: tokenInfo("rt_unit_token_0123456789abcdef", 100*time.Millisecond, time.Hour), Context: u.rt.ctx}, nil
		}
		u.wantExit(u.run(), ExitOK)
		if u.rt.count("renewToken") == 0 {
			t.Fatal("the token was never renewed")
		}
		got := mirrorBytes(u)
		for _, leak := range []string{logSecret, fileSecret, "SECRET", "sECRET", unitAdmission, "rt_unit_token_0123", "rt_unit_renewed", "0123456789abcdef"} {
			if strings.Contains(got, leak) {
				t.Fatalf("container log leaks %q:\n%s", leak, got)
			}
		}
		for _, want := range []string{"env [REDACTED]\n", "file=[REDACTED]\n", "admission=[REDACTED] token=[REDACTED]\n", "renewed=[REDACTED]\n", "plain-text\n"} {
			if !strings.Contains(got, want) {
				t.Fatalf("container log lacks %q:\n%s", want, got)
			}
		}
		retained, _, _, _ := u.diagnostic("command.log")
		if string(retained) != got {
			t.Fatalf("mirror and retained log differ:\n%q\n%q", got, retained)
		}
	})
	t.Run("every checkout credential representation is in the shared set", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		u := newUnit(t, `exit 0`)
		const user, pass = "ci-user", "ci-pass-0042"
		if err := os.WriteFile(u.cfg.CheckoutCredentialFile, []byte(user+":"+pass+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		u.rt.ctx.Workspace = runtimev1.Workspace{Access: runtimev1.AccessRead, Source: &runtimev1.Source{
			RepositoryURL: "https://127.0.0.1:1/r.git", Commit: runtimev1.GitCommit(strings.Repeat("1", 40))}}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a := &attempt{s: u.build(), parent: ctx, runCtx: ctx, c: u.rt.ctx}
		if v := a.checkout(); v == nil { // unreachable repository: the command would not launch
			t.Fatal("checkout unexpectedly succeeded")
		}
		cred := &checkoutCredential{username: user, password: pass}
		forms := []string{pass, user + ":" + pass, base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)), cred.header()}
		sink := &safeBuf{}
		m := newContainerMirror(sink, 1<<20, a.logNeedlesGen, slog.New(slog.NewTextHandler(&safeBuf{}, nil)))
		for _, f := range forms {
			m.Write([]byte("seen " + f + " end\n"))
		}
		m.Close(true, time.Second)
		for _, f := range forms {
			if strings.Contains(sink.String(), f) || strings.Contains(sink.String(), pass) {
				t.Fatalf("container log leaks %q:\n%s", f, sink.String())
			}
		}
		for _, n := range a.secretNeedles() {
			if strings.Contains(string(n), "Authorization") {
				t.Fatal("source scanning must keep its existing value set")
			}
		}
	})
	t.Run("split and overlapping secrets", func(t *testing.T) {
		sink := &safeBuf{}
		m, _ := testMirror(t, sink, 1<<20, "abcdef", "defxyz", "needle-9917")
		for _, b := range []byte("pre abcdefxyz mid need") {
			m.Write([]byte{b})
		}
		for _, b := range []byte("le-9917 post\n") {
			m.Write([]byte{b})
		}
		m.Close(true, time.Second)
		if got := sink.String(); got != "pre [REDACTED] mid [REDACTED] post\n" {
			t.Fatalf("mirror = %q", got)
		}
	})
	t.Run("a token issued mid-stream is redacted from the next byte and the held tail stays held", func(t *testing.T) {
		sink := &safeBuf{}
		m, ns := testMirror(t, sink, 1<<20, "old-secret-value")
		m.Write([]byte("a old-secret-")) // held: may become the old secret
		ns.add("rt_new_token_12345")
		m.Write([]byte("value line\nb rt_new_token_12345 c\n"))
		m.Close(true, time.Second)
		if got := sink.String(); got != "a [REDACTED] line\nb [REDACTED] c\n" {
			t.Fatalf("mirror = %q", got)
		}
	})
	t.Run("a rebuild at every split point leaks no part of either value", func(t *testing.T) {
		const old, tok = "old-secret-value-0001", "rt_new_token_98765"
		stream := "x " + old + " y " + tok + " z " + old + tok + "\n"
		for cut := 0; cut <= len(stream); cut++ {
			for _, addAt := range []int{0, cut / 2, cut} {
				sink := &safeBuf{}
				m, ns := testMirror(t, sink, 1<<20, old)
				m.Write([]byte(stream[:addAt]))
				ns.add(tok)
				m.Write([]byte(stream[addAt:cut]))
				m.Write([]byte(stream[cut:]))
				m.Close(true, time.Second)
				got := sink.String()
				for _, part := range []string{old[:6], tok[:6], old[len(old)-5:], tok[len(tok)-5:]} {
					if strings.Contains(got, part) && !(addAt > 0 && strings.Contains(stream[:addAt], tok[:6])) {
						t.Fatalf("cut %d add %d leaks %q: %q", cut, addAt, part, got)
					}
				}
			}
		}
	})
	t.Run("a tail that may start a secret is released only for output seen to its end", func(t *testing.T) {
		for whole, want := range map[bool]string{true: "ok partial-tok\n", false: "ok \n"} {
			sink := &safeBuf{}
			m, _ := testMirror(t, sink, 1<<20, "partial-token-secret")
			m.Write([]byte("ok partial-tok"))
			m.Close(whole, time.Second)
			if got := sink.String(); got != want {
				t.Fatalf("whole=%v: mirror = %q, want %q", whole, got, want)
			}
		}
	})
	t.Run("an unusable redaction set fails closed for mirror and retained log", func(t *testing.T) {
		u := mirrorUnit(t, `echo "$DB_PASS"; echo more`)
		big := strings.Repeat("S", logs.MaxRedactValueLen+1)
		u.rt.resolve = func(r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
			b := u.rt.ctx.Secrets[0]
			return runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
				ValueBase64: base64.StdEncoding.EncodeToString([]byte(big))}, nil
		}
		u.wantExit(u.run(), ExitOK)
		if got := mirrorBytes(u); got != MirrorDisabledMarker || strings.Contains(got, "S") && strings.Contains(got, "SSS") {
			t.Fatalf("mirror = %q, want only the disabled marker", got)
		}
		if _, _, _, ok := u.diagnostic("command.log"); ok {
			t.Fatal("the retained log was uploaded although the set is unusable")
		}
	})
	t.Run("no raw byte is ever written before redaction", func(t *testing.T) {
		// Feed every prefix split of a secret-bearing stream; no sink content may
		// contain the secret or any 6-byte prefix of it.
		const secret = "Tr0ub4dor&3-xyz"
		stream := "a " + secret + " b\nc " + secret + secret + "\n"
		for cut := 0; cut <= len(stream); cut++ {
			sink := &safeBuf{}
			m, _ := testMirror(t, sink, 1<<20, secret)
			m.Write([]byte(stream[:cut]))
			m.Write([]byte(stream[cut:]))
			m.Close(true, time.Second)
			if got := sink.String(); strings.Contains(got, secret[:6]) {
				t.Fatalf("cut %d leaks: %q", cut, got)
			}
		}
	})
}

func Test_container_log_bounds(t *testing.T) {
	t.Run("a runaway command is cut at the retained bound with one marker", func(t *testing.T) {
		u := mirrorUnit(t, `head -c 300000 /dev/zero | tr '\0' 'z'; echo; exit 1`)
		u.rt.ctx.Limits.MaxDiagnosticBytes = 4096
		u.wantExit(u.run(), ExitOK)
		got := mirrorBytes(u)
		capacity := dataCapacity(commandLogCapacity(4096))
		if int64(len(got)) > capacity+int64(len(MirrorTruncationMarker)) || !strings.HasSuffix(got, MirrorTruncationMarker) {
			t.Fatalf("mirror is %d bytes (bound %d + marker), tail %q", len(got), capacity, got[max(0, len(got)-40):])
		}
		retained, _, _, _ := u.diagnostic("command.log")
		if strings.TrimSuffix(got, MirrorTruncationMarker) != strings.TrimSuffix(string(retained), truncationMarker) {
			t.Fatalf("mirror and retained bytes differ before the marker (%d vs %d)", len(got), len(retained))
		}
		wantBoundSidecar(t, u, contract.Partial, contract.ReasonTruncated)
	})
	t.Run("redaction cannot expand the mirror past its bound", func(t *testing.T) {
		sink := &safeBuf{}
		m, _ := testMirror(t, sink, 1000, "a")
		for range 100 {
			m.Write(bytes.Repeat([]byte("a"), 100)) // each byte becomes a 10 byte marker
		}
		stats := m.Close(true, time.Second)
		if n := len(sink.String()); n > 1000+len(MirrorTruncationMarker) || !stats.Truncated || !strings.HasSuffix(sink.String(), MirrorTruncationMarker) {
			t.Fatalf("%d bytes, %+v", n, stats)
		}
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(sink.String(), logs.Marker, ""), MirrorTruncationMarker, ""), "a") {
			t.Fatal("unredacted byte")
		}
	})
	t.Run("a full queue drops output, stays bounded, and notes the gap once", func(t *testing.T) {
		gate := newGateSink(t)
		ns := &needleSet{}
		m := newContainerMirror(gate, 1<<30, ns.get, slog.New(slog.NewTextHandler(&safeBuf{}, nil)))
		line := bytes.Repeat([]byte("x"), 1023)
		line = append(line, '\n')
		for range 4000 { // ~4 MB offered against a ~0.5 MB queue
			m.Write(line)
		}
		m.mu.Lock()
		emitted, dropped := m.stats.Emitted, m.stats.Dropped
		m.mu.Unlock()
		if !dropped || emitted > int64((mirrorQueueChunks+1)*mirrorChunkMax) {
			t.Fatalf("emitted %d, dropped %v: the queue is not bounded", emitted, dropped)
		}
		gate.release()
		deadline := time.Now().Add(5 * time.Second)
		for m.queued() > 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		m.Write([]byte("after the gap\n"))
		m.Write([]byte("later\n"))
		m.Close(true, 5*time.Second)
		got := gate.buf.String()
		if strings.Count(got, MirrorGapMarker) != 1 || !strings.Contains(got, MirrorGapMarker+"after the gap\nlater\n") {
			t.Fatalf("gap marker count %d, tail %q", strings.Count(got, MirrorGapMarker), got[max(0, len(got)-80):])
		}
	})
	t.Run("an unterminated line is held only up to a bound", func(t *testing.T) {
		sink := &safeBuf{}
		m, _ := testMirror(t, sink, 1<<20)
		m.Write(bytes.Repeat([]byte("p"), mirrorLineMax+10))
		deadline := time.Now().Add(2 * time.Second)
		for len(sink.String()) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if len(sink.String()) == 0 {
			t.Fatal("an over-long line was held back without bound")
		}
		m.Close(true, time.Second)
	})
	t.Run("after Close nothing more is written and Close returns within its wait", func(t *testing.T) {
		gate := newGateSink(t)
		ns := &needleSet{}
		m := newContainerMirror(gate, 1<<20, ns.get, slog.New(slog.NewTextHandler(&safeBuf{}, nil)))
		m.Write([]byte("blocked\n"))
		began := time.Now()
		stats := m.Close(true, 100*time.Millisecond)
		if d := time.Since(began); d > 2*time.Second || !stats.Abandoned {
			t.Fatalf("close took %v, %+v", d, stats)
		}
		m.Write([]byte("late\n"))
		gate.release()
		time.Sleep(50 * time.Millisecond)
		if strings.Contains(gate.buf.String(), "late") {
			t.Fatal("a write after Close reached the sink")
		}
	})
}

func (m *containerMirror) queued() int { return len(m.ch) }

// stallAfterSink accepts writes until a write contains trigger, then blocks
// that write (and all later ones) until released: a container log stream that
// stalls after startup.
type stallAfterSink struct {
	trigger string
	open    chan struct{}
	once    sync.Once
	buf     safeBuf
}

func newStallAfterSink(t *testing.T, trigger string) *stallAfterSink {
	s := &stallAfterSink{trigger: trigger, open: make(chan struct{})}
	t.Cleanup(s.release)
	return s
}
func (s *stallAfterSink) release() { s.once.Do(func() { close(s.open) }) }
func (s *stallAfterSink) Write(p []byte) (int, error) {
	if strings.Contains(string(p), s.trigger) {
		<-s.open
	}
	return s.buf.Write(p)
}

// The real CLI wires the entrypoint logger and the mirror to one stream. When
// that stream stalls after startup, command output and the mirror's own
// warnings must not hold up capture freeze, the diagnostic upload or the
// result.
func Test_container_log_shared_stream_stall(t *testing.T) {
	u := secretUnit(t, `echo "stall-trigger"; i=0; while [ $i -lt 120000 ]; do echo "shared stalled line $i"; i=$((i+1)); done`)
	u.rt.ctx.Limits.MaxDiagnosticBytes = 8 << 20
	sink := newStallAfterSink(t, "stall-trigger")
	stream := newContainerStream(sink) // exactly Run's wiring
	t.Cleanup(func() { sink.release(); stream.Close(10 * time.Second) })
	u.cfg.Logger = slog.New(slog.NewJSONHandler(stream, &slog.HandlerOptions{Level: slog.LevelDebug}))
	u.cfg.ContainerLog = stream
	began := time.Now()
	done := make(chan int, 1)
	go func() { done <- u.run() }()
	select {
	case code := <-done:
		u.wantExit(code, ExitOK)
	case <-time.After(30 * time.Second):
		t.Fatal("the step did not finish while the container log stream was stalled")
	}
	if d := time.Since(began); d > 20*time.Second {
		t.Fatalf("run took %v with a stalled container log", d)
	}
	if r := u.lastResult(); r.Outcome != runtimev1.OutcomeCompleted {
		t.Fatalf("result = %s, want completed", r.Outcome)
	}
	retained, _, _, _ := u.diagnostic("command.log")
	if !strings.HasSuffix(string(retained), "shared stalled line 119999\n") {
		t.Fatalf("retained log is incomplete: %d bytes", len(retained))
	}
	wantBoundSidecar(t, u, contract.Complete)
	if !strings.Contains(sink.buf.String(), `"msg"`) {
		t.Fatalf("startup logs never reached the sink: %q", sink.buf.String())
	}
	// Releasing the sink delivers what was queued, whole lines in order, and
	// marks what was lost.
	sink.release()
	stream.Close(20 * time.Second)
	got := sink.buf.String()
	if !strings.Contains(got, streamDropMarker) {
		t.Fatalf("the loss was not marked; sink has %d bytes", len(got))
	}
	for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		switch {
		case l == "", strings.HasPrefix(l, "[kodama: container log dropped output]"):
		case l == strings.TrimSpace(MirrorGapMarker), l == strings.TrimSpace(MirrorDisabledMarker), l == strings.TrimSpace(MirrorTruncationMarker):
		case strings.HasPrefix(l, "{"):
			var v map[string]any
			if json.Unmarshal([]byte(l), &v) != nil {
				t.Fatalf("garbled log line %q", l)
			}
		case l == "stall-trigger", strings.HasPrefix(l, "shared stalled line "):
		default:
			t.Fatalf("garbled line %q", l)
		}
	}
}

// chunkSink records every write so a test can see where chunks begin and end.
type chunkSink struct {
	mu     sync.Mutex
	chunks []string
}

func (c *chunkSink) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chunks = append(c.chunks, string(p))
	return len(p), nil
}

// A write holding more than mirrorLineMax of complete lines plus a short
// unterminated tail must queue the lines only. The container stream drops
// whole chunks, so a chunk that ended mid-line would leave half a line.
func Test_container_log_mirror_chunks_end_on_line_boundaries(t *testing.T) {
	sink := &chunkSink{}
	ns := &needleSet{}
	m := newContainerMirror(sink, 1<<20, ns.get, slog.New(slog.NewTextHandler(&safeBuf{}, nil)))
	var lines strings.Builder
	for i := 0; lines.Len() <= 2*mirrorLineMax; i++ {
		fmt.Fprintf(&lines, "shared stalled line %d\n", i)
	}
	m.Write([]byte(lines.String() + "shared s"))
	m.Write([]byte("talled line tail\n"))
	m.Close(true, 5*time.Second)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.chunks) == 0 {
		t.Fatal("nothing reached the sink")
	}
	for i, c := range sink.chunks {
		if !strings.HasSuffix(c, "\n") {
			t.Fatalf("chunk %d ends mid-line: %q", i, c[max(0, len(c)-30):])
		}
	}
	if got := strings.Join(sink.chunks, ""); got != lines.String()+"shared stalled line tail\n" {
		t.Fatalf("output changed: %d bytes", len(got))
	}
}

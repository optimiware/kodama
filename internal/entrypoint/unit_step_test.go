package entrypoint

// Entrypoint unit qualification (T-082): the command/result/log coordination
// behind the existing Runtime/Uploader ports, driven by the scriptable fake
// runtime, a real child process in a temporary Pod layout and, where Git is
// involved, a local HTTPS repository. No image or container is provisioned and
// nothing here is production evidence (real image execution is T-026).
//
// Requirements exercised: PX-05 (transitions are decided once and replays
// converge), PX-07 (retry is bounded; an unknown outcome is replayed, never
// re-decided), VS-03 (resolution is limited to declared references under active
// authority) and VS-07 (pinned secret versions).

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// timeline is the runtime's operations in the order the entrypoint issued them.
// A reservation is shown with the artifact name it declared.
func (u *unit) timeline() []string {
	u.rt.mu.Lock()
	defer u.rt.mu.Unlock()
	var out []string
	n := 0
	for _, c := range u.rt.calls {
		if c == "reserveArtifact" {
			c += ":" + u.rt.reserved[n].Name
			n++
		}
		out = append(out, c)
	}
	return out
}

// at returns the index of the first timeline entry equal to op at or after
// from, failing the test when it is absent.
func (u *unit) at(tl []string, op string, from int) int {
	u.t.Helper()
	if i := indexOf(tl, op, from); i >= 0 {
		return i
	}
	u.t.Fatalf("%q not found (from %d) in %v", op, from, tl)
	return -1
}

// markerLines counts the lines the command appended to its marker file; each
// launch of the command appends exactly one.
func markerLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// withMarker makes the command append one line to a marker file per launch and
// returns the file's path.
func (u *unit) withMarker() string {
	marker := filepath.Join(u.root, "launches")
	u.rt.ctx.Command.Env["KODAMA_TEST_MARKER"] = marker
	return marker
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// repoFixture serves a private repository whose pinned commit is also the
// expected tip, and points the unit's context at it.
type repoFixture struct {
	gs     *gitServer
	commit string
}

const (
	scopeUser = "kodama-reader"
	scopePass = "co-9d41c7e2-sCOPED-pw"
)

func (u *unit) withRepo(access runtimev1.WorkspaceAccess, user, pass string) *repoFixture {
	u.t.Helper()
	requireGit(u.t)
	root := u.t.TempDir()
	first, _ := seedRepo(u.t, root)
	gs := newGitServer(u.t, root, user, pass)
	u.cfg.GitCAFile = gs.CAFile
	src := &runtimev1.Source{RepositoryURL: gs.URL + "/repo.git", Commit: runtimev1.GitCommit(first)}
	if access == runtimev1.AccessWrite {
		branch := "refs/heads/main"
		src.Branch, src.ExpectedTip = &branch, ptr(runtimev1.GitCommit(first))
	}
	u.rt.ctx.Workspace = runtimev1.Workspace{Access: access, Source: src}
	if pass != "" {
		if err := os.WriteFile(u.cfg.CheckoutCredentialFile, []byte(user+":"+pass+"\n"), 0o600); err != nil {
			u.t.Fatal(err)
		}
	}
	return &repoFixture{gs: gs, commit: first}
}

func Test_unit_step_lifecycle(t *testing.T) {
	t.Run("a completed command reports its log, outputs, source and result in that order", func(t *testing.T) {
		u := newUnit(t, `echo built > "$KODAMA_OUTPUTS_DIR/out.txt"; echo hello-log; echo more > new.txt`)
		u.withRepo(runtimev1.AccessWrite, "", "")
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "out", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/out.txt")}}
		u.wantExit(u.run(), ExitOK)

		tl := u.timeline()
		want := []string{"admit", "getContext"}
		if !reflect.DeepEqual(tl[:2], want) {
			t.Fatalf("timeline starts %v, want %v", tl[:2], want)
		}
		iLog := u.at(tl, "reserveArtifact:command.log", 0)
		iMeta := u.at(tl, "reserveArtifact:command.log.meta.json", iLog)
		iOut := u.at(tl, "reserveArtifact:out", iMeta)
		iBundle := u.at(tl, "reserveArtifact:source.bundle", iOut)
		iSource := u.at(tl, "proposeSource", iBundle)
		iResult := u.at(tl, "submitResult", iSource)
		if iResult != len(tl)-1 || u.rt.count("submitResult") != 1 {
			t.Fatalf("the result is not the single final operation: %v", tl)
		}
		r := u.lastResult()
		if r.Outcome != runtimev1.OutcomeCompleted || r.ExitCode == nil || *r.ExitCode != 0 || r.Reason != nil {
			t.Fatalf("result = %+v", r)
		}
		if r.SourceCommit == nil || *r.SourceCommit != u.rt.proposals[0].Commit {
			t.Fatalf("sourceCommit %v is not the proposed commit %s", r.SourceCommit, u.rt.proposals[0].Commit)
		}
		// The result cites the verified log, then its sidecar.
		if len(r.DiagnosticArtifactIDs) != 2 || r.DiagnosticArtifactIDs[0] != reservedID(0) || r.DiagnosticArtifactIDs[1] != reservedID(1) {
			t.Fatalf("diagnostics = %v", r.DiagnosticArtifactIDs)
		}
		wantBoundSidecar(t, u, contract.Complete)
		if b, _, _, _ := u.diagnostic("command.log"); !strings.Contains(string(b), "hello-log") {
			t.Fatalf("command.log = %q", b)
		}
	})

	t.Run("a failing command reports exit before anything it produced is collected", func(t *testing.T) {
		u := newUnit(t, `echo partial > "$KODAMA_OUTPUTS_DIR/out.txt"; echo boom >&2; echo more > new.txt; exit 3`)
		u.withRepo(runtimev1.AccessWrite, "", "")
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "out", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/out.txt")}}
		u.wantExit(u.run(), ExitOK)

		tl := u.timeline()
		for _, op := range []string{"reserveArtifact:out", "reserveArtifact:source.bundle", "proposeSource"} {
			if indexOf(tl, op, 0) >= 0 {
				t.Fatalf("%s happened after a failed command: %v", op, tl)
			}
		}
		iLog := u.at(tl, "reserveArtifact:command.log", 0)
		iNotes := u.at(tl, "reserveArtifact:entrypoint.log", iLog)
		u.at(tl, "submitResult", iNotes)
		r := u.lastResult()
		wantReason(t, r, ReasonExitNonzero)
		if r.ExitCode == nil || *r.ExitCode != 3 || r.SourceCommit != nil || len(r.Outputs) != 0 {
			t.Fatalf("result = %+v", r)
		}
		// The log is complete: the command's failure is not a capture failure.
		wantBoundSidecar(t, u, contract.Complete)
		if b, _, _, _ := u.diagnostic("command.log"); !strings.Contains(string(b), "boom") {
			t.Fatalf("command.log = %q", b)
		}
	})

	t.Run("exit zero without the declared output never completes", func(t *testing.T) {
		u := newUnit(t, `echo done`)
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "out", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/missing.txt")}}
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		wantReason(t, r, ReasonOutputMissing)
		if r.ExitCode == nil || *r.ExitCode != 0 {
			t.Fatalf("the command's own exit status is not preserved: %+v", r)
		}
		tl := u.timeline()
		u.at(tl, "submitResult", u.at(tl, "reserveArtifact:command.log", 0))
	})

	t.Run("an interrupt stops the command and still submits one interrupted result after the log", func(t *testing.T) {
		u := newUnit(t, `echo started-line; echo run >> "$KODAMA_TEST_MARKER"; sleep 30; echo late >> "$KODAMA_TEST_MARKER"`)
		marker := u.withMarker()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		step := u.build()
		go func() { done <- step.Run(ctx) }()
		waitFor(t, "the command to start", func() bool { return markerLines(t, marker) == 1 })
		began := time.Now()
		cancel()
		select {
		case code := <-done:
			u.wantExit(code, ExitOK)
		case <-time.After(30 * time.Second):
			t.Fatal("the interrupted step did not finish")
		}
		if d := time.Since(began); d > 10*time.Second {
			t.Fatalf("the command was not stopped promptly: %s", d)
		}
		if markerLines(t, marker) != 1 {
			t.Fatal("the command continued after the interrupt")
		}
		r := u.lastResult()
		wantReason(t, r, ReasonInterrupted)
		if r.ExitCode != nil || r.SourceCommit != nil || len(r.Outputs) != 0 {
			t.Fatalf("result = %+v", r)
		}
		tl := u.timeline()
		iLog := u.at(tl, "reserveArtifact:command.log", 0)
		u.at(tl, "submitResult", u.at(tl, "reserveArtifact:command.log.meta.json", iLog))
		if tl[len(tl)-1] != "submitResult" || u.rt.count("submitResult") != 1 {
			t.Fatalf("the result is not the single final operation: %v", tl)
		}
		// A stopped command's log is honestly partial.
		wantBoundSidecar(t, u, contract.Partial, contract.ReasonInterrupted)
		if b, _, _, _ := u.diagnostic("command.log"); !strings.Contains(string(b), "started-line") {
			t.Fatalf("output before the interrupt was lost: %q", b)
		}
	})

	t.Run("revoked authority stops the command and nothing further is uploaded or submitted", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"; sleep 30; echo late >> "$KODAMA_TEST_MARKER"`)
		marker := u.withMarker()
		u.rt.admit = func(runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
			return runtimev1.AdmitResponse{Token: tokenInfo("rt_unit_token_0123456789abcdef", 100*time.Millisecond, time.Hour), Context: u.rt.ctx}, nil
		}
		u.rt.renew = func() (runtimev1.TokenResponse, error) {
			if markerLines(t, marker) == 0 {
				// Not launched yet: keep the authority alive until it ran.
				return runtimev1.TokenResponse{Token: tokenInfo("rt_unit_renewed_0123456789abcdef", 100*time.Millisecond, time.Hour)}, nil
			}
			return runtimev1.TokenResponse{}, apiErr("renewToken", 403, runtimev1.CodeAttemptRevoked)
		}
		began := time.Now()
		u.wantExit(u.run(), ExitRevoked)
		if d := time.Since(began); d > 15*time.Second {
			t.Fatalf("the revoked command was not stopped promptly: %s", d)
		}
		for _, op := range []string{"reserveArtifact", "completeArtifact", "proposeSource", "submitResult"} {
			if n := u.rt.count(op); n != 0 {
				t.Fatalf("%s called %d times after revocation: %v", op, n, u.rt.calls)
			}
		}
		if markerLines(t, marker) != 1 {
			t.Fatal("the command ran past revocation")
		}
		if len(u.up.objects) != 0 {
			t.Fatalf("objects were uploaded after revocation: %d", len(u.up.objects))
		}
	})

	t.Run("a command that outlives its timeout is stopped, logged as partial and reported once", func(t *testing.T) {
		u := newUnit(t, `echo before-timeout; sleep 30`)
		u.rt.ctx.Limits.TimeoutSeconds = 1
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		wantReason(t, r, ReasonTimeout)
		wantBoundSidecar(t, u, contract.Partial, contract.ReasonInterrupted)
		if u.rt.count("submitResult") != 1 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
	})

	t.Run("a context that disagrees with the admitted one never launches", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"`)
		marker := u.withMarker()
		other := u.rt.ctx
		other.StepID = "other-step"
		u.rt.fetch = func() (runtimev1.Context, error) { return other, nil }
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonContextMismatch)
		if markerLines(t, marker) != 0 {
			t.Fatal("the command launched from a context that was not the admitted one")
		}
	})
}

func Test_unit_step_retry(t *testing.T) {
	t.Run("a lost result response replays the identical result and never reruns the command", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"; echo ok`)
		marker := u.withMarker()
		u.rt.submit = func(n int, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
			if n <= 3 {
				// The server may or may not have stored it: the response is lost.
				return runtimev1.ResultResponse{}, &TransportError{Op: "submitResult", Err: errors.New("connection reset")}
			}
			return u.rt.ack(req), nil
		}
		u.wantExit(u.run(), ExitOK)
		if got := markerLines(t, marker); got != 1 {
			t.Fatalf("the command ran %d times", got)
		}
		if n := u.rt.count("submitResult"); n != 4 {
			t.Fatalf("submissions = %d, want 4", n)
		}
		for i, req := range u.rt.submitted {
			if !reflect.DeepEqual(req, u.rt.submitted[0]) {
				t.Fatalf("submission %d differs from the first:\n%+v\n%+v", i, req, u.rt.submitted[0])
			}
		}
		// No other effect is repeated: one admission, one context fetch, one
		// reservation per diagnostic.
		if u.rt.count("admit") != 1 || u.rt.count("getContext") != 1 || u.rt.count("reserveArtifact") != 2 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
	})

	t.Run("an acknowledgment for another attempt or hash is never treated as success", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.rt.submit = func(_ int, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
			ack := u.rt.ack(req)
			ack.AttemptID = "att_someone_else"
			return ack, nil
		}
		u.wantExit(u.run(), ExitFinalization)
		u.rt.submit = func(_ int, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
			ack := u.rt.ack(req)
			ack.ResultHash = runtimev1.Digest("sha256:" + strings.Repeat("0", 64))
			return ack, nil
		}
		u.wantExit(u.run(), ExitFinalization)
		if u.rt.count("submitResult") != 2 {
			t.Fatalf("an unmatched acknowledgment was retried: %v", u.rt.calls)
		}
	})

	t.Run("a rejected result is replaced once by a failed result without rerunning the command", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"`)
		marker := u.withMarker()
		u.rt.submit = func(n int, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
			if n == 1 {
				return runtimev1.ResultResponse{}, apiErr("submitResult", 422, runtimev1.CodeOutputInvalid)
			}
			return u.rt.ack(req), nil
		}
		u.wantExit(u.run(), ExitOK)
		if markerLines(t, marker) != 1 || u.rt.count("submitResult") != 2 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
		first, second := u.rt.submitted[0], u.rt.submitted[1]
		if first.Result.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("first = %+v", first.Result)
		}
		wantReason(t, second.Result, ReasonResultRejected)
		if first.ResultHash == second.ResultHash {
			t.Fatal("the replacement has the rejected result's hash")
		}
	})

	t.Run("a conflicting stored result is never rewritten or retried", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"`)
		marker := u.withMarker()
		u.rt.submit = func(int, runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
			return runtimev1.ResultResponse{}, apiErr("submitResult", 409, runtimev1.CodeResultConflict)
		}
		u.wantExit(u.run(), ExitResultConflict)
		if markerLines(t, marker) != 1 || u.rt.count("submitResult") != 1 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
	})

	t.Run("a restarted attempt cannot be admitted again and does not rerun the command", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"`)
		marker := u.withMarker()
		u.wantExit(u.run(), ExitOK)
		if markerLines(t, marker) != 1 {
			t.Fatal("setup: the command did not run once")
		}
		// The container restarts: the same attempt presents its consumed
		// admission credential again.
		u.rt.admit = func(runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
			return runtimev1.AdmitResponse{}, apiErr("admit", 409, runtimev1.CodeAdmissionConsumed)
		}
		u.wantExit(u.run(), ExitAdmission)
		if markerLines(t, marker) != 1 {
			t.Fatal("the command was rerun by a restarted entrypoint")
		}
		if u.rt.count("getContext") != 1 || u.rt.count("submitResult") != 1 {
			t.Fatalf("a refused admission still acted: %v", u.rt.calls)
		}
	})

	t.Run("a replayed admission carries a fresh nonce for the same attempt identity", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		var nonces []string
		u.rt.admit = func(req runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
			nonces = append(nonces, req.Nonce)
			if req.AdmissionCredential != unitAdmission || req.Job != unitJob {
				t.Errorf("admission identity = %+v", req)
			}
			return runtimev1.AdmitResponse{Token: tokenInfo("rt_unit_token_0123456789abcdef", time.Hour, 2*time.Hour), Context: u.rt.ctx}, nil
		}
		u.wantExit(u.run(), ExitOK)
		u.wantExit(u.run(), ExitOK)
		if len(nonces) != 2 || nonces[0] == "" || nonces[0] == nonces[1] {
			t.Fatalf("nonces = %v", nonces)
		}
		for _, r := range u.rt.submitted {
			if r.Result.Outcome != runtimev1.OutcomeCompleted {
				t.Fatalf("result = %+v", r.Result)
			}
		}
	})

	t.Run("lost log-upload and result responses replay identical declarations and keep one artifact per name", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"; echo log-line`)
		marker := u.withMarker()
		srv := newLogServer(u.rt.ctx)
		srv.lose = func(op, name string, n int) bool { return n == 1 }
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
		if got := markerLines(t, marker); got != 1 {
			t.Fatalf("the command ran %d times", got)
		}
		if len(srv.bad) != 0 || len(srv.results) != 1 || len(srv.byID) != 2 {
			t.Fatalf("bad=%v results=%d artifacts=%d", srv.bad, len(srv.results), len(srv.byID))
		}
		cited := srv.results[0].Result.DiagnosticArtifactIDs
		if len(cited) != 2 {
			t.Fatalf("diagnostics = %v", cited)
		}
		for _, id := range cited {
			a := srv.byID[id]
			if a == nil || !a.complete {
				t.Fatalf("the result cites %s, which is not a completed artifact", id)
			}
			for _, b := range a.bodies {
				if string(b) != string(a.bodies[0]) {
					t.Fatalf("%s: a replay sent different bytes", a.req.Name)
				}
			}
		}
	})

	t.Run("source publication replays the identical proposal and never rebuilds or reuploads the bundle", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"; echo change > new.txt`)
		marker := u.withMarker()
		u.withRepo(runtimev1.AccessWrite, "", "")
		u.rt.propose = func(n int, req runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
			switch n {
			case 1:
				return runtimev1.SourceProposalResponse{}, &TransportError{Op: "proposeSource", Err: errors.New("reset")}
			case 2:
				return runtimev1.SourceProposalResponse{Status: runtimev1.PublishPending, Commit: req.Commit}, nil
			}
			c := req.Commit
			return runtimev1.SourceProposalResponse{Status: runtimev1.PublishAccepted, Commit: req.Commit, AcceptedCommit: &c}, nil
		}
		u.wantExit(u.run(), ExitOK)
		if markerLines(t, marker) != 1 || len(u.rt.proposals) != 3 {
			t.Fatalf("launches=%d proposals=%d", markerLines(t, marker), len(u.rt.proposals))
		}
		bundles := 0
		for _, q := range u.rt.reserved {
			if q.Role == runtimev1.RoleBundle {
				bundles++
			}
		}
		if bundles != 1 {
			t.Fatalf("bundle reserved %d times", bundles)
		}
		for i, p := range u.rt.proposals {
			if !reflect.DeepEqual(p, u.rt.proposals[0]) {
				t.Fatalf("proposal %d differs from the first: %+v vs %+v", i, p, u.rt.proposals[0])
			}
		}
		r := u.lastResult()
		if r.Outcome != runtimev1.OutcomeCompleted || r.SourceCommit == nil || *r.SourceCommit != u.rt.proposals[0].Commit {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("a publication conflict is final: no retry, no result of success, no rerun", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"; echo change > new.txt`)
		marker := u.withMarker()
		u.withRepo(runtimev1.AccessWrite, "", "")
		u.rt.propose = func(int, runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
			return runtimev1.SourceProposalResponse{}, apiErr("proposeSource", 409, runtimev1.CodePublicationConflict)
		}
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonPublicationConflict)
		if markerLines(t, marker) != 1 || len(u.rt.proposals) != 1 {
			t.Fatalf("launches=%d proposals=%d", markerLines(t, marker), len(u.rt.proposals))
		}
	})
}

func Test_unit_step_secret_scope(t *testing.T) {
	forms := jcForms(scopeUser, scopePass)

	// uploaded is everything the runtime API was given: every reservation
	// declaration, every stored object, every result and every proposal.
	uploaded := func(u *unit) []byte {
		u.rt.mu.Lock()
		defer u.rt.mu.Unlock()
		var out []byte
		add := func(v any) { out = append(out, fmt.Sprintf("%+v\n", v)...) }
		for _, r := range u.rt.reserved {
			add(r)
		}
		for _, r := range u.rt.submitted {
			add(r)
		}
		for _, r := range u.rt.proposals {
			add(r)
		}
		u.up.mu.Lock()
		defer u.up.mu.Unlock()
		for _, o := range u.up.objects {
			out = append(out, o...)
			out = append(out, '\n')
		}
		return out
	}

	t.Run("the checkout credential reaches only the fetch and none of the retained output", func(t *testing.T) {
		// The command is hostile: it prints the credential file, its whole
		// environment, the repository configuration and a Basic header it
		// recomputes, and it keeps its environment as an output.
		u := newUnit(t, "")
		cred := u.cfg.CheckoutCredentialFile
		u.rt.ctx = unitContext(fmt.Sprintf(`env > "$KODAMA_OUTPUTS_DIR/env.txt"
cat %[1]q
cat .git/config
env
printf 'Authorization: Basic %%s\n' "$(tr -d '\n' < %[1]q | base64 | tr -d '\n')"
exit 0`, cred))
		repo := u.withRepo(runtimev1.AccessRead, scopeUser, scopePass)
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "env", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/env.txt")}}
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		if r.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("result = %+v\n%s", r, u.logs.String())
		}

		// The pinned commit was fetched with exactly the issued credential.
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(scopeUser+":"+scopePass))
		repo.gs.mu.Lock()
		authorized := 0
		for _, a := range repo.gs.auths {
			switch a {
			case want:
				authorized++
			case "":
			default:
				t.Errorf("the Git server saw an unexpected Authorization header")
			}
		}
		repo.gs.mu.Unlock()
		if authorized == 0 {
			t.Fatal("the Git server never received the checkout credential")
		}
		if head := gitRun(t, u.cfg.WorkspaceDir, "rev-parse", "HEAD"); head != repo.commit {
			t.Fatalf("HEAD = %s, want %s", head, repo.commit)
		}

		// The command never inherited it: its environment, kept as an output,
		// carries neither the credential nor the knobs that deliver it.
		envOut, _, _, ok := u.diagnostic("env")
		if !ok {
			t.Fatalf("the environment output was not uploaded: %v", u.timeline())
		}
		jcNoLeak(t, "the command's environment", envOut, forms)
		for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY", "GIT_CONFIG_VALUE", "GIT_SSL_CAINFO", "KODAMA_CHECKOUT_CREDENTIAL", "KODAMA_STEP_SERVER_URL", "KODAMA_STEP_CHECKOUT", "extraHeader"} {
			if strings.Contains(string(envOut), name) {
				t.Errorf("the command inherited %s", name)
			}
		}

		// The retained command log is the redacted output of a command that
		// tried to print everything.
		logBytes, _, _, ok := u.diagnostic("command.log")
		if !ok {
			t.Fatal("no command.log")
		}
		jcNoLeak(t, "command.log", logBytes, forms)
		if !strings.Contains(string(logBytes), "REDACT") {
			t.Fatalf("the credential the command printed left no redaction marker: %q", logBytes)
		}
		wantBoundSidecar(t, u, contract.Complete)

		// Nothing the entrypoint wrote or sent carries it, and the workspace
		// repository does not persist it.
		jcNoLeak(t, "runtime API traffic and uploads", uploaded(u), forms)
		for _, name := range []string{"command.log", "command.log.meta.json", "entrypoint.log"} {
			if b, _, _, ok := u.diagnostic(name); ok {
				jcNoLeak(t, name, b, forms)
			}
		}
		jcNoLeak(t, "entrypoint logs", []byte(u.logs.String()), forms)
		for path, b := range jcWalk(t, filepath.Join(u.cfg.WorkspaceDir, ".git")) {
			jcNoLeak(t, path, b, forms)
		}
		for path, b := range jcWalk(t, u.cfg.ScratchDir) {
			jcNoLeak(t, path, b, forms)
		}
	})

	t.Run("an anonymous checkout sends no credential at all", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		repo := u.withRepo(runtimev1.AccessRead, "", "")
		u.wantExit(u.run(), ExitOK)
		wantCompleted(t, u)
		repo.gs.mu.Lock()
		defer repo.gs.mu.Unlock()
		for _, a := range repo.gs.auths {
			if a != "" {
				t.Fatal("an Authorization header was sent without an issued credential")
			}
		}
	})

	t.Run("a refused checkout credential fails the checkout without launching or leaking it", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"`)
		marker := u.withMarker()
		u.withRepo(runtimev1.AccessRead, scopeUser, scopePass)
		wrong := "wrong-" + scopePass
		if err := os.WriteFile(u.cfg.CheckoutCredentialFile, []byte(scopeUser+":"+wrong+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonCheckoutFailed)
		if markerLines(t, marker) != 0 {
			t.Fatal("the command launched without a checkout")
		}
		bad := jcForms(scopeUser, wrong)
		jcNoLeak(t, "uploads", uploaded(u), bad)
		for _, name := range []string{"entrypoint.log", "command.log"} {
			if b, _, _, ok := u.diagnostic(name); ok {
				jcNoLeak(t, name, b, bad)
				jcNoLeak(t, name, b, forms)
			}
		}
		jcNoLeak(t, "entrypoint logs", []byte(u.logs.String()), bad)
	})

	t.Run("a command that commits the checkout credential cannot publish it", func(t *testing.T) {
		u := newUnit(t, "")
		script := fmt.Sprintf(`cut -d: -f2- %q > leak.txt`, u.cfg.CheckoutCredentialFile)
		u.rt.ctx = unitContext(script)
		u.withRepo(runtimev1.AccessWrite, scopeUser, scopePass)
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonSecretInSource)
		if n := len(u.rt.proposals); n != 0 {
			t.Fatalf("a source containing the credential was proposed %d times", n)
		}
		for _, q := range u.rt.reserved {
			if q.Role == runtimev1.RoleBundle {
				t.Fatal("a bundle containing the credential was uploaded")
			}
		}
		jcNoLeak(t, "uploads", uploaded(u), forms)
		for _, name := range []string{"command.log", "entrypoint.log"} {
			if b, _, _, ok := u.diagnostic(name); ok {
				jcNoLeak(t, name, b, forms)
			}
		}
	})

	t.Run("secret resolution is limited to the declared pinned references and never offered the checkout credential", func(t *testing.T) {
		const value = "declared-secret-value-5544"
		u := newUnit(t, `echo "value=$DB_PASS"; echo "$DB_PASS" > "$KODAMA_SECRETS_DIR/copy" 2>/dev/null; true`)
		u.withRepo(runtimev1.AccessRead, scopeUser, scopePass)
		b := runtimev1.SecretBinding{Ref: "secret://project/db-pass", Version: 7, Name: "db-pass", Delivery: runtimev1.DeliveryEnv}
		u.rt.ctx.Secrets = []runtimev1.SecretBinding{b}
		var requests []runtimev1.SecretResolveRequest
		u.rt.resolve = func(r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
			requests = append(requests, r)
			if r.Ref != b.Ref {
				return runtimev1.SecretResolveResponse{}, apiErr("resolveSecret", 403, domain.CodeForbidden)
			}
			return runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
				ValueBase64: base64.StdEncoding.EncodeToString([]byte(value))}, nil
		}
		u.wantExit(u.run(), ExitOK)
		wantCompleted(t, u)
		if len(requests) != 1 || requests[0].Ref != b.Ref {
			t.Fatalf("resolution requests = %+v, want exactly the declared reference", requests)
		}
		if requests[0].Version != b.Version {
			t.Fatalf("the request asked for version %d, want the pinned %d", requests[0].Version, b.Version)
		}
		for _, r := range requests {
			jcNoLeak(t, "secret resolution request", []byte(fmt.Sprintf("%+v", r)), forms)
		}
		logBytes, _, _, _ := u.diagnostic("command.log")
		if strings.Contains(string(logBytes), value) {
			t.Fatalf("the secret reached command.log: %q", logBytes)
		}
		jcNoLeak(t, "uploads", uploaded(u), []string{value})
		jcNoLeak(t, "entrypoint logs", []byte(u.logs.String()), append([]string{value}, forms...))
		// The delivery directory, and the checkout credential file, are not
		// part of what a declared env delivery exposes.
		if fileExists(filepath.Join(u.cfg.SecretsDir, "copy")) {
			t.Fatal("a file appeared in the secrets volume root")
		}
	})

	t.Run("a rotated or foreign secret answer is refused before the command launches", func(t *testing.T) {
		cases := map[string]func(b runtimev1.SecretBinding, r *runtimev1.SecretResolveResponse){
			"rotated version": func(b runtimev1.SecretBinding, r *runtimev1.SecretResolveResponse) { r.Version = b.Version + 1 },
			"foreign ref":     func(b runtimev1.SecretBinding, r *runtimev1.SecretResolveResponse) { r.Ref = "secret://project/other" },
			"other name":      func(b runtimev1.SecretBinding, r *runtimev1.SecretResolveResponse) { r.Name = "other" },
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"`)
				marker := u.withMarker()
				b := runtimev1.SecretBinding{Ref: "secret://project/db-pass", Version: 3, Name: "db-pass", Delivery: runtimev1.DeliveryEnv}
				u.rt.ctx.Secrets = []runtimev1.SecretBinding{b}
				u.rt.resolve = func(runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
					resp := runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
						ValueBase64: base64.StdEncoding.EncodeToString([]byte("some-secret-value-1"))}
					mutate(b, &resp)
					return resp, nil
				}
				u.wantExit(u.run(), ExitOK)
				wantReason(t, u.lastResult(), ReasonSecretUnavailable)
				if markerLines(t, marker) != 0 {
					t.Fatal("the command launched with an unpinned secret")
				}
			})
		}
	})

	t.Run("revoked authority during secret delivery launches nothing and submits nothing", func(t *testing.T) {
		u := newUnit(t, `echo run >> "$KODAMA_TEST_MARKER"`)
		marker := u.withMarker()
		b := runtimev1.SecretBinding{Ref: "secret://project/db-pass", Version: 1, Name: "db-pass", Delivery: runtimev1.DeliveryEnv}
		u.rt.ctx.Secrets = []runtimev1.SecretBinding{b}
		u.rt.resolve = func(runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
			return runtimev1.SecretResolveResponse{}, apiErr("resolveSecret", 403, runtimev1.CodeAttemptRevoked)
		}
		u.wantExit(u.run(), ExitRevoked)
		if markerLines(t, marker) != 0 || u.rt.count("submitResult") != 0 || u.rt.count("reserveArtifact") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
	})
}

func wantCompleted(t *testing.T, u *unit) {
	t.Helper()
	if r := u.lastResult(); r.Outcome != runtimev1.OutcomeCompleted {
		t.Fatalf("result = %+v\nnotes/logs:\n%s", r, u.logs.String())
	}
}

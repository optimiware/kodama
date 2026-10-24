package entrypoint

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func newResultAttempt(t *testing.T, mutate func(*unit)) (*attempt, *unit) {
	t.Helper()
	u := newUnit(t, `exit 0`)
	if mutate != nil {
		mutate(u)
	}
	s := u.build()
	return &attempt{
		s: s, parent: context.Background(), runCtx: context.Background(), c: u.rt.ctx,
		artifacts: map[domain.ArtifactID]runtimev1.ArtifactRef{}, names: map[string]bool{},
	}, u
}

func TestResultRequestNormalizesAndHashesOnce(t *testing.T) {
	a, _ := newResultAttempt(t, nil)
	a.artifacts["art_log"] = runtimev1.ArtifactRef{ID: "art_log", Scope: a.c.Scope(), Attempt: a.c.AttemptID, Kind: runtimev1.KindFile, Usable: true}
	a.diag = []domain.ArtifactID{"art_log"}
	zero := int64(0)
	req, err := a.resultRequest(verdict{outcome: runtimev1.OutcomeCompleted, exit: &zero})
	if err != nil {
		t.Fatal(err)
	}
	r := req.Result
	if r.Outputs == nil || r.DiagnosticArtifactIDs == nil || len(r.DiagnosticArtifactIDs) != 1 || r.Reason != nil || r.SourceCommit != nil {
		t.Fatalf("result = %+v", r)
	}
	want, err := runtimev1.ResultHash(r)
	if err != nil || req.ResultHash != want {
		t.Fatalf("hash = %s, want %s (%v)", req.ResultHash, want, err)
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("request does not validate: %v", err)
	}
	// Deterministic: the same verdict normalizes to the same hash.
	again, _ := a.resultRequest(verdict{outcome: runtimev1.OutcomeCompleted, exit: &zero})
	if again.ResultHash != req.ResultHash {
		t.Fatal("the hash is not deterministic")
	}
	// The hash covers the diagnostics list: it identifies the exact payload.
	a.diag = nil
	other, _ := a.resultRequest(verdict{outcome: runtimev1.OutcomeCompleted, exit: &zero})
	if other.ResultHash == req.ResultHash {
		t.Fatal("different payloads share a hash")
	}
}

func TestResultRequestRefusesWhatTheContractWouldRefuse(t *testing.T) {
	a, _ := newResultAttempt(t, func(u *unit) {
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "n", Type: runtimev1.OutputInteger}}
	})
	if _, err := a.resultRequest(verdict{outcome: runtimev1.OutcomeCompleted}); err == nil {
		t.Fatal("a completed result without its declared output was accepted")
	}
	if _, err := a.resultRequest(verdict{outcome: runtimev1.OutcomeFailed}); err == nil {
		t.Fatal("a failed result without a reason was accepted")
	}
	if _, err := a.resultRequest(failed("Bad Reason", nil)); err == nil {
		t.Fatal("an invalid reason code was accepted")
	}
}

func TestFinalizeReplacesAnInvalidResultWithAFailedOne(t *testing.T) {
	a, u := newResultAttempt(t, func(u *unit) {
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "n", Type: runtimev1.OutputInteger}}
	})
	zero := int64(0)
	if code := a.finalize(verdict{outcome: runtimev1.OutcomeCompleted, exit: &zero}); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	r := u.lastResult()
	wantReason(t, r, ReasonOutputInvalid)
	if r.ExitCode == nil || *r.ExitCode != 0 || len(r.Outputs) != 0 {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(a.notes.String(), "invalid") {
		t.Fatalf("notes = %q", a.notes.String())
	}
}

func TestFinalizeSubmissionMatrix(t *testing.T) {
	transport := &TransportError{Op: "submitResult", Err: errors.New("EOF")}
	reject := apiErr("submitResult", 422, runtimev1.CodeOutputInvalid)
	zero := int64(0)

	cases := []struct {
		name       string
		submit     func(n int, req runtimev1.ResultRequest, ack func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error)
		timeout    time.Duration
		want       int
		wantSubmit int
		identical  bool   // every submission is byte-identical
		final      string // reason of the last submission, if it differs from the first
	}{
		{name: "acknowledged", want: ExitOK, wantSubmit: 1, identical: true,
			submit: func(_ int, r runtimev1.ResultRequest, ack func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				return ack(r), nil
			}},
		{name: "lost responses are retried with the same payload", want: ExitOK, wantSubmit: 4, identical: true,
			submit: func(n int, r runtimev1.ResultRequest, ack func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				if n < 4 {
					return runtimev1.ResultResponse{}, transport
				}
				return ack(r), nil
			}},
		{name: "acknowledgment for another hash", want: ExitFinalization, wantSubmit: 1,
			submit: func(_ int, r runtimev1.ResultRequest, ack func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				a := ack(r)
				a.ResultHash = runtimev1.Digest("sha256:" + strings.Repeat("0", 64))
				return a, nil
			}},
		{name: "acknowledgment for another attempt", want: ExitFinalization, wantSubmit: 1,
			submit: func(_ int, r runtimev1.ResultRequest, ack func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				a := ack(r)
				a.AttemptID = "att_other"
				return a, nil
			}},
		{name: "conflict is never rewritten", want: ExitResultConflict, wantSubmit: 1,
			submit: func(int, runtimev1.ResultRequest, func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				return runtimev1.ResultResponse{}, apiErr("submitResult", 409, runtimev1.CodeResultConflict)
			}},
		{name: "conflict after a lost response", want: ExitResultConflict, wantSubmit: 2,
			submit: func(n int, _ runtimev1.ResultRequest, _ func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				if n == 1 {
					return runtimev1.ResultResponse{}, transport
				}
				return runtimev1.ResultResponse{}, apiErr("submitResult", 409, runtimev1.CodeResultConflict)
			}},
		{name: "revoked", want: ExitRevoked, wantSubmit: 1,
			submit: func(int, runtimev1.ResultRequest, func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				return runtimev1.ResultResponse{}, apiErr("submitResult", 403, runtimev1.CodeAttemptRevoked)
			}},
		{name: "bearer refused", want: ExitRevoked, wantSubmit: 1,
			submit: func(int, runtimev1.ResultRequest, func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				return runtimev1.ResultResponse{}, apiErr("submitResult", 401, domain.CodeInvalidCredential)
			}},
		{name: "attempt binding refused", want: ExitRevoked, wantSubmit: 1,
			submit: func(int, runtimev1.ResultRequest, func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				return runtimev1.ResultResponse{}, apiErr("submitResult", 403, domain.CodeForbidden)
			}},
		{name: "contract violation keeps the possibly accepted result", want: ExitFinalization, wantSubmit: 1,
			submit: func(int, runtimev1.ResultRequest, func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				return runtimev1.ResultResponse{}, &ProtocolError{Op: "submitResult", Err: errors.New("bad body")}
			}},
		{name: "rejection is replaced once by a failed result", want: ExitOK, wantSubmit: 2, final: ReasonResultRejected,
			submit: func(n int, r runtimev1.ResultRequest, ack func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				if n == 1 {
					return runtimev1.ResultResponse{}, reject
				}
				return ack(r), nil
			}},
		{name: "second rejection ends finalization", want: ExitFinalization, wantSubmit: 2, final: ReasonResultRejected,
			submit: func(int, runtimev1.ResultRequest, func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				return runtimev1.ResultResponse{}, reject
			}},
		{name: "persistent loss is a bounded finalization failure", want: ExitFinalization, timeout: 80 * time.Millisecond, identical: true,
			submit: func(int, runtimev1.ResultRequest, func(runtimev1.ResultRequest) runtimev1.ResultResponse) (runtimev1.ResultResponse, error) {
				return runtimev1.ResultResponse{}, transport
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, u := newResultAttempt(t, func(u *unit) {
				if tc.timeout > 0 {
					u.cfg.FinalizeTimeout = tc.timeout
				}
			})
			u.rt.submit = func(n int, r runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
				return tc.submit(n, r, u.rt.ack)
			}
			got := a.finalize(verdict{outcome: runtimev1.OutcomeCompleted, exit: &zero})
			if got != tc.want {
				t.Fatalf("exit = %d, want %d\nlogs:\n%s", got, tc.want, u.logs.String())
			}
			n := len(u.rt.submitted)
			if tc.wantSubmit != 0 && n != tc.wantSubmit {
				t.Fatalf("submissions = %d, want %d", n, tc.wantSubmit)
			}
			if tc.timeout > 0 && n < 2 {
				t.Fatalf("submissions = %d, want repeated attempts", n)
			}
			if tc.identical {
				for i, r := range u.rt.submitted {
					if r.ResultHash != u.rt.submitted[0].ResultHash || r.Result.Outcome != u.rt.submitted[0].Result.Outcome {
						t.Fatalf("submission %d differs from the first", i+1)
					}
				}
			}
			if tc.final != "" {
				wantReason(t, u.rt.submitted[n-1].Result, tc.final)
				if u.rt.submitted[n-1].ResultHash == u.rt.submitted[0].ResultHash {
					t.Fatal("the replacement result kept the rejected hash")
				}
			}
		})
	}
}

func TestFinalizeAfterLostAuthoritySubmitsNothing(t *testing.T) {
	a, u := newResultAttempt(t, nil)
	run, cancel := context.WithCancelCause(context.Background())
	cancel(errAuthorityLost)
	a.runCtx = run
	if code := a.finalize(verdict{outcome: runtimev1.OutcomeCompleted}); code != ExitRevoked {
		t.Fatalf("exit %d", code)
	}
	if len(u.rt.calls) != 0 {
		t.Fatalf("calls = %v", u.rt.calls)
	}
}

func TestFinalizeUploadsNotesAsDiagnosticAndCitesThem(t *testing.T) {
	a, u := newResultAttempt(t, nil)
	a.note("the command exited with status %d", 3)
	zero := int64(0)
	if code := a.finalize(failed(ReasonExitNonzero, &zero)); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	r := u.lastResult()
	if len(r.DiagnosticArtifactIDs) != 1 {
		t.Fatalf("result = %+v", r)
	}
	if got := string(u.up.object(r.DiagnosticArtifactIDs[0])); !strings.Contains(got, "status 3") {
		t.Fatalf("notes = %q", got)
	}
	reserved := u.rt.reserved[0]
	if reserved.Name != "entrypoint.log" || reserved.Role != runtimev1.RoleDiagnostic {
		t.Fatalf("reservation = %+v", reserved)
	}
	entries, _ := os.ReadDir(u.cfg.ScratchDir)
	if len(entries) != 0 {
		t.Fatalf("scratch files left behind: %v", entries)
	}
}

func TestFinalizeNotesUploadFailureDoesNotBlockTheResult(t *testing.T) {
	a, u := newResultAttempt(t, nil)
	a.note("something")
	u.up.err = &TransportError{Op: "upload", Err: errors.New("down")}
	if code := a.finalize(failed(ReasonExitNonzero, nil)); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if r := u.lastResult(); len(r.DiagnosticArtifactIDs) != 0 {
		t.Fatalf("result cites a diagnostic that was never stored: %+v", r)
	}
}

func TestFinalizeRevokedDuringNotesUploadStopsEverything(t *testing.T) {
	a, u := newResultAttempt(t, nil)
	a.note("something")
	u.rt.reserve = func(runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
		return runtimev1.ArtifactReserveResponse{}, apiErr("reserveArtifact", 403, runtimev1.CodeAttemptRevoked)
	}
	if code := a.finalize(failed(ReasonExitNonzero, nil)); code != ExitRevoked {
		t.Fatalf("exit %d", code)
	}
	if u.rt.count("submitResult") != 0 {
		t.Fatalf("calls = %v", u.rt.calls)
	}
}

func TestAuthorityDeniedAndRejectedByServer(t *testing.T) {
	for _, c := range []struct {
		err    error
		denied bool
		reject bool
	}{
		{apiErr("x", 403, runtimev1.CodeAttemptRevoked), true, true},
		{apiErr("x", 401, domain.CodeInvalidCredential), true, true},
		{apiErr("x", 403, domain.CodeForbidden), true, true},
		{apiErr("x", 422, runtimev1.CodeOutputInvalid), false, true},
		{apiErr("x", 404, domain.CodeNotFound), false, true},
		{&APIError{Op: "x", Status: 500}, false, false},
		{&TransportError{Op: "x", Err: errors.New("e")}, false, false},
		{&ProtocolError{Op: "x", Err: errors.New("e")}, false, false},
		{nil, false, false},
	} {
		if authorityDenied(c.err) != c.denied || rejectedByServer(c.err) != c.reject {
			t.Errorf("%v: denied=%v reject=%v", c.err, authorityDenied(c.err), rejectedByServer(c.err))
		}
	}
}

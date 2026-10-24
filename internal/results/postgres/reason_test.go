package postgres_test

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/results/resultstest"
	"github.com/optimiware/kodama/test/support/layer"
)

// Test_result_output_failure_reason (result_output_failure_reason, PX-10,
// PX-A07): a failed result that names a supported output failure reason keeps it
// from finalization through storage to the attempt, step and run a client
// reads, as a failed command-class attempt with its diagnostics retained and no
// success outputs; the identical result replays the original receipt; a reason
// the contract does not admit never reaches execution. It runs against the real
// PostgreSQL composition of the results contract (the memory twin has no
// attempt state to read back) and skips in the unit layer.
func Test_result_output_failure_reason(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	ctx := context.Background()

	failedWith := func(reason string, diag ...domain.ArtifactID) runtimev1.Result {
		res := resultstest.Failed(diag...)
		res.Reason = &reason
		return res
	}
	read := func(t *testing.T, w *pgWorld, a resultstest.Attempt) (*execution.Aggregate, *execution.Attempt) {
		t.Helper()
		agg, err := w.x.Store.Get(ctx, a.Scope, a.Run)
		if err != nil {
			t.Fatal(err)
		}
		at := agg.Attempt(a.ID)
		if at == nil {
			t.Fatalf("attempt %s not found", a.ID)
		}
		return agg, at
	}

	for _, reason := range []string{"output_invalid", "output_upload_failed"} {
		t.Run(reason+" survives finalization and retrieval", func(t *testing.T) {
			w := newPGWorld(t)
			svc, err := results.NewService(w.deps, results.Options{})
			if err != nil {
				t.Fatal(err)
			}
			a := w.NewAttempt(t, scopeA, resultstest.AttemptOptions{})
			diag := w.Artifact(t, a, resultstest.ArtifactSpec{Role: runtimev1.RoleDiagnostic})
			req := resultstest.Request(t, failedWith(reason, diag))
			submit := func() (runtimev1.ResultResponse, error) {
				return svc.Submit(ctx, results.SubmitCommand{Attempt: a.ID, Bearer: w.Bearer(t, a, results.ActionResultSubmit), Request: req})
			}
			first, err := submit()
			if err != nil || first.AttemptStatus != runtimev1.AttemptFailed {
				t.Fatalf("a failed result must fail the attempt: %+v %v", first, err)
			}

			agg, at := read(t, w, a)
			if at.Status != execution.AttemptFailed || at.Class != execution.ClassCommand {
				t.Fatalf("attempt %s class %s: failure classification changed", at.Status, at.Class)
			}
			if string(at.Reason) != reason {
				t.Fatalf("the attempt reads %q, want the specific reason %q", at.Reason, reason)
			}
			if string(agg.Run.FailureReason) != reason {
				t.Fatalf("the run's failure intent is %q, want %q", agg.Run.FailureReason, reason)
			}
			// The workload ends and the attempt settles: the step then fails with
			// the same reason and exposes no success outputs.
			if err := w.x.Terminate(w.attempt(a).intent, execution.LaunchStarted); err != nil {
				t.Fatal(err)
			}
			agg, at = read(t, w, a)
			if step := agg.Step("build"); step == nil || step.Status != execution.StepFailed || string(step.Reason) != reason || len(step.Outputs) != 0 {
				t.Fatalf("step %+v: it must fail with %q and expose no outputs", step, reason)
			}
			if string(at.Reason) != reason {
				t.Fatalf("settlement changed the attempt's reason to %q", at.Reason)
			}
			if at.Result == nil || at.Result.Reason == nil || *at.Result.Reason != reason || len(at.Result.DiagnosticArtifactIDs) != 1 || at.Result.DiagnosticArtifactIDs[0] != diag {
				t.Fatalf("the stored result lost its reason or diagnostics: %+v", at.Result)
			}
			if h, ok, err := w.deps.Finalizer.Accepted(ctx, a.Scope, a.ID); err != nil || !ok || h != req.ResultHash {
				t.Fatalf("the receipt: %v %v %v", h, ok, err)
			}

			// A replay observes the same receipt and changes nothing.
			again, err := submit()
			if err != nil || again != first {
				t.Fatalf("replay: %+v %v, want %+v", again, err, first)
			}
			if _, at2 := read(t, w, a); string(at2.Reason) != reason {
				t.Fatalf("a replay changed the reason to %q", at2.Reason)
			}
			// A different result for the settled attempt still conflicts.
			other := resultstest.Request(t, failedWith("command_failed", diag))
			if _, err := svc.Submit(ctx, results.SubmitCommand{Attempt: a.ID, Bearer: w.Bearer(t, a, results.ActionResultSubmit), Request: other}); !errors.Is(err, results.ErrResultConflict) {
				t.Fatalf("a different reason for an accepted result: %v", err)
			}
		})
	}

	t.Run("reasons outside the supported set do not bypass validation", func(t *testing.T) {
		w := newPGWorld(t)
		svc, err := results.NewService(w.deps, results.Options{})
		if err != nil {
			t.Fatal(err)
		}
		// Not a reason code: refused before any effect, the attempt stays running.
		a := w.NewAttempt(t, scopeA, resultstest.AttemptOptions{})
		bad := failedWith("Output Invalid!")
		req := runtimev1.ResultRequest{Result: bad}
		req.ResultHash, _ = runtimev1.ResultHash(bad)
		_, err = svc.Submit(ctx, results.SubmitCommand{Attempt: a.ID, Bearer: w.Bearer(t, a, results.ActionResultSubmit), Request: req})
		if !errors.Is(err, results.ErrInvalidRequest) {
			t.Fatalf("a malformed reason: %v", err)
		}
		if _, ok, _ := w.deps.Finalizer.Accepted(ctx, a.Scope, a.ID); ok {
			t.Fatal("a malformed reason was accepted")
		}
		if _, at := read(t, w, a); at.Status != execution.AttemptRunning {
			t.Fatalf("a refused result changed the attempt to %s", at.Status)
		}

		// A well-formed reason the module does not know (a command's own manifest
		// reason) is a failed command and keeps the generic classification.
		b := w.NewAttempt(t, scopeA, resultstest.AttemptOptions{})
		custom := resultstest.Request(t, failedWith("tests_red"))
		resp, err := svc.Submit(ctx, results.SubmitCommand{Attempt: b.ID, Bearer: w.Bearer(t, b, results.ActionResultSubmit), Request: custom})
		if err != nil || resp.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("a custom reason: %+v %v", resp, err)
		}
		if _, at := read(t, w, b); at.Reason != execution.ReasonCommandFailed || at.Class != execution.ClassCommand {
			t.Fatalf("an unknown reason is recorded as %q/%s, want %q/%s", at.Reason, at.Class, execution.ReasonCommandFailed, execution.ClassCommand)
		}
	})
}

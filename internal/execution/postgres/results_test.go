package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// Test_result_commit_contract: PX-10, PX-08, PX-14. Duplicate and conflicting
// hashes, cancellation/finalization contention, required validation evidence,
// settled writer publication and diagnostic-only failed outputs.
func Test_result_commit_contract(t *testing.T) {
	ctx := context.Background()
	reportDef := func(e *executiontest.Env, p executiontest.Project, extra ...string) (*execution.Run, *execution.AttemptSpec) {
		def := e.Define(p.Scope, pipelinetest.Doc(
			job("build", append([]string{readRun, "timeoutSeconds: 60", "outputs: {label: {type: string}}"}, extra...)...),
			job("after", "needs: [build]", readRun, "timeoutSeconds: 60"),
		))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		return run, in
	}

	t.Run("duplicate and conflicting hashes", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		run, in := reportDef(e, p)
		out := map[string]runtimev1.Value{"label": runtimev1.StringValue("v1")}
		first, err := e.Succeed(in, out, "")
		if err != nil || first.AttemptStatus != execution.AttemptSucceeded || first.Replayed {
			t.Fatalf("first %+v %v", first, err)
		}
		second, err := e.Succeed(in, out, "")
		if err != nil || !second.Replayed || second.ResultHash != first.ResultHash {
			t.Fatalf("duplicate %+v %v", second, err)
		}
		before := e.Get(run)
		if _, err := e.Succeed(in, map[string]runtimev1.Value{"label": runtimev1.StringValue("v2")}, ""); !errors.Is(err, execution.ErrResultConflict) {
			t.Fatalf("conflicting hash: %v", err)
		}
		after := e.Get(run)
		if after.Run.Version != before.Run.Version || after.Step("build").Outputs["label"].String() != "v1" {
			t.Fatal("a conflicting result mutated state")
		}
		// The successful output is available to the dependent step.
		if next := e.MustClaim(p.Scope.TenantID); next.StepID != "after" {
			t.Fatalf("claimed %s", next.StepID)
		}
	})

	t.Run("required validation evidence", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		_, in := reportDef(e, p)
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{"label": runtimev1.StringValue("x")}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		good, err := runtimev1.ResultHash(res)
		if err != nil {
			t.Fatal(err)
		}
		// A request whose hash does not cover its result is not evidence.
		bad := runtimev1.Digest("sha256:" + string(make([]byte, 0)) + "0000000000000000000000000000000000000000000000000000000000000000")
		if _, err := e.Svc.CommitResult(ctx, execution.ResultCommit{Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: bad, Result: res}}); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("hash mismatch: %v", err)
		}
		if _, err := e.Svc.CommitResult(ctx, execution.ResultCommit{Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: good, Result: res}}); err != nil {
			t.Fatalf("the valid result: %v", err)
		}
		// A result for an attempt of another scope is not found.
		if _, err := e.Svc.CommitResult(ctx, execution.ResultCommit{Scope: executiontest.ScopeB, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: good, Result: res}}); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("foreign scope: %v", err)
		}
	})

	t.Run("undeclared, mistyped and misplaced outputs fail finalization", func(t *testing.T) {
		for name, res := range map[string]runtimev1.Result{
			"undeclared": {Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{
				"label": runtimev1.StringValue("x"), "surprise": runtimev1.StringValue("y")}, DiagnosticArtifactIDs: []domain.ArtifactID{}},
			"mistyped": {Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{
				"label": runtimev1.IntValue(7)}, DiagnosticArtifactIDs: []domain.ArtifactID{}},
			"reader source commit": {Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), SourceCommit: ptr(runtimev1.GitCommit(executiontest.CommitA)),
				Outputs: map[string]runtimev1.Value{"label": runtimev1.StringValue("x")}, DiagnosticArtifactIDs: []domain.ArtifactID{}},
		} {
			e := executiontest.New(t)
			e.GrantAll()
			p := e.NewProject(executiontest.ScopeA)
			run, in := reportDef(e, p)
			ack, err := e.Commit(in, res)
			if err != nil || ack.AttemptStatus != execution.AttemptFailed {
				t.Fatalf("%s: ack %+v %v", name, ack, err)
			}
			agg := e.Get(run)
			if at := agg.Attempt(in.AttemptID); at.Reason != execution.ReasonNoOutputs || len(agg.Step("build").Outputs) != 0 || agg.Run.FailureIntentAt == nil {
				t.Fatalf("%s: attempt %s/%s", name, at.Status, at.Reason)
			}
		}
	})

	t.Run("missing outputs fail finalization and keep diagnostics", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		run, in := reportDef(e, p)
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		ack, err := e.Commit(in, res)
		if err != nil || ack.AttemptStatus != execution.AttemptFailed {
			t.Fatalf("ack %+v %v", ack, err)
		}
		if err := e.Terminate(in, execution.LaunchStarted); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		at := agg.Attempt(in.AttemptID)
		if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonNoOutputs || at.Class != execution.ClassValidation || at.Result == nil {
			t.Fatalf("attempt %s/%s/%s result kept %v", at.Status, at.Reason, at.Class, at.Result != nil)
		}
		if len(agg.Step("build").Outputs) != 0 || agg.Run.Status != execution.RunFailed {
			t.Fatalf("outputs %v run %s", agg.Step("build").Outputs, agg.Run.Status)
		}
		if _, err := e.Claim(p.Scope.TenantID); err == nil {
			t.Fatal("a descendant of a failed step was dispatched")
		}
	})

	t.Run("failed results are diagnostic only", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		run, in := reportDef(e, p)
		if ack, err := e.Fail(in); err != nil || ack.AttemptStatus != execution.AttemptFailed {
			t.Fatalf("%+v %v", ack, err)
		}
		agg := e.Get(run)
		if len(agg.Step("build").Outputs) != 0 || agg.Step("build").OutputCommit != "" {
			t.Fatal("a failed step exposes success outputs")
		}
		if agg.Run.FailureIntentAt == nil {
			t.Fatal("a command failure did not record failure intent")
		}
	})

	t.Run("a writer's success needs its publication settled", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("edit", writeRun, "timeoutSeconds: 60")))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		// A source commit nobody published is not a settled output; the result is
		// refused atomically and the attempt stays running.
		if _, err := e.Succeed(in, nil, executiontest.CommitB); err == nil {
			t.Fatal("an unpublished writer output was accepted")
		}
		if at := e.Get(run).Attempt(in.AttemptID); at.Status != execution.AttemptRunning || at.ResultHash != "" {
			t.Fatalf("a refused result changed the attempt: %s", at.Status)
		}
		// A writer that changed nothing reports its expected tip.
		if _, err := e.Succeed(in, nil, executiontest.CommitA); err != nil {
			t.Fatalf("unchanged writer: %v", err)
		}
		agg := e.Get(run)
		if agg.Run.Status != execution.RunSucceeded || agg.Step("edit").OutputCommit != executiontest.CommitA {
			t.Fatalf("run %s output %q", agg.Run.Status, agg.Step("edit").OutputCommit)
		}
		var holder *string
		e.Scalar(&holder, `SELECT writer_attempt_id FROM workspaces WHERE id = $1`, string(p.Workspace))
		if holder != nil {
			t.Fatalf("writer slot still held by %s", *holder)
		}
	})

	t.Run("cancellation and finalization contend", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			e := executiontest.New(t)
			e.GrantAll()
			p := e.NewProject(executiontest.ScopeA)
			run, in := reportDef(e, p)
			var wg sync.WaitGroup
			var resErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, resErr = e.Succeed(in, map[string]runtimev1.Value{"label": runtimev1.StringValue("x")}, "")
			}()
			go func() {
				defer wg.Done()
				if _, err := e.Svc.Cancel(ctx, engineer, p.Scope, run.ID); err != nil {
					t.Errorf("cancel: %v", err)
				}
			}()
			wg.Wait()
			agg := e.Get(run)
			at := agg.Attempt(in.AttemptID)
			switch {
			case resErr == nil:
				// The result won: the step succeeded and the later cancel only
				// stops what remained.
				if at.Status != execution.AttemptSucceeded || agg.Step("build").Status != execution.StepSucceeded {
					t.Fatalf("accepted result but attempt %s step %s", at.Status, agg.Step("build").Status)
				}
			case errors.Is(resErr, execution.ErrAttemptRevoked):
				if at.Status != execution.AttemptCancelled || agg.Step("build").Outputs != nil && len(agg.Step("build").Outputs) != 0 {
					t.Fatalf("revoked result but attempt %s", at.Status)
				}
				if n := e.Count(`SELECT count(*) FROM attempt_evidence WHERE kind = 'late_result'`); n != 1 {
					t.Fatalf("%d late-result evidence rows", n)
				}
			default:
				t.Fatalf("result: %v", resErr)
			}
			if agg.Run.CancelRequestedAt == nil {
				t.Fatal("cancellation lost")
			}
			if agg.Run.Status == execution.RunSucceeded {
				t.Fatal("a cancelled run succeeded")
			}
		}
	})
}

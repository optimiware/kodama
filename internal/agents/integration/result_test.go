package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const writerPipeline = "{id: implement, kind: agent, agent: {profile: %s}, timeoutSeconds: 600, workspace: {access: write, from: run}, " +
	"retry: {maxAttempts: 3, backoffSeconds: 1, on: [infrastructure, transient]}, outputs: {summary: {type: string}}}"

func artifactRef(scope domain.Scope, id domain.ArtifactID, in *execution.AttemptSpec, usable bool) runtimev1.ArtifactRef {
	return runtimev1.ArtifactRef{ID: id, Scope: scope, Attempt: in.AttemptID, Kind: runtimev1.KindFile, Usable: usable}
}

func diagnostics(ids ...domain.ArtifactID) []domain.ArtifactID {
	if ids == nil {
		return []domain.ArtifactID{}
	}
	return ids
}

// TestNeedsInputFailure verifies AR-13 / AR-A05: through the normal result
// path a needs_input result records a failed attempt with its question and
// authorized diagnostics, exposes no successful candidate, is never retried
// automatically, and revised intent starts a new run. Real PostgreSQL.
func TestNeedsInputFailure(t *testing.T) {
	e := newEnv(t)
	scope := executiontest.ScopeA
	p1 := e.register(scope, e.Draft)
	p := e.NewProject(scope)
	def := e.define(scope, pipelinetest.DocNamed("ask", "{}",
		sprintf(writerPipeline, p1.Ref()),
		"{id: verify, kind: container, image: "+pipelinetest.Image+", command: [/bin/sh, -ec], needs: [implement], timeoutSeconds: 60, workspace: {access: read, from: implement}}"))
	run := e.mustAdmit(p, def)
	in := e.claim(e.Svc, scope.TenantID)
	e.start(in)

	question := "Which currency does the inventory total use?"
	const transcript = domain.ArtifactID("art_t030_transcript")
	res := runtimev1.Result{Outcome: runtimev1.OutcomeNeedsInput, Question: &question, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: diagnostics(transcript)}
	ack, err := e.commit(in, res, map[domain.ArtifactID]runtimev1.ArtifactRef{transcript: artifactRef(scope, transcript, in, true)})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if ack.AttemptStatus != execution.AttemptFailed || ack.Replayed {
		t.Fatalf("needs_input must be a failed attempt: %+v", ack)
	}

	t.Run("the attempt is failed with the question and diagnostics preserved", func(t *testing.T) {
		agg := e.Get(run)
		at := agg.Attempt(in.AttemptID)
		if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonNeedsInput || at.Class != execution.ClassCommand {
			t.Fatalf("attempt: %s %s %s", at.Status, at.Reason, at.Class)
		}
		if at.Result == nil || at.Result.Outcome != runtimev1.OutcomeNeedsInput || at.Result.Question == nil || *at.Result.Question != question {
			t.Fatalf("the question must be recorded verbatim: %+v", at.Result)
		}
		if len(at.Result.DiagnosticArtifactIDs) != 1 || at.Result.DiagnosticArtifactIDs[0] != transcript {
			t.Fatalf("diagnostics: %v", at.Result.DiagnosticArtifactIDs)
		}
		meta, err := mustMetadata(t, e).MetadataOf(context.Background(), scope, run.ID, "implement", in.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Outcome != runtimev1.OutcomeNeedsInput || meta.Question != question || len(meta.DiagnosticArtifactIDs) != 1 ||
			meta.Profile.Version != 1 || meta.Profile.Digest != p1.Digest || meta.Status != execution.AttemptFailed || !meta.TranscriptRecorded {
			t.Fatalf("attributable metadata: %+v", meta)
		}
	})

	t.Run("no successful candidate or output is exposed and no retry follows", func(t *testing.T) {
		// Settle: the executor confirms the workload gone.
		must(t, e.Svc.ObserveAttempt(context.Background(), in.Scope, in.AttemptID, execution.Observation{Kind: execution.ObservedTerminated, WorkloadName: in.WorkloadName, Launch: execution.LaunchStarted}))
		agg := e.Get(run)
		step := agg.Step("implement")
		if step.Status != execution.StepFailed || step.OutputCommit != "" || len(step.Outputs) != 0 {
			t.Fatalf("the failed step must expose nothing: %s %q %v", step.Status, step.OutputCommit, step.Outputs)
		}
		if v := agg.Step("verify"); v.Status == execution.StepSucceeded || v.Status == execution.StepRunning {
			t.Fatalf("a dependent of the failed step must not run: %s", v.Status)
		}
		if agg.Run.Status != execution.RunFailed {
			t.Fatalf("the run must fail: %s", agg.Run.Status)
		}
		if _, err := e.Svc.ClaimRunnable(context.Background(), scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("needs_input must not be retried automatically: %v", err)
		}
		if n := e.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID)); n != 1 {
			t.Fatalf("%d attempts", n)
		}
		var accepted string
		e.Scalar(&accepted, `SELECT accepted_commit FROM workspaces WHERE tenant_id = $1 AND id = $2`, string(scope.TenantID), p.Workspace)
		if accepted != executiontest.CommitA {
			t.Fatalf("incomplete work must not move the accepted commit: %s", accepted)
		}
		if n := e.Count(`SELECT count(*) FROM workspaces WHERE writer_attempt_id IS NOT NULL`); n != 0 {
			t.Fatal("the writer slot must be released")
		}
		if n := e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'attempt' AND reason = 'needs_input'`, string(run.ID)); n != 1 {
			t.Fatalf("history must record the needs_input reason once: %d", n)
		}
	})

	t.Run("revised intent starts a new run and the old one stays as it was", func(t *testing.T) {
		before := e.Get(run).Run
		e.EditSpec(scope, p.Item, "# Plan\nUse EUR for the inventory total.")
		next := e.mustAdmit(p, def)
		if next.ID == run.ID || next.ContentRevision != before.ContentRevision+1 {
			t.Fatalf("a new run pins the revised content: %s rev %d", next.ID, next.ContentRevision)
		}
		again := e.Get(run).Run
		if again.Status != execution.RunFailed || again.ContentRevision != before.ContentRevision || again.Specification.Spec != before.Specification.Spec {
			t.Fatal("the failed run's pins and status are immutable")
		}
		in2 := e.claim(e.Svc, scope.TenantID)
		if in2.RunID != next.ID || in2.AttemptID == in.AttemptID || in2.Ordinal != 1 {
			t.Fatalf("the new run starts from attempt one: %+v", in2)
		}
	})
}

func mustMetadata(t testing.TB, e *env) adapters.MetadataReader {
	t.Helper()
	r, err := adapters.NewMetadataReader(e.Attempts)
	must(t, err)
	return r
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// TestDiagnosticOwnershipAndReplay verifies the result path an agent attempt
// shares with every command (AR-03, AR-06, AR-10): foreign, missing or
// incomplete diagnostic references fail without mutation; identical result
// hashes replay and a conflicting one cannot rewrite history; a completed
// result needs its declared outputs; cancellation prevents late success.
func TestDiagnosticOwnershipAndReplay(t *testing.T) {
	e := newEnv(t)
	scope := executiontest.ScopeA
	p1 := e.register(scope, e.Draft)
	def := e.define(scope, pipelinetest.DocNamed("diag", "{}",
		"{id: implement, kind: agent, agent: {profile: "+p1.Ref()+"}, timeoutSeconds: 600, workspace: {access: read, from: run}, outputs: {summary: {type: string}}}"))
	fail := func(reason string, diags ...domain.ArtifactID) runtimev1.Result {
		return runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: diagnostics(diags...)}
	}

	t.Run("foreign, missing and incomplete diagnostics fail without mutation", func(t *testing.T) {
		p := e.NewProject(scope)
		run := e.mustAdmit(p, def)
		in := e.claim(e.Svc, scope.TenantID)
		e.start(in)
		const art = domain.ArtifactID("art_t030_diag")
		other := e.claim2Foreign(t)
		cases := map[string]map[domain.ArtifactID]runtimev1.ArtifactRef{
			"missing":              {},
			"another attempt's":    {art: artifactRef(scope, art, other, true)},
			"another tenant's":     {art: artifactRef(executiontest.ScopeB, art, in, true)},
			"another project's":    {art: artifactRef(executiontest.ScopeA2, art, in, true)},
			"an unfinished upload": {art: artifactRef(scope, art, in, false)},
		}
		for name, refs := range cases {
			if _, err := e.commit(in, fail("command_failed", art), refs); !errors.Is(err, execution.ErrInvalid) {
				t.Errorf("%s diagnostic must be refused: %v", name, err)
			}
		}
		at := e.Get(run).Attempt(in.AttemptID)
		if at.Status != execution.AttemptRunning || at.ResultHash != "" || at.Result != nil {
			t.Fatalf("a refused result must not mutate the attempt: %s %q", at.Status, at.ResultHash)
		}
		// The correct reference is then accepted.
		ack, err := e.commit(in, fail("command_failed", art), map[domain.ArtifactID]runtimev1.ArtifactRef{art: artifactRef(scope, art, in, true)})
		if err != nil || ack.AttemptStatus != execution.AttemptFailed {
			t.Fatalf("the authorized diagnostic: %+v %v", ack, err)
		}
	})

	t.Run("identical hashes replay and a conflicting hash cannot rewrite history", func(t *testing.T) {
		p := e.NewProject(scope)
		run := e.mustAdmit(p, def)
		in := e.claim(e.Svc, scope.TenantID)
		e.start(in)
		const art = domain.ArtifactID("art_t030_replay")
		refs := map[domain.ArtifactID]runtimev1.ArtifactRef{art: artifactRef(scope, art, in, true)}
		first, err := e.commit(in, fail("command_failed", art), refs)
		if err != nil {
			t.Fatal(err)
		}
		again, err := e.commit(in, fail("command_failed", art), refs)
		if err != nil || !again.Replayed || again.ResultHash != first.ResultHash || again.AttemptStatus != first.AttemptStatus {
			t.Fatalf("an identical result replays its acknowledgment: %+v %v", again, err)
		}
		before := e.Get(run).Attempt(in.AttemptID)
		if _, err := e.commit(in, fail("a_different_reason", art), refs); !errors.Is(err, execution.ErrResultConflict) {
			t.Fatalf("a different result hash conflicts: %v", err)
		}
		after := e.Get(run).Attempt(in.AttemptID)
		if after.ResultHash != before.ResultHash || *after.Result.Reason != *before.Result.Reason || after.Version != before.Version {
			t.Fatal("a conflicting result must not mutate history")
		}
		// A late success after the failure cannot revive the attempt either.
		ok := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{"summary": runtimev1.StringValue("done")}, DiagnosticArtifactIDs: diagnostics()}
		if _, err := e.commit(in, ok, nil); !errors.Is(err, execution.ErrResultConflict) {
			t.Fatalf("a late success after a failure conflicts: %v", err)
		}
	})

	t.Run("a prose success without the declared outputs is not success", func(t *testing.T) {
		p := e.NewProject(scope)
		run := e.mustAdmit(p, def)
		in := e.claim(e.Svc, scope.TenantID)
		e.start(in)
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: diagnostics()}
		ack, err := e.commit(in, res, nil)
		if err != nil || ack.AttemptStatus != execution.AttemptFailed {
			t.Fatalf("exit zero without the declared outputs fails finalization: %+v %v", ack, err)
		}
		at := e.Get(run).Attempt(in.AttemptID)
		if at.Reason != execution.ReasonNoOutputs || len(e.Get(run).Step("implement").Outputs) != 0 {
			t.Fatalf("reason %s", at.Reason)
		}
	})

	t.Run("a completed result with its declared outputs succeeds", func(t *testing.T) {
		p := e.NewProject(scope)
		run := e.mustAdmit(p, def)
		in := e.claim(e.Svc, scope.TenantID)
		e.start(in)
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{"summary": runtimev1.StringValue("done")}, DiagnosticArtifactIDs: diagnostics()}
		ack, err := e.commit(in, res, nil)
		if err != nil || ack.AttemptStatus != execution.AttemptSucceeded {
			t.Fatalf("%+v %v", ack, err)
		}
		if st := e.Get(run).Step("implement"); st.Status != execution.StepSucceeded || st.Outputs["summary"] != runtimev1.StringValue("done") {
			t.Fatalf("step: %+v", st)
		}
	})

	t.Run("cancellation prevents a late success", func(t *testing.T) {
		p := e.NewProject(scope)
		run := e.mustAdmit(p, def)
		in := e.claim(e.Svc, scope.TenantID)
		e.start(in)
		if _, err := e.Svc.Cancel(context.Background(), e.caller(), scope, run.ID); err != nil {
			t.Fatal(err)
		}
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{"summary": runtimev1.StringValue("done")}, DiagnosticArtifactIDs: diagnostics()}
		if _, err := e.commit(in, res, nil); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("a result after cancellation is refused: %v", err)
		}
		agg := e.Get(run)
		if st := agg.Step("implement"); st.Status == execution.StepSucceeded || len(st.Outputs) != 0 {
			t.Fatalf("a cancelled attempt must not succeed: %s", st.Status)
		}
		if at := agg.Attempt(in.AttemptID); at.Status == execution.AttemptSucceeded {
			t.Fatal("the late success must not be recorded as the outcome")
		}
		if n := e.Count(`SELECT count(*) FROM attempt_evidence WHERE attempt_id = $1 AND kind = 'late_result'`, string(in.AttemptID)); n != 1 {
			t.Fatalf("the late result is kept as evidence: %d", n)
		}
	})
}

// claim2Foreign claims an attempt of another run to borrow its identity.
func (e *env) claim2Foreign(t *testing.T) *execution.AttemptSpec {
	t.Helper()
	scope := executiontest.ScopeA
	p := e.NewProject(scope)
	def := e.define(scope, pipelinetest.DocNamed("foreign", "{}", "{id: other, kind: agent, agent: {profile: coding@1}, timeoutSeconds: 600, workspace: {access: read, from: run}, outputs: {summary: {type: string}}}"))
	e.mustAdmit(p, def)
	return e.claim(e.Svc, scope.TenantID)
}

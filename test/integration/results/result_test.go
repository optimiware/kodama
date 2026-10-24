package results_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
)

func outputsOf(label string) map[string]runtimev1.Value {
	return map[string]runtimev1.Value{"label": runtimev1.StringValue(label)}
}

func decodeAck(t testing.TB, body []byte) runtimev1.ResultResponse {
	t.Helper()
	var r runtimev1.ResultResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r
}

func errCode(t testing.TB, body []byte) domain.ErrorCode {
	t.Helper()
	var r runtimev1.ErrorResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r.Error.Code
}

func outbox(e *Env, tenant domain.TenantID) int {
	return e.X.CountTenant(tenant, `SELECT count(*) FROM outbox`)
}

// Test_conflicting_result: PX-10, PX-12, IT-06 (acceptance `conflicting_result`).
// Simultaneous identical submissions produce one durable acceptance; a changed
// valid hash is result_conflict and mutates nothing; terminal retries repeat the
// whole original acknowledgment, acceptedAt included, with no duplicate history
// or outbox rows. Real PostgreSQL through the real adapters; no NATS, S3 or Git.
func Test_conflicting_result(t *testing.T) {
	e := New(t, Options{})
	p := e.NewProject(ScopeA)
	a := e.Run(p, Job("build", readRun, "outputs: {label: {type: string}}"))
	req := Request(t, Completed(outputsOf("v1")))

	// Eight identical submissions race for the one acceptance.
	acks := make([]runtimev1.ResultResponse, 8)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range acks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			acks[i], errs[i] = e.Submit(a, req)
		}()
	}
	close(start)
	wg.Wait()
	for i := range acks {
		if errs[i] != nil {
			t.Fatalf("submission %d: %v", i, errs[i])
		}
		if acks[i] != acks[0] {
			t.Fatalf("acknowledgments differ: %+v vs %+v", acks[i], acks[0])
		}
	}
	first := acks[0]
	if first.AttemptStatus != runtimev1.AttemptSucceeded || first.ResultHash != req.ResultHash || first.AttemptID != a.ID() {
		t.Fatalf("%+v", first)
	}
	persisted := e.Persisted(a)
	if persisted.ResultHash != string(req.ResultHash) || persisted.Status != execution.AttemptSucceeded || persisted.FinishedAt == nil {
		t.Fatalf("persisted %+v", persisted)
	}
	if got := string(first.AcceptedAt); got != persisted.FinishedAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("acceptedAt %s is not the persisted acceptance time %s", got, persisted.FinishedAt.UTC().Format(time.RFC3339Nano))
	}
	if n := e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'attempt' AND to_status = 'succeeded'`, string(a.Run.ID)); n != 1 {
		t.Fatalf("%d success transitions", n)
	}

	// Terminal retries: the same whole acknowledgment, no new history or events.
	history, events, version := e.HistoryRows(a), outbox(e, ScopeA.TenantID), e.Aggregate(a).Run.Version
	time.Sleep(20 * time.Millisecond)
	for range 3 {
		again, err := e.Submit(a, req)
		if err != nil || again != first {
			t.Fatalf("retry %+v %v, want %+v", again, err, first)
		}
	}
	code, body := e.SubmitHTTP(a, req)
	if code != 200 || decodeAck(t, body) != first {
		t.Fatalf("http retry %d %s", code, body)
	}
	if e.HistoryRows(a) != history || outbox(e, ScopeA.TenantID) != events || e.Aggregate(a).Run.Version != version {
		t.Fatal("a replay changed history, events or the run version")
	}

	// A different valid result is a conflict and changes nothing.
	other := Request(t, Completed(outputsOf("v2")))
	code, body = e.SubmitHTTP(a, other)
	if code != 409 || errCode(t, body) != runtimev1.CodeResultConflict {
		t.Fatalf("changed hash: %d %s", code, body)
	}
	if _, err := e.Submit(a, other); !errors.Is(err, results.ErrResultConflict) {
		t.Fatalf("changed hash: %v", err)
	}
	if e.Persisted(a).ResultHash != string(req.ResultHash) || e.HistoryRows(a) != history || outbox(e, ScopeA.TenantID) != events {
		t.Fatal("a conflicting result mutated state")
	}
	if got := e.Aggregate(a).Step("build").Outputs["label"].String(); got != "v1" {
		t.Fatalf("accepted output %q", got)
	}
	// A failed result for a succeeded attempt is also a conflict, never a rewrite.
	failed := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("command_failed"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}})
	if _, err := e.Submit(a, failed); !errors.Is(err, results.ErrResultConflict) {
		t.Fatalf("failed after success: %v", err)
	}
}

// Test_missing_output: PX-10, PX-12 (acceptance `missing_output`). An exit-zero
// result without its declared output durably fails finalization, keeps verified
// diagnostics readable, never exposes success outputs, and is not retried.
// Real PostgreSQL and S3 through the real adapters; no NATS or Git.
func Test_missing_output(t *testing.T) {
	e := New(t, Options{Artifacts: true})
	p := e.NewProject(ScopeA)
	a := e.Run(p, Job("build", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.json}, label: {type: string}}"),
		Job("after", "needs: [build]", readRun))
	log := e.Upload(a, "build.log", artifacts.RoleDiagnostic, []byte("compiled, then crashed\n"))

	res := Completed(map[string]runtimev1.Value{"label": runtimev1.StringValue("x")}, log) // report is missing
	req := Request(t, res)
	code, body := e.SubmitHTTP(a, req)
	if code != 200 {
		t.Fatalf("a durable failed acknowledgment is a 200: %d %s", code, body)
	}
	ack := decodeAck(t, body)
	if ack.AttemptStatus != runtimev1.AttemptFailed || ack.ResultHash != req.ResultHash {
		t.Fatalf("%+v", ack)
	}
	at := e.Persisted(a)
	if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonNoOutputs || at.Class != execution.ClassValidation || at.ResultHash != string(req.ResultHash) {
		t.Fatalf("persisted %s %s %s", at.Status, at.Reason, at.Class)
	}
	// Verified diagnostics stay readable and protected; nothing leaked to the step.
	if got := e.ArtSvc; got == nil {
		t.Fatal("no artifact service")
	}
	refs, err := e.ArtSvc.References(context.Background(), a.Scope, a.ID(), []domain.ArtifactID{log})
	if err != nil || !refs[log].Usable {
		t.Fatalf("diagnostic %+v %v", refs, err)
	}
	if n := e.Count(`SELECT count(*) FROM artifact_pins WHERE artifact_id = $1 AND owner_kind = 'attempt_finalization' AND owner_id = $2`, string(log), string(a.ID())); n != 1 {
		t.Fatalf("the diagnostic is not protected: %d pins", n)
	}
	agg := e.Aggregate(a)
	if out := agg.Step("build").Outputs; len(out) != 0 {
		t.Fatalf("success outputs were exposed: %v", out)
	}
	if step := agg.Step("build"); step.Status == execution.StepSucceeded {
		t.Fatalf("the step is %s", step.Status)
	}
	if after := agg.Step("after"); after.Status == execution.StepSucceeded || after.Status == execution.StepRunning {
		t.Fatalf("a dependent consumed outputs of a failed step: %s", after.Status)
	}
	// A validation failure is not retried.
	if n := e.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(a.Run.ID)); n != 1 {
		t.Fatalf("%d attempts: a validation failure must not be retried", n)
	}
	if _, err := e.X.Svc.ClaimRunnable(context.Background(), ScopeA.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("claim after validation failure: %v", err)
	}
	// Replays repeat the failed acknowledgment; the exit code zero never means success.
	if again, err := e.Submit(a, req); err != nil || again != ack {
		t.Fatalf("replay %+v %v", again, err)
	}
	// A request for a missing output that also names a foreign diagnostic is a 404,
	// not a durable failure that would retain an unverified reference.
	other := e.Run(e.NewProject(ScopeA), Job("build", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.json}}"))
	foreign := Request(t, Completed(map[string]runtimev1.Value{}, log))
	if code, body := e.SubmitHTTP(other, foreign); code != 404 || errCode(t, body) != domain.CodeNotFound {
		t.Fatalf("foreign diagnostic: %d %s", code, body)
	}
	if e.Persisted(other).Status != execution.AttemptRunning {
		t.Fatal("a rejected request changed the attempt")
	}
}

// Test_diagnostic_as_output: PX-10. A usable diagnostic of the attempt, named
// both as a declared artifact output and as a diagnostic, is not a trusted
// output: finalization fails durably and exposes no success outputs.
func Test_diagnostic_as_output(t *testing.T) {
	e := New(t, Options{Artifacts: true})
	p := e.NewProject(ScopeA)
	a := e.Run(p, Job("build", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.json}}"),
		Job("after", "needs: [build]", readRun))
	log := e.Upload(a, "build.log", artifacts.RoleDiagnostic, []byte("not a report\n"))

	req := Request(t, Completed(map[string]runtimev1.Value{"report": runtimev1.StringValue(string(log))}, log))
	code, body := e.SubmitHTTP(a, req)
	if code != 200 {
		t.Fatalf("a durable failed acknowledgment is a 200: %d %s", code, body)
	}
	if ack := decodeAck(t, body); ack.AttemptStatus != runtimev1.AttemptFailed || ack.ResultHash != req.ResultHash {
		t.Fatalf("%+v", ack)
	}
	at := e.Persisted(a)
	if at.Status != execution.AttemptFailed || at.Class != execution.ClassValidation {
		t.Fatalf("persisted %s %s", at.Status, at.Class)
	}
	agg := e.Aggregate(a)
	if out := agg.Step("build").Outputs; len(out) != 0 {
		t.Fatalf("success outputs were exposed: %v", out)
	}
	if agg.Step("build").Status == execution.StepSucceeded || agg.Step("after").Status == execution.StepSucceeded {
		t.Fatal("the diagnostic satisfied the required output")
	}
	// The valid diagnostic stays readable and protected.
	if refs, err := e.ArtSvc.References(context.Background(), a.Scope, a.ID(), []domain.ArtifactID{log}); err != nil || !refs[log].Usable {
		t.Fatalf("diagnostic %+v %v", refs, err)
	}
	if n := e.Count(`SELECT count(*) FROM artifact_pins WHERE artifact_id = $1 AND owner_kind = 'attempt_finalization' AND owner_id = $2`, string(log), string(a.ID())); n != 1 {
		t.Fatalf("the diagnostic is not protected: %d pins", n)
	}
}

// TestLateResultAndDiagnostics: PX-10, PX-12, PX-A06. The deadline applies at
// acceptance even without a reconciler tick, terminal outcomes stay immutable,
// and failed and needs-input diagnostics remain readable. Real PostgreSQL and
// S3; no NATS or Git.
func TestLateResultAndDiagnostics(t *testing.T) {
	t.Run("a result after the execution deadline is late, kept as evidence, and rejected", func(t *testing.T) {
		e := New(t, Options{})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, "timeoutSeconds: 30"))
		e.X.Clock.Advance(2 * time.Minute) // no reconciler tick runs
		before := e.Count(`SELECT count(*) FROM attempt_evidence WHERE attempt_id = $1 AND kind = 'late_result'`, string(a.ID()))
		code, body := e.SubmitHTTP(a, Request(t, Completed(nil)))
		if code != 403 || errCode(t, body) != runtimev1.CodeAttemptRevoked {
			t.Fatalf("%d %s", code, body)
		}
		at := e.Persisted(a)
		if at.Status == execution.AttemptSucceeded || at.ResultHash != "" {
			t.Fatalf("a late result changed the outcome: %s %q", at.Status, at.ResultHash)
		}
		if n := e.Count(`SELECT count(*) FROM attempt_evidence WHERE attempt_id = $1 AND kind = 'late_result'`, string(a.ID())); n != before+1 {
			t.Fatalf("late-result evidence rows %d", n)
		}
		// The timeout was persisted at acceptance time, not left for a reconciler.
		if !at.Status.Terminal() || at.Class == "" {
			t.Fatalf("the missed deadline was not persisted: %s", at.Status)
		}
		again, _ := e.SubmitHTTP(a, Request(t, Completed(nil)))
		if again != 403 {
			t.Fatalf("a repeat of a late result: %d", again)
		}
		if e.Persisted(a).Status == execution.AttemptSucceeded {
			t.Fatal("success after the deadline")
		}
	})

	t.Run("a terminal outcome stays immutable against a later different result", func(t *testing.T) {
		e := New(t, Options{})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun))
		failed := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(2)), Reason: ptr("command_failed"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}})
		ack, err := e.Submit(a, failed)
		if err != nil || ack.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("%+v %v", ack, err)
		}
		if _, err := e.Submit(a, Request(t, Completed(nil))); !errors.Is(err, results.ErrResultConflict) {
			t.Fatalf("success after failure: %v", err)
		}
		if e.Persisted(a).Status != execution.AttemptFailed {
			t.Fatal("the failed outcome was rewritten")
		}
	})

	t.Run("failed and needs-input results keep readable diagnostics", func(t *testing.T) {
		e := New(t, Options{Artifacts: true})
		p := e.NewProject(ScopeA)
		a := e.Run(p, Job("build", readRun))
		log := e.Upload(a, "build.log", artifacts.RoleDiagnostic, []byte("boom\n"))
		failed := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("command_failed"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{log}})
		ack, err := e.Submit(a, failed)
		if err != nil || ack.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("%+v %v", ack, err)
		}
		at := e.Persisted(a)
		if at.Result == nil || len(at.Result.DiagnosticArtifactIDs) != 1 || at.Result.DiagnosticArtifactIDs[0] != log || len(at.Result.Outputs) != 0 || at.Result.SourceCommit != nil {
			t.Fatalf("persisted result %+v", at.Result)
		}
		if refs, err := e.ArtSvc.References(context.Background(), a.Scope, a.ID(), []domain.ArtifactID{log}); err != nil || !refs[log].Usable {
			t.Fatalf("the diagnostic is unreadable: %+v %v", refs, err)
		}

		// needs_input: a second attempt of another project.
		b := e.Run(e.NewProject(ScopeA), Job("ask", readRun))
		blog := e.Upload(b, "ask.log", artifacts.RoleDiagnostic, []byte("asked\n"))
		needs := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeNeedsInput, Question: ptr("Which environment?"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{blog}})
		ack, err = e.Submit(b, needs)
		if err != nil || ack.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("needs_input is a failed attempt: %+v %v", ack, err)
		}
		if got := e.Persisted(b); got.Result == nil || got.Result.Question == nil || len(got.Result.Outputs) != 0 {
			t.Fatalf("%+v", got.Result)
		}
		// Success outputs and a source commit are not allowed on these outcomes.
		bad := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("command_failed"), Outputs: map[string]runtimev1.Value{"x": runtimev1.StringValue("y")}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		h, _ := runtimev1.ResultHash(bad)
		c := e.Run(e.NewProject(ScopeA), Job("bad", readRun))
		if code, body := e.SubmitHTTP(c, runtimev1.ResultRequest{ResultHash: h, Result: bad}); code != 400 {
			t.Fatalf("%d %s", code, body)
		}
	})
}

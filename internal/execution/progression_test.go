package execution

import (
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/pipeline"
)

func finish(t *testing.T, agg *Aggregate, at *Attempt, res runtimev1.Result, now time.Time) {
	t.Helper()
	agg.Observe(at, Observation{Kind: ObservedStarted, WorkloadUID: "11111111-2222-3333-4444-555555555555"}, now)
	if err := agg.CompleteAttempt(at, "sha256:"+hex64("a"), res, now); err != nil {
		t.Fatal(err)
	}
	if _, err := agg.Settle(at, now); err != nil {
		t.Fatal(err)
	}
}

// Test_dag_progression: PX-06, PX-14, PX-13, PX-A04. Conditions, skip
// propagation, successful-output references, explicit ancestor sources and gate
// non-bypass, without services.
func Test_dag_progression(t *testing.T) {
	str := func(n, v string) PinnedInput {
		return PinnedInput{Name: n, Type: "string", Value: runtimev1.StringValue(v)}
	}

	t.Run("conditions and skip propagation", func(t *testing.T) {
		agg := newAgg(t, []PinnedInput{str("mode", "slow"), {Name: "debug", Type: "boolean", Value: runtimev1.BoolValue(true)}, {Name: "n", Type: "integer", Value: runtimev1.IntValue(3)}},
			container("fast", when("mode", pipeline.StringScalar("fast"))),
			container("after-fast", needs("fast")),
			container("grandchild", needs("after-fast")),
			container("debugging", when("debug", pipeline.BoolScalar(true))),
			container("counted", when("n", pipeline.IntScalar(3))),
			container("wrong-type", when("n", pipeline.StringScalar("3"))),
			container("independent"),
		)
		if !agg.Propagate(t0) {
			t.Fatal("nothing was skipped")
		}
		want := map[string]StepStatus{"fast": StepSkipped, "after-fast": StepSkipped, "grandchild": StepSkipped, "debugging": StepPending,
			"counted": StepPending, "wrong-type": StepSkipped, "independent": StepPending}
		for id, st := range want {
			if got := agg.Step(id).Status; got != st {
				t.Errorf("%s is %s, want %s", id, got, st)
			}
		}
		if agg.Step("fast").Reason != ReasonCondition || agg.Step("after-fast").Reason != ReasonSkippedAncestor || agg.Step("grandchild").Reason != ReasonSkippedAncestor {
			t.Errorf("reasons %s/%s/%s", agg.Step("fast").Reason, agg.Step("after-fast").Reason, agg.Step("grandchild").Reason)
		}
		if agg.Propagate(t0) {
			t.Fatal("propagation is not idempotent")
		}
		// Only steps that can still run are runnable; skipped ones never are.
		var runnable []string
		for _, s := range agg.Steps {
			if agg.Runnable(s, t0, hooks) {
				runnable = append(runnable, s.ID)
			}
		}
		if len(runnable) != 3 {
			t.Fatalf("runnable %v", runnable)
		}
		// A run whose remaining steps all succeeded or were skipped succeeds.
		for _, id := range []string{"debugging", "counted", "independent"} {
			at := claim(t, agg, t0, "att_"+id)
			finish(t, agg, at, okResult(nil, ""), t0)
		}
		if done, err := agg.Finish(t0); err != nil || !done || agg.Run.Status != RunSucceeded {
			t.Fatalf("%v %v %s", done, err, agg.Run.Status)
		}
	})

	t.Run("dependencies must succeed", func(t *testing.T) {
		agg := newAgg(t, nil, container("a"), container("b", needs("a")), container("c", needs("a", "b")))
		if s := agg.NextRunnable(t0, hooks); s == nil || s.ID != "a" {
			t.Fatalf("first runnable %v", s)
		}
		at := claim(t, agg, t0, "att_a")
		if agg.NextRunnable(t0, hooks) != nil {
			t.Fatal("a dependent step ran while its dependency was unfinished")
		}
		finish(t, agg, at, okResult(nil, ""), t0)
		if s := agg.NextRunnable(t0, hooks); s == nil || s.ID != "b" {
			t.Fatalf("after a: %v", s)
		}
	})

	t.Run("successful-output references", func(t *testing.T) {
		agg := newAgg(t, []PinnedInput{str("env", "prod"), {Name: "n", Type: "integer", Value: runtimev1.IntValue(7)}, {Name: "on", Type: "boolean", Value: runtimev1.BoolValue(true)}},
			container("build", outputs("label"), writer(pipeline.RunSource)),
			container("use", needs("build"), reader("build"), args("${{ steps.build.outputs.label }}", "${{ inputs.env }}", "${{ inputs.n }}", "${{ inputs.on }}", "${{ steps.build.outputs.commit }}", "literal")),
		)
		use := agg.Step("use")
		if _, err := agg.Resolve(use); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a reference to an unfinished producer resolved: %v", err)
		}
		at := claim(t, agg, t0, "att_build")
		finish(t, agg, at, okResult(map[string]runtimev1.Value{"label": runtimev1.StringValue("v1.2")}, commitB), t0)
		rs, err := agg.Resolve(use)
		if err != nil {
			t.Fatal(err)
		}
		if got := rs.Args; len(got) != 6 || got[0] != "v1.2" || got[1] != "prod" || got[2] != "7" || got[3] != "true" || got[4] != commitB || got[5] != "literal" {
			t.Fatalf("resolved args %v", got)
		}
		for name, arg := range map[string]string{
			"interpolation": "prefix-${{ inputs.env }}", "unknown input": "${{ inputs.nope }}", "unknown output": "${{ steps.build.outputs.nope }}",
			"unknown step": "${{ steps.ghost.outputs.x }}", "expression": "${{ 1 + 1 }}",
		} {
			bad := NewStepRun("run_1", 9, container("bad", args(arg)), t0)
			if _, err := agg.Resolve(bad); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A failed producer exposes no success outputs.
		failed := newAgg(t, nil, container("p", outputs("label")), container("q", needs("p"), args("${{ steps.p.outputs.label }}")))
		pa := claim(t, failed, t0, "att_p")
		failed.Observe(pa, Observation{Kind: ObservedStarted, WorkloadUID: "11111111-2222-3333-4444-555555555555"}, t0)
		_ = failed.CompleteAttempt(pa, "sha256:"+hex64("a"), runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(2)), Reason: ptr("boom"),
			Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: nil}, t0)
		if len(failed.Step("p").Outputs) != 0 {
			t.Fatal("a failed step has outputs")
		}
		if _, err := failed.Resolve(failed.Step("q")); err == nil {
			t.Fatal("a descendant resolved a failed step's output")
		}
	})

	t.Run("sources are the run commit or an explicit successful ancestor's commit", func(t *testing.T) {
		agg := newAgg(t, nil,
			container("w", writer(pipeline.RunSource)),
			container("r-run", needs("w"), reader(pipeline.RunSource)),
			container("r-w", needs("w"), reader("w")),
			container("none", noWorkspace()),
		)
		b, err := agg.SourceFor(agg.Step("r-run"))
		if err != nil || b.Commit != commitA || b.Access != AccessRead || b.FromWriter != "" {
			t.Fatalf("run source %+v %v", b, err)
		}
		if _, err := agg.SourceFor(agg.Step("r-w")); !errors.Is(err, ErrStale) {
			t.Fatalf("the writer has not succeeded: %v", err)
		}
		if b, _ := agg.SourceFor(agg.Step("none")); b.Access != AccessNone || b.Commit != "" {
			t.Fatalf("no-workspace step %+v", b)
		}
		at := claim(t, agg, t0, "att_w")
		if at.Access != AccessWrite || at.InputCommit != commitA || at.ExpectedTip != commitA {
			t.Fatalf("writer attempt source %s/%s/%s", at.Access, at.InputCommit, at.ExpectedTip)
		}
		finish(t, agg, at, okResult(nil, commitB), t0)
		// The run's own readers still see the original commit; the explicit
		// reader sees exactly the writer's commit, never "the branch tip".
		if b, _ := agg.SourceFor(agg.Step("r-run")); b.Commit != commitA {
			t.Fatalf("branch movement changed a run-source read: %s", b.Commit)
		}
		b, err = agg.SourceFor(agg.Step("r-w"))
		if err != nil || b.Commit != commitB || b.FromWriter != "w" {
			t.Fatalf("ancestor source %+v %v", b, err)
		}
		// A writer that did not succeed offers no source.
		bad := newAgg(t, nil, container("w", writer(pipeline.RunSource)), container("r", needs("w"), reader("w")))
		if _, err := bad.SourceFor(bad.Step("r")); err == nil {
			t.Fatal("a source writer with no output was accepted")
		}
		// A malformed source (a non-writer) is refused outright.
		notWriter := newAgg(t, nil, container("a"), container("r", needs("a"), reader("a")))
		if _, err := notWriter.SourceFor(notWriter.Step("r")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("non-writer source: %v", err)
		}
	})

	t.Run("a gate cannot be bypassed", func(t *testing.T) {
		agg := newAgg(t, []PinnedInput{str("mode", "other")}, container("build"), gate("approve", "build"), container("ship", needs("approve")))
		// A stored condition on a gate is never a reason to skip it.
		agg.Step("approve").Spec.When = &pipeline.Condition{Input: "mode", Equals: pipeline.StringScalar("never")}
		agg.Propagate(t0)
		if agg.Step("approve").Status != StepPending {
			t.Fatal("a mandatory gate was skipped through a false author condition")
		}
		at := claim(t, agg, t0, "att_build")
		finish(t, agg, at, okResult(nil, ""), t0)
		for _, s := range agg.Steps {
			if s.ID != "build" && agg.Runnable(s, t0.Add(24*time.Hour), hooks) {
				t.Fatalf("%s is runnable: the gate or its successor ran without a decision", s.ID)
			}
		}
		if done, _ := agg.Finish(t0); done || agg.Run.Status == RunSucceeded {
			t.Fatal("a run with an undecided gate succeeded")
		}
		// Only a recorded approval (the gate task's seam) unblocks the successor.
		g := agg.Step("approve")
		if err := g.start(t0); err != nil {
			t.Fatal(err)
		}
		if err := g.succeed("", nil, t0); err != nil {
			t.Fatal(err)
		}
		if s := agg.NextRunnable(t0, hooks); s == nil || s.ID != "ship" {
			t.Fatalf("after approval: %v", s)
		}
		// A skipped ancestor of a gate skips the gate and everything behind it.
		sk := newAgg(t, []PinnedInput{str("mode", "slow")}, container("build", when("mode", pipeline.StringScalar("fast"))), gate("approve", "build"), container("ship", needs("approve")))
		sk.Propagate(t0)
		if sk.Step("approve").Status != StepSkipped || sk.Step("ship").Status != StepSkipped {
			t.Fatalf("gate %s successor %s", sk.Step("approve").Status, sk.Step("ship").Status)
		}
	})

	t.Run("disabled kinds are never claimed", func(t *testing.T) {
		agent := pipeline.Step{ID: "agent", Kind: pipeline.KindAgent, Agent: &pipeline.AgentSpec{Profile: "coding-v1"},
			Workspace: &pipeline.Workspace{Access: pipeline.AccessWrite, From: pipeline.RunSource}, Retry: &pipeline.Retry{MaxAttempts: 1}}
		agg := newAgg(t, nil, agent, gate("g"))
		for _, h := range []Hooks{hooks, nil, EnabledKinds{}} {
			if agg.NextRunnable(t0, h) != nil {
				t.Fatal("a disabled step kind was runnable")
			}
		}
		if agg.NextRunnable(t0, EnabledKinds{pipeline.KindAgent}) == nil {
			t.Fatal("an enabled agent hook did not make the agent step runnable")
		}
		if (EnabledKinds{pipeline.KindAgent, pipeline.KindContainer}).Enabled(pipeline.KindGate) {
			t.Fatal("a gate is never executed as a Job")
		}
	})
}

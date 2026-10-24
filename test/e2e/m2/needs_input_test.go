package m2_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// TestM2NeedsInput (m2_needs_input) runs the deterministic agent on the real
// stack: an agent step is an ordinary container step of an immutable profile,
// admitted, dispatched to Podman, admitted by runtime authority and finalized
// by the result route. Missing business input is the wrapper's `needs_input`
// outcome: the attempt FAILS with the recorded question, publishes nothing,
// is not retried, leaves its descendants unstarted and the run failed; a human
// revises the specification and a NEW run (at the new content revision)
// succeeds while the first keeps its pinned revision and history. An exit of
// zero without a structured result fails finalization, never succeeds.
// Covers AR-A05, AR-A04, AR-13, AR-06, AR-03 and PX-13 (a failed agent attempt
// consumes no gate).
func TestM2NeedsInput(t *testing.T) {
	e := kit.New(t, kit.Options{})
	def := e.Register("needs-input", kit.AgentInputs(),
		withRetry(e.AgentStep("implement")),
		kit.ContainerStep("test", "echo must-not-run", nil, "implement"),
		kit.GateStep("review", nil, "test"),
	)
	item := e.Item("Reconcile adjustments", "# Spec\nReconcile inventory adjustments per warehouse.")
	if item.ContentRevision != 1 {
		t.Fatalf("content revision %d", item.ContentRevision)
	}

	// ---- the agent reports missing input ----
	first := e.StartRun("needs-input-1", item, def.Id, e.AgentRunInputs("needs_input"))
	final := e.World.WaitRun(first.Id, 3*time.Minute)
	if final.Status != client.RunStatusFailed || final.Reason != "needs_input" {
		t.Fatalf("run %s (%s), want failed (needs_input)", final.Status, final.Reason)
	}
	step := e.StepOf(first.Id, "implement")
	if step.Status != client.StepStatusFailed || step.Reason != "needs_input" || step.Outputs != nil || step.SourceCommit != nil {
		t.Fatalf("implement step %+v exposes a candidate or outputs", step)
	}
	if len(step.Attempts) != 1 || step.Attempts[0].Status != client.AttemptStatusFailed || !step.Attempts[0].Settled || step.Attempts[0].Reason != "needs_input" {
		t.Fatalf("attempts %+v: needs_input is a failed, settled, unretried attempt", step.Attempts)
	}
	if got := e.AttemptQuestion(step.Attempts[0].Id); !strings.Contains(got, "Which warehouse owns the reconciliation adjustment?") {
		t.Fatalf("the recorded question is %q", got)
	}
	if len(step.Attempts[0].DiagnosticArtifactIds) == 0 {
		t.Fatal("the failed attempt kept no diagnostic artifacts")
	}
	// Diagnostics are authorized artifacts: the project reads them, another tenant does not.
	diag := step.Attempts[0].DiagnosticArtifactIds[0]
	if code, _ := e.Do("engineer", "GET", e.ProjectPath()+"/artifacts/"+diag, nil, nil); code != http.StatusOK {
		t.Fatalf("the project cannot read its attempt's diagnostic: %d", code)
	}
	if code, _ := e.Do("analyst", "GET", "/api/v1/tenants/"+kit.TenantB+"/projects/"+e.World.ProjectB+"/artifacts/"+diag, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another tenant read the diagnostic: %d", code)
	}
	// The descendants never started: no attempt, no gate (a gate waits only for a
	// successful test), and the run recorded the revision it was admitted at.
	for _, id := range []string{"test", "review"} {
		s := e.StepOf(first.Id, id)
		if s.Status != client.StepStatusCancelled || len(s.Attempts) != 0 {
			t.Fatalf("step %s is %s with %d attempts after a failed agent", id, s.Status, len(s.Attempts))
		}
	}
	if gates := e.Gates("reviewer", first.Id); len(gates) != 0 {
		t.Fatalf("a failed agent produced %d gates", len(gates))
	}
	if n := e.Attempts(first.Id); n != 1 {
		t.Fatalf("%d attempts for one failed agent step", n)
	}
	if first.ContentRevision == nil || *first.ContentRevision != 1 {
		t.Fatalf("the run pinned revision %v", first.ContentRevision)
	}

	// ---- a human revises the intent; the answer is a NEW run ----
	revised := e.EditSpec(item, "# Spec\nReconcile inventory adjustments per warehouse.\n\nAnswer: the receiving warehouse owns the adjustment.")
	if revised.ContentRevision != 2 {
		t.Fatalf("the edit left content revision %d", revised.ContentRevision)
	}
	// The failed run is history: the revision neither revives nor rewrites it.
	if again := e.World.GetRun(first.Id); again.Status != client.RunStatusFailed || *again.ContentRevision != 1 {
		t.Fatalf("the failed run changed after the revision: %s rev %v", again.Status, again.ContentRevision)
	}
	oneStep := e.Register("needs-input-revised", kit.AgentInputs(), e.AgentStep("implement"))
	second := e.StartRun("needs-input-2", revised, oneStep.Id, e.AgentRunInputs("completed"))
	if second.Id == first.Id || second.ContentRevision == nil || *second.ContentRevision != 2 {
		t.Fatalf("the new run %s pinned revision %v", second.Id, second.ContentRevision)
	}
	ok := e.World.WaitRun(second.Id, 3*time.Minute)
	if ok.Status != client.RunStatusSucceeded {
		t.Fatalf("the revised run is %s (%s)", ok.Status, ok.Reason)
	}
	s2 := e.StepOf(second.Id, "implement")
	if kit.Output(s2, "summary") == "" || s2.Status != client.StepStatusSucceeded {
		t.Fatalf("the revised agent step %+v", s2)
	}

	// ---- exit zero without a structured result fails finalization (AR-A04) ----
	silent := e.StartRun("needs-input-3", revised, oneStep.Id, e.AgentRunInputs("none"))
	sf := e.World.WaitRun(silent.Id, 3*time.Minute)
	ss := e.StepOf(silent.Id, "implement")
	if sf.Status == client.RunStatusSucceeded || ss.Status == client.StepStatusSucceeded || ss.Outputs != nil {
		t.Fatalf("an agent that exited 0 without declared outputs succeeded: run %s step %s", sf.Status, ss.Status)
	}
	if sf.Status != client.RunStatusFailed {
		t.Fatalf("run %s", sf.Status)
	}
}

func withRetry(step map[string]any) map[string]any {
	step["retry"] = map[string]any{"maxAttempts": 3, "backoffSeconds": 1, "on": []string{"infrastructure", "transient"}}
	return step
}

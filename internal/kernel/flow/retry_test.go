package flow_test

// Targeted retry-convergence coverage for S02 §3. An identical request answer
// or container output retry returns the recorded result even while the
// execution itself is still running with pending successors; new content is
// refused. The other reducer tests only exercise this after the execution
// has already reached a terminal state, so these cases pin the running path.

import (
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

func TestIdenticalRetryConvergesWhileSuccessorsArePending(t *testing.T) {
	at := baseTime

	t.Run("request answer", func(t *testing.T) {
		review := requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
		consume := requestAction("act-1", "consume", 1, time.Hour, literalSubject(map[string]string{"operation": "consume"}), "result")
		exec := runningRequestExecution(review, consume)
		answer := kernel.Answer{
			Decision: kernel.DecisionPass,
			Outputs:  kernel.Outputs{id.OutputKey("decision"): "approve"},
			Actor:    id.PrincipalID("principal-1"),
			At:       at.Add(time.Minute),
		}

		next, _ := mustReduce(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at.Add(2 * time.Minute)})
		if next.State != kernel.ExecutionRunning || next.Actions[1].State != kernel.ActionPending {
			t.Fatalf("execution/action1 = %q/%q, want running/pending", next.State, next.Actions[1].State)
		}
		mustNoop(t, next, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at.Add(3 * time.Minute)})

		replacement := answer
		replacement.Decision = kernel.DecisionFail
		mustRefuse(t, next, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: replacement, At: at.Add(3 * time.Minute)}, fault.ChangedIdempotencyContent)
	})

	t.Run("container output", func(t *testing.T) {
		exec := runningContainerExecution(
			containerAction("act-0", "build", 0, time.Hour, "candidate"),
			containerAction("act-1", "test", 1, time.Hour),
		)
		submitted := kernel.Outputs{id.OutputKey("candidate"): "abc"}
		next, _ := mustReduce(t, exec, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: submitted, At: at.Add(time.Minute)})
		next, _ = mustReduce(t, next, flow.ObserveFact{
			ExecutionID: exec.ID,
			ActionID:    "act-0",
			Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("termination observed")),
			At:          at.Add(2 * time.Minute),
		})
		if next.State != kernel.ExecutionRunning || next.Actions[0].State != kernel.ActionSucceeded {
			t.Fatalf("execution/action0 = %q/%q, want running/succeeded", next.State, next.Actions[0].State)
		}
		mustNoop(t, next, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: submitted, At: at.Add(3 * time.Minute)})

		different := kernel.Outputs{id.OutputKey("candidate"): "other"}
		mustRefuse(t, next, flow.OutputFact{ExecutionID: exec.ID, ActionID: "act-0", Outputs: different, At: at.Add(3 * time.Minute)}, fault.ChangedIdempotencyContent)
	})
}

// TestIdenticalAnswerRetryIgnoresTimestampAndProvenance pins S02 §3: identical
// request retries return the recorded result, and sender timestamps are ignored
// metadata. Only the settlement content (decision, outputs, principal) decides
// whether a retry is identical, so a re-delivery that differs solely in
// Answer.At, fact time or Provenance converges on the recorded result with no
// second commit.
func TestIdenticalAnswerRetryIgnoresTimestampAndProvenance(t *testing.T) {
	at := baseTime
	review := requestAction("act-0", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
	consume := requestAction("act-1", "consume", 1, time.Hour, literalSubject(map[string]string{"operation": "consume"}), "result")
	exec := runningRequestExecution(review, consume)
	answer := kernel.Answer{
		Decision:   kernel.DecisionPass,
		Outputs:    kernel.Outputs{id.OutputKey("decision"): "approve"},
		Actor:      id.PrincipalID("principal-1"),
		At:         at.Add(time.Minute),
		Provenance: []byte("first-transport-envelope"),
	}

	settled, _ := mustReduce(t, exec, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: answer, At: at.Add(2 * time.Minute)})
	if settled.Actions[0].Request == nil || settled.Actions[0].Request.Answer == nil {
		t.Fatal("passing answer was not recorded")
	}
	recorded := *settled.Actions[0].Request.Answer

	// The retry re-delivers the same settlement content with a different sender
	// timestamp, a different transport provenance and an authoritative fact time
	// past the persisted deadline. Retry convergence must not depend on either.
	retry := answer
	retry.At = at.Add(90 * time.Minute)
	retry.Provenance = []byte("second-transport-envelope")
	converged := mustNoop(t, settled, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: retry, At: at.Add(120 * time.Minute)})
	if converged.Actions[0].Request == nil || converged.Actions[0].Request.Answer == nil {
		t.Fatal("converged retry dropped the recorded answer")
	}
	if got := *converged.Actions[0].Request.Answer; !reflect.DeepEqual(got, recorded) {
		t.Fatalf("recorded answer = %+v, want the first settlement %+v", got, recorded)
	}
	if converged.Version != settled.Version {
		t.Fatalf("converged retry advanced the execution version to %d, want %d", converged.Version, settled.Version)
	}
	if converged.Actions[0].State != kernel.ActionSucceeded || converged.Actions[0].Outputs[id.OutputKey("decision")] != "approve" {
		t.Fatalf("converged retry changed the settled result: state=%q outputs=%v", converged.Actions[0].State, converged.Actions[0].Outputs)
	}

	// Replacement settlement content is still refused.
	replacement := retry
	replacement.Decision = kernel.DecisionFail
	mustRefuse(t, converged, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: replacement, At: at.Add(130 * time.Minute)}, fault.ChangedIdempotencyContent)

	newContent := retry
	newContent.Outputs = kernel.Outputs{id.OutputKey("decision"): "other"}
	mustRefuse(t, converged, flow.AnswerFact{ExecutionID: exec.ID, ActionID: "act-0", Answer: newContent, At: at.Add(130 * time.Minute)}, fault.ChangedIdempotencyContent)
}

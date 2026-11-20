package flow_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/kernel/flow"
)

func TestCancelSupersededWhileWaitingAtRequestLeavesNoFailDecision(t *testing.T) {
	at := baseTime
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"operation": {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
		"resource":  {Kind: definition.SubjectLiteral, Literal: "repo-1"},
	})
	exec := newExecution(
		containerAction("act-0", "build", 0, time.Hour),
		requestAction("act-1", "review", 1, time.Hour, subject, "approved"),
		containerAction("act-2", "deploy", 2, time.Hour),
	)
	// Advance act-0 through confirmed success and activate the review request.
	exec, _ = mustReduce(t, exec, flow.ActivateFact{ExecutionID: exec.ID, ActionID: "act-0", At: at})
	exec, _ = mustReduce(t, exec, flow.ObserveFact{
		ExecutionID: exec.ID, ActionID: "act-0",
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("done")),
		At:          at.Add(time.Minute),
	})
	exec, _ = mustReduce(t, exec, flow.ActivateFact{
		ExecutionID: exec.ID, ActionID: "act-1",
		At: at.Add(2 * time.Minute),
	})
	if exec.Actions[1].State != kernel.ActionRunning {
		t.Fatalf("review state = %q; want running", exec.Actions[1].State)
	}

	next, commit := mustReduce(t, exec, flow.CancelFact{
		ExecutionID: exec.ID,
		Reason:      flow.CancelSuperseded,
		Detail:      "rebased onto latest main",
		At:          at.Add(3 * time.Minute),
	})

	if next.State != kernel.ExecutionCancelled {
		t.Fatalf("next state = %q; want cancelled", next.State)
	}
	if commit.NextExecutionState != kernel.ExecutionCancelled {
		t.Fatalf("commit next execution state = %q; want cancelled", commit.NextExecutionState)
	}
	if commit.Failure == nil || commit.Failure.Kind != kernel.FailureCancelled ||
		commit.Failure.Reason != string(flow.CancelSuperseded) ||
		commit.Failure.Detail != "rebased onto latest main" {
		t.Fatalf("commit failure = %+v; want cancelled/superseded/reason", commit.Failure)
	}
	if commit.Answer != nil {
		t.Fatalf("superseded cancel fabricated a request answer: %+v", commit.Answer)
	}
	if next.Actions[1].State != kernel.ActionCancelled {
		t.Fatalf("review action state = %q; want cancelled", next.Actions[1].State)
	}
	if next.Actions[1].Request.Answer != nil {
		t.Fatalf("review action carries a fabricated answer: %+v", next.Actions[1].Request.Answer)
	}
	if next.Actions[2].State != kernel.ActionCancelled {
		t.Fatalf("pending deploy action state = %q; want cancelled", next.Actions[2].State)
	}
	if commit.Stop != nil {
		t.Fatalf("request wait cancel emitted container stop intent: %+v", commit.Stop)
	}
	if commit.Cleanup == nil || commit.Cleanup.Reason != "superseded" {
		t.Fatalf("cleanup intent = %+v; want reason superseded", commit.Cleanup)
	}
	if !commit.InvalidateCredential {
		t.Fatal("superseded cancel did not invalidate execution credential")
	}

	// Identical superseded retry converges (no-op).
	mustNoop(t, next, flow.CancelFact{
		ExecutionID: exec.ID,
		Reason:      flow.CancelSuperseded,
		Detail:      "rebased onto latest main",
		At:          at.Add(4 * time.Minute),
	})
	// Different detail or reason conflicts with settled cancellation.
	mustRefuse(t, next, flow.CancelFact{
		ExecutionID: exec.ID,
		Reason:      flow.CancelSuperseded,
		Detail:      "different reason",
		At:          at.Add(4 * time.Minute),
	}, fault.ChangedIdempotencyContent)
	mustRefuse(t, next, flow.CancelFact{
		ExecutionID: exec.ID,
		Reason:      flow.CancelRequested,
		At:          at.Add(4 * time.Minute),
	}, fault.ChangedIdempotencyContent)
}

func TestCancelSupersededOnRunningContainerEmitsStopAndFailure(t *testing.T) {
	at := baseTime
	exec := runningContainerExecution(
		containerAction("act-0", "build", 0, time.Hour),
		containerAction("act-1", "test", 1, time.Hour),
	)
	exec.Actions[0].Container.WorkloadID = id.WorkloadID("wl-1")
	exec.Actions[0].Container.Launch = kernel.LaunchIntent{WorkloadID: "wl-1", State: kernel.LaunchIssued}

	next, commit := mustReduce(t, exec, flow.CancelFact{
		ExecutionID: exec.ID,
		Reason:      flow.CancelSuperseded,
		Detail:      "superseded by fresh attempt",
		At:          at.Add(time.Minute),
	})
	if next.State != kernel.ExecutionCancelled {
		t.Fatalf("next state = %q; want cancelled", next.State)
	}
	if commit.Stop == nil || commit.Stop.WorkloadID != "wl-1" {
		t.Fatalf("stop intent = %+v; want workload wl-1", commit.Stop)
	}
	if commit.Failure == nil || commit.Failure.Kind != kernel.FailureCancelled || commit.Failure.Reason != "superseded" || commit.Failure.Detail != "superseded by fresh attempt" {
		t.Fatalf("commit failure = %+v; want cancelled/superseded", commit.Failure)
	}
	if len(commit.PendingCancellations) != 1 || commit.PendingCancellations[0].ActionID != "act-1" {
		t.Fatalf("pending cancellations = %+v; want act-1", commit.PendingCancellations)
	}
}

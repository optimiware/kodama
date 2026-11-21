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

func TestReduceAnswerReviseSetsDomainRejectionAndNeverPromotesOutputs(t *testing.T) {
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"operation": {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
		"candidate": {Kind: definition.SubjectLiteral, Literal: "sha-1"},
	})
	exec := kernel.Execution{
		ID:            "exec-rev-1",
		Version:       2,
		State:         kernel.ExecutionRunning,
		WorkspaceID:   "ws-rev-1",
		CurrentAction: 0,
		Actions: []kernel.Action{
			{
				ID:          "act-review",
				Index:       0,
				Name:        "review",
				Kind:        definition.ActionKindRequest,
				State:       kernel.ActionRunning,
				Version:     2,
				Timeout:     time.Hour,
				ActivatedAt: baseTime,
				Deadline:    baseTime.Add(time.Hour),
				Request: &kernel.RequestAction{
					Definition: definition.Request{
						Assignment: "role/reviewer",
						Subject:    subject,
						Outputs:    []id.OutputKey{"feedback"},
					},
					Subject: &subject,
				},
			},
			containerAction("act-integrate", "integrate", 1, time.Hour),
		},
	}

	answer := kernel.Answer{
		Decision: kernel.DecisionRevise,
		Outputs:  kernel.Outputs{"feedback": "Please add unit tests for edge cases"},
		Note:     "Missing edge case tests",
		Actor:    "reviewer-1",
		At:       baseTime.Add(5 * time.Minute),
	}
	next, commit, err := flow.Reduce(exec, flow.AnswerFact{
		ExecutionID: exec.ID,
		ActionID:    "act-review",
		Answer:      answer,
		At:          baseTime.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Reduce revise: %v", err)
	}
	if commit == nil {
		t.Fatal("expected non-nil commit for revise decision")
	}
	if next.State != kernel.ExecutionFailed {
		t.Fatalf("execution state = %q; want failed", next.State)
	}
	if next.Actions[0].State != kernel.ActionFailed {
		t.Fatalf("request action state = %q; want failed", next.Actions[0].State)
	}
	if next.Actions[1].State != kernel.ActionCancelled {
		t.Fatalf("successor action state = %q; want cancelled", next.Actions[1].State)
	}
	if next.Actions[0].Outputs != nil {
		t.Fatalf("revise must never promote downstream outputs; got %v", next.Actions[0].Outputs)
	}
	if next.Actions[0].Request.Answer == nil || next.Actions[0].Request.Answer.Decision != kernel.DecisionRevise {
		t.Fatalf("recorded answer = %+v; want decision=revise", next.Actions[0].Request.Answer)
	}
	if next.Actions[0].Request.Answer.DownstreamEligible() {
		t.Fatal("revise answer must not be downstream eligible")
	}
	if next.Failure == nil {
		t.Fatal("expected failure diagnostic on revise")
	}
	if next.Failure.Kind != kernel.FailureRuntime || next.Failure.Reason != kernel.FailureReasonNeedsRevision || next.Failure.Detail != "Missing edge case tests" {
		t.Fatalf("failure = %+v; want runtime/needs_revision/'Missing edge case tests'", next.Failure)
	}
	if !next.Failure.IsDomainRejection() {
		t.Fatal("needs_revision failure must classify as a domain rejection")
	}
	if !kernel.IsDomainRejectionReason(kernel.FailureReasonBudgetExceeded) {
		t.Fatal("budget_exceeded must classify as a domain rejection")
	}
	if kernel.IsDomainRejectionReason("answer_rejected") || kernel.IsDomainRejectionReason("workload_exited") {
		t.Fatal("system failure reasons must not classify as domain rejections")
	}

	// Identical retry converges with no-op commit.
	same, replayCommit, err := flow.Reduce(next, flow.AnswerFact{
		ExecutionID: exec.ID,
		ActionID:    "act-review",
		Answer:      answer,
		At:          baseTime.Add(6 * time.Minute),
	})
	if err != nil || replayCommit != nil {
		t.Fatalf("identical revise retry = (%v, %v); want (nil commit, nil err)", replayCommit, err)
	}
	if same.Version != next.Version {
		t.Fatalf("replay mutated version %d -> %d", next.Version, same.Version)
	}

	// Conflicting replacement decision is refused.
	for _, other := range []kernel.Decision{kernel.DecisionPass, kernel.DecisionFail} {
		replacement := answer
		replacement.Decision = other
		_, _, err := flow.Reduce(next, flow.AnswerFact{
			ExecutionID: exec.ID,
			ActionID:    "act-review",
			Answer:      replacement,
			At:          baseTime.Add(6 * time.Minute),
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
			t.Fatalf("replacement decision %q err = %v; want changed_idempotency_content", other, err)
		}
	}
}

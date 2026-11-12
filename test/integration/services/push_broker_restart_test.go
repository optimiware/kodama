//go:build integration

package services

// Push trigger: broker-down push start and restart rescan without duplicates.
//
// With the real NATS container stopped:
//   - a human push still selects and starts its pipeline from the durable event
//     table, the execution runs to success, and the publication backlog grows;
//   - a later push is held at the admission boundary after its first selection
//     is durable, then the controller is restarted while the broker is still
//     down. The new controller rescans from the zero event sequence, resumes the
//     persisted first selection, and starts exactly one execution;
//   - the pre-restart execution is never replayed, and after the broker returns
//     the backlog drains with one stored message per committed event.
//
// The admission hold is a labelled resolver seam; every selection, receipt,
// execution, event and publication is the real product path.

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/factory"
)

// TestPushBrokerDownStartAndRestartRescan proves broker-independent push starts
// and a restart rescan that resumes without duplicating an earlier execution.
func TestPushBrokerDownStartAndRestartRescan(t *testing.T) {
	resolver := newAttemptResolver(factory.DigestResolver{})
	fixture := startPushHarness(t, factory.WithImageResolver(resolver))

	const (
		refA        = "refs/heads/work/broker-down-a"
		refB        = "refs/heads/work/broker-restart-b"
		serviceID   = "broker-push-starter"
		pipelineA   = "broker-down"
		pipelineB   = "broker-restart"
		catchUpWait = 5 * time.Minute
	)
	fixture.createPushService(t, serviceID)

	directory := fixture.clone(t, fixture.engineerToken, "broker")
	commitA := fixture.commit(t, directory, "broker-down push")
	commitB := fixture.commit(t, directory, "restart rescan push")

	documentA := pushPipelineDocument(fixture.git.ProjectResourceID, serviceID,
		fixture.Image.Reference, refA, commitA)
	appliedA := fixture.applyPushPipeline(t, pipelineA, documentA)
	documentB := pushPipelineDocument(fixture.git.ProjectResourceID, serviceID,
		fixture.Image.Reference, refB, commitB)
	appliedB := fixture.applyPushPipeline(t, pipelineB, documentB)

	// Stop the real broker. The push start, admission, dispatch and execution
	// must all proceed from PostgreSQL.
	fixture.NATS.Stop(t)
	requireBrokerUnreachable(t, fixture.Harness)

	if stderr, code := fixture.push(directory, fixture.engineerToken, commitA+":"+refA); code != 0 {
		t.Fatalf("broker-down push %s: exit %d: %s", refA, code, stderr)
	}
	executionsA := fixture.waitPushExecutionCount(t, appliedA.PipelineID, 1)
	t.Cleanup(func() { fixture.stopPushExecution(t, executionsA[0].ExecutionID) })
	fixture.waitPushExecutionSucceeded(t, executionsA[0].ExecutionID)
	if admitted := fixture.executionSnapshotYAML(t, executionsA[0].ExecutionID); string(admitted) != string(documentA) {
		t.Fatalf("broker-down execution admitted YAML = %q; want %q", admitted, documentA)
	}
	pending, err := fixture.PendingPublications(t.Context())
	if err != nil {
		t.Fatalf("read pending publications: %v", err)
	}
	if pending == 0 {
		t.Fatal("no publication intent is pending while the broker is down")
	}

	// Hold the second push's first admission attempt after its selection is
	// durable, then restart the controller while the broker stays down.
	resolver.armHold()
	if stderr, code := fixture.push(directory, fixture.engineerToken, commitB+":"+refB); code != 0 {
		t.Fatalf("restart-rescan push %s: exit %d: %s", refB, code, stderr)
	}
	resolver.waitFirst(t)
	if executions := fixture.pushPipelineExecutions(t, appliedB.PipelineID); len(executions) != 0 {
		t.Fatalf("pipeline %s started %d executions before the restart rescan", appliedB.PipelineID, len(executions))
	}

	resolver.disarm()
	fixture.RestartController()
	if !fixture.BrokerUnreachable(t.Context()) {
		t.Fatal("the broker became reachable during the controller restart window")
	}

	executionsB := fixture.waitPushExecutionCount(t, appliedB.PipelineID, 1)
	t.Cleanup(func() { fixture.stopPushExecution(t, executionsB[0].ExecutionID) })
	fixture.waitPushExecutionSucceeded(t, executionsB[0].ExecutionID)
	if admitted := fixture.executionSnapshotYAML(t, executionsB[0].ExecutionID); string(admitted) != string(documentB) {
		t.Fatalf("restart-rescan execution admitted YAML = %q; want the persisted first selection %q",
			admitted, documentB)
	}

	// The earlier execution survived the restart unchanged and was never
	// replayed by the rescan.
	afterRestartA := fixture.pushPipelineExecutions(t, appliedA.PipelineID)
	if len(afterRestartA) != 1 || afterRestartA[0].ExecutionID != executionsA[0].ExecutionID {
		t.Fatalf("pipeline %s executions after restart = %+v; want the original %s only",
			appliedA.PipelineID, afterRestartA, executionsA[0].ExecutionID)
	}
	queuedA := fixture.queuedEvents(t, executionsA[0].ExecutionID)
	if len(queuedA) != 1 {
		t.Fatalf("execution %s has %d queued events after restart; want one",
			executionsA[0].ExecutionID, len(queuedA))
	}
	queuedB := fixture.queuedEvents(t, executionsB[0].ExecutionID)
	if len(queuedB) != 1 {
		t.Fatalf("execution %s has %d queued events; want one", executionsB[0].ExecutionID, len(queuedB))
	}

	// The broker returns. The backlog must drain with the original identities,
	// and no execution may be duplicated by the catch-up.
	fixture.NATS.Start(t)
	fixture.Eventually(t, catchUpWait, func() bool {
		pending, err := fixture.PendingPublications(t.Context())
		return err == nil && pending == 0
	})
	if a := fixture.pushPipelineExecutions(t, appliedA.PipelineID); len(a) != 1 {
		t.Fatalf("pipeline %s has %d executions after catch-up; want one", appliedA.PipelineID, len(a))
	}
	if b := fixture.pushPipelineExecutions(t, appliedB.PipelineID); len(b) != 1 {
		t.Fatalf("pipeline %s has %d executions after catch-up; want one", appliedB.PipelineID, len(b))
	}

	stream, _, err := fixture.EventStream(t.Context())
	if err != nil {
		t.Fatalf("read events stream after catch-up: %v", err)
	}
	seen := make(map[string]int, len(stream))
	for _, stored := range stream {
		seen[stored.MessageID]++
	}
	for _, eventID := range []string{queuedA[0].ID, queuedB[0].ID} {
		if seen[eventID] != 1 {
			t.Fatalf("event %s is stored %d times in the events stream; want exactly once", eventID, seen[eventID])
		}
	}
}

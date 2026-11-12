//go:build integration

package services

// AC18: an old event replay does not overwrite the current task state.
//
// The scenario uses the real factory composition and the real push processor,
// which deliberately keeps no in-memory cursor: every pass rescans accepted
// events from the zero sequence, so a controller restart replays the durable
// event table over work that is already done.
//
//   1. A verified git.ref.updated report for a registered resource is accepted
//      through the served verified-report ingress and starts the subscribed
//      push pipeline once (P1).
//   2. A task is created afterwards with its own current execution (T1). The
//      task is open and its request action is running.
//   3. The controller restarts. The trigger rescan replays the old event row
//      from zero and the settlement loop rescans durable terminal executions.
//      Neither may start another push execution, advance the task, replace its
//      current execution or rewrite its history.
//   4. A genuinely new verified report then starts exactly one more execution,
//      which proves the post-restart processor is live and that the absence of
//      a replay was a decision, not a stalled loop.
//
// Fixture limit: the Git effect is represented by a real verified report over a
// registered resource (actor.verify plus resource.report), not by a native Git
// push. The event, verification, receipt, trigger selection and execution are
// the real product path; real native Git reporting is proved by the push
// trigger and protected-factory scenarios.

import (
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	oldEventRepository   = "replay-repo"
	oldEventPushService  = "replay-pusher"
	oldEventBindingID    = "replay-binding"
	oldEventRef          = "refs/heads/main"
	oldEventPushPipeline = "replay-push"
	oldEventTaskPipeline = "replay-task"

	oldEventWorkflowTimeout = 3 * time.Minute
	oldEventStableWindow    = 2 * time.Second
)

// TestOldEventReplayPreservesCurrentTask proves that replaying an accepted
// push event after newer task work exists neither duplicates the push execution
// nor overwrites the task's current state.
func TestOldEventReplayPreservesCurrentTask(t *testing.T) {
	harness := factory.Start(t)
	serviceToken := oldEventCreatePushService(t, harness)

	// The verified-report ingress needs the registered resource, its exact
	// binding and the reporting service credential.
	oldEventRegisterBinding(t, harness)

	pushDocument := oldEventPushDocument(t, harness, "echo old-event-push-ok")
	pushApplied := oldEventApplyPipeline(t, harness, oldEventPushPipeline, pushDocument)

	// The old event starts exactly one push execution.
	oldEventReportPush(t, harness, serviceToken, oldEventRef, "1111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222", "first")
	first := oldEventWaitPushExecutions(t, harness, pushApplied.PipelineID, 1)[0]
	firstSettled := waitExecutionSettled(t, harness, first.ExecutionID)
	if firstSettled.State != "succeeded" {
		t.Fatalf("first push execution state = %q; want succeeded (failure=%+v)", firstSettled.State, firstSettled.Failure)
	}

	// Newer work: a task with its own current execution and a running request
	// action. It is created strictly after the old push event.
	taskApplied := oldEventApplyPipeline(t, harness, oldEventTaskPipeline, oldEventTaskDocument())
	created := harness.MustREST(harness.ProjectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "current task",
		PipelineID: taskApplied.PipelineID,
	}, nil, "old-event-task-create")
	var started api.TaskCreateAndStartResult
	harness.Decode(created, &started)
	waitRequestRunning(t, harness, started.ExecutionID, "wait")
	before := oldEventReadTask(t, harness, started.TaskID)
	if before.State != "open" {
		t.Fatalf("task state before the replay = %q; want open", before.State)
	}
	if before.CurrentExecution != started.ExecutionID {
		t.Fatalf("task current execution before the replay = %q; want %q", before.CurrentExecution, started.ExecutionID)
	}
	if len(before.ExecutionHistory) != 1 || before.ExecutionHistory[0] != started.ExecutionID {
		t.Fatalf("task history before the replay = %v; want [%s]", before.ExecutionHistory, started.ExecutionID)
	}
	version := before.LifecycleVersion

	// Restart the controller: the event table is rescanned from the zero
	// sequence and the old event is replayed. The completed receipt and cursor
	// must keep it from starting another execution or touching newer work.
	harness.RestartController()

	// The replay must be a no-op for the observed window. A duplicate start or
	// any task rewrite during these passes fails the invariant immediately.
	oldEventHoldStable(t, oldEventStableWindow, func() {
		executions := oldEventPushExecutions(t, harness, pushApplied.PipelineID)
		if len(executions) != 1 || executions[0].ExecutionID != first.ExecutionID {
			t.Fatalf("push executions after the old-event replay = %+v; want only the original %s",
				executions, first.ExecutionID)
		}
		after := oldEventReadTask(t, harness, started.TaskID)
		if after.State != "open" ||
			after.CurrentExecution != started.ExecutionID ||
			after.LifecycleVersion != version ||
			len(after.ExecutionHistory) != 1 ||
			after.ExecutionHistory[0] != started.ExecutionID {
			t.Fatalf("task after the old-event replay = %+v; want the current task open at version %d with current %s and history [%s]",
				after, version, started.ExecutionID, started.ExecutionID)
		}
	})

	// A genuinely new event after the restart starts exactly one more
	// execution. This proves the restarted processor is advancing, so the old
	// event was skipped by its durable completion, not left unprocessed.
	oldEventReportPush(t, harness, serviceToken, oldEventRef, "3333333333333333333333333333333333333333",
		"4444444444444444444444444444444444444444", "second")
	executions := oldEventWaitPushExecutions(t, harness, pushApplied.PipelineID, 2)
	var second api.ExecutionSummary
	for _, execution := range executions {
		if execution.ExecutionID != first.ExecutionID {
			second = execution
		}
	}
	if second.ExecutionID == "" {
		t.Fatalf("the new event did not start a distinct execution; executions = %+v", executions)
	}
	secondSettled := waitExecutionSettled(t, harness, second.ExecutionID)
	if secondSettled.State != "succeeded" {
		t.Fatalf("second push execution state = %q; want succeeded (failure=%+v)", secondSettled.State, secondSettled.Failure)
	}

	// The old execution and the task state are still exactly as they were.
	finalExecutions := oldEventPushExecutions(t, harness, pushApplied.PipelineID)
	if len(finalExecutions) != 2 {
		t.Fatalf("push executions after the new event = %+v; want the old and the new execution", finalExecutions)
	}
	after := oldEventReadTask(t, harness, started.TaskID)
	if after.State != "open" ||
		after.CurrentExecution != started.ExecutionID ||
		after.LifecycleVersion != version ||
		len(after.ExecutionHistory) != 1 ||
		after.ExecutionHistory[0] != started.ExecutionID {
		t.Fatalf("task after the new event = %+v; want the same current task open at version %d with current %s",
			after, version, started.ExecutionID)
	}
}

// oldEventCreatePushService creates one engineer-role automatic-start service
// and returns its issued credential.
func oldEventCreatePushService(t *testing.T, harness *factory.Harness) string {
	t.Helper()
	created := harness.MustREST(harness.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: oldEventPushService,
		Roles:     []string{"engineer"},
	}, nil, "old-event-service-create")
	var service api.ServiceCreateResult
	harness.Decode(created, &service)
	if service.Version == 0 {
		t.Fatal("service.create returned version 0")
	}
	issued := harness.MustREST(harness.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  oldEventPushService,
		TTLSeconds: 3600,
	}, &service.Version, "old-event-service-credential")
	var credential api.ServiceCredentialIssueResult
	harness.Decode(issued, &credential)
	if credential.Token == nil || *credential.Token == "" {
		t.Fatal("service.credential.issue returned no token")
	}
	return *credential.Token
}

// oldEventRegisterBinding registers the repository resource the verified
// reports cite and binds it to the push service.
func oldEventRegisterBinding(t *testing.T, harness *factory.Harness) {
	t.Helper()
	harness.MustREST(harness.ProjectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        factory.ProjectID,
		BindingID:        oldEventBindingID,
		ResourceID:       oldEventRepository,
		EnforcingService: oldEventPushService,
		TrustedOrigins:   []string{"https://git.example.test"},
		Operations:       []string{"git.ref.update"},
		ReportableEvents: []string{"git.ref.updated"},
	}, nil, "old-event-binding-register")
	// The registered binding is readable through the same public edge after the
	// service credential was issued.
	read := harness.MustREST(harness.ProjectToken, "resource.binding.read", api.ResourceBindingReadRequest{
		BindingID: oldEventBindingID,
	}, nil, "")
	var binding api.ResourceBindingReadResult
	harness.Decode(read, &binding)
	if binding.ResourceID != oldEventRepository || binding.EnforcingService != oldEventPushService {
		t.Fatalf("registered binding = %+v; want resource %s enforced by %s", binding, oldEventRepository, oldEventPushService)
	}
}

// oldEventReportPush publishes one source observation, verifies the human actor
// through the service credential and submits the verified git.ref.updated
// report. It returns the accepted event identity.
func oldEventReportPush(t *testing.T, harness *factory.Harness, serviceToken, ref, base, candidate, key string) string {
	t.Helper()
	source := harness.MustREST(harness.ProjectToken, "event.publish", api.EventPublishRequest{
		ProjectID: factory.ProjectID,
		Type:      "observation.custom",
		SubjectID: oldEventRepository,
		Data:      []byte(fmt.Sprintf(`{"ref":%q}`, ref)),
	}, nil, "old-event-source-"+key)
	var published api.EventPublishResult
	harness.Decode(source, &published)

	subject := oldEventGitSubject(ref, base, candidate)
	verified := harness.MustREST(serviceToken, "actor.verify", api.VerifyRequest{
		ActorCredential: harness.ProjectToken,
		ResourceID:      oldEventRepository,
		Operation:       "git.ref.update",
		Subject:         subject,
	}, nil, "")
	var verification api.VerifyResponse
	harness.Decode(verified, &verification)
	if verification.VerificationID == "" {
		t.Fatal("actor.verify returned no verification identity")
	}

	reported := harness.MustREST(serviceToken, "resource.report", api.ReportRequest{
		ResourceID:     oldEventRepository,
		SourceEventID:  published.EventID,
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: verification.VerificationID,
		Subject:        subject,
	}, nil, "old-event-report-"+key)
	var report api.ReportResponse
	harness.Decode(reported, &report)
	if report.EventID == "" {
		t.Fatal("resource.report returned no accepted event identity")
	}
	return report.EventID
}

// oldEventGitSubject is the exact typed effect subject of one ref update.
func oldEventGitSubject(ref, base, candidate string) api.Subject {
	return api.Subject{Fields: []api.SubjectField{
		{Name: "operation", Kind: "string", Value: "git.ref.update"},
		{Name: "resource", Kind: "string", Value: oldEventRepository},
		{Name: "ref", Kind: "string", Value: ref},
		{Name: "expected_base", Kind: "string", Value: base},
		{Name: "candidate", Kind: "string", Value: candidate},
	}}
}

// oldEventPushDocument renders one already-subscribed push pipeline.
func oldEventPushDocument(t *testing.T, harness *factory.Harness, command string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`repository: %s
service: %s
on:
  push:
    - %s
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: check
    image: %s
    timeout: 10m
    commands:
      - test "$(cat /kodama/inputs/repository)" = %s
      - test "$(cat /kodama/inputs/ref)" = %s
      - "%s"
`, oldEventRepository, oldEventPushService, oldEventRef, harness.Image.Reference, oldEventRepository, oldEventRef, command))
}

// oldEventTaskDocument renders one task pipeline that stays open on a running
// operator request.
func oldEventTaskDocument() []byte {
	return []byte(`actions:
  - name: wait
    request: role/operator
    subject:
      operation: replay.diagnostic
    timeout: 1h
`)
}

// oldEventApplyPipeline posts one pipeline and returns its identity.
func oldEventApplyPipeline(t *testing.T, harness *factory.Harness, name string, document []byte) api.PipelineApplyResult {
	t.Helper()
	applied := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "old-event-apply-"+name)
	var result api.PipelineApplyResult
	harness.Decode(applied, &result)
	if result.PipelineID == "" || result.Version == 0 {
		t.Fatalf("pipeline.apply %s = %+v; want a committed pipeline", name, result)
	}
	return result
}

// oldEventPushExecutions lists the executions of one push pipeline.
func oldEventPushExecutions(t *testing.T, harness *factory.Harness, pipelineID string) []api.ExecutionSummary {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "execution.list", api.ExecutionListRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, "")
	var list api.ExecutionListResult
	harness.Decode(result, &list)
	return list.Executions
}

// oldEventWaitPushExecutions waits for one push pipeline to reach an exact
// execution count and returns the executions.
func oldEventWaitPushExecutions(t *testing.T, harness *factory.Harness, pipelineID string, want int) []api.ExecutionSummary {
	t.Helper()
	var found []api.ExecutionSummary
	harness.Eventually(t, oldEventWorkflowTimeout, func() bool {
		current := oldEventPushExecutions(t, harness, pipelineID)
		if len(current) != want {
			return false
		}
		found = current
		return true
	})
	return found
}

// oldEventReadTask reads one task through the public edge.
func oldEventReadTask(t *testing.T, harness *factory.Harness, taskID string) api.TaskReadResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "task.read", api.TaskReadRequest{
		TaskID: taskID,
	}, nil, "")
	var task api.TaskReadResult
	harness.Decode(result, &task)
	return task
}

// oldEventHoldStable runs one invariant over a bounded real-time window. It is
// a stability hold, not a race deadline: a background pass that would duplicate
// or rewrite observed state must fail during the window.
func oldEventHoldStable(t *testing.T, window time.Duration, assert func()) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		assert()
		time.Sleep(50 * time.Millisecond)
	}
}

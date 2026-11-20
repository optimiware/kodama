//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/test/e2e/internal/harness"
)

// TestE2E_AC21V06Capabilities exercises the v0.6.0 capabilities end-to-end
// against a real installation:
//   - #40: /kodema/task/{title,description,metadata.json,spec.json} read-only
//     materialization inside a live container workload;
//   - #48: atomic multi-stage task.start (pipeline switch + output handoff)
//     and task.restart superseding an active waiting execution cleanly;
//   - #38 & #46: human OIDC event.subscribe and live HTTP SSE event streaming
//     with Last-Event-ID / since cursor resume and zero duplicates;
//   - #43: first-class project.list (caller-tenant default) and repository.list.
func TestE2E_AC21V06Capabilities(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	tenant, project := liveScope(t, instance)

	// 1. Verify #43 first-class project.list (defaulting caller tenant via
	//    GET /api/v1/projects) and repository.list against the real installation.
	rt, err := harness.NewVerifiedTransport(instance.config)
	if err != nil {
		t.Fatalf("NewVerifiedTransport: %v", err)
	}
	tr := &client.HTTPTransport{
		BaseURL:      instance.config.APIURL,
		RoundTripper: rt,
		Source:       staticCredentialSource{token: engineer, expiry: time.Now().UTC().Add(time.Hour)},
		Retry:        client.RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second},
	}
	projRes, err := tr.Invoke(ctx, api.Invocation{
		Operation: "project.list",
		Request:   api.Request{Input: []byte("{}")},
	})
	if err != nil {
		t.Fatalf("project.list (tenant-defaulting route): %v", err)
	}
	projList := decodeResult[api.ProjectListResult](t, projRes)
	foundProject := false
	for _, p := range projList.Projects {
		if p.ProjectID == instance.projectID {
			foundProject = true
			break
		}
	}
	if !foundProject {
		t.Fatalf("project.list did not include %q (got %+v)", instance.projectID, projList.Projects)
	}
	repoRes := instance.smokeCall(ctx, t, engineer, api.MethodGet, "repository.list", api.RepositoryListRequest{
		ProjectID: instance.projectID,
	}, "", nil)
	repoList := decodeResult[api.RepositoryListResult](t, repoRes)
	if len(repoList.Repositories) == 0 {
		t.Fatalf("repository.list returned no repositories for project %q", instance.projectID)
	}

	// 2. Verify #38 human OIDC event.subscribe succeeds with the project's
	//    scoped subject and a live expiry.
	subRes := instance.smokeCall(ctx, t, engineer, api.MethodPost, "event.subscribe",
		api.EventSubscribeRequest{ProjectID: instance.projectID}, "", nil)
	grant := decodeResult[api.EventSubscribeResult](t, subRes)
	if err := grant.Validate(); err != nil {
		t.Fatalf("human OIDC event.subscribe grant invalid: %v", err)
	}
	wantSubject := liveEventSubject(tenant, project)
	if len(grant.Subjects) != 1 || grant.Subjects[0] != wantSubject {
		t.Fatalf("human OIDC event.subscribe subjects = %v; want [%s]", grant.Subjects, wantSubject)
	}
	if !grant.ExpiresAt.After(started) {
		t.Fatalf("human OIDC event.subscribe expiry = %s; want after %s", grant.ExpiresAt, started)
	}

	// 3. Verify #46 live HTTP SSE stream with human OIDC auth and cursor resume
	//    (zero gaps, zero duplicates).
	stream1, err := tr.StreamEvents(ctx, client.EventStreamRequest{
		ProjectID: instance.projectID,
		Types:     []string{"observation.custom"},
	})
	if err != nil {
		t.Fatalf("StreamEvents initial: %v", err)
	}
	t.Cleanup(func() { _ = stream1.Close() })

	subject1 := "ac21-sse-1-" + instance.runID
	pub1Res := instance.smokeCall(ctx, t, engineer, api.MethodPost, "event.publish", api.EventPublishRequest{
		ProjectID: instance.projectID,
		Type:      "observation.custom",
		SubjectID: subject1,
		Data:      []byte(`{"step":1}`),
	}, instance.key("ac21-sse-1"), nil)
	pub1 := decodeResult[api.EventPublishResult](t, pub1Res)

	evt1 := waitForSSEEvent(ctx, t, stream1, pub1.EventID, 30*time.Second)
	if evt1.SubjectID != subject1 || evt1.Cursor == "" {
		t.Fatalf("SSE event 1 = %+v; want subject %q and non-empty cursor", evt1, subject1)
	}
	_ = stream1.Close()

	// Reconnect from evt1.Cursor: event 1 must not replay, event 2 must arrive.
	stream2, err := tr.StreamEvents(ctx, client.EventStreamRequest{
		ProjectID: instance.projectID,
		Since:     evt1.Cursor,
		Types:     []string{"observation.custom"},
	})
	if err != nil {
		t.Fatalf("StreamEvents resumed: %v", err)
	}
	t.Cleanup(func() { _ = stream2.Close() })

	subject2 := "ac21-sse-2-" + instance.runID
	pub2Res := instance.smokeCall(ctx, t, engineer, api.MethodPost, "event.publish", api.EventPublishRequest{
		ProjectID: instance.projectID,
		Type:      "observation.custom",
		SubjectID: subject2,
		Data:      []byte(`{"step":2}`),
	}, instance.key("ac21-sse-2"), nil)
	pub2 := decodeResult[api.EventPublishResult](t, pub2Res)

	evt2 := waitForSSEEventExcluding(ctx, t, stream2, pub2.EventID, pub1.EventID, 30*time.Second)
	if evt2.SubjectID != subject2 {
		t.Fatalf("SSE event 2 = %+v; want subject %q", evt2, subject2)
	}
	_ = stream2.Close()

	// 4. Verify #40 (/kodema/task/* materialization) + #48 (multi-stage
	//    task.start + task.restart superseding an active execution) in real
	//    Podman workloads.
	taskTitle := "AC21 v0.6 task " + instance.runID
	taskDesc := "Verify /kodema/task materialization and multi-stage restart."
	stageToken := "token-" + instance.runID

	stage1YAML, err := (harness.Document{
		Actions: []harness.Action{{
			Name:    "stage1",
			Image:   instance.image,
			Timeout: "5m",
			Outputs: []string{"stage_token"},
			Commands: []string{
				fmt.Sprintf(`test "$(cat /kodema/task/title)" = %q`, taskTitle),
				fmt.Sprintf(`test "$(cat /kodema/task/description)" = %q`, taskDesc),
				`test "$(cat /kodema/task/metadata.json)" = '{"area":"e2e","release":"0.6.0"}'`,
				`test ! -w /kodema/task/title && test ! -w /kodema/task/spec.json`,
				`cat /kodema/task/spec.json`,
				fmt.Sprintf(`printf '%%s\n' %q > /kodema/outputs/stage1/stage_token`, stageToken),
			},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build stage1 pipeline: %v", err)
	}
	stage2YAML, err := (harness.Document{
		Inputs: []string{"carried"},
		Actions: []harness.Action{{
			Name:    "gate",
			Request: "role/reviewer",
			Timeout: "10m",
			Subject: map[string]any{"question": "ac21-superseded-wait"},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build stage2 pipeline: %v", err)
	}
	stage3YAML, err := (harness.Document{
		Inputs: []string{"carried"},
		Actions: []harness.Action{{
			Name:    "stage3",
			Image:   instance.image,
			Timeout: "5m",
			Commands: []string{
				fmt.Sprintf(`test "$(cat /kodema/inputs/carried)" = %q`, stageToken),
				`echo "ac21-stage3-ok"`,
			},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build stage3 pipeline: %v", err)
	}

	p1 := instance.applyPipeline(t, "ac21-stage1-"+instance.runID, stage1YAML, instance.key("ac21-p1"))
	p2 := instance.applyPipeline(t, "ac21-stage2-"+instance.runID, stage2YAML, instance.key("ac21-p2"))
	p3 := instance.applyPipeline(t, "ac21-stage3-"+instance.runID, stage3YAML, instance.key("ac21-p3"))

	createRes := instance.smokeCall(ctx, t, engineer, api.MethodPost, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:   instance.projectID,
		Title:       taskTitle,
		Description: taskDesc,
		Metadata:    map[string]string{"release": "0.6.0", "area": "e2e"},
		PipelineID:  p1.PipelineID,
	}, instance.key("ac21-create"), nil)
	created := decodeResult[api.TaskCreateAndStartResult](t, createRes)
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, created.ExecutionID)
	})

	stage1View := instance.smokeWaitComplete(ctx, t, engineer, created.ExecutionID, 3*time.Minute)
	requireSucceeded(t, stage1View)
	stage1Action := actionByName(t, stage1View, "stage1")
	if got := stage1Action.Outputs["stage_token"]; got != stageToken {
		t.Fatalf("stage1 output stage_token = %q; want %q", got, stageToken)
	}
	stage1Log := instance.smokeReadLogs(ctx, t, engineer, created.ExecutionID, stage1Action.ActionID, "stdout")
	var specDoc struct {
		ID          string            `json:"id"`
		Title       string            `json:"title"`
		Description string            `json:"description"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(stage1Log, &specDoc); err != nil {
		t.Fatalf("parse /kodema/task/spec.json from stage1 stdout (%q): %v", string(stage1Log), err)
	}
	if specDoc.ID != created.TaskID || specDoc.Title != taskTitle || specDoc.Description != taskDesc || specDoc.Metadata["release"] != "0.6.0" {
		t.Fatalf("spec.json = %+v; want id=%q title=%q", specDoc, created.TaskID, taskTitle)
	}

	// Stage 1 succeeded -> task transitions to done; read current lifecycle version
	// and atomically start Stage 2 on the same task with a new pipeline and input.
	var taskAfterStage1 api.TaskReadResult
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		readRes := instance.smokeCall(ctx, t, engineer, api.MethodGet, "task.read", api.TaskReadRequest{TaskID: created.TaskID}, "", nil)
		taskAfterStage1 = decodeResult[api.TaskReadResult](t, readRes)
		if taskAfterStage1.State == "done" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if taskAfterStage1.State != "done" {
		t.Fatalf("task state after stage1 = %q; want done", taskAfterStage1.State)
	}

	stage2Lifecycle := taskAfterStage1.LifecycleVersion
	stage2Res := instance.smokeCall(ctx, t, engineer, api.MethodPost, "task.start", api.TaskStartRequest{
		TaskID:     created.TaskID,
		PipelineID: p2.PipelineID,
		Inputs:     map[string]string{"carried": stage1Action.Outputs["stage_token"]},
	}, instance.key("ac21-stage2-start"), &stage2Lifecycle)
	stage2Started := decodeResult[api.TaskStartResult](t, stage2Res)
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, stage2Started.ExecutionID)
	})

	// Wait until Stage 2 is actively waiting on its request gate.
	_ = instance.smokeWaitActionRunning(ctx, t, engineer, stage2Started.ExecutionID, "gate", 2*time.Minute)

	// Atomically restart the task onto Stage 3 while Stage 2 is waiting!
	restartVer := stage2Started.LifecycleVersion
	restartRes := instance.smokeCall(ctx, t, engineer, api.MethodPost, "task.restart", api.TaskRestartRequest{
		TaskID:     created.TaskID,
		Reason:     "superseded in ac21 e2e",
		PipelineID: p3.PipelineID,
		Inputs:     map[string]string{"carried": stage1Action.Outputs["stage_token"]},
	}, instance.key("ac21-stage3-restart"), &restartVer)
	restarted := decodeResult[api.TaskRestartResult](t, restartRes)
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, restarted.ExecutionID)
	})
	if restarted.SupersededExecution != stage2Started.ExecutionID {
		t.Fatalf("superseded_execution = %q; want %q", restarted.SupersededExecution, stage2Started.ExecutionID)
	}

	supersededView := instance.smokeWaitComplete(ctx, t, engineer, stage2Started.ExecutionID, 2*time.Minute)
	if supersededView.State != "cancelled" {
		t.Fatalf("superseded execution state = %q; want cancelled", supersededView.State)
	}
	if supersededView.Failure == nil || supersededView.Failure.Reason != "superseded" {
		t.Fatalf("superseded execution failure = %+v; want reason=superseded", supersededView.Failure)
	}

	stage3View := instance.smokeWaitComplete(ctx, t, engineer, restarted.ExecutionID, 3*time.Minute)
	requireSucceeded(t, stage3View)
	stage3Action := actionByName(t, stage3View, "stage3")
	stage3Log := instance.smokeReadLogs(ctx, t, engineer, restarted.ExecutionID, stage3Action.ActionID, "stdout")
	requireLogContains(t, "stage3", stage3Log, "ac21-stage3-ok")

	finalTaskRes := instance.smokeCall(ctx, t, engineer, api.MethodGet, "task.read", api.TaskReadRequest{TaskID: created.TaskID}, "", nil)
	finalTask := decodeResult[api.TaskReadResult](t, finalTaskRes)
	if len(finalTask.ExecutionHistory) != 3 ||
		finalTask.ExecutionHistory[0] != created.ExecutionID ||
		finalTask.ExecutionHistory[1] != stage2Started.ExecutionID ||
		finalTask.ExecutionHistory[2] != restarted.ExecutionID {
		t.Fatalf("final execution history = %v; want [%s %s %s]",
			finalTask.ExecutionHistory, created.ExecutionID, stage2Started.ExecutionID, restarted.ExecutionID)
	}

	finished := time.Now().UTC()
	instance.recordScenario(t, "AC21V06Capabilities", "TestE2E_AC21V06Capabilities", &stage3View, started, finished, map[string]any{
		"task_id":  created.TaskID,
		"event_id": pub2.EventID,
	})
}

func waitForSSEEvent(ctx context.Context, t *testing.T, stream client.EventStream, wantID string, timeout time.Duration) api.StreamEvent {
	t.Helper()
	return waitForSSEEventExcluding(ctx, t, stream, wantID, "", timeout)
}

func waitForSSEEventExcluding(ctx context.Context, t *testing.T, stream client.EventStream, wantID, forbidID string, timeout time.Duration) api.StreamEvent {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		evt, err := stream.Next(waitCtx)
		if err != nil {
			t.Fatalf("SSE stream ended before event %s arrived: %v", wantID, err)
		}
		if forbidID != "" && evt.ID == forbidID {
			t.Fatalf("resumed SSE stream duplicated already-delivered event %s", forbidID)
		}
		if evt.ID == wantID {
			return evt
		}
	}
}

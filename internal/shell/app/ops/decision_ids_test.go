package opsapp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/flow"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/store"
	storeexecution "github.com/optimiware/kodema/internal/shell/store/units/execution"
	"github.com/optimiware/kodema/internal/shell/store/units/readers"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/verify"
)

const reviewStageYAML = `inputs: [repository, target_ref, base, candidate]
completes_task: false
actions:
  - name: review
    request: role/reviewer
    timeout: 2h
    subject:
      operation: git.ref.update
      resource: {from: inputs.repository}
      ref: {from: inputs.target_ref}
      expected_base: {from: inputs.base}
      candidate: {from: inputs.candidate}
    outputs: [approved]
`

const integrateStageYAML = `inputs: [repository, decision_id]
actions:
  - name: push
    image: alpine:latest@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    commands: ["echo push"]
`

const mergeServiceStageYAML = `inputs: [repository, target_ref, base, candidate, decision_id]
actions:
  - name: merge
    request: service/merge-coordinator
    timeout: 1h
    subject:
      repository: {from: inputs.repository}
      ref: {from: inputs.target_ref}
      base: {from: inputs.base}
      candidate: {from: inputs.candidate}
      decision_id: {from: inputs.decision_id}
    outputs: [output]
`

type mutableClock struct {
	now time.Time
}

func (c *mutableClock) Now() time.Time { return c.now }

func TestCrossExecutionApprovalDecisionIDsEndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	setUpVerifyBinding(t, h)

	// Create merge-coordinator and an unrelated service in the same project.
	for _, svcID := range []string{"merge-coordinator", "unrelated-svc"} {
		if _, err := h.invokeKey(t, testToken, "service.create",
			jsonBody(t, map[string]any{"service_id": svcID, "roles": []string{"engineer"}}),
			"create-svc-"+svcID,
			map[string]string{"project": testProject}); err != nil {
			t.Fatalf("service.create(%s): %v", svcID, err)
		}
	}

	// Register a second project for cross-project isolation tests using a tenant-scoped admin.
	h.seedMembership(t, tenancy.Scope{TenantID: testTenant}, "membership-tenant-admin", testEngineer, 1, tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer)
	h.principals.principals["token-tenant-admin"] = identity.Principal{
		ID:      testEngineer,
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant},
		Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
	if _, err := h.invokeKey(t, "token-tenant-admin", "project.create",
		jsonBody(t, map[string]any{"tenant_id": testTenant, "project_id": "project-2", "name": "Other Project"}),
		"create-project-2",
		nil); err != nil {
		t.Fatalf("project.create(project-2): %v", err)
	}
	h.seedResource(t, tenancy.Scope{TenantID: testTenant, ProjectID: "project-2"}, "resource-2", 1)
	h.principals.principals["token-admin-p2"] = identity.Principal{
		ID:      testEngineer,
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: "project-2"},
		Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
	if _, err := h.invokeKey(t, "token-admin-p2", "service.create",
		jsonBody(t, map[string]any{"service_id": "service-p2", "roles": []string{"engineer"}}),
		"create-svc-p2",
		map[string]string{"project": "project-2"}); err != nil {
		t.Fatalf("service.create(service-p2): %v", err)
	}
	h.principals.principals["token-service-p2"] = identity.Principal{
		ID:      "service-p2",
		Kind:    identity.KindService,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: "project-2"},
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	if _, err := h.invokeKey(t, "token-admin-p2", "resource.binding.register", jsonBody(t, map[string]any{
		"binding_id":        "binding-p2",
		"resource_id":       "resource-2",
		"enforcing_service": "service-p2",
		"trusted_origins":   []string{"https://example.test"},
		"operations":        []string{"git.ref.update"},
		"reportable_events": []string{"git.ref.updated"},
	}), "register-binding-p2", map[string]string{"project": "project-2"}); err != nil {
		t.Fatalf("resource.binding.register(p2): %v", err)
	}

	applyPipeline := func(token, project, name, yamlBody string) string {
		t.Helper()
		res, err := h.invokeKey(t, token, "pipeline.apply", jsonBody(t, map[string]any{
			"name": name,
			"yaml": []byte(yamlBody),
		}), "apply-"+project+"-"+name, map[string]string{"project": project, "pipeline": name})
		if err != nil {
			t.Fatalf("pipeline.apply(%s): %v", name, err)
		}
		var out api.PipelineApplyResult
		if err := json.Unmarshal(res.Body, &out); err != nil {
			t.Fatalf("decode pipeline.apply: %v", err)
		}
		return out.PipelineID
	}
	reviewPipe := applyPipeline(testToken, testProject, "review-stage", reviewStageYAML)
	integratePipe := applyPipeline(testToken, testProject, "integrate-stage", integrateStageYAML)
	mergePipe := applyPipeline(testToken, testProject, "merge-stage", mergeServiceStageYAML)
	integratePipeP2 := applyPipeline("token-admin-p2", "project-2", "integrate-stage", integrateStageYAML)

	// Start Task A with review-stage.
	createTaskA, err := h.invokeKey(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Task A",
		"pipeline_id": reviewPipe,
		"inputs": map[string]string{
			"repository": "resource-1",
			"target_ref": "refs/heads/main",
			"base":       "base-aaa",
			"candidate":  "cand-bbb",
		},
	}), "create-task-a", map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create_and_start(A): %v", err)
	}
	var taskA api.TaskCreateAndStartResult
	if err := json.Unmarshal(createTaskA.Body, &taskA); err != nil {
		t.Fatalf("decode task.create_and_start(A): %v", err)
	}
	taskAExec1ID := taskA.ExecutionID

	// Activate review request on Execution 1.
	reviewExec := loadExecForDecisionTest(t, h, scope, id.ExecutionID(taskAExec1ID))
	reviewAction := reviewExec.Actions[0]
	activateRequestForDecisionTest(t, h, scope, reviewExec.ID, reviewAction.ID)

	// Reviewer resolves review action with pass -> gets immutable Decision ID!
	resolveBody := jsonBody(t, map[string]any{
		"execution_id":            taskAExec1ID,
		"action_id":               string(reviewAction.ID),
		"expected_action_version": 2,
		"decision":                "pass",
		"outputs":                 map[string]string{"approved": "yes"},
	})
	resolveRes, err := invokeKey(t, h, testToken, "execution.request.resolve", "resolve-review-1", resolveBody, nil, map[string]string{"execution": taskAExec1ID, "action": string(reviewAction.ID)})
	if err != nil {
		t.Fatalf("execution.request.resolve(pass): %v", err)
	}
	var resolved api.ExecutionRequestResolveResult
	if err := json.Unmarshal(resolveRes.Body, &resolved); err != nil {
		t.Fatalf("decode resolve result: %v", err)
	}
	if !strings.HasPrefix(resolved.DecisionID, "dec_") {
		t.Fatalf("resolve decision_id = %q; want dec_... prefix", resolved.DecisionID)
	}

	// Idempotent retry returns the exact same decision_id.
	replayRes, err := invokeKey(t, h, testToken, "execution.request.resolve", "resolve-review-1", resolveBody, nil, map[string]string{"execution": taskAExec1ID, "action": string(reviewAction.ID)})
	if err != nil {
		t.Fatalf("execution.request.resolve(replay): %v", err)
	}
	var replayed api.ExecutionRequestResolveResult
	if err := json.Unmarshal(replayRes.Body, &replayed); err != nil {
		t.Fatalf("decode replay result: %v", err)
	}
	if replayed.DecisionID != resolved.DecisionID {
		t.Fatalf("replay decision_id = %q; want %q", replayed.DecisionID, resolved.DecisionID)
	}

	// execution.inspect exposes decision_id on the resolved action.
	inspectRes, err := h.invoke(t, testToken, "execution.inspect", jsonBody(t, map[string]any{"execution_id": taskAExec1ID}), map[string]string{"execution": taskAExec1ID})
	if err != nil {
		t.Fatalf("execution.inspect: %v", err)
	}
	var inspected api.ExecutionInspectResult
	if err := json.Unmarshal(inspectRes.Body, &inspected); err != nil {
		t.Fatalf("decode inspect: %v", err)
	}
	if len(inspected.Actions) != 1 || inspected.Actions[0].DecisionID != resolved.DecisionID {
		t.Fatalf("inspected action decision_id = %q; want %q", inspected.Actions[0].DecisionID, resolved.DecisionID)
	}

	// request.resolved event carries decision_id in its data payload.
	evRes, err := h.invoke(t, testToken, "event.read", jsonBody(t, map[string]any{"types": []string{"request.resolved"}}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("event.read: %v", err)
	}
	var evPage api.EventReadResult
	if err := json.Unmarshal(evRes.Body, &evPage); err != nil {
		t.Fatalf("decode event.read: %v", err)
	}
	foundEventDecision := false
	for _, ev := range evPage.Events {
		if ev.Type == "request.resolved" && strings.Contains(string(ev.Data), resolved.DecisionID) {
			foundEventDecision = true
		}
	}
	if !foundEventDecision {
		t.Fatalf("request.resolved event did not include decision_id %q; events=%+v", resolved.DecisionID, evPage.Events)
	}
	markCleanupCompleteForDecisionTest(t, h, scope, reviewExec.ID)

	// Start Execution 2 (integrate-stage) on the SAME Task A.
	vA := taskA.LifecycleVersion
	startExec2Res, err := invokeKey(t, h, testToken, "task.start", "start-integrate-task-a", jsonBody(t, map[string]any{
		"task_id":     taskA.TaskID,
		"pipeline_id": integratePipe,
		"inputs": map[string]string{
			"repository":  "resource-1",
			"decision_id": resolved.DecisionID,
		},
	}), &vA, map[string]string{"task": taskA.TaskID})
	if err != nil {
		t.Fatalf("task.start(integrate on task A): %v", err)
	}
	var startExec2 api.TaskStartResult
	if err := json.Unmarshal(startExec2Res.Body, &startExec2); err != nil {
		t.Fatalf("decode task.start: %v", err)
	}
	exec2 := loadExecForDecisionTest(t, h, scope, id.ExecutionID(startExec2.ExecutionID))
	activateContainerForDecisionTest(t, h, scope, exec2.ID, exec2.Actions[0].ID)

	// Start Task B (foreign task) with integrate-stage.
	createTaskB, err := h.invokeKey(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Task B",
		"pipeline_id": integratePipe,
		"inputs": map[string]string{
			"repository":  "resource-1",
			"decision_id": resolved.DecisionID,
		},
	}), "create-task-b", map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create_and_start(B): %v", err)
	}
	var taskB api.TaskCreateAndStartResult
	if err := json.Unmarshal(createTaskB.Body, &taskB); err != nil {
		t.Fatalf("decode task.create_and_start(B): %v", err)
	}
	execB := loadExecForDecisionTest(t, h, scope, id.ExecutionID(taskB.ExecutionID))
	activateContainerForDecisionTest(t, h, scope, execB.ID, execB.Actions[0].ID)

	// Start a task-free execution in project 1.
	pv1 := uint64(1)
	startFreeRes, err := invokeKey(t, h, testToken, "pipeline.start", "start-task-free-p1", jsonBody(t, map[string]any{
		"pipeline_id": integratePipe,
		"inputs": map[string]string{
			"repository":  "resource-1",
			"decision_id": resolved.DecisionID,
		},
	}), &pv1, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("pipeline.start(task-free): %v", err)
	}
	var startFree api.PipelineStartResult
	if err := json.Unmarshal(startFreeRes.Body, &startFree); err != nil {
		t.Fatalf("decode pipeline.start: %v", err)
	}
	execFree := loadExecForDecisionTest(t, h, scope, id.ExecutionID(startFree.ExecutionID))
	activateContainerForDecisionTest(t, h, scope, execFree.ID, execFree.Actions[0].ID)

	// Start an execution in project-2.
	startP2Res, err := invokeKey(t, h, "token-admin-p2", "pipeline.start", "start-task-free-p2", jsonBody(t, map[string]any{
		"pipeline_id": integratePipeP2,
		"inputs": map[string]string{
			"repository":  "resource-2",
			"decision_id": resolved.DecisionID,
		},
	}), &pv1, map[string]string{"project": "project-2"})
	if err != nil {
		t.Fatalf("pipeline.start(p2): %v", err)
	}
	var startP2 api.PipelineStartResult
	if err := json.Unmarshal(startP2Res.Body, &startP2); err != nil {
		t.Fatalf("decode pipeline.start(p2): %v", err)
	}
	scopeP2 := tenancy.Scope{TenantID: testTenant, ProjectID: "project-2"}
	execP2 := loadExecForDecisionTest(t, h, scopeP2, id.ExecutionID(startP2.ExecutionID))
	activateContainerForDecisionTest(t, h, scopeP2, execP2.ID, execP2.Actions[0].ID)

	// Wire verifier with a mutable clock so we can deterministically test expiry.
	verifyClock := &mutableClock{now: fixedNow}
	verifierPrincipals := staticPrincipals{principals: map[string]identity.Principal{
		"cred-exec2": execPrincipalForDecisionTest(t, h, scope, exec2.ID),
		"cred-execB": execPrincipalForDecisionTest(t, h, scope, execB.ID),
		"cred-free":  execPrincipalForDecisionTest(t, h, scope, execFree.ID),
		"cred-p2":    execPrincipalForDecisionTest(t, h, scopeP2, execP2.ID),
		"cred-human": {
			ID:      testEngineer,
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleReviewer, tenancy.RoleEngineer},
			Enabled: true,
			Version: 1,
		},
		"cred-merge-svc": {
			ID:      "merge-coordinator",
			Kind:    identity.KindService,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleEngineer},
			Enabled: true,
			Version: 1,
		},
		"cred-unrelated-svc": {
			ID:      "unrelated-svc",
			Kind:    identity.KindService,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleEngineer},
			Enabled: true,
			Version: 1,
		},
	}}
	bindingReader, err := readers.NewBindingReader(h.store)
	if err != nil {
		t.Fatalf("readers.NewBindingReader: %v", err)
	}
	realVerifier, err := verify.New(verify.Config{
		Principals: verifierPrincipals,
		Bindings:   bindingReader,
		Evidence:   mustEvidenceReader(t, h.store),
		Decisions:  mustDecisionReader(t, h.store),
		Records:    mustRecordAdapter(t, h.store),
		Clock:      verifyClock,
		NewID:      func() (verify.VerificationID, error) { return h.ids.NewVerificationID() },
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	actorActions := &recordingActorActions{actions: map[string]id.ActionID{
		"cred-exec2": exec2.Actions[0].ID,
		"cred-execB": execB.Actions[0].ID,
		"cred-free":  execFree.Actions[0].ID,
		"cred-p2":    execP2.Actions[0].ID,
	}}
	h.verifier = realVerifier
	h.actorActions = actorActions
	h.buildRouter(t)

	makeVerifyBody := func(resourceID, cred, role, decisionID, candidate string) string {
		body := map[string]any{
			"actor_credential": cred,
			"resource_id":      resourceID,
			"operation":        "git.ref.update",
			"approval":         "role/" + role,
			"subject": map[string]any{"fields": []map[string]string{
				{"name": "operation", "kind": "string", "value": "git.ref.update"},
				{"name": "resource", "kind": "string", "value": resourceID},
				{"name": "ref", "kind": "string", "value": "refs/heads/main"},
				{"name": "expected_base", "kind": "string", "value": "base-aaa"},
				{"name": "candidate", "kind": "string", "value": candidate},
			}},
		}
		if decisionID != "" {
			body["decision_id"] = decisionID
		}
		return jsonBody(t, body)
	}

	// 1. Execution 2 on SAME Task A succeeds with decision_id and records decision_id in verifications!
	verifyRes, err := h.invoke(t, "token-service", "actor.verify", makeVerifyBody("resource-1", "cred-exec2", "reviewer", resolved.DecisionID, "cand-bbb"), nil)
	if err != nil {
		t.Fatalf("actor.verify(same task exec2): %v", err)
	}
	var vr api.VerifyResponse
	if err := json.Unmarshal(verifyRes.Body, &vr); err != nil {
		t.Fatalf("decode VerifyResponse: %v", err)
	}
	if !vr.Approval {
		t.Fatalf("expected approval=true for same-task execution 2")
	}
	verReader, err := readers.NewVerificationReader(h.store)
	if err != nil {
		t.Fatalf("NewVerificationReader: %v", err)
	}
	storedRec, err := verReader.ReadVerification(ctx, scope, verify.VerificationID(vr.VerificationID))
	if err != nil {
		t.Fatalf("ReadVerification: %v", err)
	}
	if storedRec.DecisionID != id.DecisionID(resolved.DecisionID) {
		t.Fatalf("stored verification decision_id = %q; want %q", storedRec.DecisionID, resolved.DecisionID)
	}

	// 2. Security rejection matrix on live store + DecisionReader:
	for _, tc := range []struct {
		name       string
		svcToken   string
		resourceID string
		cred       string
		role       string
		decisionID string
		candidate  string
	}{
		{"foreign task execution", "token-service", "resource-1", "cred-execB", "reviewer", resolved.DecisionID, "cand-bbb"},
		{"task-free execution", "token-service", "resource-1", "cred-free", "reviewer", resolved.DecisionID, "cand-bbb"},
		{"cross-project execution", "token-service-p2", "resource-2", "cred-p2", "reviewer", resolved.DecisionID, "cand-bbb"},
		{"wrong role", "token-service", "resource-1", "cred-exec2", "operator", resolved.DecisionID, "cand-bbb"},
		{"mismatched candidate", "token-service", "resource-1", "cred-exec2", "reviewer", resolved.DecisionID, "cand-evil"},
		{"human actor", "token-service", "resource-1", "cred-human", "reviewer", resolved.DecisionID, "cand-bbb"},
		{"unassigned merge-coordinator before request", "token-service", "resource-1", "cred-merge-svc", "reviewer", resolved.DecisionID, "cand-bbb"},
		{"unrelated service", "token-service", "resource-1", "cred-unrelated-svc", "reviewer", resolved.DecisionID, "cand-bbb"},
		{"forged nonexistent decision_id", "token-service", "resource-1", "cred-exec2", "reviewer", "dec_forged_does_not_exist", "cand-bbb"},
		{"without decision_id exec2 has no same-execution approval", "token-service", "resource-1", "cred-exec2", "reviewer", "", "cand-bbb"},
	} {
		t.Run("refuse "+tc.name, func(t *testing.T) {
			_, err := h.invoke(t, tc.svcToken, "actor.verify", makeVerifyBody(tc.resourceID, tc.cred, tc.role, tc.decisionID, tc.candidate), nil)
			if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
				t.Fatalf("actor.verify(%s) err = %v; want forbidden", tc.name, err)
			}
		})
	}

	// Complete exec2 cleanly so the task predecessor stop barrier is confirmed before starting merge-stage.
	completeContainerForDecisionTest(t, h, scope, exec2.ID)

	// 3. Assigned service/merge-coordinator on Task A succeeds while merge request is running!
	v2 := startExec2.LifecycleVersion
	startMergeRes, err := invokeKey(t, h, testToken, "task.start", "start-merge-task-a", jsonBody(t, map[string]any{
		"task_id":     taskA.TaskID,
		"pipeline_id": mergePipe,
		"inputs": map[string]string{
			"repository":  "resource-1",
			"target_ref":  "refs/heads/main",
			"base":        "base-aaa",
			"candidate":   "cand-bbb",
			"decision_id": resolved.DecisionID,
		},
	}), &v2, map[string]string{"task": taskA.TaskID})
	if err != nil {
		t.Fatalf("task.start(merge on task A): %v", err)
	}
	var startMerge api.TaskStartResult
	if err := json.Unmarshal(startMergeRes.Body, &startMerge); err != nil {
		t.Fatalf("decode task.start(merge): %v", err)
	}
	mergeExec := loadExecForDecisionTest(t, h, scope, id.ExecutionID(startMerge.ExecutionID))
	activateRequestForDecisionTest(t, h, scope, mergeExec.ID, mergeExec.Actions[0].ID)

	// Assigned merge-coordinator now succeeds!
	svcVerifyRes, err := h.invoke(t, "token-service", "actor.verify", makeVerifyBody("resource-1", "cred-merge-svc", "reviewer", resolved.DecisionID, "cand-bbb"), nil)
	if err != nil {
		t.Fatalf("actor.verify(assigned merge-coordinator): %v", err)
	}
	var svcVR api.VerifyResponse
	if err := json.Unmarshal(svcVerifyRes.Body, &svcVR); err != nil {
		t.Fatalf("decode svc VerifyResponse: %v", err)
	}
	if !svcVR.Approval {
		t.Fatalf("expected approval=true for assigned merge-coordinator")
	}

	// Unrelated service STILL refused even while merge-coordinator is assigned!
	if _, err := h.invoke(t, "token-service", "actor.verify", makeVerifyBody("resource-1", "cred-unrelated-svc", "reviewer", resolved.DecisionID, "cand-bbb"), nil); err == nil {
		t.Fatal("unrelated service unexpectedly accepted while merge-coordinator is assigned")
	}

	// Resolving the service/merge-coordinator request creates NO approval decision,
	// AND once resolved, merge-coordinator can no longer verify with decision_id!
	h.principals.principals["token-merge-svc"] = identity.Principal{
		ID:      "merge-coordinator",
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	mergeResolveRes, err := invokeKey(t, h, "token-merge-svc", "execution.request.resolve", "resolve-merge-1", jsonBody(t, map[string]any{
		"execution_id":            startMerge.ExecutionID,
		"action_id":               string(mergeExec.Actions[0].ID),
		"expected_action_version": 2,
		"decision":                "pass",
		"outputs":                 map[string]string{"output": "cand-bbb"},
	}), nil, map[string]string{"execution": startMerge.ExecutionID, "action": string(mergeExec.Actions[0].ID)})
	if err != nil {
		t.Fatalf("execution.request.resolve(merge-coordinator): %v", err)
	}
	var mergeResolved api.ExecutionRequestResolveResult
	if err := json.Unmarshal(mergeResolveRes.Body, &mergeResolved); err != nil {
		t.Fatalf("decode merge resolve: %v", err)
	}
	if mergeResolved.DecisionID != "" {
		t.Fatalf("service request resolution unexpectedly returned decision_id=%q", mergeResolved.DecisionID)
	}
	if _, err := h.invoke(t, "token-service", "actor.verify", makeVerifyBody("resource-1", "cred-merge-svc", "reviewer", resolved.DecisionID, "cand-bbb"), nil); err == nil {
		t.Fatal("merge-coordinator unexpectedly verified after its assigned merge request completed")
	}
	markCleanupCompleteForDecisionTest(t, h, scope, mergeExec.ID)

	// 4. Expiry: advancing injected clock past 72h refuses verification!
	v3 := startMerge.LifecycleVersion
	startExec4Res, err := invokeKey(t, h, testToken, "task.start", "start-exec4-task-a", jsonBody(t, map[string]any{
		"task_id":     taskA.TaskID,
		"pipeline_id": integratePipe,
		"inputs": map[string]string{
			"repository":  "resource-1",
			"decision_id": resolved.DecisionID,
		},
	}), &v3, map[string]string{"task": taskA.TaskID})
	if err != nil {
		t.Fatalf("task.start(exec4): %v", err)
	}
	var startExec4 api.TaskStartResult
	if err := json.Unmarshal(startExec4Res.Body, &startExec4); err != nil {
		t.Fatalf("decode task.start(exec4): %v", err)
	}
	exec4 := loadExecForDecisionTest(t, h, scope, id.ExecutionID(startExec4.ExecutionID))
	activateContainerForDecisionTest(t, h, scope, exec4.ID, exec4.Actions[0].ID)
	verifierPrincipals.principals["cred-exec4"] = execPrincipalForDecisionTest(t, h, scope, exec4.ID)
	actorActions.actions["cred-exec4"] = exec4.Actions[0].ID

	verifyClock.now = fixedNow.Add(request.DefaultDecisionTTL)
	if _, err := h.invoke(t, "token-service", "actor.verify", makeVerifyBody("resource-1", "cred-exec4", "reviewer", resolved.DecisionID, "cand-bbb"), nil); err == nil {
		t.Fatal("expired decision_id was unexpectedly accepted at exact expires_at")
	}
	verifyClock.now = fixedNow.Add(time.Minute)
	if _, err := h.invoke(t, "token-service", "actor.verify", makeVerifyBody("resource-1", "cred-exec4", "reviewer", resolved.DecisionID, "cand-bbb"), nil); err != nil {
		t.Fatalf("unexpired decision_id refused before task.cancel: %v", err)
	}

	// 5. task.cancel atomically revokes active approval decisions on Task A!
	v4 := startExec4.LifecycleVersion
	if _, err := invokeKey(t, h, testToken, "task.cancel", "cancel-task-a", jsonBody(t, map[string]any{
		"task_id": taskA.TaskID,
	}), &v4, map[string]string{"task": taskA.TaskID}); err != nil {
		t.Fatalf("task.cancel(A): %v", err)
	}
	decRow, err := h.store.Read(ctx, scope, store.ApprovalDecisionsTable, resolved.DecisionID)
	if err != nil {
		t.Fatalf("read approval_decisions after task.cancel: %v", err)
	}
	if decRow.Values["revoked_at"] == nil || decRow.Values["revoked_reason"] != "task_cancelled" {
		t.Fatalf("approval_decisions after task.cancel = revoked_at:%v reason:%v; want non-nil & task_cancelled", decRow.Values["revoked_at"], decRow.Values["revoked_reason"])
	}
	if _, err := h.invoke(t, "token-service", "actor.verify", makeVerifyBody("resource-1", "cred-exec4", "reviewer", resolved.DecisionID, "cand-bbb"), nil); err == nil {
		t.Fatal("cancelled task's revoked decision_id was unexpectedly accepted")
	}
}

func loadExecForDecisionTest(t *testing.T, h *harness, scope tenancy.Scope, execID id.ExecutionID) kernel.Execution {
	t.Helper()
	exec, err := h.execs.Load(context.Background(), scope, execID)
	if err != nil {
		t.Fatalf("execs.Load(%s): %v", execID, err)
	}
	return exec
}

func activateRequestForDecisionTest(t *testing.T, h *harness, scope tenancy.Scope, execID id.ExecutionID, actionID id.ActionID) {
	t.Helper()
	exec := loadExecForDecisionTest(t, h, scope, execID)
	_, activation, err := flow.Reduce(exec, flow.ActivateFact{
		ExecutionID: execID,
		ActionID:    actionID,
		At:          fixedNow,
	})
	if err != nil || activation == nil {
		t.Fatalf("flow.Reduce(ActivateFact): commit=%+v err=%v", activation, err)
	}
	eng := storeexecution.NewEngine(h.store.Controller())
	if _, err := eng.Commit(context.Background(), *activation); err != nil {
		t.Fatalf("engine.Commit(activate request): %v", err)
	}
}

func activateContainerForDecisionTest(t *testing.T, h *harness, scope tenancy.Scope, execID id.ExecutionID, actionID id.ActionID) {
	t.Helper()
	exec := loadExecForDecisionTest(t, h, scope, execID)
	_, act, err := flow.Reduce(exec, flow.ActivateFact{
		ExecutionID: execID,
		ActionID:    actionID,
		At:          fixedNow,
	})
	if err != nil || act == nil {
		t.Fatalf("flow.Reduce(ActivateFact container): commit=%+v err=%v", act, err)
	}
	eng := storeexecution.NewEngine(h.store.Controller())
	if _, err := eng.Commit(context.Background(), *act); err != nil {
		t.Fatalf("engine.Commit(activate container): %v", err)
	}
	exec = loadExecForDecisionTest(t, h, scope, execID)
	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Locks: []store.Lock{
			{Table: "executions", IDs: []string{string(execID)}, ExpectVersion: exec.Version},
			{Table: "actions", IDs: []string{string(actionID)}, ExpectVersion: exec.Actions[0].Version},
		},
		Writes: []store.Write{
			{
				Table:         "executions",
				ID:            string(execID),
				ExpectVersion: exec.Version,
				Values: map[string]any{
					"token_nonce":      nonce,
					"token_expires_at": fixedNow.Add(24 * time.Hour),
				},
			},
			{
				Table:         "actions",
				ID:            string(actionID),
				ExpectVersion: exec.Actions[0].Version,
				Values: map[string]any{
					"deadline": fixedNow.Add(10 * 24 * time.Hour),
				},
			},
		},
	}); err != nil {
		t.Fatalf("activateContainer token/deadline: %v", err)
	}
}

func markCleanupCompleteForDecisionTest(t *testing.T, h *harness, scope tenancy.Scope, execID id.ExecutionID) {
	t.Helper()
	exec := loadExecForDecisionTest(t, h, scope, execID)
	eng := storeexecution.NewEngine(h.store.Controller())
	if _, err := eng.Commit(context.Background(), flow.Commit{
		ExecutionID:              exec.ID,
		ExpectedExecutionVersion: exec.Version,
		NextExecutionState:       exec.State,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		CleanupUpdate:            &flow.CleanupUpdate{State: kernel.CleanupComplete},
		At:                       fixedNow.Add(45 * time.Second),
	}); err != nil {
		t.Fatalf("engine.Commit(cleanup): %v", err)
	}
}

func completeContainerForDecisionTest(t *testing.T, h *harness, scope tenancy.Scope, execID id.ExecutionID) {
	t.Helper()
	exec := loadExecForDecisionTest(t, h, scope, execID)
	_, done, err := flow.Reduce(exec, flow.ObserveFact{
		ExecutionID: exec.ID,
		ActionID:    exec.Actions[0].ID,
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("exited")),
		At:          fixedNow.Add(30 * time.Second),
	})
	if err != nil || done == nil {
		t.Fatalf("flow.Reduce(ObserveFact): commit=%+v err=%v", done, err)
	}
	eng := storeexecution.NewEngine(h.store.Controller())
	if _, err := eng.Commit(context.Background(), *done); err != nil {
		t.Fatalf("engine.Commit(complete container): %v", err)
	}
}

func execPrincipalForDecisionTest(t *testing.T, h *harness, scope tenancy.Scope, execID id.ExecutionID) identity.Principal {
	t.Helper()
	row, err := h.store.Read(context.Background(), scope, "executions", string(execID))
	if err != nil {
		t.Fatalf("read execution %s: %v", execID, err)
	}
	return identity.Principal{
		ID:          id.PrincipalID(row.Values["principal_id"].(string)),
		Kind:        identity.KindExecution,
		ExecutionID: execID,
		Scope:       scope,
		Enabled:     true,
		Version:     1,
	}
}

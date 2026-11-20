package opsapp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/log"
	shellrequest "github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
)

// TestProjectReadBoundaryAndRoleMutations exercises all acceptance criteria for
// Issue #11 through the real composed ops router, matrix authorizer, capability
// services, and store adapters:
//
//  1. Pure reviewer: project-scoped operational reads + approve/reject on
//     role/reviewer succeed; mutations and role/operator approvals stay forbidden.
//  2. Pure operator: inbox + task.read + task.start on a failed open task
//     succeed; non-failed starts, mutations, and role/reviewer approvals stay
//     forbidden.
//  3. Roleless enabled service (roles: []): project-scoped operational reads,
//     task.inbox (showing only service/<id> items), execution.request.resolve,
//     and task.reply on service/<id> succeed; mutations, human-role requests,
//     and other-service requests stay forbidden.
//  4. Disabled principals, unauthenticated callers, cross-project callers,
//     cross-tenant callers, and execution principals retain fail-closed denial.
func TestProjectReadBoundaryAndRoleMutations(t *testing.T) {
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}

	// Register a pure reviewer, pure operator, pure admin, roleless enabled
	// service, disabled roleless service, cross-project reviewer, and
	// cross-tenant reviewer.
	reviewer := identity.Principal{
		ID:      "reviewer-pure",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	}
	operator := identity.Principal{
		ID:      "operator-pure",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleOperator},
		Enabled: true,
		Version: 1,
	}
	adminOnly := identity.Principal{
		ID:      "admin-pure",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
	crossProjectReviewer := identity.Principal{
		ID:      "reviewer-project-b",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"},
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	}
	crossTenantReviewer := identity.Principal{
		ID:      "reviewer-tenant-b",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: "tenant-b", ProjectID: testProject},
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	}
	h.seedMembership(t, reviewer.Scope, "membership-reviewer-pure", reviewer.ID, 1, reviewer.Roles...)
	h.seedMembership(t, operator.Scope, "membership-operator-pure", operator.ID, 1, operator.Roles...)
	h.seedMembership(t, adminOnly.Scope, "membership-admin-pure", adminOnly.ID, 1, adminOnly.Roles...)
	h.seedMembership(t, crossProjectReviewer.Scope, "membership-reviewer-project-b", crossProjectReviewer.ID, 1, crossProjectReviewer.Roles...)
	h.seedMembership(t, crossTenantReviewer.Scope, "membership-reviewer-tenant-b", crossTenantReviewer.ID, 1, crossTenantReviewer.Roles...)

	// Create two roleless services through the real service.create API: one
	// enabled, one disabled.
	if _, err := h.invokeKey(t, testToken, "service.create", jsonBody(t, map[string]any{
		"service_id": "svc-roleless",
		"roles":      []string{},
	}), "key-svc-roleless", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create svc-roleless: %v", err)
	}
	if _, err := h.invokeKey(t, testToken, "service.create", jsonBody(t, map[string]any{
		"service_id": "svc-disabled",
		"roles":      []string{},
	}), "key-svc-disabled", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create svc-disabled: %v", err)
	}
	if _, err := h.invokeVersion(t, testToken, "service.disable", `{}`, 1, map[string]string{
		"service": "svc-disabled",
	}); err != nil {
		t.Fatalf("service.disable svc-disabled: %v", err)
	}
	if _, err := h.invokeKey(t, testToken, "service.create", jsonBody(t, map[string]any{
		"service_id": "svc-other",
		"roles":      []string{},
	}), "key-svc-other", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create svc-other: %v", err)
	}

	rolelessSvc := identity.Principal{
		ID:      "svc-roleless",
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   nil,
		Enabled: true,
		Version: 1,
	}
	disabledSvc := identity.Principal{
		ID:      "svc-disabled",
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   nil,
		Enabled: false,
		Version: 2,
	}
	disabledHuman := reviewer
	disabledHuman.Enabled = false
	otherSvc := identity.Principal{
		ID:      "svc-other",
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   nil,
		Enabled: true,
		Version: 1,
	}

	h.principals.principals["token-reviewer"] = reviewer
	h.principals.principals["token-operator"] = operator
	h.principals.principals["token-admin-only"] = adminOnly
	h.principals.principals["token-roleless-svc"] = rolelessSvc
	h.principals.principals["token-disabled-svc"] = disabledSvc
	h.principals.principals["token-disabled-human"] = disabledHuman
	h.principals.principals["token-other-svc"] = otherSvc
	h.principals.principals["token-cross-project"] = crossProjectReviewer
	h.principals.principals["token-cross-tenant"] = crossTenantReviewer
	h.buildRouter(t)

	// Seed project operational state: secret, binding, pipeline, task, and
	// executions for reviewer pass/fail, operator gate, service gate, and failed
	// task retry.
	if _, err := h.invoke(t, testToken, "secret.write", `{"value":"top-secret-plaintext"}`, map[string]string{
		"project": testProject,
		"secret":  "APP_SECRET",
	}); err != nil {
		t.Fatalf("secret.write: %v", err)
	}
	if _, err := h.invoke(t, testToken, "resource.binding.register", jsonBody(t, map[string]any{
		"binding_id":        "bind-1",
		"resource_id":       "repo-1",
		"enforcing_service": "svc-roleless",
		"trusted_origins":   []string{"https://git.example.com"},
		"operations":        []string{"git.ref.update"},
		"reportable_events": []string{"git.ref.updated"},
	}), map[string]string{"project": testProject}); err != nil {
		t.Fatalf("resource.binding.register: %v", err)
	}

	containerPipeID := applyPipeline(t, h)
	reviewerReqPipeID := applyRequestPipeline(t, h, "review-gate", "role/reviewer")
	operatorReqPipeID := applyRequestPipeline(t, h, "operator-gate", "role/operator")
	serviceReqPipeID := applyRequestPipeline(t, h, "service-gate", "service/svc-roleless")

	// Start executions and activate their running request actions.
	reviewPassExec, reviewPassAction := startAndActivateRequest(t, h, scope, reviewerReqPipeID, "role/reviewer", "start-rev-pass")
	reviewRejectExec, reviewRejectAction := startAndActivateRequest(t, h, scope, reviewerReqPipeID, "role/reviewer", "start-rev-reject")
	operatorGateExec, operatorGateAction := startAndActivateRequest(t, h, scope, operatorReqPipeID, "role/operator", "start-op-gate")
	serviceGateExec, serviceGateAction := startAndActivateRequest(t, h, scope, serviceReqPipeID, "service/svc-roleless", "start-svc-gate")

	// Create tasks: one open task with a failed current execution (for operator
	// retry), one open task with a running execution and one with a succeeded
	// execution (operator must be denied forbidden), and one waiting task
	// assigned to service/svc-roleless.
	failedTaskID, failedTaskVer := createTaskWithExecutionState(t, h, scope, containerPipeID, "Failed build", kernel.ExecutionFailed, "task-failed")
	runningTaskID, runningTaskVer := createTaskWithExecutionState(t, h, scope, containerPipeID, "Running build", kernel.ExecutionRunning, "task-running")
	succeededTaskID, succeededTaskVer := createTaskWithExecutionState(t, h, scope, containerPipeID, "Succeeded build", kernel.ExecutionSucceeded, "task-succeeded")
	waitingTaskID, waitingTaskVer, waitingQID, waitingQVer := createWaitingServiceTask(t, h, containerPipeID, "service/svc-roleless", "task-waiting")

	// Register an execution principal for fail-closed checks.
	execPrincipal := identity.Principal{
		ID:          "exec-principal-1",
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     1,
		ExecutionID: id.ExecutionID(reviewPassExec),
	}
	h.principals.principals["token-exec"] = execPrincipal
	h.facts.byID[execPrincipal.ExecutionID] = authorize.AdmittedExecution{
		Principal:   execPrincipal,
		ExecutionID: execPrincipal.ExecutionID,
		ActionID:    id.ActionID(reviewPassAction),
		Permission:  authorize.ExecOwnTaskContent,
		Deadline:    fixedNow.Add(time.Hour),
	}
	h.buildRouter(t)

	// -------------------------------------------------------------------------
	// 1. Pure reviewer: project reads + approve/reject on role/reviewer
	// -------------------------------------------------------------------------
	t.Run("pure reviewer reads, logs, approves and rejects while mutations stay forbidden", func(t *testing.T) {
		assertAllProjectReadsAllowed(t, h, "token-reviewer", failedTaskID, containerPipeID, reviewPassExec, reviewPassAction, "bind-1")

		// Inbox lists the reviewer's pending requests and never leaks failed_task
		// or service-assigned items.
		inboxRaw, err := h.invoke(t, "token-reviewer", "task.inbox", `{}`, map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("reviewer task.inbox: %v", err)
		}
		var inboxResult api.TaskInboxResult
		decodeResult(t, inboxRaw, &inboxResult)
		if len(inboxResult.Items) != 2 {
			t.Fatalf("reviewer inbox items = %d; want 2 role/reviewer requests: %+v", len(inboxResult.Items), inboxResult.Items)
		}

		// Approve (pass) role/reviewer request after inspecting execution.
		inspected, err := h.invoke(t, "token-reviewer", "execution.inspect", `{}`, map[string]string{"execution": reviewPassExec})
		if err != nil {
			t.Fatalf("reviewer execution.inspect: %v", err)
		}
		var passView api.ExecutionInspectResult
		decodeResult(t, inspected, &passView)
		if _, err := h.invokeVersion(t, "token-reviewer", "execution.request.resolve", jsonBody(t, map[string]any{
			"expected_action_version": passView.Actions[0].Version,
			"decision":                "pass",
		}), passView.Version, map[string]string{
			"execution": reviewPassExec,
			"action":    reviewPassAction,
		}); err != nil {
			t.Fatalf("reviewer approve role/reviewer: %v", err)
		}

		// Reject (fail) role/reviewer request.
		inspectedReject, err := h.invoke(t, "token-reviewer", "execution.inspect", `{}`, map[string]string{"execution": reviewRejectExec})
		if err != nil {
			t.Fatalf("reviewer execution.inspect reject: %v", err)
		}
		var rejectView api.ExecutionInspectResult
		decodeResult(t, inspectedReject, &rejectView)
		if _, err := h.invokeVersion(t, "token-reviewer", "execution.request.resolve", jsonBody(t, map[string]any{
			"expected_action_version": rejectView.Actions[0].Version,
			"decision":                "fail",
		}), rejectView.Version, map[string]string{
			"execution": reviewRejectExec,
			"action":    reviewRejectAction,
		}); err != nil {
			t.Fatalf("reviewer reject role/reviewer: %v", err)
		}

		// Forbidden mutations and wrong-role approval.
		assertForbidden(t, h, "token-reviewer", "pipeline.apply", jsonBody(t, map[string]any{
			"yaml": base64.StdEncoding.EncodeToString([]byte("actions:\n  - name: a\n    image: " + pinnedImage + "\n    commands: [echo]\n")),
		}), map[string]string{"project": testProject, "pipeline": "denied"})
		assertForbiddenVersion(t, h, "token-reviewer", "pipeline.start", `{"inputs":{}}`, 1, map[string]string{
			"project": testProject, "pipeline": containerPipeID,
		})
		assertForbidden(t, h, "token-reviewer", "task.create", jsonBody(t, map[string]any{
			"title": "denied", "description": "denied", "pipeline_id": containerPipeID,
		}), map[string]string{"project": testProject})
		assertForbidden(t, h, "token-reviewer", "secret.write", `{"value":"nope"}`, map[string]string{
			"project": testProject, "secret": "DENIED_SECRET",
		})
		assertForbidden(t, h, "token-reviewer", "membership.grant", jsonBody(t, map[string]any{
			"principal_id": "someone", "roles": []string{"viewer"},
		}), map[string]string{"tenant": testTenant})
		assertForbidden(t, h, "token-reviewer", "service.create", jsonBody(t, map[string]any{
			"service_id": "denied-svc", "roles": []string{},
		}), map[string]string{"project": testProject})
		assertForbidden(t, h, "token-reviewer", "execution.request.resolve", jsonBody(t, map[string]any{
			"expected_action_version": 2,
			"decision":                "pass",
		}), map[string]string{"execution": operatorGateExec, "action": operatorGateAction})
	})

	// -------------------------------------------------------------------------
	// 2. Pure operator: inbox + task.read + failed task.start retry
	// -------------------------------------------------------------------------
	t.Run("pure operator reads and retries failed task while non-failed start and mutations stay forbidden", func(t *testing.T) {
		assertAllProjectReadsAllowed(t, h, "token-operator", failedTaskID, containerPipeID, operatorGateExec, operatorGateAction, "bind-1")

		// Operator reads the failed task and retries it via task.start.
		readRaw, err := h.invoke(t, "token-operator", "task.read", `{}`, map[string]string{"task": failedTaskID})
		if err != nil {
			t.Fatalf("operator task.read: %v", err)
		}
		var taskView api.TaskReadResult
		decodeResult(t, readRaw, &taskView)
		if taskView.LifecycleVersion != failedTaskVer {
			t.Fatalf("task.read lifecycle_version = %d; want %d", taskView.LifecycleVersion, failedTaskVer)
		}
		if _, err := h.invokeVersion(t, "token-operator", "task.start", `{}`, taskView.LifecycleVersion, map[string]string{
			"task": failedTaskID,
		}); err != nil {
			t.Fatalf("operator task.start on failed open task: %v", err)
		}

		// Operator cannot start a non-failed open task or a waiting task.
		assertForbiddenVersionKey(t, h, "token-operator", "task.start", `{}`, runningTaskVer, "key-start-running", map[string]string{
			"task": runningTaskID,
		})
		assertForbiddenVersionKey(t, h, "token-operator", "task.start", `{}`, succeededTaskVer, "key-start-succeeded", map[string]string{
			"task": succeededTaskID,
		})
		if _, err := invokeVersionKey(t, h, "token-operator", "task.start", `{}`, waitingTaskVer, "key-start-waiting", map[string]string{
			"task": waitingTaskID,
		}); err == nil {
			t.Fatal("operator task.start on waiting task unexpectedly succeeded")
		} else if class, ok := fault.ClassOf(err); !ok || (class != fault.Forbidden && class != fault.VersionConflict) {
			t.Fatalf("operator task.start on waiting task = %v; want forbidden or version_conflict", err)
		}

		// Operator cannot mutate tasks/pipelines or answer role/reviewer requests.
		assertForbidden(t, h, "token-operator", "task.create", jsonBody(t, map[string]any{
			"title": "denied", "description": "denied", "pipeline_id": containerPipeID,
		}), map[string]string{"project": testProject})
		assertForbidden(t, h, "token-operator", "pipeline.apply", jsonBody(t, map[string]any{
			"yaml": base64.StdEncoding.EncodeToString([]byte("actions:\n  - name: a\n    image: " + pinnedImage + "\n    commands: [echo]\n")),
		}), map[string]string{"project": testProject, "pipeline": "denied-op"})

		freshReviewExec, freshReviewAction := startAndActivateRequest(t, h, scope, reviewerReqPipeID, "role/reviewer", "start-rev-op-check")
		assertForbidden(t, h, "token-operator", "execution.request.resolve", jsonBody(t, map[string]any{
			"expected_action_version": 2,
			"decision":                "pass",
		}), map[string]string{"execution": freshReviewExec, "action": freshReviewAction})
	})

	// -------------------------------------------------------------------------
	// 3. Roleless enabled service (roles: []): reads, inbox, resolve & reply
	// -------------------------------------------------------------------------
	t.Run("roleless enabled service polls inbox, reads project state, and answers assigned requests/questions", func(t *testing.T) {
		assertAllProjectReadsAllowed(t, h, "token-roleless-svc", failedTaskID, containerPipeID, serviceGateExec, serviceGateAction, "bind-1")

		// Inbox returns ONLY items explicitly assigned to service/svc-roleless.
		inboxRaw, err := h.invoke(t, "token-roleless-svc", "task.inbox", `{}`, map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("roleless service task.inbox: %v", err)
		}
		var inboxResult api.TaskInboxResult
		decodeResult(t, inboxRaw, &inboxResult)
		if len(inboxResult.Items) != 2 {
			t.Fatalf("roleless service inbox items = %d; want 2 (1 request + 1 waiting task): %+v", len(inboxResult.Items), inboxResult.Items)
		}
		for _, item := range inboxResult.Items {
			if item.Assignment != "service/svc-roleless" {
				t.Fatalf("roleless service inbox leaked foreign assignment: %+v", item)
			}
		}

		// Another roleless service sees nothing and cannot resolve svc-roleless's
		// request or reply to its question.
		otherInboxRaw, err := h.invoke(t, "token-other-svc", "task.inbox", `{}`, map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("other roleless service task.inbox: %v", err)
		}
		var otherInbox api.TaskInboxResult
		decodeResult(t, otherInboxRaw, &otherInbox)
		if len(otherInbox.Items) != 0 {
			t.Fatalf("svc-other saw items = %+v; want empty", otherInbox.Items)
		}
		assertForbidden(t, h, "token-other-svc", "execution.request.resolve", jsonBody(t, map[string]any{
			"expected_action_version": 2,
			"decision":                "pass",
		}), map[string]string{"execution": serviceGateExec, "action": serviceGateAction})
		assertForbiddenVersion(t, h, "token-other-svc", "task.reply", jsonBody(t, map[string]any{
			"question_id":               waitingQID,
			"expected_question_version": waitingQVer,
			"answer":                    "wrong service",
		}), waitingTaskVer, map[string]string{"task": waitingTaskID})

		// Exact assigned roleless service resolves its service/<id> request.
		inspected, err := h.invoke(t, "token-roleless-svc", "execution.inspect", `{}`, map[string]string{"execution": serviceGateExec})
		if err != nil {
			t.Fatalf("roleless service execution.inspect: %v", err)
		}
		var svcView api.ExecutionInspectResult
		decodeResult(t, inspected, &svcView)
		if _, err := h.invokeVersion(t, "token-roleless-svc", "execution.request.resolve", jsonBody(t, map[string]any{
			"expected_action_version": svcView.Actions[0].Version,
			"decision":                "pass",
		}), svcView.Version, map[string]string{
			"execution": serviceGateExec,
			"action":    serviceGateAction,
		}); err != nil {
			t.Fatalf("roleless service resolve own request: %v", err)
		}

		// Exact assigned roleless service replies to its waiting task question.
		if _, err := h.invokeVersion(t, "token-roleless-svc", "task.reply", jsonBody(t, map[string]any{
			"question_id":               waitingQID,
			"expected_question_version": waitingQVer,
			"answer":                    "automated response",
		}), waitingTaskVer, map[string]string{"task": waitingTaskID}); err != nil {
			t.Fatalf("roleless service task.reply: %v", err)
		}

		// Roleless service cannot answer human-role requests or mutate anything.
		assertForbidden(t, h, "token-roleless-svc", "execution.request.resolve", jsonBody(t, map[string]any{
			"expected_action_version": 2,
			"decision":                "pass",
		}), map[string]string{"execution": operatorGateExec, "action": operatorGateAction})
		assertForbidden(t, h, "token-roleless-svc", "pipeline.apply", jsonBody(t, map[string]any{
			"yaml": base64.StdEncoding.EncodeToString([]byte("actions:\n  - name: a\n    image: " + pinnedImage + "\n    commands: [echo]\n")),
		}), map[string]string{"project": testProject, "pipeline": "denied-svc"})
		assertForbidden(t, h, "token-roleless-svc", "task.create", jsonBody(t, map[string]any{
			"title": "denied", "description": "denied", "pipeline_id": containerPipeID,
		}), map[string]string{"project": testProject})
		assertForbidden(t, h, "token-roleless-svc", "secret.write", `{"value":"nope"}`, map[string]string{
			"project": testProject, "secret": "DENIED_SVC_SECRET",
		})
		assertForbidden(t, h, "token-roleless-svc", "service.create", jsonBody(t, map[string]any{
			"service_id": "denied-nested", "roles": []string{},
		}), map[string]string{"project": testProject})
		assertForbidden(t, h, "token-roleless-svc", "event.publish", jsonBody(t, map[string]any{
			"type": "observation.custom", "subject_id": "s", "data": json.RawMessage(`{}`),
		}), map[string]string{"project": testProject})
	})

	// -------------------------------------------------------------------------
	// 4. Pure admin: can read project state, still cannot approve role/operator
	// -------------------------------------------------------------------------
	t.Run("pure admin has project read visibility without implicit approval authority", func(t *testing.T) {
		assertAllProjectReadsAllowed(t, h, "token-admin-only", failedTaskID, containerPipeID, operatorGateExec, operatorGateAction, "bind-1")
		assertForbidden(t, h, "token-admin-only", "execution.request.resolve", jsonBody(t, map[string]any{
			"expected_action_version": 2,
			"decision":                "pass",
		}), map[string]string{"execution": operatorGateExec, "action": operatorGateAction})
	})

	// -------------------------------------------------------------------------
	// 5. Fail-closed boundaries: disabled, unauthenticated, cross-project,
	//    cross-tenant, and execution principals
	// -------------------------------------------------------------------------
	t.Run("disabled, unauthenticated, cross-project, cross-tenant, and execution callers fail closed", func(t *testing.T) {
		// Disabled principals are refused Forbidden.
		for _, tok := range []string{"token-disabled-human", "token-disabled-svc"} {
			assertForbidden(t, h, tok, "task.inbox", `{}`, map[string]string{"project": testProject})
			assertForbidden(t, h, tok, "task.read", `{}`, map[string]string{"task": failedTaskID})
			assertForbidden(t, h, tok, "execution.inspect", `{}`, map[string]string{"execution": operatorGateExec})
			assertForbidden(t, h, tok, "pipeline.list", `{}`, map[string]string{"project": testProject})
			assertForbidden(t, h, tok, "secret.metadata.list", `{}`, map[string]string{"project": testProject})
			assertForbidden(t, h, tok, "service.list", `{}`, map[string]string{"project": testProject})
		}

		// Cross-project caller in the same tenant fails closed on every read.
		assertDeniedScope(t, h, "token-cross-project", "task.inbox", `{}`, map[string]string{"project": testProject})
		assertDeniedScope(t, h, "token-cross-project", "task.read", `{}`, map[string]string{"task": failedTaskID})
		assertDeniedScope(t, h, "token-cross-project", "execution.inspect", `{}`, map[string]string{"execution": operatorGateExec})
		assertDeniedScope(t, h, "token-cross-project", "pipeline.list", `{}`, map[string]string{"project": testProject})
		assertDeniedScope(t, h, "token-cross-project", "secret.metadata.list", `{}`, map[string]string{"project": testProject})
		assertDeniedScope(t, h, "token-cross-project", "service.list", `{}`, map[string]string{"project": testProject})

		// Cross-tenant caller (even with an identical project name) never sees
		// tenant-a's entities: ID-targeted reads fail closed and tenant-scoped
		// lists stay empty.
		assertDeniedScope(t, h, "token-cross-tenant", "task.read", `{}`, map[string]string{"task": failedTaskID})
		assertDeniedScope(t, h, "token-cross-tenant", "execution.inspect", `{}`, map[string]string{"execution": operatorGateExec})
		crossTenantPipesRaw, err := h.invoke(t, "token-cross-tenant", "pipeline.list", `{}`, map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("cross-tenant pipeline.list: %v", err)
		}
		var crossTenantPipes api.PipelineListResult
		decodeResult(t, crossTenantPipesRaw, &crossTenantPipes)
		if len(crossTenantPipes.Pipelines) != 0 {
			t.Fatalf("cross-tenant caller saw tenant-a pipelines: %+v", crossTenantPipes.Pipelines)
		}
		crossTenantSecretsRaw, err := h.invoke(t, "token-cross-tenant", "secret.metadata.list", `{}`, map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("cross-tenant secret.metadata.list: %v", err)
		}
		var crossTenantSecrets api.SecretMetadataListResult
		decodeResult(t, crossTenantSecretsRaw, &crossTenantSecrets)
		if len(crossTenantSecrets.Secrets) != 0 {
			t.Fatalf("cross-tenant caller saw tenant-a secrets: %+v", crossTenantSecrets.Secrets)
		}

		// Unauthenticated caller (empty credential) is refused before any read.
		if _, err := h.invoke(t, "", "execution.list", `{}`, map[string]string{"project": testProject}); err == nil {
			t.Fatal("unauthenticated caller was allowed execution.list")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("unauthenticated class = %v (%v); want unauthenticated", class, err)
		}

		// Execution principal does not gain broad project read access.
		assertForbidden(t, h, "token-exec", "execution.list", `{}`, map[string]string{"project": testProject})
		assertForbidden(t, h, "token-exec", "task.inbox", `{}`, map[string]string{"project": testProject})
		assertForbidden(t, h, "token-exec", "pipeline.list", `{}`, map[string]string{"project": testProject})
		assertForbidden(t, h, "token-exec", "secret.metadata.list", `{}`, map[string]string{"project": testProject})
		assertForbidden(t, h, "token-exec", "service.list", `{}`, map[string]string{"project": testProject})
		assertForbidden(t, h, "token-exec", "resource.binding.list", `{}`, map[string]string{"project": testProject})
		assertForbidden(t, h, "token-exec", "event.read", `{}`, map[string]string{"project": testProject})
	})
}

func assertAllProjectReadsAllowed(
	t *testing.T,
	h *harness,
	token string,
	taskID, pipelineID, executionID, actionID, bindingID string,
) {
	t.Helper()
	checks := []struct {
		op     string
		body   string
		params map[string]string
	}{
		{"resource.binding.read", `{}`, map[string]string{"binding": bindingID}},
		{"resource.binding.list", `{}`, map[string]string{"project": testProject}},
		{"pipeline.read", `{}`, map[string]string{"project": testProject, "pipeline": pipelineID}},
		{"pipeline.list", `{}`, map[string]string{"project": testProject}},
		{"secret.metadata.read", `{}`, map[string]string{"project": testProject, "secret": "APP_SECRET"}},
		{"secret.metadata.list", `{}`, map[string]string{"project": testProject}},
		{"service.list", `{}`, map[string]string{"project": testProject}},
		{"task.read", `{}`, map[string]string{"task": taskID}},
		{"task.list", `{}`, map[string]string{"project": testProject}},
		{"task.history", `{}`, map[string]string{"task": taskID}},
		{"task.inbox", `{}`, map[string]string{"project": testProject}},
		{"execution.inspect", `{}`, map[string]string{"execution": executionID}},
		{"execution.list", `{}`, map[string]string{"project": testProject}},
		{"event.read", `{}`, map[string]string{"project": testProject}},
		{"event.subscribe", `{}`, map[string]string{"project": testProject}},
	}
	for _, check := range checks {
		res, err := h.invoke(t, token, check.op, check.body, check.params)
		if err != nil {
			t.Fatalf("%s as %s: %v", check.op, token, err)
		}
		if bytes.Contains(res.Body, []byte("top-secret-plaintext")) {
			t.Fatalf("%s leaked secret plaintext: %s", check.op, res.Body)
		}
	}
	// execution.logs.read reaches the retained sink (not forbidden); the harness
	// uses a no-sink stub that returns not_found when authorized.
	_, err := h.invoke(t, token, "execution.logs.read", jsonBody(t, map[string]any{
		"action_id": actionID,
		"stream":    string(log.StreamStdout),
	}), map[string]string{"execution": executionID})
	if err != nil {
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("execution.logs.read as %s = %v; want nil or not_found (authorized past boundary)", token, err)
		}
	}
}

func assertForbidden(t *testing.T, h *harness, token, op, body string, params map[string]string) {
	t.Helper()
	_, err := h.invoke(t, token, op, body, params)
	if err == nil {
		t.Fatalf("%s as %s unexpectedly succeeded; want forbidden", op, token)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("%s as %s = %v (class %v); want forbidden", op, token, err, class)
	}
}

func assertDeniedScope(t *testing.T, h *harness, token, op, body string, params map[string]string) {
	t.Helper()
	_, err := h.invoke(t, token, op, body, params)
	if err == nil {
		t.Fatalf("%s as %s unexpectedly succeeded; want forbidden or not_found", op, token)
	}
	class, ok := fault.ClassOf(err)
	if !ok || (class != fault.Forbidden && class != fault.NotFound) {
		t.Fatalf("%s as %s = %v (class %v); want forbidden or not_found", op, token, err, class)
	}
}

func assertForbiddenVersionKey(t *testing.T, h *harness, token, op, body string, version uint64, key string, params map[string]string) {
	t.Helper()
	_, err := invokeVersionKey(t, h, token, op, body, version, key, params)
	if err == nil {
		t.Fatalf("%s as %s unexpectedly succeeded; want forbidden", op, token)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("%s as %s = %v (class %v); want forbidden", op, token, err, class)
	}
}

func assertForbiddenVersion(t *testing.T, h *harness, token, op, body string, version uint64, params map[string]string) {
	t.Helper()
	_, err := h.invokeVersion(t, token, op, body, version, params)
	if err == nil {
		t.Fatalf("%s as %s unexpectedly succeeded; want forbidden", op, token)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("%s as %s = %v (class %v); want forbidden", op, token, err, class)
	}
}

func invokeVersionKey(t *testing.T, h *harness, credential, operation, input string, version uint64, key string, params map[string]string) (api.Result, error) {
	t.Helper()
	return h.router.Invoke(context.Background(), ops.Invocation{
		Operation:  authorize.OperationName(operation),
		Credential: credential,
		Request: api.Request{
			Input:           json.RawMessage(input),
			ExpectedVersion: &version,
			IdempotencyKey:  key,
		},
		Params: params,
	})
}

func applyRequestPipeline(t *testing.T, h *harness, name, assignee string) string {
	t.Helper()
	yaml := []byte("actions:\n  - name: gate\n    request: " + assignee + "\n    subject:\n      operation: review\n    timeout: 1h\n")
	applied, err := h.invokeKey(t, testToken, "pipeline.apply", jsonBody(t, map[string]any{
		"yaml": base64.StdEncoding.EncodeToString(yaml),
	}), "key-pipe-"+name, map[string]string{"project": testProject, "pipeline": name})
	if err != nil {
		t.Fatalf("pipeline.apply %s: %v", name, err)
	}
	var res api.PipelineApplyResult
	decodeResult(t, applied, &res)
	return res.PipelineID
}

func startAndActivateRequest(t *testing.T, h *harness, scope tenancy.Scope, pipelineID, assignee, key string) (string, string) {
	t.Helper()
	started, err := invokeVersionKey(t, h, testToken, "pipeline.start", `{"inputs":{}}`, 1, key, map[string]string{
		"project":  testProject,
		"pipeline": pipelineID,
	})
	if err != nil {
		t.Fatalf("pipeline.start %s: %v", pipelineID, err)
	}
	var start api.PipelineStartResult
	decodeResult(t, started, &start)

	inspected, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": start.ExecutionID})
	if err != nil {
		t.Fatalf("execution.inspect %s: %v", start.ExecutionID, err)
	}
	var view api.ExecutionInspectResult
	decodeResult(t, inspected, &view)
	actionID := view.Actions[0].ActionID

	assignment, err := shellrequest.ParseAssignment(assignee)
	if err != nil {
		t.Fatalf("ParseAssignment(%q): %v", assignee, err)
	}
	assignmentBytes, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("marshal assignment: %v", err)
	}
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"operation": {Kind: definition.SubjectLiteral, Literal: "review"},
	})
	subjectBytes, err := json.Marshal(subject)
	if err != nil {
		t.Fatalf("marshal subject: %v", err)
	}

	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{
			{
				Table:         "executions",
				ID:            start.ExecutionID,
				ExpectVersion: view.Version,
				Values: map[string]any{
					"state": string(kernel.ExecutionRunning),
				},
			},
			{
				Table:         "actions",
				ID:            actionID,
				ExpectVersion: view.Actions[0].Version,
				Values: map[string]any{
					"state":              string(kernel.ActionRunning),
					"deadline":           fixedNow.Add(time.Hour),
					"request_assignment": assignmentBytes,
					"request_subject":    subjectBytes,
				},
			},
		},
	}); err != nil {
		t.Fatalf("activate request %s/%s: %v", start.ExecutionID, actionID, err)
	}
	return start.ExecutionID, actionID
}

func createTaskWithExecutionState(
	t *testing.T,
	h *harness,
	scope tenancy.Scope,
	pipelineID, title string,
	execState kernel.ExecutionState,
	key string,
) (string, uint64) {
	t.Helper()
	created, err := h.invokeKey(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       title,
		"description": title,
		"pipeline_id": pipelineID,
		"inputs":      map[string]string{},
	}), key, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create_and_start %s: %v", title, err)
	}
	var res api.TaskCreateAndStartResult
	decodeResult(t, created, &res)

	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            res.ExecutionID,
			ExpectVersion: 1,
			Values: map[string]any{
				"state": string(execState),
			},
		}},
	}); err != nil {
		t.Fatalf("mark execution %s %s: %v", res.ExecutionID, execState, err)
	}
	return res.TaskID, res.LifecycleVersion
}

func createWaitingServiceTask(t *testing.T, h *harness, pipelineID, assignee, key string) (string, uint64, string, uint64) {
	t.Helper()
	created, err := h.invokeKey(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Service handoff",
		"description": "Ask roleless service",
		"pipeline_id": pipelineID,
		"inputs":      map[string]string{},
	}), key, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create_and_start service handoff: %v", err)
	}
	var res api.TaskCreateAndStartResult
	decodeResult(t, created, &res)

	asked, err := h.invokeVersion(t, testToken, "task.ask", jsonBody(t, map[string]any{
		"question":   "Need automated classification",
		"assignment": assignee,
	}), res.LifecycleVersion, map[string]string{"task": res.TaskID})
	if err != nil {
		t.Fatalf("task.ask %s: %v", assignee, err)
	}
	var askRes api.TaskAskResult
	decodeResult(t, asked, &askRes)
	return res.TaskID, askRes.LifecycleVersion, askRes.QuestionID, askRes.QuestionVersion
}

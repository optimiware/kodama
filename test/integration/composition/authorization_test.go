//go:build integration

package composition

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestRoleRefusalIsForbiddenOnBothEdges proves the composed edge enforces the
// declared role matrix, not only authentication and project isolation.
//
// Mutation reasoning: the development credential role snapshots deliberately
// diverge from the committed membership rows. operatorToken resolves to admin-1
// with only the operator role, while grantEngineer writes admin, engineer,
// reviewer and operator to admin-1's project membership. If the router's
// Authorize call in internal/shell/transport/ops/router.go were deleted,
// secret.write by operatorToken and the tenant-scoped membership.grant by
// tenantOperatorToken would fall through to the capability's current-membership
// read and succeed (verified in a scratch mutation run). The refusal assertions
// below therefore go red when the composed authorizer is removed; the
// admitted-role calls on the same operations prove the refusals are about roles
// rather than malformed input.
func TestRoleRefusalIsForbiddenOnBothEdges(t *testing.T) {
	harness := newHarness(t)
	runtime := newFakeRuntime(modeStayRunning)
	environment := harness.start(runtime)

	// The committed membership grants the full project role set to admin-1.
	// operatorToken is the same principal with only the operator role, so an
	// edge that skipped the role matrix would still pass the handler's
	// current-membership authority read.
	environment.grantEngineer("admin-1", projectID)

	// Seed one committed pipeline and secret so the refused mutations have
	// observable state to leave untouched.
	pipelineID := environment.applyPipeline("role-guard")
	writtenSecret := environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  "ROLE_GUARD_SECRET",
		Value:     secretValue,
	}, nil, "role-guard-secret")
	var seededSecret api.SecretWriteResult
	environment.decode(writtenSecret, &seededSecret)

	// Baselines for the no-state-change proof. Every refused call below is a
	// mutation whose only acceptable effect is none.
	inboxBefore := readRoleInbox(t, environment, projectToken)
	membershipsBefore := readRoleMemberships(t, environment, adminToken)
	pipelineBefore := readRolePipeline(t, environment, projectToken, pipelineID)
	secretBefore := readRoleSecret(t, environment, projectToken, seededSecret.SecretID)
	eventsBefore := len(readAllEventPage(t, environment).Events)

	// Every case calls an operation outside the principal's registry role set.
	// The tenant operator covers the admin-only tenant boundary without a scope
	// refusal; the service and viewer cover the non-human and read-only roles.
	refusals := []struct {
		name      string
		token     string
		principal string
		operation string
		input     any
	}{
		{
			"operator cannot create a task", operatorToken, "admin-1", "task.create",
			api.TaskCreateRequest{ProjectID: projectID, Title: "refused operator task"},
		},
		{
			"operator cannot apply a pipeline", operatorToken, "admin-1", "pipeline.apply",
			api.PipelineApplyRequest{ProjectID: projectID, Name: "refused-operator-build", YAML: []byte(smokePipeline)},
		},
		{
			"operator cannot write a secret", operatorToken, "admin-1", "secret.write",
			api.SecretWriteRequest{ProjectID: projectID, SecretID: "REFUSED_OPERATOR_SECRET", Value: secretValue},
		},
		{
			"operator cannot create a service", operatorToken, "admin-1", "service.create",
			api.ServiceCreateRequest{ProjectID: projectID, ServiceID: "refused-operator-service", Roles: []string{"operator"}},
		},
		{
			"operator cannot publish an event", operatorToken, "admin-1", "event.publish",
			api.EventPublishRequest{ProjectID: projectID, Type: "observation.custom", SubjectID: "refused-operator-event", Data: []byte(`{"note":"refused"}`)},
		},
		{
			"operator cannot register a resource binding", operatorToken, "admin-1", "resource.binding.register",
			api.ResourceBindingRegisterRequest{
				ProjectID:        projectID,
				BindingID:        "refused-operator-binding",
				ResourceID:       "refused-operator-resource",
				EnforcingService: "service-1",
				TrustedOrigins:   []string{"https://git.example.com"},
				Operations:       []string{"git.ref.update"},
				ReportableEvents: []string{"git.ref.updated"},
			},
		},
		{
			"tenant operator cannot grant a membership", tenantOperatorToken, "admin-1", "membership.grant",
			api.MembershipGrantRequest{ProjectID: projectID, PrincipalID: "refused-operator-grant", Roles: []string{"viewer"}},
		},
		{
			"service cannot create a task", serviceToken, "service-1", "task.create",
			api.TaskCreateRequest{ProjectID: projectID, Title: "refused service task"},
		},
		{
			"service cannot apply a pipeline", serviceToken, "service-1", "pipeline.apply",
			api.PipelineApplyRequest{ProjectID: projectID, Name: "refused-service-build", YAML: []byte(smokePipeline)},
		},
		{
			"service cannot write a secret", serviceToken, "service-1", "secret.write",
			api.SecretWriteRequest{ProjectID: projectID, SecretID: "REFUSED_SERVICE_SECRET", Value: secretValue},
		},
		{
			"service cannot create a service", serviceToken, "service-1", "service.create",
			api.ServiceCreateRequest{ProjectID: projectID, ServiceID: "refused-service-service", Roles: []string{"operator"}},
		},
		{
			"service cannot grant a membership", serviceToken, "service-1", "membership.grant",
			api.MembershipGrantRequest{ProjectID: projectID, PrincipalID: "refused-service-grant", Roles: []string{"viewer"}},
		},
		{
			"viewer cannot create a task", viewerToken, "viewer-1", "task.create",
			api.TaskCreateRequest{ProjectID: projectID, Title: "refused viewer task"},
		},
		{
			"viewer cannot apply a pipeline", viewerToken, "viewer-1", "pipeline.apply",
			api.PipelineApplyRequest{ProjectID: projectID, Name: "refused-viewer-build", YAML: []byte(smokePipeline)},
		},
		{
			"viewer cannot write a secret", viewerToken, "viewer-1", "secret.write",
			api.SecretWriteRequest{ProjectID: projectID, SecretID: "REFUSED_VIEWER_SECRET", Value: secretValue},
		},
		{
			"viewer cannot create a service", viewerToken, "viewer-1", "service.create",
			api.ServiceCreateRequest{ProjectID: projectID, ServiceID: "refused-viewer-service", Roles: []string{"operator"}},
		},
		{
			"viewer cannot cancel an execution", viewerToken, "viewer-1", "execution.cancel",
			api.ExecutionCancelRequest{ExecutionID: "execution-role-refusal"},
		},
		{
			"human cannot verify a resource", projectToken, "admin-1", "actor.verify",
			api.VerifyRequest{ActorCredential: projectToken, ResourceID: "repo-role-guard", Operation: "git.ref.update"},
		},
		{
			"human cannot report a resource", projectToken, "admin-1", "resource.report",
			api.ReportRequest{
				ResourceID:     "repo-role-guard",
				SourceEventID:  "event-role-guard",
				EventType:      "git.ref.updated",
				Operation:      "git.ref.update",
				VerificationID: "verification-role-guard",
			},
		},
	}
	for _, testCase := range refusals {
		t.Run(testCase.name, func(t *testing.T) {
			assertRoleRefusedOnBothEdges(t, environment, testCase.token, testCase.principal, testCase.operation, testCase.input)
		})
	}

	// No refused mutation reached a handler or committed an effect: the task
	// inbox, membership list, seeded pipeline, seeded secret and the committed
	// event log are all unchanged.
	inboxAfter := readRoleInbox(t, environment, projectToken)
	if !reflect.DeepEqual(inboxBefore, inboxAfter) {
		t.Fatalf("task inbox changed after refused mutations:\nbefore=%+v\nafter =%+v", inboxBefore, inboxAfter)
	}
	membershipsAfter := readRoleMemberships(t, environment, adminToken)
	if !reflect.DeepEqual(membershipsBefore, membershipsAfter) {
		t.Fatalf("membership list changed after refused mutations:\nbefore=%+v\nafter =%+v", membershipsBefore, membershipsAfter)
	}
	pipelineAfter := readRolePipeline(t, environment, projectToken, pipelineID)
	if !reflect.DeepEqual(pipelineBefore, pipelineAfter) {
		t.Fatalf("seeded pipeline changed after refused mutations:\nbefore=%+v\nafter =%+v", pipelineBefore, pipelineAfter)
	}
	secretAfter := readRoleSecret(t, environment, projectToken, seededSecret.SecretID)
	if !reflect.DeepEqual(secretBefore, secretAfter) {
		t.Fatalf("seeded secret metadata changed after refused mutations:\nbefore=%+v\nafter =%+v", secretBefore, secretAfter)
	}
	eventsAfter := len(readAllEventPage(t, environment).Events)
	if eventsAfter != eventsBefore {
		t.Fatalf("committed event count = %d after refused mutations; want the %d before", eventsAfter, eventsBefore)
	}

	// Admitted roles succeed on the very same operations, so each refusal above
	// is a role decision rather than an input or route failure. The engineer/
	// admin human (projectToken) is admitted to all of them.
	allowedTask := environment.mustInvoke(projectToken, "task.create", api.TaskCreateRequest{
		ProjectID: projectID,
		Title:     "allowed engineer task",
	}, nil, "allowed-task-create")
	var task api.TaskCreateResult
	environment.decode(allowedTask, &task)

	allowedTaskMCP := environment.mcpMustCall(projectToken, "task.create", api.TaskCreateRequest{
		ProjectID: projectID,
		Title:     "allowed engineer task over MCP",
	}, nil, "allowed-task-create-mcp")
	var mcpTask api.TaskCreateResult
	environment.decode(allowedTaskMCP, &mcpTask)
	if mcpTask.TaskID == "" || mcpTask.TaskID == task.TaskID {
		t.Fatalf("MCP admitted task.create = %+v; want a distinct task id", mcpTask)
	}

	environment.mustInvoke(projectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: projectID,
		Name:      "allowed-build",
		YAML:      []byte(smokePipeline),
	}, nil, "allowed-pipeline-apply")

	environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  "ALLOWED_SECRET",
		Value:     secretValue,
	}, nil, "allowed-secret-write")

	serviceCreated := environment.mustInvoke(projectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: projectID,
		ServiceID: "service-1",
		Roles:     []string{"operator"},
	}, nil, "allowed-service-create")
	var service api.ServiceCreateResult
	environment.decode(serviceCreated, &service)
	if service.ServiceID != "service-1" || service.Version == 0 {
		t.Fatalf("admitted service.create = %+v; want service-1", service)
	}

	// The admitted admin grant is also the real membership.grant that gives the
	// viewer principal a committed row for the read assertions below.
	environment.mustInvoke(adminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   projectID,
		PrincipalID: "viewer-1",
		Roles:       []string{"viewer"},
	}, nil, "allowed-membership-grant")

	// The viewer role is admitted to reads on the same project while every
	// viewer mutation above was refused.
	viewerTask := environment.mustInvoke(viewerToken, "task.read", api.TaskReadRequest{TaskID: task.TaskID}, nil, "")
	var viewerTaskView api.TaskReadResult
	environment.decode(viewerTask, &viewerTaskView)
	if viewerTaskView.TaskID != task.TaskID {
		t.Fatalf("viewer task.read = %q; want %q", viewerTaskView.TaskID, task.TaskID)
	}
	environment.mustInvoke(viewerToken, "secret.metadata.read", api.SecretMetadataReadRequest{
		ProjectID: projectID,
		SecretID:  seededSecret.SecretID,
	}, nil, "")
	environment.mustInvoke(viewerToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  projectID,
		PipelineID: pipelineID,
	}, nil, "")
	environment.mustInvoke(viewerToken, "task.inbox", api.TaskInboxRequest{ProjectID: projectID}, nil, "")
	environment.mustInvoke(viewerToken, "execution.list", api.ExecutionListRequest{ProjectID: projectID}, nil, "")

	// The service role is admitted to the service-only verification surface;
	// the human principal was refused those same operations above.
	environment.mustInvoke(projectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        projectID,
		BindingID:        "role-guard-binding",
		ResourceID:       "repo-role-guard",
		EnforcingService: "service-1",
		TrustedOrigins:   []string{"https://git.example.com"},
		Operations:       []string{"git.ref.update"},
		ReportableEvents: []string{"git.ref.updated"},
	}, nil, "role-guard-binding")
	verifyResult := environment.mustInvoke(serviceToken, "actor.verify", api.VerifyRequest{
		ActorCredential: projectToken,
		ResourceID:      "repo-role-guard",
		Operation:       "git.ref.update",
	}, nil, "")
	var verified api.VerifyResponse
	environment.decode(verifyResult, &verified)
	if verified.VerificationID == "" {
		t.Fatal("service actor.verify returned no verification id")
	}
	environment.mcpMustCall(serviceToken, "actor.verify", api.VerifyRequest{
		ActorCredential: projectToken,
		ResourceID:      "repo-role-guard",
		Operation:       "git.ref.update",
	}, nil, "")

	source := environment.mustInvoke(projectToken, "event.publish", api.EventPublishRequest{
		ProjectID: projectID,
		Type:      "observation.custom",
		SubjectID: "repo-role-guard",
		Data:      []byte(`{"ref":"refs/heads/main"}`),
	}, nil, "role-guard-source-event")
	var published api.EventPublishResult
	environment.decode(source, &published)
	report := environment.mustInvoke(serviceToken, "resource.report", api.ReportRequest{
		ResourceID:     "repo-role-guard",
		SourceEventID:  published.EventID,
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: verified.VerificationID,
	}, nil, "role-guard-report")
	var reported api.ReportResponse
	environment.decode(report, &reported)
	if reported.EventID == "" || reported.VerificationID != verified.VerificationID {
		t.Fatalf("service resource.report = %+v; want event and verification %q", reported, verified.VerificationID)
	}

	// The operator role is admitted to execution.cancel even though it was
	// refused task.create above.
	started := environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "operator cancel allowed",
		PipelineID: pipelineID,
	}, nil, "role-guard-task-start")
	var startedTask api.TaskCreateAndStartResult
	environment.decode(started, &startedTask)
	environment.eventually(10*time.Second, func() bool { return runtime.startCount() == 1 })

	cancelDeadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(cancelDeadline) {
			t.Fatal("operator execution.cancel never won the version boundary")
		}
		inspect, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: startedTask.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			t.Fatalf("execution.inspect: %+v", publicError)
		}
		var view api.ExecutionInspectResult
		environment.decode(inspect, &view)
		_, cancelError := environment.call(operatorToken, "execution.cancel", api.ExecutionCancelRequest{
			ExecutionID: startedTask.ExecutionID,
		}, &view.Version, "role-guard-cancel-"+view.ExecutionID[len(view.ExecutionID)-8:])
		switch cancelError.Code {
		case "":
		case api.CodeVersionConflict:
			time.Sleep(2 * time.Millisecond)
			continue
		default:
			t.Fatalf("operator execution.cancel refused: %+v", cancelError)
		}
		break
	}
	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: startedTask.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var view api.ExecutionInspectResult
		environment.decode(result, &view)
		return view.State == "cancelled" && view.Cleanup.State == "complete"
	})
}

// assertRoleRefusedOnBothEdges proves one role refusal is forbidden on both
// public edges with the same public code and a uniform, role-free body.
func assertRoleRefusedOnBothEdges(t *testing.T, environment *runningEnv, token, principalID, operation string, input any) {
	t.Helper()
	// Supply a valid receipt key so the only remaining refusal gate is the
	// role matrix. Without it a removed authorizer would still fail closed on
	// the missing key and the mutation proof would be inconclusive.
	key := "role-refusal-" + strings.ReplaceAll(operation, ".", "-")
	_, restError := environment.call(token, operation, input, nil, key+"-rest")
	_, mcpError, err := environment.mcpCall(token, operation, input, nil, key+"-mcp")
	if err != nil {
		t.Fatalf("%s over MCP: %v", operation, err)
	}
	if restError.Code != api.CodeForbidden || mcpError.Code != api.CodeForbidden {
		t.Fatalf("%s by %s: REST code=%q MCP code=%q; want %q on both edges",
			operation, principalID, restError.Code, mcpError.Code, api.CodeForbidden)
	}
	if restError != mcpError {
		t.Fatalf("%s by %s: REST error %+v differs from MCP error %+v", operation, principalID, restError, mcpError)
	}
	if restError.Message == "" {
		t.Fatalf("%s by %s: forbidden message is empty", operation, principalID)
	}
	// The public body must not name a role. Strip the principal identity first
	// so an identity such as viewer-1 cannot hide a leaked role token.
	remainder := strings.ReplaceAll(restError.Message, principalID, "")
	for _, role := range []string{"admin", "engineer", "reviewer", "operator", "viewer"} {
		if strings.Contains(remainder, role) {
			t.Fatalf("%s by %s: forbidden message %q leaks role %q", operation, principalID, restError.Message, role)
		}
	}
}

func readRoleInbox(t *testing.T, environment *runningEnv, token string) api.TaskInboxResult {
	t.Helper()
	result := environment.mustInvoke(token, "task.inbox", api.TaskInboxRequest{ProjectID: projectID}, nil, "")
	var inbox api.TaskInboxResult
	environment.decode(result, &inbox)
	return inbox
}

func readRoleMemberships(t *testing.T, environment *runningEnv, token string) api.MembershipListResult {
	t.Helper()
	result := environment.mustInvoke(token, "membership.list", api.MembershipListRequest{ProjectID: projectID}, nil, "")
	var memberships api.MembershipListResult
	environment.decode(result, &memberships)
	return memberships
}

func readRolePipeline(t *testing.T, environment *runningEnv, token, pipelineID string) api.PipelineReadResult {
	t.Helper()
	result := environment.mustInvoke(token, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  projectID,
		PipelineID: pipelineID,
	}, nil, "")
	var pipeline api.PipelineReadResult
	environment.decode(result, &pipeline)
	return pipeline
}

func readRoleSecret(t *testing.T, environment *runningEnv, token, secretID string) api.SecretMetadataReadResult {
	t.Helper()
	result := environment.mustInvoke(token, "secret.metadata.read", api.SecretMetadataReadRequest{
		ProjectID: projectID,
		SecretID:  secretID,
	}, nil, "")
	var secret api.SecretMetadataReadResult
	environment.decode(result, &secret)
	return secret
}

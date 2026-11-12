//go:build qualification

package production

// Q02 cross-tenant probe. Two stage tenants are exercised through the public
// API, the workload runtime and, when configured, the per-tenant Git services.
// The probe creates records in each tenant, then proves the other tenant can
// neither read nor mutate them, that the files, logs, events and credentials
// stay in their own namespace, and that no token gets an authority fallback
// across the tenant boundary.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// isolationService is one service account and its issued credential.
type isolationService struct {
	ID       string
	Version  uint64
	Token    Secret
	Stage    *resilienceStage
	Disabled bool
}

// isolationIssueService creates a viewer service in one tenant and issues a
// bounded credential. Cleanup disables the service at its current version.
func isolationIssueService(t *testing.T, stage *resilienceStage, serviceID string) *isolationService {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	createKey, err := probeIdempotencyKey("q02-isolation-service-create")
	if err != nil {
		t.Fatalf("generate the service create key: %v", err)
	}
	created, err := stage.API.Post(ctx, stage.Config.AdminToken, "service.create",
		api.ServiceCreateRequest{ProjectID: stage.Config.ProjectID, ServiceID: serviceID, Roles: []string{"viewer"}}, createKey)
	if err != nil {
		t.Fatalf("create the viewer service %q: %v", serviceID, err)
	}
	serviceResult, err := decodeStageResult[api.ServiceCreateResult](created, "service.create")
	if err != nil {
		t.Fatalf("decode service.create: %v", err)
	}
	issueKey, err := probeIdempotencyKey("q02-isolation-service-issue")
	if err != nil {
		t.Fatalf("generate the service issue key: %v", err)
	}
	issued, err := stage.API.PostVersioned(ctx, stage.Config.AdminToken, "service.credential.issue",
		api.ServiceCredentialIssueRequest{ServiceID: serviceID, TTLSeconds: 900}, issueKey, serviceResult.Version)
	if err != nil {
		t.Fatalf("issue the viewer service %q credential: %v", serviceID, err)
	}
	credential, err := decodeStageResult[api.ServiceCredentialIssueResult](issued, "service.credential.issue")
	if err != nil {
		t.Fatalf("decode service.credential.issue: %v", err)
	}
	token, err := stageCredential(credential.Token, serviceCredentialPrefix)
	if err != nil {
		t.Fatalf("the service %q credential is not a service token: %v", serviceID, err)
	}
	service := &isolationService{ID: serviceID, Version: credential.Version, Token: token, Stage: stage}
	t.Cleanup(func() {
		service.disable(t)
	})
	return service
}

// disable disables the service at its last known version. It is idempotent.
func (service *isolationService) disable(t *testing.T) {
	if service == nil || service.Stage == nil || service.Disabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key, err := probeIdempotencyKey("q02-isolation-service-disable")
	if err != nil {
		t.Errorf("generate the service disable key: %v", err)
		return
	}
	disabled, err := service.Stage.API.PostVersioned(ctx, service.Stage.Config.AdminToken, "service.disable",
		api.ServiceDisableRequest{ServiceID: service.ID}, key, service.Version)
	if err != nil {
		t.Errorf("disable the viewer service %q during cleanup: %v", service.ID, err)
		return
	}
	result, err := decodeStageResult[api.ServiceDisableResult](disabled, "service.disable")
	if err != nil {
		t.Errorf("decode service.disable for %q: %v", service.ID, err)
		return
	}
	service.Version = result.Version
	service.Disabled = true
}

// isolationGet performs one authenticated read with an explicit token.
func isolationGet(ctx context.Context, client *API, token Secret, operation string, input any) (api.Result, error) {
	method, ok := resilienceOperationMethod(operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
	}
	return resilienceInvoke(ctx, client, method, token, operation, input, nil, "", false)
}

// isolationMutate performs one authenticated mutation with an explicit token.
func isolationMutate(ctx context.Context, client *API, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	method, ok := resilienceOperationMethod(operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
	}
	return resilienceInvoke(ctx, client, method, token, operation, input, nil, idempotencyKey, false)
}

// isolationMutateVersioned performs one versioned mutation with an explicit
// token.
func isolationMutateVersioned(ctx context.Context, client *API, token Secret, operation string, input any, expectedVersion uint64, idempotencyKey string) (api.Result, error) {
	method, ok := resilienceOperationMethod(operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
	}
	return resilienceInvoke(ctx, client, method, token, operation, input, &expectedVersion, idempotencyKey, false)
}

// isolationPVC is one bounded PersistentVolumeClaim fact set.
type isolationPVC struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
}

// isolationPVCList is one bounded claim list.
type isolationPVCList struct {
	Items []isolationPVC `json:"items"`
}

// isolationReadPVCs lists one namespace's claims through the guard.
func isolationReadPVCs(ctx context.Context, kubectl *Kubectl, namespace string) ([]isolationPVC, error) {
	data, err := kubectl.Run(ctx, namespace, "get", "persistentvolumeclaims", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list isolationPVCList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("claim list JSON is unreadable")
	}
	return list.Items, nil
}

// isolationValidateClaims requires at least one workspace claim in the tenant's
// namespace and proves every claim belongs to that tenant. An empty list must
// fail: a run that never created a workspace claim would otherwise pass the
// cross-namespace disjointness check vacuously.
func isolationValidateClaims(claims []isolationPVC, tenantID, namespace, otherTenantID string) (map[string]bool, error) {
	if len(claims) == 0 {
		return nil, fmt.Errorf("namespace %q has no workspace claims", namespace)
	}
	names := make(map[string]bool, len(claims))
	for _, claim := range claims {
		if claim.Metadata.Name == "" {
			return nil, fmt.Errorf("a claim in %q has no name", namespace)
		}
		if claim.Metadata.Namespace != "" && claim.Metadata.Namespace != namespace {
			return nil, fmt.Errorf("claim %q namespace = %q, want %q", claim.Metadata.Name, claim.Metadata.Namespace, namespace)
		}
		if claim.Metadata.Labels[isolationLabelTenant] != tenantID {
			return nil, fmt.Errorf("claim %q in %q names tenant %q, want %q", claim.Metadata.Name, namespace, claim.Metadata.Labels[isolationLabelTenant], tenantID)
		}
		if otherTenantID != "" && claim.Metadata.Labels[isolationLabelTenant] == otherTenantID {
			return nil, fmt.Errorf("claim %q in %q belongs to the other tenant %q", claim.Metadata.Name, namespace, otherTenantID)
		}
		names[claim.Metadata.Name] = true
	}
	return names, nil
}

// TestStageCrossTenantIsolation is Q02 probe (j). It needs two onboarded
// tenants with separate namespaces and admin tokens. The Git sub-check is part
// of the same probe and its inputs are required by default, so an unconfigured
// Git fixture fails instead of silently turning the probe into a pass. An
// operator who deliberately runs without Git may set Q02_ALLOW_GIT_SKIP=1, which
// writes an explicit skipped-outcome evidence record next to the probe.
func TestStageCrossTenantIsolation(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config, isolationRuntimeInputs()...)
	runtime := newIsolationRuntime(t, config)
	probe := resilienceBeginProbe(t, config, "TestStageCrossTenantIsolation")
	started := time.Now().UTC()

	gitMissing := config.Missing(InputGitRepositoryA, InputGitRepositoryB)
	if !gitMissing.Empty() {
		if os.Getenv("Q02_ALLOW_GIT_SKIP") != "1" {
			t.Fatalf("Q02 Git isolation input(s) are missing: %s (set Q02_ALLOW_GIT_SKIP=1 to record the skip instead of failing)", gitMissing)
		}
		t.Logf("Git isolation sub-check skipped; recording the skip; unconfigured input(s): %s", gitMissing)
		isolationRecordSkippedEvidence(t, probe, "isolation-cross-tenant-git-skipped", started)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	nonce := resilienceNonce()

	pipelineDoc := isolationPipelineDocument(config.Image, "q02-record-"+nonce)
	pipelineA, err := resilienceApplyPipeline(ctx, runtime.A.Stage, "q02-isolation-"+nonce+"-a", pipelineDoc, "q02-isolation-pipe-a-"+nonce)
	if err != nil {
		t.Fatalf("apply tenant A's pipeline: %v", err)
	}
	pipelineB, err := resilienceApplyPipeline(ctx, runtime.B.Stage, "q02-isolation-"+nonce+"-b", pipelineDoc, "q02-isolation-pipe-b-"+nonce)
	if err != nil {
		t.Fatalf("apply tenant B's pipeline: %v", err)
	}

	taskA := isolationCreateTask(t, runtime.A.Stage, "q02 task A "+nonce)
	taskB := isolationCreateTask(t, runtime.B.Stage, "q02 task B "+nonce)
	eventA := isolationPublishEvent(t, runtime.A.Stage, "q02-event-a-"+nonce, nonce)
	eventB := isolationPublishEvent(t, runtime.B.Stage, "q02-event-b-"+nonce, nonce)

	secretA := isolationWriteSecret(t, runtime.A.Stage, "Q02_ISOLATION_"+nonce, "value-a-"+nonce)
	secretB := isolationWriteSecret(t, runtime.B.Stage, "Q02_ISOLATION_"+nonce, "value-b-"+nonce)

	serviceA := isolationIssueService(t, runtime.A.Stage, "q02-isolation-service-a-"+nonce)
	serviceB := isolationIssueService(t, runtime.B.Stage, "q02-isolation-service-b-"+nonce)

	ownAFile := "q02-cross-tenant-" + nonce + "-a"
	ownBFile := "q02-cross-tenant-" + nonce + "-b"
	logA := "q02-cross-tenant-log-a-" + nonce
	logB := "q02-cross-tenant-log-b-" + nonce
	isolatedA := "q02-cross-tenant-isolated-a-" + nonce
	isolatedB := "q02-cross-tenant-isolated-b-" + nonce
	executionA := isolationStart(t, runtime.A.Stage,
		isolationWorkspacePipeline(config.Image, ownAFile, "tenant-a", ownBFile, logA, isolatedA), "isolation-workspace-a")
	podA, err := isolationWaitPod(ctx, runtime.Kubectl, runtime.A.Namespace, isolationExecutionLabel+"="+executionA, 2*time.Minute)
	if err != nil {
		t.Fatalf("tenant A's workspace pod did not start: %v", err)
	}
	// Tenant A's container writes its marker at action start. A short settle
	// makes the write certain before tenant B starts, so tenant B's immediate
	// and post-sleep checks detect a shared workspace in the direction whose
	// writer definitely went first.
	time.Sleep(2 * time.Second)
	executionB := isolationStart(t, runtime.B.Stage,
		isolationWorkspacePipeline(config.Image, ownBFile, "tenant-b", ownAFile, logB, isolatedB), "isolation-workspace-b")
	podB, err := isolationWaitPod(ctx, runtime.Kubectl, runtime.B.Namespace, isolationExecutionLabel+"="+executionB, 2*time.Minute)
	if err != nil {
		t.Fatalf("tenant B's workspace pod did not start: %v", err)
	}
	if podA.Metadata.Labels[isolationLabelTenant] != runtime.A.ID || podB.Metadata.Labels[isolationLabelTenant] != runtime.B.ID {
		t.Fatalf("workspace pods carry the wrong tenant labels: A=%q B=%q", podA.Metadata.Labels[isolationLabelTenant], podB.Metadata.Labels[isolationLabelTenant])
	}
	credentialA, err := isolationCaptureExecutionCredential(ctx, runtime.Kubectl, runtime.A.Namespace, executionA, 60*time.Second)
	if err != nil {
		t.Fatalf("capture tenant A's execution credential: %v", err)
	}
	credentialB, err := isolationCaptureExecutionCredential(ctx, runtime.Kubectl, runtime.B.Namespace, executionB, 60*time.Second)
	if err != nil {
		t.Fatalf("capture tenant B's execution credential: %v", err)
	}
	if secrets, err := isolationExecutionSecrets(ctx, runtime.Kubectl, runtime.B.Namespace, executionA); err != nil {
		t.Fatalf("list tenant A's execution credential in tenant B's namespace: %v", err)
	} else if len(secrets) != 0 {
		t.Fatalf("tenant B's namespace contains %d of tenant A's execution credential Secrets", len(secrets))
	}
	if secrets, err := isolationExecutionSecrets(ctx, runtime.Kubectl, runtime.A.Namespace, executionA); err != nil {
		t.Fatalf("list tenant A's execution credential in its namespace: %v", err)
	} else if len(secrets) != 1 {
		t.Fatalf("tenant A's namespace contains %d of its execution credential Secrets; want exactly one", len(secrets))
	}
	inspectionA, err := resilienceInspect(ctx, runtime.A.Stage, executionA)
	if err != nil {
		t.Fatalf("inspect tenant A's execution: %v", err)
	}
	inspectionB, err := resilienceInspect(ctx, runtime.B.Stage, executionB)
	if err != nil {
		t.Fatalf("inspect tenant B's execution: %v", err)
	}
	if len(inspectionA.Actions) == 0 || len(inspectionB.Actions) == 0 {
		t.Fatalf("a workspace execution has no actions: A=%+v B=%+v", inspectionA.Actions, inspectionB.Actions)
	}

	// Files and credentials are namespaced: the other tenant's namespace sees
	// no pod or credential Secret for this execution.
	if pods, err := isolationExecutionPods(ctx, runtime.Kubectl, runtime.B.Namespace, executionA); err != nil {
		t.Fatalf("list tenant A's execution pods in tenant B's namespace: %v", err)
	} else if len(pods) != 0 {
		t.Fatalf("tenant B's namespace contains %d of tenant A's workload pods", len(pods))
	}
	if pods, err := isolationExecutionPods(ctx, runtime.Kubectl, runtime.A.Namespace, executionB); err != nil {
		t.Fatalf("list tenant B's execution pods in tenant A's namespace: %v", err)
	} else if len(pods) != 0 {
		t.Fatalf("tenant A's namespace contains %d of tenant B's workload pods", len(pods))
	}
	claimsByNamespace := make(map[string]map[string]bool, 2)
	for _, pair := range []struct {
		namespace string
		tenant    isolationTenant
		other     isolationTenant
	}{
		{runtime.A.Namespace, runtime.A, runtime.B},
		{runtime.B.Namespace, runtime.B, runtime.A},
	} {
		claims, err := isolationReadPVCs(ctx, runtime.Kubectl, pair.namespace)
		if err != nil {
			t.Fatalf("list claims in %q: %v", pair.namespace, err)
		}
		names, err := isolationValidateClaims(claims, pair.tenant.ID, pair.namespace, pair.other.ID)
		if err != nil {
			t.Fatalf("claims in tenant %q: %v", pair.tenant.ID, err)
		}
		claimsByNamespace[pair.namespace] = names
	}
	for name := range claimsByNamespace[runtime.A.Namespace] {
		if claimsByNamespace[runtime.B.Namespace][name] {
			t.Fatalf("workspace claim %q is visible in both tenant namespaces", name)
		}
	}

	// Records: foreign reads and mutations refuse uniformly as not_found.
	foreignReads := []struct {
		name      string
		token     Secret
		operation string
		input     any
	}{
		{"tenant B reading tenant A's pipeline", runtime.B.Admin, "pipeline.read", api.PipelineReadRequest{ProjectID: runtime.A.ProjectID, PipelineID: pipelineA.PipelineID}},
		{"tenant A reading tenant B's pipeline", runtime.A.Admin, "pipeline.read", api.PipelineReadRequest{ProjectID: runtime.B.ProjectID, PipelineID: pipelineB.PipelineID}},
		{"tenant B reading tenant A's task", runtime.B.Admin, "task.read", api.TaskReadRequest{TaskID: taskA}},
		{"tenant A reading tenant B's task", runtime.A.Admin, "task.read", api.TaskReadRequest{TaskID: taskB}},
		{"tenant B reading tenant A's execution", runtime.B.Admin, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionA}},
		{"tenant A reading tenant B's execution", runtime.A.Admin, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionB}},
		{"tenant B listing tenant A's executions", runtime.B.Admin, "execution.list", api.ExecutionListRequest{ProjectID: runtime.A.ProjectID}},
		{"tenant B reading tenant A's inbox", runtime.B.Admin, "task.inbox", api.TaskInboxRequest{ProjectID: runtime.A.ProjectID}},
		{"tenant B reading tenant A's events", runtime.B.Admin, "event.read", api.EventReadRequest{ProjectID: runtime.A.ProjectID}},
		{"tenant A reading tenant B's events", runtime.A.Admin, "event.read", api.EventReadRequest{ProjectID: runtime.B.ProjectID}},
		{"tenant B unsubscribing tenant A's events", runtime.B.Admin, "event.subscribe", api.EventSubscribeRequest{ProjectID: runtime.A.ProjectID}},
		{"tenant B reading tenant A's secret", runtime.B.Admin, "secret.metadata.read", api.SecretMetadataReadRequest{ProjectID: runtime.A.ProjectID, SecretID: secretA}},
		{"tenant B reading tenant A's memberships", runtime.B.Admin, "membership.list", api.MembershipListRequest{ProjectID: runtime.A.ProjectID}},
		{"tenant B reading tenant A's logs", runtime.B.Admin, "execution.logs.read", api.ExecutionLogsReadRequest{ExecutionID: executionA, ActionID: inspectionA.Actions[0].ActionID, Stream: "stdout"}},
		{"tenant A reading tenant B's logs", runtime.A.Admin, "execution.logs.read", api.ExecutionLogsReadRequest{ExecutionID: executionB, ActionID: inspectionB.Actions[0].ActionID, Stream: "stdout"}},
		{"tenant A's service token reading tenant B's executions", serviceA.Token, "execution.list", api.ExecutionListRequest{ProjectID: runtime.B.ProjectID}},
		{"tenant B's service token reading tenant A's executions", serviceB.Token, "execution.list", api.ExecutionListRequest{ProjectID: runtime.A.ProjectID}},
		{"tenant A's execution token reading tenant B's task", credentialA, "task.read", api.TaskReadRequest{TaskID: taskB}},
	}
	for _, read := range foreignReads {
		t.Run(read.name, func(t *testing.T) {
			_, err := isolationGet(ctx, runtime.API, read.token, read.operation, read.input)
			isolationExpectNotFound(t, read.name, err)
		})
	}
	// An execution credential encodes its own execution identity, so
	// execution.inspect refuses any target != actor.ExecutionID with 403
	// forbidden before touching the store (no existence oracle).
	t.Run("tenant A's execution token reading tenant B's execution", func(t *testing.T) {
		_, err := isolationGet(ctx, runtime.API, credentialA, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionB})
		if refusalErr := publicRefusalError(err, http.StatusForbidden, api.CodeForbidden); refusalErr != nil {
			t.Fatalf("tenant A's execution token reading tenant B's execution: %v", refusalErr)
		}
	})

	foreignMutations := []struct {
		name      string
		token     Secret
		operation string
		input     any
		key       string
		version   uint64
	}{
		{"tenant B updating tenant A's pipeline", runtime.B.Admin, "pipeline.update",
			api.PipelineUpdateRequest{ProjectID: runtime.A.ProjectID, PipelineID: pipelineA.PipelineID, Name: "q02-foreign-update", YAML: pipelineDoc},
			"q02-foreign-pipeline-update-" + nonce, pipelineA.Version},
		{"tenant B deleting tenant A's pipeline", runtime.B.Admin, "pipeline.delete",
			api.PipelineDeleteRequest{ProjectID: runtime.A.ProjectID, PipelineID: pipelineA.PipelineID},
			"q02-foreign-pipeline-delete-" + nonce, pipelineA.Version},
		{"tenant B commenting on tenant A's task", runtime.B.Admin, "task.comment",
			api.TaskCommentRequest{TaskID: taskA, Body: "foreign comment"},
			"q02-foreign-task-comment-" + nonce, 1},
		{"tenant B cancelling tenant A's task", runtime.B.Admin, "task.cancel",
			api.TaskCancelRequest{TaskID: taskA},
			"q02-foreign-task-cancel-" + nonce, 1},
		{"tenant B cancelling tenant A's execution", runtime.B.Admin, "execution.cancel",
			api.ExecutionCancelRequest{ExecutionID: executionA},
			"q02-foreign-execution-cancel-" + nonce, inspectionA.Version},
		{"tenant B writing tenant A's secret", runtime.B.Admin, "secret.write",
			api.SecretWriteRequest{ProjectID: runtime.A.ProjectID, SecretID: secretA, Value: "foreign"},
			"q02-foreign-secret-write-" + nonce, 0},
		{"tenant B deleting tenant A's secret", runtime.B.Admin, "secret.delete",
			api.SecretDeleteRequest{ProjectID: runtime.A.ProjectID, SecretID: secretA},
			"q02-foreign-secret-delete-" + nonce, 0},
		{"tenant B publishing into tenant A", runtime.B.Admin, "event.publish",
			api.EventPublishRequest{ProjectID: runtime.A.ProjectID, Type: "observation.custom", SubjectID: "q02-foreign-" + nonce},
			"q02-foreign-event-publish-" + nonce, 0},
		{"tenant B disabling tenant A's service", runtime.B.Admin, "service.disable",
			api.ServiceDisableRequest{ServiceID: serviceA.ID},
			"q02-foreign-service-disable-" + nonce, serviceA.Version},
		{"tenant B issuing tenant A's service credential", runtime.B.Admin, "service.credential.issue",
			api.ServiceCredentialIssueRequest{ServiceID: serviceA.ID, TTLSeconds: 900},
			"q02-foreign-service-issue-" + nonce, serviceA.Version},
		{"tenant B's execution token reporting tenant A's execution outputs", credentialB, "execution.action.outputs",
			api.ExecutionActionOutputsRequest{ExecutionID: executionA, ActionID: inspectionA.Actions[0].ActionID, Outputs: map[string]string{"foreign": "value"}},
			"q02-foreign-action-outputs-" + nonce, inspectionA.Version},
	}
	for _, mutation := range foreignMutations {
		t.Run(mutation.name, func(t *testing.T) {
			var err error
			if mutation.version > 0 {
				_, err = isolationMutateVersioned(ctx, runtime.API, mutation.token, mutation.operation, mutation.input, mutation.version, mutation.key)
			} else {
				_, err = isolationMutate(ctx, runtime.API, mutation.token, mutation.operation, mutation.input, mutation.key)
			}
			isolationExpectNotFound(t, mutation.name, err)
		})
	}
	// Humans never hold ExecOwnOutputs, so the router matrix refuses a human
	// execution.action.outputs call with 403 forbidden before any store read.
	t.Run("tenant B admin reporting tenant A's execution outputs", func(t *testing.T) {
		_, err := isolationMutateVersioned(ctx, runtime.API, runtime.B.Admin, "execution.action.outputs",
			api.ExecutionActionOutputsRequest{ExecutionID: executionA, ActionID: inspectionA.Actions[0].ActionID, Outputs: map[string]string{"foreign": "value"}},
			inspectionA.Version, "q02-foreign-admin-action-outputs-"+nonce)
		if refusalErr := publicRefusalError(err, http.StatusForbidden, api.CodeForbidden); refusalErr != nil {
			t.Fatalf("tenant B admin reporting tenant A's execution outputs: %v", refusalErr)
		}
	})

	// Positive controls: each tenant still owns its own reads and credentials.
	if _, err := isolationGet(ctx, runtime.API, runtime.A.Admin, "pipeline.read", api.PipelineReadRequest{ProjectID: runtime.A.ProjectID, PipelineID: pipelineA.PipelineID}); err != nil {
		t.Fatalf("tenant A's own pipeline read was refused: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, runtime.B.Admin, "task.read", api.TaskReadRequest{TaskID: taskB}); err != nil {
		t.Fatalf("tenant B's own task read was refused: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, runtime.A.Admin, "execution.logs.read",
		api.ExecutionLogsReadRequest{ExecutionID: executionA, ActionID: inspectionA.Actions[0].ActionID, Stream: "stdout"}); err != nil {
		t.Fatalf("tenant A's own log read was refused: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, runtime.B.Admin, "secret.metadata.read",
		api.SecretMetadataReadRequest{ProjectID: runtime.B.ProjectID, SecretID: secretB}); err != nil {
		t.Fatalf("tenant B's own secret read was refused: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, serviceA.Token, "execution.list", api.ExecutionListRequest{ProjectID: runtime.A.ProjectID}); err != nil {
		t.Fatalf("tenant A's own service credential was refused: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, credentialA, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionA}); err != nil {
		t.Fatalf("tenant A's own execution credential was refused: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, Secret(""), "execution.list", api.ExecutionListRequest{ProjectID: runtime.A.ProjectID}); err == nil {
		t.Fatal("an unauthenticated cross-tenant read was accepted")
	} else {
		var public *APIError
		if !containsAPIError(err, &public) || public.Status != http.StatusUnauthorized || public.Code != api.CodeUnauthenticated {
			t.Fatalf("an unauthenticated read = %v; want 401 unauthenticated", err)
		}
	}

	// No refused mutation changed tenant A's state.
	if _, err := isolationGet(ctx, runtime.API, runtime.A.Admin, "pipeline.read", api.PipelineReadRequest{ProjectID: runtime.A.ProjectID, PipelineID: pipelineA.PipelineID}); err != nil {
		t.Fatalf("tenant A's pipeline disappeared after foreign mutations: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, runtime.A.Admin, "task.read", api.TaskReadRequest{TaskID: taskA}); err != nil {
		t.Fatalf("tenant A's task disappeared after foreign mutations: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, serviceA.Token, "execution.list", api.ExecutionListRequest{ProjectID: runtime.A.ProjectID}); err != nil {
		t.Fatalf("tenant A's service credential was invalidated by a foreign disable: %v", err)
	}
	if _, err := isolationGet(ctx, runtime.API, runtime.A.Admin, "secret.metadata.read", api.SecretMetadataReadRequest{ProjectID: runtime.A.ProjectID, SecretID: secretA}); err != nil {
		t.Fatalf("tenant A's secret disappeared after foreign mutations: %v", err)
	}

	finalA, _, err := resilienceWaitTerminal(ctx, runtime.A.Stage, executionA, 4*time.Minute)
	if err != nil {
		t.Fatalf("tenant A's workspace execution did not settle: %v", err)
	}
	finalB, _, err := resilienceWaitTerminal(ctx, runtime.B.Stage, executionB, 4*time.Minute)
	if err != nil {
		t.Fatalf("tenant B's workspace execution did not settle: %v", err)
	}
	if finalA.State != "succeeded" || finalB.State != "succeeded" {
		t.Fatalf("workspace executions = A %q / B %q; want both succeeded (A failure %+v, B failure %+v)",
			finalA.State, finalB.State, finalA.Failure, finalB.Failure)
	}
	if _, _, err := resilienceWaitCleanupComplete(ctx, runtime.A.Stage, executionA, 2*time.Minute); err != nil {
		t.Fatalf("tenant A's workspace execution cleanup did not complete: %v", err)
	}
	if _, _, err := resilienceWaitCleanupComplete(ctx, runtime.B.Stage, executionB, 2*time.Minute); err != nil {
		t.Fatalf("tenant B's workspace execution cleanup did not complete: %v", err)
	}
	actionA, _ := resilienceAction(finalA, "workspace")
	actionB, _ := resilienceAction(finalB, "workspace")
	logsA, err := resilienceReadAllLogs(ctx, runtime.A.Stage, executionA, actionA.ActionID)
	if err != nil {
		t.Fatalf("read tenant A's logs: %v", err)
	}
	logsB, err := resilienceReadAllLogs(ctx, runtime.B.Stage, executionB, actionB.ActionID)
	if err != nil {
		t.Fatalf("read tenant B's logs: %v", err)
	}
	if !strings.Contains(string(logsA), isolatedA) || !strings.Contains(string(logsB), isolatedB) {
		t.Fatalf("the workspace isolation markers are missing from the tenant logs")
	}
	if strings.Contains(string(logsB), isolatedA) || strings.Contains(string(logsA), isolatedB) {
		t.Fatalf("a tenant log contains the other tenant's isolation marker")
	}

	// Events: each tenant sees its own committed observation and not the other.
	eventsA, err := resilienceReadAllEvents(ctx, runtime.A.Stage)
	if err != nil {
		t.Fatalf("read tenant A's events: %v", err)
	}
	if !isolationEventContains(eventsA, eventA) {
		t.Fatalf("tenant A's event read does not contain its own event %q", eventA)
	}
	if isolationEventContains(eventsA, eventB) {
		t.Fatalf("tenant A's event read contains tenant B's event %q", eventB)
	}
	eventsB, err := resilienceReadAllEvents(ctx, runtime.B.Stage)
	if err != nil {
		t.Fatalf("read tenant B's events: %v", err)
	}
	if !isolationEventContains(eventsB, eventB) {
		t.Fatalf("tenant B's event read does not contain its own event %q", eventB)
	}
	if isolationEventContains(eventsB, eventA) {
		t.Fatalf("tenant B's event read contains tenant A's event %q", eventA)
	}

	probe.Record("isolation-cross-tenant", started, time.Now().UTC(), map[string]any{
		"execution_id": executionA,
		"resource_id":  executionB,
		"event_id":     eventA,
	})

	if gitMissing.Empty() {
		isolationGitProbe(t, runtime, config)
	}
}

// isolationCreateTask creates one open task and returns its id.
func isolationCreateTask(t *testing.T, stage *resilienceStage, title string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	key, err := probeIdempotencyKey("q02-isolation-task")
	if err != nil {
		t.Fatalf("generate the task key: %v", err)
	}
	result, err := resilienceMutateResult(ctx, stage, "task.create", api.TaskCreateRequest{ProjectID: stage.Config.ProjectID, Title: title}, key)
	if err != nil {
		t.Fatalf("create task %q: %v", title, err)
	}
	created, err := decodeStageResult[api.TaskCreateResult](result, "task.create")
	if err != nil {
		t.Fatalf("decode task.create: %v", err)
	}
	return created.TaskID
}

// isolationPublishEvent publishes one custom observation and returns its id.
func isolationPublishEvent(t *testing.T, stage *resilienceStage, subject, nonce string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	key, err := probeIdempotencyKey("q02-isolation-event")
	if err != nil {
		t.Fatalf("generate the event key: %v", err)
	}
	result, err := resilienceMutateResult(ctx, stage, "event.publish", api.EventPublishRequest{
		ProjectID: stage.Config.ProjectID, Type: "observation.custom", SubjectID: subject, Data: []byte(`{"probe":"` + nonce + `"}`),
	}, key)
	if err != nil {
		t.Fatalf("publish event %q: %v", subject, err)
	}
	published, err := decodeStageResult[api.EventPublishResult](result, "event.publish")
	if err != nil {
		t.Fatalf("decode event.publish: %v", err)
	}
	return published.EventID
}

// isolationWriteSecret writes one tenant secret and returns its id.
func isolationWriteSecret(t *testing.T, stage *resilienceStage, secretID, value string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	key, err := probeIdempotencyKey("q02-isolation-secret")
	if err != nil {
		t.Fatalf("generate the secret key: %v", err)
	}
	result, err := resilienceMutateResult(ctx, stage, "secret.write", api.SecretWriteRequest{
		ProjectID: stage.Config.ProjectID, SecretID: secretID, Value: value,
	}, key)
	if err != nil {
		t.Fatalf("write secret %q: %v", secretID, err)
	}
	written, err := decodeStageResult[api.SecretWriteResult](result, "secret.write")
	if err != nil {
		t.Fatalf("decode secret.write: %v", err)
	}
	return written.SecretID
}

// isolationEventContains reports whether one event id is present.
func isolationEventContains(events []api.Event, id string) bool {
	for _, event := range events {
		if event.ID == id {
			return true
		}
	}
	return false
}

// isolationGitProbe proves the per-tenant Git services serve only their own
// tenant's credential. It opens guarded loopback port-forwards to each service
// and speaks the smart-HTTP advertisement request with basic authentication.
func isolationGitProbe(t *testing.T, runtime *isolationRuntime, config Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	serviceA := config.GitServiceA
	if serviceA == "" {
		serviceA = "kodama-git-" + runtime.A.ID
	}
	serviceB := config.GitServiceB
	if serviceB == "" {
		serviceB = "kodama-git-" + runtime.B.ID
	}
	if !dnsLabelPattern.MatchString(serviceA) || !dnsLabelPattern.MatchString(serviceB) {
		t.Fatalf("the Git service names are not kubernetes service names")
	}
	tokenA, err := isolationGitToken(ctx, runtime, runtime.A.ID)
	if err != nil {
		t.Fatalf("read tenant A's Git credential: %v", err)
	}
	tokenB, err := isolationGitToken(ctx, runtime, runtime.B.ID)
	if err != nil {
		t.Fatalf("read tenant B's Git credential: %v", err)
	}
	baseA := isolationStartGitForward(t, runtime, serviceA)
	baseB := isolationStartGitForward(t, runtime, serviceB)
	clientA := isolationGitClient(t, config, serviceA)
	clientB := isolationGitClient(t, config, serviceB)
	isolationWaitGitForward(t, clientA, baseA, config.GitRepositoryA, tokenA)
	isolationWaitGitForward(t, clientB, baseB, config.GitRepositoryB, tokenB)

	status, header, _, err := isolationGitAdvertisement(ctx, clientA, baseA, config.GitRepositoryA, tokenA)
	isolationExpectGitAllowed(t, "tenant A reading its own repository", status, header, err)
	status, header, _, err = isolationGitAdvertisement(ctx, clientB, baseB, config.GitRepositoryB, tokenB)
	isolationExpectGitAllowed(t, "tenant B reading its own repository", status, header, err)

	for _, probe := range []struct {
		name   string
		client *http.Client
		base   string
		repo   string
		token  Secret
	}{
		{"tenant A's credential on tenant B's service", clientB, baseB, config.GitRepositoryB, tokenA},
		{"tenant B's credential on tenant A's service", clientA, baseA, config.GitRepositoryA, tokenB},
		{"tenant A's credential on tenant B's repository", clientA, baseA, config.GitRepositoryB, tokenA},
		{"tenant B's credential on tenant A's repository", clientB, baseB, config.GitRepositoryA, tokenB},
		{"an unauthenticated advertisement", clientA, baseA, config.GitRepositoryA, ""},
	} {
		status, _, _, err := isolationGitAdvertisement(ctx, probe.client, probe.base, probe.repo, probe.token)
		isolationExpectGitRefused(t, probe.name, status, err)
	}
}

// isolationGitToken reads the service credential Secret the chart mounts for
// one tenant's Git service.
func isolationGitToken(ctx context.Context, runtime *isolationRuntime, tenantID string) (Secret, error) {
	name := "kodama-git-" + tenantID
	if !dnsLabelPattern.MatchString(name) {
		return "", fmt.Errorf("Git credential Secret name %q is not a kubernetes name", name)
	}
	object, err := isolationReadSecret(ctx, runtime.Kubectl, ReleaseNamespace, name)
	if err != nil {
		return "", err
	}
	return parseIsolationGitSecretToken(name, object.Data["token"])
}

// parseIsolationGitSecretToken decodes one base64-encoded service credential
// from a Git Secret, trimming at most trailing line breaks written by file
// tooling while still refusing embedded whitespace.
func parseIsolationGitSecretToken(name, encoded string) (Secret, error) {
	if encoded == "" {
		return "", fmt.Errorf("Git credential Secret %q has no token", name)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 {
		return "", fmt.Errorf("Git credential Secret %q is not base64 data", name)
	}
	value := strings.TrimRight(string(raw), "\r\n")
	token, err := stageCredential(&value, serviceCredentialPrefix)
	if err != nil {
		return "", fmt.Errorf("Git credential Secret %q is not a service token: %w", name, err)
	}
	return token, nil
}

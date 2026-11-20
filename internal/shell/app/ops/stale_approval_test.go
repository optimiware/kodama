package opsapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/identity"
	shellrequest "github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

func startAndActivateGitApprovalRequest(
	t *testing.T,
	h *harness,
	pipelineYAML string,
	key string,
	resourceID, ref, expectedBase, candidate string,
) (executionID, actionID string, actionVersion uint64) {
	t.Helper()
	started, err := h.invokeKey(t, testToken, "pipeline.start", jsonBody(t, map[string]any{
		"yaml": base64.StdEncoding.EncodeToString([]byte(pipelineYAML)),
		"inputs": map[string]string{
			"resource":      resourceID,
			"ref":           ref,
			"expected_base": expectedBase,
			"candidate":     candidate,
		},
	}), key, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("pipeline.start: %v", err)
	}
	var startResult api.PipelineStartResult
	decodeResult(t, started, &startResult)

	inspected, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": startResult.ExecutionID})
	if err != nil {
		t.Fatalf("execution.inspect: %v", err)
	}
	var view api.ExecutionInspectResult
	decodeResult(t, inspected, &view)
	if len(view.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(view.Actions))
	}
	action := view.Actions[0]

	assignment, err := shellrequest.ParseAssignment("role/reviewer")
	if err != nil {
		t.Fatalf("ParseAssignment: %v", err)
	}
	assignmentBytes, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("marshal assignment: %v", err)
	}
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"operation":     {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
		"resource":      {Kind: definition.SubjectLiteral, Literal: resourceID},
		"ref":           {Kind: definition.SubjectLiteral, Literal: ref},
		"expected_base": {Kind: definition.SubjectLiteral, Literal: expectedBase},
		"candidate":     {Kind: definition.SubjectLiteral, Literal: candidate},
	})
	subjectBytes, err := json.Marshal(subject)
	if err != nil {
		t.Fatalf("marshal subject: %v", err)
	}
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{
			{
				Table:         "executions",
				ID:            startResult.ExecutionID,
				ExpectVersion: view.Version,
				Values: map[string]any{
					"state": string(kernel.ExecutionRunning),
				},
			},
			{
				Table:         "actions",
				ID:            action.ActionID,
				ExpectVersion: action.Version,
				Values: map[string]any{
					"state":              string(kernel.ActionRunning),
					"deadline":           fixedNow.Add(time.Hour),
					"request_assignment": assignmentBytes,
					"request_subject":    subjectBytes,
				},
			},
		},
	}); err != nil {
		t.Fatalf("activate request %s/%s: %v", startResult.ExecutionID, action.ActionID, err)
	}
	return startResult.ExecutionID, action.ActionID, action.Version + 1
}

func TestStaleApprovalEndToEndWithVerifiedGitRefUpdatedReport(t *testing.T) {
	const (
		resourceID = "git-repo-1"
		serviceID  = "git-svc"
		svcToken   = "token-git-svc"
		refMain    = "refs/heads/main"
		shaBase1   = "1111111111111111111111111111111111111111"
		shaHead2   = "2222222222222222222222222222222222222222"
		shaCand1   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaCand2   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		shaCand3   = "cccccccccccccccccccccccccccccccccccccccc"
	)

	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	h.seedResource(t, scope, resourceID, 1)

	// Create the enforcing Git service and register its resource binding.
	if _, err := h.invoke(t, testToken, "service.create", jsonBody(t, map[string]any{
		"service_id": serviceID,
		"roles":      []string{"engineer"},
	}), map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create: %v", err)
	}
	h.principals.principals[svcToken] = identity.Principal{
		ID:      id.PrincipalID(serviceID),
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	if _, err := h.invoke(t, testToken, "resource.binding.register", jsonBody(t, map[string]any{
		"binding_id":        "binding-git-1",
		"resource_id":       resourceID,
		"enforcing_service": serviceID,
		"trusted_origins":   []string{"https://git.example.test"},
		"operations":        []string{"git.ref.update"},
		"reportable_events": []string{"git.ref.updated"},
	}), map[string]string{"project": testProject}); err != nil {
		t.Fatalf("resource.binding.register: %v", err)
	}

	pipelineYAML := `inputs:
  - resource
  - ref
  - expected_base
  - candidate
actions:
  - name: approve-push
    request: role/reviewer
    subject:
      operation: git.ref.update
      resource: {from: inputs.resource}
      ref: {from: inputs.ref}
      expected_base: {from: inputs.expected_base}
      candidate: {from: inputs.candidate}
`

	// Start two requests expecting shaBase1, and one expecting shaHead2.
	execStale1, actStale1, verStale1 := startAndActivateGitApprovalRequest(t, h, pipelineYAML, "start-stale-1", resourceID, refMain, shaBase1, shaCand1)
	execStale2, actStale2, verStale2 := startAndActivateGitApprovalRequest(t, h, pipelineYAML, "start-stale-2", resourceID, refMain, shaBase1, shaCand2)
	execFresh, actFresh, verFresh := startAndActivateGitApprovalRequest(t, h, pipelineYAML, "start-fresh-1", resourceID, refMain, shaHead2, shaCand3)

	// Before any verified git.ref.updated event, none of the items are stale.
	inboxBefore, err := h.invoke(t, testToken, "task.inbox", `{}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.inbox before report: %v", err)
	}
	if strings.Contains(string(inboxBefore.Body), `"stale"`) {
		t.Fatalf("inbox before git.ref.updated unexpectedly included stale field: %s", inboxBefore.Body)
	}

	// Report a real verified git.ref.updated event moving refs/heads/main to shaHead2
	// through actor.verify + resource.report.
	reportSubject := api.Subject{Fields: []api.SubjectField{
		{Name: "operation", Kind: "string", Value: "git.ref.update"},
		{Name: "resource", Kind: "string", Value: resourceID},
		{Name: "ref", Kind: "string", Value: refMain},
		{Name: "expected_base", Kind: "string", Value: shaBase1},
		{Name: "candidate", Kind: "string", Value: shaHead2},
	}}
	verified, err := h.invoke(t, svcToken, "actor.verify", jsonBody(t, api.VerifyRequest{
		ResourceID:      resourceID,
		Operation:       "git.ref.update",
		ActorCredential: "actor-engineer",
		Subject:         reportSubject,
	}), nil)
	if err != nil {
		t.Fatalf("actor.verify: %v", err)
	}
	var verifyResult api.VerifyResponse
	decodeResult(t, verified, &verifyResult)
	if verifyResult.VerificationID == "" {
		t.Fatalf("actor.verify result = %+v; want non-empty verification_id", verifyResult)
	}

	if _, err := h.invoke(t, svcToken, "resource.report", jsonBody(t, api.ReportRequest{
		ResourceID:     resourceID,
		SourceEventID:  "git-push-1",
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: verifyResult.VerificationID,
		Subject:        reportSubject,
	}), nil); err != nil {
		t.Fatalf("resource.report: %v", err)
	}

	// Now task.inbox must flag execStale1 and execStale2 as stale with
	// current_ref_commit=shaHead2, while execFresh stays non-stale.
	inboxAfter, err := h.invoke(t, testToken, "task.inbox", `{}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.inbox after report: %v", err)
	}
	var inboxResult api.TaskInboxResult
	decodeResult(t, inboxAfter, &inboxResult)
	if err := inboxResult.Validate(); err != nil {
		t.Fatalf("inbox result failed public validation: %v", err)
	}
	byExec := make(map[string]api.InboxItem, len(inboxResult.Items))
	for _, item := range inboxResult.Items {
		byExec[item.ExecutionID] = item
	}
	for _, staleExec := range []string{execStale1, execStale2} {
		item := byExec[staleExec]
		if !item.Stale || item.CurrentRefCommit != shaHead2 || !strings.Contains(item.StaleReason, shaBase1) {
			t.Fatalf("inbox[%s] = %+v; want Stale=true CurrentRefCommit=%s", staleExec, item, shaHead2)
		}
	}
	if fresh := byExec[execFresh]; fresh.Stale || fresh.CurrentRefCommit != "" || fresh.StaleReason != "" {
		t.Fatalf("inbox[%s] = %+v; want non-stale", execFresh, fresh)
	}

	// Default pass on stale request must be refused with version_conflict.
	_, err = h.invoke(t, testToken, "execution.request.resolve", jsonBody(t, api.ExecutionRequestResolveRequest{
		ExpectedActionVersion: verStale1,
		Decision:              "pass",
	}), map[string]string{"execution": execStale1, "action": actStale1})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("stale pass error = %v; want version_conflict", err)
	}
	for _, want := range []string{refMain, shaBase1, shaHead2} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("stale pass error %q missing %q", err.Error(), want)
		}
	}

	// Explicit allow_stale: true overrides the stale guard and succeeds.
	overrideRes, err := h.invoke(t, testToken, "execution.request.resolve", jsonBody(t, api.ExecutionRequestResolveRequest{
		ExpectedActionVersion: verStale1,
		Decision:              "pass",
		AllowStale:            true,
	}), map[string]string{"execution": execStale1, "action": actStale1})
	if err != nil {
		t.Fatalf("stale pass with allow_stale=true failed: %v", err)
	}
	var resolvedOverride api.ExecutionRequestResolveResult
	decodeResult(t, overrideRes, &resolvedOverride)
	if resolvedOverride.ActionVersion <= verStale1 {
		t.Fatalf("override resolved versions = %+v", resolvedOverride)
	}

	// decision: "fail" on a stale request is always allowed without allow_stale.
	if _, err := h.invoke(t, testToken, "execution.request.resolve", jsonBody(t, api.ExecutionRequestResolveRequest{
		ExpectedActionVersion: verStale2,
		Decision:              "fail",
		Note:                  "base moved; please rebase",
	}), map[string]string{"execution": execStale2, "action": actStale2}); err != nil {
		t.Fatalf("stale fail without allow_stale failed: %v", err)
	}

	// Fresh request (expected_base == shaHead2) passes normally without allow_stale.
	if _, err := h.invoke(t, testToken, "execution.request.resolve", jsonBody(t, api.ExecutionRequestResolveRequest{
		ExpectedActionVersion: verFresh,
		Decision:              "pass",
	}), map[string]string{"execution": execFresh, "action": actFresh}); err != nil {
		t.Fatalf("fresh pass failed: %v", err)
	}
}

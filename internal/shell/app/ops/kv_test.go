package opsapp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/transport/httpapi"
	"github.com/optimiware/kodema/internal/shell/transport/mcp"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
	"github.com/optimiware/kodema/internal/userland/cli"
)

type staticCredSource string

func (s staticCredSource) Credential(context.Context) (client.Credential, error) {
	return client.Credential{
		Kind:      client.KindExecution,
		Token:     string(s),
		ExpiresAt: fixedNow.Add(time.Hour),
	}, nil
}

// Verify CLI integration end-to-end via real HTTP transport and ops router.
func newRESTClient(t *testing.T, router *ops.Router, token string) (*client.HTTPTransport, func()) {
	t.Helper()
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("httpapi.NewEdge: %v", err)
	}
	server := httptest.NewServer(edge)
	transport := &client.HTTPTransport{
		BaseURL:      server.URL,
		RoundTripper: server.Client().Transport,
		Source:       staticCredSource(token),
		Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Now:          func() time.Time { return fixedNow },
	}
	return transport, server.Close
}

func seedRunningContainerExecution(
	t *testing.T,
	h *harness,
	scope tenancy.Scope,
	execID id.ExecutionID,
	actionID id.ActionID,
	principalID id.PrincipalID,
	execState kernel.ExecutionState,
	actionState kernel.ActionState,
	deadline time.Time,
) identity.Principal {
	t.Helper()
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{
			{
				Table:  "executions",
				ID:     string(execID),
				Insert: true,
				Values: map[string]any{
					"principal_id": string(principalID),
					"state":        string(execState),
					"version":      int64(1),
					"yaml_sha256":  bytes.Repeat([]byte{2}, 32),
					"snapshot":     []byte(`{}`),
					"created_at":   fixedNow,
				},
			},
			{
				Table:  "actions",
				ID:     string(actionID),
				Insert: true,
				Values: map[string]any{
					"execution_id":       string(execID),
					"index":              int64(0),
					"name":               "plan",
					"kind":               "container",
					"state":              string(actionState),
					"version":            int64(1),
					"timeout_ms":         int64(60000),
					"deadline":           deadline,
					"staged_outputs_set": false,
					"launch_state":       "issued",
				},
			},
		},
	}); err != nil {
		t.Fatalf("seed execution %s: %v", execID, err)
	}
	principal := identity.Principal{
		ID:          principalID,
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     execState == kernel.ExecutionRunning,
		Version:     1,
		ExecutionID: execID,
	}
	perm := authorize.ExecProjectKV
	if execState != kernel.ExecutionRunning || actionState != kernel.ActionRunning || !deadline.After(fixedNow) {
		perm = authorize.ExecNone
	}
	h.facts.byID[execID] = authorize.AdmittedExecution{
		Principal:   principal,
		ExecutionID: execID,
		ActionID:    actionID,
		Permission:  perm,
		Deadline:    deadline,
	}
	return principal
}

func TestKVPutGetAuthorizationIsolationAndMCPParity(t *testing.T) {
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	scopeB := tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"}

	// Seed human roles: engineer, operator, reviewer, viewer, pure admin.
	eng := identity.Principal{ID: "kv-eng", Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1}
	op := identity.Principal{ID: "kv-op", Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleOperator}, Enabled: true, Version: 1}
	rev := identity.Principal{ID: "kv-rev", Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleReviewer}, Enabled: true, Version: 1}
	viewer := identity.Principal{ID: "kv-viewer", Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleViewer}, Enabled: true, Version: 1}
	adminOnly := identity.Principal{ID: "kv-admin", Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleAdmin}, Enabled: true, Version: 1}
	engB := identity.Principal{ID: "kv-eng-b", Kind: identity.KindHuman, Scope: scopeB, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1}

	h.seedMembership(t, scope, "m-kv-eng", eng.ID, 1, eng.Roles...)
	h.seedMembership(t, scope, "m-kv-op", op.ID, 1, op.Roles...)
	h.seedMembership(t, scope, "m-kv-rev", rev.ID, 1, rev.Roles...)
	h.seedMembership(t, scope, "m-kv-viewer", viewer.ID, 1, viewer.Roles...)
	h.seedMembership(t, scope, "m-kv-admin", adminOnly.ID, 1, adminOnly.Roles...)
	h.seedMembership(t, scopeB, "m-kv-eng-b", engB.ID, 1, engB.Roles...)

	// Create roleless service account (roles: []) and disabled service account.
	if _, err := h.invokeKey(t, testToken, "service.create", `{"service_id":"svc-kv-roleless","roles":[]}`, "key-svc-kv-roleless", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create svc-kv-roleless: %v", err)
	}
	if _, err := h.invokeKey(t, testToken, "service.create", `{"service_id":"svc-kv-disabled","roles":[]}`, "key-svc-kv-disabled", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create svc-kv-disabled: %v", err)
	}
	if _, err := h.invokeVersion(t, testToken, "service.disable", `{}`, 1, map[string]string{"service": "svc-kv-disabled"}); err != nil {
		t.Fatalf("service.disable: %v", err)
	}
	svcRoleless := identity.Principal{ID: "svc-kv-roleless", Kind: identity.KindService, Scope: scope, Roles: nil, Enabled: true, Version: 1}
	svcDisabled := identity.Principal{ID: "svc-kv-disabled", Kind: identity.KindService, Scope: scope, Roles: nil, Enabled: false, Version: 2}

	// Seed active, finished, and expired executions.
	execActive := seedRunningContainerExecution(t, h, scope, "exec-kv-active", "act-kv-active", "exec-p-active", kernel.ExecutionRunning, kernel.ActionRunning, fixedNow.Add(time.Hour))
	execDone := seedRunningContainerExecution(t, h, scope, "exec-kv-done", "act-kv-done", "exec-p-done", kernel.ExecutionSucceeded, kernel.ActionSucceeded, fixedNow.Add(time.Hour))
	execExpired := seedRunningContainerExecution(t, h, scope, "exec-kv-expired", "act-kv-expired", "exec-p-expired", kernel.ExecutionRunning, kernel.ActionRunning, fixedNow.Add(-time.Second))

	h.principals.principals["tok-eng"] = eng
	h.principals.principals["tok-op"] = op
	h.principals.principals["tok-rev"] = rev
	h.principals.principals["tok-viewer"] = viewer
	h.principals.principals["tok-admin-only"] = adminOnly
	h.principals.principals["tok-eng-b"] = engB
	h.principals.principals["tok-svc-roleless"] = svcRoleless
	h.principals.principals["tok-svc-disabled"] = svcDisabled
	h.principals.principals["tok-exec-active"] = execActive
	h.principals.principals["tok-exec-done"] = execDone
	h.principals.principals["tok-exec-expired"] = execExpired
	h.buildRouter(t)

	// Allowed kv.put actors: engineer, operator, reviewer, roleless service, active execution.
	var storedKey string
	for _, tok := range []string{"tok-eng", "tok-op", "tok-rev", "tok-svc-roleless", "tok-exec-active"} {
		res, err := h.invoke(t, tok, "kv.put", jsonBody(t, map[string]any{
			"text": "written by " + tok,
		}), map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("kv.put as %s failed: %v", tok, err)
		}
		var out api.KVPutResult
		decodeResult(t, res, &out)
		if out.Existed {
			t.Fatalf("first kv.put as %s reported existed=true", tok)
		}
		if out.ContentType != "text/plain; charset=utf-8" {
			t.Fatalf("kv.put text default content_type = %q; want text/plain; charset=utf-8", out.ContentType)
		}
		storedKey = out.Key
	}

	// Idempotent re-put returns existed=true with same key.
	reput, err := h.invoke(t, "tok-eng", "kv.put", jsonBody(t, map[string]any{
		"text": "written by tok-exec-active",
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("idempotent kv.put: %v", err)
	}
	var reputOut api.KVPutResult
	decodeResult(t, reput, &reputOut)
	if !reputOut.Existed || reputOut.Key != storedKey {
		t.Fatalf("idempotent kv.put = %+v; want existed=true key=%s", reputOut, storedKey)
	}

	// Denied kv.put actors: viewer, pure admin, disabled service, finished execution, expired execution.
	for _, tok := range []string{"tok-viewer", "tok-admin-only", "tok-svc-disabled", "tok-exec-done", "tok-exec-expired"} {
		assertForbidden(t, h, tok, "kv.put", `{"text":"nope"}`, map[string]string{"project": testProject})
	}

	// Allowed kv.get actors: all human project members (incl. viewer & admin), roleless service, active execution.
	for _, tok := range []string{"tok-eng", "tok-op", "tok-rev", "tok-viewer", "tok-admin-only", "tok-svc-roleless", "tok-exec-active"} {
		res, err := h.invoke(t, tok, "kv.get", `{}`, map[string]string{"project": testProject, "key": storedKey})
		if err != nil {
			t.Fatalf("kv.get as %s failed: %v", tok, err)
		}
		var out api.KVGetResult
		decodeResult(t, res, &out)
		if out.Text == nil || *out.Text != "written by tok-exec-active" {
			t.Fatalf("kv.get as %s text = %v; want %q", tok, out.Text, "written by tok-exec-active")
		}
	}

	// Denied kv.get actors: disabled service, finished execution, expired execution.
	for _, tok := range []string{"tok-svc-disabled", "tok-exec-done", "tok-exec-expired"} {
		assertForbidden(t, h, tok, "kv.get", `{}`, map[string]string{"project": testProject, "key": storedKey})
	}

	// Cross-project isolation: project-b cannot read project-a's key, even when asking its own project.
	_, err = h.invoke(t, "tok-eng-b", "kv.get", `{}`, map[string]string{"project": "project-b", "key": storedKey})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("project-b kv.get of project-a key = %v; want not_found", err)
	}
	// Cross-project caller targeting project-a is also refused not_found.
	_, err = h.invoke(t, "tok-eng-b", "kv.get", `{}`, map[string]string{"project": testProject, "key": storedKey})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("project-b caller targeting project-a = %v; want not_found", err)
	}

	// Malformed key refused with invalid_input.
	for _, badKey := range []string{"not-a-key", "kv:sha256:UPPERCASE", "kv:sha256:short"} {
		_, err := h.invoke(t, "tok-eng", "kv.get", `{}`, map[string]string{"project": testProject, "key": badKey})
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("malformed key %q err = %v; want invalid_input", badKey, err)
		}
	}

	// MCP edge parity: kv_put and kv_get round-trip over JSON-RPC tools/call.
	mcpEdge, err := mcp.NewEdge(mcp.Config{
		Router:     h.router,
		Principals: h.principals,
		ServerInfo: api.MCPImplementation{Name: "kodema", Version: "test"},
	})
	if err != nil {
		t.Fatalf("mcp.NewEdge: %v", err)
	}
	putBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "kv_put",
			"arguments": map[string]any{
				"project_id":   testProject,
				"text":         "review findings from MCP",
				"content_type": "text/markdown; charset=utf-8",
			},
		},
	})
	rawPutResp := mcpEdge.Handle(context.Background(), "tok-rev", putBody)
	var mcpPutResp api.MCPResponse
	if err := json.Unmarshal(rawPutResp, &mcpPutResp); err != nil || mcpPutResp.Error != nil {
		t.Fatalf("MCP kv_put response = %s (err=%v)", rawPutResp, err)
	}
	mcpPutRes, err := api.DecodeMCPResult(mcpPutResp.Result)
	if err != nil {
		t.Fatalf("DecodeMCPResult(kv_put): %v", err)
	}
	var mcpPutOut api.KVPutResult
	decodeResult(t, mcpPutRes, &mcpPutOut)

	getBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "kv_get",
			"arguments": map[string]any{
				"project_id": testProject,
				"key":        mcpPutOut.Key,
			},
		},
	})
	rawGetResp := mcpEdge.Handle(context.Background(), "tok-svc-roleless", getBody)
	var mcpGetResp api.MCPResponse
	if err := json.Unmarshal(rawGetResp, &mcpGetResp); err != nil || mcpGetResp.Error != nil {
		t.Fatalf("MCP kv_get response = %s (err=%v)", rawGetResp, err)
	}
	mcpGetRes, err := api.DecodeMCPResult(mcpGetResp.Result)
	if err != nil {
		t.Fatalf("DecodeMCPResult(kv_get): %v", err)
	}
	var mcpGetOut api.KVGetResult
	decodeResult(t, mcpGetRes, &mcpGetOut)
	if mcpGetOut.Text == nil || *mcpGetOut.Text != "review findings from MCP" {
		t.Fatalf("MCP kv_get text = %v; want review findings from MCP", mcpGetOut.Text)
	}
}

// TestKVEndToEndWorkloadToDownstreamAnd16MiBPayload exercises the full PR 3
// workflow over the real HTTP REST edge + client + CLI:
//  1. Running workload runs `kodema kv put` (without --project, auto-resolved
//     via KODEMA_EXECUTION -> principal.whoami), storing a plan document.
//  2. Key is placed into task.metadata and execution action outputs.
//  3. Downstream human Reviewer & roleless Service read exact bytes via `kodema kv get`.
//  4. Full 16 MiB binary round-trip over HTTP REST succeeds; 16 MiB + 1 byte fails.
func TestKVEndToEndWorkloadToDownstreamAnd16MiBPayload(t *testing.T) {
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}

	rev := identity.Principal{ID: "e2e-rev", Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleReviewer}, Enabled: true, Version: 1}
	h.seedMembership(t, scope, "m-e2e-rev", rev.ID, 1, rev.Roles...)
	if _, err := h.invokeKey(t, testToken, "service.create", `{"service_id":"svc-e2e-opencode","roles":[]}`, "key-svc-e2e", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create: %v", err)
	}
	svc := identity.Principal{ID: "svc-e2e-opencode", Kind: identity.KindService, Scope: scope, Enabled: true, Version: 1}

	// Apply and start a real pipeline whose container action declares `outputs: [plan_key]`.
	planPipeYAML := []byte("actions:\n  - name: plan\n    image: " + pinnedImage + "\n    commands: [echo]\n    outputs: [plan_key]\n")
	appliedPlan, err := h.invokeKey(t, testToken, "pipeline.apply", jsonBody(t, map[string]any{
		"yaml": base64.StdEncoding.EncodeToString(planPipeYAML),
	}), "key-apply-kv-plan", map[string]string{"project": testProject, "pipeline": "kv-plan"})
	if err != nil {
		t.Fatalf("pipeline.apply kv-plan: %v", err)
	}
	var planPipe api.PipelineApplyResult
	decodeResult(t, appliedPlan, &planPipe)

	startedPlan, err := invokeVersionKey(t, h, testToken, "pipeline.start", `{"inputs":{}}`, 1, "key-start-kv-plan", map[string]string{
		"project":  testProject,
		"pipeline": planPipe.PipelineID,
	})
	if err != nil {
		t.Fatalf("pipeline.start kv-plan: %v", err)
	}
	var startRes api.PipelineStartResult
	decodeResult(t, startedPlan, &startRes)

	inspectedStart, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": startRes.ExecutionID})
	if err != nil {
		t.Fatalf("execution.inspect start: %v", err)
	}
	var startView api.ExecutionInspectResult
	decodeResult(t, inspectedStart, &startView)
	execRow, err := h.store.Read(context.Background(), scope, "executions", startRes.ExecutionID)
	if err != nil {
		t.Fatalf("read started execution: %v", err)
	}
	execPrincipalID, _ := execRow.Values["principal_id"].(string)
	planActionID := startView.Actions[0].ActionID
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{
			{
				Table:         "executions",
				ID:            startRes.ExecutionID,
				ExpectVersion: startView.Version,
				Values:        map[string]any{"state": string(kernel.ExecutionRunning)},
			},
			{
				Table:         "actions",
				ID:            planActionID,
				ExpectVersion: startView.Actions[0].Version,
				Values: map[string]any{
					"state":    string(kernel.ActionRunning),
					"deadline": fixedNow.Add(time.Hour),
				},
			},
		},
	}); err != nil {
		t.Fatalf("activate started execution: %v", err)
	}
	execPrincipal := identity.Principal{
		ID:          id.PrincipalID(execPrincipalID),
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     2,
		ExecutionID: id.ExecutionID(startRes.ExecutionID),
	}
	h.facts.byID[execPrincipal.ExecutionID] = authorize.AdmittedExecution{
		Principal:   execPrincipal,
		ExecutionID: execPrincipal.ExecutionID,
		ActionID:    id.ActionID(planActionID),
		Permission:  authorize.ExecProjectKV,
		Deadline:    fixedNow.Add(time.Hour),
	}

	downstreamExec := seedRunningContainerExecution(t, h, scope, "exec-e2e-2", "act-e2e-2", "exec-p-e2e-2", kernel.ExecutionRunning, kernel.ActionRunning, fixedNow.Add(time.Hour))

	h.principals.principals["tok-e2e-rev"] = rev
	h.principals.principals["tok-e2e-svc"] = svc
	h.principals.principals["tok-e2e-exec"] = execPrincipal
	h.principals.principals["tok-e2e-downstream"] = downstreamExec
	h.buildRouter(t)

	execTransport, closeExec := newRESTClient(t, h.router, "tok-e2e-exec")
	defer closeExec()
	downstreamTransport, closeDownstream := newRESTClient(t, h.router, "tok-e2e-downstream")
	defer closeDownstream()
	revTransport, closeRev := newRESTClient(t, h.router, "tok-e2e-rev")
	defer closeRev()
	svcTransport, closeSvc := newRESTClient(t, h.router, "tok-e2e-svc")
	defer closeSvc()

	// 1. Workload executes `kodema kv put -` with KODEMA_EXECUTION set and no --project.
	planContent := "# Plan for Issue #63\n\n- Implement content-addressed KV store\n- Verify RLS\n"
	var putOut, putErr bytes.Buffer
	code := cli.RunWithDeps(context.Background(), cli.Config{ServerURL: execTransport.BaseURL}, []string{"kv", "put", "-"}, cli.Ports{
		Client: execTransport,
		In:     strings.NewReader(planContent),
		Out:    &putOut,
		Err:    &putErr,
	}, cli.CommandDeps{
		Env: func(k string) string {
			if k == "KODEMA_EXECUTION" {
				return startRes.ExecutionID
			}
			return ""
		},
	})
	if code != cli.ExitSuccess {
		t.Fatalf("workload `kodema kv put -` exit=%d stderr=%q", code, putErr.String())
	}
	planKey := strings.TrimSpace(putOut.String())
	sum := sha256.Sum256([]byte(planContent))
	wantKey := "kv:sha256:" + hex.EncodeToString(sum[:])
	if planKey != wantKey {
		t.Fatalf("workload `kodema kv put -` key = %q; want %q", planKey, wantKey)
	}

	// 2. Store the key inside BOTH execution action outputs (execution.action.outputs)
	// and task.metadata (proving the exact Issue #63 handoff pattern).
	if _, err := h.invokeVersion(t, "tok-e2e-exec", "execution.action.outputs", jsonBody(t, map[string]any{
		"outputs": map[string]string{"plan_key": planKey},
	}), startView.Version+1, map[string]string{"execution": startRes.ExecutionID, "action": planActionID}); err != nil {
		t.Fatalf("execution.action.outputs with plan_key: %v", err)
	}
	actionRow, readErr := h.store.Read(context.Background(), scope, "actions", planActionID)
	if readErr != nil {
		t.Fatalf("read action row: %v", readErr)
	}
	stagedRaw, _ := actionRow.Values["staged_outputs"].([]byte)
	if !bytes.Contains(stagedRaw, []byte(planKey)) {
		t.Fatalf("action staged_outputs = %s; want plan_key=%q", stagedRaw, planKey)
	}

	pipeID := applyPipeline(t, h)
	createdTask, err := h.invokeKey(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title":       "Plan & review via KV",
		"pipeline_id": pipeID,
		"metadata":    map[string]string{"plan_key": planKey},
	}), "key-task-kv-e2e", map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create with plan_key metadata: %v", err)
	}
	var taskRes api.TaskCreateResult
	decodeResult(t, createdTask, &taskRes)

	readTaskRaw, err := h.invoke(t, "tok-e2e-rev", "task.read", `{}`, map[string]string{"task": taskRes.TaskID})
	if err != nil {
		t.Fatalf("reviewer task.read: %v", err)
	}
	var taskView api.TaskReadResult
	decodeResult(t, readTaskRaw, &taskView)
	if taskView.Metadata["plan_key"] != planKey {
		t.Fatalf("task.metadata[plan_key] = %q; want %q", taskView.Metadata["plan_key"], planKey)
	}

	// 3. Downstream action, human Reviewer, and roleless Service read exact bytes via `kodema kv get`.
	for name, tr := range map[string]client.Transport{
		"downstream-action": downstreamTransport,
		"reviewer":          revTransport,
		"roleless-service":  svcTransport,
	} {
		var getOut, getErr bytes.Buffer
		code := cli.RunWithDeps(context.Background(), cli.Config{ServerURL: "http://127.0.0.1:8080"}, []string{"kv", "get", planKey, "--project", testProject}, cli.Ports{
			Client: tr,
			Out:    &getOut,
			Err:    &getErr,
		}, cli.CommandDeps{})
		if code != cli.ExitSuccess {
			t.Fatalf("%s `kodema kv get` exit=%d stderr=%q", name, code, getErr.String())
		}
		if getOut.String() != planContent {
			t.Fatalf("%s `kodema kv get` = %q; want %q", name, getOut.String(), planContent)
		}
	}

	// 4. Full 16 MiB binary round-trip over HTTP REST (httpapi.Edge + client.HTTPTransport).
	largeBinary := make([]byte, api.MaxKVValueBytes)
	for i := range largeBinary {
		largeBinary[i] = byte(i % 251)
	}
	largePut, err := revTransport.KVPut(context.Background(), testProject, largeBinary, "application/octet-stream")
	if err != nil {
		t.Fatalf("16 MiB REST KVPut failed: %v", err)
	}
	if largePut.SizeBytes != api.MaxKVValueBytes {
		t.Fatalf("16 MiB REST KVPut size = %d; want %d", largePut.SizeBytes, api.MaxKVValueBytes)
	}
	largeGot, err := svcTransport.KVGet(context.Background(), testProject, largePut.Key)
	if err != nil {
		t.Fatalf("16 MiB REST KVGet failed: %v", err)
	}
	if !bytes.Equal(largeGot.Data, largeBinary) {
		t.Fatalf("16 MiB REST KVGet payload mismatch (got %d bytes)", len(largeGot.Data))
	}
	if largeGot.Raw.Text != nil {
		t.Fatal("16 MiB binary KVGet unexpectedly populated Text")
	}

	// Oversized payload (> 16 MiB) rejected over raw HTTP before reaching handler.
	edge, _ := httpapi.NewEdge(h.router)
	overB64 := base64.StdEncoding.EncodeToString(make([]byte, api.MaxKVValueBytes+1))
	overBody, _ := json.Marshal(api.Request{
		Input: json.RawMessage(jsonBody(t, map[string]any{"data_base64": overB64})),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+testProject+"/kv", bytes.NewReader(overBody))
	req.Header.Set("Authorization", "Bearer tok-e2e-rev")
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized >16 MiB HTTP status = %d body=%s; want 400", rec.Code, rec.Body.String())
	}

	// Regular non-KV operations STILL enforce their tight 1 MiB envelope bound!
	bigNonKV := bytes.Repeat([]byte(" "), api.MaxRequestBytes+1)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+testProject+"/tasks", bytes.NewReader(bigNonKV))
	req2.Header.Set("Authorization", "Bearer "+testToken)
	rec2 := httptest.NewRecorder()
	edge.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("non-KV >1 MiB HTTP status = %d; want 400", rec2.Code)
	}
}

func TestKVConcurrentPutRaceConvergence(t *testing.T) {
	const workers = 16
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}

	tokens := make([]string, workers)
	for i := range workers {
		principalID := id.PrincipalID(fmt.Sprintf("kv-race-eng-%d", i))
		tok := fmt.Sprintf("tok-kv-race-%d", i)
		principal := identity.Principal{
			ID:      principalID,
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   []tenancy.Role{tenancy.RoleEngineer},
			Enabled: true,
			Version: 1,
		}
		h.seedMembership(t, scope, fmt.Sprintf("m-kv-race-%d", i), principalID, 1, tenancy.RoleEngineer)
		h.principals.principals[tok] = principal
		tokens[i] = tok
	}
	h.buildRouter(t)

	for round := range 3 {
		payload := fmt.Sprintf("concurrent identical kv payload round-%d", round)
		sum := sha256.Sum256([]byte(payload))
		wantKey := "kv:sha256:" + hex.EncodeToString(sum[:])

		type outcome struct {
			res api.Result
			err error
		}
		outcomes := make([]outcome, workers)
		ready := make(chan struct{}, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup

		for i := range workers {
			body := jsonBody(t, map[string]any{
				"text":         payload,
				"content_type": fmt.Sprintf("text/plain; charset=utf-8; worker=%d", i),
			})
			wg.Add(1)
			go func(idx int, input string) {
				defer wg.Done()
				ready <- struct{}{}
				<-start
				res, err := h.router.Invoke(context.Background(), ops.Invocation{
					Operation:  "kv.put",
					Credential: tokens[idx],
					Request:    api.Request{Input: json.RawMessage(input)},
					Params:     map[string]string{"project": testProject},
				})
				outcomes[idx] = outcome{res: res, err: err}
			}(i, body)
		}

		for range workers {
			<-ready
		}
		close(start)
		wg.Wait()

		var created, existed int
		var winner api.KVPutResult
		var winnerIdx int
		for i, out := range outcomes {
			if out.err != nil {
				t.Fatalf("round %d worker %d kv.put failed: %v", round, i, out.err)
			}
			var decoded api.KVPutResult
			decodeResult(t, out.res, &decoded)
			if decoded.Key != wantKey {
				t.Fatalf("round %d worker %d key = %q; want %q", round, i, decoded.Key, wantKey)
			}
			if decoded.SizeBytes != int64(len(payload)) {
				t.Fatalf("round %d worker %d size = %d; want %d", round, i, decoded.SizeBytes, len(payload))
			}
			if decoded.Existed {
				existed++
			} else {
				created++
				winner = decoded
				winnerIdx = i
			}
		}
		if created != 1 || existed != workers-1 {
			t.Fatalf("round %d race outcomes: existed=false count=%d, existed=true count=%d; want 1 and %d",
				round, created, existed, workers-1)
		}
		for i, out := range outcomes {
			var decoded api.KVPutResult
			decodeResult(t, out.res, &decoded)
			if decoded.ContentType != winner.ContentType || !decoded.CreatedAt.Equal(winner.CreatedAt) {
				t.Fatalf("round %d worker %d metadata = (%q, %v); want winner (%q, %v)",
					round, i, decoded.ContentType, decoded.CreatedAt, winner.ContentType, winner.CreatedAt)
			}
		}

		gotRaw, err := h.invoke(t, tokens[0], "kv.get", `{}`, map[string]string{"project": testProject, "key": wantKey})
		if err != nil {
			t.Fatalf("round %d kv.get after concurrent puts: %v", round, err)
		}
		var got api.KVGetResult
		decodeResult(t, gotRaw, &got)
		if got.Text == nil || *got.Text != payload {
			t.Fatalf("round %d kv.get text = %v; want %q", round, got.Text, payload)
		}
		wantWinnerID := fmt.Sprintf("kv-race-eng-%d", winnerIdx)
		if got.CreatedBy != wantWinnerID || got.ContentType != winner.ContentType {
			t.Fatalf("round %d kv.get metadata = (%q, %q); want (%q, %q)",
				round, got.CreatedBy, got.ContentType, wantWinnerID, winner.ContentType)
		}
	}
}

package workspace_test

// Deterministic activation fixtures for the workspace tests. The fake secret
// resolver is in-memory and the tests never write outside t.TempDir or start a
// runtime.

import (
	"context"
	"maps"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

func projectScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

type resolveCall struct {
	request secret.ResolveRequest
}

type fakeSecretResolver struct {
	mu        sync.Mutex
	calls     []resolveCall
	values    map[id.SecretName]string
	err       error
	onResolve func(secret.ResolveRequest) ([]secret.Resolved, error)
}

func (resolver *fakeSecretResolver) Resolve(_ context.Context, request secret.ResolveRequest) ([]secret.Resolved, error) {
	names := append([]id.SecretName(nil), request.Names...)
	resolver.mu.Lock()
	resolver.calls = append(resolver.calls, resolveCall{request: secret.ResolveRequest{Scope: request.Scope, Names: names}})
	onResolve := resolver.onResolve
	err := resolver.err
	values := make(map[id.SecretName]string, len(resolver.values))
	maps.Copy(values, resolver.values)
	resolver.mu.Unlock()

	if onResolve != nil {
		return onResolve(request)
	}
	if err != nil {
		return nil, err
	}
	resolved := make([]secret.Resolved, 0, len(request.Names))
	for _, name := range request.Names {
		value, ok := values[name]
		if !ok {
			return nil, fault.New(fault.NotFound, "declared secret is not stored")
		}
		resolved = append(resolved, secret.Resolved{Name: name, Value: value, Scope: request.Scope})
	}
	return resolved, nil
}

func (resolver *fakeSecretResolver) callSnapshot() []resolveCall {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return append([]resolveCall(nil), resolver.calls...)
}

func (resolver *fakeSecretResolver) setValue(name id.SecretName, value string) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.values == nil {
		resolver.values = map[id.SecretName]string{}
	}
	resolver.values[name] = value
}

func testPrepareRequest() engine.PrepareRequest {
	return engine.PrepareRequest{
		Identity: engine.EffectIdentity{
			ExecutionID:    id.ExecutionID("exec-1"),
			ActionID:       id.ActionID("action-1"),
			WorkloadID:     id.WorkloadID("workload-1"),
			IdempotencyKey: "idem-1",
		},
		WorkspaceID: id.WorkspaceID("ws-1"),
		ActionName:  id.ActionName("build"),
		TaskID:      id.TaskID("task-1"),
		Container: definition.Container{
			Image:    "registry.example/image@sha256:0123456789abcdef",
			Commands: []string{"make", "test"},
			Env: []definition.EnvVar{
				{Name: id.EnvName("B"), Value: "2"},
				{Name: id.EnvName("A"), Value: "1"},
			},
			Secrets: []id.SecretName{"DB_PASSWORD"},
			Outputs: []id.OutputKey{"artifact"},
		},
		Inputs: map[id.InputKey]string{
			"source": "line1\nline2\n",
		},
		Outputs: map[id.ActionName]kernel.Outputs{
			"build": {"artifact": "out\x00"},
		},
		Task:       []byte(`{"task":true}`),
		Credential: "kodex.exec-1.token",
	}
}

func fileByPath(t *testing.T, materialization workspace.Materialization, path string) workspace.File {
	t.Helper()
	for _, file := range materialization.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("materialization has no file at %q; files: %v", path, filePaths(materialization))
	return workspace.File{}
}

func filePaths(materialization workspace.Materialization) []string {
	paths := make([]string, 0, len(materialization.Files))
	for _, file := range materialization.Files {
		paths = append(paths, file.Path)
	}
	return paths
}

func requireNoFileAt(t *testing.T, materialization workspace.Materialization, path string) {
	t.Helper()
	for _, file := range materialization.Files {
		if file.Path == path {
			t.Fatalf("materialization unexpectedly contains a file at %q", path)
		}
	}
}

func requireClass(t *testing.T, err error, class fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %q", class)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v has no fault class; want %q", err, class)
	}
	if got != class {
		t.Fatalf("error class = %q (%v); want %q", got, err, class)
	}
}

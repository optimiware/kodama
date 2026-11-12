package workspace_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

func buildConfig(scope tenancy.Scope, resolver *fakeSecretResolver) workspace.MaterializationConfig {
	return workspace.MaterializationConfig{Scope: scope, Secrets: resolver}
}

func TestBuildMaterializationExactSnapshot(t *testing.T) {
	request := testPrepareRequest()
	scope := projectScope("tenant-a", "project-a")
	resolver := &fakeSecretResolver{values: map[id.SecretName]string{"DB_PASSWORD": "p@ss\n"}}

	materialization, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
	if err != nil {
		t.Fatalf("BuildMaterialization: %v", err)
	}
	if materialization.WorkspaceID != id.WorkspaceID("ws-1") {
		t.Fatalf("workspace id = %q; want ws-1", materialization.WorkspaceID)
	}
	if materialization.Credential != "kodex.exec-1.token" {
		t.Fatalf("credential = %q; want the request credential", materialization.Credential)
	}
	if !bytes.Equal(materialization.Task, request.Task) {
		t.Fatalf("task snapshot = %q; want %q", materialization.Task, request.Task)
	}

	input := fileByPath(t, materialization, workspace.InputRoot+"/source")
	if string(input.Data) != "line1\nline2\n" {
		t.Fatalf("input bytes = %q; want exact request bytes", input.Data)
	}
	output := fileByPath(t, materialization, workspace.OutputRoot+"/build/artifact")
	if !bytes.Equal(output.Data, []byte("out\x00")) {
		t.Fatalf("output bytes = %q; want exact settled bytes", output.Data)
	}
	task := fileByPath(t, materialization, workspace.TaskPath)
	if !bytes.Equal(task.Data, request.Task) {
		t.Fatalf("task file = %q; want exact admitted bytes", task.Data)
	}
	for _, file := range materialization.Files {
		if err := file.Validate(); err != nil {
			t.Fatalf("materialized file %q is invalid: %v", file.Path, err)
		}
		if !file.Mode.IsRegular() {
			t.Fatalf("materialized file %q mode %v is not regular", file.Path, file.Mode)
		}
		if file.Mode&0o222 != 0 {
			t.Fatalf("materialized file %q mode %v is writable", file.Path, file.Mode)
		}
		if file.Mode&0o400 == 0 {
			t.Fatalf("materialized file %q mode %v is not owner-readable", file.Path, file.Mode)
		}
	}
	requireNoFileAt(t, materialization, workspace.ReservedCredentialPath)
	for _, file := range materialization.Files {
		if strings.HasPrefix(file.Path, workspace.SecretRoot+"/") {
			t.Fatalf("declared secret materialized as an ordinary file at %q", file.Path)
		}
	}
	if len(materialization.Secrets) != 1 {
		t.Fatalf("secret count = %d; want 1", len(materialization.Secrets))
	}
	if materialization.Secrets[0].Name != "DB_PASSWORD" || materialization.Secrets[0].Value != "p@ss\n" {
		t.Fatalf("secret = %+v; want the resolved read-only value", materialization.Secrets[0])
	}
	if err := materialization.Validate(); err != nil {
		t.Fatalf("Materialization.Validate: %v", err)
	}
}

func TestBuildMaterializationDeepCopiesSnapshot(t *testing.T) {
	request := testPrepareRequest()
	scope := projectScope("tenant-a", "project-a")
	resolver := &fakeSecretResolver{values: map[id.SecretName]string{"DB_PASSWORD": "value"}}

	materialization, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
	if err != nil {
		t.Fatalf("BuildMaterialization: %v", err)
	}

	request.Inputs["source"] = "mutated-after-build"
	request.Task[0] = 'X'
	request.Outputs["build"]["artifact"] = "mutated"
	resolver.setValue("DB_PASSWORD", "mutated-secret")

	input := fileByPath(t, materialization, workspace.InputRoot+"/source")
	if string(input.Data) != "line1\nline2\n" {
		t.Fatalf("input bytes changed to %q after request mutation", input.Data)
	}
	output := fileByPath(t, materialization, workspace.OutputRoot+"/build/artifact")
	if !bytes.Equal(output.Data, []byte("out\x00")) {
		t.Fatalf("output bytes changed to %q after request mutation", output.Data)
	}
	task := fileByPath(t, materialization, workspace.TaskPath)
	if !bytes.Equal(task.Data, []byte(`{"task":true}`)) {
		t.Fatalf("task bytes changed to %q after request mutation", task.Data)
	}
	if materialization.Secrets[0].Value != "value" {
		t.Fatalf("secret value changed to %q after resolver mutation", materialization.Secrets[0].Value)
	}

	input.Data[0] = 'X'
	if request.Inputs["source"] != "mutated-after-build" {
		t.Fatal("mutating a materialized file changed the request input")
	}
}

func TestBuildMaterializationTaskPresence(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	resolver := &fakeSecretResolver{values: map[id.SecretName]string{"DB_PASSWORD": "value"}}

	t.Run("task-free omits task file", func(t *testing.T) {
		request := testPrepareRequest()
		request.TaskID = ""
		request.Task = nil
		materialization, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
		if err != nil {
			t.Fatalf("BuildMaterialization: %v", err)
		}
		requireNoFileAt(t, materialization, workspace.TaskPath)
		if materialization.Task != nil {
			t.Fatalf("task snapshot = %q; want nil", materialization.Task)
		}
		if err := materialization.Validate(); err != nil {
			t.Fatalf("Materialization.Validate: %v", err)
		}
	})

	t.Run("task id without snapshot", func(t *testing.T) {
		request := testPrepareRequest()
		request.Task = nil
		_, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
		requireClass(t, err, fault.InvalidInput)
	})

	t.Run("snapshot without task id", func(t *testing.T) {
		request := testPrepareRequest()
		request.TaskID = ""
		_, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
		requireClass(t, err, fault.InvalidInput)
	})
}

func TestBuildMaterializationResolvesDeclaredSecrets(t *testing.T) {
	request := testPrepareRequest()
	request.Container.Secrets = []id.SecretName{"DB_PASSWORD", "API_KEY"}
	scope := projectScope("tenant-a", "project-a")
	resolver := &fakeSecretResolver{values: map[id.SecretName]string{
		"DB_PASSWORD": "db-value",
		"API_KEY":     "api-value",
	}}

	materialization, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
	if err != nil {
		t.Fatalf("BuildMaterialization: %v", err)
	}
	calls := resolver.callSnapshot()
	if len(calls) != 1 {
		t.Fatalf("resolver call count = %d; want 1", len(calls))
	}
	if calls[0].request.Scope != scope {
		t.Fatalf("resolver scope = %+v; want %+v", calls[0].request.Scope, scope)
	}
	if len(calls[0].request.Names) != 2 || calls[0].request.Names[0] != "DB_PASSWORD" || calls[0].request.Names[1] != "API_KEY" {
		t.Fatalf("resolver names = %v; want declared order", calls[0].request.Names)
	}
	if len(materialization.Secrets) != 2 {
		t.Fatalf("secret count = %d; want 2", len(materialization.Secrets))
	}
	if materialization.Secrets[0].Name != "DB_PASSWORD" || materialization.Secrets[0].Value != "db-value" {
		t.Fatalf("first secret = %+v; want DB_PASSWORD", materialization.Secrets[0])
	}
	if materialization.Secrets[1].Name != "API_KEY" || materialization.Secrets[1].Value != "api-value" {
		t.Fatalf("second secret = %+v; want API_KEY", materialization.Secrets[1])
	}
}

func TestBuildMaterializationNoSecretsDoesNotCallResolver(t *testing.T) {
	request := testPrepareRequest()
	request.Container.Secrets = nil
	scope := projectScope("tenant-a", "project-a")
	resolver := &fakeSecretResolver{}

	materialization, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
	if err != nil {
		t.Fatalf("BuildMaterialization: %v", err)
	}
	if len(materialization.Secrets) != 0 {
		t.Fatalf("secret count = %d; want 0", len(materialization.Secrets))
	}
	if len(resolver.callSnapshot()) != 0 {
		t.Fatal("resolver was called for a request with no declared secrets")
	}
}

func TestBuildMaterializationRejectsResolverMismatch(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	request := testPrepareRequest()
	request.Container.Secrets = []id.SecretName{"DB_PASSWORD", "API_KEY"}

	cases := []struct {
		name     string
		resolved []secret.Resolved
		err      error
		class    fault.Class
	}{
		{
			name:     "missing name",
			resolved: []secret.Resolved{{Name: "DB_PASSWORD", Value: "value", Scope: scope}},
			class:    fault.InvalidInput,
		},
		{
			name: "extra name",
			resolved: []secret.Resolved{
				{Name: "DB_PASSWORD", Value: "value", Scope: scope},
				{Name: "API_KEY", Value: "value", Scope: scope},
				{Name: "EXTRA", Value: "value", Scope: scope},
			},
			class: fault.InvalidInput,
		},
		{
			name: "duplicate name",
			resolved: []secret.Resolved{
				{Name: "DB_PASSWORD", Value: "value", Scope: scope},
				{Name: "DB_PASSWORD", Value: "other", Scope: scope},
			},
			class: fault.InvalidInput,
		},
		{
			name:  "resolver failure",
			err:   fault.New(fault.DependencyUnavailable, "secret store unavailable"),
			class: fault.DependencyUnavailable,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resolver := &fakeSecretResolver{onResolve: func(secret.ResolveRequest) ([]secret.Resolved, error) {
				return testCase.resolved, testCase.err
			}}
			_, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
			requireClass(t, err, testCase.class)
		})
	}
}

func TestBuildMaterializationRefusesInvalidInputs(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")

	cases := []struct {
		name   string
		mutate func(*engine.PrepareRequest, *workspace.MaterializationConfig)
	}{
		{
			name: "input traversal",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.Inputs = map[id.InputKey]string{"../escape": "value"}
			},
		},
		{
			name: "output traversal",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.Outputs = map[id.ActionName]kernel.Outputs{"../escape": {"artifact": "value"}}
			},
		},
		{
			name: "invalid output key",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.Outputs = map[id.ActionName]kernel.Outputs{"build": {"Bad/Key": "value"}}
			},
		},
		{
			name: "reserved credential secret",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.Container.Secrets = []id.SecretName{"KODAMA_TOKEN"}
			},
		},
		{
			name: "reserved environment shadow",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.Container.Secrets = []id.SecretName{"KODAMA_URL"}
			},
		},
		{
			name: "lowercase secret name",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.Container.Secrets = []id.SecretName{"db_password"}
			},
		},
		{
			name: "duplicate secret names",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.Container.Secrets = []id.SecretName{"DB_PASSWORD", "DB_PASSWORD"}
			},
		},
		{
			name: "empty credential",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.Credential = ""
			},
		},
		{
			name: "empty workspace id",
			mutate: func(request *engine.PrepareRequest, _ *workspace.MaterializationConfig) {
				request.WorkspaceID = ""
			},
		},
		{
			name: "invalid scope",
			mutate: func(_ *engine.PrepareRequest, config *workspace.MaterializationConfig) {
				config.Scope = tenancy.Scope{}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := testPrepareRequest()
			resolver := &fakeSecretResolver{values: map[id.SecretName]string{"DB_PASSWORD": "value"}}
			config := buildConfig(scope, resolver)
			testCase.mutate(&request, &config)
			_, err := workspace.BuildMaterialization(context.Background(), request, config)
			requireClass(t, err, fault.InvalidInput)
			if len(resolver.callSnapshot()) != 0 {
				t.Fatal("an invalid request reached the resolver")
			}
		})
	}
}

func TestBuildMaterializationFailureHasNoPartialSnapshot(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	request := testPrepareRequest()
	request.Container.Secrets = []id.SecretName{"DB_PASSWORD", "API_KEY"}

	t.Run("resolver returns values with an error", func(t *testing.T) {
		resolver := &fakeSecretResolver{onResolve: func(secret.ResolveRequest) ([]secret.Resolved, error) {
			return []secret.Resolved{{Name: "DB_PASSWORD", Value: "value", Scope: scope}}, fault.New(fault.DependencyUnavailable, "partial read")
		}}
		materialization, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
		requireClass(t, err, fault.DependencyUnavailable)
		if materialization.WorkspaceID != "" || len(materialization.Files) != 0 || len(materialization.Secrets) != 0 || materialization.Credential != "" {
			t.Fatalf("partial materialization returned on failure: %+v", materialization)
		}
	})

	t.Run("second value oversized", func(t *testing.T) {
		resolver := &fakeSecretResolver{onResolve: func(secret.ResolveRequest) ([]secret.Resolved, error) {
			return []secret.Resolved{
				{Name: "DB_PASSWORD", Value: "value", Scope: scope},
				{Name: "API_KEY", Value: strings.Repeat("x", secret.DefaultBounds.MaxValueBytes+1), Scope: scope},
			}, nil
		}}
		materialization, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
		requireClass(t, err, fault.InvalidInput)
		if materialization.WorkspaceID != "" || len(materialization.Files) != 0 || len(materialization.Secrets) != 0 {
			t.Fatalf("partial materialization returned on failure: %+v", materialization)
		}
	})
}

func TestBuildMaterializationLaterWritesDoNotChangeSnapshot(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	request := testPrepareRequest()
	resolver := &fakeSecretResolver{values: map[id.SecretName]string{"DB_PASSWORD": "first-value"}}

	first, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
	if err != nil {
		t.Fatalf("first BuildMaterialization: %v", err)
	}
	resolver.setValue("DB_PASSWORD", "second-value")
	second, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
	if err != nil {
		t.Fatalf("second BuildMaterialization: %v", err)
	}
	if first.Secrets[0].Value != "first-value" {
		t.Fatalf("first snapshot secret = %q; want first-value", first.Secrets[0].Value)
	}
	if second.Secrets[0].Value != "second-value" {
		t.Fatalf("second snapshot secret = %q; want second-value", second.Secrets[0].Value)
	}
}

func TestBuildMaterializationOnlyProvidedOutputs(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	request := testPrepareRequest()
	request.Outputs = map[id.ActionName]kernel.Outputs{
		"build": {"artifact": "a"},
		"test":  nil,
	}
	resolver := &fakeSecretResolver{values: map[id.SecretName]string{"DB_PASSWORD": "value"}}

	materialization, err := workspace.BuildMaterialization(context.Background(), request, buildConfig(scope, resolver))
	if err != nil {
		t.Fatalf("BuildMaterialization: %v", err)
	}
	fileByPath(t, materialization, workspace.OutputRoot+"/build/artifact")
	for _, file := range materialization.Files {
		if strings.HasPrefix(file.Path, workspace.OutputRoot+"/test/") {
			t.Fatalf("materialization invented an output for a nil output set: %q", file.Path)
		}
	}
}

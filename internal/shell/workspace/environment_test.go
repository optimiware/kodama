package workspace_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

func TestEnvironmentMergesLiteralEnvThenReservedContext(t *testing.T) {
	request := testPrepareRequest()
	got, err := workspace.Environment(request, "https://kodama.example")
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	want := []string{
		"B=2",
		"A=1",
		"KODAMA_URL=https://kodama.example",
		"KODAMA_EXECUTION=exec-1",
		"KODAMA_ACTION=build",
		"KODAMA_TASK=task-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %v; want %v", got, want)
	}
}

func TestEnvironmentOmitsTaskWhenTaskFree(t *testing.T) {
	request := testPrepareRequest()
	request.TaskID = ""
	request.Task = nil
	got, err := workspace.Environment(request, "https://kodama.example")
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	want := []string{
		"B=2",
		"A=1",
		"KODAMA_URL=https://kodama.example",
		"KODAMA_EXECUTION=exec-1",
		"KODAMA_ACTION=build",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %v; want %v", got, want)
	}
	for _, entry := range got {
		if strings.HasPrefix(entry, id.EnvTask+"=") {
			t.Fatalf("task-free environment contains %q", entry)
		}
	}
}

func TestEnvironmentRefusesReservedShadowAndMissingContext(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*engine.PrepareRequest)
		url    string
	}{
		{
			name: "literal shadows reserved URL",
			mutate: func(request *engine.PrepareRequest) {
				request.Container.Env = append(request.Container.Env, definition.EnvVar{Name: id.EnvName(id.EnvURL), Value: "spoofed"})
			},
			url: "https://kodama.example",
		},
		{
			name: "empty URL",
			mutate: func(*engine.PrepareRequest) {
			},
			url: "",
		},
		{
			name: "empty execution id",
			mutate: func(request *engine.PrepareRequest) {
				request.Identity.ExecutionID = ""
			},
			url: "https://kodama.example",
		},
		{
			name: "empty action slug",
			mutate: func(request *engine.PrepareRequest) {
				request.ActionName = ""
			},
			url: "https://kodama.example",
		},
		{
			name: "invalid action slug",
			mutate: func(request *engine.PrepareRequest) {
				request.ActionName = id.ActionName("Not-A-Slug")
			},
			url: "https://kodama.example",
		},
		{
			name: "invalid task id",
			mutate: func(request *engine.PrepareRequest) {
				request.TaskID = id.TaskID("bad/name")
			},
			url: "https://kodama.example",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := testPrepareRequest()
			testCase.mutate(&request)
			_, err := workspace.Environment(request, testCase.url)
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

func TestEnvironmentDoesNotLeakSecretsOrCredential(t *testing.T) {
	request := testPrepareRequest()
	request.Secrets = []engine.SecretValue{{Name: "DB_PASSWORD", Value: "secret-value"}}
	request.Credential = "kodex-credential-token"

	got, err := workspace.Environment(request, "https://kodama.example")
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "secret-value") {
		t.Fatal("environment contains a secret value")
	}
	if strings.Contains(joined, "kodex-credential-token") {
		t.Fatal("environment contains the execution credential")
	}
	if strings.Contains(joined, id.SecretTokenName+"=") {
		t.Fatal("environment contains a KODAMA_TOKEN entry; the credential is a file")
	}
}

//go:build integration

package composition

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/app"
)

// TestOptionalCapabilitiesFailClosed proves the disabled model gateway, tool
// proxy and Git operation surface refuse with a stable public error while the
// rest of the composition keeps working.
func TestOptionalCapabilitiesFailClosed(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)

	_, modelError := environment.call(projectToken, "model.invoke", api.ModelInvokeRequest{
		ProjectID: projectID,
		Model:     "m",
		Messages:  []api.ChatMessage{{Role: "user", Content: "hi"}},
	}, nil, "")
	if modelError.Code != api.CodeNotFound {
		t.Fatalf("model.invoke code = %q; want %q", modelError.Code, api.CodeNotFound)
	}

	_, toolError := environment.call(projectToken, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: projectID,
		Tool:      "search",
		Arguments: json.RawMessage(`{}`),
	}, nil, "")
	if toolError.Code != api.CodeNotFound {
		t.Fatalf("tool.invoke code = %q; want %q", toolError.Code, api.CodeNotFound)
	}

	// Git operations are resource operations, not public routes: both edges
	// refuse them as an unknown operation instead of inventing a binding.
	_, err := environment.transport(projectToken).Invoke(context.Background(), api.Invocation{
		Operation: "git.ref.update",
		Request:   api.Request{Input: json.RawMessage(`{}`)},
	})
	if err == nil {
		t.Fatal("the public client accepted the non-public git.ref.update operation")
	}
	if code := codeOf(t, err); code != api.CodeInvalidInput {
		t.Fatalf("git.ref.update client code = %q; want %q", code, api.CodeInvalidInput)
	}
	_, gitError, err := environment.mcpCall(projectToken, "git.ref.update", map[string]string{}, nil, "")
	if err != nil {
		t.Fatalf("MCP git.ref.update: %v", err)
	}
	if gitError.Code != api.CodeInvalidInput {
		t.Fatalf("MCP git.ref.update code = %q; want %q", gitError.Code, api.CodeInvalidInput)
	}

	// The rest of the composition still serves authorized reads.
	environment.mustInvoke(projectToken, "task.inbox", api.TaskInboxRequest{ProjectID: projectID}, nil, "")
}

// TestProductionProfileRefusesDevAdapters proves the same app.New entry refuses
// the development authenticator, the first-admin seed and every weaker adapter
// under the production profile.
func TestProductionProfileRefusesDevAdapters(t *testing.T) {
	harness := newHarness(t)
	productionInstallation := installation.Config{
		Environment: installation.EnvironmentProduction,
		State:       installation.StatePostgres,
		Messaging:   installation.MessagingNATS,
		Runtime:     installation.RuntimeKubernetes,
		Keys:        installation.KeyAWSKMS,
		DatabaseURL: "postgres://kodama@localhost/kodama?sslmode=verify-full",
		NATSURL:     "tls://localhost:4222",
		PublicURL:   "https://kodama.example.com",
		KeyRef:      "aws-kms:alias/kodama",
	}

	t.Run("development authenticator and seed", func(t *testing.T) {
		config := harness.config
		config.Profile = app.ProfileProduction
		config.Installation = productionInstallation
		config.Dev = harness.config.Dev
		config.Adapters = nil
		_, err := app.New(context.Background(), config)
		if err == nil {
			t.Fatal("production profile accepted the development authenticator and first-admin seed")
		}
		if !strings.Contains(err.Error(), "development authenticator") {
			t.Fatalf("refusal = %v; want the development authenticator named", err)
		}
	})

	t.Run("development adapter", func(t *testing.T) {
		config := harness.config
		config.Profile = app.ProfileProduction
		config.Installation = productionInstallation
		config.Dev = nil
		config.Adapters = &app.Adapters{Runtime: newFakeRuntime(modeComplete)}
		_, err := app.New(context.Background(), config)
		if err == nil {
			t.Fatal("production profile accepted a development runtime adapter")
		}
		if !strings.Contains(err.Error(), "development or test adapter") {
			t.Fatalf("refusal = %v; want the weaker adapter named", err)
		}
	})
}

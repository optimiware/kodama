package cli_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/cli"
)

func makeTestJWTWithClaims(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestTaskHelpDocumentsMetaAndMetadata pins that `kodema task --help`,
// `kodema task create --help`, and `kodema task run --help` all document
// `--meta` and `--metadata <k=v>`.
func TestTaskHelpDocumentsMetaAndMetadata(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "task --help", args: []string{"task", "--help"}},
		{name: "help task", args: []string{"help", "task"}},
		{name: "task create --help", args: []string{"task", "create", "--help"}},
		{name: "help task create", args: []string{"help", "task", "create"}},
		{name: "task run --help", args: []string{"task", "run", "--help"}},
		{name: "help task run", args: []string{"help", "task", "run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ports, out, errOut := testPorts(nil)
			if code := cli.Run(context.Background(), cli.Config{}, tc.args, ports); code != cli.ExitSuccess {
				t.Fatalf("%v exit=%d stderr=%q", tc.args, code, errOut.String())
			}
			helpText := out.String()
			for _, want := range []string{"--meta", "--metadata", "<k=v>"} {
				if !strings.Contains(helpText, want) {
					t.Fatalf("%v help missing %q:\n%s", tc.args, want, helpText)
				}
			}
		})
	}
}

// TestWhoamiShowsActiveServerURL pins that `kodema whoami` displays the active
// server URL in table output (both online and offline) while keeping JSON
// output structured and backwards-compatible.
func TestWhoamiShowsActiveServerURL(t *testing.T) {
	const serverURL = "https://kodema.example.com:8443"
	const issuer = "https://auth.kodema.example"
	const subject = "local:admin"
	principalID, err := cli.DeriveOIDCPrincipalID(issuer, subject)
	if err != nil {
		t.Fatalf("DeriveOIDCPrincipalID: %v", err)
	}
	jwt := makeTestJWTWithClaims(t, map[string]any{
		"iss":                issuer,
		"sub":                subject,
		"preferred_username": "admin",
		"exp":                testNow().Add(time.Hour).Unix(),
	})

	whoamiBody, err := json.Marshal(api.PrincipalWhoamiResult{
		PrincipalID: principalID,
		Kind:        "human",
		TenantID:    "tenant-1",
		ProjectID:   "demo",
		Roles:       []string{"admin"},
	})
	if err != nil {
		t.Fatalf("marshal whoami result: %v", err)
	}

	transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
		if inv.Operation != "principal.whoami" {
			t.Fatalf("unexpected operation %s", inv.Operation)
		}
		return api.Result{Operation: inv.Operation, Body: whoamiBody}, nil
	}}

	t.Run("online table shows server URL even without local OIDC session", func(t *testing.T) {
		cfg := testConfig()
		cfg.ServerURL = serverURL
		cfg.Output = cli.OutputTable
		ports, out, errOut := testPorts(transport)
		code := cli.Run(context.Background(), cfg, []string{"whoami"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("whoami exit=%d stderr=%q", code, errOut.String())
		}
		for _, want := range []string{
			"server\t" + serverURL,
			"principal_id\t" + principalID,
			"scope\tproject (demo)",
		} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("whoami table missing %q:\n%s", want, out.String())
			}
		}
	})

	t.Run("online json stays backwards-compatible with principal.whoami payload", func(t *testing.T) {
		cfg := testConfig()
		cfg.ServerURL = serverURL
		cfg.Output = cli.OutputJSON
		ports, out, errOut := testPorts(transport)
		code := cli.Run(context.Background(), cfg, []string{"whoami"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("whoami --output json exit=%d stderr=%q", code, errOut.String())
		}
		if strings.TrimSpace(out.String()) != string(whoamiBody) {
			t.Fatalf("whoami json = %q; want %q", strings.TrimSpace(out.String()), string(whoamiBody))
		}
	})

	t.Run("offline table uses config server URL or falls back to session api_url", func(t *testing.T) {
		store := fakeRecordStore{record: cli.SessionRecord{
			Version: cli.SessionFileVersion,
			Kind:    "session",
			Token:   jwt,
			Issuer:  issuer,
			APIURL:  "https://session.kodema.example",
		}}
		// With explicit ServerURL in config, config wins.
		ports, out, errOut := testPorts(nil)
		code := cli.RunWithDeps(context.Background(), cli.Config{
			ServerURL: serverURL,
			Output:    cli.OutputTable,
		}, []string{"whoami", "--offline"}, ports, cli.CommandDeps{
			Login: cli.LoginRuntime{Store: store},
		})
		if code != cli.ExitSuccess {
			t.Fatalf("whoami --offline exit=%d stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "server\t"+serverURL) {
			t.Fatalf("whoami --offline missing config server URL:\n%s", out.String())
		}

		// Without config ServerURL, falls back to persisted session api_url.
		ports, out, errOut = testPorts(nil)
		code = cli.RunWithDeps(context.Background(), cli.Config{
			Output: cli.OutputTable,
		}, []string{"whoami", "--offline"}, ports, cli.CommandDeps{
			Login: cli.LoginRuntime{Store: store},
		})
		if code != cli.ExitSuccess {
			t.Fatalf("whoami --offline fallback exit=%d stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "server\thttps://session.kodema.example") {
			t.Fatalf("whoami --offline missing session api_url:\n%s", out.String())
		}

		// Offline JSON stays valid JSON and carries server when known.
		ports, out, errOut = testPorts(nil)
		code = cli.RunWithDeps(context.Background(), cli.Config{
			Output: cli.OutputJSON,
		}, []string{"whoami", "--offline"}, ports, cli.CommandDeps{
			Login: cli.LoginRuntime{Store: store},
		})
		if code != cli.ExitSuccess {
			t.Fatalf("whoami --offline --output json exit=%d stderr=%q", code, errOut.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatalf("whoami --offline --output json invalid JSON %q: %v", out.String(), err)
		}
		if decoded["principal_id"] != principalID || decoded["server"] != "https://session.kodema.example" {
			t.Fatalf("whoami --offline json = %v", decoded)
		}
	})
}

// TestProjectScopeMismatchHintOnProjectNotVisible pins that when a CLI command
// fails with `not_found` ("project is not visible") and the presented bearer
// credential is a JWT carrying a different `project_id` claim, the CLI appends
// a concise client-side hint on stderr without polluting JSON stdout or leaking
// the raw token, and omits the hint when the scope matches, the token is not a
// project-scoped JWT, or the error is unrelated.
func TestProjectScopeMismatchHintOnProjectNotVisible(t *testing.T) {
	projectScopedJWT := makeTestJWTWithClaims(t, map[string]any{
		"iss":        "https://auth.kodema.example",
		"sub":        "local:admin",
		"project_id": "demo",
		"exp":        testNow().Add(time.Hour).Unix(),
	})
	tenantScopedJWT := makeTestJWTWithClaims(t, map[string]any{
		"iss": "https://auth.kodema.example",
		"sub": "local:admin",
		"exp": testNow().Add(time.Hour).Unix(),
	})

	notVisibleTransport := &scriptedTransport{respond: func(_ context.Context, _ int, _ api.Invocation) (api.Result, error) {
		return api.Result{}, fault.New(fault.NotFound, "project is not visible")
	}}

	t.Run("mismatch via credential source emits stderr hint and keeps JSON stdout clean", func(t *testing.T) {
		cfg := testConfig()
		cfg.Output = cli.OutputJSON
		ports, out, errOut := testPorts(notVisibleTransport)
		ports.Credential = &scriptedSource{credentials: []client.Credential{
			{Kind: client.KindSession, Token: projectScopedJWT, ExpiresAt: testNow().Add(time.Hour)},
		}}

		code := cli.Run(context.Background(), cfg, []string{"task", "list", "--project", "other-proj"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit=%d; want %d", code, cli.ExitFailure)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout must be empty on error with --output json; got %q", out.String())
		}
		stderr := errOut.String()
		for _, want := range []string{
			"project is not visible",
			"hint:",
			`"demo"`,
			`"other-proj"`,
			"--credential-file",
			"KODEMA_CREDENTIAL_FILE",
			"KODEMA_TENANT_ADMIN_CREDENTIAL_FILE",
			"kodema login",
		} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("stderr missing %q:\n%s", want, stderr)
			}
		}
		if strings.Contains(stderr, projectScopedJWT) {
			t.Fatal("stderr leaked raw JWT")
		}
	})

	t.Run("mismatch works with api.Error not_found and credential file fallback", func(t *testing.T) {
		apiErrTransport := &scriptedTransport{respond: func(_ context.Context, _ int, _ api.Invocation) (api.Result, error) {
			return api.Result{}, &api.Error{Code: api.CodeNotFound, Message: "project is not visible"}
		}}
		cfg := testConfig()
		cfg.CredentialFile = "/run/kodema/admin-token"
		ports, out, errOut := testPorts(apiErrTransport)
		ports.Files.(*fakeFS).files[cfg.CredentialFile] = []byte(projectScopedJWT + "\n")

		code := cli.Run(context.Background(), cfg, []string{"call", "task.list", "--input", `{"project_id":"foreign"}`}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit=%d; want %d", code, cli.ExitFailure)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout = %q; want empty", out.String())
		}
		if !strings.Contains(errOut.String(), "hint:") || !strings.Contains(errOut.String(), `"demo"`) {
			t.Fatalf("stderr missing project-scope hint:\n%s", errOut.String())
		}
	})

	t.Run("mismatch works for pipeline apply and prints hint once", func(t *testing.T) {
		cfg := testConfig()
		cfg.Output = cli.OutputJSON
		ports, out, errOut := testPorts(notVisibleTransport)
		ports.Credential = &scriptedSource{credentials: []client.Credential{
			{Kind: client.KindSession, Token: projectScopedJWT, ExpiresAt: testNow().Add(time.Hour)},
		}}
		fs := ports.Files.(*fakeFS)
		fs.dirs["/pipelines"] = 0o755
		fs.entries["/pipelines"] = []cli.DirEntry{
			{Name: "a.yaml"},
			{Name: "b.yaml"},
		}
		fs.files["/pipelines/a.yaml"] = []byte("version: v1\nactions: []\n")
		fs.files["/pipelines/b.yaml"] = []byte("version: v1\nactions: []\n")

		code := cli.Run(context.Background(), cfg, []string{"pipeline", "apply", "/pipelines", "--project", "other-proj"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit=%d; want %d", code, cli.ExitFailure)
		}
		if strings.Count(errOut.String(), "hint:") != 1 {
			t.Fatalf("expected exactly 1 hint line for multi-file pipeline apply:\n%s", errOut.String())
		}
		// JSON stdout for directory apply remains valid JSON.
		var decoded map[string]any
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatalf("pipeline apply --output json stdout is not valid JSON %q: %v", out.String(), err)
		}
	})

	t.Run("no hint when JWT project_id matches targeted project", func(t *testing.T) {
		cfg := testConfig()
		ports, _, errOut := testPorts(notVisibleTransport)
		ports.Credential = &scriptedSource{credentials: []client.Credential{
			{Kind: client.KindSession, Token: projectScopedJWT, ExpiresAt: testNow().Add(time.Hour)},
		}}

		code := cli.Run(context.Background(), cfg, []string{"task", "list", "--project", "demo"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit=%d; want %d", code, cli.ExitFailure)
		}
		if strings.Contains(errOut.String(), "hint:") {
			t.Fatalf("unexpected hint when JWT project_id matches target:\n%s", errOut.String())
		}
	})

	t.Run("no hint when JWT has no project_id claim (tenant-scoped)", func(t *testing.T) {
		cfg := testConfig()
		ports, _, errOut := testPorts(notVisibleTransport)
		ports.Credential = &scriptedSource{credentials: []client.Credential{
			{Kind: client.KindSession, Token: tenantScopedJWT, ExpiresAt: testNow().Add(time.Hour)},
		}}

		code := cli.Run(context.Background(), cfg, []string{"task", "list", "--project", "other-proj"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit=%d; want %d", code, cli.ExitFailure)
		}
		if strings.Contains(errOut.String(), "hint:") {
			t.Fatalf("unexpected hint for tenant-scoped JWT:\n%s", errOut.String())
		}
	})

	t.Run("no hint when credential is non-JWT opaque token", func(t *testing.T) {
		cfg := testConfig()
		ports, _, errOut := testPorts(notVisibleTransport)
		ports.Credential = &scriptedSource{credentials: []client.Credential{
			{Kind: client.KindExecution, Token: "kodex.opaque-token-not-jwt"},
		}}

		code := cli.Run(context.Background(), cfg, []string{"task", "list", "--project", "other-proj"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit=%d; want %d", code, cli.ExitFailure)
		}
		if strings.Contains(errOut.String(), "hint:") {
			t.Fatalf("unexpected hint for non-JWT token:\n%s", errOut.String())
		}
	})

	t.Run("no hint on unrelated not_found message or non-not_found error", func(t *testing.T) {
		for _, errCase := range []error{
			fault.New(fault.NotFound, "task not found"),
			fault.New(fault.Forbidden, "project is not visible"),
		} {
			transport := &scriptedTransport{respond: func(_ context.Context, _ int, _ api.Invocation) (api.Result, error) {
				return api.Result{}, errCase
			}}
			cfg := testConfig()
			ports, _, errOut := testPorts(transport)
			ports.Credential = &scriptedSource{credentials: []client.Credential{
				{Kind: client.KindSession, Token: projectScopedJWT, ExpiresAt: testNow().Add(time.Hour)},
			}}
			code := cli.Run(context.Background(), cfg, []string{"task", "list", "--project", "other-proj"}, ports)
			if code != cli.ExitFailure {
				t.Fatalf("exit=%d; want %d", code, cli.ExitFailure)
			}
			if strings.Contains(errOut.String(), "hint:") {
				t.Fatalf("unexpected hint for error %v:\n%s", errCase, errOut.String())
			}
		}
	})

	t.Run("explicit credential file does not fall through to saved session store", func(t *testing.T) {
		cfg := testConfig()
		cfg.CredentialFile = "/nonexistent/explicit-token"
		ports, _, errOut := testPorts(notVisibleTransport)
		ports.Session = &fakeSessionStore{load: client.Credential{
			Kind:      client.KindSession,
			Token:     projectScopedJWT,
			ExpiresAt: testNow().Add(time.Hour),
		}}

		code := cli.Run(context.Background(), cfg, []string{"task", "list", "--project", "other-proj"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit=%d; want %d", code, cli.ExitFailure)
		}
		if strings.Contains(errOut.String(), "hint:") {
			t.Fatalf("unexpected hint from session fallback when CredentialFile is set:\n%s", errOut.String())
		}
	})
}

// TestCallSchemaAdvertisesStringValuedMetadata pins that `kodema call --schema`
// explicitly advertises string values (`additionalProperties: {"type": "string"}`)
// for task and resource binding metadata.
func TestCallSchemaAdvertisesStringValuedMetadata(t *testing.T) {
	for _, op := range []string{
		"task.create",
		"task.create_and_start",
		"task.edit",
		"resource.binding.register",
		"resource.binding.update",
	} {
		t.Run(op, func(t *testing.T) {
			ports, out, errOut := testPorts(nil)
			code := cli.Run(context.Background(), cli.Config{}, []string{"call", "--schema", op}, ports)
			if code != cli.ExitSuccess {
				t.Fatalf("call --schema %s exit=%d stderr=%q", op, code, errOut.String())
			}
			var schema map[string]any
			if err := json.Unmarshal(out.Bytes(), &schema); err != nil {
				t.Fatalf("invalid schema JSON for %s: %v", op, err)
			}
			props := schema["properties"].(map[string]any)
			input := props["input"].(map[string]any)
			inputProps := input["properties"].(map[string]any)
			meta := inputProps["metadata"].(map[string]any)
			if meta["type"] != "object" {
				t.Fatalf("%s metadata type = %v; want object", op, meta["type"])
			}
			additional, ok := meta["additionalProperties"].(map[string]any)
			if !ok || additional["type"] != "string" {
				t.Fatalf("%s metadata additionalProperties = %v; want {\"type\":\"string\"}", op, meta["additionalProperties"])
			}
		})
	}
}

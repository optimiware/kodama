package cli_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/cli"
)

// TestRunCancelAutoFetchesVersion pins `kodema cancel <exec-id>`: when
// --expected-version is omitted, it reads the execution version via
// execution.inspect, generates an idempotency key, and invokes execution.cancel.
func TestRunCancelAutoFetchesVersion(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
		switch idx {
		case 0:
			if inv.Operation != "execution.inspect" {
				t.Fatalf("call 0 = %s; want execution.inspect", inv.Operation)
			}
			body, _ := json.Marshal(map[string]any{
				"execution_id": "exec-42",
				"state":        "running",
				"version":      7,
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		case 1:
			if inv.Operation != "execution.cancel" {
				t.Fatalf("call 1 = %s; want execution.cancel", inv.Operation)
			}
			if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 7 {
				t.Fatalf("expected_version = %v; want 7", inv.Request.ExpectedVersion)
			}
			if inv.Request.IdempotencyKey == "" {
				t.Fatal("missing auto-generated idempotency_key")
			}
			var req api.ExecutionCancelRequest
			if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
				t.Fatalf("decode cancel input: %v", err)
			}
			if req.ExecutionID != "exec-42" {
				t.Fatalf("execution_id = %q; want exec-42", req.ExecutionID)
			}
			body, _ := json.Marshal(api.ExecutionCancelResult{ExecutionVersion: 8})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		default:
			t.Fatalf("unexpected call %d", idx)
			return api.Result{}, nil
		}
	}}
	ports, out, errOut := testPorts(transport)
	cfg := testConfig()
	cfg.Output = cli.OutputTable
	code := cli.Run(context.Background(), cfg, []string{"cancel", "exec-42"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "cancelled execution exec-42 (execution_version=8)") {
		t.Fatalf("stdout = %q", out.String())
	}

	// With explicit --expected-version and --output json, skips execution.inspect.
	explicitTransport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
		if inv.Operation != "execution.cancel" {
			t.Fatalf("op = %s; want execution.cancel", inv.Operation)
		}
		if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 3 {
			t.Fatalf("expected_version = %v; want 3", inv.Request.ExpectedVersion)
		}
		body, _ := json.Marshal(api.ExecutionCancelResult{ExecutionVersion: 4})
		return api.Result{Operation: inv.Operation, Body: body}, nil
	}}
	ports, out, errOut = testPorts(explicitTransport)
	code = cli.Run(context.Background(), cfg, []string{"cancel", "exec-42", "--expected-version", "3", "--output", "json"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("explicit cancel exit = %d; stderr=%q", code, errOut.String())
	}
	var res api.ExecutionCancelResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.ExecutionVersion != 4 {
		t.Fatalf("json output = %q, err=%v", out.String(), err)
	}
}

// TestRunPipelineListAndTaskRunByPipelineName pins `kodema pipeline list` and
// resolving pipeline names in `kodema task run --pipeline <name>`.
func TestRunPipelineListAndTaskRunByPipelineName(t *testing.T) {
	t.Run("pipeline list table and json", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "pipeline.list" {
				t.Fatalf("op = %s; want pipeline.list", inv.Operation)
			}
			body, _ := json.Marshal(api.PipelineListResult{
				Pipelines: []api.PipelineSummaryView{
					{PipelineID: "pipeline-0001", Name: "build", Version: 2},
					{PipelineID: "pipeline-0002", Name: "deploy", Version: 5},
				},
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"pipeline", "list"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		for _, want := range []string{"PIPELINE", "NAME", "VERSION", "pipeline-0001", "build", "2", "pipeline-0002", "deploy", "5"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("pipeline list table missing %q:\n%s", want, out.String())
			}
		}

		ports, out, errOut = testPorts(transport)
		code = cli.Run(context.Background(), cfg, []string{"pipeline", "list", "--output", "json"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("json exit = %d; stderr=%q", code, errOut.String())
		}
		var listRes api.PipelineListResult
		if err := json.Unmarshal(out.Bytes(), &listRes); err != nil || len(listRes.Pipelines) != 2 {
			t.Fatalf("pipeline list json = %q, err=%v", out.String(), err)
		}
	})

	t.Run("task run resolves pipeline name via pipeline.list", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
			switch idx {
			case 0:
				if inv.Operation != "task.create_and_start" {
					t.Fatalf("call 0 = %s; want task.create_and_start", inv.Operation)
				}
				return api.Result{}, fault.New(fault.NotFound, "pipeline not found")
			case 1:
				if inv.Operation != "pipeline.list" {
					t.Fatalf("call 1 = %s; want pipeline.list", inv.Operation)
				}
				body, _ := json.Marshal(api.PipelineListResult{
					Pipelines: []api.PipelineSummaryView{
						{PipelineID: "pipeline-abcd1234", Name: "release", Version: 1},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case 2:
				if inv.Operation != "task.create_and_start" {
					t.Fatalf("call 2 = %s; want task.create_and_start", inv.Operation)
				}
				var req api.TaskCreateAndStartRequest
				if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
					t.Fatalf("unmarshal req: %v", err)
				}
				if req.PipelineID != "pipeline-abcd1234" {
					t.Fatalf("resolved pipeline_id = %q; want pipeline-abcd1234", req.PipelineID)
				}
				body, _ := json.Marshal(api.TaskCreateAndStartResult{
					TaskID:           "task-1",
					ExecutionID:      "exec-1",
					LifecycleVersion: 2,
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected call %d", idx)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{
			"task", "run", "--title", "Ship v1", "--pipeline", "release", "--input", "env=prod",
		}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("task run exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "exec-1") {
			t.Fatalf("stdout = %q", out.String())
		}
	})
}

// TestRunExecutionList pins `kodema execution list` pagination, filtering, and
// table/JSON rendering.
func TestRunExecutionList(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
		if inv.Operation != "execution.list" {
			t.Fatalf("op = %s; want execution.list", inv.Operation)
		}
		var req api.ExecutionListRequest
		if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if req.ProjectID != "shop" || req.State != "running" {
			t.Fatalf("req = %+v; want project=shop state=running", req)
		}
		if idx == 0 {
			body, _ := json.Marshal(api.ExecutionListResult{
				Executions: []api.ExecutionSummary{
					{ExecutionID: "exec-1", TaskID: "task-1", PipelineID: "pipeline-1", State: "running", Version: 2},
				},
				NextCursor: "cur-2",
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}
		body, _ := json.Marshal(api.ExecutionListResult{
			Executions: []api.ExecutionSummary{
				{ExecutionID: "exec-2", State: "running", Version: 1},
			},
		})
		return api.Result{Operation: inv.Operation, Body: body}, nil
	}}
	ports, out, errOut := testPorts(transport)
	cfg := testConfig()
	cfg.Output = cli.OutputTable
	code := cli.Run(context.Background(), cfg, []string{"execution", "list", "--state", "running"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
	}
	for _, want := range []string{"EXECUTION", "STATE", "VERSION", "TASK", "PIPELINE", "exec-1", "task-1", "pipeline-1", "exec-2"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("execution list missing %q:\n%s", want, out.String())
		}
	}
}

// TestRunServiceAndSecretSubcommands pins `kodema service` and `kodema secret`
// workflows including auto-version lookup and secret non-echoing.
func TestRunServiceAndSecretSubcommands(t *testing.T) {
	t.Run("service create, list, credential issue/rotate/revoke", func(t *testing.T) {
		token := "kodsvc.test-token"
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "service.create":
				var req api.ServiceCreateRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.ServiceID != "ci-bot" || req.ProjectID != "shop" || req.Roles == nil {
					t.Fatalf("service.create req = %+v", req)
				}
				body, _ := json.Marshal(api.ServiceCreateResult{ServiceID: "ci-bot", Version: 1})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "service.list":
				body, _ := json.Marshal(api.ServiceListResult{
					Services: []api.ServiceSummaryView{
						{ServiceID: "ci-bot", State: "enabled", Roles: []string{"engineer"}, Version: 3},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "service.credential.issue":
				if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 3 {
					t.Fatalf("issue expected_version = %v; want 3", inv.Request.ExpectedVersion)
				}
				var req api.ServiceCredentialIssueRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.ServiceID != "ci-bot" || req.TTLSeconds != 7200 {
					t.Fatalf("issue req = %+v; want ttl_seconds=7200", req)
				}
				body, _ := json.Marshal(api.ServiceCredentialIssueResult{State: "current", Version: 4, Token: &token})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "service.credential.rotate":
				if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 3 {
					t.Fatalf("rotate expected_version = %v; want 3", inv.Request.ExpectedVersion)
				}
				body, _ := json.Marshal(api.ServiceCredentialRotateResult{State: "current", Version: 5, Token: &token})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "service.credential.revoke":
				if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 3 {
					t.Fatalf("revoke expected_version = %v; want 3", inv.Request.ExpectedVersion)
				}
				invFlag := true
				body, _ := json.Marshal(api.ServiceCredentialRevokeResult{Version: 6, Invalidated: &invFlag})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		cfg := testConfig()
		cfg.Output = cli.OutputTable

		ports, out, errOut := testPorts(transport)
		if code := cli.Run(context.Background(), cfg, []string{"service", "create", "ci-bot", "--role", "engineer"}, ports); code != cli.ExitSuccess {
			t.Fatalf("service create exit=%d stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "ci-bot") {
			t.Fatalf("service create stdout = %q", out.String())
		}

		ports, out, errOut = testPorts(transport)
		if code := cli.Run(context.Background(), cfg, []string{"service", "list"}, ports); code != cli.ExitSuccess {
			t.Fatalf("service list exit=%d stderr=%q", code, errOut.String())
		}
		for _, want := range []string{"SERVICE", "STATE", "ROLES", "VERSION", "ci-bot", "enabled", "engineer", "3"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("service list missing %q:\n%s", want, out.String())
			}
		}

		ports, out, errOut = testPorts(transport)
		if code := cli.Run(context.Background(), cfg, []string{"service", "credential", "issue", "ci-bot", "--ttl", "2h"}, ports); code != cli.ExitSuccess {
			t.Fatalf("service credential issue exit=%d stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), token) {
			t.Fatalf("service credential issue stdout = %q", out.String())
		}

		ports, _, errOut = testPorts(transport)
		if code := cli.Run(context.Background(), cfg, []string{"service", "credential", "rotate", "ci-bot", "--ttl-seconds", "1800"}, ports); code != cli.ExitSuccess {
			t.Fatalf("service credential rotate exit=%d stderr=%q", code, errOut.String())
		}

		ports, out, errOut = testPorts(transport)
		if code := cli.Run(context.Background(), cfg, []string{"service", "credential", "revoke", "ci-bot"}, ports); code != cli.ExitSuccess {
			t.Fatalf("service credential revoke exit=%d stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "invalidated\ttrue") {
			t.Fatalf("service credential revoke stdout = %q", out.String())
		}
	})

	t.Run("secret set, list, delete with auto-version lookup", func(t *testing.T) {
		const secretPlaintext = "super-secret-value-xyz"
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "secret.write":
				var req api.SecretWriteRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.ProjectID != "shop" || req.SecretID != "API_KEY" || req.Value != secretPlaintext {
					t.Fatalf("secret.write req = %+v", req)
				}
				body, _ := json.Marshal(api.SecretWriteResult{SecretID: "API_KEY", Name: "API_KEY", Version: 1})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "secret.metadata.list":
				body, _ := json.Marshal(api.SecretMetadataListResult{
					Secrets: []api.SecretMetadataView{
						{SecretID: "API_KEY", Name: "API_KEY", Version: 2, UpdatedAt: testNow()},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "secret.metadata.read":
				body, _ := json.Marshal(api.SecretMetadataReadResult{
					SecretID: "API_KEY", Name: "API_KEY", Version: 2, UpdatedAt: testNow(),
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "secret.delete":
				if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 2 {
					t.Fatalf("secret.delete expected_version = %v; want 2", inv.Request.ExpectedVersion)
				}
				body, _ := json.Marshal(api.SecretDeleteResult{Version: 3})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		cfg := testConfig()
		cfg.Output = cli.OutputTable

		ports, out, errOut := testPorts(transport)
		ports.In = strings.NewReader(secretPlaintext + "\n")
		if code := cli.Run(context.Background(), cfg, []string{"secret", "set", "API_KEY", "--stdin"}, ports); code != cli.ExitSuccess {
			t.Fatalf("secret set exit=%d stderr=%q", code, errOut.String())
		}
		if strings.Contains(out.String()+errOut.String(), secretPlaintext) {
			t.Fatal("secret set leaked secret plaintext to output")
		}
		if !strings.Contains(out.String(), "API_KEY") {
			t.Fatalf("secret set stdout = %q", out.String())
		}

		ports, out, errOut = testPorts(transport)
		if code := cli.Run(context.Background(), cfg, []string{"secret", "list"}, ports); code != cli.ExitSuccess {
			t.Fatalf("secret list exit=%d stderr=%q", code, errOut.String())
		}
		for _, want := range []string{"SECRET", "NAME", "VERSION", "UPDATED_AT", "API_KEY", "2"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("secret list missing %q:\n%s", want, out.String())
			}
		}

		ports, out, errOut = testPorts(transport)
		if code := cli.Run(context.Background(), cfg, []string{"secret", "delete", "API_KEY"}, ports); code != cli.ExitSuccess {
			t.Fatalf("secret delete exit=%d stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "deleted secret API_KEY (version=3)") {
			t.Fatalf("secret delete stdout = %q", out.String())
		}
	})
}

// TestRunTaskAsk pins `kodema task ask` including $KODEMA_TASK default and
// auto-fetched lifecycle version.
func TestRunTaskAsk(t *testing.T) {
	t.Setenv("KODEMA_TASK", "task-agent-1")
	transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
		switch idx {
		case 0:
			if inv.Operation != "task.read" {
				t.Fatalf("call 0 = %s; want task.read", inv.Operation)
			}
			body, _ := json.Marshal(api.TaskReadResult{
				TaskID:           "task-agent-1",
				Title:            "Draft PR",
				State:            "running",
				ContentVersion:   1,
				LifecycleVersion: 4,
				Initiator:        "oidc-1",
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		case 1:
			if inv.Operation != "task.ask" {
				t.Fatalf("call 1 = %s; want task.ask", inv.Operation)
			}
			if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 4 {
				t.Fatalf("expected_version = %v; want 4", inv.Request.ExpectedVersion)
			}
			var req api.TaskAskRequest
			_ = json.Unmarshal(inv.Request.Input, &req)
			if req.TaskID != "task-agent-1" || req.Question != "Approve draft?" || req.Assignment != "role/engineer" || len(req.References) != 1 || req.References[0] != "commit=abc123" {
				t.Fatalf("task.ask req = %+v", req)
			}
			body, _ := json.Marshal(api.TaskAskResult{QuestionID: "q-9", LifecycleVersion: 5, QuestionVersion: 1})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		default:
			t.Fatalf("unexpected call %d", idx)
			return api.Result{}, nil
		}
	}}
	ports, out, errOut := testPorts(transport)
	cfg := testConfig()
	cfg.Output = cli.OutputTable
	code := cli.Run(context.Background(), cfg, []string{
		"task", "ask", "--question", "Approve draft?", "--to", "role/engineer", "--ref", "commit=abc123",
	}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("task ask exit=%d stderr=%q", code, errOut.String())
	}
	for _, want := range []string{"task_id\ttask-agent-1", "question_id\tq-9", "lifecycle_version\t5"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("task ask stdout missing %q:\n%s", want, out.String())
		}
	}
}

// TestRunGitConfigure pins `kodema git configure` in both --print and execute modes.
func TestRunGitConfigure(t *testing.T) {
	cfg := cli.Config{
		ServerURL: "https://127.0.0.1:8443",
		CAFile:    "/tmp/kodema-ca.pem",
		Output:    cli.OutputTable,
	}
	ports, out, errOut := testPorts(nil)
	code := cli.RunWithDeps(context.Background(), cfg, []string{
		"git", "configure", "--print", "--session", "/tmp/session.json",
	}, ports, cli.CommandDeps{CommandPath: "/usr/local/bin/kodema"})
	if code != cli.ExitSuccess {
		t.Fatalf("git configure --print exit=%d stderr=%q", code, errOut.String())
	}
	rendered := out.String()
	for _, want := range []string{
		"git config --local --replace-all credential.https://127.0.0.1:8443.helper ''",
		"git config --local --add credential.https://127.0.0.1:8443.helper",
		"--trusted-origin https://127.0.0.1:8443 git credential",
		"--ca-file /tmp/kodema-ca.pem",
		"--session /tmp/session.json",
		"git config --local http.https://127.0.0.1:8443.sslCAInfo /tmp/kodema-ca.pem",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("git configure --print missing %q:\n%s", want, rendered)
		}
	}

	var executed [][]string
	ports, out, errOut = testPorts(nil)
	code = cli.RunWithDeps(context.Background(), cfg, []string{
		"git", "configure", "--global",
	}, ports, cli.CommandDeps{
		CommandPath: "/usr/local/bin/kodema",
		GitConfig: func(_ context.Context, args []string) error {
			executed = append(executed, append([]string(nil), args...))
			return nil
		},
	})
	if code != cli.ExitSuccess {
		t.Fatalf("git configure --global exit=%d stderr=%q", code, errOut.String())
	}
	if len(executed) != 3 {
		t.Fatalf("executed %d git commands; want 3: %v", len(executed), executed)
	}
	if executed[0][1] != "--global" || executed[2][2] != "http.https://127.0.0.1:8443.sslCAInfo" {
		t.Fatalf("executed git args = %v", executed)
	}
}

// TestRunCallDiscoveryAndErgonomics pins `kodema call --list`, `kodema call
// --schema <op>`, space-separated `--flag value`, and `--project` /
// `$KODEMA_PROJECT` inheritance.
func TestRunCallDiscoveryAndErgonomics(t *testing.T) {
	t.Run("call --list works offline without server URL", func(t *testing.T) {
		ports, out, errOut := testPorts(nil)
		code := cli.Run(context.Background(), cli.Config{Output: cli.OutputTable}, []string{"call", "--list"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("call --list exit=%d stderr=%q", code, errOut.String())
		}
		for _, want := range []string{"OPERATION", "service.create", "execution.cancel", "pipeline.list"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("call --list missing %q", want)
			}
		}

		ports, out, errOut = testPorts(nil)
		code = cli.Run(context.Background(), cli.Config{Output: cli.OutputJSON}, []string{"call", "--list"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("call --list json exit=%d stderr=%q", code, errOut.String())
		}
		var listPayload struct {
			Operations []struct {
				Operation string `json:"operation"`
			} `json:"operations"`
		}
		if err := json.Unmarshal(out.Bytes(), &listPayload); err != nil || len(listPayload.Operations) != len(api.Routes()) {
			t.Fatalf("call --list json = %d ops, err=%v", len(listPayload.Operations), err)
		}
	})

	t.Run("call --schema prints valid JSON schema offline", func(t *testing.T) {
		ports, out, errOut := testPorts(nil)
		code := cli.Run(context.Background(), cli.Config{}, []string{"call", "--schema", "service.credential.issue"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("call --schema exit=%d stderr=%q", code, errOut.String())
		}
		var schema map[string]any
		if err := json.Unmarshal(out.Bytes(), &schema); err != nil {
			t.Fatalf("call --schema invalid JSON: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), `"expected_version"`) || !strings.Contains(out.String(), `"idempotency_key"`) {
			t.Fatalf("schema missing expected_version/idempotency_key:\n%s", out.String())
		}
	})

	t.Run("call accepts --flag value and inherits KODEMA_PROJECT", func(t *testing.T) {
		t.Setenv("KODEMA_PROJECT", "proj-env")
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "pipeline.list" {
				t.Fatalf("op = %s; want pipeline.list", inv.Operation)
			}
			var req api.PipelineListRequest
			if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
				t.Fatalf("unmarshal input: %v", err)
			}
			if req.ProjectID != "proj-env" {
				t.Fatalf("project_id = %q; want proj-env", req.ProjectID)
			}
			return api.Result{Operation: inv.Operation, Body: json.RawMessage(`{"pipelines":[]}`)}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Project = ""
		code := cli.Run(context.Background(), cfg, []string{
			"call", "pipeline.list", "--input", "{}", "--output", "json",
		}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("call exit=%d stderr=%q", code, errOut.String())
		}
		if strings.TrimSpace(out.String()) != `{"pipelines":[]}` {
			t.Fatalf("stdout = %q", out.String())
		}
	})
}

// TestRunWatchDeduplicatesUnchangedSnapshots pins that `kodema --output json
// watch <exec-id>` emits one line per state/version change rather than every
// unchanged poll tick.
func TestRunWatchDeduplicatesUnchangedSnapshots(t *testing.T) {
	polls := []string{
		`{"execution_id":"exec-1","state":"running","version":1,"actions":[{"action_id":"a1","name":"build","kind":"container","state":"running","version":1}]}`,
		`{"execution_id":"exec-1","state":"running","version":1,"actions":[{"action_id":"a1","name":"build","kind":"container","state":"running","version":1}]}`,
		`{"execution_id":"exec-1","state":"running","version":1,"actions":[{"action_id":"a1","name":"build","kind":"container","state":"running","version":1}]}`,
		`{"execution_id":"exec-1","state":"running","version":2,"actions":[{"action_id":"a1","name":"build","kind":"container","state":"succeeded","version":2}]}`,
		`{"execution_id":"exec-1","state":"succeeded","version":3,"actions":[{"action_id":"a1","name":"build","kind":"container","state":"succeeded","version":2}]}`,
	}
	transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
		return api.Result{Operation: inv.Operation, Body: json.RawMessage(polls[idx])}, nil
	}}
	ports, out, errOut := testPorts(transport)
	code := cli.Run(context.Background(), testConfig(), []string{"watch", "exec-1"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("watch exit=%d stderr=%q", code, errOut.String())
	}
	if len(transport.recorded()) != 5 {
		t.Fatalf("polls = %d; want 5", len(transport.recorded()))
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("emitted %d NDJSON lines; want 3 (deduplicated):\n%s", len(lines), out.String())
	}
}

// TestRunInspectTableHumanReadable pins that `kodema inspect` in table mode
// renders clean rows without raw minified JSON arrays inside TSV cells.
func TestRunInspectTableHumanReadable(t *testing.T) {
	body, _ := json.Marshal(api.ExecutionInspectResult{
		ExecutionID: "exec-99",
		TaskID:      "task-9",
		PipelineID:  "pipeline-1",
		State:       "running",
		Version:     3,
		Actions: []api.ExecutionAction{
			{
				ActionID: "act-1",
				Index:    0,
				Name:     "build-image",
				Kind:     "container",
				State:    "succeeded",
				Version:  2,
				Outputs:  map[string]string{"image_digest": "sha256:abcdef"},
			},
			{
				ActionID:   "act-2",
				Index:      1,
				Name:       "approve-release",
				Kind:       "request",
				State:      "running",
				Version:    1,
				Assignment: "role/reviewer",
				Subject: &api.Subject{Fields: []api.SubjectField{
					{Name: "image_digest", Kind: "string", Value: "sha256:abcdef"},
				}},
			},
		},
		Cleanup: api.ExecutionCleanup{State: "not_needed"},
	})
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
		return api.Result{Operation: inv.Operation, Body: body}, nil
	}}
	ports, out, errOut := testPorts(transport)
	cfg := testConfig()
	cfg.Output = cli.OutputTable
	code := cli.Run(context.Background(), cfg, []string{"inspect", "exec-99"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("inspect exit=%d stderr=%q", code, errOut.String())
	}
	rendered := out.String()
	if strings.Contains(rendered, `[{"action_id"`) {
		t.Fatalf("inspect table dumped raw minified JSON:\n%s", rendered)
	}
	for _, want := range []string{
		"execution_id", "exec-99", "task-9", "INDEX", "ACTION", "NAME", "KIND", "STATE",
		"build-image", "image_digest=sha256:abcdef", "approve-release", "role/reviewer",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("inspect table missing %q:\n%s", want, rendered)
		}
	}
}

// TestWhoamiAndMemberListShowHumanLabels pins that `kodema whoami` and `kodema
// member list` surface human-readable JWT display labels when the local session
// matches the principal ID.
func TestWhoamiAndMemberListShowHumanLabels(t *testing.T) {
	const issuer = "https://auth.kodema.example"
	const subject = "local:reviewer"
	principalID, err := cli.DeriveOIDCPrincipalID(issuer, subject)
	if err != nil {
		t.Fatalf("DeriveOIDCPrincipalID: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claimsJSON, _ := json.Marshal(map[string]any{
		"iss":                issuer,
		"sub":                subject,
		"preferred_username": "alice",
		"email":              "alice@example.com",
		"exp":                testNow().Add(time.Hour).Unix(),
	})
	jwt := header + "." + base64.RawURLEncoding.EncodeToString(claimsJSON) + ".sig"
	store := fakeRecordStore{record: cli.SessionRecord{
		Version: cli.SessionFileVersion,
		Kind:    "session",
		Token:   jwt,
		Issuer:  issuer,
	}}

	transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
		switch inv.Operation {
		case "principal.whoami":
			body, _ := json.Marshal(api.PrincipalWhoamiResult{
				PrincipalID: principalID,
				Kind:        "human",
				TenantID:    "tenant-1",
				Roles:       []string{"reviewer"},
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		case "membership.list":
			body, _ := json.Marshal(api.MembershipListResult{
				Memberships: []api.MembershipView{
					{PrincipalID: principalID, Roles: []string{"reviewer"}, Version: 1},
					{PrincipalID: "oidc-00000000000000000000000000000001", Roles: []string{"engineer"}, Version: 2},
				},
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		default:
			t.Fatalf("unexpected op %s", inv.Operation)
			return api.Result{}, nil
		}
	}}
	cfg := testConfig()
	cfg.Output = cli.OutputTable

	ports, out, errOut := testPorts(transport)
	code := cli.RunWithDeps(context.Background(), cfg, []string{"whoami"}, ports, cli.CommandDeps{
		Login: cli.LoginRuntime{Store: store},
	})
	if code != cli.ExitSuccess {
		t.Fatalf("whoami exit=%d stderr=%q", code, errOut.String())
	}
	for _, want := range []string{principalID, "username\talice", "email\talice@example.com", "subject\tlocal:reviewer"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("whoami missing %q:\n%s", want, out.String())
		}
	}

	ports, out, errOut = testPorts(transport)
	code = cli.RunWithDeps(context.Background(), cfg, []string{"member", "list"}, ports, cli.CommandDeps{
		Login: cli.LoginRuntime{Store: store},
	})
	if code != cli.ExitSuccess {
		t.Fatalf("member list exit=%d stderr=%q", code, errOut.String())
	}
	for _, want := range []string{"PRINCIPAL", "LABEL", "ROLES", "VERSION", "alice <alice@example.com> (you)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("member list missing %q:\n%s", want, out.String())
		}
	}
}

// TestHelpDocumentsTrustedOriginAndTaskInput pins help additions from #20.
func TestHelpDocumentsTrustedOriginAndTaskInput(t *testing.T) {
	ports, out, errOut := testPorts(nil)
	if code := cli.Run(context.Background(), cli.Config{}, []string{"--help"}, ports); code != cli.ExitSuccess {
		t.Fatalf("--help exit=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "--trusted-origin") {
		t.Fatalf("top-level help missing --trusted-origin:\n%s", out.String())
	}

	ports, out, errOut = testPorts(nil)
	if code := cli.Run(context.Background(), cli.Config{}, []string{"task", "--help"}, ports); code != cli.ExitSuccess {
		t.Fatalf("task --help exit=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "--input") || !strings.Contains(out.String(), "ask") {
		t.Fatalf("task --help missing --input or ask:\n%s", out.String())
	}
}

// TestInboxFallsBackCleanlyWhenInspectForbidden pins that a pure reviewer
// without execution.inspect permission still renders the inbox table cleanly
// with version, task_id, action_id, and full SHA-256 digest.
func TestInboxFallsBackCleanlyWhenInspectForbidden(t *testing.T) {
	const digest = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
		switch inv.Operation {
		case "task.inbox":
			body, _ := json.Marshal(api.TaskInboxResult{
				Items: []api.InboxItem{{
					Kind:        "request",
					TaskID:      "task-55",
					ExecutionID: "exec-55",
					ActionID:    "act-55",
					Assignment:  "role/reviewer",
					Subject: &api.Subject{Fields: []api.SubjectField{
						{Name: "image_digest", Kind: "string", Value: digest},
					}},
					Version: 2,
					At:      testNow(),
				}},
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		case "execution.inspect":
			return api.Result{}, fault.New(fault.Forbidden, "reviewer cannot inspect executions")
		default:
			t.Fatalf("unexpected op %s", inv.Operation)
			return api.Result{}, nil
		}
	}}
	ports, out, errOut := testPorts(transport)
	cfg := testConfig()
	cfg.Output = cli.OutputTable
	code := cli.Run(context.Background(), cfg, []string{"inbox"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("inbox exit=%d stderr=%q", code, errOut.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("inbox stderr=%q; want empty on best-effort inspect fallback", errOut.String())
	}
	for _, want := range []string{"task-55", "exec-55", "act-55", "2", digest} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("inbox table missing %q:\n%s", want, out.String())
		}
	}
}

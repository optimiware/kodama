package cli_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/cli"
)

func generateTestCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kodama-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func makeUnsignedTestJWT(t *testing.T, issuer, subject string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claimsJSON, err := json.Marshal(map[string]any{
		"iss": issuer,
		"sub": subject,
		"exp": testNow().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)
	return header + "." + payload + ".test-sig"
}

type fakeRecordStore struct {
	record cli.SessionRecord
	err    error
}

func (s fakeRecordStore) LoadRecord(context.Context) (cli.SessionRecord, error) {
	if s.err != nil {
		return cli.SessionRecord{}, s.err
	}
	return s.record, nil
}

func (s fakeRecordStore) SaveRecord(context.Context, cli.SessionRecord) error { return nil }
func (s fakeRecordStore) DeleteRecord(context.Context) error                  { return nil }

type staticDiscoveryOIDC struct {
	doc   api.DiscoveryDocument
	calls int
}

func (o *staticDiscoveryOIDC) Discover(context.Context, string) (api.DiscoveryDocument, error) {
	o.calls++
	return o.doc, nil
}

func (o *staticDiscoveryOIDC) Token(context.Context, cli.TokenRequest) (cli.TokenResponse, error) {
	return cli.TokenResponse{}, nil
}

func (o *staticDiscoveryOIDC) DeviceAuthorize(context.Context, cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
	return cli.DeviceAuthorizationResponse{}, nil
}

func (o *staticDiscoveryOIDC) Revoke(context.Context, cli.RevocationRequest) error { return nil }
func (o *staticDiscoveryOIDC) JWKS(context.Context, string) (json.RawMessage, error) {
	return nil, nil
}

// TestHelpAndZeroArgCommandSummary pins Requirement 1: help/--help/-h exit 0 on
// Out, while zero args keeps ExitUsage and prints the command summary on Err.
func TestHelpAndZeroArgCommandSummary(t *testing.T) {
	ports, out, errOut := testPorts(&scriptedTransport{})
	code := cli.Run(context.Background(), cli.Config{}, nil, ports)
	if code != cli.ExitUsage {
		t.Fatalf("zero-arg exit = %d; want %d", code, cli.ExitUsage)
	}
	if out.Len() != 0 {
		t.Fatalf("zero-arg stdout = %q; want empty", out.String())
	}
	for _, want := range []string{"usage error: a command is required", "Commands:", "logs", "inbox", "approve", "reject", "task", "whoami", "member", "mcp"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("zero-arg stderr missing %q:\n%s", want, errOut.String())
		}
	}

	helpInvocations := [][]string{
		{"help"},
		{"--help"},
		{"-h"},
		{"help", "logs"},
		{"help", "inbox"},
		{"help", "approve"},
		{"help", "reject"},
		{"help", "task"},
		{"help", "task", "create"},
		{"help", "task", "run"},
		{"help", "whoami"},
		{"help", "member"},
		{"help", "member", "grant"},
		{"help", "mcp"},
		{"help", "mcp", "stdio"},
		{"help", "mcp", "install"},
		{"logs", "--help"},
		{"logs", "-h"},
		{"inbox", "--help"},
		{"approve", "-h"},
		{"reject", "--help"},
		{"task", "--help"},
		{"task", "create", "-h"},
		{"whoami", "-h"},
		{"member", "revoke", "--help"},
		{"mcp", "install", "-h"},
		{"login", "--help"},
		{"call", "-h"},
	}
	for _, argv := range helpInvocations {
		t.Run(strings.Join(argv, "_"), func(t *testing.T) {
			p, stdout, stderr := testPorts(&scriptedTransport{})
			got := cli.Run(context.Background(), cli.Config{}, argv, p)
			if got != cli.ExitSuccess {
				t.Fatalf("Run(%v) exit = %d; want 0 (stderr=%q)", argv, got, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Usage:") {
				t.Fatalf("Run(%v) stdout = %q; want Usage", argv, stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("Run(%v) stderr = %q; want empty", argv, stderr.String())
			}
		})
	}

	p, stdout, stderr := testPorts(&scriptedTransport{})
	if got := cli.Run(context.Background(), cli.Config{}, []string{"help", "nonexistent"}, p); got != cli.ExitUsage {
		t.Fatalf("unknown help topic exit = %d; want %d (stdout=%q stderr=%q)", got, cli.ExitUsage, stdout.String(), stderr.String())
	}
}

// TestParseCABundleAndConfigCAFile pins Requirement 2 config & PEM validation.
func TestParseCABundleAndConfigCAFile(t *testing.T) {
	caPEM := generateTestCAPEM(t)
	pool, err := cli.ParseCABundle(caPEM)
	if err != nil || pool == nil {
		t.Fatalf("ParseCABundle(valid) = %v, %v; want pool", pool, err)
	}
	for _, invalid := range [][]byte{nil, []byte("   "), []byte("not a certificate")} {
		if _, err := cli.ParseCABundle(invalid); err == nil {
			t.Fatalf("ParseCABundle(%q) succeeded; want error", invalid)
		}
	}

	cfg, err := cli.ParseConfig([]byte(`{"server_url":"https://kodama.example.com","ca_file":"/etc/kodama/ca.crt"}`))
	if err != nil {
		t.Fatalf("ParseConfig(ca_file): %v", err)
	}
	if cfg.CAFile != "/etc/kodama/ca.crt" {
		t.Fatalf("CAFile = %q; want /etc/kodama/ca.crt", cfg.CAFile)
	}
	if err := (cli.Config{ServerURL: "https://kodama.example.com", CAFile: "   "}).Validate(); err == nil {
		t.Fatal("Config.Validate accepted whitespace-only CAFile")
	}
}

// TestRunLogsPaginatesAndDecodesRawBytes pins Requirement 3 non-follow mode.
func TestRunLogsPaginatesAndDecodesRawBytes(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, index int, inv api.Invocation) (api.Result, error) {
		switch index {
		case 0:
			if inv.Operation != "execution.inspect" {
				t.Fatalf("call 0 = %s; want execution.inspect", inv.Operation)
			}
			body, _ := json.Marshal(map[string]any{
				"execution_id":   "exec-1",
				"state":          "running",
				"version":        2,
				"current_action": 1,
				"actions": []map[string]any{
					{"action_id": "act-0", "index": 0, "name": "prep", "kind": "container", "state": "succeeded", "version": 2},
					{"action_id": "act-1", "index": 1, "name": "build", "kind": "container", "state": "running", "version": 1},
				},
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		case 1:
			var req api.ExecutionLogsReadRequest
			if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
				t.Fatalf("unmarshal logs req 1: %v", err)
			}
			if req.ActionID != "act-1" || req.Stream != "stdout" || req.Page != nil {
				t.Fatalf("logs req 1 = %+v", req)
			}
			body, _ := json.Marshal(api.ExecutionLogsReadResult{
				Chunks: []api.LogChunk{
					{ActionID: "act-1", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("hello ")},
				},
				NextCursor: "cursor-page-2",
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		case 2:
			var req api.ExecutionLogsReadRequest
			if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
				t.Fatalf("unmarshal logs req 2: %v", err)
			}
			if req.Page == nil || req.Page.Cursor != "cursor-page-2" {
				t.Fatalf("logs req 2 page = %+v; want cursor-page-2", req.Page)
			}
			body, _ := json.Marshal(api.ExecutionLogsReadResult{
				Chunks: []api.LogChunk{
					{ActionID: "act-1", Stream: "stdout", Offset: 6, Sequence: 2, Data: []byte("world\n")},
				},
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		default:
			t.Fatalf("unexpected call %d: %s", index, inv.Operation)
			return api.Result{}, nil
		}
	}}

	ports, out, errOut := testPorts(transport)
	cfg := testConfig()
	cfg.Output = cli.OutputTable
	code := cli.Run(context.Background(), cfg, []string{"logs", "exec-1"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
	}
	if out.String() != "hello world\n" {
		t.Fatalf("stdout = %q; want exact decoded bytes %q", out.String(), "hello world\n")
	}
}

// TestRunLogsFollowDrainsAndExitsByTerminalStatus pins Requirement 3 -f/--follow.
func TestRunLogsFollowDrainsAndExitsByTerminalStatus(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		var sleeps int
		inspectCalls := 0
		logCalls := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				inspectCalls++
				state := "running"
				if inspectCalls >= 2 {
					state = "succeeded"
				}
				body, _ := json.Marshal(map[string]any{
					"execution_id": "exec-1",
					"state":        state,
					"actions": []map[string]any{
						{"action_id": "act-1", "name": "build", "kind": "container", "state": state},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "execution.logs.read":
				logCalls++
				var chunks []api.LogChunk
				switch logCalls {
				case 1:
					chunks = []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("step 1\n")}}
				case 2:
					chunks = []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 7, Sequence: 2, Data: []byte("step 2\n")}}
				}
				body, _ := json.Marshal(api.ExecutionLogsReadResult{Chunks: chunks})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		ports.Sleep = func(time.Duration) { sleeps++ }
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"logs", "exec-1", "-f"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if sleeps != 1 {
			t.Fatalf("sleeps = %d; want 1", sleeps)
		}
		if out.String() != "step 1\nstep 2\n" {
			t.Fatalf("stdout = %q; want both drained chunks", out.String())
		}
	})

	t.Run("failed drains final crash logs and exits 1", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				body, _ := json.Marshal(map[string]any{
					"execution_id": "exec-fail",
					"state":        "failed",
					"actions": []map[string]any{
						{"action_id": "act-1", "name": "test", "kind": "container", "state": "failed"},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "execution.logs.read":
				body, _ := json.Marshal(api.ExecutionLogsReadResult{
					Chunks: []api.LogChunk{{ActionID: "act-1", Stream: "stderr", Offset: 0, Sequence: 1, Data: []byte("panic: boom\n")}},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"logs", "exec-fail", "--stream", "stderr", "--follow"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit = %d; want %d", code, cli.ExitFailure)
		}
		if out.String() != "panic: boom\n" {
			t.Fatalf("stdout = %q; want crash log drained before exit", out.String())
		}
		if !strings.Contains(errOut.String(), "failed") {
			t.Fatalf("stderr = %q; want failure status diagnostic", errOut.String())
		}
	})

	t.Run("no started action refuses clearly", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			body, _ := json.Marshal(map[string]any{
				"execution_id": "exec-q",
				"state":        "queued",
				"actions": []map[string]any{
					{"action_id": "act-0", "name": "build", "kind": "container", "state": "pending"},
				},
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, _, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		if code := cli.Run(context.Background(), cfg, []string{"logs", "exec-q"}, ports); code != cli.ExitFailure {
			t.Fatalf("exit = %d; want %d", code, cli.ExitFailure)
		}
		if !strings.Contains(errOut.String(), "no action") {
			t.Fatalf("stderr = %q; want clear no-action message", errOut.String())
		}
	})

	t.Run("picks last completed container action when none is running and supports json output", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
			if idx == 0 {
				body, _ := json.Marshal(map[string]any{
					"execution_id": "exec-done",
					"state":        "succeeded",
					"actions": []map[string]any{
						{"action_id": "act-0", "name": "prep", "kind": "container", "state": "succeeded"},
						{"action_id": "act-1", "name": "verify", "kind": "container", "state": "succeeded"},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			}
			var req api.ExecutionLogsReadRequest
			_ = json.Unmarshal(inv.Request.Input, &req)
			if req.ActionID != "act-1" {
				t.Fatalf("selected action = %q; want last completed act-1", req.ActionID)
			}
			body, _ := json.Marshal(api.ExecutionLogsReadResult{
				Chunks:    []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("verified\n")}},
				Truncated: true,
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, out, errOut := testPorts(transport)
		if code := cli.Run(context.Background(), testConfig(), []string{"logs", "exec-done"}, ports); code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		var parsed map[string]any
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("unmarshal json logs: %v (%q)", err, out.String())
		}
		if parsed["action_id"] != "act-1" || parsed["data"] != "verified\n" || parsed["truncated"] != true {
			t.Fatalf("json logs = %v", parsed)
		}
	})
}

// TestRunInboxApproveAndReject pins Requirement 4.
func TestRunInboxApproveAndReject(t *testing.T) {
	t.Run("inbox table and KODAMA_PROJECT env fallback", func(t *testing.T) {
		t.Setenv("KODAMA_PROJECT", "env-proj")
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "task.inbox" {
				t.Fatalf("operation = %s; want task.inbox", inv.Operation)
			}
			var req api.TaskInboxRequest
			if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
				t.Fatalf("decode req: %v", err)
			}
			if req.ProjectID != "env-proj" {
				t.Fatalf("project_id = %q; want env-proj", req.ProjectID)
			}
			body, _ := json.Marshal(api.TaskInboxResult{
				Items: []api.InboxItem{
					{
						Kind:        "request",
						ExecutionID: "exec-1",
						ActionID:    "act-review",
						Assignment:  "role/reviewer",
						Subject: &api.Subject{Fields: []api.SubjectField{
							{Name: "operation", Kind: "string", Value: "git.ref.update"},
						}},
						Version: 1,
						At:      testNow(),
					},
					{
						Kind:             "waiting_task",
						TaskID:           "task-9",
						Assignment:       "role/engineer",
						QuestionID:       "q-1",
						Question:         "Which target cluster?",
						Version:          1,
						LifecycleVersion: 2,
						At:               testNow(),
					},
				},
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Project = ""
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"inbox"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		rendered := out.String()
		for _, want := range []string{"KIND", "TASK", "EXECUTION", "ACTION", "PROMPT/SUMMARY", "request", "exec-1", "act-review", "waiting_task", "task-9", "Which target cluster?"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("inbox table missing %q:\n%s", want, rendered)
			}
		}
	})

	t.Run("approve auto-discovers waiting request action and sends decision pass", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
			switch idx {
			case 0:
				if inv.Operation != "execution.inspect" {
					t.Fatalf("call 0 = %s; want execution.inspect", inv.Operation)
				}
				body, _ := json.Marshal(map[string]any{
					"execution_id": "exec-7",
					"state":        "running",
					"version":      4,
					"actions": []map[string]any{
						{"action_id": "act-build", "name": "build", "kind": "container", "state": "succeeded", "version": 2},
						{"action_id": "act-gate", "name": "gate", "kind": "request", "state": "running", "version": 3},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case 1:
				if inv.Operation != "execution.request.resolve" {
					t.Fatalf("call 1 = %s; want execution.request.resolve", inv.Operation)
				}
				if inv.Request.IdempotencyKey == "" {
					t.Fatal("missing auto-generated idempotency_key")
				}
				if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 4 {
					t.Fatalf("expected_version = %v; want 4", inv.Request.ExpectedVersion)
				}
				var req api.ExecutionRequestResolveRequest
				if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
					t.Fatalf("unmarshal resolve: %v", err)
				}
				if req.ExecutionID != "exec-7" || req.ActionID != "act-gate" || req.ExpectedActionVersion != 3 || req.Decision != "pass" {
					t.Fatalf("resolve req = %+v", req)
				}
				if req.Outputs["comment"] != "looks good" {
					t.Fatalf("outputs = %+v; want comment=looks good", req.Outputs)
				}
				body, _ := json.Marshal(api.ExecutionRequestResolveResult{ExecutionVersion: 5, ActionVersion: 4})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected call %d", idx)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"approve", "exec-7", "--comment", "looks good"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "approved execution exec-7 action act-gate") {
			t.Fatalf("stdout = %q", out.String())
		}
	})

	t.Run("reject requires reason and sends decision fail", func(t *testing.T) {
		ports, _, _ := testPorts(&scriptedTransport{})
		if code := cli.Run(context.Background(), testConfig(), []string{"reject", "exec-7"}, ports); code != cli.ExitUsage {
			t.Fatalf("reject without --reason exit = %d; want %d", code, cli.ExitUsage)
		}
		// Only --output doubles as the CLI format; its aliases always carry a key=value pair.
		for _, alias := range []string{"--response-output", "--action-output"} {
			p, _, aliasErr := testPorts(&scriptedTransport{})
			code := cli.Run(context.Background(), testConfig(), []string{"approve", "exec-7", alias, "json"}, p)
			if code != cli.ExitUsage || !strings.Contains(aliasErr.String(), "must be key=value") {
				t.Fatalf("approve %s json exit = %d; stderr=%q", alias, code, aliasErr.String())
			}
		}

		transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
			if idx == 0 {
				body, _ := json.Marshal(map[string]any{
					"execution_id": "exec-7",
					"state":        "running",
					"version":      2,
					"actions": []map[string]any{
						{"action_id": "act-gate", "name": "gate", "kind": "request", "state": "running", "version": 1},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			}
			var req api.ExecutionRequestResolveRequest
			if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
				t.Fatalf("unmarshal resolve: %v", err)
			}
			if req.Decision != "fail" || req.Outputs["reason"] != "regression detected" || inv.Request.IdempotencyKey == "" {
				t.Fatalf("reject req = %+v (key=%q)", req, inv.Request.IdempotencyKey)
			}
			body, _ := json.Marshal(api.ExecutionRequestResolveResult{ExecutionVersion: 3, ActionVersion: 2})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		p2, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"reject", "exec-7", "--reason", "regression detected"}, p2)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "rejected execution exec-7 action act-gate") {
			t.Fatalf("stdout = %q", out.String())
		}
	})

	t.Run("approve refuses 0 or >1 waiting request actions when --action omitted", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			actions []map[string]any
			wantErr string
		}{
			{
				name:    "zero waiting",
				actions: []map[string]any{{"action_id": "act-1", "kind": "container", "state": "running", "version": 1}},
				wantErr: "no waiting request action",
			},
			{
				name: "multiple waiting",
				actions: []map[string]any{
					{"action_id": "act-1", "kind": "request", "state": "running", "version": 1},
					{"action_id": "act-2", "kind": "request", "state": "running", "version": 1},
				},
				wantErr: "2 waiting request actions",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
					body, _ := json.Marshal(map[string]any{"execution_id": "exec-x", "state": "running", "actions": tc.actions})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				}}
				p, _, errOut := testPorts(transport)
				if code := cli.Run(context.Background(), testConfig(), []string{"approve", "exec-x"}, p); code != cli.ExitFailure {
					t.Fatalf("exit = %d; want %d", code, cli.ExitFailure)
				}
				if !strings.Contains(errOut.String(), tc.wantErr) {
					t.Fatalf("stderr = %q; want %q", errOut.String(), tc.wantErr)
				}
			})
		}
	})
}

// TestRunTaskCommands pins Requirement 5 task shortcuts.
func TestRunTaskCommands(t *testing.T) {
	t.Run("task create and task run", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
			if inv.Request.IdempotencyKey == "" {
				t.Fatalf("call %d missing idempotency_key", idx)
			}
			if idx == 0 {
				if inv.Operation != "task.create" {
					t.Fatalf("op = %s; want task.create", inv.Operation)
				}
				var req api.TaskCreateRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.ProjectID != "proj-1" || req.Title != "Add metrics" || req.Description != "Prometheus" || req.PipelineID != "pipe-1" {
					t.Fatalf("create req = %+v", req)
				}
				body, _ := json.Marshal(api.TaskCreateResult{TaskID: "task-1", ContentVersion: 1, LifecycleVersion: 1})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			}
			if inv.Operation != "task.create_and_start" {
				t.Fatalf("op = %s; want task.create_and_start", inv.Operation)
			}
			var req api.TaskCreateAndStartRequest
			_ = json.Unmarshal(inv.Request.Input, &req)
			if req.ProjectID != "proj-1" || req.Title != "Deploy" || req.PipelineID != "pipe-deploy" {
				t.Fatalf("run req = %+v", req)
			}
			body, _ := json.Marshal(api.TaskCreateAndStartResult{TaskID: "task-2", ExecutionID: "exec-2", LifecycleVersion: 1})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}

		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		if code := cli.Run(context.Background(), cfg, []string{
			"task", "create", "--project", "proj-1", "--title", "Add metrics", "--description", "Prometheus", "--pipeline", "pipe-1",
		}, ports); code != cli.ExitSuccess {
			t.Fatalf("task create exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "task-1") {
			t.Fatalf("stdout = %q", out.String())
		}

		out.Reset()
		if code := cli.Run(context.Background(), cfg, []string{
			"task", "run", "--project", "proj-1", "--title", "Deploy", "--pipeline", "pipe-deploy",
		}, ports); code != cli.ExitSuccess {
			t.Fatalf("task run exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "task-2") || !strings.Contains(out.String(), "exec-2") {
			t.Fatalf("stdout = %q", out.String())
		}
	})

	t.Run("task show start reply cancel", func(t *testing.T) {
		readBody, _ := json.Marshal(api.TaskReadResult{
			TaskID:           "task-42",
			Title:            "Investigate latency",
			State:            "waiting",
			ContentVersion:   2,
			LifecycleVersion: 5,
			SelectedPipeline: "pipe-debug",
			Initiator:        "oidc-1234",
			Question: &api.TaskQuestion{
				QuestionID: "q-9",
				Body:       "Restart staging?",
				Assignment: "role/operator",
				Version:    3,
			},
		})
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "task.read":
				return api.Result{Operation: inv.Operation, Body: readBody}, nil
			case "task.start":
				if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 5 || inv.Request.IdempotencyKey == "" {
					t.Fatalf("task.start envelope = %+v", inv.Request)
				}
				body, _ := json.Marshal(api.TaskStartResult{ExecutionID: "exec-99", LifecycleVersion: 6})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "task.reply":
				if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 5 || inv.Request.IdempotencyKey == "" {
					t.Fatalf("task.reply envelope = %+v", inv.Request)
				}
				var req api.TaskReplyRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.TaskID != "task-42" || req.QuestionID != "q-9" || req.ExpectedQuestionVersion != 3 || req.Answer != "yes, proceed" {
					t.Fatalf("task.reply req = %+v", req)
				}
				body, _ := json.Marshal(api.TaskReplyResult{ExecutionID: "exec-100", LifecycleVersion: 6, QuestionVersion: 4})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "task.cancel":
				if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 5 || inv.Request.IdempotencyKey == "" {
					t.Fatalf("task.cancel envelope = %+v", inv.Request)
				}
				body, _ := json.Marshal(api.TaskCancelResult{LifecycleVersion: 6})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}

		cfg := testConfig()
		cfg.Output = cli.OutputTable
		for _, argv := range [][]string{
			{"task", "show", "task-42"},
			{"task", "start", "task-42"},
			{"task", "reply", "task-42", "--answer", "yes, proceed"},
			{"task", "cancel", "task-42"},
		} {
			ports, out, errOut := testPorts(transport)
			if code := cli.Run(context.Background(), cfg, argv, ports); code != cli.ExitSuccess {
				t.Fatalf("%v exit = %d; stderr=%q", argv, code, errOut.String())
			}
			if out.Len() == 0 {
				t.Fatalf("%v wrote empty stdout", argv)
			}
		}
	})

	t.Run("task list invokes registered task.list", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "task.list" {
				t.Fatalf("unexpected op %s; want task.list", inv.Operation)
			}
			var req api.TaskListRequest
			if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
				t.Fatalf("unmarshal task.list: %v", err)
			}
			if req.ProjectID != "shop" || req.Status != "open" {
				t.Fatalf("task.list req = %+v", req)
			}
			if req.Page != nil && req.Page.Cursor == "page-2" {
				body, _ := json.Marshal(api.TaskListResult{
					Tasks: []api.TaskSummaryView{{TaskID: "task-2", Title: "Second page", State: "open", ContentVersion: 1, LifecycleVersion: 1}},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			}
			body, _ := json.Marshal(api.TaskListResult{
				Tasks: []api.TaskSummaryView{
					{
						TaskID:           "task-1",
						Title:            "Ship v0.2",
						State:            "open",
						SelectedPipeline: "pipe-1",
						CurrentExecution: "exec-1",
						ContentVersion:   1,
						LifecycleVersion: 1,
					},
				},
				NextCursor: "page-2",
			})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		if code := cli.Run(context.Background(), cfg, []string{"task", "list", "--project", "shop", "--status", "open"}, ports); code != cli.ExitSuccess {
			t.Fatalf("task list exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "task-1") || !strings.Contains(out.String(), "exec-1") || !strings.Contains(out.String(), "Ship v0.2") {
			t.Fatalf("stdout = %q", out.String())
		}
		if !strings.Contains(out.String(), "task-2") {
			t.Fatalf("stdout = %q; want the second page", out.String())
		}
	})
}

// TestWhoamiAndMemberZeroHashMagic pins Requirement 5 whoami and member grant/revoke/list.
func TestWhoamiAndMemberZeroHashMagic(t *testing.T) {
	const issuer = "https://dex.kodama.example"
	const githubSubject = "github:octocat"
	sum := sha256.Sum256([]byte(issuer + "\x00" + githubSubject))
	wantOIDCID := "oidc-" + hex.EncodeToString(sum[:16])

	gotID, err := cli.DeriveOIDCPrincipalID(issuer, githubSubject)
	if err != nil || gotID != wantOIDCID {
		t.Fatalf("DeriveOIDCPrincipalID = %q, %v; want %q", gotID, err, wantOIDCID)
	}

	t.Run("whoami derives oidc principal offline from session JWT without server call", func(t *testing.T) {
		jwt := makeUnsignedTestJWT(t, issuer, githubSubject)
		store := fakeRecordStore{record: cli.SessionRecord{
			Version: cli.SessionFileVersion,
			Kind:    "session",
			Token:   jwt,
			Issuer:  issuer,
		}}
		ports, out, errOut := testPorts(nil)
		code := cli.RunWithDeps(context.Background(), cli.Config{Output: cli.OutputTable}, []string{"whoami", "--offline"}, ports, cli.CommandDeps{
			Login: cli.LoginRuntime{Store: store},
		})
		if code != cli.ExitSuccess {
			t.Fatalf("whoami exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), wantOIDCID) {
			t.Fatalf("whoami stdout = %q; want %q", out.String(), wantOIDCID)
		}
		if strings.Contains(out.String(), jwt) {
			t.Fatal("whoami leaked the raw JWT")
		}

		// Without --offline a failed server call is reported, never masked by the local claims.
		ports, out, errOut = testPorts(&scriptedTransport{respond: func(context.Context, int, api.Invocation) (api.Result, error) {
			return api.Result{}, fault.New(fault.Unauthenticated, "token rejected")
		}})
		code = cli.RunWithDeps(context.Background(), testConfig(), []string{"whoami"}, ports, cli.CommandDeps{
			Login: cli.LoginRuntime{Store: store},
		})
		if code != cli.ExitFailure || out.Len() != 0 || !strings.Contains(errOut.String(), "token rejected") {
			t.Fatalf("whoami with a failing server: exit = %d; stdout=%q stderr=%q", code, out.String(), errOut.String())
		}
	})

	t.Run("member grant with --github discovers issuer, prints derived oidc ID, and grants roles", func(t *testing.T) {
		oidc := &staticDiscoveryOIDC{doc: api.DiscoveryDocument{Issuer: issuer, APIURL: "https://kodama.example.com"}}
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "membership.grant" {
				t.Fatalf("op = %s; want membership.grant", inv.Operation)
			}
			var req api.MembershipGrantRequest
			if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
				t.Fatalf("unmarshal grant: %v", err)
			}
			if req.ProjectID != "shop" || req.PrincipalID != wantOIDCID {
				t.Fatalf("grant req = %+v; want principal %s", req, wantOIDCID)
			}
			if len(req.Roles) != 2 || req.Roles[0] != "engineer" || req.Roles[1] != "reviewer" {
				t.Fatalf("roles = %v", req.Roles)
			}
			if inv.Request.IdempotencyKey == "" {
				t.Fatal("missing idempotency_key")
			}
			body, _ := json.Marshal(api.MembershipGrantResult{Version: 1})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.RunWithDeps(context.Background(), cfg, []string{
			"member", "grant", "--github", "OctoCat", "--project", "shop", "--role", "engineer,reviewer",
		}, ports, cli.CommandDeps{Login: cli.LoginRuntime{OIDC: oidc}})
		if code != cli.ExitSuccess {
			t.Fatalf("member grant exit = %d; stderr=%q", code, errOut.String())
		}
		if oidc.calls != 1 {
			t.Fatalf("discovery calls = %d; want 1", oidc.calls)
		}
		if !strings.Contains(out.String(), wantOIDCID) {
			t.Fatalf("stdout = %q; want derived %s", out.String(), wantOIDCID)
		}
	})

	t.Run("member grant with explicit oidc- principal skips discovery", func(t *testing.T) {
		oidc := &staticDiscoveryOIDC{doc: api.DiscoveryDocument{Issuer: issuer}}
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			var req api.MembershipGrantRequest
			_ = json.Unmarshal(inv.Request.Input, &req)
			if req.PrincipalID != "oidc-direct123456" {
				t.Fatalf("principal = %q; want oidc-direct123456", req.PrincipalID)
			}
			body, _ := json.Marshal(api.MembershipGrantResult{Version: 1})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, _, errOut := testPorts(transport)
		code := cli.RunWithDeps(context.Background(), testConfig(), []string{
			"member", "grant", "oidc-direct123456", "--project", "shop", "--role", "admin",
		}, ports, cli.CommandDeps{Login: cli.LoginRuntime{OIDC: oidc}})
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if oidc.calls != 0 {
			t.Fatalf("discovery calls = %d; want 0 for direct oidc- ID", oidc.calls)
		}
	})

	t.Run("member revoke without --expected-version looks up current version via membership.list", func(t *testing.T) {
		oidc := &staticDiscoveryOIDC{doc: api.DiscoveryDocument{Issuer: issuer, APIURL: "https://kodama.example.com"}}
		transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
			if idx == 0 {
				if inv.Operation != "membership.list" {
					t.Fatalf("call 0 = %s; want membership.list", inv.Operation)
				}
				body, _ := json.Marshal(api.MembershipListResult{
					Memberships: []api.MembershipView{
						{PrincipalID: "oidc-other", Roles: []string{"viewer"}, Version: 2},
						{PrincipalID: wantOIDCID, Roles: []string{"engineer"}, Version: 7},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			}
			if inv.Operation != "membership.revoke" {
				t.Fatalf("call 1 = %s; want membership.revoke", inv.Operation)
			}
			if inv.Request.ExpectedVersion == nil || *inv.Request.ExpectedVersion != 7 || inv.Request.IdempotencyKey == "" {
				t.Fatalf("revoke envelope = %+v; want version 7", inv.Request)
			}
			var req api.MembershipRevokeRequest
			_ = json.Unmarshal(inv.Request.Input, &req)
			if req.PrincipalID != wantOIDCID || req.ProjectID != "shop" {
				t.Fatalf("revoke req = %+v", req)
			}
			body, _ := json.Marshal(api.MembershipRevokeResult{Version: 8})
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.RunWithDeps(context.Background(), cfg, []string{
			"member", "revoke", "--subject", githubSubject, "--project", "shop",
		}, ports, cli.CommandDeps{Login: cli.LoginRuntime{OIDC: oidc}})
		if code != cli.ExitSuccess {
			t.Fatalf("revoke exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), wantOIDCID) {
			t.Fatalf("stdout = %q; want %s", out.String(), wantOIDCID)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

// TestMCPStdioAndInstall pins Requirement 6: MCP stdio bridge + mcp install.
func TestMCPStdioAndInstall(t *testing.T) {
	t.Run("mcp stdio forwards JSON-RPC with rotating Bearer token and ignores 202 notifications", func(t *testing.T) {
		var mu sync.Mutex
		var authHeaders []string
		var paths []string

		rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()
			mu.Lock()
			authHeaders = append(authHeaders, req.Header.Get("Authorization"))
			paths = append(paths, req.URL.Path)
			callNum := len(authHeaders)
			mu.Unlock()

			if strings.Contains(string(body), `"notifications/initialized"`) {
				return &http.Response{
					StatusCode: http.StatusAccepted,
					Header:     http.Header{},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}
			// Return multi-line JSON to verify single-line compaction on stdout.
			respJSON := "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": " +
				[]string{"1", "1", "2"}[callNum-1] + ",\n  \"result\": {\"ok\": true}\n}"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(respJSON)),
			}, nil
		})

		source := &scriptedSource{credentials: []client.Credential{
			{Kind: client.KindSession, Token: "tok-initial", ExpiresAt: testNow().Add(time.Hour)},
			{Kind: client.KindSession, Token: "tok-initial", ExpiresAt: testNow().Add(time.Hour)},
			{Kind: client.KindSession, Token: "tok-refreshed", ExpiresAt: testNow().Add(2 * time.Hour)},
		}}
		stdin := strings.NewReader(strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			"",
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		}, "\n") + "\n")

		ports, out, errOut := testPorts(nil)
		ports.In = stdin
		ports.Credential = source

		code := cli.RunWithDeps(context.Background(), testConfig(), []string{"mcp", "stdio"}, ports, cli.CommandDeps{HTTP: rt})
		if code != cli.ExitSuccess {
			t.Fatalf("mcp stdio exit = %d; stderr=%q", code, errOut.String())
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("stdout lines = %d (%q); want exactly 2 non-notification responses", len(lines), out.String())
		}
		if lines[0] != `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` ||
			lines[1] != `{"jsonrpc":"2.0","id":2,"result":{"ok":true}}` {
			t.Fatalf("stdout lines = %v", lines)
		}
		if len(authHeaders) != 3 || authHeaders[0] != "Bearer tok-initial" || authHeaders[2] != "Bearer tok-refreshed" {
			t.Fatalf("authHeaders = %v; want initial then refreshed token", authHeaders)
		}
		for _, p := range paths {
			if p != "/mcp" {
				t.Fatalf("path = %q; want /mcp", p)
			}
		}
	})

	t.Run("mcp stdio answers the pending id on a transport failure and keeps serving", func(t *testing.T) {
		calls := 0
		rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			switch calls {
			case 1:
				return nil, io.ErrUnexpectedEOF
			case 2:
				return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			default:
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":3,"result":{}}`))}, nil
			}
		})
		ports, out, errOut := testPorts(nil)
		ports.In = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" +
			`{"jsonrpc":"2.0","id":"two","method":"tools/list"}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"tools/list"}` + "\n")
		ports.Credential = &scriptedSource{credentials: []client.Credential{humanCredential()}}

		code := cli.RunWithDeps(context.Background(), testConfig(), []string{"mcp", "stdio"}, ports, cli.CommandDeps{HTTP: rt})
		if code != cli.ExitSuccess {
			t.Fatalf("mcp stdio exit = %d; stderr=%q", code, errOut.String())
		}
		want := `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"MCP endpoint is unavailable"}}` + "\n" +
			`{"jsonrpc":"2.0","id":"two","error":{"code":-32603,"message":"MCP endpoint returned HTTP 502"}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"result":{}}` + "\n"
		if out.String() != want {
			t.Fatalf("stdout =\n%s\nwant:\n%s", out.String(), want)
		}
		if !strings.Contains(errOut.String(), "MCP endpoint is unavailable") {
			t.Fatalf("stderr = %q; want the transport failure", errOut.String())
		}
	})

	t.Run("mcp install --print and isolated file merge for claude, cursor, and codex", func(t *testing.T) {
		cfg := cli.Config{
			ServerURL: "https://kodama.example.com",
			CAFile:    "/etc/kodama/ca.pem",
			Output:    cli.OutputTable,
		}

		// Print mode for codex (TOML)
		ports, out, errOut := testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cfg, []string{"mcp", "install", "codex", "--name", "kodama-prod", "--print"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("codex --print exit = %d; stderr=%q", code, errOut.String())
		}
		wantTOML := "[mcp_servers.kodama-prod]\ncommand = \"kodactl\"\nargs = [\"--url\", \"https://kodama.example.com\", \"--ca-file\", \"/etc/kodama/ca.pem\", \"mcp\", \"stdio\"]\n"
		if out.String() != wantTOML {
			t.Fatalf("codex --print =\n%s\nwant:\n%s", out.String(), wantTOML)
		}

		// TOML has no \x or \a escape, and a relative --ca-file is written absolute.
		relCfg := cfg
		relCfg.CAFile = "ca.pem"
		absCA, _ := filepath.Abs("ca.pem")
		ports, out, errOut = testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), relCfg, []string{"mcp", "install", "codex", "--name", "a\x07\x7fb", "--print"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("codex --print exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.HasPrefix(out.String(), "[mcp_servers.\"a\\u0007\\u007Fb\"]\n") || !strings.Contains(out.String(), `"--ca-file", "`+absCA+`"`) {
			t.Fatalf("codex --print =\n%s", out.String())
		}

		for _, target := range []string{"cursor", "codex"} {
			ports, _, _ = testPorts(nil)
			if code := cli.RunWithDeps(context.Background(), cfg, []string{"mcp", "install", target, "--local"}, ports, cli.CommandDeps{}); code != cli.ExitUsage {
				t.Fatalf("mcp install %s --local exit = %d; want %d", target, code, cli.ExitUsage)
			}
		}

		// Isolated write & merge for claude, cursor, codex via fakeFS + injected HomeDir
		fakeHome := "/fake/home/dev"
		fs := newFakeFS()
		fs.dirs["/fake"] = 0o700
		fs.dirs["/fake/home"] = 0o700
		fs.dirs[fakeHome] = 0o700
		fs.files[fakeHome+"/.claude.json"] = []byte(`{"numStartups":3,"mcpServers":{"existing":{"command":"other"}}}`)

		deps := cli.CommandDeps{
			HomeDir: func() (string, error) { return fakeHome, nil },
		}

		for _, target := range []string{"claude", "cursor", "codex"} {
			var stdout, stderr bytes.Buffer
			p := cli.Ports{Files: fs, Out: &stdout, Err: &stderr}
			if code := cli.RunWithDeps(context.Background(), cfg, []string{"mcp", "install", target}, p, deps); code != cli.ExitSuccess {
				t.Fatalf("mcp install %s exit = %d; stderr=%q", target, code, stderr.String())
			}
		}

		// Verify ~/.claude.json preserved numStartups & existing server while adding kodama
		claudeBytes, err := fs.ReadFile(fakeHome + "/.claude.json")
		if err != nil {
			t.Fatalf("read .claude.json: %v", err)
		}
		var claudeDoc map[string]any
		if err := json.Unmarshal(claudeBytes, &claudeDoc); err != nil {
			t.Fatalf("unmarshal .claude.json: %v", err)
		}
		if claudeDoc["numStartups"] != float64(3) {
			t.Fatalf("numStartups = %v; want preserved 3", claudeDoc["numStartups"])
		}
		servers := claudeDoc["mcpServers"].(map[string]any)
		if servers["existing"] == nil || servers["kodama"] == nil {
			t.Fatalf("mcpServers = %v; want both existing and kodama", servers)
		}
		kodamaEntry := servers["kodama"].(map[string]any)
		if kodamaEntry["command"] != "kodactl" {
			t.Fatalf("command = %v", kodamaEntry["command"])
		}

		// Verify ~/.cursor/mcp.json and --config-file override
		if _, err := fs.ReadFile(fakeHome + "/.cursor/mcp.json"); err != nil {
			t.Fatalf("read .cursor/mcp.json: %v", err)
		}
		codexBytes, err := fs.ReadFile(fakeHome + "/.codex/config.toml")
		if err != nil || !strings.Contains(string(codexBytes), "[mcp_servers.kodama]") {
			t.Fatalf("read .codex/config.toml = %q, %v", string(codexBytes), err)
		}

		var customOut, customErr bytes.Buffer
		if code := cli.RunWithDeps(context.Background(), cfg, []string{
			"mcp", "install", "claude", "--config-file", "/fake/home/dev/custom-mcp.json",
		}, cli.Ports{Files: fs, Out: &customOut, Err: &customErr}, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("--config-file exit = %d; stderr=%q", code, customErr.String())
		}
		if _, err := fs.ReadFile("/fake/home/dev/custom-mcp.json"); err != nil {
			t.Fatalf("custom config-file not written: %v", err)
		}

		// Verify Codex TOML in-place replacement preserves surrounding sections.
		fs.files[fakeHome+"/.codex/config.toml"] = []byte("model = \"gpt-5\"\n\n[mcp_servers.kodama] # managed\ncommand = \"old\"\nargs = []\n\n[mcp_servers.kodama.env]\nSTALE = \"1\"\n\n[mcp_servers.other] # keep\ncommand = \"keep\"\n")
		if code := cli.RunWithDeps(context.Background(), cfg, []string{"mcp", "install", "codex"}, cli.Ports{
			Files: fs, Out: &customOut, Err: &customErr,
		}, deps); code != cli.ExitSuccess {
			t.Fatalf("codex update exit = %d; stderr=%q", code, customErr.String())
		}
		updatedTOMLBytes, _ := fs.ReadFile(fakeHome + "/.codex/config.toml")
		updatedTOML := string(updatedTOMLBytes)
		if !strings.Contains(updatedTOML, `model = "gpt-5"`) ||
			!strings.Contains(updatedTOML, "[mcp_servers.other] # keep\ncommand = \"keep\"\n") ||
			strings.Contains(updatedTOML, `command = "old"`) ||
			strings.Contains(updatedTOML, "STALE") ||
			strings.Count(updatedTOML, `[mcp_servers.kodama`) != 1 {
			t.Fatalf("unexpected merged TOML:\n%s", updatedTOML)
		}
	})

	t.Run("reject retries without undeclared reason output and member list renders table", func(t *testing.T) {
		resolveAttempts := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				body, _ := json.Marshal(map[string]any{
					"execution_id": "exec-9",
					"state":        "running",
					"version":      1,
					"actions":      []map[string]any{{"action_id": "act-r", "kind": "request", "state": "running", "version": 1}},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "execution.request.resolve":
				resolveAttempts++
				var req api.ExecutionRequestResolveRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if resolveAttempts == 1 {
					if req.Outputs["reason"] != "not ready" {
						t.Fatalf("attempt 1 outputs = %v; want reason", req.Outputs)
					}
					return api.Result{}, fault.New(fault.InvalidInput, `response key "reason" is not declared`)
				}
				if len(req.Outputs) != 0 || req.Decision != "fail" {
					t.Fatalf("attempt 2 req = %+v; want empty outputs and decision fail", req)
				}
				body, _ := json.Marshal(api.ExecutionRequestResolveResult{ExecutionVersion: 2, ActionVersion: 2})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "membership.list":
				body, _ := json.Marshal(api.MembershipListResult{
					Memberships: []api.MembershipView{
						{PrincipalID: "oidc-1111", Roles: []string{"admin", "reviewer"}, Version: 3},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		if code := cli.Run(context.Background(), cfg, []string{"reject", "exec-9", "--reason", "not ready"}, ports); code != cli.ExitSuccess {
			t.Fatalf("reject fallback exit = %d; stderr=%q", code, errOut.String())
		}
		if resolveAttempts != 2 {
			t.Fatalf("resolveAttempts = %d; want 2", resolveAttempts)
		}

		out.Reset()
		if code := cli.Run(context.Background(), cfg, []string{"member", "list", "--project", "shop"}, ports); code != cli.ExitSuccess {
			t.Fatalf("member list exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "PRINCIPAL") || !strings.Contains(out.String(), "oidc-1111") || !strings.Contains(out.String(), "admin,reviewer") {
			t.Fatalf("member list stdout = %q", out.String())
		}
	})
}

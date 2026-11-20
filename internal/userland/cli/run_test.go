package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/cli"
)

// TestRunCallJSONOutput pins served-operation dispatch, the exact JSON body
// output and the recorded invocation.
func TestRunCallJSONOutput(t *testing.T) {
	body := json.RawMessage(`{"id":"task-1","status":"open"}`)
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: body}, nil
	}}
	ports, out, errOut := testPorts(transport)
	code := cli.Run(context.Background(), testConfig(), []string{
		"call", "task.read", `--input={"task_id":"task-1"}`,
	}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	if out.String() != string(body)+"\n" {
		t.Fatalf("stdout = %q; want the result body plus newline", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", errOut.String())
	}
	recorded := transport.recorded()
	if len(recorded) != 1 {
		t.Fatalf("invocations = %d; want 1", len(recorded))
	}
	if recorded[0].Operation != "task.read" {
		t.Fatalf("operation = %q", recorded[0].Operation)
	}
	if string(recorded[0].Request.Input) != `{"task_id":"task-1"}` {
		t.Fatalf("input = %s", recorded[0].Request.Input)
	}
	if recorded[0].Request.ExpectedVersion != nil {
		t.Fatalf("expected version = %v; want none", recorded[0].Request.ExpectedVersion)
	}
}

// TestRunCallTableOutput pins the deterministic table format.
func TestRunCallTableOutput(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"id":"task-1","status":"open"}`)}, nil
	}}
	ports, out, errOut := testPorts(transport)
	config := testConfig()
	config.Output = cli.OutputTable
	code := cli.Run(context.Background(), config, []string{"call", "task.read", `--input={"task_id":"task-1"}`}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	want := "operation\ttask.read\nid\t\"task-1\"\nstatus\t\"open\"\n"
	if out.String() != want {
		t.Fatalf("stdout = %q; want %q", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", errOut.String())
	}
}

// TestRunCallPassesVersionAndKey pins that caller-supplied versions and
// idempotency keys reach the transport unchanged.
func TestRunCallPassesVersionAndKey(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{}`)}, nil
	}}
	ports, _, errOut := testPorts(transport)
	code := cli.Run(context.Background(), testConfig(), []string{
		"call", "pipeline.apply",
		`--input={"project_id":"shop","name":"build","yaml":"bmFtZTogYnVpbGQK"}`,
		"--expected-version=7",
		"--idempotency-key=caller-key-1",
	}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	recorded := transport.recorded()
	if len(recorded) != 1 {
		t.Fatalf("invocations = %d; want 1", len(recorded))
	}
	request := recorded[0].Request
	if request.ExpectedVersion == nil || *request.ExpectedVersion != 7 {
		t.Fatalf("expected version = %v; want 7", request.ExpectedVersion)
	}
	if request.IdempotencyKey != "caller-key-1" {
		t.Fatalf("idempotency key = %q", request.IdempotencyKey)
	}
}

// TestRunCallFailure pins the failure exit code, stdout/stderr separation and
// public diagnostics.
func TestRunCallFailure(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, _ api.Invocation) (api.Result, error) {
		return api.Result{}, fault.New(fault.Forbidden, "caller is not allowed")
	}}
	ports, out, errOut := testPorts(transport)
	code := cli.Run(context.Background(), testConfig(), []string{"call", "task.read", `--input={"task_id":"task-1"}`}, ports)
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q; want empty on failure", out.String())
	}
	if !strings.Contains(errOut.String(), "forbidden") || !strings.Contains(errOut.String(), "caller is not allowed") {
		t.Fatalf("stderr = %q; want the public code and message", errOut.String())
	}
	if strings.Contains(errOut.String(), "kodex.") {
		t.Fatalf("stderr leaked a credential-looking token: %s", errOut.String())
	}
}

// TestRunUsageErrors pins the usage exit code and that no request is sent.
func TestRunUsageErrors(t *testing.T) {
	directory := "/repo/.kodema"
	cases := []struct {
		name   string
		args   []string
		config func() cli.Config
	}{
		{name: "no command", args: nil},
		{name: "unknown command", args: []string{"frobnicate"}},
		{name: "call without operation", args: []string{"call"}},
		{name: "call unknown operation", args: []string{"call", "bogus.operation", "--input={}"}},
		{name: "call malformed input", args: []string{"call", "task.read", "--input=not json"}},
		{name: "call unknown flag", args: []string{"call", "task.read", "--bogus=1"}},
		{name: "call zero expected version", args: []string{"call", "task.read", "--expected-version=0"}},
		{name: "call invalid idempotency key", args: []string{"call", "task.read", "--idempotency-key=bad key"}},
		{name: "inspect without execution", args: []string{"inspect"}},
		{name: "watch without execution", args: []string{"watch"}},
		{name: "run without file", args: []string{"run"}},
		{name: "run unknown flag", args: []string{"run", "/repo/.kodema/build.yaml", "--title", "x"}},
		{name: "pipeline apply without project", args: []string{"pipeline", "apply", directory}, config: func() cli.Config {
			config := testConfig()
			config.Project = ""
			return config
		}},
		{name: "helper invalid protocol", args: []string{"git", "credential", "bogus", "https://git.example.com/shop/repo.git"}},
		{name: "invalid config", args: []string{"call", "task.read"}, config: func() cli.Config {
			config := testConfig()
			config.ServerURL = "http://kodema.example.com"
			return config
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &scriptedTransport{}
			ports, out, errOut := testPorts(transport)
			filesystem := newFakeFS()
			filesystem.entries[directory] = []cli.DirEntry{{Name: "build.yaml"}}
			filesystem.files[directory+"/build.yaml"] = []byte("name: build\n")
			ports.Files = filesystem
			config := testConfig()
			if testCase.config != nil {
				config = testCase.config()
			}
			code := cli.Run(context.Background(), config, testCase.args, ports)
			if code != cli.ExitUsage {
				t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitUsage, errOut.String())
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q; want empty", out.String())
			}
			if errOut.Len() == 0 {
				t.Fatal("stderr is empty; want a usage diagnostic")
			}
			if got := len(transport.recorded()); got != 0 {
				t.Fatalf("invocations = %d; want 0", got)
			}
		})
	}
}

// TestRunInspect pins the inspect command input and output.
func TestRunInspect(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"id":"exec-1","status":"running"}`)}, nil
	}}
	ports, out, errOut := testPorts(transport)
	code := cli.Run(context.Background(), testConfig(), []string{"inspect", "exec-1"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	recorded := transport.recorded()
	if len(recorded) != 1 {
		t.Fatalf("invocations = %d; want 1", len(recorded))
	}
	if recorded[0].Operation != "execution.inspect" {
		t.Fatalf("operation = %q; want execution.inspect", recorded[0].Operation)
	}
	if string(recorded[0].Request.Input) != `{"execution_id":"exec-1"}` {
		t.Fatalf("input = %s", recorded[0].Request.Input)
	}
	if !strings.Contains(out.String(), `"status":"running"`) {
		t.Fatalf("stdout = %q; want the inspected body", out.String())
	}
}

// TestRunWatch pins polling until a terminal status and the injected sleep.
func TestRunWatch(t *testing.T) {
	statuses := []string{"queued", "running", "succeeded"}
	var sleeps []time.Duration
	transport := &scriptedTransport{respond: func(_ context.Context, index int, invocation api.Invocation) (api.Result, error) {
		if index >= len(statuses) {
			return api.Result{}, fault.New(fault.InternalFailure, "watch kept polling after a terminal status")
		}
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"status":"` + statuses[index] + `"}`)}, nil
	}}
	ports, out, errOut := testPorts(transport)
	ports.Sleep = func(delay time.Duration) { sleeps = append(sleeps, delay) }
	code := cli.Run(context.Background(), testConfig(), []string{"watch", "exec-1"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	if got := len(transport.recorded()); got != 3 {
		t.Fatalf("invocations = %d; want 3", got)
	}
	if len(sleeps) != 2 {
		t.Fatalf("sleeps = %d; want 2", len(sleeps))
	}
	if lines := strings.Count(out.String(), "\n"); lines != 3 {
		t.Fatalf("stdout lines = %d; want 3 (stdout: %q)", lines, out.String())
	}
}

// TestRunWatchFailureStatus pins that a failed/cancelled execution is visible
// and exits failure.
func TestRunWatchFailureStatus(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
			return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"status":"` + status + `"}`)}, nil
		}}
		ports, out, errOut := testPorts(transport)
		code := cli.Run(context.Background(), testConfig(), []string{"watch", "exec-1"}, ports)
		if code != cli.ExitFailure {
			t.Errorf("%s: exit code = %d; want %d", status, code, cli.ExitFailure)
		}
		if !strings.Contains(out.String(), status) {
			t.Errorf("%s: stdout = %q; want the rendered status", status, out.String())
		}
		if errOut.Len() == 0 {
			t.Errorf("%s: stderr is empty; want a diagnostic", status)
		}
	}
}

// TestRunWatchCanceled pins prompt cancellation with a visible failure.
func TestRunWatchCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	transport := &scriptedTransport{respond: func(callCtx context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		calls++
		if calls > 5 {
			return api.Result{}, fault.New(fault.InternalFailure, "watch did not stop after cancellation")
		}
		if callCtx.Err() != nil {
			return api.Result{}, callCtx.Err()
		}
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"status":"running"}`)}, nil
	}}
	ports, out, errOut := testPorts(transport)
	ports.Sleep = func(time.Duration) { cancel() }
	code := cli.Run(ctx, testConfig(), []string{"watch", "exec-1"}, ports)
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
	}
	if !strings.Contains(errOut.String(), "watch canceled") {
		t.Fatalf("stderr = %q; want watch canceled", errOut.String())
	}
	if strings.Contains(out.String(), "succeeded") {
		t.Fatalf("stdout = %q; canceled watch rendered a success", out.String())
	}
	if calls > 5 {
		t.Fatalf("watch polled %d times; cancellation did not stop it", calls)
	}
}

// TestRunWatchUnknownStatus pins refusal of a status the CLI cannot classify.
func TestRunWatchUnknownStatus(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"status":"mystery"}`)}, nil
	}}
	ports, _, errOut := testPorts(transport)
	code := cli.Run(context.Background(), testConfig(), []string{"watch", "exec-1"}, ports)
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty; want a diagnostic for an unknown status")
	}
}

// TestRunLogin pins human-session persistence.
func TestRunLogin(t *testing.T) {
	source := &scriptedSource{credentials: []client.Credential{humanCredential()}}
	store := &fakeSessionStore{}
	ports, out, errOut := testPorts(&scriptedTransport{})
	ports.Credential = source
	ports.Session = store
	code := cli.Run(context.Background(), testConfig(), []string{"login"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	if store.savedCount() != 1 {
		t.Fatalf("saved credentials = %d; want 1", store.savedCount())
	}
	if store.saved[0].Token != humanCredential().Token {
		t.Fatalf("saved token differs from the human credential")
	}
	if !strings.Contains(out.String(), "logged in") {
		t.Fatalf("stdout = %q; want a login confirmation", out.String())
	}
	for _, buffer := range []*bytes.Buffer{out, errOut} {
		if strings.Contains(buffer.String(), humanCredential().Token) {
			t.Fatalf("login output leaked the token: %q", buffer.String())
		}
	}
}

// TestRunLoginRefusals pins login source, kind, expiry and persistence
// failures.
func TestRunLoginRefusals(t *testing.T) {
	expired := humanCredential()
	expired.ExpiresAt = testNow().Add(-time.Second)
	cases := []struct {
		name     string
		source   *scriptedSource
		store    *fakeSessionStore
		noSource bool
	}{
		{name: "source error", source: &scriptedSource{err: fault.New(fault.Unauthenticated, "no session")}, store: &fakeSessionStore{}},
		{name: "execution source", source: &scriptedSource{credentials: []client.Credential{{
			Kind: client.KindExecution, Token: "kodex.exec-1.opaque",
		}}}, store: &fakeSessionStore{}},
		{name: "service source", source: &scriptedSource{credentials: []client.Credential{{
			Kind: client.KindService, Token: "kodex.service-1.signature", ExpiresAt: testNow().Add(time.Hour),
		}}}, store: &fakeSessionStore{}},
		{name: "expired session", source: &scriptedSource{credentials: []client.Credential{expired}}, store: &fakeSessionStore{}},
		{name: "save failure", source: &scriptedSource{credentials: []client.Credential{humanCredential()}},
			store: &fakeSessionStore{saveErr: fault.New(fault.DependencyUnavailable, "disk unavailable")}},
		{name: "missing source", store: &fakeSessionStore{}, noSource: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ports, out, errOut := testPorts(&scriptedTransport{})
			if !testCase.noSource {
				ports.Credential = testCase.source
			}
			ports.Session = testCase.store
			code := cli.Run(context.Background(), testConfig(), []string{"login"}, ports)
			if code != cli.ExitFailure {
				t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q; want empty on login failure", out.String())
			}
			if errOut.Len() == 0 {
				t.Fatal("stderr is empty; want a login diagnostic")
			}
			if testCase.store != nil && testCase.store.savedCount() != 0 {
				t.Fatalf("saved credentials = %d; want 0", testCase.store.savedCount())
			}
			if strings.Contains(errOut.String(), "kodex.") {
				t.Fatalf("stderr leaked a credential-looking token: %s", errOut.String())
			}
		})
	}
}

// TestRunDoesNotRequireServerURLForHelper pins that the helper only needs
// trusted origins.
func TestRunDoesNotRequireServerURLForHelper(t *testing.T) {
	source := &scriptedSource{credentials: []client.Credential{humanCredential()}}
	ports, out, errOut := testPorts(&scriptedTransport{})
	ports.Credential = source
	config := cli.Config{TrustedOrigins: []string{"https://git.example.com/shop"}}
	code := cli.Run(context.Background(), config, []string{"git", "credential", "get", "https://git.example.com/shop/repo.git"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	want := "username=kodema\npassword=" + humanCredential().Token + "\n"
	if out.String() != want {
		t.Fatalf("stdout = %q; want %q", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", errOut.String())
	}
}

// TestRunInspectFailedActionStderr pins that `kodema inspect` in table mode
// surfaces the bounded stderr tail (or actionable `kodema logs` hint) for
// failed container actions, degrades gracefully on log fetch errors, skips
// non-container request actions, and keeps `--output json` pure JSON.
func TestRunInspectFailedActionStderr(t *testing.T) {
	failedContainerBody := `{
		"execution_id":"exec-42",
		"state":"failed",
		"version":4,
		"actions":[
			{"action_id":"act-1","index":0,"name":"build","kind":"container","state":"failed","version":2},
			{"action_id":"act-2","index":1,"name":"deploy","kind":"container","state":"cancelled","version":1}
		],
		"failure":{"kind":"runtime","reason":"workload_exited","detail":"workload exited with status 1"},
		"cleanup":{"state":"complete"}
	}`

	t.Run("failed container surfaces bounded stderr tail and header", func(t *testing.T) {
		var logCalls int
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				return api.Result{Operation: inv.Operation, Body: json.RawMessage(failedContainerBody)}, nil
			case "execution.logs.read":
				logCalls++
				var req api.ExecutionLogsReadRequest
				if err := json.Unmarshal(inv.Request.Input, &req); err != nil {
					t.Fatalf("unmarshal logs request: %v", err)
				}
				if req.ExecutionID != "exec-42" || req.ActionID != "act-1" || req.Stream != "stderr" {
					t.Fatalf("logs request = %+v; want exec-42/act-1/stderr", req)
				}
				body, _ := json.Marshal(api.ExecutionLogsReadResult{
					Chunks: []api.LogChunk{{
						ActionID: "act-1",
						Stream:   "stderr",
						Offset:   0,
						Sequence: 1,
						Data:     []byte(numberedLines(80)),
					}},
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
		code := cli.Run(context.Background(), cfg, []string{"inspect", "exec-42"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
		}
		if logCalls != 1 {
			t.Fatalf("logCalls = %d; want 1", logCalls)
		}
		if !strings.Contains(out.String(), "exec-42") || !strings.Contains(out.String(), "workload exited with status 1") {
			t.Fatalf("stdout = %q; want inspect table summary", out.String())
		}
		if strings.Contains(out.String(), "--- stderr") {
			t.Fatalf("stdout = %q; must not mix stderr tail into stdout", out.String())
		}
		for _, want := range []string{
			"--- stderr (action build) ---\n",
			"line 31\n",
			"line 80\n",
			"---\n",
			"stderr truncated; run 'kodema logs exec-42 --action build --stream stderr' for full logs\n",
		} {
			if !strings.Contains(errOut.String(), want) {
				t.Fatalf("stderr = %q; want %q", errOut.String(), want)
			}
		}
		for _, unwanted := range []string{"line 30\n", "line 1\n"} {
			if strings.Contains(errOut.String(), unwanted) {
				t.Fatalf("stderr = %q; did not want %q", errOut.String(), unwanted)
			}
		}
	})

	t.Run("failed container with empty stderr prints stdout and stderr logs hint", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				return api.Result{Operation: inv.Operation, Body: json.RawMessage(failedContainerBody)}, nil
			case "execution.logs.read":
				body, _ := json.Marshal(api.ExecutionLogsReadResult{})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"inspect", "exec-42"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
		}
		if !strings.Contains(out.String(), "exec-42") {
			t.Fatalf("stdout = %q; want inspect table", out.String())
		}
		for _, want := range []string{
			"no stderr captured for action build",
			"kodema logs exec-42 --action build --stream stdout",
			"kodema logs exec-42 --action build --stream stderr",
		} {
			if !strings.Contains(errOut.String(), want) {
				t.Fatalf("stderr = %q; want %q", errOut.String(), want)
			}
		}
	})

	t.Run("output json stays pure valid JSON with no log fetch", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "execution.inspect" {
				t.Fatalf("unexpected operation %s in JSON mode", inv.Operation)
			}
			return api.Result{Operation: inv.Operation, Body: json.RawMessage(failedContainerBody)}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"inspect", "exec-42", "--output", "json"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
		}
		if errOut.Len() != 0 {
			t.Fatalf("stderr = %q; want empty in JSON mode", errOut.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatalf("stdout is not valid JSON (%v): %q", err, out.String())
		}
		if decoded["execution_id"] != "exec-42" || decoded["state"] != "failed" {
			t.Fatalf("decoded JSON = %+v; want exec-42 failed", decoded)
		}
		if got := len(transport.recorded()); got != 1 {
			t.Fatalf("invocations = %d; want 1", got)
		}
	})

	t.Run("log fetch failure degrades gracefully and inspect table still succeeds", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				return api.Result{Operation: inv.Operation, Body: json.RawMessage(failedContainerBody)}, nil
			case "execution.logs.read":
				return api.Result{}, fault.New(fault.DependencyUnavailable, "log store unreachable")
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"inspect", "exec-42"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
		}
		if !strings.Contains(out.String(), "exec-42") || !strings.Contains(out.String(), "build") {
			t.Fatalf("stdout = %q; want complete inspect table", out.String())
		}
		for _, want := range []string{
			"stderr (action build) unavailable",
			"kodema logs exec-42 --action build --stream stderr",
		} {
			if !strings.Contains(errOut.String(), want) {
				t.Fatalf("stderr = %q; want %q", errOut.String(), want)
			}
		}
	})

	t.Run("failed request action does not fetch container stderr", func(t *testing.T) {
		failedRequestBody := `{
			"execution_id":"exec-req-1",
			"state":"failed",
			"version":3,
			"actions":[
				{"action_id":"act-req","index":0,"name":"approve","kind":"request","state":"failed","version":2,"decision":"fail"}
			],
			"failure":{"kind":"request","reason":"rejected","detail":"request rejected"},
			"cleanup":{"state":"not_needed"}
		}`
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "execution.inspect" {
				t.Fatalf("unexpected operation %s; must not fetch logs for request action", inv.Operation)
			}
			return api.Result{Operation: inv.Operation, Body: json.RawMessage(failedRequestBody)}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"inspect", "exec-req-1"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
		}
		if !strings.Contains(out.String(), "exec-req-1") || !strings.Contains(out.String(), "request rejected") {
			t.Fatalf("stdout = %q; want inspect table", out.String())
		}
		if errOut.Len() != 0 {
			t.Fatalf("stderr = %q; want empty for failed request action", errOut.String())
		}
	})
}

// TestRunWatchFailedActionStderr pins that `kodema watch` in table mode
// surfaces failed container stderr tails (waiting for cleanup settlement when
// pending), handles empty stderr and log fetch errors gracefully, skips failed
// request actions, and keeps `--output json` pure NDJSON.
func TestRunWatchFailedActionStderr(t *testing.T) {
	runningBody := `{"execution_id":"exec-42","state":"running","version":2,"actions":[{"action_id":"act-1","index":0,"name":"test","kind":"container","state":"running","version":1}],"cleanup":{"state":"not_needed"}}`
	failedPendingCleanupBody := `{"execution_id":"exec-42","state":"failed","version":3,"actions":[{"action_id":"act-1","index":0,"name":"test","kind":"container","state":"failed","version":2}],"failure":{"kind":"runtime","reason":"workload_exited","detail":"workload exited with status 1"},"cleanup":{"state":"pending"}}`
	failedCompleteBody := `{"execution_id":"exec-42","state":"failed","version":4,"actions":[{"action_id":"act-1","index":0,"name":"test","kind":"container","state":"failed","version":2}],"failure":{"kind":"runtime","reason":"workload_exited","detail":"workload exited with status 1"},"cleanup":{"state":"complete"}}`

	t.Run("transitioning to failed prints stderr tail before watch failed error", func(t *testing.T) {
		polls := []string{runningBody, failedPendingCleanupBody, failedCompleteBody}
		inspectIdx := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				body := polls[min(inspectIdx, len(polls)-1)]
				inspectIdx++
				return api.Result{Operation: inv.Operation, Body: json.RawMessage(body)}, nil
			case "execution.logs.read":
				body, _ := json.Marshal(api.ExecutionLogsReadResult{
					Chunks: []api.LogChunk{{
						ActionID: "act-1",
						Stream:   "stderr",
						Offset:   0,
						Sequence: 1,
						Data:     []byte("FAIL: assertion failed\nexit status 1\n"),
					}},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		sleeps := 0
		ports.Sleep = func(time.Duration) { sleeps++ }
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"watch", "exec-42"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
		}
		if sleeps != 2 {
			t.Fatalf("sleeps = %d; want 2 (running + pending cleanup settle)", sleeps)
		}
		if !strings.Contains(out.String(), "exec-42") || !strings.Contains(out.String(), "workload exited with status 1") {
			t.Fatalf("stdout = %q; want rendered watch table", out.String())
		}
		if strings.Contains(out.String(), "--- stderr") {
			t.Fatalf("stdout = %q; must not mix stderr tail into stdout", out.String())
		}
		stderrStr := errOut.String()
		for _, want := range []string{
			"--- stderr (action test) ---\n",
			"FAIL: assertion failed\nexit status 1\n",
			"---\n",
			"watch failed: execution status failed\n",
		} {
			if !strings.Contains(stderrStr, want) {
				t.Fatalf("stderr = %q; want %q", stderrStr, want)
			}
		}
		if strings.Index(stderrStr, "--- stderr (action test) ---") > strings.Index(stderrStr, "watch failed: execution status failed") {
			t.Fatalf("stderr printed watch failed before stderr tail:\n%s", stderrStr)
		}
	})

	t.Run("transitioning to failed with empty stderr prints stdout and stderr logs hint", func(t *testing.T) {
		polls := []string{runningBody, failedCompleteBody}
		inspectIdx := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				body := polls[min(inspectIdx, len(polls)-1)]
				inspectIdx++
				return api.Result{Operation: inv.Operation, Body: json.RawMessage(body)}, nil
			case "execution.logs.read":
				body, _ := json.Marshal(api.ExecutionLogsReadResult{})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, _, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"watch", "exec-42"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
		}
		for _, want := range []string{
			"no stderr captured for action test",
			"kodema logs exec-42 --action test --stream stdout",
			"kodema logs exec-42 --action test --stream stderr",
			"watch failed: execution status failed\n",
		} {
			if !strings.Contains(errOut.String(), want) {
				t.Fatalf("stderr = %q; want %q", errOut.String(), want)
			}
		}
	})

	t.Run("transitioning to failed with log fetch error degrades gracefully", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				return api.Result{Operation: inv.Operation, Body: json.RawMessage(failedCompleteBody)}, nil
			case "execution.logs.read":
				return api.Result{}, fault.New(fault.Forbidden, "logs denied")
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"watch", "exec-42"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
		}
		if !strings.Contains(out.String(), "exec-42") {
			t.Fatalf("stdout = %q; want watch table", out.String())
		}
		for _, want := range []string{
			"stderr (action test) unavailable",
			"kodema logs exec-42 --action test --stream stderr",
			"watch failed: execution status failed\n",
		} {
			if !strings.Contains(errOut.String(), want) {
				t.Fatalf("stderr = %q; want %q", errOut.String(), want)
			}
		}
	})

	t.Run("output json stays pure NDJSON with no log fetch", func(t *testing.T) {
		polls := []string{runningBody, failedCompleteBody}
		inspectIdx := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "execution.inspect" {
				t.Fatalf("unexpected operation %s in JSON mode", inv.Operation)
			}
			body := polls[min(inspectIdx, len(polls)-1)]
			inspectIdx++
			return api.Result{Operation: inv.Operation, Body: json.RawMessage(body)}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"watch", "exec-42", "--output", "json"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("stdout lines = %d; want 2 NDJSON lines:\n%s", len(lines), out.String())
		}
		for i, line := range lines {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatalf("stdout line %d is not valid JSON (%v): %q", i, err, line)
			}
		}
		if errOut.String() != "watch failed: execution status failed\n" {
			t.Fatalf("stderr = %q; want only watch failed line in JSON mode", errOut.String())
		}
	})

	t.Run("failed request action does not fetch container stderr", func(t *testing.T) {
		failedRequestBody := `{
			"execution_id":"exec-req-2",
			"state":"failed",
			"version":3,
			"actions":[{"action_id":"act-req","index":0,"name":"qa-gate","kind":"request","state":"failed","version":2,"decision":"fail"}],
			"failure":{"kind":"request","reason":"timeout","detail":"request timed out"},
			"cleanup":{"state":"not_needed"}
		}`
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "execution.inspect" {
				t.Fatalf("unexpected operation %s; must not fetch logs for request action", inv.Operation)
			}
			return api.Result{Operation: inv.Operation, Body: json.RawMessage(failedRequestBody)}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"watch", "exec-req-2"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
		}
		if !strings.Contains(out.String(), "exec-req-2") {
			t.Fatalf("stdout = %q; want watch table", out.String())
		}
		if errOut.String() != "watch failed: execution status failed\n" {
			t.Fatalf("stderr = %q; want only watch failed line for request action", errOut.String())
		}
	})
}

// TestRunErrorIsNotAProcessExit is a compile-time reminder that Run returns an
// exit code; the test process would terminate if Run called os.Exit.
func TestRunErrorIsNotAProcessExit(t *testing.T) {
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, _ api.Invocation) (api.Result, error) {
		return api.Result{}, errors.New("boom")
	}}
	ports, _, errOut := testPorts(transport)
	code := cli.Run(context.Background(), testConfig(), []string{"call", "task.read"}, ports)
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty; want a diagnostic")
	}
}

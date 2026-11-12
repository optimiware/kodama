package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/cli"
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
	directory := "/repo/.kodama"
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
		{name: "pipeline apply without project", args: []string{"pipeline", "apply", directory}, config: func() cli.Config {
			config := testConfig()
			config.Project = ""
			return config
		}},
		{name: "helper invalid protocol", args: []string{"helper", "bogus", "https://git.example.com/shop/repo.git"}},
		{name: "invalid config", args: []string{"call", "task.read"}, config: func() cli.Config {
			config := testConfig()
			config.ServerURL = "http://kodama.example.com"
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
	code := cli.Run(context.Background(), config, []string{"helper", "get", "https://git.example.com/shop/repo.git"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	want := "username=kodama\npassword=" + humanCredential().Token + "\n"
	if out.String() != want {
		t.Fatalf("stdout = %q; want %q", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", errOut.String())
	}
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

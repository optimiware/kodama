package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/cli"
)

// outputPushFixture is one container action's output directory and the
// execution the push reads.
type outputPushFixture struct {
	ports     cli.Ports
	files     *fakeFS
	transport *scriptedTransport
	deps      cli.CommandDeps
}

func newOutputPushFixture(t *testing.T, files map[string]string) (*outputPushFixture, func() string) {
	t.Helper()
	transport := &scriptedTransport{}
	transport.respond = func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		if invocation.Operation == "execution.inspect" {
			return api.Result{Operation: invocation.Operation, Body: json.RawMessage(
				`{"execution_id":"execution-1","state":"running","version":6,"current_action":1,"actions":[` +
					`{"action_id":"act-clone","index":0,"name":"clone","kind":"container","state":"succeeded","version":4},` +
					`{"action_id":"act-build","index":1,"name":"build","kind":"container","state":"running","version":2}]}`)}, nil
		}
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"execution_version":7,"action_version":3}`)}, nil
	}
	ports, out, errOut := testPorts(transport)
	filesystem := ports.Files.(*fakeFS)
	if files != nil {
		entries := make([]cli.DirEntry, 0, len(files))
		for name, content := range files {
			entries = append(entries, cli.DirEntry{Name: name})
			filesystem.files["/kodema/outputs/build/"+name] = []byte(content)
		}
		filesystem.entries["/kodema/outputs/build"] = entries
	}
	environment := map[string]string{"KODEMA_EXECUTION": "execution-1", "KODEMA_ACTION": "build"}
	fixture := &outputPushFixture{
		ports:     ports,
		files:     filesystem,
		transport: transport,
		deps:      cli.CommandDeps{Login: cli.LoginRuntime{Environment: func(name string) string { return environment[name] }}},
	}
	return fixture, func() string { return out.String() + errOut.String() }
}

func (fixture *outputPushFixture) push(args ...string) int {
	return cli.RunWithDeps(context.Background(), testConfig(), append([]string{"output", "push"}, args...), fixture.ports, fixture.deps)
}

// submissions returns the execution.action.outputs invocations of a push.
func (fixture *outputPushFixture) submissions() []api.Invocation {
	var submitted []api.Invocation
	for _, invocation := range fixture.transport.recorded() {
		if invocation.Operation == "execution.action.outputs" {
			submitted = append(submitted, invocation)
		}
	}
	return submitted
}

// TestOutputPushSubmitsTheFilesOfTheActionDirectory pins the file rules: the
// name is the key, exactly one trailing line feed is removed and an empty file
// is the empty string. The push names the running action by its ID and sends
// the execution version it observed.
func TestOutputPushSubmitsTheFilesOfTheActionDirectory(t *testing.T) {
	fixture, output := newOutputPushFixture(t, map[string]string{
		"candidate":     "8f4548c2926df0223a4cb3fd9e073172067ef5e3\n",
		"expected_base": "",
		"notes":         "two\nlines\n\n",
		"unterminated":  "value",
	})
	if code := fixture.push(); code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; output: %s", code, output())
	}
	submitted := fixture.submissions()
	if len(submitted) != 1 {
		t.Fatalf("submissions = %d; want exactly one request for all values", len(submitted))
	}
	var request api.ExecutionActionOutputsRequest
	if err := json.Unmarshal(submitted[0].Request.Input, &request); err != nil {
		t.Fatalf("submitted input: %v", err)
	}
	if request.ExecutionID != "execution-1" || request.ActionID != "act-build" {
		t.Fatalf("submitted to %q/%q; want the running action execution-1/act-build", request.ExecutionID, request.ActionID)
	}
	want := map[string]string{
		"candidate":     "8f4548c2926df0223a4cb3fd9e073172067ef5e3",
		"expected_base": "",
		"notes":         "two\nlines\n",
		"unterminated":  "value",
	}
	if len(request.Outputs) != len(want) {
		t.Fatalf("outputs = %v; want %v", request.Outputs, want)
	}
	for key, value := range want {
		if got, ok := request.Outputs[key]; !ok || got != value {
			t.Fatalf("output %q = %q (present=%t); want %q", key, got, ok, value)
		}
	}
	if submitted[0].Request.ExpectedVersion == nil || *submitted[0].Request.ExpectedVersion != 6 {
		t.Fatalf("expected version = %v; want the observed execution version 6", submitted[0].Request.ExpectedVersion)
	}
	if !strings.HasPrefix(submitted[0].Request.IdempotencyKey, "output-push:") {
		t.Fatalf("idempotency key = %q", submitted[0].Request.IdempotencyKey)
	}
	if !strings.Contains(output(), "submitted outputs candidate, expected_base, notes, unterminated") {
		t.Fatalf("output = %q; want the submitted keys in name order", output())
	}
}

// TestOutputPushWithoutFilesSendsNothing pins that an action that wrote no
// output, or declares none and so has no directory, makes no request at all.
func TestOutputPushWithoutFilesSendsNothing(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"an empty directory": {},
		"no directory":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			fixture, output := newOutputPushFixture(t, files)
			if code := fixture.push(); code != cli.ExitSuccess {
				t.Fatalf("exit code = %d; output: %s", code, output())
			}
			if calls := fixture.transport.recorded(); len(calls) != 0 {
				t.Fatalf("made %d request(s); want none", len(calls))
			}
			if output() != "" {
				t.Fatalf("output = %q; want none", output())
			}
		})
	}
}

// TestOutputPushRefusesBeforeAnyRequest pins the refusals the push decides
// itself: no request is made, and the message names the output.
func TestOutputPushRefusesBeforeAnyRequest(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		arrange func(*outputPushFixture)
		want    string
	}{
		{"a value above the bound", func(fixture *outputPushFixture) {
			fixture.files.files["/kodema/outputs/build/candidate"] = []byte(strings.Repeat("a", api.MaxSubjectValueBytes+1))
		}, `output "candidate" exceeds 4096 bytes`},
		{"too many files", func(fixture *outputPushFixture) {
			fixture.files.entries["/kodema/outputs/build"] = make([]cli.DirEntry, 9)
		}, `at most 8 are allowed`},
		{"invalid UTF-8", func(fixture *outputPushFixture) {
			fixture.files.files["/kodema/outputs/build/candidate"] = []byte{0xff, 0xfe}
		}, `output "candidate" is not valid UTF-8`},
		{"a subdirectory", func(fixture *outputPushFixture) {
			fixture.files.entries["/kodema/outputs/build"] = []cli.DirEntry{{Name: "nested", IsDir: true}}
		}, `output "nested" is a directory`},
		{"an unreadable directory", func(fixture *outputPushFixture) {
			fixture.files.dirErr["/kodema/outputs/build"] = errors.New("permission denied")
		}, `output directory "/kodema/outputs/build" is unreadable`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, output := newOutputPushFixture(t, map[string]string{"candidate": "value\n"})
			testCase.arrange(fixture)
			if code := fixture.push(); code != cli.ExitFailure {
				t.Fatalf("exit code = %d; want %d; output: %s", code, cli.ExitFailure, output())
			}
			if calls := fixture.transport.recorded(); len(calls) != 0 {
				t.Fatalf("made %d request(s) before refusing", len(calls))
			}
			if !strings.Contains(output(), testCase.want) {
				t.Fatalf("output = %q; want %q", output(), testCase.want)
			}
		})
	}
}

// TestOutputPushRetriesAVersionConflictWithTheSameKey pins the convergence
// rule: after a conflict the push reads the execution again and repeats the
// submission under the same idempotency key.
func TestOutputPushRetriesAVersionConflictWithTheSameKey(t *testing.T) {
	fixture, output := newOutputPushFixture(t, map[string]string{"candidate": "value\n"})
	inner := fixture.transport.respond
	versions := []uint64{6, 8}
	inspections, submissions := 0, 0
	fixture.transport.respond = func(ctx context.Context, index int, invocation api.Invocation) (api.Result, error) {
		if invocation.Operation == "execution.inspect" {
			version := versions[inspections]
			inspections++
			return api.Result{Operation: invocation.Operation, Body: json.RawMessage(
				`{"execution_id":"execution-1","state":"running","version":` + string(rune('0'+version)) +
					`,"actions":[{"action_id":"act-build","name":"build","kind":"container","state":"running","version":2}]}`)}, nil
		}
		submissions++
		if submissions == 1 {
			return api.Result{}, &api.Error{Code: api.CodeVersionConflict, Message: "the execution moved"}
		}
		return inner(ctx, index, invocation)
	}
	if code := fixture.push(); code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; output: %s", code, output())
	}
	submitted := fixture.submissions()
	if len(submitted) != 2 {
		t.Fatalf("submissions = %d; want the refused one and its retry", len(submitted))
	}
	if submitted[0].Request.IdempotencyKey != submitted[1].Request.IdempotencyKey {
		t.Fatalf("idempotency keys differ: %q then %q", submitted[0].Request.IdempotencyKey, submitted[1].Request.IdempotencyKey)
	}
	if *submitted[0].Request.ExpectedVersion != 6 || *submitted[1].Request.ExpectedVersion != 8 {
		t.Fatalf("expected versions = %d then %d; want each read's version, 6 then 8",
			*submitted[0].Request.ExpectedVersion, *submitted[1].Request.ExpectedVersion)
	}
}

// A caller key identifies the whole logical request, including the execution.
func TestOutputPushScopesTheKeyToTheExecution(t *testing.T) {
	fixture, output := newOutputPushFixture(t, map[string]string{"candidate": "same"})
	for _, execution := range []string{"execution-1", "execution-2"} {
		if code := fixture.push("--execution", execution); code != cli.ExitSuccess {
			t.Fatalf("push to %s: %s", execution, output())
		}
	}
	sent := fixture.submissions()
	if len(sent) != 2 || sent[0].Request.IdempotencyKey == sent[1].Request.IdempotencyKey {
		t.Fatal("identical action IDs and outputs in different executions must have different keys")
	}
}

// TestOutputPushFailsOnARefusedSubmission pins that every refusal other than a
// version conflict fails the push at once, in the server's words.
func TestOutputPushFailsOnARefusedSubmission(t *testing.T) {
	fixture, output := newOutputPushFixture(t, map[string]string{"undeclared": "value\n"})
	inner := fixture.transport.respond
	fixture.transport.respond = func(ctx context.Context, index int, invocation api.Invocation) (api.Result, error) {
		if invocation.Operation == "execution.action.outputs" {
			return api.Result{}, &api.Error{Code: api.CodeInvalidInput, Message: `output "undeclared" is not declared`}
		}
		return inner(ctx, index, invocation)
	}
	if code := fixture.push(); code != cli.ExitFailure {
		t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
	}
	if len(fixture.submissions()) != 1 {
		t.Fatalf("submissions = %d; want one, with no retry", len(fixture.submissions()))
	}
	for _, want := range []string{"outputs undeclared were not accepted", `output "undeclared" is not declared`} {
		if !strings.Contains(output(), want) {
			t.Fatalf("output = %q; want %q", output(), want)
		}
	}
}

// TestOutputPushUsage pins the usage refusals.
func TestOutputPushUsage(t *testing.T) {
	fixture, output := newOutputPushFixture(t, map[string]string{"candidate": "value\n"})
	fixture.deps = cli.CommandDeps{Login: cli.LoginRuntime{Environment: func(string) string { return "" }}}
	for _, args := range [][]string{
		{"output"},
		{"output", "pull"},
		{"output", "push"},
		{"output", "push", "extra"},
		{"output", "push", "--execution", "execution-1"},
	} {
		if code := cli.RunWithDeps(context.Background(), testConfig(), args, fixture.ports, fixture.deps); code != cli.ExitUsage {
			t.Fatalf("args %v exit code = %d; want %d; output: %s", args, code, cli.ExitUsage, output())
		}
	}
	if calls := fixture.transport.recorded(); len(calls) != 0 {
		t.Fatalf("a usage refusal made %d request(s)", len(calls))
	}
}

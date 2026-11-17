package cli_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/cli"
)

// pipelineInput is the decoded pipeline.apply input.
type pipelineInput struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	YAML      []byte `json:"yaml"`
}

// pipelineKeys maps pipeline name to the caller key used for it.
func pipelineKeys(t *testing.T, invocations []api.Invocation) map[string]string {
	t.Helper()
	keys := make(map[string]string, len(invocations))
	for _, invocation := range invocations {
		var input pipelineInput
		if err := json.Unmarshal(invocation.Request.Input, &input); err != nil {
			t.Fatalf("pipeline.apply input is not an object: %v", err)
		}
		if _, duplicate := keys[input.Name]; duplicate {
			t.Fatalf("pipeline %q was applied twice", input.Name)
		}
		keys[input.Name] = invocation.Request.IdempotencyKey
	}
	return keys
}

// TestRunPipelineApply pins deterministic enumeration, exact YAML bytes, the
// filename mapping and stable per-pipeline caller keys.
func TestRunPipelineApply(t *testing.T) {
	directory := "/repo/.kodema"
	filesystem := newFakeFS()
	filesystem.entries[directory] = []cli.DirEntry{
		{Name: "z.yaml"},
		{Name: "notes.txt"},
		{Name: "sub", IsDir: true},
		{Name: "a.yml"},
		{Name: ".hidden.yaml"},
		{Name: "b.yaml"},
	}
	contents := map[string]string{
		"a.yml":  "name: a\n# exact bytes\n",
		"b.yaml": "name: b\r\ninputs:\r\n  - key: value\r\n",
		"z.yaml": "name: z\n",
	}
	for name, content := range contents {
		filesystem.files[directory+"/"+name] = []byte(content)
	}

	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{}`)}, nil
	}}
	ports, out, errOut := testPorts(transport)
	ports.Files = filesystem
	code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", errOut.String())
	}
	recorded := transport.recorded()
	if len(recorded) != 3 {
		t.Fatalf("invocations = %d; want 3", len(recorded))
	}
	wantOrder := []string{"a", "b", "z"}
	for index, invocation := range recorded {
		if invocation.Operation != "pipeline.apply" {
			t.Errorf("invocation %d operation = %q", index, invocation.Operation)
		}
		var input pipelineInput
		if err := json.Unmarshal(invocation.Request.Input, &input); err != nil {
			t.Fatalf("invocation %d input: %v", index, err)
		}
		if input.Name != wantOrder[index] {
			t.Errorf("invocation %d name = %q; want %q", index, input.Name, wantOrder[index])
		}
		if input.ProjectID != "shop" {
			t.Errorf("invocation %d project_id = %q; want shop", index, input.ProjectID)
		}
		extension := ".yml"
		if wantOrder[index] != "a" {
			extension = ".yaml"
		}
		if string(input.YAML) != contents[wantOrder[index]+extension] {
			t.Errorf("invocation %d yaml = %q; want exact bytes %q", index, input.YAML, contents[wantOrder[index]+extension])
		}
		if invocation.Request.ExpectedVersion != nil {
			t.Errorf("invocation %d expected version = %v; want none", index, invocation.Request.ExpectedVersion)
		}
		if err := invocation.Validate(); err != nil {
			t.Errorf("invocation %d is invalid: %v", index, err)
		}
	}
	if !strings.Contains(out.String(), "applied a") || !strings.Contains(out.String(), "applied z") {
		t.Fatalf("stdout = %q; want applied lines", out.String())
	}
	firstKeys := pipelineKeys(t, recorded)
	if len(firstKeys) != 3 {
		t.Fatalf("keys = %v; want one per pipeline", firstKeys)
	}

	// Replaying the same directory sends the same per-pipeline keys.
	replay := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{}`)}, nil
	}}
	replayPorts, _, replayErr := testPorts(replay)
	replayPorts.Files = filesystem
	if code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory}, replayPorts); code != cli.ExitSuccess {
		t.Fatalf("replay exit code = %d (stderr: %s)", code, replayErr.String())
	}
	secondKeys := pipelineKeys(t, replay.recorded())
	for pipeline, key := range firstKeys {
		if secondKeys[pipeline] != key {
			t.Errorf("pipeline %q key changed across replay: %q vs %q", pipeline, key, secondKeys[pipeline])
		}
	}

	// One file applies only that pipeline and prints the id task run needs.
	single := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"pipeline_id":"pipeline-z1","version":1}`)}, nil
	}}
	singlePorts, singleOut, singleErr := testPorts(single)
	singlePorts.Files = filesystem
	if code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory + "/z.yaml"}, singlePorts); code != cli.ExitSuccess {
		t.Fatalf("single file exit code = %d (stderr: %s)", code, singleErr.String())
	}
	if keys := pipelineKeys(t, single.recorded()); len(keys) != 1 || keys["z"] != firstKeys["z"] {
		t.Fatalf("single file keys = %v; want only z with its directory key", keys)
	}
	if singleOut.String() != "applied z (pipeline_id=pipeline-z1)\n" {
		t.Fatalf("single file stdout = %q", singleOut.String())
	}
}

// TestRunPipelineApplyPartialFailure pins visible partial failure: every file
// is attempted in order and prior successes are not rolled back.
func TestRunPipelineApplyPartialFailure(t *testing.T) {
	directory := "/repo/.kodema"
	filesystem := newFakeFS()
	filesystem.entries[directory] = []cli.DirEntry{{Name: "a.yaml"}, {Name: "b.yaml"}, {Name: "z.yaml"}}
	for _, name := range []string{"a.yaml", "b.yaml", "z.yaml"} {
		filesystem.files[directory+"/"+name] = []byte("name: " + strings.TrimSuffix(name, ".yaml") + "\n")
	}
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		var input pipelineInput
		if err := json.Unmarshal(invocation.Request.Input, &input); err != nil {
			t.Fatalf("input: %v", err)
		}
		if input.Name == "b" {
			return api.Result{}, fault.New(fault.Forbidden, "pipeline denied")
		}
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{}`)}, nil
	}}
	ports, out, errOut := testPorts(transport)
	ports.Files = filesystem
	code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory}, ports)
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
	}
	if got := len(transport.recorded()); got != 3 {
		t.Fatalf("invocations = %d; want all 3 files attempted", got)
	}
	if !strings.Contains(out.String(), "applied a") || !strings.Contains(out.String(), "applied z") {
		t.Fatalf("stdout = %q; want the successful pipelines", out.String())
	}
	if !strings.Contains(errOut.String(), "failed b") || !strings.Contains(errOut.String(), "pipeline denied") {
		t.Fatalf("stderr = %q; want the visible pipeline failure", errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), "kodex.") {
		t.Fatalf("pipeline output leaked a credential-looking token")
	}
}

// TestRunPipelineApplyDuplicateMapping pins that a .yaml and .yml with the same
// base name are a visible mapping failure and are not sent.
func TestRunPipelineApplyDuplicateMapping(t *testing.T) {
	directory := "/repo/.kodema"
	filesystem := newFakeFS()
	filesystem.entries[directory] = []cli.DirEntry{{Name: "dup.yaml"}, {Name: "dup.yml"}, {Name: "ok.yaml"}}
	filesystem.files[directory+"/dup.yaml"] = []byte("name: dup-yaml\n")
	filesystem.files[directory+"/dup.yml"] = []byte("name: dup-yml\n")
	filesystem.files[directory+"/ok.yaml"] = []byte("name: ok\n")
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{}`)}, nil
	}}
	ports, _, errOut := testPorts(transport)
	ports.Files = filesystem
	code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory}, ports)
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
	}
	recorded := transport.recorded()
	if len(recorded) != 1 {
		t.Fatalf("invocations = %d; want only ok.yaml", len(recorded))
	}
	var input pipelineInput
	if err := json.Unmarshal(recorded[0].Request.Input, &input); err != nil {
		t.Fatalf("input: %v", err)
	}
	if input.Name != "ok" {
		t.Fatalf("applied pipeline = %q; want ok", input.Name)
	}
	if !strings.Contains(errOut.String(), "dup") {
		t.Fatalf("stderr = %q; want the duplicate mapping failure", errOut.String())
	}
}

// TestRunPipelineApplyFileFailures pins visible per-file failures while the
// remaining files are still attempted.
func TestRunPipelineApplyFileFailures(t *testing.T) {
	directory := "/repo/.kodema"
	cases := []struct {
		name      string
		setup     func(*fakeFS)
		wantCalls int
		wantName  string
	}{
		{
			name: "read failure",
			setup: func(filesystem *fakeFS) {
				filesystem.readErr[directory+"/a.yaml"] = fs.ErrPermission
			},
			wantCalls: 1,
			wantName:  "a",
		},
		{
			name: "invalid utf8",
			setup: func(filesystem *fakeFS) {
				filesystem.files[directory+"/a.yaml"] = []byte{0xff, 0xfe, 0x00}
			},
			wantCalls: 1,
			wantName:  "a",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			filesystem := newFakeFS()
			filesystem.entries[directory] = []cli.DirEntry{{Name: "a.yaml"}, {Name: "b.yaml"}}
			filesystem.files[directory+"/a.yaml"] = []byte("name: a\n")
			filesystem.files[directory+"/b.yaml"] = []byte("name: b\n")
			testCase.setup(filesystem)
			transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
				return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{}`)}, nil
			}}
			ports, _, errOut := testPorts(transport)
			ports.Files = filesystem
			code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory}, ports)
			if code != cli.ExitFailure {
				t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
			}
			recorded := transport.recorded()
			if len(recorded) != testCase.wantCalls {
				t.Fatalf("invocations = %d; want %d", len(recorded), testCase.wantCalls)
			}
			var input pipelineInput
			if err := json.Unmarshal(recorded[0].Request.Input, &input); err != nil {
				t.Fatalf("input: %v", err)
			}
			if input.Name != "b" {
				t.Fatalf("applied pipeline = %q; want b", input.Name)
			}
			if !strings.Contains(errOut.String(), testCase.wantName) {
				t.Fatalf("stderr = %q; want the failing file %q", errOut.String(), testCase.wantName)
			}
		})
	}
}

// TestRunPipelineApplyDirectoryFailures pins the directory boundary.
func TestRunPipelineApplyDirectoryFailures(t *testing.T) {
	directory := "/repo/.kodema"

	t.Run("read dir failure", func(t *testing.T) {
		filesystem := newFakeFS()
		filesystem.dirErr[directory] = fs.ErrPermission
		transport := &scriptedTransport{}
		ports, _, errOut := testPorts(transport)
		ports.Files = filesystem
		if code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory}, ports); code != cli.ExitFailure {
			t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
		}
		if got := len(transport.recorded()); got != 0 {
			t.Fatalf("invocations = %d; want 0", got)
		}
		if errOut.Len() == 0 {
			t.Fatal("stderr is empty; want a directory diagnostic")
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		filesystem := newFakeFS()
		filesystem.entries[directory] = nil
		transport := &scriptedTransport{}
		ports, _, errOut := testPorts(transport)
		ports.Files = filesystem
		if code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory}, ports); code != cli.ExitSuccess {
			t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
		}
		if got := len(transport.recorded()); got != 0 {
			t.Fatalf("invocations = %d; want 0", got)
		}
	})

	t.Run("unsupported names", func(t *testing.T) {
		filesystem := newFakeFS()
		filesystem.entries[directory] = []cli.DirEntry{
			{Name: "README.md"},
			{Name: "build.YAML"},
			{Name: "sub", IsDir: true},
			{Name: ".hidden.yaml"},
		}
		transport := &scriptedTransport{}
		ports, _, errOut := testPorts(transport)
		ports.Files = filesystem
		if code := cli.Run(context.Background(), testConfig(), []string{"pipeline", "apply", directory}, ports); code != cli.ExitSuccess {
			t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
		}
		if got := len(transport.recorded()); got != 0 {
			t.Fatalf("invocations = %d; want 0", got)
		}
	})
}

// TestRunFile pins `kodactl run <file>`: the inline pipeline.start input, the
// container stdout in action order and the exit code and stderr per outcome.
func TestRunFile(t *testing.T) {
	const path = "/repo/hello.yaml"
	document := []byte("actions:\n  - name: hello\n")
	cases := []struct {
		name string
		// inspections are the successive execution.inspect bodies; the last repeats.
		inspections []string
		// logs are the successive stdout chunks of each action.
		logs       map[string][]string
		wantCode   int
		wantOut    string
		wantErr    []string
		wantSleeps int
	}{
		{
			name: "succeeded prints every container action in order",
			inspections: []string{
				`{"state":"queued","actions":[
					{"action_id":"act-1","kind":"container","state":"pending"},
					{"action_id":"act-2","kind":"request","state":"pending","assignment":"service/git-service"},
					{"action_id":"act-3","kind":"container","state":"pending"}]}`,
				`{"state":"running","actions":[
					{"action_id":"act-1","kind":"container","state":"running"},
					{"action_id":"act-2","kind":"request","state":"pending","assignment":"service/git-service"},
					{"action_id":"act-3","kind":"container","state":"pending"}]}`,
				`{"state":"running","actions":[
					{"action_id":"act-1","kind":"container","state":"succeeded"},
					{"action_id":"act-2","kind":"request","state":"running","assignment":"service/git-service"},
					{"action_id":"act-3","kind":"container","state":"pending"}]}`,
				`{"state":"succeeded","cleanup":{"state":"pending"},"actions":[
					{"action_id":"act-1","kind":"container","state":"succeeded"},
					{"action_id":"act-2","kind":"request","state":"succeeded","assignment":"service/git-service","decision":"pass"},
					{"action_id":"act-3","kind":"container","state":"succeeded"}]}`,
				`{"state":"succeeded","cleanup":{"state":"complete"},"actions":[
					{"action_id":"act-1","kind":"container","state":"succeeded"},
					{"action_id":"act-2","kind":"request","state":"succeeded","assignment":"service/git-service","decision":"pass"},
					{"action_id":"act-3","kind":"container","state":"succeeded"}]}`,
			},
			logs:       map[string][]string{"act-1": {"one\n", "two\n"}, "act-3": {"three\n"}},
			wantCode:   cli.ExitSuccess,
			wantOut:    "one\ntwo\nthree\n",
			wantErr:    []string{"execution exec-1 started\n", "execution exec-1 succeeded\n"},
			wantSleeps: 4,
		},
		{
			name: "failed reports the server failure",
			inspections: []string{
				`{"state":"failed","actions":[
					{"action_id":"act-1","kind":"container","state":"failed"},
					{"action_id":"act-2","kind":"container","state":"cancelled"}],
				"failure":{"kind":"runtime","reason":"exit_code","detail":"container exited 3"}}`,
			},
			logs:     map[string][]string{"act-1": {"boom\n"}},
			wantCode: cli.ExitFailure,
			wantOut:  "boom\n",
			wantErr:  []string{"execution exec-1 failed: runtime: exit_code: container exited 3\n"},
		},
		{
			name: "cancelled exits 1",
			inspections: []string{
				`{"state":"cancelled","actions":[{"action_id":"act-1","kind":"container","state":"cancelled"}],
				"failure":{"kind":"cancelled"}}`,
			},
			wantCode: cli.ExitFailure,
			wantErr:  []string{"execution exec-1 cancelled: cancelled\n"},
		},
		{
			name: "a request waiting for a human stops the follow",
			inspections: []string{
				`{"state":"running","actions":[
					{"action_id":"act-1","kind":"container","state":"succeeded"},
					{"action_id":"act-2","name":"approve","kind":"request","state":"running","assignment":"role/reviewer"},
					{"action_id":"act-3","kind":"container","state":"pending"}]}`,
			},
			logs:     map[string][]string{"act-1": {"built\n"}},
			wantCode: cli.ExitFailure,
			wantErr:  []string{"execution exec-1 is waiting for role/reviewer", `"approve"`, "kodactl inbox", "kodactl approve exec-1"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inspects := 0
			transport := &scriptedTransport{respond: func(_ context.Context, index int, invocation api.Invocation) (api.Result, error) {
				if index > 20 {
					t.Fatalf("call %d: run did not stop following", index)
				}
				body := json.RawMessage(`{"execution_id":"exec-1","version":1}`)
				switch invocation.Operation {
				case "pipeline.start":
				case "execution.inspect":
					body = json.RawMessage(testCase.inspections[min(inspects, len(testCase.inspections)-1)])
					inspects++
				case "execution.logs.read":
					var request api.ExecutionLogsReadRequest
					if err := json.Unmarshal(invocation.Request.Input, &request); err != nil || request.Stream != "stdout" {
						t.Fatalf("logs request = %s (%v)", invocation.Request.Input, err)
					}
					var page api.ExecutionLogsReadResult
					for index, chunk := range testCase.logs[request.ActionID] {
						page.Chunks = append(page.Chunks, api.LogChunk{ActionID: request.ActionID, Stream: "stdout", Offset: uint64(index), Sequence: uint64(index + 1), Data: []byte(chunk)})
					}
					body, _ = json.Marshal(page)
				default:
					t.Fatalf("unexpected operation %s", invocation.Operation)
				}
				return api.Result{Operation: invocation.Operation, Body: body}, nil
			}}
			ports, out, errOut := testPorts(transport)
			ports.Files.(*fakeFS).files[path] = document
			sleeps := 0
			ports.Sleep = func(time.Duration) { sleeps++ }
			code := cli.Run(context.Background(), testConfig(), []string{"run", path, "--input", "branch=main"}, ports)
			if code != testCase.wantCode {
				t.Fatalf("exit code = %d; want %d (stderr: %s)", code, testCase.wantCode, errOut.String())
			}
			if out.String() != testCase.wantOut {
				t.Fatalf("stdout = %q; want %q", out.String(), testCase.wantOut)
			}
			for _, want := range testCase.wantErr {
				if !strings.Contains(errOut.String(), want) {
					t.Fatalf("stderr = %q; want %q", errOut.String(), want)
				}
			}
			if sleeps != testCase.wantSleeps {
				t.Fatalf("sleeps = %d; want %d", sleeps, testCase.wantSleeps)
			}
			start := transport.recorded()[0]
			var input api.PipelineStartRequest
			if err := json.Unmarshal(start.Request.Input, &input); err != nil {
				t.Fatalf("pipeline.start input: %v", err)
			}
			if start.Operation != "pipeline.start" || start.Request.IdempotencyKey == "" || input.ProjectID != "shop" ||
				input.PipelineID != "" || string(input.YAML) != string(document) || input.Inputs["branch"] != "main" {
				t.Fatalf("start = %s %+v (key %q)", start.Operation, input, start.Request.IdempotencyKey)
			}
		})
	}

	t.Run("interrupt names the execution left running", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		transport := &scriptedTransport{respond: func(ctx context.Context, _ int, invocation api.Invocation) (api.Result, error) {
			if invocation.Operation == "execution.inspect" {
				cancel()
				return api.Result{}, ctx.Err()
			}
			return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{"execution_id":"exec-1","version":1}`)}, nil
		}}
		ports, _, errOut := testPorts(transport)
		ports.Files.(*fakeFS).files[path] = document
		if code := cli.Run(ctx, testConfig(), []string{"run", path}, ports); code != cli.ExitFailure {
			t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
		}
		if !strings.Contains(errOut.String(), "run interrupted: execution exec-1 is still running") {
			t.Fatalf("stderr = %q; want the interrupted execution", errOut.String())
		}
	})

	t.Run("missing file starts nothing", func(t *testing.T) {
		transport := &scriptedTransport{}
		ports, out, errOut := testPorts(transport)
		if code := cli.Run(context.Background(), testConfig(), []string{"run", path}, ports); code != cli.ExitFailure {
			t.Fatalf("exit code = %d; want %d", code, cli.ExitFailure)
		}
		if got := len(transport.recorded()); got != 0 {
			t.Fatalf("invocations = %d; want 0", got)
		}
		if out.Len() != 0 || !strings.Contains(errOut.String(), "cannot read pipeline file") {
			t.Fatalf("stdout = %q, stderr = %q; want only a file diagnostic", out.String(), errOut.String())
		}
	})
}

var _ client.Transport = (*scriptedTransport)(nil)

package cli_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/cli"
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
	directory := "/repo/.kodama"
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
	directory := "/repo/.kodama"
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
	directory := "/repo/.kodama"
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
	directory := "/repo/.kodama"
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
	directory := "/repo/.kodama"

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

var _ client.Transport = (*scriptedTransport)(nil)

package cli_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// TestRunFile pins `kodema run <file>`: the inline pipeline.start input, the
// container stdout in action order, action progress, failed container stderr
// surfacing, --detach, and the exit code and stderr per outcome.
func TestRunFile(t *testing.T) {
	const path = "/repo/hello.yaml"
	document := []byte("actions:\n  - name: hello\n")
	cases := []struct {
		name string
		// inspections are the successive execution.inspect bodies; the last repeats.
		inspections []string
		// logs are the successive stdout chunks of each action.
		logs map[string][]string
		// stderrLogs are the successive stderr chunks of each action.
		stderrLogs map[string][]string
		// stderrErr simulates a failure reading the stderr stream.
		stderrErr error
		// stderrTruncated simulates server-side truncation on stderr.
		stderrTruncated bool
		wantCode        int
		wantOut         string
		wantErr         []string
		wantNotErr      []string
		wantErrCounts   map[string]int
		wantSleeps      int
	}{
		{
			name: "succeeded prints every container action in order with action transitions on stderr",
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
			logs:     map[string][]string{"act-1": {"one\n", "two\n"}, "act-3": {"three\n"}},
			wantCode: cli.ExitSuccess,
			wantOut:  "one\ntwo\nthree\n",
			wantErr: []string{
				"execution exec-1 started\n",
				"action act-1: running\n",
				"action act-1: succeeded\n",
				`execution exec-1 is waiting for service/git-service to answer request "act-2"` + "\n",
				"action act-2: succeeded (pass)\n",
				"action act-3: succeeded\n",
				"execution exec-1 succeeded\n",
			},
			wantSleeps: 4,
		},
		{
			name: "service request waiting notice is emitted once across polls",
			inspections: []string{
				`{"state":"running","actions":[
					{"action_id":"act-1","name":"qa-gate","kind":"request","state":"running","assignment":"service/uc09-qa-bot"}]}`,
				`{"state":"running","actions":[
					{"action_id":"act-1","name":"qa-gate","kind":"request","state":"running","assignment":"service/uc09-qa-bot"}]}`,
				`{"state":"running","actions":[
					{"action_id":"act-1","name":"qa-gate","kind":"request","state":"running","assignment":"service/uc09-qa-bot"}]}`,
				`{"state":"succeeded","actions":[
					{"action_id":"act-1","name":"qa-gate","kind":"request","state":"succeeded","assignment":"service/uc09-qa-bot","decision":"pass"}]}`,
			},
			wantCode: cli.ExitSuccess,
			wantOut:  "",
			wantErr: []string{
				"execution exec-1 started\n",
				`execution exec-1 is waiting for service/uc09-qa-bot to answer request "qa-gate"` + "\n",
				"action qa-gate: succeeded (pass)\n",
				"execution exec-1 succeeded\n",
			},
			wantErrCounts: map[string]int{
				`execution exec-1 is waiting for service/uc09-qa-bot to answer request "qa-gate"`: 1,
			},
			wantSleeps: 3,
		},
		{
			name: "failed surfaces container stderr on stderr while keeping stdout clean",
			inspections: []string{
				`{"state":"failed","actions":[
					{"action_id":"act-1","name":"build","kind":"container","state":"failed"},
					{"action_id":"act-2","name":"deploy","kind":"container","state":"cancelled"}],
				"failure":{"kind":"runtime","reason":"exit_code","detail":"workload exited with status 17"}}`,
			},
			logs:       map[string][]string{"act-1": {"partial stdout\n"}},
			stderrLogs: map[string][]string{"act-1": {"fatal: missing dependency\n", "exiting with 17\n"}},
			wantCode:   cli.ExitFailure,
			wantOut:    "partial stdout\n",
			wantErr: []string{
				"execution exec-1 started\n",
				"action build: failed\n",
				"--- stderr (action build) ---\n",
				"fatal: missing dependency\nexiting with 17\n",
				"---\n",
				"execution exec-1 failed: runtime: exit_code: workload exited with status 17\n",
			},
			wantNotErr: []string{"stderr truncated"},
		},
		{
			name: "failed with empty stderr reports logs inspection hint",
			inspections: []string{
				`{"state":"failed","actions":[
					{"action_id":"act-1","kind":"container","state":"failed"},
					{"action_id":"act-2","kind":"container","state":"cancelled"}],
				"failure":{"kind":"runtime","reason":"exit_code","detail":"container exited 3"}}`,
			},
			logs:     map[string][]string{"act-1": {"boom\n"}},
			wantCode: cli.ExitFailure,
			wantOut:  "boom\n",
			wantErr: []string{
				"kodema logs exec-1 --action act-1 --stream stderr",
				"execution exec-1 failed: runtime: exit_code: container exited 3\n",
			},
		},
		{
			name: "failed truncates oversized stderr tail and prints full logs hint",
			inspections: []string{
				`{"state":"failed","actions":[
					{"action_id":"act-1","name":"test","kind":"container","state":"failed"}],
				"failure":{"kind":"runtime","reason":"exit_code","detail":"workload exited with status 1"}}`,
			},
			stderrLogs: map[string][]string{"act-1": {numberedLines(80)}},
			wantCode:   cli.ExitFailure,
			wantErr: []string{
				"--- stderr (action test) ---\n",
				"line 31\n",
				"line 80\n",
				"stderr truncated; run 'kodema logs exec-1 --action test --stream stderr' for full logs\n",
				"execution exec-1 failed: runtime: exit_code: workload exited with status 1\n",
			},
			wantNotErr: []string{"line 30\n", "line 1\n"},
		},
		{
			name: "failed with byte-oversized UTF-8 stderr keeps valid UTF-8 tail and prints hint",
			inspections: []string{
				`{"state":"failed","actions":[
					{"action_id":"act-1","name":"compile","kind":"container","state":"failed"}],
				"failure":{"kind":"runtime","reason":"exit_code","detail":"workload exited with status 2"}}`,
			},
			stderrLogs: map[string][]string{"act-1": {strings.Repeat("é", 5000) + "\nfinal compile error\n"}},
			wantCode:   cli.ExitFailure,
			wantErr: []string{
				"--- stderr (action compile) ---\n",
				"final compile error\n",
				"stderr truncated; run 'kodema logs exec-1 --action compile --stream stderr' for full logs\n",
			},
		},
		{
			name: "failed with server-truncated short stderr prints full logs hint",
			inspections: []string{
				`{"state":"failed","actions":[
					{"action_id":"act-1","name":"lint","kind":"container","state":"failed"}],
				"failure":{"kind":"runtime","reason":"exit_code","detail":"workload exited with status 1"}}`,
			},
			stderrLogs:      map[string][]string{"act-1": {"lint error: unused var\n"}},
			stderrTruncated: true,
			wantCode:        cli.ExitFailure,
			wantErr: []string{
				"--- stderr (action lint) ---\n",
				"lint error: unused var\n",
				"stderr truncated; run 'kodema logs exec-1 --action lint --stream stderr' for full logs\n",
			},
		},
		{
			name: "failed stderr read error still reports execution failure and logs hint",
			inspections: []string{
				`{"state":"failed","actions":[
					{"action_id":"act-1","name":"build","kind":"container","state":"failed"}],
				"failure":{"kind":"runtime","reason":"exit_code","detail":"workload exited with status 17"}}`,
			},
			stderrErr: fault.New(fault.Forbidden, "logs denied"),
			wantCode:  cli.ExitFailure,
			wantErr: []string{
				"stderr (action build) unavailable",
				"kodema logs exec-1 --action build --stream stderr",
				"execution exec-1 failed: runtime: exit_code: workload exited with status 17\n",
			},
		},
		{
			name: "failed request action does not fetch container stderr",
			inspections: []string{
				`{"state":"failed","actions":[
					{"action_id":"act-1","name":"qa-gate","kind":"request","state":"failed","assignment":"service/qa","decision":"fail"}],
				"failure":{"kind":"request","reason":"rejected","detail":"request action was answered with a failing decision"}}`,
			},
			stderrErr: fault.New(fault.InvalidInput, "must not fetch stderr for request action"),
			wantCode:  cli.ExitFailure,
			wantErr: []string{
				"action qa-gate: failed (fail)\n",
				"execution exec-1 failed: request: rejected: request action was answered with a failing decision\n",
			},
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
			name: "oom-killed execution reports OOMKilled and memory limit",
			inspections: []string{
				`{"state":"failed","actions":[{"action_id":"act-1","kind":"container","state":"failed"}],
				"failure":{"kind":"runtime","reason":"oom_killed","detail":"OOMKilled: workload exceeded its 2 GiB memory limit (exit 137)"}}`,
			},
			wantCode: cli.ExitFailure,
			wantErr:  []string{"execution exec-1 failed: runtime: oom_killed: OOMKilled: workload exceeded its 2 GiB memory limit (exit 137)\n"},
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
			wantErr:  []string{"execution exec-1 is waiting for role/reviewer", `"approve"`, "kodema inbox", "kodema approve exec-1"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inspects := 0
			transport := &scriptedTransport{respond: func(_ context.Context, index int, invocation api.Invocation) (api.Result, error) {
				if index > 25 {
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
					if err := json.Unmarshal(invocation.Request.Input, &request); err != nil || (request.Stream != "stdout" && request.Stream != "stderr") {
						t.Fatalf("logs request = %s (%v)", invocation.Request.Input, err)
					}
					var page api.ExecutionLogsReadResult
					switch request.Stream {
					case "stdout":
						for idx, chunk := range testCase.logs[request.ActionID] {
							page.Chunks = append(page.Chunks, api.LogChunk{ActionID: request.ActionID, Stream: "stdout", Offset: uint64(idx), Sequence: uint64(idx + 1), Data: []byte(chunk)})
						}
					case "stderr":
						if testCase.stderrErr != nil {
							return api.Result{}, testCase.stderrErr
						}
						for idx, chunk := range testCase.stderrLogs[request.ActionID] {
							page.Chunks = append(page.Chunks, api.LogChunk{ActionID: request.ActionID, Stream: "stderr", Offset: uint64(idx), Sequence: uint64(idx + 1), Data: []byte(chunk)})
						}
						page.Truncated = testCase.stderrTruncated
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
			if !utf8.ValidString(errOut.String()) {
				t.Fatalf("stderr is not valid UTF-8: %q", errOut.String())
			}
			for _, want := range testCase.wantErr {
				if !strings.Contains(errOut.String(), want) {
					t.Fatalf("stderr = %q; want %q", errOut.String(), want)
				}
			}
			for _, unwanted := range testCase.wantNotErr {
				if strings.Contains(errOut.String(), unwanted) {
					t.Fatalf("stderr = %q; did not want %q", errOut.String(), unwanted)
				}
			}
			for substr, wantCount := range testCase.wantErrCounts {
				if got := strings.Count(errOut.String(), substr); got != wantCount {
					t.Fatalf("stderr count(%q) = %d; want %d (stderr: %q)", substr, got, wantCount, errOut.String())
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

	t.Run("detach prints execution id and exits 0 immediately", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			args    []string
			output  cli.OutputFormat
			wantOut string
		}{
			{
				name:    "table mode --detach",
				args:    []string{"run", "--detach", path},
				output:  cli.OutputTable,
				wantOut: "exec-detach-1\n",
			},
			{
				name:    "table mode -d after file",
				args:    []string{"run", path, "-d"},
				output:  cli.OutputTable,
				wantOut: "exec-detach-1\n",
			},
			{
				name:    "json mode --detach",
				args:    []string{"run", "--detach", path},
				output:  cli.OutputJSON,
				wantOut: "{\"execution_id\":\"exec-detach-1\",\"version\":1}\n",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				transport := &scriptedTransport{respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
					if invocation.Operation != "pipeline.start" {
						t.Fatalf("unexpected operation %s; --detach must not poll", invocation.Operation)
					}
					return api.Result{
						Operation: invocation.Operation,
						Body:      json.RawMessage(`{"execution_id":"exec-detach-1","version":1}`),
					}, nil
				}}
				ports, out, errOut := testPorts(transport)
				ports.Files.(*fakeFS).files[path] = document
				sleeps := 0
				ports.Sleep = func(time.Duration) { sleeps++ }
				cfg := testConfig()
				cfg.Output = tc.output
				if code := cli.Run(context.Background(), cfg, tc.args, ports); code != cli.ExitSuccess {
					t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
				}
				if out.String() != tc.wantOut {
					t.Fatalf("stdout = %q; want %q", out.String(), tc.wantOut)
				}
				if errOut.Len() != 0 {
					t.Fatalf("stderr = %q; want empty", errOut.String())
				}
				if sleeps != 0 {
					t.Fatalf("sleeps = %d; want 0", sleeps)
				}
				if got := len(transport.recorded()); got != 1 {
					t.Fatalf("invocations = %d; want 1", got)
				}
			})
		}
	})

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

func numberedLines(count int) string {
	var builder strings.Builder
	for i := 1; i <= count; i++ {
		builder.WriteString("line ")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteByte('\n')
	}
	return builder.String()
}

var _ client.Transport = (*scriptedTransport)(nil)

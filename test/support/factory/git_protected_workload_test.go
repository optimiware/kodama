package factory

// Executable guards for the protected-workload candidate/test/integrate command
// authors.
//
// The product runs an action script with `#!/bin/sh` and `set -e` (dash on the
// Debian workload image), so every rendered command list is executed here under
// `/bin/sh -e` in a container-shaped temporary workspace. A `git` shim and a
// `curl` shim on PATH record their argv and stdin, exactly like the workload
// scan tests: a credential that reached a process argument would be visible in
// the logs, and the HTTPS submitter's authorization must arrive on stdin only.
//
// The shims are deterministic state machines:
//
//   - the git shim owns HEAD/parent/ancestry/cleanliness answers and records
//     every invocation, so the tests can assert exact push refspecs, the
//     force-with-lease base and the absence of an integrate-time commit;
//   - the curl shim answers execution.inspect from ordered bodies and
//     execution.action.outputs from ordered statuses/bodies, so the tests can
//     assert slug-to-action-ID resolution, the current expected_version and the
//     explicit version-conflict retry.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	protectedTestToken      = "i04-g7a-test-token-0123456789abcdef"
	protectedTestExecution  = "exec-g7a-protected"
	protectedTestBase       = "1111111111111111111111111111111111111111"
	protectedTestCandidate  = "2222222222222222222222222222222222222222"
	protectedTestOther      = "3333333333333333333333333333333333333333"
	protectedTestWorkRef    = "refs/heads/work/i05-protected"
	protectedTestRepository = "project-i04/web"
)

// protectedWorkloadBox is one container-shaped workspace with executable shims
// and the exact logs and state files the tests inspect.
type protectedWorkloadBox struct {
	root       string
	workspace  string
	repository string
	tokenPath  string
	caPath     string
	markerPath string
	outputs    string

	binDir string

	gitArgv  string
	gitState string
	gitBase  string
	gitHead  string

	curlArgv    string
	curlStdin   string
	curlPutLog  string
	curlGetDir  string
	curlPutDir  string
	curlGetCnt  string
	curlPutCnt  string
	curlGetCode string

	caPEM []byte
}

// newProtectedWorkloadBox builds one isolated workspace, token mount, output
// root and shim directory. Nothing is created under a real /workspace or
// /kodama path.
func newProtectedWorkloadBox(t *testing.T, caPEM []byte) *protectedWorkloadBox {
	t.Helper()
	root := t.TempDir()
	box := &protectedWorkloadBox{
		root:       root,
		workspace:  filepath.Join(root, "workspace"),
		repository: filepath.Join(root, "workspace", "git-protected-repository"),
		tokenPath:  filepath.Join(root, "kodama", "secrets", "KODAMA_TOKEN"),
		caPath:     filepath.Join(root, "workspace", filepath.Base(gitWorkloadCAFile)),
		markerPath: filepath.Join(root, "workspace", ".kodama-workspace-marker"),
		outputs:    filepath.Join(root, "kodama", "outputs"),
		binDir:     filepath.Join(root, "bin"),
		caPEM:      caPEM,
	}
	box.gitArgv = filepath.Join(root, "logs", "git.argv")
	box.gitState = filepath.Join(root, "logs", "git.state")
	box.gitBase = protectedTestBase
	box.gitHead = protectedTestCandidate
	box.curlArgv = filepath.Join(root, "logs", "curl.argv")
	box.curlStdin = filepath.Join(root, "logs", "curl.stdin")
	box.curlPutLog = filepath.Join(root, "logs", "curl.put")
	box.curlGetDir = filepath.Join(root, "curl-get")
	box.curlPutDir = filepath.Join(root, "curl-put")
	box.curlGetCnt = filepath.Join(root, "logs", "curl.get.count")
	box.curlPutCnt = filepath.Join(root, "logs", "curl.put.count")
	box.curlGetCode = "200"

	for _, directory := range []string{
		box.workspace, box.binDir,
		filepath.Dir(box.tokenPath), filepath.Dir(box.gitArgv),
		box.curlGetDir, box.curlPutDir,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create protected workload fixture directory %s: %v", directory, err)
		}
	}
	writeProtectedFile(t, box.tokenPath, protectedTestToken+"\n")
	if err := os.WriteFile(filepath.Join(box.binDir, "git"), []byte(fakeProtectedGit), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(box.binDir, "curl"), []byte(fakeProtectedCurl), 0o755); err != nil {
		t.Fatalf("write curl shim: %v", err)
	}
	return box
}

// spec returns the internal authoring spec bound to this box. The paths mirror
// the real container paths while staying inside the temporary tree.
func (box *protectedWorkloadBox) spec() protectedWorkloadSpec {
	return protectedWorkloadSpec{
		URL:             "https://host.containers.internal:41000/" + protectedTestRepository + ".git",
		CAPEM:           box.caPEM,
		TokenPath:       box.tokenPath,
		CAPath:          box.caPath,
		Workspace:       box.workspace,
		Directory:       box.repository,
		MarkerPath:      box.markerPath,
		OutputsRoot:     box.outputs,
		WorkRef:         protectedTestWorkRef,
		ProtectedRef:    protectedWorkloadProtectedRef,
		ExpectedBase:    box.gitBase,
		ImplementAction: "implement",
		TestAction:      "test",
	}
}

// env returns a clean environment for one rendered action. The shim directory
// is first on PATH; the reserved container context is supplied exactly as the
// product supplies it.
func (box *protectedWorkloadBox) env(action string, extra ...string) []string {
	environment := make([]string, 0, len(os.Environ())+12)
	for _, entry := range os.Environ() {
		switch {
		case strings.HasPrefix(entry, "PATH="):
			continue
		case strings.HasPrefix(entry, "KODAMA_"):
			continue
		case strings.HasPrefix(entry, "FAKE_PROTECTED_"):
			continue
		case strings.HasPrefix(entry, "CURL_"):
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment,
		"PATH="+box.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KODAMA_URL=https://host.containers.internal:41000",
		"KODAMA_EXECUTION="+protectedTestExecution,
		"KODAMA_ACTION="+action,
		"FAKE_PROTECTED_GIT_ARGV="+box.gitArgv,
		"FAKE_PROTECTED_GIT_STATE="+box.gitState,
		"FAKE_PROTECTED_GIT_BASE="+box.gitBase,
		"FAKE_PROTECTED_GIT_CANDIDATE="+box.gitHead,
		"CURL_ARGV_LOG="+box.curlArgv,
		"CURL_STDIN_LOG="+box.curlStdin,
		"CURL_PUT_LOG="+box.curlPutLog,
		"CURL_GET_DIR="+box.curlGetDir,
		"CURL_PUT_DIR="+box.curlPutDir,
		"CURL_GET_COUNTER="+box.curlGetCnt,
		"CURL_PUT_COUNTER="+box.curlPutCnt,
		"CURL_GET_STATUS="+box.curlGetCode,
	)
	return append(environment, extra...)
}

// run executes one rendered action under /bin/sh -e and returns its combined
// output and exit status.
func (box *protectedWorkloadBox) run(t *testing.T, commands []string, action string, extra ...string) (string, int) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "action.sh")
	if err := os.WriteFile(script, []byte(strings.Join(commands, "\n")+"\n"), 0o700); err != nil {
		t.Fatalf("write rendered action script: %v", err)
	}
	command := exec.Command("/bin/sh", "-e", script)
	command.Env = box.env(action, extra...)
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	var exitError *exec.ExitError
	if ok := asExitError(err, &exitError); ok {
		return string(output), exitError.ExitCode()
	}
	t.Fatalf("run protected workload action: %v\n%s", err, output)
	return "", 0
}

// asExitError is a tiny errors.As equivalent kept local so this file's imports
// stay focused.
func asExitError(err error, target **exec.ExitError) bool {
	exitError, ok := err.(*exec.ExitError)
	if ok {
		*target = exitError
	}
	return ok
}

// writeProtectedFile creates one fixture file and its parent directories.
func writeProtectedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture file %s: %v", path, err)
	}
}

// readProtectedFile reads one recorded file; a missing file is empty so a
// negative assertion can distinguish "nothing happened" from a broken fixture.
func readProtectedFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read recorded file %s: %v", path, err)
	}
	return string(data)
}

// prepareProtectedRepository shapes the retained workspace for the test and
// integrate actions: a clean checkout at the candidate, the execution marker,
// the settled implement/test outputs and one test script.
func prepareProtectedRepository(t *testing.T, box *protectedWorkloadBox, testScript string) {
	t.Helper()
	if err := os.MkdirAll(box.repository, 0o755); err != nil {
		t.Fatalf("create retained repository: %v", err)
	}
	writeProtectedFile(t, box.markerPath, protectedTestExecution+"\n")
	writeProtectedFile(t, filepath.Join(box.repository, protectedWorkloadTestFile), testScript)
	writeProtectedFile(t, filepath.Join(box.outputs, "implement", "candidate"), protectedTestCandidate+"\n")
	writeProtectedFile(t, filepath.Join(box.outputs, "test", "candidate"), protectedTestCandidate+"\n")
	writeProtectedFile(t, filepath.Join(box.outputs, "test", "test_result"), "pass:"+protectedTestCandidate+"\n")
	writeProtectedFile(t, box.gitState, protectedTestCandidate+"\n")
}

// TestProtectedWorkloadUsesMountedCredentialAndTLS pins the authored security
// shape of all three actions: the execution token is read from its product
// mount on every request, Git receives it only through --config-env, curl
// receives authorization only through --config - on stdin, and the fixture CA
// is the only trust material. Requesting-argument logging is a shim, so a
// literal token or a disabled TLS check in the authored bytes is caught here.
func TestProtectedWorkloadUsesMountedCredentialAndTLS(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM
	box := newProtectedWorkloadBox(t, ca)
	spec := box.spec()

	candidate, err := protectedWorkloadCandidateCommands(spec)
	if err != nil {
		t.Fatalf("author candidate commands: %v", err)
	}
	testAction, err := protectedWorkloadTestCommands(spec)
	if err != nil {
		t.Fatalf("author test commands: %v", err)
	}
	integrate, err := protectedWorkloadIntegrateCommands(spec)
	if err != nil {
		t.Fatalf("author integrate commands: %v", err)
	}

	for name, commands := range map[string][]string{
		"candidate": candidate,
		"test":      testAction,
		"integrate": integrate,
	} {
		joined := strings.Join(commands, "\n")
		required := []string{
			gitWorkloadTokenPath,
			"--config-env=http.extraHeader=" + gitWorkloadAuthEnv,
			"http.followRedirects=false",
			"GIT_SSL_CAINFO=",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_TRACE=0",
			"GIT_CURL_VERBOSE=0",
			"repository-config-clean",
		}
		if name != "integrate" {
			required = append(required,
				"--config -",
				`header = "Authorization: Bearer %s"`,
				"--cacert ",
				"--proto '=https'",
			)
		}
		for _, value := range required {
			if !strings.Contains(joined, value) {
				t.Fatalf("%s commands do not contain %q:\n%s", name, value, joined)
			}
		}
		forbidden := []string{
			"http.sslVerify=false",
			"GIT_SSL_NO_VERIFY",
			"curl -k",
			"curl --insecure",
			"--insecure",
			"GIT_TRACE=1",
			"GIT_CURL_VERBOSE=1",
			"set -x",
			protectedTestToken,
		}
		for _, value := range forbidden {
			if strings.Contains(joined, value) {
				t.Fatalf("%s commands contain unsafe control %q:\n%s", name, value, joined)
			}
		}
		if strings.Contains(joined, "Authorization: Basic ") && !strings.Contains(joined, "Authorization: Basic $(printf") {
			t.Fatalf("%s commands carry a literal Basic header:\n%s", name, joined)
		}
		if strings.Contains(joined, "KODAMA_TOKEN=") {
			t.Fatalf("%s commands assign the token to an environment value:\n%s", name, joined)
		}
	}

	// The candidate and test actions must run the fail-closed credential scan
	// over the workspace before any output is submitted.
	for name, commands := range map[string][]string{"candidate": candidate, "test": testAction} {
		joined := strings.Join(commands, "\n")
		scan := strings.Index(joined, "scan_workspace_for_credential")
		submit := strings.LastIndex(joined, "kodama_submit_outputs")
		if scan < 0 || submit < 0 || scan > submit {
			t.Fatalf("%s commands do not scan the workspace before submitting outputs", name)
		}
	}

	// A task-associated action receives the task context; the smoke action must
	// not depend on it.
	withTask := spec
	withTask.RequireTask = true
	taskCommands, err := protectedWorkloadCandidateCommands(withTask)
	if err != nil {
		t.Fatalf("author task-associated candidate commands: %v", err)
	}
	taskJoined := strings.Join(taskCommands, "\n")
	for _, required := range []string{`test -n "$KODAMA_TASK"`, protectedWorkloadTaskFile} {
		if !strings.Contains(taskJoined, required) {
			t.Fatalf("task-associated candidate commands do not contain %q", required)
		}
	}
	smokeJoined := strings.Join(candidate, "\n")
	if strings.Contains(smokeJoined, `test -n "$KODAMA_TASK"`) {
		t.Fatalf("task-free candidate commands require a task")
	}

	// Refusals: the author never accepts a plain-HTTP origin, a credential in
	// the URL, a non-work ref, a missing/invalid CA, a path outside the
	// workspace or a malformed base.
	unsafe := []struct {
		name string
		spec protectedWorkloadSpec
	}{
		{name: "plain http", spec: withValue(spec, func(s *protectedWorkloadSpec) { s.URL = "http://host.containers.internal:41000/repository.git" })},
		{name: "url credentials", spec: withValue(spec, func(s *protectedWorkloadSpec) {
			s.URL = "https://kodama:secret@host.containers.internal:41000/repository.git"
		})},
		{name: "protected work ref", spec: withValue(spec, func(s *protectedWorkloadSpec) { s.WorkRef = "refs/heads/main" })},
		{name: "missing CA", spec: withValue(spec, func(s *protectedWorkloadSpec) { s.CAPEM = nil })},
		{name: "relative directory", spec: withValue(spec, func(s *protectedWorkloadSpec) { s.Directory = "repository" })},
		{name: "directory outside workspace", spec: withValue(spec, func(s *protectedWorkloadSpec) { s.Directory = "/tmp/repository" })},
		{name: "malformed base", spec: withValue(spec, func(s *protectedWorkloadSpec) { s.ExpectedBase = "not-a-commit" })},
	}
	for _, testCase := range unsafe {
		if _, err := protectedWorkloadCandidateCommands(testCase.spec); err == nil {
			t.Fatalf("%s: candidate command authoring accepted an unsafe spec", testCase.name)
		}
	}

	// The integrate action requires an explicit base: a missing base would
	// silently drop the force-with-lease precondition.
	missingBase := withValue(spec, func(s *protectedWorkloadSpec) { s.ExpectedBase = "" })
	if _, err := protectedWorkloadIntegrateCommands(missingBase); err == nil {
		t.Fatalf("integrate command authoring accepted a missing expected base")
	}
}

// TestProtectedWorkloadOutputsUseCurrentActionAndEnvelope proves the HTTPS
// submitter resolves the KODAMA_ACTION slug to the real action ID, refreshes
// the observed state only for an explicit version_conflict, sends the complete
// output map in the served envelope and fails closed on every other status.
func TestProtectedWorkloadOutputsUseCurrentActionAndEnvelope(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM

	t.Run("resolves the slug, refreshes state and retries once", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		writeProtectedInspect(t, box, 1, 7, "act-implement-1", "act-test-1")
		writeProtectedInspect(t, box, 2, 8, "act-implement-1", "act-test-1")
		writeProtectedPut(t, box, 1, "409", `{"code":"version_conflict","message":"stale execution version"}`)
		writeProtectedPut(t, box, 2, "200", `{"operation":"execution.action.outputs"}`)

		commands, err := protectedWorkloadSubmitCommands(box.spec(), "candidate="+protectedTestCandidate)
		if err != nil {
			t.Fatalf("author output submission: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code != 0 {
			t.Fatalf("output submission exited %d; want 0\n%s", code, output)
		}

		puts := readProtectedFile(t, box.curlPutLog)
		if count := strings.Count(puts, "PUT "); count != 2 {
			t.Fatalf("output submission made %d PUTs; want 2 (one conflict, one retry)\n%s", count, puts)
		}
		wantURL := "/api/v1/executions/" + protectedTestExecution + "/actions/act-test-1/outputs"
		for line := range strings.SplitSeq(puts, "\n") {
			if strings.HasPrefix(line, "PUT ") && !strings.Contains(line, wantURL) {
				t.Fatalf("PUT URL %q does not use the resolved action ID %q", line, wantURL)
			}
		}
		if strings.Contains(puts, "/actions/test/outputs") {
			t.Fatalf("output submission used the action slug as the action ID:\n%s", puts)
		}
		if !strings.Contains(puts, `"expected_version":7`) || !strings.Contains(puts, `"expected_version":8`) {
			t.Fatalf("output submission did not refresh expected_version after the conflict:\n%s", puts)
		}
		for _, required := range []string{
			`"execution_id":"` + protectedTestExecution + `"`,
			`"action_id":"act-test-1"`,
			`"outputs":{"candidate":"` + protectedTestCandidate + `"}`,
		} {
			if !strings.Contains(puts, required) {
				t.Fatalf("output submission body is missing %q:\n%s", required, puts)
			}
		}
		if !strings.Contains(puts, `"idempotency_key":"outputs-`) {
			t.Fatalf("output submission body carries no idempotency key:\n%s", puts)
		}

		argv := readProtectedFile(t, box.curlArgv)
		if strings.Contains(argv, protectedTestToken) {
			t.Fatalf("the execution token entered a process argument:\n%s", argv)
		}
		for _, required := range []string{"--config", "--cacert", "--proto", "=https"} {
			if !strings.Contains(argv, required) {
				t.Fatalf("curl argv is missing %q:\n%s", required, argv)
			}
		}
		stdin := readProtectedFile(t, box.curlStdin)
		if count := strings.Count(stdin, "Authorization: Bearer "+protectedTestToken); count != 4 {
			t.Fatalf("curl stdin carried the authorization %d times; want 4 (two GETs and two PUTs)\n%s", count, stdin)
		}
	})

	t.Run("sends the complete output map in one envelope", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		writeProtectedInspect(t, box, 1, 5, "act-implement-1", "act-test-1")
		writeProtectedPut(t, box, 1, "200", `{"operation":"execution.action.outputs"}`)

		commands, err := protectedWorkloadSubmitCommands(box.spec(),
			"candidate="+protectedTestCandidate, "test_result=pass:"+protectedTestCandidate)
		if err != nil {
			t.Fatalf("author output submission: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code != 0 {
			t.Fatalf("output submission exited %d; want 0\n%s", code, output)
		}
		puts := readProtectedFile(t, box.curlPutLog)
		if count := strings.Count(puts, "PUT "); count != 1 {
			t.Fatalf("output submission made %d PUTs; want 1\n%s", count, puts)
		}
		if !strings.Contains(puts, `"outputs":{"candidate":"`+protectedTestCandidate+`","test_result":"pass:`+protectedTestCandidate+`"}`) {
			t.Fatalf("output submission did not send the complete declared map:\n%s", puts)
		}
	})

	t.Run("fails closed on a non-conflict status", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		writeProtectedInspect(t, box, 1, 9, "act-implement-1", "act-test-1")
		writeProtectedPut(t, box, 1, "500", `{"code":"internal_failure","message":"boom"}`)

		commands, err := protectedWorkloadSubmitCommands(box.spec(), "candidate="+protectedTestCandidate)
		if err != nil {
			t.Fatalf("author output submission: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code == 0 {
			t.Fatalf("output submission accepted an internal failure; want a failing action\n%s", output)
		}
		puts := readProtectedFile(t, box.curlPutLog)
		if count := strings.Count(puts, "PUT "); count != 1 {
			t.Fatalf("output submission retried a non-conflict failure (%d PUTs); want 1\n%s", count, puts)
		}
	})

	t.Run("fails closed on an unidentified conflict", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		writeProtectedInspect(t, box, 1, 9, "act-implement-1", "act-test-1")
		writeProtectedPut(t, box, 1, "409", `{"code":"forbidden","message":"not a version conflict"}`)

		commands, err := protectedWorkloadSubmitCommands(box.spec(), "candidate="+protectedTestCandidate)
		if err != nil {
			t.Fatalf("author output submission: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code == 0 {
			t.Fatalf("output submission retried an unidentified 409; want a failing action\n%s", output)
		}
		puts := readProtectedFile(t, box.curlPutLog)
		if count := strings.Count(puts, "PUT "); count != 1 {
			t.Fatalf("output submission retried an unidentified conflict (%d PUTs); want 1\n%s", count, puts)
		}
	})

	t.Run("fails closed when inspect cannot resolve the action", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		writeProtectedInspect(t, box, 1, 4, "act-implement-1") // no test action
		writeProtectedPut(t, box, 1, "200", `{"operation":"execution.action.outputs"}`)

		commands, err := protectedWorkloadSubmitCommands(box.spec(), "candidate="+protectedTestCandidate)
		if err != nil {
			t.Fatalf("author output submission: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code == 0 {
			t.Fatalf("output submission accepted an inspect body without the running action; want a failing action\n%s", output)
		}
		if puts := readProtectedFile(t, box.curlPutLog); strings.Contains(puts, "PUT ") {
			t.Fatalf("output submission issued a PUT without a resolved action:\n%s", puts)
		}
	})

	t.Run("fails closed when inspect itself fails", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		box.curlGetCode = "500"
		writeProtectedInspect(t, box, 1, 4, "act-implement-1", "act-test-1")
		commands, err := protectedWorkloadSubmitCommands(box.spec(), "candidate="+protectedTestCandidate)
		if err != nil {
			t.Fatalf("author output submission: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code == 0 {
			t.Fatalf("output submission accepted a failed inspect; want a failing action\n%s", output)
		}
		if puts := readProtectedFile(t, box.curlPutLog); strings.Contains(puts, "PUT ") {
			t.Fatalf("output submission issued a PUT after a failed inspect:\n%s", puts)
		}
	})
}

// armProtectedSubmission makes a reachable output submission succeed: the
// inspect body resolves the running action and the PUT answers 200. A failure
// test that arms this proves the absence of a PUT is fail-fast ordering, not a
// broken submitter fixture.
func armProtectedSubmission(t *testing.T, box *protectedWorkloadBox) {
	t.Helper()
	writeProtectedInspect(t, box, 1, 6, "act-implement-1", "act-test-1")
	writeProtectedPut(t, box, 1, "200", `{"operation":"execution.action.outputs"}`)
}

// TestProtectedWorkloadStopsOnTestFailure proves the test action fails fast
// before submitting any output: a failing test script, a moved checkout or a
// dirty tree stops the action with no output PUT at all. The success control
// then shows the same action does submit the exact candidate and pass result.
func TestProtectedWorkloadStopsOnTestFailure(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM

	t.Run("failing test script submits nothing", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		prepareProtectedRepository(t, box, "#!/bin/sh\nexit 1\n")
		armProtectedSubmission(t, box)
		commands, err := protectedWorkloadTestCommands(box.spec())
		if err != nil {
			t.Fatalf("author test commands: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code == 0 {
			t.Fatalf("test action succeeded with a failing test script\n%s", output)
		}
		if puts := readProtectedFile(t, box.curlPutLog); strings.Contains(puts, "PUT ") {
			t.Fatalf("test action submitted outputs after a failing test:\n%s", puts)
		}
	})

	t.Run("moved checkout submits nothing", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		prepareProtectedRepository(t, box, "#!/bin/sh\nexit 0\n")
		writeProtectedFile(t, box.gitState, protectedTestOther+"\n")
		armProtectedSubmission(t, box)
		commands, err := protectedWorkloadTestCommands(box.spec())
		if err != nil {
			t.Fatalf("author test commands: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code == 0 {
			t.Fatalf("test action succeeded with a checkout outside the declared candidate\n%s", output)
		}
		if puts := readProtectedFile(t, box.curlPutLog); strings.Contains(puts, "PUT ") {
			t.Fatalf("test action submitted outputs for a moved checkout:\n%s", puts)
		}
	})

	t.Run("dirty tracked files submit nothing", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		prepareProtectedRepository(t, box, "#!/bin/sh\nexit 0\n")
		armProtectedSubmission(t, box)
		commands, err := protectedWorkloadTestCommands(box.spec())
		if err != nil {
			t.Fatalf("author test commands: %v", err)
		}
		output, code := box.run(t, commands, "test", "FAKE_PROTECTED_GIT_DIRTY=1")
		if code == 0 {
			t.Fatalf("test action succeeded with dirty tracked files\n%s", output)
		}
		if puts := readProtectedFile(t, box.curlPutLog); strings.Contains(puts, "PUT ") {
			t.Fatalf("test action submitted outputs with dirty tracked files:\n%s", puts)
		}
	})

	t.Run("a replaced workspace marker submits nothing", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		prepareProtectedRepository(t, box, "#!/bin/sh\nexit 0\n")
		writeProtectedFile(t, box.markerPath, "another-execution\n")
		armProtectedSubmission(t, box)
		commands, err := protectedWorkloadTestCommands(box.spec())
		if err != nil {
			t.Fatalf("author test commands: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code == 0 {
			t.Fatalf("test action succeeded in a workspace owned by another execution\n%s", output)
		}
		if puts := readProtectedFile(t, box.curlPutLog); strings.Contains(puts, "PUT ") {
			t.Fatalf("test action submitted outputs for a replaced workspace marker:\n%s", puts)
		}
	})

	t.Run("successful tests submit the exact candidate", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		prepareProtectedRepository(t, box, "#!/bin/sh\nset -eu\ntest 2 -gt 1\n")
		armProtectedSubmission(t, box)
		commands, err := protectedWorkloadTestCommands(box.spec())
		if err != nil {
			t.Fatalf("author test commands: %v", err)
		}
		output, code := box.run(t, commands, "test")
		if code != 0 {
			t.Fatalf("test action exited %d; want 0\n%s", code, output)
		}
		puts := readProtectedFile(t, box.curlPutLog)
		if !strings.Contains(puts, `"outputs":{"candidate":"`+protectedTestCandidate+`","test_result":"pass:`+protectedTestCandidate+`"}`) {
			t.Fatalf("test action did not submit the exact candidate and pass result:\n%s", puts)
		}
		if strings.Contains(puts, protectedTestOther) {
			t.Fatalf("test action submitted another commit:\n%s", puts)
		}
	})
}

// TestProtectedWorkloadCandidatePublishesExactOutput proves the implement action
// submits only its own declared `candidate` output and pushes exactly its HEAD
// to the granted work ref, never to the protected ref.
func TestProtectedWorkloadCandidatePublishesExactOutput(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM
	box := newProtectedWorkloadBox(t, ca)
	armProtectedSubmission(t, box)
	commands, err := protectedWorkloadCandidateCommands(box.spec())
	if err != nil {
		t.Fatalf("author candidate commands: %v", err)
	}
	output, code := box.run(t, commands, "implement")
	if code != 0 {
		t.Fatalf("candidate action exited %d; want 0\n%s", code, output)
	}
	if !strings.Contains(output, "candidate-output-submitted") {
		t.Fatalf("candidate action did not report its positive control:\n%s", output)
	}

	argv := readProtectedFile(t, box.gitArgv)
	var sawPush, sawWorkRefspec bool
	for line := range strings.SplitSeq(argv, "\n") {
		switch line {
		case "push":
			sawPush = true
		case "HEAD:" + protectedTestWorkRef:
			sawWorkRefspec = true
		}
		if strings.Contains(line, protectedWorkloadProtectedRef) {
			t.Fatalf("candidate push names the protected ref %q:\n%s", protectedWorkloadProtectedRef, argv)
		}
	}
	if !sawPush || !sawWorkRefspec {
		t.Fatalf("candidate push argv push=%t work-refspec=%t:\n%s", sawPush, sawWorkRefspec, argv)
	}
	if strings.Contains(argv, "--force-with-lease") {
		t.Fatalf("candidate push claimed a protected-ref lease:\n%s", argv)
	}

	puts := readProtectedFile(t, box.curlPutLog)
	if !strings.Contains(puts, `"outputs":{"candidate":"`+protectedTestCandidate+`"}`) {
		t.Fatalf("candidate action did not submit exactly its own commit:\n%s", puts)
	}
	if strings.Contains(puts, protectedWorkloadTestResultKey) {
		t.Fatalf("candidate action submitted a test result it never ran:\n%s", puts)
	}
	if strings.Contains(puts, protectedTestOther) {
		t.Fatalf("candidate action submitted another commit:\n%s", puts)
	}
}

// TestProtectedWorkloadStopsOnCandidateFailure proves the implement action is
// fail-closed before it submits the candidate output: an unexpected checkout
// never pushes, and a hook-refused or lease-rejected push never reaches the
// output submitter.
func TestProtectedWorkloadStopsOnCandidateFailure(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM
	failureCases := []struct {
		name       string
		shape      func(t *testing.T, box *protectedWorkloadBox)
		env        []string
		expectPush bool
	}{
		{
			name: "a checkout outside the reviewed base never pushes",
			shape: func(t *testing.T, box *protectedWorkloadBox) {
				writeProtectedFile(t, box.gitState, protectedTestOther+"\n")
			},
			expectPush: false,
		},
		{
			name: "a hook-refused push submits nothing",
			env: []string{
				"FAKE_PROTECTED_GIT_PUSH_STATUS=128",
				`FAKE_PROTECTED_GIT_PUSH_STDERR=! [remote rejected] HEAD -> refs/heads/work/i05-protected (hook declined)`,
			},
			expectPush: true,
		},
		{
			name: "a lease-style rejection submits nothing",
			env: []string{
				"FAKE_PROTECTED_GIT_PUSH_STATUS=1",
				`FAKE_PROTECTED_GIT_PUSH_STDERR=! [rejected] HEAD -> refs/heads/work/i05-protected (stale info)`,
			},
			expectPush: true,
		},
	}
	for _, testCase := range failureCases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newProtectedWorkloadBox(t, ca)
			armProtectedSubmission(t, box)
			if testCase.shape != nil {
				testCase.shape(t, box)
			}
			commands, err := protectedWorkloadCandidateCommands(box.spec())
			if err != nil {
				t.Fatalf("author candidate commands: %v", err)
			}
			output, code := box.run(t, commands, "implement", testCase.env...)
			if code == 0 {
				t.Fatalf("candidate action succeeded for %s\n%s", testCase.name, output)
			}
			if strings.Contains(output, "candidate-output-submitted") {
				t.Fatalf("candidate action reported a submitted output for %s\n%s", testCase.name, output)
			}
			if puts := readProtectedFile(t, box.curlPutLog); strings.Contains(puts, "PUT ") {
				t.Fatalf("candidate action submitted outputs for %s:\n%s", testCase.name, puts)
			}
			argv := readProtectedFile(t, box.gitArgv)
			if attempted := strings.Contains(argv, "push"); attempted != testCase.expectPush {
				t.Fatalf("candidate action push attempted=%t; want %t for %s:\n%s", attempted, testCase.expectPush, testCase.name, argv)
			}
		})
	}
}

// TestProtectedWorkloadOutputsKeyIsPerAction proves one execution's implement and
// test submissions carry different idempotency keys that both name the action
// slug, so the product cannot collapse them into one operation.
func TestProtectedWorkloadOutputsKeyIsPerAction(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM
	box := newProtectedWorkloadBox(t, ca)
	spec := box.spec()
	writeProtectedInspect(t, box, 1, 6, "act-implement-1", "act-test-1")
	writeProtectedInspect(t, box, 2, 6, "act-implement-1", "act-test-1")
	writeProtectedPut(t, box, 1, "200", `{"operation":"execution.action.outputs"}`)
	writeProtectedPut(t, box, 2, "200", `{"operation":"execution.action.outputs"}`)

	implement, err := protectedWorkloadSubmitCommands(spec, "candidate="+protectedTestCandidate)
	if err != nil {
		t.Fatalf("author implement output submission: %v", err)
	}
	if output, code := box.run(t, implement, "implement"); code != 0 {
		t.Fatalf("implement output submission exited %d; want 0\n%s", code, output)
	}
	testAction, err := protectedWorkloadSubmitCommands(spec,
		"candidate="+protectedTestCandidate, "test_result=pass:"+protectedTestCandidate)
	if err != nil {
		t.Fatalf("author test output submission: %v", err)
	}
	if output, code := box.run(t, testAction, "test"); code != 0 {
		t.Fatalf("test output submission exited %d; want 0\n%s", code, output)
	}

	puts := readProtectedFile(t, box.curlPutLog)
	implementKey := `"idempotency_key":"outputs-exec-g7a-protected-implement"`
	testKey := `"idempotency_key":"outputs-exec-g7a-protected-test"`
	if !strings.Contains(puts, implementKey) {
		t.Fatalf("implement submission key does not carry its action slug:\n%s", puts)
	}
	if !strings.Contains(puts, testKey) {
		t.Fatalf("test submission key does not carry its action slug:\n%s", puts)
	}
	if implementKey == testKey {
		t.Fatalf("implement and test submissions share one idempotency key")
	}
}

// TestProtectedWorkloadIntegratesExactCandidateAndBase proves the integrate
// action reads both settled candidate outputs, requires the exact test result
// and ancestry, and pushes only the candidate with the explicit
// --force-with-lease=refs/heads/main:<B> precondition without creating a commit.
func TestProtectedWorkloadIntegratesExactCandidateAndBase(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM

	t.Run("pushes the exact candidate with the reviewed base lease", func(t *testing.T) {
		box := newProtectedWorkloadBox(t, ca)
		prepareProtectedRepository(t, box, "#!/bin/sh\nexit 0\n")
		commands, err := protectedWorkloadIntegrateCommands(box.spec())
		if err != nil {
			t.Fatalf("author integrate commands: %v", err)
		}
		output, code := box.run(t, commands, "integrate")
		if code != 0 {
			t.Fatalf("integrate action exited %d; want 0\n%s", code, output)
		}

		argv := readProtectedFile(t, box.gitArgv)
		lines := strings.Split(argv, "\n")
		for _, forbidden := range []string{"commit", "add", "checkout", "reset"} {
			for _, line := range lines {
				if line == forbidden {
					t.Fatalf("integrate action invoked git %q; it must not create another commit:\n%s", forbidden, argv)
				}
			}
		}
		lease := "--force-with-lease=" + protectedWorkloadProtectedRef + ":" + protectedTestBase
		var sawLease, sawRefspec, sawPush bool
		for _, line := range lines {
			if line == lease {
				sawLease = true
			}
			if line == protectedTestCandidate+":"+protectedWorkloadProtectedRef {
				sawRefspec = true
			}
			if line == "push" {
				sawPush = true
			}
		}
		if !sawPush || !sawLease || !sawRefspec {
			t.Fatalf("integrate push argv push=%t lease=%t refspec=%t:\n%s", sawPush, sawLease, sawRefspec, argv)
		}
		if strings.Contains(argv, protectedTestToken) {
			t.Fatalf("the execution token entered the git argv:\n%s", argv)
		}
		if !strings.Contains(argv, "--config-env=http.extraHeader="+gitWorkloadAuthEnv) {
			t.Fatalf("integrate Git authentication did not use --config-env:\n%s", argv)
		}
		if !strings.Contains(argv, "merge-base") {
			t.Fatalf("integrate action did not check ancestry:\n%s", argv)
		}
	})

	failureCases := []struct {
		name  string
		shape func(t *testing.T, box *protectedWorkloadBox)
		env   []string
	}{
		{
			name: "a different test candidate never integrates",
			shape: func(t *testing.T, box *protectedWorkloadBox) {
				writeProtectedFile(t, filepath.Join(box.outputs, "test", "candidate"), protectedTestOther+"\n")
			},
		},
		{
			name: "a result for another candidate never integrates",
			shape: func(t *testing.T, box *protectedWorkloadBox) {
				writeProtectedFile(t, filepath.Join(box.outputs, "test", "test_result"), "pass:"+protectedTestOther+"\n")
			},
		},
		{
			name: "a missing test result never integrates",
			shape: func(t *testing.T, box *protectedWorkloadBox) {
				if err := os.Remove(filepath.Join(box.outputs, "test", "test_result")); err != nil {
					t.Fatalf("remove test result: %v", err)
				}
			},
		},
		{
			name:  "a non-ancestor base never integrates",
			env:   []string{"FAKE_PROTECTED_GIT_ANCESTOR_STATUS=1"},
			shape: func(t *testing.T, box *protectedWorkloadBox) {},
		},
		{
			name: "a moved checkout never integrates",
			shape: func(t *testing.T, box *protectedWorkloadBox) {
				writeProtectedFile(t, box.gitState, protectedTestOther+"\n")
			},
		},
		{
			name: "a replaced workspace marker never integrates",
			shape: func(t *testing.T, box *protectedWorkloadBox) {
				writeProtectedFile(t, box.markerPath, "another-execution\n")
			},
		},
	}
	for _, testCase := range failureCases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newProtectedWorkloadBox(t, ca)
			prepareProtectedRepository(t, box, "#!/bin/sh\nexit 0\n")
			testCase.shape(t, box)
			commands, err := protectedWorkloadIntegrateCommands(box.spec())
			if err != nil {
				t.Fatalf("author integrate commands: %v", err)
			}
			output, code := box.run(t, commands, "integrate", testCase.env...)
			if code == 0 {
				t.Fatalf("integrate action succeeded for %s\n%s", testCase.name, output)
			}
			if argv := readProtectedFile(t, box.gitArgv); strings.Contains(argv, "push") {
				t.Fatalf("integrate action pushed the protected ref for %s:\n%s", testCase.name, argv)
			}
		})
	}

	// A push that the served repository rejects is the last effect of the
	// integrate action: the action must fail without reporting a protected
	// integration, so a hook refusal or a stale force-with-lease is never
	// mistaken for a landed change.
	pushFailures := []struct {
		name string
		env  []string
	}{
		{
			name: "a hook-refused push never reports the integration",
			env: []string{
				"FAKE_PROTECTED_GIT_PUSH_STATUS=128",
				`FAKE_PROTECTED_GIT_PUSH_STDERR=! [remote rejected] 2222222222222222222222222222222222222222 -> refs/heads/main (hook declined)`,
			},
		},
		{
			name: "a lease-rejected push never reports the integration",
			env: []string{
				"FAKE_PROTECTED_GIT_PUSH_STATUS=1",
				`FAKE_PROTECTED_GIT_PUSH_STDERR=! [rejected] 2222222222222222222222222222222222222222 -> refs/heads/main (stale info)`,
			},
		},
	}
	for _, testCase := range pushFailures {
		t.Run(testCase.name, func(t *testing.T) {
			box := newProtectedWorkloadBox(t, ca)
			prepareProtectedRepository(t, box, "#!/bin/sh\nexit 0\n")
			commands, err := protectedWorkloadIntegrateCommands(box.spec())
			if err != nil {
				t.Fatalf("author integrate commands: %v", err)
			}
			output, code := box.run(t, commands, "integrate", testCase.env...)
			if code == 0 {
				t.Fatalf("integrate action succeeded after a rejected push\n%s", output)
			}
			if strings.Contains(output, "protected-integration-pushed") {
				t.Fatalf("integrate action reported a protected integration after a rejected push\n%s", output)
			}
			argv := readProtectedFile(t, box.gitArgv)
			if !strings.Contains(argv, "push") {
				t.Fatalf("integrate action never attempted the rejected push:\n%s", argv)
			}
			if !strings.Contains(argv, "--force-with-lease="+protectedWorkloadProtectedRef+":"+protectedTestBase) {
				t.Fatalf("rejected integrate push dropped the reviewed base lease:\n%s", argv)
			}
		})
	}
}

// withValue returns a copy of spec changed by mutate, so the table cases never
// leak one change into another.
func withValue(spec protectedWorkloadSpec, mutate func(*protectedWorkloadSpec)) protectedWorkloadSpec {
	copy := spec
	mutate(&copy)
	return copy
}

// writeProtectedInspect writes one ordered execution.inspect response. The
// actions are rendered with the same stable member order the Go API emits, so
// the shell submitter's field extraction is deliberately pinned to a concrete
// served shape rather than a hand-waved "some JSON".
func writeProtectedInspect(t *testing.T, box *protectedWorkloadBox, index int, executionVersion int, actions ...string) {
	t.Helper()
	builder := strings.Builder{}
	builder.WriteString(`{"operation":"execution.inspect","body":{"execution_id":"` + protectedTestExecution + `","state":"running","version":`)
	builder.WriteString(itoa(executionVersion))
	builder.WriteString(`,"current_action":0,"actions":[`)
	wrote := false
	for position, action := range actions {
		if wrote {
			builder.WriteString(",")
		}
		wrote = true
		name := "implement"
		if position > 0 {
			name = "test"
		}
		builder.WriteString(`{"action_id":"` + action + `","index":`)
		builder.WriteString(itoa(position))
		builder.WriteString(`,"name":"` + name + `","kind":"container","state":"running","version":3}`)
	}
	builder.WriteString(`],"cleanup":{"state":"none","attempts":0}}}`)
	path := filepath.Join(box.curlGetDir, "inspect."+itoa(index))
	writeProtectedFile(t, path, builder.String())
	writeProtectedFile(t, filepath.Join(box.curlGetDir, "inspect.last"), builder.String())
}

// writeProtectedPut writes one ordered execution.action.outputs status/body.
func writeProtectedPut(t *testing.T, box *protectedWorkloadBox, index int, status, body string) {
	t.Helper()
	writeProtectedFile(t, filepath.Join(box.curlPutDir, "status."+itoa(index)), status+"\n")
	writeProtectedFile(t, filepath.Join(box.curlPutDir, "body."+itoa(index)), body)
	writeProtectedFile(t, filepath.Join(box.curlPutDir, "status.last"), status+"\n")
	writeProtectedFile(t, filepath.Join(box.curlPutDir, "body.last"), body)
}

// itoa renders one small non-negative integer without pulling strconv into the
// fixture's import set.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}

// fakeProtectedGit is the recording git shim. It answers only the reads the
// authored actions perform and writes the candidate into its state file when
// the action commits, so the same action observes the pre-commit base and the
// post-commit candidate. A push exits with FAKE_PROTECTED_GIT_PUSH_STATUS
// (default 0) and prints FAKE_PROTECTED_GIT_PUSH_STDERR first, so a hook
// refusal or a stale lease can be exercised instead of assumed successful.
const fakeProtectedGit = `#!/bin/sh
set -u
for argument in "$@"; do printf '%s\n' "$argument" >> "$FAKE_PROTECTED_GIT_ARGV"; done
subcommand=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|--config-env) shift 2 ;;
    -*) shift ;;
    *) subcommand=$1; break ;;
  esac
done
if [ -z "$subcommand" ]; then exit 3; fi
case "$subcommand" in
  clone)
    destination=""
    for argument in "$@"; do destination=$argument; done
    mkdir -p "$destination"
    exit 0
    ;;
  rev-parse)
    case "$*" in
      *"HEAD^"*) printf '%s\n' "$FAKE_PROTECTED_GIT_BASE" ;;
      *"HEAD"*)
        if [ -f "$FAKE_PROTECTED_GIT_STATE" ]; then cat "$FAKE_PROTECTED_GIT_STATE"; else printf '%s\n' "$FAKE_PROTECTED_GIT_BASE"; fi
        ;;
      *) exit 4 ;;
    esac
    exit 0
    ;;
  merge-base)
    exit "${FAKE_PROTECTED_GIT_ANCESTOR_STATUS:-0}"
    ;;
  status)
    if [ "${FAKE_PROTECTED_GIT_DIRTY:-0}" = "1" ]; then printf ' M tracked.txt\n'; fi
    exit 0
    ;;
  add) exit 0 ;;
  commit)
    printf '%s\n' "$FAKE_PROTECTED_GIT_CANDIDATE" > "$FAKE_PROTECTED_GIT_STATE"
    exit 0
    ;;
  push)
    if [ -n "${FAKE_PROTECTED_GIT_PUSH_STDERR:-}" ]; then
      printf '%s\n' "$FAKE_PROTECTED_GIT_PUSH_STDERR" >&2
    fi
    exit "${FAKE_PROTECTED_GIT_PUSH_STATUS:-0}"
    ;;
  *) exit 5 ;;
esac
`

// fakeProtectedCurl is the recording curl shim. It drains stdin (the --config
// stream that carries authorization), records every argument and the exact PUT
// body, and serves ordered inspect/status responses from the fixture files.
const fakeProtectedCurl = `#!/bin/sh
set -u
for argument in "$@"; do printf '%s\n' "$argument" >> "$CURL_ARGV_LOG"; done
cat >> "$CURL_STDIN_LOG"
output=""
method="GET"
body=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --output=*) output=${1#--output=}; shift ;;
    --write-out|--cacert|--proto|--connect-timeout|--max-time|--header|--config) shift 2 ;;
    --write-out=*|--cacert=*|--proto=*|--connect-timeout=*|--max-time=*|--header=*|--config=*) shift ;;
    --request) method=$2; shift 2 ;;
    --request=*) method=${1#--request=}; shift ;;
    --data-binary) body=${2#@}; shift 2 ;;
    --data-binary=*) body=${1#--data-binary=}; body=${body#@}; shift ;;
    --silent|--show-error) shift ;;
    *) url=$1; shift ;;
  esac
done
if [ "$method" = "GET" ]; then
  count=0
  if [ -f "$CURL_GET_COUNTER" ]; then count=$(cat "$CURL_GET_COUNTER"); fi
  count=$((count + 1))
  printf '%s\n' "$count" > "$CURL_GET_COUNTER"
  source="$CURL_GET_DIR/inspect.$count"
  if [ ! -f "$source" ]; then source="$CURL_GET_DIR/inspect.last"; fi
  if [ -n "$output" ]; then cat "$source" > "$output"; fi
  printf '%s' "${CURL_GET_STATUS:-200}"
  exit 0
fi
count=0
if [ -f "$CURL_PUT_COUNTER" ]; then count=$(cat "$CURL_PUT_COUNTER"); fi
count=$((count + 1))
printf '%s\n' "$count" > "$CURL_PUT_COUNTER"
{
  printf 'PUT %s\n' "$url"
  if [ -n "$body" ] && [ -f "$body" ]; then cat "$body"; fi
  printf '\n'
} >> "$CURL_PUT_LOG"
status_source="$CURL_PUT_DIR/status.$count"
if [ ! -f "$status_source" ]; then status_source="$CURL_PUT_DIR/status.last"; fi
status=$(cat "$status_source")
body_source="$CURL_PUT_DIR/body.$count"
if [ ! -f "$body_source" ]; then body_source="$CURL_PUT_DIR/body.last"; fi
if [ -n "$output" ] && [ -f "$body_source" ]; then cat "$body_source" > "$output"; fi
printf '%s' "$status"
exit 0
`

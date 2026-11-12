package factory

// Authored workload commands for the protected factory golden path.
//
// git_workload.go covers a Shell-launched container cloning and pushing
// through the workload relay with its mounted execution credential, and the
// managed hooks refusing an unapproved protected write. The protected path
// needs three additional actions that stay inside the same execution and
// workspace:
//
//   - implement: clone, verify the seeded base, write a small program and its
//     tests, commit exactly one candidate, push it to a granted work ref and
//     submit the declared `candidate` output;
//   - test: read the retained checkout, require the exact implement candidate,
//     run the real test script and submit `candidate` plus `test_result` only
//     after it succeeds;
//   - integrate: read both settled outputs, require the exact candidate and the
//     matching pass result, verify the base is an ancestor, and push the same
//     candidate to `refs/heads/main` with an explicit
//     `--force-with-lease=refs/heads/main:<B>` precondition and no new commit.
//
// Every command list is authored as data and executed by the product with
// `#!/bin/sh` and `set -e` (dash on the Debian workload image). The functions
// here never rely on a negated pipeline for fail-fast behavior, never put a
// credential in an argument, and never disable TLS verification.
//
// The HTTPS output submitter resolves KODAMA_ACTION (an action slug) to the real
// action ID through execution.inspect, sends the served `expected_version`
// envelope, and retries only an explicit `version_conflict` with freshly read
// state. Authorization reaches curl through `--config -` on stdin; the mounted
// token is read on every request and never written to a workspace file.

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
)

const (
	// protectedWorkloadTokenPath is the product-mounted execution credential.
	protectedWorkloadTokenPath = gitWorkloadTokenPath
	// protectedWorkloadCAFile is where the action materializes the public CA.
	protectedWorkloadCAFile = gitWorkloadCAFile
	// protectedWorkloadWorkspace is the shared action workspace root.
	protectedWorkloadWorkspace = gitWorkloadWorkspace
	// protectedWorkloadRepositoryDirectory is the retained checkout.
	protectedWorkloadRepositoryDirectory = "/workspace/git-protected-repository"
	// protectedWorkloadMarkerPath records the execution that owns this
	// workspace, so a replaced workspace cannot pass the later actions.
	protectedWorkloadMarkerPath = "/workspace/.kodama-workspace-marker"
	// protectedWorkloadOutputsRoot is the product-materialized output root.
	protectedWorkloadOutputsRoot = "/kodama/outputs"
	// protectedWorkloadTaskFile is the product task snapshot path.
	protectedWorkloadTaskFile = "/kodama/task.json"
	// protectedWorkloadProtectedRef is the only protected ref this path moves.
	protectedWorkloadProtectedRef = "refs/heads/main"
	// protectedWorkloadCandidateKey and protectedWorkloadTestResultKey are the
	// declared output keys of the golden path.
	protectedWorkloadCandidateKey  = "candidate"
	protectedWorkloadTestResultKey = "test_result"
	// protectedWorkloadProgramFile and protectedWorkloadTestFile are the
	// deterministic fixture sources committed by the implement action.
	protectedWorkloadProgramFile = "kodama-program.sh"
	protectedWorkloadTestFile    = "kodama-test.sh"
	// protectedWorkloadSubmitAttempts bounds the version-conflict retry loop.
	protectedWorkloadSubmitAttempts = 5
	// Default action names of the shared golden scenario.
	protectedWorkloadDefaultImplementAction = "implement"
	protectedWorkloadDefaultTestAction      = "test"
)

// WorkloadCandidateSpec configures one authored protected-workload action. WorkRef
// and ExpectedBase are supplied by the scenario; the default action names match
// the golden path and may be overridden by a harness smoke test.
type WorkloadCandidateSpec struct {
	// WorkRef is the granted work branch the implement action pushes.
	WorkRef string
	// ExpectedBase is the seeded main commit the checkout must equal and the
	// candidate must build on.
	ExpectedBase string
	// RequireTask adds the reserved task-context checks for a task-associated
	// execution. A task-free smoke execution leaves it false.
	RequireTask bool
	// ImplementAction and TestAction name the earlier actions whose settled
	// outputs the later actions read. Empty selects the golden-path defaults.
	ImplementAction string
	TestAction      string
}

// protectedWorkloadSpec is the complete authored-command input. It carries no
// credential: every command reads the mounted execution token itself.
type protectedWorkloadSpec struct {
	URL          string
	CAPEM        []byte
	TokenPath    string
	CAPath       string
	Workspace    string
	Directory    string
	MarkerPath   string
	OutputsRoot  string
	WorkRef      string
	ProtectedRef string
	ExpectedBase string

	ImplementAction string
	TestAction      string
	RequireTask     bool
}

// WorkloadCandidateCommands authors the implement action: clone through the
// workload relay, verify the seeded base, write a deterministic program and its
// tests, commit one candidate, push it to the granted work ref and submit the
// declared candidate output.
func (fixture *GitFixture) WorkloadCandidateCommands(t testing.TB, spec WorkloadCandidateSpec) ([]string, error) {
	t.Helper()
	internal, err := fixture.protectedSpec(spec)
	if err != nil {
		return nil, err
	}
	return protectedWorkloadCandidateCommands(internal)
}

// WorkloadTestCandidateCommands authors the test action: read the retained
// checkout, require HEAD at the exact implement candidate with clean tracked
// files, run the committed test script and submit only a successful result.
func (fixture *GitFixture) WorkloadTestCandidateCommands(t testing.TB, spec WorkloadCandidateSpec) ([]string, error) {
	t.Helper()
	internal, err := fixture.protectedSpec(spec)
	if err != nil {
		return nil, err
	}
	return protectedWorkloadTestCommands(internal)
}

// WorkloadIntegrateCandidateCommands authors the integrate action: require both
// settled candidate outputs and the matching pass result, verify ancestry and
// push the same candidate to the protected ref with an explicit lease.
func (fixture *GitFixture) WorkloadIntegrateCandidateCommands(t testing.TB, spec WorkloadCandidateSpec) ([]string, error) {
	t.Helper()
	internal, err := fixture.protectedSpec(spec)
	if err != nil {
		return nil, err
	}
	return protectedWorkloadIntegrateCommands(internal)
}

// protectedSpec fills the fixed container paths and validates the caller's
// scenario fields. It performs no effect.
func (fixture *GitFixture) protectedSpec(spec WorkloadCandidateSpec) (protectedWorkloadSpec, error) {
	if fixture == nil || fixture.h == nil || fixture.h.TLS == nil {
		return protectedWorkloadSpec{}, errors.New("protected workload commands need the private-CA TLS fixture")
	}
	internal := protectedWorkloadSpec{
		URL:             fixture.WorkloadRepositoryURL(GitProjectRepository),
		CAPEM:           fixture.h.TLS.CAPEM,
		TokenPath:       protectedWorkloadTokenPath,
		CAPath:          protectedWorkloadCAFile,
		Workspace:       protectedWorkloadWorkspace,
		Directory:       protectedWorkloadRepositoryDirectory,
		MarkerPath:      protectedWorkloadMarkerPath,
		OutputsRoot:     protectedWorkloadOutputsRoot,
		WorkRef:         spec.WorkRef,
		ProtectedRef:    protectedWorkloadProtectedRef,
		ExpectedBase:    spec.ExpectedBase,
		ImplementAction: spec.ImplementAction,
		TestAction:      spec.TestAction,
		RequireTask:     spec.RequireTask,
	}
	if internal.ImplementAction == "" {
		internal.ImplementAction = protectedWorkloadDefaultImplementAction
	}
	if internal.TestAction == "" {
		internal.TestAction = protectedWorkloadDefaultTestAction
	}
	return internal, nil
}

// protectedWorkloadCandidateCommands renders the implement action.
func protectedWorkloadCandidateCommands(spec protectedWorkloadSpec) ([]string, error) {
	if err := validateProtectedWorkloadSpec(spec, true); err != nil {
		return nil, err
	}
	commands := protectedWorkloadPrelude(spec)
	commands = append(commands, protectedWorkloadOutputSubmitter(spec)...)
	if spec.RequireTask {
		commands = append(commands,
			`test -n "$KODAMA_TASK"`,
			"test -s "+shellQuote(protectedWorkloadTaskFile),
		)
	}
	directory := shellQuote(spec.Directory)
	program := shellQuote(protectedWorkloadProgramFile)
	testScript := shellQuote(protectedWorkloadTestFile)
	commands = append(commands,
		"rm -rf -- "+directory,
		"git_kodama clone --quiet "+shellQuote(spec.URL)+" "+directory,
		"cd "+directory,
		// The checkout must be the reviewed base before the candidate exists.
		`test "$(git rev-parse HEAD)" = `+shellQuote(spec.ExpectedBase),
		// The marker names this execution so a replaced workspace is visible.
		"printf '%s\\n' \"$KODAMA_EXECUTION\" > "+shellQuote(spec.MarkerPath),
		"cat > "+program+" <<'KODAMA_PROTECTED_PROGRAM'\n"+protectedWorkloadProgram+"KODAMA_PROTECTED_PROGRAM",
		"chmod 0755 "+program,
		"cat > "+testScript+" <<'KODAMA_PROTECTED_TEST'\n"+protectedWorkloadTest+"KODAMA_PROTECTED_TEST",
		"chmod 0755 "+testScript,
		"git add -- "+program+" "+testScript,
		"git -c user.name=kodama -c user.email=kodama@example.invalid commit --quiet -m 'I05 protected candidate'",
		"kodama_candidate=$(git rev-parse HEAD)",
		`test "$(git rev-parse HEAD^)" = `+shellQuote(spec.ExpectedBase),
		`test "$kodama_candidate" != `+shellQuote(spec.ExpectedBase),
		"git_kodama push --quiet origin "+shellQuote("HEAD:"+spec.WorkRef),
	)
	commands = append(commands, workloadScanCommands(spec.TokenPath, spec.Workspace)...)
	commands = append(commands,
		"kodama_submit_outputs \"candidate=$kodama_candidate\"",
		`printf 'candidate-output-submitted\n'`,
	)
	return commands, nil
}

// protectedWorkloadTestCommands renders the test action.
func protectedWorkloadTestCommands(spec protectedWorkloadSpec) ([]string, error) {
	if err := validateProtectedWorkloadSpec(spec, false); err != nil {
		return nil, err
	}
	implementCandidate := filepath.Join(spec.OutputsRoot, spec.ImplementAction, protectedWorkloadCandidateKey)
	directory := shellQuote(spec.Directory)
	commands := protectedWorkloadPrelude(spec)
	commands = append(commands, protectedWorkloadOutputSubmitter(spec)...)
	commands = append(commands,
		"test -s "+shellQuote(implementCandidate),
		"kodama_test_candidate=$(cat "+shellQuote(implementCandidate)+")",
		"kodama_test_marker=$(cat "+shellQuote(spec.MarkerPath)+")",
		`test "$kodama_test_marker" = "$KODAMA_EXECUTION"`,
		"cd "+directory,
		`test "$(git rev-parse HEAD)" = "$kodama_test_candidate"`,
		"kodama_test_status=$(git status --porcelain --untracked-files=no)",
		`test -z "$kodama_test_status"`,
		"test -f "+shellQuote(protectedWorkloadTestFile),
		"/bin/sh -e "+shellQuote(protectedWorkloadTestFile),
	)
	commands = append(commands, workloadScanCommands(spec.TokenPath, spec.Workspace)...)
	commands = append(commands,
		"kodama_submit_outputs \"candidate=$kodama_test_candidate\" \"test_result=pass:$kodama_test_candidate\"",
		`printf 'test-outputs-submitted\n'`,
	)
	return commands, nil
}

// protectedWorkloadIntegrateCommands renders the integrate action.
func protectedWorkloadIntegrateCommands(spec protectedWorkloadSpec) ([]string, error) {
	if err := validateProtectedWorkloadSpec(spec, true); err != nil {
		return nil, err
	}
	implementCandidate := filepath.Join(spec.OutputsRoot, spec.ImplementAction, protectedWorkloadCandidateKey)
	testCandidate := filepath.Join(spec.OutputsRoot, spec.TestAction, protectedWorkloadCandidateKey)
	testResult := filepath.Join(spec.OutputsRoot, spec.TestAction, protectedWorkloadTestResultKey)
	commands := protectedWorkloadPrelude(spec)
	commands = append(commands,
		"test -s "+shellQuote(implementCandidate),
		"test -s "+shellQuote(testCandidate),
		"test -s "+shellQuote(testResult),
		"kodama_integrate_candidate=$(cat "+shellQuote(implementCandidate)+")",
		`test "$(cat `+shellQuote(testCandidate)+`)" = "$kodama_integrate_candidate"`,
		`test "$(cat `+shellQuote(testResult)+`)" = "pass:$kodama_integrate_candidate"`,
		"kodama_integrate_marker=$(cat "+shellQuote(spec.MarkerPath)+")",
		`test "$kodama_integrate_marker" = "$KODAMA_EXECUTION"`,
		"cd "+shellQuote(spec.Directory),
		`test "$(git rev-parse HEAD)" = "$kodama_integrate_candidate"`,
		`test "$kodama_integrate_candidate" != `+shellQuote(spec.ExpectedBase),
		"if git merge-base --is-ancestor "+shellQuote(spec.ExpectedBase)+` "$kodama_integrate_candidate"; then :; else printf 'candidate-is-not-descended-from-base\n' >&2; exit 1; fi`,
		"git_kodama push --quiet "+shellQuote("--force-with-lease="+spec.ProtectedRef+":"+spec.ExpectedBase)+
			" origin \"$kodama_integrate_candidate\":"+spec.ProtectedRef,
	)
	commands = append(commands, workloadScanCommands(spec.TokenPath, spec.Workspace)...)
	commands = append(commands, `printf 'protected-integration-pushed\n'`)
	return commands, nil
}

// protectedWorkloadSubmitCommands renders the shared HTTPS output submitter and
// one call with the supplied key=value output pairs. It is exported to the
// package's executable unit guards; the action authors call the function
// directly inside their own command lists.
func protectedWorkloadSubmitCommands(spec protectedWorkloadSpec, outputs ...string) ([]string, error) {
	if err := validateProtectedWorkloadSpec(spec, false); err != nil {
		return nil, err
	}
	commands := protectedWorkloadPrelude(spec)
	commands = append(commands, protectedWorkloadOutputSubmitter(spec)...)
	var call strings.Builder
	call.WriteString("kodama_submit_outputs")
	for _, output := range outputs {
		call.WriteString(" " + shellQuote(output))
	}
	return append(commands, call.String()), nil
}

// protectedWorkloadPrelude materializes the public CA and defines the one Git
// authentication function. Every invocation reads the mounted token afresh and
// supplies the derived Basic header only through Git's environment config.
func protectedWorkloadPrelude(spec protectedWorkloadSpec) []string {
	caMaterial := string(spec.CAPEM)
	if !strings.HasSuffix(caMaterial, "\n") {
		caMaterial += "\n"
	}
	return []string{
		"cat > " + shellQuote(spec.CAPath) + " <<'KODAMA_PROTECTED_CA'\n" + caMaterial + "KODAMA_PROTECTED_CA",
		"export GIT_SSL_CAINFO=" + shellQuote(spec.CAPath),
		"export GIT_CONFIG_NOSYSTEM=1",
		"export GIT_CONFIG_GLOBAL=/dev/null",
		"export GIT_ATTR_NOSYSTEM=1",
		"export GIT_TERMINAL_PROMPT=0",
		"export GIT_TRACE=0 GIT_TRACE_PACKET=0 GIT_TRACE_CURL=0 GIT_CURL_VERBOSE=0",
		"git_kodama() {\n" +
			"  " + gitWorkloadAuthEnv + "=\"Authorization: Basic $(printf 'kodama:%s' \"$(cat " + shellQuote(spec.TokenPath) + ")\" | base64 -w0)\" \\\n" +
			"  git -c http.followRedirects=false --config-env=http.extraHeader=" + gitWorkloadAuthEnv + " \"$@\"\n" +
			"}",
	}
	// The submitter function definitions are appended by the caller's authored
	// command list through protectedWorkloadOutputSubmitter.
}

// protectedWorkloadOutputSubmitter renders the HTTPS output submission
// functions. The token is read from its mount on every request and reaches curl
// only through stdin (`--config -`), so it never appears in argv, a URL, a Git
// configuration file or a workspace file.
func protectedWorkloadOutputSubmitter(spec protectedWorkloadSpec) []string {
	token := shellQuote(spec.TokenPath)
	ca := shellQuote(spec.CAPath)
	inspectBody := shellQuote(filepath.Join(spec.Workspace, ".kodama-inspect.json"))
	inspectRequestPath := filepath.Join(spec.Workspace, ".kodama-inspect-request.json")
	inspectRequest := shellQuote(inspectRequestPath)
	responseBody := shellQuote(filepath.Join(spec.Workspace, ".kodama-outputs-response.json"))
	requestBody := shellQuote(filepath.Join(spec.Workspace, ".kodama-outputs.json"))
	dataRequest := shellQuote("@" + inspectRequestPath)
	return []string{
		"kodama_http_get() {\n" +
			"  kodama_http_output=$1\n" +
			"  kodama_http_token=$(cat " + token + ") || return 1\n" +
			"  if [ -z \"$kodama_http_token\" ]; then printf 'kodama-empty-token\\n' >&2; return 1; fi\n" +
			"  printf '%s' '{\"input\":{}}' > " + inspectRequest + "\n" +
			"  kodama_http_status=$(printf 'header = \"Authorization: Bearer %s\"\\n' \"$kodama_http_token\" | " +
			"curl --config - --silent --show-error --proto '=https' --cacert " + ca +
			" --connect-timeout 10 --max-time 30 --request GET --header 'Content-Type: application/json' " +
			"--data-binary " + dataRequest + " --output \"$kodama_http_output\" --write-out '%{http_code}' " +
			"\"$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION\") || { printf 'kodama-inspect-transport-failed\\n' >&2; return 1; }\n" +
			"  printf '%s' \"$kodama_http_status\"\n" +
			"}",
		"kodama_http_put() {\n" +
			"  kodama_put_body=$1\n" +
			"  kodama_put_output=$2\n" +
			"  kodama_put_action=$3\n" +
			"  kodama_put_token=$(cat " + token + ") || return 1\n" +
			"  if [ -z \"$kodama_put_token\" ]; then printf 'kodama-empty-token\\n' >&2; return 1; fi\n" +
			"  kodama_put_status=$(printf 'header = \"Authorization: Bearer %s\"\\n' \"$kodama_put_token\" | " +
			"curl --config - --silent --show-error --proto '=https' --cacert " + ca +
			" --connect-timeout 10 --max-time 30 --header 'Content-Type: application/json' --request PUT " +
			"--data-binary \"@$kodama_put_body\" --output \"$kodama_put_output\" --write-out '%{http_code}' " +
			"\"$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION/actions/$kodama_put_action/outputs\") || { printf 'kodama-outputs-transport-failed\\n' >&2; return 1; }\n" +
			"  printf '%s' \"$kodama_put_status\"\n" +
			"}",
		"kodama_execution_version() {\n" +
			"  tr ',' '\\n' < \"$1\" | sed -n 's/^\"version\":\\([0-9][0-9]*\\)$/\\1/p' | head -n 1\n" +
			"}",
		"kodama_action_line() {\n" +
			"  sed 's/{\"action_id\":/\\n{\"action_id\":/g' \"$1\" | grep -F '\"name\":\"'\"$2\"'\"' | head -n 1\n" +
			"}",
		"kodama_action_id() {\n" +
			"  printf '%s\\n' \"$1\" | tr ',' '\\n' | sed -n 's/^{\"action_id\":\"\\([A-Za-z0-9_-]*\\)\"$/\\1/p' | head -n 1\n" +
			"}",
		"kodama_write_outputs_body() {\n" +
			"  kodama_write_file=$1\n" +
			"  kodama_write_version=$2\n" +
			"  kodama_write_action=$3\n" +
			"  shift 3\n" +
			"  kodama_write_outputs=\n" +
			"  for kodama_write_pair in \"$@\"; do\n" +
			"    kodama_write_key=${kodama_write_pair%%=*}\n" +
			"    kodama_write_value=${kodama_write_pair#*=}\n" +
			"    case \"$kodama_write_key\" in\n" +
			"      [a-z]*) ;;\n" +
			"      *) printf 'kodama-invalid-output-key\\n' >&2; return 1 ;;\n" +
			"    esac\n" +
			"    case \"$kodama_write_key\" in\n" +
			"      *[!a-z0-9_-]*) printf 'kodama-invalid-output-key\\n' >&2; return 1 ;;\n" +
			"    esac\n" +
			"    case \"$kodama_write_value\" in\n" +
			"      *[!A-Za-z0-9._:/+@=-]*) printf 'kodama-invalid-output-value\\n' >&2; return 1 ;;\n" +
			"    esac\n" +
			"    if [ -n \"$kodama_write_outputs\" ]; then kodama_write_outputs=\"$kodama_write_outputs,\"; fi\n" +
			"    kodama_write_outputs=\"$kodama_write_outputs\\\"$kodama_write_key\\\":\\\"$kodama_write_value\\\"\"\n" +
			"  done\n" +
			"  kodama_write_execution=$(printf '%s' \"$KODAMA_EXECUTION\" | tr '_' '-')\n" +
			"  kodama_write_key=\"outputs-$(printf '%s' \"$kodama_write_execution\" | cut -c1-48)-$KODAMA_ACTION\"\n" +
			"  printf '{\"expected_version\":%s,\"idempotency_key\":\"%s\",\"input\":{\"execution_id\":\"%s\",\"action_id\":\"%s\",\"outputs\":{%s}}}' " +
			"\"$kodama_write_version\" \"$kodama_write_key\" \"$KODAMA_EXECUTION\" \"$kodama_write_action\" \"$kodama_write_outputs\" > \"$kodama_write_file\"\n" +
			"}",
		"kodama_submit_outputs() {\n" +
			"  case \"$KODAMA_EXECUTION\" in\n" +
			"    ''|*[!A-Za-z0-9_-]*) printf 'kodama-invalid-execution\\n' >&2; return 1 ;;\n" +
			"  esac\n" +
			"  case \"$KODAMA_ACTION\" in\n" +
			"    [a-z]*) ;;\n" +
			"    *) printf 'kodama-invalid-action\\n' >&2; return 1 ;;\n" +
			"  esac\n" +
			"  case \"$KODAMA_ACTION\" in\n" +
			"    *[!a-z0-9-]*) printf 'kodama-invalid-action\\n' >&2; return 1 ;;\n" +
			"  esac\n" +
			"  kodama_submit_attempt=0\n" +
			"  while [ \"$kodama_submit_attempt\" -lt " + fmt.Sprint(protectedWorkloadSubmitAttempts) + " ]; do\n" +
			"    kodama_submit_attempt=$((kodama_submit_attempt + 1))\n" +
			"    kodama_submit_status=$(kodama_http_get " + inspectBody + ") || return 1\n" +
			"    if [ \"$kodama_submit_status\" != \"200\" ]; then\n" +
			"      printf 'kodama-inspect-status %s\\n' \"$kodama_submit_status\" >&2\n" +
			"      cat " + inspectBody + " >&2\n" +
			"      rm -f " + inspectBody + "\n" +
			"      return 1\n" +
			"    fi\n" +
			"    kodama_submit_execution_version=$(kodama_execution_version " + inspectBody + ")\n" +
			"    kodama_submit_action_line=$(kodama_action_line " + inspectBody + " \"$KODAMA_ACTION\")\n" +
			"    kodama_submit_action_id=$(kodama_action_id \"$kodama_submit_action_line\")\n" +
			"    if [ -z \"$kodama_submit_execution_version\" ] || [ -z \"$kodama_submit_action_id\" ]; then\n" +
			"      printf 'kodama-inspect-unusable\\n' >&2\n" +
			"      rm -f " + inspectBody + "\n" +
			"      return 1\n" +
			"    fi\n" +
			"    kodama_write_outputs_body " + requestBody + " \"$kodama_submit_execution_version\" \"$kodama_submit_action_id\" \"$@\" || { rm -f " + inspectBody + " " + requestBody + "; return 1; }\n" +
			"    kodama_submit_put_status=$(kodama_http_put " + requestBody + " " + responseBody + " \"$kodama_submit_action_id\") || { rm -f " + inspectBody + " " + requestBody + "; return 1; }\n" +
			"    rm -f " + requestBody + "\n" +
			"    case \"$kodama_submit_put_status\" in\n" +
			"      200)\n" +
			"        rm -f " + inspectBody + " " + responseBody + "\n" +
			"        return 0\n" +
			"        ;;\n" +
			"      409)\n" +
			"        grep -q '\"code\":\"version_conflict\"' " + responseBody + "\n" +
			"        kodama_submit_conflict_status=$?\n" +
			"        if [ \"$kodama_submit_conflict_status\" -ne 0 ]; then\n" +
			"          printf 'kodama-outputs-conflict-unidentified\\n' >&2\n" +
			"          rm -f " + inspectBody + " " + responseBody + "\n" +
			"          return 1\n" +
			"        fi\n" +
			"        ;;\n" +
			"      *)\n" +
			"        printf 'kodama-outputs-status %s\\n' \"$kodama_submit_put_status\" >&2\n" +
			"        rm -f " + inspectBody + " " + responseBody + "\n" +
			"        return 1\n" +
			"        ;;\n" +
			"    esac\n" +
			"  done\n" +
			"  printf 'kodama-outputs-retries-exhausted\\n' >&2\n" +
			"  rm -f " + inspectBody + " " + responseBody + "\n" +
			"  return 1\n" +
			"}",
	}
}

// validateProtectedWorkloadSpec refuses an unsafe command input before any
// command is rendered.
func validateProtectedWorkloadSpec(spec protectedWorkloadSpec, requireBase bool) error {
	if err := validateWorkloadGitURL(spec.URL); err != nil {
		return err
	}
	if err := validateWorkloadCAPEM(spec.CAPEM); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"token": spec.TokenPath, "CA": spec.CAPath, "workspace": spec.Workspace,
		"directory": spec.Directory, "marker": spec.MarkerPath, "outputs root": spec.OutputsRoot,
	} {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("protected workload %s path %q is not absolute", name, value)
		}
	}
	workspace := strings.TrimSuffix(spec.Workspace, "/")
	if !strings.HasPrefix(spec.Directory, workspace+"/") {
		return fmt.Errorf("protected workload directory %q is not inside %q", spec.Directory, spec.Workspace)
	}
	if !strings.HasPrefix(spec.WorkRef, "refs/heads/work/") {
		return fmt.Errorf("protected workload ref %q is not a granted work branch", spec.WorkRef)
	}
	if spec.ProtectedRef != protectedWorkloadProtectedRef {
		return fmt.Errorf("protected workload ref %q is not the protected main ref", spec.ProtectedRef)
	}
	if _, err := id.ParseActionName(spec.ImplementAction); err != nil {
		return fmt.Errorf("protected workload implement action: %w", err)
	}
	if _, err := id.ParseActionName(spec.TestAction); err != nil {
		return fmt.Errorf("protected workload test action: %w", err)
	}
	if spec.ExpectedBase == "" {
		if requireBase {
			return errors.New("protected workload commands need the reviewed base commit")
		}
	} else if !validProtectedCommit(spec.ExpectedBase) {
		return fmt.Errorf("protected workload base %q is not an immutable commit identity", spec.ExpectedBase)
	}
	return nil
}

// validProtectedCommit accepts the lowercase hexadecimal object identity Git
// prints for a SHA-1 or SHA-256 repository.
func validProtectedCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

// protectedWorkloadProgram and protectedWorkloadTest are the deterministic
// fixture sources the implement action commits. The program is a real arithmetic
// helper; the test exercises a positive and two boundary vectors.
const protectedWorkloadProgram = `#!/bin/sh
set -eu
if [ "$#" -ne 2 ]; then
  printf 'usage: %s <left> <right>\n' "$0" >&2
  exit 2
fi
printf '%s\n' "$(( $1 + $2 ))"
`

// protectedWorkloadTest exercises the committed program. `set -eu` matches the
// product's action semantics: any wrong answer stops the test action before the
// output submitter runs.
const protectedWorkloadTest = `#!/bin/sh
set -eu
test "$(./kodama-program.sh 2 3)" = "5"
test "$(./kodama-program.sh 0 0)" = "0"
test "$(./kodama-program.sh -1 1)" = "0"
`

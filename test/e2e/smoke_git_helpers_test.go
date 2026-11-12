//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// This file holds the host-side Git and accepted-report plumbing shared by S2
// (protected task) and S5 (human push). The host engineer/administrator uses a
// private HOME, no inherited configuration, a token-reading askpass and the
// descriptor's CA; the token never reaches argv, a remote URL or repository
// configuration. Nothing here reads the database, journal or filesystem state
// of the installation.

// hostGit is one private host Git identity over the served HTTPS endpoint.
type hostGit struct {
	root      string
	dir       string
	home      string
	askpass   string
	origin    string
	tokenFile string
	caFile    string
	secrets   []harness.Secret
}

// hostGitAskpass answers only a credential prompt for the configured served
// origin (scheme, host and port). Git asks the username as
// "Username for 'https://host:port'" and the password as
// "Password for 'https://kodama@host:port'"; the second form is scoped by the
// same host:port with a closing quote. Any other prompt gets an empty answer,
// so the persona token never reaches another origin.
const hostGitAskpass = `#!/bin/sh
W49_GIT_AUTHORITY=${W49_GIT_ORIGIN#*://}
case "$1" in
*"${W49_GIT_ORIGIN:?}'"*|*"@${W49_GIT_AUTHORITY:?}'"*)
	case "$1" in
	*[Uu]sername*) printf 'kodama\n' ;;
	*) cat -- "${W49_GIT_TOKEN_FILE:?}" ;;
	esac
	;;
esac
`

// newHostGit prepares a fresh private HOME, an askpass helper that reads the
// persona token file, and the descriptor CA.
func (instance *live) newHostGit(t *testing.T, persona harness.Persona, label string) *hostGit {
	t.Helper()
	tokenFile := instance.config.PersonaTokenFiles[persona]
	if tokenFile == "" {
		t.Fatalf("the %s persona has no private token file; the host Git probe cannot authenticate", persona)
	}
	root := filepath.Join(t.TempDir(), label)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the private host Git root: %v", err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create the private host Git HOME: %v", err)
	}
	askpass := filepath.Join(root, "askpass.sh")
	if err := os.WriteFile(askpass, []byte(hostGitAskpass), 0o700); err != nil {
		t.Fatalf("write the host Git askpass helper: %v", err)
	}
	return &hostGit{
		root:      root,
		dir:       filepath.Join(root, "work"),
		home:      home,
		askpass:   askpass,
		origin:    strings.TrimSuffix(instance.config.GitURL(), "/"),
		tokenFile: tokenFile,
		caFile:    instance.config.GitCAFile(),
		secrets:   append(instance.config.PersonaSecrets(), instance.config.Token),
	}
}

// environment is the minimal host Git environment. It inherits only PATH and
// the launcher's TMPDIR, never ambient Git or askpass configuration.
func (git *hostGit) environment() []string {
	environment := []string{}
	for _, name := range []string{"PATH", "TMPDIR", "LANG"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return append(environment,
		"HOME="+git.home,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS="+git.askpass,
		"GIT_ASKPASS_REQUIRE=force",
		"W49_GIT_ORIGIN="+git.origin,
		"W49_GIT_TOKEN_FILE="+git.tokenFile,
	)
}

// run executes one host Git command. The token file path is the only credential
// input; the askpass script reads it.
func (git *hostGit) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	base := []string{"-c", "credential.helper=", "-c", "http.sslVerify=true", "-c", "http.followRedirects=false"}
	if git.caFile != "" {
		base = append(base, "-c", "http.sslCAInfo="+git.caFile)
	}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	base = append(base, args...)
	command := exec.CommandContext(ctx, "git", base...)
	command.Env = git.environment()
	output, err := command.CombinedOutput()
	if err != nil {
		if hostGitAuthenticationRefused(output) {
			return output, fmt.Errorf("host Git authentication was refused (HTTP 401) for %s: the persona token has expired or been revoked; installed OAuthSonas tokens live about 899 seconds", filepath.Base(git.root))
		}
		return output, fmt.Errorf("git %s (host %s): %w", strings.Join(args, " "), filepath.Base(git.root), err)
	}
	return output, nil
}

// hostGitAuthenticationRefused reports the host Git credential/401 failure
// shapes. It is deliberately narrow: a policy refusal (update rejected) and a
// transport failure are not credential expiry.
func hostGitAuthenticationRefused(output []byte) bool {
	text := strings.ToLower(string(output))
	for _, marker := range []string{
		"authentication failed",
		"could not read username",
		"http 401",
		"401 unauthorized",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// mustRun executes one host Git command and fails the test on error.
func (git *hostGit) mustRun(ctx context.Context, t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	output, err := git.run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("%v; output=%s", err, scanSafeHostGitOutput(output, git.secrets))
	}
	return output
}

// clone clones one served repository. An empty repository still clones with
// exit zero.
func (git *hostGit) clone(ctx context.Context, t *testing.T, url, directory string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		t.Fatalf("create the host checkout parent: %v", err)
	}
	git.mustRun(ctx, t, "", "clone", "--quiet", url, directory)
}

// init initialises one empty local repository.
func (git *hostGit) init(ctx context.Context, t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create the host repository: %v", err)
	}
	git.mustRun(ctx, t, "", "init", "--quiet", directory)
}

// commit writes one file and commits it, returning the new commit identity.
func (git *hostGit) commit(ctx context.Context, t *testing.T, directory, name, content, message string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the host commit file: %v", err)
	}
	git.mustRun(ctx, t, directory, "add", "--", name)
	git.mustRun(ctx, t, directory,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "--quiet", "-m", message)
	return git.revParse(ctx, t, directory, "HEAD")
}

// revParse returns one commit identity or fails the test.
func (git *hostGit) revParse(ctx context.Context, t *testing.T, directory, revision string) string {
	t.Helper()
	if revision == "HEAD" {
		if _, err := git.run(ctx, directory, "rev-parse", "--verify", "HEAD"); err != nil {
			return ""
		}
	}
	return strings.TrimSpace(string(git.mustRun(ctx, t, directory, "rev-parse", revision)))
}

// push pushes one refspec and returns the combined output with any error.
func (git *hostGit) push(ctx context.Context, directory, url string, refspecs ...string) ([]byte, error) {
	args := append([]string{"push", "--quiet", url}, refspecs...)
	return git.run(ctx, directory, args...)
}

// lsRemote returns the commit a served ref points at, or empty when absent.
func (git *hostGit) lsRemote(ctx context.Context, t *testing.T, url, ref string) string {
	t.Helper()
	output := strings.TrimSpace(string(git.mustRun(ctx, t, "", "ls-remote", "--refs", url, ref)))
	if output == "" {
		return ""
	}
	fields := strings.Fields(output)
	if len(fields) < 2 || fields[1] != ref {
		t.Fatalf("ls-remote %s returned an unusable advertisement: %q", ref, output)
	}
	return fields[0]
}

// fetch fetches one served ref and returns FETCH_HEAD's commit.
func (git *hostGit) fetch(ctx context.Context, t *testing.T, directory, url, ref string) string {
	t.Helper()
	git.mustRun(ctx, t, directory, "fetch", "--quiet", url, ref)
	return strings.TrimSpace(string(git.mustRun(ctx, t, directory, "rev-parse", "FETCH_HEAD")))
}

// parent returns the first parent commit of one fetched commit.
func (git *hostGit) parent(ctx context.Context, t *testing.T, directory, revision string) string {
	t.Helper()
	return strings.TrimSpace(string(git.mustRun(ctx, t, directory, "rev-parse", revision+"^")))
}

// scanSafeHostGitOutput bounds a host Git output for a failure message after
// scanning it for configured credentials.
func scanSafeHostGitOutput(output []byte, secrets []harness.Secret) string {
	if err := harness.ScanCredentials(string(output), secrets...); err != nil {
		return "withheld: output contained configured credential material"
	}
	text := strings.TrimSpace(string(output))
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

// smokeRepositoryURL joins the served Git origin and one repository name.
func smokeRepositoryURL(origin, repository string) string {
	return strings.TrimSuffix(origin, "/") + "/" + strings.TrimPrefix(repository, "/")
}

// smokeAdminRepository returns the tenant admin repository from the descriptor.
func (instance *live) smokeAdminRepository(t *testing.T) harness.GitRepository {
	t.Helper()
	admin := instance.config.Installation.Git.AdminResourceID
	for _, repository := range instance.config.Installation.Git.Repositories {
		if repository.Name == "admin" || (admin != "" && repository.ResourceID == admin) {
			return repository
		}
	}
	t.Fatalf("the installation descriptor names no admin repository (admin resource %q)", admin)
	return harness.GitRepository{}
}

// smokeGitInputs returns the ordinary repository of one scenario slot and the
// admin repository, naming every missing Git input the Git scenarios need.
func (instance *live) smokeGitInputs(t *testing.T, slot int) (harness.GitRepository, harness.GitRepository) {
	t.Helper()
	repository, ok := instance.config.RepositoryForSlot(slot)
	if !ok || repository.Name == "" || repository.ResourceID == "" {
		t.Fatal("the installation descriptor names no smoke repository")
	}
	if instance.config.GitURL() == "" || instance.config.GitWorkloadURL() == "" {
		t.Fatal("the installation descriptor names no host and workload Git endpoint")
	}
	if instance.config.GitServiceID() == "" {
		t.Fatal("the installation descriptor names no Git service identity")
	}
	return repository, instance.smokeAdminRepository(t)
}

// smokePipelineExecutionsResult lists every execution of one pipeline, following
// pagination. Failures are classified so a 401 aborts loudly.
func (instance *live) smokePipelineExecutionsResult(ctx context.Context, token harness.Secret, pipelineID string) ([]api.ExecutionSummary, error) {
	summaries := []api.ExecutionSummary{}
	cursor := ""
	for range 64 {
		request := api.ExecutionListRequest{
			ProjectID:  instance.projectID,
			PipelineID: pipelineID,
			Page:       &api.Page{Limit: api.MaxPageLimit},
		}
		if cursor != "" {
			request.Page.Cursor = cursor
		}
		result, err := instance.invoke(ctx, token, api.MethodGet, "execution.list", request, "", nil)
		if err != nil {
			classified, _ := smoke.Classify("execution.list", err)
			return nil, classified
		}
		var decoded api.ExecutionListResult
		if err := decodeResultBody(result, &decoded); err != nil {
			return nil, fmt.Errorf("decode execution.list: %w", err)
		}
		summaries = append(summaries, decoded.Executions...)
		if decoded.NextCursor == "" {
			return summaries, nil
		}
		cursor = decoded.NextCursor
	}
	return nil, fmt.Errorf("execution.list did not finish within 64 pages")
}

// smokePipelineExecutions lists every execution of one pipeline.
func (instance *live) smokePipelineExecutions(ctx context.Context, t *testing.T, token harness.Secret, pipelineID string) []api.ExecutionSummary {
	t.Helper()
	summaries, err := instance.smokePipelineExecutionsResult(ctx, token, pipelineID)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return summaries
}

// smokeWaitRequestInbox waits for the exact request action in one persona's
// inbox.
func (instance *live) smokeWaitRequestInbox(ctx context.Context, t *testing.T, token harness.Secret, executionID, actionID string, timeout time.Duration) api.InboxItem {
	t.Helper()
	item, _, err := smoke.Wait(ctx, 250*time.Millisecond, timeout, "task.inbox", func(ctx context.Context) (api.InboxItem, error) {
		result, err := instance.invoke(ctx, token, api.MethodGet, "task.inbox", api.TaskInboxRequest{
			ProjectID: instance.projectID,
			Kinds:     []string{"request"},
		}, "", nil)
		if err != nil {
			classified, _ := smoke.Classify("task.inbox", err)
			return api.InboxItem{}, classified
		}
		var inbox api.TaskInboxResult
		if err := decodeResultBody(result, &inbox); err != nil {
			return api.InboxItem{}, fmt.Errorf("decode task.inbox: %w", err)
		}
		found, ok := smokeFindRequestInbox(inbox, executionID, actionID)
		if !ok {
			return api.InboxItem{}, fmt.Errorf("the request is not yet in the inbox")
		}
		return found, nil
	}, func(api.InboxItem) bool { return true })
	if err != nil {
		t.Fatalf("the reviewer request for %s/%s did not appear: %v", executionID, actionID, err)
	}
	return item
}

// smokeWaitReport polls event.read for the exact accepted verified report.
func (instance *live) smokeWaitReport(ctx context.Context, t *testing.T, token harness.Secret, want smoke.ReportWant, timeout time.Duration) api.Event {
	t.Helper()
	events, _, err := smoke.Wait(ctx, 500*time.Millisecond, timeout, "event.read", func(ctx context.Context) ([]api.Event, error) {
		return instance.smokeEventReadResult(ctx, token, "resource.verified")
	}, func(events []api.Event) bool {
		_, ok := smoke.FindReport(events, want)
		return ok
	})
	if err != nil {
		t.Fatalf("the accepted report for resource=%s ref=%s candidate=%s did not appear: %v", want.Resource, want.Ref, want.Candidate, err)
	}
	event, ok := smoke.FindReport(events, want)
	if !ok {
		t.Fatalf("the accepted report for %s/%s disappeared from the page", want.Resource, want.Ref)
	}
	return event
}

// smokeSubstitution rewrites one placeholder in the Python output helper.
// The shell expression is evaluated in the workload after the candidate exists.
type smokeSubstitution struct {
	Placeholder string
	Shell       string
}

// smokeOutputSubmissionCommands reuses the Python output helper with values
// computed inside the workload. The helper is generated with bounded
// placeholders (its own validation still applies), then each placeholder is
// replaced with the shell expression before the helper runs. An identical
// retry and the version-conflict handling stay exactly the helper's.
func smokeOutputSubmissionCommands(t *testing.T, outputs map[string]string, caFile string, substitutions []smokeSubstitution) []string {
	t.Helper()
	script, err := harness.OutputSubmissionScript(outputs, caFile)
	if err != nil {
		t.Fatalf("build the output submission helper: %v", err)
	}
	const (
		submitPath = `"${TMPDIR:-/tmp}/kodama-e2e-submit.$$.py"`
		run        = "\npython3 " + submitPath
	)
	index := strings.LastIndex(script, run)
	if index < 0 {
		t.Fatalf("the output submission helper no longer ends with a runnable python3 command")
	}
	commands := []string{script[:index]}
	for _, substitution := range substitutions {
		if substitution.Placeholder == "" || strings.ContainsAny(substitution.Placeholder, "|&\\/\"'") {
			t.Fatalf("output placeholder %q is not a safe literal", substitution.Placeholder)
		}
		if strings.ContainsAny(substitution.Shell, "|&\\\"'") {
			t.Fatalf("output substitution %q is not a safe shell value", substitution.Shell)
		}
		if !strings.Contains(script, substitution.Placeholder) {
			t.Fatalf("the generated output helper does not contain the placeholder %q", substitution.Placeholder)
		}
		commands = append(commands, `sed -i "s|`+substitution.Placeholder+`|`+substitution.Shell+`|g" `+submitPath)
	}
	return append(commands, "python3 "+submitPath)
}

// smokeLogValue returns one exact `name=value` line from a retained log.
func smokeLogValue(log []byte, name string) (string, bool) {
	prefix := name + "="
	for raw := range strings.SplitSeq(string(log), "\n") {
		line := strings.TrimSpace(raw)
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return after, true
		}
	}
	return "", false
}

//go:build integration

package factorytest

// Factory S11 row AC7: the Git credential helper is an origin-scoped credential
// source, never authority.
//
// The test builds the real `kodactl` command and answers helper `get` requests
// with the real kodactl configuration parser, the real mounted-credential file
// source and the registered served Git origin. The helper returns the live
// execution credential only for its registered repository origin and never for a
// lookalike path, a sibling repository, an insecure scheme or an unregistered
// host. The returned credential is then used against the real served smart-HTTP
// edge: it carries exactly the execution's policy authority (a granted work ref
// succeeds, the approval-guarded main ref is refused), and canceling the
// execution revokes it on the edge even though the helper's local answer is
// unchanged. A fresh execution gets a fresh helper answer. Service credentials
// are never helper sessions.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/userland/cli"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	credentialHelperWorkRef = "refs/heads/work/credential-helper"
)

// TestFactoryGitCredentialHelperAuthority proves the helper's origin boundary
// and that its answer is current-authority-bearing, not a bearer capability.
func TestFactoryGitCredentialHelperAuthority(t *testing.T) {
	fixture := startGitHarness(t)
	binary := buildCredentialHelperBinary(t)

	executionID, executionToken := fixture.startExecution(t, "credential-helper")
	repositoryURL := fixture.git.HostURL + "/" + factory.GitProjectRepository + ".git"
	tokenPath := writeCredentialHelperToken(t, executionToken)
	configPath := writeCredentialHelperConfig(t, fixture.git.HostURL, tokenPath, repositoryURL)

	// The trusted request returns exactly the mounted execution credential.
	stdout, stderr := runCredentialHelper(t, binary, configPath, repositoryURL)
	if stderr != "" {
		t.Fatalf("the trusted helper request wrote diagnostics: %s", stderr)
	}
	if want := "username=kodama\npassword=" + executionToken + "\n"; stdout != want {
		t.Fatalf("trusted helper response = %q; want username kodama and the live execution credential", stdout)
	}
	helperPassword := credentialHelperPassword(t, stdout)

	// Every lookalike or unregistered target is an empty success. The path
	// boundary and the host/port/scheme checks are the helper's own trust
	// boundary; the edge still decides whether the credential has authority.
	for _, target := range []string{
		repositoryURL + ".evil",
		fixture.git.HostURL + "/project-i04/other.git",
		"https://git.invalid/" + factory.GitProjectRepository + ".git",
		"http://" + strings.TrimPrefix(fixture.git.HostURL, "https://") + "/" + factory.GitProjectRepository + ".git",
	} {
		stdout, stderr := runCredentialHelper(t, binary, configPath, target)
		if stdout != "" || stderr != "" {
			t.Fatalf("unregistered helper target %q returned %q / %q; want an empty success", target, stdout, stderr)
		}
	}
	// store and erase never answer with a credential.
	for _, protocol := range []string{"store", "erase"} {
		command := exec.CommandContext(t.Context(), binary, "--config", configPath, "helper", protocol, repositoryURL)
		command.Env = credentialHelperEnvironment(t)
		var out, errOut bytes.Buffer
		command.Stdout = &out
		command.Stderr = &errOut
		if err := command.Run(); err != nil {
			t.Fatalf("helper %s: %v", protocol, err)
		}
		if out.Len() != 0 || errOut.Len() != 0 {
			t.Fatalf("helper %s returned %q / %q; want nothing", protocol, out.String(), errOut.String())
		}
	}

	// The helper-returned password is the live execution credential: the
	// granted work ref succeeds and the approval-guarded main ref is refused.
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "credential-helper")
	candidate := fixture.commit(t, directory, "helper candidate")
	if text, code := fixture.push(directory, helperPassword, "HEAD:"+credentialHelperWorkRef); code != 0 {
		t.Fatalf("helper-credential work push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, credentialHelperWorkRef); remote != candidate {
		t.Fatalf("helper work ref = %q; want %q", remote, candidate)
	}
	workJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, credentialHelperWorkRef, candidate)
	if workJournal.VerificationID == "" {
		t.Fatalf("helper work push has no verification identity")
	}

	mainBefore := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	text, code := fixture.push(directory, helperPassword, "HEAD:"+gitMainRef)
	text = requireRefusal(t, "helper-credential protected push", text, code)
	if !strings.Contains(text, "kodama git: update rejected") {
		t.Fatalf("the helper credential did not reach the managed main hook: %s", text)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != mainBefore {
		t.Fatalf("protected main = %q after the helper push; want the unchanged %q", main, mainBefore)
	}
	requireProtectedRefNoJournal(t, fixture, gitMainRef, candidate)

	// Cancel the execution. The helper's local token file and answer are
	// unchanged, but the served Shell and Git edge both refuse the former
	// credential immediately.
	cancelProtectedRefExecution(t, fixture, executionID)
	if _, publicError := fixture.REST(executionToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, ""); publicError.Code != api.CodeUnauthenticated {
		t.Fatalf("cancelled execution credential code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeUnauthenticated)
	}
	stdout, _ = runCredentialHelper(t, binary, configPath, repositoryURL)
	if !strings.Contains(stdout, "password="+executionToken) {
		t.Fatalf("the helper did not return its local (now revoked) answer: %q", stdout)
	}
	stalePassword := credentialHelperPassword(t, stdout)
	requireProtectedEdgeGitRefusal(t, fixture, stalePassword, "credential-helper-revoked")

	revokedCandidate := fixture.commit(t, directory, "helper revoked candidate")
	text, code = fixture.push(directory, stalePassword, "HEAD:"+credentialHelperWorkRef)
	requireRefusal(t, "helper-credential push after revocation", text, code)
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, credentialHelperWorkRef); remote != candidate {
		t.Fatalf("work ref moved after revocation: %q; want %q", remote, candidate)
	}
	requireProtectedRefNoJournal(t, fixture, credentialHelperWorkRef, revokedCandidate)

	// A fresh execution gets a fresh answer, and that answer works.
	freshID, freshToken := fixture.startExecution(t, "credential-helper-fresh")
	if freshID == executionID || freshToken == executionToken {
		t.Fatalf("the fresh execution reused identity or credential")
	}
	writeCredentialHelperTokenValue(t, tokenPath, freshToken)
	stdout, _ = runCredentialHelper(t, binary, configPath, repositoryURL)
	if want := "username=kodama\npassword=" + freshToken + "\n"; stdout != want {
		t.Fatalf("fresh helper response = %q; want the fresh execution credential", stdout)
	}
	freshPassword := credentialHelperPassword(t, stdout)
	freshCommit := fixture.commit(t, directory, "helper fresh control")
	if text, code := fixture.push(directory, freshPassword, "HEAD:"+credentialHelperWorkRef); code != 0 {
		t.Fatalf("fresh helper-credential push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, credentialHelperWorkRef); remote != freshCommit {
		t.Fatalf("fresh helper work ref = %q; want %q", remote, freshCommit)
	}

	// A service credential is not a helper session even for the trusted origin.
	serviceToken, err := fixture.git.ServiceToken(t)
	if err != nil {
		t.Fatalf("read the Git service credential: %v", err)
	}
	now := time.Now()
	helperConfig := cli.Config{
		ServerURL:      fixture.git.HostURL,
		CredentialFile: tokenPath,
		TrustedOrigins: []string{repositoryURL},
	}
	request := cli.HelperRequest{Protocol: "get", URL: repositoryURL}
	serviceAnswer, err := cli.HelperDecision(helperConfig, request,
		client.Credential{Kind: client.KindService, Token: serviceToken, ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatalf("HelperDecision(service): %v", err)
	}
	if serviceAnswer.Username != "" || serviceAnswer.Password != "" {
		t.Fatalf("the service credential became a helper answer: %+v", serviceAnswer)
	}
	executionAnswer, err := cli.HelperDecision(helperConfig, request,
		client.Credential{Kind: client.KindExecution, Token: freshToken}, now)
	if err != nil {
		t.Fatalf("HelperDecision(execution control): %v", err)
	}
	if executionAnswer.Username != "kodama" || executionAnswer.Password != freshToken {
		t.Fatalf("execution helper control = %+v; want username kodama and the fresh token", executionAnswer)
	}
	if rendered := executionAnswer.String(); strings.Contains(rendered, freshToken) {
		t.Fatalf("HelperResponse.String() leaks the token: %s", rendered)
	}
}

// buildCredentialHelperBinary builds the real kodactl command from this
// checkout into the test's private directory.
func buildCredentialHelperBinary(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve the test working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("no go.mod above the test working directory")
		}
		root = parent
	}
	binary := filepath.Join(t.TempDir(), "kodactl")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/kodactl")
	command.Dir = root
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build kodactl: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return binary
}

// credentialHelperEnvironment isolates the helper from the caller's HOME and
// kodama configuration. The explicit --config supplies everything it needs.
func credentialHelperEnvironment(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
	}
}

// runCredentialHelper runs one real helper get request and returns stdout and
// stderr.
func runCredentialHelper(t *testing.T, binary, configPath, target string) (string, string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binary, "--config", configPath, "helper", "get", target)
	command.Env = credentialHelperEnvironment(t)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run the credential helper for %q: %v: %s", target, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// credentialHelperPassword extracts the password line from one helper answer.
func credentialHelperPassword(t *testing.T, stdout string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "username=kodama" || !strings.HasPrefix(lines[1], "password=") {
		t.Fatalf("helper answer %q is not the exact username/password shape", stdout)
	}
	password := strings.TrimPrefix(lines[1], "password=")
	if password == "" {
		t.Fatalf("helper answer %q carries an empty password", stdout)
	}
	return password
}

// writeCredentialHelperConfig writes one real kodactl configuration whose only
// trusted origin is the exact served repository URL.
func writeCredentialHelperConfig(t *testing.T, serverURL, tokenPath, repositoryURL string) string {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"server_url":      serverURL,
		"credential_file": tokenPath,
		"trusted_origins": []string{repositoryURL},
	})
	if err != nil {
		t.Fatalf("marshal the helper configuration: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatalf("write the helper configuration: %v", err)
	}
	return path
}

// writeCredentialHelperToken writes one mounted credential file and returns its
// path.
func writeCredentialHelperToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "KODAMA_TOKEN")
	writeCredentialHelperTokenValue(t, path, token)
	return path
}

// writeCredentialHelperTokenValue replaces one mounted credential file.
func writeCredentialHelperTokenValue(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the helper credential file: %v", err)
	}
}

//go:build e2e

package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// installationRunnerRun is one run-installation.sh execution against a fake
// token script. No installation is contacted.
type installationRunnerRun struct {
	output  string
	err     error
	marker  string
	paths   string
	tempDir string
}

// TestHarnessInstallationRunnerAcquiresAndRemovesTokens proves the generic
// runner reads a descriptor, acquires each requested persona token through the
// existing script under a private mode, exposes only private file paths to the
// command, enforces the required mode it was given, and removes the files it
// owns on exit.
func TestHarnessInstallationRunnerAcquiresAndRemovesTokens(t *testing.T) {
	run := runFakeInstallation(t, "", false, false)
	if run.err != nil {
		t.Fatalf("run-installation.sh failed: %v\n%s", run.err, redactInstallationOutput(run.output))
	}
	assertInstallationRunnerCleansUp(t, run)
	recorded := readMarker(t, run.marker)
	for _, wanted := range []string{
		"token-call admin",
		"token-call engineer",
		"file admin",
		"file engineer",
		"mode 600",
		"token acquired-admin-token",
		"token acquired-engineer-token",
		"legacy empty",
		"required 1",
	} {
		if !strings.Contains(recorded, wanted) {
			t.Fatalf("runner marker does not contain %q:\n%s", wanted, recorded)
		}
	}
	for path := range strings.SplitSeq(strings.TrimSpace(readMarker(t, run.paths)), "\n") {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("runner left its private token file %s behind", path)
		}
	}
	if strings.Contains(run.output, "acquired-admin-token") || strings.Contains(run.output, "acquired-engineer-token") {
		t.Fatalf("runner echoed an acquired token:\n%s", redactInstallationOutput(run.output))
	}
}

// TestHarnessInstallationRunnerKillsTermIgnoringCommand proves cleanup uses
// group liveness: a wrapper command that exits on TERM but leaves a
// TERM-ignoring grandchild still gets the whole group killed.
func TestHarnessInstallationRunnerKillsTermIgnoringCommand(t *testing.T) {
	root := repoRootForTest(t)
	runner := filepath.Join(root, "test", "e2e", "internal", "harness", "run-installation.sh")
	tempDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "command.pid")
	groupFile := filepath.Join(t.TempDir(), "grandchild.pid")
	command := exec.Command(runner, "--", "bash", "-c", `printf '%s' "$$" > "$PIDFILE"
bash -c 'trap "" TERM INT HUP; exec sleep 30' &
printf '%s' "$!" > "$GROUPFILE"
while :; do sleep 0.2; done`)
	command.Env = environmentList(map[string]string{
		"PATH":      "/usr/local/bin:/usr/bin:/bin",
		"HOME":      t.TempDir(),
		"TMPDIR":    tempDir,
		"PIDFILE":   pidFile,
		"GROUPFILE": groupFile,
	})
	if err := command.Start(); err != nil {
		t.Fatalf("start run-installation: %v", err)
	}
	waitForPIDFiles(t, command, pidFile, groupFile)
	command.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("wait for run-installation: %v", err)
			}
			code = exit.ExitCode()
		}
		if code != 143 {
			t.Fatalf("run-installation exit = %d, want 143", code)
		}
	case <-time.After(8 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("run-installation did not exit after the bounded escalation")
	}
	for _, path := range []string{pidFile, groupFile} {
		pid := readPID(t, path)
		if err := syscall.Kill(pid, 0); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d survived run-installation cleanup", pid)
		} else if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("probe process %d: %v", pid, err)
		}
	}
	assertWorkDirRemovedPattern(t, tempDir, "q01-installation.*")
}

// TestHarnessInstallationRunnerAcceptsEnvPrefix proves the wrapper form
// "-- env VAR=value ... command" applies the descriptor and acquisition
// settings to the runner itself, as deploy/podman/smoke.sh supplies them.
func TestHarnessInstallationRunnerAcceptsEnvPrefix(t *testing.T) {
	run := runFakeInstallation(t, "", false, true)
	if run.err != nil {
		t.Fatalf("run-installation.sh env-prefix form failed: %v\n%s", run.err, redactInstallationOutput(run.output))
	}
	assertInstallationRunnerCleansUp(t, run)
	recorded := readMarker(t, run.marker)
	for _, wanted := range []string{
		"token-call admin",
		"token-call engineer",
		"file admin",
		"file engineer",
		"mode 600",
		"legacy empty",
	} {
		if !strings.Contains(recorded, wanted) {
			t.Fatalf("env-prefix runner marker does not contain %q:\n%s", wanted, recorded)
		}
	}
}

// TestHarnessInstallationRunnerEnvArguments proves the wrapper form accepts a
// one-character assignment and leaves env options such as -i untouched.
func TestHarnessInstallationRunnerEnvArguments(t *testing.T) {
	runner := filepath.Join(repoRootForTest(t), "test", "e2e", "internal", "harness", "run-installation.sh")
	cases := []struct {
		name      string
		arguments []string
		marker    string
	}{
		{
			name:      "one-character assignment",
			arguments: []string{"--", "env", "X=one", "/bin/sh", "-c", `printf '%s' "$X"`},
			marker:    "one",
		},
		{
			name:      "env options preserved",
			arguments: []string{"--", "env", "-i", "/bin/sh", "-c", `printf '%s' clean`},
			marker:    "clean",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			command := exec.Command(runner, testCase.arguments...)
			command.Env = environmentList(map[string]string{
				"PATH":   "/usr/local/bin:/usr/bin:/bin",
				"HOME":   t.TempDir(),
				"TMPDIR": t.TempDir(),
			})
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run-installation env form failed: %v\n%s", err, output)
			}
			if !strings.HasSuffix(strings.TrimSpace(string(output)), testCase.marker) {
				t.Fatalf("command output = %q; want it to end with %q", output, testCase.marker)
			}
		})
	}
}

// TestHarnessInstallationRunnerCleansUpOnSignal proves a signal delivered while
// the supplied command runs still removes the runner's private token files and
// stays in the foreground (no orphaned child).
func TestHarnessInstallationRunnerCleansUpOnSignal(t *testing.T) {
	run := runFakeInstallation(t, "1", false, false)
	if run.err == nil {
		t.Fatal("run-installation.sh reported success after a TERM during the command")
	}
	assertInstallationRunnerCleansUp(t, run)
	recorded := readMarker(t, run.marker)
	if !strings.Contains(recorded, "command-start") {
		t.Fatalf("the command did not start:\n%s", recorded)
	}
}

// TestHarnessInstallationRunnerTerminatesProcessGroup proves a TERM delivered
// to run-installation.sh ends the wrapped command's whole process group, joins
// it, removes the private token files and exits promptly.
func TestHarnessInstallationRunnerTerminatesProcessGroup(t *testing.T) {
	root := repoRootForTest(t)
	runner := filepath.Join(root, "test", "e2e", "internal", "harness", "run-installation.sh")
	tempDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "command.pid")
	groupFile := filepath.Join(t.TempDir(), "grandchild.pid")
	access := filepath.Join(t.TempDir(), "access-code")
	if err := os.WriteFile(access, []byte("picker-code"), 0o600); err != nil {
		t.Fatalf("write access code: %v", err)
	}
	caPath, _ := testPKI(t)
	tokenScript := filepath.Join(t.TempDir(), "fake-token")
	writeExecutable(t, tokenScript, `#!/bin/sh
printf 'acquired-%s-token' "$1"
`)
	descriptor := map[string]any{
		"schema_version": 1,
		"oauthsonas": map[string]any{
			"issuer":           "https://localhost:19444",
			"audience":         "https://localhost:19443",
			"ca_file":          caPath,
			"client_id":        "kodactl",
			"redirect_uri":     "http://127.0.0.1:52790/callback",
			"access_code_file": access,
			"personas":         map[string]any{"admin": map[string]any{"id": "admin", "subject": "admin"}},
		},
	}
	descriptorPath := writeInstallationDescriptor(t, descriptor)

	command := exec.Command(runner, "--", "bash", "-c", `printf '%s' "$$" > "$PIDFILE"
sleep 30 &
printf '%s' "$!" > "$GROUPFILE"
wait`)
	command.Env = environmentList(map[string]string{
		"PATH":                   "/usr/local/bin:/usr/bin:/bin",
		"HOME":                   t.TempDir(),
		"TMPDIR":                 tempDir,
		"Q01_INSTALLATION_FILE":  descriptorPath,
		"Q01_TOKEN_SCRIPT":       tokenScript,
		"Q01_ACQUIRE_PERSONAS":   "admin",
		"Q01_TOKEN_MIN_LIFETIME": "0",
		"PIDFILE":                pidFile,
		"GROUPFILE":              groupFile,
	})
	if err := command.Start(); err != nil {
		t.Fatalf("start run-installation: %v", err)
	}
	waitForPIDFiles(t, command, pidFile, groupFile)
	command.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("wait for run-installation: %v", err)
			}
			code = exit.ExitCode()
		}
		if code != 143 {
			t.Fatalf("run-installation exit = %d, want 143", code)
		}
	case <-time.After(8 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("run-installation did not exit promptly after TERM")
	}
	for _, path := range []string{pidFile, groupFile} {
		pid := readPID(t, path)
		if err := syscall.Kill(pid, 0); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child process %d survived run-installation TERM", pid)
		} else if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("probe child process %d: %v", pid, err)
		}
	}
	assertWorkDirRemovedPattern(t, tempDir, "q01-installation.*")
}

// TestHarnessInstallationRunnerRefusesMalformedDescriptor proves a descriptor
// that is not schema version 1 is refused before any token is acquired.
func TestHarnessInstallationRunnerRefusesMalformedDescriptor(t *testing.T) {
	run := runFakeInstallation(t, "", true, false)
	if run.err == nil {
		t.Fatal("a schema version 2 descriptor was accepted")
	}
	if !strings.Contains(run.output, "schema_version") {
		t.Fatalf("descriptor failure %q does not name schema_version:\n%s", run.err, redactInstallationOutput(run.output))
	}
	if data, err := os.ReadFile(run.marker); err == nil && strings.Contains(string(data), "token-call") {
		t.Fatalf("a token was acquired before the descriptor was refused:\n%s", data)
	}
}

func assertInstallationRunnerCleansUp(t *testing.T, run installationRunnerRun) {
	t.Helper()
	assertWorkDirRemovedPattern(t, run.tempDir, "q01-installation.*")
	for path := range strings.SplitSeq(strings.TrimSpace(readMarker(t, run.paths)), "\n") {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("runner left its private token file %s behind", path)
		}
	}
}

// runFakeInstallation builds one isolated fake installation. signalParent makes
// the supplied command TERM the runner; schemaVersionOverride replaces the
// descriptor's schema version when nonzero.
func runFakeInstallation(t *testing.T, signalParent string, malformed bool, envPrefix bool) installationRunnerRun {
	t.Helper()
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	paths := filepath.Join(t.TempDir(), "paths")
	tempDir := t.TempDir()
	access := filepath.Join(t.TempDir(), "access-code")
	if err := os.WriteFile(access, []byte("picker-code"), 0o600); err != nil {
		t.Fatalf("write fake access code: %v", err)
	}
	caPath, _ := testPKI(t)
	tokenScript := filepath.Join(t.TempDir(), "fake-token")
	writeExecutable(t, tokenScript, `#!/bin/sh
persona=$1
printf 'token-call %s\n' "$persona" >> "$Q01_FAKE_MARKER"
if [ -n "${OAUTHSONAS_ACCESS_CODE_FILE:-}" ]; then
  printf 'access %s\n' "$(stat -c '%a' "$OAUTHSONAS_ACCESS_CODE_FILE")" >> "$Q01_FAKE_MARKER"
fi
printf 'acquired-%s-token' "$persona"
`)
	schemaVersion := 1
	if malformed {
		schemaVersion = 2
	}
	descriptor := map[string]any{
		"schema_version": schemaVersion,
		"oauthsonas": map[string]any{
			"issuer":           "https://localhost:19444",
			"audience":         "https://localhost:19443",
			"ca_file":          caPath,
			"client_id":        "kodactl",
			"redirect_uri":     "http://127.0.0.1:52790/callback",
			"resolve":          "",
			"access_code_file": access,
			"personas": map[string]any{
				"admin":    map[string]any{"id": "admin", "subject": "admin"},
				"engineer": map[string]any{"id": "engineer", "subject": "engineer"},
			},
		},
	}
	descriptorPath := writeInstallationDescriptor(t, descriptor)

	runner := filepath.Join(repoRootForTest(t), "test", "e2e", "internal", "harness", "run-installation.sh")
	commandCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	arguments := []string{"--"}
	environment := map[string]string{
		"PATH":                     bin + ":/usr/local/bin:/usr/bin:/bin",
		"HOME":                     t.TempDir(),
		"TMPDIR":                   tempDir,
		"Q01_INSTALLATION_FILE":    descriptorPath,
		"Q01_TOKEN_SCRIPT":         tokenScript,
		"Q01_ACQUIRE_PERSONAS":     "admin engineer",
		"Q01_TOKEN_MIN_LIFETIME":   "0",
		"Q01_REQUIRE_INSTALLATION": "1",
		"Q01_FAKE_MARKER":          marker,
		"Q01_FAKE_PATHS":           paths,
		"Q01_FAKE_SIGNAL_PARENT":   signalParent,
		"Q01_TOKEN":                "", // never inherited by the acquisition path
	}
	if envPrefix {
		arguments = append(arguments, "env",
			"Q01_INSTALLATION_FILE="+descriptorPath,
			"Q01_TOKEN_SCRIPT="+tokenScript,
			"Q01_ACQUIRE_PERSONAS=admin engineer",
			"Q01_TOKEN_MIN_LIFETIME=0",
		)
		for _, name := range []string{
			"Q01_INSTALLATION_FILE", "Q01_TOKEN_SCRIPT", "Q01_ACQUIRE_PERSONAS", "Q01_TOKEN_MIN_LIFETIME",
		} {
			delete(environment, name)
		}
	}
	arguments = append(arguments, "sh", "-c", `printf 'command-start\n' >> "$Q01_FAKE_MARKER"
for persona in admin engineer; do
  variable="Q01_$(printf '%s' "$persona" | tr '[:lower:]' '[:upper:]')_TOKEN_FILE"
  eval "file=\${$variable}"
  printf '%s\n' "$file" >> "$Q01_FAKE_PATHS"
  printf 'file %s\n' "$persona" >> "$Q01_FAKE_MARKER"
  printf 'mode %s\n' "$(stat -c '%a' "$file")" >> "$Q01_FAKE_MARKER"
  printf 'token %s\n' "$(cat "$file")" >> "$Q01_FAKE_MARKER"
done
printf 'legacy %s\n' "${Q01_TOKEN:+set}${Q01_TOKEN:-empty}" >> "$Q01_FAKE_MARKER"
printf 'required %s\n' "${Q01_REQUIRE_INSTALLATION:-unset}" >> "$Q01_FAKE_MARKER"
if [ "${Q01_FAKE_SIGNAL_PARENT:-0}" = "1" ]; then kill -TERM "$PPID"; sleep 5; fi`)
	command := exec.CommandContext(commandCtx, runner, arguments...)
	command.Dir = repoRootForTest(t)
	command.Env = environmentList(environment)
	output, err := command.CombinedOutput()
	return installationRunnerRun{
		output:  string(output),
		err:     err,
		marker:  marker,
		paths:   paths,
		tempDir: tempDir,
	}
}

func environmentList(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sortStrings(names)
	list := make([]string, 0, len(environment))
	for _, name := range names {
		list = append(list, name+"="+environment[name])
	}
	return list
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for previous := index - 1; previous >= 0 && values[previous] > values[previous+1]; previous-- {
			values[previous], values[previous+1] = values[previous+1], values[previous]
		}
	}
}

// assertWorkDirRemovedPattern proves no private runner work directory of the
// given pattern survived.
func assertWorkDirRemovedPattern(t *testing.T, tempDir, pattern string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(tempDir, pattern))
	if err != nil {
		t.Fatalf("glob work directory: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("the runner left work directories behind: %v", matches)
	}
}

func redactInstallationOutput(output string) string {
	for _, marker := range []string{"acquired-admin-token", "acquired-engineer-token", "picker-code"} {
		output = strings.ReplaceAll(output, marker, "<redacted>")
	}
	return output
}

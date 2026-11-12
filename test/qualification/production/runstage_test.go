//go:build qualification

package production

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeStageRun is one run-stage.sh execution against fake kubectl, curl and
// token executables. No cluster is contacted.
type fakeStageRun struct {
	output   string
	err      error
	elapsed  time.Duration
	marker   string
	result   string
	evidence string
	tempDir  string
}

// TestHarnessRunStageCleansUpFakeStage drives run-stage.sh with fake kubectl,
// curl and token executables. It proves the runner keeps credentials out of its
// output, writes the picker code to a 0600 file, and terminates and joins both
// port-forwards on exit.
func TestHarnessRunStageCleansUpFakeStage(t *testing.T) {
	run := runFakeStage(t, "401")
	if run.err != nil {
		t.Fatalf("run-stage.sh failed: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	assertRunStageCleansUp(t, run)
}

// TestHarnessRunStageAcceptsEnvelopeRefusalForReadiness proves the kodama
// readiness probe treats the edge's body-shape 400 as reachability: the real
// edge decodes the request envelope before authentication, so an unauthenticated
// bodyless GET is invalid_input rather than 401.
func TestHarnessRunStageAcceptsEnvelopeRefusalForReadiness(t *testing.T) {
	run := runFakeStage(t, "400")
	if run.err != nil {
		t.Fatalf("run-stage.sh refused the edge's 400 readiness response: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	assertRunStageCleansUp(t, run)
}

// TestHarnessRunStageFailsWhenEndpointUnreachable proves a 000 response still
// fails after the bounded readiness wait, and that both port-forwards are
// joined and killed on the failure path.
func TestHarnessRunStageFailsWhenEndpointUnreachable(t *testing.T) {
	run := runFakeStage(t, "000")
	if run.err == nil {
		t.Fatalf("run-stage.sh accepted an unreachable endpoint:\n%s", redactRunStageOutput(run.output))
	}
	if !strings.Contains(run.output, "did not become ready") {
		t.Fatalf("unreachable failure %q does not name readiness:\n%s", run.err, redactRunStageOutput(run.output))
	}
	if run.elapsed > 5*time.Second {
		t.Fatalf("the bounded readiness wait took %s with two 10ms attempts", run.elapsed)
	}
	recorded := readMarker(t, run.marker)
	assertNoInheritedTokens(t, recorded, false)
	assertWorkDirRemoved(t, run.tempDir)
	for _, wanted := range []string{"started kodama", "started oauthsonas", "terminated kodama", "terminated oauthsonas"} {
		if !strings.Contains(recorded, wanted) {
			t.Fatalf("failure-path marker does not contain %q:\n%s", wanted, recorded)
		}
	}
	for line := range strings.SplitSeq(recorded, "\n") {
		if strings.HasPrefix(line, "token ") || strings.HasPrefix(line, "access 600 ") {
			t.Fatalf("the failure path acquired a token before readiness:\n%s", recorded)
		}
	}
	if _, err := os.Stat(run.result); !os.IsNotExist(err) {
		t.Fatalf("the child command ran despite an unreachable endpoint")
	}
}

// TestHarnessRunStageKillsPortForwardThatIgnoresTerm proves cleanup escalates to
// KILL when a port-forward ignores TERM, while still removing the work directory
// and keeping tokens out of cleanup children.
func TestHarnessRunStageKillsPortForwardThatIgnoresTerm(t *testing.T) {
	run := runFakeStageWithOverrides(t, "401", map[string]string{"Q02_FAKE_IGNORE_TERM": "1"})
	if run.err != nil {
		t.Fatalf("run-stage.sh failed with a TERM-ignoring port-forward: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	if data, err := os.ReadFile(run.result); err != nil || strings.TrimSpace(string(data)) != "admin-token:developer-token:admin-b-token" {
		t.Fatalf("the child command did not receive the acquired tokens: data=%q err=%v", data, err)
	}
	recorded := readMarker(t, run.marker)
	if strings.Contains(recorded, "terminated ") {
		t.Fatalf("a TERM-ignoring port-forward reported graceful termination:\n%s", recorded)
	}
	assertNoInheritedTokens(t, recorded, true)
	assertProcessesGone(t, recorded)
	assertWorkDirRemoved(t, run.tempDir)
	if run.elapsed > 5*time.Second {
		t.Fatalf("cleanup of a TERM-ignoring port-forward took %s", run.elapsed)
	}
}

// TestHarnessRunStageCallerSecondToken proves a caller-supplied second tenant
// token is captured before the runner unsets qualification tokens and reaches
// only the final command, never the kubernetes, curl, token-script or sleep
// children.
func TestHarnessRunStageCallerSecondToken(t *testing.T) {
	run := runFakeStageWithOverrides(t, "401", map[string]string{
		"Q02_SECOND_ADMIN_PERSONA": "",
		"Q02_SECOND_ADMIN_TOKEN":   "caller-second-token",
	})
	if run.err != nil {
		t.Fatalf("run-stage.sh failed with a caller-supplied second token: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	if data, err := os.ReadFile(run.result); err != nil || strings.TrimSpace(string(data)) != "admin-token:developer-token:caller-second-token" {
		t.Fatalf("the final command did not receive the caller-supplied second token: data=%q err=%v", data, err)
	}
	recorded := readMarker(t, run.marker)
	assertNoInheritedTokens(t, recorded, false)
	assertWorkDirRemoved(t, run.tempDir)
}

// TestHarnessRunStageRefusesOutOfScopeInput proves the runner refuses a
// non-release namespace and unsafe Secret coordinates before any child starts.
func TestHarnessRunStageRefusesOutOfScopeInput(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
	}{
		{name: "non release namespace", overrides: map[string]string{"Q02_RELEASE_NAMESPACE": "kube-system"}},
		{name: "unsafe secret name", overrides: map[string]string{"Q02_ACCESS_CODE_SECRET": "Bad_Secret"}},
		{name: "unsafe secret key", overrides: map[string]string{"Q02_ACCESS_CODE_KEY": "Bad_Key"}},
		{name: "unsafe readiness interval", overrides: map[string]string{"Q02_READY_INTERVAL": "abc"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			run := runFakeStageWithOverrides(t, "401", testCase.overrides)
			if run.err == nil {
				t.Fatalf("unsafe input %v was accepted:\n%s", testCase.overrides, redactRunStageOutput(run.output))
			}
			if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
				t.Fatalf("a child started before the unsafe input %v was refused", testCase.overrides)
			}
		})
	}
}

// assertRunStageCleansUp checks the successful path's credentials and cleanup.
func assertRunStageCleansUp(t *testing.T, run fakeStageRun) {
	t.Helper()
	if strings.Contains(run.output, "picker-secret-code") ||
		strings.Contains(run.output, "admin-token") ||
		strings.Contains(run.output, "developer-token") {
		t.Fatalf("run-stage.sh leaked credential material:\n%s", redactRunStageOutput(run.output))
	}
	for line := range strings.SplitSeq(run.output, "\n") {
		if strings.HasPrefix(line, "+ ") {
			t.Fatalf("run-stage.sh emitted a shell trace:\n%s", redactRunStageOutput(run.output))
		}
	}
	if data, err := os.ReadFile(run.result); err != nil || strings.TrimSpace(string(data)) != "admin-token:developer-token:admin-b-token" {
		t.Fatalf("the child command did not receive the acquired tokens: data=%q err=%v", data, err)
	}
	recorded := readMarker(t, run.marker)
	assertNoInheritedTokens(t, recorded, true)
	assertWorkDirRemoved(t, run.tempDir)
	for _, wanted := range []string{
		"started kodama",
		"started oauthsonas",
		"terminated kodama",
		"terminated oauthsonas",
		"access 600 ",
	} {
		if !strings.Contains(recorded, wanted) {
			t.Fatalf("fake stage marker does not contain %q:\n%s", wanted, recorded)
		}
	}
	for line := range strings.SplitSeq(recorded, "\n") {
		if !strings.HasPrefix(line, "access 600 ") {
			continue
		}
		path := strings.TrimPrefix(line, "access 600 ")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the picker access code file %s survived run-stage.sh", path)
		}
	}
	if _, err := os.Stat(run.evidence); err != nil {
		t.Fatalf("the evidence directory was not created: %v", err)
	}
}

// runFakeStage builds one isolated fake stage. kodamaCode is the readiness code
// the fake curl prints for the kodama URL; OAuthSonas always answers 303.
func runFakeStage(t *testing.T, kodamaCode string) fakeStageRun {
	t.Helper()
	return runFakeStageWithOverrides(t, kodamaCode, nil)
}

// runFakeStageWithOverrides applies test-only environment overrides.
func runFakeStageWithOverrides(t *testing.T, kodamaCode string, overrides map[string]string) fakeStageRun {
	t.Helper()
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("locate real sleep: %v", err)
	}
	realMkdir, err := exec.LookPath("mkdir")
	if err != nil {
		t.Fatalf("locate real mkdir: %v", err)
	}
	realChmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatalf("locate real chmod: %v", err)
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	result := filepath.Join(t.TempDir(), "result")
	tempDir := t.TempDir()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, []byte("test CA placeholder\n"), 0o600); err != nil {
		t.Fatalf("write fake CA: %v", err)
	}

	writeExecutable(t, filepath.Join(bin, "kubectl"), `#!/bin/sh
marker="$Q02_FAKE_MARKER"
printf 'kubectl-token %s %s %s\n' "${Q02_ADMIN_TOKEN:-empty}" "${Q02_DEVELOPER_TOKEN:-empty}" "${Q02_SECOND_ADMIN_TOKEN:-empty}" >> "$marker"
case " $* " in
  *" port-forward "*)
    previous=""
    service=""
    for argument in "$@"; do
      if [ "$previous" = "port-forward" ]; then service="$argument"; fi
      previous="$argument"
    done
    name=${service#svc/}
    if [ "${Q02_FAKE_IGNORE_TERM:-0}" = "1" ]; then
      trap '' TERM INT
    else
      trap 'echo "terminated $name $$" >> "$marker"; exit 0' TERM INT
    fi
    echo "started $name $$" >> "$marker"
    # The trap is installed before "started" is recorded, so the cleanup signal
    # can never race the installation. The short idle sleep lets the trap run
    # inside the test's bounded cleanup wait.
    while :; do sleep 0.005; done
    ;;
  *" get secret "*)
    printf '%s' "$Q02_FAKE_ACCESS_CODE_B64"
    ;;
  *)
    echo "unexpected kubectl invocation" >&2
    exit 2
    ;;
esac
`)
	writeExecutable(t, filepath.Join(bin, "sleep"), `#!/bin/sh
printf 'sleep-token %s %s %s\n' "${Q02_ADMIN_TOKEN:-empty}" "${Q02_DEVELOPER_TOKEN:-empty}" "${Q02_SECOND_ADMIN_TOKEN:-empty}" >> "$Q02_FAKE_MARKER"
exec "$Q02_REAL_SLEEP" "$@"
`)
	writeExecutable(t, filepath.Join(bin, "mkdir"), `#!/bin/sh
printf 'mkdir-token %s %s %s\n' "${Q02_ADMIN_TOKEN:-empty}" "${Q02_DEVELOPER_TOKEN:-empty}" "${Q02_SECOND_ADMIN_TOKEN:-empty}" >> "$Q02_FAKE_MARKER"
exec "$Q02_REAL_MKDIR" "$@"
`)
	writeExecutable(t, filepath.Join(bin, "chmod"), `#!/bin/sh
printf 'chmod-token %s %s %s\n' "${Q02_ADMIN_TOKEN:-empty}" "${Q02_DEVELOPER_TOKEN:-empty}" "${Q02_SECOND_ADMIN_TOKEN:-empty}" >> "$Q02_FAKE_MARKER"
exec "$Q02_REAL_CHMOD" "$@"
`)
	writeExecutable(t, filepath.Join(bin, "curl"), `#!/bin/sh
marker="$Q02_FAKE_MARKER"
printf 'curl-token %s %s %s\n' "${Q02_ADMIN_TOKEN:-empty}" "${Q02_DEVELOPER_TOKEN:-empty}" "${Q02_SECOND_ADMIN_TOKEN:-empty}" >> "$marker"
kodama_code="${Q02_FAKE_KODAMA_CODE:-401}"
for argument in "$@"; do
  case "$argument" in
    *kodama.kodama-system.svc.cluster.local*) printf '%s' "$kodama_code"; exit 0 ;;
    *oauthsonas.kodama-system.svc.cluster.local*) printf 303; exit 0 ;;
  esac
done
printf 000
exit 7
`)
	tokenScript := filepath.Join(t.TempDir(), "fake-token")
	writeExecutable(t, tokenScript, `#!/bin/sh
marker="$Q02_FAKE_MARKER"
persona=$1
printf 'token-script-token %s %s %s\n' "${Q02_ADMIN_TOKEN:-empty}" "${Q02_DEVELOPER_TOKEN:-empty}" "${Q02_SECOND_ADMIN_TOKEN:-empty}" >> "$marker"
echo "token $persona" >> "$marker"
if [ -n "${OAUTHSONAS_ACCESS_CODE_FILE:-}" ]; then
  mode=$(stat -c '%a' "$OAUTHSONAS_ACCESS_CODE_FILE")
  echo "access $mode $OAUTHSONAS_ACCESS_CODE_FILE" >> "$marker"
fi
printf '%s-token' "$persona"
`)
	runner := filepath.Join(repoRootForTest(t), "test", "qualification", "production", "run-stage.sh")
	evidence := filepath.Join(t.TempDir(), "evidence")

	commandCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, runner, "sh", "-c", `test -n "$Q02_ADMIN_TOKEN" && test -n "$Q02_DEVELOPER_TOKEN" && test -n "${Q02_SECOND_ADMIN_TOKEN:-}" && printf '%s:%s:%s' "$Q02_ADMIN_TOKEN" "$Q02_DEVELOPER_TOKEN" "$Q02_SECOND_ADMIN_TOKEN" > "$Q02_FAKE_RESULT"`)
	command.Dir = repoRootForTest(t)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 5 * time.Second
	environment := map[string]string{
		"PATH":                     bin + ":/usr/local/bin:/usr/bin:/bin",
		"HOME":                     t.TempDir(),
		"TMPDIR":                   tempDir,
		"Q02_KUBE_CONTEXT":         "kodama-stage-k8s",
		"Q02_CA_PEM":               caPath,
		"Q02_PROJECT_ID":           "tenant-a",
		"Q02_TOKEN_SCRIPT":         tokenScript,
		"Q02_EVIDENCE_DIR":         evidence,
		"Q02_SECOND_ADMIN_PERSONA": "admin-b",
		"Q02_API_PORT":             "19443",
		"Q02_OAUTHSONAS_PORT":      "8443",
		"Q02_READY_ATTEMPTS":       "2",
		"Q02_READY_INTERVAL":       "0.01",
		// A 30ms graceful window was too short under -race: the fake forward's
		// TERM trap could miss it and be KILLed before writing its marker. A
		// one-second bound still exits immediately on the normal path and stays
		// under every test's five-second limit.
		"Q02_CLEANUP_ATTEMPTS":     "20",
		"Q02_CLEANUP_INTERVAL":     "0.05",
		"Q02_REAL_SLEEP":           realSleep,
		"Q02_REAL_MKDIR":           realMkdir,
		"Q02_REAL_CHMOD":           realChmod,
		"Q02_FAKE_KODAMA_CODE":     kodamaCode,
		"Q02_FAKE_MARKER":          marker,
		"Q02_FAKE_RESULT":          result,
		"Q02_FAKE_ACCESS_CODE_B64": "cGlja2VyLXNlY3JldC1jb2Rl",
		"Q02_ADMIN_TOKEN":          "preexisting-exported-token",
		"Q02_DEVELOPER_TOKEN":      "preexisting-exported-token",
	}
	maps.Copy(environment, overrides)
	command.Env = make([]string, 0, len(environment))
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	started := time.Now()
	output, err := command.CombinedOutput()
	return fakeStageRun{
		output:   string(output),
		err:      err,
		elapsed:  time.Since(started),
		marker:   marker,
		result:   result,
		evidence: evidence,
		tempDir:  tempDir,
	}
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake stage marker: %v", err)
	}
	return string(data)
}

// assertNoInheritedTokens proves the long-lived children never saw a caller's
// exported token or the acquired token. The token script and cleanup sleeps are
// included so an early export or a missing cleanup unset is caught.
func assertNoInheritedTokens(t *testing.T, recorded string, requireTokenScript bool) {
	t.Helper()
	seen := map[string]int{}
	for line := range strings.SplitSeq(recorded, "\n") {
		for _, prefix := range []string{"kubectl-token ", "curl-token ", "token-script-token ", "sleep-token ", "mkdir-token ", "chmod-token "} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			seen[prefix]++
			if line != prefix+"empty empty empty" {
				t.Fatalf("%s child saw a token: %s", strings.TrimSuffix(prefix, " "), line)
			}
		}
	}
	for _, prefix := range []string{"kubectl-token ", "curl-token ", "sleep-token "} {
		if seen[prefix] == 0 {
			t.Fatalf("token-environment observation missing for %s", strings.TrimSuffix(prefix, " "))
		}
	}
	if requireTokenScript && seen["token-script-token "] != 3 {
		t.Fatalf("token script observations = %d, want 3", seen["token-script-token "])
	}
}

// assertWorkDirRemoved proves the private runner work directory was removed.
func assertWorkDirRemoved(t *testing.T, tempDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(tempDir, "q02-stage.*"))
	if err != nil {
		t.Fatalf("glob work directory: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("run-stage left work directories behind: %v", matches)
	}
}

// assertProcessesGone proves the started port-forward PIDs no longer exist.
func assertProcessesGone(t *testing.T, recorded string) {
	t.Helper()
	seen := 0
	for line := range strings.SplitSeq(recorded, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "started" {
			continue
		}
		pid, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("parse started pid from %q: %v", line, err)
		}
		seen++
		if err := syscall.Kill(pid, 0); err == nil {
			t.Fatalf("port-forward process %d survived cleanup", pid)
		} else if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("probe port-forward process %d: %v", pid, err)
		}
	}
	if seen == 0 {
		t.Fatal("no started port-forward pids were recorded")
	}
}

// redactRunStageOutput keeps a failing assertion from printing credential-shaped
// values that the runner itself refused to print.
func redactRunStageOutput(output string) string {
	for _, marker := range []string{"picker-secret-code", "admin-token", "developer-token"} {
		output = strings.ReplaceAll(output, marker, "<redacted>")
	}
	return output
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the repository root")
		}
		dir = parent
	}
}

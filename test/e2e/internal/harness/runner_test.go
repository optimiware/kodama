//go:build e2e

package harness

import (
	"context"
	"encoding/json"
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
	output     string
	err        error
	elapsed    time.Duration
	marker     string
	result     string
	evidence   string
	tempDir    string
	descriptor string
	accessCopy string
}

// TestHarnessRunStageCleansUpFakeStage drives run-stage.sh with fake kubectl,
// curl and token executables. It proves the runner keeps credentials out of its
// output, writes the picker code to a 0600 file, and terminates and joins both
// port-forwards on exit.
func TestHarnessRunStageCleansUpFakeStage(t *testing.T) {
	run := runFakeStage(t, "401", nil)
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
	run := runFakeStage(t, "400", nil)
	if run.err != nil {
		t.Fatalf("run-stage.sh refused the edge's 400 readiness response: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	assertRunStageCleansUp(t, run)
}

// TestHarnessRunStageFailsWhenEndpointUnreachable proves a 000 response still
// fails after the bounded readiness wait, and that both port-forwards are
// joined and killed on the failure path.
func TestHarnessRunStageFailsWhenEndpointUnreachable(t *testing.T) {
	run := runFakeStage(t, "000", nil)
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
	assertNoInheritedToken(t, recorded, false)
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
// and keeping the token out of cleanup children.
func TestHarnessRunStageKillsPortForwardThatIgnoresTerm(t *testing.T) {
	run := runFakeStage(t, "401", map[string]string{"Q01_FAKE_IGNORE_TERM": "1"})
	if run.err != nil {
		t.Fatalf("run-stage.sh failed with a TERM-ignoring port-forward: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	if data, err := os.ReadFile(run.result); err != nil || strings.TrimSpace(string(data)) != "project-token" {
		t.Fatalf("the child command did not receive the acquired token: data=%q err=%v", data, err)
	}
	recorded := readMarker(t, run.marker)
	if strings.Contains(recorded, "terminated ") {
		t.Fatalf("a TERM-ignoring port-forward reported graceful termination:\n%s", recorded)
	}
	assertNoInheritedToken(t, recorded, true)
	assertProcessesGone(t, recorded)
	assertWorkDirRemoved(t, run.tempDir)
	if run.elapsed > 5*time.Second {
		t.Fatalf("cleanup of a TERM-ignoring port-forward took %s", run.elapsed)
	}
}

// TestHarnessRunStageFailsWhenPortForwardExits proves the runner refuses a run
// whose port-forward exited before the probe reached the service, and still
// joins/cleans up on that failure path.
func TestHarnessRunStageFailsWhenPortForwardExits(t *testing.T) {
	run := runFakeStage(t, "401", map[string]string{
		"Q01_FAKE_DIE_FORWARD": "1",
		"Q01_FAKE_CURL_DELAY":  "0.3",
	})
	if run.err == nil {
		t.Fatalf("run-stage.sh accepted a dead port-forward:\n%s", redactRunStageOutput(run.output))
	}
	if !strings.Contains(run.output, "exited before the probe reached the service") {
		t.Fatalf("dead port-forward failure %q does not name the liveness check:\n%s", run.err, redactRunStageOutput(run.output))
	}
	assertNoInheritedToken(t, readMarker(t, run.marker), false)
	assertWorkDirRemoved(t, run.tempDir)
}

// TestHarnessRunStageSignalDuringFinalCommandCleansUp proves a signal delivered
// to the runner while the final command holds the acquired token still runs
// cleanup with the token unset and joins the port-forwards.
func TestHarnessRunStageSignalDuringFinalCommandCleansUp(t *testing.T) {
	run := runFakeStage(t, "401", map[string]string{"Q01_FAKE_SIGNAL_PARENT": "1"})
	if run.err == nil {
		t.Fatal("run-stage.sh reported success after a TERM during the final command")
	}
	recorded := readMarker(t, run.marker)
	if !strings.Contains(recorded, "admin-file nonempty") {
		t.Fatalf("the final command did not observe the acquired admin token file:\n%s", recorded)
	}
	if !strings.Contains(recorded, "legacy-token empty") {
		t.Fatalf("the final command inherited the legacy Q01_TOKEN value:\n%s", recorded)
	}
	if !strings.Contains(recorded, "require-stage yes") {
		t.Fatal("the final command did not observe Q01_REQUIRE_STAGE=1")
	}
	assertNoInheritedToken(t, recorded, true)
	assertWorkDirRemoved(t, run.tempDir)
}

// TestHarnessRunStageRefusesOutOfScopeInput proves the runner refuses a
// non-release namespace and unsafe Secret coordinates before any child starts.
func TestHarnessRunStageRefusesOutOfScopeInput(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
	}{
		{name: "non release namespace", overrides: map[string]string{"Q01_RELEASE_NAMESPACE": "kube-system"}},
		{name: "unsafe secret name", overrides: map[string]string{"Q01_ACCESS_CODE_SECRET": "Bad_Secret"}},
		{name: "unsafe secret key", overrides: map[string]string{"Q01_ACCESS_CODE_KEY": "Bad_Key"}},
		{name: "unsafe readiness interval", overrides: map[string]string{"Q01_READY_INTERVAL": "abc"}},
		{name: "unsafe smoke git switch", overrides: map[string]string{"Q01_SMOKE_GIT": "sometimes"}},
		{name: "unsafe descriptor switch", overrides: map[string]string{"Q01_STAGE_DESCRIPTOR": "sometimes"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			run := runFakeStage(t, "401", testCase.overrides)
			if run.err == nil {
				t.Fatalf("unsafe input %v was accepted:\n%s", testCase.overrides, redactRunStageOutput(run.output))
			}
			if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
				t.Fatalf("a child started before the unsafe input %v was refused", testCase.overrides)
			}
		})
	}
}

// TestHarnessRunStageGeneratesStageDescriptor proves the optional stage
// descriptor is a private schema version 1 document the real loader accepts,
// names the four canonical personas, carries no Git block, and points at the
// picker file while the child runs; the picker and descriptor are removed on
// exit.
func TestHarnessRunStageGeneratesStageDescriptor(t *testing.T) {
	run := runFakeStage(t, "401", map[string]string{
		"Q01_STAGE_DESCRIPTOR":      "1",
		"Q01_STAGE_INSTALLATION_ID": "stage-test",
		"Q01_DEPLOYED_REVISION":     strings.Repeat("a", 40),
		"Q01_SERVER_IMAGE_DIGEST":   "sha256:" + strings.Repeat("b", 64),
		"Q01_OAUTHSONAS_AUDIENCE":   "https://kodama.kodama-system.svc.cluster.local:8443",
	})
	if run.err != nil {
		t.Fatalf("run-stage.sh failed: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	recorded := readMarker(t, run.marker)
	if !strings.Contains(recorded, "smoke-git 0") {
		t.Fatalf("the non-Git stage run did not disable the Git scenarios:\n%s", recorded)
	}
	if !strings.Contains(recorded, "descriptor 600") {
		t.Fatalf("the generated descriptor is not a 0600 file:\n%s", recorded)
	}
	if !strings.Contains(recorded, "access-live yes 600") {
		t.Fatalf("the picker access code was not alive while the child ran:\n%s", recorded)
	}
	if _, err := os.Stat(run.accessCopy); err != nil {
		t.Fatalf("the child did not copy the picker code for validation: %v", err)
	}
	data, err := os.ReadFile(run.descriptor)
	if err != nil {
		t.Fatalf("read the generated descriptor: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("generated descriptor is not JSON: %v", err)
	}
	if _, ok := document["git"]; ok {
		t.Fatal("the non-Git stage descriptor carries a git block")
	}
	oauth, _ := document["oauthsonas"].(map[string]any)
	if oauth == nil {
		t.Fatal("the generated descriptor has no oauthsonas block")
	}
	personas, _ := oauth["personas"].(map[string]any)
	for _, persona := range Personas {
		if _, ok := personas[string(persona)]; !ok {
			t.Fatalf("the generated descriptor has no %s persona", persona)
		}
	}
	// The descriptor is generated inside the runner's private work directory,
	// so its access-code path no longer exists after exit. Rewrite that one
	// path to the private copy the child preserved; every other member is
	// validated unchanged by the real schema version 1 loader.
	oauth["access_code_file"] = run.accessCopy
	rewritten, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("rewrite descriptor: %v", err)
	}
	path := filepath.Join(t.TempDir(), "installation.json")
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatalf("write rewritten descriptor: %v", err)
	}
	installation, err := loadInstallation(path)
	if err != nil {
		t.Fatalf("the generated descriptor is not schema version 1: %v", err)
	}
	if installation.ID != "stage-test" ||
		installation.Profile != "production" ||
		installation.Runtime != "kubernetes" ||
		installation.TenantID != "tenant-a" ||
		installation.ProjectID != "tenant-a" {
		t.Fatalf("generated descriptor identity = %+v", installation)
	}
	if installation.API.URL != "https://127.0.0.1:19443" ||
		installation.API.TLSServerName != "kodama.kodama-system.svc.cluster.local" {
		t.Fatalf("generated API block = %+v", installation.API)
	}
	if installation.Images.Tools == "" || installation.Images.ExternalHarness == "" {
		t.Fatalf("generated images = %+v; want both digest pins", installation.Images)
	}
	if installation.OAuth.Issuer == "" || installation.OAuth.Audience == "" ||
		installation.OAuth.ClientID == "" || installation.OAuth.RedirectURI == "" ||
		installation.OAuth.Resolve == "" {
		t.Fatalf("generated oauthsonas block is incomplete: %+v", installation.OAuth)
	}
	for _, persona := range Personas {
		got, ok := installation.OAuth.Personas[string(persona)]
		if !ok || got.ID != string(persona) || got.Subject != string(persona) || got.PrincipalID == "" {
			t.Fatalf("generated %s persona = %+v (present %v)", persona, got, ok)
		}
	}
	if installation.Git.URL != "" || len(installation.Git.Repositories) != 0 {
		t.Fatalf("the non-Git descriptor carries Git configuration: %+v", installation.Git)
	}
}

// TestHarnessRunStageAcceptsExistingDescriptor proves an existing
// Q01_INSTALLATION_FILE is passed through instead of generated.
func TestHarnessRunStageAcceptsExistingDescriptor(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "installation.json")
	document := `{"schema_version":1}`
	if err := os.WriteFile(existing, []byte(document), 0o600); err != nil {
		t.Fatalf("write existing descriptor: %v", err)
	}
	run := runFakeStage(t, "401", map[string]string{
		"Q01_STAGE_DESCRIPTOR":  "1",
		"Q01_INSTALLATION_FILE": existing,
	})
	if run.err != nil {
		t.Fatalf("run-stage.sh failed with an existing descriptor: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	data, err := os.ReadFile(run.descriptor)
	if err != nil {
		t.Fatalf("the child did not receive the existing descriptor: %v", err)
	}
	if string(data) != document {
		t.Fatalf("existing descriptor was replaced: %q", data)
	}
}

// TestHarnessRunStagePassesTLSBrokerInputs proves a tls:// stage broker receives
// the pass-through URL and TLS name plus the defaulted installation CA and the
// private admin token file, without any token value crossing the boundary.
func TestHarnessRunStagePassesTLSBrokerInputs(t *testing.T) {
	run := runFakeStage(t, "401", map[string]string{
		"Q01_NATS_URL":             "tls://127.0.0.1:14222",
		"Q01_NATS_TLS_SERVER_NAME": "nats.kodama-system.svc.cluster.local",
	})
	if run.err != nil {
		t.Fatalf("run-stage.sh failed with TLS broker inputs: %v\n%s", run.err, redactRunStageOutput(run.output))
	}
	recorded := readMarker(t, run.marker)
	for _, wanted := range []string{
		"nats-url tls://127.0.0.1:14222",
		"nats-server-name nats.kodama-system.svc.cluster.local",
		"nats-ca installation",
		"nats-token admin 600",
	} {
		if !strings.Contains(recorded, wanted) {
			t.Fatalf("the final command did not receive %q:\n%s", wanted, recorded)
		}
	}
	assertNoInheritedToken(t, recorded, true)
}

func assertRunStageCleansUp(t *testing.T, run fakeStageRun) {
	t.Helper()
	for _, leaked := range []string{"picker-secret-code", "project-token", "preexisting-exported-token"} {
		if strings.Contains(run.output, leaked) {
			t.Fatalf("run-stage.sh leaked credential material %q:\n%s", leaked, redactRunStageOutput(run.output))
		}
	}
	for line := range strings.SplitSeq(run.output, "\n") {
		if strings.HasPrefix(line, "+ ") {
			t.Fatalf("run-stage.sh emitted a shell trace:\n%s", redactRunStageOutput(run.output))
		}
	}
	if data, err := os.ReadFile(run.result); err != nil || strings.TrimSpace(string(data)) != "project-token" {
		t.Fatalf("the child command did not receive the acquired token: data=%q err=%v", data, err)
	}
	recorded := readMarker(t, run.marker)
	assertNoInheritedToken(t, recorded, true)
	assertWorkDirRemoved(t, run.tempDir)
	if !strings.Contains(recorded, "admin-file nonempty") || !strings.Contains(recorded, "legacy-token empty") {
		t.Fatalf("the final command did not receive the private admin token file only:\n%s", recorded)
	}
	for _, persona := range Personas {
		if !strings.Contains(recorded, "persona-file "+string(persona)+" 600") {
			t.Fatalf("the final command did not receive the private %s token file:\n%s", persona, recorded)
		}
	}
	if !strings.Contains(recorded, "require-stage yes") {
		t.Fatal("the final command did not observe Q01_REQUIRE_STAGE=1")
	}
	if !strings.Contains(recorded, "smoke-git 0") {
		t.Fatalf("the stage runner did not select the non-Git smoke mode:\n%s", recorded)
	}
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
	sawNoProxy := 0
	for line := range strings.SplitSeq(recorded, "\n") {
		if strings.HasPrefix(line, "curl-noproxy ") {
			sawNoProxy++
			if line != "curl-noproxy 1" {
				t.Fatalf("curl ran without --noproxy: %s", line)
			}
		}
	}
	if sawNoProxy == 0 {
		t.Fatal("the runner made no curl call; --noproxy is unproven")
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
	entries, err := os.ReadDir(run.evidence)
	if err != nil || len(entries) == 0 {
		t.Fatalf("the default evidence directory was not created: entries=%d err=%v", len(entries), err)
	}
}

// runFakeStage builds one isolated fake stage. kodamaCode is the readiness code
// the fake curl prints for the kodama URL; OAuthSonas always answers 303.
func runFakeStage(t *testing.T, kodamaCode string, overrides map[string]string) fakeStageRun {
	t.Helper()
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("locate real sleep: %v", err)
	}
	realTools := map[string]string{
		"Q01_REAL_RM":    "rm",
		"Q01_REAL_DATE":  "date",
		"Q01_REAL_MKDIR": "mkdir",
	}
	toolPaths := map[string]string{}
	for variable, name := range realTools {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("locate real %s: %v", name, err)
		}
		toolPaths[variable] = path
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	result := filepath.Join(t.TempDir(), "result")
	tempDir := t.TempDir()
	caPath, _ := testPKI(t)

	writeExecutable(t, filepath.Join(bin, "kubectl"), `#!/bin/sh
marker="$Q01_FAKE_MARKER"
printf 'kubectl-token %s\n' "${Q01_TOKEN:-empty}" >> "$marker"
case " $* " in
  *" port-forward "*)
    previous=""
    service=""
    for argument in "$@"; do
      if [ "$previous" = "port-forward" ]; then service="$argument"; fi
      previous="$argument"
    done
    name=${service#svc/}
    echo "started $name $$" >> "$marker"
    if [ "${Q01_FAKE_DIE_FORWARD:-0}" = "1" ]; then
      sleep 0.1
      exit 3
    fi
    if [ "${Q01_FAKE_IGNORE_TERM:-0}" = "1" ]; then
      trap '' TERM INT
    else
      trap 'echo "terminated $name $$" >> "$marker"; exit 0' TERM INT
    fi
    while :; do sleep 0.05; done
    ;;
  *" get secret "*)
    printf '%s' "$Q01_FAKE_ACCESS_CODE_B64"
    ;;
  *)
    echo "unexpected kubectl invocation" >&2
    exit 2
    ;;
esac
`)
	writeExecutable(t, filepath.Join(bin, "sleep"), `#!/bin/sh
printf 'sleep-token %s\n' "${Q01_TOKEN:-empty}" >> "$Q01_FAKE_MARKER"
exec "$Q01_REAL_SLEEP" "$@"
`)
	writeExecutable(t, filepath.Join(bin, "curl"), `#!/bin/sh
marker="$Q01_FAKE_MARKER"
printf 'curl-token %s\n' "${Q01_TOKEN:-empty}" >> "$marker"
has_noproxy=0
for argument in "$@"; do
  if [ "$argument" = "--noproxy" ]; then has_noproxy=1; fi
done
echo "curl-noproxy $has_noproxy" >> "$marker"
if [ -n "${Q01_FAKE_CURL_DELAY:-}" ]; then
  "$Q01_REAL_SLEEP" "$Q01_FAKE_CURL_DELAY"
fi
kodama_code="${Q01_FAKE_KODAMA_CODE:-401}"
for argument in "$@"; do
  case "$argument" in
    *kodama.kodama-system.svc.cluster.local*) printf '%s' "$kodama_code"; exit 0 ;;
    *oauthsonas.kodama-system.svc.cluster.local*) printf 303; exit 0 ;;
  esac
done
printf 000
exit 7
`)
	for variable, name := range map[string]string{"rm": "Q01_REAL_RM", "date": "Q01_REAL_DATE", "mkdir": "Q01_REAL_MKDIR"} {
		writeExecutable(t, filepath.Join(bin, variable), `#!/bin/sh
printf 'cmd-token `+variable+` %s\n' "${Q01_TOKEN:-empty}" >> "$Q01_FAKE_MARKER"
exec "$`+name+`" "$@"
`)
	}
	tokenScript := filepath.Join(t.TempDir(), "fake-token")
	writeExecutable(t, tokenScript, `#!/bin/sh
marker="$Q01_FAKE_MARKER"
printf 'token-script-token %s\n' "${Q01_TOKEN:-empty}" >> "$marker"
printf 'token-persona %s\n' "${1:-none}" >> "$marker"
if [ -n "${OAUTHSONAS_ACCESS_CODE_FILE:-}" ]; then
  mode=$(stat -c '%a' "$OAUTHSONAS_ACCESS_CODE_FILE")
  echo "access $mode $OAUTHSONAS_ACCESS_CODE_FILE" >> "$marker"
fi
case "${1:-}" in
  admin) printf '%s' "project-token" ;;
  engineer) printf '%s' "engineer-token" ;;
  reviewer) printf '%s' "reviewer-token" ;;
  operator) printf '%s' "operator-token" ;;
  *) printf '%s' "unknown-token" ;;
esac
`)
	runner := os.Getenv("Q01_RUN_STAGE_SCRIPT")
	if runner == "" {
		runner = filepath.Join(repoRootForTest(t), "test", "e2e", "internal", "harness", "run-stage.sh")
	}
	// Leave Q01_EVIDENCE_DIR unset so the date/mkdir child path is exercised and
	// the default evidence directory under TMPDIR is created.
	evidence := filepath.Join(tempDir, "oc", "q01-evidence")
	descriptor := filepath.Join(tempDir, "child-installation.json")
	accessCopy := filepath.Join(tempDir, "child-access-code")

	commandCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, runner, "sh", "-c", `printf 'admin-file %s\n' "${Q01_ADMIN_TOKEN_FILE:+nonempty}" >> "$Q01_FAKE_MARKER"
printf 'legacy-token %s\n' "${Q01_TOKEN:-empty}" >> "$Q01_FAKE_MARKER"
printf 'smoke-git %s\n' "${Q01_SMOKE_GIT:-unset}" >> "$Q01_FAKE_MARKER"
if [ -s "$Q01_ADMIN_TOKEN_FILE" ] && [ "$Q01_REQUIRE_STAGE" = "1" ]; then printf 'require-stage yes\n' >> "$Q01_FAKE_MARKER"; fi
for name in Q01_ADMIN_TOKEN_FILE Q01_ENGINEER_TOKEN_FILE Q01_REVIEWER_TOKEN_FILE Q01_OPERATOR_TOKEN_FILE; do
  eval "path=\${$name:-}"
  persona=${name#Q01_}
  persona=${persona%_TOKEN_FILE}
  persona=$(printf '%s' "$persona" | tr 'A-Z' 'a-z')
  if [ -n "$path" ] && [ -s "$path" ] && [ "$(stat -c '%a' "$path")" = "600" ]; then
    printf 'persona-file %s 600\n' "$persona" >> "$Q01_FAKE_MARKER"
  fi
done
if [ -n "${Q01_INSTALLATION_FILE:-}" ]; then
  cp -- "$Q01_INSTALLATION_FILE" "$Q01_FAKE_DESCRIPTOR_OUT"
  printf 'descriptor %s\n' "$(stat -c '%a' "$Q01_INSTALLATION_FILE")" >> "$Q01_FAKE_MARKER"
  access=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["oauthsonas"]["access_code_file"])' "$Q01_INSTALLATION_FILE")
  if [ -n "$access" ] && [ -f "$access" ]; then
    cp -- "$access" "$Q01_FAKE_ACCESS_COPY"
    printf 'access-live yes %s\n' "$(stat -c '%a' "$access")" >> "$Q01_FAKE_MARKER"
    printf '%s' "$access" > "$Q01_FAKE_ACCESS_PATH"
  fi
fi
if [ "${Q01_FAKE_SIGNAL_PARENT:-0}" = "1" ]; then kill -TERM "$PPID"; fi
printf 'nats-url %s\n' "${Q01_NATS_URL:-unset}" >> "$Q01_FAKE_MARKER"
printf 'nats-server-name %s\n' "${Q01_NATS_TLS_SERVER_NAME:-unset}" >> "$Q01_FAKE_MARKER"
if [ -n "${Q01_NATS_CA_FILE:-}" ] && [ "$Q01_NATS_CA_FILE" = "$Q01_CA_PEM" ]; then
  printf 'nats-ca installation\n' >> "$Q01_FAKE_MARKER"
fi
if [ -n "${Q01_NATS_TOKEN_FILE:-}" ] && [ "$Q01_NATS_TOKEN_FILE" = "$Q01_ADMIN_TOKEN_FILE" ] && [ "$(stat -c '%a' "$Q01_NATS_TOKEN_FILE")" = "600" ]; then
  printf 'nats-token admin 600\n' >> "$Q01_FAKE_MARKER"
fi
cat "$Q01_ADMIN_TOKEN_FILE" > "$Q01_FAKE_RESULT"`)
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
		"Q01_KUBE_CONTEXT":         "kodama-stage-k8s",
		"Q01_CA_PEM":               caPath,
		"Q01_PROJECT_ID":           "tenant-a",
		"Q01_TOKEN_SCRIPT":         tokenScript,
		"Q01_API_PORT":             "19443",
		"Q01_OAUTHSONAS_PORT":      "8443",
		"Q01_READY_ATTEMPTS":       "2",
		"Q01_READY_INTERVAL":       "0.01",
		"Q01_CLEANUP_ATTEMPTS":     "50",
		"Q01_CLEANUP_INTERVAL":     "0.01",
		"Q01_REAL_SLEEP":           realSleep,
		"Q01_FAKE_KODAMA_CODE":     kodamaCode,
		"Q01_FAKE_MARKER":          marker,
		"Q01_FAKE_RESULT":          result,
		"Q01_FAKE_DESCRIPTOR_OUT":  descriptor,
		"Q01_FAKE_ACCESS_COPY":     accessCopy,
		"Q01_FAKE_ACCESS_PATH":     filepath.Join(tempDir, "access-path"),
		"Q01_FAKE_ACCESS_CODE_B64": "cGlja2VyLXNlY3JldC1jb2Rl",
		"Q01_TOKEN":                "preexisting-exported-token",
	}
	maps.Copy(environment, toolPaths)
	maps.Copy(environment, overrides)
	command.Env = make([]string, 0, len(environment))
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	started := time.Now()
	output, err := command.CombinedOutput()
	return fakeStageRun{
		output:     string(output),
		err:        err,
		elapsed:    time.Since(started),
		marker:     marker,
		result:     result,
		evidence:   evidence,
		tempDir:    tempDir,
		descriptor: descriptor,
		accessCopy: accessCopy,
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

// assertNoInheritedToken proves the long-lived and cleanup children never saw a
// caller's exported token or the acquired token. The token script, cleanup
// sleeps, and the rm/date/mkdir children are included so an early export or a
// missing cleanup unset is caught.
func assertNoInheritedToken(t *testing.T, recorded string, requireTokenScript bool) {
	t.Helper()
	seen := map[string]int{}
	seenCommands := map[string]int{}
	seenPersonas := map[string]int{}
	for line := range strings.SplitSeq(recorded, "\n") {
		if strings.HasPrefix(line, "cmd-token ") {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				t.Fatalf("malformed command-token observation: %q", line)
			}
			seenCommands[fields[1]]++
			if fields[2] != "empty" {
				t.Fatalf("%s child saw a token: %s", fields[1], line)
			}
			continue
		}
		if strings.HasPrefix(line, "token-persona ") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				t.Fatalf("malformed token-persona observation: %q", line)
			}
			seenPersonas[fields[1]]++
			continue
		}
		for _, prefix := range []string{"kubectl-token ", "curl-token ", "token-script-token ", "sleep-token "} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			seen[prefix]++
			if line != prefix+"empty" {
				t.Fatalf("%s child saw a token: %s", strings.TrimSuffix(prefix, " "), line)
			}
		}
	}
	for _, prefix := range []string{"kubectl-token ", "curl-token ", "sleep-token "} {
		if seen[prefix] == 0 {
			t.Fatalf("token-environment observation missing for %s", strings.TrimSuffix(prefix, " "))
		}
	}
	if requireTokenScript {
		if seen["token-script-token "] != len(Personas) {
			t.Fatalf("token script observations = %d, want %d", seen["token-script-token "], len(Personas))
		}
		for _, persona := range Personas {
			if seenPersonas[string(persona)] != 1 {
				t.Fatalf("token script observations for %s = %d, want 1", persona, seenPersonas[string(persona)])
			}
		}
		for _, command := range []string{"rm", "date", "mkdir"} {
			if seenCommands[command] == 0 {
				t.Fatalf("token-environment observation missing for %s", command)
			}
		}
	}
}

// assertWorkDirRemoved proves the private runner work directory was removed.
func assertWorkDirRemoved(t *testing.T, tempDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(tempDir, "q01-stage.*"))
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
	for _, marker := range []string{"picker-secret-code", "project-token", "preexisting-exported-token"} {
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

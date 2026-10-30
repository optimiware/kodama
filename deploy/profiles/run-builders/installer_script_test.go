package runbuilders

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scriptEnv runs installer/install.sh against temporary directories with a fake nsenter and a
// fake apparmor_parser: no host, kernel or cluster is touched.
type scriptEnv struct {
	t                                     *testing.T
	dir, profiles, seccomp, state, loaded string
	abi                                   string
}

func newScriptEnv(t *testing.T) *scriptEnv {
	t.Helper()
	for _, tool := range []string{"sh", "cmp", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	e := &scriptEnv{t: t, dir: dir, profiles: filepath.Join(dir, "profiles"), seccomp: filepath.Join(dir, "seccomp"),
		state: filepath.Join(dir, "state"), loaded: filepath.Join(dir, "loaded"), abi: filepath.Join(dir, "abi-4.0")}
	for _, d := range []string{e.profiles, e.seccomp, filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := Embedded()
	e.write(filepath.Join(e.profiles, SeccompFile), string(p.Seccomp))
	e.write(filepath.Join(e.profiles, AppArmorProfile), string(p.AppArmor))
	e.write(e.loaded, "")
	e.write(e.abi, "")
	e.write(filepath.Join(dir, "bin", "nsenter"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n")
	e.write(filepath.Join(dir, "bin", "apparmor_parser"), `#!/bin/sh
# fake: -r loads stdin as the profile name, -R removes it.
case "$1" in
  -r)
    cat >/dev/null
    echo "$*" >>"$PARSER_LOG"
    [ -n "${FAIL_PARSER:-}" ] && exit 1
    grep -v '^kodama-rootless-build-v2 ' "$AA_PROFILES_FILE" >"$AA_PROFILES_FILE.tmp" || true
    echo "kodama-rootless-build-v2 (unconfined)" >>"$AA_PROFILES_FILE.tmp"
    mv "$AA_PROFILES_FILE.tmp" "$AA_PROFILES_FILE"
    ;;
  -R) cat >/dev/null; echo "-R" >>"$PARSER_LOG"; : >"$AA_PROFILES_FILE" ;;
esac
`)
	// The resident loop sleeps between health checks; the fake sleep returns at once and, like a
	// node whose AppArmor state changed in the meantime, can switch the profile to complain mode
	// or make the kernel's profile list unreadable.
	e.write(filepath.Join(dir, "bin", "sleep"), `#!/bin/sh
echo "$1 ready=$([ -e "$STATE_DIR/ready" ] && echo yes || echo no)" >>"$SLEEP_LOG"
action=${SLEEP_ACTION:-}
if [ -n "${SLEEP_PLAN:-}" ] && [ -s "$SLEEP_PLAN" ]; then
  action=$(head -n1 "$SLEEP_PLAN")
  sed 1d "$SLEEP_PLAN" >"$SLEEP_PLAN.n" && mv "$SLEEP_PLAN.n" "$SLEEP_PLAN"
fi
case "$action" in
  restore) echo "kodama-rootless-build-v2 (unconfined)" >"$AA_PROFILES_FILE" ;;
  complain) echo "kodama-rootless-build-v2 (complain)" >"$AA_PROFILES_FILE" ;;
  enforce) echo "kodama-rootless-build-v2 (enforce)" >"$AA_PROFILES_FILE" ;;
  remove) rm -f "$AA_PROFILES_FILE" ;;
esac
`)
	for _, name := range []string{"nsenter", "apparmor_parser", "sleep"} {
		if err := os.Chmod(filepath.Join(dir, "bin", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *scriptEnv) write(path, content string) {
	e.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *scriptEnv) run(extra ...string) (string, error) {
	cmd := exec.Command("sh", "installer/install.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(e.dir, "bin")+":"+os.Getenv("PATH"),
		"PROFILES_DIR="+e.profiles, "HOST_SECCOMP_DIR="+e.seccomp, "STATE_DIR="+e.state,
		"AA_PROFILES_FILE="+e.loaded, "AA_ABI_FILE="+e.abi, "PARSER_LOG="+filepath.Join(e.dir, "parser.log"), "SLEEP_LOG="+filepath.Join(e.dir, "sleep.log"), "RUN_ONCE=1")
	cmd.Env = append(cmd.Env, extra...)
	if len(extra) > 0 && strings.HasPrefix(extra[0], "RUN_CHECKS=") {
		cmd.Env = append(cmd.Env, "RUN_ONCE=0")
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func Test_unit_builder_profile_installer_script(t *testing.T) {
	e := newScriptEnv(t)
	out, err := e.run()
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	installed, _ := os.ReadFile(filepath.Join(e.seccomp, SeccompFile))
	if !bytes.Equal(installed, Embedded().Seccomp) {
		t.Fatal("seccomp profile not installed byte-for-byte")
	}
	status, _ := os.ReadFile(filepath.Join(e.seccomp, "INSTALLED"))
	for _, want := range []string{"profile=" + ProfileVersion, "seccomp=kodama/" + SeccompFile, "apparmor=" + AppArmorProfile, "applied="} {
		if !strings.Contains(string(status), want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
	if _, err := os.Stat(filepath.Join(e.state, "ready")); err != nil {
		t.Fatal("ready marker missing")
	}
	if loaded, _ := os.ReadFile(e.loaded); !strings.Contains(string(loaded), AppArmorProfile) {
		t.Fatal("apparmor profile not loaded")
	}
	leftovers, _ := filepath.Glob(filepath.Join(e.seccomp, ".*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}

	// Re-running (pod restart or node reboot that cleared loaded profiles) re-applies and repairs
	// a corrupted seccomp file.
	e.write(e.loaded, "")
	e.write(filepath.Join(e.seccomp, SeccompFile), "corrupt")
	if out, err := e.run(); err != nil {
		t.Fatalf("re-apply: %v\n%s", err, out)
	}
	if again, _ := os.ReadFile(filepath.Join(e.seccomp, SeccompFile)); !bytes.Equal(again, Embedded().Seccomp) {
		t.Fatal("seccomp profile not repaired")
	}
	if loaded, _ := os.ReadFile(e.loaded); !strings.Contains(string(loaded), AppArmorProfile) {
		t.Fatal("apparmor profile not reloaded")
	}

	// Failure paths fail loudly (the Pod restarts and never reports ready).
	failing := newScriptEnv(t)
	if out, err := failing.run("FAIL_PARSER=1"); err == nil || !strings.Contains(out, "could not load") {
		t.Fatalf("parser failure was not reported: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(failing.state, "ready")); err == nil {
		t.Fatal("ready marker written despite a failed load")
	}
	noABI := newScriptEnv(t)
	if err := os.Remove(noABI.abi); err != nil {
		t.Fatal(err)
	}
	if out, err := noABI.run(); err == nil || !strings.Contains(out, "AppArmor 4.x required") {
		t.Fatalf("missing abi/4.0 was not reported: %v\n%s", err, out)
	}

	// Uninstall removes the files and unloads the profile.
	if out, err := e.run("MODE=uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	for _, name := range []string{SeccompFile, "INSTALLED"} {
		if _, err := os.Stat(filepath.Join(e.seccomp, name)); err == nil {
			t.Errorf("%s still present after uninstall", name)
		}
	}
	if loaded, _ := os.ReadFile(e.loaded); strings.Contains(string(loaded), AppArmorProfile) {
		t.Fatal("apparmor profile still loaded after uninstall")
	}
}

func (e *scriptEnv) parserCalls() int {
	raw, _ := os.ReadFile(filepath.Join(e.dir, "parser.log"))
	return len(strings.Fields(string(raw)))
}

func Test_unit_builder_profile_installer_unconfined_state(t *testing.T) {
	e := newScriptEnv(t)
	// A resident check that finds the profile in complain mode reloads it and ends listed as "(unconfined)".
	out, err := e.run("RUN_CHECKS=1", "SLEEP_ACTION=complain")
	if err != nil {
		t.Fatalf("resident check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "is other, not \""+AppArmorKernelState+"\"; reloading") {
		t.Fatalf("complain mode was not detected:\n%s", out)
	}
	if loaded, _ := os.ReadFile(e.loaded); strings.TrimSpace(string(loaded)) != AppArmorKernelState {
		t.Fatalf("profile list after reload = %q", loaded)
	}
	if calls := e.parserCalls(); calls < 4 { // two loads, each "-r -K"
		t.Fatalf("expected the profile to be loaded twice, parser args seen %d", calls)
	}
	if _, err := os.Stat(filepath.Join(e.state, "ready")); err != nil {
		t.Fatal("ready marker missing after the reload")
	}

	// A profile listed as "(unconfined)" is left alone by the resident check.
	healthy := newScriptEnv(t)
	if out, err := healthy.run("RUN_CHECKS=1"); err != nil || strings.Contains(out, "reloading") {
		t.Fatalf("healthy check reloaded: %v\n%s", err, out)
	}
	if calls := healthy.parserCalls(); calls != 2 {
		t.Fatalf("healthy node loaded the profile more than once: %d", calls)
	}

	// Only the exact "(unconfined)" entry is healthy: a resident check that finds the name listed
	// in enforce mode (a different profile body loaded under the name) reloads it too.
	enforcing := newScriptEnv(t)
	if out, err := enforcing.run("RUN_CHECKS=1", "SLEEP_ACTION=enforce"); err != nil || !strings.Contains(out, "is other, not") {
		t.Fatalf("enforce-mode listing was accepted as healthy: %v\n%s", err, out)
	}
	// A load that leaves the profile in complain or enforce mode is a failure, not success.
	enforceLoad := newScriptEnv(t)
	enforceLoad.write(filepath.Join(enforceLoad.dir, "bin", "apparmor_parser"), "#!/bin/sh\ncat >/dev/null\necho 'kodama-rootless-build-v2 (enforce)' >\"$AA_PROFILES_FILE\"\n")
	if out, err := enforceLoad.run(); err == nil || !strings.Contains(out, "is not listed as") {
		t.Fatalf("enforce-mode load accepted: %v\n%s", err, out)
	}
	complaining := newScriptEnv(t)
	complaining.write(filepath.Join(complaining.dir, "bin", "apparmor_parser"), "#!/bin/sh\ncat >/dev/null\necho 'kodama-rootless-build-v2 (complain)' >\"$AA_PROFILES_FILE\"\n")
	if out, err := complaining.run(); err == nil || !strings.Contains(out, "is not listed as") {
		t.Fatalf("complain-mode load accepted: %v\n%s", err, out)
	}
}

func Test_unit_builder_profile_installer_inspection_failure(t *testing.T) {
	// Resident check: an unreadable profile list is reported, makes the Pod not ready, and does
	// not trigger a reload or pass as healthy.
	e := newScriptEnv(t)
	out, err := e.run("RUN_CHECKS=1", "SLEEP_ACTION=remove")
	if err != nil {
		t.Fatalf("resident check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cannot inspect the kernel profile list; will retry") {
		t.Fatalf("inspection failure not reported:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.state, "ready")); err == nil {
		t.Fatal("ready marker present although health is unknown")
	}
	if calls := e.parserCalls(); calls != 2 {
		t.Fatalf("an inspection failure must not reload: %d", calls)
	}

	// Uninstall: an unreadable list is a failure with nothing verified; absence is success.
	u := newScriptEnv(t)
	if err := os.Remove(u.loaded); err != nil {
		t.Fatal(err)
	}
	if out, err := u.run("MODE=uninstall"); err == nil || !strings.Contains(out, "removal of "+AppArmorProfile+" not verified") {
		t.Fatalf("uninstall reported success without inspecting: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(u.state, "ready")); err == nil {
		t.Fatal("uninstall reported ready without verifying removal")
	}
	u.write(u.loaded, "")
	if out, err := u.run("MODE=uninstall"); err != nil {
		t.Fatalf("uninstall of an absent profile: %v\n%s", err, out)
	}
	// A removal that leaves the profile loaded fails.
	stuck := newScriptEnv(t)
	stuck.write(stuck.loaded, AppArmorProfile+" (complain)\n")
	stuck.write(filepath.Join(stuck.dir, "bin", "apparmor_parser"), "#!/bin/sh\ncat >/dev/null\n")
	if out, err := stuck.run("MODE=uninstall"); err == nil || !strings.Contains(out, "still loaded") {
		t.Fatalf("uninstall accepted a profile that is still loaded: %v\n%s", err, out)
	}
}

func Test_unit_builder_profile_installer_readiness_recovers(t *testing.T) {
	e := newScriptEnv(t)
	plan := filepath.Join(e.dir, "plan")
	// Checks: healthy, the kernel's profile list unreadable, readable again, healthy.
	e.write(plan, "none\nremove\nrestore\nnone\n")
	out, err := e.run("RUN_CHECKS=4", "SLEEP_PLAN="+plan, "CHECK_INTERVAL=300")
	if err != nil {
		t.Fatalf("resident checks: %v\n%s", err, out)
	}
	trace, _ := os.ReadFile(filepath.Join(e.dir, "sleep.log"))
	// Each pair is the sleep argument and whether ready existed just before that wait. The third
	// wait follows the inspection failure (not ready, short retry); the fourth follows the
	// recovery (ready again, normal interval restored).
	got := strings.Fields(string(trace))
	want := []string{"300", "ready=yes", "300", "ready=yes", "10", "ready=no", "300", "ready=yes"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("sleep trace = %v, want %v\n%s", got, want, out)
	}
	if !strings.Contains(out, "cannot inspect the kernel profile list; will retry") || !strings.Contains(out, "profiles verified again; ready") {
		t.Fatalf("failure/recovery not logged:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.state, "ready")); err != nil {
		t.Fatal("readiness was not restored after the successful check")
	}
	if e.parserCalls() != 2 {
		t.Fatalf("recovery must not reload a healthy profile: %d parser args", e.parserCalls())
	}
	// A drifted seccomp file is repaired by the same successful check.
	drift := newScriptEnv(t)
	drift.write(filepath.Join(drift.dir, "bin", "sleep"), "#!/bin/sh\necho corrupt >\""+filepath.Join(drift.seccomp, SeccompFile)+"\"\n")
	if err := os.Chmod(filepath.Join(drift.dir, "bin", "sleep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := drift.run("RUN_CHECKS=1"); err != nil {
		t.Fatalf("drift check: %v\n%s", err, out)
	}
	if repaired, _ := os.ReadFile(filepath.Join(drift.seccomp, SeccompFile)); !bytes.Equal(repaired, Embedded().Seccomp) {
		t.Fatal("seccomp drift not repaired")
	}
}

package runprofile_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runVerify runs verify.sh with an explicit, minimal environment and a stub
// `go` first on PATH. It returns the combined output, the exit code and the
// stub's argument log.
func runVerify(t *testing.T, env map[string]string, goBody string) (out string, code int, argsLog string) {
	t.Helper()
	script, err := filepath.Abs("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(script); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("verify.sh missing or not executable: %v", err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "go-args.log")
	stub := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> " + log + "\nprintf 'cwd=%s\\n' \"$PWD\" >> " + log + "\nprintf 'GOFLAGS=%s\\n' \"${GOFLAGS:-}\" >> " + log + "\n" + goBody + "\n"
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	return buf.String(), code, string(data)
}

func verifyEnv(t *testing.T) map[string]string {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte("apiVersion: v1\ntoken: super-secret-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"KODAMA_RUNPROFILE_KUBECONFIG":    kc,
		"KODAMA_RUNPROFILE_TARGET":        "stage-probe",
		"KODAMA_RUNPROFILE_STORAGE_CLASS": "sbs-default",
		"KODAMA_RUNPROFILE_REPORT_DIR":    t.TempDir(),
	}
}

const passingGo = `echo '{"Action":"run","Package":"p","Test":"Test_run_resource_profile_qualification"}'
echo '{"Action":"pass","Package":"p","Test":"Test_run_resource_profile_qualification","Elapsed":1}'`

func Test_unit_runprofile_verify_wrapper(t *testing.T) {
	// Missing inputs are refused before anything runs, naming each one.
	for _, name := range []string{"KODAMA_RUNPROFILE_KUBECONFIG", "KODAMA_RUNPROFILE_TARGET", "KODAMA_RUNPROFILE_STORAGE_CLASS", "KODAMA_RUNPROFILE_REPORT_DIR"} {
		env := verifyEnv(t)
		delete(env, name)
		out, code, args := runVerify(t, env, passingGo)
		if code == 0 || !strings.Contains(out, name) || args != "" {
			t.Errorf("missing %s: exit %d, go run %q, output %q", name, code, args, out)
		}
	}
	if out, code, args := runVerify(t, map[string]string{}, passingGo); code == 0 || args != "" || !strings.Contains(out, "refusing") {
		t.Errorf("empty environment: exit %d args %q out %q", code, args, out)
	}
	// An unreadable kubeconfig, and build tags in GOFLAGS, are refused.
	env := verifyEnv(t)
	env["KODAMA_RUNPROFILE_KUBECONFIG"] = filepath.Join(t.TempDir(), "absent")
	if _, code, args := runVerify(t, env, passingGo); code == 0 || args != "" {
		t.Errorf("unreadable kubeconfig ran: exit %d args %q", code, args)
	}
	env = verifyEnv(t)
	env["GOFLAGS"] = "-tags=unit"
	if _, code, args := runVerify(t, env, passingGo); code == 0 || args != "" {
		t.Errorf("-tags in GOFLAGS ran: exit %d args %q", code, args)
	}

	// With every input the real test is invoked with exactly the contract's
	// arguments (no -tags unit, no -v noise) from the repository root, and the
	// JSON goes to FORGE_RESULTS.
	env = verifyEnv(t)
	results := filepath.Join(t.TempDir(), "nested", "results.json")
	env["FORGE_RESULTS"] = results
	out, code, args := runVerify(t, env, passingGo)
	if code != 0 {
		t.Fatalf("passing run exited %d: %s", code, out)
	}
	want := "test -count=1 -timeout 90m -json -run Test_run_resource_profile_qualification ./test/integration/kube/runprofile/"
	if !strings.Contains(args, want+"\n") || strings.Contains(args, "tags") {
		t.Errorf("go invoked with %q, want %q and no -tags", args, want)
	}
	if !strings.Contains(args, "cwd=") || strings.Contains(args, "cwd=/\n") {
		t.Errorf("not run from the repository root: %q", args)
	}
	data, err := os.ReadFile(results)
	if err != nil || !strings.Contains(string(data), `"Action":"pass"`) {
		t.Errorf("FORGE_RESULTS not written: %v %q", err, data)
	}
	if !strings.Contains(out, results) {
		t.Errorf("results path not reported: %q", out)
	}
	// Credentials never reach the output.
	for _, secret := range []string{"super-secret-token-value", env["KODAMA_RUNPROFILE_KUBECONFIG"]} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks %q: %s", secret, out)
		}
	}

	// Without FORGE_RESULTS a temp file is used and its path printed.
	env = verifyEnv(t)
	out, code, _ = runVerify(t, env, passingGo)
	if code != 0 || !strings.Contains(out, "kodama-runprofile-results.") {
		t.Errorf("temp results not reported: exit %d %q", code, out)
	}

	// A failing run keeps its exit status and its outcomes.
	env = verifyEnv(t)
	env["FORGE_RESULTS"] = filepath.Join(t.TempDir(), "r.json")
	failing := `echo '{"Action":"fail","Package":"p","Test":"Test_run_resource_profile_qualification"}'
exit 1`
	if _, code, _ := runVerify(t, env, failing); code != 1 {
		t.Errorf("failing run exited %d", code)
	}
	if data, _ := os.ReadFile(env["FORGE_RESULTS"]); !strings.Contains(string(data), `"Action":"fail"`) {
		t.Errorf("failing outcomes not captured: %q", data)
	}
	// A skipped or absent real test is never a pass, even with exit status 0.
	for name, body := range map[string]string{
		"skipped": `echo '{"Action":"skip","Package":"p","Test":"Test_run_resource_profile_qualification"}'`,
		"absent":  `echo '{"Action":"pass","Package":"p","Test":"Test_other"}'`,
		"silent":  `true`,
	} {
		env = verifyEnv(t)
		if out, code, _ := runVerify(t, env, body); code == 0 {
			t.Errorf("%s run reported success: %s", name, out)
		}
	}
}

// The wrapper takes no second lease: the test acquires it itself.
func Test_unit_runprofile_verify_wrapper_takes_no_lease(t *testing.T) {
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"flock", "kodama-run-resources-integration.lock", "-tags unit", "set -x"} {
		for _, l := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			if strings.Contains(l, forbidden) {
				t.Errorf("verify.sh contains %q: %s", forbidden, l)
			}
		}
	}
}

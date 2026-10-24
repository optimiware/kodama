package entrypoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testUID1 = "11111111-1111-4111-8111-111111111111"
	testUID2 = "22222222-2222-4222-8222-222222222222"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func validEnv() map[string]string {
	return map[string]string{
		"KODAMA_SERVER_URL": "https://kodama.example",
		"KODAMA_ATTEMPT_ID": "att_1",
		"KODAMA_JOB_NAME":   "job-1",
		"KODAMA_JOB_UID":    testUID1,
		"KODAMA_POD_NAME":   "job-1-abc",
		"KODAMA_POD_UID":    testUID2,
		"PATH":              "/usr/bin",
		"SECRET_ENV":        "must-not-leak",
	}
}

func TestParseArgsFromEnvironmentWithDefaults(t *testing.T) {
	cfg, err := ParseArgs(nil, envMap(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != "https://kodama.example" || cfg.AttemptID != "att_1" || cfg.Job.UID != testUID1 || cfg.Job.PodUID != testUID2 {
		t.Fatalf("cfg = %+v", cfg)
	}
	for got, want := range map[string]string{
		cfg.WorkspaceDir: "/workspace", cfg.OutputsDir: "/kodama/outputs", cfg.ResultsFile: "/kodama/results.json",
		cfg.SecretsDir: "/kodama/secrets", cfg.IdentityDir: "/kodama/identity",
		cfg.AdmissionCredentialFile: "/kodama/identity/admission", cfg.GitBinary: "git", cfg.ClientVersion: Version,
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if cfg.ChildEnv["PATH"] != "/usr/bin" || len(cfg.ChildEnv) != 1 {
		t.Fatalf("child env must be an allowlist, got %v", cfg.ChildEnv)
	}
	if cfg.KillGrace != 10*time.Second || cfg.Retry.MaxAttempts < 1 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestParseArgsStepPrefixedEnvAndFlagPrecedence(t *testing.T) {
	env := validEnv()
	env["KODAMA_STEP_ATTEMPT_ID"] = "att_step"
	env["KODAMA_STEP_WORKSPACE_DIR"] = "/ws-step"
	env["KODAMA_WORKSPACE_DIR"] = "/ws-short"
	env["KODAMA_STEP_KILL_GRACE"] = "3s"
	cfg, err := ParseArgs([]string{"--outputs-dir", "/out-flag", "--job-name", "flag-job"}, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AttemptID != "att_step" || cfg.WorkspaceDir != "/ws-step" || cfg.KillGrace != 3*time.Second {
		t.Fatalf("env precedence wrong: %+v", cfg)
	}
	if cfg.OutputsDir != "/out-flag" || cfg.Job.Name != "flag-job" {
		t.Fatalf("flags must override env: %+v", cfg)
	}
	env["KODAMA_STEP_ATTEMPT_ID"] = "att_env"
	cfg, err = ParseArgs([]string{"--attempt-id", "att_flag"}, envMap(env))
	if err != nil || cfg.AttemptID != "att_flag" {
		t.Fatalf("cfg=%+v err=%v", cfg.AttemptID, err)
	}
}

func TestParseArgsJobIdentityFromIdentityDir(t *testing.T) {
	dir := t.TempDir()
	for f, v := range map[string]string{"job-name": "job-9\n", "job-uid": testUID1 + "\n", "pod-name": "pod-9", "pod-uid": testUID2} {
		os.WriteFile(filepath.Join(dir, f), []byte(v), 0o600)
	}
	env := validEnv()
	for _, k := range []string{"KODAMA_JOB_NAME", "KODAMA_JOB_UID", "KODAMA_POD_NAME", "KODAMA_POD_UID"} {
		delete(env, k)
	}
	env["KODAMA_IDENTITY_DIR"] = dir
	cfg, err := ParseArgs(nil, envMap(env))
	if err != nil || cfg.Job.Name != "job-9" || cfg.Job.PodName != "pod-9" {
		t.Fatalf("cfg=%+v err=%v", cfg.Job, err)
	}
}

func TestParseArgsRejections(t *testing.T) {
	mut := func(f func(map[string]string)) map[string]string { e := validEnv(); f(e); return e }
	cases := map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no server":        {nil, mut(func(e map[string]string) { delete(e, "KODAMA_SERVER_URL") }), "required"},
		"no attempt":       {nil, mut(func(e map[string]string) { delete(e, "KODAMA_ATTEMPT_ID") }), "required"},
		"http remote":      {nil, mut(func(e map[string]string) { e["KODAMA_SERVER_URL"] = "http://kodama.example" }), "loopback"},
		"url credentials":  {nil, mut(func(e map[string]string) { e["KODAMA_SERVER_URL"] = "https://u:p@kodama.example" }), "credentials"},
		"ftp":              {nil, mut(func(e map[string]string) { e["KODAMA_SERVER_URL"] = "ftp://kodama.example" }), "https"},
		"bad attempt":      {nil, mut(func(e map[string]string) { e["KODAMA_ATTEMPT_ID"] = "../x" }), ""},
		"bad job uid":      {nil, mut(func(e map[string]string) { e["KODAMA_JOB_UID"] = "nope" }), "job identity"},
		"positional":       {[]string{"extra"}, validEnv(), "flags only"},
		"unknown flag":     {[]string{"--nope"}, validEnv(), ""},
		"bad log level":    {[]string{"--log-level", "loud"}, validEnv(), "log level"},
		"secrets in work":  {[]string{"--workspace-dir", "/w", "--secrets-dir", "/w/secrets"}, validEnv(), "overlap"},
		"outputs in work":  {[]string{"--workspace-dir", "/w", "--outputs-dir", "/w/out"}, validEnv(), "overlap"},
		"results in outs":  {[]string{"--outputs-dir", "/o", "--results-file", "/o/results.json"}, validEnv(), "outside"},
		"token in secrets": {[]string{"--secrets-dir", "/s", "--token-file", "/s/token"}, validEnv(), "outside"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseArgs(c.args, envMap(c.env))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q lacks %q", err, c.want)
			}
		})
	}
}

func TestParseArgsAllowsLoopbackHTTP(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8080", "http://localhost:9", "http://[::1]:80"} {
		env := validEnv()
		env["KODAMA_SERVER_URL"] = u
		if _, err := ParseArgs(nil, envMap(env)); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

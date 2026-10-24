package entrypoint

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func TestStepCommandRegistration(t *testing.T) {
	c := StepCommand()
	if c.Name != "step" || CommandName != "step" || c.Summary == "" || c.Run == nil {
		t.Fatalf("command = %+v", c)
	}
}

func TestRunUsageErrorsAndHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), nil, noEnv, &out, &errb); code != ExitUsage || !strings.Contains(errb.String(), "--server-url") {
		t.Fatalf("no flags: exit %d, stderr %q", code, errb.String())
	}
	for name, args := range map[string][]string{
		"unknown flag":   {"--bogus"},
		"positional":     {"--server-url", "https://runtime.example.invalid", "--attempt-id", "att_1", "extra"},
		"bad log level":  {"--server-url", "https://runtime.example.invalid", "--attempt-id", "att_1", "--log-level", "loud"},
		"bad attempt id": {"--server-url", "https://runtime.example.invalid", "--attempt-id", "not an id"},
		"insecure url":   {"--server-url", "http://runtime.example.invalid", "--attempt-id", "att_1"},
	} {
		out.Reset()
		errb.Reset()
		if code := Run(context.Background(), args, noEnv, &out, &errb); code != ExitUsage {
			t.Errorf("%s: exit %d", name, code)
		}
		if out.Len() != 0 {
			t.Errorf("%s: usage errors must not write to stdout", name)
		}
	}
	out.Reset()
	errb.Reset()
	if code := Run(context.Background(), []string{"-h"}, noEnv, &out, &errb); code != ExitOK || !strings.Contains(out.String(), "Usage: kodama step") || errb.Len() != 0 {
		t.Fatalf("help: exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

// bootstrapDir lays out identity files for a Run against a test server.
func bootstrapDir(t *testing.T, serverURL string) []string {
	t.Helper()
	root := t.TempDir()
	identity := filepath.Join(root, "identity")
	if err := os.MkdirAll(identity, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identity, "admission"), []byte(unitAdmission), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"--server-url", serverURL, "--attempt-id", "att_unit01", "--identity-dir", identity,
		"--workspace-dir", filepath.Join(root, "ws"), "--outputs-dir", filepath.Join(root, "out"), "--secrets-dir", filepath.Join(root, "sec"),
		"--results-file", filepath.Join(root, "results.json"), "--scratch-dir", root,
		"--job-name", unitJob.Name, "--job-uid", unitJob.UID, "--pod-name", unitJob.PodName, "--pod-uid", unitJob.PodUID,
	}
}

func TestRunExitCodesAgainstAServer(t *testing.T) {
	respond := func(status int, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	revoked := `{"error":{"code":"attempt_revoked","message":"cancelled","requestId":"r1","details":{}}}`
	profile := `{"error":{"code":"profile_unsupported","message":"old wrapper","requestId":"r1","details":{}}}`

	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"cancelled attempt", 403, revoked, ExitRevoked},
		{"incompatible wrapper", 422, profile, ExitAdmission},
		{"unparseable refusal", 400, `nope`, ExitAdmission},
		{"success status with a body that is not a contract response", 200, `{"x":1}`, ExitAdmission},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := respond(tc.status, tc.body)
			var out, errb safeBuf
			code := Run(context.Background(), append(bootstrapDir(t, srv.URL), "--log-level", "debug"), noEnv, &out, &errb)
			if code != tc.want {
				t.Fatalf("exit %d, want %d\nstderr: %s", code, tc.want, errb.String())
			}
			if !strings.Contains(errb.String(), `"msg":"admission failed"`) {
				t.Fatalf("the failure is not logged as structured JSON: %s", errb.String())
			}
			if strings.Contains(errb.String(), unitAdmission) {
				t.Fatal("the admission credential was logged")
			}
		})
	}

	t.Run("unreachable server", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		var out, errb safeBuf
		done := make(chan int, 1)
		go func() { done <- Run(context.Background(), bootstrapDir(t, url), noEnv, &out, &errb) }()
		select {
		case code := <-done:
			if code != ExitAdmission {
				t.Fatalf("exit %d\n%s", code, errb.String())
			}
		case <-time.After(60 * time.Second):
			t.Fatal("admission against an unreachable server is unbounded")
		}
	})
}

func TestExecuteRejectsIncompleteConfigurationAndBadCA(t *testing.T) {
	if code := Execute(context.Background(), Config{}); code != ExitUsage {
		t.Fatalf("empty config: exit %d", code)
	}
	u := newUnit(t, `exit 0`)
	cfg := u.cfg
	cfg.ServerURL = "http://127.0.0.1:1"
	cfg.CAFile = filepath.Join(t.TempDir(), "missing.pem")
	if code := Execute(context.Background(), cfg); code != ExitUsage {
		t.Fatalf("missing CA file: exit %d", code)
	}
	cfg.CAFile = ""
	cfg.SecretsDir = filepath.Join(cfg.WorkspaceDir, "secrets") // overlaps the workspace
	if code := Execute(context.Background(), cfg); code != ExitUsage {
		t.Fatalf("overlapping directories: exit %d", code)
	}
}

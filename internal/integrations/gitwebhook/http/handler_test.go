package gitwebhookhttp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	gitwebhookhttp "github.com/optimiware/kodama/internal/integrations/gitwebhook/http"
)

func serve(t *testing.T, f *gitwebhooktest.Fixture) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	gitwebhookhttp.New(f.Service).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, n gw.Notification) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/git-hooks/"+string(n.TriggerID), bytes.NewReader(n.Body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(gw.HeaderDeliveryID, n.DeliveryID)
	req.Header.Set(gw.HeaderTimestamp, n.Timestamp)
	req.Header.Set(gw.HeaderSignature, n.Signature)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func push(f *gitwebhooktest.Fixture) []byte {
	return gitwebhooktest.Marshal(f.T, f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))
}

func TestHookEndpoint(t *testing.T) {
	f := gitwebhooktest.NewFixture(t)
	srv := serve(t, f)

	resp, out := post(t, srv, f.Sign("d1", push(f)))
	if resp.StatusCode != http.StatusAccepted || out["outcome"] != "started" || out["runId"] == "" || out["duplicate"] != false {
		t.Fatalf("start: %d %v", resp.StatusCode, out)
	}
	resp, out = post(t, srv, f.Sign("d1", push(f)))
	if resp.StatusCode != http.StatusAccepted || out["duplicate"] != true || f.Runs.Created() != 1 {
		t.Fatalf("duplicate: %d %v runs=%d", resp.StatusCode, out, f.Runs.Created())
	}

	forged := f.SignWith([]byte(strings.Repeat("x", 40)), f.Trigger.ID, "d2", f.Clock.Now().Unix(), push(f))
	resp, out = post(t, srv, forged)
	if resp.StatusCode != http.StatusUnauthorized || out["error"].(map[string]any)["code"] != "invalid_credential" {
		t.Fatalf("forged: %d %v", resp.StatusCode, out)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("no request id")
	}

	unknown := f.Sign("d3", push(f))
	unknown.TriggerID = "ght_nope"
	if resp, _ = post(t, srv, unknown); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown trigger: %d", resp.StatusCode)
	}

	p := f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitA))
	p.Repository = "repo-other"
	resp, out = post(t, srv, f.Sign("d4", gitwebhooktest.Marshal(t, p)))
	if resp.StatusCode != http.StatusUnprocessableEntity || out["error"].(map[string]any)["code"] != "foreign_repository" {
		t.Fatalf("foreign: %d %v", resp.StatusCode, out)
	}

	resp, out = post(t, srv, f.Sign("d1", gitwebhooktest.Marshal(t, f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitB)))))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflict: %d %v", resp.StatusCode, out)
	}

	resp, out = post(t, srv, f.Sign("d5", gitwebhooktest.Marshal(t, f.Payload("refs/heads/topic", gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))))
	if resp.StatusCode != http.StatusOK || out["outcome"] != "ignored" {
		t.Fatalf("ignored: %d %v", resp.StatusCode, out)
	}

	f.Git.SetError(fmt.Errorf("git host down"))
	resp, _ = post(t, srv, f.Sign("d6", push(f)))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("outage: %d", resp.StatusCode)
	}
	if f.Runs.Created() != 1 {
		t.Fatalf("runs: %d", f.Runs.Created())
	}
}

func TestHookEndpointBoundsBody(t *testing.T) {
	f := gitwebhooktest.NewFixture(t)
	srv := serve(t, f)
	big := bytes.Repeat([]byte("a"), gw.MaxBodyBytes+1)
	resp, _ := post(t, srv, f.Sign("d1", big))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if f.Store.DeliveryCount() != 0 || f.Runs.Created() != 0 {
		t.Fatal("oversized body had effects")
	}
}

// TestExampleHookScript runs the shipped post-receive example against the real
// handler, proving its signing matches the contract and that its retry reuses
// the delivery identifier.
func TestExampleHookScript(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	f := gitwebhooktest.NewFixture(t)
	// The script signs with the wall clock; align the service clock to it.
	f.Clock.Advance(time.Since(f.Clock.Now()))
	srv := serve(t, f)

	run := func(input string) string {
		cmd := exec.Command("sh", "../example/post-receive")
		cmd.Env = []string{
			"PATH=/usr/bin:/bin:/usr/local/bin",
			"KODAMA_HOOK_URL=" + srv.URL + "/api/v1/git-hooks/" + string(f.Trigger.ID),
			"KODAMA_HOOK_SECRET=" + string(f.Secret),
			"KODAMA_REPOSITORY=" + gitwebhooktest.RepositoryName,
			"KODAMA_HOOK_ATTEMPTS=1",
		}
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hook failed: %v\n%s", err, out)
		}
		return string(out)
	}

	if out := run(fmt.Sprintf("%s %s %s\n", gitwebhooktest.Zero, gitwebhooktest.CommitA, gitwebhooktest.MainRef)); out != "" {
		t.Fatalf("unexpected output %q", out)
	}
	if f.Runs.Created() != 1 || f.Runs.Requests[0].Commit != gitwebhooktest.CommitA {
		t.Fatalf("hook did not start the run: %+v", f.Runs.Requests)
	}

	// A wrong secret is refused (the script reports it and still exits 0).
	f.Secrets.Put(f.Scope, gitwebhooktest.HookSecretRef, []byte(strings.Repeat("rotated-", 8)))
	if out := run(fmt.Sprintf("%s %s %s\n", gitwebhooktest.Zero, gitwebhooktest.CommitB, gitwebhooktest.MainRef)); !strings.Contains(out, "refused (HTTP 401)") {
		t.Fatalf("expected a refusal, got %q", out)
	}
	if f.Runs.Created() != 1 {
		t.Fatal("forged script request started a run")
	}
}

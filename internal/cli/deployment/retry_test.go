package deployment_test

import (
	"bytes"
	"strings"
	"testing"
)

// Test_deployment_cli_retry: after a lost answer the caller reruns the same
// command; the CLI sends one POST per invocation with the same scope, route,
// body bytes and key, reports the stored result as a replay, never retries on
// its own, and a changed body under the key, a revoked permission or a stale
// version fail rather than repeat the effect (AC-02, AC-05, DQ-09).
func Test_deployment_cli_retry(t *testing.T) {
	f := newFactory()
	f.drop()
	c := newCLI(t, f)

	lost := c.run(createArgs("--output", "json")...)
	if lost.code != 1 || lost.out != "" {
		t.Fatalf("lost response: exit %d out %q err %q", lost.code, lost.out, lost.err)
	}
	details := decodeJSON(t, lost.err)["error"].(map[string]any)["details"].(map[string]any)
	if details["outcomeUncertain"] != true {
		t.Fatalf("an unknown outcome must be flagged: %v", details)
	}
	rec := details["recovery"].(string)
	for _, want := range []string{"identical command", "--idempotency-key", "never use a different key", "deployment list"} {
		if !strings.Contains(rec, want) {
			t.Fatalf("guidance %q lacks %q", rec, want)
		}
	}
	if n := len(c.srv.Requests()); n != 1 {
		t.Fatalf("%d requests for one invocation: a mutation is never retried automatically", n)
	}

	retry := c.run(createArgs("--output", "json")...)
	if retry.code != 0 || retry.err != "" {
		t.Fatalf("retry: exit %d err %q", retry.code, retry.err)
	}
	stored := decodeJSON(t, retry.out)
	depID := "dep_" + strings.Repeat("c", 32)
	if stored["deployment"].(map[string]any)["id"] != depID {
		t.Fatalf("stored receipt %v", stored)
	}
	// The replay is the original receipt: created:true, version 1, pending.
	if stored["created"] != true || stored["deployment"].(map[string]any)["status"] != "pending" {
		t.Fatalf("a replay must release the original receipt unchanged: %v", stored)
	}
	// The live deployment moves on; the stored receipt must not.
	f.with(func() {
		rec := f.rec(tenant+"/"+project, depID)
		rec.status, rec.reason, rec.version = "succeeded", "ready", 4
	})
	human := c.run(createArgs()...)
	if human.code != 0 || !strings.Contains(human.out, "replayed") || !strings.Contains(human.out, "kodama.optimi.com/deployment-id=") ||
		!strings.Contains(human.out, "pending (registered)") || !strings.Contains(human.out, "version: 1") {
		t.Fatalf("replay not visible or not the original: %q", human.out)
	}
	late := c.run(createArgs("--output", "json")...)
	if late.code != 0 || late.out != retry.out {
		t.Fatalf("the replay after the live record changed differs from the first replay:\n got %s\nwant %s", late.out, retry.out)
	}

	reqs := c.srv.Requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests, want one POST per invocation", len(reqs))
	}
	for i, r := range reqs {
		if r.Method != "POST" || r.Path != base+"/deployments" || r.Header.Get("Idempotency-Key") != "deploy-01" ||
			r.Header.Get("Authorization") != "Bearer tok-manager" || !bytes.Equal(r.Body, reqs[0].Body) {
			t.Fatalf("request %d: %s %s %v", i, r.Method, r.Path, r.Header)
		}
	}
	if f.mutations() != 1 {
		t.Fatalf("%d deployments recorded for four invocations", f.mutations())
	}

	// The same key with different content conflicts and tells the caller why.
	conflict := c.run(withFlag(createArgs("--output", "json"), "image-digest", digest2)...)
	if conflict.code != 4 || conflict.out != "" {
		t.Fatalf("changed body: exit %d out %q", conflict.code, conflict.out)
	}
	e := decodeJSON(t, conflict.err)["error"].(map[string]any)
	if e["code"] != "idempotency_conflict" || e["requestId"] != "req_conflict" ||
		!strings.Contains(e["details"].(map[string]any)["recovery"].(string), "reuse a key only with the identical") {
		t.Fatalf("envelope %v", e)
	}

	// A different key for the same logical action observes the original.
	again := c.run(withFlag(createArgs("--output", "json"), "idempotency-key", "deploy-02")...)
	if again.code != 0 {
		t.Fatalf("second key: exit %d %s", again.code, again.err)
	}
	obs := decodeJSON(t, again.out)
	if obs["created"] != false || obs["deployment"].(map[string]any)["id"] != depID {
		t.Fatalf("the action must own one deployment: %v", obs)
	}
	if h := c.run(withFlag(createArgs(), "idempotency-key", "deploy-03")...); !strings.Contains(h.out, "created: false") {
		t.Fatalf("human text: %q", h.out)
	}
	if f.mutations() != 1 {
		t.Fatalf("a second key created %d records", f.mutations())
	}

	// A permission revoked since the first call fails the replay.
	f.with(func() { delete(f.managers, "tok-manager") })
	revoked := c.run(createArgs("--output", "json")...)
	if revoked.code != 3 || revoked.out != "" {
		t.Fatalf("revoked: exit %d out %q", revoked.code, revoked.out)
	}
	if e := decodeJSON(t, revoked.err)["error"].(map[string]any); e["code"] != "forbidden" || e["requestId"] != "req_403" {
		t.Fatalf("envelope %v", e)
	}
}

// TestCleanupRetryNeverRepeats: a cleanup whose answer was lost is retried by
// the caller with the unchanged version. The version moved if the request was
// recorded, so the retry is refused (exit 4) and a reread shows one request;
// asking again on the new version records nothing further.
func TestCleanupRetryNeverRepeats(t *testing.T) {
	f := newFactory()
	f.drop()
	c := newCLI(t, f)

	lost := c.run("cleanup", depA, "--if-version", "3", "--output", "json")
	if lost.code != 1 || lost.out != "" {
		t.Fatalf("lost: exit %d out %q", lost.code, lost.out)
	}
	details := decodeJSON(t, lost.err)["error"].(map[string]any)["details"].(map[string]any)
	if details["outcomeUncertain"] != true || !strings.Contains(details["recovery"].(string), "deployment get") {
		t.Fatalf("details %v", details)
	}
	if strings.Contains(details["recovery"].(string), "idempotency") {
		t.Fatalf("cleanup carries no idempotency key: %v", details["recovery"])
	}
	if n := len(c.srv.Requests()); n != 1 {
		t.Fatalf("%d requests", n)
	}
	retry := c.run("cleanup", depA, "--if-version", "3", "--output", "json")
	if retry.code != 4 || retry.out != "" {
		t.Fatalf("retry on the old version: exit %d", retry.code)
	}
	reread := decodeJSON(t, c.run("get", depA, "--output", "json").out)
	if reread["cleanup"].(map[string]any)["state"] != "requested" || reread["version"].(interface{ String() string }).String() != "4" {
		t.Fatalf("reread %v", reread)
	}
	again := c.run("cleanup", depA, "--if-version", "4", "--output", "json")
	if again.code != 0 || f.mutations() != 1 {
		t.Fatalf("repeat on the new version: exit %d, %d recorded", again.code, f.mutations())
	}
	if v := decodeJSON(t, again.out)["version"].(interface{ String() string }).String(); v != "4" {
		t.Fatalf("a repeat must leave the version alone: %s", v)
	}
}

// Test_deployment_cli_unavailable_after_commit: the write commits and the
// factory then answers a standard 503 envelope (the contract allows this for an
// unknown commit outcome). The failure keeps the server's code, status, request
// ID and Retry-After, is flagged uncertain with identical-key guidance in both
// output formats, and the caller's unchanged retry replays the stored receipt.
func Test_deployment_cli_unavailable_after_commit(t *testing.T) {
	f := newFactory()
	f.unavailable()
	c := newCLI(t, f)

	r := c.run(createArgs("--output", "json")...)
	if r.code != 1 || r.out != "" {
		t.Fatalf("exit %d out %q", r.code, r.out)
	}
	e := decodeJSON(t, r.err)["error"].(map[string]any)
	d := e["details"].(map[string]any)
	if e["code"] != "unavailable" || e["requestId"] != "req_503" || e["status"].(interface{ String() string }).String() != "503" ||
		e["retryAfterSeconds"].(interface{ String() string }).String() != "7" {
		t.Fatalf("server identity lost: %v", e)
	}
	rec, _ := d["recovery"].(string)
	if d["outcomeUncertain"] != true || !strings.Contains(rec, "identical command") || !strings.Contains(rec, "never use a different key") {
		t.Fatalf("uncertainty guidance missing: %v", d)
	}
	if n := len(c.srv.Requests()); n != 1 || f.mutations() != 1 {
		t.Fatalf("%d requests, %d recorded: a 503 is never retried automatically", n, f.mutations())
	}

	// Safe recovery: the identical command replays the stored original receipt.
	retry := c.run(createArgs("--output", "json")...)
	if retry.code != 0 || retry.err != "" {
		t.Fatalf("retry: exit %d err %q", retry.code, retry.err)
	}
	if doc := decodeJSON(t, retry.out); doc["created"] != true || doc["deployment"].(map[string]any)["status"] != "pending" {
		t.Fatalf("replay %v", doc)
	}
}

func Test_deployment_cli_unavailable_human_and_cleanup(t *testing.T) {
	f := newFactory()
	f.unavailable()
	c := newCLI(t, f)
	h := c.run(createArgs()...)
	if h.code != 1 || h.out != "" {
		t.Fatalf("exit %d out %q", h.code, h.out)
	}
	for _, want := range []string{"code unavailable", "request req_503", "retry after 7s", "identical command", "never use a different key"} {
		if !strings.Contains(h.err, want) {
			t.Fatalf("human error lacks %q: %q", want, h.err)
		}
	}

	// Cleanup commits, then 503: reread shows the request recorded; the old
	// version is refused, so the retry cannot repeat the request.
	f.unavailable()
	r := c.run("cleanup", depA, "--if-version", "3", "--output", "json")
	if r.code != 1 || r.out != "" {
		t.Fatalf("cleanup exit %d out %q", r.code, r.out)
	}
	e := decodeJSON(t, r.err)["error"].(map[string]any)
	d := e["details"].(map[string]any)
	if e["requestId"] != "req_503" || d["outcomeUncertain"] != true || !strings.Contains(d["recovery"].(string), "deployment get") ||
		strings.Contains(d["recovery"].(string), "idempotency") {
		t.Fatalf("envelope %v", e)
	}
	reread := decodeJSON(t, c.run("get", depA, "--output", "json").out)
	if reread["cleanup"].(map[string]any)["state"] != "requested" {
		t.Fatalf("reread %v", reread)
	}
	if again := c.run("cleanup", depA, "--if-version", "3"); again.code != 4 || f.mutations() != 2 {
		t.Fatalf("retry exit %d, %d recorded", again.code, f.mutations())
	}
}

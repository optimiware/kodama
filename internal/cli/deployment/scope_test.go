package deployment_test

import (
	"strings"
	"testing"
)

// allGET reports whether every recorded request was a read.
func allGET(f *cli) bool {
	for _, r := range f.srv.Requests() {
		if r.Method != "GET" {
			return false
		}
	}
	return true
}

// Test_deployment_scope: every command addresses exactly the selected
// tenant/project through the public routes, sends only the contract's members
// (never an environment, namespace, configuration, tenant or project), shows a
// foreign or hidden deployment as the same not-found, refuses a caller without
// deployment:manage, and requests cleanup only for one recorded deployment by
// ID and observed version. Reading never changes or approves anything
// (AC-01, AC-02, AC-05, DQ-09).
func Test_deployment_scope(t *testing.T) {
	f := newFactory()
	c := newCLI(t, f)

	// Reads: scoped routes only; a read is never a mutation.
	got := c.run("get", depA, "--output", "json")
	if got.code != 0 || got.err != "" {
		t.Fatalf("get: exit %d err %q", got.code, got.err)
	}
	d := decodeJSON(t, got.out)
	if d["id"] != depA || d["status"] != "succeeded" {
		t.Fatalf("deployment %v", d)
	}
	list := c.run("list", "--environment-id", "env_preview", "--status", "succeeded", "--limit", "20", "--output", "json")
	if list.code != 0 {
		t.Fatalf("list: exit %d %s", list.code, list.err)
	}
	if items := decodeJSON(t, list.out)["items"].([]any); len(items) != 1 {
		t.Fatalf("the list must hold only this project's matching deployment: %v", items)
	}
	reqs := c.srv.Requests()
	if reqs[0].Path != base+"/deployments/"+depA || reqs[0].Header.Get("Authorization") != "Bearer tok-manager" {
		t.Fatalf("get request %s %v", reqs[0].Path, reqs[0].Header)
	}
	if reqs[1].Path != base+"/deployments" || reqs[1].RawQuery != "environmentId=env_preview&limit=20&status=succeeded" {
		t.Fatalf("list request %s?%s", reqs[1].Path, reqs[1].RawQuery)
	}
	if human := c.run("get", depA); human.code != 0 || !strings.Contains(human.out, "version: 3 (use with --if-version)") {
		t.Fatalf("human output %q", human.out)
	}
	if !allGET(c) || f.mutations() != 0 {
		t.Fatal("viewing deployments must never mutate or approve")
	}

	// A foreign deployment is indistinguishable from an absent one.
	foreign := c.run("get", depC, "--output", "json")
	absent := c.run("get", "dep_00000000000000000000000000000000", "--output", "json")
	if foreign.code != 1 || foreign.out != "" || foreign.err != absent.err {
		t.Fatalf("foreign %d %q vs absent %q", foreign.code, foreign.err, absent.err)
	}
	if e := decodeJSON(t, foreign.err)["error"].(map[string]any); e["code"] != "not_found" || e["requestId"] != "req_404" {
		t.Fatalf("envelope %v", e)
	}
	// Selecting another tenant with this principal discloses nothing either.
	other := c.runScoped([]string{"--tenant", "t_other", "--project", project}, "get", depC, "--output", "json")
	if other.code != 1 || other.out != "" {
		t.Fatalf("a non-member must not read a foreign tenant: exit %d out %q", other.code, other.out)
	}
	// ...and the owning tenant's principal reads its own record.
	c.token = "tok-other"
	own := c.runScoped([]string{"--tenant", "t_other", "--project", project}, "get", depC, "--output", "json")
	if own.code != 0 || decodeJSON(t, own.out)["id"] != depC {
		t.Fatalf("own tenant read: %d %q", own.code, own.err)
	}
	c.token = "tok-manager"

	// A missing project is a usage failure before any request.
	n := len(c.srv.Requests())
	noProject := c.runScoped([]string{"--tenant", tenant}, "list")
	if noProject.code != 2 || len(c.srv.Requests()) != n {
		t.Fatalf("no project: exit %d, %d new requests", noProject.code, len(c.srv.Requests())-n)
	}

	// Cleanup names a recorded deployment and an observed version, nothing else.
	for _, bad := range [][]string{
		{"cleanup", "env_preview", "--if-version", "3"},
		{"cleanup", "dep_x", "--if-version", "3"},
		{"cleanup", "../" + depA, "--if-version", "3"},
		{"cleanup", depA},
		{"cleanup", depA, "--if-version", "0"},
		{"cleanup", depA, "extra", "--if-version", "3"},
	} {
		n = len(c.srv.Requests())
		if r := c.run(bad...); r.code != 2 || r.out != "" || len(c.srv.Requests()) != n {
			t.Fatalf("%v: exit %d, requests %d", bad, r.code, len(c.srv.Requests())-n)
		}
	}
	done := c.run("cleanup", depA, "--if-version", "3", "--output", "json")
	if done.code != 0 || done.err != "" {
		t.Fatalf("cleanup: exit %d %s", done.code, done.err)
	}
	after := decodeJSON(t, done.out)
	if after["id"] != depA || after["cleanup"].(map[string]any)["state"] != "requested" || after["version"].(interface{ String() string }).String() != "4" {
		t.Fatalf("cleanup result %v", after)
	}
	last := c.srv.Requests()[len(c.srv.Requests())-1]
	if last.Method != "POST" || last.Path != base+"/deployments/"+depA+"/cleanup" || len(last.Body) != 0 ||
		last.Header.Get("If-Match") != `"3"` || last.Header.Get("Idempotency-Key") != "" {
		t.Fatalf("cleanup request %s %s body %q headers %v", last.Method, last.Path, last.Body, last.Header)
	}
	if b := f.snapshot(tenant+"/"+project, depB); b.cleanup != "none" || b.version != 2 {
		t.Fatalf("another deployment was touched: %+v", b)
	}
	if c3 := f.snapshot("t_other/"+project, depC); c3.cleanup != "none" || c3.version != 5 {
		t.Fatalf("a foreign deployment was touched: %+v", c3)
	}
	human := c.run("get", depA)
	if !strings.Contains(human.out, "cleanup: requested") {
		t.Fatalf("human cleanup state: %q", human.out)
	}

	// Cleanup outside the scope or of an unsettled deployment records nothing.
	mutated := f.mutations()
	if r := c.run("cleanup", depC, "--if-version", "5"); r.code != 1 || f.mutations() != mutated {
		t.Fatalf("foreign cleanup: exit %d", r.code)
	}
	unsettled := c.run("cleanup", depB, "--if-version", "2", "--output", "json")
	if unsettled.code != 1 || unsettled.out != "" {
		t.Fatalf("unsettled cleanup: exit %d out %q", unsettled.code, unsettled.out)
	}
	if e := decodeJSON(t, unsettled.err)["error"].(map[string]any); e["code"] != "invalid_transition" || e["requestId"] != "req_422" {
		t.Fatalf("envelope %v", e)
	}
	if f.mutations() != mutated {
		t.Fatal("a refused cleanup recorded something")
	}

	// A stale version fails, advises a reread and is never retried.
	n = len(c.srv.Requests())
	stale := c.run("cleanup", depA, "--if-version", "3", "--output", "json")
	if stale.code != 4 || stale.out != "" || len(c.srv.Requests()) != n+1 {
		t.Fatalf("stale: exit %d, %d requests", stale.code, len(c.srv.Requests())-n)
	}
	e := decodeJSON(t, stale.err)["error"].(map[string]any)
	if e["code"] != "precondition_failed" || !strings.Contains(e["details"].(map[string]any)["recovery"].(string), "deployment get") {
		t.Fatalf("stale envelope %v", e)
	}

	// Without deployment:manage neither mutation is recorded.
	c.token = "tok-viewer"
	mutated = f.mutations()
	denied := c.run("cleanup", depB, "--if-version", "2", "--output", "json")
	if denied.code != 3 || denied.out != "" || f.mutations() != mutated {
		t.Fatalf("viewer cleanup: exit %d", denied.code)
	}
	deniedCreate := c.run(createArgs()...)
	if deniedCreate.code != 3 || deniedCreate.out != "" || f.mutations() != mutated {
		t.Fatalf("viewer create: exit %d", deniedCreate.code)
	}
	if v := c.run("get", depA); v.code != 0 {
		t.Fatalf("a viewer may still read: exit %d %s", v.code, v.err)
	}
}

// TestCreateSendsOnlyTheContract: the registration body names the attempt,
// source and image; the environment, configuration pin, run, step, action key,
// tenant and project are server-derived and never sent.
func TestCreateSendsOnlyTheContract(t *testing.T) {
	f := newFactory()
	c := newCLI(t, f)
	r := c.run(createArgs("--output", "json")...)
	if r.code != 0 {
		t.Fatalf("exit %d %s", r.code, r.err)
	}
	req := c.srv.Requests()[0]
	want := `{"attemptId":"att_deploy","source":{"repositoryUrl":"` + repo + `","commit":"` + commit + `"},"image":{"artifactId":"art_image","digest":"` + digest + `"}}`
	if req.Path != base+"/deployments" || string(req.Body) != want || req.Header.Get("Idempotency-Key") != "deploy-01" || req.Header.Get("If-Match") != "" {
		t.Fatalf("request %s %s %v", req.Path, req.Body, req.Header)
	}
	doc := decodeJSON(t, r.out)
	if doc["created"] != true || doc["annotations"].(map[string]any)["kodama.optimi.com/deployment-id"] == nil {
		t.Fatalf("receipt %v", doc)
	}
}

func TestCreateRejectsIncompleteInputBeforeAnyRequest(t *testing.T) {
	f := newFactory()
	c := newCLI(t, f)
	cases := map[string][]string{
		"no attempt":     without(createArgs(), "attempt-id"),
		"no repo":        without(createArgs(), "repository-url"),
		"no commit":      without(createArgs(), "commit"),
		"no artifact":    without(createArgs(), "image-artifact-id"),
		"no digest":      without(createArgs(), "image-digest"),
		"no key":         without(createArgs(), "idempotency-key"),
		"empty key":      withFlag(createArgs(), "idempotency-key", ""),
		"bad key":        withFlag(createArgs(), "idempotency-key", "has space"),
		"credential url": withFlag(createArgs(), "repository-url", "https://user:pw@git.t037.invalid/x.git"),
		"http url":       withFlag(createArgs(), "repository-url", "http://git.t037.invalid/x.git"),
		"short commit":   withFlag(createArgs(), "commit", "abc123"),
		"upper commit":   withFlag(createArgs(), "commit", strings.Repeat("A", 40)),
		"bad digest":     withFlag(createArgs(), "image-digest", "latest"),
		"positional":     append(createArgs(), "extra"),
		"blank attempt":  withFlag(createArgs(), "attempt-id", " "),
	}
	for name, args := range cases {
		if r := c.run(args...); r.code != 2 || r.out != "" {
			t.Errorf("%s: exit %d out %q", name, r.code, r.out)
		}
	}
	if n := len(c.srv.Requests()); n != 0 {
		t.Fatalf("%d requests for invalid input", n)
	}
}

func TestListFilterValidation(t *testing.T) {
	c := newCLI(t, newFactory())
	for _, args := range [][]string{
		{"list", "--status", "done"},
		{"list", "--environment-id", ""},
		{"list", "--limit", "0"},
		{"list", "--cursor", ""},
		{"list", "extra"},
	} {
		if r := c.run(args...); r.code != 2 {
			t.Errorf("%v: exit %d", args, r.code)
		}
	}
	if n := len(c.srv.Requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}

func TestOutcomeUnknownIsVisible(t *testing.T) {
	f := newFactory()
	f.with(func() {
		f.scopes[tenant+"/"+project][depB].unknown = true
		f.scopes[tenant+"/"+project][depB].reason = "outcome_unknown"
	})
	c := newCLI(t, f)
	r := c.run("get", depB)
	if r.code != 0 || !strings.Contains(r.out, "OUTCOME UNKNOWN") || !strings.Contains(r.out, "do not apply again") {
		t.Fatalf("%q", r.out)
	}
}

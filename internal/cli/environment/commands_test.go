package environment_test

import (
	"strings"
	"testing"
)

func TestCreateSendsExactlyTheContract(t *testing.T) {
	f := newFactory()
	c := newCLI(t, f)
	r := c.run(createArgs("--output", "json")...)
	if r.code != 0 || r.err != "" {
		t.Fatalf("exit %d err %q", r.code, r.err)
	}
	req := c.srv.Requests()[0]
	want := `{"config":{"REPLICAS":"1"},"kind":"preview","kubeconfig":{"delivery":"file","name":"KUBECONFIG","ref":"secret://project/kubeconfig"},` +
		`"name":"preview","namespace":"t037-preview","previewTtlSeconds":86400,` +
		`"secrets":[{"delivery":"env","name":"REGISTRY_TOKEN","ref":"secret://project/registry-token"}],"target":"kind"}`
	// Key order is not significant on the wire; compare as documents.
	if req.Method != "POST" || req.Path != base+"/environments" || canon(t, req.Body) != canon(t, []byte(want)) {
		t.Fatalf("request %s %s %s", req.Method, req.Path, req.Body)
	}
	if req.Header.Get("If-Match") != "" || req.Header.Get("Idempotency-Key") != "" {
		t.Fatalf("headers %v", req.Header)
	}
	if doc := decodeJSON(t, r.out); doc["name"] != "preview" || doc["id"] != "env_new" {
		t.Fatalf("doc %v", doc)
	}
	human := c.run("get", "env_new")
	if human.code != 0 || !strings.Contains(human.out, "kubeconfig: KUBECONFIG <- secret://project/kubeconfig (file)") ||
		!strings.Contains(human.out, "version: 1 (use with --if-version)") {
		t.Fatalf("human %q", human.out)
	}
}

func TestStageEnvironmentHasNoTTL(t *testing.T) {
	c := newCLI(t, newFactory())
	if r := c.run("create", "stage", "--kind", "stage", "--target", "kind", "--namespace", "ns", "--kubeconfig", "K=secret://project/k@file", "--preview-ttl-seconds", "60"); r.code != 2 {
		t.Fatalf("exit %d", r.code)
	}
	if r := c.run("create", "stage", "--kind", "stage", "--target", "kind", "--namespace", "ns", "--kubeconfig", "K=secret://project/k@file"); r.code != 0 {
		t.Fatalf("exit %d %s", r.code, r.err)
	}
	body := string(c.srv.Requests()[0].Body)
	if strings.Contains(body, "previewTtlSeconds") || strings.Contains(body, `"secrets"`) || strings.Contains(body, `"config"`) {
		t.Fatalf("unnamed members must be omitted: %s", body)
	}
}

func TestCreateRejectsBadInputBeforeAnyRequest(t *testing.T) {
	c := newCLI(t, newFactory())
	ok := createArgs()
	cases := map[string][]string{
		"no kind":          append([]string{"create", "x"}, ok[3:]...),
		"bad kind":         append(append([]string{}, ok...), "--kind", "prod"),
		"no target":        {"create", "x", "--kind", "stage", "--namespace", "n", "--kubeconfig", "K=secret://project/k@file"},
		"no namespace":     {"create", "x", "--kind", "stage", "--target", "t", "--kubeconfig", "K=secret://project/k@file"},
		"no kubeconfig":    {"create", "x", "--kind", "stage", "--target", "t", "--namespace", "n"},
		"binding no @":     {"create", "x", "--kind", "stage", "--target", "t", "--namespace", "n", "--kubeconfig", "K=secret://project/k"},
		"binding no name":  {"create", "x", "--kind", "stage", "--target", "t", "--namespace", "n", "--kubeconfig", "=secret://project/k@file"},
		"binding delivery": {"create", "x", "--kind", "stage", "--target", "t", "--namespace", "n", "--kubeconfig", "K=secret://project/k@inline"},
		"bad secret":       append(append([]string{}, ok...), "--secret", "T=value-not-reference"),
		"config no equals": append(append([]string{}, ok...), "--config", "NOVALUE"),
		"config dup":       append(append([]string{}, ok...), "--config", "REPLICAS=2"),
		"bad ttl":          append(append([]string{}, ok...), "--preview-ttl-seconds", "-5"),
		"no name":          {"create", "--kind", "stage"},
		"two names":        append([]string{"create", "a", "b"}, ok[3:]...),
	}
	for name, args := range cases {
		if r := c.run(args...); r.code != 2 || r.out != "" {
			t.Errorf("%s: exit %d", name, r.code)
		}
	}
	if n := len(c.srv.Requests()); n != 0 {
		t.Fatalf("%d requests for invalid input", n)
	}
}

func TestNameTakenAdvisesAndIsNotRetried(t *testing.T) {
	c := newCLI(t, newFactory())
	r := c.run(createArgs("--output", "json")...)
	if r.code != 0 {
		t.Fatalf("first: %d", r.code)
	}
	again := c.run(createArgs("--output", "json")...)
	if again.code != 4 || again.out != "" {
		t.Fatalf("exit %d", again.code)
	}
	e := decodeJSON(t, again.err)["error"].(map[string]any)
	if e["code"] != "name_taken" || e["requestId"] != "req_409" || !strings.Contains(e["details"].(map[string]any)["recovery"].(string), "environment list") {
		t.Fatalf("envelope %v", e)
	}
	if n := len(c.srv.Requests()); n != 2 {
		t.Fatalf("%d requests", n)
	}
}

func TestUpdateSendsOnlyNamedMembersUnderTheObservedVersion(t *testing.T) {
	f := newFactory()
	c := newCLI(t, f)
	r := c.run("update", envID, "--if-version", "1", "--namespace", "t037-next", "--config", "REPLICAS=3", "--output", "json")
	if r.code != 0 {
		t.Fatalf("exit %d %s", r.code, r.err)
	}
	req := c.srv.Requests()[0]
	if req.Method != "PATCH" || req.Path != base+"/environments/"+envID || req.Header.Get("If-Match") != `"1"` ||
		canon(t, req.Body) != canon(t, []byte(`{"namespace":"t037-next","config":{"REPLICAS":"3"}}`)) {
		t.Fatalf("request %s %s %s %v", req.Method, req.Path, req.Body, req.Header)
	}
	if v := decodeJSON(t, r.out)["version"].(interface{ String() string }).String(); v != "2" {
		t.Fatalf("version %s", v)
	}

	clear := c.run("update", envID, "--if-version", "2", "--clear-secrets", "--clear-config")
	if clear.code != 0 {
		t.Fatalf("exit %d %s", clear.code, clear.err)
	}
	if got := canon(t, c.srv.Requests()[1].Body); got != canon(t, []byte(`{"secrets":[],"config":{}}`)) {
		t.Fatalf("clear body %s", got)
	}
}

func TestUpdateRequiresExplicitVersionAndChange(t *testing.T) {
	c := newCLI(t, newFactory())
	for name, args := range map[string][]string{
		"no version":        {"update", envID, "--namespace", "n"},
		"zero version":      {"update", envID, "--if-version", "0", "--namespace", "n"},
		"nothing to change": {"update", envID, "--if-version", "1"},
		"clear and set":     {"update", envID, "--if-version", "1", "--clear-secrets", "--secret", "T=secret://project/t@env"},
		"blank namespace":   {"update", envID, "--if-version", "1", "--namespace", " "},
		"bad kubeconfig":    {"update", envID, "--if-version", "1", "--kubeconfig", "oops"},
		"no id":             {"update", "--if-version", "1", "--namespace", "n"},
		"cannot rename":     {"update", envID, "--if-version", "1", "--name", "x"},
	} {
		if r := c.run(args...); r.code != 2 || r.out != "" {
			t.Errorf("%s: exit %d", name, r.code)
		}
	}
	if n := len(c.srv.Requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}

func TestStaleUpdateAdvisesRereadAndIsNotRetried(t *testing.T) {
	f := newFactory()
	c := newCLI(t, f)
	r := c.run("update", envID, "--if-version", "9", "--namespace", "n", "--output", "json")
	if r.code != 4 || r.out != "" {
		t.Fatalf("exit %d", r.code)
	}
	e := decodeJSON(t, r.err)["error"].(map[string]any)
	if e["code"] != "precondition_failed" || !strings.Contains(e["details"].(map[string]any)["recovery"].(string), "runs already admitted keep") {
		t.Fatalf("envelope %v", e)
	}
	if n := len(c.srv.Requests()); n != 1 || f.mutations() != 0 {
		t.Fatalf("%d requests, %d mutations", n, f.mutations())
	}
}

func TestLostCreateAndUpdateAnswersAreUncertainAndNeverRetried(t *testing.T) {
	f := newFactory()
	f.drop()
	c := newCLI(t, f)
	lost := c.run(createArgs("--output", "json")...)
	d := decodeJSON(t, lost.err)["error"].(map[string]any)["details"].(map[string]any)
	if lost.code != 1 || lost.out != "" || d["outcomeUncertain"] != true || !strings.Contains(d["recovery"].(string), "environment list") {
		t.Fatalf("create: exit %d %v", lost.code, d)
	}
	f.drop()
	lostEdit := c.run("update", envID, "--if-version", "1", "--target", "other", "--output", "json")
	d = decodeJSON(t, lostEdit.err)["error"].(map[string]any)["details"].(map[string]any)
	if lostEdit.code != 1 || d["outcomeUncertain"] != true || !strings.Contains(d["recovery"].(string), "environment get") {
		t.Fatalf("update: exit %d %v", lostEdit.code, d)
	}
	if n := len(c.srv.Requests()); n != 2 {
		t.Fatalf("%d requests: a mutation is never retried automatically", n)
	}
	// The retry of an applied edit under its old version is refused, not repeated.
	again := c.run("update", envID, "--if-version", "1", "--target", "other")
	if again.code != 4 || f.mutations() != 2 {
		t.Fatalf("retry exit %d, %d mutations", again.code, f.mutations())
	}
}

// TestEnvironmentScope: routes carry the selected tenant and project, a
// foreign environment is the same not-found, and a caller without
// deployment:manage changes nothing.
func TestEnvironmentScope(t *testing.T) {
	f := newFactory()
	c := newCLI(t, f)
	if r := c.run("get", envID, "--output", "json"); r.code != 0 || decodeJSON(t, r.out)["id"] != envID {
		t.Fatalf("get: %d %s", r.code, r.err)
	}
	if r := c.run("get", "env_foreign", "--output", "json"); r.code != 1 || r.out != "" ||
		decodeJSON(t, r.err)["error"].(map[string]any)["code"] != "not_found" {
		t.Fatalf("foreign: %d %q", r.code, r.err)
	}
	if r := c.runScoped([]string{"--tenant", "t_other", "--project", project}, "list"); r.code != 1 || r.out != "" {
		t.Fatalf("non-member: %d %q", r.code, r.out)
	}
	if r := c.runScoped([]string{"--tenant", tenant}, "list"); r.code != 2 {
		t.Fatalf("no project: %d", r.code)
	}
	c.token = "tok-viewer"
	for _, args := range [][]string{createArgs(), {"update", envID, "--if-version", "1", "--namespace", "n"}} {
		if r := c.run(args...); r.code != 3 || r.out != "" {
			t.Fatalf("%v: exit %d", args, r.code)
		}
	}
	if f.mutations() != 0 {
		t.Fatal("a caller without deployment:manage changed an environment")
	}
	list := c.run("list", "--limit", "5", "--cursor", "abc.def")
	if list.code != 0 || !strings.Contains(list.out, "next page: --cursor abc.def") {
		t.Fatalf("list: %d %q", list.code, list.out)
	}
	last := c.srv.Requests()[len(c.srv.Requests())-1]
	if last.Path != base+"/environments" || last.RawQuery != "cursor=abc.def&limit=5" {
		t.Fatalf("list request %s?%s", last.Path, last.RawQuery)
	}
}

// TestUnavailableAfterCommitIsUncertain: the write commits and the factory
// answers a standard 503. The server's code, status, request ID and
// Retry-After survive, both output formats carry the recovery guidance, the
// mutation is not retried, and rereading shows what was recorded.
func TestUnavailableAfterCommitIsUncertain(t *testing.T) {
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
	if d["outcomeUncertain"] != true || !strings.Contains(d["recovery"].(string), "environment list") {
		t.Fatalf("details %v", d)
	}

	f.unavailable()
	h := c.run("update", "env_new", "--if-version", "1", "--target", "other")
	for _, want := range []string{"code unavailable", "request req_503", "retry after 7s", "environment get"} {
		if h.code != 1 || !strings.Contains(h.err, want) {
			t.Fatalf("human error lacks %q: exit %d %q", want, h.code, h.err)
		}
	}
	if n := len(c.srv.Requests()); n != 2 {
		t.Fatalf("%d requests: a 503 is never retried automatically", n)
	}

	// Recovery by rereading: both writes were recorded; repeating the edit under
	// its old version is refused, and the create is refused as a taken name.
	if g := c.run("get", "env_new", "--output", "json"); g.code != 0 || decodeJSON(t, g.out)["target"] != "other" {
		t.Fatalf("reread: %d %s", g.code, g.out)
	}
	if again := c.run("update", "env_new", "--if-version", "1", "--target", "other"); again.code != 4 || f.mutations() != 2 {
		t.Fatalf("retry exit %d, %d recorded", again.code, f.mutations())
	}
	if again := c.run(createArgs()...); again.code != 4 || f.mutations() != 2 {
		t.Fatalf("create retry exit %d, %d recorded", again.code, f.mutations())
	}
}

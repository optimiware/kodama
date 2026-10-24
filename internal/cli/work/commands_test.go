package work_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

func Test_work_create_sends_files_verbatim(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 201, itemJSON("wi_1042", "backlog", 1, 1), map[string]string{"ETag": `"1"`, "X-Request-ID": "req_1"})
	}))
	spec := writeFile(t, "spec.md", "# Plan\nAdd a review flow.\n")
	acc := writeFile(t, "acceptance.json", `[{"id":"AC-1","text":"A reviewer resolves an exception."}]`)
	r := c.run("work", "create", "--kind", "story", "--title", "Resolve inventory exceptions",
		"--spec-file", spec, "--acceptance-file", acc, "--output", "json")
	if r.code != 0 || r.err != "" {
		t.Fatalf("exit %d, stderr %q", r.code, r.err)
	}
	reqs := c.srv.Requests()
	if len(reqs) != 1 || reqs[0].Method != "POST" || reqs[0].Path != workPath {
		t.Fatalf("requests: %+v", reqs)
	}
	if got := reqs[0].Header.Get("Authorization"); got != "Bearer tok-1" {
		t.Fatalf("authorization %q", got)
	}
	b := body(t, reqs[0])
	if b["spec"] != "# Plan\nAdd a review flow.\n" || b["kind"] != "story" || b["title"] != "Resolve inventory exceptions" {
		t.Fatalf("body %v", b)
	}
	crit := b["acceptanceCriteria"].([]any)[0].(map[string]any)
	if crit["id"] != "AC-1" {
		t.Fatalf("criteria %v", crit)
	}
	if _, ok := b["assigneeId"]; ok {
		t.Fatalf("an unset assignee must be omitted: %v", b)
	}
	out := decodeJSON(t, r.out)
	if out["id"] != "wi_1042" || out["version"].(interface{ String() string }).String() != "1" {
		t.Fatalf("output %v", out)
	}
}

func Test_work_human_output_separates_version_and_revision(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, itemJSON("wi_1042", "ready", 7, 3), nil)
	}))
	r := c.run("work", "get", "wi_1042")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	if !strings.Contains(r.out, "resource version: 7") || !strings.Contains(r.out, "content revision: 3") {
		t.Fatalf("output %q", r.out)
	}
}

func Test_work_update_requires_explicit_version_before_any_request(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	for _, args := range [][]string{
		{"work", "update", "wi_1", "--title", "x"},
		{"work", "update", "wi_1", "--title", "x", "--if-version", "0"},
		{"work", "update", "wi_1", "--title", "x", "--if-version", "1.5"},
		{"work", "update", "wi_1", "--title", "x", "--if-version", "-2"},
		{"work", "update", "wi_1", "--if-version", "2"}, // nothing to change
		{"work", "update", "wi_1", "--if-version", "2", "--assignee", "u", "--clear-assignee"},
		{"work", "update", "wi_1", "--if-version", "2", "--assignee", ""},
		{"work", "transition", "wi_1", "--to", "ready"},
		{"work", "transition", "wi_1", "--to", "bogus", "--if-version", "1"},
	} {
		if r := c.run(args...); r.code != 2 || r.out != "" {
			t.Errorf("%v: exit %d out %q err %q", args, r.code, r.out, r.err)
		}
	}
	if n := len(c.srv.Requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}

func Test_work_update_distinguishes_omitted_from_cleared(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, itemJSON("wi_1", "backlog", 4, 1), nil)
	}))
	r := c.run("work", "update", "wi_1", "--if-version", "3", "--clear-assignee", "--clear-parent",
		"--blocked-reason", "Waiting for ERP", "--output", "json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	req := c.srv.Requests()[0]
	if req.Method != "PATCH" || req.Path != workPath+"/wi_1" || req.Header.Get("If-Match") != `"3"` {
		t.Fatalf("request %+v", req)
	}
	if got := strings.TrimSpace(string(req.Body)); got != `{"assigneeId":null,"blockedReason":"Waiting for ERP","parentId":null}` {
		t.Fatalf("body %s", got)
	}
}

func Test_work_update_clear_blocked_reason(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, itemJSON("wi_1", "backlog", 4, 1), nil)
	}))
	r := c.run("work", "update", "wi_1", "--if-version", "3", "--clear-blocked-reason")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	if got := strings.TrimSpace(string(c.srv.Requests()[0].Body)); got != `{"blockedReason":null}` {
		t.Fatalf("body %s", got)
	}
}

func Test_work_stale_update_is_one_request_exit_4_with_request_id(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteError(w, 412, "version_conflict", "the work item changed", "req_stale_1", nil)
	}))
	r := c.run("work", "update", "wi_1", "--title", "New", "--if-version", "1", "--output", "json")
	if r.code != 4 || r.out != "" {
		t.Fatalf("exit %d out %q", r.code, r.out)
	}
	env := decodeJSON(t, r.err)["error"].(map[string]any)
	if env["code"] != "version_conflict" || env["requestId"] != "req_stale_1" {
		t.Fatalf("envelope %v", env)
	}
	if !strings.Contains(env["details"].(map[string]any)["recovery"].(string), "work history") {
		t.Fatalf("no reread advice: %v", env)
	}
	if n := len(c.srv.Requests()); n != 1 {
		t.Fatalf("the CLI sent %d requests: stale edits are never retried or refreshed", n)
	}
	h := c.run("work", "update", "wi_1", "--title", "New", "--if-version", "1")
	if h.code != 4 || !strings.Contains(h.err, "req_stale_1") || !strings.Contains(h.err, "reread") {
		t.Fatalf("human failure %q", h.err)
	}
}

func Test_work_transition_claims_and_exact_integers(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, itemJSON("wi_1", "done", 9007199254740991, 9007199254740990), nil)
	}))
	claim := writeFile(t, "completion.json", `{"contentRevision":9007199254740991,"candidateCommit":"`+strings.Repeat("1", 40)+`","acceptedCriteria":["AC-1"],"acknowledgedFindings":[{"id":"F-7","summary":"Export is slow"}]}`)
	r := c.run("work", "transition", "wi_1", "--to", "done", "--if-version", "9007199254740990",
		"--reason", "Accepted", "--completion-file", claim, "--output", "json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	req := c.srv.Requests()[0]
	if req.Path != workPath+"/wi_1/transitions" || req.Header.Get("If-Match") != `"9007199254740990"` {
		t.Fatalf("request %+v", req)
	}
	if !strings.Contains(string(req.Body), `"contentRevision":9007199254740991`) {
		t.Fatalf("content revision not preserved: %s", req.Body)
	}
	if !strings.Contains(r.out, `"version":9007199254740991`) || !strings.Contains(r.out, `"contentRevision":9007199254740990`) {
		t.Fatalf("large integers not preserved: %s", r.out)
	}
	for _, bad := range []string{`{"contentRevision":1,"acceptedCriteria":[],"extra":true}`, `{"contentRevision":1.5,"acceptedCriteria":[]}`, `[]`, `{} {}`} {
		f := writeFile(t, "bad.json", bad)
		if r := c.run("work", "transition", "wi_1", "--to", "done", "--if-version", "2", "--completion-file", f); r.code != 2 {
			t.Errorf("%s: exit %d", bad, r.code)
		}
	}
	if n := len(c.srv.Requests()); n != 1 {
		t.Fatalf("malformed claim files reached the server: %d requests", n)
	}
}

func Test_work_review_claim_file(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, itemJSON("wi_1", "review", 6, 2), nil)
	}))
	claim := writeFile(t, "review.json", `{"candidateCommit":"`+strings.Repeat("a", 40)+`","evidence":[{"type":"artifact","id":"art_report_1"}]}`)
	r := c.run("work", "transition", "wi_1", "--to", "review", "--if-version", "5", "--review-file", claim)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	b := body(t, c.srv.Requests()[0])
	rv := b["review"].(map[string]any)
	if b["to"] != "review" || rv["candidateCommit"] != strings.Repeat("a", 40) {
		t.Fatalf("body %v", b)
	}
}

func Test_work_list_filters_and_cursors_pass_through(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, map[string]any{
			"items": []any{itemJSON("wi_1", "active", 2, 1)}, "nextCursor": "abc_-1.def_-2", "futureField": map[string]any{"n": 1},
		}, nil)
	}))
	r := c.run("work", "list", "--state", "ready", "--state", "active", "--kind", "story", "--assignee", "usr_1",
		"--blocked=false", "--limit", "25", "--cursor", "opaque.cursor-1", "--output", "json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	req := c.srv.Requests()[0]
	if req.Method != "GET" || req.Path != workPath {
		t.Fatalf("request %+v", req)
	}
	q := req.RawQuery
	for _, want := range []string{"state=ready", "state=active", "kind=story", "assigneeId=usr_1", "blocked=false", "limit=25", "cursor=opaque.cursor-1"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %q", q, want)
		}
	}
	out := decodeJSON(t, r.out)
	if out["nextCursor"] != "abc_-1.def_-2" || out["futureField"] == nil {
		t.Fatalf("output %v", out)
	}
	if h := c.run("work", "list"); !strings.Contains(h.out, "wi_1") || !strings.Contains(h.out, "next page: --cursor abc_-1.def_-2") {
		t.Fatalf("human list %q", h.out)
	}
}

func Test_work_history_reads_one_page(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, map[string]any{"items": []any{map[string]any{
			"id": "h_1", "version": 2, "contentRevision": 2, "kind": "updated", "actor": map[string]any{"type": "user", "id": "usr_a"},
			"occurredAt": "2026-10-19T10:06:00Z", "changedFields": []any{"spec"}, "before": map[string]any{}, "after": map[string]any{},
		}}, "nextCursor": nil}, nil)
	}))
	r := c.run("work", "history", "wi_1", "--limit", "10")
	if r.code != 0 || !strings.Contains(r.out, "v2 r2") || !strings.Contains(r.out, "changed: spec") {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	if req := c.srv.Requests()[0]; req.Path != workPath+"/wi_1/history" || req.RawQuery != "limit=10" {
		t.Fatalf("request %+v", req)
	}
}

func Test_scope_cursor_output_contract(t *testing.T) {
	for _, tc := range []struct {
		status   int
		code     string
		exit     int
		retry    string
		wantSecs string
	}{
		{401, "unauthenticated", 3, "", ""},
		{403, "forbidden", 3, "", ""},
		{404, "not_found", 1, "", ""},
		{409, "workspace_busy", 4, "", ""},
		{412, "version_conflict", 4, "", ""},
		{428, "version_required", 2, "", ""},
		{429, "rate_limited", 1, "7", `"retryAfterSeconds":7`},
		{503, "unavailable", 1, "3", `"retryAfterSeconds":3`},
	} {
		hdr := map[string]string{}
		if tc.retry != "" {
			hdr["Retry-After"] = tc.retry
		}
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, tc.status, tc.code, "message", "req_x", hdr)
		}))
		r := c.run("work", "transition", "wi_1", "--to", "active", "--if-version", "2", "--output", "json")
		if int(r.code) != tc.exit || r.out != "" {
			t.Errorf("%d: exit %d out %q", tc.status, r.code, r.out)
		}
		if !strings.Contains(r.err, `"code":"`+tc.code+`"`) || !strings.Contains(r.err, `"requestId":"req_x"`) || !strings.Contains(r.err, tc.wantSecs) {
			t.Errorf("%d: stderr %s", tc.status, r.err)
		}
	}
}

func Test_scope_explicit_override(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteError(w, 404, "not_found", "no such work item", "req_foreign", nil)
	}))
	r := c.run("work", "get", "wi_9", "--project", "p_other", "--tenant", "t_other")
	if r.code != 1 {
		t.Fatalf("exit %d", r.code)
	}
	if got := c.srv.Requests()[0].Path; got != "/api/v1/tenants/t_other/projects/p_other/work-items/wi_9" {
		t.Fatalf("path %s", got)
	}
}

func Test_work_lost_response_is_uncertain_and_never_resent(t *testing.T) {
	for name, args := range map[string][]string{
		"create":     {"work", "create", "--kind", "story", "--title", "T", "--output", "json"},
		"update":     {"work", "update", "wi_1", "--title", "T", "--if-version", "2", "--output", "json"},
		"transition": {"work", "transition", "wi_1", "--to", "ready", "--if-version", "2", "--output", "json"},
	} {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}))
		r := c.run(args...)
		if r.code != 1 || r.out != "" {
			t.Errorf("%s: exit %d out %q err %q", name, r.code, r.out, r.err)
			continue
		}
		d := decodeJSON(t, r.err)["error"].(map[string]any)["details"].(map[string]any)
		rec, _ := d["recovery"].(string)
		if d["outcomeUncertain"] != true || strings.Contains(rec, "idempotency-key") || !(strings.Contains(rec, "work list") || strings.Contains(rec, "work get")) {
			t.Errorf("%s: details %v", name, d)
		}
		if n := len(c.srv.Requests()); n != 1 {
			t.Errorf("%s: %d requests; a lost answer is never resent", name, n)
		}
	}
}

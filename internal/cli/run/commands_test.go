package run_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

func startArgs(extra ...string) []string {
	return append([]string{"run", "start", "--work", "wi_1042", "--definition", "pd_01", "--content-revision", "2",
		"--source-commit", commit, "--idempotency-key", "run-create-01"}, extra...)
}

func Test_run_start_sends_the_exact_keyed_request(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 201, runDoc("queued", 1), map[string]string{"ETag": `"1"`})
	}))
	r := runCLI(t, srv, append(startArgs("--input", "image=art_image_01", "--input", "region=eu",
		"--input-json", "retries=3", "--input-json", "publish=true", "--input-json", `label="7"`,
		"--input-json", "big=9007199254740991"), "--output", "json")...)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	req := srv.Requests()[0]
	if req.Method != "POST" || req.Path != base+"/runs" || req.Header.Get("Idempotency-Key") != "run-create-01" || req.Header.Get("If-Match") != "" {
		t.Fatalf("request %+v", req)
	}
	want := `{"contentRevision":2,"definitionId":"pd_01","inputs":{"big":9007199254740991,"image":"art_image_01","label":"7","publish":true,"region":"eu","retries":3},` +
		`"sourceCommit":"` + commit + `","workItemId":"wi_1042"}`
	if got := strings.TrimSpace(string(req.Body)); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
	if out := decodeJSON(t, r.out); out["status"] != "queued" || out["id"] != runID {
		t.Fatalf("output %s", r.out)
	}
}

func Test_run_start_without_inputs_omits_them_and_reports_replays(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 201, runDoc("queued", 1), map[string]string{"Idempotent-Replayed": "true"})
	}))
	r := runCLI(t, srv, startArgs()...)
	if r.code != 0 || !strings.Contains(r.out, "replays the stored result") {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	if strings.Contains(string(srv.Requests()[0].Body), "inputs") {
		t.Fatalf("body %s", srv.Requests()[0].Body)
	}
}

func Test_run_start_rejects_ambiguous_input_before_any_request(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request sent: %s", r.URL.Path)
	}))
	bad := map[string][]string{
		"duplicate string":  startArgs("--input", "a=1", "--input", "a=2"),
		"duplicate across":  startArgs("--input", "a=1", "--input-json", "a=2"),
		"no equals":         startArgs("--input", "a"),
		"empty name":        startArgs("--input", "=1"),
		"name with space":   startArgs("--input", "a b=1"),
		"json float":        startArgs("--input-json", "a=1.5"),
		"json null":         startArgs("--input-json", "a=null"),
		"json array":        startArgs("--input-json", "a=[1]"),
		"json bare word":    startArgs("--input-json", "a=true1"),
		"json trailing":     startArgs("--input-json", "a=1 2"),
		"no key":            {"run", "start", "--work", "w", "--definition", "d", "--content-revision", "2", "--source-commit", commit},
		"empty key":         append(startArgs(), "--idempotency-key", ""),
		"invalid key":       append(startArgs(), "--idempotency-key", "no spaces"),
		"no revision":       {"run", "start", "--work", "w", "--definition", "d", "--source-commit", commit, "--idempotency-key", "k"},
		"revision zero":     {"run", "start", "--work", "w", "--definition", "d", "--content-revision", "0", "--source-commit", commit, "--idempotency-key", "k"},
		"revision fraction": {"run", "start", "--work", "w", "--definition", "d", "--content-revision", "2.0", "--source-commit", commit, "--idempotency-key", "k"},
		"short commit":      {"run", "start", "--work", "w", "--definition", "d", "--content-revision", "2", "--source-commit", "abc123", "--idempotency-key", "k"},
		"branch as commit":  {"run", "start", "--work", "w", "--definition", "d", "--content-revision", "2", "--source-commit", "main", "--idempotency-key", "k"},
		"missing work":      {"run", "start", "--definition", "d", "--content-revision", "2", "--source-commit", commit, "--idempotency-key", "k"},
		"extra argument":    append(startArgs(), "stray"),
		"uppercase commit":  {"run", "start", "--work", "w", "--definition", "d", "--content-revision", "2", "--source-commit", strings.Repeat("A", 40), "--idempotency-key", "k"},
	}
	for name, args := range bad {
		if r := runCLI(t, srv, args...); r.code != 2 || r.out != "" {
			t.Errorf("%s: exit %d out %q err %q", name, r.code, r.out, r.err)
		}
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}

func Test_run_reads_and_pages(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/steps"):
			clienttest.WriteJSON(w, 200, map[string]any{"items": []any{map[string]any{
				"id": "test", "kind": "container", "status": "failed", "attemptsStarted": 2, "maxAttempts": 3,
				"attempts": []any{map[string]any{"id": "att_1", "ordinal": 1, "status": "failed", "reason": "x", "settled": true}},
			}}, "nextCursor": "s.c", "extra": 1}, nil)
		case strings.HasSuffix(r.URL.Path, "/runs"):
			clienttest.WriteJSON(w, 200, map[string]any{"items": []any{runDoc("running", 3)}, "nextCursor": "r.c"}, nil)
		default:
			clienttest.WriteJSON(w, 200, runDoc("running", 3), nil)
		}
	}))
	l := runCLI(t, srv, "run", "list", "--status", "queued", "--status", "running", "--work", "wi_1042", "--limit", "20", "--cursor", "in.cur", "--output", "json")
	if l.code != 0 || !strings.Contains(l.out, `"nextCursor":"r.c"`) {
		t.Fatalf("list: exit %d out %q err %q", l.code, l.out, l.err)
	}
	q := srv.Requests()[0].RawQuery
	for _, want := range []string{"status=queued", "status=running", "workItemId=wi_1042", "limit=20", "cursor=in.cur"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %q", q, want)
		}
	}
	if r := runCLI(t, srv, "run", "list", "--status", "bogus"); r.code != 2 {
		t.Errorf("bad status: exit %d", r.code)
	}
	s := runCLI(t, srv, "run", "steps", runID, "--limit", "5")
	if s.code != 0 || !strings.Contains(s.out, "attempt att_1 #1") || !strings.Contains(s.out, "--cursor s.c") {
		t.Fatalf("steps: exit %d out %q", s.code, s.out)
	}
	g := runCLI(t, srv, "run", "get", runID)
	if g.code != 0 || !strings.Contains(g.out, "running") || !strings.Contains(g.out, "resource version: 3") {
		t.Fatalf("get: exit %d out %q", g.code, g.out)
	}
}

func Test_run_cancel_is_a_request_not_a_settlement(t *testing.T) {
	status := "running"
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := runDoc(status, 5)
		d["cancelRequestedAt"] = "2026-10-19T10:07:00Z"
		d["reason"] = "cancel_requested"
		clienttest.WriteJSON(w, 202, d, nil)
	}))
	r := runCLI(t, srv, "run", "cancel", runID)
	if r.code != 0 || !strings.Contains(r.out, "request, not a settled outcome") || !strings.Contains(r.out, "stays running") {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	req := srv.Requests()[0]
	if req.Method != "POST" || req.Path != base+"/runs/"+runID+"/cancel" || len(req.Body) != 0 || req.Header.Get("If-Match") != "" {
		t.Fatalf("cancel must be a bodyless POST: %+v", req)
	}
	status = "cancelled"
	if r := runCLI(t, srv, "run", "cancel", runID); r.code != 0 || !strings.Contains(r.out, "already cancelled") {
		t.Fatalf("terminal: exit %d out %q", r.code, r.out)
	}
}

func Test_scope_cursor_output_contract(t *testing.T) {
	for _, tc := range []struct{ status, exit int }{{401, 3}, {403, 3}, {404, 1}, {409, 4}, {412, 4}, {428, 2}, {429, 1}, {503, 1}} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, tc.status, "code_x", "m", "req_r", map[string]string{"Retry-After": "6"})
		}))
		r := runCLI(t, srv, startArgs("--output", "json")...)
		if int(r.code) != tc.exit || r.out != "" || errorOf(t, r.err)["requestId"] != "req_r" {
			t.Errorf("%d: exit %d out %q err %q", tc.status, r.code, r.out, r.err)
		}
		if (tc.status == 429 || tc.status == 503) && !strings.Contains(r.err, `"retryAfterSeconds":6`) {
			t.Errorf("%d: retry guidance lost", tc.status)
		}
	}
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteError(w, 404, "not_found", "m", "req_f", nil)
	}))
	runCLI(t, srv, "run", "get", runID, "--tenant", "t_other", "--project", "p_other")
	if got := srv.Requests()[0].Path; got != "/api/v1/tenants/t_other/projects/p_other/runs/"+runID {
		t.Fatalf("path %s", got)
	}
}

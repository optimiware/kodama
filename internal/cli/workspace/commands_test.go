package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/cli/workspace"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
	base    = "/api/v1/tenants/" + tenant + "/projects/" + project
)

type tokens struct{}

func (tokens) TokenSource(root.EffectiveContext) (client.AccessTokenSource, error) {
	return clienttest.Tokens("tok-1"), nil
}

type result struct {
	code root.ExitCode
	out  string
	err  string
}

func run(t *testing.T, srv *clienttest.Server, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	r, err := root.NewRunner(root.RunnerConfig{Streams: root.Streams{Out: &out, Err: &errb}, Tokens: tokens{}, AllowInsecureLoopback: true}, workspace.NewCommands())
	if err != nil {
		t.Fatal(err)
	}
	full := append([]string{}, args[:2]...)
	full = append(full, "--factory", srv.URL, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(context.Background(), full)
	return result{code, out.String(), errb.String()}
}

func ws(state string, version int, mutate func(map[string]any)) map[string]any {
	m := map[string]any{
		"id": "ws_01", "workItemId": "wi_1042", "repositoryId": "repo_1", "branch": "refs/heads/kodama/work/wi_1042",
		"acceptedCommit": strings.Repeat("1", 40), "state": state, "writerAttemptId": nil, "publication": nil,
		"archiveRequestedAt": nil, "lastError": nil, "version": version,
		"createdAt": "2026-10-19T10:00:00Z", "updatedAt": "2026-10-19T10:05:00Z",
	}
	if mutate != nil {
		mutate(m)
	}
	return m
}

func Test_workspace_provision_uses_work_item_route_without_version_or_key(t *testing.T) {
	for _, tc := range []struct {
		status int
		state  string
		mut    func(map[string]any)
		want   string
	}{
		{201, "available", nil, "Clone or fetch the branch"},
		{200, "available", nil, "Clone or fetch the branch"},
		{202, "provisioning", func(m map[string]any) { m["acceptedCommit"] = nil; m["lastError"] = "provision_unavailable" }, "still provisioning"},
	} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, tc.status, ws(tc.state, 3, tc.mut), map[string]string{"ETag": `"3"`})
		}))
		r := run(t, srv, "workspace", "provision", "--work", "wi_1042")
		if r.code != 0 || !strings.Contains(r.out, tc.want) || !strings.Contains(r.out, "refs/heads/kodama/work/wi_1042") {
			t.Fatalf("%d: exit %d out %q err %q", tc.status, r.code, r.out, r.err)
		}
		req := srv.Requests()[0]
		if req.Method != "POST" || req.Path != base+"/work-items/wi_1042/workspace" {
			t.Fatalf("request %+v", req)
		}
		if req.Header.Get("If-Match") != "" || req.Header.Get("Idempotency-Key") != "" || len(req.Body) != 0 {
			t.Fatalf("provision must carry no version, key or body: %+v", req)
		}
		if strings.Contains(r.out, "https://") || strings.Contains(r.out, "git@") {
			t.Fatalf("a clone URL was invented: %q", r.out)
		}
	}
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") }))
	if r := run(t, srv, "workspace", "provision"); r.code != 2 {
		t.Fatalf("missing --work: exit %d", r.code)
	}
}

func Test_workspace_get_json_is_server_document(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := ws("available", 9007199254740991, func(m map[string]any) {
			m["publication"] = map[string]any{"attemptId": "att_1", "commit": strings.Repeat("2", 40), "expectedCommit": strings.Repeat("1", 40), "startedAt": "2026-10-19T10:06:00Z"}
			m["writerAttemptId"] = "att_1"
			m["additive"] = true
		})
		clienttest.WriteJSON(w, 200, m, nil)
	}))
	r := run(t, srv, "workspace", "get", "ws_01", "--output", "json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	var m map[string]any
	d := json.NewDecoder(strings.NewReader(r.out))
	d.UseNumber()
	if err := d.Decode(&m); err != nil || m["additive"] != true || m["version"].(json.Number).String() != "9007199254740991" {
		t.Fatalf("output %q %v", r.out, err)
	}
	if h := run(t, srv, "workspace", "get", "ws_01"); !strings.Contains(h.out, "publication in flight: attempt att_1") {
		t.Fatalf("human output %q", h.out)
	}
	if got := srv.Requests()[0]; got.Method != "GET" || got.Path != base+"/workspaces/ws_01" {
		t.Fatalf("request %+v", got)
	}
}

func Test_workspace_pending_conflicts(t *testing.T) {
	type tc struct {
		action, code string
		status       int
		exit         int
	}
	for _, c := range []tc{
		{"archive", "archive_pending", 409, 4},
		{"restore", "publication_pending", 409, 4},
		{"sync", "branch_diverged", 409, 4},
		{"sync", "version_conflict", 412, 4},
		{"archive", "version_required", 428, 2},
	} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, c.status, c.code, "server message", "req_ws_1", map[string]string{"ETag": `"6"`})
		}))
		r := run(t, srv, "workspace", c.action, "ws_01", "--if-version", "5", "--output", "json")
		if int(r.code) != c.exit || r.out != "" {
			t.Errorf("%s %s: exit %d out %q", c.action, c.code, r.code, r.out)
		}
		env := decodeErr(t, r.err)
		if env["code"] != c.code || env["requestId"] != "req_ws_1" {
			t.Errorf("%s: envelope %v", c.code, env)
		}
		if c.status == 409 || c.status == 412 {
			if rec, _ := env["details"].(map[string]any)["recovery"].(string); !strings.Contains(rec, "workspace get") {
				t.Errorf("%s: no polling advice: %v", c.code, env)
			}
		}
		reqs := srv.Requests()
		if len(reqs) != 1 {
			t.Errorf("%s: the CLI sent %d requests; it must not refresh versions or retry", c.code, len(reqs))
		}
		if reqs[0].Header.Get("If-Match") != `"5"` {
			t.Errorf("%s: expected version not preserved: %q", c.code, reqs[0].Header.Get("If-Match"))
		}
	}
}

func decodeErr(t *testing.T, s string) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		t.Fatalf("stderr %q: %v", s, err)
	}
	return env.Error
}

func Test_workspace_restore_targets_the_same_workspace(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, ws("available", 8, nil), nil)
	}))
	r := run(t, srv, "workspace", "restore", "ws_01", "--if-version", "7")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Path != base+"/workspaces/ws_01/restore" || reqs[0].Header.Get("If-Match") != `"7"` {
		t.Fatalf("restore must be one request to the same workspace: %+v", reqs)
	}
	a := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, ws("archived", 9, nil), nil)
	}))
	r = run(t, a, "workspace", "archive", "ws_01", "--if-version", "8")
	if r.code != 0 || !strings.Contains(r.out, "archived") || !strings.Contains(r.out, "restore") {
		t.Fatalf("archive: exit %d out %q", r.code, r.out)
	}
	// An action without the caller's version never reaches the network.
	if r := run(t, a, "workspace", "sync", "ws_01"); r.code != 2 || len(a.Requests()) != 1 {
		t.Fatalf("sync without version: exit %d, %d requests", r.code, len(a.Requests()))
	}
}

func Test_scope_cursor_output_contract(t *testing.T) {
	for _, tc := range []struct {
		status, exit int
		code         string
	}{{401, 3, "unauthenticated"}, {403, 3, "forbidden"}, {404, 1, "not_found"}, {429, 1, "rate_limited"}, {503, 1, "unavailable"}} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, tc.status, tc.code, "m", "req_9", map[string]string{"Retry-After": "11"})
		}))
		r := run(t, srv, "workspace", "get", "ws_foreign", "--output", "json")
		if int(r.code) != tc.exit || r.out != "" {
			t.Errorf("%d: exit %d out %q", tc.status, r.code, r.out)
		}
		if (tc.status == 429 || tc.status == 503) && !strings.Contains(r.err, `"retryAfterSeconds":11`) {
			t.Errorf("%d: retry guidance lost: %s", tc.status, r.err)
		}
	}
}

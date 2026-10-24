package environment_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/environment"
	"github.com/optimiware/kodama/internal/cli/root"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
	base    = "/api/v1/tenants/" + tenant + "/projects/" + project
	envID   = "env_t037_1"
)

type tokenResolver struct{ src client.AccessTokenSource }

func (r tokenResolver) TokenSource(root.EffectiveContext) (client.AccessTokenSource, error) {
	return r.src, nil
}

type result struct {
	code root.ExitCode
	out  string
	err  string
}

type cli struct {
	t     *testing.T
	srv   *clienttest.Server
	token string
}

func newCLI(t *testing.T, h http.Handler) *cli {
	t.Helper()
	return &cli{t: t, srv: clienttest.NewServer(t, h), token: "tok-manager"}
}

func (c *cli) run(args ...string) result {
	c.t.Helper()
	return c.runScoped([]string{"--tenant", tenant, "--project", project}, args...)
}

func (c *cli) runScoped(scope []string, args ...string) result {
	c.t.Helper()
	var out, errb bytes.Buffer
	r, err := root.NewRunner(root.RunnerConfig{
		Streams:               root.Streams{Out: &out, Err: &errb},
		Tokens:                tokenResolver{clienttest.Tokens(c.token)},
		AllowInsecureLoopback: true,
	}, environment.NewCommands())
	if err != nil {
		c.t.Fatal(err)
	}
	full := append([]string{"environment", args[0], "--factory", c.srv.URL}, scope...)
	full = append(full, args[1:]...)
	code := r.Run(context.Background(), full)
	return result{code, out.String(), errb.String()}
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not a JSON document: %v: %q", err, s)
	}
	return m
}

func createArgs(extra ...string) []string {
	args := []string{"create", "preview", "--kind", "preview", "--target", "kind", "--namespace", "t037-preview",
		"--kubeconfig", "KUBECONFIG=secret://project/kubeconfig@file",
		"--secret", "REGISTRY_TOKEN=secret://project/registry-token@env",
		"--config", "REPLICAS=1", "--preview-ttl-seconds", "86400"}
	return append(args, extra...)
}

// factory is a stateful model of the public environment contract at the HTTP
// boundary only: scoped records, strict bodies, name uniqueness and expected
// versions. It is not database qualification (T-035/T-067).
type factory struct {
	mu       sync.Mutex
	envs     map[string]map[string]any // "tenant/project" -> id -> environment
	managers map[string]bool
	tenants  map[string]string
	dropNext bool
	unavail  bool // commit the next mutation, then answer a standard 503
	mutated  int
}

func newFactory() *factory {
	return &factory{
		envs: map[string]map[string]any{
			tenant + "/" + project: {envID: envDoc(envID, "seed", 1)},
			"t_other/" + project:   {"env_foreign": envDoc("env_foreign", "stage", 7)},
		},
		managers: map[string]bool{"tok-manager": true},
		tenants:  map[string]string{"tok-manager": tenant, "tok-viewer": tenant, "tok-other": "t_other"},
	}
}

func (f *factory) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *factory) drop()          { f.with(func() { f.dropNext = true }) }
func (f *factory) mutations() int { var n int; f.with(func() { n = f.mutated }); return n }

func envDoc(id, name string, version int64) map[string]any {
	return map[string]any{
		"id": id, "name": name, "kind": "preview", "target": "kind", "namespace": "t037-preview",
		"kubeconfig": map[string]any{"name": "KUBECONFIG", "ref": "secret://project/kubeconfig", "delivery": "file"},
		"secrets":    []any{map[string]any{"name": "REGISTRY_TOKEN", "ref": "secret://project/registry-token", "delivery": "env"}},
		"config":     map[string]any{"REPLICAS": "1", "AREA": "eu"}, "previewTtlSeconds": 86400,
		"version": version, "createdAt": "2026-01-02T03:04:05Z", "updatedAt": "2026-01-02T03:04:05Z",
	}
}

func etag(v any) string {
	b, _ := json.Marshal(v)
	return `"` + string(b) + `"`
}

func (f *factory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1"), "/"), "/")
	if len(parts) < 5 || parts[2] != "projects" || parts[4] != "environments" || f.tenants[token] != parts[1] {
		clienttest.WriteError(w, 404, "not_found", "the resource was not found", "req_404", nil)
		return
	}
	scope := parts[1] + "/" + parts[3]
	rest := parts[5:]
	mutating := r.Method != http.MethodGet
	if mutating && !f.managers[token] {
		clienttest.WriteError(w, 403, "forbidden", "deployment:manage is required", "req_403", nil)
		return
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		out := []any{}
		for _, e := range f.envs[scope] {
			out = append(out, e)
		}
		clienttest.WriteJSON(w, 200, map[string]any{"items": out, "nextCursor": "abc.def"}, map[string]string{"X-Request-ID": "req_list"})
	case len(rest) == 0 && r.Method == http.MethodPost:
		f.create(w, r, scope)
	case len(rest) == 1 && r.Method == http.MethodGet:
		e, ok := f.envs[scope][rest[0]]
		if !ok {
			clienttest.WriteError(w, 404, "not_found", "the resource was not found", "req_404", nil)
			return
		}
		clienttest.WriteJSON(w, 200, e, map[string]string{"ETag": etag(e.(map[string]any)["version"])})
	case len(rest) == 1 && r.Method == http.MethodPatch:
		f.update(w, r, scope, rest[0])
	default:
		clienttest.WriteError(w, 405, "method_not_allowed", "no such route", "req_405", nil)
	}
}

func strict(w http.ResponseWriter, r *http.Request, allowed ...string) (map[string]any, bool) {
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		clienttest.WriteError(w, 400, "bad_request", "malformed body", "req_400", nil)
		return nil, false
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for k := range m {
		if !ok[k] {
			clienttest.WriteError(w, 400, "bad_request", "unknown field "+k, "req_400", nil)
			return nil, false
		}
	}
	return m, true
}

func (f *factory) unavailable() { f.with(func() { f.unavail = true }) }

func (f *factory) lose(w http.ResponseWriter) bool {
	if f.unavail {
		f.unavail = false
		clienttest.WriteError(w, 503, "unavailable", "the commit outcome is unknown", "req_503", map[string]string{"Retry-After": "7"})
		return true
	}
	if !f.dropNext {
		return false
	}
	f.dropNext = false
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
	return true
}

func (f *factory) create(w http.ResponseWriter, r *http.Request, scope string) {
	m, ok := strict(w, r, "name", "kind", "target", "namespace", "kubeconfig", "secrets", "config", "previewTtlSeconds")
	if !ok {
		return
	}
	for _, e := range f.envs[scope] {
		if e.(map[string]any)["name"] == m["name"] {
			clienttest.WriteError(w, 409, "name_taken", "the environment name is used in the project", "req_409", nil)
			return
		}
	}
	e := envDoc("env_new", m["name"].(string), 1)
	for k, v := range m {
		e[k] = v
	}
	f.envs[scope]["env_new"] = e
	f.mutated++
	if f.lose(w) {
		return
	}
	clienttest.WriteJSON(w, 201, e, map[string]string{"ETag": `"1"`, "X-Request-ID": "req_create"})
}

func (f *factory) update(w http.ResponseWriter, r *http.Request, scope, id string) {
	e, ok := f.envs[scope][id]
	if !ok {
		clienttest.WriteError(w, 404, "not_found", "the resource was not found", "req_404", nil)
		return
	}
	env := e.(map[string]any)
	if r.Header.Get("If-Match") == "" {
		clienttest.WriteError(w, 428, "precondition_required", "If-Match is required", "req_428", nil)
		return
	}
	m, ok := strict(w, r, "target", "namespace", "kubeconfig", "secrets", "config", "previewTtlSeconds")
	if !ok {
		return
	}
	if r.Header.Get("If-Match") != etag(env["version"]) {
		clienttest.WriteError(w, 412, "precondition_failed", "the version changed", "req_412", nil)
		return
	}
	for k, v := range m {
		env[k] = v
	}
	env["version"] = env["version"].(int64) + 1
	f.mutated++
	if f.lose(w) {
		return
	}
	clienttest.WriteJSON(w, 200, env, map[string]string{"ETag": etag(env["version"]), "X-Request-ID": "req_update"})
}

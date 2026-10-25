package deployment_test

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
	"github.com/optimiware/kodama/internal/cli/deployment"
	"github.com/optimiware/kodama/internal/cli/root"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
	base    = "/api/v1/tenants/" + tenant + "/projects/" + project

	depA = "dep_0123456789abcdef0123456789abcdef"
	depB = "dep_fedcba9876543210fedcba9876543210"
	depC = "dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	commit  = "1111111111111111111111111111111111111111"
	repo    = "https://git.t037.invalid/acme/inventory.git"
	digest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digest2 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// tokenResolver yields the bearer token chosen by the test.
type tokenResolver struct{ src client.AccessTokenSource }

func (r tokenResolver) TokenSource(root.EffectiveContext) (client.AccessTokenSource, error) {
	return r.src, nil
}

type result struct {
	code root.ExitCode
	out  string
	err  string
}

// cli runs the real root runner and shared HTTP client against a recording
// server with the deployment contribution.
type cli struct {
	t     *testing.T
	srv   *clienttest.Server
	token string
}

func newCLI(t *testing.T, h http.Handler) *cli {
	t.Helper()
	return &cli{t: t, srv: clienttest.NewServer(t, h), token: "tok-manager"}
}

// run executes `kodama deployment <args...>` in the default scope.
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
	}, deployment.NewCommands())
	if err != nil {
		c.t.Fatal(err)
	}
	full := append([]string{"deployment", args[0], "--factory", c.srv.URL}, scope...)
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
	args := []string{"create", "--attempt-id", "att_deploy", "--repository-url", repo, "--commit", commit,
		"--image-artifact-id", "art_image", "--image-digest", digest, "--idempotency-key", "deploy-01"}
	return append(args, extra...)
}

func withFlag(args []string, name, value string) []string {
	out := append([]string{}, args...)
	for i, a := range out {
		if a == "--"+name {
			out[i+1] = value
			return out
		}
	}
	return append(out, "--"+name, value)
}

func without(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--"+name {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// record is one stored deployment in the fake factory.
type record struct {
	id, env, status, reason, cleanup string
	version                          int64
	unknown                          bool
	actionKey, attempt               string
	imageDigest                      string
}

func (r record) json() map[string]any {
	return map[string]any{
		"id": r.id, "environmentId": r.env, "configurationDigest": digest2, "namespace": "t037-preview",
		"source": map[string]any{"repositoryUrl": repo, "commit": commit},
		"image":  map[string]any{"artifactId": "art_image", "digest": r.imageDigest},
		"producer": map[string]any{"attemptId": "att_build", "runId": "run_0", "stepId": "build",
			"toolImage": "registry.t037.invalid/tools/build@" + digest},
		"runId": "run_1", "stepId": "deploy", "actionKey": r.actionKey, "attemptId": r.attempt,
		"status": r.status, "reason": r.reason, "outcomeUnknown": r.unknown,
		"objects":   []any{},
		"expiresAt": "2026-01-03T03:04:05Z",
		"cleanup":   map[string]any{"state": r.cleanup},
		"version":   r.version, "createdAt": "2026-01-02T03:04:05Z", "updatedAt": "2026-01-02T03:04:05Z",
	}
}

// factory is a stateful model of the public deployment contract at the HTTP
// boundary only: scoped records, hidden foreign records, strict bodies, the
// idempotency key and expected-version rules. It is not a claim of database
// qualification, which belongs to the delivery module (T-035/T-067).
type factory struct {
	mu       sync.Mutex
	scopes   map[string]map[string]*record // "tenant/project" -> id -> record
	keys     map[string]storedCreate       // scope+key -> first request
	managers map[string]bool               // tokens holding deployment:manage
	readers  map[string]string             // token -> tenant it may read
	dropNext bool                          // lose the answer of the next fresh mutation
	unavail  bool                          // commit the next fresh mutation, then answer 503
	mutated  int                           // mutations that changed state
}

// storedCreate is the first request under a key together with the exact
// response that was sent for it (status, headers, body), as the delivery
// handler stores it. A replay releases that snapshot, never a fresh read.
type storedCreate struct {
	body   []byte
	id     string
	status int
	header map[string]string
	resp   []byte
}

func newFactory() *factory {
	f := &factory{
		scopes: map[string]map[string]*record{
			tenant + "/" + project: {
				depA: {id: depA, env: "env_preview", status: "succeeded", reason: "ready", cleanup: "none", version: 3, actionKey: "run_1:deploy", attempt: "att_deploy", imageDigest: digest},
				depB: {id: depB, env: "env_preview", status: "running", reason: "applying", cleanup: "none", version: 2, actionKey: "run_2:deploy", attempt: "att_other", imageDigest: digest},
			},
			"t_other/" + project: {
				depC: {id: depC, env: "env_foreign", status: "succeeded", reason: "ready", cleanup: "none", version: 5, actionKey: "run_9:deploy", attempt: "att_foreign", imageDigest: digest},
			},
		},
		keys:     map[string]storedCreate{},
		managers: map[string]bool{"tok-manager": true},
		readers:  map[string]string{"tok-manager": tenant, "tok-viewer": tenant, "tok-other": "t_other"},
	}
	return f
}

// with runs fn while holding the factory's lock: the handler goroutine and the
// test share this state.
func (f *factory) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *factory) drop()          { f.with(func() { f.dropNext = true }) }
func (f *factory) unavailable()   { f.with(func() { f.unavail = true }) }
func (f *factory) mutations() int { var n int; f.with(func() { n = f.mutated }); return n }

// snapshot returns a copy of one stored record.
func (f *factory) snapshot(scope, id string) record {
	var r record
	f.with(func() { r = *f.rec(scope, id) })
	return r
}

func (f *factory) rec(scope, id string) *record {
	if s := f.scopes[scope]; s != nil {
		return s[id]
	}
	return nil
}

func notFound(w http.ResponseWriter) {
	clienttest.WriteError(w, 404, "not_found", "the resource was not found", "req_404", nil)
}

func (f *factory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1"), "/"), "/")
	if len(parts) < 5 || parts[0] != "tenants" || parts[2] != "projects" || parts[4] != "deployments" {
		notFound(w)
		return
	}
	scope := parts[1] + "/" + parts[3]
	if f.readers[token] != parts[1] {
		// A caller outside the tenant learns nothing about it.
		notFound(w)
		return
	}
	rest := parts[5:]
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		f.list(w, r, scope)
	case len(rest) == 0 && r.Method == http.MethodPost:
		f.create(w, r, scope, token)
	case len(rest) == 1 && r.Method == http.MethodGet:
		rec := f.rec(scope, rest[0])
		if rec == nil {
			notFound(w)
			return
		}
		clienttest.WriteJSON(w, 200, rec.json(), map[string]string{"ETag": etag(rec.version), "X-Request-ID": "req_get"})
	case len(rest) == 2 && rest[1] == "cleanup" && r.Method == http.MethodPost:
		f.cleanup(w, r, scope, rest[0], token)
	default:
		clienttest.WriteError(w, 405, "method_not_allowed", "no such route", "req_405", nil)
	}
}

func etag(v int64) string { return `"` + itoa(v) + `"` }

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (f *factory) list(w http.ResponseWriter, r *http.Request, scope string) {
	q := r.URL.Query()
	for k := range q {
		if k != "environmentId" && k != "status" && k != "limit" && k != "cursor" {
			clienttest.WriteError(w, 400, "bad_request", "unknown query parameter "+k, "req_400", nil)
			return
		}
	}
	var out []any
	for _, id := range []string{depA, depB, depC} {
		rec := f.rec(scope, id)
		if rec == nil {
			continue
		}
		if e := q.Get("environmentId"); e != "" && e != rec.env {
			continue
		}
		if s := q.Get("status"); s != "" && s != rec.status {
			continue
		}
		out = append(out, rec.json())
	}
	if out == nil {
		out = []any{}
	}
	clienttest.WriteJSON(w, 200, map[string]any{"items": out, "nextCursor": nil}, map[string]string{"X-Request-ID": "req_list"})
}

// lose ends a fresh mutation after it committed: the connection is dropped, or
// a standard 503 envelope is sent (the contract allows 503 for an unknown
// commit outcome).
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

func (f *factory) create(w http.ResponseWriter, r *http.Request, scope, token string) {
	if !f.managers[token] {
		clienttest.WriteError(w, 403, "forbidden", "deployment:manage is required", "req_403", nil)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		clienttest.WriteError(w, 428, "precondition_required", "Idempotency-Key is required", "req_428", nil)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		AttemptID string `json:"attemptId"`
		Source    struct {
			RepositoryURL string `json:"repositoryUrl"`
			Commit        string `json:"commit"`
		} `json:"source"`
		Image struct {
			ArtifactID string `json:"artifactId"`
			Digest     string `json:"digest"`
		} `json:"image"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		clienttest.WriteError(w, 400, "bad_request", "unknown or malformed field: "+err.Error(), "req_400", nil)
		return
	}
	stored, used := f.keys[scope+"|"+key]
	if used {
		if !bytes.Equal(stored.body, body) {
			clienttest.WriteError(w, 409, "idempotency_conflict", "the key was used with a different request", "req_conflict", nil)
			return
		}
		// Replay the stored response unchanged, whatever the live record is now.
		for k, v := range stored.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("X-Request-ID", "req_replay")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stored.status)
		_, _ = w.Write(stored.resp)
		return
	}
	// The logical action is derived from the attempt on the server.
	action := "run_for_" + req.AttemptID + ":deploy"
	for _, rec := range f.scopes[scope] {
		if rec.actionKey == action {
			if rec.imageDigest != req.Image.Digest {
				clienttest.WriteError(w, 409, "conflict", "different immutable inputs for the action", "req_conflict2", nil)
				return
			}
			f.keys[scope+"|"+key] = f.snapshotCreate(body, rec, 200, receipt(rec, false), map[string]string{"ETag": etag(rec.version)})
			clienttest.WriteJSON(w, 200, receipt(rec, false), map[string]string{"ETag": etag(rec.version), "X-Request-ID": "req_observed"})
			return
		}
	}
	id := "dep_" + strings.Repeat("c", 32)
	rec := &record{id: id, env: "env_preview", status: "pending", reason: "registered", cleanup: "none", version: 1, actionKey: action, attempt: req.AttemptID, imageDigest: req.Image.Digest}
	f.scopes[scope][id] = rec
	storedHdr := map[string]string{"ETag": etag(1), "Location": base + "/deployments/" + id}
	f.keys[scope+"|"+key] = f.snapshotCreate(body, rec, 201, receipt(rec, true), storedHdr)
	hdr := map[string]string{"ETag": etag(1), "Location": base + "/deployments/" + id, "X-Request-ID": "req_first"}
	f.mutated++
	if f.lose(w) {
		return
	}
	clienttest.WriteJSON(w, 201, receipt(rec, true), hdr)
}

func (f *factory) snapshotCreate(body []byte, rec *record, status int, doc map[string]any, hdr map[string]string) storedCreate {
	b, _ := json.Marshal(doc)
	return storedCreate{body: body, id: rec.id, status: status, header: hdr, resp: b}
}

func receipt(rec *record, created bool) map[string]any {
	return map[string]any{
		"created": created,
		"annotations": map[string]any{
			"kodama.optimi.com/deployment-id":        rec.id,
			"kodama.optimi.com/action-key":           rec.actionKey,
			"kodama.optimi.com/configuration-digest": digest2,
		},
		"deployment": rec.json(),
	}
}

func (f *factory) cleanup(w http.ResponseWriter, r *http.Request, scope, id, token string) {
	if !f.managers[token] {
		clienttest.WriteError(w, 403, "forbidden", "deployment:manage is required", "req_403", nil)
		return
	}
	rec := f.rec(scope, id)
	if rec == nil {
		notFound(w)
		return
	}
	if r.Header.Get("If-Match") == "" {
		clienttest.WriteError(w, 428, "precondition_required", "If-Match is required", "req_428", nil)
		return
	}
	if body, _ := io.ReadAll(r.Body); len(body) != 0 {
		clienttest.WriteError(w, 400, "bad_request", "the request body must be empty", "req_400", nil)
		return
	}
	if r.Header.Get("If-Match") != etag(rec.version) {
		clienttest.WriteError(w, 412, "precondition_failed", "the version changed", "req_412", nil)
		return
	}
	if rec.cleanup == "requested" {
		clienttest.WriteJSON(w, 200, rec.json(), map[string]string{"ETag": etag(rec.version), "X-Request-ID": "req_noop"})
		return
	}
	if rec.status != "succeeded" && rec.status != "failed" && !rec.unknown {
		clienttest.WriteError(w, 422, "invalid_transition", "only a settled deployment can be cleaned up", "req_422", nil)
		return
	}
	rec.cleanup = "requested"
	rec.version++
	f.mutated++
	if f.lose(w) {
		return
	}
	clienttest.WriteJSON(w, 200, rec.json(), map[string]string{"ETag": etag(rec.version), "X-Request-ID": "req_cleanup"})
}

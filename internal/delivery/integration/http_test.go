package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
)

// httpStack serves the real handlers over the real service, store, idempotency
// adapter and runtime authority.
type httpStack struct {
	*stack
	pub, rt *httpapi.Router
	lossy   *deliverytest.LossyDB
	deny    func(httpapi.Action, httpapi.Target) error
	mu      sync.Mutex
}

type userTokens map[string]string

func (u userTokens) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || u[tok] == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: u[tok]}, nil
}

// attemptTokens is the runtime router's authenticator: it verifies the attempt
// token's signature and nothing else. The service still authenticates
// deployment.call for the route's attempt against the durable grant.
type attemptTokens struct{ s *stack }

func (a attemptTokens) Authenticate(ctx context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	claims, err := a.s.Verifier.Verify(ctx, tok)
	if err != nil {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: "attempt:" + string(claims.Session)}, nil
}

type switchAuthz struct{ h *httpStack }

func (a switchAuthz) Authorize(_ context.Context, _ httpapi.Principal, action httpapi.Action, t httpapi.Target) error {
	a.h.mu.Lock()
	deny := a.h.deny
	a.h.mu.Unlock()
	if deny != nil {
		return deny(action, t)
	}
	return nil
}

func newHTTPStack(t *testing.T, o deliverytest.Options, withLossy bool) *httpStack {
	t.Helper()
	h := &httpStack{stack: newStack(t, o, deliverytest.FlowOptions{})}
	idem := h.X.Idem
	if withLossy {
		h.lossy = &deliverytest.LossyDB{Inner: h.X.Runtime}
		var err error
		if idem, err = httppg.NewIdempotency(httppg.Config{DB: h.lossy}); err != nil {
			t.Fatal(err)
		}
	}
	keyed, err := deliverypg.NewKeyed(idem, h.Store)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h.pub, err = httpapi.NewRouter(httpapi.RouterConfig{Authenticator: userTokens{"tok-engineer": deliverytest.Engineer, "tok-viewer": deliverytest.Viewer},
		Authorizer: switchAuthz{h}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	h.rt, err = httpapi.NewRouter(httpapi.RouterConfig{Authenticator: attemptTokens{h.stack}, Authorizer: switchAuthz{h}, RateLimiter: httpapi.Unlimited(), Prefix: "/runtime/v1"})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := deliveryhttp.NewHandler(h.Svc, keyed, codec, deliveryhttp.DefaultActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.RegisterRoutes(h.pub); err != nil {
		t.Fatal(err)
	}
	rt, err := deliveryhttp.NewRuntime(h.Svc, deliveryhttp.DefaultRuntimeActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.RegisterRoutes(h.rt); err != nil {
		t.Fatal(err)
	}
	return h
}

type reply struct {
	*httptest.ResponseRecorder
}

func (h *httpStack) do(r *httpapi.Router, token, method, path, body string, headers ...string) reply {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return reply{rec}
}

const apiBase = "/api/v1/tenants/t035_acme/projects/p035_inventory"

func (h *httpStack) human(method, path, body string, headers ...string) reply {
	return h.do(h.pub, "tok-engineer", method, apiBase+path, body, headers...)
}

func (h *httpStack) attempt(a *deliverytest.Attempt, method, path, body string) reply {
	return h.do(h.rt, a.Token, method, "/runtime/v1/attempts/"+string(a.ID())+path, body)
}

func (r reply) json(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, r.Body.String())
	}
	return m
}

func (r reply) code(t testing.TB) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (r reply) deployment(t testing.TB) map[string]any {
	t.Helper()
	m := r.json(t)
	if d, ok := m["deployment"].(map[string]any); ok {
		return d
	}
	return m
}

func (h *httpStack) createBody(req delivery.RegistrationRequest, _ string) string {
	b, _ := json.Marshal(map[string]any{
		"attemptId": string(h.D.Deploy.ID()),
		"source":    map[string]string{"repositoryUrl": req.Source.RepositoryURL, "commit": req.Source.Commit},
		"image":     map[string]string{"artifactId": string(req.Image.ArtifactID), "digest": string(req.Image.Digest)},
	})
	return string(b)
}

func (h *httpStack) registerBody() string {
	b, _ := json.Marshal(map[string]any{
		"source": map[string]string{"repositoryUrl": h.Req.Source.RepositoryURL, "commit": h.Req.Source.Commit},
		"image":  map[string]string{"artifactId": string(h.Req.Image.ArtifactID), "digest": string(h.Req.Image.Digest)},
	})
	return string(b)
}

// ---- fixture conformance ---------------------------------------------------------

type fixtureExchange struct {
	Name     string `json:"name"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"response"`
}

// published loads one exchange of a published fixture.
func published(t testing.TB, fixture, name string) fixtureExchange {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", fixture+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Exchanges []fixtureExchange `json:"exchanges"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Exchanges {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("the fixture %s has no exchange %q", fixture, name)
	return fixtureExchange{}
}

// optionalKeys are members the contract documents as present only for some
// resources: a stage deployment has no expiry, an object has a uid once observed,
// a preview environment has a TTL.
var optionalKeys = map[string]bool{"expiresAt": true, "deletedAt": true, "requestedAt": true, "uid": true, "ready": true, "previewTtlSeconds": true}

// shape reduces a JSON value to its structure: object keys and scalar types.
// Maps whose keys are data (annotations, config) are a single leaf, and an empty
// array matches any array.
func shape(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		if key == "annotations" || key == "config" {
			return "map"
		}
		out := map[string]any{}
		for k, c := range x {
			if optionalKeys[k] {
				continue // present for some kinds and states only (stage, deleted, ready ...)
			}
			out[k] = shape(k, c)
		}
		return out
	case []any:
		if len(x) == 0 {
			return "array"
		}
		return []any{shape(key, x[0])}
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return "?"
}

func sameShape(a, b any) bool {
	if sa, ok := a.(string); ok && sa == "array" {
		_, isArr := b.([]any)
		return isArr || b == "array"
	}
	if sb, ok := b.(string); ok && sb == "array" {
		_, isArr := a.([]any)
		return isArr || a == "array"
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		if len(am) != len(bm) {
			return false
		}
		for k, av := range am {
			bv, ok := bm[k]
			if !ok || !sameShape(av, bv) {
				return false
			}
		}
		return true
	}
	aa, aok := a.([]any)
	ba, bok := b.([]any)
	if aok && bok {
		return len(aa) == len(ba) && (len(aa) == 0 || sameShape(aa[0], ba[0]))
	}
	return reflect.DeepEqual(a, b)
}

// conforms checks the real response has the published status, header names and
// body structure.
func conforms(t testing.TB, fixture, name string, r reply) {
	t.Helper()
	want := published(t, fixture, name)
	if r.Code != want.Response.Status {
		t.Errorf("%s/%s: status %d, published %d: %s", fixture, name, r.Code, want.Response.Status, r.Body.String())
		return
	}
	for h := range want.Response.Headers {
		if r.Header().Get(h) == "" {
			t.Errorf("%s/%s: the published header %s is absent", fixture, name, h)
		}
	}
	var wantBody, gotBody any
	if err := json.Unmarshal(want.Response.Body, &wantBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r.Body.Bytes(), &gotBody); err != nil {
		t.Fatalf("%s/%s: %v: %s", fixture, name, err, r.Body.String())
	}
	if !sameShape(shape("", wantBody), shape("", gotBody)) {
		w, _ := json.Marshal(shape("", wantBody))
		g, _ := json.Marshal(shape("", gotBody))
		t.Errorf("%s/%s: the real response has another structure than the published one:\npublished %s\nreal      %s", fixture, name, w, g)
	}
	if want.Response.Status >= 400 {
		if got := r.code(t); got != gotCode(want.Response.Body) {
			t.Errorf("%s/%s: error code %s, published %s", fixture, name, got, gotCode(want.Response.Body))
		}
	}
}

func gotCode(raw json.RawMessage) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Error.Code
}

// Test_delivery_http_idempotency proves the public and runtime mutations through
// the real handlers over real PostgreSQL: concurrent creation, changed-body
// conflicts, reauthorization of replays, lost acknowledgments, version
// preconditions inside the write, audience separation and disclosure, and that
// the real responses have the structure the published fixtures promise.
func Test_delivery_http_idempotency(t *testing.T) {
	ctx := context.Background()
	ns := deliverytest.TargetNamespace

	t.Run("concurrent public creation with one key creates and replays exactly once", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		body := h.createBody(h.Req, "")
		const n = 8
		var wg sync.WaitGroup
		res := make([]reply, n)
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				res[i] = h.human("POST", "/deployments", body, "Idempotency-Key", "k-concurrent")
			}()
		}
		close(start)
		wg.Wait()
		replayed := 0
		for i, r := range res {
			if r.Code != http.StatusCreated {
				t.Fatalf("request %d: %d %s", i, r.Code, r.Body.String())
			}
			if r.Header().Get("Idempotent-Replayed") == "true" {
				replayed++
			}
			if r.Body.String() != res[0].Body.String() || r.Header().Get("ETag") != `"1"` {
				t.Fatalf("request %d did not return the one stored response", i)
			}
		}
		id := delivery.DeploymentID(res[0].deployment(t)["id"].(string))
		if replayed != n-1 || h.deployments() != 1 || h.history(id) != 1 || h.events() != 1 || h.pins() != 1 {
			t.Fatalf("replayed=%d deployments=%d history=%d events=%d pins=%d", replayed, h.deployments(), h.history(id), h.events(), h.pins())
		}
		if h.audits("deployment.register") != 1 || h.audits("deployment.observe") != 0 {
			t.Fatalf("audits: %d registers, %d observes", h.audits("deployment.register"), h.audits("deployment.observe"))
		}
		conforms(t, "registration", "register", res[0])
	})

	t.Run("different keys for one logical action observe the original", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		body := h.createBody(h.Req, "")
		const n = 6
		var wg sync.WaitGroup
		res := make([]reply, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i] = h.human("POST", "/deployments", body, "Idempotency-Key", "k-"+string(rune('a'+i)))
			}()
		}
		wg.Wait()
		created := 0
		for i, r := range res {
			switch r.Code {
			case http.StatusCreated:
				created++
			case http.StatusOK:
				if r.json(t)["created"] != false {
					t.Fatalf("request %d observed the original but reports created", i)
				}
			default:
				t.Fatalf("request %d: %d %s", i, r.Code, r.Body.String())
			}
		}
		id := delivery.DeploymentIDFor(scopeA, h.D.Deploy.Context.ActionKey)
		if created != 1 || h.deployments() != 1 || h.history(id) != 1 || h.events() != 1 || h.pins() != 1 {
			t.Fatalf("created=%d deployments=%d history=%d events=%d pins=%d", created, h.deployments(), h.history(id), h.events(), h.pins())
		}
		if h.audits("deployment.register") != 1 || h.audits("deployment.observe") != n-1 {
			t.Fatalf("audits: %d registers, %d observes", h.audits("deployment.register"), h.audits("deployment.observe"))
		}
		for _, r := range res {
			if r.Code == http.StatusOK {
				conforms(t, "registration", "register again after a lost response, by a retry attempt of the same step", r)
				break
			}
		}
	})

	t.Run("a changed body under the same key conflicts and writes nothing", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		body := h.createBody(h.Req, "")
		if r := h.human("POST", "/deployments", body, "Idempotency-Key", "k-1"); r.Code != 201 {
			t.Fatal(r.Body.String())
		}
		// Byte-different JSON is a different request, even when it means the same:
		// the stored fingerprint covers the exact body.
		changed := strings.Replace(body, "{", "{ ", 1)
		r := h.human("POST", "/deployments", changed, "Idempotency-Key", "k-1")
		if r.Code != http.StatusConflict || r.code(t) != "idempotency_conflict" {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		conforms(t, "stage_promotion", "a changed body under the same key conflicts", r)
		if h.deployments() != 1 || h.events() != 1 || h.pins() != 1 {
			t.Fatal("a conflicting request changed state")
		}
	})

	t.Run("a refused creation writes nothing and holds no retention pin", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		wrong := strings.Replace(h.createBody(h.Req, ""), h.D.Source.Commit, strings.Repeat("2", 40), 1)
		r := h.human("POST", "/deployments", wrong, "Idempotency-Key", "k-wrong")
		if r.Code != 422 || r.code(t) != "image_provenance_rejected" {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		conforms(t, "stage_promotion", "an image that is not a verified output of this source is refused", r)
		if h.deployments() != 0 || h.events() != 0 || h.pins() != 0 || h.count(`SELECT count(*) FROM idempotency WHERE tenant_id = $1`, string(scopeA.TenantID)) != 0 {
			t.Fatal("a refused creation left state")
		}
		// A viewer cannot create, an unknown attempt is not found.
		if r := h.do(h.pub, "tok-viewer", "POST", apiBase+"/deployments", h.createBody(h.Req, ""), "Idempotency-Key", "k-viewer"); r.Code != http.StatusForbidden {
			t.Fatalf("a viewer created a deployment: %d", r.Code)
		}
		unknown := strings.Replace(h.createBody(h.Req, ""), string(h.D.Deploy.ID()), "att_t035_nowhere", 1)
		if r := h.human("POST", "/deployments", unknown, "Idempotency-Key", "k-unknown"); r.Code != http.StatusNotFound {
			t.Fatalf("an unknown attempt: %d %s", r.Code, r.Body.String())
		}
	})

	t.Run("a replay is authorized again before the stored response is released", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		body := h.createBody(h.Req, "")
		first := h.human("POST", "/deployments", body, "Idempotency-Key", "k-reauth")
		if first.Code != 201 {
			t.Fatal(first.Body.String())
		}
		id := first.deployment(t)["id"].(string)
		// The route allows the caller but not the stored deployment any more.
		h.mu.Lock()
		h.deny = func(_ httpapi.Action, tg httpapi.Target) error {
			if tg.Resource.Kind == deliveryhttp.ResourceDeployment {
				return httpapi.ErrDenied
			}
			return nil
		}
		h.mu.Unlock()
		r := h.human("POST", "/deployments", body, "Idempotency-Key", "k-reauth")
		if r.Code != http.StatusForbidden || strings.Contains(r.Body.String(), id) || r.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("a stored response was released after the deployment became unauthorized: %d %s", r.Code, r.Body.String())
		}
		h.mu.Lock()
		h.deny = nil
		h.mu.Unlock()
		// The current role is rechecked too: the same caller as a viewer is refused.
		r = h.do(h.pub, "tok-viewer", "POST", apiBase+"/deployments", body, "Idempotency-Key", "k-reauth")
		if r.Code != http.StatusForbidden {
			t.Fatalf("a viewer replayed an engineer's creation: %d", r.Code)
		}
		// And the authorized replay still returns the stored response.
		again := h.human("POST", "/deployments", body, "Idempotency-Key", "k-reauth")
		if again.Code != 201 || again.Header().Get("Idempotent-Replayed") != "true" || again.Body.String() != first.Body.String() {
			t.Fatalf("replay: %d", again.Code)
		}
		conforms(t, "stage_promotion", "a retry with the same key and body replays the stored response", again)
	})

	t.Run("a lost commit acknowledgment is recovered by repeating the request", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, true)
		body := h.createBody(h.Req, "")
		h.lossy.Lose()
		lost := h.human("POST", "/deployments", body, "Idempotency-Key", "k-lost")
		h.lossy.Off()
		if lost.Code != http.StatusServiceUnavailable || lost.Header().Get("Retry-After") == "" {
			t.Fatalf("a lost acknowledgment must be unavailable and retryable: %d %s", lost.Code, lost.Body.String())
		}
		id := delivery.DeploymentIDFor(scopeA, h.D.Deploy.Context.ActionKey)
		if h.deployments() != 1 || h.history(id) != 1 || h.events() != 1 {
			t.Fatalf("the commit did not apply: %d %d %d", h.deployments(), h.history(id), h.events())
		}
		if h.pins() != 1 {
			t.Fatalf("an uncertain outcome must keep the image protected: %d pins", h.pins())
		}
		retry := h.human("POST", "/deployments", body, "Idempotency-Key", "k-lost")
		if retry.Code != 201 || retry.Header().Get("Idempotent-Replayed") != "true" || retry.json(t)["created"] != true {
			t.Fatalf("the retry did not replay the stored response: %d %s", retry.Code, retry.Body.String())
		}
		if h.deployments() != 1 || h.history(id) != 1 || h.events() != 1 || h.pins() != 1 || h.audits("deployment.register") != 1 {
			t.Fatal("the retry ran the registration again")
		}
		// A failed commit (rolled back, error reported) leaves nothing and is retried to success.
		h2 := newHTTPStack(t, deliverytest.Options{}, true)
		h2.lossy.Fail()
		failed := h2.human("POST", "/deployments", h2.createBody(h2.Req, ""), "Idempotency-Key", "k-fail")
		h2.lossy.Off()
		if failed.Code != http.StatusServiceUnavailable || h2.deployments() != 0 || h2.events() != 0 {
			t.Fatalf("a failed commit: %d deployments=%d", failed.Code, h2.deployments())
		}
		ok := h2.human("POST", "/deployments", h2.createBody(h2.Req, ""), "Idempotency-Key", "k-fail")
		if ok.Code != 201 || h2.deployments() != 1 || h2.pins() != 1 {
			t.Fatalf("the retry after a failed commit: %d pins=%d", ok.Code, h2.pins())
		}
	})

	t.Run("environment edits compare the version inside the write", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		path := "/environments/" + string(h.D.Env.ID())
		patch := func(v, replicas string) reply {
			return h.human("PATCH", path, `{"config":{"REPLICAS":"`+replicas+`"}}`, "If-Match", v)
		}
		if r := h.human("PATCH", path, `{"config":{"REPLICAS":"2"}}`); r.Code != 428 || r.code(t) != "version_required" {
			t.Fatalf("%d", r.Code)
		}
		// Concurrent edits from the same version: exactly one wins.
		const n = 6
		var wg sync.WaitGroup
		res := make([]reply, n)
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); res[i] = patch(`"1"`, string(rune('2'+i))) }()
		}
		wg.Wait()
		wins, stale := 0, 0
		for _, r := range res {
			switch r.Code {
			case 200:
				wins++
				if r.Header().Get("ETag") != `"2"` {
					t.Fatalf("ETag %q", r.Header().Get("ETag"))
				}
			case 412:
				stale++
			default:
				t.Fatalf("%d %s", r.Code, r.Body.String())
			}
		}
		if wins != 1 || stale != n-1 {
			t.Fatalf("wins=%d stale=%d: the version comparison is not part of the write", wins, stale)
		}
		if got := h.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = 'environment.update'`, string(scopeA.TenantID)); got != 1 {
			t.Fatalf("%d audit rows for one winning edit", got)
		}
		if r := patch(`"2"`, "9"); r.Code != 200 || r.Header().Get("ETag") != `"3"` {
			t.Fatalf("the next version: %d", r.Code)
		}
		if r := patch(`"1"`, "9"); r.Code != 412 {
			t.Fatalf("a stale version: %d", r.Code)
		}
		// A no-op edit leaves the version alone.
		if r := patch(`"3"`, "9"); r.Code != 200 || r.Header().Get("ETag") != `"3"` {
			t.Fatalf("a no-op edit: %d %s", r.Code, r.Header().Get("ETag"))
		}
		// A viewer cannot edit.
		if r := h.do(h.pub, "tok-viewer", "PATCH", apiBase+path, `{"config":{"A":"1"}}`, "If-Match", `"3"`); r.Code != http.StatusForbidden {
			t.Fatalf("a viewer edited an environment: %d", r.Code)
		}
		// An environment of another tenant is not found, never edited.
		if r := h.do(h.pub, "tok-engineer", "PATCH", "/api/v1/tenants/t035_globex/projects/p035_ops/environments/"+string(h.D.Env.ID()), `{"config":{"A":"1"}}`, "If-Match", `"3"`); r.Code != http.StatusNotFound && r.Code != http.StatusForbidden {
			t.Fatalf("a foreign scope: %d", r.Code)
		}
	})

	t.Run("the cleanup transition requires and compares the current version", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		id, _ := h.settled(t)
		path := "/deployments/" + string(id) + "/cleanup"
		cur, _ := h.Store.GetDeployment(ctx, scopeA, id)
		if r := h.human("POST", path, ""); r.Code != 428 {
			t.Fatalf("without If-Match: %d", r.Code)
		}
		if r := h.human("POST", path, "", "If-Match", `"1"`); r.Code != 412 {
			t.Fatalf("stale: %d", r.Code)
		}
		if h.audits("deployment.cleanup_requested") != 0 {
			t.Fatal("a stale request recorded a cleanup")
		}
		v := `"` + itoa(int64(cur.Version())) + `"`
		const n = 5
		var wg sync.WaitGroup
		res := make([]reply, n)
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); res[i] = h.human("POST", path, "", "If-Match", v) }()
		}
		wg.Wait()
		wins, stale := 0, 0
		for _, r := range res {
			switch r.Code {
			case 200:
				wins++
			case 412:
				stale++
			default:
				t.Fatalf("%d %s", r.Code, r.Body.String())
			}
		}
		if wins != 1 || stale != n-1 || h.audits("deployment.cleanup_requested") != 1 {
			t.Fatalf("wins=%d stale=%d audits=%d", wins, stale, h.audits("deployment.cleanup_requested"))
		}
		var win reply
		for _, r := range res {
			if r.Code == 200 {
				win = r
			}
		}
		if win.deployment(t)["cleanup"].(map[string]any)["state"] != "requested" {
			t.Fatalf("%s", win.Body.String())
		}
		// A repeat on the old version is stale, never a second request; on the new one it is a no-op.
		if r := h.human("POST", path, "", "If-Match", v); r.Code != 412 {
			t.Fatalf("a repeat on the old version: %d", r.Code)
		}
		next := `"` + itoa(int64(cur.Version())+1) + `"`
		if r := h.human("POST", path, "", "If-Match", next); r.Code != 200 || h.audits("deployment.cleanup_requested") != 1 {
			t.Fatalf("a repeat on the current version: %d", r.Code)
		}
		// An unsettled deployment is not cleaned up.
		h2 := newHTTPStack(t, deliverytest.Options{}, false)
		id2 := h2.mustRegister(t).Deployment.ID()
		if r := h2.human("POST", "/deployments/"+string(id2)+"/cleanup", "", "If-Match", `"1"`); r.Code != 422 || r.code(t) != "invalid_transition" {
			t.Fatalf("cleanup of a pending deployment: %d %s", r.Code, r.Body.String())
		}
	})

	t.Run("audiences are separate: attempt tokens never reach human routes and human tokens never reach runtime routes", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		// An attempt token on a human route and a human token on a runtime route.
		if r := h.do(h.pub, h.D.Deploy.Token, "GET", apiBase+"/deployments", ""); r.Code != http.StatusUnauthorized {
			t.Fatalf("an attempt token on a human route: %d", r.Code)
		}
		if r := h.do(h.rt, "tok-engineer", "POST", "/runtime/v1/attempts/"+string(h.D.Deploy.ID())+"/deployments", h.registerBody()); r.Code != http.StatusUnauthorized {
			t.Fatalf("a human token on a runtime route: %d", r.Code)
		}
		// A token for one attempt on another attempt's route.
		if r := h.do(h.rt, h.D.Deploy.Token, "POST", "/runtime/v1/attempts/"+string(h.D.Build.ID())+"/deployments", h.registerBody()); r.Code < 400 {
			t.Fatalf("a token worked on another attempt's route: %d", r.Code)
		}
		// A body cannot choose the environment, the scope or the action key.
		for _, field := range []string{`"environmentId":"env_other"`, `"tenantId":"t035_globex"`, `"actionKey":"other"`, `"configurationDigest":"sha256:` + strings.Repeat("0", 64) + `"`} {
			body := `{` + field + `,` + h.registerBody()[1:]
			if r := h.do(h.rt, h.D.Deploy.Token, "POST", "/runtime/v1/attempts/"+string(h.D.Deploy.ID())+"/deployments", body); r.Code != http.StatusBadRequest {
				t.Fatalf("%s: %d", field, r.Code)
			}
		}
		if h.deployments() != 0 {
			t.Fatal("a refused runtime call created a deployment")
		}
		// An attempt without the deployment.call action is forbidden.
		h2 := newHTTPStack(t, deliverytest.Options{WithoutDeploymentCall: true}, false)
		r := h2.attempt(h2.D.Deploy, "POST", "/deployments", h2.registerBody())
		if r.Code != http.StatusForbidden || r.code(t) != "forbidden" {
			t.Fatalf("an attempt without the action: %d %s", r.Code, r.Body.String())
		}
		// A revoked attempt is denied even for a replay (no result.submit exception).
		h3 := newHTTPStack(t, deliverytest.Options{}, false)
		if r := h3.attempt(h3.D.Deploy, "POST", "/deployments", h3.registerBody()); r.Code != 201 {
			t.Fatal(r.Body.String())
		}
		h3.Revoke(h3.D.Deploy)
		r = h3.attempt(h3.D.Deploy, "POST", "/deployments", h3.registerBody())
		if r.Code < 400 {
			t.Fatalf("a revoked attempt registered: %d", r.Code)
		}
		if r.Code == http.StatusForbidden && r.code(t) != "attempt_revoked" && r.code(t) != "forbidden" {
			t.Fatalf("revoked: %s", r.Body.String())
		}
	})

	t.Run("the real responses have the structure the published fixtures promise", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		intended := objects(ns, "inventory", "inventory-worker")
		objs := func(o []delivery.ObjectRef) string {
			b, _ := json.Marshal(deliveryObjects(o))
			return string(b)
		}
		reg := h.attempt(h.D.Deploy, "POST", "/deployments", h.registerBody())
		conforms(t, "registration", "register", reg)
		id := reg.deployment(t)["id"].(string)
		conforms(t, "annotation_receipts", "receipt annotations", h.attempt(h.D.Deploy, "POST", "/deployments", h.registerBody()))
		retry := h.Retry(h.D)
		conforms(t, "registration", "register again after a lost response, by a retry attempt of the same step", h.attempt(retry, "POST", "/deployments", h.registerBody()))
		conforms(t, "pre_apply_intent", "record the intended objects", h.attempt(retry, "POST", "/deployments/"+id+"/outcome", `{"state":"applying","objects":`+objs(intended)+`}`))
		conforms(t, "pre_apply_intent", "different intended objects are a conflict", h.attempt(retry, "POST", "/deployments/"+id+"/outcome", `{"state":"applying","objects":`+objs(objects(ns, "other"))+`}`))
		conforms(t, "lookup", "lookup the attempt's own logical action", h.attempt(retry, "GET", "/deployments/by-action/"+retry.Context.ActionKey, ""))
		conforms(t, "lookup", "another action key is refused", h.attempt(retry, "GET", "/deployments/by-action/run_t035_other:deploy", ""))
		conforms(t, "lookup", "read the deployment", h.attempt(retry, "GET", "/deployments/"+id, ""))
		conforms(t, "structured_outcomes", "succeeded with every object observed ready", h.attempt(retry, "POST", "/deployments/"+id+"/outcome", `{"state":"succeeded","objects":`+objs(observed(intended))+`}`))
		conforms(t, "structured_outcomes", "a settled outcome is never rewritten", h.attempt(retry, "POST", "/deployments/"+id+"/outcome", `{"state":"failed","reason":"changed_my_mind"}`))

		// Cleanup: the public request, then a bound cleanup run's reports.
		cur, _ := h.Store.GetDeployment(ctx, scopeA, delivery.DeploymentID(id))
		v := `"` + itoa(int64(cur.Version())) + `"`
		conforms(t, "cleanup", "request cleanup of a settled deployment", h.human("POST", "/deployments/"+id+"/cleanup", "", "If-Match", v))
		conforms(t, "cleanup", "a stale version records nothing", h.human("POST", "/deployments/"+id+"/cleanup", "", "If-Match", v))
		cleaner := h.cleaner(t)
		h.Bindings.Bind(cleaner.Scope, cleaner.Context.RunID, delivery.DeploymentID(id))
		conforms(t, "cleanup", "a cleanup run reports a visible exception", h.attempt(cleaner, "POST", "/deployments/"+id+"/cleanup-result",
			`{"state":"failed","reason":"delete_failed","deleted":`+objs(observed(intended)[:1])+`}`))
		conforms(t, "cleanup", "the retried cleanup completes", h.attempt(cleaner, "POST", "/deployments/"+id+"/cleanup-result", `{"state":"done","deleted":`+objs(observed(intended))+`}`))
		unbound := h.cleaner(t)
		conforms(t, "cleanup", "a run that is not bound to this deployment is denied", h.attempt(unbound, "POST", "/deployments/"+id+"/cleanup-result", `{"state":"done","deleted":`+objs(observed(intended))+`}`))
	})

	t.Run("failed, unknown and reconciled outcomes have the published structure", func(t *testing.T) {
		ns := deliverytest.TargetNamespace
		for _, variant := range []string{"failed", "unknown"} {
			h := newHTTPStack(t, deliverytest.Options{}, false)
			intended := objects(ns, "inventory", "inventory-worker")
			id := h.mustRegister(t).Deployment.ID()
			h.apply(t, id, intended)
			objs, _ := json.Marshal(deliveryObjects(observed(intended)[:1]))
			if variant == "failed" {
				conforms(t, "structured_outcomes", "failed with a reason code (alternative to the report above)",
					h.attempt(h.D.Deploy, "POST", "/deployments/"+string(id)+"/outcome", `{"state":"failed","reason":"rollout_timeout","objects":`+strings.Replace(string(objs), `"ready":true`, `"ready":false`, 1)+`}`))
				continue
			}
			conforms(t, "structured_outcomes", "unknown: the apply may have taken effect (alternative to the reports above)",
				h.attempt(h.D.Deploy, "POST", "/deployments/"+string(id)+"/outcome", `{"state":"unknown"}`))
			all, _ := json.Marshal(deliveryObjects(intended))
			conforms(t, "unknown_outcome_refusal", "apply again while the outcome is unknown",
				h.attempt(h.D.Deploy, "POST", "/deployments/"+string(id)+"/outcome", `{"state":"applying","objects":`+string(all)+`}`))
			ready, _ := json.Marshal(deliveryObjects(observed(intended)))
			conforms(t, "unknown_outcome_refusal", "settle by observation",
				h.attempt(h.D.Deploy, "POST", "/deployments/"+string(id)+"/outcome", `{"state":"succeeded","objects":`+string(ready)+`}`))
		}
	})

	t.Run("environments and a true stage promotion over HTTP", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		def := `{"name":"stage","kind":"stage","target":"kind","namespace":"t035-stage","kubeconfig":{"name":"KUBECONFIG","ref":"secret://project/kubeconfig","delivery":"file"},"config":{"REPLICAS":"3"}}`
		stage := h.human("POST", "/environments", def)
		if stage.Code != 201 || stage.Header().Get("Location") == "" || stage.Header().Get("ETag") != `"1"` {
			t.Fatalf("%d %s", stage.Code, stage.Body.String())
		}
		conforms(t, "stage_promotion", "define the stage environment", stage)
		if stage.json(t)["previewTtlSeconds"] != nil {
			t.Fatal("a stage environment has no preview TTL")
		}
		if dup := h.human("POST", "/environments", def); dup.Code != 409 || dup.code(t) != "name_taken" {
			t.Fatalf("a duplicate name: %d %s", dup.Code, dup.Body.String())
		}
		// A credential outside the project is refused; a viewer cannot define one.
		bad := strings.Replace(def, "secret://project/kubeconfig", "secret://project/not-mine", 1)
		bad = strings.Replace(bad, `"name":"stage"`, `"name":"other"`, 1)
		r := h.human("POST", "/environments", bad)
		if r.Code != 422 || r.code(t) != "credential_unavailable" {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		conforms(t, "pinned_configuration", "a credential reference outside the project is refused", r)
		if r := h.do(h.pub, "tok-viewer", "POST", apiBase+"/environments", strings.Replace(def, `"name":"stage"`, `"name":"viewer"`, 1)); r.Code != 403 {
			t.Fatalf("a viewer defined an environment: %d", r.Code)
		}
		list := h.human("GET", "/environments", "")
		if list.Code != 200 || len(list.json(t)["items"].([]any)) != 2 {
			t.Fatalf("list: %d %s", list.Code, list.Body.String())
		}
		cfg := h.human("PATCH", "/environments/"+string(h.D.Env.ID()), `{"config":{"REPLICAS":"3"}}`, "If-Match", `"1"`)
		conforms(t, "pinned_configuration", "edit it under If-Match", cfg)

		// Promote the verified image to the stage environment as a separate run.
		stageEnv := stage.json(t)["id"].(string)
		env, err := h.Svc.GetEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, domain.EnvironmentID(stageEnv))
		if err != nil {
			t.Fatal(err)
		}
		built := h.Promote(h.D, env)
		first := h.mustRegister(t)
		body, _ := json.Marshal(map[string]any{
			"attemptId": string(built.Deploy.ID()),
			"source":    map[string]string{"repositoryUrl": built.Source.RepositoryURL, "commit": built.Source.Commit},
			"image":     map[string]string{"artifactId": string(built.Image), "digest": string(built.ImageDigest)},
		})
		promo := h.human("POST", "/deployments", string(body), "Idempotency-Key", "promote-1")
		if promo.Code != 201 {
			t.Fatalf("%d %s", promo.Code, promo.Body.String())
		}
		d := promo.deployment(t)
		if _, has := d["expiresAt"]; has {
			t.Fatal("a stage deployment never expires")
		}
		if d["environmentId"] != stageEnv || d["namespace"] != "t035-stage" || d["id"] == string(first.Deployment.ID()) ||
			d["image"].(map[string]any)["digest"] != string(first.Deployment.Record().Image.Digest) ||
			d["producer"].(map[string]any)["attemptId"] != string(first.Deployment.Record().Producer.Attempt) {
			t.Fatalf("the promotion is not the same verified image on another environment: %v", d)
		}
		conforms(t, "stage_promotion", "register the promotion against the promoting attempt's pinned context", promo)
		// The record is retrievable and listable by environment and status.
		got := h.human("GET", "/deployments/"+d["id"].(string), "")
		if got.Code != 200 || got.Header().Get("ETag") != `"1"` {
			t.Fatalf("%d", got.Code)
		}
		one := h.human("GET", "/deployments?environmentId="+stageEnv+"&status=pending&limit=1", "")
		items := one.json(t)["items"].([]any)
		if one.Code != 200 || len(items) != 1 || one.json(t)["nextCursor"] != nil {
			t.Fatalf("filtered list: %d %s", one.Code, one.Body.String())
		}
		all := h.human("GET", "/deployments?limit=1", "")
		cursor, _ := all.json(t)["nextCursor"].(string)
		if cursor == "" {
			t.Fatalf("a second deployment must page: %s", all.Body.String())
		}
		if r := h.human("GET", "/deployments?limit=1&cursor="+cursor, ""); r.Code != 200 || len(r.json(t)["items"].([]any)) != 1 {
			t.Fatalf("the next page: %d", r.Code)
		}
		// A foreign tenant sees none of it and a cursor does not cross scopes.
		if r := h.do(h.pub, "tok-engineer", "GET", "/api/v1/tenants/t035_globex/projects/p035_ops/deployments?limit=1&cursor="+cursor, ""); r.Code == 200 {
			t.Fatalf("a cursor crossed tenants: %d", r.Code)
		}
	})

	t.Run("credential values appear in no response, stored response, audit row or event", func(t *testing.T) {
		h := newHTTPStack(t, deliverytest.Options{}, false)
		var bodies []string
		collect := func(r reply) { bodies = append(bodies, r.Body.String()+" "+headerText(r)) }
		reg := h.attempt(h.D.Deploy, "POST", "/deployments", h.registerBody())
		collect(reg)
		id := reg.deployment(t)["id"].(string)
		collect(h.human("POST", "/deployments", h.createBody(h.Req, ""), "Idempotency-Key", "k-disclose"))
		collect(h.human("GET", "/environments", ""))
		collect(h.human("GET", "/environments/"+string(h.D.Env.ID()), ""))
		collect(h.human("GET", "/deployments/"+id, ""))
		collect(h.human("GET", "/deployments", ""))
		collect(h.attempt(h.D.Deploy, "GET", "/deployments/by-action/"+h.D.Deploy.Context.ActionKey, ""))
		collect(h.attempt(h.D.Deploy, "POST", "/deployments/"+id+"/outcome", `{"state":"applying","objects":[{"apiVersion":"apps/v1","kind":"Deployment","namespace":"`+ns+`","name":"api"}]}`))
		collect(h.human("POST", "/environments", `{"name":"x","kind":"stage","target":"kind","namespace":"ns","kubeconfig":{"name":"KUBECONFIG","ref":"secret://project/nope","delivery":"file"},"config":{"A":"KUBE-SENTINEL"}}`)) // a client-sent sentinel is its own, not a stored value
		for i, b := range bodies[:len(bodies)-1] {
			if strings.Contains(b, "KUBE-SENTINEL") {
				t.Errorf("response %d discloses the credential value: %s", i, b)
			}
		}
		for _, q := range []string{
			`SELECT count(*) FROM idempotency WHERE response_body::text LIKE '%KUBE-SENTINEL%' OR response_headers::text LIKE '%KUBE-SENTINEL%'`,
			`SELECT count(*) FROM audit WHERE metadata::text LIKE '%KUBE-SENTINEL%'`,
			`SELECT count(*) FROM environments WHERE kubeconfig::text LIKE '%KUBE-SENTINEL%' OR secrets::text LIKE '%KUBE-SENTINEL%' OR config::text LIKE '%KUBE-SENTINEL%'`,
			`SELECT count(*) FROM environment_snapshots WHERE secrets::text LIKE '%KUBE-SENTINEL%'`,
			`SELECT count(*) FROM deployments d WHERE d::text LIKE '%KUBE-SENTINEL%'`,
			`SELECT count(*) FROM deployment_history WHERE detail::text LIKE '%KUBE-SENTINEL%'`,
		} {
			if n := h.count(q); n != 0 {
				t.Errorf("a stored row holds the credential value: %s", q)
			}
		}
		if n := h.X.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE payload::text LIKE '%KUBE-SENTINEL%'`); n != 0 {
			t.Errorf("an event carries the credential value")
		}
		// The pinned bindings are references and versions only.
		b, _ := json.Marshal(h.D.Deploy.Context.Secrets)
		if strings.Contains(string(b), "KUBE-SENTINEL") || !strings.Contains(string(b), "secret://") {
			t.Errorf("pinned bindings: %s", b)
		}
	})
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func headerText(r reply) string {
	var buf bytes.Buffer
	for k, v := range r.Header() {
		buf.WriteString(k + "=" + strings.Join(v, ",") + ";")
	}
	return buf.String()
}

// deliveryObjects renders object references as the wire representation.
func deliveryObjects(in []delivery.ObjectRef) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, o := range in {
		m := map[string]any{"apiVersion": o.APIVersion, "kind": o.Kind, "namespace": o.Namespace, "name": o.Name}
		if o.UID != "" {
			m["uid"] = o.UID
		}
		if o.Ready {
			m["ready"] = true
		}
		out = append(out, m)
	}
	return out
}

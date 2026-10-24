package deliveryhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

const (
	deliveryYAML = "../../../api/delivery.yaml"
	commonYAML   = "../../../api/common.yaml"
	testdataDir  = "../testdata"
)

var (
	scopeA  = domain.Scope{TenantID: "t035_acme", ProjectID: "p035_inventory"}
	scopeB  = domain.Scope{TenantID: "t035_globex", ProjectID: "p035_ops"}
	t0      = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	digestA = runtimev1.Digest("sha256:" + strings.Repeat("a", 64))
	digestB = runtimev1.Digest("sha256:" + strings.Repeat("b", 64))
)

// sentinel is a credential-looking value that must never appear in any response,
// stored body, error or log.
const sentinel = "KUBE-SENTINEL-s3cr3t-value"

// ---- fixtures of the domain ---------------------------------------------------

func newEnvironment(t testing.TB, id, name string) delivery.Environment {
	t.Helper()
	env, err := delivery.NewEnvironment(delivery.EnvironmentRecord{
		Scope: scopeA, ID: domain.EnvironmentID(id), Name: name, Kind: delivery.KindPreview, Target: "kind", Namespace: "t035-preview",
		Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: "secret://project/kubeconfig", Delivery: runtimev1.DeliveryFile},
		Secrets:    []delivery.Credential{{Name: "REGISTRY_TOKEN", Ref: "secret://project/registry-token", Delivery: runtimev1.DeliveryEnv}},
		Config:     map[string]string{"REPLICAS": "1"}, PreviewTTL: 24 * time.Hour,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func newDeployment(t testing.TB, actionKey string) delivery.Deployment {
	t.Helper()
	exp := t0.Add(24 * time.Hour)
	d, err := delivery.NewDeployment(delivery.Registration{
		Scope: scopeA, Environment: "env_t035_1", ConfigDigest: digestB, Namespace: "t035-preview",
		Source:   delivery.Source{RepositoryURL: "https://git.t035.invalid/acme/inventory.git", Commit: strings.Repeat("1", 40)},
		Image:    delivery.Image{ArtifactID: "art_t035_image", Digest: digestA},
		Producer: delivery.Producer{Attempt: "att_t035_build", Run: "run_t035_build", Step: "build", ToolImage: "registry.t035.invalid/tools/build@sha256:" + strings.Repeat("c", 64)},
		Run:      "run_t035_deploy", Step: "deploy", ActionKey: actionKey, Attempt: "att_t035_deploy", ExpiresAt: &exp,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// ---- the OpenAPI document as a validator --------------------------------------

type spec struct {
	raw map[string]any
	c   *jsonschema.Compiler
}

func loadYAML(t testing.TB, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// translate rewrites OpenAPI 3.0 nullability into JSON Schema.
func translate(n any) {
	switch v := n.(type) {
	case map[string]any:
		if nb, _ := v["nullable"].(bool); nb {
			if ty, ok := v["type"].(string); ok {
				v["type"] = []any{ty, "null"}
			}
		}
		delete(v, "nullable")
		for _, c := range v {
			translate(c)
		}
	case []any:
		for _, c := range v {
			translate(c)
		}
	}
}

func loadSpec(t testing.TB) *spec {
	t.Helper()
	c := jsonschema.NewCompiler()
	var out map[string]any
	for name, path := range map[string]string{"common.yaml": commonYAML, "delivery.yaml": deliveryYAML} {
		raw := loadYAML(t, path)
		translate(raw)
		js, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(name, doc); err != nil {
			t.Fatal(err)
		}
		if name == "delivery.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) schema(t testing.TB, name string) *jsonschema.Schema {
	t.Helper()
	sch, err := s.c.Compile("delivery.yaml#/components/schemas/" + name)
	if err != nil {
		t.Fatalf("schema %s: %v", name, err)
	}
	return sch
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return s.schema(t, schema).Validate(v)
}

// operation is one documented operation of the spec.
type operation struct {
	id, method, path, base string
	raw                    map[string]any
}

// fullPath is the path under the server URL the operation is served at.
func (o operation) fullPath() string { return o.base + o.path }

// operations lists every operation with the server URL of its path item.
func (s *spec) operations(t testing.TB) []operation {
	t.Helper()
	servers := s.raw["servers"].([]any)
	topBase := servers[0].(map[string]any)["url"].(string)
	var out []operation
	for path, item := range s.raw["paths"].(map[string]any) {
		pi := item.(map[string]any)
		base := topBase
		if sv, ok := pi["servers"].([]any); ok {
			base = sv[0].(map[string]any)["url"].(string)
		}
		for method, op := range pi {
			if !map[string]bool{"get": true, "post": true, "patch": true, "put": true, "delete": true}[method] {
				continue
			}
			m := op.(map[string]any)
			out = append(out, operation{id: m["operationId"].(string), method: strings.ToUpper(method), path: path, base: base, raw: m})
		}
	}
	return out
}

func (s *spec) operation(t testing.TB, id string) operation {
	t.Helper()
	for _, o := range s.operations(t) {
		if o.id == id {
			return o
		}
	}
	t.Fatalf("operation %s is not in api/delivery.yaml", id)
	return operation{}
}

var wildcard = regexp.MustCompile(`\{[^}]+\}`)

// matches reports whether a concrete path is the operation's.
func (o operation) matches(path string) bool {
	parts := wildcard.Split(o.fullPath(), -1)
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return regexp.MustCompile("^" + strings.Join(parts, "[^/]+") + "$").MatchString(path)
}

// pointerSchema compiles the schema at a JSON pointer inside delivery.yaml.
func (s *spec) pointerSchema(t testing.TB, ptr string) *jsonschema.Schema {
	t.Helper()
	sch, err := s.c.Compile("delivery.yaml#" + ptr)
	if err != nil {
		t.Fatalf("schema at %s: %v", ptr, err)
	}
	return sch
}

func escape(seg string) string {
	return strings.ReplaceAll(strings.ReplaceAll(seg, "~", "~0"), "/", "~1")
}

// requestSchema returns the schema of the operation's JSON request body, or nil.
func (s *spec) requestSchema(t testing.TB, o operation) *jsonschema.Schema {
	t.Helper()
	rb, ok := o.raw["requestBody"].(map[string]any)
	if !ok {
		return nil
	}
	if _, ok := rb["content"].(map[string]any)["application/json"]; !ok {
		return nil
	}
	return s.pointerSchema(t, "/paths/"+escape(o.path)+"/"+strings.ToLower(o.method)+"/requestBody/content/application~1json/schema")
}

// response returns the documented response of a status: its inline body schema
// (nil for a shared or bodiless response) and its inline header names. It fails
// for an undocumented status.
func (s *spec) response(t testing.TB, o operation, status string) (*jsonschema.Schema, map[string]bool) {
	t.Helper()
	resp, ok := o.raw["responses"].(map[string]any)[status].(map[string]any)
	if !ok {
		t.Fatalf("%s does not document status %s", o.id, status)
	}
	headers := map[string]bool{}
	declared, _ := resp["headers"].(map[string]any)
	for h := range declared {
		headers[http.CanonicalHeaderKey(h)] = true
	}
	content, ok := resp["content"].(map[string]any)
	if !ok {
		return nil, headers
	}
	if _, ok := content["application/json"]; !ok {
		return nil, headers
	}
	return s.pointerSchema(t, "/paths/"+escape(o.path)+"/"+strings.ToLower(o.method)+"/responses/"+status+"/content/application~1json/schema"), headers
}

// documents reports whether the operation documents the status.
func (o operation) documents(status string) bool {
	_, ok := o.raw["responses"].(map[string]any)[status]
	return ok
}

// errorEnvelope compiles the shared error envelope.
func (s *spec) errorEnvelope(t testing.TB) *jsonschema.Schema {
	t.Helper()
	sch, err := s.c.Compile("common.yaml#/components/schemas/ErrorEnvelope")
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func (s *spec) example(t testing.TB, name string) []byte {
	t.Helper()
	ex := s.raw["components"].(map[string]any)["examples"].(map[string]any)[name].(map[string]any)["value"]
	b, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (s *spec) exampleNames() []string {
	var out []string
	for k := range s.raw["components"].(map[string]any)["examples"].(map[string]any) {
		out = append(out, k)
	}
	return out
}

// ---- fakes of the use-case ports -----------------------------------------------

// fakeService implements both the public and the runtime ports. Every call is
// recorded; a configured error is returned by every method. It proves the
// transport's behavior only: no business rule lives here.
type fakeService struct {
	mu    sync.Mutex
	calls []string
	err   error

	env  delivery.Environment
	envs []delivery.Environment
	dep  delivery.Deployment
	deps []delivery.Deployment

	// afterName, afterID, limit and filter capture the last list call.
	afterName string
	afterID   delivery.DeploymentID
	limit     int
	filter    delivery.DeploymentFilter
	expected  domain.Version
	bearer    string
	attempt   domain.AttemptID
	reg       delivery.RegistrationRequest
	outcome   delivery.OutcomeReport
	cleanup   delivery.CleanupReport
	editedEnv delivery.EnvironmentEdit
	input     delivery.EnvironmentInput
	caller    delivery.Caller
}

func (f *fakeService) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeService) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeService) CreateEnvironment(_ context.Context, c delivery.Caller, _ domain.Scope, in delivery.EnvironmentInput) (delivery.Environment, error) {
	f.record("CreateEnvironment")
	f.caller, f.input = c, in
	return f.env, f.err
}

func (f *fakeService) GetEnvironment(_ context.Context, c delivery.Caller, _ domain.Scope, _ domain.EnvironmentID) (delivery.Environment, error) {
	f.record("GetEnvironment")
	f.caller = c
	return f.env, f.err
}

func (f *fakeService) ListEnvironments(_ context.Context, _ delivery.Caller, _ domain.Scope, after string, limit int) ([]delivery.Environment, error) {
	f.record("ListEnvironments")
	f.afterName, f.limit = after, limit
	return f.envs, f.err
}

func (f *fakeService) UpdateEnvironment(_ context.Context, _ delivery.Caller, _ domain.Scope, _ domain.EnvironmentID, expected domain.Version, edit delivery.EnvironmentEdit) (delivery.Environment, error) {
	f.record("UpdateEnvironment")
	f.expected, f.editedEnv = expected, edit
	return f.env, f.err
}

func (f *fakeService) GetDeployment(_ context.Context, _ delivery.Caller, _ domain.Scope, _ delivery.DeploymentID) (delivery.Deployment, error) {
	f.record("GetDeployment")
	return f.dep, f.err
}

func (f *fakeService) ListDeployments(_ context.Context, _ delivery.Caller, _ domain.Scope, flt delivery.DeploymentFilter, after delivery.DeploymentID, limit int) ([]delivery.Deployment, error) {
	f.record("ListDeployments")
	f.filter, f.afterID, f.limit = flt, after, limit
	return f.deps, f.err
}

func (f *fakeService) RequestCleanupAt(_ context.Context, _ delivery.Caller, _ domain.Scope, _ delivery.DeploymentID, expected domain.Version) (delivery.Receipt, error) {
	f.record("RequestCleanupAt")
	f.expected = expected
	return delivery.Receipt{Deployment: f.dep}, f.err
}

func (f *fakeService) PlanRegistrationForAttempt(_ context.Context, c delivery.Caller, _ domain.Scope, attempt domain.AttemptID, req delivery.RegistrationRequest) (delivery.RegistrationPlan, error) {
	f.record("PlanRegistrationForAttempt")
	f.caller, f.attempt, f.reg = c, attempt, req
	return delivery.RegistrationPlan{}, f.err
}

func (f *fakeService) CompleteRegistration(context.Context, delivery.RegistrationPlan, delivery.RegisterResult, error) (delivery.Receipt, error) {
	f.record("CompleteRegistration")
	return delivery.Receipt{}, nil
}

func (f *fakeService) CompleteReplay(context.Context, delivery.RegistrationPlan) {
	f.record("CompleteReplay")
}

func (f *fakeService) Receipt(d delivery.Deployment) delivery.Receipt {
	return delivery.Receipt{Deployment: d, Annotations: d.Annotations(delivery.DefaultAnnotations)}
}

func (f *fakeService) RegisterRuntime(_ context.Context, attempt domain.AttemptID, bearer string, req delivery.RegistrationRequest) (delivery.Receipt, error) {
	f.record("RegisterRuntime")
	f.attempt, f.bearer, f.reg = attempt, bearer, req
	return delivery.Receipt{Deployment: f.dep, Annotations: f.dep.Annotations(delivery.DefaultAnnotations), Created: true}, f.err
}

func (f *fakeService) LookupByAction(_ context.Context, attempt domain.AttemptID, bearer, _ string) (delivery.Receipt, error) {
	f.record("LookupByAction")
	f.attempt, f.bearer = attempt, bearer
	return f.Receipt(f.dep), f.err
}

func (f *fakeService) GetRuntime(_ context.Context, attempt domain.AttemptID, bearer string, _ delivery.DeploymentID) (delivery.Receipt, error) {
	f.record("GetRuntime")
	f.attempt, f.bearer = attempt, bearer
	return f.Receipt(f.dep), f.err
}

func (f *fakeService) ReportOutcome(_ context.Context, attempt domain.AttemptID, bearer string, _ delivery.DeploymentID, rep delivery.OutcomeReport) (delivery.Receipt, error) {
	f.record("ReportOutcome")
	f.attempt, f.bearer, f.outcome = attempt, bearer, rep
	return f.Receipt(f.dep), f.err
}

func (f *fakeService) ReportCleanup(_ context.Context, attempt domain.AttemptID, bearer string, _ delivery.DeploymentID, rep delivery.CleanupReport) (delivery.Receipt, error) {
	f.record("ReportCleanup")
	f.attempt, f.bearer, f.cleanup = attempt, bearer, rep
	return f.Receipt(f.dep), f.err
}

// fakeRegistrar records keyed registrations and returns a configured outcome.
type fakeRegistrar struct {
	mu       sync.Mutex
	runs     int
	err      error
	replayed bool
	res      delivery.RegisterResult
	id       httpapi.RequestIdentity
	replay   func(context.Context, httpapi.Reference) error
}

func (r *fakeRegistrar) RegisterKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	_ delivery.RegisterCommand, render func(delivery.RegisterResult) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, delivery.RegisterResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs++
	r.id, r.replay = id, authorizeReplay
	if r.err != nil {
		return httpapi.StoredResponse{}, false, delivery.RegisterResult{}, r.err
	}
	out, err := render(r.res)
	if err != nil {
		return httpapi.StoredResponse{}, false, delivery.RegisterResult{}, err
	}
	if r.replayed {
		if err := authorizeReplay(ctx, out.Target); err != nil {
			return httpapi.StoredResponse{}, false, delivery.RegisterResult{}, err
		}
	}
	return out.Response, r.replayed, r.res, nil
}

// ---- the routers ----------------------------------------------------------------

type tokens map[string]string

func (t tokens) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	id, ok := t[tok]
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

// anyBearer accepts every request with an Authorization header as one runtime
// principal: the runtime handler must still check and forward the bearer.
type anyBearer struct{}

func (anyBearer) Authenticate(context.Context, *http.Request) (httpapi.Principal, error) {
	return httpapi.Principal{ID: "runtime"}, nil
}

// recordingAuthorizer allows everything and records each call, so a test can see
// the action, the target and how often the authority was asked.
type recordingAuthorizer struct {
	mu    sync.Mutex
	calls []authzCall
	deny  func(n int, a httpapi.Action, t httpapi.Target) error
}

type authzCall struct {
	action httpapi.Action
	target httpapi.Target
}

func (a *recordingAuthorizer) Authorize(_ context.Context, _ httpapi.Principal, action httpapi.Action, t httpapi.Target) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, authzCall{action, t})
	if a.deny != nil {
		return a.deny(len(a.calls), action, t)
	}
	return nil
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type api struct {
	t       testing.TB
	svc     *fakeService
	reg     *fakeRegistrar
	authz   *recordingAuthorizer
	public  *httpapi.Router
	runtime *httpapi.Router
	codec   *httpapi.CursorCodec
	logs    *logBuffer
}

func newAPI(t testing.TB) *api {
	t.Helper()
	a := &api{t: t, svc: &fakeService{}, reg: &fakeRegistrar{}, authz: &recordingAuthorizer{}, logs: &logBuffer{}}
	a.svc.env = newEnvironment(t, "env_t035_1", "preview")
	a.svc.dep = newDeployment(t, "run_t035_deploy:deploy")
	a.svc.envs = []delivery.Environment{a.svc.env}
	a.svc.deps = []delivery.Deployment{a.svc.dep}
	a.reg.res = delivery.RegisterResult{Deployment: a.svc.dep, Created: true}

	logger := slog.New(slog.NewTextHandler(a.logs, nil))
	var err error
	a.public, err = httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: tokens{"tok-engineer": "u_engineer", "tok-viewer": "u_viewer"}, Authorizer: a.authz, RateLimiter: httpapi.Unlimited(),
		Prefix: "/api/v1", Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.runtime, err = httpapi.NewRouter(httpapi.RouterConfig{Authenticator: anyBearer{}, Authorizer: a.authz, RateLimiter: httpapi.Unlimited(), Prefix: "/runtime/v1", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	a.codec, err = httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := deliveryhttp.NewHandler(a.svc, a.reg, a.codec, deliveryhttp.DefaultActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(a.public); err != nil {
		t.Fatal(err)
	}
	rh, err := deliveryhttp.NewRuntime(a.svc, deliveryhttp.DefaultRuntimeActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rh.RegisterRoutes(a.runtime); err != nil {
		t.Fatal(err)
	}
	return a
}

type resp struct{ *httptest.ResponseRecorder }

func (a *api) send(r *httpapi.Router, token, method, path, body string, headers ...string) resp {
	a.t.Helper()
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
		if strings.EqualFold(headers[i], "Content-Type") {
			req.Header.Set(headers[i], headers[i+1])
		} else {
			req.Header.Add(headers[i], headers[i+1]) // a repeated header stays repeated
		}
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return resp{rec}
}

func (a *api) human(method, path, body string, headers ...string) resp {
	a.t.Helper()
	return a.send(a.public, "tok-engineer", method, "/api/v1"+path, body, headers...)
}

func (a *api) rt(token, method, path, body string, headers ...string) resp {
	a.t.Helper()
	return a.send(a.runtime, token, method, "/runtime/v1"+path, body, headers...)
}

func (r resp) decode(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		t.Fatalf("response is not the expected JSON: %v: %s", err, r.Body.String())
	}
}

func (r resp) errEnvelope(t testing.TB) httpapi.Error {
	t.Helper()
	var e httpapi.ErrorEnvelope
	r.decode(t, &e)
	if e.Error.RequestId == "" || r.Header().Get("X-Request-ID") != e.Error.RequestId {
		t.Fatalf("error envelope without a matching request id: %s", r.Body.String())
	}
	return e.Error
}

func (r resp) errCode(t testing.TB) string { return r.errEnvelope(t).Code }

const (
	pubBase = "/tenants/t035_acme/projects/p035_inventory"
	depID   = "dep_0123456789abcdef0123456789abcdef"
)

package git_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	git "github.com/optimiware/kodama/internal/userland/git"
)

// fakeBackend is the deterministic smart-HTTP backend boundary. It records the
// validated service/repository/path/environment/headers and can inject a
// backend fault, a blocked read, or response headers that must not reach the
// client.
type fakeBackend struct {
	mu       sync.Mutex
	calls    []git.GitBackendRequest
	bodies   [][]byte
	readErrs []error
	header   http.Header
	status   int
	body     []byte
	err      error
	wait     chan struct{}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{status: http.StatusOK, body: []byte("backend-response"), header: http.Header{}}
}

func (backend *fakeBackend) Serve(ctx context.Context, request git.GitBackendRequest, body io.Reader, response git.GitResponseWriter) error {
	var data []byte
	var readErr error
	if body != nil {
		data, readErr = io.ReadAll(body)
	}
	backend.mu.Lock()
	backend.calls = append(backend.calls, request)
	backend.bodies = append(backend.bodies, data)
	backend.readErrs = append(backend.readErrs, readErr)
	wait := backend.wait
	header := backend.header.Clone()
	status := backend.status
	out := append([]byte(nil), backend.body...)
	fault := backend.err
	backend.mu.Unlock()

	if readErr != nil {
		return readErr
	}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fault != nil {
		return fault
	}
	for key, values := range header {
		for _, value := range values {
			response.Header().Add(key, value)
		}
	}
	if status != 0 {
		response.WriteHeader(status)
	}
	if len(out) > 0 {
		_, err := response.Write(out)
		return err
	}
	return nil
}

func (backend *fakeBackend) callCount() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return len(backend.calls)
}

func (backend *fakeBackend) lastCall(t *testing.T) git.GitBackendRequest {
	t.Helper()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.calls) == 0 {
		t.Fatal("backend was never called")
	}
	return backend.calls[len(backend.calls)-1]
}

func (backend *fakeBackend) lastBody() []byte {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.bodies) == 0 {
		return nil
	}
	return append([]byte(nil), backend.bodies[len(backend.bodies)-1]...)
}

func (backend *fakeBackend) lastReadErr() error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.readErrs) == 0 {
		return nil
	}
	return backend.readErrs[len(backend.readErrs)-1]
}

func (backend *fakeBackend) setResponse(status int, header http.Header, body []byte) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.status = status
	backend.header = header.Clone()
	backend.body = append([]byte(nil), body...)
}

func (backend *fakeBackend) fail(err error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.err = err
}

func (backend *fakeBackend) block() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.wait = make(chan struct{})
}

type smartFixture struct {
	*fixture
	actors    *fakeActorVerifier
	handles   *fakeActorHandles
	backend   *fakeBackend
	handler   *git.SmartHTTP
	resources map[string]string
	maxBody   int64
}

func newSmartFixture(t *testing.T) *smartFixture {
	return newSmartFixtureWith(t, 16, 0)
}

func newSmartFixtureWith(t *testing.T, maxBody int64, timeout time.Duration) *smartFixture {
	t.Helper()
	return newSmartFixtureFrom(t, newFixture(t), maxBody, timeout)
}

// classifiedActorVerifier gives the deterministic fake the fault class a real
// Shell verifier returns: a plain "unknown credential" error is
// fault.Unauthenticated, so the smart-HTTP boundary keeps 401 for a real
// credential failure. Injected fault errors pass through unchanged.
type classifiedActorVerifier struct {
	inner *fakeActorVerifier
}

func (verifier classifiedActorVerifier) VerifyReadActor(ctx context.Context, repository, credential string) (git.Identity, error) {
	identity, err := verifier.inner.VerifyReadActor(ctx, repository, credential)
	if err == nil {
		return identity, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return git.Identity{}, err
	}
	if _, ok := errors.AsType[*fault.Error](err); ok {
		return git.Identity{}, err
	}
	return git.Identity{}, fault.Wrap(fault.Unauthenticated, "the actor credential was refused", err)
}

// newTestSourceEvents returns a deterministic per-request push identity
// generator. Production composition uses NewSourceEventID (crypto/rand).
func newTestSourceEvents() func() (string, error) {
	var mutex sync.Mutex
	next := 0
	return func() (string, error) {
		mutex.Lock()
		defer mutex.Unlock()
		next++
		return fmt.Sprintf("push-%d", next), nil
	}
}

func newSmartFixtureFrom(t *testing.T, base *fixture, maxBody int64, timeout time.Duration) *smartFixture {
	t.Helper()
	actors := newFakeActorVerifier()
	actors.set("viewer-token", humanIdentity("viewer"))
	actors.set("engineer-token", humanIdentity("engineer"))
	actors.set("exec-token", executionIdentity())
	actors.set("svc-token", serviceIdentity("ci-pipeline"))
	actors.set("other-svc-token", serviceIdentity("other-svc"))
	actors.set("admin-token", humanIdentity("admin"))
	handles := newFakeActorHandles()
	backend := newFakeBackend()
	resources := map[string]string{
		"shop/web":     "res-web",
		"shop/deploy":  "res-deploy",
		"shop/guarded": "res-guarded",
		"admin":        "res-admin",
	}
	handler, err := git.NewSmartHTTP(git.SmartHTTPConfig{
		Service:      base.service,
		Backend:      backend,
		Actors:       classifiedActorVerifier{inner: actors},
		ActorHandles: handles,
		ResourceID: func(repository string) (string, error) {
			resource, ok := resources[repository]
			if !ok {
				return "", fmt.Errorf("no resource registered for %q", repository)
			}
			return resource, nil
		},
		SourceEvent:  newTestSourceEvents(),
		MaxBodyBytes: maxBody,
		Timeout:      timeout,
	})
	if err != nil {
		t.Fatalf("NewSmartHTTP: %v", err)
	}
	return &smartFixture{
		fixture:   base,
		actors:    actors,
		handles:   handles,
		backend:   backend,
		handler:   handler,
		resources: resources,
		maxBody:   maxBody,
	}
}

func (f *smartFixture) request(t *testing.T, method, target, credential string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if credential != "" {
		request.SetBasicAuth("kodama", credential)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func (f *smartFixture) bearerRequest(t *testing.T, target, credential string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func envMap(environment []string) map[string]string {
	values := map[string]string{}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func assertNoCredential(t *testing.T, values []string, credential string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(value, credential) {
			t.Fatalf("credential leaked through %q", value)
		}
	}
}

func TestSmartHTTPInfoRefsUploadPack(t *testing.T) {
	f := newSmartFixture(t)
	f.backend.setResponse(http.StatusOK, http.Header{
		"Content-Type": {"application/x-git-upload-pack-advertisement"},
	}, []byte("refs"))

	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "viewer-token", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", recorder.Code)
	}
	if f.backend.callCount() != 1 {
		t.Fatalf("backend calls = %d; want 1", f.backend.callCount())
	}
	call := f.backend.lastCall(t)
	if call.Service != "git-upload-pack" {
		t.Fatalf("backend service = %q", call.Service)
	}
	if call.Repository != "shop/web" {
		t.Fatalf("backend repository = %q", call.Repository)
	}
	if call.Path != f.repositoryPath("shop/web") {
		t.Fatalf("backend path = %q; want %q", call.Path, f.repositoryPath("shop/web"))
	}
	environment := envMap(call.Environment)
	if environment["KODAMA_REPOSITORY"] != "shop/web" || environment["KODAMA_RESOURCE_ID"] != "res-web" {
		t.Fatalf("backend environment = %+v", environment)
	}
	handle := environment["KODAMA_ACTOR_REF"]
	if handle == "" || handle == "viewer-token" {
		t.Fatalf("backend actor handle = %q; want an opaque handle", handle)
	}
	assertNoCredential(t, call.Environment, "viewer-token")
	if call.Headers.Get("Authorization") != "" {
		t.Fatal("backend received the Authorization header")
	}
	if recorder.Header().Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
		t.Fatalf("client content type = %q", recorder.Header().Get("Content-Type"))
	}
	if repositories := f.actors.verifiedRepositories(); len(repositories) != 1 || repositories[0] != "shop/web" {
		t.Fatalf("read verification repositories = %v; want [shop/web]", repositories)
	}
}

// TestSmartHTTPRepositoryLevelReadHonorsRefLessRule pins the rule at the
// smart-HTTP seam: a ref-scoped deny followed by a ref-less read rule permits
// the repository read, while an identity with no repository-wide rule is hidden.
func TestSmartHTTPRepositoryLevelReadHonorsRefLessRule(t *testing.T) {
	base := newFixtureWithConfig(t, "repo shop/web\n    - refs/heads/private = @viewer\n    R = @viewer\n")
	f := newSmartFixtureFrom(t, base, 16, 0)

	viewer := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "viewer-token", nil, nil)
	if viewer.Code != http.StatusOK {
		t.Fatalf("viewer status = %d; want 200", viewer.Code)
	}
	engineer := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "engineer-token", nil, nil)
	if engineer.Code != http.StatusNotFound {
		t.Fatalf("engineer status = %d; want 404", engineer.Code)
	}

	// A ref-prefix-only allow rule is an update rule, not a repository read
	// rule: it must not admit the smart-HTTP read advertisement.
	scoped := newSmartFixtureFrom(t, newFixtureWithConfig(t, "repo shop/web\n    RW refs/heads/ = @engineer\n"), 16, 0)
	scopedRead := scoped.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "engineer-token", nil, nil)
	if scopedRead.Code != http.StatusNotFound {
		t.Fatalf("ref-prefix-only allow status = %d; want 404", scopedRead.Code)
	}
}

// TestSmartHTTPWriteOnlyActorIsAdmittedForReceive pins the receive-pack choice:
// a write admission that succeeds is sufficient for the service advertisement;
// the exact per-ref update decision stays in the hook. A user who may write a
// ref but has no repository-wide read rule therefore gets 200 on receive-pack
// and 404 on upload-pack rather than a hidden or 403 refusal.
func TestSmartHTTPWriteOnlyActorIsAdmittedForReceive(t *testing.T) {
	base := newFixtureWithConfig(t, "repo shop/web\n    RW refs/heads/main = @engineer\n")
	f := newSmartFixtureFrom(t, base, 16, 0)

	receive := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-receive-pack", "engineer-token", nil, nil)
	if receive.Code != http.StatusOK {
		t.Fatalf("write-only receive status = %d; want 200 (body %q)", receive.Code, receive.Body.String())
	}
	read := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "engineer-token", nil, nil)
	if read.Code != http.StatusNotFound {
		t.Fatalf("write-only upload status = %d; want 404", read.Code)
	}
}

func TestSmartHTTPInfoRefsReceivePack(t *testing.T) {
	f := newSmartFixture(t)
	f.backend.setResponse(http.StatusOK, http.Header{
		"Content-Type": {"application/x-git-receive-pack-advertisement"},
	}, []byte("refs"))

	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-receive-pack", "exec-token", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", recorder.Code)
	}
	call := f.backend.lastCall(t)
	if call.Service != "git-receive-pack" {
		t.Fatalf("backend service = %q", call.Service)
	}
}

func TestSmartHTTPReceivePackPost(t *testing.T) {
	f := newSmartFixture(t)
	f.backend.setResponse(http.StatusOK, http.Header{
		"Content-Type": {"application/x-git-receive-pack-result"},
	}, []byte("result"))

	recorder := f.request(t, http.MethodPost, "/shop/web/git-receive-pack", "exec-token", []byte("commands"), map[string]string{
		"Content-Type": "application/x-git-receive-pack-request",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", recorder.Code)
	}
	call := f.backend.lastCall(t)
	if call.Service != "git-receive-pack" {
		t.Fatalf("backend service = %q", call.Service)
	}
	if body := f.backend.lastBody(); string(body) != "commands" {
		t.Fatalf("backend body = %q", body)
	}

	before := f.backend.callCount()
	recorder = f.request(t, http.MethodPost, "/shop/web/git-receive-pack", "viewer-token", []byte("commands"), map[string]string{
		"Content-Type": "application/x-git-receive-pack-request",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("read-only receive status = %d; want 403", recorder.Code)
	}
	if f.backend.callCount() != before {
		t.Fatal("read-only receive reached the backend")
	}
}

// TestSmartHTTPPolicyDependencyUnavailableIs503 pins the fail-closed smart-HTTP
// status for an unavailable committed policy: fault.DependencyUnavailable is
// 503 with the generic status body and no path or policy text, while the
// authorization refusals keep their 401/403/404 statuses.
func TestSmartHTTPPolicyDependencyUnavailableIs503(t *testing.T) {
	f := newSmartFixture(t)
	f.policies.err = fault.Wrap(fault.DependencyUnavailable,
		"the tenant Git policy is unavailable",
		fmt.Errorf("stat %q failed", f.root))
	before := f.backend.callCount()
	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "viewer-token", nil, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("dependency status = %d; want 503", recorder.Code)
	}
	if body := recorder.Body.String(); body != http.StatusText(http.StatusServiceUnavailable)+"\n" {
		t.Fatalf("body = %q; want the generic status text", body)
	}
	if body := recorder.Body.String(); strings.Contains(body, f.root) {
		t.Fatalf("body = %q; leaks the storage root", body)
	}
	if body := recorder.Body.String(); strings.Contains(body, "policy") {
		t.Fatalf("body = %q; leaks policy detail", body)
	}
	if f.backend.callCount() != before {
		t.Fatal("an unavailable policy reached the backend")
	}

	// The new dependency branch must not shadow the authorization mappings.
	f.policies.err = nil
	if recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "", nil, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d; want 401", recorder.Code)
	}
	if recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-receive-pack", "viewer-token", nil, nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("write-denied status = %d; want 403", recorder.Code)
	}
	if recorder := f.request(t, http.MethodGet, "/shop/missing/info/refs?service=git-upload-pack", "viewer-token", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown-repository status = %d; want 404", recorder.Code)
	}
}

func TestSmartHTTPAuthorizationMatrix(t *testing.T) {
	f := newSmartFixture(t)
	cases := []struct {
		name       string
		target     string
		credential string
		want       int
	}{
		{"anonymous upload", "/shop/web/info/refs?service=git-upload-pack", "", http.StatusUnauthorized},
		{"unknown credential", "/shop/web/info/refs?service=git-upload-pack", "unknown-token", http.StatusUnauthorized},
		{"viewer reads", "/shop/web/info/refs?service=git-upload-pack", "viewer-token", http.StatusOK},
		{"viewer cannot receive", "/shop/web/info/refs?service=git-receive-pack", "viewer-token", http.StatusForbidden},
		{"execution may receive", "/shop/web/info/refs?service=git-receive-pack", "exec-token", http.StatusOK},
		{"unknown repository", "/shop/missing/info/refs?service=git-upload-pack", "viewer-token", http.StatusNotFound},
		{"unauthorized repository", "/shop/deploy/info/refs?service=git-upload-pack", "engineer-token", http.StatusNotFound},
		{"pipeline service reads and writes its ref", "/shop/guarded/info/refs?service=git-receive-pack", "svc-token", http.StatusOK},
		{"other service is hidden", "/shop/guarded/info/refs?service=git-upload-pack", "other-svc-token", http.StatusNotFound},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			before := f.backend.callCount()
			recorder := f.request(t, http.MethodGet, testCase.target, testCase.credential, nil, nil)
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d; want %d (body %q)", recorder.Code, testCase.want, recorder.Body.String())
			}
			if testCase.want != http.StatusOK && f.backend.callCount() != before {
				t.Fatalf("refused request reached the backend")
			}
		})
	}
}

func TestSmartHTTPAnonymousChallenge(t *testing.T) {
	f := newSmartFixture(t)
	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "", nil, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", recorder.Code)
	}
	if !strings.Contains(recorder.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("WWW-Authenticate = %q", recorder.Header().Get("WWW-Authenticate"))
	}
	if f.backend.callCount() != 0 {
		t.Fatal("anonymous request reached the backend")
	}
}

func TestSmartHTTPBearerCredential(t *testing.T) {
	f := newSmartFixture(t)
	recorder := f.bearerRequest(t, "/shop/web/info/refs?service=git-upload-pack", "viewer-token")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", recorder.Code)
	}
}

func TestSmartHTTPWrongBasicUsername(t *testing.T) {
	f := newSmartFixture(t)
	request := httptest.NewRequest(http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", nil)
	request.SetBasicAuth("not-kodama", "viewer-token")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", recorder.Code)
	}
}

// TestSmartHTTPRejectsEmptyCredentials pins the extraction boundary: an empty
// or password-less Basic credential and an empty Bearer token are not
// credentials. They must be refused as anonymous before the verifier or the
// backend is reached, never treated as an authenticated actor.
func TestSmartHTTPRejectsEmptyCredentials(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{name: "empty bearer", header: "Bearer "},
		{name: "bearer without value", header: "Bearer"},
		{name: "empty basic", header: "Basic "},
		{name: "basic without value", header: "Basic"},
		{name: "empty basic password", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("kodama:"))},
		{name: "empty basic username and password", header: "Basic " + base64.StdEncoding.EncodeToString([]byte(":"))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newSmartFixture(t)
			request := httptest.NewRequest(http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", nil)
			request.Header.Set("Authorization", testCase.header)
			recorder := httptest.NewRecorder()
			f.handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401", recorder.Code)
			}
			if !strings.Contains(recorder.Header().Get("WWW-Authenticate"), "Basic") {
				t.Fatalf("WWW-Authenticate = %q; want a Basic challenge", recorder.Header().Get("WWW-Authenticate"))
			}
			if calls := f.actors.callCount(); calls != 0 {
				t.Fatalf("actor verifier calls = %d; an empty credential must not reach it", calls)
			}
			if calls := f.backend.callCount(); calls != 0 {
				t.Fatalf("backend calls = %d; an empty credential must not reach it", calls)
			}
		})
	}
}

func TestSmartHTTPUniformHiddenRepository(t *testing.T) {
	f := newSmartFixture(t)
	unknown := f.request(t, http.MethodGet, "/shop/missing/info/refs?service=git-upload-pack", "viewer-token", nil, nil)
	unauthorized := f.request(t, http.MethodGet, "/shop/deploy/info/refs?service=git-upload-pack", "engineer-token", nil, nil)
	if unknown.Code != http.StatusNotFound || unauthorized.Code != http.StatusNotFound {
		t.Fatalf("statuses = %d/%d; want 404/404", unknown.Code, unauthorized.Code)
	}
	if unknown.Body.String() != unauthorized.Body.String() {
		t.Fatalf("hidden repository responses differ: %q vs %q", unknown.Body.String(), unauthorized.Body.String())
	}
}

func TestSmartHTTPRejectsTraversalAndEncodedSlash(t *testing.T) {
	f := newSmartFixture(t)
	targets := []string{
		"/../info/refs?service=git-upload-pack",
		"/shop/../web/info/refs?service=git-upload-pack",
		"/shop/./web/info/refs?service=git-upload-pack",
		"/shop%2Fweb/info/refs?service=git-upload-pack",
		"/shop%2fweb/info/refs?service=git-upload-pack",
		"/shop%5Cweb/info/refs?service=git-upload-pack",
		"/SHOP/web/info/refs?service=git-upload-pack",
		"/shop/web//info/refs?service=git-upload-pack",
		"/shop/web/info/refs/extra?service=git-upload-pack",
		"/" + strings.Repeat("a", 129) + "/info/refs?service=git-upload-pack",
	}
	for _, target := range targets {
		before := f.backend.callCount()
		recorder := f.request(t, http.MethodGet, target, "viewer-token", nil, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d; want 400", target, recorder.Code)
		}
		if f.backend.callCount() != before {
			t.Errorf("%s: malformed request reached the backend", target)
		}
	}
}

func TestSmartHTTPAllRefsDenyHidesRepository(t *testing.T) {
	base := newFixtureWithConfig(t, "repo shop/web\n    - refs/ = @all\n    RW = @engineer\n")
	f := newSmartFixtureFrom(t, base, 16, 0)
	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "engineer-token", nil, nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 for an all-refs deny", recorder.Code)
	}
	if f.backend.callCount() != 0 {
		t.Fatal("all-refs-denied request reached the backend")
	}
}

func TestSmartHTTPSingleSegmentRepository(t *testing.T) {
	f := newSmartFixture(t)
	recorder := f.request(t, http.MethodGet, "/admin/info/refs?service=git-upload-pack", "admin-token", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("info/refs status = %d; want 200", recorder.Code)
	}
	recorder = f.request(t, http.MethodPost, "/admin/git-upload-pack", "admin-token", []byte("body"), map[string]string{
		"Content-Type": "application/x-git-upload-pack-request",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload-pack status = %d; want 200", recorder.Code)
	}
	if call := f.backend.lastCall(t); call.Repository != "admin" {
		t.Fatalf("backend repository = %q; want admin", call.Repository)
	}
}

func TestSmartHTTPAcceptsDotGitSuffix(t *testing.T) {
	f := newSmartFixture(t)
	recorder := f.request(t, http.MethodGet, "/shop/web.git/info/refs?service=git-upload-pack", "viewer-token", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", recorder.Code)
	}
	if call := f.backend.lastCall(t); call.Repository != "shop/web" {
		t.Fatalf("backend repository = %q; want shop/web", call.Repository)
	}
}

func TestSmartHTTPRejectsWrongRouteAndContentType(t *testing.T) {
	f := newSmartFixture(t)
	cases := []struct {
		name   string
		method string
		target string
		header map[string]string
	}{
		{"post info refs", http.MethodPost, "/shop/web/info/refs?service=git-upload-pack", map[string]string{"Content-Type": "application/x-git-upload-pack-request"}},
		{"get upload pack", http.MethodGet, "/shop/web/git-upload-pack", nil},
		{"info refs without service", http.MethodGet, "/shop/web/info/refs", nil},
		{"info refs unknown service", http.MethodGet, "/shop/web/info/refs?service=bogus", nil},
		{"post wrong content type", http.MethodPost, "/shop/web/git-upload-pack", map[string]string{"Content-Type": "text/plain"}},
		{"post missing content type", http.MethodPost, "/shop/web/git-upload-pack", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := f.request(t, testCase.method, testCase.target, "viewer-token", []byte("body"), testCase.header)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400", recorder.Code)
			}
		})
	}
}

func TestSmartHTTPBodySizeBound(t *testing.T) {
	f := newSmartFixtureWith(t, 16, 0)
	contentType := map[string]string{"Content-Type": "application/x-git-upload-pack-request"}

	exact := bytes.Repeat([]byte("a"), 16)
	recorder := f.request(t, http.MethodPost, "/shop/web/git-upload-pack", "viewer-token", exact, contentType)
	if recorder.Code != http.StatusOK {
		t.Fatalf("exact-limit status = %d; want 200", recorder.Code)
	}
	if body := f.backend.lastBody(); len(body) != 16 {
		t.Fatalf("backend body = %d bytes; want 16", len(body))
	}

	before := f.backend.callCount()
	over := bytes.Repeat([]byte("a"), 17)
	recorder = f.request(t, http.MethodPost, "/shop/web/git-upload-pack", "viewer-token", over, contentType)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit status = %d; want 413", recorder.Code)
	}
	if f.backend.callCount() != before {
		t.Fatal("declared over-limit body reached the backend")
	}

	request := httptest.NewRequest(http.MethodPost, "/shop/web/git-upload-pack", bytes.NewReader(over))
	request.ContentLength = -1
	request.SetBasicAuth("kodama", "viewer-token")
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	recorder = httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streamed over-limit status = %d; want 413", recorder.Code)
	}
	if !errors.Is(f.backend.lastReadErr(), git.ErrBodyTooLarge) {
		t.Fatalf("backend read error = %v; want ErrBodyTooLarge", f.backend.lastReadErr())
	}
}

func TestSmartHTTPHeaderAllowList(t *testing.T) {
	f := newSmartFixture(t)
	f.backend.setResponse(http.StatusOK, http.Header{
		"Content-Type":  {"application/x-git-upload-pack-result"},
		"Cache-Control": {"no-cache"},
		"Authorization": {"Bearer leaked"},
		"X-Internal":    {"leaked"},
		"Set-Cookie":    {"session=leaked"},
	}, []byte("ok"))

	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "viewer-token", nil, map[string]string{
		"Git-Protocol": "version=2",
		"X-Secret":     "do-not-forward",
		"Cookie":       "session=do-not-forward",
	})
	call := f.backend.lastCall(t)
	if call.Headers.Get("Git-Protocol") != "version=2" {
		t.Fatalf("Git-Protocol not forwarded: %q", call.Headers.Get("Git-Protocol"))
	}
	for _, header := range []string{"Authorization", "X-Secret", "Cookie"} {
		if call.Headers.Get(header) != "" {
			t.Fatalf("backend received %s", header)
		}
	}
	if recorder.Header().Get("Content-Type") != "application/x-git-upload-pack-result" {
		t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
	}
	if recorder.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("cache control = %q", recorder.Header().Get("Cache-Control"))
	}
	for _, header := range []string{"Authorization", "X-Internal", "Set-Cookie"} {
		if recorder.Header().Get(header) != "" {
			t.Fatalf("client received sanitized header %s", header)
		}
	}
}

func TestSmartHTTPBackendFailureIsSanitized(t *testing.T) {
	f := newSmartFixture(t)
	f.backend.fail(errors.New("backend exploded: password=hunter2"))
	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "viewer-token", nil, nil)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", recorder.Code)
	}
	if body := recorder.Body.String(); strings.Contains(body, "exploded") || strings.Contains(body, "hunter2") {
		t.Fatalf("backend internals leaked: %q", body)
	}
}

func TestSmartHTTPCancellation(t *testing.T) {
	f := newSmartFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", nil)
	request = request.WithContext(ctx)
	request.SetBasicAuth("kodama", "viewer-token")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d; want 408", recorder.Code)
	}
	if f.backend.callCount() != 0 {
		t.Fatal("canceled request reached the backend")
	}
}

func TestSmartHTTPTimeout(t *testing.T) {
	f := newSmartFixtureWith(t, 16, 5*time.Millisecond)
	f.backend.block()
	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", "viewer-token", nil, nil)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d; want 504", recorder.Code)
	}
}

func TestSmartHTTPReceiveEnvironmentFeedsHook(t *testing.T) {
	f := newSmartFixture(t)
	f.verifier.script("exec-token", executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, hookBase)
	f.executor.setAncestor("shop/web", hookBase, hookCandidate, true)

	recorder := f.request(t, http.MethodGet, "/shop/web/info/refs?service=git-receive-pack", "exec-token", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", recorder.Code)
	}
	environment := envMap(f.backend.lastCall(t).Environment)
	hook, err := git.NewHook(git.HookConfig{
		Service: f.service,
		Actors:  f.handles,
		Environment: func(key string) (string, bool) {
			value, ok := environment[key]
			return value, ok
		},
	})
	if err != nil {
		t.Fatalf("NewHook: %v", err)
	}
	result, err := hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
	if err != nil {
		t.Fatalf("RunUpdate through the HTTP environment: %v", err)
	}
	if !result.Accepted() {
		t.Fatalf("hook result = %+v; want accept", result)
	}
	if got := f.executor.commit("shop/web", testRef); got != hookCandidate {
		t.Fatalf("ref = %q; want %q", got, hookCandidate)
	}
}

type credentialHandleIssuer struct{}

func (credentialHandleIssuer) IssueActor(_ context.Context, credential string) (string, error) {
	return credential, nil
}

func TestSmartHTTPRejectsCredentialAsHandle(t *testing.T) {
	f := newSmartFixture(t)
	handler, err := git.NewSmartHTTP(git.SmartHTTPConfig{
		Service:      f.service,
		Backend:      f.backend,
		Actors:       f.actors,
		ActorHandles: credentialHandleIssuer{},
		ResourceID: func(repository string) (string, error) {
			return f.resources[repository], nil
		},
		SourceEvent: newTestSourceEvents(),
	})
	if err != nil {
		t.Fatalf("NewSmartHTTP: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", nil)
	request.SetBasicAuth("kodama", "viewer-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", recorder.Code)
	}
	if f.backend.callCount() != 0 {
		t.Fatal("credential-bearing handle reached the backend")
	}
}

func TestNewSmartHTTPRequiresBoundaries(t *testing.T) {
	if _, err := git.NewSmartHTTP(git.SmartHTTPConfig{}); err == nil {
		t.Fatal("NewSmartHTTP accepted an empty config")
	}
}

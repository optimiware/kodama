package deploymenttools_test

// The factory callback boundary. The deployment record service and its stores
// belong to T-035 (and are qualified against real PostgreSQL by T-067); this
// task qualifies the ordinary scripts. The scripts therefore talk to the REAL
// runtime handlers of internal/delivery/http (route table, DTOs, error mapping,
// ETags) mounted on the real router, over TLS, backed by a fixture service that
// holds the REAL delivery.Deployment aggregate in memory. State transitions,
// idempotency by action key, outcome_unknown refusal, owned-object checks and the
// receipt/annotation shapes are therefore the shipped ones; only persistence,
// attempt authentication and the pinned-context reader are fixtures. The fixture
// is also where the tests inject request loss and response loss.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// call is one request the fixture saw, in arrival order.
type call struct {
	Method, Path string
	Body         string
	Status       int
	Dropped      string // "", "request" or "response"
}

type artifact struct {
	Source delivery.Source
	Digest runtimev1.Digest
}

// fault drops a request before it is processed ("request") or after the state
// change was made but before any byte is returned ("response"): the connection
// is closed, which is exactly what a lost acknowledgment looks like to curl.
type fault struct {
	kind  string
	match func(method, path string, body []byte) bool
	left  int // negative: every time
}

type callbacks struct {
	t     testing.TB
	mu    sync.Mutex
	scope domain.Scope
	ns    string
	env   domain.EnvironmentID
	cfg   runtimev1.Digest

	tokens       map[string]domain.AttemptID
	actionKeys   map[domain.AttemptID]string
	cleanupBound map[domain.AttemptID]delivery.DeploymentID
	artifacts    map[domain.ArtifactID]artifact
	byKey        map[string]*delivery.Deployment
	calls        []call
	faults       []*fault
	// lie makes registration return the stored record whatever was requested: a
	// defective record interface, to prove the script cross-checks provenance.
	lie bool
	// onApplying runs inside the handler before the intended objects are
	// recorded, so a test can look at the cluster at exactly that moment.
	onApplying func()

	srv    *httptest.Server
	caFile string
	url    string
	now    time.Time
}

func newCallbacks(t testing.TB, dir, namespace string) *callbacks {
	t.Helper()
	c := &callbacks{
		t:            t,
		scope:        domain.Scope{TenantID: "t065_acme", ProjectID: "p065_inventory"},
		ns:           namespace,
		env:          "env_t065_preview",
		cfg:          runtimev1.Digest("sha256:" + strings.Repeat("b", 64)),
		tokens:       map[string]domain.AttemptID{},
		actionKeys:   map[domain.AttemptID]string{},
		cleanupBound: map[domain.AttemptID]delivery.DeploymentID{},
		artifacts:    map[domain.ArtifactID]artifact{},
		byKey:        map[string]*delivery.Deployment{},
		now:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	svc := &fixtureService{c: c}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: bearerPresent{}, Authorizer: allowAll{}, RateLimiter: httpapi.Unlimited(), Prefix: "/runtime/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	rh, err := deliveryhttp.NewRuntime(svc, deliveryhttp.DefaultRuntimeActions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rh.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c.serve(router, w, r) }))
	t.Cleanup(c.srv.Close)
	c.url = c.srv.URL
	c.caFile = filepath.Join(dir, "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.srv.Certificate().Raw})
	if err := os.WriteFile(c.caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

type bearerPresent struct{}

func (bearerPresent) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: "runtime"}, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

func (c *callbacks) serve(router http.Handler, w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	entry := call{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/runtime/v1"), Body: string(body)}
	var kind string
	c.mu.Lock()
	for _, f := range c.faults {
		if f.left != 0 && f.match(r.Method, r.URL.Path, body) {
			kind = f.kind
			if f.left > 0 {
				f.left--
			}
			break
		}
	}
	c.mu.Unlock()

	if kind == "request" {
		entry.Dropped = "request"
		c.record(entry)
		c.hangUp(w)
		return
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	entry.Status = rec.Code
	if kind == "response" {
		entry.Dropped = "response"
		c.record(entry)
		c.hangUp(w)
		return
	}
	c.record(entry)
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func (c *callbacks) hangUp(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		c.t.Error("the fixture server cannot hang up")
		return
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func (c *callbacks) record(e call) {
	c.mu.Lock()
	c.calls = append(c.calls, e)
	c.mu.Unlock()
}

// ---- test controls -------------------------------------------------------------

// dropResponses loses the response of every matching request, after processing.
func (c *callbacks) dropResponses(match func(method, path string, body []byte) bool) {
	c.mu.Lock()
	c.faults = append(c.faults, &fault{kind: "response", match: match, left: -1})
	c.mu.Unlock()
}

// dropRequests loses every matching request before it is processed.
func (c *callbacks) dropRequests(match func(method, path string, body []byte) bool) {
	c.mu.Lock()
	c.faults = append(c.faults, &fault{kind: "request", match: match, left: -1})
	c.mu.Unlock()
}

// dropResponseOnce loses the response of the first matching request only.
func (c *callbacks) dropResponseOnce(match func(method, path string, body []byte) bool) {
	c.mu.Lock()
	c.faults = append(c.faults, &fault{kind: "response", match: match, left: 1})
	c.mu.Unlock()
}

func (c *callbacks) clearFaults() {
	c.mu.Lock()
	c.faults = nil
	c.mu.Unlock()
}

func outcomeState(state string) func(method, path string, body []byte) bool {
	return func(method, path string, body []byte) bool {
		if method != http.MethodPost || !strings.HasSuffix(path, "/outcome") {
			return false
		}
		var b struct {
			State string `json:"state"`
		}
		return json.Unmarshal(body, &b) == nil && b.State == state
	}
}

// attempt issues an attempt identity pinned to one logical action and returns the
// attempt id and its bearer token (what the entrypoint would have written to
// /kodama/identity/token).
func (c *callbacks) attempt(actionKey string) (domain.AttemptID, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := domain.AttemptID("att_t065_" + randHex(6))
	tok := "tok-" + randHex(16)
	c.tokens[tok] = id
	c.actionKeys[id] = actionKey
	return id, tok
}

// cleanupAttempt issues an attempt of an internal cleanup run bound to exactly one
// deployment (the binding the real service reads through delivery.CleanupBindings).
func (c *callbacks) cleanupAttempt(dep delivery.DeploymentID) (domain.AttemptID, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := domain.AttemptID("att_t065_clean_" + randHex(6))
	tok := "tok-" + randHex(16)
	c.tokens[tok] = id
	c.cleanupBound[id] = dep
	return id, tok
}

// produce registers an image the build step produced from a source (the verified
// provenance the real service reads from artifacts and executions).
func (c *callbacks) produce(id domain.ArtifactID, repo, commit string, digest runtimev1.Digest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.artifacts[id] = artifact{Source: delivery.Source{RepositoryURL: repo, Commit: commit}, Digest: digest}
}

func (c *callbacks) deployment(actionKey string) (delivery.DeploymentRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.byKey[actionKey]
	if !ok {
		return delivery.DeploymentRecord{}, false
	}
	return d.Record(), true
}

func (c *callbacks) mustDeployment(actionKey string) delivery.DeploymentRecord {
	c.t.Helper()
	d, ok := c.deployment(actionKey)
	if !ok {
		c.t.Fatalf("no deployment is recorded for %s", actionKey)
	}
	return d
}

func (c *callbacks) requestCleanup(actionKey string) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.byKey[actionKey]
	if d == nil {
		c.t.Fatalf("no deployment for %s", actionKey)
	}
	if _, err := d.RequestCleanup(c.now); err != nil {
		c.t.Fatal(err)
	}
}

func (c *callbacks) markUnknown(actionKey string) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.byKey[actionKey]
	if d == nil {
		c.t.Fatalf("no deployment for %s", actionKey)
	}
	if _, err := d.MarkOutcomeUnknown(c.now); err != nil {
		c.t.Fatal(err)
	}
}

func (c *callbacks) callLog() []call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]call(nil), c.calls...)
}

func (c *callbacks) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// since returns the calls recorded after mark.
func (c *callbacks) since(mark int) []call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]call(nil), c.calls[mark:]...)
}

func countCalls(cs []call, method, suffix string) int {
	n := 0
	for _, e := range cs {
		if e.Method == method && strings.HasSuffix(e.Path, suffix) {
			n++
		}
	}
	return n
}

func outcomeStates(cs []call) []string {
	var out []string
	for _, e := range cs {
		if e.Method != http.MethodPost || !strings.HasSuffix(e.Path, "/outcome") {
			continue
		}
		var b struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal([]byte(e.Body), &b)
		s := b.State
		if e.Dropped != "" {
			s += "(lost " + e.Dropped + ")"
		}
		out = append(out, s)
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ---- the fixture service (delivery.RuntimeService) ------------------------------

type fixtureService struct{ c *callbacks }

var _ deliveryhttp.RuntimeService = (*fixtureService)(nil)

func (s *fixtureService) authenticate(attempt domain.AttemptID, bearer string) error {
	if got, ok := s.c.tokens[bearer]; !ok || got != attempt {
		return delivery.Fail(delivery.ErrUnauthenticated, nil, "unauthenticated")
	}
	return nil
}

func (s *fixtureService) receipt(d *delivery.Deployment, created bool) delivery.Receipt {
	return delivery.Receipt{Deployment: *d, Annotations: d.Annotations(delivery.DefaultAnnotations), Created: created}
}

func (s *fixtureService) RegisterRuntime(_ context.Context, attempt domain.AttemptID, bearer string, req delivery.RegistrationRequest) (delivery.Receipt, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.authenticate(attempt, bearer); err != nil {
		return delivery.Receipt{}, err
	}
	key, ok := c.actionKeys[attempt]
	if !ok {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrPinRequired, nil, "no pinned context")
	}
	if err := req.Validate(); err != nil {
		return delivery.Receipt{}, err
	}
	if existing := c.byKey[key]; existing != nil {
		if c.lie {
			return s.receipt(existing, false), nil
		}
		reg, err := s.registration(attempt, key, req, existing.Record().Producer)
		if err != nil {
			return delivery.Receipt{}, err
		}
		fp, err := reg.Fingerprint()
		if err != nil {
			return delivery.Receipt{}, err
		}
		if fp != existing.Record().Fingerprint {
			return delivery.Receipt{}, delivery.Fail(delivery.ErrConflict, nil, "the logical action was registered with other inputs")
		}
		return s.receipt(existing, false), nil
	}
	art, ok := c.artifacts[req.Image.ArtifactID]
	if !ok || art.Digest != req.Image.Digest || art.Source != req.Source {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrProvenance, nil, "the image is not a verified output of this source")
	}
	producer := delivery.Producer{Attempt: "att_t065_build", Run: "run_t065_build", Step: "build",
		ToolImage: "registry.t065.invalid/tools/build@sha256:" + strings.Repeat("c", 64)}
	reg, err := s.registration(attempt, key, req, producer)
	if err != nil {
		return delivery.Receipt{}, err
	}
	d, err := delivery.NewDeployment(reg, c.now)
	if err != nil {
		return delivery.Receipt{}, err
	}
	c.byKey[key] = &d
	return s.receipt(&d, true), nil
}

func (s *fixtureService) registration(attempt domain.AttemptID, key string, req delivery.RegistrationRequest, producer delivery.Producer) (delivery.Registration, error) {
	c := s.c
	run, step, ok := strings.Cut(key, ":")
	if !ok {
		return delivery.Registration{}, delivery.Fail(delivery.ErrPinRequired, nil, "bad action key")
	}
	exp := c.now.Add(24 * time.Hour)
	return delivery.Registration{
		Scope: c.scope, Environment: c.env, ConfigDigest: c.cfg, Namespace: c.ns,
		Source: req.Source, Image: req.Image, Producer: producer,
		Run: domain.RunID(run), Step: step, ActionKey: key, Attempt: attempt, ExpiresAt: &exp,
	}, nil
}

func (s *fixtureService) LookupByAction(_ context.Context, attempt domain.AttemptID, bearer, actionKey string) (delivery.Receipt, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.authenticate(attempt, bearer); err != nil {
		return delivery.Receipt{}, err
	}
	if pinned, ok := c.actionKeys[attempt]; !ok || pinned != actionKey {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrPinRequired, nil, "not the attempt's action")
	}
	d := c.byKey[actionKey]
	if d == nil {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return s.receipt(d, false), nil
}

func (s *fixtureService) find(id delivery.DeploymentID) *delivery.Deployment {
	for _, d := range s.c.byKey {
		if d.ID() == id {
			return d
		}
	}
	return nil
}

func (s *fixtureService) GetRuntime(_ context.Context, attempt domain.AttemptID, bearer string, id delivery.DeploymentID) (delivery.Receipt, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.authenticate(attempt, bearer); err != nil {
		return delivery.Receipt{}, err
	}
	d := s.find(id)
	if d == nil {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	if c.actionKeys[attempt] != d.ActionKey() && c.cleanupBound[attempt] != id {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrDenied, nil, "the attempt is not bound to this deployment")
	}
	return s.receipt(d, false), nil
}

func (s *fixtureService) ReportOutcome(_ context.Context, attempt domain.AttemptID, bearer string, id delivery.DeploymentID, rep delivery.OutcomeReport) (delivery.Receipt, error) {
	c := s.c
	if rep.State == delivery.OutcomeApplying && c.onApplying != nil {
		c.onApplying() // outside the lock: the hook may look at the fixture
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.authenticate(attempt, bearer); err != nil {
		return delivery.Receipt{}, err
	}
	d := s.find(id)
	if d == nil {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	if c.actionKeys[attempt] != d.ActionKey() {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrPinRequired, nil, "the deployment does not match the attempt's pinned context")
	}
	if err := rep.Validate(); err != nil {
		return delivery.Receipt{}, err
	}
	var err error
	switch rep.State {
	case delivery.OutcomeApplying:
		_, err = d.BeginApply(rep.Objects, c.now)
	case delivery.OutcomeUnknown:
		_, err = d.MarkOutcomeUnknown(c.now)
	case delivery.OutcomeSucceeded:
		_, err = d.Settle(delivery.Settlement{Succeeded: true, Objects: rep.Objects}, c.now)
	case delivery.OutcomeFailed:
		_, err = d.Settle(delivery.Settlement{Reason: rep.Reason, Objects: rep.Objects}, c.now)
	}
	if err != nil {
		return delivery.Receipt{}, err
	}
	return s.receipt(d, false), nil
}

func (s *fixtureService) ReportCleanup(_ context.Context, attempt domain.AttemptID, bearer string, id delivery.DeploymentID, rep delivery.CleanupReport) (delivery.Receipt, error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.authenticate(attempt, bearer); err != nil {
		return delivery.Receipt{}, err
	}
	d := s.find(id)
	if d == nil {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	if c.cleanupBound[attempt] != id {
		return delivery.Receipt{}, delivery.Fail(delivery.ErrDenied, nil, "the attempt is not bound to this deployment")
	}
	if _, err := d.RecordCleanup(delivery.CleanupResult{State: rep.State, Reason: rep.Reason, Deleted: rep.Deleted}, c.now); err != nil {
		return delivery.Receipt{}, err
	}
	return s.receipt(d, false), nil
}

func (c *callbacks) describe() string {
	var b strings.Builder
	for _, e := range c.callLog() {
		fmt.Fprintf(&b, "%s %s -> %d %s\n", e.Method, e.Path, e.Status, e.Dropped)
	}
	return b.String()
}

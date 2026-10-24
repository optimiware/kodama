package stack

// The runtime API the step entrypoint and the deployment scripts call, behind
// TLS on the host. The routes are the real ones: runtime admission and context
// (internal/runtimeauth/http), the delivery callbacks (internal/delivery/http on
// the real router, in front of the real delivery.Service), secret resolution
// through the real secrets.Resolver and its runtime adapter over the encrypted
// PostgreSQL store, and the result route, which is the stand-in T-086's runtime
// suite uses (runtime authorization, then execution.CommitResult: T-022 owns
// the production route and its source and artifact handling).
//
// A fault point sits in front of every route: a request may be lost before it
// is processed, or its response after the state change committed (the
// connection is closed, which is what a lost acknowledgment looks like to curl),
// and either may crash the calling attempt's container.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	"github.com/optimiware/kodama/internal/secrets"
)

// Call is one request the server saw, in arrival order.
type Call struct {
	Method, Path string
	Body         string
	Status       int
	// Dropped is "", "request" or "response".
	Dropped string
	Crashed bool
}

// Match selects requests for a fault.
type Match func(method, path string, body []byte) bool

type fault struct {
	kind string // "request" or "response"
	// crash is "", "crash" (the container is killed after the control plane saw
	// its command start) or "vanish" (killed and removed before anyone observed
	// it: the runtime cannot tell whether the command ran).
	crash string
	match Match
	left  int // negative: every time
}

// Server is the runtime API server.
type Server struct {
	t testing.TB
	s *Stack

	mu     sync.Mutex
	calls  []Call
	faults []*fault

	srv    *httptest.Server
	CAFile string
	CA     []byte
	URL    string
}

// liveDelivery is the runtime service of the CURRENT stack: a restart replaces
// the stack's service and the next request is served by the new one.
type liveDelivery struct{ s *Stack }

var _ deliveryhttp.RuntimeService = liveDelivery{}

func (l liveDelivery) RegisterRuntime(ctx context.Context, a domain.AttemptID, b string, r delivery.RegistrationRequest) (delivery.Receipt, error) {
	return l.s.Svc.RegisterRuntime(ctx, a, b, r)
}

func (l liveDelivery) LookupByAction(ctx context.Context, a domain.AttemptID, b, k string) (delivery.Receipt, error) {
	return l.s.Svc.LookupByAction(ctx, a, b, k)
}

func (l liveDelivery) GetRuntime(ctx context.Context, a domain.AttemptID, b string, id delivery.DeploymentID) (delivery.Receipt, error) {
	return l.s.Svc.GetRuntime(ctx, a, b, id)
}

func (l liveDelivery) ReportOutcome(ctx context.Context, a domain.AttemptID, b string, id delivery.DeploymentID, r delivery.OutcomeReport) (delivery.Receipt, error) {
	return l.s.Svc.ReportOutcome(ctx, a, b, id, r)
}

func (l liveDelivery) ReportCleanup(ctx context.Context, a domain.AttemptID, b string, id delivery.DeploymentID, r delivery.CleanupReport) (delivery.Receipt, error) {
	return l.s.Svc.ReportCleanup(ctx, a, b, id, r)
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

func newServer(t testing.TB, s *Stack, dir string) *Server {
	t.Helper()
	c := &Server{t: t, s: s}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: bearerPresent{}, Authorizer: allowAll{}, RateLimiter: httpapi.Unlimited(), Prefix: "/runtime/v1",
	})
	must(t, err)
	rh, err := deliveryhttp.NewRuntime(liveDelivery{s}, deliveryhttp.DefaultRuntimeActions())
	must(t, err)
	must(t, rh.RegisterRoutes(router))
	admission, err := runtimehttp.NewHandler(runtimehttp.Config{Service: serviceFunc{s}})
	must(t, err)

	route := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/deployments"):
			router.ServeHTTP(w, r)
		case strings.HasSuffix(r.URL.Path, "/secrets/resolve"):
			c.resolveSecret(w, r)
		case strings.HasSuffix(r.URL.Path, "/result"):
			c.result(w, r)
		default:
			admission.ServeHTTP(w, r)
		}
	}
	c.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".git") && s.pod != nil {
			rec := httptest.NewRecorder()
			s.pod.repo.serve(rec, r) // the candidate's anonymous Git repository: not a runtime call
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		c.serve(route, w, r)
	}))
	// The containers share the host's network namespace, and the certificate
	// names the loopback addresses the API server and this server listen on.
	c.srv.StartTLS()
	t.Cleanup(c.srv.Close)
	c.URL = c.srv.URL
	c.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.srv.Certificate().Raw})
	c.CAFile = filepath.Join(dir, "ca.crt")
	must(t, os.WriteFile(c.CAFile, c.CA, 0o600))
	return c
}

// serviceFunc is runtime admission of the current stack.
type serviceFunc struct{ s *Stack }

func (f serviceFunc) Admit(ctx context.Context, attempt domain.AttemptID, req runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
	return f.s.Admission.Admit(ctx, attempt, req)
}

func (f serviceFunc) Renew(ctx context.Context, attempt domain.AttemptID, bearer string) (runtimev1.TokenResponse, error) {
	return f.s.Admission.Renew(ctx, attempt, bearer)
}

func (f serviceFunc) Context(ctx context.Context, attempt domain.AttemptID, bearer string) (runtimev1.Context, error) {
	return f.s.Admission.Context(ctx, attempt, bearer)
}

func (c *Server) serve(route http.HandlerFunc, w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	entry := Call{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/runtime/v1"), Body: string(body)}
	var f *fault
	c.mu.Lock()
	for _, cand := range c.faults {
		if cand.left != 0 && cand.match(r.Method, r.URL.Path, body) {
			f = cand
			if cand.left > 0 {
				cand.left--
			}
			break
		}
	}
	c.mu.Unlock()

	if f != nil && f.kind == "request" {
		entry.Dropped = "request"
		if f.crash != "" {
			c.crash(r.URL.Path, f.crash)
			entry.Crashed = true
		}
		c.record(entry)
		c.hangUp(w)
		return
	}
	rec := httptest.NewRecorder()
	route(rec, r)
	entry.Status = rec.Code
	if f != nil && f.kind == "response" {
		entry.Dropped = "response"
		if f.crash != "" {
			c.crash(r.URL.Path, f.crash)
			entry.Crashed = true
		}
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

func (c *Server) hangUp(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		c.t.Error("the server cannot hang up")
		return
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func (c *Server) record(e Call) {
	c.mu.Lock()
	c.calls = append(c.calls, e)
	c.mu.Unlock()
}

// crash kills the container of the attempt the request names, after the control
// plane has observed its command start: the attempt then ends the way a workload
// that died mid-command does, with the runtime's own launch evidence (started).
func (c *Server) crash(path, mode string) {
	const marker = "/attempts/"
	i := strings.Index(path, marker)
	if i < 0 {
		c.t.Error("a crash fault on a request that names no attempt")
		return
	}
	attempt := domain.AttemptID(strings.SplitN(path[i+len(marker):], "/", 2)[0])
	if mode == "crash" && !c.s.awaitStarted(attempt, 30*time.Second) {
		c.t.Errorf("attempt %s was never observed started by the control plane", attempt)
	}
	out, err := exec.Command("podman", "ps", "-q", "--filter", "label="+podman.LabelAttempt+"="+string(attempt)).Output()
	if err != nil {
		c.t.Errorf("finding the container of %s: %v", attempt, err)
		return
	}
	for _, id := range strings.Fields(string(out)) {
		if b, err := exec.Command("podman", "kill", id).CombinedOutput(); err != nil {
			c.t.Errorf("killing %s: %v: %s", id, err, b)
		}
		if mode == "vanish" {
			if b, err := exec.Command("podman", "rm", "--force", id).CombinedOutput(); err != nil {
				c.t.Errorf("removing %s: %v: %s", id, err, b)
			}
		}
	}
}

// ---- fault controls ----------------------------------------------------------

func (c *Server) add(f *fault) {
	c.mu.Lock()
	c.faults = append(c.faults, f)
	c.mu.Unlock()
}

// DropResponses loses the response of every matching request, after processing.
func (c *Server) DropResponses(m Match) { c.add(&fault{kind: "response", match: m, left: -1}) }

// DropRequests loses every matching request before it is processed.
func (c *Server) DropRequests(m Match) { c.add(&fault{kind: "request", match: m, left: -1}) }

// DropResponseOnce loses the response of the first matching request only.
func (c *Server) DropResponseOnce(m Match) { c.add(&fault{kind: "response", match: m, left: 1}) }

// CrashAfter processes the first matching request (its effects commit), waits for
// the control plane to observe the command started and then kills the calling
// container before any response is returned: the runtime's own evidence for the
// attempt is "started".
func (c *Server) CrashAfter(m Match) {
	c.add(&fault{kind: "response", crash: "crash", match: m, left: 1})
}

// VanishBefore kills and removes the calling container instead of processing the
// first matching request, before anything observed the container running (the
// request is never processed): the runtime cannot tell whether the command ran.
func (c *Server) VanishBefore(m Match) {
	c.add(&fault{kind: "request", crash: "vanish", match: m, left: 1})
}

// VanishAfter processes the first matching request (its effects commit) and then
// kills and removes the calling container, unobserved, before any response is
// returned.
func (c *Server) VanishAfter(m Match) {
	c.add(&fault{kind: "response", crash: "vanish", match: m, left: 1})
}

// WaitCrash waits until a fault has crashed or removed a container.
func (c *Server) WaitCrash(timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range c.Since(0) {
			if e.Crashed {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("no container was crashed within %s\n%s", timeout, c.Describe())
}

// ClearFaults removes every fault.
func (c *Server) ClearFaults() {
	c.mu.Lock()
	c.faults = nil
	c.mu.Unlock()
}

// OutcomeState matches a delivery outcome report of the state.
func OutcomeState(state string) Match {
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

// CleanupResultState matches a cleanup result report of the state.
func CleanupResultState(state string) Match {
	return func(method, path string, body []byte) bool {
		if method != http.MethodPost || !strings.HasSuffix(path, "/cleanup-result") {
			return false
		}
		var b struct {
			State string `json:"state"`
		}
		return json.Unmarshal(body, &b) == nil && b.State == state
	}
}

// ---- call log ----------------------------------------------------------------

// Mark returns the position in the call log.
func (c *Server) Mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// Since returns the calls recorded after mark.
func (c *Server) Since(mark int) []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Call(nil), c.calls[mark:]...)
}

// Describe renders the call log.
func (c *Server) Describe() string {
	var b strings.Builder
	for _, e := range c.Since(0) {
		b.WriteString(e.Method + " " + e.Path + " -> " + strconv.Itoa(e.Status) + " " + e.Dropped + "\n")
	}
	return b.String()
}

// CountCalls counts the calls of the method whose path ends with suffix.
func CountCalls(cs []Call, method, suffix string) int {
	n := 0
	for _, e := range cs {
		if e.Method == method && strings.HasSuffix(e.Path, suffix) {
			n++
		}
	}
	return n
}

// OutcomeStates lists the outcome states reported, marking lost ones.
func OutcomeStates(cs []Call) []string {
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

// ---- secret resolution -------------------------------------------------------

type scopeKey struct{}

// attemptGrant is the attempt authority of the secrets resolver over execution's
// persisted attempt: it grants exactly the bindings of the attempt's immutable
// pinned context while the attempt is running.
type attemptGrant struct{ s *Stack }

func (g attemptGrant) Grant(ctx context.Context, attempt domain.AttemptID) (secrets.AttemptGrant, error) {
	scope, ok := ctx.Value(scopeKey{}).(domain.Scope)
	if !ok {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	run, err := g.s.ExecStore.RunOf(ctx, scope, attempt)
	if err != nil {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	agg, err := g.s.ExecStore.Get(ctx, scope, run)
	if err != nil {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	at := agg.Attempt(attempt)
	if at == nil {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	if at.Status != execution.AttemptRunning {
		return secrets.AttemptGrant{}, secrets.ErrAttemptInactive
	}
	doc, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	return secrets.AttemptGrant{Scope: scope, RunID: run, AttemptID: attempt, Bindings: doc.Secrets}, nil
}

func (c *Server) resolveSecret(w http.ResponseWriter, r *http.Request) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if attempt == "" {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/runtime/v1/attempts/"), "/")
		attempt = domain.AttemptID(parts[0])
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	req, err := runtimev1.DecodeRequest[runtimev1.SecretResolveRequest](body)
	if err != nil {
		reply(w, domain.CodeInvalidRequest)
		return
	}
	az, err := c.s.Admission.Authorize(r.Context(), runtimeauth.AuthorizeRequest{
		AttemptID: attempt, Bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), Action: runtimeauth.ActionSecretResolve,
	})
	switch {
	case errors.Is(err, runtimeauth.ErrAttemptRevoked):
		reply(w, runtimev1.CodeAttemptRevoked)
		return
	case err != nil:
		reply(w, domain.CodeInvalidCredential)
		return
	}
	ctx := context.WithValue(r.Context(), scopeKey{}, az.Scope)
	resp, err := c.s.secretAdapter.Resolve(ctx, attempt, req, "req_t067_secret")
	if err != nil {
		var ce *runtimev1.ContractError
		if errors.As(err, &ce) {
			reply(w, ce.Code)
			return
		}
		reply(w, domain.CodeUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---- result ------------------------------------------------------------------

func (c *Server) result(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/runtime/v1/attempts/"), "/")
	attempt := domain.AttemptID(parts[0])
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	req, err := runtimev1.DecodeRequest[runtimev1.ResultRequest](body)
	if err != nil {
		reply(w, domain.CodeInvalidRequest)
		return
	}
	az, err := c.s.Admission.Authorize(r.Context(), runtimeauth.AuthorizeRequest{
		AttemptID: attempt, Bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), Action: runtimeauth.ActionResultSubmit,
	})
	switch {
	case errors.Is(err, runtimeauth.ErrAttemptRevoked):
		reply(w, runtimev1.CodeAttemptRevoked)
		return
	case err != nil:
		reply(w, domain.CodeInvalidCredential)
		return
	}
	ack, err := c.s.Exec.CommitResult(r.Context(), execution.ResultCommit{Scope: az.Scope, AttemptID: attempt, Request: req})
	switch {
	case errors.Is(err, execution.ErrAttemptRevoked):
		reply(w, runtimev1.CodeAttemptRevoked)
		return
	case err != nil:
		c.t.Logf("result of %s refused: %v", attempt, err)
		reply(w, domain.CodeInvalidRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runtimev1.ResultResponse{
		AttemptID: attempt, ResultHash: runtimev1.Digest(ack.ResultHash), AttemptStatus: runtimev1.AttemptStatus(ack.AttemptStatus),
		AcceptedAt: runtimev1.Timestamp(time.Now().UTC().Format(time.RFC3339Nano)),
	})
}

func reply(w http.ResponseWriter, code domain.ErrorCode) {
	status, _ := runtimev1.Status(code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: "t067 fixture", RequestID: "req_t067fixture", Details: map[string]any{}}})
}

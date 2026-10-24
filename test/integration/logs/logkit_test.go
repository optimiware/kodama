package logs_test

// The log module composed locally over the real peers: the log store on a
// non-owner PostgreSQL pool, the artifact reader/writer adapters over the real
// repository and object store (with response-loss fault wrappers), the real
// Kubernetes reader over the kind API server, the real service, controller and
// protected HTTP route behind the shared router and the standalone identity
// authority. Test-local pieces implement existing ports only: placement (the
// tenant's own namespace), the delivered-value redaction source, a tap on the
// live reader and a counting API transport.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/logs"
	logsadapters "github.com/optimiware/kodama/internal/logs/adapters"
	logshttp "github.com/optimiware/kodama/internal/logs/http"
	logskube "github.com/optimiware/kodama/internal/logs/kubernetes"
	logspg "github.com/optimiware/kodama/internal/logs/postgres"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
)

// apiTap counts the requests the log reader makes to the Kubernetes API.
type apiTap struct {
	base http.RoundTripper
	mu   sync.Mutex
	reqs []string
}

func (a *apiTap) RoundTrip(r *http.Request) (*http.Response, error) {
	a.mu.Lock()
	a.reqs = append(a.reqs, r.Method+" "+r.URL.Path)
	a.mu.Unlock()
	return a.base.RoundTrip(r)
}

func (a *apiTap) Requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.reqs...)
}

// logRequests counts the requests that read a log subresource.
func (a *apiTap) logRequests() (n int) {
	for _, r := range a.Requests() {
		if strings.HasSuffix(r, "/log") {
			n++
		}
	}
	return
}

// tapLive records every open of the real reader and injects the transport
// faults of node loss: a failure before any byte, or an interruption of the
// real response after N bytes.
type tapLive struct {
	inner logs.LiveReader

	mu      sync.Mutex
	opens   int
	targets []logs.Target
	queries []logs.LiveQuery
	// FailOpen, when set, replaces the open with an error.
	FailOpen func(logs.Target) error
	// CutAfter, when > 0, makes the real stream fail with a connection reset
	// after that many bytes of the real response.
	CutAfter func(logs.Target) int
}

var errReset = errors.New("read tcp: connection reset by peer")

func (t *tapLive) Open(ctx context.Context, tg logs.Target, q logs.LiveQuery) (io.ReadCloser, error) {
	t.mu.Lock()
	t.opens++
	t.targets = append(t.targets, tg)
	t.queries = append(t.queries, q)
	fail, cut := t.FailOpen, t.CutAfter
	t.mu.Unlock()
	if fail != nil {
		if err := fail(tg); err != nil {
			return nil, err
		}
	}
	rc, err := t.inner.Open(ctx, tg, q)
	if err != nil {
		return nil, err
	}
	if cut != nil {
		if n := cut(tg); n > 0 {
			return &cutReader{rc: rc, left: n}, nil
		}
	}
	return rc, nil
}

func (t *tapLive) Opens() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.opens
}

func (t *tapLive) Targets() []logs.Target {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]logs.Target(nil), t.targets...)
}

func (t *tapLive) Queries() []logs.LiveQuery {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]logs.LiveQuery(nil), t.queries...)
}

func (t *tapLive) Reset() {
	t.mu.Lock()
	t.FailOpen, t.CutAfter = nil, nil
	t.mu.Unlock()
}

type cutReader struct {
	rc   io.ReadCloser
	left int
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errReset
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, err := c.rc.Read(p)
	c.left -= n
	if err == io.EOF && c.left > 0 {
		return n, io.EOF // the real response ended before the cut point
	}
	if err == nil && c.left <= 0 {
		return n, nil
	}
	return n, err
}

func (c *cutReader) Close() error { return c.rc.Close() }

// deliveredValues is the logs.Redactions source: the values an attempt was
// given (its admission credential and every runtime token it was issued, as
// observed at the runtime boundary) plus values a test delivered explicitly.
type deliveredValues struct {
	s   *stack
	mu  sync.Mutex
	err error
}

func (d *deliveredValues) Fail(err error) { d.mu.Lock(); d.err = err; d.mu.Unlock() }

func (d *deliveredValues) Values(_ context.Context, _ domain.Scope, attempt domain.AttemptID) ([][]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	var out [][]byte
	if c := d.s.srv.credentialOf(attempt); c != "" {
		out = append(out, []byte(c))
	}
	for _, tok := range d.s.srv.tokensOf(attempt) {
		out = append(out, []byte(tok))
	}
	d.s.extraMu.Lock()
	defer d.s.extraMu.Unlock()
	return append(out, d.s.extra[attempt]...), nil
}

// tenantPlacement places an attempt in its own tenant's namespace (the run
// namespaces are per-tenant here) and the primary container.
type tenantPlacement struct{ s *stack }

func (p tenantPlacement) Placement(_ context.Context, a logs.AttemptInfo) (string, string, error) {
	return p.s.ns(a.Scope.TenantID), kube.PrimaryContainer, nil
}

type logsOptions struct {
	// Pool replaces the non-owner runtime pool of the log and artifact stores
	// (a single-connection pool proves no code path holds a connection while
	// asking its peers for another).
	Pool *pgxpool.Pool
	// Limits replaces the read ceilings.
	Limits *logs.Limits
	// CaptureBytes bounds a controller capture.
	CaptureBytes int64
	// Lease is the controller's claim lease.
	Lease time.Duration
}

// logsKit is one composition of the log module.
type logsKit struct {
	s *stack

	Store    *logspg.Store
	ArtRepo  *artifactspg.Store
	Repo     *artifactstest.FaultRepo
	Objects  *artifactstest.FaultObjects
	Reader   *logsadapters.Artifacts
	Retained *logsadapters.Retained
	Attempts *logsadapters.Attempts
	Work     *logsadapters.Workloads
	Live     *tapLive
	API      *apiTap
	Redact   *deliveredValues
	Identity identity.Authority
	Authz    *logsadapters.Authority
	Svc      *logs.Service
	Ctrl     *logs.Controller
	HTTP     *httptest.Server
	Handler  http.Handler
}

// DefaultLimits are the read ceilings of the fixture: explicit and small enough
// for the bounds to be hit.
func DefaultLimits() logs.Limits {
	return logs.Limits{DefaultTail: 100, MaxTail: 400, MaxLines: 1000, MaxBytes: 256 << 10, MaxLineBytes: 1024, MaxDuration: 30 * time.Second, MaxStreams: 8, WriteTimeout: 5 * time.Second}
}

func (s *stack) newLogs(o logsOptions) *logsKit {
	s.t.Helper()
	t := s.t
	x := s.x
	pool := o.Pool
	if pool == nil {
		pool = x.Runtime
	}
	k := &logsKit{s: s}
	var err error
	k.Store, err = logspg.NewStore(logspg.Config{DB: pool})
	must(t, err)
	k.ArtRepo, err = artifactspg.NewStore(artifactspg.Config{DB: pool})
	must(t, err)
	k.Repo = &artifactstest.FaultRepo{Repository: k.ArtRepo}
	k.Objects = &artifactstest.FaultObjects{ObjectStore: s.objects}
	k.Reader, err = logsadapters.NewArtifacts(k.Repo, k.Objects)
	must(t, err)
	k.Retained, err = logsadapters.NewRetained(k.Reader, logsadapters.WriterConfig{
		Repository: k.Repo, Objects: k.Objects, Clock: x.Clock.Now,
		Limits: artifacts.Limits{DiagnosticBytes: 1 << 24, DiagnosticFiles: 32}, SpoolDir: t.TempDir(),
	})
	must(t, err)
	k.Attempts, err = logsadapters.NewAttempts(s.rt)
	must(t, err)
	k.Work, err = logsadapters.NewWorkloads(tenantPlacement{s})
	must(t, err)
	k.API = &apiTap{base: s.c.api.hc.Transport}
	real, err := logskube.NewReader(logskube.Config{Endpoint: s.c.api.origin, Client: &http.Client{Transport: k.API}})
	must(t, err)
	k.Live = &tapLive{inner: real}
	k.Redact = &deliveredValues{s: s}

	reader, err := identitypg.NewStore(pool, x.Events)
	must(t, err)
	k.Identity, err = identity.NewStandaloneAuthority(reader)
	must(t, err)
	k.Authz, err = logsadapters.NewAuthority(k.Identity, reader)
	must(t, err)

	lim := DefaultLimits()
	if o.Limits != nil {
		lim = *o.Limits
	}
	k.Svc, err = logs.NewService(logs.Config{
		Authority: k.Authz, Attempts: k.Attempts, Workloads: k.Work, Live: k.Live, Retained: k.Retained, Store: k.Store,
		Redactions: k.Redact, Limits: lim, Clock: x.Clock.Now,
	})
	must(t, err)
	capture := o.CaptureBytes
	if capture == 0 {
		capture = 4 << 20
	}
	lease := o.Lease
	if lease == 0 {
		lease = 30 * time.Second
	}
	k.Ctrl, err = logs.NewController(logs.ControllerConfig{
		Attempts: k.Attempts, Workloads: k.Work, Live: k.Live, Retained: k.Retained, Store: k.Store, Redactions: k.Redact,
		Clock: x.Clock.Now, CaptureBytes: capture, CaptureTimeout: 30 * time.Second, Lease: lease, SpoolDir: t.TempDir(),
	})
	must(t, err)

	h, err := logshttp.NewHandler(k.Svc, k.Svc.Limits().WriteTimeout)
	must(t, err)
	rules := identityhttp.DefaultRules()
	rules[logshttp.ActionLogs] = identityhttp.Rule{Level: identityhttp.LevelProject, Any: logshttp.RoutePermissions()[logshttp.ActionLogs]}
	authz, err := identityhttp.NewAuthorizer(k.Identity, rules)
	must(t, err)
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: bearerUsers{}, Authorizer: authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	must(t, err)
	must(t, h.RegisterRoutes(rt))
	k.Handler = rt
	k.HTTP = httptest.NewServer(rt)
	t.Cleanup(k.HTTP.Close)
	return k
}

// bearerUsers authenticates "Authorization: Bearer <user id>": the fixture's
// public principal. Authorization behind it is the real standalone identity.
type bearerUsers struct{}

func (bearerUsers) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

// runtimeAuthn authenticates a runtime token for the artifact routes: it
// verifies the bearer under the signing ring and emits the session principal
// the artifact authority keys on; the grant itself is re-read per call.
type runtimeAuthn struct{ v *rtjwt.Verifier }

func (a runtimeAuthn) Authenticate(ctx context.Context, r *http.Request) (httpapi.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	claims, err := a.v.Verify(ctx, tok)
	if err != nil {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: resultsadapters.RuntimePrincipal(claims.Tenant, claims.Session)}, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

// Aliases for the artifact module's fault wrappers (set under their own lock).
type (
	artifactsFaultObjects = artifactstest.FaultObjects
	artifactsFaultRepo    = artifactstest.FaultRepo
)

type artifactsObject = artifacts.Object

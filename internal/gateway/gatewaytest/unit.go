package gatewaytest

// The service-free fixture of the model gateway (T-083): the real gateway
// Service over the in-memory MemStore, a controlled clock, scripted provider
// streams and table authorities. No database, TLS server, secret store or
// Kubernetes object exists; nothing here is provider or harness qualification
// (real provider smoke is T-085, the OpenCode wrapper is T-031).
//
// Constructor example, as T-072 composes it:
//
//	u := gatewaytest.NewUnit(t, gatewaytest.UnitOptions{})
//	u.Configure(gatewaytest.ScopeA, nil)           // enable model access as the project admin
//	a := u.NewAttempt(gatewaytest.ScopeA)          // a run's attempt authorized for model.invoke
//	u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 40, 10, "hello"))
//	res, err := u.Invoke(a, gatewaytest.Body("gpt-a", "hi", 64), &gatewaytest.Sink{})
//	admitted, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID)

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/openai"
)

// The unit fixture's projects: two projects of one tenant and one of another.
var (
	ScopeA  = domain.Scope{TenantID: "t083_acme", ProjectID: "p083_inventory"}
	ScopeA2 = domain.Scope{TenantID: "t083_acme", ProjectID: "p083_billing"}
	ScopeB  = domain.Scope{TenantID: "t083_globex", ProjectID: "p083_ops"}
)

// UnitEpoch is the first instant of every Clock.
var UnitEpoch = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// Clock is a controlled clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at UnitEpoch.
func NewClock() *Clock { return &Clock{t: UnitEpoch} }

// Now returns the current instant.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// ---- workload authority ----------------------------------------------------------

// MemAuthority is the table fixture of workload authority: it plays both the
// route's bearer verification (gateway.Workloads) and the store's in-transaction
// recheck (WorkloadCheck). It models nothing of the runtime token itself.
type MemAuthority struct {
	mu       sync.Mutex
	attempts map[domain.AttemptID]*memAttempt
	err      error
	n        atomic.Int64
}

type memAttempt struct {
	w       gateway.Workload
	bearer  string
	revoked bool
}

// NewMemAuthority returns an authority that knows no attempt.
func NewMemAuthority() *MemAuthority {
	return &MemAuthority{attempts: map[domain.AttemptID]*memAttempt{}}
}

// Add registers a fresh attempt of a new run in scope, authorized to invoke the
// model, and returns its identity and bearer.
func (a *MemAuthority) Add(scope domain.Scope) (gateway.Workload, string) {
	n := a.n.Add(1)
	return a.AddTo(scope, domain.RunID(fmt.Sprintf("run_t083_%03d", n)))
}

// AddTo registers another attempt of an existing run.
func (a *MemAuthority) AddTo(scope domain.Scope, run domain.RunID) (gateway.Workload, string) {
	n := a.n.Add(1)
	w := gateway.Workload{Scope: scope, RunID: run, AttemptID: domain.AttemptID(fmt.Sprintf("att_t083_%03d", n)), Session: fmt.Sprintf("ses_%03d", n)}
	bearer := fmt.Sprintf("rt_t083_bearer_%03d", n)
	a.mu.Lock()
	a.attempts[w.AttemptID] = &memAttempt{w: w, bearer: bearer}
	a.mu.Unlock()
	return w, bearer
}

// Revoke ends an attempt's authority (the attempt was cancelled or its grant
// revoked).
func (a *MemAuthority) Revoke(attempt domain.AttemptID) {
	a.mu.Lock()
	if m := a.attempts[attempt]; m != nil {
		m.revoked = true
	}
	a.mu.Unlock()
}

// Fail makes every check fail like an outage of the authority (nil restores it).
func (a *MemAuthority) Fail(err error) { a.mu.Lock(); a.err = err; a.mu.Unlock() }

// Authorize implements gateway.Workloads.
func (a *MemAuthority) Authorize(_ context.Context, attempt domain.AttemptID, bearer string) (gateway.Workload, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return gateway.Workload{}, gateway.FailWith(gateway.ErrUnavailable, a.err, "the workload authority could not answer")
	}
	m := a.attempts[attempt]
	if m == nil || m.bearer != bearer {
		return gateway.Workload{}, gateway.Failf(gateway.ErrUnauthenticated, "the workload credential is not valid")
	}
	if m.revoked {
		return gateway.Workload{}, gateway.Failf(gateway.ErrForbidden, "the attempt is no longer active")
	}
	return m.w, nil
}

// Check is the store's WorkloadCheck: the current authority of the exact
// workload tuple the store was asked to act for.
func (a *MemAuthority) Check(w gateway.Workload, _ time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return gateway.FailWith(gateway.ErrUnavailable, a.err, "the workload authority could not answer")
	}
	m := a.attempts[w.AttemptID]
	if m == nil || m.w != w {
		return gateway.Failf(gateway.ErrUnauthenticated, "the workload identity is not known")
	}
	if m.revoked {
		return gateway.Failf(gateway.ErrForbidden, "the attempt is no longer active")
	}
	return nil
}

// ---- management authority --------------------------------------------------------

// MemManagement is a table-driven gateway.Authority keyed by scope and actor.
type MemManagement struct {
	mu     sync.Mutex
	grants map[mgmtKey]map[gateway.Permission]bool
	err    error
}

type mgmtKey struct {
	scope domain.Scope
	actor string
}

// NewMemManagement returns an authority that knows no actor.
func NewMemManagement() *MemManagement {
	return &MemManagement{grants: map[mgmtKey]map[gateway.Permission]bool{}}
}

// Grant makes the actor known in the scope with exactly perms.
func (m *MemManagement) Grant(scope domain.Scope, actor string, perms ...gateway.Permission) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := map[gateway.Permission]bool{}
	for _, p := range perms {
		set[p] = true
	}
	m.grants[mgmtKey{scope, actor}] = set
}

// Revoke makes the actor unknown in the scope again.
func (m *MemManagement) Revoke(scope domain.Scope, actor string) {
	m.mu.Lock()
	delete(m.grants, mgmtKey{scope, actor})
	m.mu.Unlock()
}

// Fail makes every check an outage (nil restores it).
func (m *MemManagement) Fail(err error) { m.mu.Lock(); m.err = err; m.mu.Unlock() }

// Authorize implements gateway.Authority.
func (m *MemManagement) Authorize(_ context.Context, actor gateway.Actor, scope domain.Scope, perm gateway.Permission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	set, ok := m.grants[mgmtKey{scope, actor.ID}]
	if !ok {
		return gateway.ErrHidden
	}
	if !set[perm] {
		return gateway.ErrDenied
	}
	return nil
}

// ---- credentials and destinations ------------------------------------------------

// MemCredentials is a gateway.Credentials double over a table of exact secret
// versions. Open returns the pinned version or fails; it never substitutes
// another version, and it records every version it opened.
type MemCredentials struct {
	mu      sync.Mutex
	secrets map[gateway.CredentialRef]string
	opened  []gateway.CredentialRef
	err     error
}

// NewMemCredentials returns an empty table.
func NewMemCredentials() *MemCredentials {
	return &MemCredentials{secrets: map[gateway.CredentialRef]string{}}
}

// Put stores one exact secret version.
func (c *MemCredentials) Put(ref gateway.CredentialRef, bearer string) {
	c.mu.Lock()
	c.secrets[ref] = bearer
	c.mu.Unlock()
}

// Fail makes Open fail with err (nil restores it).
func (c *MemCredentials) Fail(err error) { c.mu.Lock(); c.err = err; c.mu.Unlock() }

// Opened lists the versions opened so far.
func (c *MemCredentials) Opened() []gateway.CredentialRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]gateway.CredentialRef(nil), c.opened...)
}

// Open implements gateway.Credentials.
func (c *MemCredentials) Open(_ context.Context, _ gateway.Workload, b gateway.RunBinding, _ gateway.RequestID) (*gateway.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, gateway.FailWith(gateway.ErrCredential, c.err, "the credential is unavailable")
	}
	v, ok := c.secrets[b.Credential]
	if !ok {
		return nil, gateway.Failf(gateway.ErrCredential, "the credential is unavailable")
	}
	c.opened = append(c.opened, b.Credential)
	return gateway.NewCredential([]byte(v)), nil
}

// Check implements gateway.Credentials.
func (c *MemCredentials) Check(_ context.Context, _ domain.Scope, ref gateway.CredentialRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.secrets[ref]; !ok {
		return gateway.Failf(gateway.ErrCredential, "the credential is unavailable")
	}
	return nil
}

// MemDestinations approves exactly the listed origins.
type MemDestinations struct {
	mu      sync.Mutex
	origins map[string]bool
}

// NewMemDestinations approves the origins.
func NewMemDestinations(origins ...string) *MemDestinations {
	d := &MemDestinations{origins: map[string]bool{}}
	for _, o := range origins {
		d.origins[o] = true
	}
	return d
}

// Approve implements gateway.Destinations.
func (d *MemDestinations) Approve(_ context.Context, _ domain.Scope, e gateway.Endpoint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.origins[e.Origin()] {
		return gateway.Failf(gateway.ErrDestination, "the destination is not approved")
	}
	return nil
}

// ---- scripted provider -----------------------------------------------------------

// Step answers one provider call.
type Step func(ctx context.Context, call gateway.ProviderCall) (gateway.Stream, error)

// ScriptedProvider is a gateway.Provider that plays scripted steps in order and
// records every call it received. It makes no network connection. A call with
// no step left fails the test run loudly instead of inventing a reply.
type ScriptedProvider struct {
	mu     sync.Mutex
	script []Step
	calls  []ProviderCallRecord
}

// ProviderCallRecord is what the provider was asked, with the credential read
// once at call time (the double stands in for the transport).
type ProviderCallRecord struct {
	Endpoint  gateway.Endpoint
	Bearer    string
	Model     string
	MaxTokens int64
}

// Script queues steps for the next provider calls.
func (p *ScriptedProvider) Script(steps ...Step) {
	p.mu.Lock()
	p.script = append(p.script, steps...)
	p.mu.Unlock()
}

// Calls returns the calls received so far.
func (p *ScriptedProvider) Calls() []ProviderCallRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProviderCallRecord(nil), p.calls...)
}

// ErrUnscripted is returned for a provider call nobody scripted.
var ErrUnscripted = errors.New("gatewaytest: the provider was called with no scripted step")

// Open implements gateway.Provider.
func (p *ScriptedProvider) Open(ctx context.Context, call gateway.ProviderCall) (gateway.Stream, error) {
	p.mu.Lock()
	p.calls = append(p.calls, ProviderCallRecord{Endpoint: call.Endpoint, Bearer: string(call.Credential.Reveal()), Model: call.Completion.Model, MaxTokens: call.MaxTokens})
	var step Step
	if len(p.script) > 0 {
		step, p.script = p.script[0], p.script[1:]
	}
	p.mu.Unlock()
	if step == nil {
		return nil, ErrUnscripted
	}
	return step(ctx, call)
}

type scriptedStream struct {
	chunks []gateway.Chunk
	// then is returned once the chunks are consumed; nil ends with io.EOF after Done.
	then func(ctx context.Context) error
	dest gateway.Destination
	// closed counts Close calls, shared with the test through closed().
	closes *atomic.Int64
}

func (s *scriptedStream) Next(ctx context.Context) (gateway.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return gateway.Chunk{}, err
	}
	if len(s.chunks) > 0 {
		c := s.chunks[0]
		s.chunks = s.chunks[1:]
		return c, nil
	}
	if s.then != nil {
		return gateway.Chunk{}, s.then(ctx)
	}
	return gateway.Chunk{}, io.EOF
}

func (s *scriptedStream) Destination() gateway.Destination { return s.dest }
func (s *scriptedStream) Close() error {
	if s.closes != nil {
		s.closes.Add(1)
	}
	return nil
}

func data(model, text string) gateway.Chunk {
	return gateway.Chunk{Data: []byte(`{"model":"` + model + `","text":"` + text + `"}`), Model: model}
}

func stream(call gateway.ProviderCall, closes *atomic.Int64, chunks []gateway.Chunk, then func(ctx context.Context) error) gateway.Stream {
	return &scriptedStream{chunks: chunks, then: then, closes: closes,
		dest: gateway.Destination{Origin: call.Endpoint.Origin(), IP: "203.0.113.7"}}
}

// Reply streams the texts, then valid usage and the terminator.
func Reply(model string, in, out int64, texts ...string) Step {
	return func(_ context.Context, call gateway.ProviderCall) (gateway.Stream, error) {
		var cs []gateway.Chunk
		for _, t := range texts {
			cs = append(cs, data(model, t))
		}
		cs = append(cs, gateway.Chunk{Usage: &gateway.TokenUsage{Input: in, Output: out}}, gateway.Chunk{Done: true})
		return stream(call, nil, cs, nil), nil
	}
}

// ReplyNoUsage streams a complete reply that reports no usage.
func ReplyNoUsage(model string, texts ...string) Step {
	return func(_ context.Context, call gateway.ProviderCall) (gateway.Stream, error) {
		var cs []gateway.Chunk
		for _, t := range texts {
			cs = append(cs, data(model, t))
		}
		return stream(call, nil, append(cs, gateway.Chunk{Done: true}), nil), nil
	}
}

// ReplyInvalidUsage streams a complete reply whose usage object is malformed.
func ReplyInvalidUsage(model string) Step {
	return func(_ context.Context, call gateway.ProviderCall) (gateway.Stream, error) {
		return stream(call, nil, []gateway.Chunk{data(model, "x"), {UsageInvalid: true}, {Done: true}}, nil), nil
	}
}

// ReplyTruncated streams the texts and then ends without the terminator.
func ReplyTruncated(model string, texts ...string) Step {
	return func(_ context.Context, call gateway.ProviderCall) (gateway.Stream, error) {
		var cs []gateway.Chunk
		for _, t := range texts {
			cs = append(cs, data(model, t))
		}
		return stream(call, nil, cs, func(context.Context) error {
			return &gateway.StreamError{Kind: gateway.FaultTruncated, Err: io.ErrUnexpectedEOF}
		}), nil
	}
}

// ReplyHang streams the texts and then holds the stream open until the call's
// context ends. started (if non-nil) is closed once the texts were delivered
// and the stream is waiting; closes (if non-nil) counts Close calls, the
// observable proof that the upstream call was stopped.
func ReplyHang(model string, started chan<- struct{}, closes *atomic.Int64, texts ...string) Step {
	return func(_ context.Context, call gateway.ProviderCall) (gateway.Stream, error) {
		var cs []gateway.Chunk
		for _, t := range texts {
			cs = append(cs, data(model, t))
		}
		var once sync.Once
		return stream(call, closes, cs, func(ctx context.Context) error {
			if started != nil {
				once.Do(func() { close(started) })
			}
			<-ctx.Done()
			return ctx.Err()
		}), nil
	}
}

// ReplyFault fails to open the stream with a provider error.
func ReplyFault(e *gateway.ProviderError) Step {
	return func(context.Context, gateway.ProviderCall) (gateway.Stream, error) { return nil, e }
}

// ReplyStatus fails to open the stream with an error status of the provider.
func ReplyStatus(status int, sent bool) Step {
	return ReplyFault(&gateway.ProviderError{Kind: gateway.ProviderStatus, Status: status, Sent: sent})
}

// ---- unit world ------------------------------------------------------------------

// UnitOptions override the unit world's collaborators.
type UnitOptions struct {
	Timeouts gateway.Timeouts
	// Endpoint is the approved provider base URL; the default is a placeholder
	// that names no real host.
	Endpoint string
}

// DefaultEndpoint is the fixture provider base URL (a reserved, non-resolving
// name).
const DefaultEndpoint = "https://models.t083.example.invalid/v1"

// UnitAttempt is one attempt of a run, authorized for model.invoke.
type UnitAttempt struct {
	Workload gateway.Workload
	Bearer   string
}

// Unit is the whole service-free gateway.
type Unit struct {
	T           testing.TB
	Clock       *Clock
	Store       *MemStore
	Workloads   *MemAuthority
	Management  *MemManagement
	Credentials *MemCredentials
	Destination *MemDestinations
	Provider    *ScriptedProvider
	Svc         *gateway.Service
	// Endpoint is the approved base URL.
	Endpoint string
}

// Secret is the fixture secret: Ref at version 1 holds Bearer.
var Secret = gateway.CredentialRef{Ref: "secret://project/model-key", Version: 1}

// UnitBearer is the provider key of the fixture secret.
const UnitBearer = "sk-t083-PROVIDER-KEY-SENTINEL"

// NewUnit wires the service. The project admin (Admin) holds project:settings
// and config:read in every fixture scope; Viewer holds config:read only.
// Heartbeats are effectively off (Recheck is one minute); tests that need
// reconciliation advance the clock.
func NewUnit(t testing.TB, o UnitOptions) *Unit {
	t.Helper()
	endpoint := o.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	ep, err := gateway.ParseEndpoint(endpoint)
	must(t, err)
	u := &Unit{
		T: t, Clock: NewClock(), Workloads: NewMemAuthority(), Management: NewMemManagement(), Credentials: NewMemCredentials(),
		Destination: NewMemDestinations(ep.Origin()), Provider: &ScriptedProvider{}, Endpoint: endpoint,
	}
	for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		u.Management.Grant(s, Admin, gateway.PermRead, gateway.PermSettings)
		u.Management.Grant(s, Viewer, gateway.PermRead)
	}
	u.Credentials.Put(Secret, UnitBearer)
	u.Store, err = NewMemStore(u.Workloads.Check, u.Management)
	must(t, err)
	to := o.Timeouts
	if to == (gateway.Timeouts{}) {
		to = gateway.DefaultTimeouts()
		to.Recheck = time.Minute
		to.FinalizeBackoff = time.Millisecond
	}
	u.Svc, err = gateway.NewService(gateway.Config{
		Store: u.Store, Workloads: u.Workloads, Authority: u.Management, Credentials: u.Credentials, Provider: u.Provider,
		Estimator: Estimator(t), Decoder: openai.NewDecoder(), Destinations: u.Destination, Tenants: unitTenants{},
		Timeouts: to, Clock: u.Clock.Now,
	})
	must(t, err)
	return u
}

type unitTenants struct{}

func (unitTenants) TenantIDs(context.Context) ([]domain.TenantID, error) {
	return []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID}, nil
}

// Edit is the default enabled settings: one endpoint, the fixture secret, the
// models gpt-a and gpt-b, and roomy limits.
func (u *Unit) Edit() gateway.SettingsEdit {
	return gateway.SettingsEdit{
		Enabled: true, Endpoint: u.Endpoint, Credential: Secret, Models: []string{"gpt-a", "gpt-b"},
		Limits: gateway.Limits{MaxRequests: 10, TokenAllowance: 10000, MaxRequestBytes: 1 << 16, MaxInputTokens: 2000, MaxOutputTokens: 500},
	}
}

// Configure stores the settings of the scope as the admin: the edit is Edit()
// changed by mutate (nil keeps it). It uses the stored revision, so it can be
// called repeatedly.
func (u *Unit) Configure(scope domain.Scope, mutate func(*gateway.SettingsEdit)) gateway.ModelSettings {
	u.T.Helper()
	res, err := u.TryConfigure(scope, mutate)
	must(u.T, err)
	return res.Settings
}

// TryConfigure is Configure returning the error.
func (u *Unit) TryConfigure(scope domain.Scope, mutate func(*gateway.SettingsEdit)) (gateway.SettingsResult, error) {
	cur, err := u.Svc.GetSettings(context.Background(), gateway.Actor{ID: Admin}, scope)
	must(u.T, err)
	edit := u.Edit()
	if mutate != nil {
		mutate(&edit)
	}
	return u.Svc.ReplaceSettings(context.Background(), gateway.Actor{ID: Admin}, scope, cur.Revision, edit, "")
}

// NewAttempt registers an attempt of a new run in scope.
func (u *Unit) NewAttempt(scope domain.Scope) UnitAttempt {
	w, bearer := u.Workloads.Add(scope)
	return UnitAttempt{Workload: w, Bearer: bearer}
}

// NextAttempt registers another attempt of the run of a.
func (u *Unit) NextAttempt(a UnitAttempt) UnitAttempt {
	w, bearer := u.Workloads.AddTo(a.Workload.Scope, a.Workload.RunID)
	return UnitAttempt{Workload: w, Bearer: bearer}
}

// Invoke runs one request as the attempt.
func (u *Unit) Invoke(a UnitAttempt, body []byte, sink gateway.Sink) (gateway.InvokeResult, error) {
	return u.Svc.Invoke(context.Background(), gateway.InvokeRequest{Attempt: a.Workload.AttemptID, Bearer: a.Bearer, Body: body, Sink: sink})
}

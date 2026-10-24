package mcpproxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// The tests in this file drive the real Service through its public API against
// in-memory ports. They prove the protected-use rules (a definitive verdict
// ends the session, an outage denies without a verdict, nothing is cached and
// slow remote work is cancelled the moment current use cannot be confirmed)
// without PostgreSQL. SQL, locking and secrets-pin semantics are proved by the
// real-database suites in integration/ and postgres/.

type fakeStore struct {
	mcpproxy.Store // unimplemented methods fail loudly if a test reaches them

	mu                 sync.Mutex
	reg                mcpproxy.Registration
	sessions           map[mcpproxy.SessionID]mcpproxy.RemoteSession
	saved              []mcpproxy.SessionState
	complete           []mcpproxy.Invocation
	use                func() error
	uses               atomic.Int64
	admitErr           error
	admits             atomic.Int64
	createErr, saveErr error
	// gate, when set, holds SaveSession until it is closed.
	gate chan struct{}
}

func newFakeStore(reg mcpproxy.Registration) *fakeStore {
	return &fakeStore{reg: reg, sessions: map[mcpproxy.SessionID]mcpproxy.RemoteSession{}}
}

func (f *fakeStore) setUse(fn func() error) { f.mu.Lock(); f.use = fn; f.mu.Unlock() }
func (f *fakeStore) setAdmit(err error)     { f.mu.Lock(); f.admitErr = err; f.mu.Unlock() }

func (f *fakeStore) GetRegistration(context.Context, domain.Scope, mcpproxy.RegistrationID) (mcpproxy.Registration, error) {
	return f.reg, nil
}
func (f *fakeStore) EnsureBinding(context.Context, mcpproxy.BindingRequest) (mcpproxy.Registration, error) {
	return f.reg, nil
}
func (f *fakeStore) Recheck(context.Context, mcpproxy.Caller, time.Time) error { return nil }
func (f *fakeStore) CreateSession(_ context.Context, s mcpproxy.RemoteSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.sessions[s.ID] = s
	return nil
}
func (f *fakeStore) GetSession(_ context.Context, _ domain.Scope, id mcpproxy.SessionID) (mcpproxy.RemoteSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return mcpproxy.RemoteSession{}, mcpproxy.ErrNotFound
	}
	return s, nil
}
func (f *fakeStore) SaveSession(_ context.Context, s mcpproxy.RemoteSession, from mcpproxy.SessionState) (bool, error) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil && s.State == mcpproxy.SessionActive {
		return false, f.saveErr
	}
	cur, ok := f.sessions[s.ID]
	if !ok || cur.State != from {
		return false, nil
	}
	f.sessions[s.ID] = s
	f.saved = append(f.saved, s.State)
	return true, nil
}
func (f *fakeStore) CheckSessionUse(context.Context, mcpproxy.SessionUse) error {
	f.uses.Add(1)
	f.mu.Lock()
	fn := f.use
	f.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}
func (f *fakeStore) Admit(_ context.Context, req mcpproxy.AdmitRequest) (mcpproxy.Admission, error) {
	f.admits.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.admitErr != nil {
		return mcpproxy.Admission{}, f.admitErr
	}
	return mcpproxy.Admission{Invocation: req.Invocation}, nil
}
func (f *fakeStore) ExpireSessions(context.Context, domain.TenantID, time.Time, int) (int, error) {
	return 0, nil
}
func (f *fakeStore) RecoverStale(context.Context, domain.TenantID, time.Time, int) (int, error) {
	return 0, nil
}
func (f *fakeStore) Complete(_ context.Context, inv mcpproxy.Invocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.complete = append(f.complete, inv)
	return nil
}

func (f *fakeStore) states() []mcpproxy.SessionState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mcpproxy.SessionState(nil), f.saved...)
}
func (f *fakeStore) completed() []mcpproxy.Invocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mcpproxy.Invocation(nil), f.complete...)
}
func (f *fakeStore) session(id mcpproxy.SessionID) mcpproxy.RemoteSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id]
}

// fakeConn is one remote session. A blocking operation returns only when its
// context ends, as the SDK connection does.
type fakeConn struct {
	blockList atomic.Bool
	blockCall atomic.Bool
	// callErr, when set, is returned by CallTool after the call ran.
	callErr  atomic.Pointer[error]
	protocol atomic.Pointer[string]
	// revokedAtCancel is the revoked count when the blocked call saw its
	// context end: 1 means the credential was erased before the cancellation
	// that makes a real client send a cancel notification.
	revokedAtCancel atomic.Int64
	// onCancel runs once the blocked call has recorded revokedAtCancel.
	onCancel  func()
	listed    atomic.Int64
	called    atomic.Int64
	cancelled atomic.Int64
	closed    atomic.Int64
	revoked   atomic.Int64
	// terminated counts session terminations that would present a credential.
	terminated atomic.Int64
	started    chan struct{}
}

func newFakeConn() *fakeConn { return &fakeConn{started: make(chan struct{}, 16)} }

func (c *fakeConn) Protocol() string {
	if p := c.protocol.Load(); p != nil {
		return *p
	}
	return "2025-11-25"
}
func (c *fakeConn) Revoke() { c.revoked.Add(1) }
func (c *fakeConn) Close() error {
	// A real connection ends the remote session on Close; with its credential
	// revoked that request fails locally. The fake records which it would be.
	if c.revoked.Load() == 0 {
		c.terminated.Add(1)
	}
	c.closed.Add(1)
	return nil
}
func (c *fakeConn) ListTools(ctx context.Context, _ string) (mcpproxy.RemotePage, error) {
	c.listed.Add(1)
	if c.blockList.Load() {
		c.started <- struct{}{}
		<-ctx.Done()
		c.cancelled.Add(1)
		return mcpproxy.RemotePage{}, ctx.Err()
	}
	return mcpproxy.RemotePage{Tools: []mcpproxy.RemoteTool{{Name: "echo"}, {Name: "add"}, {Name: "hidden"}}}, nil
}
func (c *fakeConn) CallTool(ctx context.Context, _ string, _ json.RawMessage) (mcpproxy.RemoteResult, error) {
	c.called.Add(1)
	if c.blockCall.Load() {
		c.started <- struct{}{}
		<-ctx.Done()
		c.revokedAtCancel.Store(c.revoked.Load())
		if c.onCancel != nil {
			c.onCancel()
		}
		c.cancelled.Add(1)
		return mcpproxy.RemoteResult{}, ctx.Err()
	}
	if p := c.callErr.Load(); p != nil {
		return mcpproxy.RemoteResult{}, *p
	}
	return mcpproxy.RemoteResult{JSON: json.RawMessage(`{"ok":true}`)}, nil
}

type fakeRemote struct{ conn *fakeConn }

func (r fakeRemote) Open(context.Context, mcpproxy.RemoteTarget) (mcpproxy.RemoteConn, error) {
	return r.conn, nil
}

type fakeCredential struct{ recipient string }

func (c fakeCredential) Bearer() []byte    { return []byte("bearer") }
func (c fakeCredential) Recipient() string { return c.recipient }
func (c fakeCredential) Discard()          {}

type fakeCredentials struct{ recipient string }

func (c fakeCredentials) Open(context.Context, mcpproxy.CredentialRequest) (mcpproxy.Credential, error) {
	return fakeCredential{recipient: c.recipient}, nil
}

type allowAll struct{}

func (allowAll) Approve(string) error { return nil }
func (allowAll) Authorize(context.Context, string, domain.Scope, mcpproxy.Permission) error {
	return nil
}

type stubRuntime struct{}

func (stubRuntime) Authorize(context.Context, string, domain.AttemptID) (mcpproxy.Caller, error) {
	return caller(), nil
}

type fixedPolicy struct{ p mcpproxy.ToolPolicy }

func (f fixedPolicy) PolicyOf(context.Context, mcpproxy.Caller) (mcpproxy.ToolPolicy, error) {
	return f.p, nil
}

type useEnv struct {
	svc   *mcpproxy.Service
	store *fakeStore
	conn  *fakeConn
	reg   mcpproxy.Registration
	sid   mcpproxy.SessionID
	clock *clockSource
}

type clockSource struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clockSource) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clockSource) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newUseEnv opens one live session over the fakes. The periodic check runs
// every 10ms; a call is bounded at 5s so a missed cancellation fails the test
// by timeout rather than hanging.
func newUseEnv(t *testing.T) *useEnv {
	t.Helper()
	e := newUnopenedEnv(t)
	sid, err := e.svc.OpenSession(context.Background(), caller(), e.reg.ID)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	e.sid = sid
	return e
}

// newUnopenedEnv wires the service without opening a session.
func newUnopenedEnv(t *testing.T) *useEnv {
	t.Helper()
	reg := newReg(t)
	_, u, err := mcpproxy.NormalizeEndpoint(reg.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore(reg)
	conn := newFakeConn()
	var n atomic.Int64
	clock := &clockSource{t: t0}
	svc, err := mcpproxy.NewService(mcpproxy.Config{
		Store: store, Human: allowAll{}, Runtime: stubRuntime{}, Destinations: allowAll{},
		Policy:      fixedPolicy{mcpproxy.NewToolPolicy([]string{mcpproxy.Qualify(reg.ID, "echo"), mcpproxy.Qualify(reg.ID, "add")}, nil, false)},
		Credentials: fakeCredentials{recipient: mcpproxy.Origin(u)},
		Remote:      fakeRemote{conn: conn},
		Options: mcpproxy.Options{
			Protocol: "2025-11-25", Incarnation: "inc-t033-a", SessionTTL: time.Hour, CallTimeout: 5 * time.Second,
			RecheckInterval: 10 * time.Millisecond, StaleAfter: time.Minute,
			MaxArgumentBytes: 1 << 10, MaxResultBytes: 1 << 10, MaxDiscoveryPages: 4,
		},
		Clock: clock.Now,
		NewID: func(prefix string) (string, error) { return fmt.Sprintf("%st033_%016x", prefix, n.Add(1)), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &useEnv{svc: svc, store: store, conn: conn, reg: reg, clock: clock}
}

func (e *useEnv) discover(ctx context.Context) ([]mcpproxy.RemoteTool, error) {
	return e.svc.Discover(ctx, caller(), e.reg.ID, e.sid)
}

func (e *useEnv) invoke(ctx context.Context, id string) (mcpproxy.RemoteResult, error) {
	return e.svc.Invoke(ctx, caller(), e.reg.ID, e.sid, mcpproxy.InvokeRequest{ID: mcpproxy.RequestID(id), Tool: "echo", Arguments: json.RawMessage(`{}`)})
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func Test_discovery_checks_current_use_before_the_remote(t *testing.T) {
	t.Run("a healthy session lists only the policy intersection", func(t *testing.T) {
		e := newUseEnv(t)
		tools, err := e.discover(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "add" {
			t.Fatalf("tools = %+v", tools)
		}
		if e.store.uses.Load() == 0 {
			t.Fatal("discovery never checked current use")
		}
	})

	for _, tc := range []struct {
		name  string
		err   error
		state mcpproxy.SessionState
	}{
		{"a revoked or released credential ends the session", mcpproxy.ErrCredential, mcpproxy.SessionInvalid},
		{"a changed registration revision ends the session", mcpproxy.ErrBindingInvalid, mcpproxy.SessionInvalid},
		{"a vanished binding ends the session", mcpproxy.ErrNotFound, mcpproxy.SessionInvalid},
		{"an invalid stored session ends it", mcpproxy.ErrSessionInvalid, mcpproxy.SessionInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUseEnv(t)
			e.store.setUse(func() error { return tc.err })
			_, err := e.discover(context.Background())
			if err == nil {
				t.Fatal("discovery succeeded on an unusable session")
			}
			if e.conn.listed.Load() != 0 {
				t.Fatal("the remote was contacted with a credential that could not be confirmed")
			}
			if got := e.store.session(e.sid).State; got != tc.state {
				t.Fatalf("session state = %s, want %s", got, tc.state)
			}
			if e.conn.closed.Load() == 0 {
				t.Fatal("the live handle was kept")
			}
			e.noCredentialSent(t)
			// The session is gone for every later use as well: no cached verdict.
			e.store.setUse(nil)
			if _, err := e.discover(context.Background()); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
				t.Fatalf("later discovery = %v, want the same not-found", err)
			}
		})
	}

	t.Run("ended authority denies without recording a session verdict", func(t *testing.T) {
		for _, cause := range []error{mcpproxy.ErrDenied, mcpproxy.ErrUnauthenticated} {
			e := newUseEnv(t)
			e.store.setUse(func() error { return cause })
			if _, err := e.discover(context.Background()); !errors.Is(err, cause) {
				t.Fatalf("err = %v, want %v", err, cause)
			}
			if got := e.store.session(e.sid).State; got != mcpproxy.SessionActive {
				t.Fatalf("authority loss changed the session to %s; it ends with the attempt", got)
			}
			if e.conn.listed.Load() != 0 {
				t.Fatal("the remote was contacted without current authority")
			}
		}
	})

	t.Run("an outage denies this use and never invents a revocation", func(t *testing.T) {
		e := newUseEnv(t)
		e.store.setUse(func() error { return errors.New("connection reset by peer") })
		_, err := e.discover(context.Background())
		if !errors.Is(err, mcpproxy.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		if e.conn.listed.Load() != 0 {
			t.Fatal("the remote was contacted without a conclusive check")
		}
		if got := e.store.session(e.sid).State; got != mcpproxy.SessionActive {
			t.Fatalf("an outage changed the session to %s", got)
		}
		// Recovery of the dependency restores the session: nothing was recorded.
		e.store.setUse(nil)
		if _, err := e.discover(context.Background()); err != nil {
			t.Fatalf("discovery after recovery: %v", err)
		}
	})
}

func Test_slow_discovery_is_cancelled_when_use_cannot_be_confirmed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"credential revoked mid-listing", mcpproxy.ErrCredential, mcpproxy.ErrCredential},
		{"authority ended mid-listing", mcpproxy.ErrDenied, mcpproxy.ErrDenied},
		{"checker outage mid-listing", errors.New("pool exhausted"), mcpproxy.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUseEnv(t)
			e.conn.blockList.Store(true)
			var flip atomic.Bool
			e.store.setUse(func() error {
				if flip.Load() {
					return tc.err
				}
				return nil
			})
			done := make(chan error, 1)
			go func() { _, err := e.discover(context.Background()); done <- err }()
			<-e.conn.started
			flip.Store(true)
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("slow discovery kept running on cached authority")
			}
			if e.conn.cancelled.Load() != 1 {
				t.Fatalf("remote listing observed %d cancellations, want 1", e.conn.cancelled.Load())
			}
		})
	}

	t.Run("a healthy slow listing is bounded by the caller", func(t *testing.T) {
		e := newUseEnv(t)
		e.conn.blockList.Store(true)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := e.discover(ctx); done <- err }()
		<-e.conn.started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, mcpproxy.ErrUnavailable) {
				t.Fatalf("err = %v, want unavailable", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a cancelled caller did not stop discovery")
		}
	})
}

func Test_invocation_is_never_dispatched_without_a_conclusive_admission(t *testing.T) {
	t.Run("a healthy call succeeds and is recorded", func(t *testing.T) {
		e := newUseEnv(t)
		if _, err := e.invoke(context.Background(), "req_t033_ok"); err != nil {
			t.Fatal(err)
		}
		got := e.store.completed()
		if len(got) != 1 || got[0].State != mcpproxy.InvocationSucceeded {
			t.Fatalf("completed = %+v", got)
		}
	})

	for _, tc := range []struct {
		name  string
		err   error
		ended bool
	}{
		{"a revoked credential at admission ends the session", mcpproxy.ErrCredential, true},
		{"a changed binding at admission ends the session", mcpproxy.ErrBindingInvalid, true},
		{"an uncertain admission records no verdict", mcpproxy.ErrUnavailable, false},
		{"ended authority records no verdict", mcpproxy.ErrDenied, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUseEnv(t)
			e.store.setAdmit(tc.err)
			_, err := e.invoke(context.Background(), "req_t033_adm")
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if e.conn.called.Load() != 0 {
				t.Fatal("a call was dispatched after a failed admission")
			}
			if n := len(e.store.completed()); n != 0 {
				t.Fatalf("%d receipts completed for a call that never dispatched", n)
			}
			ended := e.store.session(e.sid).State != mcpproxy.SessionActive
			if ended != tc.ended {
				t.Fatalf("session ended = %v, want %v", ended, tc.ended)
			}
		})
	}
}

func Test_in_flight_call_fails_closed_on_authority_or_checker_failure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
		want error
	}{
		{"credential revoked during the call", mcpproxy.ErrCredential, "authority_ended", mcpproxy.ErrOutcomeUnknown},
		{"authority ended during the call", mcpproxy.ErrDenied, "authority_ended", mcpproxy.ErrOutcomeUnknown},
		{"session expired during the call", mcpproxy.ErrSessionInvalid, "authority_ended", mcpproxy.ErrOutcomeUnknown},
		{"checker outage during the call", errors.New("authority unreachable"), "authority_unavailable", mcpproxy.ErrOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUseEnv(t)
			e.conn.blockCall.Store(true)
			// Ending the session (settle -> drop) revokes again. Hold that back
			// until the call has recorded what it saw at cancellation, so only the
			// revoke that precedes the cancellation can be counted at that moment.
			e.store.mu.Lock()
			e.store.gate = make(chan struct{})
			gate := e.store.gate
			e.store.mu.Unlock()
			var release sync.Once
			e.conn.onCancel = func() { release.Do(func() { close(gate) }) }
			var flip atomic.Bool
			e.store.setUse(func() error {
				if flip.Load() {
					return tc.err
				}
				return nil
			})
			done := make(chan error, 1)
			go func() { _, err := e.invoke(context.Background(), "req_t033_inflight"); done <- err }()
			<-e.conn.started
			flip.Store(true)
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the call kept forwarding on cached authority")
			}
			var oe *mcpproxy.OutcomeError
			if !errors.Is(err, tc.want) || !errors.As(err, &oe) {
				t.Fatalf("err = %v, want a classified unknown outcome", err)
			}
			if oe.Code != tc.code || oe.State != mcpproxy.InvocationUnknown {
				t.Fatalf("outcome = %s/%s, want %s/unknown", oe.Code, oe.State, tc.code)
			}
			if e.conn.cancelled.Load() != 1 {
				t.Fatalf("remote call observed %d cancellations, want 1", e.conn.cancelled.Load())
			}
			// A cancellation notification must not carry a credential that the
			// check just found unusable; an outage proves nothing about it.
			wantRevoked := int64(1)
			if tc.code == "authority_unavailable" {
				wantRevoked = 0
			}
			if got := e.conn.revokedAtCancel.Load(); got != wantRevoked {
				t.Fatalf("credential revoked-at-cancel = %d, want %d", got, wantRevoked)
			}
			// The effect after dispatch is never claimed rolled back or replayed.
			if e.conn.called.Load() != 1 {
				t.Fatalf("remote call ran %d times, want exactly once", e.conn.called.Load())
			}
			got := e.store.completed()
			if len(got) != 1 || got[0].State != mcpproxy.InvocationUnknown || got[0].Code != tc.code {
				t.Fatalf("receipt = %+v, want one unknown/%s", got, tc.code)
			}
		})
	}

	t.Run("a caller cancellation stays a cancellation, not an authority loss", func(t *testing.T) {
		e := newUseEnv(t)
		e.conn.blockCall.Store(true)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := e.invoke(ctx, "req_t033_cancel"); done <- err }()
		<-e.conn.started
		cancel()
		var err error
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the call did not stop")
		}
		var oe *mcpproxy.OutcomeError
		if !errors.As(err, &oe) || oe.Code != "cancelled" {
			t.Fatalf("err = %v, want unknown/cancelled", err)
		}
	})
}

func Test_a_check_answer_is_never_cached(t *testing.T) {
	e := newUseEnv(t)
	for i := 0; i < 3; i++ {
		if _, err := e.discover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.store.uses.Load(); got != 3 {
		t.Fatalf("store consulted %d times for 3 discoveries, want 3", got)
	}
	// One revocation observed by the next use ends it, whatever came before.
	e.store.setUse(func() error { return mcpproxy.ErrCredential })
	if _, err := e.discover(context.Background()); !errors.Is(err, mcpproxy.ErrCredential) {
		t.Fatalf("err = %v, want credential", err)
	}
}

// noCredentialSent asserts the handle was closed after its credential was
// erased, so no request presenting it can have been sent.
func (e *useEnv) noCredentialSent(t *testing.T) {
	t.Helper()
	if e.conn.closed.Load() == 0 {
		t.Fatal("the live handle was kept")
	}
	if e.conn.revoked.Load() != 1 || e.conn.terminated.Load() != 0 {
		t.Fatalf("revoked=%d terminations-with-credential=%d: the credential must be erased before the handle closes",
			e.conn.revoked.Load(), e.conn.terminated.Load())
	}
}

// Test_a_credential_is_presented_only_when_use_was_just_confirmed: the remote
// session is terminated with the credential only by a deliberate close from the
// authorized caller whose current use checks out. Every other end of a session
// closes the handle without sending anything; the remote session lapses.
func Test_a_credential_is_presented_only_when_use_was_just_confirmed(t *testing.T) {
	t.Run("a deliberate close after a successful check terminates the remote session", func(t *testing.T) {
		e := newUseEnv(t)
		if err := e.svc.CloseSession(context.Background(), caller(), e.reg.ID, e.sid); err != nil {
			t.Fatal(err)
		}
		if e.conn.terminated.Load() != 1 || e.conn.revoked.Load() != 0 {
			t.Fatalf("terminated=%d revoked=%d, want a graceful termination", e.conn.terminated.Load(), e.conn.revoked.Load())
		}
		if got := e.store.session(e.sid).State; got != mcpproxy.SessionClosed {
			t.Fatalf("session = %s, want closed", got)
		}
	})

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a credential that ended", mcpproxy.ErrCredential},
		{"a changed registration", mcpproxy.ErrBindingInvalid},
		{"authority that ended", mcpproxy.ErrDenied},
		{"a check that cannot be answered", errors.New("database unavailable")},
	} {
		t.Run("a deliberate close still closes but sends nothing: "+tc.name, func(t *testing.T) {
			e := newUseEnv(t)
			e.store.setUse(func() error { return tc.err })
			if err := e.svc.CloseSession(context.Background(), caller(), e.reg.ID, e.sid); err != nil {
				t.Fatalf("closing must succeed even when it cannot be confirmed: %v", err)
			}
			e.noCredentialSent(t)
		})
	}

	t.Run("expiry and sweep send nothing", func(t *testing.T) {
		e := newUseEnv(t)
		e.clock.Advance(2 * time.Hour) // past the one-hour lifetime
		if err := e.svc.Sweep(context.Background(), caller().Scope.TenantID, 10); err != nil {
			t.Fatal(err)
		}
		e.noCredentialSent(t)
	})

	t.Run("a session found expired on use sends nothing", func(t *testing.T) {
		e := newUseEnv(t)
		e.clock.Advance(2 * time.Hour)
		if _, err := e.discover(context.Background()); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("err = %v, want the same not-found", err)
		}
		e.noCredentialSent(t)
	})

	t.Run("shutdown sends nothing", func(t *testing.T) {
		e := newUseEnv(t)
		e.svc.Shutdown()
		e.noCredentialSent(t)
	})
}

// Test_remote_failure_classes: a protocol error reply is a final failure; any
// other loss after dispatch leaves the effect unknown. Neither is retried.
func Test_remote_failure_classes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		kind  error
		state mcpproxy.InvocationState
		code  string
	}{
		{"a protocol error reply", &mcpproxy.RemoteFailure{Reply: true, Code: "invalid_params"}, mcpproxy.ErrRemoteFailed, mcpproxy.InvocationFailed, "invalid_params"},
		{"an oversized remote code is bounded", &mcpproxy.RemoteFailure{Reply: true, Code: "Not A Safe Token!"}, mcpproxy.ErrRemoteFailed, mcpproxy.InvocationFailed, "remote_error"},
		{"a lost connection", errors.New("connection reset"), mcpproxy.ErrOutcomeUnknown, mcpproxy.InvocationUnknown, "transport"},
		{"a transport failure without a reply", &mcpproxy.RemoteFailure{Reply: false, Code: "eof"}, mcpproxy.ErrOutcomeUnknown, mcpproxy.InvocationUnknown, "eof"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUseEnv(t)
			err := tc.err
			e.conn.callErr.Store(&err)
			_, got := e.invoke(context.Background(), "req_t033_class")
			var oe *mcpproxy.OutcomeError
			if !errors.Is(got, tc.kind) || !errors.As(got, &oe) || oe.State != tc.state || oe.Code != tc.code {
				t.Fatalf("got %v, want %v/%s/%s", got, tc.kind, tc.state, tc.code)
			}
			if e.conn.called.Load() != 1 {
				t.Fatalf("the remote ran %d times, want exactly once", e.conn.called.Load())
			}
			rec := e.store.completed()
			if len(rec) != 1 || rec[0].State != tc.state {
				t.Fatalf("receipt = %+v, want one %s", rec, tc.state)
			}
		})
	}
}

// Test_failed_initialization_presents_no_credential: a connection that never
// became a live session is closed without presenting the credential, whatever
// step failed after it was opened.
func Test_failed_initialization_presents_no_credential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		breaks func(e *useEnv)
	}{
		{"a remote that negotiated another protocol", func(e *useEnv) { p := "2024-11-05"; e.conn.protocol.Store(&p) }},
		{"a session that could not be stored", func(e *useEnv) {
			e.store.mu.Lock()
			e.store.createErr = errors.New("insert failed")
			e.store.mu.Unlock()
		}},
		{"a session that could not be activated", func(e *useEnv) { e.store.mu.Lock(); e.store.saveErr = errors.New("update failed"); e.store.mu.Unlock() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUnopenedEnv(t)
			tc.breaks(e)
			if _, err := e.svc.OpenSession(context.Background(), caller(), e.reg.ID); err == nil {
				t.Fatal("initialization succeeded")
			}
			if e.conn.closed.Load() != 1 || e.conn.revoked.Load() != 1 || e.conn.terminated.Load() != 0 {
				t.Fatalf("closed=%d revoked=%d terminated-with-credential=%d: the credential must be erased before the handle closes",
					e.conn.closed.Load(), e.conn.revoked.Load(), e.conn.terminated.Load())
			}
		})
	}
}

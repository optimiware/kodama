// Package mcpcontract is the one exported contract suite of the MCP proxy's
// persistence port, mcpproxy.Store (SD-13): every adapter runs it. The memory
// adapter (mcptest/memory) runs it in the unit layer with no service; the
// PostgreSQL adapter runs it behind layer.SkipService in the `test`
// environment (mcptest.PostgresHarness).
//
// The suite asserts what callers rely on: tenant and project isolation and
// not-found versus forbidden, unique and conflict outcomes, expected-version
// edits and tombstones, bindings frozen at the first authorized use, one
// receipt per request ID, the run-wide call ceiling, final outcomes, and a
// protected-use check that ends on a revoked credential, a released pin, an
// edited registration or an ended attempt while an outage of the checker proves
// nothing. It does not assert locking, lock order, isolation levels, SQL or
// RLS: those belong to the PostgreSQL checkpoints.
package mcpcontract

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// Admin is the actor of registration changes.
const Admin = "u_contract_admin"

// Endpoint is the approved remote the registrations of the suite name.
const Endpoint = "https://mcp.example.test/mcp"

// Harness is what an adapter provides: the store under test over its own
// persistence, current runtime authority and secrets, with primitives that end
// or change them.
type Harness interface {
	Store() mcpproxy.Store
	Checker() mcpproxy.SecretChecker
	// Scope is the primary project; Foreign is a project of another tenant.
	Scope() domain.Scope
	Foreign() domain.Scope
	// Incarnation is the process that owns live handles.
	Incarnation() string
	Now() time.Time
	Advance(t testing.TB, d time.Duration)

	// NewWorkload admits a fresh attempt of a fresh run in the scope with a
	// live runtime grant and returns its verified caller. NewAttempt admits
	// another attempt of the same run with its own grant session.
	NewWorkload(t testing.TB, scope domain.Scope) mcpproxy.Caller
	NewAttempt(t testing.TB, c mcpproxy.Caller) mcpproxy.Caller
	// EndAttempt ends the attempt's authority for good.
	EndAttempt(t testing.TB, c mcpproxy.Caller)
	// AuthorityOutage turns an outage of the runtime-authority check on or off.
	AuthorityOutage(t testing.TB, on bool)

	// Credential creates an MCP-kind secret with exact version 1 in the scope.
	Credential(t testing.TB, scope domain.Scope, name string) mcpproxy.SecretRef
	// RotateCredential adds a version and returns its exact reference.
	RotateCredential(t testing.TB, scope domain.Scope, name string) mcpproxy.SecretRef
	RevokeCredential(t testing.TB, scope domain.Scope, ref mcpproxy.SecretRef)
	// ReleasePins releases the run's secret pins (the run ended).
	ReleasePins(t testing.TB, c mcpproxy.Caller)
	// Audit counts the audit entries with the action in the scope's tenant.
	Audit(t testing.TB, scope domain.Scope, action string) int
}

// Open builds a fresh, isolated Harness for one subtest.
type Open func(testing.TB) Harness

// Contract runs the shared MCP proxy store contract.
func Contract(t *testing.T, open Open) {
	t.Helper()
	t.Run("registrations are tenant scoped, versioned and tombstoned", func(t *testing.T) { registrations(t, open(t)) })
	t.Run("returned and supplied registrations are detached from the store", func(t *testing.T) { detached(t, open(t)) })
	t.Run("the run binding freezes the first authorized use", func(t *testing.T) { bindings(t, open(t)) })
	t.Run("a session belongs to exactly one caller", func(t *testing.T) { sessions(t, open(t)) })
	t.Run("current use ends with the credential, the pin, the revision or the attempt", func(t *testing.T) { currentUse(t, open(t)) })
	t.Run("admission is idempotent per request and bounded per run", func(t *testing.T) { admission(t, open(t)) })
	t.Run("outcomes are final and stale receipts become unknown", func(t *testing.T) { outcomes(t, open(t)) })
}

var bg = context.Background()

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func actor() mcpproxy.Actor { return mcpproxy.Actor{ID: Admin} }

func mutation(h Harness, req string) mcpproxy.Mutation {
	return mcpproxy.Mutation{Actor: actor(), RequestID: req, Now: h.Now()}
}

func newReg(t testing.TB, h Harness, scope domain.Scope, id string, ref mcpproxy.SecretRef, tools ...string) mcpproxy.Registration {
	t.Helper()
	r, err := mcpproxy.NewRegistration(scope, mcpproxy.RegistrationID(id), mcpproxy.Draft{Endpoint: Endpoint, Secret: ref, Tools: tools}, actor(), h.Now())
	must(t, err)
	return r
}

func authorityEnded(err error) bool {
	return errors.Is(err, mcpproxy.ErrDenied) || errors.Is(err, mcpproxy.ErrUnauthenticated)
}

// ---- registrations ------------------------------------------------------------------

func registrations(t *testing.T, h Harness) {
	st, scope, foreign := h.Store(), h.Scope(), h.Foreign()
	ref := h.Credential(t, scope, "remote-token")
	fref := h.Credential(t, foreign, "foreign-token")

	r1 := newReg(t, h, scope, "mcp_c1", ref, "b", "a")
	must(t, st.CreateRegistration(bg, mutation(h, "req_c1"), r1))
	if err := st.CreateRegistration(bg, mutation(h, "req_c1b"), r1); !errors.Is(err, mcpproxy.ErrConflict) {
		t.Fatalf("a duplicate registration: %v", err)
	}
	got, err := st.GetRegistration(bg, scope, r1.ID)
	if err != nil || got.ID != r1.ID || got.Version != 1 || got.Endpoint != r1.Endpoint || got.Secret != ref || len(got.Tools) != 2 || got.Tools[0] != "a" {
		t.Fatalf("get: %+v %v", got, err)
	}
	// Another tenant never sees it, and has its own.
	if _, err := st.GetRegistration(bg, foreign, r1.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Fatalf("a foreign read: %v", err)
	}
	fr := newReg(t, h, foreign, "mcp_c9", fref, "a")
	must(t, st.CreateRegistration(bg, mutation(h, "req_c9"), fr))
	if _, err := st.GetRegistration(bg, scope, fr.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Fatalf("a foreign registration is visible: %v", err)
	}
	if got, err := st.ListRegistrations(bg, foreign, "", 10); err != nil || len(got) != 1 || got[0].ID != fr.ID {
		t.Fatalf("foreign list: %v %v", got, err)
	}
	if err := h.Checker().Check(bg, scope, fref); !errors.Is(err, mcpproxy.ErrCredential) {
		t.Fatalf("another tenant's secret passed the check: %v", err)
	}
	if err := h.Checker().Check(bg, scope, ref); err != nil {
		t.Fatalf("a live MCP secret: %v", err)
	}
	if err := h.Checker().Check(bg, scope, mcpproxy.SecretRef{Name: ref.Name, Version: ref.Version + 1}); !errors.Is(err, mcpproxy.ErrCredential) {
		t.Fatalf("a version that does not exist: %v", err)
	}

	for _, id := range []string{"mcp_c3", "mcp_c2"} {
		must(t, st.CreateRegistration(bg, mutation(h, "req_"+id), newReg(t, h, scope, id, ref, "a")))
	}
	page, err := st.ListRegistrations(bg, scope, "", 2)
	if err != nil || len(page) != 2 || page[0].ID != "mcp_c1" || page[1].ID != "mcp_c2" {
		t.Fatalf("first page: %v %v", page, err)
	}
	if page, err = st.ListRegistrations(bg, scope, "mcp_c2", 2); err != nil || len(page) != 1 || page[0].ID != "mcp_c3" {
		t.Fatalf("second page: %v %v", page, err)
	}

	// Expected-version edits.
	v2, err := r1.Edit(1, mcpproxy.Draft{Endpoint: Endpoint, Secret: ref, Tools: []string{"a", "b", "c"}}, actor(), h.Now())
	must(t, err)
	must(t, st.ReplaceRegistration(bg, mutation(h, "req_edit"), v2, 1))
	if got, _ := st.GetRegistration(bg, scope, r1.ID); got.Version != 2 || len(got.Tools) != 3 {
		t.Fatalf("after the edit: %+v", got)
	}
	if err := st.ReplaceRegistration(bg, mutation(h, "req_stale"), v2, 1); !errors.Is(err, mcpproxy.ErrVersionConflict) {
		t.Fatalf("a stale expected version: %v", err)
	}
	if err := st.ReplaceRegistration(bg, mutation(h, "req_skip"), newReg(t, h, scope, "mcp_c1", ref), 7); !errors.Is(err, mcpproxy.ErrInvalid) {
		t.Fatalf("a non-successor: %v", err)
	}
	// An edit by a caller of another tenant changes nothing.
	f2 := v2
	f2.Scope, f2.Version = foreign, 3
	if err := st.ReplaceRegistration(bg, mutation(h, "req_foreign"), f2, 2); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Fatalf("a foreign edit: %v", err)
	}

	// Deletion is an irreversible tombstone that denies further use.
	v3, err := v2.Delete(2, actor(), h.Now())
	must(t, err)
	must(t, st.ReplaceRegistration(bg, mutation(h, "req_del"), v3, 2))
	tomb, err := st.GetRegistration(bg, scope, r1.ID)
	if err != nil || !tomb.Deleted() || tomb.Version != 3 {
		t.Fatalf("the tombstone: %+v %v", tomb, err)
	}
	if live, _ := st.ListRegistrations(bg, scope, "", 10); len(live) != 2 {
		t.Fatalf("the list contains a tombstone: %v", live)
	}
	v4 := v3
	v4.Version = 4
	if err := st.ReplaceRegistration(bg, mutation(h, "req_revive"), v4, 3); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Fatalf("editing a tombstone: %v", err)
	}
	for action, n := range map[string]int{"mcp_server.created": 3, "mcp_server.updated": 1, "mcp_server.deleted": 1} {
		if got := h.Audit(t, scope, action); got != n {
			t.Fatalf("audit %s: %d, want %d", action, got, n)
		}
	}
}

// ---- bindings and sessions -----------------------------------------------------------

type live struct {
	caller mcpproxy.Caller
	reg    mcpproxy.Registration
	ref    mcpproxy.SecretRef
	sess   mcpproxy.RemoteSession
}

var seq atomic.Int64

func sid() mcpproxy.SessionID {
	return mcpproxy.SessionID(fmt.Sprintf("mcs_contract_%08d", seq.Add(1)))
}

// registered creates a registration of a fresh secret in the scope.
func registered(t *testing.T, h Harness, scope domain.Scope, tools ...string) (mcpproxy.Registration, mcpproxy.SecretRef) {
	t.Helper()
	n := seq.Add(1)
	ref := h.Credential(t, scope, fmt.Sprintf("remote-token-%d", n))
	r := newReg(t, h, scope, fmt.Sprintf("mcp_contract_%d", n), ref, tools...)
	must(t, h.Store().CreateRegistration(bg, mutation(h, "req_reg"), r))
	return r, ref
}

func newSession(t *testing.T, h Harness, c mcpproxy.Caller, reg mcpproxy.Registration) mcpproxy.RemoteSession {
	t.Helper()
	s, err := mcpproxy.NewRemoteSession(sid(), c, reg, "2025-11-25", h.Incarnation(), h.Now(), time.Hour)
	must(t, err)
	return s
}

// liveSession is a caller with a frozen binding and an active session.
func liveSession(t *testing.T, h Harness, tools ...string) live {
	t.Helper()
	if len(tools) == 0 {
		tools = []string{"echo"}
	}
	c := h.NewWorkload(t, h.Scope())
	reg, ref := registered(t, h, h.Scope(), tools...)
	frozen, err := h.Store().EnsureBinding(bg, mcpproxy.BindingRequest{Caller: c, Registration: reg.ID, Now: h.Now()})
	must(t, err)
	if frozen.Version != reg.Version || frozen.Secret != ref {
		t.Fatalf("the binding froze %+v", frozen)
	}
	sess := newSession(t, h, c, frozen)
	must(t, h.Store().CreateSession(bg, sess))
	must(t, sess.Activate(h.Now()))
	if ok, err := h.Store().SaveSession(bg, sess, mcpproxy.SessionInitializing); err != nil || !ok {
		t.Fatalf("activate: %v %v", ok, err)
	}
	return live{caller: c, reg: reg, ref: ref, sess: sess}
}

func use(h Harness, l live) mcpproxy.SessionUse {
	return mcpproxy.SessionUse{Caller: l.caller, Registration: l.reg.ID, Session: l.sess.ID, Incarnation: h.Incarnation(), Now: h.Now()}
}

func bindings(t *testing.T, h Harness) {
	st := h.Store()
	c := h.NewWorkload(t, h.Scope())
	reg, ref := registered(t, h, h.Scope(), "echo")

	// No binding, no session.
	pre := newSession(t, h, c, reg)
	if err := st.CreateSession(bg, pre); !errors.Is(err, mcpproxy.ErrBindingInvalid) {
		t.Fatalf("a session without a binding: %v", err)
	}

	got, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: c, Registration: reg.ID, Now: h.Now()})
	if err != nil || got.Version != 1 || got.Secret != ref {
		t.Fatalf("first use: %+v %v", got, err)
	}
	if again, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: c, Registration: reg.ID, Now: h.Now()}); err != nil || again.Version != 1 {
		t.Fatalf("a repeat of the same use: %v", err)
	}
	if n := h.Audit(t, h.Scope(), "mcp_server.credential_bound"); n != 1 {
		t.Fatalf("%d bound audit entries, want 1", n)
	}
	// A session that names another frozen revision is not the binding's.
	bad := pre
	bad.RegistrationVersion = 2
	if err := st.CreateSession(bg, bad); !errors.Is(err, mcpproxy.ErrBindingInvalid) {
		t.Fatalf("a session naming another revision: %v", err)
	}

	// Another attempt of the same run shares the run's binding.
	if _, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: h.NewAttempt(t, c), Registration: reg.ID, Now: h.Now()}); err != nil {
		t.Fatalf("another attempt of the run: %v", err)
	}

	// A change after the first use invalidates the binding; it is never replaced.
	next, err := reg.Edit(1, mcpproxy.Draft{Endpoint: Endpoint, Secret: ref, Tools: []string{"echo", "add"}}, actor(), h.Now())
	must(t, err)
	must(t, st.ReplaceRegistration(bg, mutation(h, "req_edit"), next, 1))
	if _, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: c, Registration: reg.ID, Now: h.Now()}); !errors.Is(err, mcpproxy.ErrBindingInvalid) {
		t.Fatalf("the binding of an edited registration: %v", err)
	}
	// A new run freezes the new revision.
	c2 := h.NewWorkload(t, h.Scope())
	if got, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: c2, Registration: reg.ID, Now: h.Now()}); err != nil || got.Version != 2 {
		t.Fatalf("a new run: %+v %v", got, err)
	}

	// A deleted registration cannot be bound; a foreign one is not found.
	tomb, err := next.Delete(2, actor(), h.Now())
	must(t, err)
	must(t, st.ReplaceRegistration(bg, mutation(h, "req_del"), tomb, 2))
	if _, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: h.NewWorkload(t, h.Scope()), Registration: reg.ID, Now: h.Now()}); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Fatalf("a deleted registration: %v", err)
	}
	reg2, _ := registered(t, h, h.Scope(), "echo")
	fc := h.NewWorkload(t, h.Foreign())
	if _, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: fc, Registration: reg2.ID, Now: h.Now()}); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Fatalf("another tenant's registration: %v", err)
	}

	// A revoked attempt freezes nothing.
	ended := h.NewWorkload(t, h.Scope())
	reg3, _ := registered(t, h, h.Scope(), "echo")
	h.EndAttempt(t, ended)
	if _, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: ended, Registration: reg3.ID, Now: h.Now()}); !authorityEnded(err) {
		t.Fatalf("an ended attempt froze a binding: %v", err)
	}
	if err := st.CreateSession(bg, newSession(t, h, ended, reg3)); !errors.Is(err, mcpproxy.ErrBindingInvalid) {
		t.Fatalf("a session for the ended attempt: %v", err)
	}
}

func sessions(t *testing.T, h Harness) {
	st := h.Store()
	l := liveSession(t, h)

	got, err := st.GetSession(bg, l.caller.Scope, l.sess.ID)
	if err != nil || got.State != mcpproxy.SessionActive || got.Caller != l.caller || got.Registration != l.reg.ID || got.Incarnation != h.Incarnation() {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := st.GetSession(bg, h.Foreign(), l.sess.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Fatalf("a foreign read: %v", err)
	}
	dup := l.sess
	dup.State = mcpproxy.SessionInitializing
	if err := st.CreateSession(bg, dup); !errors.Is(err, mcpproxy.ErrConflict) {
		t.Fatalf("a duplicate session: %v", err)
	}
	// A state change applies once, from the state it names.
	closed := got
	must(t, closed.Close(h.Now()))
	if ok, err := st.SaveSession(bg, closed, mcpproxy.SessionInitializing); err != nil || ok {
		t.Fatalf("a transition from the wrong state: %v %v", ok, err)
	}
	if ok, err := st.SaveSession(bg, closed, mcpproxy.SessionActive); err != nil || !ok {
		t.Fatalf("close: %v %v", ok, err)
	}
	if ok, err := st.SaveSession(bg, closed, mcpproxy.SessionActive); err != nil || ok {
		t.Fatalf("a repeated close: %v %v", ok, err)
	}
	if cur, _ := st.GetSession(bg, l.caller.Scope, l.sess.ID); cur.State != mcpproxy.SessionClosed {
		t.Fatalf("state %s", cur.State)
	}
	if err := st.CheckSessionUse(bg, use(h, l)); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
		t.Fatalf("a closed session: %v", err)
	}

	// Expiry: only live sessions past their lifetime are expired.
	e := liveSession(t, h)
	h.Advance(t, 2*time.Hour)
	n, err := st.ExpireSessions(bg, e.caller.Scope.TenantID, h.Now(), 100)
	if err != nil || n < 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if cur, _ := st.GetSession(bg, e.caller.Scope, e.sess.ID); cur.State != mcpproxy.SessionExpired {
		t.Fatalf("state %s", cur.State)
	}
	if cur, _ := st.GetSession(bg, l.caller.Scope, l.sess.ID); cur.State != mcpproxy.SessionClosed {
		t.Fatalf("a terminal session was rewritten: %s", cur.State)
	}
	if n, err := st.ExpireSessions(bg, e.caller.Scope.TenantID, h.Now(), 100); err != nil || n != 0 {
		t.Fatalf("a repeated sweep expired %d: %v", n, err)
	}
}

// ---- protected use -------------------------------------------------------------------

func currentUse(t *testing.T, h Harness) {
	st := h.Store()

	t.Run("a live session passes and nothing is cached", func(t *testing.T) {
		l := liveSession(t, h)
		for i := 0; i < 2; i++ {
			if err := st.CheckSessionUse(bg, use(h, l)); err != nil {
				t.Fatalf("check %d: %v", i, err)
			}
		}
		// A rotated secret does not touch what the run froze.
		h.RotateCredential(t, l.caller.Scope, l.ref.Name)
		if err := st.CheckSessionUse(bg, use(h, l)); err != nil {
			t.Fatalf("after a rotation: %v", err)
		}
	})
	t.Run("only the exact caller", func(t *testing.T) {
		l := liveSession(t, h)
		other := use(h, l)
		other.Caller = h.NewAttempt(t, l.caller)
		if err := st.CheckSessionUse(bg, other); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("another attempt of the run: %v", err)
		}
		foreign := use(h, l)
		foreign.Caller = h.NewWorkload(t, h.Foreign())
		if err := st.CheckSessionUse(bg, foreign); !errors.Is(err, mcpproxy.ErrNotFound) && !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("a foreign tenant's caller: %v", err)
		}
		wrongProc := use(h, l)
		wrongProc.Incarnation = "inc-someone-else"
		if err := st.CheckSessionUse(bg, wrongProc); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("another process: %v", err)
		}
		wrongReg := use(h, l)
		wrongReg.Registration = "mcp_other"
		if err := st.CheckSessionUse(bg, wrongReg); !errors.Is(err, mcpproxy.ErrNotFound) && !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("another registration: %v", err)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		l := liveSession(t, h)
		h.Advance(t, 2*time.Hour)
		if err := st.CheckSessionUse(bg, use(h, l)); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("an expired session: %v", err)
		}
	})
	t.Run("a revoked credential", func(t *testing.T) {
		l := liveSession(t, h)
		h.RevokeCredential(t, l.caller.Scope, l.ref)
		if err := st.CheckSessionUse(bg, use(h, l)); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("a revoked credential: %v", err)
		}
	})
	t.Run("a released pin", func(t *testing.T) {
		l := liveSession(t, h)
		h.ReleasePins(t, l.caller)
		if err := st.CheckSessionUse(bg, use(h, l)); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("a released pin: %v", err)
		}
	})
	t.Run("an edited registration", func(t *testing.T) {
		l := liveSession(t, h)
		next, err := l.reg.Edit(1, mcpproxy.Draft{Endpoint: Endpoint, Secret: l.ref, Tools: []string{"echo", "more"}}, actor(), h.Now())
		must(t, err)
		must(t, st.ReplaceRegistration(bg, mutation(h, "req_edit"), next, 1))
		if err := st.CheckSessionUse(bg, use(h, l)); !errors.Is(err, mcpproxy.ErrSessionInvalid) && !errors.Is(err, mcpproxy.ErrBindingInvalid) {
			t.Fatalf("an edited registration: %v", err)
		}
		if cur, _ := st.GetSession(bg, l.caller.Scope, l.sess.ID); cur.State != mcpproxy.SessionInvalid {
			t.Fatalf("the edit must invalidate the live sessions of the old revision: %s", cur.State)
		}
	})
	t.Run("an ended attempt", func(t *testing.T) {
		l := liveSession(t, h)
		h.EndAttempt(t, l.caller)
		if err := st.CheckSessionUse(bg, use(h, l)); !authorityEnded(err) {
			t.Fatalf("an ended attempt: %v", err)
		}
		// Authority loss is not a session verdict: the session ends with the attempt.
		if cur, _ := st.GetSession(bg, l.caller.Scope, l.sess.ID); cur.State != mcpproxy.SessionActive {
			t.Fatalf("authority loss rewrote the session: %s", cur.State)
		}
	})
	t.Run("an outage proves nothing", func(t *testing.T) {
		l := liveSession(t, h)
		h.AuthorityOutage(t, true)
		err := st.CheckSessionUse(bg, use(h, l))
		if !errors.Is(err, mcpproxy.ErrUnavailable) || errors.Is(err, mcpproxy.ErrCredential) || errors.Is(err, mcpproxy.ErrSessionInvalid) || authorityEnded(err) {
			t.Fatalf("an outage: %v", err)
		}
		h.AuthorityOutage(t, false)
		if err := st.CheckSessionUse(bg, use(h, l)); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	})
}

// ---- admission and outcomes ------------------------------------------------------------

var rid atomic.Int64

func dispatching(t *testing.T, h Harness, l live, tool string) mcpproxy.Invocation {
	t.Helper()
	inv, err := mcpproxy.NewInvocation(mcpproxy.RequestID(fmt.Sprintf("req_contract_%d", rid.Add(1))), l.sess, tool, h.Now())
	must(t, err)
	must(t, inv.Dispatch(h.Now()))
	return inv
}

func admit(h Harness, l live, inv mcpproxy.Invocation, max *int64) (mcpproxy.Admission, error) {
	return h.Store().Admit(bg, mcpproxy.AdmitRequest{Invocation: inv, Incarnation: h.Incarnation(), MaxCalls: max, Now: h.Now()})
}

func admission(t *testing.T, h Harness) {
	st := h.Store()
	l := liveSession(t, h)

	inv := dispatching(t, h, l, "echo")
	adm, err := admit(h, l, inv, nil)
	if err != nil || adm.Duplicate || adm.Invocation.ID != inv.ID {
		t.Fatalf("first admission: %+v %v", adm, err)
	}
	rec, err := st.GetInvocation(bg, l.caller.Scope, l.sess.ID, inv.ID)
	if err != nil || rec.State != mcpproxy.InvocationDispatching || rec.Tool != "echo" || rec.Caller.RunID != l.caller.RunID {
		t.Fatalf("receipt: %+v %v", rec, err)
	}
	// A repeated request never dispatches again and never charges.
	for i := 0; i < 2; i++ {
		dup, err := admit(h, l, inv, nil)
		if err != nil || !dup.Duplicate || dup.Invocation.State != mcpproxy.InvocationDispatching {
			t.Fatalf("duplicate %d: %+v %v", i, dup, err)
		}
	}
	if n := h.Audit(t, l.caller.Scope, "mcp_tool.admitted"); n != 1 {
		t.Fatalf("%d admission audit entries for one call", n)
	}
	if _, err := st.GetInvocation(bg, h.Foreign(), l.sess.ID, inv.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Fatalf("a foreign receipt read: %v", err)
	}
	// Only a dispatching receipt is admitted.
	notSent, err := mcpproxy.NewInvocation(mcpproxy.RequestID(fmt.Sprintf("req_contract_%d", rid.Add(1))), l.sess, "echo", h.Now())
	must(t, err)
	if _, err := admit(h, l, notSent, nil); !errors.Is(err, mcpproxy.ErrInvalid) {
		t.Fatalf("an admitted-but-not-dispatching receipt: %v", err)
	}

	t.Run("the run-wide ceiling", func(t *testing.T) {
		l := liveSession(t, h)
		max := int64(2)
		a, b, c := dispatching(t, h, l, "echo"), dispatching(t, h, l, "echo"), dispatching(t, h, l, "echo")
		for _, inv := range []mcpproxy.Invocation{a, b} {
			if adm, err := admit(h, l, inv, &max); err != nil || adm.Duplicate {
				t.Fatalf("within the ceiling: %+v %v", adm, err)
			}
		}
		if _, err := admit(h, l, c, &max); !errors.Is(err, mcpproxy.ErrLimitReached) {
			t.Fatalf("over the ceiling: %v", err)
		}
		if _, err := st.GetInvocation(bg, l.caller.Scope, l.sess.ID, c.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("a refused call left a receipt: %v", err)
		}
		// A replay of an admitted call is a duplicate, not a charge.
		if adm, err := admit(h, l, b, &max); err != nil || !adm.Duplicate {
			t.Fatalf("a replay at the ceiling: %+v %v", adm, err)
		}
		if n := h.Audit(t, l.caller.Scope, "mcp_tool.admitted"); n < 3 {
			t.Fatalf("audit %d", n)
		}
		// The ceiling is per run: another run has its own.
		other := liveSession(t, h)
		one := int64(1)
		if _, err := admit(h, other, dispatching(t, h, other, "echo"), &one); err != nil {
			t.Fatalf("another run: %v", err)
		}
	})
	t.Run("nothing is admitted or charged without current authority", func(t *testing.T) {
		for name, break_ := range map[string]func(live){
			"revoked credential": func(l live) { h.RevokeCredential(t, l.caller.Scope, l.ref) },
			"released pin":       func(l live) { h.ReleasePins(t, l.caller) },
			"ended attempt":      func(l live) { h.EndAttempt(t, l.caller) },
		} {
			l := liveSession(t, h)
			break_(l)
			one := int64(1)
			inv := dispatching(t, h, l, "echo")
			if _, err := admit(h, l, inv, &one); err == nil {
				t.Fatalf("%s: a call was admitted", name)
			}
			if _, err := st.GetInvocation(bg, l.caller.Scope, l.sess.ID, inv.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
				t.Fatalf("%s: a refused call left a receipt: %v", name, err)
			}
		}
		l := liveSession(t, h)
		h.AuthorityOutage(t, true)
		defer h.AuthorityOutage(t, false)
		inv := dispatching(t, h, l, "echo")
		if _, err := admit(h, l, inv, nil); !errors.Is(err, mcpproxy.ErrUnavailable) {
			t.Fatalf("an authority outage admitted a call: %v", err)
		}
		h.AuthorityOutage(t, false)
		if _, err := st.GetInvocation(bg, l.caller.Scope, l.sess.ID, inv.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("an outage left a receipt: %v", err)
		}
	})
	t.Run("another caller cannot admit on the session", func(t *testing.T) {
		l := liveSession(t, h)
		inv := dispatching(t, h, l, "echo")
		stolen := inv
		stolen.Caller = h.NewAttempt(t, l.caller)
		if _, err := admit(h, l, stolen, nil); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("a copied session: %v", err)
		}
	})
}

func outcomes(t *testing.T, h Harness) {
	st := h.Store()
	l := liveSession(t, h)
	finish := func(inv mcpproxy.Invocation, apply func(*mcpproxy.Invocation, time.Time) error) mcpproxy.Invocation {
		t.Helper()
		must(t, apply(&inv, h.Now()))
		return inv
	}

	ok := dispatching(t, h, l, "echo")
	fail := dispatching(t, h, l, "echo")
	unk := dispatching(t, h, l, "echo")
	for _, inv := range []mcpproxy.Invocation{ok, fail, unk} {
		if _, err := admit(h, l, inv, nil); err != nil {
			t.Fatal(err)
		}
	}
	done := map[string]mcpproxy.Invocation{
		"succeeded": finish(ok, func(i *mcpproxy.Invocation, n time.Time) error { return i.Succeed(n) }),
		"failed":    finish(fail, func(i *mcpproxy.Invocation, n time.Time) error { return i.Fail("remote_error", n) }),
		"unknown":   finish(unk, func(i *mcpproxy.Invocation, n time.Time) error { return i.MarkUnknown("transport", n) }),
	}
	for want, inv := range done {
		must(t, st.Complete(bg, inv))
		got, err := st.GetInvocation(bg, l.caller.Scope, l.sess.ID, inv.ID)
		if err != nil || string(got.State) != want || got.Code != inv.Code {
			t.Fatalf("%s: %+v %v", want, got, err)
		}
		// Final: nothing rewrites a terminal receipt, not even the same outcome.
		if err := st.Complete(bg, inv); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
			t.Fatalf("%s completed twice: %v", want, err)
		}
	}
	if n := h.Audit(t, l.caller.Scope, "mcp_tool.completed"); n != 3 {
		t.Fatalf("%d completion audit entries", n)
	}
	// Only a terminal outcome of a receipt that exists is recorded.
	pending := dispatching(t, h, l, "echo")
	if err := st.Complete(bg, pending); !errors.Is(err, mcpproxy.ErrInvalid) {
		t.Fatalf("a non-terminal outcome: %v", err)
	}
	ghost := finish(pending, func(i *mcpproxy.Invocation, n time.Time) error { return i.Succeed(n) })
	if err := st.Complete(bg, ghost); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
		t.Fatalf("an outcome for a call that was never admitted: %v", err)
	}

	t.Run("recovery of stale receipts", func(t *testing.T) {
		l := liveSession(t, h)
		stale, settled := dispatching(t, h, l, "echo"), dispatching(t, h, l, "echo")
		for _, inv := range []mcpproxy.Invocation{stale, settled} {
			if _, err := admit(h, l, inv, nil); err != nil {
				t.Fatal(err)
			}
		}
		h.Advance(t, 10*time.Minute)
		must(t, st.Complete(bg, finish(settled, func(i *mcpproxy.Invocation, n time.Time) error { return i.Succeed(n) })))
		n, err := st.RecoverStale(bg, l.caller.Scope.TenantID, h.Now().Add(-time.Minute), 100)
		if err != nil || n != 1 {
			t.Fatalf("recovered %d: %v", n, err)
		}
		got, _ := st.GetInvocation(bg, l.caller.Scope, l.sess.ID, stale.ID)
		if got.State != mcpproxy.InvocationUnknown || got.Code != "recovered" {
			t.Fatalf("a stale receipt must become unknown, never be requeued: %+v", got)
		}
		if got, _ := st.GetInvocation(bg, l.caller.Scope, l.sess.ID, settled.ID); got.State != mcpproxy.InvocationSucceeded {
			t.Fatalf("a settled receipt was touched: %+v", got)
		}
		if n, err := st.RecoverStale(bg, l.caller.Scope.TenantID, h.Now().Add(-time.Minute), 100); err != nil || n != 0 {
			t.Fatalf("a repeated recovery changed %d: %v", n, err)
		}
		// The recovered outcome is final: a late completion cannot rewrite it.
		late := finish(stale, func(i *mcpproxy.Invocation, n time.Time) error { return i.Succeed(n) })
		if err := st.Complete(bg, late); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
			t.Fatalf("a late completion of a recovered receipt: %v", err)
		}
		if n := h.Audit(t, l.caller.Scope, "mcp_tool.recovered"); n != 1 {
			t.Fatalf("%d recovery audit entries", n)
		}
	})
}

// detached (SD-14): the store never shares mutable state with a caller. Changing
// the registration a caller supplied or received changes nothing stored: not the
// enabled-tools allow-list, the version, the tombstone or the authorization of
// a live session, and it leaves no edit, audit entry or session invalidation.
func detached(t *testing.T, h Harness) {
	st := h.Store()
	l := liveSession(t, h, "echo", "add")
	want := []string{"add", "echo"}

	stored := func(label string) mcpproxy.Registration {
		t.Helper()
		got, err := st.GetRegistration(bg, l.caller.Scope, l.reg.ID)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got.Version != 1 || !reflect.DeepEqual(got.Tools, want) || got.Deleted() {
			t.Fatalf("%s: the stored registration changed: %+v", label, got)
		}
		if err := st.CheckSessionUse(bg, use(h, l)); err != nil {
			t.Fatalf("%s: the live session lost its authorization: %v", label, err)
		}
		if cur, _ := st.GetSession(bg, l.caller.Scope, l.sess.ID); cur.State != mcpproxy.SessionActive {
			t.Fatalf("%s: the live session is %s", label, cur.State)
		}
		if n := h.Audit(t, l.caller.Scope, "mcp_server.updated"); n != 0 {
			t.Fatalf("%s: %d unaudited-edit entries", label, n)
		}
		return got
	}
	tamper := func(r mcpproxy.Registration) {
		for i := range r.Tools {
			r.Tools[i] = "mutate"
		}
		if len(r.Tools) > 0 {
			r.Tools = append(r.Tools[:1], "extra")
		}
	}
	stored("initially")

	// The value the caller supplied (and kept) when it created the registration.
	tamper(l.reg)
	stored("after changing the supplied registration")

	// Every value a read returned.
	got, err := st.GetRegistration(bg, l.caller.Scope, l.reg.ID)
	must(t, err)
	tamper(got)
	stored("after changing a registration returned by Get")

	page, err := st.ListRegistrations(bg, l.caller.Scope, "", 100)
	must(t, err)
	for _, r := range page {
		tamper(r)
	}
	stored("after changing registrations returned by List")

	bound, err := st.EnsureBinding(bg, mcpproxy.BindingRequest{Caller: l.caller, Registration: l.reg.ID, Now: h.Now()})
	must(t, err)
	tamper(bound)
	stored("after changing a registration returned by EnsureBinding")

	// The value supplied to an edit, and the one it returns through Get.
	cur, err := st.GetRegistration(bg, l.caller.Scope, l.reg.ID)
	must(t, err)
	next, err := cur.Edit(1, mcpproxy.Draft{Endpoint: Endpoint, Secret: l.ref, Tools: []string{"add", "echo", "more"}}, actor(), h.Now())
	must(t, err)
	must(t, st.ReplaceRegistration(bg, mutation(h, "req_edit"), next, 1))
	next.Tools[0] = "tampered"
	again, err := st.GetRegistration(bg, l.caller.Scope, l.reg.ID)
	must(t, err)
	if again.Version != 2 || !reflect.DeepEqual(again.Tools, []string{"add", "echo", "more"}) {
		t.Fatalf("a retained edit changed the stored registration: %+v", again)
	}
	if n := h.Audit(t, l.caller.Scope, "mcp_server.updated"); n != 1 {
		t.Fatalf("%d edit audit entries, want exactly the one expected-version edit", n)
	}

	// The tombstone time.
	tomb, err := again.Delete(2, actor(), h.Now())
	must(t, err)
	must(t, st.ReplaceRegistration(bg, mutation(h, "req_del"), tomb, 2))
	deleted, err := st.GetRegistration(bg, l.caller.Scope, l.reg.ID)
	must(t, err)
	if !deleted.Deleted() {
		t.Fatalf("not deleted: %+v", deleted)
	}
	at := *deleted.DeletedAt
	*deleted.DeletedAt = time.Time{}
	*tomb.DeletedAt = time.Time{}
	if redo, err := st.GetRegistration(bg, l.caller.Scope, l.reg.ID); err != nil || !redo.Deleted() || !redo.DeletedAt.Equal(at) {
		t.Fatalf("the tombstone time was changed through a returned value: %+v %v", redo, err)
	}
}

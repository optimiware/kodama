package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/memdb"
)

// RecheckFunc verifies, inside the caller's transaction, that the caller's
// runtime authority is still current: ErrUnauthenticated or ErrDenied when it
// ended, any other error for an outage.
type RecheckFunc func(ctx context.Context, tx memdb.Tx, c mcpproxy.Caller, now time.Time) error

// PinFunc registers, inside the caller's transaction, that the run retains the
// exact secret version.
type PinFunc func(ctx context.Context, tx memdb.Tx, scope domain.Scope, run domain.RunID, secretName string, version int64) error

// StoreConfig wires a Store. DB, Recheck and Pin are required: there is no
// allow-all default.
type StoreConfig struct {
	DB      *memdb.Store
	Recheck RecheckFunc
	Pin     PinFunc
	Clock   func() time.Time
	// Seq numbers audit rows; shared with the World's identifiers.
	Seq *atomic.Int64
}

// Store implements mcpproxy.Store and mcpproxy.SecretChecker over memdb. One
// method call is one memdb update: the mutation and its audit entry land
// together or not at all, and a commit that fails after its function succeeded
// is reported as an uncertain outcome so the caller never dispatches on it.
type Store struct {
	db      *memdb.Store
	recheck RecheckFunc
	pin     PinFunc
	now     func() time.Time
	seq     *atomic.Int64
}

var (
	_ mcpproxy.Store         = (*Store)(nil)
	_ mcpproxy.SecretChecker = (*Store)(nil)
)

// NewStore validates the configuration.
func NewStore(c StoreConfig) *Store {
	if c.DB == nil || c.Recheck == nil || c.Pin == nil {
		panic("mcpproxy/memory: a database, an authority recheck and a pin registrar are required")
	}
	s := &Store{db: c.DB, recheck: c.Recheck, pin: c.Pin, now: c.Clock, seq: c.Seq}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.seq == nil {
		s.seq = new(atomic.Int64)
	}
	return s
}

// do runs fn in one tenant update. unknown reports that fn succeeded and the
// commit then failed: the write may or may not have landed.
func (s *Store) do(ctx context.Context, tenant domain.TenantID, fn func(memdb.Tx) error) (unknown bool, err error) {
	done := false
	err = s.db.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		done = true
		return nil
	})
	return err != nil && done, err
}

func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mcpproxy.ErrInvalid), errors.Is(err, mcpproxy.ErrNotFound), errors.Is(err, mcpproxy.ErrUnauthenticated),
		errors.Is(err, mcpproxy.ErrDenied), errors.Is(err, mcpproxy.ErrVersionConflict), errors.Is(err, mcpproxy.ErrConflict),
		errors.Is(err, mcpproxy.ErrBindingInvalid), errors.Is(err, mcpproxy.ErrSessionInvalid), errors.Is(err, mcpproxy.ErrLimitReached),
		errors.Is(err, mcpproxy.ErrInvalidTransition), errors.Is(err, mcpproxy.ErrCredential), errors.Is(err, mcpproxy.ErrUnavailable):
		return err
	case errors.Is(err, memdb.ErrConflict):
		return fmt.Errorf("%w: the record already exists", mcpproxy.ErrConflict)
	}
	return mcpproxy.Unavailablef(err, "the store failed")
}

func (s *Store) audit(tx memdb.Tx, e AuditRow) error {
	e.ID = fmt.Sprintf("aud_%012d", s.seq.Add(1))
	return auditTable.Put(tx, e)
}

// cloneRegistration detaches the mutable members of a registration (memdb
// stores values as given): the enabled-tools backing array and the tombstone
// time. Every ingress and egress of the store goes through it, so a caller
// changing a value it supplied or received changes nothing stored.
func cloneRegistration(r mcpproxy.Registration) mcpproxy.Registration {
	if r.Tools != nil {
		r.Tools = append([]string{}, r.Tools...)
	}
	if r.DeletedAt != nil {
		t := *r.DeletedAt
		r.DeletedAt = &t
	}
	return r
}

// registration reads a private copy of a registration of exactly this scope:
// another project's (or tenant's) registration is not found.
func registration(tx memdb.Tx, scope domain.Scope, id mcpproxy.RegistrationID) (mcpproxy.Registration, bool) {
	r, ok := serverTable.Get(tx, string(id))
	if !ok || r.Scope != scope {
		return mcpproxy.Registration{}, false
	}
	return cloneRegistration(r), true
}

func actorAudit(r mcpproxy.Registration, m mcpproxy.Mutation, action string) AuditRow {
	return AuditRow{Scope: r.Scope, ActorType: "user", ActorID: m.Actor.ID, Action: action, Resource: string(r.ID), RequestID: m.RequestID,
		Meta: map[string]any{"endpoint": r.Endpoint, "secretName": r.Secret.Name, "secretVersion": r.Secret.Version, "version": int64(r.Version)}}
}

// CreateRegistration inserts version 1 and its audit entry in one update.
func (s *Store) CreateRegistration(ctx context.Context, m mcpproxy.Mutation, r mcpproxy.Registration) error {
	if err := r.Validate(); err != nil || r.Version != 1 || r.Deleted() {
		return fmt.Errorf("%w: the registration is not a new version 1", mcpproxy.ErrInvalid)
	}
	unknown, err := s.do(ctx, r.Scope.TenantID, func(tx memdb.Tx) error {
		if _, ok := serverTable.Get(tx, string(r.ID)); ok {
			return mcpproxy.ErrConflict
		}
		if err := serverTable.Put(tx, cloneRegistration(r)); err != nil {
			return err
		}
		return s.audit(tx, actorAudit(r, m, "mcp_server.created"))
	})
	if unknown {
		return mcpproxy.Unavailablef(err, "the registration commit outcome is unknown; read before retrying")
	}
	return classify(err)
}

// ReplaceRegistration applies an edit or tombstone with the expected version
// and invalidates the sessions of the previous revision in the same update.
// Run bindings stay frozen at their old revision and are therefore unusable.
func (s *Store) ReplaceRegistration(ctx context.Context, m mcpproxy.Mutation, next mcpproxy.Registration, expected domain.Version) error {
	if err := next.Validate(); err != nil || next.Version != expected+1 {
		return fmt.Errorf("%w: the registration is not the successor of the expected version", mcpproxy.ErrInvalid)
	}
	unknown, err := s.do(ctx, next.Scope.TenantID, func(tx memdb.Tx) error {
		cur, ok := registration(tx, next.Scope, next.ID)
		if !ok || cur.Deleted() {
			return mcpproxy.ErrNotFound
		}
		if cur.Version != expected {
			return mcpproxy.ErrVersionConflict
		}
		if err := serverTable.Put(tx, cloneRegistration(next)); err != nil {
			return err
		}
		var live []mcpproxy.RemoteSession
		sessionTable.Scan(tx, func(sess mcpproxy.RemoteSession) bool {
			if sess.Caller.Scope == next.Scope && sess.Registration == next.ID && (sess.State == mcpproxy.SessionInitializing || sess.State == mcpproxy.SessionActive) {
				live = append(live, sess)
			}
			return true
		})
		for _, sess := range live {
			sess.State, sess.UpdatedAt = mcpproxy.SessionInvalid, next.UpdatedAt
			if err := sessionTable.Put(tx, sess); err != nil {
				return err
			}
		}
		action := "mcp_server.updated"
		if next.Deleted() {
			action = "mcp_server.deleted"
		}
		return s.audit(tx, actorAudit(next, m, action))
	})
	if unknown {
		return mcpproxy.Unavailablef(err, "the registration commit outcome is unknown; read before retrying")
	}
	return classify(err)
}

// GetRegistration reads one registration, including a tombstone.
func (s *Store) GetRegistration(ctx context.Context, scope domain.Scope, id mcpproxy.RegistrationID) (mcpproxy.Registration, error) {
	var out mcpproxy.Registration
	err := s.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := registration(tx, scope, id)
		if !ok {
			return mcpproxy.ErrNotFound
		}
		out = r
		return nil
	})
	return out, classify(err)
}

// ListRegistrations returns live registrations ordered by ID.
func (s *Store) ListRegistrations(ctx context.Context, scope domain.Scope, after mcpproxy.RegistrationID, limit int) ([]mcpproxy.Registration, error) {
	var out []mcpproxy.Registration
	err := s.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		serverTable.Scan(tx, func(r mcpproxy.Registration) bool {
			if r.Scope == scope && !r.Deleted() && r.ID > after && len(out) < limit {
				out = append(out, cloneRegistration(r))
			}
			return len(out) < limit
		})
		return nil
	})
	return out, classify(err)
}

// CheckSecret verifies the referenced secret is an MCP-kind, live secret of the
// same project with an exact, non-revoked version. Metadata only.
func (s *Store) CheckSecret(ctx context.Context, scope domain.Scope, ref mcpproxy.SecretRef) error {
	return classify(s.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error { return checkSecret(tx, scope, ref) }))
}

// Check adapts CheckSecret to the port's method name.
func (s *Store) Check(ctx context.Context, scope domain.Scope, ref mcpproxy.SecretRef) error {
	return s.CheckSecret(ctx, scope, ref)
}

// EnsureBinding freezes, on first authorized use, the registration revision and
// exact credential version for the caller's run and registers the secret pin in
// the same update. Authority is rechecked first, so a revoked attempt creates no
// binding. A registration change after the first use invalidates the binding
// rather than silently replacing the credential.
func (s *Store) EnsureBinding(ctx context.Context, req mcpproxy.BindingRequest) (mcpproxy.Registration, error) {
	c := req.Caller
	if err := c.Validate(); err != nil {
		return mcpproxy.Registration{}, err
	}
	var out mcpproxy.Registration
	unknown, err := s.do(ctx, c.Scope.TenantID, func(tx memdb.Tx) error {
		if err := s.recheck(ctx, tx, c, req.Now); err != nil {
			return err
		}
		r, ok := registration(tx, c.Scope, req.Registration)
		if !ok || r.Deleted() {
			return mcpproxy.ErrNotFound
		}
		key := string(c.RunID) + "/" + string(r.ID)
		if b, ok := bindingTable.Get(tx, key); ok && b.Scope == c.Scope {
			if b.ServerVersion != r.Version || b.Secret != r.Secret {
				return mcpproxy.ErrBindingInvalid
			}
			out = r
			return nil
		}
		if err := s.pin(ctx, tx, c.Scope, c.RunID, r.Secret.Name, r.Secret.Version); err != nil {
			return err
		}
		if err := bindingTable.Put(tx, bindingRow{Scope: c.Scope, Run: c.RunID, Server: r.ID, ServerVersion: r.Version, Secret: r.Secret}); err != nil {
			return err
		}
		out = r
		return s.audit(tx, AuditRow{Scope: c.Scope, ActorType: "workload", ActorID: string(c.AttemptID), Action: "mcp_server.credential_bound",
			Resource: string(r.ID), Meta: map[string]any{"runId": string(c.RunID), "attemptId": string(c.AttemptID), "secretName": r.Secret.Name, "secretVersion": r.Secret.Version}})
	})
	if unknown {
		return mcpproxy.Registration{}, mcpproxy.Unavailablef(err, "the binding commit outcome is unknown")
	}
	if err != nil {
		return mcpproxy.Registration{}, classify(err)
	}
	return out, nil
}

// CreateSession inserts an initializing session. A session can only exist for a
// binding frozen at the same registration revision and credential.
func (s *Store) CreateSession(ctx context.Context, sess mcpproxy.RemoteSession) error {
	c := sess.Caller
	if sess.State != mcpproxy.SessionInitializing || c.Validate() != nil {
		return fmt.Errorf("%w: a new session must be initializing", mcpproxy.ErrInvalid)
	}
	unknown, err := s.do(ctx, c.Scope.TenantID, func(tx memdb.Tx) error {
		b, ok := bindingTable.Get(tx, string(c.RunID)+"/"+string(sess.Registration))
		if !ok || b.Scope != c.Scope || b.ServerVersion != sess.RegistrationVersion || b.Secret != sess.Credential {
			return mcpproxy.ErrBindingInvalid
		}
		if _, ok := sessionTable.Get(tx, string(sess.ID)); ok {
			return mcpproxy.ErrConflict
		}
		return sessionTable.Put(tx, sess)
	})
	if unknown {
		return mcpproxy.Unavailablef(err, "the session commit outcome is unknown")
	}
	return classify(err)
}

func session(tx memdb.Tx, scope domain.Scope, id mcpproxy.SessionID) (mcpproxy.RemoteSession, bool) {
	r, ok := sessionTable.Get(tx, string(id))
	if !ok || r.Caller.Scope != scope {
		return mcpproxy.RemoteSession{}, false
	}
	return r, true
}

// GetSession reads one session of the scope.
func (s *Store) GetSession(ctx context.Context, scope domain.Scope, id mcpproxy.SessionID) (mcpproxy.RemoteSession, error) {
	var out mcpproxy.RemoteSession
	err := s.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := session(tx, scope, id)
		if !ok {
			return mcpproxy.ErrNotFound
		}
		out = r
		return nil
	})
	return out, classify(err)
}

// SaveSession persists a state transition from the given state and reports
// whether the row was in that state.
func (s *Store) SaveSession(ctx context.Context, sess mcpproxy.RemoteSession, from mcpproxy.SessionState) (bool, error) {
	var changed bool
	_, err := s.do(ctx, sess.Caller.Scope.TenantID, func(tx memdb.Tx) error {
		cur, ok := session(tx, sess.Caller.Scope, sess.ID)
		if !ok || cur.State != from {
			return nil
		}
		cur.State, cur.UpdatedAt = sess.State, sess.UpdatedAt
		changed = true
		return sessionTable.Put(tx, cur)
	})
	if err != nil {
		return false, classify(err)
	}
	return changed, nil
}

// ExpireSessions marks the tenant's live sessions past their expiry as expired.
func (s *Store) ExpireSessions(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) (int, error) {
	n := 0
	_, err := s.do(ctx, tenant, func(tx memdb.Tx) error {
		var due []mcpproxy.RemoteSession
		sessionTable.Scan(tx, func(sess mcpproxy.RemoteSession) bool {
			if (sess.State == mcpproxy.SessionInitializing || sess.State == mcpproxy.SessionActive) && !sess.ExpiresAt.After(now) {
				due = append(due, sess)
			}
			return true
		})
		sort.SliceStable(due, func(i, j int) bool { return due[i].ExpiresAt.Before(due[j].ExpiresAt) })
		if len(due) > limit {
			due = due[:limit]
		}
		for _, sess := range due {
			sess.State, sess.UpdatedAt = mcpproxy.SessionExpired, now.UTC().Truncate(time.Microsecond)
			if err := sessionTable.Put(tx, sess); err != nil {
				return err
			}
		}
		n = len(due)
		return nil
	})
	return n, classify(err)
}

// Recheck verifies current authority in its own short update.
func (s *Store) Recheck(ctx context.Context, c mcpproxy.Caller, now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	_, err := s.do(ctx, c.Scope.TenantID, func(tx memdb.Tx) error { return s.recheck(ctx, tx, c, now) })
	return classify(err)
}

// verifyUse decides, inside tx, whether a session may be used now. It is the one
// protected-use check: Admit runs it before charging a call or recording
// dispatch intent, and CheckSessionUse runs it for discovery and for the
// periodic check of an in-flight call. It reads metadata only and writes
// nothing.
func (s *Store) verifyUse(ctx context.Context, tx memdb.Tx, u mcpproxy.SessionUse) (mcpproxy.RemoteSession, error) {
	c := u.Caller
	if err := s.recheck(ctx, tx, c, u.Now); err != nil {
		return mcpproxy.RemoteSession{}, err
	}
	reg, ok := registration(tx, c.Scope, u.Registration)
	if !ok {
		return mcpproxy.RemoteSession{}, mcpproxy.ErrNotFound
	}
	sess, ok := session(tx, c.Scope, u.Session)
	if !ok {
		return mcpproxy.RemoteSession{}, mcpproxy.ErrSessionInvalid
	}
	if !sess.Owns(c) || sess.Registration != u.Registration {
		return mcpproxy.RemoteSession{}, mcpproxy.ErrSessionInvalid
	}
	if err := sess.Usable(u.Now, u.Incarnation); err != nil {
		return mcpproxy.RemoteSession{}, err
	}
	if reg.Deleted() || reg.Version != sess.RegistrationVersion {
		return mcpproxy.RemoteSession{}, mcpproxy.ErrBindingInvalid
	}
	b, ok := bindingTable.Get(tx, string(c.RunID)+"/"+string(sess.Registration))
	if !ok || b.Scope != c.Scope || b.ServerVersion != sess.RegistrationVersion || b.Secret != sess.Credential {
		return mcpproxy.RemoteSession{}, mcpproxy.ErrBindingInvalid
	}
	if err := checkVersionUse(tx, c.Scope, c.RunID, sess.Credential); err != nil {
		return mcpproxy.RemoteSession{}, err
	}
	return sess, nil
}

// CheckSessionUse implements mcpproxy.Store.
func (s *Store) CheckSessionUse(ctx context.Context, u mcpproxy.SessionUse) error {
	if err := u.Caller.Validate(); err != nil {
		return err
	}
	if err := u.Session.Validate(); err != nil {
		return mcpproxy.ErrSessionInvalid
	}
	_, err := s.do(ctx, u.Caller.Scope.TenantID, func(tx memdb.Tx) error {
		_, err := s.verifyUse(ctx, tx, u)
		return err
	})
	return classify(err)
}

func invocation(tx memdb.Tx, scope domain.Scope, session mcpproxy.SessionID, id mcpproxy.RequestID) (mcpproxy.Invocation, bool) {
	i, ok := invTable.Get(tx, string(session)+"/"+string(id))
	if !ok || i.Caller.Scope != scope {
		return mcpproxy.Invocation{}, false
	}
	return i, true
}

// Admit atomically admits one call. In one update it rechecks current runtime
// authority, the registration, the session and the exact credential version,
// inserts the dispatching receipt (a duplicate request returns the stored
// receipt and charges nothing), charges the run-wide counter against the
// claimed ceiling and appends the audit entry. A commit that fails after the
// function succeeded is ErrAdmissionUncertain: the service dispatches only on a
// nil error, so an uncertain admission never sends.
func (s *Store) Admit(ctx context.Context, req mcpproxy.AdmitRequest) (mcpproxy.Admission, error) {
	inv := req.Invocation
	c := inv.Caller
	if err := c.Validate(); err != nil {
		return mcpproxy.Admission{}, err
	}
	if inv.State != mcpproxy.InvocationDispatching {
		return mcpproxy.Admission{}, fmt.Errorf("%w: only a dispatching receipt is admitted", mcpproxy.ErrInvalid)
	}
	var out mcpproxy.Admission
	unknown, err := s.do(ctx, c.Scope.TenantID, func(tx memdb.Tx) error {
		if _, err := s.verifyUse(ctx, tx, mcpproxy.SessionUse{Caller: c, Registration: inv.Registration, Session: inv.Session,
			Incarnation: req.Incarnation, Now: req.Now}); err != nil {
			return err
		}
		if existing, ok := invocation(tx, c.Scope, inv.Session, inv.ID); ok {
			out = mcpproxy.Admission{Invocation: existing, Duplicate: true}
			return nil
		}
		stored := inv
		stored.State, stored.Code = mcpproxy.InvocationDispatching, ""
		stored.CreatedAt, stored.UpdatedAt = req.Now.UTC().Truncate(time.Microsecond), req.Now.UTC().Truncate(time.Microsecond)
		call, _ := callTable.Get(tx, string(c.RunID))
		if call.Scope != c.Scope {
			call = callRow{Scope: c.Scope, Run: c.RunID}
		}
		if req.MaxCalls != nil && call.Calls >= *req.MaxCalls {
			return mcpproxy.ErrLimitReached
		}
		call.Calls++
		if err := callTable.Put(tx, call); err != nil {
			return err
		}
		if err := invTable.Put(tx, stored); err != nil {
			return err
		}
		out = mcpproxy.Admission{Invocation: inv}
		return s.audit(tx, AuditRow{Scope: c.Scope, ActorType: "workload", ActorID: string(c.AttemptID), Action: "mcp_tool.admitted",
			Resource: string(inv.Registration), RequestID: string(inv.ID),
			Meta: map[string]any{"runId": string(c.RunID), "attemptId": string(c.AttemptID), "tool": inv.Tool, "callNumber": call.Calls}})
	})
	if unknown {
		return mcpproxy.Admission{}, fmt.Errorf("%w: %w: %v", mcpproxy.ErrUnavailable, mcpproxy.ErrAdmissionUncertain, err)
	}
	if err != nil {
		return mcpproxy.Admission{}, classify(err)
	}
	return out, nil
}

// GetInvocation reads one receipt.
func (s *Store) GetInvocation(ctx context.Context, scope domain.Scope, sess mcpproxy.SessionID, id mcpproxy.RequestID) (mcpproxy.Invocation, error) {
	var out mcpproxy.Invocation
	err := s.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		i, ok := invocation(tx, scope, sess, id)
		if !ok {
			return mcpproxy.ErrNotFound
		}
		out = i
		return nil
	})
	return out, classify(err)
}

// Complete records the terminal outcome of a dispatching receipt with its audit
// entry. The outcome is final: a terminal receipt is never rewritten.
func (s *Store) Complete(ctx context.Context, inv mcpproxy.Invocation) error {
	c := inv.Caller
	if !inv.State.Terminal() {
		return fmt.Errorf("%w: only a terminal outcome is recorded", mcpproxy.ErrInvalid)
	}
	unknown, err := s.do(ctx, c.Scope.TenantID, func(tx memdb.Tx) error {
		cur, ok := invocation(tx, c.Scope, inv.Session, inv.ID)
		if !ok || cur.State != mcpproxy.InvocationDispatching {
			return mcpproxy.ErrInvalidTransition
		}
		cur.State, cur.Code, cur.UpdatedAt = inv.State, inv.Code, inv.UpdatedAt.UTC().Truncate(time.Microsecond)
		if err := invTable.Put(tx, cur); err != nil {
			return err
		}
		return s.audit(tx, AuditRow{Scope: c.Scope, ActorType: "workload", ActorID: string(c.AttemptID), Action: "mcp_tool.completed",
			Resource: string(inv.Registration), RequestID: string(inv.ID),
			Meta: map[string]any{"runId": string(c.RunID), "attemptId": string(c.AttemptID), "tool": inv.Tool, "outcome": string(inv.State), "code": inv.Code}})
	})
	if unknown {
		return mcpproxy.Unavailablef(err, "the outcome commit is uncertain; the receipt is recoverable")
	}
	return classify(err)
}

// RecoverStale turns dispatching receipts older than the cutoff into unknown,
// with an audit entry each. It never requeues a call.
func (s *Store) RecoverStale(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) (int, error) {
	n := 0
	_, err := s.do(ctx, tenant, func(tx memdb.Tx) error {
		var stale []mcpproxy.Invocation
		invTable.Scan(tx, func(i mcpproxy.Invocation) bool {
			if i.State == mcpproxy.InvocationDispatching && i.UpdatedAt.Before(before) {
				stale = append(stale, i)
			}
			return true
		})
		sort.SliceStable(stale, func(a, b int) bool { return stale[a].UpdatedAt.Before(stale[b].UpdatedAt) })
		if len(stale) > limit {
			stale = stale[:limit]
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		for _, i := range stale {
			i.State, i.Code, i.UpdatedAt = mcpproxy.InvocationUnknown, "recovered", now
			if err := invTable.Put(tx, i); err != nil {
				return err
			}
			if err := s.audit(tx, AuditRow{Scope: i.Caller.Scope, ActorType: "system", ActorID: "mcp-proxy-recovery", Action: "mcp_tool.recovered",
				Resource: string(i.Registration), RequestID: string(i.ID),
				Meta: map[string]any{"runId": string(i.Caller.RunID), "attemptId": string(i.Caller.AttemptID), "tool": i.Tool, "outcome": "unknown", "code": "recovered"}}); err != nil {
				return err
			}
		}
		n = len(stale)
		return nil
	})
	return n, classify(err)
}

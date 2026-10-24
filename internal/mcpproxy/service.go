// Package mcpproxy mediates a workload's use of approved remote MCP servers
// over Streamable HTTP. A project registers an approved endpoint, an exact
// typed credential reference and enabled tool names; an agent profile pins the
// qualified tools an attempt may use; the proxy exposes only the intersection,
// under the active attempt's identity, using integration-owned credentials that
// never reach the workload.
//
// The package holds the registration, session and invocation entities, the
// pure tool-visibility policy, the use cases and small consumer-owned ports.
// It imports neither the MCP SDK, net/http nor pgx: those, and every peer
// module bridge, live in the protocol, http, postgres and adapters
// sub-packages and are wired by the composition root.
//
// Effects: a tool call is never retried or replayed. Admission makes the
// intent durable before any network I/O; a call whose reply is lost, cancelled
// or timed out is recorded as unknown and surfaced as such.
package mcpproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Options is the typed operational policy. Every field is required: there is
// no permissive default and no environment variable.
type Options struct {
	// Protocol is the single pinned MCP protocol revision.
	Protocol string
	// Incarnation identifies this process. Live handles are process-owned, so a
	// session recorded by another incarnation is rejected, never adopted.
	Incarnation string
	// SessionTTL is the fixed lifetime of a remote session.
	SessionTTL time.Duration
	// CallTimeout bounds one remote tool call.
	CallTimeout time.Duration
	// RecheckInterval is how often authority is rechecked during a call.
	RecheckInterval time.Duration
	// StaleAfter is the age after which a dispatching receipt is recovered as
	// unknown.
	StaleAfter time.Duration
	// MaxArgumentBytes and MaxResultBytes bound call content.
	MaxArgumentBytes int
	MaxResultBytes   int
	// MaxDiscoveryPages bounds pagination followed while filtering discovery.
	MaxDiscoveryPages int
	// RecordRaw additionally enables raw recording; it also requires a
	// recorder and the pinned transcript policy.
	RecordRaw bool
}

// Validate checks the policy.
func (o Options) Validate() error {
	switch {
	case o.Protocol == "", o.Incarnation == "":
		return errors.New("mcpproxy: a pinned protocol and a process incarnation are required")
	case o.SessionTTL <= 0, o.CallTimeout <= 0, o.RecheckInterval <= 0, o.StaleAfter <= 0:
		return errors.New("mcpproxy: session, call, recheck and recovery durations are required")
	case o.MaxArgumentBytes <= 0, o.MaxResultBytes <= 0, o.MaxDiscoveryPages <= 0:
		return errors.New("mcpproxy: argument, result and discovery bounds are required")
	}
	return nil
}

// Config wires a Service. Store, Runtime, Policy, Credentials, Remote,
// Destinations and Human are required. A missing Secrets checker makes
// registration writes fail closed; Recorder is optional (no recorder, no raw
// persistence).
type Config struct {
	Store        Store
	Human        HumanAuthority
	Runtime      RuntimeAuthority
	Policy       AttemptPolicy
	Credentials  CredentialSource
	Remote       RemoteTools
	Destinations Destinations
	Secrets      SecretChecker
	Recorder     ContentRecorder
	Options      Options
	Clock        func() time.Time
	NewID        func(prefix string) (string, error)
	Logger       *slog.Logger
}

type live struct {
	conn    RemoteConn
	tenant  domain.TenantID
	expires time.Time
}

// Service holds the use cases.
type Service struct {
	cfg  Config
	mu   sync.Mutex
	open map[SessionID]*live
}

// NewService validates cfg.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil, cfg.Human == nil, cfg.Runtime == nil, cfg.Policy == nil, cfg.Credentials == nil, cfg.Remote == nil, cfg.Destinations == nil:
		return nil, errors.New("mcpproxy: store, authorities, policy, credentials, remote tools and destinations are required")
	}
	if err := cfg.Options.Validate(); err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{cfg: cfg, open: map[SessionID]*live{}}, nil
}

func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func (s *Service) now() time.Time { return s.cfg.Clock().UTC() }

func (s *Service) newID(prefix string) (string, error) {
	id, err := s.cfg.NewID(prefix)
	if err != nil {
		return "", Unavailablef(err, "identifier generation failed")
	}
	return id, nil
}

// log records only safe identifiers and the classified outcome.
func (s *Service) log(ctx context.Context, op string, c Caller, err error) {
	level := slog.LevelInfo
	if err != nil && errors.Is(err, ErrUnavailable) {
		level = slog.LevelWarn
	}
	s.cfg.Logger.LogAttrs(ctx, level, "mcp proxy", slog.String("op", op), slog.String("outcome", Outcome(err)),
		slog.String("attemptId", string(c.AttemptID)))
}

// Outcome names the classified result of an operation for logs and audit.
func Outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, ErrUnauthenticated):
		return "unauthenticated"
	case errors.Is(err, ErrDenied):
		return "denied"
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrSessionInvalid):
		return "not_found"
	case errors.Is(err, ErrToolHidden):
		return "tool_hidden"
	case errors.Is(err, ErrLimitReached):
		return "limit_reached"
	case errors.Is(err, ErrDuplicate):
		return "duplicate"
	case errors.Is(err, ErrOutcomeUnknown):
		return "unknown_outcome"
	case errors.Is(err, ErrRemoteFailed):
		return "remote_failed"
	case errors.Is(err, ErrBindingInvalid):
		return "binding_invalid"
	case errors.Is(err, ErrCredential):
		return "credential"
	case errors.Is(err, ErrDestination):
		return "destination"
	case errors.Is(err, ErrProtocol):
		return "protocol"
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	}
	return "unavailable"
}

// ---- management ------------------------------------------------------------

// ManageCaller is the verified human behind a management request.
type ManageCaller struct {
	PrincipalID string
	RequestID   string
}

func (s *Service) authorizeHuman(ctx context.Context, scope domain.Scope, c ManageCaller, perm Permission) (Actor, error) {
	if err := scope.Validate(); err != nil {
		return Actor{}, invalidf("the scope is not valid")
	}
	actor := Actor{ID: c.PrincipalID}
	if err := actor.Validate(); err != nil {
		return Actor{}, err
	}
	switch err := s.cfg.Human.Authorize(ctx, c.PrincipalID, scope, perm); {
	case err == nil:
		return actor, nil
	case errors.Is(err, ErrDenied), errors.Is(err, ErrNotFound):
		return Actor{}, err
	default:
		return Actor{}, Unavailablef(err, "the authority failed")
	}
}

// vet applies the destination and credential checks to a draft.
func (s *Service) vet(ctx context.Context, scope domain.Scope, d Draft) (Draft, error) {
	nd, err := d.Validate()
	if err != nil {
		return Draft{}, err
	}
	if err := s.cfg.Destinations.Approve(nd.Endpoint); err != nil {
		return Draft{}, err
	}
	if s.cfg.Secrets == nil {
		return Draft{}, Unavailablef(nil, "no credential checker is configured")
	}
	switch err := s.cfg.Secrets.Check(ctx, scope, nd.Secret); {
	case err == nil:
	case errors.Is(err, ErrCredential), errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalid):
		return Draft{}, ErrCredential
	default:
		return Draft{}, Unavailablef(err, "the credential check failed")
	}
	return nd, nil
}

// Create registers a server. Authorization precedes every other step.
func (s *Service) Create(ctx context.Context, scope domain.Scope, c ManageCaller, d Draft) (Registration, error) {
	actor, err := s.authorizeHuman(ctx, scope, c, PermMutate)
	if err != nil {
		return Registration{}, err
	}
	nd, err := s.vet(ctx, scope, d)
	if err != nil {
		return Registration{}, err
	}
	id, err := s.newID("mcp_")
	if err != nil {
		return Registration{}, err
	}
	now := s.now()
	r, err := NewRegistration(scope, RegistrationID(id), nd, actor, now)
	if err != nil {
		return Registration{}, err
	}
	if err := s.cfg.Store.CreateRegistration(ctx, Mutation{Actor: actor, RequestID: c.RequestID, Now: now}, r); err != nil {
		return Registration{}, err
	}
	return r, nil
}

// Edit replaces the content of a registration if expected is its version.
func (s *Service) Edit(ctx context.Context, scope domain.Scope, c ManageCaller, id RegistrationID, expected domain.Version, d Draft) (Registration, error) {
	actor, err := s.authorizeHuman(ctx, scope, c, PermMutate)
	if err != nil {
		return Registration{}, err
	}
	if err := id.Validate(); err != nil {
		return Registration{}, ErrNotFound
	}
	cur, err := s.cfg.Store.GetRegistration(ctx, scope, id)
	if err != nil {
		return Registration{}, err
	}
	if cur.Deleted() {
		return Registration{}, ErrNotFound
	}
	nd, err := s.vet(ctx, scope, d)
	if err != nil {
		return Registration{}, err
	}
	now := s.now()
	next, err := cur.Edit(expected, nd, actor, now)
	if err != nil {
		return Registration{}, err
	}
	if err := s.cfg.Store.ReplaceRegistration(ctx, Mutation{Actor: actor, RequestID: c.RequestID, Now: now}, next, expected); err != nil {
		return Registration{}, err
	}
	return next, nil
}

// Delete tombstones a registration if expected is its version.
func (s *Service) Delete(ctx context.Context, scope domain.Scope, c ManageCaller, id RegistrationID, expected domain.Version) error {
	actor, err := s.authorizeHuman(ctx, scope, c, PermMutate)
	if err != nil {
		return err
	}
	if err := id.Validate(); err != nil {
		return ErrNotFound
	}
	cur, err := s.cfg.Store.GetRegistration(ctx, scope, id)
	if err != nil {
		return err
	}
	if cur.Deleted() {
		return ErrNotFound
	}
	now := s.now()
	next, err := cur.Delete(expected, actor, now)
	if err != nil {
		return err
	}
	return s.cfg.Store.ReplaceRegistration(ctx, Mutation{Actor: actor, RequestID: c.RequestID, Now: now}, next, expected)
}

// Get returns one live registration.
func (s *Service) Get(ctx context.Context, scope domain.Scope, c ManageCaller, id RegistrationID) (Registration, error) {
	if _, err := s.authorizeHuman(ctx, scope, c, PermList); err != nil {
		return Registration{}, err
	}
	if err := id.Validate(); err != nil {
		return Registration{}, ErrNotFound
	}
	r, err := s.cfg.Store.GetRegistration(ctx, scope, id)
	if err != nil {
		return Registration{}, err
	}
	if r.Deleted() {
		return Registration{}, ErrNotFound
	}
	return r, nil
}

// MaxPageRows bounds one list read.
const MaxPageRows = 1001

// List returns up to limit live registrations after the position, by ID.
func (s *Service) List(ctx context.Context, scope domain.Scope, c ManageCaller, after RegistrationID, limit int) ([]Registration, error) {
	if _, err := s.authorizeHuman(ctx, scope, c, PermList); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxPageRows {
		return nil, invalidf("invalid page size")
	}
	out, err := s.cfg.Store.ListRegistrations(ctx, scope, after, limit)
	if err != nil {
		return nil, err
	}
	for _, r := range out {
		if r.Scope != scope {
			return nil, fmt.Errorf("%w: the store returned a foreign registration", ErrConflict)
		}
	}
	return out, nil
}

// ---- runtime ---------------------------------------------------------------

// Authenticate verifies a workload bearer for the attempt named by the route.
func (s *Service) Authenticate(ctx context.Context, bearer string, attempt domain.AttemptID) (Caller, error) {
	c, err := s.cfg.Runtime.Authorize(ctx, bearer, attempt)
	switch {
	case err == nil:
	case errors.Is(err, ErrUnauthenticated), errors.Is(err, ErrDenied):
		return Caller{}, err
	default:
		return Caller{}, Unavailablef(err, "the runtime authority failed")
	}
	if err := c.Validate(); err != nil || c.AttemptID != attempt {
		return Caller{}, ErrUnauthenticated
	}
	return c, nil
}

func (s *Service) policy(ctx context.Context, c Caller) (ToolPolicy, error) {
	p, err := s.cfg.Policy.PolicyOf(ctx, c)
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, ErrNotFound):
		return ToolPolicy{}, ErrNotFound
	}
	return ToolPolicy{}, Unavailablef(err, "the pinned policy could not be read")
}

// OpenSession binds a new remote session to the caller. It requires the
// registration to be live and to expose at least one tool in the intersection
// of registration and pinned policy; freezes the run binding on first use;
// opens the exact credential outside any transaction; rechecks authority after
// that slow step; and verifies the pinned protocol before anything is stored.
func (s *Service) OpenSession(ctx context.Context, c Caller, regID RegistrationID) (id SessionID, err error) {
	defer func() { s.log(ctx, "open_session", c, err) }()
	if err := c.Validate(); err != nil {
		return "", err
	}
	if err := regID.Validate(); err != nil {
		return "", ErrNotFound
	}
	pol, err := s.policy(ctx, c)
	if err != nil {
		return "", err
	}
	reg, err := s.cfg.Store.GetRegistration(ctx, c.Scope, regID)
	if err != nil {
		return "", err
	}
	if reg.Deleted() || !AnyVisible(reg, pol) {
		return "", ErrNotFound
	}
	if err := s.cfg.Destinations.Approve(reg.Endpoint); err != nil {
		return "", err
	}
	now := s.now()
	reg, err = s.cfg.Store.EnsureBinding(ctx, BindingRequest{Caller: c, Registration: regID, Now: now})
	if err != nil {
		return "", err
	}
	_, u, err := NormalizeEndpoint(reg.Endpoint)
	if err != nil {
		return "", ErrConflict
	}
	requestID, err := s.newID("req_")
	if err != nil {
		return "", err
	}
	cred, err := s.cfg.Credentials.Open(ctx, CredentialRequest{Caller: c, Registration: reg, RequestID: requestID})
	if err != nil {
		if errors.Is(err, ErrCredential) || errors.Is(err, ErrBindingInvalid) {
			return "", err
		}
		return "", Unavailablef(err, "the credential could not be prepared")
	}
	defer cred.Discard()
	if cred.Recipient() != Origin(u) {
		return "", ErrCredential
	}
	// Slow credential preparation may have outlived the attempt.
	if err := s.recheck(ctx, c); err != nil {
		return "", err
	}
	conn, err := s.cfg.Remote.Open(ctx, RemoteTarget{Endpoint: reg.Endpoint, Recipient: cred.Recipient(), Bearer: cred.Bearer()})
	if err != nil {
		if errors.Is(err, ErrDestination) || errors.Is(err, ErrProtocol) || errors.Is(err, ErrCredential) {
			return "", err
		}
		return "", Unavailablef(err, "the remote server could not be reached")
	}
	if conn.Protocol() != s.cfg.Options.Protocol {
		discard(conn)
		return "", ErrProtocol
	}
	sid, err := s.newID("mcs_")
	if err != nil {
		discard(conn)
		return "", err
	}
	sess, err := NewRemoteSession(SessionID(sid), c, reg, conn.Protocol(), s.cfg.Options.Incarnation, now, s.cfg.Options.SessionTTL)
	if err == nil {
		err = s.cfg.Store.CreateSession(ctx, sess)
	}
	if err != nil {
		discard(conn)
		return "", err
	}
	if err := sess.Activate(s.now()); err != nil {
		discard(conn)
		s.fail(sess)
		return "", err
	}
	ok, err := s.cfg.Store.SaveSession(ctx, sess, SessionInitializing)
	if err != nil || !ok {
		discard(conn)
		s.fail(sess)
		if err == nil {
			err = ErrConflict
		}
		return "", err
	}
	s.mu.Lock()
	s.open[sess.ID] = &live{conn: conn, tenant: c.Scope.TenantID, expires: sess.ExpiresAt}
	s.mu.Unlock()
	return sess.ID, nil
}

// fail records a session whose initialization did not complete.
func (s *Service) fail(sess RemoteSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if sess.State == SessionActive {
		sess.State = SessionInitializing
	}
	if err := sess.Fail(s.now()); err == nil {
		_, _ = s.cfg.Store.SaveSession(ctx, sess, SessionInitializing)
	}
}

func (s *Service) recheck(ctx context.Context, c Caller) error {
	switch err := s.cfg.Store.Recheck(ctx, c, s.now()); {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnauthenticated), errors.Is(err, ErrDenied), errors.Is(err, ErrNotFound):
		return err
	default:
		return Unavailablef(err, "the authority recheck failed")
	}
}

// use is the protected-use check of a live session: current authority, the
// session, the frozen registration revision and the exact credential version,
// all from durable state in one short transaction and never from a cache. A
// definitive answer that the session or its credential is unusable invalidates
// the session and drops its live handle; no conclusive answer (a dependency
// outage) denies this use without inventing a permanent revocation.
func (s *Service) use(ctx context.Context, c Caller, sess RemoteSession) error {
	return s.ended(sess, s.check(ctx, c, sess))
}

// check classifies the durable use check without any side effect: nil, an
// authority error, a definitive verdict about the session or its credential
// (ErrBindingInvalid, ErrSessionInvalid, ErrCredential) or ErrUnavailable.
func (s *Service) check(ctx context.Context, c Caller, sess RemoteSession) error {
	err := s.cfg.Store.CheckSessionUse(ctx, SessionUse{Caller: c, Registration: sess.Registration, Session: sess.ID,
		Incarnation: s.cfg.Options.Incarnation, Now: s.now()})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnauthenticated), errors.Is(err, ErrDenied):
		return err
	case errors.Is(err, ErrNotFound):
		return ErrBindingInvalid
	case errors.Is(err, ErrBindingInvalid), errors.Is(err, ErrSessionInvalid), errors.Is(err, ErrCredential):
		return err
	case errors.Is(err, ErrUnavailable):
		return err
	}
	return Unavailablef(err, "the session use check failed")
}

// ended applies the existing session transitions when err says the session or
// its frozen credential is definitively unusable, and returns err unchanged.
// Authority loss and outages are not recorded here: the first ends with the
// attempt, the second is not a conclusion.
func (s *Service) ended(sess RemoteSession, err error) error {
	switch {
	case errors.Is(err, ErrBindingInvalid), errors.Is(err, ErrCredential):
		s.settle(sess, (*RemoteSession).Invalidate)
	case errors.Is(err, ErrSessionInvalid):
		if !s.now().Before(sess.ExpiresAt) {
			s.settle(sess, (*RemoteSession).Expire)
		} else {
			s.settle(sess, (*RemoteSession).Invalidate)
		}
	}
	return err
}

// Reasons a watched operation stopped locally.
const (
	codeAuthorityEnded       = "authority_ended"
	codeAuthorityUnavailable = "authority_unavailable"
)

// watch runs fn, a slow remote operation on a session, under a context that is
// bounded by the call timeout and cancelled as soon as the periodic current-use
// check fails: a denial, a revoked credential, an expired session or an outage
// of the checking dependency all stop local forwarding. Cached authority never
// extends an operation. The returned reason is "" when fn ended on its own,
// otherwise the classified local cause: authority_ended, authority_unavailable,
// cancelled or deadline; cause is the check's error for the authority reasons.
func (s *Service) watch(ctx context.Context, c Caller, sess RemoteSession, fn func(ctx context.Context) error) (reason string, cause error, err error) {
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.Options.CallTimeout)
	defer cancel()
	stop := make(chan struct{})
	var stopped atomic.Pointer[error]
	go func() {
		t := time.NewTicker(s.cfg.Options.RecheckInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-callCtx.Done():
				return
			case <-t.C:
				rctx, rcancel := context.WithTimeout(context.WithoutCancel(callCtx), s.cfg.Options.RecheckInterval)
				cerr := s.check(rctx, c, sess)
				rcancel()
				if cerr != nil {
					// Denial and outage alike stop forwarding: fail closed. The
					// reason is recorded and the call cancelled before the session
					// is ended, so the outcome is labelled by its true cause rather
					// than by the transport error that closing the handle causes.
					stopped.Store(&cerr)
					if !errors.Is(cerr, ErrUnavailable) {
						// The credential's end is erased before the call is
						// cancelled: cancelling makes the SDK send a cancellation
						// notification, which must not carry a bearer that was
						// just found unusable. An outage proves nothing about the
						// credential, so the handle stays intact for recovery.
						s.revoke(sess.ID)
					}
					cancel()
					_ = s.ended(sess, cerr)
					return
				}
			}
		}
	}()
	err = fn(callCtx)
	close(stop)
	if err == nil {
		return "", nil, nil
	}
	if p := stopped.Load(); p != nil {
		if errors.Is(*p, ErrUnavailable) {
			return codeAuthorityUnavailable, *p, err
		}
		return codeAuthorityEnded, *p, err
	}
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return "cancelled", nil, err
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		return "deadline", nil, err
	}
	return "", nil, err
}

// revoke erases the live handle's credential copy without contacting the
// remote or closing the handle.
func (s *Service) revoke(id SessionID) {
	s.mu.Lock()
	l := s.open[id]
	s.mu.Unlock()
	if l != nil {
		l.conn.Revoke()
	}
}

// discard closes a connection that never became a live session, without
// presenting the credential: nothing confirmed it after it was opened.
func discard(conn RemoteConn) {
	conn.Revoke()
	_ = conn.Close()
}

// dropLive forgets the process-local handle and closes it without presenting
// the credential: it is erased first, so the remote session's termination
// request fails locally and the remote session lapses on its own timer. A
// credential is only ever sent when something has just confirmed that it may be
// used; every end of a session that has not (expiry, sweep, shutdown, an
// invalidation, an abandoned initialization) therefore sends nothing.
func (s *Service) dropLive(id SessionID) { s.drop(id, false) }

// drop closes and forgets the process-local handle. With graceful, the remote
// session is terminated using the credential: only for a deliberate close by
// the authorized caller after a successful current-use check.
func (s *Service) drop(id SessionID, graceful bool) {
	s.mu.Lock()
	l := s.open[id]
	delete(s.open, id)
	s.mu.Unlock()
	if l == nil {
		return
	}
	if !graceful {
		l.conn.Revoke()
	}
	_ = l.conn.Close() // idempotent; an in-flight call ends as unknown
}

// settle records a terminal session state and drops the live handle without
// presenting the credential.
func (s *Service) settle(sess RemoteSession, apply func(*RemoteSession, time.Time) error) {
	s.settleWith(sess, apply, false)
}

func (s *Service) settleWith(sess RemoteSession, apply func(*RemoteSession, time.Time) error, graceful bool) {
	from := sess.State
	if err := apply(&sess, s.now()); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.cfg.Store.SaveSession(ctx, sess, from)
	}
	s.drop(sess.ID, graceful)
}

// lookup resolves a session for exactly the caller that owns it. Every failure
// mode of a copied, expired, closed or process-lost session is the same
// not-found to the caller.
func (s *Service) lookup(ctx context.Context, c Caller, reg RegistrationID, id SessionID) (RemoteSession, *live, error) {
	if err := c.Validate(); err != nil {
		return RemoteSession{}, nil, err
	}
	if err := id.Validate(); err != nil {
		return RemoteSession{}, nil, ErrSessionInvalid
	}
	sess, err := s.cfg.Store.GetSession(ctx, c.Scope, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RemoteSession{}, nil, ErrSessionInvalid
		}
		return RemoteSession{}, nil, err
	}
	if !sess.Owns(c) || sess.Registration != reg {
		return RemoteSession{}, nil, ErrSessionInvalid
	}
	now := s.now()
	if sess.State == SessionActive || sess.State == SessionInitializing {
		switch {
		case !now.Before(sess.ExpiresAt):
			s.settle(sess, (*RemoteSession).Expire)
			return RemoteSession{}, nil, ErrSessionInvalid
		case sess.Incarnation != s.cfg.Options.Incarnation:
			// Another process owns the live handle; this one never adopts it.
			s.settle(sess, (*RemoteSession).Invalidate)
			return RemoteSession{}, nil, ErrSessionInvalid
		}
	}
	if err := sess.Usable(now, s.cfg.Options.Incarnation); err != nil {
		return RemoteSession{}, nil, err
	}
	s.mu.Lock()
	l := s.open[id]
	s.mu.Unlock()
	if l == nil {
		s.settle(sess, (*RemoteSession).Invalidate)
		return RemoteSession{}, nil, ErrSessionInvalid
	}
	return sess, l, nil
}

// Session verifies that the session belongs to exactly this caller and is
// usable now.
func (s *Service) Session(ctx context.Context, c Caller, reg RegistrationID, id SessionID) error {
	_, _, err := s.lookup(ctx, c, reg, id)
	return err
}

// current rereads the registration and invalidates the session when it changed.
func (s *Service) current(ctx context.Context, sess RemoteSession) (Registration, error) {
	reg, err := s.cfg.Store.GetRegistration(ctx, sess.Caller.Scope, sess.Registration)
	if err != nil {
		return Registration{}, err
	}
	if reg.Deleted() || reg.Version != sess.RegistrationVersion {
		s.settle(sess, (*RemoteSession).Invalidate)
		return Registration{}, ErrBindingInvalid
	}
	return reg, nil
}

// Discover lists the tools in the intersection of the registration's enabled
// tools and the pinned profile policy. It follows remote pagination while
// filtering and never forwards a remote cursor.
func (s *Service) Discover(ctx context.Context, c Caller, regID RegistrationID, id SessionID) (tools []RemoteTool, err error) {
	defer func() { s.log(ctx, "discover", c, err) }()
	sess, l, err := s.lookup(ctx, c, regID, id)
	if err != nil {
		return nil, err
	}
	// Current authority, session, registration revision and exact credential
	// version are verified before the remote is contacted.
	if err := s.use(ctx, c, sess); err != nil {
		return nil, err
	}
	pol, err := s.policy(ctx, c)
	if err != nil {
		return nil, err
	}
	reg, err := s.current(ctx, sess)
	if err != nil {
		return nil, err
	}
	out := []RemoteTool{}
	seen := map[string]bool{}
	// Slow remote pagination is bounded and keeps rechecking current use.
	_, cause, werr := s.watch(ctx, c, sess, func(wctx context.Context) error {
		cursor := ""
		for page := 0; page < s.cfg.Options.MaxDiscoveryPages; page++ {
			pg, lerr := l.conn.ListTools(wctx, cursor)
			if lerr != nil {
				return lerr
			}
			for _, t := range pg.Tools {
				if !seen[t.Name] && Visible(reg, pol, t.Name) {
					seen[t.Name] = true
					out = append(out, t)
				}
			}
			if pg.NextCursor == "" {
				return nil
			}
			cursor = pg.NextCursor
		}
		return nil
	})
	switch {
	case werr == nil:
		return out, nil
	case cause != nil:
		return nil, cause
	}
	return nil, Unavailablef(werr, "the remote tool listing failed")
}

// InvokeRequest is one tool call.
type InvokeRequest struct {
	// ID identifies the call within the session.
	ID        RequestID
	Tool      string
	Arguments json.RawMessage
}

// Invoke runs one tool call. The tool must be in the intersection of the
// current registration and the pinned policy, whatever discovery returned.
// Admission is durable before dispatch; a failed or uncertain admission, a
// cancelled context or a duplicate request never dispatches.
func (s *Service) Invoke(ctx context.Context, c Caller, regID RegistrationID, id SessionID, req InvokeRequest) (res RemoteResult, err error) {
	defer func() { s.log(ctx, "invoke", c, err) }()
	if err := req.ID.Validate(); err != nil {
		return RemoteResult{}, err
	}
	if err := ValidateToolName(req.Tool); err != nil {
		return RemoteResult{}, ErrToolHidden
	}
	if len(req.Arguments) > s.cfg.Options.MaxArgumentBytes {
		return RemoteResult{}, ErrTooLarge
	}
	sess, l, err := s.lookup(ctx, c, regID, id)
	if err != nil {
		return RemoteResult{}, err
	}
	pol, err := s.policy(ctx, c)
	if err != nil {
		return RemoteResult{}, err
	}
	reg, err := s.current(ctx, sess)
	if err != nil {
		return RemoteResult{}, err
	}
	if !Visible(reg, pol, req.Tool) {
		return RemoteResult{}, ErrToolHidden
	}
	if err := ctx.Err(); err != nil {
		return RemoteResult{}, err
	}
	now := s.now()
	inv, err := NewInvocation(req.ID, sess, req.Tool, now)
	if err != nil {
		return RemoteResult{}, err
	}
	if err := inv.Dispatch(now); err != nil {
		return RemoteResult{}, err
	}
	adm, err := s.cfg.Store.Admit(ctx, AdmitRequest{Invocation: inv, Incarnation: s.cfg.Options.Incarnation, MaxCalls: pol.MaxToolCalls, Now: now})
	if err != nil {
		// A commit of unknown outcome is not an admission: nothing is sent. A
		// definitively unusable session or credential is recorded as such.
		return RemoteResult{}, s.ended(sess, err)
	}
	if adm.Duplicate {
		return RemoteResult{}, &OutcomeError{Kind: ErrDuplicate, RequestID: req.ID, State: adm.Invocation.State, Code: adm.Invocation.Code}
	}
	if err := ctx.Err(); err != nil {
		// Cancelled between admission and send: the send did not happen.
		s.finish(ctx, inv, func(i *Invocation, t time.Time) error { return i.Fail("cancelled_before_send", t) })
		return RemoteResult{}, err
	}
	return s.dispatch(ctx, c, sess, l, reg, pol, inv, req)
}

// OutcomeError carries the classified outcome of a call with safe correlation
// metadata. Kind is ErrOutcomeUnknown, ErrRemoteFailed or ErrDuplicate. It
// claims nothing about rollback of a remote effect.
type OutcomeError struct {
	Kind      error
	RequestID RequestID
	State     InvocationState
	Code      string
}

func (e *OutcomeError) Error() string {
	return fmt.Sprintf("%s (request %s, %s)", e.Kind.Error(), e.RequestID, e.Code)
}
func (e *OutcomeError) Is(target error) bool { return target == e.Kind }

func (s *Service) finish(ctx context.Context, inv Invocation, apply func(*Invocation, time.Time) error) Invocation {
	// The outcome is persisted even when the caller's context is gone.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := apply(&inv, s.now()); err != nil {
		return inv
	}
	if err := s.cfg.Store.Complete(pctx, inv); err != nil {
		// The receipt stays dispatching: recovery turns it into unknown. The call
		// is never resent.
		s.cfg.Logger.LogAttrs(pctx, slog.LevelWarn, "mcp proxy", slog.String("op", "complete"), slog.String("outcome", Outcome(err)),
			slog.String("requestId", string(inv.ID)))
	}
	return inv
}

func (s *Service) dispatch(ctx context.Context, c Caller, sess RemoteSession, l *live, reg Registration, pol ToolPolicy, inv Invocation, req InvokeRequest) (RemoteResult, error) {
	// Long-lived work periodically rechecks current use (authority, session and
	// exact credential) and stops local forwarding when it ends or cannot be
	// confirmed; cancellation propagates through the SDK connection.
	var res RemoteResult
	reason, _, cerr := s.watch(ctx, c, sess, func(callCtx context.Context) error {
		var err error
		res, err = l.conn.CallTool(callCtx, req.Tool, req.Arguments)
		return err
	})

	if cerr == nil {
		if len(res.JSON) > s.cfg.Options.MaxResultBytes {
			// The remote effect happened; the oversized reply cannot be delivered.
			s.finish(ctx, inv, func(i *Invocation, t time.Time) error { return i.MarkUnknown("result_too_large", t) })
			return RemoteResult{}, &OutcomeError{Kind: ErrOutcomeUnknown, RequestID: inv.ID, State: InvocationUnknown, Code: "result_too_large"}
		}
		done := s.finish(ctx, inv, func(i *Invocation, t time.Time) error { return i.Succeed(t) })
		s.record(ctx, c, reg, pol, req, res, done.State)
		return res, nil
	}
	var rf *RemoteFailure
	if errors.As(cerr, &rf) && rf.Reply {
		s.finish(ctx, inv, func(i *Invocation, t time.Time) error { return i.Fail(safeCode(rf.Code), t) })
		return RemoteResult{}, &OutcomeError{Kind: ErrRemoteFailed, RequestID: inv.ID, State: InvocationFailed, Code: safeCode(rf.Code)}
	}
	code := "transport"
	switch {
	case reason != "":
		code = reason
	case rf != nil && rf.Code != "":
		code = safeCode(rf.Code)
	}
	s.finish(ctx, inv, func(i *Invocation, t time.Time) error { return i.MarkUnknown(code, t) })
	return RemoteResult{}, &OutcomeError{Kind: ErrOutcomeUnknown, RequestID: inv.ID, State: InvocationUnknown, Code: code}
}

// safeCode bounds a remote-provided code to a classified token.
func safeCode(code string) string {
	if len(code) > 64 {
		return "remote_error"
	}
	for _, r := range code {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "remote_error"
		}
	}
	if code == "" {
		return "remote_error"
	}
	return code
}

// record hands raw content to the recorder only when every gate allows it. A
// recorder failure is logged without content and never retries the tool.
func (s *Service) record(ctx context.Context, c Caller, reg Registration, pol ToolPolicy, req InvokeRequest, res RemoteResult, state InvocationState) {
	if s.cfg.Recorder == nil || !s.cfg.Options.RecordRaw || !pol.RecordTranscript {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.cfg.Recorder.Record(rctx, RecordedCall{Caller: c, Registration: reg.ID, Tool: req.Tool, RequestID: req.ID,
		Arguments: req.Arguments, Result: res.JSON, Outcome: state}); err != nil {
		s.cfg.Logger.LogAttrs(rctx, slog.LevelWarn, "mcp proxy", slog.String("op", "record"), slog.String("outcome", "unavailable"))
	}
}

// CloseSession ends a session owned by the caller.
func (s *Service) CloseSession(ctx context.Context, c Caller, regID RegistrationID, id SessionID) (err error) {
	defer func() { s.log(ctx, "close_session", c, err) }()
	sess, _, err := s.lookup(ctx, c, regID, id)
	if err != nil {
		return err
	}
	// The remote session is terminated with the credential only when current use
	// is confirmed now. Authority that ended, a credential that ended or a check
	// that cannot be answered still closes the session, but sends nothing.
	graceful := s.use(ctx, c, sess) == nil
	s.settleWith(sess, (*RemoteSession).Close, graceful)
	return nil
}

// Abandon records a session whose local protocol initialization failed after it
// was opened. It is idempotent and never fails the caller.
func (s *Service) Abandon(ctx context.Context, c Caller, regID RegistrationID, id SessionID) {
	if sess, _, err := s.lookup(ctx, c, regID, id); err == nil {
		s.settle(sess, (*RemoteSession).Close)
	}
}

// Sweep expires the tenant's sessions past their lifetime, closes their live
// handles and recovers stale dispatching receipts as unknown. It never resumes
// or resends anything. Composition calls it periodically per tenant.
func (s *Service) Sweep(ctx context.Context, tenant domain.TenantID, limit int) error {
	now := s.now()
	if _, err := s.cfg.Store.ExpireSessions(ctx, tenant, now, limit); err != nil {
		return err
	}
	if _, err := s.cfg.Store.RecoverStale(ctx, tenant, now.Add(-s.cfg.Options.StaleAfter), limit); err != nil {
		return err
	}
	s.mu.Lock()
	var due []SessionID
	for id, l := range s.open {
		if l.tenant == tenant && !now.Before(l.expires) {
			due = append(due, id)
		}
	}
	s.mu.Unlock()
	for _, id := range due {
		s.dropLive(id)
	}
	return nil
}

// Shutdown closes every live handle. Their sessions are recoverable only by
// being rejected: another process never adopts them.
func (s *Service) Shutdown() {
	s.mu.Lock()
	ids := make([]SessionID, 0, len(s.open))
	for id := range s.open {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.dropLive(id)
	}
}

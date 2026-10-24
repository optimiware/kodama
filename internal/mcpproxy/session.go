package mcpproxy

import (
	"regexp"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// SessionID is the opaque local identifier of a remote session. It is the only
// session identifier the workload ever sees: the remote server's own session
// identifier never leaves the proxy.
type SessionID string

var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{15,127}$`)

// Validate checks the identifier syntax.
func (id SessionID) Validate() error {
	if !sessionIDRE.MatchString(string(id)) {
		return invalidf("the session identifier is not valid")
	}
	return nil
}

// SessionState is the lifecycle of a remote session binding.
type SessionState string

const (
	// SessionInitializing: the binding exists, the remote handshake is under way.
	SessionInitializing SessionState = "initializing"
	// SessionActive: usable until expiry.
	SessionActive SessionState = "active"
	// SessionFailed: initialization did not complete. Terminal.
	SessionFailed SessionState = "failed"
	// SessionClosed: terminated by the caller or on attempt end. Terminal.
	SessionClosed SessionState = "closed"
	// SessionExpired: its lifetime ended. Terminal.
	SessionExpired SessionState = "expired"
	// SessionInvalid: its registration or credential changed, or the process
	// that owned it is gone. Terminal.
	SessionInvalid SessionState = "invalid"
)

// Terminal reports whether no transition leaves the state.
func (s SessionState) Terminal() bool {
	switch s {
	case SessionFailed, SessionClosed, SessionExpired, SessionInvalid:
		return true
	}
	return false
}

// Caller is the verified authority of a runtime request: the identifiers come
// from the workload's attempt token and durable grant, never from a request
// body, header or tool output.
type Caller struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	// Session is the runtime grant session the request authenticated with.
	Session string
}

// Validate checks the identifiers.
func (c Caller) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return ErrUnauthenticated
	}
	if c.RunID.Validate() != nil || c.AttemptID.Validate() != nil || c.Session == "" || len(c.Session) > 256 {
		return ErrUnauthenticated
	}
	return nil
}

// RemoteSession binds a local session to the complete caller tuple, the
// registration revision and credential version it froze, the protocol revision
// and the process incarnation that owns its live handle. Every field except
// State and UpdatedAt is immutable; terminal states never revive.
type RemoteSession struct {
	ID                  SessionID
	Caller              Caller
	Registration        RegistrationID
	RegistrationVersion domain.Version
	Credential          SecretRef
	Protocol            string
	Incarnation         string
	State               SessionState
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ExpiresAt           time.Time
}

// NewRemoteSession creates an initializing binding that expires after ttl.
func NewRemoteSession(id SessionID, c Caller, reg Registration, protocol, incarnation string, now time.Time, ttl time.Duration) (RemoteSession, error) {
	if err := id.Validate(); err != nil {
		return RemoteSession{}, err
	}
	if err := c.Validate(); err != nil {
		return RemoteSession{}, err
	}
	if c.Scope != reg.Scope || reg.Deleted() {
		return RemoteSession{}, ErrNotFound
	}
	if protocol == "" || incarnation == "" || ttl <= 0 {
		return RemoteSession{}, invalidf("a protocol, an incarnation and a lifetime are required")
	}
	now = micros(now)
	return RemoteSession{ID: id, Caller: c, Registration: reg.ID, RegistrationVersion: reg.Version, Credential: reg.Secret,
		Protocol: protocol, Incarnation: incarnation, State: SessionInitializing, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(ttl)}, nil
}

func (s *RemoteSession) move(from SessionState, to SessionState, now time.Time) error {
	if s.State != from {
		return transitionf("a %s session cannot become %s", s.State, to)
	}
	s.State, s.UpdatedAt = to, micros(now)
	return nil
}

// Activate completes initialization.
func (s *RemoteSession) Activate(now time.Time) error {
	if !now.Before(s.ExpiresAt) {
		return transitionf("the session already expired")
	}
	return s.move(SessionInitializing, SessionActive, now)
}

// Fail ends an initialization that did not complete.
func (s *RemoteSession) Fail(now time.Time) error {
	return s.move(SessionInitializing, SessionFailed, now)
}

// Close ends a live session at the caller's or the attempt's request.
func (s *RemoteSession) Close(now time.Time) error {
	if s.State != SessionInitializing && s.State != SessionActive {
		return transitionf("a %s session cannot be closed", s.State)
	}
	s.State, s.UpdatedAt = SessionClosed, micros(now)
	return nil
}

// Invalidate ends a live session whose registration, credential or owning
// process is gone.
func (s *RemoteSession) Invalidate(now time.Time) error {
	if s.State != SessionInitializing && s.State != SessionActive {
		return transitionf("a %s session cannot be invalidated", s.State)
	}
	s.State, s.UpdatedAt = SessionInvalid, micros(now)
	return nil
}

// Expire ends a live session whose lifetime is over. Calling it before the
// expiry is a transition error.
func (s *RemoteSession) Expire(now time.Time) error {
	if now.Before(s.ExpiresAt) {
		return transitionf("the session has not reached its expiry")
	}
	if s.State != SessionInitializing && s.State != SessionActive {
		return transitionf("a %s session cannot expire", s.State)
	}
	s.State, s.UpdatedAt = SessionExpired, micros(now)
	return nil
}

// Usable reports ErrSessionInvalid unless the session is active, unexpired and
// owned by the given process incarnation. Expiry is judged from the clock, so a
// session past its lifetime is unusable even before the sweep records it.
func (s RemoteSession) Usable(now time.Time, incarnation string) error {
	switch {
	case s.State != SessionActive, !now.Before(s.ExpiresAt), s.Incarnation != incarnation:
		return ErrSessionInvalid
	}
	return nil
}

// Owns reports whether c is exactly the caller that opened the session:
// tenant, project, run, attempt and runtime grant session.
func (s RemoteSession) Owns(c Caller) bool { return s.Caller == c }

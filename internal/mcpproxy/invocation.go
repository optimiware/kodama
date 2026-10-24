package mcpproxy

import (
	"regexp"
	"time"
)

// RequestID identifies one tool call within a session. Admission is idempotent
// per (session, request ID): a duplicate never dispatches a second time.
type RequestID string

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// Validate checks the identifier syntax.
func (id RequestID) Validate() error {
	if !requestIDRE.MatchString(string(id)) {
		return invalidf("the request identifier is not valid")
	}
	return nil
}

// InvocationState is the lifecycle of one tool call receipt.
type InvocationState string

const (
	// InvocationAdmitted: authority, ownership and the call ceiling were
	// checked; nothing was sent.
	InvocationAdmitted InvocationState = "admitted"
	// InvocationDispatching: the intent to send is durable. From here a send may
	// have happened, so a lost outcome can only become unknown.
	InvocationDispatching InvocationState = "dispatching"
	// InvocationSucceeded: the remote answered with a result.
	InvocationSucceeded InvocationState = "succeeded"
	// InvocationFailed: the remote answered with a protocol error. No rollback
	// is claimed.
	InvocationFailed InvocationState = "failed"
	// InvocationUnknown: the send may have happened and the outcome was lost,
	// cancelled or timed out. Never retried automatically; irreversible.
	InvocationUnknown InvocationState = "unknown"
)

// Terminal reports whether the outcome is final.
func (s InvocationState) Terminal() bool {
	switch s {
	case InvocationSucceeded, InvocationFailed, InvocationUnknown:
		return true
	}
	return false
}

// Invocation is the durable receipt of one tool call. It records identifiers,
// the qualified tool and a classified outcome, never arguments or results.
type Invocation struct {
	ID           RequestID
	Session      SessionID
	Caller       Caller
	Registration RegistrationID
	Tool         string
	State        InvocationState
	// Code is the bounded classified reason of a terminal state.
	Code      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewInvocation creates an admitted receipt.
func NewInvocation(id RequestID, s RemoteSession, tool string, now time.Time) (Invocation, error) {
	if err := id.Validate(); err != nil {
		return Invocation{}, err
	}
	if err := ValidateToolName(tool); err != nil {
		return Invocation{}, err
	}
	now = micros(now)
	return Invocation{ID: id, Session: s.ID, Caller: s.Caller, Registration: s.Registration, Tool: tool, State: InvocationAdmitted, CreatedAt: now, UpdatedAt: now}, nil
}

// Dispatch records the durable intent to send. Only an admitted call may be
// dispatched, so a receipt can be dispatched at most once.
func (i *Invocation) Dispatch(now time.Time) error {
	if i.State != InvocationAdmitted {
		return transitionf("a %s call cannot be dispatched", i.State)
	}
	i.State, i.UpdatedAt = InvocationDispatching, micros(now)
	return nil
}

func (i *Invocation) finish(to InvocationState, code string, now time.Time) error {
	if i.State != InvocationDispatching {
		return transitionf("a %s call cannot become %s", i.State, to)
	}
	i.State, i.Code, i.UpdatedAt = to, code, micros(now)
	return nil
}

// Succeed records a remote result.
func (i *Invocation) Succeed(now time.Time) error { return i.finish(InvocationSucceeded, "ok", now) }

// Fail records a definitive remote protocol error.
func (i *Invocation) Fail(code string, now time.Time) error {
	return i.finish(InvocationFailed, code, now)
}

// MarkUnknown records that the remote effect cannot be known. It is the only
// outcome of a dispatched call whose reply was lost, and it is final.
func (i *Invocation) MarkUnknown(code string, now time.Time) error {
	return i.finish(InvocationUnknown, code, now)
}

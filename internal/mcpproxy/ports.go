package mcpproxy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and owned by this module. Composition wires
// adapters; a nil required port fails closed and nothing here has an allow-all
// default.

// Permission names a management question put to the HumanAuthority.
type Permission string

const (
	// PermList: read registrations.
	PermList Permission = "mcp_server.list"
	// PermMutate: create, edit or delete a registration. It binds a project
	// credential to an endpoint, so adapters require both project settings and
	// credential management authority.
	PermMutate Permission = "mcp_server.mutate"
)

// HumanAuthority answers whether an authenticated human currently holds a
// permission in the scope. nil allows; ErrDenied or ErrNotFound refuse; any
// other error is an outage and never access.
type HumanAuthority interface {
	Authorize(ctx context.Context, principalID string, scope domain.Scope, perm Permission) error
}

// RuntimeAuthority verifies a workload's bearer for the tool.invoke action
// against the attempt named by the route, and answers current-authority
// questions during long-lived work. Errors are ErrUnauthenticated, ErrDenied or
// anything else for an outage.
type RuntimeAuthority interface {
	Authorize(ctx context.Context, bearer string, attempt domain.AttemptID) (Caller, error)
}

// AttemptPolicy returns the immutable profile policy pinned for the attempt.
// ErrNotFound means the attempt has no agent policy (no tool is permitted).
type AttemptPolicy interface {
	PolicyOf(ctx context.Context, c Caller) (ToolPolicy, error)
}

// Destinations is the installation's approved-destination policy.
type Destinations interface {
	// Approve checks a normalized endpoint (no network access). It returns
	// ErrDestination for an endpoint outside the approved set.
	Approve(endpoint string) error
}

// SecretChecker verifies, for registration management, that the typed secret
// reference names an MCP-kind secret of the same project at an exact,
// non-revoked version. It never reads a value.
type SecretChecker interface {
	Check(ctx context.Context, scope domain.Scope, ref SecretRef) error
}

// Credential is a short-lived remote bearer prepared for one session. Discard
// clears the secret bytes.
type Credential interface {
	Bearer() []byte
	// Recipient is the intended recipient (origin) declared by the credential.
	Recipient() string
	Discard()
}

// CredentialRequest names the exact frozen credential of a run binding.
type CredentialRequest struct {
	Caller       Caller
	Registration Registration
	// RequestID is the audit correlation identifier of the use.
	RequestID string
}

// CredentialSource opens the exact pinned credential version of a binding. It
// is called outside any SQL transaction and returns ErrCredential for a
// missing, revoked, foreign, malformed or mis-addressed credential.
type CredentialSource interface {
	Open(ctx context.Context, req CredentialRequest) (Credential, error)
}

// RemoteTarget is one approved remote server and the credential to present.
type RemoteTarget struct {
	Endpoint  string
	Recipient string
	Bearer    []byte
}

// RemoteTool is one tool a remote server advertises.
type RemoteTool struct {
	Name         string
	Title        string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// RemotePage is one page of a remote tool listing.
type RemotePage struct {
	Tools      []RemoteTool
	NextCursor string
}

// RemoteResult is a remote tool result as the pinned protocol's JSON.
type RemoteResult struct {
	JSON    json.RawMessage
	IsError bool
}

// RemoteFailure classifies a failed call. A reply error means the remote
// answered definitively with a protocol error; anything else (transport loss,
// deadline, cancellation after the send) leaves the effect unknown.
type RemoteFailure struct {
	Reply bool
	Code  string
}

func (f *RemoteFailure) Error() string { return "mcpproxy: remote call failed: " + f.Code }

// RemoteConn is one initialized remote session. Calls are never retried or
// replayed by the connection.
type RemoteConn interface {
	// Protocol is the negotiated protocol revision.
	Protocol() string
	ListTools(ctx context.Context, cursor string) (RemotePage, error)
	// CallTool returns a *RemoteFailure error for a failed call.
	CallTool(ctx context.Context, name string, args json.RawMessage) (RemoteResult, error)
	// Revoke erases the credential copy at once, without contacting the remote.
	// Every later request, including the session termination Close would send,
	// fails locally. The service calls it before closing any handle whose
	// credential has not just been confirmed usable.
	Revoke()
	Close() error
}

// RemoteTools opens remote sessions: the replaceable invocation seam. It
// establishes the destination, applies the credential only to the approved
// origin and verifies the pinned protocol revision. It is not a delegation
// framework: delegated end-user access belongs to the hosted integration.
type RemoteTools interface {
	Open(ctx context.Context, t RemoteTarget) (RemoteConn, error)
}

// RecordedCall is raw call content handed to an authorized recorder.
type RecordedCall struct {
	Caller       Caller
	Registration RegistrationID
	Tool         string
	RequestID    RequestID
	Arguments    json.RawMessage
	Result       json.RawMessage
	Outcome      InvocationState
}

// ContentRecorder is the optional sink for raw call content. Recording is off
// unless a recorder is configured, raw recording is enabled and the pinned
// transcript policy allows it. A recorder failure never retries a tool.
type ContentRecorder interface {
	Record(ctx context.Context, c RecordedCall) error
}

// Mutation is the audit context of a management write.
type Mutation struct {
	Actor     Actor
	RequestID string
	Now       time.Time
}

// BindingRequest asks the store to freeze (or confirm) the run binding of a
// registration for the caller's run.
type BindingRequest struct {
	Caller       Caller
	Registration RegistrationID
	Now          time.Time
}

// AdmitRequest asks the store to admit one call atomically.
type AdmitRequest struct {
	Invocation Invocation
	// Incarnation is the process that must own the session's live handle.
	Incarnation string
	// MaxCalls is the pinned run-wide ceiling, nil when unclaimed.
	MaxCalls *int64
	Now      time.Time
}

// SessionUse names one protected use of a live session: discovery, a
// dispatched call, or a periodic check while a call is in flight.
type SessionUse struct {
	// Caller is the verified caller; it must be exactly the session's owner.
	Caller       Caller
	Registration RegistrationID
	Session      SessionID
	// Incarnation is the process that must own the session's live handle.
	Incarnation string
	Now         time.Time
}

// Admission is the result of Admit. A Duplicate carries the stored receipt and
// the call must not be dispatched.
type Admission struct {
	Invocation Invocation
	Duplicate  bool
}

// Store is the persistence port. Every method is scoped by tenant and project;
// mutations and their safe audit entries commit together, and a commit whose
// outcome is unknown is reported so the caller never dispatches on it.
type Store interface {
	CreateRegistration(ctx context.Context, m Mutation, r Registration) error
	// ReplaceRegistration stores next (an edit or tombstone of the stored
	// version expected). A stale version is ErrVersionConflict. Bindings of the
	// previous revision become unusable.
	ReplaceRegistration(ctx context.Context, m Mutation, next Registration, expected domain.Version) error
	// GetRegistration returns the registration including a tombstone.
	GetRegistration(ctx context.Context, scope domain.Scope, id RegistrationID) (Registration, error)
	// ListRegistrations returns up to limit live registrations ordered by ID.
	ListRegistrations(ctx context.Context, scope domain.Scope, after RegistrationID, limit int) ([]Registration, error)

	// EnsureBinding freezes the registration revision and exact credential
	// version for the caller's run on first use and registers the secret pin in
	// the same transaction. A registration that changed or was deleted since is
	// ErrBindingInvalid (or ErrNotFound); the binding is never replaced.
	EnsureBinding(ctx context.Context, req BindingRequest) (Registration, error)

	CreateSession(ctx context.Context, s RemoteSession) error
	GetSession(ctx context.Context, scope domain.Scope, id SessionID) (RemoteSession, error)
	// SaveSession persists a transition from the given state. It reports
	// whether the row was in that state.
	SaveSession(ctx context.Context, s RemoteSession, from SessionState) (bool, error)
	// ExpireSessions marks live sessions of the tenant past their expiry as
	// expired and returns how many.
	ExpireSessions(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) (int, error)

	// Admit atomically rechecks current authority, the session, the registration
	// and the call ceiling, then records the dispatching receipt and its audit
	// entry. Nothing is dispatched unless Admit returned a non-duplicate.
	Admit(ctx context.Context, req AdmitRequest) (Admission, error)
	// Complete records the terminal outcome of a dispatching receipt.
	Complete(ctx context.Context, inv Invocation) error
	GetInvocation(ctx context.Context, scope domain.Scope, session SessionID, id RequestID) (Invocation, error)
	// RecoverStale turns dispatching receipts last updated before the cutoff
	// into unknown. It never requeues a call.
	RecoverStale(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) (int, error)
	// Recheck verifies the caller's authority is still current.
	Recheck(ctx context.Context, c Caller, now time.Time) error
	// CheckSessionUse answers, from current durable state and without any
	// network or key-provider call, whether the session may still be used: the
	// caller's runtime authority is current, the session belongs to exactly this
	// caller and is unexpired and owned by this process, the registration
	// revision is the frozen one, and the frozen exact credential version is not
	// revoked and still pinned (unreleased) for the run. A deleted secret whose
	// version is still pinned remains usable by that run. It returns
	// ErrUnauthenticated or ErrDenied when authority ended, ErrSessionInvalid,
	// ErrBindingInvalid or ErrCredential when the session or its credential is
	// definitively unusable, and ErrUnavailable when no conclusive answer was
	// obtained. An answer is never cached: it is not permission for a later use.
	CheckSessionUse(ctx context.Context, u SessionUse) error
}

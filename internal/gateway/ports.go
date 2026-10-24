package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ---- workload identity ------------------------------------------------------

// Workload is the verified authority of one attempt. Every field comes from the
// workload authority (the runtime token's durable grant), never from a request
// body or path value.
type Workload struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	// Session identifies the runtime session the authority was established for;
	// transaction-bound rechecks use it to re-evaluate the same grant.
	Session string
}

// Validate checks the identifiers.
func (w Workload) Validate() error {
	if err := firstErr(w.Scope.Validate(), w.RunID.Validate(), w.AttemptID.Validate()); err != nil {
		return fail(ErrUnauthenticated, "the workload identity is malformed")
	}
	return nil
}

// Workloads establishes workload authority. Authorize verifies the bearer for
// the route's attempt and the model.invoke action and returns the verified
// identity. Errors are classified: ErrUnauthenticated, ErrForbidden or
// ErrUnavailable; an outage is never access.
type Workloads interface {
	Authorize(ctx context.Context, attempt domain.AttemptID, bearer string) (Workload, error)
}

// ---- management authorization -----------------------------------------------

// Permission is a management permission the gateway asks about.
type Permission string

const (
	// PermRead reads the project's model settings (config:read).
	PermRead Permission = "config:read"
	// PermSettings changes model settings and recording (project:settings).
	PermSettings Permission = "project:settings"
)

// Actor is the human principal of a management request.
type Actor struct{ ID string }

// Authority answers the management authorization question with current state.
// nil allows; ErrHidden and ErrDenied refuse; anything else is an outage and is
// treated as a refusal.
type Authority interface {
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// ---- persistence ------------------------------------------------------------

// UpdateSettings is a settings replacement. The store rechecks the actor's
// permission inside the write transaction and applies the entity transition.
type UpdateSettings struct {
	Actor     Actor
	Scope     domain.Scope
	Expected  int64
	Edit      SettingsEdit
	RequestID string
	Now       time.Time
}

// SettingsResult is the stored settings and whether this call changed them.
type SettingsResult struct {
	Settings ModelSettings
	Changed  bool
}

// AdmitCommand asks the store to admit one request atomically.
type AdmitCommand struct {
	Workload  Workload
	RequestID RequestID
	Model     string
	// RequestBytes is the size of the request body, checked against the run's
	// limit. InputBound is the conservative input estimate. OutputMax is the
	// caller's output limit; 0 selects the run's limit.
	RequestBytes int64
	InputBound   int64
	OutputMax    int64
	Now          time.Time
	// RecordingAvailable says a recorder is wired: recording is effective only
	// when the run's policy, the current settings and a recorder all agree.
	RecordingAvailable bool
}

// Admission is the committed result of an admission.
type Admission struct {
	Request   ModelRequest
	Binding   RunBinding
	Recording bool
}

// FinalizeCommand settles one request with the observed outcome.
type FinalizeCommand struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	RequestID RequestID
	Outcome   Outcome
	Now       time.Time
}

// SettleCommand is a manual settlement of an unknown request. The gateway does
// not discover remote completion itself: the actor attests it, and the store
// authorizes the actor (project:settings) in the same transaction that releases
// the slot and records the attributable audit entry.
type SettleCommand struct {
	Actor     Actor
	Scope     domain.Scope
	RequestID RequestID
	Now       time.Time
}

// Store is the transactional persistence port. Every method is tenant scoped,
// carries explicit ownership predicates and is atomic. Methods that take a
// Workload recheck its current authority inside their transaction.
type Store interface {
	// Settings returns the stored settings or the virtual disabled revision.
	Settings(ctx context.Context, scope domain.Scope) (ModelSettings, error)
	// UpdateSettings authorizes, applies and stores a replacement with its audit
	// row and project.settings_changed event.
	UpdateSettings(ctx context.Context, cmd UpdateSettings) (SettingsResult, error)

	// Admit rechecks authority, captures or reads the run binding, validates the
	// model and limits and consumes the request count and the single slot. An
	// error means nothing was consumed; ErrUnknownCommit means the caller must
	// not send.
	Admit(ctx context.Context, cmd AdmitCommand) (Admission, error)
	// BeginForwarding rechecks authority and moves admitted to forwarding.
	BeginForwarding(ctx context.Context, w Workload, id RequestID, now time.Time) error
	// Heartbeat rechecks authority and records that the worker is alive. It
	// returns ErrSuperseded once the request is no longer in flight.
	Heartbeat(ctx context.Context, w Workload, id RequestID, now time.Time) error
	// Finalize commits usage, the token charge, the request outcome, the audit
	// row and the usage.recorded event together, exactly once. A request that is
	// already settled is ErrSuperseded and nothing changes.
	Finalize(ctx context.Context, cmd FinalizeCommand) (Usage, error)
	// Request and Usage read by request identity within the scope.
	Request(ctx context.Context, scope domain.Scope, id RequestID) (ModelRequest, error)
	Usage(ctx context.Context, scope domain.Scope, id RequestID) (Usage, error)
	// RecordingOutcome stores whether content recording succeeded.
	RecordingOutcome(ctx context.Context, scope domain.Scope, id RequestID, recorded bool) error
	// Abandoned lists requests in admitted or forwarding whose heartbeat is older
	// than before.
	Abandoned(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) ([]ModelRequest, error)
	// Abandon reconciles one abandoned request: an unsent request finishes
	// failed without charge; a forwarding request becomes unknown, is charged its
	// bound and keeps the run's slot. Already settled requests are ErrSuperseded.
	Abandon(ctx context.Context, scope domain.Scope, id RequestID, before time.Time, now time.Time) (Usage, error)
	// Settle records an authorized operator's attestation that an unknown
	// request's remote call ended and releases the run's slot without changing
	// the charge. The store rechecks the actor's permission inside the write
	// transaction before it touches the budget or the request.
	Settle(ctx context.Context, cmd SettleCommand) error
}

// ---- provider and credentials ------------------------------------------------

// Credential is a provider bearer secret. It prints as a redaction in every
// formatter and encoder; Reveal is for the transport, once per request, and
// Destroy clears the bytes.
type Credential struct{ secret []byte }

// NewCredential takes ownership of secret.
func NewCredential(secret []byte) *Credential { return &Credential{secret: secret} }

// Reveal returns the secret bytes. Never log them.
func (c *Credential) Reveal() []byte {
	if c == nil {
		return nil
	}
	return c.secret
}

// Destroy clears the secret.
func (c *Credential) Destroy() {
	if c != nil {
		clear(c.secret)
		c.secret = nil
	}
}

func (*Credential) String() string               { return "[redacted credential]" }
func (*Credential) GoString() string             { return "gateway.Credential{[redacted]}" }
func (*Credential) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte("[redacted credential]")) }
func (*Credential) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }
func (*Credential) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Credentials resolves the exact credential version a run's binding pins. It
// finalizes the credential's audit and revocation check before returning, so a
// revoked version is never used, and never substitutes another version.
// Errors are ErrCredential or ErrUnavailable and carry no credential detail.
type Credentials interface {
	Open(ctx context.Context, w Workload, b RunBinding, id RequestID) (*Credential, error)
	// Kind verifies, outside any request, that the referenced secret exists in
	// the project and is a model credential at the exact version.
	Check(ctx context.Context, scope domain.Scope, ref CredentialRef) error
}

// Destinations approves provider endpoints. It is consulted when settings are
// stored; the transport enforces the same policy again at every connection.
type Destinations interface {
	Approve(ctx context.Context, scope domain.Scope, e Endpoint) error
}

// Estimator computes the conservative input token bound of a completion.
type Estimator interface {
	InputBound(c Completion) (int64, error)
}

// ProviderCall is everything the transport may send. The destination and the
// credential come from the run's binding; the completion has been validated.
type ProviderCall struct {
	Endpoint   Endpoint
	Credential *Credential
	Completion Completion
	// MaxTokens is the enforced output limit sent to the provider.
	MaxTokens int64
}

// Chunk is one validated stream event. Data is the re-encoded JSON of a
// completion chunk (never the provider's raw bytes); Done marks the terminal
// [DONE] marker, which carries no data.
type Chunk struct {
	Data  []byte
	Model string
	// Usage is valid provider-reported usage; UsageInvalid reports a usage
	// member that was present but malformed or inconsistent.
	Usage        *TokenUsage
	UsageInvalid bool
	Finish       string
	Done         bool
}

// Stream is an open, validated provider stream.
type Stream interface {
	// Next returns the next chunk; io.EOF only after the Done chunk. Any other
	// error ends the stream: a *StreamError for protocol, truncation and idle
	// faults, or the context error.
	Next(ctx context.Context) (Chunk, error)
	// Destination reports where the request went.
	Destination() Destination
	// Close releases the connection. It stops local reading; it is not proof
	// that the provider stopped.
	Close() error
}

// FaultKind classifies a stream failure.
type FaultKind string

const (
	FaultTruncated FaultKind = "truncated"
	FaultIdle      FaultKind = "idle"
	FaultProtocol  FaultKind = "protocol"
	FaultTooLarge  FaultKind = "too_large"
)

// StreamError is a failure while reading a stream.
type StreamError struct {
	Kind FaultKind
	Err  error
}

func (e *StreamError) Error() string { return "gateway: stream " + string(e.Kind) }
func (e *StreamError) Unwrap() error { return e.Err }

// ProviderFault classifies a failure to open a stream.
type ProviderFault string

const (
	// ProviderDestination: the destination or a resolved address is not approved;
	// nothing was sent.
	ProviderDestination ProviderFault = "destination"
	// ProviderConnect: no connection was established (DNS, dial, TLS); nothing was sent.
	ProviderConnect ProviderFault = "connect"
	// ProviderRedirect: the provider answered with a redirect, which is never
	// followed. The request reached the approved destination only.
	ProviderRedirect ProviderFault = "redirect"
	// ProviderStatus: the provider answered with an error status.
	ProviderStatus ProviderFault = "status"
	// ProviderProtocol: the response is not the supported stream.
	ProviderProtocol ProviderFault = "protocol"
	// ProviderTimeout: the provider did not answer in time after the send.
	ProviderTimeout ProviderFault = "timeout"
)

// ProviderError is a failure before any response byte reached the workload.
type ProviderError struct {
	Kind   ProviderFault
	Status int
	// Sent reports whether request bytes may have reached the provider.
	Sent        bool
	Destination Destination
	Err         error
}

func (e *ProviderError) Error() string { return "gateway: provider " + string(e.Kind) }
func (e *ProviderError) Unwrap() error { return e.Err }

// Provider opens validated streams.
type Provider interface {
	Open(ctx context.Context, call ProviderCall) (Stream, error)
}

// ---- recording ---------------------------------------------------------------

// Exchange is the raw content of one request, handed to the content recorder
// only when recording is authorized and on. Frames hold the validated stream
// data the workload received.
type Exchange struct {
	Scope      domain.Scope
	RunID      domain.RunID
	AttemptID  domain.AttemptID
	RequestID  RequestID
	Completion Completion
	Frames     [][]byte
	// Truncated reports that Frames hit the recording bound.
	Truncated bool
}

// Recorder retains raw exchanges. Its storage, retention and deletion policy
// belong to its owner. A recorder error is visible on the request but never
// changes the workload's result or the usage record.
type Recorder interface {
	Record(ctx context.Context, x Exchange) error
}

// ---- response sink -----------------------------------------------------------

// Sink is the workload-facing writer of one streaming response. It is the only
// place bytes reach the caller; the use case never touches HTTP.
type Sink interface {
	// Started reports whether any response byte has been committed.
	Started() bool
	// Frame writes one validated data frame and flushes it.
	Frame(ctx context.Context, data []byte) error
	// Done writes the successful terminator.
	Done(ctx context.Context) error
	// Fail writes a sanitized in-stream error. It never writes a terminator, so
	// the caller cannot mistake the stream for a complete one.
	Fail(ctx context.Context, code string) error
}

// TenantSource enumerates tenants for reconciliation. It is required by
// Service.Reconcile; service construction does not require it.
type TenantSource interface {
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}

// ---- protocol ----------------------------------------------------------------

// Decoder strictly decodes a workload's request body into the supported
// Completion subset (see protocol.md). It rejects every unsupported member or
// mode with ErrInvalid and never returns a partially accepted request.
type Decoder interface {
	Decode(body []byte) (Completion, error)
}

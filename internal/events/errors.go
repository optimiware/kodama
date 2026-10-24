package events

import "errors"

// Stable failures. Adapters wrap them so callers can classify with errors.Is.
var (
	// ErrBacklogFull rejects a new admission because the tenant's unpublished
	// backlog reached its configured count or byte limit.
	ErrBacklogFull = errors.New("event backlog is full")
	// ErrConflict: an event ID was reused with different content.
	ErrConflict = errors.New("event ID already exists with different content")
	// ErrStaleAppend: an event not known to any retained record occurred before
	// the tenant's durable deduplication boundary, so it may be a retry of an
	// event whose delivery evidence was already pruned.
	ErrStaleAppend = errors.New("event is older than the deduplication boundary")
	// ErrFutureEvent: the event's occurredAt is later than the appending
	// process's clock plus the configured maximum clock skew.
	ErrFutureEvent = errors.New("event occurred beyond the permitted clock skew")
	// ErrStaleClaim: the caller does not hold the current fencing token.
	ErrStaleClaim = errors.New("stale publication claim")
	// ErrInvalidTransition: the requested state change is not allowed.
	ErrInvalidTransition = errors.New("invalid delivery state transition")
	// ErrUnclassified: a failure was recorded without a transient or permanent class.
	ErrUnclassified = errors.New("failure is not classified as transient or permanent")
	// ErrAlreadyCompleted: the inbox already holds a completed result.
	ErrAlreadyCompleted = errors.New("delivery already completed")
	// ErrQuarantined: the delivery is quarantined and needs authorized replay.
	ErrQuarantined = errors.New("delivery is quarantined")
	// ErrCursorExpired: a cursor precedes the observation retention floor.
	ErrCursorExpired = errors.New("observation cursor expired")
	// ErrScopeMismatch: an envelope's scope differs from the trusted scope.
	ErrScopeMismatch = errors.New("envelope scope does not match the trusted scope")
	// ErrUnknownSubject: a subject token maps to no tenant.
	ErrUnknownSubject = errors.New("subject token maps to no tenant")
	// ErrNotFound reports an absent record within the caller's tenant.
	ErrNotFound = errors.New("not found")

	// ErrUnauthorized: replay was requested without authorization.
	ErrUnauthorized = errors.New("replay is not authorized")
	// ErrReplayUnsafe: the consumer's effects are unclassified or unsafe to repeat.
	ErrReplayUnsafe = errors.New("replay is unsafe for this consumer")
	// ErrReplayBeyondHorizon: replay would reach events whose inbox evidence may be gone.
	ErrReplayBeyondHorizon = errors.New("replay reaches beyond the deduplication horizon")
	// ErrReplayRangeExpired: part of the range left observation retention.
	ErrReplayRangeExpired = errors.New("replay range is no longer retained")
	// ErrReplayFenced: a restore fence requires reconciliation first.
	ErrReplayFenced = errors.New("replay is fenced pending reconciliation")
	// ErrInvalidConfiguration reports rejected adapter or policy settings.
	ErrInvalidConfiguration = errors.New("invalid event bus configuration")
)

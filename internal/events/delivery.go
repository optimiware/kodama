package events

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// FailureClass classifies a delivery failure. The zero value is deliberately
// invalid: a retry or quarantine decision needs an explicit class.
type FailureClass string

const (
	// ClassTransient failures are retried with backoff (broker or database
	// unavailable, timeouts, an unknown publication outcome).
	ClassTransient FailureClass = "transient"
	// ClassPermanent failures are poison: retrying cannot succeed.
	ClassPermanent FailureClass = "permanent"
)

// Valid reports whether c is an explicit class.
func (c FailureClass) Valid() bool { return c == ClassTransient || c == ClassPermanent }

// ClassifiedError carries a failure class through handler and broker errors.
type ClassifiedError struct {
	Class FailureClass
	Err   error
}

func (e *ClassifiedError) Error() string { return fmt.Sprintf("%s: %v", e.Class, e.Err) }
func (e *ClassifiedError) Unwrap() error { return e.Err }

// Transient marks err as retryable.
func Transient(err error) error { return &ClassifiedError{Class: ClassTransient, Err: err} }

// Permanent marks err as poison.
func Permanent(err error) error { return &ClassifiedError{Class: ClassPermanent, Err: err} }

// ClassOf returns the explicit class carried by err, or "" when unclassified.
func ClassOf(err error) FailureClass {
	var ce *ClassifiedError
	if errors.As(err, &ce) && ce.Class.Valid() {
		return ce.Class
	}
	return ""
}

// ClassOrTransient returns err's class, treating unclassified failures as
// transient so an unknown outcome is retried and eventually quarantined
// instead of being dropped.
func ClassOrTransient(err error) FailureClass {
	if c := ClassOf(err); c != "" {
		return c
	}
	return ClassTransient
}

// MaxFailureMessage bounds a stored diagnostic. Payloads are never stored in
// failure messages.
const MaxFailureMessage = 512

// SafeText makes untrusted text storable in a PostgreSQL text column: NUL and
// invalid UTF-8, which text cannot represent (the insert would fail on every
// retry), become U+FFFD. Use it for every stored display value derived from a
// delivery, handler or broker error; original bytes are kept only in bytea.
func SafeText(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "\uFFFD")
}

// IsSafeText reports whether s is storable as-is by SafeText.
func IsSafeText(s string) bool { return SafeText(s) == s }

// BoundedMessage sanitizes (SafeText) and truncates a diagnostic to
// MaxFailureMessage bytes on a rune boundary.
func BoundedMessage(msg string) string {
	msg = SafeText(msg)
	if len(msg) <= MaxFailureMessage {
		return msg
	}
	cut := MaxFailureMessage
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}

// RetryPolicy is the deterministic exponential backoff shared by publication
// and consumption retries.
type RetryPolicy struct {
	// Initial is the first retry delay; each further attempt doubles it up to Max.
	Initial time.Duration
	Max     time.Duration
	// MaxAttempts is the number of consumer attempts after which a still-failing
	// delivery is quarantined. Publication retries are never abandoned.
	MaxAttempts int
}

// Validate requires explicit, consistent settings.
func (p RetryPolicy) Validate() error {
	switch {
	case p.Initial <= 0:
		return fmt.Errorf("%w: retry initial delay must be positive", ErrInvalidConfiguration)
	case p.Max < p.Initial:
		return fmt.Errorf("%w: retry max delay must not be below the initial delay", ErrInvalidConfiguration)
	case p.MaxAttempts < 1:
		return fmt.Errorf("%w: retry max attempts must be at least 1", ErrInvalidConfiguration)
	}
	return nil
}

// Delay is the wait after the given failed attempt (1-based).
func (p RetryPolicy) Delay(attempt int) time.Duration {
	d := p.Initial
	for i := 1; i < attempt && d < p.Max; i++ {
		d *= 2
	}
	if d > p.Max {
		d = p.Max
	}
	return d
}

// PublicationState is the lifecycle of one outbox row.
type PublicationState string

const (
	// PublicationPending: waiting for its first or next (retry) attempt.
	PublicationPending PublicationState = "pending"
	// PublicationClaimed: leased to one publisher under a fencing token.
	PublicationClaimed PublicationState = "claimed"
	// PublicationPublished: durably acknowledged by the broker.
	PublicationPublished PublicationState = "published"
)

// Publication is the delivery state of one immutable outbox event. Event
// identity and content are never part of it: they cannot change across
// retries. Transitions:
//
//	pending   -> claimed    (due)
//	claimed   -> claimed    (lease expired: reclaimable, a new token fences the old holder)
//	claimed   -> published  (current fencing token only)
//	claimed   -> pending    (classified failure, scheduled retry)
type Publication struct {
	TenantID       string
	EventID        string
	State          PublicationState
	Attempts       int
	ClaimToken     int64
	LeaseExpiresAt time.Time
	NextAttemptAt  time.Time
	PublishedAt    time.Time
	LastClass      FailureClass
	LastError      string
}

// Due reports whether a publisher may claim the row at now.
func (p Publication) Due(now time.Time) bool {
	switch p.State {
	case PublicationPending:
		return !p.NextAttemptAt.After(now)
	case PublicationClaimed:
		return !p.LeaseExpiresAt.After(now)
	}
	return false
}

// Claim leases the row and returns its new fencing token. The token strictly
// increases, so a worker holding an older token can never complete the row.
func (p *Publication) Claim(now time.Time, lease time.Duration) (int64, error) {
	if lease <= 0 {
		return 0, fmt.Errorf("%w: lease must be positive", ErrInvalidConfiguration)
	}
	if p.State == PublicationPublished {
		return 0, fmt.Errorf("%w: event %s is already published", ErrInvalidTransition, p.EventID)
	}
	if !p.Due(now) {
		return 0, fmt.Errorf("%w: event %s is not due or is leased to another publisher", ErrInvalidTransition, p.EventID)
	}
	p.State = PublicationClaimed
	p.ClaimToken++
	p.Attempts++
	p.LeaseExpiresAt = now.Add(lease)
	return p.ClaimToken, nil
}

func (p *Publication) requireClaim(token int64) error {
	if token != p.ClaimToken {
		return fmt.Errorf("%w: token %d, current %d", ErrStaleClaim, token, p.ClaimToken)
	}
	if p.State != PublicationClaimed {
		return fmt.Errorf("%w: event %s is %s, not claimed", ErrInvalidTransition, p.EventID, p.State)
	}
	return nil
}

// Complete records durable broker acknowledgment for the current claim.
func (p *Publication) Complete(token int64, now time.Time) error {
	if err := p.requireClaim(token); err != nil {
		return err
	}
	p.State = PublicationPublished
	p.PublishedAt = now
	p.LeaseExpiresAt = time.Time{}
	return nil
}

// Fail releases the current claim for a scheduled retry. The class must be
// explicit. A publication that cannot be published stays pending and visible;
// it is never dropped.
func (p *Publication) Fail(token int64, class FailureClass, message string, now time.Time, policy RetryPolicy) error {
	if err := p.requireClaim(token); err != nil {
		return err
	}
	if !class.Valid() {
		return ErrUnclassified
	}
	p.State = PublicationPending
	p.LastClass = class
	p.LastError = BoundedMessage(message)
	p.LeaseExpiresAt = time.Time{}
	p.NextAttemptAt = now.Add(policy.Delay(p.Attempts))
	return nil
}

// InboxState is the lifecycle of one (consumer, event) inbox entry.
type InboxState string

const (
	// InboxPending: not yet completed; a retry may be scheduled.
	InboxPending InboxState = "pending"
	// InboxCompleted: the local effect and this result committed together.
	InboxCompleted InboxState = "completed"
	// InboxQuarantined: poison or exhausted; only authorized replay reopens it.
	InboxQuarantined InboxState = "quarantined"
)

// InboxEntry is the delivery state of one event for one consumer. Transitions:
//
//	pending     -> completed
//	pending     -> pending      (transient failure while attempts remain)
//	pending     -> quarantined  (permanent failure, or attempts exhausted)
//	quarantined -> pending      (explicit authorized replay only)
//
// A completed entry never changes: duplicates observe it and do nothing.
type InboxEntry struct {
	TenantID      string
	Consumer      string
	EventID       string
	State         InboxState
	Attempts      int
	NextAttemptAt time.Time
	CompletedAt   time.Time
	LastClass     FailureClass
	LastError     string
	Replays       int
}

// NewInboxEntry returns a fresh pending entry.
func NewInboxEntry(tenantID, consumer, eventID string) InboxEntry {
	return InboxEntry{TenantID: tenantID, Consumer: consumer, EventID: eventID, State: InboxPending}
}

// Due reports whether an ordinary delivery or repair pass may run the entry at
// now: a pending entry with a scheduled retry is not due before that time, so
// broker duplicates, several subscriptions and repair passes cannot consume the
// attempt budget during backoff. Only an authorized replay overrides the
// schedule, explicitly, in the caller.
func (e InboxEntry) Due(now time.Time) bool {
	if e.State != InboxPending || e.NextAttemptAt.IsZero() {
		return true
	}
	return !e.NextAttemptAt.After(now)
}

// RetryIn is the time left until the scheduled retry (zero when due).
func (e InboxEntry) RetryIn(now time.Time) time.Duration {
	if e.Due(now) {
		return 0
	}
	return e.NextAttemptAt.Sub(now)
}

// Begin starts an attempt. A completed entry reports ErrAlreadyCompleted and a
// quarantined one ErrQuarantined; neither runs the local effect again.
func (e *InboxEntry) Begin() error {
	switch e.State {
	case InboxCompleted:
		return ErrAlreadyCompleted
	case InboxQuarantined:
		return ErrQuarantined
	}
	e.Attempts++
	return nil
}

// Complete marks the attempt successful.
func (e *InboxEntry) Complete(now time.Time) error {
	if e.State != InboxPending {
		return fmt.Errorf("%w: inbox entry is %s", ErrInvalidTransition, e.State)
	}
	e.State = InboxCompleted
	e.CompletedAt = now
	e.NextAttemptAt = time.Time{}
	return nil
}

// Fail records a classified failure of the current attempt. It returns true
// when the entry is now quarantined and false when a retry is scheduled.
func (e *InboxEntry) Fail(class FailureClass, message string, now time.Time, policy RetryPolicy) (quarantined bool, err error) {
	if e.State != InboxPending {
		return false, fmt.Errorf("%w: inbox entry is %s", ErrInvalidTransition, e.State)
	}
	if !class.Valid() {
		return false, ErrUnclassified
	}
	e.LastClass = class
	e.LastError = BoundedMessage(message)
	if class == ClassPermanent || e.Attempts >= policy.MaxAttempts {
		e.State = InboxQuarantined
		e.NextAttemptAt = time.Time{}
		return true, nil
	}
	e.NextAttemptAt = now.Add(policy.Delay(e.Attempts))
	return false, nil
}

// Quarantine moves a pending entry straight to quarantine, for deliveries that
// cannot be processed at all (unsupported version, scope mismatch).
func (e *InboxEntry) Quarantine(message string) error {
	if e.State != InboxPending {
		return fmt.Errorf("%w: inbox entry is %s", ErrInvalidTransition, e.State)
	}
	e.State = InboxQuarantined
	e.LastClass = ClassPermanent
	e.LastError = BoundedMessage(message)
	e.NextAttemptAt = time.Time{}
	return nil
}

// Reopen returns a quarantined entry to pending for a replay. The grant must
// name an authorized operator and a reason; replay history is kept by the
// caller's durable replay record, the attempt counter restarts.
func (e *InboxEntry) Reopen(grant ReplayGrant) error {
	if err := grant.Validate(); err != nil {
		return err
	}
	if e.State != InboxQuarantined {
		return fmt.Errorf("%w: only a quarantined entry can be replayed, entry is %s", ErrInvalidTransition, e.State)
	}
	e.State = InboxPending
	e.Attempts = 0
	e.Replays++
	e.NextAttemptAt = time.Time{}
	return nil
}

package chatops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// DeliveryID identifies one notification: one event to one destination. It is
// derived from the identity, so it is known before the record commits and a
// repeated or concurrent creation names the same delivery. It is also the
// receiver idempotency key and the notification ID in the payload.
type DeliveryID string

// Validate checks the identifier syntax.
func (id DeliveryID) Validate() error {
	if !idPattern.MatchString(string(id)) {
		return invalidf("delivery id must match the identifier syntax")
	}
	return nil
}

// DeliveryIDFor derives the delivery identity of (tenant, project, destination,
// event).
func DeliveryIDFor(scope domain.Scope, destination DestinationID, eventID string) DeliveryID {
	h := sha256.New()
	for _, part := range []string{"kodama/chatops-delivery/v1", string(scope.TenantID), string(scope.ProjectID), string(destination), eventID} {
		fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return DeliveryID("cdl_" + hex.EncodeToString(h.Sum(nil)[:16]))
}

// DeliveryState is the lifecycle of one delivery.
//
//	pending   -> claimed    (due, or after an authorized replay)
//	claimed   -> delivered  (current fencing token only)
//	claimed   -> pending    (classified transient failure with attempts left)
//	claimed   -> failed     (permanent failure, or retries exhausted)
//	claimed   -> claimed    (lease expired: reclaimed under a new fencing token)
//	pending   -> failed     (the destination's binding changed: refused, not redirected)
//	failed    -> pending    (authorized, reason-bearing replay only)
//
// delivered is final: a delivered record is never replayed.
type DeliveryState string

const (
	StatePending   DeliveryState = "pending"
	StateClaimed   DeliveryState = "claimed"
	StateDelivered DeliveryState = "delivered"
	StateFailed    DeliveryState = "failed"
)

// FailedReason says why a failed delivery stopped retrying.
type FailedReason string

const (
	// FailedPermanent: a classified permanent failure.
	FailedPermanent FailedReason = "permanent"
	// FailedExhausted: transient failures used every attempt of the cycle.
	FailedExhausted FailedReason = "exhausted"
	// FailedRefused: the destination binding changed before the send.
	FailedRefused FailedReason = "refused"
)

// FailureClass is the bounded, classified reason of a failed attempt. It never
// carries a URL, credential, response body or provider diagnostic.
type FailureClass string

const (
	// ClassUnreachable: the connection could not be made (DNS, refused, TLS
	// handshake) before any request byte was sent. Transient; not delivered.
	ClassUnreachable FailureClass = "unreachable"
	// ClassTimeout: the receiver did not answer in time. Transient; the request
	// may have been processed.
	ClassTimeout FailureClass = "timeout"
	// ClassConnectionLost: the connection ended after the request was sent.
	// Transient; the request may have been processed.
	ClassConnectionLost FailureClass = "connection_lost"
	// ClassBadResponse: the response violated the adapter's bounds. Transient;
	// the request may have been processed.
	ClassBadResponse FailureClass = "bad_response"
	// ClassReceiverError: the receiver answered 5xx. Transient.
	ClassReceiverError FailureClass = "receiver_error"
	// ClassRateLimited: the receiver answered 429. Transient.
	ClassRateLimited FailureClass = "rate_limited"
	// ClassRejected: the receiver refused the notification (other 4xx).
	// Permanent: replay after the receiver or destination is fixed.
	ClassRejected FailureClass = "rejected"
	// ClassRedirectRefused: the receiver answered a redirect, which is never
	// followed. Permanent.
	ClassRedirectRefused FailureClass = "redirect_refused"
	// ClassPolicyRefused: the outbound destination policy refused the endpoint
	// or the resolved address. Permanent.
	ClassPolicyRefused FailureClass = "policy_refused"
	// ClassCredentialUnavailable: the credential is missing, revoked or not
	// bound to this recipient. Nothing was sent. Permanent.
	ClassCredentialUnavailable FailureClass = "credential_unavailable"
	// ClassCredentialSourceDown: the credential source could not answer.
	// Nothing was sent. Transient.
	ClassCredentialSourceDown FailureClass = "credential_source_unavailable"
	// ClassDestinationChanged: the endpoint or credential binding changed since
	// the intent was created. Nothing was sent. Permanent.
	ClassDestinationChanged FailureClass = "destination_changed"
	// ClassLeaseExpired: a worker stopped without recording an outcome; the
	// request may have been processed.
	ClassLeaseExpired FailureClass = "lease_expired"
	// ClassInternal: an unclassified sender failure; treated as transient.
	ClassInternal FailureClass = "internal"
)

var failureClasses = map[FailureClass]bool{
	ClassUnreachable: false, ClassTimeout: false, ClassConnectionLost: false, ClassBadResponse: false, ClassReceiverError: false,
	ClassRateLimited: false, ClassRejected: true, ClassRedirectRefused: true, ClassPolicyRefused: true, ClassCredentialUnavailable: true,
	ClassCredentialSourceDown: false, ClassDestinationChanged: true, ClassLeaseExpired: false, ClassInternal: false,
}

// Known reports whether the class is one of the module's.
func (c FailureClass) Known() bool { _, ok := failureClasses[c]; return ok }

// Permanent reports whether retrying cannot help without a person acting.
func (c FailureClass) Permanent() bool { return failureClasses[c] }

// Outcome is the result of one send attempt, as the sender reports it. A sender
// never returns an error: every failure is classified here, so the worker has no
// transport policy of its own.
type Outcome struct {
	// Delivered: the receiver answered with a 2xx status.
	Delivered bool
	Class     FailureClass
	// Uncertain: the receiver may have processed the request although no 2xx was
	// observed (timeout, connection lost after sending, bad response).
	Uncertain  bool
	HTTPStatus int
	// Receipt is the bounded provider receipt, when the receiver supplied one
	// in the configured header. A missing receipt is never fabricated.
	Receipt    string
	RetryAfter time.Duration
}

// MaxReceiptBytes bounds a recorded provider receipt.
const MaxReceiptBytes = 128

// SanitizeReceipt keeps a receipt only if it is short printable ASCII.
func SanitizeReceipt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxReceiptBytes {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}

// RetryPolicy is the bounded retry schedule of one delivery cycle. It has no
// default: the installation supplies it.
type RetryPolicy struct {
	// Initial is the delay after the first failed attempt; the delay doubles up
	// to Max.
	Initial, Max time.Duration
	// MaxAttempts is the number of attempts of one cycle (a manual replay starts
	// a new cycle; the total history is preserved).
	MaxAttempts int
}

// Validate checks the policy.
func (p RetryPolicy) Validate() error {
	if p.Initial <= 0 || p.Max < p.Initial || p.MaxAttempts < 1 || p.MaxAttempts > 100 {
		return invalidf("a retry policy needs a positive initial delay, a maximum of at least that and 1 to 100 attempts")
	}
	return nil
}

// Delay is the wait after the given failed attempt (1-based) of a cycle.
func (p RetryPolicy) Delay(attempt int) time.Duration {
	d := p.Initial
	for i := 1; i < attempt && d < p.Max; i++ {
		if d > p.Max/2 { // doubling would reach or pass the maximum (and may overflow)
			d = p.Max
			break
		}
		d *= 2
	}
	if d > p.Max {
		d = p.Max
	}
	return d
}

// Delivery is one notification and its delivery state. Its identity (Scope,
// ID, DestinationID, EventID, EventType, Payload) is immutable. Attempts is the
// total over the delivery's life; CycleAttempts counts the current cycle only.
type Delivery struct {
	Scope         domain.Scope
	ID            DeliveryID
	DestinationID DestinationID
	EventID       string
	EventType     string
	// BindingRevision is the destination binding the intent was created under
	// (and only a replay changes it).
	BindingRevision int64
	// Payload is the exact projected bytes sent on every attempt.
	Payload       []byte
	PayloadDigest string

	State         DeliveryState
	Attempts      int
	CycleAttempts int
	Replays       int
	NextAttemptAt time.Time
	// ClaimToken is the fencing token: it increases with every claim, and a
	// finalization is honored only for the current one.
	ClaimToken     int64
	LeaseExpiresAt time.Time

	LastClass     FailureClass
	LastUncertain bool
	LastStatus    int
	Receipt       string
	DeliveredAt   time.Time
	FailedReason  FailedReason

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewDelivery creates a pending intent for one destination and event. The
// payload must be the projection of that event for this delivery.
func NewDelivery(d Destination, eventID, eventType string, payload []byte, now time.Time) (Delivery, error) {
	if eventID == "" || len(eventID) > 128 {
		return Delivery{}, invalidf("an event id is 1 to 128 bytes")
	}
	if !IsSupportedEvent(eventType) || len(payload) == 0 || len(payload) > MaxPayloadBytes {
		return Delivery{}, invalidf("the delivery names a forwarded event and a bounded payload")
	}
	return Delivery{
		Scope: d.Scope, ID: DeliveryIDFor(d.Scope, d.ID, eventID), DestinationID: d.ID, EventID: eventID, EventType: eventType,
		BindingRevision: d.BindingRevision, Payload: payload, PayloadDigest: PayloadDigest(payload),
		State: StatePending, NextAttemptAt: now.UTC(), Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}, nil
}

// Due reports whether a claim may start now: a pending delivery whose schedule
// has arrived, or a claimed one whose lease expired.
func (d Delivery) Due(now time.Time) bool {
	switch d.State {
	case StatePending:
		return !now.Before(d.NextAttemptAt)
	case StateClaimed:
		return !now.Before(d.LeaseExpiresAt)
	}
	return false
}

// ClaimResult reports what Claim did.
type ClaimResult struct {
	// Claimed: the caller now holds the delivery and must send.
	Claimed bool
	// Reclaimed: an expired claim was taken over; its attempt has an unknown
	// outcome (the previous holder may have sent).
	Reclaimed bool
	// Exhausted: the expired claim was the last attempt of the cycle; the
	// delivery is now failed instead of being sent again.
	Exhausted bool
}

// Claim starts an attempt under a new fencing token. It is allowed for a due
// pending delivery and for a claimed one whose lease expired (the previous
// attempt's outcome is then unknown and is not retried beyond the cycle's
// attempts).
func (d *Delivery) Claim(now time.Time, lease time.Duration, p RetryPolicy) (ClaimResult, error) {
	if lease <= 0 {
		return ClaimResult{}, invalidf("a claim lease is positive")
	}
	if !d.Due(now) {
		return ClaimResult{}, Fail(ErrInvalidTransition, nil, "the delivery is not due")
	}
	var res ClaimResult
	if d.State == StateClaimed {
		res.Reclaimed = true
		if d.CycleAttempts >= p.MaxAttempts {
			d.State, d.FailedReason, d.LastClass, d.LastUncertain, d.LastStatus = StateFailed, FailedExhausted, ClassLeaseExpired, true, 0
			d.LeaseExpiresAt = time.Time{}
			d.touch(now)
			res.Exhausted = true
			return res, nil
		}
	}
	d.State = StateClaimed
	d.Attempts++
	d.CycleAttempts++
	d.ClaimToken++
	d.LeaseExpiresAt = now.UTC().Add(lease)
	d.touch(now)
	res.Claimed = true
	return res, nil
}

func (d *Delivery) holds(token int64) error {
	if d.State != StateClaimed || d.ClaimToken != token {
		return Fail(ErrStaleClaim, nil, "the claim is no longer current")
	}
	return nil
}

func (d *Delivery) touch(now time.Time) {
	d.Version++
	d.UpdatedAt = now.UTC()
}

// Succeed records a 2xx outcome for the current claim. Delivered is final.
func (d *Delivery) Succeed(token int64, now time.Time, o Outcome) error {
	if err := d.holds(token); err != nil {
		return err
	}
	if !o.Delivered {
		return invalidf("only a delivered outcome completes a delivery")
	}
	d.State, d.DeliveredAt = StateDelivered, now.UTC()
	d.LastClass, d.LastUncertain, d.LastStatus, d.Receipt = "", false, o.HTTPStatus, SanitizeReceipt(o.Receipt)
	d.FailedReason, d.LeaseExpiresAt = "", time.Time{}
	d.touch(now)
	return nil
}

// Fail records a failed attempt for the current claim: a transient class with
// attempts left schedules a retry; a permanent class, or the last attempt of the
// cycle, stops with a visible failed state. It reports whether a retry was
// scheduled.
func (d *Delivery) Fail(token int64, now time.Time, o Outcome, p RetryPolicy) (retry bool, err error) {
	if err := d.holds(token); err != nil {
		return false, err
	}
	if o.Delivered || !o.Class.Known() {
		return false, invalidf("a failure carries a known class")
	}
	d.LastClass, d.LastUncertain, d.LastStatus = o.Class, o.Uncertain, o.HTTPStatus
	d.LeaseExpiresAt = time.Time{}
	switch {
	case o.Class.Permanent():
		d.State, d.FailedReason = StateFailed, FailedPermanent
	case d.CycleAttempts >= p.MaxAttempts:
		d.State, d.FailedReason = StateFailed, FailedExhausted
	default:
		delay := p.Delay(d.CycleAttempts)
		if o.RetryAfter > delay {
			delay = o.RetryAfter
		}
		if delay > p.Max {
			delay = p.Max
		}
		d.State, d.NextAttemptAt = StatePending, now.UTC().Add(delay)
		retry = true
	}
	d.touch(now)
	return retry, nil
}

// Refuse fails a delivery that is due for a claim before any send because the
// destination's binding no longer matches the one the intent was created under:
// the intent is not silently redirected, and an authorized replay is the
// explicit way to resend it to the current binding. A claimed delivery whose
// lease expired is refused the same way (its last attempt has an unknown
// outcome, which the history records).
func (d *Delivery) Refuse(now time.Time) error {
	if !d.Due(now) {
		return Fail(ErrInvalidTransition, nil, "only a due delivery is refused")
	}
	d.State, d.FailedReason, d.LastClass, d.LastUncertain, d.LastStatus = StateFailed, FailedRefused, ClassDestinationChanged, false, 0
	d.LeaseExpiresAt = time.Time{}
	d.touch(now)
	return nil
}

// MaxReasonBytes bounds a replay reason.
const MaxReasonBytes = 512

// Replay reopens a failed delivery for a new cycle under the destination's
// current binding revision. Only a failed delivery is replayed: a delivered one
// is final and a pending or claimed one is already moving. The reason is
// required and is recorded with the audit entry.
func (d *Delivery) Replay(now time.Time, reason string, currentBinding int64) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > MaxReasonBytes {
		return invalidf("a replay carries a reason of 1 to %d bytes", MaxReasonBytes)
	}
	if currentBinding < 1 {
		return invalidf("a replay needs the destination's current binding")
	}
	if d.State != StateFailed {
		return Fail(ErrInvalidTransition, nil, "only a failed delivery is replayed")
	}
	d.State, d.NextAttemptAt, d.CycleAttempts, d.FailedReason = StatePending, now.UTC(), 0, ""
	d.BindingRevision = currentBinding
	d.Replays++
	d.touch(now)
	return nil
}

// Attempt is one durable record of an attempt, written before the receiver is
// called and completed afterwards.
type Attempt struct {
	Number     int
	ClaimToken int64
	StartedAt  time.Time
	FinishedAt time.Time
	// Result is "", "delivered", "retry", "failed" or "abandoned".
	Result     string
	Class      FailureClass
	Uncertain  bool
	HTTPStatus int
	Receipt    string
	Replay     int
}

// Attempt result values.
const (
	ResultDelivered = "delivered"
	ResultRetry     = "retry"
	ResultFailed    = "failed"
	ResultAbandoned = "abandoned"
)

package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// SignalID is the opaque identifier derived from a SignalIdentity. Events carry
// it; the raw caller key never appears in events.
type SignalID string

// Validate checks the identifier syntax.
func (id SignalID) Validate() error {
	if !idRE.MatchString(string(id)) {
		return invalid("signal id %q", id)
	}
	return nil
}

// SignalIdentity is the tuple that identifies one signal: tenant, project,
// authenticated caller principal and the caller-supplied key. Two callers,
// projects or tenants that use the same key have unrelated identities.
type SignalIdentity struct {
	Scope domain.Scope `json:"scope"`
	// Caller is the authenticated caller principal.
	Caller string `json:"caller"`
	// Key is the caller-supplied key, 1 to MaxSignalKeyLength characters.
	Key string `json:"key"`
}

// Validate checks the tuple.
func (i SignalIdentity) Validate() error {
	if err := i.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(i.Caller) == "" {
		return invalid("a signal identity names its caller")
	}
	if n := utf8.RuneCountInString(i.Key); n < 1 || n > MaxSignalKeyLength || !utf8.ValidString(i.Key) {
		return invalid("a signal key is 1 to %d characters", MaxSignalKeyLength)
	}
	return nil
}

// ID derives the opaque signal identifier from the tuple. The derivation is a
// pure, collision-resistant function of every field, so a different tenant,
// project, caller or key yields an unrelated identifier.
func (i SignalIdentity) ID() SignalID {
	return SignalID("sig-" + digestFields("signal-identity", string(i.Scope.TenantID), string(i.Scope.ProjectID), i.Caller, i.Key))
}

// digestFields hashes length-prefixed fields so no two field lists collide.
func digestFields(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		h.Write([]byte(strconv.Itoa(len(f)) + ":" + f + ";"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SignalTarget is the original target of a signal: the run, step and wait it was
// bound to. A recorded signal stays bound to it and is never re-routed.
type SignalTarget struct {
	RunID   domain.RunID  `json:"runId"`
	StepKey domain.StepID `json:"stepKey"`
	WaitID  WaitID        `json:"waitId"`
}

// Validate checks the three identities.
func (t SignalTarget) Validate() error {
	if err := t.RunID.Validate(); err != nil {
		return err
	}
	if err := t.StepKey.Validate(); err != nil {
		return err
	}
	return t.WaitID.Validate()
}

// SignalRequest is one authenticated request to decide a wait. It addresses the
// wait by identity (an inbox resolution), by complete correlation (an external
// signal), or both, in which case they must agree. The shell has already
// authenticated the caller and rate-limited the request.
type SignalRequest struct {
	Identity SignalIdentity `json:"identity"`
	// WaitID addresses the wait by identity; empty when addressing by correlation.
	WaitID WaitID `json:"waitId,omitempty"`
	// Correlation addresses open waits by every key and value; it must be the
	// wait's complete correlation.
	Correlation map[string]string `json:"correlation,omitempty"`
	// ExpectedVersion, when non-zero, must equal the open wait's version.
	ExpectedVersion domain.Version `json:"expectedVersion,omitempty"`
	// Outcome is the chosen outcome; it must be declared by the wait.
	Outcome string `json:"outcome"`
	// Answer is the optional answer text, at most MaxAnswerBytes of UTF-8.
	Answer string `json:"answer,omitempty"`
	// Actor references the deciding actor.
	Actor string `json:"actor"`
}

// Validate checks the request's syntax and bounds. A syntactically invalid
// request is a caller error and is never recorded.
func (r SignalRequest) Validate() error {
	if err := r.Identity.Validate(); err != nil {
		return err
	}
	if r.WaitID == "" && len(r.Correlation) == 0 {
		return invalid("a signal addresses a wait by identity or by correlation")
	}
	if r.WaitID != "" {
		if err := r.WaitID.Validate(); err != nil {
			return err
		}
	}
	if len(r.Correlation) > MaxCorrelationKeys {
		return invalid("a signal correlates on at most %d keys", MaxCorrelationKeys)
	}
	for k, v := range r.Correlation {
		if !correlationKeyRE.MatchString(k) || v == "" || len(v) > MaxRequestBytes {
			return invalid("signal correlation %q", k)
		}
	}
	if r.ExpectedVersion < 0 {
		return invalid("expected version must not be negative")
	}
	if !outcomeRE.MatchString(r.Outcome) {
		return invalid("signal outcome %q", r.Outcome)
	}
	if len(r.Answer) > MaxAnswerBytes || !utf8.ValidString(r.Answer) {
		return invalid("a signal answer is at most %d bytes of UTF-8", MaxAnswerBytes)
	}
	if strings.TrimSpace(r.Actor) == "" {
		return invalid("a signal names its actor")
	}
	return nil
}

// Digest identifies the request's content: its address, outcome and answer. A
// repeat of a recorded key with a different digest is key reuse.
func (r SignalRequest) Digest() string {
	keys := slices.Sorted(maps.Keys(r.Correlation))
	fields := []string{"signal-request", "wait", string(r.WaitID), "correlation", strconv.Itoa(len(keys))}
	for _, k := range keys {
		fields = append(fields, k, r.Correlation[k])
	}
	fields = append(fields, "outcome", r.Outcome, "answer", r.Answer)
	return "sha256:" + digestFields(fields...)
}

// RefusalReason is the bounded reason a signal was not applied.
type RefusalReason string

// Refusal reasons. key_reuse is not among them: it is a response only (ErrKeyReuse).
const (
	// RefusalDecided: the addressed wait was already decided.
	RefusalDecided RefusalReason = "decided"
	// RefusalVersionConflict: the expected version did not match the open wait.
	RefusalVersionConflict RefusalReason = "version_conflict"
	// RefusalInvalidOutcome: the outcome was not declared by the wait.
	RefusalInvalidOutcome RefusalReason = "invalid_outcome"
	// RefusalUnknownWait: no wait in the caller's scope and authority matched.
	// A missing, foreign and unauthorized wait are indistinguishable.
	RefusalUnknownWait RefusalReason = "unknown_wait"
	// RefusalAmbiguousCorrelation: several open waits matched the correlation.
	RefusalAmbiguousCorrelation RefusalReason = "ambiguous_correlation"
)

// SignalRecord is the durable decision record of a signal whose target resolved
// inside the caller's scope: a winning signal, or a refusal (decided,
// version_conflict, invalid_outcome). It is self-contained: replaying it never
// reads the wait, step or run. It has no expiry and is removed only when its
// project is deleted, so ordinary wait and run retention never removes it.
type SignalRecord struct {
	Identity SignalIdentity `json:"identity"`
	ID       SignalID       `json:"id"`
	// Digest is the request digest the identity is bound to.
	Digest string `json:"digest"`
	// Target is the original target and never changes.
	Target SignalTarget `json:"target"`
	// Applied is true for the winning signal, whose Decision is the one it made.
	Applied bool `json:"applied"`
	// Refusal is the reason an unapplied signal lost; empty when Applied.
	Refusal RefusalReason `json:"refusal,omitempty"`
	// Decision is the complete decision: the one this signal made, or the one a
	// refused (decided) signal lost to. It is zero for version_conflict and
	// invalid_outcome, which have no decision yet.
	Decision   Decision  `json:"decision,omitempty"`
	RecordedAt time.Time `json:"recordedAt"`
}

// Validate checks the record is self-consistent and replayable.
func (r SignalRecord) Validate() error {
	if err := r.Identity.Validate(); err != nil {
		return err
	}
	if r.ID != r.Identity.ID() {
		return invalid("signal id does not derive from its identity")
	}
	if r.Digest == "" || r.RecordedAt.IsZero() {
		return invalid("a signal record has a request digest and a time")
	}
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if r.Applied {
		if r.Refusal != "" || r.Decision.Kind != DecisionResolved || r.Decision.SignalID != r.ID {
			return invalid("an applied signal records the resolved decision it made")
		}
		return r.Decision.Validate()
	}
	switch r.Refusal {
	case RefusalDecided:
		if err := r.Decision.Validate(); err != nil {
			return err
		}
		if r.Decision.SignalID == r.ID {
			return invalid("a refused signal cannot be the signal that decided")
		}
	case RefusalVersionConflict, RefusalInvalidOutcome:
		if r.Decision != (Decision{}) {
			return invalid("a %s refusal has no decision", r.Refusal)
		}
	default:
		return invalid("refusal %q needs a target and does not belong in a decision record", r.Refusal)
	}
	return nil
}

// Result returns the replayable result the record holds.
func (r SignalRecord) Result() SignalResult {
	return SignalResult{SignalID: r.ID, Applied: r.Applied, Refusal: r.Refusal, Target: r.Target, Decision: r.Decision}
}

// SignalTombstone is the smaller durable record of a refusal that resolved no
// target (unknown_wait, ambiguous_correlation). It has no target or decision,
// does not expire and is removed only with its project, so a retry can never be
// rerouted to a wait that opened later.
type SignalTombstone struct {
	Identity   SignalIdentity `json:"identity"`
	ID         SignalID       `json:"id"`
	Digest     string         `json:"digest"`
	Refusal    RefusalReason  `json:"refusal"`
	RecordedAt time.Time      `json:"recordedAt"`
}

// Validate checks the tombstone.
func (t SignalTombstone) Validate() error {
	if err := t.Identity.Validate(); err != nil {
		return err
	}
	if t.ID != t.Identity.ID() {
		return invalid("signal id does not derive from its identity")
	}
	if t.Digest == "" || t.RecordedAt.IsZero() {
		return invalid("a tombstone has a request digest and a time")
	}
	if t.Refusal != RefusalUnknownWait && t.Refusal != RefusalAmbiguousCorrelation {
		return invalid("a tombstone records unknown_wait or ambiguous_correlation, not %q", t.Refusal)
	}
	return nil
}

// Result returns the replayable result of the tombstone: a refusal with no target.
func (t SignalTombstone) Result() SignalResult {
	return SignalResult{SignalID: t.ID, Refusal: t.Refusal}
}

// SignalLookup is what the store holds for one signal identity: a decision
// record, a tombstone, or nothing. At most one is set.
type SignalLookup struct {
	Record    *SignalRecord    `json:"record,omitempty"`
	Tombstone *SignalTombstone `json:"tombstone,omitempty"`
}

// Found reports whether the identity has a durable record of either shape.
func (l SignalLookup) Found() bool { return l.Record != nil || l.Tombstone != nil }

// Validate checks that at most one shape is set and that it is valid.
func (l SignalLookup) Validate() error {
	switch {
	case l.Record != nil && l.Tombstone != nil:
		return invalid("an identity has a record or a tombstone, not both")
	case l.Record != nil:
		return l.Record.Validate()
	case l.Tombstone != nil:
		return l.Tombstone.Validate()
	}
	return nil
}

func (l SignalLookup) identity() SignalIdentity {
	if l.Record != nil {
		return l.Record.Identity
	}
	return l.Tombstone.Identity
}

func (l SignalLookup) digest() string {
	if l.Record != nil {
		return l.Record.Digest
	}
	return l.Tombstone.Digest
}

func (l SignalLookup) result() SignalResult {
	if l.Record != nil {
		return l.Record.Result()
	}
	return l.Tombstone.Result()
}

// SignalResult is what a caller receives for a signal. Applied means this signal
// decided the wait. An unapplied result is a refusal; a refusal that lost to a
// decision carries it, so the caller sees what happened. Replayed is true when
// the result was served from a durable record rather than newly decided.
type SignalResult struct {
	SignalID SignalID      `json:"signalId"`
	Applied  bool          `json:"applied"`
	Refusal  RefusalReason `json:"refusal,omitempty"`
	// Target is zero for a targetless refusal.
	Target   SignalTarget `json:"target,omitempty"`
	Decision Decision     `json:"decision,omitempty"`
	Replayed bool         `json:"replayed,omitempty"`
}

// SignalOutcome is the result of routing one signal and the durable writes the
// store commits atomically with it: at most one of Record and Tombstone, and the
// updated wait when the signal decided it or found it expired. A replay has none.
type SignalOutcome struct {
	Result    SignalResult
	Wait      *Wait
	Record    *SignalRecord
	Tombstone *SignalTombstone
}

// RouteSignal is the pure routing decision a StateStore applies in one
// transaction. prior is the durable lookup for the request's identity and waits
// are the candidate waits. authorized, when set, reports whether the caller may
// decide a wait; the shell supplies it from the caller's current authority.
//
// The order is fixed:
//
//  1. Identity lookup. A recorded identity returns its recorded result
//     unchanged, whatever the wait or run now is and however many other waits
//     have opened; it is never re-routed. A repeat with a different request
//     digest is ErrKeyReuse and writes nothing.
//  2. Address by wait identity inside the caller's tenant and project. A missing,
//     foreign, unauthorized or never-opened wait is unknown_wait, indistinguishably.
//  3. Address by correlation: only open waits of the caller's project whose
//     complete correlation equals the request's. Zero matches is unknown_wait,
//     several is ambiguous_correlation. Closed waits are never matched.
//  4. Decide: a decided wait refuses as decided with its recorded decision; an
//     open wait whose deadline was reached is expired and the signal refused as
//     decided; then expected version, then outcome; otherwise the wait is
//     resolved.
//
// Every refusal is recorded under the caller's own identity: a decision record
// when a target resolved, a tombstone otherwise.
func RouteSignal(req SignalRequest, prior SignalLookup, waits []Wait, authorized func(Wait) bool, now time.Time) (SignalOutcome, error) {
	if err := req.Validate(); err != nil {
		return SignalOutcome{}, err
	}
	if err := prior.Validate(); err != nil {
		return SignalOutcome{}, err
	}
	if now.IsZero() {
		return SignalOutcome{}, invalid("routing needs the store time")
	}
	id, digest := req.Identity.ID(), req.Digest()

	// 1. Identity lookup comes before any wait matching.
	if prior.Found() {
		if prior.identity() != req.Identity {
			return SignalOutcome{}, invalid("the recorded signal belongs to another identity")
		}
		if prior.digest() != digest {
			return SignalOutcome{}, ErrKeyReuse
		}
		res := prior.result()
		res.Replayed = true
		return SignalOutcome{Result: res}, nil
	}

	// 2 and 3. Address a wait inside the caller's scope.
	visible := func(w Wait) bool {
		return w.Scope == req.Identity.Scope && (authorized == nil || authorized(w))
	}
	var matches []Wait
	if req.WaitID != "" {
		for _, w := range waits {
			if w.ID == req.WaitID && w.State != WaitPending && visible(w) &&
				(len(req.Correlation) == 0 || maps.Equal(w.Correlation, req.Correlation)) {
				matches = append(matches, w)
			}
		}
		if len(matches) != 1 {
			return tombstone(req, id, digest, RefusalUnknownWait, now), nil
		}
	} else {
		for _, w := range waits {
			if w.State == WaitOpen && visible(w) && maps.Equal(w.Correlation, req.Correlation) {
				matches = append(matches, w)
			}
		}
		switch len(matches) {
		case 0:
			return tombstone(req, id, digest, RefusalUnknownWait, now), nil
		case 1:
		default:
			return tombstone(req, id, digest, RefusalAmbiguousCorrelation, now), nil
		}
	}

	// 4. Decide.
	w := matches[0]
	rec := SignalRecord{Identity: req.Identity, ID: id, Digest: digest, Target: w.Target(), RecordedAt: now}
	var updated *Wait
	switch {
	case w.State.Closed():
		rec.Refusal, rec.Decision = RefusalDecided, w.Decision
	case !now.Before(w.Deadline):
		d, err := w.Expire(now)
		if err != nil {
			return SignalOutcome{}, err
		}
		rec.Refusal, rec.Decision, updated = RefusalDecided, d, &w
	default:
		d, err := w.Resolve(req, now)
		switch {
		case errors.Is(err, ErrWaitStale):
			rec.Refusal = RefusalVersionConflict
		case errors.Is(err, ErrInvalidOutcome):
			rec.Refusal = RefusalInvalidOutcome
		case err != nil:
			return SignalOutcome{}, err
		default:
			rec.Applied, rec.Decision, updated = true, d, &w
		}
	}
	if err := rec.Validate(); err != nil {
		return SignalOutcome{}, fmt.Errorf("routing built an invalid record: %w", err)
	}
	return SignalOutcome{Result: rec.Result(), Wait: updated, Record: &rec}, nil
}

func tombstone(req SignalRequest, id SignalID, digest string, reason RefusalReason, now time.Time) SignalOutcome {
	t := SignalTombstone{Identity: req.Identity, ID: id, Digest: digest, Refusal: reason, RecordedAt: now}
	return SignalOutcome{Result: t.Result(), Tombstone: &t}
}

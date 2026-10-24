package events

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// MaxReplaySpan bounds the observation range of one replay request.
const MaxReplaySpan = 10000

// Operator identifies who requested a replay; it becomes the audit actor.
type Operator struct{ ID string }

// ReplayRequest asks to re-run one consumer over a bounded observation range of
// one scope. Replay is targeted: it is never broadcast to other consumers.
type ReplayRequest struct {
	Scope    Scope
	Consumer string
	// FromSeq and ToSeq are the inclusive observation sequence range.
	FromSeq, ToSeq int64
	Reason         string
	Operator       Operator
	// AllowBeyondHorizon acknowledges that some events may have lost their
	// inbox evidence; it only helps consumers declared idempotent beyond it.
	AllowBeyondHorizon bool
}

// Validate checks the explicit scope, range, consumer, reason and operator.
func (r ReplayRequest) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if !consumerNamePattern.MatchString(r.Consumer) {
		return fmt.Errorf("%w: consumer name must match %s", ErrReplayUnsafe, consumerNamePattern)
	}
	if r.FromSeq < 1 || r.ToSeq < r.FromSeq {
		return fmt.Errorf("%w: replay needs an explicit range 1 <= from <= to", ErrReplayUnsafe)
	}
	if r.ToSeq-r.FromSeq+1 > MaxReplaySpan {
		return fmt.Errorf("%w: replay range exceeds %d events", ErrReplayUnsafe, MaxReplaySpan)
	}
	if err := freeText("reason", r.Reason, 512); err != nil {
		return err
	}
	return freeText("operator", r.Operator.ID, 256)
}

func freeText(field, v string, max int) error {
	if n := len([]rune(strings.TrimSpace(v))); n < 1 || len([]rune(v)) > max {
		return fmt.Errorf("%w: replay %s must be 1 to %d characters", ErrUnauthorized, field, max)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: replay %s contains a control character", ErrUnauthorized, field)
		}
	}
	return nil
}

// ReplayAuthorizer decides whether an operator may replay a consumer in a
// scope. The composition root backs it with platform authorization; events
// never authorize business actions themselves.
type ReplayAuthorizer interface {
	AuthorizeReplay(ctx context.Context, operator Operator, scope Scope, consumer string) error
}

// ReplayGrant is proof that a ReplayRequest passed Authorize. Its fields are
// unexported, so a zero or hand-built value never validates.
type ReplayGrant struct {
	request    ReplayRequest
	authorized bool
}

// Authorize validates the request and asks the authorizer. A nil authorizer
// fails closed.
func Authorize(ctx context.Context, a ReplayAuthorizer, req ReplayRequest) (ReplayGrant, error) {
	if err := req.Validate(); err != nil {
		return ReplayGrant{}, err
	}
	if a == nil {
		return ReplayGrant{}, fmt.Errorf("%w: no authorizer is configured", ErrUnauthorized)
	}
	if err := a.AuthorizeReplay(ctx, req.Operator, req.Scope, req.Consumer); err != nil {
		return ReplayGrant{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return ReplayGrant{request: req, authorized: true}, nil
}

// Validate reports ErrUnauthorized unless the grant came from Authorize.
func (g ReplayGrant) Validate() error {
	if !g.authorized {
		return ErrUnauthorized
	}
	return g.request.Validate()
}

// Request returns the authorized request.
func (g ReplayGrant) Request() ReplayRequest { return g.request }

// ReplayClassification is the dry-run result: what a replay would touch, by
// the delivery evidence each event has.
type ReplayClassification struct {
	// Events is the number of retained events in range the consumer accepts.
	Events int
	// AlreadyCompleted entries are skipped; their effect is never repeated.
	AlreadyCompleted int
	// Quarantined entries are reopened by the replay.
	Quarantined int
	// Pending entries have a retry scheduled and are processed now.
	Pending int
	// Undelivered events have no inbox entry yet and are within the horizon.
	Undelivered int
	// BeyondHorizon events have no inbox entry and are older than the inbox
	// retention: whether they were processed is unknowable.
	BeyondHorizon int
	// Expired counts range positions already pruned from observation history.
	Expired int
	// Effect is the consumer's declared effect class.
	Effect EffectClass
	// Fenced is true while a restore fence awaits reconciliation.
	Fenced bool
}

// Check decides whether the replay is allowed for the consumer.
func (c ReplayClassification) Check(spec ConsumerSpec, allowBeyondHorizon bool) error {
	switch {
	case c.Expired > 0:
		return fmt.Errorf("%w: %d position(s) were pruned", ErrReplayRangeExpired, c.Expired)
	case c.Fenced:
		return fmt.Errorf("%w: clear the fence after reconciling external effects", ErrReplayFenced)
	case spec.Effect != EffectLocal && spec.Effect != EffectExternalIntent:
		return fmt.Errorf("%w: consumer %s effects are unclassified", ErrReplayUnsafe, spec.Name)
	case c.BeyondHorizon > 0:
		if allowBeyondHorizon && spec.Effect == EffectLocal && spec.IdempotentBeyondHorizon {
			return nil
		}
		return fmt.Errorf("%w: %d event(s) lost inbox evidence; an explicit safe replay procedure is required", ErrReplayBeyondHorizon, c.BeyondHorizon)
	}
	return nil
}

package events

import (
	"fmt"
	"regexp"
	"slices"
)

// ProcessingMode states how a consumer treats observations that are older than
// what it already applied. Global ordering is never promised.
type ProcessingMode string

const (
	// ModeCurrentState consumers reload the authoritative aggregate and apply
	// version guards, so a stale observation never regresses state.
	ModeCurrentState ProcessingMode = "current_state"
	// ModeHistorical consumers (audit/export, notifications) process every
	// unique event, including older ones and distinct events sharing a version.
	ModeHistorical ProcessingMode = "historical"
)

// Observes reports whether an event observing resource version observed should
// change a consumer whose state already reflects version applied.
func (m ProcessingMode) Observes(applied, observed int64) bool {
	if m == ModeHistorical {
		return true
	}
	return observed > applied
}

// EffectClass classifies what a consumer's handler does, which decides whether
// replay is safe.
type EffectClass string

const (
	// EffectLocal: effects commit in the consumer's own database transaction.
	EffectLocal EffectClass = "local"
	// EffectExternalIntent: the handler only records durable intents; the
	// owning module's idempotent worker performs and reconciles the external call.
	EffectExternalIntent EffectClass = "external_intent"
	// EffectUnknown: unclassified. Replay is rejected.
	EffectUnknown EffectClass = "unknown"
)

var consumerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ConsumerSpec describes one durable consumer identity. The inbox is keyed by
// (tenant, Name, event ID), so renaming a consumer forfeits its deduplication
// evidence; names are stable identifiers.
type ConsumerSpec struct {
	Name string
	Mode ProcessingMode
	// Effect is declared by the consumer's owner.
	Effect EffectClass
	// IdempotentBeyondHorizon states that repeating the local effect after its
	// inbox evidence was pruned is harmless. Without it, replay beyond the
	// deduplication horizon is refused.
	IdempotentBeyondHorizon bool
	// Types restricts delivery to these event types; empty accepts all.
	Types []string
}

// Validate checks the identity and declared behavior.
func (c ConsumerSpec) Validate() error {
	if !consumerNamePattern.MatchString(c.Name) {
		return fmt.Errorf("%w: consumer name must match %s", ErrInvalidConfiguration, consumerNamePattern)
	}
	if c.Mode != ModeCurrentState && c.Mode != ModeHistorical {
		return fmt.Errorf("%w: consumer %s has no processing mode", ErrInvalidConfiguration, c.Name)
	}
	switch c.Effect {
	case EffectLocal, EffectExternalIntent, EffectUnknown:
	default:
		return fmt.Errorf("%w: consumer %s has no effect class", ErrInvalidConfiguration, c.Name)
	}
	if c.IdempotentBeyondHorizon && c.Effect != EffectLocal {
		return fmt.Errorf("%w: only local effects can be idempotent beyond the horizon", ErrInvalidConfiguration)
	}
	return nil
}

// Accepts reports whether the consumer handles an event type.
func (c ConsumerSpec) Accepts(eventType string) bool {
	return len(c.Types) == 0 || slices.Contains(c.Types, eventType)
}

// Package token declares the live credential-invalidation seam.
//
// Revocation is committed as a credential mutation; delivery of the resulting
// invalidation to consumers is a separate live effect. An invalidation names
// exactly one execution or service principal in one scope, a declared reason
// and an authoritative time. A lost invalidation delivery is retried; it never
// re-enables a revoked credential.
package token

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// InvalidationReason is one declared reason a current credential stops being
// usable.
type InvalidationReason string

const (
	// InvalidationRotated reports that a newer credential replaced the pair.
	InvalidationRotated InvalidationReason = "rotated"
	// InvalidationRevoked reports an explicit revocation.
	InvalidationRevoked InvalidationReason = "revoked"
	// InvalidationDisabled reports that the principal was disabled.
	InvalidationDisabled InvalidationReason = "disabled"
)

// InvalidationReasons returns every declared reason in declaration order.
func InvalidationReasons() []InvalidationReason {
	return []InvalidationReason{InvalidationRotated, InvalidationRevoked, InvalidationDisabled}
}

// Valid reports whether the reason is one of the declared reasons.
func (reason InvalidationReason) Valid() bool {
	for _, candidate := range InvalidationReasons() {
		if reason == candidate {
			return true
		}
	}
	return false
}

// Invalidation is one live credential invalidation. It names exactly one
// execution or service principal and never carries token material.
type Invalidation struct {
	Scope       tenancy.Scope      `json:"scope"`
	PrincipalID id.PrincipalID     `json:"principal_id"`
	ExecutionID id.ExecutionID     `json:"execution_id,omitempty"`
	ServiceID   service.ServiceID  `json:"service_id,omitempty"`
	Reason      InvalidationReason `json:"reason"`
	At          time.Time          `json:"at"`
}

// Validate checks the scope, principal, single target, declared reason and
// authoritative time. Naming both an execution and a service, or neither, is
// refused.
func (invalidation Invalidation) Validate() error {
	if err := invalidation.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParsePrincipalID(string(invalidation.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "invalidation principal id is invalid", err)
	}
	switch {
	case invalidation.ExecutionID != "" && invalidation.ServiceID != "":
		return fault.New(fault.InvalidInput, "invalidation names both an execution and a service")
	case invalidation.ExecutionID != "":
		if _, err := id.ParseExecutionID(string(invalidation.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "invalidation execution id is invalid", err)
		}
	case invalidation.ServiceID != "":
		if _, err := service.ParseServiceID(string(invalidation.ServiceID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "invalidation service id is invalid", err)
		}
	default:
		return fault.New(fault.InvalidInput, "invalidation names neither an execution nor a service")
	}
	if !invalidation.Reason.Valid() {
		return fault.New(fault.InvalidInput, "invalidation reason is not frozen")
	}
	if invalidation.At.IsZero() {
		return fault.New(fault.InvalidInput, "invalidation time is zero")
	}
	return nil
}

// InvalidationPort delivers one committed invalidation to live consumers. It
// is a post-commit retrying effect, not part of the revocation transaction.
type InvalidationPort interface {
	Invalidate(context.Context, Invalidation) error
}

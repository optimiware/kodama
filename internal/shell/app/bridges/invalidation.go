// Service disablement invalidation bridge.
//
// A committed service disablement is delivered to live consumers as one
// token.Invalidation on the scope-qualified invalidation subject. The service
// account's principal identity is its service identity (the identity adapter
// enforces the same invariant), and disablement is always the
// "disabled" reason. The bridge performs no publication of its own: it maps
// the service vocabulary to the broker's public invalidation seam.
package bridges

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/token"
)

// Invalidator maps a service disablement to the broker invalidation seam.
type Invalidator struct {
	port token.InvalidationPort
}

var _ service.Invalidator = (*Invalidator)(nil)

// NewInvalidator builds the bridge over one committed-invalidation port.
func NewInvalidator(port token.InvalidationPort) (*Invalidator, error) {
	if port == nil {
		return nil, fault.New(fault.InvalidInput, "service invalidator requires an invalidation port")
	}
	return &Invalidator{port: port}, nil
}

// InvalidateService delivers one committed disablement. A malformed command is
// refused before the broker is called; the broker's error is returned for the
// caller's retryable post-commit effect.
func (invalidator *Invalidator) InvalidateService(ctx context.Context, invalidation service.ServiceInvalidation) error {
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "service invalidation was canceled", err)
	}
	if invalidator == nil || invalidator.port == nil {
		return fault.New(fault.InternalFailure, "service invalidator is not configured")
	}
	if err := invalidation.Scope.Validate(); err != nil {
		return err
	}
	if _, err := service.ParseServiceID(string(invalidation.ServiceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "service invalidation service id is invalid", err)
	}
	if invalidation.At.IsZero() {
		return fault.New(fault.InvalidInput, "service invalidation time is zero")
	}
	message := token.Invalidation{
		Scope:       invalidation.Scope,
		PrincipalID: id.PrincipalID(invalidation.ServiceID),
		ServiceID:   invalidation.ServiceID,
		Reason:      token.InvalidationDisabled,
		At:          invalidation.At,
	}
	if err := message.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "service invalidation was canceled", err)
	}
	return invalidator.port.Invalidate(ctx, message)
}

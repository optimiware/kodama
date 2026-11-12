// Post-commit credential delivery bridge.
//
// A token.Delivery names one committed execution credential and the execution
// whose mounted credential must be replaced. The bridge resolves the current
// runtime effect identity for that execution and asks the runtime adapter to
// replace only the mounted credential, never replaying or restarting work.
// Delivery is a post-commit effect: a failure is retryable and is reported as
// an uncertain external effect, never as a rolled-back commit. The token is
// passed to the runtime and never rendered in an error or log.
package bridges

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/token"
)

// CredentialRuntime is the narrow runtime seam needed to replace a mounted
// execution credential.
type CredentialRuntime interface {
	RefreshCredential(context.Context, engine.CredentialRequest) (engine.CredentialRefresh, error)
}

// ExecutionIdentityResolver resolves the current runtime effect identity of
// one execution. It is a composition-owned lookup, not a durable permission.
type ExecutionIdentityResolver interface {
	ResolveExecutionIdentity(context.Context, id.ExecutionID) (engine.EffectIdentity, error)
}

// Deliverer implements token.Deliverer over the runtime credential seam.
type Deliverer struct {
	runtime    CredentialRuntime
	identities ExecutionIdentityResolver
}

var _ token.Deliverer = (*Deliverer)(nil)

// NewDeliverer builds the delivery bridge. Both dependencies are required.
func NewDeliverer(runtime CredentialRuntime, identities ExecutionIdentityResolver) (*Deliverer, error) {
	if runtime == nil {
		return nil, fault.New(fault.InvalidInput, "credential deliverer requires a runtime")
	}
	if identities == nil {
		return nil, fault.New(fault.InvalidInput, "credential deliverer requires an execution identity resolver")
	}
	return &Deliverer{runtime: runtime, identities: identities}, nil
}

// Deliver replaces the mounted credential of one committed execution delivery.
func (deliverer *Deliverer) Deliver(ctx context.Context, delivery token.Delivery) error {
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "credential delivery was canceled", err)
	}
	if deliverer == nil || deliverer.runtime == nil || deliverer.identities == nil {
		return fault.New(fault.InternalFailure, "credential deliverer is not configured")
	}
	if err := delivery.Validate(); err != nil {
		return err
	}
	if delivery.ExecutionID == "" {
		return fault.New(fault.UnsupportedCapability, "the execution runtime cannot deliver a service credential")
	}
	identity, err := deliverer.identities.ResolveExecutionIdentity(ctx, delivery.ExecutionID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fault.Wrap(fault.DeadlineExpired, "credential delivery was canceled", ctxErr)
		}
		return fault.Wrap(fault.DependencyUnavailable, "credential delivery target cannot be resolved", err)
	}
	if identity.ExecutionID != delivery.ExecutionID || identity.WorkloadID == "" {
		return fault.New(fault.DependencyUnavailable, "credential delivery target does not name the delivered execution workload")
	}
	evidence, err := deliverer.runtime.RefreshCredential(ctx, engine.CredentialRequest{
		Identity:   identity,
		Credential: delivery.Token,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fault.Wrap(fault.DeadlineExpired, "credential delivery was canceled", ctxErr)
		}
		return fault.Wrap(fault.UncertainExternalEffect, "credential refresh failed", err)
	}
	if !evidence.Delivered {
		return fault.New(fault.UncertainExternalEffect, "credential refresh was not confirmed")
	}
	return nil
}

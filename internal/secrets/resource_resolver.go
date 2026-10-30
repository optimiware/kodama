package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ResourceGrant is the trusted, current authority of one run service: the secret
// bindings its committed spec pins. It is produced from the service's verified
// runtime token and execution's committed resource record, never by the request
// being served. A binding made for a step is not a binding for a service, even for
// the same reference: the grant carries only the service's own pins.
type ResourceGrant struct {
	Scope    domain.Scope
	RunID    domain.RunID
	Key      string
	Bindings []runtimev1.SecretBinding
}

func (g ResourceGrant) validate() error {
	if err := g.Scope.Validate(); err != nil {
		return err
	}
	if err := g.RunID.Validate(); err != nil {
		return err
	}
	if _, ok := serviceName(g.Key); !ok {
		return invalidf("a secret is delivered only to a run service")
	}
	return nil
}

func serviceName(key string) (string, bool) {
	const prefix = "service."
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return "", false
	}
	return key[len(prefix):], true
}

func (g ResourceGrant) declared(ref string, version int64) (runtimev1.SecretBinding, bool) {
	for _, b := range g.Bindings {
		if b.Ref == ref && b.Version == version {
			return b, true
		}
	}
	return runtimev1.SecretBinding{}, false
}

// ResourceAuthority returns the service's current grant. It returns
// ErrAttemptInactive for a stopped, revoked or expired resource and
// ErrAuthorityMissing when no trusted record exists; any other error is a
// dependency failure. Resolution fails closed on all of them.
type ResourceAuthority func(ctx context.Context) (ResourceGrant, error)

// ResolveResource releases one pinned binding to the run service named by the
// authority, with exactly the order of Resolve: current authority; exact
// declaration match; read and decrypt (no SQL transaction held across the
// provider call); authority again; finalization, which atomically rechecks
// revocation and the run's pin and audits the delivery to (run, resource key, ref,
// version). A reference outside the service's own pins is ErrUndeclared; a failure
// at any step releases nothing.
func (r *Resolver) ResolveResource(ctx context.Context, current ResourceAuthority, req runtimev1.SecretResolveRequest, requestID string) (runtimev1.SecretResolveResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.SecretResolveResponse{}, invalidf("%v", err)
	}
	if current == nil {
		return runtimev1.SecretResolveResponse{}, ErrAuthorityMissing
	}
	grant, err := resourceGrant(ctx, current)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, err
	}
	binding, ok := grant.declared(req.Ref, req.Version)
	if !ok {
		return runtimev1.SecretResolveResponse{}, ErrUndeclared
	}
	name, err := ParseSecretRef(binding.Ref)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, err
	}
	pending, err := r.source.Open(ctx, VersionRequest{Scope: grant.Scope, RunID: grant.RunID, SecretName: name, Version: binding.Version})
	if err != nil {
		return runtimev1.SecretResolveResponse{}, err
	}
	defer pending.Discard()

	again, err := resourceGrant(ctx, current)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, err
	}
	if _, ok := again.declared(req.Ref, req.Version); !ok || again.Scope != grant.Scope || again.RunID != grant.RunID || again.Key != grant.Key {
		return runtimev1.SecretResolveResponse{}, ErrUndeclared
	}
	if err := pending.Finalize(ctx, FinalizeInfo{RunID: grant.RunID, ResourceKey: grant.Key, Delivery: binding.Delivery, RequestID: requestID}); err != nil {
		return runtimev1.SecretResolveResponse{}, err
	}
	return runtimev1.SecretResolveResponse{
		Ref: binding.Ref, Version: binding.Version, Name: binding.Name, Delivery: binding.Delivery,
		ValueBase64: base64.StdEncoding.EncodeToString(pending.Value()),
	}, nil
}

func resourceGrant(ctx context.Context, current ResourceAuthority) (ResourceGrant, error) {
	g, err := current(ctx)
	if err != nil {
		switch {
		case errors.Is(err, ErrAttemptInactive), errors.Is(err, ErrAuthorityMissing):
			return ResourceGrant{}, err
		}
		return ResourceGrant{}, fmt.Errorf("%w: resource authority: %v", ErrAuthorityMissing, err)
	}
	if err := g.validate(); err != nil {
		return ResourceGrant{}, fmt.Errorf("%w: %v", ErrAuthorityMissing, err)
	}
	return g, nil
}

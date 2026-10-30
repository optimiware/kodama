package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// AttemptGrant is the trusted, current authority of one step attempt: its
// identity and the secret bindings its pinned definition declares. It is
// produced by the execution module's authenticated attempt record, never by
// the request being served.
type AttemptGrant struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	Bindings  []runtimev1.SecretBinding
}

func (g AttemptGrant) validate() error {
	if err := g.Scope.Validate(); err != nil {
		return err
	}
	if err := g.RunID.Validate(); err != nil {
		return err
	}
	return g.AttemptID.Validate()
}

// declared returns the binding matching an exact reference and version.
func (g AttemptGrant) declared(ref string, version int64) (runtimev1.SecretBinding, bool) {
	for _, b := range g.Bindings {
		if b.Ref == ref && b.Version == version {
			return b, true
		}
	}
	return runtimev1.SecretBinding{}, false
}

// AttemptAuthority is the consumer-owned port to the execution module. Until an
// adapter is wired (T-020/T-021/T-027), runtime resolution is unavailable:
// there is no permissive default and no caller-supplied "authorized" flag.
type AttemptAuthority interface {
	// Grant returns the attempt's current authority. It returns
	// ErrAttemptInactive for a cancelled, expired or revoked attempt and
	// ErrAuthorityMissing when no trusted record exists. Any other error is a
	// dependency failure; resolution fails closed on all of them.
	Grant(ctx context.Context, attempt domain.AttemptID) (AttemptGrant, error)
}

// VersionRequest names one exact pinned version for one run.
type VersionRequest struct {
	Scope      domain.Scope
	RunID      domain.RunID
	SecretName string
	Version    int64
}

// FinalizeInfo is what the finalization audit records about the delivery. It
// holds identifiers only. Exactly one subject receives the value: an attempt
// (AttemptID) or a run service (ResourceKey, with AttemptID empty).
type FinalizeInfo struct {
	AttemptID domain.AttemptID
	RunID     domain.RunID
	// ResourceKey is the run resource ("service.NAME") a pinned binding is
	// delivered to, for a delivery that is not to an attempt.
	ResourceKey string
	Delivery    runtimev1.SecretDelivery
	RequestID   string
}

// Subject is the audit identity of the receiver: the attempt, or
// "resource:<run>/<key>" for a run service.
func (i FinalizeInfo) Subject() string {
	if i.ResourceKey != "" {
		return "resource:" + string(i.RunID) + "/" + i.ResourceKey
	}
	return string(i.AttemptID)
}

// PendingValue is a decrypted value that has not been released. Finalize
// revalidates, atomically with the audit write, that the version is still
// resolvable (not revoked, still pinned and unreleased for the consumer) and
// commits the audit entry. Only after Finalize returns nil may Value be
// delivered. Discard clears the bytes.
type PendingValue interface {
	Value() []byte
	Finalize(ctx context.Context, info FinalizeInfo) error
	Discard()
}

// VersionSource is the replaceable version-resolution port. The standalone
// adapter reads this module's encrypted store; a hosted adapter (O-19/T-061)
// may resolve through the platform instead, but must keep exact version
// semantics: a pinned Kodama version is never resolved to a newer value.
type VersionSource interface {
	// Open reads and decrypts exactly the requested pinned version, outside any
	// SQL transaction. It returns ErrNotFound for a missing or foreign
	// reference, ErrNotPinned when the run does not retain it, ErrRevoked for a
	// revoked version, and a *KeyError for provider failures.
	Open(ctx context.Context, req VersionRequest) (PendingValue, error)
}

// ScanSource is the optional capability of a VersionSource to read a version the
// run was given for comparison only: source scanning and log redaction. Scanning is
// not use, so it still reads a version that was revoked (or whose run released its
// pins) after an earlier step received it; it must audit the read (as a scan, never
// as a delivery) before returning a value and must never feed a delivery path. A
// source without it cannot serve disclosures of a revoked version, and the caller
// fails closed.
type ScanSource interface {
	OpenForScan(ctx context.Context, req VersionRequest) (PendingValue, error)
}

// Resolver releases secret values to attempts that declared them.
type Resolver struct {
	authority AttemptAuthority
	source    VersionSource
}

// NewResolver requires both ports; a missing authority denies resolution.
func NewResolver(a AttemptAuthority, s VersionSource) (*Resolver, error) {
	if a == nil || s == nil {
		return nil, invalidf("attempt authority and version source are required")
	}
	return &Resolver{authority: a, source: s}, nil
}

// Resolve releases the declared reference/version to the attempt.
//
// Order: current attempt authority; exact declaration match; read and decrypt
// (no SQL transaction held across the provider call); authority again, because
// the provider call may have been slow; finalization, which atomically
// rechecks revocation and the consumer's pin and commits the audit entry. A
// failure or an uncertain commit at any step releases nothing. A retry is a
// new, separately authorized and audited resolution.
func (r *Resolver) Resolve(ctx context.Context, attempt domain.AttemptID, req runtimev1.SecretResolveRequest, requestID string) (runtimev1.SecretResolveResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.SecretResolveResponse{}, invalidf("%v", err)
	}
	grant, err := r.grant(ctx, attempt)
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

	// The decryption may have taken long enough for a cancellation: check
	// authority again, and require the same declaration.
	again, err := r.grant(ctx, attempt)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, err
	}
	if _, ok := again.declared(req.Ref, req.Version); !ok || again.Scope != grant.Scope || again.RunID != grant.RunID {
		return runtimev1.SecretResolveResponse{}, ErrUndeclared
	}
	if err := pending.Finalize(ctx, FinalizeInfo{AttemptID: attempt, RunID: grant.RunID, Delivery: binding.Delivery, RequestID: requestID}); err != nil {
		return runtimev1.SecretResolveResponse{}, err
	}
	// The base64 string is the contract's wire form and cannot be erased; the
	// decrypted buffer is cleared by Discard.
	return runtimev1.SecretResolveResponse{
		Ref: binding.Ref, Version: binding.Version, Name: binding.Name, Delivery: binding.Delivery,
		ValueBase64: base64.StdEncoding.EncodeToString(pending.Value()),
	}, nil
}

func (r *Resolver) grant(ctx context.Context, attempt domain.AttemptID) (AttemptGrant, error) {
	if err := attempt.Validate(); err != nil {
		return AttemptGrant{}, invalidf("attempt ID: %v", err)
	}
	g, err := r.authority.Grant(ctx, attempt)
	if err != nil {
		switch {
		case errors.Is(err, ErrAttemptInactive), errors.Is(err, ErrAuthorityMissing):
			return AttemptGrant{}, err
		}
		return AttemptGrant{}, fmt.Errorf("%w: attempt authority: %v", ErrAuthorityMissing, err)
	}
	if g.AttemptID != attempt {
		return AttemptGrant{}, fmt.Errorf("%w: authority answered for another attempt", ErrAuthorityMissing)
	}
	if err := g.validate(); err != nil {
		return AttemptGrant{}, fmt.Errorf("%w: %v", ErrAuthorityMissing, err)
	}
	return g, nil
}

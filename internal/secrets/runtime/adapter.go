// Package secretsruntime translates the runtime contract's secret resolution
// (runtimev1.SecretResolveRequest/Response, POST
// /runtime/v1/attempts/{attemptId}/secrets/resolve) to the secrets.Resolver and
// maps the resolver's classified failures to the runtime contract's error
// codes.
//
// It does not authenticate anyone and registers no route: the attempt token is
// verified by the execution module's runtime authenticator (T-021/T-027), which
// supplies the trusted attempt ID and registers the endpoint. The adapter is
// the only place where module errors become contract errors; a failure that is
// not recognized is reported as unavailable (never as success, never with
// detail), and nothing here caches or logs a value.
package secretsruntime

import (
	"context"
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

// Adapter wraps a Resolver.
type Adapter struct {
	resolver *secrets.Resolver
}

// NewAdapter requires a resolver.
func NewAdapter(r *secrets.Resolver) (*Adapter, error) {
	if r == nil {
		return nil, errors.New("secretsruntime: a resolver is required")
	}
	return &Adapter{resolver: r}, nil
}

// Resolve resolves one declared reference for the attempt. attempt must come
// from the verified runtime token, never from the request body. requestID is the
// server-generated request identifier recorded in the audit entry. The error is
// always a *runtimev1.ContractError.
func (a *Adapter) Resolve(ctx context.Context, attempt domain.AttemptID, req runtimev1.SecretResolveRequest, requestID string) (runtimev1.SecretResolveResponse, error) {
	resp, err := a.resolver.Resolve(ctx, attempt, req, requestID)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, Translate(err)
	}
	if err := resp.Validate(); err != nil {
		// A response that violates the contract is never delivered.
		return runtimev1.SecretResolveResponse{}, &runtimev1.ContractError{Code: domain.CodeUnavailable, Message: "the secret could not be delivered"}
	}
	return resp, nil
}

// Bound resolves for one fixed attempt, in the shape of the entrypoint's
// runtime client (which is itself bound to its attempt).
type Bound struct {
	Adapter   *Adapter
	Attempt   domain.AttemptID
	RequestID string
}

// ResolveSecret resolves a reference for the bound attempt.
func (b Bound) ResolveSecret(ctx context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	return b.Adapter.Resolve(ctx, b.Attempt, req, b.RequestID)
}

// Translate maps a resolver error to its runtime contract error. Messages are
// fixed texts: they never include the reference, the value or the cause, and a
// foreign or unknown reference is indistinguishable from a missing one.
func Translate(err error) *runtimev1.ContractError {
	var ke *secrets.KeyError
	switch {
	case errors.Is(err, secrets.ErrInvalid):
		return contract(domain.CodeInvalidRequest, "the secret request is not valid")
	case errors.Is(err, secrets.ErrAttemptInactive):
		return contract(runtimev1.CodeAttemptRevoked, "the attempt is no longer active")
	case errors.Is(err, secrets.ErrAuthorityMissing):
		return contract(domain.CodeInvalidCredential, "the attempt could not be authenticated")
	case errors.Is(err, secrets.ErrUndeclared):
		return contract(domain.CodeForbidden, "the secret is not declared for this attempt")
	case errors.Is(err, secrets.ErrRevoked):
		return contract(domain.CodeForbidden, "the secret version is revoked")
	case errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrNotPinned):
		return contract(domain.CodeNotFound, "the secret was not found")
	case errors.As(err, &ke), errors.Is(err, secrets.ErrUnavailable), errors.Is(err, secrets.ErrFinalizationUncertain),
		errors.Is(err, secrets.ErrInconsistent), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return contract(domain.CodeUnavailable, "the secret could not be delivered")
	}
	// Fail closed: an unrecognized failure never becomes success or detail.
	return contract(domain.CodeUnavailable, "the secret could not be delivered")
}

func contract(code domain.ErrorCode, msg string) *runtimev1.ContractError {
	return &runtimev1.ContractError{Code: code, Message: msg}
}

package client

import (
	"context"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// CredentialSource supplies the current credential for one authentication. It
// is read on every attempt so a rotated mounted token takes effect without a
// restart.
type CredentialSource interface {
	Credential(ctx context.Context) (Credential, error)
}

// Bearer reads the current credential from source and returns the Authorization
// header value. An unusable credential and a source failure both refuse with the
// stable unauthenticated class.
func Bearer(ctx context.Context, source CredentialSource, now time.Time) (string, error) {
	credential, err := source.Credential(ctx)
	if err != nil {
		return "", err
	}
	if !credential.Usable(now) {
		return "", fault.New(fault.Unauthenticated, "credential is missing, expired or unusable")
	}
	return "Bearer " + credential.Token, nil
}

// RetryAllowed reports whether one failure may be retried. Reads retry only
// transient dependency and internal failures. Mutations retry only a transient
// dependency failure and only when the caller supplied an idempotency key; an
// ambiguous effect, a stale version or changed content is never retried.
func RetryAllowed(code fault.Code, mutation, hasIdempotencyKey bool) bool {
	if mutation {
		return hasIdempotencyKey && code == fault.CodeDependencyUnavailable
	}
	return code == fault.CodeDependencyUnavailable || code == fault.CodeInternalFailure
}

// RetryRequest returns the same request content for a retry: the caller's
// idempotency key, expected version and input are preserved.
func RetryRequest(request api.Request) api.Request {
	retried := request
	if request.Input != nil {
		retried.Input = append([]byte(nil), request.Input...)
	}
	if request.ExpectedVersion != nil {
		version := *request.ExpectedVersion
		retried.ExpectedVersion = &version
	}
	return retried
}

// Transport is the Userland REST transport boundary. HTTPTransport is the
// concrete transport; the CLI, helper and Git report client depend only on this
// interface.
type Transport interface {
	Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error)
}

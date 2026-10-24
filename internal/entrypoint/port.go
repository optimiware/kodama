// Package entrypoint implements `kodama step`, the in-Pod entrypoint that runs
// one ordinary command through the attempt's source/result lifecycle: admit,
// check out the pinned commit, materialize secrets, run the command in its own
// process group, collect declared outputs, propose source, and submit one
// idempotent result. See README.md and docs/implementation/step-entrypoint.md.
package entrypoint

import (
	"context"
	"errors"
	"fmt"
	"io"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Runtime is the consumer-owned port for the attempt-scoped runtime API
// (/runtime/v1/attempts/{attemptId}). The HTTP adapter is Client; unit tests
// substitute a fake. The adapter owns the bearer token and bounded retries of
// transport failures; the orchestrator owns every decision about what an error
// means.
type Runtime interface {
	Admit(ctx context.Context, req runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error)
	RenewToken(ctx context.Context) (runtimev1.TokenResponse, error)
	Context(ctx context.Context) (runtimev1.Context, error)
	ResolveSecret(ctx context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error)
	ReserveArtifact(ctx context.Context, req runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error)
	CompleteArtifact(ctx context.Context, id domain.ArtifactID, req runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error)
	ProposeSource(ctx context.Context, req runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error)
	SubmitResult(ctx context.Context, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error)
}

// Uploader transfers one object to the short-lived instructions returned by an
// artifact reservation. The body is re-readable so a retry can start over.
type Uploader interface {
	Upload(ctx context.Context, target runtimev1.Upload, body io.ReaderAt, size int64) error
}

// APIError is a contract error response (or an unparseable non-2xx response
// with an empty Code) returned by the runtime API.
type APIError struct {
	Op        string
	Status    int
	Code      domain.ErrorCode
	Message   string
	RequestID string
	Details   map[string]any
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("%s: unexpected HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d %s: %s", e.Op, e.Status, e.Code, e.Message)
}

// TransportError means every bounded retry of a request failed with a
// transient condition (network error, 5xx, 429). The outcome of the request on
// the server is unknown; replaying the same payload is safe by contract.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return e.Op + ": transient failure: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// ProtocolError is a response that violates the contract (wrong status,
// undecodable or invalid body). It is never retried.
type ProtocolError struct {
	Op  string
	Err error
}

func (e *ProtocolError) Error() string { return e.Op + ": contract violation: " + e.Err.Error() }
func (e *ProtocolError) Unwrap() error { return e.Err }

// apiCode returns the contract error code of err, or "".
func apiCode(err error) domain.ErrorCode {
	var api *APIError
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

func isRevoked(err error) bool { return apiCode(err) == runtimev1.CodeAttemptRevoked }

func isTransport(err error) bool {
	var t *TransportError
	return errors.As(err, &t)
}

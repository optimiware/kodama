// Package runtimeauth gives each Kubernetes Job bounded authority that is
// independent of human tokens: one-time admission credentials, short-lived
// audience-bound runtime tokens, current-attempt and cancellation checks,
// signing-key rotation, explicit action scopes and safe logging.
//
// The package is the application layer. Entities (AdmissionGrant, ActionSet,
// Policy) hold the business transitions; the consumer-owned ports in ports.go
// describe what the execution owner, the Kubernetes executor, PostgreSQL and
// the token adapters must provide. Adapters live in the postgres, jwt and
// http sub-packages and depend inward. See README.md for the state machine,
// lock order and the obligations of T-020, T-021 and T-027.
package runtimeauth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Config wires a Service. Every dependency is required: there is no
// allow-all fallback for a missing adapter.
type Config struct {
	Store    Store
	Attempts AttemptObserver
	Jobs     JobObserver
	Tokens   TokenIssuer
	Verifier TokenVerifier
	Wrapper  WrapperPolicy
	Policy   Policy
	// Resources enables the preparation/service subjects of the run_shared model
	// (runtime v2). Without it every resource operation is unavailable.
	Resources *ResourceConfig
	// Git enables the runtime-issued Git grants of the source-set wire (the seed
	// and integration routes). Without it those operations are unavailable.
	Git *GitConfig
	// Clock defaults to time.Now; Random to crypto/rand; Logger to a discard
	// logger. The service logs only operation names, safe identifiers and the
	// classified outcome.
	Clock  func() time.Time
	Random io.Reader
	Logger *slog.Logger
}

// Service is the runtime-authority use-case layer.
type Service struct {
	cfg Config
}

// NewService validates the configuration.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("runtimeauth: a grant store is required")
	case cfg.Attempts == nil:
		return nil, errors.New("runtimeauth: an attempt observer is required (no allow-all fallback)")
	case cfg.Jobs == nil:
		return nil, errors.New("runtimeauth: a Job ownership observer is required (no allow-all fallback)")
	case cfg.Tokens == nil || cfg.Verifier == nil:
		return nil, errors.New("runtimeauth: token issuance and verification are required")
	case cfg.Wrapper == nil:
		return nil, errors.New("runtimeauth: a wrapper compatibility policy is required")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	if cfg.Resources != nil {
		if err := cfg.Resources.validate(); err != nil {
			return nil, err
		}
	}
	if cfg.Git != nil {
		if err := cfg.Git.validate(); err != nil {
			return nil, err
		}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{cfg: cfg}, nil
}

func (s *Service) now() time.Time { return s.cfg.Clock().UTC() }

// outcome names the classified result of an operation for logs.
func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, ErrInvalidCredential):
		return "invalid_credential"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrAttemptRevoked):
		return "attempt_revoked"
	case errors.Is(err, ErrAdmissionConsumed):
		return "admission_consumed"
	case errors.Is(err, ErrProfileUnsupported):
		return "profile_unsupported"
	case errors.Is(err, ErrResourceRevoked):
		return "resource_revoked"
	case errors.Is(err, ErrResourceUnbound):
		return "resource_unbound"
	case errors.Is(err, ErrContextVersionMismatch):
		return "context_version_mismatch"
	case errors.Is(err, ErrReceiptConflict):
		return "receipt_conflict"
	case errors.Is(err, ErrIdempotencyConflict):
		return "idempotency_conflict"
	case errors.Is(err, ErrRefused):
		return "refused"
	case errors.Is(err, ErrPublicationConflict):
		return "publication_conflict"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	}
	return "unavailable"
}

// logOp records one operation. Only the operation, a validated attempt
// identifier and the classified outcome are written: never credentials,
// tokens, nonces, key material, context bodies or the text of a cause.
func (s *Service) logOp(ctx context.Context, op string, attempt domain.AttemptID, err error) {
	level := slog.LevelInfo
	if err != nil && outcome(err) == "unavailable" {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{slog.String("op", op), slog.String("outcome", outcome(err))}
	if attempt.Validate() == nil {
		attrs = append(attrs, slog.String("attemptId", string(attempt)))
	}
	s.cfg.Logger.LogAttrs(ctx, level, "runtime authority", attrs...)
}

// missing converts an absent grant or attempt into the non-disclosing
// credential failure.
func missing(err error) error {
	if errors.Is(err, ErrNotFound) {
		return fail(ErrInvalidCredential, "the credential is not valid")
	}
	return err
}

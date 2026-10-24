// Package gateway is the project model gateway: it mediates one verified
// OpenAI-compatible endpoint (streaming chat completions) for attempts, with
// bounded usage. It owns project model settings, the immutable per-run policy
// binding, atomic request admission (a request cap and one unresolved request
// per run across replicas), request outcomes and usage records. Provider
// credentials stay behind it.
//
// The package is the application layer. Entities (ModelSettings, RunBinding,
// RunBudget, ModelRequest, Usage) hold the business invariants and transitions;
// the consumer-owned ports in ports.go describe what authorization, storage,
// credentials, the provider transport, destination approval and the optional
// content recorder must provide. Adapters live in the postgres, openai,
// adapters and http sub-packages and depend inward. See README.md for the
// state machine, lock order, recovery rules and the obligations of the
// integration owner, and protocol.md for the frozen streaming subset.
package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Timeouts bound the gateway's own phases. Provider timeouts belong to the
// transport.
type Timeouts struct {
	// Request bounds one Invoke including the stream.
	Request time.Duration
	// Recheck is the interval of the periodic current-authority check and
	// liveness heartbeat while a stream is open.
	Recheck time.Duration
	// Finalize bounds persisting an outcome after the caller's context ended.
	Finalize time.Duration
	// FinalizeAttempts is the number of database recovery attempts for a
	// finalization. They never repeat the provider call.
	FinalizeAttempts int
	// FinalizeBackoff is the pause between those attempts.
	FinalizeBackoff time.Duration
	// Record bounds the content recorder call.
	Record time.Duration
}

// DefaultTimeouts are conservative defaults.
func DefaultTimeouts() Timeouts {
	return Timeouts{Request: 10 * time.Minute, Recheck: 15 * time.Second, Finalize: 20 * time.Second, FinalizeAttempts: 4,
		FinalizeBackoff: 250 * time.Millisecond, Record: 10 * time.Second}
}

func (t Timeouts) valid() bool {
	return t.Request > 0 && t.Recheck > 0 && t.Finalize > 0 && t.FinalizeAttempts >= 1 && t.FinalizeAttempts <= 20 && t.FinalizeBackoff >= 0 && t.Record > 0
}

// Config wires a Service. Every dependency except Recorder, Tenants, Clock and
// Logger is required: a missing adapter fails construction, there is no
// allow-all fallback.
type Config struct {
	Store        Store
	Workloads    Workloads
	Authority    Authority
	Credentials  Credentials
	Provider     Provider
	Estimator    Estimator
	Decoder      Decoder
	Destinations Destinations
	// Recorder is optional. Without one, recording cannot be enabled and no
	// content is ever kept.
	Recorder Recorder
	// MaxRecordedBytes bounds the stream content handed to the recorder.
	MaxRecordedBytes int64
	// Tenants enumerates tenants for Reconcile. NewService accepts its absence
	// (a composition that never reconciles), but Reconcile fails without it.
	Tenants  TenantSource
	Timeouts Timeouts
	Clock    func() time.Time
	// Logger receives operation names, safe identifiers and codes only: never
	// content, credentials or provider text.
	Logger *slog.Logger
}

// Service is the model gateway's use-case layer.
type Service struct {
	cfg Config
}

// NewService validates the configuration.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("gateway: a store is required")
	case cfg.Workloads == nil:
		return nil, errors.New("gateway: a workload authority is required (no allow-all fallback)")
	case cfg.Authority == nil:
		return nil, errors.New("gateway: a management authority is required (no allow-all fallback)")
	case cfg.Credentials == nil:
		return nil, errors.New("gateway: a credential source is required")
	case cfg.Provider == nil:
		return nil, errors.New("gateway: a provider transport is required")
	case cfg.Estimator == nil:
		return nil, errors.New("gateway: a token estimator is required")
	case cfg.Decoder == nil:
		return nil, errors.New("gateway: a request decoder is required")
	case cfg.Destinations == nil:
		return nil, errors.New("gateway: a destination policy is required")
	}
	if cfg.Timeouts == (Timeouts{}) {
		cfg.Timeouts = DefaultTimeouts()
	}
	if !cfg.Timeouts.valid() {
		return nil, errors.New("gateway: timeouts are out of range")
	}
	if cfg.Recorder != nil && (cfg.MaxRecordedBytes < 1 || cfg.MaxRecordedBytes > 64<<20) {
		return nil, errors.New("gateway: a recorder needs MaxRecordedBytes between 1 and 64 MiB")
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

// RecordingAvailable reports whether a content recorder is wired.
func (s *Service) RecordingAvailable() bool { return s.cfg.Recorder != nil }

// ---- management -------------------------------------------------------------

func (s *Service) authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error {
	if err := scope.Validate(); err != nil {
		return fail(ErrInvalid, "the scope is malformed")
	}
	if actor.ID == "" {
		return fail(ErrUnauthenticated, "no actor")
	}
	switch err := s.cfg.Authority.Authorize(ctx, actor, scope, perm); {
	case err == nil:
		return nil
	case errors.Is(err, ErrHidden), errors.Is(err, ErrDenied):
		return err
	default:
		return FailWith(ErrUnavailable, err, "the authority could not answer")
	}
}

// GetSettings returns the project's model settings (the virtual disabled
// revision one when none were stored).
func (s *Service) GetSettings(ctx context.Context, actor Actor, scope domain.Scope) (ModelSettings, error) {
	if err := s.authorize(ctx, actor, scope, PermRead); err != nil {
		return ModelSettings{}, err
	}
	st, err := s.cfg.Store.Settings(ctx, scope)
	return st, Classify(err)
}

// ReplaceSettings replaces the settings under the expected revision. Before any
// write it checks the edit, the endpoint's approval, that the credential is a
// usable model secret in this project at the exact version, and that recording
// is only enabled when a recorder exists. The store then rechecks the actor's
// permission inside the write transaction.
func (s *Service) ReplaceSettings(ctx context.Context, actor Actor, scope domain.Scope, expected int64, edit SettingsEdit, requestID string) (SettingsResult, error) {
	if err := s.authorize(ctx, actor, scope, PermSettings); err != nil {
		return SettingsResult{}, err
	}
	n, err := edit.normalize()
	if err != nil {
		return SettingsResult{}, err
	}
	if edit.Recording && s.cfg.Recorder == nil {
		return SettingsResult{}, fail(ErrRecording, "no content recorder is configured")
	}
	if err := s.cfg.Destinations.Approve(ctx, scope, n.endpoint); err != nil {
		return SettingsResult{}, Classify(err)
	}
	if err := s.cfg.Credentials.Check(ctx, scope, edit.Credential); err != nil {
		return SettingsResult{}, Classify(err)
	}
	res, err := s.cfg.Store.UpdateSettings(ctx, UpdateSettings{Actor: actor, Scope: scope, Expected: expected, Edit: edit, RequestID: requestID, Now: s.now()})
	return res, Classify(err)
}

// UsageOf returns one request's usage record within the scope. Reading it
// requires config:read on the project.
func (s *Service) UsageOf(ctx context.Context, actor Actor, scope domain.Scope, id RequestID) (Usage, error) {
	if err := s.authorize(ctx, actor, scope, PermRead); err != nil {
		return Usage{}, err
	}
	if err := id.Validate(); err != nil {
		return Usage{}, err
	}
	u, err := s.cfg.Store.Usage(ctx, scope, id)
	return u, Classify(err)
}

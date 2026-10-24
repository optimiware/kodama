package adapters

import (
	"context"
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// AttemptSource is the read-only view of an attempt's durable record the policy
// and metadata readers need. execution.Store satisfies it through
// NewAttemptSource. Every lookup carries the scope: a foreign, unknown or
// mismatched attempt is not found.
type AttemptSource interface {
	Attempt(ctx context.Context, scope domain.Scope, run domain.RunID, step string, attempt domain.AttemptID) (execution.Attempt, error)
}

type storeSource struct{ store execution.Store }

// NewAttemptSource reads attempts through the execution store.
func NewAttemptSource(s execution.Store) (AttemptSource, error) {
	if s == nil {
		return nil, errors.New("agents/adapters: an execution store is required")
	}
	return storeSource{store: s}, nil
}

func (s storeSource) Attempt(ctx context.Context, scope domain.Scope, run domain.RunID, step string, id domain.AttemptID) (execution.Attempt, error) {
	owner, err := s.store.RunOf(ctx, scope, id)
	if err != nil {
		return execution.Attempt{}, err
	}
	if owner != run {
		return execution.Attempt{}, execution.ErrNotFound
	}
	agg, err := s.store.Get(ctx, scope, run)
	if err != nil {
		return execution.Attempt{}, err
	}
	at := agg.Attempt(id)
	if at == nil || at.StepID != step || at.Scope != scope {
		return execution.Attempt{}, execution.ErrNotFound
	}
	return *at, nil
}

// pinned decodes the agent configuration frozen into an attempt's context and
// checks that the context belongs to the requested scope and is consistent with
// the attempt record.
func pinned(at execution.Attempt, scope domain.Scope) (agents.WrapperConfig, error) {
	notFound := fmt.Errorf("%w: no agent attempt in this scope", agents.ErrNotFound)
	c, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil {
		return agents.WrapperConfig{}, agents.Unavailablef(err, "the pinned context is unreadable")
	}
	if c.Scope() != scope || c.AttemptID != at.ID || c.RunID != at.RunID || c.StepID != at.StepID {
		return agents.WrapperConfig{}, notFound
	}
	cfg, present, err := agents.DecodeArgs(c.Command.Args)
	if err != nil {
		return agents.WrapperConfig{}, agents.Unavailablef(err, "the pinned agent payload is invalid")
	}
	if !present {
		return agents.WrapperConfig{}, notFound
	}
	if err := cfg.Verify(c.Command.Image, c.Command.Command); err != nil {
		return agents.WrapperConfig{}, agents.Unavailablef(err, "the pinned agent payload does not match its command")
	}
	profile, baseline := cfg.Pins()
	if c.Profile == nil || *c.Profile != profile || c.DesignBaseline == nil || c.DesignBaseline.Digest != baseline.Digest {
		return agents.WrapperConfig{}, agents.Unavailablef(nil, "the pinned context lacks the agent pins")
	}
	return cfg, nil
}

// PolicyReader serves the immutable, pinned policy of one agent attempt to the
// gateway and tool proxy. The policy narrows what the attempt may request; it
// establishes no authority: the caller must still authorize the attempt through
// its workload identity. It reads the frozen context only, so a profile
// registered later never changes it.
type PolicyReader struct{ attempts AttemptSource }

// NewPolicyReader requires the attempt source.
func NewPolicyReader(a AttemptSource) (PolicyReader, error) {
	if a == nil {
		return PolicyReader{}, errors.New("agents/adapters: an attempt source is required")
	}
	return PolicyReader{attempts: a}, nil
}

// PolicyOf returns the pinned policy of an attempt, keyed by scope, run, step
// and attempt. A non-agent, foreign or unknown attempt is agents.ErrNotFound.
func (r PolicyReader) PolicyOf(ctx context.Context, scope domain.Scope, run domain.RunID, step string, attempt domain.AttemptID) (agents.Policy, error) {
	at, err := r.attempts.Attempt(ctx, scope, run, step, attempt)
	if err != nil {
		return agents.Policy{}, mapAttemptError(err)
	}
	cfg, err := pinned(at, scope)
	if err != nil {
		return agents.Policy{}, err
	}
	return cfg.Policy(), nil
}

func mapAttemptError(err error) error {
	switch {
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return fmt.Errorf("%w: no agent attempt in this scope", agents.ErrNotFound)
	}
	return agents.Unavailablef(err, "the attempt lookup failed")
}

// AttemptMetadata links one attempt to its profile, result and diagnostics. It
// is a view over the existing attempt record and its diagnostic artifact IDs,
// not a second ledger: model usage stays with the AI gateway.
type AttemptMetadata struct {
	AttemptID domain.AttemptID
	Profile   agents.ProfilePin
	// WrapperVersion and DesignBaselineDigest are the pinned wrapper version and
	// design baseline of the attempt.
	WrapperVersion       string
	DesignBaselineDigest runtimev1.Digest
	// TranscriptRecorded reports the pinned transcript policy. Whether content
	// exists is decided by the diagnostic artifacts, never by this flag.
	TranscriptRecorded bool
	Status             execution.AttemptStatus
	Reason             execution.Reason
	// Outcome, Question and DiagnosticArtifactIDs come from the accepted result
	// (empty until one is accepted). A needs_input question is preserved here
	// with the failed attempt. The artifact IDs still need artifact-module
	// authorization to be read.
	Outcome               runtimev1.Outcome
	Question              string
	DiagnosticArtifactIDs []domain.ArtifactID
}

// MetadataReader reads attempt attribution and result metadata.
type MetadataReader struct{ attempts AttemptSource }

// NewMetadataReader requires the attempt source.
func NewMetadataReader(a AttemptSource) (MetadataReader, error) {
	if a == nil {
		return MetadataReader{}, errors.New("agents/adapters: an attempt source is required")
	}
	return MetadataReader{attempts: a}, nil
}

// MetadataOf returns the attributable metadata of an agent attempt.
func (r MetadataReader) MetadataOf(ctx context.Context, scope domain.Scope, run domain.RunID, step string, attempt domain.AttemptID) (AttemptMetadata, error) {
	at, err := r.attempts.Attempt(ctx, scope, run, step, attempt)
	if err != nil {
		return AttemptMetadata{}, mapAttemptError(err)
	}
	cfg, err := pinned(at, scope)
	if err != nil {
		return AttemptMetadata{}, err
	}
	_, baseline := cfg.Pins()
	m := AttemptMetadata{
		AttemptID: at.ID, Profile: cfg.Profile, WrapperVersion: cfg.Content.Wrapper.Version, DesignBaselineDigest: baseline.Digest,
		TranscriptRecorded: cfg.Content.Transcript.Record, Status: at.Status, Reason: at.Reason,
	}
	if at.Result != nil {
		m.Outcome = at.Result.Outcome
		if at.Result.Question != nil {
			m.Question = *at.Result.Question
		}
		m.DiagnosticArtifactIDs = append([]domain.ArtifactID{}, at.Result.DiagnosticArtifactIDs...)
	}
	return m, nil
}

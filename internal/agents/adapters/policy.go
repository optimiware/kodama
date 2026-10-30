package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
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

// pinnedContext is the version-independent view of an attempt's frozen context
// the agent readers need: ownership, the frozen command and the agent pins. It
// is a projection, never a second source of truth: every field is read from the
// immutable document the attempt was admitted with.
type pinnedContext struct {
	// Version is the runtime context version the attempt was pinned to: 1 (the
	// original context, which has no version member), 2 (run_shared) or 3 (run_shared
	// with a source set).
	Version        int64
	Scope          domain.Scope
	RunID          domain.RunID
	StepID         string
	AttemptID      domain.AttemptID
	Command        runtimev1.Command
	Profile        *runtimev1.Profile
	DesignBaseline *runtimev1.DesignBaseline
	// V3 is the whole context when Version is 3, nil otherwise.
	V3 *runtimev3.ContextV3
}

// decodePinned reads the frozen context of an attempt by its version. A context of
// a version this reader does not know is unavailable (fail closed): the policy
// of an attempt is never guessed from a document of another shape. Each version
// is decoded by its own strict contract decoder, so a v1 context cannot be read
// as a looser v2 or v3 one and the reverse.
func decodePinned(raw []byte) (pinnedContext, error) {
	var head struct {
		ContextVersion *int64 `json:"contextVersion"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return pinnedContext{}, agents.Unavailablef(err, "the pinned context is unreadable")
	}
	switch {
	case head.ContextVersion == nil:
		c, err := runtimev1.DecodeRequest[runtimev1.Context](raw)
		if err != nil {
			return pinnedContext{}, agents.Unavailablef(err, "the pinned context is unreadable")
		}
		return pinnedContext{Version: 1, Scope: c.Scope(), RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, Command: c.Command, Profile: c.Profile, DesignBaseline: c.DesignBaseline}, nil
	case *head.ContextVersion == runtimev2.ContextVersion:
		c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](raw)
		if err != nil {
			return pinnedContext{}, agents.Unavailablef(err, "the pinned context is unreadable")
		}
		return pinnedContext{Version: 2, Scope: domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, Command: c.Command, Profile: c.Profile, DesignBaseline: c.DesignBaseline}, nil
	case *head.ContextVersion == runtimev3.ContextVersion:
		c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](raw)
		if err != nil {
			return pinnedContext{}, agents.Unavailablef(err, "the pinned context is unreadable")
		}
		return pinnedContext{Version: 3, Scope: c.Scope(), RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, Command: c.Command, Profile: c.Profile, DesignBaseline: c.DesignBaseline, V3: &c}, nil
	}
	return pinnedContext{}, agents.Unavailablef(nil, "the pinned context has an unsupported version")
}

// pinned decodes the agent configuration frozen into an attempt's context and
// checks that the context belongs to the requested scope and is consistent with
// the attempt record. The checks are the same for every context version: the
// scope, run, step and attempt must match, the payload must verify against the
// frozen command, and the profile and design baseline pins must be the ones the
// payload implies.
func pinned(at execution.Attempt, scope domain.Scope) (agents.WrapperConfig, pinnedContext, error) {
	notFound := fmt.Errorf("%w: no agent attempt in this scope", agents.ErrNotFound)
	c, err := decodePinned(at.Context)
	if err != nil {
		return agents.WrapperConfig{}, pinnedContext{}, err
	}
	if c.Scope != scope || c.AttemptID != at.ID || c.RunID != at.RunID || c.StepID != at.StepID {
		return agents.WrapperConfig{}, pinnedContext{}, notFound
	}
	cfg, present, err := agents.DecodeArgs(c.Command.Args)
	if err != nil {
		return agents.WrapperConfig{}, pinnedContext{}, agents.Unavailablef(err, "the pinned agent payload is invalid")
	}
	if !present {
		return agents.WrapperConfig{}, pinnedContext{}, notFound
	}
	if err := cfg.Verify(c.Command.Image, c.Command.Command); err != nil {
		return agents.WrapperConfig{}, pinnedContext{}, agents.Unavailablef(err, "the pinned agent payload does not match its command")
	}
	profile, baseline := cfg.Pins()
	if c.Profile == nil || *c.Profile != profile || c.DesignBaseline == nil || c.DesignBaseline.Digest != baseline.Digest {
		return agents.WrapperConfig{}, pinnedContext{}, agents.Unavailablef(nil, "the pinned context lacks the agent pins")
	}
	return cfg, c, nil
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
	cfg, _, err := pinned(at, scope)
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
	// ContextVersion is the runtime context version the attempt was pinned to (1, 2
	// or 3). SourceSetDigest and Checkouts are set only under version 3: the digest
	// identifies the run's pinned source set, and the checkouts are where each
	// source's changes are committed, in path order.
	ContextVersion  int64
	SourceSetDigest runtimev1.Digest
	Checkouts       []agents.Checkout
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
	cfg, c, err := pinned(at, scope)
	if err != nil {
		return AttemptMetadata{}, err
	}
	_, baseline := cfg.Pins()
	m := AttemptMetadata{
		AttemptID: at.ID, Profile: cfg.Profile, WrapperVersion: cfg.Content.Wrapper.Version, DesignBaselineDigest: baseline.Digest,
		TranscriptRecorded: cfg.Content.Transcript.Record, Status: at.Status, Reason: at.Reason, ContextVersion: c.Version,
	}
	if c.V3 != nil {
		// A source-set attempt is attributed to its source set and to the checkouts
		// the wrapper plans for it: the candidate is the set of their commits, not
		// one commit. A context the wrapper could not plan is not an agent attempt
		// anyone can attribute a candidate to.
		plan, err := agents.PlanCheckouts(*c.V3)
		if err != nil {
			return AttemptMetadata{}, agents.Unavailablef(err, "the pinned source set cannot be planned for an agent")
		}
		m.Checkouts = plan
		if c.V3.Workspace.SourceSetDigest != nil {
			m.SourceSetDigest = *c.V3.Workspace.SourceSetDigest
		}
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

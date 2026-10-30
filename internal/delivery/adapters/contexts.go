package adapters

import (
	"context"
	"encoding/json"
	"errors"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Runs reads persisted runs. execution.Store implements it.
type Runs interface {
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// Execution implements delivery.PinnedContexts and delivery.Producers over the
// execution owner's persisted attempts. The pinned context is the immutable
// document dispatch stored; this adapter never reads environments, secrets or
// settings.
type Execution struct{ runs Runs }

var (
	_ delivery.PinnedContexts   = (*Execution)(nil)
	_ delivery.PinnedSourceSets = (*Execution)(nil)
	_ delivery.Producers        = (*Execution)(nil)
)

// NewExecution requires the run reader.
func NewExecution(runs Runs) (*Execution, error) {
	if runs == nil {
		return nil, errors.New("delivery/adapters: an execution store is required")
	}
	return &Execution{runs: runs}, nil
}

func (e *Execution) attempt(ctx context.Context, scope domain.Scope, id domain.AttemptID) (*execution.Attempt, error) {
	run, err := e.runs.RunOf(ctx, scope, id)
	if err != nil {
		return nil, mapExecution(err)
	}
	agg, err := e.runs.Get(ctx, scope, run)
	if err != nil {
		return nil, mapExecution(err)
	}
	at := agg.Attempt(id)
	if at == nil || at.Scope != scope {
		return nil, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return at, nil
}

func mapExecution(err error) error {
	switch {
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrDenied):
		return delivery.Fail(delivery.ErrNotFound, err, "not found")
	}
	return delivery.Fail(delivery.ErrUnavailable, err, "execution could not answer")
}

// contextOf decodes an attempt's stored context. A runtime v3 context (a run of
// the internal Git service) is projected onto the shared members of the v1
// context the deployment checks read (scope, identifiers, command, outputs,
// environment pin); its source set is read by PinnedSourceSet. Every other
// context decodes exactly as before. v3 is nil for a context of another version.
func contextOf(at *execution.Attempt) (c runtimev1.Context, v3 *runtimev3.ContextV3, err error) {
	var head struct {
		ContextVersion int64 `json:"contextVersion"`
	}
	if json.Unmarshal(at.Context, &head) == nil && head.ContextVersion == runtimev3.ContextVersion {
		d, err := runtimev3.DecodeRequest[runtimev3.ContextV3](at.Context)
		if err != nil {
			return runtimev1.Context{}, nil, err
		}
		return runtimev1.Context{
			TenantID: d.TenantID, ProjectID: d.ProjectID, RunID: d.RunID, StepID: d.StepID, AttemptID: d.AttemptID, ActionKey: d.ActionKey,
			Definition: d.Definition, WorkItem: d.WorkItem, DesignBaseline: d.DesignBaseline, Profile: d.Profile, Command: d.Command,
			Workspace: runtimev1.Workspace{Access: d.Workspace.Access}, Inputs: d.Inputs, Outputs: d.Outputs, Secrets: d.Secrets,
			Environment: d.Environment, Limits: d.Limits,
		}, &d, nil
	}
	c, err = runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	return c, nil, err
}

// Pinned implements delivery.PinnedContexts. A foreign or unknown attempt is
// ErrNotFound.
func (e *Execution) Pinned(ctx context.Context, scope domain.Scope, id domain.AttemptID) (runtimev1.Context, error) {
	at, err := e.attempt(ctx, scope, id)
	if err != nil {
		return runtimev1.Context{}, err
	}
	c, _, err := contextOf(at)
	if err != nil {
		return runtimev1.Context{}, delivery.Fail(delivery.ErrUnavailable, err, "the pinned context is unreadable")
	}
	if c.Scope() != scope || c.AttemptID != id {
		return runtimev1.Context{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return c, nil
}

// PinnedSourceSet implements delivery.PinnedSourceSets: the digest of the run's
// pinned source set and, per repository, the head commit the attempt began from,
// read from the attempt's immutable v3 context, with the primary source (the
// first writable source in repository order, else the first). An attempt of
// another context version, or of a source-free run, has none.
func (e *Execution) PinnedSourceSet(ctx context.Context, scope domain.Scope, id domain.AttemptID) (delivery.PinnedSource, bool, error) {
	at, err := e.attempt(ctx, scope, id)
	if err != nil {
		return delivery.PinnedSource{}, false, err
	}
	c, v3, err := contextOf(at)
	if err != nil {
		return delivery.PinnedSource{}, false, delivery.Fail(delivery.ErrUnavailable, err, "the pinned context is unreadable")
	}
	if c.Scope() != scope || c.AttemptID != id {
		return delivery.PinnedSource{}, false, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	if v3 == nil || v3.Workspace.SourceSetDigest == nil || len(v3.Workspace.Sources) == 0 {
		return delivery.PinnedSource{}, false, nil
	}
	commits := make([]delivery.SourceCommit, len(v3.Workspace.Sources))
	for i, src := range v3.Workspace.Sources {
		commits[i] = delivery.SourceCommit{RepositoryID: src.RepositoryID, Commit: string(src.HeadCommit)}
	}
	primary := primaryOf(v3.Workspace.Sources)
	set, err := delivery.NewSourceSet(string(*v3.Workspace.SourceSetDigest), commits)
	if err != nil {
		return delivery.PinnedSource{}, false, delivery.Fail(delivery.ErrUnavailable, err, "the pinned source set is invalid")
	}
	p := v3.Workspace.Sources[primary]
	return delivery.PinnedSource{Set: set, PrimaryURL: p.RepositoryURL, PrimaryCommit: string(p.HeadCommit)}, true, nil
}

// primaryOf is the index of the primary source: the writable source with the
// smallest repository ID, else the source with the smallest repository ID.
func primaryOf(sources []runtimev3.SourceV3) int {
	primary := -1
	for i, src := range sources {
		if primary < 0 {
			primary = i
			continue
		}
		wi, wp := src.Access == gitv1.AccessWrite, sources[primary].Access == gitv1.AccessWrite
		if wi != wp && wi || wi == wp && src.RepositoryID < sources[primary].RepositoryID {
			primary = i
		}
	}
	return primary
}

// Produced implements delivery.Producers. Accepted is true only for a
// succeeded attempt whose accepted result holds the artifact as the value of a
// declared ociArtifact output; the tool image is the producer's immutable
// command image, never caller text.
func (e *Execution) Produced(ctx context.Context, scope domain.Scope, id domain.AttemptID, artifact domain.ArtifactID) (delivery.ProducedOutput, error) {
	at, err := e.attempt(ctx, scope, id)
	if err != nil {
		return delivery.ProducedOutput{}, err
	}
	out := delivery.ProducedOutput{Scope: scope, Attempt: id, Run: at.RunID, Step: at.StepID}
	pinned, _, err := contextOf(at)
	if err != nil {
		return delivery.ProducedOutput{}, delivery.Fail(delivery.ErrUnavailable, err, "the pinned context is unreadable")
	}
	out.ToolImage = pinned.Command.Image
	if at.Status != execution.AttemptSucceeded || at.Result == nil || at.Result.Outcome != runtimev1.OutcomeCompleted {
		return out, nil
	}
	for _, decl := range pinned.Outputs {
		if decl.Type != runtimev1.OutputOCIArtifact {
			continue
		}
		if v, ok := at.Result.Outputs[decl.Name]; ok && v.IsString() && v.String() == string(artifact) {
			out.Accepted = true
		}
	}
	return out, nil
}

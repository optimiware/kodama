package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/qa"
)

// Runs reads persisted runs. execution.Store implements it.
type Runs interface {
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// Attempts implements qa.Attempts over the execution owner's persisted
// attempts. The pinned context is the immutable document dispatch stored; it is
// verified against its stored digest and its own scope and identity before it
// is believed, and nothing else (profiles, environments, settings) is read.
type Attempts struct{ runs Runs }

var _ qa.Attempts = (*Attempts)(nil)

// NewAttempts requires the run reader.
func NewAttempts(r Runs) (*Attempts, error) {
	if r == nil {
		return nil, errors.New("qa/adapters: an execution store is required")
	}
	return &Attempts{runs: r}, nil
}

func mapExecution(err error) error {
	switch {
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrDenied):
		return qa.Failf(qa.ErrNotFound, "attempt not found")
	}
	return qa.Unavailablef(err, "execution could not answer")
}

// Facts implements qa.Attempts. A foreign or unknown attempt is qa.ErrNotFound.
func (a *Attempts) Facts(ctx context.Context, scope domain.Scope, id domain.AttemptID) (qa.AttemptFacts, error) {
	run, err := a.runs.RunOf(ctx, scope, id)
	if err != nil {
		return qa.AttemptFacts{}, mapExecution(err)
	}
	agg, err := a.runs.Get(ctx, scope, run)
	if err != nil {
		return qa.AttemptFacts{}, mapExecution(err)
	}
	at := agg.Attempt(id)
	if at == nil || at.Scope != scope || at.RunID != run {
		return qa.AttemptFacts{}, qa.Failf(qa.ErrNotFound, "attempt not found")
	}
	sum := sha256.Sum256(at.Context)
	if at.ContextDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		return qa.AttemptFacts{}, qa.Unavailablef(nil, "the pinned context does not match its digest")
	}
	pin, err := DecodePinned(at.Context)
	if err != nil {
		return qa.AttemptFacts{}, qa.Unavailablef(err, "the pinned context is unreadable")
	}
	pinned := pin.Context
	if pinned.Scope() != scope || pinned.AttemptID != id || pinned.RunID != run || pinned.StepID != at.StepID {
		return qa.AttemptFacts{}, qa.Failf(qa.ErrNotFound, "attempt not found")
	}
	f := qa.AttemptFacts{Scope: scope, Run: run, Step: at.StepID, Attempt: id, Context: pinned, ContextVersion: pin.Version, Sources: pin.Sources}
	if at.Status == execution.AttemptSucceeded && at.Result != nil && at.Result.Outcome == runtimev1.OutcomeCompleted {
		f.Completed = true
		f.Outputs = map[string]string{}
		for name, v := range at.Result.Outputs {
			if v.IsString() {
				f.Outputs[name] = v.String()
			}
		}
		f.Diagnostics = append(f.Diagnostics, at.Result.DiagnosticArtifactIDs...)
	}
	return f, nil
}

// Pinned is the version-independent reading of an attempt's frozen context: the
// v1-shaped members the mapping reads, the context version and, under version 3,
// the pinned source set.
type Pinned struct {
	Context runtimev1.Context
	Version int64
	Sources qa.SourceSet
}

// DecodePinned reads a frozen context by its version, each version with its own
// strict decoder. Version 1 (no version member) is exactly the original reading.
// Version 3 projects the members QA reads (identity, work item, environment,
// inputs, outputs) onto the v1 shape and its workspace sources onto the QA
// source set; nothing else of the v3 context (Git delivery, origin, preparation)
// reaches the mapping. A version this adapter does not know is an error: a
// context of another shape is never read as v1.
func DecodePinned(raw []byte) (Pinned, error) {
	var head struct {
		ContextVersion *int64 `json:"contextVersion"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Pinned{}, err
	}
	switch {
	case head.ContextVersion == nil:
		c, err := runtimev1.DecodeRequest[runtimev1.Context](raw)
		if err != nil {
			return Pinned{}, err
		}
		return Pinned{Context: c, Version: 1}, nil
	case *head.ContextVersion == runtimev3.ContextVersion:
		c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](raw)
		if err != nil {
			return Pinned{}, err
		}
		p := Pinned{Version: 3, Context: runtimev1.Context{
			TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
			Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
			Workspace: runtimev1.Workspace{Access: c.Workspace.Access}, Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets,
			Environment: c.Environment, Limits: c.Limits,
		}}
		if c.Workspace.SourceSetDigest != nil {
			p.Sources.Digest = *c.Workspace.SourceSetDigest
			for _, s := range c.Workspace.Sources {
				p.Sources.Sources = append(p.Sources.Sources, qa.Source{
					RepositoryID: s.RepositoryID, Path: s.Path, Commit: runtimev1.GitCommit(s.Commit), HeadCommit: s.HeadCommit, Access: string(s.Access),
				})
			}
		}
		if p.Sources.Digest != "" {
			for _, l := range c.Workspace.Lineage {
				for _, out := range l.Published {
					p.Sources.Published = append(p.Sources.Published, qa.Published{RepositoryID: out.RepositoryID, Commit: out.Commit})
				}
			}
		}
		return p, nil
	}
	return Pinned{}, fmt.Errorf("unsupported context version %d", *head.ContextVersion)
}

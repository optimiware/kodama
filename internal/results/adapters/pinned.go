package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// ErrPinnedContext: the persisted context is of neither runtime wire.
var ErrPinnedContext = errors.New("the pinned context is unreadable")

// DecodePinned reads the immutable pinned context of an attempt on its own wire:
// the unchanged strict v1 decoder first, then the v2 one (a run_shared attempt).
// It returns the v1 view every consumer of the runtime v1 contract reads and, for
// a run_shared attempt, the v2 context itself (nil otherwise).
//
// The v1 view of a v2 context carries the fields the two wires share with their
// v1 meaning: identity, definition, command, inputs, declared outputs, secret
// bindings, environment, limits and the workspace access with its source. The
// source commit of a v2 attempt is the head commit of its context (the latest
// commit published in the run, equal to the run's pinned commit before any
// writer), because that is the commit the attempt's workspace started from; the
// run's pinned input commit and the attempt's recorded begin state are provenance
// the v2 context and execution carry, read through V2Provenance.
func DecodePinned(raw []byte) (runtimev1.Context, *runtimev2.ContextV2, error) {
	if c, err := runtimev1.DecodeRequest[runtimev1.Context](raw); err == nil {
		return c, nil, nil
	}
	c2, err := runtimev2.DecodeRequest[runtimev2.ContextV2](raw)
	if err != nil {
		return runtimev1.Context{}, nil, ErrPinnedContext
	}
	return projectV1(c2), &c2, nil
}

func projectV1(c runtimev2.ContextV2) runtimev1.Context {
	v := runtimev1.Context{
		TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
		Workspace: runtimev1.Workspace{Access: c.Workspace.Access},
		Inputs:    c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets, Environment: c.Environment, Limits: c.Limits,
	}
	if src := c.Workspace.Source; src != nil {
		v.Workspace.Source = &runtimev1.Source{
			RepositoryURL: src.RepositoryURL, Commit: src.HeadCommit, Branch: src.Branch, ExpectedTip: src.ExpectedTip,
		}
	}
	return v
}

// ErrBeginNotRecorded: a run_shared attempt acts before the server recorded that
// its command began, so there is no workspace state to attribute its output to.
var ErrBeginNotRecorded = errors.New("the attempt's command begin is not recorded")

// Begins reads the command-begin record the server made for an attempt.
// execution's record is the authority; the entrypoint's report only produced it.
type Begins interface {
	Begin(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (execution.BeginRecord, bool, error)
}

// ExecutionBegins implements Begins over execution's store.
type ExecutionBegins struct{ runs Runs }

var _ Begins = ExecutionBegins{}

// NewExecutionBegins requires the store.
func NewExecutionBegins(runs Runs) (ExecutionBegins, error) {
	if runs == nil {
		return ExecutionBegins{}, errors.New("results/adapters: an execution store is required")
	}
	return ExecutionBegins{runs: runs}, nil
}

// Begin implements Begins.
func (b ExecutionBegins) Begin(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (execution.BeginRecord, bool, error) {
	run, err := b.runs.RunOf(ctx, scope, attempt)
	if err != nil {
		return execution.BeginRecord{}, false, err
	}
	agg, err := b.runs.Get(ctx, scope, run)
	if err != nil {
		return execution.BeginRecord{}, false, err
	}
	if rec := BeginOf(agg, attempt); rec != nil {
		return *rec, true, nil
	}
	return execution.BeginRecord{}, false, nil
}

// BeginOf is the aggregate's begin record of the attempt, or nil.
func BeginOf(agg *execution.Aggregate, attempt domain.AttemptID) *execution.BeginRecord {
	if agg == nil {
		return nil
	}
	for _, b := range agg.Begins {
		if b.AttemptID == attempt {
			return b
		}
	}
	return nil
}

// V2Provenance is the trusted provenance of what a run_shared attempt produces:
// the repository and the head commit its context pinned, plus what the server
// recorded when its command began (workspace state clean|modified, ignored files
// present) and a reference to the lineage its context carried: the digest of the
// canonical lineage list. A source-free attempt has none. The begin record is
// execution's, never the entrypoint's claim; without it there is no provenance
// (ErrBeginNotRecorded).
func V2Provenance(c runtimev2.ContextV2, begin *execution.BeginRecord) (artifacts.Provenance, error) {
	src := c.Workspace.Source
	if src == nil {
		return artifacts.Provenance{}, nil
	}
	if begin == nil || begin.AttemptID != c.AttemptID {
		return artifacts.Provenance{}, ErrBeginNotRecorded
	}
	state := string(begin.WorkspaceState)
	if state != artifacts.WorkspaceClean && state != artifacts.WorkspaceModified {
		return artifacts.Provenance{}, fmt.Errorf("the recorded workspace state %q is not one of a source-backed attempt", state)
	}
	ref, err := LineageRef(c.Workspace.Lineage)
	if err != nil {
		return artifacts.Provenance{}, err
	}
	p := artifacts.Provenance{
		RepositoryURL: src.RepositoryURL, Commit: string(src.HeadCommit), WorkspaceState: state, IgnoredPresent: begin.IgnoredPresent, Lineage: ref,
	}
	return p, p.Validate()
}

// LineageRef is the digest of the canonical encoding of a lineage list.
func LineageRef(l []runtimev2.LineageEntry) (string, error) {
	if l == nil {
		l = []runtimev2.LineageEntry{}
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

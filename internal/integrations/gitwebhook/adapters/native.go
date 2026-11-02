package adapters

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gitpush"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/repository"
	"github.com/optimiware/kodama/internal/work"
)

// This file adapts the native trigger's ports (gitwebhook.PushRecords,
// SourceRuns, PushRepositories, WorkItems) onto the merged modules. Nothing here
// is wired: T-132 composes them in internal/app.

// ---- run admission ----

// SourceAdmitter is the part of *execution.Service the native trigger uses.
type SourceAdmitter interface {
	AdmitSourceRun(ctx context.Context, caller execution.Caller, cmd execution.SourceRunCommand) (*execution.Run, error)
}

// RunReader is the part of execution.Store that reads a run by identity.
type RunReader interface {
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// NativeRuns implements gitwebhook.SourceRuns over execution's source-set
// admission. A push origin is admitted without a work item and without a
// repository (its implicit source is the pushed repository, pinned from the
// applied push record); a work item origin pins the item's content revision.
// The run ID is the caller's stable identity, so a repeat is execution's own
// replay.
type NativeRuns struct {
	admit SourceAdmitter
	runs  RunReader
}

var _ gw.SourceRuns = (*NativeRuns)(nil)

// NewNativeRuns requires both collaborators.
func NewNativeRuns(admit SourceAdmitter, runs RunReader) (*NativeRuns, error) {
	if admit == nil || runs == nil {
		return nil, errors.New("gitwebhook/adapters: run admission and a run reader are required")
	}
	return &NativeRuns{admit: admit, runs: runs}, nil
}

// Admit implements gitwebhook.SourceRuns.
func (n *NativeRuns) Admit(ctx context.Context, req gw.SourceRunRequest) (gw.RunRef, error) {
	cmd := execution.SourceRunCommand{Scope: req.Scope, DefinitionID: req.DefinitionID, RunID: req.RunID, ContentRevision: req.ContentRevision}
	switch {
	case req.Origin.PushID != "":
		if req.Origin.WorkItemID != "" || req.ContentRevision != 0 || req.ExpectedCommit == "" {
			return gw.RunRef{}, fmt.Errorf("%w: a push run has no work item or content revision and names its commit", gw.ErrInvalid)
		}
		cmd.Origin = execution.Origin{Kind: execution.OriginPush, PushID: req.Origin.PushID, RepositoryID: req.Origin.RepositoryID, Ref: req.Origin.Ref}
	case req.Origin.WorkItemID != "":
		cmd.Origin = execution.Origin{Kind: execution.OriginWorkItem, WorkItemID: req.Origin.WorkItemID}
	default:
		return gw.RunRef{}, fmt.Errorf("%w: a run needs a push or a work item origin", gw.ErrInvalid)
	}
	run, err := n.admit.AdmitSourceRun(ctx, execution.Caller{PrincipalID: req.Principal, RequestID: req.RequestID}, cmd)
	if err != nil {
		classified := ClassifyAdmission(err)
		if errors.Is(classified, gw.ErrRunRefused) {
			return gw.RunRef{}, &refusal{code: refusalCode(err), err: classified}
		}
		return gw.RunRef{}, classified
	}
	if run == nil || run.ID != req.RunID || run.Scope != req.Scope {
		return gw.RunRef{}, fmt.Errorf("%w: admission answered another run", gw.ErrUnavailable)
	}
	if req.Origin.PushID != "" {
		if err := checkPinned(run, req); err != nil {
			return gw.RunRef{}, err
		}
	}
	return gw.RunRef{ID: run.ID}, nil
}

// refusal is a permanent admission refusal with a stable reason code. The code
// is all the trigger records and shows (gitwebhook.RefusalCode): the wrapped
// error keeps its content-free cause for diagnostics only.
type refusal struct {
	code string
	err  error
}

func (r *refusal) Error() string       { return r.err.Error() }
func (r *refusal) Unwrap() error       { return r.err }
func (r *refusal) RefusalCode() string { return r.code }

var codePrefix = regexp.MustCompile(`^([a-z][a-z0-9_]{0,63}):`)

// refusalCode maps an execution refusal to a stable code: the class of the
// sentinel, or for an admission refusal the stable code execution puts first in
// its message ("source_set_push_repository: ..."). Never a message, name or
// identifier.
func refusalCode(err error) string {
	switch {
	case errors.Is(err, execution.ErrDenied), errors.Is(err, execution.ErrHidden):
		return "run_authority_denied"
	case errors.Is(err, execution.ErrNotActionable):
		return "work_item_not_actionable"
	case errors.Is(err, execution.ErrWorkCancelled):
		return "work_item_cancelled"
	case errors.Is(err, execution.ErrPurposeDenied):
		return "run_purpose_denied"
	case errors.Is(err, execution.ErrAdmissionRefused):
		if m := codePrefix.FindStringSubmatch(strings.TrimPrefix(err.Error(), execution.ErrAdmissionRefused.Error()+": ")); m != nil {
			return m[1]
		}
		return gw.ReasonAdmissionRefused
	case errors.Is(err, execution.ErrInvalid):
		return "admission_invalid"
	case errors.Is(err, execution.ErrNotFound):
		return "admission_target_not_found"
	}
	return gw.ReasonAdmissionRefused
}

// checkPinned proves the push run's source is the pushed commit: the entry of
// the pushed repository pins exactly the record's `new` value. A different pin
// is never recorded as a start; it is an outage-class failure that stays
// visible.
func checkPinned(run *execution.Run, req gw.SourceRunRequest) error {
	if run.Sources == nil {
		return fmt.Errorf("%w: the push run pinned no source set", gw.ErrUnavailable)
	}
	for _, e := range run.Sources.Entries {
		if e.RepositoryID == req.Origin.RepositoryID {
			if e.Commit != req.ExpectedCommit {
				return fmt.Errorf("%w: the push run pinned another commit than the pushed one", gw.ErrUnavailable)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: the pushed repository is not a source of the run", gw.ErrUnavailable)
}

// Find implements gitwebhook.SourceRuns.
func (n *NativeRuns) Find(ctx context.Context, scope domain.Scope, id domain.RunID) (bool, error) {
	if err := scope.Validate(); err != nil {
		return false, fmt.Errorf("%w: %v", gw.ErrInvalid, err)
	}
	if err := id.Validate(); err != nil {
		return false, fmt.Errorf("%w: %v", gw.ErrInvalid, err)
	}
	agg, err := n.runs.Get(ctx, scope, id)
	switch {
	case errors.Is(err, execution.ErrNotFound):
		return false, nil
	case err != nil:
		return false, outage("run lookup", err)
	}
	return agg != nil && agg.Run != nil, nil
}

// ---- push records ----

// PushReloader is the part of *gitpush.Service the native trigger uses.
type PushReloader interface {
	ReloadByReceive(ctx context.Context, scope domain.Scope, repositoryID, receiveID string) (gitv1.PushRecord, error)
}

// PushReader implements gitwebhook.PushRecords over gitpush.Service.ReloadByReceive
// (scope comparison only: the trigger already holds the scope's authority). The
// contract's not_found is gitwebhook.ErrNotFound; anything else is an outage.
type PushReader struct{ pushes PushReloader }

var _ gw.PushRecords = (*PushReader)(nil)

// NewPushReader requires the reload.
func NewPushReader(p PushReloader) (*PushReader, error) {
	if p == nil {
		return nil, errors.New("gitwebhook/adapters: a push record reload is required")
	}
	return &PushReader{pushes: p}, nil
}

// Reload implements gitwebhook.PushRecords.
func (p *PushReader) Reload(ctx context.Context, scope domain.Scope, repositoryID, receiveID string) (gitv1.PushRecord, error) {
	rec, err := p.pushes.ReloadByReceive(ctx, scope, repositoryID, receiveID)
	switch {
	case err == nil:
		return rec, nil
	case gitpush.IsCode(err, domain.CodeNotFound):
		return gitv1.PushRecord{}, gw.ErrNotFound
	}
	return gitv1.PushRecord{}, outage("push record", err)
}

// ---- repositories ----

// RepositoryGetter is the part of repository.Store the native configuration
// uses: the scope's repository by identity, without an actor (the caller has
// already authorized the principal).
type RepositoryGetter interface {
	Get(ctx context.Context, scope domain.Scope, id repository.ID) (*repository.Repository, error)
}

// NativeRepositories implements gitwebhook.PushRepositories.
type NativeRepositories struct{ repos RepositoryGetter }

var _ gw.PushRepositories = (*NativeRepositories)(nil)

// NewNativeRepositories requires the getter.
func NewNativeRepositories(r RepositoryGetter) (*NativeRepositories, error) {
	if r == nil {
		return nil, errors.New("gitwebhook/adapters: a repository reader is required")
	}
	return &NativeRepositories{repos: r}, nil
}

// Repository implements gitwebhook.PushRepositories.
func (n *NativeRepositories) Repository(ctx context.Context, scope domain.Scope, id string) (gw.RepositoryFacts, error) {
	r, err := n.repos.Get(ctx, scope, repository.ID(id))
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return gw.RepositoryFacts{}, gw.ErrNotFound
	case err != nil:
		return gw.RepositoryFacts{}, outage("repository lookup", err)
	case r == nil || r.Scope != scope:
		return gw.RepositoryFacts{}, gw.ErrNotFound
	}
	return gw.RepositoryFacts{ID: string(r.ID), Hosting: r.Hosting, Status: r.Status}, nil
}

// ---- work items ----

// ItemReader is the part of work.Repository the task router uses.
type ItemReader interface {
	Get(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (work.Item, error)
}

// WorkReader implements gitwebhook.WorkItems over the work module's repository.
type WorkReader struct{ items ItemReader }

var _ gw.WorkItems = (*WorkReader)(nil)

// NewWorkReader requires the reader.
func NewWorkReader(i ItemReader) (*WorkReader, error) {
	if i == nil {
		return nil, errors.New("gitwebhook/adapters: a work item reader is required")
	}
	return &WorkReader{items: i}, nil
}

// Facts implements gitwebhook.WorkItems.
func (w *WorkReader) Facts(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (gw.WorkFacts, error) {
	it, err := w.items.Get(ctx, scope, id)
	switch {
	case errors.Is(err, work.ErrNotFound):
		return gw.WorkFacts{}, gw.ErrNotFound
	case err != nil:
		return gw.WorkFacts{}, outage("work item", err)
	case it.Scope != scope:
		return gw.WorkFacts{}, gw.ErrNotFound
	}
	return gw.WorkFacts{
		State: string(it.State), ContentRevision: it.ContentRevision, Actionable: it.Kind.Actionable(),
		Cancelled: it.State == work.StateCancelled, BlockedReason: it.BlockedReason,
	}, nil
}

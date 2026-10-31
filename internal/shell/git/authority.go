package git

import (
	"context"
	"errors"
	"fmt"
	"slices"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Code names why integration authority was refused.
type Code string

// Refusal codes. Each condition of the decision has its own code.
const (
	CodeUnknown            Code = "unknown"              // the run, repository or pipeline is not in the scope
	CodeRunInactive        Code = "run_inactive"         // the run is settled or cancelling
	CodeWrongPipeline      Code = "wrong_pipeline"       // not the repository's configured integration pipeline
	CodeStepIneligible     Code = "step_ineligible"      // not a protected integration step that is running now
	CodeWaitMissing        Code = "wait_missing"         // a required wait does not exist in the run
	CodeNotApproved        Code = "not_approved"         // a required wait is not resolved with its approving outcome
	CodeCandidateMismatch  Code = "candidate_mismatch"   // a wait's candidate correlation is not the pushed commit
	CodeRefUnprotected     Code = "ref_unprotected"      // not the protected default branch
	CodeMalformed          Code = "malformed"            // not an update of an existing ref
	CodeExpectedOldChanged Code = "expected_old_changed" // the expected old commit is not the ref's current commit
	CodeNotFastForward     Code = "not_fast_forward"     // the new candidate does not descend from the old commit
	CodeOutsideAuthority   Code = "outside_authority"    // the update is not the one the authority names
)

// Refusal is the error of a refused decision. It never carries a commit, an
// answer or any other value from the run beyond the names of its own records.
type Refusal struct {
	Code   Code
	Detail string
}

func (r *Refusal) Error() string {
	if r.Detail == "" {
		return "git integration authority refused: " + string(r.Code)
	}
	return fmt.Sprintf("git integration authority refused: %s: %s", r.Code, r.Detail)
}

// PushReason maps the refusal to the reason the worker's pre-receive verdict
// carries (docs/contracts/git-service.md).
func (r *Refusal) PushReason() string {
	switch r.Code {
	case CodeNotFastForward:
		return gitv1.ReasonNotFastForward
	case CodeRefUnprotected, CodeMalformed:
		return gitv1.ReasonRefProtected
	case CodeRunInactive:
		return gitv1.ReasonRunInactive
	}
	return gitv1.ReasonIntegrationMismatch
}

func refuse(code Code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a refusal and returns its code.
func IsRefusal(err error) (Code, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code, true
	}
	return "", false
}

// Policy is the repository's integration policy as read now.
type Policy struct {
	// DefaultBranch is the short name of the protected default branch.
	DefaultBranch string
	// IntegrationPipeline is the name of the repository's configured
	// integration pipeline; empty means none.
	IntegrationPipeline string
}

// Observation is what the worker reports of the ref and the candidate. A
// missing or unknown fast-forward fact is false.
type Observation struct {
	// Current is the commit the ref has now.
	Current string
	// FastForward is whether the candidate descends from Current.
	FastForward bool
}

// Request asks for authority to integrate one candidate in one step of one run.
type Request struct {
	Scope      domain.Scope
	Repository string
	Run        domain.RunID
	Step       domain.StepID
	// Update is the expected old commit and the new candidate on the ref.
	Update gitv1.RefUpdate
}

// Authority is the scoped receipt of a granted decision. It permits exactly the
// one update it names.
type Authority struct {
	Scope      domain.Scope
	Repository string
	Run        domain.RunID
	Step       domain.StepID
	Ref        string
	Old        string
	New        string
}

// Permits reports whether u is exactly the update the authority names.
func (a Authority) Permits(u gitv1.RefUpdate) bool {
	return a.Ref != "" && a.Old != "" && a.New != "" && u.Ref == a.Ref && u.Old == a.Old && u.New == a.New
}

// Facts are everything one decision reads, all of it read now.
type Facts struct {
	Policy Policy
	// PipelineName is the name of the pipeline the run executes.
	PipelineName string
	// Record is the run's durable record.
	Record runtime.RunRecord
	// Observed is the worker's view of the ref and the candidate.
	Observed Observation
}

// Decide is the authority decision. It is pure: it reads only req and facts.
func Decide(f Facts, req Request) (Authority, error) {
	rec := f.Record
	if rec.Run.Scope != req.Scope || rec.Run.ID != req.Run {
		return Authority{}, refuse(CodeUnknown, "run %s is not in the scope", req.Run)
	}
	if rec.Run.Settled() || rec.Run.CancelRequested {
		return Authority{}, refuse(CodeRunInactive, "run %s no longer executes", req.Run)
	}
	if f.Policy.IntegrationPipeline == "" || f.PipelineName != f.Policy.IntegrationPipeline {
		return Authority{}, refuse(CodeWrongPipeline, "the run's pipeline is not the repository's integration pipeline")
	}
	plan, err := eligibleStep(rec, req.Step)
	if err != nil {
		return Authority{}, err
	}
	for _, need := range plan.Requires {
		if err := waitSatisfied(rec, need, req.Update.New); err != nil {
			return Authority{}, err
		}
	}
	// The kernel's own check of the same facts must agree.
	if err := rec.RequirementsMet(req.Step); err != nil {
		return Authority{}, refuse(CodeNotApproved, "step %s requirements are not met", req.Step)
	}
	if f.Policy.DefaultBranch == "" || req.Update.Ref != gitv1.HeadsPrefix+f.Policy.DefaultBranch {
		return Authority{}, refuse(CodeRefUnprotected, "%s is not the protected default branch", req.Update.Ref)
	}
	if err := req.Update.Validate(); err != nil || gitv1.IsZeroObjectID(req.Update.Old) || gitv1.IsZeroObjectID(req.Update.New) {
		return Authority{}, refuse(CodeMalformed, "integration updates an existing ref to another commit")
	}
	if f.Observed.Current != req.Update.Old {
		return Authority{}, refuse(CodeExpectedOldChanged, "the ref is no longer at the expected old commit")
	}
	if !f.Observed.FastForward {
		return Authority{}, refuse(CodeNotFastForward, "the candidate does not descend from the ref")
	}
	return Authority{Scope: req.Scope, Repository: req.Repository, Run: req.Run, Step: req.Step,
		Ref: req.Update.Ref, Old: req.Update.Old, New: req.Update.New}, nil
}

// eligibleStep returns the plan step when it is the protected integration step
// of the run and is running now.
func eligibleStep(rec runtime.RunRecord, key domain.StepID) (runtime.PlanStep, error) {
	i := slices.IndexFunc(rec.Plan.Steps, func(p runtime.PlanStep) bool { return p.Key == key })
	if i < 0 {
		return runtime.PlanStep{}, refuse(CodeStepIneligible, "the plan has no step %s", key)
	}
	p := rec.Plan.Steps[i]
	if p.Kind != runtime.StepContainer || p.Authority != runtime.AuthorityIntegrate || len(p.Requires) == 0 {
		return runtime.PlanStep{}, refuse(CodeStepIneligible, "step %s is not an integration step", key)
	}
	j := slices.IndexFunc(rec.Run.Steps, func(s runtime.Step) bool { return s.Key == key })
	if j < 0 || rec.Run.Steps[j].State != runtime.StepRunning {
		return runtime.PlanStep{}, refuse(CodeStepIneligible, "step %s is not running", key)
	}
	return p, nil
}

// waitSatisfied checks one required wait: it exists, is decided with the wait's
// approving outcome by a resolution, and was opened for the candidate.
func waitSatisfied(rec runtime.RunRecord, need runtime.Requirement, candidate string) error {
	pi := slices.IndexFunc(rec.Plan.Steps, func(p runtime.PlanStep) bool { return p.Key == need.Wait && p.Kind == runtime.StepWait })
	wi := slices.IndexFunc(rec.Waits, func(w runtime.Wait) bool { return w.StepKey == need.Wait })
	si := slices.IndexFunc(rec.Run.Steps, func(s runtime.Step) bool { return s.Key == need.Wait })
	if pi < 0 || wi < 0 || si < 0 || rec.Plan.Steps[pi].Wait == nil {
		return refuse(CodeWaitMissing, "wait %s does not exist", need.Wait)
	}
	approving, ok := rec.Plan.Steps[pi].Wait.ApprovingOutcome()
	w := rec.Waits[wi]
	if !ok || need.Outcome != approving || rec.Run.Steps[si].State != runtime.StepSucceeded ||
		w.State != runtime.WaitResolved || w.Decision.Kind != runtime.DecisionResolved || w.Decision.Outcome != approving {
		return refuse(CodeNotApproved, "wait %s is not resolved with its approving outcome", need.Wait)
	}
	if got, ok := w.Correlation["candidate"]; !ok || got == "" || got != candidate {
		return refuse(CodeCandidateMismatch, "wait %s was decided for another candidate", need.Wait)
	}
	return nil
}

// Runs reads a run's durable record (runtime.StateStore satisfies it). A run
// outside the scope is reported as domain.CodeNotFound.
type Runs interface {
	Load(ctx context.Context, scope domain.Scope, id domain.RunID) (runtime.RunRecord, error)
}

// Repositories reads a repository's integration policy now.
type Repositories interface {
	IntegrationPolicy(ctx context.Context, scope domain.Scope, repositoryID string) (Policy, error)
}

// Pipelines names the pipeline a run executes, given the run and the pipeline
// identity its record carries.
type Pipelines interface {
	PipelineName(ctx context.Context, scope domain.Scope, run domain.RunID, id domain.PipelineID) (string, error)
}

// Refs asks the worker for the ref's current commit and whether the candidate
// fast-forwards it.
type Refs interface {
	Observe(ctx context.Context, scope domain.Scope, repositoryID, ref, candidate string) (Observation, error)
}

// Authorizer issues and rechecks integration authority over its ports. It holds
// no state: every call reads the current facts.
type Authorizer struct {
	Runs         Runs
	Repositories Repositories
	Pipelines    Pipelines
	Refs         Refs
}

// Issue decides the request against the current facts.
func (a Authorizer) Issue(ctx context.Context, req Request) (Authority, error) {
	if err := req.Scope.Validate(); err != nil {
		return Authority{}, err
	}
	f, err := a.facts(ctx, req.Scope, req.Repository, req.Run)
	if err != nil {
		return Authority{}, err
	}
	obs, err := a.Refs.Observe(ctx, req.Scope, req.Repository, req.Update.Ref, req.Update.New)
	if err != nil {
		return Authority{}, fmt.Errorf("git: observing %s: %w", req.Update.Ref, err)
	}
	f.Observed = obs
	return Decide(f, req)
}

// Recheck is the use-time check of an authority for the update u the worker
// reports at receive, with its Git fact. The decision is made again from fresh
// facts; the earlier grant is only the update it was issued for. The worker's
// own current value of the ref is u.Old, so the expected-old check compares the
// authority's old commit with what the worker is about to replace.
func (a Authorizer) Recheck(ctx context.Context, auth Authority, u gitv1.RefUpdate, fact gitv1.GitFact) error {
	if !auth.Permits(u) {
		return refuse(CodeOutsideAuthority, "the update is not the one the authority names")
	}
	if fact.Ref != u.Ref {
		return refuse(CodeMalformed, "the Git fact is for another ref")
	}
	f, err := a.facts(ctx, auth.Scope, auth.Repository, auth.Run)
	if err != nil {
		return err
	}
	f.Observed = Observation{Current: u.Old, FastForward: fact.FastForward != nil && *fact.FastForward}
	_, err = Decide(f, Request{Scope: auth.Scope, Repository: auth.Repository, Run: auth.Run, Step: auth.Step, Update: u})
	return err
}

func (a Authorizer) facts(ctx context.Context, scope domain.Scope, repositoryID string, run domain.RunID) (Facts, error) {
	rec, err := a.Runs.Load(ctx, scope, run)
	if err != nil {
		return Facts{}, readErr("run", err)
	}
	policy, err := a.Repositories.IntegrationPolicy(ctx, scope, repositoryID)
	if err != nil {
		return Facts{}, readErr("repository", err)
	}
	name, err := a.Pipelines.PipelineName(ctx, scope, run, rec.Run.PipelineID)
	if err != nil {
		return Facts{}, readErr("pipeline", err)
	}
	return Facts{Policy: policy, PipelineName: name, Record: rec}, nil
}

// readErr turns a record that is not in the scope into a refusal; any other
// failure is returned as an error, never as authority.
func readErr(what string, err error) error {
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeNotFound {
		return refuse(CodeUnknown, "%s is not in the scope", what)
	}
	return fmt.Errorf("git: reading the %s: %w", what, err)
}

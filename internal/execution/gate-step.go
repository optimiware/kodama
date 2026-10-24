package execution

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Gate steps wait in durable records: a ready gate step stays pending with its
// GateRef set, has zero attempts and holds no compute reservation or Job. The
// gate module (T-034) owns the gate record, its subject and its decision; this
// file is the execution side of the seam. It resolves the step-local material
// the subject is built from (pinned inputs and successful ancestor outputs
// only), binds a gate record to its step, completes the step after an
// applicable approval, fails it after a rejection, and reports which gates
// protect a later step. Every transition is an explicit, audited step
// transition; none creates an attempt.

// Reasons recorded on gate-step transitions.
const (
	// ReasonGateRejected: the reviewer rejected the gate; the run fails fast.
	ReasonGateRejected Reason = "gate_rejected"
	// ReasonGateApproved: an applicable approval completed the gate step.
	ReasonGateApproved Reason = "gate_approved"
)

// GateEvidenceInput is one evidence expression of a gate step resolved to the
// artifact identifier it names. The identifier comes only from a pinned run
// input or a successful ancestor's finalized output.
type GateEvidenceInput struct {
	// Expression is the whole-scalar reference as written in the definition.
	Expression string
	ArtifactID domain.ArtifactID
	// Producer is the ancestor step whose output the reference names, or ""
	// for a pinned run input.
	Producer string
}

// GateCandidate is the source commit under review and the step that produced
// it.
type GateCandidate struct {
	Commit string
	// Producer is the successful writer ancestor whose published commit is the
	// candidate, or "" when the candidate is the run's original source commit.
	Producer     string
	ProducerKind pipeline.StepKind
}

// GateInputs is everything execution contributes to a gate subject. It is a
// pure function of the locked or loaded aggregate: the pinned run, the frozen
// step and the finalized outputs of ancestors.
type GateInputs struct {
	Scope           domain.Scope
	RunID           domain.RunID
	StepID          string
	WorkItemID      domain.WorkItemID
	ContentRevision domain.ContentRevision
	DefinitionID    domain.DefinitionID
	Candidate       GateCandidate
	Evidence        []GateEvidenceInput
	// DeploymentID is the resolved gate.deploymentId reference, or "".
	DeploymentID string
}

// ProtectingGate is a gate step that protects a later step: the later step is
// one of its transitive successors.
type ProtectingGate struct {
	StepID  string
	Ordinal int
	Status  StepStatus
	GateRef string
	// Producer is the last writer ancestor of the gate itself ("" when none):
	// the producer of the commit the gate was asked to approve.
	Producer string
}

// GateProtection lists the gates in front of a step and the last writer among
// its own ancestors.
type GateProtection struct {
	StepID string
	Gates  []ProtectingGate
	// LastWriter is the last writer ancestor of the protected step ("" when the
	// step reads only the run's original source).
	LastWriter string
}

// ancestors returns the transitive needs of a step.
func (a *Aggregate) ancestors(stepID string) map[string]bool {
	out := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		s := a.Step(id)
		if s == nil {
			return
		}
		for _, n := range s.Spec.Needs {
			if !out[n] {
				out[n] = true
				walk(n)
			}
		}
	}
	walk(stepID)
	return out
}

// lastWriter selects the last writer among the given ancestors. Writers are
// totally ordered in a valid definition: every pair is ancestor-related, so
// the last one is the writer that is a descendant of all the others. An
// unordered pair is ambiguous and rejected.
func (a *Aggregate) lastWriter(anc map[string]bool) (*StepRun, error) {
	var writers []*StepRun
	for id := range anc {
		if s := a.Step(id); s != nil && s.Spec.IsWriter() {
			writers = append(writers, s)
		}
	}
	sort.Slice(writers, func(i, j int) bool { return writers[i].Ordinal < writers[j].Ordinal })
	for i := 1; i < len(writers); i++ {
		if !a.ancestors(writers[i].ID)[writers[i-1].ID] {
			return nil, invalidf("the source writers %s and %s are not ordered", writers[i-1].ID, writers[i].ID)
		}
	}
	if len(writers) == 0 {
		return nil, nil
	}
	return writers[len(writers)-1], nil
}

func (a *Aggregate) gateStep(stepID string) (*StepRun, error) {
	s := a.Step(stepID)
	if s == nil {
		return nil, ErrNotFound
	}
	if s.Spec.Kind != pipeline.KindGate {
		return nil, invalidf("step %s is not a gate", stepID)
	}
	return s, nil
}

// GateReady reports whether a gate step is waiting for its gate: pending, every
// dependency succeeded, and the run still accepts progression. A gate is never
// executable and never has an attempt.
func (a *Aggregate) GateReady(s *StepRun) bool {
	return s.Spec.Kind == pipeline.KindGate && s.Status == StepPending && a.Run.AcceptsDispatch() && a.dependenciesSucceeded(s)
}

// ReadyGates lists the ready gate steps in definition order.
func (a *Aggregate) ReadyGates() []*StepRun {
	var out []*StepRun
	for _, s := range a.Steps {
		if a.GateReady(s) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

// GateInputs resolves the subject material of a gate step from the aggregate:
// the run's pinned work item, content revision and definition, the candidate
// commit (the last successful writer ancestor's published commit, otherwise the
// run's original commit) and the evidence and deployment references. A
// reference to anything other than a pinned input or a successful ancestor's
// output, an unfinished producer, or an ambiguous source binding is an error.
func (a *Aggregate) GateInputs(stepID string) (GateInputs, error) {
	s, err := a.gateStep(stepID)
	if err != nil {
		return GateInputs{}, err
	}
	run := a.Run
	if run.Purpose != PurposeWork || run.SourceCommit == "" {
		return GateInputs{}, invalidf("a gate needs a work run with a pinned source commit")
	}
	anc := a.ancestors(stepID)
	for id := range anc {
		if p := a.Step(id); p == nil || p.Status != StepSucceeded {
			return GateInputs{}, fmt.Errorf("%w: ancestor %s has not succeeded", ErrStale, id)
		}
	}
	in := GateInputs{
		Scope: run.Scope, RunID: run.ID, StepID: stepID, WorkItemID: run.WorkItemID,
		ContentRevision: run.ContentRevision, DefinitionID: run.DefinitionID,
		Candidate: GateCandidate{Commit: run.SourceCommit},
	}
	w, err := a.lastWriter(anc)
	if err != nil {
		return GateInputs{}, err
	}
	if w != nil {
		if w.Status != StepSucceeded || w.OutputCommit == "" {
			return GateInputs{}, fmt.Errorf("%w: the source writer %s has no published commit", ErrStale, w.ID)
		}
		in.Candidate = GateCandidate{Commit: w.OutputCommit, Producer: w.ID, ProducerKind: w.Spec.Kind}
	}
	inputs := run.Snapshot.Values()
	if s.Spec.Gate != nil {
		for _, expr := range s.Spec.Gate.Evidence {
			id, producer, err := a.gateRef(expr, inputs, anc)
			if err != nil {
				return GateInputs{}, err
			}
			in.Evidence = append(in.Evidence, GateEvidenceInput{Expression: expr, ArtifactID: domain.ArtifactID(id), Producer: producer})
		}
		if s.Spec.Gate.DeploymentID != "" {
			id, _, err := a.gateRef(s.Spec.Gate.DeploymentID, inputs, anc)
			if err != nil {
				return GateInputs{}, err
			}
			in.DeploymentID = id
		}
	}
	return in, nil
}

// gateRef resolves one whole-scalar reference of a gate. A step reference must
// name a transitive ancestor: the definition validator guarantees it, and the
// resolution does not depend on that.
func (a *Aggregate) gateRef(expr string, inputs map[string]runtimev1.Value, anc map[string]bool) (value, producer string, err error) {
	v, err := a.resolveRef(expr, inputs)
	if err != nil {
		return "", "", err
	}
	body := strings.TrimSpace(expr)
	if strings.HasPrefix(body, "${{") && strings.HasSuffix(body, "}}") {
		parts := strings.Split(strings.TrimSpace(body[3:len(body)-2]), ".")
		if len(parts) == 4 && parts[0] == "steps" {
			if !anc[parts[1]] {
				return "", "", invalidf("step %q is not an ancestor of the gate", parts[1])
			}
			producer = parts[1]
		}
	}
	if v == "" {
		return "", "", invalidf("a gate reference resolved to nothing")
	}
	return v, producer, nil
}

// Protection reports the gates in front of a step: every gate step among its
// transitive ancestors, and the last writer ancestor of the step itself.
func (a *Aggregate) Protection(stepID string) (GateProtection, error) {
	if a.Step(stepID) == nil {
		return GateProtection{}, ErrNotFound
	}
	anc := a.ancestors(stepID)
	out := GateProtection{StepID: stepID}
	for id := range anc {
		g := a.Step(id)
		if g == nil || g.Spec.Kind != pipeline.KindGate {
			continue
		}
		pw, err := a.lastWriter(a.ancestors(id))
		if err != nil {
			return GateProtection{}, err
		}
		pg := ProtectingGate{StepID: g.ID, Ordinal: g.Ordinal, Status: g.Status, GateRef: g.GateRef}
		if pw != nil {
			pg.Producer = pw.ID
		}
		out.Gates = append(out.Gates, pg)
	}
	sort.Slice(out.Gates, func(i, j int) bool { return out.Gates[i].Ordinal < out.Gates[j].Ordinal })
	w, err := a.lastWriter(anc)
	if err != nil {
		return GateProtection{}, err
	}
	if w != nil {
		out.LastWriter = w.ID
	}
	return out, nil
}

// Covered reports whether at least one protecting gate approved the commit
// produced by the protected step's last writer ancestor: a write after an
// approval requires a new gate before the steps behind it.
func (p GateProtection) Covered() bool {
	if len(p.Gates) == 0 {
		return true
	}
	for _, g := range p.Gates {
		if g.Producer == p.LastWriter {
			return true
		}
	}
	return false
}

// BindGate attaches the gate record to its ready step. The step stays pending
// with zero attempts. Binding the same reference again is a no-op; another
// reference is refused, so a step has exactly one gate.
func (a *Aggregate) BindGate(stepID, ref string, now time.Time) (bool, error) {
	s, err := a.gateStep(stepID)
	if err != nil {
		return false, err
	}
	if ref == "" || len(ref) > 128 {
		return false, invalidf("a gate reference is required")
	}
	if s.GateRef != "" {
		if s.GateRef == ref {
			return false, nil
		}
		return false, transitionf("step %s is bound to another gate", stepID)
	}
	if !a.GateReady(s) {
		return false, fmt.Errorf("%w: gate step %s is not ready", ErrStale, stepID)
	}
	s.GateRef = ref
	s.touch(now)
	return true, nil
}

// CompleteGate performs the explicit pending-to-succeeded transition of a gate
// step after an applicable approval. No attempt is created and nothing is
// reserved. A gate step that is already terminal is not reopened.
func (a *Aggregate) CompleteGate(stepID string, now time.Time) error {
	s, err := a.gateStep(stepID)
	if err != nil {
		return err
	}
	if s.Status != StepPending {
		return transitionf("gate step %s is %s, not pending", stepID, s.Status)
	}
	if s.GateRef == "" || s.AttemptsStarted != 0 {
		return transitionf("gate step %s has no gate or an attempt", stepID)
	}
	if !a.Run.AcceptsDispatch() || !a.dependenciesSucceeded(s) {
		return fmt.Errorf("%w: gate step %s cannot progress", ErrStale, stepID)
	}
	from := s.Status
	s.Status, s.Reason, s.NextAttemptAt = StepSucceeded, ReasonGateApproved, nil
	t := now.UTC()
	s.FinishedAt = &t
	s.touch(now)
	a.recordStep(s, from, now)
	return nil
}

// RejectGate fails a pending gate step after a rejection and records the run's
// failure intent. Rework is a new run.
func (a *Aggregate) RejectGate(stepID string, now time.Time) error {
	s, err := a.gateStep(stepID)
	if err != nil {
		return err
	}
	if s.Status != StepPending {
		return transitionf("gate step %s is %s, not pending", stepID, s.Status)
	}
	return a.FailPending(s, ReasonGateRejected, now)
}

// GateCheck is the gate module's decision about one gate step, evaluated inside
// the transaction that holds the run row: it returns nil only for an applicable
// approval. Any error leaves the step pending and rolls the mutation back.
type GateCheck func(ctx context.Context, env Env, agg *Aggregate, step *StepRun) error

// BindGateMutation attaches a gate record to a ready gate step. verify runs
// under the run lock first, so a gate built from stale inputs is never bound.
// Bind runs the peer effect (inserting the gate record in the same
// transaction) through after.
func BindGateMutation(stepID, ref string, verify func(agg *Aggregate) error, after func(ctx context.Context, env Env) error) Mutation {
	return func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		if agg.Run.Status.Terminal() {
			return Effects{}, fmt.Errorf("%w: the run is %s", ErrStale, agg.Run.Status)
		}
		if verify != nil {
			if err := verify(agg); err != nil {
				return Effects{}, err
			}
		}
		if _, err := agg.BindGate(stepID, ref, env.Now()); err != nil {
			return Effects{}, err
		}
		var hooks []func(context.Context, Env) error
		if after != nil {
			hooks = append(hooks, after)
		}
		return Effects{After: hooks}, nil
	}
}

// CompleteGateMutation completes a gate step once check accepts its approval,
// then advances the run (skip propagation and completion) exactly like a
// dispatch transaction. A cancelled work item cancels the run instead.
func CompleteGateMutation(stepID string, check GateCheck) Mutation {
	return func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		now := env.Now()
		step := agg.Step(stepID)
		if step == nil {
			return Effects{}, ErrNotFound
		}
		if step.Status != StepPending {
			// A terminal gate step is never reopened or rewritten.
			return Effects{Result: GateStepOutcome{Status: step.Status}}, nil
		}
		if agg.Run.Purpose == PurposeWork {
			if _, err := env.Work(); err != nil {
				if errors.Is(err, ErrWorkCancelled) {
					agg.RequestCancel(ReasonWorkCancelled, now)
					after, aerr := advance(env, agg)
					return Effects{After: after, Result: GateStepOutcome{Status: step.Status, Cancelled: true}}, aerr
				}
				return Effects{}, err
			}
		}
		if check == nil {
			return Effects{}, fmt.Errorf("%w: no gate check", ErrNotConfigured)
		}
		if err := check(ctx, env, agg, step); err != nil {
			return Effects{}, err
		}
		if err := agg.CompleteGate(stepID, now); err != nil {
			return Effects{}, err
		}
		after, err := advance(env, agg)
		return Effects{After: after, Result: GateStepOutcome{Status: StepSucceeded, Completed: true}}, err
	}
}

// RejectGateMutation fails a pending gate step after a rejection.
func RejectGateMutation(stepID string) Mutation {
	return func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		step := agg.Step(stepID)
		if step == nil {
			return Effects{}, ErrNotFound
		}
		if step.Status != StepPending {
			return Effects{Result: GateStepOutcome{Status: step.Status}}, nil
		}
		if err := agg.RejectGate(stepID, env.Now()); err != nil {
			return Effects{}, err
		}
		after, err := advance(env, agg)
		return Effects{After: after, Result: GateStepOutcome{Status: StepFailed, Rejected: true}}, err
	}
}

// GateStepOutcome is the Result of a gate-step mutation.
type GateStepOutcome struct {
	Status    StepStatus
	Completed bool
	Rejected  bool
	Cancelled bool
}

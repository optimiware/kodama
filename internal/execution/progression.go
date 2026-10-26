package execution

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Hooks reports which step kinds this installation executes. The M1 wiring
// enables containers only: an agent or gate step is never claimed as a workload, and
// a gate step allocates no compute reservation. A nil Hooks enables nothing.
type Hooks interface {
	Enabled(kind pipeline.StepKind) bool
}

// EnabledKinds is a static Hooks selected deliberately by wiring.
type EnabledKinds []pipeline.StepKind

// Enabled implements Hooks.
func (e EnabledKinds) Enabled(k pipeline.StepKind) bool {
	for _, x := range e {
		if x == k {
			return true
		}
	}
	return false
}

// conditionHolds evaluates a step's `when` against the pinned inputs.
func conditionHolds(c *pipeline.Condition, inputs map[string]runtimev1.Value) bool {
	v, ok := inputs[c.Input]
	if !ok {
		return false
	}
	switch c.Equals.Type {
	case pipeline.TypeBoolean:
		return v.IsBool() && v == runtimev1.BoolValue(c.Equals.Bool)
	case pipeline.TypeInteger:
		return v.IsInteger() && v == runtimev1.IntValue(c.Equals.Int)
	default:
		return v.IsString() && v.String() == c.Equals.Str
	}
}

// Propagate applies skip rules to pending steps until nothing changes: a false
// condition skips an executable step, and a skipped ancestor skips its
// dependents. A gate is never skipped through an author-supplied condition (the
// definition forbids one, and the rule holds even if one were stored), so no
// path bypasses a mandatory gate: its successors become runnable only after it
// succeeded, and are skipped, not run, if the gate itself was skipped.
func (a *Aggregate) Propagate(now time.Time) bool {
	inputs := a.Run.Snapshot.Values()
	changed := false
	for again := true; again; {
		again = false
		for _, s := range a.Steps {
			if s.Status != StepPending {
				continue
			}
			reason := Reason("")
			for _, need := range s.Spec.Needs {
				if n := a.Step(need); n != nil && n.Status == StepSkipped {
					reason = ReasonSkippedAncestor
					break
				}
			}
			if reason == "" && s.Spec.When != nil && s.Spec.Kind != pipeline.KindGate && !conditionHolds(s.Spec.When, inputs) {
				reason = ReasonCondition
			}
			if reason == "" {
				continue
			}
			from := s.Status
			if err := s.skip(reason, now); err != nil {
				continue
			}
			a.recordStep(s, from, now)
			changed, again = true, true
		}
	}
	return changed
}

// dependenciesSucceeded reports that every needed step succeeded.
func (a *Aggregate) dependenciesSucceeded(s *StepRun) bool {
	for _, need := range s.Spec.Needs {
		n := a.Step(need)
		if n == nil || n.Status != StepSucceeded {
			return false
		}
	}
	return true
}

// Runnable reports whether a pending step may be claimed now: its dependencies
// succeeded, its backoff elapsed, it has no unsettled attempt, the run accepts
// dispatch, and its kind is enabled. Gate steps are never runnable here: they
// wait in durable records and consume no workload.
func (a *Aggregate) Runnable(s *StepRun, now time.Time, hooks Hooks) bool {
	if !a.Run.AcceptsDispatch() || s.Status != StepPending || !s.Due(now) {
		return false
	}
	if !s.Spec.Executable() || hooks == nil || !hooks.Enabled(s.Spec.Kind) {
		return false
	}
	if !a.dependenciesSucceeded(s) {
		return false
	}
	for _, at := range a.AttemptsOf(s.ID) {
		if !a.control(at.ID).Settled() {
			return false
		}
	}
	return true
}

// NextRunnable returns the first runnable step in definition order.
func (a *Aggregate) NextRunnable(now time.Time, hooks Hooks) *StepRun {
	steps := append([]*StepRun(nil), a.Steps...)
	sort.Slice(steps, func(i, j int) bool { return steps[i].Ordinal < steps[j].Ordinal })
	for _, s := range steps {
		if a.Runnable(s, now, hooks) {
			return s
		}
	}
	return nil
}

// WorkspaceBinding is the source of one step: the original run commit or an
// explicit successful ancestor writer's commit. The current branch tip is never
// substituted.
type WorkspaceBinding struct {
	Access Access
	// Commit is the commit to check out (empty when Access is none).
	Commit string
	// FromWriter names the ancestor writer step when the source is its output.
	FromWriter string
}

// SourceFor selects the step's source. A writer ancestor that did not succeed
// with a recorded output commit makes the step unrunnable.
func (a *Aggregate) SourceFor(s *StepRun) (WorkspaceBinding, error) {
	ws := s.Spec.Workspace
	if ws == nil || ws.Access == "" || ws.Access == pipeline.AccessNone {
		return WorkspaceBinding{Access: AccessNone}, nil
	}
	b := WorkspaceBinding{Access: Access(ws.Access)}
	if ws.From == pipeline.RunSource {
		if a.Run.SourceCommit == "" {
			return WorkspaceBinding{}, fmt.Errorf("%w: the run has no source commit", ErrInvalid)
		}
		b.Commit = a.Run.SourceCommit
		return b, nil
	}
	w := a.Step(ws.From)
	if w == nil || !w.Spec.IsWriter() {
		return WorkspaceBinding{}, fmt.Errorf("%w: the source step is not a writer", ErrInvalid)
	}
	if w.Status != StepSucceeded || w.OutputCommit == "" {
		return WorkspaceBinding{}, fmt.Errorf("%w: the source writer has no successful output", ErrStale)
	}
	b.Commit, b.FromWriter = w.OutputCommit, w.ID
	return b, nil
}

// FailPending fails a pending step that can never run (for example an
// unresolvable reference) and records the run's failure intent.
func (a *Aggregate) FailPending(s *StepRun, reason Reason, now time.Time) error {
	from := s.Status
	if err := s.fail(reason, now); err != nil {
		return err
	}
	a.recordStep(s, from, now)
	a.RecordFailure(reason, "", now)
	return nil
}

// ResolvedStep is a step's command with every whole-scalar reference replaced
// by its pinned input or finalized producer output. Secret entries are not
// values here: they are pinned bindings delivered through the secrets
// interface.
type ResolvedStep struct {
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	Outputs []runtimev1.OutputDecl
}

func scalarString(v runtimev1.Value) string {
	switch {
	case v.IsBool():
		b, _ := v.MarshalJSON()
		return string(b)
	case v.IsInteger():
		b, _ := v.MarshalJSON()
		return string(b)
	}
	return v.String()
}

// resolveRef resolves one string: a literal, or a whole-scalar reference.
func (a *Aggregate) resolveRef(raw string, inputs map[string]runtimev1.Value) (string, error) {
	if !strings.Contains(raw, "${{") {
		return raw, nil
	}
	if a.Run.Shared != nil {
		// kodama/v1alpha2 endpoint references resolve against the pinned endpoints
		// recorded at admission; nothing resolves later and the entrypoint never
		// evaluates expressions.
		if v, ok, err := resolveEndpointRef(strings.TrimSpace(raw), a.Run.Shared.Endpoints); ok {
			if err != nil {
				return "", err
			}
			return v, nil
		}
	}
	body := strings.TrimSpace(raw)
	if !strings.HasPrefix(body, "${{") || !strings.HasSuffix(body, "}}") {
		return "", fmt.Errorf("%w: only whole-scalar references are supported", ErrInvalid)
	}
	expr := strings.TrimSpace(body[3 : len(body)-2])
	switch parts := strings.Split(expr, "."); {
	case len(parts) == 2 && parts[0] == "inputs":
		v, ok := inputs[parts[1]]
		if !ok {
			return "", fmt.Errorf("%w: input %q has no pinned value", ErrInvalid, parts[1])
		}
		return scalarString(v), nil
	case len(parts) == 4 && parts[0] == "steps" && parts[2] == "outputs":
		p := a.Step(parts[1])
		if p == nil || p.Status != StepSucceeded {
			return "", fmt.Errorf("%w: step %q has no finalized outputs", ErrInvalid, parts[1])
		}
		if parts[3] == pipeline.ImplicitCommitOutput && p.OutputCommit != "" {
			return p.OutputCommit, nil
		}
		v, ok := p.Outputs[parts[3]]
		if !ok {
			return "", fmt.Errorf("%w: step %q has no output %q", ErrInvalid, parts[1], parts[3])
		}
		return scalarString(v), nil
	}
	return "", fmt.Errorf("%w: unsupported reference", ErrInvalid)
}

// Resolve prepares the command of a step for its attempt.
func (a *Aggregate) Resolve(s *StepRun) (ResolvedStep, error) {
	inputs := a.Run.Snapshot.Values()
	out := ResolvedStep{Image: s.Spec.Image, Command: append([]string{}, s.Spec.Command...), Env: map[string]string{}}
	for _, arg := range s.Spec.Args {
		v, err := a.resolveRef(arg, inputs)
		if err != nil {
			return ResolvedStep{}, err
		}
		out.Args = append(out.Args, v)
	}
	if out.Args == nil {
		out.Args = []string{}
	}
	for name, e := range s.Spec.Env {
		if e.Scalar == nil {
			continue // a secret binding, delivered separately
		}
		var raw string
		switch e.Scalar.Type {
		case pipeline.TypeBoolean:
			raw = strconv.FormatBool(e.Scalar.Bool)
		case pipeline.TypeInteger:
			raw = strconv.FormatInt(e.Scalar.Int, 10)
		default:
			raw = e.Scalar.Str
		}
		v, err := a.resolveRef(raw, inputs)
		if err != nil {
			return ResolvedStep{}, err
		}
		out.Env[name] = v
	}
	names := make([]string, 0, len(s.Spec.Outputs))
	for n := range s.Spec.Outputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		o := s.Spec.Outputs[n]
		d := runtimev1.OutputDecl{Name: n, Type: runtimev1.OutputType(o.Type)}
		if o.Type == pipeline.TypeArtifact {
			p := o.Path
			d.Path = &p
		}
		out.Outputs = append(out.Outputs, d)
	}
	if out.Outputs == nil {
		out.Outputs = []runtimev1.OutputDecl{}
	}
	return out, nil
}

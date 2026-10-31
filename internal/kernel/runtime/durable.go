package runtime

import (
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// ---------------------------------------------------------------------------
// Step outputs
// ---------------------------------------------------------------------------

// MaxOutputValueBytes bounds one recorded output value. Together with
// MaxStepOutputs it bounds everything a step can add to the record.
const MaxOutputValueBytes = 4096

// StepOutputs is the durable record of the declared outputs a container step
// produced. It is committed in the same Transition that settles the step as
// succeeded and never changes afterwards, so it is visible exactly when the step
// is and a restarted or replacement engine reads it from the record without any
// port of its own. A step that failed, was skipped or was cancelled never has any.
type StepOutputs struct {
	StepKey domain.StepID `json:"stepKey"`
	// Attempt is the workload attempt that produced the values.
	Attempt int `json:"attempt"`
	// Values holds at least one declared output name with its value.
	Values map[string]string `json:"values"`
}

// Validate checks the record on its own: a step, an attempt, one to
// MaxStepOutputs well-formed names and bounded, valid UTF-8 values. Whether the
// names are declared by the plan is checked against it (CheckOutputs).
func (o StepOutputs) Validate() error {
	if err := o.StepKey.Validate(); err != nil {
		return err
	}
	if o.Attempt < 1 {
		return invalid("outputs of step %s name no attempt", o.StepKey)
	}
	if len(o.Values) == 0 || len(o.Values) > MaxStepOutputs {
		return invalid("outputs of step %s hold 1 to %d values", o.StepKey, MaxStepOutputs)
	}
	for name, v := range o.Values {
		if !outputNameRE.MatchString(name) {
			return invalid("step %s output name %q", o.StepKey, name)
		}
		if len(v) > MaxOutputValueBytes || !utf8.ValidString(v) {
			return invalid("step %s output %q is not valid text of at most %d bytes", o.StepKey, name, MaxOutputValueBytes)
		}
	}
	return nil
}

// Output returns the recorded value of one declared output of a step. A step
// that did not succeed, or that did not produce the output, has none.
func (r RunRecord) Output(step domain.StepID, name string) (string, bool) {
	for _, o := range r.Outputs {
		if o.StepKey == step {
			v, ok := o.Values[name]
			return v, ok
		}
	}
	return "", false
}

func planStepOf(p Plan, key domain.StepID) (PlanStep, bool) {
	i := slices.IndexFunc(p.Steps, func(s PlanStep) bool { return s.Key == key })
	if i < 0 {
		return PlanStep{}, false
	}
	return p.Steps[i], true
}

func stepOf(r Run, key domain.StepID) (Step, bool) {
	i := slices.IndexFunc(r.Steps, func(s Step) bool { return s.Key == key })
	if i < 0 {
		return Step{}, false
	}
	return r.Steps[i], true
}

// checkOutputsOf checks one record against the run and plan it belongs to: a
// declared set of names on a succeeded container step at the attempt that
// settled it.
func checkOutputsOf(plan Plan, run Run, o StepOutputs) error {
	if err := o.Validate(); err != nil {
		return err
	}
	s, ok := stepOf(run, o.StepKey)
	ps, _ := planStepOf(plan, o.StepKey)
	if !ok || s.Kind != StepContainer || s.State != StepSucceeded || s.Attempt != o.Attempt {
		return fmt.Errorf("%w: outputs belong to the succeeded attempt of a container step: %s", ErrIllegalTransition, o.StepKey)
	}
	for name := range o.Values {
		if !slices.Contains(ps.Outputs, name) {
			return invalid("step %s does not declare output %q", o.StepKey, name)
		}
	}
	return nil
}

// CheckOutputs applies the outputs a Transition adds to the recorded ones and
// returns the set to persist, or the refusal. next is the run the transition
// stores and prev the stored run it replaces. An output must be declared by the
// plan, bounded, and recorded in the very commit that settles its step as
// succeeded: outputs added to a step that was already settled, or to one that did
// not succeed, are refused, so a recorded value never appears later and a
// condition never reads one value and then another. A StateStore applies it to
// every Commit.
func CheckOutputs(plan Plan, prev, next Run, recorded, add []StepOutputs) ([]StepOutputs, error) {
	out := slices.Clone(recorded)
	for i, o := range add {
		if err := checkOutputsOf(plan, next, o); err != nil {
			return nil, err
		}
		if slices.ContainsFunc(add[:i], func(x StepOutputs) bool { return x.StepKey == o.StepKey }) ||
			slices.ContainsFunc(recorded, func(x StepOutputs) bool { return x.StepKey == o.StepKey }) {
			return nil, fmt.Errorf("%w: step %s already has recorded outputs", ErrIllegalTransition, o.StepKey)
		}
		if p, ok := stepOf(prev, o.StepKey); !ok || p.State == StepSucceeded {
			return nil, fmt.Errorf("%w: step %s settled before these outputs; they are recorded with its settlement", ErrIllegalTransition, o.StepKey)
		}
		out = append(out, o)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Secret materializations
// ---------------------------------------------------------------------------

// MaterializationState is where a materialization is in its life.
type MaterializationState string

// Materialization states.
const (
	// MaterializationIssued: the identity was recorded before the resolver was
	// asked to create material under it. Material may exist.
	MaterializationIssued MaterializationState = "issued"
	// MaterializationRevoked: the resolver confirmed it removed the material and
	// revoked access. Final.
	MaterializationRevoked MaterializationState = "revoked"
)

// MaterializationRef is the durable, scoped identity of the private secret
// material of one step attempt. It is a pure function of (scope, run, step,
// attempt), so any engine can name it from the record, and it carries no
// directory and no value: a resolver revokes by this identity alone.
type MaterializationRef struct {
	Scope   domain.Scope  `json:"scope"`
	RunID   domain.RunID  `json:"runId"`
	StepKey domain.StepID `json:"stepKey"`
	Attempt int           `json:"attempt"`
	// ID is the identity derived from the other fields.
	ID string `json:"id"`
}

// NewMaterializationRef returns the identity of the material of one attempt.
func NewMaterializationRef(scope domain.Scope, run domain.RunID, step domain.StepID, attempt int) (MaterializationRef, error) {
	r := MaterializationRef{Scope: scope, RunID: run, StepKey: step, Attempt: attempt}
	r.ID = r.derive()
	return r, r.Validate()
}

func (r MaterializationRef) derive() string {
	return fmt.Sprintf("mat:%s:%s:%s:%s:%d", r.Scope.TenantID, r.Scope.ProjectID, r.RunID, r.StepKey, r.Attempt)
}

// Validate checks the identity: a valid scope, run and step, an attempt, and an
// ID that is exactly the derived one, so a record cannot name material it was not
// issued for.
func (r MaterializationRef) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if err := r.RunID.Validate(); err != nil {
		return err
	}
	if err := r.StepKey.Validate(); err != nil {
		return err
	}
	if r.Attempt < 1 {
		return invalid("a materialization names a step attempt")
	}
	if r.ID != r.derive() {
		return invalid("materialization id %q is not the identity of its scope, run, step and attempt", r.ID)
	}
	return nil
}

// MaterializationRecord is the durable record of one materialization: issued
// before the resolver is called, revoked once the resolver confirmed. It is what
// lets a replacement engine, after a takeover or crash, revoke material it never
// saw, and know when nothing is left to revoke.
type MaterializationRecord struct {
	Ref   MaterializationRef   `json:"ref"`
	State MaterializationState `json:"state"`
	// IssuedAt is when the identity was recorded; it never changes.
	IssuedAt time.Time `json:"issuedAt"`
}

// Validate checks the record on its own.
func (m MaterializationRecord) Validate() error {
	if err := m.Ref.Validate(); err != nil {
		return err
	}
	if m.State != MaterializationIssued && m.State != MaterializationRevoked {
		return invalid("materialization state %q", m.State)
	}
	if m.IssuedAt.IsZero() {
		return invalid("a materialization records when it was issued")
	}
	return nil
}

// checkMaterializationsOf checks records against the run they belong to.
func checkMaterializationsOf(run Run, ms []MaterializationRecord) error {
	for i, m := range ms {
		if err := m.Validate(); err != nil {
			return err
		}
		if m.Ref.Scope != run.Scope || m.Ref.RunID != run.ID {
			return invalid("materialization %s belongs to another run", m.Ref.ID)
		}
		s, ok := stepOf(run, m.Ref.StepKey)
		if !ok || s.Kind != StepContainer || m.Ref.Attempt > s.Attempt {
			return invalid("materialization %s does not name a launched attempt of a container step", m.Ref.ID)
		}
		if slices.ContainsFunc(ms[:i], func(x MaterializationRecord) bool { return x.Ref.ID == m.Ref.ID }) {
			return invalid("materialization %s is recorded twice", m.Ref.ID)
		}
	}
	return nil
}

// CheckMaterializations applies the materialization records a Transition carries
// to the recorded ones and returns the set to persist, or the refusal. next is
// the run the transition stores. A new record is issued, for the attempt that is
// running in next; an existing one only moves from issued to revoked, keeping its identity
// and time, and a repeat of the stored record changes nothing. Revoked is final.
// A StateStore applies it to every Commit.
func CheckMaterializations(next Run, recorded, add []MaterializationRecord) ([]MaterializationRecord, error) {
	if err := checkMaterializationsOf(next, add); err != nil {
		return nil, err
	}
	out := slices.Clone(recorded)
	for _, m := range add {
		i := slices.IndexFunc(out, func(x MaterializationRecord) bool { return x.Ref.ID == m.Ref.ID })
		if i < 0 {
			s, _ := stepOf(next, m.Ref.StepKey)
			if m.State != MaterializationIssued || s.State != StepRunning || s.Attempt != m.Ref.Attempt {
				return nil, fmt.Errorf("%w: material is issued for the running attempt of a step, as issued: %s", ErrIllegalTransition, m.Ref.ID)
			}
			out = append(out, m)
			continue
		}
		prev := out[i]
		switch {
		case prev.Ref != m.Ref || !prev.IssuedAt.Equal(m.IssuedAt):
			return nil, invalid("materialization %s cannot change its identity", prev.Ref.ID)
		case prev.State == m.State:
		case prev.State == MaterializationIssued && m.State == MaterializationRevoked:
			out[i] = m
		default:
			return nil, fmt.Errorf("%w: materialization %s is %s", ErrIllegalTransition, prev.Ref.ID, prev.State)
		}
	}
	return out, nil
}

// PendingRevocations returns the materializations that still need revoking:
// issued ones whose step is terminal, or whose attempt was superseded by a retry.
// An issued materialization of the attempt that is still running is live and is
// not listed. It reads recorded state only.
func (r RunRecord) PendingRevocations() []MaterializationRecord {
	var out []MaterializationRecord
	for _, m := range r.Materializations {
		if m.State != MaterializationIssued {
			continue
		}
		s, ok := stepOf(r.Run, m.Ref.StepKey)
		if ok && (s.State.Terminal() || m.Ref.Attempt < s.Attempt) {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Claim release
// ---------------------------------------------------------------------------

// ReleaseClaim is the atomic release a StateStore applies to the stored
// (current) claim when its holder gives the run up without ending it, as an
// engine does when it parks a run on an open wait. It reports whether it
// released. The held claim must be valid; a claim of another run is refused.
// Release is a no-op, not an error, for a run whose claim is not the holder's any
// more: never claimed, held by another owner or at another fence after a
// takeover, or already expired or released. Otherwise the lease ends at now and
// everything else is kept, in particular the fence, so the next claim is a normal
// takeover that increments it and a late write from the releasing holder is
// refused as an expired lease. Releasing never changes the run.
func ReleaseClaim(current, held Claim, now time.Time) (Claim, bool, error) {
	if err := held.Validate(); err != nil {
		return Claim{}, false, err
	}
	if now.IsZero() {
		return Claim{}, false, invalid("a release needs the time")
	}
	if current != (Claim{}) && (held.Scope != current.Scope || held.RunID != current.RunID) {
		return Claim{}, false, invalid("claim belongs to another run")
	}
	if current == (Claim{}) || held.Owner != current.Owner || held.Fence != current.Fence || !current.Active(now) {
		return current, false, nil
	}
	current.Expires = now
	return current, true, nil
}

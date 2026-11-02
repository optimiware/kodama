package postgres

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// same reports whether two records encode identically. Records round-trip
// through encoding/json by contract, so the encoding is their identity.
func same(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// applyTransition applies t to the stored record and returns the record to
// persist. It is pure: it reads stored, mutates a copy and returns an error
// without any effect, so the caller's transaction commits all of it or none.
//
// The order is fixed: the writer's fence and lease first, so a superseded
// writer is always told it was superseded; then the run version; then the
// legality of each change against the stored value.
func applyTransition(stored runtime.RunRecord, t runtime.Transition) (runtime.RunRecord, error) {
	if stored.Claim == (runtime.Claim{}) {
		return runtime.RunRecord{}, fmt.Errorf("%w: run %s was never claimed", runtime.ErrStaleFence, t.RunID)
	}
	if err := t.Claim.Authorizes(stored.Claim, t.Now); err != nil {
		return runtime.RunRecord{}, err
	}
	if stored.Run.Version != t.ExpectedVersion {
		return runtime.RunRecord{}, versionConflict("run %s is at version %d, the transition read %d",
			t.RunID, stored.Run.Version, t.ExpectedVersion)
	}
	if err := runtime.CheckRunUpdate(stored.Run, t.Run); err != nil {
		return runtime.RunRecord{}, err
	}
	// A run that was asked to stop never starts or parks another step, even for a
	// writer that read the cancellation.
	if stored.Run.CancelRequested {
		for i, s := range t.Run.Steps {
			if prev := stored.Run.Steps[i]; s.State != prev.State && (s.State == runtime.StepRunning || s.State == runtime.StepWaiting) {
				return runtime.RunRecord{}, fmt.Errorf("%w: run %s is cancelling", runtime.ErrExecutionClosed, t.RunID)
			}
		}
	}

	next := stored
	next.Run = t.Run.Clone()
	next.Waits = slices.Clone(stored.Waits)
	next.Commands = slices.Clone(stored.Commands)
	next.Workloads = slices.Clone(stored.Workloads)
	next.Resources = slices.Clone(stored.Resources)
	next.Outputs = slices.Clone(stored.Outputs)
	next.Materializations = slices.Clone(stored.Materializations)

	for _, w := range t.Waits {
		i := slices.IndexFunc(next.Waits, func(x runtime.Wait) bool { return x.ID == w.ID })
		if i < 0 {
			return runtime.RunRecord{}, invalid("wait %s is not a wait of run %s", w.ID, t.RunID)
		}
		prev := next.Waits[i]
		if err := checkWaitUpdate(prev, w); err != nil {
			return runtime.RunRecord{}, err
		}
		// A wait opens only as its step starts waiting, in the same transaction. A
		// wait open before its step waits would be addressable by a signal while
		// execution has not reached it. The reverse is not required: a decision
		// may precede the engine's consumption of it in the step.
		if prev.State == runtime.WaitPending && w.State == runtime.WaitOpen {
			si := slices.IndexFunc(next.Run.Steps, func(x runtime.Step) bool { return x.Key == w.StepKey })
			if si < 0 || next.Run.Steps[si].State != runtime.StepWaiting || stored.Run.Steps[si].State == runtime.StepWaiting {
				return runtime.RunRecord{}, fmt.Errorf("%w: wait %s opens only with its step entering waiting", runtime.ErrIllegalTransition, w.ID)
			}
		}
		next.Waits[i] = w
	}
	for _, c := range t.Commands {
		if i := slices.IndexFunc(next.Commands, func(x runtime.Command) bool { return x.ID == c.ID }); i >= 0 {
			if err := checkCommandUpdate(next.Commands[i], c); err != nil {
				return runtime.RunRecord{}, err
			}
			next.Commands[i] = c
			continue
		}
		if c.Fence != t.Claim.Fence {
			return runtime.RunRecord{}, fmt.Errorf("%w: command %s carries fence %d, the writer holds %d",
				runtime.ErrStaleFence, c.ID, c.Fence, t.Claim.Fence)
		}
		next.Commands = append(next.Commands, c)
	}
	for _, o := range t.Workloads {
		b := o.Binding
		i := slices.IndexFunc(next.Workloads, func(x runtime.WorkloadObservation) bool {
			return x.Binding.StepKey == b.StepKey && x.Binding.Attempt == b.Attempt
		})
		if i < 0 {
			next.Workloads = append(next.Workloads, o)
			continue
		}
		if err := runtime.CheckObservationUpdate(next.Workloads[i], o); err != nil {
			return runtime.RunRecord{}, err
		}
		next.Workloads[i] = o
	}
	for _, o := range t.Resources {
		id := resourceIdentity(o.Handle)
		i := slices.IndexFunc(next.Resources, func(x runtime.ResourceObservation) bool { return resourceIdentity(x.Handle) == id })
		if i < 0 {
			next.Resources = append(next.Resources, o)
			continue
		}
		prev := next.Resources[i]
		if o.Handle.Fence < prev.Handle.Fence {
			return runtime.RunRecord{}, fmt.Errorf("%w: resource %s fence %d, retained %d",
				runtime.ErrStaleFence, o.Handle.Name, o.Handle.Fence, prev.Handle.Fence)
		}
		if o.ObservedAt.Before(prev.ObservedAt) {
			return runtime.RunRecord{}, invalid("resource %s observation is older than the retained one", o.Handle.Name)
		}
		next.Resources[i] = o
	}

	outputs, err := runtime.CheckOutputs(stored.Plan, stored.Run, t.Run, next.Outputs, t.Outputs)
	if err != nil {
		return runtime.RunRecord{}, err
	}
	next.Outputs = outputs
	mats, err := runtime.CheckMaterializations(t.Run, next.Materializations, t.Materializations)
	if err != nil {
		return runtime.RunRecord{}, err
	}
	next.Materializations = mats

	// A wait step that moves must agree with its wait as it will be stored.
	for i, s := range next.Run.Steps {
		if s.Kind != runtime.StepWait || s.State == stored.Run.Steps[i].State {
			continue
		}
		wi := slices.IndexFunc(next.Waits, func(w runtime.Wait) bool { return w.StepKey == s.Key })
		if wi < 0 {
			return runtime.RunRecord{}, invalid("wait step %s has no wait", s.Key)
		}
		if err := runtime.CheckStepWait(s, next.Waits[wi]); err != nil {
			return runtime.RunRecord{}, err
		}
	}

	if t.RenewTTL > 0 {
		c, err := runtime.RenewClaim(stored.Claim, t.Claim, t.Now, t.RenewTTL)
		if err != nil {
			return runtime.RunRecord{}, err
		}
		next.Claim = c
	}
	return next, nil
}

// resourceIdentity is a resource's stable identity: its handle without the
// writer's fence, which changes across takeovers.
func resourceIdentity(h runtime.ResourceHandle) runtime.ResourceHandle {
	h.Fence = 0
	return h
}

// checkWaitUpdate reports whether next may replace prev. A wait's identity,
// plan-declared spec, correlation, request and deadline never change once it
// opened; a decided wait never changes at all; a change advances the version by
// exactly one. Every refusal leaves the stored wait as it was.
func checkWaitUpdate(prev, next runtime.Wait) error {
	if prev.Scope != next.Scope || prev.ID != next.ID || prev.RunID != next.RunID || prev.StepKey != next.StepKey ||
		!prev.Spec.Equal(next.Spec) {
		return invalid("wait %s cannot change its identity or declared shape", prev.ID)
	}
	switch next.Version {
	case prev.Version:
		if !same(prev, next) {
			return versionConflict("wait %s changed without a new version", prev.ID)
		}
		return nil
	case prev.Version + 1:
	default:
		return versionConflict("wait %s is at version %d, the transition carries %d", prev.ID, prev.Version, next.Version)
	}
	if prev.State.Closed() {
		return fmt.Errorf("%w: wait %s is %s", runtime.ErrWaitClosed, prev.ID, prev.State)
	}
	legal := false
	switch prev.State {
	case runtime.WaitPending:
		legal = next.State == runtime.WaitOpen || next.State == runtime.WaitCancelled || next.State == runtime.WaitPending
	case runtime.WaitOpen:
		legal = next.State.Closed() || next.State == runtime.WaitOpen
	}
	if !legal {
		return fmt.Errorf("%w: wait %s: %s -> %s", runtime.ErrIllegalTransition, prev.ID, prev.State, next.State)
	}
	if prev.State == runtime.WaitOpen && (!prev.OpenedAt.Equal(next.OpenedAt) || !prev.Deadline.Equal(next.Deadline) ||
		prev.Request != next.Request || !maps.Equal(prev.Correlation, next.Correlation)) {
		return fmt.Errorf("%w: wait %s changed its correlation, request or deadline after opening", runtime.ErrIllegalTransition, prev.ID)
	}
	if next.State == prev.State && next.Assignment == prev.Assignment {
		return fmt.Errorf("%w: wait %s changed its version and nothing else", runtime.ErrIllegalTransition, prev.ID)
	}
	return nil
}

// checkCommandUpdate reports whether next may replace prev. The command's
// identity, target, specification, fence and binding never change; its state
// follows Command.Settle (intent to any outcome, uncertain to confirmed or
// rejected, confirmed and rejected final); a confirmed workload command may
// retain its result once. A repeat of the stored command changes nothing.
func checkCommandUpdate(prev, next runtime.Command) error {
	want := prev
	if next.State != prev.State {
		if err := want.Settle(next.State, next.Reason); err != nil {
			return err
		}
	}
	if next.Result != prev.Result {
		if prev.Result != (runtime.Result{}) {
			return fmt.Errorf("%w: command %s keeps its recorded result", runtime.ErrIllegalTransition, prev.ID)
		}
		if err := want.RecordResult(next.Result); err != nil {
			return err
		}
	}
	if !same(want, next) {
		return fmt.Errorf("%w: command %s may change only its state, reason and result", runtime.ErrIllegalTransition, prev.ID)
	}
	return nil
}

// micro rounds a time down to the microsecond PostgreSQL stores, in UTC. The
// adapter normalizes every time it receives to it, so what a writer holds
// compares equal to what the database returns, and a deadline computed from an
// open time stays that open time plus the timeout (see the wait handling in
// normalize).
func micro(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }

// normalize returns t with every time at database precision. It shares nothing
// mutable with t.
func normalize(t runtime.Transition) runtime.Transition {
	t.Now = micro(t.Now)
	t.Run = t.Run.Clone()
	for i := range t.Run.Steps {
		t.Run.Steps[i].Predicate.At = micro(t.Run.Steps[i].Predicate.At)
	}
	t.Waits = slices.Clone(t.Waits)
	for i := range t.Waits {
		w := &t.Waits[i]
		w.Decision.At = micro(w.Decision.At)
		if !w.OpenedAt.IsZero() {
			// The deadline is the stored fact and the open time is derived from it,
			// exactly as loadSteps derives it, so the pair is one canonical value:
			// normalizing it again, or after a reload, changes nothing, and
			// Deadline == OpenedAt + Timeout still holds for a timeout that is not
			// a whole number of microseconds. The timeout itself is never changed.
			w.Deadline = micro(w.Deadline)
			w.OpenedAt = w.Deadline.Add(-w.Spec.Timeout)
		}
	}
	t.Commands = slices.Clone(t.Commands)
	for i := range t.Commands {
		t.Commands[i].RecordedAt = micro(t.Commands[i].RecordedAt)
	}
	t.Workloads = slices.Clone(t.Workloads)
	for i := range t.Workloads {
		t.Workloads[i].ObservedAt = micro(t.Workloads[i].ObservedAt)
	}
	t.Resources = slices.Clone(t.Resources)
	for i := range t.Resources {
		t.Resources[i].ObservedAt = micro(t.Resources[i].ObservedAt)
	}
	t.Materializations = slices.Clone(t.Materializations)
	for i := range t.Materializations {
		t.Materializations[i].IssuedAt = micro(t.Materializations[i].IssuedAt)
	}
	t.Events = slices.Clone(t.Events)
	for i := range t.Events {
		t.Events[i].At = micro(t.Events[i].At)
	}
	return t
}

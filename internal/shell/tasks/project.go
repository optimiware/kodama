package tasks

import (
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// ViewVersion is the version of the records a RunView was rebuilt from: the sum
// of the run's version and the versions of its steps and waits. Each of them only
// grows and every committed change (a step move, a wait opening, a decision, a
// cancellation) raises at least one of them, so the sum orders any two views of
// one run, including a wait decision that leaves the run's own version unchanged.
// It is a pure function of retained records, so a rebuild reproduces it.
func ViewVersion(v runtime.RunView) domain.Version {
	n := int64(v.Run.Version)
	for _, s := range v.Run.Steps {
		n += int64(s.Version)
	}
	for _, w := range v.Waits {
		n += int64(w.Version)
	}
	return domain.Version(n)
}

// Project derives a task's execution projection from the records of one run. It
// is the only way a Progress is produced, so the incremental path (an event) and
// the rebuild share it and cannot drift.
//
// The status follows the run: queued until a step moves, running while it
// executes (including while its resources are released after it settled),
// waiting while a wait step is parked on an open wait, and the outcome once the
// run is terminal. WaitingFor is the open wait's assignment and nothing else.
func Project(v runtime.RunView) Progress {
	run := v.Run
	p := Progress{RunID: run.ID, RunVersion: ViewVersion(v)}
	switch run.Status() {
	case runtime.StatusQueued:
		p.Status = StatusQueued
	case runtime.StatusSucceeded:
		p.Status = StatusSucceeded
	case runtime.StatusFailed:
		p.Status = StatusFailed
	case runtime.StatusCancelled:
		p.Status = StatusCancelled
	default: // running, and cleaning: resources are still being released
		p.Status = StatusRunning
	}
	switch p.Status {
	case StatusRunning:
		if run.Settled() {
			break // cleaning: no step is active any more
		}
		for _, s := range run.Steps {
			if s.State == runtime.StepWaiting {
				for _, w := range v.Waits {
					if w.StepKey == s.Key && w.State == runtime.WaitOpen {
						p.Status, p.Step, p.WaitingFor = StatusWaiting, s.Key, w.Assignment
						return p
					}
				}
			}
		}
		for _, s := range run.Steps {
			if s.State == runtime.StepRunning || s.State == runtime.StepWaiting {
				p.Step = s.Key
				break
			}
		}
	case StatusSucceeded, StatusFailed, StatusCancelled:
		p.Result = Result{Outcome: run.Outcome, Reason: run.Reason}
		for _, s := range run.Steps {
			if s.State == runtime.StepFailed {
				p.Result.Step = s.Key
				break
			}
		}
	}
	return p
}

// Verdict is what the projection guard decided about an observation.
type Verdict int

const (
	// VerdictApplied: the observation was newer and the projection now reflects it.
	VerdictApplied Verdict = iota
	// VerdictReplayed: the observation is the one already held; nothing changed.
	VerdictReplayed
	// VerdictStale: the observation is of another run, or older than the one held; it
	// did not change the task.
	VerdictStale
)

func (v Verdict) String() string {
	switch v {
	case VerdictApplied:
		return "applied"
	case VerdictReplayed:
		return "replayed"
	}
	return "stale"
}

// advance is the one projection guard, shared by every Store adapter. cur is the
// stored progress and obs the projection of an observed run view.
//
// An observation of any run but the current one is stale, so an older run cannot
// overwrite a newer view (selecting a different run is Start and Rerun's job,
// never a projection's). Within the current run a lower observed version is
// stale and an equal one is a replay; only a higher one applies. A rebuild
// (force) also reapplies an equal version, so a damaged projection is repaired
// from the records without ever accepting an older one.
//
// The returned progress bumps Version only when a projected field changed, so
// an observation that moves records without changing the view emits no event.
func advance(cur, obs Progress, force bool) (Progress, Verdict) {
	switch {
	case cur.RunID == "" || obs.RunID != cur.RunID, obs.RunVersion < cur.RunVersion:
		return cur, VerdictStale
	case obs.RunVersion == cur.RunVersion && (!force || cur.same(obs)):
		return cur, VerdictReplayed
	}
	next := obs
	next.Version = cur.Version
	if !cur.same(obs) {
		next.Version++
	}
	return next, VerdictApplied
}

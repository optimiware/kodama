package tasks

import (
	"errors"
	"strconv"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func progressEvents(r *rig) []Event {
	var out []Event
	for _, e := range r.events() {
		if e.Type == EventTaskProgressChanged {
			out = append(out, e)
		}
	}
	return out
}

// Test_unit_tasks_projection_versioning holds the execution projection to WM-14
// and WM-17: it is derived from run and step records, an event of another or an
// older run, or of older records, never overwrites a newer view, a replay changes
// nothing, and rebuilding from the records yields the same view.
func Test_unit_tasks_projection_versioning(t *testing.T) {
	t.Run("the projection follows the run through every status", func(t *testing.T) {
		r := newRig(t)
		task := r.task("flow")
		if task.Progress.Status != StatusPending {
			t.Fatalf("before a run: %+v", task.Progress)
		}
		task = r.start(task)
		run := task.Progress.RunID
		if p := task.Progress; p.Status != StatusQueued || p.Step != "" || p.Version != 1 || p.RunVersion == 0 {
			t.Fatalf("queued: %+v", p)
		}

		e := r.engine(run)
		e.step(func(x *runtime.Run) error { return x.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
		r.apply(run)
		if p := r.get(task.ID).Progress; p.Status != StatusRunning || p.Step != "build" || p.WaitingFor != "" {
			t.Fatalf("running: %+v", p)
		}

		e.step(func(x *runtime.Run) error {
			return x.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
		})
		e.openReview()
		r.apply(run)
		p := r.get(task.ID).Progress
		if p.Status != StatusWaiting || p.Step != "review" || p.WaitingFor != "reviewer" {
			t.Fatalf("waiting: %+v", p)
		}

		// A decision moves the wait, not the run's own version; the projection
		// still sees it and stops reporting a wait.
		w := e.rec.Waits[indexWait(e.rec, "review")]
		res, err := r.svc.Resolve(r.ctx, ResolveRequest{Caller: r.caller(scopeA, "alice"), TaskID: task.ID, RunID: run, StepKey: "review",
			WaitID: w.ID, ExpectedVersion: w.Version, Key: "k1", Outcome: "approved"})
		if err != nil || !res.Applied {
			t.Fatalf("resolve = %+v, %v", res, err)
		}
		r.apply(run)
		after := r.get(task.ID).Progress
		if after.Status != StatusRunning || after.WaitingFor != "" || after.RunVersion <= p.RunVersion || after.Version != p.Version+1 {
			t.Fatalf("after the decision: %+v (was %+v)", after, p)
		}
	})

	t.Run("a terminal run projects its outcome and a bounded result", func(t *testing.T) {
		r := newRig(t)
		r.plan.plan = solePlan()
		task := r.start(r.task("done"))
		r.engine(task.Progress.RunID).succeed()
		r.apply(task.Progress.RunID)
		p := r.get(task.ID).Progress
		if p.Status != StatusSucceeded || p.Step != "" || p.Result.Outcome != runtime.OutcomeSucceeded || !p.Status.Terminal() {
			t.Fatalf("terminal: %+v", p)
		}
	})

	t.Run("a settled run that is still cleaning up is not terminal", func(t *testing.T) {
		r := newRig(t)
		r.plan.plan = solePlan()
		task := r.start(r.task("cleaning"))
		e := r.engine(task.Progress.RunID)
		e.build()
		e.step(func(x *runtime.Run) error { return x.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone) })
		r.apply(task.Progress.RunID)
		if p := r.get(task.ID).Progress; p.Status != StatusRunning || p.Step != "" || p.Result != (Result{}) {
			t.Fatalf("cleaning: %+v, want running with no result yet", p)
		}
		if _, err := r.svc.Rerun(r.ctx, RerunRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID, ExpectedRunID: task.Progress.RunID}); !errors.Is(err, ErrRunActive) {
			t.Fatalf("a rerun before cleanup finished: %v, want the run to be active", err)
		}
	})

	t.Run("an older view of the current run cannot overwrite a newer one", func(t *testing.T) {
		r := newRig(t)
		task := r.start(r.task("order"))
		run := task.Progress.RunID
		e := r.engine(run)
		e.step(func(x *runtime.Run) error { return x.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
		older, err := r.runs.View(r.ctx, scopeA, run) // build is running
		if err != nil {
			t.Fatal(err)
		}
		e.step(func(x *runtime.Run) error {
			return x.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
		})
		e.openReview()
		r.apply(run)
		newer := r.get(task.ID)
		if newer.Progress.Status != StatusWaiting {
			t.Fatalf("setup: %+v", newer.Progress)
		}

		got, verdict, err := r.store.Advance(r.ctx, scopeA, task.ID, Project(older), false)
		if err != nil || verdict != VerdictStale {
			t.Fatalf("an older view: %v, %v, want stale", verdict, err)
		}
		if got.Progress != newer.Progress || r.get(task.ID).Progress != newer.Progress {
			t.Fatalf("the older view changed the task: %+v, want %+v", r.get(task.ID).Progress, newer.Progress)
		}
		// A forced rebuild from an older view is just as stale.
		if _, verdict, _ := r.store.Advance(r.ctx, scopeA, task.ID, Project(older), true); verdict != VerdictStale {
			t.Fatalf("a rebuild from an older view: %v, want stale", verdict)
		}
		// The event itself is only a notification: a late one for the same run is
		// answered from the records and changes nothing.
		before := len(progressEvents(r))
		if a := r.apply(run); a.Verdict != VerdictReplayed || len(progressEvents(r)) != before {
			t.Fatalf("a late event: %v, %d new events", a.Verdict, len(progressEvents(r))-before)
		}
	})

	t.Run("a replayed event applies nothing and emits no second progress event", func(t *testing.T) {
		r := newRig(t)
		task, _ := r.waiting(r.task("replay"))
		events := progressEvents(r)
		if again := r.apply(task.Progress.RunID); again.Verdict != VerdictReplayed {
			t.Fatalf("replay verdict = %v", again.Verdict)
		}
		if got := progressEvents(r); len(got) != len(events) {
			t.Fatalf("a replay emitted events: %d -> %d", len(events), len(got))
		}
		seen := map[string]bool{}
		for _, e := range events {
			if e.DedupKey == "" {
				continue // PostgreSQL hashes the key into the event ID
			}
			if seen[e.DedupKey] {
				t.Fatalf("progress events share a deduplication key: %v", e.DedupKey)
			}
			seen[e.DedupKey] = true
		}
		if last := events[len(events)-1]; last.DedupKey != "" && last.DedupKey != string(task.ID)+":"+strconv.FormatInt(int64(task.Progress.Version), 10) {
			t.Fatalf("the last event's key %q does not carry the projection version %d", last.DedupKey, task.Progress.Version)
		}
	})

	t.Run("an older run cannot overwrite the newer view after a rerun", func(t *testing.T) {
		r := newRig(t)
		r.plan.plan = solePlan()
		task := r.start(r.task("rerun"))
		first := task.Progress.RunID
		e1 := r.engine(first)
		e1.succeed()
		r.apply(first)
		finished := r.get(task.ID)
		beforeRun, err := r.runs.Load(r.ctx, scopeA, first)
		if err != nil {
			t.Fatal(err)
		}
		staleView, err := r.runs.View(r.ctx, scopeA, first)
		if err != nil {
			t.Fatal(err)
		}

		second, err := r.svc.Rerun(r.ctx, RerunRequest{Caller: r.caller(scopeA, "ops"), TaskID: task.ID, ExpectedRunID: first})
		if err != nil {
			t.Fatal(err)
		}
		next := second.Progress.RunID
		if next == first || next == "" || second.Progress.Status != StatusQueued || second.Progress.Version != finished.Progress.Version+1 {
			t.Fatalf("rerun = %+v (the first run was %s, progress %+v)", second.Progress, first, finished.Progress)
		}
		// Exactly one current run; the earlier one is history.
		if owner, err := r.store.TaskOfRun(r.ctx, scopeA, first); err != nil || owner != task.ID {
			t.Fatalf("the first run left the task's history: %v %v", owner, err)
		}
		// The first run's rows are exactly as they ended: nothing was reset.
		afterRun, err := r.runs.Load(r.ctx, scopeA, first)
		if err != nil || afterRun.Run.Version != beforeRun.Run.Version || afterRun.Run.Outcome != runtime.OutcomeSucceeded {
			t.Fatalf("the rerun changed the first run: %+v, %v", afterRun.Run, err)
		}
		for i, s := range afterRun.Run.Steps {
			if s != beforeRun.Run.Steps[i] || s.State != runtime.StepSucceeded {
				t.Fatalf("step %s was reset: %+v", s.Key, s)
			}
		}

		// Events and views of the older run cannot overwrite the new view, even a
		// forced rebuild.
		if _, verdict, err := r.store.Advance(r.ctx, scopeA, task.ID, Project(staleView), false); err != nil || verdict != VerdictStale {
			t.Fatalf("an older run's view: %v, %v, want stale", verdict, err)
		}
		if _, verdict, _ := r.store.Advance(r.ctx, scopeA, task.ID, Project(staleView), true); verdict != VerdictStale {
			t.Fatalf("an older run's rebuild: %v, want stale", verdict)
		}
		if a := r.apply(first); a.Verdict != VerdictStale {
			t.Fatalf("an older run's event: %v, want stale", a.Verdict)
		}
		if got := r.get(task.ID).Progress; got != second.Progress {
			t.Fatalf("the older run changed the view: %+v, want %+v", got, second.Progress)
		}
		// The new run projects normally, and its version may restart below the old run's.
		e2 := r.engine(next)
		e2.step(func(x *runtime.Run) error { return x.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
		if a := r.apply(next); a.Verdict != VerdictApplied || a.Task.Progress.Status != StatusRunning {
			t.Fatalf("the new run: %+v", a)
		}
	})

	t.Run("rebuilding from the records yields the incremental view and repairs damage", func(t *testing.T) {
		r := newRig(t)
		task, _ := r.waiting(r.task("rebuild"))
		want := r.get(task.ID).Progress

		got, err := r.svc.Rebuild(r.ctx, r.caller(scopeA, "eng"), task.ID)
		if err != nil || got.Progress != want {
			t.Fatalf("rebuild = %+v, %v, want the incremental view %+v", got.Progress, err, want)
		}
		view, _ := r.runs.View(r.ctx, scopeA, want.RunID)
		fresh := Project(view)
		fresh.Version = want.Version
		if fresh != want {
			t.Fatalf("Project = %+v, incremental = %+v", fresh, want)
		}

		// A projection damaged without any version change is repaired by a rebuild,
		// never by an event.
		damaged := want
		damaged.Status, damaged.WaitingFor, damaged.Step = StatusFailed, "", ""
		r.damage(task, damaged)
		if a := r.apply(want.RunID); a.Verdict != VerdictReplayed || r.get(task.ID).Progress.Status != StatusFailed {
			t.Fatalf("an event repaired the damage: %v", a.Verdict)
		}
		fixed, err := r.svc.Rebuild(r.ctx, r.caller(scopeA, "eng"), task.ID)
		if err != nil || fixed.Progress.Status != StatusWaiting || fixed.Progress.WaitingFor != "reviewer" ||
			fixed.Progress.Version != damaged.Version+1 || fixed.Progress.RunVersion != want.RunVersion {
			t.Fatalf("rebuild after damage = %+v, %v", fixed.Progress, err)
		}
		if _, err := r.svc.Rebuild(r.ctx, r.caller(scopeA, "view"), task.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("a viewer rebuilds: %v", err)
		}
		n, err := r.svc.RebuildProject(r.ctx, r.caller(scopeA, "eng"))
		if err != nil || n != 1 {
			t.Fatalf("rebuild project = %d, %v, want the one task with a run", n, err)
		}
	})

	t.Run("the guard orders observations by run and version", func(t *testing.T) {
		cur := Progress{RunID: "r2", Status: StatusRunning, Step: "a", Version: 3, RunVersion: 10}
		for _, tc := range []struct {
			name    string
			obs     Progress
			force   bool
			verdict Verdict
			version domain.Version
		}{
			{"newer, changed", Progress{RunID: "r2", Status: StatusWaiting, Step: "b", WaitingFor: "reviewer", RunVersion: 11}, false, VerdictApplied, 4},
			{"newer, unchanged view", Progress{RunID: "r2", Status: StatusRunning, Step: "a", RunVersion: 11}, false, VerdictApplied, 3},
			{"equal", Progress{RunID: "r2", Status: StatusFailed, RunVersion: 10}, false, VerdictReplayed, 3},
			{"equal, forced and different", Progress{RunID: "r2", Status: StatusFailed, RunVersion: 10}, true, VerdictApplied, 4},
			{"equal, forced and same", Progress{RunID: "r2", Status: StatusRunning, Step: "a", RunVersion: 10}, true, VerdictReplayed, 3},
			{"older", Progress{RunID: "r2", Status: StatusFailed, RunVersion: 9}, true, VerdictStale, 3},
			{"another run, higher version", Progress{RunID: "r1", Status: StatusFailed, RunVersion: 99}, true, VerdictStale, 3},
		} {
			next, v := advance(cur, tc.obs, tc.force)
			if v != tc.verdict || next.Version != tc.version {
				t.Errorf("%s: verdict %v version %d, want %v %d", tc.name, v, next.Version, tc.verdict, tc.version)
			}
			if v != VerdictApplied && next != cur {
				t.Errorf("%s: a %v observation changed the progress to %+v", tc.name, v, next)
			}
		}
		if _, v := advance(Progress{Status: StatusPending}, Progress{RunID: "r1", RunVersion: 1}, false); v != VerdictStale {
			t.Errorf("a projection never selects a run: %v", v)
		}
	})

	t.Run("a run that no task owns, or another project's run, projects nothing", func(t *testing.T) {
		r := newRig(t)
		task := r.start(r.task("own"))
		a, err := r.svc.Apply(r.ctx, runtime.Event{ID: "e", Scope: scopeA, RunID: "run_unowned", Kind: runtime.EventRunStateChanged, Version: 1, At: r.clock.Now()})
		if err != nil || !a.NoTask {
			t.Fatalf("an unowned run: %+v, %v", a, err)
		}
		a, err = r.svc.Apply(r.ctx, runtime.Event{ID: "e", Scope: scopeC, RunID: task.Progress.RunID, Kind: runtime.EventRunStateChanged, Version: 1, At: r.clock.Now()})
		if err != nil || !a.NoTask {
			t.Fatalf("another project's event for this run: %+v, %v", a, err)
		}
		if _, err := r.svc.Apply(r.ctx, runtime.Event{}); err == nil {
			t.Fatal("an invalid event was accepted")
		}
	})

	t.Run("a failed projection write changes nothing and a redelivery applies it", func(t *testing.T) {
		r := newRig(t)
		if !r.memoryOnly() {
			return
		}
		task := r.start(r.task("fault"))
		run := task.Progress.RunID
		before := r.get(task.ID).Progress
		e := r.engine(run)
		e.step(func(x *runtime.Run) error { return x.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
		boom := errors.New("boom")
		r.failNext(boom)
		n := len(progressEvents(r))
		if _, err := r.svc.Apply(r.ctx, runtime.Event{ID: "e", Scope: scopeA, RunID: run, Kind: runtime.EventRunStateChanged, Version: 2, At: r.clock.Now()}); !errors.Is(err, boom) {
			t.Fatalf("apply = %v, want the write failure, which leaves the event for redelivery", err)
		}
		if got := r.get(task.ID).Progress; got != before || len(progressEvents(r)) != n {
			t.Fatalf("the failed write changed the task: %+v, %d events", got, len(progressEvents(r)))
		}
		if a := r.apply(run); a.Verdict != VerdictApplied || a.Task.Progress.Status != StatusRunning {
			t.Fatalf("the redelivery: %+v", a)
		}
	})
}

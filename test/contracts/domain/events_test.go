package domaincontract

// Task cancellation publishes the lifecycle once.
//
// Both task paths commit the Kernel cancel reduction inside the task unit:
//
//	unit.Events = append(unit.Events,
//	    event.ExcludeOwned(event.ProjectLifecycle(pre, commit))...)
//
// The unit's own event (`task.question` for AskUnit, `task.cancelled` for
// CancelUnit) is NOT a lifecycle projection class, so nothing is excluded and
// nothing may be dropped. For a running container execution with one running
// action and one pending action the union publishes, in the fixed order:
//
//	owner event, execution.cancelled, execution.cleanup,
//	action.transition (running action), action.transition (pending action)
//
// For a queued predecessor the same union publishes the owner event,
// execution.cancelled, execution.cleanup and the cancelled pending action, and
// carries NO stop intent because the workload never started.
//
// The commit is one payload: the execution state, every action update, the
// output discard, the credential invalidation, the stop intent (when a workload
// exists) and the cleanup progress travel together, so a crash before the
// effect leaves either the whole pre-state or the whole post-state.

import (
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
)

// publishedEvent is the (type, subject) identity of one published event.
type publishedEvent struct {
	Type      event.Type
	SubjectID string
}

// projectedEvent is one expected projection entry with its committed subject
// version.
type projectedEvent struct {
	Type           event.Type
	SubjectID      string
	SubjectVersion uint64
}

type cancelEventCase struct {
	name          string
	pre           kernel.Execution
	owner         event.Envelope
	wantProjected []projectedEvent
	wantPublished []publishedEvent
	wantStop      bool
}

func cancelEventCases() []cancelEventCase {
	return []cancelEventCase{
		{
			name:  "running container cancellation stops the workload and cancels every action once",
			pre:   runningExecution(),
			owner: event.Envelope{Type: event.TypeTaskCancelled, SubjectID: string(cancelTask)},
			wantProjected: []projectedEvent{
				{Type: event.TypeExecutionCancelled, SubjectID: string(cancelExecution), SubjectVersion: 8},
				{Type: event.TypeCleanup, SubjectID: string(cancelExecution), SubjectVersion: 8},
				{Type: event.TypeActionTransition, SubjectID: string(cancelContainer), SubjectVersion: 5},
				{Type: event.TypeActionTransition, SubjectID: string(cancelPendingAction), SubjectVersion: 2},
			},
			wantPublished: []publishedEvent{
				{Type: event.TypeTaskCancelled, SubjectID: string(cancelTask)},
				{Type: event.TypeExecutionCancelled, SubjectID: string(cancelExecution)},
				{Type: event.TypeCleanup, SubjectID: string(cancelExecution)},
				{Type: event.TypeActionTransition, SubjectID: string(cancelContainer)},
				{Type: event.TypeActionTransition, SubjectID: string(cancelPendingAction)},
			},
			wantStop: true,
		},
		{
			name:  "queued ask cancellation never emits a start or stop for work that never started",
			pre:   queuedExecution(),
			owner: event.Envelope{Type: event.TypeTaskQuestion, SubjectID: string(cancelTask)},
			wantProjected: []projectedEvent{
				{Type: event.TypeExecutionCancelled, SubjectID: string(cancelExecution), SubjectVersion: 3},
				{Type: event.TypeCleanup, SubjectID: string(cancelExecution), SubjectVersion: 3},
				{Type: event.TypeActionTransition, SubjectID: string(cancelPendingAction), SubjectVersion: 2},
			},
			wantPublished: []publishedEvent{
				{Type: event.TypeTaskQuestion, SubjectID: string(cancelTask)},
				{Type: event.TypeExecutionCancelled, SubjectID: string(cancelExecution)},
				{Type: event.TypeCleanup, SubjectID: string(cancelExecution)},
				{Type: event.TypeActionTransition, SubjectID: string(cancelPendingAction)},
			},
			wantStop: false,
		},
	}
}

// preImageOf maps the durable aggregate onto the projection pre-image. It reads
// only the version-guarded fields the projection needs.
func preImageOf(exec kernel.Execution) event.ExecutionPreImage {
	actions := make([]event.ActionPreImage, 0, len(exec.Actions))
	for _, action := range exec.Actions {
		actions = append(actions, event.ActionPreImage{
			ActionID: action.ID,
			Index:    action.Index,
			Kind:     action.Kind,
			State:    action.State,
			Version:  action.Version,
		})
	}
	return event.ExecutionPreImage{
		Scope:       cancelScope,
		ExecutionID: exec.ID,
		Version:     exec.Version,
		State:       exec.State,
		Actions:     actions,
	}
}

// TestTaskCancellationPublishesLifecycleOnce pins the emitter union for
// both task paths: the owner's own event plus the cancel projection, with each
// transition class published exactly once and deterministic ids.
func TestTaskCancellationPublishesLifecycleOnce(t *testing.T) {
	for _, testCase := range cancelEventCases() {
		t.Run(testCase.name, func(t *testing.T) {
			next, commit, err := flow.Reduce(testCase.pre, flow.CancelFact{
				ExecutionID: testCase.pre.ID,
				Reason:      flow.CancelRequested,
				At:          cancelAt,
			})
			if err != nil {
				t.Fatalf("flow.Reduce(cancel) refused a cancellable execution: %v", err)
			}
			if commit == nil {
				t.Fatal("flow.Reduce(cancel) returned no commit")
			}
			if next.State != kernel.ExecutionCancelled {
				t.Fatalf("reduced state = %q; want %q", next.State, kernel.ExecutionCancelled)
			}
			if commit.ExecutionID != testCase.pre.ID {
				t.Fatalf("commit execution id = %q; want %q", commit.ExecutionID, testCase.pre.ID)
			}
			if commit.ExpectedExecutionVersion != testCase.pre.Version {
				t.Fatalf("commit expected version = %d; want the aggregate version %d",
					commit.ExpectedExecutionVersion, testCase.pre.Version)
			}
			if commit.NextExecutionState != kernel.ExecutionCancelled {
				t.Fatalf("commit next state = %q; want %q", commit.NextExecutionState, kernel.ExecutionCancelled)
			}
			// Output settlement: cancellation discards staged values and
			// invalidates the execution credential in the same commit.
			if commit.Output.Action != flow.OutputDiscard {
				t.Fatalf("commit output action = %q; want %q", commit.Output.Action, flow.OutputDiscard)
			}
			if !commit.InvalidateCredential {
				t.Fatal("cancellation commit does not invalidate the execution credential")
			}
			// Cleanup is owed even when there is no workload to stop.
			if commit.Cleanup == nil {
				t.Fatal("cancellation commit carries no cleanup intent")
			}
			if commit.CleanupUpdate == nil || commit.CleanupUpdate.State != kernel.CleanupPending {
				t.Fatalf("cleanup progress = %#v; want a pending update", commit.CleanupUpdate)
			}
			// The stop barrier is a request derived from a running container
			// workload; a queued execution has no workload and must not emit one.
			if testCase.wantStop {
				if commit.Stop == nil {
					t.Fatal("running container cancellation carries no stop intent")
				}
				if commit.Stop.WorkloadID != cancelWorkload || commit.Stop.WorkspaceID != cancelWorkspace {
					t.Fatalf("stop intent = %#v; want workload %q in workspace %q",
						*commit.Stop, cancelWorkload, cancelWorkspace)
				}
				if commit.Cleanup.WorkloadID != cancelWorkload {
					t.Fatalf("cleanup workload = %q; want %q", commit.Cleanup.WorkloadID, cancelWorkload)
				}
			} else {
				if commit.Stop != nil {
					t.Fatalf("queued cancellation emitted a stop intent %#v for work that never started", *commit.Stop)
				}
				if commit.Cleanup.WorkloadID != "" {
					t.Fatalf("queued cancellation cleanup names workload %q; want none", commit.Cleanup.WorkloadID)
				}
			}
			// Every pending successor is cancelled in the same commit.
			if len(commit.PendingCancellations) != 1 {
				t.Fatalf("commit carries %d pending cancellations; want 1", len(commit.PendingCancellations))
			}
			if commit.PendingCancellations[0].NextState != kernel.ActionCancelled {
				t.Fatalf("pending cancellation next state = %q; want %q",
					commit.PendingCancellations[0].NextState, kernel.ActionCancelled)
			}

			pre := preImageOf(testCase.pre)
			projected, err := event.ProjectLifecycle(pre, *commit)
			if err != nil {
				t.Fatalf("ProjectLifecycle refused the cancel commit: %v", err)
			}
			if len(projected) != len(testCase.wantProjected) {
				t.Fatalf("projection yielded %d events; want %d: %s",
					len(projected), len(testCase.wantProjected), describeSpecs(projected))
			}
			seenIDs := make(map[id.EventID]bool, len(projected))
			for index, want := range testCase.wantProjected {
				spec := projected[index]
				if spec.Type != want.Type || spec.SubjectID != want.SubjectID || spec.SubjectVersion != want.SubjectVersion {
					t.Fatalf("projected[%d] = %s@%s v%d; want %s@%s v%d",
						index, spec.Type, spec.SubjectID, spec.SubjectVersion,
						want.Type, want.SubjectID, want.SubjectVersion)
				}
				if spec.ID == "" {
					t.Fatalf("projected[%d] has an empty event id", index)
				}
				if seenIDs[spec.ID] {
					t.Fatalf("projected event id %q is not unique inside one projection", spec.ID)
				}
				seenIDs[spec.ID] = true
				if !spec.At.Equal(cancelAt) {
					t.Fatalf("projected[%d] occurrence time = %s; want %s", index, spec.At, cancelAt)
				}
				if spec.Scope != cancelScope {
					t.Fatalf("projected[%d] scope = %#v; want %#v", index, spec.Scope, cancelScope)
				}
				if spec.Source != event.SystemSource {
					t.Fatalf("projected[%d] source = %#v; want the trusted system source", index, spec.Source)
				}
			}
			// A cancel is not a start: no execution.started and no
			// action.requested may be published for the cancelled work.
			for _, spec := range projected {
				if spec.Type == event.TypeExecutionStarted {
					t.Fatalf("cancellation published %q", event.TypeExecutionStarted)
				}
				if testCase.pre.State == kernel.ExecutionQueued && spec.Type == event.TypeActionRequested {
					t.Fatalf("queued cancellation published %q for a request that never activated", event.TypeActionRequested)
				}
			}
			// Determinism: the same pre-image and commit reproduce the exact
			// same ids and order.
			again, err := event.ProjectLifecycle(pre, *commit)
			if err != nil {
				t.Fatalf("replayed ProjectLifecycle was refused: %v", err)
			}
			if !reflect.DeepEqual(projected, again) {
				t.Fatalf("replayed projection differs:\n first: %s\nsecond: %s",
					describeSpecs(projected), describeSpecs(again))
			}

			// The task unit owns only its task event, which is not a projected
			// class: ExcludeOwned drops nothing and the union publishes every
			// lifecycle transition exactly once.
			appended := event.ExcludeOwned(projected)
			if !reflect.DeepEqual(appended, projected) {
				t.Fatalf("ExcludeOwned dropped a lifecycle event with an empty owned set:\n got: %s\nwant: %s",
					describeSpecs(appended), describeSpecs(projected))
			}
			published := make([]publishedEvent, 0, 1+len(appended))
			published = append(published, publishedEvent{Type: testCase.owner.Type, SubjectID: testCase.owner.SubjectID})
			for _, spec := range appended {
				published = append(published, publishedEvent{Type: spec.Type, SubjectID: spec.SubjectID})
			}
			if !reflect.DeepEqual(published, testCase.wantPublished) {
				t.Fatalf("published order = %s; want %s",
					describePublished(published), describePublished(testCase.wantPublished))
			}
			counts := make(map[publishedEvent]int, len(published))
			for _, spec := range published {
				counts[spec]++
			}
			for spec, count := range counts {
				if count != 1 {
					t.Fatalf("%q@%q is published %d times; want exactly once", spec.Type, spec.SubjectID, count)
				}
			}
			for _, spec := range appended {
				if spec.Type == testCase.owner.Type && spec.SubjectID == testCase.owner.SubjectID {
					t.Fatalf("appended projection repeats the owner's own event %q@%q", spec.Type, spec.SubjectID)
				}
			}
		})
	}
}

func describeSpecs(specs []event.LifecycleEvent) string {
	parts := make([]string, 0, len(specs))
	for _, spec := range specs {
		parts = append(parts, string(spec.Type)+"@"+spec.SubjectID)
	}
	return "[" + join(parts, ", ") + "]"
}

func describePublished(events []publishedEvent) string {
	parts := make([]string, 0, len(events))
	for _, spec := range events {
		parts = append(parts, string(spec.Type)+"@"+spec.SubjectID)
	}
	return "[" + join(parts, ", ") + "]"
}

func join(parts []string, separator string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, part := range parts[1:] {
		out += separator + part
	}
	return out
}

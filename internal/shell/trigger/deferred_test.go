package trigger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// TestDeferredSelectionResultIsRetained covers a selection store that reports a
// transient dependency failure as a deferred result rather than an error. No
// admission or start may run until the first selection is durable, and the
// retry must produce exactly one execution.
func TestDeferredSelectionResultIsRetained(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.selections.setSelectFn(func(trigger.SelectUnit) (trigger.Result, error) {
		return trigger.Result{Status: trigger.StatusDeferred}, nil
	})

	first := fixture.process(t)

	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; a deferred selection result must retain the event", first.Deferred)
	}
	if fixture.admission.callCount() != 0 || fixture.start.callCount() != 0 {
		t.Fatalf("admission/start ran %d/%d times without a durable first selection", fixture.admission.callCount(), fixture.start.callCount())
	}
	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; a deferred selection must not advance the cursor", fixture.completion.sequences())
	}

	fixture.selections.setSelectFn(nil)
	second := fixture.process(t)

	if !hasExecution(second.Started, id.ExecutionID("exec-1")) {
		t.Fatalf("started = %v; the retried selection must start exec-1", second.Started)
	}
	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; want exactly one", fixture.start.executionCount())
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the settled event must advance", fixture.completion.sequences())
	}
}

// uncompletedEvents filters out already-completed sequences, matching the
// real committed-event reader contract when Run loops across batches.
type uncompletedEvents struct {
	mu         sync.Mutex
	rows       []event.PendingEvent
	completion *fakeCompletion
	calls      int
}

func (events *uncompletedEvents) Pending(_ context.Context, after event.Sequence, limit int) ([]event.PendingEvent, error) {
	events.mu.Lock()
	events.calls++
	rows := append([]event.PendingEvent(nil), events.rows...)
	events.mu.Unlock()

	var pending []event.PendingEvent
	for _, row := range rows {
		if row.Sequence <= after || events.completion.has(row.Sequence) {
			continue
		}
		pending = append(pending, row)
		if limit > 0 && len(pending) == limit {
			break
		}
	}
	return pending, nil
}

func (events *uncompletedEvents) callCount() int {
	events.mu.Lock()
	defer events.mu.Unlock()
	return events.calls
}

// TestDeferredHeadOfLineDoesNotSpinRun proves that when the head-of-line event
// is deferred — even when Pending returned a full batch (len(rows) >= BatchSize)
// — Process reports More=false and Run yields to Idle (or returns nil when Idle
// is nil) instead of spinning forever.
func TestDeferredHeadOfLineDoesNotSpinRun(t *testing.T) {
	scope := testScope()
	candidate := testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	)
	rows := []event.PendingEvent{
		pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"),
		pushEvent(2, "event-2", "repo-1", "git-svc", "refs/heads/main", "base-2", "cand-2"),
		pushEvent(3, "event-3", "repo-1", "git-svc", "refs/heads/main", "base-3", "cand-3"),
	}

	t.Run("Idle nil returns after one pass when full batch defers head-of-line", func(t *testing.T) {
		completion := &fakeCompletion{}
		events := &uncompletedEvents{rows: rows, completion: completion}
		candidates := &fakeCandidates{}
		candidates.set(candidate)
		selections := newFakeSelections()
		selections.setSelectFn(func(trigger.SelectUnit) (trigger.Result, error) {
			return trigger.Result{Status: trigger.StatusDeferred}, nil
		})
		processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
			Events:         events,
			Candidates:     candidates,
			Selections:     selections,
			Select:         selections,
			Admission:      &fakeAdmission{},
			Start:          newFakeStart(selections),
			Completion:     completion,
			Clock:          fakeClock{},
			IDs:            &fakeIDs{},
			BatchSize:      2,
			DefaultTimeout: 5 * time.Minute,
		})
		if err != nil {
			t.Fatalf("NewProcessor: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		result, err := processor.Process(ctx)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		if result.More {
			t.Fatal("Process set More=true on a deferred head-of-line batch")
		}
		if !hasEvent(result.Deferred, id.EventID("event-1")) {
			t.Fatalf("Deferred = %v; want [event-1]", result.Deferred)
		}

		if err := processor.Run(ctx); err != nil {
			t.Fatalf("Run with nil Idle: %v", err)
		}
		if got := events.callCount(); got != 2 {
			t.Fatalf("Pending calls = %d; want 2 (1 for Process + 1 for Run, no spin)", got)
		}
	})

	t.Run("Idle callback runs on deferred full batch then continues across settled full batch", func(t *testing.T) {
		completion := &fakeCompletion{}
		events := &uncompletedEvents{rows: rows, completion: completion}
		candidates := &fakeCandidates{}
		candidates.set(candidate)
		selections := newFakeSelections()
		selections.setSelectFn(func(trigger.SelectUnit) (trigger.Result, error) {
			return trigger.Result{Status: trigger.StatusDeferred}, nil
		})
		start := newFakeStart(selections)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		idleCalls := 0
		processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
			Events:         events,
			Candidates:     candidates,
			Selections:     selections,
			Select:         selections,
			Admission:      &fakeAdmission{},
			Start:          start,
			Completion:     completion,
			Clock:          fakeClock{},
			IDs:            &fakeIDs{},
			BatchSize:      2,
			DefaultTimeout: 5 * time.Minute,
			Idle: func(context.Context) error {
				idleCalls++
				if idleCalls == 1 {
					if got := events.callCount(); got != 1 {
						t.Errorf("Pending calls before first Idle = %d; want 1 (no spin on deferred full batch)", got)
					}
					selections.setSelectFn(nil)
					return nil
				}
				cancel()
				return context.Canceled
			},
		})
		if err != nil {
			t.Fatalf("NewProcessor: %v", err)
		}

		err = processor.Run(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v; want context.Canceled", err)
		}
		if idleCalls != 2 {
			t.Fatalf("Idle calls = %d; want 2", idleCalls)
		}
		// Pass 1 deferred on [1,2] -> Idle #1; Pass 2 settled [1,2] (More=true,
		// continued immediately without Idle); Pass 3 settled [3] (More=false) -> Idle #2.
		if got := events.callCount(); got != 3 {
			t.Fatalf("Pending calls = %d; want 3", got)
		}
		if start.executionCount() != 3 {
			t.Fatalf("executions = %d; want all 3 settled", start.executionCount())
		}
		for _, seq := range []event.Sequence{1, 2, 3} {
			if !completion.has(seq) {
				t.Fatalf("completed = %v; missing sequence %d", completion.sequences(), seq)
			}
		}
	})
}

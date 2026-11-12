package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// scriptedTriggerEvents fails the configured number of leading Pending reads
// before returning one committed push row. alwaysFail keeps every read failing
// so a test can prove the loop retries instead of dying.
type scriptedTriggerEvents struct {
	mu         sync.Mutex
	calls      int
	failures   int
	alwaysFail bool
	row        event.PendingEvent
}

func (events *scriptedTriggerEvents) Pending(context.Context, event.Sequence, int) ([]event.PendingEvent, error) {
	events.mu.Lock()
	events.calls++
	call := events.calls
	events.mu.Unlock()
	if events.alwaysFail || call <= events.failures {
		return nil, fault.New(fault.DependencyUnavailable, "transient event read failure")
	}
	if call == events.failures+1 {
		return []event.PendingEvent{events.row}, nil
	}
	return nil, nil
}

func (events *scriptedTriggerEvents) count() int {
	events.mu.Lock()
	defer events.mu.Unlock()
	return events.calls
}

type staticTriggerCandidates struct {
	candidate trigger.Candidate
}

func (candidates staticTriggerCandidates) Candidates(context.Context, tenancy.Scope) ([]trigger.Candidate, error) {
	return []trigger.Candidate{candidates.candidate}, nil
}

type emptyTriggerSelections struct{}

func (emptyTriggerSelections) Lookup(context.Context, tenancy.Scope, pipeline.PipelineID, id.EventID) (trigger.Result, bool, error) {
	return trigger.Result{}, false, nil
}

func (emptyTriggerSelections) Unfinished(context.Context, tenancy.Scope, id.EventID) ([]trigger.Result, error) {
	return nil, nil
}

type recordingTriggerSelect struct {
	mu    sync.Mutex
	calls int
}

func (selection *recordingTriggerSelect) Select(_ context.Context, unit trigger.SelectUnit) (trigger.Result, error) {
	selection.mu.Lock()
	selection.calls++
	selection.mu.Unlock()
	return trigger.Result{
		Status:      trigger.StatusSelected,
		ExecutionID: "execution-push-1",
		Selection:   unit.Selection,
		Receipt:     unit.Receipt,
	}, nil
}

func (selection *recordingTriggerSelect) count() int {
	selection.mu.Lock()
	defer selection.mu.Unlock()
	return selection.calls
}

type unusedTriggerAdmission struct{}

func (unusedTriggerAdmission) Admit(context.Context, pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
	return pipeline.AdmissionResult{}, nil
}

type unusedTriggerStart struct{}

func (unusedTriggerStart) Start(context.Context, trigger.StartUnit) (trigger.Result, error) {
	return trigger.Result{}, nil
}

type recordingTriggerCompletion struct {
	mu        sync.Mutex
	completed []event.Sequence
}

func (completion *recordingTriggerCompletion) Complete(_ context.Context, unit trigger.CompletionUnit) error {
	completion.mu.Lock()
	completion.completed = append(completion.completed, unit.Sequence)
	completion.mu.Unlock()
	return nil
}

func (completion *recordingTriggerCompletion) count() int {
	completion.mu.Lock()
	defer completion.mu.Unlock()
	return len(completion.completed)
}

type triggerTestClock struct{}

func (triggerTestClock) Now() time.Time {
	return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
}

type triggerTestIDs struct {
	mu        sync.Mutex
	nextEvent int
}

func (ids *triggerTestIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.nextEvent++
	return id.EventID("audit-" + string(rune('a'+ids.nextEvent))), nil
}

func (ids *triggerTestIDs) NewReceiptID() (id.ReceiptID, error) {
	return "receipt-push-1", nil
}

func (ids *triggerTestIDs) NewExecutionID() (id.ExecutionID, error) {
	return "execution-push-1", nil
}

// pushEnvelope builds one accepted verified Git ref update report.
func pushEnvelope(t *testing.T) event.Envelope {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"operation":     trigger.ReportOperationGitRefUpdate,
		"resource":      "repo-1",
		"ref":           "refs/heads/main",
		"expected_base": "base-commit",
		"candidate":     "candidate-commit",
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	envelope, err := event.NewEnvelope("event-push-1",
		event.Source{Kind: event.SourceService, PrincipalID: "service-1"},
		scope, triggerTestClock{}.Now(), event.EnvelopeInput{
			Type:       event.TypeVerifiedReport,
			ReportType: trigger.ReportTypeGitRefUpdated,
			SubjectID:  "repo-1",
			Data:       data,
		})
	if err != nil {
		t.Fatalf("build push envelope: %v", err)
	}
	return envelope
}

// pushCandidate builds one pipeline/binding pair matching pushEnvelope.
func pushCandidate() trigger.Candidate {
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	return trigger.Candidate{
		Definition: pipeline.Definition{
			ID:      "pipeline-1",
			Name:    "build",
			YAML:    []byte(smokePipeline),
			Version: 1,
			PushTriggers: []pipeline.PushTrigger{{
				Repository: "repo-1",
				Service:    "service-1",
				Refs:       []string{"refs/heads/main"},
			}},
		},
		Binding: resource.Binding{
			ID:               "binding-1",
			ResourceID:       "repo-1",
			Scope:            scope,
			EnforcingService: "service-1",
			ReportableEvents: []string{string(trigger.ReportTypeGitRefUpdated)},
			Version:          1,
		},
	}
}

// triggerProcessor wires the scripted event source into a real trigger
// processor with deterministic fake commits.
func triggerProcessor(t *testing.T, events trigger.EventReader, selection *recordingTriggerSelect, completion *recordingTriggerCompletion, idle func(context.Context) error) *trigger.Processor {
	t.Helper()
	processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
		Events:         events,
		Candidates:     staticTriggerCandidates{candidate: pushCandidate()},
		Selections:     emptyTriggerSelections{},
		Select:         selection,
		Admission:      unusedTriggerAdmission{},
		Start:          unusedTriggerStart{},
		Completion:     completion,
		Clock:          triggerTestClock{},
		IDs:            &triggerTestIDs{},
		DefaultTimeout: time.Minute,
		Idle:           idle,
	})
	if err != nil {
		t.Fatalf("trigger.NewProcessor: %v", err)
	}
	return processor
}

// TestTriggerLoopResumesAfterTransientFailure proves a failing trigger pass no
// longer kills the only push loop: the supervisor logs it, waits, re-enters and
// a later committed push event still starts its pipeline.
func TestTriggerLoopResumesAfterTransientFailure(t *testing.T) {
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events := &scriptedTriggerEvents{
		failures: 1,
		row:      event.PendingEvent{Sequence: 1, Event: pushEnvelope(t)},
	}
	selection := &recordingTriggerSelect{}
	completion := &recordingTriggerCompletion{}
	app.trigger = triggerProcessor(t, events, selection, completion, func(ctx context.Context) error {
		return environment.clock.Sleep(ctx, environment.config.LoopInterval)
	})
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	eventually(t, 5*time.Second, func() bool {
		return selection.count() > 0
	})
	eventually(t, 5*time.Second, func() bool {
		return completion.count() > 0
	})
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	logged := environment.logs.String()
	if !strings.Contains(logged, "trigger") || !strings.Contains(logged, "retrying") {
		t.Fatalf("trigger failure was not logged for retry: %s", logged)
	}
}

// TestTriggerLoopRetriesRepeatedFailuresAndCloseJoins proves the supervised
// trigger loop keeps retrying a persistent failure at the configured cadence
// and that Close still cancels and joins it.
func TestTriggerLoopRetriesRepeatedFailuresAndCloseJoins(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.config.LoopInterval = 20 * time.Millisecond
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events := &scriptedTriggerEvents{alwaysFail: true}
	app.trigger = triggerProcessor(t, events, &recordingTriggerSelect{}, &recordingTriggerCompletion{}, func(ctx context.Context) error {
		return environment.clock.Sleep(ctx, environment.config.LoopInterval)
	})
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	eventually(t, 5*time.Second, func() bool {
		return events.count() >= 3
	})
	// The retry pause keeps a persistent failure from becoming a busy spin.
	if calls := events.count(); calls > 10 {
		t.Fatalf("trigger loop retried %d times in one interval window; want a paced retry", calls)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	afterClose := events.count()
	time.Sleep(50 * time.Millisecond)
	if events.count() != afterClose {
		t.Fatalf("trigger loop kept running after Close: %d -> %d", afterClose, events.count())
	}
}

// TestSupervisedLoopStopsOnCancellation covers the shared guarantee used by
// every owned loop: a non-cancellation exit is logged and retried with a pause,
// while cancellation ends the loop.
func TestSupervisedLoopStopsOnCancellation(t *testing.T) {
	for _, name := range []string{"engine", "dispatch", "trigger"} {
		t.Run(name, func(t *testing.T) {
			logs := &bytesBuffer{}
			app := &App{
				cfg:    Config{LoopInterval: 5 * time.Millisecond},
				logger: slog.New(slog.NewTextHandler(logs, nil)),
				clock:  newFakeClock(),
			}
			ctx, cancel := context.WithCancel(context.Background())
			var (
				mu    sync.Mutex
				calls int
			)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				app.superviseLoop(ctx, name, func(context.Context) error {
					mu.Lock()
					calls++
					mu.Unlock()
					return fault.New(fault.DependencyUnavailable, "loop failure")
				})
			}()
			eventually(t, 2*time.Second, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return calls >= 2
			})
			cancel()
			select {
			case <-joined:
			case <-time.After(2 * time.Second):
				t.Fatal("supervised loop did not join after cancellation")
			}
			logged := logs.String()
			if !strings.Contains(logged, name) || !strings.Contains(logged, "retrying") {
				t.Fatalf("%s failure was not logged for retry: %s", name, logged)
			}
		})
	}
}

// bytesBuffer is a race-safe log sink for loops that log from a goroutine.
type bytesBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (buffer *bytesBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	buffer.buf = append(buffer.buf, data...)
	buffer.mu.Unlock()
	return len(data), nil
}

func (buffer *bytesBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.buf)
}

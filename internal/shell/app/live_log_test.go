package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// recordingLivePublisher records every live chunk and its explicit scope.
type recordingLivePublisher struct {
	mu     sync.Mutex
	chunks []shelllog.Chunk
	scopes []tenancy.Scope
	fail   bool
}

func (publisher *recordingLivePublisher) PublishLive(_ context.Context, scope tenancy.Scope, chunk shelllog.Chunk) error {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.fail {
		return fault.New(fault.DependencyUnavailable, "live broker is unavailable")
	}
	publisher.chunks = append(publisher.chunks, chunk)
	publisher.scopes = append(publisher.scopes, scope)
	return nil
}

func (publisher *recordingLivePublisher) snapshot() ([]shelllog.Chunk, []tenancy.Scope) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	return append([]shelllog.Chunk(nil), publisher.chunks...),
		append([]tenancy.Scope(nil), publisher.scopes...)
}

// TestLiveLogPumpPublishesWhileWorkloadRuns proves the composition decorator
// publishes bounded chunks with the runtime's execution/action identity and
// monotonic offsets while the workload is still running, without writing to the
// retained sink.
func TestLiveLogPumpPublishesWhileWorkloadRuns(t *testing.T) {
	fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
		0: {
			Chunks: []kernelengine.LogChunk{{
				Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("first\n"),
			}},
			NextOffset: 6,
		},
		6: {
			Chunks: []kernelengine.LogChunk{{
				Stream: kernelengine.LogStdout, Offset: 6, Sequence: 2, Data: []byte("second\n"),
			}},
			NextOffset: 13,
		},
	})
	live := &recordingLivePublisher{}
	fixture.collector.live = live
	fixture.collector.liveInterval = 2 * time.Millisecond

	identity := fixture.removeRequest().Identity
	if _, err := fixture.collector.Start(context.Background(), kernelengine.StartRequest{
		Identity:    identity,
		WorkspaceID: fixture.workspaceID,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = fixture.collector.Close(context.Background()) })

	var chunks []shelllog.Chunk
	var scopes []tenancy.Scope
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		chunks, scopes = live.snapshot()
		if len(chunks) >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(chunks) != 2 {
		t.Fatalf("live chunks = %d; want 2", len(chunks))
	}
	for index, chunk := range chunks {
		if chunk.ExecutionID != fixture.executionID || chunk.ActionID != fixture.actionID {
			t.Fatalf("live chunk %d identity = %s/%s; want %s/%s", index, chunk.ExecutionID, chunk.ActionID, fixture.executionID, fixture.actionID)
		}
		if scopes[index] != fixture.scope {
			t.Fatalf("live chunk %d scope = %+v; want %+v", index, scopes[index], fixture.scope)
		}
	}
	if chunks[0].Offset != 0 || chunks[0].Sequence != 1 || string(chunks[0].Data) != "first\n" {
		t.Fatalf("first live chunk = %+v", chunks[0])
	}
	if chunks[1].Offset != 6 || chunks[1].Sequence != 2 || string(chunks[1].Data) != "second\n" {
		t.Fatalf("second live chunk = %+v", chunks[1])
	}
	if appended := fixture.sink.appendedChunks(); len(appended) != 0 {
		t.Fatalf("live pump wrote %d retained chunks; want none", len(appended))
	}

	// Stop is the lifecycle boundary: the pump is joined before the retention
	// checkpoint, and no live chunk is published after it.
	if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	after, _ := live.snapshot()
	time.Sleep(20 * time.Millisecond)
	settled, _ := live.snapshot()
	if len(settled) != len(after) {
		t.Fatalf("live chunks published after Stop: %d -> %d", len(after), len(settled))
	}
}

// startOutcomeRuntime overrides only the start effect, so one test can supply
// an accepted, unknown, erroring or proven-not-started start over the same log
// pages.
type startOutcomeRuntime struct {
	*logPageRuntime
	result kernelengine.StartResult
	err    error
}

func (runtime *startOutcomeRuntime) Start(context.Context, kernelengine.StartRequest) (kernelengine.StartResult, error) {
	return runtime.result, runtime.err
}

// TestLiveLogPumpStartsUnlessStartProvenNotStarted pins the live pump's start
// gate: an error or an unknown outcome may still have applied the effect (the
// engine persists LaunchUnknown for both), so the pump opens and streams. Only
// affirmative proven-not-started evidence leaves nothing to read.
func TestLiveLogPumpStartsUnlessStartProvenNotStarted(t *testing.T) {
	cases := []struct {
		name      string
		result    kernelengine.StartResult
		err       error
		wantChunk bool
	}{
		{
			name:      "start error may have applied",
			err:       fault.New(fault.UncertainExternalEffect, "start outcome is unknown"),
			wantChunk: true,
		},
		{
			name:      "start unknown",
			result:    kernelengine.StartResult{Outcome: kernelengine.StartUnknown},
			wantChunk: true,
		},
		{
			name:      "start proven not started",
			result:    kernelengine.StartResult{Outcome: kernelengine.StartProvenNotStarted},
			wantChunk: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
				0: {
					Chunks: []kernelengine.LogChunk{{
						Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("live\n"),
					}},
					NextOffset: 5,
				},
			})
			live := &recordingLivePublisher{}
			fixture.collector.live = live
			fixture.collector.liveInterval = 2 * time.Millisecond
			fixture.collector.inner = &startOutcomeRuntime{
				logPageRuntime: fixture.runtime,
				result:         testCase.result,
				err:            testCase.err,
			}
			t.Cleanup(func() { _ = fixture.collector.Close(context.Background()) })

			_, _ = fixture.collector.Start(context.Background(), kernelengine.StartRequest{
				Identity:    fixture.removeRequest().Identity,
				WorkspaceID: fixture.workspaceID,
			})
			if testCase.wantChunk {
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if chunks, _ := live.snapshot(); len(chunks) > 0 {
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
				t.Fatalf("no live chunk was published for a possibly-applied start (%s)", testCase.name)
			}
			time.Sleep(40 * time.Millisecond)
			if chunks, _ := live.snapshot(); len(chunks) != 0 {
				t.Fatalf("published %d live chunks for a proven-not-started start: %+v", len(chunks), chunks)
			}
		})
	}
}

// TestNextLiveBackoffDoublesAndCaps pins the idle/failure poll backoff: the
// delay doubles from the configured cadence, never exceeds the ceiling and is
// never shorter than the configured cadence.
func TestNextLiveBackoffDoublesAndCaps(t *testing.T) {
	base := 10 * time.Millisecond
	delay := base
	wantSteps := []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
	for index, want := range wantSteps {
		delay = nextLiveBackoff(delay, base)
		if delay != want {
			t.Fatalf("backoff step %d = %v; want %v", index, delay, want)
		}
	}
	for range 20 {
		delay = nextLiveBackoff(delay, base)
	}
	if delay != logLiveBackoffCeiling {
		t.Fatalf("backoff after many idle polls = %v; want the %v ceiling", delay, logLiveBackoffCeiling)
	}
	if got := nextLiveBackoff(time.Second, 3*time.Second); got != 3*time.Second {
		t.Fatalf("backoff with a cadence above the ceiling = %v; want the 3s cadence", got)
	}
}

// TestLiveLogPumpPublishesNothingWithoutScope proves a failed or invalid
// execution scope never falls back to an argument-supplied or empty scope: the
// pump stays silent instead of publishing cross-tenant or scope-less progress.
func TestLiveLogPumpPublishesNothingWithoutScope(t *testing.T) {
	fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
		0: {
			Chunks: []kernelengine.LogChunk{{
				Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("secret-scope\n"),
			}},
			NextOffset: 13,
		},
	})
	live := &recordingLivePublisher{}
	fixture.collector.live = live
	fixture.collector.liveInterval = 2 * time.Millisecond
	t.Cleanup(func() { _ = fixture.collector.Close(context.Background()) })

	cases := []struct {
		name  string
		scope *fakeExecutionScopes
	}{
		{
			name: "scope lookup fails",
			scope: &fakeExecutionScopes{
				scopes: map[id.ExecutionID]tenancy.Scope{},
				err:    fault.New(fault.DependencyUnavailable, "execution scope is unavailable"),
			},
		},
		{
			name: "scope is invalid",
			scope: &fakeExecutionScopes{
				scopes: map[id.ExecutionID]tenancy.Scope{fixture.executionID: {TenantID: ""}},
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture.collector.liveMu.Lock()
			fixture.collector.scopes = testCase.scope
			fixture.collector.liveMu.Unlock()
			identity := fixture.removeRequest().Identity
			if _, err := fixture.collector.Start(context.Background(), kernelengine.StartRequest{
				Identity:    identity,
				WorkspaceID: fixture.workspaceID,
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			time.Sleep(40 * time.Millisecond)
			if chunks, _ := live.snapshot(); len(chunks) != 0 {
				t.Fatalf("published %d live chunks without a valid scope: %+v", len(chunks), chunks)
			}
			fixture.collector.stopLiveLogs(context.Background(), identity)
		})
	}
}

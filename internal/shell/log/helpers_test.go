package log_test

// Deterministic retained/live log fixtures. Every sink writes under
// t.TempDir; clocks and downstream publishers are fakes.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func testScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

func testChunk(execution, action string, stream log.Stream, offset, sequence uint64, data string) log.Chunk {
	return log.Chunk{
		ExecutionID: id.ExecutionID(execution),
		ActionID:    id.ActionID(action),
		Stream:      stream,
		Offset:      offset,
		Sequence:    sequence,
		Data:        []byte(data),
	}
}

func testReadRequest(scope tenancy.Scope, execution, action string, stream log.Stream, after uint64, limit int) log.ReadRequest {
	return log.ReadRequest{
		Scope:       scope,
		ExecutionID: id.ExecutionID(execution),
		ActionID:    id.ActionID(action),
		Stream:      stream,
		AfterOffset: after,
		Limit:       limit,
	}
}

func newFileSink(t *testing.T, config log.SinkConfig) *log.FileSink {
	t.Helper()
	sink, err := log.NewFileSink(config)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	if sink == nil {
		t.Fatal("NewFileSink returned nil")
	}
	return sink
}

func defaultSinkConfig(t *testing.T, clock *fakeClock) log.SinkConfig {
	t.Helper()
	return log.SinkConfig{
		Root:      t.TempDir(),
		Clock:     clock,
		Retention: log.DefaultRetention(),
		Limits:    log.DefaultLimits(),
	}
}

func requireClass(t *testing.T, err error, class fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %q", class)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v has no fault class; want %q", err, class)
	}
	if got != class {
		t.Fatalf("error class = %q (%v); want %q", got, err, class)
	}
}

func concatChunks(chunks []log.Chunk) string {
	var buffer []byte
	for _, chunk := range chunks {
		buffer = append(buffer, chunk.Data...)
	}
	return string(buffer)
}

type recordingPublisher struct {
	mu     sync.Mutex
	calls  []log.Chunk
	scopes []tenancy.Scope
	err    error
}

func (publisher *recordingPublisher) PublishLive(_ context.Context, scope tenancy.Scope, chunk log.Chunk) error {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	cloned := chunk
	cloned.Data = append([]byte(nil), chunk.Data...)
	publisher.calls = append(publisher.calls, cloned)
	publisher.scopes = append(publisher.scopes, scope)
	return publisher.err
}

func (publisher *recordingPublisher) snapshot() []log.Chunk {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	calls := make([]log.Chunk, len(publisher.calls))
	copy(calls, publisher.calls)
	return calls
}

type blockingPublisher struct {
	mu        sync.Mutex
	calls     []log.Chunk
	scopes    []tenancy.Scope
	started   chan struct{}
	release   chan struct{}
	failFirst bool
}

func newBlockingPublisher() *blockingPublisher {
	return &blockingPublisher{
		started: make(chan struct{}, 4096),
		release: make(chan struct{}),
	}
}

func (publisher *blockingPublisher) PublishLive(ctx context.Context, scope tenancy.Scope, chunk log.Chunk) error {
	publisher.mu.Lock()
	cloned := chunk
	cloned.Data = append([]byte(nil), chunk.Data...)
	publisher.calls = append(publisher.calls, cloned)
	publisher.scopes = append(publisher.scopes, scope)
	index := len(publisher.calls)
	publisher.mu.Unlock()

	if publisher.failFirst && index == 1 {
		return fault.New(fault.DependencyUnavailable, "broker unavailable")
	}
	select {
	case publisher.started <- struct{}{}:
	default:
	}
	select {
	case <-publisher.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (publisher *blockingPublisher) releaseAll() {
	close(publisher.release)
}

func (publisher *blockingPublisher) callSnapshot() []log.Chunk {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	calls := make([]log.Chunk, len(publisher.calls))
	copy(calls, publisher.calls)
	return calls
}

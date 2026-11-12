//go:build integration

package composition

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	natsbroker "github.com/optimiware/kodama/internal/shell/integration/nats"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	messaging "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

const (
	// compositionBrokerName names the real adapter connection, so the test can
	// observe its server-side subscriptions.
	compositionBrokerName = "composition-live-log"
	// invalidationAttempts bounds how often the invalidation is republished
	// while the adapter re-establishes its scope watcher, and
	// invalidationCloseWindow is the bounded wait for the fail-closed signal on
	// each attempt.
	invalidationAttempts    = 3
	invalidationCloseWindow = 10 * time.Second
)

// liveLogRuntime stays running and serves at most one small stdout page per
// read once output is enabled. It lets the test subscribe first, then release
// output while the action is still running.
type liveLogRuntime struct {
	*fakeRuntime

	mu      sync.Mutex
	enabled bool
	data    []byte
	maxPage int
}

func newLiveLogRuntime() *liveLogRuntime {
	return &liveLogRuntime{fakeRuntime: newFakeRuntime(modeStayRunning), maxPage: 8}
}

func (runtime *liveLogRuntime) Logs(_ context.Context, request kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.enabled || request.AfterOffset >= int64(len(runtime.data)) {
		return kernelengine.LogBatch{NextOffset: request.AfterOffset}, nil
	}
	end := min(request.AfterOffset+int64(runtime.maxPage), int64(len(runtime.data)))
	return kernelengine.LogBatch{
		Chunks: []kernelengine.LogChunk{{
			Stream:   kernelengine.LogStdout,
			Offset:   request.AfterOffset,
			Sequence: 1,
			Data:     append([]byte(nil), runtime.data[request.AfterOffset:end]...),
		}},
		NextOffset: end,
	}, nil
}

func (runtime *liveLogRuntime) enableOutput(data string) {
	runtime.mu.Lock()
	runtime.enabled = true
	runtime.data = []byte(data)
	runtime.mu.Unlock()
}

// appendOutput adds output after the position the pump has already consumed,
// so a stream that is still open would publish it as a new offset.
func (runtime *liveLogRuntime) appendOutput(data string) {
	runtime.mu.Lock()
	runtime.enabled = true
	runtime.data = append(runtime.data, data...)
	runtime.mu.Unlock()
}

// newCompositionNATSBroker builds the real NATS adapter the composition will
// use as its event publisher, invalidation port and live log downstream.
func newCompositionNATSBroker(t *testing.T, instance *messaging.NATS) *natsbroker.Broker {
	t.Helper()
	adapter, err := natsbroker.NewBroker(natsbroker.BrokerConfig{
		Connect: func(ctx context.Context) (natsbroker.Client, error) {
			return natsdriver.Dial(ctx, instance.DriverConfig(compositionBrokerName))
		},
		Clock:               messaging.RealClock{},
		Principal:           messaging.NewDirectory(),
		Authorizer:          messaging.NewDirectory(),
		Sink:                messaging.NoopSink{},
		MaxReconnectBackoff: time.Second,
		IdleValidation:      time.Hour,
		MaxPublishAttempts:  2,
	})
	if err != nil {
		t.Fatalf("build the real NATS broker adapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	return adapter
}

// TestLiveLogSubscriptionReceivesWhileActionRuns is the real-composition
// regression: the durable live publisher path is composed over a real NATS
// broker, a subscriber opens the documented scope-qualified subject, and
// bounded chunks arrive with the execution/action identity and monotonic
// offsets while the action is still running. An invalidation for the scope then
// closes the live stream fail-closed.
func TestLiveLogSubscriptionReceivesWhileActionRuns(t *testing.T) {
	instance := messaging.StartNATS(t)
	instance.EnsureStreams(t)
	adapter := newCompositionNATSBroker(t, instance)

	harness := newHarness(t)
	harness.config.Adapters.Publisher = adapter
	harness.config.Adapters.Invalidator = adapter
	harness.config.Adapters.PublisherCloser = adapter.Close
	runtime := newLiveLogRuntime()
	environment := harness.start(runtime)
	environment.grantEngineer("admin-1", projectID)
	pipelineID := environment.applyPipeline("live-log")

	started := environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "live log task",
		PipelineID: pipelineID,
	}, nil, "live-log-task")
	var task api.TaskCreateAndStartResult
	environment.decode(started, &task)
	environment.eventually(10*time.Second, func() bool { return runtime.startCount() == 1 })

	// The action is running before the scope is inspected and before output is
	// enabled: the live subscription is opened against the running action.
	inspected := environment.mustInvoke(projectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: task.ExecutionID,
	}, nil, "")
	var execution api.ExecutionInspectResult
	environment.decode(inspected, &execution)
	activeAction := ""
	for _, action := range execution.Actions {
		if action.State == "running" || action.State == "pending" {
			activeAction = action.ActionID
			break
		}
	}
	if activeAction == "" {
		t.Fatalf("execution inspection has no active action: %+v", execution.Actions)
	}

	scope := tenancy.Scope{TenantID: id.TenantID(tenantID), ProjectID: id.ProjectID(projectID)}
	live, err := adapter.SubscribeLive(context.Background(), scope, id.ExecutionID(task.ExecutionID))
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	t.Cleanup(func() { _ = live.Close(context.Background()) })

	runtime.enableOutput("page-001page-002")

	first := nextLiveChunk(t, live)
	second := nextLiveChunk(t, live)
	if first.ExecutionID != id.ExecutionID(task.ExecutionID) || first.ActionID != id.ActionID(activeAction) {
		t.Fatalf("first chunk identity = %s/%s; want %s/%s", first.ExecutionID, first.ActionID, task.ExecutionID, activeAction)
	}
	if second.ExecutionID != first.ExecutionID || second.ActionID != first.ActionID {
		t.Fatalf("second chunk identity = %s/%s; want %s/%s", second.ExecutionID, second.ActionID, first.ExecutionID, first.ActionID)
	}
	if first.Offset != 0 || string(first.Data) != "page-001" {
		t.Fatalf("first chunk = %+v; want offset 0 page-001", first)
	}
	if second.Offset <= first.Offset || string(second.Data) != "page-002" {
		t.Fatalf("second chunk = %+v; want a later offset and page-002", second)
	}
	if runtime.stopCount() != 0 {
		t.Fatalf("fake runtime stops = %d; the action was not still running", runtime.stopCount())
	}
	if strings.Contains(string(first.Data)+string(second.Data), projectToken) {
		t.Fatal("live payload rendered the acting credential")
	}

	// A committed revocation for the scope closes the live stream: no live
	// chunk is delivered after the invalidation is observed. Wait for the close
	// signal itself, so a subscriber that simply never closes cannot satisfy
	// this test with a timeout. Removing the invalidation handler would leave
	// Next blocked here until the deadline, which is a failure.
	//
	// SubscribeLive returns once the adapter has opened both client
	// subscriptions, but the server may not have registered the scope watcher
	// yet; a reconnect also re-establishes it on a fresh connection, and an
	// invalidation published in that gap is never redelivered. So each attempt
	// first waits for the server-observed watcher and then republishes a fresh
	// invalidation (a distinct payload, so JetStream deduplication cannot
	// suppress the retry) inside a longer bounded close window.
	invalidationSubject, err := natsbroker.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	var closed error
	for attempt := 1; attempt <= invalidationAttempts; attempt++ {
		instance.WaitForSubscription(t, compositionBrokerName, string(invalidationSubject))
		invalidation := messaging.ServiceInvalidation(scope, id.PrincipalID("service-live"),
			"service-live", token.InvalidationRevoked, time.Now().UTC())
		if err := adapter.Invalidate(context.Background(), invalidation); err != nil {
			t.Fatalf("Invalidate: %v", err)
		}
		closedCtx, closedCancel := context.WithTimeout(context.Background(), invalidationCloseWindow)
		_, closed = live.Next(closedCtx)
		closedCancel()
		if closed == nil {
			t.Fatal("live subscription delivered after a scoped invalidation")
		}
		if !errors.Is(closed, context.DeadlineExceeded) {
			break
		}
	}
	if errors.Is(closed, context.DeadlineExceeded) {
		t.Fatalf("live subscription did not close on invalidation after %d bounded attempts: %v", invalidationAttempts, closed)
	}
	if class, ok := fault.ClassOf(closed); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("live subscription closed with %v (class %v); want a dependency-unavailable closure", closed, class)
	}

	// New output exists after the observed close. A still-open subscriber would
	// publish it at a fresh offset, so a chunk here is a fail-closed violation.
	runtime.appendOutput("page-003page-004")
	afterCtx, afterCancel := context.WithTimeout(context.Background(), time.Second)
	defer afterCancel()
	chunk, err := live.Next(afterCtx)
	if err == nil {
		t.Fatalf("live subscription delivered %q after a scoped invalidation", chunk.Data)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live subscription stopped reporting closure after invalidation: %v", err)
	}
}

// nextLiveChunk reads one live chunk with a bounded wait.
func nextLiveChunk(t *testing.T, live shelllog.LiveSubscription) shelllog.Chunk {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chunk, err := live.Next(ctx)
	if err != nil {
		t.Fatalf("live Next: %v", err)
	}
	return chunk
}

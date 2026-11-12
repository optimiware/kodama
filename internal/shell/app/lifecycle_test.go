package app

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
)

func TestPartialStartCleanupStopsInReverseOrder(t *testing.T) {
	environment := newTestEnvironment(t)
	var (
		mu    sync.Mutex
		stops []string
	)
	environment.config.Adapters.RuntimeCloser = func(context.Context) error {
		mu.Lock()
		stops = append(stops, "runtime-driver")
		mu.Unlock()
		return nil
	}
	environment.config.Adapters.PublisherCloser = func(context.Context) error {
		mu.Lock()
		stops = append(stops, "broker")
		mu.Unlock()
		return nil
	}
	environment.config.hooks = &startupHook{failAt: "router"}

	baseline := runtime.NumGoroutine()
	if _, err := New(context.Background(), environment.config); err == nil {
		t.Fatal("New succeeded despite the injected startup failure")
	}
	mu.Lock()
	got := append([]string(nil), stops...)
	mu.Unlock()
	want := []string{"broker", "runtime-driver"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("partial-start stop order = %v; want %v", got, want)
	}
	eventually(t, time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline+1
	})
}

func TestConcurrentShutdownIsIdempotentAndJoins(t *testing.T) {
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	var wait sync.WaitGroup
	errs := make([]error, 16)
	for index := range errs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs[index] = app.Close(ctx)
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("Close[%d] = %v", index, err)
		}
	}
	if err := app.Wait(); err != nil {
		t.Fatalf("Wait = %v", err)
	}
}

// stuckRuntime keeps every workload running so shutdown has a live workload to
// stop through the runtime adapter.
type stuckRuntime struct {
	*fakeRuntime
}

func (runtime *stuckRuntime) Observe(context.Context, kernelengine.ObserveRequest) (kernel.Observation, error) {
	return kernel.RunningObservation(), nil
}

// TestShutdownStopsWorkloadsBeforeClosingDriver pins the process-runtime half
// of the shutdown policy: a development host process is stopped before its
// driver closes. The durable half is asserted by
// TestShutdownStopsOnlyProcessOwnedWorkloads in controller_restart_test.go.
func TestShutdownStopsWorkloadsBeforeClosingDriver(t *testing.T) {
	environment := newTestEnvironment(t)
	stuck := &stuckRuntime{fakeRuntime: environment.runtime}
	environment.config.Adapters.Runtime = stuck

	var (
		mu    sync.Mutex
		order []string
	)
	environment.config.Adapters.RuntimeCloser = func(context.Context) error {
		mu.Lock()
		order = append(order, "close")
		mu.Unlock()
		return nil
	}
	app := environment.startApp(t)
	handler := app.Handler()
	token := "dev-project-token"
	grantProjectEngineer(t, handler, "dev-admin-token")

	// Apply a pipeline and start one task so a workload becomes active.
	recorder, result, publicError := restCall(t, handler, "PUT", "/api/v1/projects/project-1/pipelines/build", token,
		api.PipelineApplyRequest{Name: "build", YAML: []byte(smokePipeline)}, nil, "apply-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("pipeline.apply status=%d error=%+v", recorder.Code, publicError)
	}
	applied := decodeBody[api.PipelineApplyResult](t, result)
	recorder, _, publicError = restCall(t, handler, "POST", "/api/v1/projects/project-1/tasks/create-and-start", token,
		api.TaskCreateAndStartRequest{Title: "stuck", PipelineID: applied.PipelineID}, nil, "start-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	eventually(t, 5*time.Second, func() bool {
		return stuck.startCount() > 0
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) == 0 || got[len(got)-1] != "close" {
		t.Fatalf("driver close order = %v; want stop before close", got)
	}
	if got := stuck.stopCount(); got != 1 {
		t.Fatalf("shutdown issued %d workload stop(s); want exactly 1", got)
	}
}

// activeCount reports the workloads the composition still owns.
func (runtime *trackedRuntime) activeCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.active)
}

// TestTrackedRuntimeLeavesDurableWorkloadsOnClose proves the durable half at the
// smallest unit: stopAll sends no Stop and keeps the workload tracked so the
// next controller can adopt the same stable identity.
func TestTrackedRuntimeLeavesDurableWorkloadsOnClose(t *testing.T) {
	inner := &fakeRuntime{}
	tracked, err := newTrackedRuntime(inner, leaveWorkloadsOnClose)
	if err != nil {
		t.Fatalf("newTrackedRuntime: %v", err)
	}
	request := kernelengine.StartRequest{
		Identity: kernelengine.EffectIdentity{
			ExecutionID: "execution-1",
			ActionID:    "action-1",
			WorkloadID:  "workload-1",
		},
		WorkspaceID: "workspace-1",
	}
	if _, err := tracked.Start(context.Background(), request); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tracked.stopAll(context.Background()); err != nil {
		t.Fatalf("stopAll: %v", err)
	}
	if got := inner.stopCount(); got != 0 {
		t.Fatalf("durable stopAll issued %d stop(s); want 0", got)
	}
	if got := tracked.activeCount(); got != 1 {
		t.Fatalf("durable stopAll forgot the workload: active = %d; want 1", got)
	}
}

func TestTrackedRuntimeStopsEveryActiveWorkload(t *testing.T) {
	inner := &fakeRuntime{}
	tracked, err := newTrackedRuntime(inner, stopWorkloadsOnClose)
	if err != nil {
		t.Fatalf("newTrackedRuntime: %v", err)
	}
	request := kernelengine.StartRequest{
		Identity: kernelengine.EffectIdentity{
			ExecutionID: "execution-1",
			ActionID:    "action-1",
			WorkloadID:  "workload-1",
		},
		WorkspaceID: "workspace-1",
	}
	if _, err := tracked.Start(context.Background(), request); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tracked.activeCount() != 1 {
		t.Fatalf("active workloads = %d; want 1", tracked.activeCount())
	}
	if err := tracked.stopAll(context.Background()); err != nil {
		t.Fatalf("stopAll: %v", err)
	}
	if tracked.activeCount() != 0 {
		t.Fatalf("active workloads after stopAll = %d; want 0", tracked.activeCount())
	}
	inner.mu.Lock()
	stopped := append([]id.WorkloadID(nil), inner.stopped...)
	inner.mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "workload-1" {
		t.Fatalf("stopped workloads = %v", stopped)
	}
}

// cancellingRuntime cancels the caller's shutdown context on the first stop so
// stopAll can observe a cancellation between two tracked workloads.
type cancellingRuntime struct {
	*fakeRuntime
	cancel context.CancelFunc
}

func (runtime *cancellingRuntime) Stop(ctx context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	runtime.cancel()
	return runtime.fakeRuntime.Stop(ctx, request)
}

// TestCloseSurfacesRuntimeStopFailure proves a shutdown that cannot stop every
// tracked workload is reported instead of being swallowed.
func TestCloseSurfacesRuntimeStopFailure(t *testing.T) {
	environment := newTestEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	environment.config.Adapters.Runtime = &cancellingRuntime{fakeRuntime: environment.runtime, cancel: cancel}
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, workload := range []id.WorkloadID{"workload-1", "workload-2"} {
		request := kernelengine.StartRequest{
			Identity: kernelengine.EffectIdentity{
				ExecutionID: id.ExecutionID("execution-" + string(workload)),
				ActionID:    "action-1",
				WorkloadID:  workload,
			},
			WorkspaceID: "workspace-1",
		}
		if _, err := app.runtime.Start(context.Background(), request); err != nil {
			t.Fatalf("track %s: %v", workload, err)
		}
	}
	if err := app.Close(ctx); err == nil {
		t.Fatal("Close reported success although a tracked workload was not stopped")
	}
}

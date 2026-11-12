package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/installation"
)

// These tests pin controller-restart behavior: a graceful controller shutdown
// that stopped every workload the composition had started would delete a
// Kubernetes Job pod (exit 137) when the controller pod receives SIGTERM.
// Stop-on-close is only correct for the process runtime, whose
// workloads are host processes that die with the controller anyway. Podman
// containers and Kubernetes Jobs outlive the controller and are stopped only
// through persisted cancellation/deadline/terminal reconciliation.

// durableCompositionConfig builds a fully injected test composition declared as
// one durable runtime. It reuses the deterministic development composition input
// but declares the runtime kind the policy must consult, so App.Close can be
// exercised without a real cluster or container runtime.
func durableCompositionConfig(t *testing.T, runtime installation.RuntimeAdapter) Config {
	t.Helper()
	config := fullTestProfileConfig(t)
	config.Installation.Runtime = runtime
	return config
}

// startStuckTask applies the smoke pipeline and starts one task so the runtime
// has accepted a live workload.
func startStuckTask(t *testing.T, app *App) api.TaskCreateAndStartResult {
	t.Helper()
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	recorder, result, publicError := restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/pipelines/build", "dev-project-token",
		api.PipelineApplyRequest{Name: "build", YAML: []byte(smokePipeline)}, nil, "apply-restart")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("pipeline.apply status=%d error=%+v", recorder.Code, publicError)
	}
	applied := decodeBody[api.PipelineApplyResult](t, result)
	recorder, result, publicError = restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", "dev-project-token",
		api.TaskCreateAndStartRequest{Title: "restart", PipelineID: applied.PipelineID}, nil, "start-restart")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	return decodeBody[api.TaskCreateAndStartResult](t, result)
}

// TestShutdownStopsOnlyProcessOwnedWorkloads is the unit-level check:
// a durable-runtime composition must close its loops and dependencies without
// stopping a live workload, while the process runtime keeps its stop-on-close
// behavior because those workloads die with the controller process.
func TestShutdownStopsOnlyProcessOwnedWorkloads(t *testing.T) {
	cases := []struct {
		name      string
		runtime   installation.RuntimeAdapter
		wantStops int
	}{
		{name: "process", runtime: installation.RuntimeProcess, wantStops: 1},
		{name: "podman", runtime: installation.RuntimePodman, wantStops: 0},
		{name: "kubernetes", runtime: installation.RuntimeKubernetes, wantStops: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := durableCompositionConfig(t, testCase.runtime)
			stuck := &stuckRuntime{fakeRuntime: config.Adapters.Runtime.(*fakeRuntime)}
			config.Adapters.Runtime = stuck

			var driverCloses int
			config.Adapters.RuntimeCloser = func(context.Context) error {
				driverCloses++
				return nil
			}
			app, err := New(context.Background(), config)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := app.start(context.Background()); err != nil {
				t.Fatalf("start: %v", err)
			}
			startStuckTask(t, app)
			eventually(t, 5*time.Second, func() bool { return stuck.startCount() > 0 })

			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := app.Close(closeCtx); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if got := stuck.stopCount(); got != testCase.wantStops {
				t.Fatalf("controller shutdown issued %d workload stop(s) for the %s runtime; want %d",
					got, testCase.runtime, testCase.wantStops)
			}
			if got := stuck.removeCount(); got != 0 {
				t.Fatalf("controller shutdown issued %d workload removal(s) for the %s runtime; want 0",
					got, testCase.runtime)
			}
			if driverCloses != 1 {
				t.Fatalf("runtime driver close calls = %d; want 1", driverCloses)
			}
		})
	}
}

// TestCancellationStillStopsDurableRuntimeWorkload proves the explicit stop
// control is untouched: durable workloads are not stopped by controller
// shutdown, but an execution cancellation still commits a stop intent and the
// Kernel stops the running workload.
func TestCancellationStillStopsDurableRuntimeWorkload(t *testing.T) {
	config := durableCompositionConfig(t, installation.RuntimeKubernetes)
	stuck := &stuckRuntime{fakeRuntime: config.Adapters.Runtime.(*fakeRuntime)}
	config.Adapters.Runtime = stuck

	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Close(closeCtx)
	})

	started := startStuckTask(t, app)
	eventually(t, 5*time.Second, func() bool { return stuck.startCount() > 0 })
	if got := stuck.stopCount(); got != 0 {
		t.Fatalf("starting a workload issued %d stop(s); want 0", got)
	}

	cancelExecution(t, app.Handler(), started.ExecutionID)
	eventually(t, 5*time.Second, func() bool { return stuck.stopCount() > 0 })
}

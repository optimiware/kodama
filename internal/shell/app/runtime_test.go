package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
)

type trackedStubRuntime struct {
	mu          sync.Mutex
	startResult kernelengine.StartResult
	startErr    error
	stopResult  kernel.StopEvidence
	stopErr     error
	stopped     []kernelengine.StopRequest
}

func (stub *trackedStubRuntime) Capabilities(context.Context) (kernelengine.Capabilities, error) {
	return kernelengine.Capabilities{}, nil
}

func (stub *trackedStubRuntime) Prepare(_ context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	return kernelengine.Preparation{WorkspaceID: request.WorkspaceID}, nil
}

func (stub *trackedStubRuntime) Start(context.Context, kernelengine.StartRequest) (kernelengine.StartResult, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.startResult, stub.startErr
}

func (stub *trackedStubRuntime) Observe(context.Context, kernelengine.ObserveRequest) (kernel.Observation, error) {
	return kernel.UnknownObservation(), nil
}

func (stub *trackedStubRuntime) Logs(context.Context, kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	return kernelengine.LogBatch{}, nil
}

func (stub *trackedStubRuntime) Stop(_ context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.stopped = append(stub.stopped, request)
	return stub.stopResult, stub.stopErr
}

func (stub *trackedStubRuntime) Remove(context.Context, kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	return kernelengine.Removal{}, nil
}

func (stub *trackedStubRuntime) RefreshCredential(context.Context, kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	return kernelengine.CredentialRefresh{}, nil
}

func TestTrackedRuntimeRemembersUncertainAndErroredStarts(t *testing.T) {
	cases := []struct {
		name         string
		requestID    id.WorkloadID
		result       kernelengine.StartResult
		startErr     error
		wantRemember bool
		wantWorkload id.WorkloadID
	}{
		{
			name:      "unknown outcome with uncertain error uses result workload id",
			requestID: "workload-req",
			result: kernelengine.StartResult{
				Outcome:    kernelengine.StartUnknown,
				WorkloadID: "workload-res",
			},
			startErr:     fault.New(fault.UncertainExternalEffect, "lost start response"),
			wantRemember: true,
			wantWorkload: "workload-res",
		},
		{
			name:         "error with empty result workload id falls back to request identity",
			requestID:    "workload-fallback",
			result:       kernelengine.StartResult{},
			startErr:     errors.New("transport reset"),
			wantRemember: true,
			wantWorkload: "workload-fallback",
		},
		{
			name:      "result workload id normalizes empty request workload id",
			requestID: "",
			result: kernelengine.StartResult{
				Outcome:    kernelengine.StartAccepted,
				WorkloadID: "workload-assigned",
			},
			wantRemember: true,
			wantWorkload: "workload-assigned",
		},
		{
			name:      "affirmative proven not started without error is not tracked",
			requestID: "workload-refused",
			result: kernelengine.StartResult{
				Outcome:    kernelengine.StartProvenNotStarted,
				WorkloadID: "workload-refused",
			},
			wantRemember: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &trackedStubRuntime{
				startResult: tc.result,
				startErr:    tc.startErr,
				stopResult:  kernel.ConfirmedStop("terminated"),
			}
			tracked, err := newTrackedRuntime(stub, stopWorkloadsOnClose)
			if err != nil {
				t.Fatalf("newTrackedRuntime: %v", err)
			}
			_, gotErr := tracked.Start(context.Background(), kernelengine.StartRequest{
				Identity: kernelengine.EffectIdentity{
					ExecutionID:    "exec-1",
					ActionID:       "action-1",
					WorkloadID:     tc.requestID,
					IdempotencyKey: "idem-1",
				},
				WorkspaceID: "workspace-1",
			})
			if !errors.Is(gotErr, tc.startErr) {
				t.Fatalf("Start error = %v; want %v", gotErr, tc.startErr)
			}

			if err := tracked.stopAll(context.Background()); err != nil {
				t.Fatalf("stopAll: %v", err)
			}

			stub.mu.Lock()
			stopped := append([]kernelengine.StopRequest(nil), stub.stopped...)
			stub.mu.Unlock()

			if !tc.wantRemember {
				if len(stopped) != 0 {
					t.Fatalf("stopAll stopped %+v; want none", stopped)
				}
				return
			}
			if len(stopped) != 1 {
				t.Fatalf("stopAll issued %d stops; want 1", len(stopped))
			}
			if stopped[0].Identity.WorkloadID != tc.wantWorkload {
				t.Fatalf("stopAll workload id = %q; want %q", stopped[0].Identity.WorkloadID, tc.wantWorkload)
			}
			tracked.mu.Lock()
			remaining := len(tracked.active)
			tracked.mu.Unlock()
			if remaining != 0 {
				t.Fatalf("tracked active entries after confirmed stopAll = %d; want 0", remaining)
			}
		})
	}
}

package app

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
)

// closePolicy records what a graceful composition close does with the workloads
// the tracked runtime still owns. It is derived from the declared runtime kind,
// never from the adapter's concrete type, so a test fake cannot silently change
// the lifetime contract of the runtime it stands in for.
type closePolicy uint8

const (
	// leaveWorkloadsOnClose is the policy for every durable runtime (Podman and
	// Kubernetes). Those workloads are owned by durable infrastructure, not by
	// the controller process: a controller restart observes and adopts the same
	// stable workload identity. Stopping one on close would kill a running
	// command (the Job pod would exit 137 when the controller pod
	// receives SIGTERM). Their stop is driven only by persisted cancellation,
	// deadline or terminal reconciliation through the Kernel.
	leaveWorkloadsOnClose closePolicy = iota
	// stopWorkloadsOnClose is the policy for the process runtime. Its workloads
	// are host processes that die with the controller process anyway, so a
	// graceful close stops them instead of leaking them while the process lives.
	stopWorkloadsOnClose
)

// closePolicyForRuntime selects the shutdown policy from the declared runtime
// kind. An empty or unknown declaration leaves workloads running, the safe
// direction: a composition never kills a workload whose lifetime it does not
// own.
func closePolicyForRuntime(adapter installation.RuntimeAdapter) closePolicy {
	if adapter == installation.RuntimeProcess {
		return stopWorkloadsOnClose
	}
	return leaveWorkloadsOnClose
}

// trackedRuntime wraps the Kernel runtime port so the composition can stop the
// workloads it owns when (and only when) the declared runtime kind makes the
// controller process their lifetime owner. It records a workload when a start is
// accepted or its outcome is unknown (an unknown effect may exist) and forgets
// it only after confirmed stop or removal. It adds no runtime policy beyond
// that close policy.
type trackedRuntime struct {
	inner kernelengine.Runtime
	close closePolicy

	// activation resolves declared secret values and the committed execution
	// credential into one PrepareRequest before the runtime adapter is called.
	// A nil materializer (a bare tracked wrapper) delegates unchanged.
	activation *activationMaterializer

	mu     sync.Mutex
	active map[id.WorkloadID]kernelengine.StopRequest
}

var _ kernelengine.Runtime = (*trackedRuntime)(nil)

func newTrackedRuntime(inner kernelengine.Runtime, policy closePolicy) (*trackedRuntime, error) {
	if inner == nil {
		return nil, fault.New(fault.InvalidInput, "tracked runtime requires a runtime adapter")
	}
	return &trackedRuntime{
		inner:  inner,
		close:  policy,
		active: make(map[id.WorkloadID]kernelengine.StopRequest),
	}, nil
}

func (runtime *trackedRuntime) Capabilities(ctx context.Context) (kernelengine.Capabilities, error) {
	return runtime.inner.Capabilities(ctx)
}

func (runtime *trackedRuntime) Prepare(ctx context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	if runtime.activation != nil {
		prepared, err := runtime.activation.prepare(ctx, request)
		if err != nil {
			return kernelengine.Preparation{}, err
		}
		request = prepared
	}
	prepared, err := runtime.inner.Prepare(ctx, request)
	if err == nil && runtime.activation != nil {
		// A successful preparation has no following failure commit to rebase, so
		// the composition no longer needs the credential-commit revision.
		runtime.activation.forgetIssuedVersion(request.Identity.ExecutionID)
	}
	return prepared, err
}

func (runtime *trackedRuntime) Start(ctx context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	result, err := runtime.inner.Start(ctx, request)
	if err == nil && result.Outcome == kernelengine.StartProvenNotStarted {
		return result, nil
	}
	workload := result.WorkloadID
	if workload == "" {
		workload = request.Identity.WorkloadID
	}
	if workload != "" {
		runtime.remember(workload, request)
	}
	return result, err
}

func (runtime *trackedRuntime) Observe(ctx context.Context, request kernelengine.ObserveRequest) (kernel.Observation, error) {
	return runtime.inner.Observe(ctx, request)
}

func (runtime *trackedRuntime) Logs(ctx context.Context, request kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	return runtime.inner.Logs(ctx, request)
}

func (runtime *trackedRuntime) Stop(ctx context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	evidence, err := runtime.inner.Stop(ctx, request)
	if err == nil && evidence.IsConfirmed() {
		runtime.forget(request.Identity.WorkloadID)
	}
	return evidence, err
}

func (runtime *trackedRuntime) Remove(ctx context.Context, request kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	removal, err := runtime.inner.Remove(ctx, request)
	if err == nil && removal.Removed {
		runtime.forget(request.Identity.WorkloadID)
	}
	return removal, err
}

func (runtime *trackedRuntime) RefreshCredential(ctx context.Context, request kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	return runtime.inner.RefreshCredential(ctx, request)
}

// stopAll stops every workload this composition still owns when the declared
// runtime kind makes the controller process their lifetime owner. It is called
// after the background loops are joined and before the runtime driver closes.
//
// For a durable runtime it deliberately does nothing: the workload keeps
// running so the next controller can observe and adopt its stable identity. The
// Kernel's persisted cancellation/deadline/terminal reconciliation owns that
// stop, so shutdown must never turn a live command into a terminal failure.
func (runtime *trackedRuntime) stopAll(ctx context.Context) error {
	if runtime.close != stopWorkloadsOnClose {
		return nil
	}
	runtime.mu.Lock()
	pending := make([]kernelengine.StopRequest, 0, len(runtime.active))
	for _, request := range runtime.active {
		pending = append(pending, request)
	}
	runtime.mu.Unlock()
	for _, request := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		evidence, err := runtime.inner.Stop(ctx, request)
		if err != nil {
			// The stop outcome is unknown. The workload remains tracked so the
			// driver close still waits for it instead of claiming termination.
			continue
		}
		if evidence.IsConfirmed() {
			runtime.forget(request.Identity.WorkloadID)
		}
	}
	return nil
}

func (runtime *trackedRuntime) remember(workload id.WorkloadID, request kernelengine.StartRequest) {
	request.Identity.WorkloadID = workload
	runtime.mu.Lock()
	runtime.active[workload] = kernelengine.StopRequest(request)
	runtime.mu.Unlock()
}

func (runtime *trackedRuntime) forget(workload id.WorkloadID) {
	runtime.mu.Lock()
	delete(runtime.active, workload)
	runtime.mu.Unlock()
}

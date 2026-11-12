//go:build qualification

package production

// Q02 runtime-isolation probe. It proves the deployed stage gives each tenant a
// distinct namespace with the baseline pod-security profile, a LimitRange and a
// default-deny NetworkPolicy that admits only cluster DNS and Kodama, that an
// actual workload carries bounded CPU/memory requests and limits, and that a
// live tenant-A workload cannot reach a live tenant-B listener while it can
// still reach Kodama and cannot reach the Kubernetes API.

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestStageRuntimeIsolation is Q02 probe (i). Required image: a POSIX shell with
// command -v, timeout, nc, curl, kill and sleep (the stage's pinned curl image
// provides them). The tenant-B listener fails visibly when nc is absent, so a
// missing listener can never turn the cross-tenant assertion into a pass.
func TestStageRuntimeIsolation(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config, isolationRuntimeInputs()...)
	runtime := newIsolationRuntime(t, config)
	probe := resilienceBeginProbe(t, config, "TestStageRuntimeIsolation")
	started := time.Now().UTC()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	tenants := []struct {
		name   string
		tenant isolationTenant
		other  isolationTenant
	}{
		{"tenant-a", runtime.A, runtime.B},
		{"tenant-b", runtime.B, runtime.A},
	}
	resources := make(map[string]isolationResources, len(tenants))
	for _, entry := range tenants {
		t.Run(entry.name, func(t *testing.T) {
			namespace, err := isolationReadNamespace(ctx, runtime, entry.tenant.Namespace)
			if err != nil {
				t.Fatalf("read namespace %q: %v", entry.tenant.Namespace, err)
			}
			if err := validateTenantNamespace(namespace, entry.tenant.ID, entry.tenant.Namespace, entry.other.Namespace); err != nil {
				t.Fatalf("namespace isolation: %v", err)
			}
			limits, err := isolationReadLimitRange(ctx, runtime, entry.tenant)
			if err != nil {
				t.Fatalf("read the LimitRange for tenant %q: %v", entry.tenant.ID, err)
			}
			policy, err := isolationReadNetworkPolicy(ctx, runtime, entry.tenant)
			if err != nil {
				t.Fatalf("read the NetworkPolicy for tenant %q: %v", entry.tenant.ID, err)
			}
			if err := validateTenantNetworkPolicy(policy, entry.tenant.ID, entry.tenant.Namespace, entry.other.Namespace, ReleaseNamespace); err != nil {
				t.Fatalf("network policy isolation for tenant %q: %v", entry.tenant.ID, err)
			}
			resources[entry.name] = limits
		})
	}

	nonce := resilienceNonce()
	readyMarker := "q02-net-server-ready-" + nonce
	doneMarker := "q02-net-server-done-" + nonce
	isolatedMarker := "q02-network-isolated-" + nonce

	networkImage := isolationNetworkImage(config)
	serverExecution := isolationStart(t, runtime.B.Stage, isolationNetworkServerPipeline(networkImage, readyMarker, doneMarker), "isolation-net-server")
	serverPod, err := isolationWaitPod(ctx, runtime.Kubectl, runtime.B.Namespace, isolationExecutionLabel+"="+serverExecution, 2*time.Minute)
	if err != nil {
		t.Fatalf("tenant B's network listener pod did not start: %v", err)
	}
	if err := validateIsolationPod(serverPod, runtime.B.ID, runtime.B.Namespace, resources["tenant-b"]); err != nil {
		t.Fatalf("tenant B's workload is not bounded: %v", err)
	}
	serverIP, err := isolationValidPodIP(serverPod.Status.PodIP)
	if err != nil {
		t.Fatalf("tenant B's listener pod address: %v", err)
	}
	// The action proves the listener stayed alive for one second and fails the
	// execution otherwise. A short settle keeps the client from racing startup.
	time.Sleep(2 * time.Second)
	stillRunning, err := resilienceInspect(ctx, runtime.B.Stage, serverExecution)
	if err != nil {
		t.Fatalf("inspect tenant B's listener execution: %v", err)
	}
	if terminalExecutionState(stillRunning.State) {
		t.Fatalf("tenant B's listener exited before the probe connected (state %q); the workload image needs a working nc", stillRunning.State)
	}

	kodamaService, err := resilienceKodamaService()
	if err != nil {
		t.Fatalf("%v", err)
	}
	clientExecution := isolationStart(t, runtime.A.Stage,
		isolationNetworkClientPipeline(networkImage, serverIP, kodamaService, isolatedMarker), "isolation-net-client")
	clientPod, err := isolationWaitPod(ctx, runtime.Kubectl, runtime.A.Namespace, isolationExecutionLabel+"="+clientExecution, 2*time.Minute)
	if err != nil {
		t.Fatalf("tenant A's network client pod did not start: %v", err)
	}
	if err := validateIsolationPod(clientPod, runtime.A.ID, runtime.A.Namespace, resources["tenant-a"]); err != nil {
		t.Fatalf("tenant A's workload is not bounded: %v", err)
	}

	clientFinal, _, err := resilienceWaitTerminal(ctx, runtime.A.Stage, clientExecution, 4*time.Minute)
	if err != nil {
		t.Fatalf("tenant A's client did not settle: %v", err)
	}
	if clientFinal.State != "succeeded" {
		t.Fatalf("tenant A's cross-tenant client state = %q (failure %+v); want succeeded", clientFinal.State, clientFinal.Failure)
	}
	// The client's refusal only proves isolation while the listener was still
	// serving. A listener that died before the client began makes every curl
	// fail for the wrong reason, so require tenant B's execution to still be
	// non-terminal after the client has reached its terminal state.
	stillServing, err := resilienceInspect(ctx, runtime.B.Stage, serverExecution)
	if err != nil {
		t.Fatalf("inspect tenant B's listener after the client settled: %v", err)
	}
	if terminalExecutionState(stillServing.State) {
		t.Fatalf("tenant B's listener was already terminal (state %q, failure %+v) when tenant A's client finished; a missing listener must not pass",
			stillServing.State, stillServing.Failure)
	}
	// Stop the bounded listener now that it has proved its point instead of
	// waiting out its full lifetime.
	cancelKey, err := probeIdempotencyKey("q02-isolation-net-server-cancel")
	if err != nil {
		t.Fatalf("generate the listener cancel key: %v", err)
	}
	if _, err := runtime.B.Stage.API.PostVersioned(ctx, runtime.B.Admin, "execution.cancel",
		api.ExecutionCancelRequest{ExecutionID: serverExecution}, cancelKey, stillServing.Version); err != nil {
		t.Fatalf("cancel tenant B's listener after the client settled: %v", err)
	}
	serverFinal, _, err := resilienceWaitTerminal(ctx, runtime.B.Stage, serverExecution, 90*time.Second)
	if err != nil {
		t.Fatalf("tenant B's listener did not settle after the cancel: %v", err)
	}
	if serverFinal.State != "cancelled" {
		t.Fatalf("tenant B's listener state after the cancel = %q (failure %+v); want cancelled", serverFinal.State, serverFinal.Failure)
	}
	if _, _, err := resilienceWaitCleanupComplete(ctx, runtime.A.Stage, clientExecution, 2*time.Minute); err != nil {
		t.Fatalf("tenant A's client execution cleanup did not complete: %v", err)
	}
	if _, _, err := resilienceWaitCleanupComplete(ctx, runtime.B.Stage, serverExecution, 2*time.Minute); err != nil {
		t.Fatalf("tenant B's listener cleanup did not complete: %v", err)
	}
	clientAction, ok := resilienceAction(clientFinal, "net-client")
	if !ok {
		t.Fatalf("tenant A's execution has no net-client action: %+v", clientFinal.Actions)
	}
	clientLogs, err := resilienceReadAllLogs(ctx, runtime.A.Stage, clientExecution, clientAction.ActionID)
	if err != nil {
		t.Fatalf("read tenant A's client logs: %v", err)
	}
	if count, err := resilienceCountMarker(clientLogs, isolatedMarker); err != nil || count != 1 {
		t.Fatalf("tenant A's isolation marker count = %d, err = %v; want exactly one", count, err)
	}

	probe.Record("isolation-runtime", started, time.Now().UTC(), map[string]any{
		"execution_id": clientExecution,
		"resource_id":  serverExecution,
	})
}

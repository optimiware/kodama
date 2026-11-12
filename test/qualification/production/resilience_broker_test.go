//go:build qualification

package production

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestStageResilienceBrokerOutage is Q02 resilience probe (c). It saves the
// NATS statefulset replica count, scales it to zero for a bounded window,
// proves the Shell still commits reads and writes without the broker (AC15),
// restores the exact saved count IN BODY, and only then measures catch-up.
//
// The outage window is at most four minutes from the scale to the verified
// restore and must fit before the go test deadline minus a three-minute restore
// budget; otherwise the probe refuses to scale. SIGINT/SIGTERM cancels the
// outage body and still drives the in-body restore with a fresh bounded
// context. If a previous run died and left statefulset/nats at zero, the probe
// refuses to start and prints the exact manual recovery command:
//
//	kubectl --context <ctx> -n kodama-system scale statefulset nats --replicas=<n>
func TestStageResilienceBrokerOutage(t *testing.T) {
	config := loadStageConfig(t)
	requireStageInputs(t, config, resilienceResilienceInputs()...)
	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceBrokerOutage")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// SIGINT/SIGTERM must still leave a verified restore behind.
	signalCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	guard := resilienceNewReplicaGuard(stage.Cluster)
	savedReplicas, err := guard.Save(signalCtx, ReleaseNamespace, "statefulsets", resilienceNATSStatefulSet)
	if err != nil {
		t.Fatalf("save the NATS replica count (if a previous run left it at zero: %s): %v",
			resilienceManualBrokerRestore(config.KubeContext), err)
	}
	if err := resilienceCheckBrokerEntry(savedReplicas, config.KubeContext); err != nil {
		t.Fatalf("%v", err)
	}
	beforeCtx, stopBefore := context.WithTimeout(signalCtx, 3*time.Minute)
	beforeEvents, err := resilienceReadAllEvents(beforeCtx, stage)
	stopBefore()
	if err != nil {
		t.Fatalf("read the committed events before the outage: %v", err)
	}
	beforeIDs := resilienceEventIDs(beforeEvents)

	// Plan immediately before the scale, so pre-outage reads cannot consume the
	// budget that must remain for the verified restore.
	deadline, hasDeadline := t.Deadline()
	window, err := resiliencePlanBrokerOutage(time.Now(), deadline, hasDeadline)
	if err != nil {
		t.Fatalf("%v", err)
	}

	// Register the cleanup BEFORE the first scale. A probe that dies after the
	// scale must still restore NATS; the source-order pin enforces this.
	resilienceRestoreOnCleanup(t, probe, guard)

	outageCtx, stopOutage := context.WithTimeout(signalCtx, window)
	defer stopOutage()
	observation, bodyErr := resilienceRunBrokerOutage(outageCtx, stage, probe, guard)
	// Restore in body even when the window expired, a signal fired or a step
	// failed. The restore uses its own bounded context, never the cancelled
	// outage context.
	restoration, restoreErr := resilienceCloseOutage(outageCtx, guard, resilienceBrokerRestoreBudget)
	if restoreErr != nil {
		t.Fatalf("broker outage restore failed for execution %q (restoration %q): %v; manual restore: %s",
			observation.executionID, restoration, restoreErr, resilienceManualBrokerRestore(config.KubeContext))
	}
	if bodyErr != nil {
		t.Fatalf("broker outage window ended for execution %q (restoration %q): %v; manual restore: %s",
			observation.executionID, restoration, bodyErr, resilienceManualBrokerRestore(config.KubeContext))
	}

	postCtx, stopPost := context.WithTimeout(signalCtx, 5*time.Minute)
	defer stopPost()

	_, err = (resilienceWaiter{Timeout: 4 * time.Minute}).Wait(postCtx, func(ctx context.Context) (bool, error) {
		names, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, resilienceNATSSelector)
		if err != nil || len(names) != savedReplicas {
			return false, nil
		}
		for _, name := range names {
			if err := stage.Cluster.WaitPodReady(ctx, ReleaseNamespace, name, 5*time.Second); err != nil {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("the NATS pod did not return Ready after restoration for execution %q: %v", observation.executionID, err)
	}
	restoredObserved := time.Now().UTC()

	final, _, err := resilienceWaitTerminal(postCtx, stage, observation.executionID, 4*time.Minute)
	if err != nil {
		t.Fatalf("the offline execution %q did not converge after the broker returned: %v", observation.executionID, err)
	}
	terminalObserved := time.Now().UTC()
	if final.State != "succeeded" {
		t.Fatalf("offline execution %q state = %q, want succeeded (failure %+v)", observation.executionID, final.State, final.Failure)
	}
	completed, _, err := resilienceWaitCleanupComplete(postCtx, stage, observation.executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("offline execution %q cleanup did not complete: %v", observation.executionID, err)
	}

	afterEvents, err := resilienceReadAllEvents(postCtx, stage)
	if err != nil {
		t.Fatalf("read committed events after the broker returned: %v", err)
	}
	afterIDs := resilienceEventIDs(afterEvents)
	beforeDelta := resilienceEventDelta(beforeIDs, afterIDs)
	if len(beforeDelta.Lost) != 0 {
		t.Fatalf("committed events from before the outage were lost: %v", beforeDelta.Lost)
	}
	if len(beforeDelta.Duplicated) != 0 {
		t.Fatalf("committed events were duplicated across the outage: %v", beforeDelta.Duplicated)
	}
	midDelta := resilienceEventDelta(observation.midIDs, afterIDs)
	if len(midDelta.Lost) != 0 {
		t.Fatalf("events committed during the outage were lost after the broker returned: %v", midDelta.Lost)
	}
	terminalEventID := ""
	for _, event := range afterEvents {
		if event.SubjectID == observation.executionID && event.Type == "execution.succeeded" {
			terminalEventID = event.ID
		}
	}
	if terminalEventID == "" {
		t.Fatalf("the committed event stream has no execution.succeeded event for %q", observation.executionID)
	}

	if completed.Cleanup.WorkspaceID == "" {
		t.Fatalf("terminal cleanup for execution %q did not name the workspace identity", observation.executionID)
	}
	resilienceAssertNoOrphans(t, stage, observation.executionID, completed.Cleanup.WorkspaceID)

	probe.Record("resilience-broker-outage", observation.outageStarted, observation.outageObserved, map[string]any{
		"resource_id":  resilienceNATSStatefulSet,
		"execution_id": observation.executionID,
	})
	probe.Record("resilience-broker-restore", observation.outageObserved, restoredObserved, map[string]any{
		"resource_id":  resilienceNATSStatefulSet,
		"execution_id": observation.executionID,
	})
	probe.Record("resilience-broker-catch-up", restoredObserved, terminalObserved, map[string]any{
		"execution_id": observation.executionID,
		"event_id":     terminalEventID,
	})
	probe.Record("resilience-broker-execution", observation.executionStarted, terminalObserved, map[string]any{
		"execution_id": observation.executionID,
		"event_id":     terminalEventID,
	})
}

// resilienceBrokerObservation is the state the outage body hands to the
// post-restore assertions.
type resilienceBrokerObservation struct {
	executionID      string
	midIDs           []string
	executionStarted time.Time
	outageStarted    time.Time
	outageObserved   time.Time
	admittedEventID  string
}

// resilienceRunBrokerOutage performs the bounded outage-window work: scale to
// zero, prove no NATS pod remains, start an execution, read it and read the
// committed event stream without the broker. Every wait and API call uses the
// outage context so the window and a signal bound the whole body. It never
// restores; the caller always restores in body and in cleanup.
func resilienceRunBrokerOutage(ctx context.Context, stage *resilienceStage, probe *resilienceProbe, guard *resilienceReplicaGuard) (resilienceBrokerObservation, error) {
	observation := resilienceBrokerObservation{outageStarted: time.Now().UTC()}
	if err := guard.Scale(ctx, ReleaseNamespace, "statefulsets", resilienceNATSStatefulSet, 0); err != nil {
		return observation, fmt.Errorf("scale NATS to zero: %w", err)
	}
	if replicas, err := stage.Cluster.Replicas(ctx, ReleaseNamespace, "statefulsets", resilienceNATSStatefulSet); err != nil {
		return observation, fmt.Errorf("read the scaled NATS replica count: %w", err)
	} else if replicas != 0 {
		return observation, fmt.Errorf("NATS replicas after the scale = %d, want 0", replicas)
	}
	_, err := (resilienceWaiter{Timeout: 3 * time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		names, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, resilienceNATSSelector)
		if err != nil {
			return false, nil
		}
		return len(names) == 0, nil
	})
	if err != nil {
		return observation, fmt.Errorf("the NATS pods did not terminate during the outage: %w", err)
	}
	// The broker is affirmatively down before the write: the statefulset still
	// asks for zero replicas and no NATS pod remains.
	if replicas, err := stage.Cluster.Replicas(ctx, ReleaseNamespace, "statefulsets", resilienceNATSStatefulSet); err != nil {
		return observation, fmt.Errorf("re-read the NATS replica count during the outage: %w", err)
	} else if replicas != 0 {
		return observation, fmt.Errorf("NATS replicas during the outage = %d, want 0", replicas)
	}
	observation.outageObserved = time.Now().UTC()

	nonce := resilienceNonce()
	document := resilienceBrokerPipeline(stage.Config.Image, "resilience-broker-"+nonce)
	startedExecution, err := resilienceStartInline(ctx, stage, document, "resilience-broker-"+nonce)
	if err != nil {
		return observation, fmt.Errorf("the Shell did not admit an execution while the broker was down: %w", err)
	}
	observation.executionID = startedExecution.ExecutionID
	observation.executionStarted = time.Now().UTC()
	probe.Record("resilience-broker-start", observation.executionStarted, time.Now().UTC(), map[string]any{
		"execution_id": observation.executionID,
	})

	admitted, err := resilienceInspect(ctx, stage, observation.executionID)
	if err != nil {
		return observation, fmt.Errorf("the Shell did not serve a read for execution %q while the broker was down: %w", observation.executionID, err)
	}
	if admitted.ExecutionID != observation.executionID || admitted.Version == 0 {
		return observation, fmt.Errorf("execution %q read during the outage = %+v, want the committed execution", observation.executionID, admitted)
	}
	midEvents, err := resilienceReadAllEvents(ctx, stage)
	if err != nil {
		return observation, fmt.Errorf("read committed events for execution %q while the broker was down: %w", observation.executionID, err)
	}
	observation.midIDs = resilienceEventIDs(midEvents)
	for _, event := range midEvents {
		if event.SubjectID == observation.executionID {
			observation.admittedEventID = event.ID
			break
		}
	}
	if observation.admittedEventID == "" {
		return observation, fmt.Errorf("execution %q produced no committed event while the broker was down", observation.executionID)
	}
	return observation, nil
}

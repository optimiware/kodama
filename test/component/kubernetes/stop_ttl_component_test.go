//go:build component

package kubernetesconformance

// TTL cleanup under a pending stop on a real kind cluster.
//
// The runtime creates every workload Job with spec.ttlSecondsAfterFinished. The
// TTL-after-finished controller deletes a *finished* Job with foreground
// propagation, taking its pods with it, so a Job that finishes while a stop is
// pending would otherwise vanish and leave Stop with no evidence ever again.
//
// Stop's single UID-guarded suspension merge PATCH therefore also sends
// "ttlSecondsAfterFinished": null. This test proves on a real API server that:
//
//   - the cluster's TTL controller really deletes a finished Job after its TTL
//     (the control Job), so the survival assertion is not vacuous;
//   - the field is mutable on an existing Job through a merge PATCH with an
//     explicit null: after Stop the API server reports no TTL at all;
//   - a suspended Job that had already finished is still present well past that
//     TTL, with the same UID, no deletion timestamp, and Stop still confirms.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// finishedJobTTL returns a pointer for a Job spec TTL.
func finishedJobTTL(seconds int) *int { return &seconds }

// TestSuspensionClearsFinishedJobTTL proves the suspension PATCH clears the
// finished-Job TTL on the real API server and that the suspended Job survives
// past it.
func TestSuspensionClearsFinishedJobTTL(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-stop-ttl")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})

	// Control: an ordinary finished Job with a short TTL is deleted by the
	// cluster's TTL controller. If this never happened, the survival assertion
	// below would prove nothing about the clear.
	control, err := fx.Client.CreateJob(ctx, runtimekube.Job{
		Name:      "kodama-ttl-control",
		Namespace: fx.Namespace,
		Spec: runtimekube.JobSpec{
			Image:                   workloadImage,
			Command:                 []string{"/bin/sh", "-ec", "exit 0"},
			RestartPolicy:           "Never",
			BackoffLimit:            0,
			PodReplacementPolicy:    "Failed",
			TTLSecondsAfterFinished: finishedJobTTL(5),
		},
	})
	if err != nil {
		t.Fatalf("create TTL control Job: %v", err)
	}
	if control.Spec.TTLSecondsAfterFinished == nil || *control.Spec.TTLSecondsAfterFinished != 5 {
		t.Fatalf("control Job TTL = %v; want the created 5s TTL", control.Spec.TTLSecondsAfterFinished)
	}
	waitForJobGone(t, ctx, fx.Client, fx.Namespace, control.Name)

	// The adapter under test creates the workload Job with a short TTL too, so
	// an uncleared field would delete the suspended Job within seconds.
	const shortTTL = 20
	shortLived := newFixtureAdapterWithTTL(t, fx, shortTTL)
	fxShort := *fx
	fxShort.Adapter = shortLived

	identity := testIdentity("exec-stop-ttl", "action-stop-ttl", "workload-stop-ttl", "idem-stop-ttl")
	startPrepared(t, ctx, &fxShort, prepareRequest(
		identity, workspaceID, "build", "",
		// The workload finishes immediately, so the Job becomes Complete and its
		// finished-Job TTL starts counting before Stop observes it: the race
		// described above, made deterministic.
		testContainer([]string{`printf 'ttl-stop-marker\n'`}, nil), nil, "tok-stop-ttl",
	))

	jobBefore := waitForJobSettled(t, ctx, fx.Client, identity)
	if jobBefore.Spec.TTLSecondsAfterFinished == nil || *jobBefore.Spec.TTLSecondsAfterFinished != shortTTL {
		t.Fatalf("workload Job TTL = %v; want the created %ds TTL",
			jobBefore.Spec.TTLSecondsAfterFinished, shortTTL)
	}
	if jobBefore.DeletionTimestamp != nil {
		t.Fatalf("workload Job is already deleting: %v", jobBefore.DeletionTimestamp)
	}

	evidence, err := shortLived.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmation from the affirmatively terminated pod", evidence)
	}

	jobAfter := oneJob(t, ctx, fx.Client, identity)
	if jobAfter.UID != jobBefore.UID {
		t.Fatalf("Job UID changed from %q to %q; Stop must patch the same Job", jobBefore.UID, jobAfter.UID)
	}
	if jobAfter.DeletionTimestamp != nil {
		t.Fatalf("Stop left a deletion timestamp on the Job: %v", jobAfter.DeletionTimestamp)
	}
	if jobAfter.Spec.Suspend == nil || !*jobAfter.Spec.Suspend {
		t.Fatalf("Job spec.suspend = %v after Stop; want true", jobAfter.Spec.Suspend)
	}
	if jobAfter.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("suspension left ttlSecondsAfterFinished=%d; the finished Job would be TTL-deleted",
			*jobAfter.Spec.TTLSecondsAfterFinished)
	}

	// Prove the clear against the API server itself, not only the driver
	// decoder: the field must be absent from spec (the merge patch null removed
	// it).
	document, err := fx.Cluster.GetJSON(ctx, "get", "job", jobAfter.Name, "-n", fx.Namespace)
	if err != nil {
		t.Fatalf("read Job spec: %v", err)
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		t.Fatalf("Job spec = %v; want an object", document["spec"])
	}
	if value, present := spec["ttlSecondsAfterFinished"]; present {
		t.Fatalf("Job spec still carries ttlSecondsAfterFinished=%v after the suspension", value)
	}

	// Wait past the short TTL. Without the clear the TTL controller would have
	// removed the Job (and its pods) by now, as the control proved.
	select {
	case <-ctx.Done():
		t.Fatalf("waiting past the finished-Job TTL: %v", ctx.Err())
	case <-time.After(time.Duration(shortTTL+15) * time.Second):
	}

	surviving, err := fx.Client.GetJob(ctx, fx.Namespace, jobAfter.Name)
	if err != nil {
		t.Fatalf("suspended Job was removed within the finished-Job TTL: %v", err)
	}
	if surviving.UID != jobBefore.UID {
		t.Fatalf("surviving Job UID = %q; want %q", surviving.UID, jobBefore.UID)
	}
	if surviving.DeletionTimestamp != nil {
		t.Fatalf("surviving Job carries a deletion timestamp: %v", surviving.DeletionTimestamp)
	}
	onePod(t, ctx, fx.Client, identity)

	again, err := shortLived.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Stop after the TTL window: %v", err)
	}
	if !again.IsConfirmed() {
		t.Fatalf("stop evidence after the TTL window = %+v; want a still-confirmed stop", again)
	}

	removal, err := shortLived.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want the suspended Job's resources removed", removal)
	}
	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
}

// waitForJobGone blocks until GetJob reports ErrNotFound, as the TTL controller
// leaves it.
func waitForJobGone(t *testing.T, ctx context.Context, client *driverkube.Cluster, namespace, name string) {
	t.Helper()
	var last error
	probe := func(ctx context.Context) error {
		_, err := client.GetJob(ctx, namespace, name)
		switch {
		case errors.Is(err, runtimekube.ErrNotFound):
			return nil
		case err != nil:
			return err
		default:
			last = fmt.Errorf("Job %s is still present", name)
			return last
		}
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 3 * time.Minute
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "TTL deletion of "+name, probe, options); err != nil {
		t.Fatalf("the cluster TTL controller never deleted finished Job %s: %v", name, err)
	}
}

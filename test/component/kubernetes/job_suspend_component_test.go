//go:build component

package kubernetesconformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// suspensionAckTimeout bounds the wait for the Job controller to acknowledge a
// suspension PATCH with Suspended=True. It is a readiness barrier, not a fixed
// settle sleep.
const suspensionAckTimeout = 90 * time.Second

// suspensionObservationWindow bounds how long a running-Job suspension is
// watched for a replacement pod. Suspension deletes the running pod and must
// never make the Job create another one.
const suspensionObservationWindow = 20 * time.Second

// TestJobSuspensionPreservesUIDAndAcceptsTerminalJobs is the wire proof on a
// real cluster: the UID-bound merge PATCH suspends the same Job without
// replacing it, repeated suspension preserves the UID and never creates another
// pod, and an already Complete or Failed Job accepts the PATCH.
func TestJobSuspensionPreservesUIDAndAcceptsTerminalJobs(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-suspend")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})

	t.Run("running Job", func(t *testing.T) {
		identity := testIdentity("exec-suspend", "action-suspend", "workload-suspend", "idem-suspend")
		startPrepared(t, ctx, fx, prepareRequest(
			identity, workspaceID, "build", "",
			// No TERM trap: a suspended running pod is deleted promptly, so the
			// test does not wait out the full termination grace period.
			testContainer([]string{`sleep 120`}, nil), nil, "tok-suspend",
		))
		podBefore := waitForPodRunning(t, ctx, fx, fx.Client, identity)
		podUIDBefore := objectUID(t, ctx, fx, "pod", podBefore.Name)
		jobBefore := oneJob(t, ctx, fx.Client, identity)
		if jobBefore.UID == "" {
			t.Fatal("live Job has no metadata.uid")
		}
		if jobBefore.Spec.Suspend != nil && *jobBefore.Spec.Suspend {
			t.Fatalf("fresh Job is already suspended: %v", jobBefore.Spec.Suspend)
		}

		first, err := fx.Client.SuspendJob(ctx, fx.Namespace, jobBefore.Name, jobBefore.UID)
		if err != nil {
			t.Fatalf("SuspendJob: %v", err)
		}
		requireSuspendedJob(t, first, jobBefore.UID, "first suspension")

		// A repeated suspension is idempotent: same object, same UID.
		second, err := fx.Client.SuspendJob(ctx, fx.Namespace, jobBefore.Name, jobBefore.UID)
		if err != nil {
			t.Fatalf("second SuspendJob: %v", err)
		}
		if second.UID != first.UID {
			t.Fatalf("second suspension returned UID %q; want the first %q", second.UID, first.UID)
		}
		requireSuspendedJob(t, second, jobBefore.UID, "second suspension")

		// The controller eventually acknowledges the intent on the same Job.
		// The acknowledgment condition can appear while the old pod is still
		// counted as terminating, so only presence is asserted here; the
		// counters must converge once the pod is gone.
		acknowledged := waitForJobSuspended(t, ctx, fx, jobBefore.Name, jobBefore.UID)
		if acknowledged.Status.Terminating == nil {
			t.Fatalf("acknowledged suspension terminating is unknown; want the server to report it")
		}

		// No replacement workload pod may appear, and the same Job survives.
		requireNoReplacementPod(t, ctx, fx, identity, podBefore.Name, podUIDBefore)
		settled := waitForJobCountersZero(t, ctx, fx, jobBefore.Name, jobBefore.UID)
		if settled.Status.Active != 0 {
			t.Fatalf("suspended Job still reports active=%d", settled.Status.Active)
		}
		if settled.Status.Terminating == nil || *settled.Status.Terminating != 0 {
			t.Fatalf("suspended Job terminating = %v; want a present zero", settled.Status.Terminating)
		}
		jobAfter := oneJob(t, ctx, fx.Client, identity)
		if jobAfter.UID != jobBefore.UID {
			t.Fatalf("Job UID changed from %q to %q; suspension must never replace it", jobBefore.UID, jobAfter.UID)
		}
		if jobAfter.Spec.Suspend == nil || !*jobAfter.Spec.Suspend {
			t.Fatalf("Job spec.suspend = %v after suspension; want true", jobAfter.Spec.Suspend)
		}
	})

	t.Run("complete Job accepts patch", func(t *testing.T) {
		identity := testIdentity("exec-complete", "action-complete", "workload-complete", "idem-complete")
		startPrepared(t, ctx, fx, prepareRequest(
			identity, workspaceID, "build", "",
			testContainer([]string{`exit 0`}, nil), nil, "tok-complete",
		))
		waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
			Identity: identity, WorkspaceID: workspaceID,
		}, kernel.ObservationExited)
		settled := waitForJobSettled(t, ctx, fx.Client, identity)
		if settled.Status.Succeeded != 1 {
			t.Fatalf("completed Job status = %+v; want succeeded 1", settled.Status)
		}
		requireTerminalSuspensionAccepted(t, ctx, fx, settled, "complete")
	})

	t.Run("failed Job accepts patch", func(t *testing.T) {
		identity := testIdentity("exec-failed", "action-failed", "workload-failed", "idem-failed")
		startPrepared(t, ctx, fx, prepareRequest(
			identity, workspaceID, "build", "",
			testContainer([]string{`exit 7`}, nil), nil, "tok-failed",
		))
		waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
			Identity: identity, WorkspaceID: workspaceID,
		}, kernel.ObservationExited)
		settled := waitForJobSettled(t, ctx, fx.Client, identity)
		if settled.Status.Failed != 1 {
			t.Fatalf("failed Job status = %+v; want failed 1", settled.Status)
		}
		requireTerminalSuspensionAccepted(t, ctx, fx, settled, "failed")
	})
}

// requireSuspendedJob asserts one suspension answer kept the same Job identity
// and recorded the suspension intent.
func requireSuspendedJob(t *testing.T, job runtimekube.Job, wantUID, operation string) {
	t.Helper()
	if job.UID != wantUID {
		t.Fatalf("%s returned UID %q; want %q", operation, job.UID, wantUID)
	}
	if job.Name == "" || job.Namespace != testNamespace {
		t.Fatalf("%s returned an incomplete Job identity: %+v", operation, job)
	}
	if job.Spec.Suspend == nil || !*job.Spec.Suspend {
		t.Fatalf("%s returned spec.suspend = %v; want true", operation, job.Spec.Suspend)
	}
	if job.DeletionTimestamp != nil {
		t.Fatalf("%s returned a deleting Job: %v", operation, job.DeletionTimestamp)
	}
}

// requireTerminalSuspensionAccepted proves an already Complete/Failed Job
// accepts the suspension PATCH without changing its UID.
func requireTerminalSuspensionAccepted(t *testing.T, ctx context.Context, fx *fixture, job runtimekube.Job, label string) {
	t.Helper()
	suspended, err := fx.Client.SuspendJob(ctx, fx.Namespace, job.Name, job.UID)
	if err != nil {
		t.Fatalf("SuspendJob on %s Job: %v", label, err)
	}
	if suspended.UID != job.UID {
		t.Fatalf("%s Job UID changed from %q to %q", label, job.UID, suspended.UID)
	}
	if suspended.Spec.Suspend == nil || !*suspended.Spec.Suspend {
		t.Fatalf("%s Job spec.suspend = %v; want true", label, suspended.Spec.Suspend)
	}
}

// waitForJobSuspended blocks until the Job carries Suspended=True and returns
// the acknowledged Job. It reads status through the production driver.
func waitForJobSuspended(t *testing.T, ctx context.Context, fx *fixture, name, wantUID string) runtimekube.Job {
	t.Helper()
	var last runtimekube.Job
	probe := func(ctx context.Context) error {
		job, err := fx.Client.GetJob(ctx, fx.Namespace, name)
		if err != nil {
			return err
		}
		last = job
		if job.UID != wantUID {
			return fmt.Errorf("Job UID = %q; want %q", job.UID, wantUID)
		}
		for _, condition := range job.Status.Conditions {
			if condition.Type == "Suspended" && condition.Status == "True" {
				return nil
			}
		}
		return fmt.Errorf("Suspended=True condition not observed yet: %+v", job.Status.Conditions)
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = suspensionAckTimeout
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "suspended Job "+name, probe, options); err != nil {
		t.Fatalf("wait for suspension acknowledgment: %v", err)
	}
	return last
}

// waitForJobCountersZero blocks until the acknowledged Job reports active=0 and
// a present terminating=0, and returns that observation.
func waitForJobCountersZero(t *testing.T, ctx context.Context, fx *fixture, name, wantUID string) runtimekube.Job {
	t.Helper()
	var last runtimekube.Job
	probe := func(ctx context.Context) error {
		job, err := fx.Client.GetJob(ctx, fx.Namespace, name)
		if err != nil {
			return err
		}
		last = job
		if job.UID != wantUID {
			return fmt.Errorf("Job UID = %q; want %q", job.UID, wantUID)
		}
		if job.Status.Active != 0 {
			return fmt.Errorf("active = %d; want 0", job.Status.Active)
		}
		if job.Status.Terminating == nil {
			return fmt.Errorf("terminating is unknown; want a present zero")
		}
		if *job.Status.Terminating != 0 {
			return fmt.Errorf("terminating = %d; want 0", *job.Status.Terminating)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = suspensionAckTimeout
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "suspended Job counters "+name, probe, options); err != nil {
		t.Fatalf("wait for suspension counters: %v", err)
	}
	return last
}

// requireNoReplacementPod watches a bounded window after a running-Job
// suspension. It fails if the Job controller creates a second pod or a pod with
// a different identity, and requires the original pod to be gone or still
// terminating at the end of the window.
func requireNoReplacementPod(
	t *testing.T,
	ctx context.Context,
	fx *fixture,
	identity engine.EffectIdentity,
	originalName, originalUID string,
) {
	t.Helper()
	selector := map[string]string{runtimekube.LabelWorkload: string(identity.WorkloadID)}
	deadline := time.Now().Add(suspensionObservationWindow)
	for {
		pods, err := fx.Client.ListPods(ctx, fx.Namespace, selector)
		if err != nil {
			t.Fatalf("ListPods while observing suspension: %v", err)
		}
		if len(pods) > 1 {
			t.Fatalf("suspension created %d workload pods; want at most the original", len(pods))
		}
		if len(pods) == 1 {
			pod := pods[0]
			if pod.Name != originalName {
				t.Fatalf("suspension replaced pod %q with %q", originalName, pod.Name)
			}
			if uid := objectUID(t, ctx, fx, "pod", pod.Name); uid != originalUID {
				t.Fatalf("suspension replaced pod %q UID %q with %q", pod.Name, originalUID, uid)
			}
		}
		jobs, err := fx.Client.ListJobs(ctx, fx.Namespace, selector)
		if err != nil {
			t.Fatalf("ListJobs while observing suspension: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("suspension changed the workload Job count to %d", len(jobs))
		}
		if len(pods) == 0 {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("suspension observation interrupted: %v", ctx.Err())
		case <-timer.C:
		}
	}
}

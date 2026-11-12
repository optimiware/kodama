// Lifecycle behavior: stable identities, lost responses, the declared
// observation vocabulary, the stop barrier, idempotent attributed cleanup,
// credential refresh without restart and adapter reconstruction.
package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

func setupStarted(t *testing.T) (*fakeCluster, *fakeClock, *Adapter, engine.PrepareRequest) {
	t.Helper()
	fake, clock, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	return fake, clock, adapter, request
}

func onlyJob(t *testing.T, fake *fakeCluster) Job {
	t.Helper()
	jobs := fake.jobsSnapshot()
	if len(jobs) != 1 {
		t.Fatalf("cluster has %d jobs; want exactly 1", len(jobs))
	}
	return jobs[0]
}

func deleteJobDirect(t *testing.T, fake *fakeCluster, job Job) {
	t.Helper()
	if err := fake.DeleteJob(context.Background(), job.Namespace, job.Name); err != nil {
		t.Fatalf("deleting job directly: %v", err)
	}
}

// TestStartLostResponseIsUnknownAndRetryReusesJob covers crash-after-effect:
// the Job was created but the response was lost, so Start must stay unknown and
// a retry must find the same stable Job instead of creating a replacement.
func TestStartLostResponseIsUnknownAndRetryReusesJob(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)

	fake.injectAfter("CreateJob", errors.New("connection reset after write"))
	result, err := adapter.Start(context.Background(), engine.StartRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err == nil {
		t.Fatal("Start hid a lost create response")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("lost-response outcome = %q; want %q", result.Outcome, engine.StartUnknown)
	}
	if result.Outcome == engine.StartProvenNotStarted {
		t.Fatal("lost-response outcome collapsed into proven-not-started")
	}
	if fake.jobCount() != 1 {
		t.Fatalf("cluster has %d jobs after the lost response; want the single created job", fake.jobCount())
	}
	created := onlyJob(t, fake)

	fake.clearInjection("CreateJob")
	retry, err := adapter.Start(context.Background(), engine.StartRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("retry after lost response returned error: %v", err)
	}
	if retry.Outcome != engine.StartAccepted {
		t.Fatalf("retry outcome = %q; want %q", retry.Outcome, engine.StartAccepted)
	}
	if fake.jobCount() != 1 {
		t.Fatalf("retry created a replacement job; cluster has %d jobs", fake.jobCount())
	}
	if retried := onlyJob(t, fake); retried.Name != created.Name {
		t.Fatalf("retry used job name %q; want the stable name %q", retried.Name, created.Name)
	}
}

// TestStartCrashBeforeEffectStaysUnknown proves a clean transport failure with
// no affirmative evidence never becomes proven-not-started.
func TestStartCrashBeforeEffectStaysUnknown(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)

	fake.injectBefore("CreateJob", errors.New("api server unreachable"))
	result, err := adapter.Start(context.Background(), engine.StartRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err == nil {
		t.Fatal("Start hid an unreachable API server")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("pre-effect failure outcome = %q; want %q", result.Outcome, engine.StartUnknown)
	}
	if fake.jobCount() != 0 {
		t.Fatal("pre-effect failure created a job")
	}

	observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation after pre-effect failure = %q; want %q", observation.Kind, kernel.ObservationAbsent)
	}
	if observation.ProofOfNonexecution() {
		t.Fatal("absence was reported as affirmative proof of nonexecution")
	}
}

// TestObserveUsesKernelVocabulary proves every observation kind is reported
// distinctly and absence/node loss/replacement never collapse into exit or
// stop evidence.
func TestObserveUsesKernelVocabulary(t *testing.T) {
	identity := testIdentity()

	cases := []struct {
		name  string
		setup func(t *testing.T, fake *fakeCluster, adapter *Adapter)
		want  kernel.ObservationKind
	}{
		{
			name:  "job without a pod is running",
			setup: func(*testing.T, *fakeCluster, *Adapter) {},
			want:  kernel.ObservationRunning,
		},
		{
			name: "running pod",
			setup: func(_ *testing.T, fake *fakeCluster, _ *Adapter) {
				fake.addPod(testRunningPod(identity))
			},
			want: kernel.ObservationRunning,
		},
		{
			name: "pending pod is still running work",
			setup: func(_ *testing.T, fake *fakeCluster, _ *Adapter) {
				fake.addPod(testPod(identity, "Pending"))
			},
			want: kernel.ObservationRunning,
		},
		{
			name: "deleted job and no pod is absent",
			setup: func(t *testing.T, fake *fakeCluster, _ *Adapter) {
				deleteJobDirect(t, fake, onlyJob(t, fake))
			},
			want: kernel.ObservationAbsent,
		},
		{
			name: "deleted job with a live pod is running",
			setup: func(t *testing.T, fake *fakeCluster, _ *Adapter) {
				deleteJobDirect(t, fake, onlyJob(t, fake))
				fake.addPod(testRunningPod(identity))
			},
			want: kernel.ObservationRunning,
		},
		{
			name: "unknown node is unknown",
			setup: func(_ *testing.T, fake *fakeCluster, _ *Adapter) {
				fake.addPod(testPod(identity, "Unknown"))
			},
			want: kernel.ObservationUnknown,
		},
		{
			name: "terminating pod is still running",
			setup: func(_ *testing.T, fake *fakeCluster, _ *Adapter) {
				fake.addPod(testTerminatingPod(identity))
			},
			want: kernel.ObservationRunning,
		},
		{
			name: "replacement pod is unknown",
			setup: func(_ *testing.T, fake *fakeCluster, _ *Adapter) {
				fake.addPod(testRunningPod(identity))
				replacement := testRunningPod(identity)
				replacement.Name = "pod-2"
				fake.addPod(replacement)
			},
			want: kernel.ObservationUnknown,
		},
		{
			name: "failed phase without terminated container is unknown",
			setup: func(_ *testing.T, fake *fakeCluster, _ *Adapter) {
				pod := testPod(identity, "Failed")
				pod.Containers = []ContainerStatus{{Name: "workload", Terminated: false}}
				fake.addPod(pod)
			},
			want: kernel.ObservationUnknown,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter, _ := setupStarted(t)
			testCase.setup(t, fake, adapter)

			observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
				Identity:    identity,
				WorkspaceID: testWorkspaceID(),
			})
			if err != nil {
				t.Fatalf("Observe returned error: %v", err)
			}
			if observation.Kind != testCase.want {
				t.Fatalf("observation kind = %q; want %q", observation.Kind, testCase.want)
			}
			if testCase.want != kernel.ObservationExited && observation.StopConfirmed() {
				t.Fatal("non-exited observation claims a confirmed stop")
			}
		})
	}
}

// TestObserveReportsExitAndStopBarrier proves exit status and stop confirmation
// come only from affirmative container termination evidence.
func TestObserveReportsExitAndStopBarrier(t *testing.T) {
	identity := testIdentity()

	cases := []struct {
		name     string
		pod      Pod
		wantCode int
	}{
		{name: "success", pod: testTerminatedPod(identity, "Succeeded", 0), wantCode: 0},
		{name: "failure", pod: testTerminatedPod(identity, "Failed", 137), wantCode: 137},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter, _ := setupStarted(t)
			fake.addPod(testCase.pod)

			observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
				Identity:    identity,
				WorkspaceID: testWorkspaceID(),
			})
			if err != nil {
				t.Fatalf("Observe returned error: %v", err)
			}
			if observation.Kind != kernel.ObservationExited {
				t.Fatalf("observation kind = %q; want %q", observation.Kind, kernel.ObservationExited)
			}
			code, ok := observation.ExitStatus()
			if !ok || code != testCase.wantCode {
				t.Fatalf("exit status = %d (present=%v); want %d", code, ok, testCase.wantCode)
			}
			if !observation.StopConfirmed() {
				t.Fatal("terminated container did not confirm the stop barrier")
			}
			if !observation.Valid() {
				t.Fatalf("observation is not internally consistent: %+v", observation)
			}
		})
	}
}

// TestStopNeverConfirmsFromDeletionOrElapsedTime proves an unacknowledged
// suspension, missing pods, a terminating pod and the passage of time are not
// stop proof.
func TestStopNeverConfirmsFromDeletionOrElapsedTime(t *testing.T) {
	identity := testIdentity()

	t.Run("unacknowledged suspension and absent pod", func(t *testing.T) {
		fake, clock, adapter, _ := setupStarted(t)
		before := onlyJob(t, fake)
		clock.advance(24 * time.Hour)

		evidence, err := adapter.Stop(context.Background(), engine.StopRequest{
			Identity:    identity,
			WorkspaceID: testWorkspaceID(),
		})
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if evidence.IsConfirmed() {
			t.Fatal("an unacknowledged suspension was accepted as stop proof")
		}
		if fake.jobCount() != 1 {
			t.Fatal("Stop deleted the Job instead of suspending it")
		}
		if fake.callCount("SuspendJob") != 1 {
			t.Fatalf("suspension PATCHes = %d; want one", fake.callCount("SuspendJob"))
		}
		stored := fake.jobMustGet(t, before.Namespace, before.Name)
		if stored.UID != before.UID {
			t.Fatalf("Job UID changed from %q to %q", before.UID, stored.UID)
		}
		if stored.Spec.Suspend == nil || !*stored.Spec.Suspend {
			t.Fatalf("Job spec.suspend = %v; want true", stored.Spec.Suspend)
		}

		again, err := adapter.Stop(context.Background(), engine.StopRequest{
			Identity:    identity,
			WorkspaceID: testWorkspaceID(),
		})
		if err != nil {
			t.Fatalf("repeated Stop returned error: %v", err)
		}
		if again.IsConfirmed() {
			t.Fatal("elapsed time or repeated acknowledgement confirmed a stop")
		}
	})

	t.Run("node lost", func(t *testing.T) {
		fake, clock, adapter, _ := setupStarted(t)
		fake.addPod(testPod(identity, "Unknown"))
		clock.advance(24 * time.Hour)

		evidence, err := adapter.Stop(context.Background(), engine.StopRequest{
			Identity:    identity,
			WorkspaceID: testWorkspaceID(),
		})
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if evidence.IsConfirmed() {
			t.Fatal("an unreachable node confirmed a stop")
		}
	})

	t.Run("pod terminating", func(t *testing.T) {
		fake, _, adapter, _ := setupStarted(t)
		fake.addPod(testTerminatingPod(identity))

		evidence, err := adapter.Stop(context.Background(), engine.StopRequest{
			Identity:    identity,
			WorkspaceID: testWorkspaceID(),
		})
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if evidence.IsConfirmed() {
			t.Fatal("a pod with a deletion timestamp was accepted as stopped")
		}
	})
}

// TestStopConfirmsOnlyObservedPodTermination proves the stop barrier closes on
// affirmative container termination and never creates a replacement Job.
func TestStopConfirmsOnlyObservedPodTermination(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	fake.addPod(testTerminatedPod(request.Identity, "Failed", 137))
	createsBefore := fake.callCount("CreateJob")

	evidence, err := adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed termination", evidence)
	}
	if evidence.Detail == "" {
		t.Fatal("confirmed stop has no detail")
	}
	if fake.callCount("CreateJob") != createsBefore {
		t.Fatal("Stop created a replacement Job")
	}
	if selector := fake.lastSelector(); selector[LabelWorkload] != string(request.Identity.WorkloadID) {
		t.Fatalf("pod selector = %v; want the stable workload label", selector)
	}

	observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	if !observation.StopConfirmed() {
		t.Fatalf("observation after confirmed stop = %+v", observation)
	}
}

// TestTerminatingPodWithTerminatedContainerConfirmsStop covers TTL cleanup and
// termination races: once the workload container affirmatively terminated, a
// deletion timestamp must not hide the exit, keep the stop pending or block
// workspace release. A terminating pod without that evidence stays uncertain.
func TestTerminatingPodWithTerminatedContainerConfirmsStop(t *testing.T) {
	t.Run("stop confirms", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		fake.addPod(testTerminatingTerminatedPod(request.Identity, "Failed", 137))

		evidence, err := adapter.Stop(context.Background(), engine.StopRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if !evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; want confirmed termination of a terminating pod", evidence)
		}
	})

	t.Run("observe reports exit", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		fake.addPod(testTerminatingTerminatedPod(request.Identity, "Succeeded", 0))

		observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind != kernel.ObservationExited {
			t.Fatalf("observation kind = %q; want %q", observation.Kind, kernel.ObservationExited)
		}
		code, ok := observation.ExitStatus()
		if !ok || code != 0 {
			t.Fatalf("exit status = %d (present=%v); want 0", code, ok)
		}
		if !observation.StopConfirmed() {
			t.Fatal("terminating pod with a terminated container did not confirm the stop barrier")
		}
		if !observation.Valid() {
			t.Fatalf("observation is not internally consistent: %+v", observation)
		}
	})

	t.Run("terminated status without a terminal phase confirms", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		fake.addPod(testTerminatingTerminatedPod(request.Identity, "Running", 0))

		observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind != kernel.ObservationExited || !observation.StopConfirmed() {
			t.Fatalf("observation = %+v; want an exited observation with a confirmed stop", observation)
		}
	})

	t.Run("remove leaves the workspace for Workspace.Release", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		fake.addPod(testTerminatingTerminatedPod(request.Identity, "Failed", 1))
		claimName := testClaimName(t, request.WorkspaceID)

		removal, err := adapter.Remove(context.Background(), engine.RemoveRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("Remove returned error: %v", err)
		}
		if !removal.Removed {
			t.Fatalf("removal = %+v; want the terminated workload's resources removed", removal)
		}
		if !fake.claimExists(testNamespace, claimName) {
			t.Fatal("Remove deleted the workspace claim; only Workspace.Release may do that")
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Remove attempted to delete the workspace claim")
		}
	})

	t.Run("terminating container without termination stays pending", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		fake.addPod(testTerminatingPod(request.Identity))
		claimName := testClaimName(t, request.WorkspaceID)

		observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind != kernel.ObservationRunning || observation.StopConfirmed() {
			t.Fatalf("terminating observation = %+v; want running without stop confirmation", observation)
		}

		evidence, err := adapter.Stop(context.Background(), engine.StopRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if evidence.IsConfirmed() {
			t.Fatal("a terminating pod without terminated-container evidence confirmed a stop")
		}

		removal, err := adapter.Remove(context.Background(), engine.RemoveRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("Remove returned error: %v", err)
		}
		if removal.Removed {
			t.Fatal("Remove released the workspace while the terminating container may still run")
		}
		if !fake.claimExists(testNamespace, claimName) {
			t.Fatal("Remove deleted the claim while the terminating container may still run")
		}
	})
}

// TestStopLostResponseStaysPending proves a lost suspension response is not
// stop confirmation, while later observed termination still closes the barrier.
func TestStopLostResponseStaysPending(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	before := onlyJob(t, fake)
	fake.injectAfter("SuspendJob", errors.New("connection reset after suspension"))

	evidence, err := adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err == nil {
		t.Fatal("Stop hid a lost suspension response")
	}
	if evidence.IsConfirmed() {
		t.Fatal("a lost stop response was treated as confirmed stop")
	}
	if fake.jobCount() != 1 {
		t.Fatal("Stop deleted the Job instead of leaving the suspension effect")
	}
	if stored := fake.jobMustGet(t, before.Namespace, before.Name); stored.UID != before.UID {
		t.Fatalf("Job UID changed from %q to %q", before.UID, stored.UID)
	}

	fake.clearInjection("SuspendJob")
	fake.addPod(testTerminatedPod(request.Identity, "Succeeded", 0))
	evidence, err = adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("retry Stop returned error: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatal("observed termination after a lost response did not confirm stop")
	}
}

// TestRemoveIsIdempotentAndAttributed proves cleanup removes owned resources,
// is safe to repeat, and never creates a workload.
func TestRemoveIsIdempotentAndAttributed(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	fake.addPod(testTerminatedPod(request.Identity, "Succeeded", 0))
	createsBefore := fake.callCount("CreateJob")

	removal, err := adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want the owned resources removed", removal)
	}
	if removal.Detail == "" {
		t.Fatal("removal has no detail")
	}
	if fake.jobCount() != 0 {
		t.Fatal("Remove left the Job behind")
	}
	if len(fake.configMapsSnapshot()) != 0 || len(fake.secretsSnapshot()) != 0 {
		t.Fatal("Remove left activation resources behind")
	}
	claimName := testClaimName(t, request.WorkspaceID)
	if !fake.claimExists(testNamespace, claimName) {
		t.Fatal("Remove deleted the workspace claim; Remove never releases a retained workspace")
	}
	if fake.callCount("DeleteClaim") != 0 {
		t.Fatal("Remove attempted to delete the workspace claim")
	}

	second, err := adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("repeated Remove returned error: %v", err)
	}
	if !second.Removed {
		t.Fatalf("repeated removal = %+v; want idempotent success", second)
	}
	if fake.callCount("CreateJob") != createsBefore {
		t.Fatal("Remove created a workload")
	}
}

// TestRemoveRetainsWorkspaceWhilePodLive proves a live pod keeps the retained
// workspace claim and that an unreachable API never reports false success.
func TestRemoveRetainsWorkspaceWhilePodLive(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	claimName := testClaimName(t, request.WorkspaceID)
	fake.addPod(testRunningPod(request.Identity))

	removal, err := adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if removal.Removed {
		t.Fatal("Remove reported success while a pod was still live")
	}
	if !strings.Contains(removal.Detail, claimName) {
		t.Fatalf("removal detail %q does not name the retained claim %q", removal.Detail, claimName)
	}
	if !fake.claimExists(testNamespace, claimName) {
		t.Fatal("Remove released the workspace claim while a pod was live")
	}
	if fake.callCount("DeleteClaim") != 0 {
		t.Fatal("Remove attempted to delete the claim while a pod was live")
	}

	fake.setPods([]Pod{testTerminatedPod(request.Identity, "Succeeded", 0)})
	removal, err = adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove after termination returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal after termination = %+v; want success", removal)
	}
	if !fake.claimExists(testNamespace, claimName) {
		t.Fatal("Remove released the workspace claim; Workspace.Release owns that decision")
	}

	t.Run("unreachable API keeps the claim", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		claimName := testClaimName(t, request.WorkspaceID)
		fake.injectBefore("ListPods", errors.New("api server unreachable"))

		removal, _ := adapter.Remove(context.Background(), engine.RemoveRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if removal.Removed {
			t.Fatal("Remove reported success while pod state was unreachable")
		}
		if !strings.Contains(removal.Detail, claimName) {
			t.Fatalf("removal detail %q does not name the retained claim", removal.Detail)
		}
		if !fake.claimExists(testNamespace, claimName) {
			t.Fatal("Remove released the claim without pod evidence")
		}
	})
}

// TestRemoveBoundsRetriesWithInjectedBackoff proves cleanup retries are bounded
// by installation policy, use the injected clock, and leave visible pending
// work with attribution instead of a false success.
func TestRemoveBoundsRetriesWithInjectedBackoff(t *testing.T) {
	fake, clock, adapter, request := setupStarted(t)
	fake.addPod(testTerminatedPod(request.Identity, "Succeeded", 0))
	claimName := testClaimName(t, request.WorkspaceID)
	fake.injectBefore("DeleteConfigMap", errors.New("activation snapshot still mounted"))

	removal, _ := adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if removal.Removed {
		t.Fatal("Remove reported success after every activation snapshot deletion failed")
	}
	if !strings.Contains(removal.Detail, claimName) {
		t.Fatalf("removal detail %q does not attribute the retained claim %q", removal.Detail, claimName)
	}
	if attempts := fake.callCount("DeleteConfigMap"); attempts != 3 {
		t.Fatalf("DeleteConfigMap attempts = %d; want the configured 3", attempts)
	}
	if attempts := fake.callCount("DeleteJob"); attempts != 1 {
		t.Fatalf("DeleteJob attempts = %d; want one successful attempt", attempts)
	}
	sleeps := clock.recordedSleeps()
	if len(sleeps) != 2 {
		t.Fatalf("backoff waits = %v; want 2 waits between 3 attempts", sleeps)
	}
	for _, sleep := range sleeps {
		if sleep != 2*time.Second {
			t.Fatalf("backoff wait = %v; want the configured 2s", sleep)
		}
	}

	fake.clearInjection("DeleteConfigMap")
	removal, err := adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove retry returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal retry = %+v; want success once the snapshot is gone", removal)
	}
	if !fake.claimExists(testNamespace, claimName) {
		t.Fatal("Remove released the workspace claim; Workspace.Release owns that decision")
	}
}

// TestRefreshCredentialDoesNotRestartWorkload proves token replacement updates
// only the credential Secret and never replays the command.
func TestRefreshCredentialDoesNotRestartWorkload(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	jobBefore := onlyJob(t, fake)
	createsBefore := fake.callCount("CreateJob")

	refresh, err := adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   request.Identity,
		Credential: "tok-new",
	})
	if err != nil {
		t.Fatalf("RefreshCredential returned error: %v", err)
	}
	if !refresh.Delivered {
		t.Fatalf("refresh = %+v; want delivered", refresh)
	}

	secrets := fake.secretsSnapshot()
	if len(secrets) != 1 {
		t.Fatalf("cluster has %d secrets; want exactly 1", len(secrets))
	}
	if got := string(secrets[0].Data[id.SecretTokenName]); got != "tok-new" {
		t.Fatalf("credential = %q; want the refreshed value", got)
	}
	if got := string(secrets[0].Data["API_KEY"]); got != "s3cr3t" {
		t.Fatalf("declared secret = %q; refresh must not disturb other mounted values", got)
	}
	if fake.callCount("CreateJob") != createsBefore {
		t.Fatal("RefreshCredential created a new Job")
	}
	if fake.callCount("DeleteJob") != 0 {
		t.Fatal("RefreshCredential deleted the Job")
	}
	jobAfter := onlyJob(t, fake)
	if jobAfter.ResourceVersion != jobBefore.ResourceVersion || jobAfter.Name != jobBefore.Name {
		t.Fatal("RefreshCredential replaced or mutated the Job")
	}
}

// TestRefreshCredentialRecoversLostResponseAndStaleVersion proves delivery is
// recoverable after a lost update and a stale resource version.
func TestRefreshCredentialRecoversLostResponseAndStaleVersion(t *testing.T) {
	t.Run("lost update response", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		fake.injectAfter("PutSecret", errors.New("connection reset after write"))

		refresh, err := adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
			Identity:   request.Identity,
			Credential: "tok-new",
		})
		if err == nil {
			t.Fatal("RefreshCredential hid a lost update response")
		}
		if refresh.Delivered {
			t.Fatal("a lost update response was reported as delivered")
		}
		if fake.callCount("CreateJob") != 1 {
			t.Fatal("lost update restarted the workload")
		}

		fake.clearInjection("PutSecret")
		retry, err := adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
			Identity:   request.Identity,
			Credential: "tok-new",
		})
		if err != nil {
			t.Fatalf("retry returned error: %v", err)
		}
		if !retry.Delivered {
			t.Fatal("retry after lost response was not delivered")
		}
		if fake.callCount("CreateJob") != 1 {
			t.Fatal("retry restarted the workload")
		}
	})

	t.Run("stale resource version", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		fake.injectBefore("PutSecret", ErrConflict)

		refresh, err := adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
			Identity:   request.Identity,
			Credential: "tok-new",
		})
		if err == nil {
			t.Fatal("RefreshCredential hid a stale resource version")
		}
		if refresh.Delivered {
			t.Fatal("a stale resource version was reported as delivered")
		}
		if fake.callCount("CreateJob") != 1 || fake.callCount("DeleteJob") != 0 {
			t.Fatal("stale credential update restarted the workload")
		}

		fake.clearInjection("PutSecret")
		retry, err := adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
			Identity:   request.Identity,
			Credential: "tok-new",
		})
		if err != nil || !retry.Delivered {
			t.Fatalf("retry after stale version = %+v, %v; want delivered", retry, err)
		}
	})

	t.Run("empty credential", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		putsBefore := fake.callCount("PutSecret")

		refresh, err := adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
			Identity:   request.Identity,
			Credential: "",
		})
		if err == nil {
			t.Fatal("RefreshCredential accepted an empty credential")
		}
		if refresh.Delivered {
			t.Fatal("empty credential was reported as delivered")
		}
		if fake.callCount("PutSecret") != putsBefore {
			t.Fatal("empty credential reached the API")
		}
		if fake.callCount("CreateJob") != 1 {
			t.Fatal("empty credential restarted the workload")
		}
	})
}

// TestAdapterReconstructionUsesPersistedIdentity proves a fresh adapter built
// from persisted identities and live API state observes, refreshes and starts
// the same workload without duplicating it.
func TestAdapterReconstructionUsesPersistedIdentity(t *testing.T) {
	fake, clock, _, request := setupStarted(t)
	fake.addPod(testRunningPod(request.Identity))

	second := newTestAdapter(t, fake, clock)
	observation, err := second.Observe(context.Background(), engine.ObserveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("reconstructed Observe returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationRunning {
		t.Fatalf("reconstructed observation = %q; want running", observation.Kind)
	}

	if _, err := second.Prepare(context.Background(), request); err != nil {
		t.Fatalf("reconstructed Prepare returned error: %v", err)
	}
	if fake.jobCount() != 1 {
		t.Fatalf("reconstructed Prepare changed the job count to %d", fake.jobCount())
	}

	retry, err := second.Start(context.Background(), engine.StartRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("reconstructed Start returned error: %v", err)
	}
	if retry.Outcome != engine.StartAccepted {
		t.Fatalf("reconstructed Start outcome = %q; want accepted", retry.Outcome)
	}
	if fake.jobCount() != 1 {
		t.Fatalf("reconstructed Start created a duplicate; cluster has %d jobs", fake.jobCount())
	}

	refresh, err := second.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   request.Identity,
		Credential: "tok-rebuilt",
	})
	if err != nil || !refresh.Delivered {
		t.Fatalf("reconstructed refresh = %+v, %v; want delivered", refresh, err)
	}
	if fake.jobCount() != 1 {
		t.Fatalf("reconstructed operations duplicated the job; cluster has %d", fake.jobCount())
	}
}

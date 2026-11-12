// Guarded Job-suspension stop behavior.
//
// An uncertain Kubernetes stop is confirmed by suspending the one stable Job,
// never by deleting it or by adding a pod finalizer. These tests drive the
// deterministic fake: a suspension PATCH records intent only, and a separate
// controller hook acknowledges it, moves the counters and removes the pod.
// Confirmation must therefore come from fresh Job and pod observations, not
// from the PATCH answer, the deletion of the Job or elapsed time.
package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

func stopRequest(request engine.PrepareRequest) engine.StopRequest {
	return engine.StopRequest{Identity: request.Identity, WorkspaceID: request.WorkspaceID}
}

func zeroCount() *int {
	zero := 0
	return &zero
}

func oneCount() *int {
	one := 1
	return &one
}

// acknowledgeSuspension is the controller reaction every confirming test needs:
// the same Job records Suspended=True, has no active and a present zero
// terminating count.
func acknowledgeSuspension(fake *fakeCluster, job Job) {
	fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
		stored.Status.Active = 0
		stored.Status.Terminating = zeroCount()
		stored.Status.Conditions = []JobCondition{{Type: "Suspended", Status: "True", Reason: "Suspended"}}
	})
}

// TestStopSuspendsSameJobWithoutDeleting proves Stop patches the observed Job
// UID instead of deleting the Job, so the durable object survives and a retry
// can observe the controller's acknowledgment.
func TestStopSuspendsSameJobWithoutDeleting(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	before := onlyJob(t, fake)
	if before.UID == "" {
		t.Fatal("started Job has no immutable UID")
	}
	fake.setSuspendHook(func() {
		acknowledgeSuspension(fake, before)
		fake.setPods(nil)
	})

	evidence, err := adapter.Stop(context.Background(), stopRequest(request))
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed suspension", evidence)
	}
	if fake.callCount("DeleteJob") != 0 {
		t.Fatalf("Stop issued %d Job deletions; want none", fake.callCount("DeleteJob"))
	}
	if fake.callCount("SuspendJob") != 1 {
		t.Fatalf("Stop issued %d suspension PATCHes; want exactly one", fake.callCount("SuspendJob"))
	}
	if fake.callCount("CreateJob") != 1 {
		t.Fatal("Stop created or replaced a Job")
	}
	if fake.jobCount() != 1 {
		t.Fatalf("cluster has %d Jobs after Stop; want the suspended Job to survive", fake.jobCount())
	}
	after := onlyJob(t, fake)
	if after.UID != before.UID {
		t.Fatalf("Job UID changed from %q to %q; Stop must patch the same Job", before.UID, after.UID)
	}
	if after.Spec.Suspend == nil || !*after.Spec.Suspend {
		t.Fatalf("Job spec.suspend = %v; want true", after.Spec.Suspend)
	}
	if after.DeletionTimestamp != nil {
		t.Fatalf("Stop left a deletion timestamp on the Job: %v", after.DeletionTimestamp)
	}
}

// TestStopSuspensionConfirmationMatrix proves every conjunct of the guarded
// confirmation branch is required: dropping one keeps the stop pending.
func TestStopSuspensionConfirmationMatrix(t *testing.T) {
	type testCase struct {
		name   string
		mutate func(*fakeCluster, Job)
		pods   func(engine.EffectIdentity) []Pod
		want   bool
	}
	cases := []testCase{
		{
			name: "acknowledged suspension with no pod",
			want: true,
		},
		{
			name: "already complete",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					stored.Status.Conditions = []JobCondition{{Type: "Complete", Status: "True"}}
				})
			},
			want: true,
		},
		{
			name: "already failed",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					stored.Status.Conditions = []JobCondition{{Type: "Failed", Status: "True"}}
				})
			},
			want: true,
		},
		{
			name: "no acknowledgment condition",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					stored.Status.Conditions = nil
				})
			},
		},
		{
			name: "false suspended condition",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					stored.Status.Conditions = []JobCondition{{Type: "Suspended", Status: "False"}}
				})
			},
		},
		{
			name: "active pod counted",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					stored.Status.Active = 1
				})
			},
		},
		{
			name: "terminating count absent",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					stored.Status.Terminating = nil
				})
			},
		},
		{
			name: "terminating count nonzero",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					stored.Status.Terminating = oneCount()
				})
			},
		},
		{
			name: "job deleting",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					deleted := testDeadline.Add(time.Minute)
					stored.DeletionTimestamp = &deleted
				})
			},
		},
		{
			name: "suspension resumed",
			mutate: func(fake *fakeCluster, job Job) {
				fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
					resumed := false
					stored.Spec.Suspend = &resumed
				})
			},
		},
		{
			name: "potentially live pod",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{testRunningPod(identity)}
			},
		},
		{
			name: "pending pod",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{testPod(identity, "Pending")}
			},
		},
		{
			name: "unknown pod phase",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{testPod(identity, "")}
			},
		},
		{
			name: "one affirmatively terminated pod",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{testTerminatedPod(identity, "Succeeded", 0)}
			},
			want: true,
		},
		{
			name: "two workload pods",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{
					testRunningPod(identity),
					testTerminatedPod(identity, "Succeeded", 0),
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter, request := setupStarted(t)
			before := onlyJob(t, fake)
			fake.setSuspendHook(func() {
				acknowledgeSuspension(fake, before)
				if testCase.mutate != nil {
					testCase.mutate(fake, before)
				}
				if testCase.pods != nil {
					fake.setPods(testCase.pods(request.Identity))
				}
			})

			evidence, err := adapter.Stop(context.Background(), stopRequest(request))
			if err != nil {
				t.Fatalf("Stop returned error: %v", err)
			}
			if evidence.IsConfirmed() != testCase.want {
				t.Fatalf("stop evidence = %+v; want confirmed=%v", evidence, testCase.want)
			}
			if evidence.Detail == "" && !evidence.IsConfirmed() {
				t.Fatal("pending stop has no attributable detail")
			}
			if fake.callCount("DeleteJob") != 0 {
				t.Fatalf("Stop issued %d Job deletions; want none", fake.callCount("DeleteJob"))
			}
			if fake.jobCount() != 1 {
				t.Fatalf("cluster has %d Jobs after Stop; want the suspending Job to survive", fake.jobCount())
			}
		})
	}
}

// TestStopRequiresFreshPodList proves the pod list cannot be skipped, cached
// from before the suspension, or failed open.
func TestStopRequiresFreshPodList(t *testing.T) {
	t.Run("listing failure stays pending", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		fake.setSuspendHook(func() {
			// A fully acknowledged, pod-free Job would confirm if the pod list
			// were skipped or failed open.
			acknowledgeSuspension(fake, before)
			fake.setPods(nil)
		})
		fake.injectBefore("ListPods", errors.New("api server unreachable"))

		evidence, err := adapter.Stop(context.Background(), stopRequest(request))
		if evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; a failed pod list must not confirm", evidence)
		}
		requireFault(t, err, fault.DependencyUnavailable)
		if fake.callCount("SuspendJob") != 1 {
			t.Fatalf("suspension PATCHes = %d; want the suspension intent before pod evidence", fake.callCount("SuspendJob"))
		}
	})

	t.Run("pod added after the patch is observed", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		fake.setSuspendHook(func() {
			// The Job looks settled but a pod exists: a cached pre-PATCH list
			// would wrongly confirm.
			acknowledgeSuspension(fake, before)
			fake.addPod(testRunningPod(request.Identity))
		})

		evidence, err := adapter.Stop(context.Background(), stopRequest(request))
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; a pod that appeared after the PATCH is potentially live", evidence)
		}
		if fake.callCount("SuspendJob") != 1 {
			t.Fatalf("suspension PATCHes = %d; want exactly one", fake.callCount("SuspendJob"))
		}
	})

	t.Run("listing is read after the suspension request", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		fake.setSuspendHook(func() {
			acknowledgeSuspension(fake, before)
			fake.setPods(nil)
		})
		if _, err := adapter.Stop(context.Background(), stopRequest(request)); err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		calls := fake.callsSnapshot()
		suspend := indexOfCall(calls, "SuspendJob")
		list := indexOfCall(calls, "ListPods")
		if suspend < 0 || list < 0 {
			t.Fatalf("calls = %v; want a suspension PATCH and a pod list", calls)
		}
		if list < suspend {
			t.Fatalf("pod list at index %d precedes the suspension PATCH at %d; evidence must be fresh", list, suspend)
		}
	})
}

// TestStopRejectsJobReplacementDuringConfirmation proves the UID guard covers
// both the PATCH request and the observations that confirm it.
func TestStopRejectsJobReplacementDuringConfirmation(t *testing.T) {
	t.Run("replacement after the patch", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		fake.setSuspendHook(func() {
			fake.removeJob(before.Namespace, before.Name)
			replacement := before
			replacement.UID = ""
			replacement.ResourceVersion = ""
			replacement.DeletionTimestamp = nil
			suspend := true
			replacement.Spec.Suspend = &suspend
			replacement.Status.Active = 0
			replacement.Status.Terminating = zeroCount()
			replacement.Status.Conditions = []JobCondition{{Type: "Suspended", Status: "True"}}
			fake.addJob(replacement)
			fake.setPods(nil)
		})

		evidence, err := adapter.Stop(context.Background(), stopRequest(request))
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if evidence.IsConfirmed() {
			t.Fatal("a replaced Job with a new UID confirmed the original stop")
		}
		if fake.jobCount() != 1 {
			t.Fatalf("jobs = %d; want the replacement only", fake.jobCount())
		}
		if fake.callCount("DeleteJob") != 0 {
			t.Fatal("Stop deleted the replacement Job")
		}
	})

	t.Run("replacement before the patch", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		gate := fake.newGate("SuspendJob")

		type outcome struct {
			evidence kernel.StopEvidence
			err      error
		}
		results := make(chan outcome, 1)
		go func() {
			evidence, err := adapter.Stop(context.Background(), stopRequest(request))
			results <- outcome{evidence: evidence, err: err}
		}()

		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop never reached the SuspendJob barrier")
		}
		fake.removeJob(before.Namespace, before.Name)
		replacement := before
		replacement.UID = ""
		replacement.ResourceVersion = ""
		fake.addJob(replacement)
		close(gate.release)

		got := <-results
		if got.evidence.IsConfirmed() {
			t.Fatal("a Job replaced before the PATCH confirmed the stop")
		}
		requireFault(t, got.err, fault.VersionConflict)
		if fake.callCount("DeleteJob") != 0 {
			t.Fatal("Stop deleted a Job instead of reporting the UID conflict")
		}
	})
}

// TestStopLostSuspendResponseRecoversWithFreshAdapter proves a lost suspension
// response stays pending and a reconstructed adapter confirms from the durable
// Job, not from in-process preparation state or the lost PATCH answer.
func TestStopLostSuspendResponseRecoversWithFreshAdapter(t *testing.T) {
	fake, clock, adapter, request := setupStarted(t)
	before := onlyJob(t, fake)
	fake.setSuspendHook(func() {
		acknowledgeSuspension(fake, before)
		fake.setPods(nil)
	})
	fake.injectAfter("SuspendJob", errors.New("connection reset after suspension"))

	evidence, err := adapter.Stop(context.Background(), stopRequest(request))
	if evidence.IsConfirmed() {
		t.Fatal("a lost suspension response was treated as a confirmed stop")
	}
	requireFault(t, err, fault.UncertainExternalEffect)
	stored, ok := fake.jobSnapshot(before.Namespace, before.Name)
	if !ok {
		t.Fatal("the suspension effect did not leave the Job present")
	}
	if stored.UID != before.UID {
		t.Fatalf("Job UID changed to %q after the lost response; want %q", stored.UID, before.UID)
	}
	if stored.Spec.Suspend == nil || !*stored.Spec.Suspend {
		t.Fatal("the lost-response effect did not record suspension intent")
	}

	fake.clearInjection("SuspendJob")
	fresh := newTestAdapter(t, fake, clock)
	recovered, err := fresh.Stop(context.Background(), stopRequest(request))
	if err != nil {
		t.Fatalf("recovered Stop returned error: %v", err)
	}
	if !recovered.IsConfirmed() {
		t.Fatalf("recovered stop evidence = %+v; want confirmation from the surviving Job", recovered)
	}
	if fake.callCount("DeleteJob") != 0 {
		t.Fatalf("Stop issued %d Job deletions; want none", fake.callCount("DeleteJob"))
	}
	if fake.jobCount() != 1 {
		t.Fatal("recovered Stop did not leave the same Job in place")
	}
}

// TestStopKeepsDefiniteSuspendRefusalDistinctFromAnUnknownOutcome proves a
// definite server refusal (the driver's dependency_unavailable class, for
// example an immutable-UID validation) is not reported as an uncertain effect:
// nothing was suspended and no Job was deleted. The kind component test pins
// the concrete wire class of a UID-mismatched PATCH; this case pins the
// adapter's handling of any definite refusal before a kill/retry decision.
func TestStopKeepsDefiniteSuspendRefusalDistinctFromAnUnknownOutcome(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	before := onlyJob(t, fake)
	fake.injectBefore("SuspendJob",
		fault.New(fault.DependencyUnavailable, "kubernetes suspend Job rejected with status 422"))

	evidence, err := adapter.Stop(context.Background(), stopRequest(request))
	if evidence.IsConfirmed() {
		t.Fatal("a refused suspension was treated as a confirmed stop")
	}
	requireFault(t, err, fault.DependencyUnavailable)
	if fake.callCount("DeleteJob") != 0 {
		t.Fatal("a refused suspension fell back to deleting the Job")
	}
	if stored := fake.jobMustGet(t, before.Namespace, before.Name); stored.UID != before.UID {
		t.Fatalf("Job UID changed from %q to %q after a refused suspension", before.UID, stored.UID)
	}
}

// TestStopMissingJobRemainsUnconfirmed proves a missing Job is never stop
// evidence, while the independent affirmative terminated-container path stays
// intact and a 404 PATCH keeps the driver's NotFound classification.
func TestStopMissingJobRemainsUnconfirmed(t *testing.T) {
	t.Run("absent job and no pods", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		fake.removeJob(before.Namespace, before.Name)

		evidence, err := adapter.Stop(context.Background(), stopRequest(request))
		if err != nil {
			t.Fatalf("Stop returned error for an absent Job: %v", err)
		}
		if evidence.IsConfirmed() {
			t.Fatal("an absent Job and empty pod list confirmed a stop")
		}
		if evidence.Detail == "" {
			t.Fatal("pending stop has no attributable detail")
		}
		if fake.callCount("SuspendJob") != 0 {
			t.Fatal("Stop attempted to suspend a missing Job")
		}
	})

	t.Run("absent job with a live pod", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		fake.removeJob(before.Namespace, before.Name)
		fake.addPod(testRunningPod(request.Identity))

		evidence, err := adapter.Stop(context.Background(), stopRequest(request))
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if evidence.IsConfirmed() {
			t.Fatal("a live orphan pod was accepted as stopped")
		}
	})

	t.Run("absent job with affirmative termination evidence", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		fake.removeJob(before.Namespace, before.Name)
		fake.addPod(testTerminatedPod(request.Identity, "Succeeded", 0))

		evidence, err := adapter.Stop(context.Background(), stopRequest(request))
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		if !evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; want the existing terminated-container path", evidence)
		}
	})

	t.Run("job disappears before the suspension patch", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		gate := fake.newGate("SuspendJob")

		type outcome struct {
			evidence kernel.StopEvidence
			err      error
		}
		results := make(chan outcome, 1)
		go func() {
			evidence, err := adapter.Stop(context.Background(), stopRequest(request))
			results <- outcome{evidence: evidence, err: err}
		}()

		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop never reached the SuspendJob barrier")
		}
		fake.removeJob(before.Namespace, before.Name)
		close(gate.release)

		got := <-results
		if got.evidence.IsConfirmed() {
			t.Fatal("a 404 suspension PATCH confirmed a stop")
		}
		requireFault(t, got.err, fault.NotFound)
	})
}

// markJobDeleting records a deletion request on the stored Job, as a TTL or
// foreground deletion that started before this Stop does.
func markJobDeleting(t *testing.T, fake *fakeCluster, namespace, name string) {
	t.Helper()
	fake.mutateJob(namespace, name, func(stored *Job) {
		deleted := testDeadline.Add(time.Minute)
		stored.DeletionTimestamp = &deleted
	})
}

// TestStopDeletingJobConsultsPodEvidence proves a Job that already carries a
// deletion timestamp is never reported pending without consulting the workload
// pods. The independent affirmatively-terminated-container path applies
// unchanged, absence of pods is never stop evidence, a potentially live pod
// stays pending, two pods stay pending, and the deleting path never suspends or
// deletes the Job.
func TestStopDeletingJobConsultsPodEvidence(t *testing.T) {
	cases := []struct {
		name string
		pods func(engine.EffectIdentity) []Pod
		want bool
	}{
		{
			name: "terminated pod confirms",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{testTerminatedPod(identity, "Succeeded", 0)}
			},
			want: true,
		},
		{
			name: "terminating pod with terminated container confirms",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{testTerminatingTerminatedPod(identity, "Failed", 7)}
			},
			want: true,
		},
		{
			name: "running pod stays pending",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{testRunningPod(identity)}
			},
		},
		{
			name: "terminating pod without termination evidence stays pending",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{testTerminatingPod(identity)}
			},
		},
		{
			name: "no pods stays pending",
		},
		{
			name: "two pods stay pending",
			pods: func(identity engine.EffectIdentity) []Pod {
				return []Pod{
					testTerminatedPod(identity, "Succeeded", 0),
					testRunningPod(identity),
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter, request := setupStarted(t)
			before := onlyJob(t, fake)
			markJobDeleting(t, fake, before.Namespace, before.Name)
			fake.setPods(nil)
			if testCase.pods != nil {
				fake.setPods(testCase.pods(request.Identity))
			}

			evidence, err := adapter.Stop(context.Background(), stopRequest(request))
			if err != nil {
				t.Fatalf("Stop returned error: %v", err)
			}
			if evidence.IsConfirmed() != testCase.want {
				t.Fatalf("stop evidence = %+v; want confirmed=%v", evidence, testCase.want)
			}
			if evidence.Detail == "" && !evidence.IsConfirmed() {
				t.Fatal("pending stop has no attributable detail")
			}
			if fake.callCount("ListPods") != 1 {
				t.Fatalf("pod listings = %d; want the deleting path to consult pods exactly once", fake.callCount("ListPods"))
			}
			if fake.callCount("SuspendJob") != 0 {
				t.Fatalf("suspension PATCHes = %d; a deleting Job is never patched", fake.callCount("SuspendJob"))
			}
			if fake.callCount("DeleteJob") != 0 {
				t.Fatalf("Job deletions = %d; Stop never deletes", fake.callCount("DeleteJob"))
			}
		})
	}

	t.Run("pod listing failure stays pending", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		markJobDeleting(t, fake, before.Namespace, before.Name)
		fake.injectBefore("ListPods", errors.New("api server unreachable"))

		evidence, err := adapter.Stop(context.Background(), stopRequest(request))
		if evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; an unreachable pod list must not confirm", evidence)
		}
		requireFault(t, err, fault.DependencyUnavailable)
	})

	t.Run("identity guard refuses a foreign deleting Job", func(t *testing.T) {
		fake, _, adapter, request := setupStarted(t)
		before := onlyJob(t, fake)
		fake.mutateJob(before.Namespace, before.Name, func(stored *Job) {
			stored.Labels[LabelWorkspace] = "workspace-someone-else"
		})
		markJobDeleting(t, fake, before.Namespace, before.Name)
		fake.setPods([]Pod{testTerminatedPod(request.Identity, "Succeeded", 0)})

		evidence, err := adapter.Stop(context.Background(), stopRequest(request))
		if evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; a foreign deleting Job must never confirm", evidence)
		}
		requireFault(t, err, fault.Forbidden)
		if fake.callCount("ListPods") != 0 {
			t.Fatalf("pod listings = %d; the identity guard must refuse before any pod evidence", fake.callCount("ListPods"))
		}
	})
}

// TestStopSuspensionClearsFinishedJobTTL proves a successful suspension leaves
// the Job without a finished-Job TTL, so a Job that finishes while suspended is
// not removed by the TTL controller before Remove deletes it.
func TestStopSuspensionClearsFinishedJobTTL(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	before := onlyJob(t, fake)
	if before.Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("started Job carries no finished-Job TTL to clear")
	}
	fake.setSuspendHook(func() {
		acknowledgeSuspension(fake, before)
		fake.setPods(nil)
	})

	evidence, err := adapter.Stop(context.Background(), stopRequest(request))
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed suspension", evidence)
	}
	after := onlyJob(t, fake)
	if after.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("suspended Job still carries ttlSecondsAfterFinished=%d; a finishing Job would be TTL-deleted",
			*after.Spec.TTLSecondsAfterFinished)
	}
	if after.Spec.Suspend == nil || !*after.Spec.Suspend {
		t.Fatalf("suspended Job spec.suspend = %v; want true", after.Spec.Suspend)
	}
	if after.UID != before.UID {
		t.Fatalf("Job UID changed from %q to %q", before.UID, after.UID)
	}
}

// TestRemoveStillDeletesSuspendedJobAndRetainsWorkspace proves Remove keeps its
// destructive cleanup duty and never releases the retained claim.
func TestRemoveStillDeletesSuspendedJobAndRetainsWorkspace(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	job := onlyJob(t, fake)
	fake.setSuspendHook(func() {
		acknowledgeSuspension(fake, job)
		fake.setPods(nil)
	})
	evidence, err := adapter.Stop(context.Background(), stopRequest(request))
	if err != nil || !evidence.IsConfirmed() {
		t.Fatalf("Stop = %+v, %v; want a confirmed suspension", evidence, err)
	}
	claimName := testClaimName(t, request.WorkspaceID)

	removal, err := adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want the suspended Job's resources removed", removal)
	}
	if !strings.Contains(removal.Detail, claimName) {
		t.Fatalf("removal detail = %q; want the retained claim named", removal.Detail)
	}
	if fake.jobCount() != 0 {
		t.Fatal("Remove left the suspended Job behind")
	}
	if len(fake.configMapsSnapshot()) != 0 || len(fake.secretsSnapshot()) != 0 {
		t.Fatal("Remove left activation resources behind")
	}
	if fake.callCount("DeleteJob") == 0 {
		t.Fatal("Remove did not delete the Job")
	}
	if !fake.claimExists(testNamespace, claimName) {
		t.Fatal("Remove deleted the retained workspace claim")
	}
	if fake.callCount("DeleteClaim") != 0 {
		t.Fatal("Remove attempted to delete the workspace claim")
	}
}

// TestObserveDoesNotTurnSuspendedJobIntoExit proves suspension never fabricates
// an exit status or a never-started proof.
func TestObserveDoesNotTurnSuspendedJobIntoExit(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	job := onlyJob(t, fake)
	fake.mutateJob(job.Namespace, job.Name, func(stored *Job) {
		suspend := true
		stored.Spec.Suspend = &suspend
		stored.Status.Active = 0
		stored.Status.Terminating = zeroCount()
		stored.Status.Conditions = []JobCondition{{Type: "Suspended", Status: "True"}}
	})
	fake.setPods(nil)

	observation, err := adapter.Observe(context.Background(), engine.ObserveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	if observation.Kind == kernel.ObservationExited {
		t.Fatalf("observation = %+v; suspension is not an observed exit", observation)
	}
	if observation.StopConfirmed() {
		t.Fatalf("observation = %+v; suspension alone must not confirm the stop barrier", observation)
	}
	if observation.ProofOfNonexecution() {
		t.Fatalf("observation = %+v; suspension is never proof the command did not run", observation)
	}
	if _, ok := observation.ExitStatus(); ok {
		t.Fatalf("observation = %+v; suspension fabricated an exit status", observation)
	}
}

// requireFault asserts the stable public fault class.
func requireFault(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v carries no fault class; want %s", err, want)
	}
	if class != want {
		t.Fatalf("fault class = %s; want %s (error %v)", class, want, err)
	}
}

// indexOfCall reports the first recorded call of one operation, or -1.
func indexOfCall(calls []string, operation string) int {
	for index, call := range calls {
		if call == operation {
			return index
		}
	}
	return -1
}

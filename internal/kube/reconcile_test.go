package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

func (e *env) reconciler(t *testing.T, sink *kubetest.Sink, opts ...func(*kube.Options)) *kube.Reconciler {
	t.Helper()
	o := kube.Options{Clock: e.clock.Now, OrphanGrace: time.Minute, CompletedRetention: 10 * time.Minute}
	for _, f := range opts {
		f(&o)
	}
	r, err := kube.NewReconciler(kube.Dependencies{Policy: e.policy, Cluster: e.fake, Attempts: e.attempts, Observations: sink, Tenants: kubetest.Tenants{e.tenant, "t021_stranger"}}, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) dispatched(t *testing.T) {
	t.Helper()
	if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
		t.Fatal(err)
	}
	e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.DispatchConfirmed = true })
}

func (e *env) pass(t *testing.T, r *kube.Reconciler) kube.Report {
	t.Helper()
	rep, err := r.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	return rep
}

// Test_reconcile_observations: verified evidence is reported through the
// observation port; supported classes only; nothing is inferred from Job
// completion or exit zero.
func Test_reconcile_observations(t *testing.T) {
	t.Run("primary start", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		sink := &kubetest.Sink{}
		r := e.reconciler(t, sink)
		e.pass(t, r)
		if len(sink.Obs) != 0 {
			t.Fatalf("observed before the Pod: %v", sink.Kinds())
		}
		pod := e.started(t)
		_ = pod
		rep := e.pass(t, r)
		if rep.Started != 1 || len(sink.Obs) != 1 {
			t.Fatalf("%+v %v", rep, sink.Kinds())
		}
		o := sink.Obs[0]
		job := e.mustJob(t)
		if o.Kind != execution.ObservedStarted || o.WorkloadUID != job.Metadata.UID || o.WorkloadName != e.rec.JobName ||
			o.SpecFingerprint != e.rec.SpecFingerprint || o.Launch != execution.LaunchStarted {
			t.Fatalf("%+v", o)
		}
		// Once execution recorded the start, it is not reported again.
		now := e.clock.Now()
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
			r.StartedAt = &now
			r.ObservedJobUID = job.Metadata.UID
			r.Launch = execution.LaunchStarted
		})
		e.pass(t, r)
		if len(sink.Obs) != 1 {
			t.Fatalf("repeated: %v", sink.Kinds())
		}
	})

	t.Run("a same-name replacement Job is not the attempt's workload", func(t *testing.T) {
		for name, recorded := range map[string]string{
			"confirmed by the admission record":  "secret",
			"observed by execution at the start": "attempt",
		} {
			for pname, pod := range map[string]func(*kubernetes.Pod){
				"started primary": func(p *kubernetes.Pod) {
					yes := true
					p.Status.Phase = "Running"
					p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, Started: &yes, State: kubernetes.ContainerState{Running: &kubernetes.ContainerRunning{}}}}
				},
				"terminated primary": func(p *kubernetes.Pod) {
					p.Status.Phase = "Failed"
					p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 13}}}}
				},
			} {
				t.Run(name+"/"+pname, func(t *testing.T) {
					e := newEnv(t)
					e.dispatched(t)
					orig := e.mustJob(t)
					if recorded == "attempt" {
						// Execution recorded the start; the admission record is gone from view.
						now := e.clock.Now()
						e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
							r.StartedAt, r.ObservedJobUID, r.Launch = &now, orig.Metadata.UID, execution.LaunchStarted
						})
					}
					// Replace the Job with an identical one: same name and spec, new UID.
					e.fake.DropJob(e.ns(), e.rec.JobName)
					var repl kubernetes.Job
					raw, _ := jsonMarshal(e.exp.Job)
					jsonUnmarshal(t, raw, &repl)
					repl = e.fake.PutJob(e.ns(), repl)
					if repl.Metadata.UID == orig.Metadata.UID {
						t.Fatal("fixture UID did not change")
					}
					e.fake.AddPod(e.ns(), repl, pod)
					sink := &kubetest.Sink{}
					rep, err := e.reconciler(t, sink).Pass(context.Background())
					if !errors.Is(err, kube.ErrForeignWorkload) {
						t.Fatalf("got %v", err)
					}
					if len(sink.Obs) != 0 || rep.Started != 0 || rep.Failed != 0 {
						t.Fatalf("the replacement's status reached the attempt: %+v %v", rep, sink.Kinds())
					}
					if got, ok := e.job(); !ok || got.Metadata.UID != repl.Metadata.UID || e.fake.Calls("delete job") != 0 {
						t.Fatal("the replacement was touched")
					}
				})
			}
		}
	})

	t.Run("pending, unschedulable and image pull failures are not failures", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.fake.AddPod(e.ns(), e.mustJob(t), func(p *kubernetes.Pod) {
			p.Status.Phase = "Pending"
			p.Status.Conditions = []kubernetes.PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable"}}
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Waiting: &kubernetes.ContainerWaiting{Reason: "ImagePullBackOff"}}}}
		})
		sink := &kubetest.Sink{}
		e.pass(t, e.reconciler(t, sink))
		if len(sink.Obs) != 0 {
			t.Fatalf("reported %v: startup deadlines belong to execution", sink.Kinds())
		}
	})

	t.Run("completion and exit zero never become success", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		job := e.mustJob(t)
		yes := true
		e.fake.AddPod(e.ns(), job, func(p *kubernetes.Pod) {
			p.Status.Phase = "Succeeded"
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, Started: &yes, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 0}}}}
		})
		e.fake.SetJobStatus(e.ns(), e.rec.JobName, func(s *kubernetes.JobStatus) {
			s.Succeeded = 1
			s.Conditions = []kubernetes.JobCondition{{Type: "Complete", Status: "True"}}
		})
		sink := &kubetest.Sink{}
		r := e.reconciler(t, sink)
		rep := e.pass(t, r)
		for _, o := range sink.Obs {
			if o.Kind == execution.ObservedFailed || o.Kind == execution.ObservedTerminated {
				t.Fatalf("Job completion became %v", o.Kind)
			}
		}
		if rep.Failed != 0 || rep.JobsDeleted != 0 {
			t.Fatalf("%+v", rep)
		}
		// The attempt is still running (no accepted result): nothing is cleaned up.
		if _, ok := e.job(); !ok {
			t.Fatal("a completed Job was deleted before its result was finalized")
		}
	})

	t.Run("failed workload", func(t *testing.T) {
		for name, tc := range map[string]struct {
			edit   func(*kubernetes.Pod)
			launch execution.LaunchEvidence
		}{
			"nonzero exit after start": {func(p *kubernetes.Pod) {
				p.Status.Phase = "Failed"
				p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 13}}}}
			}, execution.LaunchStarted},
			"injector failure": {func(p *kubernetes.Pod) {
				p.Status.Phase = "Failed"
				p.Spec.RestartPolicy = "Never"
				p.Status.InitContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.InjectContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 1}}}}
			}, execution.LaunchNotStarted},
			"evicted before start": {func(p *kubernetes.Pod) { p.Status.Phase = "Failed"; p.Status.Reason = "Evicted" }, execution.LaunchUnknown},
		} {
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.dispatched(t)
				e.fake.AddPod(e.ns(), e.mustJob(t), tc.edit)
				sink := &kubetest.Sink{}
				rep := e.pass(t, e.reconciler(t, sink))
				var failed *execution.Observation
				for i := range sink.Obs {
					if sink.Obs[i].Kind == execution.ObservedFailed {
						failed = &sink.Obs[i]
					}
				}
				if failed == nil || rep.Failed != 1 {
					t.Fatalf("no failure reported: %v", sink.Kinds())
				}
				if failed.Class != execution.ClassInfrastructure || failed.Reason != kube.ReasonWorkloadFailed || failed.Launch != tc.launch {
					t.Fatalf("%+v", *failed)
				}
				if failed.Validate() != nil {
					t.Fatalf("not a supported observation: %v", failed.Validate())
				}
			})
		}
	})

	t.Run("lost and replaced workloads", func(t *testing.T) {
		// Job gone after confirmed dispatch.
		e := newEnv(t)
		e.dispatched(t)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		sink := &kubetest.Sink{}
		e.pass(t, e.reconciler(t, sink))
		if len(sink.Obs) != 1 || sink.Obs[0].Kind != execution.ObservedFailed || sink.Obs[0].Reason != kube.ReasonWorkloadLost || sink.Obs[0].Launch != execution.LaunchUnknown {
			t.Fatalf("%+v", sink.Obs)
		}
		if e.fake.Calls("create job") != 1 {
			t.Fatal("a disappeared confirmed workload was recreated")
		}
		// Before confirmation, absence is the executor's redrive, not a loss.
		e2 := newEnv(t)
		sink2 := &kubetest.Sink{}
		e2.pass(t, e2.reconciler(t, sink2))
		if len(sink2.Obs) != 0 {
			t.Fatal("reported a loss before dispatch was confirmed")
		}
		// Started, then the Pod disappears.
		e3 := newEnv(t)
		e3.dispatched(t)
		e3.started(t)
		now := e3.clock.Now()
		e3.attempts.Edit(e3.rec.AttemptID, func(r *kube.AttemptRecord) { r.StartedAt = &now; r.Launch = execution.LaunchStarted })
		pods, _ := e3.fake.ListPods(context.Background(), e3.ns(), "")
		_ = e3.fake.DeletePod(context.Background(), e3.ns(), pods[0].Metadata.Name, kubernetes.DeleteOptions{UID: pods[0].Metadata.UID})
		sink3 := &kubetest.Sink{}
		e3.pass(t, e3.reconciler(t, sink3))
		if len(sink3.Obs) != 1 || sink3.Obs[0].Reason != kube.ReasonWorkloadLost || sink3.Obs[0].Launch != execution.LaunchStarted {
			t.Fatalf("%+v", sink3.Obs)
		}
		// A second Pod is a replacement, not a retry.
		e4 := newEnv(t)
		e4.dispatched(t)
		e4.started(t)
		e4.started(t)
		sink4 := &kubetest.Sink{}
		e4.pass(t, e4.reconciler(t, sink4))
		var reasons []execution.Reason
		for _, o := range sink4.Obs {
			if o.Kind == execution.ObservedFailed {
				reasons = append(reasons, o.Reason)
			}
		}
		if len(reasons) != 1 || reasons[0] != kube.ReasonWorkloadReplaced {
			t.Fatalf("%v", reasons)
		}
	})

	t.Run("a foreign Job is not observed", func(t *testing.T) {
		e := newEnv(t)
		var j kubernetes.Job
		raw, _ := jsonMarshal(e.exp.Job)
		jsonUnmarshal(t, raw, &j)
		j.Spec.Template.Spec.HostNetwork = true
		foreign := e.fake.PutJob(e.ns(), j)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.DispatchConfirmed = true })
		e.fake.AddPod(e.ns(), foreign, func(p *kubernetes.Pod) {
			p.Status.Phase = "Failed"
		})
		sink := &kubetest.Sink{}
		r := e.reconciler(t, sink)
		_, err := r.Pass(context.Background())
		if err == nil {
			t.Fatal("a foreign object should be reported as an error")
		}
		if len(sink.Obs) != 0 {
			t.Fatalf("observed a foreign workload: %v", sink.Kinds())
		}
	})

	t.Run("outages conclude nothing", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "get job" {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
			}
			return nil
		}
		sink := &kubetest.Sink{}
		if _, err := e.reconciler(t, sink).Pass(context.Background()); err == nil {
			t.Fatal("outage swallowed")
		}
		if len(sink.Obs) != 0 {
			t.Fatalf("reported %v on an outage", sink.Kinds())
		}
		e.fake.Fault = nil
		e.attempts.Err = errors.New("db down")
		if _, err := e.reconciler(t, sink).Pass(context.Background()); err == nil {
			t.Fatal("store outage swallowed")
		}
	})

	t.Run("report errors are surfaced and retried", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.started(t)
		sink := &kubetest.Sink{Err: errors.New("execution down")}
		r := e.reconciler(t, sink)
		if _, err := r.Pass(context.Background()); err == nil {
			t.Fatal("swallowed")
		}
		sink.Err = nil
		e.pass(t, r)
		if len(sink.Obs) != 1 {
			t.Fatalf("%v", sink.Kinds())
		}
	})

	t.Run("tenants without a target are skipped, policy tenants are scanned", func(t *testing.T) {
		e := newEnv(t)
		sink := &kubetest.Sink{}
		rep := e.pass(t, e.reconciler(t, sink))
		if rep.Tenants != 1 {
			t.Fatalf("%+v", rep)
		}
	})
}

// Test_reconcile_cleanup: late creates and leftovers of terminal attempts are
// removed; only proven-owned objects, only after authoritative lookup.
func Test_reconcile_cleanup(t *testing.T) {
	ctx := context.Background()
	t.Run("late create after termination was confirmed", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
			r.Status, r.Live, r.TerminationRequested, r.TerminationConfirmed = execution.AttemptCancelled, false, true, true
		})
		rep := e.pass(t, e.reconciler(t, &kubetest.Sink{}))
		if rep.JobsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
		if _, ok := e.job(); ok {
			t.Fatal("the late Job survives")
		}
	})
	t.Run("before termination is confirmed execution's request owns the deletion", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.attempts.Cancel(e.rec.AttemptID)
		rep := e.pass(t, e.reconciler(t, &kubetest.Sink{}))
		if rep.JobsDeleted != 0 {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("a live attempt's workload is never removed", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.clock.Advance(time.Hour)
		rep := e.pass(t, e.reconciler(t, &kubetest.Sink{}))
		if rep.JobsDeleted+rep.SecretsDeleted+rep.PodsDeleted != 0 {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("completed workloads are retained until settled and the window passes", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.Status, r.Live = execution.AttemptSucceeded, false })
		r := e.reconciler(t, &kubetest.Sink{})
		if rep := e.pass(t, r); rep.JobsDeleted != 0 {
			t.Fatalf("deleted before settlement: %+v", rep)
		}
		settled := e.clock.Now()
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.Settled, r.SettledAt = true, &settled })
		if rep := e.pass(t, r); rep.JobsDeleted != 0 {
			t.Fatalf("deleted inside the retention window: %+v", rep)
		}
		e.clock.Advance(11 * time.Minute)
		rep := e.pass(t, r)
		if rep.JobsDeleted != 1 {
			t.Fatalf("not deleted after the window: %+v", rep)
		}
		// The admission Secret goes once the Job is absent and the attempt settled.
		rep2 := e.pass(t, r)
		if rep.SecretsDeleted+rep2.SecretsDeleted != 1 {
			t.Fatalf("secret: %+v %+v", rep, rep2)
		}
		if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); ok {
			t.Fatal("the admission Secret survives")
		}
	})
	t.Run("unsettled attempts keep their recovery Secret", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
			r.Status, r.Live, r.TerminationConfirmed = execution.AttemptFailed, false, true
		})
		r := e.reconciler(t, &kubetest.Sink{})
		e.pass(t, r) // deletes the Job
		e.pass(t, r)
		if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); !ok {
			t.Fatal("the create-recovery record was deleted before settlement")
		}
	})
	t.Run("orphans need an authoritative absence and age", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.attempts.Remove(e.rec.AttemptID)
		r := e.reconciler(t, &kubetest.Sink{})
		if rep := e.pass(t, r); rep.JobsDeleted != 0 || rep.SecretsDeleted != 0 {
			t.Fatalf("a young object was removed: %+v", rep)
		}
		e.clock.Advance(2 * time.Minute)
		// Database outage: not orphan evidence.
		e.attempts.Err = errors.New("db down")
		if _, err := r.Pass(ctx); err == nil {
			t.Fatal("outage swallowed")
		}
		if _, ok := e.job(); !ok {
			t.Fatal("deleted on an outage")
		}
		e.attempts.Err = nil
		rep := e.pass(t, r)
		if rep.JobsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
		e.pass(t, r)
		if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); ok {
			t.Fatal("the orphan Secret survives")
		}
	})
	t.Run("foreign objects are neither adopted nor deleted", func(t *testing.T) {
		e := newEnv(t)
		e.clock.Advance(time.Hour)
		foreign := e.fake.PutJob(e.ns(), kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: "someone-elses", Labels: map[string]string{kube.LabelManagedBy: kube.ValueManagedBy}}})
		// Right labels, wrong name derivation / no annotations.
		spoof := e.exp.Job
		spoof.Metadata = kubernetes.ObjectMeta{Name: "kdm-" + strings.Repeat("a", 40), Labels: e.exp.Job.Metadata.Labels, Annotations: map[string]string{
			kube.AnnTenant: string(e.tenant), kube.AnnProject: "p_inv", kube.AnnAttempt: "att_ghost"}}
		e.fake.PutJob(e.ns(), spoof)
		rep, _ := e.reconciler(t, &kubetest.Sink{}).Pass(ctx)
		if rep.JobsDeleted != 0 {
			t.Fatalf("%+v", rep)
		}
		if got, ok := e.fake.JobOf(e.ns(), "someone-elses"); !ok || got.Metadata.UID != foreign.Metadata.UID {
			t.Fatal("a foreign Job was touched")
		}
		// A Job owned by a CronJob with our labels is not ours either.
		owned := e.exp.Job
		owned.Metadata.OwnerReferences = []kubernetes.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: "c", UID: "u"}}
		e.fake.PutJob(e.ns(), owned)
		rep2, _ := e.reconciler(t, &kubetest.Sink{}).Pass(ctx) // the owned object also fails verification
		if rep2.JobsDeleted != 0 {
			t.Fatalf("%+v", rep2)
		}
	})
	t.Run("a leftover Pod without its Job is deleted by UID", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		job := e.mustJob(t)
		pod := e.fake.AddPod(e.ns(), job, nil)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
			r.Status, r.Live, r.TerminationConfirmed = execution.AttemptCancelled, false, true
		})
		r := e.reconciler(t, &kubetest.Sink{})
		if rep := e.pass(t, r); rep.PodsDeleted != 0 {
			t.Fatalf("a young Pod was deleted: %+v", rep)
		}
		e.clock.Advance(2 * time.Minute)
		_ = pod
		// The fake stamps creation time at AddPod: age passes with the clock.
		if rep := e.pass(t, r); rep.PodsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("deletion failures are reported and retried", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
			r.Status, r.Live, r.TerminationConfirmed = execution.AttemptCancelled, false, true
		})
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "delete job" && n == 1 {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
			}
			return nil
		}
		r := e.reconciler(t, &kubetest.Sink{})
		if _, err := r.Pass(ctx); err == nil {
			t.Fatal("a lost delete response is not success")
		}
		e.pass(t, r)
		if _, ok := e.job(); ok {
			t.Fatal("the Job survives")
		}
	})
}

// terminalCancelled marks the attempt cancelled with termination confirmed, as
// execution leaves it before reconciliation sweeps leftovers.
func (e *env) terminalCancelled(settled bool) {
	e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
		r.Status, r.Live, r.TerminationRequested, r.TerminationConfirmed = execution.AttemptCancelled, false, true, true
		if settled {
			now := e.clock.Now()
			r.Settled, r.SettledAt = true, &now
		}
	})
}

// replaceJob drops the attempt's Job and installs an identical one with a new
// UID.
func (e *env) replaceJob(t *testing.T) kubernetes.Job {
	t.Helper()
	e.fake.DropJob(e.ns(), e.rec.JobName)
	var j kubernetes.Job
	raw, _ := jsonMarshal(e.exp.Job)
	jsonUnmarshal(t, raw, &j)
	return e.fake.PutJob(e.ns(), j)
}

// Test_reconcile_orphan_ownership: labels, deterministic names, annotations and
// age are copyable. Orphan cleanup deletes only what the executor's own recovery
// record or execution's committed observation names by UID.
func Test_reconcile_orphan_ownership(t *testing.T) {
	ctx := context.Background()

	t.Run("forged orphan Job without a recovery record is left untouched", func(t *testing.T) {
		e := newEnv(t)
		var j kubernetes.Job
		raw, _ := jsonMarshal(e.exp.Job)
		jsonUnmarshal(t, raw, &j)
		forged := e.fake.PutJob(e.ns(), j) // right name, labels and annotations; no Secret, no attempt
		e.attempts.Remove(e.rec.AttemptID)
		e.clock.Advance(time.Hour)
		rep, err := e.reconciler(t, &kubetest.Sink{}).Pass(ctx)
		if err != nil || rep.JobsDeleted != 0 || rep.Skipped == 0 {
			t.Fatalf("%+v %v", rep, err)
		}
		if got, ok := e.job(); !ok || got.Metadata.UID != forged.Metadata.UID || e.fake.Calls("delete job") != 0 {
			t.Fatal("the unverifiable Job was touched")
		}
	})

	t.Run("a same-name replacement Job does not inherit the orphan's record", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		repl := e.replaceJob(t) // the record confirmed the old UID
		e.attempts.Remove(e.rec.AttemptID)
		e.clock.Advance(time.Hour)
		rep, err := e.reconciler(t, &kubetest.Sink{}).Pass(ctx)
		if err != nil || rep.JobsDeleted != 0 {
			t.Fatalf("%+v %v", rep, err)
		}
		if got, ok := e.job(); !ok || got.Metadata.UID != repl.Metadata.UID {
			t.Fatal("the replacement was deleted")
		}
	})

	t.Run("a record naming a different attempt does not authorize deletion", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		s := e.secret(t)
		s.Metadata.Annotations = map[string]string{}
		for k, v := range e.secret(t).Metadata.Annotations {
			s.Metadata.Annotations[k] = v
		}
		s.Metadata.Annotations[kube.AnnWorkload] = `{"v":1,"attempt":"att_other","phase":"owned","uid":"` + e.mustJob(t).Metadata.UID + `"}`
		if _, err := e.fake.UpdateSecret(ctx, e.ns(), s); err != nil {
			t.Fatal(err)
		}
		e.attempts.Remove(e.rec.AttemptID)
		e.clock.Advance(time.Hour)
		rep, _ := e.reconciler(t, &kubetest.Sink{}).Pass(ctx)
		if rep.JobsDeleted != 0 {
			t.Fatalf("%+v", rep)
		}
		if _, ok := e.job(); !ok {
			t.Fatal("deleted on a foreign record")
		}
	})

	t.Run("forged orphan Pod is left untouched", func(t *testing.T) {
		for name, setup := range map[string]func(*env, kubernetes.Job){
			"no recovery record": func(e *env, _ kubernetes.Job) { e.lostSecret(t) },
			"owner UID the record never confirmed": func(e *env, _ kubernetes.Job) {
				// Pods of a Job UID the record does not name are not its leftovers.
			},
		} {
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.dispatched(t)
				job := e.mustJob(t)
				ghost := kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: e.rec.JobName, UID: "11111111-1111-4111-8111-111111111111"}}
				ghost.Spec.Template.Metadata.Labels = map[string]string{kube.LabelJob: e.rec.JobName, kube.LabelManagedBy: kube.ValueManagedBy}
				e.fake.AddPod(e.ns(), ghost, nil)
				setup(e, job)
				e.fake.DropJob(e.ns(), e.rec.JobName)
				e.terminalCancelled(false)
				e.clock.Advance(time.Hour)
				rep, err := e.reconciler(t, &kubetest.Sink{}).Pass(ctx)
				if err != nil || rep.PodsDeleted != 0 || rep.Skipped == 0 {
					t.Fatalf("%+v %v", rep, err)
				}
				if e.fake.Calls("delete pod") != 0 {
					t.Fatal("an uncorroborated Pod was deleted")
				}
			})
		}
	})

	t.Run("a leftover Pod of the recorded UID is deleted, of a live attempt is not", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.fake.AddPod(e.ns(), e.mustJob(t), nil)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.clock.Advance(time.Hour)
		r := e.reconciler(t, &kubetest.Sink{})
		if rep, _ := r.Pass(ctx); rep.PodsDeleted != 0 {
			t.Fatalf("a live attempt's Pod was deleted: %+v", rep)
		}
		e.terminalCancelled(false)
		if rep := e.pass(t, r); rep.PodsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
	})

	t.Run("a leftover Pod is proven by execution's observed UID when the Secret is lost", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		job := e.mustJob(t)
		e.fake.AddPod(e.ns(), job, nil)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.lostSecret(t)
		e.commitStart(job.Metadata.UID)
		e.terminalCancelled(false)
		e.clock.Advance(time.Hour)
		if rep := e.pass(t, e.reconciler(t, &kubetest.Sink{})); rep.PodsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
	})

	t.Run("a Pod of a replaced Job is a leftover of the recorded UID", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		old := e.mustJob(t)
		oldPod := e.fake.AddPod(e.ns(), old, nil)
		repl := e.replaceJob(t)
		replPod := e.fake.AddPod(e.ns(), repl, nil)
		e.terminalCancelled(false)
		e.clock.Advance(time.Hour)
		rep, _ := e.reconciler(t, &kubetest.Sink{}).Pass(ctx)
		if rep.PodsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
		if _, err := e.fake.GetPod(ctx, e.ns(), oldPod.Metadata.Name); err == nil {
			t.Fatal("the recorded UID's Pod survives")
		}
		if _, err := e.fake.GetPod(ctx, e.ns(), replPod.Metadata.Name); err != nil {
			t.Fatal("the replacement's Pod was deleted")
		}
	})

	t.Run("lookup outages are not ownership evidence", func(t *testing.T) {
		for _, op := range []string{"get secret", "get job"} {
			e := newEnv(t)
			e.dispatched(t)
			e.fake.AddPod(e.ns(), e.mustJob(t), nil)
			e.fake.DropJob(e.ns(), e.rec.JobName)
			e.terminalCancelled(false)
			e.clock.Advance(time.Hour)
			e.fake.Fault = func(o string, n int) *kubetest.Fault {
				if o == op {
					return &kubetest.Fault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnavailable}}
				}
				return nil
			}
			if _, err := e.reconciler(t, &kubetest.Sink{}).Pass(ctx); err == nil {
				t.Fatalf("%s outage swallowed", op)
			}
			if e.fake.Calls("delete pod") != 0 {
				t.Fatalf("%s outage became deletion evidence", op)
			}
		}
		e := newEnv(t)
		e.dispatched(t)
		e.fake.AddPod(e.ns(), e.mustJob(t), nil)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.attempts.Remove(e.rec.AttemptID)
		e.clock.Advance(time.Hour)
		e.attempts.Err = errors.New("db down")
		if _, err := e.reconciler(t, &kubetest.Sink{}).Pass(ctx); err == nil {
			t.Fatal("database outage swallowed")
		}
		if e.fake.Calls("delete pod") != 0 {
			t.Fatal("a database outage became orphan evidence")
		}
	})

	t.Run("the recovery record is kept until the owned Pods are gone", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.fake.AddPod(e.ns(), e.mustJob(t), nil)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.terminalCancelled(true) // settled: only the Pod proof remains
		r := e.reconciler(t, &kubetest.Sink{})
		rep := e.pass(t, r) // the Pod is young and survives; the record must too
		if rep.SecretsDeleted != 0 || rep.PodsDeleted != 0 {
			t.Fatalf("%+v", rep)
		}
		if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); !ok {
			t.Fatal("the record was deleted while an owned Pod remained")
		}
		e.clock.Advance(2 * time.Minute)
		rep = e.pass(t, r)
		if rep.PodsDeleted != 1 || rep.SecretsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
		if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); ok {
			t.Fatal("the record outlives its Pods")
		}
	})

	t.Run("a late create records its UID so its Pods stay provable", func(t *testing.T) {
		e := newEnv(t)
		// The create lands but its response and the read-back are lost: the record
		// holds create_issued without a UID.
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			switch {
			case op == "create job":
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
			case op == "get job" && n >= 2:
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
			}
			return nil
		}
		if _, err := e.runtime(t).Start(ctx, e.intent()); err == nil {
			t.Fatal("expected an uncertain dispatch")
		}
		e.fake.Fault = nil
		if uid, _ := e.record(t)["uid"].(string); uid != "" {
			t.Fatalf("the UID was recorded without verification: %v", e.record(t))
		}
		late := e.mustJob(t)
		e.terminalCancelled(false)
		r := e.reconciler(t, &kubetest.Sink{})
		if rep := e.pass(t, r); rep.JobsDeleted != 1 || rep.SecretsDeleted != 0 {
			t.Fatalf("%+v", rep)
		}
		if got := e.record(t)["uid"]; got != late.Metadata.UID {
			t.Fatalf("record did not capture the deleted UID: %v", e.record(t))
		}
		// The garbage collector lags: the Pod outlives its Job.
		pod := e.fake.AddPod(e.ns(), late, nil)
		e.terminalCancelled(true)
		if rep := e.pass(t, r); rep.SecretsDeleted != 0 || rep.PodsDeleted != 0 {
			t.Fatalf("%+v", rep)
		}
		e.clock.Advance(2 * time.Minute)
		if rep := e.pass(t, r); rep.PodsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
		if _, err := e.fake.GetPod(ctx, e.ns(), pod.Metadata.Name); err == nil {
			t.Fatal("the late Job's Pod survives")
		}
	})
}

// Test_reconcile_committed_uid: execution's observed UID protects a same-name
// replacement from terminal cleanup even when the executor's own recovery record
// is gone; only a Job carrying the committed UID, or an initial create with no
// committed UID that verifies against the committed recipe, is deletable.
func Test_reconcile_committed_uid(t *testing.T) {
	t.Run("a replacement is not deletable by identity of spec alone", func(t *testing.T) {
		for _, secretLost := range []bool{true, false} {
			e := newEnv(t)
			e.dispatched(t)
			old := e.mustJob(t)
			e.commitStart(old.Metadata.UID)
			repl := e.replaceJob(t)
			if secretLost {
				e.lostSecret(t)
			} else {
				e.terminalCancelled(true)
			}
			e.terminalCancelled(true)
			e.clock.Advance(time.Hour)
			rep, err := e.reconciler(t, &kubetest.Sink{}).Pass(context.Background())
			if err != nil || rep.JobsDeleted != 0 || rep.Skipped == 0 {
				t.Fatalf("secretLost=%v: %+v %v", secretLost, rep, err)
			}
			if got, ok := e.job(); !ok || got.Metadata.UID != repl.Metadata.UID || e.fake.Calls("delete job") != 0 {
				t.Fatalf("secretLost=%v: the replacement was touched", secretLost)
			}
		}
	})
	t.Run("the committed UID's own Job is deleted after the Secret is lost", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		job := e.mustJob(t)
		e.commitStart(job.Metadata.UID)
		e.lostSecret(t)
		e.terminalCancelled(false)
		rep := e.pass(t, e.reconciler(t, &kubetest.Sink{}))
		if rep.JobsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
		if _, ok := e.job(); ok {
			t.Fatal("the committed Job survives")
		}
	})
	t.Run("a late create is recorded under the UID execution observed, and a replacement is not", func(t *testing.T) {
		for _, replaced := range []bool{false, true} {
			e := newEnv(t)
			// The create lands but its response and read-back are lost: the record
			// holds create_issued without a UID.
			e.fake.Fault = func(op string, n int) *kubetest.Fault {
				switch {
				case op == "create job":
					return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
				case op == "get job" && n >= 2:
					return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
				}
				return nil
			}
			if _, err := e.runtime(t).Start(context.Background(), e.intent()); err == nil {
				t.Fatal("expected an uncertain dispatch")
			}
			e.fake.Fault = nil
			late := e.mustJob(t)
			e.commitStart(late.Metadata.UID)
			e.terminalCancelled(false)
			live := late
			if replaced {
				live = e.replaceJob(t)
			}
			rep := e.pass(t, e.reconciler(t, &kubetest.Sink{}))
			if replaced {
				if rep.JobsDeleted != 0 || e.fake.Calls("delete job") != 0 {
					t.Fatalf("the replacement was deleted: %+v", rep)
				}
				if got, ok := e.job(); !ok || got.Metadata.UID != live.Metadata.UID {
					t.Fatal("the replacement is gone")
				}
				continue
			}
			if rep.JobsDeleted != 1 {
				t.Fatalf("%+v", rep)
			}
			if got := e.record(t)["uid"]; got != late.Metadata.UID {
				t.Fatalf("record did not capture the deleted UID: %v", e.record(t))
			}
		}
	})
	t.Run("a delete is preconditioned on the UID and retried after a transient failure", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		job := e.mustJob(t)
		e.commitStart(job.Metadata.UID)
		e.terminalCancelled(false)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "delete job" && n == 1 {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
			}
			return nil
		}
		r := e.reconciler(t, &kubetest.Sink{})
		if _, err := r.Pass(context.Background()); err == nil {
			t.Fatal("the failed delete was swallowed")
		}
		if rep := e.pass(t, r); rep.JobsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
	})
}

// Test_reconcile_recovery_secrets: a candidate Secret is deleted only when it
// validates as the attempt's own recovery record. Malformed, foreign and
// forged candidates are left untouched and surfaced.
func Test_reconcile_recovery_secrets(t *testing.T) {
	ctx := context.Background()
	mutate := func(e *env, t *testing.T, edit func(*kubernetes.Secret)) {
		t.Helper()
		s := e.secret(t)
		ann := map[string]string{}
		for k, v := range s.Metadata.Annotations {
			ann[k] = v
		}
		s.Metadata.Annotations = ann
		edit(&s)
		if _, err := e.fake.UpdateSecret(ctx, e.ns(), s); err != nil {
			t.Fatal(err)
		}
	}
	// settled terminal attempt, Job absent: the Secret is otherwise collectible.
	collectible := func(t *testing.T) *env {
		e := newEnv(t)
		e.dispatched(t)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.terminalCancelled(true)
		e.clock.Advance(time.Hour)
		return e
	}
	t.Run("a valid collectible Secret is deleted by UID", func(t *testing.T) {
		e := collectible(t)
		rep := e.pass(t, e.reconciler(t, &kubetest.Sink{}))
		if rep.SecretsDeleted != 1 {
			t.Fatalf("%+v", rep)
		}
		if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); ok {
			t.Fatal("the Secret survives")
		}
	})
	for name, edit := range map[string]func(*kubernetes.Secret){
		"malformed record": func(s *kubernetes.Secret) { s.Metadata.Annotations[kube.AnnWorkload] = "{not json" },
		"record of another attempt": func(s *kubernetes.Secret) {
			s.Metadata.Annotations[kube.AnnWorkload] = `{"v":1,"attempt":"att_other","phase":"gone"}`
		},
		"unknown phase": func(s *kubernetes.Secret) {
			s.Metadata.Annotations[kube.AnnWorkload] = `{"v":1,"attempt":"` + "x" + `","phase":"wat"}`
		},
		"missing record":     func(s *kubernetes.Secret) { delete(s.Metadata.Annotations, kube.AnnWorkload) },
		"forged fingerprint": func(s *kubernetes.Secret) { s.Metadata.Annotations[kube.AnnFingerprint] = "sha256:forged" },
		"differing run":      func(s *kubernetes.Secret) { s.Metadata.Annotations[kube.AnnRun] = "run_other" },
		"unexpected content": func(s *kubernetes.Secret) { s.Data["extra"] = []byte("x") },
		"foreign owner": func(s *kubernetes.Secret) {
			s.Metadata.OwnerReferences = []kubernetes.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "c", UID: "u"}}
		},
	} {
		t.Run("untouched: "+name, func(t *testing.T) {
			e := collectible(t)
			mutate(e, t, edit)
			rep, err := e.reconciler(t, &kubetest.Sink{}).Pass(ctx)
			if err != nil || rep.SecretsDeleted != 0 || rep.Skipped == 0 {
				t.Fatalf("%+v %v", rep, err)
			}
			if e.fake.Calls("delete secret") != 0 {
				t.Fatal("an invalid recovery Secret was deleted")
			}
			if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); !ok {
				t.Fatal("the invalid recovery Secret is gone")
			}
		})
	}
	t.Run("an aged orphan Secret with a malformed record is untouched", func(t *testing.T) {
		e := newEnv(t)
		e.dispatched(t)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.attempts.Remove(e.rec.AttemptID)
		mutate(e, t, func(s *kubernetes.Secret) { s.Metadata.Annotations[kube.AnnWorkload] = "{not json" })
		e.clock.Advance(time.Hour)
		rep, err := e.reconciler(t, &kubetest.Sink{}).Pass(ctx)
		if err != nil || rep.SecretsDeleted != 0 || rep.Skipped == 0 || e.fake.Calls("delete secret") != 0 {
			t.Fatalf("%+v %v", rep, err)
		}
	})
	t.Run("a lookup or deletion failure is retried, never treated as absence", func(t *testing.T) {
		for _, op := range []string{"get job", "delete secret"} {
			e := collectible(t)
			first := e.fake.Calls(op) + 1 // the next such call fails once
			e.fake.Fault = func(o string, n int) *kubetest.Fault {
				if o == op && n == first {
					return &kubetest.Fault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnavailable}}
				}
				return nil
			}
			r := e.reconciler(t, &kubetest.Sink{})
			if _, err := r.Pass(ctx); err == nil {
				t.Fatalf("%s failure swallowed", op)
			}
			if op == "get job" && e.fake.Calls("delete secret") != 0 {
				t.Fatal("a Job lookup failure authorized deletion")
			}
			if rep := e.pass(t, r); rep.SecretsDeleted != 1 {
				t.Fatalf("%s: retry: %+v", op, rep)
			}
		}
	})
}

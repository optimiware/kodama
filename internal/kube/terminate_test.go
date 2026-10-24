package kube_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

func (e *env) started(t *testing.T) kubernetes.Pod {
	t.Helper()
	yes := true
	return e.fake.AddPod(e.ns(), e.mustJob(t), func(p *kubernetes.Pod) {
		p.Status.Phase = "Running"
		p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, Started: &yes, State: kubernetes.ContainerState{Running: &kubernetes.ContainerRunning{}}}}
	})
}

// Test_terminate_conservatively: delete only verified owned objects with UID
// preconditions; report Stopped only for verified absence of the Job and its Pods;
// report launch evidence only when affirmative.
func Test_terminate_conservatively(t *testing.T) {
	ctx := context.Background()
	t.Run("started workload: delete, hold, then gone with started evidence", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		if _, err := x.Start(ctx, e.intent()); err != nil {
			t.Fatal(err)
		}
		pod := e.started(t)
		e.attempts.Cancel(e.rec.AttemptID)
		e.fake.Hold = true // foreground deletion waits on finalizers
		res, err := x.Stop(ctx, e.workload())
		if err != nil || res.Stopped {
			t.Fatalf("gone while the Job is held open: %+v %v", res, err)
		}
		if res.Launch != execution.LaunchStarted {
			t.Fatalf("launch = %s", res.Launch)
		}
		// Still held: not gone, and the evidence stays.
		res, err = x.Stop(ctx, e.workload())
		if err != nil || res.Stopped || res.Launch != execution.LaunchStarted {
			t.Fatalf("second call: %+v %v", res, err)
		}
		e.fake.Release()
		// The Pod is gone with the Job; a leftover Pod controlled by it blocks Gone.
		ghost := kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: e.rec.JobName, UID: e.record(t)["uid"].(string)}}
		ghost.Spec.Template.Metadata.Labels = map[string]string{kube.LabelJob: e.rec.JobName}
		e.fake.AddPod(e.ns(), ghost, nil)
		res, err = x.Stop(ctx, e.workload())
		if err != nil || res.Stopped {
			t.Fatalf("gone with a Pod still controlled by the Job: %+v %v", res, err)
		}
		_ = pod
		// The leftover Pod controlled by the recorded UID was deleted by its own UID.
		res, err = x.Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchStarted {
			t.Fatalf("final: %+v %v", res, err)
		}
		// Gone is idempotent.
		res, _ = e.runtime(t).Stop(ctx, e.workload())
		if !res.Stopped {
			t.Fatal("not idempotent")
		}
	})

	t.Run("delete uses the verified UID with foreground propagation", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		orig := e.mustJob(t)
		var seen kubernetes.DeleteOptions
		e.fake.Fault = func(op string, n int) *kubetest.Fault { return nil }
		wrapped := &deleteSpy{Fake: e.fake, onDelete: func(o kubernetes.DeleteOptions) { seen = o }}
		xs, _ := kube.NewRuntime(kube.Dependencies{Policy: e.policy, Cluster: wrapped, Attempts: e.attempts, Registrar: e.reg}, kube.Options{Clock: e.clock.Now})
		e.attempts.Cancel(e.rec.AttemptID)
		if _, err := xs.Stop(ctx, e.workload()); err != nil {
			t.Fatal(err)
		}
		if seen.UID != orig.Metadata.UID || seen.Propagation != "Foreground" {
			t.Fatalf("delete options: %+v", seen)
		}
	})

	t.Run("foreign Job at the name is never deleted", func(t *testing.T) {
		e := newEnv(t)
		var j kubernetes.Job
		raw, _ := jsonMarshal(e.exp.Job)
		jsonUnmarshal(t, raw, &j)
		j.Spec.Template.Spec.HostNetwork = true
		foreign := e.fake.PutJob(e.ns(), j)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil {
			t.Fatal(err)
		}
		if e.fake.Calls("delete job") != 0 {
			t.Fatal("deleted a foreign Job")
		}
		got := e.mustJob(t)
		if got.Metadata.UID != foreign.Metadata.UID {
			t.Fatal("the foreign Job changed")
		}
		// No recovery record and no committed evidence: the foreign Job is untouched
		// and the absence of a record is not evidence that nothing launched.
		if !res.Stopped || res.Launch != execution.LaunchUnknown {
			t.Fatalf("%+v", res)
		}
	})

	t.Run("uid replacement: the recorded workload is gone, the replacement untouched", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		e.started(t)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		var j kubernetes.Job
		raw, _ := jsonMarshal(e.exp.Job)
		jsonUnmarshal(t, raw, &j)
		e.fake.PutJob(e.ns(), j)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := x.Stop(ctx, e.workload())
		if err != nil {
			t.Fatal(err)
		}
		if e.fake.Calls("delete job") != 0 {
			t.Fatal("deleted the replacement")
		}
		if res.Stopped {
			t.Fatal("gone while the old Job's Pod still exists")
		}
		if e.fake.Calls("delete pod") != 1 {
			t.Fatal("the orphaned Pod of the recorded UID was not deleted")
		}
		res, err = x.Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchStarted {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("no record and no committed evidence: gone, launch unknown", func(t *testing.T) {
		e := newEnv(t)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchUnknown {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("an issued create keeps the workload not gone until quiescent", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}}
			}
			return nil
		}
		_, _ = e.runtime(t).Start(ctx, e.intent())
		e.fake.Fault = nil
		e.attempts.Cancel(e.rec.AttemptID)
		x := e.runtime(t)
		res, err := x.Stop(ctx, e.workload())
		if err != nil || res.Stopped {
			t.Fatalf("gone while a create may still land: %+v %v", res, err)
		}
		e.clock.Advance(10 * time.Minute)
		res, err = x.Stop(ctx, e.workload())
		if err != nil || !res.Stopped {
			t.Fatalf("not gone after quiescence: %+v %v", res, err)
		}
		if res.Launch != execution.LaunchUnknown {
			t.Fatalf("absence after an uncertain create is not evidence: %s", res.Launch)
		}
	})

	t.Run("lookup outage is not absence", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		e.attempts.Cancel(e.rec.AttemptID)
		for _, op := range []string{"get job", "list pods", "get secret"} {
			e.fake.Fault = func(o string, n int) *kubetest.Fault {
				if o == op {
					return &kubetest.Fault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnavailable}}
				}
				return nil
			}
			res, err := x.Stop(ctx, e.workload())
			if err == nil || res.Stopped {
				t.Fatalf("%s outage reported as %+v %v", op, res, err)
			}
		}
		e.fake.Fault = nil
		// A lost delete response is repeated safely.
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "delete job" && n == 1 {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
			}
			return nil
		}
		if _, err := x.Stop(ctx, e.workload()); err == nil {
			t.Fatal("a lost delete response is not success")
		}
		res, err := x.Stop(ctx, e.workload())
		if err != nil || !res.Stopped {
			t.Fatalf("after the lost delete: %+v %v", res, err)
		}
	})

	t.Run("terminate names the committed Job", func(t *testing.T) {
		e := newEnv(t)
		w := e.workload()
		w.Name = "kdm-" + "0000000000000000000000000000000000000000"
		if _, err := e.runtime(t).Stop(ctx, w); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("got %v", err)
		}
		w = execution.Workload{Scope: e.rec.Scope, AttemptID: "att_unknown", Name: execution.WorkloadName(e.rec.Scope.TenantID, "att_unknown")}
		if _, err := e.runtime(t).Stop(ctx, w); !errors.Is(err, kube.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.ObservedJobUID = "11111111-2222-3333-4444-555555555555" })
		w = e.workload()
		w.UID = "99999999-2222-3333-4444-555555555555"
		if _, err := e.runtime(t).Stop(ctx, w); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("another instance: %v", err)
		}
	})

	t.Run("terminates under policy drift using the recorded UID", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		cfg := kubetest.PolicyConfig(t, "acme")
		cfg.RuntimeCA = e.policy.RuntimeCA()
		cfg.Workload.CPULimit = "8"
		p, _ := kube.NewExecutionPolicy(cfg)
		drifted, _ := kube.NewRuntime(kube.Dependencies{Policy: p, Cluster: e.fake, Attempts: e.attempts, Registrar: e.reg}, kube.Options{Clock: e.clock.Now})
		e.attempts.Cancel(e.rec.AttemptID)
		if _, err := drifted.Stop(ctx, e.workload()); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.job(); ok {
			t.Fatal("drift stranded the workload")
		}
	})
}

// lostSecret removes the attempt's admission Secret: the recovery record is gone.
func (e *env) lostSecret(t *testing.T) {
	t.Helper()
	s := e.secret(t)
	if err := e.fake.DeleteSecret(context.Background(), e.ns(), s.Metadata.Name, kubernetes.DeleteOptions{UID: s.Metadata.UID}); err != nil {
		t.Fatal(err)
	}
}

// commitStart records the start the way execution does after the reconciler's
// observation.
func (e *env) commitStart(uid string) {
	now := e.clock.Now()
	e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
		r.StartedAt, r.ObservedJobUID, r.Launch, r.DispatchConfirmed = &now, uid, execution.LaunchStarted, true
	})
}

// Test_terminate_missing_record: a lost admission Secret or vanished Pods never
// erase launch evidence execution committed, and absence of the record never
// manufactures not_started.
func Test_terminate_missing_record(t *testing.T) {
	ctx := context.Background()
	t.Run("committed start survives a lost Secret and the Pod's disappearance", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		if _, err := x.Start(ctx, e.intent()); err != nil {
			t.Fatal(err)
		}
		uid := e.mustJob(t).Metadata.UID
		e.commitStart(uid)
		e.lostSecret(t)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || res.Launch != execution.LaunchStarted {
			t.Fatalf("%+v %v", res, err)
		}
		if e.fake.Calls("delete job") != 1 {
			t.Fatal("the committed UID did not authorize deleting the Job")
		}
		res, err = e.runtime(t).Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchStarted {
			t.Fatalf("after the Job is gone: %+v %v", res, err)
		}
	})
	t.Run("committed UID finds Pods that outlive the Job and the Secret", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		job := e.mustJob(t)
		e.fake.AddPod(e.ns(), job, nil)
		e.commitStart(job.Metadata.UID)
		e.lostSecret(t)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || res.Stopped || res.Launch != execution.LaunchStarted {
			t.Fatalf("gone with a surviving Pod: %+v %v", res, err)
		}
		if e.fake.Calls("delete pod") != 1 {
			t.Fatal("the surviving Pod of the committed UID was not deleted")
		}
		res, err = e.runtime(t).Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchStarted {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("a replacement Job is never deleted under the committed UID", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		old := e.mustJob(t)
		e.commitStart(old.Metadata.UID)
		e.lostSecret(t)
		e.fake.DropJob(e.ns(), e.rec.JobName)
		var j kubernetes.Job
		raw, _ := jsonMarshal(e.exp.Job)
		jsonUnmarshal(t, raw, &j)
		repl := e.fake.PutJob(e.ns(), j)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || res.Launch != execution.LaunchStarted {
			t.Fatalf("%+v %v", res, err)
		}
		if e.fake.Calls("delete job") != 0 {
			t.Fatal("deleted the replacement")
		}
		if got := e.mustJob(t); got.Metadata.UID != repl.Metadata.UID {
			t.Fatal("the replacement changed")
		}
	})
	t.Run("the record keeps started evidence after the Pods go", func(t *testing.T) {
		// The record's launch evidence stays when execution has none and the Pods
		// are gone.
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		e.started(t)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := x.Stop(ctx, e.workload())
		if err != nil || res.Launch != execution.LaunchStarted {
			t.Fatalf("%+v %v", res, err)
		}
		res, err = e.runtime(t).Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchStarted {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("committed not_started survives a lost Secret and a missing Job", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		e.fake.DropJob(e.ns(), e.rec.JobName)
		e.lostSecret(t)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.Launch = execution.LaunchNotStarted })
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchNotStarted {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("a lost Secret of an issued create is not a never-created workload", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}}
			}
			return nil
		}
		_, _ = e.runtime(t).Start(ctx, e.intent())
		e.fake.Fault = nil
		e.lostSecret(t)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || res.Launch == execution.LaunchNotStarted {
			t.Fatalf("manufactured evidence: %+v %v", res, err)
		}
	})
}

// Test_launch_evidence: launch evidence is NotStarted only when affirmative.
func Test_launch_evidence(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, edit func(*kubernetes.Pod)) execution.StopEvidence {
		e := newEnv(t)
		x := e.runtime(t)
		_, _ = x.Start(ctx, e.intent())
		e.fake.AddPod(e.ns(), e.mustJob(t), edit)
		e.attempts.Cancel(e.rec.AttemptID)
		_, _ = x.Stop(ctx, e.workload())
		res, err := x.Stop(ctx, e.workload())
		if err != nil || !res.Stopped {
			t.Fatalf("%+v %v", res, err)
		}
		return res
	}
	t.Run("pending pod is unknown", func(t *testing.T) {
		if got := run(t, func(p *kubernetes.Pod) { p.Status.Phase = "Pending" }).Launch; got != execution.LaunchUnknown {
			t.Fatalf("pending: %s", got)
		}
	})
	t.Run("image pull failure of the primary is unknown", func(t *testing.T) {
		got := run(t, func(p *kubernetes.Pod) {
			p.Status.Phase = "Pending"
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Waiting: &kubernetes.ContainerWaiting{Reason: "ImagePullBackOff"}}}}
		}).Launch
		if got != execution.LaunchUnknown {
			t.Fatalf("image pull: %s", got)
		}
	})
	t.Run("failed injector is affirmative not_started", func(t *testing.T) {
		got := run(t, func(p *kubernetes.Pod) {
			p.Status.Phase = "Failed"
			p.Spec.RestartPolicy = "Never"
			p.Status.InitContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.InjectContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 1}}}}
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Waiting: &kubernetes.ContainerWaiting{Reason: "PodInitializing"}}}}
		}).Launch
		if got != execution.LaunchNotStarted {
			t.Fatalf("failed init: %s", got)
		}
	})
	t.Run("a terminated primary is started", func(t *testing.T) {
		got := run(t, func(p *kubernetes.Pod) {
			p.Status.Phase = "Failed"
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 13}}}}
		}).Launch
		if got != execution.LaunchStarted {
			t.Fatalf("terminated: %s", got)
		}
	})
}

type deleteSpy struct {
	*kubetest.Fake
	onDelete func(kubernetes.DeleteOptions)
}

func (d *deleteSpy) DeleteJob(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	d.onDelete(o)
	return d.Fake.DeleteJob(ctx, ns, name, o)
}

// Test_launch_evidence_persists: stronger launch evidence is saved before the
// Pod is deleted, survives restart and the Pod's disappearance, and a weaker
// later observation never replaces it.
func Test_launch_evidence_persists(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	x := e.runtime(t)
	if _, err := x.Start(ctx, e.intent()); err != nil {
		t.Fatal(err)
	}
	pod := e.fake.AddPod(e.ns(), e.mustJob(t), func(p *kubernetes.Pod) {
		p.Status.Phase = "Failed"
		p.Spec.RestartPolicy = "Never"
		p.Status.InitContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.InjectContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 1}}}}
		p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Waiting: &kubernetes.ContainerWaiting{Reason: "PodInitializing"}}}}
	})
	e.attempts.Cancel(e.rec.AttemptID)
	e.fake.Hold = true // the Job stays while the evidence changes
	res, err := x.Stop(ctx, e.workload())
	if err != nil || res.Stopped || res.Launch != execution.LaunchNotStarted {
		t.Fatalf("first observation: %+v %v", res, err)
	}
	if got := e.record(t)["launch"]; got != "not_started" {
		t.Fatalf("persisted launch = %v", got)
	}
	// The Pod is later seen to have started: the record is upgraded before anything
	// is deleted again.
	e.fake.EditPod(e.ns(), pod.Metadata.Name, func(p *kubernetes.Pod) {
		p.Status.Phase = "Failed"
		p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 13}}}}
	})
	res, err = e.runtime(t).Stop(ctx, e.workload())
	if err != nil || res.Stopped || res.Launch != execution.LaunchStarted {
		t.Fatalf("second observation: %+v %v", res, err)
	}
	if got := e.record(t)["launch"]; got != "started" {
		t.Fatalf("started evidence not persisted: %v", got)
	}
	// The Pod disappears; a reconstructed executor still reports started.
	e.fake.EditPod(e.ns(), pod.Metadata.Name, func(p *kubernetes.Pod) {
		p.Status.ContainerStatuses = nil
		p.Status.InitContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.InjectContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 1}}}}
	})
	res, err = e.runtime(t).Stop(ctx, e.workload())
	if err != nil || res.Launch != execution.LaunchStarted {
		t.Fatalf("weaker later evidence replaced started: %+v %v", res, err)
	}
	if got := e.record(t)["launch"]; got != "started" {
		t.Fatalf("downgraded: %v", got)
	}
	e.fake.Release()
	_ = e.fake.DeletePod(ctx, e.ns(), pod.Metadata.Name, kubernetes.DeleteOptions{UID: pod.Metadata.UID})
	var final execution.StopEvidence
	for i := 0; i < 4 && !final.Stopped; i++ {
		final, err = e.runtime(t).Stop(ctx, e.workload())
		if err != nil {
			t.Fatal(err)
		}
	}
	if !final.Stopped || final.Launch != execution.LaunchStarted {
		t.Fatalf("after the Pod disappeared: %+v", final)
	}
}

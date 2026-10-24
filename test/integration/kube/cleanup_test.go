package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func (s *stack) startRun(script string, o ...func(*stepSpec)) (*execution.Run, *execution.AttemptSpec) {
	s.t.Helper()
	st := stepSpec{Script: script, Outputs: []string{"v"}}
	for _, f := range o {
		f(&st)
	}
	run, _ := s.submit(executiontest.ScopeA, st)
	if ok, err := s.svc.DispatchNext(context.Background(), executiontest.ScopeA.TenantID); err != nil || !ok {
		s.t.Fatalf("dispatch: %v %v", ok, err)
	}
	agg := s.e.Get(run)
	a := agg.Attempts[0]
	return run, &execution.AttemptSpec{Scope: run.Scope, RunID: run.ID, AttemptID: a.ID, WorkloadName: a.WorkloadName, StepID: a.StepID, ActionKey: a.ActionKey,
		SpecFingerprint: a.SpecFingerprint, ContextDigest: a.ContextDigest, Access: a.Access, Purpose: execution.PurposeCleanup}
}

// Test_cleanup_recovery (PX-08, PX-A06): termination survives delete-response loss,
// finalizers and delayed Pod deletion, UID replacement, late creates after the
// attempt is terminal, and lookup outages; absence and settlement are reported
// only with appropriate evidence.
func Test_cleanup_recovery(t *testing.T) {
	ctx := context.Background()

	t.Run("delete response loss and a finalizer-held Pod", func(t *testing.T) {
		s := newStack(t)
		run, in := s.startRun("sleep 300")
		ns := s.ns(in.Scope.TenantID)
		s.until(time.Minute, "the primary container to start", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.StartedAt != nil, "not started"
		})
		// A finalizer keeps the Pod (and so the foreground-deleted Job) around.
		pods := s.podsOf(in)
		if len(pods) != 1 {
			t.Fatalf("%d Pods", len(pods))
		}
		s.mustKubectl("-n", ns, "patch", "pod", pods[0].Metadata.Name, "--type=merge", "-p", `{"metadata":{"finalizers":["t021.kodama.invalid/hold"]}}`)
		// The delete is applied and its response is lost, once.
		s.cl.Lose = func(op string, n int) bool { return op == "delete job" && n == 1 }
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		s.cl.reset()
		// While the Pod is held the workload is not reported gone, and nothing settles.
		for i := 0; i < 4; i++ {
			s.tick()
			time.Sleep(time.Second)
		}
		_, ctl := s.attempt(in)
		if ctl.TerminationConfirmedAt != nil || ctl.Settled() {
			t.Fatalf("gone reported while the Pod is held: confirmed=%v settled=%v", ctl.TerminationConfirmedAt, ctl.Settled())
		}
		if s.e.Get(run).Run.Status.Terminal() {
			t.Fatal("the run completed while its Pod was alive")
		}
		job, ok := s.jobOf(in)
		if !ok || job.Metadata.DeletionTimestamp == "" {
			t.Fatalf("expected a foreground-deleting Job, got present=%v", ok)
		}
		// Releasing the finalizer lets deletion complete; only then is it gone.
		s.mustKubectl("-n", ns, "patch", "pod", pods[0].Metadata.Name, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		s.until(2*time.Minute, "the run to be cancelled", func() (bool, string) {
			st := s.e.Get(run).Run.Status
			return st.Terminal(), string(st)
		})
		if got := s.e.Get(run).Run.Status; got != execution.RunCancelled {
			t.Fatalf("run %s", got)
		}
		_, ctl = s.attempt(in)
		if ctl.Launch != execution.LaunchStarted || !ctl.Settled() {
			t.Fatalf("launch=%s settled=%v", ctl.Launch, ctl.Settled())
		}
		if _, ok := s.jobOf(in); ok || len(s.podsOf(in)) != 0 {
			t.Fatal("the workload survives")
		}
		if s.held() != 0 {
			t.Fatalf("%d reservations held", s.held())
		}
	})

	t.Run("UID replacement during termination", func(t *testing.T) {
		s := newStack(t)
		run, in := s.startRun("sleep 300", func(st *stepSpec) { st.Image = unpullable })
		ns := s.ns(in.Scope.TenantID)
		orig, _ := s.jobOf(in)
		// Our Job vanishes and a foreign object takes the name.
		s.mustKubectl("-n", ns, "delete", "job", in.WorkloadName, "--cascade=foreground", "--wait=true")
		rec, _ := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
		exp, _ := s.policy.Expect(rec)
		var foreign kubernetes.Job
		raw, _ := json.Marshal(exp.Job)
		_ = json.Unmarshal(raw, &foreign)
		foreign.Spec.Template.Spec.Containers[0].Command = []string{"sleep", "600"}
		foreign.Spec.Template.Spec.Containers[0].Args = nil
		f, err := s.c.client.CreateJob(ctx, ns, foreign)
		must(t, err)
		if f.Metadata.UID == orig.Metadata.UID {
			t.Fatal("UID did not change")
		}
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		s.until(2*time.Minute, "termination to be confirmed", func() (bool, string) {
			_, c := s.attempt(in)
			return c.TerminationConfirmedAt != nil, "unconfirmed"
		})
		after, ok := s.jobOf(in)
		if !ok || after.Metadata.UID != f.Metadata.UID {
			t.Fatal("the executor deleted an object it could not verify")
		}
	})

	t.Run("a same-name replacement of an observed Job is not deleted after the Secret is gone", func(t *testing.T) {
		s := newStack(t)
		run, in := s.startRun("sleep 300")
		ns := s.ns(in.Scope.TenantID)
		s.until(time.Minute, "the primary container to start", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.StartedAt != nil, "not started"
		})
		orig, _ := s.jobOf(in)
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		s.until(2*time.Minute, "the run to settle", func() (bool, string) {
			st := s.e.Get(run).Run.Status
			return st.Terminal(), string(st)
		})
		if _, ok := s.jobOf(in); ok {
			t.Fatal("the workload survives cancellation")
		}
		// The recovery Secret is collected once the attempt is settled and the Job
		// absent: committed evidence alone (execution's observed UID) remains.
		s.until(time.Minute, "the Secret to be deleted", func() (bool, string) { return !s.secretExists(in), "secret present" })
		admitted := s.admitCount(in)
		// This is a replacement, not a delayed create: the first Job was observed
		// and deleted. An identical recipe under the same name, with another UID,
		// is a foreign object and must never be treated as the attempt's workload.
		rec, _ := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
		if rec.ObservedJobUID != orig.Metadata.UID {
			t.Fatalf("execution did not commit the observed UID: %q != %q", rec.ObservedJobUID, orig.Metadata.UID)
		}
		exp, err := s.policy.Expect(rec)
		must(t, err)
		repl, err := s.c.client.CreateJob(ctx, ns, exp.Job)
		must(t, err)
		if repl.Metadata.UID == orig.Metadata.UID {
			t.Fatal("the UID did not change")
		}
		time.Sleep(3 * time.Second) // past the orphan grace
		for i := 0; i < 4; i++ {
			s.tick()
			time.Sleep(500 * time.Millisecond)
		}
		got, ok := s.jobOf(in)
		if !ok || got.Metadata.UID != repl.Metadata.UID || got.Metadata.DeletionTimestamp != "" {
			t.Fatal("the same-name replacement was deleted on the strength of its spec")
		}
		if now := s.admitCount(in); now != admitted {
			t.Fatalf("the replacement was admitted (%d -> %d)", admitted, now)
		}
		if _, err := s.srv.rt.Renew(ctx, in.AttemptID, s.srv.token(in.AttemptID)); err == nil {
			t.Fatal("a token of a terminal attempt was renewed")
		}
		s.mustKubectl("-n", ns, "delete", "job", in.WorkloadName, "--cascade=foreground", "--wait=true")
	})

	t.Run("a genuinely delayed initial create is deleted", func(t *testing.T) {
		s := newStack(t)
		run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: "sleep 300", Outputs: []string{"v"}})
		in := s.claim(executiontest.ScopeA.TenantID)
		ns := s.ns(in.Scope.TenantID)
		// The create is issued but never reaches the API server in time: the
		// dispatch is unconfirmed and execution never observes a Job.
		s.cl.Fail = func(op string, n int) error {
			if op == "create job" {
				return lost(op)
			}
			return nil
		}
		if _, err := s.runtime.Start(ctx, *in); err == nil {
			t.Fatal("an unsent create cannot be confirmed")
		}
		s.cl.reset()
		if s.jobsNamed(in) != 0 {
			t.Fatal("a Job exists before the delayed create")
		}
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		// No Job exists and no create can still be in flight past the quiescence
		// window: termination is confirmed, but the launch stays unknown, so only a
		// trusted effect reconciliation settles the attempt.
		s.until(3*time.Minute, "termination to be confirmed", func() (bool, string) {
			_, c := s.attempt(in)
			return c.TerminationConfirmedAt != nil, "unconfirmed"
		})
		s.e.Effects.Set(execution.VerdictSettled, nil)
		s.until(2*time.Minute, "the attempt to settle", func() (bool, string) {
			_, c := s.attempt(in)
			return c.Settled(), "unsettled"
		})
		rec, err := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
		must(t, err)
		if rec.ObservedJobUID != "" {
			t.Fatalf("a UID was committed for a Job nobody observed: %q", rec.ObservedJobUID)
		}
		admitted := s.admitCount(in)
		// The delayed create lands now, after the attempt is terminal.
		exp, err := s.policy.Expect(rec)
		must(t, err)
		late, err := s.c.client.CreateJob(ctx, ns, exp.Job)
		must(t, err)
		s.until(time.Minute, "the late Job to be deleted", func() (bool, string) {
			j, ok := s.jobOf(in)
			return !ok || j.Metadata.UID != late.Metadata.UID, "late Job present"
		})
		if now := s.admitCount(in); now != admitted {
			t.Fatalf("the late workload was admitted (%d -> %d)", admitted, now)
		}
		if _, err := s.srv.rt.Renew(ctx, in.AttemptID, s.srv.token(in.AttemptID)); err == nil {
			t.Fatal("a token of a terminal attempt was renewed")
		}
		s.until(time.Minute, "the Secret to be deleted", func() (bool, string) { return !s.secretExists(in), "secret present" })
		if s.held() != 0 {
			t.Fatalf("%d reservations held", s.held())
		}
	})

	t.Run("a lost recovery Secret does not erase committed launch evidence", func(t *testing.T) {
		s := newStack(t)
		run, in := s.startRun("sleep 300")
		ns := s.ns(in.Scope.TenantID)
		s.until(time.Minute, "the primary container to start", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.StartedAt != nil, "not started"
		})
		// The admission Secret (the executor's recovery record) disappears while the
		// workload runs and execution holds the committed start.
		s.mustKubectl("-n", ns, "delete", "secret", kube.SecretName(in.WorkloadName), "--wait=true")
		if s.secretExists(in) {
			t.Fatal("the Secret survives its deletion")
		}
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		s.until(2*time.Minute, "the run to be cancelled", func() (bool, string) {
			st := s.e.Get(run).Run.Status
			return st.Terminal(), string(st)
		})
		_, ctl := s.attempt(in)
		if ctl.Launch != execution.LaunchStarted || !ctl.Settled() {
			t.Fatalf("a missing record turned the started workload into %s (settled=%v)", ctl.Launch, ctl.Settled())
		}
		if _, ok := s.jobOf(in); ok || len(s.podsOf(in)) != 0 {
			t.Fatal("the workload survives")
		}
	})

	t.Run("persisted launch evidence outlives the Pod and a restarted executor", func(t *testing.T) {
		s := newStack(t)
		_, in := s.startRun("sleep 300")
		ns := s.ns(in.Scope.TenantID)
		// Observe the start through the Pod only: no reconciliation tick runs, so
		// execution never commits the start and the executor's own record is the
		// only holder of the evidence.
		var pod kubernetes.Pod
		eventually(t, time.Minute, "the primary container to run", func() (bool, string) {
			for _, p := range s.podsOf(in) {
				for _, cs := range p.Status.ContainerStatuses {
					if cs.Name == kube.PrimaryContainer && cs.State.Running != nil {
						pod = p
						return true, ""
					}
				}
			}
			return false, "not running"
		})
		if _, ctl := s.attempt(in); ctl.Launch == execution.LaunchStarted {
			t.Fatal("execution already committed the start; the test would not isolate the executor's record")
		}
		// Deleting the Job interrupts the entrypoint, which reports an interrupted
		// result; refuse it so execution cannot commit the start behind the
		// executor's back and the executor's record stays the only holder.
		s.srv.RefuseResults.Store(true)
		// A finalizer keeps the Pod through the Job's deletion.
		s.mustKubectl("-n", ns, "patch", "pod", pod.Metadata.Name, "--type=merge", "-p", `{"metadata":{"finalizers":["t021.kodama.invalid/hold"]}}`)
		w := in.Workload("")
		res, err := s.newRuntime(s.c.client).Stop(ctx, w)
		if err != nil || res.Stopped || res.Launch != execution.LaunchStarted {
			t.Fatalf("first termination: %+v %v", res, err)
		}
		// The evidence is in the durable record before anything disappears.
		sec, err := s.c.client.GetSecret(ctx, ns, kube.SecretName(in.WorkloadName))
		must(t, err)
		if !strings.Contains(sec.Metadata.Annotations[kube.AnnWorkload], `"launch":"started"`) {
			t.Fatalf("started evidence not persisted: %s", sec.Metadata.Annotations[kube.AnnWorkload])
		}
		// Release the Pod; the workload disappears. A reconstructed executor reads
		// the record and still reports started.
		s.mustKubectl("-n", ns, "patch", "pod", pod.Metadata.Name, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		eventually(t, 2*time.Minute, "the workload to disappear", func() (bool, string) {
			_, jobThere := s.jobOf(in)
			return !jobThere && len(s.podsOf(in)) == 0, "workload present"
		})
		if _, ctl := s.attempt(in); ctl.Launch == execution.LaunchStarted {
			t.Fatal("execution committed the start while the test held it back")
		}
		var final execution.StopEvidence
		eventually(t, time.Minute, "gone to be reported", func() (bool, string) {
			final, err = s.newRuntime(s.c.client).Stop(ctx, w)
			return err == nil && final.Stopped, "not gone"
		})
		if final.Launch != execution.LaunchStarted {
			t.Fatalf("the started evidence was lost with the Pod: %+v", final)
		}
	})

	t.Run("forged orphan candidates are left untouched", func(t *testing.T) {
		s := newStack(t)
		_, in := s.startRun("sleep 300")
		ns := s.ns(in.Scope.TenantID)
		rec, err := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
		must(t, err)
		exp, err := s.policy.Expect(rec)
		must(t, err)
		// A Job with the executor's labels and annotations, the deterministic name
		// of an attempt that does not exist, and a claimed fingerprint. Nothing the
		// executor wrote corroborates it.
		ghost := domain.AttemptID("att_t021ghost")
		forged := exp.Job
		forged.Metadata.Name = execution.WorkloadName(in.Scope.TenantID, ghost)
		forged.Metadata.Labels = map[string]string{kube.LabelManagedBy: kube.ValueManagedBy, kube.LabelJob: forged.Metadata.Name}
		forged.Metadata.Annotations = map[string]string{}
		for k, v := range exp.Job.Metadata.Annotations {
			forged.Metadata.Annotations[k] = v
		}
		forged.Metadata.Annotations[kube.AnnAttempt] = string(ghost)
		forged.Spec.Template.Metadata.Labels = forged.Metadata.Labels
		created, err := s.c.client.CreateJob(ctx, ns, forged)
		must(t, err)
		time.Sleep(3 * time.Second) // past the orphan grace
		for i := 0; i < 3; i++ {
			s.tick()
		}
		got, err := s.c.client.GetJob(ctx, ns, forged.Metadata.Name)
		if err != nil || got.Metadata.UID != created.Metadata.UID {
			t.Fatalf("the forged orphan candidate was deleted: %v", err)
		}
		// The real attempt's workload is untouched as well.
		if j, ok := s.jobOf(in); !ok || j.Metadata.DeletionTimestamp != "" {
			t.Fatal("the live attempt's Job was disturbed")
		}
	})

	t.Run("invalid recovery Secrets are left untouched and a valid one is collected", func(t *testing.T) {
		s := newStack(t)
		_, in := s.startRun("sleep 300")
		ns := s.ns(in.Scope.TenantID)
		real, err := s.c.client.GetSecret(ctx, ns, kube.SecretName(in.WorkloadName))
		must(t, err)
		// forge copies the real Secret for an attempt that does not exist: the
		// deterministic name, labels and annotations a controller would write.
		forge := func(ghost domain.AttemptID, edit func(*kubernetes.Secret)) kubernetes.Secret {
			job := execution.WorkloadName(in.Scope.TenantID, ghost)
			sec := kubernetes.Secret{
				Metadata: kubernetes.ObjectMeta{
					Name: kube.SecretName(job), Namespace: ns, Labels: map[string]string{kube.LabelManagedBy: kube.ValueManagedBy, kube.LabelJob: job},
					Annotations: map[string]string{},
				},
				Type: "Opaque", Immutable: real.Immutable, Data: map[string][]byte{},
			}
			for k, v := range real.Metadata.Annotations {
				sec.Metadata.Annotations[k] = v
			}
			for k, v := range real.Data {
				sec.Data[k] = v
			}
			sec.Metadata.Annotations[kube.AnnAttempt] = string(ghost)
			sec.Metadata.Annotations[kube.AnnWorkload] = `{"v":1,"attempt":"` + string(ghost) + `","phase":"gone"}`
			if edit != nil {
				edit(&sec)
			}
			created, err := s.c.client.CreateSecret(ctx, ns, sec)
			must(t, err)
			return created
		}
		invalid := map[string]kubernetes.Secret{
			"malformed": forge("att_t021bad1", func(x *kubernetes.Secret) { x.Metadata.Annotations[kube.AnnWorkload] = "{not json" }),
			"other attempt": forge("att_t021bad2", func(x *kubernetes.Secret) {
				x.Metadata.Annotations[kube.AnnWorkload] = `{"v":1,"attempt":"att_t021other","phase":"gone"}`
			}),
		}
		valid := forge("att_t021good", nil)
		time.Sleep(3 * time.Second) // past the orphan grace
		for i := 0; i < 3; i++ {
			s.tick()
		}
		for name, sec := range invalid {
			got, err := s.c.client.GetSecret(ctx, ns, sec.Metadata.Name)
			if err != nil || got.Metadata.UID != sec.Metadata.UID {
				t.Fatalf("the invalid recovery Secret %q was deleted: %v", name, err)
			}
		}
		if _, err := s.c.client.GetSecret(ctx, ns, valid.Metadata.Name); !errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("the valid aged orphan Secret was not collected: %v", err)
		}
		if !s.secretExists(in) {
			t.Fatal("the live attempt's Secret was disturbed")
		}
		for _, sec := range invalid {
			must(t, s.c.client.DeleteSecret(ctx, ns, sec.Metadata.Name, kubernetes.DeleteOptions{UID: sec.Metadata.UID}))
		}
	})

	t.Run("a recovery Secret with a forged fingerprint authorizes no deletion", func(t *testing.T) {
		s := newStack(t)
		run, in := s.startRun("sleep 300")
		ns := s.ns(in.Scope.TenantID)
		s.until(time.Minute, "the primary container to start", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.StartedAt != nil, "not started"
		})
		sec, err := s.c.client.GetSecret(ctx, ns, kube.SecretName(in.WorkloadName))
		must(t, err)
		orig := sec.Metadata.Annotations[kube.AnnFingerprint]
		s.mustKubectl("-n", ns, "annotate", "secret", sec.Metadata.Name, "--overwrite", kube.AnnFingerprint+"=sha256:forged")
		job, _ := s.jobOf(in)
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			s.tick()
			time.Sleep(time.Second)
		}
		// Neither the workload nor the inconsistent record is removed, and the
		// attempt is not reported gone on the strength of it.
		got, ok := s.jobOf(in)
		if !ok || got.Metadata.UID != job.Metadata.UID {
			t.Fatal("the Job was deleted under a record that does not match the attempt")
		}
		now, err := s.c.client.GetSecret(ctx, ns, sec.Metadata.Name)
		if err != nil || now.Metadata.UID != sec.Metadata.UID {
			t.Fatalf("the forged Secret was deleted: %v", err)
		}
		if _, ctl := s.attempt(in); ctl.TerminationConfirmedAt != nil || ctl.Settled() {
			t.Fatal("termination was confirmed through a forged record")
		}
		// Restoring the genuine fingerprint lets termination complete.
		s.mustKubectl("-n", ns, "annotate", "secret", sec.Metadata.Name, "--overwrite", kube.AnnFingerprint+"="+orig)
		s.until(3*time.Minute, "the run to be cancelled", func() (bool, string) {
			st := s.e.Get(run).Run.Status
			return st.Terminal(), string(st)
		})
	})

	t.Run("lookup outages are not absence", func(t *testing.T) {
		s := newStack(t)
		run, in := s.startRun("sleep 300")
		s.until(time.Minute, "the primary container to start", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.StartedAt != nil, "not started"
		})
		for _, op := range []string{"get job", "list pods", "get secret"} {
			s.cl.Fail = func(o string, n int) error {
				if o == op {
					return &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnavailable}
				}
				return nil
			}
			res, err := s.runtime.Stop(ctx, in.Workload(""))
			if err == nil || res.Stopped {
				t.Fatalf("%s outage reported as gone (%+v, %v)", op, res, err)
			}
		}
		s.cl.reset()
		// Reconciliation during a list outage deletes nothing and reports an error.
		s.cl.Fail = func(op string, n int) error {
			if op == "list jobs" || op == "get job" {
				return &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}
			}
			return nil
		}
		if _, err := s.rec.Pass(ctx); err == nil {
			t.Fatal("an outage was swallowed")
		}
		s.cl.reset()
		if _, ok := s.jobOf(in); !ok {
			t.Fatal("deleted during an outage")
		}
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		s.until(2*time.Minute, "the run to be cancelled", func() (bool, string) {
			st := s.e.Get(run).Run.Status
			return st.Terminal(), string(st)
		})
	})
	_ = errors.New
	_ = kube.ErrPolicy
}

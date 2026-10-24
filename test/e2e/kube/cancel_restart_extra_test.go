package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// restart replaces the controller process: a fresh executor, execution service
// and reconciler over the same durable state, with no memory of the old ones.
func (s *stack) restart() {
	s.t.Helper()
	s.runtime = s.newRuntime(s.cl)
	s.svc = s.newService(s.opt)
	deps := kube.Dependencies{
		Policy: s.policy, Cluster: s.cl, Attempts: s.attempts, Registrar: s.registry, Logs: s.c.client,
		Observations: s.svc, Tenants: tenantIDs{executiontest.ScopeA.TenantID, executiontest.ScopeB.TenantID},
	}
	rec, err := kube.NewReconciler(deps, s.kopt)
	must(s.t, err)
	s.rec = rec
}

// cancelThenRestart (execution_assembly_cancel_restart on `kube`; PX-A06, PX-08,
// IT-A05): a cancellation commits while the Job runs and the process dies before
// it can delete anything. The next process finishes it: the Job and its Pod are
// deleted by verified UID, the run settles cancelled, a late result is refused
// and the entrypoint's token authorizes nothing.
func cancelThenRestart(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: slow(120), Outputs: []string{"v"}})
	in := s.dispatch(run)

	eventually(t, 3*time.Minute, "the entrypoint to be admitted", func() (bool, string) {
		for _, e := range s.srv.admitEvents(in.AttemptID) {
			if e.Status == 200 {
				return true, ""
			}
		}
		return false, "no admission yet"
	})
	tok := s.srv.token(in.AttemptID)
	job, _ := s.jobOf(in)

	// The cancellation commits; every cluster call the dying process would make
	// to stop the workload fails.
	s.cl.Fail = func(op string, _ int) error {
		if op == "delete job" {
			return &kubeUnavailable
		}
		return nil
	}
	if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
		t.Fatal(err)
	}
	s.cl.reset()
	if at, _ := s.attempt(in); at.Status != execution.AttemptCancelled {
		t.Fatalf("attempt %s after the cancellation committed", at.Status)
	}
	if j, ok := s.jobOf(in); !ok || j.Metadata.UID != job.Metadata.UID {
		t.Fatal("the Job should still be running when the process died")
	}

	s.restart()
	s.until(4*time.Minute, "the next process to settle the cancellation", func() (bool, string) {
		st := s.e.Get(run).Run.Status
		return st.Terminal(), string(st)
	})
	if got := s.e.Get(run).Run.Status; got != execution.RunCancelled {
		t.Fatalf("run %s", got)
	}
	s.until(2*time.Minute, "the Job to be deleted", func() (bool, string) {
		_, still := s.jobOf(in)
		return !still, "the Job still exists"
	})
	at, ctl := s.attempt(in)
	if at.Status != execution.AttemptCancelled || !ctl.Settled() || ctl.Launch != execution.LaunchStarted {
		t.Fatalf("attempt %s settled=%v launch=%s", at.Status, ctl.Settled(), ctl.Launch)
	}
	if s.held() != 0 {
		t.Fatalf("%d reservations still held", s.held())
	}

	// Late authority is refused after the restart: the token and a result.
	if _, err := s.registry.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: in.AttemptID, Bearer: tok, Action: runtimeauth.ActionContextRead}); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
		t.Fatalf("the cancelled attempt's token: %v", err)
	}
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptrTo(int64(0)), Outputs: map[string]runtimev1.Value{"v": runtimev1.StringValue("late")}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	hash, err := runtimev1.ResultHash(res)
	must(t, err)
	if _, err := s.svc.CommitResult(ctx, execution.ResultCommit{Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: hash, Result: res}}); !errors.Is(err, execution.ErrAttemptRevoked) {
		t.Fatalf("late result: %v", err)
	}

	// A process that restarts before the cancellation ever reached it also
	// settles: the run that never dispatched is cancelled with no Job at all.
	run2, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
	if _, err := s.svc.Cancel(ctx, s.caller(), run2.Scope, run2.ID); err != nil {
		t.Fatal(err)
	}
	s.restart()
	s.until(2*time.Minute, "the undispatched run to settle", func() (bool, string) {
		st := s.e.Get(run2).Run.Status
		return st.Terminal(), string(st)
	})
	if got := s.e.Get(run2).Run.Status; got != execution.RunCancelled {
		t.Fatalf("run %s", got)
	}
	if n := s.attemptsOf(run2); n != 0 {
		t.Fatalf("a cancelled run created %d attempts", n)
	}
}

// foreignJobAtName (PX-03, IT-06): a Job that some other party created at the
// attempt's deterministic name is never adopted, mutated, deleted or admitted;
// the executor refuses it and creates no credential.
func foreignJobAtName(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	tenant := executiontest.ScopeA.TenantID
	_, _ = s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
	in := s.claim(tenant)
	ns := s.ns(tenant)
	rec, err := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
	must(t, err)
	exp, err := s.policy.Expect(rec)
	must(t, err)
	var hostile kubernetes.Job
	raw, _ := json.Marshal(exp.Job)
	must(t, json.Unmarshal(raw, &hostile))
	hostile.Spec.Template.Spec.Containers[0].Command = []string{"sleep", "600"}
	hostile.Spec.Template.Spec.Containers[0].Args = nil
	before, err := s.c.client.CreateJob(ctx, ns, hostile)
	must(t, err)

	if _, err := s.runtime.Start(ctx, *in); !errors.Is(err, kube.ErrForeignWorkload) {
		t.Fatalf("a foreign Job at the name: %v", err)
	}
	if res, err := s.runtime.Stop(ctx, in.Workload("")); err != nil {
		t.Fatalf("stop: %v %+v", err, res)
	}
	after, ok := s.jobOf(in)
	if !ok {
		t.Fatal("the foreign Job was deleted")
	}
	if specOf(t, before) != specOf(t, after) {
		t.Fatal("the foreign Job was modified")
	}
	if s.secretExists(in) || s.grants(in) != 0 {
		t.Fatal("a credential was created or registered for a collision")
	}
	if n := s.admitCount(in); n != 0 {
		t.Fatalf("%d admissions reached the runtime for a foreign Job", n)
	}
	s.mustKubectl("-n", ns, "delete", "job", in.WorkloadName, "--wait=false", "--cascade=background")
}

// specOf returns the part of a live Job a foreign party could not change without
// it being visible: UID, spec, annotations and labels.
func specOf(t *testing.T, j kubernetes.Job) string {
	t.Helper()
	var doc map[string]any
	must(t, json.Unmarshal(j.Raw, &doc))
	b, _ := json.Marshal(map[string]any{"uid": j.Metadata.UID, "spec": doc["spec"], "annotations": j.Metadata.Annotations, "labels": j.Metadata.Labels})
	return string(b)
}

// restartAdoptsRunningJob (PX-A02 component evidence): a controller that restarts
// while the Job runs, with no memory, adopts the same Job and UID: the run still
// completes through the real entrypoint with one attempt, one Job and one
// admission.
func restartAdoptsRunningJob(t *testing.T) {
	s := newStack(t)
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: slow(15), Outputs: []string{"v"}})
	in := s.dispatch(run)
	eventually(t, 3*time.Minute, "the entrypoint to be admitted", func() (bool, string) {
		for _, e := range s.srv.admitEvents(in.AttemptID) {
			if e.Status == 200 {
				return true, ""
			}
		}
		return false, "no admission yet"
	})
	job, _ := s.jobOf(in)
	s.restart()
	agg := s.settle(run, in)
	if got := strOutput(t, agg, "v"); got != "ok" {
		t.Fatalf("output %q", got)
	}
	if got, ok := s.jobOf(in); ok && got.Metadata.UID != job.Metadata.UID {
		t.Fatalf("the Job was replaced: %s != %s", got.Metadata.UID, job.Metadata.UID)
	}
	if n := s.attemptsOf(run); n != 1 || s.grants(in) != 1 {
		t.Fatalf("attempts=%d grants=%d", n, s.grants(in))
	}
	ok200 := 0
	for _, e := range s.srv.admitEvents(in.AttemptID) {
		if e.Status == 200 {
			ok200++
		}
	}
	if ok200 != 1 {
		t.Fatalf("%d admissions", ok200)
	}
}

package kube_full_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/test/support/stage"
)

// TestKubeCancelRestart is kube_cancel_restart on the installed production composition.
// PX-A06 and IT-A05 use an admission hold so cancel is committed while Job delete is refused.
// PX-A02 uses a test-only HTTPS loopback proxy that withholds the committed create acknowledgment
// and its read-back until a replacement server adopts the same Job. The ordinary chart is restored.
func TestKubeCancelRestart(t *testing.T) {
	t.Run("lost_create_acknowledgment", lostCreateAcknowledgment)

	t.Run("cancel committed while delete is refused", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Cancel", "# c")
		w.Provision(item.Id)
		def := w.Definition("sleeper", stage.Step{Script: `sleep 900`, Timeout: 900})
		ns := w.Namespace(w.TenantA)
		hold := i.HoldJobs(i.Context(2*time.Minute), ns, "DELETE")
		run := w.Run("cancel-hold", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		job := i.WaitJob(ns, attempt, true, 2*time.Minute)
		pod := i.WaitPod(ns, job.Name, time.Minute)
		i.WaitLaunched(ns, pod.Name)
		token := i.RuntimeToken(ns, pod.Name)
		if r := w.CancelRun(run.Id); r.StatusCode() != http.StatusAccepted {
			t.Fatalf("cancel: %d", r.StatusCode())
		}
		stage.WaitFor(t, "committed cancellation and token revocation", time.Minute, func() bool {
			return i.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND revoked_at IS NOT NULL`, attempt) == 1
		})
		if len(i.JobsOfAttempt(ns, attempt)) != 1 {
			t.Fatal("Job delete was not held")
		}
		hold.Release()
		if final := w.WaitRun(run.Id, 3*time.Minute); final.Status != client.RunStatusCancelled {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		w.WaitRemoved(attempt)
		code, _ := submitResult(t, i, attempt, token)
		if code != http.StatusUnauthorized && code != http.StatusForbidden && code != http.StatusConflict {
			t.Fatalf("a late result was accepted: HTTP %d", code)
		}
	})

	t.Run("restart adopts the running Job", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Adopt", "# a")
		w.Provision(item.Id)
		def := w.Definition("sleeper", stage.Step{Script: `sleep 900`, Timeout: 900})
		ns := w.Namespace(w.TenantA)
		run := w.Run("adopt-run", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		job := i.WaitJob(ns, attempt, true, 2*time.Minute)
		pod := i.WaitPod(ns, job.Name, time.Minute)
		i.WaitLaunched(ns, pod.Name)
		uid := job.UID
		// Crash, don't drain: a graceful stop can reconcile the workload away.
		i.Crash(i.Context(6 * time.Minute))
		got := i.JobsOfAttempt(ns, attempt)
		if len(got) != 1 || got[0].UID != uid {
			t.Fatalf("restart did not adopt %s", uid)
		}
		w.CancelRun(run.Id)
		w.WaitRemoved(attempt)
	})

}

func lostCreateAcknowledgment(t *testing.T) {
	i := stage.New(t)
	w := i.Seed()
	fault := i.InstallCreateFault(i.Context(10 * time.Minute))
	item := w.WorkItem("Lost create acknowledgment", "# recovery")
	w.Provision(item.Id)
	def := w.Definition("lost-create", stage.Step{Script: stage.CommandBarrier + `echo recovered`, Timeout: 900})
	ns := w.Namespace(w.TenantA)
	pods := i.HoldPods(i.Context(2*time.Minute), ns)
	create := i.HoldJobs(i.Context(2*time.Minute), ns, "CREATE")
	run := w.Run("lost-create-run", item, def.Id)
	attempt := w.WaitAttempt(run.Id, time.Minute)
	var name string
	stage.WaitFor(t, "persisted Job identity", time.Minute, func() bool {
		atts := w.Attempts(run.Id)
		if len(atts) != 1 {
			return false
		}
		name = atts[0].JobName
		return name != ""
	})
	fault.Arm(ns, name)
	create.Release()
	var uid string
	stage.WaitForEvery(t, "committed create with acknowledgment withheld", 2*time.Minute, time.Second, func() bool {
		s := fault.Status()
		uid = s.UID
		return s.Lost && s.Creates == 1 && uid != ""
	})
	if i.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id=$1 AND dispatch_confirmed_at IS NULL`, attempt) != 1 {
		t.Fatal("fault did not preserve an unconfirmed dispatch")
	}
	jobs := i.JobsOfAttempt(ns, attempt)
	if len(jobs) != 1 || jobs[0].UID != uid {
		t.Fatal("upstream Job not committed exactly once")
	}
	i.Crash(i.Context(6 * time.Minute))
	stage.WaitFor(t, "replacement process confirms the original Job", 2*time.Minute, func() bool {
		return i.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id=$1 AND dispatch_confirmed_at IS NOT NULL`, attempt) == 1
	})
	jobs = i.JobsOfAttempt(ns, attempt)
	if len(jobs) != 1 || jobs[0].UID != uid {
		t.Fatal("recovery replaced the Job")
	}
	pods.Release()
	pod := i.WaitPod(ns, name, 2*time.Minute)
	i.WaitCommandBarrier(ns, pod.Name)
	if n := i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id=$1`, run.Id); n != 1 {
		t.Fatalf("attempts=%d", n)
	}
	if n := i.OwnerCount(`SELECT count(*) FROM runtime_grants WHERE attempt_id=$1`, attempt); n != 1 {
		t.Fatalf("runtime grants=%d", n)
	}
	if ps, err := i.PodsOfJob(i.Context(time.Minute), ns, name); err != nil || len(ps) != 1 {
		t.Fatal("recovery did not preserve one workload Pod")
	}
	i.ReleaseCommand(ns, pod.Name)
	if final := w.WaitRun(run.Id, 3*time.Minute); final.Status != client.RunStatusSucceeded {
		t.Fatalf("recovered run %s (%s)", final.Status, final.Reason)
	}
	w.WaitRemoved(attempt)
}

func submitResult(t *testing.T, i *stage.Installation, attempt, token string) (int, []byte) {
	t.Helper()
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: new(int64), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	hash, err := runtimev1.ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	resp, body, err := i.TryRaw(http.MethodPost, "/runtime/v1/attempts/"+attempt+"/result", token, stage.JSONBody(t, runtimev1.ResultRequest{ResultHash: hash, Result: res}), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return 0, nil
	}
	return resp.StatusCode, body
}

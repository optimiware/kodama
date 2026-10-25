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
// PX-A02 (lost create response) is not exercised: the logs client refuses a loopback http API, so
// the installed evidence is adoption of a live Job after a crash restart of the server.
func TestKubeCancelRestart(t *testing.T) {
	t.Log("FAULT_CONFIG none on the chart install. PX-A02 lost-create-response is not injected: the logs client refuses a loopback http API (logs/kubernetes: the endpoint must be https). Adoption after restart is the installed evidence.")

	t.Run("cancel committed while delete is refused", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Cancel", "# c")
		w.Provision(item.Id)
		def := w.Definition("sleeper", stage.Step{Script: `sleep 300`})
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
		time.Sleep(6 * time.Second)
		if len(i.JobsOfAttempt(ns, attempt)) != 1 {
			t.Fatal("Job delete was not held")
		}
		hold.Release()
		if final := w.WaitRun(run.Id, 3*time.Minute); final.Status != client.RunStatusCancelled {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		w.WaitRemoved(attempt)
		code, _ := submitResult(t, i, attempt, token)
		if code < 400 {
			t.Fatalf("a late result was accepted: HTTP %d", code)
		}
	})

	t.Run("restart adopts the running Job", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Adopt", "# a")
		w.Provision(item.Id)
		def := w.Definition("sleeper", stage.Step{Script: `sleep 300`})
		ns := w.Namespace(w.TenantA)
		run := w.Run("adopt-run", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		job := i.WaitJob(ns, attempt, true, 2*time.Minute)
		pod := i.WaitPod(ns, job.Name, time.Minute)
		i.WaitLaunched(ns, pod.Name)
		uid := job.UID
		// Crash, don't drain: a graceful stop can reconcile the workload away.
		if _, err := i.Kube.Run(i.Context(2*time.Minute), "-n", i.S.Namespace, "delete", "pod", "-l", "app.kubernetes.io/component=server", "--force", "--grace-period=0"); err != nil {
			t.Fatalf("crash the server: %v", err)
		}
		if _, err := i.Kube.Run(i.Context(5*time.Minute), "-n", i.S.Namespace, "rollout", "status", "deploy/"+i.Deployment(), "--timeout=4m"); err != nil {
			t.Fatalf("server rollout: %v", err)
		}
		i.WaitReady(i.Context(3 * time.Minute))
		got := i.JobsOfAttempt(ns, attempt)
		if len(got) != 1 || got[0].UID != uid {
			t.Fatalf("restart did not adopt %s", uid)
		}
		w.CancelRun(run.Id)
		w.WaitRemoved(attempt)
	})

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

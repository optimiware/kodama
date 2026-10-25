package assembly_execution_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/test/support/stage"
)

// roundtripKube is execution_assembly_roundtrip on the installed Kubernetes server.
// Observations are per operation (admission, context, artifact reserve/complete, result),
// not an aggregate callback count. The runtime token is never logged.
func roundtripKube(t *testing.T) {
	t.Helper()
	i := stage.New(t)
	w := i.Seed()
	ctx := t.Context()
	item := w.WorkItem("Roundtrip", "# roundtrip")
	w.Provision(item.Id)
	def := w.Definition("roundtrip", stage.Step{
		Script:    `echo "roundtrip log line"; echo "artifact bytes" > /kodama/outputs/report; printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`,
		Outputs:   []string{"v"},
		Artifacts: map[string]string{"report": "/kodama/outputs/report"},
	})
	events := i.OpenEvents("engineer", w.ProjectEvents(), "")
	events.Until(t, "the snapshot", 15*time.Second, func(f stage.Frame) bool { return f.Event == "snapshot" })

	run := w.Run("roundtrip-run", item, def.Id)
	attempt := w.WaitAttempt(run.Id, time.Minute)
	ns := w.Namespace(w.TenantA)
	job := i.WaitJob(ns, attempt, true, 2*time.Minute)
	if got := w.Attempts(run.Id)[0].JobName; got == "" || got != job.Name || job.Attempt() != attempt {
		t.Fatalf("workload %s (attempt %s) is not the attempt's stable job %s", job.Name, job.Attempt(), got)
	}
	if n := i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
		t.Fatalf("%d attempts", n)
	}
	pod := i.WaitPod(ns, job.Name, time.Minute)
	i.WaitLaunched(ns, pod.Name)

	final := w.WaitRun(run.Id, 4*time.Minute)
	if final.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", final.Status, final.Reason)
	}
	ops := runtimeOps(t, i, attempt)
	for _, op := range []string{"admit", "getContext", "submitResult"} {
		if ops[op] < 1 {
			t.Errorf("missing runtime operation %s (saw %v)", op, ops)
		}
	}

	steps := w.Steps(run.Id)
	outs := *steps[0].Outputs
	if v, err := outs["v"].AsRunsScalar0(); err != nil || v != "ok" {
		t.Fatalf("output v = %q (%v)", v, err)
	}
	artifactID, err := outs["report"].AsRunsScalar0()
	if err != nil || artifactID == "" {
		t.Fatalf("the artifact output %v (%v)", outs["report"], err)
	}
	download := true
	art, err := i.API("engineer").GetArtifactWithResponse(ctx, w.TenantA, w.Project, artifactID, &client.GetArtifactParams{Download: &download})
	if err != nil || art.StatusCode() != http.StatusOK {
		t.Fatalf("artifact: %v %s", err, art.Body)
	}
	var transfer struct {
		Download struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"download"`
	}
	mustJSON(t, art.Body, &transfer)
	req, _ := http.NewRequest(http.MethodGet, transfer.Download.URL, nil)
	for k, v := range transfer.Download.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(data) != "artifact bytes\n" {
		t.Fatalf("download: %d %q", resp.StatusCode, data)
	}
	// Reservation and completion are the artifact record the download just verified.
	// The runtime route does not log those operations under a stable op name.
	if n := i.OwnerCount(`SELECT count(*) FROM artifacts WHERE id = $1 AND state = 'usable'`, artifactID); n != 1 {
		t.Fatalf("artifact %s was not reserved and completed (status rows %d)", artifactID, n)
	}
	sum := sha256.Sum256(data)
	if !strings.Contains(string(art.Body), hex.EncodeToString(sum[:])) {
		t.Fatalf("the artifact record carries no digest")
	}

	w.WaitRemoved(attempt)
	if lines := w.WaitLogs("engineer", run.Id); len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "roundtrip log line") {
		t.Fatalf("retained log %q", lines)
	}

	var order []string
	events.Until(t, "the run's terminal event", 30*time.Second, func(f stage.Frame) bool {
		if !strings.Contains(f.Data, run.Id) && !strings.Contains(f.Data, attempt) {
			return false
		}
		if f.Type() == "step_attempt.state_changed" && strings.Contains(f.Data, `"succeeded"`) {
			order = append(order, "attempt")
		}
		if f.Type() == "run.state_changed" && strings.Contains(f.Data, `"succeeded"`) {
			order = append(order, "run")
			return true
		}
		return false
	})
	events.Cancel()
	if strings.Join(order, ",") != "attempt,run" {
		t.Fatalf("terminal events arrived as %v", order)
	}

	if n := i.OwnerCount(`SELECT count(*) FROM runtime_grants WHERE attempt_id = $1`, attempt); n != 1 {
		t.Fatalf("%d runtime grants for one attempt", n)
	}
	if n := i.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND revoked_at IS NOT NULL AND workload_removed_at IS NOT NULL`, attempt); n != 1 {
		t.Fatalf("the attempt keeps authority or a workload")
	}
	digest := final.InputSnapshot.Digest
	second := w.Run("roundtrip-rerun", item, def.Id)
	if second.Id == run.Id {
		t.Fatal("a rerun reused the run")
	}
	if w.WaitRun(second.Id, 4*time.Minute).Status != client.RunStatusSucceeded {
		t.Fatalf("rerun %s", w.GetRun(second.Id).Status)
	}
	if got := w.GetRun(run.Id); got.InputSnapshot.Digest != digest || got.Status != client.RunStatusSucceeded {
		t.Fatalf("the first run changed after a rerun: status %s", got.Status)
	}
	if n := i.OwnerCount(`SELECT count(*) FROM runs WHERE work_item_id = $1`, item.Id); n != 2 {
		t.Fatalf("%d runs", n)
	}
	if n := i.TenantCount(w.TenantA, `SELECT count(*) FROM run_history WHERE run_id = $1`, run.Id); n < 3 {
		t.Fatalf("%d history rows", n)
	}
	replay := w.CreateRun("engineer", "roundtrip-run", item, def.Id)
	if replay.StatusCode() != http.StatusCreated || replay.JSON201.Id != run.Id || replay.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d", replay.StatusCode())
	}
}

func cancelRestartKube(t *testing.T) {
	t.Helper()
	t.Run("cancel committed while Job delete is refused, then the next process finishes it", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Cancel then restart", "# c")
		w.Provision(item.Id)
		def := w.Definition("sleeper", stage.Step{Script: `sleep 300`})
		ns := w.Namespace(w.TenantA)
		hold := i.HoldJobs(i.Context(2*time.Minute), ns, "DELETE")
		run := w.Run("cancel-then-restart", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		job := i.WaitJob(ns, attempt, true, 2*time.Minute)
		pod := i.WaitPod(ns, job.Name, time.Minute)
		i.WaitLaunched(ns, pod.Name)
		token := i.RuntimeToken(ns, pod.Name)

		if r := w.CancelRun(run.Id); r.StatusCode() != http.StatusAccepted {
			t.Fatalf("cancel: %d", r.StatusCode())
		}
		// The cancellation is committed. The server cannot delete the Job while the hold stands.
		time.Sleep(8 * time.Second)
		if len(i.JobsOfAttempt(ns, attempt)) != 1 {
			t.Fatal("the held delete removed the Job")
		}
		i.Stop(i.Context(3 * time.Minute))
		hold.Release()
		i.Start(i.Context(6 * time.Minute))

		if final := w.WaitRun(run.Id, 3*time.Minute); final.Status != client.RunStatusCancelled {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		w.WaitRemoved(attempt)
		if n := i.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND revoked_at IS NOT NULL`, attempt); n != 1 {
			t.Fatal("the attempt kept its authority")
		}
		if code := kubeLateResult(t, i, attempt, token); code < 400 {
			t.Fatalf("a late result was accepted: HTTP %d", code)
		}
		if got := w.GetRun(run.Id); got.Status != client.RunStatusCancelled {
			t.Fatalf("a late result changed the run to %s", got.Status)
		}
		if n := i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
			t.Fatalf("%d attempts: the restart dispatched again", n)
		}
	})

	t.Run("restart, then cancel the adopted attempt", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Restart then cancel", "# c")
		w.Provision(item.Id)
		def := w.Definition("sleeper", stage.Step{Script: `sleep 300`})
		ns := w.Namespace(w.TenantA)
		run := w.Run("restart-then-cancel", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		job := i.WaitJob(ns, attempt, true, 2*time.Minute)
		pod := i.WaitPod(ns, job.Name, time.Minute)
		i.WaitLaunched(ns, pod.Name)
		uid := job.UID
		// A graceful scale-down can reconcile the workload away. Kill the process
		// so the next one has to adopt the Job the dead process left behind.
		crashCtx := i.Context(2 * time.Minute)
		if _, err := i.Kube.Run(crashCtx, "-n", i.S.Namespace, "delete", "pod", "-l", "app.kubernetes.io/component=server", "--force", "--grace-period=0"); err != nil {
			t.Fatalf("crash the server: %v", err)
		}
		if _, err := i.Kube.Run(i.Context(5*time.Minute), "-n", i.S.Namespace, "rollout", "status", "deploy/"+i.Deployment(), "--timeout=4m"); err != nil {
			t.Fatalf("server rollout: %v", err)
		}
		i.WaitReady(i.Context(3 * time.Minute))
		stage.WaitFor(t, "the restarted process to adopt "+uid, 90*time.Second, func() bool {
			again := i.JobsOfAttempt(ns, attempt)
			return len(again) == 1 && again[0].UID == uid
		})
		w.CancelRun(run.Id)
		if final := w.WaitRun(run.Id, 3*time.Minute); final.Status != client.RunStatusCancelled {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		w.WaitRemoved(attempt)
		if n := i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
			t.Fatalf("%d attempts", n)
		}
	})

	t.Run("a foreign Job at the workload name is never adopted or touched", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Squatter", "# s")
		w.Provision(item.Id)
		def := w.Definition("squatted", stage.Step{Script: `echo should-not-run`})
		ns := w.Namespace(w.TenantA)
		ctx := i.Context(3 * time.Minute)
		hold := i.HoldJobs(ctx, ns, "CREATE")
		run := w.Run("squatted-run", item, def.Id)
		w.WaitSourceRetained(run.Id)
		var name, attempt string
		stage.WaitFor(t, "the attempt's deterministic Job name", time.Minute, func() bool {
			atts := w.Attempts(run.Id)
			if len(atts) == 0 || atts[0].JobName == "" {
				return false
			}
			name, attempt = atts[0].JobName, atts[0].Id
			return true
		})
		uid, spec := createForeignJob(t, i, ns, name)
		hold.Release()
		// The server refuses the foreign object (conflict) and never adopts it.
		// The run may stay unconfirmed until the startup budget; success is the failure.
		stage.WaitFor(t, "the server to refuse the foreign Job", 3*time.Minute, func() bool {
			logs, err := i.Kube.Run(context.Background(), "-n", i.S.Namespace, "logs", "deploy/"+i.Deployment(), "--since=10m")
			if err != nil {
				return false
			}
			return strings.Contains(logs, attempt) && strings.Contains(logs, `"error":"conflict"`)
		})
		if st, ok := w.RunStatus(run.Id); ok && st == string(client.RunStatusSucceeded) {
			t.Fatal("the server reported success for a foreign Job")
		}
		gotUID, gotSpec := jobIdentity(t, i, ns, name)
		if gotUID != uid || gotSpec != spec {
			t.Fatalf("the foreign Job was replaced or mutated")
		}
		if n := i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
			t.Fatalf("%d attempts", n)
		}
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = i.Kube.Run(c, "-n", ns, "delete", "job", name, "--ignore-not-found", "--wait=false")
	})
}

func resultGuardKube(t *testing.T) {
	t.Helper()
	i := stage.New(t)
	w := i.Seed()
	_, other := w.Scripted("other-attempt", stage.Step{Script: `echo other`})
	w.WaitRun(other.Id, 4*time.Minute)
	w.WaitLogs("engineer", other.Id)
	var foreign string
	i.MustScan(&foreign, `SELECT id FROM artifacts WHERE tenant_id = $1 ORDER BY created_at LIMIT 1`, w.TenantA)

	item := w.WorkItem("Guarded", "# guard")
	w.Provision(item.Id)
	def := w.Definition("guarded", stage.Step{Script: `sleep 300`, Outputs: []string{"v"}})
	ns := w.Namespace(w.TenantA)
	run := w.Run("guard-run", item, def.Id)
	attempt := w.WaitAttempt(run.Id, time.Minute)
	job := i.WaitJob(ns, attempt, true, 2*time.Minute)
	pod := i.WaitPod(ns, job.Name, time.Minute)
	i.WaitLaunched(ns, pod.Name)
	token := i.RuntimeToken(ns, pod.Name)
	running := func() bool {
		return i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE id = $1 AND status = 'running' AND result_hash IS NULL`, attempt) == 1
	}
	ok := map[string]runtimev1.Value{"v": runtimev1.StringValue("ok")}

	for idx, tc := range []struct {
		name    string
		outputs map[string]runtimev1.Value
	}{
		{"a missing declared output", nil},
		{"an undeclared output", map[string]runtimev1.Value{"v": runtimev1.StringValue("ok"), "extra": runtimev1.StringValue("x")}},
		{"a mistyped output", map[string]runtimev1.Value{"v": runtimev1.IntValue(3)}},
	} {
		itemX := w.WorkItem("Invalid outputs", "# invalid")
		w.Provision(itemX.Id)
		runX := w.Run("invalid-outputs-"+string(rune('a'+idx)), itemX, def.Id)
		attX := w.WaitAttempt(runX.Id, time.Minute)
		jobX := i.WaitJob(ns, attX, true, 2*time.Minute)
		podX := i.WaitPod(ns, jobX.Name, time.Minute)
		i.WaitLaunched(ns, podX.Name)
		code, body := kubeSubmit(t, i, attX, i.RuntimeToken(ns, podX.Name), completed(tc.outputs), "")
		ackX, err := runtimev1.DecodeResponse[runtimev1.ResultResponse](body)
		if code != http.StatusOK || err != nil || ackX.AttemptStatus != "failed" {
			t.Fatalf("%s: HTTP %d", tc.name, code)
		}
		if final := w.WaitRun(runX.Id, 3*time.Minute); final.Status != client.RunStatusFailed {
			t.Fatalf("%s: run %s", tc.name, final.Status)
		}
		if st := w.Steps(runX.Id)[0]; st.Outputs != nil || st.Status != client.StepStatusFailed {
			t.Fatalf("%s: a failed step exposes outputs", tc.name)
		}
		w.WaitRemoved(attX)
	}

	for name, tc := range map[string]struct {
		res  runtimev1.Result
		hash runtimev1.Digest
		code string
	}{
		"a result that fails its hash": {res: completed(ok), hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000", code: "result_hash_mismatch"},
		"another attempt's artifact": {res: func() runtimev1.Result {
			r := completed(ok)
			r.DiagnosticArtifactIDs = []domain.ArtifactID{domain.ArtifactID(foreign)}
			return r
		}()},
	} {
		code, body := kubeSubmit(t, i, attempt, token, tc.res, tc.hash)
		errCode := stage.ErrorCode(body)
		if code < 400 || code >= 500 || (tc.code != "" && errCode != tc.code) {
			t.Errorf("%s: HTTP %d %s", name, code, errCode)
		}
		if !running() {
			t.Fatalf("%s changed the attempt", name)
		}
	}
	if got := w.GetRun(run.Id); got.Status != client.RunStatusRunning {
		t.Fatalf("a refused result moved the run to %s", got.Status)
	}

	i.RelayDown(i.Context(2 * time.Minute))
	stage.WaitFor(t, "readiness to fail while the database path is down", 90*time.Second, func() bool {
		resp, _, err := i.TryRaw(http.MethodGet, "/readyz", "", nil, nil)
		return err != nil || resp.StatusCode != http.StatusOK
	})
	if code, _ := kubeSubmit(t, i, attempt, token, completed(ok), ""); code != http.StatusServiceUnavailable && code != 0 {
		t.Errorf("a result during a database outage: HTTP %d", code)
	}
	i.RelayUp(i.Context(3 * time.Minute))
	stage.WaitFor(t, "readiness after the outage", 90*time.Second, func() bool {
		resp, _, err := i.TryRaw(http.MethodGet, "/readyz", "", nil, nil)
		return err == nil && resp.StatusCode == http.StatusOK
	})
	if !running() || i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE id = $1 AND result IS NOT NULL`, attempt) != 0 {
		t.Fatal("the outage left a partial commit")
	}

	code, body := kubeSubmit(t, i, attempt, token, completed(ok), "")
	if code != http.StatusOK {
		t.Fatalf("the valid result: HTTP %d", code)
	}
	ack, err := runtimev1.DecodeResponse[runtimev1.ResultResponse](body)
	if err != nil || ack.AttemptStatus != "succeeded" {
		t.Fatalf("acknowledgment (%v)", err)
	}
	again, replay := kubeSubmit(t, i, attempt, token, completed(ok), "")
	if again != http.StatusOK {
		t.Fatalf("an identical replay: HTTP %d", again)
	}
	if r2, err := runtimev1.DecodeResponse[runtimev1.ResultResponse](replay); err != nil || r2.ResultHash != ack.ResultHash || r2.AttemptStatus != ack.AttemptStatus {
		t.Fatal("the replay answered a different result")
	}
	if code, _ := kubeSubmit(t, i, attempt, token, completed(map[string]runtimev1.Value{"v": runtimev1.StringValue("changed")}), ""); code != http.StatusConflict && code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("a different result for a finished attempt: HTTP %d", code)
	}
	if n := i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE id = $1 AND status = 'succeeded'`, attempt); n != 1 {
		t.Fatal("the attempt is not succeeded exactly once")
	}
	final := w.WaitRun(run.Id, 3*time.Minute)
	if final.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", final.Status, final.Reason)
	}
	if v, err := (*w.Steps(run.Id)[0].Outputs)["v"].AsRunsScalar0(); err != nil || v != "ok" {
		t.Fatalf("output %q (%v)", v, err)
	}
	w.WaitRemoved(attempt)
	if code, _ := kubeSubmit(t, i, attempt, token, completed(map[string]runtimev1.Value{"v": runtimev1.StringValue("late")}), ""); code < 400 {
		t.Fatalf("a different result after the workload was removed: HTTP %d", code)
	}
	if v, _ := (*w.Steps(run.Id)[0].Outputs)["v"].AsRunsScalar0(); v != "ok" {
		t.Fatalf("a late result rewrote the output to %q", v)
	}
}

func kubeLateResult(t *testing.T, i *stage.Installation, attempt, token string) int {
	t.Helper()
	code, _ := kubeSubmit(t, i, attempt, token, completed(nil), "")
	return code
}

func kubeSubmit(t *testing.T, i *stage.Installation, attempt, token string, res runtimev1.Result, hash runtimev1.Digest) (int, []byte) {
	t.Helper()
	if res.Outputs == nil {
		res.Outputs = map[string]runtimev1.Value{}
	}
	if res.DiagnosticArtifactIDs == nil {
		res.DiagnosticArtifactIDs = []domain.ArtifactID{}
	}
	if hash == "" {
		h, err := runtimev1.ResultHash(res)
		if err != nil {
			t.Fatal(err)
		}
		hash = h
	}
	resp, body, err := i.TryRaw(http.MethodPost, "/runtime/v1/attempts/"+attempt+"/result", token, stage.JSONBody(t, runtimev1.ResultRequest{ResultHash: hash, Result: res}), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return 0, nil
	}
	return resp.StatusCode, body
}

func runtimeOps(t *testing.T, i *stage.Installation, attempt string) map[string]int {
	t.Helper()
	out := map[string]int{}
	logs, err := i.Kube.Run(context.Background(), "-n", i.S.Namespace, "logs", "deploy/"+i.Deployment(), "--since=30m")
	if err != nil {
		t.Fatalf("server logs: %v", err)
	}
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, attempt) {
			continue
		}
		var ev struct {
			Op  string `json:"op"`
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Op == "" {
			continue
		}
		if ev.Msg == "runtime request" || ev.Msg == "runtime authority" {
			out[ev.Op]++
		}
	}
	return out
}

func createForeignJob(t *testing.T, i *stage.Installation, ns, name string) (uid, spec string) {
	t.Helper()
	manifest := `apiVersion: batch/v1
kind: Job
metadata: {name: ` + name + `, namespace: ` + ns + `}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: c
          image: ` + i.WorkloadImage() + `
          command: ["sleep", "600"]
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
`
	if _, err := i.Kube.RunInput(context.Background(), []byte(manifest), "apply", "-f", "-"); err != nil {
		t.Fatalf("foreign job: %v", err)
	}
	return jobIdentity(t, i, ns, name)
}

func jobIdentity(t *testing.T, i *stage.Installation, ns, name string) (uid, spec string) {
	t.Helper()
	uid, err := i.Kube.Run(context.Background(), "-n", ns, "get", "job", name, "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		t.Fatalf("foreign job uid: %v", err)
	}
	spec, err = i.Kube.Run(context.Background(), "-n", ns, "get", "job", name, "-o", "jsonpath={.spec}")
	if err != nil {
		t.Fatalf("foreign job spec: %v", err)
	}
	return strings.TrimSpace(uid), strings.TrimSpace(spec)
}

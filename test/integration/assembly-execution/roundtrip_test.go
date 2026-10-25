package assembly_execution_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// TestExecutionAssemblyRoundtrip (execution_assembly_roundtrip): the actual
// callbacks of the real entrypoint through the real server. The attempt's
// identity is stable and the workload it names is the one that runs (PX-03); the
// transitions commit with their events in order (PX-05); the declared file output
// is uploaded to S3, verified and downloadable, the scalar output is finalized and
// the retained log outlives the workload (PX-10); a rerun is a new run and the
// first keeps its inputs and history (PX-12); runtime admission is one-time and the
// workload's authority ends with the attempt (IT-03, IT-06); the stored responses
// replay (AC-06, AC-12).
func TestExecutionAssemblyRoundtrip(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.Name, func(t *testing.T) {
			if b.Name == "kube" {
				roundtripKube(t)
				return
			}
			s := kit.New(t, kit.Options{})
			s.Start()
			s.WaitReady()
			w := s.Seed(true)
			ctx := t.Context()

			item := w.WorkItem("Roundtrip", "# roundtrip")
			w.Provision(item.Id)
			def := w.PodmanDefinition("roundtrip", kit.Step{
				Script:    `echo "roundtrip log line"; echo "artifact bytes" > /kodama/outputs/report; sleep 4; printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`,
				Outputs:   []string{"v"},
				Artifacts: map[string]string{"report": "/kodama/outputs/report"},
			})
			events := s.OpenEvents("engineer", w.ProjectEvents(), "")
			events.Until(t, "the snapshot", 10*time.Second, func(f kit.Frame) bool { return f.Event == "snapshot" })
			callbacks := s.Front.Requests()

			run := w.Run("roundtrip-run", item, def.Id)
			attempt := w.WaitAttempt(run.Id, time.Minute)
			c := s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
			job := w.Attempts(run.Id)[0].JobName
			if len(c.Names) != 1 || c.Names[0] != job || c.Attempt() != attempt {
				t.Fatalf("workload %v (attempt label %q) is not the attempt's stable %s / %s", c.Names, c.Attempt(), job, attempt)
			}
			if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
				t.Fatalf("%d attempts", n)
			}

			final := w.WaitRun(run.Id, 3*time.Minute)
			if final.Status != client.RunStatusSucceeded {
				t.Fatalf("run %s (%s)", final.Status, final.Reason)
			}
			if got := s.Front.Requests() - callbacks; got < 5 {
				t.Fatalf("the entrypoint made %d runtime API callbacks (admit, context, reserve, complete, result)", got)
			}

			// Outputs: the scalar and the verified artifact.
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
			art, err := s.API("engineer").GetArtifactWithResponse(ctx, kit.TenantA, w.Project, artifactID, &client.GetArtifactParams{Download: &download})
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
			if transfer.Download.URL == "" {
				t.Fatalf("no download instruction: %s", art.Body)
			}
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
			sum := sha256.Sum256(data)
			if !strings.Contains(string(art.Body), hex.EncodeToString(sum[:])) {
				t.Fatalf("the artifact record carries no digest %x: %s", sum, art.Body)
			}

			// The log is retained before the workload goes; the workload is removed once.
			w.WaitRemoved(attempt)
			if lines := w.WaitLogs("engineer", run.Id); len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "roundtrip log line") {
				t.Fatalf("retained log %q", lines)
			}

			// Events: the attempt's success is committed and delivered before the run's.
			var order []string
			events.Until(t, "the run's terminal event", 30*time.Second, func(f kit.Frame) bool {
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

			// Admission is one-time and grants end with the attempt; history is durable (IT-03, IT-06, PX-12).
			if n := s.OwnerCount(`SELECT count(*) FROM runtime_grants WHERE attempt_id = $1`, attempt); n != 1 {
				t.Fatalf("%d runtime grants for one attempt", n)
			}
			if n := s.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND revoked_at IS NOT NULL AND workload_removed_at IS NOT NULL`, attempt); n != 1 {
				t.Fatalf("the attempt keeps authority or a workload")
			}
			digest := final.InputSnapshot.Digest
			second := w.Run("roundtrip-rerun", item, def.Id)
			if second.Id == run.Id {
				t.Fatal("a rerun reused the run")
			}
			if w.WaitRun(second.Id, 3*time.Minute).Status != client.RunStatusSucceeded {
				t.Fatalf("rerun %s", w.GetRun(second.Id).Status)
			}
			if got := w.GetRun(run.Id); got.InputSnapshot.Digest != digest || got.Status != client.RunStatusSucceeded {
				t.Fatalf("the first run changed after a rerun: %+v", got)
			}
			if n := s.OwnerCount(`SELECT count(*) FROM runs WHERE work_item_id = $1`, item.Id); n != 2 {
				t.Fatalf("%d runs", n)
			}
			if n := s.TenantCount(kit.TenantA, `SELECT count(*) FROM run_history WHERE run_id = $1`, run.Id); n < 3 {
				t.Fatalf("%d history rows", n)
			}

			// Stored responses replay (AC-06, AC-12): the same key returns the first admission.
			replay := w.CreateRun("engineer", "roundtrip-run", item, def.Id)
			if replay.StatusCode() != http.StatusCreated || replay.JSON201.Id != run.Id || replay.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
				t.Fatalf("replay: %d %s", replay.StatusCode(), replay.Body)
			}
		})
	}
}

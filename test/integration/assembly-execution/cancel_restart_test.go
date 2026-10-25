package assembly_execution_test

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// TestExecutionAssemblyCancelRestart (execution_assembly_cancel_restart):
// cancellation and process restart in both orders, against real containers. A
// cancellation committed just before the process stops is finished by the next
// process, which stops the container, settles the attempt and removes the
// workload; a restart before the cancellation changes nothing about it; a late
// result with the dead attempt's token is refused after either; and a
// container that merely holds the workload's name is never adopted, stopped or
// removed. PX-A06, PX-08, PX-03, PX-A02, PX-16, IT-06.
func TestExecutionAssemblyCancelRestart(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.Name, func(t *testing.T) {
			if b.Name == "kube" {
				cancelRestartKube(t)
				return
			}
			t.Run("cancel, then the process stops before it finishes", func(t *testing.T) {
				s := kit.New(t, kit.Options{})
				s.Start()
				s.WaitReady()
				w := s.Seed(true)
				item := w.WorkItem("Cancel then restart", "# c")
				w.Provision(item.Id)
				def := w.PodmanDefinition("sleeper", kit.Step{Script: `sleep 300`})
				run := w.Run("cancel-then-restart", item, def.Id)
				attempt := w.WaitAttempt(run.Id, time.Minute)
				c := s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
				s.WaitLaunched(c.ID)
				token := s.RuntimeToken(c.ID)

				if r := w.CancelRun(run.Id); r.StatusCode() != http.StatusAccepted {
					t.Fatalf("cancel: %d %s", r.StatusCode(), r.Body)
				}
				s.Stop() // the cancellation is committed; whatever the first process had not done is left to the next
				s.Start()
				s.WaitReady()

				if final := w.WaitRun(run.Id, 2*time.Minute); final.Status != client.RunStatusCancelled {
					t.Fatalf("run %s (%s)", final.Status, final.Reason)
				}
				w.WaitRemoved(attempt)
				if n := s.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND revoked_at IS NOT NULL`, attempt); n != 1 {
					t.Fatal("the attempt kept its authority")
				}
				if code, errCode := lateResult(t, s, attempt, token); code < 400 {
					t.Fatalf("a late result: HTTP %d %s", code, errCode)
				}
				if got := w.GetRun(run.Id); got.Status != client.RunStatusCancelled {
					t.Fatalf("a late result changed the run to %s", got.Status)
				}
				if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
					t.Fatalf("%d attempts: the restart dispatched again", n)
				}
			})

			t.Run("restart, then cancel the adopted attempt", func(t *testing.T) {
				s := kit.New(t, kit.Options{})
				s.Start()
				s.WaitReady()
				w := s.Seed(true)
				item := w.WorkItem("Restart then cancel", "# c")
				w.Provision(item.Id)
				def := w.PodmanDefinition("sleeper", kit.Step{Script: `sleep 300`})
				run := w.Run("restart-then-cancel", item, def.Id)
				attempt := w.WaitAttempt(run.Id, time.Minute)
				c := s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
				s.WaitLaunched(c.ID)

				s.Restart()
				if cs := s.Containers(kit.TenantA, attempt); len(cs) != 1 || cs[0].ID != c.ID {
					t.Fatalf("the restarted process did not adopt %s: %+v", c.ID, cs)
				}
				w.CancelRun(run.Id)
				if final := w.WaitRun(run.Id, 2*time.Minute); final.Status != client.RunStatusCancelled {
					t.Fatalf("run %s (%s)", final.Status, final.Reason)
				}
				w.WaitRemoved(attempt)
				if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
					t.Fatalf("%d attempts", n)
				}
			})

			t.Run("a container that holds the workload's name is never adopted or touched", func(t *testing.T) {
				// Neither the dispatch loop nor the periodic repair runs on its own: the test claims
				// the attempt itself, so it knows the workload's name before anything is started and
				// can occupy it, and then drives the repair pass that redrives the unconfirmed attempt.
				s := kit.New(t, kit.Options{NoDispatch: true, Tune: func(tn *app.Tuning) {
					tn.Limit.StartupBudget = 10 * time.Second
					tn.ReconcileInterval = time.Hour
				}})
				s.Start()
				s.WaitReady()
				w := s.Seed(true)
				item := w.WorkItem("Squatter", "# s")
				w.Provision(item.Id)
				def := w.PodmanDefinition("squatted", kit.Step{Script: `echo should-not-run`})
				run := w.Run("squatted-run", item, def.Id)
				w.WaitSourceRetained(run.Id)

				spec, err := s.Server().Core().Execution.ClaimRunnable(t.Context(), domain.TenantID(kit.TenantA))
				if err != nil || spec == nil {
					t.Fatalf("claim: %v", err)
				}
				squatter, err := exec.Command("podman", "run", "-d", "--name", spec.WorkloadName, "--network", "none", kit.PodmanImage, "sleep", "600").CombinedOutput()
				if err != nil {
					t.Fatalf("the squatter: %v: %s", err, squatter)
				}
				id := strings.TrimSpace(string(squatter))
				t.Cleanup(func() { _ = exec.Command("podman", "rm", "-f", "-t", "0", id).Run() })

				// The repair pass redrives the unconfirmed attempt; the runtime refuses the foreign
				// container, and the attempt cannot start before its startup deadline fails it.
				core := s.Server().Core()
				kit.WaitFor(t, "the run to fail for want of a workload it may start", 3*time.Minute, func() bool {
					_, _ = core.Execution.ReconcileTenant(t.Context(), domain.TenantID(kit.TenantA))
					st, _ := w.RunStatus(run.Id)
					return st == string(client.RunStatusFailed)
				})
				if final := w.GetRun(run.Id); final.Status != client.RunStatusFailed {
					t.Fatalf("run %s (%s)", final.Status, final.Reason)
				}
				out, err := exec.Command("podman", "inspect", "--format", "{{.State.Status}} {{.Id}} {{len .Config.Labels}}", spec.WorkloadName).CombinedOutput()
				if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "running "+id+" ") {
					t.Fatalf("the foreign container was stopped, replaced or removed: %v %s", err, out)
				}
				if cs := s.Containers(kit.TenantA, string(spec.AttemptID)); len(cs) != 0 {
					t.Fatalf("a managed container exists for the attempt: %+v", cs)
				}
				if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
					t.Fatalf("%d attempts", n)
				}
			})
		})
	}
}

package m1_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// lateResult submits a completed result with a workload's own bearer token.
func lateResult(t *testing.T, s *kit.Stack, attempt, token string) (int, string) {
	t.Helper()
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: new(int64), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	h, err := runtimev1.ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := s.Raw("POST", "/runtime/v1/attempts/"+attempt+"/result", token, kit.JSONBody(t, runtimev1.ResultRequest{ResultHash: h, Result: res}), map[string]string{"Content-Type": "application/json"})
	return resp.StatusCode, kit.ErrorCode(body)
}

// TestM1CancelRace (m1_cancel_race): cancellation against real containers.
// A cancelled run stops its container, revokes the attempt (a late result with
// the workload's own token is refused and cannot change the run), settles and
// removes the workload; a cancellation racing the dispatch at every offset
// leaves no container and no live authority; and a cancelled work item reaches
// its running attempt through the event consumer, or, with the broker away,
// through the periodic scan. PX-A06, PX-08, PX-09, IT-A05, WS-A03.
func TestM1CancelRace(t *testing.T) {
	s := kit.New(t, kit.Options{})
	s.Start()
	s.WaitReady()
	w := s.Seed(true)
	sleeper := w.PodmanDefinition("sleeper", kit.Step{Script: `echo started; sleep 300`})

	t.Run("a cancelled run stops its container and rejects a late result", func(t *testing.T) {
		item := w.WorkItem("Cancel running", "# cancel")
		w.Provision(item.Id)
		run := w.Run("cancel-running", item, sleeper.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		c := s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
		token := s.RuntimeToken(c.ID)

		cancelled := w.CancelRun(run.Id)
		if cancelled.StatusCode() != http.StatusAccepted || cancelled.JSON202.CancelRequestedAt == nil {
			t.Fatalf("cancel: %d %s", cancelled.StatusCode(), cancelled.Body)
		}
		// New authority is gone at once; the run stays nonterminal until the workload is settled.
		kit.WaitFor(t, "the attempt to be revoked", 30*time.Second, func() bool {
			return s.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND revoked_at IS NOT NULL`, attempt) == 1
		})
		code, errCode := lateResult(t, s, attempt, token)
		if code != http.StatusForbidden && code != http.StatusUnauthorized || (errCode != "attempt_revoked" && errCode != "invalid_credential") {
			t.Fatalf("a late result with the workload's token: %d %s", code, errCode)
		}
		final := w.WaitRun(run.Id, 2*time.Minute)
		if final.Status != client.RunStatusCancelled {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		w.WaitRemoved(attempt)
		if code, errCode = lateResult(t, s, attempt, token); code < 400 {
			t.Fatalf("a result after the workload was removed: %d %s", code, errCode)
		}
		if got := w.GetRun(run.Id); got.Status != client.RunStatusCancelled {
			t.Fatalf("a late result changed the run to %s", got.Status)
		}
		if steps := w.Steps(run.Id); steps[0].Outputs != nil || steps[0].Attempts[0].Status == client.AttemptStatusSucceeded {
			t.Fatalf("the cancelled attempt %+v", steps[0])
		}
		// Cancelling again, or a terminal run, changes nothing.
		if again := w.CancelRun(run.Id); again.StatusCode() != http.StatusAccepted || again.JSON202.Status != client.RunStatusCancelled {
			t.Fatalf("repeat cancel: %d %s", again.StatusCode(), again.Body)
		}
	})

	t.Run("cancellation racing dispatch leaves no container and no live authority", func(t *testing.T) {
		delays := []time.Duration{0, 40 * time.Millisecond, 150 * time.Millisecond, 400 * time.Millisecond, time.Second}
		var runs []string
		for i, d := range delays {
			item := w.WorkItem("Cancel race", "# race")
			w.Provision(item.Id)
			run := w.Run("cancel-race-"+d.String(), item, sleeper.Id)
			time.Sleep(d)
			if r := w.CancelRun(run.Id); r.StatusCode() != http.StatusAccepted {
				t.Fatalf("cancel %d: %d %s", i, r.StatusCode(), r.Body)
			}
			runs = append(runs, run.Id)
		}
		for _, id := range runs {
			if final := w.WaitRun(id, 2*time.Minute); final.Status != client.RunStatusCancelled {
				t.Fatalf("run %s is %s (%s)", id, final.Status, final.Reason)
			}
		}
		kit.WaitFor(t, "every workload to be removed", 90*time.Second, func() bool {
			return len(s.Containers(kit.TenantA, "")) == 0
		})
		if n := s.OwnerCount(`SELECT count(*) FROM attempt_controls c JOIN step_attempts a ON a.tenant_id = c.tenant_id AND a.id = c.attempt_id
			WHERE a.run_id = ANY($1) AND (c.revoked_at IS NULL OR c.workload_removed_at IS NULL) AND c.dispatch_confirmed_at IS NOT NULL`, runs); n != 0 {
			t.Fatalf("%d dispatched attempts keep authority or a workload", n)
		}
		// A run cancelled before its dispatch never claimed an attempt.
		if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = ANY($1) AND status = 'running'`, runs); n != 0 {
			t.Fatalf("%d attempts are still running", n)
		}
	})

	t.Run("a cancelled work item reaches its running attempt through the consumer, or the scan when the broker is away", func(t *testing.T) {
		for _, brokerDown := range []bool{false, true} {
			item := w.WorkItem("Cancel item", "# item")
			w.Provision(item.Id)
			run := w.Run("cancel-item-"+time.Now().Format("150405.000"), item, sleeper.Id)
			attempt := w.WaitAttempt(run.Id, time.Minute)
			s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
			kit.WaitFor(t, "the outbox to drain", 30*time.Second, func() bool { return s.PendingEvents(kit.TenantA) == 0 })
			if brokerDown {
				s.NATS.Stop()
			}
			w.CancelItem(item)
			if final := w.WaitRun(run.Id, 2*time.Minute); final.Status != client.RunStatusCancelled {
				t.Fatalf("broker down %v: run %s (%s)", brokerDown, final.Status, final.Reason)
			}
			w.WaitRemoved(attempt)
			if brokerDown {
				s.NATS.Start()
			}
		}
	})
}

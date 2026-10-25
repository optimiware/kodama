package assembly_execution_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// submit posts a result as the workload's own token would, returning the HTTP
// status, the stable error code and the body.
func submit(t *testing.T, s *kit.Stack, attempt, token string, res runtimev1.Result, hash runtimev1.Digest) (int, string, []byte) {
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
	resp, body := s.Raw("POST", "/runtime/v1/attempts/"+attempt+"/result", token, kit.JSONBody(t, runtimev1.ResultRequest{ResultHash: hash, Result: res}), map[string]string{"Content-Type": "application/json"})
	return resp.StatusCode, kit.ErrorCode(body), body
}

func completed(outputs map[string]runtimev1.Value) runtimev1.Result {
	return runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: new(int64), Outputs: outputs}
}

// TestExecutionAssemblyResultGuard (execution_assembly_result_guard): the result
// route guards a real, running attempt. Results that fail their hash or cite another attempt's artifact
// are refused and change nothing, and one that misses, adds or mistypes a declared
// output fails the attempt instead of succeeding it (PX-10); a database outage during the callback
// rolls the whole commit back and the same result commits once afterwards
// (rollback and replay); the accepted result replays to the same answer, a
// different one conflicts and never rewrites it (AC-12, PX-12); and the attempt
// that delivered it is settled and its workload removed (PX-16, IT-06).
func TestExecutionAssemblyResultGuard(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.Name, func(t *testing.T) {
			if b.Name == "kube" {
				resultGuardKube(t)
				return
			}
			s := kit.New(t, kit.Options{})
			s.Start()
			s.WaitReady()
			w := s.Seed(true)

			// An artifact of some other attempt, for the cross-attempt reference.
			_, other := w.Scripted("other-attempt", kit.Step{Script: `echo other`})
			w.WaitRun(other.Id, 2*time.Minute)
			w.WaitLogs("engineer", other.Id)
			var foreign string
			s.MustScan(&foreign, `SELECT id FROM artifacts WHERE tenant_id = $1 ORDER BY created_at LIMIT 1`, kit.TenantA)

			item := w.WorkItem("Guarded", "# guard")
			w.Provision(item.Id)
			def := w.PodmanDefinition("guarded", kit.Step{Script: `sleep 300`, Outputs: []string{"v"}})
			run := w.Run("guard-run", item, def.Id)
			attempt := w.WaitAttempt(run.Id, time.Minute)
			c := s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
			s.WaitLaunched(c.ID)
			token := s.RuntimeToken(c.ID)
			running := func() bool {
				return s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE id = $1 AND status = 'running' AND result_hash IS NULL`, attempt) == 1
			}
			ok := map[string]runtimev1.Value{"v": runtimev1.StringValue("ok")}

			// Success finalization validates the declared outputs: a completed result that misses one,
			// carries an undeclared one or mistypes one is accepted as the attempt's failure, never as
			// a success, and the run fails with no success outputs (PX-10, PX-A07).
			for i, tc := range []struct {
				name    string
				outputs map[string]runtimev1.Value
			}{
				{"a missing declared output", nil},
				{"an undeclared output", map[string]runtimev1.Value{"v": runtimev1.StringValue("ok"), "extra": runtimev1.StringValue("x")}},
				{"a mistyped output", map[string]runtimev1.Value{"v": runtimev1.IntValue(3)}},
			} {
				name, outputs := tc.name, tc.outputs
				itemX := w.WorkItem("Invalid outputs", "# invalid")
				w.Provision(itemX.Id)
				runX := w.Run(fmt.Sprintf("invalid-outputs-%d", i), itemX, def.Id)
				attX := w.WaitAttempt(runX.Id, time.Minute)
				cX := s.WaitContainer(kit.TenantA, attX, "running", time.Minute)
				s.WaitLaunched(cX.ID)
				code, errCode, body := submit(t, s, attX, s.RuntimeToken(cX.ID), completed(outputs), "")
				ackX, err := runtimev1.DecodeResponse[runtimev1.ResultResponse](body)
				if code != http.StatusOK || err != nil || ackX.AttemptStatus != "failed" {
					t.Fatalf("%s: HTTP %d %s: %s", name, code, errCode, body)
				}
				if final := w.WaitRun(runX.Id, 2*time.Minute); final.Status != client.RunStatusFailed {
					t.Fatalf("%s: run %s", name, final.Status)
				}
				if st := w.Steps(runX.Id)[0]; st.Outputs != nil || st.Status != client.StepStatusFailed {
					t.Fatalf("%s: a failed step exposes outputs: %+v", name, st)
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
				code, errCode, body := submit(t, s, attempt, token, tc.res, tc.hash)
				if code < 400 || code >= 500 || (tc.code != "" && errCode != tc.code) {
					t.Errorf("%s: HTTP %d %s: %s", name, code, errCode, body)
				}
				if !running() {
					t.Fatalf("%s changed the attempt", name)
				}
			}
			if got := w.GetRun(run.Id); got.Status != client.RunStatusRunning {
				t.Fatalf("a refused result moved the run to %s", got.Status)
			}

			// A database outage during the callback: nothing commits, and the same result commits once later.
			s.PG.Stop()
			if code, errCode, body := submit(t, s, attempt, token, completed(ok), ""); code != http.StatusServiceUnavailable {
				t.Errorf("a result during a database outage: HTTP %d %s: %s", code, errCode, body)
			}
			s.PG.Start()
			kit.WaitFor(t, "readiness after the outage", 30*time.Second, func() bool {
				resp, _ := s.Raw("GET", "/readyz", "", nil, nil)
				return resp.StatusCode == http.StatusOK
			})
			if !running() || s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE id = $1 AND result IS NOT NULL`, attempt) != 0 {
				t.Fatal("the outage left a partial commit")
			}

			// The valid result commits; its replay answers the same; a different result conflicts.
			code, errCode, body := submit(t, s, attempt, token, completed(ok), "")
			if code != http.StatusOK {
				t.Fatalf("the valid result: HTTP %d %s: %s", code, errCode, body)
			}
			ack, err := runtimev1.DecodeResponse[runtimev1.ResultResponse](body)
			if err != nil || ack.AttemptStatus != "succeeded" {
				t.Fatalf("acknowledgment %+v (%v): %s", ack, err, body)
			}
			again, _, replay := submit(t, s, attempt, token, completed(ok), "")
			if again != http.StatusOK {
				t.Fatalf("an identical replay: HTTP %d: %s", again, replay)
			}
			if r2, err := runtimev1.DecodeResponse[runtimev1.ResultResponse](replay); err != nil || r2.ResultHash != ack.ResultHash || r2.AttemptStatus != ack.AttemptStatus {
				t.Fatalf("the replay answered %+v, first %+v", r2, ack)
			}
			if code, errCode, body := submit(t, s, attempt, token, completed(map[string]runtimev1.Value{"v": runtimev1.StringValue("changed")}), ""); code != http.StatusConflict && code != http.StatusForbidden && code != http.StatusUnauthorized {
				t.Fatalf("a different result for a finished attempt: HTTP %d %s: %s", code, errCode, body)
			}
			if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE id = $1 AND status = 'succeeded'`, attempt); n != 1 {
				t.Fatalf("the attempt is not succeeded exactly once")
			}

			final := w.WaitRun(run.Id, 2*time.Minute)
			if final.Status != client.RunStatusSucceeded {
				t.Fatalf("run %s (%s)", final.Status, final.Reason)
			}
			steps := w.Steps(run.Id)
			if v, err := (*steps[0].Outputs)["v"].AsRunsScalar0(); err != nil || v != "ok" {
				t.Fatalf("output %q (%v): a later result rewrote the accepted one", v, err)
			}
			// The attempt is settled, its authority is gone and its still-running workload is removed.
			w.WaitRemoved(attempt)
			if code, _, _ := submit(t, s, attempt, token, completed(map[string]runtimev1.Value{"v": runtimev1.StringValue("late")}), ""); code < 400 {
				t.Fatalf("a different result after the workload was removed: HTTP %d", code)
			}
			if code, _, _ := submit(t, s, attempt, token, completed(ok), ""); code != http.StatusOK && code < 400 {
				t.Fatalf("the accepted result no longer replays after settlement: HTTP %d", code)
			}
			if v, _ := (*w.Steps(run.Id)[0].Outputs)["v"].AsRunsScalar0(); v != "ok" {
				t.Fatalf("a late result rewrote the output to %q", v)
			}
		})
	}
}

// lateResult submits a completed result with a workload's own token.
func lateResult(t *testing.T, s *kit.Stack, attempt, token string) (int, string) {
	t.Helper()
	code, errCode, _ := submit(t, s, attempt, token, completed(nil), "")
	return code, errCode
}

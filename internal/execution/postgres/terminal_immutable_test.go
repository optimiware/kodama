package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// Test_terminal_immutable: PX-12, PX-02, PX-05. Late results, late
// observations, direct prohibited SQL updates and reruns cannot rewrite terminal
// history or pinned inputs.
func Test_terminal_immutable(t *testing.T) {
	ctx := context.Background()
	e := executiontest.New(t)
	e.GrantAll()
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.DocNamed("immut", "{region: {type: string, default: eu}}", pipelinetest.Container("job", readRun)))
	run := e.MustAdmit(p, def, nil)
	in := e.MustClaim(p.Scope.TenantID)
	e.Start(in)
	ack, err := e.Succeed(in, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	before := e.Get(run)
	if before.Run.Status != execution.RunSucceeded {
		t.Fatalf("run %s", before.Run.Status)
	}
	digest, inputs := before.Run.SnapshotDigest, before.Run.Snapshot.Inputs

	t.Run("late results", func(t *testing.T) {
		// The same hash replays the original acknowledgment.
		again, err := e.Succeed(in, nil, "")
		if err != nil || !again.Replayed || again.ResultHash != ack.ResultHash || again.AttemptStatus != execution.AttemptSucceeded {
			t.Fatalf("replay %+v %v", again, err)
		}
		// A conflicting result cannot rewrite the accepted one.
		if _, err := e.Fail(in); !errors.Is(err, execution.ErrResultConflict) {
			t.Fatalf("conflicting result: %v", err)
		}
	})

	t.Run("late observations are evidence only", func(t *testing.T) {
		for _, obs := range []execution.Observation{
			{Kind: execution.ObservedStarted, WorkloadUID: "11111111-2222-3333-4444-555555555555"},
			{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted},
		} {
			if err := e.Observe(in, obs); err != nil {
				t.Fatalf("%s: %v", obs.Kind, err)
			}
		}
		if n := e.Count(`SELECT count(*) FROM attempt_evidence WHERE kind = 'late_observation' AND attempt_id = $1`, string(in.AttemptID)); n != 2 {
			t.Fatalf("%d late observation evidence rows", n)
		}
		// A different Job's observation is refused and kept as mismatch evidence.
		if err := e.Observe(in, execution.Observation{Kind: execution.ObservedStarted, WorkloadName: "kdm-other", WorkloadUID: "99999999-2222-3333-4444-555555555555"}); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("foreign job: %v", err)
		}
		if n := e.Count(`SELECT count(*) FROM attempt_evidence WHERE kind = 'observation_mismatch'`); n != 1 {
			t.Fatalf("%d mismatch rows", n)
		}
	})

	t.Run("direct updates are refused by the database", func(t *testing.T) {
		for name, sql := range map[string]string{
			"run status":      `UPDATE runs SET status = 'failed' WHERE id = $1`,
			"run pinned":      `UPDATE runs SET inputs = '{"inputs":[],"secrets":[]}' WHERE id = $1`,
			"run definition":  `UPDATE runs SET definition_digest = 'sha256:0000000000000000000000000000000000000000000000000000000000000000' WHERE id = $1`,
			"run cancel":      `UPDATE runs SET cancel_requested_at = now(), cancel_reason = 'cancelled' WHERE id = $1`,
			"run delete":      `DELETE FROM runs WHERE id = $1`,
			"step status":     `UPDATE step_runs SET status = 'pending', finished_at = NULL WHERE run_id = $1`,
			"step spec":       `UPDATE step_runs SET spec = '{}' WHERE run_id = $1`,
			"attempt status":  `UPDATE step_attempts SET status = 'failed', failure_class = 'command' WHERE run_id = $1`,
			"attempt result":  `UPDATE step_attempts SET result_hash = NULL, result = NULL WHERE run_id = $1`,
			"attempt context": `UPDATE step_attempts SET context = '{}' WHERE run_id = $1`,
			"attempt delete":  `DELETE FROM step_attempts WHERE run_id = $1`,
		} {
			// As the owner the guards are the only defense.
			if err := e.OwnerExecErr(sql, string(run.ID)); err == nil {
				t.Errorf("%s: the owner's update succeeded", name)
			}
		}
		// History has no update or delete policy at all: nothing can change.
		for _, sql := range []string{`UPDATE run_history SET reason = 'cancelled' WHERE run_id = $1`, `DELETE FROM run_history WHERE run_id = $1`, `DELETE FROM attempt_evidence WHERE run_id = $1`} {
			if n, err := e.OwnerExecRows(sql, string(run.ID)); err == nil && n != 0 {
				t.Errorf("%s changed %d rows", sql, n)
			}
		}
		// As the runtime role the column grants refuse pinned columns outright.
		for name, sql := range map[string]string{
			"runtime pinned input": `UPDATE runs SET inputs = '{"inputs":[],"secrets":[]}' WHERE id = $1`,
			"runtime definition":   `UPDATE runs SET definition_id = 'pd_other' WHERE id = $1`,
			"runtime context":      `UPDATE step_attempts SET context = '{}' WHERE run_id = $1`,
			"runtime delete":       `DELETE FROM runs WHERE id = $1`,
		} {
			err := e.RuntimeTx(p.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, string(run.ID))
				return err
			})
			if err == nil {
				t.Errorf("%s: the runtime role's update succeeded", name)
			}
		}
		after := e.Get(run)
		if after.Run.Status != execution.RunSucceeded || after.Run.SnapshotDigest != digest || len(after.Run.Snapshot.Inputs) != len(inputs) ||
			after.Attempt(in.AttemptID).ResultHash != ack.ResultHash || after.Run.Version != before.Run.Version {
			t.Fatal("a refused update still changed terminal state")
		}
	})

	t.Run("a rerun is a new run and leaves history and pins untouched", func(t *testing.T) {
		e.Variable(p.Scope, "region", "us")
		rerun, err := e.Admit(p, def, nil)
		if err != nil {
			t.Fatal(err)
		}
		if rerun.ID == run.ID {
			t.Fatal("rerun reused the run")
		}
		orig := e.Get(run)
		if orig.Run.Status != execution.RunSucceeded || orig.Run.SnapshotDigest != digest {
			t.Fatalf("the original run changed: %s %s", orig.Run.Status, orig.Run.SnapshotDigest)
		}
		got := map[domain.RunID]runtimev1.Value{}
		for _, r := range []*execution.Run{orig.Run, e.Get(rerun).Run} {
			in, ok := inputByName(r, "region")
			if !ok {
				t.Fatal("region input not pinned")
			}
			got[r.ID] = in
		}
		if got[run.ID].String() != "eu" || got[rerun.ID].String() != "us" {
			t.Fatalf("pinned region: original %q, rerun %q", got[run.ID].String(), got[rerun.ID].String())
		}
		if orig.Run.SnapshotDigest == e.Get(rerun).Run.SnapshotDigest {
			t.Fatal("different pins share a digest")
		}
	})
	_ = ctx
}

func inputByName(r *execution.Run, name string) (runtimev1.Value, bool) {
	for _, in := range r.Snapshot.Inputs {
		if in.Name == name {
			return in.Value, true
		}
	}
	return runtimev1.Value{}, false
}

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/workspace"
)

var executionTables = []string{"runs", "step_runs", "step_attempts", "attempt_controls", "attempt_evidence", "run_history",
	"execution_quota_scopes", "execution_reservations", "work_cancellation_requests"}

// Test_execution_isolation: tenant/project predicates, composite ownership
// foreign keys, pooled context, scoped authority and hidden foreign references.
func Test_execution_isolation(t *testing.T) {
	ctx := context.Background()
	e := executiontest.New(t)
	e.GrantAll()
	pa := e.NewProject(executiontest.ScopeA)
	pa2 := e.NewProject(executiontest.ScopeA2)
	pb := e.NewProject(executiontest.ScopeB)
	defA := e.Define(pa.Scope, pipelinetest.Doc(job("a", readRun, "timeoutSeconds: 60")))
	defA2 := e.Define(pa2.Scope, pipelinetest.Doc(job("a", readRun, "timeoutSeconds: 60")))
	defB := e.Define(pb.Scope, pipelinetest.Doc(job("a", readRun, "timeoutSeconds: 60")))
	runA := e.MustAdmit(pa, defA, nil)
	inA := e.MustClaim(pa.Scope.TenantID)
	runA2 := e.MustAdmit(pa2, defA2, nil)
	if _, err := e.Svc.Admit(ctx, execution.Caller{PrincipalID: executiontest.OtherEng}, execution.AdmitCommand{Scope: pb.Scope, WorkItemID: pb.Item, DefinitionID: defB.ID,
		ContentRevision: e.ContentRevision(pb), SourceCommit: executiontest.CommitA}); err != nil {
		t.Fatal(err)
	}

	t.Run("scoped reads", func(t *testing.T) {
		for name, scope := range map[string]domain.Scope{"other tenant": executiontest.ScopeB, "other project": executiontest.ScopeA2} {
			if _, err := e.Store.Get(ctx, scope, runA.ID); !errors.Is(err, execution.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := e.Store.Get(ctx, executiontest.ScopeA2, runA2.ID); err != nil {
			t.Fatal(err)
		}
		runs, err := e.Store.List(ctx, executiontest.ScopeA, execution.ListQuery{Limit: 50})
		if err != nil || len(runs) != 1 || runs[0].ID != runA.ID {
			t.Fatalf("list of project A: %v %v", runs, err)
		}
	})

	t.Run("forced row level security, with and without tenant context", func(t *testing.T) {
		for _, table := range executionTables {
			var own, foreign, none int
			e.Scalar(&own, `SELECT 0`) // keep the helper warm
			if err := e.RuntimeTx(executiontest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = $1`, table), string(executiontest.ScopeA.TenantID)).Scan(&foreign); err != nil {
					return err
				}
				return tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&own)
			}); err != nil {
				t.Fatal(err)
			}
			if err := e.Runtime.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&none); err != nil {
				t.Fatal(err)
			}
			if foreign != 0 || none != 0 {
				t.Errorf("%s: tenant B sees %d of A's rows, no context sees %d", table, foreign, none)
			}
		}
		// A write for another tenant is refused by the policy.
		err := e.RuntimeTx(executiontest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO execution_quota_scopes (tenant_id, scope_key) VALUES ($1, 'tenant')`, string(executiontest.ScopeA.TenantID))
			return err
		})
		if err == nil {
			t.Error("a cross-tenant insert succeeded")
		}
	})

	t.Run("composite ownership foreign keys", func(t *testing.T) {
		// A run cannot name a work item of another project (even in its tenant),
		// nor pin another project's definition.
		for name, sql := range map[string]string{
			"work item of another project": fmt.Sprintf(`INSERT INTO runs (tenant_id, project_id, id, purpose, work_item_id, workspace_id, definition_id, definition_digest, content_revision,
				specification, source_commit, inputs, snapshot_digest, source_retention, retained_ref_id, requested_by_type, requested_by_id, status, status_reason, created_at, updated_at)
				SELECT '%[1]s', '%[2]s', 'run_fk1', 'work', w.id, ws.id, d.id, d.digest, 1, '{}', '%[5]s', '{"inputs":[],"secrets":[]}', 'sha256:%[6]s', 'pending', 'pin_x',
				'user', 'u', 'queued', 'created', now(), now()
				FROM work_items w, workspaces ws, pipeline_definitions d WHERE w.id = '%[3]s' AND ws.work_item_id = '%[3]s' AND d.id = '%[4]s'`,
				executiontest.ScopeA.TenantID, executiontest.ScopeA.ProjectID, pa2.Item, defA.ID, executiontest.CommitA, zeros(64)),
		} {
			if err := e.OwnerExecErr(sql); err == nil {
				t.Errorf("%s: the foreign key accepted a foreign reference", name)
			}
		}
		// An attempt bookkeeping row cannot reference an attempt of another project.
		err := e.OwnerExecErr(`INSERT INTO attempt_controls (tenant_id, project_id, attempt_id, run_id) VALUES ($1, $2, $3, $4)`,
			string(executiontest.ScopeA.TenantID), string(executiontest.ScopeA2.ProjectID), string(inA.AttemptID), string(runA.ID))
		if err == nil {
			t.Error("a control row crossed projects")
		}
		err = e.OwnerExecErr(`INSERT INTO execution_reservations (tenant_id, project_id, attempt_id, run_id, held_at) VALUES ($1, $2, $3, $4, now())`,
			string(executiontest.ScopeA.TenantID), string(executiontest.ScopeA2.ProjectID), string(inA.AttemptID), string(runA.ID))
		if err == nil {
			t.Error("a reservation crossed projects")
		}
	})

	t.Run("pooled tenant context never leaks", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 40; i++ {
			scope := []domain.Scope{executiontest.ScopeA, executiontest.ScopeB}[i%2]
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := e.RuntimeTx(scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
					var ctxTenant *string
					var n, other int
					if err := tx.QueryRow(ctx, `SELECT kodama_tenant_id()`).Scan(&ctxTenant); err != nil {
						return err
					}
					if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&n); err != nil {
						return err
					}
					if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE tenant_id <> $1`, string(scope.TenantID)).Scan(&other); err != nil {
						return err
					}
					if ctxTenant == nil || *ctxTenant != string(scope.TenantID) || other != 0 || n == 0 {
						return fmt.Errorf("context %v, %d rows (%d foreign)", ctxTenant, n, other)
					}
					return nil
				})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		var left *string
		if err := e.Runtime.QueryRow(ctx, `SELECT kodama_tenant_id()`).Scan(&left); err != nil || left != nil {
			t.Fatalf("a pooled connection kept a tenant context: %v %v", left, err)
		}
	})

	t.Run("foreign references are hidden", func(t *testing.T) {
		other := execution.Caller{PrincipalID: executiontest.OtherEng}
		if _, err := e.Svc.Cancel(ctx, other, executiontest.ScopeB, runA.ID); !errors.Is(err, execution.ErrNotFound) {
			t.Errorf("cancel across tenants: %v", err)
		}
		if _, err := e.Svc.Cancel(ctx, engineer, executiontest.ScopeA2, runA.ID); !errors.Is(err, execution.ErrNotFound) {
			t.Errorf("cancel across projects: %v", err)
		}
		if err := e.Svc.ObserveAttempt(ctx, executiontest.ScopeB, inA.AttemptID, execution.Observation{Kind: execution.ObservedDispatched}); !errors.Is(err, execution.ErrNotFound) {
			t.Errorf("observe: %v", err)
		}
		res := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("x"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		h, _ := runtimev1.ResultHash(res)
		if _, err := e.Svc.CommitResult(ctx, execution.ResultCommit{Scope: executiontest.ScopeB, AttemptID: inA.AttemptID, Request: runtimev1.ResultRequest{ResultHash: h, Result: res}}); !errors.Is(err, execution.ErrNotFound) {
			t.Errorf("commit: %v", err)
		}
		// A missing attempt answers exactly like a foreign one.
		if err := e.Svc.ObserveAttempt(ctx, executiontest.ScopeA, "att_missing", execution.Observation{Kind: execution.ObservedDispatched}); !errors.Is(err, execution.ErrNotFound) {
			t.Errorf("missing attempt: %v", err)
		}
		authority, _ := execpg.NewAttemptAuthority(e.Store)
		for name, scope := range map[string]domain.Scope{"tenant": executiontest.ScopeB, "project": executiontest.ScopeA2} {
			if _, err := authority.Authority(ctx, scope, inA.AttemptID); !errors.Is(err, workspace.ErrNotFound) {
				t.Errorf("attempt authority across %s: %v", name, err)
			}
		}
		// Nothing of tenant A changed.
		if got := e.Get(runA).Attempt(inA.AttemptID); got.Status != execution.AttemptRunning || got.Version != 1 {
			t.Fatalf("a foreign call changed the attempt: %s v%d", got.Status, got.Version)
		}
	})

	t.Run("scans and capacity stay inside the tenant", func(t *testing.T) {
		// Tenant B's claim finds its own run only; tenant A's runnable work is
		// never claimed on its behalf, nor counted against its capacity.
		in, err := e.Claim(executiontest.ScopeB.TenantID)
		if err != nil || in.Scope != executiontest.ScopeB {
			t.Fatalf("tenant B claim: %+v %v", in, err)
		}
		if _, err := e.Claim(executiontest.ScopeB.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("tenant B's second claim: %v", err)
		}
		if n := e.CountTenant(executiontest.ScopeB.TenantID, `SELECT count(*) FROM execution_reservations`); n != 1 {
			t.Fatalf("tenant B sees %d reservations, want its own 1", n)
		}
		if n := e.Count(`SELECT count(*) FROM step_attempts WHERE tenant_id = $1`, string(executiontest.ScopeA.TenantID)); n != 1 {
			t.Fatalf("tenant A has %d attempts", n)
		}
	})
}

func zeros(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

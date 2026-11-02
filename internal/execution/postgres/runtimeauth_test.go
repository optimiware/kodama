package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	repositorypg "github.com/optimiware/kodama/internal/repository/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
)

func fakePeers(e *executiontest.Env) execpg.Peers {
	return execpg.Peers{Authority: adapters.BindAuthority, Workspaces: e.Workspaces, Secrets: e.Secrets}
}

// Test_runtime_store_construction: SD-04, IT-03. A store that serves runtime
// credentials cannot be built without its grant revoker, so omission can never
// silently leave the grants of cancelled attempts alive; the component store
// without the runtime schema stays constructible.
func Test_runtime_store_construction(t *testing.T) {
	e := executiontest.New(t)
	revoker := func(context.Context, pgx.Tx, domain.Scope, domain.AttemptID, time.Time) (int, error) { return 0, nil }
	if _, err := execpg.NewRuntimeStore(e.Runtime, e.Events, fakePeers(e), nil, execpg.Options{}); !errors.Is(err, execution.ErrNotConfigured) {
		t.Fatalf("a runtime store without a revoker: %v", err)
	}
	if _, err := execpg.NewRuntimeStore(nil, e.Events, fakePeers(e), revoker, execpg.Options{}); !errors.Is(err, execution.ErrNotConfigured) {
		t.Fatalf("a runtime store without a database: %v", err)
	}
	peers := fakePeers(e)
	peers.Secrets = nil
	if _, err := execpg.NewRuntimeStore(e.Runtime, e.Events, peers, revoker, execpg.Options{}); !errors.Is(err, execution.ErrNotConfigured) {
		t.Fatalf("a runtime store without a peer: %v", err)
	}
	if _, err := execpg.NewRuntimeStore(e.Runtime, e.Events, fakePeers(e), revoker, execpg.Options{}); err != nil {
		t.Fatal(err)
	}
	// The component constructor keeps working without the runtime schema.
	if _, err := execpg.NewStore(e.Runtime, e.Events, fakePeers(e), execpg.Options{}); err != nil {
		t.Fatal(err)
	}
}

// grantTx records what the bridge-facing revoker receives.
type recordedCall struct {
	scope   domain.Scope
	attempt domain.AttemptID
	xid     int64
}

// Test_runtime_store_revocation: SD-04, SD-10, IT-03, IT-06, PX-07. The
// revoker receives the store's own transaction and the scoped identifiers of
// each attempt whose authority ended without an accepted result, in attempt
// order; the real runtimeauth binding revokes in that transaction so a failure
// of any later step rolls the revocation back; an accepted result and a
// repeated cancellation revoke nothing. Real PostgreSQL carrying the runtime
// grant schema.
func Test_runtime_store_revocation(t *testing.T) {
	ctx := context.Background()
	e := executiontest.New(t)
	e.GrantAll()
	manifest, err := storage.NewManifest(storage.BaseContribution(), execpg.RunResourceMigrations(), repositorypg.Migrations(), workspacepg.Migrations(), execpg.RunSourceMigrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, e.Migration, manifest, storage.MigrateOptions{RuntimeRole: e.Base.DB.RuntimeRole}); err != nil {
		t.Fatal(err)
	}

	var calls []recordedCall
	appender := &failingAppender{inner: e.Events}
	revoker := func(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
		var xid int64
		if err := tx.QueryRow(ctx, `SELECT txid_current()`).Scan(&xid); err != nil {
			return 0, err
		}
		calls = append(calls, recordedCall{scope, attempt, xid})
		return adapters.RevokeAttemptGrants(ctx, tx, scope, attempt, now)
	}
	rt, err := execpg.NewRuntimeStore(e.Runtime, appender, fakePeers(e), revoker, execpg.Options{Clock: e.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	svc := e.NewService(executiontest.Options{Store: rt})

	grantFor := func(in *execution.AttemptSpec) {
		t.Helper()
		e.OwnerExec(`INSERT INTO runtime_grants (tenant_id, project_id, run_id, attempt_id, job_namespace, job_name, credential_digest, signing_generation,
			actions, authority_revision, context_digest, context_doc, created_at, expires_at)
			VALUES ($1::text, $2::text, $3::text, $4::text, 'ns-t062', 'job-t062', sha256(convert_to($4::text, 'UTF8')), 1, ARRAY['result.submit'], 1, sha256(convert_to($3::text, 'UTF8')), '\x7b7d', now(), now() + interval '1 hour')`,
			string(in.Scope.TenantID), string(in.Scope.ProjectID), string(in.RunID), string(in.AttemptID))
	}
	revoked := func(in *execution.AttemptSpec) bool {
		t.Helper()
		return e.Count(`SELECT count(*) FROM runtime_grants WHERE attempt_id = $1 AND revoked_at IS NOT NULL`, string(in.AttemptID)) == 1
	}
	start := func(p executiontest.Project, step string) (*execution.Run, *execution.AttemptSpec) {
		t.Helper()
		def := e.Define(p.Scope, pipelinetest.Doc(job(step, readRun, "timeoutSeconds: 60")))
		run := e.MustAdmit(p, def, nil)
		in, err := svc.ClaimRunnable(ctx, p.Scope.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		e.Start(in)
		grantFor(in)
		return run, in
	}
	cancel := func(run *execution.Run) error {
		_, err := svc.Cancel(ctx, execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t062"}, run.Scope, run.ID)
		return err
	}

	t.Run("the revoker joins the store's transaction with scoped identifiers, and its effect rolls back with a later failure", func(t *testing.T) {
		p := e.NewProject(executiontest.ScopeA)
		run, in := start(p, "edit")
		appender.fail.Store(eventsv1.TypeRunStateChanged)
		if err := cancel(run); !errors.Is(err, errInjected) {
			t.Fatalf("%v", err)
		}
		appender.fail.Store("")
		if revoked(in) || e.Get(run).Control(in.AttemptID).RevokedAt != nil {
			t.Fatal("a failed cancellation left the revocation behind")
		}
		var rolledBack []recordedCall
		rolledBack = append(rolledBack, calls...)
		calls = nil
		if err := cancel(run); err != nil {
			t.Fatal(err)
		}
		if !revoked(in) || e.Get(run).Control(in.AttemptID).RevokedAt == nil {
			t.Fatal("the committed cancellation did not revoke the grant with the execution authority")
		}
		if len(rolledBack) != 1 || len(calls) != 1 {
			t.Fatalf("%d calls in the failed and %d in the committed transaction", len(rolledBack), len(calls))
		}
		got := calls[0]
		if got.scope != p.Scope || got.attempt != in.AttemptID || got.xid <= 0 || got.xid == rolledBack[0].xid {
			t.Fatalf("%+v (failed transaction %+v)", got, rolledBack[0])
		}
		// A repeated cancellation revokes nothing again.
		calls = nil
		if err := cancel(run); err != nil || len(calls) != 0 {
			t.Fatalf("%v %d calls", err, len(calls))
		}
	})

	t.Run("an accepted result keeps its grant for receipt replay", func(t *testing.T) {
		p := e.NewProject(executiontest.ScopeA)
		_, in := start(p, "build")
		calls = nil
		code := int64(0)
		result := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &code, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		hash, err := runtimev1.ResultHash(result)
		if err != nil {
			t.Fatal(err)
		}
		ack, err := svc.CommitResult(ctx, execution.ResultCommit{Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: hash, Result: result}})
		if err != nil || ack.AttemptStatus != execution.AttemptSucceeded {
			t.Fatalf("%+v %v", ack, err)
		}
		if revoked(in) || len(calls) != 0 {
			t.Fatalf("an accepted result revoked its grant (%d calls)", len(calls))
		}
	})

	t.Run("a deadline failure without a result revokes the grant, in attempt order within its transaction", func(t *testing.T) {
		p := e.NewProject(executiontest.ScopeA)
		_, in := start(p, "slow")
		calls = nil
		e.Clock.Advance(2 * time.Hour)
		if _, err := svc.ReconcileTenant(ctx, p.Scope.TenantID); err != nil {
			t.Fatal(err)
		}
		if !revoked(in) {
			t.Fatal("a timed-out attempt kept a usable grant")
		}
		if len(calls) == 0 || calls[0].attempt != in.AttemptID {
			t.Fatalf("%+v", calls)
		}
	})
}

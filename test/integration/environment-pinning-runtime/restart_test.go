package environmentpinningruntime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

// Test_environment_pin_restart_scope — environment_pin_restart_scope
// (DQ-15/PX-01, tenancy): a restart between preparation and commit, between
// admission and dispatch, and after dispatch reconstructs everything from the
// persisted pin; the pin, its retained snapshot, its credential version pin and
// its dispatch are visible only inside the owning tenant and project, as the
// non-owner application role sees them.
func Test_environment_pin_restart_scope(t *testing.T) {
	ctx := context.Background()

	t.Run("a_plan_prepared_before_a_restart_commits_once_after_it", func(t *testing.T) {
		w := newWorld(t, stack.Options{})
		plan := w.prepare(t)
		w.Restart() // the process that prepared it is gone
		run, created, err := w.ExecStore.Admit(ctx, plan)
		if err != nil || !created {
			t.Fatalf("admit after restart: %v %v", created, err)
		}
		// A lost acknowledgment: the same plan again, from yet another process.
		w.Restart()
		again, created, err := w.ExecStore.Admit(ctx, plan)
		if err != nil || created || again.ID != run.ID {
			t.Fatalf("the repeated admission: created=%v err=%v", created, err)
		}
		if n := w.Count(`SELECT count(*) FROM runs WHERE purpose = 'work'`); n != 1 {
			t.Fatalf("%d runs committed", n)
		}
		if n := w.Count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1 AND released_at IS NULL`, string(run.ID)); n != 1 {
			t.Fatalf("%d credential pins for the run", n)
		}
	})

	t.Run("dispatch_after_a_restart_uses_the_persisted_pin_only", func(t *testing.T) {
		w := newWorld(t, stack.Options{})
		run := w.admit(t)
		admitted := pinOf(t, run)

		// Everything about the environment changes while no process is running.
		ns := "t067-after-restart"
		if _, err := w.Svc.UpdateEnvironment(ctx, stack.Caller(), scopeA, w.env.ID(), w.env.Version(), delivery.EnvironmentEdit{Namespace: &ns}); err != nil {
			t.Fatal(err)
		}
		w.X.RotateSecret(scopeA, w.secret.ID(), kubeconfigV2)
		w.Restart()

		c := w.claimContext(t, run)
		if c.Environment == nil || *c.Environment != admitted || len(c.Secrets) != 1 || c.Secrets[0].Version != 1 {
			t.Fatalf("dispatch after the restart did not use the persisted pin: %+v %+v", c.Environment, c.Secrets)
		}
		// The persisted context is what a third process reads back.
		w.Restart()
		agg, err := w.ExecStore.Get(ctx, scopeA, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(agg.Attempts) != 1 {
			t.Fatalf("%d attempts", len(agg.Attempts))
		}
		stored, err := runtimev1.DecodeRequest[runtimev1.Context](agg.Attempts[0].Context)
		if err != nil || stored.Environment == nil || *stored.Environment != admitted {
			t.Fatalf("the stored context differs: %v", err)
		}
		// A new admission after the restart sees the edited environment.
		if fresh := pinOf(t, w.admit(t)); fresh.ConfigDigest == admitted.ConfigDigest {
			t.Fatal("a later admission did not see the edit")
		}
	})

	t.Run("the_pin_is_visible_only_to_its_tenant_and_project", func(t *testing.T) {
		w := newWorld(t, stack.Options{})
		run := w.admit(t)
		pin := pinOf(t, run)
		w.Restart()

		resolver, err := execadapters.NewDeliveryEnvironments(w.Svc)
		if err != nil {
			t.Fatal(err)
		}
		engineer := execution.Actor{ID: executiontest.Engineer, Type: execution.ActorUser}
		for name, scope := range map[string]domain.Scope{"another tenant": scopeB, "another project": scopeA2} {
			if _, err := w.ExecStore.Get(ctx, scope, run.ID); !errors.Is(err, execution.ErrNotFound) {
				t.Errorf("%s read the pinned run: %v", name, err)
			}
			if _, err := w.Svc.RetainedSnapshot(ctx, scope, pin.ID, pin.ConfigDigest); !errors.Is(err, delivery.ErrNotFound) {
				t.Errorf("%s read the retained snapshot: %v", name, err)
			}
			// Naming the environment by its identifier does not adopt it.
			if _, err := resolver.ResolveEnvironment(ctx, execution.EnvironmentRequest{Scope: scope, Actor: engineer, EnvironmentID: pin.ID}); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Errorf("%s resolved the environment: %v", name, err)
			}
		}

		// The application role under another tenant's row-level security sees none
		// of the pin's rows; under the owning tenant it sees them.
		count := func(tenant domain.TenantID, sql string, arg string) int {
			var n int
			err := w.X.RuntimeTx(tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, sql, arg).Scan(&n)
			})
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			return n
		}
		for sql, arg := range map[string]string{
			`SELECT count(*) FROM environment_snapshots WHERE environment_id = $1`: string(pin.ID),
			`SELECT count(*) FROM runs WHERE id = $1`:                              string(run.ID),
			`SELECT count(*) FROM secret_pins WHERE consumer_id = $1`:              string(run.ID),
		} {
			if n := count(scopeB.TenantID, sql, arg); n != 0 {
				t.Errorf("another tenant's session saw %d rows of %q", n, sql)
			}
			if n := count(scopeA.TenantID, sql, arg); n == 0 {
				t.Errorf("the owning tenant's session saw nothing of %q", sql)
			}
		}

		// A forged cross-project reference is refused by the schema itself: a run
		// of project A2 cannot be written naming project A's snapshot, and the
		// application role cannot insert one.
		err = w.X.RuntimeTx(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO environment_snapshots (tenant_id, project_id, environment_id, digest, kind, target, namespace, config, preview_ttl_seconds, kubeconfig, secrets, created_at)
SELECT tenant_id, $2, environment_id, digest, kind, target, namespace, config, preview_ttl_seconds, kubeconfig, secrets, created_at FROM environment_snapshots WHERE environment_id = $1`,
				string(pin.ID), string(scopeA2.ProjectID))
			return err
		})
		if err == nil {
			t.Error("a snapshot of one project was copied into another project's environment")
		}
		t.Logf("cross-project snapshot insert: %v", err)
	})
}

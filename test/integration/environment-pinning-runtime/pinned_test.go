package environmentpinningruntime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// Test_environment_pinned_at_admission — environment_pinned_at_admission
// (DQ-15/PX-01): admitting a run that declares an environment resolves it under
// the actor's current authority, retains the immutable configuration snapshot
// and registers the exact credential version pin in the same transaction as the
// run; the persisted snapshot, the run's input snapshot and the dispatched
// runtime context all name that one pin, and no plaintext is stored anywhere.
func Test_environment_pinned_at_admission(t *testing.T) {
	ctx := context.Background()

	t.Run("the_pin_credential_version_and_run_commit_together", func(t *testing.T) {
		w := newWorld(t, stackOptions())
		if w.snapshots() != 0 {
			t.Fatalf("a snapshot was retained before any admission")
		}
		run := w.admit(t)
		pin := pinOf(t, run)
		if pin.ID != w.env.ID() {
			t.Fatalf("pin names %s, want %s", pin.ID, w.env.ID())
		}

		// The retained snapshot is the pin: same digest, the environment's
		// namespace, and the credential bound at its exact current version.
		snap, err := w.Svc.RetainedSnapshot(ctx, scopeA, pin.ID, pin.ConfigDigest)
		if err != nil {
			t.Fatalf("the pinned snapshot is not retained: %v", err)
		}
		if snap.Pin() != pin || snap.Namespace != namespace || snap.Validate() != nil {
			t.Fatalf("the retained snapshot is not the admitted pin: %+v", snap)
		}
		if len(snap.Secrets) != 1 || snap.Secrets[0].Version != 1 || snap.Secrets[0].Name != "KUBECONFIG" || snap.Secrets[0].Delivery != runtimev1.DeliveryFile {
			t.Fatalf("snapshot credential bindings: %+v", snap.Secrets)
		}
		if w.snapshots() != 1 {
			t.Fatalf("%d snapshots retained, want 1", w.snapshots())
		}

		// The credential version pin is registered for the run (the same
		// transaction: the run row, its steps and the pin exist together).
		if n := w.runPins(run.ID, 1); n != 1 {
			t.Fatalf("the run's credential pin is missing: %d", n)
		}
		if n := w.Count(`SELECT count(*) FROM runs WHERE tenant_id = $1 AND id = $2`, string(scopeA.TenantID), string(run.ID)); n != 1 {
			t.Fatalf("run rows: %d", n)
		}

		// The run's own persisted snapshot carries the pin and the bindings.
		agg := w.X.Get(run)
		got := agg.Run.Snapshot
		if len(got.Environments) != 1 || got.Environments[0].Pin != pin || len(got.Secrets) != 1 || got.Secrets[0].Binding != snap.Secrets[0] {
			t.Fatalf("the persisted run snapshot differs from the admitted pin: %+v", got)
		}

		// Dispatch sends the context built from the persisted pin only.
		c := w.claimContext(t, run)
		if c.Environment == nil || *c.Environment != pin || len(c.Secrets) != 1 || c.Secrets[0] != snap.Secrets[0] {
			t.Fatalf("the runtime context does not carry the admitted pin: %+v / %+v", c.Environment, c.Secrets)
		}
		// No plaintext in any persisted run or snapshot column.
		for _, table := range []string{"runs", "step_attempts", "environment_snapshots"} {
			if n := w.Count(`SELECT count(*) FROM ` + table + ` t WHERE t::text LIKE '%SENTINEL%'`); n != 0 {
				t.Errorf("credential plaintext reached %s", table)
			}
		}
	})

	t.Run("admission_is_bound_to_the_actor_and_scope_it_resolved_for", func(t *testing.T) {
		w := newWorld(t, stackOptions())
		actor := pipeline.Actor{ID: executiontest.Engineer, Type: pipeline.ActorUser}

		// An environment of another project or tenant is not referenceable, even
		// by its identifier, and its retained snapshots are not readable.
		if _, err := w.X.Validator.ValidateYAML(ctx, scopeB, actor, []byte(docWith(string(w.env.ID())))); err == nil {
			t.Fatal("a definition of another tenant referenced this project's environment")
		}
		if _, err := w.X.Validator.ValidateYAML(ctx, scopeA2, actor, []byte(docWith(string(w.env.ID())))); err == nil {
			t.Fatal("a definition of another project referenced this project's environment")
		}
		if _, err := w.Svc.RetainedSnapshot(ctx, scopeB, w.env.ID(), runtimev1.Digest("sha256:"+strings.Repeat("a", 64))); !errors.Is(err, delivery.ErrNotFound) {
			t.Fatalf("a foreign scope read a snapshot: %v", err)
		}
		// An unknown environment is refused when the definition is validated.
		if _, err := w.X.Validator.ValidateYAML(ctx, scopeA, actor, []byte(docWith("env_t067_missing"))); err == nil {
			t.Fatal("a definition referencing a missing environment validated")
		}

		// An actor without authority cannot admit; nothing is persisted and no
		// environment is resolved for it.
		before := w.counts()
		_, err := w.Exec.Admit(ctx, execution.Caller{PrincipalID: executiontest.Stranger, RequestID: "req_t067"}, w.command())
		if err == nil {
			t.Fatal("an actor without authority admitted a run")
		}
		w.requireUnchanged(t, before, "a refused admission")
		if w.snapshots() != 0 {
			t.Fatalf("a refused admission retained a snapshot: %d", w.snapshots())
		}
	})

	t.Run("a_run_without_an_environment_pins_nothing", func(t *testing.T) {
		w := newWorld(t, stackOptions())
		plain := w.X.Define(scopeA, pipelinetest.Doc(pipelinetest.Container("plain")))
		cmd := w.command()
		cmd.DefinitionID = plain.ID
		run, err := w.Exec.Admit(ctx, caller(), cmd)
		if err != nil {
			t.Fatalf("a no-environment admission failed: %v", err)
		}
		if len(run.Snapshot.Environments) != 0 || w.snapshots() != 0 || w.runPins(run.ID, 1) != 0 {
			t.Fatal("an ordinary admission acquired an environment pin")
		}
	})

	t.Run("the_snapshot_row_is_immutable_to_the_application_role", func(t *testing.T) {
		w := newWorld(t, stackOptions())
		run := w.admit(t)
		pin := pinOf(t, run)
		mutate := func(sql string) error {
			return w.X.RuntimeTx(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, string(scopeA.TenantID), string(pin.ID), string(pin.ConfigDigest))
				return err
			})
		}
		if err := mutate(`UPDATE environment_snapshots SET namespace = 'kube-system' WHERE tenant_id = $1 AND environment_id = $2 AND digest = $3`); err == nil {
			t.Fatal("the runtime role rewrote a retained snapshot")
		}
		if err := mutate(`DELETE FROM environment_snapshots WHERE tenant_id = $1 AND environment_id = $2 AND digest = $3`); err == nil {
			t.Fatal("the runtime role deleted a retained snapshot")
		}
		var ns string
		w.Scalar(&ns, `SELECT namespace FROM environment_snapshots WHERE tenant_id = $1 AND environment_id = $2 AND digest = $3`, string(scopeA.TenantID), string(pin.ID), string(pin.ConfigDigest))
		if ns != namespace {
			t.Fatalf("the snapshot namespace is %q", ns)
		}
	})
}

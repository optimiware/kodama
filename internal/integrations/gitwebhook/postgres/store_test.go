package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/execution/executiontest"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	gwpg "github.com/optimiware/kodama/internal/integrations/gitwebhook/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// TestPostgresStoreContract runs the Store contract the memory store runs
// (Test_git_trigger_store_atomic_claim) against PostgreSQL.
func TestPostgresStoreContract(t *testing.T) {
	storeContract(t, func(t testing.TB) storeSubject { return newEnv(t).storeSubject() })
}

// TestPostgresStoreIsolation proves what the memory store cannot: another
// tenant's trigger, delivery and audit are never visible or writable, through
// the composite keys and row level security.
func TestPostgresStoreIsolation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, b := executiontest.ScopeA, executiontest.ScopeB
	tr := e.trigger(a, "")
	if err := e.store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.ClaimDelivery(ctx, claimOf(tr, "dlv-1", "d")); err != nil {
		t.Fatal(err)
	}
	key := gw.DeliveryKey{TriggerID: tr.ID, DeliveryID: "dlv-1"}
	foreignDefinition := e.def(a).ID
	e.def(b) // registered up front: registration writes its own audit record
	countOwn := func(table string) int { return e.count(`SELECT count(*) FROM ` + table) }

	t.Run("a tenant context sees only its own rows", func(t *testing.T) {
		for _, table := range []string{"git_triggers", "git_trigger_deliveries", "git_trigger_admissions"} {
			var n int
			if err := e.inTenant(b.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
			}); err != nil || n != 0 {
				t.Errorf("tenant B sees %d rows of %s (%v)", n, table, err)
			}
		}
		var n int
		if err := e.inTenant(b.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM git_triggers WHERE id = $1`, string(tr.ID)).Scan(&n)
		}); err != nil || n != 0 {
			t.Errorf("tenant B reads tenant A's trigger by address: %d %v", n, err)
		}
		if err := e.inTenant(a.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM git_triggers WHERE id = $1`, string(tr.ID)).Scan(&n)
		}); err != nil || n != 1 {
			t.Errorf("tenant A reads its trigger: %d %v", n, err)
		}
		// No tenant context at all sees nothing, whatever the predicate.
		if err := e.x.Runtime.QueryRow(ctx, `SELECT count(*) FROM git_triggers`).Scan(&n); err != nil || n != 0 {
			t.Errorf("a connection without a tenant context sees %d triggers (%v)", n, err)
		}
	})

	t.Run("another tenant cannot claim, finish, update or audit against the trigger", func(t *testing.T) {
		beforeDeliveries, beforeAudit := countOwn("git_trigger_deliveries"), countOwn("audit")
		foreignClaim := claimOf(tr, "dlv-foreign", "d")
		foreignClaim.Scope = b
		if _, created, err := e.store.ClaimDelivery(ctx, foreignClaim); !errors.Is(err, gw.ErrNotFound) || created {
			t.Fatalf("a claim in another tenant's scope = %v created=%v, want ErrNotFound", created, err)
		}
		// The same delivery under the other tenant's context cannot even see the claim.
		foreignClaim.Key.DeliveryID = "dlv-1"
		if _, created, err := e.store.ClaimDelivery(ctx, foreignClaim); !errors.Is(err, gw.ErrNotFound) || created {
			t.Fatalf("a replayed claim in another scope = %v created=%v", created, err)
		}
		foreignAudit := audit("git_hook.ignored", tr, "dlv-1")
		foreignAudit.Scope = b
		if _, err := e.store.FinishDelivery(ctx, key, gw.Finish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonBranchFiltered}, foreignAudit); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("a finish carrying another tenant's audit = %v", err)
		}
		forged := tr
		forged.Scope = b
		if _, err := e.store.UpdateTrigger(ctx, forged, 1, audit("git_trigger.disabled", forged, "")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("another tenant's update = %v", err)
		}
		// A trigger cannot be installed against another tenant's definition.
		cross := e.trigger(b, "")
		cross.DefinitionID = foreignDefinition
		if err := e.store.InsertTrigger(ctx, cross, audit("git_trigger.installed", cross, "")); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("a foreign definition = %v, want ErrNotFound", err)
		}
		// An audit record cannot be written for a trigger's tenant under another's context.
		if err := e.store.RecordAudit(ctx, gw.AuditRecord{Action: "x", Scope: executiontest.ScopeA, TriggerID: "not valid!", At: clock}); err == nil {
			t.Fatal("an audit record for an invalid resource identifier was stored")
		}
		if d, au := countOwn("git_trigger_deliveries"), countOwn("audit"); d != beforeDeliveries || au != beforeAudit {
			t.Fatalf("a refused call changed state: deliveries %d->%d, audit %d->%d", beforeDeliveries, d, beforeAudit, au)
		}
		rec, _, err := e.store.ClaimDelivery(ctx, claimOf(tr, "dlv-1", "d"))
		if err != nil || rec.Done || rec.Scope != a {
			t.Fatalf("the owner's delivery was disturbed: %+v %v", rec, err)
		}
	})

	t.Run("rows cannot be written for another tenant under a tenant context", func(t *testing.T) {
		err := e.inTenant(b.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO git_trigger_deliveries (tenant_id, project_id, trigger_id, delivery_id, body_digest, ref, commit_id, received_at)
VALUES ($1, $2, $3, 'dlv-rls', 'd', 'refs/heads/main', $4, now())`, string(a.TenantID), string(a.ProjectID), string(tr.ID), executiontest.CommitA)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Fatalf("an insert for another tenant = %v, want a row level security refusal", err)
		}
		var updated int64
		if err := e.inTenant(b.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE git_triggers SET enabled = false, version = version + 1 WHERE id = $1`, string(tr.ID))
			updated = tag.RowsAffected()
			return err
		}); err != nil || updated != 0 {
			t.Fatalf("another tenant updated %d trigger rows (%v)", updated, err)
		}
	})

	t.Run("the locator reveals only ownership", func(t *testing.T) {
		var tenant, project string
		if err := e.x.Runtime.QueryRow(ctx, `SELECT tenant_id, project_id FROM kodama_git_trigger_locate($1)`, string(tr.ID)).Scan(&tenant, &project); err != nil ||
			tenant != string(a.TenantID) || project != string(a.ProjectID) {
			t.Fatalf("locate = %q %q %v", tenant, project, err)
		}
		if err := e.x.Runtime.QueryRow(ctx, `SELECT tenant_id FROM kodama_git_trigger_locate('trg-absent')`).Scan(&tenant); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("an unknown address = %v", err)
		}
	})
}

// TestPostgresStoreDurability proves claims and outcomes survive lost commit
// acknowledgements and restarts, and that the schema itself keeps outcomes
// write-once.
func TestPostgresStoreDurability(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a := executiontest.ScopeA
	tr := e.trigger(a, "")
	if err := e.store.InsertTrigger(ctx, tr, audit("git_trigger.installed", tr, "")); err != nil {
		t.Fatal(err)
	}
	key := func(id string) gw.DeliveryKey { return gw.DeliveryKey{TriggerID: tr.ID, DeliveryID: id} }

	t.Run("a claim whose acknowledgement was lost is the claim", func(t *testing.T) {
		lost := &lostCommit{inner: e.x.Runtime}
		store, err := gwpg.NewStore(lost, gwpg.Options{})
		if err != nil {
			t.Fatal(err)
		}
		lost.armed.Store(true)
		if _, _, err := store.ClaimDelivery(ctx, claimOf(tr, "dlv-lost", "d")); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("claim = %v, want ErrUnavailable", err)
		}
		rec, created, err := e.store.ClaimDelivery(ctx, claimOf(tr, "dlv-lost", "d"))
		if err != nil || created || rec.Done {
			t.Fatalf("the redelivery finds the landed claim: %+v created=%v %v", rec, created, err)
		}
	})

	t.Run("a finish whose acknowledgement was lost replays as the outcome", func(t *testing.T) {
		if _, _, err := e.store.ClaimDelivery(ctx, claimOf(tr, "dlv-fin", "d")); err != nil {
			t.Fatal(err)
		}
		lost := &lostCommit{inner: e.x.Runtime}
		store, _ := gwpg.NewStore(lost, gwpg.Options{})
		lost.armed.Store(true)
		if _, err := store.FinishDelivery(ctx, key("dlv-fin"), gw.Finish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonNotBranch}, audit("git_hook.ignored", tr, "dlv-fin")); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("finish = %v, want ErrUnavailable", err)
		}
		before := e.audits(a, tr.ID)
		got, err := e.store.FinishDelivery(ctx, key("dlv-fin"), gw.Finish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonNotBranch}, audit("git_hook.ignored", tr, "dlv-fin"))
		if err != nil || !got.Done || got.Outcome != gw.OutcomeIgnored {
			t.Fatalf("retry = %+v %v", got, err)
		}
		if e.audits(a, tr.ID) != before {
			t.Fatal("the retried finish wrote its audit a second time")
		}
	})

	t.Run("the audit is written with the change it describes", func(t *testing.T) {
		if _, _, err := e.store.ClaimDelivery(ctx, claimOf(tr, "dlv-atomic", "d")); err != nil {
			t.Fatal(err)
		}
		before := e.audits(a, tr.ID)
		bad := audit("git_hook.ignored", tr, "dlv-atomic")
		bad.Action = strings.Repeat("x", 200) // the audit table refuses it: the finish must roll back with it
		if _, err := e.store.FinishDelivery(ctx, key("dlv-atomic"), gw.Finish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonNotBranch}, bad); err == nil {
			t.Fatal("a finish with an unwritable audit record succeeded")
		}
		rec, _, _ := e.store.ClaimDelivery(ctx, claimOf(tr, "dlv-atomic", "d"))
		if rec.Done || e.audits(a, tr.ID) != before {
			t.Fatalf("the finish outlived its audit: %+v audits %d->%d", rec, before, e.audits(a, tr.ID))
		}
	})

	t.Run("the schema keeps a finished outcome and the version sequence", func(t *testing.T) {
		if _, _, err := e.store.ClaimDelivery(ctx, claimOf(tr, "dlv-final", "d")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.FinishDelivery(ctx, key("dlv-final"), gw.Finish{Outcome: gw.OutcomeIgnored, Reason: gw.ReasonNotBranch}, audit("git_hook.ignored", tr, "dlv-final")); err != nil {
			t.Fatal(err)
		}
		// As the runtime role, whose column grants allow these updates, the
		// guards themselves refuse them (the owner role has no UPDATE policy).
		exec := func(stmt string, args ...any) error {
			return e.inTenant(a.TenantID, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt, args...); return err })
		}
		trigger := string(tr.ID)
		for name, stmt := range map[string]string{
			"a finished delivery changes outcome": `UPDATE git_trigger_deliveries SET outcome = 'rejected', reason = 'x' WHERE trigger_id = $1 AND delivery_id = 'dlv-final'`,
			"a finished delivery is reopened":     `UPDATE git_trigger_deliveries SET done = false, outcome = NULL, reason = NULL, completed_at = NULL WHERE trigger_id = $1 AND delivery_id = 'dlv-final'`,
			"a trigger skips a version":           `UPDATE git_triggers SET version = version + 2 WHERE id = $1`,
			"a trigger repeats a version":         `UPDATE git_triggers SET enabled = NOT enabled WHERE id = $1`,
		} {
			if err := exec(stmt, trigger); err == nil || !strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "version must advance") {
				t.Errorf("%s: %v, want a guard refusal", name, err)
			}
		}
		for name, stmt := range map[string]string{
			"a delivery changes its body digest": `UPDATE git_trigger_deliveries SET body_digest = 'other' WHERE trigger_id = $1`,
			"a trigger changes its repository":   `UPDATE git_triggers SET repository_id = 'other' WHERE id = $1`,
			"a trigger changes its owner":        `UPDATE git_triggers SET project_id = 'other' WHERE id = $1`,
		} {
			if err := exec(stmt, trigger); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s: %v, want permission denied", name, err)
			}
		}
		// A run cannot start before admission began.
		if _, _, err := e.store.ClaimDelivery(ctx, claimOf(tr, "dlv-norun", "d")); err != nil {
			t.Fatal(err)
		}
		run := e.newRun(a)
		if err := exec(`UPDATE git_trigger_deliveries SET done = true, outcome = 'started', run_id = $2, completed_at = now() WHERE trigger_id = $1 AND delivery_id = 'dlv-norun'`, trigger, string(run)); err == nil {
			t.Fatal("a delivery started without admitting")
		}
		// Admission cannot be withdrawn.
		if _, err := e.store.BeginAdmission(ctx, key("dlv-norun")); err != nil {
			t.Fatal(err)
		}
		if err := exec(`UPDATE git_trigger_deliveries SET admitting = false WHERE trigger_id = $1 AND delivery_id = 'dlv-norun'`, trigger); err == nil || !strings.Contains(err.Error(), "withdrawn") {
			t.Fatalf("admission withdrawn: %v", err)
		}
		// The runtime role can never delete.
		for _, table := range []string{"git_triggers", "git_trigger_deliveries"} {
			err := e.inTenant(a.TenantID, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, `DELETE FROM `+table); return err })
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("delete from %s = %v, want permission denied", table, err)
			}
		}
	})

	t.Run("state survives a restart onto new connections", func(t *testing.T) {
		restarted, err := gwpg.NewStore(e.restartedPool(), gwpg.Options{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := restarted.Trigger(ctx, tr.ID)
		if err != nil || got.Scope != a || got.ID != tr.ID {
			t.Fatalf("trigger after restart: %+v %v", got, err)
		}
		rec, created, err := restarted.ClaimDelivery(ctx, claimOf(tr, "dlv-final", "d"))
		if err != nil || created || !rec.Done || rec.Outcome != gw.OutcomeIgnored {
			t.Fatalf("delivery after restart: %+v created=%v %v", rec, created, err)
		}
		if _, ok, err := restarted.AcquireAdmission(ctx, key("dlv-norun"), "P", time.Now(), time.Minute); err != nil || !ok {
			t.Fatalf("an admitting delivery after restart: ok=%v %v", ok, err)
		}
	})
}

// TestPostgresMigrations proves the contribution's ordering and guarantees.
func TestPostgresMigrations(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	t.Run("the contribution is one valid migration and reapplication is a no-op", func(t *testing.T) {
		c := gwpg.Migrations()
		if c.Module != "gitwebhook" || len(c.Migrations) != 1 || c.Migrations[0].ID != "T100_001_git_triggers.sql" {
			t.Fatalf("contribution = %+v", c)
		}
		if !strings.Contains(c.Migrations[0].SQL, storage.RuntimeRolePlaceholder) {
			t.Fatal("grants must follow the deployment's runtime role")
		}
		manifest, err := storage.NewManifest(contributions()...)
		if err != nil {
			t.Fatal(err)
		}
		out, err := storage.Migrate(ctx, e.x.Migration, manifest, storage.MigrateOptions{RuntimeRole: e.x.Base.DB.RuntimeRole})
		if err != nil || len(out.Applied) != 0 {
			t.Fatalf("reapply applied %v: %v", out.Applied, err)
		}
	})

	t.Run("an edited migration is detected", func(t *testing.T) {
		m := gwpg.Migrations()
		m.Migrations[0].SQL += "\n-- edited"
		cs := contributions()
		cs[len(cs)-1] = m
		manifest, err := storage.NewManifest(cs...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, e.x.Migration, manifest, storage.MigrateOptions{RuntimeRole: e.x.Base.DB.RuntimeRole}); err == nil {
			t.Fatal("a modified applied migration must be rejected")
		}
	})

	t.Run("row level security is enabled and forced everywhere", func(t *testing.T) {
		for _, table := range []string{"git_triggers", "git_trigger_deliveries", "git_trigger_admissions"} {
			var rls, forced bool
			if err := e.x.Migration.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&rls, &forced); err != nil || !rls || !forced {
				t.Errorf("%s: rls=%v forced=%v %v", table, rls, forced, err)
			}
			var deletes int
			if err := e.x.Migration.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = $1 AND cmd IN ('DELETE', 'ALL')`, table).Scan(&deletes); err != nil || deletes != 0 {
				t.Errorf("%s has %d delete-capable policies (%v)", table, deletes, err)
			}
		}
	})
}

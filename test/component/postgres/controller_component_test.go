//go:build component

package postgrescomponent

import (
	"context"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestControllerEventCommitOnPostgreSQL is the real proof of the engine
// commit path: an execution-bound lifecycle event and audit row commit
// atomically with the Kernel state write through the kodama_controller role,
// under forced RLS, the 0013 INSERT grants and the 0014 INSERT policies. It also proves the
// lock-after-scope order (plain SELECT derives the scope, then FOR UPDATE).
func TestControllerEventCommitOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "queued", 1, "", "")
	controllerDB := shard.Role(store.ControllerRole, 4)
	defer controllerDB.Close()
	pgtest.RequireNonOwnerSession(t, ctx, controllerDB, store.ControllerRole)
	controller := storepg.NewController(controllerDB)

	unit := controllerEventUnit(scope, 1, "execution-1", "event-c1", "audit-c1")
	if _, err := controller.Execute(ctx, "execution-1", unit); err != nil {
		t.Fatalf("controller engine commit: %v", err)
	}
	state, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT state FROM executions WHERE id = 'execution-1'`)
	if err != nil || state != "running" {
		t.Fatalf("execution state = %q, err=%v; want running", state, err)
	}
	version, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT version FROM executions WHERE id = 'execution-1'`)
	if err != nil || version != 2 {
		t.Fatalf("execution version = %d, err=%v; want 2", version, err)
	}
	eventCount, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM events WHERE id = 'event-c1'`)
	if err != nil || eventCount != 1 {
		t.Fatalf("event rows = %d, err=%v; want 1", eventCount, err)
	}
	sequence, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT publish_sequence FROM events WHERE id = 'event-c1'`)
	if err != nil || sequence <= 0 {
		t.Fatalf("event publish_sequence = %d, err=%v; want positive", sequence, err)
	}
	publishState, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT publish_state FROM events WHERE id = 'event-c1'`)
	if err != nil || publishState != "pending" {
		t.Fatalf("event publish_state = %q, err=%v; want pending", publishState, err)
	}
	auditCount, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM audit WHERE id = 'audit-c1'`)
	if err != nil || auditCount != 1 {
		t.Fatalf("audit rows = %d, err=%v; want 1", auditCount, err)
	}

	// A foreign-scope event refuses the whole unit before anything is staged.
	foreign := controllerEventUnit(scope, 2, "execution-1", "event-foreign", "audit-foreign")
	foreign.Events[0].Scope = pgtest.Scope("tenant-2", "project-2")
	_, err = controller.Execute(ctx, "execution-1", foreign)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("foreign-scope event error = %v; want forbidden", err)
	}
	if count, _ := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM events WHERE id = 'event-foreign'`); count != 0 {
		t.Fatal("foreign-scope event was inserted")
	}
	if count, _ := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM audit WHERE id = 'audit-foreign'`); count != 0 {
		t.Fatal("foreign-scope audit was inserted")
	}

	// An event/audit unit without a guarded execution row is refused.
	unguarded := controllerEventUnit(scope, 2, "execution-1", "event-unguarded", "audit-unguarded")
	unguarded.Locks = nil
	_, err = controller.Execute(ctx, "execution-1", unguarded)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("unguarded event unit error = %v; want forbidden", err)
	}

	// A receipt stays refused even on the event path.
	receiptUnit := controllerEventUnit(scope, 2, "execution-1", "event-receipt", "audit-receipt")
	receiptUnit.Receipt = &receipt.Record{
		ID: "receipt-controller", Key: receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID, PrincipalID: "principal-1", Operation: "execution.start", CallerKey: "caller-1"},
		ContentHash: pgtest.Fingerprint(`{"x":1}`), Result: []byte(`{}`), At: pgtest.FixtureTime,
	}
	_, err = controller.Execute(ctx, "execution-1", receiptUnit)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("controller receipt error = %v; want forbidden", err)
	}

	// events/audit are INSERT-only: an update write is refused.
	updateUnit := controllerEventUnit(scope, 2, "execution-1", "event-update", "audit-update")
	updateUnit.Events = nil
	updateUnit.Audit = nil
	updateUnit.Writes = append(updateUnit.Writes, store.Write{Table: "events", ID: "event-c1", Values: map[string]any{"publish_state": "published"}})
	_, err = controller.Execute(ctx, "execution-1", updateUnit)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("controller event update error = %v; want forbidden", err)
	}
}

// TestControllerConcurrentEngineUnitsOneWinner proves two engine units on one
// execution cannot both commit: one wins and one gets version_conflict, with
// exactly one event/audit pair persisted.
func TestControllerConcurrentEngineUnitsOneWinner(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "queued", 1, "", "")
	controllerDB := shard.Role(store.ControllerRole, 4)
	defer controllerDB.Close()
	pgtest.RequireNonOwnerSession(t, ctx, controllerDB, store.ControllerRole)
	controller := storepg.NewController(controllerDB)
	runtime := shard.Runtime(2)
	defer runtime.Close()

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for index := range 2 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			suffix := "a"
			if index == 1 {
				suffix = "b"
			}
			unit := controllerEventUnit(scope, 1, "execution-1", "event-"+suffix, "audit-"+suffix)
			<-start
			_, errs[index] = controller.Execute(ctx, "execution-1", unit)
		}(index)
	}
	close(start)
	wg.Wait()
	winners, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			winners++
			continue
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("loser error = %v; want version_conflict", err)
		}
		conflicts++
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d; want exactly one each", winners, conflicts)
	}
	events, _ := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM events`)
	audits, _ := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM audit`)
	if events != 1 || audits != 1 {
		t.Fatalf("events=%d audit=%d; want exactly one committed pair", events, audits)
	}
	// The winner's state and the matching event/audit pair are durable through
	// the application role; the loser left neither event nor audit.
	executionRow := scopedRow(t, ctx, runtime, scope, 2,
		`SELECT state, version FROM executions WHERE id = 'execution-1'`)
	if executionRow[0] != "running" || executionRow[1] != "2" {
		t.Fatalf("winning execution state/version = %v; want running/2", executionRow)
	}
	eventA := scopedCount(t, ctx, runtime, scope, `SELECT count(*) FROM events WHERE id = 'event-a'`)
	eventB := scopedCount(t, ctx, runtime, scope, `SELECT count(*) FROM events WHERE id = 'event-b'`)
	auditA := scopedCount(t, ctx, runtime, scope, `SELECT count(*) FROM audit WHERE id = 'audit-a'`)
	auditB := scopedCount(t, ctx, runtime, scope, `SELECT count(*) FROM audit WHERE id = 'audit-b'`)
	if eventA+eventB != 1 || auditA+auditB != 1 {
		t.Fatalf("event pairs a=%d b=%d audit a=%d b=%d; want exactly one each", eventA, eventB, auditA, auditB)
	}
	if eventA != auditA || eventB != auditB {
		t.Fatalf("event/audit winners differ: event-a=%d audit-a=%d event-b=%d audit-b=%d", eventA, auditA, eventB, auditB)
	}
}

func controllerEventUnit(scope tenancy.Scope, expectVersion uint64, executionID, eventID, auditID string) store.Unit {
	return store.Unit{
		Locks: []store.Lock{{Table: "executions", IDs: []string{executionID}, ExpectVersion: expectVersion}},
		Writes: []store.Write{{
			Table: "executions", ID: executionID, ExpectVersion: expectVersion,
			Values: map[string]any{"state": "running"},
		}},
		Events: []event.Envelope{{
			ID: id.EventID(eventID), Type: event.TypeExecutionStarted,
			Source: event.Source{Kind: event.SourceShell, PrincipalID: "principal-1"},
			At:     pgtest.FixtureTime, Scope: scope, SubjectID: executionID, SubjectVersion: expectVersion + 1,
		}},
		Audit: []event.Audit{{
			ID: id.EventID(auditID), Scope: scope, Actor: "principal-1", Operation: "execution.start", At: pgtest.FixtureTime,
		}},
	}
}

// TestControllerUnscopedPolicyReadOnPostgreSQL proves the controller's declared
// cross-scope SELECT policy in the production shape: an UNSCOPED session (no
// kodama.tenant_id/project_id) reads executions from different tenants. It is a
// raw-policy proof; the product Controller.Read adapter's own production shape
// is asserted by TestControllerReadProductionShapeOnPostgreSQL.
func TestControllerUnscopedPolicyReadOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-2", "project-2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	shard.SeedExecution(ctx, scope1, "execution-1", "queued", 1, "", "")
	shard.SeedExecution(ctx, scope2, "execution-2", "queued", 1, "", "")
	controllerDB := shard.Role(store.ControllerRole, 2)
	defer controllerDB.Close()
	pgtest.RequireNonOwnerSession(t, ctx, controllerDB, store.ControllerRole)
	rows, err := pgtest.QueryStringsN(ctx, controllerDB, 2,
		`SELECT id, tenant_id FROM executions ORDER BY id`)
	if err != nil {
		t.Fatalf("controller unscoped policy read: %v%s", err, pgtest.CauseText(err))
	}
	if len(rows) != 2 || rows[0][1] != "tenant-1" || rows[1][1] != "tenant-2" {
		t.Fatalf("controller unscoped rows = %v; want both tenants", rows)
	}
}

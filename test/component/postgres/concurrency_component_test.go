//go:build component

package postgrescomponent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestExpectedVersionRace proves exactly one winner on a real optimistic
// version race and that the loser leaves no partial state.
func TestExpectedVersionRace(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "queued", 1, "", "")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for index := range 2 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, results[index] = executor.Execute(ctx, scope, store.Unit{
				Writes: []store.Write{{
					Table: "executions", ID: "execution-1", ExpectVersion: 1,
					Values: map[string]any{"state": "running"},
				}},
			})
		}(index)
	}
	close(start)
	wg.Wait()

	winners, losers := 0, 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("loser error = %v; want version_conflict", err)
		}
		losers++
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners=%d losers=%d; want exactly one of each", winners, losers)
	}
	row, err := executor.Read(ctx, scope, "executions", "execution-1")
	if err != nil {
		t.Fatalf("read execution: %v", err)
	}
	if row.Table != "executions" || row.Values["id"] != "execution-1" ||
		row.Values["tenant_id"] != "tenant-1" || row.Values["project_id"] != "project-1" ||
		row.Values["state"] != "running" || row.Values["version"] != int64(2) {
		t.Fatalf("execution row = %v; want execution-1 in tenant-1/project-1 running at version 2", row.Values)
	}
}

// TestReceiptCompareAndSetUnderConcurrency inserts an incomplete receipt and
// races two completion units. Exactly one applies; the loser replays the
// winner's durable result. A changed-content completion is refused and a replay
// returns the original result.
func TestReceiptCompareAndSetUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	key := receipt.Key{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID,
		PrincipalID: "principal-1", Operation: "trigger.select", CallerKey: "caller-1",
	}
	content := `{"selection":"build"}`
	contentHash := pgtest.Fingerprint(content)
	incomplete := receipt.Record{
		ID: "receipt-1", Key: key, ContentHash: contentHash,
		Result: []byte(`{"in_progress":true}`), At: pgtest.FixtureTime,
	}
	if _, err := executor.Execute(ctx, scope, store.Unit{
		Receipt: &incomplete, ReceiptMode: store.ReceiptInsertIncomplete,
	}); err != nil {
		t.Fatalf("insert incomplete receipt: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	type outcome struct {
		result store.UnitResult
		err    error
	}
	outcomes := make([]outcome, 2)
	for index := range 2 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			record := receipt.Record{
				ID: "receipt-1", Key: key, ContentHash: contentHash,
				Result: []byte(`{"winner":` + string(rune('1'+index)) + `}`), At: pgtest.FixtureTime,
			}
			<-start
			outcomes[index].result, outcomes[index].err = executor.Execute(ctx, scope, store.Unit{
				Receipt:       &record,
				ReceiptMode:   store.ReceiptComplete,
				ReceiptExpect: &store.ReceiptExpect{ContentHash: contentHash},
			})
		}(index)
	}
	close(start)
	wg.Wait()

	applied, replayed := 0, 0
	for _, outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("completion error = %v; want a winner and a replay", outcome.err)
		}
		if outcome.result.Replayed {
			replayed++
		} else {
			applied++
		}
	}
	if applied != 1 || replayed != 1 {
		t.Fatalf("applied=%d replayed=%d; want exactly one each", applied, replayed)
	}

	// Both outcomes must return the durable result, not the caller's own
	// payload: the loser may take the already-completed branch or the zero-row
	// reclassify branch, and neither may hand back a result it did not commit.
	// Reading as the application role makes the comparison independent of the
	// goroutine iteration order.
	durableRow := scopedRow(t, ctx, runtime, scope, 1, `SELECT result FROM receipts WHERE id = 'receipt-1'`)
	winnerResult := durableRow[0]
	for index, outcome := range outcomes {
		if got := string(outcome.result.Receipt.Result); got != winnerResult {
			t.Fatalf("outcome %d result = %s; want the durable %s", index, got, winnerResult)
		}
	}

	// A sequential completion retry takes the already-completed branch
	// deterministically: a different result with the same content hash is a
	// replay of the original result, never the caller's payload.
	retry := receipt.Record{
		ID: "receipt-1", Key: key, ContentHash: contentHash,
		Result: []byte(`{"winner":"retry"}`), At: pgtest.FixtureTime,
	}
	retried, err := executor.Execute(ctx, scope, store.Unit{
		Receipt: &retry, ReceiptMode: store.ReceiptComplete,
		ReceiptExpect: &store.ReceiptExpect{ContentHash: contentHash},
	})
	if err != nil {
		t.Fatalf("sequential completion retry: %v", err)
	}
	if !retried.Replayed || string(retried.Receipt.Result) != winnerResult {
		t.Fatalf("sequential retry = %+v; want a replay of the original %s", retried, winnerResult)
	}

	// An insert-mode unit for the same key with different content is refused
	// even after the receipt completed: a completed receipt is not a blank
	// check for a different request.
	staleInsert := receipt.Record{
		ID: "receipt-2", Key: key, ContentHash: pgtest.Fingerprint(`{"selection":"stale"}`),
		Result: []byte(`{"stale":true}`), At: pgtest.FixtureTime,
	}
	_, err = executor.Execute(ctx, scope, store.Unit{Receipt: &staleInsert})
	if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
		t.Fatalf("insert-mode changed-content error = %v; want changed_idempotency_content", err)
	}

	// Changed content is refused and never replaces the committed result.
	changed := receipt.Record{
		ID: "receipt-1", Key: key, ContentHash: pgtest.Fingerprint(`{"selection":"other"}`),
		Result: []byte(`{"winner":"other"}`), At: pgtest.FixtureTime,
	}
	_, err = executor.Execute(ctx, scope, store.Unit{
		Receipt: &changed, ReceiptMode: store.ReceiptComplete,
		ReceiptExpect: &store.ReceiptExpect{ContentHash: changed.ContentHash},
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
		t.Fatalf("changed-content error = %v; want changed_idempotency_content", err)
	}

	// A replay of the original content returns the original durable result.
	replay := receipt.Record{
		ID: "receipt-1", Key: key, ContentHash: contentHash,
		Result: []byte(`{"ignored":true}`), At: pgtest.FixtureTime,
	}
	result, err := executor.Execute(ctx, scope, store.Unit{Receipt: &replay})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !result.Replayed || string(result.Receipt.Result) != winnerResult {
		t.Fatalf("replay result = %+v; want original %s", result, winnerResult)
	}

	// The committed receipt row is durable and unique, read back through the
	// application role. A success that persisted nothing cannot pass.
	requireScopedCount(t, ctx, runtime, scope,
		`SELECT count(*) FROM receipts WHERE id = 'receipt-1'`, 1, "durable receipt rows")
	durable := scopedRow(t, ctx, runtime, scope, 1, `SELECT result FROM receipts WHERE id = 'receipt-1'`)
	if durable[0] != winnerResult {
		t.Fatalf("durable receipt result = %q; want %q", durable[0], winnerResult)
	}
	requireScopedCount(t, ctx, runtime, scope,
		`SELECT count(*) FROM receipts WHERE operation = 'trigger.select' AND caller_key = 'caller-1'`, 1, "durable receipt key rows")
}

// TestReceiptZeroRowReclassificationOnPostgreSQL forces the completion loser
// through the zero-row reclassify path: a test-owned session holds the winning
// UPDATE uncommitted until the product completer is blocked on the receipt row,
// then commits. The completer must return the durable winner result, not its
// own payload, and must not overwrite the row.
func TestReceiptZeroRowReclassificationOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	key := receipt.Key{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID,
		PrincipalID: "principal-1", Operation: "trigger.select", CallerKey: "caller-1",
	}
	contentHash := pgtest.Fingerprint(`{"selection":"build"}`)
	incomplete := receipt.Record{
		ID: "receipt-1", Key: key, ContentHash: contentHash,
		Result: []byte(`{"in_progress":true}`), At: pgtest.FixtureTime,
	}
	if _, err := executor.Execute(ctx, scope, store.Unit{
		Receipt: &incomplete, ReceiptMode: store.ReceiptInsertIncomplete,
	}); err != nil {
		t.Fatalf("insert incomplete receipt: %v", err)
	}

	// The holder performs the winning completion but does not commit yet.
	holder, err := storepg.New(runtime).Begin(ctx, scope)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
	winning := []byte(`{"winner":"held"}`)
	if _, err := holder.Exec(ctx, store.Statement{
		SQL:  `UPDATE receipts SET result = $1, completed_at = $2 WHERE id = 'receipt-1' AND completed_at IS NULL`,
		Args: []any{winning, pgtest.FixtureTime},
	}); err != nil {
		t.Fatalf("holder completion: %v", err)
	}

	type outcome struct {
		result store.UnitResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record := receipt.Record{
			ID: "receipt-1", Key: key, ContentHash: contentHash,
			Result: []byte(`{"winner":"caller"}`), At: pgtest.FixtureTime,
		}
		result, err := executor.Execute(ctx, scope, store.Unit{
			Receipt: &record, ReceiptMode: store.ReceiptComplete,
			ReceiptExpect: &store.ReceiptExpect{ContentHash: contentHash},
		})
		done <- outcome{result: result, err: err}
	}()
	requireReceiptLockWaiter(t, ctx, shard, 1)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	loser := <-done
	if loser.err != nil {
		t.Fatalf("zero-row completion error = %v; want a replay", loser.err)
	}
	if !loser.result.Replayed {
		t.Fatalf("zero-row completion replayed=false; want the durable winner")
	}
	if string(loser.result.Receipt.Result) != string(winning) {
		t.Fatalf("zero-row completion result = %s; want the durable %s", loser.result.Receipt.Result, winning)
	}
	durable := scopedRow(t, ctx, runtime, scope, 1, `SELECT result FROM receipts WHERE id = 'receipt-1'`)
	if durable[0] != string(winning) {
		t.Fatalf("durable receipt result = %q; want the holder's %q", durable[0], winning)
	}
}

// requireReceiptLockWaiter polls pg_stat_activity until exactly want backends
// are blocked on a receipts UPDATE. It is a bounded barrier, not a sleep.
func requireReceiptLockWaiter(t *testing.T, ctx context.Context, shard *pgtest.Shard, want int64) {
	t.Helper()
	observer, err := shard.RawSession(ctx, "postgres")
	if err != nil {
		t.Fatalf("observer session: %v", err)
	}
	defer observer.Close(context.WithoutCancel(ctx))
	deadline := time.Now().Add(15 * time.Second)
	for {
		rows, err := observer.Query(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND state = 'active' AND pid <> pg_backend_pid()
			  AND query LIKE '%UPDATE receipts%'`)
		if err != nil {
			t.Fatalf("observe receipt waiters: %v", err)
		}
		var waiters int64
		if !rows.Next() {
			rows.Close()
			t.Fatalf("observe receipt waiters: no row: %v", rows.Err())
		}
		if err := rows.Scan(&waiters); err != nil {
			rows.Close()
			t.Fatalf("scan receipt waiters: %v", err)
		}
		rows.Close()
		if waiters == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("receipt lock waiters = %d; want %d", waiters, want)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("observe receipt waiters: %v", ctx.Err())
		default:
		}
	}
}

// TestPipelineNameUniquenessUnderConcurrency proves the declared unique
// constraint lets exactly one of two same-name applies commit.
func TestPipelineNameUniquenessUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for index := range 2 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, errs[index] = executor.Execute(ctx, scope, store.Unit{
				Writes: []store.Write{{
					Table: "pipelines", ID: "pipeline-" + string(rune('a'+index)), Insert: true,
					Values: map[string]any{
						"name": "build", "yaml": []byte("actions: []\n"),
						"yaml_sha256": []byte{0}, "push_triggers": []byte(`[]`),
						"version": int64(1), "created_at": pgtest.FixtureTime,
					},
				}},
			})
		}(index)
	}
	close(start)
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
			continue
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("loser error = %v; want version_conflict", err)
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d; want exactly one each", successes, conflicts)
	}
	count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*)::text FROM pipelines WHERE name = 'build'`)
	if err != nil || count != 1 {
		t.Fatalf("pipeline name count = %d, err=%v; want 1", count, err)
	}
	// The durable winner is also visible to the application role.
	requireScopedCount(t, ctx, runtime, scope,
		`SELECT count(*) FROM pipelines WHERE name = 'build'`, 1, "durable pipeline name rows")
}

// TestUnitRollbackAtomicityAcrossStateEventAuditReceipt proves one unit commits
// state, event, audit and receipt together and that a failing unit commits
// none of them.
func TestUnitRollbackAtomicityAcrossStateEventAuditReceipt(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedPipeline(ctx, scope, "pipeline-1", "build", []byte("actions: []\n"), 1, "[]")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	record := receipt.Record{
		ID: "receipt-1",
		Key: receipt.Key{
			TenantID: scope.TenantID, ProjectID: scope.ProjectID,
			PrincipalID: "principal-1", Operation: "execution.start", CallerKey: "caller-1",
		},
		ContentHash: pgtest.Fingerprint(`{"start":1}`),
		Result:      []byte(`{"ok":true}`),
		At:          pgtest.FixtureTime,
	}
	envelope := event.Envelope{
		ID: "event-1", Type: event.TypeExecutionQueued,
		Source: event.Source{Kind: event.SourceShell, PrincipalID: "principal-1"},
		At:     pgtest.FixtureTime, Scope: scope, SubjectID: "execution-1", SubjectVersion: 1,
	}
	audit := event.Audit{
		ID: "audit-1", Scope: scope, Actor: "principal-1", Operation: "execution.start", At: pgtest.FixtureTime,
	}
	unit := store.Unit{
		Writes: []store.Write{{
			Table: "executions", ID: "execution-1", Insert: true,
			Values: map[string]any{
				"pipeline_id": "pipeline-1", "task_id": nil, "principal_id": "principal-1",
				"state": "queued", "version": int64(1), "yaml_sha256": []byte{0},
				"snapshot": []byte(`{}`), "token_nonce": nil, "token_expires_at": nil,
				"workspace_id": "workspace-1", "created_at": pgtest.FixtureTime,
			},
		}},
		Receipt: &record,
		Events:  []event.Envelope{envelope},
		Audit:   []event.Audit{audit},
	}
	if _, err := executor.Execute(ctx, scope, unit); err != nil {
		t.Fatalf("commit atomic unit: %v", err)
	}
	requireUnitRowCounts(t, ctx, shard, 1)
	requireScopedUnitRowCounts(t, ctx, runtime, scope, 1)

	// A second unit is valid through its write, receipt and audit inserts and
	// fails only on the final event insert, which reuses the committed event id.
	// The first three tables are staged before that failure, so the rollback
	// assertion is not true by construction.
	failing := unit
	failing.Writes = append([]store.Write(nil), unit.Writes...)
	failing.Writes[0].ID = "execution-2"
	failing.Writes[0].Values = map[string]any{
		"pipeline_id": "pipeline-1", "task_id": nil, "principal_id": "principal-1",
		"state": "queued", "version": int64(1), "yaml_sha256": []byte{0},
		"snapshot": []byte(`{}`), "token_nonce": nil, "token_expires_at": nil,
		"workspace_id": "workspace-2", "created_at": pgtest.FixtureTime,
	}
	secondRecord := record
	secondRecord.ID = "receipt-2"
	secondRecord.Key.CallerKey = "caller-2"
	failing.Receipt = &secondRecord
	// The event id is already committed, so the last statement of the unit
	// violates the event primary key after the write, receipt and audit landed.
	failing.Events = []event.Envelope{envelope}
	secondAudit := audit
	secondAudit.ID = "audit-2"
	failing.Audit = []event.Audit{secondAudit}
	_, err := executor.Execute(ctx, scope, failing)
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("duplicate-event unit error = %v; want version_conflict", err)
	}
	requireUnitRowCounts(t, ctx, shard, 1)
	requireScopedUnitRowCounts(t, ctx, runtime, scope, 1)
	requireNoRow(t, ctx, shard, "executions", "execution-2")
	requireNoRow(t, ctx, shard, "receipts", "receipt-2")
	requireNoRow(t, ctx, shard, "audit", "audit-2")

	// A second failure point: the state write lands, then the receipt insert
	// collides on its primary key before audit and events are staged.
	early := unit
	early.Writes = append([]store.Write(nil), unit.Writes...)
	early.Writes[0].ID = "execution-3"
	early.Writes[0].Values = map[string]any{
		"pipeline_id": "pipeline-1", "task_id": nil, "principal_id": "principal-1",
		"state": "queued", "version": int64(1), "yaml_sha256": []byte{0},
		"snapshot": []byte(`{}`), "token_nonce": nil, "token_expires_at": nil,
		"workspace_id": "workspace-3", "created_at": pgtest.FixtureTime,
	}
	earlyRecord := record
	earlyRecord.ID = "receipt-1"
	earlyRecord.Key.CallerKey = "caller-3"
	earlyRecord.ContentHash = pgtest.Fingerprint(`{"start":3}`)
	early.Receipt = &earlyRecord
	thirdEvent := envelope
	thirdEvent.ID = "event-3"
	early.Events = []event.Envelope{thirdEvent}
	thirdAudit := audit
	thirdAudit.ID = "audit-3"
	early.Audit = []event.Audit{thirdAudit}
	_, err = executor.Execute(ctx, scope, early)
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("duplicate-receipt unit error = %v; want version_conflict", err)
	}
	requireUnitRowCounts(t, ctx, shard, 1)
	requireScopedUnitRowCounts(t, ctx, runtime, scope, 1)
	requireNoRow(t, ctx, shard, "executions", "execution-3")
	requireNoRow(t, ctx, shard, "audit", "audit-3")
	requireNoRow(t, ctx, shard, "events", "event-3")
}

func requireUnitRowCounts(t *testing.T, ctx context.Context, shard *pgtest.Shard, want int64) {
	t.Helper()
	for _, table := range []string{"executions", "events", "audit", "receipts"} {
		pgtest.RequireRowCount(t, ctx, shard.Admin(), `SELECT count(*)::text FROM `+table, want, table+" rows")
	}
}

// requireScopedUnitRowCounts reads the same durable counts back through the
// application role, so the atomic unit is proven visible to the role that will
// read it after commit.
func requireScopedUnitRowCounts(t *testing.T, ctx context.Context, runtime *pgdriver.DB, scope tenancy.Scope, want int64) {
	t.Helper()
	for _, table := range []string{"executions", "events", "audit", "receipts"} {
		requireScopedCount(t, ctx, runtime, scope, `SELECT count(*) FROM `+table, want, table+" scoped rows")
	}
}

func requireNoRow(t *testing.T, ctx context.Context, shard *pgtest.Shard, table, rowID string) {
	t.Helper()
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM `+table+` WHERE id = $1`, 0, table+" row "+rowID, rowID)
}

// TestLockOrderSerializesOppositeAcquisition proves two units that name the
// same two rows in opposite order acquire the same first lock and do not
// deadlock. A test-owned session holds the tasks row, both units are observed
// blocked on that same row while the executions row stays free, and only then
// is the holder released. It also drives a real two-session 40P01 deadlock
// through the driver wrapper and asserts the declared fault class.
func TestLockOrderSerializesOppositeAcquisition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedTask(ctx, scope, "task-1", "open", 1, "")
	shard.SeedExecution(ctx, scope, "execution-1", "queued", 1, "", "")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	pgtest.RequireNonOwnerSession(t, ctx, runtime, "kodama_runtime")
	executor := storepg.New(runtime)

	// The holder owns the tasks row for the whole interleaving. The units must
	// therefore queue on tasks first, whichever order they name their locks.
	holder, err := shard.RawSession(ctx, "postgres")
	if err != nil {
		t.Fatalf("holder session: %v", err)
	}
	defer holder.Close(context.WithoutCancel(ctx))
	if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := holder.Exec(ctx, `SELECT id FROM tasks WHERE id = $1 FOR UPDATE`, "task-1"); err != nil {
		t.Fatalf("holder lock tasks: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_, _ = holder.Exec(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for index := range 2 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			locks := []store.Lock{
				{Table: "tasks", IDs: []string{"task-1"}},
				{Table: "executions", IDs: []string{"execution-1"}},
			}
			if index == 1 {
				locks[0], locks[1] = locks[1], locks[0]
			}
			<-start
			_, errs[index] = executor.Execute(ctx, scope, store.Unit{Locks: locks})
		}(index)
	}
	close(start)

	// Barrier: both units must be waiting, both on the tasks row and neither on
	// the executions row. A dropped or reversed lock sort leaves the
	// executions row held and fails the NOWAIT probe below.
	requireLockWaiters(t, ctx, shard, 2, 0)
	probe, err := shard.RawSession(ctx, "postgres")
	if err != nil {
		t.Fatalf("probe session: %v", err)
	}
	defer probe.Close(context.WithoutCancel(ctx))
	if _, err := probe.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("probe begin: %v", err)
	}
	if _, err := probe.Exec(ctx, `SELECT id FROM executions WHERE id = $1 FOR UPDATE NOWAIT`, "execution-1"); err != nil {
		_, _ = probe.Exec(context.WithoutCancel(ctx), `ROLLBACK`)
		t.Fatalf("executions row was held while both units waited on tasks: %v%s", err, pgtest.CauseText(err))
	}
	if _, err := probe.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("probe rollback: %v", err)
	}

	if _, err := holder.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	released = true
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("unit %d error = %v; want both units to commit after the same first lock", index, err)
		}
	}

	t.Run("driver maps a real deadlock", func(t *testing.T) {
		requireDeadlockMapping(t, ctx, shard, scope)
	})
}

// requireLockWaiters polls pg_stat_activity until exactly wantTasks backends are
// blocked on a tasks lock and wantExec backends on an executions lock. The loop
// is a bounded barrier with no fixed sleep: each iteration is one catalog
// round-trip.
func requireLockWaiters(t *testing.T, ctx context.Context, shard *pgtest.Shard, wantTasks, wantExec int64) {
	t.Helper()
	observer, err := shard.RawSession(ctx, "postgres")
	if err != nil {
		t.Fatalf("observer session: %v", err)
	}
	defer observer.Close(context.WithoutCancel(ctx))
	deadline := time.Now().Add(15 * time.Second)
	var tasksWait, execWait int64
	for {
		rows, err := observer.Query(ctx, `SELECT
			count(*) FILTER (WHERE query LIKE '%FROM tasks%'),
			count(*) FILTER (WHERE query LIKE '%FROM executions%')
			FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND state = 'active' AND pid <> pg_backend_pid()`)
		if err != nil {
			t.Fatalf("observe lock waiters: %v", err)
		}
		if !rows.Next() {
			rows.Close()
			t.Fatalf("observe lock waiters: no row: %v", rows.Err())
		}
		if err := rows.Scan(&tasksWait, &execWait); err != nil {
			rows.Close()
			t.Fatalf("scan lock waiters: %v", err)
		}
		rows.Close()
		if tasksWait == wantTasks && execWait == wantExec {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock waiters = tasks:%d executions:%d; want tasks:%d executions:%d",
				tasksWait, execWait, wantTasks, wantExec)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("observe lock waiters: %v", ctx.Err())
		default:
		}
	}
}

// requireDeadlockMapping runs a real cross-lock cycle through two scoped driver
// wrapper transactions. PostgreSQL raises 40P01 for the victim; the wrapper must
// map that to the declared version_conflict class.
func requireDeadlockMapping(t *testing.T, ctx context.Context, shard *pgtest.Shard, scope tenancy.Scope) {
	t.Helper()
	runtime := shard.Runtime(4)
	defer runtime.Close()
	scoped := storepg.New(runtime)
	txA, err := scoped.Begin(ctx, scope)
	if err != nil {
		t.Fatalf("deadlock session A begin: %v", err)
	}
	defer func() { _ = txA.Rollback(context.WithoutCancel(ctx)) }()
	txB, err := scoped.Begin(ctx, scope)
	if err != nil {
		t.Fatalf("deadlock session B begin: %v", err)
	}
	defer func() { _ = txB.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := txA.Exec(ctx, store.Statement{
		SQL: `SELECT id FROM tasks WHERE id = $1 FOR UPDATE`, Args: []any{"task-1"},
	}); err != nil {
		t.Fatalf("session A lock tasks: %v", err)
	}
	if _, err := txB.Exec(ctx, store.Statement{
		SQL: `SELECT id FROM executions WHERE id = $1 FOR UPDATE`, Args: []any{"execution-1"},
	}); err != nil {
		t.Fatalf("session B lock executions: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errA = txA.Exec(ctx, store.Statement{
			SQL: `SELECT id FROM executions WHERE id = $1 FOR UPDATE`, Args: []any{"execution-1"},
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errB = txB.Exec(ctx, store.Statement{
			SQL: `SELECT id FROM tasks WHERE id = $1 FOR UPDATE`, Args: []any{"task-1"},
		})
	}()
	close(start)
	wg.Wait()

	deadlocks := 0
	for index, err := range []error{errA, errB} {
		if err == nil {
			continue
		}
		code, ok := pgtest.SQLState(err)
		if !ok || code != "40P01" {
			t.Fatalf("session %d error = %v; want SQLSTATE 40P01", index, err)
		}
		class, ok := fault.ClassOf(err)
		if !ok || class != fault.VersionConflict {
			t.Fatalf("session %d deadlock class = %q; want version_conflict", index, class)
		}
		deadlocks++
	}
	if deadlocks != 1 {
		t.Fatalf("deadlock errors = %d; want exactly one victim", deadlocks)
	}
}

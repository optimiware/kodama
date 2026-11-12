//go:build component

package postgrescomponent

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	identityunits "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestMembershipFirstGrantAbsenceRaceOnPostgreSQL proves two real sessions
// granting the same absent membership key race on the absence guard:
// exactly one commits and the other observes version_conflict, for both a
// tenant-wide (NULL project) key and a project key.
func TestMembershipFirstGrantAbsenceRaceOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(6)
	defer runtime.Close()
	adapter, err := identityunits.New(identityunits.Config{
		Executor: storepg.New(runtime), Scanner: storepg.New(runtime),
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	// Both racers of a subtest draw one row id each from the adapter's shared
	// counter before the guard decides the winner, and the subtests run
	// sequentially, so the winning row holds one of the two ids drawn by its
	// own subtest.
	drawn := 0
	for name, scope := range map[string]tenancy.Scope{
		"tenant-wide": pgtest.TenantScope("tenant-1"),
		"project":     pgtest.Scope("tenant-1", "project-1"),
	} {
		rowIDs := []any{fmt.Sprintf("row-%d", drawn+1), fmt.Sprintf("row-%d", drawn+2)}
		drawn += 2
		t.Run(name, func(t *testing.T) {
			principal := id.PrincipalID("race-" + name)
			observer := shard.ObserveUnit(t, "membership", "first-grant."+name)
			type outcome struct {
				result tenancyadmin.MembershipGrantResult
				err    error
			}
			outcomes := make([]outcome, 2)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for index := range 2 {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()
					record := receipt.Record{ID: id.ReceiptID(fmt.Sprintf("receipt-race-%s-%d", name, index)),
						Key: receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID,
							PrincipalID: "principal-1", Operation: "membership.grant",
							CallerKey: fmt.Sprintf("race-%s-%d", name, index)},
						ContentHash: pgtest.Fingerprint(name), Result: []byte(`{"version":1}`), At: pgtest.FixtureTime}
					<-start
					outcomes[index].result, outcomes[index].err = adapter.GrantMembership(ctx, tenancyadmin.MembershipGrantUnit{
						Owner: scope,
						Membership: tenancy.Membership{Scope: scope, PrincipalID: principal,
							Roles: []tenancy.Role{tenancy.RoleEngineer}, Version: 1},
						ExpectedVersion: 0, At: pgtest.FixtureTime, Receipt: record,
					})
				}(index)
			}
			close(start)
			wg.Wait()
			winners, losers := 0, 0
			for _, outcome := range outcomes {
				if outcome.err == nil {
					winners++
					continue
				}
				if class, ok := fault.ClassOf(outcome.err); !ok || class != fault.VersionConflict {
					t.Fatalf("loser error = %v; want version_conflict", outcome.err)
				}
				losers++
			}
			if winners != 1 || losers != 1 {
				t.Fatalf("winners=%d losers=%d; want exactly one each", winners, losers)
			}
			shard.RequireGoldenRow(t, ctx, runtime, scope, "memberships", map[string]any{"principal_id": string(principal)}, map[string]any{
				"id": pgtest.GoldenOneOf(rowIDs...), "tenant_id": "tenant-1", "project_id": projectValue(scope),
				"principal_id": string(principal), "roles": `["engineer"]`,
				"version": int64(1), "created_at": pgtest.FixtureTime,
			})
			for index := range 2 {
				receiptID := fmt.Sprintf("receipt-race-%s-%d", name, index)
				if outcomes[index].err != nil {
					shard.RequireRowAbsent(t, ctx, runtime, scope, "receipts", map[string]any{"id": receiptID})
					continue
				}
				shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": receiptID}, map[string]any{
					"id": receiptID, "tenant_id": "tenant-1", "project_id": projectValue(scope),
					"principal_id": "principal-1", "operation": "membership.grant",
					"caller_key":     fmt.Sprintf("race-%s-%d", name, index),
					"content_sha256": pgtest.FingerprintBytes(name),
					"result":         pgtest.GoldenJSONOf(rawJSON([]byte(`{"version":1}`))),
					"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
					"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
				})
			}
			observer.Finish(map[string]int{"memberships": 1, "receipts": 1})
		})
	}
}

func projectValue(scope tenancy.Scope) any {
	if scope.TenantScoped() {
		return pgtest.NullValue
	}
	return string(scope.ProjectID)
}

type membershipGrantOutcome struct {
	result tenancyadmin.MembershipGrantResult
	err    error
}

// startMembershipGrant runs one real first-grant unit in its own goroutine and
// reports the outcome. The receipt carries the caller identity the executor
// records on success.
func startMembershipGrant(ctx context.Context, adapter *identityunits.Adapter,
	scope tenancy.Scope, principal id.PrincipalID, expected, version uint64) <-chan membershipGrantOutcome {
	out := make(chan membershipGrantOutcome, 1)
	go func() {
		record := receipt.Record{
			ID: "receipt-gap-a",
			Key: receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID,
				PrincipalID: "principal-admin", Operation: "membership.grant", CallerKey: "gap-a"},
			ContentHash: pgtest.Fingerprint("gap-a"), Result: []byte(`{"version":1}`), At: pgtest.FixtureTime,
		}
		audit := event.Audit{ID: "audit-gap-a", Scope: scope, Actor: "principal-admin",
			Operation: "membership.grant", At: pgtest.FixtureTime}
		envelope := event.Envelope{ID: "event-gap-a", Type: event.TypeConfigChanged,
			Source: event.Source{Kind: event.SourceShell, PrincipalID: "principal-admin"},
			At:     pgtest.FixtureTime, Scope: scope, SubjectID: string(principal), SubjectVersion: version}
		result, err := adapter.GrantMembership(ctx, tenancyadmin.MembershipGrantUnit{
			Owner: scope,
			Membership: tenancy.Membership{Scope: scope, PrincipalID: principal,
				Roles: []tenancy.Role{tenancy.RoleEngineer}, Version: version},
			ExpectedVersion: expected, At: pgtest.FixtureTime, Receipt: record,
			Audit: []event.Audit{audit}, Events: []event.Envelope{envelope},
		})
		out <- membershipGrantOutcome{result: result, err: err}
	}()
	return out
}

// waitForBlockedMembershipWriter polls pg_stat_activity with a bounded deadline
// until one backend is blocked on a memberships statement, and returns that
// backend's pid. Polling is the bounded condition; no fixed sleep is used as a
// correctness argument.
func waitForBlockedMembershipWriter(t *testing.T, shard *pgtest.Shard) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := pgtest.QueryStrings(context.Background(), shard.Admin(),
			`SELECT pid::text FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND pid <> pg_backend_pid() AND query ILIKE '%memberships%'`)
		if err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if len(rows) > 0 {
			return rows[0][0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no backend blocked on a memberships statement within the deadline")
	return ""
}

// requireHeldAdvisoryLock proves the blocked first-grant backend holds exactly
// one granted advisory lock: the absence lock it took before the guard and
// the insert. The unique index alone would still serialize the insert, so only
// this pg_locks fact distinguishes the advisory-lock layer.
func requireHeldAdvisoryLock(t *testing.T, ctx context.Context, shard *pgtest.Shard, pid string) {
	t.Helper()
	backend, err := strconv.Atoi(pid)
	if err != nil {
		t.Fatalf("blocked backend pid %q is not an integer: %v", pid, err)
	}
	held, err := pgtest.ScalarString(ctx, shard.Admin(),
		`SELECT count(*)::text FROM pg_locks WHERE pid = $1 AND locktype = 'advisory' AND granted`, backend)
	if err != nil {
		t.Fatalf("pg_locks for backend %d: %v", backend, err)
	}
	if held != "1" {
		t.Fatalf("blocked backend %d holds %s granted advisory locks; want exactly 1 (the absence lock)", backend, held)
	}
}

// awaitMembershipGrant returns the unit outcome or fails the bounded wait.
func awaitMembershipGrant(t *testing.T, out <-chan membershipGrantOutcome) membershipGrantOutcome {
	t.Helper()
	select {
	case outcome := <-out:
		return outcome
	case <-time.After(20 * time.Second):
		t.Fatal("membership grant did not settle after the competing session ended")
		return membershipGrantOutcome{}
	}
}

// TestMembershipGrantReadCommittedGapsOnPostgreSQL proves the first-grant
// absence guard and the present-row id+version pin both hold under READ
// COMMITTED: an uncommitted competing row is invisible to the guard but the
// unique index still serializes the insert, and an uncommitted row lock blocks
// the versioned lock read. The competing session is a raw admin transaction, so
// no product hook is needed.
func TestMembershipGrantReadCommittedGapsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()

	t.Run("competing insert commits", func(t *testing.T) {
		shard := migratedShard(t)
		shard.SeedScope(ctx, "tenant-1", "project-1")
		runtime := shard.Runtime(6)
		defer runtime.Close()
		adapter, err := identityunits.New(identityunits.Config{
			Executor: storepg.New(runtime), Scanner: storepg.New(runtime),
			Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
		})
		if err != nil {
			t.Fatalf("identity.New: %v", err)
		}
		scope := pgtest.TenantScope("tenant-1")
		principal := id.PrincipalID("gap-insert")
		observer := shard.ObserveUnit(t, "membership", "first-grant.read-committed-commit")
		sessionB, err := shard.Admin().Begin(ctx)
		if err != nil {
			t.Fatalf("begin competing session: %v", err)
		}
		defer func() { _ = sessionB.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := sessionB.Exec(ctx,
			`INSERT INTO memberships (id, tenant_id, project_id, principal_id, roles, version, created_at)
			 VALUES ('membership-gap-b', 'tenant-1', NULL, $1, '["engineer"]', 7, $2)`,
			string(principal), pgtest.FixtureTime); err != nil {
			t.Fatalf("competing insert: %v", err)
		}
		out := startMembershipGrant(ctx, adapter, scope, principal, 0, 1)
		blockedPID := waitForBlockedMembershipWriter(t, shard)
		requireHeldAdvisoryLock(t, ctx, shard, blockedPID)
		if err := sessionB.Commit(ctx); err != nil {
			t.Fatalf("competing commit: %v", err)
		}
		outcome := awaitMembershipGrant(t, out)
		if class, ok := fault.ClassOf(outcome.err); !ok || class != fault.VersionConflict {
			t.Fatalf("grant error = %v; want version_conflict after the unique-index wait", outcome.err)
		}
		shard.RequireGoldenRow(t, ctx, runtime, scope, "memberships", map[string]any{"id": "membership-gap-b"}, map[string]any{
			"id": "membership-gap-b", "tenant_id": "tenant-1", "project_id": pgtest.NullValue,
			"principal_id": string(principal), "roles": `["engineer"]`,
			"version": int64(7), "created_at": pgtest.FixtureTime,
		})
		shard.RequireRowAbsent(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-gap-a"})
		shard.RequireRowAbsent(t, ctx, runtime, scope, "audit", map[string]any{"id": "audit-gap-a"})
		shard.RequireRowAbsent(t, ctx, runtime, scope, "events", map[string]any{"id": "event-gap-a"})
		observer.Finish(map[string]int{"memberships": 1})
	})

	t.Run("competing insert rolls back", func(t *testing.T) {
		shard := migratedShard(t)
		shard.SeedScope(ctx, "tenant-1", "project-1")
		runtime := shard.Runtime(6)
		defer runtime.Close()
		adapter, err := identityunits.New(identityunits.Config{
			Executor: storepg.New(runtime), Scanner: storepg.New(runtime),
			Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
		})
		if err != nil {
			t.Fatalf("identity.New: %v", err)
		}
		scope := pgtest.Scope("tenant-1", "project-1")
		principal := id.PrincipalID("gap-rollback")
		observer := shard.ObserveUnit(t, "membership", "first-grant.read-committed-rollback")
		sessionB, err := shard.Admin().Begin(ctx)
		if err != nil {
			t.Fatalf("begin competing session: %v", err)
		}
		// A failing assertion between the insert and the explicit rollback must
		// not leave the competing session open: the blocked grant would never
		// finish and the pool close would hang instead of failing the test.
		defer func() { _ = sessionB.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := sessionB.Exec(ctx,
			`INSERT INTO memberships (id, tenant_id, project_id, principal_id, roles, version, created_at)
			 VALUES ('membership-gap-b', 'tenant-1', 'project-1', $1, '["engineer"]', 7, $2)`,
			string(principal), pgtest.FixtureTime); err != nil {
			t.Fatalf("competing insert: %v", err)
		}
		out := startMembershipGrant(ctx, adapter, scope, principal, 0, 1)
		blockedPID := waitForBlockedMembershipWriter(t, shard)
		requireHeldAdvisoryLock(t, ctx, shard, blockedPID)
		if err := sessionB.Rollback(ctx); err != nil {
			t.Fatalf("competing rollback: %v", err)
		}
		outcome := awaitMembershipGrant(t, out)
		if outcome.err != nil {
			t.Fatalf("grant error = %v; want the committed insert after rollback", outcome.err)
		}
		shard.RequireGoldenRow(t, ctx, runtime, scope, "memberships", map[string]any{"principal_id": string(principal)}, map[string]any{
			// The adapter's id source is fresh in this subtest and the unit
			// draws its row id exactly once.
			"id": "row-1", "tenant_id": "tenant-1", "project_id": "project-1",
			"principal_id": string(principal), "roles": `["engineer"]`,
			"version": int64(1), "created_at": pgtest.FixtureTime,
		})
		shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-gap-a"}, map[string]any{
			"id": "receipt-gap-a", "tenant_id": "tenant-1", "project_id": "project-1",
			"principal_id": "principal-admin", "operation": "membership.grant", "caller_key": "gap-a",
			"content_sha256": pgtest.FingerprintBytes("gap-a"),
			"result":         pgtest.GoldenJSONOf(rawJSON([]byte(`{"version":1}`))),
			"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
			"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
		})
		shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "audit-gap-a"}, map[string]any{
			"id": "audit-gap-a", "tenant_id": "tenant-1", "project_id": "project-1",
			"actor": "principal-admin", "operation": "membership.grant", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
		})
		shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-gap-a"}, map[string]any{
			"id": "event-gap-a", "tenant_id": "tenant-1", "project_id": "project-1",
			"type": "config.changed", "report_type": pgtest.NullValue,
			"subject_id": string(principal), "subject_version": int64(1),
			"source_kind": "shell", "source_principal": "principal-admin",
			"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
			"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
			"publish_sequence": pgtest.GoldenDBSequence(),
		})
		observer.Finish(map[string]int{"memberships": 1, "receipts": 1, "audit": 1, "events": 1})
	})

	t.Run("present row version pin", func(t *testing.T) {
		shard := migratedShard(t)
		shard.SeedScope(ctx, "tenant-1", "project-1")
		shard.SeedMembership(ctx, pgtest.TenantScope("tenant-1"), "membership-gap", "gap-pinned", 1, string(tenancy.RoleEngineer))
		runtime := shard.Runtime(6)
		defer runtime.Close()
		adapter, err := identityunits.New(identityunits.Config{
			Executor: storepg.New(runtime), Scanner: storepg.New(runtime),
			Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
		})
		if err != nil {
			t.Fatalf("identity.New: %v", err)
		}
		scope := pgtest.TenantScope("tenant-1")
		principal := id.PrincipalID("gap-pinned")
		observer := shard.ObserveUnit(t, "membership", "grant.read-committed-version")
		sessionB, err := shard.Admin().Begin(ctx)
		if err != nil {
			t.Fatalf("begin competing session: %v", err)
		}
		defer func() { _ = sessionB.Rollback(context.WithoutCancel(ctx)) }()
		affected, err := sessionB.Exec(ctx,
			`UPDATE memberships SET version = 2 WHERE id = 'membership-gap'`)
		if err != nil || affected.RowsAffected != 1 {
			t.Fatalf("competing update: rows=%d err=%v", affected.RowsAffected, err)
		}
		out := startMembershipGrant(ctx, adapter, scope, principal, 1, 2)
		waitForBlockedMembershipWriter(t, shard)
		if err := sessionB.Commit(ctx); err != nil {
			t.Fatalf("competing commit: %v", err)
		}
		outcome := awaitMembershipGrant(t, out)
		if class, ok := fault.ClassOf(outcome.err); !ok || class != fault.VersionConflict {
			t.Fatalf("grant error = %v; want version_conflict after the row-lock wait", outcome.err)
		}
		shard.RequireGoldenRow(t, ctx, runtime, scope, "memberships", map[string]any{"id": "membership-gap"}, map[string]any{
			"id": "membership-gap", "tenant_id": "tenant-1", "project_id": pgtest.NullValue,
			"principal_id": string(principal), "roles": `["engineer"]`,
			"version": int64(2), "created_at": pgtest.FixtureTime,
		})
		shard.RequireRowAbsent(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-gap-a"})
		shard.RequireRowAbsent(t, ctx, runtime, scope, "audit", map[string]any{"id": "audit-gap-a"})
		shard.RequireRowAbsent(t, ctx, runtime, scope, "events", map[string]any{"id": "event-gap-a"})
		observer.Finish(map[string]int{"memberships": 1})
	})
}

// TestMembershipGuardWithoutUniqueIndexOnPostgreSQL isolates the absence-guard
// SELECT layer of the first-grant guard. With the unique constraint in
// place a missing or no-op guard is masked: the INSERT still hits the unique
// index and maps to version_conflict. Here a committed tenant-wide row already
// owns the key and nothing but the guard SELECT can refuse the grant.
func TestMembershipGuardWithoutUniqueIndexOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(6)
	defer runtime.Close()
	adapter, err := identityunits.New(identityunits.Config{
		Executor: storepg.New(runtime), Scanner: storepg.New(runtime),
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	// The DROP CONSTRAINT is isolated to this throwaway shard, which owns its
	// own container and database; the declared catalog is proven intact by the
	// schema-facts test. The constraint name is the one the catalog declares
	// for the memberships unique key (tenant_id, project_id, principal_id).
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`ALTER TABLE memberships DROP CONSTRAINT memberships_scope_principal`); err != nil {
		t.Fatalf("drop memberships unique constraint: %v%s", err, pgtest.CauseText(err))
	}
	constraints, err := pgtest.ScalarString(ctx, shard.Admin(),
		`SELECT count(*)::text FROM pg_constraint WHERE conname = 'memberships_scope_principal'`)
	if err != nil || constraints != "0" {
		t.Fatalf("memberships_scope_principal constraints after drop = %q, err=%v; want 0", constraints, err)
	}
	uniqueKeyIndexes, err := pgtest.ScalarString(ctx, shard.Admin(),
		`SELECT count(*)::text FROM pg_indexes
		 WHERE tablename = 'memberships' AND indexdef LIKE 'CREATE UNIQUE%' AND indexdef LIKE '%principal_id%'`)
	if err != nil || uniqueKeyIndexes != "0" {
		t.Fatalf("unique indexes over principal_id after drop = %q, err=%v; want 0", uniqueKeyIndexes, err)
	}

	scope := pgtest.TenantScope("tenant-1")
	principal := id.PrincipalID("gap-guard")
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO memberships (id, tenant_id, project_id, principal_id, roles, version, created_at)
		 VALUES ('membership-guard-b', 'tenant-1', NULL, $1, '["engineer"]', 7, $2)`,
		string(principal), pgtest.FixtureTime); err != nil {
		t.Fatalf("committed competing insert: %v", err)
	}
	observer := shard.ObserveUnit(t, "membership", "first-grant.guard-only")
	outcome := awaitMembershipGrant(t, startMembershipGrant(ctx, adapter, scope, principal, 0, 1))
	if class, ok := fault.ClassOf(outcome.err); !ok || class != fault.VersionConflict {
		t.Fatalf("grant error = %v; want version_conflict from the absence guard alone", outcome.err)
	}
	rows, err := pgtest.ScalarInt(ctx, shard.Admin(),
		`SELECT count(*) FROM memberships
		 WHERE tenant_id = 'tenant-1' AND project_id IS NULL AND principal_id = $1`, string(principal))
	if err != nil || rows != 1 {
		t.Fatalf("memberships rows for the guarded key = %d, err=%v; want exactly the committed one", rows, err)
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "memberships", map[string]any{"principal_id": string(principal)}, map[string]any{
		"id": "membership-guard-b", "tenant_id": "tenant-1", "project_id": pgtest.NullValue,
		"principal_id": string(principal), "roles": `["engineer"]`,
		"version": int64(7), "created_at": pgtest.FixtureTime,
	})
	shard.RequireRowAbsent(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-gap-a"})
	shard.RequireRowAbsent(t, ctx, runtime, scope, "audit", map[string]any{"id": "audit-gap-a"})
	shard.RequireRowAbsent(t, ctx, runtime, scope, "events", map[string]any{"id": "event-gap-a"})
	observer.Finish(map[string]int{})
}

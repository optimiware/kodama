//go:build component

package postgrescomponent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/kv"
	"github.com/optimiware/kodema/internal/shell/store"
	storepg "github.com/optimiware/kodema/internal/shell/store/postgres"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	pgtest "github.com/optimiware/kodema/test/support/infrastructure/postgres"
)

// TestMigration0009KVStoreUpgradeRLSAndRoundTripOnPostgreSQL verifies:
//  1. Incremental upgrade from 0001..0008 to 0009_kv_store.sql succeeds cleanly
//     and is idempotent on re-run.
//  2. Both UTF-8 and raw binary payloads round-trip cleanly through kv.Service
//     over PostgreSQL's non-owner runtime role under forced RLS.
//  3. Re-putting identical bytes in the same project is idempotent (Existed=true).
//  4. Strict tenant/project RLS isolation: Project B and Tenant B cannot read
//     Project A's KV key, and storing identical bytes in Project B creates an
//     independent row (Existed=false).
//  5. Immutability at the PostgreSQL role layer: kodema_runtime cannot UPDATE
//     or DELETE rows in kv_objects (SQLSTATE 42501 insufficient_privilege).
//  6. Concurrent identical kv.put calls converge cleanly on PostgreSQL:
//     0 errors, exactly 1 Existed=false, and N-1 Existed=true preserving the
//     winner's metadata.
func TestMigration0009KVStoreUpgradeRLSAndRoundTripOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	runner := pgtest.NewMigrationRunner(shard)

	// 1. Apply 0001..0008 first, then upgrade incrementally to 0009_kv_store.sql.
	pre, err := runner.Apply(ctx, 8)
	if err != nil {
		t.Fatalf("apply 0001..0008: %v", err)
	}
	if len(pre) != 8 || pre[7].Name != "0008_task_review_lifecycle.sql" {
		t.Fatalf("pre migrations = %d ending at %q; want 8 ending at 0008_task_review_lifecycle.sql", len(pre), pre[len(pre)-1].Name)
	}
	upgraded, err := runner.Apply(ctx, 0)
	if err != nil {
		t.Fatalf("apply incremental 0009_kv_store.sql: %v", err)
	}
	if len(upgraded) != 1 || upgraded[0].Name != "0009_kv_store.sql" {
		t.Fatalf("upgraded = %+v; want [0009_kv_store.sql]", upgraded)
	}
	again, err := runner.Apply(ctx, 0)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-run after 0009 = %d (err=%v); want 0", len(again), err)
	}

	// 2. Seed two tenants and two projects under tenant-1.
	scopeA := pgtest.Scope("tenant-1", "project-1")
	scopeB := pgtest.Scope("tenant-1", "project-2")
	scopeOtherTenant := pgtest.Scope("tenant-2", "project-t2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedProject(ctx, "tenant-1", "project-2")
	shard.SeedScope(ctx, "tenant-2", "project-t2")

	now := time.Now().UTC().Truncate(time.Microsecond)
	seedHuman := func(scope tenancy.Scope, rowID string, principalID id.PrincipalID, roles ...tenancy.Role) identity.Principal {
		rawRoles, _ := json.Marshal(roles)
		if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
			`INSERT INTO memberships (id, tenant_id, project_id, principal_id, roles, version, created_at)
			 VALUES ($1, $2, $3, $4, $5::jsonb, 1, $6)`,
			rowID, string(scope.TenantID), string(scope.ProjectID), string(principalID), string(rawRoles), now); err != nil {
			t.Fatalf("seed membership %s: %v", principalID, err)
		}
		return identity.Principal{
			ID:      principalID,
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   roles,
			Enabled: true,
			Version: 1,
		}
	}

	engA := seedHuman(scopeA, "mem-kv-a", "human-kv-a", tenancy.RoleEngineer)
	engB := seedHuman(scopeB, "mem-kv-b", "human-kv-b", tenancy.RoleEngineer)
	engT2 := seedHuman(scopeOtherTenant, "mem-kv-t2", "human-kv-t2", tenancy.RoleEngineer)

	runtime := shard.Runtime(16)
	t.Cleanup(runtime.Close)
	pgStore := storepg.New(runtime)
	ids := &integrationIDs{}
	kvSvc, err := kv.NewService(kv.Config{
		Clock:    integrationClock{now: now},
		IDs:      ids,
		Executor: pgStore,
		Scanner:  pgStore,
	})
	if err != nil {
		t.Fatalf("kv.NewService: %v", err)
	}

	// 3. UTF-8 and binary round-trips + idempotent re-put.
	utf8Payload := []byte("# Review Notes\n\nAll checks passed.\n")
	binaryPayload := []byte{0x00, 0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe}

	resUTF8, err := kvSvc.Put(ctx, kv.PutCommand{
		Scope:       scopeA,
		Actor:       engA,
		Data:        utf8Payload,
		ContentType: "text/markdown; charset=utf-8",
	})
	if err != nil {
		t.Fatalf("kv.Put UTF-8 on Postgres: %v%s", err, pgtest.CauseText(err))
	}
	if resUTF8.Existed || resUTF8.Metadata.Key != kv.KeyFor(utf8Payload) {
		t.Fatalf("kv.Put UTF-8 result = %+v", resUTF8)
	}

	resUTF8Again, err := kvSvc.Put(ctx, kv.PutCommand{
		Scope:       scopeA,
		Actor:       engA,
		Data:        utf8Payload,
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("idempotent kv.Put on Postgres: %v%s", err, pgtest.CauseText(err))
	}
	if !resUTF8Again.Existed || resUTF8Again.Metadata.ContentType != "text/markdown; charset=utf-8" {
		t.Fatalf("idempotent kv.Put = %+v; want existed=true preserving original content_type", resUTF8Again)
	}

	resBin, err := kvSvc.Put(ctx, kv.PutCommand{
		Scope:       scopeA,
		Actor:       engA,
		Data:        binaryPayload,
		ContentType: "image/png",
	})
	if err != nil {
		t.Fatalf("kv.Put binary on Postgres: %v%s", err, pgtest.CauseText(err))
	}
	gotBin, err := kvSvc.Get(ctx, kv.GetCommand{Scope: scopeA, Actor: engA, Key: resBin.Metadata.Key})
	if err != nil {
		t.Fatalf("kv.Get binary on Postgres: %v%s", err, pgtest.CauseText(err))
	}
	if !bytes.Equal(gotBin.Data, binaryPayload) {
		t.Fatalf("binary round-trip mismatch: got %x; want %x", gotBin.Data, binaryPayload)
	}

	// 4. RLS isolation: Project B and Tenant B cannot read Project A's key.
	if _, err := kvSvc.Get(ctx, kv.GetCommand{Scope: scopeB, Actor: engB, Key: resUTF8.Metadata.Key}); err == nil {
		t.Fatal("Project B read Project A's KV key on Postgres")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("Project B kv.Get class = %v; want not_found", class)
	}
	if _, err := kvSvc.Get(ctx, kv.GetCommand{Scope: scopeOtherTenant, Actor: engT2, Key: resUTF8.Metadata.Key}); err == nil {
		t.Fatal("Tenant B read Tenant A's KV key on Postgres")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("Tenant B kv.Get class = %v; want not_found", class)
	}

	// Identical content in Project B creates an independent row (Existed=false).
	resProjB, err := kvSvc.Put(ctx, kv.PutCommand{
		Scope: scopeB,
		Actor: engB,
		Data:  utf8Payload,
	})
	if err != nil {
		t.Fatalf("kv.Put identical bytes in Project B: %v%s", err, pgtest.CauseText(err))
	}
	if resProjB.Existed {
		t.Fatal("Project B Put reported Existed=true for Project A's key")
	}

	// 5. Runtime role immutability: UPDATE and DELETE on kv_objects are refused by PostgreSQL privileges (42501).
	tx, err := pgStore.Begin(ctx, scopeA)
	if err != nil {
		t.Fatalf("pgStore.Begin for UPDATE check: %v", err)
	}
	_, updateErr := tx.Exec(ctx, store.Statement{
		SQL:  "UPDATE kv_objects SET content_type = 'text/plain' WHERE key = $1",
		Args: []any{string(resUTF8.Metadata.Key)},
	})
	_ = tx.Rollback(ctx)
	pgtest.RequireSQLState(t, updateErr, "42501", "kodema_runtime cannot UPDATE kv_objects")

	tx, err = pgStore.Begin(ctx, scopeA)
	if err != nil {
		t.Fatalf("pgStore.Begin for DELETE check: %v", err)
	}
	_, deleteErr := tx.Exec(ctx, store.Statement{
		SQL:  "DELETE FROM kv_objects WHERE key = $1",
		Args: []any{string(resUTF8.Metadata.Key)},
	})
	_ = tx.Rollback(ctx)
	pgtest.RequireSQLState(t, deleteErr, "42501", "kodema_runtime cannot DELETE from kv_objects")

	// 6. Concurrent identical kv.put race on PostgreSQL: N concurrent goroutines
	// putting identical bytes must produce 0 errors, exactly 1 Existed=false,
	// and N-1 Existed=true, all preserving the single winner's metadata.
	const workers = 12
	raceActors := make([]identity.Principal, workers)
	for i := range workers {
		raceActors[i] = seedHuman(
			scopeA,
			fmt.Sprintf("mem-kv-race-%d", i),
			id.PrincipalID(fmt.Sprintf("human-kv-race-%d", i)),
			tenancy.RoleEngineer,
		)
	}
	racePayload := []byte("concurrent identical kv.put race payload on PostgreSQL")
	wantRaceKey := kv.KeyFor(racePayload)

	type raceOutcome struct {
		res kv.PutResult
		err error
	}
	outcomes := make([]raceOutcome, workers)
	ready := make(chan struct{}, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			res, err := kvSvc.Put(ctx, kv.PutCommand{
				Scope:       scopeA,
				Actor:       raceActors[idx],
				Data:        racePayload,
				ContentType: fmt.Sprintf("text/plain; charset=utf-8; worker=%d", idx),
			})
			outcomes[idx] = raceOutcome{res: res, err: err}
		}(i)
	}
	for range workers {
		<-ready
	}
	close(start)
	wg.Wait()

	var created, existed int
	var winner kv.Metadata
	for i, out := range outcomes {
		if out.err != nil {
			t.Fatalf("concurrent Postgres kv.Put worker %d failed: %v%s", i, out.err, pgtest.CauseText(out.err))
		}
		if out.res.Metadata.Key != wantRaceKey {
			t.Fatalf("worker %d key = %s; want %s", i, out.res.Metadata.Key, wantRaceKey)
		}
		if out.res.Existed {
			existed++
		} else {
			created++
			winner = out.res.Metadata
		}
	}
	if created != 1 || existed != workers-1 {
		t.Fatalf("concurrent Postgres kv.Put outcomes: existed=false count=%d, existed=true count=%d; want 1 and %d",
			created, existed, workers-1)
	}
	for i, out := range outcomes {
		if out.res.Metadata.CreatedBy != winner.CreatedBy ||
			out.res.Metadata.ContentType != winner.ContentType ||
			!out.res.Metadata.CreatedAt.Equal(winner.CreatedAt) {
			t.Fatalf("worker %d metadata = %+v; want winner %+v", i, out.res.Metadata, winner)
		}
	}
	gotRace, err := kvSvc.Get(ctx, kv.GetCommand{Scope: scopeA, Actor: engA, Key: wantRaceKey})
	if err != nil {
		t.Fatalf("kv.Get after concurrent Postgres puts: %v%s", err, pgtest.CauseText(err))
	}
	if !bytes.Equal(gotRace.Data, racePayload) {
		t.Fatalf("kv.Get payload mismatch after concurrent Postgres puts")
	}
}

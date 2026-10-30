package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	"github.com/optimiware/kodama/internal/state"
	statepg "github.com/optimiware/kodama/internal/state/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

func TestPostgresStateLifecycle(t *testing.T) {
	ctx := context.Background()
	db := pgtest.New(t)
	owner, err := storage.Open(ctx, storage.Config{URL: db.MigrationURL})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	manifest, err := storage.NewManifest(storage.BaseContribution(), statepg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatal(err)
	}
	pool, err := storage.Open(ctx, storage.Config{URL: db.RuntimeURL})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, err := statepg.New(statepg.Config{DB: pool})
	if err != nil {
		t.Fatal(err)
	}
	scope := state.Scope{TenantID: "t_state", ProjectID: "p_state"}
	req := statev1.WriteRequest{Value: json.RawMessage(`{"release":"v1"}`)}
	entry, err := store.Create(ctx, scope, "deploy", "resources/app", "att_1", req)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Generation < 1 || entry.Revision != 1 {
		t.Fatalf("entry = %+v", entry)
	}
	other := state.Scope{TenantID: "t_other", ProjectID: "p_other"}
	if _, err := store.Get(ctx, other, "deploy", "resources/app"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("cross-tenant read = %v", err)
	}
	stale := entry.ETag()
	updated, err := store.Update(ctx, scope, "deploy", "resources/app", "att_1", stale, statev1.WriteRequest{Value: json.RawMessage(`{"release":"v2"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 {
		t.Fatalf("updated = %+v", updated)
	}
	if _, err := store.Update(ctx, scope, "deploy", "resources/app", "att_1", stale, req); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale update = %v", err)
	}
	items, next, err := store.List(ctx, scope, "deploy", "resources/", "", 1)
	if err != nil || len(items) != 1 || next != "" {
		t.Fatalf("list = %+v next=%q err=%v", items, next, err)
	}
	if err := store.Delete(ctx, scope, "deploy", "resources/app", "att_1", updated.ETag()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, scope, "deploy", "resources/app"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("get deleted = %v", err)
	}
}

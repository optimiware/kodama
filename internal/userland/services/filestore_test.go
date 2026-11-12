package services_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

func fileStoreKey() services.MergeKey {
	return services.MergeKey{Repository: "shop/web", Ref: "refs/heads/release/2026"}
}

func TestFileCoordinationStoreReopenPreservesRecord(t *testing.T) {
	directory := t.TempDir()
	key := fileStoreKey()
	first, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}

	created, err := first.CreateClaim(context.Background(), testTenant, key, "merge-worker-1", "op-1", testNow.Add(time.Minute), testNow)
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	if created.Generation != 1 || created.State != services.EffectNotStarted || created.Owner != "merge-worker-1" {
		t.Fatalf("created record = %+v; want a generation-1 not_started claim", created)
	}

	// A new store instance over the same directory observes the record.
	second, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("second NewFileCoordinationStore: %v", err)
	}
	loaded, ok, err := second.Load(context.Background(), testTenant, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("a reopened store did not observe the durable record")
	}
	if loaded != created {
		t.Fatalf("loaded = %+v; want %+v", loaded, created)
	}

	next := loaded
	next.State = services.EffectApplied
	next.Result = services.MergeResult{
		Key:               key,
		State:             services.EffectApplied,
		ObservedBase:      "base-1111111",
		ObservedCandidate: "candidate-2222222",
		ObservedOutput:    "Merge made by the 'ort' strategy.",
	}
	next.UpdatedAt = testNow.Add(time.Second)
	if err := second.Transition(context.Background(), testTenant, loaded, next); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	reloaded, ok, err := first.Load(context.Background(), testTenant, key)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if !ok || reloaded != next {
		t.Fatalf("first store sees %+v; want %+v", reloaded, next)
	}
}

func TestFileCoordinationStoreIsolatesTenants(t *testing.T) {
	directory := t.TempDir()
	key := fileStoreKey()
	store, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	otherTenant := id.TenantID("tenant-b")

	first, err := store.CreateClaim(context.Background(), testTenant, key, "owner-a", "op-a", testNow.Add(time.Minute), testNow)
	if err != nil {
		t.Fatalf("tenant-a CreateClaim: %v", err)
	}
	second, err := store.CreateClaim(context.Background(), otherTenant, key, "owner-b", "op-b", testNow.Add(time.Minute), testNow)
	if err != nil {
		t.Fatalf("tenant-b CreateClaim: %v", err)
	}
	if first.Generation != 1 || second.Generation != 1 {
		t.Fatalf("generations = %d,%d; want independent generation 1 per tenant", first.Generation, second.Generation)
	}

	loadedA, _, err := store.Load(context.Background(), testTenant, key)
	if err != nil {
		t.Fatalf("tenant-a Load: %v", err)
	}
	loadedB, _, err := store.Load(context.Background(), otherTenant, key)
	if err != nil {
		t.Fatalf("tenant-b Load: %v", err)
	}
	if loadedA.Owner != "owner-a" || loadedB.Owner != "owner-b" {
		t.Fatalf("owners = %q,%q; want isolated per-tenant records", loadedA.Owner, loadedB.Owner)
	}

	// A transition in one tenant never changes the other.
	next := loadedB
	next.UpdatedAt = testNow.Add(time.Second)
	if err := store.Transition(context.Background(), otherTenant, loadedB, next); err != nil {
		t.Fatalf("tenant-b Transition: %v", err)
	}
	unchanged, _, err := store.Load(context.Background(), testTenant, key)
	if err != nil {
		t.Fatalf("tenant-a reload: %v", err)
	}
	if unchanged != loadedA {
		t.Fatalf("tenant-a record changed to %+v; want %+v", unchanged, loadedA)
	}
}

func TestFileCoordinationStoreRejectsStaleTransition(t *testing.T) {
	directory := t.TempDir()
	key := fileStoreKey()
	store, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	created, err := store.CreateClaim(context.Background(), testTenant, key, "merge-worker-1", "op-1", testNow.Add(time.Minute), testNow)
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}

	current := created
	current.UpdatedAt = testNow.Add(time.Second)
	if err := store.Transition(context.Background(), testTenant, created, current); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	advanced := current
	advanced.UpdatedAt = testNow.Add(2 * time.Second)
	if err := store.Transition(context.Background(), testTenant, current, advanced); err != nil {
		t.Fatalf("second Transition: %v", err)
	}

	type transitionCase struct {
		expected services.CoordinationRecord
		next     services.CoordinationRecord
	}
	changedGeneration := advanced
	changedGeneration.Generation = advanced.Generation + 5
	changedOwner := advanced
	changedOwner.Owner = "intruder"
	changedOperation := advanced
	changedOperation.OperationID = "op-2"
	changedKey := advanced
	changedKey.Key = services.MergeKey{Repository: "shop/other", Ref: "refs/heads/main"}

	cases := map[string]transitionCase{
		"stale expected":     {expected: created, next: current},
		"changed generation": {expected: advanced, next: changedGeneration},
		"changed owner":      {expected: advanced, next: changedOwner},
		"changed operation":  {expected: advanced, next: changedOperation},
		"changed key":        {expected: advanced, next: changedKey},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if err := store.Transition(context.Background(), testTenant, testCase.expected, testCase.next); !errors.Is(err, services.ErrStaleClaim) {
				t.Fatalf("Transition error = %v; want ErrStaleClaim", err)
			}
		})
	}

	loaded, _, err := store.Load(context.Background(), testTenant, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded != advanced {
		t.Fatalf("stored record = %+v; want the last successful transition %+v", loaded, advanced)
	}
}

func TestFileCoordinationStoreCreateClaimIsExclusiveAcrossInstances(t *testing.T) {
	directory := t.TempDir()
	key := fileStoreKey()
	first, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("first NewFileCoordinationStore: %v", err)
	}
	second, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("second NewFileCoordinationStore: %v", err)
	}

	start := make(chan struct{})
	errs := make([]error, 2)
	owners := []string{"owner-a", "owner-b"}
	var wait sync.WaitGroup
	for index, store := range []*services.FileCoordinationStore{first, second} {
		wait.Add(1)
		go func(index int, store *services.FileCoordinationStore) {
			defer wait.Done()
			<-start
			_, errs[index] = store.CreateClaim(context.Background(), testTenant, key, owners[index], "op-1", testNow.Add(time.Minute), testNow)
		}(index, store)
	}
	close(start)
	wait.Wait()

	winners, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, services.ErrClaimed):
			refused++
		default:
			t.Fatalf("unexpected CreateClaim error: %v", err)
		}
	}
	if winners != 1 || refused != 1 {
		t.Fatalf("claims: %d winners, %d refused; want one exclusive owner across store instances", winners, refused)
	}

	loaded, ok, err := first.Load(context.Background(), testTenant, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("no record after the exclusive claim")
	}
	if loaded.Owner != "owner-a" && loaded.Owner != "owner-b" {
		t.Fatalf("stored owner = %q; want one of the competing owners", loaded.Owner)
	}
}

func TestFileCoordinationStoreRejectsInvalidInput(t *testing.T) {
	if _, err := services.NewFileCoordinationStore(""); err == nil {
		t.Fatal("NewFileCoordinationStore accepted an empty directory")
	}

	directory := t.TempDir()
	store, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	key := fileStoreKey()

	if _, err := store.CreateClaim(context.Background(), id.TenantID("../escape"), key, "owner", "op-1", testNow, testNow); err == nil {
		t.Fatal("CreateClaim accepted an invalid tenant")
	}
	if _, err := store.CreateClaim(context.Background(), testTenant, services.MergeKey{Repository: "../etc", Ref: "refs/heads/main"}, "owner", "op-1", testNow, testNow); err == nil {
		t.Fatal("CreateClaim accepted an invalid repository")
	}
	if _, _, err := store.Load(context.Background(), testTenant, services.MergeKey{}); err == nil {
		t.Fatal("Load accepted an invalid key")
	}

	records := 0
	if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			records++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk store directory: %v", err)
	}
	if records != 0 {
		t.Fatalf("store directory contains %d files after only invalid inputs", records)
	}
}

func TestFileCoordinationStoreCreatesMissingDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "nested", "coordination")
	store, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stat created directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", directory)
	}
	if _, err := store.CreateClaim(context.Background(), testTenant, fileStoreKey(), "owner", "op-1", testNow.Add(time.Minute), testNow); err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
}

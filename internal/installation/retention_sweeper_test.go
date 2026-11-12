package installation

// Retention-sweeper tests. They drive the S10 §5 policy through the
// same MayDelete rule the product uses, with a deterministic store. The
// component test in test/component/postgres proves the PostgreSQL adapter; this
// test proves the decision loop is conservative, complete and idempotent.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

var retentionSweepNow = time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)

type fakeRetentionStore struct {
	candidates map[string][]RetentionCandidate
	queryErr   map[string]error
	deleteErr  map[string]error
	deleted    []string
	queries    []string
}

func newFakeRetentionStore() *fakeRetentionStore {
	return &fakeRetentionStore{
		candidates: make(map[string][]RetentionCandidate),
		queryErr:   make(map[string]error),
		deleteErr:  make(map[string]error),
	}
}

func retentionKey(scope RetentionScope, class RetentionClass) string {
	return scope.TenantID + "\x00" + scope.ProjectID + "\x00" + string(class)
}

func (store *fakeRetentionStore) Candidates(_ context.Context, scope RetentionScope, class RetentionClass, _ time.Time) ([]RetentionCandidate, error) {
	key := retentionKey(scope, class)
	store.queries = append(store.queries, key)
	if err := store.queryErr[key]; err != nil {
		return nil, err
	}
	return append([]RetentionCandidate(nil), store.candidates[key]...), nil
}

func (store *fakeRetentionStore) Delete(_ context.Context, scope RetentionScope, class RetentionClass, candidate RetentionCandidate, _ time.Time) (bool, error) {
	key := retentionKey(scope, class)
	if err := store.deleteErr[key]; err != nil {
		return false, err
	}
	remaining := make([]RetentionCandidate, 0, len(store.candidates[key]))
	for _, existing := range store.candidates[key] {
		if existing.ID == candidate.ID {
			continue
		}
		remaining = append(remaining, existing)
	}
	store.candidates[key] = remaining
	store.deleted = append(store.deleted, candidate.ID)
	return true, nil
}

func sweeperTestStore() (*fakeRetentionStore, []RetentionScope) {
	scope := RetentionScope{TenantID: "tenant-1", ProjectID: "project-1"}
	store := newFakeRetentionStore()
	store.candidates[retentionKey(scope, RetentionLogs)] = []RetentionCandidate{
		{ID: "log-expired", CreatedAt: retentionSweepNow.Add(-31 * 24 * time.Hour)},
		{ID: "log-live", CreatedAt: retentionSweepNow.Add(-29 * 24 * time.Hour)},
	}
	store.candidates[retentionKey(scope, RetentionEvidence)] = []RetentionCandidate{
		{ID: "evidence-expired", CreatedAt: retentionSweepNow.Add(-91 * 24 * time.Hour)},
		{ID: "evidence-boundary", CreatedAt: retentionSweepNow.Add(-90 * 24 * time.Hour)},
		{ID: "evidence-live", CreatedAt: retentionSweepNow.Add(-89 * 24 * time.Hour)},
		{ID: "evidence-active", CreatedAt: retentionSweepNow.Add(-400 * 24 * time.Hour), ActiveReferences: true},
		{ID: "evidence-unknown-time"},
	}
	store.candidates[retentionKey(scope, RetentionAudit)] = []RetentionCandidate{
		{ID: "audit-expired", CreatedAt: retentionSweepNow.Add(-366 * 24 * time.Hour)},
		{ID: "audit-boundary", CreatedAt: retentionSweepNow.Add(-365 * 24 * time.Hour)},
		{ID: "audit-live", CreatedAt: retentionSweepNow.Add(-364 * 24 * time.Hour)},
	}
	store.candidates[retentionKey(scope, RetentionReceipts)] = []RetentionCandidate{
		{ID: "receipt-ancient", CreatedAt: retentionSweepNow.Add(-10 * 365 * 24 * time.Hour)},
	}
	store.candidates[retentionKey(scope, RetentionRawContent)] = []RetentionCandidate{
		{ID: "raw-content-expired", CreatedAt: retentionSweepNow.Add(-400 * 24 * time.Hour)},
	}
	return store, []RetentionScope{scope}
}

func TestRetentionSweeperAppliesThePolicyAndIsIdempotent(t *testing.T) {
	store, scopes := sweeperTestStore()
	sweeper, err := NewRetentionSweeper(RetentionPolicy{}, store)
	if err != nil {
		t.Fatalf("NewRetentionSweeper: %v", err)
	}

	result, err := sweeper.Sweep(context.Background(), scopes, retentionSweepNow)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	wantDeleted := map[RetentionClass]int{
		RetentionLogs:     1,
		RetentionEvidence: 1,
		RetentionAudit:    1,
	}
	for class, want := range wantDeleted {
		if got := result.DeletedByClass[class]; got != want {
			t.Errorf("deleted %s = %d, want %d", class, got, want)
		}
	}
	if got := result.KeptByClass[RetentionEvidence]; got != 4 {
		t.Errorf("kept evidence = %d, want 4 (boundary, live, active, unknown time)", got)
	}
	if got := result.KeptByClass[RetentionAudit]; got != 2 {
		t.Errorf("kept audit = %d, want 2 (boundary, live)", got)
	}
	if got := result.DeletedByClass[RetentionReceipts]; got != 0 {
		t.Errorf("deleted receipts = %d, want 0", got)
	}
	if got := result.DeletedByClass[RetentionRawContent]; got != 0 {
		t.Errorf("deleted raw content = %d, want 0 while the opt-in is off", got)
	}

	for _, id := range []string{
		"log-expired", "evidence-expired", "audit-expired",
	} {
		if !slices.Contains(store.deleted, id) {
			t.Errorf("expired row %q was not deleted", id)
		}
	}
	for _, id := range []string{
		"log-live", "evidence-boundary", "evidence-live", "evidence-active",
		"evidence-unknown-time", "audit-boundary", "audit-live", "receipt-ancient",
		"raw-content-expired",
	} {
		if slices.Contains(store.deleted, id) {
			t.Errorf("row %q was deleted but must be kept", id)
		}
	}

	// A second sweep over the same clock must delete nothing: the first sweep
	// already removed exactly the expired rows and the store is authoritative.
	second, err := sweeper.Sweep(context.Background(), scopes, retentionSweepNow)
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	for _, class := range RetentionClasses() {
		if got := second.DeletedByClass[class]; got != 0 {
			t.Errorf("second sweep deleted %d %s rows; want 0", got, class)
		}
	}
}

func TestRetentionSweeperIsConservativeOnStoreFailure(t *testing.T) {
	store, scopes := sweeperTestStore()
	scope := scopes[0]
	store.queryErr[retentionKey(scope, RetentionEvidence)] = errors.New("evidence scan unavailable")
	sweeper, err := NewRetentionSweeper(RetentionPolicy{}, store)
	if err != nil {
		t.Fatalf("NewRetentionSweeper: %v", err)
	}
	result, err := sweeper.Sweep(context.Background(), scopes, retentionSweepNow)
	if err == nil {
		t.Fatal("Sweep reported success although a candidate scan failed")
	}
	if got := result.DeletedByClass[RetentionEvidence]; got != 0 {
		t.Fatalf("deleted %d evidence rows after an uncertain scan; want 0", got)
	}
	// An uncertain class must not stop the other classes: their expiry is
	// proven independently.
	if got := result.DeletedByClass[RetentionAudit]; got != 1 {
		t.Errorf("audit deletions = %d, want 1 despite the evidence scan fault", got)
	}

	store, scopes = sweeperTestStore()
	store.deleteErr[retentionKey(scopes[0], RetentionAudit)] = errors.New("audit delete unavailable")
	sweeper, err = NewRetentionSweeper(RetentionPolicy{}, store)
	if err != nil {
		t.Fatalf("NewRetentionSweeper: %v", err)
	}
	result, err = sweeper.Sweep(context.Background(), scopes, retentionSweepNow)
	if err == nil {
		t.Fatal("Sweep reported success although a delete failed")
	}
	if got := result.DeletedByClass[RetentionAudit]; got != 0 {
		t.Errorf("audit deletions = %d, want 0 after a failed delete", got)
	}
	if slices.Contains(store.deleted, "audit-expired") {
		t.Error("an audit row was reported deleted although the store refused it")
	}
}

func TestRetentionSweeperRefusesAnUnusableClockOrScope(t *testing.T) {
	store, scopes := sweeperTestStore()
	sweeper, err := NewRetentionSweeper(RetentionPolicy{}, store)
	if err != nil {
		t.Fatalf("NewRetentionSweeper: %v", err)
	}
	if _, err := sweeper.Sweep(context.Background(), scopes, time.Time{}); err == nil {
		t.Fatal("Sweep accepted a zero clock")
	}
	if _, err := sweeper.Sweep(context.Background(), []RetentionScope{{TenantID: ""}}, retentionSweepNow); err == nil {
		t.Fatal("Sweep accepted a scope without a tenant")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("an invalid sweep deleted %v; want no deletion", store.deleted)
	}

	if _, err := NewRetentionSweeper(RetentionPolicy{}, nil); err == nil {
		t.Fatal("NewRetentionSweeper accepted a nil store")
	}
}

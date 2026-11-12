package restorequal

// Q03 retention tests. The drill uses the product's pure retention rule
// (internal/installation.RetentionPolicy) rather than a parallel copy.
//
// Sources:
//   - KODAMA.md line 517: default retention logs 30 days, ordinary evidence 90
//     days, audit metadata one year, extended by active references; operation
//     receipts remain until project deletion; raw model/tool content is off by
//     default and authorized opt-in starts at seven days.
//   - docs/specs/10-production.md §5 (Retention and measured targets).

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/userland/session"
)

// TestRetentionBoundaries pins the frozen lifetimes and their exact boundary.
func TestRetentionBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	policy := installation.RetentionPolicy{}

	t.Run("ordinary families use the frozen log evidence audit lifetimes", func(t *testing.T) {
		for _, testCase := range []struct {
			class    installation.RetentionClass
			duration time.Duration
		}{
			{class: installation.RetentionLogs, duration: 30 * 24 * time.Hour},
			{class: installation.RetentionEvidence, duration: 90 * 24 * time.Hour},
			{class: installation.RetentionAudit, duration: 365 * 24 * time.Hour},
		} {
			t.Run(string(testCase.class), func(t *testing.T) {
				window, err := policy.Window(testCase.class)
				if err != nil {
					t.Fatalf("Window: %v", err)
				}
				if window.Duration != testCase.duration {
					t.Fatalf("window = %v; want %v", window.Duration, testCase.duration)
				}
				if window.Disabled || window.UntilProjectDeletion {
					t.Fatalf("window = %+v; want a plain TTL", window)
				}

				atBoundary, err := policy.MayDelete(testCase.class, now.Add(-testCase.duration), now, false)
				if err != nil {
					t.Fatalf("MayDelete at boundary: %v", err)
				}
				if atBoundary.Delete {
					t.Fatalf("item exactly at the %s boundary was deleted; want kept", testCase.class)
				}

				justPast, err := policy.MayDelete(testCase.class, now.Add(-testCase.duration-time.Nanosecond), now, false)
				if err != nil {
					t.Fatalf("MayDelete just past: %v", err)
				}
				if !justPast.Delete {
					t.Fatalf("item one nanosecond past the %s boundary was kept; want deleted", testCase.class)
				}

				activeReference, err := policy.MayDelete(testCase.class, now.Add(-testCase.duration-time.Hour), now, true)
				if err != nil {
					t.Fatalf("MayDelete with an active reference: %v", err)
				}
				if activeReference.Delete {
					t.Fatalf("item with an active reference was deleted; want kept")
				}
				if !strings.Contains(activeReference.Reason, "reference") {
					t.Fatalf("active-reference reason = %q; want it to name references", activeReference.Reason)
				}
			})
		}
	})

	t.Run("receipts are kept until project deletion", func(t *testing.T) {
		window, err := policy.Window(installation.RetentionReceipts)
		if err != nil {
			t.Fatalf("Window(receipts): %v", err)
		}
		if !window.UntilProjectDeletion || window.Disabled || window.Duration != 0 {
			t.Fatalf("receipts window = %+v; want until project deletion", window)
		}
		decision, err := policy.MayDelete(
			installation.RetentionReceipts,
			now.Add(-100*365*24*time.Hour),
			now, false,
		)
		if err != nil {
			t.Fatalf("MayDelete(receipts): %v", err)
		}
		if decision.Delete {
			t.Fatal("an old receipt was deleted before project deletion")
		}
		if !strings.Contains(decision.Reason, "project deletion") {
			t.Fatalf("receipt reason = %q; want it to name project deletion", decision.Reason)
		}
	})

	t.Run("raw model and tool content is off by default", func(t *testing.T) {
		window, err := policy.Window(installation.RetentionRawContent)
		if err != nil {
			t.Fatalf("Window(raw content): %v", err)
		}
		if !window.Disabled {
			t.Fatalf("raw content window = %+v; want disabled by default", window)
		}
		decision, err := policy.MayDelete(
			installation.RetentionRawContent,
			now.Add(-100*365*24*time.Hour),
			now, false,
		)
		if err != nil {
			t.Fatalf("MayDelete(raw content): %v", err)
		}
		if decision.Delete {
			t.Fatal("raw content was deleted although retention is disabled; want kept/not collected")
		}
		if !strings.Contains(decision.Reason, "disabled") {
			t.Fatalf("raw-content reason = %q; want it to name the disabled family", decision.Reason)
		}
	})

	t.Run("authorized raw content opt-in starts at seven days", func(t *testing.T) {
		optIn := installation.RetentionPolicy{RawContentOptIn: true}
		window, err := optIn.Window(installation.RetentionRawContent)
		if err != nil {
			t.Fatalf("Window(raw content opt-in): %v", err)
		}
		if window.Disabled {
			t.Fatal("opted-in raw content is still disabled")
		}
		if window.Duration != 7*24*time.Hour {
			t.Fatalf("raw content opt-in window = %v; want 7 days", window.Duration)
		}

		atBoundary, err := optIn.MayDelete(
			installation.RetentionRawContent,
			now.Add(-7*24*time.Hour), now, false,
		)
		if err != nil {
			t.Fatalf("MayDelete at boundary: %v", err)
		}
		if atBoundary.Delete {
			t.Fatal("opted-in raw content exactly at seven days was deleted; want kept")
		}

		justPast, err := optIn.MayDelete(
			installation.RetentionRawContent,
			now.Add(-7*24*time.Hour-time.Nanosecond), now, false,
		)
		if err != nil {
			t.Fatalf("MayDelete just past: %v", err)
		}
		if !justPast.Delete {
			t.Fatal("opted-in raw content one nanosecond past seven days was kept; want deleted")
		}
	})
}

// TestRetentionSweeperIntegration drives the product sweeper, not a parallel
// copy, over one scope. The sweep must delete only the rows the frozen policy
// proves expired: an active reference extends the window, receipts and
// default-off raw content are not offered for deletion at all, and an
// authorized raw-content opt-in starts at seven days.
func TestRetentionSweeperIntegration(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	newStore := func() (*q03RetentionStore, []installation.RetentionScope) {
		store := &q03RetentionStore{
			candidates: map[installation.RetentionClass][]installation.RetentionCandidate{
				installation.RetentionLogs: {
					{ID: "log-expired", CreatedAt: now.Add(-31 * day)},
					{ID: "log-referenced", CreatedAt: now.Add(-31 * day), ActiveReferences: true},
				},
				installation.RetentionEvidence: {
					{ID: "evidence-expired", CreatedAt: now.Add(-91 * day)},
				},
				installation.RetentionAudit: {
					{ID: "audit-expired", CreatedAt: now.Add(-366 * day)},
				},
				installation.RetentionReceipts: {
					{ID: "receipt-old", CreatedAt: now.Add(-1000 * day)},
				},
				installation.RetentionRawContent: {
					{ID: "raw-old", CreatedAt: now.Add(-1000 * day)},
				},
			},
		}
		return store, []installation.RetentionScope{{TenantID: "tenant-q03", ProjectID: "project-q03"}}
	}

	t.Run("ordinary defaults keep receipts and default-off raw content", func(t *testing.T) {
		store, scopes := newStore()
		sweeper, err := installation.NewRetentionSweeper(installation.RetentionPolicy{}, store)
		if err != nil {
			t.Fatalf("NewRetentionSweeper: %v", err)
		}
		result, err := sweeper.Sweep(context.Background(), scopes, now)
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		sort.Strings(store.deleted)
		if want := []string{"audit:audit-expired", "evidence:evidence-expired", "logs:log-expired"}; !reflect.DeepEqual(store.deleted, want) {
			t.Fatalf("deleted = %v; want %v", store.deleted, want)
		}
		if store.queried(installation.RetentionReceipts) {
			t.Fatal("the sweeper queried receipt candidates; receipts stay until project deletion")
		}
		if store.queried(installation.RetentionRawContent) {
			t.Fatal("the sweeper queried raw content while retention is disabled")
		}
		if result.KeptByClass[installation.RetentionLogs] != 1 {
			t.Fatalf("kept logs = %d; want the actively referenced row kept", result.KeptByClass[installation.RetentionLogs])
		}
	})

	t.Run("authorized raw-content opt-in starts at seven days", func(t *testing.T) {
		store, scopes := newStore()
		store.candidates[installation.RetentionRawContent] = []installation.RetentionCandidate{
			{ID: "raw-expired", CreatedAt: now.Add(-8 * day)},
			{ID: "raw-current", CreatedAt: now.Add(-6 * day)},
			{ID: "raw-referenced", CreatedAt: now.Add(-30 * day), ActiveReferences: true},
		}
		sweeper, err := installation.NewRetentionSweeper(installation.RetentionPolicy{RawContentOptIn: true}, store)
		if err != nil {
			t.Fatalf("NewRetentionSweeper: %v", err)
		}
		if _, err := sweeper.Sweep(context.Background(), scopes, now); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		sort.Strings(store.deleted)
		want := []string{
			"audit:audit-expired",
			"evidence:evidence-expired",
			"logs:log-expired",
			"raw_content:raw-expired",
		}
		if !reflect.DeepEqual(store.deleted, want) {
			t.Fatalf("deleted = %v; want %v", store.deleted, want)
		}
	})
}

// TestSessionRawContentRetention pins the checkpoint owner's raw-content rule:
// off by default, seven-day authorized opt-in and an active reference that
// extends the deadline.
func TestSessionRawContentRetention(t *testing.T) {
	created := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	off := session.RetentionPolicy{}
	if off.RetainRaw(created, created, false) || off.RetainRaw(created, created, true) {
		t.Fatal("raw content was retained without an explicit opt-in")
	}

	on := session.RetentionPolicy{RawContent: true}
	if !on.RetainRaw(created.Add(7*24*time.Hour-time.Nanosecond), created, false) {
		t.Fatal("opted-in raw content was withheld before seven days")
	}
	if on.RetainRaw(created.Add(7*24*time.Hour), created, false) {
		t.Fatal("opted-in raw content was retained exactly at seven days")
	}
	if !on.RetainRaw(created.Add(30*24*time.Hour), created, true) {
		t.Fatal("an active checkpoint reference did not extend raw-content retention")
	}
}

// q03RetentionStore is an in-memory installation.RetentionStore that records
// queries and proven deletions.
type q03RetentionStore struct {
	candidates map[installation.RetentionClass][]installation.RetentionCandidate
	queries    []installation.RetentionClass
	deleted    []string
}

func (store *q03RetentionStore) Candidates(_ context.Context, _ installation.RetentionScope, class installation.RetentionClass, _ time.Time) ([]installation.RetentionCandidate, error) {
	store.queries = append(store.queries, class)
	return append([]installation.RetentionCandidate(nil), store.candidates[class]...), nil
}

func (store *q03RetentionStore) Delete(_ context.Context, _ installation.RetentionScope, class installation.RetentionClass, candidate installation.RetentionCandidate, _ time.Time) (bool, error) {
	store.deleted = append(store.deleted, string(class)+":"+candidate.ID)
	remaining := store.candidates[class][:0]
	for _, existing := range store.candidates[class] {
		if existing.ID != candidate.ID {
			remaining = append(remaining, existing)
		}
	}
	store.candidates[class] = remaining
	return true, nil
}

func (store *q03RetentionStore) queried(class installation.RetentionClass) bool {
	return slices.Contains(store.queries, class)
}

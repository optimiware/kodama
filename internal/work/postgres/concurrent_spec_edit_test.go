package postgres_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/work"
)

func str(s string) *string { return &s }

// Test_concurrent_spec_edit: two edits made against the same observed version
// yield exactly one accepted edit and one version conflict; only the winner
// increments the revisions and writes history, audit and outbox records.
// Real PostgreSQL. WM-02, WM-08, WM-A02.
func Test_concurrent_spec_edit(t *testing.T) {
	e := newEnv(t)
	it := e.create(scopeA, work.KindStory, "Resolve inventory exceptions")
	if it.Version != 1 || it.ContentRevision != 1 || it.State != work.StateBacklog {
		t.Fatalf("created %+v", it)
	}
	h0, a0, o0 := e.records(it)
	if h0 != 1 || a0 != 1 || o0 != 1 {
		t.Fatalf("creation records: history %d audit %d outbox %d", h0, a0, o0)
	}

	edit := func(principal, spec string) func() error {
		return func() error {
			_, err := e.service.Edit(context.Background(), work.EditCommand{
				Scope: scopeA, Caller: e.caller(principal), ID: it.ID, Expected: it.Version, Patch: work.Patch{Spec: str(spec)},
			})
			return err
		}
	}
	errs := parallel(edit(analyst, "# Plan\nAnalyst version"), edit(engineer, "# Plan\nEngineer version"))
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case is(err, work.ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins %d conflicts %d (%v)", wins, conflicts, errs)
	}

	got, err := e.service.Get(context.Background(), scopeA, e.caller(analyst), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.ContentRevision != 2 || (got.Spec != "# Plan\nAnalyst version" && got.Spec != "# Plan\nEngineer version") {
		t.Fatalf("after the race: %+v", got)
	}
	h, a, o := e.records(it)
	if h != 2 || a != 2 || o != 2 {
		t.Fatalf("records after one accepted edit: history %d audit %d outbox %d", h, a, o)
	}

	// The loser's retry with the observed version conflicts again; with the
	// fresh version it applies on top, never overwriting silently.
	if _, err := e.service.Edit(context.Background(), work.EditCommand{Scope: scopeA, Caller: e.caller(analyst), ID: it.ID, Expected: it.Version, Patch: work.Patch{Spec: str("stale")}}); !is(err, work.ErrVersionConflict) {
		t.Fatalf("stale retry: %v", err)
	}
	next, err := e.service.Edit(context.Background(), work.EditCommand{Scope: scopeA, Caller: e.caller(analyst), ID: it.ID, Expected: got.Version, Patch: work.Patch{Spec: str("# Plan\nMerged")}})
	if err != nil || next.Version != 3 || next.ContentRevision != 3 {
		t.Fatalf("retry on the fresh version: %v %+v", err, next)
	}

	// History retains previous and current content in version order.
	page, err := e.service.History(context.Background(), scopeA, e.caller(analyst), it.ID, work.HistoryQuery{Limit: 10})
	if err != nil || len(page) != 3 {
		t.Fatalf("history: %v %d", err, len(page))
	}
	for i, h := range page {
		if int(h.Version) != i+1 {
			t.Fatalf("history order: %d at %d", h.Version, i)
		}
	}
	if page[2].Before["spec"] != got.Spec || page[2].After["spec"] != "# Plan\nMerged" || page[2].Actor.ID != analyst {
		t.Fatalf("third entry: %+v", page[2])
	}
}

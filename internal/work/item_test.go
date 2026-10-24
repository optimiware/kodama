package work_test

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/work"
)

func str(s string) *string { return &s }

// Test_item_content_and_versions: spec/criteria changes increment the content
// revision, every other mutation only the version, with before/after history.
func Test_item_content_and_versions(t *testing.T) {
	it := newItem(work.KindStory)
	if it.State != work.StateBacklog || it.Version != 1 || it.ContentRevision != 1 {
		t.Fatalf("new item = %s v%d r%d", it.State, it.Version, it.ContentRevision)
	}

	t.Run("spec_edit", func(t *testing.T) {
		next, h, err := it.ApplyPatch(work.Patch{Spec: str("# Plan\nChanged")}, engineer, now, "wih_2", "req_1")
		if err != nil || next.Version != 2 || next.ContentRevision != 2 {
			t.Fatalf("spec edit: %v v%d r%d", err, next.Version, next.ContentRevision)
		}
		if h.Before[work.FieldSpec] != it.Spec || h.After[work.FieldSpec] != "# Plan\nChanged" || !h.ContentChanged() {
			t.Fatalf("history = %+v", h)
		}
	})
	t.Run("title_and_blocking_do_not_change_the_revision", func(t *testing.T) {
		next, h, err := it.ApplyPatch(work.Patch{Title: str("Renamed"), BlockedReason: work.Some("waiting for ERP")}, engineer, now, "wih_2", "")
		if err != nil || next.Version != 2 || next.ContentRevision != 1 || next.BlockedReason != "waiting for ERP" {
			t.Fatalf("%v %+v", err, next)
		}
		if h.ContentChanged() || len(h.ChangedFields) != 2 || h.Before[work.FieldBlockedReason] != nil || h.After[work.FieldBlockedReason] != "waiting for ERP" {
			t.Fatalf("history = %+v", h)
		}
		cleared, h2, err := next.ApplyPatch(work.Patch{BlockedReason: work.Clear[string]()}, engineer, now, "wih_3", "")
		if err != nil || cleared.BlockedReason != "" || cleared.Version != 3 || h2.Before[work.FieldBlockedReason] != "waiting for ERP" || h2.After[work.FieldBlockedReason] != nil {
			t.Fatalf("clear: %v %+v %+v", err, cleared, h2)
		}
	})
	t.Run("unchanged_values_are_no_change", func(t *testing.T) {
		same := it.Criteria
		if _, _, err := it.ApplyPatch(work.Patch{Spec: &it.Spec, Criteria: &same, Title: &it.Title}, engineer, now, "wih_2", ""); !is(err, work.ErrNoChange) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("validation", func(t *testing.T) {
		long := strings.Repeat("x", work.MaxSpecBytes+1)
		dup := []work.Criterion{{ID: "A", Text: "x"}, {ID: "A", Text: "y"}}
		bad := []work.Criterion{{ID: "bad id", Text: "x"}}
		for name, p := range map[string]work.Patch{
			"empty":            {},
			"blank title":      {Title: str("  ")},
			"oversized spec":   {Spec: &long},
			"duplicate ids":    {Criteria: &dup},
			"bad criterion id": {Criteria: &bad},
			"self parent":      {Parent: work.Some(it.ID)},
			"blank block":      {BlockedReason: work.Some(" ")},
			"bad assignee":     {Assignee: work.Some("a b")},
		} {
			if _, _, err := it.ApplyPatch(p, engineer, now, "wih_2", ""); !is(err, work.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
	t.Run("terminal_items_accept_no_edits", func(t *testing.T) {
		for _, st := range []work.State{work.StateDone, work.StateCancelled} {
			x := inState(work.KindStory, st)
			if _, _, err := x.ApplyPatch(work.Patch{Title: str("x")}, engineer, now, "wih_2", ""); !is(err, work.ErrInvalidTransition) {
				t.Errorf("%s: %v", st, err)
			}
		}
	})
	t.Run("field_sensitive_permissions", func(t *testing.T) {
		if got := (work.Patch{Spec: str("x")}).RequiredPermissions(); len(got) != 1 || got[0] != work.PermEditSpec {
			t.Errorf("spec: %v", got)
		}
		if got := (work.Patch{BlockedReason: work.Clear[string](), Assignee: work.Some("u_1")}).RequiredPermissions(); len(got) != 1 || got[0] != work.PermEditTechnical {
			t.Errorf("technical: %v", got)
		}
		if got := (work.Patch{Title: str("x"), BlockedReason: work.Clear[string]()}).RequiredPermissions(); len(got) != 2 {
			t.Errorf("mixed: %v", got)
		}
	})
	t.Run("creation_validation", func(t *testing.T) {
		base := work.NewItemParams{Scope: scope, ID: "wi_2", Kind: work.KindTask, Title: "t", Actor: engineer, Now: now, HistoryID: "wih_1"}
		for name, mutate := range map[string]func(*work.NewItemParams){
			"kind":     func(p *work.NewItemParams) { p.Kind = "feature" },
			"title":    func(p *work.NewItemParams) { p.Title = "" },
			"actor":    func(p *work.NewItemParams) { p.Actor = work.Actor{ID: "x", Type: "robot"} },
			"scope":    func(p *work.NewItemParams) { p.Scope.ProjectID = "" },
			"self":     func(p *work.NewItemParams) { p.ParentID = p.ID },
			"evidence": func(p *work.NewItemParams) { p.EvidenceLinks = []work.EvidenceRef{{Type: "url", ID: "x"}} },
			"commit": func(p *work.NewItemParams) {
				p.EvidenceLinks = []work.EvidenceRef{{Type: work.EvidenceCommit, ID: "abc"}}
			},
		} {
			p := base
			mutate(&p)
			if _, _, err := work.NewItem(p); !is(err, work.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, h, err := work.NewItem(base); err != nil || h.Kind != work.ChangeCreated || h.Version != 1 {
			t.Errorf("valid creation: %v %+v", err, h)
		}
	})
}

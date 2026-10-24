package dependencies_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
)

var (
	scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	actor = work.Actor{ID: "u_engineer", Type: work.ActorUser}
	now   = time.Date(2026, 10, 19, 10, 5, 0, 123456789, time.UTC)
)

func owner(state work.State) dependencies.Owner {
	return dependencies.Owner{Scope: scope, ID: "wi_a", State: state, Version: 4, ContentRevision: 2}
}

func link(t testing.TB, dependent, prerequisite string) dependencies.Link {
	t.Helper()
	l, err := dependencies.NewLink(scope, domain.WorkItemID(dependent), domain.WorkItemID(prerequisite))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Test_dependency_entity: scope and identity validation, self-link rejection,
// explicit add and remove, terminal edit restrictions, no-op behavior and the
// version and content-revision rules. No external services. WM-01, WM-05.
func Test_dependency_entity(t *testing.T) {
	t.Run("a_link_needs_a_valid_scope_and_valid_endpoints", func(t *testing.T) {
		for name, tc := range map[string]struct {
			scope     domain.Scope
			dep, pre  domain.WorkItemID
			wantClass error
		}{
			"missing tenant":       {domain.Scope{ProjectID: "p_inventory"}, "wi_a", "wi_b", work.ErrInvalid},
			"missing project":      {domain.Scope{TenantID: "t_acme"}, "wi_a", "wi_b", work.ErrInvalid},
			"empty dependent":      {scope, "", "wi_b", work.ErrInvalid},
			"empty prerequisite":   {scope, "wi_a", "", work.ErrInvalid},
			"spaces":               {scope, "wi a", "wi_b", work.ErrInvalid},
			"overlong":             {scope, "wi_a", domain.WorkItemID(strings.Repeat("x", 129)), work.ErrInvalid},
			"path traversal":       {scope, "wi_a", "../wi_b", work.ErrInvalid},
			"self link":            {scope, "wi_a", "wi_a", dependencies.ErrGraph},
			"self link, bad scope": {domain.Scope{}, "wi_a", "wi_a", work.ErrInvalid},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := dependencies.NewLink(tc.scope, tc.dep, tc.pre)
				if !errors.Is(err, tc.wantClass) {
					t.Fatalf("err = %v, want class %v", err, tc.wantClass)
				}
			})
		}
		l, err := dependencies.NewLink(scope, "wi_a", "wi_b")
		if err != nil || l.Scope != scope || l.DependentID != "wi_a" || l.PrerequisiteID != "wi_b" {
			t.Fatalf("valid link: %+v %v", l, err)
		}
	})

	t.Run("add_increments_the_version_and_keeps_everything_else", func(t *testing.T) {
		for _, st := range []work.State{work.StateBacklog, work.StateReady, work.StateActive, work.StateReview} {
			o := owner(st)
			c, err := o.Add(link(t, "wi_a", "wi_b"), dependencies.Facts{}, actor, now, "wih_1", "req_1")
			if err != nil {
				t.Fatalf("%s: %v", st, err)
			}
			if c.Operation != dependencies.OpAdd || c.Owner.Version != 5 || c.Owner.ContentRevision != 2 || c.Owner.State != st ||
				c.Owner.ID != o.ID || c.Owner.Scope != o.Scope {
				t.Fatalf("%s: owner after add = %+v", st, c.Owner)
			}
			h := c.History
			if h.Kind != work.ChangeUpdated || h.Version != 5 || h.ContentRevision != 2 || h.ID != "wih_1" || h.RequestID != "req_1" ||
				h.Actor != actor || h.From != st || h.To != st || h.WorkItemID != "wi_a" || h.Scope != scope {
				t.Fatalf("%s: history = %+v", st, h)
			}
			if len(h.ChangedFields) != 1 || h.ChangedFields[0] != dependencies.FieldDependencies || h.ContentChanged() {
				t.Fatalf("changed fields %v", h.ChangedFields)
			}
			if !h.OccurredAt.Equal(now.Truncate(time.Microsecond)) || h.OccurredAt.Location() != time.UTC {
				t.Fatalf("occurred at %v", h.OccurredAt)
			}
			before := h.Before[dependencies.FieldDependencies].(map[string]any)["prerequisiteId"]
			after := h.After[dependencies.FieldDependencies].(map[string]any)["prerequisiteId"]
			if before != nil || after != "wi_b" {
				t.Fatalf("delta before=%v after=%v", before, after)
			}
		}
	})

	t.Run("remove_increments_the_version_and_records_the_removed_prerequisite", func(t *testing.T) {
		c, err := owner(work.StateActive).Remove(link(t, "wi_a", "wi_b"), dependencies.Facts{LinkExists: true}, actor, now, "wih_2", "")
		if err != nil {
			t.Fatal(err)
		}
		if c.Operation != dependencies.OpRemove || c.Owner.Version != 5 || c.Owner.ContentRevision != 2 || c.Owner.State != work.StateActive {
			t.Fatalf("owner after remove = %+v", c.Owner)
		}
		before := c.History.Before[dependencies.FieldDependencies].(map[string]any)["prerequisiteId"]
		after := c.History.After[dependencies.FieldDependencies].(map[string]any)["prerequisiteId"]
		if before != "wi_b" || after != nil {
			t.Fatalf("delta before=%v after=%v", before, after)
		}
	})

	t.Run("duplicate_add_and_absent_remove_are_no_ops", func(t *testing.T) {
		if _, err := owner(work.StateReady).Add(link(t, "wi_a", "wi_b"), dependencies.Facts{LinkExists: true}, actor, now, "wih_3", ""); !errors.Is(err, work.ErrNoChange) {
			t.Fatalf("duplicate add: %v", err)
		}
		if _, err := owner(work.StateReady).Remove(link(t, "wi_a", "wi_b"), dependencies.Facts{}, actor, now, "wih_4", ""); !errors.Is(err, work.ErrNoChange) {
			t.Fatalf("absent remove: %v", err)
		}
	})

	t.Run("done_and_cancelled_dependents_accept_no_link_changes", func(t *testing.T) {
		for _, st := range []work.State{work.StateDone, work.StateCancelled} {
			o := owner(st)
			if err := o.CheckEditable(); !errors.Is(err, work.ErrInvalidTransition) {
				t.Fatalf("%s editable: %v", st, err)
			}
			// Even the no-op forms are refused: terminal items accept no edits.
			for name, f := range map[string]dependencies.Facts{"new": {}, "existing": {LinkExists: true}} {
				if _, err := o.Add(link(t, "wi_a", "wi_b"), f, actor, now, "wih_5", ""); !errors.Is(err, work.ErrInvalidTransition) {
					t.Errorf("%s add (%s link): %v", st, name, err)
				}
				if _, err := o.Remove(link(t, "wi_a", "wi_b"), f, actor, now, "wih_5", ""); !errors.Is(err, work.ErrInvalidTransition) {
					t.Errorf("%s remove (%s link): %v", st, name, err)
				}
			}
		}
	})

	t.Run("terminal_prerequisites_are_allowed_because_only_the_owner_is_restricted", func(t *testing.T) {
		// The entity sees only the dependent; a done or cancelled prerequisite is
		// an ordinary link target.
		if _, err := owner(work.StateReady).Add(link(t, "wi_a", "wi_done"), dependencies.Facts{}, actor, now, "wih_6", ""); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a_cycle_and_the_size_bound_are_graph_failures", func(t *testing.T) {
		_, err := owner(work.StateReady).Add(link(t, "wi_a", "wi_b"), dependencies.Facts{WouldCycle: true}, actor, now, "wih_7", "")
		if !errors.Is(err, dependencies.ErrGraph) {
			t.Fatalf("cycle: %v", err)
		}
		if msg := err.Error(); strings.Contains(msg, "wi_") {
			t.Fatalf("the message names a work item: %q", msg)
		}
		_, err = owner(work.StateReady).Add(link(t, "wi_a", "wi_b"), dependencies.Facts{PrerequisiteCount: dependencies.MaxPrerequisites}, actor, now, "wih_8", "")
		if !errors.Is(err, dependencies.ErrGraph) {
			t.Fatalf("size bound: %v", err)
		}
		if _, err := owner(work.StateReady).Add(link(t, "wi_a", "wi_b"), dependencies.Facts{PrerequisiteCount: dependencies.MaxPrerequisites - 1}, actor, now, "wih_9", ""); err != nil {
			t.Fatalf("one below the bound: %v", err)
		}
		// An existing link is never re-checked for cycles: it is already stored.
		if _, err := owner(work.StateReady).Add(link(t, "wi_a", "wi_b"), dependencies.Facts{LinkExists: true, WouldCycle: true}, actor, now, "wih_10", ""); !errors.Is(err, work.ErrNoChange) {
			t.Fatalf("existing link: %v", err)
		}
	})

	t.Run("the_link_must_belong_to_the_owner", func(t *testing.T) {
		o := owner(work.StateReady)
		foreignScope := domain.Scope{TenantID: "t_globex", ProjectID: "p_inventory"}
		foreign, err := dependencies.NewLink(foreignScope, "wi_a", "wi_b")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.Add(foreign, dependencies.Facts{}, actor, now, "wih_11", ""); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("foreign scope: %v", err)
		}
		if _, err := o.Add(link(t, "wi_other", "wi_b"), dependencies.Facts{}, actor, now, "wih_12", ""); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("another dependent: %v", err)
		}
	})

	t.Run("an_attributable_actor_and_a_history_id_are_required", func(t *testing.T) {
		o := owner(work.StateReady)
		l := link(t, "wi_a", "wi_b")
		if _, err := o.Add(l, dependencies.Facts{}, work.Actor{}, now, "wih_13", ""); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("anonymous actor: %v", err)
		}
		if _, err := o.Add(l, dependencies.Facts{}, work.Actor{ID: "x", Type: "robot"}, now, "wih_13", ""); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("unknown actor type: %v", err)
		}
		if _, err := o.Add(l, dependencies.Facts{}, actor, now, "", ""); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("no history id: %v", err)
		}
	})

	t.Run("the_version_range_is_enforced", func(t *testing.T) {
		o := owner(work.StateReady)
		o.Version = 9007199254740991
		if _, err := o.Add(link(t, "wi_a", "wi_b"), dependencies.Facts{}, actor, now, "wih_14", ""); !errors.Is(err, work.ErrInvalid) {
			t.Fatalf("overflowing version: %v", err)
		}
	})
}

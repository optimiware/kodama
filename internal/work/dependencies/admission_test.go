package dependencies_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/work"
)

// Test_dependency_admission_policy: the exported pure completion policy of the
// work package. Only done satisfies a prerequisite; cancelled never does; a
// missing row is never satisfied; the refusal is generic. No external services.
// WM-05.
func Test_dependency_admission_policy(t *testing.T) {
	states := []work.State{work.StateBacklog, work.StateReady, work.StateActive, work.StateReview, work.StateDone, work.StateCancelled}
	for _, st := range states {
		want := st == work.StateDone
		if got := work.PrerequisiteSatisfied(work.PrerequisiteStatus{ID: "wi_b", State: st, Found: true}); got != want {
			t.Errorf("PrerequisiteSatisfied(%s) = %v, want %v", st, got, want)
		}
		err := work.DependenciesComplete([]work.PrerequisiteStatus{{ID: "wi_b", State: st, Found: true}})
		if want && err != nil {
			t.Errorf("a done prerequisite blocks admission: %v", err)
		}
		if !want && !errors.Is(err, work.ErrPrerequisite) {
			t.Errorf("%s prerequisite: err = %v, want ErrPrerequisite", st, err)
		}
	}

	t.Run("no_links_admit", func(t *testing.T) {
		if err := work.DependenciesComplete(nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a_missing_row_is_never_satisfied_even_with_a_done_state", func(t *testing.T) {
		if work.PrerequisiteSatisfied(work.PrerequisiteStatus{ID: "wi_b", State: work.StateDone, Found: false}) {
			t.Fatal("a row that was not found satisfied the link")
		}
		err := work.DependenciesComplete([]work.PrerequisiteStatus{{ID: "wi_a", State: work.StateDone, Found: true}, {ID: "wi_b"}})
		if !errors.Is(err, work.ErrPrerequisite) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("one_unmet_prerequisite_blocks_among_many_done", func(t *testing.T) {
		ps := []work.PrerequisiteStatus{
			{ID: "wi_1", State: work.StateDone, Found: true},
			{ID: "wi_2", State: work.StateDone, Found: true},
			{ID: "wi_3", State: work.StateCancelled, Found: true},
		}
		if err := work.DependenciesComplete(ps); !errors.Is(err, work.ErrPrerequisite) {
			t.Fatalf("err = %v", err)
		}
		ps[2].State = work.StateDone
		if err := work.DependenciesComplete(ps); err != nil {
			t.Fatalf("all done: %v", err)
		}
	})
	t.Run("the_refusal_names_neither_prerequisite_nor_state", func(t *testing.T) {
		err := work.DependenciesComplete([]work.PrerequisiteStatus{{ID: "wi_secret_id", State: work.StateReview, Found: true}})
		if err == nil || strings.Contains(err.Error(), "wi_secret_id") || strings.Contains(err.Error(), "review") {
			t.Fatalf("message = %v", err)
		}
	})
}

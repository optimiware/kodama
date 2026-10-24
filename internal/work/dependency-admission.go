package work

import (
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// PrerequisiteStatus is the current state of one direct business prerequisite
// of a work item, as read in the admission transaction. Found is false when the
// referenced work item could not be read in the item's scope: a missing row is
// never satisfied.
type PrerequisiteStatus struct {
	ID    domain.WorkItemID
	State State
	Found bool
}

// PrerequisiteSatisfied reports whether the prerequisite lets its dependent
// start. Only done satisfies a dependency link: a cancelled prerequisite never
// does (delete the link to drop the prerequisite deliberately), and reopening a
// done prerequisite withdraws the satisfaction again.
func PrerequisiteSatisfied(p PrerequisiteStatus) bool {
	return p.Found && p.State == StateDone
}

// DependenciesComplete is the pure admission policy of the business dependency
// graph (WM-05): every direct prerequisite must be done. It returns nil for an
// empty set, and an ErrPrerequisite error with a fixed, client-safe message
// otherwise; the message names neither the prerequisite nor its state, so the
// failure discloses nothing beyond the refusal itself.
func DependenciesComplete(ps []PrerequisiteStatus) error {
	unmet := 0
	for _, p := range ps {
		if !PrerequisiteSatisfied(p) {
			unmet++
		}
	}
	if unmet > 0 {
		return prerequisite("a prerequisite work item is not done")
	}
	return nil
}

// String implements fmt.Stringer for diagnostics in tests and logs.
func (p PrerequisiteStatus) String() string {
	if !p.Found {
		return fmt.Sprintf("%s: missing", p.ID)
	}
	return fmt.Sprintf("%s: %s", p.ID, p.State)
}

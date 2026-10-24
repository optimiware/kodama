package dependencies

import (
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// Caller identifies who is asking; it is the work module's verified caller.
type Caller = work.Caller

// Permissions of the dependency use cases, reusing the work module's names (the
// fixed role mapping belongs to the identity module). Reading dependencies
// needs work_item:read; adding or removing a business relationship is an edit
// of the dependent's intent, like changing its parent.
const (
	PermList   = work.PermRead
	PermMutate = work.PermEditSpec
)

// ListCommand lists the direct prerequisites of Dependent, ordered by the
// prerequisite identifier. After is the continuation key (exclusive); Limit is
// the number of rows to read (callers ask for page size + 1).
type ListCommand struct {
	Scope     domain.Scope
	Caller    Caller
	Dependent domain.WorkItemID
	After     domain.WorkItemID
	Limit     int
}

// AddCommand adds the link Dependent -> Prerequisite under the dependent's
// expected resource version.
type AddCommand struct {
	Scope        domain.Scope
	Caller       Caller
	Dependent    domain.WorkItemID
	Prerequisite domain.WorkItemID
	Expected     domain.Version
}

// RemoveCommand removes the link Dependent -> Prerequisite under the
// dependent's expected resource version.
type RemoveCommand = AddCommand

// Prerequisite is one listed link.
type Prerequisite struct {
	Link      Link
	CreatedAt time.Time
	CreatedBy work.Actor
}

// Page is one page of a dependent's prerequisites together with the dependent's
// version read in the same snapshot, so a client can use it as If-Match.
type Page struct {
	OwnerVersion domain.Version
	Items        []Prerequisite
}

// Outcome is the result of Add or Remove. Changed is false for a no-op (the
// link already existed or was already absent): nothing was written and the
// version is the unchanged one.
type Outcome struct {
	Link    Link
	Changed bool
	Owner   Owner
}

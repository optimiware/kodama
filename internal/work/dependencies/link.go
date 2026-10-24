// Package dependencies is the business dependency graph of work management
// (task T-045): same-project, acyclic prerequisite links between work items,
// evaluated when a work item is admitted to active.
//
// A link says "the dependent work item cannot start until its prerequisite is
// done". It is not a pipeline step DAG: pipeline definitions order steps inside
// one run, links order business intent across work items.
//
// The package holds the link entity, the use cases and the small ports they
// need. It imports no pgx, net/http or broker type; the postgres and http
// sub-packages are the adapters and the composition root (T-046) wires them.
package dependencies

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// MaxPrerequisites bounds the direct prerequisites of one work item. It keeps
// admission checks, list pages and history entries bounded.
const MaxPrerequisites = 100

// FieldDependencies is the changed-field name a link change records in work
// item history and in the work_item.updated event.
const FieldDependencies = "dependencies"

// Link is one business dependency edge: DependentID cannot start before
// PrerequisiteID is done. Scope, DependentID and PrerequisiteID are immutable.
type Link struct {
	Scope          domain.Scope
	DependentID    domain.WorkItemID
	PrerequisiteID domain.WorkItemID
}

// NewLink validates the scope and both endpoint identifiers and rejects a
// self-link. Ownership of the endpoints is a persistence fact checked in the
// mutating transaction: an identifier alone is never authority.
func NewLink(scope domain.Scope, dependent, prerequisite domain.WorkItemID) (Link, error) {
	if err := scope.Validate(); err != nil {
		return Link{}, fmt.Errorf("%w: %v", work.ErrInvalid, err)
	}
	if err := dependent.Validate(); err != nil {
		return Link{}, fmt.Errorf("%w: dependent: %v", work.ErrInvalid, err)
	}
	if err := prerequisite.Validate(); err != nil {
		return Link{}, fmt.Errorf("%w: dependency id: %v", work.ErrInvalid, err)
	}
	if dependent == prerequisite {
		return Link{}, graphf("a work item cannot depend on itself")
	}
	return Link{Scope: scope, DependentID: dependent, PrerequisiteID: prerequisite}, nil
}

// Operation is the kind of link change.
type Operation string

const (
	OpAdd    Operation = "add"
	OpRemove Operation = "remove"
)

// Owner is the dependent work item as the graph sees it: the entity whose
// version a link change increments. A link change never touches the item's
// content revision, state or any other field.
type Owner struct {
	Scope           domain.Scope
	ID              domain.WorkItemID
	State           work.State
	Version         domain.Version
	ContentRevision domain.ContentRevision
}

// Facts are the current graph facts a link change depends on. The repository
// answers them inside the mutating transaction, after the project and item
// locks were acquired.
type Facts struct {
	// LinkExists reports whether the link is already present.
	LinkExists bool
	// WouldCycle reports whether the prerequisite already depends, directly or
	// transitively, on the dependent. Only meaningful for an insertion.
	WouldCycle bool
	// PrerequisiteCount is the dependent's current number of direct prerequisites.
	PrerequisiteCount int
}

// Change is the outcome of an explicit link operation: the owner after the
// change (its version incremented, content revision preserved), the
// attributable history entry and the edge to write.
type Change struct {
	Operation Operation
	Link      Link
	Owner     Owner
	History   work.HistoryEntry
}

// CheckEditable reports whether the owner accepts link changes. Like every
// edit, it is refused for done and cancelled items (reopen a done item first);
// the prerequisites of a link may be in any state.
func (o Owner) CheckEditable() error {
	if o.State == work.StateDone || o.State == work.StateCancelled {
		return fmt.Errorf("%w: a %s work item cannot be edited", work.ErrInvalidTransition, o.State)
	}
	return nil
}

// Add inserts l. It returns work.ErrNoChange when the link already exists
// (adding twice is idempotent and writes nothing), ErrGraph when the link
// would close a cycle or exceed MaxPrerequisites, and work.ErrInvalidTransition
// when the owner is done or cancelled.
func (o Owner) Add(l Link, f Facts, actor work.Actor, now time.Time, historyID, requestID string) (Change, error) {
	if err := o.check(l, actor, historyID); err != nil {
		return Change{}, err
	}
	if f.LinkExists {
		return Change{}, work.ErrNoChange
	}
	if f.WouldCycle {
		return Change{}, graphf("the dependency would create a cycle")
	}
	if f.PrerequisiteCount >= MaxPrerequisites {
		return Change{}, graphf("a work item can have at most %d prerequisites", MaxPrerequisites)
	}
	return o.change(OpAdd, l, nil, string(l.PrerequisiteID), actor, now, historyID, requestID)
}

// Remove deletes l. It returns work.ErrNoChange when the link is absent.
func (o Owner) Remove(l Link, f Facts, actor work.Actor, now time.Time, historyID, requestID string) (Change, error) {
	if err := o.check(l, actor, historyID); err != nil {
		return Change{}, err
	}
	if !f.LinkExists {
		return Change{}, work.ErrNoChange
	}
	return o.change(OpRemove, l, string(l.PrerequisiteID), nil, actor, now, historyID, requestID)
}

func (o Owner) check(l Link, actor work.Actor, historyID string) error {
	if err := o.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", work.ErrInvalid, err)
	}
	if l.Scope != o.Scope || l.DependentID != o.ID {
		return fmt.Errorf("%w: the link does not belong to the work item", work.ErrInvalid)
	}
	if err := o.CheckEditable(); err != nil {
		return err
	}
	if err := actor.Validate(); err != nil {
		return err
	}
	if historyID == "" {
		return fmt.Errorf("%w: a history id is required", work.ErrInvalid)
	}
	return nil
}

// change builds the next owner and its history entry. before and after are the
// prerequisite identifier (or nil) of the link before and after the change: a
// bounded delta rather than a copy of the graph.
func (o Owner) change(op Operation, l Link, before, after any, actor work.Actor, now time.Time, historyID, requestID string) (Change, error) {
	next := o
	next.Version = o.Version + 1
	if err := next.Version.Validate(); err != nil {
		return Change{}, fmt.Errorf("%w: %v", work.ErrInvalid, err)
	}
	at := now.UTC().Truncate(time.Microsecond)
	h := work.HistoryEntry{
		ID: historyID, Scope: o.Scope, WorkItemID: o.ID, Version: next.Version, ContentRevision: next.ContentRevision,
		Kind: work.ChangeUpdated, Actor: actor, OccurredAt: at, ChangedFields: []string{FieldDependencies},
		From: o.State, To: o.State,
		Before:    map[string]any{FieldDependencies: map[string]any{"prerequisiteId": before}},
		After:     map[string]any{FieldDependencies: map[string]any{"prerequisiteId": after}},
		RequestID: requestID,
	}
	return Change{Operation: op, Link: l, Owner: next, History: h}, nil
}

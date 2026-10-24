package work

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Item is the work item entity. Tenant/project ownership, kind and identity are
// immutable after creation. Every mutation increments Version; only an actual
// change of the specification or acceptance criteria increments
// ContentRevision.
type Item struct {
	Scope           domain.Scope
	ID              domain.WorkItemID
	Kind            Kind
	Title           string
	Spec            string
	Criteria        []Criterion
	EvidenceLinks   []EvidenceRef
	AssigneeID      string
	ParentID        domain.WorkItemID
	State           State
	BlockedReason   string
	Version         domain.Version
	ContentRevision domain.ContentRevision
	// ReviewSubject is the candidate and evidence recorded when the item
	// entered review; nil in every other state.
	ReviewSubject *ReviewSubject
	// Completion is the human decision recorded when the item reached done; nil
	// in every other state. Earlier decisions stay in history after a reopen.
	Completion *CompletionRecord
	CreatedBy  Actor
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Changed-field names, as carried in history and work_item.updated events.
const (
	FieldKind          = "kind"
	FieldTitle         = "title"
	FieldSpec          = "spec"
	FieldCriteria      = "acceptanceCriteria"
	FieldEvidenceLinks = "evidenceLinks"
	FieldAssignee      = "assigneeId"
	FieldParent        = "parentId"
	FieldBlockedReason = "blockedReason"
)

// ChangeKind classifies a history entry.
type ChangeKind string

const (
	ChangeCreated      ChangeKind = "created"
	ChangeUpdated      ChangeKind = "updated"
	ChangeTransitioned ChangeKind = "transitioned"
)

// HistoryEntry is one immutable record of a mutation: who, when, why, which
// fields, and the previous and new values of those fields. Version is the
// resource version the mutation produced, so history is ordered by version.
type HistoryEntry struct {
	ID              string
	Scope           domain.Scope
	WorkItemID      domain.WorkItemID
	Version         domain.Version
	ContentRevision domain.ContentRevision
	Kind            ChangeKind
	Actor           Actor
	OccurredAt      time.Time
	Reason          string
	ChangedFields   []string
	From, To        State
	Before, After   map[string]any
	// Review and Completion snapshot the decision evidence of the transition.
	Review     *ReviewSubject
	Completion *CompletionRecord
	RequestID  string
}

// NewItemParams are the creation inputs. The new item starts in backlog at
// version 1 and content revision 1.
type NewItemParams struct {
	Scope         domain.Scope
	ID            domain.WorkItemID
	Kind          Kind
	Title         string
	Spec          string
	Criteria      []Criterion
	EvidenceLinks []EvidenceRef
	AssigneeID    string
	ParentID      domain.WorkItemID
	Actor         Actor
	Now           time.Time
	HistoryID     string
	RequestID     string
}

// NewItem validates the inputs and returns the created item with its history
// entry.
func NewItem(p NewItemParams) (Item, HistoryEntry, error) {
	if err := p.Scope.Validate(); err != nil {
		return Item{}, HistoryEntry{}, invalid("%v", err)
	}
	if err := p.ID.Validate(); err != nil {
		return Item{}, HistoryEntry{}, invalid("%v", err)
	}
	if !p.Kind.Valid() {
		return Item{}, HistoryEntry{}, invalid("kind must be epic, story, task or defect")
	}
	if err := p.Actor.Validate(); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if p.Now.IsZero() || p.HistoryID == "" {
		return Item{}, HistoryEntry{}, invalid("creation time and history id are required")
	}
	if err := validateTitle(p.Title); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if err := validateSpec(p.Spec); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if err := validateCriteria(p.Criteria); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if err := validateEvidenceLinks(p.EvidenceLinks); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if p.AssigneeID != "" {
		if err := validateOpaqueID("assignee id", p.AssigneeID); err != nil {
			return Item{}, HistoryEntry{}, err
		}
	}
	if p.ParentID != "" {
		if err := p.ParentID.Validate(); err != nil {
			return Item{}, HistoryEntry{}, invalid("parent id: %v", err)
		}
		if p.ParentID == p.ID {
			return Item{}, HistoryEntry{}, invalid("a work item cannot be its own parent")
		}
	}
	now := p.Now.UTC().Truncate(time.Microsecond)
	it := Item{
		Scope: p.Scope, ID: p.ID, Kind: p.Kind, Title: p.Title, Spec: p.Spec,
		Criteria: cloneCriteria(p.Criteria), EvidenceLinks: cloneEvidence(p.EvidenceLinks),
		AssigneeID: p.AssigneeID, ParentID: p.ParentID, State: StateBacklog,
		Version: 1, ContentRevision: 1, CreatedBy: p.Actor, CreatedAt: now, UpdatedAt: now,
	}
	after := map[string]any{
		FieldKind: string(it.Kind), FieldTitle: it.Title, FieldSpec: it.Spec, FieldCriteria: it.Criteria,
	}
	changed := []string{FieldKind, FieldTitle, FieldSpec, FieldCriteria}
	if len(it.EvidenceLinks) > 0 {
		after[FieldEvidenceLinks] = it.EvidenceLinks
		changed = append(changed, FieldEvidenceLinks)
	}
	if it.AssigneeID != "" {
		after[FieldAssignee] = it.AssigneeID
		changed = append(changed, FieldAssignee)
	}
	if it.ParentID != "" {
		after[FieldParent] = string(it.ParentID)
		changed = append(changed, FieldParent)
	}
	h := HistoryEntry{
		ID: p.HistoryID, Scope: it.Scope, WorkItemID: it.ID, Version: it.Version, ContentRevision: it.ContentRevision,
		Kind: ChangeCreated, Actor: p.Actor, OccurredAt: now, ChangedFields: changed,
		To: StateBacklog, Before: map[string]any{}, After: after, RequestID: p.RequestID,
	}
	return it, h, nil
}

func validateTitle(t string) error {
	return validateText("title", t, MaxTitleChars, false)
}

func validateSpec(s string) error {
	if len(s) > MaxSpecBytes {
		return invalid("spec must be at most %d bytes", MaxSpecBytes)
	}
	return validateText("spec", s, MaxSpecBytes, true)
}

// Nullable distinguishes an omitted patch field (Set false) from one that sets
// a value or clears it (Set true with a nil Value).
type Nullable[T any] struct {
	Set   bool
	Value *T
}

// Some sets the field to v.
func Some[T any](v T) Nullable[T] { return Nullable[T]{Set: true, Value: &v} }

// Clear sets the field to empty.
func Clear[T any]() Nullable[T] { return Nullable[T]{Set: true} }

// Patch is a partial edit. Omitted fields stay unchanged. It cannot carry state,
// version, content revision, ownership or caller identity.
type Patch struct {
	Title         *string
	Spec          *string
	Criteria      *[]Criterion
	EvidenceLinks *[]EvidenceRef
	Assignee      Nullable[string]
	Parent        Nullable[domain.WorkItemID]
	BlockedReason Nullable[string]
}

// Empty reports whether the patch names no field.
func (p Patch) Empty() bool {
	return p.Title == nil && p.Spec == nil && p.Criteria == nil && p.EvidenceLinks == nil &&
		!p.Assignee.Set && !p.Parent.Set && !p.BlockedReason.Set
}

// RequiredPermissions lists the permissions the patch needs, by field: analysts
// edit intent, engineers technical fields.
func (p Patch) RequiredPermissions() []Permission {
	var out []Permission
	if p.Title != nil || p.Spec != nil || p.Criteria != nil || p.EvidenceLinks != nil || p.Parent.Set {
		out = append(out, PermEditSpec)
	}
	if p.Assignee.Set || p.BlockedReason.Set {
		out = append(out, PermEditTechnical)
	}
	return out
}

// ApplyPatch validates p against the current item and returns the edited item
// and its history entry. It returns ErrNoChange when the patch changes nothing,
// and ErrInvalidTransition for items in a terminal state, which accept no edits
// (reopen a done item first).
func (it Item) ApplyPatch(p Patch, actor Actor, now time.Time, historyID, requestID string) (Item, HistoryEntry, error) {
	if p.Empty() {
		return Item{}, HistoryEntry{}, invalid("the patch names no field")
	}
	if err := actor.Validate(); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if it.State == StateDone || it.State == StateCancelled {
		return Item{}, HistoryEntry{}, fmt.Errorf("%w: a %s work item cannot be edited", ErrInvalidTransition, it.State)
	}
	next := it
	before, after := map[string]any{}, map[string]any{}
	var changed []string
	note := func(field string, was, now any) {
		changed = append(changed, field)
		before[field], after[field] = was, now
	}

	if p.Title != nil && *p.Title != it.Title {
		if err := validateTitle(*p.Title); err != nil {
			return Item{}, HistoryEntry{}, err
		}
		note(FieldTitle, it.Title, *p.Title)
		next.Title = *p.Title
	} else if p.Title != nil {
		if err := validateTitle(*p.Title); err != nil {
			return Item{}, HistoryEntry{}, err
		}
	}
	contentChanged := false
	if p.Spec != nil {
		if err := validateSpec(*p.Spec); err != nil {
			return Item{}, HistoryEntry{}, err
		}
		if *p.Spec != it.Spec {
			note(FieldSpec, it.Spec, *p.Spec)
			next.Spec = *p.Spec
			contentChanged = true
		}
	}
	if p.Criteria != nil {
		if err := validateCriteria(*p.Criteria); err != nil {
			return Item{}, HistoryEntry{}, err
		}
		if !criteriaEqual(it.Criteria, *p.Criteria) {
			note(FieldCriteria, cloneCriteria(it.Criteria), cloneCriteria(*p.Criteria))
			next.Criteria = cloneCriteria(*p.Criteria)
			contentChanged = true
		}
	}
	if p.EvidenceLinks != nil {
		if err := validateEvidenceLinks(*p.EvidenceLinks); err != nil {
			return Item{}, HistoryEntry{}, err
		}
		if !evidenceEqual(it.EvidenceLinks, *p.EvidenceLinks) {
			note(FieldEvidenceLinks, cloneEvidence(it.EvidenceLinks), cloneEvidence(*p.EvidenceLinks))
			next.EvidenceLinks = cloneEvidence(*p.EvidenceLinks)
		}
	}
	if p.Assignee.Set {
		want := ""
		if p.Assignee.Value != nil {
			want = *p.Assignee.Value
			if err := validateOpaqueID("assignee id", want); err != nil {
				return Item{}, HistoryEntry{}, err
			}
		}
		if want != it.AssigneeID {
			note(FieldAssignee, nullableString(it.AssigneeID), nullableString(want))
			next.AssigneeID = want
		}
	}
	if p.Parent.Set {
		var want domain.WorkItemID
		if p.Parent.Value != nil {
			want = *p.Parent.Value
			if err := want.Validate(); err != nil {
				return Item{}, HistoryEntry{}, invalid("parent id: %v", err)
			}
			if want == it.ID {
				return Item{}, HistoryEntry{}, invalid("a work item cannot be its own parent")
			}
		}
		if want != it.ParentID {
			note(FieldParent, nullableString(string(it.ParentID)), nullableString(string(want)))
			next.ParentID = want
		}
	}
	if p.BlockedReason.Set {
		want := ""
		if p.BlockedReason.Value != nil {
			want = *p.BlockedReason.Value
			if err := validateText("blocked reason", want, MaxReasonChars, false); err != nil {
				return Item{}, HistoryEntry{}, err
			}
		}
		if want != it.BlockedReason {
			note(FieldBlockedReason, nullableString(it.BlockedReason), nullableString(want))
			next.BlockedReason = want
		}
	}
	if len(changed) == 0 {
		return Item{}, HistoryEntry{}, ErrNoChange
	}
	sort.Strings(changed)
	next.Version = it.Version + 1
	if contentChanged {
		next.ContentRevision = it.ContentRevision + 1
	}
	if err := next.Version.Validate(); err != nil {
		return Item{}, HistoryEntry{}, invalid("%v", err)
	}
	if err := next.ContentRevision.Validate(); err != nil {
		return Item{}, HistoryEntry{}, invalid("%v", err)
	}
	next.UpdatedAt = now.UTC().Truncate(time.Microsecond)
	h := HistoryEntry{
		ID: historyID, Scope: it.Scope, WorkItemID: it.ID, Version: next.Version, ContentRevision: next.ContentRevision,
		Kind: ChangeUpdated, Actor: actor, OccurredAt: next.UpdatedAt, ChangedFields: changed,
		From: it.State, To: it.State, Before: before, After: after, RequestID: requestID,
	}
	return next, h, nil
}

// ContentChanged reports whether the history entry changed the specification or
// acceptance criteria.
func (h HistoryEntry) ContentChanged() bool {
	for _, f := range h.ChangedFields {
		if f == FieldSpec || f == FieldCriteria {
			return true
		}
	}
	return false
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func cloneCriteria(cs []Criterion) []Criterion {
	if cs == nil {
		return []Criterion{}
	}
	return append([]Criterion{}, cs...)
}

func cloneEvidence(es []EvidenceRef) []EvidenceRef {
	if es == nil {
		return []EvidenceRef{}
	}
	return append([]EvidenceRef{}, es...)
}

func criteriaEqual(a, b []Criterion) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func evidenceEqual(a, b []EvidenceRef) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// criterionIDs returns the item's criterion identifiers as a set.
func (it Item) criterionIDs() map[string]struct{} {
	out := make(map[string]struct{}, len(it.Criteria))
	for _, c := range it.Criteria {
		out[c.ID] = struct{}{}
	}
	return out
}

// HasSpecification reports whether a specification and acceptance criteria are
// supplied, the condition for leaving backlog.
func (it Item) HasSpecification() bool {
	return strings.TrimSpace(it.Spec) != "" && len(it.Criteria) > 0
}

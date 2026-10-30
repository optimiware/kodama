package tasks

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

type Kind string

const (
	Work      Kind = "task"
	Decision  Kind = "decision"
	Promotion Kind = "promotion"
	Incident  Kind = "incident"
)

// Resolution records a decision, not merely completion. An answer/rejection
// never grants permission to promote. Actor authority is established by the
// calling use case; the model checks the supplied role against the inbox.
type Resolution struct {
	Outcome string `json:"outcome"`
	ActorID string `json:"actorId"`
	Role    string `json:"role"`
	Answer  string `json:"answer"`
}

type Task struct {
	Scope          domain.Scope        `json:"scope"`
	ID             domain.TaskID       `json:"id"`
	ParentID       domain.TaskID       `json:"parentId,omitempty"`
	BlockedByID    domain.TaskID       `json:"blockedByTaskId,omitempty"`
	Kind           Kind                `json:"kind"`
	Title          string              `json:"title"`
	Body           string              `json:"body"`
	State          string              `json:"state"`
	ParkedAt       *time.Time          `json:"parkedAt,omitempty"`
	ResumeState    string              `json:"resumeState,omitempty"`
	AssignedRole   string              `json:"assignedRole,omitempty"`
	AssignedUserID string              `json:"assignedUserId,omitempty"`
	RepositoryID   domain.RepositoryID `json:"repoId,omitempty"`
	Branch         string              `json:"branch,omitempty"`
	Context        json.RawMessage     `json:"context"`
	Resolution     *Resolution         `json:"resolution,omitempty"`
	ResolvedAt     *time.Time          `json:"resolvedAt,omitempty"`
	Version        domain.Version      `json:"version"`
	CreatedAt      time.Time           `json:"createdAt"`
	UpdatedAt      time.Time           `json:"updatedAt"`
}

func (t Task) Validate(w Workflow) error {
	if err := w.Validate(); err != nil {
		return err
	}
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	if err := t.ID.Validate(); err != nil {
		return err
	}
	if err := t.Version.Validate(); err != nil {
		return err
	}
	if !slices.Contains([]Kind{Work, Decision, Promotion, Incident}, t.Kind) ||
		len(t.Title) == 0 || len([]rune(t.Title)) > 512 || !slices.Contains(w.States, t.State) {
		return invalid("task kind, title or state")
	}
	var context map[string]json.RawMessage
	if json.Unmarshal(t.Context, &context) != nil || context == nil {
		return invalid("task context must be an object")
	}
	for _, id := range []domain.TaskID{t.ParentID, t.BlockedByID} {
		if id != "" && (id.Validate() != nil || id == t.ID) {
			return invalid("task reference")
		}
	}
	if t.RepositoryID != "" && t.RepositoryID.Validate() != nil {
		return invalid("repository reference")
	}
	if t.AssignedRole != "" && !slices.Contains([]string{"admin", "engineer", "reviewer", "operator", "viewer"}, t.AssignedRole) {
		return invalid("inbox role")
	}
	if t.ParkedAt == nil {
		if t.BlockedByID != "" || t.ResumeState != "" {
			return invalid("unparked task has blocker")
		}
	} else if t.BlockedByID == "" || !slices.Contains(w.States, t.ResumeState) || w.Terminal(t.State) {
		return invalid("parked task needs blocker and resume state")
	}
	if (t.Resolution == nil) != (t.ResolvedAt == nil) {
		return invalid("resolution timestamp")
	}
	if t.Kind != Work && (t.AssignedRole == "" || (t.Resolution == nil && t.State != w.Inbox.OpenState)) {
		return invalid("inbox request must have a role and remain open until resolved")
	}
	if t.Resolution != nil && (t.Kind == Work || t.State != w.Inbox.ResolvedState || t.Resolution.ActorID == "" || t.Resolution.Answer == "" ||
		t.Resolution.Role != t.AssignedRole || !slices.Contains([]string{"approved", "rejected", "answered"}, t.Resolution.Outcome)) {
		return invalid("resolution")
	}
	if t.CreatedAt.IsZero() || t.UpdatedAt.Before(t.CreatedAt) {
		return invalid("task timestamps")
	}
	return nil
}

func (t Task) check(w Workflow, expected domain.Version) error {
	if err := t.Validate(w); err != nil {
		return err
	}
	if t.Version != expected || t.Version == domain.MaxSafeInteger || t.Resolution != nil {
		return fmt.Errorf("%w: stale or resolved task", domain.ErrConflict)
	}
	return nil
}

// Transition returns a copy and the destination state's pipeline name. It never
// changes a parked task or completes a decision without a resolution.
func (t Task) Transition(w Workflow, expected domain.Version, to string, now time.Time) (Task, string, error) {
	if err := t.check(w, expected); err != nil {
		return Task{}, "", err
	}
	_, ok := w.Edge(t.State, to)
	if !ok || t.ParkedAt != nil || now.Before(t.UpdatedAt) || (t.Kind != Work && w.Terminal(to)) {
		return Task{}, "", invalid("transition refused")
	}
	t = t.Clone()
	t.State, t.Version, t.UpdatedAt = to, t.Version+1, now
	return t, w.Triggers[to], nil
}

func (t Task) Park(w Workflow, expected domain.Version, blocker Task, resume string, now time.Time) (Task, error) {
	if err := t.check(w, expected); err != nil {
		return Task{}, err
	}
	_, edge := w.Edge(t.State, resume)
	if blocker.Validate(w) != nil || blocker.Scope != t.Scope || blocker.ID == t.ID || blocker.Kind == Work ||
		blocker.State != w.Inbox.OpenState || blocker.AssignedRole == "" || blocker.ParkedAt != nil || blocker.Resolution != nil ||
		t.ParkedAt != nil || w.Terminal(t.State) || (resume != t.State && !edge) || now.Before(t.UpdatedAt) {
		return Task{}, invalid("parking refused")
	}
	t = t.Clone()
	t.ParkedAt, t.BlockedByID, t.ResumeState = &now, blocker.ID, resume
	t.Version, t.UpdatedAt = t.Version+1, now
	return t, nil
}

func (t Task) Resolve(w Workflow, expected domain.Version, decision Resolution, now time.Time) (Task, error) {
	if err := t.check(w, expected); err != nil {
		return Task{}, err
	}
	if t.Kind == Work || t.State != w.Inbox.OpenState || t.ParkedAt != nil || decision.ActorID == "" || decision.Answer == "" ||
		t.AssignedRole == "" || decision.Role != t.AssignedRole ||
		!slices.Contains([]string{"approved", "rejected", "answered"}, decision.Outcome) || now.Before(t.UpdatedAt) {
		return Task{}, invalid("resolution refused")
	}
	t = t.Clone()
	t.State, t.Resolution, t.ResolvedAt = w.Inbox.ResolvedState, &decision, &now
	t.Version, t.UpdatedAt = t.Version+1, now
	return t, nil
}

// Resume consumes only an affirmative resolution (or an answer for a decision).
// Rejected promotion/incident requests remain parked for explicit rework.
func (t Task) Resume(w Workflow, expected domain.Version, blocker Task, now time.Time) (Task, string, error) {
	if err := t.check(w, expected); err != nil {
		return Task{}, "", err
	}
	if blocker.Validate(w) != nil || blocker.Scope != t.Scope || blocker.ID != t.BlockedByID || t.ParkedAt == nil || blocker.Resolution == nil ||
		!(blocker.Resolution.Outcome == "approved" || (blocker.Kind == Decision && blocker.Resolution.Outcome == "answered")) || now.Before(t.UpdatedAt) {
		return Task{}, "", invalid("resume refused")
	}
	trigger := w.Triggers[t.ResumeState]
	if t.ResumeState != t.State {
		_, ok := w.Edge(t.State, t.ResumeState)
		if !ok {
			return Task{}, "", invalid("resume transition removed")
		}
	}
	t = t.Clone()
	t.State = t.ResumeState
	t.ParkedAt, t.BlockedByID, t.ResumeState = nil, "", ""
	t.Version, t.UpdatedAt = t.Version+1, now
	return t, trigger, nil
}

func (t Task) Clone() Task {
	t.Context = slices.Clone(t.Context)
	if t.Resolution != nil {
		r := *t.Resolution
		t.Resolution = &r
	}
	if t.ParkedAt != nil {
		v := *t.ParkedAt
		t.ParkedAt = &v
	}
	if t.ResolvedAt != nil {
		v := *t.ResolvedAt
		t.ResolvedAt = &v
	}
	return t
}

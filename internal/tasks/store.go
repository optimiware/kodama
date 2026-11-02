package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

var ErrNotFound = errors.New("tasks: not found")

type Event struct {
	ID        string
	TaskID    domain.TaskID
	Type      string
	ActorType string
	ActorID   string
	Payload   json.RawMessage
	CreatedAt time.Time
}

func (e Event) Validate() error {
	var payload map[string]json.RawMessage
	if domain.TaskID(e.ID).Validate() != nil || e.TaskID.Validate() != nil || e.Type == "" || e.ActorID == "" || e.CreatedAt.IsZero() ||
		json.Unmarshal(e.Payload, &payload) != nil || payload == nil {
		return invalid("event")
	}
	switch e.ActorType {
	case "user", "workload", "system", "operator":
		return nil
	default:
		return invalid("event actor type")
	}
}

// Store atomically persists task changes and their audit/outbox events within
// a project. Callbacks do no network I/O. Authorization belongs to the caller.
type Store interface {
	Update(context.Context, domain.Scope, func(Tx) error) error
}

type Tx interface {
	Workflow() Workflow
	Get(domain.TaskID) (Task, error)
	// GetByBlocker returns the task parked behind the given request.
	GetByBlocker(domain.TaskID) (Task, error)
	Save(Task, domain.Version) error // expected=0 creates; updates increment once
	Append(Event) error
}

// CheckSave is shared adapter policy, distinct from SQL ownership enforcement.
// Parked context cannot be edited into a different promotion subject. Resolved
// decisions are immutable; rework creates a fresh request.
func CheckSave(next Task, current *Task, expected domain.Version, scope domain.Scope, w Workflow) error {
	if next.Scope != scope {
		return ErrNotFound
	}
	if err := next.Validate(w); err != nil {
		return err
	}
	if expected == 0 {
		if current != nil || next.Version != 1 {
			return domain.ErrConflict
		}
		return nil
	}
	if current == nil {
		return ErrNotFound
	}
	if current.Version != expected || next.Version != expected+1 || current.Resolution != nil ||
		next.Kind != current.Kind || !next.CreatedAt.Equal(current.CreatedAt) || next.UpdatedAt.Before(current.UpdatedAt) {
		return domain.ErrConflict
	}
	if current.ParkedAt != nil && (!sameContext(next.Context, current.Context) || next.RepositoryID != current.RepositoryID || next.Branch != current.Branch) {
		return domain.ErrConflict
	}
	return nil
}

func sameContext(a, b json.RawMessage) bool {
	var left, right any
	decode := func(raw json.RawMessage, out *any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		return d.Decode(out)
	}
	return decode(a, &left) == nil && decode(b, &right) == nil && reflect.DeepEqual(left, right)
}

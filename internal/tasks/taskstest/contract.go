// Package taskstest exports the same behavior contract for memory and SQL.
package taskstest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/tasks"
)

var Scope = domain.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
var OtherProject = domain.Scope{TenantID: "tenant-a", ProjectID: "project-b"}
var OtherTenant = domain.Scope{TenantID: "tenant-b", ProjectID: "project-a"}

// These deliberately aren't the documentation's SDLC names.
func Workflow() tasks.Workflow {
	return tasks.Workflow{
		InitialState: "draft", States: []string{"draft", "verify", "ship", "closed", "question"},
		TerminalStates: []string{"closed"},
		Transitions:    []tasks.Transition{{From: "draft", To: "verify"}, {From: "verify", To: "ship"}, {From: "ship", To: "closed"}},
		Triggers:       map[string]string{"draft": "plan", "verify": "test", "ship": "promote"},
		Inbox:          tasks.Inbox{OpenState: "question", ResolvedState: "closed"},
	}
}

func Task(id string) tasks.Task {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	return tasks.Task{Scope: Scope, ID: domain.TaskID(id), Kind: tasks.Work, Title: id, State: "draft",
		Context: json.RawMessage(`{"candidate":"commit-a"}`), Version: 1, CreatedAt: now, UpdatedAt: now}
}

type Fixture struct {
	Store  tasks.Store
	Events func(context.Context, domain.Scope) ([]tasks.Event, error)
}

func Contract(t *testing.T, factory func(*testing.T) Fixture) {
	t.Helper()
	t.Run("park_resolve_resume_atomic", func(t *testing.T) {
		f := factory(t)
		parent := Task("parent")
		parent.State = "verify"
		blocker := Task("approval")
		blocker.Kind, blocker.State, blocker.AssignedRole = tasks.Promotion, "question", "reviewer"
		now := parent.UpdatedAt.Add(time.Minute)
		must(t, f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error {
			if err := tx.Save(blocker, 0); err != nil {
				return err
			}
			if err := tx.Save(parent, 0); err != nil {
				return err
			}
			parked, err := parent.Park(tx.Workflow(), 1, blocker, "ship", now)
			if err != nil {
				return err
			}
			return tx.Save(parked, 1)
		}))
		must(t, f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error {
			parent, err := tx.Get("parent")
			if err != nil {
				return err
			}
			if _, _, err := parent.Transition(tx.Workflow(), parent.Version, "ship", now); err == nil {
				t.Fatal("parked task advanced")
			}
			blocker, err := tx.Get("approval")
			if err != nil {
				return err
			}
			resolved, err := blocker.Resolve(tx.Workflow(), 1, tasks.Resolution{Outcome: "approved", ActorID: "reviewer-1", Role: "reviewer", Answer: "Promote commit-a"}, now)
			if err != nil {
				return err
			}
			if err := tx.Save(resolved, 1); err != nil {
				return err
			}
			resumed, trigger, err := parent.Resume(tx.Workflow(), 2, resolved, now)
			if err != nil {
				return err
			}
			if resumed.State != "ship" || trigger != "promote" || resumed.ParkedAt != nil {
				t.Fatalf("resume: %+v, trigger=%s", resumed, trigger)
			}
			if err := tx.Save(resumed, 2); err != nil {
				return err
			}
			return tx.Append(tasks.Event{ID: "event-1", TaskID: parent.ID, Type: "task.resolved", ActorType: "user", ActorID: "reviewer-1", Payload: json.RawMessage(`{"outcome":"approved"}`), CreatedAt: now})
		}))
		events, err := f.Events(t.Context(), Scope)
		must(t, err)
		if len(events) != 1 || events[0].Type != "task.resolved" {
			t.Fatalf("events=%+v", events)
		}
	})

	t.Run("rollback_and_reference_isolation", func(t *testing.T) {
		f := factory(t)
		original := Task("original")
		must(t, f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error { return tx.Save(original, 0) }))
		// Mutating caller-owned JSON must not rewrite the committed row.
		original.Context[0] = '['
		stop := errors.New("rollback")
		err := f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error {
			row, err := tx.Get("original")
			if err != nil {
				return err
			}
			if !candidateA(row.Context) {
				t.Fatalf("aliased context: %s", row.Context)
			}
			next, _, err := row.Transition(tx.Workflow(), 1, "verify", row.UpdatedAt.Add(time.Minute))
			if err != nil {
				return err
			}
			if err := tx.Save(next, 1); err != nil {
				return err
			}
			if err := tx.Append(tasks.Event{ID: "rolled-back", TaskID: row.ID, Type: "task.transitioned", ActorType: "system", ActorID: "test", Payload: json.RawMessage(`{}`), CreatedAt: row.UpdatedAt}); err != nil {
				return err
			}
			return stop
		})
		if !errors.Is(err, stop) {
			t.Fatalf("rollback=%v", err)
		}
		must(t, f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error {
			row, err := tx.Get("original")
			if err != nil {
				return err
			}
			if row.Version != 1 || row.State != "draft" {
				t.Fatalf("rollback leaked: %+v", row)
			}
			row.Context[0] = '[' // Get isolation, even in a committed read callback.
			return nil
		}))
		must(t, f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error {
			row, err := tx.Get("original")
			if err != nil {
				return err
			}
			if !candidateA(row.Context) {
				t.Fatal("read mutation leaked")
			}
			return nil
		}))
		events, err := f.Events(t.Context(), Scope)
		must(t, err)
		if len(events) != 0 {
			t.Fatal("event escaped rollback")
		}
		for _, foreign := range []domain.Scope{OtherProject, OtherTenant} {
			err := f.Store.Update(t.Context(), foreign, func(tx tasks.Tx) error { _, err := tx.Get("original"); return err })
			if !errors.Is(err, tasks.ErrNotFound) {
				t.Fatalf("foreign scope: %v", err)
			}
		}
	})

	t.Run("concurrent_compare_and_swap", func(t *testing.T) {
		f := factory(t)
		row := Task("race")
		must(t, f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error { return tx.Save(row, 0) }))
		var winners atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				err := f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error {
					next := row.Clone()
					next.Title = "changed"
					next.Version = 2
					return tx.Save(next, 1)
				})
				if err == nil {
					winners.Add(1)
				} else if !errors.Is(err, domain.ErrConflict) {
					t.Errorf("CAS: %v", err)
				}
			})
		}
		wg.Wait()
		if winners.Load() != 1 {
			t.Fatalf("winners=%d", winners.Load())
		}
	})

	t.Run("duplicate_and_foreign_blocker", func(t *testing.T) {
		f := factory(t)
		row := Task("same")
		must(t, f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error { return tx.Save(row, 0) }))
		err := f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error { return tx.Save(row, 0) })
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("duplicate=%v", err)
		}
		foreign := Task("foreign")
		foreign.Scope = OtherProject
		must(t, f.Store.Update(t.Context(), OtherProject, func(tx tasks.Tx) error { return tx.Save(foreign, 0) }))
		err = f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error {
			next := Task("new")
			next.BlockedByID = "foreign"
			next.ParkedAt = &next.CreatedAt
			next.ResumeState = "verify"
			return tx.Save(next, 0)
		})
		if !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("foreign blocker=%v", err)
		}
		for _, field := range []string{"repository", "assignee"} {
			err := f.Store.Update(t.Context(), Scope, func(tx tasks.Tx) error {
				next := Task("bad-reference")
				if field == "repository" {
					next.RepositoryID = "foreign-repo"
				} else {
					next.AssignedUserID = "foreign-user"
				}
				return tx.Save(next, 0)
			})
			if !errors.Is(err, tasks.ErrNotFound) {
				t.Fatalf("%s reference: %v", field, err)
			}
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func candidateA(raw json.RawMessage) bool {
	var value map[string]string
	return json.Unmarshal(raw, &value) == nil && value["candidate"] == "commit-a"
}

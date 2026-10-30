package tasks_test

import (
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/tasks"
	"github.com/optimiware/kodama/internal/tasks/taskstest"
)

func TestInboxRejectionDoesNotPromote(t *testing.T) {
	w := taskstest.Workflow()
	parent := taskstest.Task("work")
	parent.State = "verify"
	blocker := taskstest.Task("decision")
	blocker.Kind, blocker.State, blocker.AssignedRole = tasks.Promotion, w.Inbox.OpenState, "reviewer"
	now := parent.CreatedAt.Add(time.Minute)
	parked, err := parent.Park(w, 1, blocker, "ship", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"rejected", "answered"} {
		resolved, err := blocker.Resolve(w, 1, tasks.Resolution{Outcome: outcome, ActorID: "u1", Role: "reviewer", Answer: "No promotion"}, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := parked.Resume(w, 2, resolved, now); err == nil {
			t.Fatalf("%s promoted", outcome)
		}
		if _, err := resolved.Resolve(w, 2, tasks.Resolution{Outcome: "approved", ActorID: "u1", Role: "reviewer", Answer: "changed"}, now); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("resolved decision changed: %v", err)
		}
	}
	if _, err := blocker.Resolve(w, 1, tasks.Resolution{Outcome: "approved", ActorID: "agent", Role: "engineer", Answer: "approve"}, now); err == nil {
		t.Fatal("wrong role accepted")
	}
	if _, _, err := blocker.Transition(w, 1, w.Inbox.ResolvedState, now); err == nil {
		t.Fatal("decision resolved without answer")
	}
	changed := parked.Clone()
	changed.Context = []byte(`{"candidate":"commit-b"}`)
	changed.Version++
	if err := tasks.CheckSave(changed, &parked, parked.Version, parked.Scope, w); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("parked subject changed: %v", err)
	}
}

func TestProjectWorkflowValidation(t *testing.T) {
	for name, edit := range map[string]func(*tasks.Workflow){
		"duplicate state":  func(w *tasks.Workflow) { w.States = append(w.States, "draft") },
		"unknown edge":     func(w *tasks.Workflow) { w.Transitions[0].To = "unknown" },
		"terminal edge":    func(w *tasks.Workflow) { w.Transitions[0].From = "closed" },
		"missing inbox":    func(w *tasks.Workflow) { w.Inbox.OpenState = "open" },
		"terminal initial": func(w *tasks.Workflow) { w.InitialState = "closed" },
	} {
		t.Run(name, func(t *testing.T) {
			w := taskstest.Workflow()
			edit(&w)
			if w.Validate() == nil {
				t.Fatal("invalid workflow accepted")
			}
		})
	}
	if err := taskstest.Workflow().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestClarificationResumesSameStateProcess(t *testing.T) {
	w := taskstest.Workflow()
	parent := taskstest.Task("work")
	request := taskstest.Task("question")
	request.Kind, request.State, request.AssignedRole = tasks.Decision, w.Inbox.OpenState, "engineer"
	now := parent.CreatedAt.Add(time.Minute)
	parked, err := parent.Park(w, 1, request, parent.State, now)
	if err != nil {
		t.Fatal(err)
	}
	answered, err := request.Resolve(w, 1, tasks.Resolution{Outcome: "answered", ActorID: "user", Role: "engineer", Answer: "Use the existing API"}, now)
	if err != nil {
		t.Fatal(err)
	}
	resumed, trigger, err := parked.Resume(w, 2, answered, now)
	if err != nil || resumed.State != "draft" || trigger != "plan" {
		t.Fatalf("resume: %+v %q %v", resumed, trigger, err)
	}
}

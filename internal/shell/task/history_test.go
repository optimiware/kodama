package task_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func historyCommand(actor authorize.Principal, limit int, cursor string) task.HistoryCommand {
	return task.HistoryCommand{
		Actor:  actor,
		Scope:  actor.Scope,
		TaskID: taskID,
		Limit:  limit,
		Cursor: cursor,
	}
}

// TestHistoryPassesScopeLimitAndCursor proves history is an authorized read
// whose stable cursor and limit reach the reader unchanged.
func TestHistoryPassesScopeLimitAndCursor(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	entry := task.HistoryEntry{
		Kind:        task.HistoryExecution,
		ExecutionID: executionOne,
		State:       kernel.ExecutionSucceeded,
		At:          testNow,
	}
	message := task.Message{Author: engineerPrincipal, At: testNow, Body: "comment"}
	question := task.Question{ID: questionID, Body: "question", Version: 1, AskedBy: engineerPrincipal, At: testNow}
	page := task.HistoryPage{
		Entries: []task.HistoryEntry{
			entry,
			{Kind: task.HistoryMessage, Message: &message, At: testNow},
			{Kind: task.HistoryQuestion, Question: &question, At: testNow},
		},
		NextCursor: "cursor-2",
	}
	env.reader.history = func(_ context.Context, scope tenancy.Scope, taskIDValue id.TaskID, query task.HistoryQuery) (task.HistoryPage, error) {
		if scope != projectScope() || taskIDValue != taskID {
			t.Fatalf("history read = %+v/%q", scope, taskIDValue)
		}
		return page, nil
	}

	result, err := env.service.History(t.Context(), historyCommand(actor, 2, "cursor-1"))
	requireNoError(t, err, "History")
	if !reflect.DeepEqual(result, page) {
		t.Fatalf("history page = %+v; want %+v", result, page)
	}
	queries := env.reader.historyQueries()
	if len(queries) != 1 || queries[0].Limit != 2 || queries[0].Cursor != "cursor-1" {
		t.Fatalf("history queries = %+v; want one with limit 2 cursor cursor-1", queries)
	}
	calls := env.authorizer.captured()
	if len(calls) != 1 || calls[0].Operation != "task.history" || calls[0].Scope != projectScope() {
		t.Fatalf("authorizer calls = %+v; want one task.history", calls)
	}
	requireNoPortCalls(t, env.ports)
}

// TestHistoryRefusesUnauthorizedAndCrossScope proves reads are scoped and
// authorized before the reader is called.
func TestHistoryRefusesUnauthorizedAndCrossScope(t *testing.T) {
	t.Run("actor scope mismatch", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		actor.Scope = otherScope()
		_, err := env.service.History(t.Context(), historyCommand(actor, 1, ""))
		requireFaultClass(t, err, fault.Forbidden)
		if len(env.reader.historyQueries()) != 0 {
			t.Fatal("reader called for a cross-scope history command")
		}
	})
	t.Run("authorizer denies", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		env.authorizer.authorize = func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
			return authorize.DecisionDeny, nil
		}
		_, err := env.service.History(t.Context(), historyCommand(actor, 1, ""))
		requireFaultClass(t, err, fault.Forbidden)
		if len(env.reader.historyQueries()) != 0 {
			t.Fatal("reader called for a denied history command")
		}
	})
	t.Run("membership removed", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		env.authority.read = func(reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
			return allowFacts(reads, func(read authorize.AuthorityRead) bool {
				return read.Check == authorize.CheckMembership
			}), nil
		}
		_, err := env.service.History(t.Context(), historyCommand(actor, 1, ""))
		requireFaultClass(t, err, fault.Forbidden)
		if len(env.reader.historyQueries()) != 0 {
			t.Fatal("reader called after membership removal")
		}
	})
	t.Run("cross-scope task", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		value := baseTask()
		value.Scope = otherScope()
		env.reader.put(value)
		_, err := env.service.History(t.Context(), historyCommand(actor, 1, ""))
		requireFaultClass(t, err, fault.NotFound)
	})
}

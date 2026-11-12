package work

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// operator is one human principal holding the operator role.
func operator() identity.Principal {
	return identity.Principal{
		ID:      testPrincipal,
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleOperator},
		Enabled: true,
		Version: 1,
	}
}

// seedWaitingTask inserts one waiting task with an operator-assigned question.
func (f *fixture) seedWaitingTask(t *testing.T, scope tenancy.Scope, taskID id.TaskID, at time.Time) {
	t.Helper()
	f.seedWaitingTaskAssigned(t, scope, taskID, request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator}, at)
}

// seedWaitingTaskAssigned inserts one waiting task with the supplied question
// assignment.
func (f *fixture) seedWaitingTaskAssigned(t *testing.T, scope tenancy.Scope, taskID id.TaskID, assignment request.Assignment, at time.Time) {
	t.Helper()
	encodedAssignment, err := jsonColumn(assignment)
	if err != nil {
		t.Fatalf("encode assignment: %v", err)
	}
	references, err := jsonColumn([]string{"git:main", "sha256:1111111111111111111111111111111111111111111111111111111111111111"})
	if err != nil {
		t.Fatalf("encode question references: %v", err)
	}
	_, err = f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "tasks",
			ID:     string(taskID),
			Insert: true,
			Values: map[string]any{
				"content_version":     int64(1),
				"lifecycle_version":   int64(3),
				"state":               string("waiting"),
				"initiator_id":        testPrincipal,
				"title":               "waiting task",
				"created_at":          testNow,
				"question_id":         "question-1",
				"question_body":       "which ref?",
				"question_assignment": encodedAssignment,
				"question_references": references,
				"question_version":    int64(1),
				"question_asked_by":   testPrincipal,
				"question_at":         at,
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed waiting task: %v", err)
	}
}

// seedRequestAction inserts one running request action assigned to a role.
//
// The request projection is self-sufficient: the fixture also seeds the
// authoritative admitted snapshot and the resolved subject an activation
// commits, so the item can carry its declared outputs and immutable subject.
func (f *fixture) seedRequestAction(t *testing.T, scope tenancy.Scope, actionID id.ActionID, executionID id.ExecutionID, assignment request.Assignment, at time.Time) {
	t.Helper()
	resolved := definition.NewSubject(map[string]definition.SubjectValue{
		"seed": {Kind: definition.SubjectLiteral, Literal: "seed-1"},
	})
	f.seedRequestProjection(t, scope, requestProjection{
		executionID: executionID,
		actionID:    actionID,
		name:        "approve",
		assignment:  assignment,
		declared: definition.Request{
			Assignment: assignment.String(),
			Subject:    resolved,
			Outputs:    []id.OutputKey{"approved"},
		},
		resolved: resolved,
		at:       at,
	})
}

// seedFailedTask inserts one open task whose current execution failed. The
// execution's created_at is the supplied at, which is the projection's At.
func (f *fixture) seedFailedTask(t *testing.T, scope tenancy.Scope, taskID id.TaskID, executionID id.ExecutionID, at time.Time) {
	t.Helper()
	f.seedExecutionForPrincipalAt(t, scope, executionID, taskID, testPrincipal, string(kernel.ExecutionFailed), 3, at)
	_, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "tasks",
			ID:     string(taskID),
			Insert: true,
			Values: map[string]any{
				"content_version":      int64(1),
				"lifecycle_version":    int64(2),
				"state":                string("open"),
				"initiator_id":         testPrincipal,
				"title":                "failed task",
				"created_at":           testNow,
				"current_execution_id": string(executionID),
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed failed task: %v", err)
	}
}

func TestInboxQueryCollectsAssignedKinds(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedWaitingTask(t, scope, "task-waiting", testNow.Add(2*time.Minute))
	fixture.seedRequestAction(t, scope, "action-1", "execution-request", request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator}, testNow.Add(time.Minute))
	fixture.seedFailedTask(t, scope, "task-failed", "execution-failed", testNow.Add(3*time.Minute))
	// A service-assigned request must not appear for a human operator.
	fixture.seedRequestAction(t, scope, "action-service", "execution-service", request.Assignment{Kind: request.AssignmentService, ServiceID: "service-9"}, testNow.Add(4*time.Minute))

	page, err := fixture.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("inbox items = %d; want 3: %+v", len(page.Items), page.Items)
	}
	seen := make(map[inbox.Kind]bool)
	for _, item := range page.Items {
		seen[item.Kind] = true
	}
	for _, kind := range []inbox.Kind{inbox.KindRequest, inbox.KindWaitingTask, inbox.KindFailedTask} {
		if !seen[kind] {
			t.Fatalf("missing inbox kind %q: %+v", kind, page.Items)
		}
	}
	for _, item := range page.Items {
		switch item.Kind {
		case inbox.KindRequest:
			if item.ActionID != "action-1" || item.ExecutionID != "execution-request" {
				t.Fatalf("request item = %+v", item)
			}
			if item.Version != 1 || !item.At.Equal(testNow.Add(time.Minute)) {
				t.Fatalf("request item facts = %+v", item)
			}
		case inbox.KindWaitingTask:
			if item.TaskID != "task-waiting" || item.Version != 1 || item.Assignment.Role != tenancy.RoleOperator {
				t.Fatalf("waiting item = %+v", item)
			}
		case inbox.KindFailedTask:
			// A failed_task item names only the task. It must not carry an
			// execution/action identity, because the public encoder refuses
			// that shape.
			if item.TaskID != "task-failed" {
				t.Fatalf("failed item = %+v", item)
			}
			if item.ExecutionID != "" || item.ActionID != "" {
				t.Fatalf("failed item carries an execution/action identity: %+v", item)
			}
			if item.Version != 2 || item.At.IsZero() {
				t.Fatalf("failed item facts = %+v; want lifecycle version 2 and a nonzero At", item)
			}
		}
	}
	// A kind filter returns only that kind.
	filtered, err := fixture.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Kinds: []inbox.Kind{inbox.KindWaitingTask}, Limit: 10})
	if err != nil {
		t.Fatalf("filtered Query: %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Kind != inbox.KindWaitingTask {
		t.Fatalf("kind filter returned %+v", filtered.Items)
	}
}

func TestInboxPaginationStability(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	for index := range 3 {
		fixture.seedWaitingTask(t, scope, id.TaskID("task-"+string(rune('a'+index))), testNow.Add(time.Duration(index)*time.Minute))
	}
	var (
		items  []inbox.Item
		cursor string
	)
	for {
		page, err := fixture.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if len(items) == 1 {
			// A concurrent later item must never reorder or duplicate the
			// pages already returned.
			fixture.seedWaitingTask(t, scope, "task-late", testNow.Add(10*time.Minute))
		}
	}
	seen := make(map[id.TaskID]bool)
	for _, item := range items {
		if seen[item.TaskID] {
			t.Fatalf("inbox repeated task %q", item.TaskID)
		}
		seen[item.TaskID] = true
	}
	if len(items) != 4 || !seen["task-late"] {
		t.Fatalf("paged items = %+v", items)
	}
	// A cursor from another project scope is refused.
	page, err := fixture.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Limit: 1})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	_, err = fixture.inbox.Query(context.Background(), inbox.Query{Scope: otherScope(), Principal: identity.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: otherScope(), Roles: []tenancy.Role{tenancy.RoleOperator}, Enabled: true, Version: 1}, Limit: 1, Cursor: page.NextCursor})
	mustClass(t, err, fault.Forbidden)
}

func TestInboxScopeIsolation(t *testing.T) {
	fixture := newFixture(t)
	fixture.seedWaitingTask(t, testScope(), "task-waiting", testNow)
	page, err := fixture.inbox.Query(context.Background(), inbox.Query{Scope: otherScope(), Principal: identity.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: otherScope(), Roles: []tenancy.Role{tenancy.RoleOperator}, Enabled: true, Version: 1}, Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("a cross-scope inbox query leaked items: %+v", page.Items)
	}
}

// TestInboxWaitingItemExposesQuestionProjection proves a waiting item carries
// the outstanding question identity/body/references and the task lifecycle
// version, while Version stays the QUESTION version. Request and failed-task
// items leave the new fields empty.
func TestInboxWaitingItemExposesQuestionProjection(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedWaitingTask(t, scope, "task-waiting", testNow)
	fixture.seedRequestAction(t, scope, "action-1", "execution-request", request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator}, testNow)
	fixture.seedFailedTask(t, scope, "task-failed", "execution-failed", testNow)

	page, err := fixture.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("inbox items = %d; want 3: %+v", len(page.Items), page.Items)
	}
	for _, item := range page.Items {
		switch item.Kind {
		case inbox.KindWaitingTask:
			if item.QuestionID != "question-1" {
				t.Errorf("waiting question id = %q; want question-1", item.QuestionID)
			}
			if item.Question != "which ref?" {
				t.Errorf("waiting question body = %q; want the stored body", item.Question)
			}
			wantReferences := []string{"git:main", "sha256:1111111111111111111111111111111111111111111111111111111111111111"}
			if len(item.References) != len(wantReferences) || item.References[0] != wantReferences[0] || item.References[1] != wantReferences[1] {
				t.Errorf("waiting references = %v; want %v", item.References, wantReferences)
			}
			if item.Version != 1 {
				t.Errorf("waiting Version = %d; want the QUESTION version 1", item.Version)
			}
			if item.LifecycleVersion != 3 {
				t.Errorf("waiting LifecycleVersion = %d; want the task lifecycle 3", item.LifecycleVersion)
			}
			if item.Version == item.LifecycleVersion {
				t.Errorf("question and lifecycle versions collapsed to %d", item.Version)
			}
			if item.Assignment.Role != tenancy.RoleOperator {
				t.Errorf("waiting assignment = %+v; want the question assignment", item.Assignment)
			}
		case inbox.KindRequest:
			if item.QuestionID != "" || item.Question != "" || item.References != nil || item.LifecycleVersion != 0 {
				t.Errorf("request item carries a question projection: %+v", item)
			}
		case inbox.KindFailedTask:
			if item.QuestionID != "" || item.Question != "" || item.References != nil || item.LifecycleVersion != 0 {
				t.Errorf("failed-task item carries a question projection: %+v", item)
			}
			if item.Version != 2 {
				t.Errorf("failed-task Version = %d; want its lifecycle version 2", item.Version)
			}
		}
	}
}

// TestInboxWaitingItemCorruptReferencesFailsClosed proves the waiting-task
// projection does not silently drop a row whose stored question references are
// not decodable: the query fails with an internal_failure instead of returning
// an item with a truncated projection or hiding the corruption.
func TestInboxWaitingItemCorruptReferencesFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedWaitingTask(t, scope, "task-waiting", testNow)
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "tasks",
			ID:            "task-waiting",
			ExpectVersion: 0,
			Expect:        map[string]any{"lifecycle_version": int64(3)},
			Values:        map[string]any{"question_references": []byte(`{"not":"references"}`)},
		}},
	}); err != nil {
		t.Fatalf("corrupt question references: %v", err)
	}
	_, err := fixture.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Limit: 10})
	mustClass(t, err, fault.InternalFailure)
}

// TestInboxUnknownAssignmentKindIsInvisible proves the inbox default is
// fail-closed: an assignment whose kind is neither role nor service selects
// nobody, so a corrupt or foreign row cannot leak a question projection.
func TestInboxUnknownAssignmentKindIsInvisible(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedWaitingTaskAssigned(t, scope, "task-unknown", request.Assignment{Kind: request.AssignmentKind("unknown")}, testNow)
	page, err := fixture.inbox.Query(context.Background(), inbox.Query{Scope: scope, Principal: operator(), Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("an unknown assignment kind leaked items: %+v", page.Items)
	}
}

// humanWithRoles and serviceWithRoles build inbox principals with the supplied
// current roles.
func humanWithRoles(principalID string, roles ...tenancy.Role) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID(principalID),
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
}

func serviceWithRoles(principalID string, roles ...tenancy.Role) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID(principalID),
		Kind:    identity.KindService,
		Scope:   testScope(),
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
}

// TestInboxAssignmentVisibilityIsKindAware drives the real inbox adapter. A
// role-assigned waiting item or request is visible only to a human currently
// holding that exact role; a service holding the same role must not receive it
// (the inbox admits service principals, and the question body/references must
// not leak). A service-assigned item is visible only to that exact service, and
// no human or other service sees it.
func TestInboxAssignmentVisibilityIsKindAware(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedWaitingTaskAssigned(t, scope, "task-role", request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}, testNow.Add(time.Minute))
	fixture.seedWaitingTaskAssigned(t, scope, "task-service", request.Assignment{Kind: request.AssignmentService, ServiceID: "service-a"}, testNow.Add(2*time.Minute))
	fixture.seedRequestAction(t, scope, "action-role", "execution-role", request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}, testNow.Add(3*time.Minute))
	fixture.seedRequestAction(t, scope, "action-service", "execution-service", request.Assignment{Kind: request.AssignmentService, ServiceID: "service-a"}, testNow.Add(4*time.Minute))

	roleItems := []string{"task-role", "action-role"}
	serviceItems := []string{"task-service", "action-service"}
	cases := []struct {
		name      string
		principal identity.Principal
		want      []string
	}{
		{name: "exact role holder", principal: humanWithRoles("human-reviewer", tenancy.RoleReviewer), want: roleItems},
		{name: "other human role", principal: humanWithRoles("human-engineer", tenancy.RoleEngineer)},
		{name: "admin does not inherit reviewer", principal: humanWithRoles("human-admin", tenancy.RoleAdmin)},
		{name: "role holder with admin", principal: humanWithRoles("human-reviewer-admin", tenancy.RoleReviewer, tenancy.RoleAdmin), want: roleItems},
		{name: "service holding the role does not see role items", principal: serviceWithRoles("service-a", tenancy.RoleReviewer), want: serviceItems},
		{name: "other service holding the role sees nothing", principal: serviceWithRoles("service-b", tenancy.RoleReviewer)},
		{name: "human does not see service items", principal: humanWithRoles("human-reviewer", tenancy.RoleReviewer), want: roleItems},
		{name: "human with the assigned service id does not see service items", principal: humanWithRoles("service-a", tenancy.RoleReviewer), want: roleItems},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			page, err := fixture.inbox.Query(context.Background(), inbox.Query{
				Scope:     scope,
				Principal: testCase.principal,
				Kinds:     []inbox.Kind{inbox.KindWaitingTask, inbox.KindRequest},
				Limit:     10,
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			got := make([]string, 0, len(page.Items))
			for _, item := range page.Items {
				switch item.Kind {
				case inbox.KindWaitingTask:
					got = append(got, string(item.TaskID))
					if testCase.principal.Kind == identity.KindService && item.Assignment.Kind == request.AssignmentRole {
						t.Fatalf("a service received a role-assigned waiting item: %+v", item)
					}
				case inbox.KindRequest:
					got = append(got, string(item.ActionID))
				}
			}
			if len(got) != len(testCase.want) {
				t.Fatalf("visible items = %v; want %v", got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					t.Fatalf("visible items = %v; want %v", got, testCase.want)
				}
			}
		})
	}
}

// queryInbox drives the real adapter for one principal and page.
func queryInbox(t *testing.T, fixture *fixture, principal identity.Principal, kinds []inbox.Kind, limit int, cursor string) inbox.Page {
	t.Helper()
	page, err := fixture.inbox.Query(context.Background(), inbox.Query{
		Scope:     testScope(),
		Principal: principal,
		Kinds:     kinds,
		Limit:     limit,
		Cursor:    cursor,
	})
	if err != nil {
		t.Fatalf("inbox query: %v", err)
	}
	return page
}

// collectFailedTaskIDs walks every page and returns the failed_task task ids in
// page order. An empty kinds list asks for every kind, so non-failed items are
// ignored.
func collectFailedTaskIDs(t *testing.T, fixture *fixture, principal identity.Principal, kinds []inbox.Kind, limit int) []id.TaskID {
	t.Helper()
	var (
		ids    []id.TaskID
		cursor string
	)
	for {
		page := queryInbox(t, fixture, principal, kinds, limit, cursor)
		for _, item := range page.Items {
			if item.Kind == inbox.KindFailedTask {
				ids = append(ids, item.TaskID)
			}
		}
		if page.NextCursor == "" {
			return ids
		}
		if page.NextCursor == cursor {
			t.Fatalf("inbox cursor did not advance from %q", cursor)
		}
		cursor = page.NextCursor
	}
}

// seedTaskRow inserts one task row with the supplied state and current
// execution pointer. An empty execution id seeds no pointer.
func (f *fixture) seedTaskRow(t *testing.T, scope tenancy.Scope, taskID id.TaskID, state string, current id.ExecutionID, lifecycleVersion uint64) {
	t.Helper()
	values := map[string]any{
		"content_version":   int64(1),
		"lifecycle_version": int64(lifecycleVersion),
		"state":             state,
		"initiator_id":      testPrincipal,
		"title":             "task",
		"created_at":        testNow,
	}
	if current != "" {
		values["current_execution_id"] = string(current)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{Table: "tasks", ID: string(taskID), Insert: true, Values: values}},
	}); err != nil {
		t.Fatalf("seed task %q: %v", taskID, err)
	}
}

// updateTask advances one task row under its explicit lifecycle guard.
func (f *fixture) updateTask(t *testing.T, scope tenancy.Scope, taskID id.TaskID, lifecycleVersion uint64, values map[string]any) {
	t.Helper()
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "tasks",
			ID:     string(taskID),
			Expect: map[string]any{"lifecycle_version": int64(lifecycleVersion)},
			Values: values,
		}},
	}); err != nil {
		t.Fatalf("update task %q: %v", taskID, err)
	}
}

// assertPublicInboxItem proves the store projection satisfies the declared
// public inbox schema. A failed_task item that carries an execution identity is
// refused here exactly as the public result encoder refuses it.
func assertPublicInboxItem(t *testing.T, item inbox.Item) {
	t.Helper()
	public := api.InboxItem{
		Kind:             string(item.Kind),
		TaskID:           string(item.TaskID),
		ExecutionID:      string(item.ExecutionID),
		ActionID:         string(item.ActionID),
		Assignment:       item.Assignment.String(),
		Subject:          publicSubject(item.Subject),
		Outputs:          publicOutputs(item.Outputs),
		QuestionID:       string(item.QuestionID),
		Question:         item.Question,
		References:       item.References,
		Version:          item.Version,
		LifecycleVersion: item.LifecycleVersion,
		At:               item.At,
	}
	if err := public.Validate(); err != nil {
		t.Errorf("inbox item violates the public schema: %v; item=%+v", err, item)
	}
}

// publicSubject converts the immutable typed subject into the public ordered
// field form. The stored subject is a map, so names are sorted to keep the wire
// projection deterministic.
func publicSubject(subject *definition.Subject) *api.Subject {
	if subject == nil {
		return nil
	}
	fields := subject.Fields()
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	public := &api.Subject{Fields: make([]api.SubjectField, 0, len(names))}
	for _, name := range names {
		public.Fields = append(public.Fields, api.SubjectField{
			Name:  name,
			Kind:  "string",
			Value: fields[name].Literal,
		})
	}
	return public
}

// publicOutputs converts the declared output names into the public string list
// in declaration order.
func publicOutputs(outputs []id.OutputKey) []string {
	if outputs == nil {
		return nil
	}
	public := make([]string, len(outputs))
	for index, key := range outputs {
		public[index] = string(key)
	}
	return public
}

// TestInboxFailedTaskProjection proves the failed_task projection carries only
// the task identity, its lifecycle version and a nonzero execution timestamp.
// The public schema refuses any execution/action identity and any question
// projection on this kind.
func TestInboxFailedTaskProjection(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	failedAt := testNow.Add(3 * time.Minute)
	fixture.seedFailedTask(t, scope, "task-failed", "execution-failed", failedAt)

	page := queryInbox(t, fixture, operator(), []inbox.Kind{inbox.KindFailedTask}, 10, "")
	if len(page.Items) != 1 {
		t.Fatalf("failed_task items = %+v; want one", page.Items)
	}
	item := page.Items[0]
	if item.Kind != inbox.KindFailedTask || item.TaskID != "task-failed" {
		t.Fatalf("failed_task identity = %+v", item)
	}
	if item.ExecutionID != "" {
		t.Errorf("failed_task execution_id = %q; the public schema refuses an execution identity", item.ExecutionID)
	}
	if item.ActionID != "" {
		t.Errorf("failed_task action_id = %q; the public schema refuses an action identity", item.ActionID)
	}
	if item.Assignment != (request.Assignment{}) {
		t.Errorf("failed_task assignment = %+v; want none", item.Assignment)
	}
	if item.Version != 2 {
		t.Errorf("failed_task Version = %d; want the task lifecycle version 2", item.Version)
	}
	if item.At.IsZero() {
		t.Errorf("failed_task At is zero; want the failed execution's created_at")
	}
	if !item.At.Equal(failedAt) {
		t.Errorf("failed_task At = %s; want %s", item.At, failedAt)
	}
	if item.QuestionID != "" || item.Question != "" || len(item.References) != 0 || item.LifecycleVersion != 0 {
		t.Errorf("failed_task carries a question projection: %+v", item)
	}
	assertPublicInboxItem(t, item)
}

// TestInboxFailedTasksRequireHumanOperator proves failed tasks belong to human
// operators only. Reviewer/engineer/viewer/admin humans do not see them, and a
// service holding the operator role does not either: KODAMA.md assigns failed
// task follow-up to human operators, while services see only explicit
// assignments.
func TestInboxFailedTasksRequireHumanOperator(t *testing.T) {
	fixture := newFixture(t)
	fixture.seedFailedTask(t, testScope(), "task-failed", "execution-failed", testNow.Add(time.Minute))

	cases := []struct {
		name      string
		principal identity.Principal
		want      int
	}{
		{name: "operator human", principal: humanWithRoles("human-operator", tenancy.RoleOperator), want: 1},
		{name: "operator and reviewer human", principal: humanWithRoles("human-operator-reviewer", tenancy.RoleOperator, tenancy.RoleReviewer), want: 1},
		{name: "reviewer-only human", principal: humanWithRoles("human-reviewer", tenancy.RoleReviewer)},
		{name: "engineer-only human", principal: humanWithRoles("human-engineer", tenancy.RoleEngineer)},
		{name: "viewer human", principal: humanWithRoles("human-viewer", tenancy.RoleViewer)},
		{name: "admin human", principal: humanWithRoles("human-admin", tenancy.RoleAdmin)},
		{name: "service holding operator", principal: serviceWithRoles("service-operator", tenancy.RoleOperator)},
		{name: "service holding operator and reviewer", principal: serviceWithRoles("service-operator-reviewer", tenancy.RoleOperator, tenancy.RoleReviewer)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			page := queryInbox(t, fixture, testCase.principal, []inbox.Kind{inbox.KindFailedTask}, 10, "")
			if len(page.Items) != testCase.want {
				t.Fatalf("failed_task items = %+v; want %d", page.Items, testCase.want)
			}
		})
	}
}

// TestInboxFailedTaskFilteringBeforePagination proves the operator filter runs
// before the merge, page slice and cursor calculation. An unauthorized failed
// task must never consume a page slot or become a cursor key.
func TestInboxFailedTaskFilteringBeforePagination(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedFailedTask(t, scope, "task-failed-early", "execution-failed-early", testNow.Add(time.Minute))
	fixture.seedFailedTask(t, scope, "task-failed-late", "execution-failed-late", testNow.Add(2*time.Minute))
	fixture.seedWaitingTaskAssigned(t, scope, "task-waiting-reviewer", request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}, testNow.Add(3*time.Minute))

	reviewer := humanWithRoles("human-reviewer", tenancy.RoleReviewer)
	// The earlier failed tasks must be filtered out before the Limit=1 slice:
	// the reviewer's only visible item is the later waiting task.
	page := queryInbox(t, fixture, reviewer, nil, 1, "")
	if len(page.Items) != 1 || page.Items[0].Kind != inbox.KindWaitingTask || page.Items[0].TaskID != "task-waiting-reviewer" {
		t.Fatalf("reviewer first page = %+v; want only the waiting task", page.Items)
	}
	if page.NextCursor != "" {
		t.Fatalf("reviewer cursor = %q; want none: an unauthorized failed task must not enter the cursor", page.NextCursor)
	}

	for _, kinds := range [][]inbox.Kind{{inbox.KindFailedTask}, nil, {}} {
		page := queryInbox(t, fixture, reviewer, kinds, 1, "")
		if page.NextCursor != "" {
			t.Fatalf("reviewer kinds=%v cursor = %q; want none", kinds, page.NextCursor)
		}
		if got := collectFailedTaskIDs(t, fixture, reviewer, kinds, 1); len(got) != 0 {
			t.Fatalf("reviewer kinds=%v saw failed tasks %v", kinds, got)
		}
	}

	operatorPrincipal := operator()
	explicit := collectFailedTaskIDs(t, fixture, operatorPrincipal, []inbox.Kind{inbox.KindFailedTask}, 1)
	if !slices.Equal(explicit, []id.TaskID{"task-failed-early", "task-failed-late"}) {
		t.Fatalf("operator explicit failed_task ids = %v; want [task-failed-early task-failed-late]", explicit)
	}
	defaultKinds := collectFailedTaskIDs(t, fixture, operatorPrincipal, nil, 1)
	if !slices.Equal(defaultKinds, explicit) {
		t.Fatalf("operator default-kind ids = %v; want %v", defaultKinds, explicit)
	}
	emptyKinds := collectFailedTaskIDs(t, fixture, operatorPrincipal, []inbox.Kind{}, 1)
	if !slices.Equal(emptyKinds, explicit) {
		t.Fatalf("operator empty-kind ids = %v; want %v", emptyKinds, explicit)
	}
}

// TestInboxFailedTasksUseCurrentPrincipal proves every query re-evaluates the
// caller's current roles. Removing the operator role removes the item on the
// next query; restoring it brings the item back.
func TestInboxFailedTasksUseCurrentPrincipal(t *testing.T) {
	fixture := newFixture(t)
	fixture.seedFailedTask(t, testScope(), "task-failed", "execution-failed", testNow.Add(time.Minute))

	principal := operator()
	if got := collectFailedTaskIDs(t, fixture, principal, []inbox.Kind{inbox.KindFailedTask}, 10); !slices.Equal(got, []id.TaskID{"task-failed"}) {
		t.Fatalf("operator failed tasks = %v; want [task-failed]", got)
	}

	principal.Roles = []tenancy.Role{tenancy.RoleReviewer}
	if got := collectFailedTaskIDs(t, fixture, principal, []inbox.Kind{inbox.KindFailedTask}, 10); len(got) != 0 {
		t.Fatalf("reviewer-role failed tasks = %v; want none after operator role removal", got)
	}

	principal.Roles = []tenancy.Role{tenancy.RoleReviewer, tenancy.RoleOperator}
	if got := collectFailedTaskIDs(t, fixture, principal, []inbox.Kind{inbox.KindFailedTask}, 10); !slices.Equal(got, []id.TaskID{"task-failed"}) {
		t.Fatalf("reinstated operator failed tasks = %v; want [task-failed]", got)
	}

	principal.Roles = nil
	if got := collectFailedTaskIDs(t, fixture, principal, []inbox.Kind{inbox.KindFailedTask}, 10); len(got) != 0 {
		t.Fatalf("roleless human failed tasks = %v; want none", got)
	}
}

// TestInboxFailedTaskCurrentExecutionOnly proves eligibility is the task's
// CURRENT execution being failed while the task is open. A historical failure,
// a non-failed current execution, a missing execution and a non-open task are
// all ineligible; a retry or cancel removes a previously eligible task.
func TestInboxFailedTaskCurrentExecutionOnly(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedFailedTask(t, scope, "task-current-failed", "execution-current-failed", testNow.Add(time.Minute))

	for _, state := range []struct {
		name  string
		state string
	}{
		{name: "running", state: string(kernel.ExecutionRunning)},
		{name: "succeeded", state: string(kernel.ExecutionSucceeded)},
		{name: "cancelled", state: string(kernel.ExecutionCancelled)},
		{name: "queued", state: string(kernel.ExecutionQueued)},
	} {
		taskID := id.TaskID("task-current-" + state.name)
		executionID := id.ExecutionID("execution-current-" + state.name)
		fixture.seedExecution(t, scope, executionID, taskID, state.state, 1)
		fixture.seedTaskRow(t, scope, taskID, string(task.StateOpen), executionID, 2)
	}

	// A historical failure is not enough: the current execution runs.
	fixture.seedExecution(t, scope, "execution-historical-failed", "task-historical", string(kernel.ExecutionFailed), 3)
	fixture.seedExecution(t, scope, "execution-historical-current", "task-historical", string(kernel.ExecutionRunning), 1)
	fixture.seedTaskRow(t, scope, "task-historical", string(task.StateOpen), "execution-historical-current", 2)

	// No current execution.
	fixture.seedTaskRow(t, scope, "task-no-execution", string(task.StateOpen), "", 2)

	// A failed current execution does not lift a waiting/done/cancelled task
	// into the operator inbox.
	for _, state := range []struct {
		name  string
		state task.State
	}{
		{name: "waiting", state: task.StateWaiting},
		{name: "done", state: task.StateDone},
		{name: "cancelled", state: task.StateCancelled},
	} {
		taskID := id.TaskID("task-" + state.name)
		executionID := id.ExecutionID("execution-" + state.name + "-failed")
		fixture.seedExecution(t, scope, executionID, taskID, string(kernel.ExecutionFailed), 3)
		fixture.seedTaskRow(t, scope, taskID, string(state.state), executionID, 2)
	}

	want := []id.TaskID{"task-current-failed"}
	if got := collectFailedTaskIDs(t, fixture, operator(), []inbox.Kind{inbox.KindFailedTask}, 10); !slices.Equal(got, want) {
		t.Fatalf("eligible failed tasks = %v; want %v", got, want)
	}

	// A retry replaces the current execution pointer: the old failure is
	// history, not an inbox item.
	fixture.seedFailedTask(t, scope, "task-retry", "execution-retry-failed", testNow.Add(2*time.Minute))
	want = []id.TaskID{"task-current-failed", "task-retry"}
	if got := collectFailedTaskIDs(t, fixture, operator(), []inbox.Kind{inbox.KindFailedTask}, 10); !slices.Equal(got, want) {
		t.Fatalf("failed tasks before retry = %v; want %v", got, want)
	}
	fixture.seedExecution(t, scope, "execution-retry-running", "task-retry", string(kernel.ExecutionRunning), 1)
	fixture.updateTask(t, scope, "task-retry", 2, map[string]any{
		"current_execution_id": string("execution-retry-running"),
		"lifecycle_version":    int64(3),
	})
	want = []id.TaskID{"task-current-failed"}
	if got := collectFailedTaskIDs(t, fixture, operator(), []inbox.Kind{inbox.KindFailedTask}, 10); !slices.Equal(got, want) {
		t.Fatalf("failed tasks after retry = %v; want %v (historical failure is not eligible)", got, want)
	}

	// Cancelling an open failed task settles it out of the inbox.
	fixture.seedFailedTask(t, scope, "task-cancel", "execution-cancel-failed", testNow.Add(3*time.Minute))
	want = []id.TaskID{"task-current-failed", "task-cancel"}
	if got := collectFailedTaskIDs(t, fixture, operator(), []inbox.Kind{inbox.KindFailedTask}, 10); !slices.Equal(got, want) {
		t.Fatalf("failed tasks before cancel = %v; want %v", got, want)
	}
	fixture.updateTask(t, scope, "task-cancel", 2, map[string]any{
		"state":             string(task.StateCancelled),
		"lifecycle_version": int64(3),
	})
	want = []id.TaskID{"task-current-failed"}
	if got := collectFailedTaskIDs(t, fixture, operator(), []inbox.Kind{inbox.KindFailedTask}, 10); !slices.Equal(got, want) {
		t.Fatalf("failed tasks after cancel = %v; want %v", got, want)
	}
}

// requestProjection describes one running request action whose execution row
// carries an admitted snapshot declaring the request action.
type requestProjection struct {
	executionID id.ExecutionID
	actionID    id.ActionID
	name        id.ActionName
	assignment  request.Assignment
	declared    definition.Request
	resolved    definition.Subject
	at          time.Time
	// declaredName and declaredKind optionally disagree with the persisted
	// action row so a corruption test can prove the projection checks the
	// admitted declaration instead of trusting the row.
	declaredName id.ActionName
	declaredKind definition.ActionKind
	// rowIndex optionally overrides the persisted action row index (default
	// 0) and admitted optionally replaces the single admitted action, so a
	// corruption test can prove the projection selects the declaration by the
	// persisted row index rather than by declaration position.
	rowIndex int
	admitted []pipeline.AdmittedAction
}

// seedRequestProjection inserts one running request action together with the
// execution snapshot that declares its response output names. The stored
// request_subject column carries the resolved immutable subject an activation
// commits, while the snapshot keeps the declared (possibly reference) subject.
func (f *fixture) seedRequestProjection(t *testing.T, scope tenancy.Scope, seed requestProjection) {
	t.Helper()
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   []byte("kind: Pipeline\nversion: 1\n"),
		Inputs: map[id.InputKey]string{"seed": "seed-1"},
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	declared := seed.declared
	snapshotName := seed.name
	if seed.declaredName != "" {
		snapshotName = seed.declaredName
	}
	snapshotKind := seed.declaredKind
	if snapshotKind == "" {
		snapshotKind = definition.ActionKindRequest
	}
	admittedActions := seed.admitted
	if admittedActions == nil {
		admittedAction := pipeline.AdmittedAction{
			Index:   0,
			Name:    snapshotName,
			Kind:    snapshotKind,
			Timeout: time.Minute,
		}
		if snapshotKind == definition.ActionKindRequest {
			admittedAction.Request = &declared
		} else {
			admittedAction.Container = &definition.Container{
				Image:    "registry.example/build@sha256:" + strings.Repeat("a", 64),
				Commands: []string{"true"},
			}
		}
		admittedActions = []pipeline.AdmittedAction{admittedAction}
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		PipelineID:      testPipeline,
		PipelineVersion: 1,
		Snapshot:        snapshot,
		Actions:         admittedActions,
	})
	if err != nil {
		t.Fatalf("snapshotdoc.Encode: %v", err)
	}
	assignment, err := jsonColumn(seed.assignment)
	if err != nil {
		t.Fatalf("encode assignment: %v", err)
	}
	subject, err := jsonColumn(seed.resolved)
	if err != nil {
		t.Fatalf("encode resolved subject: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{
			{
				Table:  "executions",
				ID:     string(seed.executionID),
				Insert: true,
				Values: map[string]any{
					"pipeline_id":      testPipeline,
					"task_id":          nil,
					"principal_id":     testPrincipal,
					"state":            string(kernel.ExecutionRunning),
					"version":          int64(1),
					"yaml_sha256":      snapshot.YAMLSHA256[:],
					"snapshot":         document,
					"token_nonce":      nil,
					"token_expires_at": nil,
					"workspace_id":     nil,
					"created_at":       seed.at,
				},
			},
			{
				Table:  "actions",
				ID:     string(seed.actionID),
				Insert: true,
				Values: map[string]any{
					"execution_id":       string(seed.executionID),
					"index":              int64(seed.rowIndex),
					"name":               string(seed.name),
					"kind":               string(definition.ActionKindRequest),
					"state":              string(kernel.ActionRunning),
					"version":            int64(1),
					"timeout_ms":         int64(time.Minute / time.Millisecond),
					"deadline":           seed.at,
					"staged_outputs_set": false,
					"request_assignment": assignment,
					"request_subject":    subject,
				},
			},
		},
	}); err != nil {
		t.Fatalf("seed request projection: %v", err)
	}
}

// operatorRequestSeed returns one operator-assigned running request whose
// declared subject contains both a literal and a resolved input reference, and
// whose declared response names are ordered.
func operatorRequestSeed(executionID id.ExecutionID, actionID id.ActionID, at time.Time) requestProjection {
	return requestProjection{
		executionID: executionID,
		actionID:    actionID,
		name:        "approve",
		assignment:  request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator},
		declared: definition.Request{
			Assignment: "role/operator",
			Subject: definition.NewSubject(map[string]definition.SubjectValue{
				"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
				"seed":     {Kind: definition.SubjectReference, Reference: definition.Reference{Input: true, Key: "seed"}},
			}),
			Outputs: []id.OutputKey{"approved", "decision"},
		},
		resolved: definition.NewSubject(map[string]definition.SubjectValue{
			"artifact": {Kind: definition.SubjectLiteral, Literal: "built:seed-1"},
			"seed":     {Kind: definition.SubjectLiteral, Literal: "seed-1"},
		}),
		at: at,
	}
}

// TestRequestInboxItemIsSelfSufficient proves a request inbox item carries the
// immutable resolved subject, the declared response output names and the action
// deadline, so the exact assignee can decide without a second read. A
// waiting_task or failed_task item carries neither request field.
func TestRequestInboxItemIsSelfSufficient(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	at := testNow.Add(2 * time.Minute)
	fixture.seedRequestProjection(t, scope, operatorRequestSeed("execution-1", "action-1", at))
	fixture.seedWaitingTask(t, scope, "task-waiting", testNow)
	fixture.seedFailedTask(t, scope, "task-failed", "execution-failed", testNow.Add(time.Minute))

	page := queryInbox(t, fixture, operator(), nil, 10, "")
	if len(page.Items) != 3 {
		t.Fatalf("inbox items = %d; want 3: %+v", len(page.Items), page.Items)
	}
	for _, item := range page.Items {
		switch item.Kind {
		case inbox.KindRequest:
			if item.Subject == nil {
				t.Fatalf("request item carries no immutable subject: %+v", item)
			}
			fields := item.Subject.Fields()
			if len(fields) != 2 {
				t.Fatalf("request subject fields = %+v; want artifact and seed", fields)
			}
			if value := fields["artifact"]; value.Kind != definition.SubjectLiteral || value.Literal != "built:seed-1" {
				t.Errorf("request subject artifact = %+v; want the resolved literal built:seed-1", value)
			}
			if value := fields["seed"]; value.Kind != definition.SubjectLiteral || value.Literal != "seed-1" {
				t.Errorf("request subject seed = %+v; want the resolved literal seed-1", value)
			}
			if len(item.Outputs) != 2 || item.Outputs[0] != "approved" || item.Outputs[1] != "decision" {
				t.Errorf("request outputs = %v; want the declared names [approved decision]", item.Outputs)
			}
			if !item.At.Equal(at) {
				t.Errorf("request item deadline = %s; want the action deadline %s", item.At, at)
			}
			if item.QuestionID != "" || item.Question != "" || len(item.References) != 0 || item.LifecycleVersion != 0 {
				t.Errorf("request item carries the waiting question projection: %+v", item)
			}
			assertPublicInboxItem(t, item)
		case inbox.KindWaitingTask:
			if item.Subject != nil || len(item.Outputs) != 0 {
				t.Errorf("waiting_task item carries the request-only projection: %+v", item)
			}
		case inbox.KindFailedTask:
			if item.Subject != nil || len(item.Outputs) != 0 {
				t.Errorf("failed_task item carries the request-only projection: %+v", item)
			}
		}
	}
}

// TestRequestInboxItemDoesNotLeakUnresolvedSubject proves the projection never
// exposes a declared runtime lookup: the item carries the immutable literal an
// activation persisted, and a running row whose stored subject still contains
// an unresolved reference fails closed instead of leaking the lookup.
func TestRequestInboxItemDoesNotLeakUnresolvedSubject(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	fixture.seedRequestProjection(t, scope, seed)

	item := queryInbox(t, fixture, operator(), []inbox.Kind{inbox.KindRequest}, 10, "").Items[0]
	if item.Subject == nil {
		t.Fatal("request item carries no immutable subject")
	}
	for name, value := range item.Subject.Fields() {
		if value.Kind != definition.SubjectLiteral {
			t.Fatalf("request subject %q = %+v; want a resolved literal, never a runtime lookup", name, value)
		}
	}

	// A committed running request can never carry an unresolved subject. A
	// corrupt or partially activated row must fail the read, not expose the
	// reference and not silently drop the subject.
	corrupt := newFixture(t)
	unresolved := seed
	unresolved.executionID = "execution-unresolved"
	unresolved.actionID = "action-unresolved"
	unresolved.resolved = definition.NewSubject(map[string]definition.SubjectValue{
		"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
	})
	corrupt.seedRequestProjection(t, scope, unresolved)
	_, err := corrupt.inbox.Query(context.Background(), inbox.Query{
		Scope: scope, Principal: operator(), Kinds: []inbox.Kind{inbox.KindRequest}, Limit: 10,
	})
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxItemCorruptSnapshotFailsClosed proves the declared output
// names come from the authoritative admitted snapshot: an execution row whose
// snapshot cannot be decoded fails the read instead of returning a request item
// with an invented or empty output list.
func TestRequestInboxItemCorruptSnapshotFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedExecution(t, scope, "execution-1", "", string(kernel.ExecutionRunning), 1)
	assignment, err := jsonColumn(request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator})
	if err != nil {
		t.Fatalf("encode assignment: %v", err)
	}
	subject, err := jsonColumn(definition.NewSubject(map[string]definition.SubjectValue{
		"seed": {Kind: definition.SubjectLiteral, Literal: "seed-1"},
	}))
	if err != nil {
		t.Fatalf("encode subject: %v", err)
	}
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "actions",
			ID:     "action-1",
			Insert: true,
			Values: map[string]any{
				"execution_id":       "execution-1",
				"index":              int64(0),
				"name":               "approve",
				"kind":               string(definition.ActionKindRequest),
				"state":              string(kernel.ActionRunning),
				"version":            int64(1),
				"timeout_ms":         int64(time.Minute / time.Millisecond),
				"deadline":           testNow,
				"staged_outputs_set": false,
				"request_assignment": assignment,
				"request_subject":    subject,
			},
		}},
	}); err != nil {
		t.Fatalf("seed request action: %v", err)
	}
	_, err = fixture.inbox.Query(context.Background(), inbox.Query{
		Scope: scope, Principal: operator(), Kinds: []inbox.Kind{inbox.KindRequest}, Limit: 10,
	})
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxItemProjectionSurvivesPagination proves the self-sufficient
// request fields travel with every page item and are never dropped by the
// cursor merge.
func TestRequestInboxItemProjectionSurvivesPagination(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	for index := range 3 {
		suffix := string(rune('a' + index))
		seed := operatorRequestSeed(id.ExecutionID("execution-"+suffix), id.ActionID("action-"+suffix), testNow.Add(time.Duration(index)*time.Minute))
		seed.resolved = definition.NewSubject(map[string]definition.SubjectValue{
			"seed": {Kind: definition.SubjectLiteral, Literal: "seed-" + suffix},
		})
		fixture.seedRequestProjection(t, scope, seed)
	}

	seen := make(map[string]bool, 3)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("inbox pagination did not terminate")
		}
		page := queryInbox(t, fixture, operator(), []inbox.Kind{inbox.KindRequest}, 1, cursor)
		if len(page.Items) != 1 {
			t.Fatalf("page %d has %d items; want exactly 1", pages, len(page.Items))
		}
		item := page.Items[0]
		if seen[string(item.ActionID)] {
			t.Fatalf("request %q repeated across pages", item.ActionID)
		}
		seen[string(item.ActionID)] = true
		if item.Subject == nil {
			t.Fatalf("paged request %q lost its immutable subject: %+v", item.ActionID, item)
		}
		wantLiteral := "seed-" + string(item.ActionID[len(item.ActionID)-1])
		if value := item.Subject.Fields()["seed"]; value.Kind != definition.SubjectLiteral || value.Literal != wantLiteral {
			t.Errorf("paged request %q subject seed = %+v; want %q", item.ActionID, value, wantLiteral)
		}
		if len(item.Outputs) != 2 || item.Outputs[0] != "approved" || item.Outputs[1] != "decision" {
			t.Errorf("paged request %q outputs = %v; want the declared names", item.ActionID, item.Outputs)
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatalf("inbox cursor did not advance from %q", cursor)
		}
		cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("pages covered %d requests; want 3", len(seen))
	}
}

// queryRequestInbox runs one request-only inbox query for the operator.
func queryRequestInbox(t *testing.T, fixture *fixture) (inbox.Page, error) {
	t.Helper()
	return fixture.inbox.Query(context.Background(), inbox.Query{
		Scope:     testScope(),
		Principal: operator(),
		Kinds:     []inbox.Kind{inbox.KindRequest},
		Limit:     10,
	})
}

// TestRequestInboxItemEmptySubjectFailsClosed proves a running request whose
// stored subject is present but empty fails the read with internal_failure
// instead of returning a self-sufficient item without an immutable subject.
func TestRequestInboxItemEmptySubjectFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	seed.resolved = definition.NewSubject(map[string]definition.SubjectValue{})
	fixture.seedRequestProjection(t, testScope(), seed)
	_, err := queryRequestInbox(t, fixture)
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxItemDeclarationNameMismatchFailsClosed proves the declared
// output names come from the admitted action that the persisted row names. A
// snapshot whose admitted action at the same index carries a different name is
// corruption and fails the read.
func TestRequestInboxItemDeclarationNameMismatchFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	seed.declaredName = "other"
	fixture.seedRequestProjection(t, testScope(), seed)
	_, err := queryRequestInbox(t, fixture)
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxItemDeclarationKindMismatchFailsClosed proves the persisted
// request row must match the admitted action kind. A request row whose admitted
// declaration is a container action is corruption and fails the read.
func TestRequestInboxItemDeclarationKindMismatchFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	seed.declaredKind = definition.ActionKindContainer
	fixture.seedRequestProjection(t, testScope(), seed)
	_, err := queryRequestInbox(t, fixture)
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxItemInvalidDeclaredOutputFailsClosed proves a declared
// response output name is parsed with the declared-key grammar. A
// snapshot that declares an invalid name is corruption and fails the read
// instead of leaking the name onto the wire.
func TestRequestInboxItemInvalidDeclaredOutputFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	seed.declared.Outputs = []id.OutputKey{"Approved"}
	fixture.seedRequestProjection(t, testScope(), seed)
	_, err := queryRequestInbox(t, fixture)
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxItemEmptySubjectFieldNameFailsClosed proves an immutable
// request subject may not contain an empty field name: the projection fails
// closed instead of publishing a nameless field.
func TestRequestInboxItemEmptySubjectFieldNameFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	seed.resolved = definition.NewSubject(map[string]definition.SubjectValue{
		"": {Kind: definition.SubjectLiteral, Literal: "seed-1"},
	})
	fixture.seedRequestProjection(t, testScope(), seed)
	_, err := queryRequestInbox(t, fixture)
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxItemSelectsDeclarationByRowIndex proves the declared output
// names are selected by the persisted action row index, not by declaration
// order. The snapshot admits two request actions with the same name; the row is
// the second one, so the item must carry the second declaration's outputs.
func TestRequestInboxItemSelectsDeclarationByRowIndex(t *testing.T) {
	fixture := newFixture(t)
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	first := seed.declared
	first.Outputs = []id.OutputKey{"first"}
	second := seed.declared
	second.Outputs = []id.OutputKey{"approved", "decision"}
	seed.admitted = []pipeline.AdmittedAction{
		{Index: 0, Name: "approve", Kind: definition.ActionKindRequest, Timeout: time.Minute, Request: &first},
		{Index: 1, Name: "approve", Kind: definition.ActionKindRequest, Timeout: time.Minute, Request: &second},
	}
	seed.rowIndex = 1
	fixture.seedRequestProjection(t, testScope(), seed)

	page := queryInbox(t, fixture, operator(), []inbox.Kind{inbox.KindRequest}, 10, "")
	if len(page.Items) != 1 {
		t.Fatalf("request inbox items = %d; want 1: %+v", len(page.Items), page.Items)
	}
	if got := page.Items[0].Outputs; !slices.Equal(got, []id.OutputKey{"approved", "decision"}) {
		t.Fatalf("request outputs = %v; want the second declaration's names [approved decision]", got)
	}
}

// TestRequestInboxItemMissingExecutionFailsClosed proves a request action that
// references a missing execution is reported as internal_failure, never as
// not_found: the corruption is in the inbox projection, not in the caller's
// requested resource.
func TestRequestInboxItemMissingExecutionFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	assignment, err := jsonColumn(request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleOperator})
	if err != nil {
		t.Fatalf("encode assignment: %v", err)
	}
	subject, err := jsonColumn(definition.NewSubject(map[string]definition.SubjectValue{
		"seed": {Kind: definition.SubjectLiteral, Literal: "seed-1"},
	}))
	if err != nil {
		t.Fatalf("encode subject: %v", err)
	}
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "actions",
			ID:     "action-1",
			Insert: true,
			Values: map[string]any{
				"execution_id":       "execution-missing",
				"index":              int64(0),
				"name":               "approve",
				"kind":               string(definition.ActionKindRequest),
				"state":              string(kernel.ActionRunning),
				"version":            int64(1),
				"timeout_ms":         int64(time.Minute / time.Millisecond),
				"deadline":           testNow,
				"staged_outputs_set": false,
				"request_assignment": assignment,
				"request_subject":    subject,
			},
		}},
	}); err != nil {
		t.Fatalf("seed orphaned request action: %v", err)
	}
	_, err = queryRequestInbox(t, fixture)
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxCorruptRowFailsEveryPrincipal proves the fail-closed design:
// projection corruption is decoded and validated before the assignedTo filter,
// so a row assigned to another principal still fails the read instead of being
// silently skipped as unauthorised.
func TestRequestInboxCorruptRowFailsEveryPrincipal(t *testing.T) {
	fixture := newFixture(t)
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	seed.assignment = request.Assignment{Kind: request.AssignmentService, ServiceID: "service-other"}
	seed.resolved = definition.NewSubject(map[string]definition.SubjectValue{})
	fixture.seedRequestProjection(t, testScope(), seed)

	// The operator is not the assignee, yet the corrupt row is not invisible.
	_, err := queryRequestInbox(t, fixture)
	mustClass(t, err, fault.InternalFailure)
}

// TestRequestInboxCorruptAssignmentFailsClosed proves a running request whose
// stored assignment no longer satisfies the declared grammar fails the read
// with internal_failure. A corrupt assignment is never decoded into an empty
// assignment and silently filtered out as unauthorised.
func TestRequestInboxCorruptAssignmentFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	seed := operatorRequestSeed("execution-1", "action-1", testNow)
	seed.assignment = request.Assignment{Kind: request.AssignmentRole, Role: tenancy.Role("not-a-role")}
	fixture.seedRequestProjection(t, testScope(), seed)
	_, err := queryRequestInbox(t, fixture)
	mustClass(t, err, fault.InternalFailure)
}

// TestInboxQueryServesThePublicMaximumPage pins the public page bound
// (api.MaxPageLimit = 200) through the store adapter. A full 200-row page must
// carry a cursor naming its 200th row, the next page must serve the 201st row
// exactly once, the accepted boundary sizes 100/101/200 must not lose or repeat
// a row, the default page stays api.DefaultPageLimit, and 201 stays refused.
func TestInboxQueryServesThePublicMaximumPage(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	total := api.MaxPageLimit + 1
	for index := range total {
		fixture.seedWaitingTask(t, scope, id.TaskID(fmt.Sprintf("task-page-%03d", index)), testNow.Add(time.Duration(index)*time.Minute))
	}
	kinds := []inbox.Kind{inbox.KindWaitingTask}

	for _, limit := range []int{100, 101, api.MaxPageLimit} {
		page := queryInbox(t, fixture, operator(), kinds, limit, "")
		if len(page.Items) != limit {
			t.Fatalf("limit %d first page has %d items; want %d", limit, len(page.Items), limit)
		}
		last := fmt.Sprintf("task-page-%03d", limit-1)
		if string(page.Items[len(page.Items)-1].TaskID) != last {
			t.Fatalf("limit %d first page last item = %q; want %q", limit, page.Items[len(page.Items)-1].TaskID, last)
		}
		if page.NextCursor == "" {
			t.Fatalf("limit %d first page has no cursor with %d more rows", limit, total-limit)
		}
		cursor, err := decodeInboxCursor(page.NextCursor, scope)
		if err != nil {
			t.Fatalf("limit %d cursor: %v", limit, err)
		}
		if cursor.ID != last {
			t.Fatalf("limit %d cursor names %q; want the last served row %q", limit, cursor.ID, last)
		}
		next := queryInbox(t, fixture, operator(), kinds, limit, page.NextCursor)
		wantNext := fmt.Sprintf("task-page-%03d", limit)
		if len(next.Items) == 0 || string(next.Items[0].TaskID) != wantNext {
			t.Fatalf("limit %d next page starts at %+v; want %q", limit, next.Items, wantNext)
		}
		if limit == api.MaxPageLimit && (len(next.Items) != 1 || next.NextCursor != "") {
			t.Fatalf("limit %d next page = %+v/%q; want the single 201st row and no cursor", limit, next.Items, next.NextCursor)
		}
	}

	// An omitted or zero limit keeps the unchanged default page size.
	for _, limit := range []int{0} {
		page := queryInbox(t, fixture, operator(), kinds, limit, "")
		if len(page.Items) != api.DefaultPageLimit {
			t.Fatalf("limit %d page has %d items; want the unchanged default %d", limit, len(page.Items), api.DefaultPageLimit)
		}
		if page.NextCursor == "" {
			t.Fatalf("limit %d page has no cursor; %d rows remain", limit, total-api.DefaultPageLimit)
		}
	}

	// Above the contract bound the store still refuses, and never truncates.
	for _, limit := range []int{api.MaxPageLimit + 1, api.MaxPageLimit + 100} {
		_, err := fixture.inbox.Query(context.Background(), inbox.Query{
			Scope: scope, Principal: operator(), Kinds: kinds, Limit: limit,
		})
		mustClass(t, err, fault.InvalidInput)
	}
	_, err := fixture.inbox.Query(context.Background(), inbox.Query{
		Scope: scope, Principal: operator(), Kinds: kinds, Limit: -1,
	})
	mustClass(t, err, fault.InvalidInput)
}

// TestInboxMaximumPageWalkIsComplete proves the 200-row boundary walk returns
// every row exactly once in stable order.
func TestInboxMaximumPageWalkIsComplete(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	total := api.MaxPageLimit + 1
	for index := range total {
		fixture.seedWaitingTask(t, scope, id.TaskID(fmt.Sprintf("task-walk-%03d", index)), testNow.Add(time.Duration(index)*time.Minute))
	}
	seen := make([]string, 0, total)
	cursor := ""
	for {
		page := queryInbox(t, fixture, operator(), []inbox.Kind{inbox.KindWaitingTask}, api.MaxPageLimit, cursor)
		for _, item := range page.Items {
			seen = append(seen, string(item.TaskID))
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatalf("cursor did not advance from %q", cursor)
		}
		cursor = page.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("walk served %d rows; want %d", len(seen), total)
	}
	for index, taskID := range seen {
		want := fmt.Sprintf("task-walk-%03d", index)
		if taskID != want {
			t.Fatalf("walk row %d = %q; want %q", index, taskID, want)
		}
	}
}

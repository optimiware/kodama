package domaincontract

// The waiting-task inbox item.
//
// A waiting task's inbox item must expose what the exact outstanding assignee
// needs to answer without a second read:
//
//   - the question identity (task.QuestionID, the value task.reply names),
//   - the question body and its durable references,
//   - the task lifecycle version the reply names as its expected task version.
//
// Item.Version is the QUESTION version, the value task.reply names as its
// expected question version. Visibility is NOT widened: only the exact
// outstanding assignee sees the item — a human currently holding the assigned
// role, or the exact assigned service id. Admin does not inherit Reviewer, and
// a human never sees a service-assigned item.

import (
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestWaitingInboxItemExposesQuestion pins the additive waiting-item shape.
func TestWaitingInboxItemExposesQuestion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	assignment := roleAssignment(t, tenancy.RoleReviewer)
	item := inbox.Item{
		Kind:             inbox.KindWaitingTask,
		TaskID:           id.TaskID("task-1"),
		Assignment:       assignment,
		QuestionID:       task.QuestionID("question-1"),
		Question:         "Which base should the merge use?",
		References:       []string{"sha256:1111111111111111111111111111111111111111111111111111111111111111"},
		Version:          7,  // question version
		LifecycleVersion: 12, // task lifecycle version
		At:               now,
	}

	if item.QuestionID != task.QuestionID("question-1") {
		t.Errorf("waiting item question id = %q; want question-1", item.QuestionID)
	}
	if item.Question == "" || len(item.References) != 1 {
		t.Errorf("waiting item lost question body/references: %+v", item)
	}
	if item.Version == item.LifecycleVersion {
		t.Fatalf("question version and task lifecycle version collapsed to %d; they are independent", item.Version)
	}
	if item.Version != 7 || item.LifecycleVersion != 12 {
		t.Errorf("versions = question %d / lifecycle %d; want 7 / 12", item.Version, item.LifecycleVersion)
	}
	if item.Assignment != assignment {
		t.Errorf("waiting item assignment = %+v; want the question assignment %+v", item.Assignment, assignment)
	}

	// The reply names exactly these values; a different question id type would
	// force a lossy conversion at the public edge.
	if got := reflect.TypeOf(item.QuestionID); got != reflect.TypeOf(task.QuestionID("")) {
		t.Errorf("inbox question id type = %v; want task.QuestionID", got)
	}
}

// TestWaitingItemFieldsAreAdditive pins that the item carries every field
// the request and failed-task consumers read, plus the four question fields.
func TestWaitingItemFieldsAreAdditive(t *testing.T) {
	typ := reflect.TypeOf(inbox.Item{})
	for _, field := range []string{
		"Kind", "TaskID", "ExecutionID", "ActionID", "Assignment", "Version", "At",
		"QuestionID", "Question", "References", "LifecycleVersion",
	} {
		if _, ok := typ.FieldByName(field); !ok {
			t.Errorf("inbox.Item is missing field %q", field)
		}
	}
}

// TestNonWaitingItemsCarryNoQuestion pins that request and failed-task items
// leave the question projection empty. A failed-task item's Version is already
// its lifecycle version.
func TestNonWaitingItemsCarryNoQuestion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	requestItem := inbox.Item{
		Kind:        inbox.KindRequest,
		ExecutionID: id.ExecutionID("execution-1"),
		ActionID:    id.ActionID("action-1"),
		Assignment:  roleAssignment(t, tenancy.RoleEngineer),
		Version:     3,
		At:          now,
	}
	failedItem := inbox.Item{
		Kind:        inbox.KindFailedTask,
		TaskID:      id.TaskID("task-2"),
		ExecutionID: id.ExecutionID("execution-2"),
		Version:     9,
		At:          now,
	}
	for _, item := range []inbox.Item{requestItem, failedItem} {
		if item.QuestionID != "" || item.Question != "" || item.References != nil || item.LifecycleVersion != 0 {
			t.Errorf("%s item carries a question projection: %+v", item.Kind, item)
		}
	}
}

// TestWaitingItemVisibilityUnchanged pins the exact-assignee rule as a SPEC
// ORACLE: it is the declared rule, not a widened visibility set. This vector
// does not exercise the production filter. The real inbox adapter is pinned by
// TestInboxAssignmentVisibilityIsKindAware in
// internal/shell/store/units/work/inbox_test.go, including the principal-kind
// check that keeps a service holding a role from seeing a role-assigned item.
func TestWaitingItemVisibilityUnchanged(t *testing.T) {
	reviewerItem := inbox.Item{
		Kind:       inbox.KindWaitingTask,
		TaskID:     id.TaskID("task-role"),
		Assignment: roleAssignment(t, tenancy.RoleReviewer),
		Version:    4,
	}
	serviceItem := inbox.Item{
		Kind:       inbox.KindWaitingTask,
		TaskID:     id.TaskID("task-service"),
		Assignment: serviceAssignment(t, "service-a"),
		Version:    5,
	}

	reviewer := human("human-reviewer", tenancy.RoleReviewer)
	engineer := human("human-engineer", tenancy.RoleEngineer)
	admin := human("human-admin", tenancy.RoleAdmin)
	reviewerAdmin := human("human-reviewer-admin", tenancy.RoleReviewer, tenancy.RoleAdmin)
	serviceA := servicePrincipal("service-a")
	serviceB := servicePrincipal("service-b")

	cases := []struct {
		name      string
		principal identity.Principal
		item      inbox.Item
		visible   bool
	}{
		{name: "role holder sees role item", principal: reviewer, item: reviewerItem, visible: true},
		{name: "other role does not see role item", principal: engineer, item: reviewerItem, visible: false},
		{name: "admin does not inherit reviewer", principal: admin, item: reviewerItem, visible: false},
		{name: "role holder with admin sees role item", principal: reviewerAdmin, item: reviewerItem, visible: true},
		{name: "human does not see service item", principal: reviewerAdmin, item: serviceItem, visible: false},
		{name: "exact service sees service item", principal: serviceA, item: serviceItem, visible: true},
		{name: "other service does not see service item", principal: serviceB, item: serviceItem, visible: false},
		{name: "service does not see role item", principal: serviceA, item: reviewerItem, visible: false},
	}
	for _, testCase := range cases {
		if got := waitingItemVisible(testCase.principal, testCase.item); got != testCase.visible {
			t.Errorf("%s: visible = %v; want %v", testCase.name, got, testCase.visible)
		}
	}
}

// waitingItemVisible is the current-assignee rule: a human sees a
// role-assigned item only when it currently holds that exact role; a service
// sees only its own exact service-assigned item. Nothing else widens it.
func waitingItemVisible(principal identity.Principal, item inbox.Item) bool {
	switch item.Assignment.Kind {
	case request.AssignmentRole:
		return principal.Kind == identity.KindHuman && principal.HasRole(item.Assignment.Role)
	case request.AssignmentService:
		return principal.Kind == identity.KindService &&
			string(principal.ID) == string(item.Assignment.ServiceID)
	default:
		return false
	}
}

func roleAssignment(t *testing.T, role tenancy.Role) request.Assignment {
	t.Helper()
	assignment, err := request.ParseAssignment("role/" + string(role))
	if err != nil {
		t.Fatalf("parse role assignment: %v", err)
	}
	return assignment
}

func serviceAssignment(t *testing.T, serviceID string) request.Assignment {
	t.Helper()
	assignment, err := request.ParseAssignment("service/" + serviceID)
	if err != nil {
		t.Fatalf("parse service assignment: %v", err)
	}
	return assignment
}

func human(principalID string, roles ...tenancy.Role) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID(principalID),
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
}

func servicePrincipal(principalID string) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID(principalID),
		Kind:    identity.KindService,
		Scope:   tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	}
}

package work

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// engineerRequires is the grant set every task and pipeline operation in these
// tests requires: an Engineer human or service.
func engineerRequires() authorize.Requires {
	return authorize.Requires{
		HumanRoles:   []tenancy.Role{tenancy.RoleEngineer},
		ServiceRoles: []tenancy.Role{tenancy.RoleEngineer},
	}
}

// membershipRead builds one current-membership authority read.
func membershipRead(scope tenancy.Scope, operation authorize.OperationName, principal id.PrincipalID, required authorize.Requires) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Operation:   operation,
		Required:    required,
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
	}
}

// seedMembership inserts one memberships row with the supplied roles.
func (f *fixture) seedMembership(t *testing.T, scope tenancy.Scope, identity, principal string, version uint64, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := jsonColumn(roles)
	if err != nil {
		t.Fatalf("encode membership roles: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "memberships",
			ID:     identity,
			Insert: true,
			Values: map[string]any{
				"principal_id": principal,
				"roles":        encoded,
				"version":      int64(version),
				"created_at":   testNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// setMembershipRoles demotes a membership to the supplied roles at the next
// version, so a unit that read the earlier grant no longer holds it.
func (f *fixture) setMembershipRoles(t *testing.T, scope tenancy.Scope, identity string, version uint64, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := jsonColumn(roles)
	if err != nil {
		t.Fatalf("encode membership roles: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "memberships",
			ID:            identity,
			ExpectVersion: version,
			Values:        map[string]any{"roles": encoded},
		}},
	}); err != nil {
		t.Fatalf("demote membership: %v", err)
	}
}

// deleteMembership revokes a membership row.
func (f *fixture) deleteMembership(t *testing.T, scope tenancy.Scope, identity string, version uint64) {
	t.Helper()
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "memberships",
			ID:            identity,
			Delete:        true,
			ExpectVersion: version,
		}},
	}); err != nil {
		t.Fatalf("revoke membership: %v", err)
	}
}

// seedServiceWithRoles inserts one services row with the supplied roles.
func (f *fixture) seedServiceWithRoles(t *testing.T, scope tenancy.Scope, identity, state string, version uint64, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := jsonColumn(roles)
	if err != nil {
		t.Fatalf("encode service roles: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "services",
			ID:     identity,
			Insert: true,
			Values: map[string]any{
				"name":             identity,
				"state":            state,
				"roles":            encoded,
				"token_nonce":      nil,
				"token_expires_at": nil,
				"version":          int64(version),
				"created_at":       testNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

// setServiceRoles demotes a service to the supplied roles at the next version.
func (f *fixture) setServiceRoles(t *testing.T, scope tenancy.Scope, identity string, version uint64, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := jsonColumn(roles)
	if err != nil {
		t.Fatalf("encode service roles: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "services",
			ID:            identity,
			ExpectVersion: version,
			Values:        map[string]any{"roles": encoded},
		}},
	}); err != nil {
		t.Fatalf("demote service: %v", err)
	}
}

// commitCounts captures the committed rows a refused authority commit must not
// change.
type commitCounts struct {
	receipts int
	audits   int
	events   int
}

func (f *fixture) commitCounts(t *testing.T, scope tenancy.Scope) commitCounts {
	t.Helper()
	return commitCounts{
		receipts: f.countRows(t, scope, "receipts", nil),
		audits:   f.countRows(t, scope, "audit", nil),
		events:   f.countRows(t, scope, "events", nil),
	}
}

func (f *fixture) requireUnchanged(t *testing.T, scope tenancy.Scope, before commitCounts) {
	t.Helper()
	after := f.commitCounts(t, scope)
	if after != before {
		t.Fatalf("a refused commit changed rows: before %+v, after %+v", before, after)
	}
}

// TestTaskWritePortsRefuseRevokedMembership builds every authority-carrying
// task unit while the membership still holds Engineer, then demotes it before
// the port commits. Every port must re-evaluate the current grant and refuse
// with no row, receipt, audit or event change.
func TestTaskWritePortsRefuseRevokedMembership(t *testing.T) {
	type testCase struct {
		name  string
		setup func(*fixture, tenancy.Scope)
		apply func(*testing.T, *fixture, tenancy.Scope) error
		check func(*testing.T, *fixture, tenancy.Scope)
	}
	cases := []testCase{
		{
			name:  "create",
			setup: func(*fixture, tenancy.Scope) {},
			apply: func(t *testing.T, f *fixture, scope tenancy.Scope) error {
				unit := task.CreateUnit{
					Owner: scope, TaskID: testTask, Title: "Fix the build", Initiator: testPrincipal, At: testNow,
					Receipt: f.receipt("task.create", "auth-create", contentHashA),
					Audit:   []event.Audit{f.audit(scope, "task.create")},
					Events:  []event.Envelope{f.event(scope, string(testTask), 1)},
				}
				unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.create", testPrincipal, engineerRequires())}
				f.setMembershipRoles(t, scope, "membership-1", 1, tenancy.RoleViewer)
				_, err := f.task.Create(context.Background(), unit)
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if _, ok := f.storedRow(t, scope, "tasks", string(testTask)); ok {
					t.Fatal("a refused create committed a task row")
				}
			},
		},
		{
			name: "start",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
			},
			apply: func(t *testing.T, f *fixture, scope tenancy.Scope) error {
				unit := f.startUnit(scope, testTask, testExecution, 1, 1, f.receipt("task.start", "auth-start", contentHashA))
				unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.start", testPrincipal, engineerRequires())}
				f.setMembershipRoles(t, scope, "membership-1", 1, tenancy.RoleViewer)
				_, err := f.task.Start(context.Background(), unit)
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if _, ok := f.storedRow(t, scope, "executions", string(testExecution)); ok {
					t.Fatal("a refused start committed an execution")
				}
				if versionValue(f.storedTask(t, scope, testTask), "lifecycle_version") != 1 {
					t.Fatal("a refused start advanced the task lifecycle")
				}
			},
		},
		{
			name: "ask",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
				f.startTask(t, scope, testTask, 1, testExecution)
			},
			apply: func(t *testing.T, f *fixture, scope tenancy.Scope) error {
				unit := f.askUnit(t, scope, testTask, 2, f.receipt("task.ask", "auth-ask", contentHashA))
				unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.ask", testPrincipal, engineerRequires())}
				f.setMembershipRoles(t, scope, "membership-1", 1, tenancy.RoleViewer)
				_, err := f.task.Ask(context.Background(), unit)
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				row := f.storedTask(t, scope, testTask)
				if row.Values["question_id"] != nil || textValue(row, "state") != string(task.StateOpen) {
					t.Fatalf("a refused ask changed the task: %+v", row.Values)
				}
			},
		},
		{
			name: "reply",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
				f.startTask(t, scope, testTask, 1, testExecution)
				if _, err := f.task.Ask(context.Background(), f.askUnit(t, scope, testTask, 2, f.receipt("task.ask", "auth-reply-setup", contentHashA))); err != nil {
					t.Fatalf("setup ask: %v", err)
				}
			},
			apply: func(t *testing.T, f *fixture, scope tenancy.Scope) error {
				unit := f.replyUnit(scope, testTask, "execution-2", 3, 1, 1, f.receipt("task.reply", "auth-reply", contentHashA))
				unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.reply", testPrincipal, engineerRequires())}
				f.setMembershipRoles(t, scope, "membership-1", 1, tenancy.RoleViewer)
				_, err := f.task.Reply(context.Background(), unit)
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if _, ok := f.storedRow(t, scope, "executions", "execution-2"); ok {
					t.Fatal("a refused reply committed an execution")
				}
				if f.storedTask(t, scope, testTask).Values["question_id"] == nil {
					t.Fatal("a refused reply cleared the question")
				}
			},
		},
		{
			name: "cancel",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
				f.createTask(t, scope, testTask)
				f.startTask(t, scope, testTask, 1, testExecution)
			},
			apply: func(t *testing.T, f *fixture, scope tenancy.Scope) error {
				unit := f.cancelUnit(t, scope, testTask, 2, f.receipt("task.cancel", "auth-cancel", contentHashA))
				unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.cancel", testPrincipal, engineerRequires())}
				f.setMembershipRoles(t, scope, "membership-1", 1, tenancy.RoleViewer)
				_, err := f.task.Cancel(context.Background(), unit)
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				row := f.storedTask(t, scope, testTask)
				if textValue(row, "state") != string(task.StateOpen) || versionValue(row, "lifecycle_version") != 2 {
					t.Fatalf("a refused cancel changed the task: %+v", row.Values)
				}
				execution, ok := f.storedRow(t, scope, "executions", string(testExecution))
				if !ok || versionValue(execution, "version") != 1 {
					t.Fatalf("a refused cancel changed the execution: %+v", execution.Values)
				}
			},
		},
		{
			name: "edit",
			setup: func(f *fixture, scope tenancy.Scope) {
				f.createTask(t, scope, testTask)
			},
			apply: func(t *testing.T, f *fixture, scope tenancy.Scope) error {
				unit := task.EditUnit{
					Owner: scope, TaskID: testTask, Kind: task.EditContent, ExpectedContentVersion: 1,
					Title: "changed", Receipt: f.receipt("task.edit", "auth-edit", contentHashA),
				}
				unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.edit", testPrincipal, engineerRequires())}
				f.setMembershipRoles(t, scope, "membership-1", 1, tenancy.RoleViewer)
				_, err := f.task.Edit(context.Background(), unit)
				return err
			},
			check: func(t *testing.T, f *fixture, scope tenancy.Scope) {
				if title := textValue(f.storedTask(t, scope, testTask), "title"); title != "Fix the build" {
					t.Fatalf("a refused edit changed the title to %q", title)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			scope := testScope()
			test.setup(fixture, scope)
			fixture.seedMembership(t, scope, "membership-1", testPrincipal, 1, tenancy.RoleEngineer)
			before := fixture.commitCounts(t, scope)
			mustClass(t, test.apply(t, fixture, scope), fault.Forbidden)
			test.check(t, fixture, scope)
			fixture.requireUnchanged(t, scope, before)
		})
	}
}

// TestTaskStartRefusesRevokedMembership deletes the membership row after the
// start unit was built; the pinned membership lock no longer resolves and the
// commit is refused without any row change.
func TestTaskStartRefusesRevokedMembership(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.seedMembership(t, scope, "membership-1", testPrincipal, 1, tenancy.RoleEngineer)
	unit := fixture.startUnit(scope, testTask, testExecution, 1, 1, fixture.receipt("task.start", "auth-revoked", contentHashA))
	unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.start", testPrincipal, engineerRequires())}
	before := fixture.commitCounts(t, scope)
	fixture.deleteMembership(t, scope, "membership-1", 1)
	mustClass(t, func() error {
		_, err := fixture.task.Start(context.Background(), unit)
		return err
	}(), fault.Forbidden)
	if _, ok := fixture.storedRow(t, scope, "executions", string(testExecution)); ok {
		t.Fatal("a refused start committed an execution")
	}
	fixture.requireUnchanged(t, scope, before)
}

// TestTaskReplyRefusesChangedPredecessorExecutionVersion proves the reply unit's
// execution_version authority read is pinned: a predecessor whose version
// advanced after the unit was built cannot authorize the reply.
func TestTaskReplyRefusesChangedPredecessorExecutionVersion(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.createTask(t, scope, testTask)
	fixture.startTask(t, scope, testTask, 1, testExecution)
	if _, err := fixture.task.Ask(context.Background(), fixture.askUnit(t, scope, testTask, 2, fixture.receipt("task.ask", "reply-version-setup", contentHashA))); err != nil {
		t.Fatalf("setup ask: %v", err)
	}
	unit := fixture.replyUnit(scope, testTask, "execution-2", 3, 1, 1, fixture.receipt("task.reply", "reply-version", contentHashA))
	unit.Authority = []authorize.AuthorityRead{{
		Operation:       "task.reply",
		Required:        engineerRequires(),
		Check:           authorize.CheckExecutionVersion,
		Scope:           scope,
		PrincipalID:     testPrincipal,
		TargetID:        string(testExecution),
		ExpectedVersion: 2,
	}}
	// Advance the cancelled predecessor after the reply unit observed it.
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            string(testExecution),
			ExpectVersion: 2,
			Values:        map[string]any{"state": "cancelled"},
		}},
	}); err != nil {
		t.Fatalf("advance predecessor: %v", err)
	}
	before := fixture.commitCounts(t, scope)
	mustClass(t, func() error {
		_, err := fixture.task.Reply(context.Background(), unit)
		return err
	}(), fault.VersionConflict)
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-2"); ok {
		t.Fatal("a refused reply committed a new execution")
	}
	if fixture.storedTask(t, scope, testTask).Values["question_id"] == nil {
		t.Fatal("a refused reply cleared the question")
	}
	fixture.requireUnchanged(t, scope, before)
}

// TestPipelineChangeRefusesDemotedPushServiceAdmin proves the conditional Admin
// read is evaluated at the commit boundary: a principal that still holds
// Engineer but lost Admin cannot apply a push-trigger pipeline change.
func TestPipelineChangeRefusesDemotedPushServiceAdmin(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("v1"), 1)
	fixture.seedMembership(t, scope, "membership-1", testPrincipal, 1, tenancy.RoleEngineer, tenancy.RoleAdmin)
	unit := fixture.changeUnit(scope, pipeline.ChangeUpdate, testPipeline, "build", []byte("v2"), 1, fixture.receipt("pipeline.update", "auth-push-admin", contentHashA))
	unit.Authority = []authorize.AuthorityRead{
		membershipRead(scope, "pipeline.update", testPrincipal, engineerRequires()),
		{
			Operation:   "pipeline.update",
			Required:    engineerRequires(),
			Check:       authorize.CheckPushServiceAdmin,
			Scope:       scope,
			PrincipalID: testPrincipal,
		},
	}
	before := fixture.commitCounts(t, scope)
	// Demote to Engineer only: membership still passes, push Admin does not.
	fixture.setMembershipRoles(t, scope, "membership-1", 1, tenancy.RoleEngineer)
	mustClass(t, func() error {
		_, err := fixture.pipelines.Change(context.Background(), unit)
		return err
	}(), fault.Forbidden)
	row, ok := fixture.storedRow(t, scope, "pipelines", string(testPipeline))
	if !ok || versionValue(row, "version") != 1 || string(bytesValue(row, "yaml")) != "v1" {
		t.Fatalf("a refused push-admin change modified the pipeline: %+v", row.Values)
	}
	fixture.requireUnchanged(t, scope, before)
}

package work

// In-transaction vectors for the conditional operator retry on
// task.start. The commit boundary, not the grant stage, is the authority: it
// re-reads the task state and the named current execution and refuses an
// operator-only unit that is not open+failed or that omits the conditional
// check entirely.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// operatorRequires is the task.start grant set: the operator is a
// human-only conditional grant and the service grant stays engineer.
func operatorRequires() authorize.Requires {
	return authorize.Requires{
		HumanRoles:   []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator},
		ServiceRoles: []tenancy.Role{tenancy.RoleEngineer},
	}
}

// operatorStartRead builds the conditional read the task service emits for an
// operator-only retry.
func operatorStartRead(scope tenancy.Scope, principal id.PrincipalID, executionID id.ExecutionID, version uint64) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Operation:       "task.start",
		Required:        operatorRequires(),
		Check:           authorize.CheckFailedTaskOperatorStart,
		Scope:           scope,
		PrincipalID:     principal,
		TargetID:        string(executionID),
		ExpectedVersion: version,
	}
}

// seedOperatorOpenTask inserts one open task whose current execution row has
// the supplied state and version. An empty execution id seeds no pointer.
func (f *fixture) seedOperatorOpenTask(t *testing.T, scope tenancy.Scope, taskID id.TaskID, executionID id.ExecutionID, state string, version uint64) {
	t.Helper()
	values := map[string]any{
		"content_version":   int64(1),
		"lifecycle_version": int64(2),
		"state":             string(task.StateOpen),
		"initiator_id":      testPrincipal,
		"title":             "operator retry",
		"created_at":        testNow,
	}
	if executionID != "" {
		values["current_execution_id"] = string(executionID)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "tasks",
			ID:     string(taskID),
			Insert: true,
			Values: values,
		}},
	}); err != nil {
		t.Fatalf("seed open task: %v", err)
	}
	if executionID != "" {
		// The execution principal is distinct from the acting principal so the
		// membership read is not mistaken for an execution principal.
		f.seedExecutionForPrincipal(t, scope, executionID, taskID, "execution-principal-1", state, version)
	}
}

// operatorStartUnit builds one start unit carrying the operator grant and the
// conditional read (unless omitConditional is set).
func (f *fixture) operatorStartUnit(t *testing.T, scope tenancy.Scope, executionID id.ExecutionID, current id.ExecutionID, currentVersion uint64, omitConditional bool) task.StartUnit {
	t.Helper()
	unit := f.startUnit(scope, testTask, executionID, 2, 1, f.receipt("task.start", "operator-start-"+string(executionID), contentHashA))
	unit.Initiator = testPrincipal
	unit.Authority = []authorize.AuthorityRead{
		membershipRead(scope, "task.start", testPrincipal, operatorRequires()),
	}
	if !omitConditional {
		unit.Authority = append(unit.Authority, operatorStartRead(scope, testPrincipal, current, currentVersion))
	}
	return unit
}

// TestOperatorStartCommitsOnlyOpenFailedCurrentExecution is the positive case:
// an operator-only unit carrying the conditional read commits when the task is
// open and its current execution is failed.
func TestOperatorStartCommitsOnlyOpenFailedCurrentExecution(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 3)

	result, err := fixture.task.Start(context.Background(), fixture.operatorStartUnit(t, scope, "execution-retry", testExecution, 3, false))
	if err != nil {
		t.Fatalf("operator retry start: %v", err)
	}
	if result.ExecutionID != "execution-retry" || result.LifecycleVersion != 3 {
		t.Fatalf("start result = %+v; want the queued retry at lifecycle 3", result)
	}
	row := fixture.storedTask(t, scope, testTask)
	if textValue(row, "current_execution_id") != "execution-retry" {
		t.Fatalf("task current execution = %q; want the queued retry", textValue(row, "current_execution_id"))
	}
}

// TestOperatorStartRefusesNonFailedCurrentExecution proves every non-failed
// current execution refuses forbidden, including absent rows.
func TestOperatorStartRefusesNonFailedCurrentExecution(t *testing.T) {
	cases := []struct {
		name            string
		state           string
		version         uint64
		executionID     id.ExecutionID
		deleteExecution bool
	}{
		{name: "running", state: string(kernel.ExecutionRunning), version: 3, executionID: testExecution},
		{name: "succeeded", state: string(kernel.ExecutionSucceeded), version: 3, executionID: testExecution},
		{name: "cancelled", state: string(kernel.ExecutionCancelled), version: 3, executionID: testExecution},
		{name: "no execution", executionID: ""},
		{name: "absent execution row", executionID: "execution-missing", version: 3, deleteExecution: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t)
			scope := testScope()
			fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
			fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
			fixture.seedOperatorOpenTask(t, scope, testTask, testCase.executionID, testCase.state, testCase.version)
			if testCase.deleteExecution {
				if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
					Writes: []store.Write{{
						Table:         "executions",
						ID:            string(testCase.executionID),
						Delete:        true,
						ExpectVersion: testCase.version,
					}},
				}); err != nil {
					t.Fatalf("delete execution: %v", err)
				}
			}

			_, err := fixture.task.Start(context.Background(), fixture.operatorStartUnit(t, scope, "execution-retry", testCase.executionID, testCase.version, false))
			mustClass(t, err, fault.Forbidden)
			if _, ok := fixture.storedRow(t, scope, "executions", "execution-retry"); ok {
				t.Fatal("a refused operator retry committed an execution")
			}
		})
	}
}

// TestOperatorStartRefusesNonOpenTask proves a waiting/done/cancelled task is
// forbidden even when its historical execution is failed.
func TestOperatorStartRefusesNonOpenTask(t *testing.T) {
	for _, state := range []task.State{task.StateWaiting, task.StateDone, task.StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			fixture := newFixture(t)
			scope := testScope()
			fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
			fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
			fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 3)
			if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
				Writes: []store.Write{{
					Table:         "tasks",
					ID:            string(testTask),
					ExpectVersion: 0,
					Expect:        map[string]any{"lifecycle_version": int64(2)},
					Values:        map[string]any{"state": string(state), "lifecycle_version": int64(3)},
				}},
			}); err != nil {
				t.Fatalf("move task state: %v", err)
			}
			unit := fixture.operatorStartUnit(t, scope, "execution-retry", testExecution, 3, false)
			unit.ExpectedLifecycleVersion = 3
			_, err := fixture.task.Start(context.Background(), unit)
			mustClass(t, err, fault.Forbidden)
		})
	}
}

// TestOperatorStartRefusesMovedExecutionVersion proves the conditional read is
// a version pin: an execution that advanced after the observation is a
// version_conflict, never a silent retry.
func TestOperatorStartRefusesMovedExecutionVersion(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 3)

	// The service observed version 2; the row already advanced to 3.
	_, err := fixture.task.Start(context.Background(), fixture.operatorStartUnit(t, scope, "execution-retry", testExecution, 2, false))
	mustClass(t, err, fault.VersionConflict)
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-retry"); ok {
		t.Fatal("a stale operator retry committed an execution")
	}
}

// TestOperatorStartRefusesMovedCurrentExecution proves a task whose current
// execution pointer changed after the observation is a version_conflict.
func TestOperatorStartRefusesMovedCurrentExecution(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 3)

	// The read names an execution that is no longer the task's current one.
	unit := fixture.operatorStartUnit(t, scope, "execution-retry", "execution-old", 3, false)
	_, err := fixture.task.Start(context.Background(), unit)
	mustClass(t, err, fault.VersionConflict)
}

// TestOperatorStartRequiresTheConditionalRead is the fail-closed proof: a
// service that grants the operator role but omits the conditional read cannot
// start. The store re-reads the actor's current roles and refuses an
// operator-only unit that carries no failed-task check.
func TestOperatorStartRequiresTheConditionalRead(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 3)

	_, err := fixture.task.Start(context.Background(), fixture.operatorStartUnit(t, scope, "execution-retry", testExecution, 3, true))
	mustClass(t, err, fault.Forbidden)
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-retry"); ok {
		t.Fatal("an operator-only start without the conditional check committed an execution")
	}
}

// TestEngineerStartNeedsNoConditionalRead proves the engineer grant is
// unchanged: an engineer-only unit without the conditional read still starts an
// open task with no execution.
func TestEngineerStartNeedsNoConditionalRead(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.seedMembership(t, scope, "membership-engineer", testPrincipal, 1, tenancy.RoleEngineer)
	fixture.createTask(t, scope, testTask)

	unit := fixture.startUnit(scope, testTask, "execution-engineer", 1, 1, fixture.receipt("task.start", "engineer-start", contentHashA))
	unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.start", testPrincipal, operatorRequires())}
	if _, err := fixture.task.Start(context.Background(), unit); err != nil {
		t.Fatalf("engineer start without the conditional read: %v", err)
	}
}

// TestOperatorStartGuardRefusesNoInitiatingPrincipal proves a unit that declares
// task.start but carries no task.start membership read fails closed instead of
// silently skipping the conditional-grant guard.
func TestOperatorStartGuardRefusesNoInitiatingPrincipal(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	reads := []authorize.AuthorityRead{
		newStartRead(scope, authorize.CheckTaskVersion, "", 0),
	}
	mustClass(t, requireOperatorStartCondition(context.Background(), fixture.store, fixture.store, scope, reads), fault.Forbidden)
}

// newStartRead builds one task.start authority read for the guard tests.
func newStartRead(scope tenancy.Scope, check authorize.AuthorityCheck, target string, expected uint64) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Operation:       "task.start",
		Required:        operatorRequires(),
		Check:           check,
		Scope:           scope,
		PrincipalID:     testPrincipal,
		TargetID:        target,
		ExpectedVersion: expected,
	}
}

// failingExecutor fails every read so the guard's actor-role read error path is
// exercised.
type failingExecutor struct{ err error }

func (executor failingExecutor) Execute(context.Context, tenancy.Scope, store.Unit) (store.UnitResult, error) {
	return store.UnitResult{}, executor.err
}

func (executor failingExecutor) Read(context.Context, tenancy.Scope, string, string) (store.Row, error) {
	return store.Row{}, executor.err
}

// TestOperatorStartGuardPropagatesActorRoleReadFailure proves an unreadable
// current-role fact is returned, never swallowed as an allow.
func TestOperatorStartGuardPropagatesActorRoleReadFailure(t *testing.T) {
	scope := testScope()
	want := fault.New(fault.DependencyUnavailable, "membership read failed")
	reads := []authorize.AuthorityRead{
		newStartRead(scope, authorize.CheckMembership, "", 0),
	}
	err := requireOperatorStartCondition(context.Background(), failingExecutor{err: want}, nil, scope, reads)
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("guard error = %v; want the actor-role read failure to propagate", err)
	}
}

// TestEngineerAndOperatorStoreStartNeedsNoConditionalRead proves the store
// guard treats an engineer-and-operator actor as an engineer: the conditional
// failed-task read is not required for a fresh start.
func TestEngineerAndOperatorStoreStartNeedsNoConditionalRead(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.seedMembership(t, scope, "membership-engineer-operator", testPrincipal, 1, tenancy.RoleEngineer, tenancy.RoleOperator)
	fixture.createTask(t, scope, testTask)

	unit := fixture.startUnit(scope, testTask, "execution-engineer-operator", 1, 1, fixture.receipt("task.start", "engineer-operator-start", contentHashA))
	unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "task.start", testPrincipal, operatorRequires())}
	if _, err := fixture.task.Start(context.Background(), unit); err != nil {
		t.Fatalf("engineer+operator start without the conditional read: %v", err)
	}
}

// TestOperatorStartRefusesMissingTaskRow proves a missing task row is a
// version_conflict (the task the observed execution named is gone), never a
// forbidden that could be mistaken for a retryable state.
func TestOperatorStartRefusesMissingTaskRow(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
	resolver := authorityResolver{executor: fixture.store, scanner: fixture.store}
	read := operatorStartRead(scope, testPrincipal, testExecution, 3)
	_, err := resolver.failedTaskOperatorStartLocks(context.Background(), read, authorityContext{TaskID: testTask})
	mustClass(t, err, fault.VersionConflict)
}

// TestOperatorStartRefusesMissingExecutionRow proves a task that names the
// observed current execution but whose execution row is gone is forbidden.
func TestOperatorStartRefusesMissingExecutionRow(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 3)
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            string(testExecution),
			Delete:        true,
			ExpectVersion: 3,
		}},
	}); err != nil {
		t.Fatalf("delete execution: %v", err)
	}
	resolver := authorityResolver{executor: fixture.store, scanner: fixture.store}
	read := operatorStartRead(scope, testPrincipal, testExecution, 3)
	_, err := resolver.failedTaskOperatorStartLocks(context.Background(), read, authorityContext{TaskID: testTask})
	mustClass(t, err, fault.Forbidden)
}

// TestOperatorStartReplayLookupIsOperationScoped proves the pre-flight receipt
// lookup is scoped by operation. A receipt for the same principal and caller key
// but another operation must not be mistaken for this task.start retry: a fresh
// operator start at a running current execution still fails closed.
func TestOperatorStartReplayLookupIsOperationScoped(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	fixture.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionRunning), 3)

	other := fixture.receipt("task.create", "shared-caller", contentHashB)
	other.Result = []byte(`{}`)
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{Receipt: &other}); err != nil {
		t.Fatalf("seed other-operation receipt: %v", err)
	}

	unit := fixture.startUnit(scope, testTask, "execution-retry", 2, 1, fixture.receipt("task.start", "shared-caller", contentHashA))
	unit.Initiator = testPrincipal
	unit.Authority = []authorize.AuthorityRead{
		membershipRead(scope, "task.start", testPrincipal, operatorRequires()),
		operatorStartRead(scope, testPrincipal, testExecution, 3),
	}
	_, err := fixture.task.Start(context.Background(), unit)
	mustClass(t, err, fault.Forbidden)
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-retry"); ok {
		t.Fatal("a cross-operation receipt admitted a fresh operator start")
	}
}

// seedOperatorRetryAtRunning seeds the operator-only membership and an open
// task whose current execution is RUNNING, so a fresh operator start is refused
// unless the pre-flight receipt lookup wrongly classifies it as a replay.
func (f *fixture) seedOperatorRetryAtRunning(t *testing.T, scope tenancy.Scope) {
	t.Helper()
	f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	f.seedMembership(t, scope, "membership-operator", testPrincipal, 1, tenancy.RoleOperator)
	f.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionRunning), 3)
}

// seedReplayReceipt commits one complete receipt in the supplied scope with the
// supplied identity fields, so a scoping test can prove the pre-flight lookup
// ignores it. A complete receipt is the strongest false positive: the executor
// would serve it if the lookup ever selected it.
func (f *fixture) seedReplayReceipt(t *testing.T, scope tenancy.Scope, principal id.PrincipalID, operation, callerKey string) {
	t.Helper()
	record := f.receiptFor(scope, operation, callerKey, contentHashB)
	record.Key.PrincipalID = principal
	record.Result = []byte(`{}`)
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{Receipt: &record}); err != nil {
		t.Fatalf("seed %s receipt: %v", operation, err)
	}
}

// assertFreshOperatorStartForbidden sends a fresh operator start at the running
// current execution and proves the pre-flight lookup did not mistake a foreign
// receipt for this retry: the start is forbidden and commits nothing.
func (f *fixture) assertFreshOperatorStartForbidden(t *testing.T, scope tenancy.Scope, callerKey string) {
	t.Helper()
	unit := f.startUnit(scope, testTask, "execution-retry", 2, 1, f.receipt("task.start", callerKey, contentHashA))
	unit.Initiator = testPrincipal
	unit.Authority = []authorize.AuthorityRead{
		membershipRead(scope, "task.start", testPrincipal, operatorRequires()),
		operatorStartRead(scope, testPrincipal, testExecution, 3),
	}
	_, err := f.task.Start(context.Background(), unit)
	mustClass(t, err, fault.Forbidden)
	if _, ok := f.storedRow(t, scope, "executions", "execution-retry"); ok {
		t.Fatal("a foreign receipt admitted a fresh operator start")
	}
}

// TestOperatorStartReplayLookupIsPrincipalScoped proves the pre-flight receipt
// lookup is scoped by principal. An engineer commits a task.start receipt under
// caller key "shared-caller"; an operator-only principal then sends a fresh
// task.start with the same key for an open task whose current execution is
// RUNNING. The other principal's receipt must not take the replay path, so the
// fresh start still fails closed and commits no execution.
func TestOperatorStartReplayLookupIsPrincipalScoped(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedOperatorRetryAtRunning(t, scope)
	fixture.seedReplayReceipt(t, scope, "principal-someone-else", "task.start", "shared-caller")
	fixture.assertFreshOperatorStartForbidden(t, scope, "shared-caller")
}

// TestOperatorStartReplayLookupIsProjectScoped proves the pre-flight receipt
// lookup is scoped by tenant and project. A receipt with the same principal,
// operation and caller key in another project or tenant must not take the
// replay path.
func TestOperatorStartReplayLookupIsProjectScoped(t *testing.T) {
	cases := []struct {
		name  string
		scope tenancy.Scope
	}{
		{name: "another project", scope: otherScope()},
		{name: "another tenant", scope: tenancy.Scope{TenantID: "tenant-2", ProjectID: testProject}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t)
			scope := testScope()
			fixture.seedOperatorRetryAtRunning(t, scope)
			fixture.seedReplayReceipt(t, testCase.scope, testPrincipal, "task.start", "shared-caller")
			fixture.assertFreshOperatorStartForbidden(t, scope, "shared-caller")
		})
	}
}

// TestOperatorStartReplayLookupIsCallerKeyScoped proves the pre-flight receipt
// lookup is scoped by caller key. A receipt of the same principal and operation
// under a different caller key must not take the replay path.
func TestOperatorStartReplayLookupIsCallerKeyScoped(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedOperatorRetryAtRunning(t, scope)
	fixture.seedReplayReceipt(t, scope, testPrincipal, "task.start", "other-caller")
	fixture.assertFreshOperatorStartForbidden(t, scope, "shared-caller")
}

// TestOperatorStartRefusesUnversionedObservedExecution proves a present current
// execution row with no version is invalid_input, never an unguarded pin.
func TestOperatorStartRefusesUnversionedObservedExecution(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 0)
	resolver := authorityResolver{executor: fixture.store, scanner: fixture.store}
	read := operatorStartRead(scope, testPrincipal, testExecution, 3)
	_, err := resolver.failedTaskOperatorStartLocks(context.Background(), read, authorityContext{TaskID: testTask})
	mustClass(t, err, fault.InvalidInput)
}

// TestOperatorStartRefusesUnversionedRead proves a conditional read with no
// observed version is invalid_input, never a version-conflict wildcard.
func TestOperatorStartRefusesUnversionedRead(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedOperatorOpenTask(t, scope, testTask, testExecution, string(kernel.ExecutionFailed), 3)
	resolver := authorityResolver{executor: fixture.store, scanner: fixture.store}
	read := operatorStartRead(scope, testPrincipal, testExecution, 0)
	_, err := resolver.failedTaskOperatorStartLocks(context.Background(), read, authorityContext{TaskID: testTask})
	mustClass(t, err, fault.InvalidInput)
}

package executionstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// membershipRead is one in-commit current-membership read for the given role.
func membershipRead(scope tenancy.Scope, principal id.PrincipalID, roles ...tenancy.Role) []authorize.AuthorityRead {
	return []authorize.AuthorityRead{{
		Operation:   "pipeline.start",
		Required:    authorize.Requires{HumanRoles: roles},
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
	}}
}

// serviceEnabledRead is one in-commit current-service read for an exact row.
func serviceEnabledRead(scope tenancy.Scope, principal id.PrincipalID, serviceID string) []authorize.AuthorityRead {
	return []authorize.AuthorityRead{{
		Operation:   "execution.request.resolve",
		Check:       authorize.CheckServiceEnabled,
		Scope:       scope,
		PrincipalID: principal,
		TargetID:    serviceID,
	}}
}

// seedMembership writes one committed membership row in the given scope.
func seedMembership(t *testing.T, backend *memory.Store, scope tenancy.Scope, rowID, principal string, roles []tenancy.Role, version uint64) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal roles: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:  "memberships",
		ID:     rowID,
		Insert: true,
		Values: map[string]any{
			"principal_id": principal,
			"roles":        encoded,
			"version":      int64(version),
			"created_at":   testNow,
		},
	}}}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// replaceMembershipRoles changes the committed roles of one membership row,
// advancing its version.
func replaceMembershipRoles(t *testing.T, backend *memory.Store, scope tenancy.Scope, rowID string, version uint64, roles []tenancy.Role) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal roles: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         "memberships",
		ID:            rowID,
		ExpectVersion: version,
		Values:        map[string]any{"roles": encoded},
	}}}); err != nil {
		t.Fatalf("replace membership roles: %v", err)
	}
}

// deleteMembership removes one membership row, the strongest revocation.
func deleteMembership(t *testing.T, backend *memory.Store, scope tenancy.Scope, rowID string, version uint64) {
	t.Helper()
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         "memberships",
		ID:            rowID,
		Delete:        true,
		ExpectVersion: version,
	}}}); err != nil {
		t.Fatalf("delete membership: %v", err)
	}
}

// seedService writes one committed service row in the given scope.
func seedService(t *testing.T, backend *memory.Store, scope tenancy.Scope, serviceID, state string, version uint64) {
	t.Helper()
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:  "services",
		ID:     serviceID,
		Insert: true,
		Values: map[string]any{
			"name":       serviceID,
			"state":      state,
			"roles":      []byte("[]"),
			"version":    int64(version),
			"created_at": testNow,
		},
	}}}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

// disableService changes one committed service row to disabled.
func disableService(t *testing.T, backend *memory.Store, scope tenancy.Scope, serviceID string, version uint64) {
	t.Helper()
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         "services",
		ID:            serviceID,
		ExpectVersion: version,
		Values:        map[string]any{"state": "disabled"},
	}}}); err != nil {
		t.Fatalf("disable service: %v", err)
	}
}

// TestStartRefusesRevokedMembershipWithoutPersisting proves the Start port
// re-asserts the membership fact it was built with.
func TestStartRefusesRevokedMembershipWithoutPersisting(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)

	unit := startUnit(scope, "exec-1", "start-revoked", containerAction())
	unit.Authority = membershipRead(scope, "user-1", tenancy.RoleEngineer)
	deleteMembership(t, backend, scope, "membership-1", 1)

	_, err := adapter.Start(context.Background(), unit)
	requireClass(t, err, fault.Forbidden)
	if _, err := adapter.Load(context.Background(), scope, "exec-1"); err == nil {
		t.Fatal("a revoked membership must not commit the execution")
	}
	if _, ok := backend.Snapshot(scope, "events", "event-start-revoked"); ok {
		t.Fatal("a revoked membership must not commit the event intent")
	}
	if _, ok := backend.Snapshot(scope, "receipts", "receipt-start-revoked"); ok {
		t.Fatal("a revoked membership must not commit the receipt")
	}

	// Positive control: a current Engineer membership commits.
	seedMembership(t, backend, scope, "membership-2", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
	allowed := startUnit(scope, "exec-ok", "start-allowed", containerAction())
	allowed.Authority = membershipRead(scope, "user-1", tenancy.RoleEngineer)
	if _, err := adapter.Start(context.Background(), allowed); err != nil {
		t.Fatalf("current membership Start: %v", err)
	}
}

// TestStartRefusesDemotedMembershipWithoutPersisting proves a demotion (the row
// still exists but no longer grants the required role) is refused.
func TestStartRefusesDemotedMembershipWithoutPersisting(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)

	unit := startUnit(scope, "exec-1", "start-demoted", containerAction())
	unit.Authority = membershipRead(scope, "user-1", tenancy.RoleEngineer)
	replaceMembershipRoles(t, backend, scope, "membership-1", 1, []tenancy.Role{tenancy.RoleViewer})

	_, err := adapter.Start(context.Background(), unit)
	requireClass(t, err, fault.Forbidden)
	if _, err := adapter.Load(context.Background(), scope, "exec-1"); err == nil {
		t.Fatal("a demoted membership must not commit the execution")
	}
}

// TestStartRefusesDisabledServiceWithoutPersisting proves the service_enabled
// guard asserts state, version and the tenant-wide scope fallback.
func TestStartRefusesDisabledServiceWithoutPersisting(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}
	seedPipeline(t, backend, scope, 3)
	seedService(t, backend, tenantScope, "service-1", "enabled", 1)

	unit := startUnit(scope, "exec-1", "start-service", containerAction())
	unit.Authority = serviceEnabledRead(scope, "service-1", "service-1")
	disableService(t, backend, tenantScope, "service-1", 1)

	_, err := adapter.Start(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, err := adapter.Load(context.Background(), scope, "exec-1"); err == nil {
		t.Fatal("a disabled service must not commit the execution")
	}

	// Positive control: the tenant-wide enabled service is visible and guarded.
	seedService(t, backend, tenantScope, "service-2", "enabled", 1)
	allowed := startUnit(scope, "exec-ok", "start-service-ok", containerAction())
	allowed.Authority = serviceEnabledRead(scope, "service-2", "service-2")
	if _, err := adapter.Start(context.Background(), allowed); err != nil {
		t.Fatalf("enabled tenant-wide service Start: %v", err)
	}
}

// TestTransitionRefusesRevokedMembershipWithoutPersisting proves the Transition
// port re-asserts the membership fact it was built with.
func TestTransitionRefusesRevokedMembershipWithoutPersisting(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	actions := startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)

	cancel := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionCancelled,
		NextCurrentAction:        new(-1),
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		PendingCancellations:     []flow.ActionUpdate{{ActionID: actions[0], ExpectedVersion: 1, NextState: kernel.ActionCancelled}},
		At:                       testNow,
	}
	unit := transitionUnit(scope, "exec-1", cancel, "cancel-revoked")
	unit.Authority = membershipRead(scope, "user-1", tenancy.RoleEngineer)
	deleteMembership(t, backend, scope, "membership-1", 1)

	_, err := adapter.Transition(context.Background(), unit)
	requireClass(t, err, fault.Forbidden)
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Version != 1 || aggregate.State != kernel.ExecutionQueued || aggregate.Actions[0].State != kernel.ActionPending {
		t.Fatalf("revoked membership changed state: %+v", aggregate)
	}
	if _, ok := backend.Snapshot(scope, "events", "event-cancel-revoked"); ok {
		t.Fatal("revoked membership must not publish the event intent")
	}

	// Positive control: a current membership commits the same transition.
	seedMembership(t, backend, scope, "membership-2", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
	allowed := transitionUnit(scope, "exec-1", cancel, "cancel-allowed")
	allowed.Authority = membershipRead(scope, "user-1", tenancy.RoleEngineer)
	if _, err := adapter.Transition(context.Background(), allowed); err != nil {
		t.Fatalf("current membership Transition: %v", err)
	}
}

// TestActivateRefusesAChangedExecutionWithoutPersisting proves the
// execution_running guard is asserted at the commit boundary.
func TestActivateRefusesAChangedExecutionWithoutPersisting(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	if _, err := adapter.Start(context.Background(), startUnit(scope, "exec-1", "start-activate", requestOnlyAction())); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The engine marks the execution running before activating its request.
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         "executions",
		ID:            "exec-1",
		ExpectVersion: 1,
		Values:        map[string]any{"state": string(kernel.ExecutionRunning)},
	}}}); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	action := snapshotdoc.DeriveActionID("exec-1", 0)
	unit := activationUnit(scope, "exec-1", action, "activate-changed")
	unit.ExpectedExecutionVersion = 2
	unit.Authority = []authorize.AuthorityRead{
		{Check: authorize.CheckExecutionRunning, Scope: scope, PrincipalID: "engine", TargetID: "exec-1"},
		{Check: authorize.CheckExecutionVersion, Scope: scope, PrincipalID: "engine", TargetID: "exec-1", ExpectedVersion: 2},
	}
	// The execution is cancelled after the unit was built.
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         "executions",
		ID:            "exec-1",
		ExpectVersion: 2,
		Values:        map[string]any{"state": string(kernel.ExecutionCancelled)},
	}}}); err != nil {
		t.Fatalf("cancel execution: %v", err)
	}

	_, err := adapter.Activate(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Actions[0].State != kernel.ActionPending {
		t.Fatalf("refused activation changed the action: %+v", aggregate.Actions[0])
	}
	if _, ok := backend.Snapshot(scope, "receipts", "receipt-activate-changed"); ok {
		t.Fatal("refused activation must not commit the receipt")
	}

	// Positive control: a fresh running execution activates.
	if _, err := adapter.Start(context.Background(), startUnit(scope, "exec-2", "start-activate-ok", requestOnlyAction())); err != nil {
		t.Fatalf("Start exec-2: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:         "executions",
		ID:            "exec-2",
		ExpectVersion: 1,
		Values:        map[string]any{"state": string(kernel.ExecutionRunning)},
	}}}); err != nil {
		t.Fatalf("mark exec-2 running: %v", err)
	}
	allowed := activationUnit(scope, "exec-2", snapshotdoc.DeriveActionID("exec-2", 0), "activate-allowed")
	allowed.ExpectedExecutionVersion = 2
	allowed.Authority = []authorize.AuthorityRead{
		{Check: authorize.CheckExecutionRunning, Scope: scope, PrincipalID: "engine", TargetID: "exec-2"},
		{Check: authorize.CheckExecutionVersion, Scope: scope, PrincipalID: "engine", TargetID: "exec-2", ExpectedVersion: 2},
	}
	if _, err := adapter.Activate(context.Background(), allowed); err != nil {
		t.Fatalf("running Activate: %v", err)
	}
}

// TestResolveRefusesRevokedMembershipWithoutPersisting proves the Resolve port
// re-asserts the assignment membership fact it was built with.
func TestResolveRefusesRevokedMembershipWithoutPersisting(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleReviewer}, 1)

	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-revoked", "e", testNow)
	unit.Authority = membershipRead(scope, "user-1", tenancy.RoleReviewer)
	deleteMembership(t, backend, scope, "membership-1", 1)

	_, err := adapter.Resolve(context.Background(), unit)
	requireClass(t, err, fault.Forbidden)
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Version != 2 || aggregate.Actions[0].State != kernel.ActionRunning || aggregate.Actions[0].Request.Answer != nil {
		t.Fatalf("revoked membership changed state: %+v", aggregate)
	}
	if _, ok := backend.Snapshot(scope, "events", "event-resolve-revoked"); ok {
		t.Fatal("revoked membership must not publish the event intent")
	}

	// Positive control: a current Reviewer membership settles the request.
	seedMembership(t, backend, scope, "membership-2", "user-1", []tenancy.Role{tenancy.RoleReviewer}, 1)
	allowed := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-allowed", "f", testNow)
	allowed.Authority = membershipRead(scope, "user-1", tenancy.RoleReviewer)
	if _, err := adapter.Resolve(context.Background(), allowed); err != nil {
		t.Fatalf("current membership Resolve: %v", err)
	}
}

// TestResolveRefusesDisabledServiceWithoutPersisting proves the service
// assignment check asserts the exact service row and its enabled state.
func TestResolveRefusesDisabledServiceWithoutPersisting(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	seedService(t, backend, scope, "service-1", "enabled", 1)

	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-service", "e", testNow)
	unit.Assignment = request.Assignment{Kind: request.AssignmentService, ServiceID: "service-1"}
	unit.Authority = serviceEnabledRead(scope, "service-1", "service-1")
	disableService(t, backend, scope, "service-1", 1)

	_, err := adapter.Resolve(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Version != 2 || aggregate.Actions[0].Request.Answer != nil {
		t.Fatalf("disabled service changed state: %+v", aggregate)
	}

	// Positive control: an enabled service settles the request.
	seedService(t, backend, scope, "service-2", "enabled", 1)
	allowed := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-service-ok", "f", testNow)
	allowed.Assignment = request.Assignment{Kind: request.AssignmentService, ServiceID: "service-2"}
	allowed.Authority = serviceEnabledRead(scope, "service-2", "service-2")
	if _, err := adapter.Resolve(context.Background(), allowed); err != nil {
		t.Fatalf("enabled service Resolve: %v", err)
	}
}

// TestResolveRefusesAnExpiredActionDeadlineThroughAuthority proves the
// action_deadline authority read adds the transaction-clock deadline guard.
func TestResolveRefusesAnExpiredActionDeadlineThroughAuthority(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")

	unit := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-deadline", "e", testNow)
	unit.Authority = []authorize.AuthorityRead{{
		Check:       authorize.CheckActionDeadline,
		Scope:       scope,
		PrincipalID: "user-1",
		TargetID:    string(action),
	}}
	clock = testNow.Add(60 * time.Second) // the persisted deadline: equality expires

	_, err := adapter.Resolve(context.Background(), unit)
	requireClass(t, err, fault.DeadlineExpired)
	aggregate := loadExecution(t, adapter, scope, "exec-1")
	if aggregate.Version != 2 || aggregate.Actions[0].State != kernel.ActionRunning || aggregate.Actions[0].Request.Answer != nil {
		t.Fatalf("expired deadline changed state: %+v", aggregate)
	}

	// Positive control: before the deadline the same authority permits the
	// settlement.
	clock = testNow
	allowed := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-deadline-ok", "f", testNow)
	allowed.Authority = unit.Authority
	if _, err := adapter.Resolve(context.Background(), allowed); err != nil {
		t.Fatalf("live deadline Resolve: %v", err)
	}
}

// TestResolveReplayRefusesRevokedMembership proves the answered-replay branch
// still evaluates unit.Authority before returning a settled receipt.
func TestResolveReplayRefusesRevokedMembership(t *testing.T) {
	clock := testNow
	adapter, backend := newStoreWithClock(t, func() time.Time { return clock })
	scope := projectScope()
	action := startedRequestExecution(t, adapter, backend, scope, "exec-1")
	seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleReviewer}, 1)

	first := firstResolveUnit(t, adapter, scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-replay-auth", "e", testNow)
	first.Authority = membershipRead(scope, "user-1", tenancy.RoleReviewer)
	settled, err := adapter.Resolve(context.Background(), first)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	deleteMembership(t, backend, scope, "membership-1", 1)
	replay := replayResolveUnit(scope, "exec-1", action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, "resolve-replay-auth", "e", testNow.Add(time.Minute))
	replay.Authority = membershipRead(scope, "user-1", tenancy.RoleReviewer)
	_, err = adapter.Resolve(context.Background(), replay)
	requireClass(t, err, fault.Forbidden)

	seedMembership(t, backend, scope, "membership-2", "user-1", []tenancy.Role{tenancy.RoleReviewer}, 1)
	replayed, err := adapter.Resolve(context.Background(), replay)
	if err != nil {
		t.Fatalf("authorized replay Resolve: %v", err)
	}
	if !replayed.Replayed || replayed.ExecutionVersion != settled.ExecutionVersion || replayed.ActionVersion != settled.ActionVersion {
		t.Fatalf("replay = %+v; want %+v replayed", replayed, settled)
	}
}

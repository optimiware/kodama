package executionstore

// Membership-union authority vectors for the execution adapter.
//
// The adapter confirms a human membership against BOTH applicable rows: the
// tenant-wide row of the caller's tenant and the exact-project row. Their
// union is the effective grant, so a project row can only add roles and never
// shadows a tenant-wide grant (and vice versa). Both observed rows are pinned
// into the commit unit: a present row by memberships.id identity AND version,
// an absent row by a deliberate absence guard on its unique key. A change to
// either row between confirmation and the commit is a version_conflict, and a
// current union that no longer grants the required role is forbidden.
//
// These tests use the real execution and request services over the memory
// store, and hand-built pinned units where the confirmation/commit gap has to
// be exercised deterministically.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// humanActorAs is one enabled human principal with an explicit identity and
// session roles. The session roles authorize the service operation; the
// adapter re-reads the committed membership union at commit.
func humanActorAs(principal id.PrincipalID, scope tenancy.Scope, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      principal,
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
}

// requestActionForRole is one request action whose admitted assignment names an
// explicit role, so the commit-time assignment authority requires that role.
func requestActionForRole(index int, name, role string) pipeline.AdmittedAction {
	action := approvalAction(index, name)
	action.Request.Assignment = "role/" + role
	return action
}

// reconcileThroughEngine activates the first pending request exactly as the
// Kernel reconciler does.
func reconcileThroughEngine(t *testing.T, backend *memory.Store, executionID id.ExecutionID) kernel.Execution {
	t.Helper()
	reconciler := engine.New(NewEngine(backend.Controller()), unusedRuntime{}, retainedWorkspace{}, integrationClock{now: testNow})
	aggregate, err := reconciler.Reconcile(context.Background(), executionID)
	if err != nil {
		t.Fatalf("engine.Reconcile: %v", err)
	}
	return aggregate
}

// TestMembershipUnionThroughRealServices proves the effective grant is the
// union of the tenant-wide and exact-project rows for the real service paths.
func TestMembershipUnionThroughRealServices(t *testing.T) {
	ctx := context.Background()

	t.Run("bob resolves a reviewer request", func(t *testing.T) {
		backend := memory.NewWithClock(func() time.Time { return testNow })
		adapter := New(backend, backend)
		scope := projectScope()
		seedPipeline(t, backend, scope, 3)
		// The starter holds the pipeline.start Engineer right.
		seedMembership(t, backend, scope, "membership-starter", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		// Bob is tenant-wide Engineer and project Reviewer.
		seedMembership(t, backend, tenancy.Scope{TenantID: scope.TenantID}, "membership-tenant-bob", "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, scope, "membership-project-bob", "principal-bob", []tenancy.Role{tenancy.RoleReviewer}, 1)

		ids := &integrationIDs{}
		execService := newRealExecutionService(t, adapter, ids)
		requestService := newRealRequestService(t, adapter, ids)
		start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{approvalAction(0, "approve")}, "start-bob-resolve")
		actionID := snapshotdoc.DeriveActionID(start.ExecutionID, 0)
		activated := reconcileThroughEngine(t, backend, start.ExecutionID)

		result, err := requestService.Resolve(ctx, request.ResolveCommand{
			Actor:                    humanActorAs("principal-bob", scope, tenancy.RoleReviewer),
			Scope:                    scope,
			ExecutionID:              start.ExecutionID,
			ActionID:                 actionID,
			ExpectedExecutionVersion: activated.Version,
			ExpectedActionVersion:    activated.Actions[0].Version,
			Decision:                 request.DecisionPass,
			Response:                 kernel.Outputs{"decision": "yes"},
		})
		if err != nil {
			t.Fatalf("Bob (tenant Engineer + project Reviewer) resolve: %v", err)
		}
		if result.Replayed {
			t.Fatal("first settlement replayed")
		}
	})

	t.Run("bob cancels with the tenant engineer grant beside a project reviewer row", func(t *testing.T) {
		backend := memory.NewWithClock(func() time.Time { return testNow })
		adapter := New(backend, backend)
		scope := projectScope()
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, scope, "membership-starter", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, tenancy.Scope{TenantID: scope.TenantID}, "membership-tenant-bob", "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, scope, "membership-project-bob", "principal-bob", []tenancy.Role{tenancy.RoleReviewer}, 1)

		ids := &integrationIDs{}
		execService := newRealExecutionService(t, adapter, ids)
		start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{approvalAction(0, "approve")}, "start-bob-cancel")

		// The project Reviewer row shadows nothing: the tenant-wide Engineer row
		// still grants execution.cancel.
		if _, err := execService.Cancel(ctx, execution.CancelCommand{
			Actor:                    humanActorAs("principal-bob", scope, tenancy.RoleEngineer),
			Scope:                    scope,
			ExecutionID:              start.ExecutionID,
			ExpectedExecutionVersion: 1,
			CallerKey:                "cancel-bob",
			Content:                  receipt.Content{MediaType: "application/json", Bytes: []byte(`{"cancel":"bob"}`)},
		}); err != nil {
			t.Fatalf("Bob (tenant Engineer + project Reviewer) cancel: %v", err)
		}
		aggregate := loadExecution(t, adapter, scope, start.ExecutionID)
		if aggregate.State != kernel.ExecutionCancelled {
			t.Fatalf("cancel state = %q; want cancelled", aggregate.State)
		}
	})

	t.Run("carol keeps tenant admin beside a project viewer row", func(t *testing.T) {
		backend := memory.NewWithClock(func() time.Time { return testNow })
		adapter := New(backend, backend)
		scope := projectScope()
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, scope, "membership-starter", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, tenancy.Scope{TenantID: scope.TenantID}, "membership-tenant-carol", "principal-carol", []tenancy.Role{tenancy.RoleAdmin}, 1)
		seedMembership(t, backend, scope, "membership-project-carol", "principal-carol", []tenancy.Role{tenancy.RoleViewer}, 1)

		ids := &integrationIDs{}
		execService := newRealExecutionService(t, adapter, ids)
		requestService := newRealRequestService(t, adapter, ids)
		start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{requestActionForRole(0, "admin-approve", "admin")}, "start-carol-admin")
		actionID := snapshotdoc.DeriveActionID(start.ExecutionID, 0)
		activated := reconcileThroughEngine(t, backend, start.ExecutionID)

		// The commit-time assignment authority requires Admin; the project
		// Viewer row must not narrow Carol's tenant-wide Admin grant.
		if _, err := requestService.Resolve(ctx, request.ResolveCommand{
			Actor:                    humanActorAs("principal-carol", scope, tenancy.RoleAdmin),
			Scope:                    scope,
			ExecutionID:              start.ExecutionID,
			ActionID:                 actionID,
			ExpectedExecutionVersion: activated.Version,
			ExpectedActionVersion:    activated.Actions[0].Version,
			Decision:                 request.DecisionPass,
			Response:                 kernel.Outputs{"decision": "yes"},
		}); err != nil {
			t.Fatalf("Carol (tenant Admin + project Viewer) resolve Admin request: %v", err)
		}
	})
}

// membershipPin is the observed shape of one committed membership row.
type membershipPin struct {
	rowID   string
	scope   tenancy.Scope
	roles   []tenancy.Role
	version uint64
}

// pinnedMembershipRead builds one membership authority read whose sampled rows
// are pinned exactly as a service that read both rows would pin them.
func pinnedMembershipRead(scope tenancy.Scope, principal id.PrincipalID, required []tenancy.Role, tenant, project membershipPin) []authorize.AuthorityRead {
	tenantMembership := &tenancy.Membership{
		Scope:       tenant.scope,
		PrincipalID: principal,
		Roles:       append([]tenancy.Role(nil), tenant.roles...),
		Version:     tenant.version,
	}
	var projectMembership *tenancy.Membership
	if project.rowID != "" {
		projectMembership = &tenancy.Membership{
			Scope:       project.scope,
			PrincipalID: principal,
			Roles:       append([]tenancy.Role(nil), project.roles...),
			Version:     project.version,
		}
	}
	return []authorize.AuthorityRead{{
		Operation:   "pipeline.start",
		Required:    authorize.Requires{HumanRoles: required},
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
		Targets: authorize.MembershipTargets(
			scope,
			tenantMembership,
			projectMembership,
			tenant.rowID,
			project.rowID,
		),
	}}
}

// TestMembershipUnionPinsBothRows proves both observed rows are locked by row
// identity AND version, and that a change to EITHER row between confirmation
// and commit is a version_conflict that persists nothing.
func TestMembershipUnionPinsBothRows(t *testing.T) {
	ctx := context.Background()
	scope := projectScope()
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}

	newPinnedStore := func(t *testing.T) (*Store, *memory.Store) {
		t.Helper()
		adapter, backend := newStore(t)
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, tenantScope, "membership-tenant", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, scope, "membership-project", "user-1", []tenancy.Role{tenancy.RoleReviewer}, 1)
		return adapter, backend
	}
	pinned := func() []authorize.AuthorityRead {
		return pinnedMembershipRead(
			scope, "user-1", []tenancy.Role{tenancy.RoleEngineer},
			membershipPin{rowID: "membership-tenant", scope: tenantScope, roles: []tenancy.Role{tenancy.RoleEngineer}, version: 1},
			membershipPin{rowID: "membership-project", scope: scope, roles: []tenancy.Role{tenancy.RoleReviewer}, version: 1},
		)
	}

	t.Run("unchanged rows commit", func(t *testing.T) {
		adapter, _ := newPinnedStore(t)
		unit := startUnit(scope, "exec-pinned-ok", "start-pinned-ok", containerAction())
		unit.Authority = pinned()
		if _, err := adapter.Start(ctx, unit); err != nil {
			t.Fatalf("unchanged pinned union Start: %v", err)
		}
	})

	cases := []struct {
		name   string
		mutate func(t *testing.T, backend *memory.Store)
	}{
		{
			name: "tenant row version bumped",
			mutate: func(t *testing.T, backend *memory.Store) {
				replaceMembershipRoles(t, backend, tenantScope, "membership-tenant", 1, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer})
			},
		},
		{
			name: "project row version bumped",
			mutate: func(t *testing.T, backend *memory.Store) {
				replaceMembershipRoles(t, backend, scope, "membership-project", 1, []tenancy.Role{tenancy.RoleReviewer, tenancy.RoleViewer})
			},
		},
		{
			name: "project row revoked",
			mutate: func(t *testing.T, backend *memory.Store) {
				deleteMembership(t, backend, scope, "membership-project", 1)
			},
		},
		{
			name: "tenant row revoked",
			mutate: func(t *testing.T, backend *memory.Store) {
				deleteMembership(t, backend, tenantScope, "membership-tenant", 1)
			},
		},
		{
			name: "project row deleted and recreated at the same version",
			mutate: func(t *testing.T, backend *memory.Store) {
				deleteMembership(t, backend, scope, "membership-project", 1)
				seedMembership(t, backend, scope, "membership-project-recreated", "user-1", []tenancy.Role{tenancy.RoleReviewer}, 1)
			},
		},
		{
			name: "tenant row deleted and recreated at the same version",
			mutate: func(t *testing.T, backend *memory.Store) {
				deleteMembership(t, backend, tenantScope, "membership-tenant", 1)
				seedMembership(t, backend, tenantScope, "membership-tenant-recreated", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			adapter, backend := newPinnedStore(t)
			unit := startUnit(scope, "exec-pinned", "start-pinned", containerAction())
			unit.Authority = pinned()
			test.mutate(t, backend)

			_, err := adapter.Start(ctx, unit)
			requireClass(t, err, fault.VersionConflict)
			if _, err := adapter.Load(ctx, scope, "exec-pinned"); err == nil {
				t.Fatal("a changed membership pin must not commit the execution")
			}
			if _, ok := backend.Snapshot(scope, "events", "event-start-pinned"); ok {
				t.Fatal("a changed membership pin must not commit the event intent")
			}
			if _, ok := backend.Snapshot(scope, "receipts", "receipt-start-pinned"); ok {
				t.Fatal("a changed membership pin must not commit the receipt")
			}
		})
	}
}

// TestMembershipUnionPinsAbsence proves a row that was absent at confirmation
// is guarded by its unique key, so its creation before commit is a
// version_conflict.
func TestMembershipUnionPinsAbsence(t *testing.T) {
	ctx := context.Background()
	scope := projectScope()
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}

	t.Run("absent project row created", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, tenantScope, "membership-tenant", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		unit := startUnit(scope, "exec-absent-project", "start-absent-project", containerAction())
		unit.Authority = pinnedMembershipRead(
			scope, "user-1", []tenancy.Role{tenancy.RoleEngineer},
			membershipPin{rowID: "membership-tenant", scope: tenantScope, roles: []tenancy.Role{tenancy.RoleEngineer}, version: 1},
			membershipPin{},
		)
		seedMembership(t, backend, scope, "membership-project-new", "user-1", []tenancy.Role{tenancy.RoleReviewer}, 1)

		_, err := adapter.Start(ctx, unit)
		requireClass(t, err, fault.VersionConflict)
		if _, err := adapter.Load(ctx, scope, "exec-absent-project"); err == nil {
			t.Fatal("a project row created after confirmation must not commit")
		}
		if _, ok := backend.Snapshot(scope, "events", "event-start-absent-project"); ok {
			t.Fatal("a project row created after confirmation must not commit the event intent")
		}
	})

	t.Run("absent tenant row created", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, scope, "membership-project", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		unit := startUnit(scope, "exec-absent-tenant", "start-absent-tenant", containerAction())
		unit.Authority = pinnedMembershipRead(
			scope, "user-1", []tenancy.Role{tenancy.RoleEngineer},
			membershipPin{},
			membershipPin{rowID: "membership-project", scope: scope, roles: []tenancy.Role{tenancy.RoleEngineer}, version: 1},
		)
		seedMembership(t, backend, tenantScope, "membership-tenant-new", "user-1", []tenancy.Role{tenancy.RoleViewer}, 1)

		_, err := adapter.Start(ctx, unit)
		requireClass(t, err, fault.VersionConflict)
		if _, err := adapter.Load(ctx, scope, "exec-absent-tenant"); err == nil {
			t.Fatal("a tenant row created after confirmation must not commit")
		}
	})

	t.Run("absent rows that stay absent commit", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, scope, "membership-project", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		unit := startUnit(scope, "exec-absent-ok", "start-absent-ok", containerAction())
		unit.Authority = pinnedMembershipRead(
			scope, "user-1", []tenancy.Role{tenancy.RoleEngineer},
			membershipPin{},
			membershipPin{rowID: "membership-project", scope: scope, roles: []tenancy.Role{tenancy.RoleEngineer}, version: 1},
		)
		if _, err := adapter.Start(ctx, unit); err != nil {
			t.Fatalf("unchanged absence pin Start: %v", err)
		}
	})
}

// TestProjectOnlyMembershipGrantsNoTenantRight proves a tenant-scoped
// membership read observes only the tenant-wide row, so a project-only
// membership never grants a tenant-scoped right even when its roles would.
func TestProjectOnlyMembershipGrantsNoTenantRight(t *testing.T) {
	ctx := context.Background()
	scope := projectScope()
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}

	read := []authorize.AuthorityRead{{
		Operation:   "membership.list",
		Required:    authorize.Requires{HumanRoles: []tenancy.Role{tenancy.RoleAdmin}},
		Check:       authorize.CheckMembership,
		Scope:       tenantScope,
		PrincipalID: "user-1",
	}}

	t.Run("project-only admin is not a tenant admin", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedMembership(t, backend, scope, "membership-project", "user-1", []tenancy.Role{tenancy.RoleAdmin}, 1)
		_, err := adapter.authorityLocks(ctx, read)
		requireClass(t, err, fault.Forbidden)
	})

	t.Run("project admin does not add to a tenant viewer", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedMembership(t, backend, tenantScope, "membership-tenant", "user-1", []tenancy.Role{tenancy.RoleViewer}, 1)
		seedMembership(t, backend, scope, "membership-project", "user-1", []tenancy.Role{tenancy.RoleAdmin}, 1)
		_, err := adapter.authorityLocks(ctx, read)
		requireClass(t, err, fault.Forbidden)
	})

	t.Run("tenant admin grants the tenant right", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedMembership(t, backend, tenantScope, "membership-tenant", "user-1", []tenancy.Role{tenancy.RoleAdmin}, 1)
		if _, err := adapter.authorityLocks(ctx, read); err != nil {
			t.Fatalf("tenant Admin tenant-scoped read: %v", err)
		}
	})
}

// mutatingExecutor runs one mutation against the committed store immediately
// before it delegates a commit, widening the adapter's read-to-commit gap into
// a deterministic concurrent change.
type mutatingExecutor struct {
	store.Executor
	mutate func()
}

func (executor *mutatingExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if executor.mutate != nil {
		mutate := executor.mutate
		executor.mutate = nil
		mutate()
	}
	return executor.Executor.Execute(ctx, scope, unit)
}

// TestMembershipUnionLocksRefuseChangeBeforeCommit proves the adapter's own
// unpinned read still pins both rows as locks, so a revocation or creation
// racing the commit is a version_conflict through the real service path.
func TestMembershipUnionLocksRefuseChangeBeforeCommit(t *testing.T) {
	ctx := context.Background()
	scope := projectScope()
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}

	t.Run("project row revoked before commit", func(t *testing.T) {
		backend := memory.NewWithClock(func() time.Time { return testNow })
		executor := &mutatingExecutor{Executor: backend}
		adapter := New(executor, backend)
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, scope, "membership-starter", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, tenantScope, "membership-tenant-bob", "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, scope, "membership-project-bob", "principal-bob", []tenancy.Role{tenancy.RoleReviewer}, 1)

		ids := &integrationIDs{}
		execService := newRealExecutionService(t, adapter, ids)
		start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{approvalAction(0, "approve")}, "start-race-revoke")
		executor.mutate = func() { deleteMembership(t, backend, scope, "membership-project-bob", 1) }

		_, err := execService.Cancel(ctx, execution.CancelCommand{
			Actor:                    humanActorAs("principal-bob", scope, tenancy.RoleEngineer),
			Scope:                    scope,
			ExecutionID:              start.ExecutionID,
			ExpectedExecutionVersion: 1,
			CallerKey:                "cancel-race-revoke",
			Content:                  receipt.Content{MediaType: "application/json", Bytes: []byte(`{"cancel":"race"}`)},
		})
		requireClass(t, err, fault.VersionConflict)
		aggregate := loadExecution(t, adapter, scope, start.ExecutionID)
		if aggregate.Version != 1 || aggregate.State != kernel.ExecutionQueued {
			t.Fatalf("revoked membership changed state: %+v", aggregate)
		}
		if _, ok := backend.Snapshot(scope, "events", "event-cancel-race-revoke"); ok {
			t.Fatal("revoked membership must not publish the cancel event intent")
		}
	})

	t.Run("absent tenant row created before commit", func(t *testing.T) {
		backend := memory.NewWithClock(func() time.Time { return testNow })
		executor := &mutatingExecutor{Executor: backend}
		adapter := New(executor, backend)
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, scope, "membership-starter", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, scope, "membership-project-bob", "principal-bob", []tenancy.Role{tenancy.RoleEngineer}, 1)

		ids := &integrationIDs{}
		execService := newRealExecutionService(t, adapter, ids)
		start := startThroughService(t, execService, scope, []pipeline.AdmittedAction{approvalAction(0, "approve")}, "start-race-absent")
		executor.mutate = func() {
			seedMembership(t, backend, tenantScope, "membership-tenant-new", "principal-bob", []tenancy.Role{tenancy.RoleViewer}, 1)
		}

		_, err := execService.Cancel(ctx, execution.CancelCommand{
			Actor:                    humanActorAs("principal-bob", scope, tenancy.RoleEngineer),
			Scope:                    scope,
			ExecutionID:              start.ExecutionID,
			ExpectedExecutionVersion: 1,
			CallerKey:                "cancel-race-absent",
			Content:                  receipt.Content{MediaType: "application/json", Bytes: []byte(`{"cancel":"absent"}`)},
		})
		requireClass(t, err, fault.VersionConflict)
		aggregate := loadExecution(t, adapter, scope, start.ExecutionID)
		if aggregate.Version != 1 || aggregate.State != kernel.ExecutionQueued {
			t.Fatalf("created tenant row changed state: %+v", aggregate)
		}
	})
}

// TestMembershipUnionRefusalPersistsNothing proves a current union that no
// longer grants the required role refuses with forbidden and commits nothing.
func TestMembershipUnionRefusalPersistsNothing(t *testing.T) {
	ctx := context.Background()
	adapter, backend := newStore(t)
	scope := projectScope()
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}
	seedPipeline(t, backend, scope, 3)
	seedMembership(t, backend, tenantScope, "membership-tenant", "user-1", []tenancy.Role{tenancy.RoleViewer}, 1)
	seedMembership(t, backend, scope, "membership-project", "user-1", []tenancy.Role{tenancy.RoleViewer}, 1)

	unit := startUnit(scope, "exec-forbidden", "start-forbidden", containerAction())
	unit.Authority = membershipRead(scope, "user-1", tenancy.RoleEngineer)

	_, err := adapter.Start(ctx, unit)
	requireClass(t, err, fault.Forbidden)
	if _, err := adapter.Load(ctx, scope, "exec-forbidden"); err == nil {
		t.Fatal("a non-granting union must not commit the execution")
	}
	if _, ok := backend.Snapshot(scope, "events", "event-start-forbidden"); ok {
		t.Fatal("a non-granting union must not commit the event intent")
	}
	if _, ok := backend.Snapshot(scope, "receipts", "receipt-start-forbidden"); ok {
		t.Fatal("a non-granting union must not commit the receipt")
	}

	// The flow commit path is unaffected by the adapter's refusal: the same
	// shape commits once either row grants the required role.
	replaceMembershipRoles(t, backend, scope, "membership-project", 1, []tenancy.Role{tenancy.RoleEngineer})
	allowed := startUnit(scope, "exec-forbidden-ok", "start-forbidden-ok", containerAction())
	allowed.Authority = membershipRead(scope, "user-1", tenancy.RoleEngineer)
	if _, err := adapter.Start(ctx, allowed); err != nil {
		t.Fatalf("granting union Start: %v", err)
	}
}

// seedServiceWithRoles writes one committed service row with an explicit role
// set, so the service branch of the membership check can be exercised.
func seedServiceWithRoles(t *testing.T, backend *memory.Store, scope tenancy.Scope, serviceID, state string, version uint64, roles []tenancy.Role) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal service roles: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:  "services",
		ID:     serviceID,
		Insert: true,
		Values: map[string]any{
			"name":       serviceID,
			"state":      state,
			"roles":      encoded,
			"version":    int64(version),
			"created_at": testNow,
		},
	}}}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

// membershipSweepRead is one commit-time membership read for a sweep principal.
func membershipSweepRead(scope tenancy.Scope, principal id.PrincipalID, service bool) []authorize.AuthorityRead {
	read := authorize.AuthorityRead{
		Operation:   "pipeline.start",
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
		Required:    authorize.Requires{HumanRoles: []tenancy.Role{tenancy.RoleEngineer}},
	}
	if service {
		read.Required = authorize.Requires{ServiceRoles: []tenancy.Role{tenancy.RoleEngineer}}
	}
	return []authorize.AuthorityRead{read}
}

// membershipSweepPrincipal is one principal kind the membership check can see.
type membershipSweepPrincipal struct {
	name      string
	principal id.PrincipalID
	service   bool
	seed      func(t *testing.T, adapter *Store, backend *memory.Store, scope tenancy.Scope)
	want      fault.Class // empty means the operation must commit
}

// TestMembershipPrincipalKindsAcrossEveryPort runs every principal kind
// through every membership-bearing commit port (Start, Transition, Resolve,
// Activate).
//
// An execution principal holds no membership roles, so a membership authority
// read for it is forbidden on EVERY port and commits nothing; it must never
// pass silently with no lock. The human union and service rules are the
// positive/negative controls.
func TestMembershipPrincipalKindsAcrossEveryPort(t *testing.T) {
	ctx := context.Background()
	cases := []membershipSweepPrincipal{
		{
			name:      "human with project engineer",
			principal: "user-1",
			seed: func(t *testing.T, _ *Store, backend *memory.Store, scope tenancy.Scope) {
				seedMembership(t, backend, scope, "membership-project", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
			},
		},
		{
			name:      "human with tenant engineer only",
			principal: "user-1",
			seed: func(t *testing.T, _ *Store, backend *memory.Store, scope tenancy.Scope) {
				seedMembership(t, backend, tenancy.Scope{TenantID: scope.TenantID}, "membership-tenant", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
			},
		},
		{
			name:      "human with viewer only",
			principal: "user-1",
			seed: func(t *testing.T, _ *Store, backend *memory.Store, scope tenancy.Scope) {
				seedMembership(t, backend, scope, "membership-project", "user-1", []tenancy.Role{tenancy.RoleViewer}, 1)
			},
			want: fault.Forbidden,
		},
		{
			name:      "human with an empty role row",
			principal: "user-1",
			seed: func(t *testing.T, _ *Store, backend *memory.Store, scope tenancy.Scope) {
				seedMembership(t, backend, scope, "membership-project", "user-1", nil, 1)
			},
			want: fault.Forbidden,
		},
		{
			name:      "human with no row",
			principal: "principal-ghost",
			want:      fault.Forbidden,
		},
		{
			name:      "human only in another tenant",
			principal: "user-1",
			seed: func(t *testing.T, _ *Store, backend *memory.Store, _ tenancy.Scope) {
				seedMembership(t, backend, tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-2"}, "membership-other", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
			},
			want: fault.Forbidden,
		},
		{
			name:      "enabled service with engineer",
			principal: "service-1",
			service:   true,
			seed: func(t *testing.T, _ *Store, backend *memory.Store, scope tenancy.Scope) {
				seedServiceWithRoles(t, backend, tenancy.Scope{TenantID: scope.TenantID}, "service-1", "enabled", 1, []tenancy.Role{tenancy.RoleEngineer})
			},
		},
		{
			name:      "disabled service with engineer",
			principal: "service-1",
			service:   true,
			seed: func(t *testing.T, _ *Store, backend *memory.Store, scope tenancy.Scope) {
				seedServiceWithRoles(t, backend, tenancy.Scope{TenantID: scope.TenantID}, "service-1", "disabled", 1, []tenancy.Role{tenancy.RoleEngineer})
			},
			want: fault.Forbidden,
		},
		{
			name:      "running execution principal",
			principal: "exec-principal-1",
			seed: func(t *testing.T, adapter *Store, backend *memory.Store, scope tenancy.Scope) {
				// The principal's execution is running; a membership read for
				// it must still be forbidden because executions hold ExecNone.
				startedRequestExecution(t, adapter, backend, scope, "exec-sweep-fixture")
			},
			want: fault.Forbidden,
		},
	}

	operations := []string{"Start", "Transition", "Resolve", "Activate"}
	for _, operation := range operations {
		t.Run(operation, func(t *testing.T) {
			for _, principalCase := range cases {
				t.Run(principalCase.name, func(t *testing.T) {
					adapter, backend := newStoreWithClock(t, func() time.Time { return testNow })
					scope := projectScope()
					if principalCase.seed != nil {
						principalCase.seed(t, adapter, backend, scope)
					}
					executionID, receiptKey, err := runMembershipSweepPort(t, adapter, backend, scope, operation, principalCase)
					if principalCase.want == "" {
						if err != nil {
							t.Fatalf("%s for %s: %v", operation, principalCase.name, err)
						}
						return
					}
					requireClass(t, err, principalCase.want)
					if _, ok := backend.Snapshot(scope, "receipts", "receipt-"+receiptKey); ok {
						t.Fatalf("%s refusal committed the receipt %q", operation, "receipt-"+receiptKey)
					}
					if _, ok := backend.Snapshot(scope, "events", "event-"+receiptKey); ok {
						t.Fatalf("%s refusal committed the event %q", operation, "event-"+receiptKey)
					}
					if operation == "Start" {
						if _, err := adapter.Load(ctx, scope, executionID); err == nil {
							t.Fatalf("%s refusal committed the execution %q", operation, executionID)
						}
					}
				})
			}
		})
	}
}

// runMembershipSweepPort prepares one port's natural fixture, attaches a
// membership read for the sweep principal, and runs the port. It returns the
// target execution identity and the receipt key so a refusal can prove nothing
// committed.
func runMembershipSweepPort(t *testing.T, adapter *Store, backend *memory.Store, scope tenancy.Scope, operation string, principalCase membershipSweepPrincipal) (id.ExecutionID, string, error) {
	t.Helper()
	ctx := context.Background()
	key := "sweep-" + operation + "-" + string(principalCase.principal)
	switch operation {
	case "Start":
		seedPipeline(t, backend, scope, 3)
		executionID := id.ExecutionID("exec-" + key)
		unit := startUnit(scope, executionID, key, containerAction())
		unit.Authority = membershipSweepRead(scope, principalCase.principal, principalCase.service)
		_, err := adapter.Start(ctx, unit)
		return executionID, key, err
	case "Transition":
		executionID := id.ExecutionID("exec-" + key)
		actions := startedExecution(t, adapter, backend, scope, executionID, containerAction())
		cancel := flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: 1,
			NextExecutionState:       kernel.ExecutionCancelled,
			NextCurrentAction:        new(-1),
			Output:                   flow.OutputChange{Action: flow.OutputDiscard},
			PendingCancellations:     []flow.ActionUpdate{{ActionID: actions[0], ExpectedVersion: 1, NextState: kernel.ActionCancelled}},
			At:                       testNow,
		}
		unit := transitionUnit(scope, executionID, cancel, key)
		unit.Authority = membershipSweepRead(scope, principalCase.principal, principalCase.service)
		_, err := adapter.Transition(ctx, unit)
		return executionID, key, err
	case "Resolve":
		executionID := id.ExecutionID("exec-" + key)
		action := startedRequestExecution(t, adapter, backend, scope, executionID)
		unit := firstResolveUnit(t, adapter, scope, executionID, action, request.DecisionPass, kernel.Outputs{"decision": "yes"}, key, "e", testNow)
		unit.Authority = membershipSweepRead(scope, principalCase.principal, principalCase.service)
		_, err := adapter.Resolve(ctx, unit)
		return executionID, key, err
	case "Activate":
		seedPipeline(t, backend, scope, 3)
		executionID := id.ExecutionID("exec-" + key)
		if _, err := adapter.Start(ctx, startUnit(scope, executionID, "start-"+key, requestOnlyAction())); err != nil {
			t.Fatalf("seed activation execution: %v", err)
		}
		// The engine marks the execution running before activating its request.
		if _, err := backend.Execute(ctx, scope, store.Unit{Writes: []store.Write{{
			Table:         "executions",
			ID:            string(executionID),
			ExpectVersion: 1,
			Values:        map[string]any{"state": string(kernel.ExecutionRunning)},
		}}}); err != nil {
			t.Fatalf("mark sweep execution running: %v", err)
		}
		unit := activationUnit(scope, executionID, snapshotdoc.DeriveActionID(executionID, 0), key)
		unit.ExpectedExecutionVersion = 2
		unit.Authority = membershipSweepRead(scope, principalCase.principal, principalCase.service)
		_, err := adapter.Activate(ctx, unit)
		return executionID, key, err
	default:
		t.Fatalf("unknown sweep operation %q", operation)
		return "", "", nil
	}
}

// TestPushServiceAdminUnionPassesAndLocksBothRows proves CheckPushServiceAdmin
// evaluates the two-row membership union rather than preferring a project row
// in isolation: a tenant-wide Admin beside a project Engineer passes and locks
// both rows.
func TestPushServiceAdminUnionPassesAndLocksBothRows(t *testing.T) {
	ctx := context.Background()
	scope := projectScope()
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}
	reads := []authorize.AuthorityRead{{
		Operation:   "pipeline.apply",
		Check:       authorize.CheckPushServiceAdmin,
		Scope:       scope,
		PrincipalID: "user-admin",
	}}

	t.Run("tenant-wide Admin plus project Engineer passes with two-row locks", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedMembership(t, backend, tenantScope, "membership-tenant-admin", "user-admin", []tenancy.Role{tenancy.RoleAdmin}, 2)
		seedMembership(t, backend, scope, "membership-project-eng", "user-admin", []tenancy.Role{tenancy.RoleEngineer}, 3)

		locks, err := adapter.authorityLocks(ctx, reads)
		if err != nil {
			t.Fatalf("authorityLocks CheckPushServiceAdmin: %v", err)
		}
		if len(locks) != 2 {
			t.Fatalf("locks = %+v; want 2 membership locks", locks)
		}
		if locks[0].Table != tableMemberships || len(locks[0].IDs) != 1 || locks[0].IDs[0] != "membership-project-eng" || locks[0].ExpectVersion != 3 || locks[0].TenantWide {
			t.Fatalf("project lock = %+v; want membership-project-eng@v3 project-scoped", locks[0])
		}
		if locks[1].Table != tableMemberships || len(locks[1].IDs) != 1 || locks[1].IDs[0] != "membership-tenant-admin" || locks[1].ExpectVersion != 2 || !locks[1].TenantWide {
			t.Fatalf("tenant lock = %+v; want membership-tenant-admin@v2 tenant-wide", locks[1])
		}

		seedPipeline(t, backend, scope, 3)
		unit := startUnit(scope, "exec-push-admin-ok", "start-push-admin-ok", containerAction())
		unit.Authority = reads
		if _, err := adapter.Start(ctx, unit); err != nil {
			t.Fatalf("Start with CheckPushServiceAdmin union: %v", err)
		}
	})

	t.Run("concurrent mutation of either row conflicts", func(t *testing.T) {
		backend := memory.NewWithClock(func() time.Time { return testNow })
		executor := &mutatingExecutor{Executor: backend}
		adapter := New(executor, backend)
		seedPipeline(t, backend, scope, 3)
		seedMembership(t, backend, tenantScope, "membership-tenant-admin", "user-admin", []tenancy.Role{tenancy.RoleAdmin}, 1)
		seedMembership(t, backend, scope, "membership-project-eng", "user-admin", []tenancy.Role{tenancy.RoleEngineer}, 1)
		executor.mutate = func() {
			replaceMembershipRoles(t, backend, scope, "membership-project-eng", 1, []tenancy.Role{tenancy.RoleViewer})
		}

		unit := startUnit(scope, "exec-push-admin-race", "start-push-admin-race", containerAction())
		unit.Authority = reads
		_, err := adapter.Start(ctx, unit)
		requireClass(t, err, fault.VersionConflict)
	})

	t.Run("union without Admin and service principal are refused", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedMembership(t, backend, tenantScope, "membership-tenant-eng", "user-nonadmin", []tenancy.Role{tenancy.RoleEngineer}, 1)
		seedMembership(t, backend, scope, "membership-project-rev", "user-nonadmin", []tenancy.Role{tenancy.RoleReviewer}, 1)
		_, err := adapter.authorityLocks(ctx, []authorize.AuthorityRead{{
			Operation:   "pipeline.apply",
			Check:       authorize.CheckPushServiceAdmin,
			Scope:       scope,
			PrincipalID: "user-nonadmin",
		}})
		requireClass(t, err, fault.Forbidden)

		seedServiceWithRoles(t, backend, scope, "service-1", "enabled", 1, []tenancy.Role{tenancy.RoleEngineer})
		_, err = adapter.authorityLocks(ctx, []authorize.AuthorityRead{{
			Operation:   "pipeline.apply",
			Check:       authorize.CheckPushServiceAdmin,
			Scope:       scope,
			PrincipalID: "service-1",
		}})
		requireClass(t, err, fault.Forbidden)
	})
}

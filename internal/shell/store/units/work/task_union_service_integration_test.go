package work

// These tests drive the REAL task service onto the work adapters over the
// memory store for the membership union. Bob is tenant-wide Engineer and
// project Reviewer: under the union he keeps the engineer grant (task
// creation/edit/start/ask/cancel) AND gains the reviewer grant (reply as the
// assigned reviewer). Under a shadowing lookup the project Reviewer row would
// hide the tenant Engineer row and task creation would fail.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// seedBobUnion seeds the tenant-wide Engineer row and the exact-project
// Reviewer row for Bob, plus his authenticated human credential.
func seedBobUnion(t *testing.T, env *realServiceEnv, tenantVersion, projectVersion uint64) {
	t.Helper()
	env.fixture.seedMembership(t, tenancy.Scope{TenantID: testTenant}, "membership-tenant-bob", unionPrincipal, tenantVersion, tenancy.RoleEngineer)
	env.fixture.seedMembership(t, env.scope, "membership-project-bob", unionPrincipal, projectVersion, tenancy.RoleReviewer)
	env.registerHuman(t, unionPrincipal, tenancy.RoleEngineer, tenancy.RoleReviewer)
}

// TestRealTaskServiceUnionGrantsEveryTaskUnit drives create, content edit,
// comment, pipeline selection, start, ask, reviewer reply and cancel with Bob's
// union membership. Every unit's membership check must read both rows: the
// tenant Engineer grant enables the engineer operations while the project
// Reviewer grant enables the assigned reply.
func TestRealTaskServiceUnionGrantsEveryTaskUnit(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	seedBobUnion(t, env, 3, 7)
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	bob := env.registerHuman(t, unionPrincipal, tenancy.RoleEngineer, tenancy.RoleReviewer)

	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: bob, Scope: env.scope, Title: "Union task",
		CallerKey: "bob-create", Content: integrationContent("bob-create"),
	})
	if err != nil {
		t.Fatalf("Bob (tenant Engineer + project Reviewer) task create: %v", err)
	}
	if _, err := env.service.Edit(ctx, task.EditCommand{
		Actor: bob, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditContent,
		ExpectedContentVersion: 1, Title: "Union task edited",
		CallerKey: "bob-edit-content", Content: integrationContent("bob-edit-content"),
	}); err != nil {
		t.Fatalf("Bob task content edit: %v", err)
	}
	if _, err := env.service.Edit(ctx, task.EditCommand{
		Actor: bob, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditComment,
		ExpectedContentVersion: 2, Body: "a comment from Bob",
		CallerKey: "bob-edit-comment", Content: integrationContent("bob-edit-comment"),
	}); err != nil {
		t.Fatalf("Bob task comment: %v", err)
	}
	if _, err := env.service.Edit(ctx, task.EditCommand{
		Actor: bob, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditSelectPipeline,
		ExpectedLifecycleVersion: 1, PipelineID: testPipeline,
		CallerKey: "bob-select-pipeline", Content: integrationContent("bob-select-pipeline"),
	}); err != nil {
		t.Fatalf("Bob task select pipeline: %v", err)
	}
	if _, err := env.service.Start(ctx, task.StartCommand{
		Actor: bob, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		CallerKey: "bob-start", Content: integrationContent("bob-start"),
	}); err != nil {
		t.Fatalf("Bob task start: %v", err)
	}
	if _, err := env.service.Ask(ctx, task.AskCommand{
		Actor: bob, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 3,
		Question:   "Which branch should I target?",
		Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		CallerKey:  "bob-ask", Content: integrationContent("bob-ask"),
	}); err != nil {
		t.Fatalf("Bob task ask: %v", err)
	}
	current, err := env.fixture.task.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("read asked task: %v", err)
	}
	if current.Question == nil || current.Question.Assignment.Role != tenancy.RoleReviewer {
		t.Fatalf("outstanding question = %+v; want a reviewer assignment", current.Question)
	}
	replied, err := env.service.Reply(ctx, task.ReplyCommand{
		Actor: bob, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 4,
		QuestionID: current.Question.ID, ExpectedQuestionVersion: current.Question.Version,
		Answer: "main", CallerKey: "bob-reply", Content: integrationContent("bob-reply"),
	})
	if err != nil {
		t.Fatalf("Bob reply as the assigned project Reviewer: %v", err)
	}
	if replied.ExecutionID == "" {
		t.Fatal("Bob's reply committed no fresh execution")
	}
	if _, err := env.service.Cancel(ctx, task.CancelCommand{
		Actor: bob, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 5,
		CallerKey: "bob-cancel", Content: integrationContent("bob-cancel"),
	}); err != nil {
		t.Fatalf("Bob task cancel: %v", err)
	}
	row := env.fixture.storedTask(t, env.scope, created.TaskID)
	if state := textValue(row, "state"); state != string(task.StateCancelled) {
		t.Fatalf("final task state = %q; want cancelled", state)
	}
	if versionValue(row, "lifecycle_version") != 6 {
		t.Fatalf("final lifecycle version = %d; want 6", versionValue(row, "lifecycle_version"))
	}
}

// TestRealTaskServiceUnionCreateAndStart proves the nested create-and-start
// unit also reads both rows.
func TestRealTaskServiceUnionCreateAndStart(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	seedBobUnion(t, env, 3, 7)
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	bob := env.registerHuman(t, unionPrincipal, tenancy.RoleEngineer, tenancy.RoleReviewer)

	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: bob, Scope: env.scope, Title: "Union create-and-start", PipelineID: testPipeline, Start: true,
		CallerKey: "bob-create-and-start", Content: integrationContent("bob-create-and-start"),
	})
	if err != nil {
		t.Fatalf("Bob create-and-start: %v", err)
	}
	if created.ExecutionID == "" {
		t.Fatal("Bob's create-and-start committed no execution")
	}
}

// TestRealTaskServiceUnionDemotedTenantRowForbidden proves a union member that
// loses the required role before the port reads is refused forbidden with
// nothing committed: the project Reviewer row cannot replace the dropped
// tenant-wide Engineer grant.
func TestRealTaskServiceUnionDemotedTenantRowForbidden(t *testing.T) {
	env := newRealServiceEnv(t)
	ctx := context.Background()
	seedBobUnion(t, env, 3, 7)
	bob := env.registerHuman(t, unionPrincipal, tenancy.RoleEngineer, tenancy.RoleReviewer)
	env.setMembershipRows(t, tenancy.Scope{TenantID: testTenant}, "membership-tenant-bob", 3, tenancy.RoleViewer)

	before := env.fixture.commitCounts(t, env.scope)
	_, err := env.service.Create(ctx, task.CreateCommand{
		Actor: bob, Scope: env.scope, Title: "Must be refused",
		CallerKey: "bob-demoted-create", Content: integrationContent("bob-demoted-create"),
	})
	mustClass(t, err, fault.Forbidden)
	env.fixture.requireUnchanged(t, env.scope, before)
	if count := env.fixture.countRows(t, env.scope, "tasks", nil); count != 0 {
		t.Fatalf("a refused union create committed %d tasks", count)
	}
}

// unionMutationCase is one change committed after the port confirmed the union
// and before the unit committed.
type unionMutationCase struct {
	name   string
	setup  func(t *testing.T, env *realServiceEnv)
	mutate func(t *testing.T, env *realServiceEnv)
}

// TestRealTaskServiceUnionRefusesChangeBetweenConfirmationAndCommit proves both
// membership rows are pinned by identity AND version, and that an absent row is
// pinned as an absence. The hook commits in the confirmation-to-commit window,
// so the store must refuse with version_conflict and commit nothing.
func TestRealTaskServiceUnionRefusesChangeBetweenConfirmationAndCommit(t *testing.T) {
	cases := []unionMutationCase{
		{
			name:  "tenant role change bumps the pinned version",
			setup: func(t *testing.T, env *realServiceEnv) { seedBobUnion(t, env, 3, 7) },
			mutate: func(t *testing.T, env *realServiceEnv) {
				env.setMembershipRows(t, tenancy.Scope{TenantID: testTenant}, "membership-tenant-bob", 3, tenancy.RoleEngineer, tenancy.RoleOperator)
			},
		},
		{
			name:  "tenant row deleted and recreated at the same version",
			setup: func(t *testing.T, env *realServiceEnv) { seedBobUnion(t, env, 3, 7) },
			mutate: func(t *testing.T, env *realServiceEnv) {
				env.recreateMembershipRow(t, tenancy.Scope{TenantID: testTenant}, unionPrincipal,
					"membership-tenant-bob", 3, "membership-tenant-bob-regrant", tenancy.RoleEngineer)
			},
		},
		{
			name:  "project role change bumps the pinned version",
			setup: func(t *testing.T, env *realServiceEnv) { seedBobUnion(t, env, 3, 7) },
			mutate: func(t *testing.T, env *realServiceEnv) {
				env.setMembershipRows(t, env.scope, "membership-project-bob", 7, tenancy.RoleReviewer, tenancy.RoleOperator)
			},
		},
		{
			name:  "project row revoked",
			setup: func(t *testing.T, env *realServiceEnv) { seedBobUnion(t, env, 3, 7) },
			mutate: func(t *testing.T, env *realServiceEnv) {
				env.executeDirect(t, env.scope, store.Unit{Writes: []store.Write{{
					Table:         "memberships",
					ID:            "membership-project-bob",
					Delete:        true,
					ExpectVersion: 7,
				}}})
			},
		},
		{
			name: "absent project row created",
			setup: func(t *testing.T, env *realServiceEnv) {
				env.fixture.seedMembership(t, tenancy.Scope{TenantID: testTenant}, "membership-tenant-bob", unionPrincipal, 3, tenancy.RoleEngineer)
			},
			mutate: func(t *testing.T, env *realServiceEnv) {
				env.fixture.seedMembership(t, env.scope, "membership-project-bob", unionPrincipal, 1, tenancy.RoleViewer)
			},
		},
		{
			name: "absent tenant row created",
			setup: func(t *testing.T, env *realServiceEnv) {
				env.fixture.seedMembership(t, env.scope, "membership-project-bob", unionPrincipal, 7, tenancy.RoleEngineer)
			},
			mutate: func(t *testing.T, env *realServiceEnv) {
				env.fixture.seedMembership(t, tenancy.Scope{TenantID: testTenant}, "membership-tenant-bob", unionPrincipal, 1, tenancy.RoleViewer)
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var mutator *mutatingExecutor
			env := newRealServiceEnvWithExecutor(t, func(inner store.Executor) store.Executor {
				mutator = &mutatingExecutor{inner: inner}
				return mutator
			})
			ctx := context.Background()
			test.setup(t, env)
			bob := env.registerHuman(t, unionPrincipal, tenancy.RoleEngineer, tenancy.RoleReviewer)
			mutator.hook = func() { test.mutate(t, env) }
			before := env.fixture.commitCounts(t, env.scope)

			_, err := env.service.Create(ctx, task.CreateCommand{
				Actor: bob, Scope: env.scope, Title: "Pinned union create",
				CallerKey: "bob-pinned-" + test.name, Content: integrationContent("bob-pinned-" + test.name),
			})
			mustClass(t, err, fault.VersionConflict)
			env.fixture.requireUnchanged(t, env.scope, before)
			if count := env.fixture.countRows(t, env.scope, "tasks", nil); count != 0 {
				t.Fatalf("a refused pin change committed %d tasks", count)
			}
		})
	}
}

// TestRealTaskServiceUnionStartRefusesPinnedRowChange proves a task.start unit
// carries the same union pins: a tenant-wide role change after confirmation
// refuses the start with no execution and no task change.
func TestRealTaskServiceUnionStartRefusesPinnedRowChange(t *testing.T) {
	var mutator *mutatingExecutor
	env := newRealServiceEnvWithExecutor(t, func(inner store.Executor) store.Executor {
		mutator = &mutatingExecutor{inner: inner}
		return mutator
	})
	ctx := context.Background()
	seedBobUnion(t, env, 3, 7)
	env.fixture.seedPipeline(t, env.scope, testPipeline, "build", integrationPipelineYAML("echo v1"), 1)
	bob := env.registerHuman(t, unionPrincipal, tenancy.RoleEngineer, tenancy.RoleReviewer)
	created, err := env.service.Create(ctx, task.CreateCommand{
		Actor: bob, Scope: env.scope, Title: "Pinned start", PipelineID: testPipeline,
		CallerKey: "bob-pinned-start-create", Content: integrationContent("bob-pinned-start-create"),
	})
	if err != nil {
		t.Fatalf("setup create: %v", err)
	}
	mutator.hook = func() {
		env.setMembershipRows(t, tenancy.Scope{TenantID: testTenant}, "membership-tenant-bob", 3, tenancy.RoleEngineer, tenancy.RoleOperator)
	}
	before := env.fixture.commitCounts(t, env.scope)
	_, err = env.service.Start(ctx, task.StartCommand{
		Actor: bob, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "bob-pinned-start", Content: integrationContent("bob-pinned-start"),
	})
	mustClass(t, err, fault.VersionConflict)
	env.fixture.requireUnchanged(t, env.scope, before)
	if count := env.fixture.countRows(t, env.scope, "executions", nil); count != 0 {
		t.Fatalf("a refused pinned start committed %d executions", count)
	}
	if versionValue(env.fixture.storedTask(t, env.scope, created.TaskID), "lifecycle_version") != 1 {
		t.Fatal("a refused pinned start advanced the task lifecycle")
	}
}

//go:build component

package postgrescomponent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

const integrationImage = "registry.example.com/run@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type integrationClock struct{ now time.Time }

func (clock integrationClock) Now() time.Time { return clock.now }

type integrationIDs struct {
	mu        sync.Mutex
	task      int
	question  int
	execution int
	principal int
	event     int
	receipt   int
}

// integrationIDsSnapshot captures the deterministic counters so a replay that
// allocates speculative ids can restore them after proving no row changed.
type integrationIDsSnapshot struct {
	task, question, execution, principal, event, receipt int
}

func (ids *integrationIDs) snapshot() integrationIDsSnapshot {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	return integrationIDsSnapshot{ids.task, ids.question, ids.execution, ids.principal, ids.event, ids.receipt}
}

func (ids *integrationIDs) restore(saved integrationIDsSnapshot) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.task, ids.question, ids.execution = saved.task, saved.question, saved.execution
	ids.principal, ids.event, ids.receipt = saved.principal, saved.event, saved.receipt
}

func (ids *integrationIDs) NewTaskID() (id.TaskID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.task++
	return id.TaskID(fmt.Sprintf("task-%d", ids.task)), nil
}

func (ids *integrationIDs) NewQuestionID() (task.QuestionID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.question++
	return task.QuestionID(fmt.Sprintf("question-%d", ids.question)), nil
}

func (ids *integrationIDs) NewExecutionID() (id.ExecutionID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.execution++
	return id.ExecutionID(fmt.Sprintf("execution-%d", ids.execution)), nil
}

func (ids *integrationIDs) NewPrincipalID() (id.PrincipalID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.principal++
	return id.PrincipalID(fmt.Sprintf("execution-principal-%d", ids.principal)), nil
}

func (ids *integrationIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.event++
	return id.EventID(fmt.Sprintf("event-%d", ids.event)), nil
}

func (ids *integrationIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.receipt++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.receipt)), nil
}

// allowAuthorizer is an in-process stand-in, not a conformance subject: this
// suite proves the PostgreSQL adapter and server enforcement, NOT authorization
// policy. Every operation it admits here is still constrained by the real
// database role, scope and row-level security. The real authorizer is exercised
// by its own unit suite and the composition boundary suites.
type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
	return authorize.DecisionAllow, nil
}

// confirmAuthority is an in-process stand-in, not a conformance subject: it
// confirms every authority read so the PostgreSQL adapter path can run. It does
// not prove authority evaluation; that belongs to the real authority reader and
// the composition boundary suites.
type confirmAuthority struct{}

func (confirmAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		facts[index] = authorize.AuthorityFact{
			Check: read.Check, Scope: read.Scope, PrincipalID: read.PrincipalID,
			TargetID: read.TargetID, Version: read.ExpectedVersion, Allowed: true,
		}
	}
	return facts, nil
}

type integrationPrincipals struct {
	principals map[id.PrincipalID]identity.Principal
}

func (reader integrationPrincipals) ReadPrincipal(_ context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	principal, ok := reader.principals[request.PrincipalID]
	if !ok || request.Scope != principal.Scope {
		return identity.Principal{}, fault.New(fault.NotFound, "principal is not present in this scope")
	}
	return principal, nil
}

// staticResolver answers every registry reference with a fixed immutable digest.
// The registry boundary is not under test here; admission, timeout resolution
// and the persisted action row are.
type staticResolver struct{}

func (staticResolver) Resolve(_ context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	return pipeline.ResolvedImage{Reference: request.Reference, Digest: integrationImageDigest}, nil
}

const integrationImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type serviceEnv struct {
	shard      *pgtest.Shard
	runtime    *pgdriver.DB
	scope      tenancy.Scope
	task       *task.Service
	management *pipeline.Management
	reader     *work.Task
	exec       *executionstore.Store
	executions *readers.ExecutionReader
	ids        *integrationIDs
}

// hookedStore is a storepg.Store whose Execute runs a one-shot hook before the
// real commit transaction opens. It lets a component test change a row after
// the capability's authority confirmation read and before the adapter's
// in-commit guarded lock, which is exactly the window the guard must close.
type hookedStore struct {
	*storepg.Store
	hook func()
}

func (hooked *hookedStore) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if hooked.hook != nil {
		hooked.hook()
	}
	return hooked.Store.Execute(ctx, scope, unit)
}

func newServiceEnv(t *testing.T) *serviceEnv {
	t.Helper()
	return newServiceEnvWithHook(t, nil)
}

func newServiceEnvWithHook(t *testing.T, hook func()) *serviceEnv {
	t.Helper()
	shard := migratedShard(t)
	ctx := context.Background()
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedMembership(ctx, pgtest.Scope("tenant-1", "project-1"), "membership-1", "principal-1", 1,
		string(tenancy.RoleEngineer), string(tenancy.RoleAdmin))
	return newServiceEnvOnShard(t, shard, hook)
}

// newServiceEnvOnShard builds the real services over a shard the caller has
// already migrated and seeded with the tenant-1/project-1 scope and the
// principal-1 membership, so a test can choose the migration level.
func newServiceEnvOnShard(t *testing.T, shard *pgtest.Shard, hook func()) *serviceEnv {
	t.Helper()
	scope := pgtest.Scope("tenant-1", "project-1")
	runtime := shard.Runtime(4)
	t.Cleanup(runtime.Close)
	base := storepg.New(runtime)
	var executor store.Executor = base
	var scanner store.Scanner = base
	if hook != nil {
		hooked := &hookedStore{Store: base, hook: hook}
		executor = hooked
		scanner = hooked
	}

	taskAdapter, err := work.NewTask(executor, scanner)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	pipelines, err := work.NewPipelines(executor, scanner, func() (pipeline.PipelineID, error) {
		return pipeline.PipelineID("pipeline-1"), nil
	})
	if err != nil {
		t.Fatalf("NewPipelines: %v", err)
	}
	executionReader, err := readers.NewExecutionReader(executor, scanner)
	if err != nil {
		t.Fatalf("NewExecutionReader: %v", err)
	}
	ids := &integrationIDs{}
	principal := identity.Principal{
		ID: "principal-1", Kind: identity.KindHuman, Scope: scope,
		Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleAdmin}, Enabled: true, Version: 1,
	}
	admissionService, err := pipeline.NewAdmissionService(pipeline.AdmissionConfig{
		Resolver: staticResolver{}, Reader: pipelines,
	})
	if err != nil {
		t.Fatalf("pipeline.NewAdmissionService: %v", err)
	}
	service, err := task.NewService(task.ServiceConfig{
		Clock:          integrationClock{now: pgtest.FixtureTime},
		IDs:            ids,
		Authorizer:     allowAuthorizer{},
		Authority:      confirmAuthority{},
		Principals:     integrationPrincipals{principals: map[id.PrincipalID]identity.Principal{principal.ID: principal}},
		Pipelines:      pipelines,
		Reader:         taskAdapter,
		Executions:     executionReader,
		Aggregates:     executionstore.New(executor, scanner),
		Admission:      admissionService,
		Creates:        taskAdapter,
		Edits:          taskAdapter,
		Starts:         taskAdapter,
		Asks:           taskAdapter,
		Replies:        taskAdapter,
		Cancels:        taskAdapter,
		Progress:       taskAdapter,
		DefaultTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("task.NewService: %v", err)
	}
	management, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: ids,
		Authorizer: allowAuthorizer{}, Authority: confirmAuthority{},
		Changes: pipelines, Reader: pipelines,
	})
	if err != nil {
		t.Fatalf("pipeline.NewManagement: %v", err)
	}
	return &serviceEnv{shard: shard, runtime: runtime, scope: scope, task: service, management: management, reader: taskAdapter,
		exec: executionstore.New(executor, scanner), executions: executionReader, ids: ids}
}

func (env *serviceEnv) actor() authorize.Principal {
	return authorize.Principal{
		ID: "principal-1", Kind: identity.KindHuman, Scope: env.scope,
		Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleAdmin}, Enabled: true, Version: 1,
	}
}

func integrationContent(label string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(`{"request":"` + label + `"}`)}
}

func integrationPipelineYAML(command string) []byte {
	return []byte("actions:\n  - name: build\n    image: " + integrationImage + "\n    commands:\n      - " + command + "\n")
}

// serviceExecutionRow is the whole executions row a task service start or reply
// commits for the "build" pipeline admitted from yaml: the digest, snapshot and
// workspace id are the exact values the product derives from the admitted yaml
// and the execution id.
func serviceExecutionRow(pipelineID pipeline.PipelineID, taskID id.TaskID, executionID id.ExecutionID, principal, state string, version int64, yaml []byte) map[string]any {
	return map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": string(pipelineID), "task_id": string(taskID),
		"principal_id": principal, "state": state, "version": version,
		"yaml_sha256": pgtest.SHA256(yaml),
		"snapshot": admittedSnapshot(admittedSnapshotWant{
			PipelineID: pipelineID, PipelineVersion: 1, YAML: yaml,
			Actions: []admittedActionWant{{Name: "build", Kind: definition.ActionKindContainer, Timeout: 5 * time.Minute}},
		}),
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID(executionID)),
		"failure":      pgtest.NullValue, "created_at": pgtest.FixtureTime,
	}
}

// serviceReceiptRow is the whole receipts row a principal-1 service operation
// commits. Every service test passes integrationContent(callerKey), so the
// stored content digest is derived from that content. An operation that admits
// work records the admitted yaml digest as its snapshot hash and the new
// execution id; any other operation stores NULL for both.
func serviceReceiptRow(t testing.TB, receiptID, operation, callerKey string, result any, executionID id.ExecutionID, admittedYAML []byte) map[string]any {
	t.Helper()
	var execution, snapshot any = pgtest.NullValue, pgtest.NullValue
	if executionID != "" {
		execution = string(executionID)
	}
	if admittedYAML != nil {
		snapshot = pgtest.SHA256(admittedYAML)
	}
	return map[string]any{
		"id": receiptID, "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": operation, "caller_key": callerKey,
		"content_sha256": pgtest.ContentSHA256(t, integrationContent(callerKey)), "result": result,
		"completed_at": pgtest.FixtureTime, "execution_id": execution,
		"snapshot_sha256": snapshot, "created_at": pgtest.FixtureTime,
	}
}

// TestRealTaskLifecycleOnPostgreSQL drives the real task service through create,
// start, ask, reply, ask and cancel over the PostgreSQL work adapters.
func TestRealTaskLifecycleOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	pipelineYAML := integrationPipelineYAML("echo v1")
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: pipelineYAML,
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Fix the build", PipelineID: applied.PipelineID,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	startObserver := env.shard.ObserveUnit(t, "task", "start")
	started, err := env.task.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start: %v", err)
	}
	if started.LifecycleVersion != 2 {
		t.Fatalf("start lifecycle version = %d; want 2", started.LifecycleVersion)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "tasks", map[string]any{"id": string(created.TaskID)}, map[string]any{
		"id": string(created.TaskID), "tenant_id": "tenant-1", "project_id": "project-1",
		"content_version": int64(1), "lifecycle_version": int64(2), "state": "open",
		"pipeline_id": string(applied.PipelineID), "current_execution_id": string(started.ExecutionID),
		"initiator_id": "principal-1", "title": "Fix the build", "description": pgtest.NullValue,
		"metadata": pgtest.NullValue, "inputs": pgtest.NullValue,
		"execution_history": `["` + string(started.ExecutionID) + `"]`,
		"question_id":       pgtest.NullValue, "question_body": pgtest.NullValue, "question_assignment": pgtest.NullValue,
		"question_references": pgtest.NullValue, "question_version": pgtest.NullValue,
		"question_asked_by": pgtest.NullValue, "question_at": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(started.ExecutionID)},
		serviceExecutionRow(applied.PipelineID, created.TaskID, started.ExecutionID, "execution-principal-1", "queued", 1, pipelineYAML))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": "act-1acff0a8c7ead35d55c97c8b0d5b93f4"}, map[string]any{
		"id": "act-1acff0a8c7ead35d55c97c8b0d5b93f4", "execution_id": string(started.ExecutionID),
		"tenant_id": "tenant-1", "project_id": "project-1", "index": int64(0),
		"name": "build", "kind": "container", "state": "pending", "version": int64(1),
		"timeout_ms": int64(300000), "deadline": pgtest.NullValue, "outputs": pgtest.NullValue,
		"staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
		"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-3"},
		serviceReceiptRow(t, "receipt-3", "task.start", "task-start", pgtest.GoldenJSONOf(started), started.ExecutionID, pipelineYAML))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-5"}, map[string]any{
		"id": "event-5", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.start", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-6"}, map[string]any{
		"id": "event-6", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "execution.queued", "report_type": pgtest.NullValue,
		"subject_id": string(started.ExecutionID), "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	startObserver.Finish(map[string]int{"tasks": 1, "executions": 1, "actions": 1, "receipts": 1, "audit": 1, "events": 1})
	saved := env.ids.snapshot()
	startReplay := env.shard.ObserveUnit(t, "task", "start.replay")
	startedAgain, err := env.task.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "task start replay", started, startedAgain)
	startReplay.Finish(map[string]int{})
	env.ids.restore(saved)
	startedCurrent := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT current_execution_id, execution_history::text FROM tasks WHERE id = $1`, string(created.TaskID))
	if startedCurrent[0] != string(started.ExecutionID) {
		t.Fatalf("tasks.current_execution_id after start = %q; want %q", startedCurrent[0], started.ExecutionID)
	}
	if !jsonEqual(startedCurrent[1], `["`+string(started.ExecutionID)+`"]`) {
		t.Fatalf("execution_history after start = %s; want only %q", startedCurrent[1], started.ExecutionID)
	}
	askObserver := env.shard.ObserveUnit(t, "task", "ask")
	asked, err := env.task.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		Question: "Which branch?", Assignment: requestAssignment(), CallerKey: "task-ask-1",
		Content: integrationContent("task-ask-1"),
	})
	if err != nil {
		t.Fatalf("task ask: %v", err)
	}
	current, err := env.reader.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("read asked task: %v%s", err, pgtest.CauseText(err))
	}
	if current.ID != created.TaskID || current.Scope != env.scope || current.Title != "Fix the build" ||
		current.State != task.StateWaiting || current.LifecycleVersion != 3 {
		t.Fatalf("read asked task = %+v; want the created task in scope at waiting/3", current)
	}
	if current.Question == nil || current.Question.Body != "Which branch?" || current.Question.Version != 1 {
		t.Fatalf("read asked task question = %+v; want the asked question at version 1", current.Question)
	}
	taskGolden := func(state string, lifecycle int64, current any, hasQuestion bool, history string) {
		questionID, questionBody, questionAssignment, questionVersion := any(pgtest.NullValue), any(pgtest.NullValue), any(pgtest.NullValue), any(pgtest.NullValue)
		askedBy, askedAt, questionReferences := any(pgtest.NullValue), any(pgtest.NullValue), any(pgtest.NullValue)
		if hasQuestion {
			questionID, questionBody = "question-1", "Which branch?"
			questionAssignment, questionVersion = `{"kind":"role","role":"engineer"}`, int64(1)
			askedBy, askedAt = "principal-1", pgtest.FixtureTime
			// An ask without references stores the JSON value null (not SQL NULL):
			// jsonColumn(nil) in internal/shell/store/units/work/task.go encodes it.
			questionReferences = "null"
		}
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "tasks", map[string]any{"id": string(created.TaskID)}, map[string]any{
			"id": string(created.TaskID), "tenant_id": "tenant-1", "project_id": "project-1",
			"content_version": int64(1), "lifecycle_version": lifecycle, "state": state,
			"pipeline_id": string(applied.PipelineID), "current_execution_id": current,
			"initiator_id": "principal-1", "title": "Fix the build", "description": pgtest.NullValue,
			"metadata": pgtest.NullValue, "inputs": pgtest.NullValue, "execution_history": history,
			"question_id": questionID, "question_body": questionBody, "question_assignment": questionAssignment,
			"question_references": questionReferences, "question_version": questionVersion,
			"question_asked_by": askedBy, "question_at": askedAt, "created_at": pgtest.FixtureTime,
		})
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(started.ExecutionID)},
		serviceExecutionRow(applied.PipelineID, created.TaskID, started.ExecutionID, "execution-principal-1", "cancelled", 2, pipelineYAML))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": "act-1acff0a8c7ead35d55c97c8b0d5b93f4"}, map[string]any{
		"id": "act-1acff0a8c7ead35d55c97c8b0d5b93f4", "execution_id": string(started.ExecutionID),
		"tenant_id": "tenant-1", "project_id": "project-1", "index": int64(0),
		"name": "build", "kind": "container", "state": "cancelled", "version": int64(2),
		"timeout_ms": int64(300000), "deadline": pgtest.NullValue, "outputs": pgtest.NullValue,
		"staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
		"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "cleanup", map[string]any{"execution_id": string(started.ExecutionID)}, map[string]any{
		"id": "cln-f7ad8200d3f20d57ccccb16ef308d8b7", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": string(started.ExecutionID), "workload_id": pgtest.NullValue,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID(started.ExecutionID)),
		"state":        "pending", "attempts": int64(0), "last_error": pgtest.NullValue, "next_retry_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-4"},
		serviceReceiptRow(t, "receipt-4", "task.ask", "task-ask-1", pgtest.GoldenJSONOf(asked), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-7"}, map[string]any{
		"id": "event-7", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.ask", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	// derived: the ask cancels the queued execution through the Kernel cancel
	// reduction; event.ProjectLifecycle (internal/shell/event/contract_lifecycle.go)
	// projects its committed versions (execution 1->2, action 1->2) into these
	// bounded facts. A zero cleanup attempt count is omitted from the cleanup facts.
	cancelledActionID := "act-1acff0a8c7ead35d55c97c8b0d5b93f4"
	eventRows := []struct {
		id, eventType, subject, source string
		version                        int64
		data                           any
	}{
		{"event-8", "task.question", string(created.TaskID), "principal-1", 3, pgtest.NullValue},
		{"event-3f2cb0380832312f7f642df69b287d860dab66498c58f614889355fd244073b0", "action.transition", cancelledActionID, "system", 2,
			pgtest.GoldenJSONOf(map[string]any{
				"action_id": cancelledActionID, "index": 0, "kind": "container",
				"previous_state": "pending", "state": "cancelled", "version": 2,
			})},
		{"event-ca1951af2c9a95f6e21a188a6b3318162dedb2f530be062bd5000265c71de432", "execution.cancelled", string(started.ExecutionID), "system", 2,
			pgtest.GoldenJSONOf(map[string]any{
				"execution_id": started.ExecutionID, "previous_state": "queued", "state": "cancelled", "version": 2,
			})},
		{"event-291701c985cef748cbc732bd1de874385efc195331c286131cae4419a1dfb6f3", "execution.cleanup", string(started.ExecutionID), "system", 2,
			pgtest.GoldenJSONOf(map[string]any{
				"execution_id": started.ExecutionID, "cleanup_state": "pending",
			})},
	}
	for _, row := range eventRows {
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": row.id}, map[string]any{
			"id": row.id, "tenant_id": "tenant-1", "project_id": "project-1",
			"type": row.eventType, "report_type": pgtest.NullValue,
			"subject_id": row.subject, "subject_version": row.version,
			"source_kind": "shell", "source_principal": row.source,
			"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
			"at": pgtest.FixtureTime, "data": row.data, "publish_state": "pending",
			"publish_sequence": pgtest.GoldenDBSequence(),
		})
	}
	taskGolden("waiting", 3, string(started.ExecutionID), true,
		`["`+string(started.ExecutionID)+`"]`)
	askObserver.Finish(map[string]int{"tasks": 1, "executions": 1, "actions": 1, "cleanup": 1, "receipts": 1, "audit": 1, "events": 4})
	saved = env.ids.snapshot()
	askReplay := env.shard.ObserveUnit(t, "task", "ask.replay")
	askedAgain, err := env.task.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		Question: "Which branch?", Assignment: requestAssignment(), CallerKey: "task-ask-1",
		Content: integrationContent("task-ask-1"),
	})
	if err != nil {
		t.Fatalf("task ask replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "task ask replay", asked, askedAgain)
	askReplay.Finish(map[string]int{})
	env.ids.restore(saved)
	askedAssignment := scopedRow(t, ctx, env.runtime, env.scope, 1,
		`SELECT question_assignment::text FROM tasks WHERE id = $1`, string(created.TaskID))
	if !jsonEqual(askedAssignment[0], `{"kind":"role","role":"engineer"}`) {
		t.Fatalf("durable question_assignment = %s; want role/engineer", askedAssignment[0])
	}
	replyObserver := env.shard.ObserveUnit(t, "task", "reply")
	replied, err := env.task.Reply(ctx, task.ReplyCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 3,
		QuestionID: current.Question.ID, ExpectedQuestionVersion: current.Question.Version, Answer: "main",
		CallerKey: "task-reply", Content: integrationContent("task-reply"),
	})
	if err != nil || replied.ExecutionID == "" {
		t.Fatalf("task reply: %v (execution=%q)", err, replied.ExecutionID)
	}
	repliedCurrent := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT current_execution_id, execution_history::text FROM tasks WHERE id = $1`, string(created.TaskID))
	if repliedCurrent[0] != string(replied.ExecutionID) {
		t.Fatalf("tasks.current_execution_id after reply = %q; want %q", repliedCurrent[0], replied.ExecutionID)
	}
	if repliedCurrent[0] == string(started.ExecutionID) {
		t.Fatalf("tasks.current_execution_id after reply still names the predecessor %q", started.ExecutionID)
	}
	if !jsonEqual(repliedCurrent[1], `["`+string(started.ExecutionID)+`","`+string(replied.ExecutionID)+`"]`) {
		t.Fatalf("execution_history after reply = %s; want both executions in order", repliedCurrent[1])
	}
	// The reply committed one durable task_messages row; the reader's scanner
	// page must return that row's author and body, not merely a nil error.
	answered, err := env.reader.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("read answered task: %v%s", err, pgtest.CauseText(err))
	}
	if len(answered.Messages) != 1 || answered.Messages[0].Author != "principal-1" || answered.Messages[0].Body != "main" {
		t.Fatalf("read answered task messages = %+v; want the principal-1 reply body", answered.Messages)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(replied.ExecutionID)},
		serviceExecutionRow(applied.PipelineID, created.TaskID, replied.ExecutionID, "execution-principal-2", "queued", 1, pipelineYAML))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": "act-07806f5085a24e48aaf4a0a17dc70918"}, map[string]any{
		"id": "act-07806f5085a24e48aaf4a0a17dc70918", "execution_id": string(replied.ExecutionID),
		"tenant_id": "tenant-1", "project_id": "project-1", "index": int64(0),
		"name": "build", "kind": "container", "state": "pending", "version": int64(1),
		"timeout_ms": int64(300000), "deadline": pgtest.NullValue, "outputs": pgtest.NullValue,
		"staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
		"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-5"},
		serviceReceiptRow(t, "receipt-5", "task.reply", "task-reply", pgtest.GoldenJSONOf(replied), replied.ExecutionID, pipelineYAML))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-9"}, map[string]any{
		"id": "event-9", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.reply", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	for _, row := range []struct {
		id, eventType, subject, source string
		version                        int64
	}{
		{"event-10", "task.reply", string(created.TaskID), "principal-1", 4},
		{"event-11", "execution.queued", string(replied.ExecutionID), "principal-1", 1},
	} {
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": row.id}, map[string]any{
			"id": row.id, "tenant_id": "tenant-1", "project_id": "project-1",
			"type": row.eventType, "report_type": pgtest.NullValue,
			"subject_id": row.subject, "subject_version": row.version,
			"source_kind": "shell", "source_principal": row.source,
			"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
			"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
			"publish_sequence": pgtest.GoldenDBSequence(),
		})
	}
	taskGolden("open", 4, string(replied.ExecutionID), false,
		`["`+string(started.ExecutionID)+`","`+string(replied.ExecutionID)+`"]`)
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "task_messages", map[string]any{"task_id": string(created.TaskID)}, map[string]any{
		"id": "receipt-5", "task_id": string(created.TaskID), "tenant_id": "tenant-1",
		"project_id": "project-1", "author_id": "principal-1", "at": pgtest.FixtureTime,
		"body": "main", "refs": pgtest.NullValue,
	})
	replyObserver.Finish(map[string]int{"tasks": 1, "task_messages": 1, "executions": 1, "actions": 1, "receipts": 1, "audit": 1, "events": 2})
	saved = env.ids.snapshot()
	replyReplay := env.shard.ObserveUnit(t, "task", "reply.replay")
	repliedAgain, err := env.task.Reply(ctx, task.ReplyCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 3,
		QuestionID: current.Question.ID, ExpectedQuestionVersion: current.Question.Version, Answer: "main",
		CallerKey: "task-reply", Content: integrationContent("task-reply"),
	})
	if err != nil {
		t.Fatalf("task reply replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "task reply replay", replied, repliedAgain)
	replyReplay.Finish(map[string]int{})
	env.ids.restore(saved)
	if _, err := env.task.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 4,
		Question: "Anything else?", Assignment: requestAssignment(), CallerKey: "task-ask-2",
		Content: integrationContent("task-ask-2"),
	}); err != nil {
		t.Fatalf("second ask: %v", err)
	}
	cancelObserver := env.shard.ObserveUnit(t, "task", "cancel")
	cancelled, err := env.task.Cancel(ctx, task.CancelCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 5,
		CallerKey: "task-cancel", Content: integrationContent("task-cancel"),
	})
	if err != nil || cancelled.LifecycleVersion != 6 {
		t.Fatalf("task cancel: %v (version=%d)", err, cancelled.LifecycleVersion)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-7"},
		serviceReceiptRow(t, "receipt-7", "task.cancel", "task-cancel", pgtest.GoldenJSONOf(cancelled), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-14"}, map[string]any{
		"id": "event-14", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.cancel", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-15"}, map[string]any{
		"id": "event-15", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "task.cancelled", "report_type": pgtest.NullValue,
		"subject_id": string(created.TaskID), "subject_version": int64(6),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	taskGolden("cancelled", 6, string(replied.ExecutionID), false,
		`["`+string(started.ExecutionID)+`","`+string(replied.ExecutionID)+`"]`)
	cancelObserver.Finish(map[string]int{"tasks": 1, "receipts": 1, "audit": 1, "events": 1})
	saved = env.ids.snapshot()
	cancelReplay := env.shard.ObserveUnit(t, "task", "cancel.replay")
	cancelledAgain, err := env.task.Cancel(ctx, task.CancelCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 5,
		CallerKey: "task-cancel", Content: integrationContent("task-cancel"),
	})
	if err != nil {
		t.Fatalf("task cancel replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "task cancel replay", cancelled, cancelledAgain)
	cancelReplay.Finish(map[string]int{})
	env.ids.restore(saved)
	state, err := pgtest.ScalarString(ctx, env.shard.Admin(), `SELECT state FROM tasks WHERE id = $1`, string(created.TaskID))
	if err != nil || state != string(task.StateCancelled) {
		t.Fatalf("final task state = %q, err=%v; want cancelled", state, err)
	}
	// Start commits one queued execution; reply commits a fresh one, each with
	// its admitted action.
	executions, err := pgtest.ScalarInt(ctx, env.shard.Admin(),
		`SELECT count(*)::text FROM executions WHERE task_id = $1`, string(created.TaskID))
	if err != nil || executions != 2 {
		t.Fatalf("task executions = %d, err=%v; want 2", executions, err)
	}
	actions, err := pgtest.ScalarInt(ctx, env.shard.Admin(),
		`SELECT count(*)::text FROM actions a JOIN executions e ON e.id = a.execution_id WHERE e.task_id = $1`, string(created.TaskID))
	if err != nil || actions != 2 {
		t.Fatalf("task actions = %d, err=%v; want 2", actions, err)
	}
}

// TestTaskCreateAndEditsPersistOnPostgreSQL proves the task create, edit-content
// and edit-comment units persist every column they write, read back as the
// application role.
func TestTaskCreateAndEditsPersistOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	createObserver := env.shard.ObserveUnit(t, "task", "create")
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Original title", Description: "Original description",
		Metadata: []byte(`{"ticket":"ABC-1"}`), PipelineID: applied.PipelineID,
		Inputs:    map[id.InputKey]string{"repository": "org/repo"},
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v%s", err, pgtest.CauseText(err))
	}
	createdRow := scopedRow(t, ctx, env.runtime, env.scope, 7,
		`SELECT title, description, metadata::text, inputs::text, content_version::text,
		        lifecycle_version::text, state FROM tasks WHERE id = $1`, string(created.TaskID))
	if createdRow[0] != "Original title" || createdRow[1] != "Original description" ||
		createdRow[4] != "1" || createdRow[5] != "1" || createdRow[6] != "open" {
		t.Fatalf("created task row = %v; want title/description/content 1/lifecycle 1/open", createdRow)
	}
	if !jsonEqual(createdRow[2], `{"ticket":"ABC-1"}`) || !jsonEqual(createdRow[3], `{"repository":"org/repo"}`) {
		t.Fatalf("created task metadata/inputs = %q/%q; want the submitted JSON", createdRow[2], createdRow[3])
	}
	initiator := scopedRow(t, ctx, env.runtime, env.scope, 1,
		`SELECT initiator_id FROM tasks WHERE id = $1`, string(created.TaskID))
	if initiator[0] != "principal-1" {
		t.Fatalf("created task initiator = %q; want principal-1", initiator[0])
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-2"},
		serviceReceiptRow(t, "receipt-2", "task.create", "task-create", pgtest.GoldenJSONOf(created), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-4"}, map[string]any{
		"id": "event-4", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.create", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-3"}, map[string]any{
		"id": "event-3", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "task.created", "report_type": pgtest.NullValue,
		"subject_id": string(created.TaskID), "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	shardGolden := func(table string, key, want map[string]any) {
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, table, key, want)
	}
	shardGolden("tasks", map[string]any{"id": string(created.TaskID)}, map[string]any{
		"id": string(created.TaskID), "tenant_id": "tenant-1", "project_id": "project-1",
		"content_version": int64(1), "lifecycle_version": int64(1), "state": "open",
		"pipeline_id": string(applied.PipelineID), "current_execution_id": pgtest.NullValue,
		"initiator_id": "principal-1", "title": "Original title", "description": "Original description",
		"metadata": `{"ticket":"ABC-1"}`, "inputs": `{"repository":"org/repo"}`,
		"execution_history": pgtest.NullValue, "question_id": pgtest.NullValue,
		"question_body": pgtest.NullValue, "question_assignment": pgtest.NullValue,
		"question_references": pgtest.NullValue, "question_version": pgtest.NullValue,
		"question_asked_by": pgtest.NullValue, "question_at": pgtest.NullValue,
		"created_at": pgtest.FixtureTime,
	})

	createObserver.Finish(map[string]int{"tasks": 1, "receipts": 1, "audit": 1, "events": 1})
	saved := env.ids.snapshot()
	createReplay := env.shard.ObserveUnit(t, "task", "create.replay")
	createdAgain, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Original title", Description: "Original description",
		Metadata: []byte(`{"ticket":"ABC-1"}`), PipelineID: applied.PipelineID,
		Inputs:    map[id.InputKey]string{"repository": "org/repo"},
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "task create replay", created, createdAgain)
	createReplay.Finish(map[string]int{})
	env.ids.restore(saved)
	editObserver := env.shard.ObserveUnit(t, "task", "edit.content")
	edited, err := env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditContent,
		ExpectedContentVersion: 1, Title: "Edited title", Description: "Edited description",
		Metadata: []byte(`{"ticket":"ABC-2"}`), CallerKey: "task-edit", Content: integrationContent("task-edit"),
	})
	if err != nil {
		t.Fatalf("task edit content: %v%s", err, pgtest.CauseText(err))
	}
	editedRow := scopedRow(t, ctx, env.runtime, env.scope, 4,
		`SELECT title, description, metadata::text, content_version::text FROM tasks WHERE id = $1`, string(created.TaskID))
	if editedRow[0] != "Edited title" || editedRow[1] != "Edited description" || editedRow[3] != "2" ||
		!jsonEqual(editedRow[2], `{"ticket":"ABC-2"}`) {
		t.Fatalf("edited task row = %v; want edited title/description/metadata at content 2", editedRow)
	}
	shardGolden("tasks", map[string]any{"id": string(created.TaskID)}, map[string]any{
		"id": string(created.TaskID), "tenant_id": "tenant-1", "project_id": "project-1",
		"content_version": int64(2), "lifecycle_version": int64(1), "state": "open",
		"pipeline_id": string(applied.PipelineID), "current_execution_id": pgtest.NullValue,
		"initiator_id": "principal-1", "title": "Edited title", "description": "Edited description",
		"metadata": `{"ticket":"ABC-2"}`, "inputs": `{"repository":"org/repo"}`,
		"execution_history": pgtest.NullValue, "question_id": pgtest.NullValue,
		"question_body": pgtest.NullValue, "question_assignment": pgtest.NullValue,
		"question_references": pgtest.NullValue, "question_version": pgtest.NullValue,
		"question_asked_by": pgtest.NullValue, "question_at": pgtest.NullValue,
		"created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-3"},
		serviceReceiptRow(t, "receipt-3", "task.edit", "task-edit", pgtest.GoldenJSONOf(edited), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-5"}, map[string]any{
		"id": "event-5", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.edit", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	editObserver.Finish(map[string]int{"tasks": 1, "receipts": 1, "audit": 1})
	saved = env.ids.snapshot()
	editReplay := env.shard.ObserveUnit(t, "task", "edit.content.replay")
	editedAgain, err := env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditContent,
		ExpectedContentVersion: 1, Title: "Edited title", Description: "Edited description",
		Metadata: []byte(`{"ticket":"ABC-2"}`), CallerKey: "task-edit", Content: integrationContent("task-edit"),
	})
	if err != nil {
		t.Fatalf("task edit content replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "task edit content replay", edited, editedAgain)
	editReplay.Finish(map[string]int{})
	env.ids.restore(saved)

	commentObserver := env.shard.ObserveUnit(t, "task", "edit.comment")
	commented, err := env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditComment,
		ExpectedContentVersion: 2, Body: "Comment body", References: []string{"ref-1"},
		CallerKey: "task-comment", Content: integrationContent("task-comment"),
	})
	if err != nil {
		t.Fatalf("task edit comment: %v%s", err, pgtest.CauseText(err))
	}
	commentedRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT content_version::text, title FROM tasks WHERE id = $1`, string(created.TaskID))
	if commentedRow[0] != "3" || commentedRow[1] != "Edited title" {
		t.Fatalf("commented task row = %v; want content 3 and the edited title", commentedRow)
	}
	message := scopedRow(t, ctx, env.runtime, env.scope, 3,
		`SELECT author_id, body, refs::text FROM task_messages WHERE task_id = $1`, string(created.TaskID))
	shardGolden("tasks", map[string]any{"id": string(created.TaskID)}, map[string]any{
		"id": string(created.TaskID), "tenant_id": "tenant-1", "project_id": "project-1",
		"content_version": int64(3), "lifecycle_version": int64(1), "state": "open",
		"pipeline_id": string(applied.PipelineID), "current_execution_id": pgtest.NullValue,
		"initiator_id": "principal-1", "title": "Edited title", "description": "Edited description",
		"metadata": `{"ticket":"ABC-2"}`, "inputs": `{"repository":"org/repo"}`,
		"execution_history": pgtest.NullValue, "question_id": pgtest.NullValue,
		"question_body": pgtest.NullValue, "question_assignment": pgtest.NullValue,
		"question_references": pgtest.NullValue, "question_version": pgtest.NullValue,
		"question_asked_by": pgtest.NullValue, "question_at": pgtest.NullValue,
		"created_at": pgtest.FixtureTime,
	})
	if message[0] != "principal-1" || message[1] != "Comment body" || !jsonEqual(message[2], `["ref-1"]`) {
		t.Fatalf("task message = %v; want principal-1/Comment body/[ref-1]", message)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-4"},
		serviceReceiptRow(t, "receipt-4", "task.comment", "task-comment", pgtest.GoldenJSONOf(commented), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-6"}, map[string]any{
		"id": "event-6", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.comment", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shardGolden("task_messages", map[string]any{"task_id": string(created.TaskID)}, map[string]any{
		"id": "receipt-4", "task_id": string(created.TaskID),
		"tenant_id": "tenant-1", "project_id": "project-1", "author_id": "principal-1",
		"at": pgtest.FixtureTime, "body": "Comment body", "refs": `["ref-1"]`,
	})
	commentObserver.Finish(map[string]int{"tasks": 1, "task_messages": 1, "receipts": 1, "audit": 1})
	saved = env.ids.snapshot()
	commentReplay := env.shard.ObserveUnit(t, "task", "edit.comment.replay")
	commentedAgain, err := env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditComment,
		ExpectedContentVersion: 2, Body: "Comment body", References: []string{"ref-1"},
		CallerKey: "task-comment", Content: integrationContent("task-comment"),
	})
	if err != nil {
		t.Fatalf("task edit comment replay: %v%s", err, pgtest.CauseText(err))
	}
	requireReplayOf(t, "task edit comment replay", commented, commentedAgain)
	commentReplay.Finish(map[string]int{})
	env.ids.restore(saved)
	if _, err := env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditContent,
		ExpectedContentVersion: 3, Title: "No metadata", Description: "", Metadata: nil,
		CallerKey: "task-edit-empty", Content: integrationContent("task-edit-empty"),
	}); err != nil {
		t.Fatalf("task edit empty metadata: %v%s", err, pgtest.CauseText(err))
	}
	shardGolden("tasks", map[string]any{"id": string(created.TaskID)}, map[string]any{
		"id": string(created.TaskID), "tenant_id": "tenant-1", "project_id": "project-1",
		"content_version": int64(4), "lifecycle_version": int64(1), "state": "open",
		"pipeline_id": string(applied.PipelineID), "current_execution_id": pgtest.NullValue,
		"initiator_id": "principal-1", "title": "No metadata", "description": pgtest.NullValue,
		"metadata": pgtest.NullValue, "inputs": `{"repository":"org/repo"}`,
		"execution_history": pgtest.NullValue, "question_id": pgtest.NullValue,
		"question_body": pgtest.NullValue, "question_assignment": pgtest.NullValue,
		"question_references": pgtest.NullValue, "question_version": pgtest.NullValue,
		"question_asked_by": pgtest.NullValue, "question_at": pgtest.NullValue,
		"created_at": pgtest.FixtureTime,
	})
}

// jsonEqual compares two JSON documents independent of whitespace/key order.
func jsonEqual(left, right string) bool {
	var leftValue, rightValue any
	if err := json.Unmarshal([]byte(left), &leftValue); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(right), &rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func requestAssignment() request.Assignment {
	return request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer}
}

// TestPipelinePushTriggersPersistOnPostgreSQL proves the real pipeline service
// persists the push projection and the versioned definition in one unit.
func TestPipelinePushTriggersPersistOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	yaml := []byte("repository: repo-1\nservice: service-1\non:\n  push:\n    - refs/heads/main\n" +
		"inputs: [repository, ref, commit, previous_commit]\n" +
		"actions:\n  - name: test\n    image: " + integrationImage + "\n    commands:\n      - echo ok\n")
	applyObserver := env.shard.ObserveUnit(t, "pipeline", "apply")
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "push", YAML: yaml,
		CallerKey: "pipeline-push", Content: integrationContent("pipeline-push"),
	})
	if err != nil {
		t.Fatalf("push apply: %v", err)
	}
	triggers, err := pgtest.ScalarString(ctx, env.shard.Admin(),
		`SELECT push_triggers::text FROM pipelines WHERE id = $1`, string(applied.PipelineID))
	if err != nil || triggers == "" || triggers == "[]" {
		t.Fatalf("push_triggers = %q, err=%v; want a non-empty projection", triggers, err)
	}
	name, err := pgtest.ScalarString(ctx, env.shard.Admin(), `SELECT name FROM pipelines WHERE id = $1`, string(applied.PipelineID))
	if err != nil || name != "push" {
		t.Fatalf("durable pipeline name = %q, err=%v; want push", name, err)
	}
	yamlDigest := sha256.Sum256(yaml)
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "pipelines", map[string]any{"id": string(applied.PipelineID)}, map[string]any{
		"id": string(applied.PipelineID), "tenant_id": "tenant-1", "project_id": "project-1",
		"name": "push", "yaml": yaml, "yaml_sha256": yamlDigest[:],
		"push_triggers": pgtest.GoldenJSONOf([]pipeline.PushTrigger{{
			Repository: "repo-1", Service: "service-1", Refs: []string{"refs/heads/main"},
		}}),
		"version": int64(1), "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-1"},
		serviceReceiptRow(t, "receipt-1", "pipeline.apply", "pipeline-push", pgtest.GoldenJSONOf(applied), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-1"}, map[string]any{
		"id": "event-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "pipeline.apply", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-2"}, map[string]any{
		"id": "event-2", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "push", "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	applyObserver.Finish(map[string]int{"pipelines": 1, "receipts": 1, "audit": 1, "events": 1})
}

// TestExecutionPrincipalMembershipGuardOnPostgreSQL proves the task service
// refuses a start whose actor membership was already revoked before the unit was
// built: the capability's authority read is refused, so no commit is attempted.
func TestExecutionPrincipalMembershipGuardOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	created := applyPipelineAndCreateTask(t, env)
	// Revoke the actor's membership, then a start must be refused before the
	// port writes anything.
	if _, err := pgtest.ExecSQL(ctx, env.shard.Admin(), `DELETE FROM memberships WHERE principal_id = 'principal-1'`); err != nil {
		t.Fatalf("revoke membership: %v", err)
	}
	_, err := env.task.Start(ctx, task.StartCommand{
		Actor: env.actor(), Scope: env.scope, TaskID: created, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start-denied", Content: integrationContent("task-start-denied"),
	})
	if class, ok := fault.ClassOf(err); !ok || (class != fault.Forbidden && class != fault.NotFound && class != fault.Unauthenticated) {
		t.Fatalf("start after revoke error = %v; want a refusal", err)
	}
	requireNoExecutionRows(t, ctx, env)
}

// TestExecutionPrincipalMembershipVersionGuardInsideCommitOnPostgreSQL proves
// the adapter's in-commit guard, not just its read-time lookup: a membership
// that is changed after the capability confirmed authority and before the
// commit transaction's guarded lock must fail the start and persist nothing.
func TestExecutionPrincipalMembershipVersionGuardInsideCommitOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	armed := false
	var env *serviceEnv
	env = newServiceEnvWithHook(t, func() {
		if !armed {
			return
		}
		armed = false
		if _, err := pgtest.ExecSQL(ctx, env.shard.Admin(),
			`UPDATE memberships SET version = version + 1 WHERE principal_id = 'principal-1'`); err != nil {
			t.Fatalf("bump membership after confirmation: %v", err)
		}
	})
	created := applyPipelineAndCreateTask(t, env)
	armed = true
	_, err := env.task.Start(ctx, task.StartCommand{
		Actor: env.actor(), Scope: env.scope, TaskID: created, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start-stale-membership", Content: integrationContent("task-start-stale-membership"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("start with a membership version bumped after confirmation error = %v; want version_conflict", err)
	}
	version, err := pgtest.ScalarInt(ctx, env.shard.Admin(),
		`SELECT version FROM memberships WHERE principal_id = 'principal-1'`)
	if err != nil || version != 2 {
		t.Fatalf("membership version = %d, err=%v; want the fixture's bump to 2", version, err)
	}
	requireNoExecutionRows(t, ctx, env)
}

// TestExecutionPrincipalMembershipDeleteGuardInsideCommitOnPostgreSQL proves a
// membership deleted after authority confirmation and before the commit fails
// the guarded lock and persists nothing.
func TestExecutionPrincipalMembershipDeleteGuardInsideCommitOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	armed := false
	var env *serviceEnv
	env = newServiceEnvWithHook(t, func() {
		if !armed {
			return
		}
		armed = false
		if _, err := pgtest.ExecSQL(ctx, env.shard.Admin(),
			`DELETE FROM memberships WHERE principal_id = 'principal-1'`); err != nil {
			t.Fatalf("delete membership after confirmation: %v", err)
		}
	})
	created := applyPipelineAndCreateTask(t, env)
	armed = true
	_, err := env.task.Start(ctx, task.StartCommand{
		Actor: env.actor(), Scope: env.scope, TaskID: created, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start-deleted-membership", Content: integrationContent("task-start-deleted-membership"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("start with a membership deleted after confirmation error = %v; want version_conflict", err)
	}
	requireNoExecutionRows(t, ctx, env)
}

// TestExecutionPrincipalMembershipMissingRoleOnPostgreSQL proves a membership
// that exists in scope but lacks the operation's required role is refused by the
// adapter's own authority evaluation, even when the capability-level stand-in
// confirmed it.
func TestExecutionPrincipalMembershipMissingRoleOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	created := applyPipelineAndCreateTask(t, env)
	if _, err := pgtest.ExecSQL(ctx, env.shard.Admin(),
		`UPDATE memberships SET roles = '["viewer"]'::jsonb WHERE principal_id = 'principal-1'`); err != nil {
		t.Fatalf("demote membership to viewer: %v", err)
	}
	_, err := env.task.Start(ctx, task.StartCommand{
		Actor: env.actor(), Scope: env.scope, TaskID: created, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start-viewer", Content: integrationContent("task-start-viewer"),
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("start by a membership without the engineer role error = %v; want forbidden", err)
	}
	requireNoExecutionRows(t, ctx, env)
}

// applyPipelineAndCreateTask seeds one open task whose pipeline is applied and
// returns the task id. It leaves the task ready for a start.
func applyPipelineAndCreateTask(t *testing.T, env *serviceEnv) id.TaskID {
	t.Helper()
	ctx := context.Background()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: env.actor(), Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor: env.actor(), Scope: env.scope, Title: "Guard", PipelineID: applied.PipelineID,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	return created.TaskID
}

// requireNoExecutionRows asserts a refused start persisted neither an execution
// nor an action.
func requireNoExecutionRows(t *testing.T, ctx context.Context, env *serviceEnv) {
	t.Helper()
	executions, err := pgtest.ScalarInt(ctx, env.shard.Admin(), `SELECT count(*)::text FROM executions`)
	if err != nil || executions != 0 {
		t.Fatalf("executions after refused start = %d, err=%v; want 0", executions, err)
	}
	actions, err := pgtest.ScalarInt(ctx, env.shard.Admin(), `SELECT count(*)::text FROM actions`)
	if err != nil || actions != 0 {
		t.Fatalf("actions after refused start = %d, err=%v; want 0", actions, err)
	}
}

// TestResolvedTimeoutPersistedOnPostgreSQL proves the real pipeline admission
// resolves an action's absent timeout from the task service default and the work
// adapter persists the resolved value in actions.timeout_ms.
func TestResolvedTimeoutPersistedOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Timeout", PipelineID: applied.PipelineID,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	if _, err := env.task.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	}); err != nil {
		t.Fatalf("task start: %v", err)
	}
	timeout, err := pgtest.ScalarInt(ctx, env.shard.Admin(),
		`SELECT a.timeout_ms FROM actions a JOIN executions e ON e.id = a.execution_id WHERE e.task_id = $1`,
		string(created.TaskID))
	if err != nil || timeout != 300000 {
		t.Fatalf("persisted action timeout_ms = %d, err=%v; want 300000 (5m default)", timeout, err)
	}
}

// TestObservedTaskSelectPipelineOnPostgreSQL observes the select-pipeline unit
// on its own shard so unrelated states cannot shift its row ids.
func TestObservedTaskSelectPipelineOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Select", PipelineID: applied.PipelineID,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	observer := env.shard.ObserveUnit(t, "task", "select_pipeline")
	selected, err := env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditSelectPipeline,
		ExpectedLifecycleVersion: 1, PipelineID: applied.PipelineID,
		Inputs: map[id.InputKey]string{"repository": "org/repo"}, CallerKey: "task-select",
		Content: integrationContent("task-select"),
	})
	if err != nil {
		t.Fatalf("task select pipeline: %v%s", err, pgtest.CauseText(err))
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "tasks", map[string]any{"id": string(created.TaskID)}, map[string]any{
		"id": string(created.TaskID), "tenant_id": "tenant-1", "project_id": "project-1",
		"content_version": int64(1), "lifecycle_version": int64(2), "state": "open",
		"pipeline_id": string(applied.PipelineID), "current_execution_id": pgtest.NullValue,
		"initiator_id": "principal-1", "title": "Select", "description": pgtest.NullValue,
		"metadata": pgtest.NullValue, "inputs": `{"repository":"org/repo"}`,
		"execution_history": pgtest.NullValue, "question_id": pgtest.NullValue,
		"question_body": pgtest.NullValue, "question_assignment": pgtest.NullValue,
		"question_references": pgtest.NullValue, "question_version": pgtest.NullValue,
		"question_asked_by": pgtest.NullValue, "question_at": pgtest.NullValue,
		"created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-3"},
		serviceReceiptRow(t, "receipt-3", "task.select_pipeline", "task-select", pgtest.GoldenJSONOf(selected), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-5"}, map[string]any{
		"id": "event-5", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.select_pipeline", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	observer.Finish(map[string]int{"tasks": 1, "receipts": 1, "audit": 1})
	replayObserver := env.shard.ObserveUnit(t, "task", "select_pipeline.replay")
	selectedAgain, err := env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditSelectPipeline,
		ExpectedLifecycleVersion: 1, PipelineID: applied.PipelineID,
		Inputs: map[id.InputKey]string{"repository": "org/repo"}, CallerKey: "task-select",
		Content: integrationContent("task-select"),
	})
	if err != nil {
		t.Fatalf("select pipeline replay: %v", err)
	}
	requireReplayOf(t, "task select pipeline replay", selected, selectedAgain)
	replayObserver.Finish(map[string]int{})
}

// TestObservedTaskCreateAndStartOnPostgreSQL observes the create-and-start
// composite unit on its own shard.
func TestObservedTaskCreateAndStartOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	pipelineYAML := integrationPipelineYAML("echo v1")
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: pipelineYAML,
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	observer := env.shard.ObserveUnit(t, "task", "create_and_start")
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Create and start", PipelineID: applied.PipelineID,
		Start: true, CallerKey: "task-create-start", Content: integrationContent("task-create-start"),
	})
	if err != nil {
		t.Fatalf("task create and start: %v%s", err, pgtest.CauseText(err))
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "tasks", map[string]any{"id": string(created.TaskID)}, map[string]any{
		"id": string(created.TaskID), "tenant_id": "tenant-1", "project_id": "project-1",
		"content_version": int64(1), "lifecycle_version": int64(2), "state": "open",
		"pipeline_id": string(applied.PipelineID), "current_execution_id": string(created.ExecutionID),
		"initiator_id": "principal-1", "title": "Create and start", "description": pgtest.NullValue,
		"metadata": pgtest.NullValue, "inputs": pgtest.NullValue,
		"execution_history": `["` + string(created.ExecutionID) + `"]`,
		"question_id":       pgtest.NullValue, "question_body": pgtest.NullValue, "question_assignment": pgtest.NullValue,
		"question_references": pgtest.NullValue, "question_version": pgtest.NullValue,
		"question_asked_by": pgtest.NullValue, "question_at": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(created.ExecutionID)},
		serviceExecutionRow(applied.PipelineID, created.TaskID, created.ExecutionID, "execution-principal-1", "queued", 1, pipelineYAML))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": "act-1acff0a8c7ead35d55c97c8b0d5b93f4"}, map[string]any{
		"id": "act-1acff0a8c7ead35d55c97c8b0d5b93f4", "execution_id": string(created.ExecutionID),
		"tenant_id": "tenant-1", "project_id": "project-1", "index": int64(0),
		"name": "build", "kind": "container", "state": "pending", "version": int64(1),
		"timeout_ms": int64(300000), "deadline": pgtest.NullValue, "outputs": pgtest.NullValue,
		"staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
		"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-2"},
		serviceReceiptRow(t, "receipt-2", "task.create_and_start", "task-create-start", pgtest.GoldenJSONOf(created), created.ExecutionID, pipelineYAML))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-5"}, map[string]any{
		"id": "event-5", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "task.create_and_start", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	for _, row := range []struct {
		id, eventType, subject string
	}{
		{"event-3", "task.created", string(created.TaskID)},
		{"event-4", "execution.queued", string(created.ExecutionID)},
	} {
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": row.id}, map[string]any{
			"id": row.id, "tenant_id": "tenant-1", "project_id": "project-1",
			"type": row.eventType, "report_type": pgtest.NullValue,
			"subject_id": row.subject, "subject_version": int64(1),
			"source_kind": "shell", "source_principal": "principal-1",
			"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
			"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
			"publish_sequence": pgtest.GoldenDBSequence(),
		})
	}
	observer.Finish(map[string]int{"tasks": 1, "executions": 1, "actions": 1, "receipts": 1, "audit": 1, "events": 2})
	replayObserver := env.shard.ObserveUnit(t, "task", "create_and_start.replay")
	createdAgain, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Create and start", PipelineID: applied.PipelineID,
		Start: true, CallerKey: "task-create-start", Content: integrationContent("task-create-start"),
	})
	if err != nil {
		t.Fatalf("create and start replay: %v", err)
	}
	requireReplayOf(t, "task create and start replay", created, createdAgain)
	replayObserver.Finish(map[string]int{})
}

// TestObservedPipelineUpdateAndDeleteOnPostgreSQL observes the pipeline.update
// and pipeline.delete commit units on one isolated shard.
func TestObservedPipelineUpdateAndDeleteOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v%s", err, pgtest.CauseText(err))
	}
	updateObserver := env.shard.ObserveUnit(t, "pipeline", "update")
	updatedYAML := integrationPipelineYAML("echo v2")
	updated, err := env.management.Update(ctx, pipeline.UpdateCommand{
		Actor: actor, Scope: env.scope, PipelineID: applied.PipelineID, Name: "build",
		YAML: updatedYAML, ExpectedVersion: 1, CallerKey: "pipeline-update",
		Content: integrationContent("pipeline-update"),
	})
	if err != nil {
		t.Fatalf("pipeline update: %v%s", err, pgtest.CauseText(err))
	}
	updateDigest := sha256.Sum256(updatedYAML)
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "pipelines", map[string]any{"id": string(applied.PipelineID)}, map[string]any{
		"id": string(applied.PipelineID), "tenant_id": "tenant-1", "project_id": "project-1",
		"name": "build", "yaml": updatedYAML, "yaml_sha256": updateDigest[:],
		"push_triggers": pgtest.GoldenJSONOf([]pipeline.PushTrigger{}), "version": int64(2), "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-2"},
		serviceReceiptRow(t, "receipt-2", "pipeline.update", "pipeline-update", pgtest.GoldenJSONOf(updated), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-3"}, map[string]any{
		"id": "event-3", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "pipeline.update", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-4"}, map[string]any{
		"id": "event-4", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": string(applied.PipelineID), "subject_version": int64(2),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	updateObserver.Finish(map[string]int{"pipelines": 1, "receipts": 1, "audit": 1, "events": 1})
	deleteObserver := env.shard.ObserveUnit(t, "pipeline", "delete")
	deleted, err := env.management.Delete(ctx, pipeline.DeleteCommand{
		Actor: actor, Scope: env.scope, PipelineID: applied.PipelineID, ExpectedVersion: updated.Version,
		CallerKey: "pipeline-delete", Content: integrationContent("pipeline-delete"),
	})
	if err != nil {
		t.Fatalf("pipeline delete: %v%s", err, pgtest.CauseText(err))
	}
	env.shard.RequireRowAbsent(t, ctx, env.runtime, env.scope, "pipelines", map[string]any{"id": string(applied.PipelineID)})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-3"},
		serviceReceiptRow(t, "receipt-3", "pipeline.delete", "pipeline-delete", pgtest.GoldenJSONOf(deleted), "", nil))
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-5"}, map[string]any{
		"id": "event-5", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "pipeline.delete", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-6"}, map[string]any{
		"id": "event-6", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": string(applied.PipelineID), "subject_version": int64(3),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	deleteObserver.Finish(map[string]int{"pipelines": 1, "receipts": 1, "audit": 1, "events": 1})
}

// TestTaskVersionLosersWriteNothingOnPostgreSQL observes every task unit's
// stale-version or changed-idempotency loser and requires zero changed rows.
func TestTaskVersionLosersWriteNothingOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Losers", PipelineID: applied.PipelineID,
		Start: true, CallerKey: "task-create-start", Content: integrationContent("task-create-start"),
	})
	if err != nil {
		t.Fatalf("create and start setup: %v", err)
	}
	expectVersionConflict := func(label string, err error) {
		t.Helper()
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("%s error = %v; want version_conflict", label, err)
		}
	}
	expectChangedContent := func(label string, err error) {
		t.Helper()
		if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
			t.Fatalf("%s error = %v; want changed_idempotency_content", label, err)
		}
	}

	if _, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Plain", PipelineID: applied.PipelineID,
		CallerKey: "task-create-plain", Content: integrationContent("task-create-plain"),
	}); err != nil {
		t.Fatalf("plain create setup: %v", err)
	}
	createLoser := env.shard.ObserveUnit(t, "task", "create.loser")
	_, err = env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Changed", PipelineID: applied.PipelineID,
		CallerKey: "task-create-plain", Content: integrationContent("changed-content"),
	})
	expectChangedContent("create", err)
	createLoser.Finish(map[string]int{})

	createAndStartLoser := env.shard.ObserveUnit(t, "task", "create_and_start.loser")
	_, err = env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Changed", PipelineID: applied.PipelineID,
		Start: true, CallerKey: "task-create-start", Content: integrationContent("changed-content"),
	})
	expectChangedContent("create and start", err)
	createAndStartLoser.Finish(map[string]int{})

	contentLoser := env.shard.ObserveUnit(t, "task", "edit.content.loser")
	_, err = env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditContent,
		ExpectedContentVersion: 99, Title: "Loser", CallerKey: "loser-content",
		Content: integrationContent("loser-content"),
	})
	expectVersionConflict("edit content", err)
	contentLoser.Finish(map[string]int{})

	commentLoser := env.shard.ObserveUnit(t, "task", "edit.comment.loser")
	_, err = env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditComment,
		ExpectedContentVersion: 99, Body: "loser", CallerKey: "loser-comment",
		Content: integrationContent("loser-comment"),
	})
	expectVersionConflict("edit comment", err)
	commentLoser.Finish(map[string]int{})

	startLoser := env.shard.ObserveUnit(t, "task", "start.loser")
	_, err = env.task.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 99,
		CallerKey: "loser-start", Content: integrationContent("loser-start"),
	})
	expectVersionConflict("start", err)
	startLoser.Finish(map[string]int{})

	askLoser := env.shard.ObserveUnit(t, "task", "ask.loser")
	_, err = env.task.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 99,
		Question: "loser", Assignment: requestAssignment(), CallerKey: "loser-ask",
		Content: integrationContent("loser-ask"),
	})
	expectVersionConflict("ask", err)
	askLoser.Finish(map[string]int{})

	cancelLoser := env.shard.ObserveUnit(t, "task", "cancel.loser")
	_, err = env.task.Cancel(ctx, task.CancelCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 99,
		CallerKey: "loser-cancel", Content: integrationContent("loser-cancel"),
	})
	expectVersionConflict("cancel", err)
	cancelLoser.Finish(map[string]int{})

	selectLoser := env.shard.ObserveUnit(t, "task", "select_pipeline.loser")
	_, err = env.task.Edit(ctx, task.EditCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, Kind: task.EditSelectPipeline,
		ExpectedLifecycleVersion: 99, PipelineID: applied.PipelineID,
		CallerKey: "loser-select", Content: integrationContent("loser-select"),
	})
	expectVersionConflict("select pipeline", err)
	selectLoser.Finish(map[string]int{})

	// A successful ask gives the reply loser a current question to name.
	if _, err := env.task.Ask(ctx, task.AskCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 2,
		Question: "Ready?", Assignment: requestAssignment(), CallerKey: "loser-ask-setup",
		Content: integrationContent("loser-ask-setup"),
	}); err != nil {
		t.Fatalf("ask setup: %v%s", err, pgtest.CauseText(err))
	}
	replyLoser := env.shard.ObserveUnit(t, "task", "reply.loser")
	_, err = env.task.Reply(ctx, task.ReplyCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 99,
		QuestionID: "question-missing", ExpectedQuestionVersion: 99, Answer: "loser",
		CallerKey: "loser-reply", Content: integrationContent("loser-reply"),
	})
	expectVersionConflict("reply", err)
	replyLoser.Finish(map[string]int{})
}

// TestObservedTaskProgressOnPostgreSQL drives the task progress commit unit
// directly on the work adapter: a current completion writes state "done" at the
// next lifecycle version and touches nothing else, and a stale lifecycle
// version or a different execution is a version_conflict that changes no row.
func TestObservedTaskProgressOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	actor := env.actor()
	applied, err := env.management.Apply(ctx, pipeline.ApplyCommand{
		Actor: actor, Scope: env.scope, Name: "build", YAML: integrationPipelineYAML("echo v1"),
		CallerKey: "pipeline-apply", Content: integrationContent("pipeline-apply"),
	})
	if err != nil {
		t.Fatalf("pipeline apply: %v", err)
	}
	created, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Fix the build", PipelineID: applied.PipelineID,
		CallerKey: "task-create", Content: integrationContent("task-create"),
	})
	if err != nil {
		t.Fatalf("task create: %v", err)
	}
	started, err := env.task.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: created.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start", Content: integrationContent("task-start"),
	})
	if err != nil {
		t.Fatalf("task start: %v%s", err, pgtest.CauseText(err))
	}
	if started.LifecycleVersion != 2 {
		t.Fatalf("start lifecycle version = %d; want 2", started.LifecycleVersion)
	}
	taskRow := func(taskID id.TaskID, title, state string, lifecycle int64, executionID id.ExecutionID) map[string]any {
		return map[string]any{
			"id": string(taskID), "tenant_id": "tenant-1", "project_id": "project-1",
			"content_version": int64(1), "lifecycle_version": lifecycle, "state": state,
			"pipeline_id": string(applied.PipelineID), "current_execution_id": string(executionID),
			"initiator_id": "principal-1", "title": title, "description": pgtest.NullValue,
			"metadata": pgtest.NullValue, "inputs": pgtest.NullValue,
			"execution_history": `["` + string(executionID) + `"]`,
			"question_id":       pgtest.NullValue, "question_body": pgtest.NullValue, "question_assignment": pgtest.NullValue,
			"question_references": pgtest.NullValue, "question_version": pgtest.NullValue,
			"question_asked_by": pgtest.NullValue, "question_at": pgtest.NullValue, "created_at": pgtest.FixtureTime,
		}
	}
	progressUnit := task.ProgressUnit{
		Owner: env.scope, TaskID: created.TaskID, ExecutionID: started.ExecutionID,
		ExpectedLifecycleVersion: 2, At: pgtest.FixtureTime,
	}
	requireVersionConflict := func(label string, err error) {
		t.Helper()
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("%s error = %v; want version_conflict", label, err)
		}
	}

	progressObserver := env.shard.ObserveUnit(t, "task", "progress")
	progressed, err := env.reader.Apply(ctx, progressUnit)
	if err != nil {
		t.Fatalf("task progress: %v%s", err, pgtest.CauseText(err))
	}
	if progressed.LifecycleVersion != 3 || progressed.Replayed {
		t.Fatalf("task progress result = %+v; want lifecycle version 3, not replayed", progressed)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "tasks", map[string]any{"id": string(created.TaskID)},
		taskRow(created.TaskID, "Fix the build", "done", 3, started.ExecutionID))
	progressObserver.Finish(map[string]int{"tasks": 1})
	current, err := env.reader.Get(ctx, env.scope, created.TaskID)
	if err != nil {
		t.Fatalf("read progressed task: %v%s", err, pgtest.CauseText(err))
	}
	if current.State != task.StateDone || current.LifecycleVersion != 3 || current.CurrentExecution != started.ExecutionID {
		t.Fatalf("progressed task = %+v; want the current execution at done/3", current)
	}

	// The same unit a second time is stale: the task is no longer at lifecycle 2.
	staleObserver := env.shard.ObserveUnit(t, "task", "progress.stale")
	_, err = env.reader.Apply(ctx, progressUnit)
	requireVersionConflict("stale task progress", err)
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "tasks", map[string]any{"id": string(created.TaskID)},
		taskRow(created.TaskID, "Fix the build", "done", 3, started.ExecutionID))
	staleObserver.Finish(map[string]int{})

	// A completion for an execution that is not the task's current one is
	// refused even at the right lifecycle version.
	other, err := env.task.Create(ctx, task.CreateCommand{
		Actor: actor, Scope: env.scope, Title: "Other task", PipelineID: applied.PipelineID,
		CallerKey: "task-create-other", Content: integrationContent("task-create-other"),
	})
	if err != nil {
		t.Fatalf("second task create: %v", err)
	}
	otherStarted, err := env.task.Start(ctx, task.StartCommand{
		Actor: actor, Scope: env.scope, TaskID: other.TaskID, ExpectedLifecycleVersion: 1,
		CallerKey: "task-start-other", Content: integrationContent("task-start-other"),
	})
	if err != nil {
		t.Fatalf("second task start: %v%s", err, pgtest.CauseText(err))
	}
	if otherStarted.LifecycleVersion != 2 || otherStarted.ExecutionID == "execution-other" {
		t.Fatalf("second task start = %+v; want lifecycle 2 and an execution other than execution-other", otherStarted)
	}
	wrongObserver := env.shard.ObserveUnit(t, "task", "progress.wrong_execution")
	_, err = env.reader.Apply(ctx, task.ProgressUnit{
		Owner: env.scope, TaskID: other.TaskID, ExecutionID: "execution-other",
		ExpectedLifecycleVersion: 2, At: pgtest.FixtureTime,
	})
	requireVersionConflict("task progress for another execution", err)
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "tasks", map[string]any{"id": string(other.TaskID)},
		taskRow(other.TaskID, "Other task", "open", 2, otherStarted.ExecutionID))
	wrongObserver.Finish(map[string]int{})
}

//go:build component

package postgrescomponent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// requestRaceEnv binds the two services and the application-role handle used
// by the resolve/outputs-versus-cancel races.
type requestRaceEnv struct {
	shard     *pgtest.Shard
	scope     tenancy.Scope
	runtime   *pgdriver.DB
	execution *execution.Service
	request   *request.Service
	ids       *integrationIDs
	actor     authorize.Principal
	// now is the one instant both services read as their clock. It is truncated
	// to the microsecond PostgreSQL stores, so every timestamp the services
	// write (receipt, audit, event, answer, cleanup retry) is exactly now.
	now time.Time
}

func newRequestRaceEnv(t *testing.T) *requestRaceEnv {
	t.Helper()
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedMembership(ctx, scope, "membership-1", "principal-1", 1,
		string(tenancy.RoleEngineer), string(tenancy.RoleAdmin))
	runtime := shard.Runtime(6)
	t.Cleanup(runtime.Close)
	execStore := executionstore.New(storepg.New(runtime), storepg.New(runtime))
	ids := &integrationIDs{}
	now := time.Now().UTC().Truncate(time.Microsecond)
	executionService, err := execution.NewService(execution.Config{
		Clock: integrationClock{now: now}, IDs: ids,
		Authorizer: allowAuthorizer{}, Authority: confirmAuthority{},
		Reader: execStore, State: execStore, Transitions: execStore, Starts: execStore,
	})
	if err != nil {
		t.Fatalf("execution.NewService: %v", err)
	}
	requestService, err := request.NewService(request.Config{
		Clock: integrationClock{now: now}, IDs: ids,
		Authorizer: allowAuthorizer{}, Authority: confirmAuthority{},
		State: execStore, Activations: execStore, Resolutions: execStore,
	})
	if err != nil {
		t.Fatalf("request.NewService: %v", err)
	}
	actor := humanAdmin(scope, "principal-1")
	actor.Roles = []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleAdmin}
	return &requestRaceEnv{
		shard: shard, scope: scope, runtime: runtime,
		execution: executionService, request: requestService, ids: ids, actor: actor, now: now,
	}
}

// commitIDs are the deterministic ids one service call draws, in the order the
// services draw them: the receipt, then the audit row, then the owner event.
type commitIDs struct{ receipt, audit, event string }

// nextCommitIDs returns the ids the next Resolve, Cancel or StageOutputs call
// allocates. Call it before the service call.
func (env *requestRaceEnv) nextCommitIDs() commitIDs {
	saved := env.ids.snapshot()
	return commitIDs{
		receipt: fmt.Sprintf("receipt-%d", saved.receipt+1),
		audit:   fmt.Sprintf("event-%d", saved.event+1),
		event:   fmt.Sprintf("event-%d", saved.event+2),
	}
}

// seededNonce is the exact credential nonce the running-execution seeds write
// with decode(repeat('05',32),'hex').
var seededNonce = bytes.Repeat([]byte{5}, 32)

// seededTimes are the database-clock readings a running-execution seed wrote
// with now(): created_at, token_expires_at and the action deadline. They are
// read back once, before any unit runs, so every later golden row compares the
// exact value the seed stored and a unit that rewrites one is caught.
type seededTimes struct{ created, tokenExpires, deadline time.Time }

func readSeededTimes(t *testing.T, ctx context.Context, shard *pgtest.Shard, scope tenancy.Scope, executionID id.ExecutionID) seededTimes {
	t.Helper()
	row := scopedRow(t, ctx, shard.Admin(), scope, 3,
		`SELECT (extract(epoch FROM e.created_at) * 1000000)::bigint::text,
		        (extract(epoch FROM e.token_expires_at) * 1000000)::bigint::text,
		        (extract(epoch FROM a.deadline) * 1000000)::bigint::text
		 FROM executions e JOIN actions a ON a.execution_id = e.id WHERE e.id = $1`, string(executionID))
	var micros [3]int64
	for index, text := range row {
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			t.Fatalf("seeded timestamp %d = %q: %v", index, text, err)
		}
		micros[index] = value
	}
	return seededTimes{
		created:      time.UnixMicro(micros[0]).UTC(),
		tokenExpires: time.UnixMicro(micros[1]).UTC(),
		deadline:     time.UnixMicro(micros[2]).UTC(),
	}
}

// eventWant is one expected durable events row. Every column other than these
// is fixed by the shell producer: the tenant/project scope, no report type, no
// correlation or causation, a pending publication state.
type eventWant struct {
	id, typ, subject, principal string
	version                     int64
	data                        any
}

// requireEvent asserts one whole events row written by a service call at env.now.
func (env *requestRaceEnv) requireEvent(t *testing.T, ctx context.Context, want eventWant) {
	t.Helper()
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": want.id}, map[string]any{
		"id": want.id, "tenant_id": "tenant-1", "project_id": "project-1",
		"type": want.typ, "report_type": pgtest.NullValue,
		"subject_id": want.subject, "subject_version": want.version,
		"source_kind": "shell", "source_principal": want.principal,
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": env.now, "data": want.data, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
}

// The lifecycle projection stamps these bounded facts (internal/shell/event
// contract_lifecycle.go) on every projected event; the shell producer for a
// projected event is the trusted "system" principal.
const lifecycleProducer = "system"

func lifecycleExecutionData(executionID id.ExecutionID, previous, state string, version int) any {
	return pgtest.GoldenJSONOf(map[string]any{
		"execution_id": string(executionID), "previous_state": previous, "state": state, "version": version,
	})
}

func lifecycleFailureData(executionID id.ExecutionID, previous, state string, version int, kind, reason string) any {
	return pgtest.GoldenJSONOf(map[string]any{
		"execution_id": string(executionID), "previous_state": previous, "state": state, "version": version,
		"failure_kind": kind, "reason": reason,
	})
}

func lifecycleCleanupData(executionID id.ExecutionID, state string) any {
	return pgtest.GoldenJSONOf(map[string]any{"execution_id": string(executionID), "cleanup_state": state})
}

func lifecycleActionData(actionID id.ActionID, index int, kind, previous, state string, version int) any {
	return pgtest.GoldenJSONOf(map[string]any{
		"action_id": string(actionID), "index": index, "kind": kind,
		"previous_state": previous, "state": state, "version": version,
	})
}

// resolveReceiptContent is the retry-identity content the request service
// fingerprints for an answer with no response values: {decision, response}.
func resolveReceiptContent(decision request.Decision) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(`{"decision":"` + string(decision) + `","response":null}`)}
}

// seededRequestYAMLSHA and seededContainerYAMLSHA are the yaml_sha256 values the
// running-execution seeds store.
func seededRequestYAMLSHA() []byte   { return pgtest.SHA256([]byte("request-yaml")) }
func seededContainerYAMLSHA() []byte { return pgtest.SHA256([]byte("output-yaml")) }

// reset truncates the shard and reseeds the scope and membership so each
// deterministic or concurrent variant starts from the same durable state.
func (env *requestRaceEnv) reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	env.shard.TruncateAll(ctx)
	env.shard.SeedScope(ctx, "tenant-1", "project-1")
	env.shard.SeedMembership(ctx, env.scope, "membership-1", "principal-1", 1,
		string(tenancy.RoleEngineer), string(tenancy.RoleAdmin))
}

// TestRequestResolveVersusCancelOnPostgreSQL races one request resolution and
// one execution cancellation at the same expected version. Exactly one commits;
// the loser gets version_conflict and leaves no receipt, audit or event. Both
// winner orders are driven deterministically, then the concurrent race is
// repeated to exercise the barrier.
func TestRequestResolveVersusCancelOnPostgreSQL(t *testing.T) {
	env := newRequestRaceEnv(t)
	ctx := context.Background()

	// Deterministic resolve winner, then a cancel at the pre-settlement
	// version that must lose.
	env.reset(t)
	executionID, actionID := seedRunningRequest(ctx, t, env.shard, env.scope)
	if _, err := env.request.Resolve(ctx, resolveCommand(env.actor, env.scope, executionID, actionID)); err != nil {
		t.Fatalf("resolve winner: %v%s", err, pgtest.CauseText(err))
	}
	_, err := env.execution.Cancel(ctx, cancelCommand(env.actor, env.scope, executionID, "cancel-resolve", `{"cancel":1}`))
	requireRaceLoser(t, err, "cancel after resolve commit")
	requireResolveWinner(t, ctx, env, executionID, actionID)

	// Deterministic cancel winner, then a resolve at the pre-settlement
	// versions that must lose.
	env.reset(t)
	executionID, actionID = seedRunningRequest(ctx, t, env.shard, env.scope)
	seeded := readSeededTimes(t, ctx, env.shard, env.scope, executionID)
	cancelRowsObserver := env.shard.ObserveUnit(t, "execution", "cancel")
	cancelIDs := env.nextCommitIDs()
	if _, err := env.execution.Cancel(ctx, cancelCommand(env.actor, env.scope, executionID, "cancel-1", `{"cancel":2}`)); err != nil {
		t.Fatalf("cancel winner: %v%s", err, pgtest.CauseText(err))
	}
	_, err = env.request.Resolve(ctx, resolveCommand(env.actor, env.scope, executionID, actionID))
	requireRaceLoser(t, err, "resolve after cancel commit")
	requireCancelOverResolve(t, ctx, env, executionID, actionID)
	requireCancelledRows(t, ctx, env, executionID, actionID, seeded, cancelIDs)
	// derived: the cancel reduction records pending cleanup at the commit time
	// (internal/shell/store/units/execution/commit.go planCleanup); the row id is
	// sha256(executionID || "\x00cleanup")[:16] and the workspace is the seeded one.
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "cleanup", map[string]any{"execution_id": string(executionID)}, map[string]any{
		"id": "cln-0fc711f8490a1b9f6525f58a074624c3", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": string(executionID), "workload_id": pgtest.NullValue,
		"workspace_id": "workspace-request",
		"state":        "pending", "attempts": int64(0), "last_error": pgtest.NullValue, "next_retry_at": env.now,
	})
	// The projected lifecycle events of the cancel commit, by their
	// hash-derived ids (event.lifecycleEventID over execution id, committed
	// version, type and subject): cleanup progress, then the cancelled action.
	env.requireEvent(t, ctx, eventWant{
		id:  "event-ad6217fd8dc617dfd403f63ec8f2506b43d1e16402dac8954b913663d0350f0f",
		typ: "execution.cleanup", subject: string(executionID), version: 2, principal: lifecycleProducer,
		data: lifecycleCleanupData(executionID, "pending"),
	})
	env.requireEvent(t, ctx, eventWant{
		id:  "event-907a594befb2d5db87d5ad1b423213781f840925a958d366402187c25563ac4b",
		typ: "action.transition", subject: string(actionID), version: 2, principal: lifecycleProducer,
		data: lifecycleActionData(actionID, 0, "request", "running", "cancelled", 2),
	})
	cancelRowsObserver.Finish(map[string]int{"executions": 1, "actions": 1, "cleanup": 1, "receipts": 1, "audit": 1, "events": 3})

	// Concurrent race: exactly one commits, the loser is a version conflict,
	// and the winner's durable state is checked every iteration.
	for iteration := range 10 {
		env.reset(t)
		executionID, actionID = seedRunningRequest(ctx, t, env.shard, env.scope)
		resolveErr, cancelErr := raceResolveAndCancel(ctx, env, executionID, actionID)
		switch {
		case resolveErr == nil && cancelErr == nil:
			t.Fatalf("iteration %d: both resolve and cancel committed", iteration)
		case resolveErr == nil:
			requireRaceLoser(t, cancelErr, "concurrent cancel loser")
			requireResolveWinner(t, ctx, env, executionID, actionID)
		case cancelErr == nil:
			requireRaceLoser(t, resolveErr, "concurrent resolve loser")
			requireCancelOverResolve(t, ctx, env, executionID, actionID)
		default:
			t.Fatalf("iteration %d: both failed: resolve=%v cancel=%v", iteration, resolveErr, cancelErr)
		}
	}
}

// TestOutputSubmissionVersusCancelOnPostgreSQL races one output submission and
// one execution cancellation at the same expected version. Exactly one commits;
// the winner's staged outputs, receipt, audit and events are durable and the
// loser leaves none. Both winner orders are driven deterministically, then the
// concurrent race is repeated.
func TestOutputSubmissionVersusCancelOnPostgreSQL(t *testing.T) {
	env := newRequestRaceEnv(t)
	ctx := context.Background()
	submitted := kernel.Outputs{id.OutputKey("result"): "ok"}

	// Deterministic outputs winner, then a cancel that must lose.
	env.reset(t)
	executionID, actionID := seedRunningContainer(ctx, t, env.shard, env.scope)
	seeded := readSeededTimes(t, ctx, env.shard, env.scope, executionID)
	outputsObserver := env.shard.ObserveUnit(t, "execution", "outputs")
	outputsIDs := env.nextCommitIDs()
	outputsContent := outputsCommand(env.actor, env.scope, executionID, actionID, "outputs-1", submitted).Content
	if _, err := env.execution.StageOutputs(ctx, outputsCommand(env.actor, env.scope, executionID, actionID, "outputs-1", submitted)); err != nil {
		t.Fatalf("outputs winner: %v%s", err, pgtest.CauseText(err))
	}
	_, err := env.execution.Cancel(ctx, cancelCommand(env.actor, env.scope, executionID, "cancel-2", `{"cancel":2}`))
	requireRaceLoser(t, err, "cancel after outputs commit")
	requireOutputsWinner(t, ctx, env, executionID, actionID, submitted)
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": pgtest.NullValue, "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "running", "version": int64(2),
		"yaml_sha256": seededContainerYAMLSHA(), "snapshot": pgtest.GoldenJSONOf(rawJSON(runningContainerDocument(t))),
		"token_nonce": seededNonce, "token_expires_at": seeded.tokenExpires,
		"workspace_id": "workspace-output", "failure": pgtest.NullValue, "created_at": seeded.created,
	})
	// derived: staging reduces to a running->running action commit at the
	// service clock, and actionCommitValues stamps activated_at from commit.At
	// when the seeded row has none (store/units/execution/commit.go).
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "run", "kind": "container", "state": "running", "version": int64(2),
		"timeout_ms": int64(3600000), "deadline": seeded.deadline,
		"outputs": pgtest.NullValue, "staged_outputs": `{"result":"ok"}`, "staged_outputs_set": true, "stop": pgtest.NullValue,
		"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
		"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": env.now, "answer_provenance": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{
		"operation": "execution.action.outputs", "caller_key": "outputs-1",
	}, map[string]any{
		"id": outputsIDs.receipt, "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "execution.action.outputs", "caller_key": "outputs-1",
		"content_sha256": pgtest.ContentSHA256(t, outputsContent),
		"result": pgtest.GoldenJSONOf(execution.TransitionResult{
			ExecutionVersion: 2, ActionVersion: 2,
			NextExecution: kernel.ExecutionRunning, NextAction: kernel.ActionRunning,
		}),
		"completed_at": env.now, "execution_id": string(executionID),
		"snapshot_sha256": pgtest.NullValue, "created_at": env.now,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"operation": "execution.action.outputs"}, map[string]any{
		"id": outputsIDs.audit, "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "execution.action.outputs", "at": env.now, "detail": pgtest.NullValue,
	})
	env.requireEvent(t, ctx, eventWant{
		id: outputsIDs.event, typ: "action.transition", subject: string(executionID), version: 2,
		principal: "principal-1", data: pgtest.NullValue,
	})
	// Settlement promotes the staged outputs in one commit.
	settleStore := executionstore.New(storepg.New(env.runtime), storepg.New(env.runtime))
	minusOne := -1
	if _, err := settleStore.Transition(ctx, execution.TransitionUnit{
		Owner: env.scope, ExecutionID: executionID, ExpectedExecutionVersion: 2, ExpectedActionVersion: 2,
		Commit: flow.Commit{ExecutionID: executionID, ExpectedExecutionVersion: 2,
			NextExecutionState: kernel.ExecutionSucceeded, ActionID: actionID,
			ExpectedActionVersion: 2, NextActionState: kernel.ActionSucceeded, NextCurrentAction: &minusOne,
			Output: flow.OutputChange{Action: flow.OutputPromote, Values: submitted}, At: time.Now()},
	}); err != nil {
		t.Fatalf("settle outputs: %v%s", err, pgtest.CauseText(err))
	}
	settledAction := scopedRow(t, ctx, env.runtime, env.scope, 5,
		`SELECT state, version::text, outputs::text, (staged_outputs IS NULL)::text, staged_outputs_set::text
		 FROM actions WHERE id = $1`, string(actionID))
	if settledAction[0] != "succeeded" || settledAction[1] != "3" || settledAction[2] != `{"result": "ok"}` ||
		settledAction[3] != "true" || settledAction[4] != "false" {
		t.Fatalf("settled action = %v; want succeeded/3/promoted/cleared", settledAction)
	}
	settledExecution := scopedRow(t, ctx, env.runtime, env.scope, 3,
		`SELECT state, version::text, (failure IS NULL)::text FROM executions WHERE id = $1`, string(executionID))
	if settledExecution[0] != "succeeded" || settledExecution[1] != "3" || settledExecution[2] != "true" {
		t.Fatalf("settled execution = %v; want succeeded/3/no failure", settledExecution)
	}
	outputsObserver.Finish(map[string]int{"executions": 1, "actions": 1, "receipts": 1, "audit": 1, "events": 1})

	// Deterministic cancel winner, then an output submission that must lose.
	env.reset(t)
	executionID, actionID = seedRunningContainer(ctx, t, env.shard, env.scope)
	if _, err := env.execution.Cancel(ctx, cancelCommand(env.actor, env.scope, executionID, "cancel-2", `{"cancel":2}`)); err != nil {
		t.Fatalf("cancel winner: %v%s", err, pgtest.CauseText(err))
	}
	_, err = env.execution.StageOutputs(ctx, outputsCommand(env.actor, env.scope, executionID, actionID, "outputs-1", submitted))
	requireRaceLoser(t, err, "outputs after cancel commit")
	requireCancelOverOutputs(t, ctx, env, executionID, actionID)

	// Concurrent race: exactly one commits, the loser is a version conflict,
	// and the winner's durable state is checked every iteration.
	for iteration := range 10 {
		env.reset(t)
		executionID, actionID = seedRunningContainer(ctx, t, env.shard, env.scope)
		outputsErr, cancelErr := raceOutputsAndCancel(ctx, env, executionID, actionID, submitted)
		switch {
		case outputsErr == nil && cancelErr == nil:
			t.Fatalf("iteration %d: both outputs and cancel committed", iteration)
		case outputsErr == nil:
			requireRaceLoser(t, cancelErr, "concurrent cancel loser")
			requireOutputsWinner(t, ctx, env, executionID, actionID, submitted)
		case cancelErr == nil:
			requireRaceLoser(t, outputsErr, "concurrent outputs loser")
			requireCancelOverOutputs(t, ctx, env, executionID, actionID)
		default:
			t.Fatalf("iteration %d: both failed: outputs=%v cancel=%v", iteration, outputsErr, cancelErr)
		}
	}
}

func resolveCommand(actor authorize.Principal, scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID) request.ResolveCommand {
	return request.ResolveCommand{
		Actor: actor, Scope: scope, ExecutionID: executionID, ActionID: actionID,
		ExpectedExecutionVersion: 1, ExpectedActionVersion: 1,
		Decision: request.DecisionPass,
	}
}

func cancelCommand(actor authorize.Principal, scope tenancy.Scope, executionID id.ExecutionID, callerKey, content string) execution.CancelCommand {
	return execution.CancelCommand{
		Actor: actor, Scope: scope, ExecutionID: executionID,
		ExpectedExecutionVersion: 1, CallerKey: callerKey,
		Content: receipt.Content{MediaType: "application/json", Bytes: []byte(content)},
	}
}

func outputsCommand(actor authorize.Principal, scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID, callerKey string, outputs kernel.Outputs) execution.OutputsCommand {
	return execution.OutputsCommand{
		Actor: actor, Scope: scope, ExecutionID: executionID, ActionID: actionID,
		ExpectedExecutionVersion: 1, Outputs: outputs, CallerKey: callerKey,
		Content: receipt.Content{MediaType: "application/json", Bytes: []byte(`{"outputs":1}`)},
	}
}

func raceResolveAndCancel(ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID, actionID id.ActionID) (resolveErr, cancelErr error) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, resolveErr = env.request.Resolve(ctx, resolveCommand(env.actor, env.scope, executionID, actionID))
	}()
	go func() {
		defer wg.Done()
		<-start
		_, cancelErr = env.execution.Cancel(ctx, cancelCommand(env.actor, env.scope, executionID, "cancel-race", `{"cancel":3}`))
	}()
	close(start)
	wg.Wait()
	return resolveErr, cancelErr
}

func raceOutputsAndCancel(ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID, actionID id.ActionID, submitted kernel.Outputs) (outputsErr, cancelErr error) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, outputsErr = env.execution.StageOutputs(ctx, outputsCommand(env.actor, env.scope, executionID, actionID, "outputs-race", submitted))
	}()
	go func() {
		defer wg.Done()
		<-start
		_, cancelErr = env.execution.Cancel(ctx, cancelCommand(env.actor, env.scope, executionID, "cancel-race", `{"cancel":4}`))
	}()
	close(start)
	wg.Wait()
	return outputsErr, cancelErr
}

// requireRaceLoser accepts the two declared refusal classes a race loser can
// observe: a version conflict when it read the pre-commit version, or a changed
// content refusal when it read the winner's settled state after the commit.
func requireRaceLoser(t *testing.T, err error, label string) {
	t.Helper()
	class, ok := fault.ClassOf(err)
	if !ok || (class != fault.VersionConflict && class != fault.ChangedIdempotencyContent) {
		t.Fatalf("%s error = %v; want version_conflict or changed_idempotency_content", label, err)
	}
}

// requireResolveWinner proves the resolve commit is durable and the cancel
// loser left no state, receipt, audit or event behind.
func requireResolveWinner(t *testing.T, ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID, actionID id.ActionID) {
	t.Helper()
	executionRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT state, version FROM executions WHERE id = $1`, string(executionID))
	if executionRow[0] != "succeeded" || executionRow[1] != "2" {
		t.Fatalf("resolved execution state/version = %v; want succeeded/2", executionRow)
	}
	requireTokenCleared(t, ctx, env, executionID)
	actionRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT state, version FROM actions WHERE id = $1`, string(actionID))
	if actionRow[0] != "succeeded" || actionRow[1] != "2" {
		t.Fatalf("resolved action state/version = %v; want succeeded/2", actionRow)
	}
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM receipts WHERE operation = 'execution.request.resolve'`, 1, "resolve receipts")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM receipts WHERE operation = 'execution.cancel'`, 0, "cancel receipts after resolve")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM audit WHERE operation = 'execution.request.resolve'`, 1, "resolve audit")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM audit WHERE operation = 'execution.cancel'`, 0, "cancel audit after resolve")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM events WHERE type = 'request.resolved'`, 1, "resolve events")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM events WHERE type = 'execution.cancelled'`, 0, "cancel events after resolve")
}

// requireOutputsWinner proves the staged outputs, receipt, audit and event are
// durable and the cancel loser left no state or rows behind.
func requireOutputsWinner(t *testing.T, ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID, actionID id.ActionID, submitted kernel.Outputs) {
	t.Helper()
	executionRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT state, version FROM executions WHERE id = $1`, string(executionID))
	if executionRow[0] != "running" || executionRow[1] != "2" {
		t.Fatalf("staged execution state/version = %v; want running/2", executionRow)
	}
	actionRow := scopedRow(t, ctx, env.runtime, env.scope, 4,
		`SELECT state, version, staged_outputs_set::text, COALESCE(staged_outputs::text, '')
		 FROM actions WHERE id = $1`, string(actionID))
	if actionRow[0] != "running" || actionRow[1] != "2" || actionRow[2] != "true" {
		t.Fatalf("staged action state/version/set = %v; want running/2/true", actionRow[:3])
	}
	var staged map[string]string
	if err := json.Unmarshal([]byte(actionRow[3]), &staged); err != nil {
		t.Fatalf("decode staged outputs %q: %v", actionRow[3], err)
	}
	want := make(map[string]string, len(submitted))
	for key, value := range submitted {
		want[string(key)] = value
	}
	if !reflect.DeepEqual(staged, want) {
		t.Fatalf("staged outputs = %v; want %v", staged, want)
	}
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM receipts WHERE operation = 'execution.action.outputs'`, 1, "outputs receipts")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM receipts WHERE operation = 'execution.cancel'`, 0, "cancel receipts after outputs")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM audit WHERE operation = 'execution.action.outputs'`, 1, "outputs audit")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM audit WHERE operation = 'execution.cancel'`, 0, "cancel audit after outputs")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM events WHERE type = 'action.transition'`, 1, "outputs events")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM events WHERE type = 'execution.cancelled'`, 0, "cancel events after outputs")
}

// requireCancelledRows asserts every row of the cancel winner over the seeded
// running request: the cancelled execution and action, the receipt, the audit
// row and the owner's execution.cancelled event. cancelIDs are the ids the
// Cancel call drew (receipt, audit, event, in that order).
func requireCancelledRows(t *testing.T, ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID, actionID id.ActionID, seeded seededTimes, cancelIDs commitIDs) {
	t.Helper()
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": pgtest.NullValue, "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "cancelled", "version": int64(2),
		"yaml_sha256": seededRequestYAMLSHA(), "snapshot": pgtest.GoldenJSONOf(rawJSON(runningRequestDocument(t))),
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"workspace_id": "workspace-request", "failure": pgtest.NullValue, "created_at": seeded.created,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "ask", "kind": "request", "state": "cancelled", "version": int64(2),
		"timeout_ms": int64(3600000), "deadline": seeded.deadline,
		"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject":    `{"ref":{"kind":"literal","literal":"refs/heads/main"}}`,
		"request_assignment": `{"kind":"role","role":"engineer"}`,
		"request_decision":   pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	// derived: the transition result is {expected+1 versions, next states}
	// (store/units/execution/transition.go); the content is the Cancel command's.
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{
		"operation": "execution.cancel", "caller_key": "cancel-1",
	}, map[string]any{
		"id": cancelIDs.receipt, "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "execution.cancel", "caller_key": "cancel-1",
		"content_sha256": pgtest.ContentSHA256(t, cancelCommand(env.actor, env.scope, executionID, "cancel-1", `{"cancel":2}`).Content),
		"result": pgtest.GoldenJSONOf(execution.TransitionResult{
			ExecutionVersion: 2, ActionVersion: 2,
			NextExecution: kernel.ExecutionCancelled, NextAction: kernel.ActionCancelled,
		}),
		"completed_at": env.now, "execution_id": string(executionID),
		"snapshot_sha256": pgtest.NullValue, "created_at": env.now,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"operation": "execution.cancel"}, map[string]any{
		"id": cancelIDs.audit, "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "execution.cancel", "at": env.now, "detail": pgtest.NullValue,
	})
	env.requireEvent(t, ctx, eventWant{
		id: cancelIDs.event, typ: "execution.cancelled", subject: string(executionID), version: 2,
		principal: "principal-1", data: pgtest.NullValue,
	})
}

// requireCancelDurableState proves a cancel commit is durable: the execution
// and action are cancelled, no staged outputs remain, and exactly one cancel
// receipt, audit row and event exist.
func requireCancelDurableState(t *testing.T, ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID, actionID id.ActionID) {
	t.Helper()
	executionRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT state, version FROM executions WHERE id = $1`, string(executionID))
	if executionRow[0] != "cancelled" || executionRow[1] != "2" {
		t.Fatalf("cancelled execution state/version = %v; want cancelled/2", executionRow)
	}
	requireTokenCleared(t, ctx, env, executionID)
	actionRow := scopedRow(t, ctx, env.runtime, env.scope, 4,
		`SELECT state, version, staged_outputs_set::text, COALESCE(staged_outputs::text, '')
		 FROM actions WHERE id = $1`, string(actionID))
	if actionRow[0] != "cancelled" || actionRow[1] != "2" || actionRow[2] != "false" || actionRow[3] != "" {
		t.Fatalf("cancelled action state/version/staged = %v; want cancelled/2/false/<empty>", actionRow)
	}
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM receipts WHERE operation = 'execution.cancel'`, 1, "cancel receipts")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM audit WHERE operation = 'execution.cancel'`, 1, "cancel audit")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM events WHERE type = 'execution.cancelled'`, 1, "cancel events")
}

// requireCancelOverResolve adds the resolve-loser checks to a cancel winner.
func requireCancelOverResolve(t *testing.T, ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID, actionID id.ActionID) {
	t.Helper()
	requireCancelDurableState(t, ctx, env, executionID, actionID)
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM receipts WHERE operation = 'execution.request.resolve'`, 0, "resolve receipts after cancel")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM audit WHERE operation = 'execution.request.resolve'`, 0, "resolve audit after cancel")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM events WHERE type = 'request.resolved'`, 0, "resolve events after cancel")
}

// requireCancelOverOutputs adds the outputs-loser checks to a cancel winner.
// The cancel projection emits exactly one action.transition event; an outputs
// commit would add a second, so the count proves the loser persisted nothing.
func requireCancelOverOutputs(t *testing.T, ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID, actionID id.ActionID) {
	t.Helper()
	requireCancelDurableState(t, ctx, env, executionID, actionID)
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM receipts WHERE operation = 'execution.action.outputs'`, 0, "outputs receipts after cancel")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM audit WHERE operation = 'execution.action.outputs'`, 0, "outputs audit after cancel")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM events WHERE type = 'action.transition'`, 1, "action transition events after cancel")
}

// TestExecutionSettlementPersistsOutputsAndFailureOnPostgreSQL proves a
// settling commit persists the promoted outputs and a typed failure in the same
// durable state as the settlement.
func TestExecutionSettlementPersistsOutputsAndFailureOnPostgreSQL(t *testing.T) {
	ctx := context.Background()

	t.Run("promoted outputs", func(t *testing.T) {
		env := newRequestRaceEnv(t)
		env.reset(t)
		executionID, actionID := seedRunningContainer(ctx, t, env.shard, env.scope)
		seeded := readSeededTimes(t, ctx, env.shard, env.scope, executionID)
		store := executionstore.New(storepg.New(env.runtime), storepg.New(env.runtime))
		staged := kernel.Outputs{id.OutputKey("result"): "staged"}
		settled := kernel.Outputs{id.OutputKey("result"): "settled"}
		settleObserver := env.shard.ObserveUnit(t, "execution", "settlement.pass")
		stage, err := store.Transition(ctx, execution.TransitionUnit{
			Owner: env.scope, ExecutionID: executionID, ExpectedExecutionVersion: 1, ExpectedActionVersion: 1,
			Commit: flow.Commit{ExecutionID: executionID, ExpectedExecutionVersion: 1,
				NextExecutionState: kernel.ExecutionRunning, ActionID: actionID,
				ExpectedActionVersion: 1, NextActionState: kernel.ActionRunning,
				Output: flow.OutputChange{Action: flow.OutputStage, Values: staged}, At: pgtest.FixtureTime},
		})
		if err != nil {
			t.Fatalf("stage outputs: %v%s", err, pgtest.CauseText(err))
		}
		minusOne := -1
		if _, err := store.Transition(ctx, execution.TransitionUnit{
			Owner: env.scope, ExecutionID: executionID,
			ExpectedExecutionVersion: stage.ExecutionVersion, ExpectedActionVersion: stage.ActionVersion,
			Commit: flow.Commit{ExecutionID: executionID,
				ExpectedExecutionVersion: stage.ExecutionVersion, NextExecutionState: kernel.ExecutionSucceeded,
				ActionID: actionID, ExpectedActionVersion: stage.ActionVersion, NextActionState: kernel.ActionSucceeded,
				NextCurrentAction: &minusOne,
				Output:            flow.OutputChange{Action: flow.OutputPromote, Values: settled}, At: pgtest.FixtureTime},
		}); err != nil {
			t.Fatalf("settle outputs: %v%s", err, pgtest.CauseText(err))
		}
		actionRow := scopedRow(t, ctx, env.runtime, env.scope, 5,
			`SELECT state, outputs::text, (staged_outputs IS NULL)::text, staged_outputs_set::text, version::text
			 FROM actions WHERE id = $1`, string(actionID))
		if actionRow[0] != "succeeded" || !jsonEqual(actionRow[1], `{"result":"settled"}`) ||
			actionRow[2] != "true" || actionRow[3] != "false" || actionRow[4] != "3" {
			t.Fatalf("settled action = %v; want succeeded/settled outputs/cleared staged/false/3", actionRow)
		}
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
			"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
			"pipeline_id": pgtest.NullValue, "task_id": pgtest.NullValue,
			"principal_id": "execution-principal-1", "state": "succeeded", "version": int64(3),
			"yaml_sha256": seededContainerYAMLSHA(), "snapshot": pgtest.GoldenJSONOf(rawJSON(runningContainerDocument(t))),
			"token_nonce": seededNonce, "token_expires_at": seeded.tokenExpires,
			"workspace_id": "workspace-output", "failure": pgtest.NullValue, "created_at": seeded.created,
		})
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
			"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
			"index": int64(0), "name": "run", "kind": "container", "state": "succeeded", "version": int64(3),
			"timeout_ms": int64(3600000), "deadline": seeded.deadline,
			"outputs": `{"result":"settled"}`, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
			"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
			"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
			"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
			"observation": pgtest.NullValue, "activated_at": pgtest.FixtureTime, "answer_provenance": pgtest.NullValue,
		})
		settleObserver.Finish(map[string]int{"executions": 1, "actions": 1})
		executionRow := scopedRow(t, ctx, env.runtime, env.scope, 3,
			`SELECT state, version::text, (failure IS NULL)::text FROM executions WHERE id = $1`, string(executionID))
		if executionRow[0] != "succeeded" || executionRow[1] != "3" || executionRow[2] != "true" {
			t.Fatalf("settled execution = %v; want succeeded/3/no failure", executionRow)
		}
	})

	t.Run("plain transition", func(t *testing.T) {
		env := newRequestRaceEnv(t)
		env.reset(t)
		executionID, actionID := seedRunningContainer(ctx, t, env.shard, env.scope)
		seeded := readSeededTimes(t, ctx, env.shard, env.scope, executionID)
		store := executionstore.New(storepg.New(env.runtime), storepg.New(env.runtime))
		transitionObserver := env.shard.ObserveUnit(t, "execution", "transition")
		if _, err := store.Transition(ctx, execution.TransitionUnit{
			Owner: env.scope, ExecutionID: executionID, ExpectedExecutionVersion: 1, ExpectedActionVersion: 1,
			Commit: flow.Commit{ExecutionID: executionID, ExpectedExecutionVersion: 1,
				NextExecutionState: kernel.ExecutionRunning, ActionID: actionID,
				ExpectedActionVersion: 1, NextActionState: kernel.ActionRunning,
				Output: flow.OutputChange{Action: flow.OutputNone}, At: pgtest.FixtureTime},
		}); err != nil {
			t.Fatalf("plain transition: %v%s", err, pgtest.CauseText(err))
		}
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
			"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
			"pipeline_id": pgtest.NullValue, "task_id": pgtest.NullValue,
			"principal_id": "execution-principal-1", "state": "running", "version": int64(2),
			"yaml_sha256": seededContainerYAMLSHA(), "snapshot": pgtest.GoldenJSONOf(rawJSON(runningContainerDocument(t))),
			"token_nonce": seededNonce, "token_expires_at": seeded.tokenExpires,
			"workspace_id": "workspace-output", "failure": pgtest.NullValue,
			"created_at": seeded.created,
		})
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
			"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
			"index": int64(0), "name": "run", "kind": "container", "state": "running", "version": int64(2),
			"timeout_ms": int64(3600000), "deadline": seeded.deadline,
			"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
			"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
			"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
			"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
			"observation": pgtest.NullValue, "activated_at": pgtest.FixtureTime, "answer_provenance": pgtest.NullValue,
		})
		transitionObserver.Finish(map[string]int{"executions": 1, "actions": 1})
	})

	t.Run("typed failure", func(t *testing.T) {
		env := newRequestRaceEnv(t)
		env.reset(t)
		executionID, actionID := seedRunningContainer(ctx, t, env.shard, env.scope)
		seeded := readSeededTimes(t, ctx, env.shard, env.scope, executionID)
		store := executionstore.New(storepg.New(env.runtime), storepg.New(env.runtime))
		minusOne := -1
		failedObserver := env.shard.ObserveUnit(t, "execution", "settlement.failed")
		if _, err := store.Transition(ctx, execution.TransitionUnit{
			Owner: env.scope, ExecutionID: executionID, ExpectedExecutionVersion: 1, ExpectedActionVersion: 1,
			Commit: flow.Commit{ExecutionID: executionID, ExpectedExecutionVersion: 1,
				NextExecutionState: kernel.ExecutionFailed, ActionID: actionID,
				ExpectedActionVersion: 1, NextActionState: kernel.ActionFailed,
				NextCurrentAction: &minusOne,
				Output:            flow.OutputChange{Action: flow.OutputDiscard},
				Failure:           &kernel.Failure{Kind: kernel.FailureRuntime, Reason: "workload_exited", Detail: "exit status 2"},
				At:                pgtest.FixtureTime},
		}); err != nil {
			t.Fatalf("failed settlement: %v%s", err, pgtest.CauseText(err))
		}
		executionRow := scopedRow(t, ctx, env.runtime, env.scope, 3,
			`SELECT state, version::text, failure::text FROM executions WHERE id = $1`, string(executionID))
		if executionRow[0] != "failed" || executionRow[1] != "2" {
			t.Fatalf("failed execution = %v; want failed/2", executionRow[:2])
		}
		var failure kernel.Failure
		if err := json.Unmarshal([]byte(executionRow[2]), &failure); err != nil {
			t.Fatalf("decode failure %q: %v", executionRow[2], err)
		}
		if failure.Kind != kernel.FailureRuntime || failure.Reason != "workload_exited" || failure.Detail != "exit status 2" {
			t.Fatalf("persisted failure = %+v; want runtime/workload_exited/exit status 2", failure)
		}
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
			"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
			"pipeline_id": pgtest.NullValue, "task_id": pgtest.NullValue,
			"principal_id": "execution-principal-1", "state": "failed", "version": int64(2),
			"yaml_sha256": seededContainerYAMLSHA(), "snapshot": pgtest.GoldenJSONOf(rawJSON(runningContainerDocument(t))),
			"token_nonce": seededNonce, "token_expires_at": seeded.tokenExpires,
			"workspace_id": "workspace-output", "failure": `{"kind":"runtime","reason":"workload_exited","detail":"exit status 2"}`,
			"created_at": seeded.created,
		})
		env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
			"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
			"index": int64(0), "name": "run", "kind": "container", "state": "failed", "version": int64(2),
			"timeout_ms": int64(3600000), "deadline": seeded.deadline,
			"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
			"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
			"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
			"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
			"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
		})
		failedObserver.Finish(map[string]int{"executions": 1, "actions": 1})
		actionRow := scopedRow(t, ctx, env.runtime, env.scope, 2,
			`SELECT state, (outputs IS NULL)::text FROM actions WHERE id = $1`, string(actionID))
		if actionRow[0] != "failed" || actionRow[1] != "true" {
			t.Fatalf("failed action = %v; want failed with no outputs", actionRow)
		}
	})
}

// seedPendingRequestExecution creates a running execution with one pending
// request action through the real execution adapter and engine transition.
func seedPendingRequestExecution(t *testing.T, env *requestRaceEnv, executionID id.ExecutionID) (id.ExecutionID, id.ActionID) {
	t.Helper()
	ctx := context.Background()
	yaml := []byte("actions:\n  - name: ask\n    request: role/engineer\n")
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: yaml})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"ref": {Kind: definition.SubjectLiteral, Literal: "refs/heads/main"},
	})
	action := pipeline.AdmittedAction{
		Index: 0, Name: "ask", Kind: definition.ActionKindRequest, Timeout: 37 * time.Minute,
		Request: &definition.Request{Assignment: "role/engineer", Subject: subject},
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{PipelineID: "pipeline-request", PipelineVersion: 1,
		Snapshot: snapshot, Actions: []pipeline.AdmittedAction{action}})
	if err != nil {
		t.Fatalf("Encode snapshot: %v", err)
	}
	store := executionstore.New(storepg.New(env.runtime), storepg.New(env.runtime))
	startObserver := env.shard.ObserveUnit(t, "execution", "start")
	if _, err := store.Start(ctx, execution.StartUnit{
		Owner: env.scope, PipelineID: "pipeline-request", ExpectedPipelineVersion: 1,
		Snapshot: snapshot, Actions: []pipeline.AdmittedAction{action},
		Initiator: "principal-1", ExecutionPrincipal: "execution-principal-1",
		NewExecutionID: executionID, At: pgtest.FixtureTime,
	}); err != nil {
		t.Fatalf("execution Start: %v%s", err, pgtest.CauseText(err))
	}
	startRow := scopedRow(t, ctx, env.runtime, env.scope, 5,
		`SELECT principal_id, workspace_id, extract(epoch FROM created_at)::bigint::text, state, version::text
		 FROM executions WHERE id = $1`, string(executionID))
	if startRow[0] != "execution-principal-1" || startRow[1] != string(snapshotdoc.DeriveWorkspaceID(executionID)) ||
		startRow[2] != strconv.FormatInt(pgtest.FixtureTime.Unix(), 10) || startRow[3] != "queued" || startRow[4] != "1" {
		t.Fatalf("started execution = %v; want execution-principal-1/%s/%d/queued/1", startRow, snapshotdoc.DeriveWorkspaceID(executionID), pgtest.FixtureTime.Unix())
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": "pipeline-request", "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "queued", "version": int64(1),
		"yaml_sha256": pgtest.SHA256(yaml), "snapshot": pgtest.GoldenJSONOf(rawJSON(document)),
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID(executionID)),
		"failure":      pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(snapshotdoc.DeriveActionID(executionID, 0))}, map[string]any{
		"id": string(snapshotdoc.DeriveActionID(executionID, 0)), "execution_id": string(executionID),
		"tenant_id": "tenant-1", "project_id": "project-1", "index": int64(0),
		"name": "ask", "kind": "request", "state": "pending", "version": int64(1),
		"timeout_ms": int64(2220000), "deadline": pgtest.NullValue, "outputs": pgtest.NullValue,
		"staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject":    pgtest.NullValue,
		"request_assignment": `{"kind":"role","role":"engineer"}`,
		"request_decision":   pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	startObserver.Finish(map[string]int{"executions": 1, "actions": 1})
	if _, err := store.Transition(ctx, execution.TransitionUnit{
		Owner: env.scope, ExecutionID: executionID, ExpectedExecutionVersion: 1,
		Commit: flow.Commit{
			ExecutionID: executionID, ExpectedExecutionVersion: 1,
			NextExecutionState: kernel.ExecutionRunning,
			Output:             flow.OutputChange{Action: flow.OutputNone},
			At:                 pgtest.FixtureTime,
		},
	}); err != nil {
		t.Fatalf("execution running transition: %v%s", err, pgtest.CauseText(err))
	}
	return executionID, snapshotdoc.DeriveActionID(executionID, 0)
}

// TestRequestActivationPersistsExactRowsOnPostgreSQL drives the real request
// activation unit and asserts every durable column, receipt, audit and event it
// writes, including the exact credential pair and timeout.
func TestRequestActivationPersistsExactRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newRequestRaceEnv(t)
	env.reset(t)
	env.shard.SeedPipeline(ctx, env.scope, "pipeline-request", "request",
		[]byte("actions:\n  - name: ask\n    request: role/engineer\n"), 1, "[]")
	executionID, actionID := seedPendingRequestExecution(t, env, "execution-activate-1")
	expiry := pgtest.FixtureTime.Add(29 * time.Minute)
	nonce := tokenNonce(21)
	deadline := pgtest.FixtureTime.Add(37 * time.Minute)
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"ref": {Kind: definition.SubjectLiteral, Literal: "refs/heads/main"},
	})
	yaml := []byte("actions:\n  - name: ask\n    request: role/engineer\n")
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: yaml})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{PipelineID: "pipeline-request", PipelineVersion: 1,
		Snapshot: snapshot, Actions: []pipeline.AdmittedAction{{Index: 0, Name: "ask",
			Kind: definition.ActionKindRequest, Timeout: 37 * time.Minute,
			Request: &definition.Request{Assignment: "role/engineer", Subject: subject}}}})
	if err != nil {
		t.Fatalf("Encode snapshot: %v", err)
	}
	envelope, err := event.NewEnvelope(id.EventID("event-activate-1"),
		event.Source{Kind: event.SourceShell, PrincipalID: "principal-1"}, env.scope, pgtest.FixtureTime,
		event.EnvelopeInput{Type: event.TypeActionRequested, SubjectID: string(actionID), SubjectVersion: 2})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	store := executionstore.New(storepg.New(env.runtime), storepg.New(env.runtime))
	activationObserver := env.shard.ObserveUnit(t, "activation", "commit")
	_, err = store.Activate(ctx, request.ActivationUnit{
		Owner: env.scope, ExecutionID: executionID, ActionID: actionID,
		ExpectedExecutionVersion: 2, ExpectedActionVersion: 1,
		Commit: flow.Commit{ExecutionID: executionID, ExpectedExecutionVersion: 2,
			NextExecutionState: kernel.ExecutionRunning, Output: flow.OutputChange{Action: flow.OutputNone},
			At: pgtest.FixtureTime},
		Subject: subject, Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer},
		Timeout: 37 * time.Minute, Deadline: deadline, At: pgtest.FixtureTime,
		CredentialUpdate: &token.Credential{Nonce: nonce, Expiry: expiry},
		Receipt: receipt.Record{ID: "receipt-activate-1",
			Key: receipt.Key{TenantID: env.scope.TenantID, ProjectID: env.scope.ProjectID,
				PrincipalID: "principal-1", Operation: "execution.request.activate", CallerKey: "activate-1"},
			ContentHash: pgtest.Fingerprint("activate-1"), At: pgtest.FixtureTime},
		Audit:  []event.Audit{{ID: "audit-activate-1", Scope: env.scope, Actor: "principal-1", Operation: "execution.request.activate", At: pgtest.FixtureTime}},
		Events: []event.Envelope{envelope},
	})
	if err != nil {
		t.Fatalf("Activate: %v%s", err, pgtest.CauseText(err))
	}
	executionRow := scopedRow(t, ctx, env.runtime, env.scope, 3,
		`SELECT state, version, encode(token_nonce,'hex') FROM executions WHERE id = $1`, string(executionID))
	if executionRow[0] != "running" || executionRow[1] != "3" || executionRow[2] != hex.EncodeToString(nonce) {
		t.Fatalf("activated execution = %v; want running/3/exact nonce", executionRow)
	}
	expiryRow := scopedRow(t, ctx, env.runtime, env.scope, 1,
		`SELECT extract(epoch FROM token_expires_at)::bigint::text FROM executions WHERE id = $1`, string(executionID))
	if expiryRow[0] != strconv.FormatInt(expiry.Unix(), 10) {
		t.Fatalf("activated token expiry = %s; want %d", expiryRow[0], expiry.Unix())
	}
	actionRow := scopedRow(t, ctx, env.runtime, env.scope, 6,
		`SELECT state, version, timeout_ms::text, extract(epoch FROM deadline)::bigint::text,
		        request_subject::text, request_assignment::text FROM actions WHERE id = $1`, string(actionID))
	if actionRow[0] != "running" || actionRow[1] != "2" || actionRow[2] != "2220000" ||
		actionRow[3] != strconv.FormatInt(deadline.Unix(), 10) {
		t.Fatalf("activated action = %v; want running/2/timeout 2220000/deadline %d", actionRow[:4], deadline.Unix())
	}
	var storedSubject map[string]map[string]string
	if err := json.Unmarshal([]byte(actionRow[4]), &storedSubject); err != nil {
		t.Fatalf("decode request_subject %q: %v", actionRow[4], err)
	}
	if storedSubject["ref"]["kind"] != "literal" || storedSubject["ref"]["literal"] != "refs/heads/main" {
		t.Fatalf("stored request_subject = %v; want the literal ref", storedSubject)
	}
	var storedAssignment request.Assignment
	if err := json.Unmarshal([]byte(actionRow[5]), &storedAssignment); err != nil {
		t.Fatalf("decode request_assignment %q: %v", actionRow[5], err)
	}
	if storedAssignment != (request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer}) {
		t.Fatalf("stored request_assignment = %+v; want role/engineer", storedAssignment)
	}
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM receipts WHERE id = 'receipt-activate-1' AND operation = 'execution.request.activate'`, 1, "activation receipt")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM audit WHERE id = 'audit-activate-1' AND operation = 'execution.request.activate'`, 1, "activation audit")
	requireScopedCount(t, ctx, env.runtime, env.scope,
		`SELECT count(*) FROM events WHERE id = 'event-activate-1' AND type = 'action.requested'`, 1, "activation event")
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": "pipeline-request", "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "running", "version": int64(3),
		"yaml_sha256": pgtest.SHA256(yaml), "snapshot": pgtest.GoldenJSONOf(rawJSON(document)),
		"token_nonce": nonce, "token_expires_at": expiry,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID(executionID)),
		"failure":      pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "ask", "kind": "request", "state": "running", "version": int64(2),
		"timeout_ms": int64(2220000), "deadline": deadline, "outputs": pgtest.NullValue,
		"staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject":    `{"ref":{"kind":"literal","literal":"refs/heads/main"}}`,
		"request_assignment": `{"kind":"role","role":"engineer"}`,
		"request_decision":   pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-activate-1"}, map[string]any{
		"id": "receipt-activate-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "execution.request.activate", "caller_key": "activate-1",
		"content_sha256": pgtest.FingerprintBytes("activate-1"),
		"result": pgtest.GoldenJSONOf(request.ActivationResult{
			ExecutionVersion: 3, ActionVersion: 2, Deadline: deadline,
		}),
		"completed_at": pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "audit-activate-1"}, map[string]any{
		"id": "audit-activate-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "execution.request.activate", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-activate-1"}, map[string]any{
		"id": "event-activate-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "action.requested", "report_type": pgtest.NullValue,
		"subject_id": string(actionID), "subject_version": int64(2),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	activationObserver.Finish(map[string]int{"executions": 1, "actions": 1, "receipts": 1, "audit": 1, "events": 1})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": "pipeline-request", "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "running", "version": int64(3),
		"yaml_sha256": pgtest.SHA256(yaml), "snapshot": string(document),
		"token_nonce": nonce, "token_expires_at": expiry,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID(executionID)),
		"failure":      pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID),
		"tenant_id": "tenant-1", "project_id": "project-1", "index": int64(0),
		"name": "ask", "kind": "request", "state": "running", "version": int64(2),
		"timeout_ms": int64(2220000), "deadline": deadline,
		"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false,
		"stop": pgtest.NullValue, "request_subject": `{"ref":{"kind":"literal","literal":"refs/heads/main"}}`,
		"request_assignment": `{"kind":"role","role":"engineer"}`,
		"request_decision":   pgtest.NullValue, "request_actor": pgtest.NullValue,
		"request_resolved_at": pgtest.NullValue, "workload_id": pgtest.NullValue,
		"launch_state": "not_issued", "launch_key": pgtest.NullValue, "observation": pgtest.NullValue,
		"activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})

	// An activation with no credential update clears the pair rather than
	// persisting a stale one.
	secondID, secondActionID := seedPendingRequestExecution(t, env, "execution-activate-2")
	if _, err := pgtest.ExecSQL(ctx, env.shard.Admin(),
		`UPDATE executions SET token_nonce = decode(repeat('07',32),'hex'), token_expires_at = now() + interval '1 hour' WHERE id = $1`,
		string(secondID)); err != nil {
		t.Fatalf("seed stale credential: %v", err)
	}
	if _, err := store.Activate(ctx, request.ActivationUnit{
		Owner: env.scope, ExecutionID: secondID, ActionID: secondActionID,
		ExpectedExecutionVersion: 2, ExpectedActionVersion: 1,
		Commit: flow.Commit{ExecutionID: secondID, ExpectedExecutionVersion: 2,
			NextExecutionState: kernel.ExecutionRunning, Output: flow.OutputChange{Action: flow.OutputNone},
			At: pgtest.FixtureTime},
		Subject: subject, Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer},
		Timeout: 37 * time.Minute, Deadline: deadline, At: pgtest.FixtureTime,
	}); err != nil {
		t.Fatalf("Activate without credential: %v%s", err, pgtest.CauseText(err))
	}
	cleared := scopedRow(t, ctx, env.runtime, env.scope, 1,
		`SELECT (token_nonce IS NULL AND token_expires_at IS NULL)::text FROM executions WHERE id = $1`, string(secondID))
	if cleared[0] != "true" {
		t.Fatalf("activation without a credential update left a token pair")
	}
}

// requireTokenCleared proves a settlement cleared the live token pair in the
// same durable execution row as the settled state.
func requireTokenCleared(t *testing.T, ctx context.Context, env *requestRaceEnv, executionID id.ExecutionID) {
	t.Helper()
	row := scopedRow(t, ctx, env.runtime, env.scope, 1,
		`SELECT (token_nonce IS NULL AND token_expires_at IS NULL)::text FROM executions WHERE id = $1`,
		string(executionID))
	if row[0] != "true" {
		t.Fatalf("settled execution still carries a token pair")
	}
}

// TestCredentialInstallPersistsExactNonceAndExpiryOnPostgreSQL proves a commit
// that installs a credential persists the exact nonce and expiry it was given.
func TestCredentialInstallPersistsExactNonceAndExpiryOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newRequestRaceEnv(t)
	env.reset(t)
	executionID, _ := seedRunningContainer(ctx, t, env.shard, env.scope)
	expiry := pgtest.FixtureTime.Add(41 * time.Minute)
	nonce := tokenNonce(11)
	store := executionstore.New(storepg.New(env.runtime), storepg.New(env.runtime))
	if _, err := store.Transition(ctx, execution.TransitionUnit{
		Owner: env.scope, ExecutionID: executionID, ExpectedExecutionVersion: 1,
		Commit: flow.Commit{
			ExecutionID: executionID, ExpectedExecutionVersion: 1,
			NextExecutionState: kernel.ExecutionRunning,
			Output:             flow.OutputChange{Action: flow.OutputNone},
			At:                 pgtest.FixtureTime,
		},
		CredentialUpdate: &token.Credential{Nonce: nonce, Expiry: expiry},
	}); err != nil {
		t.Fatalf("install credential: %v%s", err, pgtest.CauseText(err))
	}
	row := scopedRow(t, ctx, env.runtime, env.scope, 2,
		`SELECT encode(token_nonce, 'hex'), extract(epoch FROM token_expires_at)::bigint::text
		 FROM executions WHERE id = $1`, string(executionID))
	if row[0] != hex.EncodeToString(nonce) || row[1] != strconv.FormatInt(expiry.Unix(), 10) {
		t.Fatalf("persisted credential = %v; want nonce %x and expiry %d", row, nonce, expiry.Unix())
	}
}

// runningRequestSeed returns the admitted request action and the snapshot
// document seedRunningRequest stores, so a test can state the stored document
// exactly.
func runningRequestSeed(t *testing.T) (pipeline.AdmittedAction, []byte) {
	t.Helper()
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions:\n  - name: ask\n    request: role/engineer\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	action := pipeline.AdmittedAction{
		Index: 0, Name: "ask", Kind: definition.ActionKindRequest, Timeout: time.Hour,
		Request: &definition.Request{
			Assignment: "role/engineer",
			Subject: definition.NewSubject(map[string]definition.SubjectValue{
				"ref": {Kind: definition.SubjectLiteral, Literal: "refs/heads/main"},
			}),
		},
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		PipelineID: "pipeline-request", PipelineVersion: 1, Snapshot: snapshot,
		Actions: []pipeline.AdmittedAction{action},
	})
	if err != nil {
		t.Fatalf("Encode snapshot: %v", err)
	}
	return action, document
}

func runningRequestDocument(t *testing.T) []byte {
	t.Helper()
	_, document := runningRequestSeed(t)
	return document
}

func seedRunningRequest(ctx context.Context, t *testing.T, shard *pgtest.Shard, scope tenancy.Scope) (id.ExecutionID, id.ActionID) {
	t.Helper()
	executionID := id.ExecutionID("execution-request-1")
	action, document := runningRequestSeed(t)
	rows, err := snapshotdoc.ActionRows(executionID, []pipeline.AdmittedAction{action})
	if err != nil {
		t.Fatalf("ActionRows: %v", err)
	}
	actionID := id.ActionID(rows[0].ID)
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO executions (id, tenant_id, project_id, pipeline_id, task_id, principal_id, state, version,
		 yaml_sha256, snapshot, token_nonce, token_expires_at, workspace_id, created_at)
		 VALUES ($1,$2,$3,NULL,NULL,'execution-principal-1','running',1,$4,$5,decode(repeat('05',32),'hex'),now() + interval '1 hour',$6,now())`,
		string(executionID), scope.TenantID, scope.ProjectID, seededRequestYAMLSHA(), document, "workspace-request"); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	assignment, _ := json.Marshal(request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer})
	subject := `{"ref":{"kind":"literal","literal":"refs/heads/main"}}`
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO actions (id, execution_id, tenant_id, project_id, index, name, kind, state, version,
		 timeout_ms, deadline, staged_outputs_set, launch_state, request_subject, request_assignment)
		 VALUES ($1,$2,$3,$4,0,'ask','request','running',1,3600000,now() + interval '1 hour',false,'not_issued',$5::jsonb,$6::jsonb)`,
		string(actionID), string(executionID), scope.TenantID, scope.ProjectID, subject, assignment); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	return executionID, actionID
}

// runningContainerSeed returns the admitted container action and the snapshot
// document seedRunningContainer stores.
func runningContainerSeed(t *testing.T) (pipeline.AdmittedAction, []byte) {
	t.Helper()
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions:\n  - name: run\n    image: " + integrationImage + "\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	action := pipeline.AdmittedAction{
		Index: 0, Name: "run", Kind: definition.ActionKindContainer, Timeout: time.Hour,
		Container: &definition.Container{
			Image: integrationImage, Commands: []string{"echo", "ok"},
			Outputs: []id.OutputKey{id.OutputKey("result")},
		},
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		PipelineID: "pipeline-output", PipelineVersion: 1, Snapshot: snapshot,
		Actions: []pipeline.AdmittedAction{action},
	})
	if err != nil {
		t.Fatalf("Encode snapshot: %v", err)
	}
	return action, document
}

func runningContainerDocument(t *testing.T) []byte {
	t.Helper()
	_, document := runningContainerSeed(t)
	return document
}

func seedRunningContainer(ctx context.Context, t *testing.T, shard *pgtest.Shard, scope tenancy.Scope) (id.ExecutionID, id.ActionID) {
	t.Helper()
	executionID := id.ExecutionID("execution-output-1")
	action, document := runningContainerSeed(t)
	rows, err := snapshotdoc.ActionRows(executionID, []pipeline.AdmittedAction{action})
	if err != nil {
		t.Fatalf("ActionRows: %v", err)
	}
	actionID := id.ActionID(rows[0].ID)
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO executions (id, tenant_id, project_id, pipeline_id, task_id, principal_id, state, version,
		 yaml_sha256, snapshot, token_nonce, token_expires_at, workspace_id, created_at)
		 VALUES ($1,$2,$3,NULL,NULL,'execution-principal-1','running',1,$4,$5,decode(repeat('05',32),'hex'),now() + interval '1 hour',$6,now())`,
		string(executionID), scope.TenantID, scope.ProjectID, seededContainerYAMLSHA(), document, "workspace-output"); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`INSERT INTO actions (id, execution_id, tenant_id, project_id, index, name, kind, state, version,
		 timeout_ms, deadline, staged_outputs_set, launch_state)
		 VALUES ($1,$2,$3,$4,0,'run','container','running',1,3600000,now() + interval '1 hour',false,'not_issued')`,
		string(actionID), string(executionID), scope.TenantID, scope.ProjectID); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	return executionID, actionID
}

// TestObservedResolveFailOnPostgreSQL observes a failing request resolution.
func TestObservedResolveFailOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newRequestRaceEnv(t)
	env.reset(t)
	executionID, actionID := seedRunningRequest(ctx, t, env.shard, env.scope)
	seeded := readSeededTimes(t, ctx, env.shard, env.scope, executionID)
	observer := env.shard.ObserveUnit(t, "request", "resolve.fail")
	command := resolveCommand(env.actor, env.scope, executionID, actionID)
	command.ExecutionID = executionID
	command.ActionID = actionID
	command.ExpectedExecutionVersion = 1
	command.ExpectedActionVersion = 1
	command.Decision = request.DecisionFail
	resolveIDs := env.nextCommitIDs()
	if _, err := env.request.Resolve(ctx, command); err != nil {
		t.Fatalf("resolve fail: %v%s", err, pgtest.CauseText(err))
	}
	// derived: a failing answer settles the execution failed with the typed
	// runtime/answer_rejected failure (kernel flow reduceAnswer).
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": pgtest.NullValue, "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "failed", "version": int64(2),
		"yaml_sha256": seededRequestYAMLSHA(), "snapshot": pgtest.GoldenJSONOf(rawJSON(runningRequestDocument(t))),
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"workspace_id": "workspace-request",
		"failure": pgtest.GoldenJSONOf(kernel.Failure{
			Kind: kernel.FailureRuntime, Reason: "answer_rejected",
			Detail: "request action was answered with a failing decision",
		}),
		"created_at": seeded.created,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "ask", "kind": "request", "state": "failed", "version": int64(2),
		"timeout_ms": int64(3600000), "deadline": seeded.deadline,
		"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject":    `{"ref":{"kind":"literal","literal":"refs/heads/main"}}`,
		"request_assignment": `{"kind":"role","role":"engineer"}`,
		"request_decision":   "fail", "request_actor": "principal-1", "request_resolved_at": env.now,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": resolveIDs.receipt}, map[string]any{
		"id": resolveIDs.receipt, "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "execution.request.resolve",
		"caller_key":     string(executionID) + "/" + string(actionID),
		"content_sha256": pgtest.ContentSHA256(t, resolveReceiptContent(request.DecisionFail)),
		"result": pgtest.GoldenJSONOf(request.ResolveResult{
			ExecutionVersion: 2, ActionVersion: 2, Decision: request.DecisionFail,
		}),
		"completed_at": env.now, "execution_id": string(executionID),
		"snapshot_sha256": pgtest.NullValue, "created_at": env.now,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": resolveIDs.audit}, map[string]any{
		"id": resolveIDs.audit, "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "execution.request.resolve", "at": env.now, "detail": pgtest.NullValue,
	})
	// derived: a failing answer's terminal commit records pending cleanup at the
	// commit time (kernel flow reduceAnswer; internal/shell/store/units/execution
	// planCleanup). A request-first execution names no workload, so the intent is
	// workspace-only. The row id is "cln-" plus the first 16 bytes of
	// sha256(executionID || "\x00cleanup").
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "cleanup", map[string]any{"execution_id": string(executionID)}, map[string]any{
		"id": "cln-0fc711f8490a1b9f6525f58a074624c3", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": string(executionID), "workload_id": pgtest.NullValue,
		"workspace_id": "workspace-request",
		"state":        "pending", "attempts": int64(0), "last_error": pgtest.NullValue, "next_retry_at": env.now,
	})
	// The owner's request.resolved event, then the projected execution.failed,
	// cleanup progress and action.transition events by their hash-derived ids.
	env.requireEvent(t, ctx, eventWant{
		id: resolveIDs.event, typ: "request.resolved", subject: string(executionID), version: 2,
		principal: "principal-1", data: pgtest.NullValue,
	})
	env.requireEvent(t, ctx, eventWant{
		id:  "event-7fad9a4bfd5c7a823b204d8e55285d3328cf3d4527760a5059bd937967d365cb",
		typ: "execution.failed", subject: string(executionID), version: 2, principal: lifecycleProducer,
		data: lifecycleFailureData(executionID, "running", "failed", 2, "runtime", "answer_rejected"),
	})
	env.requireEvent(t, ctx, eventWant{
		id:  "event-ad6217fd8dc617dfd403f63ec8f2506b43d1e16402dac8954b913663d0350f0f",
		typ: "execution.cleanup", subject: string(executionID), version: 2, principal: lifecycleProducer,
		data: lifecycleCleanupData(executionID, "pending"),
	})
	env.requireEvent(t, ctx, eventWant{
		id:  "event-907a594befb2d5db87d5ad1b423213781f840925a958d366402187c25563ac4b",
		typ: "action.transition", subject: string(actionID), version: 2, principal: lifecycleProducer,
		data: lifecycleActionData(actionID, 0, "request", "running", "failed", 2),
	})
	observer.Finish(map[string]int{"executions": 1, "actions": 1, "cleanup": 1, "receipts": 1, "audit": 1, "events": 4})
}

// TestObservedResolvePassNonLastOnPostgreSQL observes a passing resolution of
// the first of two request actions, so the execution stays running.
func TestObservedResolvePassNonLastOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newRequestRaceEnv(t)
	env.reset(t)
	yaml := []byte("actions:\n  - name: a\n    request: role/engineer\n  - name: b\n    request: role/engineer\n")
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: yaml})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"ref": {Kind: definition.SubjectLiteral, Literal: "refs/heads/main"},
	})
	actions := []pipeline.AdmittedAction{
		{Index: 0, Name: "a", Kind: definition.ActionKindRequest, Timeout: time.Hour,
			Request: &definition.Request{Assignment: "role/engineer", Subject: subject}},
		{Index: 1, Name: "b", Kind: definition.ActionKindRequest, Timeout: time.Hour,
			Request: &definition.Request{Assignment: "role/engineer", Subject: subject}},
	}
	store := executionstore.New(storepg.New(env.runtime), storepg.New(env.runtime))
	executionID := id.ExecutionID("execution-nonlast")
	if _, err := store.Start(ctx, execution.StartUnit{
		Owner: env.scope, Snapshot: snapshot, Actions: actions,
		ExecutionPrincipal: "execution-principal-1", NewExecutionID: executionID, At: pgtest.FixtureTime,
	}); err != nil {
		t.Fatalf("execution Start: %v%s", err, pgtest.CauseText(err))
	}
	if _, err := store.Transition(ctx, execution.TransitionUnit{
		Owner: env.scope, ExecutionID: executionID, ExpectedExecutionVersion: 1,
		Commit: flow.Commit{ExecutionID: executionID, ExpectedExecutionVersion: 1,
			NextExecutionState: kernel.ExecutionRunning, Output: flow.OutputChange{Action: flow.OutputNone}, At: pgtest.FixtureTime},
	}); err != nil {
		t.Fatalf("running transition: %v%s", err, pgtest.CauseText(err))
	}
	actionID := snapshotdoc.DeriveActionID(executionID, 0)
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := store.Activate(ctx, request.ActivationUnit{
		Owner: env.scope, ExecutionID: executionID, ActionID: actionID,
		ExpectedExecutionVersion: 2, ExpectedActionVersion: 1,
		Commit: flow.Commit{ExecutionID: executionID, ExpectedExecutionVersion: 2,
			NextExecutionState: kernel.ExecutionRunning, Output: flow.OutputChange{Action: flow.OutputNone}, At: pgtest.FixtureTime},
		Subject: subject, Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleEngineer},
		Timeout: time.Hour, Deadline: deadline, At: pgtest.FixtureTime,
	}); err != nil {
		t.Fatalf("activation: %v%s", err, pgtest.CauseText(err))
	}
	observer := env.shard.ObserveUnit(t, "request", "resolve.pass.nonlast")
	command := resolveCommand(env.actor, env.scope, executionID, actionID)
	command.ExpectedExecutionVersion = 3
	command.ExpectedActionVersion = 2
	resolveIDs := env.nextCommitIDs()
	if _, err := env.request.Resolve(ctx, command); err != nil {
		t.Fatalf("resolve non-last: %v%s", err, pgtest.CauseText(err))
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{Snapshot: snapshot, Actions: actions})
	if err != nil {
		t.Fatalf("Encode snapshot: %v", err)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": pgtest.NullValue, "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "running", "version": int64(4),
		"yaml_sha256": pgtest.SHA256(yaml), "snapshot": pgtest.GoldenJSONOf(rawJSON(document)),
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID(executionID)),
		"failure":      pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "a", "kind": "request", "state": "succeeded", "version": int64(3),
		"timeout_ms": int64(3600000), "deadline": deadline,
		"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject":    `{"ref":{"kind":"literal","literal":"refs/heads/main"}}`,
		"request_assignment": `{"kind":"role","role":"engineer"}`,
		"request_decision":   "pass", "request_actor": "principal-1", "request_resolved_at": env.now,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	// derived: the result carries the expected versions plus one: execution
	// 3->4 and action 2->3 (store/units/execution/request.go Resolve).
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": resolveIDs.receipt}, map[string]any{
		"id": resolveIDs.receipt, "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "execution.request.resolve",
		"caller_key":     string(executionID) + "/" + string(actionID),
		"content_sha256": pgtest.ContentSHA256(t, resolveReceiptContent(request.DecisionPass)),
		"result": pgtest.GoldenJSONOf(request.ResolveResult{
			ExecutionVersion: 4, ActionVersion: 3, Decision: request.DecisionPass,
		}),
		"completed_at": env.now, "execution_id": string(executionID),
		"snapshot_sha256": pgtest.NullValue, "created_at": env.now,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": resolveIDs.audit}, map[string]any{
		"id": resolveIDs.audit, "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "execution.request.resolve", "at": env.now, "detail": pgtest.NullValue,
	})
	// derived: the owner event's subject version is the action version plus one
	// (request service buildEvent); the only projected event is the action's
	// running->succeeded transition at the committed action version 3, because
	// the execution stays running.
	env.requireEvent(t, ctx, eventWant{
		id: resolveIDs.event, typ: "request.resolved", subject: string(executionID), version: 3,
		principal: "principal-1", data: pgtest.NullValue,
	})
	env.requireEvent(t, ctx, eventWant{
		id:  "event-854315a020e5ffc9b3e9f1559c0787266a62a6d7e9bbd9979b35200195b06f6e",
		typ: "action.transition", subject: string(actionID), version: 3, principal: lifecycleProducer,
		data: lifecycleActionData(actionID, 0, "request", "running", "succeeded", 3),
	})
	observer.Finish(map[string]int{"executions": 1, "actions": 1, "receipts": 1, "audit": 1, "events": 2})
}

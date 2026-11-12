//go:build component

package postgrescomponent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

func TestGoldenEventAndAuditRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)
	envelope, err := event.NewEnvelope("event-golden-1",
		event.Source{Kind: event.SourceShell, PrincipalID: "principal-1"}, scope, pgtest.FixtureTime,
		event.EnvelopeInput{Type: event.TypeExecutionQueued, SubjectID: "subject-1", SubjectVersion: 2,
			CorrelationID: "corr-1", CausationID: "cause-1", Data: []byte(`{"k":"v"}`)})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	audit := event.Audit{ID: "audit-golden-1", Scope: scope, Actor: "principal-1",
		Operation: "test.operation", At: pgtest.FixtureTime, Detail: "audit-detail"}
	if _, err := executor.Execute(ctx, scope, store.Unit{
		Audit: []event.Audit{audit}, Events: []event.Envelope{envelope},
	}); err != nil {
		t.Fatalf("event-bearing commit: %v%s", err, pgtest.CauseText(err))
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-golden-1"}, map[string]any{
		"id": "event-golden-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "execution.queued", "report_type": pgtest.NullValue,
		"subject_id": "subject-1", "subject_version": int64(2),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": "corr-1", "causation_id": "cause-1",
		"at": pgtest.FixtureTime, "data": []byte(`{"k":"v"}`),
		"publish_state": "pending", "publish_sequence": pgtest.GoldenDBSequence(),
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "audit-golden-1"}, map[string]any{
		"id": "audit-golden-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "test.operation",
		"at": pgtest.FixtureTime, "detail": "audit-detail",
	})
}

func TestGoldenReceiptRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)
	key := receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID,
		PrincipalID: "principal-1", Operation: "trigger.select", CallerKey: "caller-1"}
	content := `{"selection":"build"}`
	contentHash := pgtest.Fingerprint(content)
	digest := sha256.Sum256([]byte("snapshot"))
	snapshot := digest
	contentBytes := sha256.Sum256([]byte(content))

	incomplete := receipt.Record{ID: "receipt-golden-1", Key: key, ContentHash: contentHash,
		Result: []byte(`{"progress":true}`), ExecutionID: "execution-1", SnapshotHash: &snapshot, At: pgtest.FixtureTime}
	if _, err := executor.Execute(ctx, scope, store.Unit{Receipt: &incomplete, ReceiptMode: store.ReceiptInsertIncomplete}); err != nil {
		t.Fatalf("insert incomplete: %v%s", err, pgtest.CauseText(err))
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-golden-1"}, map[string]any{
		"id": "receipt-golden-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "trigger.select", "caller_key": "caller-1",
		"content_sha256": contentBytes[:], "result": []byte(`{"progress":true}`),
		"completed_at": pgtest.NullValue, "execution_id": "execution-1",
		"snapshot_sha256": digest[:], "created_at": pgtest.FixtureTime,
	})

	completedAt := pgtest.FixtureTime.Add(time.Minute)
	complete := receipt.Record{ID: "receipt-golden-1", Key: key, ContentHash: contentHash,
		Result: []byte(`{"done":true}`), ExecutionID: "execution-2", At: completedAt}
	if _, err := executor.Execute(ctx, scope, store.Unit{Receipt: &complete, ReceiptMode: store.ReceiptComplete,
		ReceiptExpect: &store.ReceiptExpect{ContentHash: contentHash}}); err != nil {
		t.Fatalf("complete: %v%s", err, pgtest.CauseText(err))
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-golden-1"}, map[string]any{
		"id": "receipt-golden-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "trigger.select", "caller_key": "caller-1",
		"content_sha256": contentBytes[:], "result": []byte(`{"done":true}`),
		"completed_at": completedAt, "execution_id": "execution-2",
		"snapshot_sha256": digest[:], "created_at": pgtest.FixtureTime,
	})

	inserted := receipt.Record{ID: "receipt-golden-2",
		Key: receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID,
			PrincipalID: "principal-1", Operation: "trigger.select", CallerKey: "caller-2"},
		ContentHash: pgtest.Fingerprint(`{"selection":"other"}`), Result: []byte(`{"done":false}`), At: pgtest.FixtureTime}
	if _, err := executor.Execute(ctx, scope, store.Unit{Receipt: &inserted}); err != nil {
		t.Fatalf("insert complete: %v%s", err, pgtest.CauseText(err))
	}
	otherContent := sha256.Sum256([]byte(`{"selection":"other"}`))
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-golden-2"}, map[string]any{
		"id": "receipt-golden-2", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "trigger.select", "caller_key": "caller-2",
		"content_sha256": otherContent[:], "result": []byte(`{"done":false}`),
		"completed_at": pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
}

func TestGoldenCleanupRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-cleanup", "running", 1, "", "")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	store := executionstore.New(storepg.New(runtime), storepg.New(runtime))

	if _, err := store.Transition(ctx, execution.TransitionUnit{
		Owner: scope, ExecutionID: "execution-cleanup", ExpectedExecutionVersion: 1,
		Commit: flow.Commit{ExecutionID: "execution-cleanup", ExpectedExecutionVersion: 1,
			NextExecutionState: kernel.ExecutionFailed,
			Output:             flow.OutputChange{Action: flow.OutputNone},
			Cleanup:            &flow.CleanupIntent{WorkloadID: "workload-1", WorkspaceID: "ws-1", Reason: "failed"},
			At:                 pgtest.FixtureTime},
	}); err != nil {
		t.Fatalf("cleanup intent: %v%s", err, pgtest.CauseText(err))
	}
	// derived: cleanupID is "cln-" plus the first 16 bytes of
	// sha256(execution id + "\x00cleanup") (internal/shell/store/units/execution/codec.go).
	shard.RequireGoldenRow(t, ctx, runtime, scope, "cleanup", map[string]any{"execution_id": "execution-cleanup"}, map[string]any{
		"id": "cln-f63d2c55f9c85b1f6c3a8ab73d7c6417", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": "execution-cleanup", "workload_id": "workload-1", "workspace_id": "ws-1",
		"state": "pending", "attempts": int64(0), "last_error": pgtest.NullValue,
		"next_retry_at": pgtest.FixtureTime,
	})
	retryAt := pgtest.FixtureTime.Add(time.Minute)
	if _, err := store.Transition(ctx, execution.TransitionUnit{
		Owner: scope, ExecutionID: "execution-cleanup", ExpectedExecutionVersion: 2,
		Commit: flow.Commit{ExecutionID: "execution-cleanup", ExpectedExecutionVersion: 2,
			NextExecutionState: kernel.ExecutionFailed,
			Output:             flow.OutputChange{Action: flow.OutputNone},
			CleanupUpdate:      &flow.CleanupUpdate{State: kernel.CleanupPending, WorkloadID: "workload-1", WorkspaceID: "ws-1", Attempts: 2, LastError: "boom"},
			At:                 retryAt},
	}); err != nil {
		t.Fatalf("cleanup retry: %v%s", err, pgtest.CauseText(err))
	}
	// The attempt count grew from 0 to 2, so the durable retry is delayed by the
	// capped exponential backoff of the new count: 1s * 2^(2-1) = 2s.
	shard.RequireGoldenRow(t, ctx, runtime, scope, "cleanup", map[string]any{"execution_id": "execution-cleanup"}, map[string]any{
		"id": "cln-f63d2c55f9c85b1f6c3a8ab73d7c6417", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": "execution-cleanup", "workload_id": "workload-1", "workspace_id": "ws-1",
		"state": "pending", "attempts": int64(2), "last_error": "boom",
		"next_retry_at": retryAt.Add(2 * time.Second),
	})
	if _, err := store.Transition(ctx, execution.TransitionUnit{
		Owner: scope, ExecutionID: "execution-cleanup", ExpectedExecutionVersion: 3,
		Commit: flow.Commit{ExecutionID: "execution-cleanup", ExpectedExecutionVersion: 3,
			NextExecutionState: kernel.ExecutionFailed,
			Output:             flow.OutputChange{Action: flow.OutputNone},
			CleanupUpdate:      &flow.CleanupUpdate{State: kernel.CleanupComplete, WorkloadID: "workload-1", WorkspaceID: "ws-1", Attempts: 3},
			At:                 retryAt.Add(time.Minute)},
	}); err != nil {
		t.Fatalf("cleanup complete: %v%s", err, pgtest.CauseText(err))
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "cleanup", map[string]any{"execution_id": "execution-cleanup"}, map[string]any{
		"id": "cln-f63d2c55f9c85b1f6c3a8ab73d7c6417", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": "execution-cleanup", "workload_id": "workload-1", "workspace_id": "ws-1",
		"state": "complete", "attempts": int64(3), "last_error": pgtest.NullValue,
		"next_retry_at": pgtest.NullValue,
	})
}

// TestGoldenPreparationFailureProofAndCleanupRowsOnPostgreSQL pins the
// persisted shape: a preparation failure that happened before Start carries the
// never-started proof on the action row and names the failed identity in the
// cleanup row, so a fresh controller can remove and release from the database
// alone. The execution is started through the real adapter Start path so the
// persisted snapshot document carries the shared schema version and admitted
// action definitions Load requires; only the engine's transition payload
// (flow.Commit) is hand-built to pin the adapter's row shape. The engine-side
// emission of that payload is pinned by internal/kernel/engine/enginetest.
func TestGoldenPreparationFailureProofAndCleanupRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	store := executionstore.New(storepg.New(runtime), storepg.New(runtime))

	// Start the execution through the real write path so the persisted
	// executions.snapshot document carries the shared schema version and the
	// admitted action definitions Load requires. A hand-seeded empty document
	// is version 0 and Load refuses it.
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML: []byte("actions:\n  - name: build\n    image: " + integrationImage + "\n"),
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	admitted := pipeline.AdmittedAction{
		Index: 0, Name: "build", Kind: definition.ActionKindContainer, Timeout: time.Minute,
		Container: &definition.Container{Image: integrationImage, Commands: []string{"echo", "ok"}},
	}
	started, err := store.Start(ctx, execution.StartUnit{
		Owner: scope, ExpectedPipelineVersion: 1, Snapshot: snapshot,
		Actions:   []pipeline.AdmittedAction{admitted},
		Initiator: "principal-1", ExecutionPrincipal: "execution-principal-1",
		NewExecutionID: "execution-prep", At: pgtest.FixtureTime,
	})
	if err != nil {
		t.Fatalf("execution Start: %v%s", err, pgtest.CauseText(err))
	}
	actionID := snapshotdoc.DeriveActionID(started.ExecutionID, 0)

	proof := kernel.NeverStarted("preparation failed before the start request was issued")
	launch := kernel.LaunchIntent{
		WorkloadID:     id.WorkloadID("workload-prep"),
		State:          kernel.LaunchIssued,
		IdempotencyKey: "launch-key",
	}
	if _, err := store.Transition(ctx, execution.TransitionUnit{
		Owner: scope, ExecutionID: "execution-prep", ExpectedExecutionVersion: 1,
		ExpectedActionVersion: 1,
		Commit: flow.Commit{
			ExecutionID:              "execution-prep",
			ExpectedExecutionVersion: 1,
			NextExecutionState:       kernel.ExecutionFailed,
			ActionID:                 actionID,
			ExpectedActionVersion:    1,
			NextActionState:          kernel.ActionFailed,
			Output:                   flow.OutputChange{Action: flow.OutputDiscard},
			Launch:                   &launch,
			Observation:              &proof,
			Failure:                  &kernel.Failure{Kind: kernel.FailurePreparation, Reason: "preparation_failed"},
			CleanupUpdate: &flow.CleanupUpdate{
				State:       kernel.CleanupPending,
				WorkloadID:  launch.WorkloadID,
				WorkspaceID: "workspace-execution-prep",
			},
			At: pgtest.FixtureTime,
		},
	}); err != nil {
		t.Fatalf("preparation-failure transition: %v%s", err, pgtest.CauseText(err))
	}

	// The product adapter round-trips the proof and the cleanup attribution a
	// fresh controller reads before it removes anything.
	loaded, err := store.Load(ctx, scope, "execution-prep")
	if err != nil {
		t.Fatalf("Load: %v%s", err, pgtest.CauseText(err))
	}
	if loaded.Failure == nil || loaded.Failure.Kind != kernel.FailurePreparation {
		t.Fatalf("loaded failure = %+v; want preparation", loaded.Failure)
	}
	if len(loaded.Actions) != 1 || loaded.Actions[0].Container == nil ||
		!loaded.Actions[0].Container.Observation.ProofOfNonexecution() {
		t.Fatalf("loaded action lost the never-started proof: %+v", loaded.Actions)
	}
	wantCleanup := kernel.Cleanup{
		State:       kernel.CleanupPending,
		WorkloadID:  launch.WorkloadID,
		WorkspaceID: "workspace-execution-prep",
	}
	if loaded.Cleanup != wantCleanup {
		t.Fatalf("loaded cleanup = %+v; want %+v", loaded.Cleanup, wantCleanup)
	}

	// derived: cleanupID is "cln-" plus the first 16 bytes of
	// sha256(execution id + "\x00cleanup") (internal/shell/store/units/execution/codec.go).
	shard.RequireGoldenRow(t, ctx, runtime, scope, "cleanup", map[string]any{"execution_id": "execution-prep"}, map[string]any{
		"id": "cln-14e31fd640a551ac0ac3d274416f9b2f", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": "execution-prep", "workload_id": "workload-prep", "workspace_id": "workspace-execution-prep",
		"state": "pending", "attempts": int64(0), "last_error": pgtest.NullValue,
		"next_retry_at": pgtest.FixtureTime,
	})
}

func TestOrderedMembershipAndBindingPagesOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	for _, suffix := range []string{"a", "b", "c"} {
		shard.SeedResource(ctx, scope, "resource-"+suffix)
		shard.SeedService(ctx, scope, "service-"+suffix, "service-"+suffix)
		shard.SeedMembership(ctx, scope, "membership-"+suffix, "principal-"+suffix, 1, "engineer")
		shard.SeedBinding(ctx, scope, "binding-"+suffix, "resource-"+suffix, "service-"+suffix)
	}
	runtime := shard.Runtime(4)
	defer runtime.Close()
	adapter, err := identity.New(identity.Config{Executor: storepg.New(runtime), Scanner: storepg.New(runtime),
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{}})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	membershipIDs := pagedMembershipIDs(t, ctx, adapter, scope)
	if fmt.Sprint(membershipIDs) != "[principal-a principal-b principal-c]" {
		t.Fatalf("paged membership principals = %v; want a,b,c ascending", membershipIDs)
	}
	bindingIDs := pagedBindingIDs(t, ctx, adapter, scope)
	if fmt.Sprint(bindingIDs) != "[binding-a binding-b binding-c]" {
		t.Fatalf("paged binding ids = %v; want a,b,c ascending", bindingIDs)
	}
}

func pagedMembershipIDs(t *testing.T, ctx context.Context, adapter *identity.Adapter, scope tenancy.Scope) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for range 5 {
		result, err := adapter.ListMemberships(ctx, tenancyadmin.MembershipQuery{Scope: scope, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListMemberships: %v", err)
		}
		if len(result.Memberships) > 2 {
			t.Fatalf("membership page has %d rows for limit 2", len(result.Memberships))
		}
		for _, membership := range result.Memberships {
			ids = append(ids, string(membership.PrincipalID))
		}
		if result.NextCursor == "" {
			return ids
		}
		cursor = result.NextCursor
	}
	t.Fatalf("membership pagination did not terminate")
	return nil
}

func pagedBindingIDs(t *testing.T, ctx context.Context, adapter *identity.Adapter, scope tenancy.Scope) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for range 5 {
		result, err := adapter.QueryBindings(ctx, resource.BindingQuery{Scope: scope, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("QueryBindings: %v", err)
		}
		if len(result.Bindings) > 2 {
			t.Fatalf("binding page has %d rows for limit 2", len(result.Bindings))
		}
		for _, binding := range result.Bindings {
			ids = append(ids, string(binding.ID))
		}
		if result.NextCursor == "" {
			return ids
		}
		cursor = result.NextCursor
	}
	t.Fatalf("binding pagination did not terminate")
	return nil
}

func TestOrderedTaskActionsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	store := executionstore.New(storepg.New(runtime), storepg.New(runtime))
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("actions:\n  - name: a\n    image: " + integrationImage + "\n  - name: b\n    image: " + integrationImage + "\n  - name: c\n    image: " + integrationImage + "\n")})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	container := definition.Container{Image: integrationImage, Commands: []string{"echo", "ok"}}
	var actions []pipeline.AdmittedAction
	for index, name := range []string{"a", "b", "c"} {
		actions = append(actions, pipeline.AdmittedAction{Index: index, Name: id.ActionName(name),
			Kind: definition.ActionKindContainer, Timeout: time.Minute, Container: &container})
	}
	if _, err := store.Start(ctx, execution.StartUnit{
		Owner: scope, ExpectedPipelineVersion: 1, Snapshot: snapshot, Actions: actions,
		Initiator: "principal-1", ExecutionPrincipal: "execution-principal-1",
		NewExecutionID: "execution-actions", At: pgtest.FixtureTime,
	}); err != nil {
		t.Fatalf("execution Start: %v%s", err, pgtest.CauseText(err))
	}
	execution, err := store.Load(ctx, scope, id.ExecutionID("execution-actions"))
	if err != nil {
		t.Fatalf("Load: %v%s", err, pgtest.CauseText(err))
	}
	var names []string
	for _, action := range execution.Actions {
		names = append(names, string(action.Name))
	}
	if fmt.Sprint(names) != "[a b c]" {
		t.Fatalf("task action order = %v; want index order a,b,c", names)
	}
}

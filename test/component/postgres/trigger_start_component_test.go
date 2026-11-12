//go:build component

package postgrescomponent

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestTriggerStartedWholeRowsOnPostgreSQL drives the real trigger start unit
// (select then start) and whole-row asserts every row it writes, including the
// replay that must add nothing.
func TestTriggerStartedWholeRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard, scoped := adapterScoped(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedPipeline(ctx, scope, "pipeline-1", "build", []byte("actions: []\n"), 1, "[]")
	shard.SeedEvent(ctx, scope, "event-1", "execution.queued", "subject-1", 1)
	adapter, err := work.NewTriggers(scoped, scoped, adapterController(t, shard), func() time.Time { return pgtest.FixtureTime }, nil)
	if err != nil {
		t.Fatalf("NewTriggers: %v", err)
	}
	selection := trigger.Selection{PipelineID: "pipeline-1", EventID: "event-1", ServiceID: "service-1",
		Inputs: map[id.InputKey]string{"ref": "refs/heads/main"}}
	record := receipt.Record{ID: "receipt-trigger-1",
		Key: receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID,
			PrincipalID: "principal-1", Operation: "pipeline.start", CallerKey: "pipeline-1/event-1"},
		ContentHash: pgtest.Fingerprint("selection"), At: pgtest.FixtureTime}
	if _, err := adapter.Select(ctx, trigger.SelectUnit{Owner: scope, PipelineID: "pipeline-1", EventID: "event-1",
		ExpectedPipelineVersion: 1, Selection: selection, Receipt: record}); err != nil {
		t.Fatalf("Select: %v%s", err, pgtest.CauseText(err))
	}
	admittedYAML := []byte("actions:\n  - name: run\n    image: " + integrationImage + "\n")
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: admittedYAML})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	container := definition.Container{Image: integrationImage, Commands: []string{"echo", "ok"}}
	action := pipeline.AdmittedAction{Index: 0, Name: "run", Kind: definition.ActionKindContainer, Timeout: time.Minute, Container: &container}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{PipelineID: "pipeline-1", PipelineVersion: 1, Snapshot: snapshot, Actions: []pipeline.AdmittedAction{action}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	queued, err := event.NewEnvelope("event-queued-1",
		event.Source{Kind: event.SourceShell, PrincipalID: "principal-1"}, scope, pgtest.FixtureTime,
		event.EnvelopeInput{Type: event.TypeExecutionQueued, SubjectID: "execution-trigger-1", SubjectVersion: 1})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	unit := trigger.StartUnit{Owner: scope, PipelineID: "pipeline-1", EventID: "event-1",
		Selection: selection, Admission: pipeline.AdmissionResult{PipelineID: "pipeline-1", PipelineVersion: 1,
			Snapshot: snapshot, Actions: []pipeline.AdmittedAction{action}},
		ExpectedPipelineVersion: 1, Initiator: "principal-1", ExecutionPrincipal: "execution-principal-1",
		NewExecutionID: "execution-trigger-1", Status: trigger.StatusSelected, At: pgtest.FixtureTime,
		Receipt: record,
		Audit:   []event.Audit{{ID: "audit-start-1", Scope: scope, Actor: "principal-1", Operation: "pipeline.start", At: pgtest.FixtureTime}},
		Events:  []event.Envelope{queued}}
	observer := shard.ObserveUnit(t, "trigger", "started")
	started, err := adapter.Start(ctx, unit)
	if err != nil {
		t.Fatalf("Start: %v%s", err, pgtest.CauseText(err))
	}
	if started.Replayed || started.ExecutionID != "execution-trigger-1" {
		t.Fatalf("started = %+v; want a fresh execution-trigger-1", started)
	}
	actionID := snapshotdoc.DeriveActionID("execution-trigger-1", 0)
	assertTriggerStartedRows(t, ctx, shard, scoped, scope, document, admittedYAML, started, actionID)
	observer.Finish(map[string]int{"executions": 1, "actions": 1, "receipts": 1, "audit": 1, "events": 1})

	// The replay completes nothing new and returns the stored result.
	replayObserver := shard.ObserveUnit(t, "trigger", "started.replay")
	replayed, err := adapter.Start(ctx, unit)
	if err != nil {
		t.Fatalf("Start replay: %v%s", err, pgtest.CauseText(err))
	}
	if !replayed.Replayed || replayed.ExecutionID != "execution-trigger-1" {
		t.Fatalf("replay = %+v; want Replayed execution-trigger-1", replayed)
	}
	requireReplayOf(t, "trigger start replay", started, replayed)
	for table, want := range map[string]int64{"executions": 1, "actions": 1, "audit": 1, "events": 2} {
		count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM `+table)
		if err != nil || count != want {
			t.Fatalf("%s rows after trigger start replay = %d, err=%v; want %d", table, count, err, want)
		}
	}
	assertTriggerStartedRows(t, ctx, shard, scoped, scope, document, admittedYAML, started, actionID)
	replayObserver.Finish(map[string]int{})
}

// assertTriggerStartedRows whole-row asserts everything the trigger start
// commits. started is the first (non-replay) result: the receipt stores the
// common selection envelope, which is that result beside the selection's exact
// YAML bytes; the trigger test's selection carries none, so the YAML is null.
func assertTriggerStartedRows(t *testing.T, ctx context.Context, shard *pgtest.Shard, scoped *storepg.Store, scope tenancy.Scope, document, admittedYAML []byte, started trigger.Result, actionID id.ActionID) {
	t.Helper()
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "executions", map[string]any{"id": "execution-trigger-1"}, map[string]any{
		"id": "execution-trigger-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": "pipeline-1", "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "queued", "version": int64(1),
		"yaml_sha256": pgtest.SHA256(admittedYAML), "snapshot": string(document),
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID("execution-trigger-1")),
		"failure":      pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": "execution-trigger-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "run", "kind": "container", "state": "pending", "version": int64(1),
		"timeout_ms": int64(60000), "deadline": pgtest.NullValue, "outputs": pgtest.NullValue,
		"staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject": pgtest.NullValue, "request_assignment": pgtest.NullValue,
		"request_decision": pgtest.NullValue, "request_actor": pgtest.NullValue, "request_resolved_at": pgtest.NullValue,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "receipts", map[string]any{"id": "receipt-trigger-1"}, map[string]any{
		"id": "receipt-trigger-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "pipeline.start", "caller_key": "pipeline-1/event-1",
		"content_sha256": pgtest.FingerprintBytes("selection"),
		"result":         pgtest.GoldenJSONOf(map[string]any{"result": started, "yaml": nil}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "audit", map[string]any{"id": "audit-start-1"}, map[string]any{
		"id": "audit-start-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "pipeline.start", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope, "events", map[string]any{"id": "event-queued-1"}, map[string]any{
		"id": "event-queued-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "execution.queued", "report_type": pgtest.NullValue,
		"subject_id": "execution-trigger-1", "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
}

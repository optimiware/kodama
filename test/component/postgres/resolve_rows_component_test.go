//go:build component

package postgrescomponent

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestRequestResolveWholeRowsOnPostgreSQL proves a pass resolution writes every
// row of its unit with exact durable values.
func TestRequestResolveWholeRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newRequestRaceEnv(t)
	env.reset(t)
	executionID, actionID := seedRunningRequest(ctx, t, env.shard, env.scope)
	seeded := readSeededTimes(t, ctx, env.shard, env.scope, executionID)
	resolveObserver := env.shard.ObserveUnit(t, "request", "resolve")
	resolveIDs := env.nextCommitIDs()
	if _, err := env.request.Resolve(ctx, resolveCommand(env.actor, env.scope, executionID, actionID)); err != nil {
		t.Fatalf("resolve: %v%s", err, pgtest.CauseText(err))
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "executions", map[string]any{"id": string(executionID)}, map[string]any{
		"id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"pipeline_id": pgtest.NullValue, "task_id": pgtest.NullValue,
		"principal_id": "execution-principal-1", "state": "succeeded", "version": int64(2),
		"yaml_sha256": seededRequestYAMLSHA(), "snapshot": pgtest.GoldenJSONOf(rawJSON(runningRequestDocument(t))),
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"workspace_id": "workspace-request", "failure": pgtest.NullValue, "created_at": seeded.created,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "actions", map[string]any{"id": string(actionID)}, map[string]any{
		"id": string(actionID), "execution_id": string(executionID), "tenant_id": "tenant-1", "project_id": "project-1",
		"index": int64(0), "name": "ask", "kind": "request", "state": "succeeded", "version": int64(2),
		"timeout_ms": int64(3600000), "deadline": seeded.deadline,
		"outputs": pgtest.NullValue, "staged_outputs": pgtest.NullValue, "staged_outputs_set": false, "stop": pgtest.NullValue,
		"request_subject":    `{"ref":{"kind":"literal","literal":"refs/heads/main"}}`,
		"request_assignment": `{"kind":"role","role":"engineer"}`,
		"request_decision":   "pass", "request_actor": "principal-1", "request_resolved_at": env.now,
		"workload_id": pgtest.NullValue, "launch_state": "not_issued", "launch_key": pgtest.NullValue,
		"observation": pgtest.NullValue, "activated_at": pgtest.NullValue, "answer_provenance": pgtest.NullValue,
	})
	// derived: the unit result is {expected+1 versions, decision}; the content is
	// the request service's {decision,response} retry identity.
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{
		"operation": "execution.request.resolve", "caller_key": string(executionID) + "/" + string(actionID),
	}, map[string]any{
		"id": resolveIDs.receipt, "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "execution.request.resolve",
		"caller_key":     string(executionID) + "/" + string(actionID),
		"content_sha256": pgtest.ContentSHA256(t, resolveReceiptContent(request.DecisionPass)),
		"result": pgtest.GoldenJSONOf(request.ResolveResult{
			ExecutionVersion: 2, ActionVersion: 2, Decision: request.DecisionPass,
		}),
		"completed_at": env.now, "execution_id": string(executionID),
		"snapshot_sha256": pgtest.NullValue, "created_at": env.now,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"operation": "execution.request.resolve"}, map[string]any{
		"id": resolveIDs.audit, "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "execution.request.resolve", "at": env.now, "detail": pgtest.NullValue,
	})
	// derived: the last passing answer's terminal commit records pending cleanup
	// at the commit time (kernel flow reduceAnswer). A request-first execution
	// names no workload, so the intent is workspace-only. The row id is "cln-"
	// plus the first 16 bytes of sha256(executionID || "\x00cleanup").
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "cleanup", map[string]any{"execution_id": string(executionID)}, map[string]any{
		"id": "cln-0fc711f8490a1b9f6525f58a074624c3", "tenant_id": "tenant-1", "project_id": "project-1",
		"execution_id": string(executionID), "workload_id": pgtest.NullValue,
		"workspace_id": "workspace-request",
		"state":        "pending", "attempts": int64(0), "last_error": pgtest.NullValue, "next_retry_at": env.now,
	})
	// The owner's request.resolved event, then the projected execution.succeeded,
	// cleanup progress and action.transition events by their hash-derived ids
	// (event.lifecycleEventID over execution id, committed version, type and subject).
	env.requireEvent(t, ctx, eventWant{
		id: resolveIDs.event, typ: "request.resolved", subject: string(executionID), version: 2,
		principal: "principal-1", data: pgtest.NullValue,
	})
	env.requireEvent(t, ctx, eventWant{
		id:  "event-0a51a9a42ab489c47eddeba3e436661bef63f4ea7a19d32ba52da95ee54a2ed0",
		typ: "execution.succeeded", subject: string(executionID), version: 2, principal: lifecycleProducer,
		data: lifecycleExecutionData(executionID, "running", "succeeded", 2),
	})
	env.requireEvent(t, ctx, eventWant{
		id:  "event-ad6217fd8dc617dfd403f63ec8f2506b43d1e16402dac8954b913663d0350f0f",
		typ: "execution.cleanup", subject: string(executionID), version: 2, principal: lifecycleProducer,
		data: lifecycleCleanupData(executionID, "pending"),
	})
	env.requireEvent(t, ctx, eventWant{
		id:  "event-907a594befb2d5db87d5ad1b423213781f840925a958d366402187c25563ac4b",
		typ: "action.transition", subject: string(actionID), version: 2, principal: lifecycleProducer,
		data: lifecycleActionData(actionID, 0, "request", "running", "succeeded", 2),
	})
	resolveObserver.Finish(map[string]int{"executions": 1, "actions": 1, "cleanup": 1, "receipts": 1, "audit": 1, "events": 4})
}

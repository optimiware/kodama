//go:build component

package postgrescomponent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/gateway"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestGatewayUsageAndContentRowsOnPostgreSQL drives the gateway's real usage
// and content commit units and whole-row asserts their durable rows.
func TestGatewayUsageAndContentRowsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)
	shard.SeedExecution(ctx, scope, "execution-usage", "running", 1, "", "")

	usage, err := gateway.UsageRecord{
		ID: "usage-1", Scope: scope, PrincipalID: "principal-1", ExecutionID: "execution-usage",
		Model: "model-a", UpstreamModel: "upstream-a", Outcome: gateway.UsageSucceeded,
		UsageKnown: true, At: pgtest.FixtureTime,
	}.Unit()
	if err != nil {
		t.Fatalf("usage unit: %v", err)
	}
	usageObserver := shard.ObserveUnit(t, "gateway", "usage.record")
	if _, err := executor.Execute(ctx, scope, usage); err != nil {
		t.Fatalf("usage execute: %v%s", err, pgtest.CauseText(err))
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "model_usage", map[string]any{"id": "usage-1"}, map[string]any{
		"id": "usage-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "execution_id": "execution-usage",
		"model": "model-a", "upstream_model": "upstream-a", "outcome": "succeeded",
		"usage_known": true, "prompt_tokens": int64(0), "completion_tokens": int64(0),
		"total_tokens": int64(0), "recorded_at": pgtest.FixtureTime, "version": int64(1),
	})
	usageObserver.Finish(map[string]int{"model_usage": 1})

	recorder := gateway.StoreContentRecorder{Executor: executor,
		NewID: func() (string, error) { return "content-1", nil }}
	contentObserver := shard.ObserveUnit(t, "gateway", "content.record")
	if err := recorder.Record(ctx, gateway.ContentRecord{
		Scope: scope, PrincipalID: "principal-1", Model: "model-a",
		Request: []byte(`{"q":1}`), Response: []byte(`{"a":2}`),
		At: pgtest.FixtureTime, RetainUntil: pgtest.FixtureTime.Add(7 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("content record: %v%s", err, pgtest.CauseText(err))
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "content_records", map[string]any{"id": "content-1"}, map[string]any{
		"id": "content-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "execution_id": pgtest.NullValue,
		"model": "model-a", "request_body": []byte(`{"q":1}`), "response_body": []byte(`{"a":2}`),
		"recorded_at": pgtest.FixtureTime, "retain_until": pgtest.FixtureTime.Add(7 * 24 * time.Hour),
		"version": int64(1),
	})
	contentObserver.Finish(map[string]int{"content_records": 1})
}

// TestGatewayUsageRowWithoutExecutionStoresNullOnPostgreSQL commits the
// usage unit of a record that names no execution and whole-row asserts the
// durable row: execution_id is SQL NULL, so the nullable model_usage_execution
// foreign key is not evaluated. The same unit carrying the empty text instead
// is refused by that key and leaves no row. It observes the same unit×state as
// the execution-attributed row above: one model_usage insert.
func TestGatewayUsageRowWithoutExecutionStoresNullOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)

	record := gateway.UsageRecord{
		ID: "usage-no-execution", Scope: scope, PrincipalID: "principal-1", ExecutionID: "",
		Model: "model-a", UpstreamModel: "upstream-a", Outcome: gateway.UsageFailed,
		At: pgtest.FixtureTime,
	}
	usage, err := record.Unit()
	if err != nil {
		t.Fatalf("usage unit: %v", err)
	}
	usageObserver := shard.ObserveUnit(t, "gateway", "usage.record")
	if _, err := executor.Execute(ctx, scope, usage); err != nil {
		t.Fatalf("usage execute: %v%s", err, pgtest.CauseText(err))
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "model_usage", map[string]any{"id": "usage-no-execution"}, map[string]any{
		"id": "usage-no-execution", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "execution_id": pgtest.NullValue,
		"model": "model-a", "upstream_model": "upstream-a", "outcome": "failed",
		"usage_known": false, "prompt_tokens": int64(0), "completion_tokens": int64(0),
		"total_tokens": int64(0), "recorded_at": pgtest.FixtureTime, "version": int64(1),
	})
	usageObserver.Finish(map[string]int{"model_usage": 1})

	record.ID = "usage-empty-text"
	emptyText, err := record.Unit()
	if err != nil {
		t.Fatalf("usage unit: %v", err)
	}
	emptyText.Writes[0].Values["execution_id"] = ""
	_, err = executor.Execute(ctx, scope, emptyText)
	if err == nil {
		t.Fatal("a usage row carrying an empty-text execution_id committed; want the model_usage_execution foreign key to refuse it")
	}
	if cause := pgtest.CauseText(err); !strings.Contains(cause, "model_usage_execution") {
		t.Fatalf("empty-text execution_id refusal = %s; want the model_usage_execution foreign key", cause)
	}
	shard.RequireRowAbsent(t, ctx, runtime, scope, "model_usage", map[string]any{"id": "usage-empty-text"})
}

//go:build integration

package factorytest

// Regression: a pipeline with task and execution history is deletable.
//
// The schema carries no foreign keys from executions or tasks to pipelines, so
// pipeline.delete is not refused once an execution or task names the pipeline,
// and executions.pipeline_id and tasks.pipeline_id stay as durable evidence.
// This test proves the public deletion succeeds, the executions and tasks stay
// readable and still name the deleted pipeline, and every new admission from
// it is refused not_found without writing an execution or a task.

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

func TestFactoryDeletesPipelineWithHistory(t *testing.T) {
	harness := factory.Start(t)
	ctx := context.Background()
	token := harness.ProjectToken

	applied := decodeResult[api.PipelineApplyResult](harness, harness.MustREST(token, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      "pipeline-history",
		YAML: marshalPipeline(t, pipelineDoc{Actions: []pipelineActionDoc{{
			Name:     "work",
			Image:    harness.Image.Reference,
			Timeout:  approvalContainerTimeout,
			Commands: []string{`echo "pipeline history"`},
		}}}),
	}, nil, "history-apply"))

	// Three kinds of history name the pipeline: a task-free execution, a task
	// with an execution, and a task that only selects it.
	direct := decodeResult[api.PipelineStartResult](harness, harness.MustREST(token, "pipeline.start", api.PipelineStartRequest{
		ProjectID: factory.ProjectID, PipelineID: applied.PipelineID,
	}, &applied.Version, "history-start"))
	ran := decodeResult[api.TaskCreateAndStartResult](harness, harness.MustREST(token, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID: factory.ProjectID, Title: "ran the pipeline", PipelineID: applied.PipelineID,
	}, nil, "history-create-and-start"))
	selected := decodeResult[api.TaskCreateResult](harness, harness.MustREST(token, "task.create", api.TaskCreateRequest{
		ProjectID: factory.ProjectID, Title: "selected the pipeline", PipelineID: applied.PipelineID,
	}, nil, "history-create"))
	executionIDs := []string{direct.ExecutionID, ran.ExecutionID}
	settled := make(map[string]api.ExecutionInspectResult, len(executionIDs))
	for _, executionID := range executionIDs {
		settled[executionID] = waitPipelineHistoryExecution(t, harness, token, executionID)
	}

	counts := pipelineHistoryCounts(t, ctx, harness)
	deleted := decodeResult[api.PipelineDeleteResult](harness, harness.MustREST(token, "pipeline.delete", api.PipelineDeleteRequest{
		ProjectID: factory.ProjectID, PipelineID: applied.PipelineID,
	}, &applied.Version, "history-delete"))
	if deleted.Version != applied.Version+1 {
		t.Fatalf("deletion version = %d; want %d", deleted.Version, applied.Version+1)
	}

	// The public read is a scoped miss, not a stale success, and the row is gone.
	_, publicError := harness.REST(token, "pipeline.read", api.PipelineReadRequest{
		ProjectID: factory.ProjectID, PipelineID: applied.PipelineID,
	}, nil, "")
	if publicError.Code != api.CodeNotFound {
		t.Fatalf("pipeline.read after deletion = %q (%s); want %q", publicError.Code, publicError.Message, api.CodeNotFound)
	}
	pgtest.RequireRowCount(t, ctx, harness.Postgres.Admin(),
		`SELECT count(*)::text FROM pipelines WHERE id = $1`, 0, "pipeline rows after deletion", applied.PipelineID)

	// Every history row survives and keeps the deleted pipeline's identity.
	pgtest.RequireRowCount(t, ctx, harness.Postgres.Admin(),
		`SELECT count(*)::text FROM executions WHERE pipeline_id = $1 AND id = ANY($2)`, 2,
		"executions still naming the deleted pipeline", applied.PipelineID, executionIDs)
	pgtest.RequireRowCount(t, ctx, harness.Postgres.Admin(),
		`SELECT count(*)::text FROM tasks WHERE pipeline_id = $1 AND id = ANY($2)`, 2,
		"tasks still naming the deleted pipeline", applied.PipelineID, []string{ran.TaskID, selected.TaskID})

	// The executions are still inspectable and listable by the deleted pipeline.
	for _, executionID := range executionIDs {
		inspected := decodeResult[api.ExecutionInspectResult](harness, harness.MustREST(token, "execution.inspect",
			api.ExecutionInspectRequest{ExecutionID: executionID}, nil, ""))
		before := settled[executionID]
		if inspected.PipelineID != applied.PipelineID || inspected.State != before.State || inspected.Version != before.Version {
			t.Fatalf("execution %s after deletion = pipeline %q state %q version %d; want pipeline %q state %q version %d",
				executionID, inspected.PipelineID, inspected.State, inspected.Version, applied.PipelineID, before.State, before.Version)
		}
	}
	listed := decodeResult[api.ExecutionListResult](harness, harness.MustREST(token, "execution.list", api.ExecutionListRequest{
		ProjectID: factory.ProjectID, PipelineID: applied.PipelineID,
	}, nil, ""))
	seen := make(map[string]bool, len(listed.Executions))
	for _, execution := range listed.Executions {
		if execution.PipelineID != applied.PipelineID {
			t.Fatalf("execution.list by the deleted pipeline returned %+v", execution)
		}
		seen[execution.ExecutionID] = true
	}
	if len(listed.Executions) != 2 || !seen[direct.ExecutionID] || !seen[ran.ExecutionID] {
		t.Fatalf("execution.list by the deleted pipeline = %+v; want exactly %v", listed.Executions, executionIDs)
	}

	// The tasks are still readable with their selection and history.
	for _, taskID := range []string{ran.TaskID, selected.TaskID} {
		read := decodeResult[api.TaskReadResult](harness, harness.MustREST(token, "task.read", api.TaskReadRequest{TaskID: taskID}, nil, ""))
		if read.SelectedPipeline != applied.PipelineID {
			t.Fatalf("task %s selected pipeline after deletion = %q; want %q", taskID, read.SelectedPipeline, applied.PipelineID)
		}
	}
	history := decodeResult[api.TaskHistoryResult](harness, harness.MustREST(token, "task.history", api.TaskHistoryRequest{TaskID: ran.TaskID}, nil, ""))
	if len(history.Entries) == 0 {
		t.Fatalf("task.history of %s after deletion is empty", ran.TaskID)
	}

	// Nothing new is admitted from the deleted pipeline, and a refusal writes
	// no execution and no task.
	open := decodeResult[api.TaskReadResult](harness, harness.MustREST(token, "task.read", api.TaskReadRequest{TaskID: selected.TaskID}, nil, ""))
	refusals := []struct {
		operation string
		input     any
		expected  *uint64
	}{
		{"task.start", api.TaskStartRequest{TaskID: selected.TaskID}, &open.LifecycleVersion},
		{"pipeline.start", api.PipelineStartRequest{ProjectID: factory.ProjectID, PipelineID: applied.PipelineID}, &applied.Version},
		{"task.create_and_start", api.TaskCreateAndStartRequest{
			ProjectID: factory.ProjectID, Title: "again", PipelineID: applied.PipelineID,
		}, nil},
		{"task.create", api.TaskCreateRequest{
			ProjectID: factory.ProjectID, Title: "select again", PipelineID: applied.PipelineID,
		}, nil},
	}
	for _, refusal := range refusals {
		_, publicError := harness.REST(token, refusal.operation, refusal.input, refusal.expected, "history-refused-"+refusal.operation)
		if publicError.Code != api.CodeNotFound {
			t.Fatalf("%s against the deleted pipeline = %q (%s); want %q",
				refusal.operation, publicError.Code, publicError.Message, api.CodeNotFound)
		}
		if after := pipelineHistoryCounts(t, ctx, harness); after != counts {
			t.Fatalf("refused %s changed rows: before %+v, after %+v", refusal.operation, counts, after)
		}
	}
	after := decodeResult[api.TaskReadResult](harness, harness.MustREST(token, "task.read", api.TaskReadRequest{TaskID: selected.TaskID}, nil, ""))
	if after.LifecycleVersion != open.LifecycleVersion || after.CurrentExecution != "" {
		t.Fatalf("task %s after the refused start = lifecycle %d current %q; want it untouched at %d",
			selected.TaskID, after.LifecycleVersion, after.CurrentExecution, open.LifecycleVersion)
	}
}

// decodeResult decodes one served result into its typed value.
func decodeResult[T any](harness *factory.Harness, result api.Result) T {
	var decoded T
	harness.Decode(result, &decoded)
	return decoded
}

// pipelineHistoryRows counts the rows an admission would add.
type pipelineHistoryRows struct {
	executions int64
	actions    int64
	tasks      int64
}

func pipelineHistoryCounts(t *testing.T, ctx context.Context, harness *factory.Harness) pipelineHistoryRows {
	t.Helper()
	count := func(table string) int64 {
		value, err := pgtest.ScalarInt(ctx, harness.Postgres.Admin(), `SELECT count(*)::text FROM `+table)
		if err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return value
	}
	return pipelineHistoryRows{executions: count("executions"), actions: count("actions"), tasks: count("tasks")}
}

// waitPipelineHistoryExecution waits for one execution to succeed with its
// cleanup complete and a stable version, so the row no longer changes.
func waitPipelineHistoryExecution(t *testing.T, harness *factory.Harness, token, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	stable := false
	harness.Eventually(t, 3*time.Minute, func() bool {
		result, publicError := harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID}, nil, "")
		if publicError.Code != "" {
			stable = false
			return false
		}
		var current api.ExecutionInspectResult
		harness.Decode(result, &current)
		switch current.State {
		case "failed", "cancelled":
			t.Fatalf("execution %s settled %q (%+v); want succeeded", executionID, current.State, current.Failure)
		}
		settled := current.State == "succeeded" && current.Cleanup.State == "complete"
		done := settled && stable && current.Version == last.Version
		stable = settled
		last = current
		return done
	})
	return last
}

package executionstore

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// inlineSnapshot builds the exact task-free admitted snapshot of one inline
// start. The store never re-derives admission from YAML.
func inlineSnapshot(t *testing.T, yaml []byte, inputs map[id.InputKey]string) pipeline.Snapshot {
	t.Helper()
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: yaml, Inputs: inputs})
	if err != nil {
		t.Fatalf("pipeline.NewSnapshot: %v", err)
	}
	return snapshot
}

// inlineStartUnit is one hand-built pipeline-less start commit unit. It carries
// an empty pipeline identity, version zero and the immutable snapshot.
func inlineStartUnit(scope tenancy.Scope, executionID id.ExecutionID, key string, snapshot pipeline.Snapshot, actions ...pipeline.AdmittedAction) execution.StartUnit {
	return execution.StartUnit{
		Owner:                   scope,
		PipelineID:              "",
		ExpectedPipelineVersion: 0,
		Snapshot:                snapshot,
		Actions:                 actions,
		Initiator:               "user-1",
		ExecutionPrincipal:      "exec-principal-1",
		NewExecutionID:          executionID,
		At:                      testNow,
		Receipt:                 testReceipt(scope, key, "a"),
		Audit:                   []event.Audit{testAudit(scope, key)},
		Events:                  []event.Envelope{testEvent(scope, key, string(executionID))},
	}
}

// inlineReceiptFor builds the exact receipt of one inline start whose
// idempotency content is the admitted YAML digest plus inputs.
func inlineReceiptFor(t *testing.T, scope tenancy.Scope, key, content string) receipt.Record {
	t.Helper()
	hash, err := receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(content)})
	if err != nil {
		t.Fatalf("receipt.Fingerprint: %v", err)
	}
	record := testReceipt(scope, key, "a")
	record.ContentHash = hash
	return record
}

// countTableRows counts the committed rows of one table in one scope.
func countTableRows(t *testing.T, backend *memory.Store, scope tenancy.Scope, table string) int {
	t.Helper()
	page, err := backend.Query(context.Background(), scope, store.ScanQuery{Table: table, Limit: store.MaxQueryLimit})
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	return len(page.Rows)
}

// TestInlineStartPersistsPipelineLessSnapshot pins the pipeline-less durable
// shape: no pipeline row, NULL pipeline/task links, exact admitted YAML, hash
// and actions, and a snapshot document without a pipeline binding.
func TestInlineStartPersistsPipelineLessSnapshot(t *testing.T) {
	adapter, backend := newStore(t)
	scope := projectScope()
	yaml := []byte("name: inline\non: push\n")
	snapshot := inlineSnapshot(t, yaml, map[id.InputKey]string{"target": "linux"})
	unit := inlineStartUnit(scope, "exec-inline", "inline-start", snapshot, containerAction())

	result, err := adapter.Start(context.Background(), unit)
	if err != nil {
		t.Fatalf("inline Start: %v", err)
	}
	if result.ExecutionID != "exec-inline" || result.PipelineVersion != 0 || result.Replayed {
		t.Fatalf("inline result = %+v; want exec-inline version 0 fresh", result)
	}

	if rows := countTableRows(t, backend, scope, tablePipelines); rows != 0 {
		t.Fatalf("inline start created %d pipeline rows; want none", rows)
	}

	row, err := backend.Read(context.Background(), scope, tableExecutions, "exec-inline")
	if err != nil {
		t.Fatalf("read executions: %v", err)
	}
	if row.Values["pipeline_id"] != nil {
		t.Fatalf("inline pipeline_id = %v; want NULL", row.Values["pipeline_id"])
	}
	if row.Values["task_id"] != nil {
		t.Fatalf("inline task_id = %v; want NULL", row.Values["task_id"])
	}
	snapshotData, ok := row.Values["snapshot"].([]byte)
	if !ok {
		t.Fatalf("snapshot column = %T; want bytes", row.Values["snapshot"])
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(snapshotData, &document); err != nil {
		t.Fatalf("snapshot document: %v", err)
	}
	if _, present := document["pipeline_id"]; present {
		t.Fatal("inline snapshot document carries pipeline_id")
	}
	if _, present := document["pipeline_version"]; present {
		t.Fatal("inline snapshot document carries pipeline_version")
	}

	aggregate := loadExecution(t, adapter, scope, "exec-inline")
	if aggregate.State != kernel.ExecutionQueued || aggregate.Version != 1 {
		t.Fatalf("inline execution = %+v; want queued version 1", aggregate)
	}
	if got := string(aggregate.Snapshot.YAML()); got != string(yaml) {
		t.Fatalf("inline yaml = %q; want the exact admitted bytes %q", got, string(yaml))
	}
	if aggregate.Snapshot.YAMLDigest() != snapshot.YAMLSHA256 {
		t.Fatal("inline snapshot hash changed between admission and reload")
	}
	if got := aggregate.Snapshot.Inputs()["target"]; got != "linux" {
		t.Fatalf("inline input = %q; want linux", got)
	}
	if len(aggregate.Actions) != 1 {
		t.Fatalf("inline actions = %d; want 1", len(aggregate.Actions))
	}
	build := aggregate.Actions[0]
	if build.Name != "build" || build.Container == nil || build.Container.Definition.Image != containerAction().Container.Image {
		t.Fatalf("inline action did not round-trip: %+v", build)
	}

	view, err := adapter.Inspect(context.Background(), scope, "exec-inline")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if view.PipelineID != "" {
		t.Fatalf("inline view pipeline = %q; want empty", view.PipelineID)
	}
	if view.TaskID != "" {
		t.Fatalf("inline view task = %q; want empty", view.TaskID)
	}
}

// TestInlineStartReceiptConvergence pins exactly-once admission for inline
// starts: equal retries and a lost response replay the original execution, a
// concurrent duplicate commits one execution, and changed YAML or inputs under
// the same caller key refuse without a second execution.
func TestInlineStartReceiptConvergence(t *testing.T) {
	ctx := context.Background()
	scope := projectScope()
	yaml := []byte("name: inline\non: push\n")
	snapshot := inlineSnapshot(t, yaml, map[id.InputKey]string{"target": "linux"})
	content := `{"yaml_sha256":"sha256:inline-a","inputs":{"target":"linux"}}`

	t.Run("equal retry replays the original execution", func(t *testing.T) {
		adapter, backend := newStore(t)
		unit := inlineStartUnit(scope, "exec-inline", "inline-retry", snapshot, containerAction())
		unit.Receipt = inlineReceiptFor(t, scope, "inline-retry", content)

		first, err := adapter.Start(ctx, unit)
		if err != nil {
			t.Fatalf("first Start: %v", err)
		}
		second, err := adapter.Start(ctx, unit)
		if err != nil {
			t.Fatalf("second Start: %v", err)
		}
		if !second.Replayed || second.ExecutionID != first.ExecutionID || second.PipelineVersion != 0 {
			t.Fatalf("retry = %+v; want %+v replayed", second, first)
		}
		if rows := countTableRows(t, backend, scope, tableExecutions); rows != 1 {
			t.Fatalf("executions = %d; want exactly one", rows)
		}
	})

	t.Run("lost response retry replays the committed execution", func(t *testing.T) {
		adapter, backend := newStore(t)
		unit := inlineStartUnit(scope, "exec-inline", "inline-lost", snapshot, containerAction())
		unit.Receipt = inlineReceiptFor(t, scope, "inline-lost", content)

		committed, err := adapter.Start(ctx, unit)
		if err != nil {
			t.Fatalf("committed Start: %v", err)
		}
		// The caller never observed the first result; the identical retry must
		// return the original committed execution instead of creating another.
		replayed, err := adapter.Start(ctx, unit)
		if err != nil {
			t.Fatalf("lost-response retry: %v", err)
		}
		if !replayed.Replayed || replayed.ExecutionID != committed.ExecutionID {
			t.Fatalf("lost-response retry = %+v; want %+v replayed", replayed, committed)
		}
		if rows := countTableRows(t, backend, scope, tableExecutions); rows != 1 {
			t.Fatalf("executions = %d; want exactly one", rows)
		}
	})

	t.Run("changed inputs refuse under the same caller key", func(t *testing.T) {
		adapter, backend := newStore(t)
		unit := inlineStartUnit(scope, "exec-inline", "inline-changed-inputs", snapshot, containerAction())
		unit.Receipt = inlineReceiptFor(t, scope, "inline-changed-inputs", content)
		if _, err := adapter.Start(ctx, unit); err != nil {
			t.Fatalf("first Start: %v", err)
		}

		changed := unit
		changed.NewExecutionID = "exec-changed-inputs"
		changed.Receipt = inlineReceiptFor(t, scope, "inline-changed-inputs", `{"yaml_sha256":"sha256:inline-a","inputs":{"target":"darwin"}}`)
		_, err := adapter.Start(ctx, changed)
		requireClass(t, err, fault.ChangedIdempotencyContent)
		if rows := countTableRows(t, backend, scope, tableExecutions); rows != 1 {
			t.Fatalf("executions = %d; want exactly one", rows)
		}
	})

	t.Run("changed yaml bytes refuse under the same caller key", func(t *testing.T) {
		adapter, backend := newStore(t)
		unit := inlineStartUnit(scope, "exec-inline", "inline-changed-yaml", snapshot, containerAction())
		unit.Receipt = inlineReceiptFor(t, scope, "inline-changed-yaml", content)
		if _, err := adapter.Start(ctx, unit); err != nil {
			t.Fatalf("first Start: %v", err)
		}

		otherSnapshot := inlineSnapshot(t, []byte("name: inline-changed\non: push\n"), map[id.InputKey]string{"target": "linux"})
		changed := inlineStartUnit(scope, "exec-changed-yaml", "inline-changed-yaml", otherSnapshot, containerAction())
		changed.Receipt = inlineReceiptFor(t, scope, "inline-changed-yaml", `{"yaml_sha256":"sha256:inline-b","inputs":{"target":"linux"}}`)
		_, err := adapter.Start(ctx, changed)
		requireClass(t, err, fault.ChangedIdempotencyContent)

		aggregate := loadExecution(t, adapter, scope, "exec-inline")
		if got := string(aggregate.Snapshot.YAML()); got != string(yaml) {
			t.Fatalf("committed inline yaml = %q; want the original %q", got, string(yaml))
		}
		if rows := countTableRows(t, backend, scope, tableExecutions); rows != 1 {
			t.Fatalf("executions = %d; want exactly one", rows)
		}
	})

	t.Run("concurrent duplicate creates exactly one execution", func(t *testing.T) {
		adapter, backend := newStore(t)
		executions := []id.ExecutionID{"exec-a", "exec-b"}
		results := make([]execution.StartResult, 2)
		errs := make([]error, 2)
		units := make([]execution.StartUnit, 2)
		for index, executionID := range executions {
			units[index] = inlineStartUnit(scope, executionID, "inline-race", snapshot, containerAction())
			units[index].Receipt = inlineReceiptFor(t, scope, "inline-race", content)
			units[index].Receipt.ID = "receipt-inline-race"
			units[index].Audit = []event.Audit{testAudit(scope, "inline-race")}
			units[index].Events = []event.Envelope{testEvent(scope, "inline-race", string(executionID))}
		}

		var wait sync.WaitGroup
		var start sync.WaitGroup
		start.Add(1)
		for index := range units {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				start.Wait()
				results[index], errs[index] = adapter.Start(ctx, units[index])
			}(index)
		}
		start.Done()
		wait.Wait()

		for index := range errs {
			if errs[index] != nil {
				t.Fatalf("start %d: %v", index, errs[index])
			}
		}
		if rows := countTableRows(t, backend, scope, tableExecutions); rows != 1 {
			t.Fatalf("executions = %d; want exactly one", rows)
		}
		winner := results[0].ExecutionID
		loser := executions[1]
		if winner == executions[1] {
			loser = executions[0]
		}
		if _, err := adapter.Load(ctx, scope, winner); err != nil {
			t.Fatalf("winner load: %v", err)
		}
		if _, err := adapter.Load(ctx, scope, loser); err == nil {
			t.Fatal("the losing duplicate must not commit a second execution")
		}
	})
}

// TestInlineStartAuthorityAndRollback pins the commit-time guards of a
// pipeline-less start: the current membership union and service enablement are
// still re-read for a fresh commit and for an equal replay, and a failed commit
// leaves no partial execution, action, receipt, event or audit row.
func TestInlineStartAuthorityAndRollback(t *testing.T) {
	ctx := context.Background()
	scope := projectScope()
	snapshot := inlineSnapshot(t, []byte("name: inline\non: push\n"), nil)

	t.Run("revoked membership refuses commit and replay", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedMembership(t, backend, scope, "membership-1", "user-1", []tenancy.Role{tenancy.RoleEngineer}, 1)
		unit := inlineStartUnit(scope, "exec-membership", "inline-membership", snapshot, containerAction())
		unit.Authority = []authorize.AuthorityRead{inlineMembershipRead(scope, "user-1", false)}
		if _, err := adapter.Start(ctx, unit); err != nil {
			t.Fatalf("first inline Start: %v", err)
		}

		replaceMembershipRoles(t, backend, scope, "membership-1", 1, []tenancy.Role{tenancy.RoleViewer})
		_, err := adapter.Start(ctx, unit)
		requireClass(t, err, fault.Forbidden)
		if rows := countTableRows(t, backend, scope, tableExecutions); rows != 1 {
			t.Fatalf("executions = %d; want exactly one", rows)
		}
	})

	t.Run("disabled service refuses commit and replay", func(t *testing.T) {
		adapter, backend := newStore(t)
		seedServiceWithRoles(t, backend, scope, "service-1", "enabled", 1, []tenancy.Role{tenancy.RoleEngineer})
		unit := inlineStartUnit(scope, "exec-service", "inline-service", snapshot, containerAction())
		unit.ExecutionPrincipal = "service-1"
		unit.Authority = []authorize.AuthorityRead{inlineMembershipRead(scope, "service-1", true)}
		if _, err := adapter.Start(ctx, unit); err != nil {
			t.Fatalf("first inline Start: %v", err)
		}

		if _, err := backend.Execute(ctx, scope, store.Unit{Writes: []store.Write{{
			Table:         tableServices,
			ID:            "service-1",
			ExpectVersion: 1,
			Values:        map[string]any{"state": "disabled"},
		}}}); err != nil {
			t.Fatalf("disable service: %v", err)
		}
		_, err := adapter.Start(ctx, unit)
		requireClass(t, err, fault.Forbidden)
		if rows := countTableRows(t, backend, scope, tableExecutions); rows != 1 {
			t.Fatalf("executions = %d; want exactly one", rows)
		}
	})

	t.Run("failed commit leaves no partial records", func(t *testing.T) {
		adapter, backend := newStore(t)
		unit := inlineStartUnit(scope, "exec-rollback", "inline-rollback", snapshot, containerAction())
		injected := fault.New(fault.DependencyUnavailable, "simulated inline commit failure")
		backend.FailNextCommit(injected)

		_, err := adapter.Start(ctx, unit)
		requireClass(t, err, fault.DependencyUnavailable)
		if _, err := adapter.Load(ctx, scope, "exec-rollback"); err == nil {
			t.Fatal("failed inline commit left an execution row")
		}
		if rows := countTableRows(t, backend, scope, tableActions); rows != 0 {
			t.Fatalf("failed inline commit left %d action rows", rows)
		}
		if _, found := backend.Receipt(unit.Receipt.Key); found {
			t.Fatal("failed inline commit left a receipt")
		}
		if events := backend.Events(scope); len(events) != 0 {
			t.Fatalf("failed inline commit left %d event rows", len(events))
		}
		if audits := backend.Audit(scope); len(audits) != 0 {
			t.Fatalf("failed inline commit left %d audit rows", len(audits))
		}

		if _, err := adapter.Start(ctx, unit); err != nil {
			t.Fatalf("retry after failed inline commit: %v", err)
		}
		if rows := countTableRows(t, backend, scope, tableExecutions); rows != 1 {
			t.Fatalf("executions = %d; want exactly one after the retry", rows)
		}
	})
}

// inlineMembershipRead is the commit-time membership authority read of one
// inline pipeline.start. The human and service grant vocabularies differ, so the
// caller says which union to evaluate.
func inlineMembershipRead(scope tenancy.Scope, principal id.PrincipalID, service bool) authorize.AuthorityRead {
	required := authorize.Requires{HumanRoles: []tenancy.Role{tenancy.RoleEngineer}}
	if service {
		required = authorize.Requires{ServiceRoles: []tenancy.Role{tenancy.RoleEngineer}}
	}
	return authorize.AuthorityRead{
		Operation:   "pipeline.start",
		Required:    required,
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
	}
}

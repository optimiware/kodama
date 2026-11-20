package work

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/receipt"
	"github.com/optimiware/kodema/internal/shell/resource"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

// definitionFromRow decodes one pipelines row for the round-trip assertions.
func definitionFromRow(row store.Row) pipeline.Definition {
	return pipeline.Definition{
		ID:      pipeline.PipelineID(textValue(row, "id")),
		Name:    textValue(row, "name"),
		YAML:    bytesValue(row, "yaml"),
		Version: versionValue(row, "version"),
	}
}

// changeUnit builds one complete pipeline-management commit unit.
func (f *fixture) changeUnit(scope tenancy.Scope, kind pipeline.ChangeKind, identity pipeline.PipelineID, name string, yaml []byte, expected uint64, record receipt.Record) pipeline.ChangeUnit {
	return pipeline.ChangeUnit{
		Owner:           scope,
		Kind:            kind,
		PipelineID:      identity,
		Name:            name,
		YAML:            yaml,
		ExpectedVersion: expected,
		Receipt:         record,
		Audit:           []event.Audit{f.audit(scope, string(kind))},
		Events:          []event.Envelope{f.event(scope, name, expected+1)},
	}
}

func TestPipelineChangeApplyCreatesExactBytes(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	record := fixture.receipt("pipeline.apply", "apply-1", contentHashA)
	unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "build", []byte("kind: Pipeline\nversion: 1\n"), 0, record)
	unit.Events[0].Type = event.TypeConfigChanged
	result, err := fixture.pipelines.Change(context.Background(), unit)
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if result.PipelineID != "pipeline-new-1" || result.Version != 1 || result.Replayed {
		t.Fatalf("unexpected apply result: %+v", result)
	}
	row, ok := fixture.storedRow(t, scope, "pipelines", string(result.PipelineID))
	if !ok {
		t.Fatal("the pipeline row was not committed")
	}
	definition := definitionFromRow(row)
	if definition.Name != "build" || string(definition.YAML) != "kind: Pipeline\nversion: 1\n" || definition.Version != 1 {
		t.Fatalf("pipeline row = %+v", definition)
	}
	stored, ok := fixture.store.Receipt(record.Key)
	if !ok {
		t.Fatal("the receipt was not committed")
	}
	replayed, isReplay, err := replay[pipeline.ChangeResult](store.UnitResult{Replayed: true, Receipt: stored})
	if err != nil || !isReplay {
		t.Fatalf("decode receipt result: %v / %v", isReplay, err)
	}
	if replayed.PipelineID != result.PipelineID || replayed.Version != 1 {
		t.Fatalf("receipt result = %+v", replayed)
	}
	if count := fixture.countRows(t, scope, "audit", nil); count != 1 {
		t.Fatalf("committed audit rows = %d; want 1", count)
	}
	if events := fixture.store.Events(scope); len(events) != 1 || events[0].Envelope.Type != event.TypeConfigChanged {
		t.Fatalf("committed events = %+v", events)
	}
}

func TestPipelineChangeUpdateAndDeleteAtExpectedVersion(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\nversion: 1\n"), 1)
	update := fixture.changeUnit(scope, pipeline.ChangeUpdate, testPipeline, "build", []byte("kind: Pipeline\nversion: 2\n"), 1, fixture.receipt("pipeline.update", "update-1", contentHashA))
	result, err := fixture.pipelines.Change(context.Background(), update)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Version != 2 {
		t.Fatalf("unexpected update result: %+v", result)
	}
	row, _ := fixture.storedRow(t, scope, "pipelines", string(testPipeline))
	if versionValue(row, "version") != 2 || string(bytesValue(row, "yaml")) != "kind: Pipeline\nversion: 2\n" {
		t.Fatalf("pipeline row after update = %+v", row.Values)
	}
	stale := fixture.changeUnit(scope, pipeline.ChangeUpdate, testPipeline, "build", []byte("stale"), 1, fixture.receipt("pipeline.update", "update-stale", contentHashA))
	if _, err := fixture.pipelines.Change(context.Background(), stale); err == nil {
		t.Fatal("a stale update must be refused")
	} else {
		mustClass(t, err, fault.VersionConflict)
	}
	row, _ = fixture.storedRow(t, scope, "pipelines", string(testPipeline))
	if versionValue(row, "version") != 2 || string(bytesValue(row, "yaml")) != "kind: Pipeline\nversion: 2\n" {
		t.Fatalf("a stale update changed the row: %+v", row.Values)
	}
	deletion := fixture.changeUnit(scope, pipeline.ChangeDelete, testPipeline, "build", nil, 2, fixture.receipt("pipeline.delete", "delete-1", contentHashA))
	result, err = fixture.pipelines.Change(context.Background(), deletion)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result.Version != 3 {
		t.Fatalf("unexpected delete result: %+v", result)
	}
	if _, ok := fixture.storedRow(t, scope, "pipelines", string(testPipeline)); ok {
		t.Fatal("the deleted pipeline row is still committed")
	}
}

func TestPipelineChangeReplayAndChangedContent(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("v1"), 1)
	unit := fixture.changeUnit(scope, pipeline.ChangeUpdate, testPipeline, "build", []byte("v2"), 1, fixture.receipt("pipeline.update", "update-once", contentHashA))
	first, err := fixture.pipelines.Change(context.Background(), unit)
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	replayed, err := fixture.pipelines.Change(context.Background(), unit)
	if err != nil {
		t.Fatalf("replayed Change: %v", err)
	}
	if !replayed.Replayed || replayed.Version != first.Version {
		t.Fatalf("replay = %+v; want the original %+v", replayed, first)
	}
	row, _ := fixture.storedRow(t, scope, "pipelines", string(testPipeline))
	if versionValue(row, "version") != 2 {
		t.Fatalf("a replayed update advanced the version: %d", versionValue(row, "version"))
	}
	changed := unit
	changed.Receipt = fixture.receipt("pipeline.update", "update-once", contentHashB)
	if _, err := fixture.pipelines.Change(context.Background(), changed); err == nil {
		t.Fatal("changed receipt content must be refused")
	} else {
		mustClass(t, err, fault.ChangedIdempotencyContent)
	}
}

func TestPipelineChangeRollbackLeavesNothing(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.store.FailNextCommit(fault.New(fault.DependencyUnavailable, "commit did not start"))
	record := fixture.receipt("pipeline.apply", "apply-rollback", contentHashA)
	_, err := fixture.pipelines.Change(context.Background(), fixture.changeUnit(scope, pipeline.ChangeApply, "", "build", []byte("v1"), 0, record))
	mustClass(t, err, fault.DependencyUnavailable)
	if count := fixture.countRows(t, scope, "pipelines", nil); count != 0 {
		t.Fatalf("a failed apply committed %d pipelines", count)
	}
	if _, ok := fixture.store.Receipt(record.Key); ok {
		t.Fatal("a failed apply committed its receipt")
	}
	if events := fixture.store.Events(scope); len(events) != 0 {
		t.Fatalf("a failed apply published %d events", len(events))
	}
	if count := fixture.countRows(t, scope, "audit", nil); count != 0 {
		t.Fatalf("a failed apply committed %d audit rows", count)
	}
}

func TestPipelineChangeRaceOneWinner(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("v1"), 1)
	units := []pipeline.ChangeUnit{
		fixture.changeUnit(scope, pipeline.ChangeUpdate, testPipeline, "build", []byte("a"), 1, fixture.receipt("pipeline.update", "race-a", contentHashA)),
		fixture.changeUnit(scope, pipeline.ChangeUpdate, testPipeline, "build", []byte("b"), 1, fixture.receipt("pipeline.update", "race-b", contentHashA)),
	}
	start := make(chan struct{})
	results := make([]error, len(units))
	var wait sync.WaitGroup
	for index := range units {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, results[index] = fixture.pipelines.Change(context.Background(), units[index])
		}(index)
	}
	close(start)
	wait.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		mustClass(t, err, fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("pipeline race produced %d winners; want 1", winners)
	}
	row, _ := fixture.storedRow(t, scope, "pipelines", string(testPipeline))
	if versionValue(row, "version") != 2 {
		t.Fatalf("pipeline version = %d; want 2", versionValue(row, "version"))
	}
}

func TestPipelineChangeCrossScopeRefused(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPipeline(t, scope, testPipeline, "build", []byte("v1"), 1)
	unit := fixture.changeUnit(otherScope(), pipeline.ChangeUpdate, testPipeline, "build", []byte("hijack"), 1, fixture.receiptFor(otherScope(), "pipeline.update", "cross-scope", contentHashA))
	if _, err := fixture.pipelines.Change(context.Background(), unit); err == nil {
		t.Fatal("a cross-scope pipeline update must be refused")
	} else {
		mustClass(t, err, fault.VersionConflict)
	}
	row, _ := fixture.storedRow(t, scope, "pipelines", string(testPipeline))
	if string(bytesValue(row, "yaml")) != "v1" {
		t.Fatalf("a cross-scope update changed the row: %+v", row.Values)
	}
}

func testPushTriggers() []pipeline.PushTrigger {
	return []pipeline.PushTrigger{{
		Repository: "repo-1",
		Service:    "svc-a",
		Refs:       []string{"refs/heads/main", "refs/heads/release"},
	}}
}

func TestPipelineReaderRoundTripsPushProjection(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedServiceWithRoles(t, scope, "svc-a", serviceStateEnabled, 1, tenancy.RoleEngineer)
	fixture.seedServiceWithRoles(t, scope, "svc-b", serviceStateEnabled, 1, tenancy.RoleEngineer)
	unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "build", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "push-1", contentHashA))
	unit.PushTriggers = testPushTriggers()
	result, err := fixture.pipelines.Change(context.Background(), unit)
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	definition, err := fixture.pipelines.Get(context.Background(), scope, result.PipelineID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(definition.PushTriggers) != 1 {
		t.Fatalf("push projection = %+v; want one trigger", definition.PushTriggers)
	}
	trigger := definition.PushTriggers[0]
	if trigger.Repository != "repo-1" || trigger.Service != "svc-a" || len(trigger.Refs) != 2 {
		t.Fatalf("push projection = %+v", trigger)
	}
	if string(definition.YAML) != "kind: Pipeline\n" || definition.Version != 1 {
		t.Fatalf("definition = %+v", definition)
	}
	byName, err := fixture.pipelines.ByName(context.Background(), scope, "build")
	if err != nil {
		t.Fatalf("ByName: %v", err)
	}
	if byName.ID != result.PipelineID || len(byName.PushTriggers) != 1 {
		t.Fatalf("ByName = %+v", byName)
	}
	listed, err := fixture.pipelines.List(context.Background(), scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != result.PipelineID {
		t.Fatalf("List = %+v", listed)
	}
	// An update replaces the projection in the same unit.
	update := fixture.changeUnit(scope, pipeline.ChangeUpdate, result.PipelineID, "build", []byte("kind: Pipeline\nversion: 2\n"), 1, fixture.receipt("pipeline.update", "push-2", contentHashA))
	update.PushTriggers = []pipeline.PushTrigger{{Repository: "repo-2", Service: "svc-b", Refs: []string{"refs/heads/dev"}}}
	if _, err := fixture.pipelines.Change(context.Background(), update); err != nil {
		t.Fatalf("Update: %v", err)
	}
	definition, err = fixture.pipelines.Get(context.Background(), scope, result.PipelineID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if len(definition.PushTriggers) != 1 || definition.PushTriggers[0].Repository != "repo-2" {
		t.Fatalf("updated push projection = %+v", definition.PushTriggers)
	}
	// A plain projection is stored as an empty array, never NULL.
	plain := fixture.changeUnit(scope, pipeline.ChangeUpdate, result.PipelineID, "build", []byte("kind: Pipeline\nversion: 3\n"), 2, fixture.receipt("pipeline.update", "push-3", contentHashA))
	if _, err := fixture.pipelines.Change(context.Background(), plain); err != nil {
		t.Fatalf("plain Update: %v", err)
	}
	definition, err = fixture.pipelines.Get(context.Background(), scope, result.PipelineID)
	if err != nil {
		t.Fatalf("Get after plain update: %v", err)
	}
	if len(definition.PushTriggers) != 0 {
		t.Fatalf("plain push projection = %+v; want empty", definition.PushTriggers)
	}
	row, _ := fixture.storedRow(t, scope, "pipelines", string(result.PipelineID))
	if string(bytesValue(row, "push_triggers")) != "[]" {
		t.Fatalf("stored push_triggers = %q; want []", bytesValue(row, "push_triggers"))
	}
}

func TestPipelineCandidatesRequireMatchingBinding(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedServiceWithRoles(t, scope, "svc-a", serviceStateEnabled, 1, tenancy.RoleEngineer)
	fixture.seedServiceWithRoles(t, scope, "svc-b", serviceStateEnabled, 1, tenancy.RoleEngineer)
	unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "build", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "candidate-1", contentHashA))
	unit.PushTriggers = testPushTriggers()
	if _, err := fixture.pipelines.Change(context.Background(), unit); err != nil {
		t.Fatalf("Change: %v", err)
	}
	fixture.seedBinding(t, scope, "binding-1", "repo-1", "svc-a", 3)
	candidates, err := fixture.pipelines.Candidates(context.Background(), scope)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d; want 1", len(candidates))
	}
	candidate := candidates[0]
	if candidate.Binding.ID != "binding-1" || candidate.Binding.Version != 3 {
		t.Fatalf("candidate binding = %+v", candidate.Binding)
	}
	if candidate.Binding.ResourceID != "repo-1" || candidate.Binding.EnforcingService != "svc-a" {
		t.Fatalf("candidate binding identity = %+v", candidate.Binding)
	}
	if len(candidate.Binding.Operations) != 1 || candidate.Binding.Operations[0] != resource.OperationGitRefUpdate {
		t.Fatalf("candidate binding operations = %+v", candidate.Binding.Operations)
	}
	if !candidate.Binding.DeclaresReport("git.ref.updated") {
		t.Fatalf("candidate binding reportable events = %+v", candidate.Binding.ReportableEvents)
	}
	if candidate.Binding.Metadata["owner"] != "team" {
		t.Fatalf("candidate binding metadata = %+v", candidate.Binding.Metadata)
	}
	if len(candidate.Definition.PushTriggers) != 1 || candidate.Definition.PushTriggers[0].Service != "svc-a" {
		t.Fatalf("candidate definition = %+v", candidate.Definition)
	}
	// A pipeline whose trigger has no binding is omitted so an orphaned
	// selection is refused instead of started without a binding.
	second := fixture.changeUnit(scope, pipeline.ChangeApply, "", "unbound", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "candidate-2", contentHashA))
	second.PushTriggers = []pipeline.PushTrigger{{Repository: "repo-2", Service: "svc-b", Refs: []string{"refs/heads/main"}}}
	if _, err := fixture.pipelines.Change(context.Background(), second); err != nil {
		t.Fatalf("second Change: %v", err)
	}
	candidates, err = fixture.pipelines.Candidates(context.Background(), scope)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Definition.Name != "build" {
		t.Fatalf("candidates with an unbound pipeline = %+v", candidates)
	}
}

// TestPipelineCandidatesSelectBindingByRepository pins KODEMA.md's push
// trigger: `service` names the automatic-start principal, not the reporting
// Git service, so the candidate binding is the repository's binding whatever
// its enforcing service. Every binding of the repository is a candidate; the
// processor then requires the enforcing service to be the report source.
func TestPipelineCandidatesSelectBindingByRepository(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedServiceWithRoles(t, scope, "svc-start", serviceStateEnabled, 1, tenancy.RoleEngineer)
	unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "build", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "candidate-start-service", contentHashA))
	unit.PushTriggers = []pipeline.PushTrigger{{Repository: "repo-1", Service: "svc-start", Refs: []string{"refs/heads/main"}}}
	if _, err := fixture.pipelines.Change(context.Background(), unit); err != nil {
		t.Fatalf("Change: %v", err)
	}
	fixture.seedBinding(t, scope, "binding-git", "repo-1", "svc-git", 2)
	fixture.seedBinding(t, scope, "binding-mirror", "repo-1", "svc-mirror", 1)
	fixture.seedBinding(t, scope, "binding-other-repo", "repo-2", "svc-git", 1)

	candidates, err := fixture.pipelines.Candidates(context.Background(), scope)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	got := make(map[resource.BindingID]string, len(candidates))
	for _, candidate := range candidates {
		if candidate.Definition.Name != "build" {
			t.Fatalf("candidate definition = %q; want build", candidate.Definition.Name)
		}
		got[candidate.Binding.ID] = string(candidate.Binding.EnforcingService)
	}
	want := map[resource.BindingID]string{"binding-git": "svc-git", "binding-mirror": "svc-mirror"}
	if !reflect.DeepEqual(got, want) || len(candidates) != len(want) {
		t.Fatalf("candidate bindings = %v (%d candidates); want one candidate per repo-1 binding %v", got, len(candidates), want)
	}
}

func TestPipelineReaderScopeIsolation(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedServiceWithRoles(t, scope, "svc-a", serviceStateEnabled, 1, tenancy.RoleEngineer)
	unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "build", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "scope-1", contentHashA))
	unit.PushTriggers = testPushTriggers()
	result, err := fixture.pipelines.Change(context.Background(), unit)
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if _, err := fixture.pipelines.Get(context.Background(), otherScope(), result.PipelineID); err == nil {
		t.Fatal("a cross-scope pipeline read must be refused")
	} else {
		mustClass(t, err, fault.NotFound)
	}
	if _, err := fixture.pipelines.ByName(context.Background(), otherScope(), "build"); err == nil {
		t.Fatal("a cross-scope name read must be refused")
	} else {
		mustClass(t, err, fault.NotFound)
	}
	listed, err := fixture.pipelines.List(context.Background(), otherScope())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("a cross-scope list leaked definitions: %+v", listed)
	}
	candidates, err := fixture.pipelines.Candidates(context.Background(), otherScope())
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("a cross-scope candidate list leaked definitions: %+v", candidates)
	}
}

// TestPipelineChangeEnforcesPushServiceValidation proves Pipelines.Change
// enforces on.push automatic-start service existence, enablement and engineer
// role at the commit boundary for both project-scoped and tenant-wide services.
func TestPipelineChangeEnforcesPushServiceValidation(t *testing.T) {
	ctx := context.Background()
	scope := testScope()
	tenantWide := tenancy.Scope{TenantID: scope.TenantID}

	t.Run("missing service refused", func(t *testing.T) {
		fixture := newFixture(t)
		before := fixture.commitCounts(t, scope)
		unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "push", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "missing-svc", contentHashA))
		unit.PushTriggers = testPushTriggers()
		_, err := fixture.pipelines.Change(ctx, unit)
		mustClass(t, err, fault.InvalidInput)
		fixture.requireUnchanged(t, scope, before)
	})

	t.Run("disabled service refused", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.seedServiceWithRoles(t, scope, "svc-a", "disabled", 1, tenancy.RoleEngineer)
		before := fixture.commitCounts(t, scope)
		unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "push", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "disabled-svc", contentHashA))
		unit.PushTriggers = testPushTriggers()
		_, err := fixture.pipelines.Change(ctx, unit)
		mustClass(t, err, fault.InvalidInput)
		fixture.requireUnchanged(t, scope, before)
	})

	t.Run("service without engineer role refused", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.seedServiceWithRoles(t, scope, "svc-a", serviceStateEnabled, 1, tenancy.RoleViewer)
		before := fixture.commitCounts(t, scope)
		unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "push", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "viewer-svc", contentHashA))
		unit.PushTriggers = testPushTriggers()
		_, err := fixture.pipelines.Change(ctx, unit)
		mustClass(t, err, fault.InvalidInput)
		fixture.requireUnchanged(t, scope, before)
	})

	t.Run("enabled tenant-wide engineer service accepted", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.seedServiceWithRoles(t, tenantWide, "svc-a", serviceStateEnabled, 1, tenancy.RoleEngineer)
		unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "push", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "tenant-svc", contentHashA))
		unit.PushTriggers = testPushTriggers()
		result, err := fixture.pipelines.Change(ctx, unit)
		if err != nil {
			t.Fatalf("Change with tenant-wide engineer service: %v", err)
		}
		if result.Version != 1 {
			t.Fatalf("result = %+v; want version 1", result)
		}
	})
}

// TestPipelineChangePinsPushServiceRowAgainstConcurrentModification proves
// Pipelines.Change pins each referenced on.push services row (version, enabled
// state and TenantWide flag) inside the atomic commit unit so a concurrent
// service.disable or engineer-role removal cannot race past validation.
func TestPipelineChangePinsPushServiceRowAgainstConcurrentModification(t *testing.T) {
	ctx := context.Background()
	scope := testScope()
	tenantWide := tenancy.Scope{TenantID: scope.TenantID}

	cases := []struct {
		name         string
		serviceScope tenancy.Scope
		mutate       func(t *testing.T, fixture *fixture, svcScope tenancy.Scope)
	}{
		{
			name:         "project service disabled concurrently",
			serviceScope: scope,
			mutate: func(t *testing.T, fixture *fixture, svcScope tenancy.Scope) {
				t.Helper()
				if _, err := fixture.store.Execute(ctx, svcScope, store.Unit{
					Writes: []store.Write{{
						Table:         "services",
						ID:            "svc-a",
						ExpectVersion: 1,
						Values:        map[string]any{"state": "disabled"},
					}},
				}); err != nil {
					t.Fatalf("concurrent disable service: %v", err)
				}
			},
		},
		{
			name:         "project service engineer role removed concurrently",
			serviceScope: scope,
			mutate: func(t *testing.T, fixture *fixture, svcScope tenancy.Scope) {
				t.Helper()
				roles, err := jsonColumn([]tenancy.Role{tenancy.RoleViewer})
				if err != nil {
					t.Fatalf("encode roles: %v", err)
				}
				if _, err := fixture.store.Execute(ctx, svcScope, store.Unit{
					Writes: []store.Write{{
						Table:         "services",
						ID:            "svc-a",
						ExpectVersion: 1,
						Values:        map[string]any{"roles": roles},
					}},
				}); err != nil {
					t.Fatalf("concurrent role removal: %v", err)
				}
			},
		},
		{
			name:         "tenant-wide service disabled concurrently",
			serviceScope: tenantWide,
			mutate: func(t *testing.T, fixture *fixture, svcScope tenancy.Scope) {
				t.Helper()
				if _, err := fixture.store.Execute(ctx, svcScope, store.Unit{
					Writes: []store.Write{{
						Table:         "services",
						ID:            "svc-a",
						ExpectVersion: 1,
						Values:        map[string]any{"state": "disabled"},
					}},
				}); err != nil {
					t.Fatalf("concurrent disable tenant-wide service: %v", err)
				}
			},
		},
		{
			name:         "tenant-wide service engineer role removed concurrently",
			serviceScope: tenantWide,
			mutate: func(t *testing.T, fixture *fixture, svcScope tenancy.Scope) {
				t.Helper()
				roles, err := jsonColumn([]tenancy.Role{tenancy.RoleViewer})
				if err != nil {
					t.Fatalf("encode roles: %v", err)
				}
				if _, err := fixture.store.Execute(ctx, svcScope, store.Unit{
					Writes: []store.Write{{
						Table:         "services",
						ID:            "svc-a",
						ExpectVersion: 1,
						Values:        map[string]any{"roles": roles},
					}},
				}); err != nil {
					t.Fatalf("concurrent tenant-wide role removal: %v", err)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t)
			fixture.seedServiceWithRoles(t, testCase.serviceScope, "svc-a", serviceStateEnabled, 1, tenancy.RoleEngineer)
			var before commitCounts
			executor := &mutatingExecutor{
				inner: fixture.store,
				hook: func() {
					testCase.mutate(t, fixture, testCase.serviceScope)
					before = fixture.commitCounts(t, scope)
				},
			}
			racing, err := NewPipelines(executor, fixture.store, nil)
			if err != nil {
				t.Fatalf("NewPipelines: %v", err)
			}
			unit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "push-race", []byte("kind: Pipeline\n"), 0, fixture.receipt("pipeline.apply", "race-1", contentHashA))
			unit.PushTriggers = testPushTriggers()
			_, err = racing.Change(ctx, unit)
			mustClass(t, err, fault.VersionConflict)
			fixture.requireUnchanged(t, scope, before)
			if count := fixture.countRows(t, scope, "pipelines", nil); count != 0 {
				t.Fatalf("pipelines committed after concurrent service modification = %d; want 0", count)
			}
		})
	}
}

// TestPipelineChangeReplaysPushPipelineAfterServiceDisable proves an equal-key
// retry of an already-committed on.push apply or update still converges on its
// original receipt even after the referenced service is disabled, while a
// changed-content retry returns changed_idempotency_content and a fresh commit
// is refused invalid_input.
func TestPipelineChangeReplaysPushPipelineAfterServiceDisable(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedServiceWithRoles(t, scope, "svc-a", serviceStateEnabled, 1, tenancy.RoleEngineer)

	applyUnit := fixture.changeUnit(scope, pipeline.ChangeApply, "", "push", []byte("kind: Pipeline\nv: 1\n"), 0, fixture.receipt("pipeline.apply", "push-apply-1", contentHashA))
	applyUnit.PushTriggers = testPushTriggers()
	applied, err := fixture.pipelines.Change(ctx, applyUnit)
	if err != nil {
		t.Fatalf("initial push apply: %v", err)
	}

	updateUnit := fixture.changeUnit(scope, pipeline.ChangeUpdate, applied.PipelineID, "push", []byte("kind: Pipeline\nv: 2\n"), 1, fixture.receipt("pipeline.update", "push-update-1", contentHashA))
	updateUnit.PushTriggers = testPushTriggers()
	updated, err := fixture.pipelines.Change(ctx, updateUnit)
	if err != nil {
		t.Fatalf("initial push update: %v", err)
	}

	// Disable the referenced service after both commits succeeded.
	if _, err := fixture.store.Execute(ctx, scope, store.Unit{
		Writes: []store.Write{{
			Table:         "services",
			ID:            "svc-a",
			ExpectVersion: 1,
			Values:        map[string]any{"state": "disabled"},
		}},
	}); err != nil {
		t.Fatalf("disable svc-a: %v", err)
	}
	before := fixture.commitCounts(t, scope)

	replayedApply, err := fixture.pipelines.Change(ctx, applyUnit)
	if err != nil {
		t.Fatalf("replay apply after service disable: %v", err)
	}
	if !replayedApply.Replayed || replayedApply.PipelineID != applied.PipelineID || replayedApply.Version != applied.Version {
		t.Fatalf("replayed apply = %+v; want replayed %+v", replayedApply, applied)
	}

	replayedUpdate, err := fixture.pipelines.Change(ctx, updateUnit)
	if err != nil {
		t.Fatalf("replay update after service disable: %v", err)
	}
	if !replayedUpdate.Replayed || replayedUpdate.PipelineID != updated.PipelineID || replayedUpdate.Version != updated.Version {
		t.Fatalf("replayed update = %+v; want replayed %+v", replayedUpdate, updated)
	}
	fixture.requireUnchanged(t, scope, before)

	// Changed content under the same key must still report changed_idempotency_content.
	changed := applyUnit
	changed.Receipt.ContentHash = contentHashB
	_, err = fixture.pipelines.Change(ctx, changed)
	mustClass(t, err, fault.ChangedIdempotencyContent)

	// A fresh push apply against the now-disabled service must still be refused.
	fresh := fixture.changeUnit(scope, pipeline.ChangeApply, "", "push-2", []byte("kind: Pipeline\nv: 3\n"), 0, fixture.receipt("pipeline.apply", "push-apply-fresh", contentHashA))
	fresh.PushTriggers = testPushTriggers()
	_, err = fixture.pipelines.Change(ctx, fresh)
	mustClass(t, err, fault.InvalidInput)
	fixture.requireUnchanged(t, scope, before)
}

package work

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// serviceStartAuthority is the exact current-authority read set the push
// processor supplies for one pipeline.start: the service membership and
// enablement, the current pipeline/binding versions and the event.read grant.
func serviceStartAuthority(scope tenancy.Scope, principal id.PrincipalID, pipelineID pipeline.PipelineID, pipelineVersion uint64, bindingID string, bindingVersion uint64) []authorize.AuthorityRead {
	start, _ := authorize.LookupOperation("pipeline.start")
	read := func(operation authorize.Operation, check authorize.AuthorityCheck) authorize.AuthorityRead {
		return authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principal,
		}
	}
	reads := []authorize.AuthorityRead{
		read(start, authorize.CheckMembership),
		read(start, authorize.CheckServiceEnabled),
	}
	pipelineVersionRead := read(start, authorize.CheckPipelineVersion)
	pipelineVersionRead.TargetID = string(pipelineID)
	pipelineVersionRead.ExpectedVersion = pipelineVersion
	bindingVersionRead := read(start, authorize.CheckBindingVersion)
	bindingVersionRead.TargetID = bindingID
	bindingVersionRead.ExpectedVersion = bindingVersion
	reads = append(reads, pipelineVersionRead, bindingVersionRead)
	eventRead, _ := authorize.LookupOperation("event.read")
	for _, check := range eventRead.Authority {
		reads = append(reads, read(eventRead, check))
	}
	return reads
}

// seedPushServiceEnvironment seeds the pipeline, binding and enabled service
// with the roles pipeline.start and event.read require.
func (f *fixture) seedPushServiceEnvironment(t *testing.T, scope tenancy.Scope, serviceScope tenancy.Scope) {
	t.Helper()
	f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	f.seedBinding(t, scope, "binding-1", "repo-1", "svc-a", 1)
	f.seedServiceWithRoles(t, serviceScope, "svc-a", "enabled", 1, tenancy.RoleEngineer)
}

// TestTriggerStartRefusesServiceDemotedAfterSelect proves the start commit
// re-evaluates the current service membership/enablement: demoting the service
// after Select records exactly one permanent refusal, no execution and a
// completed receipt.
func TestTriggerStartRefusesServiceDemotedAfterSelect(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushServiceEnvironment(t, scope, scope)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	authority := serviceStartAuthority(scope, "svc-a", testPipeline, 1, "binding-1", 1)
	selectUnit := fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)
	selectUnit.Authority = authority
	if _, err := fixture.triggers.Select(context.Background(), selectUnit); err != nil {
		t.Fatalf("Select: %v", err)
	}
	// Demote the automatic service after the selection read its grant.
	fixture.setServiceRoles(t, scope, "svc-a", 1, tenancy.RoleViewer)
	start := fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-1", trigger.StatusSelected, 1, 1)
	start.Authority = authority
	result, err := fixture.triggers.Start(context.Background(), start)
	if err != nil {
		t.Fatalf("Start with a demoted service: %v", err)
	}
	if result.Status != trigger.StatusRefused || result.ExecutionID != "" || result.Replayed {
		t.Fatalf("authority refusal result = %+v", result)
	}
	if count := fixture.countRows(t, scope, "executions", nil); count != 0 {
		t.Fatalf("an authority refusal committed %d executions", count)
	}
	row, ok := fixture.scanRow(t, scope, "receipts", map[string]any{"id": string(record.ID)})
	if !ok || row.Values["completed_at"] == nil {
		t.Fatalf("the refusal receipt is not completed: %+v", row.Values)
	}
	completed, err := decodeSelectionResult(bytesValue(row, "result"))
	if err != nil || completed.Status != trigger.StatusRefused {
		t.Fatalf("completed refusal result = %+v / %v", completed, err)
	}
	if events := fixture.store.Events(scope); len(events) != 1 || events[0].Envelope.Type != event.TypePipelineStartRefused {
		t.Fatalf("refusal events = %+v", events)
	}
	// A retry after the refusal applies nothing and does not publish again.
	replayed, err := fixture.triggers.Start(context.Background(), start)
	if err != nil {
		t.Fatalf("replayed refusal: %v", err)
	}
	if !replayed.Replayed || replayed.Status != trigger.StatusRefused {
		t.Fatalf("replayed refusal = %+v", replayed)
	}
	if events := fixture.store.Events(scope); len(events) != 1 {
		t.Fatalf("a replayed refusal published %d events; want 1", len(events))
	}
	if count := fixture.countRows(t, scope, "executions", nil); count != 0 {
		t.Fatalf("a replayed refusal committed %d executions", count)
	}
}

// TestTriggerStartAcceptsTenantWideEnabledService proves a tenant-wide enabled
// service is pinned at its observed version instead of failing every start.
func TestTriggerStartAcceptsTenantWideEnabledService(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	tenant := tenancy.Scope{TenantID: testTenant}
	fixture.seedPushServiceEnvironment(t, scope, tenant)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	authority := serviceStartAuthority(scope, "svc-a", testPipeline, 1, "binding-1", 1)
	selectUnit := fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)
	selectUnit.Authority = authority
	if _, err := fixture.triggers.Select(context.Background(), selectUnit); err != nil {
		t.Fatalf("Select with a tenant-wide service: %v", err)
	}
	start := fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-1", trigger.StatusSelected, 1, 1)
	start.Authority = authority
	result, err := fixture.triggers.Start(context.Background(), start)
	if err != nil {
		t.Fatalf("Start with a tenant-wide service: %v", err)
	}
	if result.Status != trigger.StatusSelected || result.ExecutionID != "execution-1" {
		t.Fatalf("tenant-wide start result = %+v", result)
	}
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-1"); !ok {
		t.Fatal("the tenant-wide start did not commit its execution")
	}
}

// TestTriggerStartRefusesUnmappableAuthority proves an authority check with no
// store mapping is an error, never a silently skipped guard.
func TestTriggerStartRefusesUnmappableAuthority(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushServiceEnvironment(t, scope, scope)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	selectUnit := fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)
	selectUnit.Authority = serviceStartAuthority(scope, "svc-a", testPipeline, 1, "binding-1", 1)
	if _, err := fixture.triggers.Select(context.Background(), selectUnit); err != nil {
		t.Fatalf("Select: %v", err)
	}
	start := fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-1", trigger.StatusSelected, 1, 1)
	start.Authority = []authorize.AuthorityRead{{
		Operation:   "pipeline.start",
		Check:       authorize.AuthorityCheck("not_a_declared_check"),
		Scope:       scope,
		PrincipalID: "svc-a",
	}}
	_, err := fixture.triggers.Start(context.Background(), start)
	mustClass(t, err, fault.InvalidInput)
	if count := fixture.countRows(t, scope, "executions", nil); count != 0 {
		t.Fatalf("an unmappable authority committed %d executions", count)
	}
}

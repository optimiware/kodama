package platform_test

import (
	"context"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func ingressUnit(t *testing.T, kind event.SourceKind, principal id.PrincipalID, execution id.ExecutionID) event.IngressUnit {
	t.Helper()
	contentHash, err := receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(`{"observation":1}`)})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	scope := testScope()
	envelope := event.Envelope{
		ID:        testEventID,
		Type:      event.TypeCustomObservation,
		Source:    event.Source{Kind: kind, PrincipalID: principal},
		At:        fixedNow,
		Scope:     scope,
		SubjectID: "subject-1",
	}
	return event.IngressUnit{
		Owner:       scope,
		ExecutionID: execution,
		Event:       envelope,
		Receipt: receipt.Record{
			ID: testReceiptID,
			Key: receipt.Key{
				TenantID:    scope.TenantID,
				ProjectID:   scope.ProjectID,
				PrincipalID: principal,
				Operation:   "event.publish",
				CallerKey:   "caller-1",
			},
			ContentHash: contentHash,
			Result:      []byte(testEventID),
			At:          fixedNow,
		},
		Audit: []event.Audit{{
			ID:        "audit-1",
			Scope:     scope,
			Actor:     principal,
			Operation: "event.publish",
			At:        fixedNow,
		}},
		Authority:  ingressAuthorityReads(t, scope, kind, principal, execution),
		PostCommit: []event.PostCommitEffect{{Kind: event.PostPublishEvent, Target: string(testEventID)}},
	}
}

// ingressAuthorityReads builds exactly the registry checks that apply to the
// source kind: membership always, execution_running only for an execution.
func ingressAuthorityReads(t *testing.T, scope tenancy.Scope, kind event.SourceKind, principal id.PrincipalID, execution id.ExecutionID) []authorize.AuthorityRead {
	t.Helper()
	operation, ok := authorize.LookupOperation("event.publish")
	if !ok {
		t.Fatal("event.publish is not registered")
	}
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		if check == authorize.CheckExecutionRunning && kind != event.SourceExecution {
			continue
		}
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principal,
		}
		if check == authorize.CheckExecutionRunning {
			read.TargetID = string(execution)
		}
		reads = append(reads, read)
	}
	return reads
}

// TestIngressCommitsEventReceiptAuditAtomically asserts an execution source
// commits without any invented membership row: its execution row is the
// principal record. An execution principal holds no membership roles, so the
// membership read is not applicable and adds no lock; the execution_running
// read is what guards its execution state. (The identity AuthorityReader
// reports Allowed=false for that membership read; the commit boundary keeps the
// execution check reachable instead of refusing before it.)
func TestIngressCommitsEventReceiptAuditAtomically(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceExecution, testPrincipal, testExecution)
	result, err := adapter.Ingest(context.Background(), unit)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.Replayed {
		t.Error("first ingest was marked replayed")
	}
	if result.EventID != testEventID || result.ReceiptID != testReceiptID {
		t.Errorf("result = %#v, want the committed ids", result)
	}
	row, err := s.Read(context.Background(), testScope(), "events", string(testEventID))
	if err != nil {
		t.Fatalf("Read event: %v", err)
	}
	if got := row.Values["type"]; got != string(event.TypeCustomObservation) {
		t.Errorf("event type = %v, want custom observation", got)
	}
	if _, found := s.Receipt(unit.Receipt.Key); !found {
		t.Error("receipt was not committed")
	}
	if audits := s.Audit(testScope()); len(audits) != 1 {
		t.Errorf("audit rows = %d, want 1", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want 1", len(events))
	}
}

// TestIngressCommitsServiceSource asserts a service principal is guarded by its
// enabled service row, not by a fabricated membership.
func TestIngressCommitsServiceSource(t *testing.T) {
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceService, testService, "")
	if _, err := adapter.Ingest(context.Background(), unit); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want 1", len(events))
	}
}

// TestIngressCommitsTenantWideHumanSource asserts a project-scoped ingest
// resolves the human's membership UNION: the tenant-wide grant applies together
// with the exact-project row (absent here, so it is pinned as a deliberate
// absence) instead of being shadowed by it.
func TestIngressCommitsTenantWideHumanSource(t *testing.T) {
	s := newStore()
	seedMembership(t, s, tenancy.Scope{TenantID: testTenant}, testMembership, testActor)
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceShell, testActor, "")
	if _, err := adapter.Ingest(context.Background(), unit); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want 1", len(events))
	}
}

// TestIngressReplaysEqualReceiptWithoutSecondWrite asserts an identical retry
// converges on the original identities and writes nothing again.
func TestIngressReplaysEqualReceiptWithoutSecondWrite(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceExecution, testPrincipal, testExecution)
	if _, err := adapter.Ingest(context.Background(), unit); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	result, err := adapter.Ingest(context.Background(), unit)
	if err != nil {
		t.Fatalf("replayed Ingest: %v", err)
	}
	if !result.Replayed {
		t.Error("equal retry was not marked replayed")
	}
	if result.EventID != testEventID || result.ReceiptID != testReceiptID {
		t.Errorf("replayed result = %#v, want the original ids", result)
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d after replay, want 1", len(events))
	}
	if audits := s.Audit(testScope()); len(audits) != 1 {
		t.Errorf("audit rows = %d after replay, want 1", len(audits))
	}
}

// TestIngressRefusesChangedContent asserts a changed retry is refused before any
// write.
func TestIngressRefusesChangedContent(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceExecution, testPrincipal, testExecution)
	if _, err := adapter.Ingest(context.Background(), unit); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	changed := unit
	changed.Receipt.ContentHash, err = receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(`{"observation":2}`)})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	_, err = adapter.Ingest(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d after changed-content refusal, want 1", len(events))
	}
}

// TestIngressStaleExecutionGuardLeavesNoState asserts a changed execution fact
// refuses the commit and persists nothing.
func TestIngressStaleExecutionGuardLeavesNoState(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "succeeded")
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceExecution, testPrincipal, testExecution)
	_, err = adapter.Ingest(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a refused unit committed its receipt")
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after guard refusal, want 0", len(events))
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after guard refusal, want 0", len(audits))
	}
}

// TestIngressStaleMembershipVersionLeavesNoState asserts a membership row that
// moved past the observed version refuses the commit.
func TestIngressStaleMembershipVersionLeavesNoState(t *testing.T) {
	s := newStore()
	seedMembership(t, s, testScope(), testMembership, testActor)
	updateVersion(t, s, testScope(), "memberships", testMembership, 1)
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceShell, testActor, "")
	for index := range unit.Authority {
		unit.Authority[index].ExpectedVersion = 1
	}
	_, err = adapter.Ingest(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a stale-membership unit committed its receipt")
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after stale membership, want 0", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after stale membership, want 0", len(events))
	}
}

// TestIngressRefusesMissingPrincipalAcrossScope asserts an execution principal
// that is not present in the unit scope resolves no row and commits nothing.
func TestIngressRefusesMissingPrincipalAcrossScope(t *testing.T) {
	s := newStore()
	seedExecution(t, s, otherScope(), testExecution, testPrincipal, "running")
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceExecution, testPrincipal, testExecution)
	_, err = adapter.Ingest(context.Background(), unit)
	requireClass(t, err, fault.Forbidden)
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after scope refusal, want 0", len(events))
	}
}

// TestIngressConcurrentEqualRetriesCommitOnce asserts two equal concurrent
// attempts leave exactly one committed event.
func TestIngressConcurrentEqualRetriesCommitOnce(t *testing.T) {
	s := newStore()
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceExecution, testPrincipal, testExecution)
	var wait sync.WaitGroup
	results := make([]event.IngressResult, 2)
	errs := make([]error, 2)
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = adapter.Ingest(context.Background(), unit)
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("attempt %d: %v", index, err)
		}
	}
	if results[0].EventID != results[1].EventID {
		t.Errorf("concurrent attempts committed different events: %q vs %q", results[0].EventID, results[1].EventID)
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want exactly 1", len(events))
	}
}

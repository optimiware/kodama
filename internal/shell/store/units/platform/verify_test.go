package platform_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

func testSubject() verify.Subject {
	return verify.Subject{Fields: []verify.SubjectField{{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"}}}
}

func recordUnit(t *testing.T) verify.RecordUnit {
	t.Helper()
	contentHash, err := receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(`{"verify":1}`)})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	resultBody, err := json.Marshal(verify.RecordResult{ID: testVerification})
	if err != nil {
		t.Fatalf("Marshal result: %v", err)
	}
	return verify.RecordUnit{
		Owner: testScope(),
		Record: verify.Record{
			ID:                testVerification,
			Scope:             testScope(),
			CallingService:    service.ServiceID(testService),
			BindingID:         resource.BindingID(testBinding),
			Actor:             executionActorPrincipal(),
			ExecutionID:       testExecution,
			ActionID:          testAction,
			ResourceID:        resource.ResourceID(testResource),
			Operation:         "git.ref.update",
			Subject:           testSubject(),
			RequestedApproval: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
			Result:            verify.ResultApproved,
			Approved:          true,
			At:                fixedNow,
		},
		Receipt: receipt.Record{
			ID: testReceiptID,
			Key: receipt.Key{
				TenantID:    testTenant,
				ProjectID:   testProject,
				PrincipalID: testPrincipal,
				Operation:   "actor.verify",
				CallerKey:   "caller-1",
			},
			ContentHash: contentHash,
			Result:      resultBody,
			At:          fixedNow,
		},
		Audit: []event.Audit{{
			ID:        "audit-2",
			Scope:     testScope(),
			Actor:     testPrincipal,
			Operation: "actor.verify",
			At:        fixedNow,
		}},
		Events: []event.Envelope{{
			ID:        "event-2",
			Type:      event.TypeCustomObservation,
			Source:    event.Source{Kind: event.SourceExecution, PrincipalID: testPrincipal},
			At:        fixedNow,
			Scope:     testScope(),
			SubjectID: "subject-1",
		}},
		Authority: recordAuthorityReads(t),
	}
}

func recordAuthorityReads(t *testing.T) []authorize.AuthorityRead {
	t.Helper()
	operation, ok := authorize.LookupOperation("actor.verify")
	if !ok {
		t.Fatal("actor.verify is not registered")
	}
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       testScope(),
			PrincipalID: testPrincipal,
		}
		switch check {
		case authorize.CheckServiceEnabled:
			read.PrincipalID = testService
		case authorize.CheckBindingVersion:
			read.TargetID = testBinding
			read.ExpectedVersion = 7
		case authorize.CheckExecutionRunning:
			read.TargetID = string(testExecution)
		case authorize.CheckActionDeadline:
			read.TargetID = string(testAction)
		}
		reads = append(reads, read)
	}
	return reads
}

func seedVerificationFixtures(t *testing.T) *memoryStoreAlias {
	t.Helper()
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	seedAction(t, s, testScope(), testAction, testExecution, "running", "")
	return s
}

// TestRecordCommitsVerificationRowAtomically asserts one record commits the
// verifications row, receipt, audit row and event together.
func TestRecordCommitsVerificationRowAtomically(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	result, err := adapter.Record(context.Background(), unit)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if result.Replayed || string(result.ID) != testVerification {
		t.Errorf("result = %#v, want a fresh verification id", result)
	}
	row, err := s.Read(context.Background(), testScope(), "verifications", testVerification)
	if err != nil {
		t.Fatalf("Read verification: %v", err)
	}
	if got := row.Values["service_id"]; got != string(testService) {
		t.Errorf("service_id = %v, want %q", got, testService)
	}
	if got := row.Values["result"]; got != string(verify.ResultApproved) {
		t.Errorf("result column = %v, want approved", got)
	}
	if got := row.Values["approved"]; got != true {
		t.Errorf("approved column = %v, want true", got)
	}
	if _, present := row.Values["source_event_id"]; present {
		t.Error("verifications row still carries the dropped source_event_id column")
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

// TestRecordReplaysEqualReceiptWithoutSecondRow asserts an identical retry
// returns the original verification id and writes nothing again.
func TestRecordReplaysEqualReceiptWithoutSecondRow(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	if _, err := adapter.Record(context.Background(), unit); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	result, err := adapter.Record(context.Background(), unit)
	if err != nil {
		t.Fatalf("replayed Record: %v", err)
	}
	if !result.Replayed || string(result.ID) != testVerification {
		t.Errorf("replayed result = %#v, want the original id", result)
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d after replay, want 1", len(events))
	}
}

// TestRecordRefusesChangedContent asserts a changed retry is refused before any
// write.
func TestRecordRefusesChangedContent(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	if _, err := adapter.Record(context.Background(), unit); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	changed := unit
	changed.Receipt.ContentHash, err = receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(`{"verify":2}`)})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	_, err = adapter.Record(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d after changed-content refusal, want 1", len(events))
	}
}

// TestRecordStaleBindingGuardLeavesNoState asserts a changed binding version
// refuses the commit and persists nothing.
func TestRecordStaleBindingGuardLeavesNoState(t *testing.T) {
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 6)
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	seedAction(t, s, testScope(), testAction, testExecution, "running", "")
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	_, err = adapter.Record(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err == nil {
		t.Error("a refused record persisted a verification row")
	}
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a refused record committed its receipt")
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after guard refusal, want 0", len(events))
	}
}

// TestRecordRefusesExpiredActionDeadline asserts a deadline that is at or
// before the transaction clock refuses the commit and persists nothing.
func TestRecordRefusesExpiredActionDeadline(t *testing.T) {
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	seedActionWithDeadline(t, s, testScope(), testAction, testExecution, "running", "", fixedNow)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	_, err = adapter.Record(context.Background(), unit)
	requireClass(t, err, fault.DeadlineExpired)
	if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err == nil {
		t.Error("an expired-deadline record persisted a verification row")
	}
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("an expired-deadline record committed its receipt")
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after deadline refusal, want 0", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after deadline refusal, want 0", len(events))
	}
}

// TestRecordAcceptsFutureActionDeadline asserts the same unit commits while the
// action deadline is still strictly after the transaction clock.
func TestRecordAcceptsFutureActionDeadline(t *testing.T) {
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	seedActionWithDeadline(t, s, testScope(), testAction, testExecution, "running", "", fixedNow.Add(time.Minute))
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	if _, err := adapter.Record(context.Background(), recordUnit(t)); err != nil {
		t.Fatalf("Record with a live deadline: %v", err)
	}
}

// TestRecordAcceptsTenantWideService asserts a project-scoped record resolves
// the calling service's UNION: the tenant-wide service row applies together with
// the exact-project row (absent here) instead of being shadowed by it.
func TestRecordAcceptsTenantWideService(t *testing.T) {
	s := newStore()
	seedService(t, s, tenancy.Scope{TenantID: testTenant}, testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	seedAction(t, s, testScope(), testAction, testExecution, "running", "")
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	if _, err := adapter.Record(context.Background(), recordUnit(t)); err != nil {
		t.Fatalf("Record with a tenant-wide service: %v", err)
	}
}

// TestRecordStaleServiceVersionLeavesNoState asserts a service row that moved
// past the observed version refuses the commit.
func TestRecordStaleServiceVersionLeavesNoState(t *testing.T) {
	s := seedVerificationFixtures(t)
	updateVersion(t, s, testScope(), "services", string(testService), 1)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	for index := range unit.Authority {
		if unit.Authority[index].Check == authorize.CheckServiceEnabled {
			unit.Authority[index].ExpectedVersion = 1
		}
	}
	_, err = adapter.Record(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err == nil {
		t.Error("a stale-service record persisted a verification row")
	}
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a stale-service record committed its receipt")
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after stale service, want 0", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after stale service, want 0", len(events))
	}
}

// TestRecordStaleExecutionVersionLeavesNoState asserts an execution row that
// moved past the observed version refuses the commit.
func TestRecordStaleExecutionVersionLeavesNoState(t *testing.T) {
	s := seedVerificationFixtures(t)
	updateVersion(t, s, testScope(), "executions", string(testExecution), 1)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	for index := range unit.Authority {
		if unit.Authority[index].Check == authorize.CheckExecutionRunning {
			unit.Authority[index].ExpectedVersion = 1
		}
	}
	_, err = adapter.Record(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err == nil {
		t.Error("a stale-execution record persisted a verification row")
	}
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a stale-execution record committed its receipt")
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after stale execution, want 0", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after stale execution, want 0", len(events))
	}
}

// TestRecordRefusesCrossScopeCommit asserts a record cannot write outside the
// unit's ownership scope.
func TestRecordRefusesCrossScopeCommit(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	unit.Owner = otherScope()
	unit.Record.Scope = otherScope()
	unit.Record.Actor.Scope = otherScope()
	unit.Record.CallingService = service.ServiceID(testService)
	unit.Receipt.Key.TenantID = otherScope().TenantID
	unit.Receipt.Key.ProjectID = otherScope().ProjectID
	unit.Audit[0].Scope = otherScope()
	unit.Events[0].Scope = otherScope()
	unit.Authority = recordAuthorityReads(t)
	_, err = adapter.Record(context.Background(), unit)
	if err == nil {
		t.Fatal("cross-scope record succeeded")
	}
	if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err == nil {
		t.Error("cross-scope record wrote a verification row")
	}
}

// TestRecordConcurrentEqualRetriesCommitOnce asserts two equal concurrent
// attempts leave exactly one verification row.
func TestRecordConcurrentEqualRetriesCommitOnce(t *testing.T) {
	s := seedVerificationFixtures(t)
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	var wait sync.WaitGroup
	errs := make([]error, 2)
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, errs[index] = adapter.Record(context.Background(), unit)
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("attempt %d: %v", index, err)
		}
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want exactly 1", len(events))
	}
}

func reportUnit(t *testing.T) verify.ReportUnit {
	t.Helper()
	contentHash, err := receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(`{"report":1}`)})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	resultBody, err := json.Marshal(verify.ReportResult{VerificationID: testVerification, EventID: "event-9"})
	if err != nil {
		t.Fatalf("Marshal result: %v", err)
	}
	return verify.ReportUnit{
		Owner:            testScope(),
		ReportingService: service.ServiceID(testService),
		BindingID:        resource.BindingID(testBinding),
		SourceEventID:    "source-event-1",
		EventType:        event.ReportType("git.ref.updated"),
		VerificationID:   testVerification,
		ResourceID:       resource.ResourceID(testResource),
		Operation:        "git.ref.update",
		Subject:          testSubject(),
		Receipt: receipt.Record{
			ID: "receipt-2",
			Key: receipt.Key{
				TenantID:    testTenant,
				ProjectID:   testProject,
				PrincipalID: testService,
				Operation:   "resource.report",
				CallerKey:   "source-event-1",
			},
			ContentHash: contentHash,
			Result:      resultBody,
			At:          fixedNow,
		},
		Audit: []event.Audit{{
			ID:        "audit-3",
			Scope:     testScope(),
			Actor:     testService,
			Operation: "resource.report",
			At:        fixedNow,
		}},
		Events: []event.Envelope{{
			ID:             "event-9",
			Type:           event.TypeVerifiedReport,
			ReportType:     event.ReportType("git.ref.updated"),
			Source:         event.Source{Kind: event.SourceService, PrincipalID: testService},
			At:             fixedNow,
			Scope:          testScope(),
			SubjectID:      testResource,
			SubjectVersion: 7,
			CorrelationID:  string(testExecution),
			CausationID:    testVerification,
		}},
		Authority:  reportAuthorityReads(t),
		PostCommit: []event.PostCommitEffect{{Kind: event.PostPublishEvent}},
	}
}

func reportAuthorityReads(t *testing.T) []authorize.AuthorityRead {
	t.Helper()
	operation, ok := authorize.LookupOperation("resource.report")
	if !ok {
		t.Fatal("resource.report is not registered")
	}
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       testScope(),
			PrincipalID: testService,
		}
		switch check {
		case authorize.CheckBindingVersion:
			read.TargetID = testBinding
			read.ExpectedVersion = 7
		case authorize.CheckResourceOwnership:
			read.TargetID = testResource
		}
		reads = append(reads, read)
	}
	return reads
}

func seedReportFixtures(t *testing.T) *memoryStoreAlias {
	t.Helper()
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	seedVerification(t, s, testScope(), testVerification)
	mustInsert(t, s, testScope(), "resources", testResource, map[string]any{
		"kind":       "git",
		"version":    uint64(1),
		"metadata":   []byte(`{}`),
		"created_at": fixedNow,
	})
	return s
}

// TestReportCommitsReceiptAuditAndEventAtomically asserts one report guards the
// cited verification and commits the deduplication receipt, audit and accepted
// event together.
func TestReportCommitsReceiptAuditAndEventAtomically(t *testing.T) {
	s := seedReportFixtures(t)
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	result, err := adapter.Report(context.Background(), unit)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if result.Replayed || string(result.VerificationID) != testVerification || string(result.EventID) != "event-9" {
		t.Errorf("result = %#v, want the committed report result", result)
	}
	if _, found := s.Receipt(unit.Receipt.Key); !found {
		t.Error("report deduplication receipt was not committed")
	}
	if audits := s.Audit(testScope()); len(audits) != 1 {
		t.Errorf("audit rows = %d, want 1", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want 1", len(events))
	}
}

// TestReportReplaysEqualReceiptWithoutSecondEvent asserts an identical retry
// converges on the original result and writes nothing again.
func TestReportReplaysEqualReceiptWithoutSecondEvent(t *testing.T) {
	s := seedReportFixtures(t)
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	if _, err := adapter.Report(context.Background(), unit); err != nil {
		t.Fatalf("first Report: %v", err)
	}
	result, err := adapter.Report(context.Background(), unit)
	if err != nil {
		t.Fatalf("replayed Report: %v", err)
	}
	if !result.Replayed || string(result.EventID) != "event-9" {
		t.Errorf("replayed result = %#v, want the original event", result)
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d after replay, want 1", len(events))
	}
}

// TestReportRefusesChangedContent asserts a changed retry is refused before any
// write.
func TestReportRefusesChangedContent(t *testing.T) {
	s := seedReportFixtures(t)
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	if _, err := adapter.Report(context.Background(), unit); err != nil {
		t.Fatalf("first Report: %v", err)
	}
	changed := unit
	changed.Receipt.ContentHash, err = receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(`{"report":2}`)})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	_, err = adapter.Report(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d after changed-content refusal, want 1", len(events))
	}
}

// TestReportRefusesInconsistentUnitFacts asserts a missing or mismatched
// receipt key and a mismatched accepted-event source/causation refuse before
// any state is written.
func TestReportRefusesInconsistentUnitFacts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(unit *verify.ReportUnit)
	}{
		{name: "missing receipt", mutate: func(unit *verify.ReportUnit) { unit.Receipt = receipt.Record{} }},
		{name: "wrong receipt principal", mutate: func(unit *verify.ReportUnit) {
			unit.Receipt.Key.PrincipalID = testActor
		}},
		{name: "wrong receipt caller key", mutate: func(unit *verify.ReportUnit) {
			unit.Receipt.Key.CallerKey = "other-source-event"
		}},
		{name: "wrong receipt operation", mutate: func(unit *verify.ReportUnit) {
			unit.Receipt.Key.Operation = "actor.verify"
		}},
		{name: "wrong event source", mutate: func(unit *verify.ReportUnit) {
			unit.Events[0].Source.PrincipalID = testActor
		}},
		{name: "wrong event causation", mutate: func(unit *verify.ReportUnit) {
			unit.Events[0].CausationID = "verification-other"
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := seedReportFixtures(t)
			adapter, err := platform.NewReportAdapter(s, s)
			if err != nil {
				t.Fatalf("NewReportAdapter: %v", err)
			}
			unit := reportUnit(t)
			testCase.mutate(&unit)
			_, err = adapter.Report(context.Background(), unit)
			requireClass(t, err, fault.InvalidInput)
			if _, found := s.Receipt(unit.Receipt.Key); found {
				t.Error("a refused report committed a receipt")
			}
			if audits := s.Audit(testScope()); len(audits) != 0 {
				t.Errorf("audit rows = %d after refusal, want 0", len(audits))
			}
			if events := s.Events(testScope()); len(events) != 0 {
				t.Errorf("events = %d after refusal, want 0", len(events))
			}
		})
	}
}

// TestReportDeduplicatesByServiceAndSourceEvent asserts the deduplication
// identity is (reporting service, source event id): different verification
// content under the same identity is refused, equal content replays the
// original.
func TestReportDeduplicatesByServiceAndSourceEvent(t *testing.T) {
	s := seedReportFixtures(t)
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	first, err := adapter.Report(context.Background(), unit)
	if err != nil {
		t.Fatalf("first Report: %v", err)
	}
	changed := unit
	changed.Receipt.ContentHash, err = receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(`{"report":"different-verification"}`)})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	_, err = adapter.Report(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
	replayed, err := adapter.Report(context.Background(), unit)
	if err != nil {
		t.Fatalf("equal retry: %v", err)
	}
	if !replayed.Replayed || replayed.EventID != first.EventID {
		t.Errorf("equal retry = %#v, want replay of %#v", replayed, first)
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want exactly 1", len(events))
	}
}

// TestReportAcceptsTenantWideService asserts a project-scoped report resolves
// the reporting service's UNION: the tenant-wide service row applies together
// with the exact-project row (absent here) instead of being shadowed by it.
func TestReportAcceptsTenantWideService(t *testing.T) {
	s := newStore()
	seedService(t, s, tenancy.Scope{TenantID: testTenant}, testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	seedVerification(t, s, testScope(), testVerification)
	mustInsert(t, s, testScope(), "resources", testResource, map[string]any{
		"kind": "git", "version": uint64(1), "metadata": []byte(`{}`), "created_at": fixedNow,
	})
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	if _, err := adapter.Report(context.Background(), reportUnit(t)); err != nil {
		t.Fatalf("Report with a tenant-wide service: %v", err)
	}
}

// TestReportStaleServiceVersionLeavesNoState asserts a service row that moved
// past the observed version refuses the commit.
func TestReportStaleServiceVersionLeavesNoState(t *testing.T) {
	s := seedReportFixtures(t)
	updateVersion(t, s, testScope(), "services", string(testService), 1)
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	for index := range unit.Authority {
		if unit.Authority[index].Check == authorize.CheckServiceEnabled {
			unit.Authority[index].ExpectedVersion = 1
		}
	}
	_, err = adapter.Report(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a stale-service report committed its receipt")
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after stale service, want 0", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after stale service, want 0", len(events))
	}
}

// TestReportStaleBindingGuardLeavesNoState asserts a changed binding version
// refuses the commit and persists no receipt, audit or event.
func TestReportStaleBindingGuardLeavesNoState(t *testing.T) {
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 6)
	seedVerification(t, s, testScope(), testVerification)
	mustInsert(t, s, testScope(), "resources", testResource, map[string]any{
		"kind": "git", "version": uint64(1), "metadata": []byte(`{}`), "created_at": fixedNow,
	})
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	_, err = adapter.Report(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a refused report committed its receipt")
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after guard refusal, want 0", len(events))
	}
}

// TestReportRefusesMissingVerification asserts a report cannot cite a
// verification row that is absent from its scope.
func TestReportRefusesMissingVerification(t *testing.T) {
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	mustInsert(t, s, testScope(), "resources", testResource, map[string]any{
		"kind": "git", "version": uint64(1), "metadata": []byte(`{}`), "created_at": fixedNow,
	})
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	_, err = adapter.Report(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after missing-verification refusal, want 0", len(events))
	}
}

// TestReportConcurrentEqualRetriesCommitOnce asserts two equal concurrent
// reports leave exactly one accepted event.
func TestReportConcurrentEqualRetriesCommitOnce(t *testing.T) {
	s := seedReportFixtures(t)
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	var wait sync.WaitGroup
	errs := make([]error, 2)
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, errs[index] = adapter.Report(context.Background(), unit)
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("attempt %d: %v", index, err)
		}
	}
	if events := s.Events(testScope()); len(events) != 1 {
		t.Errorf("events = %d, want exactly 1", len(events))
	}
}

// TestEvidenceReaderReturnsPriorSucceededRequests asserts the reader returns the
// durable pass facts of one execution and ignores other executions and states.
func TestEvidenceReaderReturnsPriorSucceededRequests(t *testing.T) {
	s := newStore()
	seedAction(t, s, testScope(), "action-1", testExecution, "succeeded", "pass")
	seedActionAt(t, s, testScope(), "action-2", testExecution, 1, "approve-2", "succeeded", "pass", fixedNow.Add(time.Hour))
	seedActionAt(t, s, testScope(), "action-3", testExecution, 2, "approve-3", "failed", "fail", fixedNow.Add(time.Hour))
	seedAction(t, s, testScope(), "action-4", "exec-2", "succeeded", "pass")

	reader, err := platform.NewEvidenceReader(s)
	if err != nil {
		t.Fatalf("NewEvidenceReader: %v", err)
	}
	evidence, err := reader.ReadEvidence(context.Background(), verify.EvidenceQuery{
		Scope:       testScope(),
		ExecutionID: testExecution,
		Assignment:  request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		Operation:   "git.ref.update",
		ResourceID:  resource.ResourceID(testResource),
		Subject:     testSubject(),
	})
	if err != nil {
		t.Fatalf("ReadEvidence: %v", err)
	}
	if len(evidence) != 2 {
		t.Fatalf("evidence = %d, want 2", len(evidence))
	}
	if string(evidence[0].ActionID) != "action-1" || string(evidence[1].ActionID) != "action-2" {
		t.Errorf("evidence actions = %q, %q; want action-1, action-2", evidence[0].ActionID, evidence[1].ActionID)
	}
	for _, fact := range evidence {
		if fact.ExecutionID != testExecution {
			t.Errorf("evidence execution = %q", fact.ExecutionID)
		}
		if fact.Decision != request.DecisionPass {
			t.Errorf("evidence decision = %q, want pass", fact.Decision)
		}
		if fact.Assignment.Role != tenancy.RoleReviewer {
			t.Errorf("evidence assignment = %#v", fact.Assignment)
		}
		// The stored request subject carries the approval's operation and
		// resource alongside the effect field; the reader derives them from the
		// stored literals, never echoes the query back.
		if fact.Operation != "git.ref.update" {
			t.Errorf("evidence operation = %q, want the stored git.ref.update", fact.Operation)
		}
		if fact.ResourceID != resource.ResourceID(testResource) {
			t.Errorf("evidence resource = %q, want the stored %q", fact.ResourceID, testResource)
		}
		// The stored request subject carries the approval's operation and
		// resource alongside the effect field; the reader preserves them in the
		// matched subject instead of stripping them, so the enforcing service's
		// canonical fields can match exactly.
		if len(fact.Subject.Fields) != 3 {
			t.Fatalf("evidence subject fields = %#v, want operation, resource and ref", fact.Subject.Fields)
		}
		operation, found := fact.Subject.Field("operation")
		if !found || operation.Kind != verify.KindString || operation.Value != "git.ref.update" {
			t.Errorf("evidence operation field = %#v, found %t; want operation/string/git.ref.update", operation, found)
		}
		resourceField, found := fact.Subject.Field("resource")
		if !found || resourceField.Kind != verify.KindString || resourceField.Value != testResource {
			t.Errorf("evidence resource field = %#v, found %t; want resource/string/%s", resourceField, found, testResource)
		}
		ref, found := fact.Subject.Field("ref")
		if !found || ref.Kind != verify.KindString || ref.Value != "refs/heads/main" {
			t.Errorf("evidence ref field = %#v, found %t; want ref/string/refs-heads-main", ref, found)
		}
		if !fact.ResolvedAt.Equal(fixedNow) {
			t.Errorf("evidence resolved_at = %v", fact.ResolvedAt)
		}
	}
}

// TestEvidenceReaderRefusesMalformedStoredSubject asserts a fields-list subject
// shape and an unresolved reference are refused, never decoded to an empty
// subject.
func TestEvidenceReaderRefusesMalformedStoredSubject(t *testing.T) {
	cases := []struct {
		name    string
		subject []byte
	}{
		{name: "fields list shape", subject: []byte(`{"fields":[{"name":"ref","kind":"string","value":"refs/heads/main"}]}`)},
		{name: "unresolved reference", subject: storedReferencedRequestSubject()},
		{name: "unknown field shape", subject: []byte(`{"ref":{"kind":"literal","literal":"x","extra":1}}`)},
		{name: "empty subject", subject: []byte(`{}`)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			seedActionSubject(t, s, testScope(), "action-1", testExecution, testCase.subject)
			reader, err := platform.NewEvidenceReader(s)
			if err != nil {
				t.Fatalf("NewEvidenceReader: %v", err)
			}
			_, err = reader.ReadEvidence(context.Background(), verify.EvidenceQuery{
				Scope:       testScope(),
				ExecutionID: testExecution,
				Assignment:  request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
				Operation:   "git.ref.update",
				ResourceID:  resource.ResourceID(testResource),
				Subject:     testSubject(),
			})
			requireClass(t, err, fault.InternalFailure)
		})
	}
}

// TestEvidenceReaderIsScopeIsolated asserts another scope sees no evidence.
func TestEvidenceReaderIsScopeIsolated(t *testing.T) {
	s := newStore()
	seedAction(t, s, testScope(), "action-1", testExecution, "succeeded", "pass")
	reader, err := platform.NewEvidenceReader(s)
	if err != nil {
		t.Fatalf("NewEvidenceReader: %v", err)
	}
	evidence, err := reader.ReadEvidence(context.Background(), verify.EvidenceQuery{
		Scope:       otherScope(),
		ExecutionID: testExecution,
		Assignment:  request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		Operation:   "git.ref.update",
		ResourceID:  resource.ResourceID(testResource),
		Subject:     testSubject(),
	})
	if err != nil {
		t.Fatalf("ReadEvidence: %v", err)
	}
	if len(evidence) != 0 {
		t.Errorf("cross-scope evidence = %d, want 0", len(evidence))
	}
}

package readers_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// reportIDs allocates the reporter's durable event and receipt identities.
type reportIDs struct {
	events   int
	receipts int
}

func (ids *reportIDs) NewEventID() (id.EventID, error) {
	ids.events++
	return id.EventID("report-event-" + strconv.Itoa(ids.events)), nil
}

func (ids *reportIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.receipts++
	return id.ReceiptID("report-receipt-" + strconv.Itoa(ids.receipts)), nil
}

// seedResource writes one resources row so a report's ownership guard resolves.
func seedResource(t *testing.T, backend *memory.Store, scope tenancy.Scope, identity string) {
	t.Helper()
	metadata, err := jsonMarshal(map[string]string{})
	if err != nil {
		t.Fatalf("encode resource metadata: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "resources",
			ID:     identity,
			Insert: true,
			Values: map[string]any{
				"kind":       "git",
				"version":    int64(1),
				"metadata":   metadata,
				"created_at": testNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed resource: %v", err)
	}
}

// seedReportBinding writes one binding declaring the reported operation and
// event type.
func seedReportBinding(t *testing.T, backend *memory.Store, scope tenancy.Scope, bindingID string, resourceID resource.ResourceID, serviceID service.ServiceID, version uint64) {
	t.Helper()
	operations, err := jsonMarshal([]string{string(resource.OperationGitRefUpdate)})
	if err != nil {
		t.Fatalf("encode operations: %v", err)
	}
	origins, err := jsonMarshal([]string{})
	if err != nil {
		t.Fatalf("encode origins: %v", err)
	}
	reportable, err := jsonMarshal([]string{"git.ref.updated"})
	if err != nil {
		t.Fatalf("encode reportable events: %v", err)
	}
	metadata, err := jsonMarshal(map[string]string{})
	if err != nil {
		t.Fatalf("encode metadata: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "resource_bindings",
			ID:     bindingID,
			Insert: true,
			Values: map[string]any{
				"resource_id":       string(resourceID),
				"service_id":        string(serviceID),
				"origins":           origins,
				"operations":        operations,
				"reportable_events": reportable,
				"metadata":          metadata,
				"version":           int64(version),
				"created_at":        testNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed report binding: %v", err)
	}
}

// verificationSubject is the typed effect subject shared by the verifier and
// reporter.
func verificationSubject() verify.Subject {
	return verify.Subject{Fields: []verify.SubjectField{
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
	}}
}

// TestVerificationReaderRoundTripsTheRecordAdapter writes one full record
// through the platform adapter and reads every persisted field back.
func TestVerificationReaderRoundTripsTheRecordAdapter(t *testing.T) {
	backend := newStore()
	scope := testScope()
	records, err := platform.NewRecordAdapter(backend, backend)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	reader, err := readers.NewVerificationReader(backend)
	if err != nil {
		t.Fatalf("NewVerificationReader: %v", err)
	}
	record := verify.Record{
		ID:             "verification-roundtrip",
		Scope:          scope,
		CallingService: "service-1",
		BindingID:      "binding-1",
		Actor: identity.Principal{
			ID:          "execution-principal-1",
			Kind:        identity.KindExecution,
			Scope:       scope,
			Enabled:     true,
			Version:     3,
			ExecutionID: "execution-1",
		},
		ExecutionID:       "execution-1",
		ActionID:          "action-1",
		ResourceID:        "resource-1",
		Operation:         string(resource.OperationGitRefUpdate),
		Subject:           verificationSubject(),
		RequestedApproval: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		Result:            verify.ResultApproved,
		Approved:          true,
		At:                testNow,
	}
	if _, err := records.Record(context.Background(), verify.RecordUnit{Owner: scope, Record: record}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	read, err := reader.ReadVerification(context.Background(), scope, record.ID)
	if err != nil {
		t.Fatalf("ReadVerification: %v", err)
	}
	if read.ID != record.ID || read.Scope != scope || read.CallingService != record.CallingService {
		t.Fatalf("identity fields = %+v; want %+v", read, record)
	}
	if read.BindingID != record.BindingID || read.ResourceID != record.ResourceID || read.Operation != record.Operation {
		t.Fatalf("linkage fields = %+v; want %+v", read, record)
	}
	if read.Actor.ID != record.Actor.ID || read.Actor.Scope != scope || read.Actor.Kind != identity.KindExecution || read.Actor.ExecutionID != record.ExecutionID {
		t.Fatalf("actor = %+v; want the recorded execution actor", read.Actor)
	}
	if read.ExecutionID != record.ExecutionID || read.ActionID != record.ActionID {
		t.Fatalf("causation ids = %q/%q; want %q/%q", read.ExecutionID, read.ActionID, record.ExecutionID, record.ActionID)
	}
	if read.Result != verify.ResultApproved || !read.Approved {
		t.Fatalf("result = %q approved=%v; want approved/true", read.Result, read.Approved)
	}
	if read.RequestedApproval != record.RequestedApproval {
		t.Fatalf("requested approval = %+v; want %+v", read.RequestedApproval, record.RequestedApproval)
	}
	if len(read.Subject.Fields) != 1 || read.Subject.Fields[0] != record.Subject.Fields[0] {
		t.Fatalf("subject = %+v; want %+v", read.Subject, record.Subject)
	}
	if !read.At.Equal(testNow) {
		t.Fatalf("at = %v; want %v", read.At, testNow)
	}

	// Mutating the returned value must not alter the durable row.
	read.Actor.ID = "attacker"
	read.Subject.Fields[0].Value = "refs/heads/evil"
	read.RequestedApproval.Role = tenancy.RoleAdmin
	again, err := reader.ReadVerification(context.Background(), scope, record.ID)
	if err != nil {
		t.Fatalf("second ReadVerification: %v", err)
	}
	if again.Actor.ID != record.Actor.ID || again.Subject.Fields[0].Value != "refs/heads/main" || again.RequestedApproval.Role != tenancy.RoleReviewer {
		t.Fatalf("store changed after mutating the returned record: %+v", again)
	}
}

// TestVerificationReaderScopeIsolationAndAbsence proves a record outside the
// read scope or an unknown id is uniformly not_found.
func TestVerificationReaderScopeIsolationAndAbsence(t *testing.T) {
	backend := newStore()
	scope := testScope()
	records, err := platform.NewRecordAdapter(backend, backend)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	reader, err := readers.NewVerificationReader(backend)
	if err != nil {
		t.Fatalf("NewVerificationReader: %v", err)
	}
	record := verify.Record{
		ID:             "verification-scoped",
		Scope:          scope,
		CallingService: "service-1",
		BindingID:      "binding-1",
		Actor:          identity.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: scope, Enabled: true},
		ResourceID:     "resource-1",
		Operation:      string(resource.OperationGitRefUpdate),
		Subject:        verificationSubject(),
		Result:         verify.ResultApproved,
		At:             testNow,
	}
	if _, err := records.Record(context.Background(), verify.RecordUnit{Owner: scope, Record: record}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := reader.ReadVerification(context.Background(), otherScope(), record.ID); !isClass(err, fault.NotFound) {
		t.Fatalf("cross-scope read error = %v; want not_found", err)
	}
	if _, err := reader.ReadVerification(context.Background(), scope, "verification-missing"); !isClass(err, fault.NotFound) {
		t.Fatalf("unknown read error = %v; want not_found", err)
	}
}

// TestRealReporterCommitsThroughVerificationReader drives the real verifier and
// the real reporter end to end: the verifier commits a verification, then the
// reporter reads it through the new reader and commits the report.
func TestRealReporterCommitsThroughVerificationReader(t *testing.T) {
	backend := newStore()
	scope := testScope()
	identityAdapter, err := identitystore.New(identitystore.Config{Executor: backend, Scanner: backend, Clock: serviceClock{}})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	seedService(t, backend, scope, "service-1", 1, []tenancy.Role{tenancy.RoleEngineer}, nil)
	seedMembership(t, backend, scope, "membership-actor", testPrincipal, 1, tenancy.RoleEngineer)
	seedReportBinding(t, backend, scope, "binding-1", "resource-1", "service-1", 7)
	seedResource(t, backend, scope, "resource-1")

	bindings, err := readers.NewBindingReader(backend)
	if err != nil {
		t.Fatalf("NewBindingReader: %v", err)
	}
	verifications, err := readers.NewVerificationReader(backend)
	if err != nil {
		t.Fatalf("NewVerificationReader: %v", err)
	}
	evidence, err := platform.NewEvidenceReader(backend)
	if err != nil {
		t.Fatalf("NewEvidenceReader: %v", err)
	}
	records, err := platform.NewRecordAdapter(backend, backend)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	reports, err := platform.NewReportAdapter(backend, backend)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	actor := identity.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1}
	verificationIDs := 0
	verifier, err := verify.New(verify.Config{
		Principals: currentPrincipal{principal: actor},
		Bindings:   bindings,
		Evidence:   evidence,
		Records:    records,
		Clock:      serviceClock{},
		NewID: func() (verify.VerificationID, error) {
			verificationIDs++
			return verify.VerificationID("verification-" + strconv.Itoa(verificationIDs)), nil
		},
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	servicePrincipal := identity.Principal{ID: "service-1", Kind: identity.KindService, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1}
	record, err := verifier.Verify(context.Background(), verify.Attempt{
		Scope:      scope,
		Service:    servicePrincipal,
		Credential: "presented-actor-credential",
		ResourceID: "resource-1",
		Operation:  resource.OperationGitRefUpdate,
		Subject:    verificationSubject(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	reporter, err := verify.NewReporter(verify.ReportConfig{
		Authorizer:    allowAuthorizer{},
		Authority:     identityAdapter,
		Bindings:      bindings,
		Verifications: verifications,
		Reports:       reports,
		Clock:         serviceClock{},
		IDs:           &reportIDs{},
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	command := verify.ReportCommand{
		Scope:          scope,
		Service:        servicePrincipal,
		SourceEventID:  "source-event-1",
		EventType:      event.ReportType("git.ref.updated"),
		VerificationID: record.ID,
		ResourceID:     "resource-1",
		Operation:      resource.OperationGitRefUpdate,
		Subject:        verificationSubject(),
		Data:           []byte(`{"ref":"refs/heads/main"}`),
	}
	committed, err := reporter.Report(context.Background(), command)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if committed.VerificationID != record.ID || committed.EventID == "" || committed.Replayed {
		t.Fatalf("report result = %+v; want a fresh report for %q", committed, record.ID)
	}
	if _, ok := backend.Snapshot(scope, "events", string(committed.EventID)); !ok {
		t.Fatal("accepted report event was not committed")
	}

	// An equal retry replays the original event without a second commit.
	replayed, err := reporter.Report(context.Background(), command)
	if err != nil {
		t.Fatalf("replayed Report: %v", err)
	}
	if !replayed.Replayed || replayed.EventID != committed.EventID {
		t.Fatalf("replayed report = %+v; want the original event %q", replayed, committed.EventID)
	}
	page, err := backend.Query(context.Background(), scope, store.ScanQuery{Table: "events", Limit: store.MaxQueryLimit})
	if err != nil {
		t.Fatalf("scan events: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("committed events = %d; an equal report retry must not publish again", len(page.Rows))
	}
}

// TestReporterRefusesAnotherScopeRecord proves the reporter cannot report from
// a verification outside its own scope.
func TestReporterRefusesAnotherScopeRecord(t *testing.T) {
	backend := newStore()
	scope := testScope()
	identityAdapter, err := identitystore.New(identitystore.Config{Executor: backend, Scanner: backend, Clock: serviceClock{}})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	seedService(t, backend, scope, "service-1", 1, []tenancy.Role{tenancy.RoleEngineer}, nil)
	seedReportBinding(t, backend, scope, "binding-1", "resource-1", "service-1", 7)
	seedResource(t, backend, scope, "resource-1")
	bindings, err := readers.NewBindingReader(backend)
	if err != nil {
		t.Fatalf("NewBindingReader: %v", err)
	}
	verifications, err := readers.NewVerificationReader(backend)
	if err != nil {
		t.Fatalf("NewVerificationReader: %v", err)
	}
	reports, err := platform.NewReportAdapter(backend, backend)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	records, err := platform.NewRecordAdapter(backend, backend)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	foreign := verify.Record{
		ID:             "verification-foreign",
		Scope:          otherScope(),
		CallingService: "service-1",
		BindingID:      "binding-1",
		Actor:          identity.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: otherScope(), Enabled: true},
		ResourceID:     "resource-1",
		Operation:      string(resource.OperationGitRefUpdate),
		Subject:        verificationSubject(),
		Result:         verify.ResultApproved,
		At:             testNow,
	}
	if _, err := records.Record(context.Background(), verify.RecordUnit{Owner: otherScope(), Record: foreign}); err != nil {
		t.Fatalf("Record foreign: %v", err)
	}
	reporter, err := verify.NewReporter(verify.ReportConfig{
		Authorizer:    allowAuthorizer{},
		Authority:     identityAdapter,
		Bindings:      bindings,
		Verifications: verifications,
		Reports:       reports,
		Clock:         serviceClock{},
		IDs:           &reportIDs{},
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	servicePrincipal := identity.Principal{ID: "service-1", Kind: identity.KindService, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1}
	if _, err := reporter.Report(context.Background(), verify.ReportCommand{
		Scope:          scope,
		Service:        servicePrincipal,
		SourceEventID:  "source-event-foreign",
		EventType:      event.ReportType("git.ref.updated"),
		VerificationID: foreign.ID,
		ResourceID:     "resource-1",
		Operation:      resource.OperationGitRefUpdate,
		Subject:        verificationSubject(),
	}); !isClass(err, fault.NotFound) {
		t.Fatalf("foreign verification report error = %v; want not_found", err)
	}
}

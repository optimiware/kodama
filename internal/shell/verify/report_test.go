package verify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// Verified-report implementation surface used by these tests:
//
//	type verify.VerificationReader interface {
//	    ReadVerification(context.Context, tenancy.Scope, verify.VerificationID) (verify.Record, error)
//	}
//	type verify.ReportIDSource interface {
//	    NewEventID() (id.EventID, error)
//	    NewReceiptID() (id.ReceiptID, error)
//	}
//	type verify.ReportConfig struct {
//	    Authorizer    authorize.Authorizer
//	    Authority     authorize.AuthorityReader
//	    Bindings      verify.BindingReader
//	    Verifications verify.VerificationReader
//	    Reports       verify.ReportPort
//	    Clock         verify.Clock
//	    IDs           verify.ReportIDSource
//	}
//	func verify.NewReporter(verify.ReportConfig) (*verify.Reporter, error)
//	type verify.ReportCommand struct {
//	    Scope          tenancy.Scope
//	    Service        identity.Principal
//	    SourceEventID  string
//	    EventType      event.ReportType
//	    VerificationID verify.VerificationID
//	    ResourceID     resource.ResourceID
//	    Operation      resource.Operation
//	    Subject        verify.Subject
//	    Data           []byte
//	}
//	func (*verify.Reporter) Report(context.Context, verify.ReportCommand) (verify.ReportResult, error)

const reportSourceEventID = "source-event-1"

// verificationRecord is the approved verification fact an enforcing service
// cites when it reports an accepted effect.
func verificationRecord() verify.Record {
	return verify.Record{
		ID:             "verification-1",
		Scope:          testScope(),
		CallingService: testServiceID,
		BindingID:      testBindingID,
		Actor:          actorPrincipal(),
		ExecutionID:    testExecution,
		ActionID:       testAction,
		ResourceID:     testResourceID,
		Operation:      string(resource.OperationGitRefUpdate),
		Subject:        requiredSubject(),
		Result:         verify.ResultApproved,
		Approved:       true,
		At:             fixedNow.Add(-1),
	}
}

func reportCommand() verify.ReportCommand {
	return verify.ReportCommand{
		Scope:          testScope(),
		Service:        callingService(),
		SourceEventID:  reportSourceEventID,
		EventType:      event.ReportType("git.ref.updated"),
		VerificationID: "verification-1",
		ResourceID:     testResourceID,
		Operation:      resource.OperationGitRefUpdate,
		Subject:        requiredSubject(),
	}
}

// gitReportSubject is the exact typed subject the Git service verifies and
// reports for one ref update (KODAMA.md, Git access rules).
func gitReportSubject() verify.Subject {
	return verify.Subject{Fields: []verify.SubjectField{
		{Name: "operation", Kind: verify.KindString, Value: string(resource.OperationGitRefUpdate)},
		{Name: "resource", Kind: verify.KindString, Value: string(testResourceID)},
		{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
		{Name: "expected_base", Kind: verify.KindString, Value: "sha-base"},
		{Name: "candidate", Kind: verify.KindString, Value: "sha-candidate"},
	}}
}

// fakeAuthorizer is a deterministic authorize.Authorizer.
type fakeAuthorizer struct {
	mu       sync.Mutex
	decision authorize.Decision
	err      error
	calls    []authorizeCall
}

type authorizeCall struct {
	principal identity.Principal
	operation authorize.OperationName
	scope     tenancy.Scope
}

func newFakeAuthorizer() *fakeAuthorizer {
	return &fakeAuthorizer{decision: authorize.DecisionAllow}
}

func (fake *fakeAuthorizer) setDecision(decision authorize.Decision, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.decision = decision
	fake.err = err
}

func (fake *fakeAuthorizer) Authorize(_ context.Context, principal authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Decision, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, authorizeCall{principal: principal, operation: operation, scope: scope})
	if fake.err != nil {
		return "", fake.err
	}
	return fake.decision, nil
}

func (fake *fakeAuthorizer) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

func (fake *fakeAuthorizer) callAt(index int) authorizeCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls[index]
}

// fakeAuthority is a deterministic authorize.AuthorityReader.
type fakeAuthority struct {
	mu        sync.Mutex
	deny      map[authorize.AuthorityCheck]bool
	overrides map[authorize.AuthorityCheck]uint64
	err       error
	calls     [][]authorize.AuthorityRead
}

func newFakeAuthority() *fakeAuthority {
	return &fakeAuthority{
		deny:      make(map[authorize.AuthorityCheck]bool),
		overrides: make(map[authorize.AuthorityCheck]uint64),
	}
}

func (fake *fakeAuthority) denyCheck(check authorize.AuthorityCheck) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.deny[check] = true
}

func (fake *fakeAuthority) overrideVersion(check authorize.AuthorityCheck, version uint64) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.overrides[check] = version
}

func (fake *fakeAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, append([]authorize.AuthorityRead(nil), reads...))
	if fake.err != nil {
		return nil, fake.err
	}
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		version := read.ExpectedVersion
		if override, ok := fake.overrides[read.Check]; ok {
			version = override
		}
		facts[index] = authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     version,
			Allowed:     !fake.deny[read.Check],
		}
	}
	return facts, nil
}

func (fake *fakeAuthority) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

// fakeVerifications is a deterministic VerificationReader.
type fakeVerifications struct {
	mu     sync.Mutex
	record verify.Record
	err    error
	ids    []verify.VerificationID
	scopes []tenancy.Scope
}

func (fake *fakeVerifications) set(record verify.Record, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record = record
	fake.err = err
}

func (fake *fakeVerifications) ReadVerification(_ context.Context, scope tenancy.Scope, verificationID verify.VerificationID) (verify.Record, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.ids = append(fake.ids, verificationID)
	fake.scopes = append(fake.scopes, scope)
	if fake.err != nil {
		return verify.Record{}, fake.err
	}
	return fake.record, nil
}

func (fake *fakeVerifications) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.ids)
}

func (fake *fakeVerifications) lastID() verify.VerificationID {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.ids) == 0 {
		return ""
	}
	return fake.ids[len(fake.ids)-1]
}

func (fake *fakeVerifications) lastScope() tenancy.Scope {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.scopes) == 0 {
		return tenancy.Scope{}
	}
	return fake.scopes[len(fake.scopes)-1]
}

// fakeReports is a deterministic ReportPort with the receipt-first replay
// semantics the adapter must provide: equal normalized content returns the
// original result, changed content is refused, and the store is untouched by a
// failed commit.
type fakeReports struct {
	mu       sync.Mutex
	commits  int
	receipts map[receipt.Key]*receipt.Record
	results  map[receipt.Key]verify.ReportResult
	units    []verify.ReportUnit
	failNext error
}

func newFakeReports() *fakeReports {
	return &fakeReports{
		receipts: make(map[receipt.Key]*receipt.Record),
		results:  make(map[receipt.Key]verify.ReportResult),
	}
}

func (fake *fakeReports) setFailNext(err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.failNext = err
}

func (fake *fakeReports) Report(_ context.Context, unit verify.ReportUnit) (verify.ReportResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.units = append(fake.units, unit)
	if fake.failNext != nil {
		err := fake.failNext
		fake.failNext = nil
		return verify.ReportResult{}, err
	}
	existing := fake.receipts[unit.Receipt.Key]
	outcome, err := receipt.Match(existing, unit.Receipt)
	if err != nil {
		return verify.ReportResult{}, err
	}
	switch outcome {
	case receipt.OutcomeReplay:
		result := fake.results[unit.Receipt.Key]
		result.Replayed = true
		return result, nil
	case receipt.OutcomeNew:
		stored := unit.Receipt
		fake.receipts[unit.Receipt.Key] = &stored
		result := verify.ReportResult{VerificationID: unit.VerificationID, EventID: unit.Events[0].ID}
		fake.results[unit.Receipt.Key] = result
		fake.commits++
		return result, nil
	default:
		return verify.ReportResult{}, fault.New(fault.InternalFailure, "unexpected receipt outcome")
	}
}

func (fake *fakeReports) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.units)
}

func (fake *fakeReports) commitCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.commits
}

func (fake *fakeReports) storedCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.receipts)
}

func (fake *fakeReports) unitAt(index int) verify.ReportUnit {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.units[index]
}

// reportIDs is a deterministic, concurrency-safe ReportIDSource.
type reportIDs struct {
	mu       sync.Mutex
	events   int
	receipts int
}

func (ids *reportIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.events++
	return id.EventID(fmt.Sprintf("event-%d", ids.events)), nil
}

func (ids *reportIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.receipts++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.receipts)), nil
}

type reportHarness struct {
	authorizer    *fakeAuthorizer
	authority     *fakeAuthority
	bindings      *fakeBindings
	verifications *fakeVerifications
	reports       *fakeReports
	clock         *fakeClock
	ids           *reportIDs
	reporter      *verify.Reporter
}

func (h *reportHarness) config() verify.ReportConfig {
	return verify.ReportConfig{
		Authorizer:    h.authorizer,
		Authority:     h.authority,
		Bindings:      h.bindings,
		Verifications: h.verifications,
		Reports:       h.reports,
		Clock:         h.clock,
		IDs:           h.ids,
	}
}

func newReportHarness(t *testing.T) *reportHarness {
	t.Helper()
	instance := &reportHarness{
		authorizer:    newFakeAuthorizer(),
		authority:     newFakeAuthority(),
		bindings:      &fakeBindings{},
		verifications: &fakeVerifications{record: verificationRecord()},
		reports:       newFakeReports(),
		clock:         &fakeClock{now: fixedNow},
		ids:           &reportIDs{},
	}
	instance.bindings.set(gitBinding(), nil)
	reporter, err := verify.NewReporter(instance.config())
	if err != nil {
		t.Fatalf("verify.NewReporter: %v", err)
	}
	instance.reporter = reporter
	return instance
}

func findAuthorityRead(t *testing.T, reads []authorize.AuthorityRead, check authorize.AuthorityCheck) authorize.AuthorityRead {
	t.Helper()
	for _, read := range reads {
		if read.Check == check {
			return read
		}
	}
	t.Fatalf("authority read %q is missing from %#v", check, reads)
	return authorize.AuthorityRead{}
}

// TestReportCommitsVerifiedReportUnit is the successful ingress path: the
// reporting service is currently authorized, the binding covers the resource
// and operation, the cited verification is approved and linked, and one
// complete ReportUnit carries the receipt, audit, envelope, current-authority
// reads and publication intent.
func TestReportCommitsVerifiedReportUnit(t *testing.T) {
	h := newReportHarness(t)
	command := reportCommand()

	result, err := h.reporter.Report(context.Background(), command)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if result.VerificationID != command.VerificationID {
		t.Errorf("result verification = %q, want %q", result.VerificationID, command.VerificationID)
	}
	if result.Replayed {
		t.Error("first report was marked replayed")
	}
	if _, err := id.ParseEventID(string(result.EventID)); err != nil {
		t.Fatalf("result event id %q is invalid: %v", result.EventID, err)
	}

	if h.reports.commitCount() != 1 {
		t.Fatalf("report commits = %d, want 1", h.reports.commitCount())
	}
	unit := h.reports.unitAt(0)
	if unit.Owner != testScope() {
		t.Errorf("unit owner = %#v, want %#v", unit.Owner, testScope())
	}
	if unit.ReportingService != testServiceID {
		t.Errorf("unit reporting service = %q, want %q", unit.ReportingService, testServiceID)
	}
	if unit.BindingID != testBindingID {
		t.Errorf("unit binding = %q, want %q", unit.BindingID, testBindingID)
	}
	if unit.SourceEventID != command.SourceEventID {
		t.Errorf("unit source event = %q, want %q", unit.SourceEventID, command.SourceEventID)
	}
	if unit.EventType != command.EventType {
		t.Errorf("unit event type = %q, want %q", unit.EventType, command.EventType)
	}
	if unit.VerificationID != command.VerificationID {
		t.Errorf("unit verification = %q, want %q", unit.VerificationID, command.VerificationID)
	}
	if unit.ResourceID != testResourceID {
		t.Errorf("unit resource = %q, want %q", unit.ResourceID, testResourceID)
	}
	if unit.Operation != string(resource.OperationGitRefUpdate) {
		t.Errorf("unit operation = %q, want %q", unit.Operation, resource.OperationGitRefUpdate)
	}
	if len(unit.Subject.Fields) != len(command.Subject.Fields) {
		t.Errorf("unit subject = %#v, want %#v", unit.Subject, command.Subject)
	}
	if err := unit.ValidateEventType(gitBinding()); err != nil {
		t.Errorf("unit does not validate against the current binding: %v", err)
	}

	wantKey := receipt.Key{
		TenantID:    testTenant,
		ProjectID:   testProject,
		PrincipalID: id.PrincipalID(testServiceID),
		Operation:   "resource.report",
		CallerKey:   reportSourceEventID,
	}
	if unit.Receipt.Key != wantKey {
		t.Errorf("receipt key = %#v, want %#v", unit.Receipt.Key, wantKey)
	}
	if unit.Receipt.ContentHash == "" {
		t.Error("receipt content hash is empty")
	}
	if !unit.Receipt.At.Equal(fixedNow) {
		t.Errorf("receipt time = %v, want %v", unit.Receipt.At, fixedNow)
	}
	var stored verify.ReportResult
	if err := json.Unmarshal(unit.Receipt.Result, &stored); err != nil {
		t.Fatalf("receipt result is not a report result: %v", err)
	}
	if stored.VerificationID != result.VerificationID || stored.EventID != result.EventID {
		t.Errorf("receipt result = %#v, want %#v", stored, result)
	}

	if len(unit.Audit) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(unit.Audit))
	}
	audit := unit.Audit[0]
	if audit.Scope != testScope() {
		t.Errorf("audit scope = %#v, want %#v", audit.Scope, testScope())
	}
	if audit.Actor != id.PrincipalID(testServiceID) {
		t.Errorf("audit actor = %q, want the reporting service %q", audit.Actor, testServiceID)
	}
	if audit.Operation != "resource.report" {
		t.Errorf("audit operation = %q, want %q", audit.Operation, "resource.report")
	}
	if !audit.At.Equal(fixedNow) {
		t.Errorf("audit time = %v, want %v", audit.At, fixedNow)
	}

	if len(unit.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(unit.Events))
	}
	envelope := unit.Events[0]
	if envelope.ID != result.EventID {
		t.Errorf("envelope id = %q, want the committed result id %q", envelope.ID, result.EventID)
	}
	if envelope.Type != event.TypeVerifiedReport {
		t.Errorf("envelope type = %q, want %q", envelope.Type, event.TypeVerifiedReport)
	}
	if envelope.ReportType != command.EventType {
		t.Errorf("envelope report type = %q, want %q", envelope.ReportType, command.EventType)
	}
	wantSource := event.Source{Kind: event.SourceService, PrincipalID: id.PrincipalID(testServiceID)}
	if envelope.Source != wantSource {
		t.Errorf("envelope source = %#v, want %#v", envelope.Source, wantSource)
	}
	if envelope.Scope != testScope() {
		t.Errorf("envelope scope = %#v, want %#v", envelope.Scope, testScope())
	}
	if envelope.SubjectID != string(testResourceID) {
		t.Errorf("envelope subject = %q, want %q", envelope.SubjectID, testResourceID)
	}
	if envelope.SubjectVersion != gitBinding().Version {
		t.Errorf("envelope subject version = %d, want %d", envelope.SubjectVersion, gitBinding().Version)
	}
	if envelope.CausationID != string(command.VerificationID) {
		t.Errorf("envelope causation = %q, want the verification id %q", envelope.CausationID, command.VerificationID)
	}
	if envelope.CorrelationID != string(testExecution) {
		t.Errorf("envelope correlation = %q, want the recorded execution %q", envelope.CorrelationID, testExecution)
	}
	if !envelope.At.Equal(fixedNow) {
		t.Errorf("envelope time = %v, want %v", envelope.At, fixedNow)
	}
	wantData := `{"candidate":"sha-candidate","expected_base":"sha-base","ref":"refs/heads/main"}`
	if string(envelope.Data) != wantData {
		t.Errorf("envelope data = %q, want the verified subject %q", envelope.Data, wantData)
	}
	if err := envelope.Validate(); err != nil {
		t.Errorf("envelope is invalid: %v", err)
	}

	reads := unit.Authority
	if len(reads) != 3 {
		t.Fatalf("authority reads = %d, want 3", len(reads))
	}
	serviceRead := findAuthorityRead(t, reads, authorize.CheckServiceEnabled)
	if serviceRead.Operation != "resource.report" {
		t.Errorf("service authority operation = %q, want %q", serviceRead.Operation, "resource.report")
	}
	if serviceRead.Scope != testScope() || serviceRead.PrincipalID != id.PrincipalID(testServiceID) {
		t.Errorf("service authority read = %#v", serviceRead)
	}
	versionRead := findAuthorityRead(t, reads, authorize.CheckBindingVersion)
	if versionRead.TargetID != string(testBindingID) || versionRead.ExpectedVersion != gitBinding().Version {
		t.Errorf("binding authority read = %#v, want target %q version %d", versionRead, testBindingID, gitBinding().Version)
	}
	ownershipRead := findAuthorityRead(t, reads, authorize.CheckResourceOwnership)
	if ownershipRead.TargetID != string(testResourceID) {
		t.Errorf("ownership authority read = %#v, want target %q", ownershipRead, testResourceID)
	}
	if len(unit.PostCommit) != 1 || unit.PostCommit[0].Kind != event.PostPublishEvent {
		t.Errorf("post-commit effects = %#v, want one publish", unit.PostCommit)
	}

	if h.authorizer.count() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.authorizer.count())
	}
	call := h.authorizer.callAt(0)
	if call.operation != "resource.report" || call.scope != testScope() || call.principal.ID != id.PrincipalID(testServiceID) {
		t.Errorf("authorizer call = %#v", call)
	}
	wantBindingQuery := verify.BindingQuery{Scope: testScope(), ResourceID: testResourceID, ServiceID: testServiceID}
	if got := h.bindings.lastQuery(); got != wantBindingQuery {
		t.Errorf("binding query = %#v, want %#v", got, wantBindingQuery)
	}
	if h.authority.count() != 1 {
		t.Errorf("authority reads = %d, want 1", h.authority.count())
	}
	if h.verifications.lastID() != command.VerificationID {
		t.Errorf("verification read = %q, want %q", h.verifications.lastID(), command.VerificationID)
	}
	if got := h.verifications.lastScope(); got != command.Scope {
		t.Errorf("verification read scope = %#v, want the report command scope %#v", got, command.Scope)
	}
}

// TestReportDerivesCausationFromVerificationRecord pins S08 §3: actor and
// execution causation come from the verification record, never from the report
// body, which has no such fields.
func TestReportDerivesCausationFromVerificationRecord(t *testing.T) {
	h := newReportHarness(t)
	record := verificationRecord()
	record.ExecutionID = "exec-9"
	record.ActionID = "action-9"
	h.verifications.set(record, nil)

	if _, err := h.reporter.Report(context.Background(), reportCommand()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	envelope := h.reports.unitAt(0).Events[0]
	if envelope.CausationID != string(record.ID) {
		t.Errorf("causation = %q, want the verification record id %q", envelope.CausationID, record.ID)
	}
	if envelope.CorrelationID != string(record.ExecutionID) {
		t.Errorf("correlation = %q, want the recorded execution %q", envelope.CorrelationID, record.ExecutionID)
	}
	if envelope.CausationID == string(record.ActionID) {
		t.Error("causation was taken from the recorded action instead of the verification record")
	}
}

// TestReportOfHumanPushKeepsVerificationCausationWithoutCorrelation pins the
// report shape of a human push. The causation is still the cited verification,
// the correlation is empty because the verified pusher has no execution, and
// the bounded data is the verified Git subject, so the push processor can parse
// the ref update and start the subscribed pipeline.
func TestReportOfHumanPushKeepsVerificationCausationWithoutCorrelation(t *testing.T) {
	h := newReportHarness(t)
	record := verificationRecord()
	record.Actor = humanActor()
	record.ExecutionID = ""
	record.ActionID = ""
	record.Subject = gitReportSubject()
	h.verifications.set(record, nil)
	command := reportCommand()
	command.Subject = gitReportSubject()

	if _, err := h.reporter.Report(context.Background(), command); err != nil {
		t.Fatalf("Report: %v", err)
	}
	envelope := h.reports.unitAt(0).Events[0]
	if envelope.CausationID != string(record.ID) {
		t.Errorf("causation = %q, want the verification record id %q", envelope.CausationID, record.ID)
	}
	if envelope.CorrelationID != "" {
		t.Errorf("correlation = %q, want none for a human pusher", envelope.CorrelationID)
	}
	var data map[string]string
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		t.Fatalf("envelope data %q is not a JSON object of subject fields: %v", envelope.Data, err)
	}
	want := map[string]string{
		"operation":     string(resource.OperationGitRefUpdate),
		"resource":      string(testResourceID),
		"ref":           "refs/heads/main",
		"expected_base": "sha-base",
		"candidate":     "sha-candidate",
	}
	if !reflect.DeepEqual(data, want) {
		t.Errorf("envelope data = %v, want the verified Git subject %v", data, want)
	}
}

// oversizedReportSubject is a verified effect subject the declared public
// subject bound accepts but the declared 64 KiB event data bound cannot carry:
// the API allows 64 fields of 4 KiB names and 4 KiB values. It stands in for
// any non-Git enforcing service whose verified effect subject is much larger
// than one event payload.
func oversizedReportSubject() verify.Subject {
	fields := make([]verify.SubjectField, 0, api.MaxSubjectFields)
	name := strings.Repeat("n", api.MaxSubjectValueBytes-3)
	value := strings.Repeat("v", api.MaxSubjectValueBytes)
	for index := range api.MaxSubjectFields {
		fields = append(fields, verify.SubjectField{
			Name:  fmt.Sprintf("%s-%02d", name, index),
			Kind:  verify.KindString,
			Value: value,
		})
	}
	return verify.Subject{Fields: fields}
}

// wantSubjectBeyondEventBound fails the test unless the subject encodes to more
// than the declared event data bound as one JSON object of field name to value,
// so a passing test cannot silently stop exercising the oversized case.
func wantSubjectBeyondEventBound(t *testing.T, subject verify.Subject) {
	t.Helper()
	fields := make(map[string]string, len(subject.Fields))
	for _, field := range subject.Fields {
		fields[field.Name] = field.Value
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal subject: %v", err)
	}
	if len(encoded) <= event.DefaultBounds.MaxDataBytes {
		t.Fatalf("test subject encodes to %d bytes; want more than the %d byte event bound",
			len(encoded), event.DefaultBounds.MaxDataBytes)
	}
}

// TestReportOfNonGitTypeCarriesNoDerivedSubjectData pins the compatible
// verified-report semantics: the verified subject becomes accepted event data
// only for the Git ref update report type the push processor parses. Any other
// report type records the caller-supplied data only, so a valid verified effect
// whose subject is far larger than the 64 KiB event data bound (the subject API
// allows 64 fields of 4 KiB names and values) still commits with empty event
// data instead of being refused after the effect, and keeps its derived
// causation and correlation.
func TestReportOfNonGitTypeCarriesNoDerivedSubjectData(t *testing.T) {
	cases := []struct {
		name      string
		subject   func() verify.Subject
		oversized bool
	}{
		{"small-subject", requiredSubject, false},
		{"subject-beyond-event-bound", oversizedReportSubject, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newReportHarness(t)
			binding := gitBinding()
			binding.ReportableEvents = []string{"git.ref.updated", "resource.merged"}
			h.bindings.set(binding, nil)
			subject := testCase.subject()
			if testCase.oversized {
				wantSubjectBeyondEventBound(t, subject)
			}
			record := verificationRecord()
			record.Subject = subject
			h.verifications.set(record, nil)

			command := reportCommand()
			command.EventType = event.ReportType("resource.merged")
			command.Subject = subject

			result, err := h.reporter.Report(context.Background(), command)
			if err != nil {
				t.Fatalf("Report of a verified %q effect with a %d-field subject: %v",
					command.EventType, len(subject.Fields), err)
			}
			if result.VerificationID != record.ID {
				t.Errorf("result verification = %q, want %q", result.VerificationID, record.ID)
			}
			if h.reports.commitCount() != 1 {
				t.Fatalf("report commits = %d, want 1", h.reports.commitCount())
			}
			unit := h.reports.unitAt(0)
			if !reflect.DeepEqual(unit.Subject, subject) {
				t.Errorf("unit subject has %d fields, want the verified subject with %d fields",
					len(unit.Subject.Fields), len(subject.Fields))
			}
			if len(unit.Events) != 1 {
				t.Fatalf("events = %d, want 1", len(unit.Events))
			}
			envelope := unit.Events[0]
			if envelope.ReportType != command.EventType {
				t.Errorf("envelope report type = %q, want %q", envelope.ReportType, command.EventType)
			}
			if len(envelope.Data) != 0 {
				t.Errorf("non-Git report event data = %d bytes, want none", len(envelope.Data))
			}
			if envelope.CausationID != string(record.ID) {
				t.Errorf("causation = %q, want the verification record id %q", envelope.CausationID, record.ID)
			}
			if envelope.CorrelationID != string(record.ExecutionID) {
				t.Errorf("correlation = %q, want the recorded execution %q", envelope.CorrelationID, record.ExecutionID)
			}
			if err := envelope.Validate(); err != nil {
				t.Errorf("envelope is invalid: %v", err)
			}
		})
	}
}

// TestReportOfGitRefUpdateRefusesSubjectBeyondEventBound documents the one
// remaining refusal: the Git ref update report type derives its accepted event
// data from the verified subject because the push processor parses it, and the
// declared event data bound still applies. A real Git subject is the five small
// ref update fields, so this case is unreachable for the Git service; it is
// pinned so the derived-payload path keeps validating the bound it writes into.
func TestReportOfGitRefUpdateRefusesSubjectBeyondEventBound(t *testing.T) {
	h := newReportHarness(t)
	subject := oversizedReportSubject()
	wantSubjectBeyondEventBound(t, subject)
	record := verificationRecord()
	record.Subject = subject
	h.verifications.set(record, nil)

	command := reportCommand()
	command.Subject = subject

	_, err := h.reporter.Report(context.Background(), command)
	wantFaultClass(t, err, fault.InvalidInput)
	if h.reports.count() != 0 {
		t.Errorf("report port calls = %d, want 0 for a subject beyond the event bound", h.reports.count())
	}
	if h.verifications.count() == 0 {
		t.Error("the bound was checked before the verified subject could be read")
	}
}

// TestReportKeepsExplicitData keeps bounded data the caller derived itself:
// only an empty payload of the Git ref update type is filled from the verified
// subject.
func TestReportKeepsExplicitData(t *testing.T) {
	h := newReportHarness(t)
	command := reportCommand()
	command.Data = []byte(`{"note":"explicit"}`)

	if _, err := h.reporter.Report(context.Background(), command); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got := h.reports.unitAt(0).Events[0].Data; !bytes.Equal(got, command.Data) {
		t.Errorf("envelope data = %q, want the explicit data %q", got, command.Data)
	}
}

// TestReportRefusesWrongReportingService refuses a service that the current
// binding does not enforce for the resource.
func TestReportRefusesWrongReportingService(t *testing.T) {
	h := newReportHarness(t)
	command := reportCommand()
	command.Service.ID = id.PrincipalID("svc-other")

	_, err := h.reporter.Report(context.Background(), command)
	wantFaultClass(t, err, fault.Forbidden)
	if h.reports.count() != 0 {
		t.Errorf("report port calls = %d, want 0", h.reports.count())
	}
	if h.verifications.count() != 0 {
		t.Errorf("verification reads = %d, want 0 before binding linkage holds", h.verifications.count())
	}
}

// TestReportRefusesWrongBindingLinkage refuses a verification that was
// committed under a different binding.
func TestReportRefusesWrongBindingLinkage(t *testing.T) {
	h := newReportHarness(t)
	record := verificationRecord()
	record.BindingID = "binding-other"
	h.verifications.set(record, nil)

	_, err := h.reporter.Report(context.Background(), reportCommand())
	wantFaultClass(t, err, fault.Forbidden)
	if h.reports.count() != 0 {
		t.Errorf("report port calls = %d, want 0", h.reports.count())
	}
}

// TestReportRefusesVerificationLinkageMismatch covers every field the report
// and verification must agree on: scope, service, resource, operation, subject
// and the approved result.
func TestReportRefusesVerificationLinkageMismatch(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*verify.Record)
	}{
		{"scope", func(record *verify.Record) { record.Scope = otherScope() }},
		{"service", func(record *verify.Record) { record.CallingService = service.ServiceID("svc-other") }},
		{"resource", func(record *verify.Record) { record.ResourceID = resource.ResourceID("repo-other") }},
		{"operation", func(record *verify.Record) { record.Operation = "git.ref.other" }},
		{"subject", func(record *verify.Record) {
			record.Subject = subjectWith("candidate", verify.KindString, "sha-other")
		}},
		{"refused", func(record *verify.Record) { record.Result = verify.ResultRefused }},
		{"identity", func(record *verify.Record) { record.ID = "verification-other" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newReportHarness(t)
			record := verificationRecord()
			testCase.mutate(&record)
			h.verifications.set(record, nil)

			_, err := h.reporter.Report(context.Background(), reportCommand())
			wantFaultClass(t, err, fault.Forbidden)
			if h.reports.count() != 0 {
				t.Errorf("report port calls = %d, want 0", h.reports.count())
			}
		})
	}
}

// TestReportRefusesGitReadVerification pins that a repository read check is
// never reportable in the domain linkage: even a binding that declares
// git.read, an approved empty-subject read record and a matching command must
// not commit a verified resource report. A read verifies access, not an effect.
func TestReportRefusesGitReadVerification(t *testing.T) {
	h := newReportHarness(t)
	binding := gitBinding()
	binding.Operations = []resource.Operation{resource.OperationGitRead}
	h.bindings.set(binding, nil)
	record := verificationRecord()
	record.Operation = string(resource.OperationGitRead)
	record.Subject = verify.Subject{}
	h.verifications.set(record, nil)

	command := reportCommand()
	command.Operation = resource.OperationGitRead
	command.Subject = verify.Subject{}
	command.VerificationID = record.ID

	_, err := h.reporter.Report(context.Background(), command)
	wantFaultClass(t, err, fault.Forbidden)
	if h.reports.count() != 0 {
		t.Errorf("report port calls = %d, want 0", h.reports.count())
	}
}

// TestReportRefusesUnknownVerification propagates the reader's not_found
// refusal without committing anything.
func TestReportRefusesUnknownVerification(t *testing.T) {
	h := newReportHarness(t)
	h.verifications.set(verify.Record{}, fault.New(fault.NotFound, "unknown verification"))

	_, err := h.reporter.Report(context.Background(), reportCommand())
	wantFaultClass(t, err, fault.NotFound)
	if h.reports.count() != 0 {
		t.Errorf("report port calls = %d, want 0", h.reports.count())
	}
}

// TestReportRefusesBadEventType accepts only a binding-declared, non-lifecycle
// report type.
func TestReportRefusesBadEventType(t *testing.T) {
	cases := []struct {
		name      string
		eventType event.ReportType
	}{
		{"lifecycle-observation", event.ReportType(event.TypeCustomObservation)},
		{"lifecycle-decision", event.ReportType(event.TypePipelineStartRefused)},
		{"undeclared", event.ReportType("git.ref.deleted")},
		{"empty", event.ReportType("")},
		{"syntax", event.ReportType("Git Ref!")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newReportHarness(t)
			command := reportCommand()
			command.EventType = testCase.eventType

			_, err := h.reporter.Report(context.Background(), command)
			wantFaultClass(t, err, fault.InvalidInput)
			if h.reports.count() != 0 {
				t.Errorf("report port calls = %d, want 0", h.reports.count())
			}
			if h.verifications.count() != 0 {
				t.Errorf("verification reads = %d, want 0 before the event type is accepted", h.verifications.count())
			}
		})
	}
}

// TestReportRefusesUnauthorizedServiceWithZeroPortCalls denies the registry
// grant before any binding, authority, verification or report port is touched.
func TestReportRefusesUnauthorizedServiceWithZeroPortCalls(t *testing.T) {
	h := newReportHarness(t)
	h.authorizer.setDecision(authorize.DecisionDeny, nil)

	_, err := h.reporter.Report(context.Background(), reportCommand())
	wantFaultClass(t, err, fault.Forbidden)
	if h.authorizer.count() != 1 {
		t.Errorf("authorizer calls = %d, want 1", h.authorizer.count())
	}
	if h.bindings.queryCount() != 0 || h.authority.count() != 0 || h.verifications.count() != 0 || h.reports.count() != 0 {
		t.Errorf("unauthorized report touched a port: bindings=%d authority=%d verifications=%d reports=%d",
			h.bindings.queryCount(), h.authority.count(), h.verifications.count(), h.reports.count())
	}
}

// TestReportRefusesCrossScopeServiceWithZeroPortCalls refuses a service whose
// verified scope does not cover the report scope before any dependency runs.
func TestReportRefusesCrossScopeServiceWithZeroPortCalls(t *testing.T) {
	h := newReportHarness(t)
	command := reportCommand()
	command.Service.Scope = otherScope()

	_, err := h.reporter.Report(context.Background(), command)
	wantFaultClass(t, err, fault.Forbidden)
	if h.authorizer.count() != 0 || h.bindings.queryCount() != 0 || h.authority.count() != 0 || h.verifications.count() != 0 || h.reports.count() != 0 {
		t.Errorf("cross-scope report touched a dependency: authorizer=%d bindings=%d authority=%d verifications=%d reports=%d",
			h.authorizer.count(), h.bindings.queryCount(), h.authority.count(), h.verifications.count(), h.reports.count())
	}
}

// TestReportRefusesDisabledReportingService refuses a revoked/disabled
// reporting service before any dependency runs.
func TestReportRefusesDisabledReportingService(t *testing.T) {
	h := newReportHarness(t)
	command := reportCommand()
	command.Service.Enabled = false

	_, err := h.reporter.Report(context.Background(), command)
	wantFaultClass(t, err, fault.Forbidden)
	if h.authorizer.count() != 0 || h.bindings.queryCount() != 0 || h.authority.count() != 0 || h.verifications.count() != 0 || h.reports.count() != 0 {
		t.Errorf("disabled-service report touched a dependency: authorizer=%d bindings=%d authority=%d verifications=%d reports=%d",
			h.authorizer.count(), h.bindings.queryCount(), h.authority.count(), h.verifications.count(), h.reports.count())
	}
}

// TestReportRefusesCurrentAuthorityRevocation re-reads current authority and
// refuses a service that lost enablement after the grant lookup.
func TestReportRefusesCurrentAuthorityRevocation(t *testing.T) {
	h := newReportHarness(t)
	h.authority.denyCheck(authorize.CheckServiceEnabled)

	_, err := h.reporter.Report(context.Background(), reportCommand())
	wantFaultClass(t, err, fault.Forbidden)
	if h.reports.count() != 0 {
		t.Errorf("report port calls = %d, want 0", h.reports.count())
	}
	if h.verifications.count() != 0 {
		t.Errorf("verification reads = %d, want 0 after authority refusal", h.verifications.count())
	}
}

// TestReportRefusesBindingVersionConflict refuses a binding that moved between
// the read and the authority confirmation.
func TestReportRefusesBindingVersionConflict(t *testing.T) {
	h := newReportHarness(t)
	h.authority.overrideVersion(authorize.CheckBindingVersion, gitBinding().Version+1)

	_, err := h.reporter.Report(context.Background(), reportCommand())
	wantFaultClass(t, err, fault.VersionConflict)
	if h.reports.count() != 0 {
		t.Errorf("report port calls = %d, want 0", h.reports.count())
	}
}

// TestReportReplaysIdenticalContentAfterCurrentAuthorization returns the
// original result for an equal retry, after re-reading current authority, and
// commits once.
func TestReportReplaysIdenticalContentAfterCurrentAuthorization(t *testing.T) {
	h := newReportHarness(t)

	first, err := h.reporter.Report(context.Background(), reportCommand())
	if err != nil {
		t.Fatalf("first Report: %v", err)
	}
	second, err := h.reporter.Report(context.Background(), reportCommand())
	if err != nil {
		t.Fatalf("second Report: %v", err)
	}
	if first.Replayed {
		t.Error("first report was marked replayed")
	}
	if !second.Replayed {
		t.Error("equal retry was not marked replayed")
	}
	if second.EventID != first.EventID || second.VerificationID != first.VerificationID {
		t.Errorf("replay result = %#v, want the original %#v", second, first)
	}
	if h.reports.commitCount() != 1 {
		t.Errorf("report commits = %d, want 1", h.reports.commitCount())
	}
	if h.authorizer.count() != 2 || h.authority.count() != 2 {
		t.Errorf("replay authorization = authorizer %d authority %d, want 2 each", h.authorizer.count(), h.authority.count())
	}
}

// TestReportRefusesChangedReplay refuses changed content for the same
// (service, source-event-id) identity.
func TestReportRefusesChangedReplay(t *testing.T) {
	h := newReportHarness(t)
	if _, err := h.reporter.Report(context.Background(), reportCommand()); err != nil {
		t.Fatalf("first Report: %v", err)
	}

	changed := reportCommand()
	changed.Data = []byte(`{"ref":"refs/heads/other"}`)
	_, err := h.reporter.Report(context.Background(), changed)
	wantFaultClass(t, err, fault.ChangedIdempotencyContent)
	if h.reports.commitCount() != 1 {
		t.Errorf("report commits = %d, want 1", h.reports.commitCount())
	}
}

// TestReportRefusesReplayAfterServiceDisabled proves a replay never grants
// authority: current authority is re-read before the stored result is exposed.
func TestReportRefusesReplayAfterServiceDisabled(t *testing.T) {
	h := newReportHarness(t)
	if _, err := h.reporter.Report(context.Background(), reportCommand()); err != nil {
		t.Fatalf("first Report: %v", err)
	}
	h.authority.denyCheck(authorize.CheckServiceEnabled)

	_, err := h.reporter.Report(context.Background(), reportCommand())
	wantFaultClass(t, err, fault.Forbidden)
	if h.reports.commitCount() != 1 {
		t.Errorf("report commits = %d, want 1", h.reports.commitCount())
	}
}

// TestReportAcceptsLateReportAfterVerificationActorRevoked pins S08 §3: an
// accepted effect can be reported after the actor token expires or is revoked.
// Reporting authenticates the service and never grants new actor authority; the
// verification record stays a fact, so its actor is not re-enabled or re-read.
func TestReportAcceptsLateReportAfterVerificationActorRevoked(t *testing.T) {
	h := newReportHarness(t)
	record := verificationRecord()
	record.Actor.Enabled = false
	h.verifications.set(record, nil)

	result, err := h.reporter.Report(context.Background(), reportCommand())
	if err != nil {
		t.Fatalf("late Report after actor revocation: %v", err)
	}
	if result.VerificationID != record.ID {
		t.Errorf("result verification = %q, want %q", result.VerificationID, record.ID)
	}
	if h.reports.commitCount() != 1 {
		t.Errorf("report commits = %d, want 1", h.reports.commitCount())
	}
}

// TestReportRollbackLeavesNoPartialState commits through one port call: a
// failed apply stores nothing, and a later retry commits normally.
func TestReportRollbackLeavesNoPartialState(t *testing.T) {
	h := newReportHarness(t)
	h.reports.setFailNext(fault.New(fault.DependencyUnavailable, "report store unavailable"))

	_, err := h.reporter.Report(context.Background(), reportCommand())
	wantFaultClass(t, err, fault.DependencyUnavailable)
	if h.reports.commitCount() != 0 || h.reports.storedCount() != 0 {
		t.Errorf("failed report left state: commits=%d stored=%d", h.reports.commitCount(), h.reports.storedCount())
	}

	result, err := h.reporter.Report(context.Background(), reportCommand())
	if err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if result.Replayed {
		t.Error("retry after a failed commit was marked replayed")
	}
	if h.reports.commitCount() != 1 || h.reports.storedCount() != 1 {
		t.Errorf("retry state: commits=%d stored=%d, want 1 each", h.reports.commitCount(), h.reports.storedCount())
	}
}

// TestReportConcurrentSameSourceEventCommitsOnce serializes concurrent
// reports of the same (service, source-event-id): one commit, one event id.
func TestReportConcurrentSameSourceEventCommitsOnce(t *testing.T) {
	h := newReportHarness(t)
	const workers = 8
	results := make([]verify.ReportResult, workers)
	errs := make([]error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = h.reporter.Report(context.Background(), reportCommand())
		}(index)
	}
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Report %d: %v", index, err)
		}
	}
	if h.reports.commitCount() != 1 {
		t.Fatalf("report commits = %d, want 1", h.reports.commitCount())
	}
	eventID := results[0].EventID
	replayed := 0
	for index, result := range results {
		if result.EventID != eventID {
			t.Errorf("result %d event = %q, want %q", index, result.EventID, eventID)
		}
		if result.Replayed {
			replayed++
		}
	}
	if replayed != workers-1 {
		t.Errorf("replayed results = %d, want %d", replayed, workers-1)
	}
}

// TestNewReporterRequiresEveryDependency refuses a partial composition.
func TestNewReporterRequiresEveryDependency(t *testing.T) {
	h := newReportHarness(t)
	cases := []struct {
		name   string
		mutate func(*verify.ReportConfig)
	}{
		{"authorizer", func(config *verify.ReportConfig) { config.Authorizer = nil }},
		{"authority", func(config *verify.ReportConfig) { config.Authority = nil }},
		{"bindings", func(config *verify.ReportConfig) { config.Bindings = nil }},
		{"verifications", func(config *verify.ReportConfig) { config.Verifications = nil }},
		{"reports", func(config *verify.ReportConfig) { config.Reports = nil }},
		{"clock", func(config *verify.ReportConfig) { config.Clock = nil }},
		{"ids", func(config *verify.ReportConfig) { config.IDs = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := h.config()
			testCase.mutate(&config)
			if _, err := verify.NewReporter(config); err == nil {
				t.Errorf("NewReporter accepted a missing %s", testCase.name)
			}
		})
	}
}

// TestReportRefusesMalformedCommand validates the report shape before any
// dependency runs.
func TestReportRefusesMalformedCommand(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*verify.ReportCommand)
	}{
		{"empty-scope", func(command *verify.ReportCommand) { command.Scope = tenancy.Scope{} }},
		{"tenant-scope", func(command *verify.ReportCommand) { command.Scope = tenancy.Scope{TenantID: testTenant} }},
		{"empty-source-event", func(command *verify.ReportCommand) { command.SourceEventID = "" }},
		{"oversized-source-event", func(command *verify.ReportCommand) {
			command.SourceEventID = strings.Repeat("a", id.MaxIDLength+1)
		}},
		{"invalid-resource", func(command *verify.ReportCommand) { command.ResourceID = resource.ResourceID("bad id!") }},
		{"invalid-operation", func(command *verify.ReportCommand) { command.Operation = resource.Operation("git.ref.delete") }},
		{"invalid-verification", func(command *verify.ReportCommand) { command.VerificationID = verify.VerificationID("bad id!") }},
		{"human-service", func(command *verify.ReportCommand) { command.Service = humanActor() }},
		{"invalid-subject-kind", func(command *verify.ReportCommand) {
			command.Subject = verify.Subject{Fields: []verify.SubjectField{{Name: "ref", Kind: "object", Value: "x"}}}
		}},
		{"oversized-data", func(command *verify.ReportCommand) {
			command.Data = bytes.Repeat([]byte("a"), event.DefaultBounds.MaxDataBytes+1)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newReportHarness(t)
			command := reportCommand()
			testCase.mutate(&command)

			_, err := h.reporter.Report(context.Background(), command)
			wantFaultClass(t, err, fault.InvalidInput)
			if h.authorizer.count() != 0 || h.bindings.queryCount() != 0 || h.authority.count() != 0 || h.verifications.count() != 0 || h.reports.count() != 0 {
				t.Errorf("malformed report touched a dependency: authorizer=%d bindings=%d authority=%d verifications=%d reports=%d",
					h.authorizer.count(), h.bindings.queryCount(), h.authority.count(), h.verifications.count(), h.reports.count())
			}
		})
	}
}

// TestReportNeverCarriesCredentialMaterial checks that the committed unit and
// result expose no credential field and that refusal errors do not echo the
// reported subject.
func TestReportNeverCarriesCredentialMaterial(t *testing.T) {
	h := newReportHarness(t)
	result, err := h.reporter.Report(context.Background(), reportCommand())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	unitJSON, err := json.Marshal(h.reports.unitAt(0))
	if err != nil {
		t.Fatalf("marshal unit: %v", err)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, encoded := range []string{string(unitJSON), string(resultJSON)} {
		lower := strings.ToLower(encoded)
		for _, forbidden := range []string{"credential", "token", "secret"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("committed report contains %q: %s", forbidden, encoded)
			}
		}
	}

	// A linkage refusal names no reported value.
	h2 := newReportHarness(t)
	command := reportCommand()
	command.Subject = verify.Subject{Fields: []verify.SubjectField{
		{Name: "candidate", Kind: verify.KindString, Value: "sensitive-value"},
	}}
	_, err = h2.reporter.Report(context.Background(), command)
	if err == nil {
		t.Fatal("expected a linkage refusal")
	}
	if strings.Contains(err.Error(), "sensitive-value") {
		t.Errorf("refusal error echoed a reported value: %v", err)
	}
}

// The verified-report ingress service.
//
// types.go and ports.go declare ReportUnit, ReportPort, Record, BindingQuery
// and ReportResult. This file adds the orchestration entry point that builds
// and applies a ReportUnit, and the read-only verification seam it needs.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
)

// operationResourceReport is the declared registry row this service implements.
const operationResourceReport authorize.OperationName = "resource.report"

// VerificationReader reads one committed verification fact by identity inside
// the caller's owning scope.
//
// Scope is the owner scope of the report command, that is the reporting
// service's scope. The reader reads only inside it: a record outside the scope
// is uniformly the same not_found refusal as an unknown identity, never
// forbidden, so the seam is not an existence oracle and a store-backed reader
// never has to guess a scope.
//
// A verification record is a durable fact, never a bearer grant: the report
// path re-checks its scope, service, binding, resource, operation, subject and
// approved result, but never re-authenticates or re-enables the actor. That is
// why an accepted effect can still be reported after the actor token expires or
// is revoked (S08 §3); the reporting service's own current authority is what the
// report commit re-reads.
type VerificationReader interface {
	ReadVerification(context.Context, tenancy.Scope, VerificationID) (Record, error)
}

// ReportIDSource allocates the durable audit, receipt and accepted-event
// identities one verified-report commit unit carries.
type ReportIDSource interface {
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
}

// ReportConfig is the complete verified-report composition. Every dependency is
// required; NewReporter refuses a partial composition instead of guessing a
// substitute.
type ReportConfig struct {
	Authorizer    authorize.Authorizer
	Authority     authorize.AuthorityReader
	Bindings      BindingReader
	Verifications VerificationReader
	Reports       ReportPort
	Clock         Clock
	IDs           ReportIDSource
}

// ReportCommand is one verified-report ingress.
//
// Service is the reporting service principal already authenticated by the
// caller's transport boundary, exactly like Verifier.Attempt.Service. The
// command deliberately carries no credential, principal, approval,
// execution/action causation or receipt key: actor and execution causation come
// from the cited verification record, and (reporting service, SourceEventID) is
// the deduplication identity. Data is an optional bounded effect payload the
// public edge may derive from the report; the accepted envelope records it as
// bounded data. When it is empty and the report type is the Git ref update the
// push processor parses, the envelope carries the verified subject fields
// instead; every other type carries no derived data.
type ReportCommand struct {
	Scope          tenancy.Scope
	Service        identity.Principal
	SourceEventID  string
	EventType      event.ReportType
	VerificationID VerificationID
	ResourceID     resource.ResourceID
	Operation      resource.Operation
	Subject        Subject
	Data           []byte
}

// Reporter applies verified resource reports through the declared ReportPort.
//
// It holds only immutable dependencies, so concurrent reports share no
// per-report state. Every attempt authenticates and authorizes the reporting
// service, reads the current binding, re-reads current authority, reads the
// cited verification fact and rebuilds the complete commit unit; a previously
// allowed grant or a replayed receipt is never treated as current permission.
type Reporter struct {
	authorizer    authorize.Authorizer
	authority     authorize.AuthorityReader
	bindings      BindingReader
	verifications VerificationReader
	reports       ReportPort
	clock         Clock
	ids           ReportIDSource
}

// NewReporter validates the composition and returns a reporter.
func NewReporter(config ReportConfig) (*Reporter, error) {
	switch {
	case config.Authorizer == nil:
		return nil, fault.New(fault.InvalidInput, "reporter requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InvalidInput, "reporter requires an authority reader")
	case config.Bindings == nil:
		return nil, fault.New(fault.InvalidInput, "reporter requires a binding reader")
	case config.Verifications == nil:
		return nil, fault.New(fault.InvalidInput, "reporter requires a verification reader")
	case config.Reports == nil:
		return nil, fault.New(fault.InvalidInput, "reporter requires a report port")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "reporter requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InvalidInput, "reporter requires an identity source")
	}
	return &Reporter{
		authorizer:    config.Authorizer,
		authority:     config.Authority,
		bindings:      config.Bindings,
		verifications: config.Verifications,
		reports:       config.Reports,
		clock:         config.Clock,
		ids:           config.IDs,
	}, nil
}

// Report authenticates and authorizes the reporting service, validates the
// binding-declared report type, confirms the current-authority reads, verifies
// the cited verification linkage, and applies one complete ReportUnit through
// ReportPort receipt-first.
//
// The reporting service is authenticated by the caller's transport boundary
// exactly as Verifier.Attempt.Service is; the cited record is the durable fact
// the existing Verifier committed, and no actor credential is accepted,
// forwarded or persisted here. The event envelope uses the trusted lifecycle
// type resource.verified and carries the binding-declared type in ReportType;
// the source is the reporting service and causation/correlation come from the
// verification record. An identical retry returns the original committed result
// after current authorization, while changed content for the same (service,
// source-event-id) is refused as changed_idempotency_content by the port's
// receipt matching.
func (reporter *Reporter) Report(ctx context.Context, command ReportCommand) (ReportResult, error) {
	if err := validateReportCommand(command); err != nil {
		return ReportResult{}, err
	}

	operation, ok := authorize.LookupOperation(operationResourceReport)
	if !ok {
		return ReportResult{}, fault.New(fault.InternalFailure, "resource.report operation is not registered")
	}
	decision, err := reporter.authorizer.Authorize(ctx, command.Service, operation.Name, command.Scope)
	if err != nil {
		return ReportResult{}, reportDependencyError(err, "resource.report authorization failed")
	}
	if decision != authorize.DecisionAllow {
		return ReportResult{}, fault.New(fault.Forbidden, fmt.Sprintf(
			"principal %q may not perform %q", command.Service.ID, operation.Name))
	}

	if err := validateReportEventType(command.EventType); err != nil {
		return ReportResult{}, err
	}

	binding, err := reporter.bindings.ReadBinding(ctx, BindingQuery{
		Scope:      command.Scope,
		ResourceID: command.ResourceID,
		ServiceID:  service.ServiceID(command.Service.ID),
	})
	if err != nil {
		return ReportResult{}, reportDependencyError(err, "report binding read failed")
	}
	if err := validateReportBinding(command, binding); err != nil {
		return ReportResult{}, err
	}
	if !binding.DeclaresReport(string(command.EventType)) {
		return ReportResult{}, fault.New(fault.InvalidInput, fmt.Sprintf(
			"binding does not declare reportable event type %q", command.EventType))
	}

	reads := reportAuthorityReads(operation, command.Scope, command.Service.ID, binding, command.ResourceID)
	facts, err := reporter.authority.Read(ctx, reads)
	if err != nil {
		return ReportResult{}, reportDependencyError(err, "report authority read failed")
	}
	if err := authorize.Confirm(reads, facts); err != nil {
		return ReportResult{}, err
	}

	record, err := reporter.verifications.ReadVerification(ctx, command.Scope, command.VerificationID)
	if err != nil {
		return ReportResult{}, reportDependencyError(err, "verification record read failed")
	}
	if err := validateVerificationLinkage(command, binding, record); err != nil {
		return ReportResult{}, err
	}

	at := reporter.clock.Now()
	eventID, err := reporter.ids.NewEventID()
	if err != nil {
		return ReportResult{}, reportDependencyError(err, "report event identity allocation failed")
	}
	if _, err := id.ParseEventID(string(eventID)); err != nil {
		return ReportResult{}, fault.Wrap(fault.InternalFailure, "report event identity source returned an invalid id", err)
	}
	data, err := reportEventData(command.Data, command.EventType, record.Subject)
	if err != nil {
		return ReportResult{}, err
	}
	envelope, err := event.NewEnvelope(eventID, event.Source{
		Kind:        event.SourceService,
		PrincipalID: id.PrincipalID(command.Service.ID),
	}, command.Scope, at, event.EnvelopeInput{
		Type:           event.TypeVerifiedReport,
		ReportType:     command.EventType,
		SubjectID:      string(command.ResourceID),
		SubjectVersion: binding.Version,
		CorrelationID:  string(record.ExecutionID),
		CausationID:    string(record.ID),
		Data:           data,
	})
	if err != nil {
		return ReportResult{}, err
	}
	auditID, err := reporter.ids.NewEventID()
	if err != nil {
		return ReportResult{}, reportDependencyError(err, "report audit identity allocation failed")
	}
	if _, err := id.ParseEventID(string(auditID)); err != nil {
		return ReportResult{}, fault.Wrap(fault.InternalFailure, "report audit identity source returned an invalid id", err)
	}

	result := ReportResult{VerificationID: record.ID, EventID: eventID}
	content := reportContent{
		BindingID:      binding.ID,
		ResourceID:     command.ResourceID,
		EventType:      command.EventType,
		Operation:      command.Operation,
		VerificationID: command.VerificationID,
		Subject:        command.Subject,
		Data:           command.Data,
	}
	receiptRecord, err := reporter.buildReportReceipt(command, content, result, at)
	if err != nil {
		return ReportResult{}, err
	}

	unit := ReportUnit{
		Owner:            command.Scope,
		ReportingService: service.ServiceID(command.Service.ID),
		BindingID:        binding.ID,
		SourceEventID:    command.SourceEventID,
		EventType:        command.EventType,
		VerificationID:   record.ID,
		ResourceID:       command.ResourceID,
		Operation:        string(command.Operation),
		Subject:          command.Subject,
		Receipt:          receiptRecord,
		Audit: []event.Audit{{
			ID:        auditID,
			Scope:     command.Scope,
			Actor:     id.PrincipalID(command.Service.ID),
			Operation: string(operationResourceReport),
			At:        at,
		}},
		Events:     []event.Envelope{envelope},
		Authority:  reads,
		PostCommit: []event.PostCommitEffect{{Kind: event.PostPublishEvent}},
	}
	if err := unit.ValidateEventType(binding); err != nil {
		return ReportResult{}, err
	}

	committed, err := reporter.reports.Report(ctx, unit)
	if err != nil {
		return ReportResult{}, reportDependencyError(err, "report commit failed")
	}
	if _, err := ParseVerificationID(string(committed.VerificationID)); err != nil {
		return ReportResult{}, fault.New(fault.InternalFailure, "report port returned no valid verification id")
	}
	if _, err := id.ParseEventID(string(committed.EventID)); err != nil {
		return ReportResult{}, fault.New(fault.InternalFailure, "report port returned no valid event id")
	}
	if committed.VerificationID != record.ID {
		return ReportResult{}, fault.New(fault.InternalFailure, "report port returned a different verification id")
	}
	return committed, nil
}

// reportContent is the normalized receipt content of one report. It pins every
// reported fact that the (service, source-event-id) identity must reproduce; the
// receipt key itself carries the deduplication identity.
type reportContent struct {
	BindingID      resource.BindingID  `json:"binding_id"`
	ResourceID     resource.ResourceID `json:"resource_id"`
	EventType      event.ReportType    `json:"event_type"`
	Operation      resource.Operation  `json:"operation"`
	VerificationID VerificationID      `json:"verification_id"`
	Subject        Subject             `json:"subject"`
	Data           []byte              `json:"data,omitempty"`
}

// reportGitRefUpdated is the one binding-declared report type whose verified
// subject is also the accepted event payload consumers read: the push
// processor parses a git.ref.updated report to start subscribed pipelines. It
// mirrors trigger.ReportTypeGitRefUpdated without importing the consumer
// package. Every other report type keeps the previous event shape, with data
// only when the caller supplied it.
const reportGitRefUpdated = event.ReportType("git.ref.updated")

// reportEventData returns the bounded data of the accepted report event. Data
// the caller supplied is kept as given. Otherwise, for the Git ref update
// report type only, the data is the verified effect subject as one JSON object
// of field name to value (operation, resource, ref, expected_base and
// candidate), taken from the verification record the report already matched, so
// consumers such as the push processor read the verified effect and never a
// client-supplied value. Every other report type carries no derived data: its
// verified subject may exceed the declared event data bound even though the
// public subject bound allows it, and refusing such a valid verified report
// after the effect would reject work that succeeded.
func reportEventData(explicit []byte, reportType event.ReportType, subject Subject) ([]byte, error) {
	if len(explicit) > 0 {
		return explicit, nil
	}
	if reportType != reportGitRefUpdated || len(subject.Fields) == 0 {
		return nil, nil
	}
	fields := make(map[string]string, len(subject.Fields))
	for _, field := range subject.Fields {
		fields[field.Name] = field.Value
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "report event data could not be encoded", err)
	}
	if len(data) > event.DefaultBounds.MaxDataBytes {
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf(
			"verified report subject is %d bytes as event data; at most %d are allowed", len(data), event.DefaultBounds.MaxDataBytes))
	}
	return data, nil
}

// buildReportReceipt builds the scoped idempotency receipt for one report. The
// durable result body is the original committed ReportResult so an identical
// retry converges on the same event id without repeating the effect.
func (reporter *Reporter) buildReportReceipt(
	command ReportCommand,
	content reportContent,
	result ReportResult,
	at time.Time,
) (receipt.Record, error) {
	body, err := json.Marshal(content)
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "report receipt content could not be encoded", err)
	}
	fingerprint, err := receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: body})
	if err != nil {
		return receipt.Record{}, err
	}
	receiptID, err := reporter.ids.NewReceiptID()
	if err != nil {
		return receipt.Record{}, reportDependencyError(err, "report receipt identity allocation failed")
	}
	resultBody, err := json.Marshal(result)
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "report receipt result could not be encoded", err)
	}
	record := receipt.Record{
		ID: receiptID,
		Key: receipt.Key{
			TenantID:    command.Scope.TenantID,
			ProjectID:   command.Scope.ProjectID,
			PrincipalID: id.PrincipalID(command.Service.ID),
			Operation:   string(operationResourceReport),
			CallerKey:   command.SourceEventID,
		},
		ContentHash: fingerprint,
		Result:      resultBody,
		At:          at,
	}
	if err := record.Key.Validate(); err != nil {
		return receipt.Record{}, err
	}
	return record, nil
}

// validateReportCommand checks the report shape before any dependency is
// touched. The reporting service is an already-authenticated service principal;
// the command has no credential or caller-supplied causation.
func validateReportCommand(command ReportCommand) error {
	if err := command.Scope.Validate(); err != nil {
		return err
	}
	if !command.Scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "report scope must be project-scoped")
	}
	if err := command.Service.Validate(); err != nil {
		return err
	}
	if command.Service.Kind != identity.KindService {
		return fault.New(fault.InvalidInput, "reporting principal is not a service account")
	}
	if !command.Service.Enabled {
		return fault.New(fault.Forbidden, "reporting service is disabled")
	}
	if !scopeContains(command.Service.Scope, command.Scope) {
		return fault.New(fault.Forbidden, "reporting service is outside the report scope")
	}
	if command.SourceEventID == "" || len(command.SourceEventID) > id.MaxIDLength {
		return fault.New(fault.InvalidInput, "report source event id is invalid")
	}
	if _, err := resource.ParseResourceID(string(command.ResourceID)); err != nil {
		return err
	}
	if _, err := resource.ParseOperation(string(command.Operation)); err != nil {
		return err
	}
	if _, err := ParseVerificationID(string(command.VerificationID)); err != nil {
		return err
	}
	if !command.EventType.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("invalid reportable event type %q", command.EventType))
	}
	if err := validateReportSubject(command.Subject); err != nil {
		return err
	}
	if len(command.Data) > event.DefaultBounds.MaxDataBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf(
			"report data is %d bytes; at most %d are allowed", len(command.Data), event.DefaultBounds.MaxDataBytes))
	}
	return nil
}

// validateReportSubject applies the declared typed-subject bounds to the
// report's effect fields.
func validateReportSubject(subject Subject) error {
	fields := make([]api.SubjectField, 0, len(subject.Fields))
	for _, field := range subject.Fields {
		fields = append(fields, api.SubjectField{Name: field.Name, Kind: string(field.Kind), Value: field.Value})
	}
	if err := (api.Subject{Fields: fields}).Validate(); err != nil {
		return fault.Wrap(fault.InvalidInput, "report subject is invalid", err)
	}
	return nil
}

// validateReportEventType accepts a syntactically usable, binding-declared
// report type that is not a lifecycle type. Lifecycle names and aliases can
// never impersonate a trusted report type.
func validateReportEventType(reportType event.ReportType) error {
	if !reportType.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("invalid reportable event type %q", reportType))
	}
	if _, lifecycle := api.CanonicalEventType(string(reportType)); lifecycle {
		return fault.New(fault.InvalidInput, fmt.Sprintf("report type %q is a lifecycle type", reportType))
	}
	return nil
}

// validateReportBinding requires the current binding to be the exact resource
// and enforcing service the report names, at a current version.
func validateReportBinding(command ReportCommand, binding resource.Binding) error {
	if err := binding.Validate(); err != nil {
		return fault.Wrap(fault.InternalFailure, "report binding is invalid", err)
	}
	if binding.Version == 0 {
		return fault.New(fault.InternalFailure, "report binding has no current version")
	}
	if binding.Scope != command.Scope {
		return fault.New(fault.Forbidden, "binding is outside the report scope")
	}
	if binding.ResourceID != command.ResourceID {
		return fault.New(fault.Forbidden, "binding does not cover the reported resource")
	}
	if binding.EnforcingService != service.ServiceID(command.Service.ID) {
		return fault.New(fault.Forbidden, "binding does not authorize the reporting service")
	}
	if !declaresOperation(binding, command.Operation) {
		return fault.New(fault.Forbidden, "binding does not declare the reported operation")
	}
	return nil
}

// validateVerificationLinkage requires the cited verification fact to belong to
// this service and binding and to match the reported resource, operation and
// effect fields. The verification actor's current enablement is deliberately
// not re-read: the record is a fact about an already-accepted effect, and
// reporting grants no new actor authority.
func validateVerificationLinkage(command ReportCommand, binding resource.Binding, record Record) error {
	if record.ID != command.VerificationID {
		return fault.New(fault.Forbidden, "verification record does not match the cited verification")
	}
	if record.Result != ResultApproved {
		return fault.New(fault.Forbidden, "verification was not approved")
	}
	if err := record.Actor.Validate(); err != nil {
		return fault.Wrap(fault.InternalFailure, "verification record actor is invalid", err)
	}
	if record.Scope != command.Scope {
		return fault.New(fault.Forbidden, "verification is outside the report scope")
	}
	if record.CallingService != service.ServiceID(command.Service.ID) {
		return fault.New(fault.Forbidden, "verification belongs to a different service")
	}
	if record.BindingID != binding.ID {
		return fault.New(fault.Forbidden, "verification belongs to a different binding")
	}
	if record.ResourceID != command.ResourceID {
		return fault.New(fault.Forbidden, "verification covers a different resource")
	}
	if record.Operation != string(command.Operation) {
		return fault.New(fault.Forbidden, "verification covers a different operation")
	}
	if record.Operation == string(resource.OperationGitRead) {
		// A repository read check verifies access, not an effect. It must never
		// stand behind a verified resource report, even when the binding lists
		// the operation and the empty read subject matches.
		return fault.New(fault.Forbidden, "a git.read verification is not reportable")
	}
	if !sameSubject(command.Subject, record.Subject) {
		return fault.New(fault.Forbidden, "report subject does not match the verification record")
	}
	return nil
}

// sameSubject requires the report and the verification record to carry exactly
// the same typed field set: a report may not add or drop a verified effect
// field.
func sameSubject(left, right Subject) bool {
	return SubjectMatches(left, right) && SubjectMatches(right, left)
}

// reportAuthorityReads builds the resource.report current-authority reads in
// registry order. The adapter re-evaluates them inside the report commit, so a
// grant, binding version or resource ownership observed here is never durable
// permission.
func reportAuthorityReads(
	operation authorize.Operation,
	scope tenancy.Scope,
	principal id.PrincipalID,
	binding resource.Binding,
	resourceID resource.ResourceID,
) []authorize.AuthorityRead {
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principal,
		}
		switch check {
		case authorize.CheckBindingVersion:
			read.TargetID = string(binding.ID)
			read.ExpectedVersion = binding.Version
		case authorize.CheckResourceOwnership:
			read.TargetID = string(resourceID)
		}
		reads = append(reads, read)
	}
	return reads
}

// reportDependencyError keeps a declared fault class from a dependency and
// wraps an unclassified failure as an internal failure with a sanitized
// message. The cause stays reachable through errors.Is/errors.As but is never
// part of Error().
func reportDependencyError(err error, message string) error {
	if err == nil {
		return nil
	}
	if _, ok := fault.ClassOf(err); ok {
		return err
	}
	return fault.Wrap(fault.InternalFailure, message, err)
}

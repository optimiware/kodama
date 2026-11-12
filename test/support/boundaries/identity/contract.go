package identity

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
)

// ServiceAccount is one fixture service credential. Token is the opaque bearer
// value the contract server authenticates as the calling service.
type ServiceAccount struct {
	ID        string
	Token     string
	TenantID  string
	ProjectID string
	Enabled   bool
	Roles     []string
}

// ExecutionAccount is one fixture mounted execution credential.
type ExecutionAccount struct {
	ID        string
	Token     string
	TenantID  string
	ProjectID string
	Enabled   bool
}

// Binding is one fixture resource binding authorizing a calling service.
type Binding struct {
	ID               string
	ServiceID        string
	TenantID         string
	ProjectID        string
	ResourceID       string
	Operations       []string
	ReportableEvents []string
}

// ApprovalFact is one fixture same-execution human-role approval record.
type ApprovalFact struct {
	ExecutionID string
	Assignment  string
	ResourceID  string
	Operation   string
	Subject     api.Subject
	Decision    string
}

// ContractOptions configures one contract verification/report fixture.
type ContractOptions struct {
	Clock      *Clock
	OIDC       *Provider
	Services   []ServiceAccount
	Executions []ExecutionAccount
	Bindings   []Binding
	// ActorRoles maps a verified human principal id to its current roles.
	ActorRoles map[string][]string
	Approvals  []ApprovalFact
}

// FailureMode selects one controllable contract-server failure.
type FailureMode int

const (
	// FailureNone means no injected failure.
	FailureNone FailureMode = iota
	// FailureStatus returns one public error status before any effect.
	FailureStatus
	// FailureDelay sleeps before the response.
	FailureDelay
	// FailureDropAfterCommit commits then closes the connection without a
	// response.
	FailureDropAfterCommit
	// FailureExtraField returns an otherwise valid success envelope carrying an
	// unknown field, which a strict client must refuse.
	FailureExtraField
)

// Failure is one queued contract-server failure.
type Failure struct {
	Mode   FailureMode
	Code   string
	Delay  time.Duration
	Status int
}

// RecordedRequest is one safe record of a contract-server request. It never
// carries an Authorization header, actor credential, service credential or
// request body.
type RecordedRequest struct {
	Method            string
	Path              string
	Operation         string
	ServiceID         string
	ActorKind         string
	ActorID           string
	ResourceID        string
	ResourceOperation string
	IdempotencyKey    string
	Status            int
}

type verificationRecord struct {
	ID         string
	ServiceID  string
	BindingID  string
	ActorID    string
	ActorKind  string
	TenantID   string
	ProjectID  string
	ResourceID string
	Operation  string
	Subject    api.Subject
	Approval   string
	Approved   bool
	Roles      []string
	At         time.Time
}

type reportRecord struct {
	Fingerprint string
	Request     api.ReportRequest
	Response    api.ReportResponse
}

// reportKey scopes an idempotency key to the authenticated calling principal.
// Two services that happen to choose the same key never observe each other's
// committed report or replay flag.
type reportKey struct {
	PrincipalID string
	Key         string
}

type caller struct {
	Kind      string
	ID        string
	TenantID  string
	ProjectID string
	Enabled   bool
	Roles     []string
}

// ContractServer is a fixture of the public verification/report facts.
//
// It is not the Shell composition and does not claim full approval proof. It
// authenticates the calling service, independently authenticates the forwarded
// actor credential (human OIDC token or fixture execution token), applies the
// configured fixture binding/approval facts, returns deterministic verification
// ids and implements report replay by idempotency key.
type ContractServer struct {
	clock    *Clock
	oidc     *Provider
	verifier shellidentity.OIDCVerifier
	server   *httptest.Server

	mu             sync.Mutex
	services       []ServiceAccount
	executions     []ExecutionAccount
	bindings       []Binding
	actorRoles     map[string][]string
	approvals      []ApprovalFact
	verifications  map[string]verificationRecord
	reports        map[reportKey]reportRecord
	events         []string
	nextVerifyID   int
	nextEventID    int
	verifyFailures []Failure
	reportFailures []Failure
	recorded       []RecordedRequest
	logs           []string
	closed         bool
}

// NewContractServer starts one loopback TLS contract fixture.
func NewContractServer(options ContractOptions) (*ContractServer, error) {
	if options.OIDC == nil {
		return nil, fmt.Errorf("contract fixture requires the OIDC fixture")
	}
	clock := options.Clock
	if clock == nil {
		clock = options.OIDC.clock
	}
	verifier, err := options.OIDC.NewVerifier()
	if err != nil {
		return nil, err
	}
	server := &ContractServer{
		clock:         clock,
		oidc:          options.OIDC,
		verifier:      verifier,
		services:      append([]ServiceAccount(nil), options.Services...),
		executions:    append([]ExecutionAccount(nil), options.Executions...),
		bindings:      append([]Binding(nil), options.Bindings...),
		actorRoles:    make(map[string][]string, len(options.ActorRoles)),
		approvals:     append([]ApprovalFact(nil), options.Approvals...),
		verifications: make(map[string]verificationRecord),
		reports:       make(map[reportKey]reportRecord),
	}
	for principal, roles := range options.ActorRoles {
		server.actorRoles[principal] = append([]string(nil), roles...)
	}
	if err := server.validateAccounts(); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/verify", server.handleVerify)
	mux.HandleFunc("/api/v1/reports", server.handleReport)
	server.server = httptest.NewTLSServer(mux)
	return server, nil
}

func (server *ContractServer) validateAccounts() error {
	seen := make(map[string]string)
	for _, service := range server.services {
		if service.ID == "" || service.Token == "" {
			return fmt.Errorf("fixture service id and token are required")
		}
		if previous, duplicate := seen[service.Token]; duplicate {
			return fmt.Errorf("fixture credential is shared by %q and %q", previous, service.ID)
		}
		seen[service.Token] = service.ID
	}
	for _, execution := range server.executions {
		if execution.ID == "" || execution.Token == "" {
			return fmt.Errorf("fixture execution id and token are required")
		}
		if previous, duplicate := seen[execution.Token]; duplicate {
			return fmt.Errorf("fixture credential is shared by %q and %q", previous, execution.ID)
		}
		seen[execution.Token] = execution.ID
	}
	return nil
}

// Close stops the fixture server.
func (server *ContractServer) Close() {
	if server == nil || server.server == nil {
		return
	}
	server.mu.Lock()
	alreadyClosed := server.closed
	server.closed = true
	server.mu.Unlock()
	if !alreadyClosed {
		server.server.Close()
	}
}

// URL is the HTTPS base URL of the fixture.
func (server *ContractServer) URL() string { return server.server.URL }

// Client returns the TLS client that trusts the fixture certificate.
func (server *ContractServer) Client() *http.Client { return server.server.Client() }

// QueueVerifyFailure queues one failure for the next actor.verify request.
func (server *ContractServer) QueueVerifyFailure(failure Failure) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.verifyFailures = append(server.verifyFailures, failure)
}

// QueueReportFailure queues one failure for the next resource.report request.
func (server *ContractServer) QueueReportFailure(failure Failure) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.reportFailures = append(server.reportFailures, failure)
}

// Recorded returns safe records of every contract request.
func (server *ContractServer) Recorded() []RecordedRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]RecordedRequest(nil), server.recorded...)
}

// RecordedJSON renders the safe records for a redaction scan.
func (server *ContractServer) RecordedJSON() string {
	data, _ := json.Marshal(server.Recorded())
	return string(data)
}

// LogText returns the fixture's safe log lines.
func (server *ContractServer) LogText() string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return strings.Join(server.logs, "\n")
}

// EventCount returns the number of accepted verified reports.
func (server *ContractServer) EventCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.events)
}

// EventIDs returns the accepted event ids in commit order.
func (server *ContractServer) EventIDs() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.events...)
}

// VerificationCount returns the number of committed verification records.
func (server *ContractServer) VerificationCount() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.verifications)
}

func (server *ContractServer) record(entry RecordedRequest) {
	server.recorded = append(server.recorded, entry)
	server.logs = append(server.logs, fmt.Sprintf(
		"%s %s operation=%s service=%s actor=%s:%s resource=%s resource_operation=%s key=%s status=%d",
		entry.Method, entry.Path, entry.Operation, entry.ServiceID, entry.ActorKind, entry.ActorID,
		entry.ResourceID, entry.ResourceOperation, entry.IdempotencyKey, entry.Status,
	))
}

func (server *ContractServer) nextVerifyFailure() Failure {
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.verifyFailures) == 0 {
		return Failure{}
	}
	failure := server.verifyFailures[0]
	server.verifyFailures = server.verifyFailures[1:]
	return failure
}

func (server *ContractServer) nextReportFailure() Failure {
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.reportFailures) == 0 {
		return Failure{}
	}
	failure := server.reportFailures[0]
	server.reportFailures = server.reportFailures[1:]
	return failure
}

func (server *ContractServer) handleVerify(writer http.ResponseWriter, request *http.Request) {
	entry := RecordedRequest{Method: request.Method, Path: request.URL.Path, Operation: "actor.verify"}
	status := server.serveVerify(writer, request, &entry)
	entry.Status = status
	server.mu.Lock()
	server.record(entry)
	server.mu.Unlock()
}

func (server *ContractServer) serveVerify(writer http.ResponseWriter, request *http.Request, entry *RecordedRequest) int {
	callerPrincipal, status, code, message := server.authenticateCaller(request)
	if status != http.StatusOK {
		return writePublicError(writer, code, message)
	}
	entry.ServiceID = callerPrincipal.ID
	if callerPrincipal.Kind != "service" {
		return writePublicError(writer, api.CodeForbidden, "only an authenticated service may call verification")
	}
	if !callerPrincipal.Enabled {
		return writePublicError(writer, api.CodeForbidden, "the calling service is disabled")
	}
	if !requireJSONContentType(request) {
		return writePublicError(writer, api.CodeInvalidInput, "the request content type must be application/json")
	}

	envelope, err := decodeEnvelope(request)
	if err != nil {
		return writePublicError(writer, api.CodeInvalidInput, "the request envelope is invalid")
	}
	verifyRequest, err := api.DecodeVerifyRequest(envelope.Input)
	if err != nil {
		return writePublicError(writer, api.CodeInvalidInput, publicMessage(err))
	}
	entry.ResourceID = verifyRequest.ResourceID
	entry.ResourceOperation = verifyRequest.Operation

	failure := server.nextVerifyFailure()
	switch failure.Mode {
	case FailureStatus:
		return writePublicError(writer, failureCode(failure), "the verification fixture is unavailable")
	case FailureDelay:
		if failure.Delay > 0 {
			time.Sleep(failure.Delay)
		}
	}

	actor, err := server.resolveActor(request.Context(), verifyRequest.ActorCredential)
	if err != nil {
		return writePublicError(writer, api.CodeUnauthenticated, "the actor credential is not verifiable")
	}
	entry.ActorKind = actor.Kind
	entry.ActorID = actor.ID
	if !actor.Enabled {
		return writePublicError(writer, api.CodeForbidden, "the actor is disabled")
	}
	if !scopeContains(actor, callerPrincipal) {
		return writePublicError(writer, api.CodeForbidden, "the actor is outside the verification scope")
	}
	binding, ok := server.bindingFor(callerPrincipal, verifyRequest.ResourceID, verifyRequest.Operation)
	if !ok {
		return writePublicError(writer, api.CodeForbidden, "the binding does not authorize the calling service")
	}

	approved := false
	if verifyRequest.Approval != "" {
		if actor.Kind != "execution" {
			return writePublicError(writer, api.CodeForbidden, "approval evidence requires an execution actor")
		}
		approved = server.approvalMatches(actor.ID, verifyRequest)
		if !approved {
			return writePublicError(writer, api.CodeForbidden, "no matching approval evidence")
		}
	}

	server.mu.Lock()
	server.nextVerifyID++
	verificationID := fmt.Sprintf("verification-%04d", server.nextVerifyID)
	record := verificationRecord{
		ID:         verificationID,
		ServiceID:  callerPrincipal.ID,
		BindingID:  binding.ID,
		ActorID:    actor.ID,
		ActorKind:  actor.Kind,
		TenantID:   callerPrincipal.TenantID,
		ProjectID:  callerPrincipal.ProjectID,
		ResourceID: verifyRequest.ResourceID,
		Operation:  verifyRequest.Operation,
		Subject:    verifyRequest.Subject,
		Approval:   verifyRequest.Approval,
		Approved:   approved,
		Roles:      append([]string(nil), actor.Roles...),
		At:         server.clock.Now(),
	}
	server.verifications[verificationID] = record
	server.mu.Unlock()

	response := api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: actor.ID, Kind: actor.Kind},
		Roles:          append([]string(nil), actor.Roles...),
		Approval:       approved,
		VerificationID: verificationID,
	}
	if failure.Mode == FailureDropAfterCommit {
		dropConnection(writer)
		return 0
	}
	if failure.Mode == FailureExtraField {
		return writeCorruptResult(writer, "actor.verify", response)
	}
	return writeOperationResult(writer, "actor.verify", response)
}

func (server *ContractServer) handleReport(writer http.ResponseWriter, request *http.Request) {
	entry := RecordedRequest{Method: request.Method, Path: request.URL.Path, Operation: "resource.report"}
	status := server.serveReport(writer, request, &entry)
	entry.Status = status
	server.mu.Lock()
	server.record(entry)
	server.mu.Unlock()
}

func (server *ContractServer) serveReport(writer http.ResponseWriter, request *http.Request, entry *RecordedRequest) int {
	callerPrincipal, status, code, message := server.authenticateCaller(request)
	if status != http.StatusOK {
		return writePublicError(writer, code, message)
	}
	entry.ServiceID = callerPrincipal.ID
	if callerPrincipal.Kind != "service" {
		return writePublicError(writer, api.CodeForbidden, "only an authenticated service may report a verified resource")
	}
	if !callerPrincipal.Enabled {
		return writePublicError(writer, api.CodeForbidden, "the calling service is disabled")
	}
	if !requireJSONContentType(request) {
		return writePublicError(writer, api.CodeInvalidInput, "the request content type must be application/json")
	}

	envelope, err := decodeEnvelope(request)
	if err != nil {
		return writePublicError(writer, api.CodeInvalidInput, "the request envelope is invalid")
	}
	if envelope.IdempotencyKey == "" {
		return writePublicError(writer, api.CodeInvalidInput, "resource.report requires an idempotency key")
	}
	entry.IdempotencyKey = envelope.IdempotencyKey
	reportRequest, err := api.DecodeReportRequest(envelope.Input)
	if err != nil {
		return writePublicError(writer, api.CodeInvalidInput, publicMessage(err))
	}
	entry.ResourceID = reportRequest.ResourceID
	entry.ResourceOperation = reportRequest.Operation

	fingerprint, err := reportFingerprint(reportRequest)
	if err != nil {
		return writePublicError(writer, api.CodeInternalFailure, "the report content cannot be fingerprinted")
	}

	// Replay is decided before any injected failure so a retry of the exact
	// same report always observes the committed result. The key is scoped to
	// the authenticated caller so one principal cannot replay or probe another
	// principal's report by reusing its idempotency key.
	key := reportKey{PrincipalID: callerPrincipal.ID, Key: envelope.IdempotencyKey}
	server.mu.Lock()
	previous, replayed := server.reports[key]
	server.mu.Unlock()
	if replayed {
		if previous.Fingerprint != fingerprint {
			return writePublicError(writer, api.CodeChangedIdempotencyContent, "the idempotency key was used for different report content")
		}
		response := previous.Response
		response.Replayed = true
		return writeOperationResult(writer, "resource.report", response)
	}

	failure := server.nextReportFailure()
	switch failure.Mode {
	case FailureStatus:
		return writePublicError(writer, failureCode(failure), "the report fixture is unavailable")
	case FailureDelay:
		if failure.Delay > 0 {
			time.Sleep(failure.Delay)
		}
	}

	binding, ok := server.bindingFor(callerPrincipal, reportRequest.ResourceID, reportRequest.Operation)
	if !ok {
		return writePublicError(writer, api.CodeForbidden, "the binding does not authorize the calling service")
	}
	if !declaresEvent(binding, reportRequest.EventType) {
		return writePublicError(writer, api.CodeForbidden, "the binding does not declare the reported event type")
	}
	server.mu.Lock()
	verification, ok := server.verifications[reportRequest.VerificationID]
	if !ok || verification.ServiceID != callerPrincipal.ID ||
		verification.BindingID != binding.ID ||
		verification.TenantID != callerPrincipal.TenantID ||
		verification.ProjectID != callerPrincipal.ProjectID ||
		verification.ResourceID != reportRequest.ResourceID ||
		verification.Operation != reportRequest.Operation ||
		verification.Operation == string(api.ResourceOperationGitRead) ||
		!subjectsEqual(verification.Subject, reportRequest.Subject) {
		server.mu.Unlock()
		return writePublicError(writer, api.CodeForbidden, "the report does not match the cited verification record")
	}

	// Re-check replay under the commit lock so two concurrent retries of the
	// same key produce one accepted event.
	if concurrent, exists := server.reports[key]; exists {
		server.mu.Unlock()
		if concurrent.Fingerprint != fingerprint {
			return writePublicError(writer, api.CodeChangedIdempotencyContent, "the idempotency key was used for different report content")
		}
		response := concurrent.Response
		response.Replayed = true
		return writeOperationResult(writer, "resource.report", response)
	}

	server.nextEventID++
	eventID := fmt.Sprintf("event-%04d", server.nextEventID)
	server.events = append(server.events, eventID)
	response := api.ReportResponse{VerificationID: reportRequest.VerificationID, EventID: eventID}
	server.reports[key] = reportRecord{
		Fingerprint: fingerprint,
		Request:     reportRequest,
		Response:    response,
	}
	server.mu.Unlock()

	if failure.Mode == FailureDropAfterCommit {
		dropConnection(writer)
		return 0
	}
	if failure.Mode == FailureExtraField {
		return writeCorruptResult(writer, "resource.report", response)
	}
	return writeOperationResult(writer, "resource.report", response)
}

func (server *ContractServer) authenticateCaller(request *http.Request) (caller, int, string, string) {
	header := request.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return caller{}, http.StatusUnauthorized, api.CodeUnauthenticated, "the calling service is not authenticated"
	}
	server.mu.Lock()
	services := append([]ServiceAccount(nil), server.services...)
	executions := append([]ExecutionAccount(nil), server.executions...)
	server.mu.Unlock()
	for _, service := range services {
		if constantTimeEqual(service.Token, token) {
			return caller{Kind: "service", ID: service.ID, TenantID: service.TenantID, ProjectID: service.ProjectID, Enabled: service.Enabled, Roles: append([]string(nil), service.Roles...)}, http.StatusOK, "", ""
		}
	}
	for _, execution := range executions {
		if constantTimeEqual(execution.Token, token) {
			return caller{Kind: "execution", ID: execution.ID, TenantID: execution.TenantID, ProjectID: execution.ProjectID, Enabled: execution.Enabled}, http.StatusOK, "", ""
		}
	}
	if _, err := server.verifier.Verify(request.Context(), shellidentity.OIDCRequest{AccessToken: token, Now: server.clock.Now()}); err == nil {
		return caller{Kind: "human"}, http.StatusOK, "", ""
	}
	return caller{}, http.StatusUnauthorized, api.CodeUnauthenticated, "the calling service is not authenticated"
}

func (server *ContractServer) resolveActor(ctx context.Context, credential string) (caller, error) {
	server.mu.Lock()
	services := append([]ServiceAccount(nil), server.services...)
	executions := append([]ExecutionAccount(nil), server.executions...)
	server.mu.Unlock()
	for _, execution := range executions {
		if constantTimeEqual(execution.Token, credential) {
			return caller{Kind: "execution", ID: execution.ID, TenantID: execution.TenantID, ProjectID: execution.ProjectID, Enabled: execution.Enabled}, nil
		}
	}
	for _, service := range services {
		if constantTimeEqual(service.Token, credential) {
			return caller{}, fmt.Errorf("a service credential is not an actor credential")
		}
	}
	principal, err := server.verifier.Verify(ctx, shellidentity.OIDCRequest{AccessToken: credential, Now: server.clock.Now()})
	if err != nil {
		return caller{}, err
	}
	roles := append([]string(nil), server.actorRoles[string(principal.ID)]...)
	return caller{
		Kind:      "human",
		ID:        string(principal.ID),
		TenantID:  string(principal.Scope.TenantID),
		ProjectID: string(principal.Scope.ProjectID),
		Enabled:   principal.Enabled,
		Roles:     roles,
	}, nil
}

func (server *ContractServer) bindingFor(callerPrincipal caller, resourceID, operation string) (Binding, bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, binding := range server.bindings {
		if binding.ServiceID != callerPrincipal.ID ||
			binding.TenantID != callerPrincipal.TenantID ||
			binding.ProjectID != callerPrincipal.ProjectID ||
			binding.ResourceID != resourceID {
			continue
		}
		if slices.Contains(binding.Operations, operation) {
			return binding, true
		}
	}
	return Binding{}, false
}

func (server *ContractServer) approvalMatches(executionID string, request api.VerifyRequest) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, fact := range server.approvals {
		if fact.ExecutionID != executionID || fact.Assignment != request.Approval ||
			fact.ResourceID != request.ResourceID || fact.Operation != request.Operation ||
			fact.Decision != "pass" {
			continue
		}
		if subjectSatisfies(request.Subject, fact.Subject) {
			return true
		}
	}
	return false
}

func scopeContains(actor, target caller) bool {
	if actor.TenantID != target.TenantID {
		return false
	}
	return actor.ProjectID == "" || actor.ProjectID == target.ProjectID
}

func declaresEvent(binding Binding, eventType string) bool {
	return slices.Contains(binding.ReportableEvents, eventType)
}

func constantTimeEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func decodeEnvelope(request *http.Request) (api.Request, error) {
	data, err := io.ReadAll(io.LimitReader(request.Body, api.MaxRequestBytes+1))
	if err != nil {
		return api.Request{}, err
	}
	if len(data) > api.MaxRequestBytes {
		return api.Request{}, fmt.Errorf("request body is oversized")
	}
	return api.DecodeRequest(data)
}

func reportFingerprint(request api.ReportRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func subjectSatisfies(required, recorded api.Subject) bool {
	recordedFields, ok := subjectFieldMap(recorded)
	if !ok {
		return false
	}
	requiredFields, ok := subjectFieldMap(required)
	if !ok {
		return false
	}
	for name, want := range requiredFields {
		got, present := recordedFields[name]
		if !present || got.Kind != want.Kind || got.Value != want.Value {
			return false
		}
	}
	return true
}

func subjectsEqual(left, right api.Subject) bool {
	leftFields, ok := subjectFieldMap(left)
	if !ok {
		return false
	}
	rightFields, ok := subjectFieldMap(right)
	if !ok || len(leftFields) != len(rightFields) {
		return false
	}
	for name, want := range leftFields {
		got, present := rightFields[name]
		if !present || got.Kind != want.Kind || got.Value != want.Value {
			return false
		}
	}
	return true
}

func subjectFieldMap(subject api.Subject) (map[string]api.SubjectField, bool) {
	fields := make(map[string]api.SubjectField, len(subject.Fields))
	for _, field := range subject.Fields {
		if field.Name == "" {
			return nil, false
		}
		if _, duplicate := fields[field.Name]; duplicate {
			return nil, false
		}
		fields[field.Name] = field
	}
	return fields, true
}

func failureCode(failure Failure) string {
	if failure.Code != "" {
		return failure.Code
	}
	if failure.Status != 0 {
		for _, code := range api.ErrorCodes() {
			if status, ok := api.HTTPStatus(code); ok && status == failure.Status {
				return code
			}
		}
	}
	return api.CodeDependencyUnavailable
}

func publicMessage(err error) string {
	if public, ok := errors.AsType[*api.Error](err); ok {
		return public.Message
	}
	return "the request is invalid"
}

func writePublicError(writer http.ResponseWriter, code, message string) int {
	status, ok := api.HTTPStatus(code)
	if !ok {
		status = http.StatusInternalServerError
		code = api.CodeInternalFailure
		message = "the fixture returned an internal failure"
	}
	writeJSON(writer, status, api.Error{Code: code, Message: message})
	return status
}

func writeOperationResult(writer http.ResponseWriter, operation string, response any) int {
	codec, ok := codecFor(operation)
	if !ok {
		return writePublicError(writer, api.CodeInternalFailure, "the fixture has no codec for the operation")
	}
	body, err := codec.EncodeResult(response)
	if err != nil {
		return writePublicError(writer, api.CodeInternalFailure, "the fixture could not encode the result")
	}
	result := api.Result{Operation: operation, Body: body}
	if err := result.Validate(); err != nil {
		return writePublicError(writer, api.CodeInternalFailure, "the fixture produced an invalid result")
	}
	writeJSON(writer, api.HTTPStatusSuccess, result)
	return api.HTTPStatusSuccess
}

func writeCorruptResult(writer http.ResponseWriter, operation string, response any) int {
	codec, ok := codecFor(operation)
	if !ok {
		return writePublicError(writer, api.CodeInternalFailure, "the fixture has no codec for the operation")
	}
	body, err := codec.EncodeResult(response)
	if err != nil {
		return writePublicError(writer, api.CodeInternalFailure, "the fixture could not encode the result")
	}
	raw, err := json.Marshal(api.Result{Operation: operation, Body: body})
	if err != nil {
		return writePublicError(writer, api.CodeInternalFailure, "the fixture could not encode the result")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return writePublicError(writer, api.CodeInternalFailure, "the fixture could not encode the result")
	}
	object["unexpected_member"] = json.RawMessage(`"corrupt"`)
	writeJSON(writer, api.HTTPStatusSuccess, object)
	return api.HTTPStatusSuccess
}

func codecFor(operation string) (api.OperationCodec, bool) {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec, true
		}
	}
	return api.OperationCodec{}, false
}

func dropConnection(writer http.ResponseWriter) {
	if hijacker, ok := writer.(http.Hijacker); ok {
		connection, _, err := hijacker.Hijack()
		if err == nil {
			_ = connection.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

package runtimev2

import (
	"net/http"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Error codes new in v2. Every v1 code keeps its meaning and status.
const (
	// CodeContextVersionMismatch: a v2 attempt reached a v1 route or the reverse;
	// no side effects.
	CodeContextVersionMismatch domain.ErrorCode = "context_version_mismatch"
	// CodeReceiptConflict: a different preparation receipt already exists.
	CodeReceiptConflict domain.ErrorCode = "receipt_conflict"
	// CodeResourceUnbound: the resource's workload UID has not been bound by
	// execution yet; the caller retries later.
	CodeResourceUnbound domain.ErrorCode = "resource_unbound"
	// CodeResourceRevoked: a resource token after stop, revocation or deadline.
	CodeResourceRevoked domain.ErrorCode = "resource_revoked"
)

var newStatuses = map[domain.ErrorCode]int{
	CodeContextVersionMismatch: http.StatusConflict,
	CodeReceiptConflict:        http.StatusConflict,
	CodeResourceUnbound:        http.StatusConflict,
	CodeResourceRevoked:        http.StatusForbidden,
}

// Status returns the HTTP status of a code and whether the code is known to v2.
func Status(code domain.ErrorCode) (int, bool) {
	if s, ok := newStatuses[code]; ok {
		return s, true
	}
	return runtimev1.Status(code)
}

// ContractError is the v1 contract violation type, reused so a consumer handles
// one error type for both versions.
type ContractError = runtimev1.ContractError

// ValidationError is the v1 validation error type.
type ValidationError = runtimev1.ValidationError

// Subject is the kind of runtime principal a route family serves.
type Subject string

const (
	SubjectAttempt  Subject = "attempt"
	SubjectResource Subject = "resource"
)

// Audience returns the token audience of the subject.
func (s Subject) Audience() string {
	if s == SubjectResource {
		return ResourceAudience
	}
	return AttemptAudience
}

// CheckAudience is the pure audience rule of the route families: an attempt
// token never works on a resource route and a resource token never works on an
// attempt route; the audiences differ and the mismatch is forbidden. It decides
// nothing about whether a token is genuine or current.
func CheckAudience(route Subject, tokenAudience string) error {
	if route != SubjectAttempt && route != SubjectResource {
		return &ContractError{Code: domain.CodeForbidden, Message: "unknown route family"}
	}
	if tokenAudience != route.Audience() {
		return &ContractError{Code: domain.CodeForbidden, Message: "the token is not valid for this route family"}
	}
	return nil
}

// Operation describes one v2 route. Path is relative to the subject's base path.
// Request and the Responses values name DTO types ("" means no body). Errors lists
// the codes the route may return. Kinds restricts a resource route to the
// resource kinds that may call it.
type Operation struct {
	ID        string
	Subject   Subject
	Method    string
	Path      string
	Request   string
	Responses map[int]string
	Errors    []domain.ErrorCode
	Kinds     []ResourceKind
}

var attemptAuthErrors = []domain.ErrorCode{
	domain.CodeInvalidCredential, runtimev1.CodeAttemptRevoked, CodeContextVersionMismatch, domain.CodeUnavailable, domain.CodeRateLimited,
}

var resourceAuthErrors = []domain.ErrorCode{
	domain.CodeInvalidCredential, domain.CodeForbidden, CodeResourceRevoked, CodeResourceUnbound, CodeContextVersionMismatch,
	domain.CodeUnavailable, domain.CodeRateLimited,
}

func attemptErrs(extra ...domain.ErrorCode) []domain.ErrorCode {
	return append(append([]domain.ErrorCode{}, attemptAuthErrors...), extra...)
}

func resourceErrs(extra ...domain.ErrorCode) []domain.ErrorCode {
	return append(append([]domain.ErrorCode{}, resourceAuthErrors...), extra...)
}

// Operations is the complete v2 route table. It is the source the T-116
// integration generates the public OpenAPI fragment from; api/runtime.yaml is
// the frozen v1 table and does not contain these routes.
var Operations = []Operation{
	{ID: "admit", Subject: SubjectAttempt, Method: "POST", Path: "/admit", Request: "AdmitRequest", Responses: map[int]string{200: "AdmitResponseV2"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeInvalidCredential, domain.CodeForbidden, runtimev1.CodeAttemptRevoked,
			runtimev1.CodeAdmissionConsumed, runtimev1.CodeProfileUnsupported, CodeContextVersionMismatch, domain.CodeUnavailable, domain.CodeRateLimited}},
	{ID: "renewToken", Subject: SubjectAttempt, Method: "POST", Path: "/token", Request: "TokenRequest", Responses: map[int]string{200: "TokenResponse"}, Errors: attemptErrs(domain.CodeInvalidRequest)},
	{ID: "getContext", Subject: SubjectAttempt, Method: "GET", Path: "/context", Responses: map[int]string{200: "ContextV2"}, Errors: attemptErrs()},
	{ID: "begin", Subject: SubjectAttempt, Method: "POST", Path: "/begin", Request: "BeginRequest", Responses: map[int]string{200: "BeginResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeIdempotencyConflict)},
	{ID: "resolveSecret", Subject: SubjectAttempt, Method: "POST", Path: "/secrets/resolve", Request: "SecretResolveRequest", Responses: map[int]string{200: "SecretResolveResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound)},
	{ID: "reserveArtifact", Subject: SubjectAttempt, Method: "POST", Path: "/artifacts", Request: "ArtifactReserveRequest",
		Responses: map[int]string{200: "ArtifactReserveResponse", 201: "ArtifactReserveResponse"},
		Errors:    attemptErrs(domain.CodeInvalidRequest, domain.CodeIdempotencyConflict, runtimev1.CodeOutputInvalid)},
	{ID: "completeArtifact", Subject: SubjectAttempt, Method: "POST", Path: "/artifacts/{artifactId}/complete", Request: "ArtifactCompleteRequest", Responses: map[int]string{200: "ArtifactCompleteResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeNotFound, runtimev1.CodeArtifactIncomplete, runtimev1.CodeDigestMismatch)},
	{ID: "proposeSource", Subject: SubjectAttempt, Method: "POST", Path: "/source", Request: "SourceProposalRequest",
		Responses: map[int]string{200: "SourceProposalResponse", 202: "SourceProposalResponse"},
		Errors:    attemptErrs(domain.CodeInvalidRequest, domain.CodeNotFound, runtimev1.CodeArtifactIncomplete, runtimev1.CodePublicationConflict)},
	{ID: "submitResult", Subject: SubjectAttempt, Method: "POST", Path: "/result", Request: "ResultRequest", Responses: map[int]string{200: "ResultResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeNotFound, runtimev1.CodeArtifactIncomplete, runtimev1.CodeOutputInvalid, runtimev1.CodeResultHashMismatch, runtimev1.CodeResultConflict)},

	{ID: "admitResource", Subject: SubjectResource, Method: "POST", Path: "/admit", Request: "ResourceAdmitRequest", Responses: map[int]string{200: "ResourceAdmitResponse"},
		Kinds: []ResourceKind{ResourcePreparation, ResourceService},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeInvalidCredential, domain.CodeForbidden, CodeResourceRevoked, CodeResourceUnbound,
			runtimev1.CodeAdmissionConsumed, runtimev1.CodeProfileUnsupported, CodeContextVersionMismatch, domain.CodeUnavailable, domain.CodeRateLimited}},
	{ID: "renewResourceToken", Subject: SubjectResource, Method: "POST", Path: "/token", Request: "ResourceTokenRequest", Responses: map[int]string{200: "ResourceTokenResponse"},
		Kinds: []ResourceKind{ResourcePreparation, ResourceService}, Errors: resourceErrs(domain.CodeInvalidRequest)},
	{ID: "getResourceContext", Subject: SubjectResource, Method: "GET", Path: "/context", Responses: map[int]string{200: "ResourceContext"},
		Kinds: []ResourceKind{ResourcePreparation, ResourceService}, Errors: resourceErrs()},
	{ID: "resolveResourceSecret", Subject: SubjectResource, Method: "POST", Path: "/secrets/resolve", Request: "SecretResolveRequest", Responses: map[int]string{200: "SecretResolveResponse"},
		Kinds: []ResourceKind{ResourceService}, Errors: resourceErrs(domain.CodeInvalidRequest, domain.CodeNotFound)},
	{ID: "completePreparation", Subject: SubjectResource, Method: "POST", Path: "/preparation/complete", Request: "PreparationReceiptRequest", Responses: map[int]string{200: "PreparationReceiptResponse"},
		Kinds: []ResourceKind{ResourcePreparation}, Errors: resourceErrs(domain.CodeInvalidRequest, CodeReceiptConflict)},
}

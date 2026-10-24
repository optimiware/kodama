package runtimev1

import (
	"net/http"

	"github.com/optimiware/kodama/internal/domain"
)

// Runtime-specific error codes. Codes are stable; statuses come from Status.
const (
	CodeAttemptRevoked      domain.ErrorCode = "attempt_revoked"
	CodeAdmissionConsumed   domain.ErrorCode = "admission_consumed"
	CodeResultConflict      domain.ErrorCode = "result_conflict"
	CodePublicationConflict domain.ErrorCode = "publication_conflict"
	CodeArtifactIncomplete  domain.ErrorCode = "artifact_incomplete"
	CodeOutputInvalid       domain.ErrorCode = "output_invalid"
	CodeDigestMismatch      domain.ErrorCode = "digest_mismatch"
	CodeResultHashMismatch  domain.ErrorCode = "result_hash_mismatch"
	CodeProfileUnsupported  domain.ErrorCode = "profile_unsupported"
)

var statuses = map[domain.ErrorCode]int{
	domain.CodeInvalidRequest:      http.StatusBadRequest,
	domain.CodeInvalidCredential:   http.StatusUnauthorized,
	domain.CodeForbidden:           http.StatusForbidden,
	domain.CodeNotFound:            http.StatusNotFound,
	domain.CodeIdempotencyConflict: http.StatusConflict,
	domain.CodeVersionConflict:     http.StatusPreconditionFailed,
	domain.CodeVersionRequired:     http.StatusPreconditionRequired,
	domain.CodeRateLimited:         http.StatusTooManyRequests,
	domain.CodeUnavailable:         http.StatusServiceUnavailable,
	CodeAttemptRevoked:             http.StatusForbidden,
	CodeAdmissionConsumed:          http.StatusConflict,
	CodeResultConflict:             http.StatusConflict,
	CodePublicationConflict:        http.StatusConflict,
	CodeArtifactIncomplete:         http.StatusConflict,
	CodeOutputInvalid:              http.StatusUnprocessableEntity,
	CodeDigestMismatch:             http.StatusUnprocessableEntity,
	CodeResultHashMismatch:         http.StatusUnprocessableEntity,
	CodeProfileUnsupported:         http.StatusUnprocessableEntity,
}

// Status returns the HTTP status of a code and whether the code is known.
func Status(code domain.ErrorCode) (int, bool) {
	s, ok := statuses[code]
	return s, ok
}

// ContractError is a contract violation carrying a stable code. Foreign or
// unknown references use CodeNotFound and a message that discloses nothing
// about the other tenant's resources.
type ContractError struct {
	Code    domain.ErrorCode
	Message string
}

func (e *ContractError) Error() string { return string(e.Code) + ": " + e.Message }

// Operation describes one runtime route. Request and the Responses values name
// DTO types ("" means no body). Errors lists the codes the route may return.
type Operation struct {
	ID        string
	Method    string
	Path      string // relative to BasePath
	Request   string
	Responses map[int]string
	Errors    []domain.ErrorCode
}

var (
	authErrors = []domain.ErrorCode{domain.CodeInvalidCredential, CodeAttemptRevoked, domain.CodeUnavailable, domain.CodeRateLimited}
)

func errs(extra ...domain.ErrorCode) []domain.ErrorCode {
	return append(append([]domain.ErrorCode{}, authErrors...), extra...)
}

// Operations is the complete route table. api/runtime.yaml must equal it.
var Operations = []Operation{
	{"admit", "POST", "/admit", "AdmitRequest", map[int]string{200: "AdmitResponse"},
		[]domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeInvalidCredential, domain.CodeForbidden, CodeAttemptRevoked, CodeAdmissionConsumed, CodeProfileUnsupported, domain.CodeUnavailable, domain.CodeRateLimited}},
	{"renewToken", "POST", "/token", "TokenRequest", map[int]string{200: "TokenResponse"}, errs(domain.CodeInvalidRequest)},
	{"getContext", "GET", "/context", "", map[int]string{200: "Context"}, errs()},
	{"resolveSecret", "POST", "/secrets/resolve", "SecretResolveRequest", map[int]string{200: "SecretResolveResponse"},
		errs(domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound)},
	{"reserveArtifact", "POST", "/artifacts", "ArtifactReserveRequest", map[int]string{200: "ArtifactReserveResponse", 201: "ArtifactReserveResponse"},
		errs(domain.CodeInvalidRequest, domain.CodeIdempotencyConflict, CodeOutputInvalid)},
	{"completeArtifact", "POST", "/artifacts/{artifactId}/complete", "ArtifactCompleteRequest", map[int]string{200: "ArtifactCompleteResponse"},
		errs(domain.CodeInvalidRequest, domain.CodeNotFound, CodeArtifactIncomplete, CodeDigestMismatch)},
	{"proposeSource", "POST", "/source", "SourceProposalRequest", map[int]string{200: "SourceProposalResponse", 202: "SourceProposalResponse"},
		errs(domain.CodeInvalidRequest, domain.CodeNotFound, CodeArtifactIncomplete, CodePublicationConflict)},
	{"submitResult", "POST", "/result", "ResultRequest", map[int]string{200: "ResultResponse"},
		errs(domain.CodeInvalidRequest, domain.CodeNotFound, CodeArtifactIncomplete, CodeOutputInvalid, CodeResultHashMismatch, CodeResultConflict)},
}

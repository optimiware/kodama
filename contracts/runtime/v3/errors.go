package runtimev3

import (
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// Status returns the HTTP status of a code and whether the code is known. v3
// defines no code of its own: the v1 and v2 codes keep their meaning and status,
// and the Git contract's codes (gitv1.Status) are those the Git grant routes
// return for a repository that is not ready, archived, not empty or has an
// unsettled push.
func Status(code domain.ErrorCode) (int, bool) {
	if s, ok := runtimev2.Status(code); ok {
		return s, ok
	}
	return gitv1.Status(code)
}

// Operation describes one route; it is the v2 type. Path is relative to
// AttemptBasePath.
type Operation = runtimev2.Operation

// The two principals of the v3 routes, as in v2: attempts (token audience
// kodama-runtime) and the resource workloads of the run (kodama-runtime-resource).
const (
	SubjectAttempt  = runtimev2.SubjectAttempt
	SubjectResource = runtimev2.SubjectResource
)

// ResourceCode and ResourceKind spellings reused from v2.
const (
	CodeResourceUnbound = runtimev2.CodeResourceUnbound
	CodeResourceRevoked = runtimev2.CodeResourceRevoked
	CodeReceiptConflict = runtimev2.CodeReceiptConflict
)

// CodeContextVersionMismatch is the v2 code: an attempt of another context
// version reached a v3 route or the reverse; no side effects.
const CodeContextVersionMismatch = runtimev2.CodeContextVersionMismatch

var attemptAuthErrors = []domain.ErrorCode{
	domain.CodeInvalidCredential, domain.CodeForbidden, runtimev1.CodeAttemptRevoked, CodeContextVersionMismatch, domain.CodeUnavailable, domain.CodeRateLimited,
}

func attemptErrs(extra ...domain.ErrorCode) []domain.ErrorCode {
	return append(append([]domain.ErrorCode{}, attemptAuthErrors...), extra...)
}

var resourceAuthErrors = []domain.ErrorCode{
	domain.CodeInvalidCredential, domain.CodeForbidden, CodeResourceRevoked, CodeResourceUnbound, CodeContextVersionMismatch,
	domain.CodeUnavailable, domain.CodeRateLimited,
}

func resourceErrs(extra ...domain.ErrorCode) []domain.ErrorCode {
	return append(append([]domain.ErrorCode{}, resourceAuthErrors...), extra...)
}

// Operations is the complete v3 route table: attempt routes are relative to
// AttemptBasePath and resource routes to ResourceBasePath. It
// is the source a later integration task generates the public OpenAPI fragment
// from; api/runtime.yaml is not changed by this contract. Differences from the
// v2 attempt routes: begin and result carry the v3 messages and there is no
// source-proposal route (an internally hosted source is pushed by the
// entrypoint with the attempt's Git grant and reported as commit outputs).
// The resource routes are the v2 routes on the v3 wire: services use the
// unchanged v2 messages, the preparation carries the whole source set.
var Operations = []Operation{
	{ID: "admit", Subject: SubjectAttempt, Method: "POST", Path: "/admit", Request: "AdmitRequest", Responses: map[int]string{200: "AdmitResponseV3"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeInvalidCredential, domain.CodeForbidden, runtimev1.CodeAttemptRevoked,
			runtimev1.CodeAdmissionConsumed, runtimev1.CodeProfileUnsupported, CodeContextVersionMismatch, domain.CodeUnavailable, domain.CodeRateLimited}},
	{ID: "renewToken", Subject: SubjectAttempt, Method: "POST", Path: "/token", Request: "TokenRequest", Responses: map[int]string{200: "TokenResponse"}, Errors: attemptErrs(domain.CodeInvalidRequest)},
	{ID: "getContext", Subject: SubjectAttempt, Method: "GET", Path: "/context", Responses: map[int]string{200: "ContextV3"}, Errors: attemptErrs()},
	{ID: "begin", Subject: SubjectAttempt, Method: "POST", Path: "/begin", Request: "BeginRequestV3", Responses: map[int]string{200: "BeginResponseV3"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeIdempotencyConflict)},
	{ID: "resolveSecret", Subject: SubjectAttempt, Method: "POST", Path: "/secrets/resolve", Request: "SecretResolveRequest", Responses: map[int]string{200: "SecretResolveResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound)},
	{ID: "seedGitGrant", Subject: SubjectAttempt, Method: "POST", Path: "/git/seed", Request: "GitSeedRequest", Responses: map[int]string{200: "GitGrantResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeNotFound, gitv1.CodeHostingMismatch, gitv1.CodeRepositoryArchived, gitv1.CodeRepositoryNotReady,
			gitv1.CodeRepositoryNotEmpty, gitv1.CodeSeedPending)},
	{ID: "integrationGitGrant", Subject: SubjectAttempt, Method: "POST", Path: "/git/integration", Request: "GitIntegrationRequest", Responses: map[int]string{200: "GitGrantResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeNotFound, runtimev1.CodePublicationConflict, gitv1.CodeRepositoryArchived, gitv1.CodeRepositoryNotReady)},
	{ID: "reserveArtifact", Subject: SubjectAttempt, Method: "POST", Path: "/artifacts", Request: "ArtifactReserveRequest",
		Responses: map[int]string{200: "ArtifactReserveResponse", 201: "ArtifactReserveResponse"},
		Errors:    attemptErrs(domain.CodeInvalidRequest, domain.CodeIdempotencyConflict, runtimev1.CodeOutputInvalid)},
	{ID: "completeArtifact", Subject: SubjectAttempt, Method: "POST", Path: "/artifacts/{artifactId}/complete", Request: "ArtifactCompleteRequest", Responses: map[int]string{200: "ArtifactCompleteResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeNotFound, runtimev1.CodeArtifactIncomplete, runtimev1.CodeDigestMismatch)},
	{ID: "submitResult", Subject: SubjectAttempt, Method: "POST", Path: "/result", Request: "ResultRequestV3", Responses: map[int]string{200: "ResultResponse"},
		Errors: attemptErrs(domain.CodeInvalidRequest, domain.CodeNotFound, runtimev1.CodeArtifactIncomplete, runtimev1.CodeOutputInvalid, runtimev1.CodeResultHashMismatch, runtimev1.CodeResultConflict)},

	{ID: "admitResource", Subject: SubjectResource, Method: "POST", Path: "/admit", Request: "ResourceAdmitRequest", Responses: map[int]string{200: "ResourceAdmitResponseV3"},
		Kinds: []ResourceKind{ResourcePreparation, ResourceService},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeInvalidCredential, domain.CodeForbidden, CodeResourceRevoked, CodeResourceUnbound,
			runtimev1.CodeAdmissionConsumed, runtimev1.CodeProfileUnsupported, CodeContextVersionMismatch, domain.CodeUnavailable, domain.CodeRateLimited}},
	{ID: "renewResourceToken", Subject: SubjectResource, Method: "POST", Path: "/token", Request: "ResourceTokenRequest", Responses: map[int]string{200: "ResourceTokenResponse"},
		Kinds: []ResourceKind{ResourcePreparation, ResourceService}, Errors: resourceErrs(domain.CodeInvalidRequest)},
	{ID: "getResourceContext", Subject: SubjectResource, Method: "GET", Path: "/context", Responses: map[int]string{200: "ResourceContextV3"},
		Kinds: []ResourceKind{ResourcePreparation, ResourceService}, Errors: resourceErrs()},
	{ID: "resolveResourceSecret", Subject: SubjectResource, Method: "POST", Path: "/secrets/resolve", Request: "SecretResolveRequest", Responses: map[int]string{200: "SecretResolveResponse"},
		Kinds: []ResourceKind{ResourceService}, Errors: resourceErrs(domain.CodeInvalidRequest, domain.CodeNotFound)},
	{ID: "completePreparation", Subject: SubjectResource, Method: "POST", Path: "/preparation/complete", Request: "PreparationReceiptRequestV3", Responses: map[int]string{200: "PreparationReceiptResponse"},
		Kinds: []ResourceKind{ResourcePreparation}, Errors: resourceErrs(domain.CodeInvalidRequest, CodeReceiptConflict)},
}

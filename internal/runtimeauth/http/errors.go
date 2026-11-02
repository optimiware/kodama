// Package runtimehttp exposes the runtime-authority routes of the frozen
// runtime contract (POST /admit, POST /token, GET /context) over net/http.
// It owns transport concerns only: strict decoding, size bounds, response
// headers, contract error envelopes and safe logging. Authority decisions
// belong to runtimeauth.Service.
package runtimehttp

import (
	"errors"
	"net/http"
	"slices"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// messages are fixed, client-safe texts. Error bodies never carry the text of
// an underlying error, which can quote credentials, tokens or provider output.
var messages = map[domain.ErrorCode]string{
	domain.CodeInvalidRequest:        "The request is not valid.",
	domain.CodeInvalidCredential:     "The credential is not valid.",
	domain.CodeForbidden:             "Job identity does not match the attempt.",
	runtimev1.CodeAttemptRevoked:     "The attempt is no longer authorized.",
	runtimev1.CodeAdmissionConsumed:  "Admission was already consumed.",
	runtimev1.CodeProfileUnsupported: "Wrapper version is not supported by this profile.",
	domain.CodeUnavailable:           "The service cannot decide right now; retry.",
	domain.CodeNotFound:              "Not found.",
	// Codes new in runtime v2. The v1 routes answer context_version_mismatch for a
	// run_shared attempt; the other codes are emitted by the v2 routes.
	runtimev2.CodeContextVersionMismatch: "The attempt belongs to another runtime version.",
	runtimev2.CodeReceiptConflict:        "A different preparation receipt is already recorded.",
	runtimev2.CodeResourceUnbound:        "The resource instance is not bound yet.",
	runtimev2.CodeResourceRevoked:        "The resource is no longer authorized.",
	domain.CodeIdempotencyConflict:       "The request conflicts with the earlier one.",
	// Codes of the runtime v3 Git grant routes (the Git contract's repository
	// codes and the integration route's publication_conflict). Only the v3 routes
	// produce them; the v1 and v2 inventories exclude them.
	runtimev1.CodePublicationConflict: "The target branch is not what the approval was given over; the candidate must be recomputed.",
	gitv1.CodeHostingMismatch:         "The repository is not hosted by this installation.",
	gitv1.CodeRepositoryArchived:      "The repository is archived.",
	gitv1.CodeRepositoryNotReady:      "The repository is not ready.",
	gitv1.CodeRepositoryNotEmpty:      "The repository already has refs.",
	gitv1.CodeSeedPending:             "An earlier push to the repository is unsettled.",
}

// classify maps a service error to a contract code.
func classify(err error) domain.ErrorCode {
	switch {
	case errors.Is(err, runtimeauth.ErrInvalidRequest):
		return domain.CodeInvalidRequest
	case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound):
		return domain.CodeInvalidCredential
	case errors.Is(err, runtimeauth.ErrForbidden):
		return domain.CodeForbidden
	case errors.Is(err, runtimeauth.ErrAttemptRevoked):
		return runtimev1.CodeAttemptRevoked
	case errors.Is(err, runtimeauth.ErrAdmissionConsumed):
		return runtimev1.CodeAdmissionConsumed
	case errors.Is(err, runtimeauth.ErrProfileUnsupported):
		return runtimev1.CodeProfileUnsupported
	case errors.Is(err, runtimeauth.ErrContextVersionMismatch):
		return runtimev2.CodeContextVersionMismatch
	case errors.Is(err, runtimeauth.ErrResourceRevoked):
		return runtimev2.CodeResourceRevoked
	case errors.Is(err, runtimeauth.ErrResourceUnbound):
		return runtimev2.CodeResourceUnbound
	case errors.Is(err, runtimeauth.ErrReceiptConflict):
		return runtimev2.CodeReceiptConflict
	case errors.Is(err, runtimeauth.ErrIdempotencyConflict):
		return domain.CodeIdempotencyConflict
	case errors.Is(err, runtimeauth.ErrPublicationConflict):
		return runtimev1.CodePublicationConflict
	}
	// Includes ErrUnavailable and ErrConflict: no decision was made, nothing
	// is granted, and the client may retry.
	return domain.CodeUnavailable
}

// allowed reports whether the frozen error inventory of the operation lists
// the code.
func allowed(opID string, code domain.ErrorCode) bool {
	for _, op := range runtimev1.Operations {
		if op.ID == opID {
			return slices.Contains(op.Errors, code)
		}
	}
	return false
}

// routeCode maps err to a code the operation's frozen inventory permits. A code
// the inventory excludes degrades to the nearest permitted denial: a foreign
// action or path on a route without `forbidden` is an invalid credential.
func routeCode(opID string, err error) domain.ErrorCode {
	code := classify(err)
	if allowed(opID, code) {
		return code
	}
	// The one code the frozen v1 inventory lacks that a v1 route must still say: a
	// run_shared attempt reached the v1 wire (nothing was changed).
	if code == runtimev2.CodeContextVersionMismatch {
		return code
	}
	switch code {
	case domain.CodeForbidden, domain.CodeInvalidRequest:
		return domain.CodeInvalidCredential
	}
	return domain.CodeUnavailable
}

func statusOf(code domain.ErrorCode) int {
	if s, ok := runtimev2.Status(code); ok {
		return s
	}
	return http.StatusServiceUnavailable
}

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

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
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
	switch code {
	case domain.CodeForbidden, domain.CodeInvalidRequest:
		return domain.CodeInvalidCredential
	}
	return domain.CodeUnavailable
}

func statusOf(code domain.ErrorCode) int {
	if s, ok := runtimev1.Status(code); ok {
		return s
	}
	return http.StatusServiceUnavailable
}

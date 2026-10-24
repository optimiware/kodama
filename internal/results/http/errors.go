package resultshttp

import (
	"errors"
	"net/http"
	"slices"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
)

// messages are fixed, client-safe texts. Error bodies never carry the text of an
// underlying error, which can quote credentials, tokens, provider output or
// another tenant's identifiers.
var messages = map[domain.ErrorCode]string{
	domain.CodeInvalidRequest:         "The request is not valid.",
	domain.CodeInvalidCredential:      "The credential is not valid.",
	domain.CodeNotFound:               "Not found.",
	runtimev1.CodeAttemptRevoked:      "The attempt is no longer authorized.",
	runtimev1.CodeArtifactIncomplete:  "The artifact upload is not complete.",
	runtimev1.CodeOutputInvalid:       "The result does not satisfy the declared outputs.",
	runtimev1.CodeResultHashMismatch:  "The result hash does not match the canonical result.",
	runtimev1.CodeResultConflict:      "A different result was already accepted for this attempt.",
	runtimev1.CodePublicationConflict: "The branch tip moved; the proposal was not published.",
	domain.CodeUnavailable:            "The service cannot decide right now; retry.",
}

// classify maps a service error to a contract code and safe details.
func classify(err error) (domain.ErrorCode, map[string]any) {
	var pc *results.PublicationConflict
	switch {
	case errors.As(err, &pc):
		d := map[string]any{}
		if pc.Reason != "" {
			d["reason"] = pc.Reason
		}
		if pc.ObservedTip != "" {
			d["observedTip"] = pc.ObservedTip
		}
		return runtimev1.CodePublicationConflict, d
	case errors.Is(err, results.ErrInvalidRequest):
		return domain.CodeInvalidRequest, nil
	case errors.Is(err, results.ErrUnauthenticated):
		return domain.CodeInvalidCredential, nil
	case errors.Is(err, results.ErrForbidden):
		return domain.CodeForbidden, nil
	case errors.Is(err, results.ErrAttemptRevoked):
		return runtimev1.CodeAttemptRevoked, nil
	case errors.Is(err, results.ErrNotFound):
		return domain.CodeNotFound, nil
	case errors.Is(err, results.ErrArtifactIncomplete):
		return runtimev1.CodeArtifactIncomplete, nil
	case errors.Is(err, results.ErrOutputInvalid):
		return runtimev1.CodeOutputInvalid, nil
	case errors.Is(err, results.ErrResultHashMismatch):
		return runtimev1.CodeResultHashMismatch, nil
	case errors.Is(err, results.ErrResultConflict):
		return runtimev1.CodeResultConflict, nil
	}
	// Includes ErrUnavailable and anything unclassified: no decision was made,
	// nothing was acknowledged, and the client may repeat the request.
	return domain.CodeUnavailable, nil
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
// the inventory excludes degrades to the nearest permitted denial: a valid
// credential used for an action it does not cover, on a route without
// `forbidden`, is an invalid credential.
func routeCode(opID string, err error) (domain.ErrorCode, map[string]any) {
	code, details := classify(err)
	if allowed(opID, code) {
		return code, details
	}
	switch code {
	case domain.CodeForbidden, domain.CodeInvalidRequest:
		return domain.CodeInvalidCredential, nil
	}
	return domain.CodeUnavailable, nil
}

func statusOf(code domain.ErrorCode) int {
	if s, ok := runtimev1.Status(code); ok {
		return s
	}
	return http.StatusServiceUnavailable
}

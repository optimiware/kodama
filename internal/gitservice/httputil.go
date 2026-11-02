package gitservice

import (
	"encoding/json"
	"errors"
	"net/http"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeContractError writes a contract error in the contract's error body, with
// the status the contract assigns to its code.
func writeContractError(w http.ResponseWriter, ce *gitv1.ContractError, requestID string) {
	status, ok := gitv1.Status(ce.Code)
	if !ok {
		status = http.StatusInternalServerError
	}
	if requestID == "" {
		requestID = "unknown"
	}
	msg := ce.Message
	if msg == "" {
		msg = string(ce.Code)
	}
	writeJSON(w, status, gitv1.ErrorResponse{Error: gitv1.ErrorBody{
		Code: ce.Code, Message: truncateText(msg, 2000), RequestID: requestID, Details: map[string]any{},
	}})
}

// An ambiguous outcome has no code in the Git service contract, and its codes
// keep their statuses, but contracts/git/README.md already classifies a failure
// after the request may have been sent as Outcome Unknown (a timeout or reset,
// an unreadable response). The worker therefore answers a mutation that may have
// been applied by aborting the HTTP response: the caller sees a transport
// failure, never a status. A contract error body always means a definite refusal
// (or, for 503 unavailable, that nothing changed).
// unknownOutcomeError marks a failure after which a mutation may or may not
// have been applied (cancellation or a kill while Git was updating a ref, a
// failure after the repository was published). The caller repeats the identical
// request, which converges.
type unknownOutcomeError struct{ err error }

func (e *unknownOutcomeError) Error() string { return "unknown outcome: " + e.err.Error() }
func (e *unknownOutcomeError) Unwrap() error { return e.err }

func unknownOutcome(err error) error {
	if err == nil {
		return nil
	}
	var already *unknownOutcomeError
	if errors.As(err, &already) {
		return err
	}
	return &unknownOutcomeError{err: err}
}

// writeError maps any error to a contract error: a contract error keeps its
// code, anything else is reported as unavailable without detail.
func (s *Service) writeError(w http.ResponseWriter, err error, requestID string) {
	var unknown *unknownOutcomeError
	if errors.As(err, &unknown) {
		s.cfg.Logger.Error("git worker mutation with an unknown outcome", "error", err, "requestId", requestID)
		panic(http.ErrAbortHandler) // the server closes the connection without a response, silently
	}
	ce, ok := asContract(err)
	if !ok {
		s.cfg.Logger.Error("git worker request failed", "error", err, "requestId", requestID)
		ce = contractError(domain.CodeUnavailable, "the Git worker could not complete the request")
	}
	writeContractError(w, ce, requestID)
}

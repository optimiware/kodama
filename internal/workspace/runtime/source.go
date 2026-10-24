// Package runtime translates the frozen runtime contract's source proposal
// (runtimev1.SourceProposalRequest/Response, POST /runtime/v1/attempts/{id}/source)
// to and from the workspace use case. It registers no route: the runtime
// integration owner (T-020/T-022/T-027) authenticates the attempt, derives its
// scope and calls Propose, so attempt identity never comes from the request body.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// Proposer is the consumer-owned port; *workspace.Service implements it.
type Proposer interface {
	ProposeSource(ctx context.Context, in workspace.ProposeInput) (workspace.ProposeResult, error)
}

// Source adapts the use case to the runtime contract.
type Source struct{ svc Proposer }

// NewSource requires the use case service.
func NewSource(svc Proposer) (*Source, error) {
	if svc == nil {
		return nil, fmt.Errorf("%w: workspace service", workspace.ErrNotConfigured)
	}
	return &Source{svc: svc}, nil
}

// Result is a successful answer: 200 accepted or 202 pending.
type Result struct {
	Status   int
	Response runtimev1.SourceProposalResponse
}

// Error is a contract error: its status, stable code and safe details. The
// message never carries credentials, repository contents or foreign IDs.
type Error struct {
	Status  int
	Code    domain.ErrorCode
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Body renders the contract's error response for the given request ID.
func (e *Error) Body(requestID string) runtimev1.ErrorResponse {
	d := e.Details
	if d == nil {
		d = map[string]any{}
	}
	return runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: e.Code, Message: e.Message, RequestID: requestID, Details: d}}
}

func fail(code domain.ErrorCode, msg string, details map[string]any) *Error {
	status, ok := runtimev1.Status(code)
	if !ok {
		status = http.StatusInternalServerError
	}
	return &Error{Status: status, Code: code, Message: msg, Details: details}
}

// Propose validates the request shape, runs the use case for the authenticated
// attempt in scope and maps the outcome to the contract. Repeating an identical
// proposal observes the original publication (pending becomes accepted, a
// conflict stays a conflict).
func (s *Source) Propose(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, req runtimev1.SourceProposalRequest) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, fail(domain.CodeInvalidRequest, "the request is not valid for this operation", nil)
	}
	res, err := s.svc.ProposeSource(ctx, workspace.ProposeInput{
		Scope: scope, AttemptID: attempt,
		Commit: workspace.Commit(req.Commit), ExpectedTip: workspace.Commit(req.ExpectedTip), BundleArtifactID: req.BundleArtifactID,
	})
	if err != nil {
		return Result{}, mapError(err)
	}
	out := runtimev1.SourceProposalResponse{Commit: runtimev1.GitCommit(res.Commit)}
	switch res.Status {
	case workspace.ProposeAccepted:
		c := runtimev1.GitCommit(res.AcceptedCommit)
		out.Status, out.AcceptedCommit = runtimev1.PublishAccepted, &c
		return Result{Status: http.StatusOK, Response: out}, nil
	case workspace.ProposePending:
		out.Status = runtimev1.PublishPending
		return Result{Status: http.StatusAccepted, Response: out}, nil
	}
	return Result{}, fail(domain.CodeUnavailable, "the publication outcome is not known; repeat the proposal", nil)
}

func mapError(err error) *Error {
	var conflict *workspace.ConflictError
	var bundle *workspace.BundleError
	switch {
	case errors.As(err, &conflict):
		d := map[string]any{"reason": conflict.Reason}
		if conflict.ObservedTip != "" {
			d["observedTip"] = string(conflict.ObservedTip)
		}
		return fail(runtimev1.CodePublicationConflict, "the branch tip moved; the proposal was not published", d)
	case errors.Is(err, workspace.ErrProposalChanged):
		return fail(runtimev1.CodePublicationConflict, "a different proposal was already recorded for this attempt", map[string]any{"reason": "proposal_changed"})
	case errors.Is(err, workspace.ErrAdmissionClosed):
		return fail(runtimev1.CodePublicationConflict, "the workspace no longer admits source", map[string]any{"reason": "admission_closed"})
	case errors.Is(err, workspace.ErrAttemptRevoked), errors.Is(err, workspace.ErrNotWriter):
		return fail(runtimev1.CodeAttemptRevoked, "the attempt may not publish source", nil)
	case errors.Is(err, workspace.ErrNotFound):
		return fail(domain.CodeNotFound, "the resource was not found", nil)
	case errors.Is(err, workspace.ErrBundleIncomplete):
		return fail(runtimev1.CodeArtifactIncomplete, "the bundle artifact is not complete", nil)
	case errors.As(err, &bundle):
		return fail(domain.CodeInvalidRequest, "the bundle was rejected", map[string]any{"reason": bundle.Reason})
	case errors.Is(err, workspace.ErrSecretInSource):
		return fail(domain.CodeInvalidRequest, "the proposed source contains an injected secret", map[string]any{"reason": "secret_in_source"})
	case errors.Is(err, workspace.ErrLimitExceeded):
		return fail(domain.CodeInvalidRequest, "the bundle exceeds a size or time bound", map[string]any{"reason": "limit_exceeded"})
	case errors.Is(err, workspace.ErrInvalidValue):
		return fail(domain.CodeInvalidRequest, "the request is not valid for this operation", nil)
	}
	// Dependency failures, fencing and anything unknown: retryable, nothing disclosed.
	return fail(domain.CodeUnavailable, "a required service is unavailable; repeat the proposal", nil)
}

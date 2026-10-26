package runtimev2

import (
	"github.com/optimiware/kodama/internal/domain"
)

func invalidRequest(msg string) error {
	return &ContractError{Code: domain.CodeInvalidRequest, Message: msg}
}

// DeriveBegin applies the server's derivation of an attempt's begin record to
// the request, in the context of the attempt:
//
//   - source-free (no workspace source): the request's workspace must be null;
//     the state is none and ignoredPresent false;
//   - source-backed: the request's workspace must be set; the state is clean
//     exactly when head equals the context's headCommit, tracked files are clean
//     and no untracked file exists, otherwise modified; ignoredPresent is the
//     entrypoint's disclosure and never makes the state modified by itself.
//
// A null body for a source-backed attempt, or an object for a source-free one,
// is invalid_request. The values are derived, never client-asserted in an
// artifact request.
func DeriveBegin(ctx ContextV2, req BeginRequest) (WorkspaceState, bool, error) {
	if err := req.Validate(); err != nil {
		return "", false, invalidRequest(err.Error())
	}
	src := ctx.Workspace.Source
	switch {
	case src == nil && req.Workspace != nil:
		return "", false, invalidRequest("a source-free attempt reports a null workspace")
	case src != nil && req.Workspace == nil:
		return "", false, invalidRequest("a source-backed attempt reports its workspace")
	case src == nil:
		return StateNone, false, nil
	}
	w := req.Workspace
	if w.Head == src.HeadCommit && w.TrackedClean && !w.UntrackedPresent {
		return StateClean, w.IgnoredPresent, nil
	}
	return StateModified, w.IgnoredPresent, nil
}

// CheckBeginResponse verifies that a begin response is the derivation of the
// request for the attempt's context.
func CheckBeginResponse(ctx ContextV2, req BeginRequest, resp BeginResponse) error {
	state, ignored, err := DeriveBegin(ctx, req)
	if err != nil {
		return err
	}
	if resp.AttemptID != ctx.AttemptID {
		return &ContractError{Code: domain.CodeForbidden, Message: "the response is for another attempt"}
	}
	if resp.WorkspaceState != state || resp.IgnoredPresent != ignored {
		return invalidRequest("the response is not the derivation of the request")
	}
	return nil
}

// CheckReceipt verifies a preparation receipt request against the resource's
// context: it must be a preparation resource, the commit must be the pinned
// input commit and the internal try count at most maxAttempts.
func CheckReceipt(ctx ResourceContext, req PreparationReceiptRequest) error {
	if err := req.Validate(); err != nil {
		return invalidRequest(err.Error())
	}
	if ctx.ResourceKind != ResourcePreparation || ctx.Preparation == nil {
		return &ContractError{Code: domain.CodeForbidden, Message: "only a preparation resource records a receipt"}
	}
	if req.Commit != ctx.Preparation.Commit {
		return invalidRequest("the receipt commit is not the run's pinned input commit")
	}
	if req.Attempts > ctx.Preparation.MaxAttempts {
		return invalidRequest("the receipt reports more tries than the policy allows")
	}
	return nil
}

// CheckResourceCall decides which resource kinds may call an operation: the
// preparation has no secrets route and a service has no preparation route.
func CheckResourceCall(op Operation, kind ResourceKind) error {
	if op.Subject != SubjectResource {
		return &ContractError{Code: domain.CodeForbidden, Message: "not a resource route"}
	}
	for _, k := range op.Kinds {
		if k == kind {
			return nil
		}
	}
	return &ContractError{Code: domain.CodeForbidden, Message: "this resource kind may not call the route"}
}

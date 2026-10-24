package http

import (
	"errors"
	stdhttp "net/http"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/workspace"
)

// Module-defined error codes (frozen in api/workspace.yaml).
const (
	CodeNotActionable      domain.ErrorCode = "work_item_not_actionable"
	CodeStateConflict      domain.ErrorCode = "workspace_state_conflict"
	CodePublicationPending domain.ErrorCode = "publication_pending"
	CodeBranchDiverged     domain.ErrorCode = "branch_diverged"
	CodePublisherFenced    domain.ErrorCode = "publisher_fenced"
	CodeArchivePending     domain.ErrorCode = "archive_pending"
)

// mapError translates a use case error to its API rendering. Detail text of
// the cause is never sent: messages are fixed and details hold only safe
// codes.
func mapError(err error, w *workspace.Workspace) *httpapi.APIError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspace.ErrArchiveUncertain):
		e := httpapi.NewError(stdhttp.StatusConflict, CodeArchivePending, "archive settlement is not complete; the request is retained")
		if w != nil {
			e = e.WithDetails(map[string]any{
				"workspaceId": string(w.ID), "state": string(w.State), "version": int64(w.Version), "publicationPending": w.Pending != nil,
			})
			if tag, terr := httpapi.FormatETag(w.Version); terr == nil {
				e.Header = stdhttp.Header{"ETag": {tag}}
			}
		}
		return e
	case errors.Is(err, workspace.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, workspace.ErrDenied):
		return httpapi.ForbiddenError()
	case errors.Is(err, workspace.ErrVersionConflict):
		return httpapi.VersionConflictError()
	case errors.Is(err, workspace.ErrInvalidValue):
		return httpapi.BadRequestError("the request is not valid for this operation")
	case errors.Is(err, workspace.ErrNotActionable):
		return httpapi.NewError(stdhttp.StatusConflict, CodeNotActionable, "the work item cannot have a workspace")
	case errors.Is(err, workspace.ErrPublicationPending):
		return httpapi.NewError(stdhttp.StatusConflict, CodePublicationPending, "a source publication is unsettled; retry after it settles")
	case errors.Is(err, workspace.ErrDiverged):
		return httpapi.NewError(stdhttp.StatusConflict, CodeBranchDiverged, "the remote branch is not a fast-forward of the accepted commit").
			WithDetails(map[string]any{"lastError": workspace.ReasonBranchDiverged})
	case errors.Is(err, workspace.ErrFenced):
		return httpapi.NewError(stdhttp.StatusConflict, CodePublisherFenced, "publication is fenced by installation recovery")
	case errors.Is(err, workspace.ErrInvalidTransition), errors.Is(err, workspace.ErrStale), errors.Is(err, workspace.ErrAdmissionClosed):
		return httpapi.NewError(stdhttp.StatusConflict, CodeStateConflict, "the workspace state does not allow this operation")
	case errors.Is(err, workspace.ErrUnavailable), errors.Is(err, workspace.ErrLimitExceeded), errors.Is(err, workspace.ErrNotConfigured):
		return httpapi.UnavailableError(5*time.Second, err)
	}
	return httpapi.InternalServerError(err)
}

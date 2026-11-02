package artifactshttp

import (
	"context"
	"net/http"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// RuntimeReadService issues a download for one artifact pinned in the attempt's
// verified context. It does not use the human read authority.
type RuntimeReadService interface {
	RuntimeMetadata(ctx context.Context, caller artifacts.Caller, attempt domain.AttemptID, id domain.ArtifactID) (artifacts.View, error)
	RuntimeMetadataFromState(ctx context.Context, caller artifacts.Caller, attempt domain.AttemptID, id domain.ArtifactID, binding, key, etag string) (artifacts.View, error)
}

// RuntimeRead serves GET /runtime/v1/attempts/{attemptId}/artifacts/{artifactId}/download.
type RuntimeRead struct {
	svc     RuntimeReadService
	actions Actions
}

// NewRuntimeRead requires the service.
func NewRuntimeRead(svc RuntimeReadService, actions Actions) (*RuntimeRead, error) {
	if svc == nil || actions.Read == "" {
		return nil, errConfig
	}
	return &RuntimeRead{svc: svc, actions: actions}, nil
}

func (h *RuntimeRead) RegisterRoutes(rt *httpapi.Router) error {
	return rt.Handle(httpapi.Route{
		Method: http.MethodGet, Path: "/attempts/{attemptId}/artifacts/{artifactId}/download",
		Action: h.actions.Read, ResourceKind: ResourceAttempt, ResourceParam: "attemptId", Handler: h.download,
	})
}

func (h *RuntimeRead) download(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	id := domain.ArtifactID(r.PathValue("artifactId"))
	if attempt.Validate() != nil || id.Validate() != nil {
		httpapi.WriteError(w, r, httpapi.BadRequestError("an identifier is not valid"))
		return
	}
	binding, key := r.URL.Query().Get("binding"), r.URL.Query().Get("key")
	var v artifacts.View
	var err error
	if binding != "" || key != "" {
		if binding == "" || key == "" {
			httpapi.WriteError(w, r, httpapi.BadRequestError("binding and key are required together"))
			return
		}
		v, err = h.svc.RuntimeMetadataFromState(r.Context(), caller(c), attempt, id, binding, key, strings.Trim(r.URL.Query().Get("etag"), `"`))
	} else {
		v, err = h.svc.RuntimeMetadata(r.Context(), caller(c), attempt, id)
	}
	if err != nil {
		httpapi.WriteError(w, r, translate(err))
		return
	}
	if v.Download == nil {
		httpapi.WriteError(w, r, httpapi.NewError(http.StatusConflict, runtimev1.CodeArtifactIncomplete, "the artifact is not available for download"))
		return
	}
	resp := runtimev1.ArtifactReadResponse{
		ArtifactID: v.Artifact.ID(),
		Digest:     runtimev1.Digest(v.Artifact.Digest()),
		Download: runtimev1.Upload{
			Method: v.Download.Method, URL: v.Download.URL, Headers: headersOrEmpty(v.Download.Headers),
			ExpiresAt: runtimev1.Timestamp(v.Download.ExpiresAt.UTC().Format(time.RFC3339Nano)),
		},
	}
	if err := resp.Validate(); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, resp)
}

// Package artifactshttp is the HTTP adapter of the artifact module: the two
// internal runtime routes (reserve and complete) and the public metadata route.
//
// Runtime routes carry no tenant or project in their path. The scope is resolved
// by the attempt authority from the verified runtime credential and the attempt
// in the path, never from the generic router target or the request body, and
// the authority is asked again on every call. The adapter reuses the frozen
// runtime contract (strict decoding, response validation, status mapping) and
// the shared httpapi foundation (bounded JSON, request IDs, no-store headers,
// nondisclosing errors). Public access is authorized by the router and again by
// the module's read authority, including diagnostic visibility, before any
// transfer instruction is issued.
package artifactshttp

import (
	"context"
	"net/http"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Actions are the authorization actions of the module's routes. The composition
// root maps them to its policy.
type Actions struct {
	Reserve, Complete, Read httpapi.Action
}

// DefaultActions returns the documented action identifiers.
func DefaultActions() Actions {
	return Actions{Reserve: "artifact.reserve", Complete: "artifact.complete", Read: "artifact.read"}
}

// Resource kinds passed to the Authorizer.
const (
	ResourceAttempt  = "attempt"
	ResourceArtifact = "artifact"
)

// RuntimeService is what the runtime routes need from the module.
type RuntimeService interface {
	Reserve(ctx context.Context, caller artifacts.Caller, attempt domain.AttemptID, in artifacts.ReserveInput, requestID string) (artifacts.ReserveOutput, error)
	Complete(ctx context.Context, caller artifacts.Caller, attempt domain.AttemptID, id domain.ArtifactID, in artifacts.CompleteInput, requestID string) (artifacts.Completion, error)
}

// Runtime serves the runtime artifact routes.
type Runtime struct {
	svc     RuntimeService
	actions Actions
}

// NewRuntime requires the service.
func NewRuntime(svc RuntimeService, actions Actions) (*Runtime, error) {
	if svc == nil || actions.Reserve == "" || actions.Complete == "" {
		return nil, errConfig
	}
	return &Runtime{svc: svc, actions: actions}, nil
}

// RegisterRoutes registers
//
//	POST {prefix}/attempts/{attemptId}/artifacts
//	POST {prefix}/attempts/{attemptId}/artifacts/{artifactId}/complete
//
// on a router whose prefix is "/runtime/v1". The router authenticates the
// runtime token and authorizes the attempt reference before a handler runs.
func (h *Runtime) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodPost, Path: "/attempts/{attemptId}/artifacts", Action: h.actions.Reserve,
			ResourceKind: ResourceAttempt, ResourceParam: "attemptId", Handler: h.reserve},
		{Method: http.MethodPost, Path: "/attempts/{attemptId}/artifacts/{artifactId}/complete", Action: h.actions.Complete,
			ResourceKind: ResourceAttempt, ResourceParam: "attemptId", Handler: h.complete},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func caller(c *httpapi.Call) artifacts.Caller { return artifacts.Caller{Subject: c.Principal.ID} }

func (h *Runtime) reserve(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		httpapi.WriteError(w, r, httpapi.BadRequestError("the attempt identifier is not valid"))
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	req, err := runtimev1.DecodeRequest[runtimev1.ArtifactReserveRequest](body)
	if err != nil {
		httpapi.WriteError(w, r, translate(err))
		return
	}
	digest, derr := artifacts.NewDigest(string(req.Digest))
	if derr != nil {
		httpapi.WriteError(w, r, translate(derr))
		return
	}
	in := artifacts.ReserveInput{Name: req.Name, Kind: artifacts.Kind(req.Kind), Role: artifacts.Role(req.Role), Digest: digest,
		MediaType: req.MediaType, Size: req.SizeBytes, Registry: req.Registry}
	out, err := h.svc.Reserve(r.Context(), caller(c), attempt, in, c.RequestID)
	if err != nil {
		httpapi.WriteError(w, r, translate(err))
		return
	}
	resp := runtimev1.ArtifactReserveResponse{ArtifactID: out.ID, State: runtimev1.ArtifactState(out.State)}
	if out.Upload != nil {
		resp.Upload = &runtimev1.Upload{Method: out.Upload.Method, URL: out.Upload.URL, Headers: headersOrEmpty(out.Upload.Headers), ExpiresAt: timestamp(out.Upload.ExpiresAt)}
	}
	// A response that violates the contract is never delivered.
	if err := resp.Validate(); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	httpapi.WriteJSON(w, r, status, resp)
}

func (h *Runtime) complete(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	id := domain.ArtifactID(r.PathValue("artifactId"))
	if attempt.Validate() != nil || id.Validate() != nil {
		httpapi.WriteError(w, r, httpapi.BadRequestError("an identifier is not valid"))
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	req, err := runtimev1.DecodeRequest[runtimev1.ArtifactCompleteRequest](body)
	if err != nil {
		httpapi.WriteError(w, r, translate(err))
		return
	}
	out, err := h.svc.Complete(r.Context(), caller(c), attempt, id, artifacts.CompleteInput{Digest: artifacts.Digest(req.Digest), Size: req.SizeBytes}, c.RequestID)
	if err != nil {
		httpapi.WriteError(w, r, translate(err))
		return
	}
	resp := runtimev1.ArtifactCompleteResponse{ArtifactID: out.ID, State: runtimev1.StateUsable, Digest: runtimev1.Digest(out.Digest), SizeBytes: out.Size}
	if err := resp.Validate(); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, resp)
}

func headersOrEmpty(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}

func timestamp(t time.Time) runtimev1.Timestamp {
	return runtimev1.Timestamp(t.UTC().Format(time.RFC3339Nano))
}

// ToRuntimeRef converts a module reference to the runtime contract's trusted
// artifact metadata for CheckResult.
func ToRuntimeRef(ref artifacts.Reference) runtimev1.ArtifactRef {
	return runtimev1.ArtifactRef{ID: ref.ID, Scope: ref.Scope, Attempt: ref.Attempt, Kind: runtimev1.ArtifactKind(ref.Kind), Digest: runtimev1.Digest(ref.Digest), Usable: ref.Usable}
}

// RuntimeRefs converts a reference lookup result.
func RuntimeRefs(refs map[domain.ArtifactID]artifacts.Reference) map[domain.ArtifactID]runtimev1.ArtifactRef {
	out := make(map[domain.ArtifactID]runtimev1.ArtifactRef, len(refs))
	for id, ref := range refs {
		out[id] = ToRuntimeRef(ref)
	}
	return out
}

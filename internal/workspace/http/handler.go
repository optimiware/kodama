// Package http is the HTTP adapter of the workspace module: the five human
// operations of the HTTP contract registered through httpapi.Router. Routes
// carry explicit actions and resource metadata; authorization is the router's
// injected, deny-by-default Authorizer, and the use cases check it again.
package http

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	stdhttp "net/http"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/workspace"
)

// Service is the consumer-owned port the handlers use; *workspace.Service
// implements it.
type Service interface {
	Provision(ctx contextT, actor workspace.Actor, scope domain.Scope, item domain.WorkItemID) (*workspace.Workspace, bool, error)
	Get(ctx contextT, actor workspace.Actor, scope domain.Scope, id workspace.ID) (*workspace.Workspace, error)
	Sync(ctx contextT, actor workspace.Actor, scope domain.Scope, id workspace.ID, ifMatch domain.Version) (*workspace.Workspace, error)
	Archive(ctx contextT, actor workspace.Actor, scope domain.Scope, id workspace.ID, ifMatch domain.Version) (*workspace.Workspace, error)
	Restore(ctx contextT, actor workspace.Actor, scope domain.Scope, id workspace.ID, ifMatch domain.Version) (*workspace.Workspace, error)
	// Repositories returns the repository associations of an internally hosted
	// workspace (empty for an external one).
	Repositories(ctx contextT, actor workspace.Actor, scope domain.Scope, id workspace.ID) ([]*workspace.RepositoryAssociation, error)
}

// Authorization actions registered with each route.
const (
	ActionProvision httpapi.Action = "workspace.provision"
	ActionRead      httpapi.Action = "workspace.read"
	ActionSync      httpapi.Action = "workspace.sync"
	ActionArchive   httpapi.Action = "workspace.archive"
	ActionRestore   httpapi.Action = "workspace.restore"
)

const projectPath = "/tenants/{tenantId}/projects/{projectId}"

// Handler serves the workspace routes.
type Handler struct{ svc Service }

// NewHandler requires the use case service.
func NewHandler(svc Service) (*Handler, error) {
	if svc == nil {
		return nil, fmt.Errorf("%w: workspace service", workspace.ErrNotConfigured)
	}
	return &Handler{svc: svc}, nil
}

// Register adds the five routes under the router's prefix.
func (h *Handler) Register(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: stdhttp.MethodPost, Path: projectPath + "/work-items/{id}/workspace", Action: ActionProvision, ResourceKind: "work_item", ResourceParam: "id", Handler: h.provision},
		{Method: stdhttp.MethodGet, Path: projectPath + "/workspaces/{id}", Action: ActionRead, ResourceKind: "workspace", ResourceParam: "id", Handler: h.get},
		{Method: stdhttp.MethodPost, Path: projectPath + "/workspaces/{id}/archive", Action: ActionArchive, ResourceKind: "workspace", ResourceParam: "id", Handler: h.archive},
		{Method: stdhttp.MethodPost, Path: projectPath + "/workspaces/{id}/restore", Action: ActionRestore, ResourceKind: "workspace", ResourceParam: "id", Handler: h.restore},
		{Method: stdhttp.MethodPost, Path: projectPath + "/workspaces/{id}/sync", Action: ActionSync, ResourceKind: "workspace", ResourceParam: "id", Handler: h.sync},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return nil
}

func actorOf(c *httpapi.Call) workspace.Actor {
	return workspace.Actor{Type: workspace.ActorUser, ID: c.Principal.ID}
}

// requestContext carries the router's request ID into audit and events.
func requestContext(r *stdhttp.Request, c *httpapi.Call) contextT {
	return workspace.WithRequestID(r.Context(), c.RequestID)
}

// emptyBody accepts no body or exactly `{}`: these operations take no input,
// and a typo must not be silently dropped.
func emptyBody(w stdhttp.ResponseWriter, r *stdhttp.Request) error {
	if r.Body == nil || r.Body == stdhttp.NoBody {
		return nil
	}
	b, err := io.ReadAll(stdhttp.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		var tooBig *stdhttp.MaxBytesError
		if errors.As(err, &tooBig) {
			return httpapi.NewError(stdhttp.StatusRequestEntityTooLarge, domain.CodeInvalidRequest, "the request body is too large")
		}
		return httpapi.BadRequestError("the request body could not be read")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var v struct{} // strict: any member is an error, never silently dropped
	return httpapi.DecodeJSONBytes(b, &v)
}

func resource(c *httpapi.Call) (domain.Scope, string, error) {
	scope, err := c.Scope()
	if err != nil {
		return domain.Scope{}, "", httpapi.BadRequestError("the tenant or project identifier is not valid")
	}
	return scope, c.Target.Resource.ID, nil
}

func (h *Handler) provision(w stdhttp.ResponseWriter, r *stdhttp.Request, c *httpapi.Call) {
	scope, id, err := resource(c)
	if err == nil {
		err = emptyBody(w, r)
	}
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	ws, created, err := h.svc.Provision(requestContext(r, c), actorOf(c), scope, domain.WorkItemID(id))
	if err != nil {
		httpapi.WriteError(w, r, mapError(err, ws))
		return
	}
	status := stdhttp.StatusOK
	switch {
	case ws.State == workspace.StateProvisioning:
		status = stdhttp.StatusAccepted
	case created:
		status = stdhttp.StatusCreated
	}
	h.write(w, r, c, status, ws)
}

func (h *Handler) get(w stdhttp.ResponseWriter, r *stdhttp.Request, c *httpapi.Call) {
	scope, id, err := resource(c)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	ws, err := h.svc.Get(requestContext(r, c), actorOf(c), scope, workspace.ID(id))
	if err != nil {
		httpapi.WriteError(w, r, mapError(err, nil))
		return
	}
	h.write(w, r, c, stdhttp.StatusOK, ws)
}

// mutate runs a use case that needs If-Match and no body.
func (h *Handler) mutate(w stdhttp.ResponseWriter, r *stdhttp.Request, c *httpapi.Call,
	do func(ctx contextT, actor workspace.Actor, scope domain.Scope, id workspace.ID, ifMatch domain.Version) (*workspace.Workspace, error)) {
	scope, id, err := resource(c)
	var ifMatch domain.Version
	if err == nil {
		ifMatch, err = httpapi.RequireIfMatch(r)
	}
	if err == nil {
		err = emptyBody(w, r)
	}
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	ws, err := do(requestContext(r, c), actorOf(c), scope, workspace.ID(id), ifMatch)
	if err != nil {
		httpapi.WriteError(w, r, mapError(err, ws))
		return
	}
	h.write(w, r, c, stdhttp.StatusOK, ws)
}

func (h *Handler) archive(w stdhttp.ResponseWriter, r *stdhttp.Request, c *httpapi.Call) {
	h.mutate(w, r, c, h.svc.Archive)
}

func (h *Handler) restore(w stdhttp.ResponseWriter, r *stdhttp.Request, c *httpapi.Call) {
	h.mutate(w, r, c, h.svc.Restore)
}

func (h *Handler) sync(w stdhttp.ResponseWriter, r *stdhttp.Request, c *httpapi.Call) {
	h.mutate(w, r, c, h.svc.Sync)
}

// toResource renders the workspace as the contract's resource.
func toResource(ws *workspace.Workspace, assocs []*workspace.RepositoryAssociation) Workspace {
	hosting := External
	if ws.Internal() {
		hosting = Internal
	}
	out := Workspace{
		Id: string(ws.ID), WorkItemId: string(ws.WorkItemID), Hosting: hosting, RepositoryId: string(ws.RepositoryID), Branch: ws.WorkBranch,
		State: WorkspaceState(ws.State), Version: int64(ws.Version), CreatedAt: ws.CreatedAt.UTC(), UpdatedAt: ws.UpdatedAt.UTC(),
		Repositories: make([]RepositoryAssociation, 0, len(assocs)),
	}
	for _, a := range assocs {
		r := RepositoryAssociation{
			RepositoryId: string(a.RepositoryID), TaskBranch: a.TaskBranch, BaseCommit: string(a.BaseCommit),
			State: RepositoryAssociationState(a.State), CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(),
		}
		if a.AcceptedCommit != "" {
			s := string(a.AcceptedCommit)
			r.AcceptedCommit = &s
		}
		if a.LastError != "" {
			s := a.LastError
			r.LastError = &s
		}
		out.Repositories = append(out.Repositories, r)
	}
	if ws.AcceptedCommit != "" {
		s := string(ws.AcceptedCommit)
		out.AcceptedCommit = &s
	}
	if ws.Writer != nil {
		s := string(ws.Writer.AttemptID)
		out.WriterAttemptId = &s
	}
	if p := ws.Pending; p != nil {
		out.Publication = &Publication{AttemptId: string(p.AttemptID), Commit: string(p.Commit), ExpectedCommit: string(p.ExpectedCommit), StartedAt: p.StartedAt.UTC()}
	}
	if ws.ArchiveRequestedAt != nil {
		t := ws.ArchiveRequestedAt.UTC()
		out.ArchiveRequestedAt = &t
	}
	if ws.LastError != "" {
		s := ws.LastError
		out.LastError = &s
	}
	return out
}

// write renders the workspace with its repository associations.
func (h *Handler) write(w stdhttp.ResponseWriter, r *stdhttp.Request, c *httpapi.Call, status int, ws *workspace.Workspace) {
	var assocs []*workspace.RepositoryAssociation
	if ws.Internal() {
		var err error
		if assocs, err = h.svc.Repositories(requestContext(r, c), actorOf(c), ws.Scope, ws.ID); err != nil {
			httpapi.WriteError(w, r, mapError(err, nil))
			return
		}
	}
	if err := httpapi.SetETag(w, ws.Version); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, status, toResource(ws, assocs))
}

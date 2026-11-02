package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/tasks"
)

func (s *Server) taskRoutes(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "reviewer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		list, err := s.cfg.Store.ListTasks(r.Context(), tenant, project,
			r.URL.Query().Get("state"), r.URL.Query().Get("role"))
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	case len(rest) == 0 && r.Method == http.MethodPost:
		s.createTask(w, r, tenant, project)
	case len(rest) == 1 && r.Method == http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "reviewer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		list, err := s.cfg.Store.ListTasks(r.Context(), tenant, project, "", "")
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, t := range list {
			if t.ID == rest[0] {
				writeJSON(w, http.StatusOK, t)
				return
			}
		}
		writeErr(w, http.StatusNotFound, "not_found", "unknown task")
	case len(rest) == 2 && rest[1] == "transition" && r.Method == http.MethodPost:
		s.transitionTask(w, r, tenant, project, rest[0])
	case len(rest) == 2 && rest[1] == "park" && r.Method == http.MethodPost:
		s.parkTask(w, r, tenant, project, rest[0])
	case len(rest) == 2 && rest[1] == "resolve" && r.Method == http.MethodPost:
		s.resolveTask(w, r, tenant, project, rest[0])
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID) {
	p, err := s.authorize(r, tenant, project, "engineer")
	if err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
		return
	}
	var body struct {
		Kind         string          `json:"kind"`
		Title        string          `json:"title"`
		Body         string          `json:"body"`
		State        string          `json:"state"`
		AssignedRole string          `json:"assignedRole"`
		Context      json.RawMessage `json:"context"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Context == nil {
		body.Context = json.RawMessage(`{}`)
	}
	kind := tasks.Kind(body.Kind)
	if kind == "" {
		kind = tasks.Work
	}
	now := time.Now().UTC()
	id, _ := store.NewID("tsk")
	var created tasks.Task
	err = s.cfg.Tasks.Update(r.Context(), domain.Scope{TenantID: tenant, ProjectID: project}, func(tx tasks.Tx) error {
		wf := tx.Workflow()
		state := body.State
		if state == "" {
			state = wf.InitialState
		}
		t := tasks.Task{
			Scope: domain.Scope{TenantID: tenant, ProjectID: project}, ID: domain.TaskID(id),
			Kind: kind, Title: body.Title, Body: body.Body, State: state,
			AssignedRole: body.AssignedRole, Context: body.Context, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Save(t, 0); err != nil {
			return err
		}
		created = t
		payload, _ := json.Marshal(map[string]any{"taskId": string(t.ID), "kind": string(kind), "state": state})
		return tx.Append(tasks.Event{ID: id + "_evt", TaskID: t.ID, Type: "task.created",
			ActorType: "user", ActorID: p.UserID, Payload: payload, CreatedAt: now})
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) transitionTask(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, id string) {
	p, err := s.authorize(r, tenant, project, "engineer")
	if err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
		return
	}
	var body struct {
		To      string         `json:"to"`
		Version domain.Version `json:"version"`
	}
	if !decode(w, r, &body) {
		return
	}
	now := time.Now().UTC()
	var out tasks.Task
	trigger := ""
	err = s.cfg.Tasks.Update(r.Context(), domain.Scope{TenantID: tenant, ProjectID: project}, func(tx tasks.Tx) error {
		t, err := tx.Get(domain.TaskID(id))
		if err != nil {
			return err
		}
		next, trg, err := t.Transition(tx.Workflow(), body.Version, body.To, now)
		if err != nil {
			return err
		}
		if err := tx.Save(next, body.Version); err != nil {
			return err
		}
		out, trigger = next, trg
		payload, _ := json.Marshal(map[string]any{"taskId": id, "from": t.State, "to": next.State, "triggerPipeline": trg})
		return tx.Append(tasks.Event{ID: id + "_" + strconv.FormatInt(int64(next.Version), 10), TaskID: next.ID,
			Type: "task.transitioned", ActorType: "user", ActorID: p.UserID, Payload: payload, CreatedAt: now})
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if trigger != "" {
		s.cfg.Wake()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) parkTask(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, id string) {
	p, err := s.authorize(r, tenant, project, "engineer")
	if err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
		return
	}
	var body struct {
		Version     domain.Version `json:"version"`
		ResumeState string         `json:"resumeState"`
		Blocker     struct {
			Kind         string          `json:"kind"`
			Title        string          `json:"title"`
			Body         string          `json:"body"`
			AssignedRole string          `json:"assignedRole"`
			Context      json.RawMessage `json:"context"`
		} `json:"blocker"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Blocker.Context == nil {
		body.Blocker.Context = json.RawMessage(`{}`)
	}
	now := time.Now().UTC()
	blockerID, _ := store.NewID("tsk")
	var parked tasks.Task
	err = s.cfg.Tasks.Update(r.Context(), domain.Scope{TenantID: tenant, ProjectID: project}, func(tx tasks.Tx) error {
		wf := tx.Workflow()
		parent, err := tx.Get(domain.TaskID(id))
		if err != nil {
			return err
		}
		kind := tasks.Kind(body.Blocker.Kind)
		if kind == "" || kind == tasks.Work {
			kind = tasks.Decision
		}
		blocker := tasks.Task{
			Scope: parent.Scope, ID: domain.TaskID(blockerID), Kind: kind, ParentID: parent.ID,
			Title: body.Blocker.Title, Body: body.Blocker.Body, State: wf.Inbox.OpenState,
			AssignedRole: body.Blocker.AssignedRole, Context: body.Blocker.Context,
			Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Save(blocker, 0); err != nil {
			return err
		}
		next, err := parent.Park(wf, body.Version, blocker, body.ResumeState, now)
		if err != nil {
			return err
		}
		if err := tx.Save(next, body.Version); err != nil {
			return err
		}
		parked = next
		payload, _ := json.Marshal(map[string]any{"taskId": id, "blockedByTaskId": blockerID, "assignedRole": blocker.AssignedRole})
		return tx.Append(tasks.Event{ID: id + "_" + strconv.FormatInt(int64(next.Version), 10), TaskID: next.ID,
			Type: "task.parked", ActorType: "user", ActorID: p.UserID, Payload: payload, CreatedAt: now})
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, parked)
}

func (s *Server) resolveTask(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, id string) {
	p, err := s.authorize(r, tenant, project)
	if err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "project membership required")
		return
	}
	var body struct {
		Outcome string         `json:"outcome"`
		Answer  string         `json:"answer"`
		Version domain.Version `json:"version"`
	}
	if !decode(w, r, &body) {
		return
	}
	now := time.Now().UTC()
	var resolved, resumed tasks.Task
	trigger := ""
	scope := domain.Scope{TenantID: tenant, ProjectID: project}
	err = s.cfg.Tasks.Update(r.Context(), scope, func(tx tasks.Tx) error {
		wf := tx.Workflow()
		request, err := tx.Get(domain.TaskID(id))
		if err != nil {
			return err
		}
		roles, err := s.cfg.Store.Roles(r.Context(), tenant, p.UserID, string(project))
		if err != nil {
			return err
		}
		allowed := hasRole(roles, "admin") || hasRole(roles, request.AssignedRole)
		if !allowed {
			return tasks.ErrNotFound
		}
		next, err := request.Resolve(wf, body.Version, tasks.Resolution{
			Outcome: body.Outcome, ActorID: p.UserID, Role: request.AssignedRole, Answer: body.Answer,
		}, now)
		if err != nil {
			return err
		}
		if err := tx.Save(next, body.Version); err != nil {
			return err
		}
		resolved = next
		payload, _ := json.Marshal(map[string]any{"inboxTaskId": id, "outcome": body.Outcome})
		if err := tx.Append(tasks.Event{ID: id + "_" + strconv.FormatInt(int64(next.Version), 10), TaskID: next.ID,
			Type: "task.resolved", ActorType: "user", ActorID: p.UserID, Payload: payload, CreatedAt: now}); err != nil {
			return err
		}
		// Resume the parked parent that points at this request, if any.
		parent, err := tx.GetByBlocker(next.ID)
		if err != nil {
			// A standalone inbox request has no parked parent.
			return nil
		}
		up, trg, err := parent.Resume(wf, parent.Version, next, now)
		if err != nil {
			return nil // rejection or an unanswered promotion: stay parked
		}
		if err := tx.Save(up, parent.Version); err != nil {
			return err
		}
		resumed, trigger = up, trg
		return nil
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if trigger != "" {
		s.cfg.Wake()
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": resolved, "resumed": resumed})
}

func hasRole(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

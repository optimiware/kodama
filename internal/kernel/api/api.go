// Package api is the simplified kernel's HTTP surface. Every route is backed by
// the same use cases the CLI and product MCP tools will call.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/pipeline"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/secret"
	"github.com/optimiware/kodama/internal/kernel/store"
	"github.com/optimiware/kodama/internal/tasks"
	taskpg "github.com/optimiware/kodama/internal/tasks/postgres"
	secretkeys "github.com/optimiware/kodama/pkg/secretkeys"
)

type Config struct {
	Store          *store.Store
	Auth           Authenticator
	Tasks          taskpg.Store
	Keys           secretkeys.Provider
	InstallationID string
	Backend        string
	Wake           func()
	Logger         *slog.Logger
	// Git mounts the repository routes, the credential exchange, /git/ and the
	// worker's internal routes when set (internal Git hosting, O-36).
	Git *GitServer
}

type Server struct{ cfg Config }

func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Auth == nil {
		return nil, errors.New("api: store and authenticator are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Wake == nil {
		cfg.Wake = func() {}
	}
	return &Server{cfg: cfg}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("/api/v1/", s.authed(http.HandlerFunc(s.route)))
	if s.cfg.Git != nil {
		s.cfg.Git.mount(mux)
	}
	return mux
}

func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.cfg.Auth.Authenticate(r.Context(), r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid_credential", "a valid bearer credential is required")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// route dispatches /api/v1/tenants/{tenant}/projects/{project}/<resource>.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	parts := split(path)
	if len(parts) == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "unknown route")
		return
	}
	if s.cfg.Git != nil && s.cfg.Git.serveAPI(w, r, parts) {
		return
	}
	switch parts[0] {
	case "me":
		s.me(w, r)
		return
	case "tenants":
	default:
		writeErr(w, http.StatusNotFound, "not_found", "unknown route")
		return
	}
	if len(parts) < 2 {
		s.listTenants(w, r)
		return
	}
	tenantKey := parts[1]
	tenant, err := s.cfg.Store.TenantByKey(r.Context(), tenantKey)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "unknown tenant")
		return
	}
	if len(parts) == 2 || (len(parts) == 3 && parts[2] == "projects") {
		s.projects(w, r, tenant, parts[3:])
		return
	}
	if len(parts) < 4 || parts[2] != "projects" {
		writeErr(w, http.StatusNotFound, "not_found", "unknown route")
		return
	}
	project := domain.ProjectID(parts[3])
	rest := parts[4:]
	if len(rest) == 0 {
		s.project(w, r, tenant, project)
		return
	}
	switch rest[0] {
	case "pipelines":
		s.pipelines(w, r, tenant, project, rest[1:])
	case "runs":
		s.runs(w, r, tenant, project, rest[1:])
	case "tasks":
		s.taskRoutes(w, r, tenant, project, rest[1:])
	case "secrets":
		s.secrets(w, r, tenant, project, rest[1:])
	case "artifacts":
		s.artifacts(w, r, tenant, project, rest[1:])
	default:
		writeErr(w, http.StatusNotFound, "not_found", "unknown route")
	}
}

// authorize requires one of roles in the project (or tenant scope).
func (s *Server) authorize(r *http.Request, tenant domain.TenantID, project domain.ProjectID, roles ...string) (Principal, error) {
	p, ok := principal(r.Context())
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	user := p.UserID
	if user == "" {
		id, err := s.cfg.Store.UserIDForPrincipal(r.Context(), p.Issuer, p.Subject)
		if err != nil {
			return Principal{}, ErrForbidden
		}
		user = id
		p.UserID = id
	}
	have, err := s.cfg.Store.Roles(r.Context(), tenant, user, string(project))
	if err != nil || len(have) == 0 {
		return Principal{}, ErrForbidden
	}
	if len(roles) == 0 {
		return p, nil
	}
	for _, h := range have {
		if h == "admin" {
			return p, nil
		}
		for _, want := range roles {
			if h == want {
				return p, nil
			}
		}
	}
	return Principal{}, ErrForbidden
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p, _ := principal(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"userId": p.UserID, "displayName": p.DisplayName, "issuer": p.Issuer})
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	p, _ := principal(r.Context())
	ids, err := s.cfg.Store.TenantIDs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	type tenant struct {
		ID    string   `json:"id"`
		Roles []string `json:"roles"`
	}
	out := []tenant{}
	for _, id := range ids {
		roles, err := s.cfg.Store.Roles(r.Context(), id, p.UserID, "")
		if err != nil || len(roles) == 0 {
			continue
		}
		out = append(out, tenant{ID: string(id), Roles: roles})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, rest []string) {
	if len(rest) > 0 {
		writeErr(w, http.StatusNotFound, "not_found", "unknown route")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if _, err := s.authorize(r, tenant, "", "viewer", "engineer", "reviewer", "operator"); err != nil {
			writeErr(w, http.StatusForbidden, "forbidden", "no tenant access")
			return
		}
		list, err := s.cfg.Store.ListProjects(r.Context(), tenant)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	case http.MethodPost:
		if _, err := s.authorize(r, tenant, "", "engineer"); err != nil {
			writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
			return
		}
		var body struct {
			Key         string          `json:"key"`
			DisplayName string          `json:"displayName"`
			Workflow    json.RawMessage `json:"taskWorkflow"`
		}
		if !decode(w, r, &body) {
			return
		}
		wf := body.Workflow
		if len(wf) == 0 {
			var err error
			wf, err = json.Marshal(DefaultWorkflow())
			if err != nil {
				s.fail(w, err)
				return
			}
		}
		var parsed tasks.Workflow
		if err := json.Unmarshal(wf, &parsed); err != nil || parsed.Validate() != nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_workflow", "taskWorkflow is not a valid workflow")
			return
		}
		id, err := store.NewID("p")
		if err != nil {
			s.fail(w, err)
			return
		}
		p, err := s.cfg.Store.CreateProject(r.Context(), store.Project{
			TenantID: tenant, ID: domain.ProjectID(id), Key: body.Key, DisplayName: body.DisplayName,
			Workflow: wf, Settings: json.RawMessage(`{}`),
		})
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, p)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) project(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID) {
	switch r.Method {
	case http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "reviewer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		p, err := s.cfg.Store.GetProject(r.Context(), tenant, project)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	case http.MethodPatch:
		if _, err := s.authorize(r, tenant, project, "engineer"); err != nil {
			writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
			return
		}
		expected, ok := version(w, r)
		if !ok {
			return
		}
		var body struct {
			Workflow json.RawMessage `json:"taskWorkflow"`
			Settings json.RawMessage `json:"settings"`
		}
		if !decode(w, r, &body) {
			return
		}
		if len(body.Workflow) > 0 {
			var parsed tasks.Workflow
			if err := json.Unmarshal(body.Workflow, &parsed); err != nil || parsed.Validate() != nil {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_workflow", "taskWorkflow is not a valid workflow")
				return
			}
		}
		p, err := s.cfg.Store.UpdateProject(r.Context(), tenant, project, expected, body.Workflow, body.Settings)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) pipelines(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		list, err := s.cfg.Store.ListPipelines(r.Context(), tenant, project)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	case len(rest) == 0 && r.Method == http.MethodPost:
		if _, err := s.authorize(r, tenant, project, "engineer"); err != nil {
			writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "cannot read body")
			return
		}
		doc, err := pipeline.Parse(raw)
		if err != nil {
			s.fail(w, err)
			return
		}
		canonical, err := doc.Canonical()
		if err != nil {
			s.fail(w, err)
			return
		}
		id, _ := store.NewID("pl")
		p, err := s.cfg.Store.PutPipeline(r.Context(), store.Pipeline{
			TenantID: tenant, ProjectID: project, ID: id, Name: doc.Name,
			Definition: canonical,
		})
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, p)
	case len(rest) == 1 && r.Method == http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		p, err := s.cfg.Store.GetPipelineByName(r.Context(), tenant, project, rest[0])
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		list, err := s.cfg.Store.ListRuns(r.Context(), tenant, project)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	case len(rest) == 0 && r.Method == http.MethodPost:
		s.admitRun(w, r, tenant, project)
	case len(rest) == 1 && r.Method == http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		run, err := s.cfg.Store.GetRun(r.Context(), tenant, project, rest[0])
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, run)
	case len(rest) == 2 && rest[1] == "cancel" && r.Method == http.MethodPost:
		if _, err := s.authorize(r, tenant, project, "engineer", "operator"); err != nil {
			writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
			return
		}
		run, err := s.cfg.Store.RequestCancel(r.Context(), tenant, project, rest[0])
		if err != nil {
			s.fail(w, err)
			return
		}
		s.cfg.Wake()
		writeJSON(w, http.StatusAccepted, run)
	case len(rest) == 2 && rest[1] == "logs" && r.Method == http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		s.logs(w, r, tenant, project, rest[0])
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) admitRun(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID) {
	if _, err := s.authorize(r, tenant, project, "engineer"); err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
		return
	}
	var body struct {
		Pipeline string          `json:"pipeline"`
		Inputs   json.RawMessage `json:"inputs"`
		TaskID   string          `json:"taskId"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Inputs == nil {
		body.Inputs = json.RawMessage(`{}`)
	}
	p, err := s.cfg.Store.GetPipelineByName(r.Context(), tenant, project, body.Pipeline)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "unknown pipeline")
		return
	}
	if p.Disabled {
		writeErr(w, http.StatusConflict, "pipeline_disabled", "the pipeline is disabled")
		return
	}
	var doc pipeline.Document
	if err := json.Unmarshal(p.Definition, &doc); err != nil {
		s.fail(w, err)
		return
	}
	order, err := doc.Order()
	if err != nil {
		s.fail(w, err)
		return
	}
	steps := make([]store.Step, 0, len(order))
	byName := map[string]pipeline.Step{}
	for _, st := range doc.Steps {
		byName[st.Name] = st
	}
	for i, name := range order {
		steps = append(steps, store.Step{ID: name, StepKey: name, Ordinal: i, Image: byName[name].Image})
	}
	reqKey := r.Header.Get("Idempotency-Key")
	principalID, requestHash := "", ""
	if reqKey != "" {
		p2, _ := principal(r.Context())
		principalID = p2.UserID
		body, _ := json.Marshal(struct {
			Pipeline string          `json:"pipeline"`
			Inputs   json.RawMessage `json:"inputs"`
			TaskID   string          `json:"taskId"`
		}{body.Pipeline, body.Inputs, body.TaskID})
		sum := sha256.Sum256(body)
		requestHash = hex.EncodeToString(sum[:])
	}
	runID, _ := store.NewID("run")
	run, err := s.cfg.Store.AdmitRun(r.Context(), store.Run{
		TenantID: tenant, ProjectID: project, ID: runID, PipelineID: p.ID, TaskID: body.TaskID,
		Definition: p.Definition, Inputs: body.Inputs, ExecutionOrder: order,
	}, steps, "", reqKey, principalID, requestHash)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.cfg.Wake()
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, runID string) {
	run, err := s.cfg.Store.GetRun(r.Context(), tenant, project, runID)
	if err != nil {
		s.fail(w, err)
		return
	}
	step := r.URL.Query().Get("step")
	for _, st := range run.Steps {
		if step != "" && st.StepKey != step {
			continue
		}
		if st.LogURI == "" {
			continue
		}
		path := strings.TrimPrefix(st.LogURI, "file://")
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.Copy(w, io.LimitReader(f, 4<<20))
		return
	}
	writeErr(w, http.StatusNotFound, "not_found", "no retained log")
}

func (s *Server) secrets(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		list, err := s.cfg.Store.ListSecrets(r.Context(), tenant, project)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	case len(rest) == 1 && r.Method == http.MethodPut:
		if _, err := s.authorize(r, tenant, project, "engineer"); err != nil {
			writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
			return
		}
		var body struct {
			Value      string `json:"value"`
			TenantWide bool   `json:"tenantWide"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Value == "" {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "value is required")
			return
		}
		scopeProject := domain.ProjectID(project)
		if body.TenantWide {
			if _, err := s.authorize(r, tenant, "", "admin"); err != nil {
				writeErr(w, http.StatusForbidden, "forbidden", "admin required for a tenant secret")
				return
			}
			scopeProject = ""
		}
		id, _ := store.NewID("sec")
		current, _ := s.cfg.Store.SecretVersion(r.Context(), tenant, scopeProject, rest[0])
		nextVersion := current + 1
		existingID := id
		if current > 0 {
			// Reuse the identity so the new revision is the same logical secret.
			if list, err := s.cfg.Store.ListSecrets(r.Context(), tenant, scopeProject); err == nil {
				for _, sec := range list {
					if sec.Name == rest[0] && sec.ProjectID == scopeProject {
						existingID = sec.ID
					}
				}
			}
		}
		id = existingID
		ctx := secret.Context{InstallationID: s.cfg.InstallationID, TenantID: string(tenant),
			ProjectID: string(scopeProject), SecretID: id, Name: rest[0], Version: nextVersion}
		ciphertext, envelope, keyID, err := secret.Seal(r.Context(), s.cfg.Keys, ctx, []byte(body.Value))
		if err != nil {
			s.fail(w, err)
			return
		}
		sec, err := s.cfg.Store.PutSecret(r.Context(), tenant, scopeProject, id, rest[0], keyID, nextVersion, ciphertext, envelope)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, sec)
	case len(rest) == 1 && r.Method == http.MethodDelete:
		if _, err := s.authorize(r, tenant, project, "engineer"); err != nil {
			writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
			return
		}
		if err := s.cfg.Store.RevokeSecret(r.Context(), tenant, project, rest[0]); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) artifacts(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, project domain.ProjectID, rest []string) {
	if len(rest) != 0 {
		methodNotAllowed(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if _, err := s.authorize(r, tenant, project, "viewer", "engineer", "operator"); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return
		}
		list, err := s.cfg.Store.ListArtifacts(r.Context(), tenant, project)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	case http.MethodPost:
		if _, err := s.authorize(r, tenant, project, "engineer", "operator"); err != nil {
			writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
			return
		}
		var body store.Artifact
		if !decode(w, r, &body) {
			return
		}
		body.TenantID, body.ProjectID = tenant, project
		if body.ID == "" {
			body.ID, _ = store.NewID("art")
		}
		a, err := s.cfg.Store.RegisterArtifact(r.Context(), body)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, a)
	default:
		methodNotAllowed(w)
	}
}

// ---- helpers ----------------------------------------------------------------

func split(p string) []string {
	var out []string
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return false
	}
	return true
}

func version(w http.ResponseWriter, r *http.Request) (domain.Version, bool) {
	raw := strings.Trim(r.Header.Get("If-Match"), `"`)
	if raw == "" {
		writeErr(w, http.StatusPreconditionRequired, "version_required", "If-Match is required")
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		writeErr(w, http.StatusPreconditionFailed, "version_conflict", "If-Match is not a version")
		return 0, false
	}
	return domain.Version(n), true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, domain.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", "conflict")
	case errors.Is(err, domain.ErrInvalidValue):
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", err.Error())
	default:
		var diag *pipeline.Diagnostic
		if errors.As(err, &diag) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{
				"code": diag.Code, "message": diag.Message, "path": diag.Path}})
			return
		}
		if errors.Is(err, runner.ErrSecret) || errors.Is(err, runner.ErrTimeout) {
			writeErr(w, http.StatusServiceUnavailable, "unavailable", err.Error())
			return
		}
		s.cfg.Logger.Error("request failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "internal", "the request failed")
	}
}

// DefaultWorkflow is the recommended starting workflow for a new project. It is
// a normal project value: every state name can be renamed by the owner.
func DefaultWorkflow() tasks.Workflow {
	return tasks.Workflow{
		InitialState:   "plan",
		States:         []string{"plan", "implement", "review", "test", "preview", "promote", "done", "question"},
		TerminalStates: []string{"done"},
		Inbox:          tasks.Inbox{OpenState: "question", ResolvedState: "done"},
		Transitions: []tasks.Transition{
			{From: "plan", To: "implement"}, {From: "implement", To: "review"},
			{From: "review", To: "implement"}, {From: "review", To: "test"},
			{From: "test", To: "preview"}, {From: "preview", To: "promote"},
			{From: "promote", To: "done"},
		},
		Triggers: map[string]string{
			"plan": "plan", "implement": "implement", "review": "review",
			"test": "test", "preview": "preview", "promote": "promote",
		},
	}
}

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitservice"
	"github.com/optimiware/kodama/internal/shell/store"
)

// ForwardingSigner signs the forwarding assertion the Git route sends to a
// tenant's worker (internal/gitauth/jwt.Signer is the production signer).
type ForwardingSigner interface {
	SignForwarding(ctx context.Context, a gitv1.ForwardingAssertion) (string, error)
}

// WorkerIdentityVerifier verifies a worker's tenant-bound identity on the
// control plane's internal routes (internal/gitauth/jwt.Verifier).
type WorkerIdentityVerifier interface {
	VerifyWorkerIdentity(ctx context.Context, token string, pathTenant domain.TenantID, now time.Time) (gitv1.WorkerIdentity, error)
}

// GitWorkers locates a tenant's Git worker: the handler that serves its
// assertion-authenticated smart HTTP endpoints, and the control plane's RPC to it.
type GitWorkers interface {
	WorkerRPC
	GitHandler(ctx context.Context, tenant domain.TenantID) (http.Handler, error)
}

// GitConfig configures the Git hosting routes.
type GitConfig struct {
	Store   store.GitBackend
	Auth    Authenticator
	Workers GitWorkers
	Signer  ForwardingSigner
	// Identities verifies worker identities on /internal/git/v1; nil refuses
	// every internal call (default deny).
	Identities WorkerIdentityVerifier
	// PublicURL is the externally reachable base of this server, used to render
	// clone URLs. It never carries credentials.
	PublicURL string
	Now       func() time.Time
	Logger    *slog.Logger
	// OnEvent, when set, is called after a receive's outcome committed an event,
	// so the outbox publisher drains now instead of at its next interval.
	OnEvent func()

	// Credential lifetimes; zero takes the default.
	HumanTTL time.Duration
	SeedTTL  time.Duration
	RunTTL   time.Duration
}

// Default credential lifetimes (the contract bounds are 1h, 15m and 24h).
const (
	defaultHumanTTL  = 15 * time.Minute
	defaultSeedTTL   = 10 * time.Minute
	defaultRunTTL    = time.Hour
	assertionTTL     = 4 * time.Minute
	credentialPrefix = "gitc_"
)

// GitServer serves the repository resource, the credential exchange, the /git/
// smart-HTTP forwarding and the worker's internal control routes.
type GitServer struct {
	cfg     GitConfig
	control *GitControl
}

// NewGit validates the configuration.
func NewGit(cfg GitConfig) (*GitServer, error) {
	if cfg.Store == nil || cfg.Auth == nil || cfg.Workers == nil || cfg.Signer == nil {
		return nil, errors.New("api: git hosting requires a store, an authenticator, workers and a forwarding signer")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.HumanTTL <= 0 {
		cfg.HumanTTL = defaultHumanTTL
	}
	if cfg.SeedTTL <= 0 || cfg.SeedTTL > gitv1.MaxSeedLifetime {
		cfg.SeedTTL = defaultSeedTTL
	}
	if cfg.RunTTL <= 0 {
		cfg.RunTTL = defaultRunTTL
	}
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	control := NewGitControl(cfg.Store, cfg.Workers, cfg.Now, cfg.Logger)
	control.SetOnEvent(cfg.OnEvent)
	return &GitServer{cfg: cfg, control: control}, nil
}

// Control returns the worker-facing control plane (for in-process workers).
func (g *GitServer) Control() *GitControl { return g.control }

// Handler serves every Git route on its own: the authenticated API subset, the
// Git endpoint and the internal worker routes. The kernel Server mounts the same
// pieces into its own mux (see Server.Handler).
func (g *GitServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := g.cfg.Auth.Authenticate(r.Context(), r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid_credential", "a valid bearer credential is required")
			return
		}
		r = r.WithContext(WithPrincipal(r.Context(), p))
		if !g.serveAPI(w, r, split(strings.TrimPrefix(r.URL.Path, "/api/v1/"))) {
			writeErr(w, http.StatusNotFound, "not_found", "unknown route")
		}
	}))
	g.mount(mux)
	return mux
}

// mount registers the routes that do their own authentication.
func (g *GitServer) mount(mux *http.ServeMux) {
	mux.HandleFunc("/git/", g.serveGit)
	mux.HandleFunc(gitInternalPrefix, g.serveInternal)
}

// ---- helpers ----------------------------------------------------------------

func jsonUnmarshalClaims(raw []byte, into *GitClaims) error { return json.Unmarshal(raw, into) }

// hasRole is a literal role check: admin implies nothing here, because an
// administrator gains no implicit push authority (identity and RBAC).
func holdsRole(have []string, want ...string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

var repositoryNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$`)

// refsCursorPattern is the Git contract's cursor (contracts/git/v1): the name of
// the last ref of a page.
var refsCursorPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{1,512}$`)

var pipelineNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// gitError maps a store error to its HTTP answer.
func (g *GitServer) gitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrVersionConflict):
		writeErr(w, http.StatusPreconditionFailed, "version_conflict", "the repository changed; fetch it and retry")
	case errors.Is(err, store.ErrRepositoryNotReady):
		writeErr(w, http.StatusConflict, "repository_not_ready", "the repository is not ready")
	case errors.Is(err, store.ErrRepositoryArchived):
		writeErr(w, http.StatusConflict, "repository_archived", "the repository is archived")
	case errors.Is(err, store.ErrRepositoryNotEmpty):
		writeErr(w, http.StatusConflict, "repository_not_empty", "the repository already has refs")
	case errors.Is(err, store.ErrSeedPending):
		writeErr(w, http.StatusConflict, "seed_pending", "an earlier seed push is not recorded yet; retry after the worker reconciles")
	case errors.Is(err, domain.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", "conflict")
	case errors.Is(err, domain.ErrInvalidValue):
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", err.Error())
	default:
		g.cfg.Logger.Error("git request failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "internal", "the request failed")
	}
}

// caller resolves the authenticated principal to a user ID.
func (g *GitServer) caller(r *http.Request) (string, bool) {
	p, ok := principal(r.Context())
	if !ok {
		return "", false
	}
	if p.UserID != "" {
		return p.UserID, true
	}
	id, err := g.cfg.Store.UserIDForPrincipal(r.Context(), p.Issuer, p.Subject)
	return id, err == nil
}

// ---- repository resource ----------------------------------------------------

// repositoryView is the repository resource (docs/contracts/git-service.md).
type repositoryView struct {
	ID            string                 `json:"id"`
	TenantID      string                 `json:"tenantId"`
	ProjectID     *string                `json:"projectId"`
	Path          string                 `json:"path"`
	DefaultBranch string                 `json:"defaultBranch"`
	Status        string                 `json:"status"`
	Reason        *string                `json:"reason"`
	CloneURL      string                 `json:"cloneUrl"`
	Policy        store.RepositoryPolicy `json:"policy"`
	Empty         bool                   `json:"empty"`
	Version       int64                  `json:"version"`
	CreatedAt     string                 `json:"createdAt"`
}

// RepositoryPath renders <tenant>/context or <tenant>/<project>/<name>.
func RepositoryPath(tenantKey, projectKey, name string) string {
	if projectKey == "" {
		return tenantKey + "/" + name
	}
	return tenantKey + "/" + projectKey + "/" + name
}

func (g *GitServer) view(tenantKey, projectKey string, r store.Repository) repositoryView {
	path := RepositoryPath(tenantKey, projectKey, r.Name)
	v := repositoryView{ID: r.ID, TenantID: string(r.TenantID), Path: path, DefaultBranch: r.DefaultBranch, Status: r.Status,
		CloneURL: g.cfg.PublicURL + "/git/" + path + ".git", Policy: r.Policy, Empty: r.Empty, Version: int64(r.Version),
		CreatedAt: ts(r.CreatedAt)}
	if r.ProjectID != "" {
		p := string(r.ProjectID)
		v.ProjectID = &p
	}
	if r.Reason != "" {
		reason := r.Reason
		v.Reason = &reason
	} else if r.Status == store.RepoProvisioning {
		reason := "provisioning"
		v.Reason = &reason
	}
	return v
}

// serveAPI handles the repository and credential routes below /api/v1; it
// reports false for any other path.
//
//	/tenants/{t}/repositories[/{id}[/archive|restore|refs]]
//	/tenants/{t}/projects/{p}/repositories[/{id}[/archive|restore|refs]]
//	/tenants/{t}/git-credentials
func (g *GitServer) serveAPI(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if len(parts) < 3 || parts[0] != "tenants" {
		return false
	}
	var projectSeg string
	var rest []string
	switch {
	case parts[2] == "git-credentials" && len(parts) == 3:
	case parts[2] == "repositories":
		rest = parts[3:]
	case parts[2] == "projects" && len(parts) >= 5 && parts[4] == "repositories":
		projectSeg, rest = parts[3], parts[5:]
	default:
		return false
	}
	tenant, err := g.cfg.Store.TenantByKey(r.Context(), parts[1])
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "unknown tenant")
		return true
	}
	if parts[2] == "git-credentials" {
		g.credentials(w, r, tenant)
		return true
	}
	g.repositories(w, r, parts[1], tenant, projectSeg, rest)
	return true
}

// resolveProject maps the URL's project segment to the project: the documented
// route names it by key, the kernel's other routes by ID, so the key is tried
// first. Authorization always uses the resolved project's immutable ID.
func (g *GitServer) resolveProject(ctx context.Context, tenant domain.TenantID, segment string) (store.Project, error) {
	p, err := g.cfg.Store.GetProjectByKey(ctx, tenant, segment)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return p, err
	}
	return g.cfg.Store.GetProject(ctx, tenant, domain.ProjectID(segment))
}

// scope resolves the project of a repository route and authorizes the caller in
// it. A caller with no role in the scope, and an unknown project, see not found;
// one with a role that does not suffice is forbidden. A person bound only to the
// project is authorized by that binding, whether the URL names the project by key
// or by ID. It reports false after answering.
func (g *GitServer) scope(w http.ResponseWriter, r *http.Request, tenant domain.TenantID, projectSeg string, write bool) (store.Project, string, bool) {
	user, ok := g.caller(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return store.Project{}, "", false
	}
	var project store.Project
	if projectSeg != "" {
		var err error
		if project, err = g.resolveProject(r.Context(), tenant, projectSeg); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown project")
			return store.Project{}, "", false
		}
	}
	roles, err := g.cfg.Store.Roles(r.Context(), tenant, user, string(project.ID))
	if err != nil || len(roles) == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return store.Project{}, "", false
	}
	if write && !holdsRole(roles, "engineer", "admin") {
		writeErr(w, http.StatusForbidden, "forbidden", "engineer or admin required")
		return store.Project{}, "", false
	}
	return project, user, true
}

type policyInput struct {
	DirectPushToDefault *bool   `json:"directPushToDefault"`
	IntegrationPipeline *string `json:"integrationPipeline"`
}

func (p *policyInput) validate() error {
	if p != nil && p.IntegrationPipeline != nil && !pipelineNamePattern.MatchString(*p.IntegrationPipeline) {
		return errors.New("integrationPipeline is not a process name")
	}
	return nil
}

func (g *GitServer) repositories(w http.ResponseWriter, r *http.Request, tenantKey string, tenant domain.TenantID, projectSeg string, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		project, _, ok := g.scope(w, r, tenant, projectSeg, false)
		if !ok {
			return
		}
		list, err := g.cfg.Store.ListRepositories(r.Context(), tenant, project.ID)
		if err != nil {
			g.gitError(w, err)
			return
		}
		items := make([]repositoryView, 0, len(list))
		for _, repo := range list {
			items = append(items, g.view(tenantKey, project.Key, repo))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": nil})
	case len(rest) == 0 && r.Method == http.MethodPost:
		g.createRepository(w, r, tenantKey, tenant, projectSeg)
	case len(rest) >= 1:
		project, user, ok := g.scope(w, r, tenant, projectSeg, r.Method != http.MethodGet)
		if !ok {
			return
		}
		repo, err := g.cfg.Store.GetRepository(r.Context(), tenant, rest[0])
		if err != nil || repo.ProjectID != project.ID {
			writeErr(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		g.repositoryItem(w, r, tenantKey, project, user, repo, rest[1:])
	default:
		methodNotAllowed(w)
	}
}

func (g *GitServer) createRepository(w http.ResponseWriter, r *http.Request, tenantKey string, tenant domain.TenantID, projectSeg string) {
	project, _, ok := g.scope(w, r, tenant, projectSeg, true)
	if !ok {
		return
	}
	var body struct {
		Name          string       `json:"name"`
		DefaultBranch *string      `json:"defaultBranch"`
		Policy        *policyInput `json:"policy"`
	}
	if !decode(w, r, &body) {
		return
	}
	branch := gitv1.DefaultBranchName
	if body.DefaultBranch != nil {
		branch = *body.DefaultBranch
	}
	switch {
	case !repositoryNamePattern.MatchString(body.Name) || len(body.Name) > 512:
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "name must be slash-separated segments of letters, digits, underscores and hyphens")
		return
	case project.ID == "" && body.Name != "context":
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "the tenant-level repository is named context")
		return
	case gitv1.ValidateBranchName(branch) != nil:
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "defaultBranch is not a valid branch name")
		return
	}
	if err := body.Policy.validate(); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", err.Error())
		return
	}
	policy := store.RepositoryPolicy{}
	if body.Policy != nil {
		if body.Policy.DirectPushToDefault != nil {
			policy.DirectPushToDefault = *body.Policy.DirectPushToDefault
		}
		if body.Policy.IntegrationPipeline != nil {
			policy.IntegrationPipeline = *body.Policy.IntegrationPipeline
		}
	}
	id, err := store.NewID("repo")
	if err != nil {
		g.gitError(w, err)
		return
	}
	repo, created, err := g.cfg.Store.CreateRepository(r.Context(), store.Repository{
		TenantID: tenant, ProjectID: project.ID, ID: id, Name: body.Name, DefaultBranch: branch, Policy: policy})
	if err != nil {
		g.gitError(w, err)
		return
	}
	if !created && (repo.DefaultBranch != branch || repo.Policy != policy) {
		writeErr(w, http.StatusConflict, "repository_name_taken", "a repository with this name exists with a different configuration")
		return
	}
	repo = g.provision(r.Context(), tenant, repo)
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, g.view(tenantKey, project.Key, repo))
}

// provision asks the tenant's worker to initialize the repository and records its
// confirmation. It is best effort and idempotent on the repository ID: when the
// worker is unavailable the repository stays provisioning and the worker's
// assignment poll finishes it.
func (g *GitServer) provision(ctx context.Context, tenant domain.TenantID, repo store.Repository) store.Repository {
	if repo.Status != store.RepoProvisioning {
		return repo
	}
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := g.cfg.Workers.Initialize(ictx, tenant, gitv1.InitializeRepositoryRequest{RepositoryID: repo.ID, DefaultBranch: repo.DefaultBranch})
	if err != nil {
		g.cfg.Logger.Warn("repository initialization is pending", "repository", repo.ID, "error", err)
		return repo
	}
	confirmed, err := g.cfg.Store.ConfirmInitialized(ctx, tenant, repo.ID, repo.DefaultBranch, res.Empty)
	if err != nil {
		g.cfg.Logger.Warn("repository confirmation is pending", "repository", repo.ID, "error", err)
		return repo
	}
	return confirmed
}

func (g *GitServer) repositoryItem(w http.ResponseWriter, r *http.Request, tenantKey string, project store.Project, user string, repo store.Repository, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, g.view(tenantKey, project.Key, repo))
	case len(rest) == 0 && r.Method == http.MethodPatch:
		expected, ok := version(w, r)
		if !ok {
			return
		}
		var body struct {
			DefaultBranch *string      `json:"defaultBranch"`
			Policy        *policyInput `json:"policy"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.DefaultBranch == nil && body.Policy == nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "a patch changes defaultBranch or policy")
			return
		}
		if body.DefaultBranch != nil && gitv1.ValidateBranchName(*body.DefaultBranch) != nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "defaultBranch is not a valid branch name")
			return
		}
		if err := body.Policy.validate(); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", err.Error())
			return
		}
		patch := store.RepositoryPatch{DefaultBranch: body.DefaultBranch}
		if body.Policy != nil {
			if body.Policy.DirectPushToDefault == nil {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "a patched policy states directPushToDefault")
				return
			}
			p := store.RepositoryPolicy{DirectPushToDefault: *body.Policy.DirectPushToDefault}
			if body.Policy.IntegrationPipeline != nil {
				p.IntegrationPipeline = *body.Policy.IntegrationPipeline
			}
			patch.Policy = &p
		}
		updated, err := g.cfg.Store.UpdateRepository(r.Context(), repo.TenantID, repo.ID, expected, patch)
		if err != nil {
			g.gitError(w, err)
			return
		}
		// A changed default branch put the repository back into provisioning so
		// that the worker re-applies HEAD; ask it now, the assignment poll repeats it.
		updated = g.provision(r.Context(), repo.TenantID, updated)
		writeJSON(w, http.StatusOK, g.view(tenantKey, project.Key, updated))
	case len(rest) == 1 && (rest[0] == "archive" || rest[0] == "restore") && r.Method == http.MethodPost:
		to, reason := store.RepoArchived, "archived"
		if rest[0] == "restore" {
			to, reason = store.RepoReady, ""
		}
		updated, err := g.cfg.Store.SetRepositoryStatus(r.Context(), repo.TenantID, repo.ID, to, reason, "user", user)
		if err != nil {
			g.gitError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, g.view(tenantKey, project.Key, updated))
	case len(rest) == 1 && rest[0] == "refs" && r.Method == http.MethodGet:
		g.refs(w, r, repo)
	default:
		methodNotAllowed(w)
	}
}

// refs lists the branch or tag tips from the tenant's worker (bounded, cursor
// paginated). Server refs are never listed.
func (g *GitServer) refs(w http.ResponseWriter, r *http.Request, repo store.Repository) {
	if repo.Status == store.RepoProvisioning {
		writeErr(w, http.StatusConflict, "repository_not_ready", "the repository is not ready")
		return
	}
	prefix := gitv1.HeadsPrefix
	switch r.URL.Query().Get("prefix") {
	case "", "heads":
	case "tags":
		prefix = gitv1.TagsPrefix
	default:
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "prefix is heads or tags")
		return
	}
	limit := int64(100)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || n > gitv1.MaxPage {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "limit is between 1 and 1000")
			return
		}
		limit = n
	}
	req := gitv1.ReadRefsRequest{RepositoryID: repo.ID, Prefix: prefix, Limit: limit}
	if c := r.URL.Query().Get("cursor"); c != "" {
		if !refsCursorPattern.MatchString(c) {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "cursor is not a position of this listing")
			return
		}
		req.Cursor = &c
	}
	res, err := g.cfg.Workers.ReadRefs(r.Context(), repo.TenantID, req)
	if err != nil {
		g.cfg.Logger.Warn("reading refs failed", "repository", repo.ID, "error", err)
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "the repository's worker is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- credential exchange ----------------------------------------------------

// credentialResponse is the exchange answer: HTTP Basic with any username and the
// credential as password, shown once.
type credentialResponse struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	GrantID   string `json:"grantId"`
	ExpiresAt string `json:"expiresAt"`
}

func (g *GitServer) credentials(w http.ResponseWriter, r *http.Request, tenant domain.TenantID) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := g.caller(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	var body struct {
		ProjectID    string `json:"projectId"`
		RepositoryID string `json:"repositoryId"`
		Seed         bool   `json:"seed"`
	}
	if !decode(w, r, &body) {
		return
	}
	repo, err := g.cfg.Store.GetRepository(r.Context(), tenant, body.RepositoryID)
	if err != nil || string(repo.ProjectID) != body.ProjectID {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	roles, err := g.cfg.Store.Roles(r.Context(), tenant, user, string(repo.ProjectID))
	if err != nil || len(roles) == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	engineer := holdsRole(roles, "engineer")
	if body.Seed && !engineer {
		writeErr(w, http.StatusForbidden, "forbidden", "a seed grant requires the engineer role on the project")
		return
	}
	if repo.Status == store.RepoProvisioning {
		g.gitError(w, store.ErrRepositoryNotReady)
		return
	}
	now := g.cfg.Now().UTC()
	id, err := randomHex(12)
	if err != nil {
		g.gitError(w, err)
		return
	}
	secret, err := randomHex(32)
	if err != nil {
		g.gitError(w, err)
		return
	}
	kind, ttl := store.CredHuman, g.cfg.HumanTTL
	rc := GitGrantRepoClaim{ID: repo.ID, Read: true, Update: []string{}}
	switch {
	case body.Seed:
		kind, ttl = store.CredSeed, g.cfg.SeedTTL
		rc.Seed, rc.Update = true, []string{gitv1.HeadsPrefix + repo.DefaultBranch}
	case engineer:
		rc.Update = humanCeiling()
	}
	grantID := "gg_" + id
	claims := GitClaims{GrantID: grantID, Audience: gitv1.GrantAudience, TenantID: string(tenant), ProjectID: string(repo.ProjectID),
		Actor: GitActor{Type: "user", ID: user}, Kind: kind, Repositories: []GitGrantRepoClaim{rc},
		IssuedAt: ts(now), ExpiresAt: ts(now.Add(ttl))}
	raw, err := json.Marshal(claims)
	if err != nil {
		g.gitError(w, err)
		return
	}
	cred := store.Credential{TenantID: tenant, ProjectID: repo.ProjectID, ID: grantID, RepoID: repo.ID, Kind: kind,
		Digest: store.DigestOf(credentialPrefix + secret), Claims: raw, UserID: user, IssuedAt: now, ExpiresAt: now.Add(ttl)}
	if body.Seed {
		err = g.cfg.Store.IssueSeed(r.Context(), cred)
	} else {
		err = g.cfg.Store.IssueCredential(r.Context(), cred)
	}
	if err != nil {
		g.gitError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, credentialResponse{Username: "kodama", Password: credentialPrefix + secret, GrantID: grantID, ExpiresAt: ts(now.Add(ttl))})
}

// IssuedCredential is a run credential handed to the run's runtime context.
type IssuedCredential struct {
	GrantID   string
	Secret    string
	ExpiresAt time.Time
}

// ContextRepository is the name of a tenant's or a project's knowledge base
// repository (<tenant>/context, <tenant>/<project>/context). A run may read it and
// never update it.
const ContextRepository = "context"

// isContextOf reports whether repo is the tenant's or the project's knowledge base.
func isContextOf(repo store.Repository, project domain.ProjectID) bool {
	return repo.Name == ContextRepository && (repo.ProjectID == "" || repo.ProjectID == project)
}

// IssueRunCredential derives a run's Git credential from the run's pinned sources
// (docs/contracts/git-service.md, "Source set"): read for a pinned repository,
// update only for the run's own branches and the branch of a write source. The
// tenant's and the project's context repositories are not sources: the run may
// read them, whether or not one is also pinned, and its credential for them never
// names an update. It is issued only while the run is live, and ends with the run.
func (g *GitServer) IssueRunCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, repositoryID string) (IssuedCredential, error) {
	run, err := g.cfg.Store.RunGitInfo(ctx, tenant, project, runID)
	if err != nil {
		return IssuedCredential{}, err
	}
	if !run.Active() {
		return IssuedCredential{}, store.ErrConflict
	}
	repo, err := g.cfg.Store.GetRepository(ctx, tenant, repositoryID)
	if errors.Is(err, store.ErrNotFound) {
		return IssuedCredential{}, store.ErrNotFound
	}
	if err != nil {
		// A failure to read is not an answer: a caller that treats not found as
		// revocation must not mistake an outage for one.
		return IssuedCredential{}, err
	}
	if isContextOf(repo, project) {
		return g.issueWorkload(ctx, tenant, project, run, repo, store.CredRun, []string{}, nil)
	}
	if repo.ProjectID != project {
		return IssuedCredential{}, store.ErrNotFound
	}
	var sources []struct {
		RepositoryID string  `json:"repositoryId"`
		Access       string  `json:"access"`
		Branch       *string `json:"branch"`
	}
	if err := json.Unmarshal(run.Sources, &sources); err != nil {
		return IssuedCredential{}, err
	}
	pinned, write := false, false
	update := []string{}
	add := func(p string) {
		for _, have := range update {
			if have == p {
				return
			}
		}
		update = append(update, p)
	}
	for _, s := range sources {
		if s.RepositoryID != repositoryID {
			continue
		}
		pinned = true
		if s.Access == string(gitv1.AccessWrite) {
			write = true
			if s.Branch != nil {
				add(*s.Branch)
			}
		}
	}
	if !pinned {
		return IssuedCredential{}, store.ErrNotFound
	}
	if write {
		add(runBranchPrefix + runID + "/*")
		add(agentBranchPrefix + runID + "/*")
		if run.TaskID != "" {
			add(taskBranchPrefix + run.TaskID)
		}
	}
	return g.issueWorkload(ctx, tenant, project, run, repo, store.CredRun, update, nil)
}

// IssueIntegrationCredential issues the credential that lets a run advance the
// default branch from from to to, once: only for the repository's configured
// integration pipeline and only while the run's task is approved (no task, or one
// that is not approved, is store.ErrTaskNotApproved).
func (g *GitServer) IssueIntegrationCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, repositoryID, from, to string) (IssuedCredential, error) {
	run, err := g.cfg.Store.RunGitInfo(ctx, tenant, project, runID)
	if err != nil {
		return IssuedCredential{}, err
	}
	if !run.Active() {
		return IssuedCredential{}, store.ErrConflict
	}
	repo, err := g.cfg.Store.GetRepository(ctx, tenant, repositoryID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return IssuedCredential{}, err
	}
	if err != nil || repo.ProjectID != project {
		return IssuedCredential{}, store.ErrNotFound
	}
	if repo.Policy.IntegrationPipeline == "" || repo.Policy.IntegrationPipeline != run.PipelineName {
		return IssuedCredential{}, store.ErrConflict
	}
	// The run's task must be approved now; it is read again at every receive.
	if run.TaskID == "" {
		return IssuedCredential{}, store.ErrTaskNotApproved
	}
	if approved, err := g.cfg.Store.TaskApproved(ctx, tenant, project, run.TaskID); err != nil {
		return IssuedCredential{}, err
	} else if !approved {
		return IssuedCredential{}, store.ErrTaskNotApproved
	}
	if err := (gitv1.RefUpdate{Ref: gitv1.HeadsPrefix + repo.DefaultBranch, Old: from, New: to}).Validate(); err != nil {
		return IssuedCredential{}, domain.ErrInvalidValue
	}
	ref := gitv1.HeadsPrefix + repo.DefaultBranch
	return g.issueWorkload(ctx, tenant, project, run, repo, store.CredIntegration, []string{ref}, &GitIntegration{Ref: ref, From: from, To: to})
}

func (g *GitServer) issueWorkload(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, run store.RunGitInfo, repo store.Repository, kind string, update []string, integration *GitIntegration) (IssuedCredential, error) {
	now := g.cfg.Now().UTC()
	id, err := randomHex(12)
	if err != nil {
		return IssuedCredential{}, err
	}
	secret, err := randomHex(32)
	if err != nil {
		return IssuedCredential{}, err
	}
	runID := run.RunID
	grantID := "gg_" + id
	expires := now.Add(g.cfg.RunTTL)
	claims := GitClaims{GrantID: grantID, Audience: gitv1.GrantAudience, TenantID: string(tenant), ProjectID: string(project),
		Actor: GitActor{Type: "workload", ID: runID, RunID: &runID}, Kind: kind,
		Repositories: []GitGrantRepoClaim{{ID: repo.ID, Read: true, Update: update, Integration: integration}},
		IssuedAt:     ts(now), ExpiresAt: ts(expires)}
	raw, err := json.Marshal(claims)
	if err != nil {
		return IssuedCredential{}, err
	}
	err = g.cfg.Store.IssueCredential(ctx, store.Credential{TenantID: tenant, ProjectID: project, ID: grantID, RepoID: repo.ID, Kind: kind,
		Digest: store.DigestOf(credentialPrefix + secret), Claims: raw, RunID: runID, IssuedAt: now, ExpiresAt: expires})
	if err != nil {
		return IssuedCredential{}, err
	}
	return IssuedCredential{GrantID: grantID, Secret: credentialPrefix + secret, ExpiresAt: expires}, nil
}

// ---- /git/ forwarding -------------------------------------------------------

const gitInternalPrefix = "/internal/git/v1/"

func gitChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="kodama-git", charset="UTF-8"`)
	writeErr(w, http.StatusUnauthorized, "invalid_credential", "a valid Git credential is required")
}

func gitNotFound(w http.ResponseWriter) { writeErr(w, http.StatusNotFound, "not_found", "not found") }

// gitCredentialSecret reads the credential from HTTP Basic (any username, the
// credential as password) or a bearer token.
func gitCredentialSecret(r *http.Request) string {
	if _, password, ok := r.BasicAuth(); ok {
		return password
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	return ""
}

// gitRequest is a parsed /git/ path: the repository path segments and the Git
// endpoint after "<path>.git/".
type gitRequest struct {
	segments []string
	endpoint string
	receive  bool
}

// parseGitRequest accepts exactly the smart HTTP endpoints; everything else (dumb
// HTTP, upload-archive, other paths and methods) is not served.
func parseGitRequest(r *http.Request) (gitRequest, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/git/")
	if !ok {
		return gitRequest{}, false
	}
	i := strings.Index(rest, ".git/")
	if i < 0 {
		return gitRequest{}, false
	}
	req := gitRequest{segments: strings.Split(rest[:i], "/"), endpoint: rest[i+len(".git/"):]}
	if len(req.segments) < 2 {
		return gitRequest{}, false
	}
	for _, s := range req.segments {
		if s == "" {
			return gitRequest{}, false
		}
	}
	q := r.URL.Query()
	switch req.endpoint {
	case "info/refs":
		services := q["service"]
		if r.Method != http.MethodGet || len(q) != 1 || len(services) != 1 {
			return gitRequest{}, false
		}
		switch services[0] {
		case "git-upload-pack":
		case "git-receive-pack":
			req.receive = true
		default:
			return gitRequest{}, false
		}
	case "git-upload-pack", "git-receive-pack":
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			return gitRequest{}, false
		}
		req.receive = req.endpoint == "git-receive-pack"
	default:
		return gitRequest{}, false
	}
	return req, true
}

// resolveRepository maps the path to a repository: <tenant>/context is the
// tenant-level repository, <tenant>/<project>/<name> a project's. It never
// creates anything.
func (g *GitServer) resolveRepository(ctx context.Context, tenant domain.TenantID, segments []string) (store.Repository, error) {
	if len(segments) == 2 {
		if segments[1] != "context" {
			return store.Repository{}, store.ErrNotFound
		}
		return g.cfg.Store.RepositoryByPath(ctx, tenant, "", "context")
	}
	return g.cfg.Store.RepositoryByPath(ctx, tenant, segments[1], strings.Join(segments[2:], "/"))
}

// mayRead re-reads the authority behind a credential at this request: a person
// must still hold a role on the project, a run must still be live.
func (g *GitServer) mayRead(ctx context.Context, tenant domain.TenantID, cred store.Credential, repo store.Repository) (engineer, ok bool) {
	switch cred.Kind {
	case store.CredHuman, store.CredSeed:
		roles, err := g.cfg.Store.Roles(ctx, tenant, cred.UserID, string(repo.ProjectID))
		return err == nil && holdsRole(roles, "engineer"), err == nil && len(roles) > 0
	case store.CredRun, store.CredIntegration:
		// The run's project is the credential's: a run's credential for the tenant's
		// context repository is read with the run's project, not the repository's.
		run, err := g.cfg.Store.RunGitInfo(ctx, tenant, cred.ProjectID, cred.RunID)
		return false, err == nil && run.Active()
	}
	return false, false
}

// serveGit authenticates a smart-HTTP request, resolves its repository, signs a
// short-lived forwarding assertion and streams the request to the tenant's
// worker. The caller's credential is never forwarded. An unknown, foreign or
// unauthorized repository is the same 404 once authenticated.
func (g *GitServer) serveGit(w http.ResponseWriter, r *http.Request) {
	req, ok := parseGitRequest(r)
	if !ok {
		gitNotFound(w)
		return
	}
	secret := gitCredentialSecret(r)
	if secret == "" {
		gitChallenge(w)
		return
	}
	ctx := r.Context()
	tenant, err := g.cfg.Store.TenantByKey(ctx, req.segments[0])
	if err != nil {
		gitChallenge(w) // an unknown tenant is indistinguishable from a bad credential
		return
	}
	cred, err := g.cfg.Store.CredentialByDigest(ctx, tenant, store.DigestOf(secret))
	now := g.cfg.Now().UTC()
	if err != nil || !cred.Live(now) {
		gitChallenge(w)
		return
	}
	repo, err := g.resolveRepository(ctx, tenant, req.segments)
	if err != nil || repo.ID != cred.RepoID {
		gitNotFound(w)
		return
	}
	var claims GitClaims
	if json.Unmarshal(cred.Claims, &claims) != nil || claims.TenantID != string(tenant) || claims.Audience != gitv1.GrantAudience {
		gitNotFound(w)
		return
	}
	engineer, may := g.mayRead(ctx, tenant, cred, repo)
	if !may {
		gitNotFound(w)
		return
	}
	// receivePack is true only for a credential that may update a ref: its record
	// has an update, a seed or an integration, and a person is an engineer now.
	receive := claims.mayUpdateARef(repo.ID)
	if cred.Kind == store.CredHuman || cred.Kind == store.CredSeed {
		receive = receive && engineer
	}
	if req.receive && !receive {
		gitNotFound(w)
		return
	}
	requestID, err := randomHex(8)
	if err != nil {
		g.gitError(w, err)
		return
	}
	token, err := g.cfg.Signer.SignForwarding(ctx, gitv1.ForwardingAssertion{
		Audience: gitv1.WorkerAudience(string(tenant)), TenantID: string(tenant), RepositoryID: repo.ID, GrantID: cred.ID,
		ReceivePack: receive, IssuedAt: gitv1.Timestamp(ts(now)), ExpiresAt: gitv1.Timestamp(ts(now.Add(assertionTTL))),
		RequestID: "req_" + requestID,
	})
	if err != nil {
		g.cfg.Logger.Error("signing the forwarding assertion failed", "error", err)
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "the Git service is unavailable")
		return
	}
	worker, err := g.cfg.Workers.GitHandler(ctx, tenant)
	if err != nil {
		g.cfg.Logger.Warn("the tenant's Git worker is unavailable", "tenant", tenant, "error", err)
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "the tenant's Git worker is unavailable")
		return
	}
	fwd := r.Clone(ctx)
	fwd.URL.Path = "/" + repo.ID + ".git/" + req.endpoint
	fwd.URL.RawPath = ""
	fwd.RequestURI = ""
	fwd.Header.Del("Authorization")
	fwd.Header.Set(gitservice.AssertionHeader, token)
	worker.ServeHTTP(w, fwd)
}

// ---- internal worker routes -------------------------------------------------

// serveInternal serves /internal/git/v1/tenants/{tenantId}/…: the worker-facing
// control plane. Every call carries the worker's tenant-bound identity, which is
// refused on another tenant's path; nothing else is routed.
func (g *GitServer) serveInternal(w http.ResponseWriter, r *http.Request) {
	parts := split(strings.TrimPrefix(r.URL.Path, gitInternalPrefix))
	if len(parts) < 3 || parts[0] != "tenants" {
		gitContractErr(w, ce(domain.CodeNotFound, "not found"))
		return
	}
	tenant := domain.TenantID(parts[1])
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || g.cfg.Identities == nil || tenant.Validate() != nil {
		gitContractErr(w, ce(domain.CodeInvalidCredential, "a worker identity is required"))
		return
	}
	if _, err := g.cfg.Identities.VerifyWorkerIdentity(r.Context(), strings.TrimSpace(token), tenant, g.cfg.Now()); err != nil {
		var contract *gitv1.ContractError
		if errors.As(err, &contract) && (contract.Code == domain.CodeForbidden || contract.Code == domain.CodeUnavailable) {
			gitContractErr(w, ce(contract.Code, "the worker identity was refused"))
			return
		}
		gitContractErr(w, ce(domain.CodeInvalidCredential, "the worker identity was refused"))
		return
	}
	rest := parts[2:]
	ctx := r.Context()
	switch {
	case len(rest) == 1 && rest[0] == "assignments" && r.Method == http.MethodGet:
		res, err := g.control.Assignments(ctx, tenant)
		gitContractReply(w, res, err)
	case len(rest) == 1 && rest[0] == "pushes" && r.Method == http.MethodPost:
		req, ok := decodeGit[gitv1.PushAuthorizeRequest](w, r)
		if !ok {
			return
		}
		res, err := g.control.Authorize(ctx, tenant, req)
		gitContractReply(w, res, err)
	case len(rest) == 3 && rest[0] == "pushes" && rest[2] == "commit-intent" && r.Method == http.MethodPost:
		req, ok := decodeGit[gitv1.CommitIntentRequest](w, r)
		if !ok {
			return
		}
		res, err := g.control.CommitIntent(ctx, tenant, rest[1], req)
		gitContractReply(w, res, err)
	case len(rest) == 3 && rest[0] == "pushes" && rest[2] == "outcome" && r.Method == http.MethodPost:
		req, ok := decodeGit[gitv1.OutcomeRequest](w, r)
		if !ok {
			return
		}
		res, err := g.control.Outcome(ctx, tenant, rest[1], req)
		gitContractReply(w, res, err)
	case len(rest) == 3 && rest[0] == "repositories" && rest[2] == "initialized" && r.Method == http.MethodPost:
		req, ok := decodeGit[gitv1.InitializedRequest](w, r)
		if !ok {
			return
		}
		if req.RepositoryID != rest[1] {
			gitContractErr(w, ce(domain.CodeInvalidRequest, "repositoryId does not match the path"))
			return
		}
		res, err := g.control.Initialized(ctx, tenant, req)
		gitContractReply(w, res, err)
	default:
		gitContractErr(w, ce(domain.CodeNotFound, "not found"))
	}
}

// decodeGit strictly decodes a worker request body with the contract's decoder.
func decodeGit[T any, P interface {
	*T
	Validate() error
}](w http.ResponseWriter, r *http.Request) (T, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	var zero T
	if err != nil {
		gitContractErr(w, ce(domain.CodeInvalidRequest, "the request body is unreadable or too large"))
		return zero, false
	}
	v, err := gitv1.DecodeRequest[T, P](data)
	if err != nil {
		gitContractErr(w, err)
		return zero, false
	}
	return v, true
}

func gitContractReply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		gitContractErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// gitContractErr writes a contract error in the contract's body shape.
func gitContractErr(w http.ResponseWriter, err error) {
	var contract *gitv1.ContractError
	if !errors.As(err, &contract) {
		contract = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "the control plane could not complete the request"}
	}
	status, known := gitv1.Status(contract.Code)
	if !known {
		status = http.StatusInternalServerError
	}
	msg := contract.Message
	if msg == "" {
		msg = string(contract.Code)
	}
	id, _ := randomHex(8)
	writeJSON(w, status, gitv1.ErrorResponse{Error: gitv1.ErrorBody{Code: contract.Code, Message: msg, RequestID: "req_" + id, Details: map[string]any{}}})
}

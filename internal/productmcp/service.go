package productmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Limits bound what a tool accepts and what delegation it may start. They are
// explicit policy: a zero value in Config selects DefaultLimits.
type Limits struct {
	// MaxActiveProvisioning bounds a project's active provisioning runs, whoever
	// started them.
	MaxActiveProvisioning int
	// MaxProvisionInputs, MaxInputKeyBytes, MaxInputValueBytes and
	// MaxInputBytes bound a provisioning run's string inputs.
	MaxProvisionInputs int
	MaxInputKeyBytes   int
	MaxInputValueBytes int
	MaxInputBytes      int
	// MaxPage is the largest page a list tool asks for; DefaultPage the default.
	MaxPage     int
	DefaultPage int
	// MaxCriteria bounds the acceptance criteria of one create call.
	MaxCriteria int
	// MaxCursorBytes bounds an opaque cursor argument.
	MaxCursorBytes int
	// Field bounds of the content a tool creates. The work and identity use
	// cases validate again; these keep an agent's payload bounded before any
	// port is called.
	MaxTitleChars       int
	MaxSpecBytes        int
	MaxCriterionIDChars int
	MaxCriterionChars   int
	MaxDisplayNameChars int
	MaxProjectKeyChars  int
}

// DefaultLimits are conservative defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxActiveProvisioning: 2,
		MaxProvisionInputs:    16, MaxInputKeyBytes: 64, MaxInputValueBytes: 4 << 10, MaxInputBytes: 32 << 10,
		MaxPage: 100, DefaultPage: 50, MaxCriteria: 100, MaxCursorBytes: 1024,
		MaxTitleChars: 200, MaxSpecBytes: 64 << 10, MaxCriterionIDChars: 64, MaxCriterionChars: 2000, MaxDisplayNameChars: 200, MaxProjectKeyChars: 63,
	}
}

func (l Limits) validate() error {
	for _, v := range []int{l.MaxActiveProvisioning, l.MaxProvisionInputs, l.MaxInputKeyBytes, l.MaxInputValueBytes, l.MaxInputBytes, l.MaxPage, l.DefaultPage, l.MaxCriteria, l.MaxCursorBytes,
		l.MaxTitleChars, l.MaxSpecBytes, l.MaxCriterionIDChars, l.MaxCriterionChars, l.MaxDisplayNameChars, l.MaxProjectKeyChars} {
		if v < 1 {
			return fmt.Errorf("%w: every limit must be positive", ErrNotConfigured)
		}
	}
	if l.DefaultPage > l.MaxPage {
		return fmt.Errorf("%w: the default page exceeds the maximum", ErrNotConfigured)
	}
	return nil
}

// Config wires a Service. Every port is required: there is no default that
// grants access or reaches a module.
type Config struct {
	Repositories Repositories
	Projects     Projects
	WorkItems    WorkItems
	Provisioning Provisioning
	Results      Results
	// Limits is DefaultLimits when zero.
	Limits Limits
	Logger *slog.Logger
}

// Service is the tool layer: it validates tool arguments, binds every call to
// its Caller's tenant and project, applies idempotency, and delegates to the
// consumer-owned ports. It holds no identity of its own.
type Service struct {
	repos Repositories
	projs Projects
	items WorkItems
	prov  Provisioning
	res   Results
	lim   Limits
	log   *slog.Logger
}

// NewService validates cfg.
func NewService(cfg Config) (*Service, error) {
	missing := func(n string) error { return fmt.Errorf("%w: %s", ErrNotConfigured, n) }
	switch {
	case cfg.Repositories == nil:
		return nil, missing("Repositories")
	case cfg.Projects == nil:
		return nil, missing("Projects")
	case cfg.WorkItems == nil:
		return nil, missing("WorkItems")
	case cfg.Provisioning == nil:
		return nil, missing("Provisioning")
	case cfg.Results == nil:
		return nil, missing("Results")
	}
	if cfg.Limits == (Limits{}) {
		cfg.Limits = DefaultLimits()
	}
	if err := cfg.Limits.validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{repos: cfg.Repositories, projs: cfg.Projects, items: cfg.WorkItems, prov: cfg.Provisioning, res: cfg.Results, lim: cfg.Limits, log: cfg.Logger}, nil
}

// ---- caller and scope --------------------------------------------------------

// person admits only a person: project administration, seed requests and run
// delegation are never available to a run's attempt.
func person(c Caller, what string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.IsWorkload() {
		return Refuse(ErrDenied, "forbidden", "%s is not available to a run's attempt", what)
	}
	return nil
}

// scope binds a call to the caller's tenant and a project. The tenant is always
// the caller's. A workload may name only its run's project (omitting it selects
// that project); any other is not_found, as is a foreign one.
func scope(c Caller, project string) (domain.Scope, error) {
	if err := c.Validate(); err != nil {
		return domain.Scope{}, err
	}
	if c.IsWorkload() {
		if project == "" {
			project = string(c.Project)
		}
		if domain.ProjectID(project) != c.Project {
			return domain.Scope{}, ErrNotFound
		}
	}
	p := domain.ProjectID(project)
	if err := p.Validate(); err != nil {
		return domain.Scope{}, invalid("projectId is required and must be a valid identifier")
	}
	return domain.Scope{TenantID: c.Tenant, ProjectID: p}, nil
}

var (
	idemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,64}$`)
	idPattern      = regexp.MustCompile(domain.IDPattern)
)

func checkKey(k string) error {
	if !idemKeyPattern.MatchString(k) {
		return invalid("idempotencyKey must be 1 to 64 characters from A-Z a-z 0-9 . _ ~ : -")
	}
	return nil
}

func checkID(what, v string) error {
	if !idPattern.MatchString(v) {
		return invalid("%s is required and must be a valid identifier", what)
	}
	return nil
}

// checkText bounds a text argument: valid UTF-8 without control characters
// (newlines and tabs allowed), at most maxChars characters (0: unbounded) and
// maxBytes bytes. A violation is the stable invalid_argument.
func (s *Service) checkText(what, v string, maxChars, maxBytes int) error {
	switch {
	case !utf8.ValidString(v):
		return invalidArgument("%s is not valid UTF-8", what)
	case maxBytes > 0 && len(v) > maxBytes:
		return invalidArgument("%s exceeds %d bytes", what, maxBytes)
	case maxChars > 0 && utf8.RuneCountInString(v) > maxChars:
		return invalidArgument("%s exceeds %d characters", what, maxChars)
	case strings.ContainsFunc(v, func(r rune) bool { return (r < 0x20 && r != '\n' && r != '\t' && r != '\r') || r == 0x7f }):
		return invalidArgument("%s contains control characters", what)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) checkCursor(c *string) error {
	if c != nil && (*c == "" || len(*c) > s.lim.MaxCursorBytes) {
		return invalid("cursor is not valid")
	}
	return nil
}

// fail logs the classified outcome (never arguments or results) and returns err.
func (s *Service) fail(ctx context.Context, tool string, c Caller, err error) error {
	code, _ := classify(err)
	s.log.LogAttrs(ctx, slog.LevelInfo, "product mcp tool refused", slog.String("tool", tool), slog.String("code", code), slog.String("request_id", c.RequestID))
	return err
}

// ---- idempotent create ------------------------------------------------------

func principalOf(c Caller) string { return IdempotencyPrincipal(c) }

func requestHash(tool string, project domain.ProjectID, req any) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", invalid("the request cannot be encoded")
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", tool, project)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// portKey is the idempotency key handed to the use case: namespaced by tool so
// it can never collide with a key an HTTP client chose. The use case scopes it
// by principal, tenant and route like every other keyed create.
func portKey(tool, key string) string { return "mcp." + tool + "." + key }

// createOnce runs one idempotent create. A recorded key replays: the request
// hash must match (another request is idempotency_conflict) and the resource is
// reread through read, which authorizes the caller again, so a revoked caller
// gets neither the resource nor a stored copy. A new key creates through the
// use case (which receives the same key, making the create exactly-once in its
// own transaction) and then records the resource identifier. A failed record
// does not fail the call: the create happened, and a retry converges through
// the use case's key.
func createOnce[T any](ctx context.Context, s *Service, c Caller, project domain.ProjectID, tool, key string, req any,
	create func(ctx context.Context, portKey string) (T, string, error),
	read func(ctx context.Context, id string) (T, error)) (out T, replayed bool, err error) {
	hash, err := requestHash(tool, project, req)
	if err != nil {
		return out, false, err
	}
	rk := ReplayKey{Tenant: c.Tenant, Principal: principalOf(c), Project: project, Tool: tool, Key: key}
	rec, found, err := s.res.Lookup(ctx, rk)
	if err != nil {
		return out, false, fmt.Errorf("%w: idempotency lookup", ErrUnavailable)
	}
	if found {
		return replay(ctx, rec, hash, read)
	}
	out, id, err := create(ctx, portKey(tool, key))
	if err != nil {
		return out, false, err
	}
	won, err := s.res.Record(ctx, rk, Recorded{RequestHash: hash, ResourceID: id})
	switch {
	case err != nil:
		s.log.LogAttrs(ctx, slog.LevelWarn, "product mcp idempotency record failed", slog.String("tool", tool), slog.String("request_id", c.RequestID))
		return out, false, nil
	case won.RequestHash != hash:
		return out, false, ErrIdempotencyConflict
	case won.ResourceID != id:
		return replay(ctx, won, hash, read)
	}
	return out, false, nil
}

func replay[T any](ctx context.Context, rec Recorded, hash string, read func(context.Context, string) (T, error)) (out T, replayed bool, err error) {
	if rec.RequestHash != hash {
		return out, false, ErrIdempotencyConflict
	}
	out, err = read(ctx, rec.ResourceID)
	return out, err == nil, err
}

// ---- projects ---------------------------------------------------------------

func projectOut(p Project) ProjectOut {
	return ProjectOut{ID: string(p.ID), Key: p.Key, DisplayName: p.DisplayName, ActiveWIPLimit: p.ActiveWIPLimit, Version: p.Version}
}

// CreateProject creates a project in the caller's tenant (a person with
// project:create only; the port decides).
func (s *Service) CreateProject(ctx context.Context, c Caller, in ProjectCreateIn) (ProjectOut, error) {
	const tool = "kodama_project_create"
	if err := person(c, "creating a project"); err != nil {
		return ProjectOut{}, s.fail(ctx, tool, c, err)
	}
	if err := checkKey(in.IdempotencyKey); err != nil {
		return ProjectOut{}, s.fail(ctx, tool, c, err)
	}
	if in.Key == "" || in.ActiveWIPLimit < 0 {
		return ProjectOut{}, s.fail(ctx, tool, c, invalid("key is required and activeWipLimit cannot be negative"))
	}
	if err := firstErr(
		s.checkText("key", in.Key, s.lim.MaxProjectKeyChars, 4*s.lim.MaxProjectKeyChars),
		s.checkText("displayName", in.DisplayName, s.lim.MaxDisplayNameChars, 4*s.lim.MaxDisplayNameChars)); err != nil {
		return ProjectOut{}, s.fail(ctx, tool, c, err)
	}
	req := CreateProjectRequest{Key: in.Key, DisplayName: in.DisplayName, ActiveWIPLimit: in.ActiveWIPLimit}
	out, replayed, err := createOnce(ctx, s, c, "", tool, in.IdempotencyKey, req,
		func(ctx context.Context, pk string) (ProjectOut, string, error) {
			p, err := s.projs.Create(ctx, c, pk, req)
			return projectOut(p), string(p.ID), err
		},
		func(ctx context.Context, id string) (ProjectOut, error) {
			p, err := s.projs.Get(ctx, c, domain.ProjectID(id))
			return projectOut(p), err
		})
	out.Replayed = replayed
	if err != nil {
		return ProjectOut{}, s.fail(ctx, tool, c, err)
	}
	return out, nil
}

// GetProject reads a project the caller may read.
func (s *Service) GetProject(ctx context.Context, c Caller, in ProjectGetIn) (ProjectOut, error) {
	const tool = "kodama_project_get"
	if err := person(c, "reading a project"); err != nil {
		return ProjectOut{}, s.fail(ctx, tool, c, err)
	}
	if err := checkID("projectId", in.ProjectID); err != nil {
		return ProjectOut{}, s.fail(ctx, tool, c, err)
	}
	p, err := s.projs.Get(ctx, c, domain.ProjectID(in.ProjectID))
	if err != nil {
		return ProjectOut{}, s.fail(ctx, tool, c, err)
	}
	return projectOut(p), nil
}

// ListProjects lists the projects the caller may read.
func (s *Service) ListProjects(ctx context.Context, c Caller, in ProjectListIn) (ProjectListOut, error) {
	const tool = "kodama_project_list"
	if err := person(c, "listing projects"); err != nil {
		return ProjectListOut{}, s.fail(ctx, tool, c, err)
	}
	if err := s.checkCursor(in.Cursor); err != nil {
		return ProjectListOut{}, s.fail(ctx, tool, c, err)
	}
	l, err := s.projs.List(ctx, c, in.Cursor)
	if err == nil && len(l.Items) > s.lim.MaxPage {
		err = malformed() // a port returned more than a page
	}
	if err != nil {
		return ProjectListOut{}, s.fail(ctx, tool, c, err)
	}
	out := ProjectListOut{Items: make([]ProjectOut, 0, len(l.Items)), NextCursor: l.NextCursor}
	for _, p := range l.Items {
		out.Items = append(out.Items, projectOut(p))
	}
	return out, nil
}

// ---- repositories -----------------------------------------------------------

func repositoryOut(r gitv1.Repository) RepositoryOut {
	return RepositoryOut{
		ID: r.ID, Name: r.Name, DefaultBranch: r.DefaultBranch, Hosting: string(r.Hosting), Status: string(r.Status), Reason: r.Reason,
		CloneURL: r.CloneURL, DirectPushToDefault: r.Policy.DirectPushToDefault, Empty: r.Empty, Version: r.Version,
		CreatedAt: string(r.CreatedAt),
	}
}

// CreateRepository creates an internally hosted repository in the caller's
// project. A person needs engineer on the project; a provisioning attempt acts
// only in its run's project. The new repository is empty: an engineer seeds it
// with RequestSeed and an ordinary git push.
func (s *Service) CreateRepository(ctx context.Context, c Caller, in RepositoryCreateIn) (RepositoryOut, error) {
	const tool = "kodama_repository_create"
	sc, err := scope(c, in.ProjectID)
	if err == nil {
		err = checkKey(in.IdempotencyKey)
	}
	req := gitv1.CreateRepositoryRequest{Name: in.Name, DefaultBranch: in.DefaultBranch}
	if in.DirectPushToDefault != nil {
		req.Policy = &gitv1.RepositoryPolicyInput{DirectPushToDefault: in.DirectPushToDefault}
	}
	if err == nil {
		if verr := req.Validate(); verr != nil {
			err = invalid("%v", verr)
		}
	}
	if err != nil {
		return RepositoryOut{}, s.fail(ctx, tool, c, err)
	}
	// The hash covers the effective request: an omitted default branch or policy
	// is the same request as spelling the default out.
	effective := struct {
		Name, DefaultBranch string
		DirectPushToDefault bool
	}{req.Name, req.EffectiveDefaultBranch(), gitv1.EffectivePolicy(req.Policy).DirectPushToDefault}
	out, replayed, err := createOnce(ctx, s, c, sc.ProjectID, tool, in.IdempotencyKey, effective,
		func(ctx context.Context, pk string) (RepositoryOut, string, error) {
			r, err := s.repos.Create(ctx, c, sc, pk, req)
			if err == nil {
				err = checkRepository(r)
			}
			return repositoryOut(r), r.ID, err
		},
		func(ctx context.Context, id string) (RepositoryOut, error) {
			r, err := s.repos.Get(ctx, c, sc, id)
			if err == nil {
				err = checkRepository(r)
			}
			return repositoryOut(r), err
		})
	out.Replayed = replayed
	if err != nil {
		return RepositoryOut{}, s.fail(ctx, tool, c, err)
	}
	return out, nil
}

// GetRepository reads a repository of the caller's project.
func (s *Service) GetRepository(ctx context.Context, c Caller, in RepositoryGetIn) (RepositoryOut, error) {
	const tool = "kodama_repository_get"
	sc, err := scope(c, in.ProjectID)
	if err == nil {
		err = checkID("repositoryId", in.RepositoryID)
	}
	if err != nil {
		return RepositoryOut{}, s.fail(ctx, tool, c, err)
	}
	r, err := s.repos.Get(ctx, c, sc, in.RepositoryID)
	if err == nil {
		err = checkRepository(r)
	}
	if err != nil {
		return RepositoryOut{}, s.fail(ctx, tool, c, err)
	}
	return repositoryOut(r), nil
}

// ListRepositories lists the repositories of the caller's project.
func (s *Service) ListRepositories(ctx context.Context, c Caller, in RepositoryListIn) (RepositoryListOut, error) {
	const tool = "kodama_repository_list"
	sc, err := scope(c, in.ProjectID)
	if err == nil {
		err = s.checkCursor(in.Cursor)
	}
	if err != nil {
		return RepositoryListOut{}, s.fail(ctx, tool, c, err)
	}
	l, err := s.repos.List(ctx, c, sc, in.Cursor)
	if err == nil && (l.Validate() != nil || len(l.Items) > s.lim.MaxPage) {
		err = malformed()
	}
	if err != nil {
		return RepositoryListOut{}, s.fail(ctx, tool, c, err)
	}
	out := RepositoryListOut{Items: make([]RepositoryOut, 0, len(l.Items)), NextCursor: l.NextCursor}
	for _, r := range l.Items {
		out.Items = append(out.Items, repositoryOut(r))
	}
	return out, nil
}

// RequestSeed issues a person's single-use seed grant for an empty, ready,
// internally hosted repository and says how to push. It never serves a run's
// attempt (T-150 issues a run's seed grant through the runtime route, only for
// repositories that run created) and never stores or logs the credential: it is
// returned exactly once, to this caller.
func (s *Service) RequestSeed(ctx context.Context, c Caller, in SeedIn) (SeedOut, error) {
	const tool = "kodama_repository_request_seed"
	err := person(c, "requesting a seed grant")
	var sc domain.Scope
	if err == nil {
		sc, err = scope(c, in.ProjectID)
	}
	if err == nil {
		err = checkID("repositoryId", in.RepositoryID)
	}
	if err != nil {
		return SeedOut{}, s.fail(ctx, tool, c, err)
	}
	repo, err := s.repos.Get(ctx, c, sc, in.RepositoryID)
	if err == nil {
		err = checkRepository(repo)
	}
	if err != nil {
		return SeedOut{}, s.fail(ctx, tool, c, err)
	}
	if err := seedable(repo); err != nil {
		return SeedOut{}, s.fail(ctx, tool, c, err)
	}
	cred, err := s.repos.RequestSeed(ctx, c, sc, in.RepositoryID)
	if err != nil {
		return SeedOut{}, s.fail(ctx, tool, c, err)
	}
	if cred.Validate() != nil {
		return SeedOut{}, s.fail(ctx, tool, c, malformed())
	}
	return SeedOut{
		RepositoryID: repo.ID, CloneURL: repo.CloneURL, DefaultBranch: repo.DefaultBranch,
		Username: cred.Username, Password: cred.Password, GrantID: cred.GrantID,
		ExpiresAt:    string(cred.ExpiresAt),
		Instructions: seedInstructions(repo),
	}, nil
}

// malformed is a dependency answer that broke its contract: never passed on.
func malformed() error {
	return fmt.Errorf("%w: a dependency answered outside its contract", ErrUnavailable)
}

func checkRepository(r gitv1.Repository) error {
	if r.Validate() != nil {
		return malformed()
	}
	return nil
}

func seedable(r gitv1.Repository) error {
	switch {
	case r.Hosting != gitv1.HostingInternal:
		return Refuse(ErrInvalid, "hosting_mismatch", "only an internally hosted repository can be seeded")
	case r.Status == gitv1.StatusArchived:
		return Refuse(ErrConflict, "repository_archived", "the repository is archived")
	case r.Status != gitv1.StatusReady:
		return Refuse(ErrConflict, "repository_not_ready", "the repository is still provisioning; retry shortly")
	case !r.Empty:
		return Refuse(ErrConflict, "repository_not_empty", "the repository already has refs and cannot be seeded")
	}
	return nil
}

func seedInstructions(r gitv1.Repository) []string {
	return []string{
		"Run these in the local clone that holds the history to bring over; Kodama never imports or mirrors a provider repository.",
		fmt.Sprintf("git remote add kodama %s", r.CloneURL),
		fmt.Sprintf("git push kodama <local-branch>:refs/heads/%s", r.DefaultBranch),
		"When Git asks, enter the returned username and password; never put the password in the URL, a command line or a file.",
		fmt.Sprintf("The grant allows exactly one push whose only update creates %s; a push that also names any other ref is refused whole (seed_only) and leaves the repository empty. It expires at expiresAt and cannot be reused.", r.DefaultBranch),
		"Afterwards push other branches and tags with an ordinary Git credential. The default branch is then protected and advances only by integration unless the repository opted into direct pushes.",
	}
}

// ---- work items -------------------------------------------------------------

func workItemOut(w WorkItem) WorkItemOut {
	return WorkItemOut{ID: string(w.ID), Kind: w.Kind, Title: w.Title, State: w.State, Version: w.Version, ContentRevision: w.ContentRevision, CreatedAt: w.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

func validKind(k string) bool {
	switch k {
	case "epic", "story", "task", "defect":
		return true
	}
	return false
}

// CreateWorkItem creates a work item in backlog; a task unless another kind is
// named. The work use case authorizes work_item:create and validates the
// content; a replay rereads the item under the caller's current read authority.
func (s *Service) CreateWorkItem(ctx context.Context, c Caller, in WorkItemCreateIn) (WorkItemOut, error) {
	const tool = "kodama_work_item_create"
	sc, err := scope(c, in.ProjectID)
	if err == nil {
		err = checkKey(in.IdempotencyKey)
	}
	if in.Kind == "" {
		in.Kind = "task"
	}
	switch {
	case err != nil:
	case !validKind(in.Kind):
		err = invalid("kind must be epic, story, task or defect")
	case in.Title == "":
		err = invalid("title is required")
	case len(in.AcceptanceCriteria) > s.lim.MaxCriteria:
		err = invalidArgument("at most %d acceptance criteria", s.lim.MaxCriteria)
	default:
		err = firstErr(
			s.checkText("title", in.Title, s.lim.MaxTitleChars, 4*s.lim.MaxTitleChars),
			s.checkText("spec", in.Spec, 0, s.lim.MaxSpecBytes))
		for _, cr := range in.AcceptanceCriteria {
			if err != nil {
				break
			}
			if cr.ID == "" || cr.Text == "" {
				err = invalid("every acceptance criterion needs an id and a text")
				break
			}
			err = firstErr(
				s.checkText("criterion id", cr.ID, s.lim.MaxCriterionIDChars, s.lim.MaxCriterionIDChars),
				s.checkText("criterion text", cr.Text, s.lim.MaxCriterionChars, 4*s.lim.MaxCriterionChars))
		}
	}
	if err != nil {
		return WorkItemOut{}, s.fail(ctx, tool, c, err)
	}
	req := CreateWorkItemRequest{Kind: in.Kind, Title: in.Title, Spec: in.Spec}
	for _, cr := range in.AcceptanceCriteria {
		req.Criteria = append(req.Criteria, Criterion{ID: cr.ID, Text: cr.Text})
	}
	out, replayed, err := createOnce(ctx, s, c, sc.ProjectID, tool, in.IdempotencyKey, req,
		func(ctx context.Context, pk string) (WorkItemOut, string, error) {
			w, err := s.items.Create(ctx, c, sc, pk, req)
			return workItemOut(w), string(w.ID), err
		},
		func(ctx context.Context, id string) (WorkItemOut, error) {
			w, err := s.items.Get(ctx, c, sc, domain.WorkItemID(id))
			return workItemOut(w), err
		})
	out.Replayed = replayed
	if err != nil {
		return WorkItemOut{}, s.fail(ctx, tool, c, err)
	}
	return out, nil
}

// GetWorkItem reads a work item of the caller's project.
func (s *Service) GetWorkItem(ctx context.Context, c Caller, in WorkItemGetIn) (WorkItemOut, error) {
	const tool = "kodama_work_item_get"
	sc, err := scope(c, in.ProjectID)
	if err == nil {
		err = checkID("workItemId", in.WorkItemID)
	}
	if err != nil {
		return WorkItemOut{}, s.fail(ctx, tool, c, err)
	}
	w, err := s.items.Get(ctx, c, sc, domain.WorkItemID(in.WorkItemID))
	if err != nil {
		return WorkItemOut{}, s.fail(ctx, tool, c, err)
	}
	return workItemOut(w), nil
}

// ListWorkItems lists the work items of the caller's project.
func (s *Service) ListWorkItems(ctx context.Context, c Caller, in WorkItemListIn) (WorkItemListOut, error) {
	const tool = "kodama_work_item_list"
	sc, err := scope(c, in.ProjectID)
	if err == nil {
		err = s.checkCursor(in.Cursor)
	}
	if in.Limit == 0 {
		in.Limit = s.lim.DefaultPage
	}
	if err == nil && (in.Limit < 1 || in.Limit > s.lim.MaxPage) {
		err = invalid("limit must be between 1 and %d", s.lim.MaxPage)
	}
	if err == nil {
		for _, k := range in.Kinds {
			if !validKind(k) {
				err = invalid("kinds holds an unknown kind")
			}
		}
		if len(in.States) > 6 || len(in.Kinds) > 4 {
			err = invalid("too many state or kind filters")
		}
	}
	if err != nil {
		return WorkItemListOut{}, s.fail(ctx, tool, c, err)
	}
	l, err := s.items.List(ctx, c, sc, WorkItemQuery{States: in.States, Kinds: in.Kinds, Cursor: in.Cursor, Limit: in.Limit})
	if err == nil && len(l.Items) > in.Limit {
		err = malformed() // a port that ignores the requested limit is not trusted
	}
	if err != nil {
		return WorkItemListOut{}, s.fail(ctx, tool, c, err)
	}
	out := WorkItemListOut{Items: make([]WorkItemOut, 0, len(l.Items)), NextCursor: l.NextCursor}
	for _, w := range l.Items {
		out.Items = append(out.Items, workItemOut(w))
	}
	return out, nil
}

// ---- provisioning delegation ------------------------------------------------

// StartProvisioningRun starts a provisioning run (origin provision) in the
// caller's project. It is bounded three ways: the scope is the project a person
// names and holds run:manage in (the port authorizes the caller, as the HTTP
// route does); the inputs are bounded in count and size; and the project's
// active provisioning runs are bounded by Limits.MaxActiveProvisioning, enforced
// atomically by the admission. It never accepts or returns a credential, never
// issues a seed grant (T-150 issues a run's seed through the runtime route, for
// repositories the run creates) and is refused for a run's attempt, so a
// provisioning run cannot start further runs.
func (s *Service) StartProvisioningRun(ctx context.Context, c Caller, in ProvisioningStartIn) (RunOut, error) {
	const tool = "kodama_provisioning_run_start"
	err := person(c, "starting a provisioning run")
	var sc domain.Scope
	if err == nil {
		sc, err = scope(c, in.ProjectID)
	}
	if err == nil {
		err = checkKey(in.IdempotencyKey)
	}
	if err == nil {
		err = checkID("definitionId", in.DefinitionID)
	}
	if err == nil && in.Repository != "" {
		if verr := (gitv1.CreateRepositoryRequest{Name: in.Repository}).Validate(); verr != nil {
			err = invalid("repository is not a valid repository name")
		}
	}
	if err == nil {
		err = s.checkInputs(in.Inputs)
	}
	if err != nil {
		return RunOut{}, s.fail(ctx, tool, c, err)
	}
	runID := domain.RunID(runIdentity(c, sc.ProjectID, in.IdempotencyKey))
	req := StartProvisioning{RunID: runID, DefinitionID: domain.DefinitionID(in.DefinitionID), Repository: in.Repository, Inputs: in.Inputs, MaxActive: s.lim.MaxActiveProvisioning}
	hashed := struct {
		Definition, Repository string
		Inputs                 map[string]string
	}{in.DefinitionID, in.Repository, in.Inputs}
	out, replayed, err := createOnce(ctx, s, c, sc.ProjectID, tool, in.IdempotencyKey, hashed,
		func(ctx context.Context, _ string) (RunOut, string, error) {
			r, err := s.prov.Start(ctx, c, sc, req)
			if err == nil && r.ID != runID {
				// A port that admits another run than the derived one breaks replay.
				err = fmt.Errorf("%w: the admitted run is not the requested one", ErrUnavailable)
			}
			return runOut(r), string(r.ID), err
		},
		func(ctx context.Context, id string) (RunOut, error) {
			r, err := s.prov.Get(ctx, c, sc, domain.RunID(id))
			return runOut(r), err
		})
	out.Replayed = replayed
	if err != nil {
		return RunOut{}, s.fail(ctx, tool, c, err)
	}
	return out, nil
}

func runOut(r ProvisioningRun) RunOut {
	return RunOut{ID: string(r.ID), DefinitionID: string(r.DefinitionID), Status: r.Status, Reason: r.Reason}
}

var inputKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

func (s *Service) checkInputs(in map[string]string) error {
	if len(in) > s.lim.MaxProvisionInputs {
		return invalid("at most %d inputs", s.lim.MaxProvisionInputs)
	}
	total := 0
	for k, v := range in {
		if len(k) > s.lim.MaxInputKeyBytes || !inputKeyPattern.MatchString(k) {
			return invalid("an input name is not valid")
		}
		if len(v) > s.lim.MaxInputValueBytes {
			return invalid("an input value exceeds %d bytes", s.lim.MaxInputValueBytes)
		}
		total += len(k) + len(v)
	}
	if total > s.lim.MaxInputBytes {
		return invalid("the inputs exceed %d bytes", s.lim.MaxInputBytes)
	}
	return nil
}

// runIdentity derives the run's stable identity from the caller's idempotency
// identity, so a retry is a replay at the admission too.
func runIdentity(c Caller, project domain.ProjectID, key string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", c.Tenant, principalOf(c), project, key)
	return "run_prov_" + hex.EncodeToString(h.Sum(nil))[:32]
}

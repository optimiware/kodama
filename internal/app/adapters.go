package app

// Composition adapters. Each one is a narrow translation between a consumer-
// owned port of one capability module and the module (or explicit installation
// setting) that satisfies it in the core slice. They hold no business rules and
// carry no allow-all default: an unconfigured seam refuses or reports
// unavailable. Anything a capability owner should provide instead is recorded
// in the handoff as a prerequisite, not patched into the owner here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/observation"
	obspg "github.com/optimiware/kodama/internal/observation/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
)

// ---- pipeline definitions --------------------------------------------------

// pipelineAuthority maps the definition permissions to the fixed role matrix:
// reading and listing definitions needs run:read, registering one run:manage.
type pipelineAuthority struct {
	authority identity.Authority
	reader    identity.AccessReader
}

var _ pipeline.Authority = pipelineAuthority{}

func (a pipelineAuthority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (pipeline.Actor, error) {
	user := identity.UserID(principalID)
	if err := user.Validate(); err != nil {
		return pipeline.Actor{}, pipeline.ErrHidden
	}
	bindings, err := a.reader.CurrentBindings(ctx, scope.TenantID, user)
	if err != nil {
		return pipeline.Actor{}, pipeline.Unavailablef(err, "the identity authority is unavailable")
	}
	for _, b := range bindings {
		if b.Applies(identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}) {
			return pipeline.Actor{ID: principalID, Type: pipeline.ActorUser}, nil
		}
	}
	return pipeline.Actor{}, pipeline.ErrHidden
}

func (a pipelineAuthority) Authorize(ctx context.Context, actor pipeline.Actor, scope domain.Scope, perm pipeline.Permission) error {
	var p identity.Permission
	switch perm {
	case pipeline.PermList, pipeline.PermRead:
		p = identity.PermRunRead
	case pipeline.PermRegister:
		p = identity.PermRunManage
	default:
		return pipeline.ErrDenied // unknown permissions deny
	}
	err := a.authority.Authorize(ctx, identity.Actor{ID: actor.ID, Type: identity.ActorType(actor.Type)},
		identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return pipeline.ErrHidden
	case errors.Is(err, identity.ErrDenied):
		return pipeline.ErrDenied
	}
	return pipeline.Unavailablef(err, "the identity authority is unavailable")
}

// pipelineReferences answers whether a definition may reference a secret: the
// actor needs credential:read, the secret must exist with an active version in
// the project, and only file delivery is permitted (the core installation does
// not permit environment-variable delivery). An environment reference needs the
// delivery module (T-072): without one every environment reference is refused,
// and with one the environment must exist in the project and be readable by the
// registering user.
type pipelineReferences struct {
	authority identity.Authority
	catalog   secrets.SecretCatalog
	// environments is the delivery module's environment read; nil refuses.
	environments environmentReader
}

// environmentReader is the delivery use case that reads one environment under
// the caller's current authority.
type environmentReader interface {
	GetEnvironment(ctx context.Context, c delivery.Caller, scope domain.Scope, id domain.EnvironmentID) (delivery.Environment, error)
}

// lateEnvironments breaks the construction cycle between the pipeline
// definitions' reference policy (built first) and the delivery service (which
// needs the artifact and runtime services built after it). It refuses every
// reference until bound.
type lateEnvironments struct {
	mu  sync.RWMutex
	svc environmentReader
}

func (l *lateEnvironments) Bind(svc environmentReader) {
	l.mu.Lock()
	l.svc = svc
	l.mu.Unlock()
}

func (l *lateEnvironments) GetEnvironment(ctx context.Context, c delivery.Caller, scope domain.Scope, id domain.EnvironmentID) (delivery.Environment, error) {
	l.mu.RLock()
	svc := l.svc
	l.mu.RUnlock()
	if svc == nil {
		return delivery.Environment{}, delivery.Fail(delivery.ErrNotFound, nil, "the delivery service is not bound")
	}
	return svc.GetEnvironment(ctx, c, scope, id)
}

var _ pipeline.ReferencePolicy = pipelineReferences{}

func (r pipelineReferences) CheckSecret(ctx context.Context, scope domain.Scope, actor pipeline.Actor, name string, use pipeline.SecretUse) error {
	// Both declared forms are admitted: a file under /kodama/secrets and an ENV binding for the step's
	// command only. The grant, not the form, decides what may be resolved at run time.
	if use != pipeline.SecretUseFile && use != pipeline.SecretUseEnv {
		return pipeline.ErrReferenceDenied
	}
	err := r.authority.Authorize(ctx, identity.Actor{ID: actor.ID, Type: identity.ActorType(actor.Type)},
		identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, identity.PermCredentialRead)
	switch {
	case err == nil:
	case errors.Is(err, identity.ErrDenied), errors.Is(err, identity.ErrHidden):
		return pipeline.ErrReferenceNotFound
	default:
		return pipeline.Unavailablef(err, "the identity authority is unavailable")
	}
	if _, err := r.catalog.CurrentVersion(ctx, scope, name); err != nil {
		switch {
		case errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrDeleted):
			return pipeline.ErrReferenceNotFound
		case errors.Is(err, secrets.ErrNoActiveVersion):
			return pipeline.ErrReferenceDenied
		}
		return pipeline.Unavailablef(err, "the secret catalog is unavailable")
	}
	return nil
}

func (r pipelineReferences) CheckEnvironment(ctx context.Context, scope domain.Scope, actor pipeline.Actor, id domain.EnvironmentID) error {
	if r.environments == nil || actor.Type != pipeline.ActorUser {
		return pipeline.ErrReferenceNotFound
	}
	_, err := r.environments.GetEnvironment(ctx, delivery.Caller{Actor: delivery.Actor{Type: delivery.ActorUser, ID: actor.ID}, RequestID: "pipeline-reference"}, scope, id)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, delivery.ErrNotFound), errors.Is(err, delivery.ErrInvalid):
		return pipeline.ErrReferenceNotFound
	case errors.Is(err, delivery.ErrDenied):
		return pipeline.ErrReferenceDenied
	}
	return pipeline.Unavailablef(err, "the delivery service is unavailable")
}

// noProfiles resolves no agent profile: used by the compositions that do not
// serve agent steps (the development composition of the M1 slice), where the
// capability set rejects them first.
type noProfiles struct{}

func (noProfiles) LookupProfile(context.Context, domain.Scope, string) (pipeline.ProfileInfo, error) {
	return pipeline.ProfileInfo{}, pipeline.ErrReferenceNotFound
}

// noApprovals approves no model and no tool: until the model gateway and the MCP
// proxy are selected for an installation (they need a provider endpoint and a
// destination policy no configuration carries yet, T-085/T-031), no profile can
// name either, so no agent step can be admitted. It fails closed rather than
// pretending to approve.
type noApprovals struct{}

func (noApprovals) ApproveModel(context.Context, domain.Scope, agents.Model) error {
	return agents.ErrDenied
}

func (noApprovals) ApproveTools(context.Context, domain.Scope, []string) error {
	return agents.ErrDenied
}

// ---- workspaces ------------------------------------------------------------

// workspaceAuthorizer implements workspace.Authorizer over the identity
// authority. Provisioning, synchronization, archive, restore and the other
// management actions need workspace:manage, reading needs source:read. The two
// internal service actors (execution and the workspace reconciler) are trusted
// only for the exact actions their use cases perform; they carry no request
// identity and never reach this adapter from an HTTP route.
type workspaceAuthorizer struct{ authority identity.Authority }

var _ workspace.Authorizer = workspaceAuthorizer{}

var serviceActions = map[string]map[workspace.Action]bool{
	"execution":            {workspace.ActionSync: true, workspace.ActionPin: true, workspace.ActionAdmit: true},
	"workspace-reconciler": {workspace.ActionSync: true, workspace.ActionPin: true, workspace.ActionRemove: true},
}

func (a workspaceAuthorizer) Authorize(ctx context.Context, actor workspace.Actor, action workspace.Action, scope domain.Scope, _ workspace.Resource) error {
	if actor.Type == workspace.ActorService {
		if serviceActions[actor.ID][action] {
			return nil
		}
		return workspace.ErrDenied
	}
	var perm identity.Permission
	switch action {
	case workspace.ActionRead:
		perm = identity.PermSourceRead
	case workspace.ActionProvision, workspace.ActionSync, workspace.ActionArchive, workspace.ActionRestore:
		perm = identity.PermWorkspaceManage
	default:
		return workspace.ErrDenied // pin, remove, fence and writer admission are not human actions
	}
	err := a.authority.Authorize(ctx, identity.Actor{ID: actor.ID, Type: identity.ActorUser},
		identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, perm)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return workspace.ErrNotFound
	case errors.Is(err, identity.ErrDenied):
		return workspace.ErrDenied
	}
	return fmt.Errorf("%w: identity authority: %v", workspace.ErrUnavailable, err)
}

// workspaceItems reports whether a work item may own a workspace.
type workspaceItems struct{ repo work.Repository }

var _ workspace.WorkItems = workspaceItems{}

func (w workspaceItems) Actionable(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error {
	it, err := w.repo.Get(ctx, scope, id)
	switch {
	case err == nil:
	case errors.Is(err, work.ErrNotFound), errors.Is(err, work.ErrHidden):
		return workspace.ErrNotFound
	default:
		return fmt.Errorf("%w: work repository: %v", workspace.ErrUnavailable, err)
	}
	if !it.Kind.Actionable() {
		return workspace.ErrNotActionable
	}
	return nil
}

// projectRepositories resolves a project's repository from its settings object:
//
//	{"repository": {"id": "...", "url": "https://...", "baseRef": "refs/heads/main",
//	                "checkoutSecretRef": "secret://installation/...",
//	                "publisherSecretRef": "secret://installation/..."}}
//
// Both references must be configured installation credentials whose URL prefix
// and tenant list cover the repository, so a project cannot aim a credential at
// a host it was not issued for.
type projectRepositories struct {
	db    storage.TxBeginner
	creds map[string]GitCredential
}

var _ workspace.Repositories = projectRepositories{}

func (p projectRepositories) Repository(ctx context.Context, scope domain.Scope) (workspace.RepositoryConfig, error) {
	var settings []byte
	err := storage.WithTenant(ctx, p.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT settings FROM projects WHERE tenant_id = $1 AND id = $2`, string(scope.TenantID), string(scope.ProjectID)).Scan(&settings)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return workspace.RepositoryConfig{}, workspace.ErrNotFound
	case err != nil:
		return workspace.RepositoryConfig{}, fmt.Errorf("%w: project settings: %v", workspace.ErrUnavailable, err)
	}
	var doc struct {
		Repository *struct {
			ID                 string `json:"id"`
			URL                string `json:"url"`
			BaseRef            string `json:"baseRef"`
			CheckoutSecretRef  string `json:"checkoutSecretRef"`
			PublisherSecretRef string `json:"publisherSecretRef"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(settings, &doc); err != nil || doc.Repository == nil {
		return workspace.RepositoryConfig{}, workspace.ErrNotFound // no repository is configured
	}
	r := doc.Repository
	u, perr := url.Parse(r.URL)
	if perr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || r.ID == "" || !strings.HasPrefix(r.BaseRef, "refs/heads/") {
		return workspace.RepositoryConfig{}, workspace.ErrNotFound
	}
	for _, ref := range []string{r.CheckoutSecretRef, r.PublisherSecretRef} {
		if !p.permits(scope.TenantID, ref, r.URL) {
			return workspace.RepositoryConfig{}, workspace.ErrNotFound
		}
	}
	return workspace.RepositoryConfig{
		ID: workspace.RepositoryID(r.ID), URL: r.URL, BaseRef: r.BaseRef,
		CheckoutSecretRef: r.CheckoutSecretRef, PublisherSecretRef: r.PublisherSecretRef,
	}, nil
}

func (p projectRepositories) permits(tenant domain.TenantID, ref, repoURL string) bool {
	c, ok := p.creds[ref]
	if !ok || !urlWithinPrefix(repoURL, c.URLPrefix) {
		return false
	}
	for _, t := range c.Tenants {
		if t == "*" || t == string(tenant) {
			return true
		}
	}
	return false
}

// urlWithinPrefix reports whether repoURL lies under the https prefix: the same
// scheme and exact authority (host and port), then a path boundary, so
// "https://git.example" never covers "https://git.example.evil/x" and "/repo"
// never covers "/repository". Userinfo, queries, fragments, non-https URLs and
// unparsable input are never within a prefix.
func urlWithinPrefix(repoURL, prefix string) bool {
	r, err1 := url.Parse(repoURL)
	p, err2 := url.Parse(prefix)
	if err1 != nil || err2 != nil {
		return false
	}
	for _, u := range []*url.URL{r, p} {
		if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return false
		}
	}
	if r.Scheme != p.Scheme || !strings.EqualFold(r.Host, p.Host) {
		return false
	}
	rp, pp := cleanURLPath(r.EscapedPath()), strings.TrimRight(p.EscapedPath(), "/")
	if rp == "" && r.EscapedPath() != "" {
		return false
	}
	if strings.Contains(pp, "/../") || strings.HasSuffix(pp, "/..") || strings.Contains(pp, "/./") || strings.HasSuffix(pp, "/.") {
		return false
	}
	return pp == "" || rp == pp || strings.HasPrefix(rp, pp+"/")
}

// cleanURLPath rejects dot segments (they could escape the boundary) and
// returns the path without a trailing slash.
func cleanURLPath(p string) string {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return ""
		}
	}
	return strings.TrimRight(p, "/")
}

// installationCredentials resolves installation-level Git credentials by
// reference for the tenants they were issued to.
type installationCredentials struct{ creds map[string]GitCredential }

var _ gitad.Credentials = installationCredentials{}

func (c installationCredentials) Resolve(_ context.Context, scope domain.Scope, ref string) (gitad.Credential, error) {
	cred, ok := c.creds[ref]
	if ok {
		for _, t := range cred.Tenants {
			if t == "*" || t == string(scope.TenantID) {
				return gitad.Credential{Username: cred.Username, Password: cred.Password}, nil
			}
		}
	}
	return gitad.Credential{}, errors.New("no installation credential for this reference")
}

func indexCredentials(list []GitCredential) map[string]GitCredential {
	m := make(map[string]GitCredential, len(list))
	for _, c := range list {
		m[c.Ref] = c
	}
	return m
}

// attemptNeedles is the secret-needle port of source proposals, backed by the
// execution module's authoritative attempt record. The scan of newly introduced
// objects needs the values injected into the attempt. Values reach an attempt
// only through the runtime secret route, which is not wired yet, so the pinned
// context is the authority on whether any could have been delivered:
//
//   - an attempt whose pinned context declares no secret and no deployment
//     environment was injected nothing, and its needle set is empty;
//   - an attempt that declares either could have been injected values this
//     assembly cannot enumerate, so the answer is unavailable (the proposal is
//     refused and may be repeated once delivery is bound), never an empty set;
//   - an attempt that cannot be established (absent, foreign, unreadable or
//     with a context that does not belong to it) is likewise not an empty set.
type attemptNeedles struct{ runs needleRuns }

// needleRuns is the part of the execution store the provider reads.
type needleRuns interface {
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

var _ workspace.SecretNeedles = attemptNeedles{}

func (n attemptNeedles) Needles(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([][]byte, error) {
	if n.runs == nil {
		return nil, fmt.Errorf("%w: no execution store backs the secret needles", workspace.ErrUnavailable)
	}
	if scope.Validate() != nil || attempt.Validate() != nil {
		return nil, workspace.ErrNotFound
	}
	run, err := n.runs.RunOf(ctx, scope, attempt)
	if err != nil {
		return nil, needleLookupError(err)
	}
	agg, err := n.runs.Get(ctx, scope, run)
	if err != nil {
		return nil, needleLookupError(err)
	}
	var at *execution.Attempt
	if agg != nil {
		at = agg.Attempt(attempt)
	}
	if at == nil || at.Scope != scope || at.RunID != run {
		return nil, workspace.ErrNotFound
	}
	pinned, err := decodePinned(at.Context)
	if err != nil || pinned.Scope != scope || pinned.AttemptID != attempt || pinned.RunID != run {
		return nil, fmt.Errorf("%w: the attempt's pinned context is not readable", workspace.ErrUnavailable)
	}
	if len(pinned.Secrets) > 0 || pinned.HasEnvironment {
		return nil, fmt.Errorf("%w: the attempt declares secrets whose injected values are bound by the secret delivery assembly", workspace.ErrUnavailable)
	}
	return [][]byte{}, nil
}

func needleLookupError(err error) error {
	switch {
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrDenied):
		return workspace.ErrNotFound
	}
	return fmt.Errorf("%w: execution lookup: %v", workspace.ErrUnavailable, err)
}

// ---- artifacts -------------------------------------------------------------

// artifactReadAuthority authorizes public artifact metadata with evidence:read
// at the project; a caller without it cannot learn the artifact exists.
type artifactReadAuthority struct{ authority identity.Authority }

var _ artifacts.ReadAuthority = artifactReadAuthority{}

func (a artifactReadAuthority) AuthorizeRead(ctx context.Context, caller artifacts.Caller, scope domain.Scope, _ artifacts.ReadSubject) error {
	err := a.authority.Authorize(ctx, identity.UserActor(identity.UserID(caller.Subject)),
		identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, identity.PermEvidenceRead)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return artifacts.ErrNotFound
	case errors.Is(err, identity.ErrDenied):
		return artifacts.ErrForbidden
	}
	return err
}

// deniedRegistry verifies no registry content: OCI registrations arrive only
// through the runtime artifact route, which the core slice does not serve.
type deniedRegistry struct{}

func (deniedRegistry) Verify(context.Context, artifacts.OCIRequest) (artifacts.OCIVerification, error) {
	return artifacts.OCIVerification{}, artifacts.ErrRegistryDenied
}

// ---- observation ------------------------------------------------------------

// runStates is the execution module's seam of the observation store: current
// run and step-attempt state read through the snapshot transaction. It reads
// only the persisted state/version columns of the run tables through the
// supplied transaction, with explicit tenant and project predicates.
type runStates struct{}

var _ obspg.RunBinding = runStates{}

func (runStates) RunExists(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(run)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (runStates) ReadResources(ctx context.Context, tx pgx.Tx, q obspg.ResourceQuery) ([]observation.Resource, error) {
	var out []observation.Resource
	for _, f := range q.Families {
		var sql string
		switch f {
		case observation.FamilyRun:
			sql = `SELECT id, version, status, '', '' FROM runs WHERE tenant_id = $1 AND project_id = $2 AND ($3 = '' OR id = $3) ORDER BY id LIMIT $4`
		case observation.FamilyStepAttempt:
			sql = `SELECT id, version, status, run_id, step_id FROM step_attempts WHERE tenant_id = $1 AND project_id = $2 AND ($3 = '' OR run_id = $3) ORDER BY id LIMIT $4`
		default:
			continue
		}
		rows, err := tx.Query(ctx, sql, string(q.Scope.Tenant), string(q.Scope.Project), string(q.Scope.Run), q.Limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			res := observation.Resource{Type: f}
			if err := rows.Scan(&res.ID, &res.Version, &res.State, &res.RunID, &res.StepID); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, res)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

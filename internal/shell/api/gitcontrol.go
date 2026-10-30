package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/store"
)

// WorkerRPC is the control plane's view of one tenant's Git worker (the calls of
// gitv1.WorkerBasePath it needs). Every method is idempotent and a transport
// failure is an unknown outcome the caller repeats.
type WorkerRPC interface {
	Initialize(ctx context.Context, tenant domain.TenantID, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error)
	ReadRefs(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefsRequest) (gitv1.RefsResponse, error)
	IsAncestor(ctx context.Context, tenant domain.TenantID, req gitv1.IsAncestorRequest) (gitv1.IsAncestorResponse, error)
}

// receiveWindow is how long a receive may run once authorized; the worker kills
// an overrunning receive at the deadline.
const receiveWindow = 10 * time.Minute

// GitControl is the worker-facing side of the control plane for every tenant:
// pre-receive authorization, commit intent, the post-receive outcome, the
// initialization confirmation and the assignments. It holds no state of its own
// beyond persistence: a receive ID carries its grant (gitv1.NewReceiveID), the
// credential row carries the repository and the actor, and an outcome is keyed
// by (repository, receive). Methods take the tenant explicitly, so one value
// serves the HTTP routes of any tenant and the in-process worker of each.
type GitControl struct {
	store   store.GitBackend
	workers WorkerRPC
	now     func() time.Time
	log     *slog.Logger

	// Assignment selection is paged and rotated so that no repository starves:
	// every provisioning repository first, then a window of reconcilable ones
	// that moves on with each poll and wraps around.
	mu        sync.Mutex
	pageSize  int
	nextAfter map[domain.TenantID]string
	onEvent   func()
}

// SetOnEvent sets the hook called after an outcome committed an event.
func (c *GitControl) SetOnEvent(f func()) {
	c.mu.Lock()
	c.onEvent = f
	c.mu.Unlock()
}

// DefaultAssignmentPage is how many provisioning and how many reconcilable
// repositories one assignment poll returns (the contract bounds a poll at
// gitv1.MaxAssignments).
const DefaultAssignmentPage = gitv1.MaxAssignments / 2

// SetAssignmentPage changes the page size (tests, small installations).
func (c *GitControl) SetAssignmentPage(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 1 || n > gitv1.MaxAssignments/2 {
		n = DefaultAssignmentPage
	}
	c.pageSize = n
}

// NewGitControl builds the control plane over a backend and the worker RPC.
func NewGitControl(backend store.GitBackend, workers WorkerRPC, now func() time.Time, log *slog.Logger) *GitControl {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &GitControl{store: backend, workers: workers, now: now, log: log, pageSize: DefaultAssignmentPage, nextAfter: map[domain.TenantID]string{}}
}

func ce(code domain.ErrorCode, format string, args ...any) error {
	return &gitv1.ContractError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// storeErr maps a persistence failure to the contract error a worker handles.
func storeErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return ce(domain.CodeNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		return ce(domain.CodeIdempotencyConflict, "conflict")
	case errors.Is(err, domain.ErrInvalidValue):
		return ce(domain.CodeInvalidRequest, "%v", err)
	}
	var contract *gitv1.ContractError
	if errors.As(err, &contract) {
		return err
	}
	return ce(domain.CodeUnavailable, "the control plane could not complete the request")
}

// receiveCredential resolves the credential a receive was made under: the grant
// the receive ID carries, which must exist in the tenant. The repository comes
// from the credential, never from the worker's claim.
func (c *GitControl) receiveCredential(ctx context.Context, tenant domain.TenantID, receiveID string) (store.Credential, error) {
	grant, ok := gitv1.GrantOfReceive(receiveID)
	if !ok {
		return store.Credential{}, ce(domain.CodeInvalidRequest, "receiveId does not carry a grant")
	}
	cred, err := c.store.GetCredential(ctx, tenant, grant)
	if err != nil {
		return store.Credential{}, storeErr(err)
	}
	return cred, nil
}

// facts reads the current authority behind a credential: the repository, the
// claims, the person's roles and the run's state.
func (c *GitControl) facts(ctx context.Context, tenant domain.TenantID, cred store.Credential) (pushFacts, error) {
	repo, err := c.store.GetRepository(ctx, tenant, cred.RepoID)
	if err != nil {
		return pushFacts{}, storeErr(err)
	}
	f := pushFacts{Repo: repo, Cred: cred}
	if err := jsonUnmarshalClaims(cred.Claims, &f.Claims); err != nil {
		return pushFacts{}, ce(domain.CodeInvalidCredential, "the credential record is unreadable")
	}
	switch cred.Kind {
	case store.CredHuman, store.CredSeed:
		roles, err := c.store.Roles(ctx, tenant, cred.UserID, string(repo.ProjectID))
		if err != nil {
			return pushFacts{}, storeErr(err)
		}
		f.Engineer = holdsRole(roles, "engineer")
	case store.CredRun, store.CredIntegration:
		run, err := c.store.RunGitInfo(ctx, tenant, cred.ProjectID, cred.RunID)
		switch {
		case err == nil:
			f.Run = &run
		case !errors.Is(err, store.ErrNotFound):
			return pushFacts{}, storeErr(err)
		}
		if cred.Kind == store.CredIntegration && f.Run != nil && f.Run.TaskID != "" {
			// Integration needs the run's task approved now; a task that cannot be
			// read is an error that fails the receive, never an approval.
			approved, err := c.store.TaskApproved(ctx, tenant, cred.ProjectID, f.Run.TaskID)
			if err != nil {
				return pushFacts{}, storeErr(err)
			}
			f.TaskApproved = approved
		}
	}
	return f, nil
}

// Authorize is pre-receive's call. Authority is read now, every update is
// decided by the ref policy, a seed is consumed only when its receive is
// otherwise allowed, and any failure here fails the receive: nothing changes.
func (c *GitControl) Authorize(ctx context.Context, tenant domain.TenantID, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.PushAuthorizeResponse{}, ce(domain.CodeInvalidRequest, "%v", err)
	}
	cred, err := c.receiveCredential(ctx, tenant, req.ReceiveID)
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	if cred.ID != req.GrantID || cred.RepoID != req.RepositoryID {
		return gitv1.PushAuthorizeResponse{}, ce(domain.CodeForbidden, "the receive does not belong to this credential")
	}
	now := c.now()
	if !cred.Live(now) {
		return gitv1.PushAuthorizeResponse{}, ce(domain.CodeInvalidCredential, "the credential is expired or revoked")
	}
	f, err := c.facts(ctx, tenant, cred)
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	verdicts := decidePush(f, req)
	allowed := true
	for _, v := range verdicts {
		allowed = allowed && v.Verdict == gitv1.VerdictAllow
	}
	if allowed && cred.Kind == store.CredSeed {
		switch err := c.store.AuthorizeSeed(ctx, tenant, cred.ID, req.ReceiveID); {
		case err == nil:
		case errors.Is(err, store.ErrSeedConsumed):
			reason := gitv1.ReasonSeedConsumed
			for i := range verdicts {
				verdicts[i] = gitv1.UpdateVerdict{Ref: verdicts[i].Ref, Verdict: gitv1.VerdictDeny, Reason: &reason}
			}
		default:
			return gitv1.PushAuthorizeResponse{}, storeErr(err)
		}
	}
	return gitv1.PushAuthorizeResponse{
		PushID:   req.ReceiveID,
		Deadline: gitv1.Timestamp(now.Add(receiveWindow).UTC().Format(time.RFC3339Nano)),
		Updates:  verdicts,
	}, nil
}

// CommitIntent answers committing: true while the credential is not revoked and,
// for a run, the run is live and the repository is not archived; otherwise a
// contract error and Git aborts. The receive deadline is enforced by the worker,
// which kills an overrunning receive and refuses commit intent after it.
func (c *GitControl) CommitIntent(ctx context.Context, tenant domain.TenantID, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error) {
	if err := req.Validate(); err != nil || pushID != req.ReceiveID {
		return gitv1.CommitIntentResponse{}, ce(domain.CodeInvalidRequest, "the commit intent does not match its receive")
	}
	cred, err := c.receiveCredential(ctx, tenant, req.ReceiveID)
	if err != nil {
		return gitv1.CommitIntentResponse{}, err
	}
	f, err := c.facts(ctx, tenant, cred)
	if err != nil {
		return gitv1.CommitIntentResponse{}, err
	}
	switch {
	case cred.RevokedAt != nil:
		return gitv1.CommitIntentResponse{}, ce(gitv1.CodeRunInactive, "the credential was revoked")
	case f.Repo.Status == store.RepoArchived:
		return gitv1.CommitIntentResponse{}, ce(gitv1.CodeRepositoryArchived, "the repository is archived")
	case (cred.Kind == store.CredRun || cred.Kind == store.CredIntegration) && (f.Run == nil || !f.Run.Active()):
		return gitv1.CommitIntentResponse{}, ce(gitv1.CodeRunInactive, "the run is no longer live")
	}
	return gitv1.CommitIntentResponse{Committing: true}, nil
}

// Outcome records a receive's applied updates (possibly none): one
// git.refs_updated row under its deterministic ID and the acknowledged journal
// sequence, in one transaction, so a retried or re-reported outcome emits exactly
// one event. The kind of an update of an existing ref asks the worker whether it
// was a fast-forward, so a failure to ask is unavailable and the report repeats.
func (c *GitControl) Outcome(ctx context.Context, tenant domain.TenantID, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error) {
	if err := req.Validate(); err != nil || pushID != req.ReceiveID {
		return gitv1.OutcomeResponse{}, ce(domain.CodeInvalidRequest, "the outcome does not match its receive")
	}
	cred, err := c.receiveCredential(ctx, tenant, req.ReceiveID)
	if err != nil {
		return gitv1.OutcomeResponse{}, err
	}
	commit := store.ReceiveCommit{TenantID: tenant, RepositoryID: cred.RepoID, ReceiveID: req.ReceiveID}
	if req.JournalSequence != nil {
		commit.JournalSequence = *req.JournalSequence
	}
	if req.JournalPosition != nil {
		commit.JournalPosition = *req.JournalPosition
	}
	switch cred.Kind {
	case store.CredRun, store.CredIntegration:
		commit.ActorType, commit.ActorID, commit.OriginRunID = "workload", cred.RunID, cred.RunID
	default:
		commit.ActorType, commit.ActorID = "user", cred.UserID
	}
	for _, u := range req.Applied {
		kind, err := c.kindOf(ctx, tenant, cred.RepoID, u)
		if err != nil {
			return gitv1.OutcomeResponse{}, err
		}
		commit.Updates = append(commit.Updates, store.ReceiveUpdate{Ref: u.Ref, Old: u.Old, New: u.New, Kind: string(kind)})
	}
	res, err := c.store.CommitReceive(ctx, commit)
	if err != nil {
		return gitv1.OutcomeResponse{}, storeErr(err)
	}
	if res.EventID != "" {
		c.mu.Lock()
		notify := c.onEvent
		c.mu.Unlock()
		if notify != nil {
			notify()
		}
	}
	status := gitv1.PushRefused
	if len(commit.Updates) > 0 {
		status = gitv1.PushApplied
	}
	return gitv1.OutcomeResponse{PushID: req.ReceiveID, Status: status}, nil
}

func (c *GitControl) kindOf(ctx context.Context, tenant domain.TenantID, repositoryID string, u gitv1.RefUpdate) (gitv1.UpdateKind, error) {
	if gitv1.IsZeroObjectID(u.Old) || gitv1.IsZeroObjectID(u.New) {
		return gitv1.KindOf(u.Old, u.New, nil)
	}
	res, err := c.workers.IsAncestor(ctx, tenant, gitv1.IsAncestorRequest{RepositoryID: repositoryID, Ancestor: u.Old, Descendant: u.New})
	if err != nil {
		var contract *gitv1.ContractError
		if errors.As(err, &contract) && contract.Code == gitv1.CodeCommitNotFound {
			no := false // the old commit is gone: whatever replaced it was not a fast-forward
			return gitv1.KindOf(u.Old, u.New, &no)
		}
		return "", storeErr(err)
	}
	return gitv1.KindOf(u.Old, u.New, &res.IsAncestor)
}

// Initialized confirms an initialization: provisioning becomes ready.
func (c *GitControl) Initialized(ctx context.Context, tenant domain.TenantID, req gitv1.InitializedRequest) (gitv1.InitializedResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.InitializedResponse{}, ce(domain.CodeInvalidRequest, "%v", err)
	}
	repo, err := c.store.ConfirmInitialized(ctx, tenant, req.RepositoryID, req.DefaultBranch, req.Empty)
	if err != nil {
		return gitv1.InitializedResponse{}, storeErr(err)
	}
	return gitv1.InitializedResponse{RepositoryID: repo.ID, Status: gitv1.RepositoryStatus(repo.Status), Version: int64(repo.Version)}, nil
}

// Assignments lists what the tenant's worker acts on: every provisioning
// repository (to initialize, including one reconfigured for a changed default
// branch) and a rotating window of ready and archived ones to reconcile with
// their acknowledged sequence. An archived repository refuses pushes but its
// journal is still recovered. The window moves on with each poll and wraps, so a
// tenant with more repositories than a page is reconciled in turns.
func (c *GitControl) Assignments(ctx context.Context, tenant domain.TenantID) (gitv1.AssignmentsResponse, error) {
	c.mu.Lock()
	page, after := c.pageSize, c.nextAfter[tenant]
	c.mu.Unlock()
	out := gitv1.AssignmentsResponse{Items: []gitv1.Assignment{}}
	provisioning, err := c.store.ProvisioningRepositories(ctx, tenant, page)
	if err != nil {
		return gitv1.AssignmentsResponse{}, storeErr(err)
	}
	for _, r := range provisioning {
		out.Items = append(out.Items, gitv1.Assignment{Kind: gitv1.AssignInitialize, RepositoryID: r.ID, DefaultBranch: r.DefaultBranch})
	}
	seen := map[string]bool{}
	var last string
	add := func(rs []store.Repository) {
		for _, r := range rs {
			if seen[r.ID] || len(out.Items) >= gitv1.MaxAssignments {
				continue
			}
			seen[r.ID] = true
			ack := r.JournalAck
			out.Items = append(out.Items, gitv1.Assignment{Kind: gitv1.AssignReconcile, RepositoryID: r.ID, DefaultBranch: r.DefaultBranch, JournalAck: &ack})
			last = r.ID
		}
	}
	rs, err := c.store.ReconcilableRepositories(ctx, tenant, after, page)
	if err != nil {
		return gitv1.AssignmentsResponse{}, storeErr(err)
	}
	add(rs)
	if len(rs) < page { // the end of the order: wrap around to the beginning
		more, err := c.store.ReconcilableRepositories(ctx, tenant, "", page-len(rs))
		if err != nil {
			return gitv1.AssignmentsResponse{}, storeErr(err)
		}
		add(more)
		if len(more) < page-len(rs) {
			last = "" // everything was listed: start over
		}
	}
	c.mu.Lock()
	c.nextAfter[tenant] = last
	c.mu.Unlock()
	return out, nil
}

// ForTenant is the gitservice.ControlPlane of one tenant's in-process worker: the
// same methods the HTTP routes expose, called directly (the worker's identity is
// the process itself).
func (c *GitControl) ForTenant(tenant domain.TenantID) *TenantControl {
	return &TenantControl{c: c, tenant: tenant}
}

// TenantControl binds GitControl to one tenant; it implements
// gitservice.ControlPlane.
type TenantControl struct {
	c      *GitControl
	tenant domain.TenantID
}

func (t *TenantControl) Authorize(ctx context.Context, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error) {
	return t.c.Authorize(ctx, t.tenant, req)
}

func (t *TenantControl) CommitIntent(ctx context.Context, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error) {
	return t.c.CommitIntent(ctx, t.tenant, pushID, req)
}

func (t *TenantControl) Outcome(ctx context.Context, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error) {
	return t.c.Outcome(ctx, t.tenant, pushID, req)
}

func (t *TenantControl) Initialized(ctx context.Context, req gitv1.InitializedRequest) (gitv1.InitializedResponse, error) {
	return t.c.Initialized(ctx, t.tenant, req)
}

func (t *TenantControl) Assignments(ctx context.Context) (gitv1.AssignmentsResponse, error) {
	return t.c.Assignments(ctx, t.tenant)
}

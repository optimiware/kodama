package workspace

import (
	"context"
	"errors"
	"fmt"
	"slices"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func (s *Service) needHosting() error {
	if s.d.Hosts == nil || s.d.InternalRepos == nil {
		return fmt.Errorf("%w: internal repository hosting", ErrNotConfigured)
	}
	return nil
}

// internalRepository resolves a repository of the project and requires it to be
// internally hosted (an external one is never given a task branch here).
func (s *Service) internalRepository(ctx context.Context, scope domain.Scope, id RepositoryID) (InternalRepository, error) {
	repo, err := s.d.InternalRepos.InternalRepository(ctx, scope, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return InternalRepository{}, ErrNotFound
	case err != nil:
		return InternalRepository{}, unavailable("repository", err)
	case repo.ID != id:
		return InternalRepository{}, ErrNotFound
	case repo.Hosting != gitv1.HostingInternal:
		return InternalRepository{}, fmt.Errorf("%w: repository is not internally hosted", ErrWrongHosting)
	}
	return repo, nil
}

// readRef reads one exact ref of a repository from its worker. The commit is
// empty when the ref is absent.
func (s *Service) readRef(ctx context.Context, scope domain.Scope, repo RepositoryID, ref string) (Commit, bool, error) {
	rctx, cancel := s.gitCtx(ctx)
	defer cancel()
	resp, err := s.d.Hosts.ReadRefValues(rctx, scope.TenantID, gitv1.ReadRefValuesRequest{RepositoryID: string(repo), Refs: []string{ref}})
	if err != nil {
		return "", false, unavailable("ref values", err)
	}
	if len(resp.Values) != 1 || resp.Values[0].Ref != ref {
		return "", false, unavailable("ref values", errors.New("the worker answered for another ref"))
	}
	v := resp.Values[0].Commit
	if v == nil {
		return "", false, nil
	}
	c := Commit(*v)
	if err := c.Validate(); err != nil {
		return "", false, unavailable("ref values", err)
	}
	return c, true, nil
}

// defaultTip is the commit a new task branch starts from: the default branch's
// tip, read once and then recorded before the branch is created.
func (s *Service) defaultTip(ctx context.Context, scope domain.Scope, repo InternalRepository) (Commit, error) {
	if repo.Status != gitv1.StatusReady {
		return "", fmt.Errorf("%w: repository is %s", ErrRepositoryNotReady, repo.Status)
	}
	tip, ok, err := s.readRef(ctx, scope, repo.ID, "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrRepositoryNotReady, ReasonRepositoryEmpty)
	}
	return tip, nil
}

func validateRepositories(repos []RepositoryID) error {
	if len(repos) == 0 || len(repos) > MaxRepositories {
		return fmt.Errorf("%w: between 1 and %d repositories", ErrInvalidValue, MaxRepositories)
	}
	seen := map[RepositoryID]bool{}
	for _, r := range repos {
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r] {
			return fmt.Errorf("%w: repository %s is named twice", ErrInvalidValue, r)
		}
		seen[r] = true
	}
	return nil
}

// ProvisionInternal returns the one internally hosted workspace of an
// actionable work item with a task branch in each named repository, creating
// whatever is missing (WS-01, WS-17, WS-A13). It is the internal counterpart of
// Provision and has the same shape: the workspace and the intent of each task
// branch (with the base commit it will start at) are committed before any branch
// is created; each branch is then created through the repository's worker with
// an operation identifier that is stable per workspace and repository, outside
// SQL, and an unknown outcome is reconciled by observing the ref. Repeating the
// call, concurrently or after a crash, creates nothing more. The first
// repository names the workspace's primary repository when it is created.
// created reports whether this call reserved the workspace; a branch that could
// not be created is a safe reason on its association and the workspace stays
// provisioning (or available, for a later addition) for the next call or the
// reconciler.
func (s *Service) ProvisionInternal(ctx context.Context, actor Actor, scope domain.Scope, item domain.WorkItemID, repos []RepositoryID) (*Workspace, []*RepositoryAssociation, bool, error) {
	if err := item.Validate(); err != nil {
		return nil, nil, false, ErrNotFound
	}
	if err := validateRepositories(repos); err != nil {
		return nil, nil, false, err
	}
	if err := s.authorize(ctx, actor, ActionProvision, scope, Resource{"work_item", string(item)}); err != nil {
		return nil, nil, false, err
	}
	if err := s.needHosting(); err != nil {
		return nil, nil, false, err
	}
	switch err := s.d.WorkItems.Actionable(ctx, scope, item); {
	case err == nil:
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotActionable):
		return nil, nil, false, err
	default:
		return nil, nil, false, unavailable("work item", err)
	}

	facts := make(map[RepositoryID]InternalRepository, len(repos))
	for _, id := range repos {
		repo, err := s.internalRepository(ctx, scope, id)
		if err != nil {
			return nil, nil, false, err
		}
		facts[id] = repo
	}

	created := false
	w, err := s.d.Store.GetByWorkItem(ctx, scope, item)
	switch {
	case errors.Is(err, ErrNotFound):
		if w, created, err = s.reserveInternal(ctx, actor, scope, item, repos, facts); err != nil {
			return nil, nil, false, err
		}
		if !created {
			// A concurrent request inserted the workspace first: this request's
			// own repositories are not in it unless the winner named them too.
			if w, err = s.addRepositories(ctx, actor, w, repos, facts); err != nil {
				return nil, nil, false, err
			}
		}
	case err != nil:
		return nil, nil, false, err
	default:
		if err := w.requireInternal("internal provisioning"); err != nil {
			return nil, nil, false, err
		}
		if w, err = s.addRepositories(ctx, actor, w, repos, facts); err != nil {
			return nil, nil, false, err
		}
	}
	w, assocs, err := s.driveInternal(ctx, actor, scope, w.ID)
	if err != nil {
		return nil, nil, created, err
	}
	return w, assocs, created, nil
}

// reserveInternal commits the workspace and the intents of all named
// repositories. The base commits are read first; when two callers race, the
// first insert wins and the other receives the stored workspace, whose recorded
// bases are the ones used.
func (s *Service) reserveInternal(ctx context.Context, actor Actor, scope domain.Scope, item domain.WorkItemID, repos []RepositoryID, facts map[RepositoryID]InternalRepository) (*Workspace, bool, error) {
	bases := make(map[RepositoryID]Commit, len(repos))
	for _, id := range repos {
		tip, err := s.defaultTip(ctx, scope, facts[id])
		if err != nil {
			return nil, false, err
		}
		bases[id] = tip
	}
	primary := facts[repos[0]]
	nw, err := NewInternal(NewInternalParams{ID: ID(s.d.NewID("ws")), Scope: scope, WorkItemID: item, Primary: primary.ID,
		URL: primary.URL, CheckoutSecretRef: primary.CheckoutSecretRef}, s.now())
	if err != nil {
		return nil, false, err
	}
	assocs := make([]*RepositoryAssociation, 0, len(repos))
	names := make([]string, 0, len(repos))
	for _, id := range repos {
		a, err := NewAssociation(nw, id, bases[id], s.now())
		if err != nil {
			return nil, false, err
		}
		assocs = append(assocs, a)
		names = append(names, string(id))
	}
	w, inserted, err := s.d.Store.Insert(ctx, nw, Effects{
		Associations: assocs,
		Audit: []Audit{audit(actor, "workspace.provision_requested", "", map[string]any{
			"workItemId": string(item), "hosting": string(HostingInternal), "repositories": names, "branch": nw.WorkBranch})},
		Events: []Event{stateEvent(actor, "none", string(StateProvisioning), "provision_requested")},
	})
	if err != nil {
		return nil, false, err
	}
	if err := w.requireInternal("internal provisioning"); err != nil {
		return nil, false, err // a racing external provision won
	}
	return w, inserted, nil
}

// addRepositories records the intent for repositories the workspace does not
// associate yet (a work item that touches one more repository later).
func (s *Service) addRepositories(ctx context.Context, actor Actor, w *Workspace, repos []RepositoryID, facts map[RepositoryID]InternalRepository) (*Workspace, error) {
	agg, err := s.d.Store.View(ctx, w.Scope, w.ID, Load{Associations: true})
	if err != nil {
		return nil, err
	}
	if agg.Workspace.State == StateArchived {
		return agg.Workspace, nil
	}
	var missing []RepositoryID
	for _, id := range repos {
		if findAssociation(agg.Associations, id) == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return agg.Workspace, nil
	}
	bases := make(map[RepositoryID]Commit, len(missing))
	for _, id := range missing {
		tip, err := s.defaultTip(ctx, w.Scope, facts[id])
		if err != nil {
			return nil, err
		}
		bases[id] = tip
	}
	out, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{Associations: true}, func(a *Aggregate) (Effects, error) {
		var fx Effects
		for _, id := range missing {
			na, isNew, err := a.Workspace.AddAssociation(a.Associations, id, bases[id], s.now())
			if err != nil {
				return Effects{}, err
			}
			if !isNew {
				continue // a concurrent call recorded it first; its base wins
			}
			a.Associations = append(a.Associations, na)
			fx.Associations = append(fx.Associations, na)
			fx.Audit = append(fx.Audit, audit(actor, "workspace.repository_associated", "", map[string]any{
				"repositoryId": string(id), "branch": na.TaskBranch, "baseCommit": string(na.BaseCommit)}))
		}
		return fx, nil
	})
	if err != nil {
		return nil, err
	}
	return out.Workspace, nil
}

// driveInternal creates the task branch of every association still in intent
// and, once the workspace's associations are all created, makes a provisioning
// workspace available. Remote failures become safe reasons; only store failures
// are errors. It is what ProvisionInternal and the reconciler share.
func (s *Service) driveInternal(ctx context.Context, actor Actor, scope domain.Scope, id ID) (*Workspace, []*RepositoryAssociation, error) {
	agg, err := s.d.Store.View(ctx, scope, id, Load{Associations: true})
	if err != nil {
		return nil, nil, err
	}
	w := agg.Workspace
	if !w.Internal() {
		return nil, nil, fmt.Errorf("%w: internal provisioning", ErrWrongHosting)
	}
	if w.State != StateArchived {
		for _, a := range agg.Associations {
			if a.State != AssociationIntent {
				continue
			}
			if err := s.createTaskBranch(ctx, actor, w, a); err != nil {
				return nil, nil, err
			}
		}
	}
	return s.completeInternal(ctx, actor, scope, id)
}

// completeInternal makes the provisioning workspace available if every
// association is created; otherwise it records the first association's reason.
func (s *Service) completeInternal(ctx context.Context, actor Actor, scope domain.Scope, id ID) (*Workspace, []*RepositoryAssociation, error) {
	agg, err := s.d.Store.Update(ctx, scope, id, Load{Associations: true}, func(a *Aggregate) (Effects, error) {
		w := a.Workspace
		if w.State != StateProvisioning {
			return Effects{}, nil
		}
		reason := ""
		for _, as := range a.Associations {
			if as.State != AssociationCreated {
				reason = cmpReason(as.LastError)
				break
			}
		}
		if reason != "" {
			changed, err := w.RecordProvisioningError(reason, s.now())
			if err != nil || !changed {
				return Effects{}, err
			}
			return Effects{Audit: []Audit{audit(actor, "workspace.provision_failed", reason, nil)}}, nil
		}
		if err := w.CompleteInternalProvisioning(a.Associations, s.now()); err != nil {
			return Effects{}, err
		}
		return Effects{
			Audit:  []Audit{audit(actor, "workspace.provisioned", "", map[string]any{"commit": string(w.AcceptedCommit), "branch": w.WorkBranch, "hosting": string(HostingInternal)})},
			Events: []Event{stateEvent(actor, string(StateProvisioning), string(StateAvailable), "branch_ready")},
		}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return agg.Workspace, agg.Associations, nil
}

func cmpReason(r string) string {
	if r == "" {
		return ReasonProvisionFailed
	}
	return r
}

type branchObservation int

const (
	branchAbsent      branchObservation = iota // no ref
	branchCreated                              // at the base commit, or our own creation that was moved on
	branchIndependent                          // elsewhere, with no creation of ours in the journal
)

// createTaskBranch creates one task branch idempotently through the worker. A
// failure of any kind is reconciled by observing the ref: an unknown outcome is
// never assumed to have failed or succeeded.
func (s *Service) createTaskBranch(ctx context.Context, actor Actor, w *Workspace, a *RepositoryAssociation) error {
	repo, err := s.internalRepository(ctx, w.Scope, a.RepositoryID)
	if err != nil || repo.Status != gitv1.StatusReady {
		return s.recordAssociationError(ctx, actor, w, a.RepositoryID, ReasonRepositoryUnready)
	}
	rctx, cancel := s.gitCtx(ctx)
	resp, cerr := s.d.Hosts.CreateBranch(rctx, w.Scope.TenantID, gitv1.CreateBranchRequest{
		RepositoryID: string(a.RepositoryID), OperationID: BranchOperationID(w, a.RepositoryID), Ref: a.TaskBranch, Commit: string(a.BaseCommit)})
	cancel()
	if cerr == nil && resp.Ref == a.TaskBranch && resp.Commit == string(a.BaseCommit) {
		return s.markTaskBranchCreated(ctx, actor, w, a.RepositoryID, resp.Created)
	}
	obs, oerr := s.observeTaskBranch(ctx, w, a)
	switch {
	case oerr != nil || obs == branchAbsent:
		return s.recordAssociationError(ctx, actor, w, a.RepositoryID, ReasonProvisionFailed)
	case obs == branchIndependent:
		return s.recordAssociationError(ctx, actor, w, a.RepositoryID, ReasonBranchExists)
	}
	return s.markTaskBranchCreated(ctx, actor, w, a.RepositoryID, false)
}

// observeTaskBranch reads the task branch and, when it is not at the recorded
// base, the ref's journal: a committed entry of this association's operation
// shows the branch is ours and later work moved it, anything else shows an
// independent branch, which is never overwritten or adopted.
func (s *Service) observeTaskBranch(ctx context.Context, w *Workspace, a *RepositoryAssociation) (branchObservation, error) {
	tip, ok, err := s.readRef(ctx, w.Scope, a.RepositoryID, a.TaskBranch)
	switch {
	case err != nil:
		return branchAbsent, err
	case !ok:
		return branchAbsent, nil
	case tip == a.BaseCommit:
		return branchCreated, nil
	}
	op := BranchOperationID(w, a.RepositoryID)
	var since int64
	for range 64 {
		rctx, cancel := s.gitCtx(ctx)
		resp, err := s.d.Hosts.ReadJournal(rctx, w.Scope.TenantID, gitv1.ReadJournalRequest{
			RepositoryID: string(a.RepositoryID), Ref: &a.TaskBranch, SinceSequence: since, Limit: 256})
		cancel()
		if err != nil {
			return branchAbsent, unavailable("ref journal", err)
		}
		for _, e := range resp.Entries {
			if e.OperationID != nil && *e.OperationID == op && e.State == gitv1.JournalCommitted {
				return branchCreated, nil
			}
			since = max(since, e.Sequence)
		}
		if !resp.HasMore {
			return branchIndependent, nil
		}
	}
	return branchAbsent, unavailable("ref journal", errors.New("too many journal pages"))
}

func (s *Service) markTaskBranchCreated(ctx context.Context, actor Actor, w *Workspace, repo RepositoryID, created bool) error {
	_, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{Associations: true}, func(a *Aggregate) (Effects, error) {
		as := findAssociation(a.Associations, repo)
		if as == nil {
			return Effects{}, ErrNotFound
		}
		changed, err := as.MarkCreated(s.now())
		if err != nil || !changed {
			return Effects{}, err
		}
		a.Workspace.TouchAssociations(s.now())
		return Effects{Associations: []*RepositoryAssociation{as}, Audit: []Audit{audit(actor, "workspace.task_branch_created", "", map[string]any{
			"repositoryId": string(repo), "branch": as.TaskBranch, "commit": string(as.BaseCommit), "createdByThisCall": created})}}, nil
	})
	return err
}

func (s *Service) recordAssociationError(ctx context.Context, actor Actor, w *Workspace, repo RepositoryID, reason string) error {
	_, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{Associations: true}, func(a *Aggregate) (Effects, error) {
		as := findAssociation(a.Associations, repo)
		if as == nil || as.State != AssociationIntent {
			return Effects{}, nil
		}
		changed, err := as.RecordError(reason, s.now())
		if err != nil || !changed {
			return Effects{}, err
		}
		a.Workspace.TouchAssociations(s.now())
		return Effects{Associations: []*RepositoryAssociation{as}, Audit: []Audit{audit(actor, "workspace.task_branch_failed", reason, map[string]any{"repositoryId": string(repo)})}}, nil
	})
	return err
}

// Repositories returns the repository associations of a workspace after an
// authorization check; an externally hosted workspace has none.
func (s *Service) Repositories(ctx context.Context, actor Actor, scope domain.Scope, id ID) ([]*RepositoryAssociation, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionRead, scope, Resource{"workspace", string(id)}); err != nil {
		return nil, err
	}
	agg, err := s.d.Store.View(ctx, scope, id, Load{Associations: true})
	if err != nil {
		return nil, err
	}
	return sortedAssociations(agg.Associations), nil
}

func sortedAssociations(in []*RepositoryAssociation) []*RepositoryAssociation {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b *RepositoryAssociation) int {
		switch {
		case a.RepositoryID < b.RepositoryID:
			return -1
		case a.RepositoryID > b.RepositoryID:
			return 1
		}
		return 0
	})
	return out
}

// RepositoryRecovery is what recovery reports per repository of an internally
// hosted workspace: the last accepted commit of its task branch, and that
// uncommitted work may be lost (WS-08).
type RepositoryRecovery struct {
	RepositoryID             RepositoryID
	TaskBranch               string
	LastAcceptedCommit       Commit
	UncommittedWorkMayBeLost bool
}

// InternalRecovery returns the commit recovery starts from in each created
// repository of an internally hosted workspace.
func (s *Service) InternalRecovery(ctx context.Context, actor Actor, scope domain.Scope, id ID) ([]RepositoryRecovery, error) {
	assocs, err := s.Repositories(ctx, actor, scope, id)
	if err != nil {
		return nil, err
	}
	var out []RepositoryRecovery
	for _, a := range assocs {
		if a.State == AssociationCreated {
			out = append(out, RepositoryRecovery{RepositoryID: a.RepositoryID, TaskBranch: a.TaskBranch, LastAcceptedCommit: a.AcceptedCommit, UncommittedWorkMayBeLost: true})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: workspace has no accepted commit", ErrInvalidTransition)
	}
	return out, nil
}

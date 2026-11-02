package workspace

import (
	"context"
	"errors"
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// maxPinAttempts bounds how many retained-ref numbers one checkpoint tries when
// the host reports them taken by other commits of the run.
const maxPinAttempts = 8

// CheckpointInput names one commit output of a writer: the commit the attempt
// pushed to the task branch of a repository and the push record that put it
// there (the runtime context's commits entry).
type CheckpointInput struct {
	Scope        domain.Scope
	AttemptID    domain.AttemptID
	RepositoryID RepositoryID
	Ref          string
	Commit       Commit
	PushID       string
}

// CheckpointResult is an accepted checkpoint.
type CheckpointResult struct {
	WorkspaceID    ID
	RepositoryID   RepositoryID
	Ref            string
	Commit         Commit
	PreviousCommit Commit
	// Advanced is false when the commit was already contained in the accepted
	// history; the accepted commit never moves backwards.
	Advanced bool
	// PinRef is the retained ref refs/kodama/runs/<run>/<n> on the internal
	// host, confirmed before a result is returned.
	PinRef string
	// Replay is true when the checkpoint was accepted by an earlier call.
	Replay bool
}

func (c *InternalCheckpoint) result(replay bool) CheckpointResult {
	return CheckpointResult{WorkspaceID: c.WorkspaceID, RepositoryID: c.RepositoryID, Ref: c.Ref, Commit: c.Commit,
		PreviousCommit: c.PreviousCommit, Advanced: c.Advanced, PinRef: c.PinRef, Replay: replay}
}

// AcceptCheckpoint accepts a writer's commit output from the push record that
// put it on its task branch (WS-19, GR-09). The commit is accepted only if,
// all checked against records and never against the caller's claim:
//
//   - the attempt holds the work item's writer reservation and declared write
//     access, and the workspace admits source (while an archive is pending only
//     the reserved writer's commits are still accepted, so a push authorized
//     before the request is not lost; nothing is accepted once it completed);
//   - the repository is associated, created, and the ref is its task branch;
//   - the push record, reloaded through the push store, is applied, its entry
//     for the ref is applied and not a delete, its new value is the commit
//     (gitv1.PinnedCommit), it belongs to the repository and was made by this
//     attempt; a record that is still authorized, committing or unknown keeps
//     the output pending (ErrPushPending) and a refused one is ErrPushNotApplied;
//   - the commit's content, read server-side from the internal host, descends
//     from the commit accepted before and contains none of the secret values the
//     attempt was given or the run disclosed to its other steps (the prior-
//     disclosure scan of disclosures.go; ErrSecretInSource, nothing accepted).
//
// Acceptance moves the association's accepted commit (and the workspace's when
// the repository is the primary), records the checkpoint and its retained-ref
// intent and emits workspace.checkpoint_created in one transaction, then the
// retained ref refs/kodama/runs/<run>/<n> is created on the host and confirmed.
// A repeated call returns the same checkpoint and finishes an unfinished pin; a
// retained ref that could not be confirmed leaves the checkpoint accepted and
// the call failing with ErrUnavailable, to be repeated or reconciled.
func (s *Service) AcceptCheckpoint(ctx context.Context, actor Actor, in CheckpointInput) (CheckpointResult, error) {
	if err := firstErr(in.Scope.Validate(), in.AttemptID.Validate(), in.RepositoryID.Validate(), in.Commit.Validate(),
		validateID("push id", in.PushID), gitv1.ValidateBranchRef(in.Ref)); err != nil {
		return CheckpointResult{}, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	if err := s.authorize(ctx, actor, ActionCheckpoint, in.Scope, Resource{"attempt", string(in.AttemptID)}); err != nil {
		return CheckpointResult{}, err
	}
	if err := s.needHosting(); err != nil {
		return CheckpointResult{}, err
	}
	if s.d.Pushes == nil || s.d.Inspector == nil {
		return CheckpointResult{}, fmt.Errorf("%w: push records and push inspection", ErrNotConfigured)
	}
	auth, err := s.d.Attempts.Authority(ctx, in.Scope, in.AttemptID)
	switch {
	case errors.Is(err, ErrNotFound):
		return CheckpointResult{}, ErrNotFound
	case err != nil:
		return CheckpointResult{}, unavailable("attempt authority", err)
	case auth.Access != AccessWrite:
		return CheckpointResult{}, ErrNotWriter
	}
	var out CheckpointResult
	err = retry(3, func() error {
		var err error
		out, err = s.acceptOnce(ctx, actor, in, auth.WorkspaceID)
		return err
	})
	return out, err
}

func (s *Service) acceptOnce(ctx context.Context, actor Actor, in CheckpointInput, wsID ID) (CheckpointResult, error) {
	agg, err := s.d.Store.View(ctx, in.Scope, wsID, Load{Associations: true, Checkpoints: true})
	if err != nil {
		return CheckpointResult{}, err
	}
	w := agg.Workspace
	if err := w.requireInternal("a checkpoint"); err != nil {
		return CheckpointResult{}, err
	}
	if cp := FindCheckpoint(agg.Checkpoints, in.RepositoryID, in.PushID, in.Ref); cp != nil {
		if cp.Commit != in.Commit || cp.AttemptID != in.AttemptID {
			return CheckpointResult{}, ErrPushNotApplied
		}
		return s.finishPin(ctx, actor, w, cp, true)
	}
	assoc, err := checkpointAuthority(w, agg.Associations, in)
	if err != nil {
		return CheckpointResult{}, err
	}

	rec, err := s.d.Pushes.Reload(ctx, in.Scope, string(in.RepositoryID), in.PushID)
	switch {
	case isContract(err, domain.CodeNotFound):
		return CheckpointResult{}, ErrNotFound
	case err != nil:
		return CheckpointResult{}, unavailable("push record", err)
	}
	run, err := checkPushRecord(rec, in)
	if err != nil {
		return CheckpointResult{}, err
	}

	repo, err := s.internalRepository(ctx, in.Scope, in.RepositoryID)
	if err != nil {
		return CheckpointResult{}, err
	}
	needles, err := s.d.Secrets.Needles(ctx, in.Scope, in.AttemptID)
	if err != nil {
		return CheckpointResult{}, unavailable("secret checks", err)
	}
	since := assoc.AcceptedCommit
	ictx, cancel := s.gitCtx(ctx)
	rel, err := s.d.Inspector.Inspect(ictx, InspectRequest{Repo: s.internalRepoRef(in.Scope, repo), Ref: in.Ref, Commit: in.Commit, Since: since, Needles: needles})
	cancel()
	switch {
	case errors.Is(err, ErrSecretInSource):
		if rerr := s.refuseCheckpoint(ctx, actor, w, in, ReasonSecretInSource); rerr != nil {
			return CheckpointResult{}, rerr
		}
		return CheckpointResult{}, ErrSecretInSource
	case errors.Is(err, ErrLimitExceeded):
		return CheckpointResult{}, err
	case err != nil:
		return CheckpointResult{}, unavailable("pushed commit", err)
	case rel == RelationDiverged:
		if rerr := s.refuseCheckpoint(ctx, actor, w, in, ReasonNotFastForward); rerr != nil {
			return CheckpointResult{}, rerr
		}
		return CheckpointResult{}, ErrDiverged
	case rel != RelationDescends && rel != RelationContained:
		return CheckpointResult{}, unavailable("pushed commit", fmt.Errorf("unknown relation %q", rel))
	}

	var cp *InternalCheckpoint
	replay := false
	_, err = s.d.Store.Update(ctx, in.Scope, wsID, Load{Associations: true, Checkpoints: true}, func(a *Aggregate) (Effects, error) {
		cp, replay = nil, false
		if existing := FindCheckpoint(a.Checkpoints, in.RepositoryID, in.PushID, in.Ref); existing != nil {
			cp, replay = existing, true // a concurrent call accepted it first
			return Effects{}, nil
		}
		as, err := checkpointAuthority(a.Workspace, a.Associations, in)
		if err != nil {
			return Effects{}, err
		}
		if as.AcceptedCommit != since {
			return Effects{}, ErrStale // another acceptance moved the branch; judge again
		}
		pinRef, err := NextPinRef(a.Checkpoints, in.RepositoryID, run, 0)
		if err != nil {
			return Effects{}, err
		}
		advanced := rel == RelationDescends
		ncp, err := NewCheckpoint(a.Workspace, in.RepositoryID, in.PushID, in.Ref, in.Commit, since, advanced, in.AttemptID, run, pinRef, s.now())
		if err != nil {
			return Effects{}, err
		}
		fx := Effects{Checkpoints: []*InternalCheckpoint{ncp}}
		md := map[string]any{"repositoryId": string(in.RepositoryID), "pushId": in.PushID, "ref": in.Ref, "commit": string(in.Commit),
			"previousCommit": string(since), "attemptId": string(in.AttemptID), "advanced": advanced, "pinRef": pinRef}
		if advanced {
			if err := as.Advance(since, in.Commit, s.now()); err != nil {
				return Effects{}, err
			}
			if as.RepositoryID == a.Workspace.RepositoryID {
				a.Workspace.AcceptedCommit = in.Commit
			}
			a.Workspace.TouchAssociations(s.now())
			fx.Associations = []*RepositoryAssociation{as}
			fx.Events = []Event{{Kind: EventCheckpointCreated, Actor: actor, PreviousCommit: since, NewCommit: in.Commit,
				PublishingAttempt: in.AttemptID, Repository: in.RepositoryID}}
		}
		fx.Audit = []Audit{audit(actor, "workspace.checkpoint_accepted", "", md)}
		cp = ncp
		return fx, nil
	})
	if err != nil {
		return CheckpointResult{}, err
	}
	return s.finishPin(ctx, actor, w, cp, replay)
}

// checkpointAuthority checks that the workspace admits the attempt's source and
// returns the association the commit belongs to.
func checkpointAuthority(w *Workspace, assocs []*RepositoryAssociation, in CheckpointInput) (*RepositoryAssociation, error) {
	holder := w.Writer != nil && w.Writer.AttemptID == in.AttemptID
	switch {
	case w.Fence.Fenced:
		return nil, ErrFenced
	case w.State != StateAvailable:
		return nil, ErrAdmissionClosed
	case w.ArchiveRequestedAt != nil && !holder:
		// While an archive waits for unsettled pushes only the reserved writer
		// may still report its commits: accepting them is the evidence the
		// archive waits for. Everyone else, and everyone after the archive
		// completed, is refused.
		return nil, ErrAdmissionClosed
	case !holder:
		return nil, ErrNotWriter
	}
	a := findAssociation(assocs, in.RepositoryID)
	switch {
	case a == nil:
		return nil, ErrNotFound
	case a.State != AssociationCreated:
		return nil, fmt.Errorf("%w: the task branch of %s is not created", ErrInvalidTransition, in.RepositoryID)
	case a.TaskBranch != in.Ref:
		return nil, fmt.Errorf("%w: %q is not the task branch of the repository", ErrInvalidValue, in.Ref)
	}
	return a, nil
}

// checkPushRecord decides, from the reloaded record alone, whether the push
// applied exactly this commit to the ref for this attempt, and returns the run
// that made it.
func checkPushRecord(rec gitv1.PushRecord, in CheckpointInput) (domain.RunID, error) {
	if rec.PushID != in.PushID || rec.RepositoryID != string(in.RepositoryID) {
		return "", ErrNotFound
	}
	if rec.Actor.Type != gitv1.ActorWorkload || rec.Actor.AttemptID == nil || *rec.Actor.AttemptID != string(in.AttemptID) || rec.Actor.RunID == nil {
		return "", fmt.Errorf("%w: the push was not made by this attempt", ErrPushNotApplied)
	}
	run := domain.RunID(*rec.Actor.RunID)
	if err := run.Validate(); err != nil {
		return "", ErrPushNotApplied
	}
	var entry *gitv1.PushEntry
	for i := range rec.Entries {
		if rec.Entries[i].Ref == in.Ref {
			entry = &rec.Entries[i]
		}
	}
	switch {
	case entry == nil:
		return "", fmt.Errorf("%w: the push has no update of the ref", ErrPushNotApplied)
	case rec.Status == gitv1.PushAuthorized || rec.Status == gitv1.PushUnknown,
		entry.State == gitv1.EntryAuthorized || entry.State == gitv1.EntryCommitting || entry.State == gitv1.EntryUnknown:
		return "", ErrPushPending
	}
	pinned, err := gitv1.PinnedCommit(rec, in.Ref)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrPushNotApplied, err)
	case pinned != string(in.Commit):
		return "", fmt.Errorf("%w: the applied value is another commit", ErrPushNotApplied)
	case entry.Kind != gitv1.KindCreate && entry.Kind != gitv1.KindFastForward:
		return "", fmt.Errorf("%w: a %s is not a fast-forward of the task branch", ErrPushNotApplied, entry.Kind)
	}
	return run, nil
}

// refuseCheckpoint records, apart from the refused change, that a commit was
// not accepted and why. Nothing else is written.
func (s *Service) refuseCheckpoint(ctx context.Context, actor Actor, w *Workspace, in CheckpointInput, reason string) error {
	_, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{}, func(a *Aggregate) (Effects, error) {
		return Effects{Audit: []Audit{audit(actor, "workspace.checkpoint_refused", reason, map[string]any{
			"repositoryId": string(in.RepositoryID), "pushId": in.PushID, "ref": in.Ref, "commit": string(in.Commit), "attemptId": string(in.AttemptID)})}}, nil
	})
	return err
}

// finishPin creates the retained ref on the host and confirms it. The host
// arbitrates the number: a retained ref at another commit (another module pinned
// something for the same run) moves this checkpoint to the next number; the same
// commit already pinned there is adopted. An unknown outcome is settled by
// reading the ref, never assumed.
func (s *Service) finishPin(ctx context.Context, actor Actor, w *Workspace, cp *InternalCheckpoint, replay bool) (CheckpointResult, error) {
	for range maxPinAttempts {
		if cp.PinStatus == CheckpointPinConfirmed {
			return cp.result(replay), nil
		}
		rctx, cancel := s.gitCtx(ctx)
		_, err := s.d.Hosts.SetRetainedRef(rctx, w.Scope.TenantID, gitv1.SetRetainedRefRequest{
			RepositoryID: string(cp.RepositoryID), OperationID: PinOperationID(cp), Ref: cp.PinRef, Commit: string(cp.Commit)})
		cancel()
		confirmed := err == nil
		if !confirmed {
			value, ok, rerr := s.readRef(ctx, w.Scope, cp.RepositoryID, cp.PinRef)
			switch {
			case rerr == nil && ok && value == cp.Commit:
				confirmed = true // the request had been applied, or the commit was pinned there already
			case rerr == nil && ok && isContract(err, gitv1.CodeRetainedRefExists):
				next, nerr := s.repointPin(ctx, w, cp)
				if nerr != nil {
					return CheckpointResult{}, nerr
				}
				cp = next
				continue
			default:
				return CheckpointResult{}, unavailable("retained reference", err)
			}
		}
		next, cerr := s.confirmCheckpointPin(ctx, actor, w, cp)
		if cerr != nil {
			return CheckpointResult{}, cerr
		}
		cp = next
	}
	return CheckpointResult{}, unavailable("retained reference", errors.New("no free retained ref number"))
}

// confirmCheckpointPin records the confirmed retained ref of a checkpoint.
func (s *Service) confirmCheckpointPin(ctx context.Context, actor Actor, w *Workspace, cp *InternalCheckpoint) (*InternalCheckpoint, error) {
	var out *InternalCheckpoint
	_, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{Checkpoints: true}, func(a *Aggregate) (Effects, error) {
		cur := FindCheckpoint(a.Checkpoints, cp.RepositoryID, cp.PushID, cp.Ref)
		if cur == nil {
			return Effects{}, ErrNotFound
		}
		out = cur
		if cur.PinRef != cp.PinRef {
			return Effects{}, ErrStale // repointed meanwhile; the confirmation was for another name
		}
		if !cur.ConfirmPin(s.now()) {
			return Effects{}, nil
		}
		return Effects{Checkpoints: []*InternalCheckpoint{cur}, Audit: []Audit{audit(actor, "workspace.checkpoint_pinned", "", map[string]any{
			"repositoryId": string(cur.RepositoryID), "pushId": cur.PushID, "pinRef": cur.PinRef, "commit": string(cur.Commit)})}}, nil
	})
	return out, err
}

// repointPin moves an unconfirmed pin to the next free number of the run.
func (s *Service) repointPin(ctx context.Context, w *Workspace, cp *InternalCheckpoint) (*InternalCheckpoint, error) {
	_, n, err := gitv1.ParseRetainedRef(cp.PinRef)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	var out *InternalCheckpoint
	_, err = s.d.Store.Update(ctx, w.Scope, w.ID, Load{Checkpoints: true}, func(a *Aggregate) (Effects, error) {
		cur := FindCheckpoint(a.Checkpoints, cp.RepositoryID, cp.PushID, cp.Ref)
		if cur == nil {
			return Effects{}, ErrNotFound
		}
		out = cur
		if cur.PinRef != cp.PinRef {
			return Effects{}, nil // already repointed by a concurrent call
		}
		next, err := NextPinRef(a.Checkpoints, cur.RepositoryID, cur.RunID, n+1)
		if err != nil {
			return Effects{}, err
		}
		if err := cur.RepointPin(next); err != nil {
			return Effects{}, err
		}
		return Effects{Checkpoints: []*InternalCheckpoint{cur}}, nil
	})
	return out, err
}

// reconcileInternal finishes the durable work of an internally hosted
// workspace: task branches still in intent, retained refs not yet confirmed and
// an archive whose admission is closed.
func (s *Service) reconcileInternal(ctx context.Context, scope domain.Scope, id ID) error {
	if err := s.needHosting(); err != nil {
		return err
	}
	var errs []error
	if _, _, err := s.driveInternal(ctx, system, scope, id); err != nil {
		errs = append(errs, err)
	}
	agg, err := s.d.Store.View(ctx, scope, id, Load{Checkpoints: true})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, cp := range agg.Checkpoints {
		if cp.PinStatus != CheckpointPinIntent {
			continue
		}
		if _, err := s.finishPin(ctx, system, agg.Workspace, cp, true); err != nil {
			errs = append(errs, err)
		}
	}
	if w := agg.Workspace; w.State == StateAvailable && w.ArchiveRequestedAt != nil && w.Pending == nil {
		if _, err := s.finishArchive(ctx, system, scope, id); err != nil && !errors.Is(err, ErrArchiveUncertain) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

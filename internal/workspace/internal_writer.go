package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// HoldsWriter reports whether the attempt currently holds the writer
// reservation of the work item's internally hosted workspace and the workspace
// still admits source. It is the workspace module's side of gitpush.Reservations:
// the push authorizer asks it before a task-branch update by an attempt grant,
// and the answer is what makes the reserved writer, and only it, able to push
// the task branch (WS-18, GR-07).
//
// Closing admission ends the writer's push authority: once the workspace is
// fenced, not available or has an archive requested the answer is false for
// every attempt, so no new push is authorized while the workspace settles. A
// push that was authorized before admission closed stays unsettled in gitpush's
// records, and archive does not complete while it is (finishArchive).
//
// gitpush's obligation. Asking before authorizing, or inside the authorizing
// transaction, is not sufficient: the answer can turn false (the writer is
// released, an archive is requested) after the question and before the
// `authorized` record commits, and an uncommitted record is invisible to
// RefPushes.UnsettledOnRef, so the workspace's gate and archive check would see
// neither the record nor the closed admission. gitpush must therefore call
// HoldsWriter again after the `authorized` record is durably committed and
// before it answers the worker; when the answer is false it refuses the push
// and settles the record (nothing applies). Then either UnsettledOnRef sees the
// record or the recheck sees the closed or released admission. A recheck at
// CommitIntent is an additional guard, not the only one.
//
// It is a plain read of the committed reservation: gitpush calls it before its
// own transaction, never inside one, so it takes no transaction. A work item
// without a workspace, a foreign one, an externally hosted one and a released
// reservation all answer false; the push authorizer treats false as "no
// reservation" and refuses the update.
func (s *Service) HoldsWriter(ctx context.Context, scope domain.Scope, workItemID, attemptID string) (bool, error) {
	item, attempt := domain.WorkItemID(workItemID), domain.AttemptID(attemptID)
	if err := firstErr(scope.Validate(), item.Validate(), attempt.Validate()); err != nil {
		return false, nil
	}
	w, err := s.d.Store.GetByWorkItem(ctx, scope, item)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return w.Internal() && w.AdmissionOpen() && w.Writer != nil && w.Writer.AttemptID == attempt, nil
}

// ReserveInternalWriterMutation returns the pure reservation mutation of an
// internally hosted workspace: the same single slot as ReserveWriterMutation,
// pinned at the primary repository's accepted commit. A repeat by the attempt
// that already holds the slot is idempotent even when its own checkpoints have
// moved the accepted commit since.
//
// observed is the workspace version the caller read before it checked the
// writer gate and synchronized the repositories (CheckWriterGateAt returns it):
// those read other modules' records and cannot run inside a mutation, so the
// mutation refuses (ErrStale, the caller retries from the gate) when the
// workspace changed since, which includes every reservation, release and
// synchronization. A zero observed version is refused: an unbound reservation
// could commit against a stale gate answer.
func ReserveInternalWriterMutation(in ReserveWriterInput, observed domain.Version, now func() time.Time) Mutation {
	return func(a *Aggregate) (Effects, error) {
		w := a.Workspace
		if err := w.requireInternal("an internal writer reservation"); err != nil {
			return Effects{}, err
		}
		if w.Writer != nil && w.Writer.AttemptID == in.AttemptID {
			return Effects{}, nil
		}
		if observed == 0 {
			return Effects{}, fmt.Errorf("%w: a reservation needs the version the gate was checked at", ErrInvalidValue)
		}
		if w.Version != observed {
			return Effects{}, ErrStale
		}
		return reserveInternal(w, in, now)
	}
}

func reserveInternal(w *Workspace, in ReserveWriterInput, now func() time.Time) (Effects, error) {
	if err := w.ReserveInternalWriter(in.AttemptID, now()); err != nil {
		return Effects{}, err
	}
	return Effects{Audit: []Audit{audit(in.Actor, "workspace.writer_reserved", "", map[string]any{
		"attemptId": string(in.AttemptID), "hosting": string(HostingInternal), "acceptedCommit": string(w.AcceptedCommit)})}}, nil
}

// CheckWriterGate refuses writer assignment while a push outcome is unsettled
// on the task branch of any created repository of the workspace (WS-11, GR-09):
// an unknown outcome (ErrWriterBlocked), or a push that is authorized or
// committing (ErrWriterBlocked and ErrPushPending: the previous writer's
// authority was checked when it started, so it may still land). A new writer
// would build on an unresolved tip. A missing gate or push-record reader fails
// closed; an error from either refuses too.
func (s *Service) CheckWriterGate(ctx context.Context, scope domain.Scope, id ID) error {
	_, err := s.CheckWriterGateAt(ctx, scope, id)
	return err
}

// CheckWriterGateAt is CheckWriterGate that also returns the workspace version
// the gate was read after: the version ReserveInternalWriterMutation binds the
// reservation to. The version is captured before any gate question is asked, so
// a change to the workspace between the gate and the reservation is a stale
// reservation, never an admitted one.
func (s *Service) CheckWriterGateAt(ctx context.Context, scope domain.Scope, id ID) (domain.Version, error) {
	agg, err := s.d.Store.View(ctx, scope, id, Load{Associations: true})
	if err != nil {
		return 0, err
	}
	return agg.Workspace.Version, s.checkGate(ctx, agg.Workspace, agg.Associations)
}

func (s *Service) checkGate(ctx context.Context, w *Workspace, assocs []*RepositoryAssociation) error {
	if s.d.Gate == nil {
		return fmt.Errorf("%w: push outcome gate", ErrNotConfigured)
	}
	if s.d.RefPushes == nil {
		return fmt.Errorf("%w: push records by ref", ErrNotConfigured)
	}
	for _, a := range assocs {
		if a.State != AssociationCreated {
			continue
		}
		blocked, err := s.d.Gate.UnknownBlocksWriter(ctx, w.Scope, string(a.RepositoryID), a.TaskBranch)
		if err != nil {
			return unavailable("push outcomes", err)
		}
		if blocked {
			return fmt.Errorf("%w: repository %s", ErrWriterBlocked, a.RepositoryID)
		}
		if err := s.requireSettled(ctx, w.Scope, a); err != nil {
			if errors.Is(err, ErrPushPending) {
				return fmt.Errorf("%w: %w", ErrWriterBlocked, err)
			}
			return err // an outage stays retryable; it is not a block
		}
	}
	return nil
}

// requireSettled is ErrPushPending when a push record of the repository has an
// entry for the association's task branch that is authorized, committing or
// unknown.
func (s *Service) requireSettled(ctx context.Context, scope domain.Scope, a *RepositoryAssociation) error {
	if s.d.RefPushes == nil {
		return fmt.Errorf("%w: push records by ref", ErrNotConfigured)
	}
	pending, err := s.d.RefPushes.UnsettledOnRef(ctx, scope, string(a.RepositoryID), a.TaskBranch)
	switch {
	case err != nil:
		return s.refPushError("push records", err)
	case pending:
		return fmt.Errorf("%w: repository %s", ErrPushPending, a.RepositoryID)
	}
	return nil
}

func (s *Service) refPushError(what string, err error) error {
	if isContract(err, domain.CodeNotFound) {
		return ErrNotFound
	}
	return unavailable(what, err)
}

// ReserveInternalWriter admits the single writer of an internally hosted
// workspace. The one-writer-per-work-item rule is unchanged: competing attempts
// yield one reservation and the rest ErrWriterBusy (WS-03).
//
// Before an attempt is assigned, a push that is unknown, authorized or
// committing on any task branch refuses it (ErrWriterBlocked), and each
// repository's task branch is synchronized: a fast-forward is adopted as the
// accepted commit only when applied push records show every step was a human's
// (or an accepted checkpoint), a rewritten or missing branch refuses admission
// (ErrDiverged) and a workload push that was never accepted refuses it too
// (ErrCheckpointRequired); none of them is ever adopted.
//
// The gate answers and the synchronization are observed outside SQL after the
// workspace version is read, and applied together with the reservation in one
// mutation that requires the version unchanged: a reservation, release or
// synchronization in between makes the observation stale and it is repeated
// (bounded; ErrStale after that), so a reservation never commits against a gate
// answer that predates another writer's unknown push. Repeating the call for the
// attempt that holds the slot returns the workspace.
func (s *Service) ReserveInternalWriter(ctx context.Context, actor Actor, in ReserveWriterInput) (*Workspace, error) {
	if err := firstErr(in.WorkspaceID.Validate(), in.AttemptID.Validate()); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionAdmit, in.Scope, Resource{"workspace", string(in.WorkspaceID)}); err != nil {
		return nil, err
	}
	if err := s.needHosting(); err != nil {
		return nil, err
	}
	if err := s.checkWriterAuthority(ctx, in.Scope, in.WorkspaceID, in.AttemptID); err != nil {
		return nil, err
	}
	in.Actor = actor
	var out *Workspace
	err := retry(admissionRetries, func() error {
		agg, err := s.d.Store.View(ctx, in.Scope, in.WorkspaceID, Load{Associations: true, Checkpoints: true})
		if err != nil {
			return err
		}
		w := agg.Workspace
		if err := w.requireInternal("an internal writer reservation"); err != nil {
			return err
		}
		if w.Writer != nil && w.Writer.AttemptID == in.AttemptID {
			out = w
			return nil
		}
		switch {
		case w.Writer != nil:
			return ErrWriterBusy
		case w.State != StateAvailable:
			return w.transitionErr("reserve writer")
		case w.Fence.Fenced:
			return ErrFenced
		case w.ArchiveRequestedAt != nil:
			return ErrAdmissionClosed
		}
		observed := w.Version
		if err := s.checkGate(ctx, w, agg.Associations); err != nil {
			return err
		}
		decisions, err := s.observeSync(ctx, w, agg.Associations, agg.Checkpoints)
		if err != nil {
			return err
		}
		refusal := firstRefusal(decisions)
		cur, err := s.d.Store.Update(ctx, in.Scope, in.WorkspaceID, Load{Associations: true}, func(a *Aggregate) (Effects, error) {
			if err := a.Workspace.requireInternal("an internal writer reservation"); err != nil {
				return Effects{}, err
			}
			if a.Workspace.Writer != nil && a.Workspace.Writer.AttemptID == in.AttemptID {
				return Effects{}, nil
			}
			if a.Workspace.Version != observed {
				return Effects{}, ErrStale
			}
			fx, err := s.applySync(a, actor, decisions)
			if err != nil || refusal != nil {
				return fx, err
			}
			rfx, err := reserveInternal(a.Workspace, in, s.now)
			if err != nil {
				return Effects{}, err
			}
			fx.Audit = append(fx.Audit, rfx.Audit...)
			return fx, nil
		})
		if err != nil {
			return err
		}
		if refusal != nil {
			return fmt.Errorf("refusing admission: %w", refusal)
		}
		out = cur.Workspace
		return nil
	})
	return out, err
}

// admissionRetries bounds how often an observation is repeated after another
// change made it stale.
const admissionRetries = 4

// syncInternal is Sync for an internally hosted workspace: it observes each
// created task branch outside SQL and applies what it found in one mutation
// that checks ifMatch (when non-zero), that the workspace is still available
// and that every accepted commit still equals the one observed against. A
// change in between is ErrStale and the observation is repeated.
func (s *Service) syncInternal(ctx context.Context, actor Actor, scope domain.Scope, id ID, ifMatch domain.Version) (*Workspace, error) {
	if err := s.needHosting(); err != nil {
		return nil, err
	}
	var out *Workspace
	err := retry(admissionRetries, func() error {
		agg, err := s.d.Store.View(ctx, scope, id, Load{Associations: true, Checkpoints: true})
		if err != nil {
			return err
		}
		w := agg.Workspace
		if err := w.CheckVersion(ifMatch); err != nil {
			return err
		}
		if w.State != StateAvailable {
			return w.transitionErr("synchronize")
		}
		decisions, err := s.observeSync(ctx, w, agg.Associations, agg.Checkpoints)
		if err != nil {
			return err
		}
		cur, err := s.d.Store.Update(ctx, scope, id, Load{Associations: true}, func(a *Aggregate) (Effects, error) {
			if err := a.Workspace.requireInternal("synchronization"); err != nil {
				return Effects{}, err
			}
			if err := a.Workspace.CheckVersion(ifMatch); err != nil {
				return Effects{}, err
			}
			if a.Workspace.State != StateAvailable {
				return Effects{}, a.Workspace.transitionErr("synchronize")
			}
			return s.applySync(a, actor, decisions)
		})
		if err != nil {
			return err
		}
		out = cur.Workspace
		return firstRefusal(decisions)
	})
	return out, err
}

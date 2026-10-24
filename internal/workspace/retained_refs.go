package workspace

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// PinInput requests retention of a commit for a run or reader.
type PinInput struct {
	Scope       domain.Scope
	WorkspaceID ID
	Commit      Commit
	RunID       domain.RunID
}

// PinCommit retains a commit of the workspace's history for a reader. The
// intent is persisted first, the remote reference is created and confirmed, and
// only then is a usable pin returned. Repeating the same request returns the
// same pin. A concurrent removal of the same commit returns ErrRetentionBusy.
func (s *Service) PinCommit(ctx context.Context, actor Actor, in PinInput) (*RetainedRef, error) {
	if err := s.authorize(ctx, actor, ActionPin, in.Scope, Resource{"workspace", string(in.WorkspaceID)}); err != nil {
		return nil, err
	}
	if err := in.Commit.Validate(); err != nil {
		return nil, err
	}
	w, err := s.d.Store.Get(ctx, in.Scope, in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireInHistory(ctx, w, in.Commit); err != nil {
		return nil, err
	}
	return s.pin(ctx, actor, in.Scope, in.WorkspaceID, in.Commit, PinReader, in.RunID, "")
}

// PinWriterOutput retains the published output of one specific successful
// writer attempt. A writer that failed, was cancelled, lost the expected-tip
// race, or has not settled cannot be selected.
func (s *Service) PinWriterOutput(ctx context.Context, actor Actor, scope domain.Scope, ws ID, attempt domain.AttemptID, run domain.RunID) (*RetainedRef, error) {
	commit, err := s.SelectWriterOutput(ctx, actor, scope, ws, attempt)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, actor, ActionPin, scope, Resource{"workspace", string(ws)}); err != nil {
		return nil, err
	}
	return s.pin(ctx, actor, scope, ws, commit, PinWriterOutput, run, attempt)
}

// SelectWriterOutput returns the commit a downstream reader may bind: the
// published commit of a specific, successfully settled writer attempt.
func (s *Service) SelectWriterOutput(ctx context.Context, actor Actor, scope domain.Scope, ws ID, attempt domain.AttemptID) (Commit, error) {
	if err := firstErr(ws.Validate(), attempt.Validate()); err != nil {
		return "", ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionRead, scope, Resource{"workspace", string(ws)}); err != nil {
		return "", err
	}
	agg, err := s.d.Store.View(ctx, scope, ws, Load{Receipt: attempt})
	if err != nil {
		return "", err
	}
	return agg.Receipt.SelectableOutput()
}

func (s *Service) requireInHistory(ctx context.Context, w *Workspace, commit Commit) error {
	if w.State == StateProvisioning {
		return fmt.Errorf("%w: workspace is provisioning", ErrInvalidTransition)
	}
	if commit == w.AcceptedCommit {
		return nil
	}
	rctx, cancel := s.gitCtx(ctx)
	defer cancel()
	info, err := s.d.Refs.BranchContains(rctx, s.repoRef(w), w.WorkBranch, commit)
	if err != nil {
		return unavailable("remote history", err)
	}
	if !info.Exists || !info.Contains {
		return fmt.Errorf("%w: commit is not in the workspace history", ErrInvalidValue)
	}
	return nil
}

func (s *Service) pin(ctx context.Context, actor Actor, scope domain.Scope, id ID, commit Commit, purpose PinPurpose, run domain.RunID, attempt domain.AttemptID) (*RetainedRef, error) {
	var pin *RetainedRef
	agg, err := s.d.Store.Update(ctx, scope, id, Load{Pins: true}, func(a *Aggregate) (Effects, error) {
		pin = nil
		if a.Workspace.State == StateProvisioning {
			return Effects{}, a.Workspace.transitionErr("pin")
		}
		for _, p := range a.Pins {
			if p.RunID == run && p.Commit == commit && p.Purpose == purpose && p.AttemptID == attempt && p.Live() {
				pin = p
				return Effects{}, nil
			}
		}
		if err := CheckPinAdmission(commit, a.Pins); err != nil {
			return Effects{}, err
		}
		np, err := NewPin(PinID(s.d.NewID("pin")), a.Workspace, commit, purpose, run, attempt, s.o.RetainedRefPrefix, s.now())
		if err != nil {
			return Effects{}, err
		}
		pin = np
		return Effects{Pins: []*RetainedRef{np}, Audit: []Audit{audit(actor, "workspace.retained_ref.requested", "", map[string]any{
			"pinId": string(np.ID), "commit": string(commit), "ref": np.RefName, "purpose": string(purpose), "runId": string(run)})}}, nil
	})
	if err != nil {
		return nil, err
	}
	if pin.Status == PinConfirmed {
		return pin, nil
	}
	return s.confirmPin(ctx, actor, agg.Workspace, pin)
}

// confirmPin creates the remote reference (idempotently) and records the
// confirmation. Until then the pin is not usable.
func (s *Service) confirmPin(ctx context.Context, actor Actor, w *Workspace, pin *RetainedRef) (*RetainedRef, error) {
	rctx, cancel := s.gitCtx(ctx)
	defer cancel()
	if err := s.d.Publisher.CreateRef(rctx, s.repoRef(w), pin.RefName, pin.Commit, w.WorkBranch); err != nil {
		return nil, unavailable("retained reference", err)
	}
	var out *RetainedRef
	_, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{Pins: true}, func(a *Aggregate) (Effects, error) {
		p := findPin(a.Pins, pin.ID)
		if p == nil {
			return Effects{}, ErrNotFound
		}
		out = p
		if p.Status != PinIntent {
			return Effects{}, nil
		}
		if err := p.Confirm(s.now()); err != nil {
			return Effects{}, err
		}
		return Effects{Pins: []*RetainedRef{p}, Audit: []Audit{audit(actor, "workspace.retained_ref.confirmed", "", map[string]any{"pinId": string(p.ID), "commit": string(p.Commit)})}}, nil
	})
	return out, err
}

func findPin(pins []*RetainedRef, id PinID) *RetainedRef {
	for _, p := range pins {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// ReleasePin records that nothing depends on the pin any more. The reference is
// kept; deleting it is a separate, audited, policy-gated request.
func (s *Service) ReleasePin(ctx context.Context, actor Actor, scope domain.Scope, ws ID, pin PinID) (*RetainedRef, error) {
	if err := s.authorize(ctx, actor, ActionPin, scope, Resource{"workspace", string(ws)}); err != nil {
		return nil, err
	}
	var out *RetainedRef
	_, err := s.d.Store.Update(ctx, scope, ws, Load{Pins: true}, func(a *Aggregate) (Effects, error) {
		p := findPin(a.Pins, pin)
		if p == nil {
			return Effects{}, ErrNotFound
		}
		out = p
		before := p.Status
		if err := p.Release(s.now()); err != nil {
			return Effects{}, err
		}
		if before == p.Status {
			return Effects{}, nil
		}
		return Effects{Pins: []*RetainedRef{p}, Audit: []Audit{audit(actor, "workspace.retained_ref.released", "", map[string]any{"pinId": string(p.ID)})}}, nil
	})
	return out, err
}

// RequestRemoval audits and evaluates a request to delete a retained
// reference, including denied and deferred requests. Deletion needs an explicit
// DeletionPolicy grant, a released pin and a settled workspace; a live
// reference is never removed. The remote deletion is confirmed before the pin
// is recorded as removed, and an interrupted deletion is resumed by Reconcile.
func (s *Service) RequestRemoval(ctx context.Context, actor Actor, scope domain.Scope, ws ID, pin PinID, reason string) error {
	if err := s.authorize(ctx, actor, ActionRemove, scope, Resource{"workspace", string(ws)}); err != nil {
		return err
	}
	if len(reason) > 512 {
		return fmt.Errorf("%w: reason is too long", ErrInvalidValue)
	}
	cur, err := s.d.Store.View(ctx, scope, ws, Load{Pins: true})
	if err != nil {
		return err
	}
	target := findPin(cur.Pins, pin)
	if target == nil {
		return ErrNotFound
	}
	allow := false
	if s.d.Deletion != nil {
		if ok, err := s.d.Deletion.AllowDeletion(ctx, scope, target); err == nil {
			allow = ok
		} else {
			s.d.Logger.Warn("deletion policy unavailable; denying", "error", err.Error())
		}
	}

	var decision RemovalDecision
	var final *RetainedRef
	agg, err := s.d.Store.Update(ctx, scope, ws, Load{Pins: true}, func(a *Aggregate) (Effects, error) {
		p := findPin(a.Pins, pin)
		if p == nil {
			return Effects{}, ErrNotFound
		}
		decision = EvaluateRemoval(a.Workspace, p, a.Pins, allow)
		md := map[string]any{"pinId": string(p.ID), "commit": string(p.Commit), "ref": p.RefName, "verdict": string(decision.Verdict), "reason": decision.Reason, "requestReason": reason}
		fx := Effects{Audit: []Audit{audit(actor, "workspace.retained_ref.removal_requested", decision.Reason, md)}}
		if decision.Verdict == VerdictAllow {
			if err := p.BeginRemoval(s.now()); err != nil {
				return Effects{}, err
			}
			fx.Pins = []*RetainedRef{p}
			final = p
		}
		return fx, nil
	})
	if err != nil {
		return err
	}
	switch decision.Verdict {
	case VerdictDeny:
		return fmt.Errorf("%w: %s", ErrRemovalDenied, decision.Reason)
	case VerdictDefer:
		return fmt.Errorf("%w: %s", ErrRemovalDeferred, decision.Reason)
	}
	return s.finishRemoval(ctx, actor, agg.Workspace, final)
}

// finishRemoval deletes the remote reference (idempotently) and records it.
func (s *Service) finishRemoval(ctx context.Context, actor Actor, w *Workspace, pin *RetainedRef) error {
	rctx, cancel := s.gitCtx(ctx)
	defer cancel()
	if err := s.d.Publisher.DeleteRef(rctx, s.repoRef(w), pin.RefName, pin.Commit); err != nil {
		return unavailable("retained reference deletion", err)
	}
	_, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{Pins: true}, func(a *Aggregate) (Effects, error) {
		p := findPin(a.Pins, pin.ID)
		if p == nil {
			return Effects{}, ErrNotFound
		}
		if p.Status == PinRemoved {
			return Effects{}, nil
		}
		if err := p.CompleteRemoval(s.now()); err != nil {
			return Effects{}, err
		}
		return Effects{Pins: []*RetainedRef{p}, Audit: []Audit{audit(actor, "workspace.retained_ref.removed", "", map[string]any{"pinId": string(p.ID), "commit": string(p.Commit)})}}, nil
	})
	return err
}

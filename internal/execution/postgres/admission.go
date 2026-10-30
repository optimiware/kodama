package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/workspace"
)

// Admit implements execution.Store in its own transaction.
func (s *Store) Admit(ctx context.Context, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	if p == nil || p.Aggregate == nil || p.Aggregate.Run == nil {
		return nil, false, execution.Invalidf("an admission plan is required")
	}
	var run *execution.Run
	var created bool
	err := s.inTenant(ctx, p.Aggregate.Run.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		run, created, err = s.AdmitInTx(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return run, created, nil
}

// AdmitInTx creates the pinned run inside a caller-owned tenant transaction, as
// the keyed-create transaction of the HTTP foundation does. Under the work
// item's admission lock it verifies the pinned content revision, rechecks the
// requester's authority, inserts the run and its steps, registers the secret
// pins with the secrets module and records the run-bound retained-source intent
// through the workspace entities; the run's history, audit and outbox events
// commit with them. A repeat of the same run ID returns the stored run.
//
// A plan that pins delivery environments is revalidated first through
// Peers.Environments (execution.BindEnvironments): current authority, the
// retained snapshot and its exact credential bindings, all read in this
// transaction. Without that peer the admission fails closed. A cleanup run
// registers its retained configuration's exact secret pins the same way.
func (s *Store) AdmitInTx(ctx context.Context, tx pgx.Tx, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	agg := p.Aggregate
	run := agg.Run
	scope := run.Scope
	if err := scope.Validate(); err != nil {
		return nil, false, execution.Invalidf("%v", err)
	}
	if existing, err := s.loadRun(ctx, tx, scope, run.ID, lockNone); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, execution.ErrNotFound) {
		return nil, false, err
	}
	env := s.newEnv(tx, scope)
	ctx = execution.WithActor(ctx, p.Actor)
	if p.RequestID != "" {
		ctx = execution.WithRequestID(ctx, p.RequestID)
	}
	var pinID string
	if run.Purpose == execution.PurposeProject {
		if err := env.Authorize(ctx, p.Actor, scope, execution.PermRunManage); err != nil {
			switch {
			case errors.Is(err, execution.ErrDenied), errors.Is(err, execution.ErrHidden):
				return nil, false, execution.ErrDenied
			}
			return nil, false, execution.Unavailablef(err, "the authority is unavailable")
		}
	}
	if run.Purpose == execution.PurposeWork {
		if err := env.takeWork(ctx, run.WorkItemID); err != nil {
			return nil, false, err
		}
		adm, err := env.Work()
		if err != nil {
			return nil, false, err
		}
		if adm.ContentRevision != p.WorkRevision {
			return nil, false, fmt.Errorf("%w: the work item's specification changed during admission", execution.ErrConflict)
		}
		if err := env.Authorize(ctx, p.Actor, scope, execution.PermRunManage); err != nil {
			switch {
			case errors.Is(err, execution.ErrDenied), errors.Is(err, execution.ErrHidden):
				return nil, false, execution.ErrDenied
			}
			return nil, false, execution.Unavailablef(err, "the authority is unavailable")
		}
		pinID = s.opts.NewID("pin")
		run.RetainedRefID = pinID
	}
	// Revalidate every pinned environment (current authority; the retained,
	// unrevoked snapshot and its exact credential bindings) through this same
	// transaction. It reads only; no network call is made and no lock beyond
	// the documented order is taken. A failure aborts before anything is saved.
	if err := execution.BindEnvironments(ctx, s.bindEnvironments(tx), p); err != nil {
		return nil, false, err
	}
	if err := s.insertRun(ctx, tx, run); err != nil {
		return nil, false, err
	}
	for _, st := range agg.Steps {
		if err := s.insertStep(ctx, tx, scope, st); err != nil {
			return nil, false, err
		}
	}
	// Cleanup runs pin the exact credential versions of the deployment's
	// retained configuration the same way, so a revoked version refuses the
	// admission and the pin is released with the run.
	if len(p.Secrets) > 0 {
		if err := s.peers.Secrets.RegisterPins(ctx, tx, secretspg.PinRequest{
			Scope: scope, Consumer: secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(run.ID)}, Bindings: p.Secrets,
		}); err != nil {
			switch {
			case errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrDeleted), errors.Is(err, secrets.ErrRevoked):
				return nil, false, execution.Refusedf("a pinned secret is no longer usable")
			}
			return nil, false, err
		}
	}
	if run.Purpose == execution.PurposeWork {
		if err := s.retainSource(ctx, tx, run, pinID); err != nil {
			return nil, false, err
		}
	}
	// History, audit and events: creation first, then the pending steps and any
	// skips decided at admission.
	first := []execution.Transition{{Subject: execution.SubjectRun, ID: string(run.ID), To: string(run.Status), Reason: execution.ReasonCreated, At: run.CreatedAt}}
	for _, st := range agg.Steps {
		first = append(first, execution.Transition{Subject: execution.SubjectStep, ID: st.ID, StepID: st.ID, To: string(execution.StepPending), Reason: execution.ReasonCreated, At: run.CreatedAt})
	}
	// The creation transitions are staged on a copy: the plan is reusable, so a
	// repeat after a lost commit writes the same history and event identities
	// instead of creation rows stacked on the failed attempt's.
	staged := *agg
	staged.Transitions = append(first, agg.Transitions...)
	if err := s.writeTransitions(ctx, tx, &staged); err != nil {
		return nil, false, err
	}
	s.markClean(agg)
	return run, true, nil
}

// bindEnvironments binds the delivery revalidation to tx; nil without the peer,
// which BindEnvironments turns into a closed failure for a plan with pins.
func (s *Store) bindEnvironments(tx pgx.Tx) execution.TxEnvironments {
	if s.peers.Environments == nil {
		return nil
	}
	return s.peers.Environments(tx)
}

// retainSource records the run-bound retained-source intent as a workspace pin
// in intent status, joined to this transaction through the workspace entities.
// The run may not dispatch until the remote reference is confirmed.
func (s *Store) retainSource(ctx context.Context, tx pgx.Tx, run *execution.Run, pinID string) error {
	commit := workspace.Commit(run.SourceCommit)
	_, err := s.peers.Workspaces.UpdateInTx(ctx, tx, run.Scope, workspace.ID(run.WorkspaceID), workspace.Load{Pins: true},
		func(a *workspace.Aggregate) (workspace.Effects, error) {
			if a.Workspace.State == workspace.StateProvisioning {
				return workspace.Effects{}, fmt.Errorf("%w: the workspace is provisioning", workspace.ErrInvalidTransition)
			}
			if err := workspace.CheckPinAdmission(commit, a.Pins); err != nil {
				return workspace.Effects{}, err
			}
			pin, err := workspace.NewPin(workspace.PinID(pinID), a.Workspace, commit, workspace.PinReader, run.ID, "", s.peers.RetainedRefPrefix, s.now())
			if err != nil {
				return workspace.Effects{}, err
			}
			return workspace.Effects{Pins: []*workspace.RetainedRef{pin}, Audit: []workspace.Audit{{
				Action: "workspace.retained_ref.requested", Actor: system,
				Metadata: map[string]any{"pinId": pinID, "commit": run.SourceCommit, "ref": pin.RefName, "purpose": string(workspace.PinReader), "runId": string(run.ID)},
			}}}, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspace.ErrNotFound):
		return fmt.Errorf("%w: the workspace was not found", execution.ErrNotFound)
	case errors.Is(err, workspace.ErrInvalidTransition), errors.Is(err, workspace.ErrRetentionBusy), errors.Is(err, workspace.ErrAdmissionClosed):
		return execution.Refusedf("the workspace cannot retain the source right now")
	}
	return err
}

var _ = domain.RunID("")

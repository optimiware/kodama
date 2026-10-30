package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
)

// errExists aborts the binding mutation of Request when the step already has a
// gate; the caller returns that gate.
var errExists = errors.New("gates/postgres: the step already has a gate")

func execCtx(ctx context.Context, actor gates.Actor, reqID string) context.Context {
	ctx = execution.WithActor(ctx, execution.Actor{Type: execution.ActorType(actor.Type), ID: actor.ID})
	if reqID != "" {
		ctx = execution.WithRequestID(ctx, reqID)
	}
	return ctx
}

// Request implements gates.Store: the gate row, its evidence references, its
// audit and gate.requested event and the binding of the gate to its pending
// step commit together or not at all. The execution inputs the subject was
// built from are recomputed under the run lock and a mismatch refuses the
// request, so a gate is never bound on stale inputs.
func (s *Store) Request(ctx context.Context, p *gates.RequestPlan) (*gates.Gate, bool, error) {
	if p == nil || p.Gate == nil {
		return nil, false, gates.Invalidf("a gate plan is required")
	}
	g := p.Gate
	var out *gates.Gate
	created := false
	err := s.inTenant(ctx, g.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		ctx = execCtx(ctx, p.Actor, p.RequestID)
		verify := func(agg *execution.Aggregate) error {
			if existing, err := s.byStep(ctx, tx, g.Scope, g.RunID, g.StepID, lockNone); err == nil {
				out = existing
				return errExists
			} else if !errors.Is(err, gates.ErrNotFound) {
				return err
			}
			in, set, err := gates.ResolveInputs(agg, g.StepID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(in, p.Inputs) || !reflect.DeepEqual(set, p.SourceSet) {
				return fmt.Errorf("%w: the gate inputs changed while it was prepared", execution.ErrStale)
			}
			return nil
		}
		after := func(ctx context.Context, env execution.Env) error {
			if err := s.insertGate(ctx, tx, g); err != nil {
				return err
			}
			data := eventsv1.GateRequested{SubjectDigest: g.SubjectDigest(), ContentRevision: int64(g.Subject.ContentRevision()),
				ReviewerScope: gates.ReviewerScope, Deadline: g.Deadline.UTC().Format(timestampLayout)}
			return s.record(ctx, tx, g, "requested", p.Actor, p.RequestID, "", data, map[string]any{"deadline": g.Deadline.UTC().Format(timestampLayout)})
		}
		_, _, err := s.exec.UpdateInTx(ctx, tx, g.Scope, g.RunID, execution.UpdateOptions{LockWork: true}, execution.BindGateMutation(g.StepID, string(g.ID), verify, after))
		switch {
		case errors.Is(err, errExists):
			return nil
		case err != nil:
			return err
		}
		out, created = g, true
		return nil
	})
	if err != nil {
		return nil, false, mapExecError(err)
	}
	if created {
		g.MarkClean()
	}
	return out, created, nil
}

func (s *Store) insertGate(ctx context.Context, tx pgx.Tx, g *gates.Gate) error {
	required, err := jsonMarshal(g.Required)
	if err != nil {
		return err
	}
	depID, depCfg := g.Subject.Deployment()
	// A single-repository subject stores its commit; a source-set subject stores
	// the digest of its set instead (the commits and bases are in the canonical
	// subject), which is only possible with the source provenance columns.
	set, isSet := g.Subject.SourceSet()
	var candidate, setDigest *string
	if isSet {
		if !s.sets {
			return gates.Unavailablef(nil, "a source-set subject needs the source provenance migration")
		}
		setDigest = &set.Digest
	} else {
		c := g.Subject.CandidateCommit()
		candidate = &c
	}
	cols, vals := ``, ``
	args := []any{
		string(g.Scope.TenantID), string(g.Scope.ProjectID), string(g.ID), string(g.RunID), g.StepID, string(g.Subject.WorkItemID()),
		int64(g.Subject.ContentRevision()), candidate, string(g.Subject.DefinitionID()), nstr(depID), nstr(depCfg),
		string(g.Subject.Canonical()), g.SubjectDigest(), required, string(g.Status), int64(g.Version), micros(g.RequestedAt), micros(g.Deadline),
		micros(g.CreatedAt), micros(g.UpdatedAt),
	}
	if s.sets {
		cols, vals = `, source_set_digest`, `,$21`
		args = append(args, setDigest)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO gates (tenant_id, project_id, id, run_id, step_id, work_item_id, content_revision, candidate_commit, definition_id,
		deployment_id, deployment_config_digest, subject, subject_digest, required, status, version, requested_at, deadline, created_at, updated_at`+cols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20`+vals+`)`, args...); err != nil {
		return err
	}
	for _, r := range g.Required.Evidence {
		if _, err := tx.Exec(ctx, `INSERT INTO gate_evidence (tenant_id, project_id, gate_id, artifact_id, role, digest) VALUES ($1,$2,$3,$4,$5,$6)`,
			string(g.Scope.TenantID), string(g.Scope.ProjectID), string(g.ID), string(r.ID), string(r.Role), r.Digest); err != nil {
			return err
		}
	}
	return nil
}

// mapExecError translates execution-module failures that surface through the
// joined execution transaction.
func mapExecError(err error) error {
	switch {
	case err == nil, gates.IsBusinessError(err):
		return err
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return gates.ErrNotFound
	case errors.Is(err, execution.ErrStale):
		return fmt.Errorf("%w: %v", gates.ErrInvalidTransition, err)
	case errors.Is(err, execution.ErrInvalid), errors.Is(err, execution.ErrInvalidTransition):
		return fmt.Errorf("%w: %v", gates.ErrInvalidTransition, err)
	case errors.Is(err, execution.ErrUnavailable), errors.Is(err, execution.ErrNotConfigured), errors.Is(err, execution.ErrBusy):
		return gates.Unavailablef(err, "the execution module is unavailable")
	}
	return err
}

// Decide implements gates.Store in its own transaction.
func (s *Store) Decide(ctx context.Context, p *gates.DecisionPlan) (*gates.Gate, error) {
	if p == nil {
		return nil, gates.Invalidf("a decision plan is required")
	}
	var out *gates.Gate
	err := s.inTenant(ctx, p.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = s.DecideInTx(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecideInTx records the decision inside a caller-owned tenant transaction, as
// the keyed (idempotent) decision does. It never commits, rolls back or calls
// out. Under the gate row lock it rechecks the human reviewer's current
// authority (also after a lock wait), the expected version, the subject
// digest, the deadline and the current subject facts, applies the one
// immutable decision and writes the audit row and the gate.decided event.
func (s *Store) DecideInTx(ctx context.Context, tx pgx.Tx, p *gates.DecisionPlan) (*gates.Gate, error) {
	if err := p.Scope.Validate(); err != nil || p.GateID.Validate() != nil {
		return nil, gates.ErrNotFound
	}
	g, err := s.loadGate(ctx, tx, p.Scope, p.GateID, lockUpdate)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeReviewer(ctx, tx, p.Command.Actor, p.Scope); err != nil {
		return nil, err
	}
	view, err := s.loadRunView(ctx, tx, p.Scope, g.RunID)
	if err != nil {
		return nil, err
	}
	facts, err := s.loadFacts(ctx, tx, g, view, false)
	if err != nil {
		if errors.Is(err, gates.ErrStaleSubject) {
			// Not a definitive change of the subject; refuse the decision.
			return nil, err
		}
		return nil, err
	}
	oldVersion := g.Version
	now := s.clock()
	if err := g.Decide(p.Command, facts, now); err != nil {
		return nil, err
	}
	d := g.Decision
	tag, err := tx.Exec(ctx, `UPDATE gates SET status = $4, version = $5, decided_by_type = $6, decided_by_id = $7, decision = $8, decision_reason = $9,
		decided_at = $10, updated_at = $11
		WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $12`,
		string(p.Scope.TenantID), string(p.Scope.ProjectID), string(g.ID), string(g.Status), int64(g.Version), string(d.By.Type), d.By.ID,
		string(d.Decision), d.Reason, micros(d.At), micros(g.UpdatedAt), int64(oldVersion))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, gates.ErrVersionConflict
	}
	data := eventsv1.GateDecided{
		DecidedBy: eventsv1.Actor{Type: eventsv1.ActorType(d.By.Type), ID: d.By.ID}, Decision: string(g.Status), State: string(g.Status),
		SubjectDigest: g.SubjectDigest(),
	}
	if err := s.record(ctx, tx, g, "decided", d.By, p.RequestID, d.Reason, data, map[string]any{"decision": string(d.Decision), "state": string(g.Status)}); err != nil {
		return nil, err
	}
	g.MarkClean()
	return g, nil
}

// Invalidate implements gates.Store.
func (s *Store) Invalidate(ctx context.Context, scope domain.Scope, id gates.ID, e *gates.StaleError, requestID string) (*gates.Gate, bool, error) {
	if e == nil || !e.Definitive {
		return nil, false, gates.Invalidf("only a definitive staleness is recorded")
	}
	var out *gates.Gate
	created := false
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := s.loadGate(ctx, tx, scope, id, lockUpdate)
		if err != nil {
			return err
		}
		oldVersion := g.Version
		if !g.Invalidate(e.Reason, e.SupersedingCommit, e.SupersedingRevision, s.clock()) {
			out = g
			return nil
		}
		inv := g.Invalidation
		var sc *string
		if inv.SupersedingCommit != "" {
			sc = &inv.SupersedingCommit
		}
		var sr *int64
		if inv.SupersedingRevision != 0 {
			v := inv.SupersedingRevision
			sr = &v
		}
		tag, err := tx.Exec(ctx, `UPDATE gates SET version = $4, invalidated_at = $5, invalidation_reason = $6, superseding_commit = $7, superseding_revision = $8, updated_at = $9
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $10`,
			string(scope.TenantID), string(scope.ProjectID), string(id), int64(g.Version), micros(inv.At), inv.Reason, sc, sr, micros(g.UpdatedAt), int64(oldVersion))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return gates.ErrVersionConflict
		}
		data := eventsv1.GateInvalidated{SubjectDigest: g.SubjectDigest(), Reason: inv.Reason}
		if sc != nil {
			data.SupersedingCommit = sc
		}
		if sr != nil {
			data.SupersedingRevision = sr
		}
		if err := s.record(ctx, tx, g, "invalidated", gates.System, requestID, "", data, map[string]any{"reason": inv.Reason}); err != nil {
			return err
		}
		g.MarkClean()
		out, created = g, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// Reevaluate implements gates.Store: a read-only currency check of an
// undecided or approved gate.
func (s *Store) Reevaluate(ctx context.Context, scope domain.Scope, id gates.ID) error {
	return s.inTenantRead(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := s.loadGate(ctx, tx, scope, id, lockNone)
		if err != nil {
			return err
		}
		view, err := s.loadRunView(ctx, tx, scope, g.RunID)
		if err != nil {
			return err
		}
		f, err := s.loadFacts(ctx, tx, g, view, true)
		if err != nil {
			return err
		}
		return g.CheckCurrent(f)
	})
}

// CompleteStep implements gates.Store. Under the run lock (and the work item's
// admission lock) it rechecks the gate record, the current facts and the
// original reviewer's authority, then performs the explicit pending-to-
// succeeded step transition with no attempt and no reservation.
func (s *Store) CompleteStep(ctx context.Context, scope domain.Scope, id gates.ID) (gates.StepOutcome, error) {
	g, err := s.Get(ctx, scope, id)
	if err != nil {
		return gates.StepOutcome{}, err
	}
	if g.Status != gates.StatusApproved {
		return gates.StepOutcome{}, fmt.Errorf("%w: the gate is %s", gates.ErrInvalidTransition, g.Status)
	}
	var out gates.StepOutcome
	err = s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = s.completeInTx(ctx, tx, scope, g)
		return err
	})
	if err != nil {
		return gates.StepOutcome{}, mapExecError(err)
	}
	return out, nil
}

func (s *Store) completeInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, g *gates.Gate) (gates.StepOutcome, error) {
	ctx = execCtx(ctx, gates.System, "")
	check := func(ctx context.Context, env execution.Env, agg *execution.Aggregate, step *execution.StepRun) error {
		if step.GateRef != string(g.ID) {
			return fmt.Errorf("%w: the step is bound to another gate", gates.ErrInvalidTransition)
		}
		consumed, err := s.checkConsumable(ctx, tx, g.ID, scope, agg)
		if err != nil {
			return err
		}
		return s.recheckExpiry(consumed)
	}
	_, res, err := s.exec.UpdateInTx(ctx, tx, scope, g.RunID, execution.UpdateOptions{LockWork: true}, execution.CompleteGateMutation(g.StepID, check))
	if err != nil {
		return gates.StepOutcome{}, err
	}
	var out gates.StepOutcome
	if r, ok := res.(execution.GateStepOutcome); ok {
		out = gates.StepOutcome{Completed: r.Completed, Cancelled: r.Cancelled, Waiting: r.Status == execution.StepPending}
	}
	return out, nil
}

// checkConsumable is the progression check of one gate inside a transaction
// that already holds the run lock: the gate row is read FOR SHARE, the
// approval must be applicable against the current facts, and the original
// reviewer must still hold the reviewer authority (a revocation that committed
// while this transaction waited is seen).
//
// The gate lock, the fact reads and the authority check can each block, so no
// time sampled before them is authoritative. The check samples the injected
// clock after the gate and facts are read, and the caller must pass the
// returned gate to recheckExpiry once every authority wait it needs is over:
// an approval that reached its deadline during any wait never authorizes
// progression.
func (s *Store) checkConsumable(ctx context.Context, tx pgx.Tx, id gates.ID, scope domain.Scope, agg *execution.Aggregate) (*gates.Gate, error) {
	g, err := s.loadGate(ctx, tx, scope, id, lockShare)
	if err != nil {
		return nil, err
	}
	f, err := s.loadFacts(ctx, tx, g, agg, true)
	if err != nil {
		return nil, err
	}
	if err := g.Revalidate(f, s.clock()); err != nil {
		return nil, err
	}
	if g.Decision == nil {
		return nil, fmt.Errorf("%w: the gate has no decision", gates.ErrInvalidTransition)
	}
	switch err := s.authorizeReviewer(ctx, tx, g.Decision.By, scope); {
	case err == nil:
		return g, nil
	case errors.Is(err, gates.ErrDenied), errors.Is(err, gates.ErrHidden):
		return nil, fmt.Errorf("%w: the deciding reviewer no longer holds the reviewer authority", gates.ErrReviewerAuthority)
	default:
		return nil, err
	}
}

// recheckExpiry is the final applicability check of consumed gates, made with
// a clock sample taken after every blocking read and authority check. It is
// the last step before progression is accepted; expiry equal to the deadline
// denies. It does not alter the gate: an expired approval stays approved.
func (s *Store) recheckExpiry(consumed ...*gates.Gate) error {
	now := s.clock()
	for _, g := range consumed {
		if err := g.Applicable(now); err != nil {
			return err
		}
	}
	return nil
}

// RejectStep implements gates.Store.
func (s *Store) RejectStep(ctx context.Context, scope domain.Scope, id gates.ID) (gates.StepOutcome, error) {
	g, err := s.Get(ctx, scope, id)
	if err != nil {
		return gates.StepOutcome{}, err
	}
	if g.Status != gates.StatusRejected {
		return gates.StepOutcome{}, fmt.Errorf("%w: the gate is %s", gates.ErrInvalidTransition, g.Status)
	}
	var out gates.StepOutcome
	err = s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = s.rejectInTx(ctx, tx, scope, g)
		return err
	})
	if err != nil {
		return gates.StepOutcome{}, mapExecError(err)
	}
	return out, nil
}

func (s *Store) rejectInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, g *gates.Gate) (gates.StepOutcome, error) {
	ctx = execCtx(ctx, gates.System, "")
	_, res, err := s.exec.UpdateInTx(ctx, tx, scope, g.RunID, execution.UpdateOptions{LockWork: true}, execution.RejectGateMutation(g.StepID))
	if err != nil {
		return gates.StepOutcome{}, err
	}
	var out gates.StepOutcome
	if r, ok := res.(execution.GateStepOutcome); ok {
		out = gates.StepOutcome{Rejected: r.Rejected, Waiting: r.Status == execution.StepPending}
	}
	return out, nil
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

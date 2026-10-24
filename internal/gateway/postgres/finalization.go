package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

const usageColumns = `request_id, tenant_id, project_id, run_id, attempt_id, requested_model, actual_model, model_confirmed, destination_origin,
	destination_ip, input_tokens, output_tokens, charged_tokens, basis, uncertainty, outcome, outcome_code, recorded_at`

func scanUsage(r rowScanner) (gateway.Usage, error) {
	var (
		u                             gateway.Usage
		id, tenant, project, run, att string
		actual, ip                    *string
		basis, outcome                string
	)
	if err := r.Scan(&id, &tenant, &project, &run, &att, &u.RequestedModel, &actual, &u.ModelConfirmed, &u.Destination.Origin, &ip,
		&u.InputTokens, &u.OutputTokens, &u.Charged, &basis, &u.Uncertainty, &outcome, &u.Code, &u.RecordedAt); err != nil {
		return gateway.Usage{}, err
	}
	u.RequestID, u.Scope = gateway.RequestID(id), domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	u.RunID, u.AttemptID, u.Basis, u.Result, u.RecordedAt = domain.RunID(run), domain.AttemptID(att), gateway.Basis(basis), gateway.RequestState(outcome), u.RecordedAt.UTC()
	if actual != nil {
		u.ActualModel = *actual
	}
	if ip != nil {
		u.Destination.IP = *ip
	}
	return u, nil
}

func insertUsage(ctx context.Context, tx pgx.Tx, u gateway.Usage) error {
	var actual, ip *string
	if u.ModelConfirmed {
		actual = &u.ActualModel
	}
	if u.Destination.IP != "" {
		ip = &u.Destination.IP
	}
	origin := u.Destination.Origin
	if origin == "" {
		origin = "none"
	}
	_, err := tx.Exec(ctx, `INSERT INTO model_usage (`+usageColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		string(u.RequestID), string(u.Scope.TenantID), string(u.Scope.ProjectID), string(u.RunID), string(u.AttemptID), u.RequestedModel, actual, u.ModelConfirmed,
		origin, ip, u.InputTokens, u.OutputTokens, u.Charged, string(u.Basis), u.Uncertainty, string(u.Result), u.Code, u.RecordedAt)
	return err
}

// Usage implements gateway.Store.
func (s *Store) Usage(ctx context.Context, scope domain.Scope, id gateway.RequestID) (gateway.Usage, error) {
	var out gateway.Usage
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		u, err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageColumns+` FROM model_usage WHERE tenant_id = $1 AND project_id = $2 AND request_id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if errors.Is(err, pgx.ErrNoRows) {
			return gateway.Failf(gateway.ErrNotFound, "no usage is recorded for this request")
		}
		out = u
		return err
	})
	return out, err
}

type settlement struct {
	reason  string // audit action suffix
	actor   eventsv1.Actor
	actorID string
}

// settle persists one request's settlement in the caller's transaction: the
// request row, the budget counters, the usage record, the audit entry and the
// usage.recorded event. The event identity derives from the request, so a
// retried or duplicate settlement can never append a second event.
func (s *Store) settle(ctx context.Context, tx pgx.Tx, b gateway.RunBudget, m gateway.ModelRequest, u gateway.Usage, release bool, now time.Time, who settlement) error {
	if err := b.Settle(m.ID, u.Charged, release); err != nil {
		return err
	}
	if err := saveRequest(ctx, tx, m); err != nil {
		return err
	}
	if err := saveBudget(ctx, tx, b, now); err != nil {
		return err
	}
	if err := insertUsage(ctx, tx, u); err != nil {
		return err
	}
	if err := s.auditUsage(ctx, tx, m, u, who, b); err != nil {
		return err
	}
	return s.emitUsage(ctx, tx, m, u, who)
}

func (s *Store) auditUsage(ctx context.Context, tx pgx.Tx, m gateway.ModelRequest, u gateway.Usage, who settlement, b gateway.RunBudget) error {
	// Identifiers, numbers and codes only: never content, never provider text.
	meta := map[string]any{
		"runId": string(u.RunID), "attemptId": string(u.AttemptID), "requestedModel": u.RequestedModel, "modelConfirmed": u.ModelConfirmed,
		"origin": u.Destination.Origin, "basis": string(u.Basis), "uncertainty": u.Uncertainty, "outcome": string(u.Result), "code": u.Code,
		"charged": u.Charged, "runCharged": b.Charged, "runRequests": b.Admitted, "recording": m.Recording,
	}
	if u.ModelConfirmed {
		meta["actualModel"] = u.ActualModel
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'model_request', $8, 1, $9::jsonb)`,
		string(u.Scope.TenantID), newID("aud"), string(u.Scope.ProjectID), u.RecordedAt, who.actorKind(), who.actorID, "model.request."+who.reason, string(u.RequestID), string(raw))
	return err
}

func (w settlement) actorKind() string {
	switch w.actor.Type {
	case eventsv1.ActorWorkload:
		return "workload"
	case eventsv1.ActorUser:
		return "user"
	}
	return "system"
}

func (s *Store) emitUsage(ctx context.Context, tx pgx.Tx, m gateway.ModelRequest, u gateway.Usage, who settlement) error {
	data := eventsv1.UsageRecorded{
		RunID: string(u.RunID), AttemptID: string(u.AttemptID), Model: u.EventModel(), Basis: string(u.Basis), Uncertainty: u.Uncertainty,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.appendEvent(ctx, tx, eventsv1.Envelope{
		ID: "evt_" + string(u.RequestID), SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeUsageRecorded, OccurredAt: u.RecordedAt,
		TenantID: u.Scope.TenantID, ProjectID: u.Scope.ProjectID,
		Resource: eventsv1.Resource{Type: "usage_record", ID: string(u.RequestID), Version: 1},
		Actor:    who.actor, CorrelationID: string(u.RequestID), Data: raw,
	})
}

func workloadSettlement(attempt domain.AttemptID, reason string) settlement {
	return settlement{reason: reason, actor: eventsv1.Actor{Type: eventsv1.ActorWorkload, ID: string(attempt)}, actorID: string(attempt)}
}

var reconcilerSettlement = func(reason string) settlement {
	return settlement{reason: reason, actor: eventsv1.Actor{Type: eventsv1.ActorService, ID: "model-gateway"}, actorID: "model-gateway"}
}

// operatorSettlement attributes a manual settlement to the authorizing human.
func operatorSettlement(actor gateway.Actor, reason string) settlement {
	return settlement{reason: reason, actor: eventsv1.Actor{Type: eventsv1.ActorUser, ID: actor.ID}, actorID: actor.ID}
}

// lockSettlement locks the run's budget and the request in the module's lock
// order and checks that the request is the one holding the run's slot.
func lockSettlement(ctx context.Context, tx pgx.Tx, scope domain.Scope, id gateway.RequestID, run domain.RunID, attempt domain.AttemptID) (gateway.RunBudget, gateway.ModelRequest, error) {
	m, err := loadRequest(ctx, tx, scope, id, "")
	if err != nil {
		return gateway.RunBudget{}, gateway.ModelRequest{}, err
	}
	if m.RunID != run || (attempt != "" && m.AttemptID != attempt) {
		return gateway.RunBudget{}, gateway.ModelRequest{}, gateway.Failf(gateway.ErrNotFound, "the request does not belong to this attempt")
	}
	b, found, err := loadBudget(ctx, tx, scope, m.RunID, " FOR UPDATE")
	if err != nil {
		return gateway.RunBudget{}, gateway.ModelRequest{}, err
	}
	if !found {
		return gateway.RunBudget{}, gateway.ModelRequest{}, gateway.Failf(gateway.ErrNotFound, "the run binding does not exist")
	}
	// Re-read the request under its lock now that the budget row is held.
	m, err = loadRequest(ctx, tx, scope, id, " FOR UPDATE")
	if err != nil {
		return gateway.RunBudget{}, gateway.ModelRequest{}, err
	}
	return b, m, nil
}

// Finalize implements gateway.Store. It needs no workload authority: the
// request already consumed its admission, and a cancelled attempt must still be
// able to record what its request cost. The request's identity, run and
// attempt are compared with the command, so an identifier alone is not enough.
func (s *Store) Finalize(ctx context.Context, cmd gateway.FinalizeCommand) (gateway.Usage, error) {
	if err := firstError(cmd.Scope.Validate(), cmd.RunID.Validate(), cmd.AttemptID.Validate(), cmd.RequestID.Validate()); err != nil {
		return gateway.Usage{}, gateway.Failf(gateway.ErrInvalid, "the finalization is malformed")
	}
	var out gateway.Usage
	err := s.atomically(ctx, cmd.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, m, err := lockSettlement(ctx, tx, cmd.Scope, cmd.RequestID, cmd.RunID, cmd.AttemptID)
		if err != nil {
			return err
		}
		u, err := m.Finalize(cmd.Outcome, cmd.Now)
		if err != nil {
			return err
		}
		// Only a settled remote outcome releases the slot; an uncertain remote
		// completion is charged but keeps it until an authorized operator attests its settlement.
		if err := s.settle(ctx, tx, b, m, u, !m.State.HoldsSlot(), cmd.Now, workloadSettlement(cmd.AttemptID, "finalized")); err != nil {
			return err
		}
		out = u
		return nil
	})
	return out, err
}

// Abandon implements gateway.Store.
func (s *Store) Abandon(ctx context.Context, scope domain.Scope, id gateway.RequestID, before time.Time, now time.Time) (gateway.Usage, error) {
	var out gateway.Usage
	err := s.atomically(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		first, err := loadRequest(ctx, tx, scope, id, "")
		if err != nil {
			return err
		}
		b, m, err := lockSettlement(ctx, tx, scope, id, first.RunID, "")
		if err != nil {
			return err
		}
		if !m.HeartbeatAt.Before(before) {
			return gateway.Failf(gateway.ErrSuperseded, "the request is not abandoned")
		}
		var u gateway.Usage
		release := true
		switch m.State {
		case gateway.StateAdmitted:
			u, err = m.AbandonUnsent(now)
		case gateway.StateForwarding:
			// The owning worker is gone and the provider's outcome is unknown:
			// charge the bound and keep the slot. Lease expiry alone never
			// reopens it.
			u, err = m.MarkUnknown(now)
			release = false
		default:
			return gateway.Failf(gateway.ErrSuperseded, "the request is already settled")
		}
		if err != nil {
			return err
		}
		if err := s.settle(ctx, tx, b, m, u, release, now, reconcilerSettlement("reconciled")); err != nil {
			return err
		}
		out = u
		return nil
	})
	return out, err
}

// Settle implements gateway.Store.
func (s *Store) Settle(ctx context.Context, cmd gateway.SettleCommand) error {
	scope, id, now := cmd.Scope, cmd.RequestID, cmd.Now
	if err := firstError(scope.Validate(), id.Validate()); err != nil {
		return gateway.Failf(gateway.ErrInvalid, "the settlement is malformed")
	}
	return s.atomically(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// The actor's permission is rechecked inside this transaction, before the
		// budget or request rows are locked: the management binder holds the
		// user's authorization revision until commit, so a revocation committed
		// earlier is seen and a racing one waits for this settlement. Settling an
		// already started effect needs no workload authority.
		if err := s.management(tx).Authorize(ctx, cmd.Actor, scope, gateway.PermSettings); err != nil {
			return err
		}
		first, err := loadRequest(ctx, tx, scope, id, "")
		if err != nil {
			return err
		}
		b, m, err := lockSettlement(ctx, tx, scope, id, first.RunID, "")
		if err != nil {
			return err
		}
		if err := m.Settle(now); err != nil {
			return err
		}
		if err := b.Release(m.ID); err != nil {
			return err
		}
		if err := saveRequest(ctx, tx, m); err != nil {
			return err
		}
		if err := saveBudget(ctx, tx, b, now); err != nil {
			return err
		}
		u := gateway.Usage{RequestID: m.ID, Scope: m.Scope, RunID: m.RunID, AttemptID: m.AttemptID, RequestedModel: m.Model,
			Basis: gateway.BasisEstimated, Uncertainty: gateway.UncertaintyWorkerLost, Result: gateway.StateUnknown, Code: "settled", RecordedAt: now.UTC()}
		return s.auditUsage(ctx, tx, m, u, operatorSettlement(cmd.Actor, "settled"), b)
	})
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

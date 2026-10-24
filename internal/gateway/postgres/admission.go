package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

const requestColumns = `id, tenant_id, project_id, run_id, attempt_id, ordinal, model, input_bound, output_max, charge_bound, recording,
	recording_outcome, state, outcome_code, stream_started, admitted_at, forwarding_at, finished_at, heartbeat_at`

func scanRequest(r rowScanner) (gateway.ModelRequest, error) {
	var (
		m                             gateway.ModelRequest
		id, tenant, project, run, att string
		state                         string
		recOutcome, code              *string
		forwarding, finished          *time.Time
	)
	if err := r.Scan(&id, &tenant, &project, &run, &att, &m.Ordinal, &m.Model, &m.InputBound, &m.OutputMax, &m.ChargeBound, &m.Recording,
		&recOutcome, &state, &code, &m.StreamStarted, &m.AdmittedAt, &forwarding, &finished, &m.HeartbeatAt); err != nil {
		return gateway.ModelRequest{}, err
	}
	m.ID, m.Scope.TenantID, m.Scope.ProjectID, m.RunID, m.AttemptID = gateway.RequestID(id), domain.TenantID(tenant), domain.ProjectID(project), domain.RunID(run), domain.AttemptID(att)
	m.State = gateway.RequestState(state)
	if recOutcome != nil {
		m.RecordingOutcome = *recOutcome
	}
	if code != nil {
		m.Code = *code
	}
	m.AdmittedAt, m.HeartbeatAt = m.AdmittedAt.UTC(), m.HeartbeatAt.UTC()
	if forwarding != nil {
		t := forwarding.UTC()
		m.ForwardingAt = &t
	}
	if finished != nil {
		t := finished.UTC()
		m.FinishedAt = &t
	}
	if !m.State.Valid() {
		return gateway.ModelRequest{}, gateway.Failf(gateway.ErrUnavailable, "a stored request has an unknown state")
	}
	return m, nil
}

// loadRequest locks the request addressed by its scope and identity.
func loadRequest(ctx context.Context, tx pgx.Tx, scope domain.Scope, id gateway.RequestID, lock string) (gateway.ModelRequest, error) {
	m, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM model_requests WHERE tenant_id = $1 AND project_id = $2 AND id = $3`+lock,
		string(scope.TenantID), string(scope.ProjectID), string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.ModelRequest{}, gateway.Failf(gateway.ErrNotFound, "the request does not exist in this scope")
	}
	return m, err
}

func insertRequest(ctx context.Context, tx pgx.Tx, m gateway.ModelRequest) error {
	_, err := tx.Exec(ctx, `INSERT INTO model_requests (id, tenant_id, project_id, run_id, attempt_id, ordinal, model, input_bound, output_max, charge_bound,
		recording, state, stream_started, admitted_at, heartbeat_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'admitted',false,$12,$12)`,
		string(m.ID), string(m.Scope.TenantID), string(m.Scope.ProjectID), string(m.RunID), string(m.AttemptID), m.Ordinal, m.Model,
		m.InputBound, m.OutputMax, m.ChargeBound, m.Recording, m.AdmittedAt)
	return err
}

func saveRequest(ctx context.Context, tx pgx.Tx, m gateway.ModelRequest) error {
	var code *string
	if m.Code != "" {
		code = &m.Code
	}
	tag, err := tx.Exec(ctx, `UPDATE model_requests SET state = $4, outcome_code = $5, stream_started = $6, forwarding_at = $7, finished_at = $8, heartbeat_at = $9
		WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(m.Scope.TenantID), string(m.Scope.ProjectID), string(m.ID), string(m.State), code, m.StreamStarted, m.ForwardingAt, m.FinishedAt, m.HeartbeatAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return gateway.Failf(gateway.ErrNotFound, "the request does not exist in this scope")
	}
	return nil
}

// Admit implements gateway.Store. In one transaction it rechecks the workload's
// current authority (which locks the attempt and its grant in the module-wide
// order), captures or reads the run's binding, validates the model and limits,
// and consumes one request and the run's single slot. A refusal or rollback
// consumes nothing; an unknown commit outcome means the caller must not send.
func (s *Store) Admit(ctx context.Context, cmd gateway.AdmitCommand) (gateway.Admission, error) {
	w := cmd.Workload
	if err := w.Validate(); err != nil {
		return gateway.Admission{}, err
	}
	if err := cmd.RequestID.Validate(); err != nil {
		return gateway.Admission{}, err
	}
	var out gateway.Admission
	err := s.atomically(ctx, w.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.workload.Check(ctx, tx, w, cmd.Now); err != nil {
			return err
		}
		budget, err := s.bindRun(ctx, tx, w.Scope, w.RunID, cmd.Now)
		if err != nil {
			return err
		}
		current, err := loadSettings(ctx, tx, w.Scope, " FOR SHARE")
		if err != nil {
			return err
		}
		if !current.Enabled {
			// A project that turned model access off stops new requests of runs
			// that were bound earlier; their binding is otherwise unchanged.
			return gateway.Failf(gateway.ErrNotConfigured, "model access is not enabled for this project")
		}
		if !budget.Binding.Allows(cmd.Model) {
			return gateway.Failf(gateway.ErrModelNotAllowed, "the model is not on the run's allow-list")
		}
		outputMax := cmd.OutputMax
		if outputMax == 0 {
			outputMax = budget.Binding.Limits.MaxOutputTokens
		}
		if _, err := budget.Admit(cmd.RequestID, cmd.RequestBytes, cmd.InputBound, outputMax); err != nil {
			return err
		}
		recording := cmd.RecordingAvailable && budget.Binding.Recording && current.Recording
		req, err := gateway.NewModelRequest(cmd.RequestID, w, budget.Admitted, cmd.Model, cmd.InputBound, outputMax, recording, cmd.Now)
		if err != nil {
			return err
		}
		if err := insertRequest(ctx, tx, req); err != nil {
			return err
		}
		if err := saveBudget(ctx, tx, budget, cmd.Now); err != nil {
			return err
		}
		out = gateway.Admission{Request: req, Binding: budget.Binding, Recording: recording}
		return nil
	})
	if err != nil {
		return gateway.Admission{}, err
	}
	return out, nil
}

// owned loads the request and verifies it belongs to the workload's run and
// attempt, so a request identity alone is never authority.
func owned(m gateway.ModelRequest, w gateway.Workload) error {
	if m.Scope != w.Scope || m.RunID != w.RunID || m.AttemptID != w.AttemptID {
		return gateway.Failf(gateway.ErrNotFound, "the request does not belong to this attempt")
	}
	return nil
}

// BeginForwarding implements gateway.Store.
func (s *Store) BeginForwarding(ctx context.Context, w gateway.Workload, id gateway.RequestID, now time.Time) error {
	return s.atomically(ctx, w.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.workload.Check(ctx, tx, w, now); err != nil {
			return err
		}
		m, err := loadRequest(ctx, tx, w.Scope, id, " FOR UPDATE")
		if err != nil {
			return err
		}
		if err := owned(m, w); err != nil {
			return err
		}
		if err := m.BeginForwarding(now); err != nil {
			return err
		}
		return saveRequest(ctx, tx, m)
	})
}

// Heartbeat implements gateway.Store.
func (s *Store) Heartbeat(ctx context.Context, w gateway.Workload, id gateway.RequestID, now time.Time) error {
	return s.atomically(ctx, w.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.workload.Check(ctx, tx, w, now); err != nil {
			return err
		}
		m, err := loadRequest(ctx, tx, w.Scope, id, " FOR UPDATE")
		if err != nil {
			return err
		}
		if err := owned(m, w); err != nil {
			return err
		}
		if err := m.Beat(now); err != nil {
			return err
		}
		return saveRequest(ctx, tx, m)
	})
}

// Request implements gateway.Store.
func (s *Store) Request(ctx context.Context, scope domain.Scope, id gateway.RequestID) (gateway.ModelRequest, error) {
	var out gateway.ModelRequest
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = loadRequest(ctx, tx, scope, id, "")
		return err
	})
	return out, err
}

// RecordingOutcome implements gateway.Store.
func (s *Store) RecordingOutcome(ctx context.Context, scope domain.Scope, id gateway.RequestID, recorded bool) error {
	outcome := "failed"
	if recorded {
		outcome = "recorded"
	}
	return s.atomically(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE model_requests SET recording_outcome = $4
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND recording AND recording_outcome IS NULL`,
			string(scope.TenantID), string(scope.ProjectID), string(id), outcome)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return gateway.Failf(gateway.ErrSuperseded, "the recording outcome was not stored")
		}
		return nil
	})
}

// Abandoned implements gateway.Store.
func (s *Store) Abandoned(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) ([]gateway.ModelRequest, error) {
	if limit < 1 || limit > 1000 {
		return nil, gateway.Failf(gateway.ErrInvalid, "the limit is out of range")
	}
	var out []gateway.ModelRequest
	err := s.read(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM model_requests
			WHERE tenant_id = $1 AND state IN ('admitted', 'forwarding') AND heartbeat_at < $2 ORDER BY heartbeat_at, id LIMIT $3`,
			string(tenant), before.UTC(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanRequest(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

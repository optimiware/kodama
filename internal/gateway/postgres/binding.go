package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

const bindingColumns = `tenant_id, project_id, run_id, settings_revision, endpoint, credential_ref, credential_version, models,
	max_requests, token_allowance, max_request_bytes, max_input_tokens, max_output_tokens, recording, requests_admitted, tokens_charged, created_at`

func scanBudget(r rowScanner) (gateway.RunBudget, error) {
	var (
		b                        gateway.RunBinding
		tenant, project, run, ep string
		admitted, charged        int64
		l                        gateway.Limits
	)
	if err := r.Scan(&tenant, &project, &run, &b.SettingsRevision, &ep, &b.Credential.Ref, &b.Credential.Version, &b.Models,
		&l.MaxRequests, &l.TokenAllowance, &l.MaxRequestBytes, &l.MaxInputTokens, &l.MaxOutputTokens, &b.Recording, &admitted, &charged, &b.CreatedAt); err != nil {
		return gateway.RunBudget{}, err
	}
	endpoint, err := gateway.ParseEndpoint(ep)
	if err != nil {
		return gateway.RunBudget{}, gateway.FailWith(gateway.ErrUnavailable, err, "the stored binding is inconsistent")
	}
	b.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	b.RunID, b.Endpoint, b.Limits, b.CreatedAt = domain.RunID(run), endpoint, l, b.CreatedAt.UTC()
	if err := b.Validate(); err != nil {
		return gateway.RunBudget{}, gateway.FailWith(gateway.ErrUnavailable, err, "the stored binding is inconsistent")
	}
	return gateway.RunBudget{Binding: b, Admitted: admitted, Charged: charged}, nil
}

// loadBudget reads the run's binding and counters under lock and the request
// that holds the run's slot, if any. Callers take the lock order documented in
// store.go: the binding row first, then the request row.
func loadBudget(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, lock string) (gateway.RunBudget, bool, error) {
	b, err := scanBudget(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM model_run_bindings WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3`+lock,
		string(scope.TenantID), string(scope.ProjectID), string(run)))
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.RunBudget{}, false, nil
	}
	if err != nil {
		return gateway.RunBudget{}, false, err
	}
	var holder string
	err = tx.QueryRow(ctx, `SELECT id FROM model_requests WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3
		AND state IN ('admitted', 'forwarding', 'unknown') FOR UPDATE`, string(scope.TenantID), string(scope.ProjectID), string(run)).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return gateway.RunBudget{}, false, err
	default:
		b.Unresolved = gateway.RequestID(holder)
	}
	return b, true, nil
}

func saveBudget(ctx context.Context, tx pgx.Tx, b gateway.RunBudget, now time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE model_run_bindings SET requests_admitted = $4, tokens_charged = $5, updated_at = $6
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3`,
		string(b.Binding.Scope.TenantID), string(b.Binding.Scope.ProjectID), string(b.Binding.RunID), b.Admitted, b.Charged, now.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return gateway.Failf(gateway.ErrNotFound, "the run binding does not exist")
	}
	return nil
}

// bindRun returns the run's locked budget, creating the immutable binding from
// the project's current enabled settings at the run's first model admission and
// registering the exact credential version pin for the run in the same
// transaction. A concurrent first admission serializes on the insert; the loser
// reads the winner's binding and registers nothing (the winner's commit already
// holds the pin). The pin is the only way the credential reaches this module:
// it is never added to an attempt's file or environment bindings.
func (s *Store) bindRun(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, now time.Time) (gateway.RunBudget, error) {
	b, found, err := loadBudget(ctx, tx, scope, run, " FOR UPDATE")
	if err != nil || found {
		return b, err
	}
	settings, err := loadSettings(ctx, tx, scope, "")
	if err != nil {
		return gateway.RunBudget{}, err
	}
	binding, err := gateway.BindRun(settings, run, now)
	if err != nil {
		return gateway.RunBudget{}, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO model_run_bindings (`+bindingColumns+`, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,0,0,$15,$15)
		ON CONFLICT (tenant_id, project_id, run_id) DO NOTHING`,
		string(scope.TenantID), string(scope.ProjectID), string(run), binding.SettingsRevision, binding.Endpoint.String(), binding.Credential.Ref,
		binding.Credential.Version, binding.Models, binding.Limits.MaxRequests, binding.Limits.TokenAllowance, binding.Limits.MaxRequestBytes,
		binding.Limits.MaxInputTokens, binding.Limits.MaxOutputTokens, binding.Recording, binding.CreatedAt)
	if err != nil {
		return gateway.RunBudget{}, err
	}
	b, found, err = loadBudget(ctx, tx, scope, run, " FOR UPDATE")
	if err != nil {
		return gateway.RunBudget{}, err
	}
	if !found {
		return gateway.RunBudget{}, gateway.Failf(gateway.ErrUnavailable, "the run binding could not be created")
	}
	if tag.RowsAffected() == 1 {
		if err := s.pins.Register(ctx, tx, scope, run, binding.Credential); err != nil {
			return gateway.RunBudget{}, err
		}
	}
	return b, nil
}

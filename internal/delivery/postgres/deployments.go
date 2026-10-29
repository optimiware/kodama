package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
)

const deploymentColumns = `tenant_id, project_id, id, environment_id, config_digest, namespace, source_repository_url, source_commit,
image_artifact_id, image_digest, artifact_kind, artifact_media_type, producer_attempt_id, producer_run_id, producer_step_id, tool_image, run_id, step_id, action_key, attempt_id,
request_fingerprint, status, reason, objects, resources, expires_at, cleanup_requested_at, cleanup_state, cleanup_reason, deleted_at, version, created_at, updated_at`

func scanDeployment(row pgx.Row) (delivery.Deployment, error) {
	var rec delivery.DeploymentRecord
	var tenant, project, id, env, digest, imageArtifact, imageDigest, artifactKind, artifactMediaType, prodAttempt, prodRun, run, attempt, fp, status, cleanup string
	var objects, resources []byte
	var version int64
	err := row.Scan(&tenant, &project, &id, &env, &digest, &rec.Namespace, &rec.Source.RepositoryURL, &rec.Source.Commit,
		&imageArtifact, &imageDigest, &artifactKind, &artifactMediaType, &prodAttempt, &prodRun, &rec.Producer.Step, &rec.Producer.ToolImage, &run, &rec.Step, &rec.ActionKey, &attempt,
		&fp, &status, &rec.Reason, &objects, &resources, &rec.ExpiresAt, &rec.CleanupRequestedAt, &cleanup, &rec.CleanupReason, &rec.DeletedAt, &version, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return delivery.Deployment{}, err
	}
	rec.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	rec.ID, rec.Environment, rec.ConfigDigest = delivery.DeploymentID(id), domain.EnvironmentID(env), runtimev1.Digest(digest)
	rec.Image = delivery.Image{ArtifactID: domain.ArtifactID(imageArtifact), Digest: runtimev1.Digest(imageDigest)}
	rec.Artifact = delivery.DeployableArtifact{Kind: delivery.ArtifactKind(artifactKind), ArtifactID: domain.ArtifactID(imageArtifact), Digest: runtimev1.Digest(imageDigest), MediaType: artifactMediaType}
	rec.Producer.Attempt, rec.Producer.Run = domain.AttemptID(prodAttempt), domain.RunID(prodRun)
	rec.Run, rec.Attempt, rec.Fingerprint = domain.RunID(run), domain.AttemptID(attempt), runtimev1.Digest(fp)
	rec.Status, rec.CleanupState, rec.Version = delivery.Status(status), delivery.CleanupState(cleanup), domain.Version(version)
	rec.CreatedAt, rec.UpdatedAt = utc(rec.CreatedAt), utc(rec.UpdatedAt)
	rec.ExpiresAt, rec.CleanupRequestedAt, rec.DeletedAt = timePtr(rec.ExpiresAt), timePtr(rec.CleanupRequestedAt), timePtr(rec.DeletedAt)
	if err := json.Unmarshal(objects, &rec.Objects); err != nil {
		return delivery.Deployment{}, err
	}
	if err := json.Unmarshal(resources, &rec.Resources); err != nil {
		return delivery.Deployment{}, err
	}
	return delivery.RestoreDeployment(rec)
}

func objectsJSON(objs []delivery.ObjectRef) (string, error) {
	if objs == nil {
		objs = []delivery.ObjectRef{}
	}
	b, err := json.Marshal(objs)
	return string(b), err
}

func resourcesJSON(res []delivery.ResourceRef) (string, error) {
	if res == nil {
		res = []delivery.ResourceRef{}
	}
	b, err := json.Marshal(res)
	return string(b), err
}

// Register implements delivery.Store. In one tenant transaction: the runtime
// session check (when the mutation is a workload's), then an insert that
// yields to the logical action's existing deployment. The first caller creates
// the record, its history, audit entry and event; any other caller (a
// concurrent duplicate, a retry attempt, a replay after a lost acknowledgment)
// observes the original and is audited separately. A changed immutable input
// is ErrConflict.
func (s *Store) Register(ctx context.Context, cmd delivery.RegisterCommand) (out delivery.RegisterResult, err error) {
	if err := checkRegister(cmd); err != nil {
		return delivery.RegisterResult{}, err
	}
	rec := cmd.Deployment.Record()
	err = s.tx(ctx, rec.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if e := s.guardRuntime(ctx, tx, rec.Scope, cmd.Mutation); e != nil {
			return e
		}
		var e error
		out, e = s.registerTx(ctx, tx, cmd)
		return e
	})
	if err != nil {
		return delivery.RegisterResult{}, err
	}
	return out, nil
}

func checkRegister(cmd delivery.RegisterCommand) error {
	if err := cmd.Mutation.Validate(); err != nil {
		return err
	}
	rec := cmd.Deployment.Record()
	if rec.Status != delivery.StatusPending || rec.Version != 1 {
		return delivery.Fail(delivery.ErrInvalid, nil, "only a new pending deployment is registered")
	}
	return nil
}

// registerTx inserts the deployment inside the caller's tenant transaction, or
// observes the logical action's existing one.
func (s *Store) registerTx(ctx context.Context, tx pgx.Tx, cmd delivery.RegisterCommand) (delivery.RegisterResult, error) {
	m := cmd.Mutation
	rec := cmd.Deployment.Record()
	objs, err := objectsJSON(rec.Objects)
	if err != nil {
		return delivery.RegisterResult{}, err
	}
	res, err := resourcesJSON(rec.Resources)
	if err != nil {
		return delivery.RegisterResult{}, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO deployments (`+deploymentColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24::jsonb, $25::jsonb, $26, NULL, '', '', NULL, $27, $28, $29)
ON CONFLICT DO NOTHING`,
		string(rec.Scope.TenantID), string(rec.Scope.ProjectID), string(rec.ID), string(rec.Environment), string(rec.ConfigDigest), rec.Namespace,
		rec.Source.RepositoryURL, rec.Source.Commit, string(rec.Artifact.ArtifactID), string(rec.Artifact.Digest), string(rec.Artifact.Kind), rec.Artifact.MediaType,
		string(rec.Producer.Attempt), string(rec.Producer.Run), rec.Producer.Step, rec.Producer.ToolImage, string(rec.Run), rec.Step, rec.ActionKey, string(rec.Attempt),
		string(rec.Fingerprint), string(rec.Status), rec.Reason, objs, res, rec.ExpiresAt, int64(rec.Version), rec.CreatedAt, rec.UpdatedAt)
	if err != nil {
		return delivery.RegisterResult{}, err
	}
	if tag.RowsAffected() == 1 {
		if e := s.history(ctx, tx, m, rec.Scope, rec.ID, delivery.Transition{Kind: "registered", Attempt: cmd.Attempt,
			Change: delivery.Change{ToStatus: rec.Status, ToReason: rec.Reason, Changed: true}}, rec.Version, "none"); e != nil {
			return delivery.RegisterResult{}, e
		}
		if e := s.audit(ctx, tx, m, auditEntry{Scope: rec.Scope, Action: "deployment.register", ResourceType: "deployment", ResourceID: string(rec.ID),
			ResourceVersion: int64(rec.Version), Metadata: map[string]any{"environmentId": string(rec.Environment), "attemptId": string(cmd.Attempt), "runId": string(rec.Run)}}); e != nil {
			return delivery.RegisterResult{}, e
		}
		// Appended last: a full backlog fails here and rolls everything back.
		ev, e := stateChanged(m, cmd.Deployment, "none")
		if e != nil {
			return delivery.RegisterResult{}, e
		}
		if e := s.appender(ctx, tx, ev); e != nil {
			return delivery.RegisterResult{}, e
		}
		return delivery.RegisterResult{Deployment: cmd.Deployment, Created: true}, nil
	}
	existing, err := scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments
WHERE tenant_id = $1 AND project_id = $2 AND action_key = $3 FOR SHARE`, string(rec.Scope.TenantID), string(rec.Scope.ProjectID), rec.ActionKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return delivery.RegisterResult{}, delivery.Fail(delivery.ErrUnavailable, err, "the deployment could not be read back")
	}
	if err != nil {
		return delivery.RegisterResult{}, err
	}
	if existing.Record().Fingerprint != rec.Fingerprint {
		return delivery.RegisterResult{}, delivery.Fail(delivery.ErrConflict, nil, "the action key belongs to a different deployment request")
	}
	if err := s.audit(ctx, tx, m, auditEntry{Scope: rec.Scope, Action: "deployment.observe", ResourceType: "deployment", ResourceID: string(existing.ID()),
		ResourceVersion: int64(existing.Version()), Metadata: map[string]any{"attemptId": string(cmd.Attempt), "originAttemptId": string(existing.Record().Attempt)}}); err != nil {
		return delivery.RegisterResult{}, err
	}
	return delivery.RegisterResult{Deployment: existing, Created: false}, nil
}

// GetDeployment implements delivery.Store.
func (s *Store) GetDeployment(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (d delivery.Deployment, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		d, e = scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if errors.Is(e, pgx.ErrNoRows) {
			return delivery.Fail(delivery.ErrNotFound, e, "not found")
		}
		return e
	})
	return d, err
}

// FindByAction implements delivery.Store.
func (s *Store) FindByAction(ctx context.Context, scope domain.Scope, actionKey string) (d delivery.Deployment, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		d, e = scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND action_key = $3`,
			string(scope.TenantID), string(scope.ProjectID), actionKey))
		if errors.Is(e, pgx.ErrNoRows) {
			return delivery.Fail(delivery.ErrNotFound, e, "not found")
		}
		return e
	})
	return d, err
}

// ListDeployments implements delivery.Store.
func (s *Store) ListDeployments(ctx context.Context, scope domain.Scope, f delivery.DeploymentFilter, afterID delivery.DeploymentID, limit int) (out []delivery.Deployment, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT `+deploymentColumns+` FROM deployments
WHERE tenant_id = $1 AND project_id = $2 AND id > $3 AND ($4 = '' OR environment_id = $4) AND ($5 = '' OR status = $5)
ORDER BY id LIMIT $6`, string(scope.TenantID), string(scope.ProjectID), string(afterID), string(f.Environment), string(f.Status), limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			d, e := scanDeployment(rows)
			if e != nil {
				return e
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// Expired implements delivery.Store.
func (s *Store) Expired(ctx context.Context, scope domain.Scope, now time.Time, limit int) (out []delivery.Deployment, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT `+deploymentColumns+` FROM deployments
WHERE tenant_id = $1 AND project_id = $2 AND expires_at IS NOT NULL AND expires_at <= $3 AND deleted_at IS NULL
ORDER BY expires_at, id LIMIT $4`, string(scope.TenantID), string(scope.ProjectID), now.UTC(), limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			d, e := scanDeployment(rows)
			if e != nil {
				return e
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// Update implements delivery.Store. The transaction order is: the runtime
// session check (execution attempt lock, then grant), the deployment row lock,
// the pure entity transition, the guarded UPDATE, history, audit and finally
// the event. A transition that changed nothing (an idempotent replay) commits
// nothing and returns the stored state: the replayed receipt.
func (s *Store) Update(ctx context.Context, scope domain.Scope, id delivery.DeploymentID, m delivery.Mutation, fn func(*delivery.Deployment) (delivery.Transition, error)) (out delivery.Deployment, tr delivery.Transition, err error) {
	if err := m.Validate(); err != nil {
		return delivery.Deployment{}, delivery.Transition{}, err
	}
	err = s.tx(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if e := s.guardRuntime(ctx, tx, scope, m); e != nil {
			return e
		}
		d, e := scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if errors.Is(e, pgx.ErrNoRows) {
			return delivery.Fail(delivery.ErrNotFound, e, "not found")
		}
		if e != nil {
			return e
		}
		before := d.Record()
		tr, e = fn(&d)
		if e != nil {
			return e
		}
		out = d
		if !d.Dirty() {
			return nil
		}
		rec := d.Record()
		objs, e := objectsJSON(rec.Objects)
		if e != nil {
			return e
		}
		res, e := resourcesJSON(rec.Resources)
		if e != nil {
			return e
		}
		tag, e := tx.Exec(ctx, `UPDATE deployments SET status = $4, reason = $5, objects = $6::jsonb, resources = $7::jsonb, cleanup_requested_at = $8, cleanup_state = $9,
cleanup_reason = $10, deleted_at = $11, version = $12, updated_at = $13
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $14`,
			string(scope.TenantID), string(scope.ProjectID), string(id), string(rec.Status), rec.Reason, objs, res, rec.CleanupRequestedAt, string(rec.CleanupState),
			rec.CleanupReason, rec.DeletedAt, int64(rec.Version), rec.UpdatedAt, int64(before.Version))
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return delivery.Fail(delivery.ErrUnavailable, nil, "the deployment changed during the update")
		}
		if e := s.history(ctx, tx, m, scope, id, tr, rec.Version, string(before.Status)); e != nil {
			return e
		}
		if e := s.audit(ctx, tx, m, auditEntry{Scope: scope, Action: "deployment." + tr.Kind, ResourceType: "deployment", ResourceID: string(id),
			ResourceVersion: int64(rec.Version), Metadata: map[string]any{"from": string(before.Status), "to": string(rec.Status), "reason": rec.Reason, "attemptId": string(tr.Attempt)}}); e != nil {
			return e
		}
		if tr.Change.StatusChanged() {
			ev, e := stateChanged(m, d, string(before.Status))
			if e != nil {
				return e
			}
			return s.appender(ctx, tx, ev)
		}
		return nil
	})
	if err != nil {
		return delivery.Deployment{}, delivery.Transition{}, err
	}
	out.MarkClean()
	return out, tr, nil
}

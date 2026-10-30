package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
)

const deploymentColumnsBase = `tenant_id, project_id, id, environment_id, config_digest, namespace, source_repository_url, source_commit,
image_artifact_id, image_digest, producer_attempt_id, producer_run_id, producer_step_id, tool_image, run_id, step_id, action_key, attempt_id,
request_fingerprint, status, reason, objects, expires_at, cleanup_requested_at, cleanup_state, cleanup_reason, deleted_at, version, created_at, updated_at`

// columns lists the deployment columns scanDeployment reads: the source-set pair
// is selected only when the schema has it, NULL otherwise.
func (s *Store) columns() string {
	if s.sets {
		return deploymentColumnsBase + `, source_set_digest, source_commits`
	}
	return deploymentColumnsBase + `, NULL::text, NULL::jsonb`
}

// sourceCommitJSON is the stored form of one source-set commit.
type sourceCommitJSON struct {
	RepositoryID string `json:"repositoryId"`
	Commit       string `json:"commit"`
}

// encodeSourceSet returns the stored digest and commits of a source set, both
// nil for a deployment of a single source.
func encodeSourceSet(set delivery.SourceSet) (*string, []byte, error) {
	if set.IsZero() {
		return nil, nil, nil
	}
	commits := set.Commits()
	out := make([]sourceCommitJSON, len(commits))
	for i, c := range commits {
		out[i] = sourceCommitJSON{RepositoryID: c.RepositoryID, Commit: c.Commit}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	d := set.Digest
	return &d, raw, nil
}

// scanSource rebuilds the deployment's source: a repository and commit, and, for a
// source set, the set (the repository and commit are then its primary source's).
// A row that has no repository and commit, or half a set, is invalid, never usable.
func scanSource(url, commit, digest *string, commits []byte) (delivery.Source, error) {
	if url == nil || commit == nil || (digest == nil) != (commits == nil) {
		return delivery.Source{}, errors.New("a deployment source is a repository and commit, with its source set when it has one")
	}
	if digest == nil {
		return delivery.Source{RepositoryURL: *url, Commit: *commit}, nil
	}
	var stored []sourceCommitJSON
	dec := json.NewDecoder(bytes.NewReader(commits))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&stored); err != nil {
		return delivery.Source{}, err
	}
	list := make([]delivery.SourceCommit, len(stored))
	for i, c := range stored {
		list[i] = delivery.SourceCommit{RepositoryID: c.RepositoryID, Commit: c.Commit}
	}
	set, err := delivery.NewSourceSet(*digest, list)
	if err != nil {
		return delivery.Source{}, err
	}
	return delivery.Source{RepositoryURL: *url, Commit: *commit, SourceSet: set}, nil
}

func scanDeployment(row pgx.Row) (delivery.Deployment, error) {
	var rec delivery.DeploymentRecord
	var tenant, project, id, env, digest, imageArtifact, imageDigest, prodAttempt, prodRun, run, attempt, fp, status, cleanup string
	var objects, setCommits []byte
	var srcURL, srcCommit, setDigest *string
	var version int64
	err := row.Scan(&tenant, &project, &id, &env, &digest, &rec.Namespace, &srcURL, &srcCommit,
		&imageArtifact, &imageDigest, &prodAttempt, &prodRun, &rec.Producer.Step, &rec.Producer.ToolImage, &run, &rec.Step, &rec.ActionKey, &attempt,
		&fp, &status, &rec.Reason, &objects, &rec.ExpiresAt, &rec.CleanupRequestedAt, &cleanup, &rec.CleanupReason, &rec.DeletedAt, &version, &rec.CreatedAt, &rec.UpdatedAt, &setDigest, &setCommits)
	if err != nil {
		return delivery.Deployment{}, err
	}
	if rec.Source, err = scanSource(srcURL, srcCommit, setDigest, setCommits); err != nil {
		return delivery.Deployment{}, delivery.Fail(delivery.ErrUnavailable, err, "a stored deployment source is invalid")
	}
	rec.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	rec.ID, rec.Environment, rec.ConfigDigest = delivery.DeploymentID(id), domain.EnvironmentID(env), runtimev1.Digest(digest)
	rec.Image = delivery.Image{ArtifactID: domain.ArtifactID(imageArtifact), Digest: runtimev1.Digest(imageDigest)}
	rec.Producer.Attempt, rec.Producer.Run = domain.AttemptID(prodAttempt), domain.RunID(prodRun)
	rec.Run, rec.Attempt, rec.Fingerprint = domain.RunID(run), domain.AttemptID(attempt), runtimev1.Digest(fp)
	rec.Status, rec.CleanupState, rec.Version = delivery.Status(status), delivery.CleanupState(cleanup), domain.Version(version)
	rec.CreatedAt, rec.UpdatedAt = utc(rec.CreatedAt), utc(rec.UpdatedAt)
	rec.ExpiresAt, rec.CleanupRequestedAt, rec.DeletedAt = timePtr(rec.ExpiresAt), timePtr(rec.CleanupRequestedAt), timePtr(rec.DeletedAt)
	if err := json.Unmarshal(objects, &rec.Objects); err != nil {
		return delivery.Deployment{}, err
	}
	return delivery.RestoreDeployment(rec)
}

// nullableJSON passes a NULL jsonb for no content.
func nullableJSON(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

func objectsJSON(objs []delivery.ObjectRef) (string, error) {
	if objs == nil {
		objs = []delivery.ObjectRef{}
	}
	b, err := json.Marshal(objs)
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
	setDigest, setCommits, err := encodeSourceSet(rec.Source.SourceSet)
	if err != nil {
		return delivery.RegisterResult{}, err
	}
	srcURL, srcCommit := rec.Source.RepositoryURL, rec.Source.Commit
	cols, vals := deploymentColumnsBase, `$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22::jsonb, $23, NULL, '', '', NULL, $24, $25, $26`
	args := []any{
		string(rec.Scope.TenantID), string(rec.Scope.ProjectID), string(rec.ID), string(rec.Environment), string(rec.ConfigDigest), rec.Namespace,
		srcURL, srcCommit, string(rec.Image.ArtifactID), string(rec.Image.Digest), string(rec.Producer.Attempt),
		string(rec.Producer.Run), rec.Producer.Step, rec.Producer.ToolImage, string(rec.Run), rec.Step, rec.ActionKey, string(rec.Attempt),
		string(rec.Fingerprint), string(rec.Status), rec.Reason, objs, rec.ExpiresAt, int64(rec.Version), rec.CreatedAt, rec.UpdatedAt,
	}
	switch {
	case s.sets:
		cols, vals = cols+`, source_set_digest, source_commits`, vals+`, $27, $28::jsonb`
		args = append(args, setDigest, nullableJSON(setCommits))
	case setDigest != nil:
		return delivery.RegisterResult{}, delivery.Fail(delivery.ErrUnavailable, nil, "source set provenance needs the source provenance migration")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO deployments (`+cols+`)
VALUES (`+vals+`)
ON CONFLICT DO NOTHING`, args...)
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
	existing, err := scanDeployment(tx.QueryRow(ctx, `SELECT `+s.columns()+` FROM deployments
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
		d, e = scanDeployment(tx.QueryRow(ctx, `SELECT `+s.columns()+` FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
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
		d, e = scanDeployment(tx.QueryRow(ctx, `SELECT `+s.columns()+` FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND action_key = $3`,
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
		rows, e := tx.Query(ctx, `SELECT `+s.columns()+` FROM deployments
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
		rows, e := tx.Query(ctx, `SELECT `+s.columns()+` FROM deployments
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
		d, e := scanDeployment(tx.QueryRow(ctx, `SELECT `+s.columns()+` FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
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
		tag, e := tx.Exec(ctx, `UPDATE deployments SET status = $4, reason = $5, objects = $6::jsonb, cleanup_requested_at = $7, cleanup_state = $8,
cleanup_reason = $9, deleted_at = $10, version = $11, updated_at = $12
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $13`,
			string(scope.TenantID), string(scope.ProjectID), string(id), string(rec.Status), rec.Reason, objs, rec.CleanupRequestedAt, string(rec.CleanupState),
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

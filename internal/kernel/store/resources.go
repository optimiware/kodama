package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ---- pipelines --------------------------------------------------------------

type Pipeline struct {
	TenantID   domain.TenantID  `json:"tenantId"`
	ProjectID  domain.ProjectID `json:"projectId"`
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Source     string           `json:"source"`
	Definition json.RawMessage  `json:"definition"`
	Version    domain.Version   `json:"version"`
	Disabled   bool             `json:"disabled"`
	CreatedAt  time.Time        `json:"createdAt"`
	UpdatedAt  time.Time        `json:"updatedAt"`
	// RepositoryID and SourceCommit are set for a pipeline indexed from a
	// repository (source git): the repository and the exact commit read.
	RepositoryID string `json:"repositoryId,omitempty"`
	SourceCommit string `json:"sourceCommit,omitempty"`
}

// PutPipeline registers or replaces an API-managed definition by name.
func (s *Store) PutPipeline(ctx context.Context, p Pipeline) (Pipeline, error) {
	if p.TenantID.Validate() != nil || p.ProjectID.Validate() != nil || p.Name == "" {
		return Pipeline{}, fmt.Errorf("%w: pipeline", domain.ErrInvalidValue)
	}
	err := s.inTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO pipelines (tenant_id,project_id,id,name,definition,source)
			VALUES ($1,$2,$3,$4,$5,'api')
			ON CONFLICT (tenant_id,project_id,repo_id,name) DO UPDATE
			SET definition=excluded.definition, source='api', disabled_at=NULL,
			    version=pipelines.version+1, updated_at=now()
			RETURNING id,source,definition::jsonb,version,disabled_at IS NOT NULL,created_at,updated_at`,
			p.TenantID, p.ProjectID, p.ID, p.Name, p.Definition).
			Scan(&p.ID, &p.Source, &p.Definition, &p.Version, &p.Disabled, &p.CreatedAt, &p.UpdatedAt)
	})
	return p, translate(err)
}

// GetPipelineByName resolves an enabled or disabled pipeline of a project.
func (s *Store) GetPipelineByName(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, name string) (Pipeline, error) {
	p := Pipeline{TenantID: tenant, ProjectID: project, Name: name}
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id,source,definition::jsonb,version,disabled_at IS NOT NULL,created_at,updated_at,
			coalesce(repo_id,''),coalesce(source_commit,'')
			FROM pipelines WHERE tenant_id=$1 AND project_id=$2 AND name=$3
			ORDER BY repo_id NULLS FIRST LIMIT 1`, tenant, project, name).
			Scan(&p.ID, &p.Source, &p.Definition, &p.Version, &p.Disabled, &p.CreatedAt, &p.UpdatedAt, &p.RepositoryID, &p.SourceCommit)
	})
	return p, translate(err)
}

func (s *Store) ListPipelines(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]Pipeline, error) {
	var out []Pipeline
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,name,source,definition::jsonb,version,disabled_at IS NOT NULL,created_at,updated_at,
			coalesce(repo_id,''),coalesce(source_commit,'')
			FROM pipelines WHERE tenant_id=$1 AND project_id=$2 ORDER BY name,id`, tenant, project)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p := Pipeline{TenantID: tenant, ProjectID: project}
			if err := rows.Scan(&p.ID, &p.Name, &p.Source, &p.Definition, &p.Version, &p.Disabled, &p.CreatedAt, &p.UpdatedAt, &p.RepositoryID, &p.SourceCommit); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, translate(err)
}

// ---- runs and steps ---------------------------------------------------------

type Step struct {
	ID         string          `json:"id"`
	StepKey    string          `json:"stepKey"`
	Ordinal    int             `json:"ordinal"`
	Image      string          `json:"image"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason,omitempty"`
	ExitCode   *int            `json:"exitCode,omitempty"`
	LogURI     string          `json:"logUri,omitempty"`
	Outputs    json.RawMessage `json:"outputs,omitempty"`
	StartedAt  *time.Time      `json:"startedAt,omitempty"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
}

type Run struct {
	TenantID       domain.TenantID  `json:"tenantId"`
	ProjectID      domain.ProjectID `json:"projectId"`
	ID             string           `json:"id"`
	PipelineID     string           `json:"pipelineId"`
	PipelineName   string           `json:"pipelineName"`
	TaskID         string           `json:"taskId,omitempty"`
	Status         string           `json:"status"`
	Reason         string           `json:"reason,omitempty"`
	Backend        string           `json:"backend,omitempty"`
	ExecutionOrder []string         `json:"executionOrder"`
	Inputs         json.RawMessage  `json:"inputs"`
	Definition     json.RawMessage  `json:"definition"`
	// Sources are the run's pinned sources (runs.sources), in the shape of
	// gitv1.SourceSet.Sources; empty for a run without a repository.
	Sources []gitv1.Source `json:"sources,omitempty"`
	// TenantKey and ProjectKey address the run's repositories under /git/; they
	// are read with the run and never serialized.
	TenantKey         string         `json:"-"`
	ProjectKey        string         `json:"-"`
	CancelRequestedAt *time.Time     `json:"cancelRequestedAt,omitempty"`
	CreatedAt         time.Time      `json:"createdAt"`
	StartedAt         *time.Time     `json:"startedAt,omitempty"`
	FinishedAt        *time.Time     `json:"finishedAt,omitempty"`
	Version           domain.Version `json:"version"`
	Steps             []Step         `json:"steps,omitempty"`
}

// decodeSources reads runs.sources. A malformed or invalid pinned set is an
// error: a run never starts from sources it cannot trust.
func decodeSources(raw []byte) ([]gitv1.Source, error) {
	if len(raw) == 0 || string(raw) == "[]" || string(raw) == "null" {
		return nil, nil
	}
	var out []gitv1.Source
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if err := (gitv1.SourceSet{Sources: out}).Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// AdmitRun inserts the run and one row per step, with the immutable definition
// snapshot and resolved order, in one transaction.
func (s *Store) AdmitRun(ctx context.Context, r Run, steps []Step, triggerKey, requestKey, principal, requestHash string) (Run, error) {
	err := s.inTenant(ctx, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO runs
			(tenant_id,project_id,id,pipeline_id,task_id,definition_snapshot,inputs,trigger_key,
			 request_key,request_principal,request_hash,execution_order)
			VALUES ($1,$2,$3,$4,nullif($5,''),$6,$7,nullif($8,''),nullif($9,''),nullif($10,''),nullif($11,''),$12)
			RETURNING version,created_at`,
			r.TenantID, r.ProjectID, r.ID, r.PipelineID, r.TaskID, r.Definition, r.Inputs,
			triggerKey, requestKey, principal, requestHash, r.ExecutionOrder).
			Scan(&r.Version, &r.CreatedAt); err != nil {
			return err
		}
		for _, st := range steps {
			if _, err := tx.Exec(ctx, `INSERT INTO run_steps
				(tenant_id,project_id,run_id,id,step_key,ordinal,image) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				r.TenantID, r.ProjectID, r.ID, st.ID, st.StepKey, st.Ordinal, st.Image); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Run{}, translate(err)
	}
	return s.GetRun(ctx, r.TenantID, r.ProjectID, r.ID)
}

func (s *Store) GetRun(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id string) (Run, error) {
	r := Run{TenantID: tenant, ProjectID: project, ID: id}
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var sources []byte
		if err := tx.QueryRow(ctx, `SELECT r.pipeline_id,p.name,coalesce(r.task_id,''),r.status,coalesce(r.reason,''),
			coalesce(r.backend,''),r.execution_order,r.inputs::jsonb,r.definition_snapshot::jsonb,r.sources::jsonb,
			t.key,pr.key,r.cancel_requested_at,r.created_at,r.started_at,r.finished_at,r.version
			FROM runs r JOIN pipelines p ON p.tenant_id=r.tenant_id AND p.project_id=r.project_id AND p.id=r.pipeline_id
			JOIN tenants t ON t.id=r.tenant_id
			JOIN projects pr ON pr.tenant_id=r.tenant_id AND pr.id=r.project_id
			WHERE r.tenant_id=$1 AND r.project_id=$2 AND r.id=$3`, tenant, project, id).
			Scan(&r.PipelineID, &r.PipelineName, &r.TaskID, &r.Status, &r.Reason, &r.Backend, &r.ExecutionOrder,
				&r.Inputs, &r.Definition, &sources, &r.TenantKey, &r.ProjectKey, &r.CancelRequestedAt, &r.CreatedAt,
				&r.StartedAt, &r.FinishedAt, &r.Version); err != nil {
			return err
		}
		pinned, err := decodeSources(sources)
		if err != nil {
			return fmt.Errorf("run %s: sources: %w", id, err)
		}
		r.Sources = pinned
		rows, err := tx.Query(ctx, `SELECT id,step_key,ordinal,image,status,coalesce(reason,''),exit_code,coalesce(log_uri,''),
			outputs::jsonb,started_at,finished_at FROM run_steps
			WHERE tenant_id=$1 AND project_id=$2 AND run_id=$3 ORDER BY ordinal`, tenant, project, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st Step
			if err := rows.Scan(&st.ID, &st.StepKey, &st.Ordinal, &st.Image, &st.Status, &st.Reason, &st.ExitCode,
				&st.LogURI, &st.Outputs, &st.StartedAt, &st.FinishedAt); err != nil {
				return err
			}
			r.Steps = append(r.Steps, st)
		}
		return rows.Err()
	})
	return r, translate(err)
}

func (s *Store) ListRuns(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]Run, error) {
	var out []Run
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM runs WHERE tenant_id=$1 AND project_id=$2 ORDER BY created_at DESC,id`, tenant, project)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			r, err := s.GetRun(ctx, tenant, project, id)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, translate(err)
}

// ClaimRun atomically moves one queued run to running for the given backend.
func (s *Store) ClaimRun(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id, backend string) (Run, error) {
	r := Run{TenantID: tenant, ProjectID: project, ID: id}
	var won bool
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE runs SET status='running', backend=$4, started_at=now(), version=version+1
			WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND status='queued'
			RETURNING version`, tenant, project, id, backend).Scan(&r.Version)
		if err == pgx.ErrNoRows {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		won = true
		return nil
	})
	if err != nil {
		return Run{}, translate(err)
	}
	if !won {
		return Run{}, ErrConflict
	}
	return s.GetRun(ctx, tenant, project, id)
}

// QueuedRun is the scheduler's candidate.
type QueuedRun struct {
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
	RunID     string
}

func (s *Store) QueuedRuns(ctx context.Context, tenant domain.TenantID) ([]QueuedRun, error) {
	var out []QueuedRun
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT project_id,id FROM runs WHERE tenant_id=$1 AND status='queued'
			AND cancel_requested_at IS NULL ORDER BY created_at,id LIMIT 16`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			q := QueuedRun{TenantID: tenant}
			if err := rows.Scan(&q.ProjectID, &q.RunID); err != nil {
				return err
			}
			out = append(out, q)
		}
		return rows.Err()
	})
	return out, translate(err)
}

// RunningRuns lists claimed runs for restart adoption. A cancelled run is still
// listed: its runner must stop the workload and finish it cancelled.
func (s *Store) RunningRuns(ctx context.Context, tenant domain.TenantID) ([]QueuedRun, error) {
	var out []QueuedRun
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT project_id,id FROM runs WHERE tenant_id=$1 AND status='running'
			ORDER BY started_at,id LIMIT 64`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			q := QueuedRun{TenantID: tenant}
			if err := rows.Scan(&q.ProjectID, &q.RunID); err != nil {
				return err
			}
			out = append(out, q)
		}
		return rows.Err()
	})
	return out, translate(err)
}

func (s *Store) StartStep(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, stepID string) error {
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_steps SET status='running', started_at=now(), begin_at=now(),
			attempts=attempts+1, version=version+1
			WHERE tenant_id=$1 AND project_id=$2 AND run_id=$3 AND id=$4 AND status='pending'`,
			tenant, project, runID, stepID)
		return err
	}))
}

func (s *Store) FinishStep(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, stepID, status, reason string, exitCode int, logURI string) error {
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_steps SET status=$5, reason=nullif($6,''), exit_code=$7,
			log_uri=nullif($8,''), finished_at=now(), version=version+1
			WHERE tenant_id=$1 AND project_id=$2 AND run_id=$3 AND id=$4`,
			tenant, project, runID, stepID, status, reason, exitCode, logURI)
		return err
	}))
}

func (s *Store) FinishRun(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, status, reason string) error {
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runs SET status=$4, reason=nullif($5,''), finished_at=now(), version=version+1
			WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND status IN ('queued','running')`,
			tenant, project, runID, status, reason)
		if err != nil {
			return err
		}
		// A run's Git credentials end with it (GR-06); liveness is re-read at
		// every use as well, so this only closes the window early.
		return revokeRunCredentialsTx(ctx, tx, tenant, project, runID)
	}))
}

func (s *Store) RequestCancel(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (Run, error) {
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runs SET
			cancel_requested_at=coalesce(cancel_requested_at,now()),
			status=CASE WHEN status='queued' THEN 'cancelled' ELSE status END,
			finished_at=CASE WHEN status='queued' THEN now() ELSE finished_at END,
			version=version+1
			WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND status IN ('queued','running')`, tenant, project, runID)
		return err
	})
	if err != nil {
		return Run{}, translate(err)
	}
	return s.GetRun(ctx, tenant, project, runID)
}

// ---- secrets ----------------------------------------------------------------

type Secret struct {
	ID        string           `json:"id"`
	ProjectID domain.ProjectID `json:"projectId,omitempty"`
	Name      string           `json:"name"`
	Version   domain.Version   `json:"version"`
	RevokedAt *time.Time       `json:"revokedAt,omitempty"`
}

func (s *Store) PutSecret(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id, name, keyID string, version int64, ciphertext, envelope []byte) (Secret, error) {
	sec := Secret{ID: id, ProjectID: project, Name: name}
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO secrets (tenant_id,project_id,id,name,encrypted_value,key_id,envelope,version)
			VALUES ($1,nullif($2,''),$3,$4,$5,$6,$7::jsonb,$8)
			ON CONFLICT (tenant_id,project_id,name) DO UPDATE SET
			 encrypted_value=excluded.encrypted_value, key_id=excluded.key_id, envelope=excluded.envelope,
			 version=excluded.version, revoked_at=NULL, updated_at=now()
			RETURNING id,project_id,version,revoked_at`,
			tenant, project, id, name, ciphertext, keyID, string(envelope), version).
			Scan(&sec.ID, &sec.ProjectID, &sec.Version, &sec.RevokedAt)
	})
	return sec, translate(err)
}

// SecretVersion returns the stored version for a name, or 0 when absent.
func (s *Store) SecretVersion(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, name string) (int64, error) {
	var v int64
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT version FROM secrets WHERE tenant_id=$1 AND name=$2
			AND (project_id IS NULL OR project_id=$3) ORDER BY project_id NULLS LAST LIMIT 1`,
			tenant, name, project).Scan(&v)
	})
	if err != nil {
		return 0, err
	}
	return v, nil
}

func (s *Store) ListSecrets(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]Secret, error) {
	var out []Secret
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,coalesce(project_id,''),name,version,revoked_at FROM secrets
			WHERE tenant_id=$1 AND (project_id IS NULL OR project_id=$2) ORDER BY name,id`, tenant, project)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sec Secret
			if err := rows.Scan(&sec.ID, &sec.ProjectID, &sec.Name, &sec.Version, &sec.RevokedAt); err != nil {
				return err
			}
			out = append(out, sec)
		}
		return rows.Err()
	})
	return out, translate(err)
}

// ResolveSecret returns the ciphertext and envelope for a declared secret name,
// preferring the project binding over the tenant-shared one.
type Resolved struct {
	ID         string           `json:"id"`
	ProjectID  domain.ProjectID `json:"projectId,omitempty"`
	Name       string           `json:"name"`
	Version    int64            `json:"version"`
	KeyID      string           `json:"keyId"`
	Ciphertext []byte           `json:"-"`
	Envelope   []byte           `json:"-"`
}

func (s *Store) ResolveSecret(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, name string) (Resolved, error) {
	var r Resolved
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id,coalesce(project_id,''),name,version,key_id,encrypted_value,envelope::jsonb
			FROM secrets WHERE tenant_id=$1 AND name=$2 AND (project_id IS NULL OR project_id=$3) AND revoked_at IS NULL
			ORDER BY project_id NULLS LAST LIMIT 1`, tenant, name, project).
			Scan(&r.ID, &r.ProjectID, &r.Name, &r.Version, &r.KeyID, &r.Ciphertext, &r.Envelope)
	})
	return r, translate(err)
}

func (s *Store) ResolveSecretByID(ctx context.Context, tenant domain.TenantID, id string) (Resolved, error) {
	var r Resolved
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id,coalesce(project_id,''),name,version,key_id,encrypted_value,envelope::jsonb
			FROM secrets WHERE tenant_id=$1 AND id=$2 AND revoked_at IS NULL`, tenant, id).
			Scan(&r.ID, &r.ProjectID, &r.Name, &r.Version, &r.KeyID, &r.Ciphertext, &r.Envelope)
	})
	return r, translate(err)
}

func (s *Store) RevokeSecret(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, name string) error {
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE secrets SET revoked_at=now(), version=version+1
			WHERE tenant_id=$1 AND name=$2 AND (project_id IS NULL OR project_id=$3)`, tenant, name, project)
		return err
	}))
}

// ---- artifacts --------------------------------------------------------------

type Artifact struct {
	ID        string           `json:"id"`
	TenantID  domain.TenantID  `json:"tenantId"`
	ProjectID domain.ProjectID `json:"projectId"`
	RunID     string           `json:"runId"`
	StepID    string           `json:"stepId,omitempty"`
	TaskID    string           `json:"taskId,omitempty"`
	Name      string           `json:"name"`
	Kind      string           `json:"kind"`
	URI       string           `json:"uri"`
	Digest    string           `json:"digest,omitempty"`
	Metadata  json.RawMessage  `json:"metadata,omitempty"`
	CreatedAt time.Time        `json:"createdAt"`
}

func (s *Store) RegisterArtifact(ctx context.Context, a Artifact) (Artifact, error) {
	err := s.inTenant(ctx, a.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO artifacts
			(tenant_id,project_id,id,run_id,step_id,task_id,name,kind,uri,digest,metadata)
			VALUES ($1,$2,$3,$4,nullif($5,''),nullif($6,''),$7,$8,$9,nullif($10,''),$11)
			RETURNING created_at`,
			a.TenantID, a.ProjectID, a.ID, a.RunID, a.StepID, a.TaskID, a.Name, a.Kind, a.URI, a.Digest, a.Metadata).
			Scan(&a.CreatedAt)
	})
	return a, translate(err)
}

func (s *Store) ListArtifacts(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]Artifact, error) {
	var out []Artifact
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,run_id,coalesce(step_id,''),coalesce(task_id,''),name,kind,uri,
			coalesce(digest,''),metadata::jsonb,created_at FROM artifacts
			WHERE tenant_id=$1 AND project_id=$2 ORDER BY created_at DESC,id`, tenant, project)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a := Artifact{TenantID: tenant, ProjectID: project}
			if err := rows.Scan(&a.ID, &a.RunID, &a.StepID, &a.TaskID, &a.Name, &a.Kind, &a.URI, &a.Digest, &a.Metadata, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, translate(err)
}

func (s *Store) AppendEvent(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, actorType, actorID, eventType, resourceType, resourceID string, payload []byte) error {
	id, err := newID("evt")
	if err != nil {
		return err
	}
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit_events
			(tenant_id,project_id,id,actor_type,actor_id,event_type,resource_type,resource_id,payload)
			VALUES ($1,nullif($2,''),$3,$4,$5,$6,$7,$8,$9::jsonb)`,
			tenant, project, id, actorType, actorID, eventType, resourceType, resourceID, payload)
		return err
	}))
}

// ---- task reads -------------------------------------------------------------

type TaskItem struct {
	ID           string           `json:"id"`
	ProjectID    domain.ProjectID `json:"projectId"`
	Kind         string           `json:"kind"`
	Title        string           `json:"title"`
	Body         string           `json:"body"`
	State        string           `json:"state"`
	ParkedAt     *time.Time       `json:"parkedAt,omitempty"`
	BlockedBy    string           `json:"blockedByTaskId,omitempty"`
	ResumeState  string           `json:"resumeState,omitempty"`
	AssignedRole string           `json:"assignedRole,omitempty"`
	Context      json.RawMessage  `json:"context"`
	Resolution   json.RawMessage  `json:"resolution,omitempty"`
	Version      domain.Version   `json:"version"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

func (s *Store) ListTasks(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, state, role string) ([]TaskItem, error) {
	var out []TaskItem
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,kind,title,body,state,parked_at,coalesce(blocked_by_id,''),
			coalesce(resume_state,''),coalesce(assigned_role,''),context::jsonb,resolution::jsonb,version,created_at,updated_at
			FROM tasks WHERE tenant_id=$1 AND project_id=$2
			AND ($3='' OR state=$3) AND ($4='' OR assigned_role=$4)
			ORDER BY created_at,id`, tenant, project, state, role)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t := TaskItem{ProjectID: project}
			if err := rows.Scan(&t.ID, &t.Kind, &t.Title, &t.Body, &t.State, &t.ParkedAt, &t.BlockedBy,
				&t.ResumeState, &t.AssignedRole, &t.Context, &t.Resolution, &t.Version, &t.CreatedAt, &t.UpdatedAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, translate(err)
}

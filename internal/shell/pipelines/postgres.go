package pipelines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// Event identity of the registry's own outbox rows. The catalog's
// `pipeline.indexed` covers a definition that was accepted or refused at a
// commit; the row ID is derived from the attempt, so the row doubles as the
// idempotency record.
const (
	eventIndexed  = "pipeline.indexed"
	attemptPrefix = "pa_"
	actorID       = "pipeline-registry"
	refusalSource = "source_conflict"
)

// Postgres is the PostgreSQL Store adapter. Every operation is one
// transaction under storage.WithTenant, so row-level security binds it to the
// tenant, and each query also names the project.
type Postgres struct{ db storage.TxBeginner }

var _ Store = (*Postgres)(nil)

// NewPostgres returns the adapter over a pool of the runtime role.
func NewPostgres(db storage.TxBeginner) *Postgres { return &Postgres{db: db} }

// attemptPayload is what an audit row records about one attempt: identifiers
// and codes only, never the document.
type attemptPayload struct {
	Outcome      string `json:"outcome"` // accepted or refused
	Action       string `json:"action,omitempty"`
	RefusalCode  string `json:"refusalCode,omitempty"`
	Name         string `json:"name"`
	PipelineID   string `json:"pipelineId"`
	Source       Source `json:"source"`
	ExistingSrc  Source `json:"existingSource,omitempty"`
	EventID      string `json:"eventId,omitempty"`
	Ref          string `json:"ref,omitempty"`
	Commit       string `json:"commit,omitempty"`
	RepositoryID string `json:"repository,omitempty"`
	PipelineVer  int64  `json:"version,omitempty"`
}

const definitionColumns = `id, name, source, coalesce(repo_id,''), coalesce(source_commit,''), definition::jsonb, version,
	disabled_at IS NOT NULL, created_at, updated_at`

func scanDefinition(row pgx.Row, scope domain.Scope) (Definition, error) {
	d := Definition{Scope: scope}
	var kind string
	if err := row.Scan(&d.ID, &d.Name, &kind, &d.Source.RepositoryID, &d.Commit, &d.Document, &d.Version,
		&d.Disabled, &d.Created, &d.Updated); err != nil {
		return Definition{}, err
	}
	d.Source.Kind = SourceKind(kind)
	return d, nil
}

func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			// The project-unique constraint: the backstop behind the registry's own check.
			return ErrDuplicateName
		case "23503":
			return ErrNotFound
		}
	}
	return err
}

func (p *Postgres) projectTx(ctx context.Context, tx pgx.Tx, scope domain.Scope) error {
	var one int
	return tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE tenant_id=$1 AND id=$2`, scope.TenantID, scope.ProjectID).Scan(&one)
}

func (p *Postgres) Resolve(ctx context.Context, scope domain.Scope, name string) (string, error) {
	var id string
	err := storage.WithTenant(ctx, p.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM pipelines WHERE tenant_id=$1 AND project_id=$2 AND name=$3`,
			scope.TenantID, scope.ProjectID, name).Scan(&id)
	})
	if err != nil {
		return "", translate(err)
	}
	return id, nil
}

func (p *Postgres) Get(ctx context.Context, scope domain.Scope, id string) (Definition, error) {
	var d Definition
	err := storage.WithTenant(ctx, p.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = scanDefinition(tx.QueryRow(ctx, `SELECT `+definitionColumns+` FROM pipelines
			WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, id), scope)
		return err
	})
	if err != nil {
		return Definition{}, translate(err)
	}
	return d, nil
}

func (p *Postgres) Conflicts(ctx context.Context, scope domain.Scope) ([]Conflict, error) {
	var out []Conflict
	err := storage.WithTenant(ctx, p.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := p.projectTx(ctx, tx, scope); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, resource_id, payload::jsonb, created_at FROM audit_events
			WHERE tenant_id=$1 AND project_id=$2 AND event_type=$3 AND payload::jsonb->>'refusalCode'=$4
			ORDER BY created_at, id`, scope.TenantID, scope.ProjectID, eventIndexed, refusalSource)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c := Conflict{Scope: scope}
			var raw []byte
			if err := rows.Scan(&c.ID, &c.PipelineID, &raw, &c.Recorded); err != nil {
				return err
			}
			var pl attemptPayload
			if err := json.Unmarshal(raw, &pl); err != nil {
				return fmt.Errorf("pipelines: conflict record %s: %w", c.ID, err)
			}
			c.Name, c.Existing, c.Attempted, c.EventID, c.Ref, c.Commit = pl.Name, pl.ExistingSrc, pl.Source, pl.EventID, pl.Ref, pl.Commit
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (p *Postgres) Submit(ctx context.Context, sub Submission) (Result, error) {
	var res Result
	var refused error
	err := storage.WithTenant(ctx, p.db, sub.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, refused = Result{}, nil
		scope := sub.Scope
		if err := p.projectTx(ctx, tx, scope); err != nil {
			return err
		}
		if sub.Source.Kind == SourceGit {
			var one int
			if err := tx.QueryRow(ctx, `SELECT 1 FROM repositories WHERE tenant_id=$1 AND project_id=$2 AND id=$3`,
				scope.TenantID, scope.ProjectID, sub.Source.RepositoryID).Scan(&one); err != nil {
				return err
			}
		}
		// Serialize the name: the decision below reads the name's holder and
		// must not interleave with another registration of it.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"pipelines/"+string(scope.TenantID)+"/"+string(scope.ProjectID)+"/"+sub.Name); err != nil {
			return err
		}

		// A recorded attempt is answered from its record.
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT payload::jsonb FROM audit_events WHERE tenant_id=$1 AND id=$2`,
			scope.TenantID, attemptPrefix+sub.AttemptKey).Scan(&raw)
		switch {
		case err == nil:
			var pl attemptPayload
			if err := json.Unmarshal(raw, &pl); err != nil {
				return fmt.Errorf("pipelines: attempt record: %w", err)
			}
			if pl.Outcome == "refused" {
				res, refused = Result{Replayed: true}, &ConflictError{Name: pl.Name, Existing: pl.ExistingSrc, Attempted: pl.Source}
				return nil
			}
			d, err := scanDefinition(tx.QueryRow(ctx, `SELECT `+definitionColumns+` FROM pipelines
				WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.TenantID, scope.ProjectID, pl.PipelineID), scope)
			if err != nil {
				return err
			}
			res = Result{Definition: d, Replayed: true}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		var existing *Definition
		same := false
		row := tx.QueryRow(ctx, `SELECT `+definitionColumns+`, definition = $4::jsonb FROM pipelines
			WHERE tenant_id=$1 AND project_id=$2 AND name=$3`, scope.TenantID, scope.ProjectID, sub.Name, string(sub.Document))
		d := Definition{Scope: scope}
		var kind string
		switch err := row.Scan(&d.ID, &d.Name, &kind, &d.Source.RepositoryID, &d.Commit, &d.Document, &d.Version,
			&d.Disabled, &d.Created, &d.Updated, &same); {
		case err == nil:
			d.Source.Kind = SourceKind(kind)
			existing = &d
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		switch action := decide(existing, sub, same); action {
		case ActionCreate:
			id, err := newID()
			if err != nil {
				return err
			}
			var repo, commit any
			if sub.Source.Kind == SourceGit {
				repo, commit = sub.Source.RepositoryID, sub.Commit
			}
			created, err := scanDefinition(tx.QueryRow(ctx, `INSERT INTO pipelines (tenant_id,project_id,id,repo_id,name,definition,source,source_commit)
				VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8) RETURNING `+definitionColumns,
				scope.TenantID, scope.ProjectID, id, repo, sub.Name, string(sub.Document), string(sub.Source.Kind), commit), scope)
			if err != nil {
				return err
			}
			res = Result{Definition: created, Created: true}
			return p.recordAccepted(ctx, tx, sub, created, "created")
		case ActionUnchanged:
			res = Result{Definition: *existing}
			return p.recordAccepted(ctx, tx, sub, *existing, "unchanged")
		case ActionUpdate:
			updated, err := scanDefinition(tx.QueryRow(ctx, `UPDATE pipelines SET definition=$4::jsonb, source_commit=$5, disabled_at=NULL,
				version=version+1, updated_at=now() WHERE tenant_id=$1 AND project_id=$2 AND id=$3 RETURNING `+definitionColumns,
				scope.TenantID, scope.ProjectID, existing.ID, string(sub.Document), sub.Commit), scope)
			if err != nil {
				return err
			}
			res = Result{Definition: updated, Updated: true}
			return p.recordAccepted(ctx, tx, sub, updated, "updated")
		case ActionDuplicate:
			refused = ErrDuplicateName
			return nil
		default: // ActionConflict: record it and commit the record
			pl := attemptPayload{Outcome: "refused", RefusalCode: refusalSource, Name: sub.Name, PipelineID: existing.ID,
				Source: sub.Source, ExistingSrc: existing.Source, EventID: sub.EventID, Ref: sub.Ref, Commit: sub.Commit}
			if err := p.insertAttempt(ctx, tx, sub, existing.ID, pl); err != nil {
				return err
			}
			refused = &ConflictError{Name: sub.Name, Existing: existing.Source, Attempted: sub.Source}
			return nil
		}
	})
	if err != nil {
		return Result{}, translate(err)
	}
	if refused != nil {
		return res, refused
	}
	return res, nil
}

// recordAccepted records a Git attempt so a replay is answered from it. An API
// registration is not an indexing attempt and is not recorded.
func (p *Postgres) recordAccepted(ctx context.Context, tx pgx.Tx, sub Submission, d Definition, action string) error {
	if sub.Source.Kind != SourceGit {
		return nil
	}
	return p.insertAttempt(ctx, tx, sub, d.ID, attemptPayload{Outcome: "accepted", Action: action, Name: sub.Name,
		PipelineID: d.ID, Source: sub.Source, EventID: sub.EventID, Ref: sub.Ref, Commit: sub.Commit,
		RepositoryID: sub.Source.RepositoryID, PipelineVer: int64(d.Version)})
}

func (p *Postgres) insertAttempt(ctx context.Context, tx pgx.Tx, sub Submission, pipelineID string, pl attemptPayload) error {
	payload, err := json.Marshal(pl)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (tenant_id,id,project_id,actor_type,actor_id,event_type,resource_type,resource_id,payload)
		VALUES ($1,$2,$3,'system',$4,$5,'pipeline',$6,$7::jsonb)`,
		sub.Scope.TenantID, attemptPrefix+sub.AttemptKey, sub.Scope.ProjectID, actorID, eventIndexed, pipelineID, string(payload))
	return err
}

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "pl_" + hex.EncodeToString(b[:]), nil
}

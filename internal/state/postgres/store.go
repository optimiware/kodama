package postgres

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	"github.com/optimiware/kodama/internal/state"
	"github.com/optimiware/kodama/internal/storage"
)

// Config wires the state store to the runtime database role.
type Config struct {
	DB  storage.TxBeginner
	Now func() time.Time
}

// Store is the PostgreSQL implementation of state.Store.
type Store struct {
	db  storage.TxBeginner
	now func() time.Time
}

// New constructs the store.
func New(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("state/postgres: DB is required")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Store{db: cfg.DB, now: now}, nil
}

var _ state.Store = (*Store)(nil)

func (s *Store) Get(ctx context.Context, scope state.Scope, namespace, key string) (statev1.Entry, error) {
	if err := validateTarget(namespace, key); err != nil {
		return statev1.Entry{}, err
	}
	var entry statev1.Entry
	err := s.tx(ctx, scope, true, func(ctx context.Context, tx pgx.Tx) error {
		return scanEntry(tx.QueryRow(ctx, `SELECT namespace, key, value, artifact_references, generation, revision, updated_at, updated_by
			FROM state_entries WHERE tenant_id=$1 AND project_id=$2 AND namespace=$3 AND key=$4`,
			scope.TenantID, scope.ProjectID, namespace, key), &entry)
	})
	return entry, classify(err)
}

func (s *Store) List(ctx context.Context, scope state.Scope, namespace, prefix, cursor string, limit int) ([]statev1.Entry, string, error) {
	if err := state.ValidateNamespace(namespace); err != nil {
		return nil, "", err
	}
	if err := state.ValidatePrefix(prefix); err != nil {
		return nil, "", err
	}
	if cursor != "" {
		if err := statev1.ValidateKey(cursor); err != nil {
			return nil, "", fmt.Errorf("%w: %v", state.ErrInvalid, err)
		}
	}
	if limit <= 0 || limit > statev1.MaxListLimit {
		limit = statev1.DefaultListLimit
	}
	var items []statev1.Entry
	next := ""
	err := s.tx(ctx, scope, true, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT namespace, key, value, artifact_references, generation, revision, updated_at, updated_by
			FROM state_entries
			WHERE tenant_id=$1 AND project_id=$2 AND namespace=$3 AND key LIKE $4 AND key >= $5
			ORDER BY key LIMIT $6`, scope.TenantID, scope.ProjectID, namespace, prefix+"%", cursor, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e statev1.Entry
			if err := scanEntry(rows, &e); err != nil {
				return err
			}
			items = append(items, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(items) > limit {
			next = items[limit].Key
			items = items[:limit]
		}
		return nil
	})
	return items, next, classify(err)
}

func (s *Store) Create(ctx context.Context, scope state.Scope, namespace, key, actor string, req statev1.WriteRequest) (statev1.Entry, error) {
	if err := validateTarget(namespace, key); err != nil {
		return statev1.Entry{}, err
	}
	if err := req.Validate(); err != nil {
		return statev1.Entry{}, fmt.Errorf("%w: %v", state.ErrInvalid, err)
	}
	refs := referencesJSON(req.ArtifactReferences)
	generation := randomGeneration()
	now := s.now().UTC()
	var entry statev1.Entry
	err := s.tx(ctx, scope, false, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO state_entries
			(tenant_id, project_id, namespace, key, value, artifact_references, generation, revision, updated_at, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,1,$8,$9)
			RETURNING namespace, key, value, artifact_references, generation, revision, updated_at, updated_by`,
			scope.TenantID, scope.ProjectID, namespace, key, []byte(req.Value), refs, generation, now, actor)
		return scanEntry(row, &entry)
	})
	return entry, classify(err)
}

func (s *Store) Update(ctx context.Context, scope state.Scope, namespace, key, actor, expected string, req statev1.WriteRequest) (statev1.Entry, error) {
	if err := validateTarget(namespace, key); err != nil {
		return statev1.Entry{}, err
	}
	if err := req.Validate(); err != nil {
		return statev1.Entry{}, fmt.Errorf("%w: %v", state.ErrInvalid, err)
	}
	generation, revision, err := parseETag(expected)
	if err != nil {
		return statev1.Entry{}, fmt.Errorf("%w: %v", state.ErrInvalid, err)
	}
	refs := referencesJSON(req.ArtifactReferences)
	var entry statev1.Entry
	err = s.tx(ctx, scope, false, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `UPDATE state_entries SET
			value=$5, artifact_references=$6, revision=revision+1, updated_at=$7, updated_by=$8
			WHERE tenant_id=$1 AND project_id=$2 AND namespace=$3 AND key=$4
			  AND generation=$9 AND revision=$10
			RETURNING namespace, key, value, artifact_references, generation, revision, updated_at, updated_by`,
			scope.TenantID, scope.ProjectID, namespace, key, []byte(req.Value), refs, s.now().UTC(), actor, generation, revision)
		if err := scanEntry(row, &entry); errors.Is(err, pgx.ErrNoRows) {
			return state.ErrConflict
		} else {
			return err
		}
	})
	return entry, classify(err)
}

func (s *Store) Delete(ctx context.Context, scope state.Scope, namespace, key, expected string) error {
	if err := validateTarget(namespace, key); err != nil {
		return err
	}
	generation, revision, err := parseETag(expected)
	if err != nil {
		return fmt.Errorf("%w: %v", state.ErrInvalid, err)
	}
	err = s.tx(ctx, scope, false, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM state_entries
			WHERE tenant_id=$1 AND project_id=$2 AND namespace=$3 AND key=$4
			  AND generation=$5 AND revision=$6`,
			scope.TenantID, scope.ProjectID, namespace, key, generation, revision)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return state.ErrConflict
		}
		return nil
	})
	return classify(err)
}

func (s *Store) tx(ctx context.Context, scope state.Scope, readOnly bool, fn func(context.Context, pgx.Tx) error) error {
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", state.ErrInvalid, err)
	}
	opts := pgx.TxOptions{}
	if readOnly {
		opts.AccessMode = pgx.ReadOnly
	}
	return storage.WithTenantOptions(ctx, s.db, scope.TenantID, opts, fn)
}

func validateTarget(namespace, key string) error {
	if err := state.ValidateNamespace(namespace); err != nil {
		return err
	}
	if err := statev1.ValidateKey(key); err != nil {
		return fmt.Errorf("%w: %v", state.ErrInvalid, err)
	}
	return nil
}

func referencesJSON(refs []statev1.ArtifactReference) []byte {
	if len(refs) == 0 {
		return []byte("[]")
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		// ArtifactReference contains only strings; a failure here is a
		// programming error, not a caller error.
		panic(fmt.Sprintf("state/postgres: encode artifact references: %v", err))
	}
	return raw
}

func scanEntry(row pgx.Row, e *statev1.Entry) error {
	var value, refs []byte
	var updated time.Time
	if err := row.Scan(&e.Namespace, &e.Key, &value, &refs, &e.Generation, &e.Revision, &updated, &e.UpdatedBy); err != nil {
		return err
	}
	e.Value = value
	e.UpdatedAt = updated
	if err := json.Unmarshal(refs, &e.ArtifactReferences); err != nil {
		return err
	}
	return nil
}

func parseETag(v string) (int64, int64, error) {
	parts := strings.Split(strings.Trim(v, `"`), ":")
	if len(parts) != 3 || parts[0] != "kodama-state-v1" {
		return 0, 0, errors.New("invalid ETag")
	}
	g, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	r, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return g, r, nil
}

func randomGeneration() int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		panic(fmt.Sprintf("state/postgres: random source failed: %v", err))
	}
	return n.Int64() + 1
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return state.ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return state.ErrExists
		case "40001", "40P01", "55P03", "57014", "53300":
			return fmt.Errorf("%w: %v", state.ErrUnavailable, err)
		}
	}
	return err
}

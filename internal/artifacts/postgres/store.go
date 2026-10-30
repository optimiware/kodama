// Package postgres is the PostgreSQL adapter of the artifact module: tenant
// scoped persistence of artifact metadata, the object records that make cleanup
// recoverable, and retention pins.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries explicit tenant and project predicates and is additionally
// confined by forced row level security and composite foreign keys. No call to
// the object store or a registry happens in a transaction: the service records
// intent first, performs the external effect, then commits the outcome under a
// fenced claim.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// Config configures a Store.
type Config struct {
	// DB begins transactions; use the non-owner runtime pool (*storage.Pool).
	DB storage.TxBeginner
}

// Store implements artifacts.Repository and artifacts.CleanupStore.
type Store struct {
	db storage.TxBeginner
}

// NewStore validates cfg.
func NewStore(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("artifacts/postgres: DB is required")
	}
	return &Store{db: cfg.DB}, nil
}

var (
	_ artifacts.Repository   = (*Store)(nil)
	_ artifacts.CleanupStore = (*Store)(nil)
)

func (s *Store) write(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, tenant, fn))
}

func (s *Store) read(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn))
}

// classify maps retryable PostgreSQL conditions to ErrUnavailable. Entity and
// module sentinels pass through unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "40001", "40P01", "55P03", "57014", "53300":
			return fmt.Errorf("%w: %v", artifacts.ErrUnavailable, err)
		}
	}
	return err
}

func newAuditID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("artifacts/postgres: random source failed: %v", err))
	}
	return "aud_" + hex.EncodeToString(b)
}

// auditEntry records a transition with safe metadata only: identifiers, kinds
// and states, never digests of content, keys, URLs or credentials.
func auditEntry(ctx context.Context, tx pgx.Tx, a artifacts.Artifact, actor artifacts.Actor, action, requestID string, now time.Time) error {
	meta, err := json.Marshal(map[string]string{
		"kind": string(a.Kind()), "role": string(a.Role()), "attemptId": string(a.Attempt()), "state": string(a.State()), "disposition": string(a.Disposition()),
	})
	if err != nil {
		return err
	}
	sc := a.Scope()
	var req *string
	if requestID != "" {
		req = &requestID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, request_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'artifact', $8, $9, $10, $11)`,
		string(sc.TenantID), newAuditID(), string(sc.ProjectID), now, actor.Type, actor.ID, action, string(a.ID()), int64(a.Version()), req, meta)
	return err
}

// ---- row mapping ----------------------------------------------------------

const artifactColumns = `tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, media_type, size_bytes, registry,
source_repository_url, source_commit, state, disposition, staging_key, sealed_key, transfer_expires_at, verified_at, verified_size_bytes,
claim_token, claim_generation, claim_until, created_at, updated_at, version`

type rowScanner interface{ Scan(dest ...any) error }

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func scanArtifact(r rowScanner) (artifacts.Artifact, error) {
	rec, err := scanRecord(r)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	return artifacts.Restore(rec)
}

func scanRecord(r rowScanner) (artifacts.Record, error) {
	var (
		tenant, project, id, run, attempt, step, name, kind, role, digest, state, disp string
		mediaType, registry, srcURL, srcCommit, staging, sealed, claimToken            *string
		size, verifiedSize                                                             *int64
		transferExp, verifiedAt, claimUntil                                            *time.Time
		claimGen, version                                                              int64
		created, updated                                                               time.Time
	)
	if err := r.Scan(&tenant, &project, &id, &run, &attempt, &step, &name, &kind, &role, &digest, &mediaType, &size, &registry,
		&srcURL, &srcCommit, &state, &disp, &staging, &sealed, &transferExp, &verifiedAt, &verifiedSize,
		&claimToken, &claimGen, &claimUntil, &created, &updated, &version); err != nil {
		return artifacts.Record{}, err
	}
	res := artifacts.Reservation{
		Scope:   domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)},
		Attempt: domain.AttemptID(attempt), Run: domain.RunID(run), Step: step, Name: name,
		Kind: artifacts.Kind(kind), Role: artifacts.Role(role), Digest: artifacts.Digest(digest),
		MediaType: str(mediaType), Size: size, Registry: str(registry),
		Provenance: artifacts.Provenance{RepositoryURL: str(srcURL), Commit: str(srcCommit)},
	}
	rec := artifacts.Record{
		ID: domain.ArtifactID(id), Reservation: res, State: artifacts.State(state), Disposition: artifacts.Disposition(disp),
		StagingKey: str(staging), SealedKey: str(sealed), Version: domain.Version(version), CreatedAt: created, UpdatedAt: updated,
		TransferExpiresAt: transferExp, VerifiedAt: verifiedAt, VerifiedSize: verifiedSize,
	}
	if claimToken != nil && claimUntil != nil {
		rec.Claim = artifacts.Claim{Token: *claimToken, Generation: claimGen, Until: *claimUntil}
	} else {
		rec.Claim = artifacts.Claim{Generation: claimGen}
	}
	return rec, nil
}

// restore hydrates the workspace provenance of a run_shared producer (its own
// table, absent from a database that never applied the migration) and restores
// the artifact.
func restore(ctx context.Context, tx pgx.Tx, rec artifacts.Record) (artifacts.Artifact, error) {
	if rec.Reservation.Provenance.IsZero() {
		return artifacts.Restore(rec)
	}
	ok, err := hasProvenanceTable(ctx, tx)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	if ok {
		var state string
		var ignored bool
		var lineage *string
		err := tx.QueryRow(ctx, `SELECT workspace_state, ignored_present, lineage FROM artifact_workspace_provenance
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3`, string(rec.Reservation.Scope.TenantID), string(rec.Reservation.Scope.ProjectID), string(rec.ID)).
			Scan(&state, &ignored, &lineage)
		switch {
		case err == nil:
			p := rec.Reservation.Provenance
			p.WorkspaceState, p.IgnoredPresent, p.Lineage = state, ignored, str(lineage)
			rec.Reservation.Provenance = p
		case !errors.Is(err, pgx.ErrNoRows):
			return artifacts.Artifact{}, err
		}
	}
	return artifacts.Restore(rec)
}

// hasProvenanceTable reports whether the workspace provenance migration applied.
func hasProvenanceTable(ctx context.Context, tx pgx.Tx) (bool, error) {
	var present bool
	err := tx.QueryRow(ctx, `SELECT to_regclass('public.artifact_workspace_provenance') IS NOT NULL`).Scan(&present)
	return present, err
}

func getArtifact(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.ArtifactID, lock string) (artifacts.Artifact, error) {
	q := `SELECT ` + artifactColumns + ` FROM artifacts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`
	if lock != "" {
		q += ` ` + lock
	}
	rec, err := scanRecord(tx.QueryRow(ctx, q, string(scope.TenantID), string(scope.ProjectID), string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifacts.Artifact{}, artifacts.ErrNotFound
	}
	if err != nil {
		return artifacts.Artifact{}, err
	}
	return restore(ctx, tx, rec)
}

// ---- reads ----------------------------------------------------------------

// Get implements artifacts.Repository.
func (s *Store) Get(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (artifacts.Artifact, error) {
	var a artifacts.Artifact
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		a, err = getArtifact(ctx, tx, scope, id, "")
		return err
	})
	return a, err
}

// GetForAttempt implements artifacts.Repository. An artifact of another
// attempt is reported exactly as a missing one.
func (s *Store) GetForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, id domain.ArtifactID) (artifacts.Artifact, error) {
	a, err := s.Get(ctx, scope, id)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	if a.Attempt() != attempt {
		return artifacts.Artifact{}, artifacts.ErrNotFound
	}
	return a, nil
}

// FindByName implements artifacts.Repository.
func (s *Store) FindByName(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, name string) (artifacts.Artifact, error) {
	var a artifacts.Artifact
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		a, err = findByName(ctx, tx, scope, attempt, name)
		return err
	})
	return a, err
}

func findByName(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, name string) (artifacts.Artifact, error) {
	rec, err := scanRecord(tx.QueryRow(ctx, `SELECT `+artifactColumns+` FROM artifacts
WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 AND name = $4`, string(scope.TenantID), string(scope.ProjectID), string(attempt), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifacts.Artifact{}, artifacts.ErrNotFound
	}
	if err != nil {
		return artifacts.Artifact{}, err
	}
	return restore(ctx, tx, rec)
}

// ReferencesForAttempt implements artifacts.Repository.
func (s *Store) ReferencesForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]artifacts.Reference, error) {
	out := map[domain.ArtifactID]artifacts.Reference{}
	if len(ids) == 0 {
		return out, nil
	}
	list := make([]string, len(ids))
	for i, id := range ids {
		list[i] = string(id)
	}
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+artifactColumns+` FROM artifacts
WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 AND id = ANY($4)`, string(scope.TenantID), string(scope.ProjectID), string(attempt), list)
		if err != nil {
			return err
		}
		var recs []artifacts.Record
		for rows.Next() {
			rec, err := scanRecord(rows)
			if err != nil {
				rows.Close()
				return err
			}
			recs = append(recs, rec)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, rec := range recs {
			a, err := restore(ctx, tx, rec)
			if err != nil {
				return err
			}
			out[a.ID()] = a.Reference()
		}
		return nil
	})
	return out, err
}

// ---- reservation ----------------------------------------------------------

func lockAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kodama.artifacts.attempt:' || $1::text || '/' || $2::text || '/' || $3::text, 0))`,
		string(scope.TenantID), string(scope.ProjectID), string(attempt))
	return err
}

// Reserve implements artifacts.Repository. The per-attempt advisory lock makes
// the existence check, the aggregate limit check and the insert one atomic
// decision across concurrent reservations of the same attempt.
func (s *Store) Reserve(ctx context.Context, cmd artifacts.ReserveCommand) (artifacts.ReserveResult, error) {
	a := cmd.Artifact
	res := a.Reservation()
	var out artifacts.ReserveResult
	err := s.write(ctx, res.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockAttempt(ctx, tx, res.Scope, res.Attempt); err != nil {
			return err
		}
		existing, err := findByName(ctx, tx, res.Scope, res.Attempt, res.Name)
		if err == nil {
			out = artifacts.ReserveResult{Artifact: existing}
			return nil
		}
		if !errors.Is(err, artifacts.ErrNotFound) {
			return err
		}
		if res.Kind == artifacts.KindFile {
			if err := checkLimits(ctx, tx, res, cmd.Limits); err != nil {
				return err
			}
		}
		if err := insertArtifact(ctx, tx, a); err != nil {
			return err
		}
		if a.Kind() == artifacts.KindFile {
			if _, err := tx.Exec(ctx, `INSERT INTO artifact_objects (tenant_id, project_id, artifact_id, key, purpose, generation, state, writable_until, created_at)
VALUES ($1, $2, $3, $4, 'staging', 0, 'live', $5, $6)`, string(res.Scope.TenantID), string(res.Scope.ProjectID), string(a.ID()), a.StagingKey(), *a.TransferExpiresAt(), cmd.Now); err != nil {
				return err
			}
		}
		action := "artifact.reserved"
		if a.Kind() == artifacts.KindOCI {
			action = "artifact.registered"
		}
		if err := auditEntry(ctx, tx, a, cmd.Actor, action, cmd.RequestID, cmd.Now); err != nil {
			return err
		}
		out = artifacts.ReserveResult{Artifact: a, Created: true}
		return nil
	})
	return out, err
}

func insertArtifact(ctx context.Context, tx pgx.Tx, a artifacts.Artifact) error {
	if err := insertArtifactRow(ctx, tx, a); err != nil {
		return err
	}
	p := a.Reservation().Provenance
	if p.WorkspaceState == "" {
		return nil
	}
	// A run_shared producer's recorded workspace state is kept with the artifact
	// or the reservation fails: it is never dropped.
	ok, err := hasProvenanceTable(ctx, tx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: the database has not applied the workspace provenance migration", artifacts.ErrUnavailable)
	}
	r := a.Reservation()
	_, err = tx.Exec(ctx, `INSERT INTO artifact_workspace_provenance (tenant_id, project_id, artifact_id, workspace_state, ignored_present, lineage, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, string(r.Scope.TenantID), string(r.Scope.ProjectID), string(a.ID()), p.WorkspaceState, p.IgnoredPresent, nullable(p.Lineage), a.CreatedAt())
	return err
}

func insertArtifactRow(ctx context.Context, tx pgx.Tx, a artifacts.Artifact) error {
	r := a.Reservation()
	var size *int64
	if r.Size != nil {
		v := *r.Size
		size = &v
	}
	var verifiedAt *time.Time
	var verifiedSize *int64
	if a.VerifiedAt() != nil {
		t := *a.VerifiedAt()
		verifiedAt = &t
		if vs, ok := a.Size(); ok {
			verifiedSize = &vs
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO artifacts (tenant_id, project_id, id, run_id, attempt_id, step_id, name, kind, role, digest, media_type, size_bytes, registry,
 source_repository_url, source_commit, state, disposition, staging_key, sealed_key, transfer_expires_at, verified_at, verified_size_bytes,
 claim_generation, created_at, updated_at, version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,0,$23,$24,$25)`,
		string(r.Scope.TenantID), string(r.Scope.ProjectID), string(a.ID()), string(r.Run), string(r.Attempt), r.Step, r.Name, string(r.Kind), string(r.Role), string(r.Digest),
		nullable(r.MediaType), size, nullable(r.Registry), nullable(r.Provenance.RepositoryURL), nullable(r.Provenance.Commit),
		string(a.State()), string(a.Disposition()), nullable(a.StagingKey()), nullable(a.SealedKey()), a.TransferExpiresAt(), verifiedAt, verifiedSize,
		a.CreatedAt(), a.UpdatedAt(), int64(a.Version()))
	return err
}

// checkLimits enforces the aggregate bounds of the producing attempt. It runs
// under the attempt's advisory lock, so concurrent reservations cannot together
// exceed a limit.
func checkLimits(ctx context.Context, tx pgx.Tx, r artifacts.Reservation, l artifacts.Limits) error {
	rows, err := tx.Query(ctx, `SELECT role, count(*), coalesce(sum(size_bytes), 0)::bigint FROM artifacts
WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 AND kind = 'file' AND disposition = 'active' GROUP BY role`,
		string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.Attempt))
	if err != nil {
		return err
	}
	defer rows.Close()
	var count, bytes int64
	for rows.Next() {
		var role string
		var c, b int64
		if err := rows.Scan(&role, &c, &b); err != nil {
			return err
		}
		if role == string(r.Role) {
			count, bytes = c, b
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var maxFiles, maxBytes int64
	switch r.Role {
	case artifacts.RoleOutput:
		maxFiles, maxBytes = l.OutputFiles, l.OutputBytes
	case artifacts.RoleDiagnostic:
		maxFiles, maxBytes = l.DiagnosticFiles, l.DiagnosticBytes
	case artifacts.RoleBundle:
		maxFiles, maxBytes = l.BundleFiles, l.BundleBytes
	}
	if count+1 > maxFiles || bytes+*r.Size > maxBytes {
		return fmt.Errorf("%w: the attempt's %s artifacts would exceed their limit", artifacts.ErrLimitExceeded, r.Role)
	}
	return nil
}

// RecordTransfer implements artifacts.Repository.
func (s *Store) RecordTransfer(ctx context.Context, scope domain.Scope, id domain.ArtifactID, expires, now time.Time) error {
	return s.write(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE artifacts SET transfer_expires_at = GREATEST(transfer_expires_at, $4), updated_at = $5
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND kind = 'file' AND disposition = 'active'`,
			string(scope.TenantID), string(scope.ProjectID), string(id), expires, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return artifacts.ErrNotFound
		}
		// A renewed instruction can land a late write after cleanup: reopen the
		// staging record (cancelling deletion intent) and extend its horizon, so
		// a repeat sweep deletes the late object.
		_, err = tx.Exec(ctx, `UPDATE artifact_objects SET writable_until = GREATEST(writable_until, $4),
  state = 'live', deleted_at = NULL, delete_token = NULL, delete_until = NULL
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3 AND purpose = 'staging'`,
			string(scope.TenantID), string(scope.ProjectID), string(id), expires)
		return err
	})
}

// ---- completion -----------------------------------------------------------

// BeginCompletion implements artifacts.Repository.
func (s *Store) BeginCompletion(ctx context.Context, cmd artifacts.BeginCompletion) (artifacts.ClaimResult, error) {
	var out artifacts.ClaimResult
	err := s.write(ctx, cmd.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := getArtifact(ctx, tx, cmd.Scope, cmd.ID, "FOR UPDATE")
		if err != nil {
			return err
		}
		if a.Attempt() != cmd.Attempt || a.Disposition() != artifacts.DispositionActive {
			return artifacts.ErrNotFound
		}
		if a.Kind() == artifacts.KindOCI || a.State() == artifacts.StateUsable {
			out = artifacts.ClaimResult{Artifact: a}
			return nil
		}
		if a.Claim().Active(cmd.Now) {
			return artifacts.ErrInProgress
		}
		gen := a.Claim().Generation + 1
		until := cmd.Now.Add(cmd.Lease).Truncate(time.Microsecond)
		// Durable intent first: the candidate destination is recorded before any
		// object is written to it, so a crash can strand bytes but never forget
		// them.
		if _, err := tx.Exec(ctx, `UPDATE artifacts SET claim_token = $4, claim_generation = $5, claim_until = $6, updated_at = $7
WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(cmd.Scope.TenantID), string(cmd.Scope.ProjectID), string(cmd.ID), cmd.Token, gen, until, cmd.Now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO artifact_objects (tenant_id, project_id, artifact_id, key, purpose, generation, state, writable_until, created_at)
VALUES ($1, $2, $3, $4, 'candidate', $5, 'live', $6, $7)`, string(cmd.Scope.TenantID), string(cmd.Scope.ProjectID), string(cmd.ID), cmd.Destination, gen, until, cmd.Now); err != nil {
			return err
		}
		a, err = getArtifact(ctx, tx, cmd.Scope, cmd.ID, "")
		if err != nil {
			return err
		}
		c := a.Claim()
		out = artifacts.ClaimResult{Artifact: a, Claim: &c}
		return nil
	})
	return out, err
}

// ReleaseClaim implements artifacts.Repository.
func (s *Store) ReleaseClaim(ctx context.Context, scope domain.Scope, id domain.ArtifactID, token string) error {
	return s.write(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE artifacts SET claim_token = NULL, claim_until = NULL
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND claim_token = $4 AND state = 'reserved'`,
			string(scope.TenantID), string(scope.ProjectID), string(id), token)
		return err
	})
}

// CommitUsable implements artifacts.Repository. The row lock, the claim token
// and generation, the live lease and the winner flag on the exact candidate are
// all checked in one transaction: a fenced-out or expired completion commits
// nothing, and a destination that cleanup already scheduled cannot win.
func (s *Store) CommitUsable(ctx context.Context, cmd artifacts.CommitCommand) (artifacts.Artifact, error) {
	var out artifacts.Artifact
	err := s.write(ctx, cmd.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := getArtifact(ctx, tx, cmd.Scope, cmd.ID, "FOR UPDATE")
		if err != nil {
			return err
		}
		if a.Attempt() != cmd.Attempt {
			return artifacts.ErrNotFound
		}
		if a.Usable() {
			out = a // a concurrent completion of identical verified content won
			return nil
		}
		if !a.Open() {
			return artifacts.ErrNotFound
		}
		c := a.Claim()
		if c.Token == "" || c.Token != cmd.Claim.Token || c.Generation != cmd.Claim.Generation || !c.Active(cmd.Now) {
			return artifacts.ErrClaimLost
		}
		n, err := a.Complete(cmd.Verification, cmd.Destination, cmd.Now)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE artifact_objects SET winner = true
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3 AND key = $4 AND purpose = 'candidate' AND generation = $5 AND state = 'live'`,
			string(cmd.Scope.TenantID), string(cmd.Scope.ProjectID), string(cmd.ID), cmd.Destination, cmd.Claim.Generation)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return artifacts.ErrClaimLost
		}
		if _, err := tx.Exec(ctx, `UPDATE artifacts SET state = 'usable', sealed_key = $4, verified_at = $5, verified_size_bytes = $6,
  claim_token = NULL, claim_until = NULL, updated_at = $5, version = $7
WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(cmd.Scope.TenantID), string(cmd.Scope.ProjectID), string(cmd.ID),
			cmd.Destination, cmd.Now, cmd.Verification.Size, int64(n.Version())); err != nil {
			return err
		}
		if err := auditEntry(ctx, tx, n, cmd.Actor, "artifact.completed", cmd.RequestID, cmd.Now); err != nil {
			return err
		}
		out, err = getArtifact(ctx, tx, cmd.Scope, cmd.ID, "")
		return err
	})
	return out, err
}

// ---- pins -----------------------------------------------------------------

// Pin implements artifacts.Repository. FOR SHARE conflicts with retention's FOR
// UPDATE, so a pin and a deletion decision are totally ordered.
func (s *Store) Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner, now time.Time) error {
	return s.write(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := getArtifact(ctx, tx, scope, id, "FOR SHARE")
		if err != nil {
			return err
		}
		if a.Disposition() == artifacts.DispositionRemoved {
			return artifacts.ErrNotFound
		}
		if !a.Usable() {
			return fmt.Errorf("%w: only usable content can be pinned", artifacts.ErrIncomplete)
		}
		_, err = tx.Exec(ctx, `INSERT INTO artifact_pins (tenant_id, project_id, artifact_id, owner_kind, owner_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, string(scope.TenantID), string(scope.ProjectID), string(id), owner.Kind, owner.ID, now)
		return err
	})
}

// Unpin implements artifacts.Repository.
func (s *Store) Unpin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner) error {
	return s.write(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM artifact_pins WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3 AND owner_kind = $4 AND owner_id = $5`,
			string(scope.TenantID), string(scope.ProjectID), string(id), owner.Kind, owner.ID)
		return err
	})
}

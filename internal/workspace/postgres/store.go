// Package postgres is the PostgreSQL adapter of the workspace module. Every
// statement runs inside storage.WithTenant under the non-owner runtime role,
// carries explicit tenant and project predicates and is further constrained by
// forced row level security. Audit rows and event outbox rows are written in
// the same transaction as the state change. No network or Git call is ever made
// inside a transaction.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace"
)

// DB is satisfied by *storage.Pool and *pgxpool.Pool.
type DB interface {
	storage.TxBeginner
}

// EventAppender is the transaction-local event append of the event bus.
type EventAppender interface {
	Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error)
}

// Options configure a Store.
type Options struct {
	Clock func() time.Time
	NewID func(prefix string) string
}

// Store implements workspace.Store.
type Store struct {
	db     DB
	events EventAppender
	opts   Options
}

var _ workspace.Store = (*Store)(nil)

// NewStore requires a database and an event appender: a workspace transition
// without its audit/outbox records would be unobservable, so there is no
// construction without them.
func NewStore(db DB, events EventAppender, opts Options) (*Store, error) {
	if db == nil || events == nil {
		return nil, fmt.Errorf("%w: database and event appender are required", workspace.ErrNotConfigured)
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.NewID == nil {
		opts.NewID = func(prefix string) string {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				panic(err)
			}
			return prefix + "_" + hex.EncodeToString(b)
		}
	}
	return &Store{db: db, events: events, opts: opts}, nil
}

func (s *Store) now() time.Time { return s.opts.Clock().UTC() }

const wsColumns = `id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref, work_branch,
	initial_commit, accepted_commit, state, version,
	writer_attempt_id, writer_input_commit, writer_expected_tip, writer_reserved_at,
	publish_attempt_id, publish_commit, publish_expected_commit, publish_artifact_id, publish_bundle_digest, publish_started_at,
	publish_driver_id, publish_driver_epoch, publish_driver_claimed_at, publish_attempts, publish_last_attempt_at,
	archive_requested_at, last_error, fence_epoch, publication_fenced, created_at, updated_at`

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nstr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func tm(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.UTC()
}

func ntm(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func utcPtr(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	u := p.UTC()
	return &u
}

func scanWorkspace(row pgx.Row, scope domain.Scope) (*workspace.Workspace, error) {
	var (
		id, workItem, repoID, repoURL, checkoutRef, publisherRef, branch, state string
		initial, accepted                                                       *string
		version                                                                 int64
		wAttempt, wInput, wTip                                                  *string
		wAt                                                                     *time.Time
		pAttempt, pCommit, pExpected, pArtifact, pDigest                        *string
		pStarted                                                                *time.Time
		pDriver                                                                 *string
		pEpoch                                                                  *int64
		pClaimed                                                                *time.Time
		pAttempts                                                               int
		pLast, archiveAt                                                        *time.Time
		lastErr                                                                 *string
		epoch                                                                   int64
		fenced                                                                  bool
		created, updated                                                        time.Time
	)
	if err := row.Scan(&id, &workItem, &repoID, &repoURL, &checkoutRef, &publisherRef, &branch,
		&initial, &accepted, &state, &version,
		&wAttempt, &wInput, &wTip, &wAt,
		&pAttempt, &pCommit, &pExpected, &pArtifact, &pDigest, &pStarted,
		&pDriver, &pEpoch, &pClaimed, &pAttempts, &pLast,
		&archiveAt, &lastErr, &epoch, &fenced, &created, &updated); err != nil {
		return nil, err
	}
	w := &workspace.Workspace{
		ID: workspace.ID(id), Scope: scope, WorkItemID: domain.WorkItemID(workItem),
		RepositoryID: workspace.RepositoryID(repoID), RepositoryURL: repoURL,
		CheckoutSecretRef: checkoutRef, PublisherSecretRef: publisherRef, WorkBranch: branch,
		InitialCommit: workspace.Commit(str(initial)), AcceptedCommit: workspace.Commit(str(accepted)),
		State: workspace.State(state), Version: domain.Version(version),
		ArchiveRequestedAt: utcPtr(archiveAt), LastError: str(lastErr),
		Fence:     workspace.Fence{Epoch: epoch, Fenced: fenced},
		CreatedAt: created.UTC(), UpdatedAt: updated.UTC(),
	}
	if wAttempt != nil {
		w.Writer = &workspace.WriterReservation{
			AttemptID: domain.AttemptID(*wAttempt), InputCommit: workspace.Commit(str(wInput)),
			ExpectedTip: workspace.Commit(str(wTip)), ReservedAt: tm(wAt),
		}
	}
	if pAttempt != nil {
		w.Pending = &workspace.PendingPublication{
			AttemptID: domain.AttemptID(*pAttempt), Commit: workspace.Commit(str(pCommit)), ExpectedCommit: workspace.Commit(str(pExpected)),
			ArtifactID: domain.ArtifactID(str(pArtifact)), BundleDigest: str(pDigest), StartedAt: tm(pStarted),
			DriverID: str(pDriver), DriverClaimed: tm(pClaimed), Attempts: pAttempts, LastAttemptAt: tm(pLast),
		}
		if pEpoch != nil {
			w.Pending.DriverEpoch = *pEpoch
		}
	}
	return w, nil
}

// Insert implements workspace.Store.
func (s *Store) Insert(ctx context.Context, w *workspace.Workspace, fx workspace.Effects) (*workspace.Workspace, bool, error) {
	var out *workspace.Workspace
	created := false
	err := storage.WithTenant(ctx, s.db, w.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Concurrent reservations for one item also conflict on its deterministic
		// work branch. Both unique indexes must arbitrate the insert; naming only
		// the item index can leak a secondary-index violation during that race.
		// A suppressed conflict is a replay only if the scoped item lookup below
		// finds its workspace; unrelated identity/branch collisions still fail.
		tag, err := tx.Exec(ctx, `INSERT INTO workspaces
			(tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref,
			 work_branch, state, version, fence_epoch, publication_fenced, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT DO NOTHING`,
			w.Scope.TenantID, w.Scope.ProjectID, w.ID, w.WorkItemID, w.RepositoryID, w.RepositoryURL, w.CheckoutSecretRef, w.PublisherSecretRef,
			w.WorkBranch, w.State, int64(w.Version), w.Fence.Epoch, w.Fence.Fenced, w.CreatedAt, w.UpdatedAt)
		if err != nil {
			return mapErr(err)
		}
		created = tag.RowsAffected() == 1
		if created {
			out = w
			return s.writeEffects(ctx, tx, w, fx)
		}
		out, err = scanWorkspace(tx.QueryRow(ctx, `SELECT `+wsColumns+` FROM workspaces
			WHERE tenant_id = $1 AND project_id = $2 AND work_item_id = $3`, w.Scope.TenantID, w.Scope.ProjectID, w.WorkItemID), w.Scope)
		return mapErr(err)
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// Get implements workspace.Store.
func (s *Store) Get(ctx context.Context, scope domain.Scope, id workspace.ID) (*workspace.Workspace, error) {
	var out *workspace.Workspace
	err := storage.WithTenantOptions(ctx, s.db, scope.TenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = scanWorkspace(tx.QueryRow(ctx, `SELECT `+wsColumns+` FROM workspaces
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, scope.TenantID, scope.ProjectID, id), scope)
		return mapErr(err)
	})
	return out, err
}

// GetByWorkItem implements workspace.Store.
func (s *Store) GetByWorkItem(ctx context.Context, scope domain.Scope, item domain.WorkItemID) (*workspace.Workspace, error) {
	var out *workspace.Workspace
	err := storage.WithTenantOptions(ctx, s.db, scope.TenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = scanWorkspace(tx.QueryRow(ctx, `SELECT `+wsColumns+` FROM workspaces
			WHERE tenant_id = $1 AND project_id = $2 AND work_item_id = $3`, scope.TenantID, scope.ProjectID, item), scope)
		return mapErr(err)
	})
	return out, err
}

// View implements workspace.Store.
func (s *Store) View(ctx context.Context, scope domain.Scope, id workspace.ID, load workspace.Load) (*workspace.Aggregate, error) {
	var out *workspace.Aggregate
	err := storage.WithTenantOptions(ctx, s.db, scope.TenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		w, err := scanWorkspace(tx.QueryRow(ctx, `SELECT `+wsColumns+` FROM workspaces
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, scope.TenantID, scope.ProjectID, id), scope)
		if err != nil {
			return mapErr(err)
		}
		out, err = s.loadAggregate(ctx, tx, w, load)
		return err
	})
	return out, err
}

func (s *Store) loadAggregate(ctx context.Context, tx pgx.Tx, w *workspace.Workspace, load workspace.Load) (*workspace.Aggregate, error) {
	agg := &workspace.Aggregate{Workspace: w}
	if load.Receipt != "" {
		r, err := s.loadReceipt(ctx, tx, w, load.Receipt)
		if err != nil {
			return nil, err
		}
		agg.Receipt = r
	}
	if load.Pins {
		pins, err := s.loadPins(ctx, tx, w)
		if err != nil {
			return nil, err
		}
		agg.Pins = pins
	}
	return agg, nil
}

// Update implements workspace.Store: lock the row, run the pure mutation,
// persist the workspace under its version, upsert receipts and pins, and write
// audit and event records, all in one transaction.
func (s *Store) Update(ctx context.Context, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error) {
	var out *workspace.Aggregate
	err := storage.WithTenant(ctx, s.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.UpdateInTx(ctx, tx, scope, id, load, fn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateInTx is Update inside a caller-owned tenant transaction (from
// storage.WithTenant for scope.TenantID). It is the entry point through which
// the dispatch and result modules join writer reservation or settlement to
// their own attempt changes atomically. It never commits, rolls back or makes
// a network call.
//
// Lock order: the workspace row first. Callers that also lock attempt, run or
// project rows must take those first and the workspace last, as the database
// schema's transaction boundaries describe (dispatch: eligible run/step ->
// attempt insert -> workspace reservation).
func (s *Store) UpdateInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error) {
	w, err := scanWorkspace(tx.QueryRow(ctx, `SELECT `+wsColumns+` FROM workspaces
		WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`, scope.TenantID, scope.ProjectID, id), scope)
	if err != nil {
		return nil, mapErr(err)
	}
	agg, err := s.loadAggregate(ctx, tx, w, load)
	if err != nil {
		return nil, err
	}
	before := w.Version
	fx, err := fn(agg)
	if err != nil {
		return nil, err
	}
	if w.Version != before {
		if err := s.persistWorkspace(ctx, tx, w, before); err != nil {
			return nil, err
		}
	}
	if err := s.writeEffects(ctx, tx, w, fx); err != nil {
		return nil, err
	}
	return agg, nil
}

func (s *Store) persistWorkspace(ctx context.Context, tx pgx.Tx, w *workspace.Workspace, before domain.Version) error {
	var wa, wi, wt *string
	var wr *time.Time
	if w.Writer != nil {
		a, i, t := string(w.Writer.AttemptID), string(w.Writer.InputCommit), string(w.Writer.ExpectedTip)
		wa, wi, wt, wr = &a, &i, &t, ntm(w.Writer.ReservedAt)
	}
	var pa, pc, pe, par, pd, pdr *string
	var pst, pcl, pla *time.Time
	var pep *int64
	attempts := 0
	if p := w.Pending; p != nil {
		a, c, e, ar, d := string(p.AttemptID), string(p.Commit), string(p.ExpectedCommit), string(p.ArtifactID), p.BundleDigest
		pa, pc, pe, par, pd = &a, &c, &e, &ar, &d
		pst = ntm(p.StartedAt)
		pdr = nstr(p.DriverID)
		if p.DriverID != "" {
			ep := p.DriverEpoch
			pep, pcl = &ep, ntm(p.DriverClaimed)
		}
		pla = ntm(p.LastAttemptAt)
		attempts = p.Attempts
	}
	tag, err := tx.Exec(ctx, `UPDATE workspaces SET
		initial_commit=$5, accepted_commit=$6, state=$7, version=$8,
		writer_attempt_id=$9, writer_input_commit=$10, writer_expected_tip=$11, writer_reserved_at=$12,
		publish_attempt_id=$13, publish_commit=$14, publish_expected_commit=$15, publish_artifact_id=$16, publish_bundle_digest=$17,
		publish_started_at=$18, publish_driver_id=$19, publish_driver_epoch=$20, publish_driver_claimed_at=$21,
		publish_attempts=$22, publish_last_attempt_at=$23,
		archive_requested_at=$24, last_error=$25, fence_epoch=$26, publication_fenced=$27, updated_at=$28
		WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND version=$4`,
		w.Scope.TenantID, w.Scope.ProjectID, w.ID, int64(before),
		nstr(string(w.InitialCommit)), nstr(string(w.AcceptedCommit)), w.State, int64(w.Version),
		wa, wi, wt, wr, pa, pc, pe, par, pd, pst, pdr, pep, pcl, attempts, pla,
		utcPtr(w.ArchiveRequestedAt), nstr(w.LastError), w.Fence.Epoch, w.Fence.Fenced, w.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() != 1 {
		return workspace.ErrStale // cannot happen under FOR UPDATE; fail safe
	}
	return nil
}

func (s *Store) loadReceipt(ctx context.Context, tx pgx.Tx, w *workspace.Workspace, attempt domain.AttemptID) (*workspace.Publication, error) {
	var (
		attemptID, commit, expected, artifact, digest, status, outcome string
		observed, reason                                               *string
		created                                                        time.Time
		settled                                                        *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT attempt_id, commit_oid, expected_commit, artifact_id, bundle_digest, status, observed_tip, conflict_reason, outcome, created_at, settled_at
		FROM workspace_publications WHERE tenant_id=$1 AND project_id=$2 AND workspace_id=$3 AND attempt_id=$4`,
		w.Scope.TenantID, w.Scope.ProjectID, w.ID, attempt).
		Scan(&attemptID, &commit, &expected, &artifact, &digest, &status, &observed, &reason, &outcome, &created, &settled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return &workspace.Publication{
		WorkspaceID: w.ID, Scope: w.Scope, AttemptID: domain.AttemptID(attemptID), Commit: workspace.Commit(commit),
		ExpectedCommit: workspace.Commit(expected), ArtifactID: domain.ArtifactID(artifact), BundleDigest: digest,
		Status: workspace.PublicationStatus(status), ObservedTip: workspace.Commit(str(observed)), ConflictReason: str(reason),
		Outcome: workspace.WriterOutcome(outcome), CreatedAt: created.UTC(), SettledAt: utcPtr(settled),
	}, nil
}

func (s *Store) loadPins(ctx context.Context, tx pgx.Tx, w *workspace.Workspace) ([]*workspace.RetainedRef, error) {
	rows, err := tx.Query(ctx, `SELECT id, commit_oid, ref_name, purpose, run_id, attempt_id, status, created_at, confirmed_at, released_at, removal_requested_at, removed_at
		FROM workspace_retained_refs WHERE tenant_id=$1 AND project_id=$2 AND workspace_id=$3 ORDER BY created_at, id`,
		w.Scope.TenantID, w.Scope.ProjectID, w.ID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*workspace.RetainedRef
	for rows.Next() {
		var (
			id, commit, ref, purpose, run, status string
			attempt                               *string
			created                               time.Time
			confirmed, released, removalReq, done *time.Time
		)
		if err := rows.Scan(&id, &commit, &ref, &purpose, &run, &attempt, &status, &created, &confirmed, &released, &removalReq, &done); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, &workspace.RetainedRef{
			ID: workspace.PinID(id), WorkspaceID: w.ID, Scope: w.Scope, Commit: workspace.Commit(commit), RefName: ref,
			Purpose: workspace.PinPurpose(purpose), RunID: domain.RunID(run), AttemptID: domain.AttemptID(str(attempt)),
			Status: workspace.PinStatus(status), CreatedAt: created.UTC(), ConfirmedAt: utcPtr(confirmed), ReleasedAt: utcPtr(released),
			RemovalRequestedAt: utcPtr(removalReq), RemovedAt: utcPtr(done),
		})
	}
	return out, mapErr(rows.Err())
}

// Reconcilable implements workspace.Store.
func (s *Store) Reconcilable(ctx context.Context, tenant domain.TenantID, after workspace.ID, limit int) ([]workspace.Candidate, error) {
	var out []workspace.Candidate
	err := storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT project_id, id FROM workspaces w
			WHERE tenant_id = $1 AND id > $2 AND (
				state = 'provisioning' OR archive_requested_at IS NOT NULL AND state = 'available' OR publish_attempt_id IS NOT NULL
				OR EXISTS (SELECT 1 FROM workspace_retained_refs r WHERE r.tenant_id = w.tenant_id AND r.project_id = w.project_id
				           AND r.workspace_id = w.id AND r.status IN ('intent', 'removing')))
			ORDER BY id LIMIT $3`, tenant, string(after), limit)
		if err != nil {
			return mapErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var project, id string
			if err := rows.Scan(&project, &id); err != nil {
				return mapErr(err)
			}
			out = append(out, workspace.Candidate{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}, ID: workspace.ID(id)})
		}
		return mapErr(rows.Err())
	})
	return out, err
}

// ListProject implements workspace.Store.
func (s *Store) ListProject(ctx context.Context, scope domain.Scope, after workspace.ID, limit int) ([]workspace.ID, error) {
	var out []workspace.ID
	err := storage.WithTenantOptions(ctx, s.db, scope.TenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM workspaces WHERE tenant_id = $1 AND project_id = $2 AND id > $3 ORDER BY id LIMIT $4`,
			scope.TenantID, scope.ProjectID, string(after), limit)
		if err != nil {
			return mapErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return mapErr(err)
			}
			out = append(out, workspace.ID(id))
		}
		return mapErr(rows.Err())
	})
	return out, err
}

// mapErr translates the no-rows condition; every other driver error is passed
// on unchanged for the caller to treat as a dependency failure.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.ErrNotFound
	}
	return err
}

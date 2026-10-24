// Package postgres is the PostgreSQL adapter of the HTTP foundation: the
// idempotency table contribution and the keyed create transaction helper.
//
// Idempotent execution has this lifecycle. A key is absent; a request claims it
// with a transaction-scoped advisory lock (waiters queue behind the lock rather
// than racing); the business callback runs and the completed response is
// inserted in the same transaction; commit makes the key completed. Rollback,
// error, panic or cancellation returns it to absent. A completed record is
// immutable until it expires; an expired record is deleted under the same lock
// by the request that reuses the key. No unfinished claim is ever committed.
package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/storage"
)

const (
	// DefaultRetention is the proposed 24-hour minimum from the interface
	// specification. It is a development default, not a confirmed production
	// retention decision (decisions O-11); the composition root injects the
	// value.
	DefaultRetention = 24 * time.Hour
	// DefaultLockTimeout bounds how long a request waits for a concurrent
	// request with the same key before answering 503 with Retry-After.
	DefaultLockTimeout = 10 * time.Second
)

// Config configures Idempotency.
type Config struct {
	// DB begins transactions; use the non-owner runtime pool (*storage.Pool).
	DB storage.TxBeginner
	// Retention is how long a completed response is replayable; zero selects
	// DefaultRetention.
	Retention time.Duration
	// MaxStoredBodyBytes bounds a stored response body; zero selects
	// httpapi.DefaultMaxStoredBodyBytes.
	MaxStoredBodyBytes int
	// LockTimeout bounds the wait for a concurrent same-key request; zero
	// selects DefaultLockTimeout.
	LockTimeout time.Duration
}

// Idempotency executes keyed mutations exactly once per key.
type Idempotency struct {
	db          storage.TxBeginner
	retention   time.Duration
	maxBody     int
	lockTimeout time.Duration
}

// NewIdempotency validates cfg.
func NewIdempotency(cfg Config) (*Idempotency, error) {
	if cfg.DB == nil {
		return nil, errors.New("httpapi/postgres: Config.DB is required")
	}
	i := &Idempotency{db: cfg.DB, retention: cfg.Retention, maxBody: cfg.MaxStoredBodyBytes, lockTimeout: cfg.LockTimeout}
	if i.retention == 0 {
		i.retention = DefaultRetention
	}
	if i.maxBody == 0 {
		i.maxBody = httpapi.DefaultMaxStoredBodyBytes
	}
	if i.lockTimeout == 0 {
		i.lockTimeout = DefaultLockTimeout
	}
	if i.retention < time.Second || i.maxBody < 0 || i.maxBody > httpapi.HardMaxStoredBodyBytes || i.lockTimeout < time.Millisecond {
		return nil, errors.New("httpapi/postgres: retention, body bound or lock timeout out of range")
	}
	return i, nil
}

// Request is one idempotent mutation.
type Request struct {
	Identity httpapi.RequestIdentity
	// AuthorizeReplay is called, outside any transaction, before a stored
	// response is returned, with the resource the stored response names (the
	// zero Reference when it names none). Pass Call.AuthorizeReference. It is
	// required so a replay can never skip the current permission check.
	AuthorizeReplay func(ctx context.Context, ref httpapi.Reference) error
	// Do performs the business mutation using tx and returns the success
	// response to store. It must write every state, history, audit and outbox row
	// the mutation requires through tx, must not commit or roll back, must not
	// retain tx, and must not make network calls. Returning an error, panicking
	// or cancelling rolls everything back, including the idempotency record.
	Do func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error)
}

// Result is a completed response.
type Result struct {
	Response httpapi.StoredResponse
	// Replayed is true when Response came from storage and Do did not run.
	Replayed bool
}

// ErrOutcomeUnknown is the cause of 503 responses where commit status could not
// be confirmed. The callback is never rerun automatically: the client retries
// with the same key, which replays if the commit happened and executes if not.
var ErrOutcomeUnknown = errors.New("httpapi/postgres: commit outcome unknown")

// infraError marks a failure of this adapter's own SQL, as opposed to an error
// returned by the business callback.
type infraError struct{ err error }

func (e infraError) Error() string { return e.err.Error() }
func (e infraError) Unwrap() error { return e.err }

type record struct {
	hash    []byte
	status  int
	headers map[string]string
	body    []byte
	ref     httpapi.Reference
	expired bool
}

// Execute runs the request exactly once for its key and returns the response.
// HTTP success must be written only from the returned Result, i.e. after commit.
func (i *Idempotency) Execute(ctx context.Context, req Request) (Result, error) {
	if i == nil || i.db == nil {
		return Result{}, httpapi.UnavailableError(0, errors.New("idempotency adapter is not configured"))
	}
	if req.Do == nil || req.AuthorizeReplay == nil {
		return Result{}, httpapi.InternalServerError(errors.New("httpapi/postgres: Do and AuthorizeReplay are required"))
	}
	id := req.Identity
	if err := id.Validate(); err != nil {
		return Result{}, httpapi.InternalServerError(err)
	}

	var (
		result    Result
		callback  error
		completed bool
		replay    *record
	)
	err := storage.WithTenant(ctx, i.db, id.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := i.claim(ctx, tx, id); err != nil {
			return infraError{err}
		}
		rec, err := i.find(ctx, tx, id)
		if err != nil {
			return infraError{err}
		}
		if rec != nil && !rec.expired {
			if !bytes.Equal(rec.hash, id.Hash[:]) {
				callback = httpapi.IdempotencyConflictError()
				return callback
			}
			replay = rec
			completed = true
			return nil
		}
		if rec != nil { // expired: reuse the key under the same lock.
			if err := i.deleteExpired(ctx, tx, id); err != nil {
				return infraError{err}
			}
		}

		outcome, err := req.Do(ctx, tx)
		if err != nil {
			callback = err
			return err
		}
		if err := outcome.Response.Validate(i.maxBody); err != nil {
			callback = httpapi.InternalServerError(err)
			return callback
		}
		if err := i.insert(ctx, tx, id, outcome); err != nil {
			return infraError{err}
		}
		result = Result{Response: outcome.Response}
		completed = true
		return nil
	})
	if err != nil {
		switch {
		case callback != nil && errors.Is(err, callback):
			return Result{}, callback
		case completed:
			// The callback and insert succeeded and only commit failed: the
			// transaction may or may not be durable. Never claim success and
			// never rerun; the client retries the same key.
			return Result{}, httpapi.UnavailableError(time.Second, fmt.Errorf("%w: %v", ErrOutcomeUnknown, err))
		}
		var infra infraError
		if errors.As(err, &infra) {
			return Result{}, classify(infra.err)
		}
		return Result{}, classify(err)
	}

	if replay != nil {
		// Authorization is rechecked after the lock wait and outside any SQL
		// transaction, before cached content leaves this function.
		if err := req.AuthorizeReplay(ctx, replay.ref); err != nil {
			return Result{}, err
		}
		return Result{
			Response: httpapi.StoredResponse{Status: replay.status, Header: replay.headers, Body: replay.body},
			Replayed: true,
		}, nil
	}
	return result, nil
}

// classify maps adapter SQL failures: contention and serialization problems are
// retryable 503s; everything else is an internal error without detail.
func classify(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "55P03", "40001", "40P01", "57014", "53300":
			return httpapi.UnavailableError(time.Second, err)
		}
		return httpapi.InternalServerError(err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return httpapi.UnavailableError(time.Second, err)
	}
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	return httpapi.UnavailableError(time.Second, err)
}

// lockKey derives the advisory lock from the full lookup key, so only requests
// for the same key serialize (a 64-bit collision merely adds a harmless wait).
func lockKey(id httpapi.RequestIdentity) int64 {
	h := sha256.New()
	h.Write([]byte("kodama-idempotency-lock-v1"))
	for _, s := range []string{string(id.TenantID), id.Principal.ID, id.Method, id.Route, string(id.Key)} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	return int64(binary.BigEndian.Uint64(h.Sum(nil)[:8]))
}

// claim takes the transaction-scoped lock for the key, bounded by lock_timeout,
// then restores the previous timeout so the business callback is unaffected.
func (i *Idempotency) claim(ctx context.Context, tx pgx.Tx, id httpapi.RequestIdentity) error {
	var prev string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('lock_timeout')`).Scan(&prev); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, true)`, strconv.FormatInt(i.lockTimeout.Milliseconds(), 10)+"ms"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock($1)`, lockKey(id)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, true)`, prev)
	return err
}

const keyPredicate = `tenant_id = $1 AND principal = $2 AND method = $3 AND canonical_route = $4 AND key = $5`

func (i *Idempotency) find(ctx context.Context, tx pgx.Tx, id httpapi.RequestIdentity) (*record, error) {
	var (
		rec       record
		kind, rid *string
	)
	err := tx.QueryRow(ctx, `
SELECT request_hash, response_status::int, response_headers, response_body, target_kind, target_id,
       expires_at <= clock_timestamp()
FROM idempotency WHERE `+keyPredicate,
		string(id.TenantID), id.Principal.ID, id.Method, id.Route, string(id.Key),
	).Scan(&rec.hash, &rec.status, &rec.headers, &rec.body, &kind, &rid, &rec.expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if kind != nil && rid != nil {
		rec.ref = httpapi.Reference{Kind: *kind, ID: *rid}
	}
	return &rec, nil
}

func (i *Idempotency) deleteExpired(ctx context.Context, tx pgx.Tx, id httpapi.RequestIdentity) error {
	_, err := tx.Exec(ctx, `DELETE FROM idempotency WHERE `+keyPredicate+` AND expires_at <= clock_timestamp()`,
		string(id.TenantID), id.Principal.ID, id.Method, id.Route, string(id.Key))
	return err
}

func (i *Idempotency) insert(ctx context.Context, tx pgx.Tx, id httpapi.RequestIdentity, o httpapi.Outcome) error {
	headers := o.Response.Header
	if headers == nil {
		headers = map[string]string{}
	}
	hj, err := json.Marshal(headers)
	if err != nil {
		return err
	}
	var project, kind, rid *string
	if id.ProjectID != "" {
		p := string(id.ProjectID)
		project = &p
	}
	if o.Target != (httpapi.Reference{}) {
		kind, rid = &o.Target.Kind, &o.Target.ID
	}
	body := o.Response.Body
	if body == nil {
		body = []byte{}
	}
	_, err = tx.Exec(ctx, `
INSERT INTO idempotency (tenant_id, principal, method, canonical_route, key, project_id, request_hash,
                         response_status, response_headers, response_body, target_kind, target_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12,
        clock_timestamp() + $13::bigint * interval '1 millisecond')`,
		string(id.TenantID), id.Principal.ID, id.Method, id.Route, string(id.Key), project, id.Hash[:],
		o.Response.Status, string(hj), body, kind, rid, i.retention.Milliseconds())
	return err
}

// PurgeExpired deletes up to limit expired records of one tenant and returns how
// many were removed. Records are also replaced on demand when their key is
// reused, so purging only bounds storage; it needs no coordination with
// requests because deletion and reuse take the same row and key locks.
func (i *Idempotency) PurgeExpired(ctx context.Context, tenant domain.TenantID, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("httpapi/postgres: purge limit must be positive")
	}
	var n int64
	err := storage.WithTenant(ctx, i.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
DELETE FROM idempotency WHERE ctid IN (
    SELECT ctid FROM idempotency WHERE tenant_id = $1 AND expires_at <= clock_timestamp() LIMIT $2)`,
			string(tenant), limit)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

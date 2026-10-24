package composition_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace"
)

// Fault points. Each names one seam the execution transaction crosses; the
// event point is suffixed with the event type ("events.append:run.created").
const (
	PointSecretPin        = "secrets.pin"
	PointSecretRelease    = "secrets.release"
	PointWorkspaceReserve = "workspace.reserve"
	PointWorkspaceSettle  = "workspace.settle"
	PointGrantRevoke      = "grants.revoke"
	PointEventAppend      = "events.append"
)

var errInjected = errors.New("injected failure")

// Hit is one observed crossing of a seam, with the identity of the database
// transaction it ran in. Xid comes from txid_current() queried through the very
// pgx.Tx the seam received, so equal Xids prove the peer joined the caller's
// transaction rather than opening its own.
type Hit struct {
	Point string
	Xid   int64
	Seq   int
}

// Faults is the fault-injection and trace layer wrapped around the existing
// seams: the pool's transactions (commit), the peer transaction bindings, the
// grant revoker and the event appender. It holds no production behavior.
type Faults struct {
	pool storage.TxBeginner

	mu     sync.Mutex
	seq    int
	fail   map[string]error
	hooks  map[string]func()
	nth    map[string]int // crossings left before the seam fails (FailNth)
	trace  []Hit
	commit int // 0 normal, 1 lose the acknowledgment after commit, 2 fail the commit (rolled back)
}

// NewFaults wraps pool.
func NewFaults(pool storage.TxBeginner) *Faults {
	return &Faults{pool: pool, fail: map[string]error{}, hooks: map[string]func(){}}
}

// Fail makes the seam fail (after the peer applied its change, so the rollback
// of that change is what the test observes) until Clear.
func (f *Faults) Fail(point string) {
	f.mu.Lock()
	f.fail[point] = errInjected
	f.mu.Unlock()
}

// FailNth lets the first n-1 crossings of the seam pass and fails the n-th (and
// no later one), so an earlier effect of the same transaction has already been
// applied when the failure arrives. Clear removes it.
func (f *Faults) FailNth(point string, n int) {
	f.mu.Lock()
	if f.nth == nil {
		f.nth = map[string]int{}
	}
	f.nth[point] = n
	f.mu.Unlock()
}

// Hook runs fn when the seam is entered, before the peer acts. The transaction
// and every lock it holds are still held, which is the point of a barrier.
func (f *Faults) Hook(point string, fn func()) {
	f.mu.Lock()
	f.hooks[point] = fn
	f.mu.Unlock()
}

// Clear removes every fault and hook.
func (f *Faults) Clear() {
	f.mu.Lock()
	f.fail, f.hooks, f.nth, f.commit = map[string]error{}, map[string]func(){}, nil, 0
	f.mu.Unlock()
}

// Commit selects the commit behavior of every transaction of the pool.
func (f *Faults) Commit(mode int) {
	f.mu.Lock()
	f.commit = mode
	f.mu.Unlock()
}

// Reset forgets the trace.
func (f *Faults) Reset() {
	f.mu.Lock()
	f.trace = nil
	f.mu.Unlock()
}

// Trace returns the hits since Reset.
func (f *Faults) Trace() []Hit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Hit(nil), f.trace...)
}

// Hits returns the hits whose point has the prefix.
func (f *Faults) Hits(prefix string) []Hit {
	var out []Hit
	for _, h := range f.Trace() {
		if strings.HasPrefix(h.Point, prefix) {
			out = append(out, h)
		}
	}
	return out
}

func (f *Faults) enter(ctx context.Context, tx pgx.Tx, point string) {
	var xid int64
	if err := tx.QueryRow(ctx, `SELECT txid_current()`).Scan(&xid); err != nil {
		xid = -1
	}
	f.mu.Lock()
	f.seq++
	f.trace = append(f.trace, Hit{Point: point, Xid: xid, Seq: f.seq})
	hook := f.hooks[point]
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (f *Faults) failure(points ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range points {
		if err := f.fail[p]; err != nil {
			return err
		}
		if left, ok := f.nth[p]; ok {
			f.nth[p] = left - 1
			if left == 1 {
				return errInjected
			}
		}
	}
	return nil
}

// ---- transactions ----

type faultDB struct{ f *Faults }

// DB is the pool as a storage.TxBeginner whose commits the test controls.
func (f *Faults) DB() storage.TxBeginner { return faultDB{f} }

func (d faultDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.f.pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, f: d.f, readOnly: opts.AccessMode == pgx.ReadOnly}, nil
}

type faultTx struct {
	pgx.Tx
	f        *Faults
	readOnly bool // a read-only transaction has no outcome to lose
}

func (t *faultTx) Commit(ctx context.Context) error {
	t.f.mu.Lock()
	mode := t.f.commit
	t.f.mu.Unlock()
	if t.readOnly {
		mode = 0
	}
	switch mode {
	case 1:
		if err := t.Tx.Commit(ctx); err != nil {
			return err
		}
		return errors.New("connection reset by peer while waiting for the commit acknowledgment")
	case 2:
		_ = t.Tx.Rollback(ctx)
		return errors.New("server closed the connection unexpectedly during commit")
	}
	return t.Tx.Commit(ctx)
}

// ---- peers ----

type probeWorkspaces struct {
	inner execpg.WorkspaceTx
	f     *Faults
}

// Workspaces wraps the workspace store's transaction-joining entry point.
func (f *Faults) Workspaces(inner execpg.WorkspaceTx) execpg.WorkspaceTx {
	return &probeWorkspaces{inner: inner, f: f}
}

func (p *probeWorkspaces) UpdateInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error) {
	point := PointWorkspaceReserve
	if load.Receipt != "" {
		point = PointWorkspaceSettle
	}
	p.f.enter(ctx, tx, point)
	agg, err := p.inner.UpdateInTx(ctx, tx, scope, id, load, fn)
	if err != nil {
		return nil, err
	}
	if err := p.f.failure(point); err != nil {
		return nil, err
	}
	return agg, nil
}

type probeSecrets struct {
	inner execpg.SecretPins
	f     *Faults
}

// Secrets wraps the secrets store's pin API.
func (f *Faults) Secrets(inner execpg.SecretPins) execpg.SecretPins {
	return &probeSecrets{inner: inner, f: f}
}

func (p *probeSecrets) RegisterPins(ctx context.Context, tx pgx.Tx, req secretspg.PinRequest) error {
	p.f.enter(ctx, tx, PointSecretPin)
	if err := p.inner.RegisterPins(ctx, tx, req); err != nil {
		return err
	}
	return p.f.failure(PointSecretPin)
}

func (p *probeSecrets) ReleasePins(ctx context.Context, tx pgx.Tx, scope domain.Scope, consumer secrets.ConsumerRef) (int64, error) {
	p.f.enter(ctx, tx, PointSecretRelease)
	n, err := p.inner.ReleasePins(ctx, tx, scope, consumer)
	if err != nil {
		return 0, err
	}
	return n, p.f.failure(PointSecretRelease)
}

// Revoker wraps the grant revocation seam.
func (f *Faults) Revoker(inner execpg.AttemptGrantRevoker) execpg.AttemptGrantRevoker {
	return func(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
		f.enter(ctx, tx, PointGrantRevoke)
		n, err := inner(ctx, tx, scope, attempt, now)
		if err != nil {
			return 0, err
		}
		return n, f.failure(PointGrantRevoke)
	}
}

type probeAppender struct {
	inner execpg.EventAppender
	f     *Faults
}

// Appender wraps the event appender.
func (f *Faults) Appender(inner execpg.EventAppender) execpg.EventAppender {
	return &probeAppender{inner: inner, f: f}
}

func (p *probeAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	point := PointEventAppend + ":" + env.Type
	p.f.enter(ctx, tx, point)
	res, err := p.inner.Append(ctx, tx, env)
	if err != nil {
		return res, err
	}
	return res, p.f.failure(PointEventAppend, point)
}

// External observes calls that must never run inside a SQL transaction (the
// executor and Git). At each call it counts the runtime role's sessions that are
// idle in a transaction: the fixture runs these tests serially, so any such
// session is a transaction held across the external call.
type External struct {
	pool *pgxpool.Pool

	mu     sync.Mutex
	calls  map[string]int
	inside map[string]int
}

// Observe records one external call.
func (x *External) Observe(name string) {
	var open int
	err := x.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open)
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.calls == nil {
		x.calls, x.inside = map[string]int{}, map[string]int{}
	}
	x.calls[name]++
	if err != nil || open > 0 {
		x.inside[name]++
	}
}

// Calls is how many times the named external call ran; Inside how many of them
// ran while a transaction was open.
func (x *External) Calls(name string) int  { x.mu.Lock(); defer x.mu.Unlock(); return x.calls[name] }
func (x *External) Inside(name string) int { x.mu.Lock(); defer x.mu.Unlock(); return x.inside[name] }

type observedPublisher struct {
	workspace.RefPublisher
	ext *External
}

func (p *observedPublisher) Publish(ctx context.Context, repo workspace.RepoRef, req workspace.PublishRequest) error {
	p.ext.Observe("git.publish")
	return p.RefPublisher.Publish(ctx, repo, req)
}

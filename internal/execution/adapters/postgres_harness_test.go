package adapters_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
)

// The harness runs the production PostgreSQL store's admission orchestration
// (execpg.Store.Admit -> AdmitInTx: tenant transaction, work and authority
// checks, environment binding, run and step rows, secret pins, workspace
// retention, history/audit/event writes, commit or rollback) over controlled
// ports: a scripted transaction instead of a database, and fake peers. It
// proves ordering, abort-before-write, exact pin registration and rollback
// propagation of the code that changed. It does not parse or run SQL, so it
// certifies no SQL, RLS or restart behavior; that is T-067.

type runRow struct {
	ID, Purpose, Deployment, Digest, Inputs string
}

type fakeDB struct {
	// trace is every call in the order it happened, committed or not.
	trace []string
	// Committed state: only what survived a commit.
	rows      []string // table of each committed insert
	runs      []runRow
	pins      []string // "consumer|ref@version"
	events    []string // envelope ids
	commits   int
	rollbacks int

	// Injected failures.
	failTable   map[string]error // INSERT INTO <table> fails
	commitErr   error
	revoked     map[string]bool // secret refs whose pin registration is refused
	failEventAt int             // 1-based event append that fails (0 = none)
	eventErr    error

	eventsSeen int
}

func newFakeDB() *fakeDB { return &fakeDB{failTable: map[string]error{}, revoked: map[string]bool{}} }

func (d *fakeDB) log(format string, args ...any) {
	d.trace = append(d.trace, fmt.Sprintf(format, args...))
}

func (d *fakeDB) first(prefix string) int {
	for i, e := range d.trace {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// reset clears the trace between attempts; committed state is kept.
func (d *fakeDB) reset() { d.trace, d.eventsSeen = nil, 0 }

func (d *fakeDB) committedTable(table string) int {
	n := 0
	for _, r := range d.rows {
		if r == table {
			n++
		}
	}
	return n
}

// empty reports that nothing was committed.
func (d *fakeDB) empty() bool {
	return len(d.rows) == 0 && len(d.runs) == 0 && len(d.pins) == 0 && len(d.events) == 0 && d.commits == 0
}

func (d *fakeDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	d.log("begin")
	return &fakeTx{db: d}, nil
}

type fakeTx struct {
	pgx.Tx
	db      *fakeDB
	rows    []string
	runs    []runRow
	pins    []string
	events  []string
	settled bool
}

var insertInto = regexp.MustCompile(`INSERT INTO (\w+)`)

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m := insertInto.FindStringSubmatch(sql)
	if m == nil {
		t.db.log("exec:other") // the tenant context and the like
		return pgconn.CommandTag{}, nil
	}
	table := m[1]
	t.db.log("exec:%s", table)
	if err := t.db.failTable[table]; err != nil {
		return pgconn.CommandTag{}, err
	}
	t.rows = append(t.rows, table)
	if table == "runs" {
		str := func(i int) string {
			switch v := args[i].(type) {
			case string:
				return v
			case *string:
				if v != nil {
					return *v
				}
			case []byte:
				return string(v)
			}
			return ""
		}
		t.runs = append(t.runs, runRow{ID: str(2), Purpose: str(3), Deployment: str(6), Digest: str(13), Inputs: str(12)})
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

// QueryRow answers the admission's existing-run lookup: there is none.
func (t *fakeTx) QueryRow(context.Context, string, ...any) pgx.Row {
	t.db.log("query")
	return noRow{}
}

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }

func (t *fakeTx) Commit(context.Context) error {
	t.db.log("commit")
	if err := t.db.commitErr; err != nil {
		return err
	}
	t.settled = true
	t.db.commits++
	t.db.rows = append(t.db.rows, t.rows...)
	t.db.runs = append(t.db.runs, t.runs...)
	t.db.pins = append(t.db.pins, t.pins...)
	t.db.events = append(t.db.events, t.events...)
	return nil
}

func (t *fakeTx) Rollback(context.Context) error {
	if !t.settled {
		t.db.log("rollback")
		t.db.rollbacks++
		t.settled = true
	}
	return nil
}

// ---- peers ------------------------------------------------------------------

type peerAuthority struct {
	db  *fakeDB
	err error
}

func (a *peerAuthority) Authorize(_ context.Context, actor execution.Actor, _ domain.Scope, perm execution.Permission) error {
	a.db.log("authority:%s:%s", actor.ID, perm)
	return a.err
}

type peerWorkspaces struct{ db *fakeDB }

func (p peerWorkspaces) UpdateInTx(_ context.Context, tx pgx.Tx, _ domain.Scope, _ workspace.ID, _ workspace.Load, _ workspace.Mutation) (*workspace.Aggregate, error) {
	p.db.log("workspace")
	ft := tx.(*fakeTx)
	ft.rows = append(ft.rows, "workspace")
	return nil, nil
}

type peerSecrets struct{ db *fakeDB }

// RegisterPins pins in binding order and stops at the first revoked version,
// leaving the earlier pins of this transaction to its rollback.
func (p peerSecrets) RegisterPins(_ context.Context, tx pgx.Tx, req secretspg.PinRequest) error {
	ft := tx.(*fakeTx)
	for _, b := range req.Bindings {
		p.db.log("pin:%s@%d", b.Ref, b.Version)
		if p.db.revoked[b.Ref] {
			return fmt.Errorf("%w: %s", secrets.ErrRevoked, b.Ref)
		}
		ft.pins = append(ft.pins, fmt.Sprintf("%s|%s@%d", req.Consumer.ID, b.Ref, b.Version))
	}
	return nil
}

func (p peerSecrets) ReleasePins(context.Context, pgx.Tx, domain.Scope, secrets.ConsumerRef) (int64, error) {
	return 0, nil
}

type peerEvents struct{ db *fakeDB }

func (p peerEvents) Append(_ context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	p.db.eventsSeen++
	p.db.log("event:%s", env.ID)
	if p.db.failEventAt != 0 && p.db.eventsSeen == p.db.failEventAt {
		return eventspg.AppendResult{}, p.db.eventErr
	}
	ft := tx.(*fakeTx)
	ft.events = append(ft.events, env.ID)
	return eventspg.AppendResult{}, nil
}

// txReader is delivery's reads inside one open transaction.
type txReader struct {
	snapshots   map[string]delivery.ConfigurationSnapshot
	deployments map[delivery.DeploymentID]delivery.Deployment
	authErr     error
	snapErr     error
	depErr      error
	calls       []string
	db          *fakeDB
}

func key(env domain.EnvironmentID, d runtimev1.Digest) string { return string(env) + "|" + string(d) }

func newTx(snaps ...delivery.ConfigurationSnapshot) *txReader {
	r := &txReader{snapshots: map[string]delivery.ConfigurationSnapshot{}, deployments: map[delivery.DeploymentID]delivery.Deployment{}}
	for _, s := range snaps {
		r.snapshots[key(s.Environment, s.Digest)] = s
	}
	return r
}

func (r *txReader) note(call string) {
	r.calls = append(r.calls, call)
	if r.db != nil {
		r.db.log("bind:%s", call)
	}
}

func (r *txReader) Authorize(_ context.Context, a delivery.Actor, _ domain.Scope, p delivery.Permission) error {
	r.note("authorize:" + a.ID + ":" + string(p))
	return r.authErr
}

func (r *txReader) Snapshot(_ context.Context, _ domain.Scope, env domain.EnvironmentID, d runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	r.note("snapshot")
	if r.snapErr != nil {
		return delivery.ConfigurationSnapshot{}, r.snapErr
	}
	s, ok := r.snapshots[key(env, d)]
	if !ok {
		return delivery.ConfigurationSnapshot{}, delivery.ErrNotFound
	}
	return s, nil
}

func (r *txReader) Deployment(_ context.Context, _ domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error) {
	r.note("deployment")
	if r.depErr != nil {
		return delivery.Deployment{}, r.depErr
	}
	d, ok := r.deployments[id]
	if !ok {
		return delivery.Deployment{}, delivery.ErrNotFound
	}
	return d, nil
}

// harness is the production store wired to the controlled ports.
type harness struct {
	db        *fakeDB
	store     *execpg.Store
	reader    *txReader
	authority *peerAuthority
}

// newHarness builds the production store. A nil reader leaves the delivery
// transaction binding unconfigured.
func newHarness(t *testing.T, reader *txReader) *harness {
	t.Helper()
	db := newFakeDB()
	auth := &peerAuthority{db: db}
	peers := execpg.Peers{
		Authority:  func(pgx.Tx) execution.TxAuthority { return auth },
		Workspaces: peerWorkspaces{db},
		Secrets:    peerSecrets{db},
		Work: func(context.Context, pgx.Tx, domain.Scope, domain.WorkItemID) (workpg.Admission, error) {
			db.log("work-lock")
			return workpg.Admission{ContentRevision: 1}, nil
		},
	}
	if reader != nil {
		reader.db = db
		peers.Environments = func(pgx.Tx) execution.TxEnvironments {
			tx, err := adapters.NewTxEnvironments(reader)
			if err != nil {
				t.Fatal(err)
			}
			return tx
		}
	}
	n := 0
	st, err := execpg.NewStore(db, peerEvents{db}, peers, execpg.Options{
		Clock: func() time.Time { return t0 },
		NewID: func(prefix string) string { n++; return fmt.Sprintf("%s_%d", prefix, n) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{db: db, store: st, reader: reader, authority: auth}
}

func (h *harness) admit(plan *execution.AdmissionPlan) (*execution.Run, bool, error) {
	h.db.reset()
	return h.store.Admit(context.Background(), plan)
}

// wroteNothing reports that the attempt aborted before any row, pin, workspace
// retention or event was written (the tenant context statement is not a write).
func (h *harness) wroteNothing() bool {
	for _, e := range h.db.trace {
		if strings.HasPrefix(e, "exec:") && e != "exec:other" || strings.HasPrefix(e, "pin:") || strings.HasPrefix(e, "event:") || e == "workspace" {
			return false
		}
	}
	return true
}

func (h *harness) eventIDs() []string {
	var out []string
	for _, e := range h.db.trace {
		if id, ok := strings.CutPrefix(e, "event:"); ok {
			out = append(out, id)
		}
	}
	return out
}

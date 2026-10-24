package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/adapters"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
)

// The tests below drive the real gate store's decision path (gates/postgres
// Store.Decide: tenant transaction, gate row, reviewer authority, run view,
// facts including the deployment read, the one immutable decision, audit row and
// event) with the production DeploymentBinder (adapters.BindDeployments) over a
// scripted transaction. The scripted database keeps committed state apart from
// each transaction's staged writes, so commit and rollback are observable. It
// proves what the adapter owns: which transaction the deployment is read in and
// that the decision does not survive a failed read or a later failure. It does
// not parse SQL; real PostgreSQL qualification stays with T-073/T-038.

type gxStaged struct {
	gateUpdate []any
	audits     int
	events     []eventsv1.Envelope
}

// gxDB is the scripted tenant database.
type gxDB struct {
	mu sync.Mutex

	// Committed state.
	gateRow     depRow
	runRow      depRow
	stepRow     []any
	deployments map[string]delivery.Deployment
	decided     [][]any
	audits      int
	events      []eventsv1.Envelope
	commits     int
	rollbacks   int

	// Observations, in order, tagged with the transaction that served them.
	log []string

	// Injected failures.
	deploymentErr error
	auditErr      error
	eventErr      error
	commitErr     error

	nextTx int
}

type gxTx struct {
	pgx.Tx
	db     *gxDB
	id     int
	staged gxStaged
}

func (d *gxDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextTx++
	d.log = append(d.log, fmt.Sprintf("tx%d:begin", d.nextTx))
	return &gxTx{db: d, id: d.nextTx}, nil
}

func (x *gxTx) note(what string) {
	x.db.mu.Lock()
	x.db.log = append(x.db.log, fmt.Sprintf("tx%d:%s", x.id, what))
	x.db.mu.Unlock()
}

func (x *gxTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "set_config"):
		return pgconn.NewCommandTag("SELECT 1"), nil
	case strings.Contains(sql, "UPDATE gates"):
		x.note("update-gate")
		x.staged.gateUpdate = args
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "INSERT INTO audit"):
		x.note("audit")
		if x.db.auditErr != nil {
			return pgconn.CommandTag{}, x.db.auditErr
		}
		x.staged.audits++
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	return pgconn.CommandTag{}, fmt.Errorf("unscripted statement: %s", sql)
}

func (x *gxTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM gates WHERE"):
		x.note("gate")
		return x.db.gateRow
	case strings.Contains(sql, "FROM runs WHERE"):
		x.note("run")
		return x.db.runRow
	case strings.Contains(sql, "FROM work_items"):
		x.note("work-item")
		return depRow{values: []any{int64(2), "in_progress"}}
	case strings.Contains(sql, "FROM deployments"):
		x.note("deployment")
		if x.db.deploymentErr != nil {
			return depRow{err: x.db.deploymentErr}
		}
		tenant, project, id := args[0].(string), args[1].(string), args[2].(string)
		d, ok := x.db.deployments[tenant+"/"+project+"/"+id]
		if !ok {
			return depRow{err: pgx.ErrNoRows}
		}
		return rowOf(d)
	}
	return depRow{err: fmt.Errorf("unscripted query: %s", sql)}
}

func (x *gxTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM step_runs") {
		x.note("steps")
		return &gxRows{rows: [][]any{x.db.stepRow}}, nil
	}
	return nil, fmt.Errorf("unscripted query: %s", sql)
}

func (x *gxTx) Commit(context.Context) error {
	x.note("commit")
	x.db.mu.Lock()
	defer x.db.mu.Unlock()
	if x.db.commitErr != nil {
		return x.db.commitErr
	}
	x.db.commits++
	if x.staged.gateUpdate != nil {
		x.db.decided = append(x.db.decided, x.staged.gateUpdate)
	}
	x.db.audits += x.staged.audits
	x.db.events = append(x.db.events, x.staged.events...)
	return nil
}

func (x *gxTx) Rollback(context.Context) error {
	x.db.mu.Lock()
	x.db.rollbacks++
	x.db.log = append(x.db.log, fmt.Sprintf("tx%d:rollback", x.id))
	x.db.mu.Unlock()
	return nil
}

type gxRows struct {
	pgx.Rows
	rows [][]any
	i    int
}

func (r *gxRows) Next() bool { r.i++; return r.i <= len(r.rows) }
func (r *gxRows) Close()     {}
func (r *gxRows) Err() error { return nil }
func (r *gxRows) Scan(dest ...any) error {
	return depRow{values: r.rows[r.i-1]}.Scan(dest...)
}

// gxEvents is the event appender: it stages the envelope in the transaction it
// is handed, or fails.
type gxEvents struct{ db *gxDB }

func (e gxEvents) Append(_ context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	x := tx.(*gxTx)
	x.note("event:" + env.Type)
	if e.db.eventErr != nil {
		return eventspg.AppendResult{}, e.db.eventErr
	}
	x.staged.events = append(x.staged.events, env)
	return eventspg.AppendResult{}, nil
}

type gxAuthority struct{ db *gxDB }

func (a gxAuthority) bind(tx pgx.Tx) gates.TxAuthority {
	tx.(*gxTx).note("authority")
	return allowReviewer{}
}

type allowReviewer struct{}

func (allowReviewer) Authorize(context.Context, gates.Actor, domain.Scope, gates.Permission) error {
	return nil
}

// neverExecution fails the test if a decision touches the execution step.
type neverExecution struct{ t *testing.T }

func (n neverExecution) UpdateInTx(context.Context, pgx.Tx, domain.Scope, domain.RunID, execution.UpdateOptions, execution.Mutation) (*execution.Aggregate, any, error) {
	n.t.Error("a decision must not settle the execution step")
	return nil, nil, errors.New("unexpected")
}

type gxWorld struct {
	db    *gxDB
	store *gatespg.Store
	gate  *gates.Gate
	dep   delivery.Deployment
}

const gxCommit = commitA

func newGxWorld(t *testing.T, mutateDeployment func(*delivery.DeploymentRecord)) *gxWorld {
	t.Helper()
	dep := record(t, depScope, mutateDeployment)
	subject, err := gates.NewSubject(gates.SubjectInput{
		Scope: depScope, WorkItemID: "wi_1042", ContentRevision: 2, CandidateCommit: gxCommit, DefinitionID: "pd_01",
		DeploymentID: string(dep.ID()), DeploymentConfigDigest: string(digestA),
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := gates.Request(gates.RequestInput{ID: "gate_1", Scope: depScope, RunID: "run_1", StepID: "review", Subject: subject,
		Required: gates.Required{Evidence: []gates.RequiredEvidence{}}, Now: t0, Deadline: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	g.MarkClean()
	required, err := json.Marshal(g.Required)
	if err != nil {
		t.Fatal(err)
	}
	var nilStr *string
	var nilTime *time.Time
	var nilInt *int64
	gateRow := depRow{values: []any{string(g.ID), string(g.RunID), g.StepID, string(g.Subject.Canonical()), required, string(g.Status), int64(g.Version),
		g.RequestedAt, g.Deadline, nilStr, nilStr, nilStr, nilStr, nilTime, nilTime, nilStr, nilStr, nilInt, g.CreatedAt, g.UpdatedAt}}

	// The run: a work run whose pinned input names the deployment the gate step
	// reviews, with a single pending gate step.
	inputs, err := json.Marshal(execution.Snapshot{Inputs: []execution.PinnedInput{{Name: "deployment", Type: "string", Value: runtimev1.StringValue(string(dep.ID())), Source: "run"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, rev, commit := "wi_1042", int64(2), gxCommit
	runRow := depRow{values: []any{"work", &item, "pd_01", &rev, &commit, inputs, string(execution.RetentionConfirmed), string(execution.RunRunning), nil, nil}}
	spec, err := execution.EncodeStep(pipeline.Step{ID: "review", Kind: pipeline.KindGate, Gate: &pipeline.GateSpec{DeploymentID: "${{ inputs.deployment }}"}})
	if err != nil {
		t.Fatal(err)
	}
	stepRow := []any{"review", 0, spec, string(execution.StepPending), nilStr, 0, nilStr, []byte("{}")}

	db := &gxDB{gateRow: gateRow, runRow: runRow, stepRow: stepRow,
		deployments: map[string]delivery.Deployment{string(depScope.TenantID) + "/" + string(depScope.ProjectID) + "/" + string(dep.ID()): dep}}
	store, err := gatespg.NewStore(gatespg.Config{
		DB: db, Events: gxEvents{db}, Execution: neverExecution{t}, Authority: gxAuthority{db}.bind,
		// The production binder, unchanged.
		Deployments: adapters.BindDeployments,
		Clock:       func() time.Time { return t0 },
		NewID:       func(prefix string) string { return prefix + "_x" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &gxWorld{db: db, store: store, gate: g, dep: dep}
}

func (w *gxWorld) decide() (*gates.Gate, error) {
	return w.store.Decide(context.Background(), &gates.DecisionPlan{
		Scope: depScope, GateID: w.gate.ID, RequestID: "req_1",
		Command: gates.DecideCommand{
			Decision: gates.DecisionApprove, Reason: "ok", Actor: gates.Actor{Type: gates.ActorUser, ID: "u_reviewer"},
			ExpectedVersion: w.gate.Version, SubjectDigest: w.gate.Subject.Digest(),
		},
	})
}

// txOf returns the transaction that served the first log entry with the suffix.
func (w *gxWorld) txOf(t *testing.T, what string) string {
	t.Helper()
	for _, e := range w.db.log {
		if tx, op, ok := strings.Cut(e, ":"); ok && op == what {
			return tx
		}
	}
	t.Fatalf("%q never happened: %v", what, w.db.log)
	return ""
}

func (w *gxWorld) nothingCommitted(t *testing.T) {
	t.Helper()
	d := w.db
	if len(d.decided) != 0 || d.audits != 0 || len(d.events) != 0 || d.commits != 0 {
		t.Fatalf("effects survived: decisions %d, audits %d, events %d, commits %d (log %v)", len(d.decided), d.audits, len(d.events), d.commits, d.log)
	}
	if d.rollbacks == 0 {
		t.Fatalf("the transaction was not rolled back: %v", d.log)
	}
}

// Test_gate_deployment_binder_atomicity proves the gate_deployment_binder
// atomicity obligation at the adapter seam: the deployment is read in the
// transaction that decides, a failed read or refused deployment prevents the
// decision, and a failure after the read rolls back the decision with its audit
// row and event.
func Test_gate_deployment_binder_atomicity(t *testing.T) {
	t.Run("the deployment is read in the deciding transaction and commits with it", func(t *testing.T) {
		w := newGxWorld(t, nil)
		got, err := w.decide()
		if err != nil {
			t.Fatalf("a live deployment of the gate's project: %v", err)
		}
		if got.Status != gates.StatusApproved {
			t.Fatalf("status %s, want approved", got.Status)
		}
		deciding := w.txOf(t, "update-gate")
		for _, op := range []string{"gate", "deployment", "audit", "event:gate.decided", "commit"} {
			if tx := w.txOf(t, op); tx != deciding {
				t.Fatalf("%s ran in %s, want the deciding transaction %s (log %v)", op, tx, deciding, w.db.log)
			}
		}
		d := w.db
		if d.commits != 1 || d.rollbacks != 0 || len(d.decided) != 1 || d.audits != 1 || len(d.events) != 1 {
			t.Fatalf("commits %d rollbacks %d decisions %d audits %d events %d, want exactly one of each", d.commits, d.rollbacks, len(d.decided), d.audits, len(d.events))
		}
		if d.events[0].Type != eventsv1.TypeGateDecided || d.events[0].TenantID != depScope.TenantID {
			t.Fatalf("event %+v", d.events[0])
		}
		// The deployment was read before the decision was written.
		read, wrote := -1, -1
		for i, e := range d.log {
			if strings.HasSuffix(e, ":deployment") && read < 0 {
				read = i
			}
			if strings.HasSuffix(e, ":update-gate") && wrote < 0 {
				wrote = i
			}
		}
		if read < 0 || wrote < 0 || read > wrote {
			t.Fatalf("the decision was written before the deployment was read: %v", d.log)
		}
	})

	t.Run("a failed deployment read prevents the decision", func(t *testing.T) {
		w := newGxWorld(t, nil)
		w.db.deploymentErr = errors.New("connection reset")
		_, err := w.decide()
		if !errors.Is(err, gates.ErrUnavailable) {
			t.Fatalf("a deployment outage: %v, want unavailable", err)
		}
		w.nothingCommitted(t)
		for _, e := range w.db.log {
			if strings.HasSuffix(e, ":update-gate") || strings.HasSuffix(e, ":audit") {
				t.Fatalf("the decision was written despite the failed read: %v", w.db.log)
			}
		}
		// Recovery: the same decision commits once the read works.
		w.db.deploymentErr = nil
		if _, err := w.decide(); err != nil {
			t.Fatalf("after the outage: %v", err)
		}
		if w.db.commits != 1 || len(w.db.decided) != 1 {
			t.Fatalf("recovery did not commit exactly one decision: %d %d", w.db.commits, len(w.db.decided))
		}
	})

	t.Run("a cleaned-up, deleted or foreign deployment prevents the decision", func(t *testing.T) {
		gone := t0.Add(time.Hour)
		for name, mutate := range map[string]func(*delivery.DeploymentRecord){
			"cleanup requested": cleanupRequested,
			"deleted":           func(r *delivery.DeploymentRecord) { r.DeletedAt = &gone },
		} {
			w := newGxWorld(t, mutate)
			_, err := w.decide()
			var stale *gates.StaleError
			if !errors.As(err, &stale) || stale.Reason != gates.ReasonDeploymentChanged {
				t.Errorf("%s: %v, want a stale deployment", name, err)
			}
			w.nothingCommitted(t)
		}
		// The deployment exists, but in another project: the scoped read finds
		// nothing and the gate is stale, never bound to it.
		w := newGxWorld(t, nil)
		foreign := record(t, otherScope, nil)
		w.db.deployments = map[string]delivery.Deployment{
			string(depScope.TenantID) + "/" + string(depScope.ProjectID) + "/" + string(w.dep.ID()): foreign,
		}
		if _, err := w.decide(); !errors.Is(err, gates.ErrStaleSubject) {
			t.Errorf("a foreign deployment: %v, want stale", err)
		}
		w.nothingCommitted(t)
	})

	t.Run("a changed configuration digest prevents the decision", func(t *testing.T) {
		w := newGxWorld(t, func(r *delivery.DeploymentRecord) {
			r.ConfigDigest = runtimev1.Digest("sha256:" + strings.Repeat("d", 64))
		})
		_, err := w.decide()
		var stale *gates.StaleError
		if !errors.As(err, &stale) || stale.Reason != gates.ReasonDeploymentChanged {
			t.Fatalf("a deployment whose digest differs from the gate's subject: %v, want stale", err)
		}
		w.nothingCommitted(t)
	})

	t.Run("a failure after the read rolls back the decision and its effects", func(t *testing.T) {
		for name, arrange := range map[string]func(*gxDB){
			"the audit write fails":  func(d *gxDB) { d.auditErr = errors.New("audit down") },
			"the event append fails": func(d *gxDB) { d.eventErr = errors.New("outbox down") },
			"the commit fails":       func(d *gxDB) { d.commitErr = errors.New("commit lost") },
		} {
			w := newGxWorld(t, nil)
			arrange(w.db)
			if _, err := w.decide(); err == nil {
				t.Errorf("%s: the decision succeeded", name)
				continue
			}
			// The deployment had been read and the decision staged in the same
			// transaction before the failure.
			if w.txOf(t, "deployment") != w.txOf(t, "update-gate") {
				t.Errorf("%s: the deployment was read outside the deciding transaction: %v", name, w.db.log)
			}
			d := w.db
			if len(d.decided) != 0 || d.audits != 0 || len(d.events) != 0 || d.commits != 0 {
				t.Errorf("%s: effects survived: decisions %d audits %d events %d commits %d", name, len(d.decided), d.audits, len(d.events), d.commits)
			}
			if d.rollbacks != 1 {
				t.Errorf("%s: %d rollbacks, want 1", name, d.rollbacks)
			}
			// Without the fault the same decision commits exactly once.
			d.auditErr, d.eventErr, d.commitErr = nil, nil, nil
			if _, err := w.decide(); err != nil {
				t.Errorf("%s: after the fault: %v", name, err)
			}
			if d.commits != 1 || len(d.decided) != 1 || d.audits != 1 || len(d.events) != 1 {
				t.Errorf("%s: recovery committed %d/%d/%d/%d, want one each", name, d.commits, len(d.decided), d.audits, len(d.events))
			}
		}
	})

	t.Run("a store without the binder cannot decide a deployment gate", func(t *testing.T) {
		w := newGxWorld(t, nil)
		store, err := gatespg.NewStore(gatespg.Config{
			DB: w.db, Events: gxEvents{w.db}, Execution: neverExecution{t}, Authority: gxAuthority{w.db}.bind,
			Clock: func() time.Time { return t0 }, NewID: func(prefix string) string { return prefix + "_x" },
		})
		if err != nil {
			t.Fatal(err)
		}
		w.store = store
		if _, err := w.decide(); !errors.Is(err, gates.ErrNotConfigured) {
			t.Fatalf("no deployment binder: %v, want not configured", err)
		}
		w.nothingCommitted(t)
	})
}

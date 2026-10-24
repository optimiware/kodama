package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
)

var errInjected = errors.New("t035 injected outbox failure")

// faults is the fault injection of the atomicity tests: each fault fails one of
// the writes that must commit together with the deployment state, so a
// transaction that kept the others would leave a partial change.
type faults struct {
	s     *stack
	outbx atomic.Bool
	// backlog is the pending count to restore after the backlog fault.
	backlog int
}

// appender wraps the real event append: while armed it fails before the event
// reaches the outbox.
func (f *faults) appender(in deliverypg.EventAppender) deliverypg.EventAppender {
	return func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		if f.outbx.Load() {
			return errInjected
		}
		return in(ctx, tx, env)
	}
}

const failFunction = `CREATE OR REPLACE FUNCTION t035_inject_failure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 't035 injected failure' USING ERRCODE = '54000'; END $$`

// fault is one named way to fail a write; arm/disarm switch it.
type fault struct {
	name string
	// evented marks a fault that only affects transitions appending an event.
	evented bool
	arm     func(f *faults, t *testing.T)
	disarm  func(f *faults, t *testing.T)
}

func triggerFault(name, table, when string) fault {
	return fault{
		name: name,
		arm: func(f *faults, t *testing.T) {
			f.s.X.OwnerExec(failFunction)
			f.s.X.OwnerExec(`CREATE TRIGGER t035_fault BEFORE INSERT ON ` + table + ` FOR EACH ROW ` + when + ` EXECUTE FUNCTION t035_inject_failure()`)
		},
		disarm: func(f *faults, t *testing.T) { f.s.X.OwnerExec(`DROP TRIGGER t035_fault ON ` + table) },
	}
}

func allFaults() []fault {
	return []fault{
		triggerFault("history insert", "deployment_history", ""),
		triggerFault("audit insert", "audit", "WHEN (NEW.action LIKE 'deployment.%' OR NEW.action LIKE 'environment.%')"),
		{name: "outbox append", evented: true,
			arm:    func(f *faults, t *testing.T) { f.outbx.Store(true) },
			disarm: func(f *faults, t *testing.T) { f.outbx.Store(false) }},
		{name: "full event backlog (real rejection)", evented: true,
			arm: func(f *faults, t *testing.T) {
				// The backlog row belongs to the tenant's own context (no owner policy).
				if err := f.s.X.RuntimeTx(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `INSERT INTO event_backlog (tenant_id) VALUES ($1) ON CONFLICT DO NOTHING`, string(scopeA.TenantID)); err != nil {
						return err
					}
					if err := tx.QueryRow(ctx, `SELECT pending_count FROM event_backlog WHERE tenant_id = $1`, string(scopeA.TenantID)).Scan(&f.backlog); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, `UPDATE event_backlog SET pending_count = 1000000 WHERE tenant_id = $1`, string(scopeA.TenantID))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			},
			disarm: func(f *faults, t *testing.T) {
				if err := f.s.X.RuntimeTx(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE event_backlog SET pending_count = $2 WHERE tenant_id = $1`, string(scopeA.TenantID), f.backlog)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}},
	}
}

// state is everything one deployment mutation writes.
type state struct {
	exists                               bool
	version                              int64
	status, reason, objects, cleanup     string
	deleted                              bool
	deployments, history, audits, outbox int
	backlog                              int
	idempotency                          int
}

func (s *stack) state(id delivery.DeploymentID) state {
	var st state
	tenant := string(scopeA.TenantID)
	st.deployments = s.count(`SELECT count(*) FROM deployments WHERE tenant_id = $1`, tenant)
	if err := s.X.Migration.QueryRow(context.Background(), `SELECT version, status, reason, objects::text, cleanup_state, deleted_at IS NOT NULL
FROM deployments WHERE tenant_id = $1 AND id = $2`, tenant, string(id)).Scan(&st.version, &st.status, &st.reason, &st.objects, &st.cleanup, &st.deleted); err == nil {
		st.exists = true
	}
	st.history = s.count(`SELECT count(*) FROM deployment_history WHERE tenant_id = $1`, tenant)
	st.audits = s.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND (action LIKE 'deployment.%' OR action LIKE 'environment.%')`, tenant)
	st.outbox = s.events()
	st.backlog = s.X.CountTenant(scopeA.TenantID, `SELECT coalesce(max(pending_count), 0) FROM event_backlog WHERE tenant_id = $1`, tenant)
	st.idempotency = s.count(`SELECT count(*) FROM idempotency WHERE tenant_id = $1`, tenant)
	return st
}

// scenario is one deployment mutation: prepare brings the stack to the state
// before it and returns the deployment and the mutation itself.
type scenario struct {
	name    string
	evented bool
	// versioned marks a mutation whose replay carries a stale expected version.
	versioned bool
	prepare   func(t *testing.T, s *stack) (delivery.DeploymentID, func() error)
}

func scenarios() []scenario {
	ns := deliverytest.TargetNamespace
	ctx := context.Background()
	engineer := deliverytest.Caller(deliverytest.Engineer)
	return []scenario{
		{"registration", true, false, func(t *testing.T, s *stack) (delivery.DeploymentID, func() error) {
			id := delivery.DeploymentIDFor(scopeA, s.D.Deploy.Context.ActionKey)
			return id, func() error { _, err := s.register(s.D.Deploy, s.Req); return err }
		}},
		{"apply started", true, false, func(t *testing.T, s *stack) (delivery.DeploymentID, func() error) {
			id := s.mustRegister(t).Deployment.ID()
			return id, func() error {
				_, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objects(ns, "api")})
				return err
			}
		}},
		{"settled", true, false, func(t *testing.T, s *stack) (delivery.DeploymentID, func() error) {
			id := s.mustRegister(t).Deployment.ID()
			intended := objects(ns, "api")
			s.apply(t, id, intended)
			return id, func() error {
				_, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)})
				return err
			}
		}},
		{"outcome unknown", false, false, func(t *testing.T, s *stack) (delivery.DeploymentID, func() error) {
			id := s.mustRegister(t).Deployment.ID()
			s.apply(t, id, objects(ns, "api"))
			return id, func() error {
				_, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeUnknown})
				return err
			}
		}},
		{"cleanup requested", false, true, func(t *testing.T, s *stack) (delivery.DeploymentID, func() error) {
			id, _ := s.settled(t)
			cur, err := s.Store.GetDeployment(ctx, scopeA, id)
			if err != nil {
				t.Fatal(err)
			}
			return id, func() error {
				_, err := s.Svc.RequestCleanupAt(ctx, engineer, scopeA, id, cur.Version())
				return err
			}
		}},
		{"cleanup done", false, false, func(t *testing.T, s *stack) (delivery.DeploymentID, func() error) {
			id, intended := s.settled(t)
			if _, err := s.Svc.RequestCleanup(ctx, engineer, scopeA, id); err != nil {
				t.Fatal(err)
			}
			cleaner := s.cleaner(t)
			s.Bindings.Bind(cleaner.Scope, cleaner.Context.RunID, id)
			return id, func() error {
				_, err := s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupDone, Deleted: observed(intended)})
				return err
			}
		}},
	}
}

// Test_delivery_atomicity proves state, version, history, audit, the outbox event
// and the keyed HTTP response commit together or not at all: for every
// deployment mutation, an injected failure of each companion write (and a real
// full event backlog) leaves the record, its version and every companion row
// exactly as they were, the failure is never reported as success, and the same
// request then succeeds once without duplicates.
func Test_delivery_atomicity(t *testing.T) {
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			f := &faults{}
			s := newStack(t, deliverytest.Options{WrapAppender: f.appender}, deliverytest.FlowOptions{})
			f.s = s
			id, do := sc.prepare(t, s)
			before := s.state(id)

			for _, ft := range allFaults() {
				if ft.evented && !sc.evented {
					continue // this transition appends no event
				}
				t.Run(ft.name, func(t *testing.T) {
					ft.arm(f, t)
					err := do()
					ft.disarm(f, t)
					if err == nil {
						t.Fatal("the mutation reported success although a companion write failed")
					}
					if !errors.Is(err, delivery.ErrUnavailable) {
						t.Fatalf("an infrastructure failure must be unavailable (retry), not a business answer: %v", err)
					}
					if after := s.state(id); after != before {
						t.Fatalf("a failed mutation left a partial change:\nbefore %+v\nafter  %+v", before, after)
					}
				})
			}

			// Recovery: the same request now succeeds exactly once.
			if err := do(); err != nil {
				t.Fatalf("the request did not succeed after recovery: %v", err)
			}
			done := s.state(id)
			if !done.exists || done.version != before.version+1 && sc.name != "registration" {
				t.Fatalf("version after recovery: %+v (before %+v)", done, before)
			}
			if done.history != before.history+1 || done.audits != before.audits+1 {
				t.Fatalf("recovery wrote history=%d audits=%d, want one each", done.history-before.history, done.audits-before.audits)
			}
			if want := before.outbox; sc.evented {
				if done.outbox != want+1 {
					t.Fatalf("recovery appended %d events, want one", done.outbox-want)
				}
			} else if done.outbox != want {
				t.Fatalf("a transition without a status change appended an event")
			}
			// Replay after recovery: no duplicates. A version-preconditioned request
			// replays as a stale version, never as a second write.
			if err := do(); sc.versioned {
				is(t, err, delivery.ErrVersionConflict)
			} else if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if again := s.state(id); sc.name == "registration" {
				if again.deployments != done.deployments || again.history != done.history || again.outbox != done.outbox {
					t.Fatalf("a replayed registration duplicated state: %+v -> %+v", done, again)
				}
			} else if again.version != done.version || again.history != done.history || again.outbox != done.outbox {
				t.Fatalf("a replay duplicated state: %+v -> %+v", done, again)
			}
		})
	}

	t.Run("environment state and audit commit together", func(t *testing.T) {
		f := &faults{}
		s := newStack(t, deliverytest.Options{WrapAppender: f.appender}, deliverytest.FlowOptions{})
		f.s = s
		ctx := context.Background()
		engineer := deliverytest.Caller(deliverytest.Engineer)
		audit := triggerFault("audit insert", "audit", "WHEN (NEW.action LIKE 'environment.%')")
		envs := func() int {
			return s.count(`SELECT count(*) FROM environments WHERE tenant_id = $1`, string(scopeA.TenantID))
		}

		// Edit: the version, the values and the audit row move together.
		cfg := map[string]string{"REPLICAS": "4"}
		before := s.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = 'environment.update'`, string(scopeA.TenantID))
		audit.arm(f, t)
		_, err := s.Svc.UpdateEnvironment(ctx, engineer, scopeA, s.D.Env.ID(), s.D.Env.Version(), delivery.EnvironmentEdit{Config: &cfg})
		audit.disarm(f, t)
		is(t, err, delivery.ErrUnavailable)
		cur, err := s.Store.GetEnvironment(ctx, scopeA, s.D.Env.ID())
		if err != nil || cur.Version() != s.D.Env.Version() || cur.Record().Config["REPLICAS"] != "1" {
			t.Fatalf("a failed edit changed the environment: %v %+v", err, cur.Record())
		}
		if n := s.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = 'environment.update'`, string(scopeA.TenantID)); n != before {
			t.Fatalf("audit rows: %d", n)
		}
		edited, err := s.Svc.UpdateEnvironment(ctx, engineer, scopeA, s.D.Env.ID(), s.D.Env.Version(), delivery.EnvironmentEdit{Config: &cfg})
		if err != nil || edited.Version() != s.D.Env.Version()+1 {
			t.Fatalf("the edit did not succeed after recovery: %v", err)
		}
		if n := s.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = 'environment.update'`, string(scopeA.TenantID)); n != before+1 {
			t.Fatalf("audit rows after recovery: %d", n)
		}

		// Creation: no environment without its audit row.
		envsBefore := envs()
		audit.arm(f, t)
		_, err = s.Svc.CreateEnvironment(ctx, engineer, scopeA, delivery.EnvironmentInput{
			Name: "atomic", Kind: delivery.KindStage, Target: "kind", Namespace: "atomic",
			Kubeconfig: s.D.Env.Record().Kubeconfig,
		})
		audit.disarm(f, t)
		is(t, err, delivery.ErrUnavailable)
		if envs() != envsBefore {
			t.Fatal("a failed creation left an environment")
		}
	})

	t.Run("the keyed public creation commits the record and its stored response together", func(t *testing.T) {
		f := &faults{}
		lossy := &deliverytest.LossyDB{}
		s := newStack(t, deliverytest.Options{WrapAppender: f.appender}, deliverytest.FlowOptions{})
		f.s = s
		lossy.Inner = s.X.Runtime
		idem, err := httppg.NewIdempotency(httppg.Config{DB: lossy})
		if err != nil {
			t.Fatal(err)
		}
		keyed, err := deliverypg.NewKeyed(idem, s.Store)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		caller := deliverytest.Caller(deliverytest.Engineer)
		id := delivery.DeploymentIDFor(scopeA, s.D.Deploy.Context.ActionKey)
		identity := func(key string) httpapi.RequestIdentity {
			return httpapi.RequestIdentity{
				Principal: httpapi.Principal{ID: deliverytest.Engineer}, TenantID: scopeA.TenantID, ProjectID: scopeA.ProjectID, Method: http.MethodPost,
				Route: "/api/v1/tenants/" + string(scopeA.TenantID) + "/projects/" + string(scopeA.ProjectID) + "/deployments",
				Key:   httpapi.ClientKey(key), Hash: httpapi.RequestHash{1},
			}
		}
		plan := func() delivery.RegistrationPlan {
			p, err := s.Svc.PlanRegistrationForAttempt(ctx, caller, scopeA, s.D.Deploy.ID(), s.Req)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		runs := 0
		render := func(res delivery.RegisterResult) (httpapi.Outcome, error) {
			runs++
			body := fmt.Sprintf(`{"id":%q,"created":%v}`, res.Deployment.ID(), res.Created)
			return httpapi.Outcome{Response: httpapi.StoredResponse{Status: http.StatusCreated, Body: []byte(body)},
				Target: httpapi.Reference{Kind: "deployment", ID: string(res.Deployment.ID())}}, nil
		}
		allow := func(context.Context, httpapi.Reference) error { return nil }
		run := func(key string) (httpapi.StoredResponse, bool, error) {
			p := plan()
			resp, replayed, _, err := keyed.RegisterKeyed(ctx, identity(key), allow, p.Command, render)
			return resp, replayed, err
		}
		before := s.state(id)

		for _, ft := range allFaults() {
			t.Run(ft.name, func(t *testing.T) {
				ft.arm(f, t)
				_, _, err := run("k-atomic")
				ft.disarm(f, t)
				if err == nil {
					t.Fatal("the keyed creation reported success although a companion write failed")
				}
				if after := s.state(id); after != before {
					t.Fatalf("a failed keyed creation left state (record, history, audit, event or stored response):\nbefore %+v\nafter  %+v", before, after)
				}
			})
		}

		// A lost commit acknowledgment: the commit applied, the client saw an error.
		lossy.Lose()
		_, _, err = run("k-atomic")
		lossy.Off()
		if err == nil {
			t.Fatal("a lost acknowledgment was reported as success")
		}
		committed := s.state(id)
		if committed.deployments != 1 || committed.history != before.history+1 || committed.idempotency != 1 || committed.outbox != before.outbox+1 {
			t.Fatalf("the commit that lost its acknowledgment did not commit everything: %+v", committed)
		}
		// The retry with the same key replays the stored response without running
		// the registration again.
		runsBefore := runs
		resp, replayed, err := run("k-atomic")
		if err != nil || !replayed || runs != runsBefore {
			t.Fatalf("replay: err=%v replayed=%v ran=%v", err, replayed, runs != runsBefore)
		}
		if want := fmt.Sprintf(`{"id":%q,"created":true}`, id); string(resp.Body) != want {
			t.Fatalf("the replay returned %s, want the stored %s", resp.Body, want)
		}
		if after := s.state(id); after != committed {
			t.Fatalf("a replay wrote state: %+v -> %+v", committed, after)
		}
		// A different key for the same logical action observes the original: one
		// deployment, one history row, one event.
		resp2, replayed2, err := run("k-second")
		if err != nil || replayed2 {
			t.Fatalf("second key: %v replayed=%v", err, replayed2)
		}
		if want := fmt.Sprintf(`{"id":%q,"created":false}`, id); string(resp2.Body) != want {
			t.Fatalf("the second key returned %s, want %s", resp2.Body, want)
		}
		final := s.state(id)
		if final.deployments != 1 || final.history != committed.history || final.outbox != committed.outbox || final.idempotency != 2 {
			t.Fatalf("a second key duplicated the logical action: %+v", final)
		}
		// Keyed creation is the human path only: a workload command never runs
		// without the runtime session guard.
		p := plan()
		cmd := p.Command
		cmd.Mutation.Runtime = true
		if _, _, _, err := keyed.RegisterKeyed(ctx, identity("k-runtime"), allow, cmd, render); !errors.Is(err, delivery.ErrDenied) {
			t.Fatalf("a runtime mutation ran through the keyed adapter: %v", err)
		}
		// And the identity must be the deployment's own project.
		other := identity("k-other")
		other.ProjectID = deliverytest.ScopeA2.ProjectID
		if _, _, _, err := keyed.RegisterKeyed(ctx, other, allow, p.Command, render); !errors.Is(err, delivery.ErrDenied) {
			t.Fatalf("a foreign request identity ran: %v", err)
		}
	})
}

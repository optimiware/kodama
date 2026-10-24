package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
)

// spyCredentials counts every resolution of a current credential: a protected
// callback must make none.
type spyCredentials struct {
	inner delivery.Credentials
	calls atomic.Int64
}

func (c *spyCredentials) Current(ctx context.Context, scope domain.Scope, ref string) (delivery.CredentialInfo, error) {
	c.calls.Add(1)
	return c.inner.Current(ctx, scope, ref)
}

func (s *stack) snapshots() int {
	return s.count(`SELECT count(*) FROM environment_snapshots WHERE tenant_id = $1`, string(scopeA.TenantID))
}

// callbacks lists every protected runtime callback as one function of the
// attempt a, for the deployment id.
type callback struct {
	name string
	call func(s *stack, a *deliverytest.Attempt, id delivery.DeploymentID) error
}

func callbacks() []callback {
	ns := deliverytest.TargetNamespace
	return []callback{
		{"register", func(s *stack, a *deliverytest.Attempt, _ delivery.DeploymentID) error {
			_, err := s.register(a, s.Req)
			return err
		}},
		{"lookup", func(s *stack, a *deliverytest.Attempt, _ delivery.DeploymentID) error {
			_, err := s.Svc.LookupByAction(context.Background(), a.ID(), a.Token, a.Context.ActionKey)
			return err
		}},
		{"read", func(s *stack, a *deliverytest.Attempt, id delivery.DeploymentID) error {
			_, err := s.Svc.GetRuntime(context.Background(), a.ID(), a.Token, id)
			return err
		}},
		{"outcome", func(s *stack, a *deliverytest.Attempt, id delivery.DeploymentID) error {
			_, err := s.report(a, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objects(ns, "api")})
			return err
		}},
		{"cleanup", func(s *stack, a *deliverytest.Attempt, id delivery.DeploymentID) error {
			_, err := s.cleanup(a, id, delivery.CleanupReport{State: delivery.CleanupFailed, Reason: "delete_failed"})
			return err
		}},
	}
}

// Test_callback_pin_validation proves the callback half of the environment-pin
// checkpoint with the real record, execution and artifact adapters: every
// protected callback rejects an absent, foreign or mismatched environment or
// credential pin, valid retained pins survive environment edits, credential
// rotation and a restart, and no callback ever falls back to the environment's
// current settings or resolves a current credential.
func Test_callback_pin_validation(t *testing.T) {
	ctx := context.Background()

	// pinned edits of the deploy attempt's context, each a different way for a
	// pin to be absent, foreign or mismatched.
	type variant struct {
		name  string
		apply func(s *stack, c runtimev1.Context) (runtimev1.Context, error)
	}
	variants := []variant{
		{"absent context", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			return runtimev1.Context{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
		}},
		{"no pinned environment", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.Environment = nil
			return c, nil
		}},
		{"foreign tenant", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.TenantID = deliverytest.ScopeB.TenantID
			return c, nil
		}},
		{"foreign project", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.ProjectID = "p035_elsewhere"
			return c, nil
		}},
		{"foreign run", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.RunID = "run_t035_elsewhere"
			return c, nil
		}},
		{"another attempt", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.AttemptID = s.D.Build.ID()
			return c, nil
		}},
		{"a configuration that was never retained", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			pin := *c.Environment
			pin.ConfigDigest = runtimev1.Digest("sha256:" + strings.Repeat("c", 64))
			c.Environment = &pin
			return c, nil
		}},
		{"an environment that does not exist", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			pin := *c.Environment
			pin.ID = "env_t035_nowhere"
			c.Environment = &pin
			return c, nil
		}},
		{"missing credential bindings", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.Secrets = nil
			return c, nil
		}},
		{"a credential version that differs from the retained pin", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.Secrets = append([]runtimev1.SecretBinding(nil), c.Secrets...)
			c.Secrets[0].Version++
			return c, nil
		}},
		{"a credential delivered another way", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.Secrets = append([]runtimev1.SecretBinding(nil), c.Secrets...)
			c.Secrets[0].Delivery = runtimev1.DeliveryEnv
			return c, nil
		}},
		{"a credential under another name", func(s *stack, c runtimev1.Context) (runtimev1.Context, error) {
			c.Secrets = append([]runtimev1.SecretBinding(nil), c.Secrets...)
			c.Secrets[0].Name = "OTHER"
			return c, nil
		}},
	}

	t.Run("every callback rejects an absent, foreign or mismatched pin and writes nothing", func(t *testing.T) {
		spy := &spyCredentials{}
		s := newStack(t, deliverytest.Options{WrapCredentials: func(in delivery.Credentials) delivery.Credentials {
			spy.inner = in
			return spy
		}}, deliverytest.FlowOptions{})
		id, _ := s.settled(t)
		if _, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id); err != nil {
			t.Fatal(err)
		}
		cleaner := s.cleaner(t)
		s.Bindings.Bind(cleaner.Scope, cleaner.Context.RunID, id)

		before := struct{ deployments, history, events, audits, snaps int }{
			s.deployments(), s.history(id), s.events(), s.count(`SELECT count(*) FROM audit WHERE tenant_id = $1`, string(scopeA.TenantID)), s.snapshots()}
		spy.calls.Store(0)
		rec, _ := s.Store.GetDeployment(ctx, scopeA, id)

		for _, v := range variants {
			for _, cb := range callbacks() {
				t.Run(v.name+"/"+cb.name, func(t *testing.T) {
					a := s.D.Deploy
					if cb.name == "cleanup" {
						a = cleaner
					}
					s.Pins.Edit(a.ID(), func(c runtimev1.Context) (runtimev1.Context, error) { return v.apply(s, c) })
					defer s.Pins.Clear()
					err := cb.call(s, a, id)
					if err == nil {
						t.Fatal("the callback accepted a mismatched pin")
					}
					is(t, err, delivery.ErrPinRequired)
				})
			}
		}

		after := struct{ deployments, history, events, audits, snaps int }{
			s.deployments(), s.history(id), s.events(), s.count(`SELECT count(*) FROM audit WHERE tenant_id = $1`, string(scopeA.TenantID)), s.snapshots()}
		if before != after {
			t.Fatalf("a refused callback wrote state: before %+v after %+v", before, after)
		}
		now, _ := s.Store.GetDeployment(ctx, scopeA, id)
		if now.Version() != rec.Version() || now.Record().CleanupState != delivery.CleanupRequested {
			t.Fatalf("a refused callback changed the record: %+v", now.Record())
		}
		if n := spy.calls.Load(); n != 0 {
			t.Fatalf("callbacks resolved %d current credentials", n)
		}
	})

	t.Run("a pin whose environment is another environment's is not this deployment's", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		other := s.Environment(scopeA, "other", "kubeconfig")
		otherPromo := s.Promote(s.D, other) // a real, retained pin of another environment
		// The deploy attempt of the other environment cannot read or report on
		// this environment's deployment even though its own pin is retained and valid.
		for _, cb := range callbacks()[1:4] { // lookup (own key), read, outcome
			if cb.name == "lookup" {
				continue
			}
			err := cb.call(s, otherPromo.Deploy, id)
			if err == nil {
				t.Fatalf("%s: another environment's attempt reached the deployment", cb.name)
			}
			if !errorsIsAny(err, delivery.ErrPinRequired, delivery.ErrDenied) {
				t.Fatalf("%s: %v", cb.name, err)
			}
		}
		// Rewriting this attempt's own pin to the other environment's retained pin
		// is a pin of the same step and action naming a different environment: the
		// recorded deployment is not bound to it.
		s.Pins.Edit(s.D.Deploy.ID(), func(c runtimev1.Context) (runtimev1.Context, error) {
			c.Environment, c.Secrets = otherPromo.Deploy.Context.Environment, otherPromo.Deploy.Context.Secrets
			return c, nil
		})
		defer s.Pins.Clear()
		for _, cb := range callbacks() {
			switch cb.name {
			case "read":
				// A read is also available to a proven cleanup run, so the
				// refusal of an unbound pin may surface as a denial.
				if err := cb.call(s, s.D.Deploy, id); !errorsIsAny(err, delivery.ErrPinRequired, delivery.ErrDenied) {
					t.Fatalf("read: %v", err)
				}
			case "outcome":
				is(t, cb.call(s, s.D.Deploy, id), delivery.ErrPinRequired)
			}
		}
	})

	t.Run("valid retained pins survive edits, rotation and restart without a current-settings fallback", func(t *testing.T) {
		spy := &spyCredentials{}
		s := newStack(t, deliverytest.Options{WrapCredentials: func(in delivery.Credentials) delivery.Credentials {
			spy.inner = in
			return spy
		}}, deliverytest.FlowOptions{})
		pin := s.D.Deploy.Context.Environment
		id := s.mustRegister(t).Deployment.ID()
		intended := objects(deliverytest.TargetNamespace, "api")
		spy.calls.Store(0)
		snapshots := s.snapshots()

		// The environment is edited, its credential rotated, and its namespace
		// moved: none of it touches the pinned configuration.
		cfg, ns := map[string]string{"REPLICAS": "9"}, "elsewhere"
		if _, err := s.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, s.D.Env.ID(), s.D.Env.Version(),
			delivery.EnvironmentEdit{Config: &cfg, Namespace: &ns}); err != nil {
			t.Fatal(err)
		}
		s.X.RotateSecret(scopeA, s.Kube.ID(), "ROTATED")

		s.apply(t, id, intended)
		look, err := s.Svc.LookupByAction(ctx, s.D.Deploy.ID(), s.D.Deploy.Token, s.D.Deploy.Context.ActionKey)
		if err != nil || look.Deployment.ID() != id || look.Deployment.Pin() != *pin {
			t.Fatalf("lookup after edit and rotation: %v", err)
		}

		// A restart: fresh instances, no in-memory state, the same rows.
		svc := s.Restart()
		if _, err := svc.GetRuntime(ctx, s.D.Deploy.ID(), s.D.Deploy.Token, id); err != nil {
			t.Fatalf("read after restart: %v", err)
		}
		done, err := svc.ReportOutcome(ctx, s.D.Deploy.ID(), s.D.Deploy.Token, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)})
		if err != nil || done.Deployment.Status() != delivery.StatusSucceeded || done.Deployment.Pin() != *pin {
			t.Fatalf("settle after restart: %v", err)
		}
		// The retained snapshot is still the admitted one, not the edited environment.
		snap, err := svc.RetainedSnapshot(ctx, scopeA, pin.ID, pin.ConfigDigest)
		if err != nil || snap.Namespace != deliverytest.TargetNamespace || snap.Config["REPLICAS"] != "1" {
			t.Fatalf("retained snapshot: %v %+v", err, snap)
		}
		if got := s.snapshots(); got != snapshots {
			t.Fatalf("callbacks retained %d new snapshots", got-snapshots)
		}
		if n := spy.calls.Load(); n != 0 {
			t.Fatalf("callbacks resolved %d current credentials", n)
		}
		// A retry attempt of the same step carries the run's admitted pin, not the
		// edited environment: it observes the original deployment through the
		// retained configuration.
		retry := s.Retry(s.D)
		if *retry.Context.Environment != *pin {
			t.Fatal("the retry attempt was not pinned to the run's admitted configuration")
		}
		again, err := svc.LookupByAction(ctx, retry.ID(), retry.Token, retry.Context.ActionKey)
		if err != nil || again.Deployment.ID() != id {
			t.Fatalf("retry lookup: %v", err)
		}
	})

	t.Run("without a pinned-context reader every callback is denied", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		s.Pins.Absent(s.D.Deploy.ID())
		for _, cb := range callbacks()[:4] {
			is(t, cb.call(s, s.D.Deploy, id), delivery.ErrPinRequired)
		}
		if s.deployments() != 1 {
			t.Fatal("a denied callback changed the records")
		}
	})
}

func errorsIsAny(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}

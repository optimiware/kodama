package integration_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/storage"
)

func (s *stack) report(a *deliverytest.Attempt, id delivery.DeploymentID, rep delivery.OutcomeReport) (delivery.Receipt, error) {
	return s.Svc.ReportOutcome(context.Background(), a.ID(), a.Token, id, rep)
}

func (s *stack) apply(t *testing.T, id delivery.DeploymentID, objs []delivery.ObjectRef) delivery.Receipt {
	t.Helper()
	rc, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objs})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return rc
}

// Test_deployment_outcome_recovery proves DQ-08 and PX-07's record half: lost
// acknowledgments, restarts, duplicate and conflicting reports and unknown
// outcomes preserve one identity and never imply permission to apply again.
func Test_deployment_outcome_recovery(t *testing.T) {
	ns := deliverytest.TargetNamespace

	t.Run("the lifecycle records intent first and observed readiness last", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		intended := objects(ns, "api", "worker")
		rc := s.apply(t, id, intended)
		if rc.Deployment.Status() != delivery.StatusRunning || rc.Deployment.Reason() != delivery.ReasonApplying || len(rc.Deployment.Record().Objects) != 2 {
			t.Fatalf("after apply start: %+v", rc.Deployment.Record())
		}
		done, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)})
		if err != nil {
			t.Fatal(err)
		}
		rec := done.Deployment.Record()
		if rec.Status != delivery.StatusSucceeded || rec.Reason != delivery.ReasonReady || rec.Version != 3 {
			t.Fatalf("settled: %+v", rec)
		}
		for _, o := range rec.Objects {
			if o.UID == "" || !o.Ready {
				t.Fatalf("an object lacks its observed UID/readiness: %+v", o)
			}
		}
		hist, err := s.Svc.History(context.Background(), deliverytest.Caller(deliverytest.Viewer), scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, h := range hist {
			kinds = append(kinds, h.Kind)
		}
		if len(kinds) != 3 || kinds[0] != "registered" || kinds[1] != "apply_started" || kinds[2] != "succeeded" {
			t.Fatalf("history: %v", kinds)
		}
		if s.events() != 3 {
			t.Fatalf("one deployment.state_changed per status transition, got %d", s.events())
		}
		// The annotation identities never change across the lifecycle.
		if done.Annotations[delivery.DefaultAnnotations.Deployment] != string(id) {
			t.Fatalf("annotations: %v", done.Annotations)
		}
	})

	t.Run("a lost acknowledgment replays the stored receipt", func(t *testing.T) {
		var db *deliverytest.LossyDB
		s := newStack(t, deliverytest.Options{WrapDB: func(in storage.TxBeginner) storage.TxBeginner { db = &deliverytest.LossyDB{Inner: in}; return db }}, deliverytest.FlowOptions{})

		// Registration: the commit applied, its acknowledgment was lost.
		db.Lose()
		_, err := s.register(s.D.Deploy, s.Req)
		is(t, err, delivery.ErrUnavailable)
		db.Off()
		if s.deployments() != 1 {
			t.Fatalf("the commit should have applied: %d", s.deployments())
		}
		rc, err := s.register(s.D.Deploy, s.Req)
		if err != nil || rc.Created {
			t.Fatalf("retry after a lost registration acknowledgment: %v created=%v", err, rc.Created)
		}
		id := rc.Deployment.ID()

		// Apply start, lost then repeated: one transition.
		intended := objects(ns, "api")
		db.Lose()
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: intended})
		is(t, err, delivery.ErrUnavailable)
		db.Off()
		again := s.apply(t, id, intended)
		if again.Deployment.Version() != 2 || s.history(id) != 2 || s.events() != 2 {
			t.Fatalf("a repeated apply start is not a replay: v%d history=%d events=%d", again.Deployment.Version(), s.history(id), s.events())
		}

		// The final outcome, lost and repeated.
		db.Lose()
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)})
		is(t, err, delivery.ErrUnavailable)
		db.Off()
		done, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)})
		if err != nil || done.Deployment.Status() != delivery.StatusSucceeded || done.Deployment.Version() != 3 || s.history(id) != 3 || s.events() != 3 {
			t.Fatalf("settled replay: %v status=%s v%d history=%d events=%d", err, done.Deployment.Status(), done.Deployment.Version(), s.history(id), s.events())
		}
		// A failed (rolled back) commit leaves nothing and the retry applies.
		s2 := newStack(t, deliverytest.Options{WrapDB: func(in storage.TxBeginner) storage.TxBeginner { db = &deliverytest.LossyDB{Inner: in}; return db }}, deliverytest.FlowOptions{})
		db.Fail()
		_, err = s2.register(s2.D.Deploy, s2.Req)
		is(t, err, delivery.ErrUnavailable)
		db.Off()
		if s2.deployments() != 0 || s2.events() != 0 {
			t.Fatal("a failed commit left state")
		}
		if _, err := s2.register(s2.D.Deploy, s2.Req); err != nil || s2.deployments() != 1 {
			t.Fatalf("retry after a failed commit: %v", err)
		}
	})

	t.Run("a restart observes the same identity and state", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		s.apply(t, id, objects(ns, "api"))
		svc := s.Restart()
		look, err := svc.LookupByAction(context.Background(), s.D.Deploy.ID(), s.D.Deploy.Token, s.D.Deploy.Context.ActionKey)
		if err != nil {
			t.Fatal(err)
		}
		if look.Deployment.ID() != id || look.Deployment.Status() != delivery.StatusRunning || len(look.Deployment.Record().Objects) != 1 {
			t.Fatalf("after restart: %+v", look.Deployment.Record())
		}
		got, err := svc.GetRuntime(context.Background(), s.D.Deploy.ID(), s.D.Deploy.Token, id)
		if err != nil || got.Deployment.ID() != id {
			t.Fatalf("get after restart: %v", err)
		}
	})

	t.Run("duplicate reports replay and conflicting reports never rewrite settled history", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		intended := objects(ns, "api")
		s.apply(t, id, intended)
		if rc := s.apply(t, id, intended); rc.Deployment.Version() != 2 {
			t.Fatalf("a duplicate apply start changed the record: v%d", rc.Deployment.Version())
		}
		_, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objects(ns, "api", "extra")})
		is(t, err, delivery.ErrConflict)

		ok := observed(intended)
		if _, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: ok}); err != nil {
			t.Fatal(err)
		}
		dup, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: ok})
		if err != nil || dup.Deployment.Version() != 3 {
			t.Fatalf("duplicate success: %v v%d", err, dup.Deployment.Version())
		}
		other := observed(intended)
		other[0].UID = uid(9)
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: other})
		is(t, err, delivery.ErrConflict)
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "late_failure"})
		is(t, err, delivery.ErrConflict)
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: intended})
		is(t, err, delivery.ErrInvalidTransition)
		if s.history(id) != 3 || s.events() != 3 {
			t.Fatalf("settled history was rewritten: history=%d events=%d", s.history(id), s.events())
		}
		// Even the table owner cannot rewrite a settled outcome or history.
		if err := s.X.OwnerExecErr(`UPDATE deployments SET status = 'failed', reason = 'x_y' WHERE tenant_id = $1`, string(scopeA.TenantID)); err == nil {
			t.Fatal("a settled outcome was rewritten")
		}
		if err := s.X.OwnerExecErr(`UPDATE deployment_history SET kind = 'forged' WHERE tenant_id = $1`, string(scopeA.TenantID)); err == nil {
			t.Fatal("history was rewritten")
		}
		if err := s.X.OwnerExecErr(`DELETE FROM deployment_history WHERE tenant_id = $1`, string(scopeA.TenantID)); err == nil {
			t.Fatal("history was deleted")
		}
	})

	t.Run("an unknown outcome blocks reapply until explicitly reconciled", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		intended := objects(ns, "api", "worker")
		s.apply(t, id, intended)
		unk, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeUnknown})
		if err != nil || unk.Deployment.Status() != delivery.StatusRunning || unk.Deployment.Reason() != delivery.ReasonOutcomeUnknown {
			t.Fatalf("unknown outcome: %v %+v", err, unk.Deployment.Record())
		}
		// Repeating the report, a restart and a retry attempt all see the same.
		if _, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeUnknown}); err != nil {
			t.Fatal(err)
		}
		before := s.history(id)
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: intended})
		is(t, err, delivery.ErrOutcomeUnknown)
		retry := s.Retry(s.D)
		look, err := s.Svc.LookupByAction(context.Background(), retry.ID(), retry.Token, retry.Context.ActionKey)
		if err != nil || !look.Deployment.OutcomeUnknown() {
			t.Fatalf("the retry attempt must observe the unknown outcome: %v", err)
		}
		_, err = s.report(retry, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: intended})
		is(t, err, delivery.ErrOutcomeUnknown)
		if s.history(id) != before {
			t.Fatal("a refused apply wrote history")
		}
		// The database refuses to turn an unknown outcome back into applying.
		if err := s.X.OwnerExecErr(`UPDATE deployments SET reason = 'applying', version = version + 1 WHERE tenant_id = $1`, string(scopeA.TenantID)); err == nil {
			t.Fatal("an unknown outcome returned to applying")
		}
		// Reconciliation: observing the cluster settles it, with the real UIDs.
		fixed, err := s.report(retry, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)})
		if err != nil || fixed.Deployment.Status() != delivery.StatusSucceeded {
			t.Fatalf("reconcile: %v", err)
		}
		_, err = s.report(retry, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: intended})
		is(t, err, delivery.ErrInvalidTransition)
	})

	t.Run("an unknown outcome can be reconciled as failed", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		s.apply(t, id, objects(ns, "api"))
		if _, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeUnknown}); err != nil {
			t.Fatal(err)
		}
		failed, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "objects_absent"})
		if err != nil || failed.Deployment.Status() != delivery.StatusFailed || failed.Deployment.Reason() != "objects_absent" {
			t.Fatalf("reconcile as failed: %v", err)
		}
		// A failed deployment is not reapplied either: that is a new logical action.
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objects(ns, "api")})
		is(t, err, delivery.ErrInvalidTransition)
	})

	t.Run("a deployment that never applied fails without objects", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		_, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "manifest_invalid", Objects: observed(objects(ns, "api"))})
		is(t, err, delivery.ErrDenied) // nothing was recorded, so nothing can be observed
		rc, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "manifest_invalid"})
		if err != nil || rc.Deployment.Status() != delivery.StatusFailed {
			t.Fatalf("pre-apply failure: %v", err)
		}
	})

	t.Run("readiness must be structured and objects must belong to the deployment", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		intended := objects(ns, "api")

		// Success cannot precede the recorded intention.
		_, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)})
		is(t, err, delivery.ErrDenied) // the observed object was never recorded
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded})
		is(t, err, delivery.ErrInvalidTransition)

		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objects("kube-system", "api")})
		is(t, err, delivery.ErrDenied) // another namespace
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying})
		is(t, err, delivery.ErrInvalid)
		s.apply(t, id, intended)

		// An exit code or text is not readiness: every object needs its UID and
		// ready flag.
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded})
		is(t, err, delivery.ErrInvalid)
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: intended})
		is(t, err, delivery.ErrInvalid)
		unready := observed(intended)
		unready[0].Ready = false
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: unready})
		is(t, err, delivery.ErrInvalid)
		stray := observed(objects(ns, "stranger"))
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: stray})
		is(t, err, delivery.ErrDenied)
		badUID := observed(intended)
		badUID[0].UID = "not-a-uid"
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: badUID})
		is(t, err, delivery.ErrInvalid)
		if rc, _ := s.Svc.GetRuntime(context.Background(), s.D.Deploy.ID(), s.D.Deploy.Token, id); rc.Deployment.Status() != delivery.StatusRunning {
			t.Fatal("a refused report changed the status")
		}
	})
}

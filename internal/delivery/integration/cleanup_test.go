package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/execution"
)

// settled registers, applies and succeeds the stack's deployment.
func (s *stack) settled(t *testing.T) (delivery.DeploymentID, []delivery.ObjectRef) {
	t.Helper()
	id := s.mustRegister(t).Deployment.ID()
	intended := objects(deliverytest.TargetNamespace, "api", "worker")
	s.apply(t, id, intended)
	if _, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)}); err != nil {
		t.Fatal(err)
	}
	return id, intended
}

// cleaner is a cleanup run's attempt: an ordinary attempt that pins the
// deployment's environment. The binding to the deployment is separate.
func (s *stack) cleaner(t *testing.T) *deliverytest.Attempt {
	t.Helper()
	return s.Promote(s.D, s.D.Env).Deploy
}

func (s *stack) cleanup(a *deliverytest.Attempt, id delivery.DeploymentID, rep delivery.CleanupReport) (delivery.Receipt, error) {
	return s.Svc.ReportCleanup(context.Background(), a.ID(), a.Token, id, rep)
}

// Test_cleanup_record_boundary proves DQ-09/DQ-10's record half: a persisted
// TTL, repeatable cleanup requests and deletion acknowledgments, retained
// history, visible exceptions, and denial of foreign or unrecorded objects and of
// callbacks without a proven deployment binding. Scheduling and deletion
// belong to T-066/T-067.
func Test_cleanup_record_boundary(t *testing.T) {
	ctx := context.Background()

	t.Run("the effective expiry is persisted once from the pinned snapshot", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		first := s.mustRegister(t)
		rec := first.Deployment.Record()
		if rec.ExpiresAt == nil || rec.ExpiresAt.Sub(rec.CreatedAt) != 24*time.Hour {
			t.Fatalf("expiry %v created %v", rec.ExpiresAt, rec.CreatedAt)
		}
		// Retries and edits never recompute it.
		retry := s.Retry(s.D)
		short := time.Hour
		if _, err := s.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, s.D.Env.ID(), s.D.Env.Version(), delivery.EnvironmentEdit{PreviewTTL: &short}); err != nil {
			t.Fatal(err)
		}
		again, err := s.register(retry, s.Req)
		if err != nil || !again.Deployment.Record().ExpiresAt.Equal(*rec.ExpiresAt) {
			t.Fatalf("a retry recomputed the expiry: %v", err)
		}
		if n := s.count(`SELECT count(*) FROM deployments WHERE tenant_id = $1 AND expires_at = $2`, string(scopeA.TenantID), *rec.ExpiresAt); n != 1 {
			t.Fatal("the persisted expiry changed")
		}
		if err := s.X.OwnerExecErr(`UPDATE deployments SET expires_at = now() WHERE tenant_id = $1`, string(scopeA.TenantID)); err == nil {
			t.Fatal("the expiry was rewritten")
		}
	})

	t.Run("a configured TTL applies and stage environments never expire", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{DefaultPreviewTTL: 6 * time.Hour}, deliverytest.FlowOptions{})
		if _, err := s.Svc.CreateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, delivery.EnvironmentInput{
			Name: "stage", Kind: delivery.KindStage, Target: "kind", Namespace: "stage",
			Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: s.D.Env.Record().Kubeconfig.Ref, Delivery: s.D.Env.Record().Kubeconfig.Delivery},
		}); err != nil {
			t.Fatal(err)
		}
		bad := time.Hour
		_, err := s.Svc.CreateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, delivery.EnvironmentInput{
			Name: "stage2", Kind: delivery.KindStage, Target: "kind", Namespace: "stage", PreviewTTL: &bad,
			Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: s.D.Env.Record().Kubeconfig.Ref, Delivery: s.D.Env.Record().Kubeconfig.Delivery},
		})
		is(t, err, delivery.ErrInvalid)
		preview, err := s.Svc.CreateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, delivery.EnvironmentInput{
			Name: "defaulted", Kind: delivery.KindPreview, Target: "kind", Namespace: "prev",
			Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: s.D.Env.Record().Kubeconfig.Ref, Delivery: s.D.Env.Record().Kubeconfig.Delivery},
		})
		if err != nil || preview.Record().PreviewTTL != 6*time.Hour {
			t.Fatalf("the installation default TTL was not applied: %v", err)
		}
	})

	t.Run("expiry enumeration, requests and deletion acknowledgments are scoped and repeatable", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id, intended := s.settled(t)
		if got, err := s.Svc.ExpiredDeployments(ctx, scopeA, 10); err != nil || len(got) != 0 {
			t.Fatalf("expired before the TTL: %v %d", err, len(got))
		}
		s.X.Clock.Advance(25 * time.Hour)
		got, err := s.Svc.ExpiredDeployments(ctx, scopeA, 10)
		if err != nil || len(got) != 1 || got[0].ID() != id {
			t.Fatalf("expired: %v %d", err, len(got))
		}
		if other, err := s.Svc.ExpiredDeployments(ctx, deliverytest.ScopeB, 10); err != nil || len(other) != 0 {
			t.Fatalf("expiry enumeration crossed tenants: %v %d", err, len(other))
		}

		// Requests: authorized, scoped, idempotent.
		_, err = s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Viewer), scopeA, id)
		is(t, err, delivery.ErrDenied)
		_, err = s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), deliverytest.ScopeB, id)
		is(t, err, delivery.ErrNotFound)
		first, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id)
		if err != nil || again.Deployment.Version() != first.Deployment.Version() || again.Deployment.Record().CleanupState != delivery.CleanupRequested {
			t.Fatalf("a repeated request changed the record: %v", err)
		}
		// A system actor (the scheduler built by the composition root) may request too.
		sys := delivery.Caller{Actor: delivery.Actor{Type: delivery.ActorSystem, ID: "scheduler"}}
		if _, err := s.Svc.RequestCleanup(ctx, sys, scopeA, id); err != nil {
			t.Fatal(err)
		}

		// The execution boundary: only a recorded, requested, undeleted deployment.
		boundary, err := adapters.NewCleanup(s.Svc)
		if err != nil {
			t.Fatal(err)
		}
		if err := boundary.AuthorizeCleanup(ctx, scopeA, string(id)); err != nil {
			t.Fatalf("a requested cleanup was not authorized: %v", err)
		}
		for name, c := range map[string]struct {
			scope  delivery.DeploymentID
			tenant bool
		}{"unknown": {scope: "dep_unknown"}, "foreign scope": {scope: id, tenant: true}} {
			sc := scopeA
			if c.tenant {
				sc = deliverytest.ScopeB
			}
			if err := boundary.AuthorizeCleanup(ctx, sc, string(c.scope)); err != execution.ErrDenied {
				t.Fatalf("%s: %v", name, err)
			}
		}

		cleaner := s.cleaner(t)
		s.Bindings.Bind(cleaner.Scope, cleaner.Context.RunID, id)
		rc, err := s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupDone, Deleted: observed(intended)})
		if err != nil {
			t.Fatal(err)
		}
		rec := rc.Deployment.Record()
		if rec.DeletedAt == nil || rec.CleanupState != delivery.CleanupDone || rec.Status != delivery.StatusSucceeded || rec.Reason != delivery.ReasonReady || len(rec.Objects) != 2 {
			t.Fatalf("deletion must set deleted_at without erasing the outcome: %+v", rec)
		}
		// The acknowledgment is repeatable; later requests and enumeration agree.
		dup, err := s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupDone, Deleted: observed(intended)})
		if err != nil || dup.Deployment.Version() != rc.Deployment.Version() {
			t.Fatalf("a repeated deletion acknowledgment changed state: %v", err)
		}
		if _, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Svc.ExpiredDeployments(ctx, scopeA, 10); len(got) != 0 {
			t.Fatal("a deleted deployment is still enumerated as expired")
		}
		if err := boundary.AuthorizeCleanup(ctx, scopeA, string(id)); err != execution.ErrDenied {
			t.Fatalf("a deleted deployment authorized another cleanup run: %v", err)
		}
		_, err = s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupFailed, Reason: "delete_failed"})
		is(t, err, delivery.ErrConflict)
		// History keeps the whole delivery story; cleanup emitted no status event.
		hist, _ := s.Svc.History(ctx, deliverytest.Caller(deliverytest.Viewer), scopeA, id)
		var kinds []string
		for _, h := range hist {
			kinds = append(kinds, h.Kind)
		}
		want := []string{"registered", "apply_started", "succeeded", "cleanup_requested", "cleanup_done"}
		if len(kinds) != len(want) {
			t.Fatalf("history: %v", kinds)
		}
		for i := range want {
			if kinds[i] != want[i] {
				t.Fatalf("history: %v", kinds)
			}
		}
		if s.events() != 3 {
			t.Fatalf("cleanup changed the status event stream: %d", s.events())
		}
	})

	t.Run("failures and unknown outcomes are kept as visible exceptions and retried", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id, intended := s.settled(t)
		if _, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id); err != nil {
			t.Fatal(err)
		}
		cleaner := s.cleaner(t)
		s.Bindings.Bind(cleaner.Scope, cleaner.Context.RunID, id)
		failed, err := s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupFailed, Reason: "delete_failed", Deleted: observed(intended)[:1]})
		if err != nil || failed.Deployment.Record().CleanupState != delivery.CleanupFailed || failed.Deployment.Record().CleanupReason != "delete_failed" || failed.Deployment.Deleted() {
			t.Fatalf("exception: %v %+v", err, failed.Deployment.Record())
		}
		unk, err := s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupUnknown, Reason: "api_timeout"})
		if err != nil || unk.Deployment.Record().CleanupState != delivery.CleanupUnknown || unk.Deployment.Deleted() {
			t.Fatalf("unknown cleanup: %v", err)
		}
		// A partial deletion is never a completed cleanup.
		_, err = s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupDone, Deleted: observed(intended)[:1]})
		is(t, err, delivery.ErrInvalid)
		done, err := s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupDone, Deleted: observed(intended)})
		if err != nil || !done.Deployment.Deleted() || done.Deployment.Record().CleanupReason != "" {
			t.Fatalf("retried cleanup: %v", err)
		}
		if n := s.history(id); n != 7 { // registered, apply, succeeded, requested, failed, unknown, done
			t.Fatalf("history rows: %d", n)
		}
	})

	t.Run("foreign or unrecorded objects and absent bindings are denied", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id, intended := s.settled(t)
		cleaner := s.cleaner(t)
		report := delivery.CleanupReport{State: delivery.CleanupDone, Deleted: observed(intended)}

		// Before cleanup is requested nothing is deleted, whatever the binding.
		s.Bindings.Bind(cleaner.Scope, cleaner.Context.RunID, id)
		_, err := s.cleanup(cleaner, id, report)
		is(t, err, delivery.ErrInvalidTransition)
		if _, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id); err != nil {
			t.Fatal(err)
		}

		// Unrecorded and foreign-namespace objects.
		stray := observed(append(append([]delivery.ObjectRef(nil), intended...), objects(deliverytest.TargetNamespace, "stranger")...))
		_, err = s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupDone, Deleted: stray})
		is(t, err, delivery.ErrDenied)
		_, err = s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupDone, Deleted: observed(objects("kube-system", "api", "worker"))})
		is(t, err, delivery.ErrDenied)
		wrongUID := observed(intended)
		wrongUID[0].UID = uid(14)
		_, err = s.cleanup(cleaner, id, delivery.CleanupReport{State: delivery.CleanupDone, Deleted: wrongUID})
		is(t, err, delivery.ErrDenied)

		// Environment equality alone is not a binding: an unbound cleanup run,
		// the deployment's own script attempt and a run bound to another
		// deployment are all denied, for reads as well as mutations.
		unbound := s.cleaner(t)
		_, err = s.cleanup(unbound, id, report)
		is(t, err, delivery.ErrDenied)
		_, err = s.Svc.GetRuntime(ctx, unbound.ID(), unbound.Token, id)
		is(t, err, delivery.ErrDenied)
		_, err = s.cleanup(s.D.Deploy, id, report)
		is(t, err, delivery.ErrDenied)
		other := s.cleaner(t)
		s.Bindings.Bind(other.Scope, other.Context.RunID, "dep_somebody_else")
		_, err = s.cleanup(other, id, report)
		is(t, err, delivery.ErrDenied)

		// A properly bound run is admitted before the environment is edited.
		ok := s.cleaner(t)
		s.Bindings.Bind(ok.Scope, ok.Context.RunID, id)

		// A cleanup run whose pinned configuration is not the deployment's
		// retained one is denied even with a binding.
		cfg := map[string]string{"REPLICAS": "7"}
		if _, err := s.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, s.D.Env.ID(), s.D.Env.Version(), delivery.EnvironmentEdit{Config: &cfg}); err != nil {
			t.Fatal(err)
		}
		drift := s.cleaner(t)
		s.Bindings.Bind(drift.Scope, drift.Context.RunID, id)
		if drift.Context.Environment.ConfigDigest == s.D.Deploy.Context.Environment.ConfigDigest {
			t.Fatal("the edited environment was not re-pinned")
		}
		_, err = s.cleanup(drift, id, report)
		is(t, err, delivery.ErrDenied)

		// A foreign tenant's attempt cannot see the deployment at all.
		s.Kubeconfig(deliverytest.ScopeB, "kubeconfig", "FOREIGN")
		envB := s.Environment(deliverytest.ScopeB, "preview", "kubeconfig")
		foreign := s.NewDeploy(deliverytest.ScopeB, envB, deliverytest.FlowOptions{})
		s.Bindings.Bind(foreign.Deploy.Scope, foreign.Deploy.Context.RunID, id)
		_, err = s.cleanup(foreign.Deploy, id, report)
		is(t, err, delivery.ErrNotFound)

		rec, _ := s.Store.GetDeployment(ctx, scopeA, id)
		if rec.Deleted() || rec.Record().CleanupState != delivery.CleanupRequested {
			t.Fatalf("a denied callback changed the cleanup state: %+v", rec.Record())
		}
		// The correctly bound run, pinned before the edit, still succeeds.
		if _, err := s.cleanup(ok, id, report); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the human cleanup request compares the expected version inside the write", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id, _ := s.settled(t)
		cur, err := s.Store.GetDeployment(ctx, scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		engineer := deliverytest.Caller(deliverytest.Engineer)
		_, err = s.Svc.RequestCleanupAt(ctx, engineer, scopeA, id, cur.Version()-1)
		is(t, err, delivery.ErrVersionConflict)
		if n := s.audits("deployment.cleanup_requested"); n != 0 {
			t.Fatalf("a stale request recorded %d cleanups", n)
		}
		_, err = s.Svc.RequestCleanupAt(ctx, deliverytest.Caller(deliverytest.Viewer), scopeA, id, cur.Version())
		is(t, err, delivery.ErrDenied)
		rc, err := s.Svc.RequestCleanupAt(ctx, engineer, scopeA, id, cur.Version())
		if err != nil || rc.Deployment.Version() != cur.Version()+1 || rc.Deployment.Record().CleanupState != delivery.CleanupRequested {
			t.Fatalf("the current version was refused: %v", err)
		}
		// The scheduler-facing form is unconditional and idempotent.
		sys := delivery.Caller{Actor: delivery.Actor{Type: delivery.ActorSystem, ID: "scheduler"}}
		again, err := s.Svc.RequestCleanup(ctx, sys, scopeA, id)
		if err != nil || again.Deployment.Version() != rc.Deployment.Version() {
			t.Fatalf("scheduler request: %v", err)
		}
		// Zero is not a version.
		_, err = s.Svc.RequestCleanupAt(ctx, engineer, scopeA, id, 0)
		is(t, err, delivery.ErrInvalid)
	})

	t.Run("an applying or unsettled deployment is not cleaned up; an unknown outcome is", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		_, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id)
		is(t, err, delivery.ErrInvalidTransition)
		s.apply(t, id, objects(deliverytest.TargetNamespace, "api"))
		_, err = s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id)
		is(t, err, delivery.ErrInvalidTransition)
		if _, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeUnknown}); err != nil {
			t.Fatal(err)
		}
		rc, err := s.Svc.RequestCleanup(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, id)
		if err != nil || rc.Deployment.Record().CleanupState != delivery.CleanupRequested || !rc.Deployment.OutcomeUnknown() {
			t.Fatalf("cleanup of an unknown outcome: %v", err)
		}
	})

	t.Run("the retained configuration and owned objects survive environment edits", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id, intended := s.settled(t)
		cfg := map[string]string{"REPLICAS": "9"}
		ns := "elsewhere"
		if _, err := s.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, s.D.Env.ID(), s.D.Env.Version(), delivery.EnvironmentEdit{Config: &cfg, Namespace: &ns}); err != nil {
			t.Fatal(err)
		}
		v, err := s.Svc.RetainedView(ctx, scopeA, id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Snapshot.Namespace != deliverytest.TargetNamespace || v.Snapshot.Config["REPLICAS"] != "1" || len(v.Objects) != len(intended) ||
			v.Snapshot.Digest != v.Deployment.Record().ConfigDigest {
			t.Fatalf("retained view followed the edited environment: %+v", v.Snapshot)
		}
		if _, err := s.Svc.RetainedView(ctx, deliverytest.ScopeB, id); err == nil {
			t.Fatal("a foreign scope read the retained view")
		}
	})
}

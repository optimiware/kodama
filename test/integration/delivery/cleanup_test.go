package delivery_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

// settledFlow deploys the example application through the full path (the admitted
// step in a Podman container) and returns the flow and its settled record.
func (w *world) settledFlow(name string) (*stack.Flow, delivery.DeploymentRecord) {
	return w.settledFlowReplicas(name, 1)
}

// settledFlowReplicas is settledFlow for an application of the given replica
// count. Tests about which objects a cleanup may touch need the objects, not the
// pods (each pod holds a kernel keyring of the shared host user's small quota), so
// they deploy none.
func (w *world) settledFlowReplicas(name string, replicas int) (*stack.Flow, delivery.DeploymentRecord) {
	w.t.Helper()
	f := w.flow(stack.FlowOptions{Manifests: w.appManifests(replicas, name)})
	w.requireCompleted(w.run(f))
	rec := w.mustDeployment(f)
	if rec.Status != delivery.StatusSucceeded {
		w.t.Fatalf("the deployment is %s/%s", rec.Status, rec.Reason)
	}
	return f, rec
}

// cleanupDefinition registers the scope's ONE project cleanup definition, which
// names no deployment: execution hands each run its own through the deployment_id
// input (T-092).
func (w *world) cleanupDefinition(rec delivery.DeploymentRecord) domain.DefinitionID {
	w.t.Helper()
	return w.S.CleanupDefinition(rec.Scope, w.envOf(rec), w.img)
}

// requireBoundInput proves the deployment_id handoff for a cleanup run: the
// persisted run input, the persisted pinned context of its attempt and the
// environment the script saw are all the deployment the run was admitted for,
// and the definition's step carries no target of its own.
func (w *world) requireBoundInput(scope domain.Scope, run domain.RunID, deployment delivery.DeploymentID) {
	w.t.Helper()
	agg, err := w.S.ExecStore.Get(context.Background(), scope, run)
	if err != nil {
		w.t.Fatal(err)
	}
	if agg.Run.DeploymentID != string(deployment) {
		w.t.Fatalf("run %s is bound to %s, want %s", run, agg.Run.DeploymentID, deployment)
	}
	if got := agg.Run.Snapshot.Values()["deployment_id"]; !got.IsString() || got.String() != string(deployment) {
		w.t.Fatalf("run %s persisted the input %v, want its bound deployment %s", run, got, deployment)
	}
	for _, at := range agg.Attempts {
		c, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
		if err != nil {
			w.t.Fatal(err)
		}
		if got := c.Inputs["deployment_id"]; !got.IsString() || got.String() != string(deployment) {
			w.t.Fatalf("attempt %s was dispatched with the input %v, want %s", at.ID, got, deployment)
		}
	}
}

func (w *world) envOf(rec delivery.DeploymentRecord) delivery.Environment {
	w.t.Helper()
	env, err := w.S.DeliveryStore.GetEnvironment(context.Background(), rec.Scope, rec.Environment)
	if err != nil {
		w.t.Fatal(err)
	}
	return env
}

// expire advances the controlled clock past the preview TTL.
func (w *world) expire() { w.S.X.Clock.Advance(w.ttl + time.Hour) }

func (w *world) sweep(scope domain.Scope) deliverycleanup.Report {
	w.t.Helper()
	rep, err := w.S.Scheduler.Sweep(context.Background(), scope)
	if err != nil {
		w.t.Fatalf("sweep: %v", err)
	}
	return rep
}

// item returns the sweep's decision for the deployment.
func item(rep deliverycleanup.Report, id delivery.DeploymentID) (deliverycleanup.Item, bool) {
	for _, it := range rep.Items {
		if it.Deployment == string(id) {
			return it, true
		}
	}
	return deliverycleanup.Item{}, false
}

// requireAdmitted sweeps the scope and requires that this pass created the
// deployment's cleanup run.
func (w *world) requireAdmitted(scope domain.Scope, id delivery.DeploymentID) deliverycleanup.Item {
	w.t.Helper()
	rep := w.sweep(scope)
	it, ok := item(rep, id)
	if !ok || it.Outcome != deliverycleanup.OutcomeAdmitted {
		w.t.Fatalf("the sweep did not admit the cleanup of %s: %+v", id, rep.Items)
	}
	return it
}

// cleanupRuns counts the scope's persisted cleanup runs of the deployment.
func (w *world) cleanupRuns(scope domain.Scope, id delivery.DeploymentID) int {
	w.t.Helper()
	return w.S.Count(`SELECT count(*) FROM runs WHERE tenant_id = $1 AND project_id = $2 AND purpose = 'cleanup' AND deployment_id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
}

// runCleanup dispatches the admitted cleanup run through the Podman runtime and
// requires the accepted completed result of delete.sh.
func (w *world) runCleanup(scope domain.Scope, run domain.RunID) *stack.Outcome {
	w.t.Helper()
	o := w.S.ExecuteCleanup(scope, run)
	w.requireBoundInput(scope, run, delivery.DeploymentID(w.runDeployment(scope, run)))
	w.requireNoHardcodedTarget(scope, run)
	if !o.Succeeded() {
		w.t.Fatalf("the cleanup attempt is %s/%s reason %q\ncalls:\n%s\ncontainer output:\n%s", o.Attempt.Status, o.Attempt.Reason, o.Reason(), w.srv.Describe(), tail(o.Logs, 3500))
	}
	return o
}

// requireNoHardcodedTarget: the project's cleanup step names no target of its own.
func (w *world) requireNoHardcodedTarget(scope domain.Scope, run domain.RunID) {
	w.t.Helper()
	agg, err := w.S.ExecStore.Get(context.Background(), scope, run)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, at := range agg.Attempts {
		c, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
		if err != nil {
			w.t.Fatal(err)
		}
		if _, hardcoded := c.Command.Env["DEPLOY_DEPLOYMENT_ID"]; hardcoded {
			w.t.Fatal("the cleanup step names its target in its environment instead of consuming the input")
		}
	}
}

func (w *world) runDeployment(scope domain.Scope, run domain.RunID) string {
	w.t.Helper()
	agg, err := w.S.ExecStore.Get(context.Background(), scope, run)
	if err != nil {
		w.t.Fatal(err)
	}
	return agg.Run.DeploymentID
}

func (w *world) cleanupRunStatus(scope domain.Scope, run domain.RunID) execution.RunStatus {
	w.t.Helper()
	agg, err := w.S.ExecStore.Get(context.Background(), scope, run)
	if err != nil {
		w.t.Fatal(err)
	}
	return agg.Run.Status
}

// bystanders creates two objects in the namespace that no deployment of the
// record owns: one unmanaged, one claimed by another deployment. Cleanup must
// leave both alone. It returns their uids.
func (w *world) bystanders() map[string]string {
	w.t.Helper()
	w.ns.admin(w.t, "create", "configmap", "bystander")
	w.ns.admin(w.t, "create", "configmap", "foreign-claim")
	w.ns.admin(w.t, "annotate", "configmap", "foreign-claim", "kodama.optimi.io/deployment-id=dep_someone_else")
	return map[string]string{
		"bystander":     uidOf(w.ns.object(w.t, "configmap", "bystander")),
		"foreign-claim": uidOf(w.ns.object(w.t, "configmap", "foreign-claim")),
	}
}

func (w *world) requireBystanders(uids map[string]string) {
	w.t.Helper()
	for name, uid := range uids {
		if obj := w.ns.object(w.t, "configmap", name); obj == nil || uidOf(obj) != uid {
			w.t.Fatalf("cleanup touched %s, which no deployment of the record owns", name)
		}
	}
}

// Test_deployment_cleanup_join — deployment_cleanup_join (DQ-09/DQ-10, PX-06/
// PX-07): a preview expires by the controlled clock, the real scheduler requests
// and admits the bound cleanup run over the retained configuration, the admitted
// cleanup step runs through the Podman runtime and its real delete script removes
// exactly the recorded objects through the credential of the retained snapshot,
// and the record keeps its outcome, its objects and its history while
// completing deletedAt.
func Test_deployment_cleanup_join(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("expiry_to_deletion_through_the_scheduler_a_bound_run_and_the_real_script", func(t *testing.T) {
		w := newWorld(t, "cjoin", worldOptions{})
		f, rec := w.settledFlow("inventory")
		w.cleanupDefinition(rec)
		by := w.bystanders()
		uids := map[string]string{
			"deployment": uidOf(w.ns.object(t, "deployment", "inventory")),
			"configmap":  uidOf(w.ns.object(t, "configmap", "inventory-config")),
		}
		if rec.ExpiresAt == nil || rec.ExpiresAt.Sub(rec.CreatedAt) != w.ttl {
			t.Fatalf("the persisted expiry is %v from %v", rec.ExpiresAt, rec.CreatedAt)
		}

		// Before the expiry nothing is requested, admitted or deleted.
		if rep := w.sweep(w.scope); len(rep.Items) != 0 {
			t.Fatalf("a sweep before the expiry decided %+v", rep.Items)
		}
		if n := w.cleanupRuns(w.scope, rec.ID); n != 0 {
			t.Fatalf("%d cleanup runs before the expiry", n)
		}

		w.expire()
		w.S.Restart() // the scheduler of another process takes over
		it := w.requireAdmitted(w.scope, rec.ID)
		if it.RunID != execution.CleanupRunID(w.scope, string(rec.ID), 1) || it.Round != 1 {
			t.Fatalf("the run is not the derived identity of round 1: %+v", it)
		}
		requested := w.mustDeployment(f)
		if requested.CleanupState != delivery.CleanupRequested || requested.CleanupRequestedAt == nil || requested.DeletedAt != nil {
			t.Fatalf("expiry must record the cleanup request: %+v", w.S.Describe(w.scope, rec.ID))
		}
		agg, err := w.S.ExecStore.Get(ctx, w.scope, it.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if agg.Run.Purpose != execution.PurposeCleanup || agg.Run.DeploymentID != string(rec.ID) || agg.Run.WorkItemID != "" {
			t.Fatalf("the cleanup run: purpose=%s deployment=%s item=%s", agg.Run.Purpose, agg.Run.DeploymentID, agg.Run.WorkItemID)
		}
		if len(agg.Run.Snapshot.Environments) != 1 || agg.Run.Snapshot.Environments[0].Pin.ConfigDigest != rec.ConfigDigest {
			t.Fatalf("the cleanup run is not bound to the recorded configuration: %+v", agg.Run.Snapshot.Environments)
		}

		o := w.runCleanup(w.scope, it.RunID)
		outs := outputs(o)
		if outs["cleanup_state"] != "done" || outs["deployment_id"] != string(rec.ID) || outs["deleted_count"] != float64(2) {
			t.Fatalf("outputs: %v", outs)
		}
		// The credential reached the container through the runtime route, as the
		// retained snapshot's pinned version, for exactly this attempt.
		if w.secretResolutions(o.Attempt.ID, 1) != 1 {
			t.Fatal("the cleanup attempt did not resolve the retained credential through the runtime route")
		}

		// The cluster: exactly the recorded objects are gone.
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatal("the recorded objects still exist")
		}
		w.requireBystanders(by)
		// The record: deletion completed, the outcome and objects retained.
		after := w.mustDeployment(f)
		if after.CleanupState != delivery.CleanupDone || after.DeletedAt == nil || after.Status != delivery.StatusSucceeded || after.Reason != delivery.ReasonReady {
			t.Fatalf("the record after cleanup: %s", w.S.Describe(w.scope, rec.ID))
		}
		if len(after.Objects) != 2 {
			t.Fatalf("the record dropped its objects: %v", after.Objects)
		}
		for _, ob := range after.Objects {
			if ob.UID != uids[map[string]string{"Deployment": "deployment", "ConfigMap": "configmap"}[ob.Kind]] {
				t.Errorf("the recorded uid of %s changed", ob.Name)
			}
		}
		hist := w.history(rec.ID)
		for _, k := range []string{"registered", "succeeded", "cleanup_requested", "cleanup_unknown", "cleanup_done"} {
			if !contains(hist, k) {
				t.Errorf("the durable history lacks %s: %v", k, hist)
			}
		}
		if st := w.cleanupRunStatus(w.scope, it.RunID); st != execution.RunSucceeded {
			t.Fatalf("the cleanup run is %s", st)
		}
		// Nothing further is scheduled for a deleted deployment.
		if rep := w.sweep(w.scope); len(rep.Items) != 0 {
			t.Fatalf("a deleted deployment was decided again: %+v", rep.Items)
		}
		if n := w.cleanupRuns(w.scope, rec.ID); n != 1 {
			t.Fatalf("%d cleanup runs", n)
		}
	})

	t.Run("a_lost_deletion_acknowledgment_converges_without_another_deleter", func(t *testing.T) {
		w := newWorld(t, "clost", worldOptions{})
		f, rec := w.settledFlowReplicas("inventory", 0)
		w.cleanupDefinition(rec)
		w.expire()
		it := w.requireAdmitted(w.scope, rec.ID)

		// The deletion happens and its completion commits, but the response is
		// lost and the container is gone unobserved: whether the command ran is
		// unknown to the runtime, and no affirmative evidence is available yet.
		w.S.Effects.Withhold(true)
		w.srv.VanishAfter(stack.CleanupResultState("done"))
		if more, err := w.S.Exec.DispatchNext(ctx, w.scope.TenantID); err != nil || !more {
			t.Fatalf("dispatch: %v %v", more, err)
		}
		w.srv.WaitCrash(2 * time.Minute)
		o := w.S.AwaitRun(w.scope, it.RunID, 1)
		w.requireBoundInput(w.scope, it.RunID, rec.ID)
		if o.Attempt.Status != execution.AttemptFailed || o.Attempt.Reason != execution.ReasonOutcomeUnknown {
			t.Fatalf("the cleanup attempt is %s/%s", o.Attempt.Status, o.Attempt.Reason)
		}
		after := w.mustDeployment(f)
		if after.CleanupState != delivery.CleanupDone || after.DeletedAt == nil {
			t.Fatalf("the completion was committed before its acknowledgment was lost: %s", w.S.Describe(w.scope, rec.ID))
		}
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatal("the objects still exist")
		}
		// While the effect is unknown the run is held open: the scheduler starts
		// nothing, whatever the time.
		for i := 0; i < 3; i++ {
			w.S.X.Clock.Advance(time.Hour)
			w.S.Restart()
			w.S.Effects.Withhold(true)
			if rep := w.sweep(w.scope); len(rep.Items) != 0 {
				if d, _ := item(rep, rec.ID); d.Outcome != deliverycleanup.OutcomeDeleted && d.Outcome != deliverycleanup.OutcomeUnknown && d.Outcome != deliverycleanup.OutcomeInFlight {
					t.Fatalf("a pass over an unknown cleanup decided %+v", rep.Items)
				}
			}
		}
		// Evidence from the record settles the attempt; the record, not the failed
		// run, says the objects are gone: no further round is admitted and an
		// explicit reconciliation has nothing to do.
		w.S.Effects.Withhold(false)
		deadline := time.Now().Add(time.Minute)
		for !w.attemptSettled(o.Attempt.ID) {
			if time.Now().After(deadline) {
				t.Fatal("the cleanup attempt was not settled by the record's evidence")
			}
			if _, err := w.S.Exec.ReconcileTenant(ctx, w.scope.TenantID); err != nil {
				t.Logf("reconcile: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if st := w.cleanupRunStatus(w.scope, it.RunID); st != execution.RunFailed {
			t.Fatalf("the cleanup run is %s", st)
		}
		if rep := w.sweep(w.scope); len(rep.Items) != 0 {
			if d, _ := item(rep, rec.ID); d.Outcome != deliverycleanup.OutcomeDeleted {
				t.Fatalf("a deleted deployment was decided again: %+v", rep.Items)
			}
		}
		if _, err := w.S.Scheduler.Reconcile(ctx, w.scope, string(rec.ID)); !errors.Is(err, deliverycleanup.ErrNothingToReconcile) {
			t.Fatalf("reconcile after a completed cleanup: %v", err)
		}
		if n := w.cleanupRuns(w.scope, rec.ID); n != 1 {
			t.Fatalf("%d cleanup runs", n)
		}
	})
}

// Test_cleanup_after_work_done — cleanup_after_work_done (DQ-09, PX-06): the
// preview's expiry is independent of the business work. Whether the work item was
// completed or cancelled, the cleanup run is still admitted, dispatched and
// executed; it has no work item, workspace or retained source, and it changes
// nothing of the work item or its project's work-in-progress.
func Test_cleanup_after_work_done(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		close func(w *world, f *stack.Flow)
	}{
		{"completed", func(w *world, f *stack.Flow) {
			w.S.X.OwnerExec(`UPDATE work_items SET state = 'done', completion = '{}'::jsonb, version = version + 1 WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
				string(w.scope.TenantID), string(w.scope.ProjectID), string(f.Project.Item))
		}},
		{"cancelled", func(w *world, f *stack.Flow) { w.S.X.CancelWork(w.scope, f.Project.Item) }},
	} {
		t.Run("the_work_item_is_"+tc.name, func(t *testing.T) {
			w := newWorld(t, "cwork-"+tc.name[:4], worldOptions{})
			f, rec := w.settledFlowReplicas("inventory", 0)
			w.cleanupDefinition(rec)
			tc.close(w, f)

			type workRow struct {
				state   string
				version int64
			}
			read := func() workRow {
				var r workRow
				w.S.Scalar(&r.state, `SELECT state FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(w.scope.TenantID), string(w.scope.ProjectID), string(f.Project.Item))
				w.S.Scalar(&r.version, `SELECT version FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(w.scope.TenantID), string(w.scope.ProjectID), string(f.Project.Item))
				return r
			}
			active := func() int {
				return w.S.Count(`SELECT count(*) FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND state = 'active'`, string(w.scope.TenantID), string(w.scope.ProjectID))
			}
			before, activeBefore := read(), active()
			refsBefore := w.S.Count(`SELECT count(*) FROM workspace_retained_refs`)
			workRuns := w.S.Count(`SELECT count(*) FROM runs WHERE purpose = 'work'`)

			w.expire()
			it := w.requireAdmitted(w.scope, rec.ID)
			w.runCleanup(w.scope, it.RunID)

			if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
				t.Fatal("the objects of a finished work item were not cleaned up")
			}
			if after := w.mustDeployment(f); after.CleanupState != delivery.CleanupDone || after.DeletedAt == nil {
				t.Fatalf("the record after cleanup: %s", w.S.Describe(w.scope, rec.ID))
			}
			var itemID, wsID *string
			var retention string
			err := w.S.X.Migration.QueryRow(context.Background(), `SELECT work_item_id, workspace_id, source_retention FROM runs WHERE tenant_id = $1 AND id = $2`,
				string(w.scope.TenantID), string(it.RunID)).Scan(&itemID, &wsID, &retention)
			if err != nil {
				t.Fatal(err)
			}
			if itemID != nil || wsID != nil || retention != "not_required" {
				t.Fatalf("the cleanup run is bound to work: item=%v workspace=%v retention=%s", itemID, wsID, retention)
			}
			if got := read(); got != before {
				t.Fatalf("cleanup changed the work item: %+v -> %+v", before, got)
			}
			if got := active(); got != activeBefore {
				t.Fatalf("cleanup changed the project's work in progress: %d -> %d", activeBefore, got)
			}
			if n := w.S.Count(`SELECT count(*) FROM workspace_retained_refs`); n != refsBefore {
				t.Fatalf("cleanup retained source: %d -> %d", refsBefore, n)
			}
			if n := w.S.Count(`SELECT count(*) FROM runs WHERE purpose = 'work'`); n != workRuns {
				t.Fatalf("cleanup created a work run")
			}
		})
	}
}

// Test_cleanup_scope — cleanup_scope (DQ-09/DQ-10, PX-07): cleanup can name no
// resource but the recorded objects of the one deployment its run is bound to.
// Another tenant's deployment, another deployment of the same project, objects no
// deployment owns and objects claimed by someone else are never deleted, even
// when the names are the same and the credential could reach them.
func Test_cleanup_scope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A long TTL: only the explicit request and the final expiry make a deployment
	// due, so the interleaved retries below are the only runnable ones.
	w := newWorld(t, "scopea", worldOptions{ttl: 100 * time.Hour})
	wb := w.addScope(scopeB, "scopeb")

	// Two deployments of project A in one namespace, and one of tenant B whose
	// objects have the very same names as A's first.
	_, recA1 := w.settledFlowReplicas("inventory", 0)
	_, recA2 := w.settledFlowReplicas("billing", 0)
	_, recB := wb.settledFlowReplicas("inventory", 0)
	if recA1.ID == recB.ID {
		t.Fatal("two tenants share a deployment id")
	}
	by := w.bystanders()
	uidsA2 := map[string]string{
		"deployment": uidOf(w.ns.object(t, "deployment", "billing")),
		"configmap":  uidOf(w.ns.object(t, "configmap", "billing-config")),
	}
	uidsB := map[string]string{
		"deployment": uidOf(wb.ns.object(t, "deployment", "inventory")),
		"configmap":  uidOf(wb.ns.object(t, "configmap", "inventory-config")),
	}
	defA1 := w.cleanupDefinition(recA1)

	// Only A's first deployment is asked for cleanup, by a human request (the
	// scheduler would also expire the others).
	if _, err := w.S.Svc.RequestCleanup(ctx, stack.Caller(), w.scope, recA1.ID); err != nil {
		t.Fatal(err)
	}

	t.Run("a_run_cannot_be_admitted_for_another_scopes_or_unrequested_deployment", func(t *testing.T) {
		before := w.S.Count(`SELECT count(*) FROM runs WHERE purpose = 'cleanup'`)
		for name, cmd := range map[string]execution.CleanupCommand{
			"another tenant's deployment":      {Scope: w.scope, DeploymentID: string(recB.ID), DefinitionID: defA1},
			"an unrequested deployment":        {Scope: w.scope, DeploymentID: string(recA2.ID), DefinitionID: defA1},
			"a deployment that does not exist": {Scope: w.scope, DeploymentID: "dep_t067_missing", DefinitionID: defA1},
		} {
			if _, _, err := w.S.Exec.AdmitCleanupRound(ctx, cmd, 1); !errors.Is(err, execution.ErrDenied) && !errors.Is(err, execution.ErrNotFound) && !errors.Is(err, execution.ErrHidden) {
				t.Errorf("%s: got %v", name, err)
			}
		}
		// The deployment is the admission's, never a caller's claim: another
		// deployment supplied as the run input is refused.
		for name, target := range map[string]delivery.DeploymentID{"another deployment of the project": recA2.ID, "another tenant's deployment": recB.ID} {
			cmd := execution.CleanupCommand{Scope: w.scope, DeploymentID: string(recA1.ID), DefinitionID: defA1,
				Inputs: map[string]runtimev1.Value{"deployment_id": runtimev1.StringValue(string(target))}}
			if _, _, err := w.S.Exec.AdmitCleanupRound(ctx, cmd, 1); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Errorf("a supplied deployment_id naming %s: got %v", name, err)
			}
		}
		if _, _, err := w.S.Exec.AdmitCleanupRound(ctx, execution.CleanupCommand{Scope: scopeB, DeploymentID: string(recA1.ID), DefinitionID: defA1}, 1); !errors.Is(err, execution.ErrDenied) && !errors.Is(err, execution.ErrNotFound) && !errors.Is(err, execution.ErrHidden) {
			t.Errorf("another tenant's cleanup of A's deployment: %v", err)
		}
		if n := w.S.Count(`SELECT count(*) FROM runs WHERE purpose = 'cleanup'`); n != before {
			t.Fatalf("%d cleanup runs were created for deployments the scope does not own", n-before)
		}
		if rep := w.sweep(scopeB); len(rep.Items) != 0 {
			t.Fatalf("tenant B has nothing due: %+v", rep.Items)
		}
	})

	t.Run("a_run_bound_to_one_deployment_cannot_read_or_clean_another", func(t *testing.T) {
		// Two cleanup runs are admitted for A's first deployment through a
		// definition whose step overrides the target with another deployment: one
		// of the same project, one of another tenant. (The project definition names
		// no target; this one is the negative case.) The binding is the run's own,
		// not the script's claim.
		for round, target := range map[int]delivery.DeploymentRecord{1: recA2, 2: recB} {
			wrong := w.S.RetargetedCleanupDefinition(w.scope, w.env, w.img, target.ID)
			run, created, err := w.S.Exec.AdmitCleanupRound(ctx, execution.CleanupCommand{Scope: w.scope, DeploymentID: string(recA1.ID), DefinitionID: wrong}, round)
			if err != nil || !created {
				t.Fatalf("admitting round %d: %v %v", round, created, err)
			}
			o := w.S.ExecuteCleanup(w.scope, run.ID)
			if o.Attempt.Status != execution.AttemptFailed {
				t.Fatalf("a run bound to %s cleaned %s: %s/%s", recA1.ID, target.ID, o.Attempt.Status, o.Reason())
			}
			t.Logf("round %d naming %s: %s", round, target.ID, o.Reason())
			w.S.AwaitTerminal(w.scope, run.ID)
			// The run input is still its bound deployment, whatever the step names.
			w.requireBoundInput(w.scope, run.ID, recA1.ID)
			w.S.X.Clock.Advance(time.Hour)
		}
		for kind, uid := range uidsA2 {
			name := map[string]string{"deployment": "billing", "configmap": "billing-config"}[kind]
			if obj := w.ns.object(t, kind, name); obj == nil || uidOf(obj) != uid {
				t.Fatalf("another deployment's %s was touched", kind)
			}
		}
		for kind, uid := range uidsB {
			name := map[string]string{"deployment": "inventory", "configmap": "inventory-config"}[kind]
			if obj := wb.ns.object(t, kind, name); obj == nil || uidOf(obj) != uid {
				t.Fatalf("another tenant's %s was touched", kind)
			}
		}
		if w.mustDeploymentID(recA2.ID).CleanupState != "" || wb.mustDeploymentID(recB.ID).CleanupState != "" {
			t.Fatal("another deployment's cleanup state changed")
		}
		if w.mustDeploymentID(recA1.ID).DeletedAt != nil {
			t.Fatal("the bound deployment was completed by a script that named another")
		}
	})

	t.Run("the_bound_run_deletes_exactly_the_recorded_objects", func(t *testing.T) {
		// The scheduler retries the failed rounds with the project's one definition.
		w.S.X.Clock.Advance(time.Hour)
		rep := w.sweep(w.scope)
		it, ok := item(rep, recA1.ID)
		if !ok || it.Outcome != deliverycleanup.OutcomeAdmitted || it.Round != 3 {
			t.Fatalf("the retry of the failed rounds: %+v", rep.Items)
		}
		w.runCleanup(w.scope, it.RunID)
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatal("the recorded objects still exist")
		}
		w.requireBystanders(by)
		for kind, uid := range uidsA2 {
			name := map[string]string{"deployment": "billing", "configmap": "billing-config"}[kind]
			if obj := w.ns.object(t, kind, name); obj == nil || uidOf(obj) != uid {
				t.Fatalf("another deployment's %s was deleted", kind)
			}
		}
		for kind, uid := range uidsB {
			name := map[string]string{"deployment": "inventory", "configmap": "inventory-config"}[kind]
			if obj := wb.ns.object(t, kind, name); obj == nil || uidOf(obj) != uid {
				t.Fatalf("tenant B's same-named %s was deleted", kind)
			}
		}
		if got := wb.mustDeploymentID(recB.ID); got.CleanupState != "" || got.DeletedAt != nil {
			t.Fatalf("tenant B's record changed: %+v", got)
		}
		// Row-level security: tenant B's application session cannot see A's
		// deployment at all.
		if n := w.S.X.CountTenant(scopeB.TenantID, `SELECT count(*) FROM deployments WHERE id = $1`, string(recA1.ID)); n != 0 {
			t.Fatalf("tenant B's session sees %d of tenant A's deployments", n)
		}
	})

	t.Run("each_scope_cleans_its_own_when_it_expires", func(t *testing.T) {
		// A third deployment of project A: two are now due at once under the one
		// project definition, and so is tenant B's.
		_, recA3 := w.settledFlowReplicas("reports", 0)
		w.expire()
		w.cleanupDefinition(recA2)
		wb.cleanupDefinition(recB) // tenant B's project has its own (identical) project definition
		repA := w.sweep(w.scope)
		repB := w.sweep(scopeB)
		itA2, okA2 := item(repA, recA2.ID)
		itA3, okA3 := item(repA, recA3.ID)
		if !okA2 || itA2.Outcome != deliverycleanup.OutcomeAdmitted || !okA3 || itA3.Outcome != deliverycleanup.OutcomeAdmitted {
			t.Fatalf("project A's two due deployments: %+v", repA.Items)
		}
		itB, okB := item(repB, recB.ID)
		if !okB || itB.Outcome != deliverycleanup.OutcomeAdmitted {
			t.Fatalf("tenant B's deployment: %+v", repB.Items)
		}
		if itA2.RunID == itA3.RunID {
			t.Fatal("two deployments share one cleanup run")
		}
		// Each run persisted its OWN deployment as the input of the one definition.
		w.requireBoundInput(w.scope, itA2.RunID, recA2.ID)
		w.requireBoundInput(w.scope, itA3.RunID, recA3.ID)
		w.requireBoundInput(scopeB, itB.RunID, recB.ID)
		defA2 := w.S.Count(`SELECT count(DISTINCT definition_id) FROM runs WHERE purpose = 'cleanup' AND tenant_id = $1 AND id IN ($2, $3)`,
			string(w.scope.TenantID), string(itA2.RunID), string(itA3.RunID))
		if defA2 != 1 {
			t.Fatalf("the two runs used %d definitions, want the one project definition", defA2)
		}

		w.runCleanup(scopeB, itB.RunID)
		for i := 0; i < 2; i++ {
			o, run := w.S.ExecuteAny(w.scope, []domain.RunID{itA2.RunID, itA3.RunID})
			if !o.Succeeded() {
				t.Fatalf("run %s: %s/%s\n%s", run, o.Attempt.Status, o.Reason(), tail(o.Logs, 2000))
			}
			want := recA2.ID
			if run == itA3.RunID {
				want = recA3.ID
			}
			if outputs(o)["deployment_id"] != string(want) || outputs(o)["cleanup_state"] != "done" {
				t.Fatalf("run %s cleaned %v, want %s: %v", run, outputs(o)["deployment_id"], want, outputs(o))
			}
		}
		if wb.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "deployment", "billing") != nil || w.ns.object(t, "deployment", "reports") != nil {
			t.Fatal("expired deployments were not cleaned up")
		}
		for _, id := range []delivery.DeploymentID{recA2.ID, recA3.ID} {
			if w.mustDeploymentID(id).DeletedAt == nil {
				t.Fatalf("%s was not completed", id)
			}
		}
		w.requireBystanders(by)
	})
}

func (w *world) cleanupAttemptCount(run domain.RunID) int {
	return w.S.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run))
}

// Test_cleanup_duplicate_claim — cleanup_duplicate_claim (DQ-10, PX-07): any
// number of schedulers, restarts and explicit reconciliations claiming the same
// round converge on one run with one deleter; a cleanup whose outcome is unknown
// is never retried by a pass and is followed up only by the explicit
// reconciliation, which admits one further round that observes the recorded
// objects.
func Test_cleanup_duplicate_claim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("concurrent_claims_of_one_round_create_one_run_and_one_dispatch", func(t *testing.T) {
		w := newWorld(t, "cdup", worldOptions{})
		f, rec := w.settledFlowReplicas("inventory", 0)
		w.cleanupDefinition(rec)
		w.expire()

		const claimants = 4
		reports := make([]deliverycleanup.Report, claimants)
		sched := make([]*deliverycleanup.Scheduler, claimants)
		for i := range sched {
			sched[i] = w.S.NewScheduler()
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range sched {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rep, err := sched[i].Sweep(ctx, w.scope)
				if err != nil {
					t.Errorf("scheduler %d: %v", i, err)
				}
				reports[i] = rep
			}()
		}
		close(start)
		wg.Wait()
		admitted, converged, raced := 0, 0, 0
		for _, rep := range reports {
			it, ok := item(rep, rec.ID)
			switch {
			case !ok:
				t.Fatalf("a scheduler did not decide the deployment: %+v", rep.Items)
			case it.Outcome == deliverycleanup.OutcomeAdmitted:
				admitted++
			case it.Outcome == deliverycleanup.OutcomeDuplicate || it.Outcome == deliverycleanup.OutcomeInFlight:
				converged++
			case it.Outcome == deliverycleanup.OutcomeError && isRunKeyRace(it.Err):
				// The loser of a simultaneous insert sees the primary key of the
				// run (the derived identity) refuse its transaction, which rolled
				// back entirely. It is reported as retryable; the next pass
				// converges on the stored run (below).
				raced++
			default:
				t.Fatalf("unexpected decision: %+v", it)
			}
		}
		if admitted != 1 || admitted+converged+raced != claimants {
			t.Fatalf("%d schedulers admitted, %d converged, %d raced: want exactly one admission", admitted, converged, raced)
		}
		t.Logf("%d claimants: %d admitted, %d converged on the stored run, %d lost the insert race", claimants, admitted, converged, raced)
		runID := execution.CleanupRunID(w.scope, string(rec.ID), 1)
		if n := w.cleanupRuns(w.scope, rec.ID); n != 1 {
			t.Fatalf("%d cleanup runs for one round", n)
		}
		if n := w.cleanupAttemptCount(runID); n != 0 {
			t.Fatalf("a run that was only admitted has %d attempts", n)
		}
		// The loser's rolled-back transaction left no second pin behind.
		if n := w.S.Count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1 AND released_at IS NULL`, string(runID)); n != 1 {
			t.Fatalf("%d credential pins for the one cleanup run", n)
		}
		for i, sch := range sched {
			// Every claimant's next pass converges on the stored run.
			rep, err := sch.Sweep(ctx, w.scope)
			if err != nil {
				t.Fatal(err)
			}
			it, ok := item(rep, rec.ID)
			if !ok || it.Outcome != deliverycleanup.OutcomeInFlight {
				t.Fatalf("scheduler %d on its next pass: %+v", i, it)
			}
		}

		// A restart and a later pass see the round in flight and start nothing.
		w.S.Restart()
		it, _ := item(w.sweep(w.scope), rec.ID)
		if it.Outcome != deliverycleanup.OutcomeInFlight || w.cleanupRuns(w.scope, rec.ID) != 1 {
			t.Fatalf("a later pass: %+v", it)
		}

		// One dispatch only: after the run's one attempt is started a second
		// dispatch finds nothing runnable.
		n := w.S.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(runID))
		o := w.runCleanup(w.scope, it.RunID)
		if n != 0 || w.cleanupAttemptCount(runID) != 1 {
			t.Fatalf("the round has %d attempts", w.cleanupAttemptCount(runID))
		}
		if _, err := w.S.X.Claim(w.scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a second dispatch of the same round: %v", err)
		}
		_ = o
		if w.ns.object(t, "deployment", "inventory") != nil {
			t.Fatal("the objects still exist")
		}
		if after := w.mustDeployment(f); after.DeletedAt == nil {
			t.Fatalf("the record after cleanup: %s", w.S.Describe(w.scope, rec.ID))
		}
		// The same round claimed again after completion returns the stored run.
		defID, _ := w.S.Defs.CleanupDefinition(ctx, w.scope)
		run, created, err := w.S.Exec.AdmitCleanupRound(ctx, execution.CleanupCommand{Scope: w.scope, DeploymentID: string(rec.ID), DefinitionID: defID}, 1)
		if err != nil || created || run.ID != it.RunID {
			t.Fatalf("a repeated claim of the round: created=%v err=%v", created, err)
		}
	})

	t.Run("an_unknown_outcome_is_followed_up_only_by_one_explicit_reconciliation", func(t *testing.T) {
		w := newWorld(t, "cunk", worldOptions{})
		f, rec := w.settledFlowReplicas("inventory", 0)
		w.cleanupDefinition(rec)
		w.expire()
		it := w.requireAdmitted(w.scope, rec.ID)

		// Round 1 deletes the objects, then its container is gone before it could
		// report the completion: the record keeps the visible exception "unknown"
		// (delete_started was reported first).
		w.S.Effects.Withhold(true)
		w.srv.VanishBefore(stack.CleanupResultState("done"))
		if more, err := w.S.Exec.DispatchNext(ctx, w.scope.TenantID); err != nil || !more {
			t.Fatalf("dispatch: %v %v", more, err)
		}
		w.srv.WaitCrash(2 * time.Minute)
		first := w.S.AwaitRun(w.scope, it.RunID, 1)
		w.requireBoundInput(w.scope, it.RunID, rec.ID)
		if first.Attempt.Reason != execution.ReasonOutcomeUnknown {
			t.Fatalf("round 1 ended %s/%s", first.Attempt.Status, first.Attempt.Reason)
		}
		w.S.Effects.Withhold(false)
		deadline := time.Now().Add(time.Minute)
		for !w.attemptSettled(first.Attempt.ID) {
			if time.Now().After(deadline) {
				t.Fatal("round 1 was not settled by the record's evidence")
			}
			if _, err := w.S.Exec.ReconcileTenant(ctx, w.scope.TenantID); err != nil {
				t.Logf("reconcile: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		got := w.mustDeployment(f)
		if got.CleanupState != delivery.CleanupUnknown || got.DeletedAt != nil {
			t.Fatalf("the unknown outcome must stay visible: %s", w.S.Describe(w.scope, rec.ID))
		}
		if w.ns.object(t, "deployment", "inventory") != nil {
			t.Fatal("round 1 was expected to have deleted the objects before it was lost")
		}

		// No pass retries it, whatever the time.
		for _, wait := range []time.Duration{0, time.Hour, 48 * time.Hour} {
			w.S.X.Clock.Advance(wait)
			w.S.Restart()
			it2, ok := item(w.sweep(w.scope), rec.ID)
			if !ok || it2.Outcome != deliverycleanup.OutcomeUnknown {
				t.Fatalf("a pass over an unknown outcome decided %+v", it2)
			}
		}
		if n := w.cleanupRuns(w.scope, rec.ID); n != 1 {
			t.Fatalf("%d cleanup runs after the passes", n)
		}

		// Two operators reconcile at once: one further round, one run.
		const claimants = 2
		items := make([]deliverycleanup.Item, claimants)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range items {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				s := w.S.NewScheduler()
				items[i], _ = s.Reconcile(ctx, w.scope, string(rec.ID))
			}()
		}
		close(start)
		wg.Wait()
		admitted := 0
		for _, i := range items {
			if i.Round != 2 || i.RunID != execution.CleanupRunID(w.scope, string(rec.ID), 2) {
				t.Fatalf("reconcile decided %+v", i)
			}
			if i.Outcome == deliverycleanup.OutcomeAdmitted {
				admitted++
			}
		}
		if admitted != 1 || w.cleanupRuns(w.scope, rec.ID) != 2 {
			t.Fatalf("%d reconciliations admitted round 2; %d runs", admitted, w.cleanupRuns(w.scope, rec.ID))
		}

		// Round 2 observes the recorded objects, finds them gone and completes.
		w.runCleanup(w.scope, execution.CleanupRunID(w.scope, string(rec.ID), 2))
		after := w.mustDeployment(f)
		if after.CleanupState != delivery.CleanupDone || after.DeletedAt == nil {
			t.Fatalf("round 2: %s", w.S.Describe(w.scope, rec.ID))
		}
		if _, err := w.S.Scheduler.Reconcile(ctx, w.scope, string(rec.ID)); !errors.Is(err, deliverycleanup.ErrNothingToReconcile) {
			t.Fatalf("reconcile after completion: %v", err)
		}
	})
}

// isRunKeyRace reports whether err is the unique violation of the run's primary
// key, the one outcome of two transactions inserting the same derived run id.
func isRunKeyRace(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "runs_pkey"
}

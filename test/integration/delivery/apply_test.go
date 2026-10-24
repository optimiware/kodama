package delivery_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

// history returns the deployment's durable history receipts, in order.
func (w *world) history(id delivery.DeploymentID) []string {
	w.t.Helper()
	hs, err := w.S.DeliveryStore.History(context.Background(), w.scope, id)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, h := range hs {
		out = append(out, h.Kind)
	}
	return out
}

func (w *world) deploymentRows(f *stack.Flow) int {
	w.t.Helper()
	return w.S.Count(`SELECT count(*) FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND action_key = $3`,
		string(f.Scope.TenantID), string(f.Scope.ProjectID), execution.ActionKey(f.Run.ID, "deploy"))
}

func contains(haystack []string, needle string) bool { return countOf(haystack, needle) > 0 }

func countOf(haystack []string, needle string) int {
	n := 0
	for _, h := range haystack {
		if h == needle {
			n++
		}
	}
	return n
}

func (w *world) attemptSettled(attempt domain.AttemptID) bool {
	w.t.Helper()
	return w.S.Count(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND settled_at IS NOT NULL`, string(attempt)) == 1
}

// vanishedAttempt dispatches the flow's attempt and waits until a fault killed and
// removed its container before the control plane ever observed it running, then
// lets the control plane see only that the workload is gone. Affirmative effect
// evidence is withheld meanwhile. The runtime's launch evidence for the attempt
// is "unknown": it cannot tell whether the command ran.
func (w *world) vanishedAttempt(f *stack.Flow) *stack.Outcome {
	w.t.Helper()
	w.S.Effects.Withhold(true)
	n := w.S.Dispatch(f)
	w.srv.WaitCrash(2 * time.Minute)
	return w.S.Await(f, n)
}

// requireBlocked asserts that an attempt with an uncertain effect blocks automatic
// retry however many passes run and however much time passes: it is failed with
// the ambiguous reason, unsettled, its step is not failed or retried, no further
// attempt exists or can be dispatched, and the control plane did ask the effect
// adapter, which gave no affirmative evidence.
func (w *world) requireBlocked(f *stack.Flow, o *stack.Outcome) {
	w.t.Helper()
	if o.Attempt.Status != execution.AttemptFailed || o.Attempt.Reason != execution.ReasonOutcomeUnknown {
		w.t.Fatalf("the attempt of a vanished workload is %s/%s, want failed/%s", o.Attempt.Status, o.Attempt.Reason, execution.ReasonOutcomeUnknown)
	}
	for i := 0; i < 4; i++ {
		w.S.X.Clock.Advance(time.Minute)
		w.S.Reconcile(f)
	}
	agg, err := w.S.ExecStore.Get(context.Background(), f.Scope, f.Run.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	step := agg.Step("deploy")
	if w.attemptSettled(o.Attempt.ID) || len(agg.Attempts) != 1 || step.Status != execution.StepRunning || agg.Run.Status.Terminal() {
		w.t.Fatalf("an uncertain effect did not block: settled=%v attempts=%d step=%s run=%s",
			w.attemptSettled(o.Attempt.ID), len(agg.Attempts), step.Status, agg.Run.Status)
	}
	if more, err := w.S.Exec.DispatchNext(context.Background(), f.Scope.TenantID); err != nil || more {
		w.t.Fatalf("a further attempt was dispatched while the effect of the first is unknown: more=%v err=%v", more, err)
	}
	if len(w.S.Effects.Calls()) == 0 {
		w.t.Fatal("the control plane never asked the effect adapter")
	}
}

// recover releases the effect adapter, which now derives evidence from the
// deployment record, and drives the repair passes until the vanished attempt is
// settled as replay-safe and the step's next attempt is due; it then runs that
// attempt through the Podman runtime.
func (w *world) recover(f *stack.Flow, failed *stack.Outcome) *stack.Outcome {
	w.t.Helper()
	w.S.Effects.Withhold(false)
	deadline := time.Now().Add(time.Minute)
	for !w.attemptSettled(failed.Attempt.ID) {
		if time.Now().After(deadline) {
			w.t.Fatalf("the attempt was not settled after the adapter answered: effect queries %+v", w.S.Effects.Calls())
		}
		w.S.Reconcile(f)
		time.Sleep(200 * time.Millisecond)
	}
	var ev int
	w.S.Scalar(&ev, `SELECT count(*) FROM attempt_evidence WHERE attempt_id = $1 AND kind = 'reconciliation' AND detail->>'verdict' = 'replay_safe' AND detail->>'source' = $2`,
		string(failed.Attempt.ID), stack.EffectSource)
	if ev == 0 {
		w.t.Fatal("the settlement did not record the adapter's replay-safe reconciliation evidence")
	}
	w.S.X.Clock.Advance(5 * time.Second) // past the step's retry backoff
	return w.S.Execute(f)
}

// Test_apply_ack_loss — apply_ack_loss (DQ-07–DQ-10, PX-06/PX-07): the admitted
// apply step runs through the Podman runtime with the real entrypoint against the
// real kind cluster and the real PostgreSQL record, and loses the
// acknowledgment of each of its record calls, or its whole container, in turn.
// The intent is durable before anything is applied; an unknown response
// reconciles the recorded annotations instead of repeating the apply; an attempt
// whose effect is uncertain blocks automatic retry until evidence from the
// deployment record permits the next attempt, which settles the one deployment;
// readiness is the observed state of the live objects, never text on stdout.
func Test_apply_ack_loss(t *testing.T) {
	t.Parallel()

	t.Run("a_lost_intent_acknowledgment_and_a_vanished_workload_apply_nothing_and_the_permitted_retry_resumes_the_record", func(t *testing.T) {
		w := newWorld(t, "intent", worldOptions{})
		f := w.flow(stack.FlowOptions{Manifests: w.appManifests(0, "inventory")})

		// The intent commits, its response is lost, and the container is gone
		// before anything observed it.
		w.srv.VanishAfter(stack.OutcomeState("applying"))
		first := w.vanishedAttempt(f)
		rec := w.mustDeployment(f)
		if rec.Status != delivery.StatusRunning || rec.Reason != delivery.ReasonApplying || len(rec.Objects) != 2 {
			t.Fatalf("the intent must be durable before apply: %s/%s %v\n%s", rec.Status, rec.Reason, rec.Objects, w.S.Describe(f.Scope, rec.ID))
		}
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatal("objects were applied although the intent was never acknowledged")
		}
		w.requireBlocked(f, first)

		// A restart of the control plane changes nothing; then the record's own
		// evidence permits the next attempt, which resumes the same deployment.
		w.S.Restart()
		second := w.recover(f, first)
		w.requireCompleted(second)
		after := w.mustDeployment(f)
		if after.ID != rec.ID || after.Status != delivery.StatusSucceeded || after.Reason != delivery.ReasonReady {
			t.Fatalf("the retry must settle the same deployment: %s %s/%s", after.ID, after.Status, after.Reason)
		}
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d deployment rows for one logical action", n)
		}
		if n := w.countObjects(string(rec.ID)); n != 2 {
			t.Fatalf("%d objects carry the deployment id, want 2", n)
		}
		w.requireAnnotated(after, "deployment", "inventory")
		if got := w.history(rec.ID); !contains(got, "apply_started") || countOf(got, "succeeded") != 1 {
			t.Fatalf("the durable receipts of the transitions: %v", got)
		}
		if st := w.runStatus(f); st != execution.RunSucceeded {
			t.Fatalf("the run is %s after the accepted result", st)
		}
		if outputs(second)["deployment_id"] != string(rec.ID) {
			t.Fatalf("the accepted result names %v, not the recorded deployment", outputs(second)["deployment_id"])
		}
		// The retained credential was resolved for each attempt that got that far.
		if w.secretResolutions(second.Attempt.ID, 1) != 1 {
			t.Fatal("the retry did not resolve the pinned credential through the runtime route")
		}
	})

	t.Run("a_success_report_lost_before_it_was_processed_is_reconciled_by_annotation", func(t *testing.T) {
		w := newWorld(t, "lostreq", worldOptions{})
		f := w.flow(stack.FlowOptions{})

		// The objects are applied and observed ready; the report never reaches the
		// server and the container is gone: the record still says running.
		w.srv.VanishBefore(stack.OutcomeState("succeeded"))
		first := w.vanishedAttempt(f)
		rec := w.mustDeployment(f)
		if rec.Status != delivery.StatusRunning {
			t.Fatalf("the record must still be running, is %s/%s", rec.Status, rec.Reason)
		}
		dep := w.ns.object(t, "deployment", "inventory")
		cm := w.ns.object(t, "configmap", "inventory-config")
		if dep == nil || cm == nil {
			t.Fatal("the objects were not applied")
		}
		uids := map[string]string{"deployment": uidOf(dep), "configmap": uidOf(cm)}
		w.requireBlocked(f, first)

		second := w.recover(f, first)
		w.requireCompleted(second)
		after := w.mustDeployment(f)
		if after.ID != rec.ID || after.Status != delivery.StatusSucceeded {
			t.Fatalf("the retry must settle the same deployment: %s %s", after.ID, after.Status)
		}
		// The same live objects: nothing was recreated, and the settled record
		// names the uids the annotations found.
		if got := uidOf(w.ns.object(t, "deployment", "inventory")); got != uids["deployment"] {
			t.Errorf("the Deployment was recreated: %s -> %s", uids["deployment"], got)
		}
		if got := uidOf(w.ns.object(t, "configmap", "inventory-config")); got != uids["configmap"] {
			t.Errorf("the ConfigMap was recreated: %s -> %s", uids["configmap"], got)
		}
		for _, o := range after.Objects {
			want := uids[strings.ToLower(o.Kind)]
			if o.UID != want || !o.Ready {
				t.Errorf("the settled record names %s uid=%s ready=%v, live uid is %s", o.Name, o.UID, o.Ready, want)
			}
		}
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d deployment rows", n)
		}
	})

	t.Run("a_success_report_lost_after_it_was_processed_is_only_verified", func(t *testing.T) {
		w := newWorld(t, "lostresp", worldOptions{})
		f := w.flow(stack.FlowOptions{Manifests: w.appManifests(0, "inventory")})

		w.srv.VanishAfter(stack.OutcomeState("succeeded"))
		first := w.vanishedAttempt(f)
		rec := w.mustDeployment(f)
		if rec.Status != delivery.StatusSucceeded || rec.Reason != delivery.ReasonReady {
			t.Fatalf("the success was committed before its acknowledgment was lost: %s/%s", rec.Status, rec.Reason)
		}
		cm := resourceVersion(w.ns.object(t, "configmap", "inventory-config"))
		dep := resourceVersion(w.ns.object(t, "deployment", "inventory"))
		w.requireBlocked(f, first)

		mark := w.srv.Mark()
		second := w.recover(f, first)
		w.requireCompleted(second)
		calls := w.srv.Since(mark)
		if n := stack.CountCalls(calls, "POST", "/outcome"); n != 0 {
			t.Fatalf("the retry reported %d outcomes for a settled action: %v", n, stack.OutcomeStates(calls))
		}
		if resourceVersion(w.ns.object(t, "configmap", "inventory-config")) != cm || resourceVersion(w.ns.object(t, "deployment", "inventory")) != dep {
			t.Fatal("the retry re-applied a settled deployment")
		}
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d deployment rows", n)
		}
		if got := w.history(rec.ID); countOf(got, "succeeded") != 1 {
			t.Fatalf("the settled outcome was recorded again: %v", got)
		}
		if outputs(second)["deployment_id"] != string(rec.ID) || w.runStatus(f) != execution.RunSucceeded {
			t.Fatalf("the replay is not the original deployment's accepted result: %v", outputs(second))
		}
	})

	t.Run("one_lost_response_is_absorbed_by_the_scripts_own_idempotent_retry", func(t *testing.T) {
		w := newWorld(t, "oneloss", worldOptions{})
		f := w.flow(stack.FlowOptions{Manifests: w.appManifests(0, "inventory")})
		w.srv.DropResponseOnce(stack.OutcomeState("succeeded"))
		o := w.run(f)
		w.requireCompleted(o)
		states := stack.OutcomeStates(w.srv.Since(0))
		if countOf(states, "succeeded(lost response)") != 1 || countOf(states, "succeeded") != 1 {
			t.Fatalf("the report was not lost once and then acknowledged: %v", states)
		}
		if len(o.Agg.Attempts) != 1 || w.deploymentRows(f) != 1 || w.runStatus(f) != execution.RunSucceeded {
			t.Fatalf("attempts=%d rows=%d run=%s", len(o.Agg.Attempts), w.deploymentRows(f), w.runStatus(f))
		}
	})

	t.Run("an_unknown_outcome_is_settled_by_observing_the_recorded_objects_never_by_applying", func(t *testing.T) {
		w := newWorld(t, "unknown", worldOptions{})
		f := w.flow(stack.FlowOptions{Manifests: w.appManifests(0, "inventory")})

		// The apply happens, the success report is lost and the container is gone.
		w.srv.VanishBefore(stack.OutcomeState("succeeded"))
		first := w.vanishedAttempt(f)
		rec := w.mustDeployment(f)
		// The record states the outcome unknown (fault injection of the record's
		// state: the tool reports it itself only for an apply it cannot vouch for).
		w.markUnknown(f)
		if got := w.mustDeployment(f); got.Status != delivery.StatusRunning || got.Reason != delivery.ReasonOutcomeUnknown {
			t.Fatalf("the record must be the visible unknown outcome: %s/%s", got.Status, got.Reason)
		}
		// A live change a repeated apply would revert: the replica count (the
		// manifests ask for none).
		w.ns.admin(t, "scale", "deployment/inventory", "--replicas=1")
		eventually(t, 120*time.Second, "one replica", func() (bool, string) {
			out := w.ns.admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.status.readyReplicas}")
			return out == "1", out
		})
		w.requireBlocked(f, first)

		second := w.recover(f, first)
		w.requireCompleted(second)
		after := w.mustDeployment(f)
		if after.ID != rec.ID || after.Status != delivery.StatusSucceeded {
			t.Fatalf("the unknown outcome was not settled by observation: %s/%s", after.Status, after.Reason)
		}
		if got := w.ns.admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.spec.replicas}"); got != "1" {
			t.Fatalf("the unknown outcome was settled by applying again: replicas %s", got)
		}
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d deployment rows", n)
		}
	})

	t.Run("an_unknown_outcome_with_nothing_applied_fails_and_is_never_applied", func(t *testing.T) {
		w := newWorld(t, "nothing", worldOptions{})
		f := w.flow(stack.FlowOptions{Manifests: w.appManifests(0, "inventory")})

		w.srv.VanishAfter(stack.OutcomeState("applying"))
		first := w.vanishedAttempt(f)
		w.markUnknown(f)
		w.requireBlocked(f, first)

		second := w.recover(f, first)
		w.requireFailed(second, "apply_not_observed")
		after := w.mustDeployment(f)
		if after.Status != delivery.StatusFailed || after.Reason != "apply_not_observed" {
			t.Fatalf("settled by observation: %s/%s", after.Status, after.Reason)
		}
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatal("the script applied over an unknown outcome")
		}
		if st := w.runStatus(f); st != execution.RunFailed {
			t.Fatalf("the run is %s", st)
		}
	})

	t.Run("a_crash_the_control_plane_saw_start_is_never_retried_and_leaves_the_record_visible", func(t *testing.T) {
		w := newWorld(t, "started", worldOptions{})
		w.S.Effects.Withhold(false)
		f := w.flow(stack.FlowOptions{Manifests: w.appManifests(0, "inventory")})

		// The command was seen running, then died after the intent committed. The
		// runtime's evidence is "started": that is a possible effect, so the step
		// is not retried (the supported recovery needs evidence that the effect
		// did not happen, and none is derivable from "started").
		w.srv.CrashAfter(stack.OutcomeState("applying"))
		n := w.S.Dispatch(f)
		w.srv.WaitCrash(2 * time.Minute)
		o := w.S.Await(f, n)
		if o.Attempt.Status != execution.AttemptFailed {
			t.Fatalf("the crashed attempt is %s", o.Attempt.Status)
		}
		for i := 0; i < 3; i++ {
			w.S.X.Clock.Advance(time.Minute)
			w.S.Reconcile(f)
		}
		agg, err := w.S.ExecStore.Get(context.Background(), f.Scope, f.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(agg.Attempts) != 1 || agg.Run.Status != execution.RunFailed {
			t.Fatalf("a started attempt was retried or left open: attempts=%d run=%s", len(agg.Attempts), agg.Run.Status)
		}
		rec := w.mustDeployment(f)
		if rec.Status != delivery.StatusRunning || rec.Reason != delivery.ReasonApplying {
			t.Fatalf("the abandoned intent must stay visible: %s/%s", rec.Status, rec.Reason)
		}
		if w.ns.object(t, "deployment", "inventory") != nil {
			t.Fatal("objects were applied")
		}
		// DEPENDENCY GAP, asserted so that it is visible rather than papered over:
		// nothing settles this record (no further attempt exists, and cleanup is
		// refused for a deployment still applying).
		w.expire()
		rep := w.sweep(w.scope)
		it, ok := item(rep, rec.ID)
		if !ok || it.Outcome != "not_settled" {
			t.Fatalf("an abandoned running deployment is not reported as waiting: %+v", rep.Items)
		}
	})

	t.Run("readiness_is_observed_not_printed", func(t *testing.T) {
		w := newWorld(t, "ready", worldOptions{})
		// kubectl prints "deployment.apps/inventory created" for a workload that
		// can never be scheduled.
		stuck := w.appManifests(1, "inventory")
		stuck["app.yaml"] = strings.Replace(stuck["app.yaml"], "          command:", "          resources: {requests: {cpu: \"1000\"}}\n          command:", 1)
		f := w.flow(stack.FlowOptions{Manifests: stuck, Env: map[string]string{"DEPLOY_READY_TIMEOUT": "20"}})
		o := w.run(f)
		w.requireFailed(o, "rollout_timeout")
		if !strings.Contains(o.Logs, "created") && w.ns.object(t, "deployment", "inventory") == nil {
			t.Fatalf("the test needs the workload to have been applied:\n%s", tail(o.Logs, 800))
		}
		rec := w.mustDeployment(f)
		if rec.Status != delivery.StatusFailed || rec.Reason != "rollout_timeout" {
			t.Fatalf("the record says %s/%s for a rollout that never became ready", rec.Status, rec.Reason)
		}
		for _, ob := range rec.Objects {
			if ob.Kind == "Deployment" && ob.Ready {
				t.Fatalf("%s recorded ready", ob.Name)
			}
		}
		if st := w.runStatus(f); st != execution.RunFailed {
			t.Fatalf("the run is %s", st)
		}
	})
}

// markUnknown states the record's outcome unknown as the system actor does when
// it reconciles an apply it cannot vouch for.
func (w *world) markUnknown(f *stack.Flow) {
	w.t.Helper()
	rec := w.mustDeployment(f)
	m := delivery.Mutation{Actor: delivery.Actor{Type: delivery.ActorSystem, ID: "t067-reconciler"}, Reason: "outcome unknown", Now: w.S.X.Clock.Now().UTC().Truncate(time.Microsecond)}
	_, _, err := w.S.DeliveryStore.Update(context.Background(), w.scope, rec.ID, m, func(d *delivery.Deployment) (delivery.Transition, error) {
		ch, err := d.MarkOutcomeUnknown(m.Now)
		return delivery.Transition{Kind: "outcome_unknown", Change: ch}, err
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

var _ = fmt.Sprintf

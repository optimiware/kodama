package deploymenttools_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/delivery"
)

type lookupDoc struct {
	Verdict    string `json:"verdict"`
	Deployment *struct {
		ID, Status, Reason, Namespace string
		OutcomeUnknown                bool
		ConfigurationDigest           string
		ImageDigest                   string
		SourceCommit                  string
	} `json:"deployment"`
	Annotations map[string]string `json:"annotations"`
	Observed    []struct {
		Kind, Name, UID string
		Present, Owned  bool
		Ready           bool
		Holder          *string
	} `json:"observed"`
}

func (w *world) lookupDoc(r result) lookupDoc {
	w.t.Helper()
	var d lookupDoc
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.Stdout)), &d); err != nil {
		w.t.Fatalf("lookup stdout is not the documented JSON document: %v\n%s", err, r.Stdout)
	}
	return d
}

func resourceVersion(obj map[string]any) string {
	md, _ := obj["metadata"].(map[string]any)
	s, _ := md["resourceVersion"].(string)
	return s
}

// Test_deployment_annotation_lookup — deployment_annotation_lookup (DQ-08,
// DQ-A05): after a lost response, a restart or a retry attempt, the script finds
// what exists from the recorded identities and the objects' own annotations,
// never from memory or command output; it never applies over an unknown outcome
// or over objects it does not own.
func Test_deployment_annotation_lookup(t *testing.T) {
	t.Run("lookup_is_read_only_and_annotation_bound", func(t *testing.T) {
		w := newWorld(t, "lookup")
		key := w.nextKey("deploy")
		mdir := w.appManifests(1)

		// Nothing registered for the action yet.
		r := w.lookup(key, nil)
		w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "unregistered" || d.Deployment != nil {
			t.Fatalf("unregistered: %+v", d)
		}

		// Registered only (the cluster observation was refused): intent, no objects.
		other := w.cl.newNamespace(t, w.dir, "lookup-b")
		w.requireFailed(w.run(step{script: "apply.sh", actionKey: key, manifests: mdir, kubeconfig: other.kubeconfig, env: w.applyEnv(nil)}), "cluster_forbidden")
		r = w.lookup(key, nil)
		w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "registered" || d.Deployment.Status != "pending" {
			t.Fatalf("registered: %+v", d)
		}

		w.requireCompleted(w.apply(key, mdir, nil))
		rec := w.cb.mustDeployment(key)

		// A lookup changes neither the cluster nor the record.
		cmBefore, depBefore := resourceVersion(w.ns.object(t, "configmap", "inventory-config")), resourceVersion(w.ns.object(t, "deployment", "inventory"))
		mark := w.cb.mark()
		r = w.lookup(key, nil)
		outs := w.requireCompleted(r)
		doc := w.lookupDoc(r)
		if doc.Verdict != "applied_ready" || outs["verdict"] != "applied_ready" || outs["ready"] != true {
			t.Fatalf("applied_ready: %+v outputs %v", doc, outs)
		}
		if doc.Deployment.ID != string(rec.ID) || doc.Deployment.ConfigurationDigest != string(rec.ConfigDigest) || doc.Deployment.ImageDigest != string(w.digest) || doc.Deployment.SourceCommit != w.commit {
			t.Errorf("lookup identity differs from the record: %+v", doc.Deployment)
		}
		for k, v := range receiptAnnotations(rec) {
			if doc.Annotations[k] != v {
				t.Errorf("lookup annotation %s = %q, want %q", k, doc.Annotations[k], v)
			}
		}
		live := map[string]string{
			"ConfigMap/inventory-config": uidOf(w.ns.object(t, "configmap", "inventory-config")),
			"Deployment/inventory":       uidOf(w.ns.object(t, "deployment", "inventory")),
		}
		for _, o := range doc.Observed {
			if !o.Present || !o.Owned || !o.Ready || live[o.Kind+"/"+o.Name] != o.UID {
				t.Errorf("observed %+v does not match the live object", o)
			}
		}
		for _, c := range w.cb.since(mark) {
			if c.Method != "GET" {
				t.Errorf("lookup changed the record: %s %s", c.Method, c.Path)
			}
		}
		if resourceVersion(w.ns.object(t, "configmap", "inventory-config")) != cmBefore || resourceVersion(w.ns.object(t, "deployment", "inventory")) != depBefore {
			t.Error("lookup changed the cluster")
		}

		// Readiness comes from live status, not from the record or from text: a
		// rollout that cannot finish reads applied_not_ready although the record
		// says succeeded.
		// (The image stays the verified one; the new Pod just cannot be scheduled.)
		w.ns.admin(t, "set", "resources", "deployment/inventory", "--requests=cpu=1000")
		eventually(t, 60*time.Second, "the broken rollout to be visible", func() (bool, string) {
			out := w.ns.admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.status.updatedReplicas}/{.status.readyReplicas}/{.status.replicas}")
			return out != "" && out != "1/1/1" && out != "//", out
		})
		r = w.lookup(key, nil)
		w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "applied_not_ready" || d.Deployment.Status != "succeeded" {
			t.Errorf("applied_not_ready: %+v", d)
		}
		if got := w.requireCompleted(r)["ready"]; got != false {
			t.Errorf("ready output = %v for a rollout that is not ready", got)
		}

		// Annotations and a structured status do not vouch for the image: a live
		// workload running anything but the verified digest is an image_mismatch.
		w.ns.admin(t, "set", "resources", "deployment/inventory", "--requests=cpu=1m")
		w.ns.admin(t, "set", "image", "deployment/inventory", "app=t065.local/other@sha256:"+strings.Repeat("e", 64))
		r = w.lookup(key, nil)
		outs = w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "image_mismatch" || outs["ready"] != false {
			t.Errorf("image_mismatch: %+v outputs %v", d, outs)
		}

		// Another deployment's claim on an object, then partial and absent.
		w.ns.admin(t, "annotate", "deployment", "inventory", "--overwrite", "kodama.optimi.io/deployment-id=dep_someone_else")
		r = w.lookup(key, nil)
		w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "foreign" {
			t.Errorf("foreign: %+v", d)
		}
		w.ns.admin(t, "annotate", "deployment", "inventory", "--overwrite", "kodama.optimi.io/deployment-id="+string(rec.ID))
		w.ns.admin(t, "set", "image", "deployment/inventory", "app="+w.nodeRef)
		w.ns.admin(t, "delete", "configmap", "inventory-config")
		r = w.lookup(key, nil)
		w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "partial" {
			t.Errorf("partial: %+v", d)
		}
		w.ns.admin(t, "delete", "deployment", "inventory")
		r = w.lookup(key, nil)
		w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "absent" {
			t.Errorf("absent: %+v", d)
		}
	})

	t.Run("lost_apply_acknowledgments_reconcile_without_duplicates", func(t *testing.T) {
		w := newWorld(t, "ackloss")
		mdir := w.appManifests(1)

		// (a) The intent was recorded but its response is lost on every try: the
		// script stops before applying. Lookup shows the recorded intent and no
		// objects; a retry attempt resumes the same deployment and applies once.
		keyA := w.nextKey("deploy")
		w.cb.dropResponses(outcomeState("applying"))
		w.requireFailed(w.apply(keyA, mdir, nil), "callback_unavailable")
		w.cb.clearFaults()
		recA := w.cb.mustDeployment(keyA)
		if recA.Status != delivery.StatusRunning || recA.Reason != delivery.ReasonApplying || len(recA.Objects) != 2 {
			t.Fatalf("the intent must be durable before apply: %s/%s %v", recA.Status, recA.Reason, recA.Objects)
		}
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatal("objects were applied although the intent was not acknowledged")
		}
		r := w.lookup(keyA, nil)
		w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "absent" || d.Deployment.Status != "running" || d.Deployment.ID != string(recA.ID) {
			t.Fatalf("lookup after a lost intent: %+v", d)
		}
		w.requireCompleted(w.apply(keyA, mdir, nil))
		recA2 := w.cb.mustDeployment(keyA)
		if recA2.ID != recA.ID || recA2.Status != delivery.StatusSucceeded {
			t.Fatalf("the retry must settle the same deployment: %s %s", recA2.ID, recA2.Status)
		}
		if n := w.countObjects(t, string(recA.ID)); n != 2 {
			t.Errorf("%d objects carry the deployment id after the retry, want 2", n)
		}
		cleanupAll(w)

		// (b) The apply and the readiness observation succeed but the request that
		// reports success is lost: the record still says running, the annotations
		// say applied. The retry attempt observes first, re-applies the same
		// objects (same uids) and settles.
		keyB := w.nextKey("deploy")
		w.cb.dropRequests(outcomeState("succeeded"))
		w.requireFailed(w.apply(keyB, mdir, nil), "outcome_unacknowledged")
		w.cb.clearFaults()
		recB := w.cb.mustDeployment(keyB)
		if recB.Status != delivery.StatusRunning {
			t.Fatalf("record after a lost report: %s", recB.Status)
		}
		uids := map[string]string{
			"cm":  uidOf(w.ns.object(t, "configmap", "inventory-config")),
			"dep": uidOf(w.ns.object(t, "deployment", "inventory")),
		}
		r = w.lookup(keyB, nil)
		w.requireCompleted(r)
		if d := w.lookupDoc(r); d.Verdict != "applied_ready" || d.Deployment.Status != "running" {
			t.Fatalf("lookup must find the applied objects by annotation while the record is still running: %+v", d)
		}
		mark := w.cb.mark()
		w.requireCompleted(w.apply(keyB, mdir, nil))
		if w.cb.mustDeployment(keyB).Status != delivery.StatusSucceeded {
			t.Fatal("the retry did not settle the deployment")
		}
		if uids["cm"] != uidOf(w.ns.object(t, "configmap", "inventory-config")) || uids["dep"] != uidOf(w.ns.object(t, "deployment", "inventory")) {
			t.Error("the retry replaced objects instead of observing them")
		}
		if states := outcomeStates(w.cb.since(mark)); strings.Join(states, ",") != "applying,succeeded" {
			t.Errorf("retry reports = %v", states)
		}
		cleanupAll(w)

		// (c) The response of the success report is lost after the record settled:
		// the script cannot know, the record is already succeeded, and the retry
		// only verifies and reports, never applying again.
		keyC := w.nextKey("deploy")
		w.cb.dropResponses(outcomeState("succeeded"))
		w.requireFailed(w.apply(keyC, mdir, nil), "outcome_unacknowledged")
		w.cb.clearFaults()
		if w.cb.mustDeployment(keyC).Status != delivery.StatusSucceeded {
			t.Fatal("the success report was processed before its response was lost")
		}
		mark = w.cb.mark()
		w.requireCompleted(w.apply(keyC, mdir, nil))
		if states := outcomeStates(w.cb.since(mark)); len(states) != 0 {
			t.Errorf("a settled deployment was reported on again: %v", states)
		}
		cleanupAll(w)

		// (d) A single lost response is absorbed by the script's own retry: every
		// call of the interface is idempotent by its logical action.
		keyD := w.nextKey("deploy")
		w.cb.dropResponseOnce(outcomeState("applying"))
		w.requireCompleted(w.apply(keyD, mdir, nil))
		if states := outcomeStates(w.cb.callLog()); !containsSeq(states, "applying(lost response)", "applying") {
			t.Errorf("the lost response was not retried: %v", states)
		}
	})

	t.Run("unknown_outcome_is_settled_by_observation_never_reapplied", func(t *testing.T) {
		w := newWorld(t, "unknown")
		mdir := w.appManifests(1)

		// (a) Everything exists with this deployment's annotations. A re-apply
		// would reset replicas to 1; the settlement must not apply at all.
		key := w.nextKey("deploy")
		w.cb.dropRequests(outcomeState("succeeded"))
		w.requireFailed(w.apply(key, mdir, nil), "outcome_unacknowledged")
		w.cb.clearFaults()
		w.cb.markUnknown(key)
		w.ns.admin(t, "scale", "deployment/inventory", "--replicas=2")
		mark := w.cb.mark()
		w.requireCompleted(w.apply(key, mdir, nil))
		rec := w.cb.mustDeployment(key)
		if rec.Status != delivery.StatusSucceeded || rec.Reason != delivery.ReasonReady {
			t.Fatalf("the unknown outcome was not settled: %s/%s", rec.Status, rec.Reason)
		}
		if states := outcomeStates(w.cb.since(mark)); strings.Join(states, ",") != "succeeded" {
			t.Errorf("reconciliation reports = %v, want only the settlement (no applying)", states)
		}
		if got := w.ns.admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.spec.replicas}"); got != "2" {
			t.Errorf("replicas = %s: the objects were applied again during reconciliation", got)
		}
		cleanupAll(w)

		// (b) Nothing exists: the apply is settled failed by observation, nothing
		// is created, and the action cannot be repeated.
		keyB := w.nextKey("deploy")
		w.cb.dropRequests(outcomeState("succeeded"))
		w.requireFailed(w.apply(keyB, mdir, nil), "outcome_unacknowledged")
		w.cb.clearFaults()
		w.ns.admin(t, "delete", "deployment/inventory", "configmap/inventory-config")
		w.cb.markUnknown(keyB)
		w.requireFailed(w.apply(keyB, mdir, nil), "apply_not_observed")
		if w.cb.mustDeployment(keyB).Status != delivery.StatusFailed || w.ns.object(t, "deployment", "inventory") != nil {
			t.Error("reconciliation re-created objects or left the record unsettled")
		}
		w.requireFailed(w.apply(keyB, mdir, nil), "previously_failed")
		if w.ns.object(t, "deployment", "inventory") != nil {
			t.Error("a failed deployment was applied again")
		}

		// (c) Some objects exist: settled failed as partial; they stay owned so a
		// cleanup run can remove them.
		keyC := w.nextKey("deploy")
		w.cb.dropRequests(outcomeState("succeeded"))
		w.requireFailed(w.apply(keyC, mdir, nil), "outcome_unacknowledged")
		w.cb.clearFaults()
		w.ns.admin(t, "delete", "configmap/inventory-config")
		w.cb.markUnknown(keyC)
		w.requireFailed(w.apply(keyC, mdir, nil), "apply_partial")
		recC := w.cb.mustDeployment(keyC)
		if recC.Status != delivery.StatusFailed || recC.Reason != "apply_partial" {
			t.Errorf("record %s/%s", recC.Status, recC.Reason)
		}
		w.requireAnnotated(recC, "deployment", "inventory")
	})

	t.Run("unmanaged_objects_are_not_overwritten", func(t *testing.T) {
		w := newWorld(t, "adopt")
		mdir := w.appManifests(1)
		w.ns.admin(t, "create", "configmap", "inventory-config", "--from-literal=precious=1")
		before := resourceVersion(w.ns.object(t, "configmap", "inventory-config"))

		key := w.nextKey("deploy")
		w.requireFailed(w.apply(key, mdir, nil), "object_conflict")
		if resourceVersion(w.ns.object(t, "configmap", "inventory-config")) != before {
			t.Error("an unmanaged object was changed")
		}
		if states := outcomeStates(w.cb.callLog()); len(states) != 0 {
			t.Errorf("the intent was recorded for a conflicting apply: %v", states)
		}
		if w.ns.object(t, "deployment", "inventory") != nil {
			t.Error("part of a conflicting apply was applied")
		}
		// An explicit decision replaces it, and then the object is ours.
		w.requireCompleted(w.apply(key, mdir, map[string]string{"DEPLOY_ADOPT_EXISTING": "true"}))
		w.requireAnnotated(w.cb.mustDeployment(key), "configmap", "inventory-config")
	})

	t.Run("lookup_is_bound_to_the_attempts_own_action", func(t *testing.T) {
		w := newWorld(t, "lookupkey")
		key := w.nextKey("deploy")
		w.requireCompleted(w.apply(key, w.appManifests(1), nil))
		other := w.nextKey("other")
		// The attempt's pinned action is `other`; asking for `key` is refused.
		att, tok := w.cb.attempt(other)
		r := w.run(step{script: "lookup.sh", actionKey: other, attempt: att, token: tok, env: map[string]string{"DEPLOY_ACTION_KEY": key}})
		w.requireFailed(r, "callback_denied")
		if strings.Contains(r.Stdout, string(w.cb.mustDeployment(key).ID)) {
			t.Error("a foreign deployment's identity was disclosed")
		}
		// A token of another attempt is refused too.
		r = w.run(step{script: "lookup.sh", actionKey: key, attempt: att, token: "tok-forged", env: nil})
		w.requireFailed(r, "callback_denied")
	})
}

func containsSeq(list []string, first, second string) bool {
	for i := 0; i+1 < len(list); i++ {
		if list[i] == first && list[i+1] == second {
			return true
		}
	}
	return false
}

// countObjects counts the live ConfigMaps and Deployments of the namespace that
// carry the deployment id annotation.
func (w *world) countObjects(t *testing.T, depID string) int {
	t.Helper()
	n := 0
	for _, kind := range []string{"configmap", "deployment"} {
		out := w.ns.admin(t, "get", kind, "-o", "json")
		var list struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			t.Fatal(err)
		}
		for _, it := range list.Items {
			for _, v := range annotationsOf(it) {
				if v == depID {
					n++
				}
			}
		}
	}
	return n
}

// cleanupAll removes the example objects between scenarios of one test.
func cleanupAll(w *world) {
	w.t.Helper()
	_, _ = w.ns.adminTry("delete", "deployment/inventory", "configmap/inventory-config", "--ignore-not-found", "--wait=true", "--timeout=60s")
}

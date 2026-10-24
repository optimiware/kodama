package deploymenttools_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/delivery"
)

// Test_deployment_tools_outputs — deployment_tools_outputs (DQ-07, DQ-08,
// DQ-10): the output contract. Outputs are the verified image/source/
// configuration identities and a readiness flag that only structured observation
// of the live objects can set; mismatching provenance applies nothing; command
// text never decides an outcome.
func Test_deployment_tools_outputs(t *testing.T) {
	t.Run("documented_output_set_carries_verified_identities", func(t *testing.T) {
		w := newWorld(t, "outputs")
		key := w.nextKey("deploy")
		r := w.apply(key, w.appManifests(1), nil)
		outs := w.requireCompleted(r)
		rec := w.cb.mustDeployment(key)

		names := make([]string, 0, len(outs))
		for k := range outs {
			names = append(names, k)
		}
		sort.Strings(names)
		want := []string{"configuration_digest", "deployment_id", "deployment_status", "image_digest", "object_count", "ready", "source_commit"}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("outputs = %v, want %v", names, want)
		}
		expect := map[string]any{
			"deployment_id":        string(rec.ID),
			"deployment_status":    "succeeded",
			"configuration_digest": string(rec.ConfigDigest),
			"image_digest":         string(w.digest),
			"source_commit":        w.commit,
			"ready":                true,
			"object_count":         float64(2),
		}
		for k, v := range expect {
			if outs[k] != v {
				t.Errorf("output %s = %v (%T), want %v", k, outs[k], outs[k], v)
			}
		}
		// The outputs are the record's verified identities, and the live objects
		// carry the same ones.
		if rec.Image.Digest != w.digest || rec.Source.Commit != w.commit || rec.Status != delivery.StatusSucceeded {
			t.Errorf("record: image %s source %s status %s", rec.Image.Digest, rec.Source.Commit, rec.Status)
		}
		dep := w.requireAnnotated(rec, "deployment", "inventory")
		if annotationsOf(dep)["kodama.optimi.com/configuration-digest"] != outs["configuration_digest"] {
			t.Error("the live objects do not carry the configuration digest the step reports")
		}
		image := w.ns.admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
		if image != w.nodeRef || !strings.HasSuffix(image, "@"+outs["image_digest"].(string)) {
			t.Errorf("the Deployment runs %q, not the digest-pinned image %q", image, w.nodeRef)
		}
		// Nothing but the manifest is the contract: the script prints nothing on stdout.
		if strings.TrimSpace(r.Stdout) != "" {
			t.Errorf("apply.sh wrote to stdout: %q", r.Stdout)
		}

		// A settled action replays the same outputs without applying again.
		again := w.requireCompleted(w.apply(key, w.appManifests(1), nil))
		for k, v := range expect {
			if again[k] != v {
				t.Errorf("replayed output %s = %v, want %v", k, again[k], v)
			}
		}

		// Selecting outputs: only what the step declares is written (an undeclared
		// output is refused by the entrypoint); an unknown name fails the step.
		sel := w.requireCompleted(w.apply(key, w.appManifests(1), map[string]string{"DEPLOY_OUTPUTS": "deployment_id,ready"}))
		if len(sel) != 2 || sel["deployment_id"] != string(rec.ID) || sel["ready"] != true {
			t.Errorf("selected outputs = %v", sel)
		}
		w.requireFailed(w.apply(key, w.appManifests(1), map[string]string{"DEPLOY_OUTPUTS": "deployment_id,bogus"}), "outputs_unknown")

		// The cleanup script's outputs.
		w.cb.requestCleanup(key)
		att, tok := w.cb.cleanupAttempt(rec.ID)
		couts := w.requireCompleted(w.run(step{script: "delete.sh", attempt: att, token: tok, env: map[string]string{"DEPLOY_DEPLOYMENT_ID": string(rec.ID)}}))
		if len(couts) != 3 || couts["deployment_id"] != string(rec.ID) || couts["cleanup_state"] != "done" || couts["deleted_count"] != float64(2) {
			t.Errorf("delete outputs = %v", couts)
		}
	})

	t.Run("mismatching_source_or_image_applies_nothing", func(t *testing.T) {
		w := newWorld(t, "provenance")
		mdir := w.appManifests(1)
		noEffect := func(what string) {
			t.Helper()
			if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
				t.Errorf("%s: objects were applied", what)
			}
			if states := outcomeStates(w.cb.callLog()); len(states) != 0 {
				t.Errorf("%s: outcomes were reported: %v", what, states)
			}
		}
		otherDigest := "sha256:" + strings.Repeat("e", 64)
		otherCommit := strings.Repeat("2", 40)

		// The record interface verifies provenance: an image that is not a verified
		// output of the stated source is refused at registration.
		w.requireFailed(w.apply(w.nextKey("deploy"), mdir, map[string]string{"DEPLOY_IMAGE": "t065.local/tools@" + otherDigest}), "callback_rejected")
		w.requireFailed(w.apply(w.nextKey("deploy"), mdir, map[string]string{"DEPLOY_COMMIT": otherCommit}), "callback_rejected")
		noEffect("unverified provenance")

		// Malformed inputs are refused before any call.
		before := w.cb.mark()
		w.requireFailed(w.apply(w.nextKey("deploy"), mdir, map[string]string{"DEPLOY_IMAGE": "t065.local/tools:latest"}), "input_invalid")
		w.requireFailed(w.apply(w.nextKey("deploy"), mdir, map[string]string{"DEPLOY_COMMIT": "main"}), "input_invalid")
		w.requireFailed(w.apply(w.nextKey("deploy"), mdir, map[string]string{"DEPLOY_IMAGE_ARTIFACT_ID": ""}), "input_invalid")
		if n := len(w.cb.since(before)); n != 0 {
			t.Errorf("malformed inputs called the record interface %d times", n)
		}

		// The same logical action with other inputs is a conflict, not a second
		// deployment.
		key := w.nextKey("deploy")
		other := w.cl.newNamespace(t, w.dir, "provenance-b")
		w.requireFailed(w.run(step{script: "apply.sh", actionKey: key, manifests: mdir, kubeconfig: other.kubeconfig, env: w.applyEnv(nil)}), "cluster_forbidden")
		rec := w.cb.mustDeployment(key)
		w.cb.produce("art_t065_other", w.repoURL, otherCommit, w.digest)
		w.requireFailed(w.apply(key, mdir, map[string]string{"DEPLOY_COMMIT": otherCommit, "DEPLOY_IMAGE_ARTIFACT_ID": "art_t065_other"}), "callback_conflict")
		if got := w.cb.mustDeployment(key); got.ID != rec.ID || got.Source.Commit != w.commit {
			t.Errorf("the recorded deployment changed: %s %s", got.ID, got.Source.Commit)
		}

		// Defence in depth: even a record interface that hands back another
		// deployment's identities cannot make the script apply them.
		w.cb.lie = true
		mark := w.cb.mark()
		for name, extra := range map[string]map[string]string{
			"commit":   {"DEPLOY_COMMIT": otherCommit},
			"image":    {"DEPLOY_IMAGE": "t065.local/tools@" + otherDigest},
			"artifact": {"DEPLOY_IMAGE_ARTIFACT_ID": "art_t065_other"},
			"source":   {"DEPLOY_REPOSITORY_URL": "https://git.t065.invalid/acme/elsewhere.git"},
		} {
			w.requireFailed(w.apply(key, mdir, extra), "provenance_mismatch")
			_ = name
		}
		w.cb.lie = false
		if states := outcomeStates(w.cb.since(mark)); len(states) != 0 {
			t.Errorf("a provenance mismatch reported outcomes: %v", states)
		}
		noEffect("provenance cross-check")
	})

	t.Run("readiness_is_observed_not_printed", func(t *testing.T) {
		w := newWorld(t, "notready")
		key := w.nextKey("deploy")
		// A Deployment of the verified image that kubectl happily creates but that
		// can never become ready: its command fails at once.
		broken := w.manifests(map[string]string{"app.yaml": strings.Replace(fmt.Sprintf(sleeperDeployment, "inventory", 1, "@@IMAGE@@"), `["sleep", "3600"]`, `["false"]`, 1)})
		r := w.apply(key, broken, map[string]string{"DEPLOY_READY_TIMEOUT": "10"})
		w.requireFailed(r, "rollout_timeout")
		if !strings.Contains(r.Stderr, "kubectl: deployment.apps/inventory created") {
			t.Errorf("the scenario needs kubectl to report success in text:\n%s", r.Stderr)
		}
		rec := w.cb.mustDeployment(key)
		if rec.Status != delivery.StatusFailed || rec.Reason != "rollout_timeout" {
			t.Fatalf("record %s/%s", rec.Status, rec.Reason)
		}
		for _, o := range rec.Objects {
			if o.Ready {
				t.Errorf("%s/%s is recorded ready", o.Kind, o.Name)
			}
		}
		if got := outcomeStates(w.cb.callLog()); strings.Join(got, ",") != "applying,failed" {
			t.Errorf("outcome reports = %v, want [applying failed]", got)
		}
		if r.outputs() != nil {
			t.Errorf("a failed step produced outputs: %v", r.outputs())
		}
		// The failure is final for the action; the objects stay owned for cleanup.
		w.requireFailed(w.apply(key, broken, nil), "previously_failed")
		w.requireAnnotated(rec, "deployment", "inventory")
		w.cb.requestCleanup(key)
		att, tok := w.cb.cleanupAttempt(rec.ID)
		w.requireCompleted(w.run(step{script: "delete.sh", attempt: att, token: tok, env: map[string]string{"DEPLOY_DEPLOYMENT_ID": string(rec.ID)}}))
		if w.ns.object(t, "deployment", "inventory") != nil {
			t.Error("cleanup left the failed deployment's objects")
		}
	})

	t.Run("a_privileged_workload_never_reads_as_ready", func(t *testing.T) {
		w := newWorld(t, "privileged")
		key := w.nextKey("deploy")
		// The namespace enforces restricted Pod Security. The cluster accepts the
		// Deployment object (it only warns) and refuses its Pods, so kubectl
		// reports success in text while nothing runs: only observation tells.
		bad := w.manifests(map[string]string{
			"app.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: inventory\nspec:\n  replicas: 1\n  selector: {matchLabels: {a: b}}\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n      containers: [{name: c, image: \"@@IMAGE@@\", securityContext: {privileged: true}}]\n",
		})
		r := w.apply(key, bad, map[string]string{"DEPLOY_READY_TIMEOUT": "10"})
		w.requireFailed(r, "rollout_timeout")
		if rec := w.cb.mustDeployment(key); rec.Status != delivery.StatusFailed || rec.Reason != "rollout_timeout" {
			t.Fatalf("record %s/%s", rec.Status, rec.Reason)
		}
		if got := w.ns.admin(t, "get", "pods", "-o", "name"); got != "" {
			t.Errorf("a privileged Pod exists in the restricted namespace: %s", got)
		}
	})

	t.Run("manifest_images_are_bound_to_the_verified_digest", func(t *testing.T) {
		w := newWorld(t, "imagebind")
		otherRef := "t065.local/other@sha256:" + strings.Repeat("e", 64)
		noEffect := func(what string, before int) {
			t.Helper()
			if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
				t.Errorf("%s: objects were applied", what)
			}
			if n := len(w.cb.since(before)); n != 0 {
				t.Errorf("%s: the record interface was called %d times", what, n)
			}
		}
		twoContainers := func(extra string) string {
			return strings.Replace(fmt.Sprintf(sleeperDeployment, "inventory", 1, "@@IMAGE@@"), "      containers:\n", extra+"      containers:\n", 1)
		}

		// Valid, registered inputs accompany manifests that run another image.
		for name, files := range map[string]map[string]string{
			"hard_coded_other_digest": {"app.yaml": fmt.Sprintf(sleeperDeployment, "inventory", 1, otherRef)},
			"tag_only":                {"app.yaml": fmt.Sprintf(sleeperDeployment, "inventory", 1, "t065.local/tools:latest")},
			"foreign_sidecar": {"app.yaml": strings.Replace(fmt.Sprintf(sleeperDeployment, "inventory", 1, "@@IMAGE@@"),
				"      containers:\n", "      containers:\n        - {name: side, image: \""+otherRef+"\", command: [sleep, \"3600\"]}\n", 1)},
			"foreign_init_container": {"app.yaml": twoContainers("      initContainers:\n        - {name: init, image: \"" + otherRef + "\", command: [\"true\"]}\n")},
			"foreign_cronjob_image":  {"app.yaml": "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: inventory\nspec:\n  schedule: \"0 0 1 1 *\"\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          restartPolicy: Never\n          containers: [{name: c, image: \"" + otherRef + "\"}]\n"},
		} {
			before := w.cb.mark()
			r := w.apply(w.nextKey("deploy"), w.manifests(files), nil)
			w.requireFailed(r, "image_not_bound")
			if r.outputs() != nil || strings.Contains(r.Stdout, string(w.digest)) {
				t.Errorf("%s: a verified-image output was produced: %v", name, r.Manifest)
			}
			noEffect(name, before)
		}

		// No container runs the verified image at all: nothing was deployed.
		before := w.cb.mark()
		r := w.apply(w.nextKey("deploy"), w.manifests(map[string]string{"config.yaml": fmt.Sprintf(configMap, "inventory-config")}), nil)
		w.requireFailed(r, "image_not_deployed")
		noEffect("no workload", before)

		// A malformed allow-list is refused.
		w.requireFailed(w.apply(w.nextKey("deploy"), w.appManifests(1), map[string]string{"DEPLOY_ALLOWED_IMAGES": "t065.local/other:latest"}), "input_invalid")

		// An explicitly allowed digest-pinned auxiliary image may run next to the
		// verified one (zero replicas: nothing has to be pulled).
		key := w.nextKey("deploy")
		side := w.manifests(map[string]string{"app.yaml": strings.Replace(fmt.Sprintf(sleeperDeployment, "inventory", 0, "@@IMAGE@@"),
			"      containers:\n", "      containers:\n        - {name: side, image: \""+otherRef+"\", command: [sleep, \"3600\"]}\n", 1)})
		w.requireFailed(w.apply(key, side, nil), "image_not_bound") // not allowed without the explicit list
		w.requireCompleted(w.apply(key, side, map[string]string{"DEPLOY_ALLOWED_IMAGES": otherRef}))
		w.requireAnnotated(w.cb.mustDeployment(key), "deployment", "inventory")
	})

	t.Run("live_images_are_verified_on_replay_and_reconciliation", func(t *testing.T) {
		w := newWorld(t, "imagereplay")
		mdir := w.appManifests(1)
		otherRef := "t065.local/other@sha256:" + strings.Repeat("e", 64)

		// (a) A settled deployment whose live workload was changed to another
		// image: the retry reports image_mismatch instead of replaying the
		// verified-image outputs, and applies nothing.
		key := w.nextKey("deploy")
		w.requireCompleted(w.apply(key, mdir, nil))
		w.ns.admin(t, "set", "image", "deployment/inventory", "app="+otherRef)
		mark := w.cb.mark()
		r := w.apply(key, mdir, nil)
		w.requireFailed(r, "image_mismatch")
		if r.outputs() != nil {
			t.Errorf("a verified-image output was produced for a drifted workload: %v", r.outputs())
		}
		if states := outcomeStates(w.cb.since(mark)); len(states) != 0 {
			t.Errorf("replay reported outcomes: %v", states)
		}
		if got := w.ns.admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.spec.template.spec.containers[0].image}"); got != otherRef {
			t.Errorf("the replay changed the live image to %q", got)
		}
		cleanupAll(w)

		// (b) An unknown outcome is settled by observation only when the live
		// workload runs the verified image; with another image it settles failed.
		keyB := w.nextKey("deploy")
		w.cb.dropRequests(outcomeState("succeeded"))
		w.requireFailed(w.apply(keyB, mdir, nil), "outcome_unacknowledged")
		w.cb.clearFaults()
		w.cb.markUnknown(keyB)
		w.ns.admin(t, "set", "image", "deployment/inventory", "app="+otherRef)
		w.requireFailed(w.apply(keyB, mdir, nil), "image_mismatch")
		recB := w.cb.mustDeployment(keyB)
		if recB.Status != delivery.StatusFailed || recB.Reason != "image_mismatch" {
			t.Errorf("record %s/%s, want failed/image_mismatch", recB.Status, recB.Reason)
		}
		for _, o := range recB.Objects {
			if o.Kind == "Deployment" && o.Ready {
				t.Errorf("the drifted %s/%s is recorded ready", o.Kind, o.Name)
			}
		}
	})

	t.Run("the_verified_image_must_stay_deployed", func(t *testing.T) {
		w := newWorld(t, "lastcontainer")
		otherRef := "t065.local/other@sha256:" + strings.Repeat("e", 64)
		allowed := map[string]string{"DEPLOY_ALLOWED_IMAGES": otherRef}
		// The application container (verified image) and an allowed auxiliary
		// container. Zero replicas: the workload is structurally ready without
		// pulling anything, so only the binding of the container specs can tell.
		mdir := w.manifests(map[string]string{"app.yaml": strings.Replace(fmt.Sprintf(sleeperDeployment, "inventory", 0, "@@IMAGE@@"),
			"      containers:\n", "      containers:\n        - {name: side, image: \""+otherRef+"\", command: [sleep, \"3600\"]}\n", 1)})
		side := `{"name":"side","image":"` + otherRef + `","command":["sleep","3600"]}`
		app := `{"name":"app","image":"` + otherRef + `","command":["sleep","3600"]}`
		mutations := map[string]string{
			"removed":  `{"spec":{"template":{"spec":{"containers":[` + side + `]}}}}`,
			"replaced": `{"spec":{"template":{"spec":{"containers":[` + side + `,` + app + `]}}}}`,
		}
		mutate := func(how string) {
			t.Helper()
			w.ns.admin(t, "patch", "deployment", "inventory", "--type=merge", "-p", mutations[how])
		}
		for _, how := range []string{"removed", "replaced"} {
			// Replay of a settled deployment: no success, no outputs, nothing applied.
			key := w.nextKey("deploy")
			w.requireCompleted(w.apply(key, mdir, allowed))
			mutate(how)
			mark := w.cb.mark()
			r := w.apply(key, mdir, allowed)
			w.requireFailed(r, "image_mismatch")
			if r.outputs() != nil {
				t.Errorf("%s: a verified-image output was produced: %v", how, r.outputs())
			}
			if states := outcomeStates(w.cb.since(mark)); len(states) != 0 {
				t.Errorf("%s: replay reported outcomes: %v", how, states)
			}

			// Lookup: the annotations and the healthy auxiliary container do not make it applied_ready.
			lr := w.lookup(key, allowed)
			outs := w.requireCompleted(lr)
			if d := w.lookupDoc(lr); d.Verdict != "image_mismatch" || outs["ready"] != false {
				t.Errorf("%s: lookup %+v outputs %v", how, d, outs)
			}
			cleanupAll(w)

			// Unknown-outcome reconciliation settles failed, never succeeded.
			keyU := w.nextKey("deploy")
			w.cb.dropRequests(outcomeState("succeeded"))
			w.requireFailed(w.apply(keyU, mdir, allowed), "outcome_unacknowledged")
			w.cb.clearFaults()
			w.cb.markUnknown(keyU)
			mutate(how)
			w.requireFailed(w.apply(keyU, mdir, allowed), "image_mismatch")
			rec := w.cb.mustDeployment(keyU)
			if rec.Status != delivery.StatusFailed || rec.Reason != "image_mismatch" {
				t.Errorf("%s: record %s/%s, want failed/image_mismatch", how, rec.Status, rec.Reason)
			}
			for _, o := range rec.Objects {
				if o.Kind == "Deployment" && o.Ready {
					t.Errorf("%s: the Deployment without the verified image is recorded ready", how)
				}
			}
			cleanupAll(w)
		}

		// Control: untouched, the same objects are applied_ready and replay succeeds.
		key := w.nextKey("deploy")
		w.requireCompleted(w.apply(key, mdir, allowed))
		lr := w.lookup(key, allowed)
		w.requireCompleted(lr)
		if d := w.lookupDoc(lr); d.Verdict != "applied_ready" {
			t.Errorf("control lookup %+v", d)
		}
		w.requireCompleted(w.apply(key, mdir, allowed))
	})
}

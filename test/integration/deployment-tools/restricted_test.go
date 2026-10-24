package deploymenttools_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/delivery"
)

// Test_deployment_tools_restricted — deployment_tools_restricted (DQ-07–DQ-10):
// the scripts need no privileged runtime, apply and delete through a
// namespace-scoped credential only, and cannot reach another namespace, a
// cluster-scoped object, an object of another deployment or another run's record.
func Test_deployment_tools_restricted(t *testing.T) {
	t.Run("no_privileged_runtime", func(t *testing.T) {
		w := newWorld(t, "restricted")
		probe := `set -u
echo "uid=$(id -u) gid=$(id -g)"
grep -E '^(CapInh|CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs|Seccomp):' /proc/self/status
if touch /usr/probe 2>/dev/null; then echo rootfs=writable; else echo rootfs=read-only; fi
if touch /tmp/probe 2>/dev/null; then echo scratch=writable; else echo scratch=read-only; fi
for b in docker podman nerdctl crictl ctr buildah runc kind helm; do command -v "$b" >/dev/null 2>&1 && echo "runtime-tool=$b"; done
for s in /var/run/docker.sock /run/podman/podman.sock /run/containerd/containerd.sock; do [ -e "$s" ] && echo "socket=$s"; done
echo "kubectl=$(kubectl version --client -o json | jq -r .clientVersion.gitVersion)"
echo "jq=$(jq --version) curl=$(curl --version | head -n1 | cut -d' ' -f1-2) git=$(git --version)"
`
		r := w.run(step{command: []string{"sh", "-c", probe}})
		if r.Exit != 0 {
			t.Fatalf("probe failed: %d\n%s", r.Exit, r.Stderr)
		}
		for _, want := range []string{"uid=65532 gid=65532", "CapEff:\t0000000000000000", "CapBnd:\t0000000000000000", "CapPrm:\t0000000000000000",
			"NoNewPrivs:\t1", "rootfs=read-only", "scratch=writable", "kubectl=v1.37.1"} {
			if !strings.Contains(r.Stdout, want) {
				t.Errorf("probe output lacks %q:\n%s", want, r.Stdout)
			}
		}
		for _, bad := range []string{"runtime-tool=", "socket="} {
			if strings.Contains(r.Stdout, bad) {
				t.Errorf("the image or its runtime exposes a container runtime (%s):\n%s", bad, r.Stdout)
			}
		}
		for _, a := range restrictions() {
			if strings.Contains(a, "privileged") || strings.HasPrefix(a, "--cap-add") || strings.Contains(a, "docker.sock") {
				t.Errorf("restriction set contains %q", a)
			}
		}
	})

	t.Run("scoped_apply_and_delete", func(t *testing.T) {
		w := newWorld(t, "scoped")
		key := w.nextKey("deploy")
		mdir := w.appManifests(1)
		// An unrelated object in the same namespace: never part of the deployment.
		w.ns.admin(t, "create", "configmap", "bystander", "--from-literal=keep=me")

		// The record exists, and nothing is applied, when the intended objects are
		// recorded (DQ-08): look at the cluster at exactly that moment.
		var mu sync.Mutex
		var early []string
		sawApplying := false
		w.cb.onApplying = func() {
			out, _ := w.ns.adminTry("get", "configmap/inventory-config", "deployment/inventory", "--ignore-not-found", "-o", "name")
			mu.Lock()
			defer mu.Unlock()
			sawApplying = true
			if out != "" {
				early = append(early, out)
			}
		}
		r := w.apply(key, mdir, nil)
		outs := w.requireCompleted(r)
		mu.Lock()
		if !sawApplying || len(early) != 0 {
			t.Errorf("objects existed before the intent was recorded: saw=%v early=%v", sawApplying, early)
		}
		mu.Unlock()
		if strings.Contains(r.Stderr+r.Stdout, w.ns.token) {
			t.Error("the scoped credential reached the script's logs")
		}
		if got := outcomeStates(w.cb.callLog()); len(got) != 2 || got[0] != "applying" || got[1] != "succeeded" {
			t.Errorf("outcome reports = %v, want [applying succeeded]", got)
		}

		rec := w.cb.mustDeployment(key)
		if rec.Status != delivery.StatusSucceeded || outs["deployment_id"] != string(rec.ID) {
			t.Fatalf("record %s/%s, outputs %v", rec.Status, rec.Reason, outs)
		}
		cm := w.requireAnnotated(rec, "configmap", "inventory-config")
		dep := w.requireAnnotated(rec, "deployment", "inventory")
		uids := map[string]string{}
		for _, o := range rec.Objects {
			uids[o.Kind+"/"+o.Name] = o.UID
			if !o.Ready || o.UID == "" {
				t.Errorf("recorded object %s/%s is not observed ready with a uid", o.Kind, o.Name)
			}
		}
		if uids["ConfigMap/inventory-config"] != uidOf(cm) || uids["Deployment/inventory"] != uidOf(dep) {
			t.Errorf("recorded uids %v differ from the live objects", uids)
		}
		if got := w.ns.admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.status.readyReplicas}"); got != "1" {
			t.Errorf("the Deployment is not ready in the cluster: readyReplicas=%q", got)
		}
		if bs := w.ns.object(t, "configmap", "bystander"); bs == nil || len(annotationsOf(bs)) != 0 {
			t.Errorf("the unrelated object was touched: %v", bs)
		}

		// Cleanup: an internal cleanup run bound to exactly this deployment.
		w.cb.requestCleanup(key)
		for i := 1; i <= 2; i++ { // the repeat converges to the same completed cleanup
			att, tok := w.cb.cleanupAttempt(rec.ID)
			cr := w.run(step{script: "delete.sh", attempt: att, token: tok, env: map[string]string{"DEPLOY_DEPLOYMENT_ID": string(rec.ID)}})
			couts := w.requireCompleted(cr)
			if couts["cleanup_state"] != "done" || couts["deleted_count"] != float64(2) {
				t.Errorf("delete %d outputs = %v", i, couts)
			}
		}
		after := w.cb.mustDeployment(key)
		if after.DeletedAt == nil || after.CleanupState != delivery.CleanupDone || after.Status != delivery.StatusSucceeded || len(after.Objects) != 2 {
			t.Errorf("after cleanup the record is %s deleted=%v cleanup=%s objects=%d (history must be retained)", after.Status, after.DeletedAt, after.CleanupState, len(after.Objects))
		}
		if w.ns.object(t, "configmap", "inventory-config") != nil || w.ns.object(t, "deployment", "inventory") != nil {
			t.Error("owned objects survived the cleanup")
		}
		if w.ns.object(t, "configmap", "bystander") == nil {
			t.Error("the delete removed an object the deployment does not own")
		}
	})

	t.Run("credential_is_namespace_scoped", func(t *testing.T) {
		w := newWorld(t, "scope-a")
		other := w.cl.newNamespace(t, w.dir, "scope-b")

		// The credential itself, used directly: its own namespace only.
		if out, err := w.ns.scoped("-n", w.ns.name, "create", "configmap", "probe", "--from-literal=a=b"); err != nil {
			t.Fatalf("the scoped credential cannot create in its own namespace: %v %s", err, out)
		}
		for _, args := range [][]string{
			{"-n", other.name, "get", "configmaps"},
			{"-n", other.name, "create", "configmap", "x", "--from-literal=a=b"},
			{"-n", "kube-system", "get", "configmaps"},
			{"-n", w.ns.name, "get", "secrets"},
			{"create", "namespace", "t065-escape"},
			{"get", "nodes"},
			{"get", "clusterroles"},
		} {
			if out, err := w.ns.scoped(args...); err == nil || !strings.Contains(strings.ToLower(out), "forbidden") {
				t.Errorf("kubectl %v with the scoped credential was not refused: err=%v %s", args, err, out)
			}
		}

		// The script refuses other namespaces and cluster-scoped kinds before it
		// records an intent or touches the cluster. Structural problems are found
		// before the record is even registered; a namespace mismatch needs the
		// receipt (the namespace is the recorded configuration's), so it is found
		// right after registration and before the first outcome report.
		before := w.cb.mark()
		key := w.nextKey("deploy")
		clusterScoped := w.manifests(map[string]string{"x.yaml": "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: t065-escape\n"})
		w.requireFailed(w.apply(key, clusterScoped, nil), "manifest_cluster_scoped")
		generated := w.manifests(map[string]string{"x.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  generateName: g-\ndata: {a: b}\n"})
		w.requireFailed(w.apply(key, generated, nil), "manifest_invalid")
		if n := len(w.cb.since(before)); n != 0 {
			t.Errorf("a structurally invalid manifest still called the record interface %d times", n)
		}
		escaped := strings.Replace(fmt.Sprintf(sleeperDeployment, "escape", 1, "@@IMAGE@@"), "metadata:\n  name: escape\n", "metadata:\n  name: escape\n  namespace: "+other.name+"\n", 1)
		foreign := w.manifests(map[string]string{"x.yaml": escaped})
		w.requireFailed(w.apply(key, foreign, nil), "manifest_namespace")
		if states := outcomeStates(w.cb.since(before)); len(states) != 0 {
			t.Errorf("a refused manifest reported outcomes: %v", states)
		}
		if out, _ := other.adminTry("get", "deployment", "escape", "--ignore-not-found", "-o", "name"); out != "" {
			t.Errorf("the object reached the other namespace: %s", out)
		}

		// A credential scoped to another namespace cannot deploy here: the
		// observation is forbidden, nothing is recorded as applying, nothing is applied.
		mdir := w.appManifests(1)
		key = w.nextKey("deploy")
		mark := w.cb.mark()
		r := w.run(step{script: "apply.sh", actionKey: key, manifests: mdir, kubeconfig: other.kubeconfig, env: w.applyEnv(nil)})
		w.requireFailed(r, "cluster_forbidden")
		if states := outcomeStates(w.cb.since(mark)); len(states) != 0 {
			t.Errorf("outcomes were reported before the cluster could be observed: %v", states)
		}
		if rec := w.cb.mustDeployment(key); rec.Status != delivery.StatusPending {
			t.Errorf("the record is %s, want pending", rec.Status)
		}
		if out, _ := w.ns.adminTry("get", "configmap,deployment", "-o", "name", "-l", "!none"); strings.Contains(out, "inventory") {
			t.Errorf("objects were applied with a credential of another namespace: %s", out)
		}
	})

	t.Run("delete_never_touches_foreign_objects", func(t *testing.T) {
		w := newWorld(t, "foreign")
		key := w.nextKey("deploy")
		w.requireCompleted(w.apply(key, w.appManifests(1), nil))
		rec := w.cb.mustDeployment(key)

		// The Deployment was taken over by another deployment (its annotation
		// changed); the ConfigMap was replaced by a new object with the same name
		// that copied our annotations (different uid).
		w.ns.admin(t, "annotate", "deployment", "inventory", "--overwrite", "kodama.optimi.io/deployment-id=dep_someone_else")
		oldUID := uidOf(w.ns.object(t, "configmap", "inventory-config"))
		w.ns.admin(t, "delete", "configmap", "inventory-config")
		w.ns.admin(t, "create", "configmap", "inventory-config", "--from-literal=impostor=1")
		args := []string{"annotate", "configmap", "inventory-config"}
		for k, v := range receiptAnnotations(rec) {
			args = append(args, k+"="+v)
		}
		w.ns.admin(t, args...)
		newUID := uidOf(w.ns.object(t, "configmap", "inventory-config"))
		if oldUID == newUID {
			t.Fatal("the replacement has the same uid")
		}

		w.cb.requestCleanup(key)
		att, tok := w.cb.cleanupAttempt(rec.ID)
		r := w.run(step{script: "delete.sh", attempt: att, token: tok, env: map[string]string{"DEPLOY_DEPLOYMENT_ID": string(rec.ID)}})
		w.requireFailed(r, "object_not_owned")
		if w.ns.object(t, "deployment", "inventory") == nil || w.ns.object(t, "configmap", "inventory-config") == nil {
			t.Error("an object the deployment does not own was deleted")
		}
		after := w.cb.mustDeployment(key)
		if after.DeletedAt != nil || after.CleanupState != delivery.CleanupFailed || after.CleanupReason != "object_not_owned" {
			t.Errorf("the exception must stay visible: deleted=%v cleanup=%s/%s", after.DeletedAt, after.CleanupState, after.CleanupReason)
		}
	})

	t.Run("cleanup_requires_the_deployment_binding", func(t *testing.T) {
		w := newWorld(t, "binding")
		key := w.nextKey("deploy")
		w.requireCompleted(w.apply(key, w.appManifests(1), nil))
		rec := w.cb.mustDeployment(key)
		w.cb.requestCleanup(key)

		// A cleanup run bound to some other deployment, and a step attempt of an
		// unrelated action, are both refused; nothing is deleted.
		otherAtt, otherTok := w.cb.cleanupAttempt("dep_somebody_else")
		r := w.run(step{script: "delete.sh", attempt: otherAtt, token: otherTok, env: map[string]string{"DEPLOY_DEPLOYMENT_ID": string(rec.ID)}})
		w.requireFailed(r, "callback_denied")
		strayAtt, strayTok := w.cb.attempt(w.nextKey("other"))
		r = w.run(step{script: "delete.sh", attempt: strayAtt, token: strayTok, env: map[string]string{"DEPLOY_DEPLOYMENT_ID": string(rec.ID)}})
		w.requireFailed(r, "callback_denied")
		if w.ns.object(t, "deployment", "inventory") == nil || w.ns.object(t, "configmap", "inventory-config") == nil {
			t.Error("an unbound run deleted objects")
		}
		if after := w.cb.mustDeployment(key); after.DeletedAt != nil {
			t.Error("an unbound run completed the cleanup")
		}
	})

	t.Run("cleanup_authority_precedes_any_deletion", func(t *testing.T) {
		w := newWorld(t, "authority")
		key := w.nextKey("deploy")
		w.requireCompleted(w.apply(key, w.appManifests(1), nil))
		rec := w.cb.mustDeployment(key)
		uids := func() string {
			return uidOf(w.ns.object(t, "configmap", "inventory-config")) + "/" + uidOf(w.ns.object(t, "deployment", "inventory"))
		}
		before := uids()
		deleteEnv := map[string]string{"DEPLOY_DEPLOYMENT_ID": string(rec.ID)}
		intact := func(what string) {
			t.Helper()
			if got := uids(); got != before {
				t.Errorf("%s: owned objects changed or were deleted (%q, want %q)", what, got, before)
			}
		}

		// The originating step may READ its deployment, but it is not a cleaner:
		// with no cleanup requested nothing is deleted and no cleanup is reported.
		att, tok := w.cb.attempt(key)
		mark := w.cb.mark()
		r := w.run(step{script: "delete.sh", actionKey: key, attempt: att, token: tok, env: deleteEnv})
		w.requireFailed(r, "cleanup_not_requested")
		if n := countCalls(w.cb.since(mark), "POST", "/cleanup-result"); n != 0 {
			t.Errorf("%d cleanup reports were made without a cleanup request", n)
		}
		intact("origin token, no cleanup request")

		// A bound cleanup run is not enough without a request either.
		bAtt, bTok := w.cb.cleanupAttempt(rec.ID)
		w.requireFailed(w.run(step{script: "delete.sh", attempt: bAtt, token: bTok, env: deleteEnv}), "cleanup_not_requested")
		intact("bound run, no cleanup request")
		if after := w.cb.mustDeployment(key); after.DeletedAt != nil || after.CleanupState != delivery.CleanupNone {
			t.Errorf("the record changed: deleted=%v cleanup=%q", after.DeletedAt, after.CleanupState)
		}

		// With a request, the originating step's token is still refused by the
		// acknowledged cleanup authorization — before any Kubernetes mutation.
		w.cb.requestCleanup(key)
		att, tok = w.cb.attempt(key)
		hookArgs, hookEnv, h := w.hookDeletes(func(int, string) {})
		env := map[string]string{"DEPLOY_DEPLOYMENT_ID": string(rec.ID), "PATH": hookEnv["PATH"]}
		r = w.run(step{script: "delete.sh", actionKey: key, attempt: att, token: tok, args: hookArgs, env: env})
		h.finish()
		w.requireFailed(r, "callback_denied")
		if urls := h.urls(); len(urls) != 0 {
			t.Errorf("the originating step reached a Kubernetes delete: %v", urls)
		}
		intact("origin token, cleanup requested")
		if after := w.cb.mustDeployment(key); after.DeletedAt != nil || after.CleanupState != delivery.CleanupRequested {
			t.Errorf("the record changed: deleted=%v cleanup=%q", after.DeletedAt, after.CleanupState)
		}

		// The bound run goes through, in order: authorization, then deletes.
		mark = w.cb.mark()
		w.requireCompleted(w.run(step{script: "delete.sh", attempt: bAtt, token: bTok, env: deleteEnv}))
		var order []string
		for _, c := range w.cb.since(mark) {
			if c.Method == "POST" && strings.HasSuffix(c.Path, "/cleanup-result") {
				var b struct{ State, Reason string }
				_ = json.Unmarshal([]byte(c.Body), &b)
				order = append(order, b.State+":"+b.Reason)
			}
		}
		if strings.Join(order, ",") != "unknown:delete_started,done:" {
			t.Errorf("cleanup reports = %v, want authorization then completion", order)
		}
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Error("the bound cleanup run did not delete the owned objects")
		}
	})

	t.Run("delete_is_conditional_on_the_observed_object", func(t *testing.T) {
		deployURL, cmURL := "/deployments/inventory", "/configmaps/inventory-config"

		// Replacement and takeover between the script's observation and its delete.
		// The Deployment keeps its uid but changes hands (annotation overwritten);
		// the ConfigMap is replaced by a new object that copied our annotations.
		// Both deletes must fail on their preconditions; neither new owner is deleted.
		w := newWorld(t, "race")
		key := w.nextKey("deploy")
		w.requireCompleted(w.apply(key, w.appManifests(1), nil))
		rec := w.cb.mustDeployment(key)
		oldCM := uidOf(w.ns.object(t, "configmap", "inventory-config"))
		oldDep := uidOf(w.ns.object(t, "deployment", "inventory"))
		w.cb.requestCleanup(key)
		att, tok := w.cb.cleanupAttempt(rec.ID)
		args, env, h := w.hookDeletes(func(n int, url string) {
			switch {
			case strings.HasSuffix(url, deployURL):
				if _, err := w.ns.adminTry("annotate", "deployment", "inventory", "--overwrite", "kodama.optimi.io/deployment-id=dep_someone_else"); err != nil {
					t.Errorf("takeover: %v", err)
				}
			case strings.HasSuffix(url, cmURL):
				if _, err := w.ns.adminTry("delete", "configmap", "inventory-config"); err != nil {
					t.Errorf("replace (delete): %v", err)
				}
				if _, err := w.ns.adminTry("create", "configmap", "inventory-config", "--from-literal=impostor=1"); err != nil {
					t.Errorf("replace (create): %v", err)
				}
				a := []string{"annotate", "configmap", "inventory-config"}
				for k, v := range receiptAnnotations(rec) {
					a = append(a, k+"="+v)
				}
				if _, err := w.ns.adminTry(a...); err != nil {
					t.Errorf("replace (annotate): %v", err)
				}
			}
		})
		env["DEPLOY_DEPLOYMENT_ID"] = string(rec.ID)
		r := w.run(step{script: "delete.sh", attempt: att, token: tok, args: args, env: env})
		h.finish()
		w.requireFailed(r, "object_not_owned")
		if h.count(deployURL) < 1 || h.count(cmURL) < 1 {
			t.Fatalf("the interleaving did not happen at both deletes: %v", h.urls())
		}
		dep := w.ns.object(t, "deployment", "inventory")
		cm := w.ns.object(t, "configmap", "inventory-config")
		if dep == nil || uidOf(dep) != oldDep || annotationsOf(dep)["kodama.optimi.io/deployment-id"] != "dep_someone_else" {
			t.Errorf("the Deployment that changed hands was deleted or altered: %v", annotationsOf(dep))
		}
		if cm == nil || uidOf(cm) == oldCM || uidOf(cm) == "" {
			t.Errorf("the replacement ConfigMap was deleted (old %s, now %q)", oldCM, uidOf(cm))
		}
		if after := w.cb.mustDeployment(key); after.DeletedAt != nil || after.CleanupState != delivery.CleanupFailed || after.CleanupReason != "object_not_owned" {
			t.Errorf("the exception must stay visible: deleted=%v cleanup=%s/%s", after.DeletedAt, after.CleanupState, after.CleanupReason)
		}

	})

	t.Run("delete_retries_after_a_benign_change", func(t *testing.T) {
		deployURL := "/deployments/inventory"
		// A benign change of the same owned object (a label) between observation and
		// delete only costs a conflict: the script observes again, finds the object
		// still ours, and deletes it with the fresh resourceVersion.
		w2 := newWorld(t, "benign")
		key2 := w2.nextKey("deploy")
		w2.requireCompleted(w2.apply(key2, w2.appManifests(1), nil))
		rec2 := w2.cb.mustDeployment(key2)
		w2.cb.requestCleanup(key2)
		att2, tok2 := w2.cb.cleanupAttempt(rec2.ID)
		var once sync.Once
		args2, env2, h2 := w2.hookDeletes(func(n int, url string) {
			if strings.HasSuffix(url, deployURL) {
				once.Do(func() {
					if _, err := w2.ns.adminTry("label", "deployment", "inventory", "benign=1"); err != nil {
						t.Errorf("label: %v", err)
					}
				})
			}
		})
		env2["DEPLOY_DEPLOYMENT_ID"] = string(rec2.ID)
		r2 := w2.run(step{script: "delete.sh", attempt: att2, token: tok2, args: args2, env: env2})
		h2.finish()
		w2.requireCompleted(r2)
		if h2.count(deployURL) < 2 {
			t.Errorf("the conflicting delete was not retried after a fresh observation: %v", h2.urls())
		}
		if w2.ns.object(t, "deployment", "inventory") != nil || w2.ns.object(t, "configmap", "inventory-config") != nil {
			t.Error("owned objects survived a cleanup that only saw a benign change")
		}
		if after := w2.cb.mustDeployment(key2); after.DeletedAt == nil || after.CleanupState != delivery.CleanupDone {
			t.Errorf("cleanup did not complete: deleted=%v cleanup=%s", after.DeletedAt, after.CleanupState)
		}
	})
}

package deploymenttools_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/optimiware/kodama/internal/delivery"
)

var reasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

const sleeperDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
spec:
  replicas: %[2]d
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 1
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: app
          image: %[3]s
          imagePullPolicy: Never
          command: ["sleep", "3600"]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
`

const configMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
data:
  REPLICAS: "1"
`

// appManifests writes the example application (a ConfigMap and a Deployment whose
// image is the @@IMAGE@@ token) and returns the directory.
func (w *world) appManifests(replicas int) string {
	return w.manifests(map[string]string{
		"config.yaml": fmt.Sprintf(configMap, "inventory-config"),
		"app.yaml":    fmt.Sprintf(sleeperDeployment, "inventory", replicas, "@@IMAGE@@"),
	})
}

func (w *world) manifests(files map[string]string) string {
	w.t.Helper()
	dir, err := os.MkdirTemp(w.dir, "manifests-")
	if err != nil {
		w.t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
	return dir
}

func (w *world) applyEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"DEPLOY_MANIFESTS":         "/workspace/deploy",
		"DEPLOY_REPOSITORY_URL":    w.repoURL,
		"DEPLOY_COMMIT":            w.commit,
		"DEPLOY_IMAGE_ARTIFACT_ID": string(w.art),
		"DEPLOY_IMAGE":             w.nodeRef,
		"DEPLOY_READY_TIMEOUT":     "90",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// apply runs apply.sh as a fresh attempt of the logical action key.
func (w *world) apply(key, manifests string, extra map[string]string) result {
	w.t.Helper()
	return w.run(step{script: "apply.sh", actionKey: key, manifests: manifests, env: w.applyEnv(extra)})
}

func (w *world) lookup(key string, extra map[string]string) result {
	w.t.Helper()
	return w.run(step{script: "lookup.sh", actionKey: key, env: extra})
}

func (w *world) requireCompleted(r result) map[string]any {
	w.t.Helper()
	if r.Exit != 0 || r.outcome() != "completed" {
		w.t.Fatalf("expected a completed result, got exit %d manifest %v\ncallbacks:\n%s\nstderr:\n%s", r.Exit, r.Manifest, w.cb.describe(), tail(r.Stderr, 3000))
	}
	return r.outputs()
}

func (w *world) requireFailed(r result, reason string) {
	w.t.Helper()
	if r.Exit != 0 || r.outcome() != "failed" || r.reason() != reason {
		w.t.Fatalf("expected failure %q, got exit %d manifest %v\ncallbacks:\n%s\nstderr:\n%s", reason, r.Exit, r.Manifest, w.cb.describe(), tail(r.Stderr, 3000))
	}
}

// receiptAnnotations are the annotation values the record interface hands out for
// a deployment.
func receiptAnnotations(rec delivery.DeploymentRecord) map[string]string {
	d, err := delivery.RestoreDeployment(rec)
	if err != nil {
		panic(err)
	}
	return d.Annotations(delivery.DefaultAnnotations)
}

func (w *world) requireAnnotated(rec delivery.DeploymentRecord, kind, name string) map[string]any {
	w.t.Helper()
	obj := w.ns.object(w.t, kind, name)
	if obj == nil {
		w.t.Fatalf("%s/%s does not exist", kind, name)
	}
	got := annotationsOf(obj)
	for k, v := range receiptAnnotations(rec) {
		if got[k] != v {
			w.t.Fatalf("%s/%s annotation %s = %q, want %q", kind, name, k, got[k], v)
		}
	}
	return obj
}

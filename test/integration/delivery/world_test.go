package delivery_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"testing"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

var (
	scopeA = deliverytest.ScopeA
	scopeB = deliverytest.ScopeB
)

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
          imagePullPolicy: IfNotPresent
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

// world is one test's assembled stack — real PostgreSQL, the real execution
// service dispatching through the Podman runtime — and its own cluster
// namespace, with the project's preview environment bound to the
// namespace-scoped credential held as a project secret.
type world struct {
	t   *testing.T
	dir string
	S   *stack.Stack
	cl  *cluster
	ns  *namespace
	srv *stack.Server
	img stack.Image

	scope domain.Scope
	kube  string // the credential's secret name
	// kubeSecret is the credential's secret, for rotation and revocation.
	kubeSecret secrets.Secret
	env        delivery.Environment
	ttl        time.Duration
}

type worldOptions struct {
	stack stack.Options
	// ttl is the preview TTL of the environment (default 1 hour).
	ttl time.Duration
}

// worldSlots bounds the assembled stacks (each owns a PostgreSQL database, a
// cluster namespace and containers) alive at once, so the top-level tests can
// run in parallel without exhausting the scope's database connections.
var worldSlots = make(chan struct{}, 2)

func newWorld(t *testing.T, label string, o worldOptions) *world {
	t.Helper()
	worldSlots <- struct{}{}
	t.Cleanup(func() { <-worldSlots })
	cl := requireCluster(t)
	img := toolsImage(t)
	w := &world{t: t, cl: cl, dir: t.TempDir(), img: img, scope: scopeA, kube: "kubeconfig"}
	w.S = stack.New(t, o.stack)
	w.srv = w.S.UsePodman(w.dir)
	w.ns = cl.newNamespace(t, w.dir, label)
	w.bindEnvironment(w.scope, w.ns, o.ttl)
	return w
}

// bindEnvironment stores the namespace-scoped kubeconfig as the project's
// kubeconfig secret — what the entrypoint materializes as the file the script
// reads — and creates the preview environment over it.
func (w *world) bindEnvironment(scope domain.Scope, ns *namespace, ttl time.Duration) {
	w.t.Helper()
	raw, err := os.ReadFile(ns.kubeconfig)
	if err != nil {
		w.t.Fatal(err)
	}
	w.kubeSecret = w.S.Kubeconfig(scope, w.kube, string(raw))
	if ttl == 0 {
		ttl = time.Hour
	}
	w.ttl = ttl
	w.env = w.S.Environment(scope, stack.EnvSpec{Name: "preview", Namespace: ns.name, Kubeconfig: w.kube, PreviewTTL: &ttl})
}

// flow admits a build and the deploy run of the candidate whose deploy/ directory
// holds the example application (one replica unless the options carry other
// manifests).
func (w *world) flow(o stack.FlowOptions) *stack.Flow {
	w.t.Helper()
	o.Image = w.img
	if o.Manifests == nil {
		o.Manifests = w.appManifests(1, "inventory")
	}
	return w.S.NewFlow(w.scope, w.env, o)
}

// appManifests are the example application's files (a ConfigMap and a Deployment
// whose image is the @@IMAGE@@ token).
func (w *world) appManifests(replicas int, name string) map[string]string {
	return map[string]string{
		"config.yaml": fmt.Sprintf(configMap, name+"-config"),
		"app.yaml":    fmt.Sprintf(sleeperDeployment, name, replicas, "@@IMAGE@@"),
	}
}

// run dispatches the flow's next attempt through the Podman runtime.
func (w *world) run(f *stack.Flow) *stack.Outcome {
	w.t.Helper()
	return w.S.Execute(f)
}

// outputs are the accepted result's outputs of the attempt, by name.
func outputs(o *stack.Outcome) map[string]any {
	out := map[string]any{}
	if o.Attempt.Result == nil {
		return out
	}
	for k, v := range o.Attempt.Result.Outputs {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		var x any
		if err := json.Unmarshal(b, &x); err != nil {
			panic(err)
		}
		out[k] = x
	}
	return out
}

func (w *world) requireCompleted(o *stack.Outcome) map[string]any {
	w.t.Helper()
	if !o.Succeeded() {
		w.t.Fatalf("expected an accepted completed result: attempt %s/%s reason %q run %s\ncalls:\n%s\ncontainer output:\n%s",
			o.Attempt.Status, o.Attempt.Reason, o.Reason(), o.Agg.Run.Status, w.srv.Describe(), tail(o.Logs, 3500))
	}
	return outputs(o)
}

func (w *world) requireFailed(o *stack.Outcome, reason string) {
	w.t.Helper()
	if o.Attempt.Status != execution.AttemptFailed || o.Reason() != reason {
		w.t.Fatalf("expected the attempt to fail with %q: attempt %s/%s reason %q run %s\ncalls:\n%s\ncontainer output:\n%s",
			reason, o.Attempt.Status, o.Attempt.Reason, o.Reason(), o.Agg.Run.Status, w.srv.Describe(), tail(o.Logs, 3500))
	}
}

// deployment reads the persisted record by the logical action of the flow's step.
func (w *world) deployment(f *stack.Flow) (delivery.DeploymentRecord, bool) {
	w.t.Helper()
	d, err := w.S.DeliveryStore.FindByAction(context.Background(), f.Scope, execution.ActionKey(f.Run.ID, "deploy"))
	if err != nil {
		return delivery.DeploymentRecord{}, false
	}
	return d.Record(), true
}

func (w *world) mustDeployment(f *stack.Flow) delivery.DeploymentRecord {
	w.t.Helper()
	rec, ok := w.deployment(f)
	if !ok {
		w.t.Fatalf("no deployment is recorded for run %s", f.Run.ID)
	}
	return rec
}

func (w *world) mustDeploymentID(id delivery.DeploymentID) delivery.DeploymentRecord {
	w.t.Helper()
	d, err := w.S.DeliveryStore.GetDeployment(context.Background(), w.scope, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return d.Record()
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

// countObjects counts the example objects carrying the deployment's id.
func (w *world) countObjects(depID string) int {
	w.t.Helper()
	n := 0
	for _, kind := range []string{"configmap", "deployment"} {
		out := w.ns.admin(w.t, "get", kind, "-o", "json")
		var list struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			w.t.Fatal(err)
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

func resourceVersion(obj map[string]any) string {
	md, _ := obj["metadata"].(map[string]any)
	s, _ := md["resourceVersion"].(string)
	return s
}

// runStatus is the status the flow's run settles to (the repair passes that
// settle a failed attempt and end its run run until it is terminal).
func (w *world) runStatus(f *stack.Flow) execution.RunStatus {
	return w.S.AwaitTerminal(f.Scope, f.Run.ID)
}

// addScope binds another project's (or tenant's) own namespace and preview
// environment in the same stack: the same database, runtime API and runtime,
// nothing else shared.
func (w *world) addScope(scope domain.Scope, label string) *world {
	w.t.Helper()
	n := *w
	n.scope = scope
	n.ns = w.cl.newNamespace(w.t, w.dir, label)
	n.bindEnvironment(scope, n.ns, w.ttl)
	return &n
}

// secretResolutions counts the audited resolutions of the credential's version
// by the attempt (the entrypoint resolved exactly the pinned binding).
func (w *world) secretResolutions(attempt domain.AttemptID, version int64) int {
	w.t.Helper()
	return w.S.Count(`SELECT count(*) FROM audit WHERE action = 'secret.resolve' AND resource_id = $1 AND resource_version = $2 AND actor_id = $3`,
		w.kubeSecret.ID(), version, string(attempt))
}

func stackFlow() stack.FlowOptions { return stack.FlowOptions{} }

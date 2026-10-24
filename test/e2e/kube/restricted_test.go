package kube_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

// platformPort is the host port of a dependency the platform (never a workload)
// uses: PostgreSQL, NATS or the object store of the scope. A Pod reaches the
// host on the same port, so a probe from the Pod measures the tenant policy.
func platformPort(t *testing.T, env string) int {
	t.Helper()
	raw := os.Getenv(env)
	if raw == "" {
		t.Fatalf("%s is not set: run in the cluster stack scope (make dev-preflight)", env)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		t.Fatalf("%s has no port", env)
	}
	port, err := strconv.Atoi(u.Port())
	must(t, err)
	return port
}

// listenOther opens a second host port that accepts connections: egress to it is
// refused only by the network policy.
func listenOther(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	must(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// restrictedPod (PX-04, IT-07, IT-08, IT-A06): the injected entrypoint runs
// under the restricted baseline: exact images, bounded private storage and
// resources, token delivery, no privileged credentials or API access, and actual
// default-deny networking with only the runtime egress allowed.
func restrictedPod(t *testing.T) {
	s := newStack(t)
	other := listenOther(t)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(s.srv.URL, "https://"))
	apiSvc := s.mustKubectl("-n", "default", "get", "svc", "kubernetes", "-o", "jsonpath={.spec.clusterIP}")
	nodeIP := s.mustKubectl("get", "nodes", "-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`)
	script := fmt.Sprintf(`sleep 15
probe() { if nc -z -w 3 "$1" "$2" 2>/dev/null; then printf OPEN; else printf CLOSED; fi; }
net="runtime=$(probe %[1]s %[2]s) other=$(probe %[1]s %[3]d) apisvc=$(probe %[4]s 443) apinode=$(probe %[5]s 6443) internet=$(probe 1.1.1.1 443) postgres=$(probe %[1]s %[6]d) nats=$(probe %[1]s %[7]d) s3=$(probe %[1]s %[8]d)"
sec="uid=$(id -u) gid=$(id -g) satoken=$([ -e /var/run/secrets/kubernetes.io/serviceaccount/token ] && printf yes || printf no) toolsro=$(touch /kodama/bin/x 2>/dev/null && printf no || printf yes) tokenfile=$([ -s /kodama/identity/token ] && printf yes || printf no) admenv=$(env | grep -c adm_ || true) nnp=$(awk '/NoNewPrivs/{print $2}' /proc/self/status) capeff=$(awk '/CapEff/{print $2}' /proc/self/status) seccomp=$(awk '/^Seccomp:/{print $2}' /proc/self/status)"
mounts="identity=$(awk '$2=="/kodama/identity"{print $3}' /proc/mounts) secrets=$(awk '$2=="/kodama/secrets"{print $3}' /proc/mounts)"
printf '{"outcome":"completed","outputs":{"net":"%%s","sec":"%%s","mounts":"%%s"}}' "$net" "$sec" "$mounts" > "$KODAMA_RESULTS_FILE"
`, host, port, other, apiSvc, nodeIP, platformPort(t, "KODAMA_TEST_POSTGRES_ADMIN_URL"), platformPort(t, "KODAMA_NATS_URL"), platformPort(t, "KODAMA_S3_ENDPOINT"))
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: script, Outputs: []string{"net", "sec", "mounts"}, Timeout: 180})
	ctx := context.Background()
	claimed, err := s.svc.DispatchNext(ctx, executiontest.ScopeA.TenantID)
	if err != nil || !claimed {
		t.Fatalf("dispatch: %v %v", claimed, err)
	}
	in := &execution.AttemptSpec{Scope: run.Scope, RunID: run.ID}
	agg := s.e.Get(run)
	in.AttemptID, in.WorkloadName = agg.Attempts[0].ID, agg.Attempts[0].WorkloadName
	s.until(3*time.Minute, "the run to succeed", func() (bool, string) {
		st := s.e.Get(run).Run.Status
		at, _ := s.attempt(in)
		return st == execution.RunSucceeded, fmt.Sprintf("run %s, attempt %s (%s/%s)", st, at.Status, at.Class, at.Reason)
	})
	agg = s.e.Get(run)
	if at, _ := s.attempt(in); at.Status != execution.AttemptSucceeded {
		t.Fatalf("attempt %s", at.Status)
	}

	// Actual default-deny networking with the runtime egress allowed.
	net := strOutput(t, agg, "net")
	for _, want := range []string{"runtime=OPEN", "other=CLOSED", "apisvc=CLOSED", "apinode=CLOSED", "internet=CLOSED", "postgres=CLOSED", "nats=CLOSED", "s3=CLOSED"} {
		if !strings.Contains(net, want) {
			t.Fatalf("network enforcement: %s (want %s)", net, want)
		}
	}
	// No privileges, no service-account token, read-only tools, token delivered.
	sec := strOutput(t, agg, "sec")
	for _, want := range []string{"uid=65532", "gid=65532", "satoken=no", "toolsro=yes", "tokenfile=yes", "admenv=0", "nnp=1", "capeff=0000000000000000", "seccomp=2"} {
		if !strings.Contains(sec, want) {
			t.Fatalf("restricted shape: %s (want %s)", sec, want)
		}
	}
	if m := strOutput(t, agg, "mounts"); !strings.Contains(m, "identity=tmpfs") || !strings.Contains(m, "secrets=tmpfs") {
		t.Fatalf("memory-backed volumes: %s", m)
	}

	// The Job and Pod in the cluster carry exactly the planned images, bounds and
	// the committed fingerprint, and still verify against committed data.
	job, ok := s.jobOf(in)
	if !ok {
		t.Fatal("a completed Job is retained until its result is settled")
	}
	rec, err := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
	must(t, err)
	exp, err := s.policy.Expect(rec)
	must(t, err)
	if _, err := kube.VerifyJob(exp, job); err != nil {
		t.Fatalf("the live Job no longer verifies: %v", err)
	}
	spec := job.Spec.Template.Spec
	if spec.InitContainers[0].Image != s.c.injector || spec.Containers[0].Image != s.c.workload {
		t.Fatalf("images: %s %s", spec.InitContainers[0].Image, spec.Containers[0].Image)
	}
	if *job.Spec.BackoffLimit != 0 || len(spec.Containers) != 1 {
		t.Fatal("one Job, one primary container, backoffLimit 0")
	}
	pods := s.podsOf(in)
	if len(pods) != 1 {
		t.Fatalf("%d Pods", len(pods))
	}
	p := pods[0]
	if p.Spec.Containers[0].Resources.Limits["memory"] != "256Mi" || p.Spec.Containers[0].Resources.Limits["ephemeral-storage"] != "512Mi" {
		t.Fatalf("resource bounds: %+v", p.Spec.Containers[0].Resources)
	}
	for _, v := range p.Spec.Volumes {
		if v.EmptyDir != nil && v.EmptyDir.SizeLimit == "" {
			t.Fatalf("unbounded volume %s", v.Name)
		}
	}
	if p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken {
		t.Fatal("the Pod mounts a service-account token")
	}
	// The credential is attempt-scoped and only the Secret holds it.
	if !s.secretExists(in) {
		t.Fatal("the admission Secret should be retained until settlement")
	}
}

// restrictedVariantsRejected: the cluster itself refuses privileged and
// rootful Pods in the tenant namespace, and the executor never builds them.
func restrictedVariantsRejected(t *testing.T) {
	s := newStack(t)
	img := s.c.workload
	manifest := func(sc string, extra string) string {
		return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: bad-%s, namespace: %s}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
%s  containers:
  - name: c
    image: %s
    command: [sleep, "1"]
    resources: {requests: {cpu: 10m, memory: 8Mi, ephemeral-storage: 8Mi}, limits: {cpu: 100m, memory: 64Mi, ephemeral-storage: 64Mi}}
    securityContext: %s
`, randHex(3), s.nsA, extra, img, sc)
	}
	cases := map[string]string{
		"privileged":      manifest(`{privileged: true, capabilities: {drop: [ALL]}, runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}`, ""),
		"root":            manifest(`{allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: false, runAsUser: 0, seccompProfile: {type: RuntimeDefault}}`, ""),
		"host network":    manifest(`{allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}`, "  hostNetwork: true\n"),
		"added caps":      manifest(`{allowPrivilegeEscalation: false, capabilities: {drop: [ALL], add: [SYS_ADMIN]}, runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}`, ""),
		"no seccomp":      manifest(`{allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, runAsUser: 65532}`, ""),
		"privilege escal": manifest(`{allowPrivilegeEscalation: true, capabilities: {drop: [ALL]}, runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}`, ""),
		"host path":       manifest(`{allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}`, "  volumes: [{name: h, hostPath: {path: /}}]\n"),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := runStdin(doc, kubectlBin(), "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-")
			if err == nil || !strings.Contains(out+err.Error(), "violates PodSecurity") {
				t.Fatalf("the cluster accepted the variant: %v %s", err, out)
			}
		})
	}
	// A compliant Pod of the same shape is admitted: the refusals are the policy.
	ok := manifest(`{allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}`, "")
	if out, err := runStdin(ok, kubectlBin(), "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-"); err != nil {
		t.Fatalf("a compliant Pod was refused: %v %s", err, out)
	}
}

// Test_kube_restricted_baseline (IT-A06, PX-04, IT-07, IT-08): the production
// runtime's restricted baseline on the cluster. The Pod the executor builds is
// non-root, capability-less, token-less, bounded in storage and resources and
// reaches only the runtime callback (PostgreSQL, NATS, the object store, the
// API server and the internet are denied by the tenant default-deny policy,
// probed from inside the Pod); the cluster itself refuses privileged variants;
// the namespace quota denies workloads beyond its bound; and the tenant's service
// account holds no permission.
func Test_kube_restricted_baseline(t *testing.T) {
	t.Run("the executor's Pod and the enforced network", restrictedPod)
	t.Run("privileged and rootful variants are denied", restrictedVariantsRejected)
	t.Run("the tenant service account holds no permission", serviceAccountDenied)
	t.Run("the quota denies workloads beyond its bound", quotaDenied)
	t.Run("storage is bounded", storageBounded)
}

// serviceAccountDenied: the execution service account of the tenant namespace
// can do nothing against the API: it cannot read Secrets (the platform's data),
// create Pods or Jobs, or reach another tenant's namespace.
func serviceAccountDenied(t *testing.T) {
	s := newStack(t)
	sa := "system:serviceaccount:" + s.nsA + ":t089-runner"
	for _, c := range []struct{ verb, resource, ns string }{
		{"get", "secrets", s.nsA}, {"list", "secrets", s.nsA}, {"create", "pods", s.nsA}, {"create", "jobs", s.nsA},
		{"get", "pods", s.nsB}, {"get", "secrets", s.nsB}, {"get", "secrets", "kodama-deps"}, {"get", "nodes", ""},
	} {
		args := []string{"auth", "can-i", c.verb, c.resource, "--as", sa}
		if c.ns != "" {
			args = append(args, "-n", c.ns)
		}
		out, _ := s.kubectl(args...)
		if strings.TrimSpace(out) != "no" {
			t.Fatalf("%s may %s %s in %q: %q", sa, c.verb, c.resource, c.ns, out)
		}
	}
}

// quotaDenied: a namespace whose pod quota is exhausted refuses another Pod of the
// Job. The attempt never starts, no container runs, the quota of one tenant does
// not slow another's work, and the cancellation settles the denied attempt.
func quotaDenied(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	// One blocker Pod uses the whole quota of tenant A's namespace.
	s.mustKubectl("-n", s.nsA, "patch", "resourcequota", "compute", "--type", "merge", "-p", `{"spec":{"hard":{"pods":"1"}}}`)
	blocker := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: blocker, namespace: %s}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: c
    image: %s
    command: [sleep, "600"]
    resources: {requests: {cpu: 10m, memory: 8Mi, ephemeral-storage: 8Mi}, limits: {cpu: 100m, memory: 64Mi, ephemeral-storage: 64Mi}}
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
`, s.nsA, s.c.workload)
	if out, err := runStdin(blocker, kubectlBin(), "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-"); err != nil {
		t.Fatalf("blocker: %v %s", err, out)
	}
	// The quota's status must account for the blocker before the Job is created.
	eventually(t, 30*time.Second, "the quota to be consumed", func() (bool, string) {
		out, _ := s.kubectl("-n", s.nsA, "get", "resourcequota", "compute", "-o", "jsonpath={.status.used.pods}")
		return out == "1", out
	})

	runA, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
	inA := s.dispatch(runA)
	// Another tenant's namespace is unaffected and completes its run meanwhile.
	runB, _ := s.submit(executiontest.ScopeB, stepSpec{Script: quick, Outputs: []string{"v"}})
	inB := s.dispatch(runB)
	s.settle(runB, inB)

	eventually(t, 2*time.Minute, "the Job controller to be denied by the quota", func() (bool, string) {
		out, _ := s.kubectl("-n", s.nsA, "get", "events", "--field-selector", "involvedObject.name="+inA.WorkloadName, "-o", "jsonpath={range .items[*]}{.reason}: {.message}{\"\\n\"}{end}")
		return strings.Contains(out, "exceeded quota"), out
	})
	if pods := s.podsOf(inA); len(pods) != 0 {
		t.Fatalf("a Pod exists beyond the quota: %d", len(pods))
	}
	if st := s.attemptStatus(inA); st == execution.AttemptSucceeded {
		t.Fatalf("an attempt succeeded without a Pod: %s", st)
	}
	if n := s.admitCount(inA); n != 0 {
		t.Fatalf("%d admissions for an attempt that has no Pod", n)
	}

	// Cancelling removes the denied Job. The runtime cannot prove a launch that
	// left no Pod behind never happened, so execution waits for trusted evidence
	// of the effect (a reconciler's verdict); the cluster holds that evidence: the
	// namespace never had a Pod of the attempt.
	if _, err := s.svc.Cancel(ctx, s.caller(), runA.Scope, runA.ID); err != nil {
		t.Fatal(err)
	}
	s.e.Effects.Set(execution.VerdictSettled, nil)
	s.until(3*time.Minute, "the denied run to settle", func() (bool, string) {
		st := s.e.Get(runA).Run.Status
		return st.Terminal(), string(st)
	})
	s.until(2*time.Minute, "the denied Job to be removed", func() (bool, string) {
		_, still := s.jobOf(inA)
		return !still, "the Job still exists"
	})
	if s.held() != 0 {
		t.Fatalf("%d reservations still held", s.held())
	}
}

// storageBounded: a workload that writes beyond its bounded private volume is
// evicted by the kubelet instead of filling the node, and its attempt fails
// without a result; the volumes the executor builds all carry a size limit.
func storageBounded(t *testing.T) {
	s := newStack(t)
	// The outputs volume is limited to 32Mi: write 200Mi into it.
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: "dd if=/dev/zero of=/kodama/outputs/big bs=1M count=200 2>/dev/null\nsleep 120\n" + quick, Outputs: []string{"v"}, Timeout: 240})
	in := s.dispatch(run)
	s.until(4*time.Minute, "the attempt to fail", func() (bool, string) {
		st := s.attemptStatus(in)
		return st == execution.AttemptFailed, string(st)
	})
	events, err := s.kubectl("-n", s.nsA, "get", "events", "--field-selector", "reason=Evicted", "-o", "jsonpath={range .items[*]}{.message}{\"\\n\"}{end}")
	must(t, err)
	if !strings.Contains(events, `exceeds the limit "32Mi"`) {
		t.Fatalf("the kubelet did not evict the Pod over its volume limit: %q", events)
	}
	if got := s.e.Get(run).Run.Status; got == execution.RunSucceeded {
		t.Fatal("a workload that exceeded its storage bound succeeded")
	}
	if _, ok := s.e.Get(run).Step("work").Outputs["v"]; ok {
		t.Fatal("outputs were exposed for an evicted workload")
	}
}

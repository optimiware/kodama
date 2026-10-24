package kube_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

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

// Test_restricted_job (PX-04, IT-07, IT-08, IT-A06): the injected entrypoint runs
// under the restricted baseline: exact images, bounded private storage and
// resources, token delivery, no privileged credentials or API access, and actual
// default-deny networking with only the runtime egress allowed.
func Test_restricted_job(t *testing.T) {
	s := newStack(t)
	other := listenOther(t)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(s.srv.URL, "https://"))
	apiSvc := s.mustKubectl("-n", "default", "get", "svc", "kubernetes", "-o", "jsonpath={.spec.clusterIP}")
	nodeIP := s.mustKubectl("get", "nodes", "-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`)
	script := fmt.Sprintf(`sleep 15
probe() { if nc -z -w 3 "$1" "$2" 2>/dev/null; then printf OPEN; else printf CLOSED; fi; }
net="runtime=$(probe %[1]s %[2]s) other=$(probe %[1]s %[3]d) apisvc=$(probe %[4]s 443) apinode=$(probe %[5]s 6443) internet=$(probe 1.1.1.1 443)"
sec="uid=$(id -u) gid=$(id -g) satoken=$([ -e /var/run/secrets/kubernetes.io/serviceaccount/token ] && printf yes || printf no) toolsro=$(touch /kodama/bin/x 2>/dev/null && printf no || printf yes) tokenfile=$([ -s /kodama/identity/token ] && printf yes || printf no) admenv=$(env | grep -c adm_ || true) nnp=$(awk '/NoNewPrivs/{print $2}' /proc/self/status) capeff=$(awk '/CapEff/{print $2}' /proc/self/status) seccomp=$(awk '/^Seccomp:/{print $2}' /proc/self/status)"
mounts="identity=$(awk '$2=="/kodama/identity"{print $3}' /proc/mounts) secrets=$(awk '$2=="/kodama/secrets"{print $3}' /proc/mounts)"
printf '{"outcome":"completed","outputs":{"net":"%%s","sec":"%%s","mounts":"%%s"}}' "$net" "$sec" "$mounts" > "$KODAMA_RESULTS_FILE"
`, host, port, other, apiSvc, nodeIP)
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
	for _, want := range []string{"runtime=OPEN", "other=CLOSED", "apisvc=CLOSED", "apinode=CLOSED", "internet=CLOSED"} {
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

// Test_restricted_variants_rejected: the cluster itself refuses privileged and
// rootful Pods in the tenant namespace, and the executor never builds them.
func Test_restricted_variants_rejected(t *testing.T) {
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
			out, err := runStdin(doc, "kubectl", "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-")
			if err == nil || !strings.Contains(out+err.Error(), "violates PodSecurity") {
				t.Fatalf("the cluster accepted the variant: %v %s", err, out)
			}
		})
	}
	// A compliant Pod of the same shape is admitted: the refusals are the policy.
	ok := manifest(`{allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}`, "")
	if out, err := runStdin(ok, "kubectl", "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-"); err != nil {
		t.Fatalf("a compliant Pod was refused: %v %s", err, out)
	}
}

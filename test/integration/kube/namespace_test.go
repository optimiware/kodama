package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

// provision creates one tenant namespace exactly as the execution baseline
// requires (deployment owners do this in production; the fixtures provision
// their own isolated resources): Pod Security restricted, the tenant binding,
// a service account that mounts no token and holds no permission, a compute
// ResourceQuota, a default-deny NetworkPolicy and the single runtime egress.
func (s *stack) provision(ns, tenant string, p kube.ExecutionPolicy, opt stackOptions) {
	t := s.t
	t.Helper()
	items := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{
			"name": ns,
			"labels": map[string]string{
				"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest", "io.kodama.t021/fixture": "true",
			},
			"annotations": map[string]string{kube.AnnNamespaceTenant: tenant},
		}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "t021-runner", "namespace": ns}, "automountServiceAccountToken": false},
		{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": map[string]any{"name": "compute", "namespace": ns}, "spec": map[string]any{
			"hard": map[string]string{"pods": "20", "limits.cpu": "8", "limits.memory": "8Gi", "limits.ephemeral-storage": "20Gi"},
		}},
	}
	if !opt.NoNetworkPolicies {
		var egress []map[string]any
		for _, r := range p.RuntimeEgress() {
			var ports []map[string]any
			for _, pt := range r.Ports {
				ports = append(ports, map[string]any{"protocol": r.Protocol, "port": pt})
			}
			egress = append(egress, map[string]any{"to": []map[string]any{{"ipBlock": map[string]any{"cidr": r.CIDR}}}, "ports": ports})
		}
		items = append(items,
			map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "default-deny", "namespace": ns},
				"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Ingress", "Egress"}}},
			map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "runtime-egress", "namespace": ns},
				"spec": map[string]any{"podSelector": map[string]any{"matchLabels": map[string]string{kube.LabelManagedBy: kube.ValueManagedBy}}, "policyTypes": []string{"Egress"}, "egress": egress}},
		)
	}
	doc, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	cmd := exec.Command("kubectl", "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(doc))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provision %s: %v: %s", ns, err, out)
	}
}

// kubectl runs kubectl against the fixture cluster.
func (s *stack) kubectl(args ...string) (string, error) {
	return run("kubectl", append([]string{"--kubeconfig", s.c.kubeconfig}, args...)...)
}

func (s *stack) mustKubectl(args ...string) string {
	s.t.Helper()
	out, err := s.kubectl(args...)
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

var _ = fmt.Sprint
var _ = time.Second

func runStdin(stdin, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// apply applies documents to the fixture cluster.
func (s *stack) apply(items ...map[string]any) {
	s.t.Helper()
	doc, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	if out, err := runStdin(string(doc), "kubectl", "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-"); err != nil {
		s.t.Fatalf("apply: %v: %s", err, out)
	}
}

// Test_namespace_policy_semantics (PX-04, IT-07): the preflight judges the policy
// objects the real API server serves, not only fixtures. Port ranges, policies
// selecting Pods through the Job controller's labels, and quotas that do not
// cover executor Pods are refused before any workload exists; the compliant
// configuration remains usable. Each check builds a fresh preflight (and
// executor) so that it measures the current policy rather than a cached success.
func Test_namespace_policy_semantics(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	tenant := executiontest.ScopeA.TenantID
	ns := s.nsA

	fresh := func() error { return kube.NewPreflight(s.c.client, s.policy, 0, nil).Check(ctx, tenant) }
	if err := fresh(); err != nil {
		t.Fatalf("the compliant namespace is refused: %v", err)
	}

	// One claimed attempt, never dispatched while the namespace is non-compliant.
	_, _ = s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}, Image: unpullable})
	in := s.claim(tenant)
	if in == nil {
		t.Fatal("nothing to claim")
	}

	egressPolicy := func(name string, selector map[string]any, rules []map[string]any) map[string]any {
		return map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": map[string]any{"name": name, "namespace": ns},
			"spec":     map[string]any{"podSelector": selector, "policyTypes": []string{"Egress"}, "egress": rules}}
	}
	runtime := s.policy.RuntimeEgress()[0]
	allowed := func(ports ...map[string]any) []map[string]any {
		return []map[string]any{{"to": []map[string]any{{"ipBlock": map[string]any{"cidr": runtime.CIDR}}}, "ports": ports}}
	}
	port := runtime.Ports[0]
	managed := map[string]any{"matchLabels": map[string]string{kube.LabelManagedBy: kube.ValueManagedBy}}

	refuse := func(t *testing.T, what string) {
		t.Helper()
		if err := fresh(); !errors.Is(err, kube.ErrPreflight) {
			t.Fatalf("%s: preflight = %v", what, err)
		}
		x := s.newRuntime(s.c.client)
		if _, err := x.Start(ctx, *in); !errors.Is(err, kube.ErrPreflight) {
			t.Fatalf("%s: dispatch = %v", what, err)
		}
		if _, ok := s.jobOf(in); ok {
			t.Fatalf("%s: a workload was created in a non-compliant namespace", what)
		}
		if s.secretExists(in) {
			t.Fatalf("%s: credential material was created in a non-compliant namespace", what)
		}
	}

	t.Run("port range widening is refused", func(t *testing.T) {
		s.apply(egressPolicy("range", managed, allowed(map[string]any{"protocol": "TCP", "port": port, "endPort": port + 100})))
		refuse(t, "range")
		s.mustKubectl("-n", ns, "delete", "networkpolicy", "range", "--wait=true")
		if err := fresh(); err != nil {
			t.Fatalf("not usable after the range is removed: %v", err)
		}
	})

	t.Run("controller-label-selected permissive policies are refused", func(t *testing.T) {
		everything := []map[string]any{{"to": []map[string]any{{"ipBlock": map[string]any{"cidr": "0.0.0.0/0"}}}, "ports": []map[string]any{{"protocol": "TCP", "port": 443}}}}
		for name, label := range map[string]string{
			"by-job-name":       kube.PodLabelJobName,
			"by-controller-uid": kube.PodLabelJobUID,
			"by-legacy-name":    "job-name",
			"by-legacy-uid":     "controller-uid",
		} {
			s.apply(egressPolicy(name, map[string]any{"matchLabels": map[string]string{label: "x"}}, everything))
			refuse(t, name)
			s.mustKubectl("-n", ns, "delete", "networkpolicy", name, "--wait=true")
		}
		if err := fresh(); err != nil {
			t.Fatalf("not usable after the policies are removed: %v", err)
		}
	})

	t.Run("quotas that do not cover executor Pods are refused", func(t *testing.T) {
		hard := map[string]string{"pods": "20", "limits.cpu": "8", "limits.memory": "8Gi", "limits.ephemeral-storage": "20Gi"}
		quota := func(spec map[string]any) map[string]any {
			return map[string]any{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": map[string]any{"name": "compute", "namespace": ns}, "spec": spec}
		}
		s.mustKubectl("-n", ns, "delete", "resourcequota", "compute", "--wait=true")
		storage := map[string]any{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": map[string]any{"name": "storage", "namespace": ns},
			"spec": map[string]any{"hard": map[string]string{"limits.ephemeral-storage": "20Gi"}}}
		for name, docs := range map[string][]map[string]any{
			// The scoped quota is the only bound on Pods, CPU and memory; an unscoped
			// quota bounds storage alone.
			"scopes": {quota(map[string]any{"hard": map[string]string{"pods": "20", "limits.cpu": "8", "limits.memory": "8Gi"}, "scopes": []string{"NotTerminating"}}), storage},
			"scope selector": {quota(map[string]any{"hard": map[string]string{"pods": "20", "limits.cpu": "8", "limits.memory": "8Gi"}, "scopeSelector": map[string]any{"matchExpressions": []map[string]any{
				{"scopeName": "PriorityClass", "operator": "In", "values": []string{"t021-unrelated"}}}}}), storage},
		} {
			s.apply(docs...)
			// The API server keeps the scope the preflight must see.
			out := s.mustKubectl("-n", ns, "get", "resourcequota", "compute", "-o", "json")
			if !strings.Contains(out, "scope") {
				t.Fatalf("%s: the served quota carries no scope: %s", name, out)
			}
			refuse(t, name)
			s.mustKubectl("-n", ns, "delete", "resourcequota", "--all", "--wait=true")
		}
		refuse(t, "no quota")
		s.apply(quota(map[string]any{"hard": hard}))
		if err := fresh(); err != nil {
			t.Fatalf("not usable after the unscoped quota is restored: %v", err)
		}
	})

	t.Run("the compliant configuration dispatches", func(t *testing.T) {
		// A harmless policy for other Pods and an extra scoped quota do not disturb it.
		s.apply(egressPolicy("database", map[string]any{"matchLabels": map[string]string{"app": "database"}}, []map[string]any{{}}))
		defer s.mustKubectl("-n", ns, "delete", "networkpolicy", "database", "--wait=true")
		if err := fresh(); err != nil {
			t.Fatalf("a policy for other Pods denied the namespace: %v", err)
		}
		x := s.newRuntime(s.c.client)
		must(t, startErr(x.Start(ctx, *in)))
		if _, ok := s.jobOf(in); !ok {
			t.Fatal("no workload was created in the compliant namespace")
		}
	})
}

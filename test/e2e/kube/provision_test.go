package kube_test

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

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
				"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest", "io.kodama.t089/fixture": "true",
			},
			"annotations": map[string]string{kube.AnnNamespaceTenant: tenant},
		}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "t089-runner", "namespace": ns}, "automountServiceAccountToken": false},
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
	cmd := exec.Command(kubectlBin(), "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(doc))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provision %s: %v: %s", ns, err, out)
	}
}

// kubectl runs kubectl against the fixture cluster.
func (s *stack) kubectl(args ...string) (string, error) {
	return run(kubectlBin(), append([]string{"--kubeconfig", s.c.kubeconfig}, args...)...)
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
	if out, err := runStdin(string(doc), kubectlBin(), "--kubeconfig", s.c.kubeconfig, "apply", "-f", "-"); err != nil {
		s.t.Fatalf("apply: %v: %s", err, out)
	}
}

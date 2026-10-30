package gitworker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kube/gitworker"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func policy(t *testing.T, o gitworker.Objects, name string) kubernetes.NetworkPolicy {
	t.Helper()
	for _, p := range o.Policies {
		if p.Metadata.Name == name {
			return p
		}
	}
	t.Fatalf("no policy %s", name)
	return kubernetes.NetworkPolicy{}
}

func canonicalJSON(v any) string {
	b, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(b, &x)
	out, _ := json.Marshal(x)
	return string(out)
}

// Test_unit_gitworker_network_ingress_from_control_plane_only (T-129,
// unit_gitworker_network_ingress_from_control_plane_only, GR-03, IT-17, IT-A13):
// the namespace denies all traffic, admits ingress to the worker only from the
// control plane's Pods in the control plane's namespace on the worker's port,
// allows egress only to the control plane and cluster DNS, and a policy nobody
// rendered is refused rather than tolerated.
func Test_unit_gitworker_network_ingress_from_control_plane_only(t *testing.T) {
	spec := testSpec()
	o := mustRender(t, acme, spec)

	t.Run("default deny", func(t *testing.T) {
		p := policy(t, o, gitworker.PolicyDenyAll)
		if !p.Spec.PodSelector.Empty() || strings.Join(p.Spec.PolicyTypes, ",") != "Ingress,Egress" || len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 0 {
			t.Fatalf("not a default deny: %+v", p.Spec)
		}
	})

	t.Run("ingress", func(t *testing.T) {
		p := policy(t, o, gitworker.PolicyIngress)
		if strings.Join(p.Spec.PolicyTypes, ",") != "Ingress" || len(p.Spec.Egress) != 0 || len(p.Spec.Ingress) != 1 {
			t.Fatalf("shape: %+v", p.Spec)
		}
		if p.Spec.PodSelector.MatchLabels[gitworker.LabelApp] != gitworker.ValueApp || p.Spec.PodSelector.MatchLabels[gitworker.LabelTenantKey] != gitworker.TenantKey(acme) {
			t.Fatalf("selects %v", p.Spec.PodSelector.MatchLabels)
		}
		raw, _ := json.Marshal(p.Spec.Ingress[0])
		var rule struct {
			From []struct {
				NamespaceSelector *struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"namespaceSelector"`
				PodSelector *struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"podSelector"`
				IPBlock any `json:"ipBlock"`
			} `json:"from"`
			Ports []struct {
				Protocol string `json:"protocol"`
				Port     int    `json:"port"`
				EndPort  *int   `json:"endPort"`
			} `json:"ports"`
		}
		if err := json.Unmarshal(raw, &rule); err != nil {
			t.Fatal(err)
		}
		if len(rule.From) != 1 {
			t.Fatalf("peers: %s", raw)
		}
		peer := rule.From[0]
		// One peer carrying both selectors: a Pod of the control plane in the
		// control plane's namespace. Two peers would be an OR.
		if peer.NamespaceSelector == nil || peer.PodSelector == nil || peer.IPBlock != nil ||
			peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kodama" || len(peer.NamespaceSelector.MatchLabels) != 1 ||
			peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "kodama-server" || len(peer.PodSelector.MatchLabels) != 1 {
			t.Fatalf("peer: %s", raw)
		}
		if len(rule.Ports) != 1 || rule.Ports[0].Port != 8443 || rule.Ports[0].Protocol != "TCP" || rule.Ports[0].EndPort != nil {
			t.Fatalf("ports: %s", raw)
		}
	})

	t.Run("egress", func(t *testing.T) {
		p := policy(t, o, gitworker.PolicyEgress)
		if strings.Join(p.Spec.PolicyTypes, ",") != "Egress" || len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 2 {
			t.Fatalf("shape: %+v", p.Spec)
		}
		cp, dns := p.Spec.Egress[0], p.Spec.Egress[1]
		if len(cp.To) != 1 || cp.To[0].IPBlock != nil || cp.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kodama" ||
			cp.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "kodama-server" || len(cp.Ports) != 1 || cp.Ports[0].Port != int32(8080) {
			t.Fatalf("control-plane egress: %+v", cp)
		}
		if len(dns.To) != 1 || dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" ||
			dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" || len(dns.Ports) != 2 {
			t.Fatalf("dns egress: %+v", dns)
		}
		for _, port := range dns.Ports {
			if port.Port != int32(53) {
				t.Fatalf("dns port %v", port.Port)
			}
		}
		for _, r := range p.Spec.Egress {
			for _, peer := range r.To {
				if peer.IPBlock != nil {
					t.Fatal("egress to an address block: the worker may reach the database, broker or API server")
				}
			}
		}
	})

	t.Run("the policies exist before the worker", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		order := e.f.Mutations()
		dep := -1
		for i, c := range order {
			if c == "create deployment" {
				dep = i
			}
		}
		if dep < 0 {
			t.Fatalf("no deployment created: %v", order)
		}
		for i, c := range order {
			if c == "create networkpolicy" && i > dep {
				t.Fatalf("a policy was created after the worker: %v", order)
			}
		}
		if e.f.Calls("create networkpolicy") != 3 {
			t.Fatalf("policies created: %d", e.f.Calls("create networkpolicy"))
		}
	})

	t.Run("a policy nobody rendered is refused", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		// Policies are additive: this one would admit every Pod of an execution
		// namespace to the worker.
		e.f.putPolicy(nsOf(acme), kubernetes.NetworkPolicy{
			Metadata: kubernetes.ObjectMeta{Name: "allow-execution"},
			Spec: kubernetes.NetworkPolicySpec{
				PodSelector: kubernetes.LabelSelector{}, PolicyTypes: []string{"Ingress"},
				Ingress: []map[string]any{{"from": []map[string]any{{"namespaceSelector": map[string]any{}}}}},
			},
		})
		st, err := e.rec.Reconcile(context.Background(), acme, e.spec)
		if !isErr(err, gitworker.ErrIsolation) || st.Ready {
			t.Fatalf("ready=%v err=%v", st.Ready, err)
		}
		if _, err := e.rec.Endpoint(context.Background(), acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("a widened namespace has an endpoint: %v", err)
		}
		if e.f.Calls("delete networkpolicy") != 0 {
			t.Fatal("a foreign policy was deleted")
		}
	})

	t.Run("a widened owned policy is corrected under its UID", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		k := key(nsOf(acme), gitworker.PolicyIngress)
		e.f.mu.Lock()
		p := e.f.pols[k]
		uid := p.Metadata.UID
		p.Spec.Ingress = []map[string]any{{"from": []map[string]any{{"namespaceSelector": map[string]any{}}}}}
		e.f.pols[k] = p
		e.f.mu.Unlock()
		e.f.ResetCalls()
		st := e.reconcile(acme)
		if !st.Ready || e.f.Calls("replace networkpolicy") != 1 {
			t.Fatalf("%+v replaces=%d", st, e.f.Calls("replace networkpolicy"))
		}
		e.f.mu.Lock()
		got := e.f.pols[k]
		e.f.mu.Unlock()
		if got.Metadata.UID != uid || canonicalJSON(got.Spec) != canonicalJSON(policy(t, mustRender(t, acme, e.spec), gitworker.PolicyIngress).Spec) {
			t.Fatal("the policy was not restored in place")
		}
	})
}

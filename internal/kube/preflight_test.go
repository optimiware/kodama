package kube_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// Test_namespace_preflight: the tenant namespace baseline is checked by content
// and effective permission; anything missing, contradictory, overly permissive or
// unreadable denies.
func Test_namespace_preflight(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, mutate func(e *env, ns string)) error {
		e := newEnv(t)
		if mutate != nil {
			mutate(e, e.ns())
		}
		return kube.NewPreflight(e.fake, e.policy, 0, nil).Check(ctx, e.tenant)
	}
	if err := check(t, nil); err != nil {
		t.Fatalf("a compliant namespace is denied: %v", err)
	}
	peer := func(cidr string) kubernetes.NetworkPeer {
		return kubernetes.NetworkPeer{IPBlock: &kubernetes.IPBlock{CIDR: cidr}}
	}
	tcp := func(p float64) kubernetes.NetworkPort { return kubernetes.NetworkPort{Protocol: "TCP", Port: p} }
	workloadSel := map[string]string{kube.LabelManagedBy: kube.ValueManagedBy}
	addPolicy := func(e *env, ns string, p kubernetes.NetworkPolicy) {
		e.fake.NetPols[ns] = append(e.fake.NetPols[ns], p)
	}
	egress := func(rules ...kubernetes.NetworkEgress) kubernetes.NetworkPolicy {
		var p kubernetes.NetworkPolicy
		p.Metadata.Name = "extra"
		p.Spec.PodSelector.MatchLabels = workloadSel
		p.Spec.PolicyTypes = []string{"Egress"}
		p.Spec.Egress = rules
		return p
	}
	deny := map[string]func(e *env, ns string){
		"namespace missing": func(e *env, ns string) { delete(e.fake.Namespaces, ns) },
		"namespace not active": func(e *env, ns string) {
			n := e.fake.Namespaces[ns]
			n.Status.Phase = "Terminating"
			e.fake.Namespaces[ns] = n
		},
		"pod security baseline only": func(e *env, ns string) {
			n := e.fake.Namespaces[ns]
			n.Metadata.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}
			e.fake.Namespaces[ns] = n
		},
		"pod security missing": func(e *env, ns string) {
			n := e.fake.Namespaces[ns]
			n.Metadata.Labels = nil
			e.fake.Namespaces[ns] = n
		},
		"namespace of another tenant": func(e *env, ns string) {
			n := e.fake.Namespaces[ns]
			n.Metadata.Annotations = map[string]string{kube.AnnNamespaceTenant: "t021_globex"}
			e.fake.Namespaces[ns] = n
		},
		"namespace unbound": func(e *env, ns string) {
			n := e.fake.Namespaces[ns]
			n.Metadata.Annotations = nil
			e.fake.Namespaces[ns] = n
		},
		"service account missing": func(e *env, ns string) { delete(e.fake.ServiceAccounts, ns+"/t021-runner") },
		"service account automounts": func(e *env, ns string) {
			yes := true
			sa := e.fake.ServiceAccounts[ns+"/t021-runner"]
			sa.AutomountServiceAccountToken = &yes
			e.fake.ServiceAccounts[ns+"/t021-runner"] = sa
		},
		"service account automount unset": func(e *env, ns string) {
			sa := e.fake.ServiceAccounts[ns+"/t021-runner"]
			sa.AutomountServiceAccountToken = nil
			e.fake.ServiceAccounts[ns+"/t021-runner"] = sa
		},
		"service account reads secrets":   func(e *env, ns string) { e.fake.Permitted[ns+"/get/secrets"] = true },
		"service account execs into pods": func(e *env, ns string) { e.fake.Permitted[ns+"/create/pods/exec"] = true },
		"service account creates jobs":    func(e *env, ns string) { e.fake.Permitted[ns+"/create/jobs"] = true },
		"service account lists cluster":   func(e *env, ns string) { e.fake.Permitted["/list/nodes"] = true },
		"no quota":                        func(e *env, ns string) { e.fake.Quotas[ns] = nil },
		"quota without pods": func(e *env, ns string) {
			q := e.fake.Quotas[ns][0]
			delete(q.Spec.Hard, "pods")
			e.fake.Quotas[ns] = []kubernetes.ResourceQuota{q}
		},
		"quota without storage": func(e *env, ns string) {
			q := e.fake.Quotas[ns][0]
			delete(q.Spec.Hard, "limits.ephemeral-storage")
			e.fake.Quotas[ns] = []kubernetes.ResourceQuota{q}
		},
		"quota zero": func(e *env, ns string) {
			q := e.fake.Quotas[ns][0]
			q.Spec.Hard["limits.memory"] = "0"
			e.fake.Quotas[ns] = []kubernetes.ResourceQuota{q}
		},
		"no default deny": func(e *env, ns string) { e.fake.NetPols[ns] = e.fake.NetPols[ns][1:] },
		"default deny ingress only": func(e *env, ns string) {
			d := e.fake.NetPols[ns][0]
			d.Spec.PolicyTypes = []string{"Ingress"}
			e.fake.NetPols[ns][0] = d
		},
		"default deny with a rule": func(e *env, ns string) {
			d := e.fake.NetPols[ns][0]
			d.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}, Ports: []kubernetes.NetworkPort{tcp(8443)}}}
			e.fake.NetPols[ns][0] = d
		},
		"egress to everything": func(e *env, ns string) { addPolicy(e, ns, egress(kubernetes.NetworkEgress{})) },
		"egress anywhere on 443": func(e *env, ns string) {
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("0.0.0.0/0")}, Ports: []kubernetes.NetworkPort{tcp(443)}}))
		},
		"egress to the runtime on all ports": func(e *env, ns string) {
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}}))
		},
		"egress to the runtime network": func(e *env, ns string) {
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.0/24")}, Ports: []kubernetes.NetworkPort{tcp(8443)}}))
		},
		"egress on another port": func(e *env, ns string) {
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}, Ports: []kubernetes.NetworkPort{tcp(22)}}))
		},
		"egress on a named port": func(e *env, ns string) {
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}, Ports: []kubernetes.NetworkPort{{Port: "https"}}}))
		},
		"egress over udp": func(e *env, ns string) {
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}, Ports: []kubernetes.NetworkPort{{Protocol: "UDP", Port: float64(8443)}}}))
		},
		"egress to a namespace selector": func(e *env, ns string) {
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{{NamespaceSelector: &kubernetes.LabelSelector{}}}, Ports: []kubernetes.NetworkPort{tcp(8443)}}))
		},
		"egress by all-pods selector": func(e *env, ns string) {
			p := egress(kubernetes.NetworkEgress{})
			p.Spec.PodSelector = kubernetes.LabelSelector{}
			addPolicy(e, ns, p)
		},
		"selector expressions": func(e *env, ns string) {
			p := egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}, Ports: []kubernetes.NetworkPort{tcp(8443)}})
			p.Spec.PodSelector = kubernetes.LabelSelector{MatchExpressions: []map[string]any{{"key": "x", "operator": "Exists"}}}
			addPolicy(e, ns, p)
		},
		"egress over a port range": func(e *env, ns string) {
			end := int32(9000)
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}, Ports: []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(8443), EndPort: &end}}}))
		},
		"port range starting at an allowed port": func(e *env, ns string) {
			end := int32(8443)
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}, Ports: []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(8443), EndPort: &end}}}))
		},
		"scoped quota only": func(e *env, ns string) {
			q := e.fake.Quotas[ns][0]
			q.Spec.Scopes = []string{"BestEffort"}
			e.fake.Quotas[ns] = []kubernetes.ResourceQuota{q}
		},
		"scope selector quota only": func(e *env, ns string) {
			q := e.fake.Quotas[ns][0]
			q.Spec.ScopeSelector = &kubernetes.ScopeSelector{MatchExpressions: []kubernetes.ScopedResourceSelectorRequirement{{ScopeName: "PriorityClass", Operator: "In", Values: []string{"high"}}}}
			e.fake.Quotas[ns] = []kubernetes.ResourceQuota{q}
		},
		"a scoped quota supplies the missing bound": func(e *env, ns string) {
			q := e.fake.Quotas[ns][0]
			scoped := kubernetes.ResourceQuota{}
			scoped.Metadata.Name = "scoped"
			scoped.Spec.Hard = map[string]string{"limits.memory": q.Spec.Hard["limits.memory"]}
			scoped.Spec.Scopes = []string{"NotTerminating"}
			delete(q.Spec.Hard, "limits.memory")
			e.fake.Quotas[ns] = []kubernetes.ResourceQuota{q, scoped}
		},
		"a second quota zeroes a bound": func(e *env, ns string) {
			zero := kubernetes.ResourceQuota{}
			zero.Metadata.Name = "zero"
			zero.Spec.Hard = map[string]string{"pods": "0"}
			e.fake.Quotas[ns] = append(e.fake.Quotas[ns], zero)
		},
		"policy selecting the Job name label": func(e *env, ns string) {
			p := egress(kubernetes.NetworkEgress{})
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.PodLabelJobName: "some-job"}
			addPolicy(e, ns, p)
		},
		"policy selecting the Job controller UID label": func(e *env, ns string) {
			p := egress(kubernetes.NetworkEgress{})
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.PodLabelJobUID: "00000000-0000-0000-0000-000000000000"}
			addPolicy(e, ns, p)
		},
		"policy selecting a legacy Job label": func(e *env, ns string) {
			for _, k := range []string{"job-name", "controller-uid"} {
				p := egress(kubernetes.NetworkEgress{})
				p.Spec.PodSelector.MatchLabels = map[string]string{k: "x"}
				addPolicy(e, ns, p)
			}
		},
		"policy selecting managed-by with a Job label": func(e *env, ns string) {
			p := egress(kubernetes.NetworkEgress{})
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.LabelManagedBy: kube.ValueManagedBy, kube.PodLabelJobName: "j"}
			addPolicy(e, ns, p)
		},
		"controller label with an expression": func(e *env, ns string) {
			p := egress(kubernetes.NetworkEgress{})
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.PodLabelJobName: "j"}
			p.Spec.PodSelector.MatchExpressions = []map[string]any{{"key": "x", "operator": "Exists"}}
			addPolicy(e, ns, p)
		},
		"ingress to workload pods": func(e *env, ns string) {
			p := egress()
			p.Spec.PolicyTypes = []string{"Ingress"}
			p.Spec.Ingress = []map[string]any{{}}
			addPolicy(e, ns, p)
		},
	}
	for name, mutate := range deny {
		t.Run(name, func(t *testing.T) {
			err := check(t, mutate)
			if !errors.Is(err, kube.ErrPreflight) {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("a policy for other pods is ignored", func(t *testing.T) {
		err := check(t, func(e *env, ns string) {
			p := egress(kubernetes.NetworkEgress{})
			p.Spec.PodSelector.MatchLabels = map[string]string{"app": "database"}
			addPolicy(e, ns, p)
		})
		if err != nil {
			t.Fatalf("denied: %v", err)
		}
	})
	t.Run("policies demonstrably for other pods are ignored", func(t *testing.T) {
		for name, sel := range map[string]kubernetes.LabelSelector{
			"other managed-by value":         {MatchLabels: map[string]string{kube.LabelManagedBy: "someone-else"}},
			"other label with a Job label":   {MatchLabels: map[string]string{kube.PodLabelJobName: "j", "app": "database"}},
			"other label with an expression": {MatchLabels: map[string]string{"app": "database"}, MatchExpressions: []map[string]any{{"key": "x", "operator": "Exists"}}},
		} {
			err := check(t, func(e *env, ns string) {
				p := egress(kubernetes.NetworkEgress{})
				p.Spec.PodSelector = sel
				addPolicy(e, ns, p)
			})
			if err != nil {
				t.Fatalf("%s denied: %v", name, err)
			}
		}
	})
	t.Run("a scoped quota alongside an unscoped one is allowed", func(t *testing.T) {
		err := check(t, func(e *env, ns string) {
			scoped := kubernetes.ResourceQuota{}
			scoped.Metadata.Name = "scoped"
			scoped.Spec.Hard = map[string]string{"pods": "2"}
			scoped.Spec.Scopes = []string{"BestEffort"}
			e.fake.Quotas[ns] = append(e.fake.Quotas[ns], scoped)
		})
		if err != nil {
			t.Fatalf("denied: %v", err)
		}
	})
	t.Run("a bound split across unscoped quotas is aggregated", func(t *testing.T) {
		err := check(t, func(e *env, ns string) {
			q := e.fake.Quotas[ns][0]
			other := kubernetes.ResourceQuota{}
			other.Metadata.Name = "memory"
			other.Spec.Hard = map[string]string{"limits.memory": q.Spec.Hard["limits.memory"]}
			delete(q.Spec.Hard, "limits.memory")
			e.fake.Quotas[ns] = []kubernetes.ResourceQuota{q, other}
		})
		if err != nil {
			t.Fatalf("denied: %v", err)
		}
	})
	t.Run("a narrower egress inside the declared destination is allowed", func(t *testing.T) {
		err := check(t, func(e *env, ns string) {
			addPolicy(e, ns, egress(kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{peer("192.0.2.10/32")}, Ports: []kubernetes.NetworkPort{tcp(8443)}}))
		})
		if err != nil {
			t.Fatalf("denied: %v", err)
		}
	})
	t.Run("unreadable is denied, an outage is unavailable", func(t *testing.T) {
		for _, op := range []string{"get namespace", "get serviceaccount", "access review", "list resourcequotas", "list networkpolicies"} {
			e := newEnv(t)
			e.fake.Fault = func(o string, n int) *kubetest.Fault {
				if o == op {
					return &kubetest.Fault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrForbidden, Status: 403}}
				}
				return nil
			}
			if err := kube.NewPreflight(e.fake, e.policy, 0, nil).Check(ctx, e.tenant); !errors.Is(err, kube.ErrPreflight) {
				t.Fatalf("%s forbidden: %v", op, err)
			}
			e.fake.Fault = func(o string, n int) *kubetest.Fault {
				if o == op {
					return &kubetest.Fault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnavailable}}
				}
				return nil
			}
			err := kube.NewPreflight(e.fake, e.policy, 0, nil).Check(ctx, e.tenant)
			if !errors.Is(err, kube.ErrUnavailable) {
				t.Fatalf("%s outage: %v", op, err)
			}
		}
	})
	t.Run("tenant without a target", func(t *testing.T) {
		e := newEnv(t)
		if err := kube.NewPreflight(e.fake, e.policy, 0, nil).Check(ctx, "t021_stranger"); !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a success is cached only for its TTL", func(t *testing.T) {
		e := newEnv(t)
		p := kube.NewPreflight(e.fake, e.policy, time.Minute, e.clock.Now)
		if err := p.Check(ctx, e.tenant); err != nil {
			t.Fatal(err)
		}
		calls := e.fake.Calls("get namespace")
		if err := p.Check(ctx, e.tenant); err != nil || e.fake.Calls("get namespace") != calls {
			t.Fatal("not cached")
		}
		e.fake.Namespaces[e.ns()] = kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: e.ns()}}
		e.clock.Advance(2 * time.Minute)
		if err := p.Check(ctx, e.tenant); !errors.Is(err, kube.ErrPreflight) {
			t.Fatalf("stale cache: %v", err)
		}
	})
}

package kube_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Test_unit_kube_preflight_accepts_owned_run_policies: the namespace preflight of
// the run_shared baseline accepts exactly the per-run policies the adapter
// renders (by shape), at the pinned Pod Security level, and still refuses a
// foreign broad allow that selects run Pods and an owned-looking policy of any
// other shape.
func Test_unit_kube_preflight_accepts_owned_run_policies(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, stageBody())
	e.ensure(e.specs.Service(t, "app", false))
	e.ensure(e.specs.Service(t, "db", false))
	spec := e.specs.Network(t, []string{"app", "db"}, nil, map[string][]string{"build": {"registry"}})
	net := e.ensure(spec)
	// The adapter's own preflight verifies owned policies against their records; a
	// success is cached for the TTL, so the clock moves past it before each check.
	check := func(e *runEnv) error {
		e.clock.Advance(time.Minute)
		return e.rr.Preflight().CheckRun(ctx, e.specs.Scope.TenantID, e.body.PodSecurity)
	}
	if n := len(e.fake.NetPols[e.ns]); n != 2+4+2 { // chart, network (cli svc pub eg0), two ingress
		t.Fatalf("policies %d", n)
	}
	if err := check(e); err != nil {
		t.Fatalf("the adapter's own policies are denied: %v", err)
	}
	// The same namespace under the attempt_private baseline is not the run baseline.
	if err := kube.NewPreflight(e.fake, e.policy, 0, nil).Check(ctx, e.specs.Scope.TenantID); err == nil {
		t.Fatal("a baseline namespace passed the restricted check")
	}
	// A preflight with nothing to verify owned policies against refuses them: a
	// policy that merely has the shape of ours is not evidence.
	if err := kube.NewPreflight(e.fake, e.policy, 0, nil).CheckRun(ctx, e.specs.Scope.TenantID, e.body.PodSecurity); err == nil || !errors.Is(err, kube.ErrPreflight) {
		t.Fatalf("owned policies passed without a verifier: %v", err)
	}
	// A cache entry of one baseline is not the other's.
	cached := e.rr.Preflight()
	if err := cached.CheckRun(ctx, e.specs.Scope.TenantID, e.body.PodSecurity); err != nil {
		t.Fatal(err)
	}
	if err := cached.Check(ctx, e.specs.Scope.TenantID); err == nil {
		t.Fatal("a run_shared success satisfied the attempt_private check")
	}

	pub, _ := e.fake.GetNetworkPolicy(ctx, e.ns, net.Handle.Name+"-pub")
	cli, _ := e.fake.GetNetworkPolicy(ctx, e.ns, net.Handle.Name+"-cli")
	in, _ := e.fake.GetNetworkPolicy(ctx, e.ns, e.specs.Service(t, "db", false).Name+"-in")
	prof, _ := e.fake.GetNetworkPolicy(ctx, e.ns, net.Handle.Name+"-eg0")
	tenant := string(e.specs.Scope.TenantID)
	run := kube.RunKey(e.specs.Scope, e.specs.Run)

	clone := func(p kubernetes.NetworkPolicy, name string) kubernetes.NetworkPolicy {
		p.Metadata.Name, p.Metadata.UID, p.Raw = name, "", nil
		p.Metadata.Labels = map[string]string{kube.LabelResourceOwner: kube.ValueResourceOwner}
		p.Metadata.Annotations = map[string]string{kube.AnnTenant: tenant}
		p.Spec.Egress = append([]kubernetes.NetworkEgress(nil), p.Spec.Egress...)
		return p
	}
	except := pub.Spec.Egress[0].To[0].IPBlock.Except
	public := func(cidr string, ex []string, ports ...float64) kubernetes.NetworkEgress {
		var ps []kubernetes.NetworkPort
		for _, p := range ports {
			ps = append(ps, kubernetes.NetworkPort{Protocol: "TCP", Port: p})
		}
		return kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: cidr, Except: ex}}}, Ports: ps}
	}
	runSel := map[string]string{kube.LabelRunKey: run, kube.LabelRole: kube.RoleLabelStep}
	podPeer := func(l map[string]string) kubernetes.NetworkPeer {
		return kubernetes.NetworkPeer{PodSelector: &kubernetes.LabelSelector{MatchLabels: l}}
	}

	refuse := map[string]func(e *runEnv){
		"foreign broad egress selecting run Pods": func(e *runEnv) {
			p := clone(pub, "foreign-egress")
			p.Metadata.Labels = nil
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.LabelRunKey: run}
			p.Spec.Egress = []kubernetes.NetworkEgress{public("0.0.0.0/0", nil, 443)}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"foreign ingress selecting run Pods by role": func(e *runEnv) {
			p := clone(in, "foreign-ingress")
			p.Metadata.Labels = nil
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.LabelRole: kube.RoleLabelService}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"foreign allow-all selecting every Pod": func(e *runEnv) {
			var p kubernetes.NetworkPolicy
			p.Metadata.Name = "allow-all"
			p.Spec.PolicyTypes = []string{"Egress"}
			p.Spec.Egress = []kubernetes.NetworkEgress{{}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"owned policy of another tenant": func(e *runEnv) {
			p := clone(pub, "x")
			p.Metadata.Annotations[kube.AnnTenant] = "t021_other"
			p.Spec.Egress = pub.Spec.Egress
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"public egress without the private ranges": func(e *runEnv) {
			p := clone(pub, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{public("0.0.0.0/0", except[:3], 80, 443)}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"public egress on another port": func(e *runEnv) {
			p := clone(pub, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{public("0.0.0.0/0", except, 80, 443, 22)}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"public egress with no ports": func(e *runEnv) {
			p := clone(pub, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{public("0.0.0.0/0", except)}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"default route beside another block": func(e *runEnv) {
			p := clone(pub, "x")
			r := public("0.0.0.0/0", except, 80, 443)
			r.To = append(r.To, kubernetes.NetworkPeer{IPBlock: &kubernetes.IPBlock{CIDR: "10.1.0.0/16"}})
			p.Spec.Egress = []kubernetes.NetworkEgress{r}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"named destination too broad": func(e *runEnv) {
			p := clone(prof, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{public("10.0.0.0/7", nil, 443)}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"named destination at the metadata address": func(e *runEnv) {
			p := clone(prof, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{public("169.254.169.254/32", nil, 80)}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"all destinations": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{{Ports: []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(443)}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"a peer in another namespace": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{
				NamespaceSelector: &kubernetes.LabelSelector{MatchLabels: map[string]string{"team": "other"}}, PodSelector: &kubernetes.LabelSelector{MatchLabels: map[string]string{kube.LabelRunKey: run}}}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"a peer selected by arbitrary labels": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{podPeer(map[string]string{"app": "control-plane"})}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"selector outside the run labels": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.LabelRunKey: run, "app": "other"}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"selector without a run": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.LabelRole: kube.RoleLabelStep}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"selector expression": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.PodSelector.MatchExpressions = []map[string]any{{"key": "a", "operator": "Exists"}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"ingress from everywhere": func(e *runEnv) {
			p := clone(in, "x")
			p.Spec.Ingress = []map[string]any{{"ports": []any{map[string]any{"protocol": "TCP", "port": 5432}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"ingress from another namespace": func(e *runEnv) {
			p := clone(in, "x")
			p.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"namespaceSelector": map[string]any{}}}, "ports": []any{map[string]any{"protocol": "TCP", "port": 5432}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"ingress from an address block": func(e *runEnv) {
			p := clone(in, "x")
			p.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0"}}}, "ports": []any{map[string]any{"protocol": "TCP", "port": 5432}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"ingress on all ports": func(e *runEnv) {
			p := clone(in, "x")
			p.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{kube.LabelRunShared: "true"}}}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"ingress on a port range": func(e *runEnv) {
			p := clone(in, "x")
			p.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{kube.LabelRunShared: "true"}}}}, "ports": []any{map[string]any{"protocol": "TCP", "port": 1024, "endPort": 65535}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"UDP ingress": func(e *runEnv) {
			p := clone(in, "x")
			p.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{kube.LabelRunShared: "true"}}}}, "ports": []any{map[string]any{"protocol": "UDP", "port": 53}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"an unknown ingress field": func(e *runEnv) {
			p := clone(in, "x")
			p.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{kube.LabelRunShared: "true"}}}}, "ports": []any{map[string]any{"protocol": "TCP", "port": 5432}}, "extra": 1}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"rules without their policy type": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.PolicyTypes = []string{"Ingress"}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"no policy types": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.PolicyTypes = nil
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"pod and address peers mixed": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{podPeer(runSel), {IPBlock: &kubernetes.IPBlock{CIDR: "10.1.1.0/24"}}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"pod egress on a UDP port": func(e *runEnv) {
			p := clone(cli, "x")
			p.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{podPeer(runSel)}, Ports: []kubernetes.NetworkPort{{Protocol: "UDP", Port: float64(53)}}}}
			e.fake.NetPols[e.ns] = append(e.fake.NetPols[e.ns], p)
		},
		"namespace without the pinned Pod Security level": func(e *runEnv) {
			n := e.fake.Namespaces[e.ns]
			n.Metadata.Labels["pod-security.kubernetes.io/enforce"] = "restricted"
			e.fake.Namespaces[e.ns] = n
		},
		"namespace without the pinned Pod Security version": func(e *runEnv) {
			n := e.fake.Namespaces[e.ns]
			n.Metadata.Labels["pod-security.kubernetes.io/enforce-version"] = "v1.0"
			e.fake.Namespaces[e.ns] = n
		},
		"tenant may create policies": func(e *runEnv) { e.fake.Permitted[e.ns+"/create/networkpolicies"] = true },
		"tenant may create claims":   func(e *runEnv) { e.fake.Permitted[e.ns+"/create/persistentvolumeclaims"] = true },
		"no default deny": func(e *runEnv) {
			e.fake.NetPols[e.ns] = e.fake.NetPols[e.ns][1:]
		},
	}
	for name, mutate := range refuse {
		t.Run(name, func(t *testing.T) {
			x := newRunEnv(t, stageBody())
			x.ensure(x.specs.Service(t, "app", false))
			x.ensure(x.specs.Service(t, "db", false))
			x.ensure(spec)
			if err := check(x); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			mutate(x)
			err := check(x)
			if err == nil || !errors.Is(err, kube.ErrPreflight) {
				t.Fatalf("accepted: %v", err)
			}
			// A refused namespace never gets another resource (the adapter's own success
			// is cached for the preflight TTL).
			x.clock.Advance(time.Minute)
			before := x.fake.Calls("create pod")
			if err := x.ensureErr(x.specs.Service(t, "cache", false)); err == nil {
				t.Fatal("a resource was created in a namespace that fails the baseline")
			}
			if x.fake.Calls("create pod") != before {
				t.Fatal("a Pod was created in a refused namespace")
			}
		})
	}
}

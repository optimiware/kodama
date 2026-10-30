package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func policyOf(t *testing.T, e *runEnv, name string) kubernetes.NetworkPolicy {
	t.Helper()
	p, err := e.fake.GetNetworkPolicy(context.Background(), e.ns, name)
	if err != nil {
		t.Fatalf("policy %s: %v", name, err)
	}
	return p
}

func policyNames(e *runEnv, label, value string) []string {
	return e.fake.PoliciesWith(e.ns, label, value)
}

func egressBlocks(p kubernetes.NetworkPolicy) (cidrs []string, ports []float64) {
	for _, r := range p.Spec.Egress {
		for _, to := range r.To {
			if to.IPBlock != nil {
				cidrs = append(cidrs, to.IPBlock.CIDR)
			}
		}
		for _, pt := range r.Ports {
			if n, ok := pt.Port.(float64); ok {
				ports = append(ports, n)
			}
		}
	}
	sort.Float64s(ports)
	return
}

func peerLabels(r kubernetes.NetworkEgress) map[string]string {
	if len(r.To) != 1 || r.To[0].PodSelector == nil {
		return nil
	}
	return r.To[0].PodSelector.MatchLabels
}

func ingressFrom(t *testing.T, p kubernetes.NetworkPolicy) (peers []map[string]string, ports []float64) {
	t.Helper()
	raw, _ := json.Marshal(p.Spec.Ingress)
	var rules []struct {
		From  []kubernetes.NetworkPeer `json:"from"`
		Ports []kubernetes.NetworkPort `json:"ports"`
	}
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		for _, f := range r.From {
			peers = append(peers, f.PodSelector.MatchLabels)
		}
		for _, pt := range r.Ports {
			if n, ok := pt.Port.(float64); ok {
				ports = append(ports, n)
			}
		}
	}
	sort.Float64s(ports)
	return
}

func stageBody() execution.ExecutionPolicy {
	b := tenantBody()
	b.EgressProfiles = map[string]execution.EgressProfile{
		"registry": {Destinations: []execution.EgressDestination{{Host: "10.20.30.0/24", Ports: []int64{443, 5000}}}},
	}
	return b
}

// Test_unit_kube_run_network_policies_tenant_model: under the O-30 stage model
// the run's network resource adds only per-run, label-selected allow policies:
// the run's steps may reach the tenant's service Pods (the service's own ingress
// policy bounds ports and peers to the tenant's run_shared Pods), public egress
// is TCP 80/443 to the default route minus the private, link-local, CGNAT,
// loopback and configured cluster ranges, and a named profile adds exactly its
// destinations for the workloads that name it. Nothing else is allowed here:
// DNS and the Kodama callback stay with the chart's policies.
func Test_unit_kube_run_network_policies_tenant_model(t *testing.T) {
	e := newRunEnv(t, stageBody(), func(_ *kube.RunResourceDependencies, o *kube.RunResourceOptions) {
		o.ClusterCIDRs = []string{"203.0.113.0/24"}
	})
	svc := e.ensure(e.specs.Service(t, "db", false))
	spec := e.specs.Network(t, []string{"db"}, nil, map[string][]string{"build": {"registry"}, "service.db": {}, "preparation": {}})
	o := e.ensure(spec)
	if o.Phase != execution.PhaseReady || o.Handle.UID == "" || o.Fingerprint == "" {
		t.Fatalf("observation %+v", o)
	}
	rec, _ := e.fake.SecretOf(e.ns, spec.Name+"-rec")
	if rec.Metadata.UID != o.Handle.UID {
		t.Fatal("the network's instance identity is not its record's")
	}
	names := policyNames(e, kube.LabelResourceKind, "network")
	want := []string{spec.Name + "-cli", spec.Name + "-eg0", spec.Name + "-pub", spec.Name + "-svc"}
	if !eqStrings(names, want) {
		t.Fatalf("policies %v, want %v", names, want)
	}
	key := kube.RunKey(spec.Scope, spec.RunID)
	for _, n := range names {
		p := policyOf(t, e, n)
		if p.Metadata.Annotations[kube.AnnResourceUID] != rec.Metadata.UID || p.Metadata.Annotations[kube.AnnFingerprint] != o.Fingerprint ||
			p.Metadata.Labels[kube.LabelResourceOwner] != kube.ValueResourceOwner || len(p.Spec.Ingress) != 0 ||
			p.Spec.PodSelector.MatchLabels[kube.LabelRunKey] != key || len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != "Egress" {
			t.Fatalf("policy %s: %+v", n, p)
		}
	}
	cli := policyOf(t, e, spec.Name+"-cli")
	if cli.Spec.PodSelector.MatchLabels[kube.LabelRole] != kube.RoleLabelStep || len(cli.Spec.Egress) != 1 || len(cli.Spec.Egress[0].Ports) != 0 {
		t.Fatalf("client policy %+v", cli.Spec)
	}
	if got := peerLabels(cli.Spec.Egress[0]); got[kube.LabelRole] != kube.RoleLabelService || got[kube.LabelRunShared] != "true" || got[kube.LabelRunKey] != "" {
		t.Fatalf("tenant model peers %v", got)
	}
	pub := policyOf(t, e, spec.Name+"-pub")
	cidrs, ports := egressBlocks(pub)
	if len(cidrs) != 1 || cidrs[0] != "0.0.0.0/0" || len(ports) != 2 || ports[0] != 80 || ports[1] != 443 || pub.Spec.PodSelector.MatchLabels[kube.LabelRole] != kube.RoleLabelStep {
		t.Fatalf("public egress %v %v", cidrs, ports)
	}
	except := pub.Spec.Egress[0].To[0].IPBlock.Except
	for _, need := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "127.0.0.0/8", "203.0.113.0/24"} {
		found := false
		for _, x := range except {
			found = found || x == need
		}
		if !found {
			t.Fatalf("public egress does not exclude %s: %v", need, except)
		}
	}
	prof := policyOf(t, e, spec.Name+"-eg0")
	cidrs, ports = egressBlocks(prof)
	if len(cidrs) != 1 || cidrs[0] != "10.20.30.0/24" || len(ports) != 2 || ports[0] != 443 || ports[1] != 5000 ||
		prof.Spec.PodSelector.MatchLabels[kube.LabelWorkload] != kube.WorkloadHash(spec.Scope, spec.RunID, "build") {
		t.Fatalf("profile policy %+v", prof.Spec)
	}
	// The service's ingress (rendered with the service, where its ports are known)
	// admits the tenant's run_shared Pods on the declared port only.
	in := policyOf(t, e, svc.Handle.Name+"-in")
	peers, iports := ingressFrom(t, in)
	if len(peers) != 1 || peers[0][kube.LabelRunShared] != "true" || peers[0][kube.LabelRunKey] != "" || len(iports) != 1 || iports[0] != 5432 {
		t.Fatalf("service ingress %v %v", peers, iports)
	}

	// Verification and repair: a deleted policy is a lost boundary for a bound
	// network; a repeated Ensure restores it by name and fingerprint.
	pc := policyOf(t, e, spec.Name+"-pub")
	if err := e.fake.DeleteNetworkPolicy(context.Background(), e.ns, spec.Name+"-pub", kubernetes.DeleteOptions{UID: pc.Metadata.UID}); err != nil {
		t.Fatal(err)
	}
	if got := e.observe(o.Handle); got.Phase != execution.PhaseFailed {
		t.Fatalf("deleted policy: %+v", got)
	}
	if again := e.ensure(spec); again.Phase != execution.PhaseReady || again.Handle.UID != o.Handle.UID {
		t.Fatalf("repair %+v", again)
	}
	// A widened policy is not the run's (verified field by field).
	e.fake.MutateRaw("policy", e.ns, spec.Name+"-pub", func(d map[string]any) {
		sp := d["spec"].(map[string]any)
		sp["egress"].([]any)[0].(map[string]any)["ports"] = []any{map[string]any{"protocol": "TCP", "port": 22}}
	})
	if _, err := e.rr.Observe(context.Background(), o.Handle); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("a modified policy was accepted: %v", err)
	}
}

// Test_unit_kube_run_network_policy_digests: a named profile's destinations come
// from the pinned policy and must equal the digest the run pinned; a destination
// that is a general allow, or a host with no resolver, is refused with nothing created.
func Test_unit_kube_run_network_policy_digests(t *testing.T) {
	e := newRunEnv(t, stageBody())
	good := e.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})
	bad := good
	n := *good.Network
	n.Egress = map[string]string{"registry": "sha256:" + strings.Repeat("0", 64)}
	bad.Network = &n
	if err := e.ensureErr(bad); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("a mismatching digest was accepted: %v", err)
	}
	unknown := good
	n2 := *good.Network
	n2.Workloads = map[string][]string{"build": {"nope"}}
	unknown.Network = &n2
	if err := e.ensureErr(unknown); err == nil {
		t.Fatal("an unknown profile was accepted")
	}
	if len(e.fake.Names("secret", e.ns)) != 0 || len(policyNames(e, kube.LabelResourceOwner, kube.ValueResourceOwner)) != 0 {
		t.Fatal("a refused network left objects")
	}
	// A wrong model: the spec's model is the pinned policy's.
	wrong := good
	n3 := *good.Network
	n3.Isolation = execution.IsolationRun
	wrong.Network = &n3
	if err := e.ensureErr(wrong); err == nil {
		t.Fatal("a model other than the pinned one was accepted")
	}
	for name, dest := range map[string]execution.EgressDestination{
		"default route": {Host: "0.0.0.0/0", Ports: []int64{443}},
		"wide block":    {Host: "10.0.0.0/7", Ports: []int64{443}},
		"metadata":      {Host: "169.254.169.254", Ports: []int64{80}},
		"hostname":      {Host: "registry.example.invalid", Ports: []int64{443}},
		"no ports":      {Host: "10.1.1.1", Ports: nil},
	} {
		b := stageBody()
		b.EgressProfiles = map[string]execution.EgressProfile{"registry": {Destinations: []execution.EgressDestination{dest}}}
		x := newRunEnv(t, b)
		if err := x.ensureErr(x.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// Test_unit_kube_run_network_policies_run_model: under isolation: run, only the
// run's own steps reach its services, a service reaches another only along a
// declared edge (by port name, both directions), nothing is open to the public
// without publicEgress, and hostname destinations are resolved once and pinned.
func Test_unit_kube_run_network_policies_run_model(t *testing.T) {
	body := runBody()
	body.EgressProfiles = map[string]execution.EgressProfile{
		"mirror": {Destinations: []execution.EgressDestination{{Host: "mirror.example.invalid", Ports: []int64{443}}}},
	}
	res := &resolver{answers: map[string][]string{"mirror.example.invalid": {"198.51.100.7", "198.51.100.8"}}}
	e := newRunEnv(t, body, func(d *kube.RunResourceDependencies, _ *kube.RunResourceOptions) { d.Resolver = res })
	e.ensure(e.specs.Service(t, "app", false))
	db := e.ensure(e.specs.Service(t, "db", false))
	edges := []execution.EdgeSpec{{From: "app", To: "db", Ports: []string{"sql"}}}
	spec := e.specs.Network(t, []string{"app", "db"}, edges, map[string][]string{"build": {"mirror"}})
	o := e.ensure(spec)
	key := kube.RunKey(spec.Scope, spec.RunID)

	names := policyNames(e, kube.LabelResourceKind, "network")
	want := []string{spec.Name + "-cli", spec.Name + "-eg0", spec.Name + "-ei0", spec.Name + "-eo0", spec.Name + "-svc"}
	if !eqStrings(names, want) {
		t.Fatalf("policies %v, want %v (no public egress policy under publicEgress false)", names, want)
	}
	cli := policyOf(t, e, spec.Name+"-cli")
	if got := peerLabels(cli.Spec.Egress[0]); got[kube.LabelRunKey] != key || got[kube.LabelRole] != kube.RoleLabelService || got[kube.LabelRunShared] != "" {
		t.Fatalf("run model client peers %v", got)
	}
	svcDNS := policyOf(t, e, spec.Name+"-svc")
	if len(svcDNS.Spec.Egress) != 1 {
		t.Fatalf("under the run model a service reaches only DNS and its edges: %+v", svcDNS.Spec.Egress)
	}
	app, db2 := kube.WorkloadHash(spec.Scope, spec.RunID, "service.app"), kube.WorkloadHash(spec.Scope, spec.RunID, "service.db")
	eo, ei := policyOf(t, e, spec.Name+"-eo0"), policyOf(t, e, spec.Name+"-ei0")
	if eo.Spec.PodSelector.MatchLabels[kube.LabelWorkload] != app || peerLabels(eo.Spec.Egress[0])[kube.LabelWorkload] != db2 {
		t.Fatalf("edge egress %+v", eo.Spec)
	}
	if p := eo.Spec.Egress[0].Ports; len(p) != 1 || p[0].Port != "sql" {
		t.Fatalf("edge ports %+v", p)
	}
	peers, _ := ingressFrom(t, ei)
	if ei.Spec.PodSelector.MatchLabels[kube.LabelWorkload] != db2 || len(peers) != 1 || peers[0][kube.LabelWorkload] != app {
		t.Fatalf("edge ingress %+v", ei.Spec)
	}
	// The service ingress admits this run's steps only.
	in := policyOf(t, e, db.Handle.Name+"-in")
	if peers, _ := ingressFrom(t, in); len(peers) != 1 || peers[0][kube.LabelRunKey] != key || peers[0][kube.LabelRole] != kube.RoleLabelStep || peers[0][kube.LabelRunShared] != "" {
		t.Fatalf("service ingress under the run model: %v", peers)
	}
	// Hostnames were resolved once and pinned: later DNS drift neither changes the
	// policies nor makes them look foreign.
	cidrs, _ := egressBlocks(policyOf(t, e, spec.Name+"-eg0"))
	if len(cidrs) != 2 || cidrs[0] != "198.51.100.7/32" || cidrs[1] != "198.51.100.8/32" {
		t.Fatalf("pinned destinations %v", cidrs)
	}
	res.answers["mirror.example.invalid"] = []string{"203.0.113.9"}
	if got := e.observe(o.Handle); got.Phase != execution.PhaseReady || res.calls != 1 {
		t.Fatalf("after DNS drift: %+v (resolutions %d)", got, res.calls)
	}
	// An edge between undeclared services or by an invalid port name is refused.
	bad := e.specs.Network(t, []string{"app"}, []execution.EdgeSpec{{From: "app", To: "ghost", Ports: []string{"sql"}}}, nil)
	bad.Name = spec.Name
	if err := e.ensureErr(bad); err == nil {
		t.Fatal("an edge to an undeclared service was accepted")
	}
}

type resolver struct {
	answers map[string][]string
	calls   int
}

func (r *resolver) ResolveHost(_ context.Context, host string) ([]netip.Addr, error) {
	r.calls++
	var out []netip.Addr
	for _, a := range r.answers[host] {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

// Test_unit_kube_run_network_teardown: stopping a network fences it, removing it
// deletes exactly the policies that verify as its own (by marks, name and instance
// anchor) and then its record; a policy that merely copies its labels is left alone
// and does not block removal.
func Test_unit_kube_run_network_teardown(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, stageBody())
	spec := e.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})
	o := e.ensure(spec)
	ours := policyNames(e, kube.LabelResourceKind, "network")
	if len(ours) != 4 {
		t.Fatalf("policies %v", ours)
	}
	// Lookalikes: our labels, our name prefix, but not our anchor / not our annotations.
	pub := policyOf(t, e, spec.Name+"-pub")
	lookalike := pub
	lookalike.Metadata.Name, lookalike.Metadata.UID, lookalike.Raw = spec.Name+"-zzz", "", nil
	lookalike.Metadata.Annotations = map[string]string{kube.AnnTenant: string(spec.Scope.TenantID)}
	stale := pub
	stale.Metadata.Name, stale.Metadata.UID, stale.Raw = spec.Name+"-old", "", nil
	stale.Metadata.Annotations = map[string]string{}
	for k, v := range pub.Metadata.Annotations {
		stale.Metadata.Annotations[k] = v
	}
	stale.Metadata.Annotations[kube.AnnResourceUID] = "an-earlier-instance"
	for _, p := range []kubernetes.NetworkPolicy{lookalike, stale} {
		if _, err := e.fake.CreateNetworkPolicy(ctx, e.ns, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.rr.Remove(ctx, o.Handle); !errors.Is(err, execution.ErrResourceFenced) {
		t.Fatalf("remove before stop: %v", err)
	}
	if ev := e.stop(o.Handle); !ev.Stopped {
		t.Fatalf("stop %+v", ev)
	}
	if got := policyNames(e, kube.LabelResourceKind, "network"); len(got) != 6 {
		t.Fatalf("a stop deleted policies before removal: %v", got)
	}
	requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
	if got := e.remove(o.Handle); !got.Removed {
		t.Fatalf("remove %+v", got)
	}
	left := policyNames(e, kube.LabelResourceKind, "network")
	if len(left) != 2 || left[0] != spec.Name+"-old" || left[1] != spec.Name+"-zzz" {
		t.Fatalf("lookalikes must stay and ours must go: %v", left)
	}
	if len(liveSecrets(e)) != 0 {
		t.Fatal("the record outlived the network")
	}
}

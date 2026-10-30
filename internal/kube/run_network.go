package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// The run's network boundary (seam S8). The tenant namespace keeps its chart
// rendered default deny, DNS and Kodama-callback rules; this resource adds only
// per-run, label-selected NetworkPolicies, all of them allow rules (policies are
// additive, so no rule here can widen what the chart denies for other Pods):
//
//   - clients (-cli): the run's step Pods may reach service Pods: those of the
//     tenant under the tenant model, those of the run under the run model. The
//     service's own ingress policy (rendered with the service, where its ports
//     are known) restricts the ports and the peers;
//   - service DNS (-svc): the run's service Pods may resolve names through the
//     cluster resolver and, under the tenant model, reach the tenant's services;
//   - edges (-eo, -ei; run model only): service-to-service access along a
//     declared edge, by port name, in both directions;
//   - public egress (-pub; publicEgress only): the run's step Pods may reach TCP
//     80/443 of public addresses (private, link-local, CGNAT, loopback and the
//     configured cluster ranges excluded);
//   - named profiles (-egN): the resolved destinations of each profile a workload
//     names, verified against the digest the run pinned.
//
// Anything not allowed here stays denied by the namespace's default deny: other
// tenants, the control plane, the Kubernetes API, node and metadata addresses
// and every destination outside the profiles.

// requiredPublicExcept are the ranges public egress never reaches.
var requiredPublicExcept = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "127.0.0.0/8"}

// minProfilePrefixBits bounds how broad a named profile destination may be: a
// block wider than /8 is a general allow, not a destination.
const minProfilePrefixBits = 8

func tcpPort(p any) kubernetes.NetworkPort {
	return kubernetes.NetworkPort{Protocol: portProtocolTCP, Port: p}
}

func podPeer(labels map[string]string) kubernetes.NetworkPeer {
	return kubernetes.NetworkPeer{PodSelector: &kubernetes.LabelSelector{MatchLabels: labels}}
}

// dnsRule is the cluster resolver rule of the chart's baseline (exactly what
// the namespace preflight recognizes).
func dnsRule() kubernetes.NetworkEgress {
	return kubernetes.NetworkEgress{
		To: []kubernetes.NetworkPeer{{
			NamespaceSelector: &kubernetes.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			PodSelector:       &kubernetes.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}},
		Ports: []kubernetes.NetworkPort{{Protocol: "UDP", Port: float64(53)}, {Protocol: "TCP", Port: float64(53)}},
	}
}

func (x runRender) netPolicy(suffix string, sel map[string]string, types []string, anchor string) kubernetes.NetworkPolicy {
	var p kubernetes.NetworkPolicy
	p.APIVersion, p.Kind = "networking.k8s.io/v1", "NetworkPolicy"
	p.Metadata = x.meta(x.spec.Name+suffix, nil)
	p.Metadata.Annotations[AnnResourceUID] = anchor
	p.Spec.PodSelector.MatchLabels = sel
	p.Spec.PolicyTypes = types
	return p
}

// networkPolicies renders every policy of the run's network resource. anchor is
// the record Secret's UID (the resource's instance identity). resolve maps a
// profile destination host to address blocks.
func (x runRender) networkPolicies(anchor string, resolve func(host string) ([]netip.Prefix, error)) ([]kubernetes.NetworkPolicy, error) {
	n := x.spec.Network
	if n == nil || (n.Isolation != execution.IsolationTenant && n.Isolation != execution.IsolationRun) {
		return nil, wrap(ErrPolicy, "the network model is unknown")
	}
	if n.Isolation != x.body.Network.Isolation || n.PublicEgress != x.body.Network.PublicEgress {
		return nil, wrap(ErrPolicy, "the run's network model is not the pinned policy's")
	}
	key := x.runKey()
	step := map[string]string{LabelRunKey: key, LabelRole: RoleLabelStep}
	svc := map[string]string{LabelRunKey: key, LabelRole: RoleLabelService}
	servicePeer := podPeer(map[string]string{LabelRole: RoleLabelService, LabelRunShared: "true"})
	if n.Isolation == execution.IsolationRun {
		servicePeer = podPeer(svc)
	}
	var out []kubernetes.NetworkPolicy

	cli := x.netPolicy(suffixClients, step, []string{"Egress"}, anchor)
	cli.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{servicePeer}}}
	out = append(out, cli)

	sd := x.netPolicy(suffixServiceDNS, svc, []string{"Egress"}, anchor)
	sd.Spec.Egress = []kubernetes.NetworkEgress{dnsRule()}
	if n.Isolation == execution.IsolationTenant {
		sd.Spec.Egress = append(sd.Spec.Egress, kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{servicePeer}})
	}
	out = append(out, sd)

	if n.Isolation == execution.IsolationRun {
		edges, err := x.edgePolicies(anchor)
		if err != nil {
			return nil, err
		}
		out = append(out, edges...)
	}

	if n.PublicEgress {
		pub := x.netPolicy(suffixPublic, step, []string{"Egress"}, anchor)
		except := append([]string(nil), requiredPublicExcept...)
		except = append(except, x.publicExcept...)
		pub.Spec.Egress = []kubernetes.NetworkEgress{{
			To:    []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: "0.0.0.0/0", Except: except}}},
			Ports: []kubernetes.NetworkPort{tcpPort(float64(80)), tcpPort(float64(443))},
		}}
		out = append(out, pub)
	}

	profiles, err := x.profilePolicies(anchor, key, resolve)
	if err != nil {
		return nil, err
	}
	return append(out, profiles...), nil
}

// edgePolicies renders the declared service-to-service edges of the run model:
// an egress rule on the calling service and an ingress rule on the called one,
// both by port name.
func (x runRender) edgePolicies(anchor string) ([]kubernetes.NetworkPolicy, error) {
	n := x.spec.Network
	known := map[string]bool{}
	for _, s := range n.Services {
		known[s] = true
	}
	edges := append([]execution.EdgeSpec(nil), n.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	key := x.runKey()
	sel := func(service string) map[string]string {
		return map[string]string{LabelRunKey: key, LabelRole: RoleLabelService, LabelWorkload: WorkloadHash(x.spec.Scope, x.spec.RunID, execution.ServiceKey(service))}
	}
	var out []kubernetes.NetworkPolicy
	for i, e := range edges {
		if !known[e.From] || !known[e.To] || e.From == e.To || len(e.Ports) == 0 {
			return nil, wrap(ErrPolicy, "a declared edge is not between two declared services")
		}
		ports := make([]kubernetes.NetworkPort, 0, len(e.Ports))
		names := append([]string(nil), e.Ports...)
		sort.Strings(names)
		for _, p := range names {
			if !validPortName(p) {
				return nil, wrap(ErrPolicy, "an edge port is not a valid port name")
			}
			ports = append(ports, tcpPort(p))
		}
		eo := x.netPolicy(fmt.Sprintf("%s%d", suffixEdgeOut, i), sel(e.From), []string{"Egress"}, anchor)
		eo.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{podPeer(sel(e.To))}, Ports: ports}}
		ei := x.netPolicy(fmt.Sprintf("%s%d", suffixEdgeIn, i), sel(e.To), []string{"Ingress"}, anchor)
		portDocs := make([]map[string]any, 0, len(ports))
		for _, p := range ports {
			portDocs = append(portDocs, map[string]any{"protocol": portProtocolTCP, "port": p.Port})
		}
		ei.Spec.Ingress = []map[string]any{{"from": []map[string]any{{"podSelector": map[string]any{"matchLabels": sel(e.From)}}}, "ports": portDocs}}
		out = append(out, eo, ei)
	}
	return out, nil
}

// profilePolicies renders one egress policy per workload that names profiles.
func (x runRender) profilePolicies(anchor, key string, resolve func(string) ([]netip.Prefix, error)) ([]kubernetes.NetworkPolicy, error) {
	n := x.spec.Network
	workloads := make([]string, 0, len(n.Workloads))
	for w, names := range n.Workloads {
		if len(names) > 0 {
			workloads = append(workloads, w)
		}
	}
	sort.Strings(workloads)
	var out []kubernetes.NetworkPolicy
	for i, w := range workloads {
		names := append([]string(nil), n.Workloads[w]...)
		sort.Strings(names)
		var rules []kubernetes.NetworkEgress
		for j, name := range names {
			if j > 0 && names[j-1] == name {
				continue
			}
			pinned, ok := n.Egress[name]
			digest, have := x.body.EgressDigest(name)
			if !ok || !have || digest != pinned {
				return nil, wrap(ErrPolicy, "an egress profile's destinations differ from the digest the run pinned")
			}
			r, err := x.profileRules(x.body.EgressProfiles[name], resolve)
			if err != nil {
				return nil, err
			}
			rules = append(rules, r...)
		}
		p := x.netPolicy(fmt.Sprintf("%s%d", suffixEgress, i), map[string]string{LabelRunKey: key, LabelWorkload: WorkloadHash(x.spec.Scope, x.spec.RunID, w)}, []string{"Egress"}, anchor)
		p.Spec.Egress = rules
		out = append(out, p)
	}
	return out, nil
}

func (x runRender) profileRules(p execution.EgressProfile, resolve func(string) ([]netip.Prefix, error)) ([]kubernetes.NetworkEgress, error) {
	if len(p.Destinations) == 0 {
		return nil, wrap(ErrPolicy, "an egress profile has no destination")
	}
	var rules []kubernetes.NetworkEgress
	for _, d := range p.Destinations {
		if len(d.Ports) == 0 {
			return nil, wrap(ErrPolicy, "an egress destination has no ports")
		}
		ports := make([]kubernetes.NetworkPort, 0, len(d.Ports))
		sorted := append([]int64(nil), d.Ports...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		for _, pt := range sorted {
			if pt < 1 || pt > 65535 {
				return nil, wrap(ErrPolicy, "an egress port is out of range")
			}
			ports = append(ports, tcpPort(float64(pt)))
		}
		prefixes, err := destinationPrefixes(d.Host, resolve)
		if err != nil {
			return nil, err
		}
		peers := make([]kubernetes.NetworkPeer, 0, len(prefixes))
		for _, pf := range prefixes {
			if err := checkProfilePrefix(pf); err != nil {
				return nil, err
			}
			peers = append(peers, kubernetes.NetworkPeer{IPBlock: &kubernetes.IPBlock{CIDR: pf.String()}})
		}
		rules = append(rules, kubernetes.NetworkEgress{To: peers, Ports: ports})
	}
	return rules, nil
}

// destinationPrefixes turns a destination host into address blocks: a literal
// address or block directly, a hostname through the installation's resolver. A
// hostname with no resolver is refused: it is never allowed by default.
func destinationPrefixes(host string, resolve func(string) ([]netip.Prefix, error)) ([]netip.Prefix, error) {
	if pf, err := netip.ParsePrefix(host); err == nil {
		return []netip.Prefix{pf.Masked()}, nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Prefix{netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())}, nil
	}
	if resolve == nil {
		return nil, wrap(ErrPolicy, "an egress destination names a host and no resolver is configured")
	}
	return resolve(host)
}

// checkProfilePrefix refuses a destination that is a general allow: a block
// wider than /8, the default route, or anything touching link-local and metadata
// addresses.
func checkProfilePrefix(p netip.Prefix) error {
	if !p.IsValid() || p.Bits() < minProfilePrefixBits && p.Addr().Is4() || p.Bits() < 16 && p.Addr().Is6() {
		return wrap(ErrPolicy, "an egress destination is too broad")
	}
	link := netip.MustParsePrefix("169.254.0.0/16")
	link6 := netip.MustParsePrefix("fe80::/10")
	if p.Overlaps(link) || p.Overlaps(link6) {
		return wrap(ErrPolicy, "an egress destination reaches link-local or metadata addresses")
	}
	return nil
}

// KeyResolved is the record Secret key that pins the addresses a network
// resource resolved its hostname destinations to, once, when the resource was
// planned. Rendering and every later verification use the pinned answer, so
// DNS drift can neither change the policies nor make them look foreign.
const KeyResolved = "resolved"

// profileHosts lists, sorted and unique, the destination hosts of the profiles
// the run's workloads name that are not literal addresses or blocks.
func profileHosts(n *execution.NetworkSpec, body execution.ExecutionPolicy) []string {
	seen := map[string]bool{}
	var out []string
	for _, names := range n.Workloads {
		for _, name := range names {
			for _, d := range body.EgressProfiles[name].Destinations {
				if _, err := netip.ParsePrefix(d.Host); err == nil {
					continue
				}
				if _, err := netip.ParseAddr(d.Host); err == nil {
					continue
				}
				if !seen[d.Host] {
					seen[d.Host] = true
					out = append(out, d.Host)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// resolveHosts resolves each host once through the installation's resolver and
// returns the sorted address blocks (host addresses) per host.
func resolveHosts(ctx context.Context, r HostResolver, hosts []string) (map[string][]string, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	if r == nil {
		return nil, wrap(ErrPolicy, "an egress destination names a host and no resolver is configured")
	}
	out := map[string][]string{}
	for _, h := range hosts {
		addrs, err := r.ResolveHost(ctx, h)
		if err != nil {
			return nil, wrapCause(ErrUnavailable, err, "resolving an egress destination")
		}
		if len(addrs) == 0 {
			return nil, wrap(ErrPolicy, "an egress destination resolved to no address")
		}
		set := map[string]bool{}
		for _, a := range addrs {
			a = a.Unmap()
			set[netip.PrefixFrom(a, a.BitLen()).String()] = true
		}
		for p := range set {
			out[h] = append(out[h], p)
		}
		sort.Strings(out[h])
	}
	return out, nil
}

// pinnedResolver answers hostname destinations from the pinned answers only.
func pinnedResolver(pinned map[string][]string) func(string) ([]netip.Prefix, error) {
	return func(host string) ([]netip.Prefix, error) {
		list, ok := pinned[host]
		if !ok || len(list) == 0 {
			return nil, wrap(ErrPolicy, "an egress destination host has no pinned address")
		}
		out := make([]netip.Prefix, 0, len(list))
		for _, s := range list {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, wrap(ErrPolicy, "a pinned egress address is malformed")
			}
			out = append(out, p)
		}
		return out, nil
	}
}

func marshalResolved(m map[string][]string) ([]byte, error) { return json.Marshal(m) }

// storedResolver reads the pinned answers out of a record Secret.
func storedResolver(data map[string][]byte) func(string) ([]netip.Prefix, error) {
	var pinned map[string][]string
	if raw := data[KeyResolved]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &pinned)
	}
	return pinnedResolver(pinned)
}

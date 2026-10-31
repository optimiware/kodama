package kernelrt

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// admit decides, before any effect, whether this cluster can satisfy a request
// for scope: the required capabilities are declared, the scope has a target,
// the namespace is live and holds the service account, and, when network
// isolation is required, the namespace has a default-deny ingress policy. Every
// refusal wraps runtime.ErrNotReached and runtime.ErrCapability: nothing was
// created, and retrying without a cluster change cannot help.
func (c *core) admit(ctx context.Context, scope domain.Scope, required runtime.CapabilitySet) (Target, error) {
	if err := c.capabilities().Satisfies(required); err != nil {
		return Target{}, notReached(err)
	}
	t, err := c.targets.Target(ctx, scope)
	if err != nil {
		return Target{}, notReached(fmt.Errorf("%w: no target for the scope: %w", runtime.ErrCapability, err))
	}
	if !nameRE.MatchString(t.Namespace) || !nameRE.MatchString(t.ServiceAccount) {
		return Target{}, notReached(fmt.Errorf("%w: the scope's target is malformed", runtime.ErrCapability))
	}
	ns, err := c.cluster.GetNamespace(ctx, t.Namespace)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return Target{}, unsupported("namespace %s does not exist", t.Namespace)
	case err != nil:
		return Target{}, notReached(fmt.Errorf("reading the namespace: %w", err))
	case ns.Status.Phase == "Terminating" || ns.Metadata.DeletionTimestamp != "":
		return Target{}, unsupported("namespace %s is terminating", t.Namespace)
	}
	if _, err := c.cluster.GetServiceAccount(ctx, t.Namespace, t.ServiceAccount); err != nil {
		if errors.Is(err, kubernetes.ErrNotFound) {
			return Target{}, unsupported("service account %s does not exist", t.ServiceAccount)
		}
		return Target{}, notReached(fmt.Errorf("reading the service account: %w", err))
	}
	if required.Has(runtime.CapNetworkIsolation) {
		if err := c.requireDefaultDeny(ctx, t.Namespace); err != nil {
			return Target{}, err
		}
	}
	return t, nil
}

// requireDefaultDeny verifies the namespace's effective network isolation.
// NetworkPolicies are additive, so a default-deny policy alone proves nothing:
// any other policy that selects the adapter's Pods can allow traffic back in.
// It therefore requires both:
//
//   - some policy selects every Pod, governs ingress and egress and allows no
//     ingress; and
//   - every policy that selects the adapter's Pods (an empty selector, a
//     selector of labels the adapter sets, or any expression selector) allows
//     only these shapes: ingress from Pods of the same namespace; egress to
//     Pods of the same namespace; egress to anywhere on DNS port 53 only; and
//     public egress to 0.0.0.0/0 on ports 80 and 443 only with the private,
//     link-local and CGNAT ranges excepted.
//
// An allow-all rule, a namespace selector without the DNS port, or an
// unexcepted public block overlaps another tenant's traffic and is refused.
// This judges the policy objects; whether the network plugin enforces them is
// the operator's attestation (Config.NetworkPolicyEnforced).
func (c *core) requireDefaultDeny(ctx context.Context, ns string) error {
	policies, err := c.cluster.ListNetworkPolicies(ctx, ns)
	if err != nil {
		return notReached(fmt.Errorf("listing network policies: %w", err))
	}
	denied := false
	for _, p := range policies {
		s := p.Spec
		if s.PodSelector.Empty() && slices.Contains(s.PolicyTypes, "Ingress") && slices.Contains(s.PolicyTypes, "Egress") && len(s.Ingress) == 0 {
			denied = true
		}
		if !selectsAdapterPods(s.PodSelector) {
			continue
		}
		if err := allowedShapes(s); err != nil {
			return unsupported("network policy %s allows more than tenant-scoped traffic: %v", p.Metadata.Name, err)
		}
	}
	if !denied {
		return unsupported("namespace %s has no default-deny network policy", ns)
	}
	return nil
}

// selectsAdapterPods reports whether a policy's selector may select the
// adapter's Pods. The Pods carry only the adapter's own labels, so a selector
// that requires any other label, or another value of the managed-by label,
// cannot match them. An expression selector cannot be evaluated here and is
// assumed to match.
func selectsAdapterPods(sel kubernetes.LabelSelector) bool {
	if len(sel.MatchExpressions) > 0 {
		return true
	}
	for k, v := range sel.MatchLabels {
		if !isAdapterLabel(k) || (k == labelManaged && v != valueManaged) {
			return false
		}
	}
	return true
}

func isAdapterLabel(k string) bool {
	switch k {
	case labelManaged, labelScope, labelRun, labelKind, labelRole, labelObject:
		return true
	}
	return false
}

// protectedRanges are the addresses an egress block may never allow: other
// tenants and the cluster itself live there. The list is the private, shared,
// loopback, link-local, multicast and reserved IPv4 space and the IPv6 unique
// local, link-local, loopback, multicast, IPv4-mapped and NAT64 space (the last
// two reach IPv4 private addresses through IPv6 spellings).
var protectedRanges = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/3",
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "100::/64", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// covered reports whether every address of p lies inside one of the excepts.
func covered(p netip.Prefix, excepts []netip.Prefix) bool {
	overlapping := false
	for _, e := range excepts {
		if e.Bits() <= p.Bits() && e.Contains(p.Addr()) {
			return true
		}
		if p.Overlaps(e) {
			overlapping = true
		}
	}
	if !overlapping || p.Bits() == p.Addr().BitLen() {
		return false
	}
	// Split into halves: the block is covered if both halves are.
	lo := netip.PrefixFrom(p.Addr(), p.Bits()+1)
	hi := netip.PrefixFrom(flipBit(p.Addr(), p.Bits()), p.Bits()+1)
	return covered(lo.Masked(), excepts) && covered(hi.Masked(), excepts)
}

func flipBit(a netip.Addr, bit int) netip.Addr {
	b := a.AsSlice()
	b[bit/8] ^= 0x80 >> (bit % 8)
	out, _ := netip.AddrFromSlice(b)
	return out
}

// allowedBlock judges the addresses an ipBlock actually allows: the block minus
// its exceptions must contain no protected address, and the rule must name
// explicit single ports. How the block is spelled does not matter: two halves
// of the default route are judged as the address space they are.
func allowedBlock(b *kubernetes.IPBlock, ports map[int]bool) error {
	block, err := netip.ParsePrefix(b.CIDR)
	if err != nil {
		return fmt.Errorf("address block %q is not a CIDR", b.CIDR)
	}
	block = block.Masked()
	var excepts []netip.Prefix
	for _, e := range b.Except {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return fmt.Errorf("exception %q is not a CIDR", e)
		}
		excepts = append(excepts, p.Masked())
	}
	for _, prot := range protectedRanges {
		if !block.Overlaps(prot) {
			continue
		}
		// The overlap is the smaller of the two when nested.
		inter := prot
		if block.Bits() > prot.Bits() {
			inter = block
		}
		if !covered(inter, excepts) {
			return fmt.Errorf("address block %s allows protected addresses in %s", b.CIDR, inter)
		}
	}
	if len(ports) == 0 || ports[0] {
		return fmt.Errorf("address block %s must name explicit single ports", b.CIDR)
	}
	return nil
}

func allowedShapes(s kubernetes.NetworkPolicySpec) error {
	for i, rule := range s.Ingress {
		peers, hasFrom := rule["from"].([]any)
		if !hasFrom || len(peers) == 0 {
			return fmt.Errorf("ingress rule %d admits every source", i)
		}
		for _, raw := range peers {
			peer, _ := raw.(map[string]any)
			if peer["namespaceSelector"] != nil || peer["ipBlock"] != nil || peer["podSelector"] == nil {
				return fmt.Errorf("ingress rule %d admits sources outside the namespace", i)
			}
		}
	}
	for i, rule := range s.Egress {
		ports := portSet(rule.Ports)
		switch {
		case len(rule.To) == 0:
			if !onlyPorts(ports, 53) {
				return fmt.Errorf("egress rule %d admits every destination", i)
			}
		default:
			for _, peer := range rule.To {
				if err := egressPeer(peer, ports); err != nil {
					return fmt.Errorf("egress rule %d: %w", i, err)
				}
			}
		}
	}
	return nil
}

// portSet returns the single ports of a rule; a range or a named port yields a
// zero entry, which no port allowance accepts.
func portSet(ps []kubernetes.NetworkPort) map[int]bool {
	if len(ps) == 0 {
		return nil
	}
	out := map[int]bool{}
	for _, p := range ps {
		n := 0
		switch v := p.Port.(type) {
		case float64:
			n = int(v)
		case int:
			n = v
		case int32:
			n = int(v)
		}
		if p.EndPort != nil {
			n = 0
		}
		out[n] = true
	}
	return out
}

func onlyPorts(set map[int]bool, allowed ...int) bool {
	if len(set) == 0 {
		return false
	}
	for p := range set {
		if !slices.Contains(allowed, p) {
			return false
		}
	}
	return true
}

func egressPeer(p kubernetes.NetworkPeer, ports map[int]bool) error {
	switch {
	case p.IPBlock != nil:
		return allowedBlock(p.IPBlock, ports)
	case p.NamespaceSelector != nil:
		if onlyPorts(ports, 53) {
			return nil // cluster DNS in another namespace
		}
		return errors.New("a namespace selector reaches other namespaces")
	case p.PodSelector != nil:
		return nil // Pods of this namespace
	}
	return errors.New("an empty peer")
}

// checkStorage verifies the workspace storage class exists and is usable for a
// run claim, and reports whether it binds only on first use.
func (c *core) storageClass(ctx context.Context) (kubernetes.StorageClass, error) {
	sc, err := c.cluster.GetStorageClass(ctx, c.cfg.StorageClass)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return sc, unsupported("storage class %s does not exist", c.cfg.StorageClass)
	case err != nil:
		return sc, notReached(fmt.Errorf("reading the storage class: %w", err))
	}
	return sc, nil
}

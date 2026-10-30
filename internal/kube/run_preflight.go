package kube

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Under the run_shared baseline the namespace holds, besides the chart's default
// deny and runtime egress, the per-run policies the run-resource adapter owns.
// Preflight accepts exactly those, by shape: a policy carrying the owner label
// must select run Pods by the run labels, admit only run-labelled Pods and the
// cluster resolver, public TCP 80/443 with the private ranges excluded, or a
// bounded named destination. An owned-looking policy of any other shape, of
// another tenant or with a broad allow is refused, and a policy without the
// owner label is judged as before: it may select run Pods only within the
// declared runtime egress.

// runPodLabels are the labels run Pods carry beyond the executor's: a foreign
// policy selecting one of them may select run Pods.
var runPodLabels = map[string]bool{
	LabelResourceOwner: true, LabelResourceKind: true, LabelRunKey: true, LabelRole: true, LabelWorkload: true, LabelRunShared: true,
}

// selectsRunWorkload is selectsWorkload with the run labels counted as labels
// run Pods carry.
func selectsRunWorkload(sel kubernetes.LabelSelector) (selects bool, unsupported bool) {
	for k, v := range sel.MatchLabels {
		switch {
		case k == LabelManagedBy && v == ValueManagedBy:
		case dynamicWorkloadLabels[k], runPodLabels[k]:
		default:
			return false, false
		}
	}
	if len(sel.MatchExpressions) > 0 {
		return true, true
	}
	return true, false
}

// checkRunNetwork is checkNetwork for the run_shared baseline.
func checkRunNetwork(ctx context.Context, pols []kubernetes.NetworkPolicy, allowed []EgressRule, tenant domain.TenantID, verify OwnedPolicyVerifier) error {
	denyAll := false
	for _, pol := range pols {
		if pol.Spec.PodSelector.Empty() && hasType(pol, "Ingress") && hasType(pol, "Egress") &&
			len(pol.Spec.Ingress) == 0 && len(pol.Spec.Egress) == 0 {
			denyAll = true
		}
	}
	if !denyAll {
		return deny("the namespace has no default-deny network policy for ingress and egress")
	}
	for _, pol := range pols {
		if pol.Metadata.Labels[LabelResourceOwner] == ValueResourceOwner {
			if err := validateOwnedPolicy(pol, tenant); err != nil {
				return err
			}
			// Policies are additive: a policy that only has the shape of ours widens the
			// boundary as well as a foreign one. It must be exactly what the committed
			// spec of its resource renders.
			if verify == nil {
				return ownedDeny("no verifier of owned policies is configured")
			}
			if err := verify.VerifyOwnedPolicy(ctx, tenant, pol); err != nil {
				return err
			}
			continue
		}
		sel, unsupported := selectsRunWorkload(pol.Spec.PodSelector)
		if !sel {
			continue
		}
		if unsupported {
			return deny("a network policy uses a selector expression that cannot be evaluated")
		}
		if len(pol.Spec.Ingress) > 0 {
			return deny("a network policy admits ingress to workload Pods")
		}
		for _, rule := range pol.Spec.Egress {
			if clusterDNS(rule) {
				continue
			}
			if err := egressCovered(rule, allowed); err != nil {
				return err
			}
		}
	}
	return nil
}

func ownedDeny(why string) error {
	return deny("an owned run network policy is not an accepted shape: %s", why)
}

// validateOwnedPolicy accepts an owned run policy of exactly the shapes the
// adapter renders.
func validateOwnedPolicy(pol kubernetes.NetworkPolicy, tenant domain.TenantID) error {
	if pol.Metadata.Annotations[AnnTenant] != string(tenant) {
		return ownedDeny("another tenant's policy")
	}
	sel := pol.Spec.PodSelector
	if len(sel.MatchExpressions) > 0 || sel.MatchLabels[LabelRunKey] == "" {
		return ownedDeny("it does not select by run")
	}
	if !labelsWithin(sel.MatchLabels, LabelRunKey, LabelRole, LabelWorkload) || !roleOK(sel.MatchLabels[LabelRole]) {
		return ownedDeny("its selector uses a label outside the run labels")
	}
	if len(pol.Spec.PolicyTypes) == 0 {
		return ownedDeny("it has no policy types")
	}
	for _, t := range pol.Spec.PolicyTypes {
		if t != "Ingress" && t != "Egress" {
			return ownedDeny("unknown policy type")
		}
	}
	if (len(pol.Spec.Ingress) > 0 && !hasType(pol, "Ingress")) || (len(pol.Spec.Egress) > 0 && !hasType(pol, "Egress")) {
		return ownedDeny("rules without their policy type")
	}
	for _, in := range pol.Spec.Ingress {
		if err := validateOwnedIngress(in); err != nil {
			return err
		}
	}
	for _, e := range pol.Spec.Egress {
		if err := validateOwnedEgress(e); err != nil {
			return err
		}
	}
	return nil
}

func roleOK(r string) bool { return r == "" || r == RoleLabelStep || r == RoleLabelService }

func labelsWithin(l map[string]string, keys ...string) bool {
	for k := range l {
		if !slices.Contains(keys, k) {
			return false
		}
	}
	return true
}

// ownedPeer accepts a same-namespace Pod peer selected by run labels only.
func ownedPeer(p kubernetes.NetworkPeer) bool {
	if p.PodSelector == nil || p.IPBlock != nil || p.NamespaceSelector != nil || len(p.PodSelector.MatchExpressions) > 0 || len(p.PodSelector.MatchLabels) == 0 {
		return false
	}
	return labelsWithin(p.PodSelector.MatchLabels, LabelRunKey, LabelRole, LabelWorkload, LabelRunShared) && roleOK(p.PodSelector.MatchLabels[LabelRole])
}

// ownedPort accepts a TCP port by number or a valid port name, never a range.
func ownedPort(p kubernetes.NetworkPort) bool {
	if (p.Protocol != "" && p.Protocol != "TCP") || p.EndPort != nil {
		return false
	}
	if n, ok := portNumber(p.Port); ok {
		return n >= 1 && n <= 65535
	}
	s, ok := p.Port.(string)
	return ok && validPortName(s)
}

func validateOwnedIngress(in map[string]any) error {
	raw, _ := json.Marshal(in)
	var rule struct {
		From  []kubernetes.NetworkPeer `json:"from"`
		Ports []kubernetes.NetworkPort `json:"ports"`
	}
	var strict map[string]json.RawMessage
	if json.Unmarshal(raw, &rule) != nil || json.Unmarshal(raw, &strict) != nil {
		return ownedDeny("an ingress rule is unreadable")
	}
	for k := range strict {
		if k != "from" && k != "ports" {
			return ownedDeny("an ingress rule has an unknown field")
		}
	}
	if len(rule.From) == 0 || len(rule.Ports) == 0 {
		return ownedDeny("an ingress rule admits all peers or all ports")
	}
	for _, p := range rule.From {
		if !ownedPeer(p) {
			return ownedDeny("an ingress peer is not a run-labelled Pod")
		}
	}
	for _, p := range rule.Ports {
		if !ownedPort(p) {
			return ownedDeny("an ingress port is not a TCP port")
		}
	}
	return nil
}

func validateOwnedEgress(e kubernetes.NetworkEgress) error {
	if clusterDNS(e) {
		return nil
	}
	if len(e.To) == 0 {
		return ownedDeny("an egress rule admits all destinations")
	}
	pods, blocks := 0, 0
	for _, p := range e.To {
		switch {
		case ownedPeer(p):
			pods++
		case p.IPBlock != nil && p.PodSelector == nil && p.NamespaceSelector == nil:
			blocks++
		default:
			return ownedDeny("an egress peer is neither a run-labelled Pod nor an address block")
		}
	}
	if pods > 0 && blocks > 0 {
		return ownedDeny("an egress rule mixes Pod and address peers")
	}
	if pods > 0 {
		for _, p := range e.Ports {
			if !ownedPort(p) {
				return ownedDeny("an egress port is not a TCP port")
			}
		}
		return nil
	}
	return validateOwnedBlocks(e)
}

// validateOwnedBlocks accepts the public egress rule (default route with the
// private ranges excluded, TCP 80 and 443 only) or bounded named destinations.
func validateOwnedBlocks(e kubernetes.NetworkEgress) error {
	if len(e.Ports) == 0 {
		return ownedDeny("an address rule admits all ports")
	}
	public := false
	for _, p := range e.To {
		pf, err := netip.ParsePrefix(p.IPBlock.CIDR)
		if err != nil {
			return ownedDeny("an address block is malformed")
		}
		if pf.Bits() == 0 {
			public = true
		}
	}
	if public {
		if len(e.To) != 1 || e.To[0].IPBlock.CIDR != "0.0.0.0/0" {
			return ownedDeny("the default route appears beside other destinations")
		}
		excluded := map[string]bool{}
		for _, x := range e.To[0].IPBlock.Except {
			if pf, err := netip.ParsePrefix(x); err == nil {
				excluded[pf.Masked().String()] = true
			}
		}
		for _, need := range requiredPublicExcept {
			if !excluded[need] {
				return ownedDeny("public egress does not exclude the private ranges")
			}
		}
		if len(e.Ports) != 2 {
			return ownedDeny("public egress is not TCP 80 and 443")
		}
		seen := map[int32]bool{}
		for _, p := range e.Ports {
			n, ok := portNumber(p.Port)
			if !ok || (p.Protocol != "" && p.Protocol != "TCP") || p.EndPort != nil || (n != 80 && n != 443) {
				return ownedDeny("public egress is not TCP 80 and 443")
			}
			seen[n] = true
		}
		if !seen[80] || !seen[443] {
			return ownedDeny("public egress is not TCP 80 and 443")
		}
		return nil
	}
	for _, p := range e.To {
		pf, _ := netip.ParsePrefix(p.IPBlock.CIDR)
		if len(p.IPBlock.Except) > 0 || checkProfilePrefix(pf) != nil {
			return ownedDeny("a named destination is too broad")
		}
	}
	for _, p := range e.Ports {
		n, ok := portNumber(p.Port)
		if !ok || n < 1 || (p.Protocol != "" && p.Protocol != "TCP") || p.EndPort != nil {
			return ownedDeny("a named destination port is not a TCP port")
		}
	}
	return nil
}

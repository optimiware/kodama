package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// AnnNamespaceTenant marks which tenant a namespace belongs to. The policy maps
// tenants to namespaces; the namespace must agree, so a misconfigured mapping
// cannot place one tenant's workloads in another's namespace.
const AnnNamespaceTenant = AnnTenant

const (
	labelPSAEnforce = "pod-security.kubernetes.io/enforce"
	nsActive        = "Active"
)

// Preflight is the read-only check of a tenant namespace against the execution
// baseline: Pod Security restricted, the expected per-tenant service account
// with no effective permissions, a ResourceQuota that bounds compute, and
// default-deny NetworkPolicies with only the declared runtime egress. It checks
// policy content and effective permissions, not merely names, and denies what it
// cannot read. Provisioning is a deployment concern; this module never creates
// or edits these objects. Enforcement by the network plugin cannot be proven
// from the API: the acceptance fixtures probe it.
type Preflight struct {
	cluster Cluster
	policy  ExecutionPolicy
	ttl     time.Duration
	now     func() time.Time

	// ownedPolicy verifies one owned-looking run network policy against its
	// authoritative record and exact rendering (the run-resource adapter). With none
	// configured an owned-looking policy cannot be verified and is refused.
	ownedPolicy OwnedPolicyVerifier

	mu sync.Mutex
	ok map[preflightKey]time.Time
}

// OwnedPolicyVerifier verifies that a NetworkPolicy carrying the run-resource
// owner label is exactly a policy the committed spec of its resource renders.
// (*RunResources).VerifyOwnedPolicy implements it.
type OwnedPolicyVerifier interface {
	VerifyOwnedPolicy(ctx context.Context, tenant domain.TenantID, pol kubernetes.NetworkPolicy) error
}

// WithOwnedPolicies sets the verifier of owned run policies and returns p.
func (p *Preflight) WithOwnedPolicies(v OwnedPolicyVerifier) *Preflight {
	p.ownedPolicy = v
	return p
}

// preflightKey caches a success per tenant and baseline: the attempt_private
// baseline (restricted Pod Security) and the pinned run_shared one differ.
type preflightKey struct {
	tenant    domain.TenantID
	level     string
	ver       string
	runShared bool
}

// baseline is the Pod Security level (and optionally version) a check requires.
// A run_shared check also admits the owned per-run network policies and requires
// that the tenant account cannot create or delete the run-resource kinds.
type baseline struct {
	level, version string
	runShared      bool
}

// NewPreflight builds a checker. A success is cached for ttl.
func NewPreflight(c Cluster, p ExecutionPolicy, ttl time.Duration, now func() time.Time) *Preflight {
	if now == nil {
		now = time.Now
	}
	return &Preflight{cluster: c, policy: p, ttl: ttl, now: now, ok: map[preflightKey]time.Time{}}
}

// forbiddenAccess are the (verb, resource) pairs a tenant service account must
// not hold: reading or changing Secrets, Pods, Jobs, quotas and policies,
// executing into Pods, minting tokens and listing the cluster.
var forbiddenAccess = []kubernetes.ResourceAttributes{
	{Verb: "get", Resource: "secrets"}, {Verb: "list", Resource: "secrets"}, {Verb: "watch", Resource: "secrets"},
	{Verb: "get", Resource: "pods"}, {Verb: "list", Resource: "pods"}, {Verb: "create", Resource: "pods"}, {Verb: "delete", Resource: "pods"},
	{Verb: "create", Resource: "pods", Subresource: "exec"}, {Verb: "create", Resource: "pods", Subresource: "attach"},
	{Verb: "create", Group: "batch", Resource: "jobs"}, {Verb: "delete", Group: "batch", Resource: "jobs"}, {Verb: "get", Group: "batch", Resource: "jobs"},
	{Verb: "get", Resource: "configmaps"}, {Verb: "list", Resource: "configmaps"},
	{Verb: "create", Resource: "serviceaccounts", Subresource: "token"},
	{Verb: "update", Resource: "resourcequotas"}, {Verb: "update", Group: "networking.k8s.io", Resource: "networkpolicies"},
	{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "rolebindings"},
}

// runForbiddenAccess are the further pairs a tenant account must not hold under
// the run_shared baseline: it must never create, change or delete the claims,
// Services and network policies the run-resource adapter owns.
var runForbiddenAccess = []kubernetes.ResourceAttributes{
	{Verb: "create", Resource: "persistentvolumeclaims"}, {Verb: "delete", Resource: "persistentvolumeclaims"},
	{Verb: "create", Resource: "services"}, {Verb: "delete", Resource: "services"},
	{Verb: "create", Group: "networking.k8s.io", Resource: "networkpolicies"}, {Verb: "delete", Group: "networking.k8s.io", Resource: "networkpolicies"},
}

var clusterForbidden = []kubernetes.ResourceAttributes{
	{Verb: "list", Resource: "namespaces"}, {Verb: "list", Resource: "nodes"}, {Verb: "list", Resource: "secrets"},
}

// Check validates the tenant's namespace against the attempt_private baseline;
// nil means the baseline holds now.
func (p *Preflight) Check(ctx context.Context, tenant domain.TenantID) error {
	return p.run(ctx, tenant, baseline{level: "restricted"})
}

// CheckRun validates the tenant's namespace against the pinned run_shared
// baseline: the policy's Pod Security level (and version, when pinned), the
// same service account, permission, quota and default-deny requirements, and the
// owned per-run network policies accepted by exact shape. A foreign policy that
// selects run Pods is judged exactly as for attempt_private runs.
func (p *Preflight) CheckRun(ctx context.Context, tenant domain.TenantID, ps execution.PolicyPodSecurity) error {
	if ps.Level != "baseline" && ps.Level != "restricted" {
		return deny("the pinned policy names an unknown Pod Security level")
	}
	return p.run(ctx, tenant, baseline{level: ps.Level, version: ps.Version, runShared: true})
}

func (p *Preflight) run(ctx context.Context, tenant domain.TenantID, b baseline) error {
	key := preflightKey{tenant: tenant, level: b.level, ver: b.version, runShared: b.runShared}
	p.mu.Lock()
	if t, ok := p.ok[key]; ok && p.ttl > 0 && p.now().Sub(t) < p.ttl {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	if err := p.check(ctx, tenant, b); err != nil {
		p.mu.Lock()
		delete(p.ok, key)
		p.mu.Unlock()
		return err
	}
	p.mu.Lock()
	p.ok[key] = p.now()
	p.mu.Unlock()
	return nil
}

func deny(format string, args ...any) error { return wrap(ErrPreflight, format, args...) }

func unreadable(what string, err error) error {
	if errors.Is(err, kubernetes.ErrForbidden) || errors.Is(err, kubernetes.ErrNotFound) {
		return wrapCause(ErrPreflight, err, "%s is not readable", what)
	}
	return wrapCause(ErrUnavailable, err, "%s could not be read", what)
}

func (p *Preflight) check(ctx context.Context, tenant domain.TenantID, b baseline) error {
	tgt, err := p.policy.Tenant(tenant)
	if err != nil {
		return err
	}
	ns, err := p.cluster.GetNamespace(ctx, tgt.Namespace)
	if err != nil {
		return unreadable("the namespace", err)
	}
	if ns.Status.Phase != nsActive || ns.Metadata.DeletionTimestamp != "" {
		return deny("the namespace is not active")
	}
	if ns.Metadata.Labels[labelPSAEnforce] != b.level {
		return deny("the namespace does not enforce Pod Security %s", b.level)
	}
	if b.version != "" && ns.Metadata.Labels[labelPSAEnforce+"-version"] != b.version {
		return deny("the namespace does not pin the Pod Security version")
	}
	if ns.Metadata.Annotations[AnnNamespaceTenant] != string(tenant) {
		return deny("the namespace is not bound to the tenant")
	}
	sa, err := p.cluster.GetServiceAccount(ctx, tgt.Namespace, tgt.ServiceAccount)
	if err != nil {
		return unreadable("the service account", err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		return deny("the service account automounts its token")
	}
	user := "system:serviceaccount:" + tgt.Namespace + ":" + tgt.ServiceAccount
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + tgt.Namespace, "system:authenticated"}
	forbidden := forbiddenAccess
	if b.runShared {
		forbidden = append(append([]kubernetes.ResourceAttributes(nil), forbiddenAccess...), runForbiddenAccess...)
	}
	for _, a := range forbidden {
		a.Namespace = tgt.Namespace
		allowed, err := p.cluster.Allowed(ctx, user, groups, a)
		if err != nil {
			return unreadable("effective permissions", err)
		}
		if allowed {
			return deny("the service account holds a permission it must not have (%s %s)", a.Verb, a.Resource)
		}
	}
	for _, a := range clusterForbidden {
		allowed, err := p.cluster.Allowed(ctx, user, groups, a)
		if err != nil {
			return unreadable("effective permissions", err)
		}
		if allowed {
			return deny("the service account holds a cluster permission it must not have (%s %s)", a.Verb, a.Resource)
		}
	}
	quotas, err := p.cluster.ListResourceQuotas(ctx, tgt.Namespace)
	if err != nil {
		return unreadable("the resource quotas", err)
	}
	if err := checkQuota(quotas); err != nil {
		return err
	}
	pols, err := p.cluster.ListNetworkPolicies(ctx, tgt.Namespace)
	if err != nil {
		return unreadable("the network policies", err)
	}
	if b.runShared {
		return checkRunNetwork(ctx, pols, p.policy.RuntimeEgress(), tenant, p.ownedPolicy)
	}
	return checkNetwork(pols, p.policy.RuntimeEgress())
}

// checkQuota requires positive hard bounds on Pods and on compute and ephemeral
// storage (in request, limit or plain form). Only unscoped quotas count: a quota
// restricted by scopes or a scope selector bounds some class of Pods, and
// nothing shows executor Pods belong to it. Evaluating scope semantics is
// deliberately not attempted; deployments provide an unscoped quota. Any
// listed bound that is zero, negative or unparsable denies, whichever quota
// carries it.
func checkQuota(qs []kubernetes.ResourceQuota) error {
	need := [][]string{
		{"pods"},
		{"limits.cpu", "requests.cpu", "cpu"},
		{"limits.memory", "requests.memory", "memory"},
		{"limits.ephemeral-storage", "requests.ephemeral-storage", "ephemeral-storage"},
	}
	for _, names := range need {
		found := false
		for _, q := range qs {
			if q.Scoped() {
				continue
			}
			for _, n := range names {
				v, ok := q.Spec.Hard[n]
				if !ok {
					continue
				}
				qty, err := ParseQuantity(v)
				if err != nil || qty <= 0 {
					return deny("the resource quota %s is not a positive bound", n)
				}
				found = true
			}
		}
		if !found {
			return deny("the namespace has no unscoped resource quota for %s", names[0])
		}
	}
	return nil
}

// Executor Pods carry the managed-by label with a fixed value. The Job controller
// and the recipe add labels whose values are per-Job (the Job's name and UID),
// unknown to a namespace-level check; a selector naming one of them is treated as
// possibly matching.
var dynamicWorkloadLabels = map[string]bool{
	LabelJob:         true,
	PodLabelJobUID:   true,
	PodLabelJobName:  true,
	"controller-uid": true, // legacy Job controller labels
	"job-name":       true,
}

// selectsWorkload reports whether a policy's Pod selector may select executor
// Pods. A selector with a label no executor Pod carries, or a different value
// for the fixed one, demonstrably selects something else. A selector naming only
// the fixed label or per-Job labels may select workload Pods. Selector
// expressions are not evaluated: unless matchLabels already excludes the
// workload, they are reported unsupported.
func selectsWorkload(sel kubernetes.LabelSelector) (selects bool, unsupported bool) {
	for k, v := range sel.MatchLabels {
		switch {
		case k == LabelManagedBy && v == ValueManagedBy:
		case dynamicWorkloadLabels[k]:
		default:
			return false, false
		}
	}
	if len(sel.MatchExpressions) > 0 {
		return true, true
	}
	return true, false
}

func hasType(pol kubernetes.NetworkPolicy, t string) bool {
	for _, x := range pol.Spec.PolicyTypes {
		if x == t {
			return true
		}
	}
	return false
}

// checkNetwork requires a namespace-wide default deny (ingress and egress, no
// rules) and refuses any policy selecting workload Pods that admits ingress or
// egress beyond the declared destinations.
func checkNetwork(pols []kubernetes.NetworkPolicy, allowed []EgressRule) error {
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
		sel, unsupported := selectsWorkload(pol.Spec.PodSelector)
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

func portNumber(p any) (int32, bool) {
	switch v := p.(type) {
	case float64:
		return int32(v), v == float64(int32(v))
	case json.Number:
		n, err := v.Int64()
		return int32(n), err == nil
	case int:
		return int32(v), true
	case int32:
		return v, true
	case int64:
		return int32(v), true
	}
	return 0, false // named ports and wildcards are not accepted
}

// clusterDNS is the one non-address peer the baseline allows: name resolution of the
// callback and of declared egress destinations. It is exactly kube-system's kube-dns
// on port 53, and nothing else. A ClusterIP ipBlock does not reach CoreDNS under
// Cilium's kube-proxy replacement, so the peer has to be the Service's Pods.
func clusterDNS(r kubernetes.NetworkEgress) bool {
	if len(r.To) != 1 || len(r.Ports) == 0 {
		return false
	}
	peer := r.To[0]
	if peer.IPBlock != nil || peer.NamespaceSelector == nil || peer.PodSelector == nil {
		return false
	}
	if len(peer.NamespaceSelector.MatchExpressions) != 0 || len(peer.PodSelector.MatchExpressions) != 0 {
		return false
	}
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" || len(peer.NamespaceSelector.MatchLabels) != 1 {
		return false
	}
	if peer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" || len(peer.PodSelector.MatchLabels) != 1 {
		return false
	}
	for _, port := range r.Ports {
		n, ok := portNumber(port.Port)
		if !ok || n != 53 || port.EndPort != nil {
			return false
		}
		proto := port.Protocol
		if proto == "" {
			proto = "TCP"
		}
		if proto != "UDP" && proto != "TCP" {
			return false
		}
	}
	return true
}

// egressCovered requires an egress rule to name explicit destinations and ports,
// all inside one declared allowed rule.
func egressCovered(r kubernetes.NetworkEgress, allowed []EgressRule) error {
	if len(r.To) == 0 || len(r.Ports) == 0 {
		return deny("an egress rule admits all destinations or all ports")
	}
	for _, peer := range r.To {
		if peer.IPBlock == nil || peer.NamespaceSelector != nil || peer.PodSelector != nil {
			return deny("an egress rule names a peer that is not an address block")
		}
		cidr, err := netip.ParsePrefix(peer.IPBlock.CIDR)
		if err != nil {
			return deny("an egress rule has an invalid address block")
		}
		cidr = cidr.Masked()
		for _, port := range r.Ports {
			n, ok := portNumber(port.Port)
			if !ok {
				return deny("an egress rule uses a named or wildcard port")
			}
			if port.EndPort != nil {
				return deny("an egress rule uses a port range")
			}
			proto := port.Protocol
			if proto == "" {
				proto = "TCP"
			}
			if !coveredBy(cidr, proto, n, allowed) {
				return deny("an egress rule exceeds the declared runtime egress")
			}
		}
	}
	return nil
}

func coveredBy(cidr netip.Prefix, proto string, port int32, allowed []EgressRule) bool {
	for _, a := range allowed {
		outer, err := netip.ParsePrefix(a.CIDR)
		if err != nil || a.Protocol != proto {
			continue
		}
		outer = outer.Masked()
		if outer.Bits() <= cidr.Bits() && outer.Contains(cidr.Addr()) {
			for _, p := range a.Ports {
				if p == port {
					return true
				}
			}
		}
	}
	return false
}

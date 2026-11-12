//go:build qualification

package production

// Deployment-object isolation checks for the Q02 runtime-isolation probe.
//
// Every function here is pure: it parses one bounded Kubernetes object the
// probe read through the confinement guard and returns a refusal instead of
// accepting a widened boundary. The unit tests feed the same functions both the
// chart's compliant shape and a deliberately unsafe shape, so a probe that
// stopped detecting a foreign tenant peer, an unbounded workload or a missing
// limit range would fail before it ever ran on a stage.

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	isolationNamespacePrefix     = "kodama-tenant-"
	isolationLimitRangeName      = "kodama-tenant-limits"
	isolationNetworkPolicyName   = "kodama-tenant-default-deny"
	isolationLabelTenant         = "kodama.dev/tenant"
	isolationNamespaceNameLabel  = "kubernetes.io/metadata.name"
	isolationPodSecurityEnforce  = "pod-security.kubernetes.io/enforce"
	isolationPodSecurityWarn     = "pod-security.kubernetes.io/warn"
	isolationPodSecurityAudit    = "pod-security.kubernetes.io/audit"
	isolationReleaseDNSNamespace = "kube-system"
	isolationGitNamespace        = "git"
)

// isolationNamespace is one Kubernetes Namespace object.
type isolationNamespace struct {
	Metadata struct {
		Name   string            `json:"name"`
		UID    string            `json:"uid"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

// isolationLimitRange is one Kubernetes LimitRange object.
type isolationLimitRange struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Limits []struct {
			Type           string            `json:"type"`
			Default        map[string]string `json:"default"`
			DefaultRequest map[string]string `json:"defaultRequest"`
		} `json:"limits"`
	} `json:"spec"`
}

// isolationResources is the CPU/memory request and limit pair the LimitRange
// publishes and a workload Pod must carry.
type isolationResources struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// isolationSelector is one label selector inside a NetworkPolicy peer.
type isolationSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

// isolationPeerSelector is one from/to peer.
type isolationPeerSelector struct {
	NamespaceSelector *isolationSelector `json:"namespaceSelector"`
	PodSelector       *isolationSelector `json:"podSelector"`
	IPBlock           *struct {
		CIDR string `json:"cidr"`
	} `json:"ipBlock"`
}

// isolationPort is one NetworkPolicy port. Kubernetes renders a numeric port as
// a JSON number and a named port as a string, so the raw form is kept.
type isolationPort struct {
	Protocol string          `json:"protocol"`
	Port     json.RawMessage `json:"port"`
}

// isolationRule is one ingress or egress rule.
type isolationRule struct {
	From  []isolationPeerSelector `json:"from"`
	To    []isolationPeerSelector `json:"to"`
	Ports []isolationPort         `json:"ports"`
}

// isolationNetworkPolicy is one NetworkPolicy object.
type isolationNetworkPolicy struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		PodSelector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"podSelector"`
		PolicyTypes []string        `json:"policyTypes"`
		Ingress     []isolationRule `json:"ingress"`
		Egress      []isolationRule `json:"egress"`
	} `json:"spec"`
}

// isolationPod is the bounded workload-Pod fact set the isolation probe reads.
type isolationPod struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		HostNetwork                  bool  `json:"hostNetwork"`
		HostPID                      bool  `json:"hostPID"`
		HostIPC                      bool  `json:"hostIPC"`
		AutomountServiceAccountToken *bool `json:"automountServiceAccountToken"`
		Volumes                      []struct {
			Name     string `json:"name"`
			HostPath *struct {
				Path string `json:"path"`
			} `json:"hostPath"`
		} `json:"volumes"`
		Containers []struct {
			Name            string `json:"name"`
			SecurityContext struct {
				Privileged *bool `json:"privileged"`
			} `json:"securityContext"`
			Resources struct {
				Requests map[string]string `json:"requests"`
				Limits   map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
		PodIP string `json:"podIP"`
	} `json:"status"`
}

// isolationPodList is one bounded Pod list.
type isolationPodList struct {
	Items []isolationPod `json:"items"`
}

// isolationNamespaceForTenant derives the chart's namespace for one tenant.
func isolationNamespaceForTenant(tenantID string) string {
	return isolationNamespacePrefix + tenantID
}

// parseIsolationNamespace decodes one Namespace object.
func parseIsolationNamespace(data []byte) (isolationNamespace, error) {
	var object isolationNamespace
	if err := json.Unmarshal(data, &object); err != nil {
		return isolationNamespace{}, fmt.Errorf("namespace JSON is unreadable")
	}
	if object.Metadata.Name == "" {
		return isolationNamespace{}, fmt.Errorf("namespace object has no name")
	}
	return object, nil
}

// validateTenantNamespace proves one assigned tenant namespace carries the
// tenant label and the baseline/restricted pod-security profile, and is not the
// other tenant's namespace.
func validateTenantNamespace(object isolationNamespace, tenantID, tenantNamespace, otherNamespace string) error {
	if object.Metadata.Name != tenantNamespace {
		return fmt.Errorf("namespace object is %q, want %q", object.Metadata.Name, tenantNamespace)
	}
	if tenantNamespace != isolationNamespaceForTenant(tenantID) {
		return fmt.Errorf("tenant namespace %q is not the derived namespace %q", tenantNamespace, isolationNamespaceForTenant(tenantID))
	}
	if otherNamespace != "" && tenantNamespace == otherNamespace {
		return fmt.Errorf("both tenants were assigned the same namespace %q", tenantNamespace)
	}
	if object.Metadata.Labels[isolationLabelTenant] != tenantID {
		return fmt.Errorf("namespace %q tenant label = %q, want %q", tenantNamespace, object.Metadata.Labels[isolationLabelTenant], tenantID)
	}
	if object.Metadata.Labels[isolationPodSecurityEnforce] != "baseline" {
		return fmt.Errorf("namespace %q pod-security enforce = %q, want baseline", tenantNamespace, object.Metadata.Labels[isolationPodSecurityEnforce])
	}
	if object.Metadata.Labels[isolationPodSecurityWarn] != "restricted" {
		return fmt.Errorf("namespace %q pod-security warn = %q, want restricted", tenantNamespace, object.Metadata.Labels[isolationPodSecurityWarn])
	}
	if object.Metadata.Labels[isolationPodSecurityAudit] != "restricted" {
		return fmt.Errorf("namespace %q pod-security audit = %q, want restricted", tenantNamespace, object.Metadata.Labels[isolationPodSecurityAudit])
	}
	return nil
}

// parseIsolationLimitRange decodes one LimitRange object and returns its
// Container default request/limit pair.
func parseIsolationLimitRange(data []byte, tenantID, tenantNamespace string) (isolationResources, error) {
	var object isolationLimitRange
	if err := json.Unmarshal(data, &object); err != nil {
		return isolationResources{}, fmt.Errorf("limit range JSON is unreadable")
	}
	if object.Metadata.Name != isolationLimitRangeName {
		return isolationResources{}, fmt.Errorf("limit range object is %q, want %q", object.Metadata.Name, isolationLimitRangeName)
	}
	if object.Metadata.Namespace != tenantNamespace {
		return isolationResources{}, fmt.Errorf("limit range namespace = %q, want %q", object.Metadata.Namespace, tenantNamespace)
	}
	if object.Metadata.Labels[isolationLabelTenant] != tenantID {
		return isolationResources{}, fmt.Errorf("limit range tenant label = %q, want %q", object.Metadata.Labels[isolationLabelTenant], tenantID)
	}
	for _, limit := range object.Spec.Limits {
		if limit.Type != "Container" {
			continue
		}
		resources := isolationResources{
			CPURequest:    limit.DefaultRequest["cpu"],
			CPULimit:      limit.Default["cpu"],
			MemoryRequest: limit.DefaultRequest["memory"],
			MemoryLimit:   limit.Default["memory"],
		}
		if err := validateIsolationResources(resources); err != nil {
			return isolationResources{}, fmt.Errorf("limit range %q: %w", object.Metadata.Name, err)
		}
		return resources, nil
	}
	return isolationResources{}, fmt.Errorf("limit range %q has no Container defaults", object.Metadata.Name)
}

// parseIsolationNetworkPolicy decodes one NetworkPolicy object.
func parseIsolationNetworkPolicy(data []byte) (isolationNetworkPolicy, error) {
	var object isolationNetworkPolicy
	if err := json.Unmarshal(data, &object); err != nil {
		return isolationNetworkPolicy{}, fmt.Errorf("network policy JSON is unreadable")
	}
	if object.Metadata.Name == "" {
		return isolationNetworkPolicy{}, fmt.Errorf("network policy object has no name")
	}
	return object, nil
}

// validateTenantNetworkPolicy proves the tenant policy is a default deny in
// both directions, permits only cluster DNS and the release namespace's Kodama
// service (plus the optional Git namespace), and contains no peer that selects
// another tenant or every namespace. Ingress may only come from the release
// namespace: kube-system and git are egress-only destinations, and every rule
// must name the exact ports the chart renders, so an unpublished all-port rule
// is refused. The chart emits namespace peers only, so any ipBlock is refused.
func validateTenantNetworkPolicy(object isolationNetworkPolicy, tenantID, tenantNamespace, otherNamespace, releaseNamespace string) error {
	if object.Metadata.Name != isolationNetworkPolicyName {
		return fmt.Errorf("network policy object is %q, want %q", object.Metadata.Name, isolationNetworkPolicyName)
	}
	if object.Metadata.Namespace != tenantNamespace {
		return fmt.Errorf("network policy namespace = %q, want %q", object.Metadata.Namespace, tenantNamespace)
	}
	if object.Metadata.Labels[isolationLabelTenant] != tenantID {
		return fmt.Errorf("network policy tenant label = %q, want %q", object.Metadata.Labels[isolationLabelTenant], tenantID)
	}
	if len(object.Spec.PodSelector.MatchLabels) != 0 {
		return fmt.Errorf("network policy %q is not the empty-pod-selector default deny", object.Metadata.Name)
	}
	hasIngress, hasEgress := false, false
	for _, policyType := range object.Spec.PolicyTypes {
		switch policyType {
		case "Ingress":
			hasIngress = true
		case "Egress":
			hasEgress = true
		}
	}
	if !hasIngress || !hasEgress {
		return fmt.Errorf("network policy %q does not deny both ingress and egress", object.Metadata.Name)
	}
	if len(object.Spec.Ingress) == 0 {
		return fmt.Errorf("network policy %q admits no Kodama ingress", object.Metadata.Name)
	}
	if len(object.Spec.Egress) == 0 {
		return fmt.Errorf("network policy %q admits no cluster DNS egress", object.Metadata.Name)
	}

	ingressPeers := map[string]bool{releaseNamespace: true}
	egressPeers := map[string]bool{
		isolationReleaseDNSNamespace: true,
		releaseNamespace:             true,
		isolationGitNamespace:        true,
	}
	for index, rule := range object.Spec.Ingress {
		if err := validateIsolationRule(rule, true, tenantNamespace, otherNamespace, ingressPeers); err != nil {
			return fmt.Errorf("network policy %q ingress rule %d: %w", object.Metadata.Name, index, err)
		}
	}
	for index, rule := range object.Spec.Egress {
		if err := validateIsolationRule(rule, false, tenantNamespace, otherNamespace, egressPeers); err != nil {
			return fmt.Errorf("network policy %q egress rule %d: %w", object.Metadata.Name, index, err)
		}
	}
	if !isolationPolicyAllows(object.Spec.Ingress, releaseNamespace, 8443, true) {
		return fmt.Errorf("network policy %q does not admit Kodama ingress on 8443 from %q", object.Metadata.Name, releaseNamespace)
	}
	if !isolationPolicyAllows(object.Spec.Egress, isolationReleaseDNSNamespace, 53, false) {
		return fmt.Errorf("network policy %q does not admit cluster DNS egress on 53", object.Metadata.Name)
	}
	if !isolationPolicyAllows(object.Spec.Egress, releaseNamespace, 8443, false) {
		return fmt.Errorf("network policy %q does not admit Kodama egress on 8443 to %q", object.Metadata.Name, releaseNamespace)
	}
	return nil
}

// validateIsolationRule proves one rule names only allowlisted namespace peers
// for its direction and carries an explicit port for each peer that the chart
// publishes. A rule without ports admits every port, and a same-namespace pod
// selector is not a chart shape, so both fail closed.
func validateIsolationRule(rule isolationRule, ingress bool, tenantNamespace, otherNamespace string, allowed map[string]bool) error {
	peers := rule.To
	direction := "destination"
	if ingress {
		peers = rule.From
		direction = "source"
	}
	if len(peers) == 0 {
		return fmt.Errorf("rule admits every %s", direction)
	}
	namespaces := make([]string, 0, len(peers))
	for peerIndex, peer := range peers {
		name, err := validateIsolationPeer(peer, tenantNamespace, otherNamespace, allowed)
		if err != nil {
			return fmt.Errorf("peer %d: %w", peerIndex, err)
		}
		namespaces = append(namespaces, name)
	}
	if len(rule.Ports) == 0 {
		return fmt.Errorf("rule admits every port")
	}
	for _, port := range rule.Ports {
		for _, namespace := range namespaces {
			if !isolationRulePortAllowed(ingress, namespace, port) {
				return fmt.Errorf("port %s is not an allowed %s port for namespace %q", isolationPortText(port), direction, namespace)
			}
		}
	}
	return nil
}

// validateIsolationPeer refuses an ipBlock, a peer that selects every
// namespace, a same-namespace pod selector, the tenant's own namespace, another
// tenant, or an unnamed namespace outside the pinned allowlist. It returns the
// selected namespace so the rule-port check can use it.
func validateIsolationPeer(peer isolationPeerSelector, tenantNamespace, otherNamespace string, allowed map[string]bool) (string, error) {
	if peer.IPBlock != nil {
		return "", fmt.Errorf("peer uses an ipBlock; the chart emits namespace peers only")
	}
	if peer.NamespaceSelector == nil {
		return "", fmt.Errorf("peer does not name a namespace; a same-namespace pod selector is not the chart shape")
	}
	if len(peer.NamespaceSelector.MatchLabels) == 0 {
		return "", fmt.Errorf("peer selects every namespace")
	}
	name := peer.NamespaceSelector.MatchLabels[isolationNamespaceNameLabel]
	if name == "" {
		return "", fmt.Errorf("peer namespace selector does not name a namespace")
	}
	if name == tenantNamespace {
		return "", fmt.Errorf("peer selects the tenant's own namespace")
	}
	if otherNamespace != "" && name == otherNamespace {
		return "", fmt.Errorf("peer selects the other tenant namespace %q", name)
	}
	if !allowed[name] {
		return "", fmt.Errorf("peer selects unlisted namespace %q", name)
	}
	return name, nil
}

// isolationRulePortAllowed pins one NetworkPolicy port to the chart's published
// route: cluster DNS answers UDP/TCP 53, the Git service answers TCP 443, and
// the release namespace answers the Kodama ports 8443 and 8080. Egress may also
// use the chart's configurable unprivileged Git service port in the release
// namespace. Ingress never reaches beyond the Kodama ports.
func isolationRulePortAllowed(ingress bool, namespace string, port isolationPort) bool {
	protocol := strings.ToUpper(port.Protocol)
	if protocol == "" {
		protocol = "TCP"
	}
	value, ok := isolationPortValue(port.Port)
	if !ok {
		return false
	}
	switch namespace {
	case isolationReleaseDNSNamespace:
		return !ingress && (protocol == "UDP" || protocol == "TCP") && value == 53
	case isolationGitNamespace:
		return !ingress && protocol == "TCP" && value == 443
	default:
		if protocol != "TCP" {
			return false
		}
		if value == 8443 || value == 8080 {
			return true
		}
		return !ingress && value >= 1024 && value <= 65535
	}
}

// isolationPortText renders one port for a refusal message without echoing a
// value that could not be parsed.
func isolationPortText(port isolationPort) string {
	protocol := strings.ToUpper(port.Protocol)
	if protocol == "" {
		protocol = "TCP"
	}
	return protocol + "/" + strings.TrimSpace(string(port.Port))
}

// isolationPolicyAllows reports whether one rule set contains a peer for the
// named namespace on the requested TCP port. A rule without ports admits every
// port.
func isolationPolicyAllows(rules []isolationRule, namespace string, port int, ingress bool) bool {
	for _, rule := range rules {
		peers := rule.To
		if ingress {
			peers = rule.From
		}
		matched := false
		for _, peer := range peers {
			if peer.NamespaceSelector == nil {
				continue
			}
			if peer.NamespaceSelector.MatchLabels[isolationNamespaceNameLabel] == namespace {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if len(rule.Ports) == 0 {
			return true
		}
		for _, candidate := range rule.Ports {
			if protocol := strings.ToUpper(candidate.Protocol); protocol != "" && protocol != "TCP" {
				continue
			}
			value, ok := isolationPortValue(candidate.Port)
			if ok && value == port {
				return true
			}
		}
	}
	return false
}

// isolationPortValue decodes one numeric NetworkPolicy port.
func isolationPortValue(raw json.RawMessage) (int, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return 0, false
	}
	if strings.HasPrefix(text, `"`) {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return 0, false
		}
		text = name
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < 1 || value > 65535 {
		return 0, false
	}
	return value, true
}

// parseIsolationPods decodes one bounded Pod list.
func parseIsolationPods(data []byte) ([]isolationPod, error) {
	var list isolationPodList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("pod list JSON is unreadable")
	}
	return list.Items, nil
}

// validateIsolationPod proves one workload Pod is tenant-labelled, has no host
// namespace or host path, and carries a positive CPU/memory request and limit
// on every container. The namespace LimitRange is checked separately: a
// LimitRange publishes defaults, not a maximum, so a Pod may legitimately carry
// a different explicitly configured bound.
func validateIsolationPod(pod isolationPod, tenantID, tenantNamespace string, _ isolationResources) error {
	if pod.Metadata.Namespace != "" && pod.Metadata.Namespace != tenantNamespace {
		return fmt.Errorf("pod %q namespace = %q, want %q", pod.Metadata.Name, pod.Metadata.Namespace, tenantNamespace)
	}
	if pod.Metadata.Labels[isolationLabelTenant] != tenantID {
		return fmt.Errorf("pod %q tenant label = %q, want %q", pod.Metadata.Name, pod.Metadata.Labels[isolationLabelTenant], tenantID)
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		return fmt.Errorf("pod %q shares a host namespace", pod.Metadata.Name)
	}
	if pod.Spec.AutomountServiceAccountToken != nil && *pod.Spec.AutomountServiceAccountToken {
		return fmt.Errorf("pod %q automounts a service account token", pod.Metadata.Name)
	}
	// A nil automountServiceAccountToken inherits the ServiceAccount default,
	// which is true. Only an explicit false, or a pod whose volumes carry no
	// kube-api-access projection, proves the token is not mounted.
	kubeAPIAccessVolume := false
	for _, volume := range pod.Spec.Volumes {
		if strings.HasPrefix(volume.Name, "kube-api-access-") {
			kubeAPIAccessVolume = true
		}
		if volume.HostPath != nil {
			return fmt.Errorf("pod %q mounts host path %q", pod.Metadata.Name, volume.HostPath.Path)
		}
	}
	if pod.Spec.AutomountServiceAccountToken == nil && kubeAPIAccessVolume {
		return fmt.Errorf("pod %q leaves the service account token automount implicit while mounting a kube-api-access volume", pod.Metadata.Name)
	}
	if len(pod.Spec.Containers) == 0 {
		return fmt.Errorf("pod %q has no containers", pod.Metadata.Name)
	}
	for _, container := range pod.Spec.Containers {
		if container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
			return fmt.Errorf("pod %q container %q is privileged", pod.Metadata.Name, container.Name)
		}
		pair := isolationResources{
			CPURequest:    container.Resources.Requests["cpu"],
			CPULimit:      container.Resources.Limits["cpu"],
			MemoryRequest: container.Resources.Requests["memory"],
			MemoryLimit:   container.Resources.Limits["memory"],
		}
		if err := validateIsolationResources(pair); err != nil {
			return fmt.Errorf("pod %q container %q: %w", pod.Metadata.Name, container.Name, err)
		}
	}
	return nil
}

// validateIsolationResources requires a positive request and limit pair with a
// limit of at least the request.
func validateIsolationResources(resources isolationResources) error {
	cpuRequest, err := isolationCPU(resources.CPURequest)
	if err != nil {
		return fmt.Errorf("cpu request: %w", err)
	}
	cpuLimit, err := isolationCPU(resources.CPULimit)
	if err != nil {
		return fmt.Errorf("cpu limit: %w", err)
	}
	memoryRequest, err := isolationMemory(resources.MemoryRequest)
	if err != nil {
		return fmt.Errorf("memory request: %w", err)
	}
	memoryLimit, err := isolationMemory(resources.MemoryLimit)
	if err != nil {
		return fmt.Errorf("memory limit: %w", err)
	}
	if cpuLimit < cpuRequest {
		return fmt.Errorf("cpu limit %q is below the request %q", resources.CPULimit, resources.CPURequest)
	}
	if memoryLimit < memoryRequest {
		return fmt.Errorf("memory limit %q is below the request %q", resources.MemoryLimit, resources.MemoryRequest)
	}
	return nil
}

// isolationCPU parses the Kubernetes CPU quantity subset the deployment uses
// into millicores.
func isolationCPU(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("quantity is empty")
	}
	text := strings.TrimSpace(value)
	scale := 1000.0
	if before, ok := strings.CutSuffix(text, "m"); ok {
		text = before
		scale = 1
	} else if before, ok := strings.CutSuffix(text, "n"); ok {
		text = before
		scale = 0.000001
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("quantity %q is not a positive CPU value", value)
	}
	return int64(number * scale), nil
}

// isolationMemory parses the Kubernetes memory quantity subset the deployment
// uses into bytes.
func isolationMemory(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("quantity is empty")
	}
	text := strings.TrimSpace(value)
	multipliers := []struct {
		suffix string
		scale  float64
	}{
		{"Ki", 1 << 10},
		{"Mi", 1 << 20},
		{"Gi", 1 << 30},
		{"Ti", 1 << 40},
		{"K", 1e3},
		{"M", 1e6},
		{"G", 1e9},
		{"T", 1e12},
	}
	scale := 1.0
	for _, candidate := range multipliers {
		if before, ok := strings.CutSuffix(text, candidate.suffix); ok {
			text = before
			scale = candidate.scale
			break
		}
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("quantity %q is not a positive memory value", value)
	}
	return int64(number * scale), nil
}

// isolationValidPodIP requires one readable pod address for the live network
// probe.
func isolationValidPodIP(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if net.ParseIP(value) == nil {
		return "", fmt.Errorf("pod address %q is not an IP address", raw)
	}
	return value, nil
}

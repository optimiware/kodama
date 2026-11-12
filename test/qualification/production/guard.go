//go:build qualification

package production

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ReleaseNamespace is the one non-tenant namespace a Q02 run may mutate.
const ReleaseNamespace = "kodama-system"

// Access is one namespaced kubectl access the guard evaluates before a process
// starts.
type Access struct {
	Context   string
	Namespace string
	Verb      string
	Resource  string
}

// Guard confines every kubectl access to the assigned context and namespaces,
// and confines mutations to namespaced workload resources. It knows no node,
// cluster-scoped, tofu or cloud-resource operation.
type Guard struct {
	context    string
	namespaces map[string]struct{}
}

// NewGuard validates the assigned kube context and tenant namespaces. System
// namespaces (including the release namespace) can never be assigned as tenant
// namespaces.
func NewGuard(kubeContext string, tenantNamespaces ...string) (*Guard, error) {
	if err := validateKubeContext(kubeContext); err != nil {
		return nil, fmt.Errorf("kubernetes context %q: %w", kubeContext, err)
	}
	guard := &Guard{context: kubeContext, namespaces: map[string]struct{}{}}
	for _, namespace := range tenantNamespaces {
		if !dnsLabelPattern.MatchString(namespace) {
			return nil, fmt.Errorf("tenant namespace %q is not a kubernetes namespace", namespace)
		}
		if systemNamespaces[namespace] {
			return nil, fmt.Errorf("tenant namespace %q is a system namespace", namespace)
		}
		if _, duplicate := guard.namespaces[namespace]; duplicate {
			return nil, fmt.Errorf("tenant namespace %q is assigned twice", namespace)
		}
		guard.namespaces[namespace] = struct{}{}
	}
	return guard, nil
}

// Context returns the one assigned kube context.
func (guard *Guard) Context() string { return guard.context }

// NamespaceAllowed reports whether a namespace is the release namespace or an
// assigned tenant namespace.
func (guard *Guard) NamespaceAllowed(namespace string) bool {
	if namespace == ReleaseNamespace {
		return true
	}
	_, ok := guard.namespaces[namespace]
	return ok
}

// Admit refuses an access outside the assigned context or namespace, an
// unknown or cluster-scoped resource, and any verb outside the known read and
// mutation sets. It returns only a refusal; use Normalize to obtain the values
// that should be executed.
func (guard *Guard) Admit(access Access) error {
	_, err := guard.Normalize(access)
	return err
}

// Normalize trims and lower-cases the verb and resource, admits the normalized
// access, and returns exactly the values a caller must execute. The context and
// namespace stay exact: whitespace in either is never silently repaired.
func (guard *Guard) Normalize(access Access) (Access, error) {
	access.Verb = strings.ToLower(strings.TrimSpace(access.Verb))
	access.Resource = strings.ToLower(strings.TrimSpace(access.Resource))
	if access.Context != guard.context {
		return Access{}, fmt.Errorf("kubernetes context %q is not the assigned context", access.Context)
	}
	if !guard.NamespaceAllowed(access.Namespace) {
		return Access{}, fmt.Errorf("namespace %q is outside the assigned kubectl scope", access.Namespace)
	}
	if access.Resource == "" {
		return Access{}, fmt.Errorf("kubectl resource is empty")
	}
	// A qualification probe may read exactly one assigned tenant (or release)
	// namespace object to verify its labels. A list, a create, and any foreign
	// name stay refused: the namespace scope is never widened to the cluster.
	if after, ok := strings.CutPrefix(access.Resource, "namespaces/"); ok {
		name := after
		if access.Verb != "get" && access.Verb != "describe" {
			return Access{}, fmt.Errorf("kubectl namespace access is read-only")
		}
		if !dnsLabelPattern.MatchString(name) || !guard.NamespaceAllowed(name) {
			return Access{}, fmt.Errorf("namespace %q is outside the assigned kubectl scope", name)
		}
		return access, nil
	}
	if deniedResources[access.Resource] || deniedResources[baseResource(access.Resource)] {
		return Access{}, fmt.Errorf("kubernetes resource %q is never available to qualification", access.Resource)
	}
	if refusedVerbs[access.Verb] {
		return Access{}, fmt.Errorf("kubectl verb %q is never available to qualification", access.Verb)
	}
	switch {
	case mutationVerbs[access.Verb]:
		if !allowedMutations[access.Resource] {
			return Access{}, fmt.Errorf("kubernetes mutation of %q is not allowlisted", access.Resource)
		}
		// Claim mutation is limited to deleting one owned retained workspace
		// claim in an assigned tenant namespace. kodama-system holds the Git
		// keep claim and the NATS claim, whose loss is never a probe outcome.
		if baseResource(access.Resource) == "persistentvolumeclaims" {
			if access.Verb != "delete" {
				return Access{}, fmt.Errorf("kubernetes claim mutation must be delete, not %q", access.Verb)
			}
			if access.Namespace == ReleaseNamespace {
				return Access{}, fmt.Errorf("kubernetes claim mutation is limited to tenant namespaces, not %q", access.Namespace)
			}
		}
	case readVerbs[access.Verb]:
		if !allowedReads[access.Resource] {
			return Access{}, fmt.Errorf("kubernetes read of %q is not allowlisted", access.Resource)
		}
	default:
		return Access{}, fmt.Errorf("kubectl verb %q is not allowlisted", access.Verb)
	}
	return access, nil
}

// CheckExecutable admits only kubectl. A tofu, cloud CLI or any other process is
// refused before it can start.
func (guard *Guard) CheckExecutable(path string) error {
	base := strings.TrimSuffix(filepath.Base(path), ".exe")
	if base != "kubectl" {
		return fmt.Errorf("executable %q is not kubectl", base)
	}
	return nil
}

// baseResource strips a subresource suffix.
func baseResource(resource string) string {
	if before, _, ok := strings.Cut(resource, "/"); ok {
		return before
	}
	return resource
}

// systemNamespaces can never be assigned to a tenant qualification scope.
var systemNamespaces = map[string]bool{
	"default":         true,
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
	ReleaseNamespace:  true,
}

// refusedVerbs are refused even if a read or mutation allowlist is extended
// later. They reach a shell, copy files, edit live objects, proxy, or query
// authorization outside the harness's namespaced workload scope.
var refusedVerbs = map[string]bool{
	"exec":   true,
	"cp":     true,
	"attach": true,
	"edit":   true,
	"proxy":  true,
	"auth":   true,
}

var mutationVerbs = map[string]bool{
	"create":           true,
	"delete":           true,
	"deletecollection": true,
	"patch":            true,
	"replace":          true,
	"update":           true,
	"scale":            true,
	"evict":            true,
	"annotate":         true,
	"label":            true,
	"rollout":          true,
}

var readVerbs = map[string]bool{
	"get":          true,
	"list":         true,
	"describe":     true,
	"logs":         true,
	"wait":         true,
	"port-forward": true,
	"top":          true,
}

var allowedMutations = map[string]bool{
	"pods":          true,
	"pods/eviction": true,
	"deployments":   true,
	"statefulsets":  true,
	"daemonsets":    true,
	"replicasets":   true,
	"jobs":          true,
	"cronjobs":      true,
	// A workspace-loss probe must delete exactly one owned retained claim in an
	// assigned tenant namespace. The live probe is gated on an explicit
	// disposable-stage confirmation; Normalize additionally refuses any other
	// claim verb and every claim mutation in kodama-system, which holds the Git
	// keep claim and the NATS claim.
	"persistentvolumeclaims": true,
}

var allowedReads = map[string]bool{
	"pods":                   true,
	"pods/log":               true,
	"services":               true,
	"deployments":            true,
	"statefulsets":           true,
	"daemonsets":             true,
	"replicasets":            true,
	"jobs":                   true,
	"cronjobs":               true,
	"configmaps":             true,
	"secrets":                true,
	"events":                 true,
	"endpoints":              true,
	"persistentvolumeclaims": true,
	"limitranges":            true,
	"networkpolicies":        true,
}

// deniedResources are refused even when an allowlist map is extended later.
var deniedResources = map[string]bool{
	"nodes":                           true,
	"node":                            true,
	"namespaces":                      true,
	"namespace":                       true,
	"persistentvolumes":               true,
	"persistentvolume":                true,
	"storageclasses":                  true,
	"storageclass":                    true,
	"customresourcedefinitions":       true,
	"customresourcedefinition":        true,
	"clusterroles":                    true,
	"clusterrole":                     true,
	"clusterrolebindings":             true,
	"clusterrolebinding":              true,
	"priorityclasses":                 true,
	"priorityclass":                   true,
	"volumeattachments":               true,
	"volumeattachment":                true,
	"csidrivers":                      true,
	"csidriver":                       true,
	"mutatingwebhookconfigurations":   true,
	"validatingwebhookconfigurations": true,
}

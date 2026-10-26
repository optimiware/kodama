package runprofile

import (
	"fmt"
	"sort"
	"strings"
)

// NamespacePrefix is the only namespace prefix the qualifier may create,
// mutate or delete.
const NamespacePrefix = "krp-t111-"

// Labels every created object carries; cleanup trusts nothing else.
const (
	LabelTask = "openforge.dev/task"
	LabelRun  = "openforge.dev/run"
	TaskValue = "t111"
)

// allowedAddCaps are the only capabilities a probe Pod may add. Every probe
// Pod is a user-namespaced Pod (hostUsers false) so they apply inside its
// own user namespace; none reaches host privilege. SYS_ADMIN is present
// because the container runtime's default seccomp profile lets a process
// create a user namespace only when that capability is in the container's
// bounding set. It is never permitted together with host namespaces.
var allowedAddCaps = map[string]bool{"SETUID": true, "SETGID": true, "SYS_ADMIN": true, "SYS_CHROOT": true}

// supportedKinds is the closed set of kinds the qualifier may write. Every one
// is namespaced except Namespace itself, which Guard confines to the task prefix.
var supportedKinds = map[string]bool{
	"Namespace": true, "ResourceQuota": true, "PersistentVolumeClaim": true, "Pod": true,
	"Service": true, "Secret": true, "NetworkPolicy": true,
}

// Guard refuses any object that would give a probe host privilege or place
// it outside the task's own namespaces. It is applied to every manifest
// before kubectl sees it, so a typo in a fixture cannot escalate.
func Guard(obj map[string]any, runID string) error {
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if kind == "" || name == "" {
		return fmt.Errorf("guard: object lacks kind or name")
	}
	if !supportedKinds[kind] {
		return fmt.Errorf("guard: object kind %q is not supported (cluster-scoped or unknown writes are refused)", kind)
	}
	ns, _ := meta["namespace"].(string)
	switch kind {
	case "Namespace":
		if !strings.HasPrefix(name, NamespacePrefix) {
			return fmt.Errorf("guard: namespace %q lacks prefix %s", name, NamespacePrefix)
		}
	default:
		if !strings.HasPrefix(ns, NamespacePrefix) {
			return fmt.Errorf("guard: %s/%s targets namespace %q outside %s*", kind, name, ns, NamespacePrefix)
		}
	}
	labels, _ := meta["labels"].(map[string]any)
	if labels[LabelTask] != TaskValue || labels[LabelRun] != runID {
		return fmt.Errorf("guard: %s/%s lacks task/run ownership labels", kind, name)
	}
	if ann, ok := meta["annotations"].(map[string]any); ok {
		for k, v := range ann {
			if strings.HasPrefix(k, "container.apparmor.security.beta.kubernetes.io/") && v != "runtime/default" {
				return fmt.Errorf("guard: %s/%s weakens AppArmor via annotation", kind, name)
			}
		}
	}
	if kind != "Pod" {
		return nil
	}
	spec, _ := obj["spec"].(map[string]any)
	return guardPod(name, spec)
}

func guardPod(name string, spec map[string]any) error {
	for _, f := range []string{"hostNetwork", "hostPID", "hostIPC", "shareProcessNamespace"} {
		if b, _ := spec[f].(bool); b {
			return fmt.Errorf("guard: pod %s sets %s", name, f)
		}
	}
	if b, ok := spec["hostUsers"].(bool); !ok || b {
		return fmt.Errorf("guard: pod %s must set hostUsers: false", name)
	}
	if _, ok := spec["nodeName"]; ok {
		return fmt.Errorf("guard: pod %s pins a node", name)
	}
	if err := guardSecurity(name, "pod", spec["securityContext"]); err != nil {
		return err
	}
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		for k := range vm {
			switch k {
			case "name", "emptyDir", "persistentVolumeClaim", "secret", "configMap":
			default:
				return fmt.Errorf("guard: pod %s volume source %q is not permitted", name, k)
			}
		}
	}
	for _, key := range []string{"containers", "initContainers"} {
		list, _ := spec[key].([]any)
		for _, c := range list {
			cm, _ := c.(map[string]any)
			if err := guardSecurity(name, "container "+fmt.Sprint(cm["name"]), cm["securityContext"]); err != nil {
				return err
			}
			if devs, ok := cm["volumeDevices"]; ok && devs != nil {
				return fmt.Errorf("guard: pod %s maps block devices", name)
			}
			image, _ := cm["image"].(string)
			if !strings.Contains(image, "@sha256:") {
				return fmt.Errorf("guard: pod %s image %q is not digest-pinned", name, image)
			}
		}
	}
	return nil
}

func guardSecurity(pod, where string, raw any) error {
	sc, _ := raw.(map[string]any)
	if sc == nil {
		return nil
	}
	if b, _ := sc["privileged"].(bool); b {
		return fmt.Errorf("guard: pod %s %s is privileged", pod, where)
	}
	if v, _ := sc["procMount"].(string); v != "" && v != "Default" {
		return fmt.Errorf("guard: pod %s %s changes procMount", pod, where)
	}
	for _, field := range []string{"seccompProfile", "appArmorProfile"} {
		if p, ok := sc[field].(map[string]any); ok {
			if t, _ := p["type"].(string); t != "RuntimeDefault" {
				return fmt.Errorf("guard: pod %s %s sets %s %q (only RuntimeDefault is permitted)", pod, where, field, t)
			}
		}
	}
	if caps, ok := sc["capabilities"].(map[string]any); ok {
		add, _ := caps["add"].([]any)
		var bad []string
		for _, c := range add {
			if s, _ := c.(string); !allowedAddCaps[s] {
				bad = append(bad, fmt.Sprint(c))
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			return fmt.Errorf("guard: pod %s %s adds capabilities %v", pod, where, bad)
		}
	}
	return nil
}

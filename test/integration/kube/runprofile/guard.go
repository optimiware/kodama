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

// The only Localhost profiles a probe Pod may reference: the installation
// installed seccomp and AppArmor profiles of the O-30 `step` role (seam S3).
// seccompProfile.localhostProfile is relative to the kubelet seccomp root.
const (
	StepSeccompProfile  = "kodama/kodama-rootless-build-v1.json"
	StepAppArmorProfile = "kodama-rootless-build-v1"
)

// allowedAddCaps are the only capabilities a probe Pod may add: the two the
// user-namespace mapping helpers (newuidmap/newgidmap) need. Every probe Pod
// is a user-namespaced Pod (hostUsers false). SYS_ADMIN and every other
// capability are refused for every Pod; there is no builder exception.
var allowedAddCaps = map[string]bool{"SETUID": true, "SETGID": true}

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
			if strings.HasPrefix(k, "container.apparmor.security.beta.kubernetes.io/") && v != "runtime/default" && v != "localhost/"+StepAppArmorProfile {
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
	podSC, _ := spec["securityContext"].(map[string]any)
	if err := guardSecurity(name, "pod", podSC, nil, false); err != nil {
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
			if err := guardSecurity(name, "container "+fmt.Sprint(cm["name"]), ctrSC(cm), podSC, true); err != nil {
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

func ctrSC(c map[string]any) map[string]any {
	sc, _ := c["securityContext"].(map[string]any)
	return sc
}

// effective returns the field a container effectively uses: its own value,
// else the Pod's.
func effective(field string, sc, podSC map[string]any) any {
	if v, ok := sc[field]; ok {
		return v
	}
	return podSC[field]
}

// profileAllowed accepts only RuntimeDefault or the exact installed Localhost
// profile for field ("seccompProfile" or "appArmorProfile"). Unconfined and
// every other Localhost name are refused.
func profileAllowed(field string, v any) (string, bool) {
	p, ok := v.(map[string]any)
	if !ok {
		return "", true
	}
	t, _ := p["type"].(string)
	switch t {
	case "RuntimeDefault":
		return t, true
	case "Localhost":
		want := StepSeccompProfile
		if field == "appArmorProfile" {
			want = StepAppArmorProfile
		}
		got, _ := p["localhostProfile"].(string)
		return t + " " + got, got == want
	}
	return t, false
}

func isLocalhostProfile(field string, v any) bool {
	p, _ := v.(map[string]any)
	if t, _ := p["type"].(string); t != "Localhost" {
		return false
	}
	_, ok := profileAllowed(field, v)
	return ok
}

// guardSecurity refuses any security shape beyond the O-30 step role. podSC
// is the enclosing Pod's context when sc belongs to a container. Every
// container, with or without added capabilities, must run as an explicit
// non-root user with `drop: [ALL]`: a user-namespaced Pod (hostUsers false is
// required for every probe Pod) never inherits runtime-default capabilities
// or root by omission.
func guardSecurity(pod, where string, sc, podSC map[string]any, container bool) error {
	if sc == nil {
		sc = map[string]any{}
	}
	if b, _ := sc["privileged"].(bool); b {
		return fmt.Errorf("guard: pod %s %s is privileged", pod, where)
	}
	for _, field := range []string{"seccompProfile", "appArmorProfile"} {
		if p, ok := sc[field]; ok {
			if desc, allowed := profileAllowed(field, p); !allowed {
				return fmt.Errorf("guard: pod %s %s sets %s %q (only RuntimeDefault or the installed %s profile is permitted)", pod, where, field, desc, "kodama-rootless-build-v1")
			}
		}
	}
	var added []string
	caps, _ := sc["capabilities"].(map[string]any)
	if caps != nil {
		add, _ := caps["add"].([]any)
		var bad []string
		for _, c := range add {
			s, _ := c.(string)
			added = append(added, s)
			if !allowedAddCaps[s] {
				bad = append(bad, fmt.Sprint(c))
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			return fmt.Errorf("guard: pod %s %s adds capabilities %v (only SETUID and SETGID are permitted)", pod, where, bad)
		}
	}
	if !container {
		return nil // the rules below need the container's effective Pod context
	}
	dropsAll := false
	for _, c := range anyList(caps["drop"]) {
		if c == "ALL" {
			dropsAll = true
		}
	}
	if !dropsAll {
		return fmt.Errorf("guard: pod %s %s does not drop ALL capabilities", pod, where)
	}
	if b, _ := effective("runAsNonRoot", sc, podSC).(bool); !b {
		return fmt.Errorf("guard: pod %s %s does not set runAsNonRoot true", pod, where)
	}
	if n, ok := asInt(effective("runAsUser", sc, podSC)); !ok || n == 0 {
		return fmt.Errorf("guard: pod %s %s does not run as an explicit non-zero user", pod, where)
	}
	if v, _ := sc["procMount"].(string); v != "" && v != "Default" {
		sort.Strings(added)
		stepShaped := v == "Unmasked" && strings.Join(added, ",") == "SETGID,SETUID" &&
			sc["allowPrivilegeEscalation"] == true &&
			isLocalhostProfile("seccompProfile", effective("seccompProfile", sc, podSC)) &&
			isLocalhostProfile("appArmorProfile", effective("appArmorProfile", sc, podSC))
		if !stepShaped {
			return fmt.Errorf("guard: pod %s %s changes procMount outside the complete step role shape", pod, where)
		}
	}
	return nil
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

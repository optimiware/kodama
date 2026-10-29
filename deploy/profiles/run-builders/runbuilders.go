// Package runbuilders ships the installation-owned node profiles that let ordinary run_shared
// step Pods run rootless Buildah within the O-30 ceiling (non-root, hostUsers false, only
// SETUID/SETGID, no SYS_ADMIN, never privileged): a seccomp profile and an AppArmor profile
// that, unlike the container runtime defaults, permit user-namespace creation and the nested
// mounts a user-namespaced builder needs (T-111 evidence).
//
// The package embeds the profile files, validates them and renders the installer manifests
// (a privileged DaemonSet in an installation-owned namespace; installation management, never
// tenant execution) deterministically. It performs no cluster calls. Rendered files are
// configuration, not qualification evidence: node installation and the Buildah probe belong to
// T-121.
package runbuilders

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Names the step-role policy (seam S3) and the Pod templates must use. Changing a profile's
// behavior requires a new version suffix and a new file name; the v1 names are never reused.
const (
	// ProfileVersion identifies both profiles.
	ProfileVersion = "kodama-rootless-build-v1"
	// SeccompFile is the file name under the kubelet seccomp root's kodama/ directory.
	SeccompFile = ProfileVersion + ".json"
	// SeccompLocalhostProfile is the securityContext.seccompProfile.localhostProfile value.
	SeccompLocalhostProfile = "kodama/" + SeccompFile
	// AppArmorProfile is the securityContext.appArmorProfile.localhostProfile value.
	AppArmorProfile = ProfileVersion
	// HostSeccompDir is the only host path the installer writes.
	HostSeccompDir = "/var/lib/kubelet/seccomp/kodama"
	// Namespace is the installation-owned installer namespace; it is never a tenant namespace.
	Namespace = "kodama-node-profiles"
	// Name names the installer DaemonSet, ConfigMap and ServiceAccount.
	Name = "kodama-node-profiles"
	// UninstallName names the uninstall DaemonSet.
	UninstallName = "kodama-node-profiles-uninstall"
	// DefaultInstallerImage is Ubuntu 24.04 (docker.io/library/ubuntu:24.04, the OS family of the
	// stage nodes) pinned by its multi-arch OCI index digest, resolved from the registry on
	// 2026-10-29. The image only supplies a POSIX shell, nsenter, cmp and sha256sum; the host's
	// apparmor_parser does the loading.
	DefaultInstallerImage = "docker.io/library/ubuntu@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852"
)

//go:embed seccomp/kodama-rootless-build-v1.json
var seccompProfile []byte

//go:embed apparmor/kodama-rootless-build-v1
var apparmorProfile []byte

//go:embed installer/install.sh
var installScript []byte

// Profiles are the exact bytes installed on the nodes.
type Profiles struct {
	Seccomp  []byte
	AppArmor []byte
}

// Embedded returns the profiles shipped with this package.
func Embedded() Profiles {
	return Profiles{Seccomp: bytes.Clone(seccompProfile), AppArmor: bytes.Clone(apparmorProfile)}
}

// InstallScript returns the installer script carried in the ConfigMap.
func InstallScript() []byte { return bytes.Clone(installScript) }

// Options customize rendering.
type Options struct {
	// Image overrides DefaultInstallerImage. It must be pinned by a sha256 digest.
	Image string
}

var imagePattern = regexp.MustCompile(`^[a-z0-9]+([./_:-][a-z0-9]+)*(:[A-Za-z0-9_.-]+)?@sha256:([0-9a-f]{64})$`)

// placeholderDigest reports digests made of one repeated character (for example all zeros), the
// shape used for "fill me in" constants.
func placeholderDigest(hexDigest string) bool {
	return strings.Trim(hexDigest, hexDigest[:1]) == ""
}

// ValidateImage requires a digest-pinned reference with a real (non-placeholder) digest.
func ValidateImage(image string) error {
	m := imagePattern.FindStringSubmatch(image)
	if m == nil {
		return fmt.Errorf("installer image %q must be pinned by a sha256 digest", image)
	}
	if placeholderDigest(m[3]) {
		return fmt.Errorf("installer image %q uses a placeholder digest; supply a verified digest", image)
	}
	return nil
}

// Render returns the installer manifests (Namespace, ServiceAccount, ConfigMap, DaemonSet) as one
// YAML stream. The output depends only on the embedded files and opts.
func Render(opts Options) ([]byte, error) {
	return render(opts, false)
}

// RenderUninstall returns the manifest of the uninstall DaemonSet, which removes the profiles
// from every node and relies on the Namespace, ServiceAccount and ConfigMap of Render.
func RenderUninstall(opts Options) ([]byte, error) {
	return render(opts, true)
}

func render(opts Options, uninstall bool) ([]byte, error) {
	image := opts.Image
	if image == "" {
		image = DefaultInstallerImage
	}
	if err := ValidateImage(image); err != nil {
		return nil, err
	}
	p := Embedded()
	if err := ValidateSeccomp(p.Seccomp); err != nil {
		return nil, err
	}
	if err := ValidateAppArmor(p.AppArmor); err != nil {
		return nil, err
	}
	var objects []map[string]any
	if uninstall {
		objects = []map[string]any{daemonSet(image, p, true)}
	} else {
		objects = []map[string]any{namespaceObject(), serviceAccount(), configMap(p), daemonSet(image, p, false)}
	}
	var out bytes.Buffer
	for i, object := range objects {
		if i > 0 {
			out.WriteString("---\n")
		}
		encoder := yaml.NewEncoder(&out)
		encoder.SetIndent(2)
		if err := encoder.Encode(object); err != nil {
			return nil, err
		}
		if err := encoder.Close(); err != nil {
			return nil, err
		}
	}
	if err := ValidateManifests(out.Bytes(), p); err != nil {
		return nil, fmt.Errorf("rendered manifests are invalid: %w", err)
	}
	return out.Bytes(), nil
}

func digest(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func labels(component string) map[string]any {
	return map[string]any{
		"app.kubernetes.io/name":      Name,
		"app.kubernetes.io/component": component,
		"app.kubernetes.io/part-of":   "kodama",
	}
}

func namespaceObject() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/part-of": "kodama",
				// The installer is privileged by design (installation management).
				"pod-security.kubernetes.io/enforce": "privileged",
				"pod-security.kubernetes.io/audit":   "privileged",
				"pod-security.kubernetes.io/warn":    "privileged",
			},
		},
	}
}

func serviceAccount() map[string]any {
	return map[string]any{
		"apiVersion":                   "v1",
		"kind":                         "ServiceAccount",
		"automountServiceAccountToken": false,
		"metadata":                     map[string]any{"name": Name, "namespace": Namespace, "labels": labels("node-profiles")},
	}
}

func configMap(p Profiles) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": Name, "namespace": Namespace, "labels": labels("node-profiles"),
			"annotations": map[string]any{"kodama.io/profile-version": ProfileVersion},
		},
		"data": map[string]any{
			SeccompFile:    string(p.Seccomp),
			ProfileVersion: string(p.AppArmor),
			"install.sh":   string(installScript),
		},
	}
}

func daemonSet(image string, p Profiles, uninstall bool) map[string]any {
	name, mode := Name, "install"
	if uninstall {
		name, mode = UninstallName, "uninstall"
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name": name, "namespace": Namespace, "labels": labels("node-profiles-" + mode),
			"annotations": map[string]any{"kodama.io/profile-version": ProfileVersion},
		},
		"spec": map[string]any{
			"selector":       map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": name}},
			"updateStrategy": map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": "10%"}},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{"app.kubernetes.io/name": name, "app.kubernetes.io/part-of": "kodama"},
					// A profile change rolls the DaemonSet.
					"annotations": map[string]any{"kodama.io/profiles-sha256": digest(p.Seccomp, p.AppArmor, installScript)},
				},
				"spec": map[string]any{
					"serviceAccountName":            Name,
					"automountServiceAccountToken":  false,
					"priorityClassName":             "system-node-critical",
					"hostPID":                       true, // nsenter into the host mount namespace (apparmor_parser)
					"terminationGracePeriodSeconds": 10,
					"nodeSelector":                  map[string]any{"kubernetes.io/os": "linux"},
					"tolerations":                   []any{map[string]any{"operator": "Exists"}},
					"containers": []any{map[string]any{
						"name":    "installer",
						"image":   image,
						"command": []any{"/bin/sh", "/scripts/install.sh"},
						"env":     []any{map[string]any{"name": "MODE", "value": mode}},
						"securityContext": map[string]any{
							"privileged": true, // installation management; see docs/operations/run-builder-profile.md
							"runAsUser":  0,
						},
						"resources": map[string]any{
							"requests": map[string]any{"cpu": "10m", "memory": "16Mi"},
							"limits":   map[string]any{"cpu": "100m", "memory": "64Mi"},
						},
						"readinessProbe": map[string]any{
							"exec":          map[string]any{"command": []any{"test", "-f", "/run/kodama-node-profiles/ready"}},
							"periodSeconds": 10,
						},
						"volumeMounts": []any{
							map[string]any{"name": "profiles", "mountPath": "/profiles", "readOnly": true},
							map[string]any{"name": "scripts", "mountPath": "/scripts", "readOnly": true},
							map[string]any{"name": "host-seccomp", "mountPath": "/host-seccomp"},
						},
					}},
					"volumes": []any{
						map[string]any{"name": "profiles", "configMap": map[string]any{
							"name": Name,
							"items": []any{
								map[string]any{"key": SeccompFile, "path": SeccompFile},
								map[string]any{"key": ProfileVersion, "path": ProfileVersion},
							},
						}},
						map[string]any{"name": "scripts", "configMap": map[string]any{
							"name":        Name,
							"defaultMode": 0o555,
							"items":       []any{map[string]any{"key": "install.sh", "path": "install.sh"}},
						}},
						map[string]any{"name": "host-seccomp", "hostPath": map[string]any{"path": HostSeccompDir, "type": "DirectoryOrCreate"}},
					},
				},
			},
		},
	}
}

// seccompProfile is the OCI runtime-spec LinuxSeccomp document the CRI loads for a Localhost
// profile. Unknown fields are rejected so a docker-style gate (includes/excludes) cannot hide.
type seccompDocument struct {
	DefaultAction string   `json:"defaultAction"`
	Architectures []string `json:"architectures"`
	Syscalls      []struct {
		Names    []string `json:"names"`
		Action   string   `json:"action"`
		ErrnoRet *uint    `json:"errnoRet,omitempty"`
		Args     []struct {
			Index    uint   `json:"index"`
			Value    uint64 `json:"value"`
			ValueTwo uint64 `json:"valueTwo,omitempty"`
			Op       string `json:"op"`
		} `json:"args,omitempty"`
	} `json:"syscalls"`
}

// RequiredUnconditional are the syscalls a user-namespaced rootless builder needs without any
// capability gate. The container runtime default allows them only with CAP_SYS_ADMIN (and
// chroot/sethostname only with other capabilities), which the O-30 ceiling excludes.
var RequiredUnconditional = []string{
	"clone", "clone3", "unshare", "setns", "mount", "umount", "umount2", "pivot_root",
	"mount_setattr", "open_tree", "move_mount", "fsopen", "fsmount", "fsconfig", "fspick",
	"keyctl", "chroot", "sethostname", "setdomainname",
}

// ForbiddenSyscalls must never be allowed in any form: kernel modules, kexec, reboot, BPF and
// perf, swap, clock changes, accounting, raw I/O and similar host operations.
var ForbiddenSyscalls = []string{
	"init_module", "finit_module", "delete_module", "create_module", "query_module", "get_kernel_syms",
	"kexec_load", "kexec_file_load", "reboot", "bpf", "perf_event_open", "swapon", "swapoff",
	"settimeofday", "stime", "clock_settime", "clock_settime64", "acct", "iopl",
	"ioperm", "quotactl", "quotactl_fd", "syslog", "lookup_dcookie", "fanotify_init",
	"open_by_handle_at", "userfaultfd", "io_uring_setup", "io_uring_enter", "io_uring_register",
	"pidfd_getfd", "kcmp", "process_madvise", "add_key", "request_key", "nfsservctl", "_sysctl",
	"uselib", "ustat", "sysfs", "vhangup", "mbind", "set_mempolicy", "get_mempolicy",
	"move_pages", "migrate_pages",
}

// ValidateSeccomp checks the seccomp profile's invariants.
func ValidateSeccomp(data []byte) error {
	var doc seccompDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return fmt.Errorf("seccomp profile: %w", err)
	}
	if doc.DefaultAction != "SCMP_ACT_ERRNO" {
		return fmt.Errorf("seccomp profile: defaultAction %q must be SCMP_ACT_ERRNO", doc.DefaultAction)
	}
	unconditional := map[string]bool{}
	allowed := map[string]bool{}
	for _, rule := range doc.Syscalls {
		switch rule.Action {
		case "SCMP_ACT_ALLOW":
			for _, name := range rule.Names {
				allowed[name] = true
				if len(rule.Args) == 0 {
					unconditional[name] = true
				}
			}
		case "SCMP_ACT_ERRNO", "SCMP_ACT_KILL", "SCMP_ACT_KILL_PROCESS", "SCMP_ACT_KILL_THREAD":
			for _, name := range rule.Names {
				for _, need := range RequiredUnconditional {
					if name == need {
						return fmt.Errorf("seccomp profile: %s must not be overridden by action %s", name, rule.Action)
					}
				}
			}
		default:
			return fmt.Errorf("seccomp profile: unsupported action %q", rule.Action)
		}
	}
	for _, need := range RequiredUnconditional {
		if !unconditional[need] {
			return fmt.Errorf("seccomp profile: %s must be allowed unconditionally", need)
		}
	}
	var present []string
	for _, name := range ForbiddenSyscalls {
		if allowed[name] {
			present = append(present, name)
		}
	}
	if len(present) > 0 {
		sort.Strings(present)
		return fmt.Errorf("seccomp profile: forbidden syscalls allowed: %s", strings.Join(present, ", "))
	}
	return nil
}

// AppArmorDenials are the runtime default's denials of host /proc, /sys and kcore access that
// the Kodama profile keeps verbatim.
var AppArmorDenials = []string{
	"deny @{PROC}/* w,",
	"deny @{PROC}/{[^1-9/],[^1-9/][^0-9/],[^1-9s/][^0-9y/][^0-9s/],[^1-9/][^0-9/][^0-9/][^0-9/]*}/** w,",
	"deny @{PROC}/sys/[^k]** w,",
	"deny @{PROC}/sys/kernel/{?,??,[^s][^h][^m]**} w,",
	"deny @{PROC}/sysrq-trigger rwklx,",
	"deny @{PROC}/kcore rwklx,",
	"deny /sys/[^f]*/** wklx,",
	"deny /sys/f[^s]*/** wklx,",
	"deny /sys/fs/[^c]*/** wklx,",
	"deny /sys/fs/c[^g]*/** wklx,",
	"deny /sys/fs/cg[^r]*/** wklx,",
	"deny /sys/firmware/** rwklx,",
	"deny /sys/devices/virtual/powercap/** rwklx,",
	"deny /sys/kernel/security/** rwklx,",
}

// AppArmorRequiredRules are the rules a user-namespaced builder needs.
var AppArmorRequiredRules = []string{"userns,", "mount,", "remount,", "umount,", "pivot_root,"}

// appArmorRules returns the profile's non-comment lines, trimmed and with trailing comments cut.
func appArmorRules(data []byte) []string {
	var rules []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#include") {
			if line != "" {
				rules = append(rules, line)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "  #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		rules = append(rules, line)
	}
	return rules
}

// ValidateAppArmor checks the AppArmor profile's invariants.
func ValidateAppArmor(data []byte) error {
	rules := appArmorRules(data)
	has := map[string]bool{}
	for _, rule := range rules {
		has[rule] = true
	}
	if !has["abi <abi/4.0>,"] {
		return errors.New("apparmor profile: abi <abi/4.0>, is required for the userns rule")
	}
	if !has["profile "+AppArmorProfile+" flags=(attach_disconnected,mediate_deleted) {"] {
		return fmt.Errorf("apparmor profile: profile %s with attach_disconnected,mediate_deleted is required", AppArmorProfile)
	}
	for _, rule := range AppArmorRequiredRules {
		if !has[rule] {
			return fmt.Errorf("apparmor profile: missing rule %q", rule)
		}
	}
	for _, rule := range AppArmorDenials {
		if !has[rule] {
			return fmt.Errorf("apparmor profile: missing default denial %q", rule)
		}
	}
	for _, rule := range rules {
		switch {
		case rule == "deny mount,":
			return errors.New("apparmor profile: the default `deny mount,` must be replaced by explicit mount rules")
		case strings.Contains(rule, "unconfined") && !strings.HasPrefix(rule, "signal"):
			return fmt.Errorf("apparmor profile: %q must not grant unconfined behavior", rule)
		case strings.HasPrefix(rule, "allow ") || strings.HasPrefix(rule, "audit allow "):
			return fmt.Errorf("apparmor profile: explicit allow rule %q is not part of the default shape", rule)
		case strings.HasPrefix(rule, "capability ") || strings.HasPrefix(rule, "deny capability"):
			return fmt.Errorf("apparmor profile: capability rule %q changes the default shape", rule)
		}
	}
	return nil
}

package runbuilders

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func mustRender(t *testing.T, opts Options) []byte {
	t.Helper()
	out, err := Render(opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

func decodeStream(t *testing.T, stream []byte) []object {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(stream))
	var docs []object
	for {
		var doc object
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		docs = append(docs, doc)
	}
	return docs
}

func Test_unit_builder_profile_render(t *testing.T) {
	first, second := mustRender(t, Options{}), mustRender(t, Options{})
	if !bytes.Equal(first, second) {
		t.Fatal("rendering is not deterministic")
	}
	// The committed installer manifests are exactly what the renderer produces.
	for file, render := range map[string]func(Options) ([]byte, error){
		"installer/node-profiles.yaml":           Render,
		"installer/node-profiles-uninstall.yaml": RenderUninstall,
	} {
		committed, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		want, err := render(Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(committed, want) {
			t.Errorf("%s is stale; regenerate with go run ./deploy/profiles/run-builders/cmd/render", file)
		}
	}

	docs := decodeStream(t, first)
	kinds := []string{}
	for _, doc := range docs {
		kinds = append(kinds, text(doc, "kind"))
	}
	if strings.Join(kinds, ",") != "Namespace,ServiceAccount,ConfigMap,DaemonSet" {
		t.Fatalf("kinds = %v", kinds)
	}
	for _, doc := range docs[1:] {
		if got := text(child(doc, "metadata"), "namespace"); got != Namespace || strings.Contains(got, "tenant") {
			t.Fatalf("%s namespace %q", text(doc, "kind"), got)
		}
	}
	// The ConfigMap carries the exact profile bytes and the script.
	data := child(docs[2], "data")
	p := Embedded()
	if text(data, SeccompFile) != string(p.Seccomp) || text(data, ProfileVersion) != string(p.AppArmor) || text(data, "install.sh") != string(InstallScript()) {
		t.Fatal("ConfigMap content differs from the shipped files")
	}
	// The embedded files are also the files on disk.
	for file, want := range map[string][]byte{"seccomp/" + SeccompFile: p.Seccomp, "apparmor/" + ProfileVersion: p.AppArmor} {
		onDisk, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(onDisk, want) {
			t.Fatalf("%s: %v", file, err)
		}
	}

	spec := child(child(child(docs[3], "spec"), "template"), "spec")
	container := list(spec, "containers")[0].(object)
	if text(container, "image") != DefaultInstallerImage || !strings.Contains(DefaultInstallerImage, "@sha256:") {
		t.Fatalf("image %q", text(container, "image"))
	}
	if spec["hostNetwork"] != nil || spec["hostIPC"] != nil || spec["automountServiceAccountToken"] != false {
		t.Fatalf("unexpected host settings: %v", spec)
	}
	hostPaths := []string{}
	for _, volume := range list(spec, "volumes") {
		if host := child(volume.(object), "hostPath"); host != nil {
			hostPaths = append(hostPaths, text(host, "path"))
		}
	}
	if strings.Join(hostPaths, ",") != HostSeccompDir {
		t.Fatalf("hostPaths = %v", hostPaths)
	}

	// An explicit digest-pinned override renders; anything else is refused.
	override := "registry.example.com/mirror/ubuntu@sha256:" + strings.Repeat("ab12", 16)
	if !strings.Contains(string(mustRender(t, Options{Image: override})), override) {
		t.Fatal("override image missing from output")
	}
	for _, image := range []string{
		"docker.io/library/ubuntu:24.04",
		"docker.io/library/ubuntu",
		"docker.io/library/ubuntu@sha256:" + strings.Repeat("0", 64),
		"docker.io/library/ubuntu@sha256:" + strings.Repeat("f", 64),
		"docker.io/library/ubuntu@sha256:abc",
	} {
		if _, err := Render(Options{Image: image}); err == nil {
			t.Errorf("image %q was rendered", image)
		}
	}

	// The uninstall DaemonSet reuses the installer ConfigMap and runs in uninstall mode.
	uninstall := decodeStream(t, mustUninstall(t))
	if len(uninstall) != 1 || text(uninstall[0], "kind") != "DaemonSet" || text(child(uninstall[0], "metadata"), "name") != UninstallName {
		t.Fatalf("uninstall manifest = %v", uninstall)
	}
	if !strings.Contains(string(mustUninstall(t)), "value: uninstall") {
		t.Fatal("uninstall DaemonSet does not run in uninstall mode")
	}
}

func mustUninstall(t *testing.T) []byte {
	t.Helper()
	out, err := RenderUninstall(Options{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- manifest validation failures ---------------------------------------------------------

func mutate(t *testing.T, change func(docs []object)) []byte {
	t.Helper()
	docs := decodeStream(t, mustRender(t, Options{}))
	change(docs)
	var out bytes.Buffer
	for i, doc := range docs {
		if i > 0 {
			out.WriteString("---\n")
		}
		encoded, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(encoded)
	}
	return out.Bytes()
}

func podSpec(docs []object) object { return child(child(child(docs[3], "spec"), "template"), "spec") }

func Test_unit_builder_profile_denials(t *testing.T) {
	// The shipped profile denies every forbidden host operation: not allowed in any form.
	var doc seccompDocument
	if err := json.Unmarshal(Embedded().Seccomp, &doc); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, rule := range doc.Syscalls {
		if rule.Action == "SCMP_ACT_ALLOW" {
			for _, name := range rule.Names {
				allowed[name] = true
			}
		}
	}
	for _, name := range ForbiddenSyscalls {
		if allowed[name] {
			t.Errorf("%s is allowed", name)
		}
	}
	if len(ForbiddenSyscalls) < 40 {
		t.Fatal("forbidden list shrank")
	}
	for _, name := range []string{"kexec_load", "reboot", "bpf", "perf_event_open", "init_module", "swapon", "settimeofday", "acct"} {
		found := false
		for _, forbidden := range ForbiddenSyscalls {
			found = found || forbidden == name
		}
		if !found || allowed[name] {
			t.Errorf("%s must be in the forbidden list and denied", name)
		}
	}
	if doc.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatalf("default action %q", doc.DefaultAction)
	}

	// The profile equals the containerd default plus exactly the documented deviations.
	raw, err := os.ReadFile("testdata/containerd-v2.3.6-default-syscalls.txt")
	if err != nil {
		t.Fatal(err)
	}
	defaults := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			defaults[line] = true
		}
	}
	var extra []string
	for name := range allowed {
		if !defaults[name] {
			extra = append(extra, name)
		}
	}
	want := append([]string{}, RequiredUnconditional...)
	sort.Strings(extra)
	sort.Strings(want)
	// clone is partly allowed by the default (without namespace flags), so it is not "extra"
	// relative to the capability-free default list only if listed there; it is not.
	if strings.Join(extra, ",") != strings.Join(want, ",") {
		t.Fatalf("deviations from the default:\n got  %v\n want %v", extra, want)
	}
	for name := range defaults {
		if !allowed[name] {
			t.Errorf("default-allowed syscall %s was dropped", name)
		}
	}

	// Validators reject weakened profiles.
	weaken := func(edit func(d map[string]any)) error {
		var raw map[string]any
		if err := json.Unmarshal(Embedded().Seccomp, &raw); err != nil {
			t.Fatal(err)
		}
		edit(raw)
		out, _ := json.Marshal(raw)
		return ValidateSeccomp(out)
	}
	appendRule := func(d map[string]any, rule map[string]any) {
		d["syscalls"] = append(d["syscalls"].([]any), rule)
	}
	for name, edit := range map[string]func(map[string]any){
		"module loading allowed": func(d map[string]any) {
			appendRule(d, map[string]any{"names": []any{"init_module"}, "action": "SCMP_ACT_ALLOW"})
		},
		"bpf allowed with args": func(d map[string]any) {
			appendRule(d, map[string]any{"names": []any{"bpf"}, "action": "SCMP_ACT_ALLOW", "args": []any{map[string]any{"index": 0, "value": 0, "op": "SCMP_CMP_EQ"}}})
		},
		"clone3 overridden by ENOSYS": func(d map[string]any) {
			appendRule(d, map[string]any{"names": []any{"clone3"}, "action": "SCMP_ACT_ERRNO", "errnoRet": 38})
		},
		"default allow": func(d map[string]any) { d["defaultAction"] = "SCMP_ACT_ALLOW" },
		"docker-style gate": func(d map[string]any) {
			appendRule(d, map[string]any{"names": []any{"x"}, "action": "SCMP_ACT_ALLOW", "includes": map[string]any{"caps": []any{"CAP_SYS_ADMIN"}}})
		},
	} {
		if err := weaken(edit); err == nil {
			t.Errorf("%s: validation passed", name)
		}
	}

	// The AppArmor profile keeps every default denial and does not allow host writes back.
	rules := Embedded().AppArmor
	for _, denial := range AppArmorDenials {
		if !strings.Contains(string(rules), "  "+denial) {
			t.Errorf("missing denial %q", denial)
		}
	}

	// Manifest validation failures.
	cases := map[string]func(docs []object){
		"tenant namespace": func(docs []object) { child(docs[3], "metadata")["namespace"] = "kodama-tenant-acme" },
		"tag only image": func(docs []object) {
			list(podSpec(docs), "containers")[0].(object)["image"] = "docker.io/library/ubuntu:24.04"
		},
		"extra hostPath": func(docs []object) {
			volumes := list(podSpec(docs), "volumes")
			podSpec(docs)["volumes"] = append(volumes, object{"name": "root", "hostPath": object{"path": "/"}})
		},
		"securityfs hostPath": func(docs []object) {
			podSpec(docs)["volumes"] = append(list(podSpec(docs), "volumes"), object{"name": "sec", "hostPath": object{"path": "/sys/kernel/security"}})
		},
		"hostNetwork": func(docs []object) { podSpec(docs)["hostNetwork"] = true },
		"token mounted": func(docs []object) {
			podSpec(docs)["automountServiceAccountToken"] = true
		},
		"extra kind": func(docs []object) {
			docs[1]["kind"] = "ClusterRoleBinding"
		},
		"tampered profile": func(docs []object) {
			child(docs[2], "data")[SeccompFile] = "{}"
		},
		"namespace not privileged-labelled": func(docs []object) {
			delete(child(child(docs[0], "metadata"), "labels"), "pod-security.kubernetes.io/enforce")
		},
		"foreign namespace": func(docs []object) { docs[0]["metadata"].(object)["name"] = "kodama-system" },
	}
	for name, change := range cases {
		if err := ValidateManifests(mutate(t, change), Embedded()); err == nil {
			t.Errorf("%s: validation passed", name)
		}
	}
	if err := ValidateManifests(mutate(t, func([]object) {}), Embedded()); err != nil {
		t.Fatalf("control mutation failed: %v", err)
	}
}

// --- seccomp behavior -----------------------------------------------------------------------

// seccompAllows evaluates the profile like libseccomp: a syscall is allowed when a rule with
// action ALLOW names it and all of the rule's argument conditions hold; otherwise defaultAction
// (ERRNO) applies. The runtime-spec profile has no capability input at all, so the result is
// the same for a container with no capabilities as for one holding SYS_ADMIN.
func seccompAllows(doc seccompDocument, name string, args ...uint64) bool {
	for _, rule := range doc.Syscalls {
		named := false
		for _, n := range rule.Names {
			named = named || n == name
		}
		if !named || rule.Action != "SCMP_ACT_ALLOW" {
			continue
		}
		ok := true
		for _, cond := range rule.Args {
			var arg uint64
			if int(cond.Index) < len(args) {
				arg = args[cond.Index]
			}
			switch cond.Op {
			case "SCMP_CMP_EQ":
				ok = ok && arg == cond.Value
			case "SCMP_CMP_LT":
				ok = ok && arg < cond.Value
			case "SCMP_CMP_GT":
				ok = ok && arg > cond.Value
			case "SCMP_CMP_MASKED_EQ":
				ok = ok && arg&cond.Value == cond.ValueTwo
			default:
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func Test_unit_builder_profile_seccomp_allows_userns_without_sys_admin(t *testing.T) {
	if err := ValidateSeccomp(Embedded().Seccomp); err != nil {
		t.Fatal(err)
	}
	var doc seccompDocument
	decoder := json.NewDecoder(bytes.NewReader(Embedded().Seccomp))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	const (
		cloneNewNS   = 0x00020000
		cloneNewUser = 0x10000000
		cloneNewPID  = 0x20000000
		cloneNewNet  = 0x40000000
	)
	// Every required syscall is allowed whatever its arguments, including namespace flags, and no
	// rule gates it on a capability (the schema has no capability field; unknown fields are rejected).
	for _, name := range RequiredUnconditional {
		for _, args := range [][]uint64{{}, {0}, {cloneNewUser | cloneNewNS | cloneNewPID | cloneNewNet, 0xdead}} {
			if !seccompAllows(doc, name, args...) {
				t.Errorf("%s%v is not allowed", name, args)
			}
		}
	}
	// What the runtime default does for the same container (no SYS_ADMIN): namespace-creating
	// clone and clone3 are refused. Documented as the T-111 denial this profile removes.
	if !seccompAllows(doc, "clone", cloneNewUser|cloneNewNS) {
		t.Fatal("clone with CLONE_NEWUSER|CLONE_NEWNS must be allowed")
	}
	// Sanity of the unchanged parts of the default: sockets, personality and ordinary syscalls.
	if !seccompAllows(doc, "socket", 1) || !seccompAllows(doc, "socket", 2) || !seccompAllows(doc, "socket", 10) {
		t.Fatal("AF_UNIX/INET/INET6 sockets must be allowed")
	}
	if seccompAllows(doc, "socket", 40) || seccompAllows(doc, "socket", 38) {
		t.Fatal("AF_VSOCK and AF_ALG sockets must stay denied")
	}
	if !seccompAllows(doc, "personality", 0) || seccompAllows(doc, "personality", 0x0004) {
		t.Fatal("personality filtering must be unchanged")
	}
	for _, name := range []string{"read", "write", "execve", "openat", "setuid", "setresgid", "setgroups", "capset", "prctl", "ptrace"} {
		if !seccompAllows(doc, name) {
			t.Errorf("%s must stay allowed as in the default", name)
		}
	}
	for _, name := range ForbiddenSyscalls {
		if seccompAllows(doc, name) {
			t.Errorf("%s must stay denied", name)
		}
	}
	// The pinned architectures cover the stage worker CPUs.
	arches := strings.Join(doc.Architectures, ",")
	for _, arch := range []string{"SCMP_ARCH_X86_64", "SCMP_ARCH_AARCH64"} {
		if !strings.Contains(arches, arch) {
			t.Errorf("architecture %s missing", arch)
		}
	}
}

func Test_unit_builder_profile_apparmor_rules(t *testing.T) {
	data := Embedded().AppArmor
	if err := ValidateAppArmor(data); err != nil {
		t.Fatal(err)
	}
	rules := appArmorRules(data)
	has := func(rule string) bool {
		for _, r := range rules {
			if r == rule {
				return true
			}
		}
		return false
	}
	for _, rule := range []string{
		"abi <abi/4.0>,", "userns,", "mount,", "remount,", "umount,", "pivot_root,", "capability,", "network,", "file,",
		"signal (send,receive) peer=" + AppArmorProfile + ",",
	} {
		if !has(rule) {
			t.Errorf("missing rule %q", rule)
		}
	}
	for _, denial := range AppArmorDenials {
		if !has(denial) {
			t.Errorf("missing denial %q", denial)
		}
	}
	if has("deny mount,") || has("flags=(unconfined)") {
		t.Fatal("profile must not deny mounts or run unconfined")
	}
	for _, rule := range rules {
		if strings.Contains(rule, "unconfined") && !strings.HasPrefix(rule, "signal (receive)") {
			t.Errorf("unexpected unconfined reference %q", rule)
		}
	}

	// Each mutation the validator must refuse.
	for name, edit := range map[string]func(string) string{
		"no userns":        func(s string) string { return strings.Replace(s, "  userns,\n", "", 1) },
		"deny mount":       func(s string) string { return strings.Replace(s, "  mount,\n", "  deny mount,\n", 1) },
		"no pivot_root":    func(s string) string { return strings.Replace(s, "  pivot_root,\n", "", 1) },
		"abi 3.0":          func(s string) string { return strings.Replace(s, "abi <abi/4.0>,\n", "abi <abi/3.0>,\n", 1) },
		"proc/sys allowed": func(s string) string { return strings.Replace(s, "  deny @{PROC}/sys/[^k]** w,", "  # removed", 1) },
		"kcore allowed":    func(s string) string { return strings.Replace(s, "  deny @{PROC}/kcore rwklx,\n", "", 1) },
		"firmware":         func(s string) string { return strings.Replace(s, "  deny /sys/firmware/** rwklx,\n", "", 1) },
		"wrong name":       func(s string) string { return strings.Replace(s, "profile "+AppArmorProfile, "profile other", 1) },
		"allow rule": func(s string) string {
			return strings.Replace(s, "  userns,\n", "  userns,\n  allow /proc/sys/** w,\n", 1)
		},
	} {
		changed := edit(string(data))
		if changed == string(data) {
			t.Fatalf("%s: edit did not apply", name)
		}
		if err := ValidateAppArmor([]byte(changed)); err == nil {
			t.Errorf("%s: validation passed", name)
		}
	}

	// When the parser is installed (Ubuntu 24.04 apparmor 4.x), compile without loading.
	if parser, err := exec.LookPath("apparmor_parser"); err == nil {
		out, err := exec.Command(parser, "-QK", "-S", filepath.Join("apparmor", ProfileVersion)).CombinedOutput()
		if err != nil {
			t.Fatalf("apparmor_parser: %v\n%s", err, out)
		}
	} else if os.Getenv("KODAMA_REQUIRE_APPARMOR_PARSER") == "1" {
		t.Fatal("apparmor_parser is required but missing")
	}
}

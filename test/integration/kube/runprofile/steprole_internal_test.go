package runprofile

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func stepPodForTest(t *testing.T, unmasked bool) map[string]any {
	t.Helper()
	q := &Qualifier{c: &Cluster{RunID: "abc"}, nsMain: NamespacePrefix + "abc"}
	p := q.stepPod("b", "quay.io/buildah/stable@sha256:"+strings.Repeat("a", 64), unmasked, false)
	data, err := json.Marshal(q.c.PodObject(p))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func Test_unit_runprofile_step_role_shape(t *testing.T) {
	for _, unmasked := range []bool{false, true} {
		obj := stepPodForTest(t, unmasked)
		if err := Guard(obj, "abc"); err != nil {
			t.Fatalf("unmasked=%t: guard refused the step role: %v", unmasked, err)
		}
		if bad := StepRoleSpecViolations(obj, unmasked); len(bad) != 0 {
			t.Errorf("unmasked=%t: rendered Pod deviates from S3: %v", unmasked, bad)
		}
		spec := obj["spec"].(map[string]any)
		psc := spec["securityContext"].(map[string]any)
		c := spec["containers"].([]any)[0].(map[string]any)
		csc := c["securityContext"].(map[string]any)
		caps := csc["capabilities"].(map[string]any)
		checks := map[string]bool{
			"hostUsers false":          spec["hostUsers"] == false,
			"runAsUser 1000":           psc["runAsUser"] == float64(1000) && psc["runAsGroup"] == float64(1000),
			"runAsNonRoot":             psc["runAsNonRoot"] == true,
			"seccomp Localhost":        fmt.Sprint(psc["seccompProfile"]) == "map[localhostProfile:kodama/kodama-rootless-build-v1.json type:Localhost]",
			"apparmor Localhost":       fmt.Sprint(psc["appArmorProfile"]) == "map[localhostProfile:kodama-rootless-build-v1 type:Localhost]",
			"drop ALL":                 fmt.Sprint(caps["drop"]) == "[ALL]",
			"add SETUID SETGID":        fmt.Sprint(caps["add"]) == "[SETUID SETGID]",
			"allowPrivilegeEscalation": csc["allowPrivilegeEscalation"] == true,
			"not privileged":           csc["privileged"] == nil,
			"no host namespaces":       spec["hostNetwork"] == nil && spec["hostPID"] == nil && spec["hostIPC"] == nil,
			"procMount only unmasked":  (csc["procMount"] == "Unmasked") == unmasked,
		}
		for name, ok := range checks {
			if !ok {
				t.Errorf("unmasked=%t: %s not rendered: %v", unmasked, name, obj)
			}
		}
		for _, v := range spec["volumes"].([]any) {
			if _, host := v.(map[string]any)["hostPath"]; host {
				t.Error("step Pod mounts a hostPath")
			}
		}
	}
	// ko is an ordinary step too: it renders the identical S3 role (no
	// restricted-compatible exception), and a restricted-compatible Pod is
	// reported as deviating from it.
	q := &Qualifier{c: &Cluster{RunID: "abc"}, nsMain: NamespacePrefix + "abc"}
	koDigest := "ghcr.io/ko-build/ko@sha256:" + strings.Repeat("a", 64)
	ko := q.c.PodObject(q.stepPod("ko", koDigest, false, false))
	if err := Guard(ko, "abc"); err != nil {
		t.Fatalf("guard refused the ko step role: %v", err)
	}
	if bad := StepRoleSpecViolations(ko, false); len(bad) != 0 {
		t.Errorf("ko Pod deviates from S3: %v", bad)
	}
	kc := podSC(ko)
	if kc["allowPrivilegeEscalation"] != true || fmt.Sprint(kc["capabilities"]) != "map[add:[SETUID SETGID] drop:[ALL]]" || ko["spec"].(map[string]any)["hostUsers"] != false {
		t.Errorf("ko Pod is not the step role: %v", kc)
	}
	restricted := q.c.PodObject(q.builderPod("ko", koDigest, 65532))
	if len(StepRoleSpecViolations(restricted, false)) == 0 {
		t.Error("a restricted-compatible Pod was accepted as the step role")
	}
	src, err := os.ReadFile("phase_ko.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "builderPod(\"ko\"") || strings.Contains(string(src), "65532") {
		t.Error("phase_ko.go still builds a restricted-compatible ko Pod")
	}
	if !strings.Contains(string(src), "stepPod(\"ko\"") {
		t.Error("phase_ko.go does not run ko in the step role")
	}

	// Every deviation from the role is reported.
	mutations := map[string]func(map[string]any){
		"SYS_ADMIN added": func(o map[string]any) {
			podCaps(o)["add"] = []any{"SETUID", "SETGID", "SYS_ADMIN"}
		},
		"no capabilities added": func(o map[string]any) { delete(podCaps(o), "add") },
		"capabilities not dropped": func(o map[string]any) {
			podCaps(o)["drop"] = []any{"NET_RAW"}
		},
		"hostUsers true": func(o map[string]any) { o["spec"].(map[string]any)["hostUsers"] = true },
		"root": func(o map[string]any) {
			o["spec"].(map[string]any)["securityContext"].(map[string]any)["runAsUser"] = 0
		},
		"no privilege escalation": func(o map[string]any) { podSC(o)["allowPrivilegeEscalation"] = false },
		"runtime default seccomp": func(o map[string]any) {
			o["spec"].(map[string]any)["securityContext"].(map[string]any)["seccompProfile"] = map[string]any{"type": "RuntimeDefault"}
		},
		"default apparmor": func(o map[string]any) {
			delete(o["spec"].(map[string]any)["securityContext"].(map[string]any), "appArmorProfile")
		},
		"unmasked without being requested": func(o map[string]any) { podSC(o)["procMount"] = "Unmasked" },
		"privileged":                       func(o map[string]any) { podSC(o)["privileged"] = true },
		"hostNetwork":                      func(o map[string]any) { o["spec"].(map[string]any)["hostNetwork"] = true },
	}
	for name, f := range mutations {
		obj := stepPodForTest(t, false)
		f(obj)
		if len(StepRoleSpecViolations(obj, false)) == 0 {
			t.Errorf("%s: deviation not reported", name)
		}
	}
	// The unmasked variant must carry procMount; the default must not.
	if len(StepRoleSpecViolations(stepPodForTest(t, false), true)) == 0 {
		t.Error("default role accepted where Unmasked was required")
	}

	// In-Pod view: the exact bounding set, escalation permitted, installed profiles.
	good := ParseStepView(strings.Join([]string{"uid=1000", "gid=1000", "user=build", "cap_bnd=00000000000000c0", "no_new_privs=0", "seccomp=2",
		"apparmor=kodama-rootless-build-v1 (enforce)", "uid_map=0 1182859264 65536", "subuid_line=build:1:999", "subuid_line=build:1001:64535",
		"subgid_line=build:1:999", "subgid_line=build:1001:64535"}, "\n"))
	if bad := StepViewViolations(good, "build"); len(bad) != 0 {
		t.Fatalf("good view rejected: %v", bad)
	}
	for key, val := range map[string]string{"uid": "0", "user": "root", "cap_bnd": "00000000000000cc", "no_new_privs": "1", "seccomp": "0",
		"apparmor": "cri-containerd.apparmor.d (enforce)", "uid_map": "0 0 4294967295"} {
		v := ParseStepView("")
		for k, x := range good.KV {
			v.KV[k] = x
		}
		v.KV[key] = val
		if len(StepViewViolations(v, "build")) == 0 {
			t.Errorf("view with %s=%q accepted", key, val)
		}
	}
	if err := SubIDFit(good.SubUID, "build", StepUID, 60000); err != nil {
		t.Errorf("stock-image layout rejected: %v", err)
	}
	if err := stepOwnersFit(65535); err != nil {
		t.Error(err)
	}
}

func podSC(o map[string]any) map[string]any {
	return o["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)
}

func podCaps(o map[string]any) map[string]any { return podSC(o)["capabilities"].(map[string]any) }

func Test_unit_runprofile_subid_ranges(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		ok      bool
	}{
		{"stock layout", []string{"build:1:999", "build:1001:64535"}, true},
		{"numeric user", []string{"1000:1:999", "1000:1001:64535"}, true},
		{"beyond the Pod range", []string{"build:100000:65536"}, false},
		{"ends past 65535", []string{"build:1001:64536"}, false},
		{"overlaps own id", []string{"build:1:65535"}, false},
		{"too small", []string{"build:1:999"}, false},
		{"other user only", []string{"root:1:65000"}, false},
		{"overlapping entries", []string{"build:1:999", "build:500:60000"}, false},
		{"malformed", []string{"build:x:y"}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		err := SubIDFit(c.entries, "build", StepUID, 60000)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%t", c.name, err, c.ok)
		}
	}
	// Every helper owner fits the stock layout's usable IDs, and a 65,536 owner would not.
	if err := stepOwnersFit(65535); err != nil {
		t.Error(err)
	}
	if stepOwnersFit(60000) == nil {
		t.Error("an owner above the usable range went unnoticed")
	}
	for _, o := range stepOwners {
		if o.UID > 65534 || o.GID > 65534 {
			t.Errorf("owner %s outside the Pod range", o.Name)
		}
	}
	// The fallback mapping fits and skips the user's own ID.
	if err := SubIDFit(strings.Split(strings.TrimSpace(fallbackSubID), "\n"), "build", StepUID, 60000); err != nil {
		t.Errorf("fallback mapping does not fit: %v", err)
	}
}

func Test_unit_runprofile_buildah_probe_inputs(t *testing.T) {
	src := helperSource()
	for _, o := range stepOwners {
		if !strings.Contains(src, fmt.Sprintf("{%q, %d, %d}", o.Name, o.UID, o.GID)) {
			t.Errorf("helper lacks owner %v", o)
		}
	}
	if strings.Contains(src, "{{OWNERS}}") || strings.Contains(src, "65534") {
		t.Error("helper owners not rendered or still use 65534")
	}
	script := buildahScript("http://deps:8080", "registry.x:5000")
	for _, want := range []string{"--isolation=chroot", "--storage-driver=vfs", "$HOME/containers/root", "$HOME/containers/run", "--authfile /creds/config.json", "--digestfile", "pushed_digest="} {
		if !strings.Contains(script, want) {
			t.Errorf("buildah script lacks %q", want)
		}
	}
	// With the fallback mount the Pod gets the two files from a Secret.
	q := &Qualifier{c: &Cluster{RunID: "abc"}, nsMain: NamespacePrefix + "abc"}
	obj := q.c.PodObject(q.stepPod("b", "quay.io/buildah/stable@sha256:"+strings.Repeat("a", 64), false, true))
	if err := Guard(obj, "abc"); err != nil {
		t.Fatalf("fallback subid shape refused: %v", err)
	}
	if !strings.Contains(fmt.Sprint(obj), "/etc/subuid") || !strings.Contains(fmt.Sprint(obj), "/etc/subgid") {
		t.Error("fallback subid files are not mounted")
	}
}

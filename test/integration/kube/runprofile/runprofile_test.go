package runprofile_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/integration/kube/runprofile"
	"github.com/optimiware/kodama/test/support/layer"
)

// Test_run_resource_profile_qualification is the REAL qualification of the
// deployable rootless run-resource profile on the explicitly configured
// target (the acceptance planned for T-121). The infrastructure-free unit
// layer (-tags unit) skips it and runs only the Test_unit_runprofile_*
// offline tests; that skip is not qualification evidence. In the normal
// non-unit layer it never skips: a missing target, tool, credential or
// prerequisite fails it, and so does any required capability of the O-30
// profile (storage, ko, Buildah in the step role, PostgreSQL, the tenant
// network model, cleanup) that is not proven by a real probe. The legacy
// kind-only suites do not stand in for it. Run it through verify.sh.
//
//	KODAMA_RUNPROFILE_KUBECONFIG    explicit kubeconfig of the disposable-probe-capable target
//	KODAMA_RUNPROFILE_TARGET        name recorded in the report
//	KODAMA_RUNPROFILE_STORAGE_CLASS CSI class for the RWOP run claim
//	KODAMA_RUNPROFILE_REPORT_DIR    where receipts and report.json/report.md are retained
func Test_run_resource_profile_qualification(t *testing.T) {
	layer.SkipService(t, "run-resource profile qualification (a real Kubernetes target)")
	cfg, err := runprofile.ConfigFromEnv(os.Getenv)
	if err != nil {
		t.Fatalf("qualification cannot run (no skip, no substitute evidence): %v", err)
	}
	release, err := runprofile.AcquireLease(runprofile.LeasePath, 20*time.Minute)
	if err != nil {
		t.Fatalf("manual integration lease: %v", err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Minute)
	defer cancel()
	rep, err := runprofile.Run(ctx, cfg)
	if rep != nil {
		t.Logf("report: %s (run %s) verdict=%s", cfg.ReportDir, rep.RunID, rep.Verdict)
		for _, c := range rep.Checks {
			t.Logf("  %-34s required=%-5t %-14s %s", c.ID, c.Required, c.Status, firstN(c.Detail, 160))
		}
		for _, m := range rep.Matrix {
			t.Logf("  matrix %-14s %s", m.Status, m.Combination)
		}
		for _, h := range rep.Handoffs {
			t.Logf("  HANDOFF: %s", h)
		}
		t.Logf("  cleanup verified=%t leftovers=%v", rep.Cleanup.Verified, rep.Cleanup.Leftovers)
	}
	if err != nil {
		t.Fatalf("qualification run failed: %v", err)
	}
	if rep.Verdict != "qualified" {
		t.Fatalf("profile is %s, not qualified: see the report and handoffs above", rep.Verdict)
	}
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func okPod() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "p", "namespace": runprofile.NamespacePrefix + "abc", "labels": map[string]any{runprofile.LabelTask: "t111", runprofile.LabelRun: "abc"}},
		"spec": map[string]any{
			"hostUsers":       false,
			"securityContext": map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}, "runAsNonRoot": true, "runAsUser": 1000},
			"containers": []any{map[string]any{"name": "main", "image": "example.invalid/x@sha256:" + strings.Repeat("a", 64),
				"securityContext": map[string]any{"capabilities": map[string]any{"drop": []any{"ALL"}}}}},
		},
	}
}

func clone(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(m)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func Test_unit_runprofile_guard(t *testing.T) {
	if err := runprofile.Guard(okPod(), "abc"); err != nil {
		t.Fatalf("baseline pod refused: %v", err)
	}
	mut := map[string]func(map[string]any){
		"privileged":        func(p map[string]any) { setContainerSC(p, "privileged", true) },
		"host network":      func(p map[string]any) { p["spec"].(map[string]any)["hostNetwork"] = true },
		"host pid":          func(p map[string]any) { p["spec"].(map[string]any)["hostPID"] = true },
		"host users":        func(p map[string]any) { p["spec"].(map[string]any)["hostUsers"] = true },
		"host users absent": func(p map[string]any) { delete(p["spec"].(map[string]any), "hostUsers") },
		"hostPath volume": func(p map[string]any) {
			p["spec"].(map[string]any)["volumes"] = []any{map[string]any{"name": "h", "hostPath": map[string]any{"path": "/run/containerd/containerd.sock"}}}
		},
		"unconfined seccomp": func(p map[string]any) {
			p["spec"].(map[string]any)["securityContext"] = map[string]any{"seccompProfile": map[string]any{"type": "Unconfined"}, "runAsNonRoot": true, "runAsUser": 1000}
		},
		"unconfined apparmor": func(p map[string]any) { setContainerSC(p, "appArmorProfile", map[string]any{"type": "Unconfined"}) },
		"apparmor annotation": func(p map[string]any) {
			p["metadata"].(map[string]any)["annotations"] = map[string]any{"container.apparmor.security.beta.kubernetes.io/main": "unconfined"}
		},
		"net admin": func(p map[string]any) { setContainerSC(p, "capabilities", map[string]any{"add": []any{"NET_ADMIN"}}) },
		"mutable image": func(p map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "alpine:latest"
		},
		"foreign ns":     func(p map[string]any) { p["metadata"].(map[string]any)["namespace"] = "kodama-tenant-stage" },
		"missing labels": func(p map[string]any) { delete(p["metadata"].(map[string]any), "labels") },
		"other run": func(p map[string]any) {
			p["metadata"].(map[string]any)["labels"].(map[string]any)[runprofile.LabelRun] = "zzz"
		},
		"pinned node":   func(p map[string]any) { p["spec"].(map[string]any)["nodeName"] = "n1" },
		"proc unmasked": func(p map[string]any) { setContainerSC(p, "procMount", "Unmasked") },
		"sys admin": func(p map[string]any) {
			setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}, "add": []any{"SETUID", "SETGID", "SYS_ADMIN"}})
		},
		"sys chroot": func(p map[string]any) {
			setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}, "add": []any{"SYS_CHROOT"}})
		},
		"all capabilities": func(p map[string]any) {
			setContainerSC(p, "capabilities", map[string]any{"add": []any{"ALL"}})
		},
		"capabilities without drop all": func(p map[string]any) {
			setContainerSC(p, "capabilities", map[string]any{"add": []any{"SETUID"}})
			setContainerSC(p, "runAsUser", 1000)
		},
		"capabilities as root": func(p map[string]any) {
			setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}, "add": []any{"SETUID", "SETGID"}})
			setContainerSC(p, "runAsUser", 0)
		},
		"unknown localhost seccomp": func(p map[string]any) {
			p["spec"].(map[string]any)["securityContext"] = map[string]any{"seccompProfile": map[string]any{"type": "Localhost", "localhostProfile": "other.json"}, "runAsNonRoot": true, "runAsUser": 1000}
		},
		"v1 localhost apparmor": func(p map[string]any) {
			setContainerSC(p, "appArmorProfile", map[string]any{"type": "Localhost", "localhostProfile": "kodama-rootless-build-v1"})
		},
		"v1 apparmor annotation": func(p map[string]any) {
			p["metadata"].(map[string]any)["annotations"] = map[string]any{"container.apparmor.security.beta.kubernetes.io/main": "localhost/kodama-rootless-build-v1"}
		},
		"unknown localhost apparmor": func(p map[string]any) {
			setContainerSC(p, "appArmorProfile", map[string]any{"type": "Localhost", "localhostProfile": "my-profile"})
		},
		"host ipc": func(p map[string]any) { p["spec"].(map[string]any)["hostIPC"] = true },
		"container seccomp unconfined over pod localhost": func(p map[string]any) {
			setContainerSC(p, "seccompProfile", map[string]any{"type": "Unconfined"})
		},
	}
	for name, f := range mut {
		p := clone(t, okPod())
		f(p)
		if err := runprofile.Guard(p, "abc"); err == nil {
			t.Errorf("%s: guard accepted a forbidden object", name)
		}
	}
	ns := map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "labels": map[string]any{runprofile.LabelTask: "t111", runprofile.LabelRun: "abc"}}}
	if err := runprofile.Guard(ns, "abc"); err == nil {
		t.Error("guard accepted a namespace outside the task prefix")
	}
	// Only the closed set of namespaced kinds (plus task-prefixed Namespaces) may be written.
	for _, kind := range []string{"ClusterRole", "ClusterRoleBinding", "PersistentVolume", "StorageClass", "CustomResourceDefinition", "Node", "ValidatingWebhookConfiguration", "Deployment", "ConfigMap", "ServiceAccount", ""} {
		o := map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": "x", "namespace": runprofile.NamespacePrefix + "abc", "labels": map[string]any{runprofile.LabelTask: "t111", runprofile.LabelRun: "abc"}}}
		if err := runprofile.Guard(o, "abc"); err == nil {
			t.Errorf("guard accepted unsupported kind %q", kind)
		}
	}
	for _, kind := range []string{"Secret", "Service", "NetworkPolicy", "ResourceQuota", "PersistentVolumeClaim"} {
		o := map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": "x", "namespace": runprofile.NamespacePrefix + "abc", "labels": map[string]any{runprofile.LabelTask: "t111", runprofile.LabelRun: "abc"}}}
		if err := runprofile.Guard(o, "abc"); err != nil {
			t.Errorf("guard refused supported kind %q: %v", kind, err)
		}
	}
	// Permitted additions are exactly the mapping-helper capabilities, as a
	// non-root user, with only the installed Localhost profiles.
	p := clone(t, okPod())
	setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}, "add": []any{"SETUID", "SETGID"}})
	setContainerSC(p, "allowPrivilegeEscalation", true)
	setContainerSC(p, "runAsUser", 1000)
	p["spec"].(map[string]any)["securityContext"] = map[string]any{
		"runAsNonRoot":    true,
		"seccompProfile":  map[string]any{"type": "Localhost", "localhostProfile": runprofile.StepSeccompProfile},
		"appArmorProfile": map[string]any{"type": "Localhost", "localhostProfile": runprofile.StepAppArmorProfile},
	}
	if err := runprofile.Guard(p, "abc"); err != nil {
		t.Errorf("step role shape refused: %v", err)
	}
	if runprofile.StepAppArmorProfile != "kodama-rootless-build-v2" || runprofile.StepAppArmorKernelState != "kodama-rootless-build-v2 (unconfined)" ||
		runprofile.StepSeccompProfile != "kodama/kodama-rootless-build-v1.json" {
		t.Errorf("step profile names changed: %s %s %s", runprofile.StepAppArmorProfile, runprofile.StepAppArmorKernelState, runprofile.StepSeccompProfile)
	}
	// procMount: Unmasked is allowed only inside that exact shape.
	setContainerSC(p, "procMount", "Unmasked")
	if err := runprofile.Guard(p, "abc"); err != nil {
		t.Errorf("step role with procMount Unmasked refused: %v", err)
	}
	// ...and the SYS_ADMIN/privileged/Unconfined/host-user variants of it are not.
	for name, f := range map[string]func(map[string]any){
		"sys admin": func(p map[string]any) {
			setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}, "add": []any{"SETUID", "SETGID", "SYS_ADMIN"}})
		},
		"privileged": func(p map[string]any) { setContainerSC(p, "privileged", true) },
		"host users": func(p map[string]any) { p["spec"].(map[string]any)["hostUsers"] = true },
		"unconfined apparmor": func(p map[string]any) {
			setContainerSC(p, "appArmorProfile", map[string]any{"type": "Unconfined"})
		},
		"unconfined apparmor at pod level": func(p map[string]any) {
			p["spec"].(map[string]any)["securityContext"].(map[string]any)["appArmorProfile"] = map[string]any{"type": "Unconfined"}
		},
		"v1 apparmor": func(p map[string]any) {
			p["spec"].(map[string]any)["securityContext"].(map[string]any)["appArmorProfile"] = map[string]any{"type": "Localhost", "localhostProfile": "kodama-rootless-build-v1"}
		},
		"hostPath": func(p map[string]any) {
			p["spec"].(map[string]any)["volumes"] = []any{map[string]any{"name": "h", "hostPath": map[string]any{"path": "/"}}}
		},
	} {
		v := clone(t, p)
		f(v)
		if err := runprofile.Guard(v, "abc"); err == nil {
			t.Errorf("step role variant %q accepted", name)
		}
	}
}

// stepShapedPod is the complete step role (non-root, drop ALL + SETUID/SETGID,
// escalation, Localhost profiles, hostUsers false).
func stepShapedPod(t *testing.T) map[string]any {
	p := clone(t, okPod())
	setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}, "add": []any{"SETUID", "SETGID"}})
	setContainerSC(p, "allowPrivilegeEscalation", true)
	p["spec"].(map[string]any)["securityContext"] = map[string]any{
		"runAsNonRoot": true, "runAsUser": 1000,
		"seccompProfile":  map[string]any{"type": "Localhost", "localhostProfile": runprofile.StepSeccompProfile},
		"appArmorProfile": map[string]any{"type": "Localhost", "localhostProfile": runprofile.StepAppArmorProfile},
	}
	return p
}

func delContainerSC(p map[string]any, key string) {
	c := p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	delete(c["securityContext"].(map[string]any), key)
}

// Identity and capability omissions are refused independently of any added
// capability: a user-namespaced Pod never inherits root or the runtime's
// default capability set by leaving a field out.
func Test_unit_runprofile_guard_omissions(t *testing.T) {
	if err := runprofile.Guard(stepShapedPod(t), "abc"); err != nil {
		t.Fatalf("complete step role refused: %v", err)
	}
	unmasked := func(p map[string]any) { setContainerSC(p, "procMount", "Unmasked") }
	cases := map[string]func(map[string]any){
		"root with Localhost profiles, drop ALL, no additions, Unmasked": func(p map[string]any) {
			setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}})
			setContainerSC(p, "allowPrivilegeEscalation", false)
			p["spec"].(map[string]any)["securityContext"].(map[string]any)["runAsUser"] = 0
			delete(p["spec"].(map[string]any)["securityContext"].(map[string]any), "runAsNonRoot")
			unmasked(p)
		},
		"root without additions": func(p map[string]any) {
			setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}})
			p["spec"].(map[string]any)["securityContext"].(map[string]any)["runAsUser"] = 0
		},
		"container runs as root over a non-root Pod": func(p map[string]any) { setContainerSC(p, "runAsUser", 0) },
		"missing runAsNonRoot": func(p map[string]any) {
			delete(p["spec"].(map[string]any)["securityContext"].(map[string]any), "runAsNonRoot")
		},
		"runAsNonRoot false": func(p map[string]any) { setContainerSC(p, "runAsNonRoot", false) },
		"missing runAsUser": func(p map[string]any) {
			delete(p["spec"].(map[string]any)["securityContext"].(map[string]any), "runAsUser")
		},
		"missing capabilities entirely":  func(p map[string]any) { delContainerSC(p, "capabilities") },
		"missing drop with no additions": func(p map[string]any) { setContainerSC(p, "capabilities", map[string]any{}) },
		"drop without ALL":               func(p map[string]any) { setContainerSC(p, "capabilities", map[string]any{"drop": []any{"NET_RAW"}}) },
		"no container securityContext": func(p map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["securityContext"] = nil
		},
		"Unmasked without hostUsers false": func(p map[string]any) { unmasked(p); p["spec"].(map[string]any)["hostUsers"] = true },
		"Unmasked with hostUsers absent":   func(p map[string]any) { unmasked(p); delete(p["spec"].(map[string]any), "hostUsers") },
		"Unmasked without Localhost seccomp": func(p map[string]any) {
			unmasked(p)
			p["spec"].(map[string]any)["securityContext"].(map[string]any)["seccompProfile"] = map[string]any{"type": "RuntimeDefault"}
		},
		"Unmasked without Localhost AppArmor": func(p map[string]any) {
			unmasked(p)
			delete(p["spec"].(map[string]any)["securityContext"].(map[string]any), "appArmorProfile")
		},
		"Unmasked without the added capabilities": func(p map[string]any) {
			unmasked(p)
			setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}})
		},
		"Unmasked without escalation": func(p map[string]any) { unmasked(p); setContainerSC(p, "allowPrivilegeEscalation", false) },
		"Unmasked with only SETUID": func(p map[string]any) {
			unmasked(p)
			setContainerSC(p, "capabilities", map[string]any{"drop": []any{"ALL"}, "add": []any{"SETUID"}})
		},
		"init container without drop ALL": func(p map[string]any) {
			c := clone(t, p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any))
			c["name"] = "init"
			c["securityContext"] = map[string]any{}
			p["spec"].(map[string]any)["initContainers"] = []any{c}
		},
	}
	for name, f := range cases {
		p := stepShapedPod(t)
		f(p)
		if err := runprofile.Guard(p, "abc"); err == nil {
			t.Errorf("%s: guard accepted the omission", name)
		}
	}
	// The complete step role with Unmasked is the only Unmasked shape accepted.
	p := stepShapedPod(t)
	setContainerSC(p, "procMount", "Unmasked")
	if err := runprofile.Guard(p, "abc"); err != nil {
		t.Errorf("complete step role with Unmasked refused: %v", err)
	}
}

func setContainerSC(p map[string]any, key string, v any) {
	c := p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	sc, _ := c["securityContext"].(map[string]any)
	if sc == nil {
		sc = map[string]any{}
		c["securityContext"] = sc
	}
	sc[key] = v
}

func Test_unit_runprofile_configuration(t *testing.T) {
	_, err := runprofile.ConfigFromEnv(func(string) string { return "" })
	if err == nil {
		t.Fatal("empty environment produced a configuration")
	}
	for _, want := range []string{"KODAMA_RUNPROFILE_KUBECONFIG", "KODAMA_RUNPROFILE_TARGET", "KODAMA_RUNPROFILE_STORAGE_CLASS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	// A legacy kind scope variable is not a target.
	_, err = runprofile.ConfigFromEnv(func(k string) string {
		if k == "KODAMA_TEST_KUBECONFIG" {
			return "/nonexistent"
		}
		return ""
	})
	if err == nil {
		t.Fatal("legacy test kubeconfig must not select the qualification target")
	}
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kc, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runprofile.ConfigFromEnv(func(k string) string {
		switch k {
		case "KODAMA_RUNPROFILE_KUBECONFIG":
			return kc
		case "KODAMA_RUNPROFILE_TARGET":
			return "t"
		case "KODAMA_RUNPROFILE_STORAGE_CLASS":
			return "c"
		case "KODAMA_RUNPROFILE_KUBECTL":
			return "definitely-not-a-binary-xyz"
		}
		return ""
	})
	if err == nil {
		t.Fatal("missing kubectl did not fail closed")
	}
}

func Test_unit_runprofile_pins(t *testing.T) {
	pins, err := runprofile.LoadPins()
	if err != nil {
		t.Fatal(err)
	}
	for name, img := range pins.Images {
		if !strings.Contains(img.Ref, "@sha256:") || strings.Contains(img.Ref, "example") || strings.Contains(img.Ref, strings.Repeat("1", 64)) {
			t.Errorf("%s: %s is not a real digest pin", name, img.Ref)
		}
	}
}

func rep(checks ...runprofile.Check) *runprofile.Report {
	r := &runprofile.Report{Checks: checks, Started: time.Now()}
	r.Cleanup = runprofile.Cleanup{Verified: true, Namespaces: []runprofile.CleanupItem{{Name: "krp-t111-a", Deleted: true}}}
	return r
}

func full() []runprofile.Check {
	var out []runprofile.Check
	for _, id := range runprofile.RequiredChecks {
		out = append(out, runprofile.Check{ID: id, Required: true, Status: runprofile.Pass})
	}
	return out
}

func Test_unit_runprofile_verdicts(t *testing.T) {
	pass := runprofile.Check{ID: "build.ko", Required: true, Status: runprofile.Pass}
	cases := []struct {
		name string
		r    *runprofile.Report
		want string
	}{
		{"all required pass", rep(full()...), "qualified"},
		{"required blocked", rep(pass, runprofile.Check{ID: "build.buildah_vfs", Required: true, Status: runprofile.Blocked, Handoff: "install X"}), "blocked"},
		{"required failed beats blocked", rep(runprofile.Check{ID: "a", Required: true, Status: runprofile.Blocked}, runprofile.Check{ID: "b", Required: true, Status: runprofile.Fail}), "failed"},
		{"required never ran", rep(pass, runprofile.Check{ID: "n", Required: true, Status: runprofile.NotRun}), "blocked"},
		{"optional failure does not matter", rep(append(full(), runprofile.Check{ID: "o", Required: false, Status: runprofile.Fail})...), "qualified"},
		{"overlay not advertised is not a pass", rep(append(full(), runprofile.Check{ID: "build.buildah_overlay_fuse", Required: false, Status: runprofile.NotAdvertised})...), "qualified"},
	}
	for _, c := range cases {
		c.r.Finalize()
		if c.r.Verdict != c.want {
			t.Errorf("%s: verdict %s, want %s", c.name, c.r.Verdict, c.want)
		}
	}
	unclean := rep(full()...)
	unclean.Cleanup.Verified = false
	unclean.Finalize()
	if unclean.Verdict == "qualified" {
		t.Error("unverified cleanup qualified the profile")
	}
	empty := rep()
	empty.Finalize()
	for _, row := range empty.Matrix {
		if row.Status == "qualified" {
			t.Errorf("matrix row %q qualified without any check", row.Combination)
		}
	}
	blocked := rep(pass, runprofile.Check{ID: "psa.builder_pod", Required: true, Status: runprofile.Pass},
		runprofile.Check{ID: "step.role_shape", Required: true, Status: runprofile.Pass},
		runprofile.Check{ID: "step.role_shape.ko", Required: true, Status: runprofile.Pass},
		runprofile.Check{ID: "build.buildah_vfs", Required: true, Status: runprofile.Blocked, Handoff: "install X"})
	blocked.Finalize()
	if len(blocked.Handoffs) != 1 || !strings.Contains(blocked.Markdown(), "install X") {
		t.Error("handoff not surfaced in report")
	}
	var ko, buildah string
	for _, row := range blocked.Matrix {
		if strings.HasPrefix(row.Combination, "ko build") {
			ko = row.Status
		}
		if strings.HasPrefix(row.Combination, "Buildah build+push") {
			buildah = row.Status
		}
	}
	if ko != "qualified" || buildah != "blocked" {
		t.Errorf("matrix ko=%s buildah=%s", ko, buildah)
	}
}

func Test_unit_runprofile_redaction(t *testing.T) {
	dir := t.TempDir()
	rec, err := runprofile.NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.AddSecret("s3cr3t-value-123")
	if got := rec.Redact("token=s3cr3t-value-123 end"); strings.Contains(got, "s3cr3t") {
		t.Fatalf("not redacted: %s", got)
	}
	k := &runprofile.Kubectl{Binary: "/bin/echo", Kubeconfig: "/kc", Rec: rec}
	out, err := k.Run(context.Background(), []byte("secret stdin s3cr3t-value-123"), "hello", "s3cr3t-value-123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("output leaked: %s", out)
	}
	_ = rec.Close()
	data, _ := os.ReadFile(filepath.Join(dir, "receipts.jsonl"))
	if strings.Contains(string(data), "s3cr3t") || strings.Contains(string(data), "secret stdin") || !strings.Contains(string(data), "stdinSha256") {
		t.Fatalf("receipt leaked secret or lacks stdin digest: %s", data)
	}
}

func Test_unit_runprofile_quiet_output(t *testing.T) {
	dir := t.TempDir()
	rec, _ := runprofile.NewRecorder(dir)
	rec.SetQuiet(true)
	k := &runprofile.Kubectl{Binary: "/bin/echo", Kubeconfig: "/kc", Rec: rec}
	if _, err := k.Run(context.Background(), nil, "$2y$05$hash"); err != nil {
		t.Fatal(err)
	}
	_ = rec.Close()
	data, _ := os.ReadFile(filepath.Join(dir, "receipts.jsonl"))
	if strings.Count(string(data), "$2y$05$hash") != 1 { // argv only, never output
		t.Fatalf("output not suppressed: %s", data)
	}
}

func Test_unit_runprofile_dependency_archive(t *testing.T) {
	data, err := runprofile.DependencyTree("payload-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 1024 || len(data)%512 != 0 || !bytes.Equal(data[len(data)-1024:], make([]byte, 1024)) {
		t.Fatal("tar stream lacks its end-of-archive trailer (busybox tar reports a short read)")
	}
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = b
	}
	if string(files["data.txt"]) != "payload-x\n" {
		t.Errorf("data.txt = %q", files["data.txt"])
	}
	zr, err := zip.NewReader(bytes.NewReader(files["goproxy/example.com/dep/@v/v1.0.0.zip"]), int64(len(files["goproxy/example.com/dep/@v/v1.0.0.zip"])))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["example.com/dep@v1.0.0/go.mod"] || !names["example.com/dep@v1.0.0/dep.go"] {
		t.Errorf("module zip layout wrong: %v", names)
	}
	if !strings.Contains(string(files["goproxy/example.com/dep/@v/v1.0.0.info"]), `"Version":"v1.0.0"`) {
		t.Error("info file wrong")
	}
}

func Test_unit_runprofile_lease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.lock")
	release, err := runprofile.AcquireLease(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runprofile.AcquireLease(path, 300*time.Millisecond); err == nil {
		t.Fatal("second holder acquired the lease")
	}
	release()
	again, err := runprofile.AcquireLease(path, time.Second)
	if err != nil {
		t.Fatalf("lease not released: %v", err)
	}
	again()
}

package runprofile

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// logStub is a kubectl stand-in that logs every invocation to a file and
// answers namespace creation/lookup according to the given script fragments.
func logStub(t *testing.T, create, getNS string) (path, logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/bash
echo "$*" >> %q
args="$*"
case "$args" in
  *" create "*) %s ;;
  *" get namespace "*" -o json"*) %s ;;
  *" get pv -o jsonpath"*) ;;
  *" get namespace -l "*) ;;
  *" proxy "*) echo "Starting to serve on 127.0.0.1:1"; exec sleep 30 ;;
  *) echo "unexpected: $args" >&2; exit 2 ;;
esac
`, logFile, create, getNS)
	path = filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, logFile
}

func newCluster(t *testing.T, kubectl string) *Cluster {
	t.Helper()
	rec, err := NewRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	return &Cluster{K: &Kubectl{Binary: kubectl, Kubeconfig: "/kc", Rec: rec}, RunID: "abc"}
}

func Test_unit_runprofile_namespace_collision(t *testing.T) {
	kubectl, calls := logStub(t, `echo 'Error from server (AlreadyExists): namespaces "krp-t111-abc" already exists' >&2; exit 1`, `exit 9`)
	c := newCluster(t, kubectl)
	err := c.CreateNamespace(context.Background(), NamespacePrefix+"abc", nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision not refused: %v", err)
	}
	if len(c.Namespaces) != 0 {
		t.Fatalf("foreign namespace recorded as owned: %v", c.Namespaces)
	}
	log, _ := os.ReadFile(calls)
	for _, bad := range []string{" apply ", " label ", " patch ", " delete ", " replace "} {
		if strings.Contains(" "+string(log)+" ", bad) {
			t.Fatalf("foreign namespace modified via%s: %s", bad, log)
		}
	}
	if strings.Count(string(log), "\n") != 1 || !strings.Contains(string(log), " create ") {
		t.Fatalf("expected exactly one create call, got %q", log)
	}
	// Apply must never carry a Namespace (it would adopt and relabel).
	if _, err := c.Apply(context.Background(), c.obj("v1", "Namespace", "", NamespacePrefix+"abc", nil, nil)); err == nil {
		t.Fatal("Apply accepted a Namespace object")
	}
	// A bare creation response without a UID records nothing.
	kubectl2, _ := logStub(t, `true`, `exit 9`)
	c2 := newCluster(t, kubectl2)
	if err := c2.CreateNamespace(context.Background(), NamespacePrefix+"abc", nil); err == nil || len(c2.Namespaces) != 0 {
		t.Fatalf("empty creation UID accepted: %v %v", err, c2.Namespaces)
	}
}

func Test_unit_runprofile_namespace_replacement(t *testing.T) {
	// Creation reports uid-created; a later lookup by name finds a foreign
	// replacement. The recorded UID is the creation response's, so teardown
	// refuses the replacement and sends no delete.
	kubectl, _ := logStub(t, `if [[ "$args" == *"-o jsonpath"* ]]; then printf uid-created; fi`,
		`echo '{"metadata":{"uid":"uid-replacement","resourceVersion":"9","labels":{"openforge.dev/task":"t111","openforge.dev/run":"abc"}}}'`)
	c := newCluster(t, kubectl)
	// ResourceQuota apply is not part of this scenario: drive creation only.
	name := NamespacePrefix + "abc"
	_ = c.CreateNamespaceOnly(context.Background(), name, nil)
	if c.Namespaces[name] != "uid-created" {
		t.Fatalf("recorded %q, want the UID from the creation response", c.Namespaces[name])
	}
	api, port := serveStatus(t, 200)
	_ = port
	rep := &Report{}
	q := &Qualifier{c: c, rep: rep}
	hc := &http.Client{Timeout: 5 * time.Second}
	deleted, pending, detail := q.deleteChecked(context.Background(), hc, api.url, name, c.Namespaces[name])
	if deleted || pending || !strings.Contains(detail, "refused") || api.count() != 0 {
		t.Fatalf("replacement was deleted or not refused: deleted=%v pending=%v detail=%q calls=%d", deleted, pending, detail, api.count())
	}
}

func Test_unit_runprofile_complete_inventory(t *testing.T) {
	cleanOK := Cleanup{Verified: true, Namespaces: []CleanupItem{{Name: "krp-t111-a", Deleted: true}}}
	// Empty report with "verified" cleanup.
	empty := &Report{Cleanup: cleanOK}
	empty.Finalize()
	if empty.Verdict == "qualified" {
		t.Fatal("zero-check report qualified")
	}
	for _, id := range RequiredChecks {
		c := empty.Find(id)
		if c == nil || c.Status != NotRun || !c.Required {
			t.Errorf("missing %s not recorded as required not_run: %+v", id, c)
		}
	}
	// Partial report: everything but the browser and cleanup evidence.
	var partial []Check
	for _, id := range RequiredChecks {
		if id != "service.browser" {
			partial = append(partial, Check{ID: id, Required: true, Status: Pass})
		}
	}
	p := &Report{Checks: partial, Cleanup: cleanOK}
	p.Finalize()
	if p.Verdict == "qualified" || p.Find("service.browser").Status != NotRun {
		t.Fatalf("partial report: verdict %s", p.Verdict)
	}
	// Complete but cleanup unproven or no namespaces recorded.
	var all []Check
	for _, id := range RequiredChecks {
		all = append(all, Check{ID: id, Required: true, Status: Pass})
	}
	for name, cl := range map[string]Cleanup{"unverified": {Verified: false, Namespaces: cleanOK.Namespaces}, "no namespaces": {Verified: true}} {
		r := &Report{Checks: append([]Check(nil), all...), Cleanup: cl}
		r.Finalize()
		if r.Verdict == "qualified" {
			t.Errorf("%s cleanup qualified", name)
		}
	}
	ok := &Report{Checks: append([]Check(nil), all...), Cleanup: cleanOK}
	ok.Finalize()
	if ok.Verdict != "qualified" {
		t.Fatalf("complete passing report not qualified: %s", ok.Verdict)
	}
}

func Test_unit_runprofile_interrupted_run(t *testing.T) {
	kubectl, _ := logStub(t, `exit 9`, `exit 9`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := Run(ctx, &Config{Kubectl: kubectl, Kubeconfig: "/kc", Target: "t", StorageClass: "c", ReportDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict == "qualified" {
		t.Fatal("interrupted run qualified")
	}
	c := rep.Find("run.interrupted")
	if c == nil || c.Status != Fail || !strings.Contains(c.Detail, "provision") || !strings.Contains(c.Detail, "network") {
		t.Fatalf("interruption not recorded with omitted phases: %+v", c)
	}
	for _, id := range RequiredChecks {
		if rep.Find(id) == nil {
			t.Errorf("required check %s missing from interrupted report", id)
		}
	}
}

func Test_unit_runprofile_buildkit_mode_selection(t *testing.T) {
	pass := func(id string) Check {
		return Check{ID: id, Required: false, Status: Pass, Evidence: []string{"default pushed"}}
	}
	// Default-only success: selected check names the default mode only.
	sel := SelectedFromDefault(pass("build.buildkit_default"))
	r := &Report{Checks: []Check{pass("build.buildkit_default"), sel}}
	r.Finalize()
	name := r.buildKitRowName()
	if !strings.Contains(name, "rootless default") || strings.Contains(name, "no-process-sandbox") {
		t.Fatalf("default-only success advertised %q", name)
	}
	if strings.Contains(sel.Detail, "no-process-sandbox") || sel.Mode != "rootless default (process sandbox)" {
		t.Fatalf("selected projection mislabelled: %+v", sel)
	}
	for _, row := range r.Matrix {
		if strings.Contains(row.Combination, "no-process-sandbox") || strings.Contains(row.Combination, "run-exclusive") {
			t.Errorf("row claims untested mode: %s", row.Combination)
		}
	}
	// Fallback: default blocked, no-process-sandbox passed -> that mode is rendered.
	fb := &Report{Checks: []Check{
		{ID: "build.buildkit_default", Status: Blocked},
		{ID: "build.buildkit_selected", Required: true, Status: Pass, Mode: "rootless --oci-worker-no-process-sandbox"},
	}}
	fb.Finalize()
	if !strings.Contains(fb.buildKitRowName(), "no-process-sandbox") {
		t.Fatalf("fallback mode not rendered: %q", fb.buildKitRowName())
	}
	// Historical shape: both blocked stays blocked, never qualified.
	bl := &Report{Checks: []Check{
		{ID: "build.buildkit_default", Status: Blocked},
		{ID: "build.buildkit_selected", Required: true, Status: Blocked, Mode: "rootless --oci-worker-no-process-sandbox"},
	}}
	bl.Finalize()
	for _, row := range bl.Matrix {
		if strings.HasPrefix(row.Combination, "BuildKit selected") && row.Status != "blocked" {
			t.Errorf("blocked BuildKit row is %s", row.Status)
		}
	}
	// No selected check at all: never qualified.
	none := &Report{}
	none.Finalize()
	for _, row := range none.Matrix {
		if strings.HasPrefix(row.Combination, "BuildKit selected") && row.Status == "qualified" {
			t.Error("untested BuildKit row qualified")
		}
	}
}

func Test_unit_runprofile_network_controls(t *testing.T) {
	targets := []NetTarget{
		{"srv-same(pod ip)", "", true}, {"srv-other-run(pod ip)", "", true},
		{"kubernetes-api", "", false}, {"public-1.1.1.1", "", false},
	}
	reverse := []NetTarget{{"srv-same(pod ip)", "", true}}
	obs := func(ctlAPI, ctlPub, afterAPI, afterPub string) NetObservations {
		return NetObservations{
			Control:      map[string]string{"srv-same(pod ip)": "open", "srv-other-run(pod ip)": "open", "kubernetes-api": ctlAPI, "public-1.1.1.1": ctlPub},
			After:        map[string]string{"srv-same(pod ip)": "open", "srv-other-run(pod ip)": "closed", "kubernetes-api": afterAPI, "public-1.1.1.1": afterPub},
			ControlOther: map[string]string{"srv-same(pod ip)": "open"}, AfterOther: map[string]string{"srv-same(pod ip)": "closed"},
			ControlForeign: map[string]string{"srv-same(pod ip)": "open"}, AfterForeign: map[string]string{"srv-same(pod ip)": "closed"},
		}
	}
	if st, d := EvaluateNetwork(targets, reverse, obs("open", "open", "closed", "closed")); st != Pass {
		t.Fatalf("all controls live: %s %s", st, d)
	}
	for name, o := range map[string]NetObservations{
		"closed API control":    obs("closed", "open", "closed", "closed"),
		"absent API control":    obs("", "open", "closed", "closed"),
		"closed public control": obs("open", "closed", "closed", "closed"),
		"absent public control": obs("open", "", "closed", "closed"),
	} {
		st, d := EvaluateNetwork(targets, reverse, o)
		if st == Pass || st != Blocked || !strings.Contains(d, "Unproven") {
			t.Errorf("%s: omitted control passed: %s %s", name, st, d)
		}
		if !strings.Contains(d, "allow srv-same") {
			t.Errorf("%s: proven claims not narrowed in detail: %s", name, d)
		}
	}
	// A live control that is not denied afterwards is a failure.
	if st, _ := EvaluateNetwork(targets, reverse, obs("open", "open", "open", "closed")); st != Fail {
		t.Errorf("reachable API after policy: %s", st)
	}
	// A core control missing is a failure, not a pass.
	o := obs("open", "open", "closed", "closed")
	o.Control["srv-other-run(pod ip)"] = ""
	if st, _ := EvaluateNetwork(targets, reverse, o); st != Fail {
		t.Errorf("missing core control: %s", st)
	}
}

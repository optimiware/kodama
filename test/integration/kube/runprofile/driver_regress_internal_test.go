package runprofile

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
	// Partial report: everything but PostgreSQL and the cleanup evidence.
	var partial []Check
	for _, id := range RequiredChecks {
		if id != "service.postgres" {
			partial = append(partial, Check{ID: id, Required: true, Status: Pass})
		}
	}
	p := &Report{Checks: partial, Cleanup: cleanOK}
	p.Finalize()
	if p.Verdict == "qualified" || p.Find("service.postgres").Status != NotRun {
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

func Test_unit_runprofile_inventory_is_o30(t *testing.T) {
	// The required inventory is exactly storage, ko, Buildah in the step role,
	// PostgreSQL and the tenant network model; BuildKit and browser rows,
	// checks and pins are gone.
	for _, id := range RequiredChecks {
		if strings.Contains(id, "buildkit") || strings.Contains(id, "browser") || id == "network.builder_egress" {
			t.Errorf("required inventory still contains %s", id)
		}
	}
	want := []string{"storage.rwop_sequential", "storage.rwop_exclusive", "storage.idmapped_ownership", "storage.release_delete",
		"build.ko", "step.role_shape.ko", "build.buildah_vfs", "step.role_shape", "service.postgres", "network.matrix"}
	for _, id := range want {
		found := false
		for _, r := range RequiredChecks {
			found = found || r == id
		}
		if !found {
			t.Errorf("required inventory lacks %s", id)
		}
	}
	r := &Report{Cleanup: Cleanup{Verified: true, Namespaces: []CleanupItem{{Name: "krp-t111-a", Deleted: true}}}}
	r.Finalize()
	for _, row := range r.Matrix {
		l := strings.ToLower(row.Combination)
		if strings.Contains(l, "buildkit") || strings.Contains(l, "browser") {
			t.Errorf("support matrix still lists %q", row.Combination)
		}
	}
	pins, err := LoadPins()
	if err != nil {
		t.Fatal(err)
	}
	for name := range pins.Images {
		if name == "buildkit" || name == "browser" {
			t.Errorf("pin %s must be removed", name)
		}
	}
}

func netTargets() []NetTarget {
	return []NetTarget{
		{"same", "cli-same", "", AllowTenant, ClaimSameTenant, ProbeHTTP},
		{"other-run", "cli-same", "", AllowTenant, ClaimSameTenant, ProbeHTTP},
		{"foreign", "cli-same", "", DenyCore, ClaimForeignTenant, ProbeTCP},
		{"api", "cli-same", "", DenyProbe, ClaimAPI, ProbeTCP},
		{"metadata", "cli-same", "", DenyProbe, ClaimMetadata, ProbeTCP},
		{"public", "cli-same", "", AllowPublic, ClaimPublic, ProbeTLS},
	}
}

func netObs(mut func(c, a map[string]string)) NetObservations {
	c := map[string]string{"same": "open", "other-run": "open", "foreign": "open", "api": "open", "metadata": "open", "public": "open"}
	a := map[string]string{"same": "open", "other-run": "open", "foreign": "closed", "api": "closed", "metadata": "closed", "public": "open"}
	if mut != nil {
		mut(c, a)
	}
	return NetObservations{Control: c, After: a}
}

func Test_unit_runprofile_network_controls(t *testing.T) {
	if st, d := EvaluateNetwork(netTargets(), netObs(nil)); st != Pass {
		t.Fatalf("all controls live: %s %s", st, d)
	}
	// A deny claim about a destination the run does not own is unproven
	// without a live control: blocked, claims narrowed, never passed.
	for name, mut := range map[string]func(c, a map[string]string){
		"closed API control":      func(c, a map[string]string) { c["api"] = "closed" },
		"absent API control":      func(c, a map[string]string) { delete(c, "api") },
		"closed metadata control": func(c, a map[string]string) { c["metadata"] = "closed" },
		"absent metadata control": func(c, a map[string]string) { delete(c, "metadata") },
	} {
		st, d := EvaluateNetwork(netTargets(), netObs(mut))
		if st != Blocked || !strings.Contains(d, "Unproven") || !strings.Contains(d, "allow same") {
			t.Errorf("%s: %s %s", name, st, d)
		}
	}
	// A live control that is not denied afterwards is a failure.
	for name, mut := range map[string]func(c, a map[string]string){
		"reachable API after policy":      func(c, a map[string]string) { a["api"] = "open" },
		"reachable metadata after policy": func(c, a map[string]string) { a["metadata"] = "open" },
		"reachable foreign tenant":        func(c, a map[string]string) { a["foreign"] = "open" },
		"unknown foreign observation":     func(c, a map[string]string) { delete(a, "foreign") },
		"foreign control missing":         func(c, a map[string]string) { c["foreign"] = "" },
		"same-tenant control missing":     func(c, a map[string]string) { c["same"] = "" },
	} {
		if st, _ := EvaluateNetwork(netTargets(), netObs(mut)); st != Fail {
			t.Errorf("%s: %s", name, st)
		}
	}
}

func Test_unit_runprofile_tenant_network_verdicts(t *testing.T) {
	for name, mut := range map[string]func(c, a map[string]string){
		"other-run client blocked by an over-strict run-scoped policy": func(c, a map[string]string) { a["other-run"] = "closed" },
		"public egress closed":  func(c, a map[string]string) { a["public"] = "closed" },
		"public control closed": func(c, a map[string]string) { c["public"] = "closed" },
		"probe execution error": func(c, a map[string]string) { a["error:cli-same"] = "exec failed" },
	} {
		st, d := EvaluateNetwork(netTargets(), netObs(mut))
		if st != Fail {
			t.Errorf("%s: %s %s", name, st, d)
		}
	}
	// Every claim of the tenant model must be tested: dropping one fails.
	for _, claim := range requiredNetClaims {
		var kept []NetTarget
		for _, tg := range netTargets() {
			if tg.Claim != claim {
				kept = append(kept, tg)
			}
		}
		st, d := EvaluateNetwork(kept, netObs(nil))
		if st != Fail || !strings.Contains(d, "claim not tested: "+claim) {
			t.Errorf("untested claim %q did not fail: %s %s", claim, st, d)
		}
	}
	// No target set and no observation ever passes.
	if st, _ := EvaluateNetwork(nil, NetObservations{}); st == Pass {
		t.Error("empty matrix passed")
	}
	st, d := EvaluateNetwork(netTargets(), netObs(nil))
	for _, want := range []string{"allow other-run", "deny foreign", "deny api", "deny metadata", "allow public"} {
		if st != Pass || !strings.Contains(d, want) {
			t.Errorf("pass detail lacks %q: %s", want, d)
		}
	}
}

// The policy the driver applies is the tenant model: any Pod of the tenant
// namespace reaches the service ports (no run label selector) and public
// egress excludes private, link-local and CGNAT ranges.
func Test_unit_runprofile_tenant_policy_shape(t *testing.T) {
	q := &Qualifier{c: &Cluster{RunID: "abc"}}
	pols := q.tenantPolicies(NamespacePrefix+"abc", []int{8080})
	names := map[string]map[string]any{}
	for _, p := range pols {
		if err := Guard(p, "abc"); err != nil {
			t.Fatalf("policy refused by guard: %v", err)
		}
		names[p["metadata"].(map[string]any)["name"].(string)] = p
	}
	for _, n := range []string{"default-deny", "dns-egress", "tenant-egress", "tenant-ingress", "public-egress"} {
		if names[n] == nil {
			t.Fatalf("missing policy %s", n)
		}
	}
	blob := fmt.Sprint(names["tenant-egress"], names["tenant-ingress"])
	if strings.Contains(blob, "krp/run") {
		t.Errorf("tenant policy must not select by run label: %s", blob)
	}
	pub := fmt.Sprint(names["public-egress"])
	for _, want := range []string{"0.0.0.0/0", "169.254.0.0/16", "10.0.0.0/8", "100.64.0.0/10", "port:443", "port:80"} {
		if !strings.Contains(pub, want) {
			t.Errorf("public egress lacks %s: %s", want, pub)
		}
	}
}

func Test_unit_runprofile_probe_classifier(t *testing.T) {
	cases := []struct {
		name string
		mode string
		rc   int
		out  string
		want string
	}{
		// plain HTTP (busybox wget)
		{"http success", ProbeHTTP, 0, "", StateOpen},
		{"http server error is still an answer", ProbeHTTP, 1, "wget: server returned error: HTTP/1.1 400 Bad Request", StateOpen},
		{"http 404 from the metadata endpoint is an answer", ProbeHTTP, 1, "wget: server returned error: HTTP/1.1 404 Not Found", StateOpen},
		{"http connection refused", ProbeHTTP, 1, "wget: can't connect to remote host (10.1.2.3): Connection refused", StateClosed},
		// BusyBox reports "download timed out" both when a connect is dropped and
		// when a connection was accepted and the response never arrived, so it
		// proves nothing about reachability: undecided, never closed.
		{"http download timeout is ambiguous (connect dropped or response stalled)", ProbeHTTP, 1, "wget: download timed out", StateError},
		{"http any timeout is ambiguous", ProbeHTTP, 1, "wget: can't connect to remote host (10.1.2.3): Operation timed out", StateError},
		// connect-only TCP (busybox nc -z -v -w)
		{"tcp established", ProbeTCP, 0, "10.1.2.3 (10.1.2.3:8080) open", StateOpen},
		{"tcp established then a stalled peer is still open", ProbeTCP, 0, "kubernetes.default.svc.cluster.local (10.96.0.1:443) open", StateOpen},
		{"tcp connect timeout (dropped)", ProbeTCP, 1, "nc: 10.255.255.1 (10.255.255.1:80): Operation timed out", StateClosed},
		{"tcp refused without a message", ProbeTCP, 1, "", StateClosed},
		{"tcp refused with a message", ProbeTCP, 1, "nc: 10.1.2.3 (10.1.2.3:80): Connection refused", StateClosed},
		{"tcp no route", ProbeTCP, 1, "nc: 10.1.2.3: No route to host", StateClosed},
		{"tcp dns failure", ProbeTCP, 1, "nc: bad address 'srv.ns.svc.cluster.local'", StateError},
		{"tcp tool missing", ProbeTCP, 127, "nc is not installed", StateError},
		{"tcp usage error", ProbeTCP, 1, "BusyBox v1.37.0 multi-call binary.\nUsage: nc [OPTIONS] HOST PORT", StateError},
		{"tcp unknown exit", ProbeTCP, 2, "", StateError},
		{"http no route", ProbeHTTP, 1, "wget: can't connect to remote host (1.2.3.4): No route to host", StateClosed},
		{"http network unreachable", ProbeHTTP, 1, "wget: can't connect to remote host: Network is unreachable", StateClosed},
		{"http dns failure", ProbeHTTP, 1, "wget: bad address 'srv.ns.svc.cluster.local'", StateError},
		{"http connection reset is not decided", ProbeHTTP, 1, "wget: error getting response: Connection reset by peer", StateError},
		{"http unknown failure with a non-zero status", ProbeHTTP, 1, "wget: something unforeseen", StateError},
		{"http tool missing", ProbeHTTP, 127, "sh: wget: not found", StateError},
		{"http empty output non-zero", ProbeHTTP, 1, "", StateError},
		// verified TLS (curl)
		{"tls success", ProbeTLS, 0, "http=301 verify=0", StateOpen},
		{"tls success without verification", ProbeTLS, 0, "http=301 verify=20", StateError},
		{"tls success without a response", ProbeTLS, 0, "http=000 verify=0", StateError},
		{"tls refused", ProbeTLS, 7, "curl: (7) Failed to connect to 1.1.1.1 port 443 after 3 ms: Couldn't connect to server", StateClosed},
		{"tls connect timeout", ProbeTLS, 28, "curl: (28) Connection timed out after 5001 milliseconds", StateClosed},
		{"tls operation timeout after connecting", ProbeTLS, 28, "curl: (28) Operation timed out after 10001 milliseconds with 0 bytes received", StateError},
		{"tls dns failure", ProbeTLS, 6, "curl: (6) Could not resolve host: one.one.one.one", StateError},
		{"tls handshake failure", ProbeTLS, 35, "curl: (35) OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to 1.1.1.1:443", StateError},
		{"tls certificate verification failure", ProbeTLS, 60, "curl: (60) SSL certificate problem: unable to get local issuer certificate", StateError},
		{"tls peer certificate failure", ProbeTLS, 51, "curl: (51) SSL: no alternative certificate subject name matches target host name", StateError},
		{"tls tool missing", ProbeTLS, 127, "curl is not installed", StateError},
		{"tls unknown exit", ProbeTLS, 56, "curl: (56) Recv failure: Connection reset by peer", StateError},
	}
	for _, c := range cases {
		got, reason := ClassifyProbe(c.mode, c.rc, c.out)
		if got != c.want {
			t.Errorf("%s: classified %s (%s), want %s", c.name, got, reason, c.want)
		}
		if got == StateOpen && c.rc != 0 && !strings.Contains(c.out, "HTTP/1.1") {
			t.Errorf("%s: a failing probe without an HTTP answer was open", c.name)
		}
	}
	// Result lines carry the exit status; a missing or garbled one is an error.
	out := "probe\tsame\t0\t\nprobe\tapi\t1\twget: server returned error: HTTP/1.1 400 Bad Request\nprobe\tpublic\t60\tcurl: (60) SSL certificate problem\nprobe\tbad\tx\toops\nnoise\n"
	st, _ := parseProbeLines(out, map[string]string{"same": ProbeHTTP, "api": ProbeHTTP, "public": ProbeTLS, "bad": ProbeHTTP})
	for name, want := range map[string]string{"same": StateOpen, "api": StateOpen, "public": StateError, "bad": StateError} {
		if st[name] != want {
			t.Errorf("parsed %s = %q, want %s", name, st[name], want)
		}
	}
	if _, ok := st["missing"]; ok {
		t.Error("a target without a result line gained an observation")
	}
	// The script reports the exit status and uses a real TLS client for TLS targets.
	for _, want := range []string{"rc=$?", "ssl_verify_result", "--connect-timeout"} {
		if !strings.Contains(probeFn, want) {
			t.Errorf("probe script lacks %q", want)
		}
	}
	// Public HTTPS is probed as HTTPS, never as plaintext to port 443.
	src, err := os.ReadFile("phase_network.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "http://1.1.1.1") || !strings.Contains(string(src), "https://1.1.1.1/") {
		t.Error("public HTTPS target is not an HTTPS URL")
	}
}

func Test_unit_runprofile_probe_errors_never_pass(t *testing.T) {
	for name, mut := range map[string]func(c, a map[string]string){
		"public probe error after policy":  func(c, a map[string]string) { a["public"] = StateError },
		"public control error":             func(c, a map[string]string) { c["public"] = StateError },
		"api control error":                func(c, a map[string]string) { c["api"] = StateError },
		"metadata policy error":            func(c, a map[string]string) { a["metadata"] = StateError },
		"foreign tenant observation error": func(c, a map[string]string) { a["foreign"] = StateError },
	} {
		if st, _ := EvaluateNetwork(netTargets(), netObs(mut)); st != Fail {
			t.Errorf("%s: %s", name, st)
		}
	}
	// A plaintext public probe cannot back the public-HTTPS claim.
	tg := netTargets()
	tg[len(tg)-1].Mode = ProbeHTTP
	if st, d := EvaluateNetwork(tg, netObs(nil)); st != Fail || !strings.Contains(d, "verified TLS") {
		t.Errorf("plaintext public probe: %s %s", st, d)
	}
}

// An accepted connection followed by a stalled response cannot substantiate a
// denial. The probe script runs for real here (POSIX sh) with stub nc and wget
// standing in for a peer that accepts the TCP connection and then never
// answers: the connect-only probe sees the connection, and the HTTP probe's
// BusyBox "download timed out" is undecided.
func Test_unit_runprofile_stalled_peer_is_not_a_denial(t *testing.T) {
	dir := t.TempDir()
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("wget", `echo "wget: download timed out" >&2; exit 1`)
	stub("nc", `echo "$5 ($5:$6) open"; exit 0`) // nc -z -v -w 4 HOST PORT: the connection is accepted
	script := probeFn + `
probe "api via wget" http "http://kubernetes.default.svc.cluster.local:443/"
probe "api via nc" tcp "http://kubernetes.default.svc.cluster.local:443/"
`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe script: %v", err)
	}
	modes := map[string]string{"api via wget": ProbeHTTP, "api via nc": ProbeTCP}
	st, detail := parseProbeLines(string(out), modes)
	if st["api via wget"] != StateError {
		t.Errorf("a stalled HTTP response was classified %s (%s), want %s", st["api via wget"], detail["api via wget"], StateError)
	}
	if st["api via nc"] != StateOpen {
		t.Errorf("an accepted connection was classified %s (%s), want %s", st["api via nc"], detail["api via nc"], StateOpen)
	}
	// Neither can back a deny claim: the connect-only probe shows the endpoint
	// reachable (denial not substantiated) and the stalled HTTP probe is an error.
	for _, mode := range []string{ProbeHTTP, ProbeTCP} {
		name := map[string]string{ProbeHTTP: "api via wget", ProbeTCP: "api via nc"}[mode]
		tg := netTargets()
		for i := range tg {
			if tg[i].Claim == ClaimAPI {
				tg[i].Name, tg[i].Mode = name, mode
			}
		}
		c := map[string]string{"same": "open", "other-run": "open", "foreign": "open", "metadata": "open", "public": "open", name: "open"}
		a := map[string]string{"same": "open", "other-run": "open", "foreign": "closed", "metadata": "closed", "public": "open", name: st[name]}
		got, d := EvaluateNetwork(tg, NetObservations{Control: c, After: a})
		if got != Fail {
			t.Errorf("%s: a stalled/accepted API connection produced %s (%s), not a failure", name, got, d)
		}
	}
	// A deny claim backed by an HTTP-mode probe is refused outright.
	tg := netTargets()
	for i := range tg {
		if tg[i].Kind == DenyProbe || tg[i].Kind == DenyCore {
			tg[i].Mode = ProbeHTTP
		}
	}
	if got, d := EvaluateNetwork(tg, netObs(nil)); got != Fail || !strings.Contains(d, "connect-only") {
		t.Errorf("HTTP-mode deny claims were accepted: %s %s", got, d)
	}
	// The driver's own targets: every deny claim is connect-only.
	src, _ := os.ReadFile("phase_network.go")
	for _, claim := range []string{"ClaimForeignTenant, ProbeTCP", "ClaimAPI, ProbeTCP", "ClaimMetadata, ProbeTCP"} {
		if !strings.Contains(string(src), claim) {
			t.Errorf("target for %s is not connect-only", claim)
		}
	}
}

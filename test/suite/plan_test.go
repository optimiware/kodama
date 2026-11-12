package suite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func noEnvironment(string) string { return "" }

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// laneNamed returns one lane of a plan or fails.
func laneNamed(t *testing.T, plan Plan, name string) Lane {
	t.Helper()
	for _, wave := range plan.Waves {
		for _, lane := range wave.Lanes {
			if lane.Name == name {
				return lane
			}
		}
	}
	t.Fatalf("the plan has no lane %q: %+v", name, plan)
	return Lane{}
}

func laneNames(wave Wave) []string {
	names := make([]string, 0, len(wave.Lanes))
	for _, lane := range wave.Lanes {
		names = append(names, lane.Name)
	}
	return names
}

// value returns the last setting of one variable in a lane environment, the
// one a child process observes.
func value(lane Lane, name string) (string, bool) {
	found, ok := "", false
	for _, entry := range lane.Env {
		if rest, matches := strings.CutPrefix(entry, name+"="); matches {
			found, ok = rest, true
		}
	}
	return found, ok
}

func requireValue(t *testing.T, lane Lane, name, want string) {
	t.Helper()
	if got, ok := value(lane, name); !ok || got != want {
		t.Errorf("lane %s: %s = %q (set %v); want %q", lane.Name, name, got, ok, want)
	}
}

// flagValue returns the argument following one command-line flag.
func flagValue(lane Lane, flag string) string {
	if index := slices.Index(lane.Argv, flag); index >= 0 && index+1 < len(lane.Argv) {
		return lane.Argv[index+1]
	}
	return ""
}

// requireVerifiedGoTest asserts that lane.Argv[offset:] wraps go test in
// verify-go-test.sh with the exact expected test list and -json output.
func requireVerifiedGoTest(t *testing.T, lane Lane, offset int, expected []string) {
	t.Helper()
	wantPrefix := make([]string, 0, 2+2*len(expected))
	wantPrefix = append(wantPrefix, testVerifier)
	for _, name := range expected {
		wantPrefix = append(wantPrefix, "--expect", name)
	}
	wantPrefix = append(wantPrefix, "--", "go", "test")
	if len(lane.Argv) < offset+len(wantPrefix) || !slices.Equal(lane.Argv[offset:offset+len(wantPrefix)], wantPrefix) {
		t.Fatalf("lane %s argv[%d:] = %v; want prefix %v", lane.Name, offset, lane.Argv[offset:], wantPrefix)
	}
	if !slices.Contains(lane.Argv[offset+len(wantPrefix):], "-json") {
		t.Errorf("lane %s verified go test lacks -json: %v", lane.Name, lane.Argv)
	}
}

// writeDescriptor writes one stage installation descriptor and the private
// token file its environment names.
func writeDescriptor(t *testing.T, mutate func(map[string]any)) (path string) {
	t.Helper()
	document := map[string]any{
		"schema_version": 1,
		"revision":       "3b438282",
		"tenant_id":      "tenant-a",
		"project_id":     "project-a",
		"api": map[string]any{
			"url":             "https://127.0.0.1:18443",
			"ca_file":         "/run/stage/ca.pem",
			"tls_server_name": "kodama.kodama-system.svc.cluster.local",
		},
		"images": map[string]any{"tools": "registry.test/tools@sha256:" + strings.Repeat("a", 64)},
		"oauthsonas": map[string]any{
			"issuer":           "https://oauthsonas.kodama-system.svc.cluster.local:8443",
			"ca_file":          "/run/stage/ca.pem",
			"client_id":        "kodactl",
			"redirect_uri":     "http://127.0.0.1:52790/callback",
			"resolve":          "oauthsonas.kodama-system.svc.cluster.local:8443:127.0.0.1",
			"access_code_file": "/run/stage/access-code",
		},
		"git": map[string]any{"repositories": []map[string]any{
			{"name": "admin"}, {"name": "smoke/first"},
		}},
		"smoke_repository": map[string]any{"name": "smoke/selected"},
	}
	if mutate != nil {
		mutate(document)
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the descriptor: %v", err)
	}
	path = filepath.Join(t.TempDir(), "installation.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write the descriptor: %v", err)
	}
	return path
}

func stageEnvironmentValues() map[string]string {
	return map[string]string{
		"Q01_KUBE_CONTEXT":             "admin@kodama-stage-k8s",
		"Q01_ADMIN_TOKEN_FILE":         "/run/stage/admin-token",
		"Q01_HARNESS_REVISION":         "53b9ede8",
		"KODAMA_Q03_STAGE_INSTANCE_ID": "11111111-2222-3333-4444-555555555555",
	}
}

// TestStagePlanOrdersWavesByBlastRadius proves the stage plan: the portable
// proofs, the non-disruptive probes and the RDB drill share the first wave, the
// chaos probes run alone and one at a time in the second, and both dialects
// derive from the one descriptor.
func TestStagePlanOrdersWavesByBlastRadius(t *testing.T) {
	options := Options{
		Target:               TargetStage,
		Installation:         writeDescriptor(t, nil),
		EvidenceDir:          "/evidence",
		Race:                 true,
		Q03Mode:              "restore",
		SecondAdminTokenFile: "/run/suite/second-admin.token",
	}
	plan, err := BuildPlan(options, environment(stageEnvironmentValues()))
	if err != nil {
		t.Fatalf("BuildPlan(stage): %v", err)
	}
	if len(plan.Waves) != 2 {
		t.Fatalf("stage plan has %d waves; want 2", len(plan.Waves))
	}
	if got, want := laneNames(plan.Waves[0]), []string{"stage-e2e", "stage-probes", "stage-rdb-drill"}; !slices.Equal(got, want) {
		t.Fatalf("wave 1 lanes = %v; want %v", got, want)
	}
	if got, want := laneNames(plan.Waves[1]), []string{"stage-chaos"}; !slices.Equal(got, want) {
		t.Fatalf("wave 2 lanes = %v; want %v", got, want)
	}

	e2e := laneNamed(t, plan, "stage-e2e")
	requireVerifiedGoTest(t, e2e, 0, PortableE2ETests)
	if got := flagValue(e2e, "-run"); got != PortableE2ERunPattern {
		t.Errorf("stage-e2e selects %q; want the portable pattern", got)
	}
	if got := flagValue(e2e, "-parallel"); got != "6" {
		t.Errorf("stage-e2e -parallel = %q; want the scenario concurrency 6", got)
	}
	requireValue(t, e2e, "Q01_INSTALLATION_FILE", options.Installation)
	requireValue(t, e2e, "Q01_REQUIRE_INSTALLATION", "1")
	requireValue(t, e2e, "Q01_SMOKE_GIT", "1")
	requireValue(t, e2e, "Q01_EVIDENCE_DIR", "/evidence/stage-e2e")
	requireValue(t, e2e, "KODAMA_E2E_CONCURRENCY", "6")

	probes := laneNamed(t, plan, "stage-probes")
	requireVerifiedGoTest(t, probes, 0, StageWave1Probes)
	if got := flagValue(probes, "-run"); got != StageWave1RunPattern() {
		t.Errorf("stage-probes selects %q; want wave 1", got)
	}
	for name, want := range map[string]string{
		"Q02_REQUIRE_STAGE":           "1",
		"Q02_API_URL":                 "https://127.0.0.1:18443",
		"Q02_CA_PEM":                  "/run/stage/ca.pem",
		"Q02_TLS_SERVER_NAME":         "kodama.kodama-system.svc.cluster.local",
		"Q02_PROJECT_ID":              "project-a",
		"Q02_TENANT_ID":               "tenant-a",
		"Q02_TENANT_NAMESPACE":        "kodama-tenant-tenant-a",
		"Q02_GIT_SERVICE_A":           "kodama-git-tenant-a",
		"Q02_GIT_REPOSITORY_A":        "smoke/selected",
		"Q02_KUBE_CONTEXT":            "admin@kodama-stage-k8s",
		"Q02_DEPLOYED_REVISION":       "3b438282",
		"Q02_HARNESS_REVISION":        "53b9ede8",
		"Q02_ADMIN_TOKEN_FILE":        "/run/stage/admin-token",
		"Q02_SECOND_ADMIN_TOKEN_FILE": "/run/suite/second-admin.token",
		"Q02_EVIDENCE_DIR":            "/evidence/stage-probes",
		"KODAMA_STAGE_CONCURRENCY":    "6",
	} {
		requireValue(t, probes, name, want)
	}

	drill := laneNamed(t, plan, "stage-rdb-drill")
	if got, want := drill.Argv, []string{"scripts/q03-stage-drill", "restore"}; !slices.Equal(got, want) {
		t.Errorf("stage-rdb-drill command = %v; want %v", got, want)
	}
	requireValue(t, drill, "KODAMA_Q03_EVIDENCE_DIR", "/evidence/stage-rdb-drill")
	requireValue(t, drill, "KODAMA_Q03_SOURCE_REVISION", "3b438282")

	chaos := laneNamed(t, plan, "stage-chaos")
	requireVerifiedGoTest(t, chaos, 0, StageWave2ChaosProbes)
	if got := flagValue(chaos, "-run"); got != StageWave2ChaosRunPattern() {
		t.Errorf("stage-chaos selects %q; want wave 2", got)
	}
	if got := flagValue(chaos, "-parallel"); got != "1" {
		t.Errorf("stage-chaos -parallel = %q; want 1", got)
	}
	requireValue(t, chaos, "Q02_EVIDENCE_DIR", "/evidence/stage-chaos")
	if _, ok := value(chaos, "KODAMA_STAGE_CONCURRENCY"); ok {
		t.Error("stage-chaos sets a wave 1 concurrency")
	}

	// The stage lanes run inside the stage lock; none takes a slot of its own.
	for _, wave := range plan.Waves {
		for _, lane := range wave.Lanes {
			if lane.Argv[0] == slotLauncher {
				t.Errorf("stage lane %s takes its own slot: %v", lane.Name, lane.Argv)
			}
		}
	}
}

// TestStagePlanKeepsExportedInputs proves an input the caller exported is
// never overridden: an exported token value suppresses the token file, and the
// second tenant's settings are not derived at all.
func TestStagePlanKeepsExportedInputs(t *testing.T) {
	values := stageEnvironmentValues()
	values["Q02_ADMIN_TOKEN"] = "exported-admin-token-value"
	values["Q02_TENANT_NAMESPACE"] = "custom-namespace"
	values["Q02_KUBE_CONTEXT"] = "probe-context"
	values["Q02_REQUIRE_STAGE"] = "0"
	values["KODAMA_Q03_EVIDENCE_DIR"] = "/elsewhere/q03"
	values["KODAMA_Q03_SOURCE_REVISION"] = "exported-revision"
	plan, err := BuildPlan(Options{
		Target:       TargetStage,
		Installation: writeDescriptor(t, func(document map[string]any) { delete(document, "smoke_repository") }),
		EvidenceDir:  "/evidence",
	}, environment(values))
	if err != nil {
		t.Fatalf("BuildPlan(stage): %v", err)
	}
	probes := laneNamed(t, plan, "stage-probes")
	for _, name := range []string{"Q02_ADMIN_TOKEN_FILE", "Q02_TENANT_NAMESPACE", "Q02_KUBE_CONTEXT", "Q02_REQUIRE_STAGE", "Q02_SECOND_ADMIN_TOKEN_FILE"} {
		if got, ok := value(probes, name); ok {
			t.Errorf("stage-probes derives %s=%q although the caller exported it or its token", name, got)
		}
	}
	requireValue(t, probes, "Q02_GIT_REPOSITORY_A", "smoke/first")
	if got, ok := value(laneNamed(t, plan, "stage-rdb-drill"), "KODAMA_Q03_EVIDENCE_DIR"); ok {
		t.Errorf("stage-rdb-drill overrides the exported evidence directory with %q", got)
	}
	if got, ok := value(laneNamed(t, plan, "stage-rdb-drill"), "KODAMA_Q03_SOURCE_REVISION"); ok {
		t.Errorf("stage-rdb-drill overrides the exported source revision with %q", got)
	}
	if got := laneNamed(t, plan, "stage-rdb-drill").Argv[1]; got != "plan" {
		t.Errorf("default RDB drill mode = %q; want plan", got)
	}
	for _, wave := range plan.Waves {
		for _, lane := range wave.Lanes {
			if rendered := strings.Join(append(lane.Argv, lane.Env...), " "); strings.Contains(rendered, "exported-admin-token-value") {
				t.Errorf("lane %s carries a token value: %s", lane.Name, rendered)
			}
		}
	}
}

// TestStagePlanRefusesASilentlyNarrowedRun proves a stage run never shrinks on
// its own: a missing descriptor, cluster context or drill input is an error,
// and only an explicit flag leaves a part out.
func TestStagePlanRefusesASilentlyNarrowedRun(t *testing.T) {
	installation := writeDescriptor(t, nil)
	without := func(name string) func(string) string {
		values := stageEnvironmentValues()
		delete(values, name)
		return environment(values)
	}
	for _, refused := range []struct {
		name    string
		options Options
		getenv  func(string) string
		message string
	}{
		{"no descriptor", Options{}, environment(stageEnvironmentValues()), "installation descriptor"},
		{"no cluster context", Options{Installation: installation}, without("Q01_KUBE_CONTEXT"), "Q01_KUBE_CONTEXT"},
		{"no drill input", Options{Installation: installation}, without("KODAMA_Q03_STAGE_INSTANCE_ID"), "--skip-q03"},
		{"unknown drill mode", Options{Installation: installation, Q03Mode: "apply"}, environment(stageEnvironmentValues()), "Q03 mode"},
		{"unsupported descriptor", Options{Installation: writeDescriptor(t, func(document map[string]any) { document["schema_version"] = 2 })},
			environment(stageEnvironmentValues()), "schema_version"},
	} {
		refused.options.Target = TargetStage
		refused.options.EvidenceDir = "/evidence"
		if _, err := BuildPlan(refused.options, refused.getenv); err == nil || !strings.Contains(err.Error(), refused.message) {
			t.Errorf("%s: BuildPlan error = %v; want one naming %q", refused.name, err, refused.message)
		}
	}

	plan, err := BuildPlan(Options{
		Target: TargetStage, Installation: installation, EvidenceDir: "/evidence", SkipQ03: true, SkipChaos: true,
	}, without("KODAMA_Q03_STAGE_INSTANCE_ID"))
	if err != nil {
		t.Fatalf("BuildPlan with explicit skips: %v", err)
	}
	if len(plan.Waves) != 1 || !slices.Equal(laneNames(plan.Waves[0]), []string{"stage-e2e", "stage-probes"}) {
		t.Fatalf("explicitly narrowed stage plan = %+v; want one wave with the e2e and probe lanes", plan.Waves)
	}
}

// TestPodmanPlanSelectsAnInstallation proves the podman target runs the
// portable proofs against the described installation, or against a disposable
// one when the caller supplied its port set, and refuses to run without either.
func TestPodmanPlanSelectsAnInstallation(t *testing.T) {
	existing, err := BuildPlan(Options{
		Target: TargetPodman, Installation: "/state/installation.json", EvidenceDir: "/evidence", Race: true,
	}, noEnvironment)
	if err != nil {
		t.Fatalf("BuildPlan(podman, existing): %v", err)
	}
	if got, want := laneNames(existing.Waves[0]), []string{"portable-e2e-podman", "qualification-restore-local"}; !slices.Equal(got, want) {
		t.Fatalf("podman lanes = %v; want %v", got, want)
	}
	lane := laneNamed(t, existing, "portable-e2e-podman")
	if !slices.Equal(lane.Argv[:3], []string{slotLauncher, "stack", "--"}) || !slices.Contains(lane.Argv, "-race") {
		t.Errorf("portable-e2e-podman command = %v; want a raced go test in a stack slot", lane.Argv)
	}
	requireVerifiedGoTest(t, lane, 3, PortableE2ETests)
	requireValue(t, lane, "Q01_SMOKE_GIT", "1")
	requireValue(t, lane, "Q01_INSTALLATION_FILE", "/state/installation.json")
	requireValue(t, lane, "KODAMA_EVIDENCE_DIR", "/evidence/portable-e2e-podman")
	requireValue(t, lane, "KODAMA_E2E_CONCURRENCY", "8")
	requireValue(t, laneNamed(t, existing, "qualification-restore-local"), "KODAMA_EVIDENCE_DIR", "/evidence/qualification-restore-local")

	ports := map[string]string{}
	for index, name := range podmanPortEnv {
		ports[name] = []string{"26101", "26102", "26103", "26104", "26105", "26106", "26107", "26108"}[index]
	}
	disposable, err := BuildPlan(Options{Target: TargetPodman, EvidenceDir: "/evidence", SkipQ03: true}, environment(ports))
	if err != nil {
		t.Fatalf("BuildPlan(podman, disposable): %v", err)
	}
	lane = laneNamed(t, disposable, "portable-e2e-podman")
	if want := []string{slotLauncher, "stack", "--", "deploy/podman/smoke.sh"}; !slices.Equal(lane.Argv, want) {
		t.Errorf("disposable lane command = %v; want %v", lane.Argv, want)
	}
	requireValue(t, lane, "Q01_SMOKE_GIT", "1")
	requireValue(t, lane, "KODAMA_SMOKE_TEST_RUN", PortableE2ERunPattern)
	requireValue(t, lane, "KODAMA_SMOKE_EXPECT", strings.Join(PortableE2ETests, " "))
	requireValue(t, lane, "KODAMA_SMOKE_SHARD_REPOS", "2")
	requireValue(t, lane, "KODAMA_EVIDENCE_DIR", "/evidence/portable-e2e-podman")
	requireValue(t, lane, "Q01_NATS_URL", "nats://127.0.0.1:26106")
	if len(disposable.Waves[0].Lanes) != 1 {
		t.Errorf("--skip-q03 kept the restore lane: %v", laneNames(disposable.Waves[0]))
	}

	delete(ports, "KODAMA_LOCAL_GIT_PORT")
	_, err = BuildPlan(Options{Target: TargetPodman, EvidenceDir: "/evidence"}, environment(ports))
	if err == nil || !strings.Contains(err.Error(), "KODAMA_LOCAL_GIT_PORT") || strings.Contains(err.Error(), "KODAMA_LOCAL_NATS_PORT") {
		t.Errorf("incomplete port set error = %v; want one naming only KODAMA_LOCAL_GIT_PORT", err)
	}
}

// TestKindPlanSelectsTheLocalSuitesOrTheInstallation proves the kind target.
func TestKindPlanSelectsTheLocalSuitesOrTheInstallation(t *testing.T) {
	local, err := BuildPlan(Options{Target: TargetKind, EvidenceDir: "/evidence"}, noEnvironment)
	if err != nil {
		t.Fatalf("BuildPlan(kind): %v", err)
	}
	if got, want := laneNames(local.Waves[0]), []string{"integration-lifecycle-kind", "component-kubernetes-deploy"}; !slices.Equal(got, want) {
		t.Fatalf("kind lanes = %v; want %v", got, want)
	}
	lifecycle := laneNamed(t, local, "integration-lifecycle-kind")
	if !slices.Equal(lifecycle.Argv[:3], []string{slotLauncher, "kind", "--"}) || flagValue(lifecycle, "-run") != "^TestKubernetes" {
		t.Errorf("integration-lifecycle-kind command = %v; want ^TestKubernetes in a kind slot", lifecycle.Argv)
	}
	requireVerifiedGoTest(t, lifecycle, 3, KindLifecycleTests)
	requireValue(t, lifecycle, "KODAMA_EVIDENCE_DIR", "/evidence/integration-lifecycle-kind")
	requireValue(t, laneNamed(t, local, "component-kubernetes-deploy"), "KODAMA_EVIDENCE_DIR", "/evidence/component-kubernetes-deploy")

	installed, err := BuildPlan(Options{Target: TargetKind, Installation: "/kind/installation.json", EvidenceDir: "/evidence"}, noEnvironment)
	if err != nil {
		t.Fatalf("BuildPlan(kind, installation): %v", err)
	}
	if got, want := laneNames(installed.Waves[0]), []string{"portable-e2e-kind"}; !slices.Equal(got, want) {
		t.Fatalf("kind installation lanes = %v; want %v", got, want)
	}
	portableKind := laneNamed(t, installed, "portable-e2e-kind")
	requireVerifiedGoTest(t, portableKind, 3, PortableE2ETests)
	requireValue(t, portableKind, "Q01_SMOKE_GIT", "1")
	requireValue(t, portableKind, "KODAMA_EVIDENCE_DIR", "/evidence/portable-e2e-kind")
}

// TestLocalAllSplitsTheLifecyclePackageByRuntime proves the lifecycle package
// runs once per runtime, each half in its own pool.
func TestLocalAllSplitsTheLifecyclePackageByRuntime(t *testing.T) {
	plan, err := BuildPlan(Options{Target: TargetLocalAll, EvidenceDir: "/evidence", Race: true}, environment(map[string]string{"Q01_DEPLOYED_REVISION": "deadbeef"}))
	if err != nil {
		t.Fatalf("BuildPlan(local-all): %v", err)
	}
	if len(plan.Waves) != 1 || len(plan.Waves[0].Lanes) != 9 {
		t.Fatalf("local-all = %d wave(s), lanes %v; want one wave of 9 lanes", len(plan.Waves), laneNames(plan.Waves[0]))
	}
	for _, lane := range plan.Waves[0].Lanes {
		requireValue(t, lane, "KODAMA_EVIDENCE_DIR", "/evidence/"+lane.Name)
	}
	requireValue(t, laneNamed(t, plan, "qualification-restore-local"), "KODAMA_Q03_SOURCE_REVISION", "deadbeef")
	podman := laneNamed(t, plan, "integration-lifecycle-podman")
	if podman.Argv[1] != "stack" || flagValue(podman, "-skip") != "^TestKubernetes" {
		t.Errorf("integration-lifecycle-podman command = %v; want -skip ^TestKubernetes in a stack slot", podman.Argv)
	}
	kind := laneNamed(t, plan, "integration-lifecycle-kind")
	if kind.Argv[1] != "kind" || flagValue(kind, "-run") != "^TestKubernetes" {
		t.Errorf("integration-lifecycle-kind command = %v; want -run ^TestKubernetes in a kind slot", kind.Argv)
	}
	requireVerifiedGoTest(t, kind, 3, KindLifecycleTests)
	skipped, err := BuildPlan(Options{Target: TargetLocalAll, EvidenceDir: "/evidence", SkipQ03: true}, noEnvironment)
	if err != nil || len(skipped.Waves[0].Lanes) != 8 {
		t.Errorf("--skip-q03 local-all = %v lanes (error %v); want 8", laneNames(skipped.Waves[0]), err)
	}
}

// TestConcurrencyOverrideStaysWithinEachPackageBound proves one --concurrency
// value never reaches a package above the maximum that package accepts.
func TestConcurrencyOverrideStaysWithinEachPackageBound(t *testing.T) {
	plan, err := BuildPlan(Options{Target: TargetLocalAll, EvidenceDir: "/evidence", Concurrency: 32}, noEnvironment)
	if err != nil {
		t.Fatalf("BuildPlan(local-all, concurrency 32): %v", err)
	}
	requireValue(t, laneNamed(t, plan, "integration-lifecycle-kind"), "KODAMA_LIFECYCLE_KIND_CONCURRENCY", "6")
	requireValue(t, laneNamed(t, plan, "qualification-restore-local"), "KODAMA_Q03_CONCURRENCY", "8")

	stage, err := BuildPlan(Options{
		Target: TargetStage, Installation: writeDescriptor(t, nil), EvidenceDir: "/evidence", Concurrency: 32,
	}, environment(stageEnvironmentValues()))
	if err != nil {
		t.Fatalf("BuildPlan(stage, concurrency 32): %v", err)
	}
	requireValue(t, laneNamed(t, stage, "stage-e2e"), "KODAMA_E2E_CONCURRENCY", "32")
	requireValue(t, laneNamed(t, stage, "stage-probes"), "KODAMA_STAGE_CONCURRENCY", "24")
	if got := flagValue(laneNamed(t, stage, "stage-probes"), "-parallel"); got != "24" {
		t.Errorf("stage-probes -parallel = %q; want 24", got)
	}

	serial, err := BuildPlan(Options{Target: TargetLocalAll, EvidenceDir: "/evidence", Concurrency: 1}, noEnvironment)
	if err != nil {
		t.Fatalf("BuildPlan(local-all, concurrency 1): %v", err)
	}
	requireValue(t, laneNamed(t, serial, "integration-lifecycle-kind"), "KODAMA_LIFECYCLE_KIND_CONCURRENCY", "1")
}

func TestBuildPlanRefusesInvalidOptions(t *testing.T) {
	for _, refused := range []struct {
		name    string
		options Options
	}{
		{"unknown target", Options{Target: "cloud", EvidenceDir: "/evidence"}},
		{"negative concurrency", Options{Target: TargetLocalAll, EvidenceDir: "/evidence", Concurrency: -1}},
		{"excessive concurrency", Options{Target: TargetLocalAll, EvidenceDir: "/evidence", Concurrency: MaxConcurrency + 1}},
		{"relative evidence directory", Options{Target: TargetLocalAll, EvidenceDir: "evidence"}},
		{"no evidence directory", Options{Target: TargetLocalAll}},
	} {
		if _, err := BuildPlan(refused.options, noEnvironment); err == nil {
			t.Errorf("%s was accepted", refused.name)
		}
	}
}

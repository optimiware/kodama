package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"example.invalid/inventory/internal/delivery"
	"example.invalid/inventory/test/support/clusterfake"
	"example.invalid/inventory/test/support/factoryfake"
)

const (
	scriptDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	scriptCommit = "1111111111111111111111111111111111111111"
)

// scriptRun is one execution of an ordinary ci script under the step
// entrypoint's contract: explicit variables, an output root, a results file and a
// secrets directory.
type scriptRun struct {
	t        *testing.T
	root     string // the application source (the step's workspace)
	outputs  string
	results  string
	secrets  string
	token    string
	kubectl  string
	state    string
	registry *httptest.Server
	repo     string
}

func appRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..")
}

func newScriptRun(t *testing.T) *scriptRun {
	t.Helper()
	root := appRoot(t)
	dir := t.TempDir()
	r := &scriptRun{t: t, root: root, outputs: filepath.Join(dir, "outputs"), results: filepath.Join(dir, "results.json"), secrets: filepath.Join(dir, "secrets"),
		token: filepath.Join(dir, "token"), state: filepath.Join(dir, "kube-state"), kubectl: filepath.Join(dir, "kubectl")}
	for _, d := range []string{r.outputs, r.secrets, r.state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(r.token, []byte("runtime-token-0123456789abcdef\n"), 0o600)
	build := exec.Command("go", "build", "-o", r.kubectl, "./test/support/fakekubectl")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the kubectl double: %v\n%s", err, out)
	}
	// A registry double that holds exactly the image digest under test.
	r.registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == http.MethodHead && strings.HasSuffix(q.URL.Path, "/manifests/"+scriptDigest) {
			w.Header().Set("Docker-Content-Digest", scriptDigest)
			return
		}
		http.NotFound(w, q)
	}))
	t.Cleanup(r.registry.Close)
	r.repo = strings.TrimPrefix(r.registry.URL, "http://") + "/acme/inventory"
	return r
}

func (r *scriptRun) env(extra map[string]string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir(), "GOCACHE=" + os.Getenv("GOCACHE"), "GOMODCACHE=" + os.Getenv("GOMODCACHE"),
		"GOFLAGS=-mod=readonly", "GOPROXY=off",
		"KODAMA_OUTPUTS_DIR=" + r.outputs, "KODAMA_RESULTS_FILE=" + r.results, "KODAMA_SECRETS_DIR=" + r.secrets,
		"KODAMA_RUNTIME_TOKEN_FILE=" + r.token, "KUBECTL=" + r.kubectl, "FAKEKUBECTL_STATE=" + r.state, "REGISTRY_INSECURE=true"}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func (r *scriptRun) run(script string, extra map[string]string) (string, error) {
	r.t.Helper()
	_ = os.Remove(r.results)
	cmd := exec.Command("sh", filepath.Join("ci", script))
	cmd.Dir = r.root
	cmd.Env = r.env(extra)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *scriptRun) manifest() map[string]any {
	r.t.Helper()
	raw, err := os.ReadFile(r.results)
	if err != nil {
		r.t.Fatalf("the script wrote no results manifest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		r.t.Fatalf("results manifest is not JSON: %s", raw)
	}
	return m
}

// scenario wires a factory double whose pinned context matches a snapshot.
type scenario struct {
	fac     *factoryfake.Server
	snap    delivery.Snapshot
	kubecfg string
}

func (r *scriptRun) scenario(kind, namespace, attempt, actionKey string, mutate func(*delivery.Snapshot)) (*scenario, map[string]string) {
	r.t.Helper()
	log := &clusterfake.Timeline{}
	fac := factoryfake.New(r.t, log)
	snap := delivery.Snapshot{
		Format: delivery.SnapshotFormat, Tenant: "t_acme", Project: "p_inventory", Environment: "env_" + kind, Kind: kind, Target: "kind", Namespace: namespace,
		Config: map[string]string{"OIDC_ISSUER": "https://idp.example.invalid", "OIDC_AUDIENCE": "inventory", "ERP_BASE_URL": "https://erp.example.invalid",
			"WORKER_TENANT": "11111111-1111-4111-8111-111111111111", "WORKER_SERVICE_ID": "svc-worker", "DATABASE_RUNTIME_ROLE": "inventory_runtime",
			"REPLICAS": map[string]string{"preview": "1", "stage": "2"}[kind], "APP_URL": "http://inventory." + kind + ".example.invalid"},
		Kubeconfig: "KUBECONFIG",
		Secrets: []delivery.Binding{
			{Ref: "secret://project/kubeconfig", Version: 1, Name: "KUBECONFIG", Delivery: "file"},
			{Ref: "secret://project/db-runtime-" + kind, Version: 1, Name: "DATABASE_RUNTIME_URL", Delivery: "file"},
			{Ref: "secret://project/db-migrate-" + kind, Version: 1, Name: "DATABASE_MIGRATION_URL", Delivery: "file"},
			{Ref: "secret://project/cursor-" + kind, Version: 1, Name: "CURSOR_KEY", Delivery: "file"},
			{Ref: "secret://project/erp-" + kind, Version: 1, Name: "ERP_TOKEN", Delivery: "file"},
		},
	}
	if kind == "preview" {
		snap.TTLSeconds = 86400
	}
	if mutate != nil {
		mutate(&snap)
	}
	dig, _ := snap.Digest()
	raw, _ := json.Marshal(snap)
	var bindings []map[string]any
	for _, b := range snap.Secrets {
		bindings = append(bindings, map[string]any{"ref": b.Ref, "version": b.Version, "name": b.Name, "delivery": b.Delivery})
		content := "value-of-" + b.Name + "\n"
		if b.Name == "KUBECONFIG" {
			content = "namespace=" + snap.Namespace + "\n"
		}
		_ = os.WriteFile(filepath.Join(r.secrets, b.Name), []byte(content), 0o600)
	}
	fac.SetContext(attempt, map[string]any{
		"tenantId": snap.Tenant, "projectId": snap.Project, "runId": "run_" + kind, "stepId": kind, "attemptId": attempt, "actionKey": actionKey,
		"command":   map[string]any{"image": "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		"workspace": map[string]any{"access": "read", "source": map[string]any{"repositoryUrl": "https://git.example.invalid/acme/inventory.git", "commit": scriptCommit, "branch": nil, "expectedTip": nil}},
		"inputs":    map[string]any{"image": "art_image_1", "image_digest": scriptDigest, "image_repository": r.repo, "environment_snapshot": string(raw)},
		"secrets":   bindings, "environment": map[string]any{"id": snap.Environment, "configDigest": dig},
		"x-namespace": snap.Namespace, "x-kind": kind,
	})
	return &scenario{fac: fac, snap: snap}, map[string]string{
		"KODAMA_RUNTIME_URL": fac.URL(), "KODAMA_ATTEMPT_ID": attempt, "KODAMA_INPUT_ENVIRONMENT_SNAPSHOT": string(raw),
	}
}

// TestDeliveryScripts runs the generated deploy scripts exactly as the step
// entrypoint would, against the factory callback double, a registry double and a
// kubectl double, and keeps the evidence for contract validation when
// INVENTORY_SCRIPT_EVIDENCE_DIR is set.
func TestDeliveryScripts(t *testing.T) {
	evidence := os.Getenv("INVENTORY_SCRIPT_EVIDENCE_DIR")
	keep := func(t *testing.T, r *scriptRun, name string) {
		if evidence == "" {
			return
		}
		dst := filepath.Join(evidence, name)
		_ = os.MkdirAll(dst, 0o700)
		for _, f := range []string{r.results, filepath.Join(r.outputs, "deployment.json"), filepath.Join(r.outputs, "e2e.json")} {
			if raw, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dst, filepath.Base(f)), raw, 0o600)
			}
		}
	}

	t.Run("preview and stage promote one image digest with distinct explicit configuration", func(t *testing.T) {
		var deployments = map[string]delivery.Record{}
		for _, kind := range []string{"preview", "stage"} {
			r := newScriptRun(t)
			sc, env := r.scenario(kind, "acme-"+kind, "att_"+kind, "run_"+kind+":"+kind, nil)
			script := "deploy-" + kind + ".sh"
			out, err := r.run(script, env)
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", script, err, out)
			}
			m := r.manifest()
			outputs, _ := m["outputs"].(map[string]any)
			if m["outcome"] != "completed" || outputs["deployment_id"] == "" || outputs["app_url"] != "http://inventory."+kind+".example.invalid" {
				t.Fatalf("%v", m)
			}
			rec, ok := sc.fac.Record("run_" + kind + ":" + kind)
			if !ok || rec.Status != "succeeded" || rec.Image.Digest != scriptDigest || len(rec.Objects) != 7 {
				t.Fatalf("%+v", rec)
			}
			ev, err := os.ReadFile(filepath.Join(r.outputs, "deployment.json"))
			if err != nil || !strings.Contains(string(ev), scriptCommit) || !strings.Contains(string(ev), "registry.example.invalid/tools@sha256:") {
				t.Fatalf("the evidence artifact identifies source and tool image: %s %v", ev, err)
			}
			deployments[kind] = rec
			keep(t, r, kind)
		}
		p, s := deployments["preview"], deployments["stage"]
		if p.Image.Digest != s.Image.Digest {
			t.Error("stage must promote the preview's image digest")
		}
		if p.ConfigurationDigest == s.ConfigurationDigest || p.ID == s.ID || p.Namespace == s.Namespace {
			t.Errorf("distinct explicit configuration must give distinct identities: %+v %+v", p, s)
		}
	})

	t.Run("a snapshot that does not match the pinned digest is refused with a failed result and no effect", func(t *testing.T) {
		r := newScriptRun(t)
		sc, env := r.scenario("preview", "acme-preview", "att_bad", "run_bad:preview", nil)
		tampered := sc.snap
		tampered.Config = map[string]string{}
		for k, v := range sc.snap.Config {
			tampered.Config[k] = v
		}
		tampered.Config["ERP_BASE_URL"] = "https://erp.attacker.example.invalid"
		raw, _ := json.Marshal(tampered)
		env["KODAMA_INPUT_ENVIRONMENT_SNAPSHOT"] = string(raw)
		out, err := r.run("deploy-preview.sh", env)
		if err == nil {
			t.Fatalf("expected a nonzero exit:\n%s", out)
		}
		if m := r.manifest(); m["outcome"] != "failed" || m["reason"] != "snapshot_mismatch" {
			t.Fatalf("%v", m)
		}
		if sc.fac.Calls["register"] != 0 {
			t.Fatal("nothing may be registered for a mismatched snapshot")
		}
		if entries, _ := os.ReadDir(r.state); len(entries) != 0 {
			t.Fatal("nothing may be applied")
		}
		keep(t, r, "mismatch")
	})

	t.Run("a revoked attempt stops with no effect", func(t *testing.T) {
		r := newScriptRun(t)
		sc, env := r.scenario("preview", "acme-preview", "att_rev", "run_rev:preview", nil)
		sc.fac.Revoke("att_rev")
		if _, err := r.run("deploy-preview.sh", env); err == nil {
			t.Fatal("expected failure")
		}
		if m := r.manifest(); m["reason"] != "authority_lost" {
			t.Fatalf("%v", m)
		}
		if entries, _ := os.ReadDir(r.state); len(entries) != 0 {
			t.Fatal("nothing may be applied")
		}
	})

	t.Run("a credential for the wrong namespace cannot observe or apply: the record stays pending", func(t *testing.T) {
		r := newScriptRun(t)
		sc, env := r.scenario("preview", "acme-preview", "att_ns", "run_ns:preview", nil)
		_ = os.WriteFile(filepath.Join(r.secrets, "KUBECONFIG"), []byte("namespace=some-other-namespace\n"), 0o600)
		if _, err := r.run("deploy-preview.sh", env); err == nil {
			t.Fatal("expected failure")
		}
		if m := r.manifest(); m["reason"] != delivery.ReasonObservation {
			t.Fatalf("%v", m)
		}
		// The refusal is known before any external change, so nothing is claimed: the
		// registered record stays pending and the same action can be retried once fixed.
		if rec, _ := sc.fac.Record("run_ns:preview"); rec.Status != "pending" {
			t.Fatalf("%+v", rec)
		}
		if entries, _ := os.ReadDir(r.state); len(entries) != 0 {
			t.Fatal("nothing may be applied")
		}
	})

	t.Run("a lost apply response is reconciled by observation on rerun, without reapplying", func(t *testing.T) {
		r := newScriptRun(t)
		sc, env := r.scenario("preview", "acme-preview", "att_loss", "run_loss:preview", nil)
		_ = os.WriteFile(filepath.Join(r.state, "DROP_APPLY_RESPONSE"), nil, 0o600)
		out, err := r.run("deploy-preview.sh", env)
		if err != nil {
			t.Fatalf("a lost apply response is settled by observation: %v\n%s", err, out)
		}
		if rec, _ := sc.fac.Record("run_loss:preview"); rec.Status != "succeeded" {
			t.Fatalf("%+v", rec)
		}
		before, _ := os.ReadFile(filepath.Join(r.state, "Deployment.inventory-api.json"))
		if _, err := r.run("deploy-preview.sh", env); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(filepath.Join(r.state, "Deployment.inventory-api.json"))
		if string(before) != string(after) {
			t.Fatal("a settled deployment must not be changed by a rerun")
		}
		keep(t, r, "recovered")
	})

	t.Run("e2e refuses a live target that is not the intended deployment", func(t *testing.T) {
		const dep = "dep_0123456789abcdef0123456789abcdef"
		t.Setenv("INVENTORY_DEPLOYMENT_ID", dep)
		t.Setenv("INVENTORY_CONFIGURATION_DIGEST", "sha256:"+strings.Repeat("1", 64))
		t.Setenv("INVENTORY_IMAGE_DIGEST", scriptDigest)
		t.Setenv("INVENTORY_SOURCE_COMMIT", scriptCommit)
		s := newSystem(t)
		if _, err := s.worker(tenantA, "svc-worker-a"); err != nil {
			t.Fatal(err)
		}
		r := newScriptRun(t)
		_ = os.WriteFile(filepath.Join(r.secrets, "E2E_TOKEN_REVIEWER"), []byte(s.token("rita")+"\n"), 0o600)
		_ = os.WriteFile(filepath.Join(r.secrets, "E2E_TOKEN_OUTSIDER"), []byte(s.token("bea")+"\n"), 0o600)
		env := map[string]string{"APP_URL": s.base, "DEPLOYMENT_ID": dep, "CONFIGURATION_DIGEST": "sha256:" + strings.Repeat("1", 64), "IMAGE_DIGEST": scriptDigest,
			"SOURCE_COMMIT": scriptCommit, "E2E_TENANT": tenantA, "E2E_OTHER_TENANT": tenantB}
		if out, err := r.run("e2e.sh", env); err != nil {
			t.Fatalf("e2e against the intended deployment: %v\n%s", err, out)
		}
		if m := r.manifest(); m["outcome"] != "completed" {
			t.Fatalf("%v", m)
		}
		if _, err := os.Stat(filepath.Join(r.outputs, "e2e.json")); err != nil {
			t.Fatal("the e2e report is retained")
		}
		keep(t, r, "e2e")

		env["DEPLOYMENT_ID"] = "dep_ffffffffffffffffffffffffffffffff"
		if _, err := r.run("e2e.sh", env); err == nil {
			t.Fatal("a mismatched deployment must fail")
		}
		if m := r.manifest(); m["outcome"] != "failed" || m["reason"] != delivery.ReasonDeploymentMismatch {
			t.Fatalf("%v", m)
		}
	})
}

var _ = fmt.Sprintf

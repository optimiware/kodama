package acceptance_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/test/support/layer"
)

// scriptEnv is the step entrypoint's contract for a command: explicit variables
// and an output root, results file and secrets directory.
type scriptEnv struct {
	outputs, results string
	env              []string
}

func newScriptEnv(t testing.TB, extra ...string) scriptEnv {
	t.Helper()
	dir := t.TempDir()
	s := scriptEnv{outputs: filepath.Join(dir, "outputs"), results: filepath.Join(dir, "results.json")}
	if err := os.MkdirAll(s.outputs, 0o700); err != nil {
		t.Fatal(err)
	}
	s.env = append([]string{"KODAMA_OUTPUTS_DIR=" + s.outputs, "KODAMA_RESULTS_FILE=" + s.results, "KODAMA_SECRETS_DIR=" + filepath.Join(dir, "secrets")}, extra...)
	return s
}

func (s scriptEnv) manifest(t testing.TB) []byte {
	t.Helper()
	return []byte(readFile(t, s.results))
}

func requireFile(t testing.TB, path string) {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		t.Errorf("declared artifact %s is missing or not a regular file: %v", path, err)
	}
}

// registryRepo returns host:port/path for an isolated repository in the OCI
// registry, or skips/fails when no registry is provided.
func registryRepo(t testing.TB) (host, repo string) {
	t.Helper()
	layer.SkipService(t, "OCI registry")
	raw := os.Getenv("KODAMA_REGISTRY_URL")
	if raw == "" {
		if os.Getenv("KODAMA_REQUIRE_POSTGRES") == "1" {
			t.Fatal("KODAMA_REGISTRY_URL is unset but services are required")
		}
		t.Skip("registry acceptance skipped: set KODAMA_REGISTRY_URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("KODAMA_REGISTRY_URL %q", raw)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return u.Host, u.Host + "/" + resourcePrefix + "/inventory-" + hex.EncodeToString(b[:])
}

// TestDeliveryScriptContracts is the fixture-level companion of the real
// delivery acceptance (acceptance/delivery, Test_blueprint_go_delivery): it runs
// the generated scripts on the host against real PostgreSQL, with the factory's
// callbacks and the cluster as controlled doubles, and validates their results
// manifests against the merged factory contracts and the rendered pipelines. The
// real-cluster execution in the tools image is not claimed here.
func TestDeliveryScriptContracts(t *testing.T) {
	env := postgresEnv(t)
	app := generate(t)
	rendered := renderPipelines(t, app, nil)

	t.Run("quality.sh fails closed: a formatting error and a missing database are failures with diagnostics", func(t *testing.T) {
		bad := generate(t)
		writeFile(t, filepath.Join(bad, "internal/exceptions/zz_unformatted.go"), "package exceptions\nfunc   X( ){ }\n")
		mustRun(t, bad, nil, "git", "add", "-A")
		s := newScriptEnv(t) // no INVENTORY_TEST_ADMIN_URL
		out, err := run(t, bad, s.env, "sh", "ci/quality.sh")
		if err == nil {
			t.Fatalf("quality.sh must fail:\n%s", tail(out, 3000))
		}
		if err := validateManifest(s.manifest(t), nil); err != nil || !strings.Contains(string(s.manifest(t)), `"reason":"quality_checks_failed"`) {
			t.Fatalf("a failed manifest was expected: %s %v", s.manifest(t), err)
		}
		report := readFile(t, filepath.Join(s.outputs, "quality.json"))
		for _, want := range []string{`"passed":false`, `{"id":"gofmt","status":"failed"`, `{"id":"database-configured","status":"failed"`} {
			if !strings.Contains(report, want) {
				t.Errorf("quality.json lacks %s:\n%s", want, report)
			}
		}
		if !strings.Contains(readFile(t, filepath.Join(s.outputs, "quality.log")), "zz_unformatted.go") {
			t.Error("the log retains the diagnostics")
		}
	})

	t.Run("build.sh refuses a dirty checkout and writes a failed result", func(t *testing.T) {
		_, repo := registryRepo(t)
		dirty := generate(t)
		writeFile(t, filepath.Join(dirty, "untracked.txt"), "x")
		const tools = "registry.example.invalid/inventory/tools@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		// Without a digest-pinned tool identity the build refuses first.
		s := newScriptEnv(t, "KO_DOCKER_REPO="+repo, "REGISTRY_INSECURE=true")
		if _, err := run(t, dirty, s.env, "sh", "ci/build.sh"); err == nil || !strings.Contains(string(s.manifest(t)), "build_tool_identity") {
			t.Fatalf("a build without a pinned tools image must be refused: %v %s", err, s.manifest(t))
		}
		s = newScriptEnv(t, "KO_DOCKER_REPO="+repo, "REGISTRY_INSECURE=true", "TOOLS_IMAGE="+tools)
		if _, err := run(t, dirty, s.env, "sh", "ci/build.sh"); err == nil {
			t.Fatal("a dirty tree must not be built")
		}
		if !strings.Contains(string(s.manifest(t)), "build_tree_dirty") {
			t.Fatalf("%s", s.manifest(t))
		}
	})

	t.Run("preview, stage and e2e scripts produce contract-valid, identified evidence", func(t *testing.T) {
		evidence := t.TempDir()
		results := goTest(t, app, append(env, "INVENTORY_SCRIPT_EVIDENCE_DIR="+evidence), "^TestDeliveryScripts$", "./test/acceptance/")
		requirePassed(t, results,
			"test/acceptance.TestDeliveryScripts",
			"test/acceptance.TestDeliveryScripts/preview_and_stage_promote_one_image_digest_with_distinct_explicit_configuration",
			"test/acceptance.TestDeliveryScripts/e2e_refuses_a_live_target_that_is_not_the_intended_deployment",
			"test/acceptance.TestDeliveryScripts/a_snapshot_that_does_not_match_the_pinned_digest_is_refused_with_a_failed_result_and_no_effect",
		)
		for _, c := range []struct{ dir, file, step string }{
			{"preview", "inventory-preview.yaml", "preview"},
			{"stage", "inventory-stage.yaml", "stage"},
			{"e2e", "inventory-stage.yaml", "e2e"},
			{"recovered", "inventory-preview.yaml", "preview"},
		} {
			raw := []byte(readFile(t, filepath.Join(evidence, c.dir, "results.json")))
			decl := declaredOutputs(t, rendered[c.file], c.step)
			if err := validateManifest(raw, decl); err != nil {
				t.Errorf("%s: %v\n%s", c.dir, err, raw)
			}
			for name, d := range decl {
				if d.Type == pipeline.TypeArtifact {
					requireFile(t, filepath.Join(evidence, c.dir, filepath.Base(d.Path)))
					_ = name
				}
			}
		}
		// A refusal is a valid failed manifest, never a success.
		fail := []byte(readFile(t, filepath.Join(evidence, "mismatch", "results.json")))
		if err := validateManifest(fail, declaredOutputs(t, rendered["inventory-preview.yaml"], "preview")); err != nil || !bytes.Contains(fail, []byte(`"outcome":"failed"`)) {
			t.Errorf("mismatch manifest: %v %s", err, fail)
		}
		// The deployment evidence identifies source, image and tool image.
		var dep struct {
			DeploymentID string
			Source       struct{ Commit string }
			Image        struct{ Digest string }
			ToolImage    string
			Objects      []struct{ UID string }
			ExpiresAt    string
		}
		if err := json.Unmarshal([]byte(readFile(t, filepath.Join(evidence, "preview", "deployment.json"))), &dep); err != nil {
			t.Fatal(err)
		}
		if dep.DeploymentID == "" || len(dep.Source.Commit) != 40 || !strings.HasPrefix(dep.Image.Digest, "sha256:") || dep.ToolImage == "" || len(dep.Objects) != 7 || dep.ExpiresAt == "" {
			t.Errorf("incomplete deployment evidence: %+v", dep)
		}
	})
}

var _ = fmt.Sprintf
var _ = time.Second

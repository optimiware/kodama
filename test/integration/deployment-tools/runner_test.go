package deploymenttools_test

// Runs the real scripts in the real image the way the Podman runtime runs an
// ordinary step: unprivileged user, every capability dropped, no new privileges,
// read-only root filesystem with a small scratch tmpfs, bounded processes and
// memory, no container-runtime socket, and only the files a step receives
// (checkout, scoped kubeconfig, runtime token, platform CA, results location).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// restrictions are the execution restrictions of an ordinary step container. The
// tests assert, inside the container, that they took effect.
func restrictions() []string {
	return []string{
		"--user=65532:65532",
		"--userns=keep-id:uid=65532,gid=65532", // host-owned test files appear as the step user
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--read-only",
		"--tmpfs=/tmp:rw,size=64m,mode=1777",
		"--pids-limit=256",
		"--memory=256m",
		"--network=host", // the API server and the callback fixture listen on loopback
	}
}

type world struct {
	t       *testing.T
	dir     string
	cl      *cluster
	ns      *namespace
	cb      *callbacks
	image   string // local tag the steps run
	nodeRef string // the same image as the kind node knows it (digest-pinned)
	digest  runtimev1.Digest
	commit  string
	repoURL string
	art     domain.ArtifactID
	seq     int
}

func newWorld(t *testing.T, label string) *world {
	t.Helper()
	cl := requireCluster(t)
	w := &world{t: t, cl: cl, dir: t.TempDir(), commit: strings.Repeat("1", 40),
		repoURL: "https://git.t065.invalid/acme/inventory.git", art: "art_t065_image"}
	w.image = toolsImage(t)
	ref, digest := cl.loadIntoNode(t, w.image, w.dir)
	w.nodeRef, w.digest = ref, runtimev1.Digest(digest)
	w.ns = cl.newNamespace(t, w.dir, label)
	w.cb = newCallbacks(t, w.dir, w.ns.name)
	w.cb.produce(w.art, w.repoURL, w.commit, w.digest)
	return w
}

// step describes one run of one script.
type step struct {
	script     string
	actionKey  string // the attempt's pinned logical action (apply/lookup)
	attempt    domain.AttemptID
	token      string
	manifests  string // host directory mounted at /workspace/deploy
	kubeconfig string
	env        map[string]string
	args       []string // extra podman arguments (checks only)
	command    []string // overrides the script (introspection)
}

type result struct {
	Exit     int
	Stdout   string
	Stderr   string
	Manifest map[string]any
	Took     time.Duration
}

func (r result) outcome() string { s, _ := r.Manifest["outcome"].(string); return s }
func (r result) reason() string  { s, _ := r.Manifest["reason"].(string); return s }
func (r result) outputs() map[string]any {
	m, _ := r.Manifest["outputs"].(map[string]any)
	return m
}

func (w *world) nextKey(step string) string {
	w.seq++
	return fmt.Sprintf("run_t065_%s%d:%s", w.ns.name[len("t065-"):], w.seq, step)
}

// run executes the step. A script must always leave a result manifest behind;
// the helper returns it decoded (nil when none was written).
func (w *world) run(s step) result {
	t := w.t
	t.Helper()
	runDir, err := os.MkdirTemp(w.dir, "step-")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"identity", "secrets", "outputs", "results"} {
		if err := os.MkdirAll(filepath.Join(runDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if s.attempt == "" {
		s.attempt, s.token = w.cb.attempt(s.actionKey)
	}
	write := func(rel, content string) string {
		p := filepath.Join(runDir, rel)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	token := write("identity/token", s.token+"\n")
	kubeconfig := s.kubeconfig
	if kubeconfig == "" {
		kubeconfig = w.ns.kubeconfig
	}
	runID, stepID, _ := strings.Cut(s.actionKey, ":")

	args := []string{"run", "--rm"}
	args = append(args, restrictions()...)
	args = append(args,
		"-v", token+":/kodama/identity/token:ro",
		"-v", kubeconfig+":/kodama/secrets/KUBECONFIG:ro",
		"-v", w.cb.caFile+":/kodama/bootstrap/ca.crt:ro",
		"-v", filepath.Join(runDir, "results")+":/kodama/run:rw",
		"-v", filepath.Join(runDir, "outputs")+":/kodama/outputs:rw",
	)
	if s.manifests != "" {
		args = append(args, "-v", s.manifests+":/workspace/deploy:ro")
	}
	env := map[string]string{
		"KODAMA_SERVER_URL":   w.cb.url,
		"KODAMA_ATTEMPT_ID":   string(s.attempt),
		"KODAMA_RUN_ID":       runID,
		"KODAMA_STEP_ID":      stepID,
		"KODAMA_RESULTS_FILE": "/kodama/run/results.json",
		"KODAMA_OUTPUTS_DIR":  "/kodama/outputs",
		"KODAMA_SECRETS_DIR":  "/kodama/secrets",
		"KODAMA_WORKSPACE":    "/workspace",
		"RT_RETRY_DELAY":      "0",
		"RT_ATTEMPTS":         "3",
	}
	for k, v := range s.env {
		env[k] = v
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, s.args...)
	args = append(args, w.image)
	if len(s.command) > 0 {
		args = append(args, s.command...)
	} else {
		args = append(args, "/opt/kodama/deploy/"+s.script)
	}

	cmd := exec.Command("podman", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err = cmd.Run()
	res := result{Stdout: out.String(), Stderr: errb.String(), Took: time.Since(start)}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("podman: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(runDir, "results", "results.json")); err == nil {
		if err := json.Unmarshal(raw, &res.Manifest); err != nil {
			t.Fatalf("the results manifest is not JSON: %v: %s", err, raw)
		}
		checkManifestShape(t, raw)
	}
	t.Logf("%s (%s): exit %d in %s\n%s", s.script, s.actionKey, res.Exit, res.Took.Round(time.Millisecond), tail(res.Stderr, 2500))
	return res
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// checkManifestShape applies the entrypoint's results-manifest rules that can be
// checked without its parser: strict JSON object with only known members, no
// floats, a reason matching the reason-code pattern, outputs only for a
// completed outcome, scalar output values of the contract's types.
func checkManifestShape(t *testing.T, raw []byte) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("the manifest is not strict JSON: %v", err)
	}
	if len(raw) > 1<<20 {
		t.Fatalf("the manifest exceeds 1 MiB")
	}
	for k := range m {
		switch k {
		case "outcome", "reason", "outputs", "question":
		default:
			t.Fatalf("unknown manifest member %q", k)
		}
	}
	switch m["outcome"] {
	case "completed":
		if _, has := m["reason"]; has {
			t.Fatalf("a completed manifest carries no reason")
		}
	case "failed":
		reason, _ := m["reason"].(string)
		if !reasonPattern.MatchString(reason) {
			t.Fatalf("failed manifest reason %q is not a reason code", reason)
		}
		if _, has := m["outputs"]; has {
			t.Fatalf("a failed manifest carries no outputs")
		}
	default:
		t.Fatalf("unexpected manifest outcome %v", m["outcome"])
	}
	if outs, ok := m["outputs"].(map[string]any); ok {
		for name, v := range outs {
			switch x := v.(type) {
			case string, bool:
			case json.Number:
				if strings.ContainsAny(x.String(), ".eE") {
					t.Fatalf("output %s is not an integer: %s", name, x)
				}
			default:
				t.Fatalf("output %s has an unsupported type %T", name, v)
			}
		}
	}
}

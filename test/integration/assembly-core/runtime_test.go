package assembly_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// Test_core_assembly_current_runtime (core_assembly_current_runtime, SD-09,
// PX-10, PX-16, AC-12): the composition as KODAMA_ENV=test builds it today, not as
// T-070 assumed it. The environment and Podman runtime wiring are the ones the
// process reports; the dispatcher, the runtime API and the log, gate, agent and
// delivery routes exist; production's guard still refuses the same configuration;
// and a real container step completes an artifact and retains its log over
// spool directories that the installation never created: the fixture's own
// directories are removed before the process starts, so the assembly alone
// provides what artifact completion and log retention write to.
func Test_core_assembly_current_runtime(t *testing.T) {
	s := kit.New(t, kit.Options{})
	cfg := s.CoreConfig()
	if cfg.Env != app.Test || cfg.Runtime.Podman.Network == "" || cfg.Runtime.EntrypointBinary == "" || cfg.Runtime.StateDir == "" {
		t.Fatalf("the fixture does not describe the test environment's runtime: env %q, %+v", cfg.Env, cfg.Runtime)
	}

	// No spool directory exists: the kit's fixture creates them (it predates the
	// assembly doing so), so they are removed; nothing here prepares them again.
	artifactSpool := filepath.Join(cfg.Git.ScratchRoot, "artifact-spool")
	logSpool := filepath.Join(cfg.Git.ScratchRoot, "log-spool")
	for _, dir := range []string{artifactSpool, logSpool} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s must not exist before the process starts: %v", dir, err)
		}
	}

	t.Run("production refuses the same configuration", func(t *testing.T) {
		prod := cfg
		prod.Env = app.Production
		srv, err := s.TryStart(prod)
		if err == nil {
			_ = srv.Shutdown(t.Context())
			t.Fatal("production started with the Podman runtime")
		}
		if !strings.Contains(err.Error(), "production") {
			t.Fatalf("the refusal does not name production: %v", err)
		}
		for _, dir := range []string{artifactSpool, logSpool} {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("a refused configuration created %s", dir)
			}
		}
	})

	srv := s.Start()
	s.WaitReady()

	t.Run("environment, runtime and routes are the test composition", func(t *testing.T) {
		core := srv.Core()
		if core.Slice == nil || core.Slice.Backend != "podman" {
			t.Fatalf("the execution backend is %+v, want the Podman runtime", core.Slice)
		}
		workers := core.Workers()
		sort.Strings(workers)
		joined := strings.Join(workers, ",")
		for _, want := range []string{"dispatch", "reconciler", "event-publisher", "gate-reconciler", "deployment-cleanup"} {
			if !strings.Contains(joined, want) {
				t.Errorf("worker %s is missing from %v", want, workers)
			}
		}
		registered := strings.Join(core.Routes.Registered, ",")
		for _, want := range []string{"executor-dispatch", "runtime-admission", "runtime-artifacts", "step-result-callbacks", "logs"} {
			if !strings.Contains(registered, want) {
				t.Errorf("route %s is missing from %v", want, core.Routes.Registered)
			}
		}
		// The runtime API refuses an unauthenticated caller instead of answering 404.
		if resp, _ := s.Raw("POST", "/runtime/v1/attempts/att_current/result", "", strings.NewReader("{}"), nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("the runtime result route without a credential: %d", resp.StatusCode)
		}
	})

	t.Run("the process created its private spool directories", func(t *testing.T) {
		for _, dir := range []string{artifactSpool, logSpool} {
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				t.Fatalf("%s was not created by the assembly: %v", filepath.Base(dir), err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
				t.Errorf("%s has mode %o, want 0700", filepath.Base(dir), info.Mode().Perm())
			}
		}
	})

	w := s.Seed(true)
	item := w.WorkItem("Current runtime", "# current runtime")
	w.Provision(item.Id)
	def := w.PodmanDefinition("current-runtime", kit.Step{
		Script:    `echo "current runtime log line"; echo "artifact bytes" > /kodama/outputs/report; printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`,
		Outputs:   []string{"v"},
		Artifacts: map[string]string{"report": "/kodama/outputs/report"},
	})

	run := w.Run("current-runtime-run", item, def.Id)
	attempt := w.WaitAttempt(run.Id, time.Minute)
	c := s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
	if len(c.Names) != 1 || c.Attempt() != attempt {
		t.Fatalf("the workload %v is not the attempt %s", c.Names, attempt)
	}
	final := w.WaitRun(run.Id, 3*time.Minute)
	if final.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s): artifact completion or log retention failed without its spool directory", final.Status, final.Reason)
	}

	// Artifact completion wrote its verification copy under the spool the
	// assembly created, and the artifact is verified and downloadable.
	outs := *w.Steps(run.Id)[0].Outputs
	artifactID, err := outs["report"].AsRunsScalar0()
	if err != nil || artifactID == "" {
		t.Fatalf("the artifact output %v (%v)", outs["report"], err)
	}
	download := true
	art, err := s.API("engineer").GetArtifactWithResponse(t.Context(), kit.TenantA, w.Project, artifactID, &client.GetArtifactParams{Download: &download})
	if err != nil || art.StatusCode() != http.StatusOK {
		t.Fatalf("artifact: %v %s", err, art.Body)
	}
	var transfer struct {
		Download struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"download"`
	}
	mustJSON(t, art.Body, &transfer)
	req, _ := http.NewRequest(http.MethodGet, transfer.Download.URL, nil)
	for k, v := range transfer.Download.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(data) != "artifact bytes\n" {
		t.Fatalf("download: %d %q", resp.StatusCode, data)
	}

	// The log was retained (the wrapper's own upload and the controller's capture
	// both stage under the log spool) and outlives the container.
	w.WaitRemoved(attempt)
	if lines := w.WaitLogs("engineer", run.Id); !strings.Contains(strings.Join(lines, "\n"), "current runtime log line") {
		t.Fatalf("retained log %q", lines)
	}
	for _, role := range []string{"output", "diagnostic"} {
		if n := s.OwnerCount(`SELECT count(*) FROM artifacts WHERE tenant_id = $1 AND run_id = $2 AND role = $3`, kit.TenantA, run.Id, role); n < 1 {
			t.Fatalf("no %s artifact of the run: the output and the retained log are both expected", role)
		}
	}

	// A restart over the existing directories adopts them.
	s.Stop()
	s.Restart()
	s.WaitReady()
}

func mustJSON(t testing.TB, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
}

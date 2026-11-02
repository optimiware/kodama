package main

// Source-set compatibility of the wrapper (task T-153, ADR 0011). These tests run
// the real wrapper (`run`) against a real Git, a TLS stand-in for the runtime
// context routes and a scripted stand-in for the OpenCode binary, which edits the
// checkouts and writes the structured result exactly as a harness would. The
// model, gateway and MCP proxy are not exercised (they are T-031/T-085 evidence);
// nothing here is a live-provider qualification.
//
// The wrapper never publishes: it commits, on the branches the plan chooses, and
// the step entrypoint pushes what the attempt's own grant selects. The tests
// therefore play the entrypoint's selection rule against local bare repositories
// to show that what the wrapper leaves behind is exactly what the grant lets be
// published, and nothing more.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/domain"
)

const (
	v1Cases = "../../../contracts/runtime/v1/fixtures/cases.json"
	v2Cases = "../../../contracts/runtime/v2/fixtures/cases.json"
	v3Cases = "../../../contracts/runtime/v3/fixtures/cases.json"
)

func fixturePayload(t *testing.T, path, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			return c.Payload
		}
	}
	t.Fatalf("no fixture %q in %s", name, path)
	return nil
}

// payload builds a real wrapper payload (the profile expansion the control plane
// freezes into the step's arguments) for the tuple this image carries.
func payload(t *testing.T) (agents.WrapperConfig, []string) {
	t.Helper()
	d := agentstest.Draft(t, "coding")
	d.Wrapper = agents.Wrapper{ID: spec.WrapperID, Version: spec.WrapperVersion, Command: []string{spec.WrapperPath},
		Harness: agents.Harness{ID: spec.HarnessID, Version: spec.HarnessVersion}}
	d.Tools = nil
	d.Transcript.Record = false
	p, err := agents.NewProfileVersion(agents.Candidate{
		Scope: domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}, ID: "ap_1", Content: agentstest.Content(t, d),
		CreatedBy: agents.Actor{ID: "u_1", Type: agents.ActorUser}, CreatedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := agents.Expand(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg, present, err := agents.DecodeArgs(exp.Args)
	if err != nil || !present {
		t.Fatalf("payload: %v %v", present, err)
	}
	return cfg, exp.Args
}

func conformanceDecl() runtimev1.OutputDecl {
	p := spec.DeclaredOutputPath(spec.ConformanceFile)
	return runtimev1.OutputDecl{Name: spec.ConformanceOutput, Type: runtimev1.OutputArtifact, Path: &p}
}

// runtimeFake serves the attempt's context on the route of its version and
// answers 409 on every other one, as the control plane does.
type runtimeFake struct {
	srv  *httptest.Server
	ca   string
	mu   sync.Mutex
	hits []string
}

func newRuntime(t *testing.T, dir string, version int64, body []byte) *runtimeFake {
	t.Helper()
	f := &runtimeFake{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits = append(f.hits, r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer rt_test_token_value" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		want := map[int64]string{1: "/runtime/v1/attempts/att_01/context", 2: "/runtime/v2/attempts/att_01/context", 3: "/runtime/v3/attempts/att_01/context"}[version]
		if r.URL.Path != want {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"context_version_mismatch","message":"the attempt belongs to another runtime version","requestId":"r","details":{}}}`))
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	f.ca = filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *runtimeFake) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.hits...)
}

// gitT runs git in dir for test setup and inspection.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "-c", "safe.directory=*"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + dir}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// repo makes a checkout at dir with one commit on main, detached the way the
// preparation leaves it, and returns the commit.
func repo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "seed")
	head := gitT(t, dir, "rev-parse", "HEAD")
	gitT(t, dir, "checkout", "-q", "--detach", head)
	return head
}

// scenario is one wrapper run in a temporary pod layout.
type scenario struct {
	t         *testing.T
	root      string
	workspace string
	outputs   string
	results   string
	envDump   string
	marker    string
	rt        *runtimeFake
	args      []string
	cfg       agents.WrapperConfig
	script    string
}

// newScenario lays out the workspace, outputs and results paths and serves ctx on
// the route of version.
func newScenario(t *testing.T, version int64, ctxBody []byte, script string) *scenario {
	t.Helper()
	cfg, args := payload(t)
	s := &scenario{t: t, root: t.TempDir(), cfg: cfg, args: args}
	s.workspace = filepath.Join(s.root, "workspace")
	s.outputs = filepath.Join(s.root, "outputs")
	s.results = filepath.Join(s.root, "results.json")
	s.envDump = filepath.Join(s.root, "harness-env.txt")
	s.marker = filepath.Join(s.root, "harness-ran")
	for _, d := range []string{s.workspace, s.outputs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.root, "token"), []byte("rt_test_token_value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.rt = newRuntime(t, s.root, version, ctxBody)
	s.script = script
	return s
}

// harness writes the scripted stand-in for the OpenCode binary: `--version`
// answers the pin; `run` records its environment, then runs the script in the
// workspace (its working directory, as the wrapper sets it).
func (s *scenario) harness() string {
	bin := filepath.Join(s.root, "opencode")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo " + spec.HarnessVersion + "; exit 0; fi\n" +
		"touch '" + s.marker + "'\n" +
		"env > '" + s.envDump + "'\n" +
		s.script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		s.t.Fatal(err)
	}
	return bin
}

// run executes the wrapper and returns its exit code and the manifest it wrote.
func (s *scenario) run() (int, manifest) {
	s.t.Helper()
	env := map[string]string{
		spec.EnvResultsFile: s.results, spec.EnvAttemptID: "att_01", spec.EnvWorkspace: s.workspace, spec.EnvOutputsDir: s.outputs,
		spec.EnvPlatformURL: s.rt.srv.URL, spec.EnvTokenFile: filepath.Join(s.root, "token"), spec.EnvCAFile: s.rt.ca,
		spec.EnvOpenCodeBin: s.harness(), spec.EnvIdleSeconds: "60", "PATH": os.Getenv("PATH"), "TMPDIR": s.root, "LANG": "C.UTF-8",
	}
	var logs bytes.Buffer
	code := run(context.Background(), options{args: s.args, getenv: func(k string) string { return env[k] }, stdout: &logs})
	raw, err := os.ReadFile(s.results)
	if err != nil {
		s.t.Fatalf("no manifest (exit %d): %v\n%s", code, err, logs.String())
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		s.t.Fatal(err)
	}
	s.t.Logf("wrapper logs:\n%s", logs.String())
	return code, m
}

func (s *scenario) ran() bool {
	_, err := os.Stat(s.marker)
	return err == nil
}

func (s *scenario) conformance() runtimev1.ConformanceReport {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.outputs, spec.ConformanceFile))
	if err != nil {
		s.t.Fatalf("no conformance report: %v", err)
	}
	var rep runtimev1.ConformanceReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		s.t.Fatal(err)
	}
	return rep
}

const resultCompleted = `cat > .kodama-result.json <<'J'
{"outcome":"completed","summary":"implemented the change across repositories"}
J`

// v3Context decodes a published v3 fixture and adapts it to the payload's pins and
// the conformance output, as admission would.
func v3Context(t *testing.T, cfg agents.WrapperConfig, fixture string, mutate func(*runtimev3.ContextV3)) (runtimev3.ContextV3, []byte) {
	t.Helper()
	c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](fixturePayload(t, v3Cases, fixture))
	if err != nil {
		t.Fatal(err)
	}
	prof, base := cfg.Pins()
	c.Profile, c.DesignBaseline = &prof, &base
	c.Outputs = []runtimev1.OutputDecl{conformanceDecl()}
	if mutate != nil {
		mutate(&c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return c, raw
}

// entrypointSelection is the rule the step entrypoint applies after the wrapper
// exits (internal/entrypoint stepv3push.go): the writer's write source goes to its
// task branch when the commit descends from the expected tip; every attempt also
// pushes the branch it left checked out when a pattern of its own grant selects it
// and it holds a commit beyond the one the source started on. The result is the
// (ref, commit) the entrypoint would push for one checkout.
func entrypointSelection(t *testing.T, c runtimev3.ContextV3, src runtimev3.SourceV3, dir, start string) (updates map[string]string) {
	t.Helper()
	updates = map[string]string{}
	var patterns []string
	for _, r := range c.Git.Repositories {
		if r.RepositoryID == src.RepositoryID {
			patterns = r.Update
		}
	}
	permits := func(ref string) bool {
		for _, p := range patterns {
			if gitv1.MatchRefPattern(p, ref) {
				return true
			}
		}
		return false
	}
	head := gitT(t, dir, "rev-parse", "HEAD")
	headRef := ""
	if out, err := exec.Command("git", "-C", dir, "-c", "safe.directory=*", "symbolic-ref", "-q", "HEAD").Output(); err == nil {
		headRef = strings.TrimSpace(string(out))
	}
	writer := c.Workspace.Access == runtimev3.AccessWrite && src.Access == gitv1.AccessWrite && src.Branch != nil
	if writer && head != start && permits(*src.Branch) {
		updates[*src.Branch] = head
	}
	if headRef != "" && (src.Branch == nil || headRef != *src.Branch) && permits(headRef) && head != start {
		updates[headRef] = head
	}
	return updates
}

// pushAsEntrypoint performs the selected updates against a bare repository and
// returns its refs.
func pushAsEntrypoint(t *testing.T, dir string, updates map[string]string) map[string]string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitT(t, filepath.Dir(bare), "init", "-q", "--bare", "-b", "main", bare)
	for ref, commit := range updates {
		gitT(t, dir, "push", "-q", bare, commit+":"+ref)
	}
	refs := map[string]string{}
	if out := gitT(t, bare, "for-each-ref", "--format=%(refname) %(objectname)"); out != "" {
		for _, l := range strings.Split(out, "\n") {
			f := strings.Fields(l)
			refs[f[0]] = f[1]
		}
	}
	return refs
}

// Test_unit_wrapper_pushes_with_attempt_grant: the wrapper leaves each checkout of
// a source-set attempt on exactly the branch, and at exactly the commit, that the
// attempt's own Git grant lets the entrypoint publish; it holds, uses and passes
// on no credential and publishes nothing itself (AR-11, AR-15, AR-A09). The name
// is the task's required test; the push itself is the entrypoint's, played here
// with its selection rule against a local bare repository.
//
// The selection rule (entrypointSelection) is REPLAYED from
// internal/entrypoint/stepv3push.go (planSource); the entrypoint exports no
// selection function to call, so this copy cannot follow it automatically. The
// entrypoint's own behavior is proved by Test_unit_entrypoint_pushes_agent_branch
// and Test_unit_entrypoint_writer_pushes_task_branch (internal/entrypoint); if
// planSource's rule changes, this replay and those tests must change together.
func Test_unit_wrapper_pushes_with_attempt_grant(t *testing.T) {
	cfg, _ := payload(t)
	taskBranch := "refs/heads/kodama/work/wi_1042"

	t.Run("the writer: task branch for its write source, an agent branch for the rest", func(t *testing.T) {
		c, body := v3Context(t, cfg, "context_v3_work_item_writer_two_repositories", nil)
		s := newScenario(t, 3, body, `echo backend-change > backend/feature.txt
echo frontend-change > frontend/ui.txt
`+resultCompleted)
		backStart := repo(t, filepath.Join(s.workspace, "backend"))
		frontStart := repo(t, filepath.Join(s.workspace, "frontend"))

		code, m := s.run()
		if code != 0 || m.Outcome != "completed" {
			t.Fatalf("exit %d manifest %+v", code, m)
		}

		// Backend (the writer's write source): committed on the detached HEAD, for the task branch.
		backHead := gitT(t, filepath.Join(s.workspace, "backend"), "rev-parse", "HEAD")
		if backHead == backStart || gitT(t, filepath.Join(s.workspace, "backend"), "rev-parse", "HEAD^") != backStart {
			t.Fatalf("backend: one commit on top of the start")
		}
		if out, err := exec.Command("git", "-C", filepath.Join(s.workspace, "backend"), "-c", "safe.directory=*", "symbolic-ref", "-q", "HEAD").Output(); err == nil {
			t.Fatalf("the writer's write source must stay detached for the entrypoint's task-branch push, on %s", out)
		}
		// Frontend (a read source): on the run's agent branch, checked out.
		front := filepath.Join(s.workspace, "frontend")
		wantFrontRef := "refs/heads/kodama/agents/wi_1042/run_01/generate"
		if got := gitT(t, front, "symbolic-ref", "HEAD"); got != wantFrontRef {
			t.Fatalf("frontend is on %q, want %q", got, wantFrontRef)
		}
		if gitT(t, front, "rev-parse", "HEAD^") != frontStart {
			t.Fatalf("frontend: one commit on top of the start")
		}
		if got := gitT(t, front, "log", "-1", "--format=%an <%ae>"); got != "Kodama <step@kodama.invalid>" {
			t.Fatalf("author %q", got)
		}

		// What the entrypoint would push with the attempt's own grant, and nothing else.
		backUpdates := entrypointSelection(t, c, c.Workspace.Sources[0], filepath.Join(s.workspace, "backend"), backStart)
		frontUpdates := entrypointSelection(t, c, c.Workspace.Sources[1], front, frontStart)
		if len(backUpdates) != 1 || backUpdates[taskBranch] != backHead {
			t.Fatalf("backend updates: %v", backUpdates)
		}
		if len(frontUpdates) != 1 || frontUpdates[wantFrontRef] != gitT(t, front, "rev-parse", "HEAD") {
			t.Fatalf("frontend updates: %v", frontUpdates)
		}
		backRemote := pushAsEntrypoint(t, filepath.Join(s.workspace, "backend"), backUpdates)
		if len(backRemote) != 1 || backRemote[taskBranch] != backHead {
			t.Fatalf("remote refs after the entrypoint's push: %v", backRemote)
		}

		// The conformance report names the primary source's commit, bound to the pinned baseline.
		rep := s.conformance()
		if string(rep.CandidateCommit) != backHead || rep.BaselineDigest != c.DesignBaseline.Digest {
			t.Fatalf("report: %+v", rep)
		}
	})

	t.Run("a reader never reaches the task branch, not even for a write source", func(t *testing.T) {
		c, body := v3Context(t, cfg, "context_v3_work_item_reader_first_attempt", nil)
		s := newScenario(t, 3, body, `echo b > backend/feature.txt
echo f > frontend/ui.txt
`+resultCompleted)
		backStart := repo(t, filepath.Join(s.workspace, "backend"))
		frontStart := repo(t, filepath.Join(s.workspace, "frontend"))
		if code, m := s.run(); code != 0 || m.Outcome != "completed" {
			t.Fatalf("exit %d manifest %+v", code, m)
		}
		want := "refs/heads/kodama/agents/wi_1042/run_01/test"
		for i, start := range []string{backStart, frontStart} {
			dir := filepath.Join(s.workspace, c.Workspace.Sources[i].Path)
			if got := gitT(t, dir, "symbolic-ref", "HEAD"); got != want {
				t.Fatalf("%s is on %q, want %q", dir, got, want)
			}
			ups := entrypointSelection(t, c, c.Workspace.Sources[i], dir, start)
			if len(ups) != 1 || ups[want] == "" {
				t.Fatalf("%s updates: %v", dir, ups)
			}
			if refs := pushAsEntrypoint(t, dir, ups); len(refs) != 1 || refs[want] == "" {
				t.Fatalf("remote refs: %v", refs)
			}
		}
		if out := gitT(t, filepath.Join(s.workspace, "backend"), "branch", "--list", "kodama/work/*"); out != "" {
			t.Fatalf("a task branch exists in a reader's checkout: %q", out)
		}
	})

	t.Run("an unchanged source stays as it was and nothing of it is published", func(t *testing.T) {
		c, body := v3Context(t, cfg, "context_v3_work_item_writer_two_repositories", nil)
		s := newScenario(t, 3, body, `echo only-backend > backend/feature.txt
`+resultCompleted)
		repo(t, filepath.Join(s.workspace, "backend"))
		frontStart := repo(t, filepath.Join(s.workspace, "frontend"))
		if code, m := s.run(); code != 0 || m.Outcome != "completed" {
			t.Fatalf("exit %d manifest %+v", code, m)
		}
		front := filepath.Join(s.workspace, "frontend")
		if gitT(t, front, "rev-parse", "HEAD") != frontStart {
			t.Fatal("an unchanged source moved")
		}
		if out, err := exec.Command("git", "-C", front, "-c", "safe.directory=*", "symbolic-ref", "-q", "HEAD").Output(); err == nil {
			t.Fatalf("an unchanged source got a branch: %s", out)
		}
		if out := gitT(t, front, "for-each-ref", "refs/heads/kodama"); out != "" {
			t.Fatalf("an unchanged source got a branch: %s", out)
		}
		if ups := entrypointSelection(t, c, c.Workspace.Sources[1], front, frontStart); len(ups) != 0 {
			t.Fatalf("updates for an unchanged source: %v", ups)
		}
	})

	t.Run("a commit the agent made itself ends up on the agent branch with the wrapper's own", func(t *testing.T) {
		c, body := v3Context(t, cfg, "context_v3_push_origin_reader_root_source", nil)
		s := newScenario(t, 3, body, `echo one > one.txt
git add one.txt && git -c user.name=Agent -c user.email=a@example.invalid commit -q -m "agent commit"
echo two > two.txt
`+resultCompleted)
		start := repo(t, s.workspace)
		if code, m := s.run(); code != 0 || m.Outcome != "completed" {
			t.Fatalf("exit %d manifest %+v", code, m)
		}
		want := "refs/heads/kodama/runs/run_01/verify"
		if got := gitT(t, s.workspace, "symbolic-ref", "HEAD"); got != want {
			t.Fatalf("on %q, want %q", got, want)
		}
		if n := gitT(t, s.workspace, "rev-list", "--count", start+"..HEAD"); n != "2" {
			t.Fatalf("%s commits on top of the start, want the agent's and the wrapper's", n)
		}
		if ups := entrypointSelection(t, c, c.Workspace.Sources[0], s.workspace, start); len(ups) != 1 || ups[want] == "" {
			t.Fatalf("updates: %v", ups)
		}
		if _, err := os.Lstat(filepath.Join(s.workspace, spec.ResultFile)); err == nil {
			t.Fatal("the result file was committed or left behind")
		}
		if out := gitT(t, s.workspace, "ls-files"); strings.Contains(out, spec.ResultFile) {
			t.Fatalf("the result file is tracked: %s", out)
		}
	})

	t.Run("the harness and the checkouts hold no credential and no remote", func(t *testing.T) {
		_, body := v3Context(t, cfg, "context_v3_work_item_writer_two_repositories", nil)
		s := newScenario(t, 3, body, resultCompleted)
		repo(t, filepath.Join(s.workspace, "backend"))
		repo(t, filepath.Join(s.workspace, "frontend"))
		if code, m := s.run(); code != 0 || m.Outcome != "completed" {
			t.Fatalf("exit %d manifest %+v", code, m)
		}
		raw, err := os.ReadFile(s.envDump)
		if err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, spec.ModelKeyEnv: true,
			"OPENCODE_DISABLE_PROJECT_CONFIG": true, "OPENCODE_DISABLE_MODELS_FETCH": true, "OPENCODE_DISABLE_AUTOUPDATE": true, "OPENCODE_DISABLE_CLAUDE_CODE": true,
			"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_NOSYSTEM": true, "GIT_TERMINAL_PROMPT": true, spec.EnvAttemptID: true, spec.EnvWorkspace: true,
			"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_CACHE_HOME": true,
			"PWD": true, "SHLVL": true, "_": true, "OLDPWD": true}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			name, val, _ := strings.Cut(line, "=")
			if !allowed[name] {
				t.Errorf("the harness environment carries %s", name)
			}
			for _, secret := range []string{"rt_test_token_value", "credential", "ASKPASS"} {
				if strings.Contains(val, secret) || strings.Contains(strings.ToUpper(name), strings.ToUpper(secret)) {
					t.Errorf("%s=%q looks like a credential", name, val)
				}
			}
		}
		for _, src := range []string{"backend", "frontend"} {
			dir := filepath.Join(s.workspace, src)
			if out := gitT(t, dir, "remote"); out != "" {
				t.Errorf("%s has remotes: %q", src, out)
			}
			cfgRaw, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"credential", "extraheader", "url ", "helper", "rt_test_token_value"} {
				if strings.Contains(string(cfgRaw), bad) {
					t.Errorf("%s/.git/config contains %q", src, bad)
				}
			}
		}
	})

	t.Run("a context whose grant does not match its source set is refused and nothing runs", func(t *testing.T) {
		// The contract requires the grant to be exactly what the source set derives,
		// so an incoherent one is not a valid context at all (the planner's own
		// refusal of such a plan is proved in internal/agents).
		_, body := v3Context(t, cfg, "context_v3_work_item_reader_first_attempt", func(c *runtimev3.ContextV3) {
			c.Git.Repositories[1].Update = []string{"refs/heads/kodama/agents/wi_1042/run_01/other"}
		})
		s := newScenario(t, 3, body, resultCompleted)
		repo(t, filepath.Join(s.workspace, "backend"))
		repo(t, filepath.Join(s.workspace, "frontend"))
		code, m := s.run()
		if code != 0 || m.Outcome != "failed" || m.Reason != spec.ReasonContextMismatch || s.ran() {
			t.Fatalf("exit %d manifest %+v ran=%v", code, m, s.ran())
		}
	})

	t.Run("a checkout that is not a repository fails before the harness runs", func(t *testing.T) {
		_, body := v3Context(t, cfg, "context_v3_work_item_writer_two_repositories", nil)
		s := newScenario(t, 3, body, resultCompleted)
		repo(t, filepath.Join(s.workspace, "backend")) // frontend is missing
		code, m := s.run()
		if code != 0 || m.Outcome != "failed" || m.Reason != spec.ReasonSourceInvalid || s.ran() {
			t.Fatalf("exit %d manifest %+v ran=%v", code, m, s.ran())
		}
	})

	t.Run("a failed or input-needing attempt commits nothing and creates no branch", func(t *testing.T) {
		for name, doc := range map[string]struct{ result, outcome string }{
			"failed":      {`{"outcome":"failed","reason":"cannot_reproduce"}`, "failed"},
			"needs_input": {`{"outcome":"needs_input","question":"which tier?"}`, "needs_input"},
		} {
			_, body := v3Context(t, cfg, "context_v3_work_item_writer_two_repositories", nil)
			s := newScenario(t, 3, body, "echo dirty > backend/x.txt\necho dirty > frontend/y.txt\ncat > .kodama-result.json <<'J'\n"+doc.result+"\nJ")
			starts := []string{repo(t, filepath.Join(s.workspace, "backend")), repo(t, filepath.Join(s.workspace, "frontend"))}
			code, m := s.run()
			if code != 0 || m.Outcome != doc.outcome {
				t.Fatalf("%s: exit %d manifest %+v", name, code, m)
			}
			for i, src := range []string{"backend", "frontend"} {
				dir := filepath.Join(s.workspace, src)
				if gitT(t, dir, "rev-parse", "HEAD") != starts[i] {
					t.Errorf("%s: %s was committed", name, src)
				}
				if out := gitT(t, dir, "for-each-ref", "refs/heads/kodama"); out != "" {
					t.Errorf("%s: %s got a branch: %s", name, src, out)
				}
			}
		}
	})
}

// plantHostile makes the checkout of dir hostile in one way. Each plants a program
// that would touch marker if Git ran it. It never calls Git in dir afterwards (the
// test's own Git would run the program).
type hostile struct {
	name  string
	plant func(t *testing.T, s *scenario, dir string)
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// nestedRepo plants a repository below dir at sub/. With gitlink the outer
// repository records it (a commit made before anything hostile exists); the
// nested configuration then names a clean filter and a file-system monitor and a
// same-size edit (which Git must hash, not just stat) waits to be cleaned. Git
// is never run in the nested repository after its configuration is planted.
func nestedRepo(t *testing.T, s *scenario, dir, program string, gitlink bool) {
	t.Helper()
	sub := filepath.Join(dir, "sub")
	repo(t, sub)
	if gitlink {
		gitT(t, dir, "add", "sub")
		gitT(t, dir, "commit", "-q", "-m", "record the nested repository")
	}
	appendFile(t, filepath.Join(sub, ".git", "config"), "[filter \"x\"]\n\tclean = "+program+"\n[core]\n\tfsmonitor = "+program+"\n")
	if err := os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("* filter=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "README.md"), []byte("sEEd\n"), 0o644); err != nil { // same size as "seed\n"
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hostiles(marker string) []hostile {
	touch := "touch " + marker
	script := func(t *testing.T, dir, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+touch+"\ncat\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := func(dir string) string { return filepath.Join(dir, ".git", "config") }
	return []hostile{
		{"a clean filter", func(t *testing.T, s *scenario, dir string) {
			f := script(t, s.root, "clean.sh")
			appendFile(t, cfg(dir), "[filter \"x\"]\n\tclean = "+f+"\n")
			if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a filter under a legacy, mixed-case header", func(t *testing.T, s *scenario, dir string) {
			appendFile(t, cfg(dir), "[ Filter.x ]\n\tclean = "+script(t, s.root, "clean.sh")+"\n")
		}},
		{"a hooks path", func(t *testing.T, s *scenario, dir string) {
			hooks := filepath.Join(s.root, "hooks")
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			script(t, hooks, "pre-commit")
			appendFile(t, cfg(dir), "[core]\n\tHooksPath = "+hooks+"\n")
		}},
		{"a file-system monitor", func(t *testing.T, s *scenario, dir string) {
			appendFile(t, cfg(dir), "[core]\n\tfsmonitor = "+script(t, s.root, "fsm.sh")+"\n")
		}},
		{"a work tree on the header line", func(t *testing.T, s *scenario, dir string) {
			appendFile(t, cfg(dir), "[core] worktree = "+s.root+"\n")
		}},
		{"an include", func(t *testing.T, s *scenario, dir string) {
			inc := filepath.Join(s.root, "inc.cfg")
			if err := os.WriteFile(inc, []byte("[filter \"y\"]\n\tclean = "+script(t, s.root, "clean.sh")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			appendFile(t, cfg(dir), "[includeIf \"gitdir:"+dir+"/\"]\n\tpath = "+inc+"\n")
		}},
		{"a per-worktree configuration", func(t *testing.T, s *scenario, dir string) {
			appendFile(t, cfg(dir), "[extensions]\n\tworktreeConfig = true\n")
			if err := os.WriteFile(filepath.Join(dir, ".git", "config.worktree"), []byte("[core]\n\tfsmonitor = "+script(t, s.root, "fsm.sh")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a configuration that is a symlink", func(t *testing.T, s *scenario, dir string) {
			real := filepath.Join(s.root, "real.cfg")
			raw, _ := os.ReadFile(cfg(dir))
			if err := os.WriteFile(real, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			os.Remove(cfg(dir))
			if err := os.Symlink(real, cfg(dir)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a commondir that points at an agent-controlled repository", func(t *testing.T, s *scenario, dir string) {
			// Git reads configuration, hooks and refs from the common directory a
			// `commondir` file names, even when GIT_DIR is set: the planted copy carries a
			// clean filter and the checkout has a dirty file, so `git add -A` would run it.
			common := filepath.Join(s.root, "common.git")
			if out, err := exec.Command("cp", "-R", filepath.Join(dir, ".git"), common).CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			appendFile(t, filepath.Join(common, "config"), "[filter \"x\"]\n\tclean = "+script(t, s.root, "clean.sh")+"\n")
			if err := os.WriteFile(filepath.Join(dir, ".git", "commondir"), []byte(common+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"an alternate object store", func(t *testing.T, s *scenario, dir string) {
			other := filepath.Join(s.root, "elsewhere")
			repo(t, other)
			if err := os.WriteFile(filepath.Join(dir, ".git", "objects", "info", "alternates"), []byte(filepath.Join(other, ".git", "objects")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"history grafts", func(t *testing.T, s *scenario, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".git", "info", "grafts"), []byte(strings.Repeat("0", 40)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a submodule repository directory", func(t *testing.T, s *scenario, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, ".git", "modules", "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"a nested repository recorded as a gitlink whose configuration names a filter and a monitor", func(t *testing.T, s *scenario, dir string) {
			nestedRepo(t, s, dir, script(t, s.root, "clean.sh"), true)
		}},
		{"an untracked nested repository whose configuration names a filter", func(t *testing.T, s *scenario, dir string) {
			nestedRepo(t, s, dir, script(t, s.root, "clean.sh"), false)
		}},
		{"a header whose quoted subsection hides the section's key", func(t *testing.T, s *scenario, dir string) {
			appendFile(t, cfg(dir), "[core \"a]\"] fsmonitor = "+script(t, s.root, "fsm.sh")+"\n")
		}},
		{"a source replaced by a symlink to another repository", func(t *testing.T, s *scenario, dir string) {
			other := filepath.Join(s.root, "elsewhere")
			repo(t, other)
			os.RemoveAll(dir)
			if err := os.Symlink(other, dir); err != nil {
				t.Fatal(err)
			}
		}},
		{"a .git that is a gitdir file", func(t *testing.T, s *scenario, dir string) {
			other := filepath.Join(s.root, "elsewhere")
			repo(t, other)
			os.RemoveAll(filepath.Join(dir, ".git"))
			if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{".git replaced by a symlink", func(t *testing.T, s *scenario, dir string) {
			other := filepath.Join(s.root, "elsewhere")
			repo(t, other)
			os.RemoveAll(filepath.Join(dir, ".git"))
			if err := os.Symlink(filepath.Join(other, ".git"), filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
	}
}

// Test_unit_wrapper_refuses_hostile_checkouts: a checkout whose repository names a
// program or redirects Git is refused with source_invalid, before the harness runs
// when it is found at the start and after it when the agent plants it, and the
// planted program never runs. (The checkouts are directories the agent controls.)
func Test_unit_wrapper_refuses_hostile_checkouts(t *testing.T) {
	cfg, _ := payload(t)
	for _, h := range hostiles("MARKER") {
		t.Run("at the start: "+h.name, func(t *testing.T) {
			_, body := v3Context(t, cfg, "context_v3_work_item_writer_two_repositories", nil)
			s := newScenario(t, 3, body, resultCompleted)
			marker := filepath.Join(s.root, "hostile-ran")
			repo(t, filepath.Join(s.workspace, "backend"))
			front := filepath.Join(s.workspace, "frontend")
			repo(t, front)
			for _, hh := range hostiles(marker) {
				if hh.name == h.name {
					hh.plant(t, s, front)
				}
			}
			code, m := s.run()
			if code != 0 || m.Outcome != "failed" || m.Reason != spec.ReasonSourceInvalid || s.ran() {
				t.Fatalf("exit %d manifest %+v harness ran=%v", code, m, s.ran())
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the hostile program ran")
			}
		})
	}

	for _, name := range []string{"a clean filter", "a hooks path", "a file-system monitor", "a source replaced by a symlink to another repository", "a .git that is a gitdir file",
		"a commondir that points at an agent-controlled repository", "an alternate object store", "a nested repository recorded as a gitlink whose configuration names a filter and a monitor",
		"an untracked nested repository whose configuration names a filter", "a header whose quoted subsection hides the section's key"} {
		t.Run("planted by the agent: "+name, func(t *testing.T) {
			_, body := v3Context(t, cfg, "context_v3_work_item_writer_two_repositories", nil)
			s := newScenario(t, 3, body, "")
			marker := filepath.Join(s.root, "hostile-ran")
			repo(t, filepath.Join(s.workspace, "backend"))
			repo(t, filepath.Join(s.workspace, "frontend"))
			// The harness stand-in cannot call the Go planters, so the planting is done by the
			// script through a hook the test runs right before the wrapper commits: here, the
			// script simply waits for the test's planter via a file handshake.
			ready, plant := filepath.Join(s.root, "ready"), filepath.Join(s.root, "planted")
			s.script = "echo change > backend/feature.txt\ntouch " + ready + "\nwhile [ ! -f " + plant + " ]; do sleep 0.05; done\n" + resultCompleted
			go func() {
				for {
					if _, err := os.Stat(ready); err == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				for _, hh := range hostiles(marker) {
					if hh.name == name {
						hh.plant(t, s, filepath.Join(s.workspace, "frontend"))
					}
				}
				_ = os.WriteFile(plant, nil, 0o644)
			}()
			code, m := s.run()
			if code != 0 || m.Outcome != "failed" || m.Reason != spec.ReasonSourceInvalid {
				t.Fatalf("exit %d manifest %+v", code, m)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the hostile program ran")
			}
		})
	}

	t.Run("the refusal list does not refuse an ordinary preparation configuration", func(t *testing.T) {
		for _, ok := range []string{
			"[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n",
			"[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = sha256\n",
			"\ufeff[core]\n\tbare = false\n# [filter \"x\"]\n; clean = x\n",
		} {
			if k := forbiddenConfigKey(ok); k != "" {
				t.Errorf("%q refused for %s", ok, k)
			}
		}
		for _, bad := range []string{"[core \"a]\"] worktree = /x\n", "[core \"a\\\"]\"]\n\tfsmonitor=x\n", "[pager]\ntest=x\n", "[remote \"o\"]\nvcs=x\n", "[submodule \"s\"]\nupdate=!x\n", "[trace2]\neventTarget=x\n",
			"[core]\n\talternateRefsCommand = x\n", "[sequence]\neditor=x\n", "[commit]\ntemplate=x\n", "[core]\nexcludesFile=x\n", "[core \"unterminated\n",
			"[filter \"a\"]\n", "[FILTER.a]\nclean=x\n", "[include]\npath=/x\n", "[core]\n\tWorkTree=/x\n", "[core\n", "[alias]\nst=status\n"} {
			if k := forbiddenConfigKey(bad); k == "" {
				t.Errorf("%q accepted", bad)
			}
		}
	})
}

// Test_unit_git_nested_repository_experiment records, as a test, the measurement
// that decides how the wrapper treats nested repositories (git 2.47 in the pinned
// image; re-run it when the image's git changes). In a checkout whose index
// records a gitlink, the nested repository's own configuration is honored:
//
//   - `git status` asks the nested repository (and so runs its fsmonitor and
//     clean filter) unless submodules are ignored; `-c diff.ignoreSubmodules=all`
//     stops it, and the wrapper sets it;
//   - `git add -A` runs the nested repository's clean filter on a file it must
//     hash, whatever the status options, with or without `.gitmodules` and under
//     `-c core.fsmonitor=false` (which propagates to the child but does not stop
//     filters). Nothing on the command line stops it, so the wrapper refuses a
//     checkout that contains a nested `.git` (noNestedRepository) instead.
//
// An untracked nested repository was measured as not descended into by either
// command. The test fails if the second behavior stops holding, which means the
// refusal could be relaxed and this comment re-read.
func Test_unit_git_nested_repository_experiment(t *testing.T) {
	s := &scenario{t: t, root: t.TempDir()}
	dir := filepath.Join(s.root, "outer")
	repo(t, dir)
	canary := filepath.Join(s.root, "canary")
	program := filepath.Join(s.root, "p.sh")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ntouch "+canary+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nestedRepo(t, s, dir, program, true)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "safe.directory=*", "-c", "core.fsmonitor=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + s.root}
		_ = cmd.Run()
	}
	exists := func() bool { _, err := os.Stat(canary); return err == nil }

	run("-c", "diff.ignoreSubmodules=all", "status", "--porcelain")
	if exists() {
		t.Fatal("status ran the nested repository's program although submodules are ignored")
	}
	run("add", "-A")
	if !exists() {
		t.Fatal("git add -A no longer runs a nested repository's clean filter: the nested-repository refusal may be relaxed (see the test comment)")
	}
}

// Test_unit_wrapper_branch_states: whatever branch state the agent leaves a checkout
// in, the wrapper ends it on the branch the plan names (or on HEAD for the writer's
// write source), with the agent's own commits kept, and the entrypoint's selection
// publishes only what the grant selects.
func Test_unit_wrapper_branch_states(t *testing.T) {
	cfg, _ := payload(t)
	agentCommit := "git -c user.name=Agent -c user.email=a@example.invalid commit -q -m agent"

	t.Run("a reader's source on a branch of the agent's own naming is moved onto the agent branch", func(t *testing.T) {
		c, body := v3Context(t, cfg, "context_v3_work_item_reader_first_attempt", nil)
		s := newScenario(t, 3, body, "cd frontend\ngit checkout -q -b mine\necho x > f.txt\ngit add f.txt\n"+agentCommit+"\ncd ..\n"+resultCompleted)
		repo(t, filepath.Join(s.workspace, "backend"))
		front := filepath.Join(s.workspace, "frontend")
		start := repo(t, front)
		if code, m := s.run(); code != 0 || m.Outcome != "completed" {
			t.Fatalf("exit %d manifest %+v", code, m)
		}
		want := "refs/heads/kodama/agents/wi_1042/run_01/test"
		if got := gitT(t, front, "symbolic-ref", "HEAD"); got != want {
			t.Fatalf("on %q, want %q", got, want)
		}
		if gitT(t, front, "rev-list", "--count", start+"..HEAD") != "1" {
			t.Fatal("the agent's own commit must be kept, with nothing added")
		}
		ups := entrypointSelection(t, c, c.Workspace.Sources[1], front, start)
		if len(ups) != 1 || ups[want] == "" {
			t.Fatalf("the grant selects the agent branch only, not %v", ups)
		}
	})

	t.Run("a reader's detached commit with a clean tree becomes the agent branch", func(t *testing.T) {
		c, body := v3Context(t, cfg, "context_v3_work_item_reader_first_attempt", nil)
		s := newScenario(t, 3, body, "cd frontend\necho x > f.txt\ngit add f.txt\n"+agentCommit+"\ncd ..\n"+resultCompleted)
		repo(t, filepath.Join(s.workspace, "backend"))
		front := filepath.Join(s.workspace, "frontend")
		start := repo(t, front)
		if code, m := s.run(); code != 0 || m.Outcome != "completed" {
			t.Fatalf("exit %d manifest %+v", code, m)
		}
		want := "refs/heads/kodama/agents/wi_1042/run_01/test"
		if gitT(t, front, "symbolic-ref", "HEAD") != want || gitT(t, front, "rev-list", "--count", start+"..HEAD") != "1" {
			t.Fatal("expected the agent's detached commit on the agent branch")
		}
		_ = c
	})

	t.Run("the writer's write source switched to another branch is committed there and still goes to the task branch only", func(t *testing.T) {
		c, body := v3Context(t, cfg, "context_v3_work_item_writer_two_repositories", nil)
		s := newScenario(t, 3, body, "cd backend\ngit checkout -q -b mine\necho x > f.txt\ncd ..\n"+resultCompleted)
		back := filepath.Join(s.workspace, "backend")
		start := repo(t, back)
		repo(t, filepath.Join(s.workspace, "frontend"))
		if code, m := s.run(); code != 0 || m.Outcome != "completed" {
			t.Fatalf("exit %d manifest %+v", code, m)
		}
		if gitT(t, back, "symbolic-ref", "HEAD") != "refs/heads/mine" || gitT(t, back, "rev-list", "--count", start+"..HEAD") != "1" {
			t.Fatal("the writer's change is committed on the branch the agent chose")
		}
		ups := entrypointSelection(t, c, c.Workspace.Sources[0], back, start)
		if len(ups) != 1 || ups["refs/heads/kodama/work/wi_1042"] == "" {
			t.Fatalf("only the task branch may be published, not %v", ups)
		}
	})
}

// Test_unit_wrapper_two_sources_one_repository: two checkouts of one repository get
// two distinct agent branches, and both are published to the one repository.
func Test_unit_wrapper_two_sources_one_repository(t *testing.T) {
	cfg, _ := payload(t)
	c, body := v3Context(t, cfg, "context_v3_work_item_reader_first_attempt", func(c *runtimev3.ContextV3) {
		second := &c.Workspace.Sources[1]
		second.RepositoryID, second.Path, second.RepositoryURL = c.Workspace.Sources[0].RepositoryID, "docs", c.Workspace.Sources[0].RepositoryURL
		c.Git.Repositories = c.Git.Repositories[:1]
		c.Workspace.Preparation.Sources[1].Path = "docs"
		set, err := c.Workspace.SourceSet()
		if err != nil {
			t.Fatal(err)
		}
		d, err := set.Digest()
		if err != nil {
			t.Fatal(err)
		}
		dg := runtimev1.Digest(d)
		c.Workspace.SourceSetDigest = &dg
	})
	s := newScenario(t, 3, body, "echo a > backend/a.txt\necho d > docs/d.txt\n"+resultCompleted)
	starts := map[string]string{"backend": repo(t, filepath.Join(s.workspace, "backend")), "docs": repo(t, filepath.Join(s.workspace, "docs"))}
	if code, m := s.run(); code != 0 || m.Outcome != "completed" {
		t.Fatalf("exit %d manifest %+v", code, m)
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitT(t, filepath.Dir(bare), "init", "-q", "--bare", "-b", "main", bare)
	refs := map[string]bool{}
	for i, p := range []string{"backend", "docs"} {
		dir := filepath.Join(s.workspace, p)
		ups := entrypointSelection(t, c, c.Workspace.Sources[i], dir, starts[p])
		if len(ups) != 1 {
			t.Fatalf("%s: %v", p, ups)
		}
		for ref, commit := range ups {
			refs[ref] = true
			gitT(t, dir, "push", "-q", bare, commit+":"+ref)
		}
	}
	if len(refs) != 2 {
		t.Fatalf("two sources of one repository shared a ref: %v", refs)
	}
	if out := gitT(t, bare, "for-each-ref", "--format=%(refname)"); len(strings.Split(out, "\n")) != 2 {
		t.Fatalf("the repository holds %q", out)
	}
}

// Test_unit_wrapper_old_context_unchanged: contexts v1 and v2 are read and run
// exactly as before: the same prompt, the same single request for v1, the same
// single-checkout commit on the pinned HEAD with no branch and no Git change, and
// the same harness environment.
func Test_unit_wrapper_old_context_unchanged(t *testing.T) {
	cfg, _ := payload(t)
	prof, base := cfg.Pins()

	v1Body := func(t *testing.T) []byte {
		c, err := runtimev1.DecodeRequest[runtimev1.Context](fixturePayload(t, v1Cases, "context_reader"))
		if err != nil {
			t.Fatal(err)
		}
		c.Profile, c.DesignBaseline = &prof, &base
		c.Outputs = []runtimev1.OutputDecl{conformanceDecl()}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	v2Body := func(t *testing.T) []byte {
		c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](fixturePayload(t, v2Cases, "context_v2_reader_first_attempt"))
		if err != nil {
			t.Fatal(err)
		}
		c.Profile, c.DesignBaseline = &prof, &base
		c.Outputs = []runtimev1.OutputDecl{conformanceDecl()}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	t.Run("the prompt of a v1 context is byte-identical to the one the original wrapper wrote", func(t *testing.T) {
		golden, err := os.ReadFile("testdata/prompt_v1.golden")
		if err != nil {
			t.Fatal(err)
		}
		pc := pinnedContext{Version: 1, WorkItem: &runtimev1.WorkItem{ID: "wi_1", ContentRevision: 2, Spec: "Add a flow.",
			AcceptanceCriteria: []runtimev1.AcceptanceCriterion{{ID: "AC-1", Text: "works"}}}}
		got := prompt(cfgWithGoldenPins(t), pc)
		if got != string(golden) {
			t.Fatalf("the v1 prompt changed:\n%s", got)
		}
		// Neither v1 nor v2 carries a repositories section.
		if strings.Contains(prompt(cfg, pinnedContext{Version: 2}), "## Repositories") {
			t.Fatal("a v2 prompt names repositories")
		}
	})

	for name, c := range map[string]struct {
		version int64
		body    func(*testing.T) []byte
		paths   []string
	}{
		"v1": {1, v1Body, []string{"/runtime/v1/attempts/att_01/context"}},
		"v2": {2, v2Body, []string{"/runtime/v1/attempts/att_01/context", "/runtime/v3/attempts/att_01/context", "/runtime/v2/attempts/att_01/context"}},
	} {
		t.Run(name+" context: one checkout committed on its HEAD, no branch, the conformance bound to that commit", func(t *testing.T) {
			s := newScenario(t, c.version, c.body(t), "echo change > feature.txt\n"+resultCompleted)
			start := repo(t, s.workspace)
			code, m := s.run()
			if code != 0 || m.Outcome != "completed" {
				t.Fatalf("exit %d manifest %+v", code, m)
			}
			if got := s.rt.paths(); !equalStrings(got, c.paths) {
				t.Fatalf("requests %v, want %v", got, c.paths)
			}
			head := gitT(t, s.workspace, "rev-parse", "HEAD")
			if head == start || gitT(t, s.workspace, "rev-parse", "HEAD^") != start {
				t.Fatal("expected one commit on the pinned start")
			}
			if out, err := exec.Command("git", "-C", s.workspace, "-c", "safe.directory=*", "symbolic-ref", "-q", "HEAD").Output(); err == nil {
				t.Fatalf("a v%d attempt must stay on its detached HEAD, on %s", c.version, out)
			}
			if out := gitT(t, s.workspace, "for-each-ref", "refs/heads/kodama"); out != "" {
				t.Fatalf("a branch was created: %s", out)
			}
			if rep := s.conformance(); string(rep.CandidateCommit) != head || rep.BaselineDigest != base.Digest {
				t.Fatalf("report: %+v", rep)
			}
		})
	}

	t.Run("a context nobody serves is a context mismatch, not a guess", func(t *testing.T) {
		s := newScenario(t, 4, []byte(`{}`), resultCompleted)
		repo(t, s.workspace)
		code, m := s.run()
		if code != 0 || m.Outcome != "failed" || m.Reason != spec.ReasonContextMismatch || s.ran() {
			t.Fatalf("exit %d manifest %+v ran=%v", code, m, s.ran())
		}
		if got := s.rt.paths(); len(got) != 3 {
			t.Fatalf("requests %v", got)
		}
	})

	t.Run("a pin mismatch still fails closed on every version", func(t *testing.T) {
		other := cfgOtherBaseline(t)
		_, otherBase := other.Pins()
		var body []byte
		c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](fixturePayload(t, v3Cases, "context_v3_work_item_writer_two_repositories"))
		if err != nil {
			t.Fatal(err)
		}
		c.Profile, c.DesignBaseline = &prof, &otherBase
		c.Outputs = []runtimev1.OutputDecl{conformanceDecl()}
		if body, err = json.Marshal(c); err != nil {
			t.Fatal(err)
		}
		s := newScenario(t, 3, body, resultCompleted)
		repo(t, filepath.Join(s.workspace, "backend"))
		repo(t, filepath.Join(s.workspace, "frontend"))
		code, m := s.run()
		if code != 0 || m.Outcome != "failed" || m.Reason != spec.ReasonContextMismatch || s.ran() {
			t.Fatalf("exit %d manifest %+v", code, m)
		}
	})

	t.Run("the original reason codes are all still defined", func(t *testing.T) {
		got := []string{spec.ReasonIncomplete, spec.ReasonPayloadInvalid, spec.ReasonConfigInvalid, spec.ReasonVersionMismatch, spec.ReasonStartFailed,
			spec.ReasonExitNonzero, spec.ReasonHarnessError, spec.ReasonIdle, spec.ReasonResultMissing, spec.ReasonResultInvalid, spec.ReasonContextMismatch,
			spec.ReasonOutputUndeclared, spec.ReasonCommitFailed, spec.ReasonAgentReportedFail}
		want := []string{"wrapper_incomplete", "payload_invalid", "config_invalid", "harness_version_mismatch", "harness_start_failed", "harness_exit_nonzero",
			"harness_error", "harness_idle", "result_missing", "result_invalid", "context_mismatch", "output_undeclared", "commit_failed", "agent_reported_failure"}
		if !equalStrings(got, want) {
			t.Fatalf("%v", got)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// cfgWithGoldenPins is the payload the golden prompt was generated from: the
// agentstest draft with the wrapper tuple of this image, tools unset (the same
// content `payload` builds), so the prompt is comparable byte for byte.
func cfgWithGoldenPins(t *testing.T) agents.WrapperConfig {
	t.Helper()
	cfg, _ := payload(t)
	return cfg
}

// cfgOtherBaseline is the same profile pinned to another design baseline.
func cfgOtherBaseline(t *testing.T) agents.WrapperConfig {
	t.Helper()
	d := agentstest.Draft(t, "coding")
	d.Wrapper = agents.Wrapper{ID: spec.WrapperID, Version: spec.WrapperVersion, Command: []string{spec.WrapperPath},
		Harness: agents.Harness{ID: spec.HarnessID, Version: spec.HarnessVersion}}
	d.Tools, d.Transcript.Record = nil, false
	moved, err := runtimev1.NewDesignBaseline([]string{"AR-01"}, nil,
		[]runtimev1.SourceRef{{Repository: "https://git.example.invalid/kodama.git", Revision: "4040404040404040404040404040404040404040", Path: "docs/specs/agent-runtime.md"}}, []string{"other-check"})
	if err != nil {
		t.Fatal(err)
	}
	d.DesignBaseline = moved
	p, err := agents.NewProfileVersion(agents.Candidate{
		Scope: domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}, ID: "ap_2", Content: agentstest.Content(t, d),
		CreatedBy: agents.Actor{ID: "u_1", Type: agents.ActorUser}, CreatedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := agents.Expand(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := agents.DecodeArgs(exp.Args)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

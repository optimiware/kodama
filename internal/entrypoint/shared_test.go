package entrypoint

// Unit qualification of the run_shared (v2) attempt (T-113): wire selection,
// verification of the controller-prepared /workspace, the begin fence, the
// absence of any checkout/reset/clean, and sealed publication. A real child
// process, a real local Git and scripted ports; no container and no cluster.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// ---- Git fixtures ----------------------------------------------------------------

func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		// The fixtures' own Git is never steered by a test's hostile environment.
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Prep", "GIT_AUTHOR_EMAIL=prep@kodama.invalid", "GIT_COMMITTER_NAME=Prep", "GIT_COMMITTER_EMAIL=prep@kodama.invalid")
}

// tgit runs the real git for fixtures (never the entrypoint's hardened one).
func tgit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func writeFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// preparedWorkspace builds what a preparation workload leaves: a repository with
// one pinned commit checked out. It returns the commit.
func preparedWorkspace(t testing.TB, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tgit(t, dir, "init", "--quiet", "--template=")
	writeFile(t, filepath.Join(dir, "README.md"), "# inventory\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, ".gitignore"), "build/\n*.log\n")
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "--quiet", "--no-gpg-sign", "-m", "pinned")
	return tgit(t, dir, "rev-parse", "HEAD")
}

// ---- the v2 port double --------------------------------------------------------------

// scriptV2 is the run_shared admission, context and begin. It records the order of
// its calls against the command's own marker files.
type scriptV2 struct {
	mu sync.Mutex

	ctx      runtimev2.ContextV2
	admit    func(runtimev1.AdmitRequest) (runtimev2.AdmitResponseV2, error)
	fetch    func() (runtimev2.ContextV2, error)
	begin    func(runtimev2.BeginRequest) (runtimev2.BeginResponse, error)
	calls    []string
	begun    []runtimev2.BeginRequest
	onBegin  func() // runs inside Begin, before it returns the acknowledgment
	derive   bool   // acknowledge with the derivation of the request (the honest server)
	ackState *runtimev2.WorkspaceState
}

func (s *scriptV2) AdmitV2(_ context.Context, req runtimev1.AdmitRequest) (runtimev2.AdmitResponseV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "admit")
	if s.admit != nil {
		return s.admit(req)
	}
	return runtimev2.AdmitResponseV2{Token: tokenInfo("rt_unit_token_0123456789abcdef", time.Hour, 2*time.Hour), Context: s.ctx}, nil
}

func (s *scriptV2) ContextV2(context.Context) (runtimev2.ContextV2, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "getContext")
	if s.fetch != nil {
		return s.fetch()
	}
	return s.ctx, nil
}

func (s *scriptV2) Begin(_ context.Context, req runtimev2.BeginRequest) (runtimev2.BeginResponse, error) {
	s.mu.Lock()
	s.calls = append(s.calls, "begin")
	s.begun = append(s.begun, req)
	fn, hook := s.begin, s.onBegin
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fn != nil {
		return fn(req)
	}
	state, ignored, err := runtimev2.DeriveBegin(s.ctx, req)
	if err != nil {
		return runtimev2.BeginResponse{}, err
	}
	if s.ackState != nil {
		state = *s.ackState
	}
	return runtimev2.BeginResponse{AttemptID: s.ctx.AttemptID, BeganAt: runtimev2.Timestamp(time.Now().UTC().Format(time.RFC3339Nano)), WorkspaceState: state, IgnoredPresent: ignored}, nil
}

func (s *scriptV2) count(op string) (n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == op {
			n++
		}
	}
	return
}

// sharedContext is a valid run_shared context running script under /bin/sh.
func sharedContext(t testing.TB, script string, ws runtimev2.WorkspaceV2) runtimev2.ContextV2 {
	t.Helper()
	c := runtimev2.ContextV2{
		ContextVersion: 2, RunModel: "run_shared",
		TenantID: "t_acme", ProjectID: "p_inventory", RunID: "run_01", StepID: "build", AttemptID: "att_unit01", ActionKey: "run_01.build",
		Definition: runtimev1.Definition{ID: "pd_01", Digest: runtimev1.Digest("sha256:" + strings.Repeat("d", 64))},
		Command:    runtimev1.Command{Image: testImage, Command: []string{"/bin/sh", "-c"}, Args: []string{script}, Env: map[string]string{}},
		Workspace:  ws, Endpoints: map[string]runtimev2.Endpoint{}, Egress: []string{},
		Run:    runtimev2.RunWindow{StartedAt: timestamp(-time.Minute), DeadlineAt: timestamp(time.Hour)},
		Policy: runtimev2.PolicyPin{ID: "run-v1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("f", 64))},
		Inputs: map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 30, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 64 << 10},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the fixture context is invalid: %v", err)
	}
	return c
}

func readerWorkspace(input, head string, lineage ...runtimev2.LineageEntry) runtimev2.WorkspaceV2 {
	if lineage == nil {
		lineage = []runtimev2.LineageEntry{}
	}
	return runtimev2.WorkspaceV2{
		Access: runtimev1.AccessRead, MountPath: "/workspace",
		Source:      &runtimev2.WorkspaceSource{RepositoryURL: "https://git.example.invalid/acme/inventory.git", InputCommit: runtimev1.GitCommit(input), HeadCommit: runtimev1.GitCommit(head)},
		Preparation: &runtimev2.PreparationPin{Commit: runtimev1.GitCommit(input), TreeID: runtimev1.GitCommit(strings.Repeat("3", 40)), ReceiptDigest: runtimev1.Digest("sha256:" + strings.Repeat("e", 64))},
		Lineage:     lineage,
	}
}

func writerWorkspace(input, head string) runtimev2.WorkspaceV2 {
	w := readerWorkspace(input, head)
	w.Access = runtimev1.AccessWrite
	branch, tip := "refs/heads/kodama/wi-1", runtimev1.GitCommit(head)
	w.Source.Branch, w.Source.ExpectedTip = &branch, &tip
	return w
}

func freeWorkspace(lineage ...runtimev2.LineageEntry) runtimev2.WorkspaceV2 {
	if lineage == nil {
		lineage = []runtimev2.LineageEntry{}
	}
	return runtimev2.WorkspaceV2{Access: runtimev1.AccessNone, MountPath: "/workspace", Lineage: lineage}
}

// sharedUnit is a Step on the run_shared wire over a temporary Pod layout.
type sharedUnit struct {
	*unit
	v2   *scriptV2
	mark string // a file the begin double creates and the command checks
}

func newShared(t *testing.T, script func(mark, ran string) string, ws func(unit *unit) (runtimev2.WorkspaceV2, string)) *sharedUnit {
	t.Helper()
	u := newUnit(t, "true")
	su := &sharedUnit{unit: u, mark: filepath.Join(u.root, "begun")}
	w, _ := ws(u)
	su.v2 = &scriptV2{ctx: sharedContext(t, script(su.mark, filepath.Join(u.root, "ran")), w)}
	u.cfg.RuntimeAPI = RuntimeAPIv2
	su.v2.onBegin = func() { writeFile(t, su.mark, "begun") }
	return su
}

func (s *sharedUnit) run() int {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s.step = NewStepV2(s.cfg, s.rt, s.v2, s.up)
	return s.step.Run(ctx)
}

// commandRan reports whether the command left its witness.
func (s *sharedUnit) ranFile() string { return filepath.Join(s.root, "ran") }

// beganThenRan is the command script that proves the begin fence: it creates its
// witness only if the begin double's marker already existed when it started.
func beganThenRan(mark, ran string) string { return fmt.Sprintf(`test -f %q && touch %q`, mark, ran) }

func nothing(string, string) string { return "true" }

func sourceBacked(readerOrWriter string) func(*unit) (runtimev2.WorkspaceV2, string) {
	return func(u *unit) (runtimev2.WorkspaceV2, string) {
		commit := preparedWorkspace(u.t, u.cfg.WorkspaceDir)
		if readerOrWriter == "write" {
			return writerWorkspace(commit, commit), commit
		}
		return readerWorkspace(commit, commit), commit
	}
}

func sourceFree(u *unit) (runtimev2.WorkspaceV2, string) {
	if err := os.MkdirAll(u.cfg.WorkspaceDir, 0o755); err != nil {
		u.t.Fatal(err)
	}
	return freeWorkspace(), ""
}

// ---- wire selection ----------------------------------------------------------------------

func Test_unit_entrypoint_v2_wire_selection(t *testing.T) {
	t.Run("the selector is explicit and strict", func(t *testing.T) {
		env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
		base := map[string]string{
			"KODAMA_SERVER_URL": "https://runtime.example.invalid", "KODAMA_ATTEMPT_ID": "att_unit01",
			"KODAMA_JOB_NAME": unitJob.Name, "KODAMA_JOB_UID": unitJob.UID, "KODAMA_POD_NAME": unitJob.PodName, "KODAMA_POD_UID": unitJob.PodUID,
		}
		with := func(k, v string) map[string]string {
			m := map[string]string{}
			for a, b := range base {
				m[a] = b
			}
			if k != "" {
				m[k] = v
			}
			return m
		}
		for _, tc := range []struct {
			name string
			env  map[string]string
			args []string
			want string
			err  bool
		}{
			{"unset keeps v1", with("", ""), nil, RuntimeAPIv1, false},
			{"v1", with("KODAMA_RUNTIME_API", "v1"), nil, RuntimeAPIv1, false},
			{"v2", with("KODAMA_RUNTIME_API", "v2"), nil, RuntimeAPIv2, false},
			{"step-scoped v2", with("KODAMA_STEP_RUNTIME_API", "v2"), nil, RuntimeAPIv2, false},
			{"flag overrides the environment", with("KODAMA_RUNTIME_API", "v1"), []string{"--runtime-api", "v2"}, RuntimeAPIv2, false},
			{"v3 selects the source-set wire", with("KODAMA_RUNTIME_API", "v3"), nil, RuntimeAPIv3, false},
			{"v4 is refused", with("KODAMA_RUNTIME_API", "v4"), nil, "", true},
			{"case matters", with("KODAMA_RUNTIME_API", "V2"), nil, "", true},
		} {
			cfg, err := ParseConfig(tc.args, env(tc.env), nil)
			if tc.err {
				if err == nil {
					t.Errorf("%s: an unsupported wire was accepted", tc.name)
				}
				continue
			}
			if err != nil || cfg.RuntimeAPI != tc.want || cfg.SharedRun() != (tc.want == RuntimeAPIv2) || cfg.SourceSetRun() != (tc.want == RuntimeAPIv3) {
				t.Errorf("%s: %q %v", tc.name, cfg.RuntimeAPI, err)
			}
		}
		var out, errb bytes.Buffer
		if code := Run(context.Background(), nil, env(with("KODAMA_RUNTIME_API", "v9")), &out, &errb); code != ExitUsage {
			t.Errorf("an unknown wire exits %d", code)
		}
	})

	t.Run("v2 uses the v2 attempt routes and v1 keeps the v1 routes", func(t *testing.T) {
		for _, wire := range []string{RuntimeAPIv1, RuntimeAPIv2} {
			srv := newWireServer(t, wire, "true")
			u := newUnit(t, "true")
			u.cfg.RuntimeAPI = wire
			u.cfg.ServerURL = srv.URL()
			if wire == RuntimeAPIv2 {
				if err := os.MkdirAll(u.cfg.WorkspaceDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			code := Execute(ctx, u.cfg)
			cancel()
			if code != ExitOK {
				t.Fatalf("%s: exit %d\n%s", wire, code, u.logs.String())
			}
			paths := srv.paths()
			if len(paths) < 4 {
				t.Fatalf("%s: paths %v", wire, paths)
			}
			for _, p := range paths {
				if !strings.HasPrefix(p, "/runtime/"+wire+"/attempts/att_unit01/") {
					t.Fatalf("%s: a request left the selected wire: %s (%v)", wire, p, paths)
				}
			}
			if wire == RuntimeAPIv2 && !contains(paths, "/runtime/v2/attempts/att_unit01/begin") {
				t.Fatalf("v2 never called begin: %v\n%s\n%+v", paths, u.logs.String(), srv.result)
			}
			if wire == RuntimeAPIv1 && contains(paths, "/runtime/v1/attempts/att_unit01/begin") {
				t.Fatalf("v1 called begin: %v", paths)
			}
		}
	})

	t.Run("a context of the other version is refused before any command", func(t *testing.T) {
		for _, tc := range []struct{ wire, answer string }{
			{RuntimeAPIv2, "v1ctx"},    // v2 selected, a v1 context answers
			{RuntimeAPIv1, "v2ctx"},    // v1 selected, a ContextV2 answers
			{RuntimeAPIv1, "mismatch"}, // v1 selected for a run_shared attempt: 409 context_version_mismatch
			{RuntimeAPIv2, "mismatch"},
		} {
			t.Run(tc.wire+" "+tc.answer, func(t *testing.T) {
				u := newUnit(t, "true")
				ran := filepath.Join(u.root, "ran")
				srv := newWireServer(t, tc.wire, fmt.Sprintf(`touch %q`, ran))
				srv.answer = tc.answer
				u.cfg.RuntimeAPI, u.cfg.ServerURL = tc.wire, srv.URL()
				if err := os.MkdirAll(u.cfg.WorkspaceDir, 0o755); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if code := Execute(ctx, u.cfg); code != ExitAdmission {
					t.Fatalf("exit %d\n%s", code, u.logs.String())
				}
				if _, err := os.Stat(ran); err == nil {
					t.Fatal("a command ran from a context of the other version")
				}
				for _, p := range srv.paths() {
					if strings.HasSuffix(p, "/begin") || strings.HasSuffix(p, "/result") {
						t.Fatalf("the attempt went on after a refused context: %v", srv.paths())
					}
				}
			})
		}
	})

	t.Run("the v2 wire refuses a context that is not the attempt's or differs on refetch", func(t *testing.T) {
		su := newShared(t, beganThenRan, sourceFree)
		su.v2.ctx.AttemptID = "att_someone_else"
		if code := su.run(); code != ExitAdmission || su.v2.count("begin") != 0 {
			t.Fatalf("a context for another attempt: exit %d, calls %v", code, su.v2.calls)
		}
		su = newShared(t, beganThenRan, sourceFree)
		other := su.v2.ctx
		other.StepID = "deploy"
		su.v2.fetch = func() (runtimev2.ContextV2, error) { return other, nil }
		su.wantExit(su.run(), ExitOK)
		wantReason(t, su.lastResult(), ReasonContextMismatch)
		if su.v2.count("begin") != 0 {
			t.Fatalf("begin followed an untrusted context: %v", su.v2.calls)
		}
		if _, err := os.Stat(su.ranFile()); err == nil {
			t.Fatal("the command ran")
		}
	})
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ---- verification and the begin fence ---------------------------------------------------------

func Test_unit_entrypoint_v2_verifies_workspace_and_begins_before_command(t *testing.T) {
	t.Run("a clean prepared tree: begin reports its head, then the command runs", func(t *testing.T) {
		su := newShared(t, beganThenRan, sourceBacked("read"))
		su.wantExit(su.run(), ExitOK)
		if len(su.v2.begun) != 1 {
			t.Fatalf("calls %v", su.v2.calls)
		}
		w := su.v2.begun[0].Workspace
		head := tgit(t, su.cfg.WorkspaceDir, "rev-parse", "HEAD")
		if w == nil || string(w.Head) != head || !w.TrackedClean || w.UntrackedPresent || w.IgnoredPresent {
			t.Fatalf("begin reported %+v, want a clean tree at %s", w, head)
		}
		if _, err := os.Stat(su.ranFile()); err != nil {
			t.Fatal("the command did not start after the acknowledgment (or never started)")
		}
		if r := su.lastResult(); r.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("result %+v", r)
		}
		order := strings.Join(su.v2.calls, ",")
		if order != "admit,getContext,begin" {
			t.Fatalf("order %s", order)
		}
	})

	t.Run("modified, untracked and ignored files are reported, not repaired", func(t *testing.T) {
		for _, tc := range []struct {
			name                      string
			mutate                    func(dir string)
			clean, untracked, ignored bool
		}{
			{"modified tracked file", func(d string) { writeFile(t, filepath.Join(d, "main.go"), "package changed\n") }, false, false, false},
			{"deleted tracked file", func(d string) { _ = os.Remove(filepath.Join(d, "README.md")) }, false, false, false},
			{"untracked file", func(d string) { writeFile(t, filepath.Join(d, "generated.txt"), "x") }, true, true, false},
			{"ignored file", func(d string) { writeFile(t, filepath.Join(d, "build", "out.bin"), "x") }, true, false, true},
			{"all three", func(d string) {
				writeFile(t, filepath.Join(d, "main.go"), "package changed\n")
				writeFile(t, filepath.Join(d, "generated.txt"), "x")
				writeFile(t, filepath.Join(d, "debug.log"), "x")
			}, false, true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				su := newShared(t, nothing, sourceBacked("read"))
				tc.mutate(su.cfg.WorkspaceDir)
				snapshot := dirSnapshot(t, su.cfg.WorkspaceDir)
				su.wantExit(su.run(), ExitOK)
				w := su.v2.begun[0].Workspace
				if w == nil || w.TrackedClean != tc.clean || w.UntrackedPresent != tc.untracked || w.IgnoredPresent != tc.ignored {
					t.Fatalf("begin reported %+v", w)
				}
				if got := dirSnapshot(t, su.cfg.WorkspaceDir); got != snapshot {
					t.Fatalf("verification changed the scratch:\n%s\n--\n%s", snapshot, got)
				}
			})
		}
	})

	t.Run("a head other than the run's head is reported as found", func(t *testing.T) {
		su := newShared(t, nothing, sourceBacked("read"))
		writeFile(t, filepath.Join(su.cfg.WorkspaceDir, "later.txt"), "later")
		tgit(t, su.cfg.WorkspaceDir, "add", "-A")
		tgit(t, su.cfg.WorkspaceDir, "commit", "--quiet", "--no-gpg-sign", "-m", "later")
		newHead := tgit(t, su.cfg.WorkspaceDir, "rev-parse", "HEAD")
		su.wantExit(su.run(), ExitOK)
		if w := su.v2.begun[0].Workspace; w == nil || string(w.Head) != newHead || string(w.Head) == string(su.v2.ctx.Workspace.Source.HeadCommit) || !w.TrackedClean {
			t.Fatalf("begin reported %+v", w)
		}
	})

	t.Run("a source-free attempt reports a null workspace and the scratch is never cleared", func(t *testing.T) {
		su := newShared(t, beganThenRan, sourceFree)
		ran := su.ranFile()
		su.v2.ctx.Command.Args = []string{fmt.Sprintf(`test -f %q && test "$(cat /dev/null; cat %q)" = kept && touch %q`, su.mark, filepath.Join(su.cfg.WorkspaceDir, "from-step-one.txt"), ran)}
		writeFile(t, filepath.Join(su.cfg.WorkspaceDir, "from-step-one.txt"), "kept")
		su.v2.ctx.Workspace.Lineage = []runtimev2.LineageEntry{{StepID: "one", AttemptID: "att_unit00", Access: runtimev1.AccessNone, BeganAt: timestamp(-time.Minute), WorkspaceState: runtimev2.StateNone}}
		su.wantExit(su.run(), ExitOK)
		if su.v2.begun[0].Workspace != nil {
			t.Fatalf("begin reported %+v for a source-free attempt", su.v2.begun[0].Workspace)
		}
		if _, err := os.Stat(ran); err != nil {
			t.Fatalf("the second step did not find the first step's file\n%s", su.logs.String())
		}
		if b, _ := os.ReadFile(filepath.Join(su.cfg.WorkspaceDir, "from-step-one.txt")); string(b) != "kept" {
			t.Fatal("the scratch's content was changed")
		}
		if left, _ := filepath.Glob(filepath.Join(su.cfg.WorkspaceDir, ".kodama-probe-*")); len(left) != 0 {
			t.Fatalf("the writability probe was left behind: %v", left)
		}
	})

	t.Run("verification failures stop before the begin and the command", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(su *sharedUnit)
		}{
			{"the mount is missing", func(su *sharedUnit) { _ = os.RemoveAll(su.cfg.WorkspaceDir) }},
			{"the mount is a file", func(su *sharedUnit) {
				_ = os.RemoveAll(su.cfg.WorkspaceDir)
				writeFile(t, su.cfg.WorkspaceDir, "not a directory")
			}},
			{"no repository", func(su *sharedUnit) { _ = os.RemoveAll(filepath.Join(su.cfg.WorkspaceDir, ".git")) }},
			{"the pinned commit is not in the store", func(su *sharedUnit) {
				su.v2.ctx.Workspace.Source.InputCommit = runtimev1.GitCommit(strings.Repeat("a", 40))
				su.v2.ctx.Workspace.Source.HeadCommit = su.v2.ctx.Workspace.Source.InputCommit
				su.v2.ctx.Workspace.Preparation.Commit = su.v2.ctx.Workspace.Source.InputCommit
			}},
			{"the head commit is not in the store", func(su *sharedUnit) {
				// A writer of this run published a commit the scratch does not hold.
				published := runtimev1.GitCommit(strings.Repeat("b", 40))
				su.v2.ctx.Workspace.Source.HeadCommit = published
				su.v2.ctx.Workspace.Lineage = []runtimev2.LineageEntry{{StepID: "gen", AttemptID: "att_unit00", Access: runtimev1.AccessWrite,
					BeganAt: timestamp(-time.Minute), WorkspaceState: runtimev2.StateClean, PublishedCommit: &published}}
			}},
			{".git is a symlink", func(su *sharedUnit) {
				real := filepath.Join(su.root, "elsewhere.git")
				if err := os.Rename(filepath.Join(su.cfg.WorkspaceDir, ".git"), real); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, filepath.Join(su.cfg.WorkspaceDir, ".git")); err != nil {
					t.Fatal(err)
				}
			}},
			{".git is a gitdir file", func(su *sharedUnit) {
				real := filepath.Join(su.root, "elsewhere.git")
				if err := os.Rename(filepath.Join(su.cfg.WorkspaceDir, ".git"), real); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(su.cfg.WorkspaceDir, ".git"), "gitdir: "+real+"\n")
			}},
			{"HEAD is a symlink", func(su *sharedUnit) {
				head := filepath.Join(su.cfg.WorkspaceDir, ".git", "HEAD")
				_ = os.Rename(head, filepath.Join(su.root, "HEAD.real"))
				if err := os.Symlink(filepath.Join(su.root, "HEAD.real"), head); err != nil {
					t.Fatal(err)
				}
			}},
			{"HEAD names a ref outside refs", func(su *sharedUnit) {
				writeFile(t, filepath.Join(su.cfg.WorkspaceDir, ".git", "HEAD"), "ref: ../../etc/passwd\n")
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				su := newShared(t, beganThenRan, sourceBacked("read"))
				ran := su.ranFile()
				su.v2.ctx.Command.Args = []string{fmt.Sprintf(`touch %q`, ran)}
				tc.setup(su)
				su.wantExit(su.run(), ExitOK)
				wantReason(t, su.lastResult(), ReasonWorkspaceUnverified)
				if su.v2.count("begin") != 0 {
					t.Fatalf("begin was called for an unverified workspace: %v", su.v2.calls)
				}
				if _, err := os.Stat(ran); err == nil {
					t.Fatal("the command ran in an unverified workspace")
				}
				if tc.name == "the mount is missing" {
					if _, err := os.Stat(su.cfg.WorkspaceDir); err == nil {
						t.Fatal("a missing mount was recreated")
					}
				}
			})
		}
	})

	t.Run("the source-free scratch must be writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		su := newShared(t, beganThenRan, sourceFree)
		if err := os.Chmod(su.cfg.WorkspaceDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(su.cfg.WorkspaceDir, 0o755) })
		su.wantExit(su.run(), ExitOK)
		wantReason(t, su.lastResult(), ReasonWorkspaceUnverified)
		if su.v2.count("begin") != 0 {
			t.Fatal("begin was called for a read-only scratch")
		}
	})

	t.Run("a refused or unacknowledged begin starts nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			want int
		}{
			{"cancelled before the begin", apiErr("begin", 403, runtimev1.CodeAttemptRevoked), ExitRevoked},
			{"unreachable", &TransportError{Op: "begin", Err: errors.New("connection reset")}, ExitAdmission},
			{"conflict", apiErr("begin", 409, domain.CodeIdempotencyConflict), ExitAdmission},
			{"contract violation", &ProtocolError{Op: "begin", Err: errors.New("bad body")}, ExitAdmission},
		} {
			t.Run(tc.name, func(t *testing.T) {
				su := newShared(t, beganThenRan, sourceBacked("read"))
				ran := su.ranFile()
				su.v2.ctx.Command.Args = []string{fmt.Sprintf(`touch %q`, ran)}
				su.v2.begin = func(runtimev2.BeginRequest) (runtimev2.BeginResponse, error) {
					return runtimev2.BeginResponse{}, tc.err
				}
				su.wantExit(su.run(), tc.want)
				if _, err := os.Stat(ran); err == nil {
					t.Fatal("the command ran without an acknowledged begin")
				}
				if su.rt.count("submitResult") != 0 {
					t.Fatal("a result was submitted for an attempt that never started")
				}
			})
		}
		t.Run("an acknowledgment that is not the derivation of the request", func(t *testing.T) {
			su := newShared(t, beganThenRan, sourceBacked("read"))
			ran := su.ranFile()
			su.v2.ctx.Command.Args = []string{fmt.Sprintf(`touch %q`, ran)}
			writeFile(t, filepath.Join(su.cfg.WorkspaceDir, "x.txt"), "untracked")
			clean := runtimev2.StateClean
			su.v2.ackState = &clean // claims clean although the tree has an untracked file
			su.wantExit(su.run(), ExitAdmission)
			if _, err := os.Stat(ran); err == nil {
				t.Fatal("the command ran after an inconsistent acknowledgment")
			}
		})
		t.Run("the command waits for the acknowledgment", func(t *testing.T) {
			su := newShared(t, beganThenRan, sourceBacked("read"))
			ran := su.ranFile()
			su.v2.ctx.Command.Args = []string{fmt.Sprintf(`test -f %q && touch %q`, su.mark, ran)}
			su.v2.onBegin = func() {
				time.Sleep(300 * time.Millisecond)
				if _, err := os.Stat(ran); err == nil {
					t.Error("the command started before the acknowledgment returned")
				}
				writeFile(t, su.mark, "begun")
			}
			su.wantExit(su.run(), ExitOK)
			if _, err := os.Stat(ran); err != nil {
				t.Fatal("the command did not run after the acknowledgment")
			}
		})
	})
}

// dirSnapshot is a deterministic listing of every file, link and mode under dir,
// content included, to prove verification changed nothing.
func dirSnapshot(t testing.TB, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&b, "%s %v %d", rel, fi.Mode(), fi.Size())
		if fi.Mode().IsRegular() && !strings.HasPrefix(rel, ".git"+string(filepath.Separator)+"objects") {
			data, _ := os.ReadFile(p)
			fmt.Fprintf(&b, " %x", data)
		}
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

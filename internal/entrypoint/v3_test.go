package entrypoint

// Unit qualification of the v3 workload programs (T-151): `kodama prepare` for a
// source set, the attempt's verification of every source, the scoped Git credential
// helper and the explicit pushes. Real Git against a local smart-HTTPS server
// (test/support/git); no cluster, no container.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	gitsupport "github.com/optimiware/kodama/test/support/git"
)

// ---- the v3 preparation ------------------------------------------------------------------

// scriptResV3 is the v3 resource port, recorded.
type scriptResV3 struct {
	mu       sync.Mutex
	ctx      runtimev3.ResourceContextV3
	calls    []string
	admits   []runtimev3.ResourceAdmitRequest
	receipts []runtimev3.PreparationReceiptRequestV3
	done     func(n int, req runtimev3.PreparationReceiptRequestV3) (runtimev3.PreparationReceiptResponse, error)
}

func (s *scriptResV3) token() runtimev3.ResourceTokenInfo {
	t := resourceToken(30*time.Minute, time.Hour)
	t.MaxExpiresAt = s.ctx.Preparation.DeadlineAt
	return t
}

func (s *scriptResV3) Dispose() { s.mu.Lock(); s.calls = append(s.calls, "dispose"); s.mu.Unlock() }

func (s *scriptResV3) AdmitResourceV3(_ context.Context, req runtimev3.ResourceAdmitRequest) (runtimev3.ResourceAdmitResponseV3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "admit")
	s.admits = append(s.admits, req)
	return runtimev3.ResourceAdmitResponseV3{Token: s.token(), Context: s.ctx}, nil
}

func (s *scriptResV3) RenewResourceToken(context.Context) (runtimev3.ResourceTokenResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "renew")
	return runtimev3.ResourceTokenResponse{Token: s.token()}, nil
}

func (s *scriptResV3) ResourceContextV3(context.Context) (runtimev3.ResourceContextV3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "getContext")
	return s.ctx, nil
}

func (s *scriptResV3) CompletePreparationV3(_ context.Context, req runtimev3.PreparationReceiptRequestV3) (runtimev3.PreparationReceiptResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "completePreparation")
	s.receipts = append(s.receipts, req)
	if s.done != nil {
		return s.done(len(s.receipts), req)
	}
	return runtimev3.PreparationReceiptResponse{ReceiptDigest: runtimev1.Digest("sha256:" + strings.Repeat("e", 64)), RecordedAt: timestamp(0)}, nil
}

type prepV3 struct {
	t        *testing.T
	root     string
	gs       *gitsupport.Server
	backend  *gitsupport.Repo
	frontend *gitsupport.Repo
	pinB     string // the pinned commits; the branches have moved on
	pinF     string
	rt       *scriptResV3
	cfg      ResourceConfig
	logs     *safeBuf
}

func newPrepV3(t *testing.T) *prepV3 {
	t.Helper()
	requireGit(t)
	gs := gitsupport.NewServer(t)
	p := &prepV3{t: t, root: t.TempDir(), gs: gs, rt: &scriptResV3{}, logs: &safeBuf{}}
	p.backend, p.frontend = gs.NewRepo(t, "backend"), gs.NewRepo(t, "frontend")
	p.pinB, p.pinF = p.backend.Initial, p.frontend.Initial
	// Both branches move past the pinned commits: the run pins, the checkout must
	// not follow the branch.
	p.backend.HumanPush(t, "refs/heads/main", "later", gitsupport.File{Path: "later.txt", Content: "later"})
	p.frontend.HumanPush(t, "refs/heads/main", "later", gitsupport.File{Path: "later.txt", Content: "later"})
	for _, d := range []string{"workspace", "scratch", "identity", "git"} {
		if err := os.MkdirAll(filepath.Join(p.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(p.root, "git", "credential"), gs.ReadToken+"\n")
	writeFile(t, filepath.Join(p.root, "git", "ca.pem"), mustRead(t, gs.CAFile))
	writeFile(t, filepath.Join(p.root, "identity", "admission"), unitAdmission+"\n")
	podName, podUID := "kr-0123456789abcdef01234567-x7k2p", "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"
	p.cfg = ResourceConfig{
		ServerURL: "https://runtime.example.invalid", RunID: "run_01", Key: "preparation", RuntimeAPI: RuntimeAPIv3,
		AdmissionCredentialFile: filepath.Join(p.root, "identity", "admission"),
		Identity:                runtimev2.ResourceIdentity{WorkloadName: "kr-0123456789abcdef01234567", WorkloadUID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", PodName: &podName, PodUID: &podUID},
		WorkspaceDir:            filepath.Join(p.root, "workspace"), GitDir: filepath.Join(p.root, "git"), ScratchDir: filepath.Join(p.root, "scratch"),
		AdmissionWait: 5 * time.Second,
		Logger:        slog.New(slog.NewJSONHandler(p.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	p.rt.ctx = p.context(p.pinB, p.pinF, 2)
	return p
}

func (p *prepV3) context(b, f string, attempts int64) runtimev3.ResourceContextV3 {
	return runtimev3.ResourceContextV3{
		ContextVersion: 3, ResourceKind: runtimev3.ResourcePreparation, TenantID: "t_acme", ProjectID: "p_inventory", RunID: "run_01", ResourceKey: "preparation",
		Preparation: &runtimev3.PreparationContextV3{
			Sources: []runtimev3.PreparationSource{
				{RepositoryID: "repo_01", RepositoryURL: p.gs.URL + "/backend.git", Commit: runtimev3.GitCommit(b), Path: "backend"},
				{RepositoryID: "repo_02", RepositoryURL: p.gs.URL + "/frontend.git", Commit: runtimev3.GitCommit(f), Path: "frontend"},
			},
			MountPath: "/workspace", TimeoutSeconds: 60, MaxBytes: 1 << 30, MaxAttempts: attempts, DeadlineAt: timestamp(time.Hour),
			Git: runtimev3.PreparationGit{GrantID: "gg_prep", BaseURL: p.gs.URL, CredentialFile: "/kodama/git/credential", CAFile: ptr("/kodama/git/ca.pem"), Repositories: []string{"repo_01", "repo_02"}},
		},
		Policy: runtimev3.PolicyPin{ID: "run-v1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("f", 64))},
	}
}

func (p *prepV3) run() int {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return newPreparationV3(p.cfg, p.rt).run(ctx)
}

func Test_unit_prepare_checks_out_source_set_once(t *testing.T) {
	t.Run("one admission checks out every source at its path and pinned commit and records one receipt", func(t *testing.T) {
		p := newPrepV3(t)
		if code := p.run(); code != ExitOK {
			t.Fatalf("exit %d\ncalls %v\n%s", code, p.rt.calls, p.logs.String())
		}
		if got := p.rt.calls; !reflect.DeepEqual(got, []string{"admit", "getContext", "completePreparation", "dispose"}) {
			t.Fatalf("calls %v: a preparation admits, reads its context, records one receipt and nothing else", got)
		}
		if len(p.rt.admits) != 1 || len(p.rt.receipts) != 1 {
			t.Fatalf("admits %d receipts %d", len(p.rt.admits), len(p.rt.receipts))
		}
		for path, pin := range map[string]string{"backend": p.pinB, "frontend": p.pinF} {
			dir := filepath.Join(p.cfg.WorkspaceDir, path)
			if head := tgit(t, dir, "rev-parse", "HEAD"); head != pin {
				t.Fatalf("%s: HEAD %s, want the pinned %s", path, head, pin)
			}
			if _, err := os.Stat(filepath.Join(dir, "later.txt")); err == nil {
				t.Fatalf("%s followed the branch past the pinned commit", path)
			}
			if b := tgit(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); b != "HEAD" {
				t.Fatalf("%s is on a branch %q, not detached", path, b)
			}
		}
		r := p.rt.receipts[0]
		if len(r.Sources) != 2 || !r.HooksDisabled || r.Submodules != "skipped" || !r.CredentialsRemoved || r.Attempts != 1 {
			t.Fatalf("receipt %+v", r)
		}
		for i, path := range []string{"backend", "frontend"} {
			tree := tgit(t, filepath.Join(p.cfg.WorkspaceDir, path), "rev-parse", "HEAD^{tree}")
			if r.Sources[i].Path != path || string(r.Sources[i].TreeID) != tree {
				t.Fatalf("receipt source %d = %+v, tree %s", i, r.Sources[i], tree)
			}
		}
		if err := runtimev3.CheckReceipt(p.rt.ctx, r); err != nil {
			t.Fatalf("the receipt is not valid for the assignment: %v", err)
		}
		a := p.rt.admits[0]
		if a.AdmissionCredential != unitAdmission || a.Nonce == "" || a.Validate() != nil {
			t.Fatalf("admit request %+v", a)
		}
	})

	t.Run("only the read-only credential reaches only the two repositories and nothing keeps it", func(t *testing.T) {
		p := newPrepV3(t)
		if code := p.run(); code != ExitOK {
			t.Fatalf("exit %d\n%s", code, p.logs.String())
		}
		var gets, writes int
		seen := map[string]bool{}
		for _, r := range p.gs.Requests() {
			switch {
			case r.Role == gitsupport.RoleWrite:
				t.Fatalf("a request was made with a write credential: %+v", r)
			case r.Role == gitsupport.RoleNone && !(r.Method == "GET" && strings.HasSuffix(r.Path, "/info/refs")):
				t.Fatalf("a request after the first challenge was anonymous: %+v", r) // only Git's first, unauthenticated probe carries no credential
			case r.Service == "git-receive-pack":
				writes++
			}
			if r.Method == "GET" && r.Role == gitsupport.RoleRead {
				gets++
				seen[r.Path] = true
			}
		}
		if writes != 0 || gets != 2 || !seen["/backend.git/info/refs"] || !seen["/frontend.git/info/refs"] {
			t.Fatalf("requests %+v: each repository is advertised exactly once and nothing is written", p.gs.Requests())
		}
		if _, err := os.Stat(filepath.Join(p.cfg.GitDir, "credential")); err == nil {
			t.Fatal("the credential file was left behind")
		}
		for _, form := range (preparationCredential{value: p.gs.ReadToken}).forms() {
			if strings.Contains(p.logs.String(), form) {
				t.Fatal("the credential is in the logs")
			}
			_ = filepath.WalkDir(p.cfg.WorkspaceDir, func(path string, d os.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					if b, _ := os.ReadFile(path); bytes.Contains(b, []byte(form)) {
						t.Fatalf("the credential is in %s", path)
					}
				}
				return nil
			})
		}
		for _, path := range []string{"backend", "frontend"} {
			cfgBytes, _ := os.ReadFile(filepath.Join(p.cfg.WorkspaceDir, path, ".git", "config"))
			for _, bad := range []string{"extraheader", "credential", "remote", "url", "helper"} {
				if strings.Contains(strings.ToLower(string(cfgBytes)), bad) {
					t.Fatalf("%s/.git/config keeps %q:\n%s", path, bad, cfgBytes)
				}
			}
		}
	})

	t.Run("a source that cannot be fetched fails the whole preparation inside its tries, with no receipt", func(t *testing.T) {
		p := newPrepV3(t)
		p.rt.ctx = p.context(p.pinB, strings.Repeat("9", 40), 2) // a commit the frontend does not have
		p.cfg.AdmissionWait = time.Second
		if code := p.run(); code != ExitCheckout {
			t.Fatalf("exit %d, want %d\n%s", code, ExitCheckout, p.logs.String())
		}
		if len(p.rt.receipts) != 0 {
			t.Fatal("a receipt was recorded for a failed preparation")
		}
		if n := strings.Count(p.logs.String(), "checkout try failed"); n != 2 {
			t.Fatalf("%d tries, want the 2 the policy allows inside the one workload", n)
		}
	})

	t.Run("a v2 selection keeps the v2 preparation and a context mismatch checks out nothing", func(t *testing.T) {
		p := newPrepV3(t)
		other := p.rt.ctx
		src := append([]runtimev3.PreparationSource{}, other.Preparation.Sources...)
		src[0].Commit = runtimev3.GitCommit(strings.Repeat("8", 40))
		prep := *other.Preparation
		prep.Sources = src
		other.Preparation = &prep
		p.rt.ctx = p.context(p.pinB, p.pinF, 1)
		fetched := other
		pp := &scriptResV3Fetch{scriptResV3: p.rt, fetched: fetched}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if code := newPreparationV3(p.cfg, pp).run(ctx); code != ExitAdmission {
			t.Fatalf("exit %d, want %d", code, ExitAdmission)
		}
		if entries, _ := os.ReadDir(p.cfg.WorkspaceDir); len(entries) != 0 {
			t.Fatalf("a mismatching context was acted on: %v", entries)
		}
	})
}

// scriptResV3Fetch answers GET /context with another context than admission did.
type scriptResV3Fetch struct {
	*scriptResV3
	fetched runtimev3.ResourceContextV3
}

func (s *scriptResV3Fetch) ResourceContextV3(context.Context) (runtimev3.ResourceContextV3, error) {
	return s.fetched, nil
}

// ---- verification of every source -----------------------------------------------------------

func Test_unit_entrypoint_verifies_all_sources(t *testing.T) {
	t.Run("every source is observed in order at begin and the command starts only after the acknowledgment", func(t *testing.T) {
		f := newV3Fixture(t, false, "")
		mark, ran := filepath.Join(f.root, "begun"), filepath.Join(f.root, "ran")
		f.script = beganThenRan(mark, ran)
		f.v3.ctx = f.context()
		f.v3.onBegin = func() { writeFile(t, mark, "begun") }
		f.wantOK()
		if !fileExists(ran) {
			t.Fatal("the command did not start after begin")
		}
		if len(f.v3.begun) != 1 {
			t.Fatalf("begin was made %d times", len(f.v3.begun))
		}
		b := f.v3.begun[0]
		if len(b.Sources) != 2 || b.Sources[0].Path != "backend" || b.Sources[1].Path != "frontend" {
			t.Fatalf("begin sources %+v", b.Sources)
		}
		for i, s := range []*v3Source{f.backend, f.frontend} {
			o := b.Sources[i]
			if o.Head != runtimev3.GitCommit(s.pinned) || !o.TrackedClean || o.UntrackedPresent || o.IgnoredPresent {
				t.Fatalf("observation of %s: %+v", s.path, o)
			}
		}
	})

	t.Run("the state of each source is reported and nothing is checked out, reset or cleaned", func(t *testing.T) {
		f := newV3Fixture(t, false, "true")
		writeFile(t, filepath.Join(f.backend.dir(f), "README.md"), "edited by an earlier step\n")
		writeFile(t, filepath.Join(f.frontend.dir(f), "scratch.txt"), "untracked\n")
		f.wantOK()
		o := f.v3.begun[0].Sources
		if o[0].TrackedClean || o[0].UntrackedPresent || !o[1].TrackedClean || !o[1].UntrackedPresent {
			t.Fatalf("observations %+v", o)
		}
		if b := mustRead(t, filepath.Join(f.backend.dir(f), "README.md")); b != "edited by an earlier step\n" {
			t.Fatalf("the tracked file was reset: %q", b)
		}
		if !fileExists(filepath.Join(f.frontend.dir(f), "scratch.txt")) {
			t.Fatal("an untracked file was cleaned")
		}
		for _, s := range []*v3Source{f.backend, f.frontend} {
			if head := tgit(t, s.dir(f), "rev-parse", "HEAD"); head != s.pinned {
				t.Fatalf("%s HEAD moved to %s", s.path, head)
			}
		}
		if n := len(f.gs.Requests()); n != 0 {
			t.Fatalf("a reader that changed nothing talked to the Git service %d times", n)
		}
	})

	for name, break_ := range map[string]func(*v3Fixture){
		"a missing source": func(f *v3Fixture) { _ = os.RemoveAll(f.frontend.dir(f)) },
		"a source that is not a repository": func(f *v3Fixture) {
			_ = os.RemoveAll(filepath.Join(f.frontend.dir(f), ".git"))
		},
		"a source without its pinned commit": func(f *v3Fixture) {
			dir := f.frontend.dir(f)
			_ = os.RemoveAll(dir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			tgit(t, dir, "init", "--quiet", "--template=") // an unrelated history
			writeFile(t, filepath.Join(dir, "other.txt"), "other")
			tgit(t, dir, "add", "-A")
			tgit(t, dir, "commit", "--quiet", "--no-gpg-sign", "-m", "unrelated")
		},
		"a source that is a symlink": func(f *v3Fixture) {
			dir, moved := f.frontend.dir(f), f.frontend.dir(f)+"-elsewhere"
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, dir); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name+" is refused before begin and before any command", func(t *testing.T) {
			f := newV3Fixture(t, false, "")
			ran := filepath.Join(f.root, "ran")
			f.script = "touch " + ran
			f.v3.ctx = f.context()
			break_(f)
			r := f.wantOK()
			f.wantFailed(r, ReasonWorkspaceUnverified)
			if f.v3.count("begin") != 0 || fileExists(ran) {
				t.Fatalf("begin %d, command ran %v", f.v3.count("begin"), fileExists(ran))
			}
		})
	}

	t.Run("a hostile repository configuration is never read", func(t *testing.T) {
		f := newV3Fixture(t, false, "true")
		marker := filepath.Join(f.root, "executed")
		hostile := "[core]\n\tfsmonitor = touch " + marker + "\n\thooksPath = " + f.root + "/hooks\n[include]\n\tpath = /nonexistent\n"
		for _, s := range []*v3Source{f.backend, f.frontend} {
			cfgPath := filepath.Join(s.dir(f), ".git", "config")
			old := mustRead(t, cfgPath)
			writeFile(t, cfgPath, old+hostile)
		}
		f.wantOK()
		if fileExists(marker) {
			t.Fatal("configuration written into a source's repository was executed")
		}
	})

	t.Run("a source-free attempt verifies only that the workspace is writable", func(t *testing.T) {
		f := newV3Fixture(t, false, "true")
		c := f.v3.ctx
		c.Workspace = runtimev3.WorkspaceV3{Access: runtimev3.AccessNone, MountPath: "/workspace", Sources: []runtimev3.SourceV3{}, Lineage: []runtimev3.LineageEntryV3{}}
		c.Git = nil
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		f.v3.ctx = c
		f.wantOK()
		if b := f.v3.begun[0]; b.Sources == nil || len(b.Sources) != 0 {
			t.Fatalf("a source-free begin observes %+v", b.Sources)
		}
	})
}

// ---- the scoped Git credential helper ---------------------------------------------------------

func Test_unit_entrypoint_git_credential_helper_scoped(t *testing.T) {
	const secret = "kc_0123456789abcdefABCDEF.-_~+/="
	dir := t.TempDir()
	file := filepath.Join(dir, "credential")
	writeFile(t, file, secret+"\n")
	const base = "https://git.example.invalid/git"
	get := func(t *testing.T, extra []string, req ...string) (string, int, string) {
		t.Helper()
		var out, errb bytes.Buffer
		args := append([]string{"--base-url", base, "--file", file}, extra...)
		code := RunGitCredentialHelper(append(args, "get"), strings.NewReader(strings.Join(req, "\n")+"\n\n"), &out, &errb)
		return out.String(), code, errb.String()
	}
	answer := "username=kodama\npassword=" + secret + "\n"

	t.Run("it answers for a repository beneath the base URL, with the conventional user", func(t *testing.T) {
		out, code, _ := get(t, nil, "protocol=https", "host=git.example.invalid", "path=git/t_acme/p_inventory/backend.git")
		if code != ExitOK || out != answer {
			t.Fatalf("exit %d, answer %q", code, out)
		}
		// Git may pass a user name of its own; the answer does not depend on it.
		if out, _, _ := get(t, nil, "protocol=https", "host=git.example.invalid", "username=someone", "path=git/t/p/r.git"); out != answer {
			t.Fatalf("answer %q", out)
		}
	})

	t.Run("it refuses every other host, port, scheme and path, with no output at all", func(t *testing.T) {
		for name, req := range map[string][]string{
			"another host":         {"protocol=https", "host=evil.example.invalid", "path=git/t/p/r.git"},
			"a host with a suffix": {"protocol=https", "host=git.example.invalid.evil.test", "path=git/t/p/r.git"},
			"another port":         {"protocol=https", "host=git.example.invalid:8443", "path=git/t/p/r.git"},
			"plain http":           {"protocol=http", "host=git.example.invalid", "path=git/t/p/r.git"},
			"a sibling path":       {"protocol=https", "host=git.example.invalid", "path=gitlab/t/p/r.git"},
			"another root":         {"protocol=https", "host=git.example.invalid", "path=other/t/p/r.git"},
			"a traversal":          {"protocol=https", "host=git.example.invalid", "path=git/../other/r.git"},
			"an encoded traversal": {"protocol=https", "host=git.example.invalid", "path=git/%2e%2e/other/r.git"},
			"no path":              {"protocol=https", "host=git.example.invalid"},
			"no host":              {"protocol=https", "path=git/t/p/r.git"},
			"no protocol":          {"host=git.example.invalid", "path=git/t/p/r.git"},
			"ssh":                  {"protocol=ssh", "host=git.example.invalid", "path=git/t/p/r.git"},
		} {
			out, code, errOut := get(t, nil, req...)
			if out != "" || code != ExitOK || strings.Contains(errOut, secret) {
				t.Fatalf("%s: exit %d, answer %q, stderr %q", name, code, out, errOut)
			}
		}
	})

	t.Run("a runtime grant's helper answers for its one repository only", func(t *testing.T) {
		repo := []string{"--repository-url", base + "/t_acme/p_inventory/backend.git"}
		if out, _, _ := get(t, repo, "protocol=https", "host=git.example.invalid", "path=git/t_acme/p_inventory/backend.git"); out != answer {
			t.Fatalf("the grant's repository: %q", out)
		}
		if out, _, _ := get(t, repo, "protocol=https", "host=git.example.invalid", "path=git/t_acme/p_inventory/frontend.git"); out != "" {
			t.Fatalf("another repository of the same service got %q", out)
		}
		if out, _, _ := get(t, repo, "protocol=https", "host=git.example.invalid", "path=git/t_acme/p_inventory/backend.git-evil/x"); out != "" {
			t.Fatalf("a repository whose name starts with the granted one got %q", out)
		}
	})

	t.Run("store and erase never store or answer, and an unusable file is never echoed", func(t *testing.T) {
		for _, op := range []string{"store", "erase"} {
			var out, errb bytes.Buffer
			code := RunGitCredentialHelper([]string{"--base-url", base, "--file", file, op}, strings.NewReader("protocol=https\nhost=git.example.invalid\npassword=x\n\n"), &out, &errb)
			if code != ExitOK || out.Len() != 0 || mustRead(t, file) != secret+"\n" {
				t.Fatalf("%s: exit %d out %q", op, code, out.String())
			}
		}
		bad := filepath.Join(dir, "bad")
		writeFile(t, bad, "not a credential with spaces\n")
		var out, errb bytes.Buffer
		code := RunGitCredentialHelper([]string{"--base-url", base, "--file", bad, "get"}, strings.NewReader("protocol=https\nhost=git.example.invalid\npath=git/x/y.git\n\n"), &out, &errb)
		if code == ExitOK || out.Len() != 0 || strings.Contains(errb.String(), "spaces") {
			t.Fatalf("exit %d out %q err %q", code, out.String(), errb.String())
		}
		for _, args := range [][]string{{"--file", file, "get"}, {"--base-url", "http://git.example.invalid", "--file", file, "get"}, {"--base-url", base, "get"}} {
			out.Reset()
			if code := RunGitCredentialHelper(args, strings.NewReader("\n"), &out, &errb); code != ExitUsage || out.Len() != 0 {
				t.Fatalf("%v: exit %d", args, code)
			}
		}
	})

	t.Run("the Git configuration names paths and the scope, never the credential", func(t *testing.T) {
		cfg, err := Config{GitHelper: []string{"/kodama/bin/kodama", GitCredentialHelperCommandName}}.credentialConfig(base, "", "/kodama/git/credential")
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(cfg, "\n")
		if strings.Contains(joined, secret) || !strings.Contains(joined, "credential.useHttpPath=true") ||
			!strings.Contains(joined, "credential."+base+".helper=!'/kodama/bin/kodama' 'git-credential-helper' --base-url '"+base+"' --file '/kodama/git/credential'") {
			t.Fatalf("configuration:\n%s", joined)
		}
		cfg, _ = Config{GitHelper: []string{"/bin/it's"}}.credentialConfig(base, base+"/a/b.git", "/kodama/git/seed-credential")
		if j := strings.Join(cfg, " "); !strings.Contains(j, `'/bin/it'\''s'`) || !strings.Contains(j, "credential."+base+"/a/b.git.helper=") || !strings.Contains(j, "--repository-url") {
			t.Fatalf("a repository-scoped configuration: %s", j)
		}
	})

	t.Run("Git offers the credential to the Git service and to no other server", func(t *testing.T) {
		requireGit(t)
		gs, other := gitsupport.NewServer(t), gitsupport.NewServer(t)
		gs.NewRepo(t, "backend")
		other.NewRepo(t, "backend")
		cred := filepath.Join(dir, "write")
		writeFile(t, cred, gs.WriteToken+"\n")
		cfg, err := Config{}.credentialConfig(gs.URL, "", cred) // the test binary is the helper
		if err != nil {
			t.Fatal(err)
		}
		g := &git{bin: "git", home: dir, pathEnv: os.Getenv("PATH"), cfg: cfg, group: true}
		g.caFile = gs.CAFile
		if _, err := g.run(context.Background(), dir, nil, "ls-remote", gs.URL+"/backend.git"); err != nil {
			t.Fatalf("the service's repository was not readable with the helper's credential: %v", err)
		}
		if n := gs.CountRequests(func(r gitsupport.Request) bool { return r.Role == gitsupport.RoleWrite }); n == 0 {
			t.Fatalf("the service never saw the credential: %+v", gs.Requests())
		}
		// A server on another port (the same host name): the helper never answers, so
		// the server sees an anonymous request and nothing more.
		g.caFile = other.CAFile
		if _, err := g.run(context.Background(), dir, nil, "ls-remote", other.URL+"/backend.git"); err == nil {
			t.Fatal("another server accepted the request")
		}
		for _, r := range other.Requests() {
			if r.Role != gitsupport.RoleNone {
				t.Fatalf("the credential was offered to another server: %+v", r)
			}
		}
		if len(other.Requests()) == 0 {
			t.Fatal("the other server saw nothing; the test proves nothing")
		}
	})
}

// ---- explicit pushes ------------------------------------------------------------------------------

const gitCommitAs = `git -c user.name=agent -c user.email=agent@example.invalid -c commit.gpgsign=false commit -q`

func Test_unit_entrypoint_pushes_agent_branch(t *testing.T) {
	branch := v3Agent + "analysis"

	t.Run("a branch the command left checked out in the run's namespace is pushed and reported with its push record", func(t *testing.T) {
		f := newV3Fixture(t, false, "")
		f.script = `cd frontend && git checkout -q -b kodama/agents/wi_1042/run_01/analysis && echo findings > a.txt && git add a.txt && ` + gitCommitAs + ` -m findings && echo unstaged > u.txt`
		f.v3.ctx = f.context()
		r := f.wantOK()
		if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 1 {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
		got := r.Commits[0]
		local := tgit(t, f.frontend.dir(f), "rev-parse", "refs/heads/kodama/agents/wi_1042/run_01/analysis")
		if got.RepositoryID != "repo_02" || got.Ref != branch || string(got.Commit) != local || got.PushID != "push_1" {
			t.Fatalf("commit output %+v, want repo_02 %s %s push_1", got, branch, local)
		}
		if tip := tipOf(t, f.frontend, branch); tip != local {
			t.Fatalf("the service's %s is %s, want %s", branch, tip, local)
		}
		if out := f.frontend.repo.Git(t, "ls-tree", "-r", "--name-only", branch); strings.Contains(out, "u.txt") || !strings.Contains(out, "a.txt") {
			t.Fatalf("the pushed tree is %q: only what the command committed is published by a reader", out)
		}
		// Nothing else moved: not the default branch, not the task branch, not the other repository.
		for _, s := range []*v3Source{f.backend, f.frontend} {
			if tip := tipOf(t, s, "refs/heads/main"); tip != s.pinned {
				t.Fatalf("%s: main moved to %s", s.path, tip)
			}
			if tip := tipOf(t, s, v3Task); tip != s.pinned {
				t.Fatalf("%s: the task branch moved to %s", s.path, tip)
			}
		}
		if n := f.backend.repo.Git(t, "for-each-ref", "refs/heads/kodama/agents"); n != "" {
			t.Fatalf("the unchanged repository got refs: %s", n)
		}
		// The credential was the Git credential, scoped to the service, and the push used the write role only.
		if f.gs.CountRequests(func(r gitsupport.Request) bool {
			return r.Service == "git-receive-pack" && r.Method == "POST" && r.Role != gitsupport.RoleWrite
		}) != 0 {
			t.Fatalf("a push was made without the attempt's credential: %+v", f.gs.Requests())
		}
	})

	t.Run("a command that commits in both repositories reports both, ordered", func(t *testing.T) {
		f := newV3Fixture(t, false, "")
		mk := func(dir string) string {
			return `(cd ` + dir + ` && git checkout -q -b kodama/agents/wi_1042/run_01/analysis && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x)`
		}
		f.script = mk("frontend") + " && " + mk("backend")
		f.v3.ctx = f.context()
		r := f.wantOK()
		if len(r.Commits) != 2 || r.Commits[0].RepositoryID != "repo_01" || r.Commits[1].RepositoryID != "repo_02" {
			t.Fatalf("commits: %s", describe(r.Commits))
		}
	})

	for name, script := range map[string]string{
		"a branch outside the run's namespace":          `cd frontend && git checkout -q -b feature/x && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`,
		"another run's namespace":                       `cd frontend && git checkout -q -b kodama/agents/wi_1042/run_99/x && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`,
		"the default branch":                            `cd frontend && git checkout -q -b main && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`,
		"a detached HEAD":                               `cd frontend && git checkout -q --detach && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`,
		"the task branch, which a reader's grant lacks": `cd backend && git checkout -q -b kodama/work/wi_1042 && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`,
		"a namespace branch with no commit of its own":  `cd frontend && git checkout -q -b kodama/agents/wi_1042/run_01/empty`,
		"an uncommitted tree":                           `cd backend && echo x > n.txt`,
	} {
		t.Run(name+" publishes nothing", func(t *testing.T) {
			f := newV3Fixture(t, false, script)
			r := f.wantOK()
			if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 0 {
				t.Fatalf("result %+v\n%s", r, f.logs.String())
			}
			if n := f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" }); n != 0 {
				t.Fatalf("%d receive requests", n)
			}
			for _, s := range []*v3Source{f.backend, f.frontend} {
				if refs := s.repo.Git(t, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/kodama/work/wi_1042\nrefs/heads/main" {
					t.Fatalf("%s refs: %s", s.path, refs)
				}
			}
		})
	}

	t.Run("a commit that carries the Git credential is never pushed", func(t *testing.T) {
		f := newV3Fixture(t, false, "")
		f.script = `cd frontend && git checkout -q -b kodama/agents/wi_1042/run_01/analysis && cp ` + filepath.Join(f.cfg.GitDir, "credential") + ` leak.txt && git add leak.txt && ` + gitCommitAs + ` -m leak`
		f.v3.ctx = f.context()
		r := f.wantOK()
		f.wantFailed(r, ReasonSecretInSource)
		if tip := tipOf(t, f.frontend, branch); tip != "" {
			t.Fatalf("a commit holding the credential was pushed: %s", tip)
		}
		if strings.Contains(f.logs.String(), f.gs.WriteToken) {
			t.Fatal("the credential is in the logs")
		}
	})

	t.Run("a destination ref named after a secret publishes nothing and is never repeated", func(t *testing.T) {
		const injected = "injected-secret-value-77aa"
		for name, writer := range map[string]bool{"a reader's agent branch": false, "a writer's agent branch": true} {
			dir := "frontend"
			if writer {
				dir = "backend"
			}
			for _, secret := range []string{"$DB_PASS", "CRED"} {
				f := newV3Fixture(t, writer, "")
				if secret == "CRED" {
					secret = "$(cat " + filepath.Join(f.cfg.GitDir, "credential") + ")"
				}
				f.script = `cd ` + dir + ` && git checkout -q -b "kodama/agents/wi_1042/run_01/` + secret + `" && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`
				f.v3.ctx = f.context()
				c := f.v3.ctx
				c.Secrets = []runtimev1.SecretBinding{secretBinding("db-pass", "secret://project/db-pass", runtimev1.DeliveryEnv)}
				f.v3.ctx = c
				f.rt.resolve = func(r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
					return runtimev1.SecretResolveResponse{Ref: r.Ref, Version: 1, Name: "db-pass", Delivery: runtimev1.DeliveryEnv, ValueBase64: base64.StdEncoding.EncodeToString([]byte(injected))}, nil
				}
				r := f.wantOK()
				f.wantFailed(r, ReasonSecretInSource)
				if n := f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" }); n != 0 {
					t.Fatalf("%s (%s): %d receive requests reached the server", name, secret, n)
				}
				for _, s := range []*v3Source{f.backend, f.frontend} {
					if refs := s.repo.Git(t, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/kodama/work/wi_1042\nrefs/heads/main" {
						t.Fatalf("%s: %s refs: %s", name, s.path, refs)
					}
				}
				result, _ := json.Marshal(f.v3.results)
				hay := string(result) + f.logs.String()
				for _, obj := range f.up.objects {
					hay += string(obj)
				}
				for _, v := range []string{injected, f.gs.WriteToken} {
					if strings.Contains(hay, v) {
						t.Fatalf("%s (%s): a secret is in a result, log or upload", name, secret)
					}
				}
			}
		}
	})

	t.Run("a denied push fails the attempt with the service's refusal and reports nothing", func(t *testing.T) {
		f := newV3Fixture(t, false, "")
		f.script = `cd frontend && git checkout -q -b kodama/agents/wi_1042/run_01/analysis && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`
		f.v3.ctx = f.context()
		// The service revokes the credential: every push is refused.
		writeFile(t, filepath.Join(f.cfg.GitDir, "credential"), "revoked-credential-0123456789\n")
		r := f.wantOK()
		f.wantFailed(r, ReasonPublicationUnsettled)
		if tipOf(t, f.frontend, branch) != "" {
			t.Fatal("a refused push changed the service")
		}
	})
}

func Test_unit_entrypoint_writer_pushes_task_branch(t *testing.T) {
	t.Run("the writer commits the whole tree of the write source and pushes the task branch with a lease", func(t *testing.T) {
		f := newV3Fixture(t, true, "mkdir -p backend/sub && echo changed > backend/README.md && echo new > backend/sub/x.txt && echo ro > frontend/ignored-by-nobody.txt")
		r := f.wantOK()
		if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 1 {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
		c := r.Commits[0]
		if c.RepositoryID != "repo_01" || c.Ref != v3Task || c.PushID != "push_1" {
			t.Fatalf("commit output %+v", c)
		}
		tip := tipOf(t, f.backend, v3Task)
		if tip == f.backend.pinned || tip != string(c.Commit) {
			t.Fatalf("the task branch is %s, the output says %s", tip, c.Commit)
		}
		if parent := f.backend.repo.Git(t, "rev-parse", tip+"^"); parent != f.backend.pinned {
			t.Fatalf("the commit's parent is %s, want the pinned commit", parent)
		}
		if out := f.backend.repo.Git(t, "ls-tree", "-r", "--name-only", tip); !strings.Contains(out, "sub/x.txt") {
			t.Fatalf("the pushed tree %q misses the new file", out)
		}
		if author := f.backend.repo.Git(t, "log", "-1", "--format=%an <%ae>", tip); author != "Kodama <step@kodama.invalid>" {
			t.Fatalf("author %q", author)
		}
		// The default branch is never touched, and the read source is never written.
		if m := tipOf(t, f.backend, "refs/heads/main"); m != f.backend.pinned {
			t.Fatalf("main moved to %s", m)
		}
		if tipOf(t, f.frontend, v3Task) != f.frontend.pinned || f.gs.CountRequests(func(r gitsupport.Request) bool {
			return strings.HasPrefix(r.Path, "/frontend.git") && r.Service == "git-receive-pack"
		}) != 0 {
			t.Fatal("the read source was written")
		}
		// The shared workspace follows the published commit and is clean for the next holder.
		if head := tgit(t, f.backend.dir(f), "rev-parse", "HEAD"); head != tip {
			t.Fatalf("the workspace HEAD is %s, want the published %s", head, tip)
		}
		if st := tgit(t, f.backend.dir(f), "status", "--porcelain"); st != "" {
			t.Fatalf("the workspace is dirty after the push:\n%s", st)
		}
	})

	t.Run("commits the command made are kept and uncommitted work is added after them", func(t *testing.T) {
		f := newV3Fixture(t, true, `cd backend && echo one > one.txt && git add one.txt && `+gitCommitAs+` -m own && echo two > two.txt`)
		r := f.wantOK()
		if len(r.Commits) != 1 {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
		tip := string(r.Commits[0].Commit)
		if n := f.backend.repo.Git(t, "rev-list", "--count", f.backend.pinned+".."+tip); n != "2" {
			t.Fatalf("%s commits on top of the pinned one, want the command's and the entrypoint's", n)
		}
		if out := f.backend.repo.Git(t, "ls-tree", "-r", "--name-only", tip); !strings.Contains(out, "one.txt") || !strings.Contains(out, "two.txt") {
			t.Fatalf("tree %q", out)
		}
	})

	t.Run("the writer also pushes the agent branch the command left checked out, in one receive", func(t *testing.T) {
		f := newV3Fixture(t, true, `cd backend && git checkout -q -b kodama/agents/wi_1042/run_01/work && echo one > one.txt && git add one.txt && `+gitCommitAs+` -m own && echo two > two.txt`)
		r := f.wantOK()
		if len(r.Commits) != 2 || r.Commits[0].Ref != v3Agent+"work" || r.Commits[1].Ref != v3Task || r.Commits[0].PushID != r.Commits[1].PushID {
			t.Fatalf("commits: %s", describe(r.Commits))
		}
		if n := f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" && r.Method == "POST" }); n != 1 {
			t.Fatalf("%d receives, want 1 atomic push", n)
		}
	})

	t.Run("an unchanged writer pushes and reports nothing", func(t *testing.T) {
		f := newV3Fixture(t, true, "true")
		r := f.wantOK()
		if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 0 {
			t.Fatalf("result %+v", r)
		}
		if n := f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" }); n != 0 {
			t.Fatalf("%d receive requests", n)
		}
	})

	t.Run("a task branch that moved is a conflict: the lease holds and nothing is overwritten", func(t *testing.T) {
		f := newV3Fixture(t, true, "echo mine > backend/mine.txt")
		var human string
		f.v3.onBegin = func() {
			human = f.backend.repo.HumanPush(t, v3Task, "someone else", gitsupport.File{Path: "theirs.txt", Content: "theirs"})
		}
		r := f.wantOK()
		f.wantFailed(r, ReasonPublicationConflict)
		if tip := tipOf(t, f.backend, v3Task); tip != human {
			t.Fatalf("the task branch is %s, want the other writer's %s", tip, human)
		}
	})

	t.Run("a lost acknowledgment is repeated and, when the update had applied, never reported without its push record", func(t *testing.T) {
		f := newV3Fixture(t, true, "echo mine > backend/mine.txt")
		f.gs.DropNextReceivePackAcks(1)
		r := f.wantOK()
		f.wantFailed(r, ReasonPublicationUnsettled)
		if tip := tipOf(t, f.backend, v3Task); tip == f.backend.pinned {
			t.Fatal("the dropped acknowledgment was supposed to follow an applied update")
		}
	})

	t.Run("a result that does not descend from the expected tip is never pushed", func(t *testing.T) {
		f := newV3Fixture(t, true, `cd backend && echo x > x.txt && git add x.txt && `+gitCommitAs+` --amend -q -m rewritten`)
		r := f.wantOK()
		f.wantFailed(r, ReasonSourceFailed)
		if tipOf(t, f.backend, v3Task) != f.backend.pinned {
			t.Fatal("a rewritten history was pushed")
		}
	})

	t.Run("the Git credential in the tree publishes nothing", func(t *testing.T) {
		f := newV3Fixture(t, true, "")
		f.script = "cp " + filepath.Join(f.cfg.GitDir, "credential") + " backend/leak.txt"
		f.v3.ctx = f.context()
		r := f.wantOK()
		f.wantFailed(r, ReasonSecretInSource)
		if tipOf(t, f.backend, v3Task) != f.backend.pinned || f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" }) != 0 {
			t.Fatal("the credential left the Pod")
		}
	})

	t.Run("an injected secret in the tree publishes nothing, in any source", func(t *testing.T) {
		const secret = "injected-secret-value-77aa"
		f := newV3Fixture(t, true, `echo "$DB_PASS" > backend/leak.txt`)
		c := f.v3.ctx
		c.Secrets = []runtimev1.SecretBinding{secretBinding("db-pass", "secret://project/db-pass", runtimev1.DeliveryEnv)}
		f.v3.ctx = c
		f.rt.resolve = func(r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
			return runtimev1.SecretResolveResponse{Ref: r.Ref, Version: 1, Name: "db-pass", Delivery: runtimev1.DeliveryEnv, ValueBase64: base64.StdEncoding.EncodeToString([]byte(secret))}, nil
		}
		r := f.wantOK()
		f.wantFailed(r, ReasonSecretInSource)
		if tipOf(t, f.backend, v3Task) != f.backend.pinned {
			t.Fatal("an injected secret was pushed")
		}
		if strings.Contains(f.logs.String(), secret) {
			t.Fatal("the secret is in the logs")
		}
	})

	t.Run("hooks, filters and configuration written into the workspace are not run by the entrypoint's commit", func(t *testing.T) {
		f := newV3Fixture(t, true, "echo changed > backend/README.md")
		marker := filepath.Join(f.root, "executed")
		dir := f.backend.dir(f)
		writeFile(t, filepath.Join(dir, ".git", "hooks", "pre-commit"), "#!/bin/sh\ntouch "+marker+"\n")
		_ = os.Chmod(filepath.Join(dir, ".git", "hooks", "pre-commit"), 0o755)
		cfgPath := filepath.Join(dir, ".git", "config")
		writeFile(t, cfgPath, mustRead(t, cfgPath)+"[filter \"evil\"]\n\tclean = touch "+marker+"\n[core]\n\thooksPath = "+filepath.Join(dir, ".git", "hooks")+"\n")
		writeFile(t, filepath.Join(dir, ".git", "info", "attributes"), "* filter=evil\n")
		r := f.wantOK()
		if len(r.Commits) != 1 {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
		if fileExists(marker) {
			t.Fatal("something the workspace configured was executed with the attempt's authority")
		}
	})
}

func Test_unit_entrypoint_no_publisher_for_internal(t *testing.T) {
	f := newV3Fixture(t, true, "echo x > backend/x.txt && echo y > frontend/y.txt")
	r := f.wantOK()
	if len(r.Commits) != 1 {
		t.Fatalf("result %+v", r)
	}
	// The publisher is the entrypoint with its grant: no proposal, no bundle, no v1 result.
	if f.rt.count("proposeSource") != 0 || f.rt.count("submitResult") != 0 || f.v3.count("submitResult") != 1 {
		t.Fatalf("v1 calls %v, v3 calls %v", f.rt.calls, f.v3.calls)
	}
	for _, q := range f.rt.reserved {
		if q.Role == runtimev1.RoleBundle || q.Name == "source.bundle" {
			t.Fatalf("a bundle was reserved: %+v", q)
		}
	}
	// The v3 route table has no source-proposal route, and the v3 result has no sourceCommit.
	for _, op := range runtimev3.Operations {
		if op.ID == "proposeSource" || strings.HasSuffix(op.Path, "/source") {
			t.Fatalf("v3 declares a source-proposal route: %+v", op)
		}
	}
	if r.Outputs == nil || reflect.ValueOf(r).FieldByName("SourceCommit").IsValid() {
		t.Fatal("a v3 result carries a source commit")
	}
	// A seed or integration push never happened for an attempt that asked for neither.
	if n := f.backend.repo.Git(t, "for-each-ref", "--format=%(refname)", "refs/heads"); n != "refs/heads/kodama/work/wi_1042\nrefs/heads/main" {
		t.Fatalf("refs %s", n)
	}
	// The runtime-side authority of the internal repository is the grant alone: the
	// checkout credential file of v1/v2 is neither read nor required.
	if _, err := os.Stat(f.cfg.CheckoutCredentialFile); err == nil {
		t.Fatal("the fixture left a v1 checkout credential")
	}
}

// ---- v1 and v2 are unchanged --------------------------------------------------------------------------

func Test_unit_entrypoint_old_contexts_unchanged(t *testing.T) {
	t.Run("a v1 writer still proposes a bundle and reports a source commit", func(t *testing.T) {
		u := newUnit(t, "echo more >> new.txt")
		u.withRepo(runtimev1.AccessWrite, "", "")
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		if r.Outcome != runtimev1.OutcomeCompleted || r.SourceCommit == nil || len(u.rt.proposals) != 1 {
			t.Fatalf("result %+v proposals %d\n%s", r, len(u.rt.proposals), u.logs.String())
		}
		if u.step.v2 != nil || u.step.v3 != nil {
			t.Fatal("a v1 step carries a newer wire")
		}
	})

	t.Run("a v2 writer still publishes through the source proposal", func(t *testing.T) {
		su := newShared(t, func(string, string) string { return "echo more >> new.txt" }, sourceBacked("write"))
		su.wantExit(su.run(), ExitOK)
		if r := su.lastResult(); r.Outcome != runtimev1.OutcomeCompleted || r.SourceCommit == nil || len(su.rt.proposals) != 1 {
			t.Fatalf("result %+v proposals %d\n%s", r, len(su.rt.proposals), su.logs.String())
		}
		if su.step.v3 != nil {
			t.Fatal("a v2 step carries the v3 wire")
		}
	})

	t.Run("each wire refuses the other versions' contexts before any command", func(t *testing.T) {
		for _, tc := range []struct{ wire, answer string }{
			{RuntimeAPIv1, "v3ctx"}, {RuntimeAPIv2, "v3ctx"}, {RuntimeAPIv3, "v2ctx"}, {RuntimeAPIv3, "v1ctx"},
		} {
			srv := newWireServerV3(t, tc.wire, tc.answer)
			u := newUnit(t, "touch "+filepath.Join(t.TempDir(), "ran"))
			u.cfg.RuntimeAPI, u.cfg.ServerURL = tc.wire, srv.URL()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			code := Execute(ctx, u.cfg)
			cancel()
			if code != ExitAdmission {
				t.Fatalf("wire %s answered %s: exit %d, want %d\n%s", tc.wire, tc.answer, code, ExitAdmission, u.logs.String())
			}
			if len(srv.paths()) == 0 || srv.sawBegin() {
				t.Fatalf("wire %s: paths %v", tc.wire, srv.paths())
			}
		}
	})

	t.Run("the v3 wire speaks only /runtime/v3 and a v1 or v2 client has no v3 operations", func(t *testing.T) {
		srv := newWireServerV3(t, RuntimeAPIv3, "")
		u := newUnit(t, "true")
		u.cfg.RuntimeAPI, u.cfg.ServerURL = RuntimeAPIv3, srv.URL()
		if err := os.MkdirAll(u.cfg.WorkspaceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		code := Execute(ctx, u.cfg)
		cancel()
		if code != ExitOK {
			t.Fatalf("exit %d\n%s", code, u.logs.String())
		}
		for _, p := range srv.paths() {
			if !strings.HasPrefix(p, "/runtime/v3/attempts/att_unit01/") {
				t.Fatalf("a request left the v3 wire: %s", p)
			}
		}
		if srv.beginBody != `{"sources":[]}` || !strings.Contains(srv.resultBody, `"commits":[]`) || strings.Contains(srv.resultBody, "sourceCommit") {
			t.Fatalf("begin %s result %s", srv.beginBody, srv.resultBody)
		}
		for _, mk := range []func() (*Client, error){
			func() (*Client, error) {
				return NewClient("https://runtime.example.invalid", "att_unit01", "", RetryPolicy{}, nil)
			},
			func() (*Client, error) {
				return NewClientV2("https://runtime.example.invalid", "att_unit01", "", RetryPolicy{}, nil)
			},
		} {
			c, err := mk()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.AdmitV3(context.Background(), runtimev1.AdmitRequest{}); err == nil {
				t.Fatal("an older client made a v3 admission")
			}
			if _, err := c.ContextV3(context.Background()); err == nil {
				t.Fatal("an older client read a v3 context")
			}
		}
	})

	t.Run("the defaults keep the v1 wire and add no v3 requirement", func(t *testing.T) {
		cfg := Config{}
		cfg.applyDefaults()
		if cfg.RuntimeAPI != RuntimeAPIv1 || cfg.SharedRun() || cfg.SourceSetRun() {
			t.Fatalf("defaults select %q", cfg.RuntimeAPI)
		}
		// A v1 and a v2 layout need no Git directory: it is validated only for v3.
		u := newUnit(t, "true")
		u.cfg.GitDir = u.cfg.WorkspaceDir
		if err := u.cfg.Validate(); err != nil {
			t.Fatalf("a v1 configuration became subject to the Git directory: %v", err)
		}
		u.cfg.RuntimeAPI = RuntimeAPIv3
		if err := u.cfg.Validate(); err == nil {
			t.Fatal("a v3 Git directory that overlaps the workspace was accepted")
		}
	})
}

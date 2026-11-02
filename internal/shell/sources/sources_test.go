package sources_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/shell/api"
	secret "github.com/optimiware/kodama/internal/shell/secrets"
	"github.com/optimiware/kodama/internal/shell/sources"
	"github.com/optimiware/kodama/internal/shell/store"
)

const (
	tenant  domain.TenantID  = "t_acme"
	project domain.ProjectID = "p_erp"
)

var scope = domain.Scope{TenantID: tenant, ProjectID: project}

func commit(c byte) string { return strings.Repeat(string(c), 40) }

// env is the shell's repository and run records (the in-memory Git store), the
// issuer of run credentials over them, and a branch-tip table a test moves.
type env struct {
	t     *testing.T
	mem   *store.MemoryGit
	dir   string
	snaps sources.FileSnapshots
	tips  *tips
	issue *issuer
}

type tips struct {
	mu    sync.Mutex
	at    map[string]string // repository id -> tip
	calls atomic.Int32
	err   error
}

func (t *tips) Tip(_ context.Context, _ domain.Scope, repo store.Repository, ref string) (string, error) {
	t.calls.Add(1)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return "", t.err
	}
	if ref != gitv1.HeadsPrefix+repo.DefaultBranch {
		return "", fmt.Errorf("unexpected ref %s", ref)
	}
	return t.at[repo.ID], nil
}

func (t *tips) move(id, to string) { t.mu.Lock(); t.at[id] = to; t.mu.Unlock() }

type keys struct{}

func (keys) ScopeKeys(context.Context, domain.Scope) (string, string, error) {
	return "acme", "erp", nil
}

// issuer issues a credential only while the run is live in the shell's record, as
// the Git server does, and counts what it issued.
type issuer struct {
	mem    *store.MemoryGit
	seq    atomic.Int32
	issued []string
	mu     sync.Mutex
}

func (i *issuer) IssueRunCredential(ctx context.Context, tn domain.TenantID, pr domain.ProjectID, runID, repositoryID string) (api.IssuedCredential, error) {
	info, err := i.mem.RunGitInfo(ctx, tn, pr, runID)
	if err != nil {
		return api.IssuedCredential{}, err
	}
	if !info.Active() {
		return api.IssuedCredential{}, store.ErrConflict
	}
	n := i.seq.Add(1)
	cred := api.IssuedCredential{GrantID: fmt.Sprintf("g%d", n), Secret: fmt.Sprintf("gitcred-%s-%s-%d", runID, repositoryID, n), ExpiresAt: time.Now().Add(time.Hour)}
	i.mu.Lock()
	i.issued = append(i.issued, cred.Secret)
	i.mu.Unlock()
	return cred, nil
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, mem: store.NewMemoryGit(), dir: t.TempDir(), tips: &tips{at: map[string]string{}}}
	e.mem.AddTenant(tenant, "acme")
	e.mem.AddProject(tenant, project, "erp")
	e.snaps = sources.FileSnapshots{Dir: filepath.Join(e.dir, "snapshots")}
	e.issue = &issuer{mem: e.mem}
	return e
}

// repo creates a ready repository; project "" makes the tenant's knowledge base.
func (e *env) repo(pr domain.ProjectID, name string, empty bool) store.Repository {
	e.t.Helper()
	r, _, err := e.mem.CreateRepository(context.Background(), store.Repository{TenantID: tenant, ProjectID: pr, ID: "r_" + string(pr) + "_" + name, Name: name, DefaultBranch: "main"})
	if err != nil {
		e.t.Fatal(err)
	}
	if r, err = e.mem.ConfirmInitialized(context.Background(), tenant, r.ID, "main", empty); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) run(id, status string, declared ...gitv1.Source) {
	e.t.Helper()
	raw, err := json.Marshal(declared)
	if err != nil {
		e.t.Fatal(err)
	}
	e.mem.SetRun(store.RunGitInfo{TenantID: tenant, ProjectID: project, RunID: id, PipelineName: "build", Status: status, Sources: raw})
}

func (e *env) preparer() *sources.Preparer {
	e.t.Helper()
	p, err := sources.NewPreparer(sources.PreparerConfig{Runs: e.mem, Repositories: e.mem, Keys: keys{}, Tips: e.tips, Snapshots: e.snaps, BaseURL: "https://kodama.example/"})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func src(repo store.Repository, c, path string) gitv1.Source {
	return gitv1.Source{RepositoryID: repo.ID, Commit: c, Path: path, Access: gitv1.AccessRead}
}

func prep(runID, plan string) runtime.PrepareRequest {
	return runtime.PrepareRequest{Scope: scope, RunID: domain.RunID(runID), PlanDigest: plan}
}

func (e *env) inputs(p *sources.Preparer, runID, plan string) runtime.PreparationInputs {
	e.t.Helper()
	in, err := p.Inputs(context.Background(), prep(runID, plan))
	if err != nil {
		e.t.Fatalf("inputs: %v", err)
	}
	return in
}

// workspaceSpec is what the engine hands ResourceManager.Ensure with the inputs.
func workspaceSpec(in runtime.PreparationInputs) runtime.ResourceSpec {
	return runtime.ResourceSpec{Scope: in.Scope, Owner: runtime.OwnerFor(runtime.LifetimePipeline, in.Scope), RunID: in.RunID,
		Kind: runtime.ResourceWorkspace, Name: "workspace", Lifetime: runtime.LifetimePipeline, Identity: string(in.RunID) + "-ws",
		SpecDigest: in.Digest, Fence: 1, Preparation: &in}
}

func mustFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Test_unit_shell_source_preparation proves the shell prepares a run's pinned
// inputs, with scoped credentials that end with the run, and that those inputs
// meet the kernel's workspace-preparation contract without the kernel learning
// anything about Git.
func Test_unit_shell_source_preparation(t *testing.T) {
	t.Run("pinned sources keep their exact commits and paths", func(t *testing.T) {
		e := newEnv(t)
		back, front := e.repo(project, "backend", false), e.repo(project, "frontend", false)
		e.run("run1", "running", src(front, commit('b'), "web"), src(back, commit('a'), "."))
		p := e.preparer()
		in := e.inputs(p, "run1", "plan-1")

		if len(in.Sources) != 2 {
			t.Fatalf("sources: %+v", in.Sources)
		}
		// Ordered by path, each at its declared commit and its own endpoint.
		if got := in.Sources[0]; got.Revision != commit('a') || got.Path != "." || got.Locator != "https://kodama.example/git/acme/erp/backend.git" {
			t.Errorf("first source: %+v", got)
		}
		if got := in.Sources[1]; got.Revision != commit('b') || got.Path != "web" || got.Locator != "https://kodama.example/git/acme/erp/frontend.git" {
			t.Errorf("second source: %+v", got)
		}
		if e.tips.calls.Load() != 0 {
			t.Error("a pinned commit was resolved from a branch")
		}
		// Credentials are references, owned by the run's project, one per repository.
		if len(in.Credentials) != 2 {
			t.Fatalf("credentials: %+v", in.Credentials)
		}
		for _, c := range in.Credentials {
			if c.Owner != (domain.Owner{TenantID: tenant, ProjectID: project}) || !strings.HasPrefix(c.Name, "KODAMA_GIT_") || c.Validate() != nil {
				t.Errorf("credential reference: %+v", c)
			}
		}
		// No credential in what the kernel sees.
		raw, _ := json.Marshal(in)
		if strings.Contains(string(raw), "gitcred") || strings.Contains(string(raw), "@") {
			t.Errorf("the inputs carry a credential: %s", raw)
		}

		// The kernel's workspace-preparation contract: valid inputs, a valid workspace spec.
		if err := in.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := workspaceSpec(in).Validate(); err != nil {
			t.Fatal(err)
		}
		// Deterministic for one run and plan; any change of the input set changes the digest.
		if again := e.inputs(p, "run1", "plan-1"); again.Digest != in.Digest {
			t.Fatalf("the digest changed between calls: %s %s", in.Digest, again.Digest)
		}
		if other := e.inputs(p, "run1", "plan-2"); other.Digest == in.Digest {
			t.Fatal("another plan has the same digest")
		}
		// The first preparation is recorded: a changed run record does not change what
		// this run and plan were prepared with; another plan is prepared afresh.
		e.run("run1", "running", src(front, commit('c'), "web"), src(back, commit('a'), "."))
		if same := e.inputs(p, "run1", "plan-1"); same.Digest != in.Digest {
			t.Fatal("a recorded preparation was rediscovered")
		}
		if other := e.inputs(p, "run1", "plan-3"); other.Digest == in.Digest || other.Sources[1].Revision != commit('c') {
			t.Fatal("another plan did not read the current run record")
		}
	})

	t.Run("a context repository is pinned in the recorded inputs and survives a restart", func(t *testing.T) {
		e := newEnv(t)
		ten, prj := e.repo("", "context", false), e.repo(project, "context", false)
		e.tips.at[ten.ID], e.tips.at[prj.ID] = commit('1'), commit('2')
		e.run("run1", "running")
		p := e.preparer()
		first := e.inputs(p, "run1", "plan-1")

		if len(first.Sources) != 2 || first.Sources[0].Path != ".context/tenant" || first.Sources[1].Path != ".context/project" {
			t.Fatalf("context sources: %+v", first.Sources)
		}
		if first.Sources[0].Revision != commit('1') || first.Sources[1].Revision != commit('2') {
			t.Fatalf("the context was not pinned to the tips: %+v", first.Sources)
		}
		if first.Sources[0].Locator != "https://kodama.example/git/acme/context.git" || first.Sources[1].Locator != "https://kodama.example/git/acme/erp/context.git" {
			t.Fatalf("locators: %+v", first.Sources)
		}
		if err := workspaceSpec(first).Validate(); err != nil {
			t.Fatal(err)
		}
		if len(mustFiles(t, e.snaps.Dir)) != 1 {
			t.Fatalf("snapshots on disk: %v", mustFiles(t, e.snaps.Dir))
		}

		// The knowledge base moves on; the run keeps what it was prepared with, also in a new process.
		e.tips.move(ten.ID, commit('3'))
		e.tips.move(prj.ID, commit('4'))
		calls := e.tips.calls.Load()
		for name, pp := range map[string]*sources.Preparer{"same": p, "restarted": e.preparer()} {
			again := e.inputs(pp, "run1", "plan-1")
			if again.Digest != first.Digest || again.Sources[0].Revision != commit('1') {
				t.Errorf("%s preparer re-pinned the context: %+v", name, again.Sources)
			}
		}
		if e.tips.calls.Load() != calls {
			t.Error("a recorded pin was resolved from the branch again")
		}
		// Another run starts from the current tip.
		e.run("run2", "running")
		if next := e.inputs(p, "run2", "plan-1"); next.Sources[0].Revision != commit('3') || next.Digest == first.Digest {
			t.Errorf("the next run did not start from the current tip: %+v", next.Sources)
		}
		// Even the Git service being unreachable does not disturb the recorded pin.
		e.tips.err = errors.New("git worker unreachable")
		if again := e.inputs(p, "run1", "plan-1"); again.Digest != first.Digest {
			t.Error("an outage changed the recorded inputs")
		}
		if _, err := p.Inputs(context.Background(), prep("run2", "plan-2")); !errors.Is(err, sources.ErrSourceUnavailable) {
			t.Errorf("an unreachable tip must fail the preparation: %v", err)
		}
	})

	t.Run("an absent or empty knowledge base is skipped, an unusable one fails", func(t *testing.T) {
		e := newEnv(t)
		e.run("run1", "running")
		p := e.preparer()
		if in := e.inputs(p, "run1", "plan-a"); len(in.Sources) != 0 {
			t.Fatalf("an absent knowledge base has sources: %+v", in.Sources)
		}
		e.repo("", "context", true)
		if in := e.inputs(p, "run1", "plan-b"); len(in.Sources) != 0 || len(in.Credentials) != 0 {
			t.Fatalf("an empty knowledge base has sources: %+v", in)
		}
		// One still provisioning is not usable, and a failed preparation records nothing.
		pr, _, err := e.mem.CreateRepository(context.Background(), store.Repository{TenantID: tenant, ProjectID: project, ID: "r_prov", Name: "context", DefaultBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Inputs(context.Background(), prep("run1", "plan-c")); !errors.Is(err, sources.ErrSourceUnavailable) || !strings.Contains(err.Error(), pr.ID) {
			t.Fatalf("a repository still provisioning: %v", err)
		}
		if got := len(mustFiles(t, e.snaps.Dir)); got != 2 {
			t.Fatalf("%d snapshots recorded for two successful preparations", got)
		}
	})

	t.Run("recorded inputs are replayed, whatever the knowledge base becomes", func(t *testing.T) {
		restart := func(e *env, repos sources.Repositories) *sources.Preparer {
			p, err := sources.NewPreparer(sources.PreparerConfig{Runs: e.mem, Repositories: repos, Keys: keys{}, Tips: e.tips, Snapshots: e.snaps, BaseURL: "https://kodama.example"})
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		replays := func(t *testing.T, e *env, repos sources.Repositories, first runtime.PreparationInputs, plan string) {
			t.Helper()
			calls := e.tips.calls.Load()
			for name, pp := range map[string]*sources.Preparer{"restarted": restart(e, repos), "again": restart(e, repos)} {
				again := e.inputs(pp, "run1", plan)
				if again.Digest != first.Digest || fmt.Sprint(again) != fmt.Sprint(first) {
					t.Errorf("%s preparer did not replay the recorded inputs:\n%+v\n%+v", name, first, again)
				}
			}
			if e.tips.calls.Load() != calls {
				t.Error("a recorded input set was rediscovered from the branch")
			}
		}

		t.Run("a context created after the first preparation", func(t *testing.T) {
			e := newEnv(t)
			e.run("run1", "running")
			first := e.inputs(e.preparer(), "run1", "plan-1")
			if len(first.Sources) != 0 {
				t.Fatalf("%+v", first)
			}
			ten := e.repo("", "context", false)
			e.tips.at[ten.ID] = commit('1')
			replays(t, e, e.mem, first, "plan-1")
			// A preparation for another plan is a new snapshot and sees the knowledge base.
			if next := e.inputs(e.preparer(), "run1", "plan-2"); len(next.Sources) != 1 || next.Sources[0].Revision != commit('1') {
				t.Fatalf("another plan: %+v", next)
			}
		})

		t.Run("an empty context that becomes populated", func(t *testing.T) {
			e := newEnv(t)
			ten := e.repo("", "context", true)
			e.tips.at[ten.ID] = commit('1')
			e.run("run1", "running")
			populated := &atomic.Bool{}
			repos := flipRepos{Repositories: e.mem, populated: populated}
			first := e.inputs(restart(e, repos), "run1", "plan-1")
			if len(first.Sources) != 0 || len(first.Credentials) != 0 {
				t.Fatalf("%+v", first)
			}
			populated.Store(true)
			replays(t, e, repos, first, "plan-1")
			if next := e.inputs(restart(e, repos), "run1", "plan-2"); len(next.Sources) != 1 {
				t.Fatalf("another plan: %+v", next)
			}
		})

		t.Run("a context whose default branch changes", func(t *testing.T) {
			e := newEnv(t)
			prj := e.repo(project, "context", false)
			e.tips.at[prj.ID] = commit('1')
			e.run("run1", "running")
			branch := &atomic.Value{}
			repos := flipRepos{Repositories: e.mem, branch: branch}
			first := e.inputs(restart(e, repos), "run1", "plan-1")
			if len(first.Sources) != 1 || first.Sources[0].Revision != commit('1') {
				t.Fatalf("%+v", first)
			}
			branch.Store("trunk")
			e.tips.at[prj.ID] = commit('5')
			replays(t, e, repos, first, "plan-1")
			if next := e.inputs(restart(e, repos), "run1", "plan-2"); next.Sources[0].Revision != commit('5') {
				t.Fatalf("another plan: %+v", next)
			}
		})

		t.Run("a damaged or foreign record is refused", func(t *testing.T) {
			setup := func() (*env, *sources.Preparer, []string) {
				e := newEnv(t)
				e.run("run1", "running")
				e.run("run2", "running")
				p := e.preparer()
				e.inputs(p, "run1", "plan")
				e.inputs(p, "run2", "plan")
				files := mustFiles(t, e.snaps.Dir)
				if len(files) != 2 {
					t.Fatalf("%v", files)
				}
				return e, p, files
			}
			refused := func(p *sources.Preparer) (n int) {
				for _, id := range []string{"run1", "run2"} {
					if _, err := p.Inputs(context.Background(), prep(id, "plan")); errors.Is(err, sources.ErrSourceUnavailable) {
						n++
					}
				}
				return n
			}
			// Another run's record under this run's key is not this run's inputs.
			e, p, files := setup()
			raw, _ := os.ReadFile(filepath.Join(e.snaps.Dir, files[0]))
			if err := os.WriteFile(filepath.Join(e.snaps.Dir, files[1]), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if n := refused(p); n != 1 {
				t.Fatalf("%d runs refused a record that is another run's", n)
			}
			// A damaged record, and one whose digest no longer matches its content.
			e, p, files = setup()
			for _, f := range files {
				if err := os.WriteFile(filepath.Join(e.snaps.Dir, f), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if n := refused(p); n != 2 {
				t.Fatalf("%d runs used a damaged record", n)
			}
			e, p, files = setup()
			for _, f := range files {
				raw, _ := os.ReadFile(filepath.Join(e.snaps.Dir, f))
				tampered := strings.Replace(string(raw), `"digest":"sha256:`, `"digest":"sha256:0`, 1)
				if err := os.WriteFile(filepath.Join(e.snaps.Dir, f), []byte(tampered), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if n := refused(p); n != 2 {
				t.Fatalf("%d runs used a record whose digest does not match", n)
			}
		})
	})

	t.Run("a source-free run prepares an empty, valid and stable input set", func(t *testing.T) {
		e := newEnv(t)
		e.run("run1", "running")
		p := e.preparer()
		in := e.inputs(p, "run1", "plan")
		if len(in.Sources) != 0 || len(in.Credentials) != 0 || in.Digest == "" || in.Scope != scope || in.RunID != "run1" {
			t.Fatalf("source-free inputs: %+v", in)
		}
		if err := workspaceSpec(in).Validate(); err != nil {
			t.Fatal(err)
		}
		if e.inputs(p, "run1", "plan").Digest != in.Digest || e.inputs(p, "run1", "plan-2").Digest == in.Digest {
			t.Fatal("the digest is not a function of run and plan")
		}
		if e.tips.calls.Load() != 0 || e.issue.seq.Load() != 0 {
			t.Fatal("a source-free run touched Git")
		}
	})

	t.Run("sources that cannot be pinned fail the preparation with a stable error", func(t *testing.T) {
		e := newEnv(t)
		good := e.repo(project, "backend", false)
		e.mem.AddProject(tenant, "p_other", "other")
		other := e.repo("p_other", "elsewhere", false)
		for name, declared := range map[string][]gitv1.Source{
			"a repository that does not exist": {{RepositoryID: "r_missing", Commit: commit('a'), Path: "."}},
			"another project's repository":     {src(other, commit('a'), ".")},
			"a floating ref":                   {src(good, "main", ".")},
			"a short commit":                   {src(good, "abc123", ".")},
			"two sources at one path":          {src(good, commit('a'), "x"), src(good, commit('b'), "x")},
			"a path outside the workspace":     {src(good, commit('a'), "../escape")},
		} {
			e.run("run1", "running", declared...)
			if _, err := e.preparer().Inputs(context.Background(), prep("run1", "plan")); !errors.Is(err, sources.ErrSourceUnavailable) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := e.preparer().Inputs(context.Background(), prep("run-unknown", "plan")); !errors.Is(err, sources.ErrSourceUnavailable) {
			t.Errorf("an unknown run: %v", err)
		}
		if _, err := e.preparer().Inputs(context.Background(), runtime.PrepareRequest{Scope: scope, RunID: "run1"}); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("no plan digest: %v", err)
		}
		if _, err := e.preparer().Inputs(context.Background(), runtime.PrepareRequest{RunID: "run1", PlanDigest: "p"}); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("no scope: %v", err)
		}
	})

	t.Run("another tenant's run is not visible", func(t *testing.T) {
		e := newEnv(t)
		e.run("run1", "running")
		req := prep("run1", "plan")
		req.Scope = domain.Scope{TenantID: "t_other", ProjectID: project}
		if _, err := e.preparer().Inputs(context.Background(), req); !errors.Is(err, sources.ErrSourceUnavailable) {
			t.Fatalf("a foreign tenant prepared the run: %v", err)
		}
	})

	t.Run("run credentials are private material that is revoked with the run", func(t *testing.T) {
		e := newEnv(t)
		back := e.repo(project, "backend", false)
		ctxRepo := e.repo(project, "context", false)
		e.tips.at[ctxRepo.ID] = commit('9')
		e.run("run1", "running", src(back, commit('a'), "."))
		in := e.inputs(e.preparer(), "run1", "plan")
		if len(in.Credentials) != 2 {
			t.Fatalf("credentials: %+v", in.Credentials)
		}

		var events []secret.Event
		custody, err := secret.NewCustody(secret.CustodyConfig{Root: filepath.Join(e.dir, "custody"), Source: sources.CredentialSource{Issuer: e.issue},
			Observe: func(ev secret.Event) { events = append(events, ev) }})
		if err != nil {
			t.Fatal(err)
		}
		ref, err := runtime.NewMaterializationRef(scope, "run1", "prepare", 1)
		if err != nil {
			t.Fatal(err)
		}
		m, err := custody.Resolve(context.Background(), runtime.ResolveRequest{Ref: ref, Refs: in.Credentials})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(m.Directory, "/workspace") {
			t.Fatalf("credentials in the workspace: %s", m.Directory)
		}
		for _, c := range in.Credentials {
			b, err := os.ReadFile(filepath.Join(m.Directory, c.Name))
			if err != nil || !strings.HasPrefix(string(b), "gitcred-run1-") {
				t.Fatalf("credential %s: %q %v", c.Name, b, err)
			}
		}
		// Rotation while the run lives replaces the material and is observable.
		rotated, err := custody.Resolve(context.Background(), runtime.ResolveRequest{Ref: ref, Refs: in.Credentials})
		if err != nil || rotated.Revision == m.Revision {
			t.Fatalf("rotation: %v %q %q", err, m.Revision, rotated.Revision)
		}
		if events[len(events)-1].Kind != secret.EventRotated {
			t.Fatalf("events: %+v", events)
		}
		// The run ends: nothing can be issued for it again, and revocation removes the files.
		e.run("run1", "succeeded", src(back, commit('a'), "."))
		_, err = custody.Resolve(context.Background(), runtime.ResolveRequest{Ref: ref, Refs: in.Credentials})
		if err == nil || !errors.Is(err, secret.ErrUnavailable) {
			t.Fatalf("a credential was issued for a finished run: %v", err)
		}
		if err := custody.Revoke(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(m.Directory); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the credentials outlived the run: %v", err)
		}
		e.run("run1", "running", src(back, commit('a'), "."))
		if _, err := custody.Resolve(context.Background(), runtime.ResolveRequest{Ref: ref, Refs: in.Credentials}); !errors.Is(err, runtime.ErrMaterializationRevoked) {
			t.Fatalf("a revoked identity resolved: %v", err)
		}
		// No credential value in anything retained.
		e.issue.mu.Lock()
		issued := append([]string(nil), e.issue.issued...)
		e.issue.mu.Unlock()
		err = filepath.WalkDir(e.dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			b, rerr := os.ReadFile(p)
			for _, s := range issued {
				if rerr == nil && strings.Contains(string(b), s) {
					t.Errorf("%s retains a Git credential", p)
				}
			}
			return rerr
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range issued {
			if strings.Contains(fmt.Sprintf("%+v", events), s) {
				t.Error("an event carries a Git credential")
			}
		}
	})

	t.Run("the credential source refuses what is not the run's own Git credential", func(t *testing.T) {
		e := newEnv(t)
		e.run("run1", "running")
		back := e.repo(project, "backend", false)
		name, err := sources.CredentialName(back.ID)
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := runtime.NewMaterializationRef(scope, "run1", "build", 1)
		cs := sources.CredentialSource{Issuer: e.issue}
		ctx := context.Background()
		if _, err := cs.Fetch(ctx, ref, runtime.SecretRef{Owner: domain.Owner{TenantID: tenant, ProjectID: project}, Name: name}); err != nil {
			t.Fatal(err)
		}
		for what, r := range map[string]runtime.SecretRef{
			"a tenant-owned reference":  {Owner: domain.Owner{TenantID: tenant}, Name: name},
			"another project":           {Owner: domain.Owner{TenantID: tenant, ProjectID: "p_other"}, Name: name},
			"another reference's name":  {Owner: domain.Owner{TenantID: tenant, ProjectID: project}, Name: "DEPLOY_TOKEN"},
			"a malformed credential id": {Owner: domain.Owner{TenantID: tenant, ProjectID: project}, Name: "KODAMA_GIT_zz"},
		} {
			if _, err := cs.Fetch(ctx, ref, r); !errors.Is(err, secret.ErrUnavailable) {
				t.Errorf("%s: %v", what, err)
			}
		}
		// Other references go to the next source.
		next := cs
		next.Next = fixed("v")
		if s, err := next.Fetch(ctx, ref, runtime.SecretRef{Owner: domain.Owner{TenantID: tenant}, Name: "DEPLOY_TOKEN"}); err != nil || string(s.Value) != "v" {
			t.Errorf("fallback: %v", err)
		}
		if _, err := sources.CredentialName("repo id/with slash"); !errors.Is(err, sources.ErrSourceUnavailable) {
			t.Error("an unusable repository id was named")
		}
	})

	t.Run("snapshots are first-writer-wins and refuse bad keys and bad inputs", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "snapshots")
		key := strings.Repeat("ab", 32)
		inputs := func(i int) runtime.PreparationInputs {
			return runtime.PreparationInputs{Scope: scope, RunID: "run1", Digest: fmt.Sprintf("d%d", i),
				Sources: []runtime.SourceInput{{Name: "s", Locator: "https://x/git/a/b.git", Revision: commit("0123456789abcdef"[i]), Path: "."}}}
		}
		var wg sync.WaitGroup
		got := make([]runtime.PreparationInputs, 8)
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := (sources.FileSnapshots{Dir: dir}).PutIfAbsent(context.Background(), key, inputs(i))
				if err != nil {
					t.Error(err)
				}
				got[i] = c
			}()
		}
		wg.Wait()
		for _, c := range got {
			if c.Digest != got[0].Digest {
				t.Fatalf("racing writers disagree: %v", got)
			}
		}
		if c, ok, err := (sources.FileSnapshots{Dir: dir}).Get(context.Background(), key); err != nil || !ok || c.Digest != got[0].Digest || c.Sources[0].Revision != got[0].Sources[0].Revision {
			t.Fatalf("read back: %+v %v %v", c, ok, err)
		}
		if _, ok, _ := (sources.FileSnapshots{Dir: dir}).Get(context.Background(), strings.Repeat("cd", 32)); ok {
			t.Fatal("an unrecorded snapshot was found")
		}
		for _, bad := range []string{"../escape", "short", ""} {
			if _, err := (sources.FileSnapshots{Dir: dir}).PutIfAbsent(context.Background(), bad, inputs(0)); err == nil {
				t.Errorf("key %q accepted", bad)
			}
		}
		unpinned := inputs(0)
		unpinned.Sources[0].Revision = ""
		if _, err := (sources.FileSnapshots{Dir: dir}).PutIfAbsent(context.Background(), strings.Repeat("ef", 32), unpinned); err == nil {
			t.Error("an unpinned input set was recorded")
		}
		leftovers, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
		if len(leftovers) != 0 {
			t.Errorf("temporary files left: %v", leftovers)
		}
	})

	t.Run("the preparer refuses a base URL that carries a credential", func(t *testing.T) {
		e := newEnv(t)
		for _, base := range []string{"", "ssh://git", "https://user:pw@kodama.example"} {
			if _, err := sources.NewPreparer(sources.PreparerConfig{Runs: e.mem, Repositories: e.mem, Keys: keys{}, Tips: e.tips, Snapshots: e.snaps, BaseURL: base}); err == nil {
				t.Errorf("base URL %q accepted", base)
			}
		}
		if _, err := sources.NewPreparer(sources.PreparerConfig{BaseURL: "https://x"}); err == nil {
			t.Error("a preparer without its ports was accepted")
		}
	})

	t.Run("run liveness and the checkout wiring", func(t *testing.T) {
		e := newEnv(t)
		live := sources.Liveness(e.mem)
		ctx := context.Background()
		if active, err := live.RunActive(ctx, tenant, project, "nope"); err != nil || active {
			t.Fatalf("an unknown run is live: %v %v", active, err)
		}
		e.run("run1", "running")
		if active, err := live.RunActive(ctx, tenant, project, "run1"); err != nil || !active {
			t.Fatalf("a running run: %v %v", active, err)
		}
		e.run("run1", "failed")
		if active, _ := live.RunActive(ctx, tenant, project, "run1"); active {
			t.Fatal("a failed run is live")
		}
		boom := errors.New("database down")
		if _, err := sources.Liveness(brokenRuns{boom}).RunActive(ctx, tenant, project, "run1"); !errors.Is(err, boom) {
			t.Fatalf("an outage is not unknown: %v", err)
		}
		cred, err := sources.Credentials(e.issue).IssueRunCredential(ctx, tenant, project, "run1", "r")
		if !errors.Is(err, store.ErrConflict) || cred != (runner.IssuedCredential{}) {
			t.Fatalf("a credential for a finished run: %v %+v", err, cred)
		}
		e.run("run1", "running")
		if cred, err := sources.Credentials(e.issue).IssueRunCredential(ctx, tenant, project, "run1", "r"); err != nil || cred.Secret == "" || cred.GrantID == "" {
			t.Fatalf("a credential for a live run: %v", err)
		}

		// Without a checkout backend, or a state directory, no sources are wired: runs that pin sources fail.
		if g := sources.NewGit(sources.GitConfig{Backend: noCheckout{}, StateDir: t.TempDir()}); g != nil {
			t.Error("a backend that cannot check out was wired")
		}
		if g := sources.NewGit(sources.GitConfig{Backend: runner.Process{}}); g != nil {
			t.Error("sources were wired without a state directory")
		}
		state := t.TempDir()
		g := sources.NewGit(sources.GitConfig{Backend: runner.Process{}, Repositories: e.mem, Issuer: e.issue, Runs: e.mem, PublicURL: "https://kodama.example", StateDir: state})
		if g == nil || g.Dir != filepath.Join(state, "git-credentials") || g.BaseURL != "https://kodama.example" || g.Liveness == nil || g.Credentials == nil {
			t.Fatalf("the checkout was not wired: %+v", g)
		}
	})
}

// flipRepos reports the knowledge base as populated, or on another default
// branch, once switched (the memory store only reconfigures an empty repository).
type flipRepos struct {
	sources.Repositories
	populated *atomic.Bool
	branch    *atomic.Value // string; empty keeps the record's branch
}

func (f flipRepos) RepositoryByPath(ctx context.Context, tn domain.TenantID, projectKey, name string) (store.Repository, error) {
	r, err := f.Repositories.RepositoryByPath(ctx, tn, projectKey, name)
	if err == nil && f.populated != nil && f.populated.Load() {
		r.Empty = false
	}
	if f.branch != nil {
		if b, _ := f.branch.Load().(string); err == nil && b != "" {
			r.DefaultBranch = b
		}
	}
	return r, err
}

type fixed string

func (f fixed) Fetch(context.Context, runtime.MaterializationRef, runtime.SecretRef) (secret.Secret, error) {
	return secret.Secret{Value: []byte(f), Version: "1"}, nil
}

type brokenRuns struct{ err error }

func (b brokenRuns) RunGitInfo(context.Context, domain.TenantID, domain.ProjectID, string) (store.RunGitInfo, error) {
	return store.RunGitInfo{}, b.err
}

// noCheckout is a backend with no Checkout.
type noCheckout struct{}

func (noCheckout) Name() string { return "nocheckout" }
func (noCheckout) Prepare(context.Context, domain.Scope, string) (string, func(context.Context) error, error) {
	return "", nil, nil
}
func (noCheckout) Execute(context.Context, runner.Spec, io.Writer) (int, error) { return 0, nil }

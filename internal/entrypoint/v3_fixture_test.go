package entrypoint

// Fixtures of the v3 (source set) workload programs (T-151): a real `git`, the
// smart-HTTPS server of test/support/git with distinct read and write credentials
// and a post-receive hook that prints the push record line, a prepared multi-source
// workspace, a scripted v3 port and a builder of valid ContextV3 documents.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	gitsupport "github.com/optimiware/kodama/test/support/git"
)

const (
	v3WorkItem = "wi_1042"
	v3Task     = "refs/heads/kodama/work/wi_1042"
	v3Agent    = "refs/heads/kodama/agents/wi_1042/run_01/"
)

// installPushIDHook makes the repository answer every receive that updated a ref
// with the line the Git worker's post-receive hook must print (README.md): one
// monotonically numbered push record per receive.
func installPushIDHook(t testing.TB, repo *gitsupport.Repo) {
	t.Helper()
	hook := "#!/bin/sh\nd=\"${GIT_DIR:-.}\"\nn=$(cat \"$d/kodama-pushes\" 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > \"$d/kodama-pushes\"\necho \"kodama-push-id: push_$n\" >&2\n"
	if err := os.MkdirAll(filepath.Join(repo.Path, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Path, "hooks", "post-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
}

// checkoutInto makes dir the repository a preparation leaves: the pinned commit,
// detached, with no configuration of the origin.
func checkoutInto(t testing.TB, repo *gitsupport.Repo, commit, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tgit(t, dir, "init", "--quiet", "--template=")
	tgit(t, dir, "fetch", "--quiet", "--no-tags", repo.Path, commit)
	tgit(t, dir, "checkout", "--quiet", "--detach", commit)
}

// v3Source is one repository of the fixture: the bare repository on the server,
// its pinned commit and where it lives in the workspace.
type v3Source struct {
	repo   *gitsupport.Repo
	id     string
	path   string
	pinned string
	tree   string
}

func (s *v3Source) dir(f *v3Fixture) string { return filepath.Join(f.cfg.WorkspaceDir, s.path) }

// scriptV3 is the v3 admission, context, begin and result with every operation
// recorded and replaceable.
type scriptV3 struct {
	mu      sync.Mutex
	ctx     runtimev3.ContextV3
	calls   []string
	begun   []runtimev3.BeginRequestV3
	results []runtimev3.ResultRequestV3
	onBegin func()
	begin   func(runtimev3.BeginRequestV3) (runtimev3.BeginResponseV3, error)
	admit   func() (runtimev3.AdmitResponseV3, error)
	fetch   func() (runtimev3.ContextV3, error)
	submit  func(n int, req runtimev3.ResultRequestV3) (runtimev1.ResultResponse, error)

	// The runtime-issued grants (the Git grant routes), with the calls they received.
	seed        func(n int, req runtimev3.GitSeedRequest) (runtimev3.GitGrantResponse, error)
	integration func(n int, req runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error)
	// integrationCtx replaces integration and sees the request's context (a stalled call).
	integrationCtx func(ctx context.Context, n int, req runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error)
	seeds          []runtimev3.GitSeedRequest
	integrals      []runtimev3.GitIntegrationRequest
}

func (s *scriptV3) SeedGrant(_ context.Context, req runtimev3.GitSeedRequest) (runtimev3.GitGrantResponse, error) {
	s.mu.Lock()
	s.rec("seedGitGrant")
	s.seeds = append(s.seeds, req)
	n, fn := len(s.seeds), s.seed
	s.mu.Unlock()
	if fn == nil {
		return runtimev3.GitGrantResponse{}, &APIError{Op: "seedGitGrant", Status: 403, Code: domain.CodeForbidden}
	}
	return fn(n, req)
}

func (s *scriptV3) IntegrationGrant(ctx context.Context, req runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
	s.mu.Lock()
	s.rec("integrationGitGrant")
	s.integrals = append(s.integrals, req)
	n, fn, fnCtx := len(s.integrals), s.integration, s.integrationCtx
	s.mu.Unlock()
	if fnCtx != nil {
		return fnCtx(ctx, n, req)
	}
	if fn == nil {
		return runtimev3.GitGrantResponse{}, &APIError{Op: "integrationGitGrant", Status: 403, Code: domain.CodeForbidden}
	}
	return fn(n, req)
}

func (s *scriptV3) rec(op string) { s.calls = append(s.calls, op) }

func (s *scriptV3) AdmitV3(context.Context, runtimev1.AdmitRequest) (runtimev3.AdmitResponseV3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("admit")
	if s.admit != nil {
		return s.admit()
	}
	return runtimev3.AdmitResponseV3{Token: tokenInfo("rt_unit_token_0123456789abcdef", time.Hour, 2*time.Hour), Context: s.ctx}, nil
}

func (s *scriptV3) ContextV3(context.Context) (runtimev3.ContextV3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("getContext")
	if s.fetch != nil {
		return s.fetch()
	}
	return s.ctx, nil
}

func (s *scriptV3) BeginV3(_ context.Context, req runtimev3.BeginRequestV3) (runtimev3.BeginResponseV3, error) {
	s.mu.Lock()
	s.rec("begin")
	s.begun = append(s.begun, req)
	hook, fn := s.onBegin, s.begin
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fn != nil {
		return fn(req)
	}
	state, ignored, err := runtimev3.DeriveBegin(s.ctx, req)
	if err != nil {
		return runtimev3.BeginResponseV3{}, err
	}
	return runtimev3.BeginResponseV3{AttemptID: s.ctx.AttemptID, BeganAt: runtimev3.Timestamp(time.Now().UTC().Format(time.RFC3339Nano)), WorkspaceState: state, IgnoredPresent: ignored}, nil
}

func (s *scriptV3) SubmitResultV3(_ context.Context, req runtimev3.ResultRequestV3) (runtimev1.ResultResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("submitResult")
	s.results = append(s.results, req)
	if s.submit != nil {
		return s.submit(len(s.results), req)
	}
	status := runtimev1.AttemptFailed
	if req.Result.Outcome == runtimev1.OutcomeCompleted {
		status = runtimev1.AttemptSucceeded
	}
	return runtimev1.ResultResponse{AttemptID: s.ctx.AttemptID, ResultHash: req.ResultHash, AttemptStatus: status, AcceptedAt: timestamp(0)}, nil
}

func (s *scriptV3) count(op string) (n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == op {
			n++
		}
	}
	return
}

// v3Fixture is one v3 attempt over a prepared two-source workspace: backend (the
// write source, task branch) and frontend (a read source).
type v3Fixture struct {
	*unit
	v3       *scriptV3
	gs       *gitsupport.Server
	backend  *v3Source
	frontend *v3Source
	script   string
	writer   bool
}

func newV3Fixture(t *testing.T, writer bool, script string) *v3Fixture {
	t.Helper()
	requireGit(t)
	u := newUnit(t, "true")
	gs := gitsupport.NewServer(t)
	f := &v3Fixture{unit: u, v3: &scriptV3{}, gs: gs, script: script, writer: writer}
	// The v1 port is only for renewal, secrets and artifacts: a v3 attempt never
	// proposes source and never submits a v1 result.
	u.rt.propose = func(int, runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
		t.Error("a v3 attempt proposed source")
		return runtimev1.SourceProposalResponse{}, &APIError{Op: "proposeSource", Status: 500}
	}
	u.rt.submit = func(int, runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
		t.Error("a v3 attempt submitted a v1 result")
		return runtimev1.ResultResponse{}, &APIError{Op: "submitResult", Status: 500}
	}
	mk := func(name, id, path string) *v3Source {
		repo := gs.NewRepo(t, name)
		installPushIDHook(t, repo)
		s := &v3Source{repo: repo, id: id, path: path, pinned: repo.Initial}
		repo.SetRef(t, v3Task, repo.Initial) // the task branch exists at the expected tip
		return s
	}
	f.backend, f.frontend = mk("backend", "repo_01", "backend"), mk("frontend", "repo_02", "frontend")
	for _, s := range []*v3Source{f.backend, f.frontend} {
		checkoutInto(t, s.repo, s.pinned, s.dir(f))
		s.tree = tgit(t, s.dir(f), "rev-parse", "HEAD^{tree}")
	}
	u.cfg.GitDir = filepath.Join(u.root, "git")
	writeFile(t, filepath.Join(u.cfg.GitDir, "credential"), gs.WriteToken+"\n")
	writeFile(t, filepath.Join(u.cfg.GitDir, "ca.pem"), mustRead(t, gs.CAFile))
	u.cfg.RuntimeAPI = RuntimeAPIv3
	f.v3.ctx = f.context()
	return f
}

func mustRead(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// context builds a valid ContextV3 of the fixture's attempt.
func (f *v3Fixture) context() runtimev3.ContextV3 {
	f.t.Helper()
	access := runtimev3.AccessRead
	var tip *runtimev3.GitCommit
	if f.writer {
		access = runtimev3.AccessWrite
		c := runtimev3.GitCommit(f.backend.pinned)
		tip = &c
	}
	branch := v3Task
	wi := domain.WorkItemID(v3WorkItem)
	src := func(s *v3Source, a gitv1.SourceAccess, b *string, expected *runtimev3.GitCommit) runtimev3.SourceV3 {
		return runtimev3.SourceV3{
			Source:        gitv1.Source{RepositoryID: s.id, Commit: s.pinned, Path: s.path, Access: a, Branch: b},
			RepositoryURL: f.gs.URL + "/" + s.repo.Name + ".git", HeadCommit: runtimev3.GitCommit(s.pinned), ExpectedTip: expected,
		}
	}
	c := runtimev3.ContextV3{
		ContextVersion: 3, RunModel: "run_shared",
		TenantID: "t_acme", ProjectID: "p_inventory", RunID: "run_01", StepID: "build", AttemptID: "att_unit01", ActionKey: "run_01.build",
		Definition: runtimev1.Definition{ID: "pd_01", Digest: runtimev1.Digest("sha256:" + strings.Repeat("d", 64))},
		Origin:     runtimev3.Origin{Kind: runtimev3.OriginWorkItem, WorkItemID: &wi},
		WorkItem:   &runtimev1.WorkItem{ID: wi, ContentRevision: 2, Spec: "# Plan", AcceptanceCriteria: []runtimev1.AcceptanceCriterion{{ID: "AC-1", Text: "works"}}},
		Command:    runtimev1.Command{Image: testImage, Command: []string{"/bin/sh", "-c"}, Args: []string{f.script}, Env: map[string]string{}},
		Workspace: runtimev3.WorkspaceV3{
			Access: access, MountPath: "/workspace",
			Sources: []runtimev3.SourceV3{src(f.backend, gitv1.AccessWrite, &branch, tip), src(f.frontend, gitv1.AccessRead, nil, nil)},
			Lineage: []runtimev3.LineageEntryV3{},
		},
		Endpoints: map[string]runtimev3.Endpoint{}, Egress: []string{},
		Run:    runtimev3.RunWindow{StartedAt: timestamp(-time.Minute), DeadlineAt: timestamp(time.Hour)},
		Policy: runtimev3.PolicyPin{ID: "run-v1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("f", 64))},
		Inputs: map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 30, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 64 << 10},
	}
	set, err := c.Workspace.SourceSet()
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := set.Digest()
	if err != nil {
		f.t.Fatal(err)
	}
	d := runtimev3.Digest(digest)
	c.Workspace.SourceSetDigest = &d
	c.Workspace.Preparation = &runtimev3.PreparationV3{
		Sources: []runtimev3.PreparedSource{
			{Path: f.backend.path, Commit: runtimev3.GitCommit(f.backend.pinned), TreeID: runtimev3.GitCommit(f.backend.tree)},
			{Path: f.frontend.path, Commit: runtimev3.GitCommit(f.frontend.pinned), TreeID: runtimev3.GitCommit(f.frontend.tree)},
		},
		ReceiptDigest: runtimev1.Digest("sha256:" + strings.Repeat("e", 64)),
	}
	grant, ca, integ := "gg_01", "/kodama/git/ca.pem", "/kodama/git/credential"
	cred := integ
	c.Git = &runtimev3.GitDelivery{
		GrantID: &grant, BaseURL: f.gs.URL, CredentialFile: &cred, CAFile: &ca,
		Runtime: runtimev3.GitRuntimeGrants{Integration: &runtimev3.GitRuntimeGrant{CredentialFile: "/kodama/git/integration-credential"}},
	}
	access2, err := c.GitAccess()
	if err != nil {
		f.t.Fatal(err)
	}
	c.Git.Repositories = access2
	if err := c.Validate(); err != nil {
		f.t.Fatalf("the fixture context is invalid: %v", err)
	}
	return c
}

func (f *v3Fixture) run() int {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f.step = NewStepV3(f.cfg, f.rt, f.v3, f.up)
	return f.step.Run(ctx)
}

func (f *v3Fixture) wantOK() runtimev3.ResultV3 {
	f.t.Helper()
	if code := f.run(); code != ExitOK {
		f.t.Fatalf("exit = %d, want %d\ncalls: %v\nlogs:\n%s", code, ExitOK, f.v3.calls, f.logs.String())
	}
	return f.result()
}

func (f *v3Fixture) result() runtimev3.ResultV3 {
	f.t.Helper()
	f.v3.mu.Lock()
	defer f.v3.mu.Unlock()
	if len(f.v3.results) == 0 {
		f.t.Fatalf("no result submitted; calls %v\nlogs:\n%s", f.v3.calls, f.logs.String())
	}
	return f.v3.results[len(f.v3.results)-1].Result
}

func (f *v3Fixture) wantFailed(r runtimev3.ResultV3, reason string) {
	f.t.Helper()
	if r.Outcome != runtimev3.OutcomeFailed || r.Reason == nil || *r.Reason != reason {
		got := "<nil>"
		if r.Reason != nil {
			got = *r.Reason
		}
		f.t.Fatalf("result = %s/%s, want failed/%s\nlogs:\n%s", r.Outcome, got, reason, f.logs.String())
	}
	if len(r.Commits) != 0 {
		f.t.Fatalf("a failed result reports commits %+v", r.Commits)
	}
}

// tip is the commit a ref of a bare repository points at, "" when absent.
func tipOf(t testing.TB, s *v3Source, ref string) string { return s.repo.Tip(t, ref) }

func describe(cs []runtimev3.CommitOutput) string {
	var parts []string
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s %s %s %s", c.RepositoryID, c.Ref, c.Commit, c.PushID))
	}
	return strings.Join(parts, "; ")
}

package git_test

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

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	git "github.com/optimiware/kodema/internal/userland/git"
)

func TestParseConfigOptionApproveRebase(t *testing.T) {
	t.Run("valid exact, reversed order, prefix, and narrower ref", func(t *testing.T) {
		parsed, err := git.ParseConfig([]byte(`
repo admin
    RW = @admin

repo shop/web
    option resource = git-shop-web
    RW refs/heads/main = @execution
    option approve.refs/heads/main = reviewer
    option approve-rebase.refs/heads/main = disjoint

repo shop/api
    option resource = git-shop-api
    RW refs/heads/main = @execution
    option approve-rebase.refs/heads/main = disjoint
    option approve.refs/heads/main = reviewer

repo shop/deploy
    option resource = git-shop-deploy
    RW refs/heads/release/ = @execution
    option approve.refs/heads/release/ = operator
    option approve-rebase.refs/heads/release/ = disjoint

repo shop/narrow
    option resource = git-shop-narrow
    RW refs/heads/release/ = @execution
    option approve.refs/heads/release/ = operator
    option approve-rebase.refs/heads/release/v1 = disjoint
`))
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}

		web, ok := parsed.Policy("shop/web")
		if !ok {
			t.Fatal("missing shop/web policy")
		}
		if mode, ok := web.ApproveRebaseFor("refs/heads/main"); !ok || mode != git.ApproveRebaseDisjoint {
			t.Fatalf("shop/web ApproveRebaseFor(main) = %q, %v; want disjoint, true", mode, ok)
		}
		if _, ok := web.ApproveRebaseFor("refs/heads/other"); ok {
			t.Fatal("shop/web ApproveRebaseFor(other) = true; want false")
		}
		// Mutating returned policy must not corrupt Config.
		web.ApproveRebases[0].Mode = "mutated"
		webAgain, _ := parsed.Policy("shop/web")
		if mode, _ := webAgain.ApproveRebaseFor("refs/heads/main"); mode != git.ApproveRebaseDisjoint {
			t.Fatalf("clonePolicy did not isolate ApproveRebases: got %q", mode)
		}

		apiPolicy, _ := parsed.Policy("shop/api")
		if mode, ok := apiPolicy.ApproveRebaseFor("refs/heads/main"); !ok || mode != git.ApproveRebaseDisjoint {
			t.Fatalf("shop/api ApproveRebaseFor(main) = %q, %v; want disjoint, true", mode, ok)
		}

		deploy, _ := parsed.Policy("shop/deploy")
		if mode, ok := deploy.ApproveRebaseFor("refs/heads/release/2026"); !ok || mode != git.ApproveRebaseDisjoint {
			t.Fatalf("shop/deploy ApproveRebaseFor(release/2026) = %q, %v; want disjoint, true", mode, ok)
		}

		narrow, _ := parsed.Policy("shop/narrow")
		if mode, ok := narrow.ApproveRebaseFor("refs/heads/release/v1"); !ok || mode != git.ApproveRebaseDisjoint {
			t.Fatalf("shop/narrow ApproveRebaseFor(release/v1) = %q, %v; want disjoint, true", mode, ok)
		}
		if _, ok := narrow.ApproveRebaseFor("refs/heads/release/v2"); ok {
			t.Fatal("shop/narrow ApproveRebaseFor(release/v2) = true; want false")
		}
	})

	t.Run("rejects invalid configurations", func(t *testing.T) {
		cases := []struct {
			name string
			conf string
			want string
		}{
			{
				name: "unknown mode",
				conf: "repo shop/web\n    option approve.refs/heads/main = reviewer\n    option approve-rebase.refs/heads/main = always\n",
				want: "unknown approve-rebase mode",
			},
			{
				name: "invalid ref",
				conf: "repo shop/web\n    option approve.refs/heads/main = reviewer\n    option approve-rebase.main = disjoint\n",
				want: "invalid approve-rebase ref",
			},
			{
				name: "glob ref",
				conf: "repo shop/web\n    option approve.refs/heads/main = reviewer\n    option approve-rebase.refs/heads/* = disjoint\n",
				want: "invalid approve-rebase ref",
			},
			{
				name: "duplicate approve-rebase on same ref",
				conf: "repo shop/web\n    option approve.refs/heads/main = reviewer\n    option approve-rebase.refs/heads/main = disjoint\n    option approve-rebase.refs/heads/main = disjoint\n",
				want: "twice",
			},
			{
				name: "dangling without any approve option",
				conf: "repo shop/web\n    RW refs/heads/main = @execution\n    option approve-rebase.refs/heads/main = disjoint\n",
				want: "without a matching option approve rule",
			},
			{
				name: "dangling with different approve ref",
				conf: "repo shop/web\n    option approve.refs/heads/other = reviewer\n    option approve-rebase.refs/heads/main = disjoint\n",
				want: "without a matching option approve rule",
			},
			{
				name: "broader approve-rebase than approve",
				conf: "repo shop/web\n    option approve.refs/heads/main = reviewer\n    option approve-rebase.refs/heads/ = disjoint\n",
				want: "without a matching option approve rule",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := git.ParseConfig([]byte(tc.conf))
				if err == nil {
					t.Fatalf("ParseConfig accepted invalid config:\n%s", tc.conf)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("ParseConfig error = %q; want substring %q", err.Error(), tc.want)
				}
			})
		}
	})
}

func TestHookParsePushOptionsValidation(t *testing.T) {
	validBase := strings.Repeat("a", 40)
	validCandidate := strings.Repeat("b", 40)

	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name: "invalid count non-numeric",
			env: map[string]string{
				git.EnvPushOptionCount: "abc",
			},
			wantErr: "invalid push option count",
		},
		{
			name: "negative count",
			env: map[string]string{
				git.EnvPushOptionCount: "-1",
			},
			wantErr: "invalid push option count",
		},
		{
			name: "excessive count",
			env: map[string]string{
				git.EnvPushOptionCount: "65",
			},
			wantErr: "invalid push option count",
		},
		{
			name: "missing indexed option",
			env: map[string]string{
				git.EnvPushOptionCount: "1",
			},
			wantErr: "missing push option entry",
		},
		{
			name: "malformed kodema option without equals",
			env: map[string]string{
				git.EnvPushOptionCount:        "1",
				git.EnvPushOptionPrefix + "0": "kodema.approved_base",
			},
			wantErr: "malformed kodema push option",
		},
		{
			name: "unknown kodema option",
			env: map[string]string{
				git.EnvPushOptionCount:        "1",
				git.EnvPushOptionPrefix + "0": "kodema.unknown=value",
			},
			wantErr: "unknown kodema push option",
		},
		{
			name: "only approved_base provided",
			env: map[string]string{
				git.EnvPushOptionCount:        "1",
				git.EnvPushOptionPrefix + "0": "kodema.approved_base=" + validBase,
			},
			wantErr: "provided together",
		},
		{
			name: "only approved_candidate provided",
			env: map[string]string{
				git.EnvPushOptionCount:        "1",
				git.EnvPushOptionPrefix + "0": "kodema.approved_candidate=" + validCandidate,
			},
			wantErr: "provided together",
		},
		{
			name: "duplicate approved_base",
			env: map[string]string{
				git.EnvPushOptionCount:        "3",
				git.EnvPushOptionPrefix + "0": "kodema.approved_base=" + validBase,
				git.EnvPushOptionPrefix + "1": "kodema.approved_base=" + validBase,
				git.EnvPushOptionPrefix + "2": "kodema.approved_candidate=" + validCandidate,
			},
			wantErr: "duplicate push option kodema.approved_base",
		},
		{
			name: "duplicate approved_candidate",
			env: map[string]string{
				git.EnvPushOptionCount:        "3",
				git.EnvPushOptionPrefix + "0": "kodema.approved_base=" + validBase,
				git.EnvPushOptionPrefix + "1": "kodema.approved_candidate=" + validCandidate,
				git.EnvPushOptionPrefix + "2": "kodema.approved_candidate=" + validCandidate,
			},
			wantErr: "duplicate push option kodema.approved_candidate",
		},
		{
			name: "short oid",
			env: map[string]string{
				git.EnvPushOptionCount:        "2",
				git.EnvPushOptionPrefix + "0": "kodema.approved_base=abcdef1234",
				git.EnvPushOptionPrefix + "1": "kodema.approved_candidate=" + validCandidate,
			},
			wantErr: "40-hex commit oid",
		},
		{
			name: "uppercase hex oid",
			env: map[string]string{
				git.EnvPushOptionCount:        "2",
				git.EnvPushOptionPrefix + "0": "kodema.approved_base=" + strings.Repeat("A", 40),
				git.EnvPushOptionPrefix + "1": "kodema.approved_candidate=" + validCandidate,
			},
			wantErr: "40-hex commit oid",
		},
		{
			name: "all-zero null oid",
			env: map[string]string{
				git.EnvPushOptionCount:        "2",
				git.EnvPushOptionPrefix + "0": "kodema.approved_base=" + strings.Repeat("0", 40),
				git.EnvPushOptionPrefix + "1": "kodema.approved_candidate=" + validCandidate,
			},
			wantErr: "40-hex commit oid",
		},
		{
			name: "64-hex sha256 rejected by strict 40-hex check",
			env: map[string]string{
				git.EnvPushOptionCount:        "2",
				git.EnvPushOptionPrefix + "0": "kodema.approved_base=" + validBase,
				git.EnvPushOptionPrefix + "1": "kodema.approved_candidate=" + strings.Repeat("b", 64),
			},
			wantErr: "40-hex commit oid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookFixture(t)
			f.verifier.script(testCredential, executionScript("exe-1"))
			for k, v := range tc.env {
				f.env[k] = v
			}
			stdin := strings.NewReader(hookBase + " " + hookCandidate + " " + testRef + "\n")
			result, err := f.hook.RunPreReceive(context.Background(), stdin)
			if err == nil || result.Accepted() {
				t.Fatalf("RunPreReceive accepted malformed push options: result=%+v err=%v", result, err)
			}
			if !strings.Contains(result.Message, tc.wantErr) {
				t.Fatalf("result.Message = %q; want substring %q", result.Message, tc.wantErr)
			}
			if len(f.verifier.verifyRequests()) != 0 {
				t.Fatalf("verification was called despite malformed push options")
			}
		})
	}

	t.Run("unrelated push options are ignored cleanly", func(t *testing.T) {
		f := newHookFixture(t)
		f.verifier.script(testCredential, executionScript("exe-1"))
		f.executor.setRef("shop/web", testRef, hookBase)
		f.executor.setAncestor("shop/web", hookBase, hookCandidate, true)
		f.env[git.EnvPushOptionCount] = "3"
		f.env[git.EnvPushOptionPrefix+"0"] = "ci.skip"
		f.env[git.EnvPushOptionPrefix+"1"] = "kodema.approved_base=" + hookBase
		f.env[git.EnvPushOptionPrefix+"2"] = "kodema.approved_candidate=" + hookCandidate
		stdin := strings.NewReader(hookBase + " " + hookCandidate + " " + testRef + "\n")
		result, err := f.hook.RunPreReceive(context.Background(), stdin)
		if err != nil || !result.Accepted() {
			t.Fatalf("RunPreReceive = %+v, %v; want accept", result, err)
		}
	})
}

// realGitExecutor runs native git commands directly against real temp bare
// repositories for disjoint-rebase integration tests.
type realGitExecutor struct{}

func (realGitExecutor) Run(ctx context.Context, command git.Command) (git.CommandResult, error) {
	if command.Name != "git" {
		return git.CommandResult{}, fmt.Errorf("unsupported command %q", command.Name)
	}
	cmd := exec.CommandContext(ctx, "git", command.Args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := git.CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// realGitObserver observes refs directly from real bare repositories.
type realGitObserver struct {
	storage git.Storage
	exec    realGitExecutor
}

func (o realGitObserver) Observe(ctx context.Context, repository, ref string) (git.RefState, error) {
	path, err := o.storage.RepositoryPath(testTenant, repository)
	if err != nil {
		return git.RefState{}, err
	}
	res, err := o.exec.Run(ctx, git.Command{
		Name: "git",
		Args: []string{"-C", path, "rev-parse", "--verify", "--quiet", ref},
	})
	if err != nil {
		return git.RefState{}, err
	}
	if res.ExitCode != 0 {
		return git.RefState{Ref: ref, Commit: ""}, nil
	}
	return git.RefState{Ref: ref, Commit: strings.TrimSpace(string(res.Stdout))}, nil
}

// approvalKey identifies one approved `(execution, resource, ref, base, candidate, role)` tuple.
type approvalKey struct {
	credential string
	resourceID string
	ref        string
	base       string
	candidate  string
	approval   string
}

type verifiedRecord struct {
	id         string
	principal  api.VerifiedPrincipal
	resourceID string
	operation  string
	subject    api.Subject
	approved   bool
}

// shellMirrorVerifier faithfully enforces Shell's /api/v1/verify and
// /api/v1/report contracts, including same-execution approval matching and
// exact subject-linkage checking on report.
type shellMirrorVerifier struct {
	mu         sync.Mutex
	principals map[string]struct {
		principal api.VerifiedPrincipal
		roles     []string
	}
	approvals map[approvalKey]bool
	records   map[string]verifiedRecord
	verifies  []api.VerifyRequest
	reports   []api.ReportRequest
	nextID    int
}

func newShellMirrorVerifier() *shellMirrorVerifier {
	return &shellMirrorVerifier{
		principals: map[string]struct {
			principal api.VerifiedPrincipal
			roles     []string
		}{
			"exec-1": {principal: api.VerifiedPrincipal{ID: "exe-1", Kind: "execution"}},
			"exec-2": {principal: api.VerifiedPrincipal{ID: "exe-2", Kind: "execution"}},
			"human-1": {
				principal: api.VerifiedPrincipal{ID: "usr-1", Kind: "human"},
				roles:     []string{"engineer", "reviewer"},
			},
		},
		approvals: make(map[approvalKey]bool),
		records:   make(map[string]verifiedRecord),
	}
}

func (v *shellMirrorVerifier) grantApproval(credential, resourceID, ref, base, candidate, role string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.approvals[approvalKey{
		credential: credential,
		resourceID: resourceID,
		ref:        ref,
		base:       base,
		candidate:  candidate,
		approval:   "role/" + role,
	}] = true
}

func (v *shellMirrorVerifier) Verify(_ context.Context, req api.VerifyRequest) (api.VerifyResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.verifies = append(v.verifies, req)
	entry, ok := v.principals[req.ActorCredential]
	if !ok {
		return api.VerifyResponse{}, fault.New(fault.Unauthenticated, "invalid actor credential")
	}
	approved := false
	if req.Approval != "" {
		if entry.principal.Kind != "execution" {
			return api.VerifyResponse{}, fault.New(fault.Forbidden, "approval evidence requires an execution actor")
		}
		fields := subjectFieldMap(req.Subject)
		key := approvalKey{
			credential: req.ActorCredential,
			resourceID: req.ResourceID,
			ref:        fields["ref"],
			base:       fields["expected_base"],
			candidate:  fields["candidate"],
			approval:   req.Approval,
		}
		if !v.approvals[key] {
			return api.VerifyResponse{}, fault.New(fault.Forbidden, "no matching approval evidence")
		}
		approved = true
	}
	v.nextID++
	verID := fmt.Sprintf("ver-%d", v.nextID)
	v.records[verID] = verifiedRecord{
		id:         verID,
		principal:  entry.principal,
		resourceID: req.ResourceID,
		operation:  req.Operation,
		subject:    req.Subject,
		approved:   approved,
	}
	return api.VerifyResponse{
		Principal:      entry.principal,
		Roles:          append([]string(nil), entry.roles...),
		VerificationID: verID,
		Approval:       approved,
	}, nil
}

func (v *shellMirrorVerifier) Report(_ context.Context, req api.ReportRequest) (api.ReportResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	rec, ok := v.records[req.VerificationID]
	if !ok {
		return api.ReportResponse{}, fault.New(fault.Forbidden, "verification record not found")
	}
	if rec.resourceID != req.ResourceID || rec.operation != req.Operation {
		return api.ReportResponse{}, fault.New(fault.Forbidden, "verification linkage mismatch")
	}
	if !subjectsEqual(rec.subject, req.Subject) {
		return api.ReportResponse{}, fault.New(fault.Forbidden, "report subject does not match the verification record")
	}
	v.reports = append(v.reports, req)
	return api.ReportResponse{
		VerificationID: req.VerificationID,
		EventID:        "evt-" + req.SourceEventID,
	}, nil
}

func subjectFieldMap(subject api.Subject) map[string]string {
	out := make(map[string]string, len(subject.Fields))
	for _, f := range subject.Fields {
		out[f.Name] = f.Value
	}
	return out
}

func subjectsEqual(a, b api.Subject) bool {
	if len(a.Fields) != len(b.Fields) {
		return false
	}
	am := subjectFieldMap(a)
	bm := subjectFieldMap(b)
	for k, v := range am {
		if bm[k] != v {
			return false
		}
	}
	return true
}

// realRepoHarness sets up a real bare repository managed by git.Service plus a
// working clone to construct commits, branches, rebases, and edge cases.
type realRepoHarness struct {
	t        *testing.T
	barePath string
	workPath string
	service  *git.Service
	verifier *shellMirrorVerifier
	journal  *fakeJournal
	observer realGitObserver
	seq      int
}

func newRealRepoHarness(t *testing.T, policyConf string) *realRepoHarness {
	t.Helper()
	root := t.TempDir()
	storage, err := git.NewLocalStorage(root)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	bareLoc, err := storage.EnsureRepository(testTenant, "shop/web")
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	runGitIn(t, "", "init", "--bare", "--quiet", "--initial-branch=main", bareLoc.Path)

	workPath := filepath.Join(t.TempDir(), "work")
	runGitIn(t, "", "clone", "--quiet", bareLoc.Path, workPath)
	runGitIn(t, workPath, "config", "user.name", "Test")
	runGitIn(t, workPath, "config", "user.email", "test@example.invalid")

	config, err := git.ParseConfig([]byte(policyConf))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	verifier := newShellMirrorVerifier()
	journal := newFakeJournal(&callLog{})
	observer := realGitObserver{storage: storage}
	service, err := git.NewService(git.ServiceConfig{
		TenantID:    testTenant,
		Storage:     storage,
		Policies:    &fakePolicySource{config: config},
		Executor:    realGitExecutor{},
		Observer:    observer,
		Journal:     journal,
		Verify:      verifier,
		Retention:   &fakeRetention{},
		Clock:       &fakeClock{now: testNow},
		ResourceIDs: map[string]string{"shop/web": testResourceID},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &realRepoHarness{
		t:        t,
		barePath: bareLoc.Path,
		workPath: workPath,
		service:  service,
		verifier: verifier,
		journal:  journal,
		observer: observer,
	}
}

func (h *realRepoHarness) writeFile(rel string, data []byte, mode os.FileMode) {
	h.t.Helper()
	full := filepath.Join(h.workPath, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("MkdirAll(%s): %v", rel, err)
	}
	if err := os.WriteFile(full, data, mode); err != nil {
		h.t.Fatalf("WriteFile(%s): %v", rel, err)
	}
}

func (h *realRepoHarness) commitAll(msg string) string {
	h.t.Helper()
	runGitIn(h.t, h.workPath, "add", "-A")
	runGitIn(h.t, h.workPath, "commit", "-q", "-m", msg)
	return strings.TrimSpace(runGitIn(h.t, h.workPath, "rev-parse", "HEAD"))
}

func (h *realRepoHarness) pushRef(localRef, remoteRef string) {
	h.t.Helper()
	runGitIn(h.t, h.workPath, "push", "-q", "origin", localRef+":"+remoteRef)
}

func (h *realRepoHarness) nextEventID() string {
	h.seq++
	return fmt.Sprintf("evt-%d", h.seq)
}

func (h *realRepoHarness) update(cred, base, candidate, approvedBase, approvedCandidate string) (git.UpdateResult, error) {
	return h.service.Update(context.Background(), git.UpdateRequest{
		ResourceID:        testResourceID,
		Repository:        "shop/web",
		Ref:               testRef,
		ExpectedBase:      base,
		Candidate:         candidate,
		ApprovedBase:      approvedBase,
		ApprovedCandidate: approvedCandidate,
		ActorCredential:   cred,
		SourceEventID:     h.nextEventID(),
	})
}

func runGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

const disjointPolicyConf = `
repo shop/web
    option resource = res-web
    R = @all
    RW+ refs/heads/work/ = @engineer @execution
    RW refs/heads/main = @execution
    option approve.refs/heads/main = reviewer
    option approve-rebase.refs/heads/main = disjoint
`

const strictPolicyConf = `
repo shop/web
    option resource = res-web
    R = @all
    RW+ refs/heads/work/ = @engineer @execution
    RW refs/heads/main = @execution
    option approve.refs/heads/main = reviewer
`

func TestDisjointRebaseRealGitAcceptsAndRefuses(t *testing.T) {
	t.Run("strict mode without option approve-rebase rejects rebased push even with push options", func(t *testing.T) {
		h := newRealRepoHarness(t, strictPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		// Feature branch changes feat.txt
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("feat.txt", []byte("feature\n"), 0o644)
		approvedCand := h.commitAll("feat")
		h.pushRef("HEAD", "refs/heads/work/approved")

		// Main advances on disjoint other.txt
		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("other.txt", []byte("other\n"), 0o644)
		movedMain := h.commitAll("main advance")
		h.pushRef("HEAD", "refs/heads/main")

		// Rebase work onto movedMain
		runGitIn(t, h.workPath, "checkout", "-q", "work")
		runGitIn(t, h.workPath, "rebase", "-q", "main")
		rebased := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/rebased")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		_, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
		if !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("strict mode Update error = %v; want ErrApprovalRequired", err)
		}
		if len(h.verifier.reports) != 0 {
			t.Fatalf("strict mode sent %d reports; want 0", len(h.verifier.reports))
		}
	})

	t.Run("fast path uses single Verify call when pushed matches approved exactly", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		h.writeFile("feat.txt", []byte("feature\n"), 0o644)
		cand := h.commitAll("feat")
		h.pushRef("HEAD", "refs/heads/work/feat")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, cand, "reviewer")

		res, err := h.update("exec-1", base, cand, base, cand)
		if err != nil {
			t.Fatalf("fast-path Update: %v", err)
		}
		if !res.Reported || res.Ref.Commit != cand {
			t.Fatalf("fast-path result = %+v; want reported commit %s", res, cand)
		}
		if len(h.verifier.verifies) != 1 {
			t.Fatalf("fast-path verify calls = %d; want 1", len(h.verifier.verifies))
		}
	})

	t.Run("disjoint single-commit linear rebase accepted and reports pushed commit", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("dir/feat.txt", []byte("feature\n"), 0o644)
		approvedCand := h.commitAll("feat")
		h.pushRef("HEAD", "refs/heads/work/approved")

		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("dir/other.txt", []byte("other\n"), 0o644)
		movedMain := h.commitAll("main advance")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "work")
		runGitIn(t, h.workPath, "rebase", "-q", "main")
		rebased := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/rebased")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		res, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
		if err != nil {
			t.Fatalf("disjoint single-commit Update: %v", err)
		}
		if !res.Reported || res.Ref.Commit != rebased {
			t.Fatalf("result = %+v; want reported commit %s", res, rebased)
		}
		if len(h.verifier.reports) != 1 {
			t.Fatalf("reports = %d; want 1", len(h.verifier.reports))
		}
		reportFields := subjectFieldMap(h.verifier.reports[0].Subject)
		if reportFields["expected_base"] != movedMain || reportFields["candidate"] != rebased {
			t.Fatalf("reported subject = %+v; want expected_base=%s candidate=%s", reportFields, movedMain, rebased)
		}
		if h.verifier.reports[0].VerificationID != res.Entry.VerificationID {
			t.Fatalf("report VerificationID = %q; want %q", h.verifier.reports[0].VerificationID, res.Entry.VerificationID)
		}
	})

	t.Run("disjoint multi-commit linear rebase with binary, mode, symlink, rename, delete, empty file accepted", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("keep.txt", []byte("keep\n"), 0o644)
		h.writeFile("remove.txt", []byte("remove\n"), 0o644)
		h.writeFile("bin.dat", []byte{0x00, 0x01, 0x02, 0x03}, 0o644)
		h.writeFile("main_only.txt", []byte("v1\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		// Multi-commit feature branch
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("feat.sh", []byte("#!/bin/sh\necho hi\n"), 0o644)
		h.writeFile("bin.dat", []byte{0x00, 0x01, 0x02, 0xff}, 0o644)
		h.writeFile("empty.txt", nil, 0o644)
		h.commitAll("c1 add files and binary")

		if err := os.Chmod(filepath.Join(h.workPath, "feat.sh"), 0o755); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		runGitIn(t, h.workPath, "mv", "keep.txt", "renamed.txt")
		if err := os.Remove(filepath.Join(h.workPath, "remove.txt")); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if err := os.Symlink("renamed.txt", filepath.Join(h.workPath, "link.txt")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		h.commitAll("c2 mode rename delete symlink")

		runGitIn(t, h.workPath, "commit", "-q", "--allow-empty", "-m", "c3 empty")
		approvedCand := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/approved")

		// Main advances on disjoint files (2 commits)
		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("main_only.txt", []byte("v2\n"), 0o644)
		h.commitAll("main1")
		h.writeFile("docs/readme.md", []byte("hello\n"), 0o644)
		movedMain := h.commitAll("main2")
		h.pushRef("HEAD", "refs/heads/main")

		// Rebase work onto movedMain preserving empty commit
		runGitIn(t, h.workPath, "checkout", "-q", "work")
		runGitIn(t, h.workPath, "rebase", "-q", "--empty=keep", "main")
		rebased := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/rebased")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		res, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
		if err != nil {
			t.Fatalf("disjoint multi-commit Update: %v", err)
		}
		if !res.Reported || res.Ref.Commit != rebased {
			t.Fatalf("result = %+v; want reported commit %s", res, rebased)
		}
	})

	t.Run("disjoint rebase onto equal base accepted when patches match", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("feat.txt", []byte("hello\n"), 0o644)
		approvedCand := h.commitAll("original msg")
		h.pushRef("HEAD", "refs/heads/work/approved")

		// Amend only commit message -> different SHA, same base, identical patch.
		runGitIn(t, h.workPath, "commit", "-q", "--amend", "-m", "amended msg")
		rebased := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/rebased")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		res, err := h.update("exec-1", base, rebased, base, approvedCand)
		if err != nil {
			t.Fatalf("equal-base Update: %v", err)
		}
		if !res.Reported || res.Ref.Commit != rebased {
			t.Fatalf("result = %+v; want reported %s", res, rebased)
		}
	})

	t.Run("refuses overlapping file even when git rebase auto-merges cleanly", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("shared.txt", []byte("line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		// Feature edits bottom of shared.txt
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("shared.txt", []byte("line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8-feat\n"), 0o644)
		approvedCand := h.commitAll("edit bottom")
		h.pushRef("HEAD", "refs/heads/work/approved")

		// Main edits top of shared.txt (disjoint hunk -> clean auto-merge!)
		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("shared.txt", []byte("line1-main\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"), 0o644)
		movedMain := h.commitAll("edit top")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "work")
		runGitIn(t, h.workPath, "rebase", "-q", "main")
		rebased := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/rebased")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		_, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
		assertDisjointRefusal(t, err, "overlap")
	})

	t.Run("refuses rename overlap (work renames file modified on main)", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("a.txt", []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		runGitIn(t, h.workPath, "mv", "a.txt", "b.txt")
		approvedCand := h.commitAll("rename a to b")
		h.pushRef("HEAD", "refs/heads/work/approved")

		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("a.txt", []byte("line1-changed\nline2\nline3\nline4\nline5\n"), 0o644)
		movedMain := h.commitAll("modify a")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "work")
		runGitIn(t, h.workPath, "rebase", "-q", "main")
		rebased := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/rebased")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		_, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
		assertDisjointRefusal(t, err, "overlap")
	})

	t.Run("refuses delete overlap (main deletes file renamed/modified or work deletes file modified on main)", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("a.txt", []byte("hello\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		if err := os.Remove(filepath.Join(h.workPath, "a.txt")); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		approvedCand := h.commitAll("delete a")
		h.pushRef("HEAD", "refs/heads/work/approved")

		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("a.txt", []byte("updated on main\n"), 0o644)
		movedMain := h.commitAll("modify a")
		h.pushRef("HEAD", "refs/heads/main")

		// Manually construct a rebased commit deleting a.txt on top of movedMain.
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "rebased", "main")
		if err := os.Remove(filepath.Join(h.workPath, "a.txt")); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		rebased := h.commitAll("delete a after main edit")
		h.pushRef("HEAD", "refs/heads/work/rebased")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		_, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
		assertDisjointRefusal(t, err, "overlap")
	})

	t.Run("refuses file vs directory prefix overlap", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("sub/file.txt", []byte("nested\n"), 0o644)
		approvedCand := h.commitAll("add sub/file.txt")
		h.pushRef("HEAD", "refs/heads/work/approved")

		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("sub", []byte("file named sub\n"), 0o644)
		movedMain := h.commitAll("add file sub")
		h.pushRef("HEAD", "refs/heads/main")

		// Replace file `sub` with `sub/file.txt` on top of movedMain.
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "rebased", "main")
		if err := os.Remove(filepath.Join(h.workPath, "sub")); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		h.writeFile("sub/file.txt", []byte("nested\n"), 0o644)
		rebased := h.commitAll("replace sub with sub/file.txt")
		h.pushRef("HEAD", "refs/heads/work/rebased")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		_, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
		assertDisjointRefusal(t, err, "overlap")
	})

	t.Run("refuses altered content, whitespace-only tampering, mode tampering, binary tampering, and added empty file", func(t *testing.T) {
		tamperCases := []struct {
			name   string
			mutate func(t *testing.T, h *realRepoHarness)
		}{
			{
				name: "altered text content",
				mutate: func(t *testing.T, h *realRepoHarness) {
					h.writeFile("feat.txt", []byte("hello world TAMPERED\n"), 0o644)
				},
			},
			{
				name: "whitespace-only tampering (patch-id bypass attempt)",
				mutate: func(t *testing.T, h *realRepoHarness) {
					h.writeFile("feat.txt", []byte("  hello   world\n"), 0o644)
				},
			},
			{
				name: "file mode tampering 644 to 755 (patch-id bypass attempt)",
				mutate: func(t *testing.T, h *realRepoHarness) {
					if err := os.Chmod(filepath.Join(h.workPath, "feat.txt"), 0o755); err != nil {
						t.Fatalf("Chmod: %v", err)
					}
				},
			},
			{
				name: "binary byte tampering",
				mutate: func(t *testing.T, h *realRepoHarness) {
					h.writeFile("bin.dat", []byte{0x00, 0x01, 0x02, 0x99}, 0o644)
				},
			},
			{
				name: "sneaked extra empty file",
				mutate: func(t *testing.T, h *realRepoHarness) {
					h.writeFile("sneaky-empty.txt", nil, 0o644)
				},
			},
		}

		for _, tc := range tamperCases {
			t.Run(tc.name, func(t *testing.T) {
				h := newRealRepoHarness(t, disjointPolicyConf)
				h.writeFile("base.txt", []byte("base\n"), 0o644)
				base := h.commitAll("base")
				h.pushRef("HEAD", "refs/heads/main")

				runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
				h.writeFile("feat.txt", []byte("hello world\n"), 0o644)
				h.writeFile("bin.dat", []byte{0x00, 0x01, 0x02, 0x04}, 0o644)
				approvedCand := h.commitAll("feat")
				h.pushRef("HEAD", "refs/heads/work/approved")

				runGitIn(t, h.workPath, "checkout", "-q", "main")
				h.writeFile("other.txt", []byte("other\n"), 0o644)
				movedMain := h.commitAll("main advance")
				h.pushRef("HEAD", "refs/heads/main")

				runGitIn(t, h.workPath, "checkout", "-q", "work")
				runGitIn(t, h.workPath, "rebase", "-q", "main")
				tc.mutate(t, h)
				runGitIn(t, h.workPath, "add", "-A")
				runGitIn(t, h.workPath, "commit", "-q", "--amend", "--no-edit")
				rebased := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
				h.pushRef("HEAD", "refs/heads/work/rebased")

				h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

				_, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
				assertDisjointRefusal(t, err, "differs from approved")
			})
		}
	})

	t.Run("refuses extra commit added or commit dropped/squashed during rebase", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("f1.txt", []byte("one\n"), 0o644)
		h.commitAll("c1")
		h.writeFile("f2.txt", []byte("two\n"), 0o644)
		approvedCand := h.commitAll("c2")
		h.pushRef("HEAD", "refs/heads/work/approved")

		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("other.txt", []byte("other\n"), 0o644)
		movedMain := h.commitAll("main advance")
		h.pushRef("HEAD", "refs/heads/main")

		// Case A: extra commit added
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "extra", "work")
		runGitIn(t, h.workPath, "rebase", "-q", "main")
		h.writeFile("f3.txt", []byte("three\n"), 0o644)
		extraTip := h.commitAll("c3 extra")
		h.pushRef("HEAD", "refs/heads/work/extra")

		// Case B: commits squashed (1 commit instead of 2)
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "squashed", "main")
		h.writeFile("f1.txt", []byte("one\n"), 0o644)
		h.writeFile("f2.txt", []byte("two\n"), 0o644)
		squashedTip := h.commitAll("c1+c2 squashed")
		h.pushRef("HEAD", "refs/heads/work/squashed")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		_, err := h.update("exec-1", movedMain, extraTip, base, approvedCand)
		assertDisjointRefusal(t, err, "commit count differs")

		_, err = h.update("exec-1", movedMain, squashedTip, base, approvedCand)
		assertDisjointRefusal(t, err, "commit count differs")
	})

	t.Run("refuses merge commit in approved or pushed series", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		// Side branch + merge in approved series
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "side")
		h.writeFile("side.txt", []byte("side\n"), 0o644)
		h.commitAll("side")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work-merge", base)
		h.writeFile("feat.txt", []byte("feat\n"), 0o644)
		h.commitAll("feat")
		runGitIn(t, h.workPath, "merge", "-q", "--no-ff", "side", "-m", "merge side")
		approvedMerge := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/merge-approved")

		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("other.txt", []byte("other\n"), 0o644)
		movedMain := h.commitAll("main advance")
		h.pushRef("HEAD", "refs/heads/main")

		// Linear push attempting to claim approval from approvedMerge
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "linear-push", "main")
		h.writeFile("feat.txt", []byte("feat\n"), 0o644)
		h.writeFile("side.txt", []byte("side\n"), 0o644)
		linearPush := h.commitAll("linear")
		h.pushRef("HEAD", "refs/heads/work/linear-push")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedMerge, "reviewer")
		_, err := h.update("exec-1", movedMain, linearPush, base, approvedMerge)
		assertDisjointRefusal(t, err, "merge commit")

		// Now linear approved series, but pushed series has a merge commit!
		runGitIn(t, h.workPath, "checkout", "-q", "-b", "linear-approved", base)
		h.writeFile("clean.txt", []byte("clean\n"), 0o644)
		cleanApproved := h.commitAll("clean")
		h.pushRef("HEAD", "refs/heads/work/clean-approved")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "pushed-merge", "main")
		runGitIn(t, h.workPath, "merge", "-q", "--no-ff", "linear-approved", "-m", "merge clean")
		pushedMerge := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/pushed-merge")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, cleanApproved, "reviewer")
		_, err = h.update("exec-1", movedMain, pushedMerge, base, cleanApproved)
		assertDisjointRefusal(t, err, "merge commit")
	})

	t.Run("refuses non-fast-forward push even when policy grants RW+", func(t *testing.T) {
		rwPlusConf := `
repo shop/web
    option resource = res-web
    R = @all
    RW+ refs/heads/main = @execution
    option approve.refs/heads/main = reviewer
    option approve-rebase.refs/heads/main = disjoint
`
		h := newRealRepoHarness(t, rwPlusConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("feat.txt", []byte("feat\n"), 0o644)
		approvedCand := h.commitAll("feat")
		h.pushRef("HEAD", "refs/heads/work/approved")

		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("other.txt", []byte("other\n"), 0o644)
		movedMain := h.commitAll("main advance")
		h.pushRef("HEAD", "refs/heads/main")

		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, approvedCand, "reviewer")

		// Pushing approvedCand directly without rebasing onto movedMain is a rewind!
		_, err := h.update("exec-1", movedMain, approvedCand, base, approvedCand)
		assertDisjointRefusal(t, err, "fast-forward")
	})

	t.Run("refuses missing/unreachable approved commits, unapproved candidate, foreign execution, and human actor", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		base := h.commitAll("base")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "-b", "work")
		h.writeFile("feat.txt", []byte("feat\n"), 0o644)
		approvedCand := h.commitAll("feat")
		h.pushRef("HEAD", "refs/heads/work/approved")

		runGitIn(t, h.workPath, "checkout", "-q", "main")
		h.writeFile("other.txt", []byte("other\n"), 0o644)
		movedMain := h.commitAll("main advance")
		h.pushRef("HEAD", "refs/heads/main")

		runGitIn(t, h.workPath, "checkout", "-q", "work")
		runGitIn(t, h.workPath, "rebase", "-q", "main")
		rebased := strings.TrimSpace(runGitIn(t, h.workPath, "rev-parse", "HEAD"))
		h.pushRef("HEAD", "refs/heads/work/rebased")

		// 1. Unapproved candidate (no approval granted yet)
		_, err := h.update("exec-1", movedMain, rebased, base, approvedCand)
		if !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("unapproved candidate error = %v; want ErrApprovalRequired", err)
		}

		// 2. Foreign execution approval (exec-2 holds approval, exec-1 pushes)
		h.verifier.grantApproval("exec-2", testResourceID, testRef, base, approvedCand, "reviewer")
		_, err = h.update("exec-1", movedMain, rebased, base, approvedCand)
		if !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("foreign execution error = %v; want ErrApprovalRequired", err)
		}

		// 3. Human actor cannot satisfy execution approval
		_, err = h.update("human-1", movedMain, rebased, base, approvedCand)
		if !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("human actor error = %v; want ErrApprovalRequired", err)
		}

		// 4. Missing approved_candidate commit in repository object store
		missingOID := strings.Repeat("d", 40)
		h.verifier.grantApproval("exec-1", testResourceID, testRef, base, missingOID, "reviewer")
		_, err = h.update("exec-1", movedMain, rebased, base, missingOID)
		assertDisjointRefusal(t, err, "not found in repository")
	})

	t.Run("unborn ref creation does not enter disjoint-rebase path", func(t *testing.T) {
		h := newRealRepoHarness(t, disjointPolicyConf)
		h.writeFile("base.txt", []byte("base\n"), 0o644)
		c1 := h.commitAll("c1")
		h.writeFile("feat.txt", []byte("feat\n"), 0o644)
		c2 := h.commitAll("c2")
		h.pushRef("HEAD", "refs/heads/work/seed")

		// Note: refs/heads/main does NOT exist yet!
		h.verifier.grantApproval("exec-1", testResourceID, testRef, c1, c2, "reviewer")

		// Attempt creating main (ExpectedBase="") while pointing push options at (c1, c2).
		_, err := h.update("exec-1", "", c2, c1, c2)
		if !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("unborn ref creation error = %v; want ErrApprovalRequired", err)
		}
	})
}

func assertDisjointRefusal(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected refusal containing %q, got nil", wantSubstr)
	}
	if !errors.Is(err, git.ErrApprovalRequired) {
		t.Fatalf("error = %v; want errors.Is(_, ErrApprovalRequired)", err)
	}
	var refusal *git.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %T (%v); want *git.RefusalError", err, err)
	}
	if !strings.Contains(refusal.ClientMessage, wantSubstr) || !strings.Contains(refusal.ClientMessage, "fresh review required") {
		t.Fatalf("refusal.ClientMessage = %q; want substrings %q and %q", refusal.ClientMessage, wantSubstr, "fresh review required")
	}
}

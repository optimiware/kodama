package entrypoint

// The v3 wire and the two runtime-issued Git grants (T-151), over loopback HTTP
// with the real adapter: route selection, version refusal, and `kodama step-git`
// with the entrypoint's push of the seed and the integration.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	gitsupport "github.com/optimiware/kodama/test/support/git"
)

// sourceFreeV3 is a valid source-free ContextV3 running script.
func sourceFreeV3(t testing.TB, script string) runtimev3.ContextV3 {
	t.Helper()
	c := runtimev3.ContextV3{
		ContextVersion: 3, RunModel: "run_shared",
		TenantID: "t_acme", ProjectID: "p_inventory", RunID: "run_01", StepID: "build", AttemptID: "att_unit01", ActionKey: "run_01.build",
		Definition: runtimev1.Definition{ID: "pd_01", Digest: runtimev1.Digest("sha256:" + strings.Repeat("d", 64))},
		Origin:     runtimev3.Origin{Kind: runtimev3.OriginManual},
		Command:    runtimev1.Command{Image: testImage, Command: []string{"/bin/sh", "-c"}, Args: []string{script}, Env: map[string]string{}},
		Workspace:  runtimev3.WorkspaceV3{Access: runtimev3.AccessNone, MountPath: "/workspace", Sources: []runtimev3.SourceV3{}, Lineage: []runtimev3.LineageEntryV3{}},
		Endpoints:  map[string]runtimev3.Endpoint{}, Egress: []string{},
		Run:    runtimev3.RunWindow{StartedAt: timestamp(-time.Minute), DeadlineAt: timestamp(time.Hour)},
		Policy: runtimev3.PolicyPin{ID: "run-v1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("f", 64))},
		Inputs: map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 30, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 64 << 10},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the fixture context is invalid: %v", err)
	}
	return c
}

// wireServerV3 is a minimal runtime API of ONE route family over a source-free
// attempt; answer makes it reply with another version's context.
type wireServerV3 struct {
	t                     *testing.T
	srv                   *httptest.Server
	wire                  string
	answer                string
	v3ctx                 runtimev3.ContextV3
	v2ctx                 runtimev2.ContextV2
	v1ctx                 runtimev1.Context
	mu                    sync.Mutex
	seen                  []string
	begin                 string
	result                string
	beginBody, resultBody string
}

func newWireServerV3(t *testing.T, wire, answer string) *wireServerV3 {
	t.Helper()
	w := &wireServerV3{t: t, wire: wire, answer: answer, v3ctx: sourceFreeV3(t, "true"), v2ctx: sharedContext(t, "true", freeWorkspace()), v1ctx: unitContext("true")}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *wireServerV3) URL() string { return w.srv.URL }

func (w *wireServerV3) paths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

func (w *wireServerV3) sawBegin() bool {
	for _, p := range w.paths() {
		if strings.HasSuffix(p, "/begin") {
			return true
		}
	}
	return false
}

func replyJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

func failV3(rw http.ResponseWriter, status int, code domain.ErrorCode) {
	replyJSON(rw, status, runtimev3.ErrorResponse{Error: runtimev3.ErrorBody{Code: code, Message: string(code), RequestID: "req_wire", Details: map[string]any{}}})
}

func (w *wireServerV3) context() any {
	switch w.answer {
	case "v1ctx":
		return w.v1ctx
	case "v2ctx":
		return w.v2ctx
	}
	return w.v3ctx
}

func (w *wireServerV3) serve(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	w.seen = append(w.seen, r.URL.Path)
	w.mu.Unlock()
	route, ok := strings.CutPrefix(r.URL.Path, "/runtime/"+w.wire+"/attempts/att_unit01")
	if !ok {
		failV3(rw, http.StatusNotFound, domain.CodeNotFound)
		return
	}
	token := tokenInfo("rt_wire_token_0123456789abcdef", time.Hour, 2*time.Hour)
	switch route {
	case "/admit":
		switch w.answer {
		case "v1ctx":
			replyJSON(rw, http.StatusOK, runtimev1.AdmitResponse{Token: token, Context: w.v1ctx})
		case "v2ctx":
			replyJSON(rw, http.StatusOK, runtimev2.AdmitResponseV2{Token: token, Context: w.v2ctx})
		default:
			replyJSON(rw, http.StatusOK, runtimev3.AdmitResponseV3{Token: token, Context: w.v3ctx})
		}
	case "/context":
		replyJSON(rw, http.StatusOK, w.context())
	case "/token":
		replyJSON(rw, http.StatusOK, runtimev1.TokenResponse{Token: token})
	case "/begin":
		w.mu.Lock()
		w.beginBody = strings.TrimSpace(string(body))
		w.mu.Unlock()
		replyJSON(rw, http.StatusOK, runtimev3.BeginResponseV3{AttemptID: "att_unit01", BeganAt: timestamp(0), WorkspaceState: runtimev3.StateNone})
	case "/result":
		var q runtimev3.ResultRequestV3
		if json.Unmarshal(body, &q) != nil {
			failV3(rw, http.StatusBadRequest, domain.CodeInvalidRequest)
			return
		}
		w.mu.Lock()
		w.resultBody = string(body)
		w.mu.Unlock()
		replyJSON(rw, http.StatusOK, runtimev1.ResultResponse{AttemptID: "att_unit01", ResultHash: q.ResultHash, AttemptStatus: runtimev1.AttemptSucceeded, AcceptedAt: timestamp(0)})
	default:
		failV3(rw, http.StatusNotFound, domain.CodeNotFound)
	}
}

// ---- runtime-issued grants -----------------------------------------------------------------

func workloadActor() gitv1.Actor {
	run, attempt := "run_01", "att_unit01"
	return gitv1.Actor{Type: gitv1.ActorWorkload, ID: "wl_att_unit01", RunID: &run, AttemptID: &attempt}
}

func claims(kind gitv1.GrantKind, repo gitv1.GrantRepository) gitv1.GrantClaims {
	return gitv1.GrantClaims{
		GrantID: "gg_" + string(kind), Audience: gitv1.GrantAudience, TenantID: "t_acme", ProjectID: "p_inventory", Actor: workloadActor(), Kind: kind,
		Repositories: []gitv1.GrantRepository{repo}, IssuedAt: gitv1.Timestamp(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)),
		ExpiresAt: gitv1.Timestamp(time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)),
	}
}

// integrationGrant answers an integration request with the approved update of the
// fixture's backend and the credential cred(n).
func (f *v3Fixture) integrationGrant(candidate string, cred func(n int) string) {
	f.v3.integration = func(n int, q runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
		g := claims(gitv1.GrantIntegration, gitv1.GrantRepository{
			ID: q.RepositoryID, Read: true, Update: []string{"refs/heads/main"},
			Integration: &gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: f.backend.pinned, To: candidate, GateID: q.GateID},
		})
		return runtimev3.GitGrantResponse{Grant: g, Credential: cred(n), CredentialFile: "/kodama/git/integration-credential"}, nil
	}
}

// stepGit is the command line a test command uses; TEST_BIN is this test binary,
// which dispatches `step-git` (TestMain).
func (f *v3Fixture) withTestBin(script string) {
	c := f.v3.ctx
	c.Command.Args = []string{script}
	c.Command.Env = map[string]string{"TEST_BIN": os.Args[0]}
	f.v3.ctx = c
}

func Test_unit_step_git_integration_pushes_the_approved_candidate(t *testing.T) {
	f := newV3Fixture(t, true, "")
	candidate, work := f.backend.repo.Commit(t, f.backend.pinned, "approved candidate", gitsupport.File{Path: "candidate.txt", Content: "approved"})
	tgit(t, f.backend.dir(f), "fetch", "--quiet", work, candidate) // the candidate is in the workspace's object store
	f.integrationGrant(candidate, func(int) string { return f.gs.WriteToken })
	f.withTestBin(`"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1 > "$KODAMA_OUTPUTS_DIR/grant.txt"; env | grep -c 'KODAMA_TOKEN_FILE\|KODAMA_SERVER_URL' > "$KODAMA_OUTPUTS_DIR/envcount" || true`)
	r := f.wantOK()
	if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 1 {
		t.Fatalf("result %+v\n%s", r, f.logs.String())
	}
	got := r.Commits[0]
	if got.RepositoryID != "repo_01" || got.Ref != "refs/heads/main" || string(got.Commit) != candidate || got.PushID != "push_1" {
		t.Fatalf("commit output %+v", got)
	}
	if main := tipOf(t, f.backend, "refs/heads/main"); main != candidate {
		t.Fatalf("main is %s, want the approved candidate %s", main, candidate)
	}
	if tip := tipOf(t, f.backend, v3Task); tip != f.backend.pinned {
		t.Fatalf("the task branch moved to %s", tip)
	}
	cred := filepath.Join(f.cfg.GitDir, "integration-credential")
	if fi, err := os.Stat(cred); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.cfg.OutputsDir, "grant.txt")); strings.Contains(string(b), f.gs.WriteToken) || !strings.Contains(string(b), "integration gg_integration refs/heads/main") {
		t.Fatalf("step-git printed %q", b)
	}
	if strings.Contains(f.logs.String(), f.gs.WriteToken) {
		t.Fatal("the credential is in the logs")
	}
	// The command never held the attempt's token or the control plane's address.
	if got := strings.TrimSpace(mustRead(t, filepath.Join(f.cfg.OutputsDir, "envcount"))); got != "0" {
		t.Fatalf("the command's environment names the token file or the server: %s", got)
	}
}

func Test_unit_step_git_integration_is_exactly_the_approved_update(t *testing.T) {
	// The approved candidate is not in the workspace: nothing is pushed, nothing invented.
	f := newV3Fixture(t, true, "")
	other, _ := f.backend.repo.Commit(t, f.backend.pinned, "never fetched", gitsupport.File{Path: "n.txt", Content: "n"})
	f.integrationGrant(other, func(int) string { return f.gs.WriteToken })
	f.withTestBin(`"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1`)
	r := f.wantOK()
	f.wantFailed(r, ReasonSourceFailed)
	if tipOf(t, f.backend, "refs/heads/main") != f.backend.pinned {
		t.Fatal("main moved")
	}
}

func Test_unit_step_git_seed_pushes_the_default_branch_of_a_created_repository(t *testing.T) {
	requireGit(t)
	u := newUnit(t, "true")
	gs := gitsupport.NewServer(t)
	bare := filepath.Join(gs.Root, "seeded.git")
	tgit(t, ".", "init", "--quiet", "--bare", "-b", "main", bare)
	tgit(t, bare, "config", "http.receivepack", "true")
	tgit(t, bare, "config", "http.uploadpack", "true")
	repo := &gitsupport.Repo{Name: "seeded", Path: bare}
	installPushIDHook(t, repo)

	c := sourceFreeV3(t, "")
	c.Origin = runtimev3.Origin{Kind: runtimev3.OriginProvision}
	ca := "/kodama/git/ca.pem"
	c.Git = &runtimev3.GitDelivery{BaseURL: gs.URL, CAFile: &ca, Repositories: []runtimev3.GitRepositoryAccess{},
		Runtime: runtimev3.GitRuntimeGrants{Seed: &runtimev3.GitRuntimeGrant{CredentialFile: "/kodama/git/seed-credential"}}}
	c.Command.Env = map[string]string{"TEST_BIN": os.Args[0]}
	url := gs.URL + "/seeded.git"
	c.Command.Args = []string{`set -e
mkdir seed && cd seed && git init -q -b main && echo '# seeded' > README.md && git add README.md &&
git -c user.name=a -c user.email=a@example.invalid commit -q -m initial
"$TEST_BIN" step-git seed --repository repo_09 --url ` + url + ` --dir "$KODAMA_WORKSPACE/seed"
echo "seed credential: $(cat "$KODAMA_WORKSPACE/../git/seed-credential")"`}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	u.cfg.GitDir = filepath.Join(u.root, "git")
	writeFile(t, filepath.Join(u.cfg.GitDir, "ca.pem"), mustRead(t, gs.CAFile))
	if err := os.MkdirAll(u.cfg.WorkspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	u.cfg.ContainerLog = &safeBuf{}
	v3 := &scriptV3{ctx: c}
	v3.seed = func(_ int, q runtimev3.GitSeedRequest) (runtimev3.GitGrantResponse, error) {
		g := claims(gitv1.GrantSeed, gitv1.GrantRepository{ID: q.RepositoryID, Read: true, Update: []string{"refs/heads/main"}, Seed: true})
		return runtimev3.GitGrantResponse{Grant: g, Credential: gs.WriteToken, CredentialFile: "/kodama/git/seed-credential"}, nil
	}
	u.rt.propose = func(int, runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
		t.Error("a source was proposed")
		return runtimev1.SourceProposalResponse{}, &APIError{Status: 500}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if code := NewStepV3(u.cfg, u.rt, v3, u.up).Run(ctx); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, u.logs.String())
	}
	r := v3.results[len(v3.results)-1].Result
	local := tgit(t, filepath.Join(u.cfg.WorkspaceDir, "seed"), "rev-parse", "HEAD")
	if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 1 {
		t.Fatalf("result %+v\n%s", r, u.logs.String())
	}
	if got := r.Commits[0]; got.RepositoryID != "repo_09" || got.Ref != "refs/heads/main" || string(got.Commit) != local || got.PushID != "push_1" {
		t.Fatalf("commit output %+v, want the seed %s", got, local)
	}
	if tip := tgit(t, bare, "rev-parse", "refs/heads/main"); tip != local {
		t.Fatalf("the repository's default branch is %s, want %s", tip, local)
	}
	if refs := tgit(t, bare, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Fatalf("refs %q", refs)
	}
	// The command printed the seed credential: neither the retained log nor the live
	// container log nor the entrypoint's own log carries it.
	if log, _, _, ok := (&unit{rt: u.rt, up: u.up}).diagnostic("command.log"); !ok || bytes.Contains(log, []byte(gs.WriteToken)) || !bytes.Contains(log, []byte("seed credential:")) {
		t.Fatalf("command.log: %v %q", ok, log)
	}
	if strings.Contains(u.cfg.ContainerLog.(*safeBuf).String(), gs.WriteToken) || strings.Contains(u.logs.String(), gs.WriteToken) {
		t.Fatal("the seed credential is in a log")
	}
}

// Every runtime-issued credential is registered before the command can see it and
// stays registered for the attempt: live output, the retained log and the pushed
// source are all held to it, however often the file is replaced.
func Test_unit_step_git_credentials_are_registered_before_they_are_exposed(t *testing.T) {
	const first, second = "first-integration-credential-AAAA1111", "second-integration-credential-BBBB2222"
	f := newV3Fixture(t, true, "")
	candidate, work := f.backend.repo.Commit(t, f.backend.pinned, "approved candidate", gitsupport.File{Path: "candidate.txt", Content: "approved"})
	tgit(t, f.backend.dir(f), "fetch", "--quiet", work, candidate)
	f.integrationGrant(candidate, func(n int) string { return map[int]string{1: first, 2: second}[n] })
	f.cfg.ContainerLog = &safeBuf{}
	cred := filepath.Join(f.cfg.GitDir, "integration-credential")
	f.withTestBin(`set -e
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1
FIRST=$(cat ` + cred + `)
echo "live: $FIRST"
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1
echo "live: $(cat ` + cred + `)"
echo "$FIRST" > backend/leak.txt`)
	r := f.wantOK()
	// The earlier credential, replaced by the second issuance, is still in the disclosure set.
	f.wantFailed(r, ReasonSecretInSource)
	if tipOf(t, f.backend, v3Task) != f.backend.pinned || tipOf(t, f.backend, "refs/heads/main") != f.backend.pinned {
		t.Fatal("a commit containing an earlier credential was pushed")
	}
	if n := f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" }); n != 0 {
		t.Fatalf("%d receive requests", n)
	}
	if f.v3.count("integrationGitGrant") != 2 {
		t.Fatalf("calls %v", f.v3.calls)
	}
	log, _, _, ok := f.diagnostic("command.log")
	if !ok || !bytes.Contains(log, []byte("live:")) {
		t.Fatalf("command.log: %v %q", ok, log)
	}
	live := f.cfg.ContainerLog.(*safeBuf).String()
	if !strings.Contains(live, "live:") {
		t.Fatalf("the live container log shows nothing: %q", live)
	}
	for name, hay := range map[string]string{"command.log": string(log), "live container log": live, "entrypoint log": f.logs.String()} {
		for _, c := range []string{first, second} {
			if strings.Contains(hay, c) {
				t.Fatalf("a runtime-issued credential is in the %s:\n%s", name, hay)
			}
		}
	}
	for id, obj := range f.up.objects {
		for _, c := range []string{first, second} {
			if bytes.Contains(obj, []byte(c)) {
				t.Fatalf("a runtime-issued credential is in the upload %s", id)
			}
		}
	}
}

func Test_unit_step_git_refusals(t *testing.T) {
	// Each case runs one broker request from a real command and records its status.
	type tc struct {
		name, cmd string
		grant     func(f *v3Fixture)
		want      string
	}
	run := func(c tc) {
		t.Run(c.name, func(t *testing.T) {
			f := newV3Fixture(t, true, "")
			if c.grant != nil {
				c.grant(f)
			}
			status := filepath.Join(f.root, "status")
			f.withTestBin(`B="$TEST_BIN"; ` + c.cmd + `; echo $? > ` + status)
			f.wantOK()
			if got := strings.TrimSpace(mustRead(t, status)); got != c.want {
				t.Fatalf("step-git exited %s, want %s\n%s", got, c.want, f.logs.String())
			}
			if entries, _ := os.ReadDir(f.cfg.GitDir); len(entries) != 2 {
				t.Fatalf("a refused request wrote a Git file: %v", entries)
			}
			if strings.Contains(f.logs.String(), "secret-credential") {
				t.Fatal("a credential is in the logs")
			}
		})
	}
	bad := func(file string, mutate func(*gitv1.GrantClaims)) func(*v3Fixture) {
		return func(f *v3Fixture) {
			f.v3.integration = func(_ int, q runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
				g := claims(gitv1.GrantIntegration, gitv1.GrantRepository{ID: q.RepositoryID, Read: true, Update: []string{"refs/heads/main"},
					Integration: &gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: f.backend.pinned, To: strings.Repeat("c", 40), GateID: q.GateID}})
				mutate(&g)
				return runtimev3.GitGrantResponse{Grant: g, Credential: "secret-credential-0123456789", CredentialFile: file}, nil
			}
		}
	}
	const integ = `"$B" step-git integration --repository repo_01 --gate gate_1`
	run(tc{"no seed capability", `"$B" step-git seed --repository repo_09 --url https://x.example.invalid/a.git --dir "$KODAMA_WORKSPACE"`, nil, "1"})
	run(tc{"missing arguments", `"$B" step-git integration --repository repo_01`, nil, "2"})
	run(tc{"an unknown subcommand", `"$B" step-git fetch`, nil, "2"})
	run(tc{"a request with the wrong key", `KODAMA_STEP_GIT_KEY=wrong ` + integ, bad("/kodama/git/integration-credential", func(*gitv1.GrantClaims) {}), "1"})
	run(tc{"the control plane refuses", integ, nil, "1"})
	run(tc{"another credential file", integ, bad("/kodama/git/credential", func(*gitv1.GrantClaims) {}), "1"})
	run(tc{"another repository's grant", integ, bad("/kodama/git/integration-credential", func(g *gitv1.GrantClaims) { g.Repositories[0].ID = "repo_02" }), "1"})
	run(tc{"another gate's grant", integ, bad("/kodama/git/integration-credential", func(g *gitv1.GrantClaims) { g.Repositories[0].Integration.GateID = "gate_2" }), "1"})

	t.Run("the wrong key never reaches the control plane", func(t *testing.T) {
		f := newV3Fixture(t, true, "")
		f.withTestBin(`KODAMA_STEP_GIT_KEY=wrong "$TEST_BIN" step-git integration --repository repo_01 --gate gate_1 || true`)
		f.wantOK()
		if f.v3.count("integrationGitGrant") != 0 {
			t.Fatal("an unauthenticated request was forwarded")
		}
	})

	t.Run("a command without a grant capability gets no socket", func(t *testing.T) {
		f := newV3Fixture(t, false, "")
		c := f.v3.ctx
		c.Git.Runtime.Integration = nil
		c.Workspace.Sources[0].Access, c.Workspace.Sources[0].Branch = gitv1.AccessRead, nil
		set, _ := c.Workspace.SourceSet()
		d, _ := set.Digest()
		dd := runtimev3.Digest(d)
		c.Workspace.SourceSetDigest = &dd
		acc, _ := c.GitAccess()
		c.Git.Repositories = acc
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		f.v3.ctx = c
		f.withTestBin(`env | grep -c KODAMA_STEP_GIT > ` + filepath.Join(f.root, "seen") + ` || true`)
		f.wantOK()
		if got := strings.TrimSpace(mustRead(t, filepath.Join(f.root, "seen"))); got != "0" {
			t.Fatalf("a command without a grant capability was given the broker: %s", got)
		}
	})
}

// ---- the broker's lifetime ------------------------------------------------------------------

func socketDirOf(t *testing.T, file string) string {
	t.Helper()
	return filepath.Dir(strings.TrimSpace(mustRead(t, file)))
}

// A stalled grant call is cancelled when the command ends, a request queued behind
// it issues nothing, and the step finishes within its budget.
func Test_unit_step_git_broker_ends_with_the_command(t *testing.T) {
	f := newV3Fixture(t, true, "")
	f.cfg.FinalizeTimeout = 10 * time.Second
	started, cancelled := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.v3.integrationCtx = func(ctx context.Context, n int, _ runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
		once.Do(func() { close(started) })
		<-ctx.Done() // the control plane never answers
		close(cancelled)
		return runtimev3.GitGrantResponse{}, ctx.Err()
	}
	sock := filepath.Join(f.root, "sock")
	c := f.v3.ctx
	c.Limits.TimeoutSeconds = 2
	f.v3.ctx = c
	f.withTestBin(`echo "$KODAMA_STEP_GIT_SOCKET" > ` + sock + `
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1 &
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_2 &
sleep 30`)
	begun := time.Now()
	r := f.wantOK()
	elapsed := time.Since(begun)
	f.wantFailed(r, ReasonTimeout)
	select {
	case <-started:
	default:
		t.Fatal("the grant call never started")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("the stalled grant call was not cancelled when the command ended")
	}
	if n := f.v3.count("integrationGitGrant"); n != 1 {
		t.Fatalf("%d grant calls: the request queued behind the stalled one must issue nothing", n)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("the step took %s: shutdown delayed finalization past the command's timeout plus a short bound", elapsed)
	}
	if _, err := os.Stat(socketDirOf(t, sock)); err == nil {
		t.Fatal("the private socket directory survived")
	}
	if entries, _ := os.ReadDir(f.cfg.GitDir); len(entries) != 2 {
		t.Fatalf("a grant file was written: %v", entries)
	}
}

// Loss of authority ends the command and still removes the broker's socket.
func Test_unit_step_git_broker_is_removed_when_authority_is_lost(t *testing.T) {
	f := newV3Fixture(t, true, "")
	sock := filepath.Join(f.root, "sock")
	f.withTestBin(`echo "$KODAMA_STEP_GIT_SOCKET" > ` + sock + `
sleep 30`)
	f.v3.admit = func() (runtimev3.AdmitResponseV3, error) {
		return runtimev3.AdmitResponseV3{Token: tokenInfo("rt_unit_token_0123456789abcdef", 800*time.Millisecond, 2*time.Hour), Context: f.v3.ctx}, nil
	}
	f.rt.renew = func() (runtimev1.TokenResponse, error) {
		return runtimev1.TokenResponse{}, &APIError{Op: "renewToken", Status: 403, Code: runtimev1.CodeAttemptRevoked}
	}
	begun := time.Now()
	if code := f.run(); code != ExitRevoked {
		t.Fatalf("exit %d, want %d\n%s", code, ExitRevoked, f.logs.String())
	}
	if time.Since(begun) > 20*time.Second {
		t.Fatal("the revoked step lingered")
	}
	if _, err := os.Stat(socketDirOf(t, sock)); err == nil {
		t.Fatal("the socket directory of a revoked attempt survived")
	}
	if f.v3.count("submitResult") != 0 {
		t.Fatal("a revoked attempt submitted a result")
	}
}

// A normal command end also removes the socket, and a request arriving after it
// finds nothing to talk to.
func Test_unit_step_git_broker_is_gone_after_the_command(t *testing.T) {
	f := newV3Fixture(t, true, "")
	sock := filepath.Join(f.root, "sock")
	f.withTestBin(`echo "$KODAMA_STEP_GIT_SOCKET" > ` + sock + ` && echo "$KODAMA_STEP_GIT_KEY" > ` + sock + `.key`)
	f.wantOK()
	path := strings.TrimSpace(mustRead(t, sock))
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		t.Fatal("the socket directory survived the command")
	}
	var out, errb bytes.Buffer
	env := map[string]string{EnvGitSocket: path, EnvGitKey: strings.TrimSpace(mustRead(t, sock+".key"))}
	code := RunStepGit(context.Background(), []string{"integration", "--repository", "repo_01", "--gate", "gate_1"}, func(k string) string { return env[k] }, &out, &errb)
	if code != ExitError || f.v3.count("integrationGitGrant") != 0 {
		t.Fatalf("a late request: exit %d, grant calls %d", code, f.v3.count("integrationGitGrant"))
	}
}

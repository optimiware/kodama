package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitservice"
	"github.com/optimiware/kodama/internal/kernel/api"
	"github.com/optimiware/kodama/internal/kernel/serve"
	"github.com/optimiware/kodama/internal/kernel/store"
)

// TestMain lets the test binary be the hook binary: the worker's hook shims run
// `<test binary> git-hook <name>`, as `kodama git-hook` does in the product.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "git-hook" {
		os.Exit(gitservice.HookMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Environ()))
	}
	os.Exit(m.Run())
}

var (
	hookOnce sync.Once
	hookPath string
)

// hookBinary builds the minimal hook binary (what `kodama git-hook` does) once,
// without the race detector: a race-instrumented test binary starts slowly and a
// push runs several hooks. It falls back to the test binary, which serves
// git-hook from TestMain.
func hookBinary(t *testing.T) string {
	t.Helper()
	hookOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kernel-git-hook-")
		if err != nil {
			return
		}
		out := filepath.Join(dir, "kodama")
		build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-buildvcs=false", "-o", out,
			"github.com/optimiware/kodama/internal/gitservice/internal/hookmain")
		build.Env = append(os.Environ(), "GOFLAGS=")
		if msg, err := build.CombinedOutput(); err != nil {
			t.Logf("building the hook binary failed, using the test binary: %v\n%s", err, msg)
			return
		}
		hookPath = out
	})
	return hookPath
}

type reconcileMarker struct{}

// lostOutcomes drops the post-receive outcome of the next dropNext receives for
// good: every report of such a receive made by the worker's hook (its first call
// and its background retries) fails, which is a lost confirmation. A call made
// by the reconcile sweep carries the marker context and is let through.
type lostOutcomes struct {
	gitservice.ControlPlane
	dropNext *atomic.Int32
	lost     *sync.Map
	sweeps   *atomic.Int64
}

func (f lostOutcomes) Outcome(ctx context.Context, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error) {
	if ctx.Value(reconcileMarker{}) != nil {
		f.sweeps.Add(1)
		return f.ControlPlane.Outcome(ctx, pushID, req)
	}
	if _, already := f.lost.Load(req.ReceiveID); already {
		return gitv1.OutcomeResponse{}, &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "the control plane is down"}
	}
	if f.dropNext.Load() > 0 && f.dropNext.Add(-1) >= 0 {
		f.lost.Store(req.ReceiveID, true)
		return gitv1.OutcomeResponse{}, &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "the control plane is down"}
	}
	return f.ControlPlane.Outcome(ctx, pushID, req)
}

type gitEnv struct {
	t        *testing.T
	mem      *store.MemoryGit
	workers  *serve.GitWorkers
	server   *api.GitServer
	srv      *httptest.Server
	dir      string
	dropNext atomic.Int32
	lost     sync.Map
	sweeps   atomic.Int64
}

var tokens = map[string]api.Principal{"tok-eng": {UserID: "u_eng"}, "tok-viewer": {UserID: "u_viewer"}, "tok-admin": {UserID: "u_admin"}}

type authMap map[string]api.Principal

func (m authMap) Authenticate(_ context.Context, r *http.Request) (api.Principal, error) {
	if p, ok := m[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
		return p, nil
	}
	return api.Principal{}, api.ErrUnauthenticated
}

func newGitEnv(t *testing.T) *gitEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	e := &gitEnv{t: t, mem: store.NewMemoryGit(), dir: t.TempDir()}
	e.mem.AddTenant("t_acme", "acme")
	e.mem.AddProject("t_acme", "p_erp", "erp")
	e.mem.Grant("t_acme", "u_eng", "p_erp", "engineer")
	e.mem.Grant("t_acme", "u_viewer", "p_erp", "viewer")
	signer, err := serve.NewGitSigner("development", "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.workers, err = serve.NewGitWorkers(serve.GitWorkersConfig{Env: "development", StateDir: filepath.Join(e.dir, "state"), Verifier: signer.Verifier(), HookBinary: hookBinary(t),
		WrapControl: func(_ domain.TenantID, c gitservice.ControlPlane) gitservice.ControlPlane {
			return lostOutcomes{ControlPlane: c, dropNext: &e.dropNext, lost: &e.lost, sweeps: &e.sweeps}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.workers.Close() })
	e.server, err = api.NewGit(api.GitConfig{Store: e.mem, Auth: authMap(tokens), Workers: e.workers, Signer: signer, Identities: signer.Verifier()})
	if err != nil {
		t.Fatal(err)
	}
	e.workers.BindControl(e.server.Control())
	e.srv = httptest.NewServer(e.server.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *gitEnv) api(method, path, token string, body any, headers ...string) (int, map[string]any) {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func (e *gitEnv) createRepo(name string, policy map[string]any) map[string]any {
	e.t.Helper()
	code, repo := e.api(http.MethodPost, "/api/v1/tenants/acme/projects/p_erp/repositories", "tok-eng", map[string]any{"name": name, "policy": policy})
	if code != http.StatusCreated || repo["status"] != "ready" {
		e.t.Fatalf("create %s: %d %v", name, code, repo)
	}
	return repo
}

func (e *gitEnv) credential(token string, repo map[string]any, seed bool) (grant, password string, status int, body map[string]any) {
	e.t.Helper()
	status, body = e.api(http.MethodPost, "/api/v1/tenants/acme/git-credentials", token,
		map[string]any{"projectId": repo["projectId"], "repositoryId": repo["id"], "seed": seed})
	g, _ := body["grantId"].(string)
	p, _ := body["password"].(string)
	return g, p, status, body
}

func (e *gitEnv) cloneURL(repo map[string]any, password string) string {
	path, _ := repo["path"].(string)
	return strings.Replace(e.srv.URL, "http://", "http://kodama:"+password+"@", 1) + "/git/" + path + ".git"
}

// git runs the Git client in dir with no global configuration and no prompt.
func (e *gitEnv) git(dir string, args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "HOME="+e.dir,
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_AUTHOR_DATE=2026-10-30T10:00:00Z", "GIT_COMMITTER_DATE=2026-10-30T10:00:00Z")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (e *gitEnv) mustGit(dir string, args ...string) string {
	e.t.Helper()
	out, err := e.git(dir, args...)
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

// work creates a local repository with one commit and returns it and its commit.
func (e *gitEnv) work() (dir, commit string) {
	e.t.Helper()
	dir = filepath.Join(e.dir, "work-"+time.Now().Format("150405.000000"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.mustGit(dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.mustGit(dir, "add", ".")
	e.mustGit(dir, "commit", "-q", "-m", "first")
	return dir, e.mustGit(dir, "rev-parse", "HEAD")
}

func (e *gitEnv) refsEvents() []store.MemoryEvent { return e.mem.EventsOfType("git.refs_updated") }

type refsPayload struct {
	ReceiveID   string `json:"receiveId"`
	OriginRunID any    `json:"originRunId"`
	Updates     []struct{ Ref, Old, New, Kind string }
}

func payload(t *testing.T, ev store.MemoryEvent) refsPayload {
	t.Helper()
	var p refsPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

const zeroSHA = "0000000000000000000000000000000000000000"

// Test_unit_kernel_git_receive_event_once drives the real Git worker against a
// temporary bare repository: a seed bootstraps an empty repository, an authorized
// push applies and produces exactly one git.refs_updated event, and a refused
// push changes and records nothing.
func Test_unit_kernel_git_receive_event_once(t *testing.T) {
	e := newGitEnv(t)
	repo := e.createRepo("backend", nil)
	repoID := repo["id"].(string)
	work, seedCommit := e.work()

	// No refs yet: a seed grant is the only way in.
	_, plain, _, _ := e.credential("tok-eng", repo, false)
	if out, err := e.git(work, "push", e.cloneURL(repo, plain), "main:refs/heads/main"); err == nil || !strings.Contains(out, "refused") {
		t.Fatalf("an ordinary credential seeded the repository: %v\n%s", err, out)
	}
	if len(e.refsEvents()) != 0 {
		t.Fatal("a refused push recorded an event")
	}

	seedGrant, seedPW, status, body := e.credential("tok-eng", repo, true)
	if status != http.StatusCreated {
		t.Fatalf("seed credential: %d %v", status, body)
	}
	if out, err := e.git(work, "push", e.cloneURL(repo, seedPW), "main:refs/heads/main", "main:refs/heads/other"); err == nil || !strings.Contains(out, "seed_only") {
		t.Fatalf("a seed with an extra ref was accepted: %v\n%s", err, out)
	}
	if len(e.refsEvents()) != 0 {
		t.Fatal("a refused seed recorded an event")
	}
	if out, err := e.git(work, "push", e.cloneURL(repo, seedPW), "main:refs/heads/main"); err != nil {
		t.Fatalf("seed push: %v\n%s", err, out)
	}
	events := e.refsEvents()
	if len(events) != 1 {
		t.Fatalf("git.refs_updated events after the seed = %d", len(events))
	}
	p := payload(t, events[0])
	if events[0].ID != store.GitReceiveEventID(repoID, p.ReceiveID) || events[0].ActorType != "user" || events[0].ActorID != "u_eng" ||
		p.OriginRunID != nil || len(p.Updates) != 1 || p.Updates[0].Ref != "refs/heads/main" || p.Updates[0].Old != zeroSHA ||
		p.Updates[0].New != seedCommit || p.Updates[0].Kind != "create" {
		t.Fatalf("event = %s", events[0].Payload)
	}
	if grant, ok := gitv1.GrantOfReceive(p.ReceiveID); !ok || grant != seedGrant {
		t.Fatalf("the receive %s does not carry its grant %s", p.ReceiveID, seedGrant)
	}
	stored, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID)
	if stored.Empty || stored.JournalAck < 1 {
		t.Fatalf("repository after the seed = %+v: emptiness and the acknowledged journal sequence follow the applied receive", stored)
	}

	// The seed is single-use, and the repository is no longer seedable.
	e.mustGit(work, "commit", "-q", "--allow-empty", "-m", "second")
	if out, err := e.git(work, "push", e.cloneURL(repo, seedPW), "main:refs/heads/main"); err == nil || !strings.Contains(out, "seed_consumed") {
		t.Fatalf("a consumed seed was accepted again: %v\n%s", err, out)
	}
	if _, _, status, _ := e.credential("tok-eng", repo, true); status != http.StatusConflict {
		t.Fatalf("seed of a repository with refs = %d", status)
	}

	// An engineer pushes a branch: one more event, with the history intact.
	_, pw, _, _ := e.credential("tok-eng", repo, false)
	e.mustGit(work, "checkout", "-q", "-b", "feature/x")
	if err := os.WriteFile(filepath.Join(work, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustGit(work, "add", ".")
	e.mustGit(work, "commit", "-q", "-m", "x")
	featureCommit := e.mustGit(work, "rev-parse", "HEAD")
	if out, err := e.git(work, "push", e.cloneURL(repo, pw), "feature/x:refs/heads/feature/x", "feature/x:refs/tags/v1"); err != nil {
		t.Fatalf("branch push: %v\n%s", err, out)
	}
	events = e.refsEvents()
	if len(events) != 2 {
		t.Fatalf("events after the branch push = %d", len(events))
	}
	if q := payload(t, events[1]); len(q.Updates) != 2 {
		t.Fatalf("one receive is one event with both updates: %s", events[1].Payload)
	}
	// A fast-forward of the branch is classified from the worker's facts.
	if err := os.WriteFile(filepath.Join(work, "y.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustGit(work, "add", ".")
	e.mustGit(work, "commit", "-q", "-m", "y")
	if out, err := e.git(work, "push", e.cloneURL(repo, pw), "feature/x:refs/heads/feature/x"); err != nil {
		t.Fatalf("fast-forward push: %v\n%s", err, out)
	}
	events = e.refsEvents()
	if q := payload(t, events[2]); len(events) != 3 || len(q.Updates) != 1 || q.Updates[0].Kind != "fast_forward" || q.Updates[0].Old != featureCommit {
		t.Fatalf("fast-forward event = %v", events[len(events)-1].Payload)
	}

	// The default branch is protected: refused whole, nothing changes, no event.
	before := len(e.refsEvents())
	out, err := e.git(work, "push", e.cloneURL(repo, pw), "feature/x:refs/heads/main", "feature/x:refs/heads/ok")
	if err == nil || !strings.Contains(out, "ref_protected") {
		t.Fatalf("a direct push to the default branch: %v\n%s", err, out)
	}
	if len(e.refsEvents()) != before {
		t.Fatal("a refused receive recorded an event")
	}
	if ls := e.mustGit(work, "ls-remote", e.cloneURL(repo, pw)); strings.Contains(ls, "refs/heads/ok") || !strings.Contains(ls, seedCommit+"\trefs/heads/main") {
		t.Fatalf("ls-remote after the refused receive:\n%s", ls)
	}

	// A read-only person fetches the history but cannot push.
	_, viewerPW, status, _ := e.credential("tok-viewer", repo, false)
	if status != http.StatusCreated {
		t.Fatalf("viewer credential = %d", status)
	}
	clone := filepath.Join(e.dir, "clone")
	if out, err := e.git(e.dir, "clone", "-q", e.cloneURL(repo, viewerPW), clone); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if got := e.mustGit(clone, "rev-parse", "origin/main"); got != seedCommit {
		t.Fatalf("the seeded history changed: %s, want %s", got, seedCommit)
	}
	if out, err := e.git(clone, "push", "origin", "HEAD:refs/heads/viewer"); err == nil {
		t.Fatalf("a viewer pushed: %s", out)
	}

	// Archiving refuses pushes and keeps the history.
	if code, _ := e.api(http.MethodPost, "/api/v1/tenants/acme/projects/p_erp/repositories/"+repoID+"/archive", "tok-eng", nil); code != http.StatusOK {
		t.Fatalf("archive = %d", code)
	}
	if out, err := e.git(work, "push", e.cloneURL(repo, pw), "feature/x:refs/heads/after"); err == nil || !strings.Contains(out, "repository_archived") {
		t.Fatalf("a push to an archived repository: %v\n%s", err, out)
	}
	if _, err := e.git(e.dir, "ls-remote", e.cloneURL(repo, viewerPW)); err != nil {
		t.Fatalf("history of an archived repository: %v", err)
	}
	if len(e.refsEvents()) != before {
		t.Fatal("a refused push recorded an event")
	}
}

// Test_unit_kernel_git_journal_ack_reconcile: a worker that lost the outcome of a
// receive re-reports it on reconcile, which commits the event exactly once and
// acknowledges the journal sequence; a repeated sweep changes nothing.
func Test_unit_kernel_git_journal_ack_reconcile(t *testing.T) {
	e := newGitEnv(t)
	repo := e.createRepo("backend", nil)
	repoID := repo["id"].(string)
	work, seedCommit := e.work()
	_, seedPW, status, _ := e.credential("tok-eng", repo, true)
	if status != http.StatusCreated {
		t.Fatal(status)
	}

	e.dropNext.Store(1) // the confirmation never reaches the control plane
	if out, err := e.git(work, "push", e.cloneURL(repo, seedPW), "main:refs/heads/main"); err != nil {
		t.Fatalf("push with a lost confirmation: %v\n%s", err, out)
	}
	if len(e.refsEvents()) != 0 {
		t.Fatalf("an event was recorded although the outcome was lost: %d", len(e.refsEvents()))
	}
	lost, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID)
	if lost.JournalAck != 0 || !lost.Empty {
		t.Fatalf("repository while the outcome is unrecorded = %+v", lost)
	}
	// Until it is recorded, a new seed is refused: the earlier seed is unacknowledged.
	if _, _, status, body := e.credential("tok-eng", repo, true); status != http.StatusConflict || body["error"].(map[string]any)["code"] != "seed_pending" {
		t.Fatalf("seed while a receive is unacknowledged = %d %v", status, body)
	}
	assigned, err := e.server.Control().Assignments(t.Context(), "t_acme")
	if err != nil || len(assigned.Items) != 1 || assigned.Items[0].Kind != gitv1.AssignReconcile || *assigned.Items[0].JournalAck != 0 {
		t.Fatalf("assignments = %+v, %v", assigned, err)
	}

	sweep := func() {
		t.Helper()
		e.workers.Reconcile(context.WithValue(t.Context(), reconcileMarker{}, true), []domain.TenantID{"t_acme"})
	}
	sweep()
	events := e.refsEvents()
	if len(events) != 1 {
		t.Fatalf("events after the reconcile = %d", len(events))
	}
	p := payload(t, events[0])
	if len(p.Updates) != 1 || p.Updates[0].New != seedCommit || p.Updates[0].Kind != "create" || events[0].ActorID != "u_eng" {
		t.Fatalf("re-reported event = %s", events[0].Payload)
	}
	acked, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID)
	if acked.JournalAck < 1 || acked.Empty {
		t.Fatalf("repository after the reconcile = %+v: the journal is acknowledged", acked)
	}

	// A repeated sweep, and the worker's own background retry, add nothing.
	calls := e.sweeps.Load()
	sweep()
	sweep()
	if len(e.refsEvents()) != 1 {
		t.Fatalf("events after repeated sweeps = %d", len(e.refsEvents()))
	}
	if again, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID); again.JournalAck != acked.JournalAck {
		t.Fatalf("journal_ack moved from %d to %d without a new receive", acked.JournalAck, again.JournalAck)
	}
	if e.sweeps.Load() != calls {
		t.Fatalf("a settled window was reported again (%d calls)", e.sweeps.Load()-calls)
	}
	assigned, _ = e.server.Control().Assignments(t.Context(), "t_acme")
	if *assigned.Items[0].JournalAck != acked.JournalAck {
		t.Fatalf("the assignment carries ack %d, want %d", *assigned.Items[0].JournalAck, acked.JournalAck)
	}
	if _, _, status, _ := e.credential("tok-eng", repo, true); status != http.StatusConflict {
		t.Fatalf("seed after the reconcile = %d (the repository has refs)", status)
	}

	// Later receives are acknowledged live once nothing earlier is unreported, and
	// a lost outcome after them is recovered the same way.
	_, pw, _, _ := e.credential("tok-eng", repo, false)
	e.mustGit(work, "checkout", "-q", "-b", "next")
	if err := os.WriteFile(filepath.Join(work, "n.txt"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustGit(work, "add", ".")
	e.mustGit(work, "commit", "-q", "-m", "n")
	e.mustGit(work, "push", e.cloneURL(repo, pw), "next:refs/heads/next")
	live, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID)
	if len(e.refsEvents()) != 2 || live.JournalAck <= acked.JournalAck {
		t.Fatalf("live outcome: events %d, journal_ack %d (was %d)", len(e.refsEvents()), live.JournalAck, acked.JournalAck)
	}

	e.dropNext.Store(1)
	e.mustGit(work, "push", e.cloneURL(repo, pw), "next:refs/heads/next2")
	if len(e.refsEvents()) != 2 {
		t.Fatalf("events with a lost outcome = %d", len(e.refsEvents()))
	}
	if again, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID); again.JournalAck != live.JournalAck {
		t.Fatalf("journal_ack moved to %d with an unrecorded receive", again.JournalAck)
	}
	// A receive after the lost one must not acknowledge past it.
	e.mustGit(work, "push", e.cloneURL(repo, pw), "next:refs/heads/next3")
	if len(e.refsEvents()) != 3 {
		t.Fatalf("events = %d", len(e.refsEvents()))
	}
	mid, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID)
	if mid.JournalAck != live.JournalAck {
		t.Fatalf("journal_ack %d skipped the unrecorded receive (it was %d)", mid.JournalAck, live.JournalAck)
	}
	sweep()
	if len(e.refsEvents()) != 4 {
		t.Fatalf("events after the second reconcile = %d", len(e.refsEvents()))
	}
	end, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID)
	if end.JournalAck <= mid.JournalAck {
		t.Fatalf("journal_ack %d did not advance past the recovered receive", end.JournalAck)
	}
	ids := map[string]bool{}
	for _, ev := range e.refsEvents() {
		if ids[ev.ID] {
			t.Fatalf("duplicate event %s", ev.ID)
		}
		ids[ev.ID] = true
	}
}

// The production refusal and the worker hosting modes.
func TestGitWorkersHostingModes(t *testing.T) {
	signer, err := serve.NewGitSigner("development", "", "")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	prod, err := serve.NewGitWorkers(serve.GitWorkersConfig{Env: "production", StateDir: state, Verifier: signer.Verifier()})
	if !errors.Is(err, serve.ErrInProcessInProduction) {
		t.Fatalf("production without a worker address: %v", err)
	}
	if _, herr := prod.GitHandler(t.Context(), "t_acme"); !errors.Is(herr, serve.ErrInProcessInProduction) {
		t.Fatalf("a worker was started in production: %v", herr)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatal("production created a data root")
	}
	remote, err := serve.NewGitWorkers(serve.GitWorkersConfig{Env: "production", WorkerURL: "https://git-{tenantId}.tenants.svc:8443", ControlToken: "secret"})
	if err != nil {
		t.Fatalf("a remote worker address in production: %v", err)
	}
	if h, err := remote.GitHandler(t.Context(), "t_acme"); err != nil || h == nil {
		t.Fatalf("remote handler: %v", err)
	}
	if _, err := serve.NewGitWorkers(serve.GitWorkersConfig{Env: "production", WorkerURL: "https://git-{tenantId}.example.invalid"}); err == nil {
		t.Fatal("a remote worker without the control token was accepted")
	}
	if _, err := serve.NewGitWorkers(serve.GitWorkersConfig{Env: "production", WorkerURL: "https://git.example.invalid", ControlToken: "secret"}); err == nil {
		t.Fatal("one worker address shared by every tenant was accepted")
	}
	if _, err := serve.NewGitSigner("production", "", ""); err == nil {
		t.Fatal("production generated ephemeral signing keys")
	}
}

// Test_unit_kernel_git_default_branch_change_moves_head: changing the default
// branch of an empty repository puts it back into provisioning, the worker's
// idempotent initialize re-applies HEAD, and after a seed an ordinary clone checks
// out the new branch.
func Test_unit_kernel_git_default_branch_change_moves_head(t *testing.T) {
	e := newGitEnv(t)
	repo := e.createRepo("backend", nil)
	repoID := repo["id"].(string)
	path := "/api/v1/tenants/acme/projects/p_erp/repositories/" + repoID

	code, patched := e.api(http.MethodPatch, path, "tok-eng", map[string]any{"defaultBranch": "trunk"}, "If-Match", "2")
	if code != http.StatusOK || patched["status"] != "ready" || patched["defaultBranch"] != "trunk" {
		t.Fatalf("patch = %d %v: the worker applies the change within the request", code, patched)
	}
	head, err := os.ReadFile(filepath.Join(e.dir, "state", "git", "t_acme", "repositories", repoID+".git", "HEAD"))
	if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/trunk" {
		t.Fatalf("HEAD = %q, %v", head, err)
	}

	work, commit := e.work()
	_, seedPW, status, body := e.credential("tok-eng", patched, true)
	if status != http.StatusCreated {
		t.Fatalf("seed = %d %v", status, body)
	}
	if out, err := e.git(work, "push", e.cloneURL(patched, seedPW), "main:refs/heads/main"); err == nil {
		t.Fatalf("the old default branch was seeded: %s", out)
	}
	if out, err := e.git(work, "push", e.cloneURL(patched, seedPW), "main:refs/heads/trunk"); err != nil {
		t.Fatalf("seed of the new default branch: %v\n%s", err, out)
	}
	_, pw, _, _ := e.credential("tok-viewer", patched, false)
	clone := filepath.Join(e.dir, "clone")
	if out, err := e.git(e.dir, "clone", "-q", e.cloneURL(patched, pw), clone); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if branch := e.mustGit(clone, "symbolic-ref", "--short", "HEAD"); branch != "trunk" {
		t.Fatalf("the clone checked out %q", branch)
	}
	if got := e.mustGit(clone, "rev-parse", "HEAD"); got != commit {
		t.Fatalf("the clone's HEAD is %s, want %s", got, commit)
	}
	if _, err := os.Stat(filepath.Join(clone, "README.md")); err != nil {
		t.Fatalf("nothing was checked out: %v", err)
	}
	// With refs the default branch no longer changes.
	if code, _ := e.api(http.MethodPatch, path, "tok-eng", map[string]any{"defaultBranch": "main"}, "If-Match", "4"); code != http.StatusConflict {
		t.Fatalf("patch of a repository with refs = %d", code)
	}
}

// A receive whose outcome was lost is recovered even when the repository was
// archived before the worker re-reported it: an archive refuses pushes, not
// recovery.
func Test_unit_kernel_git_archive_before_recovery(t *testing.T) {
	e := newGitEnv(t)
	repo := e.createRepo("backend", nil)
	repoID := repo["id"].(string)
	work, seedCommit := e.work()
	_, seedPW, _, _ := e.credential("tok-eng", repo, true)
	e.dropNext.Store(1)
	if out, err := e.git(work, "push", e.cloneURL(repo, seedPW), "main:refs/heads/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if len(e.refsEvents()) != 0 {
		t.Fatal("the outcome was not lost")
	}
	if code, _ := e.api(http.MethodPost, "/api/v1/tenants/acme/projects/p_erp/repositories/"+repoID+"/archive", "tok-eng", nil); code != http.StatusOK {
		t.Fatalf("archive = %d", code)
	}
	e.workers.Reconcile(context.WithValue(t.Context(), reconcileMarker{}, true), []domain.TenantID{"t_acme"})
	events := e.refsEvents()
	if len(events) != 1 || payload(t, events[0]).Updates[0].New != seedCommit {
		t.Fatalf("events after the recovery of an archived repository = %d", len(events))
	}
	if stored, _ := e.mem.GetRepository(t.Context(), "t_acme", repoID); stored.Status != store.RepoArchived || stored.JournalAck < 1 || stored.Empty {
		t.Fatalf("repository = %+v", stored)
	}
}

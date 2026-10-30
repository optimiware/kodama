package entrypoint

// Unit qualification of `kodama prepare` (T-113): the one-shot, controller-selected
// checkout of a run_shared run. A real HTTPS Git server with Basic credentials, a
// real Git and a scripted resource port; no cluster, no container.

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
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

// scriptRes is a resourceRuntime whose operations can be replaced; all calls are
// recorded.
type scriptRes struct {
	mu sync.Mutex

	ctx     runtimev2.ResourceContext
	admit   func(n int, req runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error)
	fetch   func() (runtimev2.ResourceContext, error)
	renew   func() (runtimev2.ResourceTokenResponse, error)
	resolve func(req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error)
	done    func(n int, req runtimev2.PreparationReceiptRequest) (runtimev2.PreparationReceiptResponse, error)
	// onDispose runs when the port's credentials are dropped.
	onDispose func()

	calls    []string
	admits   []runtimev2.ResourceAdmitRequest
	resolved []runtimev1.SecretResolveRequest
	receipts []runtimev2.PreparationReceiptRequest
}

func (s *scriptRes) rec(op string) { s.calls = append(s.calls, op) }

func (s *scriptRes) count(op string) (n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == op {
			n++
		}
	}
	return
}

func resourceToken(ttl, max time.Duration) runtimev2.ResourceTokenInfo {
	return runtimev2.ResourceTokenInfo{Token: "rt_unit_resource_token_0123456789abcdef", TokenType: "Bearer", Audience: runtimev2.ResourceAudience,
		ExpiresAt: timestamp(ttl), MaxExpiresAt: timestamp(max)}
}

// token is a token that lives half an hour and ends at the resource's deadline.
func (s *scriptRes) token() runtimev2.ResourceTokenInfo {
	t := resourceToken(30*time.Minute, time.Hour)
	switch {
	case s.ctx.Preparation != nil:
		t.MaxExpiresAt = s.ctx.Preparation.DeadlineAt
	case s.ctx.Service != nil:
		t.MaxExpiresAt = s.ctx.Service.DeadlineAt
	}
	return t
}

// Dispose records that the port's credentials were dropped.
func (s *scriptRes) Dispose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("dispose")
	if s.onDispose != nil {
		s.onDispose()
	}
}

func (s *scriptRes) AdmitResource(_ context.Context, req runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("admit")
	s.admits = append(s.admits, req)
	if s.admit != nil {
		return s.admit(len(s.admits), req)
	}
	return runtimev2.ResourceAdmitResponse{Token: s.token(), Context: s.ctx}, nil
}

func (s *scriptRes) RenewResourceToken(context.Context) (runtimev2.ResourceTokenResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("renew")
	if s.renew != nil {
		return s.renew()
	}
	return runtimev2.ResourceTokenResponse{Token: s.token()}, nil
}

func (s *scriptRes) ResourceContext(context.Context) (runtimev2.ResourceContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("getContext")
	if s.fetch != nil {
		return s.fetch()
	}
	return s.ctx, nil
}

func (s *scriptRes) ResolveSecret(_ context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("resolveSecret")
	s.resolved = append(s.resolved, req)
	if s.resolve != nil {
		return s.resolve(req)
	}
	return runtimev1.SecretResolveResponse{}, &APIError{Op: "resolveResourceSecret", Status: 403, Code: domain.CodeForbidden}
}

func (s *scriptRes) CompletePreparation(_ context.Context, req runtimev2.PreparationReceiptRequest) (runtimev2.PreparationReceiptResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("completePreparation")
	s.receipts = append(s.receipts, req)
	if s.done != nil {
		return s.done(len(s.receipts), req)
	}
	return runtimev2.PreparationReceiptResponse{ReceiptDigest: runtimev1.Digest("sha256:" + strings.Repeat("e", 64)), RecordedAt: timestamp(0)}, nil
}

func resErr(op string, status int, code domain.ErrorCode) error {
	return &APIError{Op: op, Status: status, Code: code, Message: string(code)}
}

// prepServer is the HTTPS Git server of the preparation tests: git-http-backend
// behind Basic credentials, with a request hook a test installs before the
// entrypoint runs (the server's handler is never replaced while it serves).
type prepServer struct {
	*httptest.Server
	CAFile string
	mu     sync.Mutex
	auths  []string
	paths  []string
	wrap   func(w http.ResponseWriter, r *http.Request, next http.Handler)
}

func newPrepServer(t *testing.T, root, user, pass string) *prepServer {
	t.Helper()
	backend := filepath.Join(gitRun(t, ".", "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not available")
	}
	ps := &prepServer{}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	ps.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		ps.mu.Lock()
		ps.auths = append(ps.auths, r.Header.Get("Authorization"))
		ps.paths = append(ps.paths, r.URL.Path)
		wrap := ps.wrap
		ps.mu.Unlock()
		if user != "" && (!ok || u != user || p != pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if wrap != nil {
			wrap(w, r, h)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ps.Close)
	ps.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ps.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ps.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return ps
}

func (ps *prepServer) setWrap(fn func(w http.ResponseWriter, r *http.Request, next http.Handler)) {
	ps.mu.Lock()
	ps.wrap = fn
	ps.mu.Unlock()
}

// ---- the preparation fixture ---------------------------------------------------------------

type prepUnit struct {
	t      *testing.T
	root   string
	rt     *scriptRes
	cfg    ResourceConfig
	logs   *safeBuf
	gs     *prepServer
	first  string
	second string
	url    string
	cred   string
}

const prepToken = "ghp_prepare_checkout_token_01"

func newPrepUnit(t *testing.T, mutate func(root string)) *prepUnit {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	repos := filepath.Join(root, "repos")
	if err := os.MkdirAll(repos, 0o755); err != nil {
		t.Fatal(err)
	}
	first, second := seedRepo(t, repos)
	if mutate != nil {
		mutate(repos)
	}
	gs := newPrepServer(t, repos, "reader", prepToken)
	u := &prepUnit{t: t, root: root, rt: &scriptRes{}, logs: &safeBuf{}, gs: gs, first: first, second: second, url: gs.URL + "/repo.git"}
	for _, d := range []string{"workspace", "scratch", "identity"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	u.cred = filepath.Join(root, "identity", "checkout-credential")
	writeFile(t, u.cred, "reader:"+prepToken+"\n")
	writeFile(t, filepath.Join(root, "identity", "admission"), unitAdmission+"\n")
	u.rt.ctx = u.context(first, 2, 60, time.Hour)
	podName, podUID := "kr-0123456789abcdef01234567-x7k2p", "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"
	u.cfg = ResourceConfig{
		ServerURL: "https://runtime.example.invalid", RunID: "run_01", Key: "preparation",
		AdmissionCredentialFile: filepath.Join(root, "identity", "admission"),
		Identity:                runtimev2.ResourceIdentity{WorkloadName: "kr-0123456789abcdef01234567", WorkloadUID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", PodName: &podName, PodUID: &podUID},
		WorkspaceDir:            filepath.Join(root, "workspace"), CheckoutCredentialFile: u.cred, GitCAFile: gs.CAFile, ScratchDir: filepath.Join(root, "scratch"),
		AdmissionWait: 5 * time.Second,
		Logger:        slog.New(slog.NewJSONHandler(u.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return u
}

func (u *prepUnit) context(commit string, attempts, timeoutSeconds int64, deadline time.Duration) runtimev2.ResourceContext {
	return runtimev2.ResourceContext{
		ContextVersion: 2, ResourceKind: runtimev2.ResourcePreparation, TenantID: "t_acme", ProjectID: "p_inventory", RunID: "run_01", ResourceKey: "preparation",
		Preparation: &runtimev2.PreparationContext{
			RepositoryURL: u.url, Commit: runtimev1.GitCommit(commit), MountPath: "/workspace", TimeoutSeconds: timeoutSeconds, MaxBytes: 1 << 30, MaxAttempts: attempts, DeadlineAt: timestamp(deadline),
		},
		Policy: runtimev2.PolicyPin{ID: "run-v1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("f", 64))},
	}
}

func (u *prepUnit) run() int {
	u.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return newPreparation(u.cfg, u.rt).run(ctx)
}

func (u *prepUnit) wantExit(got, want int) {
	u.t.Helper()
	if got != want {
		u.t.Fatalf("exit = %d, want %d\ncalls: %v\nlogs:\n%s", got, want, u.rt.calls, u.logs.String())
	}
}

func (u *prepUnit) head() string { return tgit(u.t, u.cfg.WorkspaceDir, "rev-parse", "HEAD") }

func (u *prepUnit) fetches() int {
	u.gs.mu.Lock()
	defer u.gs.mu.Unlock()
	return len(u.gs.auths)
}

// ---- tests ----------------------------------------------------------------------------------------

func Test_unit_prepare_checks_out_once_and_records_receipt(t *testing.T) {
	t.Run("one admission, one checkout, one receipt that describes the tree", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		u.wantExit(u.run(), ExitOK)
		if got := u.head(); got != u.first {
			t.Fatalf("HEAD %s, want the pinned %s (the branch moved on to %s)", got, u.first, u.second)
		}
		if b, _ := os.ReadFile(filepath.Join(u.cfg.WorkspaceDir, "a.txt")); string(b) != "one" {
			t.Fatalf("the pinned tree was not checked out: %q", b)
		}
		if len(u.rt.admits) != 1 || u.rt.count("completePreparation") != 1 {
			t.Fatalf("calls %v", u.rt.calls)
		}
		for _, c := range u.rt.calls {
			if c != "admit" && c != "getContext" && c != "completePreparation" && c != "renew" && c != "dispose" {
				t.Fatalf("a preparation called %s", c)
			}
		}
		r := u.rt.receipts[0]
		tree := tgit(t, u.cfg.WorkspaceDir, "rev-parse", "HEAD^{tree}")
		if string(r.Commit) != u.first || string(r.TreeID) != tree || !r.HooksDisabled || r.Submodules != "skipped" || !r.CredentialsRemoved || r.Attempts != 1 {
			t.Fatalf("receipt %+v (tree %s)", r, tree)
		}
		if err := runtimev2.CheckReceipt(u.rt.ctx, r); err != nil {
			t.Fatalf("the receipt is not valid for the assignment: %v", err)
		}
		// The admission carried the identity the workload observed, the credential and a nonce.
		a := u.rt.admits[0]
		if a.AdmissionCredential != unitAdmission || a.Identity.WorkloadName != u.cfg.Identity.WorkloadName || a.Identity.PodUID == nil || a.Nonce == "" || a.Validate() != nil {
			t.Fatalf("admit request %+v", a)
		}
		// The credential reached the server only as an HTTP header of Git, and
		// nowhere in the workspace, the logs or the command line.
		if u.fetches() == 0 {
			t.Fatal("the repository server saw no request")
		}
		for name, hay := range map[string]string{"logs": u.logs.String()} {
			if strings.Contains(hay, prepToken) {
				t.Fatalf("the credential is in the %s", name)
			}
		}
		cfgBytes, _ := os.ReadFile(filepath.Join(u.cfg.WorkspaceDir, ".git", "config"))
		for _, bad := range []string{"extraheader", "http.", "remote", "credential", "url"} {
			if strings.Contains(strings.ToLower(string(cfgBytes)), bad) {
				t.Fatalf(".git/config keeps %q:\n%s", bad, cfgBytes)
			}
		}
		if _, err := os.Stat(u.cred); err == nil {
			t.Fatal("the credential file was left in place")
		}
	})

	t.Run("never over existing content, never twice", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		u.wantExit(u.run(), ExitOK)
		writeFile(t, u.cred, "reader:"+prepToken+"\n")
		before := dirSnapshot(t, u.cfg.WorkspaceDir)
		u.wantExit(u.run(), ExitCheckout)
		if u.rt.count("completePreparation") != 1 {
			t.Fatalf("a second receipt was attempted: %v", u.rt.calls)
		}
		if after := dirSnapshot(t, u.cfg.WorkspaceDir); after != before {
			t.Fatal("a second run changed the prepared workspace")
		}
		// A fresh volume's lost+found is the only thing allowed to exist.
		u2 := newPrepUnit(t, nil)
		if err := os.Mkdir(filepath.Join(u2.cfg.WorkspaceDir, "lost+found"), 0o700); err != nil {
			t.Fatal(err)
		}
		u2.wantExit(u2.run(), ExitOK)
	})

	t.Run("the receipt is recorded only after the tree is verified", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		u.rt.ctx = u.context(strings.Repeat("1", 40), 2, 30, time.Hour) // a commit the repository does not have
		u.wantExit(u.run(), ExitCheckout)
		if u.rt.count("completePreparation") != 0 {
			t.Fatal("a receipt was recorded for a failed checkout")
		}
	})

	t.Run("admission refusals do nothing to the workspace", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			want int
		}{
			{"revoked", resErr("admitResource", 403, runtimev2.CodeResourceRevoked), ExitRevoked},
			{"foreign identity", resErr("admitResource", 403, domain.CodeForbidden), ExitAdmission},
			{"consumed", resErr("admitResource", 409, runtimev1.CodeAdmissionConsumed), ExitAdmission},
			{"unsupported client", resErr("admitResource", 422, runtimev1.CodeProfileUnsupported), ExitAdmission},
			{"contract violation", &ProtocolError{Op: "admitResource", Err: errors.New("bad body")}, ExitAdmission},
		} {
			t.Run(tc.name, func(t *testing.T) {
				u := newPrepUnit(t, nil)
				u.rt.admit = func(int, runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
					return runtimev2.ResourceAdmitResponse{}, tc.err
				}
				u.wantExit(u.run(), tc.want)
				if entries, _ := os.ReadDir(u.cfg.WorkspaceDir); len(entries) != 0 || u.fetches() != 0 {
					t.Fatalf("a refused admission still touched the workspace (%d entries) or the repository (%d requests)", len(entries), u.fetches())
				}
			})
		}
	})

	t.Run("an unbound resource is retried with the same nonce", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		u.rt.admit = func(n int, _ runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
			if n < 3 {
				return runtimev2.ResourceAdmitResponse{}, resErr("admitResource", 409, runtimev2.CodeResourceUnbound)
			}
			return runtimev2.ResourceAdmitResponse{Token: u.rt.token(), Context: u.rt.ctx}, nil
		}
		u.wantExit(u.run(), ExitOK)
		if len(u.rt.admits) != 3 || u.rt.admits[0].Nonce != u.rt.admits[1].Nonce || u.rt.admits[1].Nonce != u.rt.admits[2].Nonce {
			t.Fatalf("admissions %+v", u.rt.admits)
		}
	})

	t.Run("a context that is another resource's is never acted on", func(t *testing.T) {
		for name, mutate := range map[string]func(c *runtimev2.ResourceContext){
			"another key": func(c *runtimev2.ResourceContext) {
				c.ResourceKey = "service.db"
				c.ResourceKind = runtimev2.ResourceService
			},
			"another run":      func(c *runtimev2.ResourceContext) { c.RunID = "run_other" },
			"a plain http URL": func(c *runtimev2.ResourceContext) { c.Preparation.RepositoryURL = "http://git.example.invalid/r.git" },
			"a file URL":       func(c *runtimev2.ResourceContext) { c.Preparation.RepositoryURL = "file:///etc" },
			"a URL with a credential": func(c *runtimev2.ResourceContext) {
				c.Preparation.RepositoryURL = "https://bot:token@git.example.invalid/r.git"
			},
		} {
			t.Run(name, func(t *testing.T) {
				u := newPrepUnit(t, nil)
				c := u.rt.ctx
				p := *c.Preparation
				c.Preparation = &p
				mutate(&c)
				u.rt.ctx = c
				if code := u.run(); code == ExitOK {
					t.Fatalf("exit %d", code)
				}
				if u.fetches() != 0 || u.rt.count("completePreparation") != 0 {
					t.Fatal("work was done for a refused assignment")
				}
			})
		}
		u := newPrepUnit(t, nil)
		other := u.rt.ctx
		p := *other.Preparation
		p.Commit = runtimev1.GitCommit(u.second)
		other.Preparation = &p
		u.rt.fetch = func() (runtimev2.ResourceContext, error) { return other, nil }
		u.wantExit(u.run(), ExitAdmission)
		if u.fetches() != 0 {
			t.Fatal("a checkout followed an inconsistent context")
		}
	})

	t.Run("receipt outcomes", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			want int
		}{
			{"conflict with a recorded receipt", resErr("completePreparation", 409, runtimev2.CodeReceiptConflict), ExitCheckout},
			{"revoked by a stop", resErr("completePreparation", 403, runtimev2.CodeResourceRevoked), ExitRevoked},
			{"unknown outcome", &TransportError{Op: "completePreparation", Err: errors.New("connection reset")}, ExitFinalization},
		} {
			t.Run(tc.name, func(t *testing.T) {
				u := newPrepUnit(t, nil)
				u.rt.done = func(int, runtimev2.PreparationReceiptRequest) (runtimev2.PreparationReceiptResponse, error) {
					return runtimev2.PreparationReceiptResponse{}, tc.err
				}
				u.wantExit(u.run(), tc.want)
			})
		}
	})
}

// flaky makes the first failFirst requests of the Git server fail, calling hook
// with the request count.
func flaky(gs *prepServer, failFirst int, hook func(n int)) {
	var mu sync.Mutex
	n := 0
	gs.setWrap(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if hook != nil {
			hook(cur)
		}
		if cur <= failFirst {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func Test_unit_prepare_retries_inside_one_workload(t *testing.T) {
	t.Run("a failed try is cleaned and retried; the receipt counts the tries", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		// The first try leaves a stray file behind (as a half-done checkout would).
		flaky(u.gs, 1, func(n int) {
			if n == 1 {
				writeFile(t, filepath.Join(u.cfg.WorkspaceDir, "partial.tmp"), "half a checkout")
			}
		})
		u.wantExit(u.run(), ExitOK)
		if _, err := os.Stat(filepath.Join(u.cfg.WorkspaceDir, "partial.tmp")); err == nil {
			t.Fatal("the partial tree of the failed try was kept")
		}
		if got := u.head(); got != u.first {
			t.Fatalf("HEAD %s", got)
		}
		if r := u.rt.receipts[0]; r.Attempts != 2 {
			t.Fatalf("attempts = %d, want the 2 tries this workload used", r.Attempts)
		}
		// One workload: one admission and one receipt, however many tries.
		if len(u.rt.admits) != 1 || u.rt.count("completePreparation") != 1 {
			t.Fatalf("calls %v", u.rt.calls)
		}
		if status := tgit(t, u.cfg.WorkspaceDir, "status", "--porcelain"); status != "" {
			t.Fatalf("the prepared tree is not clean: %q", status)
		}
	})

	t.Run("the tries are bounded by maxAttempts and end without a receipt", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		u.rt.ctx = u.context(u.first, 2, 60, time.Hour)
		flaky(u.gs, 1<<30, nil)
		u.wantExit(u.run(), ExitCheckout)
		if u.rt.count("completePreparation") != 0 {
			t.Fatal("a receipt was recorded after exhausted tries")
		}
		if len(u.rt.admits) != 1 {
			t.Fatalf("a retry re-admitted: %d admissions", len(u.rt.admits))
		}
		tries := strings.Count(u.logs.String(), `"msg":"checkout try failed"`)
		if tries != 2 {
			t.Fatalf("%d tries, want exactly maxAttempts=2\n%s", tries, u.logs.String())
		}
		// maxAttempts of 1 is exactly one try.
		u1 := newPrepUnit(t, nil)
		u1.rt.ctx = u1.context(u1.first, 1, 60, time.Hour)
		flaky(u1.gs, 1<<30, nil)
		u1.wantExit(u1.run(), ExitCheckout)
		if n := strings.Count(u1.logs.String(), `"msg":"checkout try failed"`); n != 1 {
			t.Fatalf("%d tries with maxAttempts=1", n)
		}
	})

	t.Run("one timeout covers every try", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		u.rt.ctx = u.context(u.first, 10, 2, time.Hour) // ten tries allowed, two seconds in all
		flaky(u.gs, 1<<30, func(int) { time.Sleep(700 * time.Millisecond) })
		start := time.Now()
		u.wantExit(u.run(), ExitCheckout)
		if time.Since(start) > 20*time.Second {
			t.Fatalf("the tries outlived the timeout: %s", time.Since(start))
		}
		if n := strings.Count(u.logs.String(), `"msg":"checkout try failed"`); n >= 10 {
			t.Fatalf("%d tries inside a 2s budget", n)
		}
	})

	t.Run("a passed deadline allows no try at all", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		u.rt.ctx = u.context(u.first, 2, 60, 2*time.Second)
		u.rt.ctx.Preparation.DeadlineAt = timestamp(-time.Second)
		u.rt.admit = func(int, runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
			return runtimev2.ResourceAdmitResponse{Token: resourceToken(time.Hour, 2*time.Hour), Context: u.rt.ctx}, nil
		}
		// A response whose token outlives the deadline is not even a valid admission.
		if code := u.run(); code == ExitOK || u.fetches() != 0 {
			t.Fatalf("exit %d with %d repository requests", code, u.fetches())
		}
	})

	t.Run("a stop during the tries ends them", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		u.rt.renew = func() (runtimev2.ResourceTokenResponse, error) {
			return runtimev2.ResourceTokenResponse{}, resErr("renewResourceToken", 403, runtimev2.CodeResourceRevoked)
		}
		u.rt.admit = func(int, runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
			t := u.rt.token()
			t.ExpiresAt = timestamp(1100 * time.Millisecond)
			return runtimev2.ResourceAdmitResponse{Token: t, Context: u.rt.ctx}, nil
		}
		flaky(u.gs, 1<<30, func(int) { time.Sleep(1500 * time.Millisecond) })
		u.rt.ctx = u.context(u.first, 5, 60, time.Hour)
		if code := u.run(); code != ExitRevoked {
			t.Fatalf("exit %d\n%s", code, u.logs.String())
		}
		if u.rt.count("completePreparation") != 0 {
			t.Fatal("a receipt followed a revocation")
		}
	})
}

func Test_unit_prepare_hardening(t *testing.T) {
	t.Run("nothing in the repository or the environment runs", func(t *testing.T) {
		sentinel := filepath.Join(t.TempDir(), "ran")
		var head string
		u := newPrepUnit(t, func(repos string) {
			// A hostile repository: attribute filters, a submodule at another URL, and a
			// hook in the server's bare repository.
			work := t.TempDir()
			tgit(t, work, "init", "-q", "-b", "main")
			writeFile(t, filepath.Join(work, "a.txt"), "content\n")
			writeFile(t, filepath.Join(work, ".gitattributes"), "* filter=evil\n*.txt text eol=crlf\n")
			writeFile(t, filepath.Join(work, ".gitmodules"), "[submodule \"lib\"]\n\tpath = lib\n\turl = https://127.0.0.1:1/other.git\n")
			tgit(t, work, "add", ".")
			tgit(t, work, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",lib")
			tgit(t, work, "commit", "-q", "--no-gpg-sign", "-m", "hostile")
			head = tgit(t, work, "rev-parse", "HEAD")
			bare := filepath.Join(repos, "hostile.git")
			tgit(t, ".", "clone", "-q", "--bare", work, bare)
			tgit(t, bare, "config", "http.uploadpack", "true")
			tgit(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
			writeFile(t, filepath.Join(bare, "hooks", "post-receive"), "#!/bin/sh\ntouch "+sentinel+"\n")
			_ = os.Chmod(filepath.Join(bare, "hooks", "post-receive"), 0o755)
		})
		u.url = u.gs.URL + "/hostile.git"
		u.rt.ctx = u.context(head, 1, 60, time.Hour)
		// A hostile ambient configuration: global and system Git config and the
		// scratch HOME define filters and credential helpers that would run.
		hostile := fmt.Sprintf("[filter \"evil\"]\n\tsmudge = sh -c 'touch %s; cat'\n\tclean = sh -c 'touch %s; cat'\n\trequired = true\n[credential]\n\thelper = !sh -c 'touch %s'\n[core]\n\thooksPath = %s\n", sentinel, sentinel, sentinel, filepath.Join(u.root, "hooks"))
		writeFile(t, filepath.Join(u.cfg.ScratchDir, ".gitconfig"), hostile)
		writeFile(t, filepath.Join(u.cfg.ScratchDir, ".config", "git", "config"), hostile)
		writeFile(t, filepath.Join(u.root, "hooks", "post-checkout"), "#!/bin/sh\ntouch "+sentinel+"\n")
		_ = os.Chmod(filepath.Join(u.root, "hooks", "post-checkout"), 0o755)
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(u.cfg.ScratchDir, ".gitconfig"))
		t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(u.cfg.ScratchDir, ".gitconfig"))
		t.Setenv("GIT_DIR", filepath.Join(u.root, "elsewhere"))
		t.Setenv("GIT_EXEC_PATH", filepath.Join(u.root, "no-such-exec-path"))
		t.Setenv("GIT_SSH_COMMAND", "touch "+sentinel)

		u.wantExit(u.run(), ExitOK)
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatal("something ran: a filter, a hook, a credential helper or a transport")
		}
		if got := u.head(); got != head {
			t.Fatalf("HEAD %s", got)
		}
		// The attribute filter did not touch the bytes (no CRLF conversion either: no
		// attribute-driven conversion beyond what the content says is configured
		// here) and the gitlink is an empty directory: the submodule was not fetched.
		if b, _ := os.ReadFile(filepath.Join(u.cfg.WorkspaceDir, "a.txt")); string(b) == "" {
			t.Fatal("the file was emptied by a filter")
		}
		if entries, _ := os.ReadDir(filepath.Join(u.cfg.WorkspaceDir, "lib")); len(entries) != 0 {
			t.Fatalf("the submodule was populated: %v", entries)
		}
		u.gs.mu.Lock()
		for _, p := range u.gs.paths {
			if !strings.HasPrefix(p, "/hostile.git/") {
				t.Fatalf("a request left the pinned repository: %s", p)
			}
		}
		u.gs.mu.Unlock()
		if r := u.rt.receipts[0]; r.Submodules != "skipped" || !r.HooksDisabled {
			t.Fatalf("receipt %+v", r)
		}
	})

	t.Run("the credential never goes to another host, even through a redirect", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		var seen []string
		var mu sync.Mutex
		other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Header.Get("Authorization"))
			mu.Unlock()
			http.Error(w, "no", http.StatusForbidden)
		}))
		t.Cleanup(other.Close)
		u.gs.setWrap(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if strings.HasPrefix(r.URL.Path, "/repo.git/info/refs") {
				http.Redirect(w, r, other.URL+"/steal.git/info/refs?service=git-upload-pack", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
		})
		u.rt.ctx = u.context(u.first, 1, 30, time.Hour)
		u.wantExit(u.run(), ExitCheckout)
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 0 {
			t.Fatalf("the redirect was followed to another host with %v", seen)
		}
		if strings.Contains(u.logs.String(), prepToken) {
			t.Fatal("the credential is in the logs")
		}
	})

	t.Run("a failed fetch never echoes the credential", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		writeFile(t, u.cred, "reader:wrong-password-value\n")
		u.rt.ctx = u.context(u.first, 1, 30, time.Hour)
		u.wantExit(u.run(), ExitCheckout)
		if strings.Contains(u.logs.String(), "wrong-password-value") {
			t.Fatalf("the credential leaked into the logs:\n%s", u.logs.String())
		}
	})

	t.Run("a credential or its configuration left in the workspace blocks the receipt", func(t *testing.T) {
		u := newPrepUnit(t, nil)
		p := newPreparation(u.cfg, u.rt)
		tgit(t, u.cfg.WorkspaceDir, "init", "-q")
		cred := &checkoutCredential{username: "reader", password: prepToken}
		forms := cred.forms()
		if !p.removeCredential(cred) {
			t.Fatalf("a clean repository was refused:\n%s", u.logs.String())
		}
		for name, leak := range map[string]func(){
			"extra header in config": func() {
				writeFile(t, filepath.Join(u.cfg.WorkspaceDir, ".git", "config"), "[http \"https://x\"]\n\textraHeader = Authorization: Basic abc\n")
			},
			"credential helper in config": func() {
				writeFile(t, filepath.Join(u.cfg.WorkspaceDir, ".git", "config"), "[credential]\n\thelper = store\n")
			},
			"password in FETCH_HEAD": func() {
				writeFile(t, filepath.Join(u.cfg.WorkspaceDir, ".git", "FETCH_HEAD"), "https://reader:"+prepToken+"@host/r.git\n")
			},
			"base64 form in a ref": func() {
				writeFile(t, filepath.Join(u.cfg.WorkspaceDir, ".git", "refs", "x"), forms[2])
			},
		} {
			_ = os.RemoveAll(filepath.Join(u.cfg.WorkspaceDir, ".git"))
			tgit(t, u.cfg.WorkspaceDir, "init", "-q")
			leak()
			if p.removeCredential(&checkoutCredential{username: "reader", password: prepToken}) {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("an unremovable credential file (a read-only mount) is not a failure", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root removes anything")
		}
		u := newPrepUnit(t, nil)
		dir := filepath.Dir(u.cred)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		u.wantExit(u.run(), ExitOK)
		if !u.rt.receipts[0].CredentialsRemoved {
			t.Fatal("the receipt does not say the credentials are gone")
		}
	})
}

func Test_unit_resource_config_and_commands(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	base := map[string]string{
		"KODAMA_SERVER_URL": "https://runtime.example.invalid", "KODAMA_CA_FILE": "/ca.pem", "KODAMA_RUN_ID": "run_01", "KODAMA_RESOURCE_KEY": "preparation",
		"KODAMA_ADMISSION_CREDENTIAL_FILE": "/identity/admission", "KODAMA_WORKLOAD_NAME": "kr-abc", "KODAMA_WORKLOAD_UID": "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f",
		"KODAMA_POD_NAME": "kr-abc-x7k2p", "KODAMA_POD_UID": "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60",
		"KODAMA_WORKSPACE_DIR": "/workspace", "KODAMA_CHECKOUT_CREDENTIAL_FILE": "/identity/checkout",
	}
	cfg, rest, err := parseResourceConfig("prepare", nil, env(base), nil)
	if err != nil || len(rest) != 0 {
		t.Fatalf("%v %v", err, rest)
	}
	if cfg.Key != "preparation" || cfg.Identity.WorkloadName != "kr-abc" || cfg.Identity.PodName == nil || *cfg.Identity.PodUID != "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60" ||
		cfg.WorkspaceDir != "/workspace" || cfg.CheckoutCredentialFile != "/identity/checkout" || cfg.CAFile != "/ca.pem" {
		t.Fatalf("config %+v", cfg)
	}
	// A service key, no Pod pair, argv after `--`.
	svc := map[string]string{}
	for k, v := range base {
		svc[k] = v
	}
	svc["KODAMA_RESOURCE_KEY"], svc["KODAMA_POD_NAME"], svc["KODAMA_POD_UID"] = "service.db", "", ""
	cfg, rest, err = parseResourceConfig("service-run", []string{"--", "/bin/postgres", "-c", "x=y"}, env(svc), nil)
	if err != nil || cfg.Identity.PodName != nil || strings.Join(rest, " ") != "/bin/postgres -c x=y" {
		t.Fatalf("%+v %v %v", cfg, rest, err)
	}
	for name, bad := range map[string]map[string]string{
		"a pod name without a uid": {"KODAMA_POD_UID": ""},
		"a non-resource key":       {"KODAMA_RESOURCE_KEY": "workspace"},
		"http to a remote host":    {"KODAMA_SERVER_URL": "http://runtime.example.invalid"},
		"no run":                   {"KODAMA_RUN_ID": ""},
		"a bad workload uid":       {"KODAMA_WORKLOAD_UID": "has space"},
	} {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range bad {
			m[k] = v
		}
		if _, _, err := parseResourceConfig("prepare", nil, env(m), nil); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// The commands are registrations like StepCommand.
	if c := PrepareCommand(); c.Name != "prepare" || c.Run == nil || c.Summary == "" {
		t.Fatalf("%+v", c)
	}
	if c := ServiceRunCommand(); c.Name != "service-run" || c.Run == nil || c.Summary == "" {
		t.Fatalf("%+v", c)
	}
	var out, errb strings.Builder
	if code := RunPrepare(context.Background(), []string{"extra"}, env(base), &out, &errb); code != ExitUsage {
		t.Fatalf("prepare with a positional argument: %d", code)
	}
	if code := RunServiceRun(context.Background(), nil, env(svc), &out, &errb); code != ExitUsage {
		t.Fatalf("service-run without an argv: %d", code)
	}
	if code := RunPrepare(context.Background(), []string{"-h"}, env(base), &out, &errb); code != ExitOK {
		t.Fatalf("prepare -h: %d", code)
	}
}

// Review finding 3: the checkout's timeout must end Git AND its helpers. A server
// that accepts the connection and never answers leaves git-remote-https running
// and holding Git's stderr pipe open; killing only git made the call wait for the
// helper past every deadline.
func Test_unit_prepare_stalled_server_ends_with_its_helpers(t *testing.T) {
	u := newPrepUnit(t, nil)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	u.gs.setWrap(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		select { // never answers, until the test is over
		case <-release:
		case <-r.Context().Done():
		}
	})
	u.rt.ctx = u.context(u.first, 1, 2, time.Hour) // one try, two seconds
	port := u.gs.URL[strings.LastIndex(u.gs.URL, ":")+1:]

	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- u.run() }()
	select {
	case code := <-done:
		if code != ExitCheckout {
			t.Fatalf("exit %d\n%s", code, u.logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the preparation outlived its timeout: a stalled helper held the call")
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("took %s for a 2s budget", took)
	}
	if u.rt.count("completePreparation") != 0 {
		t.Fatal("a receipt was recorded")
	}
	// No git process (git, git-remote-https) that names the stalled server survives.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("ps", "-axo", "pid=,command=").Output()
		var left []string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "127.0.0.1:"+port) && strings.Contains(line, "git") && !strings.Contains(line, "ps -axo") {
				left = append(left, line)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendants survived the timeout: %v", left)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

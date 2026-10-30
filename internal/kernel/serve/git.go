package serve

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gitjwt "github.com/optimiware/kodama/internal/gitauth/jwt"
	"github.com/optimiware/kodama/internal/gitservice"
	"github.com/optimiware/kodama/internal/kernel/api"
)

// TokenIssuer is the issuer claim of the forwarding assertions and worker
// identities of the Git hosting.
const TokenIssuer = "kodama-git"

// ErrInProcessInProduction is the refusal of the in-process, temporary-directory
// Git worker outside development: production hosts each tenant's worker on its
// own volume and the server reaches it by address (the test environment's
// container workers are T-170).
var ErrInProcessInProduction = errors.New("kernel: production refuses the in-process Git worker; set KODAMA_GIT_WORKER_URL to the tenant workers' address")

// GitWorkersConfig configures how tenants' Git workers are hosted.
type GitWorkersConfig struct {
	// Env is the environment name; only "development" hosts in process unless
	// InProcess is explicitly set.
	Env string
	// InProcess hosts one worker per tenant in this process. It is the
	// development default and the explicit stage-only embedding. Any other
	// production profile requires WorkerURL.
	InProcess bool
	// InProcessAck must be "stage-only" for InProcess outside development.
	InProcessAck string
	// StateDir is the root of in-process data roots (<StateDir>/git/<tenantId>).
	StateDir string
	// WorkerURL is the address of a tenant's worker, with {tenantId} replaced by
	// the tenant ID; empty hosts in process (development only).
	WorkerURL string
	// ControlToken authenticates the control plane to a worker's RPC. Empty in
	// development generates one for the in-process workers.
	ControlToken string
	// HookBinary is the executable the hooks run as `<HookBinary> git-hook <name>`
	// (the running executable when empty).
	HookBinary string
	// Verifier verifies forwarding assertions in the in-process workers.
	Verifier *gitjwt.Verifier
	Logger   *slog.Logger
	// WrapControl, when set (tests), wraps the control plane an in-process worker
	// calls, to observe or fail its calls.
	WrapControl func(tenant domain.TenantID, control gitservice.ControlPlane) gitservice.ControlPlane
}

// GitWorkers hosts one Git worker per tenant and is the api.GitWorkers of the
// server: in development each runs in the server process on a data root under the
// state directory; otherwise the server forwards to the tenant's worker by
// address. It implements api.GitWorkers.
type GitWorkers struct {
	cfg     GitWorkersConfig
	control *api.GitControl

	mu      sync.Mutex
	workers map[domain.TenantID]*endpoint
	closed  bool
}

var _ api.GitWorkers = (*GitWorkers)(nil)

// endpoint is one tenant's worker as the control plane reaches it.
type endpoint struct {
	rpc *workerRPC
	git http.Handler
	svc *gitservice.Service // nil for a remote worker
}

// NewGitWorkers validates the hosting mode. Outside development without a worker
// address it returns ErrInProcessInProduction: the caller may still serve the API
// with the returned refusing value, which fails every worker call closed.
func NewGitWorkers(cfg GitWorkersConfig) (*GitWorkers, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	g := &GitWorkers{cfg: cfg, workers: map[domain.TenantID]*endpoint{}}
	inProcess := cfg.InProcess || cfg.Env == "development"
	if inProcess && cfg.Env != "development" && cfg.InProcessAck != "stage-only" {
		return g, errors.New("kernel: in-process Git hosting outside development needs the explicit stage-only acknowledgement")
	}
	if cfg.WorkerURL == "" {
		if !inProcess {
			return g, ErrInProcessInProduction
		}
		if cfg.Verifier == nil {
			return g, errors.New("kernel: the in-process Git worker needs a forwarding verifier")
		}
		if cfg.StateDir == "" {
			return g, errors.New("kernel: the in-process Git worker needs a state directory")
		}
		if g.cfg.ControlToken == "" {
			token, err := randomToken()
			if err != nil {
				return g, err
			}
			g.cfg.ControlToken = token
		}
		g.cfg.InProcess = true
		return g, nil
	}
	if !strings.Contains(cfg.WorkerURL, "{tenantId}") {
		return g, errors.New("kernel: KODAMA_GIT_WORKER_URL names each tenant's own worker and must contain {tenantId}")
	}
	if cfg.ControlToken == "" {
		return g, errors.New("kernel: remote Git workers need KODAMA_GIT_CONTROL_TOKEN")
	}
	return g, nil
}

// BindControl connects the in-process workers to the control plane they call. It
// must precede the first worker call.
func (g *GitWorkers) BindControl(c *api.GitControl) { g.control = c }

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b[:]), nil
}

func (g *GitWorkers) endpoint(tenant domain.TenantID) (*endpoint, error) {
	if err := tenant.Validate(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, errors.New("kernel: the Git workers are stopped")
	}
	if ep := g.workers[tenant]; ep != nil {
		return ep, nil
	}
	var ep *endpoint
	switch {
	case g.cfg.WorkerURL != "":
		target, err := url.Parse(strings.ReplaceAll(g.cfg.WorkerURL, "{tenantId}", string(tenant)))
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			return nil, errors.New("kernel: the tenant Git worker address is not an http(s) URL")
		}
		proxy := &httputil.ReverseProxy{
			Rewrite:       func(r *httputil.ProxyRequest) { r.SetURL(target) },
			FlushInterval: -1, // stream packs both ways
			ErrorHandler:  g.workerUnavailable,
		}
		ep = &endpoint{rpc: &workerRPC{base: strings.TrimSuffix(target.String(), "/"), token: g.cfg.ControlToken, hc: &http.Client{}}, git: proxy}
	case g.cfg.InProcess || g.cfg.Env == "development":
		if g.control == nil {
			return nil, errors.New("kernel: the Git workers are not bound to the control plane")
		}
		root, err := filepath.Abs(filepath.Join(g.cfg.StateDir, "git", string(tenant)))
		if err != nil {
			return nil, err
		}
		var control gitservice.ControlPlane = g.control.ForTenant(tenant)
		if g.cfg.WrapControl != nil {
			control = g.cfg.WrapControl(tenant, control)
		}
		svc, err := gitservice.New(gitservice.Config{
			TenantID: tenant, DataRoot: root, ControlPlane: control,
			Assertions: g.cfg.Verifier, Callers: bearerCallers{token: g.cfg.ControlToken},
			HookBinary: g.cfg.HookBinary, Logger: g.cfg.Logger.With("git-worker", tenant),
		})
		if err != nil {
			return nil, fmt.Errorf("kernel: starting the Git worker of tenant %s: %w", tenant, err)
		}
		h := svc.Handler()
		ep = &endpoint{rpc: &workerRPC{base: "http://git-worker.invalid", token: g.cfg.ControlToken, hc: &http.Client{Transport: handlerTransport{h}}},
			git: svc.GitHandler(), svc: svc}
	default:
		return nil, ErrInProcessInProduction
	}
	g.workers[tenant] = ep
	return ep, nil
}

// workerUnavailable answers a request whose forwarding to the tenant's worker
// failed (the connection was refused or lost, the worker timed out): 503 with
// Retry-After, as the API document promises, and the same body shape as the
// other Git refusals. The default handler would answer 502 with no body and no
// hint. A caller that has already gone away gets no answer. Nothing about the
// worker's address or the transport error reaches the caller.
func (g *GitWorkers) workerUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	g.cfg.Logger.Warn("the tenant's Git worker could not be reached", "path", r.URL.Path, "error", err)
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"error":{"code":"unavailable","message":"the tenant's Git worker is unavailable"}}`+"\n")
}

// GitHandler serves the tenant's smart HTTP endpoints (api.GitWorkers).
func (g *GitWorkers) GitHandler(_ context.Context, tenant domain.TenantID) (http.Handler, error) {
	ep, err := g.endpoint(tenant)
	if err != nil {
		return nil, err
	}
	return ep.git, nil
}

func (g *GitWorkers) Initialize(ctx context.Context, tenant domain.TenantID, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error) {
	ep, err := g.endpoint(tenant)
	if err != nil {
		return gitv1.InitializeRepositoryResponse{}, err
	}
	return workerCall[gitv1.InitializeRepositoryResponse](ctx, ep.rpc, req.RepositoryID, "initialize", req)
}

func (g *GitWorkers) ReadRefs(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefsRequest) (gitv1.RefsResponse, error) {
	ep, err := g.endpoint(tenant)
	if err != nil {
		return gitv1.RefsResponse{}, err
	}
	return workerCall[gitv1.RefsResponse](ctx, ep.rpc, req.RepositoryID, "refs/list", req)
}

func (g *GitWorkers) IsAncestor(ctx context.Context, tenant domain.TenantID, req gitv1.IsAncestorRequest) (gitv1.IsAncestorResponse, error) {
	ep, err := g.endpoint(tenant)
	if err != nil {
		return gitv1.IsAncestorResponse{}, err
	}
	return workerCall[gitv1.IsAncestorResponse](ctx, ep.rpc, req.RepositoryID, "objects/is-ancestor", req)
}

// ReadFiles reads pipeline definitions at an exact commit from the tenant's
// worker (dispatch.Files).
func (g *GitWorkers) ReadFiles(ctx context.Context, tenant domain.TenantID, req gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error) {
	ep, err := g.endpoint(tenant)
	if err != nil {
		return gitv1.ReadFilesResponse{}, err
	}
	return workerCall[gitv1.ReadFilesResponse](ctx, ep.rpc, req.RepositoryID, "files", req)
}

// Reconcile asks each started in-process worker to poll its assignments once
// (initialize what is provisioning, re-report what is unacknowledged). A remote
// worker polls on its own.
func (g *GitWorkers) Reconcile(ctx context.Context, tenants []domain.TenantID) {
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return
		}
		ep, err := g.endpoint(tenant)
		if err != nil || ep.svc == nil {
			continue
		}
		if err := ep.svc.Reconcile(ctx); err != nil && ctx.Err() == nil {
			g.cfg.Logger.Warn("git worker reconcile failed", "tenant", tenant, "error", err)
		}
	}
}

// Started reports whether the tenant's in-process worker exists.
func (g *GitWorkers) Started(tenant domain.TenantID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.workers[tenant] != nil
}

// Ready reports the hosting mode is usable. In-process workers open lazily on
// first use, so readiness means the process still owns the worker registry.
func (g *GitWorkers) Ready() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return errors.New("git workers are stopped")
	}
	return nil
}

// Close stops every in-process worker.
func (g *GitWorkers) Close() error {
	g.mu.Lock()
	g.closed = true
	workers := g.workers
	g.workers = map[domain.TenantID]*endpoint{}
	g.mu.Unlock()
	var first error
	for _, ep := range workers {
		if ep.svc != nil {
			if err := ep.svc.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// bearerCallers authenticates the control plane's RPC to a worker with a shared
// secret.
type bearerCallers struct{ token string }

func (b bearerCallers) VerifyControlCall(_ context.Context, r *http.Request) error {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || b.token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(b.token)) != 1 {
		return &gitv1.ContractError{Code: domain.CodeInvalidCredential, Message: "the caller was refused"}
	}
	return nil
}

// ControlCallers is the ControlCallerVerifier of a standalone worker holding the
// shared control token.
func ControlCallers(token string) gitservice.ControlCallerVerifier {
	return bearerCallers{token: token}
}

// handlerTransport calls an in-process handler as an HTTP round trip.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// workerRPC is the control plane's client of a worker's RPC (gitv1.WorkerBasePath).
type workerRPC struct {
	base, token string
	hc          *http.Client
}

func workerCall[T any, P interface {
	*T
	Validate() error
}](ctx context.Context, c *workerRPC, repositoryID, op string, in any) (T, error) {
	var zero T
	body, err := json.Marshal(in)
	if err != nil {
		return zero, err
	}
	if domain.RepositoryID(repositoryID).Validate() != nil {
		return zero, &gitv1.ContractError{Code: domain.CodeInvalidRequest, Message: "repositoryId is not a valid ID"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+gitv1.WorkerBasePath+"/repositories/"+repositoryID+"/"+op, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return zero, err
	}
	if resp.StatusCode != http.StatusOK {
		var e gitv1.ErrorResponse
		if json.Unmarshal(data, &e) == nil && e.Error.Code != "" {
			return zero, &gitv1.ContractError{Code: e.Error.Code, Message: e.Error.Message}
		}
		return zero, &gitv1.ContractError{Code: domain.CodeUnavailable, Message: fmt.Sprintf("the worker answered %d", resp.StatusCode)}
	}
	return gitv1.DecodeResponse[T, P](data)
}

// ---- token keys -------------------------------------------------------------

// NewGitSigner builds the control plane's signer for forwarding assertions and
// worker identities. The two keys are distinct. Production reads both from PEM
// files; development generates ephemeral keys when none are configured.
func NewGitSigner(env, forwardingKeyFile, identityKeyFile string) (*gitjwt.Signer, error) {
	load := func(path string) (*ecdsa.PrivateKey, error) {
		if path == "" {
			if env == "production" {
				return nil, errors.New("kernel: production requires KODAMA_GIT_FORWARDING_KEY_FILE and KODAMA_GIT_IDENTITY_KEY_FILE")
			}
			return gitjwt.GenerateKey()
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("kernel: git signing key: %w", err)
		}
		return gitjwt.ParsePrivateKeyPEM(data)
	}
	fk, err := load(forwardingKeyFile)
	if err != nil {
		return nil, err
	}
	ik, err := load(identityKeyFile)
	if err != nil {
		return nil, err
	}
	fr, err := gitjwt.NewKeyRing(1, fk)
	if err != nil {
		return nil, err
	}
	ir, err := gitjwt.NewKeyRing(1, ik)
	if err != nil {
		return nil, err
	}
	return gitjwt.NewSigner(gitjwt.SignerConfig{Issuer: TokenIssuer, Forwarding: fr, Identity: ir})
}

// ParsePublicKeys reads the forwarding public keys of a standalone worker: one or
// two PUBLIC KEY PEM blocks, the current key first and the previous one for a
// rotation overlap.
func ParsePublicKeys(data []byte) (gitjwt.PublicKeys, error) {
	var keys []*ecdsa.PublicKey
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "PUBLIC KEY" {
			return gitjwt.PublicKeys{}, errors.New("kernel: only PUBLIC KEY blocks are accepted for verification keys")
		}
		pub, err := gitjwt.ParsePublicKeyPEM(pem.EncodeToMemory(block))
		if err != nil {
			return gitjwt.PublicKeys{}, err
		}
		keys = append(keys, pub)
		data = rest
	}
	switch len(keys) {
	case 1:
		return gitjwt.PublicKeys{Current: keys[0]}, nil
	case 2:
		return gitjwt.PublicKeys{Current: keys[0], Previous: keys[1]}, nil
	}
	return gitjwt.PublicKeys{}, errors.New("kernel: expected one or two PUBLIC KEY blocks")
}

// reconcileLoop drives the in-process workers: it starts a worker for every
// tenant that has repositories and lets it poll its assignments, so provisioning
// finishes and unreported receives are re-reported.
func reconcileLoop(ctx context.Context, workers *GitWorkers, tenants func(context.Context) ([]domain.TenantID, error), hasWork func(context.Context, domain.TenantID) bool, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		ids, err := tenants(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("git reconcile cannot list tenants", "error", err)
			}
		} else {
			var active []domain.TenantID
			for _, id := range ids {
				if workers.Started(id) || hasWork(ctx, id) {
					active = append(active, id)
				}
			}
			workers.Reconcile(ctx, active)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

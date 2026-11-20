package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	opsapp "github.com/optimiware/kodema/internal/shell/app/ops"
)

const (
	gitCompanionProvisionPath    = "/_kodema/repositories"
	gitCompanionMaxResponseBytes = 64 << 10
	gitCompanionDefaultTimeout   = 30 * time.Second
	gitCompanionDefaultServiceID = "git"
)

// GitCompanionConfig configures the Shell composition's optional HTTP boundary
// to the independent Userland Git companion (`kodema git serve`). Shell never
// accesses Git storage or `conf/git.conf` directly; it calls the Git companion
// over HTTP to provision repositories and derives default binding origins and
// clone URLs from this configuration.
type GitCompanionConfig struct {
	// AdminURL is the base URL of the Git companion HTTP server (for example
	// "http://kodema-default-git:8081" or "https://git-{tenant}.svc:8081").
	// A "{tenant}" placeholder is replaced by the owning tenant ID per request.
	AdminURL string
	// PublicGitURL is the client-facing Git HTTP base URL used to render
	// clone_url (for example "https://127.0.0.1:8443/git"). When empty, it
	// falls back to "<Installation.PublicURL>/git" when PublicURL is set.
	PublicGitURL string
	// ServiceID is the default enforcing service ID for repository bindings
	// created via repository.create. Defaults to "git".
	ServiceID string
	// TrustedOrigins are the default trusted origins for repository bindings
	// created via repository.create. When empty, defaults to the installation's
	// public URL and workload URL (deduplicated).
	TrustedOrigins []string
	// TokenFile is the path to the bearer token file presented to the Git
	// companion's provisioning endpoint. It is read on every request so
	// rotations take effect without restarting the server.
	TokenFile string
	// CAFile optionally trusts a PEM CA certificate bundle when AdminURL uses
	// HTTPS.
	CAFile string
	// RoundTripper optionally overrides the HTTP transport (for tests).
	RoundTripper http.RoundTripper
}

func (cfg *GitCompanionConfig) validate() error {
	if cfg == nil {
		return nil
	}
	adminURL := strings.TrimSpace(cfg.AdminURL)
	if adminURL == "" {
		return fault.New(fault.InvalidInput, "git companion configuration requires an admin URL")
	}
	sample := strings.ReplaceAll(adminURL, "{tenant}", "tenant-check")
	parsed, err := url.Parse(sample)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fault.New(fault.InvalidInput, "git companion admin URL must be an absolute http(s) URL without userinfo, query or fragment")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fault.New(fault.InvalidInput, "git companion admin URL must use http or https")
	}
	if strings.TrimSpace(cfg.TokenFile) == "" {
		return fault.New(fault.InvalidInput, "git companion configuration requires a provision token file")
	}
	return nil
}

type httpRepositoryProvisioner struct {
	adminURL       string
	publicGitURL   string
	serviceID      string
	trustedOrigins []string
	tokenFile      string
	client         *http.Client
}

var _ opsapp.RepositoryProvisioner = (*httpRepositoryProvisioner)(nil)

func newHTTPRepositoryProvisioner(cfg Config) (*httpRepositoryProvisioner, error) {
	if cfg.GitCompanion == nil {
		return nil, nil
	}
	if err := cfg.GitCompanion.validate(); err != nil {
		return nil, err
	}
	rt := cfg.GitCompanion.RoundTripper
	if rt == nil {
		var transport *http.Transport
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = base.Clone()
		} else {
			transport = &http.Transport{}
		}
		if caFile := strings.TrimSpace(cfg.GitCompanion.CAFile); caFile != "" {
			pemData, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fault.Wrap(fault.InvalidInput, "git companion CA file is unreadable", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemData) {
				return nil, fault.New(fault.InvalidInput, "git companion CA file contains no valid PEM certificate")
			}
			tlsCfg := transport.TLSClientConfig
			if tlsCfg == nil {
				tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
			} else {
				tlsCfg = tlsCfg.Clone()
			}
			tlsCfg.RootCAs = pool
			transport.TLSClientConfig = tlsCfg
		}
		rt = transport
	}
	serviceID := strings.TrimSpace(cfg.GitCompanion.ServiceID)
	if serviceID == "" {
		serviceID = gitCompanionDefaultServiceID
	}
	publicGitURL := strings.TrimRight(strings.TrimSpace(cfg.GitCompanion.PublicGitURL), "/")
	if publicGitURL == "" {
		if pub := strings.TrimRight(strings.TrimSpace(cfg.Installation.PublicURL), "/"); pub != "" {
			publicGitURL = pub + "/git"
		}
	}
	origins := deriveDefaultGitTrustedOrigins(cfg.GitCompanion.TrustedOrigins, cfg.Installation.PublicURL, cfg.WorkloadURL)
	return &httpRepositoryProvisioner{
		adminURL:       strings.TrimRight(strings.TrimSpace(cfg.GitCompanion.AdminURL), "/"),
		publicGitURL:   publicGitURL,
		serviceID:      serviceID,
		trustedOrigins: origins,
		tokenFile:      strings.TrimSpace(cfg.GitCompanion.TokenFile),
		client:         &http.Client{Transport: rt, Timeout: gitCompanionDefaultTimeout},
	}, nil
}

func deriveDefaultGitTrustedOrigins(explicit []string, publicURL, workloadURL string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(explicit)+2)
	add := func(raw string) {
		trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
		if trimmed == "" || seen[trimmed] {
			return
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	if len(explicit) > 0 {
		for _, origin := range explicit {
			add(origin)
		}
		return out
	}
	add(publicURL)
	add(workloadURL)
	return out
}

func (p *httpRepositoryProvisioner) DefaultServiceID() string {
	return p.serviceID
}

func (p *httpRepositoryProvisioner) DefaultTrustedOrigins() []string {
	return append([]string(nil), p.trustedOrigins...)
}

func (p *httpRepositoryProvisioner) CloneURL(repository string) string {
	repo := strings.TrimLeft(strings.TrimSpace(repository), "/")
	if p.publicGitURL == "" || repo == "" {
		return ""
	}
	if !strings.HasSuffix(repo, ".git") {
		repo += ".git"
	}
	return p.publicGitURL + "/" + repo
}

type gitCompanionProvisionRequest struct {
	Repository    string `json:"repository"`
	ResourceID    string `json:"resource_id"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

type gitCompanionProvisionResponse struct {
	Repository    string `json:"repository"`
	ResourceID    string `json:"resource_id"`
	DefaultBranch string `json:"default_branch"`
	Created       bool   `json:"created"`
}

func (p *httpRepositoryProvisioner) ProvisionRepository(ctx context.Context, req opsapp.RepositoryProvisionRequest) (opsapp.RepositoryProvisionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tokenBytes, err := os.ReadFile(p.tokenFile)
	if err != nil {
		return opsapp.RepositoryProvisionResult{}, fault.Wrap(fault.DependencyUnavailable,
			"git companion provision credential is unavailable", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return opsapp.RepositoryProvisionResult{}, fault.New(fault.DependencyUnavailable,
			"git companion provision credential is empty")
	}

	payload, err := json.Marshal(gitCompanionProvisionRequest{
		Repository:    req.Repository,
		ResourceID:    req.ResourceID,
		DefaultBranch: req.DefaultBranch,
	})
	if err != nil {
		return opsapp.RepositoryProvisionResult{}, fault.Wrap(fault.InternalFailure,
			"encode git repository provision request", err)
	}
	baseURL := strings.ReplaceAll(p.adminURL, "{tenant}", string(req.TenantID))
	targetURL := strings.TrimRight(baseURL, "/") + gitCompanionProvisionPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return opsapp.RepositoryProvisionResult{}, fault.Wrap(fault.InternalFailure,
			"build git repository provision request", err)
	}
	httpReq.Header.Set("Content-Type", api.MediaType)
	httpReq.Header.Set("Accept", api.MediaType)
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return opsapp.RepositoryProvisionResult{}, ctxErr
		}
		return opsapp.RepositoryProvisionResult{}, fault.Wrap(fault.DependencyUnavailable,
			"git companion service is unavailable", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, gitCompanionMaxResponseBytes+1))
	if err != nil || len(body) > gitCompanionMaxResponseBytes {
		return opsapp.RepositoryProvisionResult{}, fault.New(fault.DependencyUnavailable,
			"git companion returned an unreadable response")
	}
	if resp.StatusCode != http.StatusOK {
		return opsapp.RepositoryProvisionResult{}, mapGitCompanionError(resp.StatusCode, body)
	}
	var decoded gitCompanionProvisionResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return opsapp.RepositoryProvisionResult{}, fault.Wrap(fault.DependencyUnavailable,
			"git companion returned a malformed provision response", err)
	}
	repoName := decoded.Repository
	if repoName == "" {
		repoName = req.Repository
	}
	branch := decoded.DefaultBranch
	if branch == "" {
		branch = req.DefaultBranch
	}
	return opsapp.RepositoryProvisionResult{
		Repository:    repoName,
		DefaultBranch: branch,
		CloneURL:      p.CloneURL(repoName),
		Created:       decoded.Created,
	}, nil
}

func mapGitCompanionError(status int, body []byte) error {
	var apiErr api.Error
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Code != "" {
		if class, ok := fault.ClassFor(fault.Code(apiErr.Code)); ok {
			msg := strings.TrimSpace(apiErr.Message)
			if msg == "" {
				msg = string(class)
			}
			return fault.New(class, msg)
		}
	}
	switch status {
	case http.StatusBadRequest:
		return fault.New(fault.InvalidInput, "git companion refused the repository request")
	case http.StatusUnauthorized:
		return fault.New(fault.DependencyUnavailable, "git companion rejected the server provision credential")
	case http.StatusForbidden:
		return fault.New(fault.Forbidden, "git companion refused repository provisioning")
	case http.StatusConflict:
		return fault.New(fault.VersionConflict, "repository already exists with a conflicting resource binding")
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusBadGateway:
		return fault.New(fault.DependencyUnavailable, "git companion service is temporarily unavailable")
	default:
		return fault.New(fault.DependencyUnavailable, fmt.Sprintf("git companion returned status %d", status))
	}
}

package app

// The production registry verifier of `ociArtifact` outputs (T-101).
//
// internal/artifacts/oci.Verifier fetches the digest-addressed manifest from
// the registry origin an operator-configured policy authorizes for the
// tenant and repository, hashes it, and reports the trusted provenance of the
// producing attempt. This file supplies both injected authorities:
//
//   - RegistryConfig (registry.json) is the operator's statement of which
//     registries, with which credential, which tenants may register which
//     repository prefixes. Nothing the caller spells (repository, tag) or any
//     registry annotation establishes ownership;
//   - attemptProvenance reads the producing attempt's immutable pinned context
//     and reports its source repository and commit. An attempt without a
//     source has no provenance, and the registration is refused.
//
// With no registry configured the verifier is deniedRegistry: every OCI
// registration is refused (fail closed), and the capability is recorded as
// disabled.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/oci"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// RegistryConfig is the operator's registry statement (registry.json, strict
// JSON). Empty by default: no OCI registration is verified.
type RegistryConfig struct {
	// Endpoints are the registries a tenant may register images from.
	Endpoints []RegistryEndpoint `json:"endpoints"`
	// CAFile adds private trust roots for the registries (PEM).
	CAFile string `json:"caFile,omitempty"`
	// HTTPClient performs the registry requests (nil: a client over the
	// system roots and CAFile). Redirects are always restricted by the
	// verifier to the authorized origin.
	HTTPClient *http.Client `json:"-"`
}

// RegistryEndpoint authorizes one registry origin for tenants and repository
// prefixes.
type RegistryEndpoint struct {
	// Host is the registry as repository references spell it: host[:port].
	Host string `json:"host"`
	// URL is the origin requests go to (scheme://host[:port]); default
	// https://<host>. Its host must be Host.
	URL string `json:"url,omitempty"`
	// AuthorizationFile names, in the secrets directory, the file holding the
	// complete Authorization header value the registry needs (absent:
	// anonymous). It is only ever sent to URL's origin, over https.
	AuthorizationFile string `json:"authorizationFile,omitempty"`
	// AllowInsecureHTTP permits an http origin (the local fixture; refused in
	// production and together with an authorization).
	AllowInsecureHTTP bool `json:"allowInsecureHttp,omitempty"`
	// Tenants lists the tenants allowed to use the endpoint ("*" allows every
	// tenant of the installation and must be written deliberately).
	Tenants []string `json:"tenants"`
	// Repositories lists the repository path prefixes the endpoint may serve:
	// "team/app" matches "team/app" and "team/app/x", never "team/application".
	Repositories []string `json:"repositories"`
	// Authorization is the resolved header value (read from AuthorizationFile
	// by LoadRegistry; set directly by a test).
	Authorization string `json:"-"`
}

// Configured reports whether any registry is named.
func (c RegistryConfig) Configured() bool { return len(c.Endpoints) > 0 }

// LoadRegistry reads registry.json and the authorization files it names (paths
// are relative to dir, the secrets directory).
func LoadRegistry(path, dir string) (RegistryConfig, error) {
	var c RegistryConfig
	if err := readStrictJSON(path, "registry file", &c); err != nil {
		return RegistryConfig{}, err
	}
	for i := range c.Endpoints {
		e := &c.Endpoints[i]
		if e.AuthorizationFile == "" {
			continue
		}
		p := e.AuthorizationFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return RegistryConfig{}, fmt.Errorf("registry %s: %w", e.Host, err)
		}
		e.Authorization = strings.TrimSpace(string(b))
	}
	return c, nil
}

// Validate checks the statement against the environment. Nothing is guessed.
func (c RegistryConfig) Validate(env Env) error {
	if !c.Configured() {
		if c.CAFile != "" {
			return errors.New("registry: trust roots without a registry")
		}
		return nil
	}
	var errs []error
	for i, e := range c.Endpoints {
		if err := e.validate(env); err != nil {
			errs = append(errs, fmt.Errorf("registry endpoint %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func (e RegistryEndpoint) origin() string {
	if e.URL != "" {
		return e.URL
	}
	return "https://" + e.Host
}

func (e RegistryEndpoint) validate(env Env) error {
	ref, err := artifacts.ParseRegistryRef(e.Host + "/repository")
	if err != nil || ref.Host != e.Host {
		return errors.New("the host must be a lowercase host[:port]")
	}
	u, err := url.Parse(e.origin())
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the url must be an origin (scheme://host[:port])")
	}
	if originHost(u) != e.Host {
		return errors.New("the url's host must be the registry host")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && e.AllowInsecureHTTP && env != Production:
		if e.Authorization != "" || e.AuthorizationFile != "" {
			return errors.New("an authorization is never sent over plain http")
		}
	default:
		return errors.New("the registry must be reached over https (plain http only for the local fixture outside production)")
	}
	if len(e.Tenants) == 0 {
		return errors.New("the tenants must be listed (\"*\" for every tenant)")
	}
	if len(e.Repositories) == 0 {
		return errors.New("the repository prefixes must be listed")
	}
	for _, p := range e.Repositories {
		if r, err := artifacts.ParseRegistryRef(e.Host + "/" + p); err != nil || r.Repository != p {
			return fmt.Errorf("the repository prefix %q is not a lowercase repository path", p)
		}
	}
	if strings.ContainsAny(e.Authorization, "\r\n") {
		return errors.New("the authorization must be one header value")
	}
	return nil
}

// registryPolicy is the oci.Policy over the configured endpoints. It answers
// from operator configuration only: an unlisted host, tenant or repository is
// artifacts.ErrRegistryDenied.
type registryPolicy struct{ endpoints []RegistryEndpoint }

var _ oci.Policy = registryPolicy{}

func (p registryPolicy) Authorize(_ context.Context, scope domain.Scope, _ domain.AttemptID, ref artifacts.RegistryRef) (oci.Endpoint, error) {
	for _, e := range p.endpoints {
		if !strings.EqualFold(e.Host, ref.Host) || !tenantListed(e.Tenants, scope.TenantID) || !repositoryListed(e.Repositories, ref.Repository) {
			continue
		}
		return oci.Endpoint{BaseURL: e.origin(), Authorization: e.Authorization, Insecure: e.AllowInsecureHTTP}, nil
	}
	return oci.Endpoint{}, artifacts.ErrRegistryDenied
}

// originHost is the origin's host the way the verifier compares it with the
// repository's: lowercase, without the scheme's default port.
func originHost(u *url.URL) string {
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return strings.ToLower(u.Host)
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return strings.ToLower(host)
	}
	return strings.ToLower(net.JoinHostPort(host, port))
}

func tenantListed(list []string, tenant domain.TenantID) bool {
	for _, t := range list {
		if t == "*" || t == string(tenant) {
			return true
		}
	}
	return false
}

func repositoryListed(prefixes []string, repository string) bool {
	for _, p := range prefixes {
		if repository == p || strings.HasPrefix(repository, p+"/") {
			return true
		}
	}
	return false
}

// RunReader is the execution store's committed read of a run.
type RunReader interface {
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// attemptProvenance is the oci.ProvenanceResolver: the trusted source of an
// image is the source the producing attempt's immutable pinned context named,
// verified against the digest the committed attempt carries. A run or attempt
// that is missing, foreign, inconsistent or source-free has no provenance.
type attemptProvenance struct{ runs RunReader }

var _ oci.ProvenanceResolver = attemptProvenance{}

func (p attemptProvenance) Resolve(ctx context.Context, q oci.ProvenanceQuery) (artifacts.Provenance, error) {
	if q.Scope.Validate() != nil || q.Run.Validate() != nil || q.Attempt.Validate() != nil {
		return artifacts.Provenance{}, artifacts.ErrProvenanceMissing
	}
	agg, err := p.runs.Get(ctx, q.Scope, q.Run)
	switch {
	case err == nil:
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return artifacts.Provenance{}, artifacts.ErrProvenanceMissing
	default:
		return artifacts.Provenance{}, err // an outage: the registration is retried
	}
	if agg == nil || agg.Run == nil || agg.Run.Scope != q.Scope || agg.Run.ID != q.Run {
		return artifacts.Provenance{}, artifacts.ErrProvenanceMissing
	}
	at := agg.Attempt(q.Attempt)
	if at == nil || at.Scope != q.Scope || at.RunID != q.Run {
		return artifacts.Provenance{}, artifacts.ErrProvenanceMissing
	}
	sum := sha256.Sum256(at.Context)
	if at.ContextDigest != fmt.Sprintf("sha256:%x", sum) {
		return artifacts.Provenance{}, artifacts.ErrProvenanceMissing
	}
	c, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil || c.Scope() != q.Scope || c.RunID != q.Run || c.AttemptID != q.Attempt {
		return artifacts.Provenance{}, artifacts.ErrProvenanceMissing
	}
	src := c.Workspace.Source
	if src == nil || src.RepositoryURL == "" || src.Commit == "" {
		return artifacts.Provenance{}, artifacts.ErrProvenanceMissing
	}
	prov := artifacts.Provenance{RepositoryURL: src.RepositoryURL, Commit: string(src.Commit)}
	if prov.Validate() != nil {
		return artifacts.Provenance{}, artifacts.ErrProvenanceMissing
	}
	return prov, nil
}

// registryClient is the registry HTTP client: the system roots plus the
// operator's CA, no proxy from the environment, TLS 1.2 or later.
func (c RegistryConfig) registryClient() (*http.Client, error) {
	if c.HTTPClient != nil {
		return c.HTTPClient, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("registry CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("registry CA: no certificate in the file")
		}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

// NewRegistryVerifier builds the verifier of `ociArtifact` registrations. With
// no registry configured it is deniedRegistry (every registration refused) and
// configured is false; the caller records the capability as disabled.
func NewRegistryVerifier(cfg RegistryConfig, env Env, runs RunReader) (v artifacts.RegistryVerifier, configured bool, err error) {
	if !cfg.Configured() {
		return deniedRegistry{}, false, nil
	}
	if err := cfg.Validate(env); err != nil {
		return nil, false, err
	}
	if runs == nil {
		return nil, false, fmt.Errorf("%w: the run reader of the registry provenance is required", execution.ErrNotConfigured)
	}
	client, err := cfg.registryClient()
	if err != nil {
		return nil, false, err
	}
	ver, err := oci.NewVerifier(oci.Config{
		Policy: registryPolicy{endpoints: append([]RegistryEndpoint(nil), cfg.Endpoints...)}, Provenance: attemptProvenance{runs: runs}, HTTPClient: client,
	})
	if err != nil {
		return nil, false, err
	}
	return ver, true, nil
}

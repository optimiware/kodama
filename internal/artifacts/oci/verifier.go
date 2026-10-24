// Package oci verifies OCI registrations for the artifact module.
//
// Authorization is adapter-local and injected: a Policy decides which registry
// endpoint, with which credential, a tenant/project may use for a repository,
// and a ProvenanceResolver supplies trusted source provenance. Neither the
// caller's spelling of the repository nor any registry annotation establishes
// ownership or provenance. Content is verified by digest: the verifier fetches
// the digest-addressed manifest from the authorized origin with bounded
// requests, hashes the returned bytes and validates the manifest
// representation. Tags and digest headers are never trusted.
package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
)

// Endpoint is the registry origin a policy authorizes for a repository.
type Endpoint struct {
	// BaseURL is scheme://host[:port] with no userinfo, path or query. The host
	// must be the one named by the repository reference.
	BaseURL string
	// Authorization is the complete Authorization header value for this
	// registry (empty for anonymous access). It is only ever sent to BaseURL's
	// origin and only over https unless Insecure is set.
	Authorization string
	// Insecure permits http (development fixtures only).
	Insecure bool
}

// Policy maps a tenant/project/attempt and a repository to the registry
// endpoint it may use. It returns artifacts.ErrRegistryDenied when the project
// has no permission for the repository; any other error is an outage.
type Policy interface {
	Authorize(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ref artifacts.RegistryRef) (Endpoint, error)
}

// ProvenanceQuery identifies the registered content for provenance lookup.
type ProvenanceQuery struct {
	Scope      domain.Scope
	Attempt    domain.AttemptID
	Run        domain.RunID
	Repository string
	Digest     artifacts.Digest
}

// ProvenanceResolver returns the trusted source provenance of content produced
// by the attempt, or artifacts.ErrProvenanceMissing. The execution module
// supplies the production implementation.
type ProvenanceResolver interface {
	Resolve(ctx context.Context, q ProvenanceQuery) (artifacts.Provenance, error)
}

// Config configures a Verifier.
type Config struct {
	Policy     Policy
	Provenance ProvenanceResolver
	// HTTPClient supplies the transport (and a default timeout); nil selects a
	// plain client. Its redirect policy is always replaced.
	HTTPClient *http.Client
	// Timeout bounds one registry request (default 30s).
	Timeout time.Duration
	// MaxManifestBytes bounds the manifest read (default 4 MiB).
	MaxManifestBytes int64
	// MaxRedirects bounds same-origin redirects (default 3).
	MaxRedirects int
}

// Verifier implements artifacts.RegistryVerifier.
type Verifier struct {
	cfg    Config
	client *http.Client
}

var _ artifacts.RegistryVerifier = (*Verifier)(nil)

// NewVerifier requires both injected authorities: without them nothing can be
// verified.
func NewVerifier(cfg Config) (*Verifier, error) {
	if cfg.Policy == nil || cfg.Provenance == nil {
		return nil, errors.New("artifacts/oci: a repository policy and a provenance resolver are required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxManifestBytes <= 0 {
		cfg.MaxManifestBytes = 4 << 20
	}
	if cfg.MaxRedirects <= 0 {
		cfg.MaxRedirects = 3
	}
	base := http.Client{}
	if cfg.HTTPClient != nil {
		base = *cfg.HTTPClient
	}
	base.Timeout = cfg.Timeout
	maxRedirects := cfg.MaxRedirects
	base.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return errUnsafeRedirect
		}
		first := via[0].URL
		// Only the authorized origin may be contacted: never another host, port
		// or scheme, so a credential cannot be forwarded elsewhere.
		if req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host) {
			return errUnsafeRedirect
		}
		return nil
	}
	return &Verifier{cfg: cfg, client: &base}, nil
}

var errUnsafeRedirect = errors.New("unsafe registry redirect")

var manifestTypes = []string{
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
}

// Verify implements artifacts.RegistryVerifier.
func (v *Verifier) Verify(ctx context.Context, req artifacts.OCIRequest) (artifacts.OCIVerification, error) {
	ref, err := artifacts.ParseRegistryRef(req.Registry)
	if err != nil || req.Digest.Validate() != nil {
		return artifacts.OCIVerification{}, artifacts.ErrRegistryDenied
	}
	ep, err := v.cfg.Policy.Authorize(ctx, req.Scope, req.Attempt, ref)
	if err != nil {
		if errors.Is(err, artifacts.ErrRegistryDenied) {
			return artifacts.OCIVerification{}, artifacts.ErrRegistryDenied
		}
		return artifacts.OCIVerification{}, fmt.Errorf("%w: repository policy", artifacts.ErrRegistryUnavailable)
	}
	target, err := v.target(ep, ref, req.Digest)
	if err != nil {
		return artifacts.OCIVerification{}, err
	}

	body, mediaType, err := v.fetch(ctx, ep, target, req.Digest)
	if err != nil {
		return artifacts.OCIVerification{}, err
	}
	if err := validateManifest(body, mediaType); err != nil {
		return artifacts.OCIVerification{}, err
	}

	prov, err := v.cfg.Provenance.Resolve(ctx, ProvenanceQuery{Scope: req.Scope, Attempt: req.Attempt, Run: req.Run, Repository: ref.String(), Digest: req.Digest})
	if err != nil {
		if errors.Is(err, artifacts.ErrProvenanceMissing) {
			return artifacts.OCIVerification{}, artifacts.ErrProvenanceMissing
		}
		return artifacts.OCIVerification{}, fmt.Errorf("%w: provenance resolution", artifacts.ErrRegistryUnavailable)
	}
	if prov.IsZero() || prov.Validate() != nil {
		return artifacts.OCIVerification{}, artifacts.ErrProvenanceMissing
	}
	return artifacts.OCIVerification{Repository: ref.String(), Digest: req.Digest, ManifestMediaType: mediaType, ManifestSize: int64(len(body)), Provenance: prov}, nil
}

// target builds the digest-addressed manifest URL after validating that the
// authorized origin is the registry the repository names.
func (v *Verifier) target(ep Endpoint, ref artifacts.RegistryRef, d artifacts.Digest) (string, error) {
	u, err := url.Parse(ep.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", artifacts.ErrRegistryDenied
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && ep.Insecure:
	default:
		return "", artifacts.ErrRegistryDenied
	}
	if u.Scheme == "http" && ep.Authorization != "" && !ep.Insecure {
		return "", artifacts.ErrRegistryDenied
	}
	if !strings.EqualFold(canonicalHost(u), ref.Host) {
		// The policy authorized some other origin than the one the caller named.
		return "", artifacts.ErrRegistryDenied
	}
	return u.Scheme + "://" + u.Host + "/v2/" + ref.Repository + "/manifests/" + string(d), nil
}

func canonicalHost(u *url.URL) string {
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return strings.ToLower(u.Host)
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return strings.ToLower(host)
	}
	return strings.ToLower(net.JoinHostPort(host, port))
}

func (v *Verifier) fetch(ctx context.Context, ep Endpoint, target string, want artifacts.Digest) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", artifacts.ErrRegistryDenied
	}
	req.Header.Set("Accept", strings.Join(manifestTypes, ", "))
	if ep.Authorization != "" {
		req.Header.Set("Authorization", ep.Authorization)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		if errors.Is(err, errUnsafeRedirect) {
			return nil, "", fmt.Errorf("%w: unsafe redirect", artifacts.ErrRegistryContent)
		}
		return nil, "", fmt.Errorf("%w: request failed", artifacts.ErrRegistryUnavailable)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, "", artifacts.ErrRegistryDenied
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500, resp.StatusCode == http.StatusRequestTimeout:
		return nil, "", fmt.Errorf("%w: status %d", artifacts.ErrRegistryUnavailable, resp.StatusCode)
	default:
		return nil, "", fmt.Errorf("%w: status %d", artifacts.ErrRegistryContent, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, v.cfg.MaxManifestBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: reading manifest", artifacts.ErrRegistryUnavailable)
	}
	if int64(len(body)) > v.cfg.MaxManifestBytes {
		return nil, "", fmt.Errorf("%w: manifest too large", artifacts.ErrRegistryContent)
	}
	sum := sha256.Sum256(body)
	if got := artifacts.DigestOfSum(sum[:]); got != want {
		return nil, "", fmt.Errorf("%w: manifest bytes do not hash to the digest", artifacts.ErrRegistryContent)
	}
	// A digest header is advisory; when present it must not contradict the
	// bytes that were actually hashed.
	if h := resp.Header.Get("Docker-Content-Digest"); h != "" && h != string(want) {
		return nil, "", fmt.Errorf("%w: digest header disagrees with the content", artifacts.ErrRegistryContent)
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !isManifestType(mt) {
		return nil, "", fmt.Errorf("%w: not a manifest representation", artifacts.ErrRegistryContent)
	}
	return body, mt, nil
}

func isManifestType(mt string) bool {
	for _, t := range manifestTypes {
		if mt == t {
			return true
		}
	}
	return false
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      *int64 `json:"size"`
}

// validateManifest checks the representation: schema version 2, a declared
// media type matching the header, and the structure each type requires.
func validateManifest(body []byte, mediaType string) error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", artifacts.ErrRegistryContent, msg) }
	dec := json.NewDecoder(bytes.NewReader(body))
	var m struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        *descriptor  `json:"config"`
		Layers        []descriptor `json:"layers"`
		Manifests     []descriptor `json:"manifests"`
	}
	if err := dec.Decode(&m); err != nil {
		return bad("manifest is not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("trailing data after the manifest")
	}
	if m.SchemaVersion != 2 {
		return bad("unsupported manifest schema version")
	}
	if m.MediaType != "" && m.MediaType != mediaType {
		return bad("manifest media type disagrees with its content type")
	}
	check := func(d descriptor) bool {
		return artifacts.Digest(d.Digest).Validate() == nil && d.Size != nil && *d.Size >= 0
	}
	switch mediaType {
	case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		if len(m.Manifests) == 0 {
			return bad("an index lists at least one manifest")
		}
		for _, d := range m.Manifests {
			if !check(d) {
				return bad("an index entry has no valid digest and size")
			}
		}
	default:
		if m.Config == nil || !check(*m.Config) {
			return bad("an image manifest has a config descriptor")
		}
		for _, d := range m.Layers {
			if !check(d) {
				return bad("a layer has no valid digest and size")
			}
		}
	}
	return nil
}

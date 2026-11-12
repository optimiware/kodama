//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// defaultGitTimeout bounds one authenticated Git smart-HTTP read.
const defaultGitTimeout = 60 * time.Second

// GitProbe is the e2e-only authenticated Git smart-HTTP reader. It verifies
// TLS against the installation CA (or the system roots), authenticates with a
// private token and never follows a redirect.
type GitProbe struct {
	base    *url.URL
	token   Secret
	client  *http.Client
	timeout time.Duration
}

// NewGitProbe builds one host-side Git reader. The optional CA file is the
// installation trust root; an empty path uses the system roots.
func NewGitProbe(gitURL, caFile string, token Secret) (*GitProbe, error) {
	normalized, err := validateAPIURL(gitURL)
	if err != nil {
		return nil, fmt.Errorf("git URL: %w", err)
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("git URL: %w", err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: base.Hostname()}
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("git CA file: cannot read the CA file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("git CA file: the CA file contains no certificate")
		}
		tlsConfig.RootCAs = pool
	}
	return &GitProbe{
		base:  base,
		token: token,
		client: &http.Client{
			Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout: defaultGitTimeout,
	}, nil
}

// ListRefs performs an authenticated upload-pack advertisement for one
// repository and returns the bounded response body.
func (probe *GitProbe) ListRefs(ctx context.Context, repository string) ([]byte, error) {
	if probe == nil {
		return nil, fmt.Errorf("git probe is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	segments := strings.Split(strings.Trim(repository, "/"), "/")
	for _, segment := range segments {
		if !safePathParameter(segment) {
			return nil, fmt.Errorf("git repository %q is not a bounded path", repository)
		}
	}
	target := *probe.base
	target.Path = path.Join(strings.TrimSuffix(probe.base.Path, "/"), path.Join(segments...), "info", "refs")
	target.RawQuery = "service=git-upload-pack"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build the Git probe request")
	}
	request.Header.Set("Accept", "*/*")
	if probe.token.IsSet() {
		request.SetBasicAuth("kodama", probe.token.Reveal())
	}
	requestCtx := ctx
	if probe.timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, probe.timeout)
		defer cancel()
	}
	response, err := probe.client.Do(request.WithContext(requestCtx))
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, requestCtx.Err()
		}
		return nil, fmt.Errorf("the Git endpoint is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, fmt.Errorf("the Git endpoint redirected; redirects are never followed")
	}
	data, err := readBounded(response.Body, 1<<20)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the Git endpoint refused the authenticated read (status %d)", response.StatusCode)
	}
	return data, nil
}

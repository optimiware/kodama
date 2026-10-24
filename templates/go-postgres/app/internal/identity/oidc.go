// Package identity verifies the application's own OIDC identities.
//
// It establishes who the caller is, nothing more: issuer, signature, audience,
// expiry and a non-empty subject are verified. Role, tenant, group or any other
// claim in the token is deliberately ignored. What the subject may do in which
// application tenant comes only from this application's membership records
// (package tenancy), so a token minted for another system, such as the software
// factory, grants nothing here.
package identity

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrUnauthenticated is returned for a token that fails verification.
var ErrUnauthenticated = errors.New("identity: token verification failed")

// ErrUnavailable is returned when the identity provider cannot be reached.
var ErrUnavailable = errors.New("identity: provider unavailable")

// Principal is a verified subject.
type Principal struct{ Subject string }

// OIDCConfig configures the verifier.
type OIDCConfig struct {
	// Issuer is the exact issuer URL of the application's OIDC provider.
	Issuer string
	// Audience is the audience the access token must carry.
	Audience string
	// AllowInsecureIssuer permits an http issuer on a non-loopback host. Use
	// only in a closed test network; production issuers are https.
	AllowInsecureIssuer bool
	// HTTPClient is used for discovery and key retrieval. Defaults to a client with a timeout.
	HTTPClient *http.Client
}

// OIDCVerifier verifies bearer tokens with go-oidc. Provider discovery is lazy
// and retried, so a provider outage at start-up is reported by readiness rather
// than by a crash loop, while verification itself always fails closed.
type OIDCVerifier struct {
	cfg OIDCConfig
	mu  sync.Mutex
	ver *oidc.IDTokenVerifier
}

// NewOIDCVerifier validates the configuration.
func NewOIDCVerifier(cfg OIDCConfig) (*OIDCVerifier, error) {
	u, err := url.Parse(cfg.Issuer)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("identity: issuer must be an absolute URL without credentials, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !cfg.AllowInsecureIssuer && !loopback(u.Hostname()) {
			return nil, errors.New("identity: an http issuer is only accepted on loopback or with AllowInsecureIssuer")
		}
	default:
		return nil, errors.New("identity: issuer scheme must be https")
	}
	if strings.TrimSpace(cfg.Audience) == "" {
		return nil, errors.New("identity: an audience is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &OIDCVerifier{cfg: cfg}, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (v *OIDCVerifier) verifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ver != nil {
		return v.ver, nil
	}
	dctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, v.cfg.HTTPClient), 10*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(dctx, v.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	v.ver = p.Verifier(&oidc.Config{ClientID: v.cfg.Audience, SupportedSigningAlgs: []string{"RS256", "ES256"}})
	return v.ver, nil
}

// Ready checks fresh discovery independently of cached token verification.
func (v *OIDCVerifier) Ready(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, v.cfg.HTTPClient), 10*time.Second)
	defer cancel()
	if _, err := oidc.NewProvider(dctx, v.cfg.Issuer); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Verify checks a raw bearer token.
func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	ver, err := v.verifier(ctx)
	if err != nil {
		return Principal{}, err
	}
	tok, err := ver.Verify(oidc.ClientContext(ctx, v.cfg.HTTPClient), raw)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
			return Principal{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return Principal{}, ErrUnauthenticated
	}
	if tok.Subject == "" || len(tok.Subject) > 256 {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{Subject: tok.Subject}, nil
}

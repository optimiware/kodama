package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/optimiware/kodama/internal/identity"
)

// Defaults.
const (
	DefaultKeyCacheTTL   = 10 * time.Minute
	DefaultStaleKeyLimit = time.Hour
	DefaultMinRefresh    = 5 * time.Second
	DefaultMaxTokenBytes = 8 << 10
	maxJWKSBytes         = 1 << 20
	httpTimeout          = 10 * time.Second
)

// Config configures a Verifier.
type Config struct {
	// Issuer is the exact issuer identifier; discovery must report the same
	// value. Required.
	Issuer string
	// Audience is the factory API audience every access token must carry. It
	// must differ from the audience of OIDC client ID tokens. Required.
	Audience string
	// AllowInsecureHTTP permits http:// issuer and JWKS URLs. It exists for the
	// local fixture profile only; production configuration never sets it.
	AllowInsecureHTTP bool
	// HTTPClient performs discovery and JWKS requests. Nil selects a client
	// with a 10 s timeout that follows no redirects. Inject it from the
	// composition root.
	HTTPClient *http.Client
	// SigningAlgs lists the accepted JWS algorithms; default RS256 and ES256.
	// "none" and symmetric algorithms are never accepted.
	SigningAlgs []string
	// KeyCacheTTL is how long fetched keys are used before a refresh;
	// StaleKeyLimit bounds how long they may still be used while the issuer is
	// unreachable. MinKeyRefresh rate-limits refreshes triggered by an unknown
	// key ID.
	KeyCacheTTL   time.Duration
	StaleKeyLimit time.Duration
	MinKeyRefresh time.Duration
	// MaxTokenBytes bounds the accepted token size.
	MaxTokenBytes int
	// Clock is the token-expiry clock; nil selects the wall clock.
	Clock func() time.Time
	// AcceptIDTokenClaims disables the rejection of tokens carrying nonce or
	// at_hash. Leave false unless an issuer's access tokens carry them; it never
	// permits a token whose audience is not Audience.
	AcceptIDTokenClaims bool
}

// Verifier verifies bearer access tokens. It implements
// identity.CredentialVerifier.
type Verifier struct {
	cfg    Config
	client *http.Client
	algs   []jose.SignatureAlgorithm

	mu          sync.Mutex
	verifier    *coreoidc.IDTokenVerifier
	discoverErr error
	discoverAt  time.Time
}

var _ identity.CredentialVerifier = (*Verifier)(nil)

// NewVerifier validates the configuration. It performs no network call: the
// issuer is discovered on first use and retried after a failure, so an issuer
// outage at startup is not a startup failure and never opens access.
func NewVerifier(cfg Config) (*Verifier, error) {
	if cfg.Audience == "" {
		return nil, errors.New("oidc: an API audience is required")
	}
	if err := checkURL("issuer", cfg.Issuer, cfg.AllowInsecureHTTP); err != nil {
		return nil, err
	}
	if cfg.KeyCacheTTL == 0 {
		cfg.KeyCacheTTL = DefaultKeyCacheTTL
	}
	if cfg.StaleKeyLimit == 0 {
		cfg.StaleKeyLimit = DefaultStaleKeyLimit
	}
	if cfg.MinKeyRefresh == 0 {
		cfg.MinKeyRefresh = DefaultMinRefresh
	}
	if cfg.MaxTokenBytes == 0 {
		cfg.MaxTokenBytes = DefaultMaxTokenBytes
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	algs := cfg.SigningAlgs
	if len(algs) == 0 {
		algs = []string{"RS256", "ES256"}
	}
	v := &Verifier{cfg: cfg, client: cfg.HTTPClient}
	for _, a := range algs {
		switch jose.SignatureAlgorithm(a) {
		case jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512, jose.PS256, jose.PS384, jose.PS512, jose.EdDSA:
			v.algs = append(v.algs, jose.SignatureAlgorithm(a))
		default:
			return nil, fmt.Errorf("oidc: signing algorithm %q is not supported (asymmetric algorithms only)", a)
		}
	}
	if v.client == nil {
		v.client = &http.Client{Timeout: httpTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return v, nil
}

func checkURL(what, raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("oidc: %s must be an absolute URL without credentials, query or fragment", what)
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return fmt.Errorf("oidc: %s must use https (http only with AllowInsecureHTTP for the development fixture)", what)
	}
	return nil
}

// callState is attached to one Verify call's context so the key set can report
// that a failure was the issuer's, not the credential's.
type callState struct {
	mu          sync.Mutex
	unavailable bool
}

func (s *callState) markUnavailable() { s.mu.Lock(); s.unavailable = true; s.mu.Unlock() }
func (s *callState) isUnavailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unavailable
}

type stateKey struct{}

// Verify checks the token and returns its verified (issuer, subject). A bad
// credential is identity.ErrUnauthenticated; an unreachable issuer, discovery
// failure or JWKS failure is identity.ErrUnavailable. Neither message contains
// the token or a provider response body.
func (v *Verifier) Verify(ctx context.Context, raw string) (identity.Principal, error) {
	tok, err := v.VerifyAccessToken(ctx, raw)
	if err != nil {
		return identity.Principal{}, err
	}
	return tok.Principal, nil
}

// VerifyAccessToken verifies the token and returns the principal together with
// its expiry, granted scopes and optional delegation. The scopes are
// informational for authorization: Kodama authorizes by current membership and
// role, never by an OAuth scope alone.
func (v *Verifier) VerifyAccessToken(ctx context.Context, raw string) (identity.AccessToken, error) {
	if raw == "" || len(raw) > v.cfg.MaxTokenBytes {
		return identity.AccessToken{}, unauthenticated("the token is missing or too large")
	}
	state := &callState{}
	ctx = context.WithValue(ctx, stateKey{}, state)
	verifier, err := v.discover(ctx)
	if err != nil {
		return identity.AccessToken{}, err
	}
	tok, err := verifier.Verify(ctx, raw)
	if err != nil {
		if state.isUnavailable() {
			return identity.AccessToken{}, identity.Unavailablef(nil, "the issuer's signing keys could not be fetched")
		}
		return identity.AccessToken{}, unauthenticated(classify(err))
	}
	claims := map[string]json.RawMessage{}
	if err := tok.Claims(&claims); err != nil {
		return identity.AccessToken{}, unauthenticated("the claims are malformed")
	}
	if !v.cfg.AcceptIDTokenClaims {
		for _, name := range []string{"nonce", "at_hash"} {
			if _, ok := claims[name]; ok {
				return identity.AccessToken{}, unauthenticated("an ID token is not an API access token")
			}
		}
	}
	p := identity.Principal{Issuer: tok.Issuer, Subject: tok.Subject}
	if err := p.Validate(); err != nil {
		return identity.AccessToken{}, unauthenticated("the subject is missing or malformed")
	}
	return identity.AccessToken{
		Principal: p, Expiry: tok.Expiry, Scopes: scopesFromClaims(claims),
		Delegation: delegationFromClaims(claims),
	}, nil
}

func scopesFromClaims(claims map[string]json.RawMessage) []string {
	for _, name := range []string{"scope", "scp"} {
		raw, ok := claims[name]
		if !ok {
			continue
		}
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			return strings.Fields(one)
		}
		var many []string
		if err := json.Unmarshal(raw, &many); err == nil {
			return many
		}
	}
	return nil
}

// delegationFromClaims extracts the issuer-defined intermediary from the RFC
// 8693 act claim. azp/client_id are deliberately not treated as delegation:
// ordinary CLI access tokens can carry them, and marking those as delegated
// would incorrectly remove human-only authority.
func delegationFromClaims(claims map[string]json.RawMessage) *identity.Delegation {
	actor := ""
	if raw, ok := claims["act"]; ok {
		var act struct {
			Sub string `json:"sub"`
		}
		if json.Unmarshal(raw, &act) == nil {
			actor = act.Sub
		}
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > identity.MaxSubjectBytes || !utf8.ValidString(actor) ||
		strings.ContainsFunc(actor, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return nil
	}
	return &identity.Delegation{Actor: actor, Scopes: scopesFromClaims(claims)}
}

func unauthenticated(reason string) error {
	return fmt.Errorf("%w: %s", identity.ErrUnauthenticated, reason)
}

// classify maps go-oidc's detailed failure to a fixed reason; the original error
// text can quote token content and is not propagated.
func classify(err error) string {
	var expired *coreoidc.TokenExpiredError
	if errors.As(err, &expired) {
		return "the token is expired"
	}
	return "the token is not valid for this API"
}

// discover performs (or reuses) issuer discovery and returns the verifier.
func (v *Verifier) discover(ctx context.Context) (*coreoidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verifier != nil {
		return v.verifier, nil
	}
	// While the issuer is down, answer from the last failure for MinKeyRefresh
	// instead of queueing every request behind a fresh discovery attempt.
	if v.discoverErr != nil && time.Since(v.discoverAt) < v.cfg.MinKeyRefresh {
		return nil, v.discoverErr
	}
	dctx := coreoidc.ClientContext(ctx, v.client)
	fail := func(msg string) (*coreoidc.IDTokenVerifier, error) {
		v.discoverErr, v.discoverAt = identity.Unavailablef(nil, "%s", msg), time.Now()
		return nil, v.discoverErr
	}
	provider, err := coreoidc.NewProvider(dctx, v.cfg.Issuer)
	if err != nil {
		return fail("issuer discovery failed")
	}
	var meta struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&meta); err != nil || checkURL("jwks_uri", meta.JWKSURL, v.cfg.AllowInsecureHTTP) != nil {
		return fail("issuer discovery returned no usable signing-key location")
	}
	v.discoverErr = nil
	ks := &keySet{
		url: meta.JWKSURL, client: v.client, algs: v.algs, ttl: v.cfg.KeyCacheTTL, stale: v.cfg.StaleKeyLimit, minRefresh: v.cfg.MinKeyRefresh,
	}
	names := make([]string, len(v.algs))
	for i, a := range v.algs {
		names[i] = string(a)
	}
	v.verifier = coreoidc.NewVerifier(v.cfg.Issuer, ks, &coreoidc.Config{
		ClientID: v.cfg.Audience, SupportedSigningAlgs: names, Now: v.cfg.Clock,
	})
	return v.verifier, nil
}

// keySet is a JWKS cache that reports fetch failures as issuer unavailability.
type keySet struct {
	url        string
	client     *http.Client
	algs       []jose.SignatureAlgorithm
	ttl, stale time.Duration
	minRefresh time.Duration

	mu          sync.Mutex
	set         jose.JSONWebKeySet
	fetchedAt   time.Time
	lastAttempt time.Time
	lastErr     error
}

var _ coreoidc.KeySet = (*keySet)(nil)

// VerifySignature implements coreoidc.KeySet.
func (k *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, k.algs)
	if err != nil {
		return nil, errors.New("unparsable or unsupported signature")
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("exactly one signature is required")
	}
	kid := jws.Signatures[0].Header.KeyID
	keys, err := k.lookup(ctx, kid)
	if err != nil {
		if st, ok := ctx.Value(stateKey{}).(*callState); ok {
			st.markUnavailable()
		}
		return nil, err
	}
	for _, key := range keys {
		if payload, err := jws.Verify(key.Key); err == nil {
			return payload, nil
		}
	}
	return nil, errors.New("no key verifies the signature")
}

func (k *keySet) candidates(kid string) []jose.JSONWebKey {
	var out []jose.JSONWebKey
	if kid != "" {
		for _, key := range k.set.Key(kid) {
			if key.Use == "" || key.Use == "sig" {
				out = append(out, key)
			}
		}
		return out
	}
	for _, key := range k.set.Keys {
		if key.Use == "" || key.Use == "sig" {
			out = append(out, key)
		}
	}
	return out
}

func (k *keySet) lookup(ctx context.Context, kid string) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	if len(k.set.Keys) == 0 || now.Sub(k.fetchedAt) > k.ttl {
		if err := k.refresh(ctx, now); err != nil {
			// Keep serving cached keys through an outage, but only for the
			// stale limit: a removed key must not stay valid indefinitely.
			if len(k.set.Keys) == 0 || now.Sub(k.fetchedAt) > k.stale {
				return nil, err
			}
		}
	}
	if c := k.candidates(kid); len(c) > 0 {
		return c, nil
	}
	// Unknown key ID: the issuer may have rotated. Refresh, rate-limited; while the
	// last fetch failed the failure repeats instead, so an outage is never
	// mistaken for an unknown key.
	if k.lastErr != nil || now.Sub(k.lastAttempt) >= k.minRefresh {
		if err := k.refresh(ctx, now); err != nil {
			return nil, err
		}
	}
	return k.candidates(kid), nil
}

func (k *keySet) refresh(ctx context.Context, now time.Time) (err error) {
	// While the issuer's key endpoint is failing, repeat the last failure for
	// minRefresh instead of queueing every request behind a fresh fetch.
	if k.lastErr != nil && now.Sub(k.lastAttempt) < k.minRefresh {
		return k.lastErr
	}
	k.lastAttempt = now
	defer func() { k.lastErr = err }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return errors.New("signing keys: bad request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return errors.New("signing keys: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signing keys: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil || len(body) > maxJWKSBytes {
		return errors.New("signing keys: unreadable or oversized response")
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil || len(set.Keys) == 0 {
		return errors.New("signing keys: malformed or empty key set")
	}
	k.set, k.fetchedAt = set, now
	return nil
}

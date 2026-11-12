package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
)

// Default fixture claim names and values.
const (
	DefaultAudience     = "kodama-api"
	DefaultTenantClaim  = "https://kodama/tenant"
	DefaultProjectClaim = "https://kodama/project"
	DefaultSubjectClaim = "sub"
	DefaultRoleClaim    = "https://kodama/roles"
)

// DefaultOtherIDTokenClientID is the foreign client id the hostile
// IDTokenAudienceOther mode mints.
const DefaultOtherIDTokenClientID = "another-client"

// DefaultArrayIDTokenAudience is the second audience the array mode adds.
const DefaultArrayIDTokenAudience = "kodama-other-audience"

// IDTokenAudienceMode selects the aud claim of minted ID tokens. OIDC Core
// 3.1.3.7 requires the ID token audience to contain the OAuth client_id; the
// hostile modes exist only to prove the product enforces that rule.
type IDTokenAudienceMode string

const (
	// IDTokenAudienceClient is the default: aud is the string client_id.
	IDTokenAudienceClient IDTokenAudienceMode = ""
	// IDTokenAudienceAPI mints aud as the API audience only.
	IDTokenAudienceAPI IDTokenAudienceMode = "api"
	// IDTokenAudienceOther mints aud as a different client id only.
	IDTokenAudienceOther IDTokenAudienceMode = "other"
	// IDTokenAudienceArray mints aud as [client_id, extra].
	IDTokenAudienceArray IDTokenAudienceMode = "array"
)

// OIDCOptions configures one in-process OIDC provider.
type OIDCOptions struct {
	Clock        *Clock
	Audience     string
	TenantClaim  string
	ProjectClaim string
	SubjectClaim string
	RoleClaim    string
	Algorithms   []string
	Keys         []*SigningKey
	// CacheTTL is copied into the verifier configuration built by
	// VerifierConfig. Zero means the verifier fetches the JWKS on every
	// verification.
	CacheTTL time.Duration
	// IDTokenAudience selects the minted ID token aud claim. The zero value is
	// the RFC-correct string client_id.
	IDTokenAudience IDTokenAudienceMode
	// IDTokenAudienceExtra is the second member of the array mode. Zero means
	// DefaultArrayIDTokenAudience.
	IDTokenAudienceExtra string
}

// RequestRecord is one safe record of an OIDC provider request. It never
// carries an Authorization header, client secret, token or token-endpoint body.
type RequestRecord struct {
	Method    string
	Path      string
	GrantType string
	ClientID  string
	Status    int
}

// RevocationRecord is one safe RFC 7009 revocation observation. The token is
// the refresh token the client asked to revoke; it is kept only so a test can
// assert the exact token was sent and is never rendered by the fixture.
type RevocationRecord struct {
	Token         string
	ClientID      string
	TokenTypeHint string
	Status        int
}

// AuthorizationRequest is the safe subset of one browser authorization request
// handed to a manual-approval hook. It never carries a code or a token.
type AuthorizationRequest struct {
	ClientID      string
	RedirectURI   string
	State         string
	Nonce         string
	CodeChallenge string
	LoginHint     string
}

// refreshFamily is one rotating refresh-token family. Every rotation issues a
// new token in the same family and invalidates the previous one; presenting a
// previously rotated token revokes the whole family.
type refreshFamily struct {
	id       string
	current  string
	clientID string
	revoked  bool
}

// deviceGrant is one RFC 8628 device authorization.
type deviceGrant struct {
	deviceCode string
	userCode   string
	clientID   string
	scopes     []string
	expiresAt  time.Time
	interval   time.Duration
	lastPoll   time.Time
	polled     bool
	approved   bool
	plan       []string
}

// Provider is an in-process deterministic OIDC provider over loopback TLS.
type Provider struct {
	clock                *Clock
	issuer               string
	audience             string
	tenantClaim          string
	projectClaim         string
	subjectClaim         string
	roleClaim            string
	algorithms           []string
	cacheTTL             time.Duration
	idTokenAudience      IDTokenAudienceMode
	idTokenAudienceExtra string

	server *httptest.Server

	mu            sync.Mutex
	keys          map[string]*SigningKey
	order         []string
	rawJWKS       []byte
	jwksStatus    int
	jwksDelay     time.Duration
	jwksMalformed bool
	jwksFetches   int
	tokenStatus   int
	refreshGate   *RefreshGate
	clients       map[string]*oauthClient
	codes         map[string]*grantRecord
	refresh       map[string]*grantRecord
	seenRefresh   map[string]string
	families      map[string]*refreshFamily
	requests      []RequestRecord
	revocations   []RevocationRecord
	logs          []string
	closed        bool

	refreshTTL time.Duration

	autoApprove       bool
	authorizationHook func(AuthorizationRequest) error
	deviceInterval    time.Duration
	deviceExpiresIn   time.Duration
	devicePollPlan    []string
	devices           map[string]*deviceGrant
	deviceOrder       []string
	revocationStatus  int
	hostileTokenCalls int
}

type oauthClient struct {
	id     string
	secret string
	public bool
	claims map[string]any
	ttl    time.Duration
}

type grantRecord struct {
	clientID     string
	redirectURI  string
	claims       map[string]any
	expiresAt    time.Time
	nonce        string
	challenge    string
	viaAuthorize bool
	family       string
}

// NewProvider starts one loopback TLS OIDC provider.
func NewProvider(options OIDCOptions) (*Provider, error) {
	clock := options.Clock
	if clock == nil {
		clock = NewClock(DefaultClockTime())
	}
	audience := options.Audience
	if audience == "" {
		audience = DefaultAudience
	}
	tenantClaim := options.TenantClaim
	if tenantClaim == "" {
		tenantClaim = DefaultTenantClaim
	}
	projectClaim := options.ProjectClaim
	if projectClaim == "" {
		projectClaim = DefaultProjectClaim
	}
	subjectClaim := options.SubjectClaim
	if subjectClaim == "" {
		subjectClaim = DefaultSubjectClaim
	}
	roleClaim := options.RoleClaim
	if roleClaim == "" {
		roleClaim = DefaultRoleClaim
	}
	algorithms := options.Algorithms
	if len(algorithms) == 0 {
		algorithms = []string{"RS256", "ES256"}
	}
	idTokenAudienceExtra := options.IDTokenAudienceExtra
	if idTokenAudienceExtra == "" {
		idTokenAudienceExtra = DefaultArrayIDTokenAudience
	}

	provider := &Provider{
		clock:                clock,
		audience:             audience,
		tenantClaim:          tenantClaim,
		projectClaim:         projectClaim,
		subjectClaim:         subjectClaim,
		roleClaim:            roleClaim,
		algorithms:           append([]string(nil), algorithms...),
		cacheTTL:             options.CacheTTL,
		idTokenAudience:      options.IDTokenAudience,
		idTokenAudienceExtra: idTokenAudienceExtra,
		keys:                 make(map[string]*SigningKey),
		clients:              make(map[string]*oauthClient),
		codes:                make(map[string]*grantRecord),
		refresh:              make(map[string]*grantRecord),
		seenRefresh:          make(map[string]string),
		families:             make(map[string]*refreshFamily),
		refreshTTL:           24 * time.Hour,
		autoApprove:          true,
		deviceInterval:       5 * time.Second,
		deviceExpiresIn:      10 * time.Minute,
		devices:              make(map[string]*deviceGrant),
	}

	keys := options.Keys
	if len(keys) == 0 {
		key, err := NewRSAKey("key-1")
		if err != nil {
			return nil, err
		}
		keys = []*SigningKey{key}
	}
	if err := provider.setKeys(keys...); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", provider.handleDiscovery)
	mux.HandleFunc("/.well-known/jwks.json", provider.handleJWKS)
	mux.HandleFunc("/oauth/token", provider.handleToken)
	mux.HandleFunc("/oauth/token-evil", provider.handleHostileToken)
	mux.HandleFunc("/oauth/authorize", provider.handleAuthorize)
	mux.HandleFunc("/oauth/device", provider.handleDeviceAuthorization)
	mux.HandleFunc("/oauth/revoke", provider.handleRevocation)
	provider.server = httptest.NewTLSServer(mux)
	provider.issuer = provider.server.URL
	return provider, nil
}

// Close stops the fixture server.
func (provider *Provider) Close() {
	if provider == nil || provider.server == nil {
		return
	}
	provider.mu.Lock()
	alreadyClosed := provider.closed
	provider.closed = true
	provider.mu.Unlock()
	if !alreadyClosed {
		provider.server.Close()
	}
}

// Clock returns the fixture clock shared by tokens, JWKS and the verifier.
func (provider *Provider) Clock() *Clock { return provider.clock }

// Issuer is the HTTPS issuer URL of the fixture.
func (provider *Provider) Issuer() string { return provider.issuer }

// Audience is the configured Kodama audience.
func (provider *Provider) Audience() string { return provider.audience }

// TenantClaim returns the configured tenant claim name.
func (provider *Provider) TenantClaim() string { return provider.tenantClaim }

// ProjectClaim returns the configured project claim name.
func (provider *Provider) ProjectClaim() string { return provider.projectClaim }

// SubjectClaim returns the configured subject claim name.
func (provider *Provider) SubjectClaim() string { return provider.subjectClaim }

// RoleClaim returns the configured role claim name.
func (provider *Provider) RoleClaim() string { return provider.roleClaim }

// JWKSURL is the published key-set URL.
func (provider *Provider) JWKSURL() string {
	return provider.issuer + "/.well-known/jwks.json"
}

// AuthorizationEndpoint is the browser authorization endpoint.
func (provider *Provider) AuthorizationEndpoint() string {
	return provider.issuer + "/oauth/authorize"
}

// TokenEndpoint is the token endpoint.
func (provider *Provider) TokenEndpoint() string {
	return provider.issuer + "/oauth/token"
}

// DeviceAuthorizationEndpoint is the RFC 8628 device authorization endpoint.
func (provider *Provider) DeviceAuthorizationEndpoint() string {
	return provider.issuer + "/oauth/device"
}

// RevocationEndpoint is the RFC 7009 revocation endpoint.
func (provider *Provider) RevocationEndpoint() string {
	return provider.issuer + "/oauth/revoke"
}

// CACertPEM returns the fixture certificate as a PEM certificate. A child
// process can trust the fixture through SSL_CERT_FILE.
func (provider *Provider) CACertPEM() []byte {
	if provider == nil || provider.server == nil || provider.server.Certificate() == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.server.Certificate().Raw})
}

// Client returns the TLS client that trusts the fixture certificate.
func (provider *Provider) Client() *http.Client {
	return provider.server.Client()
}

// ClientWithTimeout returns a trusting client with an explicit timeout.
func (provider *Provider) ClientWithTimeout(timeout time.Duration) *http.Client {
	clone := *provider.server.Client()
	clone.Timeout = timeout
	return &clone
}

// VerifierConfig returns the production verifier configuration for this
// fixture. Tests build the real internal/shell/integration/oidc verifier from
// it.
func (provider *Provider) VerifierConfig() shelloidc.Config {
	return shelloidc.Config{
		Issuer:        provider.issuer,
		Audience:      provider.audience,
		JWKSURL:       provider.JWKSURL(),
		Algorithms:    append([]string(nil), provider.algorithms...),
		HTTPClient:    provider.Client(),
		MaxJWKSBytes:  1 << 20,
		MaxTokenBytes: 1 << 20,
		CacheTTL:      provider.cacheTTL,
		Clock:         provider.clock.Now,
		SubjectClaim:  provider.subjectClaim,
		TenantClaim:   provider.tenantClaim,
		ProjectClaim:  provider.projectClaim,
	}
}

// NewVerifier builds the production OIDC verifier against this fixture.
func (provider *Provider) NewVerifier() (shellidentity.OIDCVerifier, error) {
	return shelloidc.NewVerifier(provider.VerifierConfig())
}

// setKeys publishes exactly the supplied keys.
func (provider *Provider) setKeys(keys ...*SigningKey) error {
	next := make(map[string]*SigningKey, len(keys))
	order := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == nil || key.Kid == "" {
			return fmt.Errorf("published signing key is nil or has no id")
		}
		if _, duplicate := next[key.Kid]; duplicate {
			return fmt.Errorf("published signing key id %q is repeated", key.Kid)
		}
		next[key.Kid] = key
		order = append(order, key.Kid)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.keys = next
	provider.order = order
	return nil
}

// AddKey publishes one additional key, keeping the current keys.
func (provider *Provider) AddKey(key *SigningKey) error {
	if key == nil || key.Kid == "" {
		return fmt.Errorf("signing key is nil or has no id")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if _, duplicate := provider.keys[key.Kid]; duplicate {
		return fmt.Errorf("signing key id %q is already published", key.Kid)
	}
	provider.keys[key.Kid] = key
	provider.order = append(provider.order, key.Kid)
	return nil
}

// RetireKey removes one key from the published JWKS. A verifier may still use a
// cached copy until its cache TTL expires.
func (provider *Provider) RetireKey(kid string) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if _, present := provider.keys[kid]; !present {
		return
	}
	delete(provider.keys, kid)
	remaining := provider.order[:0]
	for _, candidate := range provider.order {
		if candidate != kid {
			remaining = append(remaining, candidate)
		}
	}
	provider.order = remaining
}

// PublishedKeys returns the current key ids in publication order.
func (provider *Provider) PublishedKeys() []string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]string(nil), provider.order...)
}

// Key returns one published key by id.
func (provider *Provider) Key(kid string) (*SigningKey, bool) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	key, ok := provider.keys[kid]
	return key, ok
}

func (provider *Provider) signingKey(kid string) (*SigningKey, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	key, ok := provider.keys[kid]
	if !ok {
		return nil, fmt.Errorf("signing key %q is not published", kid)
	}
	return key, nil
}

func (provider *Provider) firstSigningKey() (*SigningKey, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.order) == 0 {
		return nil, fmt.Errorf("no signing key is published")
	}
	return provider.keys[provider.order[0]], nil
}

// SetJWKSFailure makes the JWKS endpoint return status. Zero clears it.
func (provider *Provider) SetJWKSFailure(status int) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.jwksStatus = status
}

// SetJWKSMalformed makes the JWKS endpoint return a malformed document.
func (provider *Provider) SetJWKSMalformed(malformed bool) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.jwksMalformed = malformed
}

// SetRawJWKS serves exactly body from the JWKS endpoint instead of the
// published keys. It lets a test exercise documents the fixture's own
// setKeys/AddKey guards refuse, such as a duplicate kid. A nil body restores
// the published key set.
func (provider *Provider) SetRawJWKS(body []byte) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.rawJWKS = append([]byte(nil), body...)
}

// SetJWKSDelay delays the JWKS response.
func (provider *Provider) SetJWKSDelay(delay time.Duration) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.jwksDelay = delay
}

// JWKSFetchCount returns the number of JWKS requests the fixture served.
func (provider *Provider) JWKSFetchCount() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.jwksFetches
}

// SetTokenEndpointStatus makes the token endpoint fail with status. Zero clears
// it.
func (provider *Provider) SetTokenEndpointStatus(status int) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.tokenStatus = status
}

// RefreshGate deterministically overlaps refresh-token requests. While a gate
// is installed, every refresh_token token request signals Arrived and blocks
// until Release (or the request's own context dies). The fixture bounds the
// wait at refreshGateHold so a forgotten Release cannot hang a child forever.
type RefreshGate struct {
	provider *Provider
	arrived  chan struct{}
	release  chan struct{}
	once     sync.Once
	arrive   sync.Once
}

// refreshGateHold bounds one gated refresh request.
const refreshGateHold = 30 * time.Second

// NewRefreshGate installs and returns a refresh gate. Only the last installed
// gate is active.
func (provider *Provider) NewRefreshGate() *RefreshGate {
	gate := &RefreshGate{
		provider: provider,
		arrived:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	provider.mu.Lock()
	provider.refreshGate = gate
	provider.mu.Unlock()
	return gate
}

// Arrived is closed when at least one refresh request reached the gate.
func (gate *RefreshGate) Arrived() <-chan struct{} { return gate.arrived }

// Release lets every gated request continue. It is idempotent.
func (gate *RefreshGate) Release() {
	if gate == nil {
		return
	}
	gate.once.Do(func() { close(gate.release) })
}

// Close releases the gate and uninstalls it.
func (gate *RefreshGate) Close() {
	if gate == nil {
		return
	}
	gate.Release()
	gate.provider.mu.Lock()
	if gate.provider.refreshGate == gate {
		gate.provider.refreshGate = nil
	}
	gate.provider.mu.Unlock()
}

// waitForRefreshGate blocks one refresh request until the gate is released or
// the bounded hold expires. It reports whether the request may proceed.
func (gate *RefreshGate) waitForRefreshGate(ctx context.Context) bool {
	gate.arrive.Do(func() { close(gate.arrived) })
	timer := time.NewTimer(refreshGateHold)
	defer timer.Stop()
	select {
	case <-gate.release:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// SetAutoApprove enables or disables the automatic authorization approval. A
// manual fixture installs an authorization hook instead.
func (provider *Provider) SetAutoApprove(approve bool) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.autoApprove = approve
}

// SetAuthorizationHook installs the manual-approval hook. It runs without the
// provider lock held and may block until a test approves or denies.
func (provider *Provider) SetAuthorizationHook(hook func(AuthorizationRequest) error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.authorizationHook = hook
}

// SetDeviceInterval sets the RFC 8628 polling interval the fixture advertises.
func (provider *Provider) SetDeviceInterval(interval time.Duration) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.deviceInterval = interval
}

// SetDeviceExpiresIn sets the advertised device authorization lifetime.
func (provider *Provider) SetDeviceExpiresIn(lifetime time.Duration) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.deviceExpiresIn = lifetime
}

// SetDevicePollPlan forces the next token polls to return the supplied RFC 8628
// codes in order. "approved" issues the token. An empty plan restores the
// normal approval and interval logic.
func (provider *Provider) SetDevicePollPlan(plan []string) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.devicePollPlan = append([]string(nil), plan...)
}

// SetRevocationStatus makes the revocation endpoint fail with status.
func (provider *Provider) SetRevocationStatus(status int) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.revocationStatus = status
}

// SetRefreshTokenTTL sets the refresh-token lifetime.
func (provider *Provider) SetRefreshTokenTTL(ttl time.Duration) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.refreshTTL = ttl
}

// PendingUserCodes returns the user codes waiting for device approval.
func (provider *Provider) PendingUserCodes() []string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	codes := make([]string, 0, len(provider.deviceOrder))
	for _, deviceCode := range provider.deviceOrder {
		grant := provider.devices[deviceCode]
		if grant != nil && !grant.approved {
			codes = append(codes, grant.userCode)
		}
	}
	return codes
}

// ApproveDevice approves one pending user code. It reports whether the code was
// found.
func (provider *Provider) ApproveDevice(userCode string) bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	for _, grant := range provider.devices {
		if grant.userCode == userCode {
			grant.approved = true
			return true
		}
	}
	return false
}

// Revocations returns safe records of every revocation request.
func (provider *Provider) Revocations() []RevocationRecord {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]RevocationRecord(nil), provider.revocations...)
}

// HostileTokenCalls returns how many requests the alternate token path served.
// A refresh that re-discovers instead of using the recorded endpoint reaches it.
func (provider *Provider) HostileTokenCalls() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.hostileTokenCalls
}

// RefreshFamilyRevoked reports whether the family owning token was revoked.
func (provider *Provider) RefreshFamilyRevoked(token string) bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	familyID := provider.seenRefresh[token]
	family := provider.families[familyID]
	return familyID != "" && family != nil && family.revoked
}

// Requests returns safe records of every OIDC provider request.
func (provider *Provider) Requests() []RequestRecord {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]RequestRecord(nil), provider.requests...)
}

// LogText returns the fixture's safe log lines.
func (provider *Provider) LogText() string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return strings.Join(provider.logs, "\n")
}

func (provider *Provider) record(method, path, grantType, clientID string, status int) {
	provider.requests = append(provider.requests, RequestRecord{
		Method:    method,
		Path:      path,
		GrantType: grantType,
		ClientID:  clientID,
		Status:    status,
	})
	provider.logs = append(provider.logs, fmt.Sprintf("%s %s grant=%s client=%s status=%d", method, path, grantType, clientID, status))
}

// RegisterClient registers one OAuth client for the token endpoint.
func (provider *Provider) RegisterClient(clientID, secret string, claims map[string]any) error {
	return provider.RegisterClientWithTTL(clientID, secret, claims, time.Hour)
}

// RegisterClientWithTTL registers one OAuth client with an explicit access
// token lifetime.
func (provider *Provider) RegisterClientWithTTL(clientID, secret string, claims map[string]any, ttl time.Duration) error {
	if clientID == "" || secret == "" {
		return fmt.Errorf("OAuth client id and secret are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("OAuth client token lifetime must be positive")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.clients[clientID] = &oauthClient{
		id:     clientID,
		secret: secret,
		claims: cloneClaims(claims),
		ttl:    ttl,
	}
	return nil
}

// RegisterPublicClient registers one public OAuth client without a secret. The
// token endpoint accepts an empty client secret only for a public client, which
// is what the browser authorization-code + PKCE and device flows use.
func (provider *Provider) RegisterPublicClient(clientID string, claims map[string]any, ttl time.Duration) error {
	if clientID == "" {
		return fmt.Errorf("OAuth client id is required")
	}
	if ttl <= 0 {
		return fmt.Errorf("OAuth client token lifetime must be positive")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.clients[clientID] = &oauthClient{
		id:     clientID,
		public: true,
		claims: cloneClaims(claims),
		ttl:    ttl,
	}
	return nil
}

func cloneClaims(claims map[string]any) map[string]any {
	if claims == nil {
		return nil
	}
	cloned := make(map[string]any, len(claims))
	maps.Copy(cloned, claims)
	return cloned
}

// IssueAuthorizationCode seeds one one-time authorization code.
func (provider *Provider) IssueAuthorizationCode(clientID, redirectURI string, claims map[string]any) (string, error) {
	if clientID == "" {
		return "", fmt.Errorf("authorization code client id is empty")
	}
	code, err := randomToken()
	if err != nil {
		return "", err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if _, present := provider.clients[clientID]; !present {
		return "", fmt.Errorf("unknown OAuth client %q", clientID)
	}
	provider.codes[code] = &grantRecord{
		clientID:    clientID,
		redirectURI: redirectURI,
		claims:      cloneClaims(claims),
		expiresAt:   provider.clock.Now().Add(5 * time.Minute),
	}
	return code, nil
}

// IssueRefreshToken seeds one refresh token for a registered client.
func (provider *Provider) IssueRefreshToken(clientID string, claims map[string]any) (string, error) {
	if clientID == "" {
		return "", fmt.Errorf("refresh token client id is empty")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if _, present := provider.clients[clientID]; !present {
		return "", fmt.Errorf("unknown OAuth client %q", clientID)
	}
	return provider.issueRefreshLocked(clientID, claims)
}

func (provider *Provider) issueRefreshLocked(clientID string, claims map[string]any) (string, error) {
	familyID, err := randomToken()
	if err != nil {
		return "", err
	}
	family := &refreshFamily{id: familyID, clientID: clientID}
	token, err := provider.issueRefreshInFamilyLocked(clientID, claims, family)
	if err != nil {
		return "", err
	}
	family.current = token
	provider.families[familyID] = family
	return token, nil
}

// issueRefreshInFamilyLocked mints one rotating refresh token inside family.
func (provider *Provider) issueRefreshInFamilyLocked(clientID string, claims map[string]any, family *refreshFamily) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	ttl := provider.refreshTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	provider.refresh[token] = &grantRecord{
		clientID:  clientID,
		claims:    cloneClaims(claims),
		expiresAt: provider.clock.Now().Add(ttl),
		family:    family.id,
	}
	provider.seenRefresh[token] = family.id
	return token, nil
}

// revokeFamilyLocked invalidates every current token of family.
func (provider *Provider) revokeFamilyLocked(familyID string) {
	family := provider.families[familyID]
	if family == nil {
		return
	}
	family.revoked = true
	family.current = ""
	for token, grant := range provider.refresh {
		if grant.family == familyID {
			delete(provider.refresh, token)
		}
	}
}

func randomToken() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (provider *Provider) handleDiscovery(writer http.ResponseWriter, request *http.Request) {
	provider.mu.Lock()
	provider.record(request.Method, request.URL.Path, "", "", http.StatusOK)
	algorithms := append([]string(nil), provider.algorithms...)
	provider.mu.Unlock()

	document := map[string]any{
		"issuer":                                provider.issuer,
		"authorization_endpoint":                provider.issuer + "/oauth/authorize",
		"token_endpoint":                        provider.issuer + "/oauth/token",
		"jwks_uri":                              provider.JWKSURL(),
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", "client_credentials"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": algorithms,
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"claims_supported":                      []string{"iss", "aud", "sub", "exp", "iat", "nonce", "azp", provider.tenantClaim, provider.projectClaim, provider.roleClaim},
	}
	writeJSON(writer, http.StatusOK, document)
}

func (provider *Provider) handleAuthorize(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	clientID := query.Get("client_id")

	provider.mu.Lock()
	provider.record(request.Method, request.URL.Path, "", clientID, 0)
	provider.mu.Unlock()

	if request.Method != http.MethodGet {
		writeOAuthError(writer, http.StatusMethodNotAllowed, "invalid_request", "the authorization endpoint requires GET")
		return
	}

	redirectURI := query.Get("redirect_uri")
	state := query.Get("state")
	nonce := query.Get("nonce")
	challenge := query.Get("code_challenge")
	challengeMethod := query.Get("code_challenge_method")

	provider.mu.Lock()
	client, clientKnown := provider.clients[clientID]
	autoApprove := provider.autoApprove
	hook := provider.authorizationHook
	requestRecord := AuthorizationRequest{
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		State:         state,
		Nonce:         nonce,
		CodeChallenge: challenge,
		LoginHint:     query.Get("login_hint"),
	}
	provider.mu.Unlock()

	if !clientKnown {
		writeOAuthError(writer, http.StatusBadRequest, "unauthorized_client", "the fixture does not know that client")
		return
	}
	if !validLoopbackRedirect(redirectURI) {
		writeOAuthError(writer, http.StatusBadRequest, "invalid_request", "the redirect_uri is not the exact loopback callback")
		return
	}
	if query.Get("response_type") != "code" {
		writeOAuthError(writer, http.StatusBadRequest, "unsupported_response_type", "the fixture requires response_type=code")
		return
	}
	if challengeMethod != "S256" || !validCodeChallenge(challenge) {
		writeOAuthError(writer, http.StatusBadRequest, "invalid_request", "the fixture requires a PKCE S256 challenge")
		return
	}
	if state == "" || nonce == "" {
		writeOAuthError(writer, http.StatusBadRequest, "invalid_request", "state and nonce are required")
		return
	}

	if !autoApprove {
		if hook == nil {
			writeOAuthError(writer, http.StatusForbidden, "access_denied", "the fixture has no authorization approver")
			return
		}
		if err := hook(requestRecord); err != nil {
			writeOAuthError(writer, http.StatusForbidden, "access_denied", "the authorization was denied")
			return
		}
	}

	claims := cloneClaims(client.claims)
	if requestRecord.LoginHint != "" {
		claims["login_hint"] = requestRecord.LoginHint
	}
	code, err := randomToken()
	if err != nil {
		writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not mint a code")
		return
	}
	provider.mu.Lock()
	provider.codes[code] = &grantRecord{
		clientID:     client.id,
		redirectURI:  redirectURI,
		claims:       claims,
		expiresAt:    provider.clock.Now().Add(5 * time.Minute),
		nonce:        nonce,
		challenge:    challenge,
		viaAuthorize: true,
	}
	provider.mu.Unlock()

	target, err := url.Parse(redirectURI)
	if err != nil {
		writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not redirect")
		return
	}
	values := target.Query()
	values.Set("code", code)
	values.Set("state", state)
	target.RawQuery = values.Encode()
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

// validLoopbackRedirect accepts exactly http://127.0.0.1:<port>/callback.
func validLoopbackRedirect(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.User != nil {
		return false
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" ||
		parsed.Path != "/callback" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return false
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return false
	}
	return true
}

// validCodeChallenge enforces the RFC 7636 S256 challenge alphabet and length.
func validCodeChallenge(challenge string) bool {
	if len(challenge) < 43 || len(challenge) > 128 {
		return false
	}
	for index := 0; index < len(challenge); index++ {
		character := challenge[index]
		switch {
		case character >= 'A' && character <= 'Z':
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-', character == '.', character == '_', character == '~':
		default:
			return false
		}
	}
	return true
}

// pkceChallenge is the fixture's independent RFC 7636 S256 computation.
func pkceChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (provider *Provider) handleDeviceAuthorization(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writeOAuthError(writer, http.StatusBadRequest, "invalid_request", "the device request is malformed")
		return
	}
	clientID := request.PostForm.Get("client_id")

	provider.mu.Lock()
	provider.record(request.Method, request.URL.Path, "device_authorization", clientID, 0)
	client, known := provider.clients[clientID]
	if !known {
		provider.mu.Unlock()
		writeOAuthError(writer, http.StatusBadRequest, "unauthorized_client", "the fixture does not know that client")
		return
	}
	deviceCode, err := randomToken()
	userCode, userErr := randomUserCode()
	if err != nil || userErr != nil {
		provider.mu.Unlock()
		writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not mint a device code")
		return
	}
	interval := provider.deviceInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	lifetime := provider.deviceExpiresIn
	if lifetime <= 0 {
		lifetime = 10 * time.Minute
	}
	grant := &deviceGrant{
		deviceCode: deviceCode,
		userCode:   userCode,
		clientID:   client.id,
		scopes:     strings.Fields(request.PostForm.Get("scope")),
		expiresAt:  provider.clock.Now().Add(lifetime),
		interval:   interval,
		plan:       append([]string(nil), provider.devicePollPlan...),
	}
	provider.devices[deviceCode] = grant
	provider.deviceOrder = append(provider.deviceOrder, deviceCode)
	issuer := provider.issuer
	provider.mu.Unlock()

	writeJSON(writer, http.StatusOK, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          issuer + "/device",
		"verification_uri_complete": issuer + "/device?user_code=" + url.QueryEscape(userCode),
		"expires_in":                int(lifetime / time.Second),
		"interval":                  int(interval / time.Second),
	})
}

// randomUserCode returns one RFC 8628 shaped user code.
func randomUserCode() (string, error) {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X-%X", buffer[:2], buffer[2:]), nil
}

func (provider *Provider) handleRevocation(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writeOAuthError(writer, http.StatusBadRequest, "invalid_request", "the revocation request is malformed")
		return
	}
	token := request.PostForm.Get("token")
	clientID := request.PostForm.Get("client_id")
	hint := request.PostForm.Get("token_type_hint")

	provider.mu.Lock()
	status := provider.revocationStatus
	provider.record(request.Method, request.URL.Path, "revocation", clientID, status)
	provider.revocations = append(provider.revocations, RevocationRecord{
		Token:         token,
		ClientID:      clientID,
		TokenTypeHint: hint,
		Status:        status,
	})
	if status == 0 {
		familyID := ""
		if grant, present := provider.refresh[token]; present {
			familyID = grant.family
		} else if seen := provider.seenRefresh[token]; seen != "" {
			familyID = seen
		}
		if familyID != "" {
			provider.revokeFamilyLocked(familyID)
		}
	}
	provider.mu.Unlock()

	if status != 0 {
		writeOAuthError(writer, status, "temporarily_unavailable", "the fixture revocation endpoint is unavailable")
		return
	}
	// RFC 7009: an unknown or already revoked token is a successful no-op.
	writeJSON(writer, http.StatusOK, map[string]any{})
}

func (provider *Provider) handleHostileToken(writer http.ResponseWriter, request *http.Request) {
	provider.mu.Lock()
	provider.hostileTokenCalls++
	provider.record(request.Method, request.URL.Path, "", "", http.StatusBadRequest)
	provider.mu.Unlock()
	writeOAuthError(writer, http.StatusBadRequest, "invalid_grant", "no token is issued here")
}

func (provider *Provider) handleJWKS(writer http.ResponseWriter, request *http.Request) {
	provider.mu.Lock()
	provider.jwksFetches++
	status := provider.jwksStatus
	delay := provider.jwksDelay
	malformed := provider.jwksMalformed
	raw := append([]byte(nil), provider.rawJWKS...)
	keys := make([]*SigningKey, 0, len(provider.order))
	for _, kid := range provider.order {
		keys = append(keys, provider.keys[kid])
	}
	provider.record(request.Method, request.URL.Path, "", "", status)
	provider.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if status != 0 {
		writer.WriteHeader(status)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if malformed {
		_, _ = io.WriteString(writer, `{"keys":[`)
		return
	}
	if raw != nil {
		_, _ = writer.Write(raw)
		return
	}
	document, err := JWKS(keys...)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = writer.Write(document)
}

// TokenResponse is one OAuth token endpoint response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// ExpiresAt converts the relative lifetime into an absolute fixture-clock time.
func (response TokenResponse) ExpiresAt(now time.Time) time.Time {
	return now.Add(time.Duration(response.ExpiresIn) * time.Second)
}

func (provider *Provider) handleToken(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		provider.mu.Lock()
		provider.record(request.Method, request.URL.Path, "", "", http.StatusBadRequest)
		provider.mu.Unlock()
		writeOAuthError(writer, http.StatusBadRequest, "invalid_request", "the token request is malformed")
		return
	}
	grantType := request.PostForm.Get("grant_type")
	clientID, clientSecret := tokenClientCredentials(request)

	provider.mu.Lock()
	status := provider.tokenStatus
	provider.record(request.Method, request.URL.Path, grantType, clientID, status)
	provider.mu.Unlock()

	if status != 0 {
		writeOAuthError(writer, status, "temporarily_unavailable", "the fixture token endpoint is unavailable")
		return
	}

	if grantType == "refresh_token" {
		provider.mu.Lock()
		gate := provider.refreshGate
		provider.mu.Unlock()
		if gate != nil && !gate.waitForRefreshGate(request.Context()) {
			writeOAuthError(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "the fixture refresh gate was not released")
			return
		}
	}

	switch grantType {
	case "client_credentials":
		provider.handleClientCredentials(writer, clientID, clientSecret)
	case "authorization_code":
		provider.handleAuthorizationCode(writer, request, clientID, clientSecret)
	case "refresh_token":
		provider.handleRefresh(writer, request, clientID, clientSecret)
	case "urn:ietf:params:oauth:grant-type:device_code":
		provider.handleDeviceToken(writer, request, clientID, clientSecret)
	default:
		writeOAuthError(writer, http.StatusBadRequest, "unsupported_grant_type", "the fixture does not support that grant type")
	}
}

func tokenClientCredentials(request *http.Request) (string, string) {
	if clientID, secret, ok := request.BasicAuth(); ok {
		return clientID, secret
	}
	return request.PostForm.Get("client_id"), request.PostForm.Get("client_secret")
}

func (provider *Provider) authenticateClient(clientID, secret string) (*oauthClient, bool) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	client, present := provider.clients[clientID]
	if !present {
		return nil, false
	}
	if client.public {
		if secret != "" {
			return nil, false
		}
		return client, true
	}
	if subtle.ConstantTimeCompare([]byte(client.secret), []byte(secret)) != 1 {
		return nil, false
	}
	return client, true
}

func (provider *Provider) handleClientCredentials(writer http.ResponseWriter, clientID, secret string) {
	client, ok := provider.authenticateClient(clientID, secret)
	if !ok {
		writeOAuthError(writer, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	response, err := provider.mintClientResponse(client, client.claims)
	if err != nil {
		writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not mint a token")
		return
	}
	provider.mu.Lock()
	refresh, err := provider.issueRefreshLocked(client.id, client.claims)
	provider.mu.Unlock()
	if err == nil {
		response.RefreshToken = refresh
	}
	writeJSON(writer, http.StatusOK, response)
}

func (provider *Provider) handleAuthorizationCode(writer http.ResponseWriter, request *http.Request, clientID, secret string) {
	client, ok := provider.authenticateClient(clientID, secret)
	if !ok {
		writeOAuthError(writer, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	code := request.PostForm.Get("code")
	redirectURI := request.PostForm.Get("redirect_uri")
	verifier := request.PostForm.Get("code_verifier")

	provider.mu.Lock()
	grant, present := provider.codes[code]
	if present {
		delete(provider.codes, code)
	}
	expired := present && !provider.clock.Now().Before(grant.expiresAt)
	provider.mu.Unlock()
	if !present || expired || grant.clientID != client.id || (grant.redirectURI != "" && grant.redirectURI != redirectURI) {
		writeOAuthError(writer, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid")
		return
	}
	if grant.viaAuthorize {
		// The browser-issued code is bound to the exact redirect URI and the
		// PKCE S256 challenge the authorization request carried.
		if redirectURI != grant.redirectURI || grant.challenge == "" || verifier == "" ||
			pkceChallenge(verifier) != grant.challenge {
			writeOAuthError(writer, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid")
			return
		}
	}

	response, err := provider.mintClientResponse(client, grant.claims)
	if err != nil {
		writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not mint a token")
		return
	}
	if grant.viaAuthorize {
		idToken, idErr := provider.mintIDToken(client, grant)
		if idErr != nil {
			writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not mint an identity token")
			return
		}
		response.IDToken = idToken
	}
	provider.mu.Lock()
	refresh, err := provider.issueRefreshLocked(client.id, grant.claims)
	provider.mu.Unlock()
	if err == nil {
		response.RefreshToken = refresh
	}
	writeJSON(writer, http.StatusOK, response)
}

// mintIDToken signs the ID token carrying the authorization request's nonce.
// Its aud claim follows OIDC Core 3.1.3.7: the client_id by default, or the
// configured hostile/array shape.
func (provider *Provider) mintIDToken(client *oauthClient, grant *grantRecord) (string, error) {
	token, err := provider.Mint(TokenSpec{
		Audience:  provider.idTokenAudienceFor(client.id),
		Extra:     cloneClaims(grant.claims),
		Nonce:     grant.nonce,
		ExpiresAt: provider.clock.Now().Add(client.ttl),
	})
	if err != nil {
		return "", err
	}
	return token.Value, nil
}

// idTokenAudienceFor returns the configured ID token aud value. The default is
// the string client_id; the hostile modes exist for negative tests only.
func (provider *Provider) idTokenAudienceFor(clientID string) any {
	provider.mu.Lock()
	mode := provider.idTokenAudience
	extra := provider.idTokenAudienceExtra
	audience := provider.audience
	provider.mu.Unlock()
	switch mode {
	case IDTokenAudienceAPI:
		return audience
	case IDTokenAudienceOther:
		return DefaultOtherIDTokenClientID
	case IDTokenAudienceArray:
		if extra == "" {
			extra = DefaultArrayIDTokenAudience
		}
		return []string{clientID, extra}
	default:
		return clientID
	}
}

// SetIDTokenAudience replaces the minted ID token aud shape. It is intended for
// negative tests and must be called before the login starts.
func (provider *Provider) SetIDTokenAudience(mode IDTokenAudienceMode, extra string) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.idTokenAudience = mode
	if extra != "" {
		provider.idTokenAudienceExtra = extra
	}
}

func (provider *Provider) handleRefresh(writer http.ResponseWriter, request *http.Request, clientID, secret string) {
	client, ok := provider.authenticateClient(clientID, secret)
	if !ok {
		writeOAuthError(writer, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	refreshToken := request.PostForm.Get("refresh_token")

	provider.mu.Lock()
	grant, present := provider.refresh[refreshToken]
	familyID := provider.seenRefresh[refreshToken]
	var family *refreshFamily
	if familyID != "" {
		family = provider.families[familyID]
	}
	reused := present && family != nil && family.current != "" && family.current != refreshToken
	expired := present && !provider.clock.Now().Before(grant.expiresAt)
	switch {
	case !present:
		// A token that once belonged to a family but is no longer current is a
		// reuse: revoke the whole family, as RFC 6819 / OAuth security BCP
		// require, and refuse with invalid_grant.
		if familyID != "" {
			provider.revokeFamilyLocked(familyID)
		}
		provider.mu.Unlock()
		writeOAuthError(writer, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	case reused:
		provider.revokeFamilyLocked(family.id)
		provider.mu.Unlock()
		writeOAuthError(writer, http.StatusBadRequest, "invalid_grant", "the refresh token was already used")
		return
	case expired || family == nil || family.revoked || grant.clientID != client.id:
		if family != nil && family.revoked {
			provider.mu.Unlock()
			writeOAuthError(writer, http.StatusBadRequest, "invalid_grant", "the refresh token family is revoked")
			return
		}
		provider.mu.Unlock()
		writeOAuthError(writer, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	}
	delete(provider.refresh, refreshToken)
	rotated, err := provider.issueRefreshInFamilyLocked(client.id, grant.claims, family)
	if err == nil {
		family.current = rotated
	}
	provider.mu.Unlock()
	if err != nil {
		writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not rotate the token")
		return
	}

	response, err := provider.mintClientResponse(client, grant.claims)
	if err != nil {
		writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not mint a token")
		return
	}
	response.RefreshToken = rotated
	writeJSON(writer, http.StatusOK, response)
}

// handleDeviceToken implements one RFC 8628 device token poll.
func (provider *Provider) handleDeviceToken(writer http.ResponseWriter, request *http.Request, clientID, secret string) {
	client, ok := provider.authenticateClient(clientID, secret)
	if !ok {
		writeOAuthError(writer, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	deviceCode := request.PostForm.Get("device_code")

	provider.mu.Lock()
	grant, present := provider.devices[deviceCode]
	if !present || grant.clientID != client.id {
		provider.mu.Unlock()
		writeOAuthError(writer, http.StatusBadRequest, "expired_token", "the device code is unknown")
		return
	}
	now := provider.clock.Now()
	if !now.Before(grant.expiresAt) {
		provider.mu.Unlock()
		writeOAuthError(writer, http.StatusBadRequest, "expired_token", "the device authorization expired")
		return
	}
	if len(grant.plan) > 0 {
		action := grant.plan[0]
		grant.plan = grant.plan[1:]
		switch action {
		case "authorization_pending":
			provider.mu.Unlock()
			writeOAuthError(writer, http.StatusBadRequest, "authorization_pending", "the user has not approved the request")
			return
		case "slow_down":
			provider.mu.Unlock()
			writeOAuthError(writer, http.StatusBadRequest, "slow_down", "the client is polling too fast")
			return
		case "access_denied":
			provider.mu.Unlock()
			writeOAuthError(writer, http.StatusBadRequest, "access_denied", "the user denied the request")
			return
		case "expired_token":
			provider.mu.Unlock()
			writeOAuthError(writer, http.StatusBadRequest, "expired_token", "the device authorization expired")
			return
		case "approved":
			// fall through to mint below
		default:
			provider.mu.Unlock()
			writeOAuthError(writer, http.StatusBadRequest, "authorization_pending", "the user has not approved the request")
			return
		}
	} else {
		if grant.polled {
			elapsed := now.Sub(grant.lastPoll)
			if elapsed < grant.interval {
				grant.lastPoll = now
				provider.mu.Unlock()
				writeOAuthError(writer, http.StatusBadRequest, "slow_down", "the client is polling too fast")
				return
			}
		}
		grant.polled = true
		grant.lastPoll = now
		if !grant.approved {
			provider.mu.Unlock()
			writeOAuthError(writer, http.StatusBadRequest, "authorization_pending", "the user has not approved the request")
			return
		}
	}
	claims := cloneClaims(client.claims)
	provider.mu.Unlock()

	response, err := provider.mintClientResponse(client, claims)
	if err != nil {
		writeOAuthError(writer, http.StatusInternalServerError, "server_error", "the fixture could not mint a token")
		return
	}
	provider.mu.Lock()
	refresh, err := provider.issueRefreshLocked(client.id, claims)
	provider.mu.Unlock()
	if err == nil {
		response.RefreshToken = refresh
	}
	writeJSON(writer, http.StatusOK, response)
}

func (provider *Provider) mintClientResponse(client *oauthClient, claims map[string]any) (TokenResponse, error) {
	now := provider.clock.Now()
	token, err := provider.Mint(TokenSpec{
		Subject:   client.id,
		Extra:     cloneClaims(claims),
		ExpiresAt: now.Add(client.ttl),
	})
	if err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{
		AccessToken: token.Value,
		TokenType:   "Bearer",
		ExpiresIn:   int(client.ttl / time.Second),
	}, nil
}

// ExchangeCode performs one authorization-code exchange against the fixture
// token endpoint.
func (provider *Provider) ExchangeCode(ctx context.Context, code, clientID, secret, redirectURI string) (TokenResponse, error) {
	return provider.exchange(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {secret},
		"redirect_uri":  {redirectURI},
	})
}

// ExchangeRefreshToken performs one refresh-token exchange against the fixture
// token endpoint.
func (provider *Provider) ExchangeRefreshToken(ctx context.Context, refreshToken, clientID, secret string) (TokenResponse, error) {
	return provider.exchange(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {secret},
	})
}

func (provider *Provider) exchange(ctx context.Context, form url.Values) (TokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := provider.Client().Do(request)
	if err != nil {
		return TokenResponse{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return TokenResponse{}, err
	}
	if response.StatusCode != http.StatusOK {
		return TokenResponse{}, fmt.Errorf("token endpoint returned %d", response.StatusCode)
	}
	var decoded TokenResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return TokenResponse{}, fmt.Errorf("decode token response: %w", err)
	}
	if decoded.AccessToken == "" {
		return TokenResponse{}, fmt.Errorf("token response has no access token")
	}
	return decoded, nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeOAuthError(writer http.ResponseWriter, status int, code, description string) {
	writeJSON(writer, status, map[string]string{"error": code, "error_description": description})
}

// requireJSONContentType enforces the public application/json media type.
func requireJSONContentType(request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

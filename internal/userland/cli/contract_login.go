package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/optimiware/kodama/api"
)

// OAuth login declarations: every port, value and fixed constant the kodactl
// login library exchanges is declared here. The behavior lives in the
// login/refresh/logout/pkce/device/idtoken/loopback/session_record files.

// SessionFileVersion is the durable human-session schema version written by a
// fresh login. Version 1 files remain readable as access-token-only sessions.
const SessionFileVersion = 2

// OAuthCallbackPath is the one loopback redirect path the listener serves.
const OAuthCallbackPath = "/callback"

// MaxSessionFileBytes bounds one durable session file. A file of exactly the
// bound is accepted; one byte more is refused.
const MaxSessionFileBytes = 16 << 10

// RefreshLeeway is how close to expiry a session may be before a refresh.
const RefreshLeeway = time.Minute

// ErrReauthenticationRequired reports that the stored session cannot be used
// again: the caller must run a new login instead of replaying an old token.
var ErrReauthenticationRequired = errors.New("reauthentication required")

// LoginMode is one selected login flow.
type LoginMode string

const (
	// LoginAuto lets SelectLoginMode choose a flow from the environment.
	LoginAuto LoginMode = "auto"
	// LoginBrowser is the browser authorization-code + PKCE flow.
	LoginBrowser LoginMode = "browser"
	// LoginDevice is the RFC 8628 device authorization flow.
	LoginDevice LoginMode = "device"
)

// LoginEnvironment is the local evidence SelectLoginMode uses.
type LoginEnvironment struct {
	Display       string
	SSHConnection string
	ForceDevice   bool
	ForceBrowser  bool
}

// BrowserOpener opens one authorization URL in the system browser.
type BrowserOpener interface {
	Open(ctx context.Context, url string) error
}

// CallbackParams is one loopback authorization response. It never carries the
// PKCE verifier or any token.
type CallbackParams struct {
	Code             string
	State            string
	Error            string
	ErrorDescription string
}

// LoopbackListener binds the one loopback callback and returns exactly one
// authorization response. The returned close function must be idempotent.
type LoopbackListener interface {
	Listen(ctx context.Context) (address string, wait func(context.Context) (CallbackParams, error), close func() error, err error)
}

// TokenRequest is one token-endpoint exchange over the injected OIDC port.
// Endpoint is the exact token endpoint the caller discovered (login) or
// recorded in the session (refresh); the port must use exactly it, must not
// re-discover or substitute another endpoint, and must refuse a non-https
// endpoint outside the numeric loopback fixture or an endpoint from a foreign
// origin.
type TokenRequest struct {
	Endpoint     string
	GrantType    string
	Code         string
	RedirectURI  string
	ClientID     string
	CodeVerifier string
	RefreshToken string
	DeviceCode   string
}

// TokenResponse is one token-endpoint result.
type TokenResponse struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	ExpiresIn    time.Duration
}

// DeviceAuthorizationRequest is one RFC 8628 device authorization request.
// Endpoint is the exact discovered device authorization endpoint and follows
// the same port rule as TokenRequest.Endpoint.
type DeviceAuthorizationRequest struct {
	Endpoint string
	ClientID string
	Scopes   []string
}

// DeviceAuthorizationResponse is one RFC 8628 device authorization result.
type DeviceAuthorizationResponse struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       time.Duration
	Interval        time.Duration
}

// RevocationRequest is one RFC 7009 token revocation request. Endpoint is the
// exact revocation endpoint recorded in the session and follows the same port
// rule as TokenRequest.Endpoint.
type RevocationRequest struct {
	Endpoint      string
	Token         string
	ClientID      string
	TokenTypeHint string
}

// OAuthError is one token-endpoint OAuth error response. Its rendered message
// carries only the public code and description, never a token.
type OAuthError struct {
	Code        string
	Description string
}

// OIDCClient is the consumer-owned OAuth/OIDC network port. The login library
// only exchanges values. Every request carries the exact endpoint to use. The
// adapter MUST send the request to exactly that endpoint, MUST NOT
// re-discover, override or fall back to another endpoint, and MUST refuse an
// https downgrade or an endpoint from a foreign origin (the discovery origin
// rule: https, or a numeric loopback fixture).
type OIDCClient interface {
	Discover(ctx context.Context, discoveryURL string) (api.DiscoveryDocument, error)
	Token(ctx context.Context, request TokenRequest) (TokenResponse, error)
	DeviceAuthorize(ctx context.Context, request DeviceAuthorizationRequest) (DeviceAuthorizationResponse, error)
	Revoke(ctx context.Context, request RevocationRequest) error
	JWKS(ctx context.Context, issuer string) (json.RawMessage, error)
}

// Clock is the injected time and sleep boundary.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, duration time.Duration) error
}

// Random is the injected entropy boundary. Production uses crypto/rand.
type Random interface {
	Read(p []byte) (int, error)
}

// SessionLock serializes reload -> refresh -> replace across processes. The
// port is consumer-owned: kodactl supplies the cross-process flock
// implementation.
type SessionLock interface {
	Acquire(ctx context.Context) (release func() error, err error)
}

// SessionRecordStore is the durable v2 session boundary. A missing record is
// reported as an error satisfying errors.Is(err, fs.ErrNotExist).
type SessionRecordStore interface {
	LoadRecord(ctx context.Context) (SessionRecord, error)
	SaveRecord(ctx context.Context, record SessionRecord) error
	DeleteRecord(ctx context.Context) error
}

// LoginDeps is one login invocation. Every external collaborator is injected.
type LoginDeps struct {
	APIURL   string
	Mode     LoginMode
	OIDC     OIDCClient
	Browser  BrowserOpener
	Loopback LoopbackListener
	Random   Random
	Clock    Clock
	Lock     SessionLock
	Store    SessionRecordStore
	Out      io.Writer
	Err      io.Writer
	Timeout  time.Duration
}

// RefreshDeps is one refresh invocation. The expected issuer, client, audience
// and API URL must equal the record's binding before any token call.
type RefreshDeps struct {
	APIURL   string
	Issuer   string
	ClientID string
	Audience string
	OIDC     OIDCClient
	Clock    Clock
	Lock     SessionLock
	Store    SessionRecordStore
}

// LogoutDeps is one logout invocation. The expected issuer, client, audience
// and API URL must equal the record's binding before any revocation call.
type LogoutDeps struct {
	APIURL   string
	Issuer   string
	ClientID string
	Audience string
	OIDC     OIDCClient
	Lock     SessionLock
	Store    SessionRecordStore
}

// IDTokenExpectations is the set of facts one ID token must prove. Audience is
// the expected `aud` member: browser login passes the OAuth client_id because
// the ID token proves the authentication to this client (OIDC Core 3.1.3.7),
// not the API audience the access token binds.
type IDTokenExpectations struct {
	Issuer   string
	Audience string
	Nonce    string
	Now      time.Time
}

// SessionRecord is one durable human session. The access token keeps the
// stable `token` JSON name; refresh_token is additive. A version 1 file is a
// legacy access-token-only session.
type SessionRecord struct {
	Version            int       `json:"version"`
	Kind               string    `json:"kind"`
	Token              string    `json:"token"`
	ExpiresAt          time.Time `json:"expires_at"`
	RefreshToken       string    `json:"refresh_token"`
	Issuer             string    `json:"issuer"`
	ClientID           string    `json:"client_id"`
	Audience           string    `json:"audience"`
	APIURL             string    `json:"api_url"`
	TokenEndpoint      string    `json:"token_endpoint"`
	RevocationEndpoint string    `json:"revocation_endpoint"`
}

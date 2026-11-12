package keys

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// scalewayAPIBase is the Scaleway API origin. The Key Manager decrypt call is
// one authenticated JSON POST, so this client is plain net/http: no provider SDK.
const scalewayAPIBase = "https://api.scaleway.com"

// scalewayMaxResponse bounds one decrypt response. A wrapped 32-byte key and its
// JSON envelope are a few hundred bytes.
const scalewayMaxResponse int64 = 64 << 10

// scalewayRequestTimeout bounds one decrypt request as a whole. The caller's
// context can shorten it further and is always honoured.
const scalewayRequestTimeout = 30 * time.Second

// scalewayMaxSecretKeyBytes bounds the credential value. A Scaleway IAM secret
// key is a UUID (36 bytes); the bound is generous for any supported token shape
// while keeping a misconfigured value from ever becoming an oversized header.
const scalewayMaxSecretKeyBytes = 256

// Explicit Scaleway transport defaults. The client owns its transport so neither
// the process environment nor a caller proxy policy can route a credentialed
// request.
const (
	scalewayDialTimeout           = 10 * time.Second
	scalewayDialKeepAlive         = 30 * time.Second
	scalewayIdleConnTimeout       = 90 * time.Second
	scalewayTLSHandshakeTimeout   = 10 * time.Second
	scalewayExpectContinueTimeout = time.Second
)

// ScalewayKMSConfig configures the Scaleway Key Manager client.
type ScalewayKMSConfig struct {
	// SecretKey is the IAM API secret key of an application whose policy allows
	// only Key Manager decrypt. It is sent as X-Auth-Token and never logged,
	// returned or rendered in an error.
	SecretKey string
	// BaseURL overrides the API origin. Empty selects https://api.scaleway.com.
	// Only an explicit https URL without userinfo, query or fragment is accepted.
	//
	// It exists for tests and for a composition root that is itself trusted: a
	// production composition must never take this value from operator
	// configuration (an operator-selected origin would receive the credential).
	// The client intentionally does not restrict the host to Scaleway; the
	// wiring order must keep BaseURL out of operator configuration, or a later
	// composition change must restrict the host.
	BaseURL string
	// HTTPClient optionally supplies HTTP client settings such as explicit TLS
	// roots. New copies the client, installs a private transport with Proxy=nil
	// (so neither HTTP_PROXY/HTTPS_PROXY nor a caller proxy determines routing)
	// and always refuses redirects; the caller's client is never mutated. Its
	// transport must be an explicit *http.Transport other than the shared
	// http.DefaultTransport. Nil selects an owned transport with verification on.
	HTTPClient *http.Client
}

// ScalewayKMS implements KMSClient over the Key Manager REST API.
type ScalewayKMS struct {
	secretKey string
	baseURL   string
	client    *http.Client
}

var _ KMSClient = (*ScalewayKMS)(nil)

// NewScalewayKMS validates the configuration and returns the client. It makes
// no request and never contacts the Key Manager.
func NewScalewayKMS(config ScalewayKMSConfig) (*ScalewayKMS, error) {
	if !validScalewaySecretKey(config.SecretKey) {
		return nil, fault.New(fault.InvalidInput, "scaleway key manager secret key must be 1 to 256 bytes without a whitespace, control or non-ASCII byte")
	}
	base, err := scalewayBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}

	client := &http.Client{}
	if config.HTTPClient != nil {
		copied := *config.HTTPClient
		client = &copied
	}
	owned, err := scalewayOwnedTransport(client.Transport)
	if err != nil {
		return nil, err
	}
	client.Transport = owned
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Timeout = scalewayRequestTimeout

	return &ScalewayKMS{secretKey: config.SecretKey, baseURL: base, client: client}, nil
}

// validScalewaySecretKey reports whether key is a usable credential value. The
// value becomes an HTTP header field, so it must be 1 to scalewayMaxSecretKeyBytes
// printable ASCII bytes: a control character (CR, LF, NUL, tab, DEL), a space or
// any non-ASCII byte is refused at construction rather than left to net/http's
// later header validation, so header injection fails before any client exists.
// Leading and trailing spaces are refused, not trimmed, so the configured value
// is exactly the value sent. The error names neither the value nor the
// offending byte.
func validScalewaySecretKey(key string) bool {
	if key == "" || len(key) > scalewayMaxSecretKeyBytes {
		return false
	}
	for index := 0; index < len(key); index++ {
		if key[index] <= ' ' || key[index] >= 0x7f {
			return false
		}
	}
	return true
}

// Decrypt unwraps blob with the key named by keyRef ("scw-kms:<region>/<key id>").
// The returned bytes are the decoded plaintext unmodified; the shared cloud
// provider enforces the 32-byte key-encryption-key length. No response body,
// credential or plaintext enters an error.
func (kms *ScalewayKMS) Decrypt(ctx context.Context, keyRef secret.KeyRef, blob []byte) ([]byte, error) {
	// Cancellation is reported as the context error itself, never as a fault.
	// net/http returns the cause inside a *url.Error, and a body-read failure
	// carries transport detail, so both failure paths below re-read the context:
	// fault.ClassOf on the resulting dependency_unavailable fault would misreport
	// a cancellation as a KMS outage. Returning ctx.Err() keeps
	// errors.Is(err, context.Canceled) true while ClassOf reports no fault class,
	// so the caller can tell cancellation from an unavailable dependency. This
	// entry check also keeps a cancelled call from parsing the key reference or
	// building a request.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	region, keyID, err := parseScalewayKeyRef(keyRef)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Ciphertext string `json:"ciphertext"`
	}{base64.StdEncoding.EncodeToString(blob)})
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "scaleway key manager request could not be encoded", err)
	}
	endpoint := kms.baseURL + "/key-manager/v1alpha1/regions/" + url.PathEscape(region) +
		"/keys/" + url.PathEscape(keyID) + "/decrypt"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "scaleway key manager request could not be built", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Token", kms.secretKey)

	response, err := kms.client.Do(request)
	if err != nil {
		// See the entry comment: a cancelled request is cancellation, not an
		// unavailable dependency.
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		// A transport failure can name the URL and its private dial cause; the
		// public fault carries neither, and never a header value.
		return nil, fault.Wrap(fault.DependencyUnavailable, "scaleway key manager is unavailable", err)
	}
	defer func() { _ = response.Body.Close() }()
	// Classify the status before reading anything: the refusal is already
	// decided by the status line, so an error body (which may be large) is
	// never read, bounded, parsed or rendered. Only an accepted 200 body is
	// subject to the response bound.
	switch {
	case response.StatusCode == http.StatusOK:
	case response.StatusCode == http.StatusUnauthorized:
		return nil, fault.New(fault.Unauthenticated, "scaleway key manager rejected the credential")
	case response.StatusCode == http.StatusForbidden:
		return nil, fault.New(fault.Forbidden, "scaleway key manager denied decrypt for this credential")
	case response.StatusCode == http.StatusNotFound:
		return nil, fault.New(fault.NotFound, "scaleway key manager key was not found")
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= http.StatusInternalServerError:
		return nil, fault.New(fault.DependencyUnavailable, "scaleway key manager is unavailable")
	case response.StatusCode >= http.StatusBadRequest && response.StatusCode < http.StatusInternalServerError:
		return nil, fault.New(fault.InvalidInput, "scaleway key manager rejected the request")
	default:
		// 1xx, other 2xx, 3xx and 4xx/5xx handled above: no plaintext is
		// returned for anything but 200, even when the body is valid.
		return nil, fault.New(fault.DependencyUnavailable, "scaleway key manager returned an unexpected status")
	}
	payload, err := readScalewayResponse(response.Body)
	if err != nil {
		// See the entry comment: a body read interrupted by cancellation is
		// cancellation, not an unavailable dependency.
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, err
	}
	var decoded struct {
		Plaintext string `json:"plaintext"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Plaintext == "" {
		return nil, fault.New(fault.DependencyUnavailable, "scaleway key manager returned no plaintext")
	}
	plaintext, err := base64.StdEncoding.DecodeString(decoded.Plaintext)
	if err != nil {
		return nil, fault.New(fault.DependencyUnavailable, "scaleway key manager returned malformed plaintext")
	}
	return plaintext, nil
}

// scalewayBaseURL validates the API origin. The client never downgrades to
// cleartext, never carries an embedded credential, query or fragment, and
// trims only a trailing path separator.
func scalewayBaseURL(value string) (string, error) {
	base := strings.TrimRight(value, "/")
	if base == "" {
		base = scalewayAPIBase
	}
	if strings.ContainsAny(base, " \t\r\n") {
		return "", fault.New(fault.InvalidInput, "scaleway key manager base URL contains whitespace")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fault.New(fault.InvalidInput, "scaleway key manager base URL is not a valid URL")
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fault.New(fault.InvalidInput, "scaleway key manager base URL must be an explicit https URL")
	}
	if parsed.User != nil {
		return "", fault.New(fault.InvalidInput, "scaleway key manager base URL must not carry userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", fault.New(fault.InvalidInput, "scaleway key manager base URL must not carry a query or fragment")
	}
	return base, nil
}

// scalewayOwnedTransport returns the private transport New installs. A nil
// transport gets explicit defaults with no proxy function; a caller-supplied
// *http.Transport is cloned so its explicit TLS, dial and HTTP/2 settings are
// preserved and its proxy is cleared. An opaque RoundTripper, a typed nil and
// the shared http.DefaultTransport are refused rather than silently trusted.
func scalewayOwnedTransport(transport http.RoundTripper) (*http.Transport, error) {
	if transport == nil {
		return newScalewayTransport(), nil
	}
	caller, ok := transport.(*http.Transport)
	if !ok {
		return nil, fault.New(fault.InvalidInput, "scaleway key manager client must use an explicit HTTP transport")
	}
	if caller == nil {
		return nil, fault.New(fault.InvalidInput, "scaleway key manager client supplies a nil HTTP transport")
	}
	if http.DefaultTransport != nil && caller == http.DefaultTransport {
		return nil, fault.New(fault.InvalidInput, "scaleway key manager client must not use the shared default HTTP transport")
	}
	owned := caller.Clone()
	owned.Proxy = nil
	return owned, nil
}

// newScalewayTransport builds the client-owned transport with explicit
// defaults and no proxy function. TLS verification stays at the system default;
// it deliberately does not clone a process-wide transport.
func newScalewayTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   scalewayDialTimeout,
			KeepAlive: scalewayDialKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       scalewayIdleConnTimeout,
		TLSHandshakeTimeout:   scalewayTLSHandshakeTimeout,
		ExpectContinueTimeout: scalewayExpectContinueTimeout,
	}
}

// readScalewayResponse reads at most one byte over the documented bound so an
// oversized body is refused instead of truncated into a parse failure.
func readScalewayResponse(body io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(body, scalewayMaxResponse+1))
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "scaleway key manager response could not be read", err)
	}
	if int64(len(payload)) > scalewayMaxResponse {
		return nil, fault.New(fault.DependencyUnavailable, "scaleway key manager response exceeds the configured bound")
	}
	return payload, nil
}

// parseScalewayKeyRef splits "scw-kms:<region>/<key id>". Both parts are path
// segments of the request, so anything outside a plain identifier is refused.
func parseScalewayKeyRef(keyRef secret.KeyRef) (region, keyID string, err error) {
	rest, ok := strings.CutPrefix(string(keyRef), ScalewayKeyRefPrefix)
	if !ok {
		return "", "", fault.New(fault.InvalidInput, "key reference is not a scaleway key manager reference")
	}
	region, keyID, ok = strings.Cut(rest, "/")
	if !ok || !plainIdentifier(region) || !plainIdentifier(keyID) {
		return "", "", fault.New(fault.InvalidInput, "scaleway key reference must be scw-kms:<region>/<key id>")
	}
	return region, keyID, nil
}

func plainIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

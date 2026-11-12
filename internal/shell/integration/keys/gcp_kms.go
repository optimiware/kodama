package keys

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// gcpKMSEndpoint is the Cloud KMS API origin, which serves every location. The
// decrypt call is one authenticated JSON POST and its bearer token is one GET
// to the metadata server (gcp_metadata.go), so this client is plain net/http:
// no provider SDK and no OAuth library.
const gcpKMSEndpoint = "https://cloudkms.googleapis.com"

// gcpKMSMaxResponse bounds one decrypt response. A wrapped 32-byte key and its
// JSON envelope are a few hundred bytes.
const gcpKMSMaxResponse int64 = 64 << 10

// gcpKMSRequestTimeout bounds one decrypt request as a whole. The caller's
// context can shorten it further and is always honoured.
const gcpKMSRequestTimeout = 30 * time.Second

// Explicit Cloud KMS transport defaults. The client owns its transport so
// neither the process environment nor a caller proxy policy can route a
// request that carries the bearer token.
const (
	gcpKMSDialTimeout           = 10 * time.Second
	gcpKMSDialKeepAlive         = 30 * time.Second
	gcpKMSIdleConnTimeout       = 90 * time.Second
	gcpKMSTLSHandshakeTimeout   = 10 * time.Second
	gcpKMSExpectContinueTimeout = time.Second
)

// GCPKMSConfig configures the Cloud KMS client. The client holds no configured
// credential: it authenticates as the workload identity the GKE metadata
// server issues a token for.
type GCPKMSConfig struct {
	// KMSEndpoint overrides the Cloud KMS origin. Empty selects
	// https://cloudkms.googleapis.com. Only an explicit https URL without
	// userinfo, query or fragment is accepted.
	//
	// It exists for tests and for a composition root that is itself trusted: a
	// production composition must never take this value from operator
	// configuration (an operator-selected origin would receive the bearer
	// token). The client intentionally does not restrict the host to Google;
	// the wiring order must keep KMSEndpoint out of operator configuration, or
	// a later composition change must restrict the host.
	KMSEndpoint string
	// MetadataEndpoint overrides the metadata server origin the bearer token is
	// read from. Empty selects http://169.254.169.254. Only an http origin
	// (no path, userinfo, query or fragment) whose host is a loopback or
	// link-local IP literal, or metadata.google.internal, is accepted. https is
	// refused: the GKE metadata server answers plain HTTP only, so an https
	// origin could only be something else.
	//
	// It exists for tests and for a composition root that is itself trusted: a
	// production composition must never take this value from operator
	// configuration (an operator-selected origin would choose the identity
	// every decrypt runs as). The host restriction keeps the token source on
	// the node; it is not a reason to expose the field.
	MetadataEndpoint string
	// HTTPClient optionally supplies HTTP client settings for the Cloud KMS
	// calls, such as explicit TLS roots. New copies the client, installs a
	// private transport with Proxy=nil (so neither HTTP_PROXY/HTTPS_PROXY nor a
	// caller proxy determines routing) and always refuses redirects; the
	// caller's client is never mutated. Its transport must be an explicit
	// *http.Transport other than the shared http.DefaultTransport. Nil selects
	// an owned transport with verification on. The metadata request never uses
	// this client: it always has its own private transport.
	HTTPClient *http.Client
	// Now optionally supplies the clock that decides when the cached token is
	// refreshed. Nil selects time.Now.
	Now func() time.Time
}

// GCPKMS implements KMSClient over the Cloud KMS REST API.
type GCPKMS struct {
	endpoint string
	client   *http.Client
	tokens   *gcpTokenSource
}

var _ KMSClient = (*GCPKMS)(nil)

// NewGCPKMS validates the configuration and returns the client. It makes no
// request: it contacts neither Cloud KMS nor the metadata server.
func NewGCPKMS(config GCPKMSConfig) (*GCPKMS, error) {
	endpoint, err := gcpKMSOrigin(config.KMSEndpoint)
	if err != nil {
		return nil, err
	}
	tokens, err := newGCPTokenSource(config.MetadataEndpoint, config.Now)
	if err != nil {
		return nil, err
	}

	client := &http.Client{}
	if config.HTTPClient != nil {
		copied := *config.HTTPClient
		client = &copied
	}
	owned, err := gcpKMSOwnedTransport(client.Transport)
	if err != nil {
		return nil, err
	}
	client.Transport = owned
	client.CheckRedirect = gcpRefuseRedirect
	client.Timeout = gcpKMSRequestTimeout

	return &GCPKMS{endpoint: endpoint, client: client, tokens: tokens}, nil
}

// Decrypt unwraps blob with the key named by keyRef
// ("gcp-kms:projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>").
// The returned bytes are the decoded plaintext unmodified; the shared cloud
// provider enforces the 32-byte key-encryption-key length. No response body,
// token, URL, ciphertext or plaintext enters an error.
func (kms *GCPKMS) Decrypt(ctx context.Context, keyRef secret.KeyRef, blob []byte) ([]byte, error) {
	// Cancellation is reported as the context error itself, never as a fault.
	// net/http returns the cause inside a *url.Error, and a body-read failure
	// carries transport detail, so every failure path of the metadata and the
	// decrypt request re-reads the context: fault.ClassOf on the resulting
	// dependency_unavailable fault would misreport a cancellation as an outage.
	// Returning ctx.Err() keeps errors.Is(err, context.Canceled) true while
	// ClassOf reports no fault class. This entry check also keeps a cancelled
	// call from parsing the key reference, reading a token or building a
	// request.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The reference is validated before a token is read: a refused reference
	// costs no I/O at all.
	reference, err := ParseGCPReference(keyRef)
	if err != nil {
		return nil, err
	}
	// The checksum lets Cloud KMS refuse a ciphertext corrupted in transit. The
	// API encodes int64 as a JSON string.
	body, err := json.Marshal(struct {
		Ciphertext       string `json:"ciphertext"`
		CiphertextCRC32C string `json:"ciphertextCrc32c"`
	}{base64.StdEncoding.EncodeToString(blob), strconv.FormatUint(uint64(gcpCRC32C(blob)), 10)})
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "gcp kms request could not be encoded", err)
	}
	// ":decrypt" is the API's custom-method suffix on the key resource and
	// stays literal; only the four identifiers are escaped.
	endpoint := kms.endpoint + "/v1/projects/" + url.PathEscape(reference.Project) +
		"/locations/" + url.PathEscape(reference.Location) +
		"/keyRings/" + url.PathEscape(reference.KeyRing) +
		"/cryptoKeys/" + url.PathEscape(reference.CryptoKey) + ":decrypt"

	plaintext, rejected, err := kms.decryptOnce(ctx, endpoint, body)
	if rejected {
		// Cloud KMS refused the token (expired early or revoked) and
		// decryptOnce dropped it: retry exactly once with a newly read token.
		// A second rejection is returned, never retried.
		plaintext, _, err = kms.decryptOnce(ctx, endpoint, body)
	}
	return plaintext, err
}

// decryptOnce performs one decrypt request with the current token. rejected
// reports a 401, after which the token is no longer cached.
func (kms *GCPKMS) decryptOnce(ctx context.Context, endpoint string, body []byte) (plaintext []byte, rejected bool, err error) {
	token, err := kms.tokens.token(ctx)
	if err != nil {
		return nil, false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, fault.Wrap(fault.InternalFailure, "gcp kms request could not be built", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")

	response, err := kms.client.Do(request)
	if err != nil {
		// See the entry comment of Decrypt: a cancelled request is
		// cancellation, not an unavailable dependency.
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, false, contextErr
		}
		// A transport failure can name the URL and its private dial cause; the
		// public fault carries neither, and never a header value.
		return nil, false, fault.Wrap(fault.DependencyUnavailable, "gcp kms is unavailable", err)
	}
	defer func() { _ = response.Body.Close() }()
	// Classify the status before reading anything: the refusal is already
	// decided by the status line, so an error body (which may be large) is
	// never read, bounded, parsed or rendered. Only an accepted 200 body is
	// subject to the response bound.
	switch {
	case response.StatusCode == http.StatusOK:
	case response.StatusCode == http.StatusUnauthorized:
		kms.tokens.drop(token)
		return nil, true, fault.New(fault.Unauthenticated, "gcp kms rejected the workload identity token")
	case response.StatusCode == http.StatusForbidden:
		return nil, false, fault.New(fault.Forbidden, "gcp kms denied decrypt for this workload identity")
	case response.StatusCode == http.StatusNotFound:
		return nil, false, fault.New(fault.NotFound, "gcp kms key was not found")
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= http.StatusInternalServerError:
		return nil, false, fault.New(fault.DependencyUnavailable, "gcp kms is unavailable")
	case response.StatusCode >= http.StatusBadRequest && response.StatusCode < http.StatusInternalServerError:
		// INVALID_ARGUMENT (a blob this key did not produce) and
		// FAILED_PRECONDITION (a disabled key version) both arrive as 400.
		return nil, false, fault.New(fault.InvalidInput, "gcp kms rejected the request")
	default:
		// 1xx, other 2xx, 3xx and 4xx/5xx handled above: no plaintext is
		// returned for anything but 200, even when the body is valid.
		return nil, false, fault.New(fault.DependencyUnavailable, "gcp kms returned an unexpected status")
	}
	payload, err := gcpReadResponse(response.Body, gcpKMSMaxResponse, "gcp kms")
	if err != nil {
		// See the entry comment of Decrypt: a body read interrupted by
		// cancellation is cancellation, not an unavailable dependency.
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, false, contextErr
		}
		return nil, false, err
	}
	var decoded struct {
		Plaintext       string       `json:"plaintext"`
		PlaintextCRC32C *json.Number `json:"plaintextCrc32c"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Plaintext == "" {
		return nil, false, fault.New(fault.DependencyUnavailable, "gcp kms returned no plaintext")
	}
	plaintext, err = base64.StdEncoding.DecodeString(decoded.Plaintext)
	if err != nil {
		return nil, false, fault.New(fault.DependencyUnavailable, "gcp kms returned malformed plaintext")
	}
	// Cloud KMS reports the checksum of the plaintext it produced (an int64 in
	// a JSON string). A reply that carries one must match it: bytes that fail
	// their own checksum are discarded, never returned.
	if decoded.PlaintextCRC32C != nil {
		checksum, err := strconv.ParseInt(decoded.PlaintextCRC32C.String(), 10, 64)
		if err != nil || checksum != int64(gcpCRC32C(plaintext)) {
			clear(plaintext)
			return nil, false, fault.New(fault.DependencyUnavailable, "gcp kms returned plaintext that fails its checksum")
		}
	}
	return plaintext, false, nil
}

// gcpCRC32C is the CRC32C (Castagnoli) checksum Cloud KMS uses for its
// integrity fields.
func gcpCRC32C(data []byte) uint32 {
	return crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
}

// gcpKMSOrigin validates the Cloud KMS origin. The client never downgrades to
// cleartext, never carries an embedded credential, query or fragment, and
// trims only a trailing path separator.
func gcpKMSOrigin(value string) (string, error) {
	origin := strings.TrimRight(value, "/")
	if origin == "" {
		origin = gcpKMSEndpoint
	}
	parsed, err := gcpParseOrigin(origin, "gcp kms endpoint")
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fault.New(fault.InvalidInput, "gcp kms endpoint must be an explicit https URL")
	}
	return origin, nil
}

// gcpParseOrigin applies the refusals the Cloud KMS and metadata origins
// share: whitespace, an unparsable URL, userinfo, a query and a fragment.
// subject names the origin in the refusal; the value itself is never rendered.
func gcpParseOrigin(origin, subject string) (*url.URL, error) {
	if strings.ContainsAny(origin, " \t\r\n") {
		return nil, fault.New(fault.InvalidInput, subject+" contains whitespace")
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, subject+" is not a valid URL")
	}
	if parsed.User != nil {
		return nil, fault.New(fault.InvalidInput, subject+" must not carry userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, fault.New(fault.InvalidInput, subject+" must not carry a query or fragment")
	}
	return parsed, nil
}

// gcpKMSOwnedTransport returns the private transport New installs for the
// Cloud KMS calls. A nil transport gets explicit defaults with no proxy
// function; a caller-supplied *http.Transport is cloned so its explicit TLS,
// dial and HTTP/2 settings are preserved and its proxy is cleared. An opaque
// RoundTripper, a typed nil and the shared http.DefaultTransport are refused
// rather than silently trusted.
func gcpKMSOwnedTransport(transport http.RoundTripper) (*http.Transport, error) {
	if transport == nil {
		return newGCPKMSTransport(), nil
	}
	caller, ok := transport.(*http.Transport)
	if !ok {
		return nil, fault.New(fault.InvalidInput, "gcp kms client must use an explicit HTTP transport")
	}
	if caller == nil {
		return nil, fault.New(fault.InvalidInput, "gcp kms client supplies a nil HTTP transport")
	}
	if http.DefaultTransport != nil && caller == http.DefaultTransport {
		return nil, fault.New(fault.InvalidInput, "gcp kms client must not use the shared default HTTP transport")
	}
	owned := caller.Clone()
	owned.Proxy = nil
	return owned, nil
}

// newGCPKMSTransport builds the client-owned transport with explicit defaults
// and no proxy function. TLS verification stays at the system default; it
// deliberately does not clone a process-wide transport.
func newGCPKMSTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   gcpKMSDialTimeout,
			KeepAlive: gcpKMSDialKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       gcpKMSIdleConnTimeout,
		TLSHandshakeTimeout:   gcpKMSTLSHandshakeTimeout,
		ExpectContinueTimeout: gcpKMSExpectContinueTimeout,
	}
}

// gcpRefuseRedirect is the redirect policy of both clients: a redirect is
// returned as the response it is, so neither the bearer token nor the token
// request is ever replayed to another location.
func gcpRefuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// gcpReadResponse reads at most one byte over bound so an oversized body is
// refused instead of truncated into a parse failure. subject names the
// responder in the fault.
func gcpReadResponse(body io.Reader, bound int64, subject string) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(body, bound+1))
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, subject+" response could not be read", err)
	}
	if int64(len(payload)) > bound {
		return nil, fault.New(fault.DependencyUnavailable, subject+" response exceeds the configured bound")
	}
	return payload, nil
}

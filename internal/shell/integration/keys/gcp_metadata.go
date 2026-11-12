package keys

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// gcpMetadataEndpoint is the metadata server origin. On GKE with Workload
// Identity Federation the node-local GKE metadata server intercepts this
// address and answers for the pod's Kubernetes service account. The server is
// plain HTTP on a link-local address by design.
//
// The IP literal is used instead of metadata.google.internal, as Google's Go
// metadata client (cloud.google.com/go/compute/metadata) does: it needs no
// resolver, and a name resolved through the pod's cluster DNS and search path
// is one more party that could redirect the token request.
const gcpMetadataEndpoint = "http://169.254.169.254"

// gcpMetadataHost is the documented metadata server name, the only host name a
// metadata override may carry.
const gcpMetadataHost = "metadata.google.internal"

// gcpMetadataTokenPath is the access token of the service account the workload
// runs as.
const gcpMetadataTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"

// gcpMetadataFlavor is the value of the Metadata-Flavor header. The request
// must carry it or the server refuses, and the response must carry it or the
// responder is not the metadata server.
const gcpMetadataFlavor = "Google"

// gcpMetadataMaxResponse bounds one token response. An access token and its
// JSON envelope are one to two kilobytes.
const gcpMetadataMaxResponse int64 = 16 << 10

// The metadata server is on the node, so its bounds are short: a token read
// that cannot connect or does not answer fails fast instead of holding a
// decrypt for the Cloud KMS timeout.
const (
	gcpMetadataRequestTimeout = 10 * time.Second
	gcpMetadataDialTimeout    = 2 * time.Second
)

// gcpTokenExpiryMargin is how long before its expiry a cached token stops
// being used. It is the margin GKE documents for clients that read the
// metadata server directly (3 minutes 45 seconds); the server may hand out a
// cached token that is already close to expiry, and such a token is used for
// the call that read it but never cached.
const gcpTokenExpiryMargin = 3*time.Minute + 45*time.Second

// gcpMaxTokenLifetime caps the lifetime a token response can claim, so an
// absurd expires_in neither overflows nor pins a token in memory. One hour is
// the default lifetime; a longer-lived token is simply read again earlier.
const gcpMaxTokenLifetime = time.Hour

// gcpTokenSource reads the workload's access token from the metadata server
// and caches it in memory until shortly before it expires. It starts no
// goroutine: a token is read by the decrypt that needs it.
type gcpTokenSource struct {
	endpoint string
	client   *http.Client
	now      func() time.Time

	mu      sync.Mutex
	cached  string
	refresh time.Time
}

// newGCPTokenSource validates the metadata origin and builds the token source
// with its own private transport: no proxy function (the token request must
// reach the node directly, whatever HTTP_PROXY says), no redirects, no pooled
// connection between the rare reads and a short whole-request timeout.
func newGCPTokenSource(endpoint string, now func() time.Time) (*gcpTokenSource, error) {
	origin, err := gcpMetadataOrigin(endpoint)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &gcpTokenSource{
		endpoint: origin + gcpMetadataTokenPath,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:             nil,
				DialContext:       (&net.Dialer{Timeout: gcpMetadataDialTimeout}).DialContext,
				DisableKeepAlives: true,
			},
			CheckRedirect: gcpRefuseRedirect,
			Timeout:       gcpMetadataRequestTimeout,
		},
		now: now,
	}, nil
}

// gcpMetadataOrigin validates the metadata origin. It is an http origin with
// no path whose host is a loopback or link-local address, which is never
// routed off the node, or the one documented metadata name; the bearer-token
// source can never be pointed at an arbitrary network host.
func gcpMetadataOrigin(value string) (string, error) {
	origin := strings.TrimRight(value, "/")
	if origin == "" {
		origin = gcpMetadataEndpoint
	}
	parsed, err := gcpParseOrigin(origin, "gcp metadata endpoint")
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" || parsed.Host == "" {
		return "", fault.New(fault.InvalidInput, "gcp metadata endpoint must be an explicit http URL")
	}
	if parsed.Path != "" {
		return "", fault.New(fault.InvalidInput, "gcp metadata endpoint must not carry a path")
	}
	if !gcpMetadataHostAllowed(parsed.Hostname()) {
		return "", fault.New(fault.InvalidInput, "gcp metadata endpoint host must be a loopback or link-local IP literal or metadata.google.internal")
	}
	return origin, nil
}

func gcpMetadataHostAllowed(host string) bool {
	if host == gcpMetadataHost {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && (address.IsLoopback() || address.IsLinkLocalUnicast())
}

// token returns a usable access token, reading one when none is cached or the
// cached one is inside the expiry margin. The lock is held across the read so
// concurrent decrypts read once; a caller waiting behind that read is bounded
// by gcpMetadataRequestTimeout. A failed read caches nothing.
func (source *gcpTokenSource) token(ctx context.Context) (string, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	// The wait for the lock is not interruptible: report a cancellation that
	// arrived during it instead of reading or returning a token.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now := source.now()
	if source.cached != "" && now.Before(source.refresh) {
		return source.cached, nil
	}
	// The token past its margin is forgotten before the read, so a failed read
	// leaves nothing cached.
	source.cached = ""
	token, lifetime, err := source.read(ctx)
	if err != nil {
		return "", err
	}
	// The lifetime is counted from before the request, so the cached token is
	// dropped no later than the margin promises. A token already inside the
	// margin serves this call only.
	if lifetime > gcpTokenExpiryMargin {
		source.cached = token
		source.refresh = now.Add(lifetime - gcpTokenExpiryMargin)
	}
	return token, nil
}

// drop forgets token if it is still the cached one, so a token Cloud KMS
// rejected is never sent again while a newer token read by a concurrent
// decrypt is kept.
func (source *gcpTokenSource) drop(token string) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.cached == token {
		source.cached = ""
	}
}

// read performs one token request. No response body and no token enters an
// error.
func (source *gcpTokenSource) read(ctx context.Context) (string, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.endpoint, nil)
	if err != nil {
		return "", 0, fault.Wrap(fault.InternalFailure, "gcp metadata request could not be built", err)
	}
	request.Header.Set("Metadata-Flavor", gcpMetadataFlavor)

	response, err := source.client.Do(request)
	if err != nil {
		// See the entry comment of Decrypt: a cancelled request is
		// cancellation, not an unavailable dependency.
		if contextErr := ctx.Err(); contextErr != nil {
			return "", 0, contextErr
		}
		return "", 0, fault.Wrap(fault.DependencyUnavailable, "gcp metadata server is unavailable", err)
	}
	defer func() { _ = response.Body.Close() }()
	// As for the decrypt call, the status decides a refusal: an error body is
	// never read, parsed or rendered.
	switch {
	case response.StatusCode == http.StatusOK:
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusNotFound:
		// No service account is bound to the workload (Workload Identity is
		// not configured for it), or the bound one is disabled.
		return "", 0, fault.New(fault.Unauthenticated, "gcp metadata server has no service account token: the workload has no usable identity")
	case response.StatusCode == http.StatusForbidden:
		return "", 0, fault.New(fault.Forbidden, "gcp metadata server denied a service account token: the workload has no usable identity")
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= http.StatusInternalServerError:
		return "", 0, fault.New(fault.DependencyUnavailable, "gcp metadata server is unavailable")
	default:
		return "", 0, fault.New(fault.DependencyUnavailable, "gcp metadata server returned an unexpected status")
	}
	// Anything else that answers on the metadata address (a proxy, a captive
	// service) lacks this header: its token is refused, not sent to Cloud KMS.
	if response.Header.Get("Metadata-Flavor") != gcpMetadataFlavor {
		return "", 0, fault.New(fault.DependencyUnavailable, "gcp metadata response does not identify the metadata server")
	}
	payload, err := gcpReadResponse(response.Body, gcpMetadataMaxResponse, "gcp metadata server")
	if err != nil {
		// See the entry comment of Decrypt: a body read interrupted by
		// cancellation is cancellation, not an unavailable dependency.
		if contextErr := ctx.Err(); contextErr != nil {
			return "", 0, contextErr
		}
		return "", 0, err
	}
	var decoded struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil ||
		!gcpValidAccessToken(decoded.AccessToken) ||
		!strings.EqualFold(decoded.TokenType, "Bearer") ||
		decoded.ExpiresIn <= 0 {
		return "", 0, fault.New(fault.DependencyUnavailable, "gcp metadata server returned no usable access token")
	}
	lifetime := gcpMaxTokenLifetime
	if decoded.ExpiresIn < int64(gcpMaxTokenLifetime/time.Second) {
		lifetime = time.Duration(decoded.ExpiresIn) * time.Second
	}
	return decoded.AccessToken, lifetime, nil
}

// gcpValidAccessToken reports whether token can be a bearer credential. It
// becomes an HTTP header field, so it must be printable ASCII without a space:
// a control character, a space or any non-ASCII byte is refused here rather
// than left to net/http's later header validation. The response bound already
// limits its length.
func gcpValidAccessToken(token string) bool {
	if token == "" {
		return false
	}
	for index := 0; index < len(token); index++ {
		if token[index] <= ' ' || token[index] >= 0x7f {
			return false
		}
	}
	return true
}

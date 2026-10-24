// Package client is the typed HTTP transport shared by every CLI command.
//
// It addresses only the configured factory under /api/v1, attaches a bearer
// credential from a consumer-owned token source, and never follows redirects,
// so a credential cannot reach another destination. It never retries: a
// mutation whose outcome is unknown is resolved by the caller by rereading the
// resource, or by replaying the identical keyed request.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
)

// DefaultMaxResponseBytes bounds a response body when Config leaves it zero.
const DefaultMaxResponseBytes = 4 << 20

// AccessTokenSource supplies the opaque OAuth access token for a request. It
// returns an error wrapping ErrNotAuthenticated when no usable credential is
// held; it never starts an interactive login.
type AccessTokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// Config wires a Client.
type Config struct {
	// BaseURL is the factory origin, for example https://factory.example.invalid.
	BaseURL string
	// AllowInsecureLoopback permits http:// only for loopback hosts, for
	// local development and tests. Any other http:// URL is rejected.
	AllowInsecureLoopback bool
	Tokens                AccessTokenSource
	// HTTP is copied; its redirect policy is replaced. Nil uses a default.
	HTTP             *http.Client
	MaxResponseBytes int64
	// Now is used for HTTP-date retry guidance; nil uses time.Now.
	Now func() time.Time
}

// Client performs scoped API requests.
type Client struct {
	base   *url.URL
	http   http.Client
	tokens AccessTokenSource
	max    int64
	now    func() time.Time
}

// New validates the configuration.
func New(cfg Config) (*Client, error) {
	if cfg.Tokens == nil {
		return nil, errors.New("client: an access token source is required")
	}
	base, err := ParseBaseURL(cfg.BaseURL, cfg.AllowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	c := &Client{base: base, tokens: cfg.Tokens, max: cfg.MaxResponseBytes, now: cfg.Now}
	if cfg.HTTP != nil {
		c.http = *cfg.HTTP
	}
	// A redirect could carry the bearer credential to another destination.
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if c.max <= 0 {
		c.max = DefaultMaxResponseBytes
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// ParseBaseURL validates a factory origin: https (or loopback http when
// allowed), a host, no credentials, query or fragment, and no path.
func ParseBaseURL(raw string, allowInsecureLoopback bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("client: factory must be an absolute URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowInsecureLoopback && IsLoopbackHost(u.Hostname()):
	default:
		return nil, fmt.Errorf("client: factory must use https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("client: factory must be an origin without credentials, path, query or fragment")
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// IsLoopbackHost reports whether host is a literal loopback IP or localhost.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Do sends req. On 2xx it decodes the body into out when out is non-nil. On
// any other status it returns the Response together with a *RemoteError. A
// failure to get a response returns a *TransportError. Do never retries.
func (c *Client) Do(ctx context.Context, req Request, out any) (*Response, error) {
	path, err := req.buildPath()
	if err != nil {
		return nil, err
	}
	if err := req.validateHeaders(); err != nil {
		return nil, err
	}
	var body []byte
	if req.Body != nil {
		if body, err = json.Marshal(req.Body); err != nil {
			return nil, fmt.Errorf("client: encode request body: %w", err)
		}
	}
	token, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n\x00") {
		return nil, fmt.Errorf("%w: credential is empty or malformed", ErrNotAuthenticated)
	}

	// path is already escaped segment by segment. Path must hold the decoded
	// form and RawPath the escaped form, or URL.String escapes it twice
	// ("a b" would reach the server as the literal "a%20b").
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return nil, fmt.Errorf("client: invalid request path: %w", err)
	}
	u := *c.base
	u.Path = decoded
	u.RawPath = path
	u.RawQuery = req.Query.Encode()
	hreq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("client: build request: %w", err)
	}
	if len(body) == 0 {
		hreq.Body, hreq.ContentLength = http.NoBody, 0
	}
	mutation := isMutation(req.Method)
	if mutation {
		// net/http transparently resends a request whose body can be rewound
		// (GetBody) or that carries an Idempotency-Key, after a failure on a
		// reused connection - even when the server already applied it. The
		// client never replays a mutation: a lost answer is reported as
		// uncertain and the caller decides.
		hreq.GetBody = nil
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		hreq.Header.Set("Content-Type", "application/json")
	}
	if req.IfVersion != 0 {
		tag, _ := httpapi.FormatETag(req.IfVersion) // validated above
		hreq.Header.Set("If-Match", tag)
	}
	if req.IdempotencyKey != "" {
		setIdempotencyKey(hreq, req.IdempotencyKey, mutation && len(body) == 0)
	}

	res, err := c.http.Do(hreq)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, &TransportError{Method: req.Method, Path: path, Uncertain: mutation, Err: err}
	}
	defer res.Body.Close()
	raw, err := readBounded(res.Body, c.max)
	if err != nil {
		return nil, &TransportError{Method: req.Method, Path: path, Uncertain: mutation, Err: err}
	}
	resp := newResponse(res, raw, c.now())
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return resp, remoteError(res, resp, mutation)
	}
	if out != nil && len(raw) > 0 {
		if !isJSONContent(res.Header) {
			return resp, &TransportError{Method: req.Method, Path: path, Uncertain: mutation,
				Err: errors.Join(ErrMalformedResponse, errors.New("response is not JSON"))}
		}
		if err := decodeComplete(raw, out); err != nil {
			return resp, &TransportError{Method: req.Method, Path: path, Uncertain: mutation, Err: err}
		}
	} else if out != nil && res.StatusCode != http.StatusNoContent {
		return resp, &TransportError{Method: req.Method, Path: path, Uncertain: mutation,
			Err: errors.Join(ErrMalformedResponse, errors.New("empty response body"))}
	}
	return resp, nil
}

// setIdempotencyKey sends the caller's key unchanged. When a keyed mutation has
// no body, net/http would treat it as replayable on the strength of the
// canonical header name alone (it does an exact-key lookup), so the name is
// written in upper case: header names are case-insensitive on the wire and the
// server sees the same header, but the transport cannot resend the request.
func setIdempotencyKey(hreq *http.Request, key string, hideFromTransport bool) {
	if hideFromTransport {
		hreq.Header[strings.ToUpper(httpapi.IdempotencyKeyHeader)] = []string{key}
		return
	}
	hreq.Header.Set(httpapi.IdempotencyKeyHeader, key)
}

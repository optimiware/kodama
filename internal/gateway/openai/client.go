package openai

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

// Timeouts bound the phases of one provider call.
type Timeouts struct {
	// Connect bounds DNS, TCP and the TLS handshake.
	Connect time.Duration
	// Headers bounds the wait for the response status after the send.
	Headers time.Duration
	// Total bounds the whole call including the stream.
	Total time.Duration
}

// DefaultTimeouts are conservative defaults.
func DefaultTimeouts() Timeouts {
	return Timeouts{Connect: 10 * time.Second, Headers: 60 * time.Second, Total: 10 * time.Minute}
}

// Config configures NewClient. Every bound is explicit constructor input.
type Config struct {
	// Policy approves destinations and resolved addresses. Required.
	Policy *Policy
	// TLS is an optional base configuration (for example private roots). The
	// minimum version is raised to TLS 1.2 and certificate verification can not
	// be disabled through this adapter.
	TLS      *tls.Config
	Timeouts Timeouts
	Stream   StreamLimits
	// UserAgent defaults to "kodama-gateway".
	UserAgent string
}

// Client is the restricted OpenAI-compatible transport. It implements
// gateway.Provider.
//
// It builds each outbound request from validated fields only: the URL comes
// from the run's approved endpoint, the only credential is the run's pinned
// provider key, and no caller header, cookie, URL or member is copied. It uses
// no environment proxy, follows no redirect (same-origin ones included), keeps
// no idle connection between calls, requests an identity encoding, and
// validates every resolved address at every connection before dialing the
// validated address itself, so the TLS server name is still the endpoint's host.
type Client struct {
	policy *Policy
	http   *http.Client
	to     Timeouts
	lim    StreamLimits
	agent  string
}

var _ gateway.Provider = (*Client)(nil)

// NewClient validates the configuration.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Policy == nil {
		return nil, errors.New("openai: a destination policy is required")
	}
	to := cfg.Timeouts
	if to == (Timeouts{}) {
		to = DefaultTimeouts()
	}
	if to.Connect <= 0 || to.Headers <= 0 || to.Total <= 0 {
		return nil, errors.New("openai: timeouts must be positive")
	}
	lim := cfg.Stream
	if lim == (StreamLimits{}) {
		lim = DefaultStreamLimits()
	}
	if !lim.valid() {
		return nil, errors.New("openai: stream limits are out of range")
	}
	tlsCfg := &tls.Config{}
	if cfg.TLS != nil {
		tlsCfg = cfg.TLS.Clone()
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	if tlsCfg.InsecureSkipVerify {
		return nil, errors.New("openai: certificate verification cannot be disabled")
	}
	c := &Client{policy: cfg.Policy, to: to, lim: lim, agent: cfg.UserAgent}
	if c.agent == "" {
		c.agent = "kodama-gateway"
	}
	tr := &http.Transport{
		Proxy:                  nil, // never the process environment's proxy
		DialContext:            c.dial,
		TLSClientConfig:        tlsCfg,
		TLSHandshakeTimeout:    to.Connect,
		ResponseHeaderTimeout:  to.Headers,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		MaxResponseHeaderBytes: 16 << 10,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	c.http = &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

type callKey struct{}

// callState carries one call's approved target in and its dial evidence out.
type callState struct {
	target    dialTarget
	ip        atomic.Value // string
	connected atomic.Bool
}

func (c *Client) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	st, _ := ctx.Value(callKey{}).(*callState)
	host, port, err := net.SplitHostPort(addr)
	if st == nil || err != nil || host != st.target.host || port != st.target.port {
		return nil, &destinationError{what: "a connection to an unapproved target was attempted"}
	}
	addrs, err := c.policy.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: c.to.Connect}
	var last error
	for _, a := range addrs {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err != nil {
			last = err
			continue
		}
		st.ip.Store(a.Unmap().String())
		return conn, nil
	}
	return nil, &connectError{err: last}
}

func bearerHeader(cred *gateway.Credential) (string, error) {
	b := cred.Reveal()
	if len(b) == 0 || len(b) > 4096 {
		return "", gateway.Failf(gateway.ErrCredential, "the credential is not usable")
	}
	for _, x := range b {
		if x < 0x21 || x > 0x7e {
			return "", gateway.Failf(gateway.ErrCredential, "the credential is not usable")
		}
	}
	return "Bearer " + string(b), nil
}

// Open implements gateway.Provider.
func (c *Client) Open(ctx context.Context, call gateway.ProviderCall) (gateway.Stream, error) {
	if call.Endpoint.IsZero() || call.Credential == nil {
		return nil, gateway.Failf(gateway.ErrInvalid, "an endpoint and a credential are required")
	}
	if err := c.policy.Approve(ctx, domain.Scope{}, call.Endpoint); err != nil {
		return nil, &gateway.ProviderError{Kind: gateway.ProviderDestination, Err: err}
	}
	auth, err := bearerHeader(call.Credential)
	if err != nil {
		return nil, err
	}
	body, err := EncodeRequest(call.Completion, call.MaxTokens)
	if err != nil {
		return nil, err
	}
	st := &callState{target: targetOf(call.Endpoint)}
	dest := func() gateway.Destination {
		ip, _ := st.ip.Load().(string)
		return gateway.Destination{Origin: call.Endpoint.Origin(), IP: ip}
	}

	total, cancelTotal := context.WithTimeout(ctx, c.to.Total)
	rctx, cancel := context.WithCancelCause(total)
	stop := func(cause error) { cancel(cause); cancelTotal() }
	rctx = context.WithValue(rctx, callKey{}, st)
	rctx = httptrace.WithClientTrace(rctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { st.connected.Store(true) }})

	u, err := url.Parse(call.Endpoint.ChatURL())
	if err != nil {
		stop(nil)
		return nil, gateway.Failf(gateway.ErrInvalid, "the endpoint is not valid")
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		stop(nil)
		return nil, gateway.Failf(gateway.ErrInvalid, "the request cannot be built")
	}
	req.Header = http.Header{
		"Content-Type":    {"application/json"},
		"Accept":          {"text/event-stream"},
		"Accept-Encoding": {"identity"},
		"Authorization":   {auth},
		"User-Agent":      {c.agent},
	}
	resp, err := c.http.Do(req)
	if err != nil {
		stop(err)
		return nil, c.openError(ctx, err, st, dest())
	}
	fail := func(kind gateway.ProviderFault, status int) error {
		_ = resp.Body.Close()
		stop(nil)
		return &gateway.ProviderError{Kind: kind, Status: status, Sent: true, Destination: dest()}
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fail(gateway.ProviderRedirect, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fail(gateway.ProviderStatus, resp.StatusCode)
	}
	mt, _, merr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if merr != nil || mt != "text/event-stream" {
		return nil, fail(gateway.ProviderProtocol, resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil, fail(gateway.ProviderProtocol, resp.StatusCode)
	}
	return newStream(resp.Body, c.lim, stop, dest()), nil
}

func (c *Client) openError(ctx context.Context, err error, st *callState, dest gateway.Destination) error {
	var de *destinationError
	var ce *connectError
	sent := st.connected.Load()
	switch {
	case errors.As(err, &de):
		return &gateway.ProviderError{Kind: gateway.ProviderDestination, Destination: dest, Err: de}
	case !sent:
		kind := gateway.ProviderConnect
		if errors.As(err, &ce) {
			err = ce
		}
		return &gateway.ProviderError{Kind: kind, Destination: dest, Err: err}
	case ctx.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || isTimeout(err)):
		return &gateway.ProviderError{Kind: gateway.ProviderTimeout, Sent: true, Destination: dest, Err: err}
	}
	return &gateway.ProviderError{Kind: gateway.ProviderProtocol, Sent: true, Destination: dest, Err: err}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// String reports the protocol revision for diagnostics.
func (c *Client) String() string { return fmt.Sprintf("openai client (%s)", ProtocolRevision) }

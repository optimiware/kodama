package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/pkg/netpolicy"
)

// DefaultReceiptHeader is the response header a generic receiver may use to
// return a provider receipt. It is optional: a receiver that omits it is still
// delivered, and no receipt is ever fabricated.
const DefaultReceiptHeader = "X-Receipt-Id"

// Config configures a Client. Policy is required; everything else has a bounded
// default so the zero values are safe.
type Config struct {
	Policy *Policy
	// Credentials resolves recipient-bound credentials. Nil means authenticated
	// destinations fail closed.
	Credentials chatops.CredentialSource
	// RootCAs are the installation's trust anchors; nil uses the system roots.
	// Certificate verification is never disabled.
	RootCAs *x509.CertPool
	// ReceiptHeader names the optional receipt response header. Empty selects
	// DefaultReceiptHeader.
	ReceiptHeader string
	// MaxResponseHeaderBytes and MaxResponseBodyBytes bound what is read from a
	// receiver (defaults 16 KiB and 16 KiB). The body is discarded after the
	// bound, never retained.
	MaxResponseHeaderBytes int64
	MaxResponseBodyBytes   int64
	// DialTimeout and TLSTimeout bound connection setup within the attempt's
	// own deadline (defaults 10 s).
	DialTimeout time.Duration
	TLSTimeout  time.Duration
	UserAgent   string
}

// Client implements chatops.Sender.
type Client struct {
	cfg Config
}

var _ chatops.Sender = (*Client)(nil)

// NewClient validates the configuration.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Policy == nil {
		return nil, errors.New("webhook: a destination policy is required")
	}
	if cfg.ReceiptHeader == "" {
		cfg.ReceiptHeader = DefaultReceiptHeader
	}
	if cfg.MaxResponseHeaderBytes <= 0 {
		cfg.MaxResponseHeaderBytes = 16 << 10
	}
	if cfg.MaxResponseBodyBytes <= 0 {
		cfg.MaxResponseBodyBytes = 16 << 10
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.TLSTimeout <= 0 {
		cfg.TLSTimeout = 10 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "kodama-chatops/1"
	}
	if http.CanonicalHeaderKey(cfg.ReceiptHeader) == "" || strings.ContainsAny(cfg.ReceiptHeader, " \t\r\n:") {
		return nil, errors.New("webhook: the receipt header name is not valid")
	}
	return &Client{cfg: cfg}, nil
}

// Send performs one attempt. The transport is built per send from the verified
// destination scope, so the dial-time address rule (and a scoped private
// allowance, when the installation supplied one) can never leak across tenants.
// Connections are not reused: webhook traffic is low-volume and a fresh,
// policy-checked connection per attempt is the stronger guarantee.
func (c *Client) Send(ctx context.Context, req chatops.SendRequest) chatops.Outcome {
	d := req.Destination
	if err := c.cfg.Policy.Approve(req.Scope, d.Endpoint); err != nil {
		return chatops.Outcome{Class: chatops.ClassPolicyRefused}
	}
	auth, refused := resolveCredential(ctx, c.cfg.Credentials, req)
	if refused != nil {
		return *refused
	}

	dialer := &net.Dialer{Timeout: c.cfg.DialTimeout, Control: c.cfg.Policy.control(req.Scope)}
	transport := &http.Transport{
		Proxy:                  nil, // never the environment proxy: it would bypass the address policy
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.cfg.RootCAs},
		TLSHandshakeTimeout:    c.cfg.TLSTimeout,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: c.cfg.MaxResponseHeaderBytes,
		MaxIdleConns:           1,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var sent atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() { sent.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) },
	}
	httpReq, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, d.Endpoint, bytes.NewReader(req.Payload))
	if err != nil {
		return chatops.Outcome{Class: chatops.ClassPolicyRefused}
	}
	h := httpReq.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, */*;q=0.1")
	h.Set("User-Agent", c.cfg.UserAgent)
	h.Set("Idempotency-Key", req.IdempotencyKey)
	h.Set("Kodama-Delivery-Id", string(req.DeliveryID))
	h.Set("Kodama-Event-Id", req.EventID)
	h.Set("Kodama-Event-Type", req.EventType)
	h.Set("Kodama-Attempt", strconv.Itoa(req.Attempt))
	if auth != "" {
		h.Set("Authorization", auth)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return classifyError(err, sent.Load())
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, c.cfg.MaxResponseBodyBytes) // bounded; never retained

	out := chatops.Outcome{HTTPStatus: resp.StatusCode}
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		out.Delivered = true
		if vs := resp.Header.Values(c.cfg.ReceiptHeader); len(vs) == 1 {
			out.Receipt = chatops.SanitizeReceipt(vs[0])
		}
	case code >= 300 && code < 400:
		out.Class = chatops.ClassRedirectRefused
	case code == http.StatusTooManyRequests:
		out.Class = chatops.ClassRateLimited
		out.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
	case code == http.StatusRequestTimeout || code == http.StatusTooEarly:
		out.Class = chatops.ClassReceiverError
	case code >= 400 && code < 500:
		out.Class = chatops.ClassRejected
	case code >= 500:
		out.Class = chatops.ClassReceiverError
	default:
		out.Class = chatops.ClassBadResponse
		out.Uncertain = true
	}
	return out
}

// classifyError maps a transport error to a bounded class. Raw error text is
// never kept: it can carry the URL or the resolved address.
func classifyError(err error, sent bool) chatops.Outcome {
	var netErr net.Error
	switch {
	case errors.Is(err, netpolicy.ErrAddressNotAllowed):
		return chatops.Outcome{Class: chatops.ClassPolicyRefused}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), errors.As(err, &netErr) && netErr.Timeout():
		return chatops.Outcome{Class: chatops.ClassTimeout, Uncertain: sent}
	case strings.Contains(err.Error(), "response headers exceeded"):
		return chatops.Outcome{Class: chatops.ClassBadResponse, Uncertain: true}
	case sent:
		return chatops.Outcome{Class: chatops.ClassConnectionLost, Uncertain: true}
	}
	return chatops.Outcome{Class: chatops.ClassUnreachable}
}

// retryAfter reads the delta-seconds form only, bounded to one hour; an HTTP
// date or anything else is ignored (the retry policy's own schedule applies).
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	// Clamp the integer before converting: n seconds can overflow a Duration.
	const maxSeconds = int(time.Hour / time.Second)
	return time.Duration(min(n, maxSeconds)) * time.Second
}

package protocol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/mcpproxy"
)

// NewTransport returns an HTTP transport confined to the approved destinations:
//
//   - ambient proxy routing is disabled (no HTTP(S)_PROXY);
//   - only approved host:port pairs are dialed;
//   - the DNS answer is validated and the validated IP is dialed directly, so a
//     second lookup cannot rebind the connection, while TLS still verifies the
//     certificate against the approved host name (http.Transport takes the TLS
//     server name from the URL, not from the dialed address);
//   - TLS 1.2 or later with the given roots (nil selects the system roots).
func NewTransport(d *Destinations, roots *x509.CertPool) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := d.approvedHostPort(addr)
			if err != nil {
				return nil, err
			}
			addrs, err := d.Resolve(ctx, host)
			if err != nil {
				return nil, err
			}
			var last error
			for _, a := range addrs {
				c, err := dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), mustPort(port)).String())
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
		TLSClientConfig:        &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        30 * time.Second,
		ForceAttemptHTTP2:      true,
	}
}

func mustPort(p string) uint16 {
	var n uint16
	for _, r := range p {
		n = n*10 + uint16(r-'0')
	}
	return n
}

// noRedirects makes every redirect a plain response: a credential-bearing
// request is never replayed to another location.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// bearerTransport adds the integration credential to requests for the approved
// origin only. Any other target gets no credential, and inbound credentials and
// cookies are never copied: the transport starts from the SDK's own headers.
type bearerTransport struct {
	base   http.RoundTripper
	origin string
	mu     sync.RWMutex
	token  []byte
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Cookie")
	if !strings.EqualFold(requestOrigin(req.URL), t.origin) {
		return nil, mcpproxy.ErrDestination
	}
	t.mu.RLock()
	if len(t.token) == 0 {
		t.mu.RUnlock()
		return nil, errClosed
	}
	req.Header.Set("Authorization", "Bearer "+string(t.token))
	t.mu.RUnlock()
	return t.base.RoundTrip(req)
}

func requestOrigin(u *url.URL) string { return u.Scheme + "://" + strings.ToLower(u.Host) }

// clear erases the credential copy.
func (t *bearerTransport) clear() {
	t.mu.Lock()
	clear(t.token)
	t.token = nil
	t.mu.Unlock()
}

var errClosed = errors.New("protocol: connection closed")

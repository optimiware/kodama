// Package webhook is the outbound HTTP adapter of ChatOps: the installation's
// destination policy, the recipient-bound credential attachment and the
// bounded client that implements chatops.Sender and chatops.EndpointPolicy.
//
// The client dials directly through net.Dialer with netpolicy.PublicOnly as the
// Control hook, so the address policy is checked on the actual resolved TCP
// destination immediately before connect rather than in a separate DNS
// precheck. It uses no environment proxy and no alternate TLS dialer, verifies
// certificates normally, bounds headers, bodies and deadlines, and never follows
// a redirect. Private destinations are disabled unless an installation supplies
// an explicit, tenant-scoped, exact-address allowance; there is no blanket
// bypass.
package webhook

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/pkg/netpolicy"
)

// PrivateEndpoint is one installation-approved exception to the public-only
// rule: an exact resolved address and port, usable only by one tenant (and
// optionally one project). Loopback and private ranges may be named; the
// unspecified, multicast and link-local ranges (which include cloud metadata
// services) never can. There is no CIDR, wildcard or "allow all" form.
type PrivateEndpoint struct {
	Scope domain.Scope // ProjectID may be empty to cover the tenant's projects
	Addr  netip.AddrPort
}

func (p PrivateEndpoint) validate() error {
	a := p.Addr.Addr().Unmap()
	switch {
	case p.Scope.TenantID.Validate() != nil:
		return errors.New("webhook: a private endpoint names its tenant")
	case p.Scope.ProjectID != "" && p.Scope.ProjectID.Validate() != nil:
		return errors.New("webhook: a private endpoint project is not valid")
	case !p.Addr.IsValid() || p.Addr.Port() == 0 || a.Zone() != "":
		return errors.New("webhook: a private endpoint is an exact address and port")
	case a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast():
		return errors.New("webhook: unspecified, multicast and link-local addresses cannot be allowed")
	}
	return nil
}

// PolicyConfig is the installation's destination policy.
type PolicyConfig struct {
	// Ports lists the permitted destination ports. Empty means 443 only.
	Ports []int
	// Hosts optionally restricts destinations to exact host names or
	// "*.suffix" patterns. Empty permits any host that passes the address
	// policy at dial time.
	Hosts []string
	// PrivateEndpoints are the exact, scoped exceptions described above.
	PrivateEndpoints []PrivateEndpoint
}

// Policy approves destinations and builds the dial-time address check. It
// implements chatops.EndpointPolicy.
type Policy struct {
	ports   []int
	hosts   []string
	private []PrivateEndpoint
}

var _ chatops.EndpointPolicy = (*Policy)(nil)

// NewPolicy validates the configuration.
func NewPolicy(cfg PolicyConfig) (*Policy, error) {
	p := &Policy{ports: slices.Clone(cfg.Ports), hosts: slices.Clone(cfg.Hosts)}
	if len(p.ports) == 0 {
		p.ports = []int{443}
	}
	for _, port := range p.ports {
		if port < 1 || port > 65535 {
			return nil, errors.New("webhook: a policy port is not valid")
		}
	}
	for i, h := range p.hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		bare := strings.TrimPrefix(h, "*.")
		if bare == "" || strings.ContainsAny(bare, "*/:@ ") || strings.HasPrefix(bare, ".") {
			return nil, fmt.Errorf("webhook: policy host %q is not an exact name or *.suffix", h)
		}
		p.hosts[i] = h
	}
	for _, e := range cfg.PrivateEndpoints {
		if err := e.validate(); err != nil {
			return nil, err
		}
		// Normalize mapped addresses so the dial-time comparison is exact.
		e.Addr = netip.AddrPortFrom(e.Addr.Addr().Unmap(), e.Addr.Port())
		p.private = append(p.private, e)
	}
	return p, nil
}

// Approve checks an endpoint: the generic shape (https, no user-info, query or
// fragment), the permitted port, the host allow-list and, for an IP-literal
// host, the address policy. A name is never resolved here (that would be a
// separate DNS precheck); the resolved address is checked at dial time.
func (p *Policy) Approve(scope domain.Scope, endpoint string) error {
	if err := chatops.ValidateEndpoint(endpoint); err != nil {
		return chatops.Fail(chatops.ErrPolicy, err, "the endpoint is not an approved https destination")
	}
	u, _ := url.Parse(endpoint)
	port := 443
	if s := u.Port(); s != "" {
		port, _ = strconv.Atoi(s)
	}
	if !slices.Contains(p.ports, port) {
		return chatops.Fail(chatops.ErrPolicy, nil, "the endpoint port is not permitted")
	}
	host := strings.ToLower(u.Hostname())
	if len(p.hosts) > 0 && !p.hostAllowed(host) {
		return chatops.Fail(chatops.ErrPolicy, nil, "the endpoint host is not approved")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !netpolicy.IsPublic(ip) && !p.privateAllowed(scope, netip.AddrPortFrom(ip.Unmap(), uint16(port))) {
			return chatops.Fail(chatops.ErrPolicy, nil, "the endpoint address is not permitted")
		}
	}
	return nil
}

func (p *Policy) hostAllowed(host string) bool {
	for _, h := range p.hosts {
		if h == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(h, "*"); ok && strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

func (p *Policy) privateAllowed(scope domain.Scope, ap netip.AddrPort) bool {
	for _, e := range p.private {
		if e.Scope.TenantID == scope.TenantID && (e.Scope.ProjectID == "" || e.Scope.ProjectID == scope.ProjectID) && e.Addr == ap {
			return true
		}
	}
	return false
}

// control returns the net.Dialer.Control hook for one tenant scope: the public
// address rule, or an exact scoped private allowance. It runs on every
// resolved address immediately before connect.
func (p *Policy) control(scope domain.Scope) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		err := netpolicy.PublicOnly(network, address, c)
		if err == nil || len(p.private) == 0 {
			return err
		}
		ap, perr := netip.ParseAddrPort(address)
		if perr != nil {
			return netpolicy.ErrAddressNotAllowed
		}
		if p.privateAllowed(scope, netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())) {
			return nil
		}
		return netpolicy.ErrAddressNotAllowed
	}
}

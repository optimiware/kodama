package protocol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/optimiware/kodama/internal/mcpproxy"
)

// Resolver looks up the addresses of a host. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DestinationConfig is the installation's approved-destination policy. It is
// typed configuration with no permissive default: an empty Allowed set approves
// nothing.
type DestinationConfig struct {
	// Allowed lists approved "host" (port 443) or "host:port" entries,
	// compared case-insensitively and exactly. No wildcard exists.
	Allowed []string
	// AllowPrivate permits loopback and private-network addresses for approved
	// hosts. It is an explicit installation decision (for example an in-cluster
	// MCP server). Link-local, metadata, unspecified, multicast and other
	// non-routable ranges stay prohibited regardless.
	AllowPrivate bool
	// Resolver defaults to net.DefaultResolver.
	Resolver Resolver
}

// Destinations implements mcpproxy.Destinations and validates the network
// route of an approved destination.
type Destinations struct {
	allowed      map[string]bool
	allowPrivate bool
	resolver     Resolver
}

var _ mcpproxy.Destinations = (*Destinations)(nil)

// NewDestinations validates the policy.
func NewDestinations(cfg DestinationConfig) (*Destinations, error) {
	d := &Destinations{allowed: map[string]bool{}, allowPrivate: cfg.AllowPrivate, resolver: cfg.Resolver}
	if d.resolver == nil {
		d.resolver = net.DefaultResolver
	}
	for _, a := range cfg.Allowed {
		hp, err := normalizeHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("protocol: invalid approved destination %q: %w", a, err)
		}
		d.allowed[hp] = true
	}
	return d, nil
}

// normalizeHostPort lowercases and defaults the port to 443.
func normalizeHostPort(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || strings.ContainsAny(s, "/@?#* ") {
		return "", errors.New("not a host or host:port")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host, port = strings.Trim(s, "[]"), "443"
		if strings.Contains(host, ":") {
			if _, perr := netip.ParseAddr(host); perr != nil {
				return "", errors.New("not a host or host:port")
			}
		} else if strings.Contains(s, ":") {
			return "", errors.New("not a host or host:port")
		}
	}
	if host == "" || port == "" || port == "0" {
		return "", errors.New("not a host or host:port")
	}
	return net.JoinHostPort(host, port), nil
}

// hostPortOf returns the normalized host:port of a URL.
func hostPortOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// Approve implements mcpproxy.Destinations: the endpoint must be a normalized
// https URL whose host and port are approved. It performs no network access.
func (d *Destinations) Approve(endpoint string) error {
	_, u, err := mcpproxy.NormalizeEndpoint(endpoint)
	if err != nil {
		return mcpproxy.ErrDestination
	}
	if !d.allowed[hostPortOf(u)] {
		return mcpproxy.ErrDestination
	}
	return nil
}

// approvedHostPort checks a dial target.
func (d *Destinations) approvedHostPort(hostport string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(strings.ToLower(hostport))
	if err != nil || !d.allowed[net.JoinHostPort(host, port)] {
		return "", "", mcpproxy.ErrDestination
	}
	return host, port, nil
}

// permitted reports whether an address may be dialed for an approved host.
func (d *Destinations) permitted(a netip.Addr) bool {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsUnspecified(), a.IsMulticast(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsInterfaceLocalMulticast():
		return false // includes the 169.254.169.254 metadata address and fe80::/10
	}
	for _, p := range alwaysProhibited {
		if p.Contains(a) {
			return false
		}
	}
	if a.IsLoopback() || a.IsPrivate() {
		return d.allowPrivate
	}
	for _, p := range sharedAddressSpace {
		if p.Contains(a) {
			return d.allowPrivate
		}
	}
	return true
}

var (
	alwaysProhibited = mustPrefixes("0.0.0.0/8", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"240.0.0.0/4", "64:ff9b::/96", "100::/64", "2001::/32", "2001:db8::/32", "2002::/16", "169.254.0.0/16", "fe80::/10")
	// sharedAddressSpace is carrier-grade NAT, treated like private networks.
	sharedAddressSpace = mustPrefixes("100.64.0.0/10")
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Resolve returns the addresses of an approved host after validating every
// answer: if any one is prohibited the whole answer is rejected, so a mixed
// public/internal answer (DNS rebinding) cannot select the internal address.
func (d *Destinations) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		var lerr error
		addrs, lerr = d.resolver.LookupNetIP(ctx, "ip", host)
		if lerr != nil {
			return nil, fmt.Errorf("protocol: resolving the approved host: %w", lerr)
		}
	}
	if len(addrs) == 0 {
		return nil, mcpproxy.ErrDestination
	}
	for _, a := range addrs {
		if !d.permitted(a) {
			return nil, mcpproxy.ErrDestination
		}
	}
	return addrs, nil
}

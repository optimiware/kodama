package openai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

// ApprovedOrigin is one installation-approved provider origin and the path
// prefix below which endpoints may point. The approval is installation
// configuration supplied to the constructor; there is no environment lookup and
// no wildcard.
type ApprovedOrigin struct {
	// Origin is "https://host" or "https://host:port".
	Origin string
	// PathPrefix is "" (any path) or a prefix such as "/v1" that the endpoint
	// path must equal or sit below.
	PathPrefix string
}

// Resolver resolves a host name. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// AddressPolicy decides whether a resolved address may be connected to.
type AddressPolicy func(netip.Addr) bool

// PublicOnly denies every address that is not publicly routable unicast:
// loopback, private, link-local (including the cloud metadata address),
// carrier-grade NAT, multicast, unspecified, reserved and benchmarking ranges.
// IPv4-mapped IPv6 addresses are judged as their IPv4 form.
func PublicOnly(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range deniedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var deniedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"255.255.255.255/32", "64:ff9b::/96", "2001:db8::/32", "2002::/16", "fec0::/10",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// AllowPrefixes extends an AddressPolicy with explicitly allowed networks (a
// private provider endpoint on an operator's network, or a loopback test
// fixture). The allowance is still an exact, explicit list.
func AllowPrefixes(base AddressPolicy, prefixes ...netip.Prefix) AddressPolicy {
	return func(a netip.Addr) bool {
		for _, p := range prefixes {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return base(a)
	}
}

// Policy is the destination approval of the transport: the approved origins and
// the address policy applied to every resolution.
type Policy struct {
	origins   []ApprovedOrigin
	addresses AddressPolicy
	resolver  Resolver
}

// PolicyConfig configures NewPolicy.
type PolicyConfig struct {
	Origins []ApprovedOrigin
	// Addresses defaults to PublicOnly.
	Addresses AddressPolicy
	// Resolver defaults to net.DefaultResolver.
	Resolver Resolver
}

// NewPolicy validates the approved origins. At least one is required: an empty
// policy approves nothing.
func NewPolicy(cfg PolicyConfig) (*Policy, error) {
	if len(cfg.Origins) == 0 {
		return nil, errors.New("openai: at least one approved origin is required")
	}
	p := &Policy{addresses: cfg.Addresses, resolver: cfg.Resolver}
	if p.addresses == nil {
		p.addresses = PublicOnly
	}
	if p.resolver == nil {
		p.resolver = net.DefaultResolver
	}
	for _, o := range cfg.Origins {
		ep, err := gateway.ParseEndpoint(o.Origin + o.PathPrefix)
		if err != nil || ep.Origin() != o.Origin || ep.Path() != o.PathPrefix {
			return nil, fmt.Errorf("openai: approved origin %q is not a normalized https origin with an optional path prefix", o.Origin)
		}
		p.origins = append(p.origins, o)
	}
	return p, nil
}

var _ gateway.Destinations = (*Policy)(nil)

// Approve implements gateway.Destinations: the endpoint's origin must be
// approved and its path must sit below the approved prefix.
func (p *Policy) Approve(_ context.Context, _ domain.Scope, e gateway.Endpoint) error {
	if p == nil {
		return gateway.Failf(gateway.ErrDestination, "no destination policy is configured")
	}
	for _, o := range p.origins {
		if o.Origin != e.Origin() {
			continue
		}
		if o.PathPrefix == "" || e.Path() == o.PathPrefix || strings.HasPrefix(e.Path(), o.PathPrefix+"/") {
			return nil
		}
	}
	return gateway.Failf(gateway.ErrDestination, "the endpoint is not an approved destination")
}

// resolve returns the addresses of host after policy validation. It is called
// for every connection: a name that resolved to an approved address earlier
// and to a forbidden one now (DNS rebinding) is refused, and so is an answer
// that mixes approved and forbidden addresses.
func (p *Policy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		var lerr error
		addrs, lerr = p.resolver.LookupNetIP(ctx, "ip", host)
		if lerr != nil {
			return nil, &connectError{err: lerr}
		}
	}
	if len(addrs) == 0 {
		return nil, &connectError{err: errors.New("no addresses")}
	}
	for _, a := range addrs {
		if !p.addresses(a) {
			return nil, &destinationError{what: "an address of the destination is not approved"}
		}
	}
	return addrs, nil
}

type destinationError struct{ what string }

func (e *destinationError) Error() string { return "openai: " + e.what }

type connectError struct{ err error }

func (e *connectError) Error() string { return "openai: connect: " + e.err.Error() }
func (e *connectError) Unwrap() error { return e.err }

// dialTarget is the origin host and port a call may connect to.
type dialTarget struct{ host, port string }

func targetOf(e gateway.Endpoint) dialTarget {
	return dialTarget{host: e.Host(), port: strconv.Itoa(e.Port())}
}

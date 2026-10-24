// Copyright 2023 Harness, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Modified for Kodama: retain only public-address classification and a TCP
// dial hook; omit resolver prechecks, configurable exceptions and error mapping.
// Tighten IPv6 special-use filtering. See README.md for the upstream revision.

// Package netpolicy supplies a conservative public-address policy for outbound
// TCP connections. It does not authorize URLs, redirects, proxies or credentials.
package netpolicy

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

// ErrAddressNotAllowed is returned when a destination fails the address policy.
var ErrAddressNotAllowed = errors.New("network address is not allowed")

var ipv6GlobalUnicast = netip.MustParsePrefix("2000::/3")

// Special-use ranges not excluded by netip's unicast/private predicates.
// This is deliberately conservative, not a routability or availability test.
var reservedPrefixes = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, including broadcast
	netip.MustParsePrefix("2001::/23"),       // IETF special use, including Teredo
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4 can embed private IPv4
	netip.MustParsePrefix("3fff::/20"),       // documentation
}

// IsPublic reports whether addr passes the public-unicast address policy.
// IPv4-mapped IPv6 addresses are checked as IPv4. Scoped addresses, private
// networks, special-use ranges and IPv6 outside 2000::/3 are refused.
func IsPublic(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	if addr.Is6() && !ipv6GlobalUnicast.Contains(addr) {
		return false
	}
	for _, prefix := range reservedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// PublicOnly implements net.Dialer.Control. It checks each resolved address
// immediately before connect, rather than resolving a name in a separate
// preflight check. Unresolved names and non-TCP networks are refused.
//
// Use with a direct transport: a proxy or custom DialTLSContext can bypass the
// destination check. URL approval, redirect policy and credential audience
// remain responsibilities of the consuming adapter.
func PublicOnly(network, address string, _ syscall.RawConn) error {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return ErrAddressNotAllowed
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrAddressNotAllowed
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !IsPublic(addr) {
		return ErrAddressNotAllowed
	}
	return nil
}

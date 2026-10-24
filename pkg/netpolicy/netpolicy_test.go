package netpolicy_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/pkg/netpolicy"
)

func TestPublicAddresses(t *testing.T) {
	for _, raw := range []string{"1.1.1.1", "140.82.121.4", "::ffff:1.1.1.1", "2606:4700::1", "2001:4860:4860::8888"} {
		t.Run(raw, func(t *testing.T) {
			addr := netip.MustParseAddr(raw)
			if !netpolicy.IsPublic(addr) {
				t.Fatal("public address refused")
			}
			if err := netpolicy.PublicOnly("tcp", net.JoinHostPort(raw, "443"), nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNonPublicAddresses(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "0.1.2.3", "10.0.0.1", "100.64.0.1", "127.0.0.1",
		"169.254.169.254", "172.16.0.1", "192.168.1.1", "192.0.0.1", "192.0.2.1",
		"192.88.99.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::127.0.0.1", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
		"::ffff:169.254.169.254", "::ffff:192.0.2.1", "::ffff:224.0.0.1",
		"fc00::1", "fd00:ec2::254", "fe80::1", "fe80::1%en0", "ff02::1",
		"100::1", "2001::1", "2001:2::1", "2001:20::1", "2001:db8::1",
		"2002:a9fe:a9fe::1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::1",
		"3fff::1", "5f00::1", "2606:4700::1%en0",
	} {
		t.Run(raw, func(t *testing.T) {
			if netpolicy.IsPublic(netip.MustParseAddr(raw)) {
				t.Fatal("non-public address accepted")
			}
			if err := netpolicy.PublicOnly("tcp", net.JoinHostPort(raw, "443"), nil); !errors.Is(err, netpolicy.ErrAddressNotAllowed) {
				t.Fatalf("got %v, want address rejection", err)
			}
		})
	}
	if netpolicy.IsPublic(netip.Addr{}) {
		t.Fatal("invalid address accepted")
	}
}

// The hook has exactly the dialer's Control signature, so a consuming adapter
// installs it on a direct net.Dialer (as ChatOps' webhook client does) with no
// wrapper that could add an exception.
var _ func(network, address string, c syscall.RawConn) error = (&net.Dialer{Control: netpolicy.PublicOnly}).Control

func TestDialHookNetworks(t *testing.T) {
	for _, tc := range []struct {
		network, address string
		ok               bool
	}{
		{"tcp", "1.1.1.1:443", true}, {"tcp4", "1.1.1.1:443", true}, {"tcp6", "[2606:4700::1]:443", true}, {"tcp", "[2606:4700::1]:8443", true},
		{"tcp4", "127.0.0.1:443", false}, {"tcp6", "[::1]:443", false}, {"tcp", "[::ffff:127.0.0.1]:443", false},
		{"udp", "1.1.1.1:443", false}, {"udp4", "1.1.1.1:443", false}, {"ip", "1.1.1.1:443", false}, {"unixgram", "/tmp/x", false},
	} {
		err := netpolicy.PublicOnly(tc.network, tc.address, nil)
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, netpolicy.ErrAddressNotAllowed)) {
			t.Errorf("%s %s: got %v, want ok=%v", tc.network, tc.address, err, tc.ok)
		}
	}
}

func TestDialHookRejectsInvalidDestinations(t *testing.T) {
	for _, tc := range []struct{ network, address string }{
		{"tcp", "example.invalid:443"},
		{"tcp", "not-an-address"},
		{"tcp", ":443"},
		{"tcp", "[bad::ip]:443"},
		{"udp", "1.1.1.1:443"},
		{"unix", "/tmp/kodama.sock"},
	} {
		if err := netpolicy.PublicOnly(tc.network, tc.address, nil); !errors.Is(err, netpolicy.ErrAddressNotAllowed) {
			t.Errorf("%s %s: got %v, want address rejection", tc.network, tc.address, err)
		}
	}
}

// Exercise the actual net.Dialer hook against a local listener. No external
// network or DNS is needed, and a rejected address must never reach Accept.
func TestDialHookBlocksBeforeConnect(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	dialer := net.Dialer{Control: netpolicy.PublicOnly}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp4", listener.Addr().String())
	if conn != nil {
		conn.Close()
		t.Fatal("connected to a refused address")
	}
	if !errors.Is(err, netpolicy.ErrAddressNotAllowed) {
		t.Fatalf("got %v, want address rejection", err)
	}
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	accepted, err := listener.Accept()
	if accepted != nil {
		accepted.Close()
		t.Fatal("refused connection reached the listener")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("accept: got %v, want timeout", err)
	}
}

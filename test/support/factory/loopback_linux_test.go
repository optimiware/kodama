//go:build linux

package factory

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// ensureLoopback makes loopback routable for a loopback-only test.
//
// A fresh network namespace (for example `unshare -rn`) starts with the
// loopback interface DOWN, so a loopback client gets ENETUNREACH even though
// the test needs no route. When the connect probe fails this helper raises the
// standard loopback interface through the ordinary SIOCSIFFLAGS ioctl. It
// touches no other interface, address or route; on a routed host the probe
// succeeds and no ioctl runs. If the process lacks CAP_NET_ADMIN the shared
// infrastructure policy applies: skip unless KODAMA_REQUIRE_INFRA=1.
func ensureLoopback(t testing.TB) {
	t.Helper()
	if loopbackUsable() {
		return
	}
	if err := enableLoopback(); err != nil {
		harness.SkipOrFail(t, os.Getenv("KODAMA_REQUIRE_INFRA") == "1",
			fmt.Errorf("loopback is unusable and could not be enabled: %w", err))
	}
	if !loopbackUsable() {
		harness.SkipOrFail(t, os.Getenv("KODAMA_REQUIRE_INFRA") == "1",
			fmt.Errorf("loopback is still unusable after enabling the interface"))
	}
}

// ifreq mirrors the kernel's struct ifreq for SIOCSIFFLAGS: the 16-byte
// interface name followed by the short flags field (padded to the kernel's
// 40-byte struct on 64-bit hosts; only name and flags are read).
type ifreq struct {
	Name  [syscall.IFNAMSIZ]byte
	Flags uint16
	_     [22]byte
}

// enableLoopback sets IFF_UP on the loopback interface. It changes no address
// or route and fails closed with the ioctl error when it is not permitted.
func enableLoopback() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	var request ifreq
	copy(request.Name[:], "lo")
	request.Flags = syscall.IFF_UP
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&request))); errno != 0 {
		return errno
	}
	return nil
}

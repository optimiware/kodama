//go:build linux

package native

import (
	"fmt"
	"net"
	"syscall"
)

// systemPeerUID returns the connecting peer's uid from SO_PEERCRED. The hook
// channel only accepts a process running as the same uid.
func systemPeerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credential, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			credentialErr = err
			return
		}
		uid = credential.Uid
	}); err != nil {
		return 0, err
	}
	if credentialErr != nil {
		return 0, fmt.Errorf("read peer credentials: %w", credentialErr)
	}
	return uid, nil
}

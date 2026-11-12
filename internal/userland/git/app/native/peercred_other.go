//go:build !linux

package native

import (
	"net"
	"os"
)

// systemPeerUID reports the current process uid where the platform exposes no
// socket peer-credential lookup. The same-uid check is then vacuously true.
func systemPeerUID(*net.UnixConn) (uint32, error) {
	return uint32(os.Getuid()), nil
}

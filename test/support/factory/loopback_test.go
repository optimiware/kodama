package factory

import (
	"net"
	"time"
)

// loopbackUsable reports whether a loopback TCP client can reach a loopback
// listener. A fresh network namespace has the loopback interface DOWN: binding
// 127.0.0.1 still succeeds, but connecting to it fails with ENETUNREACH even
// though no route is needed.
func loopbackUsable() bool {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false
	}
	defer func() { _ = listener.Close() }()
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

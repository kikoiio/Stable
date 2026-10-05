package ipc

import (
	"net"
	"time"
)

// ListenPrivate creates a Unix-domain listener at path with mode 0600.
// If removeStale is true, an existing socket file is removed first (supervisor
// and conversation service). If false, an already-present path is an error
// (network proxy).
func ListenPrivate(path string, removeStale bool) (net.Listener, error) {
	return listenPrivate(path, removeStale)
}

// DialPrivate connects to a Unix-domain socket with the given timeout.
func DialPrivate(path string, timeout time.Duration) (net.Conn, error) {
	return dialPrivate(path, timeout)
}

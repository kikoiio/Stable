package ipc

import (
	"net"
	"time"
)

// ListenPrivate creates a private Unix-domain listener on Linux/macOS or a
// current-user named pipe on Windows. If removeStale is true, an existing Unix
// socket file is removed first; named pipes are managed by the kernel. If
// false, an already-active endpoint is reported as already in use.
func ListenPrivate(path string, removeStale bool) (net.Listener, error) {
	return listenPrivate(path, removeStale)
}

// DialPrivate connects to the private local endpoint with the given timeout.
func DialPrivate(path string, timeout time.Duration) (net.Conn, error) {
	return dialPrivate(path, timeout)
}

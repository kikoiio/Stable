//go:build unix

package ipc

import (
	"net"
	"os"
	"time"

	"stable/internal/platform/secfile"
)

func listenPrivate(path string, removeStale bool) (net.Listener, error) {
	if removeStale {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = secfile.ChmodPrivate(path, 0600); err != nil {
		listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return listener, nil
}

func dialPrivate(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

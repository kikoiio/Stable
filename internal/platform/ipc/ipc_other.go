//go:build !linux

package ipc

import (
	"errors"
	"net"
	"time"
)

func listenPrivate(string, bool) (net.Listener, error) {
	return nil, errors.New("ipc: private unix sockets are only supported on Linux")
}

func dialPrivate(string, time.Duration) (net.Conn, error) {
	return nil, errors.New("ipc: private unix sockets are only supported on Linux")
}

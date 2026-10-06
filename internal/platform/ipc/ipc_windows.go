//go:build windows

package ipc

import (
	"errors"
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func listenPrivate(path string, removeStale bool) (net.Listener, error) {
	pipe := PipeName(path)
	if !removeStale {
		if conn, err := winio.DialPipe(pipe, nil); err == nil {
			_ = conn.Close()
			return nil, errors.New("named pipe already in use")
		}
	}
	return winio.ListenPipe(pipe, &winio.PipeConfig{SecurityDescriptor: currentUserPipeSDDL(), MessageMode: false, InputBufferSize: 64 * 1024, OutputBufferSize: 64 * 1024})
}

func currentUserPipeSDDL() string {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return CurrentUserPipeSDDL()
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return CurrentUserPipeSDDL()
	}
	return PipeSDDL(user.User.Sid.String())
}

func dialPrivate(path string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(PipeName(path), &timeout)
}

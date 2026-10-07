package runtime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"stable/internal/platform/ipc"
	"stable/internal/platform/paths"
)

// RemoteControlAction identifies a remote lifecycle operation sent to the
// runtime supervisor over its private control socket.
type RemoteControlAction string

const (
	RemoteControlStart  RemoteControlAction = "start"
	RemoteControlStop   RemoteControlAction = "stop"
	RemoteControlStatus RemoteControlAction = "status"
	RemoteControlPair   RemoteControlAction = "pair"
)

// RemoteControlRequest is the private CLI-to-supervisor request contract.
// ListenAddr may be empty for start, in which case the remote package applies
// its loopback default. TLS files must be supplied together.
type RemoteControlRequest struct {
	Action      RemoteControlAction `json:"action"`
	ListenAddr  string              `json:"listen_addr,omitempty"`
	TLSCertFile string              `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string              `json:"tls_key_file,omitempty"`
}

// Validate rejects unsupported operations and fields that do not apply to
// the requested operation. It deliberately leaves address and certificate
// validation to the remote listener configuration layer.
func (r RemoteControlRequest) Validate() error {
	switch r.Action {
	case RemoteControlStart:
		if (r.TLSCertFile == "") != (r.TLSKeyFile == "") {
			return errors.New("remote start requires both TLS certificate and key")
		}
	case RemoteControlStop, RemoteControlStatus, RemoteControlPair:
		if r.ListenAddr != "" || r.TLSCertFile != "" || r.TLSKeyFile != "" {
			return fmt.Errorf("remote %s does not accept listener configuration", r.Action)
		}
	default:
		return fmt.Errorf("unknown remote control action %q", r.Action)
	}
	return nil
}

// RemoteControlResult is the private supervisor response. PairingToken is
// populated only by a successful pair request and must be printed once by the
// local CLI; callers must not persist it or include it in logs.
type RemoteControlResult struct {
	Running      bool      `json:"running"`
	ListenAddr   string    `json:"listen_addr,omitempty"`
	PairingToken string    `json:"pairing_token,omitempty"`
	PairExpires  time.Time `json:"pair_expires_at,omitempty"`
	ErrorCode    string    `json:"error_code,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// RemoteControlError carries a supervisor-declared operation failure while
// preserving its machine-readable code for CLI handling.
type RemoteControlError struct {
	Code    string
	Message string
}

func (e *RemoteControlError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// RemoteControl sends one validated remote lifecycle request over the
// existing private supervisor socket and reads its typed result.
func RemoteControl(p paths.Paths, request RemoteControlRequest) (RemoteControlResult, error) {
	if err := request.Validate(); err != nil {
		return RemoteControlResult{}, err
	}
	conn, err := ipc.DialPrivate(p.Socket, 500*time.Millisecond)
	if err != nil {
		return RemoteControlResult{}, errors.New("runtime is not running")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return RemoteControlResult{}, err
	}
	var result RemoteControlResult
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&result); err != nil {
		return RemoteControlResult{}, err
	}
	if strings.TrimSpace(result.Error) != "" {
		return result, &RemoteControlError{Code: result.ErrorCode, Message: result.Error}
	}
	return result, nil
}

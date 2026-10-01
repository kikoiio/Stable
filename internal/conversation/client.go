package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Request opens a short-lived socket client, performs one operation and returns
// all messages produced by that operation. The UI calls it in a background cmd.
func Request(ctx context.Context, socket string, request ClientMsg) ([]ServerMsg, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, errors.New("session service is not reachable; start the runtime with stable")
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	decoder := json.NewDecoder(reader)
	var out []ServerMsg
	for {
		var m ServerMsg
		if err = decoder.Decode(&m); err != nil {
			return out, fmt.Errorf("read session response: %w", err)
		}
		if m.Type == "done" {
			return out, nil
		}
		if m.Type == "error" {
			return out, errors.New(m.Error)
		}
		out = append(out, m)
	}
}

package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/platform/ipc"
)

type StreamClient struct {
	conn    net.Conn
	decoder *json.Decoder
	mu      sync.Mutex
}

func OpenRun(ctx context.Context, socket string, request agent.ExecutionRequest) (*StreamClient, error) {
	client, err := openStream(ctx, socket)
	if err != nil {
		return nil, err
	}
	if err = client.Send(ClientMsg{Op: "run_start", SessionID: request.Work.SessionID, Run: &request}); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

// InvokeSkill opens a skill run: the service activates the skill and binds
// this connection to the resulting run, so run events (and the plain error
// plus done on activation failure) stream back over the returned client.
func InvokeSkill(ctx context.Context, socket, sessionID, name, args string) (*StreamClient, error) {
	client, err := openStream(ctx, socket)
	if err != nil {
		return nil, err
	}
	if err = client.Send(ClientMsg{Op: "skill_invoke", SessionID: sessionID, SkillName: name, SkillArgs: args}); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func SubscribeRun(ctx context.Context, socket, sessionID, runID string, afterSeq uint64) (*StreamClient, error) {
	client, err := openStream(ctx, socket)
	if err != nil {
		return nil, err
	}
	if err = client.Send(ClientMsg{Op: "run_subscribe", SessionID: sessionID, RunID: runID, AfterSeq: afterSeq}); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func openStream(ctx context.Context, socket string) (*StreamClient, error) {
	conn, err := ipc.DialPrivate(socket, 2*time.Second)
	if err != nil {
		return nil, errors.New("session service is not reachable; start the runtime with stable")
	}
	return &StreamClient{conn: conn, decoder: json.NewDecoder(bufio.NewReader(conn))}, nil
}

func (c *StreamClient) Send(message ClientMsg) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return json.NewEncoder(c.conn).Encode(message)
}

func (c *StreamClient) Receive() (ServerMsg, error) {
	var message ServerMsg
	err := c.decoder.Decode(&message)
	return message, err
}

func (c *StreamClient) Cancel(sessionID, runID string) error {
	return c.Send(ClientMsg{Op: "run_cancel", SessionID: sessionID, RunID: runID})
}

func (c *StreamClient) Close() error { return c.conn.Close() }

type GoalSocketClient struct{ Socket string }

func (c GoalSocketClient) RunGoal(ctx context.Context, request agent.ExecutionRequest) (agent.RunOutcome, error) {
	stream, err := OpenRun(ctx, c.Socket, request)
	if err != nil {
		return agent.RunOutcome{}, err
	}
	defer stream.Close()
	messages := make(chan ServerMsg, 16)
	readErr := make(chan error, 1)
	stopRead := make(chan struct{})
	defer close(stopRead)
	go func() {
		for {
			message, receiveErr := stream.Receive()
			if receiveErr != nil {
				select {
				case readErr <- receiveErr:
				case <-stopRead:
				}
				return
			}
			select {
			case messages <- message:
			case <-stopRead:
				return
			}
		}
	}()
	var runID string = request.RunID
	cancelSent := false
	ctxDone := ctx.Done()
	var cancelDeadline <-chan time.Time
	var cancelTimer *time.Timer
	defer func() {
		if cancelTimer != nil {
			cancelTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctxDone:
			if runID != "" && !cancelSent {
				_ = stream.Cancel(request.Work.SessionID, runID)
				cancelSent = true
			}
			ctxDone = nil
			cancelTimer = time.NewTimer(2 * time.Second)
			cancelDeadline = cancelTimer.C
		case <-cancelDeadline:
			return agent.RunOutcome{RunID: runID, Status: agent.RunCancelled}, ctx.Err()
		case receiveErr := <-readErr:
			return agent.RunOutcome{}, receiveErr
		case message := <-messages:
			switch message.Type {
			case "run_started":
				runID = message.RunID
				if ctx.Err() != nil && !cancelSent {
					_ = stream.Cancel(request.Work.SessionID, runID)
					cancelSent = true
				}
			case "run_outcome":
				if message.Outcome == nil {
					return agent.RunOutcome{}, errors.New("session service returned empty run outcome")
				}
				if cancelSent || ctx.Err() != nil {
					return *message.Outcome, ctx.Err()
				}
				return *message.Outcome, nil
			case "error":
				return agent.RunOutcome{}, errors.New(message.Error)
			}
		}
	}
}

// Request opens a short-lived socket client, performs one operation and returns
// all messages produced by that operation. The UI calls it in a background cmd.
func Request(ctx context.Context, socket string, request ClientMsg) ([]ServerMsg, error) {
	conn, err := ipc.DialPrivate(socket, 2*time.Second)
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

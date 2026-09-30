package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"stable/internal/runtime"
)

type wireMsg struct {
	Type    string `json:"type"`
	Error   string `json:"error"`
	Message *struct {
		Role string `json:"role"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"message"`
	Proposal *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"proposal"`
	Goal *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"goal"`
}

func printWire(line []byte) bool {
	var m wireMsg
	if json.Unmarshal(line, &m) != nil {
		return false
	}
	switch {
	case m.Error != "":
		fmt.Println("error:", m.Error)
	case m.Proposal != nil:
		fmt.Printf("proposal %s status=%s\n", m.Proposal.ID, m.Proposal.Status)
	case m.Goal != nil:
		fmt.Printf("goal %s status=%s\n", m.Goal.ID, m.Goal.Status)
	case m.Message != nil:
		fmt.Printf("[%s/%s] %s\n", m.Message.Role, m.Message.Kind, m.Message.Text)
	default:
		return false
	}
	return m.Error != ""
}

// chat connects a thin terminal to the persistent session service. One-shot
// mode (any action flag) sends a single operation, prints the responses and
// exits — scripts and e2e tests rely on it; interactive mode renders the live
// transcript.
func chat(args []string, p runtime.Paths) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	goal := fs.String("goal", "", "goal to focus (say/reply target)")
	say := fs.String("say", "", "send a steering message to the focused goal")
	reply := fs.String("reply", "", "reply to an open agent question")
	create := fs.String("create-goal", "", "describe a new goal in natural language")
	confirm := fs.String("confirm", "", "confirm a criteria proposal by ID")
	reject := fs.String("reject", "", "reject a criteria proposal by ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", p.ChatSocket, time.Second)
	if err != nil {
		return errors.New("session service is not reachable; is the runtime running? (stable up)")
	}
	defer conn.Close()

	var out []map[string]string
	if *say != "" {
		out = append(out, map[string]string{"op": "say", "goal": *goal, "text": *say})
	}
	if *reply != "" {
		out = append(out, map[string]string{"op": "reply", "goal": *goal, "text": *reply})
	}
	if *create != "" {
		out = append(out, map[string]string{"op": "create_goal", "goal": *goal, "text": *create})
	}
	if *confirm != "" {
		out = append(out, map[string]string{"op": "confirm", "id": *confirm, "goal": *goal})
	}
	if *reject != "" {
		out = append(out, map[string]string{"op": "reject", "id": *reject})
	}
	if len(out) > 1 {
		return errors.New("chat one-shot mode accepts only one action flag")
	}
	if len(out) == 1 {
		if err = json.NewEncoder(conn).Encode(out[0]); err != nil {
			return err
		}
		return readResponses(conn, 10*time.Second, time.Second)
	}
	return interactive(conn, bufio.NewScanner(os.Stdin), *goal)
}

// readResponses prints server messages until an error, the overall deadline,
// or a quiet period with no further traffic.
func readResponses(conn net.Conn, limit, quiet time.Duration) error {
	deadline := time.Now().Add(limit)
	last := time.Now()
	sawError := false
	reader := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(quiet))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break
		}
		last = time.Now()
		if printWire(line) {
			sawError = true
		}
		if sawError || time.Now().After(deadline) || time.Since(last) >= quiet {
			break
		}
	}
	if sawError {
		return errors.New("session service reported an error")
	}
	return nil
}

func interactive(conn net.Conn, in *bufio.Scanner, focus string) error {
	responses := make(chan []byte, 32)
	go func() {
		defer close(responses)
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			responses <- line
		}
	}()
	fmt.Println("connected to the persistent session; /goal ID to focus, /confirm ID, /reject ID, /reply text, /status, /quit")
	send := func(msg map[string]string) error { return json.NewEncoder(conn).Encode(msg) }
	for fmt.Print("> "); in.Scan(); fmt.Print("> ") {
		text := strings.TrimSpace(in.Text())
		if text == "" {
			continue
		}
		drain := false
		switch {
		case text == "/quit":
			return nil
		case strings.HasPrefix(text, "/goal "):
			focus = field(text)
			fmt.Println("focused goal:", focus)
		case strings.HasPrefix(text, "/confirm "):
			if err := send(map[string]string{"op": "confirm", "id": field(text), "goal": focus}); err != nil {
				return err
			}
			drain = true
		case strings.HasPrefix(text, "/reject "):
			if err := send(map[string]string{"op": "reject", "id": field(text)}); err != nil {
				return err
			}
			drain = true
		case strings.HasPrefix(text, "/reply "):
			if focus == "" {
				fmt.Println("no focused goal; use /goal ID")
				continue
			}
			if err := send(map[string]string{"op": "reply", "goal": focus, "text": strings.TrimPrefix(text, "/reply ")}); err != nil {
				return err
			}
			drain = true
		case text == "/status":
			if err := send(map[string]string{"op": "status"}); err != nil {
				return err
			}
			drain = true
		case strings.HasPrefix(text, "/"):
			fmt.Println("unknown command; try /goal, /confirm, /reject, /reply, /status, /quit")
		default:
			if focus == "" {
				fmt.Println("no focused goal; use /goal ID")
				continue
			}
			if err := send(map[string]string{"op": "say", "goal": focus, "text": text}); err != nil {
				return err
			}
			drain = true
		}
		if drain {
			// Print whatever arrives within a short window so the transcript
			// stays interactive without blocking the prompt.
			timeout := time.After(500 * time.Millisecond)
		drainLoop:
			for {
				select {
				case line, ok := <-responses:
					if !ok {
						return errors.New("session closed")
					}
					printWire(line)
				case <-timeout:
					break drainLoop
				}
			}
		}
	}
	return in.Err()
}

func field(text string) string {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

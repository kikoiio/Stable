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

	"stable/internal/core"
	"stable/internal/platform/paths"
)

type wireMsg struct {
	Type     string                 `json:"type"`
	Error    string                 `json:"error"`
	Message  *core.SessionMessage   `json:"message"`
	Proposal *core.CriteriaProposal `json:"proposal"`
	Goal     *core.Goal             `json:"goal"`
}

func printWire(line []byte) (bool, string) {
	var m wireMsg
	if json.Unmarshal(line, &m) != nil {
		return false, ""
	}
	switch {
	case m.Error != "":
		fmt.Println("error:", m.Error)
	case m.Proposal != nil:
		fmt.Printf("proposal %s status=%s\n", m.Proposal.ID, m.Proposal.Status)
		if m.Proposal.Status == core.ProposalPending {
			fmt.Printf("确认请输入 /confirm %s；拒绝请输入 /reject %s\n", m.Proposal.ID, m.Proposal.ID)
		}
	case m.Goal != nil:
		if m.Goal.Status == core.GoalPendingReverification {
			fmt.Printf("goal %s status=%s（待复核）\n", m.Goal.ID, m.Goal.Status)
			fmt.Printf("criteria_revision: %d\n", m.Goal.CriteriaRevision)
		} else {
			fmt.Printf("goal %s status=%s\n", m.Goal.ID, m.Goal.Status)
		}
		if m.Goal.Reason != "" {
			fmt.Println("reason:", m.Goal.Reason)
		}
	case m.Message != nil:
		fmt.Printf("[%s/%s] %s\n", m.Message.Role, m.Message.Kind, m.Message.Text)
		if m.Message.Kind == core.MessageKindCriteriaProposal {
			var proposal core.CriteriaProposal
			if json.Unmarshal(m.Message.Payload, &proposal) == nil {
				for _, criterion := range proposal.Criteria {
					fmt.Printf("  - %s: %s %s\n", criterion.ID, criterion.Kind, criterion.Payload)
				}
			}
		}
	default:
		return false, ""
	}
	if m.Message != nil && m.Message.Kind == core.MessageKindCriteriaConfirm {
		return m.Error != "", m.Message.GoalID
	}
	return m.Error != "", ""
}

// chat connects a thin terminal to the persistent session service. One-shot
// mode (any action flag) sends a single operation, prints the responses and
// exits — scripts and e2e tests rely on it; interactive mode renders the live
// transcript.
func chat(args []string, p paths.Paths) error {
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
		return readResponses(conn, 2*time.Minute)
	}
	return interactive(conn, bufio.NewScanner(os.Stdin), *goal)
}

// readResponses waits for the server's completion marker. The model may take
// longer than a quiet interval to prepare a criteria proposal.
func readResponses(conn net.Conn, limit time.Duration) error {
	_ = conn.SetReadDeadline(time.Now().Add(limit))
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("session response incomplete: %w", err)
		}
		var m wireMsg
		if json.Unmarshal(line, &m) == nil && m.Type == "done" {
			return nil
		}
		if failed, _ := printWire(line); failed {
			return errors.New("session service reported an error")
		}
	}
}

func interactive(conn net.Conn, in *bufio.Scanner, focus string) error {
	responses := make(chan []byte, 32)
	inputs := make(chan string)
	inputDone := make(chan error, 1)
	go func() {
		for in.Scan() {
			inputs <- in.Text()
		}
		inputDone <- in.Err()
	}()
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
	fmt.Println("已连接。直接输入可聊天；/goal <目标描述> 创建目标；/say <文字> 纠偏当前目标；/status 查看进度；/quit 退出。")
	send := func(msg map[string]string) error { return json.NewEncoder(conn).Encode(msg) }
	fmt.Print("> ")
	for {
		select {
		case line, ok := <-responses:
			if !ok {
				return errors.New("session closed")
			}
			var m wireMsg
			if json.Unmarshal(line, &m) == nil && m.Type == "done" {
				continue
			}
			_, goalID := printWire(line)
			if goalID != "" {
				focus = goalID
				fmt.Println("当前目标:", focus)
			}
			fmt.Print("> ")
			continue
		case err := <-inputDone:
			return err
		case line := <-inputs:
			text := strings.TrimSpace(line)
			if text == "" {
				fmt.Print("> ")
				continue
			}
			switch {
			case text == "/quit":
				return nil
			case strings.HasPrefix(text, "/focus "):
				focus = field(text)
				fmt.Println("当前目标:", focus)
			case strings.HasPrefix(text, "/goal ") || strings.HasPrefix(text, "/create "):
				if err := send(map[string]string{"op": "create_goal", "text": strings.TrimSpace(strings.SplitN(text, " ", 2)[1])}); err != nil {
					return err
				}
			case strings.HasPrefix(text, "/confirm "):
				if err := send(map[string]string{"op": "confirm", "id": field(text)}); err != nil {
					return err
				}
			case strings.HasPrefix(text, "/reject "):
				if err := send(map[string]string{"op": "reject", "id": field(text)}); err != nil {
					return err
				}
			case strings.HasPrefix(text, "/reply "):
				if focus == "" {
					fmt.Println("请先用 /goal 设置目标，或用 /focus 选择已有目标")
					continue
				}
				if err := send(map[string]string{"op": "reply", "goal": focus, "text": strings.TrimPrefix(text, "/reply ")}); err != nil {
					return err
				}
			case strings.HasPrefix(text, "/say "):
				if focus == "" {
					fmt.Println("请先用 /goal 设置目标，或用 /focus 选择已有目标")
					continue
				}
				if err := send(map[string]string{"op": "say", "goal": focus, "text": strings.TrimPrefix(text, "/say ")}); err != nil {
					return err
				}
			case text == "/status":
				if err := send(map[string]string{"op": "status"}); err != nil {
					return err
				}
			case strings.HasPrefix(text, "/"):
				fmt.Println("可用命令：/goal、/focus、/confirm、/reject、/say、/reply、/status、/quit")
			default:
				if err := send(map[string]string{"op": "chat", "text": text}); err != nil {
					return err
				}
			}
			fmt.Print("> ")
		}
	}
}

func field(text string) string {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

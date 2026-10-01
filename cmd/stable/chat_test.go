package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInteractiveGoalFlow(t *testing.T) {
	client, server := net.Pipe()
	input, inputWriter := io.Pipe()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = outputWriter
	t.Cleanup(func() {
		os.Stdout = oldStdout
		client.Close()
		server.Close()
		inputWriter.Close()
		outputWriter.Close()
		output.Close()
	})

	done := make(chan error, 1)
	go func() { done <- interactive(client, bufio.NewScanner(input), "") }()
	decoder := json.NewDecoder(server)
	encoder := json.NewEncoder(server)
	readCommand := func() map[string]string {
		t.Helper()
		_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
		var msg map[string]string
		if err := decoder.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		return msg
	}

	if _, err := fmt.Fprintln(inputWriter, "/goal 修复传感器连接"); err != nil {
		t.Fatal(err)
	}
	if got := readCommand(); got["op"] != "create_goal" || got["text"] != "修复传感器连接" {
		t.Fatalf("/goal sent %+v", got)
	}
	if err := encoder.Encode(map[string]any{"type": "proposal", "proposal": map[string]any{
		"id": "prop-1", "status": "proposed", "criteria": []map[string]any{{
			"id": "erc-clean", "kind": "kicad.erc_clean", "payload": map[string]any{"max_violations": 0},
		}},
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := fmt.Fprintln(inputWriter, "/confirm prop-1"); err != nil {
		t.Fatal(err)
	}
	if got := readCommand(); got["op"] != "confirm" || got["id"] != "prop-1" || got["goal"] != "" {
		t.Fatalf("/confirm sent %+v", got)
	}
	if err := encoder.Encode(map[string]any{"type": "message", "message": map[string]any{
		"role": "system", "kind": "criteria_confirm", "goal_id": "goal-1", "text": "目标已创建",
	}}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(output)
	_ = output.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for focused goal: %v", err)
		}
		if strings.Contains(line, "当前目标: goal-1") {
			break
		}
	}
	if _, err := fmt.Fprintln(inputWriter, "/say 优先检查 J1"); err != nil {
		t.Fatal(err)
	}
	if got := readCommand(); got["op"] != "say" || got["goal"] != "goal-1" || got["text"] != "优先检查 J1" {
		t.Fatalf("/say sent %+v", got)
	}
	if _, err := fmt.Fprintln(inputWriter, "你好"); err != nil {
		t.Fatal(err)
	}
	if got := readCommand(); got["op"] != "chat" || got["goal"] != "" || got["text"] != "你好" {
		t.Fatalf("plain text sent %+v", got)
	}
	if _, err := fmt.Fprintln(inputWriter, "/quit"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interactive did not quit")
	}
}

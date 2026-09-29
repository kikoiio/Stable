package decision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"proactive-agent/internal/core"
)

func TestValidateProposal(t *testing.T) {
	good := core.ProposedAction{Kind: "wait", Parameters: json.RawMessage(`{}`), Reason: "await event"}
	if err := Validate(good); err != nil {
		t.Fatal(err)
	}
	bad := []core.ProposedAction{
		{Kind: "delete_everything", Parameters: json.RawMessage(`{}`), Reason: "x"},
		{Kind: "wait", Parameters: json.RawMessage(`{}`)},
		{Kind: "wait", Reason: "x"},
		{Kind: "execute_capability", Parameters: json.RawMessage(`{}`), Reason: "x"},
	}
	for _, p := range bad {
		if err := Validate(p); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}

func TestCodexInvalidJSONAndBoundedRetry(t *testing.T) {
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object"}`), 0644); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "fake-codex")
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\necho x >> '" + count + "'\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = '-o' ]; then shift; echo 'not-json' > \"$1\"; exit 0; fi; shift; done\n"
	if err := os.WriteFile(cli, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	c := Codex{CLI: cli, SchemaPath: schema, Workdir: dir, Timeout: time.Second, Attempts: 2}
	_, err := c.Decide(context.Background(), core.DecisionContext{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected unavailable: %v", err)
	}
	data, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "x\nx\n" {
		t.Fatalf("unexpected retry count: %q", data)
	}
}

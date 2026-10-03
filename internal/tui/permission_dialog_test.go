package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"stable/internal/permission"
)

func TestPermissionDialogShowsRequestWithoutRawParameters(t *testing.T) {
	request := permission.ApprovalRequest{ID: "approve-1", Operation: permission.Operation{ID: "op", Kind: permission.OpCommand, Name: "run-check", Target: "kicad-cli", Parameters: []byte(`{"api_key":"secret-marker"}`)}, Authority: permission.Authority{AllowedRoot: "/project"}, Reason: "command needs review", ExpiresAt: time.Now().Add(time.Minute)}
	a := permission.Prompt(request)
	view := renderApprovalDialog([]permission.ApprovalPrompt{a}, 0, 120)
	for _, want := range []string{"run-check", "kicad-cli", "/project", "command needs review", "允许一次", "保存精确规则"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in %s", want, view)
		}
	}
	encoded, _ := json.Marshal(a)
	if strings.Contains(string(encoded), "secret-marker") {
		t.Fatal("raw operation parameters leaked into the prompt")
	}
}

func TestPermissionDialogMapsAllChoices(t *testing.T) {
	want := map[string]permission.ApprovalChoice{"1": permission.ChoiceAllowOnce, "2": permission.ChoiceSaveRule, "3": permission.ChoiceDeny}
	for key, choice := range want {
		got, err := approvalChoice(key)
		if err != nil || got != choice {
			t.Fatalf("%s => %s, %v; want %s", key, got, err, choice)
		}
	}
}

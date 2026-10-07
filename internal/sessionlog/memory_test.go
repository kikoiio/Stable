package sessionlog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMemoryEventsRoundTripAndProjectMetadata(t *testing.T) {
	root := t.TempDir()
	session, err := Create(root, "memory")
	if err != nil {
		t.Fatal(err)
	}
	action := MemoryActionRecord{Scope: "project", Entry: "style.md", Operation: "save", State: "success", At: time.Now().UTC()}
	background := MemoryBackgroundRecord{Action: "extract", State: "success", Count: 2, At: time.Now().UTC()}
	if _, err := Append(root, session.ID, EventMemoryAction, action); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(root, session.ID, EventMemoryBackground, background); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Events) != 3 {
		t.Fatalf("events = %d, want creation plus two memory events", len(replay.Events))
	}
	var gotAction MemoryActionRecord
	if replay.Events[1].Type != EventMemoryAction || decodeData(replay.Events[1].Data, &gotAction) != nil || !reflect.DeepEqual(gotAction, action) {
		t.Fatalf("action event = %+v, got %+v", replay.Events[1], gotAction)
	}
	var gotBackground MemoryBackgroundRecord
	if replay.Events[2].Type != EventMemoryBackground || decodeData(replay.Events[2].Data, &gotBackground) != nil || !reflect.DeepEqual(gotBackground, background) {
		t.Fatalf("background event = %+v, got %+v", replay.Events[2], gotBackground)
	}
	projection := Project(replay)
	if len(projection.Items) != 2 || projection.Items[0].Kind != ItemMemoryAction || projection.Items[1].Kind != ItemMemoryBackground {
		t.Fatalf("memory projection = %+v", projection.Items)
	}
	if projection.Items[0].MemoryAction == nil || projection.Items[1].MemoryBackground == nil {
		t.Fatalf("projection dropped memory metadata: %+v", projection.Items)
	}
	data, err := json.Marshal(replay.Events[1].Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "body") || strings.Contains(string(data), "remembered text") {
		t.Fatalf("memory action event contains memory body: %s", data)
	}
}

func TestMemoryEventsRejectBodyAndInvalidPayload(t *testing.T) {
	root := t.TempDir()
	session, err := Create(root, "memory-invalid")
	if err != nil {
		t.Fatal(err)
	}
	withBody := map[string]any{
		"scope": "project", "operation": "save", "state": "success", "at": time.Now().UTC(),
		"body": "remembered text",
	}
	if _, err := Append(root, session.ID, EventMemoryAction, withBody); err == nil {
		t.Fatal("memory body field was accepted in event")
	}
	if _, err := Append(root, session.ID, EventMemoryBackground, MemoryBackgroundRecord{
		Action: "extract", State: "failed", Count: -1, Reason: strings.Repeat("x", MaxMemoryEventText+1), At: time.Now().UTC(),
	}); err == nil {
		t.Fatal("invalid memory background payload was accepted")
	}
}

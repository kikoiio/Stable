package execution

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestHelperRequestJSONRoundTrip(t *testing.T) {
	want := HelperRequest{
		Tool: "write_file",
		Args: map[string]any{
			"path":    "notes/todo.txt",
			"content": "finish M04",
			"create":  true,
		},
		Workspace: "/workspace/candidate",
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got HelperRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip request mismatch: got %#v, want %#v", got, want)
	}
	if string(data) != `{"tool":"write_file","args":{"content":"finish M04","create":true,"path":"notes/todo.txt"},"workspace":"/workspace/candidate"}` {
		t.Fatalf("unexpected request JSON: %s", data)
	}
}

func TestHelperResponseJSONRoundTrip(t *testing.T) {
	want := HelperResponse{
		Output:    "updated notes/todo.txt",
		IsError:   false,
		Additions: 2,
		Removals:  1,
		DiffText:  "@@ -1 +1,2 @@\n-old\n+new\n+done\n",
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got HelperResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip response mismatch: got %#v, want %#v", got, want)
	}
	if string(data) != `{"output":"updated notes/todo.txt","is_error":false,"additions":2,"removals":1,"diff_text":"@@ -1 +1,2 @@\n-old\n+new\n+done\n"}` {
		t.Fatalf("unexpected response JSON: %s", data)
	}
}

func TestHelperResponseOmitsOptionalFields(t *testing.T) {
	data, err := json.Marshal(HelperResponse{Output: "tool failed", IsError: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"output":"tool failed","is_error":true}` {
		t.Fatalf("optional fields were not omitted: %s", data)
	}

	var got HelperResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Output != "tool failed" || !got.IsError || got.Additions != 0 || got.Removals != 0 || got.DiffText != "" {
		t.Fatalf("unexpected error response: %#v", got)
	}
}

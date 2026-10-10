package agentcatalog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func definitionText(name, fields, body string) []byte {
	return []byte("---\nname: " + name + "\ndescription: Useful read-only role\n" + fields + "---\n" + body)
}

func TestParseDefinitionAndReadonlyIntersection(t *testing.T) {
	d, err := ParseDefinition(definitionText("Custom-role", "model: provider/model-v2\ntools: [grep, read_file, write_file, command, grep]\ndisallowedTools: [read_file]\nmaxTurns: 3\nbackground: true\n", "Private role instructions"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "custom-role" || d.Model != "provider/model-v2" || d.MaxTurns != 3 || !d.Background || d.Instruction != "Private role instructions" {
		t.Fatalf("incorrect definition: %#v", d)
	}
	if got := d.EffectiveTools(); !reflect.DeepEqual(got, []string{"grep"}) {
		t.Fatalf("unsafe tools: %v", got)
	}
	data, err := json.Marshal(d.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), d.Instruction) || strings.Contains(string(data), "instruction") {
		t.Fatalf("body in public metadata: %s", data)
	}
	data, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), d.Instruction) {
		t.Fatalf("body serialized: %s", data)
	}
}

func TestDefaultAndExplicitEmptyTools(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         []string
	}{
		{"inherited", "", []string{"glob", "grep", "read_file"}},
		{"empty", "tools: []\n", []string{}},
		{"excluded", "disallowedTools: [glob, grep, read_file]\n", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDefinition(definitionText("test", tc.fields, "Instructions"))
			if err != nil {
				t.Fatal(err)
			}
			if d.Model != "inherit" || d.MaxTurns != 8 {
				t.Fatalf("wrong defaults: %#v", d)
			}
			if got := d.EffectiveTools(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestWorktreeIsolationEnablesOnlyBoundedFileTools(t *testing.T) {
	d, err := ParseDefinition(definitionText("builder", "isolation: worktree\ntools: [read_file, glob, grep, write_file, edit_file, command]\n", "Instructions"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"command", "edit_file", "glob", "grep", "read_file", "write_file"}
	if got := d.EffectiveTools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("effective worktree tools=%v want %v", got, want)
	}
	if got := d.EffectiveToolsForIsolation("none"); !reflect.DeepEqual(got, []string{"glob", "grep", "read_file"}) {
		t.Fatalf("explicit read-only override enabled writes: %v", got)
	}
	if got := (Definition{Name: "explore"}).EffectiveToolsForIsolation("worktree"); !reflect.DeepEqual(got, []string{"glob", "grep", "read_file"}) {
		t.Fatalf("explore gained write tools: %v", got)
	}
}

func TestParseRejectsInvalidDefinitionsWithoutLeakingValues(t *testing.T) {
	secret := "PRIVATE_ROLE_OR_CREDENTIAL_MARKER"
	cases := map[string][]byte{
		"no frontmatter":        []byte(secret),
		"no close":              []byte("---\nname: test\n"),
		"empty YAML":            []byte("---\n---\nInstructions"),
		"invalid YAML":          definitionText("test", "model: ["+secret+"\n", secret),
		"second document":       []byte("---\nname: test\n...\nname: test2\n---\nInstructions"),
		"unknown field":         definitionText("test", "permissionMode: bypassPermissions\n", secret),
		"unknown sensitive key": definitionText("test", secret+": value\n", secret),
		"empty name":            definitionText("''", "", secret),
		"invalid name":          definitionText("../bad", "", secret),
		"name too long":         definitionText(strings.Repeat("a", 65), "", secret),
		"empty description":     []byte("---\nname: test\ndescription: ''\n---\n" + secret),
		"description too long":  []byte("---\nname: test\ndescription: " + strings.Repeat("a", 257) + "\n---\n" + secret),
		"missing body":          definitionText("test", "", ""),
		"duplicate field":       definitionText("test", "name: other\n", secret),
		"unknown tool":          definitionText("test", "tools: ["+secret+"]\n", secret),
		"wildcard tool":         definitionText("test", "tools: ['*']\n", secret),
		"scalar tools":          definitionText("test", "tools: grep\n", secret),
		"null tools":            definitionText("test", "tools: null\n", secret),
		"numeric tool":          definitionText("test", "tools: [10]\n", secret),
		"null name":             definitionText("null", "", secret),
		"alias":                 definitionText("test", "tools: &list [grep]\ndisallowedTools: *list\n", secret),
		"zero turns":            definitionText("test", "maxTurns: 0\n", secret),
		"too many turns":        definitionText("test", "maxTurns: 9\n", secret),
		"negative turns":        definitionText("test", "maxTurns: -1\n", secret),
		"fractional turns":      definitionText("test", "maxTurns: 2.5\n", secret),
		"string bool":           definitionText("test", "background: 'true'\n", secret),
		"model whitespace":      definitionText("test", "model: 'provider model'\n", secret),
		"model oversized":       definitionText("test", "model: "+strings.Repeat("a", 257)+"\n", secret),
		"NUL":                   definitionText("test", "", secret+"\x00"),
		"invalid UTF8":          append(definitionText("test", "", secret), 0xff),
		"oversized file":        definitionText("test", "", strings.Repeat("x", MaxFileBytes)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDefinition(data)
			if err == nil {
				t.Fatal("invalid definition accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("private input leaked in error: %v", err)
			}
		})
	}
}

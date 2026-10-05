package commands

import "testing"

func TestRegisterFindAndList(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:        "deploy",
		Description: "Deploy the project",
		Aliases:     []string{"d", "ship"},
		Kind:        KindLocal,
	})
	r.Register(&Command{Name: "help", Description: "Show help", Kind: KindLocal})

	cmd, ok := r.Find("deploy")
	if !ok || cmd.Description != "Deploy the project" {
		t.Fatalf("find by name: %v %v", cmd, ok)
	}
	if cmd, ok = r.Find("ship"); !ok || cmd.Name != "deploy" {
		t.Fatalf("find by alias: %v %v", cmd, ok)
	}
	if _, ok = r.Find("missing"); ok {
		t.Fatal("find returned an unknown command")
	}

	list := r.List()
	if len(list) != 2 || list[0].Name != "deploy" || list[1].Name != "help" {
		t.Fatalf("list not sorted by name: %v", list)
	}
}

func TestRegisterPanicsOnConflicts(t *testing.T) {
	base := &Command{Name: "alpha", Aliases: []string{"a"}}
	for _, test := range []struct {
		name string
		cmd  *Command
	}{
		{"name vs name", &Command{Name: "alpha"}},
		{"name vs alias", &Command{Name: "a"}},
		{"alias vs name", &Command{Name: "beta", Aliases: []string{"alpha"}}},
		{"alias vs alias", &Command{Name: "beta", Aliases: []string{"a"}}},
	} {
		r := NewRegistry()
		r.Register(base)
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: register did not panic", test.name)
				}
			}()
			r.Register(test.cmd)
		}()
	}
}

func TestRegisterOptionalBuiltinsWin(t *testing.T) {
	r := NewRegistry()
	builtin := &Command{Name: "deploy", Description: "builtin deploy", Aliases: []string{"d"}}
	r.Register(builtin)

	if r.RegisterOptional(&Command{Name: "deploy", Description: "custom deploy"}) {
		t.Fatal("optional register accepted a duplicate name")
	}
	if r.RegisterOptional(&Command{Name: "d"}) {
		t.Fatal("optional register accepted a name colliding with a builtin alias")
	}
	if r.RegisterOptional(&Command{Name: "custom", Aliases: []string{"deploy"}}) {
		t.Fatal("optional register accepted an alias colliding with a builtin name")
	}
	if r.RegisterOptional(&Command{Name: "other", Aliases: []string{"d"}}) {
		t.Fatal("optional register accepted a duplicate alias")
	}
	cmd, ok := r.Find("deploy")
	if !ok || cmd.Description != "builtin deploy" {
		t.Fatalf("builtin was replaced: %v", cmd)
	}
	if cmd, ok = r.Find("d"); !ok || cmd.Name != "deploy" {
		t.Fatalf("builtin alias lost: %v", cmd)
	}

	if !r.RegisterOptional(&Command{Name: "custom"}) {
		t.Fatal("optional register rejected a free name")
	}
	if _, ok = r.Find("custom"); !ok {
		t.Fatal("optional register did not store the command")
	}
}

func TestParse(t *testing.T) {
	for _, test := range []struct {
		input, name, args string
	}{
		{"", "", ""},
		{"hello", "", ""},
		{" /deploy x  ", "deploy", "x"},
		{"/help", "help", ""},
		{"/help   ", "help", ""},
		{"/DEPLOY now", "deploy", "now"},
		{"/git log --oneline extra", "git", "log --oneline extra"},
	} {
		name, args := Parse(test.input)
		if name != test.name || args != test.args {
			t.Fatalf("Parse(%q) = (%q, %q) want (%q, %q)", test.input, name, args, test.name, test.args)
		}
	}
}

package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proactive-agent/internal/core"
)

func TestPolicyRejectsEscapeCapabilityAndStaleVersion(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sensor.kicad_sch")
	if err := os.WriteFile(inside, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.kicad_sch")
	if err := os.WriteFile(outside, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.kicad_sch")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	g := core.Goal{ID: "g", AllowedRoot: root, AllowedCapabilities: []string{"kicad.repair_connection"}}
	o := core.Observation{GoalID: "g", ArtifactID: "current"}
	p := Policy{Declared: map[string]core.CapabilityDescriptor{"kicad.repair_connection": {Name: "kicad.repair_connection"}}}
	a := core.ProposedAction{Kind: "execute_capability", Capability: "kicad.repair_connection", Target: inside, ExpectedArtifactID: "current"}
	if err := p.Check(g, o, a); err != nil {
		t.Fatal(err)
	}
	a.Target = outside
	if err := p.Check(g, o, a); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside: %v", err)
	}
	a.Target = link
	if err := p.Check(g, o, a); err == nil {
		t.Fatal("symlink escape accepted")
	}
	a.Target = inside
	a.Capability = "delete"
	if err := p.Check(g, o, a); err == nil {
		t.Fatal("unauthorized capability accepted")
	}
	a.Capability = "kicad.repair_connection"
	a.ExpectedArtifactID = "old"
	if err := p.Check(g, o, a); err == nil {
		t.Fatal("stale digest accepted")
	}
}

package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"stable/internal/core"
)

type Policy struct {
	Declared map[string]core.CapabilityDescriptor
}

func (p Policy) Check(g core.Goal, o core.Observation, a core.ProposedAction) error {
	if o.GoalID != g.ID {
		return errors.New("observation belongs to another goal")
	}
	switch a.Kind {
	case "wait", "ask_human", "observe":
		return nil
	case "open_computer", "execute_capability":
	default:
		return fmt.Errorf("unknown action kind: %s", a.Kind)
	}
	if a.ExpectedArtifactID == "" || a.ExpectedArtifactID != o.ArtifactID {
		return errors.New("proposal references a stale design digest")
	}
	capability := a.Capability
	if a.Kind == "open_computer" {
		if capability == "" {
			capability = "computer.ensure_open"
		}
		if capability != "computer.ensure_open" {
			return errors.New("open_computer requires computer.ensure_open")
		}
	}
	allowed := false
	for _, name := range g.AllowedCapabilities {
		if name == capability {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("capability %q is not authorized", capability)
	}
	if len(p.Declared) > 0 {
		if _, ok := p.Declared[capability]; !ok {
			return fmt.Errorf("capability %q is not declared", capability)
		}
	}
	if a.Target == "" {
		return errors.New("action target missing")
	}
	root, err := filepath.EvalSymlinks(g.AllowedRoot)
	if err != nil {
		return fmt.Errorf("allowed root: %w", err)
	}
	target, err := filepath.EvalSymlinks(a.Target)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("target must be a regular file")
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("target outside authorized root: %s", a.Target)
	}
	return nil
}

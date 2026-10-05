package permission

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type RuleEffect string

const (
	EffectAllow RuleEffect = "allow"
	EffectDeny  RuleEffect = "deny"
	EffectAsk   RuleEffect = "ask"
)

type ExactRule struct {
	Effect           RuleEffect    `json:"effect"`
	Kind             OperationKind `json:"kind"`
	Name             string        `json:"name"`
	Target           string        `json:"target"`
	ParametersDigest string        `json:"parameters_digest"`
	ScopeDigest      string        `json:"scope_digest"`
	Protocol         string        `json:"protocol,omitempty"`
	Host             string        `json:"host,omitempty"`
	Port             uint16        `json:"port,omitempty"`
}

type Policy struct{ Rules []ExactRule }

func (p Policy) Decide(a Authority, o Operation) PermissionDecision {
	scope, err := a.ScopeDigest()
	if err != nil {
		return PermissionDecision{Kind: DecisionDeny, Reason: err.Error()}
	}
	operation, err := o.Digest()
	if err != nil {
		return PermissionDecision{Kind: DecisionDeny, Reason: err.Error(), ScopeDigest: scope}
	}
	deny := func(reason string) PermissionDecision {
		return PermissionDecision{Kind: DecisionDeny, Reason: reason, ScopeDigest: scope, OperationDigest: operation}
	}
	// The only ask-free write in plan mode is the session's plan file; it is
	// exempt from the candidate-only write restriction but must still resolve
	// inside the authorized root and is still subject to exact rules below.
	planWrite := false
	if o.Kind == OpNetwork {
		if !networkGranted(a.Network, o) {
			return deny("network destination is not explicitly authorized")
		}
	} else if o.Kind == OpRead || o.Kind == OpWrite || o.Kind == OpLegacy {
		if o.Target == "" {
			return deny("file operation has no target")
		}
		resolved, err := resolvePath(o.Target)
		if err != nil {
			return deny("target path cannot be safely resolved")
		}
		if a.Mode == ModePlan && a.PlanFilePath != "" && o.Kind == OpWrite {
			planPath, planErr := resolvePath(a.PlanFilePath)
			if planErr == nil && planPath == resolved {
				planWrite = true
			}
		}
		allowed, err := contained(a.AllowedRoot, resolved)
		candidate, candidateErr := contained(a.CandidateRoot, resolved)
		if err != nil || candidateErr != nil || (!allowed && !candidate) {
			return deny("target is outside authorized project data")
		}
		if (o.Kind == OpWrite || o.Kind == OpLegacy) && !planWrite {
			if !candidate {
				return deny("writes are restricted to the isolated candidate")
			}
			if a.FormalRoot != "" {
				formal, ferr := contained(a.FormalRoot, resolved)
				if ferr != nil || formal {
					return deny("formal project files are read-only")
				}
			}
		}
	} else if o.Kind != OpCommand {
		return deny("unknown operation kind")
	}
	for _, r := range p.Rules {
		match, valid := matchesExact(r, a, o, scope)
		if !valid {
			return deny("permission rule is invalid")
		}
		if match && r.Effect == EffectDeny {
			return deny("operation is explicitly denied")
		}
	}
	for _, r := range p.Rules {
		match, _ := matchesExact(r, a, o, scope)
		if match && r.Effect == EffectAsk {
			return PermissionDecision{Kind: DecisionAsk, Reason: "operation requires user approval by rule", ScopeDigest: scope, OperationDigest: operation}
		}
	}
	for _, r := range p.Rules {
		match, _ := matchesExact(r, a, o, scope)
		if match && r.Effect == EffectAllow {
			return PermissionDecision{Kind: DecisionAllow, Reason: "exact saved rule", ScopeDigest: scope, OperationDigest: operation}
		}
	}
	if o.Kind == OpNetwork {
		return PermissionDecision{Kind: DecisionAllow, Reason: "explicit network grant", ScopeDigest: scope, OperationDigest: operation}
	}
	if a.Mode == ModeBypass {
		return PermissionDecision{Kind: DecisionAllow, Reason: "bypass mode within hard boundaries", ScopeDigest: scope, OperationDigest: operation}
	}
	switch o.Kind {
	case OpRead:
		return PermissionDecision{Kind: DecisionAllow, Reason: "authorized project read", ScopeDigest: scope, OperationDigest: operation}
	case OpWrite, OpLegacy:
		if a.Mode == ModeAcceptEdits {
			return PermissionDecision{Kind: DecisionAllow, Reason: "candidate edit accepted by mode", ScopeDigest: scope, OperationDigest: operation}
		}
		if planWrite {
			return PermissionDecision{Kind: DecisionAllow, Reason: "plan file write allowed in plan mode", ScopeDigest: scope, OperationDigest: operation}
		}
	}
	return PermissionDecision{Kind: DecisionAsk, Reason: fmt.Sprintf("%s operation requires user approval", o.Kind), ScopeDigest: scope, OperationDigest: operation}
}

func matchesExact(r ExactRule, a Authority, o Operation, scope string) (bool, bool) {
	if r.Effect != EffectAllow && r.Effect != EffectDeny && r.Effect != EffectAsk {
		return false, false
	}
	if r.Kind == "" || r.Name == "" || r.ScopeDigest == "" {
		return false, false
	}
	p, err := digest(o.Parameters)
	if err != nil {
		return false, false
	}
	target, targetErr := ruleTarget(a, o)
	if targetErr != nil {
		return false, false
	}
	return r.Kind == o.Kind && r.Name == o.Name && r.Target == target && r.ParametersDigest == p && r.ScopeDigest == scope && r.Protocol == o.Protocol && r.Host == o.Host && r.Port == o.Port, true
}

func ruleTarget(a Authority, o Operation) (string, error) {
	if o.Target == "" {
		return "", nil
	}
	resolved, err := resolvePath(o.Target)
	if err != nil {
		return "", err
	}
	for _, entry := range []struct{ prefix, root string }{{"candidate:", a.CandidateRoot}, {"project:", a.AllowedRoot}} {
		if entry.root == "" {
			continue
		}
		inside, e := contained(entry.root, resolved)
		if e != nil {
			return "", e
		}
		if inside {
			base, e := filepath.Abs(entry.root)
			if e != nil {
				return "", e
			}
			realBase, e := filepath.EvalSymlinks(base)
			if e != nil {
				return "", e
			}
			rel, e := filepath.Rel(realBase, resolved)
			if e != nil {
				return "", e
			}
			return entry.prefix + filepath.ToSlash(rel), nil
		}
	}
	return filepath.Clean(o.Target), nil
}

func networkGranted(grants []NetworkGrant, o Operation) bool {
	if o.Protocol == "" || o.Host == "" || o.Port == 0 {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(o.Host), ".")
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	for _, g := range grants {
		gh := strings.TrimSuffix(strings.ToLower(g.Host), ".")
		if ip := net.ParseIP(gh); ip != nil {
			gh = ip.String()
		}
		if strings.EqualFold(g.Protocol, o.Protocol) && gh == host && g.Port == o.Port {
			return true
		}
	}
	return false
}

func resolvePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symbolic link targets are not accepted")
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		return resolved, nil
	}
	parent := filepath.Dir(abs)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}

func contained(root, target string) (bool, error) {
	if root == "" {
		return false, nil
	}
	r, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	t, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	r, err = filepath.EvalSymlinks(r)
	if err != nil {
		// A root that does not exist yet (e.g. the lazily created candidate
		// directory during a read-only investigation) contains nothing; that
		// is a negative answer, not a resolution failure.
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	parent := filepath.Dir(t)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return false, err
	}
	t = filepath.Join(resolvedParent, filepath.Base(t))
	rel, err := filepath.Rel(r, t)
	if err != nil {
		return false, err
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))), nil
}

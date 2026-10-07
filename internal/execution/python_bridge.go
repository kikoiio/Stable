package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"stable/internal/core"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
)

var ErrOutcomeUnknown = errors.New("capability outcome unknown")

type PythonBridge struct {
	Python      string
	Script      string
	AllowedRoot string
	Sandbox     sandbox.SandboxManager
	Profile     sandbox.SandboxProfile
	ProfileFor  func(core.CapabilityRequest) (sandbox.SandboxProfile, error)
	GuestScript string
	Timeout     time.Duration
	Env         []string
	// NetworkGrants are trusted grants supplied by the caller that owns this
	// bridge. They are pinned for each one-shot call; persistent computer
	// sessions reject them before session creation.
	NetworkGrants   []permission.NetworkGrant
	NetworkResolver sandbox.GrantResolver
	sessionMu       sync.Mutex
	sessions        map[string]sandbox.SandboxSession
}

func (b *PythonBridge) Call(ctx context.Context, req core.CapabilityRequest) (core.CapabilityResult, error) {
	var out core.CapabilityResult
	if req.ProtocolVersion != 1 || req.OperationID == "" || req.Kind == "" {
		return out, errors.New("invalid capability request")
	}
	if b.Sandbox == nil {
		return out, fmt.Errorf("%w: legacy bridge has no isolated execution manager", sandbox.ErrUnavailable)
	}
	python := b.Python
	if python == "" {
		python = "python3"
	}
	limit := b.Timeout
	if limit <= 0 {
		limit = 30 * time.Second
	}
	if len(b.Env) > 0 {
		return out, errors.New("legacy bridge cannot pass host environment to Python")
	}
	var err error
	profile := b.Profile
	if b.ProfileFor != nil {
		profile, err = b.ProfileFor(req)
		if err != nil {
			return out, err
		}
	}
	if len(b.NetworkGrants) > 0 {
		pinned, pinErr := PinNetworkGrants(ctx, b.NetworkGrants, b.NetworkResolver)
		if pinErr != nil {
			return out, pinErr
		}
		profile.NetworkGrants = pinned
	}
	if strings.HasPrefix(req.Kind, "computer.") && len(profile.NetworkGrants) > 0 {
		if sessionErr := RejectPersistentNetwork(permission.Authority{Network: profile.NetworkGrants}); sessionErr != nil {
			return out, sessionErr
		}
	}
	if b.Script != "" {
		hostDir, dirErr := filepath.Abs(filepath.Dir(b.Script))
		if dirErr != nil {
			return out, dirErr
		}
		found := false
		for _, mount := range profile.ReadOnlyMounts {
			if mount.HostPath == hostDir && mount.GuestPath == "/workspace/bridge" {
				found = true
				break
			}
		}
		if !found {
			profile.ReadOnlyMounts = append(profile.ReadOnlyMounts, sandbox.ReadOnlyMount{HostPath: hostDir, GuestPath: "/workspace/bridge"})
		}
	}
	if err = validateBridgeTarget(req, profile); err != nil {
		return out, err
	}
	wireReq, err := sandboxRequest(req, profile)
	if err != nil {
		return out, err
	}
	if profile.Timeout <= 0 {
		profile.Timeout = limit
	}
	guestScript := b.GuestScript
	if guestScript == "" {
		guestScript = filepath.Join("/workspace/bridge", filepath.Base(b.Script))
	}
	wireRequest, err := json.Marshal(wireReq)
	if err != nil {
		return out, err
	}
	input := strings.NewReader(string(append(wireRequest, '\n')))
	var result sandbox.SandboxResult
	var runErr error
	if strings.HasPrefix(req.Kind, "computer.") {
		result, runErr = b.callComputerSession(ctx, req, profile, python, guestScript, input)
	} else {
		result, runErr = b.Sandbox.RunIsolated(ctx, profile, []string{python, guestScript}, input)
	}
	if runErr != nil {
		return out, fmt.Errorf("%w: isolated bridge failed: %v: %s", ErrOutcomeUnknown, runErr, strings.TrimSpace(string(result.Stderr)))
	}
	if result.ExitCode != 0 {
		return out, fmt.Errorf("%w: isolated bridge exited %d: %s", ErrOutcomeUnknown, result.ExitCode, strings.TrimSpace(string(result.Stderr)))
	}
	decoder := json.NewDecoder(strings.NewReader(string(result.Stdout)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&out); err != nil {
		return out, fmt.Errorf("%w: invalid response: %v", ErrOutcomeUnknown, err)
	}
	if out.ProtocolVersion != 1 || out.OperationID != req.OperationID {
		return out, fmt.Errorf("%w: protocol version or operation ID mismatch", ErrOutcomeUnknown)
	}
	for i, path := range out.EvidencePaths {
		hostPath, mapErr := sandboxPathToHost(path, profile)
		if mapErr != nil {
			return out, fmt.Errorf("%w: evidence path mapping failed: %v", ErrOutcomeUnknown, mapErr)
		}
		out.EvidencePaths[i] = hostPath
	}
	switch out.Status {
	case "applied", "already_satisfied", "unsupported", "blocked", "observed", "pass", "fail", "stale":
	default:
		return out, fmt.Errorf("%w: invalid status %q", ErrOutcomeUnknown, out.Status)
	}
	for _, p := range out.EvidencePaths {
		if err = checkEvidencePath(profile.RunRoot, p); err != nil {
			return out, fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
		}
	}
	return out, nil
}

func (b *PythonBridge) callComputerSession(ctx context.Context, req core.CapabilityRequest, profile sandbox.SandboxProfile, python, guestScript string, input io.Reader) (sandbox.SandboxResult, error) {
	var empty sandbox.SandboxResult
	if req.GoalID == "" || profile.CandidateRoot == "" {
		return empty, errors.New("computer bridge requires a goal and isolated candidate")
	}
	key := "computer-" + req.GoalID
	candidateID := filepath.Base(profile.CandidateRoot)
	profile.SessionID = key
	profile.CandidateID = candidateID
	profile.SessionArgv = []string{python, guestScript}
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	if b.sessions == nil {
		b.sessions = map[string]sandbox.SandboxSession{}
	}
	session, ok := b.sessions[key]
	if ok && session.CandidateID != candidateID {
		_ = b.Sandbox.StopIsolatedSession(context.Background(), session.ID)
		delete(b.sessions, key)
		ok = false
	}
	if !ok {
		var err error
		session, err = b.Sandbox.StartIsolatedSession(ctx, profile)
		if err != nil {
			return empty, fmt.Errorf("start isolated computer session: %w", err)
		}
		b.sessions[key] = session
	}
	result, err := b.Sandbox.CallIsolatedSession(ctx, session, input)
	if err != nil {
		_ = b.Sandbox.StopIsolatedSession(context.Background(), session.ID)
		delete(b.sessions, key)
		return empty, fmt.Errorf("computer session request failed: %w", err)
	}
	return result, nil
}

func validateBridgeTarget(req core.CapabilityRequest, p sandbox.SandboxProfile) error {
	var payload struct {
		Path        string `json:"path"`
		AllowedRoot string `json:"allowed_root"`
	}
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return errors.New("legacy bridge payload is invalid")
	}
	if payload.Path == "" || payload.AllowedRoot == "" {
		return errors.New("legacy bridge target and root are required")
	}
	allowedGuest, err := hostRootToGuest(payload.AllowedRoot, p)
	if err != nil {
		return err
	}
	targetGuest, err := hostPathToGuest(payload.Path, p)
	if err != nil {
		return err
	}
	if targetGuest != allowedGuest && !strings.HasPrefix(targetGuest, allowedGuest+string(os.PathSeparator)) {
		return errors.New("bridge target is outside the declared allowed root")
	}
	if req.Kind == "kicad.repair_connection" && filepath.Clean(payload.AllowedRoot) != filepath.Clean(p.CandidateRoot) {
		return errors.New("repair bridge must be scoped to the isolated candidate")
	}
	if err := checkEvidencePath(p.CandidateRoot, payload.Path); err == nil {
		return nil
	}
	if req.Kind != "kicad.repair_connection" {
		return checkEvidencePath(p.ProjectRoot, payload.Path)
	}
	return errors.New("repair bridge target is outside the isolated candidate")
}

func sandboxRequest(req core.CapabilityRequest, p sandbox.SandboxProfile) (core.CapabilityRequest, error) {
	var payload map[string]any
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return req, errors.New("legacy bridge payload is invalid")
	}
	if path, ok := payload["path"].(string); ok && path != "" {
		guest, err := hostPathToGuest(path, p)
		if err != nil {
			return req, err
		}
		payload["path"] = guest
	}
	if allowed, ok := payload["allowed_root"].(string); ok && allowed != "" {
		guest, err := hostRootToGuest(allowed, p)
		if err != nil {
			return req, err
		}
		payload["allowed_root"] = guest
	}
	if path, ok := payload["report_path"].(string); ok && path != "" {
		guest, err := hostOutputPathToGuest(path, p.RunRoot)
		if err != nil {
			return req, err
		}
		payload["report_path"] = guest
	}
	if path, ok := payload["run_root"].(string); ok && path != "" {
		guest, err := hostPathToGuest(path, p)
		if err != nil {
			return req, err
		}
		payload["run_root"] = guest
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return req, err
	}
	req.Payload = raw
	return req, nil
}

func hostPathToGuest(path string, p sandbox.SandboxProfile) (string, error) {
	for _, item := range []struct{ host, guest string }{{p.CandidateRoot, "/workspace/candidate"}, {p.ProjectRoot, "/workspace/project"}, {p.RunRoot, "/workspace/run"}} {
		if item.host == "" {
			continue
		}
		if err := checkEvidencePath(item.host, path); err == nil {
			rel, err := filepath.Rel(item.host, path)
			if err != nil {
				return "", err
			}
			if rel == "." {
				return item.guest, nil
			}
			return filepath.Join(item.guest, rel), nil
		}
	}
	return "", errors.New("bridge path is outside all isolated mounts")
}

func hostOutputPathToGuest(path, root string) (string, error) {
	if path == "" || root == "" || !filepath.IsAbs(path) || !filepath.IsAbs(root) {
		return "", errors.New("bridge output path or run root is invalid")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("bridge output path is outside the private run root")
	}
	if err = checkEvidencePath(root, filepath.Dir(path)); err != nil {
		return "", err
	}
	return filepath.Join("/workspace/run", rel), nil
}

func hostRootToGuest(path string, p sandbox.SandboxProfile) (string, error) {
	for _, item := range []struct{ host, guest string }{{p.CandidateRoot, "/workspace/candidate"}, {p.ProjectRoot, "/workspace/project"}, {p.RunRoot, "/workspace/run"}} {
		if item.host != "" && filepath.Clean(path) == filepath.Clean(item.host) {
			return item.guest, nil
		}
	}
	return "", errors.New("bridge allowed root does not match an isolated mount")
}

func sandboxPathToHost(path string, p sandbox.SandboxProfile) (string, error) {
	for _, item := range []struct{ host, guest string }{{p.CandidateRoot, "/workspace/candidate"}, {p.ProjectRoot, "/workspace/project"}, {p.RunRoot, "/workspace/run"}} {
		if item.host != "" && (path == item.guest || strings.HasPrefix(path, item.guest+string(os.PathSeparator))) {
			rel, err := filepath.Rel(item.guest, path)
			if err != nil {
				return "", err
			}
			return filepath.Join(item.host, rel), nil
		}
	}
	return "", errors.New("sandbox returned path outside mounted roots")
}

func checkEvidencePath(root, path string) error {
	if root == "" || path == "" {
		return errors.New("evidence root or path missing")
	}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("evidence outside allowed root: %s", path)
	}
	return nil
}

func (b *PythonBridge) Observe(ctx context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
	return b.Call(ctx, r)
}
func (b *PythonBridge) Execute(ctx context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
	return b.Call(ctx, r)
}
func (b *PythonBridge) Verify(ctx context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
	return b.Call(ctx, r)
}

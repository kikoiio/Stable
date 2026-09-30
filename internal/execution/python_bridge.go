package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"stable/internal/core"
)

var ErrOutcomeUnknown = errors.New("capability outcome unknown")

type PythonBridge struct {
	Python      string
	Script      string
	AllowedRoot string
	Timeout     time.Duration
	Env         []string
}

func (b *PythonBridge) Call(ctx context.Context, req core.CapabilityRequest) (core.CapabilityResult, error) {
	var out core.CapabilityResult
	if req.ProtocolVersion != 1 || req.OperationID == "" || req.Kind == "" {
		return out, errors.New("invalid capability request")
	}
	python := b.Python
	if python == "" {
		python = "python3"
	}
	limit := b.Timeout
	if limit <= 0 {
		limit = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	request, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	cmd := exec.CommandContext(ctx, python, b.Script)
	cmd.Stdin = bytes.NewReader(append(request, '\n'))
	if len(b.Env) > 0 {
		cmd.Env = append(os.Environ(), b.Env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return out, fmt.Errorf("%w: %v: %s", ErrOutcomeUnknown, err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&out); err != nil {
		return out, fmt.Errorf("%w: invalid response: %v", ErrOutcomeUnknown, err)
	}
	if out.ProtocolVersion != 1 || out.OperationID != req.OperationID {
		return out, fmt.Errorf("%w: protocol version or operation ID mismatch", ErrOutcomeUnknown)
	}
	switch out.Status {
	case "applied", "already_satisfied", "unsupported", "blocked", "observed", "pass", "fail", "stale":
	default:
		return out, fmt.Errorf("%w: invalid status %q", ErrOutcomeUnknown, out.Status)
	}
	for _, p := range out.EvidencePaths {
		if err = checkEvidencePath(b.AllowedRoot, p); err != nil {
			return out, fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
		}
	}
	return out, nil
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

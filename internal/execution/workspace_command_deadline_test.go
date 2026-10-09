package execution

import (
	"context"
	"io"
	"testing"
	"time"

	"stable/internal/platform/sandbox"
)

type workspaceDeadlineSandbox struct {
	*executorTestSandbox
	profile           sandbox.SandboxProfile
	effectiveDeadline time.Time
	hasDeadline       bool
}

func (s *workspaceDeadlineSandbox) RunIsolated(ctx context.Context, profile sandbox.SandboxProfile, argv []string, stdin io.Reader) (sandbox.SandboxResult, error) {
	s.profile = profile
	runCtx, cancel := context.WithTimeout(ctx, profile.Timeout)
	defer cancel()
	s.effectiveDeadline, s.hasDeadline = runCtx.Deadline()
	return sandbox.SandboxResult{Stdout: []byte(`{"output":"deadline captured"}`)}, nil
}

func TestWorkspaceCommandDeadlineIsCappedAndInheritsParentDeadline(t *testing.T) {
	for _, tc := range []struct {
		name            string
		parentTimeout   time.Duration
		wantParentBound bool
	}{
		{name: "default command capped at 90 seconds"},
		{name: "shorter parent deadline wins", parentTimeout: 30 * time.Second, wantParentBound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, lease, executor, _ := writerExecutorFixture(t)
			defer func() {
				if _, err := manager.ReleaseCompletedWriter(context.Background(), lease); err != nil {
					t.Errorf("release workspace writer: %v", err)
				}
			}()

			deadlineSandbox := &workspaceDeadlineSandbox{executorTestSandbox: &executorTestSandbox{}}
			runner := executor.(*toolRunExecutor)
			runner.deps.Sandbox = deadlineSandbox

			ctx := context.Background()
			var parentDeadline time.Time
			if tc.parentTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parentTimeout)
				defer cancel()
				parentDeadline, _ = ctx.Deadline()
			}
			if _, err := runner.executeCommand(ctx, map[string]any{"command": "printf controlled", "timeout": 600}); err != nil {
				t.Fatalf("workspace command: %v", err)
			}

			if deadlineSandbox.profile.Timeout != 90*time.Second {
				t.Fatalf("workspace profile timeout=%s, want 90s cap", deadlineSandbox.profile.Timeout)
			}
			if !deadlineSandbox.hasDeadline {
				t.Fatal("sandbox execution context has no deadline")
			}
			remaining := time.Until(deadlineSandbox.effectiveDeadline)
			if remaining <= 0 || remaining > 90*time.Second {
				t.Fatalf("effective sandbox deadline has %s remaining, want >0 and <=90s", remaining)
			}
			if tc.wantParentBound && !deadlineSandbox.effectiveDeadline.Equal(parentDeadline) {
				t.Fatalf("effective deadline=%s, want shorter parent deadline %s", deadlineSandbox.effectiveDeadline, parentDeadline)
			}
		})
	}
}

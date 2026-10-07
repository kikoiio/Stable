package agent

import "context"

type forkSkillParentRunKey struct{}

// WithForkSkillParentRun carries the active run's already-derived read-only
// execution context through the skill provider call.
func WithForkSkillParentRun(ctx context.Context, parent ParentRun) context.Context {
	return context.WithValue(ctx, forkSkillParentRunKey{}, parent)
}

// ForkSkillParentRunFromContext returns the parent run context installed by
// the tool executor. Slash skill entry points construct their own parent.
func ForkSkillParentRunFromContext(ctx context.Context) (ParentRun, bool) {
	parent, ok := ctx.Value(forkSkillParentRunKey{}).(ParentRun)
	return parent, ok
}

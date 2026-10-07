package mcp

import (
	"strings"
	"testing"

	"stable/internal/execution"
)

// schemaTool builds a tool whose input schema serializes deterministically.
func schemaTool(name, description string) execution.MCPToolSchema {
	return execution.MCPToolSchema{
		Name:        name,
		Description: description,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// sizedTools returns a one-tool set whose measured character count is
// exactly target, padding the description with incompressible ASCII so the
// boundary cases below are exact.
func sizedTools(t *testing.T, target int) []execution.MCPToolSchema {
	t.Helper()
	tools := []execution.MCPToolSchema{schemaTool("mcp__srv__tool", "")}
	base := MeasureSchemaChars(tools)
	if target < base {
		t.Fatalf("target %d is below the minimal tool size %d", target, base)
	}
	tools[0].Description = strings.Repeat("a", target-base)
	if got := MeasureSchemaChars(tools); got != target {
		t.Fatalf("constructed schema measures %d chars, want exactly %d", got, target)
	}
	return tools
}

func TestStrategyEstimateTokens(t *testing.T) {
	// 625 chars / 2.5 = 250 tokens；不足一个 token 的余数截断。
	if got := EstimateSchemaTokens(625); got != 250 {
		t.Errorf("EstimateSchemaTokens(625) = %d, want 250", got)
	}
	if got := EstimateSchemaTokens(624); got != 249 {
		t.Errorf("EstimateSchemaTokens(624) = %d, want 249（截断）", got)
	}
}

func TestStrategyApplyTiers(t *testing.T) {
	// 默认窗口 200k，预算 2 万 token = 5 万字符（CharsPerToken 2.5）。
	// 窗口 2500 时预算为 250 token = 625 字符，用来测边界。
	large := sizedTools(t, 50_000+125)
	atBudget := sizedTools(t, 625)
	belowBudget := sizedTools(t, 624)
	builtinOnly := []execution.MCPToolSchema{schemaTool("read_file", "built-in")}

	cases := []struct {
		desc         string
		tools        []execution.MCPToolSchema
		window       int
		wantEager    int
		wantDispatch int
	}{
		{"小 schema 全量 eager",
			[]execution.MCPToolSchema{schemaTool("mcp__linear__create_issue", "stub")},
			DefaultContextWindow, 1, 0},
		{"大 schema 全量 dispatch", large, DefaultContextWindow, 0, 1},
		{"恰好等于预算判 dispatch（严格小于才 eager）", atBudget, 2500, 0, 1},
		{"预算少一个字符判 eager", belowBudget, 2500, 1, 0},
		{"没有 mcp__ 工具两档全空", builtinOnly, DefaultContextWindow, 0, 0},
		{"nil 输入两档全空", nil, DefaultContextWindow, 0, 0},
	}
	for _, c := range cases {
		eager, dispatch := applyWithWindow(c.tools, c.window)
		if len(eager) != c.wantEager || len(dispatch) != c.wantDispatch {
			t.Errorf("%s：得到 eager=%d dispatch=%d，期望 eager=%d dispatch=%d",
				c.desc, len(eager), len(dispatch), c.wantEager, c.wantDispatch)
		}
	}

	// 内容抽查：eager 档放进的就是原工具。
	one := []execution.MCPToolSchema{schemaTool("mcp__linear__create_issue", "stub")}
	got, _ := applyWithWindow(one, DefaultContextWindow)
	if len(got) != 1 || got[0].Name != "mcp__linear__create_issue" {
		t.Errorf("eager 清单内容不对：%+v", got)
	}
}

func TestStrategyEnvOverrideForcesBothWays(t *testing.T) {
	small := []execution.MCPToolSchema{schemaTool("mcp__linear__create_issue", "stub")}
	large := sizedTools(t, 50_000+125)

	// 强制 dispatch：本该 eager 的小配置被压到 dispatch 档。
	t.Setenv(envLoadingOverride, "dispatch")
	eager, dispatch := Apply(small)
	if len(eager) != 0 || len(dispatch) != 1 {
		t.Errorf("env=dispatch 应强制全部 dispatch，得到 eager=%d dispatch=%d", len(eager), len(dispatch))
	}

	// 强制 eager：远超预算的大配置也被提进 eager 档。
	t.Setenv(envLoadingOverride, "eager")
	eager, dispatch = Apply(large)
	if len(eager) != 1 || len(dispatch) != 0 {
		t.Errorf("env=eager 应强制全部 eager，得到 eager=%d dispatch=%d", len(eager), len(dispatch))
	}

	// 其他值忽略，回落到体积判定：小的 eager，大的 dispatch。
	t.Setenv(envLoadingOverride, "bogus")
	eager, dispatch = Apply(small)
	if len(eager) != 1 || len(dispatch) != 0 {
		t.Errorf("非法 env 值应被忽略（小配置回落 eager），得到 eager=%d dispatch=%d", len(eager), len(dispatch))
	}
	_, dispatch = Apply(large)
	if len(dispatch) != 1 {
		t.Errorf("非法 env 值应被忽略（大配置回落 dispatch），得到 dispatch=%d", len(dispatch))
	}

	// 大小写与空白不敏感，对齐上游的判定方式。
	t.Setenv(envLoadingOverride, "  EAGER ")
	eager, _ = Apply(large)
	if len(eager) != 1 {
		t.Errorf("env 值应大小写不敏感地生效，得到 eager=%d", len(eager))
	}
}

func TestStrategyMeasureCountsOnlyMCPPrefix(t *testing.T) {
	if got := MeasureSchemaChars(nil); got != 0 {
		t.Errorf("nil 输入应为 0，得到 %d", got)
	}
	plain := []execution.MCPToolSchema{schemaTool("read_file", "built-in")}
	if got := MeasureSchemaChars(plain); got != 0 {
		t.Errorf("非 mcp__ 前缀工具不计入，得到 %d", got)
	}
	one := []execution.MCPToolSchema{schemaTool("mcp__linear__create_issue", "stub")}
	mixed := append(plain, one...)
	if got, want := MeasureSchemaChars(mixed), MeasureSchemaChars(one); got != want || got <= 0 {
		t.Errorf("混入内建工具后计值应不变且大于 0，得到 %d（期望 %d）", got, want)
	}
}

func TestStrategyApplySortsByNameStably(t *testing.T) {
	// 乱序输入 + 两个同名工具：按 Name 排序，同名保持输入相对顺序。
	in := []execution.MCPToolSchema{
		schemaTool("mcp__zeta__list", "1"),
		{Name: "mcp__alpha__dup", Description: "first", InputSchema: map[string]any{"type": "object"}},
		schemaTool("mcp__mid__run", "2"),
		{Name: "mcp__alpha__dup", Description: "second", InputSchema: map[string]any{"type": "object"}},
	}
	eager, dispatch := Apply(in)
	if len(dispatch) != 0 {
		t.Fatalf("小 schema 不该出现 dispatch 档，得到 %d 项", len(dispatch))
	}
	wantNames := []string{"mcp__alpha__dup", "mcp__alpha__dup", "mcp__mid__run", "mcp__zeta__list"}
	if len(eager) != len(wantNames) {
		t.Fatalf("eager 应有 %d 项，得到 %d", len(wantNames), len(eager))
	}
	for i, want := range wantNames {
		if eager[i].Name != want {
			t.Errorf("eager[%d].Name = %q, want %q", i, eager[i].Name, want)
		}
	}
	if eager[0].Description != "first" || eager[1].Description != "second" {
		t.Errorf("同名工具应保持输入相对顺序：得到 %q, %q", eager[0].Description, eager[1].Description)
	}
}

func TestStrategyApplyWithoutMCPToolsYieldsEmptyTiers(t *testing.T) {
	// 非前缀工具不进任何一档（补一条走 Apply 本体的断言，表驱动里走的是
	// applyWithWindow）。
	eager, dispatch := Apply([]execution.MCPToolSchema{schemaTool("read_file", "built-in")})
	if len(eager) != 0 || len(dispatch) != 0 {
		t.Errorf("没有 mcp__ 工具时两档都该为空，得到 eager=%d dispatch=%d", len(eager), len(dispatch))
	}
}

func TestStrategyApplyUsesInjectableContextWindow(t *testing.T) {
	orig := ContextWindow
	defer func() { ContextWindow = orig }()
	ContextWindow = func() int { return 2500 }

	// 625 字符在注入窗口 2500 下恰好等于预算 → dispatch；624 → eager。
	_, dispatch := Apply(sizedTools(t, 625))
	if len(dispatch) != 1 {
		t.Errorf("注入窗口 2500 后 625 字符应判 dispatch")
	}
	eager, _ := Apply(sizedTools(t, 624))
	if len(eager) != 1 {
		t.Errorf("注入窗口 2500 后 624 字符应判 eager")
	}
}

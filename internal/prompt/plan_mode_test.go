package prompt

import (
	"strings"
	"testing"
)

func TestBuildPlanModeReminderCadence(t *testing.T) {
	const planPath = "/tmp/goal/plan.md"
	full1 := BuildPlanModeReminder(planPath, false, 1)
	if full1 == "" {
		t.Fatal("iteration 1 must produce the full reminder")
	}
	for i := 2; i <= 5; i++ {
		sparse := BuildPlanModeReminder(planPath, false, i)
		if sparse == full1 {
			t.Fatalf("iteration %d must be sparse, got the full reminder", i)
		}
		if len(sparse) >= len(full1) {
			t.Fatalf("iteration %d sparse reminder (%d bytes) is not shorter than full (%d bytes)", i, len(sparse), len(full1))
		}
	}
	if got := BuildPlanModeReminder(planPath, false, 6); got != full1 {
		t.Fatal("iteration 6 must repeat the full reminder")
	}
}

func TestBuildPlanModeReminderFullContent(t *testing.T) {
	const planPath = "/tmp/goal/plan.md"
	full := BuildPlanModeReminder(planPath, true, 1)
	if !strings.Contains(full, planPath) {
		t.Fatalf("full reminder missing plan path %s", planPath)
	}
	for _, phase := range []string{"阶段 1", "阶段 2", "阶段 3", "阶段 4", "阶段 5"} {
		if !strings.Contains(full, phase) {
			t.Fatalf("full reminder missing %s", phase)
		}
	}
	for _, marker := range []string{"ask_user", "exit_plan_mode", "edit_file", "计划文件已存在", "增量编辑"} {
		if !strings.Contains(full, marker) {
			t.Fatalf("full reminder missing %q", marker)
		}
	}
	if strings.Contains(full, "子代理") {
		t.Fatal("full reminder must not reference subagents")
	}
}

func TestBuildPlanModeReminderPlanFileStates(t *testing.T) {
	const planPath = "/tmp/goal/plan.md"
	exists := BuildPlanModeReminder(planPath, true, 1)
	if !strings.Contains(exists, "edit_file") || strings.Contains(exists, "write_file 工具在 "+planPath+" 创建") {
		t.Fatal("planExists=true must direct incremental edits, not creation")
	}
	missing := BuildPlanModeReminder(planPath, false, 1)
	if !strings.Contains(missing, "尚不存在") || !strings.Contains(missing, "write_file") {
		t.Fatal("planExists=false must direct creating the plan file with write_file")
	}
}

func TestBuildPlanModeReminderSparseContent(t *testing.T) {
	const planPath = "/tmp/goal/plan.md"
	sparse := BuildPlanModeReminder(planPath, false, 3)
	for _, marker := range []string{planPath, "五阶段", "ask_user", "exit_plan_mode"} {
		if !strings.Contains(sparse, marker) {
			t.Fatalf("sparse reminder missing %q: %s", marker, sparse)
		}
	}
	if strings.Contains(sparse, "阶段 1") {
		t.Fatal("sparse reminder must not carry the full phase list")
	}
}

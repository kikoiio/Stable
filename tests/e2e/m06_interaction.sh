#!/usr/bin/env bash
# M06 计划与任务交互端到端验证：
# 自定义命令补全/展开/热更新、计划模式与审批弹窗状态机（auto/feedback/
# 非计划模式报错）、ask_user 提问答复与遗留排队、todo 任务清单的
# 脱敏/事件/重启恢复、目标提案 confirm/reject 协议状态机。
# 全部通过真实 conversation unix socket 驱动；仅 acceptEdits 候选写
# 一个用例需要 bwrap 沙箱与 agentworker 工具辅助进程。
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-2}

# The sandbox write scenario executes the real tool helper; resolve it the
# same way m04_tools.sh does (installed copy first, else build a throwaway).
if [[ -z "${STABLE_M06_HELPER:-}" ]]; then
  if [[ -x "$project_root/dev-install/libexec/agentworker" ]]; then
    export STABLE_M06_HELPER="$project_root/dev-install/libexec/agentworker"
  elif [[ -x "$project_root/bin/agentworker" ]]; then
    export STABLE_M06_HELPER="$project_root/bin/agentworker"
  else
    helper_root=$(mktemp -d "${TMPDIR:-/tmp}/stable-m06-helper-XXXXXXXX")
    go build -buildvcs=false -o "$helper_root/agentworker" ./cmd/agentworker
    export STABLE_M06_HELPER="$helper_root/agentworker"
  fi
fi

printf '\n==> M06 custom commands: completion, expansion and hot reload\n'
go test -p 1 ./tests/e2e -run '^TestM06CustomCommandsHotReloadAndExpansion$' -count=1 -v

printf '\n==> M06 plan mode: direct plan write, auto approval, accept-edits runs\n'
go test -p 1 ./tests/e2e -run '^TestM06PlanApprovalAutoAndAcceptEdits$' -count=1 -v

printf '\n==> M06 plan approval feedback keeps plan mode\n'
go test -p 1 ./tests/e2e -run '^TestM06PlanFeedbackKeepsPlanMode$' -count=1 -v

printf '\n==> M06 exit_plan_mode outside plan mode is refused\n'
go test -p 1 ./tests/e2e -run '^TestM06ExitPlanModeOutsidePlanMode$' -count=1 -v

printf '\n==> M06 ask_user round trip over the M05 question protocol\n'
go test -p 1 ./tests/e2e -run '^TestM06AskUserReplyRoundTrip$' -count=1 -v

printf '\n==> M06 leftover question reply is queued for the next run\n'
go test -p 1 ./tests/e2e -run '^TestM06AskLeftoverReplyQueuedForNextRun$' -count=1 -v

printf '\n==> M06 todo list: journal events, redaction and restart recovery\n'
go test -p 1 ./tests/e2e -run '^TestM06TodoPersistenceRedactionAndRestart$' -count=1 -v

printf '\n==> M06 proposal confirm/reject protocol state machines\n'
go test -p 1 ./tests/e2e -run '^TestM06ProposalProtocolStateMachines$' -count=1 -v

printf '\nM06 e2e: all scenario groups passed.\n'

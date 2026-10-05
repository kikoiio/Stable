#!/usr/bin/env bash
# M07-A 技能(Skills)与命令关联端到端验证：
# 两种布局解析与补全注册、skill_invoke 斜杠激活与 load_skill 工具激活、
# fork 技能两入口拒绝、正文级热更新、skill_reload 数量报告、
# 清单快照/一次性 delta/重启后重建一致（不重复 inventory 与 delta）。
# 全部通过真实 conversation unix socket 驱动，不需要 bwrap 沙箱。
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-2}

printf '\n==> M07-A skill activation, hot reload and fork refusal\n'
go test -p 1 ./tests/e2e -run '^TestM07ASkillActivationHotReloadAndForkRefusal$' -count=1 -v

printf '\n==> M07-A skill inventory, one-shot delta and restart consistency\n'
go test -p 1 ./tests/e2e -run '^TestM07ASkillInventoryDeltaAndRestartConsistency$' -count=1 -v

printf '\nM07-A e2e: all scenario groups passed.\n'

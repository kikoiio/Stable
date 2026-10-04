#!/usr/bin/env bash
# M05 会话、压缩、快照与 rewind 端到端验证：
# 多会话创建/搜索/重启一致恢复、超阈值压缩与持久边界审计、
# 候选多次变更/快照/rewind/重启恢复且正式工程不变、
# /say 有序消费与 /reply 精确答复且目标事实不变。
# 全部通过真实 conversation unix socket 驱动，不需要 bwrap 沙箱。
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$project_root"
export GOMAXPROCS=${GOMAXPROCS:-2}

printf '\n==> M05 session search and restart-consistent restore\n'
go test -p 1 ./tests/e2e -run '^TestM05SessionSearchAndRestartRestore$' -count=1 -v

printf '\n==> M05 over-threshold compaction and boundary audit\n'
go test -p 1 ./tests/e2e -run '^TestM05CompactionBoundaryAudit$' -count=1 -v

printf '\n==> M05 candidate snapshots, rewind and crash recovery\n'
go test -p 1 ./tests/e2e -run '^TestM05SnapshotRewindAndRestartRecovery$' -count=1 -v

printf '\n==> M05 /say ordering, /reply exactness and goal fact boundary\n'
go test -p 1 ./tests/e2e -run '^TestM05SayReplyAndGoalFactBoundary$' -count=1 -v

printf '\nM05 e2e: all scenario groups passed.\n'

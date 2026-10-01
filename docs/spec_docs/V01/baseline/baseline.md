# V01 主树基线记录（T01）

> 记录时间：2026-10-01。此目录位于被忽略的 `/docs/` 下，属于工作区文件，不入库。
> 用途：所有 V01 worktree 与集成阶段（T37–T42）的对照基线。

## HEAD

```
e053e5a96520ec4746aacbeb84ec1c26c12df471  (master)
```

## git status --short（T01 时点）

```
 M Makefile
 M README.md
 M cmd/stable/chat.go
 M cmd/stable/chatserve.go
 M cmd/stable/main.go
 M cmd/stable/oneshot.go
 M internal/conversation/protocol.go
 M internal/conversation/service.go
 M internal/conversation/service_test.go
 M internal/conversation/session.go
 M internal/goalrun/create.go
 M internal/runtime/doctor.go
 M internal/runtime/supervisor.go
 M scripts/package_linux.sh
 M scripts/run_local.sh
 M tests/package/cli.sh
?? cmd/stable/chat_test.go
?? internal/decision/chat.go
```

## 已跟踪差异

- `git diff --binary` 快照：`docs/spec_docs/V01/baseline/tracked.diff`（16 个文件，+327/−170）。
- 已用 `grep -E 'pending_reverification|CriteriaRevision|EvidenceProvenance|VerificationToken|ConfirmGoalCriteria|CommitVerification'` 逐项核对：**0 处匹配**，现有改动均为 V01 之前的既有工作（chat CLI、conversation、runtime、打包脚本等），不作为 V01 增量提交。

## 未跟踪代码文件（T05 需复制到各 worktree）

| 文件 | md5 |
| --- | --- |
| `cmd/stable/chat_test.go` | `b87e6a4482f4dcbe9a8dee9bb9074ec8` |
| `internal/decision/chat.go` | `544633db765b4110aa79abd62c24d0bc` |

## 文档忽略说明

`.gitignore` 第 10 行 `/docs/` 忽略了整个文档目录，四份 V01 文档与 `contracts/`、本基线目录目前均不入库；T39/T42 提交 V01 文档时需用 `git add -f docs/spec_docs/V01/` 显式纳入，且不纳入其他忽略文件。

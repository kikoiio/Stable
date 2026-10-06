# S02 目录、身份、IPC 与运行时生命周期(阶段 2)Checklist

> 状态:Linux 与交叉编译验收已完成；真实 Windows/macOS 行为按 real-os-acceptance.md 待阶段 5 验收。每一项通过运行代码或观察行为验证,先有证据再下结论。真实 OS(Windows/macOS)行为验证不在本机执行——按 real-os-acceptance.md 留阶段 5,不得在本清单冒充通过。执行环境:Ubuntu linux/amd64,go1.26.0;并行/重型操作遵守 AGENTS.md 内存规则。

## 实现完整性

- [x] paths 用户基目录推导齐备且 Linux 默认值逐字节不变(验证:`go test ./internal/platform/paths/ -count=1` 含 HOME/XDG 矩阵断言;`CGO_ENABLED=0 GOOS={windows,darwin} go build ./internal/platform/paths/` 退出码 0)
- [x] appconfig 路径推导已下沉,签名不变,新增 UserCommandsDir(验证:`go test ./internal/appconfig/ -count=1` 退出码 0,原有断言未修改)
- [x] TUI 目录来源统一,无自拼配置路径(验证:`grep -rn '\.config' internal/tui/ --include='*.go'` 无硬编码拼接;`go build ./...` 退出码 0)
- [x] secfile 私密 API(MkdirAllPrivate/OpenFilePrivate/ChmodPrivate/OwnedByCurrentUser/IsPrivate)可用,POSIX 落盘权限位与现状一致(验证:`go test ./internal/platform/secfile/ -count=1` 含临时目录权限位断言)
- [x] Windows ACL/SDDL 构造逻辑可在 Linux 契约测试(验证:`go test ./internal/platform/secfile/ ./internal/platform/ipc/ -run 'SDDL|PipeName|Private' -count=1` 退出码 0)
- [x] 16 个业务文件无私密 chmod 直调,行为回归(验证:`make platform-check` Chmod 扫描零违例;T9–T14 各包 `go test -count=1` 退出码 0)
- [x] ipc 三平台实现齐备,unix 侧逐行等价原 linux 实现(验证:`go build ./...` + 双 GOOS 编译;`go test ./internal/conversation/ ./internal/runtime/ -count=1` 退出码 0;`git diff` 确认 ipc_unix.go 仅改名/tag 变更)
- [x] lock 三平台实现齐备,supervisor 20s 重试语义不变(验证:`go test ./internal/runtime/ -count=1` 退出码 0;双 GOOS 编译 0)
- [x] proc 三平台实现齐备,Terminate/AdoptChild 接线,业务包无 syscall//proc 直调(验证:`grep -n 'syscall\.\|/proc/' internal/runtime/ internal/conversation/ -r` 零命中;`go test ./internal/runtime/... -count=1` 退出码 0;双 GOOS 编译 0)
- [x] doctor 工具清单按 GOOS 拆分且 Linux 清单不变(验证:单测驱动 Doctor() 断言清单与迁移前一致;不运行 stable CLI;双 GOOS 编译 0)

## 需求对照(spec AC1–AC9 逐条)

- [x] AC1 三平台路径推导单测过,TUI 无自拼路径(验证:paths user_test + grep + 双 GOOS 编译)
- [x] AC2 业务包 Chmod 直调为 0,Linux 行为一致,Windows ACL 契约单测过(验证:门禁 + secfile 定向测试)
- [x] AC3 ipc 三向编译过,Linux socket 集成测试不改断言全过,Windows 可拆逻辑有契约单测(验证:`go build` 三向 + conversation/runtime 测试 + pipe_name_test)
- [x] AC4 lock 三向编译过,supervisor 锁重试行为不变,Windows 可拆逻辑有契约单测(验证:同上 + lock 定向测试)
- [x] AC5 业务包无 /proc、syscall.Signal 直调,三向编译过,Linux 停止/重启/崩溃恢复测试全过(验证:门禁 + `go test ./internal/runtime/...` + `bash tests/e2e/waiting_restart.sh`)
- [x] AC6 doctor Linux 输出一致,非 Linux 编译过,不可用能力报明确原因(验证:单测 + 代码审阅 + 双 GOOS 编译)
- [x] AC7 `make test` 全过且 `make platform-check` 零违例(验证:两命令退出码 0)
- [x] AC8 real-os-acceptance.md 成文,含逐项「操作→期望」,S02 报告不声称已验证(验证:文档审阅)
- [x] AC9 S00 矩阵 C01–C05 已更新、C06–C10 未动;`git ls-files docs/spec_docs/S02/` 可见五份文档(验证:git diff 审阅 + git 命令)

## 编译与测试

- [x] `make test` 全过(`go test ./... -count=1`)
- [x] `make platform-check` 零违例(含扩展后的 unix/darwin/windows tag 白名单与 Chmod 扫描)
- [x] `go mod tidy` 后无二次变化,go-winio 为直接依赖且未被未用引用

## 端到端场景

- [x] 场景 1(Linux 运行时生命周期回归):appconfig.Load → paths.Resolve → lock 获取 → IPC 监听 → supervisor 启动 Temporal/worker(ConfigureChild+AdoptChild)→ 存活监控 → 停止/重启恢复,行为与 S02 前一致(验证:`go test ./internal/runtime/... -count=1`、`bash tests/e2e/waiting_restart.sh`;输出 `WAITING RESTART PASS`)
- [x] 场景 2(非 Linux 开发者视角):`GOOS=windows/darwin go build ./...` 直接成功;非 Linux 仅在触发 unsupported 能力(store Open、sandbox Probe、pgid API)时得到明确错误,无 panic(验证:双 GOOS 编译 + 契约单测/代码审阅)
- [x] 场景 3(门禁可持续):临时引入一处 `os.Chmod` 业务直调 → `make platform-check` 必须报出 → 删除后复验归零(验证:负向注入)
- [x] 场景 4(路径覆盖层):设置 XDG_CONFIG_HOME/XDG_STATE_HOME/STABLE_STATE_DIR 后,ConfigPath/StateDir/UserSkillsDir/UserCommandsDir 推导随覆盖一致变化,Linux 无覆盖时逐字节保持现状(验证:appconfig/paths 单测矩阵)
- [x] 场景 5(会话清理链):proc.Cmdline 身份匹配 → proc.Terminate 替换直调后,Xvfb 会话清理路径在 Linux 单测/审阅下行为不变(验证:runtime 定向测试)

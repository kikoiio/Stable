# M07-C MCP Checklist

> 状态:已批准(2026-10-06)。每一项通过运行代码或观察行为验证,先有证据再下结论。执行环境:Ubuntu linux/amd64,go1.26.0;开发在 /home/neo/Projects/stable-m07c worktree(m07c 分支);并行/重型操作遵守 AGENTS.md 内存规则。所有验证使用临时目录与 fixture,不读取、不加载真实用户配置。

## 实现完整性

- [x] go-sdk 依赖引入,构建与 tidy 稳定(验证:`go build ./...` 退出码 0;重复 `go mod tidy` 无二次变化)
- [x] appconfig MCPServerConfig 类型与 mcp_servers 解析可用,存量断言未改(验证:`go test ./internal/appconfig/ -count=1`)
- [x] sanitize/coerce/discover_timeout/strategy 纯函数件齐备,源端契约用例移植(验证:`go test ./internal/mcp/ -count=1`)
- [x] 两级配置 LoadMerge 合并/校验/rejections 齐备(验证:config_test 覆盖同名覆盖、位置保留、非法跳过、符号链接、超限)
- [x] Manager 实现 MCPCaller 全部方法与 ConnectAll/Reload/EnsureFresh/Shutdown/Status(验证:mcp_test fixture 生命周期、目标解析与状态；差分重载路径已实现并由全量测试覆盖构建)
- [x] mcptest fixture 三模式(正常/探测迟钝/秒退)可用且被单测与 e2e 复用(验证:单测与 `tests/e2e/m07c_mcp_test.go` 共用子包)
- [x] 执行 host 分支三入口接线,顺序为 pre_tool_use hook → 权限门 → 调用(验证:execution MCP 单测；MCPCaller nil 时按未知工具处理)
- [x] 权限 OpMCPTool 判定齐备且其他 Kind 零变化(验证:`go test ./internal/permission/ -count=1`)
- [x] sessionlog 事件族 mcp_reload/mcp_server 落盘与往返(验证:`go test ./internal/sessionlog/ -count=1`)
- [x] conversation op/handlers/instructions 注入齐备(验证:全量 conversation 测试与 M07C e2e；instructions 按会话去重)
- [x] hooks http 动作替换「未启用」桩(验证:`go test ./internal/hooks/ -count=1`,httptest 覆盖方法、头展开、体、超时、on_error)
- [x] 装配与两处 schema 组装追加,无配置时清单与 M07-C 前逐项一致(验证:`go build ./...` 及全量测试)
- [x] TUI /mcp 命令与渲染(验证:`go build ./...` 与代码审阅)
- [x] e2e 场景组与薄包装落盘(验证:`bash tests/e2e/m07c_mcp.sh` 退出码 0)

## 需求对照(spec AC1–AC9 逐条)

- [ ] AC1 两级发现与合并、非法条目跳过可见、/mcp 列表一致(验证:e2e 场景① + LoadMerge 单测)
- [ ] AC2 自动全连、探测超时回退、失败跳过隔离、关闭无残留(验证:Manager 单测 + discover 回退单测 + e2e 场景②)
- [ ] AC3 eager 直调、dispatch 查询+调用、强制覆盖、命名归一与覆盖语义(验证:strategy/sanitize 单测 + e2e 场景④)
- [ ] AC4 hook 拦截、默认 ask/批准/拒绝、参数强转、未知工具指引(验证:execution/permission 单测 + e2e 场景③④)
- [ ] AC5 连接失败/调用失败/未知工具三形态互不混淆(验证:e2e 场景②④ 断言三种错误文案)
- [ ] AC6 instructions 首轮注入一次,无 instructions 不注入(验证:e2e 场景⑤ + conversation 单测)
- [ ] AC7 mtime 自动生效、/mcp reload 报告、事件投影、主配置其余键不热生效(验证:e2e 场景⑥ + Manager 单测)
- [ ] AC8 http hook 请求到达/头展开/响应回流/超时可区分/on_error(验证:e2e 场景⑦ + hooks 单测)
- [ ] AC9 超限与异常跳过截断有报告、不崩溃、事件重启投影一致(验证:e2e 场景⑧ + 超限单测)

## 编译与测试

- [x] `make test` 全过(`GOFLAGS=-p=2 GOMAXPROCS=2 make test`)
- [x] `go mod tidy` 后无二次变化,go-sdk 为直接依赖且无版本冲突
- [x] `CGO_ENABLED=0 GOOS={windows,darwin} go build ./internal/mcp/` 退出码 0

## 端到端场景

- [ ] 场景 1(全链路):当前 e2e 已验证真实 stdio、eager 直调、/mcp 列表/重载和事件投影；dispatch、hook 拦截与审批路径由 execution/hooks 单测覆盖，尚未合并为单一端到端场景。
- [ ] 场景 2(热更新):差分 `Reload`、mtime 检测和 Runner schema 同步已实现，但尚未有覆盖增/删/改及重启回放的完整 e2e。
- [x] 场景 3(http hook):`internal/hooks/http_action_test.go` 的 httptest 矩阵覆盖请求、响应、截断、超时和 `on_error`。
- [ ] 场景 4(失败链):单元测试覆盖调用失败与未知工具；秒退 fixture 的完整服务 e2e 尚未加入。
- [x] 场景 5(回归保障):无 MCP 时 manager 注入为空时执行路径保持未知工具，schema 组装保留既有清单；全量回归测试通过。

## 覆盖边界

当前保留未勾选的端到端场景是测试覆盖缺口，不表示实现已失败。实现级测试、全量测试、跨平台编译和 M07C 薄包装均已通过；后续若需要发布级验收，应补齐场景 1、2、4 的组合式 e2e。

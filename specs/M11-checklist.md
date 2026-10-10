# M11 Linux 全量对照验收

> 当前状态：进行中。M08、M10-A、M10-B 已合并主分支并有各自 checklist 证据。跨平台运行时验收不在本轮范围。

## 功能迁移对照

- [x] M08 指令发现、记忆读写/召回、run 后提取、整理与目标事实隔离：对照 `specs/M08/checklist.md`，所列 AC 均有测试证据。
- [x] M10-A print、provider 选择、只读权限和临时 session 清理：对照 `specs/M10-A/checklist.md`，所列 AC 均有测试证据。
- [x] M10-B remote 配对、目录审批、浏览器会话、权限请求、TLS 与关闭生命周期：对照 `specs/M10-B/checklist.md`，所列 AC 均有测试证据。
- [ ] M01 真实 Linux 终端场景：打开会话、输入多行、导航目标后返回；resize、长回复、错误提示；补全边界。（验证：在可交互 TTY 中启动已配置 fake-provider 服务并逐项记录终端输出。）
- [ ] M07-C 完整服务进程重启：MCP 配置 reload 事件、服务状态和 session replay 在关闭并重新启动 stable service 后一致。（验证：真实 CLI/runtime 生命周期脚本，不以同进程重建 service 代替。）
- [ ] Remote 浏览器新 run 流：新提交 run 的 `text_delta` 即时显示在当前 transcript。（验证：`node --test tests/remote-ui.test.cjs`。）
- [ ] UTF-8 记忆上下文限制：中文和混合文本的完整渲染字节数不超过 128 KiB，section 仍闭合。（验证：`go test ./internal/conversation -run TestRenderMemoryContextIsBoundedAndContainsSelectedText -count=1`。）

## 普通任务与长期目标完整流程

- [ ] 普通任务：用户输入 → 同一 agent runner → 工具权限/隔离 → 候选检查 → 用户接收；拒绝或失败不修改正式工程。（验证：M03/M04/M10-A fake-provider 集成场景及安装包 CLI。）
- [ ] 长期目标：创建/批准 → runner 执行 → 证据独立验证 → 接收/导出 → 证据失效时重新调度和复核；不能将 agent 输出当作验收证据。（验证：安装包 restart acceptance 与 M03 restart e2e。）
- [ ] 普通任务与目标共享 runner、工具及权限边界，目标事实仍独立持久化。（验证：conversation integration assertions 对照两类 WorkRef。）

## Linux 编译、回归与安装

- [ ] GitHub Actions `Go` workflow：Node 浏览器 UI 回归、Go build/unit、package/install/CLI/e2e/restart 全通过。
- [ ] M09 Workspace Linux `writer-sandbox-volume` 通过。
- [ ] Linux x86_64 发布包安装、启动、停止、重启及 SHA256 记录完成。
- [ ] README 与发布安装文档记录最终 Linux 验收范围和已知边界。

## 跨平台范围

- [x] 本轮暂缓 Darwin/Windows 运行时与主机验收；不据 Linux 证据推断其他平台已验收。

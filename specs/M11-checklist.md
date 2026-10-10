# M11 Linux 全量对照验收

> 当前状态：Linux 功能与验收通过，发布操作待最终分支合并。M08、M10-A、M10-B 的 AC 均有独立 checklist 证据。跨平台运行时验收不在本轮范围。

## 功能迁移对照

- [x] M08 指令发现、记忆读写/召回、run 后提取、整理与目标事实隔离：对照 `specs/M08/checklist.md`，所列 AC 均有测试证据。
- [x] M10-A print、provider 选择、只读权限和临时 session 清理：对照 `specs/M10-A/checklist.md`，所列 AC 均有测试证据。
- [x] M10-B remote 配对、目录审批、浏览器会话、权限请求、TLS 与关闭生命周期：对照 `specs/M10-B/checklist.md`，所列 AC 均有测试证据。
- [x] M01 真实 Linux TTY 场景：打开/加载会话、提交多行输入、导航目标后返回；超视口长回复翻页与 resize、provider 错误提示、命令和受限路径补全。（验证：Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771)，`build-and-test` 的 `M01 production TTY acceptance` 使用 Python PTY、隔离 HOME/state、loopback fake provider 和正式 `stable` 无参数入口；通过。）
- [x] M07-C 完整服务进程重启：MCP 配置 reload 事件、服务状态和 session replay 在关闭并重新启动 stable service 后一致。（验证：E2E run [38035068654](https://github.com/kikoiio/Stable/actions/runs/38035068654), job `e2e-m07c`; 断言 supervisor PID 改变、MCP fixture 重连和 session replay 事件一致；通过。）
- [x] Remote 浏览器新 run 流：新提交 run 的 `text_delta` 即时显示在当前 transcript。（验证：Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771) 的 `node --test tests/remote-ui.test.cjs`；通过。）
- [x] UTF-8 记忆上下文限制：中文和混合文本的完整渲染字节数不超过 128 KiB，section 仍闭合。（验证：Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771) 的全量 `go test ./...`；通过。）

## 普通任务与长期目标完整流程

- [x] 普通任务：用户输入 → 同一 agent runner → 工具权限/隔离 → 候选检查 → 用户接收；拒绝或失败不修改正式工程。（验证：E2E run [38035068654](https://github.com/kikoiio/Stable/actions/runs/38035068654) 的 `e2e-m04`、`unit`，以及 Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771) 的 `test-package`；通过。）
- [x] 长期目标：创建/批准 → runner 执行 → 证据独立验证 → 接收/导出 → 证据失效时重新调度和复核；不能将 agent 输出当作验收证据。（验证：E2E run [38035068654](https://github.com/kikoiio/Stable/actions/runs/38035068654) 的 `e2e-m03`、`e2e-core`，以及 Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771) 的 `test-package` restart acceptance；通过。）
- [x] 普通任务与目标共享 runner、工具及权限边界，目标事实仍独立持久化。（验证：E2E run [38035068654](https://github.com/kikoiio/Stable/actions/runs/38035068654) 的 `unit`、`e2e-m03` 和 Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771) 的 package acceptance；通过。）

## Linux 编译、回归与安装

- [x] GitHub Actions `Go` workflow：Node 浏览器 UI 回归、Go build/unit、package/install/CLI/e2e/restart 全通过。（验证：Go run [38035068771](https://github.com/kikoiio/Stable/actions/runs/38035068771)，`build-and-test` 和 `test-package` 均通过。）
- [x] GitHub Actions `E2E` workflow 全部 jobs 通过。（验证：E2E run [38035068654](https://github.com/kikoiio/Stable/actions/runs/38035068654)，七个 jobs 均通过。）
- [x] M09 Workspace Linux `writer-sandbox-volume` 通过。（验证：run [38035077579](https://github.com/kikoiio/Stable/actions/runs/38035077579)，使用 disposable ext4 volume 的真实 quota 与 writer sandbox acceptance 通过。）
- [ ] Linux x86_64 发布包安装、启动、停止、重启及 SHA256 记录完成。（Go `test-package` 已通过；artifact 已校验，待合并后发布 v0.1.0 并复核 Release assets。）
- [x] README 与 Linux 发布安装文档记录验收范围和已知边界；Darwin/Windows/ARM64 未由 Linux 结果推定为已验收。

## 同 SHA 证据

上述 Go、E2E 与 M09 Workspace Linux workflows 均验证代码 SHA `5bd721e88bd9895ba37aa0ced5327f9e84d36fa4`。此 SHA 的 Go `test-package` artifact 为 `stable-0.1.0-linux-amd64.tar.gz` 与配套 `.sha256`；下载后归档 SHA-256 为 `d53e370f313dd5ecc8f7f8041ab2080ec21cd5da192fefe5e09ea82d0503c9a4`，与清单文件一致。该 artifact 的安装、CLI、E2E 与重启验收均由 `test-package` job 完成。

## 跨平台范围

- [x] 本轮暂缓 Darwin/Windows 运行时与主机验收；不据 Linux 证据推断其他平台已验收。

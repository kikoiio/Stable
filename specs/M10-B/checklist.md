# M10-B WebSocket Remote 与浏览器会话 Checklist

> 每项验证使用 fake provider、临时配置和自签证书；不使用真实模型密钥或公网。涉及完整测试套件时，按 AGENTS.md 优先使用免费云端 CI，并在调用前向用户说明服务、用途和所需权限，获得许可后再执行。

## AC1：Remote 生命周期与传输

- [ ] `stable remote status` 能显示停止和运行状态；无参数启动 remote 时监听地址为 loopback（验证：CLI 测试及读取实际 listener 地址）。
- [ ] `stable up` 不会自行启动 remote；`stable remote up` 在 runtime 未运行时会启动 runtime 后监听；`stable remote down` 关闭 remote 但保留 runtime（验证：生命周期集成测试，分别检查 runtime/remote 状态）。
- [ ] 非 loopback 监听缺少证书或私钥时启动失败，错误不会回退到明文；提供有效自签证书后 HTTPS 可访问（验证：配置与 TLS listener 自动化测试）。
- [ ] 执行 `stable down` 后 remote listener 关闭且 WebSocket 连接结束（验证：端到端启动服务、连接后执行 down，再确认端口连接失败）。

## AC2：配对、Origin 与凭据保护

- [x] 配对 token 在 5 分钟内仅成功消费一次；错误、过期、重用 token 返回拒绝（验证：配对存储测试覆盖每种输入）。
- [x] remote/runtime 停止或重启后旧浏览器 cookie 无法认证，重新配对后可连接（验证：会话生命周期测试）。
- [x] 浏览器会话 cookie 设置 HttpOnly、SameSite=Strict；TLS 下设置 Secure（验证：检查配对响应的 `Set-Cookie` 属性）。
- [x] 未认证 HTTP/WebSocket、跨站 Origin 和错误 Host 无法读取会话、目录、目标或 provider 信息（验证：HTTP/WebSocket 安全测试检查拒绝状态和无敏感响应体）。
- [ ] 模型密钥标记值不出现在页面资源、WebSocket 帧、HTTP 响应、日志、错误正文或 transcript（验证：使用 fake provider 注入唯一测试标记并扫描这些输出）。
- [x] 达到每来源地址每分钟 5 次配对失败后进入 1 分钟冷却，响应不泄漏 token 是否有效（验证：限流测试检查阈值前后行为及通用错误响应）。

## AC3：逐连接目录授权

- [x] WebSocket 先请求目录并等待 TUI 批准；TUI 弹窗展示客户端标识、解析后的绝对目录和到期时间（验证：conversation/TUI 集成测试检查请求内容和弹窗状态）。
- [x] 获批连接只能访问获批规范目录内的会话；伪造 grant、替换 root 或提交另一个目录均被拒绝（验证：授权边界测试分别发送合法和越界消息）。
- [x] TUI 拒绝、离线、目录不存在、路径解析失败、两分钟超时和服务关闭都不能创建 grant（验证：自动化拒绝路径测试检查 waiter 返回错误且 grant registry 为空）。
- [x] 断开后原 grant 失效；重新连接必须再次弹出 TUI 批准，之前的批准不能复用（验证：断线重连测试）。

## AC4：会话、执行和交互

- [x] fake provider 下浏览器可创建、列举、加载普通会话并恢复 transcript（验证：浏览器集成测试检查各请求响应和恢复后的会话事实）。
- [x] 浏览器可发送聊天、接收流式输出、取消 run，并可在断开后重新订阅仍在运行的 run（验证：fake provider 流测试检查事件顺序、取消终态和恢复订阅）。
- [ ] 工具写入/命令权限请求在浏览器显示原始权限提示，用户选择通过现有审批协议返回；未批准操作不会执行（验证：permission gate 集成测试在批准/拒绝两种路径检查工具结果）。
- [ ] 计划审批和 ask_user 均能显示并往返提交；交互等待遵循服务端超时/取消，断开不会自动批准（验证：计划审批与问题交互测试覆盖响应和取消路径）。
- [ ] 客户端不能提交可信 permission bounds、provider key、服务端路径映射或 runtime 控制；服务端从已批准 grant 生成执行范围（验证：对这些字段篡改/注入请求并确认拒绝或忽略，且边界不变）。

## AC5：目标只读与功能边界

- [x] 页面显示目标摘要、状态和最近事件（验证：fake store 中写入可识别状态后，浏览器 UI 展示相同字段）。
- [x] 远程请求无法创建、修改或确认目标；页面不提供 hooks/skills/MCP 管理、候选接收、snapshot rewind、runtime 管理或任意文件浏览入口（验证：UI 导航检查及对相应操作发送请求后确认拒绝且状态不变）。

## AC6：资源限制与可重复性

- [x] 配对请求 body 超过 16 KiB 被拒绝，WebSocket 帧超过 1 MiB 被关闭或拒绝（验证：`TestPairEndpointRejectsOversizedBody` 与 `TestWebSocketLimitsConcurrentConnectionsAndFrameSize`）。
- [x] 第 17 个活动 request ID 和第 9 个并发 WebSocket 连接被拒绝；重复活动 request ID 不能串流或覆盖既有请求（验证：`TestWebSocketRejectsDuplicateAndSeventeenthActiveRequest` 与 `TestWebSocketLimitsConcurrentConnectionsAndFrameSize`；重复 ID 拒绝用空 ID 回报，避免与原流混淆）。
- [ ] WebSocket 关闭后 stream subscription、context、grant、IPC 连接和 handler goroutine 均释放（验证：断线清理测试等待订阅数归零、grant 撤销且 handler 收敛）。
- [ ] 验收使用 fake provider、临时目录/配置、自签测试证书和 TUI 审批替身，不需要真实 API key 或公网（验证：集成测试启动参数与 fixture 检查）。

## AC7：文档与完整验证

- [x] README 说明 loopback 默认、LAN TLS 证书配置、pair token、目录批准、remote up/down/status 及 `stable down` 行为，且示例与 `stable remote --help` 一致（验证：逐条对照 README 与 CLI help 输出）。
- [ ] `go test -p 2 ./...` 全部通过（验证：记录命令实际退出码和摘要；按本机资源规则安排云端免费 CI 并在使用前获得用户许可）。
- [x] `gofmt`/静态格式检查通过，工作区没有本次新增的格式错误（验证：格式检查命令输出为空且退出码为 0）。

## 端到端场景

- [ ] 用户启动 `stable remote up` 并运行 `stable remote pair`；浏览器输入 token；服务端建立同源 HttpOnly cookie；用户请求项目目录；本机 TUI 显示客户端与规范目录并批准；浏览器创建会话、流式聊天、取消 run，再发起一次权限请求并由用户审批（验证：用 fake provider 和 TUI 替身运行一条自动化全链路，逐段检查可见状态与事件）。
- [ ] 同一连接获批后断开并重连；第二次目录请求被 TUI 再次展示。随后尝试跨站 Origin、无效 grant 和 LAN 无证书启动；三者均被拒绝，原获批目录与目标状态未被越权访问（验证：安全边界端到端测试检查拒绝结果及状态未变）。
- [ ] 远程连接仍活动时执行 `stable down`；浏览器连接终止，remote 端口关闭，runtime 退出（验证：生命周期端到端测试检查 WebSocket close、listener 拒绝新连接及 runtime 状态）。

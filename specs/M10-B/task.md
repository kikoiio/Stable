# M10-B WebSocket Remote 与浏览器会话 Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|---|---|---|
| 修改 | `cmd/stable/main.go` | 注册 `stable remote` 命令并保持现有命令行为 |
| 新建 | `cmd/stable/remote.go` | remote up/down/status/pair 参数、输出及错误处理 |
| 新建 | `cmd/stable/remote_test.go` | remote CLI 与 control client 行为测试 |
| 新建 | `internal/runtime/remote_control.go` | remote 生命周期的私有 runtime control 消息类型与客户端调用 |
| 新建 | `internal/runtime/remote_control_test.go` | control 消息编码与校验测试 |
| 修改 | `internal/runtime/supervisor.go` | remote manager 所有权、生命周期和 control 请求分发 |
| 修改 | `go.mod`、`go.sum` | 固定 `github.com/coder/websocket v1.8.15` |
| 新建 | `internal/remote/config.go` | loopback 默认值、监听地址和 TLS 配置校验 |
| 新建 | `internal/remote/auth.go` | 一次性配对、内存会话摘要、cookie 与限流 |
| 新建 | `internal/remote/manager.go` | HTTP/TLS listener、静态资源、状态和关闭清理 |
| 新建 | `internal/remote/websocket.go` | Origin 校验、WebSocket envelope 和 conversation bridge |
| 新建 | `internal/remote/ui/index.html` | 浏览器页面结构 |
| 新建 | `internal/remote/ui/app.css` | 浏览器界面样式与窄屏布局 |
| 新建 | `internal/remote/ui/app.js` | 配对、WebSocket、会话聊天和审批交互 |
| 新建 | `internal/remote/config_test.go` | 监听与 TLS 配置校验测试 |
| 新建 | `internal/remote/auth_test.go` | 配对、会话和限流测试 |
| 新建 | `internal/remote/manager_test.go` | HTTP/TLS/Origin/服务生命周期测试 |
| 新建 | `internal/remote/websocket_test.go` | bridge、限额和断线清理测试 |
| 修改 | `internal/conversation/protocol.go` | remote access 请求、resolve/release 和 grant 字段校验 |
| 新建 | `internal/conversation/remote_access.go` | pending 目录请求、超时等待和连接 grant 状态 |
| 修改 | `internal/conversation/service.go` | remote access IPC 分发、grant 生命周期及 root 解析 |
| 新建 | `internal/conversation/remote_access_test.go` | 目录请求、拒绝、超时、重连和越权测试 |
| 修改 | `internal/tui/model.go` | pending remote access 轮询、状态和消息路由 |
| 新建 | `internal/tui/remote_access_dialog.go` | 展示客户端与规范目录并批准/拒绝 |
| 新建 | `internal/tui/remote_access_test.go` | 弹窗呈现与批准/拒绝交互测试 |
| 新建 | `internal/remote/*_test.go` | pairing、TLS、Origin、limits、WebSocket bridge 与清理测试 |
| 修改 | `README.md` | 记录 remote 生命周期、监听、TLS、配对与目录批准方式 |

## T1：定义 remote runtime control 契约

**文件：** `internal/runtime/remote_control.go`、`internal/runtime/remote_control_test.go`
**依赖：** 无
**步骤：**
1. 定义 remote start/stop/status/pair 请求和结果的私有 JSON 结构。
2. 定义 CLI 到既有 supervisor control socket 的 typed 调用，拒绝未知操作和缺失参数。
3. 对返回状态、启动配置错误和 runtime 未运行场景增加契约测试。

**验证：** 运行 `go test -p 2 ./internal/runtime`，期望 control 消息可往返编码，错误请求不被接受。

## T2：实现 remote 监听配置校验

**文件：** `internal/remote/config.go`、`internal/remote/config_test.go`
**依赖：** 无；可与 T1 并行
**步骤：**
1. 设置缺省监听地址 `127.0.0.1:8765`。
2. 解析监听 IP，非 loopback 监听必须同时提供 cert/key。
3. 验证证书和私钥可加载；配置错误返回明确错误且不得尝试明文监听。

**验证：** 运行 `go test -p 2 ./internal/remote -run 'TestRemoteConfig'`，覆盖缺省、loopback、LAN 缺证书、有效自签证书和不匹配密钥。

## T3：增加 conversation 远程访问协议与状态

**文件：** `internal/conversation/protocol.go`、`internal/conversation/remote_access.go`、`internal/conversation/remote_access_test.go`
**依赖：** 无；可与 T1、T2 并行
**步骤：**
1. 定义 request/list/resolve/release 操作和请求、grant 数据结构。
2. 创建 pending request 时解析并规范化项目目录，记录客户端标签和 2 分钟过期时间。
3. 仅允许本机 TUI resolve 有效请求；拒绝、过期、service 关闭时唤醒 waiter 并返回失败。
4. 批准后生成不可猜测且绑定单连接与单目录的 grant；支持释放和过期撤销。

**验证：** 运行 `go test -p 2 ./internal/conversation -run 'TestRemoteAccess'`，期望批准成功、拒绝/超时 fail closed、重复 resolve 与过期 grant 被拒绝。

## T4：实现 conversation grant 强制校验

**文件：** `internal/conversation/service.go`、`internal/conversation/remote_access_test.go`
**依赖：** T3
**步骤：**
1. 将 remote grant ID 纳入所有远程 session/run/审批/目标读取请求。
2. 在统一 service 入口校验 grant 有效性，并从 grant 派生 project root；不信任客户端 root 或 permission bounds。
3. 保持本机 socket 原有 root 授权路径不变，确保两类来源不能互相扩大权限。
4. 连接释放、超时或 service 关闭时撤销 grant 并取消关联订阅。

**验证：** 运行 `go test -p 2 ./internal/conversation`，期望没有 grant、伪造 grant、错目录和已撤销 grant 的操作均失败，既有本机会话测试仍通过。

## T5：增加 TUI 目录批准入口

**文件：** `internal/tui/model.go`、`internal/tui/remote_access_dialog.go`、`internal/tui/remote_access_test.go`
**依赖：** T3
**步骤：**
1. 在 TUI 更新循环中轮询 pending remote access 请求并避免重复提示。
2. 显示客户端标识、规范化绝对路径和过期时间，提供批准与拒绝动作。
3. 将选择发回 conversation service；TUI 退出或离线时不生成默认批准。
4. 确保 remote access 弹窗优先级与现有权限/计划/提问弹窗队列兼容。

**验证：** 运行 `go test -p 2 ./internal/tui -run 'TestRemoteAccess'`，期望批准/拒绝请求 ID 正确，其他弹窗状态不被误消费。

## T6：实现配对和浏览器会话存储

**文件：** `internal/remote/auth.go`、`internal/remote/auth_test.go`
**依赖：** T2；可与 T3、T5 并行
**步骤：**
1. 用密码学随机数生成 5 分钟有效的一次性配对 token，服务端只保存带域分隔的摘要。
2. 原子消费 token，错误、过期和重复使用均返回通用失败。
3. 成功后签发 opaque HttpOnly、SameSite=Strict cookie；TLS 时设置 Secure，服务端只保存内存摘要。
4. remote 停止时清空 token/session store；对来源地址实施每分钟 5 次失败、冷却 1 分钟的限制。

**验证：** 运行 `go test -p 2 ./internal/remote -run 'TestPairing|TestBrowserSession|TestPairRateLimit'`，期望单次消费、cookie 属性、限流和重启失效行为符合 spec。

## T7：实现 RemoteManager 与同源 HTTP 服务

**文件：** `internal/remote/manager.go`、`internal/remote/manager_test.go`、`internal/remote/ui/index.html`
**依赖：** T2、T6
**步骤：**
1. 建立可重复 Start/Stop/Status 的 manager，托管静态文件和配对 endpoint。
2. loopback 使用 HTTP；非 loopback 只启动 TLS listener，禁止明文降级。
3. 校验 Host 与精确 Origin；配对 endpoint body 限制为 16 KiB。
4. Stop 时关闭 listener、HTTP 活动连接并清理内存认证状态。

**验证：** 运行 `go test -p 2 ./internal/remote -run 'TestRemoteHTTP|TestRemoteTLS|TestRemoteOrigin|TestRemoteStop'`，期望同源可用、跨站与明文 LAN 被拒绝、停止后端口不可连接。

## T8：实现 WebSocket conversation bridge

**文件：** `go.mod`、`go.sum`、`internal/remote/websocket.go`、`internal/remote/websocket_test.go`
**依赖：** T4、T6、T7
**步骤：**
1. 只接受已认证 cookie 和同源 WebSocket 握手，拒绝 binary/非法 JSON envelope。
2. 固定 plan 选定的 `github.com/coder/websocket v1.8.15` 依赖。
3. 将目录请求交给 conversation service 并等待本机 TUI grant，再开放会话操作。
4. 把每条 WebSocket 的请求绑定到其 grant，桥接现有 conversation IPC 和流式响应。
5. 限制帧至 1 MiB、每连接活动 request ID 至 16、总并发连接至 8；拒绝重复活动 ID。
6. 断开时撤销 grant、取消 stream、关闭 IPC 并等待 handler goroutine 退出。

**验证：** 运行 `go test -p 2 ./internal/remote -run 'TestWebSocket'`，期望聊天/流/取消可往返，未认证、越权、超限、断线清理均有明确验证。

## T9：接入 supervisor remote 生命周期

**文件：** `internal/runtime/supervisor.go`、`internal/runtime/remote_control.go`、`internal/runtime/remote_control_test.go`
**依赖：** T1、T7
**步骤：**
1. 由 supervisor 持有单个 RemoteManager，并串行处理 remote start/stop/status/pair 请求。
2. `remote up` 在 runtime 未运行时先启动 runtime，再启动 remote；`remote down` 保留 runtime。
3. supervisor 退出（包括 `stable down`）时关闭 remote 并等待其连接清理完成。
4. 返回当前监听地址与状态，不在 control response/log 中暴露配对 token 以外的私密值；pair token 只回给本机 CLI 一次。

**验证：** 运行 `go test -p 2 ./internal/runtime -run 'TestRemoteControl|TestSupervisorRemoteLifecycle'`，期望 start/stop/status/pair 分发正确，supervisor 退出后 remote 不再监听。

## T10：增加 stable remote CLI

**文件：** `cmd/stable/main.go`、`cmd/stable/remote.go`、`cmd/stable/remote_test.go`
**依赖：** T1、T9
**步骤：**
1. 注册 `stable remote up|down|status|pair`，解析 listen/cert/key 参数。
2. 将命令转成 typed runtime control 请求；运行态之外需要启动时复用现有 runtime 启动流程。
3. 输出可读状态和一次性 pair token；错误时返回非零状态，不输出密钥或内部凭据。
4. 确认 `stable down` 仍通过 supervisor 收尾关闭 remote。

**验证：** 运行 `go test -p 2 ./cmd/stable -run 'TestRemoteCLI'`，期望命令参数校验、状态输出、pair 单次打印和非零失败符合行为定义。

## T11：实现浏览器会话页面

**文件：** `internal/remote/ui/index.html`、`internal/remote/ui/app.css`、`internal/remote/ui/app.js`、`internal/remote/manager.go`
**依赖：** T7、T8
**步骤：**
1. 页面提供配对输入、目录请求和连接状态，不把 token 放入 URL、localStorage 或日志。
2. 展示获批目录内的会话列表、会话内容、文本回复流和 run 取消。
3. 呈现工具权限、计划审批与 ask_user，并将用户选择按现有 conversation 协议提交。
4. 展示目标摘要、状态和最近事件；不显示 provider key，不提供 spec 排除的管理操作。
5. 处理断线重连与已有 run 重新订阅；每次新 WebSocket 连接重新请求目录批准。

**验证：** 运行 `go test -p 2 ./internal/remote -run 'TestEmbeddedUI|TestBrowserConversation'`；自动化浏览器集成场景期望覆盖配对、授权、聊天/流式响应、取消、审批、提问及只读目标状态。

## T12：集成验证与文档

**文件：** `README.md`、相关 `*_test.go`
**依赖：** T4、T5、T8、T9、T10、T11
**步骤：**
1. 补充端到端测试：启动 runtime 与 remote、配对、请求目录并由 TUI 替身批准、完成浏览器会话、停止服务。
2. 补充拒绝场景：TUI 离线/超时、跨站 Origin、LAN 缺证书、伪造 grant、配对重用、资源超限。
3. README 说明 loopback 默认、LAN TLS 配置、pair、目录批准、remote down 与 stable down 的差异。
4. 运行受影响包的定向测试和静态格式检查；完整 `go test -p 2 ./...` 按仓库资源规则安排执行。

**验证：** 定向集成测试全部通过，README 操作步骤与 CLI help 一致；完整测试套件通过后记录实际结果。若完整测试需使用云端免费 CI，先向用户说明服务、用途和所需权限并取得许可。

## 执行顺序

```text
并行批次 1：T1（runtime control 契约） | T2（remote 配置校验） | T3（conversation access 状态）
并行批次 2：T4（grant 强制校验） | T5（TUI 审批，依赖 T3） | T6（配对认证，依赖 T2）
汇合点 A：T4 + T5 + T6
顺序批次：T7（HTTP manager，依赖 T2/T6）
并行批次 3：T8（WS bridge，依赖 T4/T6/T7） | T9（supervisor lifecycle，依赖 T1/T7）
并行批次 4：T10（CLI，依赖 T1/T9） | T11（浏览器 UI，依赖 T7/T8）
汇合点：T10 + T11 → T12（集成验证与 README）
```

**并行边界：** T1、T2、T3 文件互不重叠。T4 依赖 T3 并会扩展 `remote_access_test.go`，不能与 T3 同时修改该测试文件；T5 只改 `internal/tui`，可与 T4/T6 并行。T7 先提供稳定 Manager API，之后 T8 与 T9 才能并行接入；T8 与 T9 修改不同目录。T10 与 T11 修改不同功能文件，但共享 CLI/UI 集成环境时应隔离测试端口和 fixture。并行执行不得同时编辑同一文件、共用测试端口或覆盖同一 fixture。

**资源安排：** 并行批次 1/2 以轻量代码与定向测试为主；集成测试及完整测试套件错峰执行。依据本机 AGENTS.md，重型测试优先使用免费云端 CI；调用前需说明用途及权限并取得用户许可。每个任务仍需运行其定向验证，不因并行而跳过。

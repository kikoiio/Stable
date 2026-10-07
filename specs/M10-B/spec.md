# M10-B WebSocket Remote 与浏览器会话 Spec

## 背景

M10-A 已为本机脚本提供一次性 Print，并加入 provider 选择。本地 TUI 与 conversation service 已支持流式 run、会话恢复、工具权限审批、计划审批、用户提问和目标状态查询，但当前 IPC 只供本机进程使用。迁移地图将 WebSocket remote 归入 M10，并要求远程权限遵循本地 TUI 边界、目标状态可见；访问控制、模型凭据和工作目录授权必须单独确定。

## 目标

- 允许用户从浏览器连接本机 Stable，并通过 WebSocket 使用通用 agent 会话。
- 在目录被本机用户批准后，远程会话复用现有 conversation service、runner 和权限门。
- 浏览器可查看会话、接收流式回答、取消 run、处理权限/计划/提问交互并查看目标状态。
- 默认只监听 loopback；显式启用 LAN 时使用 remote 服务配置的 TLS 证书。
- remote 服务显式启停，避免普通 `stable up` 意外开放网络入口。

## 功能需求

- **F1 Remote 生命周期：** 提供 `stable remote up|down|status`。`remote up` 在需要时启动本机 runtime 并启动 remote 服务；`remote down` 仅关闭 remote 服务；`stable down` 同时关闭 remote 与 runtime。remote 默认绑定 loopback；只有显式指定非 loopback 地址才允许 LAN 访问。
- **F2 传输与 TLS：** 浏览器静态界面和 WebSocket 由同一 origin 提供。loopback 可使用本机 HTTP；非 loopback 监听必须配置 TLS 证书与私钥，否则启动失败。不得在传输失败时降级为明文 LAN。
- **F3 配对认证：** 新浏览器须输入由本机 CLI 生成的一次性配对 token。token 有短时有效期、只能成功使用一次，服务端仅保存不可逆摘要；成功配对后使用受保护的浏览器会话凭证。浏览器会话凭证只保存在内存中，remote 或 runtime 停止/重启后立即失效，浏览器必须重新配对。未认证连接不得读取会话、目录、目标或模型信息。测试默认 token 有效期为 5 分钟。
- **F4 Origin 与凭据保护：** HTTP 与 WebSocket 只接受 remote 自身 origin；拒绝跨站 WebSocket。模型 API key 只由服务端私有配置或 provider 专属环境变量读取，不发送到页面或远程 protocol，也不得写入 remote 日志。
- **F5 目录请求与本机批准：** 每个远程 WebSocket 连接必须请求一个项目目录，并由本机 TUI 弹窗批准该连接对该目录的访问。弹窗显示远程客户端标识和解析后的绝对目录；批准仅对当前连接有效。目录不存在、路径解析失败、被拒绝或超时均 fail closed。默认等待 2 分钟；本机 TUI 不在线时请求超时并拒绝。
- **F6 会话与执行：** 目录获批后，远程客户端可以创建、列举和加载该目录中的普通会话，并发起、订阅及取消 run。服务端复用现有 runner、工具执行器和权限审批；浏览器不能提交或修改可信 permission bounds、provider key、服务端路径映射或 runtime 控制命令。断线不自动批准新目录，也不绕过重连后的目录审批。
- **F7 浏览器交互：** 页面提供会话列表/切换、流式聊天、run 取消、现有工具权限审批、计划审批和 ask_user 提问交互；会话连接断开后可重新订阅现有 run 和恢复会话事实。交互请求在服务端等待现有超时/取消规则，不因 browser UI 绕过权限门。
- **F8 目标状态可见：** 页面显示本机 Stable 的目标摘要、状态和最近事件；本里程碑中目标内容为只读，不从远程页面创建、修改或确认长期目标。
- **F9 管理功能边界：** 页面不提供 hooks、skills、MCP 配置管理、候选接收、snapshot rewind、runtime 管理或任意本机文件浏览。这些操作仍由本机已有入口完成。

## 非功能需求

- **N1 默认拒绝：** 未认证、Origin 不符、TLS 缺失、目录未批准、审批超时及权限验证失败均拒绝请求，不静默降级。
- **N2 权限等价：** 远程执行在获批目录内使用与本地 TUI 相同的 permission policy、只读/写入边界和逐次审批规则；远程传输本身不扩大权限。
- **N3 凭据与会话安全：** 配对 token、浏览器会话凭证、模型密钥不得进入 transcript、日志、错误正文或前端存储。浏览器会话凭证使用 HttpOnly、SameSite 限制；启用 TLS 时设置 Secure。浏览器会话凭证不跨 remote/runtime 重启持久化。
- **N4 资源限制：** 限制 HTTP body、WebSocket 帧、并发连接和认证失败频率；连接关闭时释放订阅、context 和 goroutine。
- **N5 可重复验收：** 使用本机 fake provider、临时配置、自签测试证书和自动化 TUI 审批替身完成验证；不依赖真实 API key 或公网。

## 不做的事

- 不把 remote 默认绑定到 LAN 或公网；不自动发现、端口转发或部署公网 relay。
- 不向浏览器发送模型 API key，也不接受浏览器上传的 provider key。
- 不允许客户端任意访问目录；每个 WebSocket 连接都必须通过本机 TUI 的目录授权。
- 不复刻完整 TUI 管理面；本期不提供 hooks/skills/MCP 管理、候选接收、rewind 或长期目标写操作。
- 不改变本机 conversation socket 的传输或访问控制协议。

## 验收标准

- **AC1（F1/F2）：** `remote status` 可报告运行态；remote 默认仅监听 loopback。显式 LAN 绑定缺少证书时启动失败；配置有效 TLS 后才接受连接。`stable down` 后 remote socket 不再接受连接。
- **AC2（F3/F4/N1/N3）：** 一次性 token 可配对一次并在过期、重用或错误时被拒绝；未认证/跨站 WebSocket 不得取得会话数据；模型密钥不出现在浏览器、响应、日志和 transcript。
- **AC3（F5/N1）：** 每个 WebSocket 连接都先收到本机 TUI 的目录批准；批准后只允许访问所批准的规范目录。拒绝、路径越界、TUI 离线和超时均 fail closed。
- **AC4（F6/F7/N2）：** fake provider 下浏览器完成会话创建/恢复、聊天流式接收、run 取消、权限审批、计划审批和提问往返；可信 permission bounds 由服务端生成，远程连接不能绕过审批。
- **AC5（F8/F9）：** 页面可见目标状态和最近事件；远程请求不能更改目标，且管理功能不暴露未纳入本期的操作。
- **AC6（N4/N5）：** 断开连接后订阅与 goroutine 释放；帧/请求/并发/认证频率限制有自动化验证。全流程只用 fake provider、临时配置和测试证书。
- **AC7：** `go test -p 2 ./...` 通过，README 记录监听、TLS、配对、目录批准和启动/停止行为。

## 方案比较

1. **推荐并按已确认选择撰写：** 显式启停的 remote 服务，同源托管浏览器界面；默认 loopback，LAN 通过配置 TLS 显式开启；一次性配对 token；每连接由本机 TUI 批准目录。可复用 conversation/runner 权限边界，且普通 runtime 启动不会意外暴露远程服务。
2. **仅提供 WebSocket API：** 实现面较小，但没有内置可用客户端；浏览器跨域部署会增加认证和 Origin 管理复杂度。
3. **完整复制所有 TUI 功能：** 功能覆盖广，但会把 hooks、MCP、skills、rewind 和候选接收纳入本次安全评审，超出已确认的核心远程会话范围。

## 明确默认值

- 配对 token 有效期 5 分钟、成功使用一次。
- 浏览器会话凭证只保存在内存中；remote 或 runtime 停止/重启后失效，重新配对即可恢复访问。
- 本机 TUI 目录批准等待 2 分钟；没有在线 TUI 时拒绝。
- 目录批准只对产生该请求的 WebSocket 连接有效；重连必须重新批准。
- `remote up` 可自动启动 runtime；`remote down` 保留 runtime，`stable down` 关闭二者。

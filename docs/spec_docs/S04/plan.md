# S04 Linux 沙箱、网络与 KiCad 工作流 Plan

> 状态：已完成（2026-10-07 系列收尾确认）。基于已批准的 [spec.md](spec.md)。本计划只交付 Linux x86_64；非 Linux 保留可编译 stub 和明确 unsupported 结果。

## 架构概览

S04 采用五层闭环，保留现有平台边界，不新增第二套沙箱：

1. **隔离层（`internal/platform/sandbox`）**：Linux manager 继续是唯一的受保护执行入口。profile 校验、每次隔离探测、正式/候选/运行目录挂载、环境过滤、网络代理挂载和进程组清理都集中在这里；任何失败都返回不可用或明确的拒绝原因。
2. **执行编排层（`internal/execution`）**：从可信 `permission.Authority` 统一生成一次性命令和 Python bridge 的 sandbox profile。执行前固定并校验网络授权，profile 携带授权给一次性运行；持久 GUI session 明确拒绝网络授权。所有错误在这里映射为工具拒绝、超时、阻断或未知结果，避免调用方自行降级。
3. **KiCad 能力层（`internal/platform/kicad`）**：新增统一的 Linux 工具链和显示能力描述：工具路径、版本探测、KiCad 模板/配置来源、Xvfb、窗口枚举和截图工具状态。无头 ERC 与 GUI worker 使用同一份能力规则和私有运行目录约束，`doctor` 复用同一探测结果。
4. **工作流层（`internal/candidate` 与 Python workers）**：ERC checker 保留摘要前后校验、报告安全读取和验收阻断语义；computer worker 保留会话代际协议，补齐显示、截图、进程和锁的失败分类。两条工作流只接收已验证的 profile 和能力信息，不自行访问正式工程或主机网络。
5. **诊断与验收层（`internal/runtime`、测试与文档）**：`doctor` 报告隔离环境中的实际能力及原因；沙箱、网络、ERC、GUI 和回归测试分别覆盖契约与真实 Linux 前置条件；S00 矩阵记录 S04 实际证据和未执行项目。

数据流为：

```text
permission.Authority
        │
        ▼
execution profile builder ── pins grants ──► sandbox manager
        │                                      │
        ├──────── kicad ERC bridge ◄───────────┤
        └──────── computer session ◄──────────┘
                         │
                         ▼
              bounded result / evidence
```

## 核心数据结构

### `SandboxProfile`

沿用现有 profile，并保证 `NetworkGrants` 只包含已固定 `ResolvedIPs` 的授权。profile 校验拒绝空地址、重复授权、非法挂载和持久会话授权。

### `NetworkGrantPinner`

接收 `permission.NetworkGrant` 列表，解析并固定地址，返回规范化授权；解析失败或结果为空时返回可分类错误。一次性执行每次重新 pin，代理每次拨号前重新解析并比对固定地址。

### `SandboxCapability`

```go
type SandboxCapability struct {
    Name      string
    Available bool
    Detail    string
}
```

用于区分 bubblewrap、代理助手、显示和工具能力不可用原因。

### `ToolStatus` 与 `Capabilities`

```go
type ToolStatus struct {
    Name      string
    Path      string
    Version   string
    Available bool
    Detail    string
}

type Capabilities struct {
    Python       ToolStatus
    CLI          ToolStatus
    GUI          ToolStatus
    Display      ToolStatus
    Window       ToolStatus
    Screenshot   ToolStatus
    TemplateRoot string
    Sandbox      SandboxCapability
}
```

`Discover(ctx, sandboxManager, profile) (Capabilities, error)` 同时检查主机可定位性与隔离环境可见性；`Environment(runRoot string) []string` 生成私有 KiCad 配置、缓存和数据目录变量；`RequiredFor(kind)` 声明 ERC、GUI、截图各自需要的能力集合。

### 执行 profile 构造器

- `CommandSandboxProfile(authority, runRoot, timeout, pinner)`：从可信权限范围生成一次性命令 profile，固定并携带网络授权。
- `BridgeSandboxProfile(request, authority, runRoot, pinner)`：为 ERC、依赖收集和 computer bridge 复用相同根目录、环境和网络边界。
- `SessionSandboxProfile(...)`：拒绝非空网络授权后再建立持久 session，保留 session ID、candidate ID 和 generation 绑定。

这些构造器只接受已经由 `ForRun` 校验过的 authority，不接受客户端自行扩大的路径或 grant。

## 模块设计

### `internal/platform/sandbox`

**职责：** 保持 Linux namespace/bubblewrap 隔离、profile 校验、网络代理挂载、Probe、一次性命令和持久会话生命周期。

**改动：** 将网络授权校验和代理启动错误统一为可识别的不可用原因；补齐取消/超时后的进程组和临时目录清理；为能力探测提供可供 `doctor` 和 KiCad resolver 复用的结果。

**依赖：** `permission.NetworkGrant`、`platform/proc`、`platform/ipc`、`secfile`。

### `internal/execution`

**职责：** 将可信 authority 转成一次性命令、helper、ERC bridge 和 GUI bridge 的 profile。

**改动：** 统一 pin grant、profile 根目录和环境变量；把 `authority.Network` 接到一次性 `command`/bridge；保留持久 session 网络拒绝；统一错误到工具结果的映射。

**依赖：** `permission`、`platform/sandbox`、`platform/kicad`、现有候选/会话协议。

### `internal/platform/kicad`

**职责：** 解析并验证 Linux 下 Python、`kicad-cli`、`eeschema`、Xvfb、窗口探测和截图工具，以及 KiCad 模板和私有配置布局。

**改动：** 提供 ERC/GUI 所需能力集合和隔离可见性探测；让 resolver 输出可传入 profile 的环境/挂载信息；缺失能力返回具体原因。

**依赖：** `platform/sandbox`、`platform/secfile`、标准进程执行 API。该包不访问候选内容，也不负责自动安装。

### `internal/candidate`

**职责：** 运行候选 ERC 并把结果纳入 review/acceptance。

**改动：** 通过 KiCad resolver 获得版本、模板和报告路径；保持正式/候选 digest 前后校验、8 MiB 报告上限和 unavailable 阻断；禁止在 checker 中直接主机运行 KiCad。

**依赖：** `platform/kicad`、`platform/sandbox`、`platform/secfile`。

### `workers/kicad` 与 `workers/computer`

**职责：** 在 guest 中执行 ERC/依赖收集和 GUI 会话协议。

**改动：** 只使用 profile 传入的私有 XDG/显示配置；GUI 统一窗口身份、截图证据、代际和停止清理错误；不改变 capability JSON 的成功状态。

**依赖：** guest 可见的 `/workspace/project`、`/workspace/candidate`、`/workspace/run` 和受限工具路径。

### `internal/runtime`

**职责：** 对用户展示真实运行能力。

**改动：** `doctor` 复用 sandbox/KiCad capability discovery，分别报告 host lookup、isolated visibility、display 和 screenshot 状态；保留既有基础文件检查。

**依赖：** `appconfig.Paths`、`platform/sandbox`、`platform/kicad`。

## 模块交互

1. `ForRun` 从持久化权限边界构造 authority，并创建私有 run root。
2. 一次性命令或 bridge 请求进入 execution profile builder。
3. builder 对 authority 中的 grant 做 pinning，校验根目录和运行类型，生成 profile。
4. sandbox manager 先 Probe，再启动隔离命令/bridge；有 grant 时启动私有代理并保持 network namespace。
5. ERC/GUI worker 返回 bounded JSON；execution 校验证据路径、状态和摘要，candidate 或会话层更新结果。
6. `doctor` 使用相同 profile 规则执行轻量 capability probe，输出每项能力及原因。

## 文件组织

```text
internal/platform/kicad/
├── capabilities.go          — ToolStatus、Capabilities、Discover、RequiredFor
├── capabilities_linux.go    — Linux 工具/模板/显示探测与隔离可见性检查
├── capabilities_other.go    — 非 Linux 明确不可用结果
└── capabilities_test.go     — 纯契约与 fake sandbox 测试

internal/platform/sandbox/
├── process.go               — profile/能力校验补充
├── process_linux.go         — grant 代理、Probe、清理和错误分类
├── session.go               — 持久会话网络拒绝与清理
├── network.go               — grant pinning/重解析/代理错误
└── *_test.go                — 隔离、网络、取消、残留测试

internal/execution/
├── sandbox_profile.go       — authority 到 profile 的统一构造
├── tool_executor.go         — command/helper 使用统一 profile
├── python_bridge.go         — ERC/GUI bridge 能力与证据校验
└── *_test.go                — grant 接线、fail-closed、生命周期测试

internal/candidate/
├── erc_checker.go           — resolver 接入和报告/摘要边界
└── erc_checker_test.go      — 工具缺失、报告异常、摘要漂移测试

internal/runtime/
├── doctor.go                — 输出隔离可见能力和原因
└── doctor_test.go           — 能力组合和假阳性测试

workers/kicad/
├── erc.py / dependencies.py — 使用 profile 的私有配置和工具约束
└── test_*.py                — ERC/依赖边界回归

workers/computer/
├── bridge.py                — GUI 显示、截图、代际、清理错误分类
└── test_candidate.py        — 会话与证据边界回归

docs/spec_docs/S04/
├── spec.md
├── plan.md
├── task.md
└── checklist.md

docs/spec_docs/S00/platform-capability-matrix.md
                            — 记录 S04 Linux 状态、证据与未执行项
```

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 隔离实现 | 继续使用现有 Linux bubblewrap/namespace | 已有正式/候选挂载和代理语义，避免引入第二套隔离模型 |
| grant 固定时机 | 每次一次性执行 profile 构造时 pin，并由代理每次拨号重验 | 授权范围与实际 DNS 状态绑定，避免长期缓存地址 |
| 持久 session 网络 | 明确拒绝 | 现有 session 控制协议没有逐请求网络授权边界，扩大范围会削弱安全证明 |
| KiCad 能力发现 | 新增统一 resolver，ERC、GUI、doctor 共用 | 消除主机 PATH 与隔离 PATH 的口径差，集中报告原因 |
| 能力不可用 | fail closed，返回结构化 detail | 安全能力不能通过主机直跑或普通覆盖降级 |
| GUI 协议 | 保留现有 capability JSON，扩展错误类别 | 避免破坏 store/session 的既有状态和调用方 |
| 测试策略 | 纯契约测试 + Linux 环境定向测试 + 端到端前置条件记录 | 既覆盖安全状态机，又如实区分本机依赖证据 |
| 非 Linux | 保留可编译 stub 和明确 unsupported | 维持仓库跨目标编译边界，但不宣称支持 |

## 需求归属

| 需求 | 主要归属 | 验证入口 |
|------|----------|----------|
| F1 / N1 | `platform/sandbox`、`execution` | 隔离 profile、Probe、根目录和宿主哨兵测试 |
| F2 | `platform/sandbox/network`、`execution` | grant pinning、代理连接、解析变化和 session 拒绝测试 |
| F3 | `platform/kicad`、`candidate`、`workers/kicad` | ERC 版本、报告边界、摘要漂移和阻断测试 |
| F4 | `platform/sandbox/session`、`workers/computer` | GUI 启动、观察、截图、代际和清理测试 |
| F5 | `platform/kicad`、`runtime/doctor` | 隔离可见性与 doctor 假阳性测试 |
| F6 / N6 | `sandbox`、`execution`、`runtime` | 拒绝/阻断/超时/取消/清理错误分类测试 |
| F7 / N4 | `candidate`、现有 lifecycle 与权限模块 | S03 回归测试和候选边界测试 |
| N2 / N3 | Linux adapter、契约测试、文档 | Linux 构建、定向测试和环境证据记录 |
| N5 | `sandbox/process`、`sandbox/session`、workers | 超时取消后的进程树和运行目录检查 |
| N7 | `docs/spec_docs/S04`、S00 矩阵 | 文档完整性与证据位置审阅 |

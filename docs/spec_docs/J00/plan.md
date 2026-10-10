# J00 云端兼容性与候选隔离试验 Plan

## 架构概览

J00 的实现保持在试验工具和 CI 层，不改 Stable 运行时核心。

1. **夹具与来源层**：保存公开最小 `.eprj3` 夹具的 manifest、文件摘要和独立预期，试验开始前校验完整性。
2. **云端环境层**：在 GitHub Actions 中固定 Ubuntu x64 runner，下载并校验官方客户端，建立独立 `HOME`、XDG、运行目录和项目内 `.tmp`，准备 Xvfb/X11 及资源观测。
3. **探针编排层**：按 discover → start → identify → observe → apply → save → reopen → check → export → cleanup 顺序执行；每一步都记录真实命令状态和结构化结果。
4. **隔离验证层**：为候选复制独立 profile 与运行目录，对正式根建立只读边界，比较正式工程和关联身份的前后摘要，验证自动保存、恢复和桥接是否越界；无法证明的能力返回受限/不可用。
5. **证据层**：生成机器可读的 J00 报告、能力矩阵、脱敏日志和产物清单，所有结论绑定客户端/探针/夹具/提交摘要。
6. **workflow 与契约测试层**：提供手动触发的 J00 workflow，以及不依赖真实客户端的假后端契约测试；真实客户端只在获授权的云端 job 中运行，单 job 单实例。

推荐以 Python 标准库实现编排和 JSON 证据，因为需要可靠的超时、进程树、摘要和失败分类；shell 仅负责 workflow 入口和环境准备。无头客户端统一使用 X11/Xvfb，并把临时文件导向项目目录。

## 核心数据结构

### RunContext

记录 `run_id`、提交 SHA、runner 镜像/架构、客户端版本与安装包 SHA-256、探针版本、夹具摘要、开始/结束时间和允许环境变量摘要。

### CapabilityObservation

记录能力 ID、状态（`verified` / `limited` / `unavailable` / `unverified`）、对应阶段、原因、证据引用和依赖能力。

### ProjectSnapshot

记录工程根与入口的受控相对路径、格式、文件 manifest 摘要、工程身份、器件/引脚/网络计数、快照摘要和来源会话。

### OperationObservation

记录操作 ID、目标对象、期望旧值、规范化新值、候选/会话代次、前后快照引用、真实退出状态、结果和错误分类。

### ArtifactRecord

记录产物类型、候选内相对路径、MIME、大小、SHA-256、来源快照、生成操作和校验结果。

### SessionRecord

记录会话 ID、generation、客户端/桥接 PID 归属、profile 与工程路径摘要、启动/关闭时间、退出状态和清理结果。

### J00Report

汇总以上记录、每阶段结果、正式根前后摘要、隔离结论、资源观测、脱敏日志索引和退出结论。

## 核心接口

- `FixtureVerifier.verify(manifest, fixture_root) -> FixtureRecord`：校验夹具文件集、摘要和独立预期。
- `ProcessSupervisor.run(argv, env, cwd, timeout) -> CommandResult`：执行单个受监督命令，记录 stdout/stderr 摘要、退出码、超时和进程树；`terminate_tree()` 等待真实退出。
- `ClientProbe.discover() -> CapabilityObservation[]`：发现版本、CLI、MCP/桥接、隐藏窗口和扩展能力。
- `ClientProbe.start(profile, project) -> SessionRecord`：在独立 profile 中启动客户端并绑定工程；返回 session/generation。
- `ClientProbe.identify(session) -> ProjectSnapshot`、`observe(session) -> ProjectSnapshot`：识别和读取受控工程。
- `ClientProbe.apply_parameter(session, operation) -> OperationObservation`：按对象 ID 与期望旧值执行一次参数探针。
- `ClientProbe.save(session)`、`reopen(project, fresh_session) -> ProjectSnapshot`：保存后使用新会话确认持久化。
- `ClientProbe.check(session_or_frozen_candidate, kind) -> StepResult`：分开执行规则、网络/结构和其他适用检查。
- `ClientProbe.export(candidate, kind, output_dir) -> ArtifactRecord`：导出并校验 BOM、网表、截图和报告来源。
- `IsolationVerifier.prepare()`、`verify_formal_unchanged()`、`verify_candidate_identity()`、`finalize() -> IsolationResult`：建立并验证正式根只读、候选身份、离线边界、自动保存/恢复和清理。
- `EvidenceWriter.write(report, logs, artifacts)`：输出 JSON、脱敏日志索引、Markdown 摘要和 artifact manifest。

真实客户端通过命令/协议适配器实现 `ClientProbe`；同一接口提供 fake backend 给契约测试，避免把未知的官方 schema 写死在编排层。

## 模块设计

### 夹具与预期

**文件：** `fixtures/lceda/j00/manifest.json`、公开 `.eprj3` 文件集、`expected.json`

**职责：** 列出入口、允许文件、文件摘要、工程身份预期、器件/参数/引脚/网络预期和可执行探针的旧值/新值；禁止把未列出的缓存或凭据纳入夹具。

**依赖：** 官方公开夹具或经授权生成的最小夹具；J00 运行前必须有固定摘要。

### 环境准备与客户端安装

**文件：** `scripts/lceda/j00_env.sh`

**职责：** 检查 Ubuntu x64、记录 `df`/内存/cgroup、创建项目内 `.tmp`、独立 `HOME`/XDG/profile、准备 Xvfb/X11，按 pinned URL 与 SHA-256 下载并安装官方客户端；不保存账号状态。

**依赖：** GitHub Actions runner、官方公开下载源、系统依赖；不依赖 Stable 运行时。

### 探针契约与编排

**文件：** `scripts/lceda/j00_contracts.py`、`scripts/lceda/j00_probe.py`

**职责：** 定义上述记录和状态枚举，串联固定阶段，施加超时和单实例约束，调用 fake 或真实 `ClientProbe`，在任一关键证据缺失时 fail closed。

**依赖：** 环境记录、夹具 manifest、进程监督、客户端适配器、隔离验证器。

### 客户端命令/协议适配器

**文件：** `scripts/lceda/j00_client.py`

**职责：** 将固定的 CLI/MCP/桥接探针映射到 `ClientProbe` 接口，规范化版本、工程快照、参数操作、保存/重开、检查与导出返回；不向模型暴露任意脚本入口。

**依赖：** J00 实测得到的官方命令和返回 schema；未知或不稳定接口返回 `unavailable`/`unverified`。

### 隔离与生命周期监督

**文件：** `scripts/lceda/j00_isolation.py`

**职责：** 建立正式根只读视图、候选副本、独立 profile/运行目录和候选身份快照；监控自动保存、恢复、桥接绑定与离线边界；结束时清理并核验进程树。

**依赖：** `ProcessSupervisor`、系统文件摘要、可用的网络/显示隔离能力；无法证明的边界不得标记通过。

### 证据与报告

**文件：** `scripts/lceda/j00_evidence.py`

**职责：** 脱敏并保存 JSON 报告、命令日志摘要、产物 manifest、能力矩阵和 Markdown 摘要；校验产物非空、格式、来源和候选摘要。

**依赖：** `J00Report`、候选文件集、workflow artifact 目录。

### 契约测试与假后端

**文件：** `tests/lceda/j00/fake_client.py`、`tests/lceda/j00/test_probe_contract.py`、`tests/lceda/j00/test_isolation_contract.py`

**职责：** 使用可控假客户端覆盖正常、超时、崩溃、旧值不符、重开失败、导出失败、未知写结果和残留进程；验证状态分类、证据绑定和清理，不模拟真实客户端能力结论。

**依赖：** Python 标准库和临时目录；无需下载客户端或访问网络。

### 云端 workflow

**文件：** `.github/workflows/lceda-j00.yml`

**职责：** 仅手动触发，固定 runner、权限、超时和并发；按环境准备 → 探针 → 清理 → 上传脱敏报告顺序运行。默认不取 secrets、不上传非公开数据，失败也执行清理。

**依赖：** 上述脚本、公开夹具、官方安装包下载；首次触发前需要用户明确授权。

### 文档与忽略规则

**文件：** `docs/spec_docs/J00/{spec,plan,task,checklist}.md`、`.gitignore`

**职责：** 保留阶段规格与实际证据；为 J00 文档目录加入显式忽略例外，运行产物目录保持忽略，避免报告和缓存混入源码提交。

## 模块交互

```text
手动 workflow_dispatch
  → j00_env.sh
      → 记录 runner/资源
      → 下载并校验客户端
      → 建立 HOME/XDG/.tmp/Xvfb
  → j00_probe.py
      → FixtureVerifier 校验 manifest 与夹具摘要
      → IsolationVerifier.prepare 建立正式基线、候选与身份记录
      → ClientProbe.discover
      → ClientProbe.start(candidate)
      → identify/observe 记录基线快照
      → apply_parameter（仅允许 manifest 中的对象和旧值）
      → save → 关闭写会话
      → 新 generation reopen → observe 持久化结果
      → 分别执行 check 与 export
      → ClientProbe.close
      → IsolationVerifier.verify_formal_unchanged / verify_candidate_identity
      → ProcessSupervisor 终止并核验本次进程树
  → j00_evidence.py
      → 生成 JSON/Markdown 报告、能力矩阵、产物 manifest
      → workflow 上传脱敏失败/成功证据
```

任一关键阶段失败都会停止后续写入或接收假设，继续执行可安全的关闭、清理和证据写入；不能确认的能力进入 `limited`、`unavailable` 或 `unverified`。

## 文件组织

```text
fixtures/lceda/j00/
├── manifest.json
├── expected.json
└── minimal.eprj3/

scripts/lceda/
├── j00_env.sh
├── j00_contracts.py
├── j00_probe.py
├── j00_client.py
├── j00_isolation.py
└── j00_evidence.py

tests/lceda/j00/
├── fake_client.py
├── test_probe_contract.py
└── test_isolation_contract.py

.github/workflows/lceda-j00.yml
docs/spec_docs/J00/
├── spec.md
├── plan.md
├── task.md
├── checklist.md
├── compatibility-report.md
└── capability-matrix.md
```

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 编排语言 | Python 标准库 | 现有仓库已有 Python worker/test；标准库足以处理进程树、超时、JSON、摘要和脱敏，减少新依赖 |
| 客户端接入 | `ClientProbe` 适配器与配置化命令/协议 | J00 先测 Linux 实际能力，避免把未知 CLI/MCP schema 固化进 Stable 核心 |
| 环境入口 | Bash 准备环境，Python 执行探针 | Bash 适合 workflow 和系统工具，Python 负责可测试的状态与错误分类 |
| 客户端来源 | 官方 URL + 固定 SHA-256，二进制不入仓库 | 可重复且避免把受许可软件提交到源码 |
| workflow 触发 | 首版仅 `workflow_dispatch`、`contents: read`、单 job 单实例 | 需要用户授权，避免在普通 PR 上自动下载/启动客户端和产生重型负载 |
| 隔离策略 | 候选副本、独立 profile/运行目录、正式根只读、身份摘要前后比较 | 同一路径复制不足以证明工程/云端身份独立，必须验证实际边界 |
| 离线策略 | 优先客户端离线模式和无凭据环境；可用时增加网络命名空间/出口观测，能力不足则标记受限 | 不把“未联网”假设写成证据，明确记录实际覆盖 |
| 资源与临时文件 | 单实例、有界超时和项目内 `.tmp`/artifact 目录 | 遵守仓库资源约束，避免 `/tmp` 配额和并行 GUI 负载 |
| 状态语义 | 细分步骤结果与能力状态，失败关闭 | 命令成功、截图生成和单项检查不能替代完整隔离/持久化结论 |
| 测试策略 | fake backend 契约测试 + 获授权云端真实客户端试验 | 本机/PR 测试保持轻量，真实 GUI 与客户端只在云端验证 |

## 自检

- **spec 覆盖：** F1–F10 均有模块或接口归属；N1–N8 通过 runner 限定、数据保护、固定版本/摘要、隔离验收、证据边界和资源约束落实。
- **接口完整性：** 夹具校验、进程监督、发现、启动、读取、修改、保存、重开、检查、导出、隔离和报告写入均有接口定义。
- **依赖清晰度：** 环境准备先于探针；夹具和隔离先于写入；清理与证据写入覆盖成功和失败路径；fake backend 不依赖真实客户端。
- **矛盾检查：** 未将 J00 扩大为生产适配器或真实用户工程处理；未把未验证能力写成支持承诺；云端 workflow 保持手动触发并需要显式授权。

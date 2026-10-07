# S00 平台支持范围与能力契约 Plan

> 状态:已完成（2026-10-07 系列收尾确认）。依据已批准的 spec.md(docs/spec_docs/S00/spec.md)。

## 架构概览

S00 的产物是「四件套流程文档 + 一份唯一交付物 + 一处构建配置修改」:

- `docs/spec_docs/S00/spec.md` / `plan.md` / `task.md` / `checklist.md` —— 流程四件套
- `docs/spec_docs/S00/platform-capability-matrix.md` —— **唯一交付物**(单一事实源)
- `.gitignore` —— 追加 S00 白名单条目(不改其他忽略规则)

**能力表文档内部结构**(六个区块,与 spec 的 F 需求一一对应):

| 区块 | 内容 | 覆盖 |
|------|------|------|
| 1 支持范围声明 | 仅 Linux 受支持;macOS/Windows 不作支持承诺;判定依据 | F1 |
| 2 矩阵总览 | 10 行 × 3 平台状态汇总表(一屏可读) | F2、F6 |
| 3 逐行能力详述 | 每行一个小节:范围界定 → Linux 现状与证据 → 安全不变量 → 能力不可用行为 → 验收条件 → 等价机制线索 | F2–F6 |
| 4 发布门槛清单 | 平台声明「支持」前必须通过的事项;KiCad 规则 | F7 |
| 5 完成标志对照 | 逐条对照路线图阶段 0 完成标志 | F8 |
| 6 审计差异记录 | 路线图盘点与代码实况不符之处 | N3 |

## 核心数据结构

### 能力行模板

区块 3 每行小节的统一结构:编号(C01–C10)、能力名称(用路线图术语)、范围界定(含/不含什么)、Linux 状态 + 证据、macOS/Windows 状态、等价机制线索、安全不变量(≥1 条)、能力不可用行为、验收条件(按状态等级分组)。

### 状态取值

`支持 / 降级 / 不支持 / 未评估` 四值;Linux 列禁用「未评估」;macOS/Windows 本阶段固定「未评估(无支持承诺)」。

### 证据类型

`可运行验证`(附验证命令或测试名,撰写时须在本机跑通)/ `仅代码审阅`(须说明为何不可运行)。

### 验收条件编号方案

`AC-<域>-<序号>`,10 个域缩写对应 10 行(如 AC-PATH-1、AC-IPC-2、AC-CAND-1);每条写成「运行什么 → 期望什么」;声明「支持」须满足该行全部条目,声明「降级」须满足标注〔降级〕的子集。

### 审计来源映射

初始映射(来自路线图盘点,已核实文件存在;审计阶段逐行验证并允许修正):

| 能力行 | 初始映射(代表位置) |
|--------|----------------------|
| 路径与安装布局 | internal/appconfig/config.go、internal/appconfig/owner_unix.go、internal/runtime/paths.go、internal/tui/model.go |
| 本机 IPC | internal/runtime/supervisor.go、internal/conversation/service.go、internal/conversation/client.go |
| 单实例锁 | internal/sandbox/network.go、internal/runtime/supervisor.go |
| 进程树托管 | internal/runtime/supervisor.go、internal/runtime/sessions.go、internal/sandbox/session.go、internal/sandbox/process_linux.go |
| 私密文件与安全存储 | internal/appconfig/owner_unix.go、internal/appconfig/config.go |
| 安全根目录访问 | internal/candidate/workspace.go、internal/candidate/erc_checker.go |
| 候选目录事务 | internal/candidate/accept.go、internal/candidate/snapshot.go、internal/candidate/rewind.go |
| 网络隔离 | internal/sandbox/linux.go、internal/sandbox/network_linux.go、internal/sandbox/network_other.go、internal/sandbox/process_linux.go、cmd/agentworker/main.go |
| 无头 KiCad | internal/runtime/doctor.go、internal/candidate/erc_checker.go |
| 交互 GUI 会话 | internal/runtime/sessions.go、internal/runtime/doctor.go |

## 模块设计(审计工作流)

```
路线图盘点 + 源码
      │
      ▼
审计(逐行,10 行可并行取证)
  每行:核实映射文件内容相符 → 定位机制细节 → 尝试映射到可运行验证
  (go test 包 / Makefile 目标 / doctor 等命令;不可运行才标「仅代码审阅」)
  发现矩阵外依赖点 → 增补行候选;发现路线图失实 → 差异记录
      │
      ▼
写作(审计记录 → 填充能力表文档六区块,四处对齐自查 N1)
      │
      ▼
跟踪(.gitignore 白名单 → git ls-files 验证 AC9)
      │
      ▼
checklist 验收(AC1–AC9 + 抽验可运行证据)
```

**审计原则**:证据反映当前真实行为而非目标行为;每行至少尝试一个可运行验证,映射优先级为 定向 `go test` > Makefile 目标 > 手工可观察行为。

## 文件组织

```
docs/spec_docs/S00/
├── spec.md
├── plan.md
├── task.md
├── checklist.md
└── platform-capability-matrix.md   — 唯一交付物
.gitignore                           — 追加 S00 白名单条目
```

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 交付物形态 | 单一 Markdown 文档 | 用户已批(方案 A);阶段 1–5 引用方便 |
| 矩阵行数 | 固定 10 行 + 允许标注「补充」的增补行 | 与路线图阶段 0 条目一一对应,又覆盖审计新发现 |
| 验收条件编号 | AC-域-序号,一经使用不重命名 | 阶段 1–5 可稳定引用 |
| Linux 证据标准 | 优先可运行验证,不可行才「仅代码审阅」 | 路线图要求 Linux 现状作回归基线;可运行证据才可复验 |
| macOS/Windows 内容 | 统一「未评估」+ 等价机制线索(标注未验证) | 用户决策:暂只声明 Linux;线索仅为后续阶段起点 |
| 审计深度 | 文件级引用 + 逐行验证尝试,不逐函数审阅 | 平衡工作量与可信度 |
| 差异处理 | 差异记录区块,不改路线图 | spec「不做的事」+ N3;修订决策留给用户 |
| 文档语言 | 中文 Markdown | 与项目现有文档一致 |

# J00 云端兼容性与候选隔离试验 Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|---|---|---|
| 新建 | `fixtures/lceda/j00/manifest.json` | 夹具入口、允许文件、摘要、客户端来源与探针参数 |
| 新建 | `fixtures/lceda/j00/expected.json` | 工程身份、器件/参数/引脚/网络的独立预期 |
| 新建 | `fixtures/lceda/j00/minimal.eprj3/` | 公开最小工程文件集 |
| 修改 | `.gitignore` | 允许 J00 规格文档跟踪，忽略运行产物 |
| 新建 | `scripts/lceda/j00_env.sh` | runner、依赖、Xvfb、独立目录与客户端校验 |
| 新建 | `scripts/lceda/j00_contracts.py` | 记录、状态枚举和 JSON 契约 |
| 新建 | `scripts/lceda/j00_probe.py` | 阶段编排、进程监督与 fail-closed |
| 新建 | `scripts/lceda/j00_client.py` | CLI/MCP/桥接适配与结果规范化 |
| 新建 | `scripts/lceda/j00_isolation.py` | 候选、正式根、身份、离线边界和清理 |
| 新建 | `scripts/lceda/j00_evidence.py` | 脱敏报告、产物校验和能力矩阵 |
| 新建 | `tests/lceda/j00/fake_client.py` | 可控假客户端 |
| 新建 | `tests/lceda/j00/test_probe_contract.py` | 探针/状态/证据契约测试 |
| 新建 | `tests/lceda/j00/test_isolation_contract.py` | 隔离、超时、残留进程契约测试 |
| 新建 | `.github/workflows/lceda-j00.yml` | 手动云端试验 workflow |
| 修改 | `docs/spec_docs/J00/` | 记录实际兼容性报告和能力矩阵 |

## T1：建立公开夹具与 manifest

**文件：** `fixtures/lceda/j00/*`、`.gitignore`
**依赖：** 无

**步骤：**

1. 固定公开最小 `.eprj3` 工程来源、入口和允许文件集。
2. 记录文件 SHA-256、工程身份预期、对象预期和一次参数探针的旧/新值。
3. 在 manifest 中固定客户端来源 URL、版本候选和安装包摘要字段；未知值不得填假值。
4. 为 J00 文档目录加入跟踪例外，为本地/CI 运行产物保持忽略。

**验证：** 运行夹具校验脚本，期望允许文件、摘要和预期字段全部一致；`git diff --check` 通过。

## T2：实现环境准备与安全目录

**文件：** `scripts/lceda/j00_env.sh`
**依赖：** T1

**步骤：**

1. 检查 Ubuntu x64、必要系统命令、磁盘/内存/cgroup 并输出记录。
2. 创建项目内 `.tmp`、独立 `HOME`/XDG/profile、运行和 artifact 目录。
3. 从 manifest 下载官方客户端，校验 SHA-256，拒绝空/不匹配包。
4. 在 X11/Xvfb 下准备隐藏窗口运行参数，清除 `WAYLAND_DISPLAY`，默认禁用 GPU。
5. 设置 trap，确保失败和取消时关闭本任务启动的显示/安装辅助进程。

**验证：** `bash -n scripts/lceda/j00_env.sh`；用假的下载文件验证摘要失败关闭和目录清理。

## T3：定义 Python 契约与状态

**文件：** `scripts/lceda/j00_contracts.py`
**依赖：** 无

**步骤：**

1. 定义 RunContext、CapabilityObservation、ProjectSnapshot、OperationObservation、ArtifactRecord、SessionRecord、J00Report。
2. 定义步骤结果和能力状态枚举、必填字段、时间/摘要格式和脱敏规则。
3. 提供 JSON 序列化/反序列化及 schema 校验入口。

**验证：** `python3 -m py_compile scripts/lceda/j00_contracts.py`；契约测试能拒绝缺少摘要、路径越界和未知状态的记录。

## T4：建立 fake backend 与基础契约测试

**文件：** `tests/lceda/j00/fake_client.py`、`tests/lceda/j00/test_probe_contract.py`
**依赖：** T3

**步骤：**

1. 实现正常读写、旧值不符、超时、崩溃、重开失败、导出失败和未知写结果脚本。
2. 为每个场景产生确定的命令结果、会话代次和工程快照。
3. 编写测试验证状态分类、停止后续写入和证据引用完整性。

**验证：** `python3 -m unittest tests/lceda/j00/test_probe_contract.py`，全部场景通过。

## T5：实现候选隔离与生命周期监督

**文件：** `scripts/lceda/j00_isolation.py`、`tests/lceda/j00/test_isolation_contract.py`
**依赖：** T3

**步骤：**

1. 创建候选副本、独立 profile/运行目录及正式根基线摘要。
2. 对正式根施加只读/路径边界，记录候选工程身份和自动保存/恢复位置。
3. 实现离线模式/网络观测能力探测；无法证明时返回受限。
4. 实现进程树终止、真实退出等待、正式根前后摘要比较和清理证据。
5. 覆盖路径替换、同名工程、超时、残留子进程和中断恢复。

**验证：** `python3 -m unittest tests/lceda/j00/test_isolation_contract.py`；期望正式根摘要不变、残留进程被报告而非静默忽略。

## T6：实现证据与报告

**文件：** `scripts/lceda/j00_evidence.py`
**依赖：** T3

**步骤：**

1. 写入 JSON 报告、阶段结果、能力矩阵、artifact manifest 和 Markdown 摘要。
2. 对环境变量、命令输出和路径执行脱敏，保留退出码与摘要。
3. 校验产物存在、非空、格式/MIME、来源快照和当前候选摘要。
4. 让成功与失败路径都能写出可读报告。

**验证：** 用 T4/T5 的假记录生成报告，期望 schema 校验通过、凭据模式不出现在输出、错误状态可追溯。

## T7：实现客户端适配器

**文件：** `scripts/lceda/j00_client.py`
**依赖：** T3

**步骤：**

1. 实现发现、启动、识别、观察、参数修改、保存、重开、检查、导出和关闭接口。
2. 将官方返回规范化为核心契约；保留原始摘要和真实退出状态。
3. 对未知命令/schema、需激活、超时和不支持能力返回明确分类。
4. 禁止任意脚本或未列入 manifest 的写入路径。

**验证：** 用 fake backend 驱动每个接口；期望正常结果可序列化，未知接口进入 `unavailable`/`unverified`。

## T8：串联 J00 探针

**文件：** `scripts/lceda/j00_probe.py`
**依赖：** T1、T3、T5、T6、T7

**步骤：**

1. 按固定阶段顺序调用夹具校验、隔离准备、发现、启动、读取、受控修改、保存、全新会话重开、检查、导出和清理。
2. 在每个写动作前验证候选身份、对象 ID、旧值和批准范围。
3. 对状态不明的写操作停止重放，继续安全关闭并记录终态。
4. 将所有阶段结果交给证据模块，计算 J00 退出条件。

**验证：** `python3 scripts/lceda/j00_probe.py --backend fake ...`；成功场景生成完整闭环报告，失败场景不越过写入闸门且仍完成清理。

## T9：接入手动 GitHub Actions workflow

**文件：** `.github/workflows/lceda-j00.yml`
**依赖：** T2、T8、T1

**步骤：**

1. 设置 `workflow_dispatch`、`contents: read`、单 job 单客户端、并发取消和明确超时。
2. 顺序运行环境准备、契约测试、真实探针、清理和脱敏 artifact 上传。
3. 不读取 secrets；失败时上传必要脱敏证据并执行清理。
4. 记录 workflow 输入、提交 SHA、客户端/夹具摘要和 runner 资源。

**验证：** 静态检查 YAML、shell 和 Python 引用；使用 fake backend 的 workflow 分支完成一次无客户端 smoke。

## T10：本地集成与回归门禁

**文件：** `tests/lceda/j00/`
**依赖：** T4、T5、T6、T7、T8、T9

**步骤：**

1. 增加单一入口运行全部 J00 契约测试和静态检查。
2. 验证正常、失败、取消、重启和产物来源场景的报告一致性。
3. 确认不启动真实客户端时测试不访问网络、不读取凭据、不写 `/tmp` 大文件。

**验证：** `python3 -m unittest discover -s tests/lceda/j00 -p 'test_*.py'`、`python3 -m py_compile scripts/lceda/*.py`、`git diff --check` 全部通过。

## T11：准备云端试验授权与运行前审查

**文件：** `docs/spec_docs/J00/compatibility-report.md`、`docs/spec_docs/J00/capability-matrix.md`
**依赖：** T9、T10

**步骤：**

1. 列出将调用的 GitHub Actions workflow、官方客户端来源、runner 资源、上传 artifact 范围和权限。
2. 确认只使用公开代码/夹具，无账号 profile、密钥或真实工程。
3. 在触发前向用户说明服务、用途、权限、数据范围和潜在费用，取得明确授权。
4. 授权缺失时保持未执行，不用本机 GUI 代替。

**验证：** 审查清单中每项均有记录；未授权时 workflow 未触发。

## T12：执行 J00 云端真实客户端闭环

**文件：** `.github/workflows/lceda-j00.yml` 运行记录、workflow artifacts
**依赖：** T11

**步骤：**

1. 仅在 T11 授权后手动触发 workflow。
2. 观察客户端发现、读取、修改、保存、重开、检查、导出、隔离和清理。
3. 失败时保留脱敏日志、报告和必要截图；不得重复触发掩盖状态不明。
4. 只读查询 workflow 结果、job 状态和 artifact 清单，确认真实退出码与清理结果。

**验证：** 至少一条闭环通过；正式根/关联对象无变化；客户端、桥接、显示服务和子进程均已退出；否则记录明确失败/不可用边界。

## T13：整理兼容性报告与阶段交付

**文件：** `docs/spec_docs/J00/compatibility-report.md`、`docs/spec_docs/J00/capability-matrix.md`
**依赖：** T12

**步骤：**

1. 绑定提交 SHA、客户端版本/摘要、夹具摘要、workflow 链接和 artifact 摘要。
2. 按能力标记已验证、受限支持、不可用或未验证，列出证据和限制。
3. 对照 AC01–AC10 记录实际结果，不把取消、缺凭据或缺报告写成通过。
4. 给出 J01 是否可开始的明确闸门结论。

**验证：** 报告逐项覆盖 AC01–AC10；文档引用的 workflow/job/artifact 可只读查询到；`git diff --check` 通过。

## 执行顺序

```text
批次 0：T1 ─┐
           ├→ T2 ─┐
批次 0：T3 ─┼→ T8 ─┬→ T9 → T10 → T11（授权门）→ T12 → T13
           ├→ T4 ─┘
           ├→ T5 ─┐
           ├→ T6 ─┤
           └→ T7 ─┘
```

- T1 与 T3 可并行；T2 依赖 T1。
- T4、T5、T6、T7 共享 T3 的契约但修改不同文件，可并行；T5 负责隔离路径，T6 负责报告路径，避免交叉写入。
- T8 汇合 T1/T3/T5/T6/T7；T9 再依赖 T2/T8。
- T10 是本地集成汇合点；T11 是云端授权闸门，不能绕过。
- T12 是唯一真实客户端云端任务，单独调度；T13 只读取 T12 的结果并整理文档。
- 任何真实客户端、GUI、下载大文件或云端 job 均不得与其他重型任务并发。

# S04 Linux 沙箱、网络与 KiCad 工作流 Tasks

> 基于已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。任务按依赖 DAG 执行，未完成本文件和 checklist 审批前不进入实现。

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/platform/kicad/capabilities.go` | 工具状态、能力集合和公共发现接口 |
| 新建 | `internal/platform/kicad/capabilities_linux.go` | Linux 工具、模板、显示和隔离可见性探测 |
| 新建 | `internal/platform/kicad/capabilities_other.go` | 非 Linux 明确不可用结果 |
| 新建 | `internal/platform/kicad/capabilities_test.go` | resolver 纯契约测试 |
| 修改 | `internal/platform/sandbox/process.go`、`process_linux.go`、`network.go`、`session.go` | profile、网络授权、探测、清理和 session 边界 |
| 修改 | `internal/platform/sandbox/*_test.go` | 沙箱、代理、取消、残留和 session 测试 |
| 修改 | `internal/execution/sandbox_profile.go`、`tool_executor.go`、`python_bridge.go` | authority 到 profile 的统一接线 |
| 修改 | `internal/execution/*_test.go` | grant 接线和 fail-closed 测试 |
| 修改 | `internal/candidate/erc_checker.go`、`erc_checker_test.go` | KiCad resolver 和报告边界 |
| 修改 | `workers/kicad/erc.py`、`dependencies.py`、对应测试 | 私有配置和工具约束 |
| 修改 | `workers/computer/bridge.py`、`test_candidate.py` | GUI 会话、截图和清理错误 |
| 修改 | `internal/runtime/doctor.go` | 隔离能力诊断 |
| 新建/修改 | `internal/runtime/doctor_test.go` | doctor 组合和假阳性测试 |
| 新建/修改 | `tests/e2e/m03_sandbox_network_test.go`、`tests/e2e/m03_computer_session_test.go`、相关脚本 | Linux 端到端场景 |
| 修改 | `docs/spec_docs/S00/platform-capability-matrix.md` | S04 状态、证据和未执行项 |
| 新建 | `docs/spec_docs/S04/{spec,plan,task,checklist}.md` | 四份 S04 交付文档 |

## T1：建立 KiCad 能力模型与非 Linux stub

**文件：** `internal/platform/kicad/capabilities.go`、`capabilities_linux.go`、`capabilities_other.go`、`capabilities_test.go`  
**依赖：** 无  
**步骤：**

1. 定义 `ToolStatus`、`Capabilities`、`SandboxCapability` 和 ERC/GUI/截图所需能力集合。
2. 实现公共发现入口和私有 XDG 环境构造。
3. Linux 实现检查 `python3`、`kicad-cli`、`eeschema`、Xvfb、窗口探测、截图工具和 KiCad 模板。
4. 非 Linux 返回明确 unsupported，不引入 Linux syscall。
5. 用 fake sandbox 覆盖主机存在但隔离不可见、缺失工具和模板缺失组合。

**验证：** `go test ./internal/platform/kicad/...`；目标平台 `CGO_ENABLED=0 GOOS=darwin/windows go build ./internal/platform/kicad/...` 通过，非 Linux 结果为 unsupported。

## T2：补齐 sandbox profile 和网络授权校验

**文件：** `internal/platform/sandbox/process.go`、`network.go`、`session.go`、相关测试  
**依赖：** 无  
**步骤：**

1. 收紧 profile 对空/重复/未 pin grant、非法挂载和敏感环境的校验。
2. 将代理、助手、socket 和临时目录错误统一为可识别的不可用原因。
3. 保持持久 session 带 grant 时 fail-closed。
4. 补充取消/超时后的进程组、代理和临时目录清理断言。

**验证：** `go test ./internal/platform/sandbox/... -count=1`；错误场景确认无宿主残留。

## T3：统一 authority 到一次性 profile 的构造

**文件：** `internal/execution/sandbox_profile.go`、新增定向测试  
**依赖：** T2  
**步骤：**

1. 增加一次性命令、bridge 和 session profile 构造函数。
2. 从可信 authority 读取网络 grant，使用 resolver pin 后写入 profile。
3. 复用正式/候选/run root 校验，拒绝客户端扩大路径或 grant。
4. 对无 grant、pin 失败、路径变化和 session grant 分别返回分类错误。

**验证：** `go test ./internal/execution -run 'Test.*Profile|Test.*Grant' -count=1`。

## T4：把一次性 command/helper 接入网络授权

**文件：** `internal/execution/tool_executor.go`、`tool_executor_test.go`、`permission` 相关 fake  
**依赖：** T3  
**步骤：**

1. 让 command 和 helper 使用统一 profile builder。
2. 保持 read-only helper 的 scratch candidate 语义。
3. 验证 grant 只进入一次性运行，未 pin 或代理不可用时拒绝。
4. 保持 timeout、output limit、redaction 和 candidate checkpoint 行为。

**验证：** `go test ./internal/execution -run 'Test.*(Command|Helper|Network|Isolation)' -count=1`。

## T5：把 bridge（ERC/依赖/GUI）接入统一 profile

**文件：** `internal/execution/python_bridge.go`、`sandbox_profile.go`、`python_bridge_test.go`  
**依赖：** T3  
**步骤：**

1. bridge 复用 authority/profile 构造，不接受请求 payload 扩大根目录。
2. 一次性 bridge 传递已 pin grant；computer persistent session 拒绝 grant。
3. 保持 guest path/evidence path 映射和 1 MiB JSON 限制。
4. 为未知结果、超时、取消和 session stale 添加分类断言。

**验证：** `go test ./internal/execution -run 'TestPythonBridge|Test.*Session|Test.*Evidence' -count=1`。

## T6：将 KiCad resolver 接入 Go 侧 ERC checker

**文件：** `internal/candidate/erc_checker.go`、`erc_checker_test.go`  
**依赖：** T1、T3  
**步骤：**

1. 用 resolver 提供版本、私有配置和模板能力，不在 checker 内自行主机直跑。
2. 保留 formal/candidate digest 前后校验、报告根目录和 8 MiB 上限。
3. 将工具不可用、版本无效、报告异常和摘要漂移映射为 unavailable/block。
4. 保持 finding、review、acceptance 的现有状态语义。

**验证：** `go test ./internal/candidate -run 'Test.*ERC|Test.*Checker' -count=1`。

## T7：统一 KiCad Python worker 配置与依赖报告

**文件：** `workers/kicad/erc.py`、`dependencies.py`、`test_erc.py`、`test_dependencies.py`  
**依赖：** T1、T6  
**步骤：**

1. 从 profile 环境读取私有 XDG 目录和模板来源。
2. 禁止 worker 回退到允许根目录外的配置或报告路径。
3. 保持版本不可用、依赖缺失、ERC 报告异常和 digest 变化的原有结果。
4. 增加模板缺失、工具不可见和配置越界用例。

**验证：** `python3 -m unittest workers/kicad/test_erc.py workers/kicad/test_dependencies.py`。

## T8：强化 Linux GUI worker 会话与截图生命周期

**文件：** `workers/computer/bridge.py`、`test_candidate.py`  
**依赖：** T1  
**步骤：**

1. 统一显示、窗口探测、截图工具和 KiCad 配置的能力判断。
2. 保持 session/generation/artifact digest/window identity 绑定。
3. 处理启动失败、截图失败、超时、取消和恢复，返回稳定 error code。
4. 只清理当前 handle 对应的 Xvfb、eeschema 和自有 lock。

**验证：** `python3 -m unittest workers/computer/test_candidate.py`；无显示依赖时验证明确 blocked/unsupported。

## T9：收敛 sandbox session 的进程清理错误

**文件：** `internal/platform/sandbox/session.go`、`process_linux.go`、相关测试  
**依赖：** T2、T8  
**步骤：**

1. 统一 SIGTERM/SIGKILL 升级和 context cancellation 行为。
2. 清理失败时返回包含 PID/session ID 的错误，不吞掉信号失败。
3. 确保 stale generation 不会复用或清理新会话。
4. 验证 run root、proxy/control socket 和临时目录最终清理。

**验证：** `go test ./internal/platform/sandbox -run 'Test.*Session|Test.*Cancel|Test.*Cleanup' -count=1`。

## T10：接入 doctor 的隔离能力诊断

**文件：** `internal/runtime/doctor.go`、新建/修改 `doctor_test.go`  
**依赖：** T1、T2、T6、T8  
**步骤：**

1. 保留基础文件检查，加入 sandbox/KiCad capability discovery。
2. 分开报告主机定位、隔离可见性、显示、截图和模板状态。
3. 覆盖 host PATH 假阳性、缺失工具、缺失模板和 bwrap 不可用。
4. 保持 `Missing` 和 CLI 输出兼容。

**验证：** `go test ./internal/runtime -run 'Test.*Doctor|Test.*Missing' -count=1`。

## T11：运行 Linux 集成与端到端场景

**文件：** `tests/e2e/m03_sandbox_network_test.go`、`m03_computer_session_test.go`、相关脚本  
**依赖：** T4、T5、T6、T7、T8、T9、T10  
**步骤：**

1. 验证默认断网、批准目标可达、未授权目标不可达和解析变化拒绝。
2. 验证 ERC 有效/阻断、formal digest 不变、报告证据受限。
3. 验证 GUI 启动、截图、stale/recover、停止无残留和无关 PID 不受影响。
4. 记录 KiCad/Xvfb/bwrap 前置条件及实际结果。

**验证：** 定向 `go test` 与 `bash tests/e2e/m03_sandbox_network.sh`、`bash tests/e2e/m03_computer_session.sh`；环境缺失时记录明确未执行原因。

## T12：回归、矩阵与文档收口

**文件：** `docs/spec_docs/S00/platform-capability-matrix.md`、S04 四件套  
**依赖：** T11  
**步骤：**

1. 运行受影响 Go/Python 测试和 Linux 构建。
2. 将实际命令、结果和环境限制写入 S00 对应 C08/C09/C10 条目。
3. 写入 `checklist.md`，逐条对应 AC1–AC12，保留未执行项。
4. 检查四份文档无未完成占位内容、链接有效、非 Linux 未被声明为支持。

**验证：** `git diff --check`、文档占位符扫描、受影响包测试汇总和 checklist 逐项证据。

## 执行顺序

```text
批次 1（可并行）：
  T1  KiCad 能力模型
  T2  sandbox profile/网络校验

批次 2：
  T3  authority → profile
  T6  ERC 接入（依赖 T1，和 T3 可在 T3 完成后并行）
  T7  Python worker 配置（依赖 T1，待 T6 后汇合）
  T8  GUI worker（依赖 T1，可与 T6/T7 并行）

批次 3：
  T4  command/helper 接线（依赖 T3）
  T5  bridge 接线（依赖 T3）
  T9  session 清理（依赖 T2、T8）

批次 4：
  T10 doctor（依赖 T1、T2、T6、T8）
  T11 集成/e2e（依赖 T4、T5、T6、T7、T8、T9、T10）

批次 5：
  T12 回归、矩阵和文档收口（依赖 T11）
```

共享文件边界：T2/T9 都修改 `internal/platform/sandbox`，必须串行；T3/T4/T5 共享 profile 契约，先完成 T3 再并行；T6/T7 共享 KiCad 配置契约，先完成 T6 再修改 worker；T10/T11 只在前序任务完成后运行集成验证。

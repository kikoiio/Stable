# S04 Linux 沙箱、网络与 KiCad 工作流 Checklist

> 每项通过运行代码或观察行为验证。真实 KiCad/Xvfb/bubblewrap 依赖若缺失，记录为未执行并说明原因，不把缺失环境当作通过。
>
> 状态：验收完成（2026-10-07）。补齐无关进程/锁保护回归和隔离临时 HOME 下的完整 `stable doctor` 运行；S04 不声明 macOS/Windows 支持。

## 实现完整性

- [x] **AC1 / F1：Linux 隔离边界** — `go test ./internal/platform/sandbox -count=1`、`bash tests/e2e/m03_sandbox_files.sh`、`bash tests/e2e/m03_sandbox_network.sh` 通过，覆盖挂载边界、namespace、Probe 和无宿主回退。
- [x] **AC2 / F2：一次性网络授权** — sandbox/execution 定向测试及 `bash tests/e2e/m03_sandbox_network.sh` 通过，覆盖 pinning、代理、未授权目标、解析变化与持久 session grant 拒绝。
- [x] **AC3 / F3：无头 ERC** — candidate 定向测试、KiCad worker 测试及 `bash tests/e2e/m03_kicad_candidate.sh` 通过，formal digest 和报告边界断言通过。
- [x] **AC4 / F4：GUI 生命周期** — execution、sandbox、computer worker 测试及 `bash tests/e2e/m03_computer_session.sh` 通过，覆盖窗口、截图、generation、停止和 stale。
- [x] **AC5 / F4/N1：误杀防护** — `go test ./internal/runtime -run '^TestStopSessionProcessesIgnoresUnrelatedPID$' -count=1` 证明伪造 runtime_handle 指向无关 PID 时不会发信号；`python3 -m unittest workers/computer/test_candidate.py` 覆盖进程身份不匹配及非自有锁不删除；M03 computer session e2e 已验证 formal digest 不变、真实会话停止和 stale/recover。
- [x] **AC6 / F5：能力诊断** — KiCad capability resolver 与 `internal/runtime/doctor_test.go` 通过，覆盖隔离不可见和主机 PATH 假阳性。
- [x] **AC7 / F6：拒绝与阻断分类** — sandbox、execution、candidate、runtime 定向测试及全量 e2e 通过，覆盖越界、网络、超时/取消、报告和清理错误。
- [x] **AC8 / F7/N4：既有边界回归** — `GOMAXPROCS=1 go test -p 1 ./...` 通过，包含候选、摘要/依赖、敏感环境、文件限制和 session generation。

## 集成

- [x] 一次性 command、helper、ERC bridge 和 computer bridge 都经过统一 profile builder（execution 定向测试通过）。
- [x] authority 中的 grant 只进入一次性 profile，持久 session 明确拒绝（profile/bridge/session 测试通过）。
- [x] ERC、GUI 和 doctor 使用同一能力发现规则（resolver 与 doctor 测试通过）。
- [x] evidence path、报告路径、截图路径均位于允许的 run root（candidate/computer e2e 通过）。

## 编译与测试

- [x] Linux `go build ./...` 通过。
- [x] 受影响 Go 包定向测试通过：sandbox、execution、candidate、runtime。
- [x] Python worker 单元测试通过：`workers/kicad` 与 `workers/computer`。
- [x] 非 Linux 目标 `CGO_ENABLED=0 GOOS=darwin/windows go build ./...` 通过；运行时仍返回 unsupported。
- [x] 文档占位符、链接和 `git diff --check` 检查通过。

## 端到端场景

- [x] **网络场景**：`bash tests/e2e/m03_sandbox_network.sh` 通过，覆盖默认无网络、获批目标、未授权目标和 DNS 解析变化。
- [x] **ERC 场景**：`bash tests/e2e/m03_kicad_candidate.sh` 通过，隔离 `kicad-cli`、私有报告和 formal digest 断言通过。
- [x] **GUI 场景**：`bash tests/e2e/m03_computer_session.sh` 通过，覆盖 Xvfb/eeschema、截图、generation、stop 和无残留。
- [x] **诊断场景**：当前源码构建的完整安装布局在 `/tmp` 组装；使用临时 HOME、XDG 配置/状态目录及空配置运行 `stable doctor`，退出码 0，helper、share 资源、主机工具、隔离可见性、KiCad 模板和 Temporal 端口均逐项报告 OK；未读取用户级配置。

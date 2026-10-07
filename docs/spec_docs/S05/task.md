# S05 Linux 发布与支持声明 Tasks

> 状态：已批准（2026-10-07）。依据已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。

## 文件清单

| 操作 | 文件 | 职责 |
|---|---|---|
| 修改 | `scripts/install_linux.sh` | 校验包、分阶段安装和切换入口 |
| 新建 | `scripts/uninstall_linux.sh` | 删除 Stable 管理的应用版本和入口 |
| 修改 | `scripts/package_linux.sh` | 将卸载入口纳入包并维持归档/校验产物 |
| 新建 | `tests/package/lifecycle.sh` | 安装、升级、回退、失败保护、卸载和数据保留验收 |
| 修改 | `tests/package/cli.sh` | 校验归档含安装与卸载入口 |
| 修改 | `Makefile` | 将生命周期验收纳入 `test-package` |
| 修改 | `.github/workflows/go.yml` | `ubuntu-26.04` 包验收与 workflow artifact 上传 |
| 修改 | `README.md`、`docs/install-linux.md` | 安装、升级、回退、卸载说明与目标范围 |
| 修改 | `docs/spec_docs/S00/platform-capability-matrix.md` | 记录实际 Linux runner 包验收证据及未验平台 |
| 新建 | `docs/spec_docs/S05/checklist.md` | 阶段四生成的行为验收清单 |

## T1：安装包校验与同卷暂存

**文件：** `scripts/install_linux.sh`

**依赖：** 无

**步骤：**
1. 保留 Linux x86_64 检查；检查 `HOME`、包版本字符串和必需的二进制、Temporal 与资源文件。
2. 在任何已安装内容或命令入口改动前，完成上述校验；输入无效时以非零退出并说明缺失对象。
3. 将包内容复制到 `$HOME/.local/opt/stable/.install-*`，确保暂存目录与正式目录位于同一文件系统。
4. 安装失败时清理本次暂存目录；不删除当前入口指向的版本。

**验证：** `bash -n scripts/install_linux.sh` 通过。安装场景由依赖于本任务的 T4/T5 集成验收覆盖。

## T2：切换入口与实现卸载

**文件：** `scripts/install_linux.sh`、新建 `scripts/uninstall_linux.sh`

**依赖：** T1

**步骤：**
1. 将已完整暂存的内容落到 `$HOME/.local/opt/stable/<version>`；只替换同版本目标，不删除其他版本。
2. 使用同目录临时符号链接和 rename 更新 `stable`、`stable-uninstall`；若第二个入口切换失败，恢复第一个入口原目标。
3. 新建卸载脚本：先解析并确认 Stable 管理目录边界，再删除该根目录中的版本目录。
4. 仅移除解析后仍指向该安装根目录的 `stable` 与 `stable-uninstall`；保留配置、凭证、运行状态及其他用户文件。
5. 打包所需的包内 `uninstall.sh` 由 T3 接入 `scripts/uninstall_linux.sh`。

**验证：** `bash -n scripts/install_linux.sh && bash -n scripts/uninstall_linux.sh` 通过。升级、入口回滚和卸载行为由 T4/T5 集成验收覆盖。

## T3：将安装与卸载入口装入发布包

**文件：** `scripts/package_linux.sh`、`tests/package/cli.sh`

**依赖：** T2

**步骤：**
1. 从仓库卸载脚本生成包根目录 `uninstall.sh`，并保留现有 `install.sh`、版本文件及用户级安装布局。
2. 保持 `VERSION`、Temporal CLI 固定版本/摘要、包名和 SHA-256 文件名一致。
3. 在 CLI 包检查中断言归档包含可执行安装入口、卸载入口和所有现有必需资源。

**验证：** 在目标 Linux x86_64 环境运行 `make package`；检查 `sha256sum -c` 成功，`tar -tzf` 列出两个入口及现有资源。

## T4：验收首次安装、重装、升级与回退

**文件：** 新建 `tests/package/lifecycle.sh`

**依赖：** T3

**步骤：**
1. 使用独立临时 HOME 和隔离 PATH，准备当前版本包及一个旧版安装目录/包夹具。
2. 验证首次安装与同版本重复安装后，`stable`、`stable-uninstall` 均指向有效版本目录。
3. 安装新版夹具，验证入口切换且旧版本目录保留。
4. 从旧版包重装以回退，验证 `stable version` 和解析路径指向预期版本。
5. 清理本测试创建的进程、临时 HOME 和临时文件。

**验证：** `bash tests/package/lifecycle.sh` 通过；输出各生命周期场景的独立 PASS 结果。

## T5：验收无效包保护与安全卸载

**文件：** `tests/package/lifecycle.sh`

**依赖：** T4

**步骤：**
1. 向独立临时目录构造缺少必需文件、版本不合法的包输入；安装失败后原 `stable` 入口和可执行版本不变。
2. 在隔离 HOME 放置配置、状态数据库、目标数据哨兵，以及指向其他路径的同名命令入口。
3. 运行 `stable-uninstall`，验证所有 Stable 版本目录和受管入口消失。
4. 验证所有用户数据哨兵和无关入口仍存在且内容不变。

**验证：** `bash tests/package/lifecycle.sh` 通过；异常安装保持旧入口，卸载只删除受管路径。

## T6：接入现有安装包验收目标

**文件：** `Makefile`

**依赖：** T5

**步骤：**
1. 将 `tests/package/lifecycle.sh` 加入 `test-package`，安排在包构建之后、完整 runtime/e2e 场景之前。
2. 保持既有 install、CLI、e2e、restart 验收顺序不变，所有使用固定端口的脚本仍在同一 job 串行运行。

**验证：** `make -n test-package` 显示先构建包，再顺序执行生命周期与既有验收脚本；直接运行生命周期脚本通过。

## T7：更新 Linux 使用与支持说明

**文件：** `README.md`、`docs/install-linux.md`

**依赖：** T2、T3

**步骤：**
1. 描述 Ubuntu 26.04 x86_64 包目标和 KiCad/Python/Xvfb 等运行依赖。
2. 给出归档校验、安装、升级、从旧版包回退、卸载的可复制命令。
3. 说明卸载保留用户配置、凭证、数据库和目标状态。
4. 明确 macOS、Windows、其他发行版和 ARM64 本轮未验、未声明支持。

**验证：** 按文档逐段检查命令与实际脚本入口一致；`rg` 确认无“macOS/Windows 已支持”或其他发行版支持声明；`git diff --check` 通过。

## T8：固定目标 runner 并上传验收产物

**文件：** `.github/workflows/go.yml`

**依赖：** T6

**步骤：**
1. 将 `build-and-test` 和 `test-package` 两个 job 的 runner 固定为 `ubuntu-26.04`，保留依赖安装、Go setup 和单 job 顺序验收。
2. `make test-package` 成功后上传 `dist/*.tar.gz` 与配套 `.sha256` 为 workflow artifact。
3. 保持上传步骤默认仅在前序成功时运行；不添加 GitHub Release、写仓库权限或发布凭据。

**验证：** 在 GitHub Actions 运行目标 workflow；两个 job 均使用 `ubuntu-26.04` 并通过，artifact 含归档与校验文件且 SHA-256 校验成功。

## T9：更新 S00 能力矩阵证据

**文件：** `docs/spec_docs/S00/platform-capability-matrix.md`

**依赖：** T8 的 Ubuntu 26.04 workflow run 成功

**步骤：**
1. 记录 Linux 发布目标和实际 runner 镜像/架构。
2. 记录真实执行的构建、包级验收命令及 artifact/校验结果，不写入未执行的验收。
3. 保留 macOS/Windows 真实系统行为待验状态；明确本轮不完成整体路线图阶段 5。

**验证：** 对照 workflow 实际结果逐条核对矩阵证据；`git diff --check` 通过，且未将目标或待办写成已完成。

## 执行顺序

```text
T1 → T2 → T3
           ├→ T4 → T5 → T6 → T8 → T9
           └→ T7 ────────────────┘
```

- **最大并行批次：** T3 后可并行 T4 与 T7；T4–T6 依次修改同一测试集/Make 目标，不并行；T8 需等待 T6；T9 必须等待真实目标 runner 证据。
- **共享文件边界：** T1/T2 都修改安装器，串行；T4/T5 都修改 `lifecycle.sh`，串行；其余任务按文件清单隔离。
- **资源安排：** shell 生命周期脚本使用临时 HOME，单次运行；完整 `make test-package` 包含 runtime/e2e，作为单一重型 CI job 串行执行，不并发启动本地包验收。

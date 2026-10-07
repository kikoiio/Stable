# S05 Linux 发布与支持声明 Plan

> 状态：已完成（2026-10-07）。基于已批准的 [spec.md](spec.md)。S05 覆盖 Ubuntu 26.04 x86_64 的 Linux 发布切片，并完成 S00–S05 解耦系列收尾。

## 架构概览

1. **包构建层**：继续以 `scripts/package_linux.sh` 为 Linux 包入口，从 `VERSION` 构造应用包，将固定依赖、资源、安装和卸载入口装入版本化目录，生成 tar.gz 与 SHA-256。
2. **安装生命周期层**：扩展用户级安装脚本，校验包内容后复制到版本化 staging 目录，再切换 `$HOME/.local/bin` 中 Stable 命令入口；版本目录并存以支持回退。提供对应卸载入口，只删除 Stable 管理的应用目录和命令入口。
3. **包验收层**：扩展 `tests/package`，在隔离 HOME 下覆盖首装、重装、升级、失败保持旧入口、回退、卸载、数据保留和包完整性；沿用现有安装后 CLI/runtime/e2e/restart 验收。
4. **CI 交付层**：调整现有 GitHub Actions `build-and-test` 与 `test-package` jobs 使用 `ubuntu-26.04`；在同一 `test-package` job 内构建并验收包，仅成功后上传验收过的 tar.gz 和 SHA-256 workflow artifact，不添加公开发布步骤。
5. **文档与状态层**：更新 README、Linux 安装说明和 S00 能力矩阵，记录 Ubuntu 26.04 x86_64 包目标、安装生命周期与实际 runner 证据；macOS/Windows 保持未验证且无支持承诺；相关工作不属于本系列后续任务。

## 核心数据结构与接口

### 包产物

- 应用版本：仓库根目录 `VERSION` 的内容。
- 安装包：`dist/stable-${VERSION}-linux-amd64.tar.gz`。
- 校验文件：`dist/stable-${VERSION}-linux-amd64.tar.gz.sha256`；CI artifact 包含这两个文件。
- 包根目录含 `bin/stable`、内部 `libexec`、Temporal CLI、`share` 资源、`install.sh`、`uninstall.sh` 和版本文件。

### 用户级安装布局

- 版本安装路径：`$HOME/.local/opt/stable/<version>`。
- 安装暂存目录：同一父目录下的 `.install-*`，确保最终目录同卷移动。
- 稳定命令入口：`$HOME/.local/bin/stable`，指向当前版本的 `bin/stable`。
- 卸载入口：`$HOME/.local/bin/stable-uninstall`，指向当前版本的 `uninstall.sh`。

### 脚本调用契约

- `bash install.sh`：从解包后的完整包安装；校验必需文件和版本后，先暂存并安装版本目录，再切换入口。升级不删除旧版本；重新安装旧版包即可回退。
- `stable-uninstall`：删除 Stable 管理的所有版本目录以及解析后仍指向该目录的 `stable`、`stable-uninstall` 链接；不删除配置、凭证和运行状态目录，也不清理指向其他路径的同名文件。
- `make package` 与 `make test-package`：分别构建包及构建后运行包级验收。

该设计不增加 Go CLI 子命令或新的持久化数据结构。

## 模块设计

### Linux 包构建

**文件：** `scripts/package_linux.sh`、`VERSION`

**职责：** 保留 Linux x86_64 守卫和固定 Temporal CLI 版本/摘要；构建 CLI、内部子程序、worker 资源及包内安装/卸载脚本；生成版本化归档与 SHA-256。

**依赖：** Go/CGO 编译工具、Temporal CLI 下载地址、仓库资源文件。

### 用户级安装与卸载

**文件：** `scripts/install_linux.sh`、新增 `scripts/uninstall_linux.sh`、包内 `install.sh`/`uninstall.sh`

**职责：** 安装脚本验证包清单与版本，复制到同卷暂存目录，落成版本目录并更新稳定链接；卸载脚本只删除 `$HOME/.local/opt/stable` 下的版本目录和解析后仍指向该目录的命令链接。

**依赖：** `$HOME`、标准 Linux shell/coreutils；不需要 root 权限，不触碰配置和运行数据目录。

### 安装包验收

**文件：** 新增 `tests/package/lifecycle.sh`，修改 `Makefile`；复用 `tests/package/install.sh`、`cli.sh`、`e2e.sh`、`restart.sh`

**职责：** 使用隔离 HOME 验证包清单、首装/重装/升级/回退/失败保护/卸载和数据保留；原有 runtime 与端到端包验收继续执行。

**依赖：** 同一份 `dist` 包、Temporal 与现有 E2E 系统依赖；固定端口测试按顺序运行。

### GitHub Actions

**文件：** `.github/workflows/go.yml`

**职责：** `build-and-test` 与 `test-package` jobs 均固定使用 `ubuntu-26.04`；`test-package` 完成 `make test-package` 后才上传 `dist/*.tar.gz` 和配套 `.sha256` 为 workflow artifact。

**依赖：** 仓库现有依赖安装 action、Go setup action、`actions/upload-artifact`；不增加 Release 凭据或发布权限。

### 安装文档与能力矩阵

**文件：** `README.md`、`docs/install-linux.md`、`docs/spec_docs/S00/platform-capability-matrix.md`

**职责：** 说明 Ubuntu 26.04 x86_64 的包目标、运行依赖、安装/升级/回退/卸载；补记 S05 的 Linux runner 证据，并说明 macOS/Windows 未验证且不属于本系列支持目标。

**依赖：** S05 实际 CI 和包验收结果；文档证据只记录真正运行过的检查。

## 模块交互

```text
GitHub Actions (ubuntu-26.04)
  ├─ build-and-test：源码构建和 Go 单测
  └─ test-package（单一、顺序 job）
       ├─ make package
       │    └─ package_linux.sh → 版本化 tar.gz + SHA-256
       ├─ make test-package
       │    ├─ 校验包清单/摘要/版本
       │    ├─ 隔离 HOME：首装、重装、升级、回退、失败保护、卸载
       │    └─ 既有 CLI/runtime/e2e/restart 验收
       └─ 上述步骤全通过后 → 上传同一份 tar.gz + SHA-256 artifact
```

安装生命周期数据流：

1. 用户解包归档并运行 `install.sh`。
2. 安装器检查系统目标、版本和必需文件；校验失败时不改现有入口。
3. 安装器复制到版本目录同级的 staging，再放置完整版本目录。
4. 安装器逐个原子更新 `stable` 与 `stable-uninstall` 两个用户级入口；若第二个入口切换失败，恢复第一个入口原来的目标；升级不移除旧版。
5. 用户运行 `stable-uninstall` 时，卸载器验证 Stable 根目录边界，移除受管版本目录及受管入口；配置和运行数据留存。
6. 文档/能力矩阵只在 CI 和包验收有实际结果后写入对应证据；CI 不创建 GitHub Release。

## 文件组织

```text
scripts/
├── package_linux.sh                 — 打包并携带安装/卸载入口
├── install_linux.sh                 — 分阶段安装并切换用户级入口
└── uninstall_linux.sh               — 安全删除 Stable 受管安装

.github/workflows/go.yml             — 两个 Linux job 使用 ubuntu-26.04，包验收成功后上传 artifact
Makefile                             — 将生命周期验收接入 test-package

tests/package/
├── cli.sh                           — 检查包内卸载入口
└── lifecycle.sh                     — 新增安装、升级、回退、失败保护与卸载验收

README.md                            — 简明安装生命周期与支持范围
docs/install-linux.md                — Linux 安装依赖和操作说明
docs/spec_docs/S00/platform-capability-matrix.md — 记录 S05 实际 Linux runner 证据

docs/spec_docs/S05/
├── spec.md                           — 已批准
├── plan.md                           — 本计划
├── task.md                           — 阶段三生成
└── checklist.md                      — 阶段四生成
```

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| CI 目标镜像 | 固定 `ubuntu-26.04`，不使用 `ubuntu-latest` | runner 与规格目标一致，避免 latest 切换造成验收环境漂移 |
| 包格式与依赖来源 | 保留版本化 tar.gz、独立 SHA-256、固定 Temporal CLI 版本和摘要 | 延续现有 Linux 包契约，用户可独立校验，外部依赖可追踪 |
| 安装位置与回退 | 每版本独立目录，稳定命令通过用户级 symlink 指向当前版；新包验证/落盘后才切换，升级不清理旧版 | 保持现有布局并支持明确回退，失败可保留既有入口 |
| 安装器实现 | 继续使用 Bash 脚本，不新增 Go 子命令、root 安装或系统级包管理依赖 | 当前包已有 shell 安装入口，符合单用户 CLI 的交付形式 |
| 卸载语义 | 提供 `stable-uninstall` 用户级命令；仅删除 Stable 安装根目录与解析后指向该根目录的命令链接 | 可从任一受管版本触发卸载，并避免删除同名但指向其他位置的用户文件；用户数据位于不同目录，默认保留 |
| CI 验收与产物 | 在现有 `test-package` job 中顺序运行 `make test-package`，完成后由 `actions/upload-artifact` 上传被测包和校验文件 | 上传的正是已验收包；无需跨 job 传递或公开 Release 凭据 |
| 能力矩阵证据 | 仅在 CI/包验收实际通过后更新 S00 的 S05 记录 | 避免将规格目标写成已验证事实 |

## 自检

- **spec 覆盖：** F1–F6 均有对应模块；N1–N5 通过 runner 限定、用户数据保护、固定版本/摘要、隔离验收和证据边界落实。
- **接口完整性：** 包名、校验文件、安装目录、稳定入口、卸载命令和 Make 目标均已定义。
- **依赖清晰度：** 包构建先于包验收；验收成功后才上传 artifact；能力矩阵和文档证据依赖实际运行结果，没有循环依赖。
- **矛盾检查：** 只声明 Ubuntu 26.04 x86_64 包验收；未将本轮结果扩展到其他 Linux 发行版或 macOS/Windows 支持。

# S05 Linux 发布与支持声明 Spec

> 状态：已批准（2026-10-07）。S05 是平台可移植性路线图阶段 5 的 Linux 发布切片，不代表整个阶段 5 完成。

## 背景

S01–S04 已完成 Linux x86_64 上的核心 CLI/TUI、隔离执行和 KiCad 工作流。当前已有 Linux tar.gz、SHA-256、安装脚本及安装包验收；GitHub Actions 已在 Ubuntu runner 运行测试，但还没有把发布包作为 workflow artifact 交付。本轮按已确认范围，只完成 Linux 发布切片，不声称 Windows/macOS 已通过真实系统验收或获得支持。

## 目标

- 在 Ubuntu 26.04 x86_64 上构建版本化 Linux tar.gz 与 SHA-256 校验文件。
- 由 GitHub Actions 构建并验收安装包，将包与校验文件作为 workflow artifact 保存；不发布到 GitHub Releases。
- 验证安装、升级、旧版回退与卸载：升级保留旧版本；卸载移除应用文件和入口，保留配置及运行数据。
- 更新安装与支持文档，明确 Linux 支持范围、操作方式和未完成的平台验收边界。

## 功能需求

- **F1 Linux 包构建**：在 Ubuntu 26.04 x86_64 构建版本化 tar.gz，包含 CLI、内部子程序、固定版本 Temporal CLI、运行资源及安装/卸载入口，并生成 SHA-256 校验文件；缺少必需组件或校验失败时构建失败。
- **F2 安装与升级**：安装到用户目录下的版本化路径，通过稳定入口选择当前版本。首次安装、同版本重装及升级均可重复执行；新版本先完成暂存，再切换入口。失败时旧入口和旧版本仍可用，升级保留旧版以便回退。
- **F3 卸载**：提供明确的卸载操作，移除 Stable 管理的应用版本目录和命令入口；保留用户配置、凭证、状态数据库及目标数据，不触碰其他文件。
- **F4 Linux 发布验收**：GitHub Actions 在 Ubuntu 26.04 x86_64 上构建包并运行包完整性、安装/升级/回退/卸载、CLI、runtime 生命周期及现有包级端到端验收。失败的任务不上传成功产物。
- **F5 CI 产物**：成功构建后将 tar.gz 与 SHA-256 文件作为 GitHub Actions workflow artifact 上传；本轮不发布 GitHub Releases 或其他公开渠道。
- **F6 文档与支持声明**：安装说明和 README 描述目标系统、依赖、安装/升级/回退/卸载方式；明确本轮只验收 Linux，macOS/Windows 实机验收和支持声明仍待后续完成。

## 非功能需求

- **N1 支持边界**：本轮实现和真实 runner 验收仅覆盖 Ubuntu 26.04 x86_64；Linux 其他发行版/架构及 macOS、Windows 不因交叉编译或 Linux CI 结果而获得支持声明。
- **N2 数据安全**：打包过程不读取或收录开发者本机配置、凭证、状态目录和运行数据；安装/升级只操作 Stable 管理的应用路径；卸载保留配置、凭证、数据库和目标数据。
- **N3 版本与依赖可追踪**：应用版本继续以仓库 `VERSION` 为唯一来源；Temporal CLI 版本及下载校验固定；CI 上传的校验文件对应同一份被安装和验收的 tar.gz。
- **N4 可重复验收**：包级验收使用隔离临时 HOME 和模拟模型服务，不要求真实模型密钥；运行时及固定端口场景按顺序执行，避免相互干扰。
- **N5 诚实证据**：CI 成功只证明本轮 Linux 目标上的构建和包验收。阶段 2–4 遗留的 macOS/Windows 真实系统行为仍列为未验，不以交叉编译替代。

## 不做的事

- 不实现或真实验收 macOS、Windows 发布；不为其他 Linux 发行版或 ARM64 增加支持声明。
- 不把 tar.gz 或其他产物发布到 GitHub Releases、包仓库或公开下载站；只生成并上传 GitHub Actions workflow artifact。
- 不实现 macOS/Windows 沙箱、网络代理、KiCad 工具链或 GUI 后端，也不完成其阶段 2–4 的实机验收。
- 不更换 SQLite 驱动或重写 Linux runtime、沙箱、候选工作流；仅在安装生命周期所需范围内修改打包、安装、卸载、CI 和文档。
- 不收录或迁移现有用户配置、凭证、数据库、目标状态；不增加自动删除用户数据的卸载选项。

## 验收标准

- **AC1（F1 包完整性）**：在 Ubuntu 26.04 x86_64 构建后，版本化 tar.gz 与 SHA-256 文件存在；`sha256sum -c` 成功；包内二进制报告版本与 `VERSION` 一致，Temporal CLI 版本与固定版本一致，必需资源齐全。
- **AC2（F2 首装与重装）**：在隔离 HOME 首次安装及重复安装后，`stable` 入口均指向预期版本；`stable version` 与 `stable help` 可运行；预先放置的配置/状态哨兵内容不变。
- **AC3（F2 升级与回退）**：安装旧版后安装新版，入口切换到新版且旧版目录仍存在；重新安装旧版后入口可回退；注入无效/不完整的新包时，已安装入口仍指向先前可用版本。
- **AC4（F3 卸载）**：卸载后 Stable 管理的应用版本目录及命令入口消失；配置、凭证、数据库、目标状态和无关文件仍存在且内容不变。
- **AC5（F4/F5 CI 与产物）**：GitHub Actions 在 `ubuntu-26.04` runner 上完成 Linux 构建、单元测试和现有安装包验收；成功 workflow artifact 同时包含 tar.gz 与其 SHA-256 文件，校验通过；失败时不上传成功产物，也不创建 GitHub Release。
- **AC6（F6 文档）**：README 和安装说明明确写出 Ubuntu 26.04 x86_64 发布目标、系统依赖、安装/升级/回退/卸载命令，以及用户数据保留行为。
- **AC7（N1/N5 支持边界）**：平台能力矩阵和文档明确本轮 Linux runner 实测范围；macOS、Windows、其他发行版和 ARM64 保持未验/未声明支持，交叉编译不被写作实机验收证据。

## 参考

- GitHub-hosted `ubuntu-26.04` runner：<https://github.com/actions/runner-images/blob/main/images/ubuntu/Ubuntu2604-Readme.md>

# S05 Linux 发布与支持声明 Checklist

> 状态：验收完成（2026-10-07）。每项均按运行包、GitHub Actions 或文档检查记录结果。范围仅为 Ubuntu 26.04 x86_64。

## 实现完整性

- [x] **AC1 / F1：包完整性** — 构建得到版本化 tar.gz 与 SHA-256 文件；归档包含 CLI、内部子程序、Temporal CLI、资源、`install.sh` 和 `uninstall.sh`（验证：Ubuntu 26.04 runner 的包验收日志 `AC1 manifest PASS`；下载 artifact 后 `sha256sum -c` 为 `OK`）。
- [x] **AC2 / F2：首装与重装** — 在隔离 HOME 首次及重复安装后，两个命令入口均解析到有效版本；配置/状态哨兵保持原值（验证：workflow lifecycle 日志 `LIFECYCLE first install and reinstall PASS`，CLI 日志 `AC2 lifecycle PASS`）。
- [x] **AC3 / F2：升级、回退与失败保护** — 新版本切换成功且旧版保留；重装旧包可回退；无效或不完整包不改变旧入口（验证：workflow lifecycle 日志 `LIFECYCLE upgrade and rollback PASS`、`invalid package protection PASS`、`entry switch rollback PASS`）。
- [x] **AC4 / F3：卸载与数据保留** — 卸载删除 Stable 版本目录与受管入口，保留配置、凭证、运行数据库、目标数据和无关文件（验证：workflow lifecycle 日志 `LIFECYCLE uninstall and data preservation PASS`，覆盖 config、credentials、state.db、目标文件及外部 symlink 哨兵）。
- [x] **AC5 / F4/F5：目标 CI 与 artifact** — `build-and-test`、`test-package` 都在 `ubuntu-26.04` 成功；artifact 含被验收的 tar.gz 和配套 SHA-256，下载后校验通过；workflow 不发布 GitHub Release（验证：Go run 37568537248 两 job 成功；artifact 下载校验通过；workflow 无 Release 步骤）。
- [x] **AC6 / F6：使用文档** — README 和 Linux 安装说明写清 Ubuntu 26.04 x86_64 目标、依赖、安装/升级/回退/卸载与数据保留方式（验证：对照 README、Linux 安装文档与脚本命令；Actions 包验收通过）。
- [x] **AC7 / N1/N5：支持边界与证据** — S00 能力矩阵记录本轮真实 Linux runner 结果；macOS、Windows、其他发行版和 ARM64 保持未验/未声明支持（验证：S00 §1、§9 记录 runner 日志与 run 链接，其他平台仍为待验）。

## 集成

- [x] 发布包中的安装/卸载入口来自仓库维护的 Linux 脚本，包清单、版本文件及 SHA-256 相互对应（验证：`PACKAGE CLI PASS`、`AC1 manifest PASS`、artifact `sha256sum -c` 成功）。
- [x] `make test-package` 先构建包，再按顺序运行生命周期、CLI、runtime/e2e 和 restart 验收（验证：本机 `make -n test-package` 与 runner 中 `PACKAGE INSTALL/LIFECYCLE/CLI/E2E/RESTART PASS`）。
- [x] workflow 上传的 tar.gz 与 SHA-256 是同一 job 构建并通过包验收的文件（验证：run 37568537248 的 test-package 成功后上传；artifact 下载后 SHA-256 校验成功）。
- [x] 卸载命令只移除 Stable 管理的路径；同名但指向其他目录的用户入口不被删除（验证：`LIFECYCLE uninstall and data preservation PASS` 的外部 symlink 哨兵）。

## 编译与测试

- [x] Linux Go 构建与单元测试通过（验证：`ubuntu-26.04` 上 `go build ./cmd/...` 和 `go test ./...` 成功）。
- [x] 安装、打包和卸载脚本 shell 语法检查通过（验证：本机对 package/install/uninstall/lifecycle/cli 脚本运行 `bash -n` 成功）。
- [x] 全部发布包验收通过（验证：run 37568537248 的 `make test-package` 成功，包含 install、CLI、e2e、restart 和 lifecycle PASS）。
- [x] 文档和 workflow 改动无空白错误（验证：`git diff --check`；workflow run 解析并成功执行两个 `ubuntu-26.04` jobs）。

## 端到端场景

- [x] **场景 1：安装包完成模拟目标** — 从 CI 生成的包在隔离 HOME 安装 Stable，启动 runtime，通过模拟模型服务创建并确认目标，候选工程通过验收并导出结果，随后停止 runtime；配置数据保留（验证：runner 日志 `PACKAGE E2E PASS`）。
- [x] **场景 2：发布升级后回退** — 安装旧版、升级到新版本、核对新入口，再安装旧版包回退并运行 `stable version`（验证：runner 日志 `LIFECYCLE upgrade and rollback PASS`；同版本入口切换失败注入也通过回滚断言）。
- [x] **场景 3：卸载保留数据** — 运行 `stable-uninstall` 后 Stable 命令不可用，用户配置、数据库和目标状态仍可读取，无关文件仍在（验证：runner 日志 `LIFECYCLE uninstall and data preservation PASS`）。

## 验收证据

- GitHub Actions：<https://github.com/kikoiio/Stable/actions/runs/37568537248>，提交 `31993ea`；`build-and-test` 与 `test-package` 均成功。
- Artifact：`stable-linux-amd64-37568537248-1`；下载后的归档 SHA-256：`f7b2453df8fe2a4b0b10396b162c98b39a658dadd40c1575fdf95fa598b58cf7`，配套校验文件执行 `sha256sum -c` 输出 `OK`。

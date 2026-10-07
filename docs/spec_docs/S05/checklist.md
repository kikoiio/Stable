# S05 Linux 发布与支持声明 Checklist

> 状态：已批准（2026-10-07）。每项通过运行包、GitHub Actions 或观察文档验证。范围仅为 Ubuntu 26.04 x86_64；没有真实结果前不标记通过。

## 实现完整性

- [ ] **AC1 / F1：包完整性** — 构建得到版本化 tar.gz 与 SHA-256 文件；归档包含 CLI、内部子程序、Temporal CLI、资源、`install.sh` 和 `uninstall.sh`（验证：在 Ubuntu 26.04 runner 运行 `make package`，再运行 `sha256sum -c` 并检查 `tar -tzf` 清单与二进制版本）。
- [ ] **AC2 / F2：首装与重装** — 在隔离 HOME 首次及重复安装后，两个命令入口均解析到有效版本；配置/状态哨兵保持原值（验证：`bash tests/package/lifecycle.sh` 的首装和重装场景）。
- [ ] **AC3 / F2：升级、回退与失败保护** — 新版本切换成功且旧版保留；重装旧包可回退；无效或不完整包不改变旧入口（验证：`bash tests/package/lifecycle.sh` 的升级、回退和错误注入场景）。
- [ ] **AC4 / F3：卸载与数据保留** — 卸载删除 Stable 版本目录与受管入口，保留配置、凭证、运行数据库、目标数据和无关文件（验证：`bash tests/package/lifecycle.sh` 卸载场景，对比哨兵内容和目录）。
- [ ] **AC5 / F4/F5：目标 CI 与 artifact** — `build-and-test`、`test-package` 都在 `ubuntu-26.04` 成功；artifact 含被验收的 tar.gz 和配套 SHA-256，下载后校验通过；workflow 不发布 GitHub Release（验证：目标 workflow run、下载 artifact 后 `sha256sum -c`，并核对 workflow 上传条件与发布步骤）。
- [ ] **AC6 / F6：使用文档** — README 和 Linux 安装说明写清 Ubuntu 26.04 x86_64 目标、依赖、安装/升级/回退/卸载与数据保留方式（验证：逐条按文档命令对照真实脚本入口和验收输出）。
- [ ] **AC7 / N1/N5：支持边界与证据** — S00 能力矩阵记录本轮真实 Linux runner 结果；macOS、Windows、其他发行版和 ARM64 保持未验/未声明支持（验证：对照 workflow run 更新记录，并审阅矩阵 §1 和平台状态行）。

## 集成

- [ ] 发布包中的安装/卸载入口来自仓库维护的 Linux 脚本，包清单、版本文件及 SHA-256 相互对应（验证：`tests/package/cli.sh` 与 `sha256sum -c`）。
- [ ] `make test-package` 先构建包，再按顺序运行生命周期、CLI、runtime/e2e 和 restart 验收（验证：`make -n test-package` 与目标 runner 完整日志）。
- [ ] workflow 上传的 tar.gz 与 SHA-256 是同一 job 构建并通过包验收的文件（验证：下载 artifact 后与 job 记录的归档名及校验结果核对）。
- [ ] 卸载命令只移除 Stable 管理的路径；同名但指向其他目录的用户入口不被删除（验证：生命周期卸载场景中的无关 symlink 哨兵）。

## 编译与测试

- [ ] Linux Go 构建与单元测试通过（验证：`ubuntu-26.04` 上的 `go build ./cmd/...` 和 `go test ./...` workflow 步骤）。
- [ ] 安装、打包和卸载脚本 shell 语法检查通过（验证：`bash -n scripts/package_linux.sh && bash -n scripts/install_linux.sh && bash -n scripts/uninstall_linux.sh`）。
- [ ] 全部发布包验收通过（验证：在 `ubuntu-26.04` 运行 `make test-package`，检查 install、CLI、e2e、restart 和 lifecycle 脚本结果）。
- [ ] 文档和 workflow 改动无空白错误（验证：`git diff --check`；workflow 的实际目标 run 通过配置解析）。

## 端到端场景

- [ ] **场景 1：安装包完成模拟目标** — 从 CI 生成的包在隔离 HOME 安装 Stable，启动 runtime，通过模拟模型服务创建并确认目标，候选工程通过验收并导出结果，随后停止 runtime；配置数据保留（验证：`tests/package/e2e.sh` 在同一包上的完整输出与导出文件断言）。
- [ ] **场景 2：发布升级后回退** — 安装旧版、升级到新版本、核对新入口，再安装旧版包回退并运行 `stable version`（验证：`tests/package/lifecycle.sh` 检查入口解析、版本输出及旧版目录）。
- [ ] **场景 3：卸载保留数据** — 运行 `stable-uninstall` 后 Stable 命令不可用，用户配置、数据库和目标状态仍可读取，无关文件仍在（验证：卸载前后对照隔离 HOME 哨兵）。

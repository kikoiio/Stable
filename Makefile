# Stable — 统一入口。版本号唯一来源是 ./VERSION。
.PHONY: check test e2e m03-e2e m04-e2e m05-e2e m07b-e2e cases package test-package install-dev run clean platform-check

check: ## 检查开发环境依赖
	bash scripts/check_env.sh

test: ## Go 单元测试
	go test ./...

platform-check: ## 平台边界门禁（业务包无 syscall/unix/GOOS；双 GOOS 编译）
	bash scripts/check-platform.sh

e2e: ## 源码级端到端测试（含 V01 标准变化与 V02 工程依赖变化）
	bash tests/e2e/run.sh
	bash tests/e2e/waiting_restart.sh
	bash tests/e2e/unsupported.sh
	bash tests/e2e/criteria_change.sh
	bash tests/e2e/dependency_change.sh

m03-e2e: ## M03 权限、隔离、候选接收与恢复验证（低并发）
	bash tests/e2e/m03_suite.sh

m04-e2e: ## M04 工具执行、隔离拒绝与可选 Linux 正例（串行）
	bash tests/e2e/m04_tools.sh

m05-e2e: ## M05 会话搜索恢复、压缩边界、快照 rewind 与问答边界（无需沙箱）
	bash tests/e2e/m05_sessions.sh

m07b-e2e: ## M07-B hooks 合并、拒绝、通知回流与重启投影（无需沙箱）
	bash tests/e2e/m07b_hooks.sh

cases: ## 场景用例（tests/cases，需要 kicad-cli）
	@for c in tests/cases/cases/*/; do \
		bash tests/cases/run_case.sh "$$(basename "$$c")" || exit 1; \
	done

package: ## 构建 dist/ 发布包
	bash scripts/package_linux.sh

test-package: package ## 安装包验收（install / lifecycle / cli / e2e / restart）
	bash tests/package/install.sh
	bash tests/package/lifecycle.sh
	bash tests/package/cli.sh
	bash tests/package/e2e.sh
	bash tests/package/restart.sh

install-dev: ## 重新打包并覆盖安装到 ~/.local（会先 stable down）
	bash scripts/dev_update.sh

run: ## 构建开发版并启动 stable 对话（退出对话后运行时继续工作）
	bash scripts/run_local.sh

clean: ## 删除运行数据和打包产物（run/ 与 dist/）
	rm -rf run dist

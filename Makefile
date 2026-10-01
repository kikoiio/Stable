# Stable — 统一入口。版本号唯一来源是 ./VERSION。
.PHONY: check test e2e cases package test-package install-dev run clean

check: ## 检查开发环境依赖
	bash scripts/check_env.sh

test: ## Go 单元测试
	go test ./...

e2e: ## 源码级端到端测试（run / waiting_restart / unsupported / criteria_change）
	bash tests/e2e/run.sh
	bash tests/e2e/waiting_restart.sh
	bash tests/e2e/unsupported.sh
	bash tests/e2e/criteria_change.sh

cases: ## 场景用例（tests/cases，需要 kicad-cli）
	@for c in tests/cases/cases/*/; do \
		bash tests/cases/run_case.sh "$$(basename "$$c")" || exit 1; \
	done

package: ## 构建 dist/ 发布包
	bash scripts/package_linux.sh

test-package: package ## 安装后 CLI 验收（install / cli / e2e / restart）
	bash tests/package/install.sh
	bash tests/package/cli.sh
	bash tests/package/e2e.sh
	bash tests/package/restart.sh

install-dev: ## 重新打包并覆盖安装到 ~/.local（会先 stable down）
	bash scripts/dev_update.sh

run: ## 构建开发版并启动 stable 对话（退出对话后运行时继续工作）
	bash scripts/run_local.sh

clean: ## 删除运行数据和打包产物（run/ 与 dist/）
	rm -rf run dist

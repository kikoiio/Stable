# S01 平台边界抽取(阶段 1)Spec

> 状态:已批准(2026-10-06)。S01 = 平台可移植性路线图「阶段 1:抽取边界,保留 Linux 行为」(依据 docs/platform-portability-roadmap.md 与 docs/spec_docs/S00/platform-capability-matrix.md)。用户已确认:阶段 1 四项工作全量纳入本里程碑;SQLite 采用平台 stub 方案(linux 保留 mattn/go-sqlite3,非 Linux 编译通过但打开 store 返回明确 unsupported,驱动选型留阶段 5);编译门槛为 GOOS=windows/darwin 下 `go build ./...` 全绿(非测试代码),测试文件编译问题清单化不阻塞;架构形态为 internal/platform 六域子包。

## 背景

S00 能力表已建立回归基线:Linux 10 项能力全部「支持」,26 条可运行验证可随时重放;审计定位了全部平台耦合点——6 处 `sandbox.LinuxManager{}` 直接构造(含业务包 internal/dependency/service.go)、无 build tag 的 `internal/sandbox/session.go`、`internal/appconfig/owner_unix.go`、candidate 包的 openat2/renameat2 直用、internal/runtime/supervisor.go 的 `syscall.Flock`、双轨布局推导(runtime.Resolve 与 cmd/stable/chatserve.go 的 chatserveHelperPath)、依赖 CGO 的 SQLite 驱动。全仓编译探针实测(2026-10-06):GOOS=windows/darwin 下 `go build ./...` 失败,且报错被依赖链遮蔽(candidate→sandbox 传导),真实清单比表面广——这正是阶段 1 要建立边界后才能完整枚举并逐项关闭的。

## 目标

- **平台接口包**:新建 `internal/platform/{paths,ipc,lock,proc,secfile,sandbox}` 六域,每域 = 能力接口 + Linux 实现(现有逻辑原样迁入)+ 非 Linux unsupported stub。
- **组合根集中**:全部平台依赖由 cmd/*(stable、agentworker、chatserve dev 入口)构造注入,业务包零平台依赖(grep 门禁可执行)。
- **编译清单关闭**:GOOS=windows 与 darwin 下 `go build ./...` 全绿(非测试代码);问题清单逐项记录处置方式。
- **resolver 统一**:布局推导单轨化,`.exe` 后缀、libexec 子程序、share 资源、dev-install 候选集中处理。
- **SQLite 平台 stub**:linux 沿用 mattn/go-sqlite3;非 Linux 编译通过、打开 store 返回明确 unsupported(驱动选型留阶段 5)。
- **Linux 行为保持**:S00 的 26 条可运行验证重放全过、`make test` 全过——搬移而非重写。

## 功能需求

- F1 平台接口包:新建 `internal/platform/{paths,ipc,lock,proc,secfile,sandbox}` 六个子包,每域提供能力接口(能力定义对应 S00 C01–C08 行)、Linux 实现(现有逻辑原样迁入,行为与权限不变,linux build tag)与非 Linux unsupported stub(other build tag,返回明确 unsupported 错误)。接口设计使业务包无需 import `syscall`、`golang.org/x/sys/unix` 或判断 `runtime.GOOS`。
- F2 组合根集中:全部平台依赖迁移到 cmd/stable、cmd/agentworker(及 chatserve dev 入口)构造并注入;业务包不再构造具体平台实现——含 6 处 `LinuxManager{}` 构造点(cmd/stable/main.go、chatserve.go×2、internal/runtime/supervisor.go×2、internal/dependency/service.go、cmd/agentworker/main.go)。
- F3 编译清单关闭:GOOS=windows 与 GOOS=darwin 下 `go build ./...` 全绿(非测试代码);过程中产出逐项编译问题清单(来源、处置方式:迁移到 platform/stub/记录不阻塞),清单落 S01 目录。
- F4 SQLite 平台 stub:internal/store 驱动文件拆分——linux 沿用 mattn/go-sqlite3(行为不变);非 linux 编译通过,打开数据库返回明确 unsupported 错误。
- F5 resolver 统一:runtime.Resolve 与 chatserveHelperPath 双轨收敛为单一 resolver,集中处理 `.exe` 后缀、libexec 子程序、share 资源、dev-install 布局候选;全部调用方改走 resolver。
- F6 平台专属文件 build tags:session.go、owner_unix.go 等所有含平台 API 的文件持有明确 build tags 或迁入 platform;检查可执行(grep 验证)。
- F7 Linux 行为保持:迁移后 Linux 行为、权限、错误语义与 S00 能力表记载一致;S00 已执行的 26 条可运行验证全部重放通过,`make test` 全过。

## 非功能需求

- N1 回归安全:每个任务迁移后 `make test` 与定向测试保持绿;S00 验证命令作为回归门槛随时重放。
- N2 grep 门禁可执行:门禁规则写成可执行检查(脚本或 make 目标)——业务包不得出现 `runtime.GOOS`、`syscall.`、`golang.org/x/sys/unix` 直用;白名单 = internal/platform 与持有 build tags 的平台实现文件。
- N3 诚实性:stub 错误信息明确说明平台与能力名,不伪装、不 panic;编译清单如实记录(含测试文件编译问题清单,标注「不阻塞」)。
- N4 能力表同步:S01 完成后 S00 能力表按实际变化追加文件位置更新(如证据路径变更),不重写历史结论;Linux 各行状态不得因迁移降级。
- N5 版本跟踪:S01 五份文档落 `docs/spec_docs/S01/` 并加 .gitignore 白名单(同 S00 方式,不影响其他忽略状态)。
- N6 提交纪律:按任务分组提交;每个提交点 Linux 测试全绿。

## 不做的事

- 不改变 Linux 上任何可见行为、权限、IPC 语义、候选事务语义(S00 记载为准)——本阶段是搬移而非重写;Linux 实现内部逻辑只搬不改(编译必需的最小调整除外,须在清单中记录)。
- 不做 macOS/Windows 的真实实现与适配设计(路径策略、ACL、Job Object、命名管道等属阶段 2+;S00 状态维持「未评估」)。
- 不统一 TUI commands 目录的 XDG 来源、不替代 XDG-only 默认值(阶段 2 第 1 项)。
- 不改 doctor 的探测清单与口径(doctor 平台化探测属阶段 2 第 5 项)。
- 不构建跨平台 CI、发布包、安装脚本,不切换 SQLite 驱动选型(阶段 5)。
- 不改造 tests/e2e 的 bash 套件与测试文件的平台化(测试编译问题清单化、不阻塞)。
- 不修订 S00 能力表已有结论(仅按 N4 追加位置更新)。

## 验收标准

- AC1(对应 F1):六个 platform 子包存在且各含 接口 + linux 实现 + other stub;grep 门禁检查跑通且零违例(N2 检查工具落地)。
- AC2(对应 F2):业务包中 `LinuxManager{}` 等具体平台构造 grep 计数为 0;组合根构造点集中在 cmd/*,全部经注入。
- AC3(对应 F3):`GOOS=windows go build ./...` 与 `GOOS=darwin go build ./...` 退出码均为 0;编译问题清单文档存在且每项有处置记录。
- AC4(对应 F4):Linux 下 store 测试全过;windows/darwin 编译通过;非 Linux 打开 store 返回明确 unsupported(探针+代码审阅)。
- AC5(对应 F5):布局推导单轨——chatserveHelperPath 删除或成薄壳,grep 无第二套推导;`.exe`/dev-install/libexec/share 处理集中在 resolver。
- AC6(对应 F6):grep 检查——含平台 API 的 .go 文件均持有 build tag 或位于 platform,无 tag 平台文件清单为空。
- AC7(对应 F7):`make test` 全过;S00 的 26 条可运行验证重放全过;S00 能力表完成位置更新(N4)。
- AC8(对应 N5):`git ls-files docs/spec_docs/S01/` 可见五份文档;spec_docs 其他文件忽略状态不变。
- AC9(端到端):Linux 上构建→启动 runtime→TUI 会话→候选验收关键路径重放,行为与迁移前一致(借助现有 e2e/定向测试)。

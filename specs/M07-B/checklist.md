# M07-B Hooks Checklist

> 状态:验收完成(2026-10-06)。基于已批准的 [spec.md](spec.md)、[plan.md](plan.md) 和 [task.md](task.md)。每项通过运行代码或观察行为验证,聚焦系统行为。

## 配置发现与合并

- [x] **C01 / AC1**:两个配置文件的 hook 均被发现与解析。(验证:`go test ./internal/hooks/` — loader 用例覆盖两级文件、字段解析、自动 id;e2e 场景 1 `/hooks` 列表含两级来源条目。)
- [x] **C02 / AC1**:合并规则生效——用户+项目条目都生效、显式 id 冲突项目覆盖、同文件重复跳过。(验证:loader 合并测试断言顺序与覆盖;e2e 场景 1 跨文件同 id 后仅项目条目生效。)
- [x] **C03 / AC1**:非法 hook 跳过且来源/原因在加载报告可见;`/hooks` 列表与实际加载一致。(验证:loader Rejections 测试;e2e `/hooks` 输出含 rejections 逐条原因;TUI 测试断言列表渲染。)

## 事件触发

- [x] **C04 / AC2**:run_start/run_end 在会话 run 与目标 run 中各触发一次,终态各分支(完成/失败/取消/预算耗尽)均触发 run_end。(验证:conversation 测试断言四分支 RunEnd 调用与 run_id 落盘;e2e 会话 run 两个 hook_fired 块。)
- [x] **C05 / AC2**:pre_tool_use/post_tool_use 在每次工具调用前后触发(含主机工具);同事件多 hook 按合并列表顺序执行。(验证:execution fake HookRunner 断言调用序列与参数;conversation fire 顺序测试。)
- [x] **C06 / AC2**:async hook 不阻塞主流程且输出仍回流。(验证:conversation 异步测试(带延迟的 command hook,主流程先行完成,输出最终入队);e2e 场景 3。)

## 动作执行

- [x] **C07 / AC3**:command hook 进程收到 `STABLE_EVENT`/`STABLE_TOOL`/`STABLE_FILE_PATH` 并产出输出。(验证:hooks 包测试断言环境变量与输出捕获;e2e run_start command hook 写标记文件被读取。)
- [x] **C08 / AC3**:超时有明确报告且与一般失败可区分(TimedOut);prompt hook 产出提醒文本。(验证:hooks 包超时用例(短 timeout + sleep 命令);prompt 动作用例。)
- [x] **C09 / AC3**:http/agent 调用返回指明归属的「未启用」错误(http 留 M07-C、agent 留 M09)。(验证:hooks 包 Fire 用例断言错误文本;e2e 场景 3 触发后 transcript 可见。)

## 拦截与条件

- [x] **C10 / AC4**:`reject` hook 使工具调用被拦截——模型收到含 hook id 的拒绝结果,权限门未被调用,拒绝事实落会话事件。(验证:execution 测试断言 Gate.Authorize 未被调用且结果为「Blocked by hook <id>: …」;conversation 落盘断言 Rejected=true;e2e 场景 2。)
- [x] **C11 / AC4**:`on_error` 三态语义正确——fail 上报、ignore 继续、reject(仅 pre_tool_use)拦截。(验证:hooks 包 Fire 单元用例三态;execution 失败动作 + on_error=reject 拦截用例。)
- [x] **C12 / AC5**:条件表达式四种比较与 `&&`/`||`/`!` 组合正确(以 tool/file_path/args 变量验证)。(验证:hooks 包条件求值表驱动用例覆盖全部操作符与变量源。)
- [x] **C13 / AC5**:once hook 同会话第二次不触发、新会话重新可触发;条件不通过不消耗 once。(验证:conversation onceFired 按会话隔离测试。)

## 输出回流

- [x] **C14 / AC6**:hook 输出在下一次 run 中以系统提醒对模型可见,注入后清空不重复。(验证:conversation 测试断言注入位置(会话 run 在技能段后、目标 run 前置)与 Drain 幂等;e2e 场景 2 post_tool_use 输出现在下一轮 run 的实收消息。)
- [x] **C15 / AC6**:注入文本不落会话日志而 hook 事件落盘;通知超限截断/丢弃可观察。(验证:conversation 测试——注入后 sessionlog 无对应 message 事件、hook_fired 存在;队列 20 条上限丢弃最旧并附标注。)

## 热更新与审计

- [x] **C16 / AC7**:修改 hooks 配置文件后无需重启,下一次事件触发即生效;新增/删除 hook 可感知;`/hooks reload` 报告数量变化。(验证:conversation mtime 重载测试;e2e 场景 4 改文件后新 hook 生效、reload 报告 Before/After。)
- [x] **C17 / AC8**:hook 触发/拒绝/reload 落会话事件,重启后 transcript 投影一致。(验证:e2e 场景 5 重启前后 hook_fired/hook_reload 块重现且内容一致;TUI 渲染测试断言两块;sessionlog 投影 round-trip。)
- [x] **C18**:hook 事件不进入模型上下文(hook_fired/hook_reload 不被 `prompt.MessagesFromItems` 消费)。(验证:sessionlog/投影测试——投影含 Item 但消息构建不含。)

## 集成与安全

- [x] **C19**:hook 拒绝只能收紧不能放宽——未拒绝的调用照常经权限门,权限门语义零改动。(验证:execution 回归——无 hook 或 hook 放行时 Gate 行为与 M04 基线一致;`go test ./internal/execution/...` 全绿。)
- [x] **C20**:hook 输出经脱敏管线(凭据不因 hook 通道泄漏);模型无法创建或修改 hook(无任何此类工具/op)。(验证:conversation 脱敏测试(fake credential 出现在 hook 输出中被替换);工具 schema 与 op 白名单核对无 hook 写入面。)
- [x] **C21**:hooks 功能关闭(HookGate nil)时服务/TUI 行为与 M07-A 基线一致。(验证:全量 `go test ./...` — 既有 harness 未注入 HookGate 回归全绿;PreToolUse/RunStart nil 安全。)
- [x] **C22 / AC9**:超限(文件大小/hook 数量/输出长度/通知条数)与异常(非法配置、不可读、符号链接)被跳过或截断且有报告;不崩溃不阻塞。(验证:hooks 包界限用例(256KB、100 条、8KB 截断)+ conversation 队列上限;e2e 异常文件场景。)

## 编译与测试

- [x] **C23 / 编译**:`gofmt -l internal cmd` 无输出、`go vet ./...` 干净、`go test ./...` 全部通过。(验证:验收终验三条命令。)

## 端到端场景

- [x] **C24 / 全链路**:`tests/e2e/m07b_hooks.sh` 全部场景通过——两级配置合并、事件触发、工具拦截、输出回流、once/async、未启用报错、热更新、重启投影。(验证:验收时亲自复跑 PASS。)
- [x] **C25 / 回归**:`m07a_skills.sh`、`m05_sessions.sh`、`m06_interaction.sh` 通过。(验证:验收时亲自复跑。)

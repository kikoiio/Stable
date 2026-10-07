# S00 平台支持范围与能力契约 Checklist

> 状态:已验收(2026-10-06)，并纳入 S00–S05 系列收尾(2026-10-07)。每一项通过运行代码或观察行为验证,聚焦交付物的内容与可复验性,先有证据再下结论。验证环境:Ubuntu linux/amd64,go1.26.0。

## 实现完整性
- [x] `platform-capability-matrix.md` 存在且六区块齐全:①支持范围声明 ②矩阵总览 ③逐行能力详述 ④发布门槛清单 ⑤完成标志对照 ⑥审计差异记录(验证:打开文档逐区块核对——六区块全部有内容;证据:grep 结构计数)
- [x] C01–C10 每小节字段齐全:状态、≥1 文件级代码引用、证据类型、安全不变量(≥1)、能力不可用行为、按状态等级的验收条件、等价机制线索(验证:逐小节对照 plan.md 能力行模板——grep 计数:C 小节 10、安全不变量块 10、能力不可用行为块 10、验收条件块 10、等价机制线索 ≥10)

## 需求对照(spec AC1–AC9 逐条)
- [x] AC1 支持范围声明明确——仅 Linux(x86_64 CLI/TUI)受支持,macOS/Windows 未评估且不作支持承诺;与矩阵状态一致(验证:读 §1 声明并对照 §2 总览——一致)
- [x] AC2 矩阵 10 行 × 3 平台状态齐全且唯一,取值仅支持/降级/不支持/未评估,Linux 列无「未评估」(验证:grep 总览表数据行=10;Linux 列未评估计数=0)
- [x] AC3 Linux 每行有文件级引用 + 证据类型标注(验证:逐行核对;引用文件均经审计子代理实地核实存在)
- [x] AC4 每行 ≥1 安全不变量 + 1 能力不可用行为,行为符合 fail closed 与向用户说明(验证:逐行核对措辞与语义——10/10)
- [x] AC5 每行有按状态等级的编号验收条件,编号无重复、无悬空引用(验证:全文扫描——AC 定义唯一性检查通过;重复出现均为门槛清单/对照表交叉引用;〔降级〕子集标注齐备)
- [x] AC6 macOS/Windows 每行「未评估(无支持承诺)」且线索标注「未经验证,不构成承诺」(验证:逐格核对——总览 20 格全部未评估;10 行等价机制线索均带标注)
- [x] AC7 发布门槛清单存在且含 KiCad 规则:无头 ERC 检查必须可用、GUI 会话允许降级(验证:读 §4 G7)
- [x] AC8 完成标志对照逐条给出达成位置与证据(验证:读 §5——两条完成标志均有位置与证据,含 26 条已执行验证计数)
- [x] AC9 `git ls-files docs/spec_docs/S00/` 列出五份文档;`git check-ignore` 确认 spec_docs 两份旧文档仍被忽略(验证:命令输出 5 个文件;check-ignore 命中 2 个旧文档)

## 一致性与诚实性(集成层)
- [x] 五处(声明/总览/小节/不变量/验收条件)对同一能力描述无矛盾(验证:抽 C02、C07、C09 三行交叉核对——声明、状态、不变量、条件、缺口表述一致)
- [x] 差异记录每条含出处;路线图未被修改(验证:读 §6——14 条均含文件/行为出处;`git diff docs/platform-portability-roadmap.md` 为空,工作区干净)
- [x] 证据类型为「可运行验证」的条目均附命令或测试名且本机跑通;「仅代码审阅」条目说明了原因(验证:逐条核对;26 条可运行验证对应命令均已执行,仅代码审阅项均注明原因如「需运行时环境/依赖完整沙箱 GUI 环境/无现成 fixture」)

## 端到端场景
- [x] 场景 1(消费者视角):仅读 `platform-capability-matrix.md` 回答——①Windows 现在支持吗 → 未评估(无支持承诺)(§1/§2);②某平台声明「支持」须通过哪些条目 → 该行验收条件全部条目 + §4 G1–G9 门槛(§1 判定依据);③KiCad 规则 → 无头 ERC 必须可用、GUI 允许降级(§1、§4 G7)。答案与已批决策一致。
- [x] 场景 2(可复验性):任取 3 条「可运行验证」重跑,结论与文档一致(验证:`go test ./internal/appconfig/ -run TestLoadAndValidate -count=1` → ok;`go test ./internal/store/ -run TestReconcileAcceptanceConflictBlocks -count=1` → ok;`go test ./internal/sandbox/ -run TestBubblewrapArgs -count=1 -v` → 3 用例 PASS)
- [x] 场景 3(阶段衔接):从能力表摘出 AC-PATH-1…5、AC-IPC-1…6、AC-LOCK-1…4 可直接构成阶段 1 的门槛清单条目,每条自含「运行 X → 期望 Y」,无需二次解释。

## 补充取证(开发期实测,超出 checklist 条目)
- 跨平台编译探针:`GOOS=darwin go build ./internal/sandbox/` 失败(session.go undefined LinuxManager/Pdeathsig);`GOOS=windows go build ./internal/sandbox/` 失败(rc=1);`GOOS=windows go build ./internal/appconfig/` 失败(rc=1,owner_unix.go undefined syscall.Stat_t);`GOOS=windows go build ./internal/candidate/` 失败(依赖链 sandbox/session.go)——证实非 Linux fail-closed 主形态为编译期不可构建(已写入 C08 与差异记录 D2/D10)。
- Python 桥契约:`python3 -m unittest discover -s workers/kicad` 与 `-s workers/computer` 均 OK。

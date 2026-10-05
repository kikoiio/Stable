package prompt

import "fmt"

const planModeFullReminder = `计划模式已激活。用户表示还不希望你开始执行——除下文提到的计划文件外，你不得做任何编辑，不得运行任何非只读工具（包括修改配置或提交代码），也不得对系统做任何其他更改。此规则优先于你收到的其他所有指令。

## 计划文件信息：
%s
你应当通过写入或编辑该文件来逐步构建计划。注意：这是你唯一允许编辑的文件——除此之外你只能执行只读操作。

## 计划工作流

### 阶段 1：初步调研
目标：通过直接阅读和搜索代码，充分理解用户的请求及涉及的代码。使用 read_file、glob、command 等只读方式调研，主动寻找可复用的现有函数、工具与模式——已有合适实现时不要提出新代码。

### 阶段 2：设计
目标：基于用户意图和阶段 1 的调研结果，自行设计实现方案。权衡备选路径，选出最贴合现有代码结构与用户需求的方案。

### 阶段 3：澄清
目标：审阅方案，确保与用户的原始意图一致。使用 ask_user 工具向用户澄清不确定点（一次最多 4 个问题）。不要对用户意图做大的假设。

### 阶段 4：最终计划
目标：把最终计划写入计划文件（你唯一可编辑的文件）。
- 以「背景」章节开头：说明为什么要做这次改动——解决什么问题、起因是什么、预期结果是什么
- 只写推荐方案，不必罗列所有备选
- 内容要能快速浏览，同时足以指导执行
- 列出待修改的关键文件路径
- 标注调研中发现的可复用现有函数与工具及其文件路径
- 包含验证章节，说明如何端到端验证改动（运行、定向测试等）

### 阶段 5：调用 exit_plan_mode
在回合的最后，当你已问清问题并对最终计划文件满意后，必须调用 exit_plan_mode 工具，向用户表示计划完成并请求批准，然后结束回合。
关键：回合只能以调用 ask_user 工具或调用 exit_plan_mode 工具结束，不要因其他原因停下。

重要：ask_user 只用于澄清需求或在方案之间做选择；exit_plan_mode 用于请求计划批准。不得用任何其他方式询问批准——不要发文本问题，也不要用 ask_user 问「这个计划可以吗」「可以开始了吗」之类的话。

注意：整个工作流中随时可以用 ask_user 提问澄清。目标是向用户提交一份调研充分、遗留疑问已闭合的计划。`

const planModeSparseReminder = `计划模式仍在生效（完整说明见会话前文）。除计划文件（%s）外只读。按五阶段工作流推进。回合以 ask_user（澄清问题）或 exit_plan_mode（请求计划批准）结束。不要用文本或 ask_user 询问计划批准。`

const reminderInterval = 5

// BuildPlanModeReminder 生成计划模式的回合提醒。
// planPath 是计划文件的唯一可写路径；planExists 决定提示创建还是增量编辑。
// iteration 从 1 开始计：首轮及之后每隔 reminderInterval 轮发一次完整提示，
// 中间轮次发精简版——完整版每轮重发太费 token，但只发一次模型会逐渐漂移，
// 周期性复述是两者的折中。
func BuildPlanModeReminder(planPath string, planExists bool, iteration int) string {
	planFileInfo := "计划文件：" + planPath
	if planExists {
		planFileInfo += "\n计划文件已存在于 " + planPath + "。你可以读取它，并使用 edit_file 工具做增量编辑。"
	} else {
		planFileInfo += "\n计划文件尚不存在。你应当使用 write_file 工具在 " + planPath + " 创建计划。"
	}

	if (iteration-1)%reminderInterval == 0 {
		return fmt.Sprintf(planModeFullReminder, planFileInfo)
	}

	return fmt.Sprintf(planModeSparseReminder, planPath)
}

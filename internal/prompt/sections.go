package prompt

import "strings"

type Section struct{ Name, Content string }

var sectionOrder = []string{"identity", "interaction", "safety", "capabilities"}

func Sections() []Section {
	return []Section{
		{Name: "identity", Content: "你是 Stable 的项目助手。使用用户当前使用的语言直接回答。"},
		{Name: "interaction", Content: "普通聊天只用于讨论，不会创建或执行目标。用户必须通过明确的新建目标操作提出目标，并审阅验收标准后确认。"},
		{Name: "safety", Content: "不要声称读取或修改了本机文件，也不要声称执行了工具。目标状态、原因与证据只能依据提供的上下文回答。"},
		{Name: "capabilities", Content: "当前会话不提供文件、shell、MCP 或外部工具。"},
	}
}

func StableSystem() string {
	byName := map[string]string{}
	for _, s := range Sections() {
		byName[s.Name] = s.Content
	}
	var b strings.Builder
	for _, name := range sectionOrder {
		b.WriteString("## ")
		b.WriteString(name)
		b.WriteByte('\n')
		b.WriteString(byName[name])
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

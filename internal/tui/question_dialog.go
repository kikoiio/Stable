package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
)

// questionOption is one selectable option of the question dialog, parsed out
// of the transcript form the conversation service renders for ask_user
// questions ("[header] question", "  - label：description", "(可多选)").
type questionOption struct {
	Label       string
	Description string
}

// parseQuestionSpec extracts the dialog structure from a question prompt.
// Prompts that do not match the rendered form return no options; the dialog
// then shows the prompt verbatim and only the free-input path stays usable.
func parseQuestionSpec(prompt string) (header, text string, options []questionOption, multi bool) {
	for _, line := range strings.Split(prompt, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "[") && strings.Contains(trimmed, "]"):
			if header == "" {
				rest := strings.TrimPrefix(trimmed, "[")
				end := strings.Index(rest, "]")
				header, text = rest[:end], strings.TrimSpace(rest[end+1:])
			}
		case strings.HasPrefix(trimmed, "- "):
			option := questionOption{Label: strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))}
			if label, desc, found := strings.Cut(option.Label, "："); found {
				option.Label, option.Description = strings.TrimSpace(label), strings.TrimSpace(desc)
			}
			if option.Label != "" {
				options = append(options, option)
			}
		case strings.HasPrefix(trimmed, "(") && strings.Contains(trimmed, "多选"):
			multi = true
		}
	}
	return header, text, options, multi
}

// renderQuestionDialog shows one pending question with numbered options, the
// multi-select annotation, and the free-input escape hatch. The verbatim
// prompt stays in the transcript; the dialog re-renders the parsed structure.
func renderQuestionDialog(q sessionlog.PendingQuestion, cursor int, picked map[int]bool, other bool, otherText string, width int) string {
	header, text, options, multi := parseQuestionSpec(q.Prompt)
	var b strings.Builder
	b.WriteString("待答问题\n")
	if header != "" {
		fmt.Fprintf(&b, "[%s] %s\n", header, text)
	} else {
		b.WriteString(sanitizeReviewText(q.Prompt) + "\n")
	}
	if len(options) > 0 {
		b.WriteString("\n")
		for i, option := range options {
			if multi {
				mark := "[ ]"
				if picked[i] {
					mark = "[x]"
				}
				if option.Description != "" {
					fmt.Fprintf(&b, "%s %d. %s —— %s\n", mark, i+1, option.Label, option.Description)
				} else {
					fmt.Fprintf(&b, "%s %d. %s\n", mark, i+1, option.Label)
				}
				continue
			}
			mark := "  "
			if i == clamp(cursor, 0, len(options)-1) {
				mark = "> "
			}
			if option.Description != "" {
				fmt.Fprintf(&b, "%s%d. %s —— %s\n", mark, i+1, option.Label, option.Description)
			} else {
				fmt.Fprintf(&b, "%s%d. %s\n", mark, i+1, option.Label)
			}
		}
		if multi {
			b.WriteString("（可多选：数字或 Space 勾选，Enter 提交）\n")
		}
	}
	if other {
		fmt.Fprintf(&b, "\n其他答复：%s▏\n", otherText)
	} else {
		b.WriteString("\n按 o 以「其他」自由输入答复。\n")
	}
	b.WriteString("Enter 提交 · Esc 稍后（问题保持待答，仍可用 /reply 答复）")
	return truncateReviewLines(b.String(), width)
}

// popupQuestion returns the oldest pending live question the user has not
// deferred. Questions restored by a session load are never marked live, so a
// restored session opens without dialog noise (spec F4: leftover questions
// stay answerable through /reply).
func (m Model) popupQuestion() (sessionlog.PendingQuestion, bool) {
	for _, q := range m.Questions {
		if q.Status != sessionlog.QuestionPending || !m.liveQuestions[q.QuestionID] || m.questionDismissed[q.QuestionID] {
			continue
		}
		return q, true
	}
	return sessionlog.PendingQuestion{}, false
}

// handleQuestionKey drives the question dialog. Number keys answer a
// single-select question immediately; a multi-select question toggles the
// chosen options and Enter submits them joined with commas. "o" switches to
// the free-form "other" input; Esc defers the dialog while the question stays
// pending and remains answerable through /reply.
func (m Model) handleQuestionKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	question, ok := m.popupQuestion()
	if !ok {
		return m, nil
	}
	if m.questionDialogID != question.QuestionID {
		m.resetQuestionDialog()
		m.questionDialogID = question.QuestionID
	}
	_, _, options, multi := parseQuestionSpec(question.Prompt)
	if m.QuestionOther {
		return m.handleQuestionOtherKey(k, question)
	}
	switch k.String() {
	case "up", "k":
		if m.QuestionCursor > 0 {
			m.QuestionCursor--
		}
		return m, nil
	case "down", "j":
		if m.QuestionCursor < len(options)-1 {
			m.QuestionCursor++
		}
		return m, nil
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		index := int(k.String()[0] - '1')
		if index >= len(options) {
			return m, nil
		}
		if multi {
			m.toggleQuestionPick(index)
			return m, nil
		}
		return m.sendQuestionReply(question, options[index].Label)
	case " ":
		if !multi || len(options) == 0 {
			return m, nil
		}
		m.toggleQuestionPick(clamp(m.QuestionCursor, 0, len(options)-1))
		return m, nil
	case "enter":
		if multi {
			text := questionReplyText(options, m.QuestionPicked)
			if text == "" {
				m.Status = "先勾选至少一个选项，或按 o 自由输入。"
				return m, nil
			}
			return m.sendQuestionReply(question, text)
		}
		if len(options) > 0 {
			return m.sendQuestionReply(question, options[clamp(m.QuestionCursor, 0, len(options)-1)].Label)
		}
		// A prompt without parseable options is answered as free text.
		m.QuestionOther, m.questionOtherText = true, ""
		return m, nil
	case "o":
		m.QuestionOther, m.questionOtherText = true, ""
		return m, nil
	case "esc":
		if m.questionDismissed == nil {
			m.questionDismissed = map[string]bool{}
		}
		m.questionDismissed[question.QuestionID] = true
		m.resetQuestionDialog()
		m.Status = "问题已稍后：仍可用 /reply 答复。"
		return m, nil
	}
	return m, nil
}

// handleQuestionOtherKey edits the free-form answer of the question dialog;
// Enter submits the typed text, Esc returns to the option list.
func (m Model) handleQuestionOtherKey(k tea.KeyMsg, question sessionlog.PendingQuestion) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.QuestionOther, m.questionOtherText = false, ""
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.questionOtherText)
		if text == "" {
			m.Status = "请输入非空答复，或按 Esc 返回选项。"
			return m, nil
		}
		return m.sendQuestionReply(question, text)
	case "backspace":
		if runes := []rune(m.questionOtherText); len(runes) > 0 {
			m.questionOtherText = string(runes[:len(runes)-1])
		}
		return m, nil
	}
	if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
		m.questionOtherText += k.String()
		return m, nil
	}
	return m, nil
}

func (m *Model) toggleQuestionPick(index int) {
	if m.QuestionPicked == nil {
		m.QuestionPicked = map[int]bool{}
	}
	m.QuestionPicked[index] = !m.QuestionPicked[index]
}

// questionReplyText assembles the reply text of a multi-select answer: the
// picked labels in option order, joined with commas.
func questionReplyText(options []questionOption, picked map[int]bool) string {
	var chosen []string
	for i, option := range options {
		if picked[i] {
			chosen = append(chosen, option.Label)
		}
	}
	return strings.Join(chosen, ",")
}

// resetQuestionDialog clears the per-question dialog state; the next popup
// starts from the first option with nothing picked.
func (m *Model) resetQuestionDialog() {
	m.questionDialogID = ""
	m.QuestionCursor, m.QuestionPicked = 0, nil
	m.QuestionOther, m.questionOtherText = false, ""
}

// sendQuestionReply submits one question answer through the reply op. The
// dialog state resets immediately; the popup drops once the service confirms
// the reply and the refreshed question list marks it replied.
func (m Model) sendQuestionReply(question sessionlog.PendingQuestion, text string) (tea.Model, tea.Cmd) {
	m.resetQuestionDialog()
	m.Pending = true
	m.Status = "正在记录答复…"
	return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "reply", SessionID: m.ActiveSession, QuestionID: question.QuestionID, Text: text})
}

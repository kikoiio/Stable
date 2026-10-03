package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

type Composer struct {
	Input     textarea.Model
	MaxHeight int
	Focused   bool
}

func NewComposer() Composer {
	in := textarea.New()
	in.Prompt = ""
	in.Placeholder = "输入消息 · Enter 发送 · Ctrl+J 换行"
	in.ShowLineNumbers = false
	in.SetHeight(1)
	in.CharLimit = 0
	return Composer{Input: in, MaxHeight: 6}
}
func (c *Composer) SetSize(width, maxHeight int) {
	c.MaxHeight = max(1, maxHeight)
	c.Input.SetWidth(max(1, width))
	c.resize()
}
func (c *Composer) resize() {
	n := strings.Count(c.Input.Value(), "\n") + 1
	c.Input.SetHeight(min(max(1, n), max(1, c.MaxHeight)))
}
func (c *Composer) SetValue(s string) { c.Input.SetValue(s); c.resize() }
func (c Composer) Value() string      { return c.Input.Value() }
func (c *Composer) Focus() tea.Cmd    { c.Focused = true; return c.Input.Focus() }
func (c *Composer) Blur()             { c.Focused = false; c.Input.Blur() }
func (c Composer) Height() int        { return max(1, min(strings.Count(c.Value(), "\n")+1, c.MaxHeight)) }
func (c Composer) View() string       { return c.Input.View() }
func (c *Composer) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	c.Input, cmd = c.Input.Update(msg)
	c.resize()
	return cmd
}

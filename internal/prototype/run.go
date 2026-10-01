package prototype

import tea "github.com/charmbracelet/bubbletea"

// Run starts the isolated, in-memory interaction prototype.
func Run() error {
	_, err := tea.NewProgram(NewModel(), tea.WithAltScreen()).Run()
	return err
}

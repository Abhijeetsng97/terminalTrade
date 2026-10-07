package tui

import (
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Abhijeetsng97/terminalTrade/internal/app"
)

// NewProgram builds the bubbletea program bound to an SSH session
// (or the local terminal when in/out are nil).
func NewProgram(a *app.App, in io.Reader, out io.Writer) *tea.Program {
	m := New(a)
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if in != nil && out != nil {
		opts = append(opts, tea.WithInput(in), tea.WithOutput(out))
	}
	return tea.NewProgram(m, opts...)
}
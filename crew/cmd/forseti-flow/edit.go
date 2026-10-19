// The builder TUI: a full-screen editor over the raw flow YAML with
// validate-on-save. Forms for individual constructs remain a later refinement;
// keeping the source visible is deliberate — the file is the builder's model.
package main

import (
	"fmt"
	"os"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"forseti/crew/internal/flow"
)

type editorModel struct {
	path   string
	ta     textarea.Model
	status string
	width  int
	height int
}

func newEditorModel(path string, data []byte) editorModel {
	ta := textarea.New()
	ta.SetValue(string(data))
	ta.Focus()
	ta.CharLimit = 0
	return editorModel{path: path, ta: ta, status: "ctrl+s save (validates) · esc quit"}
}

func (m editorModel) Init() tea.Cmd { return textarea.Blink }

func (m editorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ta.SetWidth(max(20, msg.Width-2))
		m.ta.SetHeight(max(5, msg.Height-4))
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "ctrl+s":
			text := m.ta.Value()
			if _, err := flow.Load([]byte(text)); err != nil {
				m.status = "NOT SAVED: " + err.Error()
				return m, nil
			}
			if err := os.WriteFile(m.path, []byte(text), 0o644); err != nil {
				m.status = "write failed: " + err.Error()
				return m, nil
			}
			m.status = "saved " + m.path
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m editorModel) View() string {
	return fmt.Sprintf("%s\n%s\n%s", m.status, m.ta.View(), "ctrl+s save · esc quit")
}

func runFlowEditor(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(newEditorModel(path, data), tea.WithAltScreen()).Run()
	return err
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

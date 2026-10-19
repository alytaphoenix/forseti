package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestEditorValidateOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flow.yaml")
	valid := "version: 1\nname: t\nevidence: {x: {kind: shell, run: \"true\"}}\non: {pre_commit: {require: [x]}}\n"
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newEditorModel(path, []byte(valid))
	m.ta.SetValue(valid + "\n# edited\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	mm := next.(editorModel)
	if !strings.HasPrefix(mm.status, "saved") {
		t.Fatalf("status = %q", mm.status)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# edited") {
		t.Fatalf("file not saved:\n%s", data)
	}

	bad := "version: 1\nname: t\nnope: 1\n"
	before, _ := os.ReadFile(path)
	m2 := newEditorModel(path, before)
	m2.ta.SetValue(bad)
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	mm = next.(editorModel)
	if !strings.HasPrefix(mm.status, "NOT SAVED") {
		t.Fatalf("invalid save status = %q", mm.status)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("invalid content was written")
	}
}

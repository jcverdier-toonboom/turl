package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jcverdier/turl/internal/secrets"
	"github.com/jcverdier/turl/internal/spec"
)

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestTreeNavigation(t *testing.T) {
	ws, err := spec.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(ws, secrets.Open(ws.Dir))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if got := m.selectedKey(); got != "flow:example-pipeline" {
		t.Fatalf("start = %q (cursor %d, rows %+v)", got, m.cursor, m.rows)
	}
	m.Update(key("j"))
	if got := m.selectedKey(); got != "coll:httpbin" {
		t.Fatalf("after j = %q (cursor %d)", got, m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // fold
	if !m.folded("httpbin") || m.selectedKey() != "coll:httpbin" {
		t.Fatalf("fold: folded=%v sel=%q", m.folded("httpbin"), m.selectedKey())
	}
	m.Update(key("j"))
	if got := m.selectedKey(); got != "coll:" {
		t.Fatalf("after fold+j = %q", got)
	}
	m.Update(key("l"))
	m.Update(key("j"))
	if got := m.selectedKey(); got != "req:echo-ws" {
		t.Fatalf("default collection child = %q", got)
	}
	m.Update(key("h"))
	if got := m.selectedKey(); got != "coll:" {
		t.Fatalf("h to parent = %q", got)
	}
}

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/jcverdier/turl/internal/runner"
	"github.com/jcverdier/turl/internal/secrets"
	"github.com/jcverdier/turl/internal/spec"
	"github.com/jcverdier/turl/internal/vars"
)

func TestSecretsNeverShown(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	ws, _ := spec.Init(t.TempDir())
	marker := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(ws.Dir, secrets.FileName),
		[]byte("key: {command: 'touch "+marker+"; echo sk-very-secret-1'}\n"), 0o644)
	os.WriteFile(ws.CollectionPath("httpbin"), []byte("headers: {Authorization: 'Bearer {{$secret.key}}'}\n"), 0o644)
	store := secrets.Open(ws.Dir)
	vars.Secrets = store
	defer func() { vars.Secrets = nil }()

	m := New(ws, store)
	m.width, m.height = 120, 30
	m.layout()
	m.selectKey("req:httpbin/get")
	m.refresh(false)
	view := ansi.Strip(m.pager.Plain())
	if !strings.Contains(view, "Bearer ‹secret:key›") {
		t.Fatalf("preview should show the marker:\n%s", view)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("previewing a request ran the secret command")
	}

	// Once fetched (e.g. by a run), the value is redacted from any pane,
	// including a response echoing it.
	if _, err := store.Get("key"); err != nil {
		t.Fatal(err)
	}
	m.runs["req:httpbin/get"] = &runState{result: &runner.Result{
		StatusCode: 200, Status: "200 OK", Headers: map[string][]string{},
		Body: []byte(`{"headers": {"Authorization": "Bearer sk-very-secret-1"}}`),
	}}
	m.setTab(tabResponse)
	if view := m.pager.Plain(); strings.Contains(view, "sk-very-secret-1") || !strings.Contains(view, "Bearer ••••••••") {
		t.Fatalf("response not redacted:\n%s", view)
	}
}

package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jcverdier/turl/internal/vars"
)

// setup writes secrets.yaml with a command that counts its runs.
func setup(t *testing.T, defs string) (*Store, string) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	defs = strings.ReplaceAll(defs, "COUNTER", counter)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(defs), 0o644); err != nil {
		t.Fatal(err)
	}
	return Open(dir), counter
}

func runs(t *testing.T, counter string) int {
	b, _ := os.ReadFile(counter)
	return strings.Count(string(b), "x")
}

func TestCacheAndPermissions(t *testing.T) {
	s, counter := setup(t, `
key:
  command: |
    printf x >> COUNTER; echo '{"SecretString": {"api_key": "sk-123456789"}}'
  jq: .SecretString.api_key
`)
	for i := 0; i < 3; i++ {
		v, err := s.Get("key")
		if err != nil || v != "sk-123456789" {
			t.Fatalf("get: %q %v", v, err)
		}
	}
	if n := runs(t, counter); n != 1 {
		t.Fatalf("command ran %d times, want 1", n)
	}
	// A new process (fresh Store) reads the disk cache.
	if v, _ := Open(filepath.Dir(s.Path())).Get("key"); v != "sk-123456789" || runs(t, counter) != 1 {
		t.Fatal("disk cache not reused")
	}
	es, _ := os.ReadDir(s.CacheDir())
	if len(es) != 1 {
		t.Fatalf("cache entries: %d", len(es))
	}
	fi, _ := os.Stat(filepath.Join(s.CacheDir(), es[0].Name()))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %o", fi.Mode().Perm())
	}
	for _, d := range s.privateDirs() {
		if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %o", d, st.Mode().Perm())
		}
	}
	s.Invalidate("key")
	if _, err := s.Get("key"); err != nil || runs(t, counter) != 2 {
		t.Fatalf("invalidate did not refetch: %v", err)
	}
	if got := Redact("Authorization: Bearer sk-123456789"); got != "Authorization: Bearer ••••••••" {
		t.Fatalf("redact: %q", got)
	}
}

func TestRefusesLooseCacheDir(t *testing.T) {
	s, _ := setup(t, "k: {command: echo value-123}\n")
	if _, err := s.Get("k"); err != nil {
		t.Fatal(err)
	}
	os.Chmod(s.CacheDir(), 0o755)
	if err := s.checkDir(); err == nil {
		t.Fatal("world-readable cache dir accepted")
	}
}

func TestErrorsAndMasking(t *testing.T) {
	s, counter := setup(t, `
bad: {command: "echo boom >&2; exit 3"}
counted: {command: "printf x >> COUNTER; echo tok-abcdef"}
`)
	if _, err := s.Get("bad"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want stderr in error, got %v", err)
	}
	if _, err := s.Get("nope"); err == nil {
		t.Fatal("unknown secret accepted")
	}
	vars.Secrets = s
	defer func() { vars.Secrets = nil }()
	scope := vars.NewScope(vars.Layer{Name: "env", Vars: map[string]string{"auth": "Bearer {{$secret.counted}}"}})
	if got, _ := scope.Masked().Render("{{auth}}"); got != "Bearer ‹secret:counted›" {
		t.Fatalf("masked: %q", got)
	}
	if refs := scope.SecretRefs("{{auth}}"); len(refs) != 1 || refs[0] != "counted" {
		t.Fatalf("refs: %v", refs)
	}
	if runs(t, counter) != 0 {
		t.Fatal("masked rendering ran the command")
	}
	if got, _ := scope.Render("{{auth}}"); got != "Bearer tok-abcdef" {
		t.Fatalf("render: %q", got)
	}
}

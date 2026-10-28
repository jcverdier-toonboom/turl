// Package secrets fetches secrets by running shell commands (aws, op, vault,
// pass...). Only the commands are stored in the workspace; values are cached
// in a private temp directory for a TTL so rotation is picked up without
// running the command on every request.
package secrets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jcverdier/turl/internal/jq"
)

// FileName is the secrets definition file in a workspace.
const FileName = "secrets.yaml"

const (
	defaultTTL     = time.Hour
	commandTimeout = 2 * time.Minute
)

// Def describes how to obtain one secret.
type Def struct {
	Description string `yaml:"description,omitempty"`
	Command     string `yaml:"command"`
	JQ          string `yaml:"jq,omitempty"`  // extract a field when the command prints JSON
	TTL         string `yaml:"ttl,omitempty"` // cache lifetime, default 1h; "0" = memory only
}

func (d Def) ttl() time.Duration {
	if d.TTL == "" {
		return defaultTTL
	}
	if d.TTL == "0" {
		return 0
	}
	t, err := time.ParseDuration(d.TTL)
	if err != nil || t < 0 {
		return defaultTTL
	}
	return t
}

// Store resolves secrets of one workspace. It is safe for concurrent use.
type Store struct {
	path     string // secrets.yaml
	cacheDir string

	mu  sync.Mutex
	mem map[string]entry // cache key -> value, for ttl 0 and to avoid disk reads
}

type entry struct {
	value string
	at    time.Time
}

func (e entry) fresh(ttl time.Duration) bool { return ttl == 0 || time.Since(e.at) < ttl }

// Open returns the store of a workspace directory.
func Open(workspaceDir string) *Store {
	h := sha256.Sum256([]byte(workspaceDir))
	return &Store{
		path:     filepath.Join(workspaceDir, FileName),
		cacheDir: filepath.Join(baseCacheDir(), hex.EncodeToString(h[:8])),
		mem:      map[string]entry{},
	}
}

// userDir is a per-user directory under the system temp dir ($TMPDIR is
// already per-user on macOS; /tmp is shared on Linux, hence the uid).
func userDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("turl-%d", os.Getuid()))
}

func baseCacheDir() string { return filepath.Join(userDir(), "secrets") }

// privateDirs are the directories that must be ours and 0700, outermost last.
func (s *Store) privateDirs() []string {
	var out []string
	stop := userDir()
	for d := s.cacheDir; ; d = filepath.Dir(d) {
		out = append(out, d)
		if d == stop || d == filepath.Dir(d) {
			return out
		}
	}
}

func (s *Store) Path() string     { return s.path }
func (s *Store) CacheDir() string { return s.cacheDir }

// Defs loads secrets.yaml (re-read on every call so edits apply at once).
func (s *Store) Defs() (map[string]Def, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Def{}, nil
	}
	if err != nil {
		return nil, err
	}
	defs := map[string]Def{}
	if err := yaml.Unmarshal(b, &defs); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return defs, nil
}

func (s *Store) def(name string) (Def, error) {
	defs, err := s.Defs()
	if err != nil {
		return Def{}, err
	}
	d, ok := defs[name]
	if !ok {
		return Def{}, fmt.Errorf("unknown secret %q: define it in %s", name, s.path)
	}
	if strings.TrimSpace(d.Command) == "" {
		return Def{}, fmt.Errorf("secret %q has no command", name)
	}
	return d, nil
}

// cacheKey changes when the command or filter changes, so editing a
// definition never serves a stale value.
func cacheKey(name string, d Def) string {
	h := sha256.Sum256([]byte(name + "\x00" + d.Command + "\x00" + d.JQ))
	return hex.EncodeToString(h[:12])
}

// Get returns a secret, from cache when fresh.
func (s *Store) Get(name string) (string, error) {
	d, err := s.def(name)
	if err != nil {
		return "", err
	}
	key := cacheKey(name, d)
	ttl := d.ttl()

	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.mem[key]; ok && e.fresh(ttl) {
		return e.value, nil
	}
	if ttl > 0 {
		if e, ok := s.readCache(key, ttl); ok {
			s.mem[key] = e
			register(e.value)
			return e.value, nil
		}
	}
	v, err := fetch(name, d)
	if err != nil {
		return "", err
	}
	s.mem[key] = entry{value: v, at: time.Now()}
	register(v)
	if ttl > 0 {
		if err := s.writeCache(key, v); err != nil {
			// Still usable; it just won't survive this process.
			fmt.Fprintf(os.Stderr, "turl: not caching secret %s: %v\n", name, err)
		}
	}
	return v, nil
}

func (s *Store) readCache(key string, ttl time.Duration) (entry, bool) {
	if s.checkDir() != nil {
		return entry{}, false
	}
	p := filepath.Join(s.cacheDir, key)
	st, err := os.Stat(p)
	if err != nil || time.Since(st.ModTime()) >= ttl {
		return entry{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return entry{}, false
	}
	return entry{value: string(b), at: st.ModTime()}, true
}

func (s *Store) writeCache(key, v string) error {
	if err := os.MkdirAll(s.cacheDir, 0o700); err != nil {
		return err
	}
	// MkdirAll does not fix the mode of existing parents; tighten ours.
	for _, d := range s.privateDirs() {
		if fi, err := os.Lstat(d); err == nil && fi.IsDir() {
			if uid, ok := owner(fi); !ok || uid == os.Getuid() {
				if err := os.Chmod(d, 0o700); err != nil {
					return err
				}
			}
		}
	}
	if err := s.checkDir(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.cacheDir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(v); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.cacheDir, key))
}

// checkDir refuses a cache directory that another user could read or swap:
// it must be a real directory, owned by us, with mode 0700.
func (s *Store) checkDir() error {
	for _, d := range s.privateDirs() {
		st, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory", d)
		}
		if st.Mode().Perm() != 0o700 {
			return fmt.Errorf("%s has mode %o, want 700", d, st.Mode().Perm())
		}
		if uid, ok := owner(st); ok && uid != os.Getuid() {
			return fmt.Errorf("%s is owned by uid %d", d, uid)
		}
	}
	return nil
}

func fetch(name string, d Def) (string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-c", d.Command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		if msg != "" {
			return "", fmt.Errorf("secret %s: %v: %s", name, err, msg)
		}
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	v := strings.TrimRight(stdout.String(), "\r\n")
	if d.JQ != "" {
		out, err := jq.Eval(d.JQ, jq.DecodeOne([]byte(v)), jq.Env{})
		if err != nil {
			return "", fmt.Errorf("secret %s: jq %s: %w", name, d.JQ, err)
		}
		if len(out) == 0 || out[0] == nil {
			return "", fmt.Errorf("secret %s: jq %s returned null", name, d.JQ)
		}
		v = jq.ToString(out[0])
	}
	if v == "" {
		return "", fmt.Errorf("secret %s: command printed nothing", name)
	}
	return v, nil
}

// Invalidate forgets cached values so the next Get re-runs the commands.
func (s *Store) Invalidate(names ...string) {
	defs, err := s.Defs()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range names {
		d, ok := defs[n]
		if !ok {
			continue
		}
		key := cacheKey(n, d)
		delete(s.mem, key)
		_ = os.Remove(filepath.Join(s.cacheDir, key))
	}
}

// Refresh re-fetches secrets now; with no names, every defined secret.
func (s *Store) Refresh(names ...string) error {
	if len(names) == 0 {
		defs, err := s.Defs()
		if err != nil {
			return err
		}
		for n := range defs {
			names = append(names, n)
		}
	}
	s.Invalidate(names...)
	var errs []error
	for _, n := range names {
		if _, err := s.Get(n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Clear deletes every cached value of this workspace.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mem = map[string]entry{}
	return os.RemoveAll(s.cacheDir)
}

// Info describes a secret without its value.
type Info struct {
	Name        string
	Description string
	Command     string
	TTL         time.Duration
	Cached      bool
	Age         time.Duration
}

func (s *Store) List() ([]Info, error) {
	defs, err := s.Defs()
	if err != nil {
		return nil, err
	}
	var out []Info
	for n, d := range defs {
		in := Info{Name: n, Description: d.Description, Command: d.Command, TTL: d.ttl()}
		key := cacheKey(n, d)
		if st, err := os.Stat(filepath.Join(s.cacheDir, key)); err == nil && (in.TTL == 0 || time.Since(st.ModTime()) < in.TTL) {
			in.Cached, in.Age = true, time.Since(st.ModTime())
		}
		s.mu.Lock()
		if _, ok := s.mem[key]; ok && !in.Cached {
			in.Cached = true
		}
		s.mu.Unlock()
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Template is written by turl init and :secret edit.
const Template = `# Secrets are fetched by running shell commands; only the commands live here,
# so this file is safe to commit. Use a secret anywhere as {{$secret.NAME}}.
#
# Values are cached in a private temp directory (mode 0600) for ` + "`ttl`" + `, and
# re-fetched automatically when a request using them gets a 401 or 403.
#
# ai_api_key:
#   description: API key of the AI service
#   command: aws secretsmanager get-secret-value --secret-id prod/ai --query SecretString --output text
#   jq: .api_key      # optional: pick a field when the command prints JSON
#   ttl: 1h           # default 1h; 0 keeps the value in memory only
#
# github_token:
#   command: gh auth token
#   ttl: 12h
`

// --- redaction ---------------------------------------------------------------

var (
	redactMu sync.RWMutex
	known    = map[string]bool{}
)

// register remembers a value so Redact can hide it from the UI.
func register(v string) {
	if len(v) < 6 { // too short to redact without mangling unrelated text
		return
	}
	redactMu.Lock()
	known[v] = true
	redactMu.Unlock()
}

// Redact replaces every secret value fetched by this process with a mask.
func Redact(s string) string {
	redactMu.RLock()
	defer redactMu.RUnlock()
	for v := range known {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "••••••••")
		}
	}
	return s
}

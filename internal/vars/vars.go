// Package vars implements {{variable}} substitution.
package vars

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var placeholder = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// Scope resolves variables through ordered layers (first match wins).
// A Scope is not safe for concurrent use; hand a Snapshot to goroutines.
type Scope struct {
	layers []Layer
	// mask renders {{$secret.x}} as a marker instead of fetching it, for
	// previews that must neither leak secrets nor run their commands.
	mask bool
}

// SecretProvider fetches secrets for {{$secret.NAME}}.
type SecretProvider interface {
	Get(name string) (string, error)
	Invalidate(names ...string)
}

// Secrets is the provider used by every scope; nil disables secrets.
var Secrets SecretProvider

// SecretMarker is what a masked scope renders in place of a secret.
func SecretMarker(name string) string { return "‹secret:" + name + "›" }

var secretMarker = regexp.MustCompile(`‹secret:([^›]+)›`)

type Layer struct {
	Name string
	Vars map[string]string
}

func NewScope(layers ...Layer) *Scope { return &Scope{layers: layers} }

// Child returns a scope with an extra highest-priority layer.
func (s *Scope) Child(name string, v map[string]string) *Scope {
	if v == nil {
		v = map[string]string{}
	}
	ls := append([]Layer{{Name: name, Vars: v}}, s.layers...)
	return &Scope{layers: ls, mask: s.mask}
}

// InsertBefore returns a scope with l placed just above the first layer whose
// name starts with prefix (lower priority than everything before it), or at
// the bottom when there is no such layer.
func (s *Scope) InsertBefore(prefix string, l Layer) *Scope {
	ls := make([]Layer, 0, len(s.layers)+1)
	inserted := false
	for _, x := range s.layers {
		if !inserted && strings.HasPrefix(x.Name, prefix) {
			ls = append(ls, l)
			inserted = true
		}
		ls = append(ls, x)
	}
	if !inserted {
		ls = append(ls, l)
	}
	return &Scope{layers: ls, mask: s.mask}
}

// Snapshot deep-copies the scope.
func (s *Scope) Snapshot() *Scope {
	ls := make([]Layer, len(s.layers))
	for i, l := range s.layers {
		m := make(map[string]string, len(l.Vars))
		for k, v := range l.Vars {
			m[k] = v
		}
		ls[i] = Layer{Name: l.Name, Vars: m}
	}
	return &Scope{layers: ls, mask: s.mask}
}

// Masked returns a scope that renders secrets as markers.
func (s *Scope) Masked() *Scope {
	return &Scope{layers: s.layers, mask: true}
}

// SecretRefs lists the secrets the strings use, directly or through other
// variables, without fetching them.
func (s *Scope) SecretRefs(ins ...string) []string {
	ms := s.Masked()
	var out []string
	for _, in := range ins {
		r, err := ms.Render(in)
		if err != nil {
			continue
		}
		for _, m := range secretMarker.FindAllStringSubmatch(r, -1) {
			out = append(out, m[1])
		}
	}
	return dedupe(out)
}

func (s *Scope) Lookup(name string) (string, string, bool) {
	for _, l := range s.layers {
		if v, ok := l.Vars[name]; ok {
			return v, l.Name, true
		}
	}
	return "", "", false
}

// Entry is a resolved variable, for display.
type Entry struct {
	Name, Value, Source string
	Shadowed            bool
}

func (s *Scope) Entries() []Entry {
	seen := map[string]bool{}
	var out []Entry
	for _, l := range s.layers {
		keys := make([]string, 0, len(l.Vars))
		for k := range l.Vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, Entry{Name: k, Value: l.Vars[k], Source: l.Name, Shadowed: seen[k]})
			seen[k] = true
		}
	}
	return out
}

// MissingError lists undefined variables.
type MissingError struct{ Names []string }

func (e *MissingError) Error() string {
	return "undefined variable(s): " + strings.Join(e.Names, ", ")
}

// Render substitutes placeholders. Values are rendered recursively so env
// values can reference other vars (e.g. token: "{{$env.API_TOKEN}}").
func (s *Scope) Render(in string) (string, error) {
	return s.render(in, 0)
}

func (s *Scope) render(in string, depth int) (string, error) {
	if depth > 8 {
		return "", fmt.Errorf("variable expansion too deep in %q", in)
	}
	var missing []string
	var firstErr error
	out := placeholder.ReplaceAllStringFunc(in, func(m string) string {
		name := strings.TrimSpace(placeholder.FindStringSubmatch(m)[1])
		v, ok, err := s.resolve(name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return m
		}
		if !ok {
			missing = append(missing, name)
			return m
		}
		if strings.Contains(v, "{{") {
			r, err := s.render(v, depth+1)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return m
			}
			v = r
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	if len(missing) > 0 {
		return "", &MissingError{Names: dedupe(missing)}
	}
	return out, nil
}

// Missing reports undefined plain variables referenced by the strings.
func (s *Scope) Missing(ins ...string) []string {
	var out []string
	for _, in := range ins {
		for _, m := range placeholder.FindAllStringSubmatch(in, -1) {
			n := strings.TrimSpace(m[1])
			if strings.HasPrefix(n, "$") {
				continue
			}
			if _, _, ok := s.Lookup(n); !ok {
				out = append(out, n)
			}
		}
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

var shCache sync.Map

func (s *Scope) resolve(name string) (string, bool, error) {
	if !strings.HasPrefix(name, "$") {
		v, _, ok := s.Lookup(name)
		return v, ok, nil
	}
	switch {
	case strings.HasPrefix(name, "$secret."):
		n := strings.TrimPrefix(name, "$secret.")
		if s.mask {
			return SecretMarker(n), true, nil
		}
		if Secrets == nil {
			return "", false, fmt.Errorf("{{%s}}: secrets are not configured", name)
		}
		v, err := Secrets.Get(n)
		if err != nil {
			return "", false, err
		}
		return v, true, nil
	case name == "$uuid":
		return uuid(), true, nil
	case name == "$timestamp":
		return strconv.FormatInt(time.Now().Unix(), 10), true, nil
	case name == "$timestamp_ms":
		return strconv.FormatInt(time.Now().UnixMilli(), 10), true, nil
	case name == "$isotime":
		return time.Now().UTC().Format(time.RFC3339), true, nil
	case name == "$randint":
		var b [2]byte
		_, _ = rand.Read(b[:])
		return strconv.Itoa(int(b[0])<<8 | int(b[1])), true, nil
	case strings.HasPrefix(name, "$env."):
		v, ok := os.LookupEnv(strings.TrimPrefix(name, "$env."))
		return v, ok, nil
	case strings.HasPrefix(name, "$sh:"):
		// Run a shell command (e.g. a secret manager); cached per session.
		cmd := strings.TrimSpace(strings.TrimPrefix(name, "$sh:"))
		if v, ok := shCache.Load(cmd); ok {
			return v.(string), true, nil
		}
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		out, err := exec.Command(shell, "-c", cmd).Output()
		if err != nil {
			return "", false, fmt.Errorf("{{$sh:%s}}: %w", cmd, err)
		}
		v := strings.TrimRight(string(out), "\r\n")
		shCache.Store(cmd, v)
		return v, true, nil
	}
	return "", false, fmt.Errorf("unknown builtin {{%s}}", name)
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Collections group requests targeting the same service. A collection is a
// folder of requests/; requests stored directly in requests/ belong to the
// default collection. An optional _collection.yaml in the folder holds
// settings shared by every request of the collection.

const (
	// DefaultCollectionName is how the default collection is displayed.
	DefaultCollectionName = "default collection"
	collectionFile        = "_collection.yaml"
)

// Collection settings.
type Collection struct {
	Name        string            `yaml:"-"`
	Description string            `yaml:"description,omitempty"`
	Vars        map[string]string `yaml:"vars,omitempty"`    // override env vars of the same name
	Headers     map[string]string `yaml:"headers,omitempty"` // a request's own headers win
}

func isCollectionFile(p string) bool { return filepath.Base(p) == collectionFile }

// SplitCollection splits "ai/get-link" into ("ai", "get-link"). Requests
// without a folder belong to the default collection ("").
func SplitCollection(name string) (collection, short string) {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

// JoinCollection is the inverse of SplitCollection.
func JoinCollection(collection, short string) string {
	if collection == "" {
		return short
	}
	return collection + "/" + short
}

// CollectionLabel is the display name of a collection.
func CollectionLabel(name string) string {
	if name == "" {
		return DefaultCollectionName
	}
	return name
}

// ParseCollectionName maps user input to a collection name; "default" and
// "default collection" mean the default collection.
func ParseCollectionName(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	switch strings.ToLower(s) {
	case "", "default", DefaultCollectionName, "-":
		return "", nil
	}
	if strings.Contains(s, "/") {
		return "", fmt.Errorf("collection names cannot contain '/': %q", s)
	}
	return s, ValidName(s)
}

func (w *Workspace) CollectionDir(name string) string {
	return filepath.Join(w.RequestsDir(), filepath.FromSlash(name))
}

func (w *Workspace) CollectionPath(name string) string {
	return filepath.Join(w.CollectionDir(name), collectionFile)
}

// Collections lists the named collections (folders of requests/), including
// empty ones. The default collection is not included.
func (w *Workspace) Collections() []string {
	es, _ := os.ReadDir(w.RequestsDir())
	var out []string
	for _, e := range es {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func (w *Workspace) CollectionExists(name string) bool {
	if name == "" {
		return true
	}
	st, err := os.Stat(w.CollectionDir(name))
	return err == nil && st.IsDir()
}

// LoadCollection returns the settings of a collection; a missing settings
// file yields empty settings.
func (w *Workspace) LoadCollection(name string) (*Collection, error) {
	c := &Collection{Name: name}
	if name == "" {
		return c, nil
	}
	if _, err := os.Stat(w.CollectionPath(name)); os.IsNotExist(err) {
		return c, nil
	}
	if err := readYAML(w.CollectionPath(name), c); err != nil {
		return c, err
	}
	c.Name = name
	return c, nil
}

const collectionTemplate = `# Settings shared by every request of this collection.
description: ""
# vars:                 # defaults for this service; they override env vars of the same name
#   base_url: "{{ai_url}}"
# headers:              # added to every request; a request's own headers win
#   Authorization: "Bearer {{ai_token}}"
`

// NewCollection creates the folder and a commented settings file.
func (w *Workspace) NewCollection(name string) error {
	name, err := ParseCollectionName(name)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("the %s always exists", DefaultCollectionName)
	}
	if w.CollectionExists(name) {
		return fmt.Errorf("collection %s already exists", name)
	}
	if err := os.MkdirAll(w.CollectionDir(name), 0o755); err != nil {
		return err
	}
	return os.WriteFile(w.CollectionPath(name), []byte(collectionTemplate), 0o644)
}

// EnsureCollectionFile creates the settings file if missing (for editing).
func (w *Workspace) EnsureCollectionFile(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("the %s has no settings; create a collection with :collection new <name>", DefaultCollectionName)
	}
	p := w.CollectionPath(name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		return p, os.WriteFile(p, []byte(collectionTemplate), 0o644)
	}
	return p, nil
}

// RequestsIn returns the names of the requests of a collection.
func (w *Workspace) RequestsIn(collection string) []string {
	var out []string
	for _, n := range listYAML(w.RequestsDir()) {
		if c, _ := SplitCollection(n); c == collection {
			out = append(out, n)
		}
	}
	return out
}

// RenameCollection renames the folder and updates flow references. It
// returns the number of flows updated.
func (w *Workspace) RenameCollection(oldName, newName string) (int, error) {
	newName, err := ParseCollectionName(newName)
	if err != nil {
		return 0, err
	}
	if oldName == "" || newName == "" {
		return 0, fmt.Errorf("the %s cannot be renamed; use :move to move requests", DefaultCollectionName)
	}
	if w.CollectionExists(newName) {
		return 0, fmt.Errorf("collection %s already exists", newName)
	}
	if err := os.Rename(w.CollectionDir(oldName), w.CollectionDir(newName)); err != nil {
		return 0, err
	}
	return w.updateFlowRefs(func(ref string) (string, bool) {
		if c, short := SplitCollection(ref); c == oldName {
			return JoinCollection(newName, short), true
		}
		return "", false
	})
}

// DeleteCollection removes a collection with its requests.
func (w *Workspace) DeleteCollection(name string) error {
	if name == "" {
		return fmt.Errorf("the %s cannot be deleted", DefaultCollectionName)
	}
	if !w.CollectionExists(name) {
		return fmt.Errorf("no collection %s", name)
	}
	return os.RemoveAll(w.CollectionDir(name))
}

// MoveRequest moves a request to another collection, keeping its short
// name, and updates flow references. It returns the new name.
func (w *Workspace) MoveRequest(it Item, collection string) (string, error) {
	if it.Kind != KindRequest {
		return "", fmt.Errorf("only requests belong to collections")
	}
	collection, err := ParseCollectionName(collection)
	if err != nil {
		return "", err
	}
	_, short := SplitCollection(it.Name)
	newName := JoinCollection(collection, short)
	if newName == it.Name {
		return newName, fmt.Errorf("%s is already in %s", it.Name, CollectionLabel(collection))
	}
	return newName, w.Rename(it, newName)
}

// flowRef matches `request: name` lines of flow files.
var flowRef = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?request:\s*)(["']?)([^"'#\r\n]+?)(["']?)([ \t]*(?:#.*)?)$`)

// updateFlowRefs rewrites `request:` references in every flow file.
func (w *Workspace) updateFlowRefs(rename func(ref string) (string, bool)) (int, error) {
	updated := 0
	for _, n := range listYAML(w.FlowsDir()) {
		p := findFile(w.FlowsDir(), n)
		b, err := os.ReadFile(p)
		if err != nil {
			return updated, err
		}
		changed := false
		out := flowRef.ReplaceAllStringFunc(string(b), func(line string) string {
			m := flowRef.FindStringSubmatch(line)
			if to, ok := rename(strings.TrimSpace(m[3])); ok {
				changed = true
				return m[1] + m[2] + to + m[4] + m[5]
			}
			return line
		})
		if changed {
			if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
				return updated, err
			}
			updated++
		}
	}
	return updated, nil
}

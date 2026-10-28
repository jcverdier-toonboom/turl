// Package spec defines the on-disk format of a turl workspace: requests,
// flows, environments and persisted state, all stored as YAML files so they
// can be edited with any editor and versioned with git.
package spec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jcverdier/turl/internal/secrets"
)

const (
	ProtoHTTP = "http"
	ProtoWS   = "ws"
)

// Request is a single HTTP or WebSocket request.
type Request struct {
	Name        string            `yaml:"-"`
	Collection  string            `yaml:"-"` // "" for the default collection and inline steps
	Description string            `yaml:"description,omitempty"`
	Protocol    string            `yaml:"protocol,omitempty"` // http (default) | ws
	Method      string            `yaml:"method,omitempty"`
	URL         string            `yaml:"url"`
	Query       map[string]string `yaml:"query,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty"`
	Inputs      []string          `yaml:"inputs,omitempty"` // variables prompted before running

	// HTTP body: one of Body, BodyFile, Multipart.
	Body      string            `yaml:"body,omitempty"`
	BodyFile  string            `yaml:"body_file,omitempty"`
	Multipart map[string]string `yaml:"multipart,omitempty"` // value "@path" uploads a file

	// WebSocket.
	Messages []string `yaml:"messages,omitempty"` // sent in order once connected
	Until    string   `yaml:"until,omitempty"`    // jq filter; done when it yields a truthy value
	Count    int      `yaml:"count,omitempty"`    // done after N received messages

	Extract        map[string]string `yaml:"extract,omitempty"` // var -> jq filter
	SaveTo         string            `yaml:"save_to,omitempty"` // write response body to file
	Timeout        string            `yaml:"timeout,omitempty"`
	Insecure       bool              `yaml:"insecure,omitempty"`
	NoFollow       bool              `yaml:"no_follow_redirects,omitempty"`
	ExpectStatus   []int             `yaml:"expect_status,omitempty"` // default: any 2xx
	AllowAnyStatus bool              `yaml:"allow_any_status,omitempty"`
}

func (r *Request) IsWS() bool {
	p := strings.ToLower(r.Protocol)
	if p == ProtoWS || p == "wss" || p == "websocket" {
		return true
	}
	u := strings.ToLower(strings.TrimSpace(r.URL))
	return strings.HasPrefix(u, "ws://") || strings.HasPrefix(u, "wss://")
}

func (r *Request) MethodOrDefault() string {
	if r.IsWS() {
		return "WS"
	}
	if r.Method == "" {
		if r.Body != "" || r.BodyFile != "" || len(r.Multipart) > 0 {
			return "POST"
		}
		return "GET"
	}
	return strings.ToUpper(r.Method)
}

func (r *Request) TimeoutOr(def time.Duration) time.Duration {
	if r.Timeout == "" {
		return def
	}
	d, err := time.ParseDuration(r.Timeout)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// Step is one step of a flow: either a reference to a saved request or an
// inline request definition. Fields set on the step override the referenced
// request.
type Step struct {
	Name    string            `yaml:"name,omitempty"`
	Use     string            `yaml:"request,omitempty"`
	Vars    map[string]string `yaml:"vars,omitempty"`
	Inline  *Request          `yaml:"inline,omitempty"`
	Extract map[string]string `yaml:"extract,omitempty"`
	Until   string            `yaml:"until,omitempty"`
	SaveTo  string            `yaml:"save_to,omitempty"`
	// Retry polls the step until Until matches against the HTTP response.
	Retry      int    `yaml:"retry,omitempty"`
	RetryDelay string `yaml:"retry_delay,omitempty"`
	Skip       bool   `yaml:"skip,omitempty"`
}

func (s *Step) Label(i int) string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Use != "":
		return s.Use
	}
	return fmt.Sprintf("step %d", i+1)
}

// Flow chains requests; values extracted by one step are available to later
// steps as {{var}}.
type Flow struct {
	Name        string            `yaml:"-"`
	Description string            `yaml:"description,omitempty"`
	Inputs      []string          `yaml:"inputs,omitempty"`
	Vars        map[string]string `yaml:"vars,omitempty"`
	Steps       []Step            `yaml:"steps"`
}

// State is persisted between sessions: the active environment and values
// captured with `extract` or `:set`.
type State struct {
	Env    string            `yaml:"env,omitempty"`
	Vars   map[string]string `yaml:"vars,omitempty"`
	Folded []string          `yaml:"folded,omitempty"` // collapsed collections in the TUI
}

// Kind of a collection item.
type Kind int

const (
	KindRequest Kind = iota
	KindFlow
)

// Item is an entry of the collection shown in the TUI.
type Item struct {
	Kind    Kind
	Name    string
	Path    string
	Request *Request
	Flow    *Flow
	Err     error // parse error, item is still listed so it can be fixed
}

// Workspace is a directory holding requests/, flows/, envs/ and state.yaml.
type Workspace struct {
	Dir string
}

func (w *Workspace) RequestsDir() string { return filepath.Join(w.Dir, "requests") }
func (w *Workspace) FlowsDir() string    { return filepath.Join(w.Dir, "flows") }
func (w *Workspace) EnvsDir() string     { return filepath.Join(w.Dir, "envs") }
func (w *Workspace) StatePath() string   { return filepath.Join(w.Dir, "state.yaml") }

func (w *Workspace) RequestPath(name string) string {
	return filepath.Join(w.RequestsDir(), name+".yaml")
}
func (w *Workspace) FlowPath(name string) string { return filepath.Join(w.FlowsDir(), name+".yaml") }
func (w *Workspace) EnvPath(name string) string  { return filepath.Join(w.EnvsDir(), name+".yaml") }

// Find resolves the workspace directory: explicit dir, $TURL_DIR, the nearest
// .turl directory walking up from cwd, then ~/.config/turl.
func Find(explicit string) (*Workspace, error) {
	if explicit != "" {
		return open(explicit)
	}
	if d := os.Getenv("TURL_DIR"); d != "" {
		return open(d)
	}
	if cwd, err := os.Getwd(); err == nil {
		for d := cwd; ; d = filepath.Dir(d) {
			cand := filepath.Join(d, ".turl")
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				return open(cand)
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return open(filepath.Join(home, ".config", "turl"))
}

func open(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	w := &Workspace{Dir: abs}
	for _, d := range []string{w.RequestsDir(), w.FlowsDir(), w.EnvsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// Init creates a workspace in dir and seeds it with examples.
func Init(dir string) (*Workspace, error) {
	w, err := open(dir)
	if err != nil {
		return nil, err
	}
	return w, w.seedExamples()
}

func (w *Workspace) IsEmpty() bool {
	for _, d := range []string{w.RequestsDir(), w.FlowsDir()} {
		if es, _ := os.ReadDir(d); len(es) > 0 {
			return false
		}
	}
	return true
}

func listYAML(dir string) []string {
	var names []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := filepath.Ext(p)
		if ext != ".yaml" && ext != ".yml" || isCollectionFile(p) {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		names = append(names, strings.TrimSuffix(filepath.ToSlash(rel), ext))
		return nil
	})
	sort.Strings(names)
	return names
}

func findFile(dir, name string) string {
	for _, ext := range []string{".yaml", ".yml"} {
		p := filepath.Join(dir, filepath.FromSlash(name)+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(dir, filepath.FromSlash(name)+".yaml")
}

// Items loads all flows and requests (flows first).
func (w *Workspace) Items() []Item {
	var items []Item
	for _, n := range listYAML(w.FlowsDir()) {
		f, err := w.LoadFlow(n)
		items = append(items, Item{Kind: KindFlow, Name: n, Path: findFile(w.FlowsDir(), n), Flow: f, Err: err})
	}
	for _, n := range listYAML(w.RequestsDir()) {
		r, err := w.LoadRequest(n)
		items = append(items, Item{Kind: KindRequest, Name: n, Path: findFile(w.RequestsDir(), n), Request: r, Err: err})
	}
	return items
}

func readYAML(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeYAML(path string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func (w *Workspace) LoadRequest(name string) (*Request, error) {
	r := &Request{}
	if err := readYAML(findFile(w.RequestsDir(), name), r); err != nil {
		return nil, err
	}
	r.Name = name
	r.Collection, _ = SplitCollection(name)
	return r, nil
}

func (w *Workspace) LoadFlow(name string) (*Flow, error) {
	f := &Flow{}
	if err := readYAML(findFile(w.FlowsDir(), name), f); err != nil {
		return nil, err
	}
	f.Name = name
	return f, nil
}

func (w *Workspace) SaveRequest(r *Request) error { return writeYAML(w.RequestPath(r.Name), r) }

// Lookup finds an item by name; flows win over requests with the same name.
// A "flow:" or "req:" prefix disambiguates.
func (w *Workspace) Lookup(name string) (Item, error) {
	switch {
	case strings.HasPrefix(name, "flow:"):
		n := strings.TrimPrefix(name, "flow:")
		f, err := w.LoadFlow(n)
		return Item{Kind: KindFlow, Name: n, Path: findFile(w.FlowsDir(), n), Flow: f}, err
	case strings.HasPrefix(name, "req:"):
		n := strings.TrimPrefix(name, "req:")
		r, err := w.LoadRequest(n)
		return Item{Kind: KindRequest, Name: n, Path: findFile(w.RequestsDir(), n), Request: r}, err
	}
	if _, err := os.Stat(findFile(w.FlowsDir(), name)); err == nil {
		return w.Lookup("flow:" + name)
	}
	if _, err := os.Stat(findFile(w.RequestsDir(), name)); err == nil {
		return w.Lookup("req:" + name)
	}
	return Item{}, fmt.Errorf("no request or flow named %q", name)
}

func (w *Workspace) Envs() []string { return listYAML(w.EnvsDir()) }

func (w *Workspace) LoadEnv(name string) (map[string]string, error) {
	if name == "" {
		return map[string]string{}, nil
	}
	m := map[string]string{}
	if err := readYAML(findFile(w.EnvsDir(), name), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (w *Workspace) LoadState() *State {
	s := &State{}
	_ = readYAML(w.StatePath(), s)
	if s.Vars == nil {
		s.Vars = map[string]string{}
	}
	if s.Env == "" {
		if envs := w.Envs(); len(envs) > 0 {
			s.Env = envs[0]
		}
	}
	return s
}

func (w *Workspace) SaveState(s *State) error { return writeYAML(w.StatePath(), s) }

// ValidName rejects names that would escape the workspace.
func ValidName(name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:*?\"<>|") {
		return fmt.Errorf("invalid name %q", name)
	}
	return nil
}

// Templates for new items, written as text so comments document the format.
const httpTemplate = `# HTTP request. Variables: {{name}} from env / extracted values / :set,
# builtins: {{$uuid}} {{$timestamp}} {{$isotime}} {{$env.HOME}} {{$sh:cmd}}
description: ""
method: GET
url: "{{base_url}}/"
headers:
  Accept: application/json
# query: {page: "1"}
# body: |
#   {"hello": "world"}
# body_file: ./asset.png          # raw upload (e.g. presigned PUT)
# multipart: {file: "@./asset.png", kind: image}
# inputs: [file]                  # prompt for these before running
# extract:                        # jq filters applied to the JSON body
#   token: .access_token          # also available: $status, $headers
# save_to: ./out.bin
# timeout: 30s
`

const wsTemplate = `# WebSocket request. Messages are sent in order once connected.
# Without until/count the connection stays open in the TUI: press i to send.
description: ""
protocol: ws
url: "{{ws_url}}"
headers: {}
messages:
  - '{"type": "ping"}'
# until: 'select(.status == "done")'   # jq; stop when it yields a truthy value
# count: 1                              # or stop after N messages
# extract:                              # applied to the last message, $all = every message
#   result: .result.url
# timeout: 60s
`

const flowTemplate = `# Flow: steps run in order, extracted vars flow into the next steps.
description: ""
# inputs: [file]
vars: {}
steps:
  - request: some-request
  # - request: other
  #   vars: {key: value}
  #   extract: {id: .id}
  # - name: poll
  #   request: status
  #   until: '.state == "done"'
  #   retry: 30
  #   retry_delay: 2s
`

func (w *Workspace) NewItem(kind, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	var path, tmpl string
	switch kind {
	case "http", "req", "request":
		path, tmpl = w.RequestPath(name), httpTemplate
	case "ws", "websocket":
		path, tmpl = w.RequestPath(name), wsTemplate
	case "flow":
		path, tmpl = w.FlowPath(name), flowTemplate
	case "env":
		path, tmpl = w.EnvPath(name), "base_url: https://example.com\n"
	default:
		return "", fmt.Errorf("unknown kind %q (http|ws|flow|env)", kind)
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(tmpl), 0o644)
}

// Rename moves an item file.
func (w *Workspace) Rename(it Item, newName string) error {
	if err := ValidName(newName); err != nil {
		return err
	}
	dir := w.RequestsDir()
	if it.Kind == KindFlow {
		dir = w.FlowsDir()
	}
	dst := filepath.Join(dir, filepath.FromSlash(newName)+filepath.Ext(it.Path))
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", newName)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(it.Path, dst); err != nil {
		return err
	}
	if it.Kind == KindRequest {
		_, err := w.updateFlowRefs(func(ref string) (string, bool) { return newName, ref == it.Name })
		return err
	}
	return nil
}

// Duplicate copies an item file under a new name.
func (w *Workspace) Duplicate(it Item, newName string) error {
	if err := ValidName(newName); err != nil {
		return err
	}
	b, err := os.ReadFile(it.Path)
	if err != nil {
		return err
	}
	dir := w.RequestsDir()
	if it.Kind == KindFlow {
		dir = w.FlowsDir()
	}
	dst := filepath.Join(dir, filepath.FromSlash(newName)+filepath.Ext(it.Path))
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", newName)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func (w *Workspace) seedExamples() error {
	files := map[string]string{
		w.EnvPath("local"): `# Environment variables, select with :env <name> or E
base_url: https://httpbin.org
ws_url: wss://echo.websocket.org
`,
		filepath.Join(w.Dir, secrets.FileName): secrets.Template,
		w.CollectionPath("httpbin"): `# Settings shared by every request of the httpbin collection.
description: Requests against httpbin.org, a request/response echo service
vars:
  base_url: https://httpbin.org   # wins over the env's base_url for this collection
headers:
  X-Client: turl                  # added to every request of the collection
`,
		w.RequestPath("httpbin/get"): `description: Simple GET, try | then .headers in the response pane
method: GET
url: "{{base_url}}/get"
query:
  hello: world
headers:
  Accept: application/json
  X-Request-Id: "{{$uuid}}"
extract:
  origin: .origin
`,
		w.RequestPath("httpbin/post-json"): `description: POST a JSON body that references a previously extracted var
method: POST
url: "{{base_url}}/anything"
headers:
  Content-Type: application/json
body: |
  {"origin": "{{origin}}", "at": "{{$isotime}}"}
extract:
  echoed_at: .json.at
`,
		w.RequestPath("httpbin/upload"): `description: Raw file upload (like a presigned PUT); asks for the file to send
method: PUT
url: "{{upload_url}}"
inputs: [file]
headers:
  Content-Type: application/octet-stream
body_file: "{{file}}"
`,
		w.RequestPath("echo-ws"): `description: Interactive WebSocket, press i to send messages, :close to disconnect
protocol: ws
url: "{{ws_url}}"
messages:
  - '{"type": "hello", "id": "{{$uuid}}"}'
`,
		w.FlowPath("example-pipeline"): `description: >
  The "AI service" pattern: get an upload link, upload an asset, then run the
  job over WebSocket. Here simulated with httpbin + an echo server.
inputs: [file]
steps:
  - name: get upload link
    request: httpbin/post-json
    extract:
      # a real API would return it, e.g. .data.upload_url
      upload_url: '"{{base_url}}/put"'
  - name: upload asset
    request: httpbin/upload
  - name: run job over ws
    inline:
      protocol: ws
      url: "{{ws_url}}"
      messages:
        - '{"type": "job", "upload": "{{upload_url}}"}'
      until: 'select(.type? == "job")'
      extract:
        job_upload: .upload
`,
	}
	for p, c := range files {
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			return err
		}
	}
	return nil
}

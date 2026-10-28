// Package tui is the interactive terminal UI.
package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jcverdier/turl/internal/secrets"
	"github.com/jcverdier/turl/internal/spec"
	"github.com/jcverdier/turl/internal/vars"
)

type focus int

const (
	focusList focus = iota
	focusPager
)

type mode int

const (
	modeNormal mode = iota
	modeCommand
	modeSearch
	modeJQ
	modeInsert
	modeInput
	modeConfirm
)

func (m mode) String() string {
	return [...]string{"NORMAL", "COMMAND", "SEARCH", "JQ", "INSERT", "INPUT", "CONFIRM"}[m]
}

type tab int

const (
	tabRequest tab = iota
	tabResponse
	tabHeaders
	tabLog
	tabVars
	numTabs
)

var tabNames = [...]string{"Request", "Response", "Headers", "Log", "Vars"}

type statusKind int

const (
	stInfo statusKind = iota
	stOK
	stErr
)

// Model is the root Bubble Tea model.
type Model struct {
	ws      *spec.Workspace
	secrets *secrets.Store
	state   *spec.State
	env     map[string]string
	cwd     string

	items    []spec.Item
	rows     []listRow // the tree shown in the list pane
	cursor   int       // index into rows
	listOff  int
	filter   string
	prevFltr string

	focus focus
	mode  mode
	tab   tab
	help  bool

	pager      pager
	contentKey string
	input      textinput.Model
	history    map[mode][]string
	histIdx    int

	jqFilter string
	prevJQ   string
	jqErr    string

	runs    map[string]*runState
	prompt  *promptState
	confirm func() tea.Cmd

	pending string
	status  string
	stKind  statusKind

	width, height int
	hideHints     bool
}

// New builds the model for a workspace.
func New(ws *spec.Workspace, store *secrets.Store) *Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 0
	cwd, _ := os.Getwd()
	m := &Model{
		ws:      ws,
		secrets: store,
		state:   ws.LoadState(),
		cwd:     cwd,
		input:   ti,
		history: map[mode][]string{},
		runs:    map[string]*runState{},
		tab:     tabRequest,
	}
	m.loadEnv()
	m.reload()
	return m
}

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) loadEnv() {
	env, err := m.ws.LoadEnv(m.state.Env)
	if err != nil {
		m.setErr(err.Error())
		env = map[string]string{}
	}
	m.env = env
}

func (m *Model) scope() *vars.Scope {
	return vars.NewScope(
		vars.Layer{Name: "state", Vars: m.state.Vars},
		vars.Layer{Name: "env:" + m.state.Env, Vars: m.env},
	)
}

func (m *Model) saveState() {
	if err := m.ws.SaveState(m.state); err != nil {
		m.setErr("saving state: " + err.Error())
	}
}

// reload re-reads the workspace, keeping the selection.
func (m *Model) reload() {
	key := m.selectedKey()
	m.items = m.ws.Items()
	m.buildRows()
	m.selectKey(key)
	m.loadEnv()
	m.refresh(false)
}

func (m *Model) applyFilter() { m.rebuild() }

// fuzzy matches the query as a subsequence (smartcase).
func fuzzy(s, q string) bool {
	if smartcase(q) {
		s = strings.ToLower(s)
	}
	qi := 0
	qr := []rune(q)
	for _, r := range s {
		if qi < len(qr) && r == qr[qi] {
			qi++
		}
	}
	return qi == len(qr)
}

func (m *Model) setInfo(s string) { m.status, m.stKind = s, stInfo }
func (m *Model) setOK(s string)   { m.status, m.stKind = s, stOK }
func (m *Model) setErr(s string)  { m.status, m.stKind = s, stErr }

func (m *Model) enterInput(md mode, prompt, value string) {
	m.mode = md
	m.input.Prompt = prompt
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.Focus()
	m.histIdx = len(m.history[md])
}

func (m *Model) leaveInput() {
	m.mode = modeNormal
	m.input.Blur()
	m.input.SetValue("")
}

type editorDoneMsg struct {
	err     error
	select_ string
	editor  string
	path    string
	changed bool
}

func editorCmd() []string {
	for _, v := range []string{"VISUAL", "EDITOR"} {
		if e := strings.TrimSpace(os.Getenv(v)); e != "" {
			return strings.Fields(e)
		}
	}
	return []string{"vi"}
}

// edit opens path in $EDITOR, then reloads and selects selectKey.
func (m *Model) edit(path, selectKey string) tea.Cmd {
	e := editorCmd()
	before, _ := os.ReadFile(path)
	c := exec.Command(e[0], append(e[1:], path)...)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		after, _ := os.ReadFile(path)
		return editorDoneMsg{err: err, select_: selectKey, editor: e[0], path: path, changed: string(before) != string(after)}
	})
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.refresh(false)
		return m, nil
	case runEventMsg:
		return m, m.handleEvent(msg)
	case tickMsg:
		if m.anyRunning() {
			return m, tick()
		}
		return m, nil
	case secretsDoneMsg:
		if msg.err != nil {
			m.setErr(msg.err.Error())
		} else {
			m.setOK(msg.what)
		}
		m.refresh(false)
		return m, nil
	case editorDoneMsg:
		m.reload()
		switch {
		case msg.err != nil:
			m.setErr("editor " + msg.editor + ": " + msg.err.Error() + " (set $EDITOR)")
		case msg.changed:
			m.setOK("saved " + shortPath(msg.path))
		default:
			m.setInfo("no changes (" + msg.editor + ")")
		}
		if msg.select_ != "" {
			m.selectKey(msg.select_)
			m.refresh(false)
		}
		if it, ok := m.selected(); ok && it.Err != nil {
			m.setErr(it.Err.Error())
		}
		return m, nil
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	if m.mode != modeNormal && m.mode != modeConfirm {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) layout() {
	_, rw := m.paneWidths()
	// Right pane: border (2) + tabs line (1).
	// Right pane: border (2) + tabs line (1).
	m.pager.SetSize(max(1, rw-4), max(1, m.mainHeight()-2-1))
	m.input.Width = max(10, m.width-14)
}

func (m *Model) paneWidths() (int, int) {
	lw := m.width * 30 / 100
	lw = min(max(lw, 26), 46)
	if lw > m.width/2 {
		lw = m.width / 2
	}
	return lw, m.width - lw
}

func (m *Model) listHeight() int { return max(1, m.mainHeight()-2) }

// showHints reports whether the key hints bar fits on screen.
func (m *Model) showHints() bool { return !m.hideHints && m.height >= 12 }

// mainHeight is the height of the two panes: everything but the header,
// the status line and the hints bar.
func (m *Model) mainHeight() int {
	h := m.height - 2
	if m.showHints() {
		h--
	}
	return h
}

func (m *Model) handleKey(k tea.KeyMsg) tea.Cmd {
	switch m.mode {
	case modeNormal:
		return m.normalKey(k)
	case modeConfirm:
		fn := m.confirm
		m.confirm = nil
		m.mode = modeNormal
		if k.String() == "y" || k.String() == "Y" {
			return fn()
		}
		m.setInfo("cancelled")
		return nil
	}
	return m.inputKey(k)
}

func (m *Model) inputKey(k tea.KeyMsg) tea.Cmd {
	md := m.mode
	switch k.String() {
	case "esc", "ctrl+c", "ctrl+[":
		switch md {
		case modeJQ:
			m.jqFilter = m.prevJQ
			m.jqErr = ""
			m.refresh(false)
		case modeSearch:
			if m.focus == focusList {
				m.filter = m.prevFltr
				m.applyFilter()
				m.refresh(false)
			} else {
				m.pager.ClearSearch()
			}
		case modeInput:
			m.prompt = nil
			m.setInfo("run cancelled")
		}
		m.leaveInput()
		return nil
	case "enter":
		v := m.input.Value()
		if v != "" && md != modeInput {
			h := m.history[md]
			if len(h) == 0 || h[len(h)-1] != v {
				m.history[md] = append(h, v)
			}
		}
		switch md {
		case modeCommand:
			m.leaveInput()
			return m.runCommand(v)
		case modeSearch:
			m.leaveInput()
			if m.focus == focusList {
				m.filter = v
				m.applyFilter()
				m.refresh(false)
				if len(m.rows) == 0 {
					m.setErr("no match: " + v)
				}
			} else if n := m.pager.Search(v); n == 0 && v != "" {
				m.setErr("pattern not found: " + v)
			} else if v != "" {
				m.setInfo("/" + v + "  " + plural(n, "match", "matches"))
			}
			return nil
		case modeJQ:
			m.leaveInput()
			m.setJQ(v)
			return nil
		case modeInsert:
			m.input.SetValue("")
			m.histIdx = len(m.history[md])
			return m.sendWS(v)
		case modeInput:
			return m.submitPrompt(v)
		}
	case "up", "ctrl+p":
		if h := m.history[md]; len(h) > 0 && m.histIdx > 0 {
			m.histIdx--
			m.input.SetValue(h[m.histIdx])
			m.input.CursorEnd()
			m.liveUpdate()
		}
		return nil
	case "down", "ctrl+n":
		if h := m.history[md]; m.histIdx < len(h) {
			m.histIdx++
			if m.histIdx == len(h) {
				m.input.SetValue("")
			} else {
				m.input.SetValue(h[m.histIdx])
			}
			m.input.CursorEnd()
			m.liveUpdate()
		}
		return nil
	case "tab":
		if md == modeCommand {
			m.complete()
			return nil
		}
	case "backspace":
		if m.input.Value() == "" && md != modeInput {
			// Like vim: backspace on an empty command line leaves it.
			return m.inputKey(tea.KeyMsg{Type: tea.KeyEsc})
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	m.liveUpdate()
	return cmd
}

// liveUpdate applies search, filter and jq as the user types.
func (m *Model) liveUpdate() {
	v := m.input.Value()
	switch m.mode {
	case modeSearch:
		if m.focus == focusList {
			m.filter = v
			m.applyFilter()
			m.refresh(false)
		} else {
			m.pager.Search(v)
		}
	case modeJQ:
		m.jqFilter = strings.TrimSpace(v)
		m.refresh(false)
	}
}

func (m *Model) setJQ(f string) {
	m.jqFilter = strings.TrimSpace(f)
	if m.jqFilter == "." {
		m.jqFilter = ""
	}
	if m.tab != tabResponse && m.jqFilter != "" {
		m.tab = tabResponse
	}
	m.refresh(false)
	if m.jqErr != "" {
		m.setErr("jq: " + m.jqErr)
	} else if m.jqFilter != "" {
		m.setInfo("jq: " + m.jqFilter + "   (esc to clear)")
	}
}

func (m *Model) sendWS(text string) tea.Cmd {
	rs := m.runs[m.selectedKey()]
	if rs == nil || rs.input == nil || !rs.running {
		m.leaveInput()
		m.setErr("no open WebSocket for this request — press enter to connect")
		return nil
	}
	msg, err := m.scope().Render(text)
	if err != nil {
		m.setErr(err.Error())
		return nil
	}
	select {
	case rs.input <- msg:
	default:
		m.setErr("send queue full")
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return p
}

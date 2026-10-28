package tui

import (
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jcverdier/turl/internal/curl"
	"github.com/jcverdier/turl/internal/runner"
	"github.com/jcverdier/turl/internal/spec"
)

// Keys starting a two-key sequence (gg, dd, yy, cw, ctrl+w h, ...).
var prefixKeys = map[string]bool{"g": true, "d": true, "y": true, "c": true, "z": true, "ctrl+w": true}

func (m *Model) normalKey(k tea.KeyMsg) tea.Cmd {
	s := k.String()
	if m.pending != "" {
		seq := m.pending + " " + s
		m.pending = ""
		return m.sequence(seq)
	}
	if prefixKeys[s] {
		m.pending = s
		return nil
	}
	if m.help {
		switch s {
		case "q", "esc", "?":
			m.help = false
			m.refresh(false)
			return nil
		}
	}
	it, hasItem := m.selected()
	switch s {
	// Panes and tabs.
	case "tab", "ctrl+i":
		m.toggleFocus()
	case "ctrl+h", "left":
		m.focus = focusList
	case "ctrl+l", "right":
		m.focus = focusPager
	case "h":
		if m.focus == focusPager {
			m.focus = focusList
		} else {
			m.listLeft()
		}
	case "l":
		if m.focus == focusList {
			m.listRight()
		}
	case "1", "2", "3", "4", "5":
		m.setTab(tab(s[0] - '1'))
	case "]", "L":
		m.setTab((m.tab + 1) % numTabs)
	case "[", "H":
		m.setTab((m.tab + numTabs - 1) % numTabs)

	// Movement.
	case "j", "down", "ctrl+n":
		m.move(1)
	case "k", "up", "ctrl+p":
		m.move(-1)
	case "ctrl+d":
		m.move(m.half())
	case "ctrl+u":
		m.move(-m.half())
	case "ctrl+f", "pgdown", " ":
		m.move(m.page())
	case "ctrl+b", "pgup":
		m.move(-m.page())
	case "ctrl+e":
		m.pager.Scroll(1)
	case "ctrl+y":
		m.pager.Scroll(-1)
	case "G", "end":
		m.top(false)
	case "home":
		m.top(true)
	case "{":
		m.jumpCollection(-1)
	case "}":
		m.jumpCollection(1)

	// Search and jq.
	case "/":
		if m.focus == focusList {
			m.prevFltr = m.filter
			m.enterInput(modeSearch, "/", m.filter)
		} else {
			m.enterInput(modeSearch, "/", "")
		}
	case "n", "N":
		dir := 1
		if s == "N" {
			dir = -1
		}
		if m.focus == focusList {
			m.move(dir)
		} else if i, n := m.pager.Next(dir); n > 0 {
			m.setInfo("/" + m.pager.query + "  [" + strconv.Itoa(i) + "/" + strconv.Itoa(n) + "]")
		} else if m.pager.query != "" {
			m.setErr("pattern not found: " + m.pager.query)
		}
	case "|":
		m.prevJQ = m.jqFilter
		if m.tab != tabResponse {
			m.setTab(tabResponse)
		}
		m.enterInput(modeJQ, "jq ", m.jqFilter)
	case ":":
		m.enterInput(modeCommand, ":", "")
	case "esc":
		switch {
		case m.pager.query != "":
			m.pager.ClearSearch()
		case m.jqFilter != "":
			m.jqFilter, m.jqErr = "", ""
			m.refresh(false)
		case m.filter != "":
			key := m.selectedKey()
			m.filter = ""
			m.applyFilter()
			m.selectKey(key)
			m.refresh(false)
		}
		m.setInfo("")
	case "?":
		m.help = !m.help
		m.focus = focusPager
		m.refresh(false)

	// Actions.
	case "enter", "r":
		if c, ok := m.onCollection(); ok {
			if s == "enter" {
				m.setFolded(c, !m.folded(c))
			}
			return nil
		}
		if hasItem {
			return m.requestRun(it, false)
		}
	case "R":
		if hasItem {
			return m.requestRun(it, true)
		}
	case "x":
		if !m.stopRun(m.selectedKey()) {
			m.setInfo("nothing running")
		}
	case "ctrl+c":
		if m.stopRun(m.selectedKey()) {
			return nil
		}
		return m.quit()
	case "q":
		return m.quit()
	case "i":
		rs := m.runs[m.selectedKey()]
		if rs == nil || rs.input == nil || !rs.running {
			m.setErr("no open WebSocket — select a ws request and press enter")
			return nil
		}
		m.tab = tabResponse
		m.refresh(true)
		m.enterInput(modeInsert, "ws> ", "")
	case "a":
		m.enterInput(modeCommand, ":", "new http "+m.newPrefix())
	case "o":
		m.enterInput(modeCommand, ":", "new ws "+m.newPrefix())
	case "C":
		m.enterInput(modeCommand, ":", "collection new ")
	case "m":
		if hasItem && it.Kind == spec.KindRequest {
			m.enterInput(modeCommand, ":", "move ")
			m.setInfo("collections: " + strings.Join(m.collectionChoices(), ", "))
		}
	case "A":
		m.enterInput(modeCommand, ":", "new flow ")
	case "e":
		if c, ok := m.onCollection(); ok {
			return m.editCollection(c)
		}
		if hasItem {
			return m.edit(it.Path, itemKey(it))
		}
	case "E":
		m.cycleEnv()
	case "D":
		if hasItem {
			m.enterInput(modeCommand, ":", "dup "+it.Name+"-copy")
		}
	case "p":
		m.enterInput(modeCommand, ":", "import ")
		m.setInfo("paste a curl command into the clipboard, then name the request")
	case "ctrl+r":
		m.reload()
		m.setInfo("reloaded " + shortPath(m.ws.Dir))
	}
	return nil
}

func (m *Model) sequence(seq string) tea.Cmd {
	it, hasItem := m.selected()
	switch seq {
	case "g g":
		m.top(true)
	case "g t":
		m.setTab((m.tab + 1) % numTabs)
	case "g T":
		m.setTab((m.tab + numTabs - 1) % numTabs)
	case "g e":
		if m.state.Env != "" {
			return m.edit(m.ws.EnvPath(m.state.Env), "")
		}
	case "ctrl+w h", "ctrl+w ctrl+h":
		m.focus = focusList
	case "ctrl+w l", "ctrl+w ctrl+l":
		m.focus = focusPager
	case "ctrl+w w", "ctrl+w ctrl+w":
		m.toggleFocus()
	case "z a", "z o", "z c":
		if c, ok := m.selectedCollection(); ok {
			fold := seq == "z c" || (seq == "z a" && !m.folded(c))
			m.selectKey(collKey(c))
			m.setFolded(c, fold)
		}
	case "z M":
		m.foldAll(true)
	case "z R":
		m.foldAll(false)
	case "d d":
		if c, ok := m.onCollection(); ok {
			m.confirmDeleteCollection(c)
			return nil
		}
		if hasItem {
			m.mode = modeConfirm
			m.status = "delete " + kindName(it) + " " + it.Name + "? (y/N)"
			m.confirm = func() tea.Cmd { return m.deleteItem(it) }
		}
	case "c w", "c c":
		if c, ok := m.onCollection(); ok {
			if c == "" {
				m.setErr("the " + spec.DefaultCollectionName + " cannot be renamed")
				return nil
			}
			m.enterInput(modeCommand, ":", "collection rename "+c)
			return nil
		}
		if hasItem {
			m.enterInput(modeCommand, ":", "rename "+it.Name)
		}
	case "y y":
		if m.focus == focusPager {
			m.yank(m.pager.Plain(), "view")
		} else {
			m.yankCurl()
		}
	case "y c":
		m.yankCurl()
	case "y b":
		if rs := m.runs[m.selectedKey()]; rs != nil && rs.result != nil {
			m.yank(string(rs.result.Body), "response body")
		} else {
			m.setErr("no response")
		}
	case "y u":
		if hasItem && it.Request != nil {
			p, _ := runner.PrepareIn(m.ws, it.Request, m.scope())
			m.yank(p.URL, "url")
		}
	case "y n":
		if c, ok := m.onCollection(); ok {
			m.yank(spec.CollectionLabel(c), "collection name")
		} else if hasItem {
			m.yank(it.Name, "name")
		}
	case "y p":
		if hasItem {
			m.yank(it.Path, "path")
		}
	}
	return nil
}

func kindName(it spec.Item) string {
	if it.Kind == spec.KindFlow {
		return "flow"
	}
	return "request"
}

func (m *Model) quit() tea.Cmd {
	for _, rs := range m.runs {
		if rs.running {
			rs.cancel()
		}
	}
	return tea.Quit
}

func (m *Model) toggleFocus() {
	if m.focus == focusList {
		m.focus = focusPager
	} else {
		m.focus = focusList
	}
}

func (m *Model) setTab(t tab) {
	m.help = false
	m.tab = t
	m.refresh(false)
}

func (m *Model) half() int {
	if m.focus == focusList {
		return max(1, m.listHeight()/2)
	}
	return m.pager.Half()
}

func (m *Model) page() int {
	if m.focus == focusList {
		return max(1, m.listHeight()-2)
	}
	return m.pager.Page()
}

func (m *Model) move(n int) {
	if m.focus == focusPager {
		m.pager.Scroll(n)
		return
	}
	m.moveList(n)
}

func (m *Model) top(top bool) {
	if m.focus == focusPager {
		if top {
			m.pager.Top()
		} else {
			m.pager.Bottom()
		}
		return
	}
	m.listEdge(top)
}

func (m *Model) cycleEnv() {
	envs := m.ws.Envs()
	if len(envs) == 0 {
		m.setErr("no environments — :new env <name>")
		return
	}
	next := envs[0]
	for i, e := range envs {
		if e == m.state.Env {
			next = envs[(i+1)%len(envs)]
		}
	}
	m.switchEnv(next)
}

func (m *Model) switchEnv(name string) {
	if _, err := os.Stat(m.ws.EnvPath(name)); err != nil {
		m.setErr("no env " + name)
		return
	}
	m.state.Env = name
	m.saveState()
	m.loadEnv()
	m.refresh(false)
	m.setOK("env: " + name)
}

func (m *Model) yank(s, what string) {
	if err := copyClipboard(s); err != nil {
		m.setErr("clipboard: " + err.Error())
		return
	}
	m.setOK("yanked " + what + " (" + humanSize(len(s)) + ")")
}

func (m *Model) yankCurl() {
	it, ok := m.selected()
	if !ok || it.Request == nil {
		m.setErr("select a request to yank as curl")
		return
	}
	p, err := runner.PrepareIn(m.ws, it.Request, m.scope())
	cmd := curl.Export(p)
	if err := copyClipboard(cmd); err != nil {
		m.setErr("clipboard: " + err.Error())
		return
	}
	if err != nil {
		m.setInfo("yanked curl (with unresolved vars: " + err.Error() + ")")
		return
	}
	what := "curl command"
	if it.Request.IsWS() {
		what = "websocat command"
	}
	m.setOK("yanked " + what)
}

func (m *Model) deleteItem(it spec.Item) tea.Cmd {
	if err := os.Remove(it.Path); err != nil {
		m.setErr(err.Error())
		return nil
	}
	delete(m.runs, itemKey(it))
	m.reload()
	m.setOK("deleted " + shortPath(it.Path))
	return nil
}

// newPrefix places new requests in the collection under the cursor.
func (m *Model) newPrefix() string {
	if c, ok := m.selectedCollection(); ok && c != "" {
		return c + "/"
	}
	return ""
}

func (m *Model) collectionChoices() []string {
	return append(m.ws.Collections(), "default")
}

func (m *Model) editCollection(c string) tea.Cmd {
	path, err := m.ws.EnsureCollectionFile(c)
	if err != nil {
		m.setErr(err.Error())
		return nil
	}
	return m.edit(path, collKey(c))
}

func (m *Model) confirmDeleteCollection(c string) {
	if c == "" {
		m.setErr("the " + spec.DefaultCollectionName + " cannot be deleted; delete its requests with dd")
		return
	}
	n := len(m.ws.RequestsIn(c))
	m.mode = modeConfirm
	m.status = "delete collection " + c + " and its " + plural(n, "request", "requests") + "? (y/N)"
	m.confirm = func() tea.Cmd {
		if err := m.ws.DeleteCollection(c); err != nil {
			m.setErr(err.Error())
			return nil
		}
		m.setFolded(c, false)
		m.reload()
		m.setOK("deleted collection " + c)
		return nil
	}
}

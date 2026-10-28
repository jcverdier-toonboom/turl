package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jcverdier/turl/internal/curl"
	"github.com/jcverdier/turl/internal/secrets"
	"github.com/jcverdier/turl/internal/spec"
)

var commandNames = []string{
	"quit", "edit", "new", "rename", "dup", "delete", "import", "curl", "env", "set", "unset",
	"vars", "write", "run", "rerun", "collection", "move", "mkcol", "secret", "stop", "close", "send", "jq", "reload", "nohlsearch", "help", "filter", "hints",
}

// aliases maps vim-style abbreviations to commands.
var aliases = map[string]string{
	"q": "quit", "q!": "quit", "qa": "quit", "qa!": "quit", "wq": "quit", "x": "quit",
	"e": "edit", "mv": "rename", "cp": "dup", "rm": "delete", "del": "delete",
	"w": "write", "w!": "write", "noh": "nohlsearch", "h": "help", "r": "run",
	"col": "collection", "coll": "collection", "secrets": "secret",
}

func (m *Model) runCommand(line string) tea.Cmd {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	if a, ok := aliases[name]; ok {
		name = a
	} else {
		// Accept any unambiguous prefix (:ren, :imp...).
		var hits []string
		for _, c := range commandNames {
			if strings.HasPrefix(c, name) {
				hits = append(hits, c)
			}
		}
		if len(hits) == 1 {
			name = hits[0]
		}
	}
	it, hasItem := m.selected()
	switch name {
	case "quit":
		return m.quit()
	case "edit":
		switch {
		case arg == "env":
			if m.state.Env == "" {
				m.setErr("no active env")
				return nil
			}
			return m.edit(m.ws.EnvPath(m.state.Env), "")
		case arg == "state":
			return m.edit(m.ws.StatePath(), "")
		case arg == "secrets":
			return m.secretCommand("edit")
		case arg != "":
			found, err := m.ws.Lookup(arg)
			if err != nil {
				m.setErr(err.Error())
				return nil
			}
			return m.edit(found.Path, itemKey(found))
		case hasItem:
			return m.edit(it.Path, itemKey(it))
		}
	case "new":
		kind, n, _ := strings.Cut(arg, " ")
		n = strings.TrimSpace(n)
		if n == "" {
			m.setErr("usage: :new http|ws|flow|env <name>")
			return nil
		}
		path, err := m.ws.NewItem(kind, n)
		if err != nil {
			m.setErr(err.Error())
			return nil
		}
		if kind == "env" {
			m.state.Env = n
			m.saveState()
			return m.edit(path, "")
		}
		key := "req:" + n
		if kind == "flow" {
			key = "flow:" + n
		}
		m.filter = ""
		return m.edit(path, key)
	case "rename":
		if !hasItem {
			return nil
		}
		if arg == "" || arg == it.Name {
			m.setErr("usage: :rename <new-name>")
			return nil
		}
		if err := m.ws.Rename(it, arg); err != nil {
			m.setErr(err.Error())
			return nil
		}
		key := strings.SplitN(itemKey(it), ":", 2)[0] + ":" + arg
		if rs, ok := m.runs[itemKey(it)]; ok {
			delete(m.runs, itemKey(it))
			m.runs[key] = rs
		}
		m.reload()
		m.selectKey(key)
		m.refresh(false)
		m.setOK("renamed to " + arg)
	case "secret":
		return m.secretCommand(arg)
	case "mkcol":
		return m.runCommand("collection new " + arg)
	case "collection":
		return m.collectionCommand(arg)
	case "move":
		if !hasItem || it.Kind != spec.KindRequest {
			m.setErr("select a request to move")
			return nil
		}
		newName, err := m.ws.MoveRequest(it, arg)
		if err != nil {
			m.setErr(err.Error())
			return nil
		}
		if rs, ok := m.runs[itemKey(it)]; ok {
			delete(m.runs, itemKey(it))
			m.runs["req:"+newName] = rs
		}
		c, _ := spec.SplitCollection(newName)
		m.setFolded(c, false)
		m.reload()
		m.selectKey("req:" + newName)
		m.refresh(false)
		m.setOK("moved to " + spec.CollectionLabel(c))
	case "dup":
		if !hasItem || arg == "" {
			m.setErr("usage: :dup <new-name>")
			return nil
		}
		if err := m.ws.Duplicate(it, arg); err != nil {
			m.setErr(err.Error())
			return nil
		}
		key := strings.SplitN(itemKey(it), ":", 2)[0] + ":" + arg
		m.reload()
		m.selectKey(key)
		m.refresh(false)
		m.setOK("duplicated as " + arg)
	case "delete":
		if hasItem {
			return m.sequence("d d")
		}
	case "import":
		if arg == "" {
			m.setErr("usage: :import <name>   (reads a curl command from the clipboard)")
			return nil
		}
		if err := spec.ValidName(arg); err != nil {
			m.setErr(err.Error())
			return nil
		}
		src, err := pasteClipboard()
		if err != nil {
			m.setErr(err.Error())
			return nil
		}
		req, err := curl.Import(src)
		if err != nil {
			m.setErr("import: " + err.Error())
			return nil
		}
		req.Name = arg
		if _, err := os.Stat(m.ws.RequestPath(arg)); err == nil {
			m.setErr(arg + " already exists")
			return nil
		}
		if err := m.ws.SaveRequest(req); err != nil {
			m.setErr(err.Error())
			return nil
		}
		m.filter = ""
		m.reload()
		m.selectKey("req:" + arg)
		m.tab = tabRequest
		m.refresh(false)
		m.setOK("imported " + arg + " — e to edit")
	case "curl":
		m.yankCurl()
	case "env":
		if arg == "" {
			envs := m.ws.Envs()
			if len(envs) == 0 {
				m.setInfo("no environments — :new env <name>")
			} else {
				m.setInfo("envs: " + strings.Join(envs, ", ") + "   (active: " + m.state.Env + ")")
			}
			return nil
		}
		m.switchEnv(arg)
	case "set":
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			k, v, ok = strings.Cut(arg, " ")
		}
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			m.setErr("usage: :set name=value")
			return nil
		}
		m.state.Vars[k] = strings.TrimSpace(v)
		m.saveState()
		m.refresh(false)
		m.setOK("set " + k)
	case "unset":
		for _, k := range strings.Fields(arg) {
			delete(m.state.Vars, k)
		}
		m.saveState()
		m.refresh(false)
		m.setOK("unset " + arg)
	case "vars":
		m.setTab(tabVars)
	case "write":
		if arg == "" {
			m.setErr("usage: :w <file>   (writes the current view, e.g. a jq result)")
			return nil
		}
		path := arg
		if strings.HasPrefix(path, "~/") {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, path[2:])
		}
		data := m.pager.Plain() + "\n"
		// Unfiltered response: write the exact body bytes.
		if rs := m.runs[m.selectedKey()]; m.tab == tabResponse && m.jqFilter == "" && rs != nil && rs.result != nil && !rs.result.WS {
			data = string(rs.result.Body)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			m.setErr(err.Error())
			return nil
		}
		m.setOK("written " + path + " (" + humanSize(len(data)) + ")")
	case "run":
		if arg != "" {
			found, err := m.ws.Lookup(arg)
			if err != nil {
				m.setErr(err.Error())
				return nil
			}
			m.selectKey(itemKey(found))
			return m.requestRun(found, false)
		}
		if hasItem {
			return m.requestRun(it, false)
		}
	case "rerun":
		if hasItem {
			return m.requestRun(it, true)
		}
	case "stop", "close":
		if !m.stopRun(m.selectedKey()) {
			m.setInfo("nothing running")
		}
	case "send":
		return m.sendWS(arg)
	case "jq":
		m.setJQ(arg)
	case "filter":
		m.filter = arg
		m.applyFilter()
		m.refresh(false)
	case "reload":
		m.reload()
		m.setOK("reloaded")
	case "hints":
		m.hideHints = !m.hideHints
		m.layout()
		m.refresh(false)
	case "nohlsearch":
		m.pager.ClearSearch()
	case "help":
		m.help = true
		m.focus = focusPager
		m.refresh(false)
	default:
		m.setErr("not a command: " + name)
	}
	return nil
}

// complete performs tab completion on the command line: command names,
// then item names, env names or kinds depending on the command.
func (m *Model) complete() {
	v := m.input.Value()
	fields := strings.Fields(v)
	trailing := strings.HasSuffix(v, " ")
	var cands []string
	var prefix, word string
	switch {
	case len(fields) == 0 || (len(fields) == 1 && !trailing):
		if len(fields) == 1 {
			word = fields[0]
		}
		cands = commandNames
	default:
		cmd := fields[0]
		if a, ok := aliases[cmd]; ok {
			cmd = a
		}
		if !trailing {
			word = fields[len(fields)-1]
			prefix = strings.TrimSuffix(v, word)
		} else {
			prefix = v
		}
		argIdx := len(fields) - 1
		if trailing {
			argIdx++
		}
		switch {
		case cmd == "env":
			cands = m.ws.Envs()
		case cmd == "new" && argIdx == 1:
			cands = []string{"http", "ws", "flow", "env"}
		case cmd == "edit":
			cands = append([]string{"env", "state", "secrets"}, m.itemNames()...)
		case cmd == "secret" && argIdx == 1:
			cands = []string{"list", "refresh", "clear", "edit"}
		case cmd == "secret":
			if infos, err := m.secrets.List(); err == nil {
				for _, in := range infos {
					cands = append(cands, in.Name)
				}
			}
		case cmd == "move" || (cmd == "collection" && argIdx >= 2):
			cands = m.collectionChoices()
		case cmd == "collection":
			cands = []string{"new", "rename", "delete", "edit", "list"}
		case cmd == "run" || cmd == "rename" || cmd == "dup":
			cands = m.itemNames()
		case cmd == "set" || cmd == "unset":
			for _, e := range m.scope().Entries() {
				cands = append(cands, e.Name)
			}
		}
	}
	var hits []string
	seen := map[string]bool{}
	for _, c := range cands {
		if strings.HasPrefix(c, word) && !seen[c] {
			seen[c] = true
			hits = append(hits, c)
		}
	}
	if len(hits) == 0 {
		return
	}
	if len(hits) == 1 {
		m.input.SetValue(prefix + hits[0] + " ")
		m.input.CursorEnd()
		return
	}
	lcp := hits[0]
	for _, h := range hits[1:] {
		for !strings.HasPrefix(h, lcp) {
			lcp = lcp[:len(lcp)-1]
		}
	}
	m.input.SetValue(prefix + lcp)
	m.input.CursorEnd()
	m.setInfo(strings.Join(hits, "  "))
}

func (m *Model) itemNames() []string {
	var out []string
	for _, it := range m.items {
		out = append(out, it.Name)
	}
	return out
}

// collectionCommand handles :collection new|rename|delete|edit|list.
func (m *Model) collectionCommand(arg string) tea.Cmd {
	sub, rest, _ := strings.Cut(arg, " ")
	args := strings.Fields(rest)
	cur, hasCur := m.selectedCollection()
	switch sub {
	case "", "list", "ls":
		var names []string
		for _, c := range append(m.ws.Collections(), "") {
			names = append(names, fmt.Sprintf("%s (%d)", spec.CollectionLabel(c), len(m.ws.RequestsIn(c))))
		}
		m.setInfo("collections: " + strings.Join(names, ", "))
	case "new", "add":
		if len(args) != 1 {
			m.setErr("usage: :collection new <name>")
			return nil
		}
		if err := m.ws.NewCollection(args[0]); err != nil {
			m.setErr(err.Error())
			return nil
		}
		m.filter = ""
		m.reload()
		m.selectKey(collKey(args[0]))
		m.refresh(false)
		m.setOK("created collection " + args[0] + " — a to add a request, e for its settings")
	case "rename", "mv":
		from, to := cur, ""
		switch len(args) {
		case 1:
			to = args[0]
		case 2:
			from, to = args[0], args[1]
		default:
			m.setErr("usage: :collection rename [old] <new>")
			return nil
		}
		if len(args) == 1 && !hasCur {
			m.setErr("select a collection")
			return nil
		}
		flows, err := m.ws.RenameCollection(from, to)
		if err != nil {
			m.setErr(err.Error())
			return nil
		}
		if m.folded(from) {
			m.setFolded(from, false)
			m.setFolded(to, true)
		}
		for key, rs := range m.runs {
			if c, short := spec.SplitCollection(strings.TrimPrefix(key, "req:")); strings.HasPrefix(key, "req:") && c == from {
				delete(m.runs, key)
				m.runs["req:"+spec.JoinCollection(to, short)] = rs
			}
		}
		m.reload()
		m.selectKey(collKey(to))
		m.refresh(false)
		msg := "renamed collection to " + to
		if flows > 0 {
			msg += fmt.Sprintf(" (updated %s)", plural(flows, "flow", "flows"))
		}
		m.setOK(msg)
	case "delete", "rm":
		c := cur
		if len(args) == 1 {
			c = args[0]
		} else if !hasCur {
			m.setErr("usage: :collection delete <name>")
			return nil
		}
		m.confirmDeleteCollection(c)
	case "edit", "e":
		c := cur
		if len(args) == 1 {
			c = args[0]
		} else if !hasCur {
			m.setErr("usage: :collection edit <name>")
			return nil
		}
		return m.editCollection(c)
	default:
		m.setErr("usage: :collection new|rename|delete|edit|list")
	}
	return nil
}

type secretsDoneMsg struct {
	what string
	err  error
}

// secretCommand handles :secret list|refresh [names]|clear|edit.
func (m *Model) secretCommand(arg string) tea.Cmd {
	fields := strings.Fields(arg)
	sub := "list"
	if len(fields) > 0 {
		sub, fields = fields[0], fields[1:]
	}
	switch sub {
	case "list", "ls":
		m.setTab(tabVars)
	case "refresh":
		// Commands like aws can take seconds: run them off the UI loop.
		names := fields
		m.setInfo("fetching secrets…")
		return func() tea.Msg {
			what := "refreshed all secrets"
			if len(names) > 0 {
				what = "refreshed " + strings.Join(names, ", ")
			}
			return secretsDoneMsg{what: what, err: m.secrets.Refresh(names...)}
		}
	case "clear":
		if err := m.secrets.Clear(); err != nil {
			m.setErr(err.Error())
			return nil
		}
		m.refresh(false)
		m.setOK("cleared cached secrets")
	case "edit":
		if _, err := os.Stat(m.secrets.Path()); os.IsNotExist(err) {
			if err := os.WriteFile(m.secrets.Path(), []byte(secrets.Template), 0o644); err != nil {
				m.setErr(err.Error())
				return nil
			}
		}
		return m.edit(m.secrets.Path(), "")
	default:
		m.setErr("usage: :secret list|refresh [name…]|clear|edit")
	}
	return nil
}

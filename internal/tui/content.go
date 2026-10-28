package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/itchyny/gojq"

	"github.com/jcverdier/turl/internal/jq"
	"github.com/jcverdier/turl/internal/runner"
	"github.com/jcverdier/turl/internal/secrets"
	"github.com/jcverdier/turl/internal/spec"
)

// refresh recomputes the pager content for the selected item and tab.
func (m *Model) refresh(follow bool) {
	key := fmt.Sprintf("%s|%d|%v", m.selectedKey(), m.tab, m.help)
	// Never show a fetched secret, whatever echoes it (e.g. httpbin).
	content := secrets.Redact(m.content())
	if key != m.contentKey {
		m.contentKey = key
		m.pager.ClearSearch()
		m.pager.SetContent(content, false)
		m.pager.Top()
		if m.tab == tabLog || m.isLiveWS() {
			m.pager.Bottom()
		}
		return
	}
	m.pager.SetContent(content, follow)
}

func (m *Model) isLiveWS() bool {
	rs := m.runs[m.selectedKey()]
	return m.tab == tabResponse && rs != nil && rs.result != nil && rs.result.WS
}

func (m *Model) content() string {
	if m.help {
		return helpText()
	}
	if c, ok := m.onCollection(); ok && m.tab != tabVars {
		return m.collectionView(c)
	}
	it, ok := m.selected()
	if !ok {
		if len(m.items) == 0 {
			return sMuted.Render("Empty workspace " + shortPath(m.ws.Dir) + "\n\n" +
				"  a      new http request\n  o      new websocket request\n  A      new flow\n" +
				"  C      new collection\n  p      import a curl command from the clipboard\n  :new env dev   create an environment")
		}
		return sMuted.Render("No match for filter " + m.filter + " — esc to clear")
	}
	rs := m.runs[itemKey(it)]
	switch m.tab {
	case tabRequest:
		return m.requestView(it)
	case tabResponse:
		return m.responseView(rs)
	case tabHeaders:
		return headersView(rs)
	case tabLog:
		return logView(rs)
	case tabVars:
		return m.varsView()
	}
	return ""
}

func (m *Model) requestView(it spec.Item) string {
	var b strings.Builder
	src, _ := os.ReadFile(it.Path)
	if it.Err != nil {
		b.WriteString(sErr.Render("✗ "+it.Err.Error()) + "\n\n")
	}
	switch {
	case it.Request != nil:
		r := it.Request
		if r.Description != "" {
			b.WriteString(sMuted.Render(r.Description) + "\n\n")
		}
		p, err := runner.PrepareIn(m.ws, r, m.scope().Masked())
		b.WriteString(methodStyle(p.Method).Render(p.Method) + " " + p.URL + "\n")
		for _, h := range p.Headers {
			b.WriteString(sKey.Render(h.Key) + ": " + h.Value + "\n")
		}
		switch {
		case p.Body != "":
			b.WriteString("\n" + renderBody([]byte(p.Body)) + "\n")
		case p.BodyFile != "":
			b.WriteString("\n" + sMuted.Render("body from file "+p.BodyFile) + "\n")
		case len(p.Multipart) > 0:
			b.WriteString("\n")
			for _, kv := range p.Multipart {
				b.WriteString(sMuted.Render("form ") + kv.Key + "=" + kv.Value + "\n")
			}
		}
		for i, msg := range p.Messages {
			b.WriteString("\n" + sSent.Render(fmt.Sprintf("→ message %d", i+1)) + "\n" + renderBody([]byte(msg)) + "\n")
		}
		if err != nil {
			b.WriteString("\n" + sWarn.Render("⚠ "+err.Error()) + "\n")
		}
		if len(r.Inputs) > 0 {
			b.WriteString(sMuted.Render("prompts for: "+strings.Join(r.Inputs, ", ")) + "\n")
		}
	case it.Flow != nil:
		f := it.Flow
		if f.Description != "" {
			b.WriteString(sMuted.Render(strings.TrimSpace(f.Description)) + "\n\n")
		}
		for i, s := range f.Steps {
			line := fmt.Sprintf("%2d. %s", i+1, s.Label(i))
			var req *spec.Request
			if s.Inline != nil {
				req = s.Inline
			} else if s.Use != "" {
				r, err := m.ws.LoadRequest(s.Use)
				if err != nil {
					line += "  " + sErr.Render("✗ "+err.Error())
				}
				req = r
			}
			if req != nil {
				meth := req.MethodOrDefault()
				line += "  " + methodStyle(meth).Render(meth) + " " + sMuted.Render(req.URL)
			}
			if s.Skip {
				line = sMuted.Render(line + "  (skip)")
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\n" + sSection.Render("── "+shortPath(it.Path)+" ──") + "\n")
	b.WriteString(highlightYAML(strings.TrimRight(string(src), "\n")))
	return b.String()
}

func (m *Model) collectionView(c string) string {
	var b strings.Builder
	b.WriteString(sTitle.Render(spec.CollectionLabel(c)) + "\n")
	if c == "" {
		b.WriteString(sMuted.Render("Requests that are not in a collection.\nMove one with m (:move <collection>), create a collection with C.") + "\n")
	} else {
		col, err := m.ws.LoadCollection(c)
		if err != nil {
			b.WriteString(sErr.Render("✗ "+err.Error()) + "\n")
		}
		if col.Description != "" {
			b.WriteString(sMuted.Render(col.Description) + "\n")
		}
		b.WriteString("\n" + sSection.Render("── settings · "+shortPath(m.ws.CollectionPath(c))+" ──") + "\n")
		if len(col.Vars) == 0 && len(col.Headers) == 0 {
			b.WriteString(sMuted.Render("none — press e to set shared vars (base_url…) and headers (auth…)") + "\n")
		}
		for _, k := range sortedKeys(col.Vars) {
			b.WriteString(sKey.Render(k) + " = " + col.Vars[k] + "\n")
		}
		for _, k := range sortedKeys(col.Headers) {
			b.WriteString(sKey.Render(k) + ": " + col.Headers[k] + "\n")
		}
	}
	names := m.ws.RequestsIn(c)
	b.WriteString("\n" + sSection.Render(fmt.Sprintf("── %s ──", plural(len(names), "request", "requests"))) + "\n")
	for _, n := range names {
		_, short := spec.SplitCollection(n)
		r, err := m.ws.LoadRequest(n)
		if err != nil {
			b.WriteString(sErr.Render("!     ") + short + "  " + sErr.Render(err.Error()) + "\n")
			continue
		}
		meth := r.MethodOrDefault()
		b.WriteString(methodStyle(meth).Render(fmt.Sprintf("%-6s", meth)) + " " + short + "  " + sMuted.Render(r.URL) + "\n")
	}
	if len(names) == 0 {
		b.WriteString(sMuted.Render("empty — press a to add a request here") + "\n")
	}
	return b.String()
}

func (m *Model) responseView(rs *runState) string {
	m.jqErr = ""
	if rs == nil {
		return sMuted.Render("No response yet — press enter to run.")
	}
	var b strings.Builder
	res := rs.result
	if rs.err != nil {
		b.WriteString(sErr.Render("✗ "+rs.err.Error()) + "\n")
	}
	if res == nil {
		if rs.running {
			b.WriteString(sMuted.Render("running… " + rs.elapsed().Round(time.Millisecond).String()))
		}
		return b.String()
	}
	var code *gojq.Code
	if m.jqFilter != "" {
		c, err := jq.Compile(m.jqFilter)
		if err != nil {
			m.jqErr = err.Error()
			b.WriteString(sErr.Render("jq: "+err.Error()) + "\n")
		} else {
			code = c
		}
	}
	if res.WS {
		return b.String() + m.wsView(rs, code)
	}
	meta := []string{statusStyle(res.StatusCode).Render(res.Status)}
	if res.Proto != "" {
		meta = append([]string{sMuted.Render(res.Proto)}, meta...)
	}
	meta = append(meta, sMuted.Render(res.Duration.Round(time.Millisecond).String()), sMuted.Render(humanSize(len(res.Body))))
	if ct := res.Headers.Get("Content-Type"); ct != "" {
		meta = append(meta, sMuted.Render(ct))
	}
	if res.Status == "" {
		meta = []string{sMuted.Render(res.Method + " " + res.URL)}
	}
	b.WriteString(strings.Join(meta, sMuted.Render(" · ")) + "\n")
	if m.jqFilter != "" {
		b.WriteString(sMuted.Render("jq "+m.jqFilter) + "\n")
	}
	b.WriteString("\n")
	if code != nil {
		vals, _ := jq.Decode(res.Body)
		out, err := runAll(code, vals, jqEnvFor(res))
		if err != nil {
			m.jqErr = err.Error()
			b.WriteString(sErr.Render("jq: "+err.Error()) + "\n")
		}
		b.WriteString(renderValues(out))
		return b.String()
	}
	b.WriteString(renderBody(res.Body))
	return b.String()
}

func (m *Model) wsView(rs *runState, code *gojq.Code) string {
	var b strings.Builder
	res := rs.result
	state := sOK.Render("● open")
	if !rs.running {
		state = sMuted.Render("○ closed")
	}
	recv := 0
	for _, msg := range rs.msgs {
		if msg.Dir == runner.Recv {
			recv++
		}
	}
	b.WriteString(fmt.Sprintf("%s %s %s\n", methodStyle("WS").Render("WS"), state,
		sMuted.Render(fmt.Sprintf("· %d sent · %d received · %s", len(rs.msgs)-recv, recv, rs.elapsed().Round(time.Second)))))
	b.WriteString(sMuted.Render(res.URL) + "\n")
	if code != nil {
		b.WriteString(sMuted.Render("jq "+m.jqFilter+"  (applied to each received message)") + "\n")
	}
	if rs.running && rs.input != nil {
		b.WriteString(sMuted.Render("i to send a message · x to close") + "\n")
	}
	for _, msg := range rs.msgs {
		if code != nil {
			if msg.Dir != runner.Recv {
				continue
			}
			out, err := runAll(code, []any{jq.DecodeOne(msg.Data)}, jq.Env{})
			if err != nil {
				m.jqErr = err.Error()
				continue
			}
			if len(out) > 0 {
				b.WriteString("\n" + renderValues(out) + "\n")
			}
			continue
		}
		ts := msg.Time.Format("15:04:05.000")
		if msg.Dir == runner.Sent {
			b.WriteString("\n" + sSent.Render("→ sent ") + sMuted.Render(ts) + "\n")
		} else {
			b.WriteString("\n" + sRecv.Render("← recv ") + sMuted.Render(ts) + "\n")
		}
		b.WriteString(renderBody(msg.Data) + "\n")
	}
	return b.String()
}

func runAll(code *gojq.Code, inputs []any, env jq.Env) ([]any, error) {
	var out []any
	for _, in := range inputs {
		vs, err := jq.Run(code, in, env)
		out = append(out, vs...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func jqEnvFor(res *runner.Result) jq.Env {
	env := jq.Env{Status: res.StatusCode, Headers: map[string]string{}}
	for k, v := range res.Headers {
		env.Headers[k] = strings.Join(v, ", ")
	}
	return env
}

// renderValues prints jq outputs like `jq -r`, with JSON colored.
func renderValues(vals []any) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		if s, ok := v.(string); ok {
			parts = append(parts, s)
			continue
		}
		raw := jq.Marshal(v, false)
		if s, ok := prettyJSON(raw); ok {
			parts = append(parts, s)
		} else {
			parts = append(parts, string(raw))
		}
	}
	if len(parts) == 0 {
		return sMuted.Render("(no output)")
	}
	return strings.Join(parts, "\n")
}

func headersView(rs *runState) string {
	if rs == nil || rs.result == nil {
		return sMuted.Render("No request sent yet.")
	}
	res := rs.result
	var b strings.Builder
	b.WriteString(sSection.Render("── request ──") + "\n")
	b.WriteString(methodStyle(res.Method).Render(res.Method) + " " + res.URL + "\n")
	for _, h := range res.ReqHeaders {
		b.WriteString(sKey.Render(h.Key) + ": " + h.Value + "\n")
	}
	if res.ReqBody != "" {
		b.WriteString("\n" + renderBody([]byte(res.ReqBody)) + "\n")
	}
	b.WriteString("\n" + sSection.Render("── response ──") + "\n")
	if res.Status == "" {
		b.WriteString(sMuted.Render("(none)"))
		return b.String()
	}
	b.WriteString(sMuted.Render(res.Proto+" ") + statusStyle(res.StatusCode).Render(res.Status) + "\n")
	keys := make([]string, 0, len(res.Headers))
	for k := range res.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range res.Headers[k] {
			b.WriteString(sKey.Render(k) + ": " + v + "\n")
		}
	}
	return b.String()
}

func logView(rs *runState) string {
	if rs == nil {
		return sMuted.Render("Nothing run yet.")
	}
	var b strings.Builder
	for _, l := range rs.log {
		ts := sMuted.Render(l.t.Format("15:04:05.000") + " ")
		text := l.text
		switch l.kind {
		case runner.EvStepStart:
			text = sTitle.Render(text)
		case runner.EvError:
			text = sErr.Render(text)
		case runner.EvExtract:
			text = sOK.Render("✓ ") + text
		case runner.EvWSSend:
			text = sSent.Render(text)
		case runner.EvWSRecv:
			text = sRecv.Render(text)
		case runner.EvLog, runner.EvWSClose, runner.EvWSOpen:
			text = sMuted.Render(text)
		}
		b.WriteString(ts + text + "\n")
	}
	if rs.running {
		b.WriteString(sWarn.Render("● running " + rs.elapsed().Round(time.Millisecond).String()))
	} else if rs.err == nil {
		b.WriteString(sOK.Render("✓ done in " + rs.elapsed().Round(time.Millisecond).String()))
	}
	return b.String()
}

func (m *Model) varsView() string {
	var b strings.Builder
	b.WriteString(sMuted.Render("env: "+m.state.Env+"   ·   :set k=v  :unset k  :e env  ge") + "\n\n")
	entries := m.scope().Entries()
	w := 0
	for _, e := range entries {
		w = max(w, len(e.Name))
	}
	for _, e := range entries {
		name := sKey.Render(fmt.Sprintf("%-*s", w, e.Name))
		val := e.Value
		src := sMuted.Render("  [" + e.Source + "]")
		if e.Shadowed {
			b.WriteString(sMuted.Render(fmt.Sprintf("%-*s %s  [%s, shadowed]", w, e.Name, val, e.Source)) + "\n")
			continue
		}
		b.WriteString(name + " " + val + src + "\n")
	}
	if len(entries) == 0 {
		b.WriteString(sMuted.Render("(no variables)") + "\n")
	}
	b.WriteString("\n" + sSection.Render("── secrets · "+shortPath(m.secrets.Path())+" ──") + "\n")
	infos, err := m.secrets.List()
	switch {
	case err != nil:
		b.WriteString(sErr.Render("✗ "+err.Error()) + "\n")
	case len(infos) == 0:
		b.WriteString(sMuted.Render("none — :secret edit to fetch API keys with commands (aws, op, vault…)") + "\n")
	}
	for _, in := range infos {
		state := sMuted.Render("not fetched")
		if in.Cached {
			state = sOK.Render("cached")
			if in.Age > 0 {
				state += sMuted.Render(fmt.Sprintf(" %s ago, ttl %s", in.Age.Round(time.Second), in.TTL))
			} else {
				state += sMuted.Render(" in memory")
			}
		}
		b.WriteString(sKey.Render("{{$secret."+in.Name+"}}") + "  " + state + "\n")
		if in.Description != "" {
			b.WriteString("    " + sMuted.Render(in.Description) + "\n")
		}
		b.WriteString("    " + sComment.Render("$ "+in.Command) + "\n")
	}
	b.WriteString(sMuted.Render(":secret refresh [name] · :secret clear · :secret edit") + "\n")
	b.WriteString("\n" + sSection.Render("── builtins ──") + "\n")
	b.WriteString(sMuted.Render("{{$secret.NAME}} {{$uuid}} {{$timestamp}} {{$timestamp_ms}} {{$isotime}} {{$randint}} {{$env.NAME}} {{$sh:command}}"))
	return b.String()
}

func helpText() string {
	sec := func(s string) string { return "\n" + sTitle.Render(s) + "\n" }
	row := func(k, d string) string { return "  " + sKey.Render(fmt.Sprintf("%-16s", k)) + d + "\n" }
	var b strings.Builder
	b.WriteString(sTitle.Render("turl — keys") + sMuted.Render("   (q / esc / ? to close · / to search this help)") + "\n")
	b.WriteString(sec("Navigation"))
	b.WriteString(row("j k ↓ ↑", "move / scroll"))
	b.WriteString(row("gg G", "top / bottom"))
	b.WriteString(row("ctrl-d ctrl-u", "half page down / up"))
	b.WriteString(row("ctrl-f ctrl-b", "page down / up"))
	b.WriteString(row("ctrl-e ctrl-y", "scroll response one line"))
	b.WriteString(row("{ }", "jump between flows and requests"))
	b.WriteString(row("h l  tab", "focus list / response pane"))
	b.WriteString(row("ctrl-w h/l/w", "window focus, vim style"))
	b.WriteString(row("1-5  [ ]  gt gT", "tabs: request response headers log vars"))
	b.WriteString(sec("Collections"))
	b.WriteString(row("enter  za", "fold / unfold the collection"))
	b.WriteString(row("zo zc  zR zM", "open / close, open all / close all"))
	b.WriteString(row("h l", "parent collection / fold · unfold / enter"))
	b.WriteString(row("C", "new collection"))
	b.WriteString(row("m", "move the request to another collection"))
	b.WriteString(row("e cw dd", "on a collection: settings / rename / delete"))
	b.WriteString(sec("Search & jq"))
	b.WriteString(row("/", "filter list (fuzzy) or search the response pane"))
	b.WriteString(row("n N", "next / previous match"))
	b.WriteString(row("|", "jq filter on the response (live, esc to cancel)"))
	b.WriteString(row("esc", "clear search, then jq filter, then list filter"))
	b.WriteString(sec("Requests"))
	b.WriteString(row("enter r", "run (prompts for inputs)"))
	b.WriteString(row("R", "rerun with the same inputs"))
	b.WriteString(row("x  ctrl-c", "stop run / close WebSocket"))
	b.WriteString(row("i", "insert mode: send messages on an open WebSocket"))
	b.WriteString(row("e", "edit in $EDITOR"))
	b.WriteString(row("a o A", "new http request / websocket / flow"))
	b.WriteString(row("D  cw  dd", "duplicate / rename / delete"))
	b.WriteString(row("p", "import a curl command from the clipboard"))
	b.WriteString(row("yy", "yank: curl command (list) or view (response pane)"))
	b.WriteString(row("yc yb yu yn yp", "yank curl / body / url / name / file path"))
	b.WriteString(row("E  ge", "next environment / edit environment"))
	b.WriteString(row("ctrl-r", "reload files from disk"))
	b.WriteString(row("q", "quit"))
	b.WriteString(sec("Commands"))
	b.WriteString(row(":new http|ws|flow|env <name>", ""))
	b.WriteString(row(":e [name|env|state]", "edit"))
	b.WriteString(row(":env [name]", "switch / list environments"))
	b.WriteString(row(":set k=v  :unset k", "manage variables (persisted in state.yaml)"))
	b.WriteString(row(":import <name>", "create a request from a curl command in the clipboard"))
	b.WriteString(row(":w <file>", "write the response (or jq result) to a file"))
	b.WriteString(row(":jq <filter>", "set the jq filter"))
	b.WriteString(row(":send <msg>", "send a WebSocket message"))
	b.WriteString(row(":run [name]  :rerun", "run"))
	b.WriteString(row(":rename :dup :delete", "manage items"))
	b.WriteString(row(":collection new|rename|delete|edit", "manage collections (:mkcol <name>)"))
	b.WriteString(row(":move <collection>", "move the request (\"default\" for the default collection)"))
	b.WriteString(row(":curl", "yank as curl"))
	b.WriteString(row(":hints", "toggle the key hints bar"))
	b.WriteString(row(":secret refresh|clear|edit", "fetch secrets again / drop the cache / edit secrets.yaml"))
	b.WriteString(row(":q", "quit"))
	b.WriteString(sMuted.Render("\n  tab completes command names and arguments · ↑ ↓ browse history\n"))
	b.WriteString(sec("Templates"))
	b.WriteString(row("{{name}}", "env, :set, extracted and input variables"))
	b.WriteString(row("{{$uuid}} …", "builtins: $timestamp $isotime $env.NAME $sh:cmd"))
	b.WriteString(row("{{$secret.NAME}}", "value printed by a command in secrets.yaml (cached, auto-refreshed on 401/403)"))
	b.WriteString(row("extract:", "var: <jq filter> — fed to later steps of a flow"))
	return b.String()
}

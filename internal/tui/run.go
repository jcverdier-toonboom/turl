package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jcverdier/turl/internal/runner"
	"github.com/jcverdier/turl/internal/spec"
)

type logLine struct {
	t    time.Time
	kind runner.EventKind
	step string
	text string
}

// runState is the latest run of one item.
type runState struct {
	key       string
	cancel    context.CancelFunc
	input     chan string // open interactive WebSocket
	running   bool
	started   time.Time
	ended     time.Time
	log       []logLine
	result    *runner.Result
	msgs      []runner.Message
	err       error
	overrides map[string]string
}

func (rs *runState) elapsed() time.Duration {
	if rs.running {
		return time.Since(rs.started)
	}
	return rs.ended.Sub(rs.started)
}

type runEventMsg struct {
	key string
	ev  runner.Event
	ch  <-chan runner.Event
}

type tickMsg struct{}

func waitEvent(key string, ch <-chan runner.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return runEventMsg{key: key, ev: ev, ch: ch}
	}
}

func tick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func itemKey(it spec.Item) string {
	if it.Kind == spec.KindFlow {
		return "flow:" + it.Name
	}
	return "req:" + it.Name
}

func (m *Model) anyRunning() bool {
	for _, rs := range m.runs {
		if rs.running {
			return true
		}
	}
	return false
}

// requestRun asks for declared inputs (and missing variables of a single
// request) before starting. reuse skips the prompts and uses the previous
// run's values.
func (m *Model) requestRun(it spec.Item, reuse bool) tea.Cmd {
	key := itemKey(it)
	if rs := m.runs[key]; rs != nil && rs.running {
		m.setErr("already running — x or ctrl+c to stop")
		return nil
	}
	fresh, err := m.ws.Lookup(key)
	if err != nil {
		m.setErr(err.Error())
		return nil
	}
	if reuse {
		if rs := m.runs[key]; rs != nil {
			return m.startRun(fresh, rs.overrides)
		}
	}
	names := runner.Inputs(m.ws, fresh)
	if fresh.Kind == spec.KindRequest {
		// Inputs will be prompted for; only report other undefined vars.
		placeholders := map[string]string{}
		for _, n := range names {
			placeholders[n] = "x"
		}
		scope := m.scope().Masked().Child("input", placeholders)
		if _, err := runner.PrepareIn(m.ws, fresh.Request, scope); err != nil {
			var pe *runner.PrepareError
			if errors.As(err, &pe) {
				names = append(names, pe.Missing...)
			}
		}
	}
	if len(names) == 0 {
		return m.startRun(fresh, nil)
	}
	m.prompt = &promptState{item: fresh, names: names, vals: map[string]string{}}
	m.nextPrompt()
	return nil
}

type promptState struct {
	item  spec.Item
	names []string
	idx   int
	vals  map[string]string
}

func (m *Model) nextPrompt() {
	p := m.prompt
	name := p.names[p.idx]
	def, _, _ := m.scope().Lookup(name)
	m.enterInput(modeInput, name+": ", def)
	if name == "file" || strings.HasSuffix(name, "_file") || strings.HasSuffix(name, "path") {
		m.setInfo("tip: paths are relative to " + m.cwd)
	}
}

func (m *Model) submitPrompt(v string) tea.Cmd {
	p := m.prompt
	name := p.names[p.idx]
	p.vals[name] = v
	m.state.Vars[name] = v
	m.saveState()
	p.idx++
	if p.idx < len(p.names) {
		m.nextPrompt()
		return nil
	}
	m.prompt = nil
	m.mode = modeNormal
	return m.startRun(p.item, p.vals)
}

func (m *Model) startRun(it spec.Item, overrides map[string]string) tea.Cmd {
	key := itemKey(it)
	if it.Err != nil {
		m.setErr(it.Err.Error())
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan runner.Event, 256)
	rs := &runState{key: key, cancel: cancel, running: true, started: time.Now(), overrides: overrides}
	if it.Kind == spec.KindRequest && it.Request.IsWS() {
		rs.input = make(chan string, 16)
	}
	r := &runner.Runner{
		WS:    m.ws,
		Scope: m.scope().Snapshot(),
		Emit:  func(e runner.Event) { ch <- e },
		Input: rs.input,
	}
	go func() {
		_, _ = r.Run(ctx, it, overrides)
		close(ch)
	}()
	m.runs[key] = rs
	if m.selectedKey() == key {
		if it.Kind == spec.KindFlow {
			m.tab = tabLog
		} else {
			m.tab = tabResponse
		}
	}
	m.setInfo("running " + it.Name + "…")
	m.refresh(true)
	return tea.Batch(waitEvent(key, ch), tick())
}

func (m *Model) stopRun(key string) bool {
	rs := m.runs[key]
	if rs == nil || !rs.running {
		return false
	}
	rs.cancel()
	return true
}

func (m *Model) handleEvent(msg runEventMsg) tea.Cmd {
	rs := m.runs[msg.key]
	if rs == nil {
		return nil
	}
	ev := msg.ev
	add := func(text string) {
		rs.log = append(rs.log, logLine{t: ev.Time, kind: ev.Kind, step: ev.Step, text: text})
	}
	switch ev.Kind {
	case runner.EvStepStart:
		add(ev.Text)
	case runner.EvRequest:
		rs.result, rs.msgs = ev.Result, nil
		add("→ " + ev.Text)
	case runner.EvResponse:
		rs.result = ev.Result
		add("← " + ev.Text)
	case runner.EvWSOpen:
		rs.result = ev.Result
		add(ev.Text)
	case runner.EvWSSend, runner.EvWSRecv:
		rs.msgs = append(rs.msgs, *ev.Msg)
		arrow := "→ "
		if ev.Kind == runner.EvWSRecv {
			arrow = "← "
		}
		add(arrow + oneLine(ev.Msg.Data, 200))
	case runner.EvWSClose:
		add(ev.Text)
	case runner.EvExtract:
		for _, k := range sortedKeys(ev.Vars) {
			m.state.Vars[k] = ev.Vars[k]
			add(fmt.Sprintf("%s = %s", k, truncate(ev.Vars[k], 200)))
		}
		m.saveState()
	case runner.EvLog:
		add(ev.Text)
	case runner.EvError:
		add("error: " + ev.Text)
	case runner.EvDone:
		rs.running = false
		rs.ended = time.Now()
		rs.input = nil
		rs.err = ev.Err
		if ev.Result != nil {
			rs.result = ev.Result
		}
		name := strings.SplitN(msg.key, ":", 2)[1]
		switch {
		case errors.Is(ev.Err, context.Canceled):
			m.setInfo(name + ": stopped")
		case ev.Err != nil:
			m.setErr(name + ": " + ev.Err.Error())
		default:
			m.setOK(fmt.Sprintf("%s: done in %s", name, rs.elapsed().Round(time.Millisecond)))
			if m.selectedKey() == msg.key && m.tab == tabLog && strings.HasPrefix(msg.key, "flow:") {
				m.tab = tabResponse
			}
		}
	}
	if m.selectedKey() == msg.key || m.tab == tabVars {
		m.refresh(true)
	}
	if ev.Kind == runner.EvDone {
		return nil
	}
	return waitEvent(msg.key, msg.ch)
}

func oneLine(b []byte, n int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	return truncate(s, n)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

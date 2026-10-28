package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) View() string {
	if m.width < 20 || m.height < 6 {
		return "terminal too small"
	}
	lw, rw := m.paneWidths()
	mainH := m.mainHeight()
	left := m.box(m.listView(lw-2, mainH-2), lw, mainH, m.focus == focusList)
	right := m.box(m.tabsView(rw-2)+"\n"+m.pager.View(), rw, mainH, m.focus == focusPager)
	rows := []string{
		m.headerView(),
		lipgloss.JoinHorizontal(lipgloss.Top, left, right),
		m.footerView(),
	}
	if m.showHints() {
		rows = append(rows, m.hintsView())
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

type hint struct{ key, desc string }

// hints lists the most useful keys for the current mode, focus and item.
func (m *Model) hints() []hint {
	switch m.mode {
	case modeCommand:
		return []hint{{"enter", "execute"}, {"tab", "complete"}, {"↑↓", "history"}, {"esc", "cancel"},
			{":new", "http|ws|flow|env"}, {":env", "switch"}, {":set", "k=v"}, {":w", "file"}, {":q", "quit"}}
	case modeSearch:
		if m.focus == focusList {
			return []hint{{"type", "fuzzy filter"}, {"enter", "keep filter"}, {"esc", "cancel"}}
		}
		return []hint{{"type", "search (smartcase)"}, {"enter", "accept"}, {"esc", "cancel"}, {"n N", "next/prev after"}}
	case modeJQ:
		return []hint{{"type", "live filter"}, {"enter", "apply"}, {"esc", "cancel"}, {"↑↓", "history"},
			{".data", "field"}, {".[] | .id", "iterate"}, {"keys", "object keys"}, {"$status $headers", "vars"}}
	case modeInsert:
		return []hint{{"enter", "send"}, {"↑↓", "history"}, {"esc", "normal mode"}, {"{{var}}", "templates work"}}
	case modeInput:
		return []hint{{"enter", "confirm"}, {"esc", "cancel run"}, {"ctrl+u", "clear"}}
	case modeConfirm:
		return []hint{{"y", "confirm"}, {"any key", "cancel"}}
	}
	if m.pending != "" {
		switch m.pending {
		case "g":
			return []hint{{"g", "top"}, {"t T", "next/prev tab"}, {"e", "edit env"}}
		case "y":
			return []hint{{"y", "yank curl / view"}, {"c", "curl"}, {"b", "body"}, {"u", "url"}, {"n", "name"}, {"p", "path"}}
		case "d":
			return []hint{{"d", "delete item"}}
		case "c":
			return []hint{{"w", "rename"}}
		case "z":
			return []hint{{"a", "toggle fold"}, {"o", "open"}, {"c", "close"}, {"R", "open all"}, {"M", "close all"}}
		case "ctrl+w":
			return []hint{{"h", "list"}, {"l", "response"}, {"w", "toggle"}}
		}
	}
	if m.help {
		return []hint{{"j k", "scroll"}, {"/", "search"}, {"q esc ?", "close help"}}
	}
	var hs []hint
	if rs := m.runs[m.selectedKey()]; rs != nil && rs.running {
		if rs.input != nil {
			hs = append(hs, hint{"i", "send ws msg"}, hint{"x", "close ws"})
		} else {
			hs = append(hs, hint{"x", "stop"})
		}
	}
	if _, ok := m.onCollection(); ok && m.focus == focusList {
		hs = append(hs, hint{"enter", "fold"}, hint{"l", "open"}, hint{"a", "new request here"},
			hint{"e", "settings"}, hint{"cw", "rename"}, hint{"dd", "delete"}, hint{"C", "new collection"},
			hint{"zM zR", "fold all/none"}, hint{"/", "filter"})
	} else if m.focus == focusList {
		hs = append(hs, hint{"enter", "run"}, hint{"e", "edit"}, hint{"a o A", "new http/ws/flow"},
			hint{"/", "filter"}, hint{"yy", "yank curl"}, hint{"p", "paste curl"}, hint{"m", "move"}, hint{"dd", "delete"},
			hint{"E", "env"}, hint{"l", "response"})
	} else {
		hs = append(hs, hint{"j k", "scroll"}, hint{"/", "search"}, hint{"|", "jq"}, hint{"yy", "yank"},
			hint{"1-5", "tabs"}, hint{"R", "rerun"}, hint{"h", "list"})
	}
	return append(hs, hint{":", "command"}, hint{"?", "help"}, hint{"q", "quit"})
}

// hintsView renders as many hints as fit on one line.
func (m *Model) hintsView() string {
	sep := sMuted.Render("  ")
	var b strings.Builder
	w := 1
	b.WriteString(" ")
	for i, h := range m.hints() {
		item := sKey.Render(h.key) + " " + sMuted.Render(h.desc)
		iw := lipgloss.Width(item)
		if i > 0 {
			iw += lipgloss.Width(sep)
		}
		if w+iw > m.width {
			break
		}
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(item)
		w += iw
	}
	return b.String()
}

func (m *Model) box(content string, w, h int, focused bool) string {
	c := lipgloss.TerminalColor(cMuted)
	if focused {
		c = cAccent
	}
	// Pad the content horizontally by one column inside the border.
	lines := strings.Split(content, "\n")
	inner := w - 4
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, inner, "…")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(c).
		Padding(0, 1).
		Width(w - 2).
		Height(h - 2).
		MaxHeight(h).
		Render(strings.Join(lines, "\n"))
}

func (m *Model) headerView() string {
	env := m.state.Env
	if env == "" {
		env = "none"
	}
	right := sMuted.Render("env ") + lipgloss.NewStyle().Foreground(cCyan).Bold(true).Render(env)
	n := 0
	for _, rs := range m.runs {
		if rs.running {
			n++
		}
	}
	if n > 0 {
		spin := []string{"◐", "◓", "◑", "◒"}[time.Now().UnixMilli()/250%4]
		right = sWarn.Render(fmt.Sprintf("%s %d running", spin, n)) + "   " + right
	}
	// Shorten the workspace path from the left so the right side always fits.
	title := sTitle.Render(" turl ")
	path := []rune(shortPath(m.ws.Dir))
	room := m.width - lipgloss.Width(title) - lipgloss.Width(right) - 2
	if len(path) > room {
		path = append([]rune("…"), path[max(0, len(path)-room+1):]...)
	}
	if room < 2 {
		path = nil
	}
	left := title + sMuted.Render(string(path))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	return left + strings.Repeat(" ", max(1, gap)) + right
}

func (m *Model) footerView() string {
	md := m.mode.String()
	if m.mode == modeSearch && m.focus == focusList {
		md = "FILTER"
	}
	style, ok := modeStyles[md]
	if !ok {
		style = modeStyles["SEARCH"]
	}
	badge := style.Render(md)
	var body string
	switch m.mode {
	case modeNormal:
		switch m.stKind {
		case stErr:
			body = sErr.Render(m.status)
		case stOK:
			body = sOK.Render(m.status)
		default:
			body = sMuted.Render(m.status)
		}
	case modeConfirm:
		body = sWarn.Render(m.status)
	default:
		body = m.input.View()
		if m.mode == modeJQ && m.jqErr != "" {
			body += "  " + sErr.Render(truncate(m.jqErr, 60))
		}
	}
	var right []string
	if m.pending != "" {
		right = append(right, sKey.Render(strings.ReplaceAll(m.pending, "ctrl+", "^")))
	}
	if m.jqFilter != "" && m.mode != modeJQ {
		right = append(right, lipgloss.NewStyle().Foreground(cOrange).Render("jq "+truncate(m.jqFilter, 24)))
	}
	if m.filter != "" && m.mode != modeSearch {
		right = append(right, sWarn.Render("filter "+m.filter))
	}
	right = append(right, sMuted.Render(m.pager.Percent()))
	r := strings.Join(right, "  ")
	avail := m.width - lipgloss.Width(badge) - lipgloss.Width(r) - 3
	body = ansi.Truncate(body, max(0, avail), "…")
	gap := m.width - lipgloss.Width(badge) - 1 - lipgloss.Width(body) - lipgloss.Width(r) - 1
	return badge + " " + body + strings.Repeat(" ", max(1, gap)) + r
}

func (m *Model) tabsView(w int) string {
	var parts []string
	for i, n := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, n)
		if tab(i) == m.tab && !m.help {
			parts = append(parts, sTabOn.Render(label))
		} else {
			parts = append(parts, sTabOff.Render(label))
		}
	}
	if m.help {
		parts = append(parts, sTabOn.Render("? Help"))
	}
	left := strings.Join(parts, "  ")
	right := ""
	if rs := m.runs[m.selectedKey()]; rs != nil {
		switch {
		case rs.running:
			right = sWarn.Render("● " + rs.elapsed().Round(100*time.Millisecond).String())
		case rs.err != nil:
			right = sErr.Render("✗ failed")
		case rs.result != nil && rs.result.StatusCode != 0 && !rs.result.WS:
			right = statusStyle(rs.result.StatusCode).Render(fmt.Sprint(rs.result.StatusCode)) +
				sMuted.Render(" "+rs.result.Duration.Round(time.Millisecond).String())
		}
	}
	gap := w - 2 - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

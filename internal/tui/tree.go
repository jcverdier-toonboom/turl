package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/jcverdier/turl/internal/spec"
)

// The list is a tree: a FLOWS section, then a COLLECTIONS section where
// every collection is a foldable row followed by its requests.

type rowKind int

const (
	rowSection rowKind = iota
	rowBlank
	rowCollection
	rowItem
)

type listRow struct {
	kind  rowKind
	label string // section title
	coll  string // collection of a collection or request row
	item  int    // index into items for rowItem
	count int    // requests shown in a collection row
}

func (r listRow) selectable() bool { return r.kind == rowCollection || r.kind == rowItem }

func collKey(c string) string { return "coll:" + c }

func (m *Model) rowKey(r listRow) string {
	switch r.kind {
	case rowCollection:
		return collKey(r.coll)
	case rowItem:
		return itemKey(m.items[r.item])
	}
	return ""
}

func (m *Model) matches(it spec.Item) bool { return m.filter == "" || fuzzy(it.Name, m.filter) }

// rebuild recomputes the rows, keeping the selection.
func (m *Model) rebuild() {
	key := m.selectedKey()
	m.buildRows()
	m.selectKey(key)
}

func (m *Model) buildRows() {
	m.rows = m.rows[:0]
	filtering := m.filter != ""

	var flows []int
	groups := map[string][]int{}
	for i, it := range m.items {
		if !m.matches(it) {
			continue
		}
		if it.Kind == spec.KindFlow {
			flows = append(flows, i)
			continue
		}
		c, _ := spec.SplitCollection(it.Name)
		groups[c] = append(groups[c], i)
	}
	if len(flows) > 0 {
		m.rows = append(m.rows, listRow{kind: rowSection, label: "FLOWS"})
		for _, i := range flows {
			m.rows = append(m.rows, listRow{kind: rowItem, item: i})
		}
	}

	seen := map[string]bool{}
	var colls []string
	for _, c := range m.ws.Collections() {
		seen[c] = true
		colls = append(colls, c)
	}
	for c := range groups {
		if c != "" && !seen[c] {
			colls = append(colls, c)
		}
	}
	sort.Strings(colls)
	// Orphan requests go to the default collection, listed last.
	if len(groups[""]) > 0 {
		colls = append(colls, "")
	}

	header := false
	for _, c := range colls {
		reqs := groups[c]
		if filtering && len(reqs) == 0 && !fuzzy(spec.CollectionLabel(c), m.filter) {
			continue
		}
		if !header {
			if len(m.rows) > 0 {
				m.rows = append(m.rows, listRow{kind: rowBlank})
			}
			m.rows = append(m.rows, listRow{kind: rowSection, label: "COLLECTIONS"})
			header = true
		}
		m.rows = append(m.rows, listRow{kind: rowCollection, coll: c, count: len(reqs)})
		if m.folded(c) && !filtering {
			continue
		}
		for _, i := range reqs {
			m.rows = append(m.rows, listRow{kind: rowItem, coll: c, item: i})
		}
	}
}

func (m *Model) folded(c string) bool {
	for _, f := range m.state.Folded {
		if f == spec.CollectionLabel(c) {
			return true
		}
	}
	return false
}

func (m *Model) setFolded(c string, fold bool) {
	label := spec.CollectionLabel(c)
	var out []string
	for _, f := range m.state.Folded {
		if f != label {
			out = append(out, f)
		}
	}
	if fold {
		out = append(out, label)
		sort.Strings(out)
	}
	m.state.Folded = out
	m.saveState()
	m.rebuild()
	m.refresh(false)
}

func (m *Model) foldAll(fold bool) {
	m.state.Folded = nil
	if fold {
		for _, r := range m.rows {
			if r.kind == rowCollection {
				m.state.Folded = append(m.state.Folded, spec.CollectionLabel(r.coll))
			}
		}
		// Rows of folded collections are gone; include every known one.
		m.state.Folded = append(m.state.Folded, m.ws.Collections()...)
		m.state.Folded = dedupe(m.state.Folded)
	}
	m.saveState()
	m.rebuild()
	m.refresh(false)
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
	sort.Strings(out)
	return out
}

func (m *Model) currentRow() (listRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) || !m.rows[m.cursor].selectable() {
		return listRow{}, false
	}
	return m.rows[m.cursor], true
}

// selected returns the request or flow under the cursor.
func (m *Model) selected() (spec.Item, bool) {
	r, ok := m.currentRow()
	if !ok || r.kind != rowItem {
		return spec.Item{}, false
	}
	return m.items[r.item], true
}

// selectedCollection returns the collection under the cursor: the
// collection row itself, or the collection of the selected request.
func (m *Model) selectedCollection() (string, bool) {
	r, ok := m.currentRow()
	if !ok {
		return "", false
	}
	if r.kind == rowCollection {
		return r.coll, true
	}
	if m.items[r.item].Kind == spec.KindRequest {
		return r.coll, true
	}
	return "", false
}

func (m *Model) onCollection() (string, bool) {
	r, ok := m.currentRow()
	return r.coll, ok && r.kind == rowCollection
}

func (m *Model) selectedKey() string {
	if r, ok := m.currentRow(); ok {
		return m.rowKey(r)
	}
	return ""
}

// selectKey moves the cursor to a row; a request hidden in a folded
// collection selects its collection.
func (m *Model) selectKey(key string) {
	if key != "" {
		for i, r := range m.rows {
			if r.selectable() && m.rowKey(r) == key {
				m.cursor = i
				return
			}
		}
		if name, ok := strings.CutPrefix(key, "req:"); ok {
			c, _ := spec.SplitCollection(name)
			for i, r := range m.rows {
				if r.kind == rowCollection && r.coll == c {
					m.cursor = i
					return
				}
			}
		}
	}
	m.cursor = min(max(m.cursor, 0), max(0, len(m.rows)-1))
	m.snap(1)
}

// snap moves the cursor to the nearest selectable row, in dir first.
func (m *Model) snap(dir int) {
	for _, d := range []int{dir, -dir} {
		for i := m.cursor; i >= 0 && i < len(m.rows); i += d {
			if m.rows[i].selectable() {
				m.cursor = i
				return
			}
		}
	}
}

// moveList moves the cursor n selectable rows.
func (m *Model) moveList(n int) {
	old := m.cursor
	dir := 1
	if n < 0 {
		dir, n = -1, -n
	}
	for ; n > 0; n-- {
		next := -1
		for i := m.cursor + dir; i >= 0 && i < len(m.rows); i += dir {
			if m.rows[i].selectable() {
				next = i
				break
			}
		}
		if next < 0 {
			break
		}
		m.cursor = next
	}
	if old != m.cursor {
		m.refresh(false)
	}
}

func (m *Model) listEdge(top bool) {
	if top {
		m.cursor = 0
		m.snap(1)
	} else {
		m.cursor = max(0, len(m.rows)-1)
		m.snap(-1)
	}
	m.refresh(false)
}

// jumpCollection moves to the previous/next collection row.
func (m *Model) jumpCollection(dir int) {
	for i := m.cursor + dir; i >= 0 && i < len(m.rows); i += dir {
		if m.rows[i].kind == rowCollection {
			m.cursor = i
			m.refresh(false)
			return
		}
	}
	m.listEdge(dir < 0)
}

// listLeft implements h in the list: go to the parent collection, or fold it.
func (m *Model) listLeft() {
	r, ok := m.currentRow()
	if !ok {
		return
	}
	switch {
	case r.kind == rowItem && m.items[r.item].Kind == spec.KindRequest:
		m.selectKey(collKey(r.coll))
		m.refresh(false)
	case r.kind == rowCollection && !m.folded(r.coll):
		m.setFolded(r.coll, true)
	}
}

// listRight implements l in the list: unfold, enter a collection, or focus
// the response pane.
func (m *Model) listRight() {
	r, ok := m.currentRow()
	switch {
	case ok && r.kind == rowCollection && m.folded(r.coll) && m.filter == "":
		m.setFolded(r.coll, false)
	case ok && r.kind == rowCollection && r.count > 0:
		m.moveList(1)
	default:
		m.focus = focusPager
	}
}

func (m *Model) listView(w, h int) string {
	inner := w - 2
	if len(m.rows) == 0 {
		if m.filter != "" {
			return sMuted.Render("no match")
		}
		return sMuted.Render("empty — a to add, C for a collection")
	}
	// Keep a margin of 2 rows around the cursor (scrolloff).
	so := min(2, h/4)
	if m.cursor < m.listOff+so {
		m.listOff = max(0, m.cursor-so)
	}
	if m.cursor >= m.listOff+h-so {
		m.listOff = m.cursor - h + so + 1
	}
	m.listOff = min(max(0, m.listOff), max(0, len(m.rows)-h))

	var b strings.Builder
	for i := m.listOff; i < len(m.rows) && i < m.listOff+h; i++ {
		if i > m.listOff {
			b.WriteByte('\n')
		}
		r := m.rows[i]
		sel := i == m.cursor
		switch r.kind {
		case rowSection:
			b.WriteString(sSection.Render(r.label))
		case rowCollection:
			b.WriteString(m.collectionRow(r, inner, sel))
		case rowItem:
			indent := 0
			if m.items[r.item].Kind == spec.KindRequest {
				indent = 2
			}
			b.WriteString(m.itemRow(m.items[r.item], inner, sel, indent))
		}
	}
	return b.String()
}

func (m *Model) cursorMark(sel bool) string {
	if !sel {
		return " "
	}
	if m.focus != focusList {
		return sMuted.Render("▌")
	}
	return lipgloss.NewStyle().Foreground(cAccent).Render("▌")
}

func (m *Model) collectionRow(r listRow, w int, sel bool) string {
	arrow := "▾"
	if m.folded(r.coll) && m.filter == "" {
		arrow = "▸"
	}
	label := spec.CollectionLabel(r.coll)
	count := fmt.Sprint(r.count)
	running := 0
	for key, rs := range m.runs {
		if c, _ := spec.SplitCollection(strings.TrimPrefix(key, "req:")); rs.running && strings.HasPrefix(key, "req:") && c == r.coll {
			running++
		}
	}
	if running > 0 {
		count = sWarn.Render("●") + " " + count
	}
	// cursor (1) + arrow (1) + space + label + space + count.
	labelW := w - 3 - lipgloss.Width(count) - 1
	label = truncate(label, max(1, labelW))
	pad := strings.Repeat(" ", max(0, labelW-lipgloss.Width(label)))
	style := lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	if r.coll == "" {
		style = lipgloss.NewStyle().Foreground(cMuted).Bold(true).Italic(true)
	}
	text := style.Render(label)
	if sel {
		text = sSelected.Inherit(style).Render(label + pad)
		pad = ""
	}
	return m.cursorMark(sel) + sMuted.Render(arrow) + " " + text + pad + " " + sMuted.Render(count)
}

func (m *Model) itemRow(it spec.Item, w int, sel bool, indent int) string {
	meth := "FLOW"
	if it.Request != nil {
		meth = it.Request.MethodOrDefault()
	} else if it.Kind == spec.KindRequest {
		meth = "?"
	}
	badge := methodStyle(meth).Render(fmt.Sprintf("%-6s", truncate(meth, 6)))
	mark := ""
	if rs := m.runs[itemKey(it)]; rs != nil {
		switch {
		case rs.running && rs.input != nil && rs.result != nil && rs.result.WS:
			mark = sOK.Render("●")
		case rs.running:
			mark = sWarn.Render("●")
		case rs.err != nil:
			mark = sErr.Render("✗")
		case rs.result != nil && rs.result.StatusCode != 0 && !rs.result.WS:
			mark = statusStyle(rs.result.StatusCode).Render(fmt.Sprint(rs.result.StatusCode))
		default:
			mark = sOK.Render("✓")
		}
	}
	if it.Err != nil {
		mark = sErr.Render("!")
	}
	name := it.Name
	if it.Kind == spec.KindRequest {
		_, name = spec.SplitCollection(it.Name)
	}
	// cursor (1) + indent + badge (6) + 2 spaces + mark.
	nameW := w - 9 - indent - lipgloss.Width(mark)
	name = truncate(name, max(1, nameW))
	pad := strings.Repeat(" ", max(0, nameW-lipgloss.Width(name)))
	ind := strings.Repeat(" ", indent)
	if sel {
		return m.cursorMark(true) + ind + badge + " " + sSelected.Render(name+pad) + " " + mark
	}
	return " " + ind + badge + " " + sText.Render(name) + pad + " " + mark
}

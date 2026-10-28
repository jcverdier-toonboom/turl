package tui

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// pager is a read-only, soft-wrapped text view with vim-style scrolling and
// search.
type pager struct {
	src    []string // styled source lines
	lines  []string // wrapped styled lines
	plines []string // wrapped plain lines
	width  int
	height int
	off    int

	query   string
	matches []int // indexes into lines
	cur     int
}

func (p *pager) SetSize(w, h int) {
	if w == p.width && h == p.height {
		return
	}
	p.width, p.height = w, h
	p.rewrap()
}

// SetContent replaces the text. When follow is set and the view was at the
// bottom, it stays at the bottom (tail -f style).
func (p *pager) SetContent(s string, follow bool) {
	atBottom := p.AtBottom()
	p.src = strings.Split(strings.TrimRight(s, "\n"), "\n")
	p.rewrap()
	if follow && atBottom {
		p.Bottom()
	}
	p.clamp()
}

func (p *pager) rewrap() {
	p.lines = p.lines[:0]
	p.plines = p.plines[:0]
	w := p.width
	if w < 1 {
		w = 1
	}
	for _, l := range p.src {
		l = strings.ReplaceAll(l, "\t", "    ")
		plain := ansi.Strip(l)
		if ansi.StringWidth(plain) <= w {
			p.lines = append(p.lines, l)
			p.plines = append(p.plines, plain)
			continue
		}
		ws := strings.Split(ansi.Hardwrap(l, w, true), "\n")
		ps := strings.Split(ansi.Hardwrap(plain, w, true), "\n")
		if len(ws) != len(ps) {
			ws = ps
		}
		p.lines = append(p.lines, ws...)
		p.plines = append(p.plines, ps...)
	}
	p.research()
	p.clamp()
}

func (p *pager) maxOff() int {
	m := len(p.lines) - p.height
	if m < 0 {
		return 0
	}
	return m
}

func (p *pager) clamp() {
	if p.off > p.maxOff() {
		p.off = p.maxOff()
	}
	if p.off < 0 {
		p.off = 0
	}
}

func (p *pager) AtBottom() bool { return p.off >= p.maxOff() }
func (p *pager) Scroll(n int)   { p.off += n; p.clamp() }
func (p *pager) Top()           { p.off = 0 }
func (p *pager) Bottom()        { p.off = p.maxOff() }
func (p *pager) Half() int      { return max(1, p.height/2) }
func (p *pager) Page() int      { return max(1, p.height-2) }
func (p *pager) GotoLine(n int) { p.off = n; p.clamp() }

// Percent describes the scroll position like vim's ruler.
func (p *pager) Percent() string {
	switch {
	case len(p.lines) <= p.height:
		return "All"
	case p.off == 0:
		return "Top"
	case p.AtBottom():
		return "Bot"
	}
	return strconv.Itoa(p.off*100/p.maxOff()) + "%"
}

// smartcase: case-insensitive unless the query has an upper-case letter.
func smartcase(q string) bool {
	for _, r := range q {
		if unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

func (p *pager) Search(q string) int {
	p.query = q
	p.research()
	if len(p.matches) == 0 {
		return 0
	}
	// Jump to the first match at or after the current view.
	p.cur = 0
	for i, m := range p.matches {
		if m >= p.off {
			p.cur = i
			break
		}
	}
	p.reveal()
	return len(p.matches)
}

func (p *pager) research() {
	p.matches = p.matches[:0]
	if p.query == "" {
		return
	}
	q := p.query
	fold := smartcase(q)
	if fold {
		q = strings.ToLower(q)
	}
	for i, l := range p.plines {
		if fold {
			l = strings.ToLower(l)
		}
		if strings.Contains(l, q) {
			p.matches = append(p.matches, i)
		}
	}
	if p.cur >= len(p.matches) {
		p.cur = 0
	}
}

func (p *pager) ClearSearch() { p.query = ""; p.matches = nil }

func (p *pager) Next(dir int) (int, int) {
	if len(p.matches) == 0 {
		return 0, 0
	}
	p.cur = (p.cur + dir + len(p.matches)) % len(p.matches)
	p.reveal()
	return p.cur + 1, len(p.matches)
}

func (p *pager) reveal() {
	line := p.matches[p.cur]
	if line < p.off || line >= p.off+p.height {
		p.off = line - p.height/3
		p.clamp()
	}
}

func (p *pager) Plain() string { return strings.Join(p.plines, "\n") }

func (p *pager) View() string {
	var b strings.Builder
	curLine := -1
	if len(p.matches) > 0 {
		curLine = p.matches[p.cur]
	}
	for i := 0; i < p.height; i++ {
		idx := p.off + i
		if i > 0 {
			b.WriteByte('\n')
		}
		if idx >= len(p.lines) {
			b.WriteString(sTilde.Render("~"))
			continue
		}
		line := p.lines[idx]
		if p.query != "" {
			line = highlightMatches(p.plines[idx], p.query, idx == curLine, line)
		}
		b.WriteString(line)
	}
	return b.String()
}

// highlightMatches re-renders a line in plain text with matches highlighted
// (syntax colors are dropped on matching lines).
func highlightMatches(plain, q string, current bool, styled string) string {
	hay, needle := plain, q
	if smartcase(q) {
		hay, needle = strings.ToLower(plain), strings.ToLower(q)
	}
	if !strings.Contains(hay, needle) || len(hay) != len(plain) {
		return styled
	}
	st := sMatch
	if current {
		st = sMatchCur
	}
	var b strings.Builder
	for {
		i := strings.Index(hay, needle)
		if i < 0 {
			b.WriteString(plain)
			break
		}
		b.WriteString(plain[:i])
		b.WriteString(st.Render(plain[i : i+len(needle)]))
		plain, hay = plain[i+len(needle):], hay[i+len(needle):]
	}
	return b.String()
}

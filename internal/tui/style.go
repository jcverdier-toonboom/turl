package tui

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

var (
	cAccent = lipgloss.AdaptiveColor{Light: "#5A4FCF", Dark: "#A99CFF"}
	cMuted  = lipgloss.AdaptiveColor{Light: "#8A8A8A", Dark: "#6C7086"}
	cText   = lipgloss.AdaptiveColor{Light: "#303030", Dark: "#CDD6F4"}
	cGreen  = lipgloss.AdaptiveColor{Light: "#2E7D32", Dark: "#A6E3A1"}
	cYellow = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#F9E2AF"}
	cRed    = lipgloss.AdaptiveColor{Light: "#C62828", Dark: "#F38BA8"}
	cBlue   = lipgloss.AdaptiveColor{Light: "#1565C0", Dark: "#89B4FA"}
	cCyan   = lipgloss.AdaptiveColor{Light: "#00838F", Dark: "#94E2D5"}
	cPurple = lipgloss.AdaptiveColor{Light: "#7B1FA2", Dark: "#CBA6F7"}
	cOrange = lipgloss.AdaptiveColor{Light: "#D84315", Dark: "#FAB387"}
	cSel    = lipgloss.AdaptiveColor{Light: "#E4E0FF", Dark: "#313244"}

	sTitle    = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sMuted    = lipgloss.NewStyle().Foreground(cMuted)
	sText     = lipgloss.NewStyle().Foreground(cText)
	sTilde    = lipgloss.NewStyle().Foreground(cMuted)
	sErr      = lipgloss.NewStyle().Foreground(cRed)
	sOK       = lipgloss.NewStyle().Foreground(cGreen)
	sWarn     = lipgloss.NewStyle().Foreground(cYellow)
	sSection  = lipgloss.NewStyle().Foreground(cMuted).Bold(true)
	sSelected = lipgloss.NewStyle().Background(cSel).Bold(true)
	sMatch    = lipgloss.NewStyle().Background(cYellow).Foreground(lipgloss.Color("#1E1E2E"))
	sMatchCur = lipgloss.NewStyle().Background(cOrange).Foreground(lipgloss.Color("#1E1E2E")).Bold(true)
	sTabOn    = lipgloss.NewStyle().Foreground(cAccent).Bold(true).Underline(true)
	sTabOff   = lipgloss.NewStyle().Foreground(cMuted)
	sKey      = lipgloss.NewStyle().Foreground(cBlue)
	sString   = lipgloss.NewStyle().Foreground(cGreen)
	sNumber   = lipgloss.NewStyle().Foreground(cOrange)
	sBool     = lipgloss.NewStyle().Foreground(cPurple)
	sComment  = lipgloss.NewStyle().Foreground(cMuted).Italic(true)
	sSent     = lipgloss.NewStyle().Foreground(cCyan).Bold(true)
	sRecv     = lipgloss.NewStyle().Foreground(cPurple).Bold(true)

	modeStyles = map[string]lipgloss.Style{
		"NORMAL":  lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(cAccent).Foreground(lipgloss.Color("#1E1E2E")),
		"INSERT":  lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(cGreen).Foreground(lipgloss.Color("#1E1E2E")),
		"COMMAND": lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(cYellow).Foreground(lipgloss.Color("#1E1E2E")),
		"SEARCH":  lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(cYellow).Foreground(lipgloss.Color("#1E1E2E")),
		"JQ":      lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(cOrange).Foreground(lipgloss.Color("#1E1E2E")),
		"INPUT":   lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(cBlue).Foreground(lipgloss.Color("#1E1E2E")),
		"CONFIRM": lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(cRed).Foreground(lipgloss.Color("#1E1E2E")),
	}
)

func methodStyle(m string) lipgloss.Style {
	c := cText
	switch m {
	case "GET":
		c = cGreen
	case "POST":
		c = cYellow
	case "PUT":
		c = cBlue
	case "PATCH":
		c = cPurple
	case "DELETE":
		c = cRed
	case "WS":
		c = cCyan
	case "FLOW":
		c = cAccent
	}
	return lipgloss.NewStyle().Foreground(c).Bold(true)
}

func statusStyle(code int) lipgloss.Style {
	switch {
	case code == 0:
		return sMuted
	case code < 300:
		return sOK.Bold(true)
	case code < 400:
		return sWarn.Bold(true)
	}
	return sErr.Bold(true)
}

// prettyJSON indents and colors a stream of JSON values, preserving key
// order and number formatting. ok is false if body is not valid JSON.
func prettyJSON(body []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	type frame struct {
		obj bool
		n   int
		key bool // next token is a key
	}
	var sb strings.Builder
	var stack []frame
	values := 0
	indent := func(d int) { sb.WriteString("\n" + strings.Repeat("  ", d)) }
	afterValue := func() {
		if len(stack) > 0 && stack[len(stack)-1].obj {
			stack[len(stack)-1].key = true
		}
	}
	valuePrefix := func() {
		if len(stack) == 0 {
			if values > 0 {
				sb.WriteByte('\n')
			}
			values++
			return
		}
		top := &stack[len(stack)-1]
		if top.obj {
			sb.WriteString(": ")
			return
		}
		if top.n > 0 {
			sb.WriteByte(',')
		}
		indent(len(stack))
		top.n++
	}
	closeFrame := func(c string) {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.n > 0 {
			indent(len(stack))
		}
		sb.WriteString(c)
		afterValue()
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false
		}
		if len(stack) > 0 && stack[len(stack)-1].obj && stack[len(stack)-1].key {
			top := &stack[len(stack)-1]
			if d, ok := tok.(json.Delim); ok && d == '}' {
				closeFrame("}")
				continue
			}
			k, ok := tok.(string)
			if !ok {
				return "", false
			}
			if top.n > 0 {
				sb.WriteByte(',')
			}
			indent(len(stack))
			top.n++
			top.key = false
			sb.WriteString(sKey.Render(quote(k)))
			continue
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				valuePrefix()
				sb.WriteString(string(t))
				stack = append(stack, frame{obj: t == '{', key: t == '{'})
			case '}', ']':
				if len(stack) == 0 {
					return "", false
				}
				closeFrame(string(t))
			}
		case string:
			valuePrefix()
			sb.WriteString(sString.Render(quote(t)))
			afterValue()
		case json.Number:
			valuePrefix()
			sb.WriteString(sNumber.Render(t.String()))
			afterValue()
		case bool:
			valuePrefix()
			if t {
				sb.WriteString(sBool.Render("true"))
			} else {
				sb.WriteString(sBool.Render("false"))
			}
			afterValue()
		case nil:
			valuePrefix()
			sb.WriteString(sBool.Render("null"))
			afterValue()
		}
	}
	if values == 0 || len(stack) != 0 {
		return "", false
	}
	return sb.String(), true
}

func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

// renderBody pretty-prints JSON, shows text as-is and summarizes binary.
func renderBody(b []byte) string {
	if len(bytes.TrimSpace(b)) == 0 {
		return sMuted.Render("(empty body)")
	}
	if s, ok := prettyJSON(b); ok {
		return s
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return sMuted.Render("(binary data, " + humanSize(len(b)) + ") — use save_to or :w <file>")
	}
	return string(b)
}

// highlightYAML colors keys and comments of a YAML document.
func highlightYAML(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		trim := strings.TrimLeft(l, " ")
		ind := l[:len(l)-len(trim)]
		switch {
		case strings.HasPrefix(trim, "#"):
			lines[i] = sComment.Render(l)
		case trim == "":
		default:
			rest := trim
			prefix := ""
			if strings.HasPrefix(rest, "- ") {
				prefix, rest = sMuted.Render("- "), rest[2:]
			}
			if k, v, ok := strings.Cut(rest, ":"); ok && !strings.ContainsAny(k, "\"'{[ ") && (v == "" || v[0] == ' ') {
				lines[i] = ind + prefix + sKey.Render(k) + ":" + yamlValue(v)
			} else {
				lines[i] = ind + prefix + sString.Render(rest)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func yamlValue(v string) string {
	t := strings.TrimSpace(v)
	switch {
	case t == "":
		return v
	case t == "|" || t == ">" || t == "|-" || t == ">-":
		return sMuted.Render(v)
	case t == "true" || t == "false" || t == "null":
		return " " + sBool.Render(t)
	case strings.HasPrefix(t, "#"):
		return " " + sComment.Render(t)
	}
	if strings.Trim(t, "0123456789.-") == "" {
		return " " + sNumber.Render(t)
	}
	return " " + sString.Render(t)
}

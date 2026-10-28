// Package curl converts between turl requests and curl command lines.
package curl

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jcverdier/turl/internal/runner"
	"github.com/jcverdier/turl/internal/spec"
)

// Quote quotes a shell word with single quotes.
func Quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,%+", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Export renders a prepared request as a curl command (websocat for WS).
func Export(p *runner.Prepared) string {
	var parts []string
	add := func(s ...string) { parts = append(parts, s...) }
	if p.WS {
		add("websocat")
		for _, h := range p.Headers {
			add("-H", Quote(h.Key+": "+h.Value))
		}
		if p.Insecure {
			add("-k")
		}
		add(Quote(p.URL))
		cmd := strings.Join(parts, " ")
		if len(p.Messages) > 0 {
			cmd = "printf '%s\\n' " + joinQuoted(p.Messages) + " | " + cmd
		}
		return cmd
	}
	add("curl", "-sS")
	hasBody := p.Body != "" || p.BodyFile != "" || len(p.Multipart) > 0
	if p.Method != "GET" && (p.Method != "POST" || !hasBody) {
		add("-X", p.Method)
	}
	if p.Insecure {
		add("-k")
	}
	if !p.NoFollow {
		add("-L")
	}
	for _, h := range p.Headers {
		add("-H", Quote(h.Key+": "+h.Value))
	}
	switch {
	case len(p.Multipart) > 0:
		for _, kv := range p.Multipart {
			add("-F", Quote(kv.Key+"="+kv.Value))
		}
	case p.BodyFile != "":
		add("--data-binary", Quote("@"+p.BodyFile))
	case p.Body != "":
		add("--data-raw", Quote(p.Body))
	}
	add(Quote(p.URL))
	return strings.Join(parts, " ")
}

func joinQuoted(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = Quote(s)
	}
	return strings.Join(q, " ")
}

// Split tokenizes a shell command line (quotes, escapes, line continuations).
func Split(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (s[i+1] == '\n' || s[i+1] == '\r'):
			i++
			if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			// ANSI-C quoting: $'...'
			i += 2
			for ; i < len(s) && s[i] != '\''; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					switch s[i] {
					case 'n':
						cur.WriteByte('\n')
					case 't':
						cur.WriteByte('\t')
					case 'r':
						cur.WriteByte('\r')
					default:
						cur.WriteByte(s[i])
					}
					continue
				}
				cur.WriteByte(s[i])
			}
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unterminated double quote")
			}
			inWord = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// Import parses a curl command line into a request.
func Import(cmd string) (*spec.Request, error) {
	args, err := Split(strings.TrimSpace(cmd))
	if err != nil {
		return nil, err
	}
	if len(args) == 0 || (args[0] != "curl" && !strings.HasSuffix(args[0], "/curl")) {
		return nil, errors.New("not a curl command")
	}
	return ImportArgs(args[1:])
}

// ImportArgs parses already-split curl arguments (without the leading "curl").
func ImportArgs(args []string) (*spec.Request, error) {
	args = append([]string{"curl"}, args...)
	r := &spec.Request{Headers: map[string]string{}}
	var data []string
	getMode := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		// Support --opt=value.
		if strings.HasPrefix(a, "--") && strings.Contains(a, "=") {
			kv := strings.SplitN(a, "=", 2)
			a = kv[0]
			args = append(args[:i+1], append([]string{kv[1]}, args[i+1:]...)...)
		}
		switch a {
		case "-X", "--request":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.Method = strings.ToUpper(v)
		case "-H", "--header":
			v, err := next()
			if err != nil {
				return nil, err
			}
			k, val, _ := strings.Cut(v, ":")
			r.Headers[strings.TrimSpace(k)] = strings.TrimSpace(val)
		case "-d", "--data", "--data-raw", "--data-binary", "--data-ascii", "--data-urlencode":
			v, err := next()
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(v, "@") && a != "--data-raw" {
				r.BodyFile = v[1:]
				continue
			}
			if a == "--data-urlencode" {
				if k, val, ok := strings.Cut(v, "="); ok {
					v = k + "=" + url.QueryEscape(val)
				} else {
					v = url.QueryEscape(v)
				}
			}
			data = append(data, v)
		case "--json":
			v, err := next()
			if err != nil {
				return nil, err
			}
			data = append(data, v)
			setDefault(r.Headers, "Content-Type", "application/json")
			setDefault(r.Headers, "Accept", "application/json")
		case "-F", "--form", "--form-string":
			v, err := next()
			if err != nil {
				return nil, err
			}
			k, val, _ := strings.Cut(v, "=")
			if r.Multipart == nil {
				r.Multipart = map[string]string{}
			}
			r.Multipart[k] = val
		case "-u", "--user":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.Headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(v))
		case "-A", "--user-agent":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.Headers["User-Agent"] = v
		case "-b", "--cookie":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.Headers["Cookie"] = v
		case "-e", "--referer":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.Headers["Referer"] = v
		case "--url":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.URL = v
		case "-k", "--insecure":
			r.Insecure = true
		case "-G", "--get":
			getMode = true
		case "-I", "--head":
			r.Method = "HEAD"
		case "-m", "--max-time":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.Timeout = v + "s"
		case "-o", "--output":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.SaveTo = v
		case "-T", "--upload-file":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.BodyFile = v
			if r.Method == "" {
				r.Method = "PUT"
			}
		case "--compressed", "-s", "--silent", "-S", "--show-error", "-L", "--location", "-v", "--verbose",
			"-i", "--include", "-f", "--fail", "-N", "--no-buffer", "-sS", "-sSL", "-fsSL", "-sL":
		// Options taking a value we ignore.
		case "--connect-timeout", "--retry", "-w", "--write-out", "--proxy", "-x", "-c", "--cookie-jar":
			if _, err := next(); err != nil {
				return nil, err
			}
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				continue // unknown flag, ignore
			}
			if r.URL == "" {
				r.URL = a
			}
		}
	}
	if r.URL == "" {
		return nil, errors.New("no URL found")
	}
	if len(data) > 0 {
		joined := strings.Join(data, "&")
		if getMode {
			sep := "?"
			if strings.Contains(r.URL, "?") {
				sep = "&"
			}
			r.URL += sep + joined
		} else {
			r.Body = joined
			if _, ok := ci(r.Headers, "Content-Type"); !ok && !strings.HasPrefix(strings.TrimSpace(joined), "{") {
				r.Headers["Content-Type"] = "application/x-www-form-urlencoded"
			}
		}
	}
	if r.Method == "" && r.Body == "" && r.BodyFile == "" && len(r.Multipart) == 0 {
		r.Method = "GET"
	} else if r.Method == "" {
		r.Method = "POST"
	}
	if len(r.Headers) == 0 {
		r.Headers = nil
	}
	if strings.HasPrefix(r.URL, "ws://") || strings.HasPrefix(r.URL, "wss://") {
		r.Protocol = spec.ProtoWS
		r.Method = ""
	}
	return r, nil
}

func ci(m map[string]string, k string) (string, bool) {
	for kk, v := range m {
		if strings.EqualFold(kk, k) {
			return v, true
		}
	}
	return "", false
}

func setDefault(m map[string]string, k, v string) {
	if _, ok := ci(m, k); !ok {
		m[k] = v
	}
}

// SuggestName derives a request name like "post-v1-uploads" from a request.
func SuggestName(r *spec.Request) string {
	path := r.URL
	if u, err := url.Parse(r.URL); err == nil {
		if path = strings.Trim(u.Path, "/"); path == "" {
			path = u.Host
		}
	}
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(path) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 48 {
		name = strings.Trim(name[len(name)-48:], "-")
	}
	if name == "" {
		name = "request"
	}
	return strings.ToLower(r.MethodOrDefault()) + "-" + name
}

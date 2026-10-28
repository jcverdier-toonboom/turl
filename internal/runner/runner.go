// Package runner executes requests and flows, reporting progress as events.
package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/itchyny/gojq"

	"github.com/jcverdier/turl/internal/jq"
	"github.com/jcverdier/turl/internal/spec"
	"github.com/jcverdier/turl/internal/vars"
)

type EventKind int

const (
	EvStepStart EventKind = iota
	EvRequest
	EvResponse
	EvWSOpen
	EvWSSend
	EvWSRecv
	EvWSClose
	EvExtract
	EvLog
	EvError
	EvDone
)

type Event struct {
	Kind   EventKind
	Time   time.Time
	Step   string
	Text   string
	Result *Result
	Msg    *Message
	Vars   map[string]string
	Err    error
}

type Dir int

const (
	Sent Dir = iota
	Recv
)

type Message struct {
	Time   time.Time
	Dir    Dir
	Data   []byte
	Binary bool
}

// Result of one HTTP or WebSocket request.
type Result struct {
	Name       string
	WS         bool
	Method     string
	URL        string
	ReqHeaders []KV
	ReqBody    string
	Proto      string
	Status     string
	StatusCode int
	Headers    http.Header
	Body       []byte
	Duration   time.Duration
	Messages   []Message
	Started    time.Time
	Err        error
}

// Received returns the bodies of received WebSocket messages.
func (r *Result) Received() [][]byte {
	var out [][]byte
	for _, m := range r.Messages {
		if m.Dir == Recv {
			out = append(out, m.Data)
		}
	}
	return out
}

// Runner executes requests. Emit is called from the runner goroutine.
type Runner struct {
	WS    *spec.Workspace
	Scope *vars.Scope // snapshot owned by the runner
	Emit  func(Event)
	// Input feeds messages typed by the user into an open WebSocket. When
	// set, a WS request without until/count stays open until cancelled.
	Input <-chan string

	captured map[string]string
}

func (r *Runner) emit(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Result != nil {
		// The runner keeps mutating its result; hand out a copy.
		cp := *e.Result
		e.Result = &cp
	}
	if r.Emit != nil {
		r.Emit(e)
	}
}

// Run executes a request or a flow.
func (r *Runner) Run(ctx context.Context, it spec.Item, overrides map[string]string) (*Result, error) {
	r.captured = map[string]string{}
	var res *Result
	var err error
	if it.Kind == spec.KindFlow {
		res, err = r.runFlow(ctx, it.Flow, overrides)
	} else {
		res, err = r.runRequest(ctx, it.Request, r.Scope.Child("captured", r.captured).Child("input", overrides), it.Name)
	}
	if err != nil {
		r.emit(Event{Kind: EvError, Err: err, Text: err.Error()})
	}
	r.emit(Event{Kind: EvDone, Result: res, Err: err})
	return res, err
}

// Inputs lists variables to ask for before running an item.
func Inputs(ws *spec.Workspace, it spec.Item) []string {
	var out []string
	seen := map[string]bool{}
	add := func(ns []string) {
		for _, n := range ns {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	switch it.Kind {
	case spec.KindRequest:
		if it.Request != nil {
			add(it.Request.Inputs)
		}
	case spec.KindFlow:
		if it.Flow == nil {
			return nil
		}
		add(it.Flow.Inputs)
		for _, s := range it.Flow.Steps {
			if req, err := stepRequest(ws, &s); err == nil {
				add(req.Inputs)
			}
		}
	}
	return out
}

func stepRequest(ws *spec.Workspace, s *spec.Step) (*spec.Request, error) {
	var req spec.Request
	switch {
	case s.Inline != nil:
		req = *s.Inline
		req.Name = s.Name
	case s.Use != "":
		r, err := ws.LoadRequest(s.Use)
		if err != nil {
			return nil, err
		}
		req = *r
	default:
		return nil, errors.New("step needs `request:` or `inline:`")
	}
	if len(s.Extract) > 0 {
		m := map[string]string{}
		for k, v := range req.Extract {
			m[k] = v
		}
		for k, v := range s.Extract {
			m[k] = v
		}
		req.Extract = m
	}
	if s.SaveTo != "" {
		req.SaveTo = s.SaveTo
	}
	if s.Until != "" && req.IsWS() {
		req.Until = s.Until
	}
	return &req, nil
}

func (r *Runner) runFlow(ctx context.Context, f *spec.Flow, inputs map[string]string) (*Result, error) {
	if f == nil {
		return nil, errors.New("flow failed to load")
	}
	// Priority: step vars > captured during this run > inputs > flow vars > state > env.
	base := r.Scope.Child("flow", f.Vars)
	var last *Result
	for i := range f.Steps {
		s := &f.Steps[i]
		label := s.Label(i)
		if s.Skip {
			r.emit(Event{Kind: EvLog, Step: label, Text: "skipped"})
			continue
		}
		if err := ctx.Err(); err != nil {
			return last, err
		}
		r.emit(Event{Kind: EvStepStart, Step: label, Text: fmt.Sprintf("[%d/%d] %s", i+1, len(f.Steps), label)})
		req, err := stepRequest(r.WS, s)
		if err != nil {
			return last, fmt.Errorf("step %q: %w", label, err)
		}
		scope := base.Child("input", inputs).Child("captured", r.captured).Child("step", s.Vars)

		attempts := 1
		if s.Retry > 0 && s.Until != "" && !req.IsWS() {
			attempts = s.Retry + 1
		}
		delay := 2 * time.Second
		if d, err := time.ParseDuration(s.RetryDelay); err == nil && d > 0 {
			delay = d
		}
		for a := 1; ; a++ {
			res, err := r.runRequest(ctx, req, scope, label)
			if res != nil {
				last = res
			}
			if err != nil {
				return last, fmt.Errorf("step %q: %w", label, err)
			}
			if s.Until == "" || req.IsWS() {
				break
			}
			ok, err := httpUntil(s.Until, res)
			if err != nil {
				return last, fmt.Errorf("step %q until: %w", label, err)
			}
			if ok {
				break
			}
			if a >= attempts {
				return last, fmt.Errorf("step %q: until %q not satisfied after %d attempt(s)", label, s.Until, a)
			}
			r.emit(Event{Kind: EvLog, Step: label, Text: fmt.Sprintf("until not met, retry %d/%d in %s", a, attempts-1, delay)})
			select {
			case <-ctx.Done():
				return last, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return last, nil
}

func httpUntil(expr string, res *Result) (bool, error) {
	out, err := jq.Eval(expr, jq.DecodeOne(res.Body), jqEnv(res))
	if err != nil {
		return false, err
	}
	for _, v := range out {
		if jq.Truthy(v) {
			return true, nil
		}
	}
	return false, nil
}

func jqEnv(res *Result) jq.Env {
	env := jq.Env{Status: res.StatusCode, Headers: map[string]string{}}
	for k, v := range res.Headers {
		env.Headers[k] = strings.Join(v, ", ")
	}
	return env
}

func (r *Runner) runRequest(ctx context.Context, req *spec.Request, scope *vars.Scope, label string) (*Result, error) {
	if req == nil {
		return nil, errors.New("request failed to load")
	}
	p, err := PrepareIn(r.WS, req, scope)
	if err != nil {
		return nil, err
	}
	res, err := r.do(ctx, p, label)
	// A rotated secret shows up as 401/403: re-fetch the secrets this request
	// uses and retry once.
	if res != nil && (res.StatusCode == 401 || res.StatusCode == 403) && !statusOK(p, res.StatusCode) && vars.Secrets != nil {
		if names := SecretRefs(r.WS, req, scope); len(names) > 0 {
			r.emit(Event{Kind: EvLog, Step: label, Text: fmt.Sprintf("%s: refreshing secrets %s and retrying", res.Status, strings.Join(names, ", "))})
			vars.Secrets.Invalidate(names...)
			p, perr := PrepareIn(r.WS, req, scope)
			if perr != nil {
				return res, perr
			}
			return r.do(ctx, p, label)
		}
	}
	return res, err
}

func (r *Runner) do(ctx context.Context, p *Prepared, label string) (*Result, error) {
	if p.WS {
		return r.doWS(ctx, p, label)
	}
	return r.doHTTP(ctx, p, label)
}

func (r *Runner) capture(label string, p *Prepared, input any, env jq.Env) error {
	if len(p.Extract) == 0 {
		return nil
	}
	got := map[string]string{}
	for _, name := range sortedKeys(p.Extract) {
		expr := p.Extract[name]
		out, err := jq.Eval(expr, input, env)
		if err != nil {
			return fmt.Errorf("extract %s (%s): %w", name, expr, err)
		}
		if len(out) == 0 || out[0] == nil {
			return fmt.Errorf("extract %s: %s returned null", name, expr)
		}
		v := jq.ToString(out[0])
		got[name] = v
		r.captured[name] = v
	}
	r.emit(Event{Kind: EvExtract, Step: label, Vars: got})
	return nil
}

func (r *Runner) doHTTP(ctx context.Context, p *Prepared, label string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	var body io.Reader
	var preview string
	contentType := ""
	switch {
	case len(p.Multipart) > 0:
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, kv := range p.Multipart {
			if strings.HasPrefix(kv.Value, "@") {
				path := kv.Value[1:]
				f, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				fw, err := mw.CreateFormFile(kv.Key, filepath.Base(path))
				if err == nil {
					_, err = io.Copy(fw, f)
				}
				f.Close()
				if err != nil {
					return nil, err
				}
			} else if err := mw.WriteField(kv.Key, kv.Value); err != nil {
				return nil, err
			}
		}
		mw.Close()
		body, contentType = &buf, mw.FormDataContentType()
		preview = fmt.Sprintf("<multipart %d bytes>", buf.Len())
	case p.BodyFile != "":
		f, err := os.Open(p.BodyFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		st, _ := f.Stat()
		body = f
		preview = fmt.Sprintf("<file %s, %d bytes>", p.BodyFile, st.Size())
	case p.Body != "":
		body = strings.NewReader(p.Body)
		preview = p.Body
	}

	hreq, err := http.NewRequestWithContext(ctx, p.Method, p.URL, body)
	if err != nil {
		return nil, err
	}
	if f, ok := body.(*os.File); ok {
		if st, err := f.Stat(); err == nil {
			hreq.ContentLength = st.Size()
		}
	}
	for _, h := range p.Headers {
		if strings.EqualFold(h.Key, "Host") {
			hreq.Host = h.Value
			continue
		}
		hreq.Header.Add(h.Key, h.Value)
	}
	if contentType != "" && hreq.Header.Get("Content-Type") == "" {
		hreq.Header.Set("Content-Type", contentType)
	}
	if hreq.Header.Get("User-Agent") == "" {
		hreq.Header.Set("User-Agent", "turl/0.1")
	}
	reqHeaders := make([]KV, 0, len(hreq.Header))
	for _, k := range sortedKeys(flatten(hreq.Header)) {
		reqHeaders = append(reqHeaders, KV{k, hreq.Header.Get(k)})
	}

	res := &Result{Name: label, Method: p.Method, URL: p.URL, ReqHeaders: reqHeaders, ReqBody: preview, Started: time.Now()}
	r.emit(Event{Kind: EvRequest, Step: label, Text: p.Method + " " + p.URL, Result: res})

	client := &http.Client{Transport: transport(p.Insecure)}
	if p.NoFollow {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	resp, err := client.Do(hreq)
	if err != nil {
		res.Err = err
		res.Duration = time.Since(res.Started)
		return res, err
	}
	defer resp.Body.Close()
	res.Body, err = io.ReadAll(resp.Body)
	res.Duration = time.Since(res.Started)
	res.Proto, res.Status, res.StatusCode, res.Headers = resp.Proto, resp.Status, resp.StatusCode, resp.Header
	if err != nil {
		res.Err = err
		return res, err
	}
	r.emit(Event{Kind: EvResponse, Step: label, Result: res,
		Text: fmt.Sprintf("%s  %s  %s", resp.Status, res.Duration.Round(time.Millisecond), humanSize(len(res.Body)))})

	if !statusOK(p, resp.StatusCode) {
		err := fmt.Errorf("unexpected status %s", resp.Status)
		res.Err = err
		return res, err
	}
	if p.SaveTo != "" {
		if err := os.WriteFile(p.SaveTo, res.Body, 0o644); err != nil {
			return res, err
		}
		r.emit(Event{Kind: EvLog, Step: label, Text: "saved body to " + p.SaveTo})
	}
	if err := r.capture(label, p, jq.DecodeOne(res.Body), jqEnv(res)); err != nil {
		res.Err = err
		return res, err
	}
	return res, nil
}

func statusOK(p *Prepared, code int) bool {
	if p.AnyStatus {
		return true
	}
	if len(p.Expect) > 0 {
		for _, c := range p.Expect {
			if c == code {
				return true
			}
		}
		return false
	}
	return code >= 200 && code < 400
}

func flatten(h http.Header) map[string]string {
	m := map[string]string{}
	for k, v := range h {
		m[k] = strings.Join(v, ", ")
	}
	return m
}

func transport(insecure bool) http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per request
	}
	return t
}

func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// HumanSize is exported for the UI.
func HumanSize(n int) string { return humanSize(n) }

func (r *Runner) doWS(ctx context.Context, p *Prepared, label string) (*Result, error) {
	interactive := r.Input != nil && p.Until == "" && p.Count == 0
	var cancel context.CancelFunc
	if interactive {
		ctx, cancel = context.WithCancel(ctx)
	} else {
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
	}
	defer cancel()

	var until *gojq.Code
	if p.Until != "" {
		c, err := jq.Compile(p.Until)
		if err != nil {
			return nil, fmt.Errorf("until: %w", err)
		}
		until = c
	}

	h := http.Header{}
	for _, kv := range p.Headers {
		h.Add(kv.Key, kv.Value)
	}
	dialer := *websocket.DefaultDialer
	if p.Insecure {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per request
	}
	res := &Result{Name: label, WS: true, Method: "WS", URL: p.URL, ReqHeaders: p.Headers, Started: time.Now()}
	r.emit(Event{Kind: EvRequest, Step: label, Text: "WS " + p.URL, Result: res})

	conn, resp, err := dialer.DialContext(ctx, p.URL, h)
	if resp != nil {
		res.Status, res.StatusCode, res.Headers, res.Proto = resp.Status, resp.StatusCode, resp.Header, resp.Proto
		if err != nil && resp.Body != nil {
			res.Body, _ = io.ReadAll(resp.Body)
		}
	}
	if err != nil {
		res.Err = err
		res.Duration = time.Since(res.Started)
		return res, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	r.emit(Event{Kind: EvWSOpen, Step: label, Result: res, Text: "connected " + p.URL})

	type incoming struct {
		msg Message
		err error
	}
	in := make(chan incoming, 64)
	go func() {
		for {
			t, data, err := conn.ReadMessage()
			if err != nil {
				in <- incoming{err: err}
				return
			}
			in <- incoming{msg: Message{Time: time.Now(), Dir: Recv, Data: data, Binary: t == websocket.BinaryMessage}}
		}
	}()

	send := func(s string) error {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
			return err
		}
		m := Message{Time: time.Now(), Dir: Sent, Data: []byte(s)}
		res.Messages = append(res.Messages, m)
		r.emit(Event{Kind: EvWSSend, Step: label, Msg: &m})
		return nil
	}
	for _, m := range p.Messages {
		if err := send(m); err != nil {
			res.Err = err
			return res, err
		}
	}

	finish := func(matched any, err error) (*Result, error) {
		res.Duration = time.Since(res.Started)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		r.emit(Event{Kind: EvWSClose, Step: label, Result: res, Text: "closed"})
		if err != nil {
			res.Err = err
			return res, err
		}
		if matched != nil || len(p.Extract) > 0 {
			var all []any
			for _, b := range res.Received() {
				all = append(all, jq.DecodeOne(b))
			}
			if matched == nil && len(all) > 0 {
				matched = all[len(all)-1]
			}
			if err := r.capture(label, p, matched, jq.Env{All: all}); err != nil {
				res.Err = err
				return res, err
			}
		}
		if p.SaveTo != "" {
			var buf bytes.Buffer
			for _, b := range res.Received() {
				buf.Write(b)
				buf.WriteByte('\n')
			}
			if err := os.WriteFile(p.SaveTo, buf.Bytes(), 0o644); err != nil {
				return res, err
			}
		}
		return res, nil
	}

	received := 0
	input := r.Input
	for {
		select {
		case <-ctx.Done():
			if interactive || (until == nil && p.Count == 0) {
				// Closed by the user, or a fire-and-collect request timed out.
				return finish(nil, nil)
			}
			return finish(nil, fmt.Errorf("timed out after %s waiting for until/count", p.Timeout))
		case s, ok := <-input:
			if !ok {
				input = nil
				continue
			}
			if err := send(s); err != nil {
				return finish(nil, err)
			}
		case inc := <-in:
			if inc.err != nil {
				if websocket.IsCloseError(inc.err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					r.emit(Event{Kind: EvLog, Step: label, Text: "server closed connection"})
					if until != nil || p.Count > 0 {
						return finish(nil, errors.New("connection closed before until/count was satisfied"))
					}
					return finish(nil, nil)
				}
				if ctx.Err() != nil {
					continue
				}
				return finish(nil, inc.err)
			}
			m := inc.msg
			res.Messages = append(res.Messages, m)
			received++
			r.emit(Event{Kind: EvWSRecv, Step: label, Msg: &m})
			if until != nil {
				v := jq.DecodeOne(m.Data)
				out, err := jq.Run(until, v, jq.Env{})
				if err != nil {
					r.emit(Event{Kind: EvLog, Step: label, Text: "until: " + err.Error()})
				}
				for _, o := range out {
					if jq.Truthy(o) {
						return finish(v, nil)
					}
				}
			}
			if p.Count > 0 && received >= p.Count {
				return finish(nil, nil)
			}
		}
	}
}

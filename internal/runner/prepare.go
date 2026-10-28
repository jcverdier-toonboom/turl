package runner

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jcverdier/turl/internal/spec"
	"github.com/jcverdier/turl/internal/vars"
)

// KV is an ordered key/value pair.
type KV struct{ Key, Value string }

// Prepared is a request with every variable substituted.
type Prepared struct {
	Name      string
	WS        bool
	Method    string
	URL       string
	Headers   []KV
	Body      string
	BodyFile  string
	Multipart []KV
	Messages  []string
	Until     string
	Count     int
	Extract   map[string]string
	SaveTo    string
	Timeout   time.Duration
	Insecure  bool
	NoFollow  bool
	Expect    []int
	AnyStatus bool
}

func (p *Prepared) Header(name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Key, name) {
			return h.Value
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Prepare renders a request. baseDir resolves relative file paths.
func Prepare(r *spec.Request, scope *vars.Scope, baseDir string) (*Prepared, error) {
	var errs []string
	missing := map[string]bool{}
	render := func(field, s string) string {
		out, err := scope.Render(s)
		if err != nil {
			if me, ok := err.(*vars.MissingError); ok {
				for _, n := range me.Names {
					missing[n] = true
				}
			} else {
				errs = append(errs, fmt.Sprintf("%s: %v", field, err))
			}
			return s
		}
		return out
	}
	p := &Prepared{
		Name:      r.Name,
		WS:        r.IsWS(),
		Method:    r.MethodOrDefault(),
		URL:       strings.TrimSpace(render("url", r.URL)),
		Until:     r.Until,
		Count:     r.Count,
		Extract:   r.Extract,
		Insecure:  r.Insecure,
		NoFollow:  r.NoFollow,
		Expect:    r.ExpectStatus,
		AnyStatus: r.AllowAnyStatus,
	}
	def := 60 * time.Second
	p.Timeout = r.TimeoutOr(def)

	if len(r.Query) > 0 {
		u, err := url.Parse(p.URL)
		if err != nil {
			errs = append(errs, "url: "+err.Error())
		} else {
			q := u.Query()
			for _, k := range sortedKeys(r.Query) {
				q.Add(k, render("query."+k, r.Query[k]))
			}
			u.RawQuery = q.Encode()
			p.URL = u.String()
		}
	}
	for _, k := range sortedKeys(r.Headers) {
		p.Headers = append(p.Headers, KV{k, render("headers."+k, r.Headers[k])})
	}
	if r.Body != "" {
		p.Body = render("body", r.Body)
	}
	if r.BodyFile != "" {
		p.BodyFile = resolvePath(render("body_file", r.BodyFile), baseDir)
	}
	for _, k := range sortedKeys(r.Multipart) {
		v := render("multipart."+k, r.Multipart[k])
		if strings.HasPrefix(v, "@") {
			v = "@" + resolvePath(v[1:], baseDir)
		}
		p.Multipart = append(p.Multipart, KV{k, v})
	}
	for i, m := range r.Messages {
		p.Messages = append(p.Messages, render(fmt.Sprintf("messages[%d]", i), m))
	}
	if r.SaveTo != "" {
		p.SaveTo = resolvePath(render("save_to", r.SaveTo), baseDir)
	}
	if r.Until != "" {
		p.Until = render("until", r.Until)
	}
	if len(r.Extract) > 0 {
		p.Extract = map[string]string{}
		for k, v := range r.Extract {
			p.Extract[k] = render("extract."+k, v)
		}
	}
	if len(missing) > 0 {
		names := sortedKeys(boolMap(missing))
		errs = append([]string{(&vars.MissingError{Names: names}).Error()}, errs...)
		return p, &PrepareError{Missing: names, Msg: strings.Join(errs, "; ")}
	}
	if len(errs) > 0 {
		return p, &PrepareError{Msg: strings.Join(errs, "; ")}
	}
	if p.URL == "" {
		return p, &PrepareError{Msg: "url is empty"}
	}
	return p, nil
}

func boolMap(m map[string]bool) map[string]string {
	out := map[string]string{}
	for k := range m {
		out[k] = ""
	}
	return out
}

// PrepareError reports render problems; Missing lists undefined variables.
type PrepareError struct {
	Missing []string
	Msg     string
}

func (e *PrepareError) Error() string { return e.Msg }

func resolvePath(p, base string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(p) && base != "" {
		p = filepath.Join(base, p)
	}
	return p
}

// WithCollection applies the settings of the request's collection: its vars
// sit just above the environment (so a collection can pin its service's
// base_url), its headers under the request's own headers.
func WithCollection(ws *spec.Workspace, req *spec.Request, scope *vars.Scope) (*spec.Request, *vars.Scope, error) {
	if ws == nil || req == nil || req.Collection == "" {
		return req, scope, nil
	}
	c, err := ws.LoadCollection(req.Collection)
	if err != nil {
		return req, scope, fmt.Errorf("collection %s: %w", req.Collection, err)
	}
	if len(c.Vars) > 0 {
		scope = scope.InsertBefore("env:", vars.Layer{Name: "collection:" + c.Name, Vars: c.Vars})
	}
	if len(c.Headers) > 0 {
		h := map[string]string{}
		for k, v := range c.Headers {
			h[k] = v
		}
		for k, v := range req.Headers {
			for ck := range h {
				if strings.EqualFold(ck, k) {
					delete(h, ck)
				}
			}
			h[k] = v
		}
		cp := *req
		cp.Headers = h
		req = &cp
	}
	return req, scope, nil
}

// PrepareIn renders a request with its collection settings applied.
func PrepareIn(ws *spec.Workspace, req *spec.Request, scope *vars.Scope) (*Prepared, error) {
	r, sc, cerr := WithCollection(ws, req, scope)
	p, err := Prepare(r, sc, "")
	if cerr != nil {
		return p, cerr
	}
	return p, err
}

// SecretRefs lists the secrets a request uses (through its collection and
// variables too), without fetching them.
func SecretRefs(ws *spec.Workspace, req *spec.Request, scope *vars.Scope) []string {
	r, sc, _ := WithCollection(ws, req, scope)
	ins := []string{r.URL, r.Body, r.BodyFile}
	for _, m := range []map[string]string{r.Query, r.Headers, r.Multipart} {
		for _, v := range m {
			ins = append(ins, v)
		}
	}
	ins = append(ins, r.Messages...)
	return sc.SecretRefs(ins...)
}

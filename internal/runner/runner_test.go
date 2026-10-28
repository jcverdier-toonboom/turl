package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/jcverdier/turl/internal/secrets"
	"github.com/jcverdier/turl/internal/spec"
	"github.com/jcverdier/turl/internal/vars"
)

// fakeAPI mimics the AI-service pattern: POST /uploads returns a link,
// PUT /blob stores the asset, /ws streams progress then a result.
func fakeAPI(t *testing.T) *httptest.Server {
	var uploaded string
	mux := http.NewServeMux()
	srvURL := ""
	mux.HandleFunc("/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"upload_url": srvURL + "/blob", "id": 42}})
	})
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		uploaded = string(b)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, msg, _ := c.ReadMessage()
		var job map[string]any
		json.Unmarshal(msg, &job)
		c.WriteMessage(websocket.TextMessage, []byte(`{"status":"running","progress":0.5}`))
		c.WriteMessage(websocket.TextMessage, []byte(`{"status":"done","result":{"asset":`+jsonNum(job["asset"])+`,"content":"`+uploaded+`"}}`))
		c.ReadMessage()
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	return srv
}

func jsonNum(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestFlow(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	dir := t.TempDir()
	ws, err := spec.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(dir, "asset.txt")
	os.WriteFile(asset, []byte("pixels"), 0o644)
	write := func(p, s string) { os.WriteFile(p, []byte(s), 0o644) }
	write(ws.RequestPath("get-link"), `
method: POST
url: "{{base}}/uploads"
headers: {Authorization: "Bearer {{token}}"}
extract: {upload_url: .data.upload_url, asset_id: .data.id}
`)
	write(ws.RequestPath("upload"), `
method: PUT
url: "{{upload_url}}"
body_file: "{{file}}"
inputs: [file]
`)
	write(ws.FlowPath("ai"), `
steps:
  - request: get-link
  - request: upload
  - name: process
    inline:
      protocol: ws
      url: "{{ws}}/ws"
      messages: ['{"asset": {{asset_id}}}']
      until: 'select(.status == "done")'
      extract: {content: .result.content, progress_seen: '[$all[] | .progress? // empty] | length'}
`)
	scope := vars.NewScope(vars.Layer{Name: "env", Vars: map[string]string{
		"base": srv.URL, "ws": "ws" + strings.TrimPrefix(srv.URL, "http"), "token": "secret",
	}})
	it, err := ws.Lookup("ai")
	if err != nil {
		t.Fatal(err)
	}
	if got := Inputs(ws, it); len(got) != 1 || got[0] != "file" {
		t.Fatalf("inputs = %v", got)
	}
	captured := map[string]string{}
	r := &Runner{WS: ws, Scope: scope, Emit: func(e Event) {
		for k, v := range e.Vars {
			captured[k] = v
		}
	}}
	if _, err := r.Run(context.Background(), it, map[string]string{"file": asset}); err != nil {
		t.Fatal(err)
	}
	if captured["asset_id"] != "42" || captured["content"] != "pixels" || captured["progress_seen"] != "1" {
		t.Fatalf("captured = %v", captured)
	}
}

func TestStatusError(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	ws, _ := spec.Init(t.TempDir())
	req := &spec.Request{Name: "x", Method: "POST", URL: srv.URL + "/uploads"}
	r := &Runner{WS: ws, Scope: vars.NewScope()}
	res, err := r.Run(context.Background(), spec.Item{Kind: spec.KindRequest, Name: "x", Request: req}, nil)
	if err == nil || res == nil || res.StatusCode != 401 {
		t.Fatalf("want 401 error, got %v %v", res, err)
	}
}

func TestCollectionSettings(t *testing.T) {
	ws, _ := spec.Init(t.TempDir())
	os.MkdirAll(ws.CollectionDir("svc"), 0o755)
	os.WriteFile(ws.CollectionPath("svc"), []byte("vars: {base_url: 'https://svc.{{domain}}'}\nheaders: {Authorization: coll, X-Shared: s}\n"), 0o644)
	os.WriteFile(ws.RequestPath("svc/r"), []byte("url: '{{base_url}}/a'\nheaders: {authorization: mine}\n"), 0o644)
	req, _ := ws.LoadRequest("svc/r")
	scope := vars.NewScope(
		vars.Layer{Name: "state", Vars: map[string]string{}},
		vars.Layer{Name: "env:dev", Vars: map[string]string{"base_url": "https://other", "domain": "test"}},
	)
	p, err := PrepareIn(ws, req, scope)
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://svc.test/a" {
		t.Fatalf("collection var should beat env: %s", p.URL)
	}
	if p.Header("Authorization") != "mine" || p.Header("X-Shared") != "s" {
		t.Fatalf("headers: %+v", p.Headers)
	}
}

// A rotated secret: the cached token is stale, the server answers 401, the
// runner re-runs the secret command and retries with the new value.
func TestSecretRotation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	ws, _ := spec.Init(t.TempDir())
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("old-token"), 0o644)
	os.WriteFile(filepath.Join(ws.Dir, secrets.FileName), []byte("tok: {command: 'cat "+tokenFile+"'}\n"), 0o644)
	store := secrets.Open(ws.Dir)
	vars.Secrets = store
	defer func() { vars.Secrets = nil }()
	if v, _ := store.Get("tok"); v != "old-token" {
		t.Fatalf("seed cache: %q", v)
	}
	os.WriteFile(tokenFile, []byte("new-token"), 0o644) // rotation

	req := &spec.Request{Name: "x", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer {{$secret.tok}}"}}
	var logs []string
	r := &Runner{WS: ws, Scope: vars.NewScope(), Emit: func(e Event) { logs = append(logs, e.Text) }}
	res, err := r.Run(context.Background(), spec.Item{Kind: spec.KindRequest, Name: "x", Request: req}, nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("want 200 after refresh, got %v %v\n%s", res, err, strings.Join(logs, "\n"))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "refreshing secrets tok") {
		t.Fatalf("no refresh logged:\n%s", strings.Join(logs, "\n"))
	}
}

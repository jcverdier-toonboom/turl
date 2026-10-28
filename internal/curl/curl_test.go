package curl

import (
	"testing"

	"github.com/jcverdier/turl/internal/runner"
	"github.com/jcverdier/turl/internal/vars"
)

func TestImport(t *testing.T) {
	r, err := Import(`curl 'https://api.example.com/v1/items?x=1' \
  -X PATCH -H 'Authorization: Bearer abc' -H "Content-Type: application/json" \
  --data-raw '{"name":"it'\''s"}' --compressed -k`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "PATCH" || r.URL != "https://api.example.com/v1/items?x=1" {
		t.Fatalf("method/url: %s %s", r.Method, r.URL)
	}
	if r.Headers["Authorization"] != "Bearer abc" || r.Headers["Content-Type"] != "application/json" {
		t.Fatalf("headers: %v", r.Headers)
	}
	if r.Body != `{"name":"it's"}` || !r.Insecure {
		t.Fatalf("body: %q insecure=%v", r.Body, r.Insecure)
	}
}

func TestImportDefaults(t *testing.T) {
	r, err := Import(`curl -d a=1 -d b=2 https://x.test/form`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "POST" || r.Body != "a=1&b=2" || r.Headers["Content-Type"] != "application/x-www-form-urlencoded" {
		t.Fatalf("%+v", r)
	}
	r, err = Import(`curl -T ./file.bin https://bucket.test/obj`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "PUT" || r.BodyFile != "./file.bin" {
		t.Fatalf("%+v", r)
	}
}

func TestRoundTrip(t *testing.T) {
	r, err := Import(`curl -X PUT -H 'X-A: 1' --data-raw 'it'\''s' https://x.test/p`)
	if err != nil {
		t.Fatal(err)
	}
	p, err := runner.Prepare(r, vars.NewScope(), "")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Import(Export(p))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Method != "PUT" || r2.Body != "it's" || r2.Headers["X-A"] != "1" || r2.URL != r.URL {
		t.Fatalf("round trip mismatch: %+v", r2)
	}
}

func TestImportArgsAndName(t *testing.T) {
	r, err := ImportArgs([]string{"-X", "POST", "-H", "Content-Type: application/json", "-d", `{"a":1}`, "https://api.test/v1/uploads?x=1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := SuggestName(r); got != "post-v1-uploads" {
		t.Fatalf("name = %q", got)
	}
	r, _ = ImportArgs([]string{"wss://rt.test/"})
	if got := SuggestName(r); got != "ws-rt-test" {
		t.Fatalf("ws name = %q", got)
	}
}

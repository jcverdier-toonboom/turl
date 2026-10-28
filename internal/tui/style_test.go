package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPrettyJSON(t *testing.T) {
	out, ok := prettyJSON([]byte(`{"b":1,"a":[1,{"x":null},[]],"e":{},"s":"<&>"} 3`))
	if !ok {
		t.Fatal("not ok")
	}
	want := `{
  "b": 1,
  "a": [
    1,
    {
      "x": null
    },
    []
  ],
  "e": {},
  "s": "<&>"
}
3`
	if got := ansi.Strip(out); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if _, ok := prettyJSON([]byte(`{"a":`)); ok {
		t.Fatal("truncated JSON accepted")
	}
	if _, ok := prettyJSON([]byte(`hello`)); ok {
		t.Fatal("text accepted")
	}
}

func TestPagerSearch(t *testing.T) {
	var p pager
	p.SetSize(10, 3)
	p.SetContent("alpha\nbeta\ngamma\ndelta Beta!", false)
	if n := p.Search("beta"); n != 2 {
		t.Fatalf("smartcase matches = %d", n)
	}
	if n := p.Search("Beta"); n != 1 {
		t.Fatalf("case-sensitive matches = %d", n)
	}
	// "delta Beta" wraps at width 10 into two lines.
	if len(p.lines) != 5 {
		t.Fatalf("wrapped lines = %d", len(p.lines))
	}
}

package vars

import (
	"errors"
	"testing"
)

func TestRender(t *testing.T) {
	s := NewScope(
		Layer{Name: "state", Vars: map[string]string{"token": "abc"}},
		Layer{Name: "env", Vars: map[string]string{"base": "https://h", "url": "{{base}}/v1", "token": "env"}},
	)
	got, err := s.Render("{{ url }}/x?t={{token}}")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://h/v1/x?t=abc" {
		t.Fatalf("got %q", got)
	}
	_, err = s.Render("{{nope}} {{nada}}")
	var me *MissingError
	if !errors.As(err, &me) || len(me.Names) != 2 {
		t.Fatalf("want missing error, got %v", err)
	}
	if out, _ := s.Render("{{$sh: echo hi}}"); out != "hi" {
		t.Fatalf("sh: %q", out)
	}
}

func TestChildPriority(t *testing.T) {
	s := NewScope(Layer{Name: "env", Vars: map[string]string{"a": "1"}}).Child("step", map[string]string{"a": "2"})
	if v, src, _ := s.Lookup("a"); v != "2" || src != "step" {
		t.Fatalf("got %s from %s", v, src)
	}
}

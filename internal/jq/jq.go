// Package jq wraps gojq so filters behave like `jq -r` on response bodies.
package jq

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/itchyny/gojq"
)

// Vars available in every filter.
var varNames = []string{"$status", "$headers", "$all"}

// Env supplies the extra variables.
type Env struct {
	Status  int
	Headers map[string]string
	All     []any // every WebSocket message
}

func (e Env) values() []any {
	h := map[string]any{}
	for k, v := range e.Headers {
		h[strings.ToLower(k)] = v
	}
	all := e.All
	if all == nil {
		all = []any{}
	}
	return []any{e.Status, h, all}
}

// Compile parses a filter.
func Compile(expr string) (*gojq.Code, error) {
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, err
	}
	return gojq.Compile(q, gojq.WithVariables(varNames), gojq.WithEnvironLoader(os.Environ))
}

// Run evaluates code against input and returns all outputs.
func Run(code *gojq.Code, input any, env Env) ([]any, error) {
	iter := code.Run(input, env.values()...)
	var out []any
	for {
		v, ok := iter.Next()
		if !ok {
			return out, nil
		}
		if err, ok := v.(error); ok {
			if err, ok := err.(interface{ IsEmptyError() bool }); ok && err.IsEmptyError() {
				continue
			}
			return out, err
		}
		out = append(out, v)
	}
}

// Eval compiles and runs in one go.
func Eval(expr string, input any, env Env) ([]any, error) {
	code, err := Compile(expr)
	if err != nil {
		return nil, err
	}
	return Run(code, input, env)
}

// Decode parses body as a stream of JSON values (like jq does). If the body
// is not JSON it is returned as a single string value and ok is false.
func Decode(body []byte) (vals []any, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		var v any
		err := dec.Decode(&v)
		if err == io.EOF {
			break
		}
		if err != nil {
			return []any{string(body)}, false
		}
		vals = append(vals, normalize(v))
	}
	if len(vals) == 0 {
		return []any{string(body)}, false
	}
	return vals, true
}

// DecodeOne returns the first JSON value of body, or the body as a string.
func DecodeOne(body []byte) any {
	vals, _ := Decode(body)
	return vals[0]
}

// normalize converts whole floats to ints so `.id` prints 12 not 1.2e+01.
func normalize(v any) any {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) && x < 1<<53 && x > -(1<<53) {
			return int(x)
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = normalize(x[k])
		}
		return x
	}
	return v
}

// ToString renders a value like `jq -r`: raw strings, compact JSON otherwise.
func ToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return string(Marshal(v, false))
}

// Marshal encodes without HTML escaping.
func Marshal(v any, indent bool) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return []byte(fmt.Sprint(v))
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Truthy follows jq semantics: everything except false and null.
func Truthy(v any) bool {
	return v != nil && v != false
}

// Format renders results for display: raw strings, indented JSON otherwise.
func Format(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		if s, ok := v.(string); ok {
			parts[i] = s
		} else {
			parts[i] = string(Marshal(v, true))
		}
	}
	return strings.Join(parts, "\n")
}

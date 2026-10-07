package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// spore.JSONGet and spore.JSONShape answer in the child, with no tool call.
// A program that guesses an API's struct shape wrong gets zero values back,
// not an error, and then panics on an empty slice far from the cause; that
// was the most common go_run failure measured on a local model. These two
// let a program see the shape, and read a value by path that fails loudly
// with what is actually there.

const (
	shapeMaxDepth = 12
	shapeMaxKeys  = 40
	shapeMaxBytes = 8 << 10
	sampleRunes   = 40
	notJSONRunes  = 120
)

func notJSON(body string, err error) error {
	return fmt.Errorf("body is not JSON (%v); it starts with: %q", err, clip(strings.TrimSpace(body), notJSONRunes))
}

// jsonGet returns the value at path in body: keys and array indexes joined
// by dots ("a.b.0.c"), or with brackets ("a.b[0].c"). A negative index
// counts from the end, so -1 is the last element. An empty path, or "$",
// is the whole document. Numbers come back as float64.
func jsonGet(body, path string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return nil, notJSON(body, err)
	}
	at := ""
	for _, seg := range splitPath(path) {
		switch cur := v.(type) {
		case map[string]any:
			next, ok := cur[seg]
			if !ok {
				if _, err := strconv.Atoi(seg); err == nil {
					return nil, fmt.Errorf("%s is an object, not an array; keys there: %s", where(at), keyList(body, at, cur))
				}
				return nil, fmt.Errorf("no key %q at %s; keys there: %s", seg, where(at), keyList(body, at, cur))
			}
			v, at = next, joinKey(at, seg)
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil {
				return nil, fmt.Errorf("%s is an array (length %d), not an object; index it with a number", where(at), len(cur))
			}
			if i < 0 {
				i += len(cur)
			}
			if i < 0 || i >= len(cur) {
				return nil, fmt.Errorf("index %s out of range at %s (length %d)", seg, where(at), len(cur))
			}
			v, at = cur[i], fmt.Sprintf("%s[%d]", at, i)
		default:
			want := "an object"
			if _, err := strconv.Atoi(seg); err == nil {
				want = "an array"
			}
			return nil, fmt.Errorf("%s is %s, not %s", where(at), describe(cur), want)
		}
	}
	return v, nil
}

func splitPath(path string) []string {
	path = strings.TrimPrefix(strings.TrimSpace(path), "$")
	path = strings.NewReplacer("[", ".", "]", "").Replace(path)
	var segs []string
	for _, s := range strings.Split(path, ".") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

func joinKey(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

func where(at string) string {
	if at == "" {
		return "the top level"
	}
	return at
}

// keyList names an object's keys in document order, which a map has lost;
// it re-reads the object from body to recover the order.
func keyList(body, at string, obj map[string]any) string {
	keys := orderedKeysAt(body, at)
	if len(keys) != len(obj) {
		keys = keys[:0]
		for k := range obj {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return "(none, the object is empty)"
	}
	if len(keys) > shapeMaxKeys {
		return strings.Join(keys[:shapeMaxKeys], ", ") + fmt.Sprintf(", … %d more", len(keys)-shapeMaxKeys)
	}
	return strings.Join(keys, ", ")
}

func orderedKeysAt(body, at string) []string {
	n, err := parseOrdered(body)
	if err != nil {
		return nil
	}
	for _, seg := range splitPath(at) {
		switch n.kind {
		case '{':
			found := false
			for i, k := range n.keys {
				if k == seg {
					n, found = n.vals[i], true
					break
				}
			}
			if !found {
				return nil
			}
		case '[':
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(n.vals) {
				return nil
			}
			n = n.vals[i]
		default:
			return nil
		}
	}
	return n.keys
}

func describe(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("a string (%q)", clip(x, sampleRunes))
	case float64:
		return "a number (" + strconv.FormatFloat(x, 'g', -1, 64) + ")"
	case bool:
		return fmt.Sprintf("a bool (%v)", x)
	case []any:
		return fmt.Sprintf("an array (length %d)", len(x))
	default:
		return "an object"
	}
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// node is a JSON value that keeps object keys in document order.
type node struct {
	kind   byte // '{', '[', 's' string, 'n' number, 'b' bool, '0' null
	keys   []string
	vals   []*node // object values, or array elements
	scalar string
}

func parseOrdered(body string) (*node, error) {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	return parseNode(dec)
}

func parseNode(dec *json.Decoder) (*node, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch x := t.(type) {
	case json.Delim:
		// The decoder hands parseNode only an opening delimiter here.
		n := &node{kind: '['}
		if x == '{' {
			n.kind = '{'
		}
		for dec.More() {
			if x == '{' {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, fmt.Sprint(k))
			}
			v, err := parseNode(dec)
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, v)
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return nil, err
		}
		return n, nil
	case string:
		return &node{kind: 's', scalar: x}, nil
	case json.Number:
		return &node{kind: 'n', scalar: x.String()}, nil
	case bool:
		return &node{kind: 'b', scalar: strconv.FormatBool(x)}, nil
	default:
		return &node{kind: '0'}, nil
	}
}

// jsonShape outlines body: every key in document order with its type, a
// sample of each scalar, and each array's length with the shape of its
// first element. It is bounded in depth, keys per object and total size,
// so it is safe to print for any response.
func jsonShape(body string) string {
	n, err := parseOrdered(body)
	if err != nil {
		return notJSON(body, err).Error() + "\n"
	}
	var b bytes.Buffer
	renderShape(&b, n, 0, true)
	b.WriteByte('\n')
	if b.Len() > shapeMaxBytes {
		return string(b.Bytes()[:shapeMaxBytes]) + "\n… (shape cut)\n"
	}
	return b.String()
}

func renderShape(b *bytes.Buffer, n *node, depth int, sample bool) {
	indent := strings.Repeat("  ", depth)
	switch n.kind {
	case '{':
		if len(n.keys) == 0 {
			b.WriteString("{}")
			return
		}
		if depth >= shapeMaxDepth {
			b.WriteString("{…}")
			return
		}
		b.WriteString("{\n")
		for i, k := range n.keys {
			if i == shapeMaxKeys {
				fmt.Fprintf(b, "%s  … %d more keys\n", indent, len(n.keys)-shapeMaxKeys)
				break
			}
			b.WriteString(indent + "  " + k + ": ")
			renderShape(b, n.vals[i], depth+1, true)
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case '[':
		if len(n.vals) == 0 {
			b.WriteString("[]")
			return
		}
		if depth >= shapeMaxDepth {
			fmt.Fprintf(b, "[%d × …]", len(n.vals))
			return
		}
		for _, v := range n.vals[1:] {
			if v.kind != n.vals[0].kind {
				fmt.Fprintf(b, "[%d × mixed types; first: ", len(n.vals))
				renderShape(b, n.vals[0], depth, true)
				b.WriteString("]")
				return
			}
		}
		fmt.Fprintf(b, "[%d × ", len(n.vals))
		renderShape(b, n.vals[0], depth, false)
		b.WriteString("]")
	case 's':
		b.WriteString("string")
		if sample {
			fmt.Fprintf(b, " %q", clip(n.scalar, sampleRunes))
		}
	case 'n':
		b.WriteString("number")
		if sample {
			b.WriteString(" " + n.scalar)
		}
	case 'b':
		b.WriteString("bool")
		if sample {
			b.WriteString(" " + n.scalar)
		}
	default:
		b.WriteString("null")
	}
}

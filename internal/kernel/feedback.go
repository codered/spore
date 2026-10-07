package kernel

import (
	"fmt"
	"go/ast"
	"regexp"
	"strconv"
	"strings"
)

// This file turns the interpreter's failures into errors a model can act on.
// Measured on a local model, most failed go_run calls were retried blind:
// a panic named no usable line, and a missing builtin came back after the
// prompt had already warned about it.

// approximate is appended to every panic. yaegi prints a position with a
// panic, but it marks the last call the function made, not the failing
// expression, and a model that trusts it fixes the wrong line.
const approximate = "The position printed with a panic is approximate: it can mark an earlier call in the same function, not the failing expression."

// panicHints match a panic value, as yaegi words it, to what usually causes
// it. The first match wins.
var panicHints = []struct{ match, hint string }{
	{"index out of", "An index or slice bound was past the end: a slice, array or string was shorter than the code assumed, often because a decode, split or regexp found less than expected. Print len() and a sample of the input before indexing."},
	{"slice bounds out of range", "An index or slice bound was past the end: a slice, array or string was shorter than the code assumed. Print len() and a sample of the input before indexing."},
	{"nil map", "A nil map was written to: create it with make(map[K]V) first."},
	{"nil pointer", "A nil pointer was dereferenced. Check the pointer, and the error returned with it, before use."},
	{"on zero Value", "A nil pointer was dereferenced (a field read through nil). Check the pointer, and the error returned with it, before use."},
	{"interface conversion", "A type assertion failed. JSON decoded into any holds map[string]any, []any, float64, string and bool; use the comma-ok form v, ok := x.(T) and check ok."},
	{"divide by zero", "An integer was divided by zero: check the divisor first."},
}

// explainPanic words a panic for the model: its value, what usually causes
// it, and a warning about the position.
func explainPanic(value string) string {
	var b strings.Builder
	b.WriteString("panic: " + value)
	for _, h := range panicHints {
		if strings.Contains(value, h.match) {
			b.WriteString("\n" + h.hint)
			break
		}
	}
	b.WriteString("\n" + approximate)
	return b.String()
}

// orderedName is the constraint the supplied min and max share. Its name
// appears in yaegi's error when inference fails, which explainError replaces.
const orderedName = "sporeOrdered"

var builtinShims = map[string]string{
	"min": `func min[T ` + orderedName + `](x T, ys ...T) T {
	for _, y := range ys {
		if y < x {
			x = y
		}
	}
	return x
}`,
	"max": `func max[T ` + orderedName + `](x T, ys ...T) T {
	for _, y := range ys {
		if y > x {
			x = y
		}
	}
	return x
}`,
}

// shims returns declarations for the min and max builtins yaegi v0.16.1
// lacks, for whichever of them the program calls without declaring. They
// go after the program, so no line the model wrote changes number.
func shims(f *ast.File) string {
	var b strings.Builder
	for _, name := range []string{"min", "max"} {
		if callsUnresolved(f, name) {
			b.WriteString("\n\n" + builtinShims[name])
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\ntype " + orderedName + " interface {\n\t~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr | ~float32 | ~float64 | ~string\n}" + b.String() + "\n"
}

// callsUnresolved reports whether the program calls name with no
// declaration of its own in scope. Obj is nil only for an identifier the
// parser could not resolve in the file, which is how a builtin looks.
func callsUnresolved(f *ast.File, name string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name && id.Obj == nil {
				found = true
			}
		}
		return !found
	})
	return found
}

var untypedShimRE = regexp.MustCompile(`untyped (\w+) does not implement main\.` + orderedName)

// explainError makes a compile, parse or runtime error easier to act on:
// it explains a failed inference on the supplied min or max, and quotes the
// line the error names. src is the program as the model wrote it.
func explainError(src, msg string) string {
	msg = untypedShimRE.ReplaceAllString(msg,
		"min and max cannot infer a type from untyped $1 constants alone here; give one argument a type, e.g. float64(1.5)")
	return quoteLine(src, msg)
}

var posRE = regexp.MustCompile(`^(?:panic: )?(?:main\.go:)?(\d+):(\d+): `)

// quoteLine appends the source line msg's leading line:col names, with a
// caret under the column. Small models count lines badly; a quoted line is
// fixed on the first try more often than a bare number. msg is returned
// unchanged when it names no line in src.
func quoteLine(src, msg string) string {
	m := posRE.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	line, _ := strconv.Atoi(m[1])
	col, _ := strconv.Atoi(m[2])
	lines := strings.Split(src, "\n")
	if line < 1 || line > len(lines) {
		return msg
	}
	text := strings.TrimRight(lines[line-1], "\r")
	// Columns count bytes. Keep the line's tabs in the caret's indent so the
	// caret sits under the column however tabs render.
	var pad strings.Builder
	for i := 0; i < col-1 && i < len(text); i++ {
		if text[i] == '\t' {
			pad.WriteByte('\t')
		} else {
			pad.WriteByte(' ')
		}
	}
	gutter := fmt.Sprintf("%5d | ", line)
	// The quote goes after the error's first line, ahead of anything that
	// follows it, such as a panic's hint.
	first, rest, _ := strings.Cut(msg, "\n")
	out := first + "\n" + gutter + text + "\n" + strings.Repeat(" ", len(gutter)-2) + "| " + pad.String() + "^"
	if rest != "" {
		out += "\n" + rest
	}
	return out
}

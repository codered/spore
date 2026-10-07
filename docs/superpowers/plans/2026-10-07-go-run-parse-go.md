# go_run Parses Go — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `go_run` programs import `go/ast`, `go/format`, `go/parser`, `go/printer`, `go/scanner` and `go/token`, with every path by which `go/parser` reads disk closed.

**Architecture:** `kernel.Allowed` gains the six packages. `surface` (child side) copies their yaegi stdlib symbol maps and applies an override table: `parser.ParseFile` / `parser.ParseExprFrom` refuse a nil `src`, `parser.ParseDir` is removed, `ast.Print` writes to the program's capped output. `explainError` (parent side) rewrites yaegi's missing-`ParseDir` error. `Reference` gains one sentence.

**Tech Stack:** Go, yaegi v0.16.1 (`github.com/traefik/yaegi/stdlib` symbol maps).

**Spec:** `docs/superpowers/specs/2026-10-07-go-run-parse-go-design.md`

## Global Constraints

- Work in `/home/code/development/spore-parse-go` (branch `go-run-parse-go`). Never touch `/home/code/development/spore`.
- Tests: `go test -tags sqlite_fts5 ./internal/kernel/...` (the tag is required for store-backed packages; harmless here).
- Never mutate `stdlib.Symbols` in place: clone a package's map before overriding (the test binary shares the global).
- Nil-src refusal text, verbatim: `go_run cannot read files through go/parser: read the file with spore.ReadFile and pass its text as src`
- ParseDir rewrite text, verbatim: `go/parser.ParseDir is not available in go_run: list files with spore.Glob and parse each with parser.ParseFile, passing the text from spore.ReadFile`
- Keep `Allowed` sorted.
- Gate before every commit: `gofmt -l internal/` prints nothing, `go vet -tags sqlite_fts5 ./internal/kernel/`, `go test -tags sqlite_fts5 ./internal/kernel/`.
- Never strip or rewrite arguments with sed patterns; edit the lines by hand.
- Report test output you actually captured. "Ran X, passed, output not captured" is an acceptable report; reconstructed output is not.

## Review Focus

1. A non-nil `src` of another type (`[]byte`, `strings.NewReader`) must parse, not be refused — pinned in Task 1, test `TestParserAcceptsEverySrcForm`.
2. `go/parser` imported under an alias (`p "go/parser"`): yaegi names the alias in its error, so the ParseDir rewrite must match any alias — pinned in Task 1, `TestParseDirIsUnavailable` runs both spellings.
3. `ast.Print` output larger than `max_output` must be cut and flagged truncated, not bypass the cap — pinned in Task 1, `TestASTPrintIsCapped`.
4. The refusal must not echo file content through the error (the probe saw `ParseFile` leak a fragment) — pinned in Task 1, `TestParserRefusesNilSrc` checks a sentinel.
5. A future yaegi adding a `go/*` function — pinned in Task 1, `TestGoSymbolsAreReviewed`.

---

### Task 1: Surface, overrides, ParseDir message, tests

**Files:**
- Modify: `internal/kernel/child.go` (imports; `Allowed`; `ChildMain`'s `surface` call; `surface`; new `overrides`, `errParserReadsDisk`)
- Modify: `internal/kernel/feedback.go` (`explainError`)
- Create: `internal/kernel/goparse_test.go`

**Interfaces:**
- Produces: `func surface(ch *child, out io.Writer) interp.Exports` (was `surface(ch *child)`); `func overrides(out io.Writer) map[string]map[string]reflect.Value`.

- [ ] **Step 1: Write the failing tests**

Create `internal/kernel/goparse_test.go`:

```go
package kernel

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
)

const parseSample = "package p\n\nfunc a() {\n\tx := 1\n\t_ = x\n}\n\nfunc (s *S) b() {}\n"

// The whole point: read through spore.ReadFile, which policy judges, then
// parse the text.
func TestGoParsingWorks(t *testing.T) {
	r := &fakeRunner{fn: func(_ context.Context, call provider.Block) provider.Block {
		return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: parseSample}
	}}
	src := prog(`func main() {
	text, err := spore.ReadFile("p.go")
	if err != nil {
		fmt.Println(err)
		return
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", text, 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok {
			fmt.Printf("%s %d-%d\n", fd.Name.Name, fset.Position(fd.Pos()).Line, fset.Position(fd.End()).Line)
		}
		return true
	})
}`, "fmt", "go/ast", "go/parser", "go/token", "spore")
	res, err := Run(context.Background(), src, r, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "a 3-6\nb 8-8\n" {
		t.Errorf("Output = %q", res.Output)
	}
	calls := r.seen()
	if len(calls) != 1 || calls[0].Name != "fs_read" {
		t.Fatalf("calls = %+v, want one fs_read", calls)
	}
}

func TestParserAcceptsEverySrcForm(t *testing.T) {
	src := prog(`func main() {
	text := "package p\nfunc f() {}\n"
	fset := token.NewFileSet()
	for _, s := range []any{text, []byte(text), strings.NewReader(text)} {
		f, err := parser.ParseFile(fset, "p.go", s, 0)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println(f.Name.Name)
	}
	e, err := parser.ParseExprFrom(fset, "e", []byte("a+b"), 0)
	fmt.Println(e != nil, err)
}`, "fmt", "go/parser", "go/token", "strings")
	res, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "p\np\np\ntrue <nil>\n" {
		t.Errorf("Output = %q", res.Output)
	}
}

// With a nil src the real functions read the named file. The probe saw the
// error echo a fragment of it, so the sentinel must appear nowhere.
func TestParserRefusesNilSrc(t *testing.T) {
	const sentinel = "SENTINEL_kernel_must_not_read_this"
	path := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(path, []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := prog(`func main() {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, PATH, nil, 0)
	fmt.Println("file:", err)
	_, err = parser.ParseExprFrom(fset, PATH, nil, 0)
	fmt.Println("expr:", err)
}`, "fmt", "go/parser", "go/token")
	src = strings.ReplaceAll(src, "PATH", strconvQuote(path))
	res, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	const refusal = "go_run cannot read files through go/parser: read the file with spore.ReadFile and pass its text as src"
	want := "file: " + refusal + "\nexpr: " + refusal + "\n"
	if res.Output != want {
		t.Errorf("Output = %q, want %q", res.Output, want)
	}
	if strings.Contains(res.Output, sentinel) {
		t.Errorf("file content leaked into output: %q", res.Output)
	}
}

func TestParseDirIsUnavailable(t *testing.T) {
	const want = "go/parser.ParseDir is not available in go_run: list files with spore.Glob and parse each with parser.ParseFile, passing the text from spore.ReadFile"
	for _, imp := range []string{`"go/parser"`, `gp "go/parser"`} {
		name := "parser"
		if strings.HasPrefix(imp, "gp") {
			name = "gp"
		}
		src := "package main\n\nimport (\n\t\"fmt\"\n\t" + imp + "\n\t\"go/token\"\n)\n\nfunc main() {\n\tfset := token.NewFileSet()\n\tpk, err := " + name + ".ParseDir(fset, \".\", nil, 0)\n\tfmt.Println(len(pk), err)\n}\n"
		res, err := Run(context.Background(), src, &fakeRunner{}, opts())
		if err == nil {
			t.Fatalf("%s: ParseDir ran: %q", imp, res.Output)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %q, want it to contain %q", imp, err, want)
		}
	}
}

func TestASTPrintReachesOutput(t *testing.T) {
	src := prog(`func main() {
	e, _ := parser.ParseExpr("x")
	if err := ast.Print(nil, e); err != nil {
		fmt.Println(err)
	}
}`, "fmt", "go/ast", "go/parser")
	res, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, `Name: "x"`) {
		t.Errorf("Output = %q, want ast.Print's dump", res.Output)
	}
}

func TestASTPrintIsCapped(t *testing.T) {
	src := prog(`func main() {
	f, _ := parser.ParseFile(token.NewFileSet(), "p.go", "package p\nfunc a() { b(); c(); d() }\n", 0)
	ast.Print(nil, f)
}`, "go/ast", "go/parser", "go/token")
	o := opts()
	o.MaxOutput = 64
	res, err := Run(context.Background(), src, &fakeRunner{}, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Output) > 64 || !res.Truncated {
		t.Errorf("len(Output) = %d, Truncated = %v; want <= 64 and true", len(res.Output), res.Truncated)
	}
}

// Every function a program can call in the go/* packages, after overrides.
// A yaegi upgrade that adds one fails here until someone has checked
// whether it reads disk; add it to this list only after that check.
func TestGoSymbolsAreReviewed(t *testing.T) {
	want := map[string][]string{
		"go/ast/ast": {"FileExports", "FilterDecl", "FilterFile", "FilterPackage", "Fprint", "Inspect",
			"IsExported", "IsGenerated", "MergePackageFiles", "NewCommentMap", "NewIdent", "NewObj",
			"NewPackage", "NewScope", "NotNilFilter", "PackageExports", "Print", "SortImports", "Unparen", "Walk"},
		"go/format/format":   {"Node", "Source"},
		"go/parser/parser":   {"ParseExpr", "ParseExprFrom", "ParseFile"},
		"go/printer/printer": {"Fprint"},
		"go/scanner/scanner": {"PrintError"},
		"go/token/token":     {"IsExported", "IsIdentifier", "IsKeyword", "Lookup", "NewFileSet"},
	}
	ex := surface(&child{}, io.Discard)
	for key, names := range want {
		var got []string
		for name, v := range ex[key] {
			if v.Kind() == reflect.Func {
				got = append(got, name)
			}
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, names) {
			t.Errorf("%s functions = %v, want %v", key, got, names)
		}
	}
	for key := range ex {
		if strings.HasPrefix(key, "go/") {
			if _, ok := want[key]; !ok {
				t.Errorf("%s is exported to programs but not reviewed here", key)
			}
		}
	}
}

func strconvQuote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/kernel/ -run 'GoParsing|ParserAccepts|ParserRefuses|ParseDir|ASTPrint|GoSymbols' 2>&1 | tail -20`
Expected: build failure, `too many arguments in call to surface` (the snapshot test calls the new signature). That is the expected first failure.

- [ ] **Step 3: Implement the surface changes in `internal/kernel/child.go`**

3a. Add to the import block: `"go/ast"`, `"go/parser"`, `"go/token"`, `"io"`, `"maps"`.

3b. Replace the `Allowed` declaration's value with (comment above it unchanged):

```go
var Allowed = []string{
	"bytes", "encoding/base64", "encoding/csv", "encoding/hex", "encoding/json",
	"errors", "fmt", "go/ast", "go/format", "go/parser", "go/printer",
	"go/scanner", "go/token", "html", "math", "math/rand", "net/url", "regexp",
	"sort", "strconv", "strings", "sync", "sync/atomic", "text/tabwriter",
	"text/template", "time", "unicode", "unicode/utf8",
}
```

3c. Add directly below `Allowed`:

```go
var errParserReadsDisk = errors.New("go_run cannot read files through go/parser: read the file with spore.ReadFile and pass its text as src")

// overrides are the Allowed symbols a program gets changed, or not at all;
// a zero Value removes one. Given a nil src, go/parser's ParseFile and
// ParseExprFrom read the named file themselves, and ParseDir lists and
// reads a directory: each would be a file read no policy check sees.
// ast.Print writes to os.Stdout, which in the child goes nowhere, so it
// writes to the program's output instead.
func overrides(out io.Writer) map[string]map[string]reflect.Value {
	return map[string]map[string]reflect.Value{
		"go/parser/parser": {
			"ParseFile": reflect.ValueOf(func(fset *token.FileSet, filename string, src any, mode parser.Mode) (*ast.File, error) {
				if src == nil {
					return nil, errParserReadsDisk
				}
				return parser.ParseFile(fset, filename, src, mode)
			}),
			"ParseExprFrom": reflect.ValueOf(func(fset *token.FileSet, filename string, src any, mode parser.Mode) (ast.Expr, error) {
				if src == nil {
					return nil, errParserReadsDisk
				}
				return parser.ParseExprFrom(fset, filename, src, mode)
			}),
			"ParseDir": {},
		},
		"go/ast/ast": {
			"Print": reflect.ValueOf(func(fset *token.FileSet, x any) error {
				return ast.Fprint(out, fset, x, ast.NotNilFilter)
			}),
		},
	}
}
```

3d. In `ChildMain`, change `i.Use(surface(ch))` to `i.Use(surface(ch, w))`.

3e. In `surface`, change the signature and the copy loop:

```go
// surface is the whole symbol table a program sees. out is the program's
// output, for the overrides that print.
func surface(ch *child, out io.Writer) interp.Exports {
	ex := interp.Exports{}
	over := overrides(out)
	for _, p := range Allowed {
		key := p + "/" + path.Base(p)
		syms, ok := stdlib.Symbols[key]
		if !ok {
			continue
		}
		if o, ok := over[key]; ok {
			// stdlib.Symbols is shared by the whole process; never edit it.
			syms = maps.Clone(syms)
			for name, v := range o {
				if v.IsValid() {
					syms[name] = v
				} else {
					delete(syms, name)
				}
			}
		}
		ex[key] = syms
	}
```

(The rest of `surface`, from `ex["spore/spore"] = ...`, is unchanged.)

- [ ] **Step 4: Rewrite the ParseDir error in `internal/kernel/feedback.go`**

Add next to `untypedShimRE`:

```go
// parseDirRE matches yaegi's error for go/parser.ParseDir, which overrides
// removes; the import may be aliased.
var parseDirRE = regexp.MustCompile(`package \w+ "go/parser" has no symbol ParseDir`)
```

In `explainError`, before `return quoteLine(src, msg)`, add:

```go
	msg = parseDirRE.ReplaceAllLiteralString(msg,
		"go/parser.ParseDir is not available in go_run: list files with spore.Glob and parse each with parser.ParseFile, passing the text from spore.ReadFile")
```

Update the comment above `explainError` to say it also explains a call to `ParseDir`.

- [ ] **Step 5: Run the new tests**

Run: `go test -tags sqlite_fts5 ./internal/kernel/ -run 'GoParsing|ParserAccepts|ParserRefuses|ParseDir|ASTPrint|GoSymbols' -v 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL|PASS)'`
Expected: every test `--- PASS`. If `TestGoSymbolsAreReviewed` lists different names, STOP and report the exact list; do not edit the expected list to match.

- [ ] **Step 6: Gate and commit**

Run:
```bash
gofmt -l internal/
go vet -tags sqlite_fts5 ./internal/kernel/
go test -tags sqlite_fts5 ./internal/kernel/
```
Expected: `gofmt -l` prints nothing; vet silent; `ok`. `TestReferenceMatchesGolden` WILL fail now because the importable-packages line changed — that is fixed in Task 2. Every other test must pass. Then:

```bash
git add internal/kernel/child.go internal/kernel/feedback.go internal/kernel/goparse_test.go
git commit -m "kernel: let go_run import go/ast and a go/parser that cannot read disk"
```

- [ ] **Step 7: Mutation-check the three guarding tests**

Each change below is temporary. Make it, run the named test, confirm it FAILS, revert with `git checkout internal/kernel/child.go` (the tree was committed in Step 6, so this restores the committed version), and record the failure line in your report:
1. Delete the `"ParseFile": ...` and `"ParseExprFrom": ...` entries from `overrides` → `TestParserRefusesNilSrc` fails.
2. Delete the `"ParseDir": {},` entry → `TestParseDirIsUnavailable` fails.
3. Delete the `"go/ast/ast": {...}` entry → `TestASTPrintReachesOutput` fails.

After the last revert, `git status --short` must be empty: the tree is back at the Task 1 commit.

---

### Task 2: Prompt sentence, golden file, backlog

**Files:**
- Modify: `internal/kernel/reference.go:62-63`
- Modify: `internal/kernel/testdata/reference.golden` (regenerated)
- Modify: `docs/backlog.md` (the "go_run programs cannot parse Go" section)

**Interfaces:**
- Consumes: `Allowed` from Task 1 (now containing the six `go/*` packages).

- [ ] **Step 1: Add the sentence to `Reference`**

In `internal/kernel/reference.go`, directly after the `b.WriteString("Importable packages: " ...)` statement (which ends `use spore.* for files, network and shell.\n\n")`), add:

```go
	b.WriteString("go/parser parses text you pass in: read the file with spore.ReadFile " +
		"and pass its contents as src. ParseDir is unavailable (find files with " +
		"spore.Glob and parse each), and there is no type information (go/types is " +
		"absent). The parser understands current Go syntax even though your program " +
		"runs as Go 1.21.\n\n")
```

- [ ] **Step 2: Regenerate the golden file and inspect the diff**

Run: `go test -tags sqlite_fts5 ./internal/kernel/ -run TestReferenceMatchesGolden -update && git diff internal/kernel/testdata/reference.golden`
Expected: the diff shows exactly two changes — the six `go/*` names in the importable-packages line, and the new paragraph. Anything else: STOP and report.

- [ ] **Step 3: Close the backlog entry**

In `docs/backlog.md`, replace the heading `## go_run programs cannot parse Go` with `## go_run programs cannot parse Go: fixed`, replace the line `Open. Found during the #67 live run. Asked which functions in` with `Closed. Found during the #67 live run. Asked which functions in`, and replace everything from the line `Open questions:` to the end of that section (the end of the file) with:

```markdown
The three open questions are answered
(`docs/superpowers/specs/2026-10-07-go-run-parse-go-design.md`):

1. **Same-name wrappers.** Programs import the real `go/ast`, `go/format`,
   `go/parser`, `go/printer`, `go/scanner` and `go/token`. In `go/parser`,
   `ParseFile` and `ParseExprFrom` refuse a nil `src` -- the case in which
   they read the file themselves -- and `ParseDir` is absent. The model
   writes the Go it would have written anyway. `ast.Print` is pointed at the
   program's output, because `os.Stdout` goes nowhere in the child.
2. **yaegi runs it.** A probe ran `ast.Inspect` with a type switch,
   `ast.Walk` with an interpreted `Visitor`, `format.Node` and generics
   against real repository source; output matched native Go exactly. The
   probe also showed the nil-`src` read was real, and that its error echoed
   a fragment of the file.
3. **Go only.** No other language and no type information: `go/types`,
   `go/importer` and `go/build` would need disk.

A symbol snapshot test lists every function the six packages expose, so a
yaegi upgrade that adds one fails until someone checks it for disk access.
```

- [ ] **Step 4: Gate and commit**

Run:
```bash
gofmt -l internal/
go vet -tags sqlite_fts5 ./internal/kernel/
go test -tags sqlite_fts5 ./internal/kernel/
```
Expected: nothing from gofmt, vet silent, `ok` (golden now passes).

```bash
git add internal/kernel/reference.go internal/kernel/testdata/reference.golden docs/backlog.md
git commit -m "kernel: tell the model go/parser takes text, and close the backlog entry"
```

---

### After both tasks (controller, not a subagent)

1. Whole-branch review (sonnet).
2. Full CI step list in a detached worktree at HEAD: `go vet -tags sqlite_fts5 ./...`, `make fmtcheck`, `make lint`, `make vulncheck`, `make tidycheck`, `go test -tags sqlite_fts5 -race ./...`.
3. Live gate from the spec: build the branch binary, run `spore once` with stdin closed on "Which functions in internal/kernel are longer than 40 lines?", compare with a native count, and count `go_run` calls and any `shell_exec` attempts in the transcript.
4. Push the branch and open a PR.

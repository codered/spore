# go_run programs can parse Go

Date: 2026-10-07. Backlog entry: "go_run programs cannot parse Go".

## Problem

Asked which functions in `internal/kernel` are longer than 40 lines, the
model's first move in the #67 live run was a program importing `go/ast` and
`go/parser`. Neither is in `kernel.Allowed`. It tried `go run` through the
shell (declined), then writing a helper file (declined), then counted braces
by hand: 7 calls and up to 300 s, and one run hit the test cap before it
answered.

The packages cannot simply be added. `parser.ParseFile` and
`parser.ParseExprFrom` read the named file from disk when `src` is nil, and
`parser.ParseDir` lists and reads a directory, all straight through `os`. A
program could read any file the daemon can, with no `fs_read` call for policy
to judge.

## Probe (answers backlog question 2)

A throwaway harness built an interpreter the way `ChildMain` does (yaegi
v0.16.1, empty source filesystem, no GOPATH) and added the `go/*` packages.
Run against real repository source, everything a code question needs worked:

- `ast.Inspect` with a type switch, `fset.Position` line numbers, receiver
  unwrapping. Output was identical to the same program compiled natively:
  59 functions in the 1,548-line `internal/tui/app.go`, in 58 ms.
- `ast.Walk` with an interpreted type implementing `ast.Visitor`;
  `ast.FilterFile` with an interpreted closure; `ast.NewCommentMap`;
  `FuncDecl.Doc.Text()`.
- `format.Node` and `printer.Fprint` (print a signature or a body).
- Generics: type parameters on functions and types parse and are visible.
- A syntax error comes back as `file:line:col: message`.

And the two dangers were confirmed rather than assumed:

- `ParseFile(fset, "/etc/hostname", nil, 0)` read the file; its error echoed
  a fragment of the content (`expected 'package', found code`). An error
  message is a leak path, not only the AST.
- `ParseDir` walked `internal/kernel` and returned two packages.

A third finding: `ast.Print` writes to the process's real `os.Stdout`. The
kernel child's stdout is not connected, so its output would vanish silently.

The interpreter runs programs as Go 1.21, but the parser is the host's
compiled `go/parser` (Go 1.26), so files using newer syntax still parse.

## Decisions

1. **Go only (backlog question 3).** No general structured-text facility.
   Code questions about this repository are the common case; nothing else has
   been asked for.
2. **Same-name wrappers, not a new helper (backlog question 1).** The
   program imports the real `go/*` packages. `go/parser` reaches it with its
   disk paths closed. The model writes the Go it would write anyway, which is
   what the #67 run showed it reaching for. `spore.GoFuncs`-style helpers
   answer one question; a typed `spore.ParseGo` still needs `go/ast` and
   `go/token` to be useful and fails the model's first instinct.
3. **No type information.** `go/types`, `go/importer` and `go/build` stay out:
   type-checking needs an importer, and importers and `go/build` read disk.

## Design

### Surface

`Allowed` gains, in sorted position: `go/ast`, `go/format`, `go/parser`,
`go/printer`, `go/scanner`, `go/token`. `go/scanner` is there because a parse
error is a `scanner.ErrorList`.

After `surface` copies stdlib symbols, an override pass changes three
entries. The overrides sit in one table next to `Allowed`, each with a
comment saying why it exists:

| Symbol | In go_run |
|---|---|
| `go/parser.ParseFile` | Same signature. A nil `src` returns an error and reads nothing; any other `src` calls the real function. |
| `go/parser.ParseExprFrom` | Same nil-`src` guard. |
| `go/parser.ParseDir` | Absent. |
| `go/ast.Print` | Writes to the program's stdout (the capped `outWriter`), so the output reaches the model and counts against `max_output`. |

The nil-`src` error reads:

> go_run cannot read files through go/parser: read the file with
> spore.ReadFile and pass its text as src

A non-nil `src` of any type is passed through, `io.Reader` included: no
package a program can import opens a file, so a reader can only hold bytes
the program already had.

The wrapper decides on `src == nil` alone, before calling the real function,
so the refusal can never echo file content.

Of the other symbols, `go/printer` writes to `os.Stderr` only on an
unsupported argument type, a debug path; that lands in the child's stderr,
which the parent already captures and bounds. `scanner.PrintError` takes its
writer as an argument. Nothing else in the six packages touches `os`.

### Prompt

`Reference` already prints `Allowed`. One sentence follows the
"Importable packages" line:

> go/parser parses text you pass in: read the file with spore.ReadFile and
> pass its contents as src. ParseDir is unavailable (find files with
> spore.Glob and parse each), and there is no type information (go/types is
> absent). The parser understands current Go syntax even though your program
> runs as Go 1.21.

The section is part of the cached prompt prefix; changing it costs one cache
miss per session on rollout.

### Errors

yaegi reports a call to the removed symbol as
`6:12: package parser "go/parser" has no symbol ParseDir`. `explainError`
rewrites that phrase to:

> go/parser.ParseDir is not available in go_run: list files with spore.Glob
> and parse each with parser.ParseFile, passing the text from spore.ReadFile

The line number and the quoted source line `quoteLine` adds are kept.

## Testing

All through `kernel.Run` with `fakeRunner`, in `internal/kernel`:

1. **Parsing works end to end.** The fake answers `fs_read` with a Go
   snippet. The program reads it with `spore.ReadFile`, parses it, and prints
   each function's name and line span. Asserts the exact output and that the
   read reached the runner as `fs_read`.
2. **Nil src is refused and nothing leaks.** A temp file holds a sentinel
   string. `ParseFile(fset, path, nil, 0)` and
   `ParseExprFrom(fset, path, nil, 0)` each return the refusal; the program
   prints the error. Asserts the refusal text, and that the sentinel appears
   in neither the output nor the returned error.
3. **ParseDir is unavailable**, and the error carries the rewritten message.
4. **`ast.Print` reaches the output.**
5. **Symbol snapshot.** Lists every function symbol the six `go/*` packages
   expose to a program (after overrides) and compares it with a reviewed list
   in the test. A yaegi upgrade that adds a function fails the test until
   someone decides whether it reads disk.
6. `TestAllowedPackagesAreUsable` and the `Reference` golden file are
   updated.

Each of tests 2, 3 and 4 is mutation-checked: removing the override it
guards must turn it red.

**Live gate, required before the PR is opened.** Build the branch and ask,
through `spore once` with stdin closed: "Which functions in internal/kernel
are longer than 40 lines?" Passes if the answer is correct (checked against a
native `go/ast` count) in at most two `go_run` calls, with no `shell_exec` or
`go run` attempt.

## Out of scope

- Type information (`go/types`).
- Parsing languages other than Go.
- A convenience helper that reads and parses in one call.

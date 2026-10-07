package kernel

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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
	src = strings.ReplaceAll(src, "PATH", strconv.Quote(path))
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

// Only an untyped nil makes go/parser read the file. Every other empty or
// nil-valued src must stay off the disk, whatever error it produces.
func TestParserNeverReadsForNilValuedSrc(t *testing.T) {
	const sentinel = "SENTINEL_kernel_must_not_read_this"
	path := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(path, []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := prog(`func main() {
	fset := token.NewFileSet()
	var a any
	var b []byte
	var buf *bytes.Buffer
	for i, s := range []any{a, b, buf} {
		_, err := parser.ParseFile(fset, PATH, s, 0)
		fmt.Println(i, err)
	}
}`, "bytes", "fmt", "go/parser", "go/token")
	src = strings.ReplaceAll(src, "PATH", strconv.Quote(path))
	res, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.Output, sentinel) {
		t.Errorf("file content leaked: %q", res.Output)
	}
	if !strings.Contains(res.Output, "0 go_run cannot read files through go/parser") {
		t.Errorf("a nil any was not refused: %q", res.Output)
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

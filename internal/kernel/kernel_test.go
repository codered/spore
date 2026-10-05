package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/tool"
)

// The kernel re-executes its own binary as the child. Under `go test` that
// binary is the test binary, so TestMain must hand control to ChildMain
// before the test runner starts.
func TestMain(m *testing.M) {
	if IsChild() {
		os.Exit(ChildMain())
	}
	os.Exit(m.Run())
}

// fakeRunner answers helper calls. It records every call it serves.
type fakeRunner struct {
	mu    sync.Mutex
	calls []provider.Block
	fn    func(ctx context.Context, call provider.Block) provider.Block
}

func (f *fakeRunner) Run(ctx context.Context, call provider.Block) provider.Block {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, call)
	}
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "ran " + call.Name}
}

func (f *fakeRunner) seen() []provider.Block {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Block(nil), f.calls...)
}

func opts() Options {
	return Options{Timeout: 10 * time.Second, Ceiling: 30 * time.Second, MaxOutput: 1 << 20, HelperMax: 4 << 20}
}

func prog(body string, imports ...string) string {
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n")
	for _, i := range imports {
		fmt.Fprintf(&b, "\t%q\n", i)
	}
	b.WriteString(")\n\n")
	b.WriteString(body)
	return b.String()
}

func TestProgramOutputIsReturned(t *testing.T) {
	res, err := Run(context.Background(), prog(`func main() { fmt.Println("hi") }`, "fmt"), &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "hi\n" {
		t.Errorf("Output = %q, want %q", res.Output, "hi\n")
	}
}

func TestAllowedPackagesAreUsable(t *testing.T) {
	src := prog(`func main() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); time.Sleep(time.Millisecond) }()
	wg.Wait()
	b, _ := json.Marshal(map[string]int{"n": len(strings.Fields("a b c"))})
	fmt.Println(string(b), url.QueryEscape("a b"))
}`, "encoding/json", "fmt", "net/url", "strings", "sync", "time")
	res, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "{\"n\":3} a+b\n" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestForbiddenImportsAreRefusedWithAUsefulMessage(t *testing.T) {
	for _, pkg := range []string{"os", "os/exec", "net", "net/http", "unsafe", "syscall", "reflect"} {
		t.Run(pkg, func(t *testing.T) {
			src := fmt.Sprintf("package main\n\nimport _ %q\n\nfunc main() {}\n", pkg)
			_, err := Run(context.Background(), src, &fakeRunner{}, opts())
			if err == nil {
				t.Fatalf("importing %s was accepted", pkg)
			}
			want := fmt.Sprintf("package %q is not available in go_run", pkg)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to contain %q", err, want)
			}
			if strings.Contains(err.Error(), "GOPATH") {
				t.Errorf("err mentions GOPATH, which means nothing to the model: %q", err)
			}
		})
	}
}

// With $GOPATH pointing at real package source, yaegi would load it. The
// child must not look there: a source package could wrap anything.
func TestSourceImportsCannotBeLoaded(t *testing.T) {
	gopath := t.TempDir()
	pkgDir := filepath.Join(gopath, "src", "github.com", "x", "y")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "y.go"), []byte("package y\n\nfunc Hello() string { return \"loaded\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOPATH", gopath)
	src := prog(`func main() { fmt.Println(y.Hello()) }`, "fmt", "github.com/x/y")
	res, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err == nil || strings.Contains(res.Output, "loaded") {
		t.Fatalf("a source package was loaded from $GOPATH: out=%q err=%v", res.Output, err)
	}
}

func TestProgramMustBePackageMainWithMain(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"not main": {"package foo\n\nfunc main() {}\n", "package main"},
		"no main":  {"package main\n\nfunc helper() {}\n", "func main"},
		"syntax":   {"package main\n\nfunc main() { fmt.Println( }\n", "3:"},
		"method":   {"package main\n\ntype T struct{}\n\nfunc (T) main() {}\n", "func main"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := &fakeRunner{}
			_, err := Run(context.Background(), c.src, r, opts())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestInfiniteLoopTimesOut(t *testing.T) {
	o := opts()
	o.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := Run(context.Background(), prog(`func main() { for {} }`), &fakeRunner{}, o)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("timeout took %v", d)
	}
}

// Each of these kills an in-process yaegi host. In the child they must
// come back as an error, and this test process must still be running.
func TestCrashesDoNotKillTheParent(t *testing.T) {
	cases := map[string]string{
		"goroutine panic": prog(`func main() {
	go func() { panic("boom in goroutine") }()
	for i := 0; i < 1e9; i++ {}
}`),
		"stack overflow": prog(`func f() int { return f() + 1 }

func main() { f() }`),
		"nil map": prog(`func main() {
	var m map[string]int
	m["a"] = 1
}`),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			o := opts()
			o.Timeout = 20 * time.Second
			_, err := Run(context.Background(), src, &fakeRunner{}, o)
			if err == nil {
				t.Fatal("a crashing program returned no error")
			}
			if strings.Contains(err.Error(), "timed out") {
				t.Errorf("the crash was reported as a timeout: %v", err)
			}
			if len(err.Error()) > 600 {
				t.Errorf("the crash report carries a stack dump the model cannot use (%d bytes):\n%v", len(err.Error()), err)
			}
		})
	}
}

func TestOutputIsCappedAndFlagged(t *testing.T) {
	o := opts()
	o.MaxOutput = 100
	src := prog(`func main() { fmt.Print(strings.Repeat("x", 10000)) }`, "fmt", "strings")
	res, err := Run(context.Background(), src, &fakeRunner{}, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated || len(res.Output) > 100 {
		t.Errorf("truncated=%v len=%d, want true and <= 100", res.Truncated, len(res.Output))
	}
}

func TestOutputPrintedBeforeATimeoutIsKept(t *testing.T) {
	o := opts()
	o.Timeout = 500 * time.Millisecond
	res, err := Run(context.Background(), prog(`func main() { fmt.Println("before"); for {} }`, "fmt"), &fakeRunner{}, o)
	if err == nil {
		t.Fatal("want a timeout")
	}
	if res.Output != "before\n" {
		t.Errorf("Output = %q, want the line printed before the timeout", res.Output)
	}
}

func TestStderrIsCaptured(t *testing.T) {
	res, err := Run(context.Background(), prog(`func main() { panic("oops") }`), &fakeRunner{}, opts())
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("err = %v, want the panic value", err)
	}
	_ = res
}

// A human reading an approval must not use up the program's budget.
func TestBudgetPausesWhileAHelperIsInFlight(t *testing.T) {
	r := &fakeRunner{fn: func(_ context.Context, call provider.Block) provider.Block {
		time.Sleep(2 * time.Second)
		return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "slow"}
	}}
	o := opts()
	o.Timeout = time.Second
	o.Ceiling = 10 * time.Second
	src := prog(`func main() { s, _ := spore.Fetch("https://example.com"); fmt.Println(s) }`, "fmt", "spore")
	res, err := Run(context.Background(), src, r, o)
	if err != nil {
		t.Fatalf("a helper wait was charged to the budget: %v", err)
	}
	if res.Output != "slow\n" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestCeilingStopsAHelperThatNeverReturns(t *testing.T) {
	r := &fakeRunner{fn: func(ctx context.Context, call provider.Block) provider.Block {
		<-ctx.Done()
		return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "cancelled", IsError: true}
	}}
	o := opts()
	o.Ceiling = time.Second
	start := time.Now()
	_, err := Run(context.Background(), prog(`func main() { spore.Fetch("https://example.com") }`, "spore"), r, o)
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("err = %v, want the ceiling", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("ceiling took %v", d)
	}
}

func TestCancelledContextKillsTheChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := Run(ctx, prog(`func main() { for {} }`), &fakeRunner{}, opts())
	if err == nil {
		t.Fatal("a cancelled run returned no error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("cancellation took %v", d)
	}
}

func TestHelpersSendTheRightCalls(t *testing.T) {
	cases := []struct {
		call, tool, args string
	}{
		{`spore.Fetch("https://x.test")`, "web_fetch", `{"url":"https://x.test"}`},
		{`spore.Search("go", 3)`, "web_search", `{"count":3,"query":"go"}`},
		{`spore.Search("go", 0)`, "web_search", `{"query":"go"}`},
		{`spore.ReadFile("a.txt")`, "fs_read", `{"path":"a.txt"}`},
		{`"", spore.WriteFile("a.txt", "hi")`, "fs_write", `{"content":"hi","path":"a.txt"}`},
		{`"", spore.EditFile("a.txt", "x", "y")`, "fs_edit", `{"new":"y","old":"x","path":"a.txt"}`},
		{`spore.List("")`, "fs_list", `{}`},
		{`spore.List("dir")`, "fs_list", `{"path":"dir"}`},
		{`spore.Glob("**/*.go")`, "fs_glob", `{"pattern":"**/*.go"}`},
		{`spore.Grep("TODO", "")`, "fs_grep", `{"pattern":"TODO"}`},
		{`spore.Grep("TODO", "*.go")`, "fs_grep", `{"glob":"*.go","pattern":"TODO"}`},
		{`spore.Shell("ls")`, "shell_exec", `{"command":"ls"}`},
		{`spore.Recall("weather")`, "recall_search", `{"query":"weather"}`},
		{`spore.Call("mcp__gh__list", map[string]any{"repo": "x", "n": 2})`, "mcp__gh__list", `{"n":2,"repo":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.tool+" "+c.call, func(t *testing.T) {
			r := &fakeRunner{}
			src := prog(fmt.Sprintf("func main() { _, err := %s; if err != nil { panic(err) } }", c.call), "spore")
			if _, err := Run(context.Background(), src, r, opts()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			seen := r.seen()
			if len(seen) != 1 {
				t.Fatalf("runner saw %d calls, want 1", len(seen))
			}
			if seen[0].Name != c.tool || seen[0].Type != provider.BlockToolUse {
				t.Errorf("call = %s (%s), want tool_use %s", seen[0].Name, seen[0].Type, c.tool)
			}
			var got, want any
			_ = json.Unmarshal(seen[0].Input, &got)
			_ = json.Unmarshal([]byte(c.args), &want)
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) != string(wb) {
				t.Errorf("args = %s, want %s", gb, wb)
			}
		})
	}
}

func TestHelperErrorsReachTheProgram(t *testing.T) {
	r := &fakeRunner{fn: func(_ context.Context, call provider.Block) provider.Block {
		return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "denied by rule x", IsError: true}
	}}
	src := prog(`func main() {
	_, err := spore.ReadFile("secret")
	fmt.Println("err:", err)
}`, "fmt", "spore")
	res, err := Run(context.Background(), src, r, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "err: denied by rule x") {
		t.Errorf("Output = %q, want the deny text", res.Output)
	}
	if len(res.Calls) != 1 || res.Calls[0] != (Call{Tool: "fs_read", Outcome: "error"}) {
		t.Errorf("Calls = %+v", res.Calls)
	}
}

func TestKernelToolIsRefusedAsAHelper(t *testing.T) {
	r := &fakeRunner{}
	src := prog(`func main() {
	_, err := spore.Call("go_run", map[string]any{"code": "x"})
	fmt.Println(err != nil)
}`, "fmt", "spore")
	res, err := Run(context.Background(), src, r, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "true\n" {
		t.Errorf("Output = %q, want the program to see an error", res.Output)
	}
	if len(r.seen()) != 0 {
		t.Error("a nested go_run reached the runner")
	}
}

func TestHelperResultIsNotCutToTheModelBudget(t *testing.T) {
	o := opts()
	r := &fakeRunner{fn: func(ctx context.Context, call provider.Block) provider.Block {
		if got := tool.OutputLimit(ctx, 0); got != o.HelperMax {
			return provider.Block{Type: provider.BlockToolResult, ID: call.ID, IsError: true,
				Content: fmt.Sprintf("helper ctx limit = %d, want %d", got, o.HelperMax)}
		}
		return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: strings.Repeat("j", 200_000)}
	}}
	src := prog(`func main() {
	s, err := spore.Fetch("https://x.test")
	if err != nil { panic(err) }
	fmt.Println(len(s))
}`, "fmt", "spore")
	res, err := Run(context.Background(), src, r, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "200000\n" {
		t.Errorf("Output = %q, want 200000", res.Output)
	}
}

func TestConcurrentHelpers(t *testing.T) {
	r := &fakeRunner{}
	src := prog(`func main() {
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := spore.Fetch("https://x.test"); err != nil { panic(err) }
		}()
	}
	wg.Wait()
	fmt.Println("done")
}`, "fmt", "spore", "sync")
	res, err := Run(context.Background(), src, r, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "done\n" || len(r.seen()) != 10 || len(res.Calls) != 10 {
		t.Errorf("Output = %q, runner saw %d, Calls %d; want done, 10, 10", res.Output, len(r.seen()), len(res.Calls))
	}
}

func TestCallIDsAreUniqueAcrossRuns(t *testing.T) {
	r := &fakeRunner{}
	src := prog(`func main() { spore.Fetch("a"); spore.Fetch("b") }`, "spore")
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), src, r, opts()); err != nil {
			t.Fatal(err)
		}
	}
	re := regexp.MustCompile(`^gorun_[0-9a-f]{16}_\d+$`)
	ids := map[string]bool{}
	for _, c := range r.seen() {
		if !re.MatchString(c.ID) {
			t.Errorf("id %q does not match %s", c.ID, re)
		}
		if ids[c.ID] {
			t.Errorf("id %q repeated", c.ID)
		}
		ids[c.ID] = true
	}
	if len(ids) != 4 {
		t.Errorf("%d distinct ids, want 4", len(ids))
	}
}

// The runner is the policy guard, which runs in the daemon. A panic in it
// while serving a helper must fail that one call, not the process.
func TestARunnerPanicFailsOnlyThatCall(t *testing.T) {
	r := &fakeRunner{fn: func(context.Context, provider.Block) provider.Block { panic("guard bug") }}
	src := prog(`func main() {
	_, err := spore.Fetch("https://x.test")
	fmt.Println("err:", err)
}`, "fmt", "spore")
	res, err := Run(context.Background(), src, r, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "err:") || !strings.Contains(res.Output, "guard bug") {
		t.Errorf("Output = %q, want the program to see the panic as an error", res.Output)
	}
}

// Models regularly call spore.* and forget the import. The kernel adds it,
// on the package line so every later line keeps its number.
func TestMissingSporeImportIsAdded(t *testing.T) {
	src := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\ts, err := spore.Fetch(\"https://x.test\")\n\tfmt.Println(s, err)\n}\n"
	res, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "ran web_fetch <nil>\n" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestAddedImportKeepsLineNumbers(t *testing.T) {
	// undefinedThing is on line 6 of what the model wrote. (A compile error
	// is used because yaegi reports every panic at main's first statement.)
	src := "package main\n\nfunc main() {\n\tspore.Fetch(\"x\")\n\tx := 1\n\tx = x + undefinedThing\n}\n"
	_, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err == nil || !strings.HasPrefix(err.Error(), "6:") {
		t.Errorf("err = %v, want it reported at line 6", err)
	}
}

// A program that declares its own identifier named spore must not get the
// package imported over it.
func TestLocalSporeIdentifierIsLeftAlone(t *testing.T) {
	src := "package main\n\nimport \"fmt\"\n\ntype T struct{ Fetch string }\n\nvar spore = T{Fetch: \"local\"}\n\nfunc main() { fmt.Println(spore.Fetch) }\n"
	res, err := Run(context.Background(), src, &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "local\n" {
		t.Errorf("Output = %q", res.Output)
	}
}

// spore.Help answers from the documentation the parent sends with the
// program, so the prompt can list a tool in one line and the full schema
// costs nothing until a program asks for it. It is not a tool call.
func TestHelpReturnsAToolsFullDocumentation(t *testing.T) {
	o := opts()
	o.Docs = Docs(referenceSpecs())
	r := &fakeRunner{}
	res, err := Run(context.Background(), prog(`func main() {
	d, err := spore.Help("mcp__gh__list_prs")
	fmt.Println(d, err)
	_, err = spore.Help("no_such_tool")
	fmt.Println("unknown:", err)
}`, "fmt", "spore"), r, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"List pull requests.", `"repo"`, "unknown: no tool named \"no_such_tool\""} {
		if !strings.Contains(res.Output, strings.ReplaceAll(want, `\"`, `"`)) {
			t.Errorf("output lacks %q:\n%s", want, res.Output)
		}
	}
	if n := len(r.seen()); n != 0 {
		t.Errorf("Help made %d tool calls; it must answer locally", n)
	}
}

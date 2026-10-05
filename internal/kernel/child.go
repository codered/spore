package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing/fstest"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// childEnv marks a process as a kernel child. It is the only variable in a
// child's environment.
const childEnv = "SPORE_KERNEL_CHILD"

// Allowed is the stdlib a program may import. Everything with a side effect
// outside the process (os, net, os/exec, syscall) is absent: a program
// reaches the machine only through the spore package, whose every call the
// parent's policy guard judges. reflect and unsafe are absent because they
// could reach around the symbol table.
var Allowed = []string{
	"bytes", "encoding/base64", "encoding/csv", "encoding/hex", "encoding/json",
	"errors", "fmt", "html", "math", "math/rand", "net/url", "regexp", "sort",
	"strconv", "strings", "sync", "sync/atomic", "text/tabwriter",
	"text/template", "time", "unicode", "unicode/utf8",
}

// IsChild reports whether this process was started by Run to execute one
// program. cmd/spore checks it before doing anything else.
func IsChild() bool { return os.Getenv(childEnv) == "1" }

// ChildMain runs one program and returns the process exit code. It reads the
// program from fd 3 and talks to the parent on fd 4 for everything else.
func ChildMain() int {
	in, out := os.NewFile(3, "kernel-in"), os.NewFile(4, "kernel-out")
	if in == nil || out == nil {
		fmt.Fprintln(os.Stderr, "kernel: started without its pipes")
		return 2
	}
	c := newConn(in, out)
	run, err := c.read()
	if err != nil || run.Type != msgRun {
		fmt.Fprintf(os.Stderr, "kernel: expected a run message: %v\n", err)
		return 2
	}

	ch := &child{conn: c, pending: map[int64]chan msg{}, docs: run.Docs}
	go ch.readLoop()

	w := &outWriter{conn: c, limit: run.MaxOutput}
	i := interp.New(interp.Options{
		Stdout: w,
		Stderr: w,
		// Neither GOPATH nor any file system may supply source: a source
		// package could import nothing more than this surface, but it is
		// still code the parent never saw.
		GoPath:               "/nonexistent/spore-kernel",
		SourcecodeFilesystem: fstest.MapFS{},
		Env:                  []string{},
		Args:                 []string{"main"},
	})
	done := msg{Type: msgDone}
	if err := i.Use(surface(ch)); err != nil {
		done.Error = "kernel: " + err.Error()
	} else if _, err := i.Eval(run.Code); err != nil {
		done.Error = rewriteImportErrors(err.Error())
	}
	done.Truncated = w.truncated()
	if err := c.write(done); err != nil {
		return 1
	}
	// Returning exits the process, which is what stops any goroutine the
	// program left running.
	return 0
}

// surface is the whole symbol table a program sees.
func surface(ch *child) interp.Exports {
	ex := interp.Exports{}
	for _, p := range Allowed {
		key := p + "/" + path.Base(p)
		if syms, ok := stdlib.Symbols[key]; ok {
			ex[key] = syms
		}
	}
	ex["spore/spore"] = map[string]reflect.Value{
		"Fetch": reflect.ValueOf(func(url string) (string, error) {
			return ch.call("web_fetch", map[string]any{"url": url})
		}),
		"Search": reflect.ValueOf(func(query string, count int) (string, error) {
			args := map[string]any{"query": query}
			if count != 0 {
				args["count"] = count
			}
			return ch.call("web_search", args)
		}),
		"ReadFile": reflect.ValueOf(func(p string) (string, error) {
			return ch.call("fs_read", map[string]any{"path": p})
		}),
		"WriteFile": reflect.ValueOf(func(p, content string) error {
			_, err := ch.call("fs_write", map[string]any{"path": p, "content": content})
			return err
		}),
		"EditFile": reflect.ValueOf(func(p, old, new string) error {
			_, err := ch.call("fs_edit", map[string]any{"path": p, "old": old, "new": new})
			return err
		}),
		"List": reflect.ValueOf(func(p string) (string, error) {
			return ch.call("fs_list", optional(map[string]any{}, "path", p))
		}),
		"Glob": reflect.ValueOf(func(pattern string) (string, error) {
			return ch.call("fs_glob", map[string]any{"pattern": pattern})
		}),
		"Grep": reflect.ValueOf(func(pattern, glob string) (string, error) {
			return ch.call("fs_grep", optional(map[string]any{"pattern": pattern}, "glob", glob))
		}),
		"Shell": reflect.ValueOf(func(command string) (string, error) {
			return ch.call("shell_exec", map[string]any{"command": command})
		}),
		"Recall": reflect.ValueOf(func(query string) (string, error) {
			return ch.call("recall_search", map[string]any{"query": query})
		}),
		"Call": reflect.ValueOf(func(tool string, args map[string]any) (string, error) {
			return ch.call(tool, args)
		}),
		"Help": reflect.ValueOf(func(tool string) (string, error) {
			if d, ok := ch.docs[tool]; ok {
				return d, nil
			}
			return "", fmt.Errorf("no tool named %q", tool)
		}),
	}
	return ex
}

func optional(m map[string]any, key, val string) map[string]any {
	if val != "" {
		m[key] = val
	}
	return m
}

var importErrRE = regexp.MustCompile(`import "([^"]+)" error: [^\n]*`)

// rewriteImportErrors replaces yaegi's message for a missing package, which
// talks about GOPATH, with one that tells the model what it can use.
func rewriteImportErrors(s string) string {
	return importErrRE.ReplaceAllStringFunc(s, func(m string) string {
		pkg := importErrRE.FindStringSubmatch(m)[1]
		return fmt.Sprintf("package %q is not available in go_run; allowed: %s, spore. Use spore.* for files, shell and network.",
			pkg, strings.Join(Allowed, ", "))
	})
}

// child is the program's side of helper calls.
type child struct {
	conn    *conn
	next    atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan msg
	docs    map[string]string // spore.Help's answers, from the run message
}

// readLoop routes results to the helper waiting on them. The parent never
// closes its end while the child runs; if it does, the parent is gone and
// the program has no one to answer it.
func (ch *child) readLoop() {
	for {
		m, err := ch.conn.read()
		if err != nil {
			os.Exit(1)
		}
		if m.Type != msgResult {
			continue
		}
		ch.mu.Lock()
		reply := ch.pending[m.ID]
		delete(ch.pending, m.ID)
		ch.mu.Unlock()
		if reply != nil {
			reply <- m
		}
	}
}

func (ch *child) call(tool string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("spore: arguments for %s: %w", tool, err)
	}
	id := ch.next.Add(1)
	reply := make(chan msg, 1)
	ch.mu.Lock()
	ch.pending[id] = reply
	ch.mu.Unlock()
	if err := ch.conn.write(msg{Type: msgCall, ID: id, Tool: tool, Args: raw}); err != nil {
		return "", err
	}
	r := <-reply
	if r.IsError {
		return "", errors.New(r.Content)
	}
	return r.Content, nil
}

// outWriter streams program output to the parent, so output printed before
// a crash or a timeout still arrives. It stops at limit bytes.
type outWriter struct {
	conn    *conn
	limit   int
	mu      sync.Mutex
	written int
	cut     bool
}

func (w *outWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.limit - w.written
	if room <= 0 {
		w.cut = w.cut || len(p) > 0
		return len(p), nil
	}
	send := p
	if len(send) > room {
		send = send[:room]
		w.cut = true
	}
	w.written += len(send)
	if err := w.conn.write(msg{Type: msgOut, Data: string(send)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *outWriter) truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cut
}

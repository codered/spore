package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tui"
)

// e2eDaemon is a real daemon over a real store, with only the model scripted.
// workspace becomes policy.workspace, the ceiling every session root must lie
// within (internal/workspace.Root): config.Default's ceiling is the real
// $HOME, which does not contain a t.TempDir() session root, so the caller's
// workspace must be threaded in here rather than left at the default.
func e2eDaemon(t *testing.T, workspace string, turns ...provider.ScriptTurn) *client {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.DefaultModel = "script/fake"
	cfg.DataDir = t.TempDir()
	cfg.Policy.Workspace = workspace
	preg := provider.NewRegistry()
	preg.Register("script", provider.NewScript(turns...), provider.ProviderPrice{In: 1, Out: 2})
	rt, err := router.New(nil, cfg.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	srv := daemon.New(daemon.Options{Agent: agent.New(st, preg, rt, cfg, nil), Store: st, Cfg: cfg})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)
	return newClient(strings.TrimPrefix(ts.URL, "http://"))
}

// driver runs the model the way the Bubble Tea runtime would: Update is
// called from a single goroutine (this one), and any command it returns runs
// in its own goroutine whose result msg -- if any -- is fed back through
// msgs, exactly as tea.Program.handleCommands does. It must not run a
// command to completion in-line: bubbles/textarea's cursor keeps returning a
// fresh BlinkCmd (a 530ms timer) for as long as the input has focus, which
// is every normal keystroke, so a driver that recurses on each command's
// result before moving on would never return -- it would still be draining
// an unending blink chain when the test's 5s deadline in until() expired.
// The real runtime survives that because handleCommands never waits on the
// goroutines it starts; this driver does the same.
type driver struct {
	t    *testing.T
	m    *tui.Model
	msgs chan tea.Msg
}

func (d *driver) apply(msg tea.Msg) {
	_, cmd := d.m.Update(msg)
	d.runCmd(cmd)
}

func (d *driver) runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				d.runCmd(c)
			}
			return
		}
		d.msgs <- msg
	}()
}

// until applies pump messages until the screen contains want.
func (d *driver) until(want string) {
	d.t.Helper()
	deadline := time.After(5 * time.Second)
	for !strings.Contains(d.m.View(), want) {
		select {
		case msg := <-d.msgs:
			d.apply(msg)
		case <-deadline:
			d.t.Fatalf("the screen never showed %q:\n%s", want, d.m.View())
		}
	}
}

func (d *driver) typeLine(s string) {
	for _, r := range s {
		d.apply(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	d.apply(tea.KeyMsg{Type: tea.KeyEnter})
}

func (d *driver) key(k tea.KeyType) { d.apply(tea.KeyMsg{Type: k}) }

func TestTheTUIDrivesARealDaemonThroughSendStreamStopAndResend(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	ws := t.TempDir()
	c := e2eDaemon(t, ws,
		provider.ScriptTurn{Text: "first reply"},
		provider.ScriptTurn{Text: "partial", Hold: hold},
		provider.ScriptTurn{Text: "after the stop"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sid, err := c.createSession(ctx, "chat", ws)
	if err != nil {
		t.Fatal(err)
	}

	be := tuiBackend{c: c}
	d := &driver{t: t, m: tui.New(ctx, be, sid, tui.Options{}), msgs: make(chan tea.Msg, 256)}
	d.apply(tea.WindowSizeMsg{Width: 120, Height: 40})
	go tui.Pump(ctx, be, func(msg tea.Msg) { d.msgs <- msg })
	d.apply(<-d.msgs) // connected

	d.typeLine("hello")
	d.until("first reply")

	d.typeLine("again")
	d.until("partial")
	d.key(tea.KeyEsc) // INSERT -> NORMAL
	d.key(tea.KeyEsc) // stop
	d.until("stopped")

	d.apply(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	d.typeLine("once more")
	d.until("after the stop")

	if got := d.m.ExitSummary(); !strings.Contains(got, "resume: spore chat "+sid) || !strings.Contains(got, "after the stop") {
		t.Fatalf("exit summary = %q", got)
	}
}

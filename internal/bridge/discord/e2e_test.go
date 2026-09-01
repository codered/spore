package discord

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

// TestEndToEndDiscordTurnWithApproval boots a real daemon, a real store, a
// real guard and a real policy engine, and drives the whole thing from the
// fake Discord client. The provider is the scripted fake; Discord is the only
// other thing faked. Config comes from a written file through config.Load,
// because Load is what appends the baseline deny rules — a config built from
// config.Default() has no baseline, and every security assertion below would
// then be vacuous.
func TestEndToEndDiscordTurnWithApproval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeFile(t, cfgPath, `
default_model = "fake/model"

[providers.fake]
kind = "openai"
base_url = "http://127.0.0.1:1/unused"

[policy]
workspace = "`+dir+`"
default   = "ask"
allow     = ["fs_read"]
ask       = ["shell_exec"]

[bridge.discord]
enabled     = true
token       = "test-token"
guild_id    = "G"
channel_ids = ["C1"]
user_ids    = ["U"]
`)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// The provider script: one shell_exec (which policy says ask), then a
	// sentence once the tool result comes back.
	// Use a recording learn callback to ensure no blanket rules are written.
	learned := make([]string, 0)
	srv, st := newDaemonWithScriptedProviderAndLearn(t, cfg, scriptShellThenText, func(d policy.Decision, rule string) error {
		learned = append(learned, rule)
		return nil
	})

	f := newFakeClient()
	b, err := New(Options{
		Cfg: cfg.Bridge.Discord, Client: f, Turns: srv,
		Store: st, Broker: srv.Broker(), Guard: srv.Guard(),
		Throttle: -1, // flush every event; the test should not wait on a clock
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "run ls"})

	// The approval must arrive as buttons, and the pattern button must be
	// absent: shell_exec has no path-shaped argument to generalise to.
	var prompt sentMessage
	waitFor(t, func() bool {
		for _, m := range f.allSent() {
			if len(m.Message.Buttons) > 0 {
				prompt = m
				return true
			}
		}
		return false
	})
	for _, btn := range prompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Scope == policy.ScopePattern {
			t.Fatal("a one-tap blanket allow for shell_exec was offered on the phone surface")
		}
	}

	// Press "allow once".
	var allowOnce string
	for _, btn := range prompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Allow && ans.Scope == policy.ScopeOnce {
			allowOnce = btn.CustomID
		}
	}
	if allowOnce == "" {
		t.Fatal("no allow-once button")
	}
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "U", GuildID: "G",
		ChannelID: f.allThreads()[0].ThreadID, ParentID: "C1", CustomID: allowOnce})

	// The turn resumes and its text reaches Discord.
	waitFor(t, func() bool {
		for _, c := range f.finalContents(f.allThreads()[0].ThreadID) {
			if strings.Contains(c, "done") {
				return true
			}
		}
		return false
	})

	// No suspension is left open. The session id comes from the binding the
	// bridge wrote, which is also a check that the binding is correct.
	sessionID, found, err := st.SessionForExternal(context.Background(), bridgeName, f.allThreads()[0].ThreadID)
	if err != nil || !found {
		t.Fatalf("no session bound to the thread: (found=%v, err=%v)", found, err)
	}
	pending, err := srv.Guard().Pending(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d suspensions left open after the button press", len(pending))
	}

	// The key security property: answering a degraded shell_exec approval
	// (one with no path-shaped argument to generalize to) must not write a
	// blanket rule. The answer was "allow once" (ScopeOnce), not "allow
	// pattern", and learn should never have been called.
	if len(learned) != 0 {
		t.Fatalf("the guard attempted to learn %d rules for a degraded approval: %v", len(learned), learned)
	}
}

// TestEndToEndAStrangerCannotAnswerAnApproval is the same stack, driven by a
// user id that is not on the allowlist. This is the case the bridge exists to
// make safe: the approval prompt is visible in a channel, so anyone who can
// see it can try to press its buttons.
func TestEndToEndAStrangerCannotAnswerAnApproval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeFile(t, cfgPath, `
default_model = "fake/model"

[providers.fake]
kind = "openai"
base_url = "http://127.0.0.1:1/unused"

[policy]
workspace = "`+dir+`"
default   = "ask"
allow     = ["fs_read"]
ask       = ["shell_exec"]

[bridge.discord]
enabled     = true
token       = "test-token"
guild_id    = "G"
channel_ids = ["C1"]
user_ids    = ["U"]
`)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv, st := newDaemonWithScriptedProvider(t, cfg, scriptShellThenText)

	f := newFakeClient()
	b, err := New(Options{
		Cfg: cfg.Bridge.Discord, Client: f, Turns: srv,
		Store: st, Broker: srv.Broker(), Guard: srv.Guard(), Throttle: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "run ls"})

	var prompt sentMessage
	waitFor(t, func() bool {
		for _, m := range f.allSent() {
			if len(m.Message.Buttons) > 0 {
				prompt = m
				return true
			}
		}
		return false
	})
	var allowOnce string
	for _, btn := range prompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Allow && ans.Scope == policy.ScopeOnce {
			allowOnce = btn.CustomID
		}
	}
	if allowOnce == "" {
		t.Fatal("no allow-once button")
	}

	thread := f.allThreads()[0].ThreadID
	sessionID, _, err := st.SessionForExternal(context.Background(), bridgeName, thread)
	if err != nil {
		t.Fatal(err)
	}

	// A user who is not on the allowlist presses the real button, with the
	// real custom id, in the real thread. Only the user id differs.
	f.press(Interaction{
		ID: "i1", Token: "tok", UserID: "STRANGER", GuildID: "G",
		ChannelID: thread, ParentID: "C1", CustomID: allowOnce,
	})

	// Silence: not even a refusal. A reply would confirm to whoever pressed
	// it that the bot is live and that the button is real.
	if n := len(f.allResponds()); n != 0 {
		t.Fatalf("the bridge sent %d responses to a stranger's press", n)
	}
	// And the decision is still the real user's to make.
	pending, err := srv.Guard().Pending(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d suspensions open, want 1 still waiting for its owner", len(pending))
	}
}

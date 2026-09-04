package discord

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
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

	// The provider script: two shell_exec calls to test forging pattern answers.
	// Use a recording learn callback to ensure no blanket rules are written.
	learned := make([]string, 0)
	script := []provider.ScriptTurn{
		{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "tu1", Name: "shell_exec",
			Input: []byte(`{"cmd":"ls"}`),
		}}},
		{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "tu2", Name: "shell_exec",
			Input: []byte(`{"cmd":"pwd"}`),
		}}},
		{Text: "done"},
	}
	srv, st := newDaemonWithScriptedProviderAndLearn(t, cfg, script, func(d policy.Decision, rule string) error {
		learned = append(learned, rule)
		return nil
	})

	f := newFakeClient()
	b, err := New(Options{
		Cfg: cfg.Bridge.Discord, Client: f, Turns: srv, Sessions: srv,
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

	// First approval (for the first shell_exec): approve with "allow once".
	// The pattern button must be absent: shell_exec has no path-shaped argument to generalise to.
	var firstPrompt sentMessage
	waitFor(t, func() bool {
		for _, m := range f.allSent() {
			if len(m.Message.Buttons) > 0 {
				firstPrompt = m
				return true
			}
		}
		return false
	})
	for _, btn := range firstPrompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Scope == policy.ScopePattern {
			t.Fatal("a one-tap blanket allow for shell_exec was offered on the phone surface")
		}
	}

	// Press "allow once" on the first approval.
	var allowOnce string
	for _, btn := range firstPrompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Allow && ans.Scope == policy.ScopeOnce {
			allowOnce = btn.CustomID
		}
	}
	if allowOnce == "" {
		t.Fatal("no allow-once button")
	}
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "U", GuildID: "G",
		ChannelID: f.allThreads()[0].ThreadID, ParentID: "C1", CustomID: allowOnce})

	// Second approval (for the second shell_exec): this is still live.
	// Forge the answer the UI refuses to offer: a pattern-scoped answer for a
	// degraded call (one with no path-shaped argument). This tests that the
	// guard independently refuses, not just the UI. The waiter is live, so
	// this goes through Guard.Run, not Guard.Resolve.
	var secondPrompt sentMessage
	waitFor(t, func() bool {
		allSent := f.allSent()
		if len(allSent) < 2 {
			return false
		}
		// Find a sent message that's not firstPrompt and has buttons
		for _, m := range allSent {
			if m.MessageID != firstPrompt.MessageID && len(m.Message.Buttons) > 0 {
				secondPrompt = m
				return true
			}
		}
		return false
	})

	// Find the allow-once button for the second approval
	var secondAllowOnce string
	for _, btn := range secondPrompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Allow && ans.Scope == policy.ScopeOnce {
			secondAllowOnce = btn.CustomID
		}
	}
	if secondAllowOnce == "" {
		t.Fatal("no allow-once button on second approval")
	}

	// Forge a pattern-scoped press for the second approval
	sid, pid, _, err := decodeCustomID(secondAllowOnce)
	if err != nil {
		t.Fatal(err)
	}
	forged := encodeCustomID(sid, pid, true, policy.ScopePattern)
	f.press(Interaction{ID: "i2", Token: "tok2", UserID: "U", GuildID: "G",
		ChannelID: f.allThreads()[0].ThreadID, ParentID: "C1", CustomID: forged})

	// The turn resumes and its text reaches Discord.
	waitFor(t, func() bool {
		for _, c := range f.finalContents(f.allThreads()[0].ThreadID) {
			if strings.Contains(c, "done") {
				return true
			}
		}
		return false
	})

	// Get the session for later assertions.
	sessionID, found, err := st.SessionForExternal(context.Background(), bridgeName, f.allThreads()[0].ThreadID)
	if err != nil || !found {
		t.Fatalf("no session bound to the thread: (found=%v, err=%v)", found, err)
	}

	// No suspension is left open.
	pending, err := srv.Guard().Pending(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d suspensions left open after the button presses", len(pending))
	}

	// The key security property: the guard must refuse a pattern scope for
	// a degraded call (one with no path-shaped argument). Presentation is not
	// enforcement: even when someone forges a pattern-scoped press on a live
	// approval, the guard downgrades it to once and refuses to learn.
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
		Cfg: cfg.Bridge.Discord, Client: f, Turns: srv, Sessions: srv,
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

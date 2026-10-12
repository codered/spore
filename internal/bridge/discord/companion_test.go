package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/store"
)

type fakeProposals struct {
	mu       sync.Mutex
	calls    []string
	commands [][]string
}

func (f *fakeProposals) CompanionCommand(_ context.Context, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "check" {
		return "", errors.New("usage: check runs from the terminal")
	}
	return "companion said: " + strings.Join(args, " ") + "\n", nil
}

func (f *fakeProposals) commandArgs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.commands...)
}

func (f *fakeProposals) AnswerProposal(_ context.Context, _ int64, choice string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, choice)
	return "Watching ZS.", nil
}

func (f *fakeProposals) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// companionBridge is a bridge whose companion thread opens in channel CC,
// for user U only; V is a bridge user who is not a companion user.
func companionBridge(t *testing.T, mode string) (*Bridge, *fakeClient, *fakeTurns, *store.Store, *fakeProposals) {
	t.Helper()
	st := openTestStore(t)
	f := newFakeClient()
	turns := newFakeTurns()
	props := &fakeProposals{}
	cfg := testDiscordConfig()
	cfg.UserIDs = []string{"U", "V"}
	b, err := New(Options{
		Cfg: cfg, Client: f, Turns: turns, Sessions: &fakeSessions{store: st}, Store: st,
		Broker: daemon.NewBroker(daemon.NewHub()), Throttle: -1,
		Companion:   config.CompanionDiscordConfig{Mode: mode, ChannelID: "CC", UserIDs: []string{"U"}},
		CompanionOn: true, Proposals: props, Commands: props,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, f, turns, st, props
}

func TestCompanionChannelOpensOneThreadAndSendsButtons(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordThread)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordThread, ChannelID: "CC", UserIDs: []string{"U"}})
	if ch.Name() != store.OutreachChannelDiscord {
		t.Fatalf("name = %q", ch.Name())
	}
	if err := ch.Send(ctx, sid, store.Outreach{ID: 7, Kind: store.OutreachProposal, Text: "Watch ZS?"}); err != nil {
		t.Fatal(err)
	}
	threads := f.allThreads()
	if len(threads) != 1 || threads[0].ChannelID != "CC" {
		t.Fatalf("threads = %+v", threads)
	}
	thread := threads[0].ThreadID
	if bound, ok, _ := st.SessionForExternal(ctx, bridgeName, thread); !ok || bound != sid {
		t.Fatalf("thread bound to %q %v, want %s", bound, ok, sid)
	}
	in := f.sentTo(thread)
	if len(in) != 1 || in[0].Message.Content != "Watch ZS?" || len(in[0].Message.Buttons) != 3 {
		t.Fatalf("thread messages = %+v", in)
	}
	for i, want := range []string{companion.ChoiceYes, companion.ChoiceNotNow, companion.ChoiceNever} {
		id, choice, err := decodeCompanionCustomID(in[0].Message.Buttons[i].CustomID)
		if err != nil || id != 7 || choice != want {
			t.Fatalf("button %d = %q (%d %q %v)", i, in[0].Message.Buttons[i].CustomID, id, choice, err)
		}
	}
	if err := ch.Send(ctx, sid, store.Outreach{ID: 8, Kind: store.OutreachNudge, Text: "Still?"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.allThreads()); n != 1 {
		t.Fatalf("threads after a second send = %d, want 1", n)
	}
	if got := f.sentTo(thread); len(got) != 2 || len(got[1].Message.Buttons) != 0 {
		t.Fatalf("a nudge carries no buttons: %+v", got)
	}
}

func TestCompanionChannelOpensANewThreadForANewSession(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordThread)
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordThread, ChannelID: "CC", UserIDs: []string{"U"}})
	first, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	_ = ch.Send(ctx, first, store.Outreach{ID: 1, Kind: store.OutreachNudge, Text: "a"})
	second, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	_ = ch.Send(ctx, second, store.Outreach{ID: 2, Kind: store.OutreachNudge, Text: "b"})
	if n := len(f.allThreads()); n != 2 {
		t.Fatalf("threads = %d, want one per companion session", n)
	}
}

func TestCompanionChannelDMMode(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordDM)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordDM, UserIDs: []string{"U"}})
	if err := ch.Send(ctx, sid, store.Outreach{ID: 1, Kind: store.OutreachNudge, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if bound, ok, _ := st.SessionForExternal(ctx, bridgeName, "DM-U"); !ok || bound != sid {
		t.Fatalf("DM bound to %q %v", bound, ok)
	}
	if got := f.sentTo("DM-U"); len(got) != 1 {
		t.Fatalf("DM messages = %+v", got)
	}
	if n := len(f.allThreads()); n != 0 {
		t.Fatalf("DM mode opened %d threads", n)
	}
}

func TestCompanionChannelSendErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordThread)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	f.setFailNext("Send", errors.New("discord is down"))
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordThread, ChannelID: "CC", UserIDs: []string{"U"}})
	if err := ch.Send(ctx, sid, store.Outreach{ID: 1, Kind: store.OutreachNudge, Text: "hi"}); err == nil {
		t.Fatal("a failed send must be an error, so the daemon falls back to the terminal")
	}
}

func TestCompanionAdmission(t *testing.T) {
	a := NewAdmitter(config.DiscordConfig{GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U", "V"}}).
		WithCompanion("CC", []string{"U"})
	for _, tc := range []struct {
		name string
		in   Inbound
		want bool
	}{
		{"companion user in a companion thread", Inbound{UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC"}, true},
		{"bridge user who is not a companion user", Inbound{UserID: "V", GuildID: "G", ChannelID: "T", ParentID: "CC"}, false},
		{"top level in the companion channel", Inbound{UserID: "U", GuildID: "G", ChannelID: "CC"}, false},
		{"a stranger", Inbound{UserID: "X", GuildID: "G", ChannelID: "T", ParentID: "CC"}, false},
		{"ordinary channel still works", Inbound{UserID: "V", GuildID: "G", ChannelID: "C1"}, true},
	} {
		if got := a.AdmitMessage(tc.in); got != tc.want {
			t.Errorf("%s: admitted = %v, want %v", tc.name, got, tc.want)
		}
	}
	off := NewAdmitter(config.DiscordConfig{GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U"}})
	if off.AdmitMessage(Inbound{UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC"}) {
		t.Error("without the companion, its channel admits nobody")
	}
	// The companion channel is also a bridge channel: its threads still admit
	// only the companion users, not every bridge user.
	both := NewAdmitter(config.DiscordConfig{GuildID: "G", ChannelIDs: []string{"C1", "CC"}, UserIDs: []string{"U", "V"}}).
		WithCompanion("CC", []string{"U"})
	if both.AdmitMessage(Inbound{UserID: "V", GuildID: "G", ChannelID: "T", ParentID: "CC"}) {
		t.Error("a bridge user who is not a companion user was admitted in a companion thread")
	}
	if !both.AdmitMessage(Inbound{UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC"}) {
		t.Error("a companion user was not admitted in a companion thread")
	}
}

func TestProposalButtonPressReachesTheCompanion(t *testing.T) {
	_, f, _, _, props := companionBridge(t, config.CompanionDiscordThread)
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC",
		CustomID: companionCustomID(7, companion.ChoiceYes)})
	waitFor(t, func() bool { return len(f.allResponds()) == 1 })
	if got := props.all(); len(got) != 1 || got[0] != companion.ChoiceYes {
		t.Fatalf("answers = %v", got)
	}
	if r := f.allResponds()[0]; !strings.Contains(r.Content, "Watching ZS") {
		t.Fatalf("respond = %+v", r)
	}
	// V is a bridge user but not a companion user: the press is dropped.
	f.press(Interaction{ID: "i2", Token: "tok", UserID: "V", GuildID: "G", ChannelID: "T", ParentID: "CC",
		CustomID: companionCustomID(7, companion.ChoiceNever)})
	time.Sleep(50 * time.Millisecond)
	if got := props.all(); len(got) != 1 {
		t.Fatalf("a non-companion user's press was acted on: %v", got)
	}
}

func TestAReplyInTheCompanionThreadContinuesItsSession(t *testing.T) {
	ctx := context.Background()
	_, f, turns, st, _ := companionBridge(t, config.CompanionDiscordThread)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	if err := st.BindExternal(ctx, bridgeName, "T", sid); err != nil {
		t.Fatal(err)
	}
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC", Content: "yes please"})
	turns.waitForTurn(t)
	if s := turns.allStarts(); len(s) != 1 || s[0].sessionID != sid {
		t.Fatalf("starts = %+v, want one in the companion session", s)
	}
}

func TestCompanionCustomIDRejectsGarbage(t *testing.T) {
	for _, s := range []string{"companion|x|yes", "companion|7|maybe", "companion|7", "spore|s|1|allow|once"} {
		if _, _, err := decodeCompanionCustomID(s); err == nil {
			t.Errorf("decoded %q", s)
		}
	}
}

func TestSlashCompanionRepliesQuietlyWithoutATurn(t *testing.T) {
	_, f, turns, _, props := companionBridge(t, config.CompanionDiscordThread)
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "/companion pause 2h"})
	waitFor(t, func() bool { return len(f.sentTo("C1")) > 0 })
	got := f.sentTo("C1")[0].Message
	if !strings.Contains(got.Content, "companion said: pause 2h") || !got.Quiet {
		t.Fatalf("reply = %+v, want the command's text, sent quiet", got)
	}
	if a := props.commandArgs(); len(a) != 1 || strings.Join(a[0], " ") != "pause 2h" {
		t.Fatalf("command args = %q", a)
	}
	f.deliver(Inbound{MessageID: "m2", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "/companion check"})
	waitFor(t, func() bool { return len(f.sentTo("C1")) > 1 })
	if got := f.sentTo("C1")[1].Message.Content; !strings.Contains(got, "terminal") {
		t.Fatalf("check reply = %q, want the refusal", got)
	}
	if turns.startCount() != 0 {
		t.Fatalf("/companion started %d turns, want 0", turns.startCount())
	}
}

func TestSlashCompanionIsForCompanionUsersOnly(t *testing.T) {
	_, f, _, _, props := companionBridge(t, config.CompanionDiscordThread)
	f.deliver(Inbound{MessageID: "m1", UserID: "V", GuildID: "G", ChannelID: "C1", Content: "/companion pause 2h"})
	waitFor(t, func() bool { return len(f.sentTo("C1")) > 0 })
	if got := f.sentTo("C1")[0].Message; !strings.Contains(got.Content, "only the companion's users") || !got.Quiet {
		t.Fatalf("reply = %+v", got)
	}
	if a := props.commandArgs(); len(a) != 0 {
		t.Fatalf("a non-companion user ran %q", a)
	}
}

// Text the model wrote must never ping: the anchor, every outreach and
// every note go out with no mentions allowed.
func TestCompanionSendsAreQuiet(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordThread)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordThread, ChannelID: "CC", UserIDs: []string{"U"}})
	if err := ch.Send(ctx, sid, store.Outreach{ID: 1, Kind: store.OutreachProposal, Text: "@everyone watch ZS?"}); err != nil {
		t.Fatal(err)
	}
	b.Deliver(ctx, sid, "Watching ZS. @here")
	sent := f.allSent()
	if len(sent) != 3 {
		t.Fatalf("sent %d messages, want anchor, outreach and note: %+v", len(sent), sent)
	}
	for _, s := range sent {
		if !s.Message.Quiet {
			t.Errorf("%q was sent with mentions allowed", s.Message.Content)
		}
	}
}

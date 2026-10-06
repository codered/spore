package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/store"
)

// spend records one model reply with usage on a session, as a turn would.
func spend(t *testing.T, st *store.Store, sid string, in, out int, cost float64) {
	t.Helper()
	if _, err := st.AppendMessage(context.Background(), store.Message{
		SessionID: sid, Role: "assistant", BlocksJSON: []byte(`[]`), Model: "m",
		TokensIn: in, TokensOut: out, CostUSD: cost,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSlashUsageInAThreadReportsItsSession(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	b.showCost = true
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mine, _ := st.CreateSession(ctx, "mine", "")
	other, _ := st.CreateSession(ctx, "other", "")
	if err := st.BindExternal(ctx, bridgeName, "T1", mine); err != nil {
		t.Fatal(err)
	}
	spend(t, st, mine, 1200, 80, 0.25)
	spend(t, st, other, 5000, 20, 1)

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "T1", ParentID: "C1", Content: "/usage"})
	waitFor(t, func() bool { return len(f.sentTo("T1")) > 0 })

	got := f.sentTo("T1")[0].Message.Content
	for _, want := range []string{"turns: 1", "tokens in: 1,200", "cost: $0.2500", "all sessions: 2 turns, 6.2k in, 100 out, $1.25"} {
		if !strings.Contains(got, want) {
			t.Errorf("reply is missing %q:\n%s", want, got)
		}
	}
	if turns.startCount() != 0 {
		t.Fatalf("/usage started %d turns, want 0", turns.startCount())
	}
}

func TestSlashUsageInAChannelReportsOnlyTheTotal(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sid, _ := st.CreateSession(context.Background(), "s", "")
	spend(t, st, sid, 300, 30, 0.5)

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "/usage"})
	waitFor(t, func() bool { return len(f.sentTo("C1")) > 0 })

	got := f.sentTo("C1")[0].Message.Content
	if strings.Contains(got, "this session") || !strings.Contains(got, "all sessions: 1 turns, 300 in, 30 out") {
		t.Fatalf("want the 30-day total alone, got:\n%s", got)
	}
	if strings.Contains(got, "$") {
		t.Fatalf("cost is hidden unless show_cost is set, got:\n%s", got)
	}
	if turns.startCount() != 0 || len(f.allThreads()) != 0 {
		t.Fatal("/usage in a channel must open no thread and start no turn")
	}
	if _, found, _ := st.SessionForExternal(context.Background(), bridgeName, "C1"); found {
		t.Fatal("/usage bound the guild channel")
	}
}

func TestSlashUsageSettlesItsOwnAcknowledgement(t *testing.T) {
	b, f, _, _ := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.deliver(Inbound{MessageID: "d1", UserID: "U", ChannelID: "DM1", Content: "/usage"})
	waitFor(t, func() bool {
		for _, r := range f.allReacts() {
			if r.MessageID == "d1" && r.Emoji == emojiDone && !r.Removed {
				return true
			}
		}
		return false
	})
}

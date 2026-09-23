package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/store"
)

// A note for a session bound to a Discord channel is posted there.
func TestDeliverPostsToTheSessionsChannel(t *testing.T) {
	b, f, _, st := newTestBridge(t)
	ctx := context.Background()
	sid, _ := st.CreateSessionFrom(ctx, "", "", store.SourceDiscord)
	if err := st.BindExternal(ctx, bridgeName, "thread-1", sid); err != nil {
		t.Fatal(err)
	}
	b.Deliver(ctx, sid, "⏰ job 1 ran at 02:30 UTC — ok · it's in the Jobs folder")
	sent := f.sentTo("thread-1")
	if len(sent) != 1 || !strings.Contains(sent[0].Message.Content, "job 1 ran") {
		t.Fatalf("thread got %+v; want the note", sent)
	}
	if all := f.allSent(); len(all) != 1 {
		t.Errorf("the note went to %d places; want only the session's channel", len(all))
	}
}

// A job created in the TUI has a TUI session as its origin. That session has
// no Discord channel, so nothing about the job ever reaches Discord.
func TestATUISessionNeverReachesDiscord(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()
	tui, _ := st.CreateSessionFrom(ctx, "", "", store.SourceChat)
	// Another session IS bound, so there is somewhere a stray note could go.
	other, _ := st.CreateSessionFrom(ctx, "", "", store.SourceDiscord)
	if err := st.BindExternal(ctx, bridgeName, "thread-1", other); err != nil {
		t.Fatal(err)
	}

	b.Deliver(ctx, tui, "⏰ job 1 ran")
	stop := b.Follow(tui)
	stop()
	turns.publish(tui, daemon.WireEvent{Type: daemon.WireText, Text: "hello"})
	turns.publish(tui, daemon.WireEvent{Type: daemon.WireTurnDone})
	if all := f.allSent(); len(all) != 0 {
		t.Fatalf("a TUI session reached Discord: %+v", all)
	}
}

// A turn spore starts itself, like a job's check-in, is rendered into the
// session's channel the same way a turn the bridge started is.
func TestFollowRendersADaemonStartedTurn(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()
	sid, _ := st.CreateSessionFrom(ctx, "", "", store.SourceDiscord)
	if err := st.BindExternal(ctx, bridgeName, "dm-1", sid); err != nil {
		t.Fatal(err)
	}

	b.Follow(sid)
	turns.publish(sid, daemon.WireEvent{Type: daemon.WireText, Text: "Job 1 ran fine. Each run, only failures, or nothing?"})
	turns.publish(sid, daemon.WireEvent{Type: daemon.WireTurnDone})
	waitFor(t, func() bool {
		for _, c := range f.finalContents("dm-1") {
			if strings.Contains(c, "only failures") {
				return true
			}
		}
		return false
	})
}

// Before the bridge has started, or after it has closed, there is nothing to
// render with: Follow starts nothing and still returns a usable stop.
func TestFollowBeforeStartDoesNothing(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	ctx := context.Background()
	sid, _ := st.CreateSessionFrom(ctx, "", "", store.SourceDiscord)
	if err := st.BindExternal(ctx, bridgeName, "dm-1", sid); err != nil {
		t.Fatal(err)
	}
	stop := b.Follow(sid)
	stop()
	turns.publish(sid, daemon.WireEvent{Type: daemon.WireText, Text: "hello"})
	if all := f.allSent(); len(all) != 0 {
		t.Fatalf("an unstarted bridge rendered: %+v", all)
	}
}

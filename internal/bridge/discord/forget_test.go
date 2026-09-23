package discord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/store"
)

func boundSession(t *testing.T, st *store.Store, externalID string) store.Session {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateSessionFrom(ctx, "chat", "", store.SourceDiscord)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BindExternal(ctx, bridgeName, externalID, id); err != nil {
		t.Fatal(err)
	}
	sess, _, err := st.Session(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestForgettingASessionDeletesTheThreadSporeOpened(t *testing.T) {
	b, f, _, st := newTestBridge(t)
	f.setKind("thread-1", ChannelThread)
	sess := boundSession(t, st, "thread-1")

	notes := b.ForgetSessions(context.Background(), []store.Session{sess})
	if got := f.deletedChannelIDs(); len(got) != 1 || got[0] != "thread-1" {
		t.Fatalf("deleted channels = %v, want thread-1", got)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "deleted the Discord thread") {
		t.Fatalf("notes = %v", notes)
	}
}

func TestForgettingADMSessionDeletesOnlySporesOwnMessagesInItsSpan(t *testing.T) {
	b, f, _, st := newTestBridge(t)
	f.setKind("dm-1", ChannelDM)
	sess := boundSession(t, st, "dm-1")
	f.addOwnMessage("dm-1", "before", sess.CreatedAt.Add(-time.Hour))
	f.addOwnMessage("dm-1", "during", sess.CreatedAt.Add(time.Second))

	notes := b.ForgetSessions(context.Background(), []store.Session{sess})
	if got := f.deletedMessageIDs(); len(got) != 1 || got[0] != "during" {
		t.Fatalf("deleted messages = %v, want only the one inside the session's span", got)
	}
	if len(f.deletedChannelIDs()) != 0 {
		t.Fatal("a DM channel was deleted")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "1 of spore's messages") || !strings.Contains(notes[0], "yours stay") {
		t.Fatalf("notes = %v, want the count and that the user's own messages stay", notes)
	}
}

func TestAThreadTheBotCannotDeleteIsReported(t *testing.T) {
	b, f, _, st := newTestBridge(t)
	f.setKind("thread-1", ChannelThread)
	f.setFailNext("DeleteChannel", errors.New("403 Missing Permissions"))
	sess := boundSession(t, st, "thread-1")

	notes := b.ForgetSessions(context.Background(), []store.Session{sess})
	if len(notes) != 1 || !strings.Contains(notes[0], "could not delete the Discord thread") || !strings.Contains(notes[0], "Manage Threads") {
		t.Fatalf("notes = %v, want the failure and the permission it needs", notes)
	}
}

func TestSessionsWithoutABindingNeedNoDiscordWork(t *testing.T) {
	b, f, _, st := newTestBridge(t)
	id, _ := st.CreateSessionFrom(context.Background(), "local", "", store.SourceChat)
	sess, _, _ := st.Session(context.Background(), id)
	if notes := b.ForgetSessions(context.Background(), []store.Session{sess}); len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
	if len(f.deletedChannelIDs())+len(f.deletedMessageIDs()) != 0 {
		t.Fatal("Discord was touched for a session it never saw")
	}
}

func TestSnowflakeAtIsTheFirstIDDiscordCouldAssignThen(t *testing.T) {
	if got := snowflakeAt(time.UnixMilli(discordEpochMs + 1)); got != "4194304" {
		t.Fatalf("snowflakeAt(epoch+1ms) = %s, want 4194304 (1<<22)", got)
	}
	if got := snowflakeAt(time.UnixMilli(0)); got != "0" {
		t.Fatalf("a time before Discord's epoch = %s, want 0", got)
	}
}

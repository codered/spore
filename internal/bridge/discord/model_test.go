package discord

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/codered/spore/internal/models"
)

type fakeChooser struct {
	mu   sync.Mutex
	view models.View
	sets []string
}

func (f *fakeChooser) View(context.Context, string, bool) (models.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view, nil
}

func (f *fakeChooser) Set(_ context.Context, sid, op, ref string) (models.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, sid+" "+op+" "+ref)
	for i := range f.view.Ops {
		if f.view.Ops[i].Op == op {
			f.view.Ops[i].Selected = ref
		}
	}
	return f.view, nil
}

func (f *fakeChooser) setCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sets...)
}

func chooserView() models.View {
	return models.View{
		Ops: []models.Op{
			{Op: "chat", Scope: "session", Selected: "a/one", Default: "a/one"},
			{Op: "compaction", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "title", Scope: "global", Selected: "gone/x", Default: "a/one"},
			{Op: "classify", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "refinement", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "subagent", Scope: "session", Selected: "a/one", Default: "a/one"},
		},
		Groups: []models.Group{{Provider: "a", Refs: []string{"a/one", "a/two"}}},
	}
}

func selectFor(t *testing.T, msgs []sentMessage, op string) Select {
	t.Helper()
	for _, m := range msgs {
		for _, s := range m.Message.Selects {
			if _, got := decodeModelCustomID(s.CustomID); got == op {
				return s
			}
		}
	}
	t.Fatalf("no select for %s", op)
	return Select{}
}

func TestSlashModelInAThreadOffersEveryOperation(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	fc := &fakeChooser{view: chooserView()}
	b.models = fc
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sid, _ := st.CreateSession(context.Background(), "s", "")
	if err := st.BindExternal(context.Background(), bridgeName, "T1", sid); err != nil {
		t.Fatal(err)
	}
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "T1", ParentID: "C1", Content: "/model"})
	waitFor(t, func() bool { return len(f.sentTo("T1")) >= 2 })

	msgs := f.sentTo("T1")
	if !strings.Contains(msgs[0].Message.Content, "-> gone/x") {
		t.Fatalf("overview = %q", msgs[0].Message.Content)
	}
	if n0, n1 := len(msgs[0].Message.Selects), len(msgs[1].Message.Selects); n0 != 4 || n1 != 2 {
		t.Fatalf("selects per message = %d, %d; want 4, 2", n0, n1)
	}
	chat := selectFor(t, msgs, "chat")
	if s, _ := decodeModelCustomID(chat.CustomID); s != sid {
		t.Fatalf("chat select carries session %q, want %q", s, sid)
	}
	if len(chat.Options) != 2 || !chat.Options[0].Default || chat.Options[0].Value != "a/one" || !strings.HasPrefix(chat.Options[0].Label, "->") {
		t.Fatalf("chat options = %+v", chat.Options)
	}
	for _, o := range selectFor(t, msgs, "title").Options {
		if o.Value == "gone/x" {
			t.Fatal("an unavailable model is offered")
		}
	}
	if turns.startCount() != 0 {
		t.Fatalf("/model started %d turns", turns.startCount())
	}
}

func TestSlashModelInAChannelOffersOnlyDaemonWideOperations(t *testing.T) {
	b, f, turns, _ := newTestBridge(t)
	defer b.Close()
	b.models = &fakeChooser{view: chooserView()}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "/model"})
	waitFor(t, func() bool { return len(f.sentTo("C1")) >= 2 })
	for _, m := range f.sentTo("C1") {
		for _, s := range m.Message.Selects {
			if _, op := decodeModelCustomID(s.CustomID); op == "chat" || op == "subagent" {
				t.Fatalf("%s offered with no session", op)
			}
		}
	}
	if !strings.Contains(f.sentTo("C1")[0].Message.Content, "inside a thread or DM") {
		t.Fatalf("no per-session note: %q", f.sentTo("C1")[0].Message.Content)
	}
	if len(f.threads) != 0 || turns.startCount() != 0 {
		t.Fatal("/model opened a thread or a turn")
	}
}

func TestChoosingADaemonWideModelSaysItAffectsEverySession(t *testing.T) {
	b, f, _, _ := newTestBridge(t)
	defer b.Close()
	fc := &fakeChooser{view: chooserView()}
	b.models = fc
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "U", GuildID: "G", ChannelID: "T1", ParentID: "C1",
		CustomID: modelCustomID("S1", "title"), Values: []string{"a/two"}})
	waitFor(t, func() bool { return len(f.sentTo("T1")) > 0 })
	if got := fc.setCalls(); len(got) != 1 || got[0] != "S1 title a/two" {
		t.Fatalf("sets = %v", got)
	}
	if len(f.responds) != 1 {
		t.Fatalf("responds = %d, want the interaction acknowledged once", len(f.responds))
	}
	msg := f.sentTo("T1")[0].Message.Content
	if !strings.Contains(msg, "title -> a/two (every session)") || !strings.Contains(msg, "every session, not just this thread") {
		t.Fatalf("reply = %q", msg)
	}
}

func TestAModelSelectFromAStrangerChangesNothing(t *testing.T) {
	b, f, _, _ := newTestBridge(t)
	defer b.Close()
	fc := &fakeChooser{view: chooserView()}
	b.models = fc
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "STRANGER", GuildID: "G", ChannelID: "T1", ParentID: "C1",
		CustomID: modelCustomID("S1", "title"), Values: []string{"a/two"}})
	if got := fc.setCalls(); len(got) != 0 {
		t.Fatalf("a stranger changed the model: %v", got)
	}
}

func TestModelSelectCapsAtTwentyFiveKeepingSelectedAndDefault(t *testing.T) {
	v := chooserView()
	var refs []string
	for i := 0; i < 40; i++ {
		refs = append(refs, fmt.Sprintf("a/m%02d", i))
	}
	v.Groups = []models.Group{{Provider: "a", Refs: append([]string{"a/one"}, refs...)}}
	v.Ops[0].Selected = "a/m39"
	s, ok := modelSelect(v, "S1", "chat")
	if !ok || len(s.Options) != 25 {
		t.Fatalf("options = %d", len(s.Options))
	}
	if s.Options[0].Value != "a/m39" || s.Options[1].Value != "a/one" {
		t.Fatalf("first two = %+v", s.Options[:2])
	}
}

func TestSelectsRenderBeforeButtonsWithinFiveRows(t *testing.T) {
	sel := Select{CustomID: "x", Placeholder: "p", Options: []SelectOption{{Label: "a", Value: "a", Default: true}}}
	rows := componentsFor([]Button{{CustomID: "b", Label: "b"}}, []Select{sel, sel, sel, sel, sel, sel})
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	menu, ok := rows[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	if !ok || menu.MenuType != discordgo.StringSelectMenu || !menu.Options[0].Default {
		t.Fatalf("row 0 = %#v", rows[0])
	}
}

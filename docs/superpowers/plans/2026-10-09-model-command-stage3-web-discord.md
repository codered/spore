# /model Stage 3 (web modal + Discord selects) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `/model` in the web UI opens a tabbed panel (overview + one tab per operation) and in Discord replies with the overview and a select menu per operation.

**Architecture:** The web UI reads and writes the stage 1 routes from `web/app.js`; a small JS port of `modelcmd.Options` keeps the option order identical. The Discord bridge gains string-select components, a `ModelChooser` (implemented by `models.Service`), a `/model` command and a select handler; daemon-wide choices say they affect every session.

**Tech Stack:** vanilla JS/CSS (no build step), Go, discordgo v0.29.0.

**Spec:** `docs/superpowers/specs/2026-10-09-model-command-design.md` (§6 Web, Discord)

## Global Constraints

- Worktree `/home/code/development/spore-model-command`, branch `feat/model-command`. Never `git add -A`.
- `web/app.js` api() calls must use literal path templates (internal/daemon/web_test.go checks every one against the daemon's routes).
- Tabs/ops order: `overview, chat, compaction, title, classify, refinement, subagent`. Markers `->` selected, `*` default. Only choosable options (available, or the default) can be picked.
- Discord limits: 5 action rows per message, 25 options per select, option label ≤100 chars, option value ≤100 chars, custom id ≤100 chars, placeholder ≤150 chars. Discord fails an interaction not acknowledged within 3 s: acknowledge first, then do slow work.
- Commit trailer:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_016aHKRsXHkr3iTMvo9YixbC
  ```

## Review Focus

1. A Discord select for a daemon-wide op, pressed inside one thread, must say it changes every session.
2. `/model` in a plain guild channel (no session) must not create a session or thread, and must offer only the daemon-wide ops.
3. A select pressed by a user outside `user_ids` must change nothing.
4. A provider listing more than 25 models must still produce a valid select (selected and default kept).
5. In the web panel, a failed PUT must show the error in the panel, not close it.

---

### Task 1: Web panel

**Files:**
- Modify: `web/index.html`, `web/app.js`, `web/style.css`
- Test: `internal/daemon/web_test.go` (one new test)

**Interfaces:**
- Consumes: `GET /api/models?session={id}` (and `&fresh=1`), `PUT /api/sessions/{id}/model`, `PUT /api/routing?session={id}`; body `{op, ref}`; response `{ops:[{op,scope,selected,default}], groups:[{provider,refs,error}]}`.

- [ ] **Step 1: Failing test** — append to `internal/daemon/web_test.go`:

```go
func TestModelPanelIsWired(t *testing.T) {
	html, err := web.FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{`id="models"`, `id="models-tabs"`, `id="models-body"`, `id="models-note"`, `id="models-refresh"`} {
		if !strings.Contains(string(html), id) {
			t.Errorf("index.html lacks %s", id)
		}
	}
	js, err := web.FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"/model": showModels`, `"/api/models?session={id}"`, `"/api/sessions/{id}/model"`, `"/api/routing?session={id}"`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
}
```

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run 'ModelPanel|AppJSRoutes'` → FAIL (TestModelPanelIsWired).

- [ ] **Step 2: HTML** — in `web/index.html`, after the `help` overlay `</div>` and before `<script`:

```html
<div id="models" class="overlay" role="dialog" aria-modal="true" aria-labelledby="models-title" hidden>
  <div class="panel wide">
    <h2 id="models-title">Models <span class="meta">-&gt; selected · * default</span></h2>
    <div id="models-tabs" class="tabs" role="tablist"></div>
    <div id="models-body"></div>
    <div id="models-errors"></div>
    <p id="models-note" class="note"></p>
    <div class="buttons"><button type="button" id="models-refresh">Refresh</button><button type="button" data-close>Close <kbd class="hint">esc</kbd></button></div>
  </div>
</div>
```

- [ ] **Step 3: CSS** — append to the overlays section of `web/style.css` (after `.overlay .buttons`), and add `gap: 8px;` to the existing `.overlay .buttons` rule:

```css
.overlay .panel.wide { max-width: 720px; }
.tabs { display: flex; flex-wrap: wrap; gap: 4px; margin-bottom: 12px; }
.tabs .tab { font-size: 12px; padding: 4px 10px; }
.tabs .tab.on { background: var(--selected); border-color: var(--accent); color: var(--accent); }
.options { display: flex; flex-direction: column; gap: 4px; }
.options .option { display: flex; gap: 8px; text-align: left; font: 13px var(--mono); padding: 6px 10px; overflow-wrap: anywhere; }
.options .option.on { border-color: var(--accent); }
.options .option .mark { width: 2ch; flex: none; color: var(--accent); }
table.models tr.pick { cursor: pointer; }
table.models td { font: 13px var(--mono); overflow-wrap: anywhere; }
table.models td.meta { font-family: var(--sans); color: var(--dim); }
.note.error { color: var(--error); }
```

- [ ] **Step 4: JS** — in `web/app.js`:

1. Change `const slashCommands = { "/usage": showUsage };` to `const slashCommands = { "/usage": showUsage, "/model": showModels };`.
2. After `usageReport` (end of the usage functions), add:

```js
// ---------- /model ----------

// M is the open /model panel: the view the daemon last sent and the tab on
// screen (0 is the overview; i is view.ops[i - 1]).
const M = { sid: null, view: null, tab: 0, note: "", err: false };

// modelOptions mirrors internal/modelcmd.Options, so every surface lists an
// operation's choices in the same order: the selected model, the default,
// then the rest by provider.
function modelOptions(v, op) {
  const o = v.ops.find((x) => x.op === op);
  if (!o) return [];
  const avail = new Set(v.groups.flatMap((g) => g.refs || []));
  const out = [];
  const seen = new Set();
  const add = (ref) => {
    if (!ref || seen.has(ref)) return;
    seen.add(ref);
    out.push({ ref, selected: ref === o.selected, isDefault: ref === o.default, available: avail.has(ref) });
  };
  add(o.selected);
  add(o.default);
  for (const g of v.groups) for (const r of g.refs || []) add(r);
  return out;
}

const modelWhere = (scope) => (scope === "global" ? "every session" : "this session");

async function showModels(sid) {
  M.sid = sid;
  M.tab = 0;
  M.note = "";
  M.err = false;
  M.view = await api("GET", "/api/models?session={id}", { id: sid });
  renderModels();
  openOverlay("models");
}

async function refreshModels() {
  try {
    M.view = await api("GET", "/api/models?session={id}&fresh=1", { id: M.sid });
    M.note = "";
    M.err = false;
  } catch (err) {
    M.note = err.message;
    M.err = true;
  }
  renderModels();
}

// chooseModel sends one choice. chat and subagent belong to the session;
// the other operations are daemon-wide. The panel stays open either way.
async function chooseModel(op, scope, ref) {
  try {
    M.view = scope === "global"
      ? await api("PUT", "/api/routing?session={id}", { id: M.sid }, { op, ref })
      : await api("PUT", "/api/sessions/{id}/model", { id: M.sid }, { op, ref });
    const o = M.view.ops.find((x) => x.op === op);
    M.note = op + " -> " + o.selected + " (" + modelWhere(o.scope) + ")" + (o.selected === o.default ? ", the default" : "");
    M.err = false;
  } catch (err) {
    M.note = err.message;
    M.err = true;
  }
  renderModels();
}

function renderModels() {
  const v = M.view;
  const names = ["overview", ...v.ops.map((o) => o.op)];
  el("models-tabs").replaceChildren(...names.map((name, i) => h("button", {
    type: "button", role: "tab", class: i === M.tab ? "tab on" : "tab",
    "aria-selected": i === M.tab ? "true" : "false", text: name,
    onclick: () => { M.tab = i; renderModels(); },
  })));
  const body = el("models-body");
  if (M.tab === 0) {
    body.replaceChildren(h("table", { class: "keys models" }, v.ops.map((o, i) => h("tr", {
      class: "pick", onclick: () => { M.tab = i + 1; renderModels(); },
    },
      h("td", { text: o.op }),
      h("td", { text: "-> " + o.selected + (o.selected === o.default ? " *" : "") }),
      h("td", { class: "meta", text: o.selected === o.default ? modelWhere(o.scope) : modelWhere(o.scope) + "; * " + o.default }),
    ))));
  } else {
    const o = v.ops[M.tab - 1];
    body.replaceChildren(h("div", { class: "options" }, modelOptions(v, o.op).map((opt) => h("button", {
      type: "button", class: opt.selected ? "option on" : "option",
      disabled: !(opt.available || opt.isDefault),
      onclick: () => chooseModel(o.op, o.scope, opt.ref),
    },
      h("span", { class: "mark", text: opt.selected ? "->" : "" }),
      h("span", { text: opt.ref + (opt.isDefault ? " *" : "") + (opt.available ? "" : " (unavailable)") }),
    ))));
  }
  el("models-errors").replaceChildren(...v.groups.filter((g) => g.error).map((g) =>
    h("div", { class: "meta", text: "! " + g.provider + ": " + g.error })));
  const note = el("models-note");
  note.textContent = M.note;
  note.className = M.err ? "note error" : "note";
}
```

3. Where the page wires its buttons at startup (next to `el("open-settings").addEventListener(...)`), add:

```js
  el("models-refresh").addEventListener("click", refreshModels);
```

4. In the help table (the array that has `["?", ...]`, `[",", "settings"]`), add `["/model", "choose the model for each operation (type it in the composer)"]`.

- [ ] **Step 5: Run tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/`
Expected: PASS, including `TestAppJSRoutesAreRegistered` and `TestModelPanelIsWired`.

- [ ] **Step 6: Commit**

```bash
git add web/index.html web/app.js web/style.css internal/daemon/web_test.go
git commit -m "web: /model opens a panel to choose each operation's model"   # plus the trailer
```

---

### Task 2: Discord select menus

**Files:**
- Modify: `internal/bridge/discord/client.go` (types, `interactionFrom`, `componentsFor`, its two callers), `internal/bridge/discord/bridge.go` (Options/Bridge field, `/model` dispatch, interaction dispatch), `internal/daemon/server.go` (accessor), `cmd/spore/wire.go` (pass the service)
- Create: `internal/bridge/discord/model.go`, `internal/bridge/discord/model_test.go`
- Modify tests: any `componentsFor(` call in `internal/bridge/discord/*_test.go` gains a second `nil` argument.

**Interfaces:**
- Consumes: `models.View`, `modelcmd.Options`, `(modelcmd.Option).Choosable`, `modelcmd.Overview`, `modelcmd.Confirm`, `router.IsGlobalSite`, `(*store.Store).SessionForExternal(ctx, bridgeName, channelID) (string, bool, error)`.
- Produces: `discord.ModelChooser`, `discord.Options.Models`, `discord.Select`, `discord.SelectOption`, `Message.Selects`, `Interaction.Values`, `(*daemon.Server).Models() *models.Service`.

- [ ] **Step 1: Failing tests** — `internal/bridge/discord/model_test.go`:

```go
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
```

(`waitFor`, `newTestBridge`, `f.sentTo`, `f.deliver`, `f.press`, `f.threads`, `f.responds`, `turns.startCount()` already exist in the package's tests; read `fake_test.go` for their exact shapes before relying on them.)

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -run 'Model|Selects'` → FAIL to compile.

- [ ] **Step 2: Client types and components** — `internal/bridge/discord/client.go`:

1. `Interaction` gains, after `CustomID`:

```go
	// Values is what a select menu was set to; empty for a button.
	Values []string
```

2. After the `Button` type:

```go
// Select is a string select menu. Each takes an action row of its own.
type Select struct {
	CustomID    string
	Placeholder string
	Options     []SelectOption
}

// SelectOption is one entry of a Select; Default marks it chosen when the
// menu is drawn.
type SelectOption struct {
	Label   string
	Value   string
	Default bool
}
```

3. `Message` gains `Selects []Select` after `Buttons`.

4. In `interactionFrom`, replace the `out.CustomID = ...` line with:

```go
		data := i.MessageComponentData()
		out.CustomID = data.CustomID
		out.Values = data.Values
```

5. Replace `componentsFor` with:

```go
// componentsFor translates the bridge's selects and buttons into discordgo
// action rows: one row per select first, then buttons in rows of five,
// capped at Discord's five rows per message. Anything past the cap is
// dropped; callers that need more split across messages.
func componentsFor(buttons []Button, selects []Select) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	for _, s := range selects {
		if len(rows) == maxRows {
			break
		}
		opts := make([]discordgo.SelectMenuOption, 0, len(s.Options))
		for _, o := range s.Options {
			opts = append(opts, discordgo.SelectMenuOption{Label: truncate(o.Label, 100), Value: o.Value, Default: o.Default})
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    s.CustomID,
			Placeholder: truncate(s.Placeholder, 150),
			Options:     opts,
		}}})
	}
	for i := 0; i < len(buttons) && len(rows) < maxRows; i += buttonsPerRow {
		end := min(i+buttonsPerRow, len(buttons))
		var comps []discordgo.MessageComponent
		for _, b := range buttons[i:end] {
			style := discordgo.SecondaryButton
			if b.Danger {
				style = discordgo.DangerButton
			}
			comps = append(comps, discordgo.Button{CustomID: b.CustomID, Label: b.Label, Style: style})
		}
		rows = append(rows, discordgo.ActionsRow{Components: comps})
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}
```

6. Its two callers in `Send` and `Edit` become `componentsFor(m.Buttons, m.Selects)`. Update every `componentsFor(` call in the package's tests to pass `nil` as the new second argument.

- [ ] **Step 3: `internal/bridge/discord/model.go`**:

```go
package discord

import (
	"context"
	"log/slog"
	"strings"

	"github.com/codered/spore/internal/modelcmd"
	"github.com/codered/spore/internal/models"
	"github.com/codered/spore/internal/router"
)

// ModelChooser is /model's service as the bridge sees it. models.Service
// implements it.
type ModelChooser interface {
	View(ctx context.Context, sessionID string, fresh bool) (models.View, error)
	Set(ctx context.Context, sessionID, op, ref string) (models.View, error)
}

// modelPrefix marks a /model select's custom id: prefix, op, session.
const modelPrefix = "spore-model:"

// Discord's limits for a select menu.
const (
	maxSelectOptions = 25
	maxSelectValue   = 100
)

// modelGroups splits the selects across messages: Discord allows five rows
// per message, and there are six operations.
var modelGroups = [][]string{{"chat", "subagent", "compaction", "title"}, {"classify", "refinement"}}

func modelCustomID(sessionID, op string) string { return modelPrefix + op + ":" + sessionID }

func isModelCustomID(s string) bool { return strings.HasPrefix(s, modelPrefix) }

// decodeModelCustomID returns the session and op a select was drawn for.
func decodeModelCustomID(s string) (sessionID, op string) {
	op, sessionID, _ = strings.Cut(strings.TrimPrefix(s, modelPrefix), ":")
	return sessionID, op
}

// handleModel answers /model with the overview and a select per operation.
// Like /usage it never opens a session: outside a bound DM or thread there
// is no session, so only the daemon-wide operations are offered.
func (b *Bridge) handleModel(in Inbound) {
	if b.models == nil {
		b.say(in.ChannelID, "model selection is not available")
		return
	}
	sid, found, err := b.store.SessionForExternal(b.ctx, bridgeName, in.ChannelID)
	if err != nil {
		slog.Warn("discord /model: look up session", "err", err)
		return
	}
	if !found {
		sid = ""
	}
	v, err := b.models.View(b.ctx, sid, false)
	if err != nil {
		b.say(in.ChannelID, "could not list models: "+err.Error())
		return
	}
	for i, group := range modelGroups {
		var m Message
		if i == 0 {
			m.Content = "```\nModels per operation (-> selected, * default):\n" + modelcmd.Overview(v)
			for _, g := range v.Groups {
				if g.Error != "" {
					m.Content += "! " + g.Provider + ": " + g.Error + "\n"
				}
			}
			m.Content += "```"
			if sid == "" {
				m.Content += "\nchat and subagent are chosen per session: use /model inside a thread or DM."
			}
		}
		for _, op := range group {
			if sid == "" && !router.IsGlobalSite(op) {
				continue
			}
			if s, ok := modelSelect(v, sid, op); ok {
				m.Selects = append(m.Selects, s)
			}
		}
		if m.Content == "" && len(m.Selects) == 0 {
			continue
		}
		if _, err := b.client.Send(b.ctx, in.ChannelID, m); err != nil {
			slog.Warn("discord /model: send", "err", err)
		}
	}
}

// modelSelect is one operation's menu: the choosable options in the order
// every surface uses, capped at Discord's 25 (the selected model and the
// default come first, so they survive the cap).
func modelSelect(v models.View, sessionID, op string) (Select, bool) {
	var opts []SelectOption
	for _, o := range modelcmd.Options(v, op) {
		if !o.Choosable() || len(o.Ref) > maxSelectValue {
			continue
		}
		label := o.Ref
		if o.Selected {
			label = "-> " + label
		}
		if o.Default {
			label += " *"
		}
		opts = append(opts, SelectOption{Label: label, Value: o.Ref, Default: o.Selected})
		if len(opts) == maxSelectOptions {
			break
		}
	}
	if len(opts) == 0 {
		return Select{}, false
	}
	placeholder := op
	for _, o := range v.Ops {
		if o.Op == op {
			placeholder = op + ": -> " + o.Selected
		}
	}
	return Select{CustomID: modelCustomID(sessionID, op), Placeholder: placeholder, Options: opts}, true
}

// chooseModel applies a select. The catalog check can take seconds and
// Discord fails an interaction not acknowledged within three, so it
// acknowledges first and reports the outcome in the channel.
func (b *Bridge) chooseModel(i Interaction) {
	sessionID, op := decodeModelCustomID(i.CustomID)
	if b.models == nil || len(i.Values) != 1 {
		if err := b.client.Respond(b.ctx, i.ID, i.Token, "model selection is not available here"); err != nil {
			slog.Warn("discord /model: respond", "err", err)
		}
		return
	}
	ref := i.Values[0]
	if err := b.client.Respond(b.ctx, i.ID, i.Token, "choosing "+ref+" for "+op+"…"); err != nil {
		slog.Warn("discord /model: respond", "err", err)
	}
	v, err := b.models.Set(b.ctx, sessionID, op, ref)
	if err != nil {
		b.say(i.ChannelID, "could not choose "+ref+" for "+op+": "+err.Error())
		return
	}
	msg := strings.TrimSpace(modelcmd.Confirm(v, op))
	if router.IsGlobalSite(op) {
		msg += " — this changes " + op + " for every session, not just this thread"
	}
	b.say(i.ChannelID, msg)
}
```

- [ ] **Step 4: Bridge wiring** — `internal/bridge/discord/bridge.go`:

1. `Options` gains, after `ShowCost`:

```go
	// Models serves /model. Nil means /model says it is not available.
	Models ModelChooser
```

2. `Bridge` gains a `models ModelChooser` field; `New` sets it from `opts.Models`.
3. In `handleMessage`, after the `/usage` block, add:

```go
	if content == "/model" {
		b.handleModel(in)
		b.settle(in) // no turn runs, so nothing else settles it; see /new
		return
	}
```

4. In `handleInteraction`, right after the `isDetailsCustomID` block:

```go
	if isModelCustomID(i.CustomID) {
		b.chooseModel(i)
		return
	}
```

`internal/daemon/server.go`, next to `Store()`/`Guard()`/`Broker()`:

```go
func (s *Server) Models() *models.Service { return s.models }
```

`cmd/spore/wire.go`, in `buildBridge`, build the options into a variable and pass the service only when set (a nil `*models.Service` stored in the interface would not compare equal to nil):

```go
	opts := discord.Options{
		Cfg: d, Client: client, Turns: srv, Sessions: srv,
		Store: srv.Store(), Broker: srv.Broker(), Guard: srv.Guard(),
		ShowCost: cfg.ShowCost,
	}
	if m := srv.Models(); m != nil {
		opts.Models = m
	}
	return discord.New(opts)
```

- [ ] **Step 5: README** — in the `/model` section added in stage 1, append:

```markdown
In Discord, `/model` replies with the overview and a menu per operation;
inside a thread or DM it covers that conversation's session, elsewhere only
the operations chosen for every session. Nothing calls the `classify`
operation yet, so choosing a model for it has no effect today. A sub-agent
keeps the model it was launched on, even if its parent's choice changes
later.
```

- [ ] **Step 6: Run tests and gate**

```bash
go test -tags sqlite_fts5 -race ./internal/bridge/discord/ ./internal/daemon/ ./cmd/spore/
go vet -tags sqlite_fts5 ./...
make fmtcheck
make lint
go test -tags sqlite_fts5 ./...
```

- [ ] **Step 7: Commit**

```bash
git add internal/bridge/discord/client.go internal/bridge/discord/bridge.go internal/bridge/discord/model.go internal/bridge/discord/model_test.go internal/daemon/server.go cmd/spore/wire.go README.md
# plus any internal/bridge/discord/*_test.go whose componentsFor call you updated, by name
git commit -m "discord: /model replies with a menu per operation"   # plus the trailer
```

# /skills Command Implementation Plan

> **Mode:** TDD steps. Checkboxes track progress.

**Goal:** `/skills` lists cached skills with body sizes, loaded markers and
per-file errors; the prompt index bug (`Snapshot.Skills` never filled) is
fixed first so the command ships against a system that actually names skills
to the model.

**Architecture:** Client-intercepted command, one new read endpoint
`GET /api/sessions/{id}/skills`, server-computed payload, one render
function shared by the Bubble Tea loop and the plain loop.

---

### Task 1: The prompt index fix — Agent holds the caches

- [ ] **Step 1: Write failing test for a skills index in the assembled
  prompt**

In `internal/agent/context_test.go` (or `agent_test.go`): build an agent
with `Skills` set to a real `skill.NewCaches()` pointing at a temp dir
containing one valid skill file, call `Snapshot`, `Assemble`, assert the
system prompt contains the skill name and description.

- [ ] **Step 2: Run test, verify it fails** — `Agent` has no `Skills`
field.

- [ ] **Step 3: Implement the field and the Snapshot fill**

`internal/agent/agent.go`:

```go
// Skills is the skills cache set shared with the skill tools, attached
// by buildAgent. Nil means a test built the Agent with New directly.
Skills *skill.Caches
```

and inside `Snapshot`:

```go
if a.Skills != nil {
    dir := a.Cfg.SkillsDir(policy.WorkspaceFrom(ctx))
    snap.Skills = a.Skills.Skills(dir)
}
```

- [ ] **Step 4: Run test, verify it passes**

- [ ] **Step 5: Wire production construction**

In `cmd/spore/wire.go`: `buildAgent` creates the `skill.NewCaches()` set,
passes it into `buildTools` (replacing the private `NewCaches()` there),
and sets `a.Skills = skillsCache` next to `a.Facts = facts`.

---

### Task 2: The endpoint

- [ ] **Step 1: Write failing daemon test for the payload**

In `internal/daemon/sessions_test.go`: create session, temp skills dir with
two valid files + one broken frontmatter file, append one assistant message
containing a `skill_load` tool_use block, `GET /api/sessions/{id}/skills`,
assert: valid skills listed with `body_tokens > 0`, one marked `loaded`,
`errors` non-empty.

- [ ] **Step 2: Run test, verify it fails** — route missing.

- [ ] **Step 3: Implement the handler + payload types**

`internal/daemon/skills.go`:

```go
type SkillJSON struct {
    Name        string `json:"name"`
    Description string `json:"description"`
    BodyTokens  int    `json:"body_tokens"`
    Loaded      bool   `json:"loaded"`
}

type SkillsJSON struct {
    Skills []SkillJSON `json:"skills"`
    Errors []string    `json:"errors"`
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request)
```

Collect: dir from `s.cfg.SkillsDir` on the session's workspace; messages
from `s.store.Messages`; scan Blocks for `tool_use` named `skill_load`,
decode `{"name": ...}`; size each cached skill with
`agent.EstimateTokens(sk.Body)`; carry the cache's `Errors` as strings.

Register in `server.go`: `mux.HandleFunc("GET /api/sessions/{id}/skills", s.handleSkills)`.

- [ ] **Step 4: Run test, verify it passes**

---

### Task 3: The client and the TUI

- [ ] **Step 1: Write failing render test**

In `cmd/spore/tui_test.go`: construct a `skillListJSON` payload literal,
call `renderSkills`, assert the loaded marker, the token estimate, and the
error line all appear.

- [ ] **Step 2: Run, verify it fails**

- [ ] **Step 3: Implement**

`cmd/spore/client.go`:

```go
type skillListJSON struct { ... matching daemon payload ... }

func (c *client) listSkills(ctx context.Context, sessionID string) (skillListJSON, error)
```

`cmd/spore/tui.go`: `handleSkills` issues `c.listSkills`, emits
`slashSkillsMsg{list}`; `renderSkills(list)` formats and flushes; `slashDesc`
gains `"skills"` → `"list skills"`; the hint list gains `"skills"`.

`cmd/spore/chat.go`: `slashHandler` gains `case "skills": return ui.handleSkills(...)`.

- [ ] **Step 4: Run, verify it passes**

---

### Task 4: Backlog and verification

- [ ] **Step 1: Amend `docs/backlog.md` chat commands section** — close the
section as shipped and remove the `/skills` open question; note the prompt
index bug was found and fixed by this change.

- [ ] **Step 2: Full verification**

`go test ./...` and `go build ./cmd/spore`.

- [ ] **Step 3: Commit**

One commit: the fix, the endpoint, the client, the docs, spec and plan.

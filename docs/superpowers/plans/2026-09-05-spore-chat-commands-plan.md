# Chat Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Four slash commands (`/clear`, `/compact`, `/context`, `/usage`) intercepted in the Bubble Tea chat UI. `/compact` gets one new daemon endpoint; the others use existing endpoints.

**Architecture:** Commands are client-side bytes the model intercepts before `c.send()`. `/clear` move the summary boundary server-side via existing PATCH. `/context` and `/usage` compute aggregation from the existing GET transcript. `/compact` triggers `MaybeCompact` server-side via one new endpoint.

**Tech Stack:** Go, Bubble Tea, `internal/daemon`, `internal/agent`, `internal/store`, `internal/policy`, `internal/llm`.

---

### Task 1: Summary Boundary — client sets the cut point

**Files:**
- Modify: `internal/store/store.go` — add `SetSummaryThrough`
- Modify: `internal/store/store_test.go` — test boundary move
- Modify: `internal/daemon/sessions.go` — generalize PATCH to accept summary fields (already accepts workspace)
- Modify: `cmd/spore/tui.go` — wire `/clear` through the model
- Modify: `cmd/spore/tui_test.go` — harness test for `/clear`

- [ ] **Step 1: Write failing test for `SetSummaryThrough`**

Test: when called, `summaries.through_seq` is updated for the session.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store -run TestSetSummaryThrough -v`
Expected: FAIL — `SetSummaryThrough` does not exist.

- [ ] **Step 3: Implement `SetSummaryThrough`**

```go
// SetSummaryThrough is like RecordSummary but without a summary row: useful when the
// caller simply wants to move the boundary, not to store a new summary.
func (s *Store) SetSummaryThrough(sessionID string, throughSeq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		"INSERT INTO summaries (session_id, through_seq, summary) VALUES (?, ?, ?) "+
			"ON CONFLICT (session_id) DO UPDATE SET through_seq = excluded.through_seq",
		sessionID, throughSeq, "",
	)
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store -run TestSetSummaryThrough -v`
Expected: PASS

- [ ] **Step 5: Write failing test for `/clear` handler**

Test: PATCH with `summary_through` field sets the boundary.

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/daemon -run TestClearSummaryPatch -v`
Expected: FAIL — `summary_through` not accepted or handler rejects.

- [ ] **Step 7: Generalize PATCH handler**

Modify `patchSession` in `internal/daemon/sessions.go`:

```go
var fields struct {
	Workspace      *string `json:"workspace,omitempty"`
	SummaryThrough *int64  `json:"summary_through,omitempty"`
}
...
if fields.Workspace != nil {
	// existing workspace logic
}
if fields.SummaryThrough != nil {
	if *fields.SummaryThrough < 0 {
		s.writeErr(w, http.StatusBadRequest, "summary_through must be >= 0")
		return
	}
	if err := s.store.SetSummaryThrough(id, *fields.SummaryThrough); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
}
```

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./internal/daemon -run TestClearSummaryPatch -v`
Expected: PASS

- [ ] **Step 9: Wire `/clear` through chatUI**

Add command dispatch in `handleKey` before `submit`:

```go
// Intercept slash commands before the regular send path.
text := strings.TrimSpace(m.ta.Value())
if strings.HasPrefix(text, "/") {
	m.ta.Reset()
	m.ta.SetHeight(1)
	return m.runSlash(text)
}
```

Add `runSlash` on `chatUI`:

```go
func (m *chatUI) runSlash(input string) tea.Cmd {
	cmd, rest := strings.CutPrefix(input, "/")
	cmd = strings.ToLower(cmd)
	args := strings.TrimSpace(rest)
	switch cmd {
	case "clear":
		// Compute latest message seq.
		resp, err := m.sendSplash("GET", "/api/sessions/"+m.sessionID, nil)
		if err != nil { return m.flush(styDanger.Render("  ✗ " + err.Error())) }
		var doc struct {
			Session struct{ SummaryThrough int64 `json:"summary_through"` } `json:"session"`
			Messages []struct{ Seq int64 } `json:"messages"`
		}
		if err := json.Unmarshal(resp, &doc); err != nil { return m.flush(styDanger.Render("  ✗ " + err.Error())) }
		latest := doc.Session.SummaryThrough
		for _, msg := range doc.Messages {
			if msg.Seq > latest { latest = msg.Seq }
		}
		reqBody, _ := json.Marshal(map[string]int64{"summary_through": latest})
		if _, err := m.sendSplash("PATCH", "/api/sessions/"+m.sessionID, reqBody); err != nil {
			return m.flush(styDanger.Render("  ✗ clear failed: " + err.Error()))
		}
		return m.flush(styAccent.Render("  · summary boundary moved to seq" + fmt.Sprint(latest)))
	...
```

Add a `sendSplash` method to `chatUI` for direct HTTP calls that bypass the
normal turn flow. The client method is fine on `client` (in the model) but
direct access is simpler. (Or just add an exported `Client` method on the
daemon client; simpler in practice.)

- [ ] **Step 10: Write harness test for `/clear` dispatch**

Add test asserting `/clear` emits a PATCH (or mocked send call) and shows
the confirmation line in the transcript.

- [ ] **Step 11: Run tests**

Run: `go test ./cmd/spore -v`
Expected: PASS

- [ ] **Step 12: Commit**

Run: `git add internal/store/ internal/daemon/ cmd/spore/`
Run: `git commit -m "feat: summary boundary for /clear; generalize PATCH"`

---

### Task 2: `/compact` — new daemon endpoint

**Files:**
- Modify: `internal/daemon/sessions.go` — POST /compact route and handler
- Modify: `internal/daemon/server.go` — register route
- Modify: `internal/daemon/api_test.go` — test for compact endpoint
- Modify: `cmd/spore/tui.go` — wire `/compact` in runSlash
- Modify: `cmd/spore/tui_test.go` — harness test for `/compact`

- [ ] **Step 1: Write failing test**

Test: POST to `/api/sessions/{id}/compact` returns 200 with summary boundary.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon -run TestCompactEndpoint -v`
Expected: FAIL — route does not exist.

- [ ] **Step 3: Implement compact handler**

In `internal/daemon/sessions.go`:

```go
type compactResponse struct {
	ThroughSeq int64 `json:"through_seq"`
	Tokens     int   `json:"tokens"`
}

func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request, sessionID string) {
	a, err := s.agentFor(sessionID)   // existing helper
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	if err := a.MaybeCompact(r.Context(), sessionID); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	snap, err := a.Snapshot(r.Context(), sessionID)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tok := 0
	// recompute estimate
	notCompact, tokens := agent.Assemble(snap, agent.ContextConfig{MaxTokens: 65536, CompactAt: 0.75})
	if notCompact {
		// MaybeCompact did not fire; treat as estimated only.
	} else {
		// through_seq moved to snap.LatestSeq
	}
	s.writeJSON(w, http.StatusOK, compactResponse{
		ThroughSeq: snap.LatestSeq,
		Tokens:     tokens,
	})
}
```

Register: in `serveMux` route table:
```go
mux.HandleFunc("POST /api/sessions/{id}/compact", s.compact)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon -run TestCompactEndpoint -v`
Expected: PASS

- [ ] **Step 5: Wire `/compact` through runSlash**

```go
case "compact":
	resp, err := m.sendSplash("POST", "/api/sessions/"+m.sessionID+"/compact", nil)
	if err != nil { return m.flush(styDanger.Render("  ✗ compact failed: " + err.Error())) }
	var out struct {
		ThroughSeq int64 `json:"through_seq"`
		Tokens     int   `json:"tokens"`
	}
	if err := json.Unmarshal(resp, &out); err != nil { return m.flush(styDanger.Render("  ✗ " + err.Error())) }
	return m.flush(styAccent.Render(fmt.Sprintf("  · summary moved to seq %d, prompt now ~%d tokens", out.ThroughSeq, out.Tokens)))
```

- [ ] **Step 6: Write harness test for `/compact`**

Test dispatch and confirmation message.

- [ ] **Step 7: Run tests**

Run: `go test ./cmd/spore -v`
Expected: PASS

- [ ] **Step 8: Commit**

Run: `git add internal/daemon/ cmd/spore/`
Run: `git commit -m "feat: compact endpoint and /compact command"`

---

### Task 3: `/context` — client-side token breakdown

**Files:**
- Modify: `cmd/spore/tui.go` — wire `/context` in runSlash
- Modify: `cmd/spore/tui_test.go` — harness test for `/context`

- [ ] **Step 1: Write harness test**

Test: `/context` renders the five-part breakdown (system, environment, facts, summary, messages, total).

- [ ] **Step 2: Implement `/context` in runSlash**

```go
case "context":
	resp, err := m.sendSplash("GET", "/api/sessions/"+m.sessionID, nil)
	if err != nil { return m.flush(styDanger.Render("  ✗ " + err.Error())) }
	var doc transcriptResponse
	if err := json.Unmarshal(resp, &doc); err != nil { return m.flush(styDanger.Render("  ✗ " + err.Error())) }
	snap := agent.Snapshot{
		SessionID:    doc.Session.ID,
		System:       agent.SystemPrompt, // or wherever System reaches the client
		Environment:  doc.Environment,
		Facts:        doc.Facts,
		Summary:      doc.Summary,
		LatestSeq:    0, // max seq in messages
		ContextLimit: doc.ContextLimit,
	}
	for _, msg := range doc.Messages {
		if msg.Seq > snap.LatestSeq { snap.LatestSeq = msg.Seq }
	}
	parts, total := agent.SnapshotTokens(snap, agent.ContextConfig{MaxTokens: snap.ContextLimit})
	_ = parts; _ = total
	// Render parts as transcript lines.
	return m.flush(renderContext(parts, total, snap.ContextLimit))
```

Note: the transcript response does not currently include environment or
fact content. Either we extend `GET /api/sessions/{id}` to include them, or
the client does not show a truly accurate breakdown. We should create
`internal/agent/SystemPrompt` and extend the transcript endpoint to carry
`environment` and `facts` in the response.

Fix: extend `transcriptResponse` with `environment` and `facts` fields.

- [ ] **Step 3: Write harness test for extended transcript**

Test the new fields appear in the JSON.

- [ ] **Step 4: Extend `transcript` handler**

Add `Environment string` and `Facts string` to `transcriptResponse`, then
populate from `snap.Environment` and `snap.Facts`.

Run tests; expected: PASS (handler populates new fields correctly).

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/spore ./internal/daemon -v`
Expected: PASS

- [ ] **Step 6: Commit**

Run: `git add internal/daemon/ cmd/spore/`
Run: `git commit -m "feat: /context command with full prompt part breakdown"`

---

### Task 4: `/usage` — client-side aggregate

**Files:**
- Modify: `cmd/spore/tui.go` — wire `/usage` in runSlash
- Modify: `cmd/spore/tui_test.go` — harness test for `/usage`

- [ ] **Step 1: Write harness test**

Test: `/usage` renders session and total aggregates.

- [ ] **Step 2: Implement `/usage`**

```go
case "usage":
	resp, err := m.sendSplash("GET", "/api/sessions/"+m.sessionID, nil)
	if err != nil { return m.flush(styDanger.Render("  ✗ " + err.Error())) }
	var doc transcriptResponse
	if err := json.Unmarshal(resp, &doc); err != nil { return m.flush(styDanger.Render("  ✗ " + err.Error())) }
	var in, out int
	var cost float64
	var msgCount int
	for _, msg := range doc.Messages {
		in += msg.TokensIn
		out += msg.TokensOut
		cost += msg.CostUSD
		msgCount++
	}
	// Render the totals.
	return m.flush(renderUsage(in, out, cost, msgCount))
```

- [ ] **Step 3: Run tests**

Run: `go test ./cmd/spore -v`
Expected: PASS

- [ ] **Step 4: Commit**

Run: `git add cmd/spore/`
Run: `git commit -m "feat: /usage command aggregating tokens and cost"`

---

### Task 5: Help hint — show the available commands

**Files:**
- Modify: `cmd/spore/tui.go` — hint line
- Modify: `cmd/spore/tui_style.go` — helper rendering hints
- Modify: `cmd/spore/tui_test.go` — test hint strings

- [ ] **Step 1: Write harness test**

Test: `hint()` includes all command names.

- [ ] **Step 2: Update hint()**

```go
func (m *chatUI) hint() string {
	parts := []string{
		styKey.Render("enter") + styMuted.Render(" send"),
		styKey.Render("ctrl+j") + styMuted.Render(" newline"),
		styKey.Render("↑↓") + styMuted.Render(" history"),
		styKey.Render("/clear") + styMuted.Render(" clear"),
		styKey.Render("/compact") + styMuted.Render(" compact"),
		styKey.Render("/context") + styMuted.Render(" context"),
		styKey.Render("/usage") + styMuted.Render(" usage"),
		styKey.Render("ctrl+c") + styMuted.Render(" quit"),
	}
	...
```

- [ ] **Step 3: Run tests**

Run: `go test ./cmd/spore -run TestViewShowsThePrompt -v`
Expected: PASS

- [ ] **Step 4: Commit**

Run: `git add cmd/spore/`
Run: `git commit -m "feat: hint line shows available slash commands"`

---

### Task 6: Final verification

- [ ] **Step 1: Run full test suite**

Run: `make test`
Expected: all packages PASS

- [ ] **Step 2: Run lint and build**

Run: `make vet`
Run: `make build`
Expected: both PASS

- [ ] **Step 3: Final review — dispatch a code reviewer**

Use requesting-code-review on the built code.

- [ ] **Step 4: Commit any fixes and merge via PR**

---

### Files (Summary)

| Path | Action |
|------|--------|
| `internal/store/store.go` | Add `SetSummaryThrough` |
| `internal/store/store_test.go` | Test boundary move |
| `internal/daemon/sessions.go` | Generalize PATCH; add `/compact` handler; extend transcript |
| `internal/daemon/server.go` | Register `/compact` route |
| `internal/daemon/api_test.go` | Tests for PATCH summary, `/compact`, extended transcript |
| `cmd/spore/tui.go` | `runSlash` dispatch; implement `/clear`, `/compact`, `/context`, `/usage`; update `hint` |
| `cmd/spore/tui_test.go` | Harness tests for all four commands |
| `cmd/spore/tui_style.go` | Updated hint rendering |

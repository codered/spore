# Clear Command Boundary Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `/clear` remove current context without removing future user messages.

**Architecture:** The daemon owns a new clear operation. The store atomically snapshots the current maximum message sequence and moves the summary boundary to it. Transcript responses expose the boundary so `/context` can report live context while `/usage` remains historical.

**Tech Stack:** Go, SQLite, net/http, Bubble Tea, sqlite_fts5 tests

---

### Task 1: Add the atomic store operation

**Files:**
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go`

- [ ] Write a failing test for clearing through the current last sequence and repairing an oversized boundary.
- [ ] Run the focused store test and confirm the expected failure.
- [ ] Add the transactional store method.
- [ ] Run the focused store test and confirm it passes.

### Task 2: Add the daemon clear endpoint

**Files:**
- Modify: `internal/daemon/server.go`
- Modify: `internal/daemon/sessions.go`
- Test: `internal/daemon/sessions_test.go`

- [ ] Write failing endpoint tests for boundary selection and the next turn after clear.
- [ ] Run the focused daemon tests and confirm the expected failure.
- [ ] Add `POST /api/sessions/{id}/clear`.
- [ ] Expose `summary_through` in `TranscriptJSON`.
- [ ] Run the focused daemon tests and confirm they pass.

### Task 3: Update the CLI and context report

**Files:**
- Modify: `cmd/spore/client.go`
- Modify: `cmd/spore/tui.go`
- Test: `cmd/spore/tui_test.go`

- [ ] Write failing tests for the clear request and live-context filtering.
- [ ] Run the focused CLI tests and confirm the expected failure.
- [ ] Replace the client-selected boundary with the clear endpoint.
- [ ] Filter `/context` by `summary_through`.
- [ ] Remove the unused `math` import.
- [ ] Run the focused CLI tests and confirm they pass.

### Task 4: Verify and commit

- [ ] Run `gofmt` on changed Go files.
- [ ] Run `go test -tags sqlite_fts5 ./...`.
- [ ] Run `go vet -tags sqlite_fts5 ./...`.
- [ ] Run `go build -tags sqlite_fts5 ./cmd/spore`.
- [ ] Review the final diff and commit it.

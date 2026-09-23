# TUI Views 2a-1 — Worker Plan

> **For the worker:** follow this document from top to bottom. Every step is a command and its exact expected output. You will not write or edit code: each task is a script that makes its own edits and checks them. Do only what the steps say.

**Goal:** Give `spore chat` a k9s-style header with key hints, and full-screen resource views for skills, sub-agents, usage and jobs, with stop-agent and cancel-job actions.

**Architecture:** A generic resource framework in `internal/tui`. Each view is a `Resource` (columns, fetch, detail, actions), rendered by one `table` component that handles movement, filtering, sorting, the detail pane and confirmation. The model gains a `table` field (nil means the chat screen) and a 2-second refresh tick. The daemon gains `GET /api/usage` and an optional skill body on the skills endpoint. The `cmd/spore` adapter implements a new `tui.Views` interface.

**Tech Stack:** Go, Bubble Tea v1.3, bubbles, lipgloss, `charmbracelet/x/ansi`, SQLite (`-tags sqlite_fts5`). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-22-tui-views-design.md`. **Source plan:** `docs/superpowers/plans/2026-09-22-tui-views-2a1.md`. That plan's code is embedded in the task scripts in this directory. The work is regrouped as vertical slices, and two defects in the source plan are fixed (see "Changes from the source plan" at the end).

## How this plan works

- `tasks/task_NN_*.py` is one task. It applies its own edits through `hashline`, which rejects an edit if the file is not what the script expects. It then runs that task's gates: T0 checks that the edit landed, T1 runs the touched package's tests, and T2 makes a real call through a real daemon.
- `step.py NN --red` writes only the task's tests and shows that they fail before the code exists.
- `step.py NN` applies the rest and prints one `PASS`/`FAIL` line per gate. It is idempotent: running it again on a finished task changes nothing.
- `commit.py NN` commits exactly that task's files with exactly its message. It refuses if any other file changed.
- `final_gate.py` checks every requirement and ends with `ALL CHECKS PASS`.

Exit codes of `step.py NN`: `exit 0` means done and verified. `exit 1` means a gate failed. `exit 3` means drift: a file is not what the plan expected. For any exit other than 0, stop and report.

## Global constraints

- Run every command from the repository root, on branch `tui-views`.
- Do not edit any file by hand: not the Go code, not the task scripts, not this plan. The scripts are the only thing that edits code.
- Change only what a step names. Do not "fix" anything a step did not ask for, including lint findings.
- One commit per task, made only by `commit.py NN`. Never amend, rebase, reset, squash, split, or `git add` anything yourself. Never `git add -A` or `git add .`: the working tree holds the user's untracked `.agents/` and `fix_task1.py`, which must never be committed.
- Do not push. Do not merge. Do not open a PR. Do not restart or stop any running `spore` daemon.
- If a command's output differs from its expected output, stop. Do not retry with changes. Report the step, the command, and the full output.
- Test durations and the order of lines inside `--- details` do not matter. Every other character does.

## Step 0: starting state

Check the branch, the commit, the working tree, your git identity and the tools:

```bash
git rev-parse --abbrev-ref HEAD
git log -1 --format=%s
git rev-parse HEAD~1
git status --porcelain --untracked-files=no
git config user.email >/dev/null && echo "git identity set"
for t in go gofmt hashline golangci-lint govulncheck make python3; do command -v "$t" >/dev/null || echo "missing: $t"; done; echo "tools checked"
```

Expected output, exactly:

```text
tui-views
plan: TUI views 2a-1 as vertical-slice task scripts for a worker
174b2c1dc482c9f213d8c3f0f2b0feee2c801546
git identity set
tools checked
```

Check that the packages this plan touches pass before you start:

```bash
go test -tags sqlite_fts5 -count=1 ./internal/store/ ./internal/daemon/ ./internal/tui/ ./cmd/spore/ 2>&1 | awk '{print $1, $2}'
```

Expected output, exactly:

```text
ok github.com/codered/spore/internal/store
ok github.com/codered/spore/internal/daemon
ok github.com/codered/spore/internal/tui
ok github.com/codered/spore/cmd/spore
```

If any output differs, stop and report. Do not start Task 01.

---

## Task 01: the view data path, store to adapter

- [ ] **Task 01 — The view data path, store to adapter** <!-- task_01_usage_reaches_the_adapter.py -->

This adds `store.DailyUsage`, `GET /api/usage`, the skill `body` field returned for `?body=1`, the `tui.Views` interface, and the adapter's Views methods. The adapter is tested against a real daemon over real HTTP.

**Step 1.1: red.** Write the tests and show that they fail for the right reason:

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 01 --red
```

Expected output, exactly:

```text
RED as expected: the store test needs DailyUsage
RED as expected: the daemon tests need UsageJSON and SkillJSON.Body
RED as expected: the adapter tests need the Views methods
```

**Step 1.2: green.** Apply the code and run the gates:

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 01
```

Expected output, exactly:

```text
exit 0
T0 PASS route registered at its call site
T0 PASS skills handler reads ?body=1
T0 PASS adapter asserts it implements tui.Views
T0 PASS gofmt clean
T1 PASS store and daemon tests, race
T1 PASS tui still builds and passes
T2 PASS adapter reads usage, skills, agents and jobs from a real daemon; an old daemon says so
T1 PASS cmd/spore tests, race
```

**Step 1.3: commit.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/commit.py 01 && git status --porcelain --untracked-files=no
```

Expected output, exactly:

```text
committed task 01: daemon: GET /api/usage and skill bodies; the adapter serves the views over them
  cmd/spore/tui_backend.go
  cmd/spore/tui_e2e_test.go
  internal/daemon/server.go
  internal/daemon/skills.go
  internal/daemon/skills_test.go
  internal/daemon/usage.go
  internal/daemon/usage_test.go
  internal/store/usage.go
  internal/store/usage_test.go
  internal/tui/views.go
```

---

## Task 02: the resource framework and the table

- [ ] **Task 02 — Resource framework and table** <!-- task_02_resource_framework_and_table.py -->

**Step 2.1: red.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 02 --red
```

Expected output, exactly:

```text
RED as expected: the table tests need newTable
```

**Step 2.2: green.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 02
```

Expected output, exactly:

```text
exit 0
T0 PASS internal/tui/resource.go exists
T0 PASS internal/tui/table.go exists
T0 PASS internal/tui/table_test.go exists
T0 PASS gofmt clean
T1 PASS the seven table tests pass
T1 PASS tui tests, race
```

**Step 2.3: commit.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/commit.py 02 && git status --porcelain --untracked-files=no
```

Expected output, exactly:

```text
committed task 02: tui: the resource framework and the table every view runs in
  internal/tui/resource.go
  internal/tui/table.go
  internal/tui/table_test.go
```

---

## Task 03: four resources — skills, agents, usage, jobs

- [ ] **Task 03 — Four resources** <!-- task_03_four_resources.py -->

**Step 3.1: red.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 03 --red
```

Expected output, exactly:

```text
RED as expected: the resource tests need the four resources
```

**Step 3.2: green.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 03
```

Expected output, exactly:

```text
exit 0
T0 PASS TestMain pins the clock
T0 PASS the fake backend implements Views
T0 PASS internal/tui/res_registry.go exists
T0 PASS internal/tui/res_skills.go exists
T0 PASS internal/tui/res_agents.go exists
T0 PASS internal/tui/res_usage.go exists
T0 PASS internal/tui/res_jobs.go exists
T0 PASS gofmt clean
T1 PASS the five resource tests pass
T1 PASS tui tests, race
```

**Step 3.3: commit.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/commit.py 03 && git status --porcelain --untracked-files=no
```

Expected output, exactly:

```text
committed task 03: tui: skills, agents, usage and jobs resources
  internal/tui/app_test.go
  internal/tui/block_test.go
  internal/tui/res_agents.go
  internal/tui/res_jobs.go
  internal/tui/res_registry.go
  internal/tui/res_skills.go
  internal/tui/res_test.go
  internal/tui/res_usage.go
```

---

## Task 04: the header, the title rule and the three-band layout

- [ ] **Task 04 — Header, rule, three-band layout** <!-- task_04_header_and_layout.py -->

This task has no separate red step. The golden screen tests are its red: after the layout changes and before the goldens are regenerated, the script checks that exactly the four golden tests fail and nothing else does. That is the `RED as expected` line below. The script then regenerates the goldens and checks what each screen shows.

**Step 4.1: apply and verify.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 04
```

Expected output, exactly:

```text
RED as expected: TestGoldenAllSessions, TestGoldenApprovalOverlay, TestGoldenExpandedTool, TestGoldenScreens
exit 0
T0 PASS View joins header, rule, body and status
T0 PASS help lists the views
T0 PASS gofmt clean
T0 PASS goldens regenerated
T0 PASS screen-100: three header rows, the rule, keys in the status bar
T0 PASS screen-160: three header rows, the rule, keys in the status bar
T0 PASS screen-60: one header row, then the rule
T0 PASS screen-60 fits 60x24
T0 PASS screen-100 fits 100x24
T0 PASS screen-160 fits 160x24
T0 PASS sessions-all-100 fits 100x24
T0 PASS tool-expanded-100 fits 100x30
T0 PASS approval-insert-100 fits 100x30
T0 PASS approval-normal-100 fits 100x30
T0 PASS too-small is still exactly 'terminal too small'
T1 PASS tui tests, race
```

**Step 4.2: commit.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/commit.py 04 && git status --porcelain --untracked-files=no
```

Expected output, exactly:

```text
committed task 04: tui: a k9s header with key hints, a title rule, and a status bar of keys
  internal/tui/header.go
  internal/tui/testdata/approval-insert-100.golden
  internal/tui/testdata/approval-normal-100.golden
  internal/tui/testdata/screen-100.golden
  internal/tui/testdata/screen-160.golden
  internal/tui/testdata/screen-60.golden
  internal/tui/testdata/sessions-all-100.golden
  internal/tui/testdata/tool-expanded-100.golden
  internal/tui/view.go
```

---

## Task 05: views in the model, end to end

- [ ] **Task 05 — Views in the model, end to end** <!-- task_05_views_in_the_model_end_to_end.py -->

This wires the views into the model: open, navigate, refresh, act. It proves them with a test that drives the real TUI against a real daemon and cancels a real job with `x`, `y`. The second red line takes about 5 seconds: the TUI waits for a view that never opens.

**Step 5.1: red.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 05 --red
```

Expected output, exactly:

```text
RED as expected: the model tests need the view state
RED as expected: the TUI never shows a view against a real daemon
```

**Step 5.2: green.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 05
```

Expected output, exactly:

```text
exit 0
T0 PASS New picks Views up from the backend
T0 PASS keys reach resourceByHotkey
T0 PASS /commands go through slashLine
T0 PASS gofmt clean
T0 PASS view-skills-100 fits 100x24
T0 PASS view-skills-detail-100 fits 100x24
T0 PASS view-usage-100 fits 100x24
T0 PASS view-jobs-confirm-100 fits 100x24
T0 PASS view-stale-100 fits 100x24
T0 PASS view-skills-100: rule, columns, the selected row, the error row, no sidebar, view keys
T0 PASS view-skills-detail-100: the body, and esc closes
T0 PASS view-usage-100: this session first at 96%, then two days, haiku with no cache
T0 PASS view-jobs-confirm-100: the confirmation in the status bar
T0 PASS view-stale-100: marked stale, rows kept
T1 PASS tui tests, race
T2 PASS keys open views, and x/y cancels a real job, through the adapter and a real daemon
T1 PASS cmd/spore tests, race
```

**Step 5.3: commit.**

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/commit.py 05 && git status --porcelain --untracked-files=no
```

Expected output, exactly:

```text
committed task 05: tui: open views by hotkey or :command, refresh them, act on rows; an end-to-end views test
  cmd/spore/tui_e2e_test.go
  internal/tui/app.go
  internal/tui/app_test.go
  internal/tui/header.go
  internal/tui/testdata/view-jobs-confirm-100.golden
  internal/tui/testdata/view-skills-100.golden
  internal/tui/testdata/view-skills-detail-100.golden
  internal/tui/testdata/view-stale-100.golden
  internal/tui/testdata/view-usage-100.golden
  internal/tui/view.go
  internal/tui/view_test.go
```

---

## Task 06: every CI gate, at HEAD, in a detached worktree

- [ ] **Task 06 — Whole-branch gates at HEAD** <!-- task_06_whole_branch_gates.py -->

This task makes no changes and no commit. It runs `make fmtcheck vet lint test tidycheck vulncheck` and `go test -race ./...` on a fresh checkout of HEAD. It takes several minutes.

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/step.py 06
```

Expected output, exactly:

```text
exit 0
T0 PASS tasks 01-05 are committed on this branch
T1 PASS make fmtcheck vet lint
T1 PASS make test
T1 PASS go test -race ./... (what CI runs)
T1 PASS make tidycheck vulncheck
```

---

## Final gate

This re-runs every task's gates at HEAD, including Task 06, so it takes several minutes.

```bash
python3 docs/plans/2026-09-22-tui-views-2a1/final_gate.py
```

Expected output, exactly:

```text
PASS  R1  the daemon serves GET /api/usage (route at its call site)
PASS  R2  the skills endpoint returns bodies only for ?body=1
PASS  R3  cmd/spore's adapter implements tui.Views
PASS  R4  tui.New finds the views through the backend it is given
PASS  R5  tui.Run's signature is unchanged
PASS  R6  four views: skills S, agents A, usage U, jobs J
PASS  R7  hotkeys in NORMAL open views
PASS  R8  the stop-agent action exists
PASS  R9  the cancel-job action exists
PASS  R10 an open view refreshes every 2 seconds
PASS  R11 the k9s header and title rule are on every screen
PASS  R12 /usage /skills /agents typed into the input still print text
PASS  R13 usage session totals are rows labelled 'this session'
PASS  C1  no new module dependencies: go.mod and go.sum unchanged
PASS  C2  internal/tui never imports cmd/spore
PASS  C3  wire constants in internal/daemon/event.go unchanged
PASS  C4  on branch tui-views
PASS  C5  no tracked file is modified or staged
PASS  C6  exactly six commits after 174b2c1: the plan, then tasks 01-05
PASS  C7  the first commit after 174b2c1 changes only the plan directory
PASS  C8.01 commit 01 has its exact message and trailers
PASS  C9.01 commit 01 changes exactly its 10 files
PASS  C8.02 commit 02 has its exact message and trailers
PASS  C9.02 commit 02 changes exactly its 3 files
PASS  C8.03 commit 03 has its exact message and trailers
PASS  C9.03 commit 03 changes exactly its 8 files
PASS  C8.04 commit 04 has its exact message and trailers
PASS  C9.04 commit 04 changes exactly its 9 files
PASS  C8.05 commit 05 has its exact message and trailers
PASS  C9.05 commit 05 changes exactly its 11 files
PASS  C10 the user's .agents/ and fix_task1.py were never committed
PASS  C11 the branch changes nothing outside the plan's file map
PASS  G01 task 01's gates pass (--verify-only)
PASS  G02 task 02's gates pass (--verify-only)
PASS  G03 task 03's gates pass (--verify-only)
PASS  G04 task 04's gates pass (--verify-only)
PASS  G05 task 05's gates pass (--verify-only)
PASS  G06 make fmtcheck vet lint test tidycheck vulncheck and go test -race, at HEAD in a detached worktree
ALL CHECKS PASS
```

## Report

Your report must contain, pasted verbatim:

1. The output of every `step.py NN --red` command (Steps 1.1, 2.1, 3.1, 5.1) and the first line of Step 4.1.
2. The output of every `step.py NN` command.
3. The output of the final gate.
4. The output of `git log --format='%h %s' 174b2c1..HEAD`.
5. Every deviation from this plan, or the word `none`. A deviation is any command you ran that this plan does not list, any retry, and any output that differed from its expected block.

Do not claim success without these outputs.

## Manual check (for the human, not the worker)

The worker must not restart the daemon. The user does this after reviewing the report:

1. `make build`, restart the daemon from the shell that holds the Discord token, then `./spore chat`.
2. The header shows the session and the key hints. Below 100 columns it collapses to one row.
3. Under tmux, `Esc` followed quickly by `S` opens skills. `enter` shows a skill's body. `esc` closes the body, then the view.
4. `U` shows this session's usage and the last days. `J` lists jobs. `x`, `y` on a job disables it.
5. In a session with sub-agents, `A` lists them. `enter` opens a child's transcript.
6. A view keeps refreshing while a Discord turn runs: the usage numbers change.

## Changes from the source plan

- **Order.** The source plan was horizontal: daemon, then the TUI pieces, with the adapter and its end-to-end test last. Here the adapter (source Task 6 Step 2) and the `Views` interface (source Task 2 Step 1) move into Task 01. The first task therefore proves the store → daemon → adapter path against a real daemon, through a new test, `TestTheAdapterReadsTheViewsFromARealDaemon`. The TUI end-to-end test (source Task 6 Step 1) moves into Task 05, the first task where a key can reach the daemon. Source Task 7 becomes Task 06, the final gate, and the manual check above.
- **Defect fixed: `ids` redeclared.** The source plan's `table_test.go` declared `func ids(rs []Row) string`, but `sidebar_test.go` already declares `ids` in the same package, so the tui tests would not compile. The helper is renamed `rowIDs` here.
- **Defect fixed: gofmt alignment.** The source plan put the new `fakeBackend` fields directly after the old ones, unaligned. gofmt reports that file, so `make fmtcheck` would fail. This was checked against 174b2c1. Here the fields sit in their own blank-line-separated group, which gofmt accepts as written.
- **Commit messages.** The five commits keep the source plan's messages where the task is unchanged (02, 03, 04). Tasks 01 and 05 absorbed source Task 6, so their subjects say so.

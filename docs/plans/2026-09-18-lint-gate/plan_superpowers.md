# Lint and dependency gates for spore

## Goal

CI gates the code with a linter and a vulnerability scanner, both pinned, both
run through the same `make` targets a developer runs locally, and both green.
The two gates are added one linter at a time, and every task leaves `master`'s
CI green if it were merged there and then.

## Architecture

There is no product code design here. The design is the seam: `.golangci.yml`
and `go.mod` describe the rules, `Makefile` targets are the single way to run
them, and CI calls those targets. Nothing in CI may run a linter command of its
own — that is how a CI gate and a developer's checkout drift apart.

## Tech Stack

Go 1.26.6, golangci-lint v2.13.2, govulncheck v1.8.0, GNU make, GitHub Actions.
Everything in this repo needs the `sqlite_fts5` build tag; a tool run without it
is reading a different program than the one that ships.

## Spec

1. `.golangci.yml` is v2 schema, `default: none`, eight linters enabled:
   errcheck, gosec, govet, ineffassign, staticcheck, unconvert, unparam, unused.
2. `make lint` exits 0. `make vulncheck` exits 0. `make tidycheck` exits 0.
3. CI runs `make lint-install && make lint`, `make vulncheck-install && make
   vulncheck`, and `make tidycheck`. Both tool versions are pinned in the
   Makefile. No `@latest`, no `version: latest`, no `golangci-lint-action`.
4. `make test` and `make fmtcheck` stay green throughout.
5. The two test files the coverage work added stay in the tree.
6. Findings that indicate a real defect are fixed and written down, not
   silenced. Every `//nolint` carries a reason.
7. One commit per task, on a branch, with the exact messages given below.
8. `REPORT.md` and `decisions.md` are written in this plan's directory.

## Global Constraints

- **Type code exactly as given.** Do not restructure, rename, reorder or
  "improve" a block this plan hands you. Where the plan gives a rule rather
  than a block (tasks 03–06), apply the rule and nothing else.
- **Change only what a step names.** Do not reformat untouched files, do not
  upgrade modules a step does not name, do not touch `docs/`, `.agents/`,
  `fix_task1.py`, `spore_image_gen.md`, `docs/product-research.md` or
  `docs/technical-prd.md`. They are untracked on purpose and are not yours.
- **Never weaken a gate to pass it.** If a gate is red, fix the work. If you
  believe the gate itself is wrong, stop and report — do not edit the gate, the
  task script, or this plan.
- **One commit per task**, with the message given, containing only the files
  that task names. Never amend, rebase, reset, or split a commit.
- **Run every command from the repository root**, `/home/code/development/spore`.
- **Do not push, do not open a pull request.** Integration is the owner's call.
- If a check's output differs from the expected output, stop, fix your last
  edit, and try once more. If it still differs, record the step, the command
  and the full output in `REPORT.md` and continue to the next task only if that
  task does not depend on the failed one. Tasks here are ordered; in practice
  that means stop.

## How a task runs

Every task has a script that both gates the work and, for the scripted tasks,
does it. From the repository root:

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/<script>              # do it, then gate it
python3 docs/plans/2026-09-18-lint-gate/tasks/<script> --verify-only # gate only
```

Exit codes: `0` pass, `1` a gate failed, `3` the file drifted from what the
script expected (stop and report), `4` the task needs your edits first — the
message tells you which.

To see the whole board without changing anything:

```bash
python3 docs/plans/2026-09-18-lint-gate/run.py --status
```

---

## Step 0 — starting state

```bash
cd /home/code/development/spore
git rev-parse --short HEAD
git branch --show-current
git status --porcelain --untracked-files=no
git config user.email
```

Expected output, exactly:

```text
2edd07e
master
 M .github/workflows/ci.yml
 M internal/tool/subagent/tools_test.go
harshsingh24@gmail.com
```

If this differs, stop and report. In particular: the two modified files are the
unfinished coverage work, and this plan expects them.

Take the branch and preserve the two files the earlier work left behind:

```bash
git checkout -b lint-gate
mkdir -p docs/plans/2026-09-18-lint-gate/preexisting
cp .golangci.yml docs/plans/2026-09-18-lint-gate/preexisting/golangci-v1.yml
git diff .github/workflows/ci.yml > docs/plans/2026-09-18-lint-gate/preexisting/ci-lint-step.patch
git checkout -- .github/workflows/ci.yml
rm .golangci.yml
git status --porcelain --untracked-files=no
```

Expected output, exactly:

```text
 M internal/tool/subagent/tools_test.go
```

Why: that `.golangci.yml` is v1 schema and golangci-lint v2 refuses to load it
(`can't load config: unsupported version of the configuration: ""`), and that CI
step installed an unpinned `latest`. Task 01 replaces both. The originals are
kept under `preexisting/` so nothing is lost.

Install the two pinned tools:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
golangci-lint version 2>&1 | cut -d' ' -f4
```

Expected output, exactly:

```text
2.13.2
```

Confirm the suite is green before you change anything:

```bash
make test > /tmp/spore-baseline-test.log 2>&1; echo "exit=$?"
```

Expected output, exactly:

```text
exit=0
```

---

- [ ] **Task 00 — Baseline the tree** <!-- task_00_baseline.py -->

Run it. The script adds the coverage artefacts to `.gitignore` and gates that
Step 0 happened.

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_00_baseline.py; echo "exit=$?"
```

Expected: `exit=0` on the last line.

Commit, including the two test files the coverage work left in the tree:

```bash
git add .gitignore internal/tool/subagent/tools_test.go cmd/spore/client_test.go docs/plans/2026-09-18-lint-gate
git commit -q -m "ci: baseline the tree for the lint gate"
git log --format=%s -1
```

Expected output, exactly:

```text
ci: baseline the tree for the lint gate
```

---

- [ ] **Task 01 — Wire the lint seam end to end** <!-- task_01_wire_lint_seam.py -->

This task is scripted: it writes `.golangci.yml`, adds the `lint` and
`lint-install` targets to the `Makefile`, and adds the CI step. One linter is
enabled — `govet`, which is already clean — because the point of this task is
the path, not the findings.

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_01_wire_lint_seam.py; echo "exit=$?"
```

Expected: `exit=0` on the last line. If it exits 3, the files are not at the
state Step 0 leaves them in: stop and report.

Confirm the gate and CI agree on the command:

```bash
make lint
grep -c "make lint-install && make lint" .github/workflows/ci.yml
```

Expected output, exactly:

```text
0 issues.
1
```

```bash
git add .golangci.yml Makefile .github/workflows/ci.yml
git commit -q -m "ci: wire the lint gate end to end"
```

---

- [x] **Task 02 — Clear ineffassign and unconvert** <!-- task_02_ineffassign_unconvert.py -->

Two findings. See them first:

```bash
golangci-lint run --enable-only ineffassign,unconvert ./...
```

Expected, two lines naming `internal/trace/trace_test.go:40` (ineffectual
assignment to `ctx`) and `cmd/spore/approve.go:27` (unnecessary conversion).

Fix both at the site. For the ineffectual assignment: the value assigned to
`ctx` is never read before the next assignment — use it or drop the assignment,
whichever the surrounding test means. For the conversion: delete the conversion,
keep the expression.

Then enable both linters. In `.golangci.yml`, find:

```yaml
  enable:
    - govet
```

Replace with:

```yaml
  enable:
    - govet
    - ineffassign
    - unconvert
```

Check:

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_02_ineffassign_unconvert.py --verify-only; echo "exit=$?"
```

Expected: `exit=0` on the last line.

```bash
git add -u && git commit -q -m "fix: clear ineffassign and unconvert"
```

---

- [x] **Task 03 — Clear unused and staticcheck** <!-- task_03_unused_staticcheck.py -->

```bash
golangci-lint run --enable-only staticcheck,unused ./...
```

Ten findings. The rules:

- **`QF1001` / `QF1006`** (De Morgan, lift into loop condition) — apply the
  rewrite the tool suggests. These are all in tests and are cosmetic.
- **`SA1019` deprecated API** — two sites: `attribute.Value.Emit` in
  `internal/trace/trace_test.go` and `batch.ObjectsBatcher.WithObject` in
  `internal/recall/weaviate/weaviate.go`. Move to the named replacement
  (`Value.String`, `WithObjects`). If the replacement changes behaviour, keep
  the old call, add `//nolint:staticcheck // <reason>`, and write the reason in
  `decisions.md`.
- **`unused`** — three: `slashHint` in `cmd/spore/tui.go`, `newSession` in
  `internal/daemon/e2e_test.go`, `(*Supervisor).track` in
  `internal/subagent/supervisor.go`. The first two are dead: delete them. The
  third is left over from the sub-agent work in PR #27 — **read it before you
  delete it.** If a supervisor is meant to be tracking its children and nothing
  calls `track`, that is a bug report, not a deletion. Write what you found in
  `decisions.md` either way.
- **`SA4006` in `internal/policy/guard.go:254`** — "this value of `claimed` is
  never used". This is a policy guard. Do not make it green by deleting the
  assignment until you can say what the value was for. Read the function, write
  your finding in `decisions.md`, then fix it the way the finding says.

Create `docs/plans/2026-09-18-lint-gate/decisions.md` with:

```markdown
# Decisions taken while clearing the gates

Each entry: the finding, what the code actually does, what I did, and why.

## internal/policy/guard.go — SA4006, value of `claimed` never used

<your finding>

## internal/subagent/supervisor.go — unused (*Supervisor).track

<your finding>
```

Then enable both linters. In `.golangci.yml`, find:

```yaml
  enable:
    - govet
    - ineffassign
    - unconvert
```

Replace with:

```yaml
  enable:
    - govet
    - ineffassign
    - staticcheck
    - unconvert
    - unused
```

Check:

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_03_unused_staticcheck.py --verify-only; echo "exit=$?"
```

Expected: `exit=0` on the last line.

```bash
git add -A docs/plans/2026-09-18-lint-gate/decisions.md && git add -u
git commit -q -m "fix: clear unused and staticcheck"
```

---

- [x] **Task 04 — Clear errcheck** <!-- task_04_errcheck.py -->

First add the exclusion block, then fix what remains. In `.golangci.yml`, find:

```yaml
linters:
  default: none
```

Replace with:

```yaml
linters:
  exclusions:
    rules:
      # A test that ignores a teardown error is not a defect, and making
      # every test check one buries the ones that matter.
      - path: _test\.go
        linters:
          - errcheck
          - gosec
          - unparam
  default: none
```

Add the linter. In `.golangci.yml`, find:

```yaml
  enable:
    - govet
```

Replace with:

```yaml
  enable:
    - errcheck
    - govet
```

Now see what is left:

```bash
golangci-lint run --enable-only errcheck ./...
```

Four shapes, and nothing else:

1. A deferred close whose error nobody can act on:
   `defer resp.Body.Close()` becomes `defer func() { _ = resp.Body.Close() }()`
2. A print to stdout/stderr:
   `fmt.Fprintf(w, ...)` becomes `_, _ = fmt.Fprintf(w, ...)`
3. A best-effort cleanup on an error path:
   `os.Remove(tmp.Name())` becomes `_ = os.Remove(tmp.Name())`
4. **A close on the success path of an atomic write** —
   `internal/config/write.go` and `internal/memory/memory.go` both write a temp
   file, close it, and rename it over the target. A failed `Close` there means
   the bytes may not have reached the disk, and renaming over the real file
   anyway loses data. These two return the error instead of discarding it.
   Write both in `decisions.md`.

Use shape 1–3 only where the error genuinely cannot be acted on. If you find
another case like shape 4, treat it as shape 4 and write it down.

Check:

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_04_errcheck.py --verify-only; echo "exit=$?"
```

Expected: `exit=0` on the last line.

```bash
git add -u && git commit -q -m "fix: clear errcheck"
```

---

- [x] **Task 05 — Clear unparam** <!-- task_05_unparam.py -->

```bash
golangci-lint run --enable-only unparam ./...
```

Two findings, both in production code (test helpers are already excluded):

- `(*Agent).runTools` in `internal/agent/agent.go` — its `error` result is
  always nil. Drop the result and fix the callers, or return a real error if
  one of its branches should have been failing.
- `(*chatUI).nextApproval` in `cmd/spore/tui.go` — its `tea.Cmd` result is
  always nil. Same choice.

If either signature exists to satisfy an interface or a caller that is about to
need it, do not change it: add `//nolint:unparam // <reason>` and write the
reason in `decisions.md`.

Add the linter. In `.golangci.yml`, find:

```yaml
    - unconvert
    - unused
```

Replace with:

```yaml
    - unconvert
    - unparam
    - unused
```

Check:

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_05_unparam.py --verify-only; echo "exit=$?"
```

Expected: `exit=0` on the last line.

```bash
git add -u && git commit -q -m "fix: clear unparam"
```

---

- [x] **Task 06 — Put gosec on the gate** <!-- task_06_gosec.py -->

spore runs subprocesses and reads paths its user names. Most of what gosec says
here is a description of the product. Two groups are not.

Add the settings and the linter. In `.golangci.yml`, find:

```yaml
  default: none
  enable:
    - errcheck
    - govet
```

Replace with:

```yaml
  default: none
  settings:
    gosec:
      severity: medium
      confidence: medium
      # G204 is "subprocess launched with variable". Running the command the
      # user asked for is what the shell and MCP tools are; the confinement
      # that makes it safe lives in internal/policy, and is tested there.
      excludes:
        - G204
  enable:
    - errcheck
    - gosec
    - govet
```

Then:

```bash
golangci-lint run --enable-only gosec ./...
```

Seventeen findings. Fix these, do not annotate them:

- **`G118` in `internal/subagent/supervisor.go:340`** — a goroutine uses
  `context.Background()` while a request-scoped context is in hand. That is a
  child that outlives its parent's cancellation. Pass the real context, and if
  it cannot be passed, say in `decisions.md` why not.
- **`G301` / `G306` permissions** in `internal/tool/fs/fs.go`,
  `internal/skill/skill.go`, `internal/recall/weaviate/provision.go` and
  `internal/trace/phoenix/provision.go` — tighten `0755` to `0750` and `0644`
  to `0600` unless a test proves the wider mode is needed. These are a personal
  agent's own files; nothing else should be reading them.

Annotate the rest, one line each, in this exact shape, on the reported line:

```go
//nolint:gosec // <why this is the product working as designed>
```

A bare `//nolint` without a reason fails this task's gate.

Check:

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_06_gosec.py --verify-only; echo "exit=$?"
```

Expected: `exit=0` on the last line.

```bash
git add -u && git commit -q -m "fix: put gosec on the lint gate"
```

---

- [x] **Task 07 — Wire the dependency gate and clear its findings** <!-- task_07_vulnerability_gate.py -->

`govulncheck` reports 11 vulnerabilities the code actually reaches. Seven are in
the standard library and go away with the toolchain line; three are modules.
All four changes below were probed before this plan was written: they build, the
suite passes, and the scan comes back clean.

```bash
go get go@1.26.6
go get github.com/gorilla/websocket@v1.5.3
go get github.com/yuin/goldmark@v1.7.17
go get github.com/weaviate/weaviate@v1.38.0-rc.0
go mod tidy
```

`v1.38.0-rc.0` is a release candidate, and is taken because it is the only
published fix, because Weaviate recall is an optional backend, and because the
suite passes on it. Record that in `decisions.md` and in the report.

In `Makefile`, find:

```makefile
lint:
	golangci-lint run ./...
```

Replace with:

```makefile
lint:
	golangci-lint run ./...

# The dependency gate, pinned and run through a target for the same reason
# as the linter: CI and a developer must run the same command.
GOVULNCHECK_VERSION := v1.8.0

vulncheck-install:
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

vulncheck:
	govulncheck -tags sqlite_fts5 ./...

tidycheck:
	go mod tidy && git diff --exit-code go.mod go.sum
```

In `Makefile`, find:

```makefile
.PHONY: build install test test-weaviate test-phoenix vet fmt fmtcheck lint lint-install
```

Replace with:

```makefile
.PHONY: build install test test-weaviate test-phoenix vet fmt fmtcheck lint lint-install vulncheck vulncheck-install tidycheck
```

In `.github/workflows/ci.yml`, find:

```yaml
      - name: lint
        run: make lint-install && make lint
        shell: bash
```

Replace with:

```yaml
      - name: lint
        run: make lint-install && make lint
        shell: bash
      - name: vulncheck
        run: make vulncheck-install && make vulncheck
        shell: bash
      - name: tidy
        run: make tidycheck
        shell: bash
```

Check:

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_07_vulnerability_gate.py --verify-only; echo "exit=$?"
```

Expected: `exit=0` on the last line.

```bash
git add -u && git add docs/plans/2026-09-18-lint-gate/decisions.md
git commit -q -m "ci: wire the dependency gate and clear its findings"
```

---

- [ ] **Task 08 — Final gate and report** <!-- task_08_final_gate.py -->

Write `docs/plans/2026-09-18-lint-gate/REPORT.md` with exactly these headings,
then run the gate. The headings are checked; the content is what the owner
reads instead of re-deriving your work.

```markdown
# Lint and dependency gates — worker report

Branch: lint-gate. Base: 2edd07e.

## Result

PASS or FAIL, in one line, then the final gate output pasted verbatim.

## What changed per task

One short paragraph per task, 00 to 08: what you changed, and the commit hash.
Name any file you touched that the plan did not name, and say why.

## Findings I judged rather than silenced

Copy the entries from decisions.md, plus anything else you decided. At minimum:
guard.go SA4006, supervisor.track, the two atomic-write closes, the G118
context, and the weaviate release candidate. If you silenced something the plan
told you to fix, say so here in its own line.

## Deviations

Every place you did something other than what the plan said, or "none".
A gate you could not turn green belongs here with its full output.

## Commands and their output

Paste verbatim, in this order:
- `golangci-lint run ./...` before task 01 (the red state)
- `make lint`
- `make vulncheck`
- `make test` (last line is enough if it passed; all failures if it did not)
- `git log --oneline master..HEAD`
- `python3 docs/plans/2026-09-18-lint-gate/run.py --status`
```

Then:

```bash
python3 docs/plans/2026-09-18-lint-gate/tasks/task_08_final_gate.py --verify-only; echo "exit=$?"
```

Expected: `exit=0` on the last line.

```bash
git add docs/plans/2026-09-18-lint-gate/REPORT.md
git commit -q -m "docs: lint gate report"
python3 docs/plans/2026-09-18-lint-gate/run.py --status
```

Expected: `9/9 gates green` on the last line — eight tasks plus the final gate.

Do not push and do not open a pull request. Stop here and report.

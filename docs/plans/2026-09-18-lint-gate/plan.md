# Lint and dependency gates — Plan Index

Target repo: `/home/code/development/spore`, branch off `master` at `2edd07e`.

## Seams

| Seam | From | To | Verified by |
|---|---|---|---|
| source -> golangci-lint | Go packages | `.golangci.yml` + pinned golangci-lint v2.13.2 | `make lint` |
| lint target -> CI | `Makefile` `lint` | `.github/workflows/ci.yml` lint step | `make lint-install && make lint` (same string in both) |
| source -> govulncheck | `go.mod` + Go packages | pinned govulncheck v1.8.0 | `make vulncheck` |
| vulncheck target -> CI | `Makefile` `vulncheck`, `tidycheck` | `.github/workflows/ci.yml` steps | `make vulncheck`, `make tidycheck` |
| worker -> reviewer | task scripts | `REPORT.md`, `decisions.md` | `tasks/task_08_final_gate.py` |

The seam that matters is the second one: CI must run the *same command* a
developer runs, or the gate drifts from the thing it gates. Every task's T2
runs that command.

## Tasks

| # | Title | Kind | Tier | Touches | Crosses | Files | Script |
|---|---|---|---|---|---|---|---|
| 00 | Baseline the tree | scripted | T1 | repo config | — | `.gitignore`, `.golangci.yml`, `.github/workflows/ci.yml` | `tasks/task_00_baseline.py` |
| 01 | Wire the lint seam (govet only) | scripted | T2 | Makefile, CI, config | source -> golangci-lint; lint target -> CI | `.golangci.yml`, `Makefile`, `.github/workflows/ci.yml` | `tasks/task_01_wire_lint_seam.py` |
| 02 | Clear ineffassign + unconvert | manual | T2 | internal/trace, cmd/spore | source -> golangci-lint | `internal/trace/trace_test.go`, `cmd/spore/approve.go`, `.golangci.yml` | `tasks/task_02_ineffassign_unconvert.py` |
| 03 | Clear unused + staticcheck | manual | T2 | internal/policy, internal/subagent, internal/recall, internal/store, cmd/spore | source -> golangci-lint | 10 sites, `.golangci.yml`, `decisions.md` | `tasks/task_03_unused_staticcheck.py` |
| 04 | Clear errcheck | manual | T2 | cmd/spore, internal/config, internal/memory, internal/provider, internal/recall, internal/trace | source -> golangci-lint | ~43 sites, `.golangci.yml` | `tasks/task_04_errcheck.py` |
| 05 | Clear unparam | manual | T2 | internal/agent, cmd/spore | source -> golangci-lint | `internal/agent/agent.go`, `cmd/spore/tui.go`, `.golangci.yml` | `tasks/task_05_unparam.py` |
| 06 | Put gosec on the gate | manual | T2 | internal/tool/fs, internal/skill, internal/daemon, internal/subagent, cmd/spore | source -> golangci-lint | 17 sites, `.golangci.yml` | `tasks/task_06_gosec.py` |
| 07 | Wire the dependency gate | manual | T2 | go.mod, Makefile, CI | source -> govulncheck; vulncheck target -> CI | `go.mod`, `go.sum`, `Makefile`, `.github/workflows/ci.yml` | `tasks/task_07_vulnerability_gate.py` |
| 08 | Final gate and report | manual | T2 | whole repo | all | `REPORT.md` | `tasks/task_08_final_gate.py` |

## Measured starting state (2026-09-18, this machine)

Taken with golangci-lint v2.13.2 and govulncheck v1.8.0 against `2edd07e`
plus the uncommitted coverage work.

| Linter | Findings | Cleared by |
|---|---|---|
| govet | 0 | — (this is why task 01 uses it) |
| ineffassign | 1 | task 02 |
| unconvert | 1 | task 02 |
| unused | 3 | task 03 |
| staticcheck | 7 | task 03 |
| errcheck | 50 (43 after the `_test.go` exclusion) | task 04 |
| unparam | 7 (2 after the exclusion) | task 05 |
| gosec | 25 (17 after the exclusion and the G204 setting) | task 06 |
| govulncheck | 11 reachable (7 stdlib, 3 modules, 1 double-counted) | task 07 |

## Known risks, stated before handover

1. **weaviate's fix is a release candidate** (`v1.38.0-rc.0`). Task 07 takes it
   because recall over Weaviate is an optional backend and the suite passes on
   it. If that is unacceptable, the gate cannot be green and the plan is wrong
   at task 07 — say so rather than suppressing the finding, since govulncheck
   has no per-finding suppression.
2. **The toolchain bump to `go 1.26.6`** makes every developer on 1.26.4 fetch a
   toolchain on first build. That was probed here and works with the default
   `GOTOOLCHAIN=auto`.
3. **gosec on a tool-running agent is mostly noise.** Task 06 answers those
   findings in writing rather than in code; its gate refuses a bare `//nolint`.
4. `make lint` and `make vulncheck` take longer than `make test` on a cold
   module cache. CI installs both tools per job, per OS.

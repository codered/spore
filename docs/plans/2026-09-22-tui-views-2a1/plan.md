# TUI Views 2a-1 — Plan Index

Source: `docs/superpowers/plans/2026-09-22-tui-views-2a1.md` (commit 174b2c1), re-cut as vertical slices.
Worker instructions: `plan_superpowers.md`. Final gate: `final_gate.py`.

## Seams

| Seam | From | To | Verified by |
|---|---|---|---|
| store -> daemon | `store.DailyUsage` | `handleUsage` | task 01 T1 `TestUsageReportsTheSessionAndTheDays` |
| daemon HTTP -> adapter | `GET /api/usage`, `GET /api/sessions/{id}/skills?body=1`, `/api/jobs`, `/api/sessions/{id}/agents` | `tuiBackend` Views methods | task 01 T2 `TestTheAdapterReadsTheViewsFromARealDaemon` (real daemon, real HTTP) |
| missing route -> older-daemon message | plain-text 404 | `viewErr` -> `tui.ErrOlderDaemon` | task 01 T2 `TestAViewAgainstADaemonWithoutTheRouteSaysSo` |
| adapter -> tui | `tui.Views` interface | `Resource.Fetch`, `Action.Run` | compile-time `var _ tui.Views = tuiBackend{}` (task 01); task 05 T2 |
| key -> screen, end to end | `tui.Model` keys | adapter -> daemon -> store -> rendered table | task 05 T2 `TestTheTUIOpensViewsAgainstARealDaemonAndCancelsAJob` |

## Tasks

| # | Title | Kind | Tier | Touches | Crosses | Files | Script |
|---|---|---|---|---|---|---|---|
| 01 | The view data path, store to adapter | scripted | T2 | store, daemon, tui, cmd/spore | store -> daemon; daemon HTTP -> adapter; adapter -> tui | internal/store/usage{,_test}.go, internal/daemon/usage{,_test}.go, internal/daemon/skills{,_test}.go, internal/daemon/server.go, internal/tui/views.go, cmd/spore/tui_backend.go, cmd/spore/tui_e2e_test.go | tasks/task_01_usage_reaches_the_adapter.py |
| 02 | Resource framework and table | scripted | T1 | tui | — (consumes Views, proven in 01) | internal/tui/resource.go, table.go, table_test.go | tasks/task_02_resource_framework_and_table.py |
| 03 | Four resources | scripted | T1 | tui | — (consumes Views, proven in 01) | internal/tui/res_{registry,skills,agents,usage,jobs,test}.go, app_test.go, block_test.go | tasks/task_03_four_resources.py |
| 04 | Header, rule, three-band layout | scripted | T1 | tui | — | internal/tui/header.go, view.go, testdata/{screen-*,sessions-all-100,tool-expanded-100,approval-*}.golden | tasks/task_04_header_and_layout.py |
| 05 | Views in the model, end to end | scripted | T2 | tui, cmd/spore | key -> screen, end to end | internal/tui/app.go, header.go, view.go, app_test.go, view_test.go, testdata/view-*.golden, cmd/spore/tui_e2e_test.go | tasks/task_05_views_in_the_model_end_to_end.py |
| 06 | Whole-branch gates at HEAD | scripted (verify-only) | T1 | whole repository | — | none | tasks/task_06_whole_branch_gates.py |

Tasks 02–04 each touch one component and change no seam. They consume `tui.Views`,
which task 01 fixed and proved against a real daemon. The source plan's last two
tasks, the adapter and its end-to-end test, moved into tasks 01 and 05. Each seam
is proven by the first task that crosses it.

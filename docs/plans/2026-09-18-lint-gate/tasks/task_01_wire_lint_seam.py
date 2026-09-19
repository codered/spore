"""Task 01 -- wire the lint seam end to end with the smallest payload that proves it.

One linter (govet, which is already clean) travels the whole path:
source -> .golangci.yml -> pinned golangci-lint -> `make lint` -> CI step.
Nothing is cleaned up here. The point is that the path exists, is green, and
is the same path locally and in CI, before any finding is fixed on it.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import Editor, gate

KIND = "scripted"
TIER = "T2"
TOUCHES = ["go source", "Makefile", ".github/workflows/ci.yml"]
CROSSES = ["source -> golangci-lint (via .golangci.yml)", "Makefile lint target -> CI"]

CONFIG = '''# The lint gate. `make lint` runs exactly this, here and in CI.
#
# Linters are enabled one at a time, as their findings are cleared. An
# enabled linter with open findings is a gate that is expected to be red,
# and a gate that is expected to be red stops being read. `default: none`
# keeps the set explicit, so a golangci-lint release cannot enable a linter
# on our behalf.
version: "2"

run:
  # The store needs SQLite FTS5. Without the tag the linters cannot see the
  # files behind it.
  build-tags:
    - sqlite_fts5
  timeout: 5m

linters:
  default: none
  enable:
    - govet
'''

MAKE_BLOCK = [
    "",
    "# The lint gate. CI runs this exact target, so a green run here is a green",
    "# run there. The version is pinned on purpose: `latest` lets a golangci-lint",
    "# release turn master red with no commit of ours.",
    "GOLANGCI_VERSION := v2.13.2",
    "",
    "lint-install:",
    "\tgo install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)",
    "",
    "lint:",
    "\tgolangci-lint run ./...",
]

CI_STEP = [
    "      - name: lint",
    "        run: make lint-install && make lint",
    "        shell: bash",
]


def apply():
    taskkit.create_file(".golangci.yml", CONFIG)

    if not taskkit.file_contains("Makefile", "\nlint:\n"):
        with Editor("Makefile") as ed:
            ed.insert_after(ed.locate_contains('echo "gofmt needed:"'), MAKE_BLOCK)
            ed.swap(
                ed.locate_contains(".PHONY:"),
                [
                    ".PHONY: build install test test-weaviate test-phoenix vet fmt "
                    "fmtcheck lint lint-install"
                ],
            )

    if not taskkit.file_contains(".github/workflows/ci.yml", "make lint"):
        with Editor(".github/workflows/ci.yml") as ed:
            ed.insert_before(ed.locate("      - name: test"), CI_STEP)


def verify():
    gate.structural(
        "the config is v2 and carries the build tag",
        lambda: taskkit.file_contains(".golangci.yml", 'version: "2"')
        and taskkit.file_contains(".golangci.yml", "- sqlite_fts5"),
    )
    gate.structural(
        "make lint exists and pins the version",
        lambda: taskkit.file_contains("Makefile", "\nlint:\n")
        and taskkit.file_contains("Makefile", "GOLANGCI_VERSION := v2.13.2"),
    )
    gate.structural(
        "CI runs the same target rather than its own command",
        lambda: taskkit.file_contains(".github/workflows/ci.yml", "make lint-install && make lint"),
    )
    gate.component("gofmt is clean", ["make", "fmtcheck"])
    gate.component("the binary still builds", ["make", "build"])
    gate.crossing("the lint gate runs green end to end", cmd=["make", "lint"])


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))

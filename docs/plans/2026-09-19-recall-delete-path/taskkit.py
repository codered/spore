"""taskkit — apply+verify engine for vertical-slice plan tasks.

Stdlib only. Copied verbatim into every emitted plan directory.
Edits go through the hashline CLI so that a stale anchor is rejected
instead of landing on the wrong line.
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import time
from collections import namedtuple

HASHLINE_BIN = os.environ.get("HASHLINE_BIN", "hashline")

Anchor = namedtuple("Anchor", "n hash content")
Op = namedtuple("Op", "kind first last lines")


class Drift(Exception):
    """The file moved under us: anchor gone or hash mismatch. Halt and replan."""


class ManualTask(Exception):
    """This task's edit needs a human or agent; its gate is still runnable."""


class GateFailure(Exception):
    """A gate ran and said no."""


class BackendUnavailable(Exception):
    """hashline could not be used at all. Internal: triggers the fallback."""


REPORT = {"backend": "hashline", "tiers": [], "warnings": []}


def _warn(msg):
    REPORT["warnings"].append(msg)
    print("WARN: " + msg, file=sys.stderr)


def _hl(args):
    """Run hashline with JSON output. Returns the parsed dict.

    Raises Drift on an anchor/hash rejection, FileNotFoundError on a missing
    target, BackendUnavailable when hashline itself cannot be used.
    """
    if shutil.which(HASHLINE_BIN) is None and not os.path.isfile(HASHLINE_BIN):
        raise BackendUnavailable(HASHLINE_BIN + " not found on PATH")
    try:
        proc = subprocess.run([HASHLINE_BIN] + args, capture_output=True, text=True)
    except OSError as exc:
        raise BackendUnavailable(str(exc))

    # Try stdout first, then stderr if stdout is empty (hashline puts errors in stderr with --json)
    json_text = proc.stdout.strip()
    if not json_text and proc.stderr.strip():
        json_text = proc.stderr.strip()

    try:
        data = json.loads(json_text)
    except json.JSONDecodeError:
        raise BackendUnavailable(
            "non-JSON output from hashline: %r / %r" % (proc.stdout[:200], proc.stderr[:200])
        )
    if "error" in data:
        err = data["error"]
        if "I/O error" in err or "No such file" in err:
            raise FileNotFoundError(err)
        if "changed since last read" in err or "hash" in err:
            raise Drift(err)
        raise BackendUnavailable(err)
    return data


def file_contains(path, text):
    """Structural-gate helper: does the file exist and contain this text?"""
    if not os.path.exists(path):
        return False
    with open(path, encoding="utf-8") as fh:
        return text in fh.read()


def create_file(path, content):
    """Create a new file. Idempotent; conflicting existing content is Drift."""
    if os.path.exists(path):
        with open(path, encoding="utf-8") as fh:
            existing = fh.read()
        if existing == content:
            return
        raise Drift("%s already exists with different content" % path)
    parent = os.path.dirname(path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    try:
        _hl(["write", "--json", path, content])
    except BackendUnavailable as exc:
        _warn("fallback backend used: %s" % exc)
        REPORT["backend"] = "fallback"
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(content)


class Editor:
    """Anchored editor over one file. Buffers ops, emits one patch on exit."""

    def __init__(self, path):
        self.path = str(path)
        self._ops = []
        self._lines = []
        self._committed = False
        self._trailing_nl = True

    def __enter__(self):
        self._lines = self._read()
        return self

    def __exit__(self, exc_type, exc, tb):
        if exc_type is None and not self._committed:
            self.commit()
        return False

    def _read(self):
        try:
            data = _hl(["read", "--json", self.path])
            return [Anchor(l["n"], l["hash"], l["content"]) for l in data["lines"]]
        except BackendUnavailable as exc:
            _warn("fallback backend used: %s" % exc)
            REPORT["backend"] = "fallback"
            return self._fallback_read()

    def _fallback_read(self):
        with open(self.path, encoding="utf-8") as fh:
            text = fh.read()
        self._trailing_nl = text.endswith("\n")
        return [Anchor(i + 1, None, c) for i, c in enumerate(text.splitlines())]

    def locate(self, expected, occurrence=1):
        """Return the anchor of the line whose content is exactly `expected`.

        This replaces grep: matching and drift-checking are the same act,
        because the anchor carries the hash hashline will re-check.
        """
        hits = [a for a in self._lines if a.content == expected]
        if len(hits) < occurrence:
            raise Drift("%s: expected line not found: %r" % (self.path, expected))
        return hits[occurrence - 1]

    def locate_contains(self, needle, occurrence=1):
        hits = [a for a in self._lines if needle in a.content]
        if len(hits) < occurrence:
            raise Drift("%s: no line containing %r" % (self.path, needle))
        return hits[occurrence - 1]

    def locate_block(self, head_expected):
        """Return (first, last) anchors of the syntactic block headed by this line."""
        head = self.locate(head_expected)
        if REPORT["backend"] == "hashline":
            try:
                data = _hl(["find-block", "--json", self.path, "%d:%s" % (head.n, head.hash)])
                block_lines = data["block_lines"]
                # Drop only phantom padding: entries beyond snapshot that are empty
                filtered = [b for b in block_lines if b["n"] <= len(self._lines) or b.get("content", "").strip()]
                if not filtered:
                    raise BackendUnavailable("find-block returned only phantom padding")
                # Check for file growth: non-empty content beyond our snapshot
                for b in filtered:
                    if b["n"] > len(self._lines):
                        raise Drift("%s has grown since read: line %d exists in find-block" % (self.path, b["n"]))
                numbers = [b["n"] for b in filtered]
                return self._lines[min(numbers) - 1], self._lines[max(numbers) - 1]
            except BackendUnavailable as exc:
                _warn("fallback backend used: %s" % exc)
                REPORT["backend"] = "fallback"
        return self._fallback_block(head)

    def _fallback_block(self, head):
        """Indentation-scan block detection: head line plus its more-indented body."""
        base = len(head.content) - len(head.content.lstrip())
        last = head
        for anchor in self._lines[head.n:]:
            stripped = anchor.content.strip()
            indent = len(anchor.content) - len(anchor.content.lstrip())
            if stripped and indent <= base:
                break
            if stripped:
                last = anchor
        return head, last

    def swap_range(self, first, last, lines):
        self._ops.append(Op("swap", first, last, list(lines)))

    def insert_before(self, anchor, lines):
        self._ops.append(Op("insert_before", anchor, anchor, list(lines)))

    def insert_after(self, anchor, lines):
        self._ops.append(Op("insert_after", anchor, anchor, list(lines)))

    def delete(self, anchor):
        self._ops.append(Op("delete", anchor, anchor, []))

    def swap(self, anchor, lines):
        self._ops.append(Op("swap", anchor, anchor, list(lines)))

    def _verify_multiline_ops(self):
        """For any op spanning multiple lines, verify all interior lines haven't drifted.

        hashline's SWAP verb only verifies the first and last line hashes, so we must
        do a fresh read and check every line the op spans against the snapshot.
        Single-line ops skip this check since the endpoint hash is sufficient.
        """
        # Check if any op spans more than one line
        has_multiline = any(op.first.n != op.last.n for op in self._ops)
        if not has_multiline:
            return

        # Take a fresh read from hashline
        current = _hl(["read", "--json", self.path])
        current_lines = {l["n"]: l["content"] for l in current["lines"]}

        # Verify all lines spanned by each op
        for op in self._ops:
            for line_num in range(op.first.n, op.last.n + 1):
                if line_num - 1 >= len(self._lines):
                    raise Drift("%s:%d is beyond snapshot" % (self.path, line_num))
                captured_content = self._lines[line_num - 1].content
                current_content = current_lines.get(line_num)
                if current_content != captured_content:
                    raise Drift(
                        "%s:%d changed since read (expected %r, got %r)"
                        % (self.path, line_num, captured_content, current_content)
                    )

    def commit(self):
        self._committed = True
        if not self._ops:
            return
        if REPORT["backend"] == "fallback":
            self._apply_fallback()
            self._ops = []
            return
        # Before patching, verify all lines spanned by multi-line ops
        self._verify_multiline_ops()
        patch = self._render_patch()
        try:
            _hl(["patch", "--json", "--dry-run", self.path, patch])
            _hl(["patch", "--json", self.path, patch])
        except BackendUnavailable as exc:
            # Tool failure degrades. Drift, raised above as Drift, does not.
            _warn("fallback backend used: %s" % exc)
            REPORT["backend"] = "fallback"
            self._lines = self._fallback_read()
            self._apply_fallback()
        self._ops = []

    def _render_patch(self):
        """Ops address ORIGINAL line numbers; hashline does not re-number."""
        out = ["*** Begin Patch"]
        for op in sorted(self._ops, key=lambda o: o.first.n):
            first, last = op.first, op.last
            if op.kind == "swap":
                if first.n == last.n:
                    out.append("SWAP %d:%s:" % (first.n, first.hash))
                else:
                    out.append("SWAP %d:%s..%d:%s:" % (first.n, first.hash, last.n, last.hash))
                out.extend("+" + line for line in op.lines)
            elif op.kind == "delete":
                if first.n == last.n:
                    out.append("DEL %d:%s" % (first.n, first.hash))
                else:
                    out.append("DEL %d..%d" % (first.n, last.n))
            elif op.kind in ("insert_before", "insert_after"):
                keyword = "INS.PRE" if op.kind == "insert_before" else "INS.POST"
                out.append("%s %d:%s:" % (keyword, first.n, first.hash))
                out.extend("+" + line for line in op.lines)
            else:
                raise NotImplementedError(op.kind)
        out.append("*** End Patch")
        return "\n".join(out)

    def _apply_fallback(self):
        """Re-verify every op's captured content, then splice bottom-up."""
        with open(self.path, encoding="utf-8") as fh:
            text = fh.read()
        lines = text.splitlines()
        trailing_nl = text.endswith("\n")
        for op in self._ops:
            # Verify all lines spanned by this op, not just endpoints
            for line_num in range(op.first.n, op.last.n + 1):
                idx = line_num - 1
                # Get the captured content from the snapshot
                captured_anchor = self._lines[idx]
                if idx >= len(lines) or lines[idx] != captured_anchor.content:
                    raise Drift(
                        "%s:%d changed since read (expected %r, got %r)"
                        % (self.path, line_num, captured_anchor.content,
                           lines[idx] if idx < len(lines) else "<EOF>")
                    )
        for op in sorted(self._ops, key=lambda o: o.first.n, reverse=True):
            start, end = op.first.n - 1, op.last.n
            if op.kind == "swap":
                lines[start:end] = list(op.lines)
            elif op.kind == "delete":
                del lines[start:end]
            elif op.kind == "insert_before":
                lines[start:start] = list(op.lines)
            elif op.kind == "insert_after":
                lines[end:end] = list(op.lines)
            else:
                raise NotImplementedError(op.kind)
        out = "\n".join(lines) + ("\n" if trailing_nl else "")
        tmp = self.path + ".taskkit.tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            fh.write(out)
        os.replace(tmp, self.path)


def _record(tier, desc, ok, seconds, detail=""):
    REPORT["tiers"].append(
        {"tier": tier, "desc": desc, "ok": bool(ok), "seconds": round(seconds, 3), "detail": detail}
    )
    if not ok:
        raise GateFailure("[%s] %s%s" % (tier, desc, (": " + detail) if detail else ""))


_PROBE_EXC = (GateFailure, Drift, AssertionError, FileNotFoundError, OSError)


class gate:
    """Tiered, runnable gates. T0 structural, T1 component, T2 crossing."""

    @staticmethod
    def structural(desc, predicate):
        start = time.time()
        ok = bool(predicate())
        _record("T0", desc, ok, time.time() - start)

    @staticmethod
    def _cmd(tier, desc, cmd, cwd=None):
        start = time.time()
        proc = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
        detail = ((proc.stdout or "") + (proc.stderr or "")).strip()[-2000:]
        _record(tier, desc, proc.returncode == 0, time.time() - start, detail)

    @staticmethod
    def component(desc, cmd, cwd=None):
        """T1 — the touched component's own tests."""
        gate._cmd("T1", desc, cmd, cwd)

    @staticmethod
    def crossing(desc, cmd=None, check=None, cwd=None):
        """T2 — a real end-to-end call through the seam this task wired.

        Mandatory whenever a task touches two or more components.
        """
        if cmd is not None:
            gate._cmd("T2", desc, cmd, cwd)
            return
        if check is None:
            raise ValueError("crossing() needs cmd= or check=")
        start = time.time()
        ok = bool(check())
        _record("T2", desc, ok, time.time() - start)

    @staticmethod
    def run(apply, verify):
        """Idempotent driver. Probes verify() first, so a re-run is a no-op."""
        verify_only = "--verify-only" in sys.argv
        code = 0
        try:
            if verify_only:
                verify()
            else:
                if not _probe(verify):
                    apply()
                verify()
        except ManualTask as exc:
            print("MANUAL: %s" % exc, file=sys.stderr)
            code = 4
        except Drift as exc:
            print("DRIFT: %s" % exc, file=sys.stderr)
            code = 3
        except GateFailure as exc:
            print("GATE FAILED: %s" % exc, file=sys.stderr)
            code = 1
        REPORT["exit"] = code
        print(json.dumps(REPORT))
        return code


def _probe(verify):
    """Has this task already been done? Probe failures are answers, not errors."""
    saved = list(REPORT["tiers"])
    try:
        verify()
        ok = True
    except _PROBE_EXC:
        ok = False
    REPORT["tiers"] = saved  # Always restore, since this is just a probe
    return ok

#!/usr/bin/env bash
# record.sh re-records assets/tui-demo.gif from assets/demo/tui.tape.
#
# It runs a demo daemon that shares nothing with yours: its own data dir, its
# own port, and a copy of this repo at HEAD as its workspace. The daemon is
# stopped and the demo dir removed on exit, however the script ends.
#
# Needs: vhs, ANTHROPIC_API_KEY in the environment, and a built ./spore
# (make demo builds it). The turns cost a few cents.
#
# If Chromium cannot start its sandbox ("No usable sandbox"), run
#   VHS_NO_SANDBOX=true make demo
set -euo pipefail

repo=$(cd "$(dirname "$0")/../.." && pwd)
bin="$repo/spore"
addr=127.0.0.1:${SPORE_DEMO_PORT:-7788}

die() { echo "demo: $*" >&2; exit 1; }

command -v vhs >/dev/null || die "vhs is not installed (go install github.com/charmbracelet/vhs@latest)"
[ -x "$bin" ] || die "$bin is missing; run make build"
[ -n "${ANTHROPIC_API_KEY:-}" ] || die "ANTHROPIC_API_KEY is not set"
curl -s -m 1 "http://$addr/healthz" >/dev/null && die "something is already listening on $addr; set SPORE_DEMO_PORT"

# The workspace path is shown in the recording, which is why it is short.
if [ -n "${SPORE_DEMO_DIR:-}" ]; then
	[ -e "$SPORE_DEMO_DIR" ] && die "$SPORE_DEMO_DIR already exists; the demo removes its dir on exit, so give it a new one"
	demo=$SPORE_DEMO_DIR
	mkdir -p "$demo"
else
	demo=$(mktemp -d /tmp/spore-demo.XXXX)
fi
cleanup() {
	[ -x "$demo/bin/spore" ] && "$demo/bin/spore" serve --stop >/dev/null 2>&1
	rm -rf "$demo"
}
trap cleanup EXIT
mkdir -p "$demo/data" "$demo/bin" "$demo/spore"
git -C "$repo" archive --format=tar HEAD | tar -x -C "$demo/spore"

cat >"$demo/config.toml" <<EOF
default_model = "anthropic/claude-sonnet-5-5"
data_dir      = "$demo/data"
show_cost     = true

[providers.anthropic]
kind      = "anthropic"
api_key   = "\${ANTHROPIC_API_KEY}"
price_in  = 3.0
price_out = 15.0

[daemon]
addr = "$addr"

# Only file writes ask, so the tape knows which approval comes up.
[policy]
workspace = "$demo"
default   = "ask"
allow     = ["fs_read", "fs_list", "fs_glob", "fs_grep", "web_*", "go_run", "shell_exec"]
ask       = ["fs_write", "fs_edit", "mcp__*"]
EOF

# The tape types `spore chat`; this wrapper points it at the demo config.
cat >"$demo/bin/spore" <<EOF
#!/bin/sh
exec "$bin" -config "$demo/config.toml" "\$@"
EOF
chmod +x "$demo/bin/spore"
spore() { "$demo/bin/spore" "$@"; }

(cd "$demo/spore" && spore serve >"$demo/serve.log" 2>&1 &)
for _ in $(seq 40); do
	curl -s -m 1 "http://$addr/healthz" >/dev/null && break
	sleep 0.25
done
spore serve --status | grep -q running || { cat "$demo/serve.log" >&2; die "the demo daemon did not start"; }

# Two finished sessions, so the sidebar in the recording is not empty.
# spore once exits 0 even when the turn fails, so the output is checked.
seed() {
	local out
	out=$(cd "$demo/spore" && spore once "$1" 2>&1)
	if grep -q '^turn failed:' <<<"$out"; then
		grep '^turn failed:' <<<"$out" >&2
		die "a seed turn failed; check ANTHROPIC_API_KEY"
	fi
}
echo "demo: seeding history"
seed "Which five packages under internal/ hold the most Go code? A numbered list: package, line count, and one short phrase on what it does."
seed "Which three Go files under internal/ are the largest, and what does each one do? One line each."

echo "demo: recording"
cd "$repo"
PATH="$demo/bin:$PATH" SPORE_DEMO_WS="$demo/spore" vhs assets/demo/tui.tape
echo "demo: wrote assets/tui-demo.gif"

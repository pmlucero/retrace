#!/usr/bin/env bash
# Runs the demo end to end through retrace, against the scripted fake of the
# Anthropic API: record, exact replay, swap-model replay and diff (AC-19,
# AC-20, AC-23). It needs no API key and never calls the real API.
#
# Usage, from the repository root or anywhere:
#
#   examples/demo/e2e.sh [path/to/retrace]
#
# Without an argument it builds retrace into a temporary directory. It needs
# go (only to build) and uv. It fails on the first unexpected exit code.

set -euo pipefail

DEMO_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(cd "$DEMO_DIR/../.." && pwd)
WORK=$(mktemp -d)
PIDS=()

cleanup() {
	# ${PIDS[@]+...} keeps set -u quiet when no server was started.
	for pid in ${PIDS[@]+"${PIDS[@]}"}; do
		kill "$pid" 2>/dev/null || true
	done
	for pid in ${PIDS[@]+"${PIDS[@]}"}; do
		wait "$pid" 2>/dev/null || true
	done
	rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log() { printf '\n==> %s\n' "$*"; }
fail() {
	printf 'e2e: %s\n' "$*" >&2
	exit 1
}

# start <name> <args...>: starts a demo server on a free port, waits up to 30s
# for the "<name>: <url>" line it prints once its port is bound, and stores the
# URL in the variable URL.
start() {
	local name=$1
	shift
	local out="$WORK/$name.out"
	"$DEMO_DIR/.venv/bin/python" "$@" --port 0 >"$out" 2>"$WORK/$name.err" &
	PIDS+=("$!")
	local pid=$!
	local deadline=$((SECONDS + 30))
	until head -n 1 "$out" | grep -q "^$name: http://"; do
		if ! kill -0 "$pid" 2>/dev/null; then
			cat "$WORK/$name.err" >&2
			fail "$name exited before printing its URL"
		fi
		if ((SECONDS >= deadline)); then
			fail "$name did not print its URL within 30s"
		fi
		sleep 0.2
	done
	URL=$(head -n 1 "$out")
	URL=${URL#*: }
	echo "$name: $URL"
}

# expect <code> <command...>: runs the command and fails unless it exits with
# the given code.
expect() {
	local want=$1
	shift
	local got=0
	"$@" || got=$?
	if ((got != want)); then
		fail "'$*' exited with $got, want $want"
	fi
	echo "(exit $got, as expected)"
}

if (($# > 0)); then
	RETRACE=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
else
	log "Building retrace"
	(cd "$REPO_DIR" && CGO_ENABLED=0 go build -o "$WORK/retrace" ./cmd/retrace)
	RETRACE=$WORK/retrace
fi

log "Installing the demo's locked dependencies"
(cd "$DEMO_DIR" && uv sync --locked)

log "Starting the MCP servers and the fake Anthropic API"
cd "$DEMO_DIR"
start orders -m servers.orders
ORDERS=$URL
start shipping -m servers.shipping
SHIPPING=$URL
start fake-anthropic fake_anthropic.py
FAKE=$URL

# Global flags go before the command (spec §9.1). Each proxy listens on a
# free port, so the script does not clash with anything on 4790.
mkdir -p "$WORK/cassettes"
rt() { "$RETRACE" --dir "$WORK/cassettes" --no-color "$@"; }
ID=checkout

# The key is a placeholder: the fake ignores it, and it keeps the agent's SDK
# from failing for lack of one.
export ANTHROPIC_API_KEY=test
# The agent reads its model from DEMO_MODEL. Pin it, so that a value in the
# caller's environment cannot change the recorded session.
export DEMO_MODEL=claude-haiku-5-5

log "Recording the session with claude-haiku-5-5 (expect 0)"
expect 0 rt record --session "$ID" --listen 127.0.0.1:0 \
	--anthropic-upstream "$FAKE" \
	--mcp "orders=$ORDERS" --mcp "shipping=$SHIPPING" \
	-- uv run --locked python agent.py
rt show "$ID"

# An exact replay opens no upstream connection, so the servers and the fake
# play no part in it.
log "Exact replay (expect 0)"
expect 0 rt run "$ID" --as "$ID.exact" --listen 127.0.0.1:0

log "Diff with the exact replay (expect 0, AC-19)"
expect 0 rt diff "$ID" "$ID.exact"

# The fake's claude-sonnet-5-5 script calls search_orders without "status",
# an MCP call the cassette does not hold, so strict stops the replay with 3
# (spec §6.2, AC-15).
log "Swap-model replay with claude-sonnet-5-5 (expect 3)"
expect 3 rt run "$ID" --as "$ID.swap" --listen 127.0.0.1:0 \
	--model claude-sonnet-5-5 --anthropic-upstream "$FAKE"
rt show "$ID.swap"

log "Diff with the swap-model replay (expect 1, AC-20)"
expect 1 rt diff "$ID" "$ID.swap"

# AC-20 also asks for the right divergence point, so check it in the stable
# JSON format (spec §8.4): step 1, a search_orders tool_use whose arguments
# lose "status". The exit code alone would pass for any miss.
log "Checking the divergence point"
json=$(rt diff "$ID" "$ID.swap" --format json) || true
"$DEMO_DIR/.venv/bin/python" -I -c '
import json, sys
p = json.loads(sys.argv[1])["first_divergence"]
got = (p["step"], p["kind"], p["a"]["tool"], p["b"]["tool"], [e["path"] for e in p["diff"]])
want = (1, "tool_use", "search_orders", "search_orders", ["$.status"])
if got != want:
    sys.exit(f"e2e: divergence point is {got}, want {want}")
print("step 1, tool_use search_orders, $.status: as expected")
' "$json"

log "The demo ran end to end"

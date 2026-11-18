# Hidden setup for demo.tape (see SMOKE.md). Source it, do not run it:
#
#   source examples/demo/vhs-setup.sh && demo_up      # before the visible part
#   demo_down                                         # after it
#
# demo_up starts the two MCP servers on 9001 and 9002, plus a second shipping
# server on 9003 whose track_package answers differently (--delayed), uses a throwaway
# cassette directory (RETRACE_DIR), puts a retrace binary on PATH and cds to
# examples/demo. With DEMO_FAKE=1 it also starts the scripted fake Anthropic
# API on 9000 and wraps `retrace` so that record and run go to it. That
# rehearses the tape for free. Without DEMO_FAKE the tape uses the real API
# and needs ANTHROPIC_API_KEY. Needs uv, and go unless retrace is on PATH.

_DEMO_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
_DEMO_PIDS=()

_demo_wait() { # <port>: waits up to 30s for a local TCP port
	local i
	for ((i = 0; i < 150; i++)); do
		(exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null && return 0
		sleep 0.2
	done
	echo "demo: nothing listens on port $1" >&2
	return 1
}

demo_up() {
	_DEMO_WORK=$(mktemp -d)
	(cd "$_DEMO_DIR" && uv sync --locked -q) || return 1
	if ! command -v retrace >/dev/null; then
		(cd "$_DEMO_DIR/../.." && CGO_ENABLED=0 go build -o "$_DEMO_WORK/bin/retrace" ./cmd/retrace) || return 1
		export PATH="$_DEMO_WORK/bin:$PATH"
	fi
	export RETRACE_DIR="$_DEMO_WORK/cassettes" NO_COLOR=1
	export DEMO_MODEL=claude-haiku-5-5
	cd "$_DEMO_DIR" || return 1
	local py=.venv/bin/python
	$py -m servers.orders --port 9001 >/dev/null 2>&1 &
	_DEMO_PIDS+=("$!")
	$py -m servers.shipping --port 9002 >/dev/null 2>&1 &
	_DEMO_PIDS+=("$!")
	$py -m servers.shipping --port 9003 --delayed >/dev/null 2>&1 &
	_DEMO_PIDS+=("$!")
	_demo_wait 9001 && _demo_wait 9002 && _demo_wait 9003 || return 1
	if [[ -n ${DEMO_FAKE:-} ]]; then
		$py fake_anthropic.py --port 9000 >/dev/null 2>&1 &
		_DEMO_PIDS+=("$!")
		_demo_wait 9000 || return 1
		export ANTHROPIC_API_KEY=test
		# The visible commands stay the same: only here do record and the
		# run with --model learn where the fake
		# is. The live-tools run serves the LLM from the cassette.
		retrace() {
			case "${1:-} $*" in
			"record "* | "run "*" --model "*)
				command retrace "$1" --anthropic-upstream http://127.0.0.1:9000 "${@:2}" ;;
			*) command retrace "$@" ;;
			esac
		}
	elif [[ -z ${ANTHROPIC_API_KEY:-} ]]; then
		# Also needed by the replays that make no API call: the agent's SDK
		# refuses to start without a key, whatever its value.
		echo "demo: set ANTHROPIC_API_KEY, or DEMO_FAKE=1 to rehearse" >&2
		return 1
	fi
}

demo_down() {
	local pid
	for pid in ${_DEMO_PIDS[@]+"${_DEMO_PIDS[@]}"}; do kill "$pid" 2>/dev/null; done
	for pid in ${_DEMO_PIDS[@]+"${_DEMO_PIDS[@]}"}; do wait "$pid" 2>/dev/null; done
	_DEMO_PIDS=()
	unset -f retrace
	[[ -n ${_DEMO_WORK:-} ]] && rm -rf "$_DEMO_WORK"
	return 0
}

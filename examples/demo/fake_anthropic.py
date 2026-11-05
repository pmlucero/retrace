"""A scripted stand-in for the Anthropic Messages API, for the demo in CI.

This is a test double, not an emulation of the real API. It answers
POST /v1/messages (non-streaming only) with canned responses: it picks a script
by the request's model, and the step by how many assistant messages the request
already holds. It ignores the tools, the tool results and everything else in
the request, so the same request always gets the same response. That is what
lets CI record and replay the demo without an API key or network access.

Run from examples/demo with: uv run python fake_anthropic.py [--port 9000]
"""

import argparse
import json
import signal
import sys
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

HOST = "127.0.0.1"
DEFAULT_PORT = 9000
NAME = "fake-anthropic"


FINAL_HAIKU = (
    "Your order o_1002 (a steel kettle) has shipped with Parcelo and is in transit. "
    "It was last scanned at the Central hub on 2026-10-21, and it should arrive "
    "on 2026-10-24. You can follow it with tracking id trk_7342."
)

FINAL_SONNET = (
    "Your shipped order is o_1002, a steel kettle. Parcelo has it in transit "
    "(tracking id trk_7342), its last scan was at the Central hub on 2026-10-21, "
    "and it is expected on 2026-10-24."
)

# A script step: a tool call as (tool name, input), or the final answer.
Step = tuple[str, dict[str, Any]] | str

# One script per model, with the tag for its message ids: tool calls in order,
# then the final answer. The two differ in the first call only: haiku filters
# by status, sonnet fetches every order, so replaying a haiku recording with
# sonnet diverges at step 1.
SCRIPTS: dict[str, tuple[str, list[Step]]] = {
    "claude-haiku-5-5": (
        "haiku",
        [
            ("search_orders", {"customer_id": "c_812", "status": "shipped"}),
            ("get_order", {"order_id": "o_1002"}),
            ("track_package", {"tracking_id": "trk_7342"}),
            FINAL_HAIKU,
        ],
    ),
    "claude-sonnet-5-5": (
        "sonnet",
        [
            ("search_orders", {"customer_id": "c_812"}),
            ("get_order", {"order_id": "o_1002"}),
            ("track_package", {"tracking_id": "trk_7342"}),
            FINAL_SONNET,
        ],
    ),
}


class Error(Exception):
    """An Anthropic-style error response."""

    def __init__(self, status: HTTPStatus, kind: str, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.kind = kind
        self.message = message


def respond(request: dict[str, Any]) -> dict[str, Any]:
    """Return the scripted response to a parsed /v1/messages request."""
    if request.get("stream"):
        raise Error(HTTPStatus.BAD_REQUEST, "invalid_request_error", "streaming is not supported by the fake")
    model = request.get("model")
    if model not in SCRIPTS:
        raise Error(HTTPStatus.NOT_FOUND, "not_found_error", f"model: {model}")
    messages = request.get("messages")
    if not isinstance(messages, list):
        raise Error(HTTPStatus.BAD_REQUEST, "invalid_request_error", "messages: field required")

    tag, script = SCRIPTS[model]
    step = sum(1 for message in messages if isinstance(message, dict) and message.get("role") == "assistant")
    if step >= len(script):
        raise Error(HTTPStatus.BAD_REQUEST, "invalid_request_error", f"step {step + 1} is past the {model} script")

    action = script[step]
    if isinstance(action, str):
        content = [{"type": "text", "text": action}]
        stop_reason = "end_turn"
    else:
        name, arguments = action
        content = [{"type": "tool_use", "id": f"toolu_demo_{step + 1}", "name": name, "input": arguments}]
        stop_reason = "tool_use"
    return {
        "id": f"msg_demo_{tag}_{step + 1}",
        "type": "message",
        "role": "assistant",
        "model": model,
        "content": content,
        "stop_reason": stop_reason,
        "stop_sequence": None,
        "usage": {"input_tokens": 400 + 100 * step, "output_tokens": 40},
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self) -> None:
        try:
            if self.path.split("?", 1)[0] != "/v1/messages":
                raise Error(HTTPStatus.NOT_FOUND, "not_found_error", f"no route for POST {self.path}")
            request = self.read_json()
            self.send(HTTPStatus.OK, respond(request))
        except Error as err:
            # The body may be unread, so the connection cannot be reused.
            self.close_connection = True
            self.send(err.status, {"type": "error", "error": {"type": err.kind, "message": err.message}})

    def read_json(self) -> dict[str, Any]:
        """Read the request body, which must be a JSON object with a Content-Length."""
        header = self.headers.get("Content-Length")
        if header is None:
            raise Error(HTTPStatus.LENGTH_REQUIRED, "invalid_request_error", "the request has no Content-Length")
        try:
            length = int(header)
        except ValueError:
            length = -1
        if length < 0:
            raise Error(HTTPStatus.BAD_REQUEST, "invalid_request_error", f"invalid Content-Length {header!r}")
        try:
            request = json.loads(self.rfile.read(length))
        except ValueError:
            raise Error(HTTPStatus.BAD_REQUEST, "invalid_request_error", "the body is not valid JSON") from None
        if not isinstance(request, dict):
            raise Error(HTTPStatus.BAD_REQUEST, "invalid_request_error", "the body is not a JSON object")
        return request

    def send(self, status: HTTPStatus, body: dict[str, Any]) -> None:
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        if self.close_connection:
            self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, format: str, *args: Any) -> None:
        pass  # keep stderr quiet, like the MCP servers


def main() -> None:
    parser = argparse.ArgumentParser(description="Run the scripted fake Anthropic Messages API.")
    parser.add_argument(
        "--port",
        type=int,
        default=DEFAULT_PORT,
        help=f"TCP port on {HOST} (default {DEFAULT_PORT}; 0 picks a free port)",
    )
    args = parser.parse_args()

    try:
        server = ThreadingHTTPServer((HOST, args.port), Handler)
    except OSError as err:
        sys.exit(f"{NAME}: cannot listen on {HOST}:{args.port}: {err.strerror}")
    server.daemon_threads = True

    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: sys.exit(0))

    # The base URL, for ANTHROPIC_BASE_URL, in the same "<name>: <url>" form
    # as the MCP servers, once the port is bound.
    print(f"{NAME}: http://{HOST}:{server.server_address[1]}", flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()

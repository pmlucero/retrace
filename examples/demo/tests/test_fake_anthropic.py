"""Calls the scripted fake Anthropic API over HTTP."""

import http.client
import json
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

import pytest

from fake_anthropic import FINAL_HAIKU, FINAL_SONNET


def post(base_url: str, body: dict[str, Any], path: str = "/v1/messages") -> tuple[int, dict[str, Any]]:
    request = urllib.request.Request(
        base_url + path,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as err:
        return err.code, json.load(err)


def conversation(model: str, steps: int) -> dict[str, Any]:
    """A request that already holds steps assistant turns."""
    messages: list[dict[str, Any]] = [{"role": "user", "content": "Where is my order?"}]
    for n in range(1, steps + 1):
        messages.append({"role": "assistant", "content": [{"type": "text", "text": f"turn {n}"}]})
        messages.append({"role": "user", "content": "go on"})
    return {"model": model, "max_tokens": 1024, "messages": messages}


SHIPPED = {"customer_id": "c_812", "status": "shipped"}
ALL = {"customer_id": "c_812"}


@pytest.mark.parametrize(
    ("model", "tag", "first_input", "final"),
    [
        ("claude-haiku-5-5", "haiku", SHIPPED, FINAL_HAIKU),
        ("claude-sonnet-5-5", "sonnet", ALL, FINAL_SONNET),
    ],
)
def test_scripts(anthropic_url: str, model: str, tag: str, first_input: dict, final: str) -> None:
    want_calls = [
        ("search_orders", first_input),
        ("get_order", {"order_id": "o_1002"}),
        ("track_package", {"tracking_id": "trk_7342"}),
    ]
    for step, (name, arguments) in enumerate(want_calls):
        status, body = post(anthropic_url, conversation(model, step))
        assert status == 200
        assert body["id"] == f"msg_demo_{tag}_{step + 1}"
        assert body["type"] == "message"
        assert body["role"] == "assistant"
        assert body["model"] == model
        assert body["stop_reason"] == "tool_use"
        assert body["content"] == [
            {"type": "tool_use", "id": f"toolu_demo_{step + 1}", "name": name, "input": arguments}
        ]
        assert set(body["usage"]) == {"input_tokens", "output_tokens"}

    status, body = post(anthropic_url, conversation(model, 3))
    assert status == 200
    assert body["stop_reason"] == "end_turn"
    assert body["content"] == [{"type": "text", "text": final}]

    status, body = post(anthropic_url, conversation(model, 4))
    assert status == 400
    assert body["error"]["type"] == "invalid_request_error"


def test_responses_are_deterministic(anthropic_url: str) -> None:
    request = conversation("claude-haiku-5-5", 1)
    assert post(anthropic_url, request) == post(anthropic_url, request)


@pytest.mark.parametrize(
    ("headers", "want_status"),
    [
        ({}, 411),
        ({"Content-Length": "many"}, 400),
        ({"Content-Length": "-1"}, 400),
    ],
)
def test_content_length_errors(anthropic_url: str, headers: dict[str, str], want_status: int) -> None:
    connection = http.client.HTTPConnection(urllib.parse.urlsplit(anthropic_url).netloc, timeout=10)
    try:
        connection.putrequest("POST", "/v1/messages")
        for name, value in headers.items():
            connection.putheader(name, value)
        connection.endheaders()
        response = connection.getresponse()
        assert response.status == want_status
        assert json.load(response)["error"]["type"] == "invalid_request_error"
    finally:
        connection.close()


@pytest.mark.parametrize(
    ("body", "path", "want_status", "want_type"),
    [
        (conversation("claude-unknown", 0), "/v1/messages", 404, "not_found_error"),
        ({**conversation("claude-haiku-5-5", 0), "stream": True}, "/v1/messages", 400, "invalid_request_error"),
        ({"model": "claude-haiku-5-5"}, "/v1/messages", 400, "invalid_request_error"),
        (conversation("claude-haiku-5-5", 0), "/v1/complete", 404, "not_found_error"),
    ],
)
def test_errors(anthropic_url: str, body: dict, path: str, want_status: int, want_type: str) -> None:
    status, response = post(anthropic_url, body, path)
    assert status == want_status
    assert response["type"] == "error"
    assert response["error"]["type"] == want_type

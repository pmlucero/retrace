"""Fixtures that run the demo's servers as processes on free ports."""

import signal
import subprocess
import sys
from collections.abc import Iterator
from pathlib import Path

import pytest

DEMO_DIR = Path(__file__).resolve().parent.parent


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


def start(*args: str) -> tuple[subprocess.Popen[str], str]:
    """Start a server on a free port and return the process and its URL.

    Every demo server prints "<name>: <url>" as its first line once its port
    is bound.
    """
    proc = subprocess.Popen(
        [sys.executable, *args, "--port", "0"],
        cwd=DEMO_DIR,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
    )
    line = proc.stdout.readline()  # blocks until the port is bound, or EOF
    if not line:
        proc.kill()
        pytest.fail(f"{' '.join(args)} exited before printing its URL (status {proc.wait()})")
    return proc, line.split(": ", 1)[1].strip()


def stop(proc: subprocess.Popen[str]) -> int:
    proc.send_signal(signal.SIGTERM)
    try:
        return proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
        raise


def serve(*args: str) -> Iterator[str]:
    proc, url = start(*args)
    yield url
    assert stop(proc) == 0


@pytest.fixture(scope="session")
def orders_url() -> Iterator[str]:
    yield from serve("-m", "servers.orders")


@pytest.fixture(scope="session")
def shipping_url() -> Iterator[str]:
    yield from serve("-m", "servers.shipping")


@pytest.fixture(scope="session")
def anthropic_url() -> Iterator[str]:
    yield from serve("fake_anthropic.py")

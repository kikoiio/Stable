#!/usr/bin/env python3
"""Drive the production no-argument Stable entry point through a Linux PTY."""
from __future__ import annotations

import argparse
import fcntl
import http.server
import json
import os
import pty
import select
import signal
import socket
import struct
import subprocess
import sys
import termios
import threading
import time
from pathlib import Path


class Provider(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *_args):
        pass

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            request = json.loads(self.rfile.read(size))
            messages = request.get("messages", [])
            user_text = next(
                (message.get("content", "") for message in reversed(messages)
                 if message.get("role") == "user"),
                "",
            )
            with self.server.requests_lock:
                self.server.requests.append(user_text)
        except Exception as error:
            self.send_error(400, str(error))
            return

        if "M01_PROVIDER_ERROR" in user_text:
            frames = [{"error": {"message": "controlled M01 provider failure"}}]
        elif "M01_LONG_RESPONSE" in user_text:
            frames = [
                {"choices": [{"delta": {"content": f"M01_LONG_{index:03d}\n"}, "finish_reason": None}]}
                for index in range(120)
            ]
            frames.append({"choices": [{"delta": {}, "finish_reason": "stop"}]})
        else:
            frames = [
                {"choices": [{"delta": {"content": "M01_FAKE_REPLY"}, "finish_reason": None}]},
                {"choices": [{"delta": {}, "finish_reason": "stop"}]},
            ]

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        for frame in frames:
            payload = ("data: " + json.dumps(frame, ensure_ascii=False) + "\n\n").encode()
            self.wfile.write(payload)
            self.wfile.flush()
            time.sleep(0.005)
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()


class TTYRun:
    def __init__(self, executable: Path, cwd: Path, env: dict[str, str]):
        self.executable = executable
        self.cwd = cwd
        self.env = env
        self.pid = -1
        self.fd = -1
        self.output = bytearray()

    def start(self):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.chdir(self.cwd)
            os.execve(self.executable, [str(self.executable)], self.env)
        self.resize(100, 32)
        # The compact status footer may be clipped at the initial width; the
        # rendered empty-session state is stable and confirms TUI readiness.
        self.wait_for("空会话".encode(), 60)

    def resize(self, columns: int, rows: int):
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ,
                    struct.pack("HHHH", rows, columns, 0, 0))
        if self.pid > 0:
            os.kill(self.pid, signal.SIGWINCH)

    def send(self, value: bytes):
        os.write(self.fd, value)

    def wait_for(self, token: bytes, timeout: float = 20, start: int = 0, label: str = "terminal output"):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if token in self.output[start:]:
                return
            ready, _, _ = select.select([self.fd], [], [], 0.2)
            if ready:
                try:
                    data = os.read(self.fd, 65536)
                except OSError:
                    data = b""
                if data:
                    self.output.extend(data)
                    continue
            waited, status = os.waitpid(self.pid, os.WNOHANG)
            if waited:
                self.pid = -1
                raise RuntimeError(f"stable exited before {token!r} (status {status})")
        tail = bytes(self.output[-4000:]).decode("utf-8", "replace")
        raise TimeoutError(f"timed out waiting for {label} {token!r}; terminal tail:\n{tail}")

    def stop(self):
        if self.pid <= 0:
            return
        try:
            self.send(b"\x03")
        except OSError:
            pass
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            waited, status = os.waitpid(self.pid, os.WNOHANG)
            if waited:
                self.pid = -1
                if not os.WIFEXITED(status) or os.WEXITSTATUS(status) != 0:
                    raise RuntimeError(f"stable TUI exit status: {status}")
                return
            time.sleep(0.1)
        os.kill(self.pid, signal.SIGTERM)
        _, status = os.waitpid(self.pid, 0)
        self.pid = -1
        raise RuntimeError(f"stable TUI did not exit on Ctrl+C (status {status})")


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--stage", required=True, type=Path)
    args = parser.parse_args()
    stage = args.stage.resolve()
    home = stage / "home"
    # Match the production runtime trusted project root (`p.Share`).
    project = stage / "share"
    state = stage / "state"
    (stage / "tmp").mkdir(parents=True, exist_ok=True, mode=0o700)
    for directory in (home, home / ".config", home / ".cache", home / ".local" / "state", project, state):
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(directory, 0o700)
    (project / "inside-note.txt").write_text("M01 project-only completion fixture\n", encoding="utf-8")
    (stage / "outside-note.txt").write_text("outside the TUI project root\n", encoding="utf-8")
    (project / "outside-link.txt").symlink_to(stage / "outside-note.txt")

    provider = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    provider.requests = []
    provider.requests_lock = threading.Lock()
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    temporal_port = free_port()
    config_path = stage / "config.json"
    config = {
        "model": {
            "provider": "openai-compatible",
            "model": "m01-tty-fake",
            "base_url": f"http://127.0.0.1:{provider.server_port}/v1",
            "api_key": "m01-tty-fake-key",
        },
        "state_dir": str(state),
        "temporal_port": temporal_port,
    }
    config_path.write_text(json.dumps(config), encoding="utf-8")
    os.chmod(config_path, 0o600)
    # Keep only runtime essentials and the private fake-provider configuration;
    # no host credentials, config paths, or user XDG directories reach children.
    env = {
        "PATH": "/usr/local/bin:/usr/bin:/bin",
        "HOME": str(home),
        "TMPDIR": str(stage / "tmp"),
        "XDG_CONFIG_HOME": str(home / ".config"),
        "XDG_CACHE_HOME": str(home / ".cache"),
        "XDG_DATA_HOME": str(home / ".local" / "share"),
        "LANG": os.environ.get("LANG", "C.UTF-8"),
        "STABLE_CONFIG": str(config_path),
        "STABLE_STATE_DIR": str(state),
        "STABLE_TEMPORAL_PORT": str(temporal_port),
        "STABLE_PROVIDER": "openai-compatible",
        "STABLE_MODEL": "m01-tty-fake",
        "STABLE_BASE_URL": f"http://127.0.0.1:{provider.server_port}/v1",
        "STABLE_API_KEY": "m01-tty-fake-key",
        "TERM": "xterm-256color",
    }

    stable = stage / "bin" / "stable"
    tui = TTYRun(stable, project, env)
    try:
        tui.start()

        # Session navigation through command completion, then load the selected session.
        offset = len(tui.output)
        tui.send(b"/sess\t\r")
        tui.wait_for("会话".encode(), start=offset)
        offset = len(tui.output)
        tui.send(b"\r")
        tui.wait_for("空会话".encode(), start=offset)

        # Goal navigation and return to the same production chat view.
        offset = len(tui.output)
        tui.send(b"\x07")  # Ctrl+G
        tui.wait_for("目标".encode(), start=offset)
        offset = len(tui.output)
        tui.send(b"\x1b")
        tui.wait_for("输入消息".encode(), start=offset)

        # Ctrl+J inserts a real newline; Enter submits both lines to the fake provider.
        offset = len(tui.output)
        tui.send(b"M01_MULTI_FIRST\nM01_MULTI_SECOND\r")
        tui.wait_for(b"M01_FAKE_REPLY", 30, offset)
        with provider.requests_lock:
            if not any(value == "M01_MULTI_FIRST\nM01_MULTI_SECOND" for value in provider.requests):
                raise AssertionError("fake provider did not receive the two-line TTY message")
        tui.wait_for(b"completed", 30, offset)

        # Long streaming response, page navigation, terminal resize, and reflow.
        offset = len(tui.output)
        tui.send(b"M01_LONG_RESPONSE\r")
        tui.wait_for(b"M01_LONG_119", 30, offset, "long reply stream")
        tui.wait_for(b"completed", 30, offset, "long run completion")
        offset = len(tui.output)
        tui.send(b"\x1b[5~")  # Page Up
        tui.wait_for(b"M01_LONG_000", start=offset, label="Page Up transcript view")
        offset = len(tui.output)
        for _ in range(8):
            tui.send(b"\x1b[6~")  # Page Down to the latest transcript viewport.
            time.sleep(0.05)
        tui.wait_for(b"M01_LONG_119", start=offset, label="Page Down transcript view")
        offset = len(tui.output)
        tui.resize(58, 16)
        tui.wait_for(b"M01_LONG_119", start=offset, label="resized transcript view")

        # A provider error must be visible in the TUI instead of looking like success.
        offset = len(tui.output)
        tui.send(b"M01_PROVIDER_ERROR\r")
        tui.wait_for("模型错误".encode(), 30, offset)
        tui.wait_for(b"failed", 30, offset, "provider error run completion")

        # File completion accepts a trusted-share file and excludes an outward symlink.
        offset = len(tui.output)
        tui.send(b"@inside-note\t")
        tui.wait_for(b"@inside-note.txt", start=offset)
        tui.send(b"\x7f" * 32)  # Clear the completion draft with Backspace.
        offset = len(tui.output)
        tui.send(b"@outside-link\t")
        tui.wait_for(b"@outside-link", start=offset)
        if b"@outside-link.txt" in tui.output[offset:]:
            raise AssertionError("trusted-root-external symlink appeared in TTY completion")

        tui.stop()
        print("M01 production TTY acceptance passed: session completion/load, goal navigation, multiline chat, long reply/page/resize, provider error, and bounded path completion.")
        return 0
    finally:
        try:
            tui.stop()
        except Exception:
            pass
        try:
            subprocess.run([str(stable), "down"], env=env, cwd=project,
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                           text=True, timeout=25, check=True)
        finally:
            provider.shutdown()
            provider.server_close()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"M01 TTY acceptance failed: {error}", file=sys.stderr)
        raise SystemExit(1)

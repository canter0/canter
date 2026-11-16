#!/usr/bin/env python3
"""Durable scope-local ACP stdio bridge; never runs on the control-plane host."""
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import sys
import time
from datetime import datetime

MAX_MESSAGE = 2 << 20
MAX_EVENTS = 16 << 20

def atomic(path, value):
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value))
    temporary.replace(path)

def main():
    directory = Path(sys.argv[1]).resolve()
    root = Path("/workspace").resolve()
    if not directory.is_relative_to(root) or directory.parent != root / ".canter/harness":
        raise RuntimeError("invalid harness scope")
    # A durable stop receipt wins over a delayed start delivery. Never launch a
    # replacement agent after the control plane already confirmed this stop.
    if (directory / "stop").exists():
        atomic(directory / "state.json", {"status": "stopped", "protocol": "acp-v1"})
        return
    launch = json.loads((directory / "launch.json").read_text())
    cwd = Path(launch["cwd"]).resolve()
    if not cwd.is_relative_to(root) or launch["argv"] != ["opencode", "acp"]:
        raise RuntimeError("invalid harness invocation")
    incoming, delivered = directory / "in", directory / "delivered"
    incoming.mkdir(exist_ok=True)
    delivered.mkdir(exist_ok=True)
    # Reusing this process identity must never start another independent agent.
    identity = directory / "process.started"
    descriptor = os.open(identity, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.write(descriptor, str(os.getpid()).encode())
    os.close(descriptor)
    environment = {**os.environ, **launch["environment"]}
    environment.pop("OPENROUTER_API_KEY", None)
    environment.pop("OPENAI_API_KEY", None)
    environment.pop("ANTHROPIC_API_KEY", None)
    Path(environment.get("HOME", str(directory / "home"))).mkdir(parents=True, exist_ok=True)
    environment["PATH"] = "/home/wren/.opencode/bin:/usr/local/bin:" + environment.get("PATH", "/usr/bin:/bin")
    process = subprocess.Popen(launch["argv"], cwd=cwd, env=environment, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0)
    try:
        atomic(directory / "state.json", {"status": "running", "pid": process.pid, "protocol": "acp-v1"})
        selector = selectors.DefaultSelector()
        selector.register(process.stdout, selectors.EVENT_READ, "stdout")
        selector.register(process.stderr, selectors.EVENT_READ, "stderr")
        remaining = datetime.fromisoformat(launch["expiresAt"].replace("Z", "+00:00")).timestamp() - time.time()
        deadline = time.monotonic() + min(launch["timeoutSeconds"], max(0, remaining))
        buffer, total, diagnostic = b"", 0, 0
        with (directory / "events.jsonl").open("ab", buffering=0) as events, (directory / "stderr.log").open("ab", buffering=0) as stderr:
            while process.poll() is None:
                if (directory / "stop").exists() or time.monotonic() >= deadline:
                    process.stdin.close()
                    try:
                        process.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        process.terminate()
                    break
                for target in sorted(incoming.glob("*.json")):
                    identifier = target.stem
                    if not re.fullmatch(r"[0-9]{20}", identifier) or (delivered / identifier).exists():
                        continue
                    raw = target.read_bytes()
                    if len(raw) > MAX_MESSAGE:
                        raise RuntimeError("ACP input exceeded message limit")
                    json.loads(raw)
                    # Intent precedes the write. If the bridge dies in the gap,
                    # this delivery is uncertain and must not be blindly repeated.
                    receipt = delivered / identifier
                    receipt.write_text("sending")
                    process.stdin.write(raw + b"\n")
                    process.stdin.flush()
                    receipt.write_text("delivered")
                for key, _ in selector.select(0.05):
                    chunk = os.read(key.fileobj.fileno(), 65536)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        continue
                    if key.data == "stderr":
                        remaining = max(0, (64 << 10) - diagnostic)
                        stderr.write(chunk[:remaining])
                        diagnostic += min(remaining, len(chunk))
                        continue
                    buffer += chunk
                    if len(buffer) > MAX_MESSAGE and b"\n" not in buffer:
                        raise RuntimeError("ACP output exceeded message limit")
                    while b"\n" in buffer:
                        line, buffer = buffer.split(b"\n", 1)
                        if not line.strip():
                            continue
                        if len(line) > MAX_MESSAGE:
                            raise RuntimeError("ACP output exceeded message limit")
                        message = json.loads(line)
                        if not isinstance(message, dict) or message.get("jsonrpc") != "2.0":
                            raise RuntimeError("agent returned invalid ACP output")
                        encoded = json.dumps({"message": message}, separators=(",", ":")).encode() + b"\n"
                        total += len(encoded)
                        if total > MAX_EVENTS:
                            raise RuntimeError("ACP event log exceeded bounded allowance")
                        events.write(encoded)
                        os.fsync(events.fileno())
            # Drain output already available when the agent exits, including the
            # final prompt response. Never invent one from an OS exit status.
            while selector.get_map():
                ready = selector.select(0.1)
                if not ready:
                    break
                for key, _ in ready:
                    chunk = os.read(key.fileobj.fileno(), 65536)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        continue
                    if key.data != "stdout":
                        continue
                    buffer += chunk
                    while b"\n" in buffer:
                        line, buffer = buffer.split(b"\n", 1)
                        if not line.strip():
                            continue
                        if len(line) > MAX_MESSAGE:
                            raise RuntimeError("ACP output exceeded message limit")
                        message = json.loads(line)
                        encoded = json.dumps({"message": message}, separators=(",", ":")).encode() + b"\n"
                        total += len(encoded)
                        if total > MAX_EVENTS:
                            raise RuntimeError("ACP event log exceeded bounded allowance")
                        events.write(encoded)
            try:
                code = process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                code = process.wait()
    finally:
        if process.stdin and not process.stdin.closed:
            process.stdin.close()
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
    atomic(directory / "state.json", {"status": "exited", "exitCode": code, "protocol": "acp-v1"})
    return code

if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        # No launch environment or broker credential is emitted to logs.
        sys.stderr.write("ACP bridge failed; inspect its scoped state before retrying.\n")
        sys.exit(1)

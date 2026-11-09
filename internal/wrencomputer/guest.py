#!/usr/bin/env python3
"""Root-owned SSH RPC helper. Its Unix account is the computer boundary.

No provider credentials, control-plane filesystem or other computers are mounted.
All long-running commands get an independent timeout and a durable job identity.
"""
import base64
import fcntl
import hashlib
import io
import struct
import json
import math
import os
from pathlib import Path
import re
import runpy
import secrets
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from urllib.parse import urlparse

ROOT = Path(os.environ.get("CANTER_COMPUTER_ROOT", "/workspace")).resolve()
STATE = Path(os.environ.get("CANTER_COMPUTER_STATE", "/home/wren/.canter")).resolve()
JOBS = STATE / "jobs"
MAX_FILE = 8 << 20
MAX_LOG = 1 << 20
SANDBOX_LOG_CAP = 32 << 20
SANDBOX_JOB_TIMEOUT = 86400
UPLOADS = STATE / "uploads"
UPLOAD_CHUNK = 4 << 20
UPLOAD_MAX = 4 << 30
UPLOAD_LIMIT = 4
UPLOAD_TTL = 86400
SANDBOX_MARKER = Path(os.environ.get("CANTER_SANDBOX_MARKER", "/opt/canter/sandbox"))
ENV_KEY = re.compile(r"^[A-Za-z_][A-Za-z0-9_]{0,127}$")

class Failure(Exception):
    def __init__(self, code, message):
        self.code, self.message = code, message
        super().__init__(message)

def now():
    return datetime.now(timezone.utc).isoformat()

def save(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(data, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)

def workspace_path(raw, allow_root=True):
    if not isinstance(raw, str) or len(raw) > 4096 or "\0" in raw:
        raise Failure("invalid_path", "Choose a path inside /workspace.")
    if raw in ("", "/workspace", "."):
        value = ROOT
    elif raw.startswith("/workspace/"):
        value = ROOT / raw[len("/workspace/"):]
    elif raw.startswith("/"):
        raise Failure("invalid_path", "Files are restricted to /workspace.")
    else:
        value = ROOT / raw
    value = value.resolve()
    if not value.is_relative_to(ROOT) or (not allow_root and value == ROOT):
        raise Failure("invalid_path", "Files are restricted to /workspace.")
    return value

PRIVATE_DIRS = {".canter", ".browser"}


def user_workspace_path(raw, allow_root=True):
    resolved = workspace_path(raw, allow_root)
    try:
        relative = resolved.relative_to(ROOT)
    except ValueError:
        raise Failure("invalid_path", "Files are restricted to /workspace.")
    if relative.parts and relative.parts[0] in PRIVATE_DIRS:
        raise Failure("forbidden", "Choose a path outside the sandbox's private data.")
    return resolved


def job_path(identifier):
    if not isinstance(identifier, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{7,99}", identifier):
        raise Failure("invalid_job", "Invalid computer job ID.")
    return JOBS / identifier

def proc_start(pid):
    try:
        # Field 22; comm is parenthesized and may contain spaces.
        return Path("/proc/%d/stat" % pid).read_text().rsplit(")", 1)[1].split()[19]
    except (OSError, IndexError):
        try:
            value = subprocess.check_output(["ps", "-o", "lstart=", "-p", str(pid)], stderr=subprocess.DEVNULL, timeout=2).decode().strip()
            return value or None
        except (OSError, subprocess.SubprocessError):
            return None

def same_process(pid, start):
    if not pid or not start:
        return False
    return proc_start(pid) == start

def job_state(identifier):
    path = job_path(identifier)
    try:
        state = json.loads((path / "state.json").read_text())
    except FileNotFoundError:
        raise Failure("not_found", "Computer job was not found.")
    if state["status"] in ("starting", "running") and state.get("workerPid") and not same_process(state["workerPid"], state.get("workerStart")):
        state.update(status="interrupted", updatedAt=now(), error="The worker exited without a final result. Inspect files before retrying with a new job ID.")
        save(path / "state.json", state)
    return state


def inspect_job(identifier):
    path = job_path(identifier)
    state = job_state(identifier)
    # Bounded LAST-1MiB legacy snapshot — never read the full 32MiB log budget.
    try:
        with (path / "output.log").open("rb") as stream:
            size = os.fstat(stream.fileno()).st_size
            partial = size > MAX_LOG
            stream.seek(max(0, size - MAX_LOG))
            data = stream.read(min(size, MAX_LOG))
    except FileNotFoundError:
        data, partial = b"", False
    public = {key: value for key, value in state.items() if key not in ("pid", "start", "workerPid", "workerStart")}
    public["output"] = data.decode("utf-8", errors="replace")
    public["outputTruncated"] = state.get("outputTruncated", False) or partial
    return public

def start_job(data):
    identifier = data.get("id")
    path = job_path(identifier)
    command = data.get("command", "")
    timeout = data.get("timeoutSeconds", 300) or 300
    if not isinstance(command, str) or not command.strip() or len(command.encode()) > 64000:
        raise Failure("invalid_command", "A command of up to 64000 bytes is required.")
    sandbox = bool(data.get("sandbox"))
    limit = SANDBOX_JOB_TIMEOUT if sandbox else 3600
    if sandbox and not SANDBOX_MARKER.exists():
        raise Failure("invalid_request", "Sandbox jobs require the sandbox helper.")
    if isinstance(timeout, bool) or not isinstance(timeout, int) or timeout < 1 or timeout > limit:
        raise Failure("invalid_timeout", "A command timeout must be between 1 and %d seconds." % limit)
    environment = data.get("env")
    if environment is not None:
        if sandbox and not SANDBOX_MARKER.exists():
            raise Failure("invalid_request", "Sandbox jobs require the sandbox helper.")
        if not isinstance(environment, dict) or len(environment) > 64:
            raise Failure("invalid_env", "A job accepts at most 64 environment entries.")
        total = 0
        for key, value in environment.items():
            if not isinstance(key, str) or not ENV_KEY.fullmatch(key):
                raise Failure("invalid_env", "Invalid environment variable name.")
            if not isinstance(value, str) or "\x00" in value or len(value.encode()) > 8192:
                raise Failure("invalid_env", "Invalid environment variable value.")
            total += len(key) + len(value.encode())
        if total > 32768:
            raise Failure("invalid_env", "Environment values exceed their aggregate limit.")
    directory = workspace_path(data.get("directory", ""))
    if not directory.is_dir():
        raise Failure("invalid_directory", "The working directory does not exist.")
    request = {"id": identifier, "command": command, "directory": str(directory), "timeoutSeconds": timeout}
    if environment is not None or sandbox:
        request["env"] = environment or {}
        request["sandbox"] = bool(sandbox)
    digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
    JOBS.mkdir(parents=True, exist_ok=True)
    try:
        path.mkdir()
    except FileExistsError:
        try:
            previous = json.loads((path / "request.json").read_text())
        except (FileNotFoundError, json.JSONDecodeError):
            if (path / "cancel").exists() and (path / "state.json").exists():
                return inspect_job(identifier)
            raise Failure("uncertain", "Job creation is unresolved; inspect the existing job before retrying.")
        if previous.get("digest") != digest:
            raise Failure("conflict", "This job ID already belongs to a different command.")
        return inspect_job(identifier)
    save(path / "request.json", {**request, "digest": digest})
    state = {"id": identifier, "status": "starting", "createdAt": now(), "updatedAt": now(), "outputTruncated": False}
    # The worker waits on this lock before reading state. Persist its identity
    # before releasing it, so a crash during startup is observable on reconnect.
    with (path / "start.lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        save(path / "state.json", state)
        try:
            process = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "--worker", identifier], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True, close_fds=True)
            state.update(workerPid=process.pid, workerStart=proc_start(process.pid))
            save(path / "state.json", state)
        except OSError:
            state.update(status="failed", updatedAt=now(), error="The computer could not start this job.")
            save(path / "state.json", state)
            raise Failure("start_failed", "The computer could not start this job.")
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)
    return inspect_job(identifier)

def stop_job(identifier):
    path = job_path(identifier)
    # A cancelled launch may not have returned a receipt. Persist a tombstone
    # so the same ID cannot start later after control has been handed over.
    path.mkdir(parents=True, exist_ok=True)
    with (path / "start.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            state = json.loads((path / "state.json").read_text())
        except FileNotFoundError:
            (path / "cancel").touch()
            state = {"id": identifier, "status": "cancelled", "createdAt": now(), "updatedAt": now(), "outputTruncated": False}
            save(path / "state.json", state)
        fcntl.flock(lock, fcntl.LOCK_UN)
    if state["status"] not in ("starting", "running"):
        return inspect_job(identifier)
    (path / "cancel").touch()
    if same_process(state.get("pid"), state.get("start")):
        try:
            os.killpg(state["pid"], signal.SIGTERM)
        except ProcessLookupError:
            pass
    # The worker records cancellation only after the child process is reaped.
    for _ in range(15):
        current = inspect_job(identifier)
        if current["status"] not in ("starting", "running"):
            return current
        time.sleep(0.1)
    return inspect_job(identifier)

def worker(identifier):
    path = job_path(identifier)
    request = json.loads((path / "request.json").read_text())
    with (path / "start.lock").open("r") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        state = json.loads((path / "state.json").read_text())
        fcntl.flock(lock, fcntl.LOCK_UN)
    if (path / "cancel").exists():
        state.update(status="cancelled", updatedAt=now())
        save(path / "state.json", state)
        return
    environment = {**os.environ, "DISPLAY": ":99", "HOME": str(Path.home()), "TERM": "xterm-256color"}
    sandbox_job = bool(request.get("sandbox"))
    if sandbox_job:
        environment.update(request.get("env") or {})
        argv, stderr_target = ["/bin/bash", "-c", request["command"]], subprocess.PIPE
    else:
        argv, stderr_target = ["/bin/bash", "-lc", request["command"]], subprocess.STDOUT
    process = subprocess.Popen(argv, cwd=request["directory"], env=environment, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=stderr_target, start_new_session=True, close_fds=True)
    state.update(status="running", pid=process.pid, start=proc_start(process.pid), workerPid=os.getpid(), workerStart=proc_start(os.getpid()), updatedAt=now())
    save(path / "state.json", state)
    deadline = time.monotonic() + request["timeoutSeconds"]
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    if process.stderr is not None:
        selector.register(process.stderr, selectors.EVENT_READ)
    written = 0
    log_cap = SANDBOX_LOG_CAP if sandbox_job else MAX_LOG
    timed_out = False
    stopping_at = None
    streams = {}
    if sandbox_job:
        streams[process.stdout.fileno()] = (path / "stdout.log").open("wb")
        streams[process.stderr.fileno()] = (path / "stderr.log").open("wb")
    with (path / "output.log").open("wb") as log:
        while selector.get_map():
            cancelled = (path / "cancel").exists()
            if time.monotonic() >= deadline:
                timed_out = True
            if cancelled or timed_out:
                try:
                    os.killpg(process.pid, signal.SIGTERM if stopping_at is None else signal.SIGKILL if time.monotonic() - stopping_at > 1 else signal.SIGTERM)
                except ProcessLookupError:
                    pass
                if stopping_at is None:
                    stopping_at = time.monotonic()
            for key, _ in selector.select(0.1):
                chunk = os.read(key.fileobj.fileno(), 65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                remaining = max(0, log_cap - written)
                piece = chunk[:remaining]
                log.write(piece)
                log.flush()
                if key.fileobj.fileno() in streams:
                    streams[key.fileobj.fileno()].write(piece)
                    streams[key.fileobj.fileno()].flush()
                written += len(piece)
                if len(chunk) > remaining:
                    if not state.get("logTruncated"):
                        state["outputTruncated"] = True
                        state["logTruncated"] = True
                        state["updatedAt"] = now()
                        save(path / "state.json", state)
            # A child may daemonize with inherited stdout. Timeout closes those
            # handles too, even if the original shell has already exited.
            if (cancelled or timed_out) and stopping_at and time.monotonic() - stopping_at > 2:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                break
    selector.close()
    for stream in streams.values():
        stream.close()
    try:
        code = process.wait(timeout=3)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        code = process.wait()
    status = "cancelled" if (path / "cancel").exists() else "timed_out" if timed_out else "completed" if code == 0 else "failed"
    state.update(status=status, exitCode=code, updatedAt=now())
    save(path / "state.json", state)

def desktop_env():
    return {**os.environ, "DISPLAY": ":99"}

def desktop_input(data):
    kind = data.get("kind")
    env = desktop_env()
    if kind == "drag":
        values = [data.get(k) for k in ("x", "y", "x2", "y2")]
        if any(isinstance(v, bool) or not isinstance(v, int) or v < 0 or v > 4095 for v in values):
            raise Failure("invalid_input", "Invalid drag coordinates.")
        button = data.get("button", 1) or 1
        if button not in (1, 2, 3):
            raise Failure("invalid_input", "Choose mouse button 1, 2 or 3.")
        x, y, x2, y2 = values
        try:
            subprocess.run(["xdotool", "mousemove", str(x), str(y), "mousedown", str(button), "sleep", "0.05", "mousemove", str(x2), str(y2)], env=env, check=True, timeout=8, stdout=subprocess.DEVNULL)
        finally:
            subprocess.run(["xdotool", "mouseup", str(button)], env=env, timeout=3, stdout=subprocess.DEVNULL)
    elif kind in ("click", "move", "double_click", "right_click"):
        x, y = data.get("x", 0), data.get("y", 0)
        if isinstance(x, bool) or isinstance(y, bool) or not isinstance(x, int) or not isinstance(y, int) or x < 0 or y < 0 or x > 4095 or y > 4095:
            raise Failure("invalid_input", "Invalid screen coordinates.")
        subprocess.run(["xdotool", "mousemove", str(x), str(y)], env=env, check=True, timeout=8, stdout=subprocess.DEVNULL)
        if kind != "move":
            button = 3 if kind == "right_click" else data.get("button", 1) or 1
            if button not in (1, 2, 3):
                raise Failure("invalid_input", "Choose mouse button 1, 2 or 3.")
            subprocess.run(["xdotool", "click", "--repeat", "2", "--delay", "100", str(button)] if kind == "double_click" else ["xdotool", "click", str(button)], env=env, check=True, timeout=8, stdout=subprocess.DEVNULL)
    elif kind == "scroll":
        amount = data.get("amount", 1)
        if isinstance(amount, bool) or not isinstance(amount, int) or amount == 0 or abs(amount) > 12:
            raise Failure("invalid_input", "Scroll between one and twelve steps.")
        subprocess.run(["xdotool", "click", "--repeat", str(abs(amount)), "4" if amount < 0 else "5"], env=env, check=True, timeout=8, stdout=subprocess.DEVNULL)
    elif kind == "type":
        text = data.get("text", "")
        if not isinstance(text, str) or len(text.encode()) > 32000:
            raise Failure("invalid_input", "Type up to 32000 bytes at once.")
        subprocess.run(["xclip", "-selection", "clipboard"], input=text.encode(), env=env, check=True, timeout=8, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["xdotool", "key", "--clearmodifiers", "ctrl+v"], env=env, check=True, timeout=8, stdout=subprocess.DEVNULL)
    elif kind == "key":
        key = data.get("key", "")
        if not isinstance(key, str) or not re.fullmatch(r"[A-Za-z0-9_+]{1,100}", key):
            raise Failure("invalid_input", "Use a named key or key chord.")
        subprocess.run(["xdotool", "key", "--clearmodifiers", key], env=env, check=True, timeout=8, stdout=subprocess.DEVNULL)
    elif kind == "url":
        url = data.get("url", "")
        if not isinstance(url, str) or len(url) > 8000 or urlparse(url).scheme not in ("http", "https") or not urlparse(url).hostname:
            raise Failure("invalid_input", "Open an HTTP or HTTPS URL.")
        browser = shutil.which("google-chrome") or shutil.which("chromium")
        if not browser:
            raise Failure("unavailable", "This computer has no supported browser.")
        subprocess.Popen([browser, "--user-data-dir=" + str(ROOT / ".browser"), "--new-tab", url], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    else:
        raise Failure("invalid_input", "Unknown computer input action.")
    return {"accepted": True}

def stream_screen():
    # One bounded SSH process captures successive frames; no extra guest port,
    # browser token, shell access or long-lived service is exposed.
    from PIL import Image, ImageGrab, features
    native = features.check_feature("xcb")
    output = sys.stdout.buffer
    output.write(b"WREN-SCREEN/1\n")
    deadline = time.monotonic() + 20
    total = 0
    with tempfile.TemporaryDirectory() as directory:
        target = str(Path(directory) / "screen.png")
        for _ in range(160):
            started = time.monotonic()
            if started >= deadline:
                break
            if native:
                picture = ImageGrab.grab(xdisplay=":99")
            else:
                subprocess.run(["scrot", "--overwrite", target], env=desktop_env(), check=True, timeout=5, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                with Image.open(target) as source:
                    picture = source.convert("RGB")
            if picture.width > 4096 or picture.height > 4096:
                raise Failure("too_large", "Desktop dimensions exceed the screen limit.")
            encoded = io.BytesIO()
            picture.convert("RGB").save(encoded, format="JPEG", quality=82, subsampling=0)
            picture.close()
            frame = encoded.getvalue()
            total += len(frame)
            if len(frame) > 2 << 20 or total > 64 << 20:
                raise Failure("too_large", "Desktop feed exceeds its size limit.")
            output.write(struct.pack(">I", len(frame)))
            output.write(frame)
            output.flush()
            time.sleep(max(0, 0.125 - (time.monotonic() - started)))
    output.write(struct.pack(">I", 0))
    output.flush()


def stat_is_regular(mode):
    import stat
    return stat.S_ISREG(mode)


def file_version(stat):
    fields = (stat.st_dev, stat.st_ino, stat.st_size, stat.st_mtime_ns, stat.st_ctime_ns)
    return hashlib.sha256("|".join(str(v) for v in fields).encode()).hexdigest()[:32]


def job_logs(data):
    identifier = data.get("id")
    path = job_path(identifier)
    stream = data.get("stream", "combined") or "combined"
    if stream not in ("combined", "stdout", "stderr"):
        raise Failure("invalid_request", "Choose the combined, stdout or stderr stream.")
    offset = data.get("offset", 0)
    limit = data.get("limit", 65536)
    if isinstance(offset, bool) or not isinstance(offset, int) or offset < 0:
        raise Failure("invalid_request", "Invalid log offset.")
    if isinstance(limit, bool) or not isinstance(limit, int) or limit < 1 or limit > 262144:
        raise Failure("invalid_request", "A log read is limited to 262144 bytes.")
    state = job_state(identifier)
    name = {"combined": "output.log", "stdout": "stdout.log", "stderr": "stderr.log"}[stream]
    target = path / name
    if not target.exists():
        if stream != "combined" and (path / "output.log").exists():
            raise Failure("invalid_request", "Separate stream logs are not recorded for this job.")
        data_out, size = b"", 0
        if offset > 0:
            raise Failure("invalid_request", "Offset exceeds the recorded log.")
    else:
        # Bounded read through open+fstat+seek so a concurrently growing file
        # keeps a consistent view for this response.
        with target.open("rb") as stream_file:
            before = os.fstat(stream_file.fileno())
            size = before.st_size
            if offset > size:
                raise Failure("invalid_request", "Offset exceeds the recorded log.")
            stream_file.seek(offset)
            # Append is normal: this response ends at the captured size.
            count = min(limit, size - offset)
            data_out = stream_file.read(count)
            after = os.fstat(stream_file.fileno())
            if len(data_out) != count or after.st_size < size:
                raise Failure("conflict", "The log changed while it was being read.")
    next_offset = offset + len(data_out)
    result = {"jobId": identifier, "stream": stream,
              "dataBase64": base64.b64encode(data_out).decode(),
              "offset": offset, "nextOffset": next_offset,
              "eof": next_offset >= size,
              "status": state.get("status", "unknown")}
    if "exitCode" in state:
        result["exitCode"] = state["exitCode"]
    result["logTruncated"] = bool(state.get("logTruncated") or state.get("outputTruncated"))
    return result


UPLOAD_ID = re.compile(r"^upl_[A-Za-z0-9_-]{8,32}$")


def upload_path(identifier, create=False):
    if not isinstance(identifier, str) or not UPLOAD_ID.fullmatch(identifier):
        raise Failure("invalid_file", "Invalid upload ID.")
    base = UPLOADS / identifier
    if create:
        base.mkdir(parents=True, exist_ok=True)
    return base


def upload_state(identifier):
    base = upload_path(identifier)
    try:
        meta = json.loads((base / "meta.json").read_text())
        if meta.get("complete"):
            return meta, meta["size"]
        size = (base / "payload").stat().st_size if (base / "payload").exists() else 0
        return meta, size
    except (FileNotFoundError, json.JSONDecodeError):
        raise Failure("not_found", "The requested upload was not found.")


def upload_receipt(identifier, meta, received, complete=False):
    result = {"id": identifier, "path": meta["path"], "size": meta["size"],
              "offset": received, "complete": complete}
    digest = meta.get("actualSha256") if complete else meta.get("sha256")
    if digest is not None:
        result["sha256"] = digest
    return result


def upload_sweep():
    if not UPLOADS.is_dir():
        return
    deadline = time.time() - UPLOAD_TTL
    for entry in UPLOADS.iterdir():
        if not entry.is_dir() or entry.name == "registry.lock":
            continue
        lock_path = entry / "lock"
        lock = None
        try:
            lock = lock_path.open("a")
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except (OSError, IOError):
            if lock is not None:
                lock.close()
            continue
        try:
            meta = json.loads((entry / "meta.json").read_text())
            activity = meta.get("updatedUnix") or meta.get("createdUnix") or 0
            if activity < deadline:
                shutil.rmtree(entry)
        except (FileNotFoundError, json.JSONDecodeError):
            # Unguarded/partial metadata: never delete in a race; leave the entry.
            pass
        finally:
            try:
                fcntl.flock(lock, fcntl.LOCK_UN)
            except OSError:
                pass
            lock.close()


def begin_upload(data):
    if not SANDBOX_MARKER.exists():
        raise Failure("invalid_request", "Chunked uploads require the sandbox helper.")
    target = user_workspace_path(data.get("path", ""), False)
    size = data.get("size")
    if isinstance(size, bool) or not isinstance(size, int) or size < 0 or size > UPLOAD_MAX:
        raise Failure("invalid_file", "An upload must declare 0-4GiB bytes.")
    digest = data.get("sha256")
    if digest is not None and (not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest)):
        raise Failure("invalid_file", "An upload digest must be lowercase 64-hex.")
    UPLOADS.mkdir(parents=True, exist_ok=True)
    identifier = data.get("id")
    if not isinstance(identifier, str) or not UPLOAD_ID.fullmatch(identifier):
        raise Failure("invalid_file", "Invalid upload ID.")
    meta = {"id": identifier, "path": data.get("path"), "size": size, "sha256": digest, "createdAt": now(), "createdUnix": time.time(), "updatedUnix": time.time()}
    base = upload_path(identifier)
    with (UPLOADS / "registry.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        upload_sweep()
        if base.exists():
            with (base / "lock").open("a") as upload_lock:
                fcntl.flock(upload_lock, fcntl.LOCK_EX)
                prior, received = upload_state(identifier)
                if prior.get("path") == meta["path"] and prior.get("size") == meta["size"] and prior.get("sha256") == meta["sha256"]:
                    prior["updatedUnix"] = time.time()
                    save(base / "meta.json", prior)
                    return upload_receipt(identifier, prior, received, complete=prior.get("complete", False))
                raise Failure("conflict", "Upload ID already belongs to different parameters.")
        pending = [e for e in UPLOADS.iterdir() if e.is_dir() and e.name != "registry.lock" and (e / "meta.json").exists() and not json.loads((e / "meta.json").read_text()).get("complete")] if UPLOADS.is_dir() else []
        if len(pending) >= UPLOAD_LIMIT:
            raise Failure("conflict", "Too many incomplete uploads; finish or abort one first.")
        base.mkdir(parents=True)
        base.chmod(0o700)
        save(base / "meta.json", meta)
        (base / "payload").touch()
        fcntl.flock(lock, fcntl.LOCK_UN)
    return upload_receipt(identifier, meta, 0)


def upload_chunk(data):
    identifier = data.get("id")
    base = upload_path(identifier)
    meta, received = upload_state(identifier)
    offset = data.get("offset")
    raw_b64 = data.get("data", "")
    try:
        chunk = base64.b64decode(raw_b64, validate=True)
    except (ValueError, TypeError):
        raise Failure("invalid_file", "Invalid chunk data.")
    if len(chunk) > UPLOAD_CHUNK:
        raise Failure("too_large", "An upload chunk is limited to 4MiB.")
    if isinstance(offset, bool) or not isinstance(offset, int) or offset < 0:
        raise Failure("invalid_file", "Invalid chunk offset.")
    if meta.get("complete"):
        raise Failure("conflict", "This upload is already complete.")
    lock_path = base / "lock"
    with lock_path.open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        meta, received = upload_state(identifier)
        if meta.get("complete"):
            raise Failure("conflict", "This upload is already complete.")
        if offset < received:
            with (base / "payload").open("rb") as payload:
                payload.seek(offset)
                existing = payload.read(len(chunk))
            if existing == chunk:
                meta["updatedUnix"] = time.time()
                save(base / "meta.json", meta)
                fcntl.flock(lock, fcntl.LOCK_UN)
                return upload_receipt(identifier, meta, received)
            raise Failure("conflict", "Chunk bytes conflict with the stored upload.")
        if offset > received:
            raise Failure("conflict", "Upload chunks must be sequential without gaps.")
        if received + len(chunk) > meta["size"]:
            raise Failure("invalid_file", "Chunk overruns the declared upload size.")
        with (base / "payload").open("ab") as payload:
            payload.write(chunk)
            payload.flush()
            os.fsync(payload.fileno())
        received += len(chunk)
        meta["updatedAt"] = now()
        meta["updatedUnix"] = time.time()
        save(base / "meta.json", meta)
        fcntl.flock(lock, fcntl.LOCK_UN)
    return upload_receipt(identifier, meta, received)


def complete_upload(data):
    identifier = data.get("id")
    base = upload_path(identifier)
    meta, received = upload_state(identifier)
    if meta.get("complete"):
        return upload_receipt(identifier, meta, meta["size"], complete=True)
    lock_path = base / "lock"
    with lock_path.open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        meta, received = upload_state(identifier)
        if meta.get("complete"):
            return upload_receipt(identifier, meta, meta["size"], complete=True)
        if received != meta["size"]:
            raise Failure("conflict", "The upload is incomplete.")
        target = user_workspace_path(meta["path"], False)
        target.parent.mkdir(parents=True, exist_ok=True)
        digest = hashlib.sha256()
        temporary = None
        try:
            fd, temporary = tempfile.mkstemp(dir=str(target.parent))
            with os.fdopen(fd, "wb") as out, (base / "payload").open("rb") as payload:
                while chunk := payload.read(1024 * 1024):
                    digest.update(chunk)
                    out.write(chunk)
                out.flush()
                os.fsync(out.fileno())
            actual = digest.hexdigest()
            if meta.get("sha256") is not None and actual != meta["sha256"]:
                raise Failure("conflict", "The uploaded bytes do not match the declared digest.")
            os.replace(temporary, target)
            temporary = None
        finally:
            if temporary and os.path.exists(temporary):
                os.unlink(temporary)
        meta["complete"] = True
        meta["actualSha256"] = actual
        meta["updatedAt"] = now()
        meta["updatedUnix"] = time.time()
        save(base / "meta.json", meta)
        (base / "payload").unlink(missing_ok=True)
        fcntl.flock(lock, fcntl.LOCK_UN)
    return upload_receipt(identifier, meta, meta["size"], complete=True)


def abort_upload(data):
    identifier = data.get("id")
    base = upload_path(identifier)
    if not UPLOADS.is_dir():
        return {"aborted": True}
    with (UPLOADS / "registry.lock").open("a") as registry:
        fcntl.flock(registry, fcntl.LOCK_EX)
        if not base.exists():
            fcntl.flock(registry, fcntl.LOCK_UN)
            return {"aborted": True}
        lock_path = base / "lock"
        with lock_path.open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            try:
                if base.exists():
                    shutil.rmtree(base)
            finally:
                fcntl.flock(lock, fcntl.LOCK_UN)
        fcntl.flock(registry, fcntl.LOCK_UN)
    return {"aborted": True}


def resource_metrics():
    proc = Path(os.environ.get("CANTER_COMPUTER_PROC", "/proc"))
    values = {}
    for line in (proc / "meminfo").read_text(encoding="ascii").splitlines():
        key, tail = line.split(":", 1)
        fields = tail.split()
        if len(fields) == 2 and fields[1] == "kB":
            values[key] = int(fields[0]) * 1024
    total = values["MemTotal"]
    available = values["MemAvailable"]
    if total <= 0 or not 0 <= available <= total:
        raise Failure("metrics_unavailable", "Memory metrics are unavailable.")
    loads = [float(v) for v in (proc / "loadavg").read_text().split()[:3]]
    uptime = float((proc / "uptime").read_text().split()[0])
    if len(loads) != 3 or not all(math.isfinite(v) and v >= 0 for v in loads + [uptime]):
        raise Failure("metrics_unavailable", "System metrics are unavailable.")
    disk = os.statvfs(ROOT)
    unit = disk.f_frsize
    cpus = len(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else os.cpu_count()
    return {
        "sampledAt": now(),
        "cpu": {"count": cpus, "load1": loads[0], "load5": loads[1], "load15": loads[2]},
        "memory": {"totalBytes": total, "availableBytes": available, "usedBytes": total - available},
        "disk": {"totalBytes": disk.f_blocks * unit, "usedBytes": (disk.f_blocks - disk.f_bfree) * unit,
                 "availableBytes": disk.f_bavail * unit},
        "uptimeSeconds": uptime,
    }


def operation(name, data):
    data = data or {}
    if name == "probe":
        desktop = False
        if shutil.which("xdotool"):
            desktop = subprocess.run(["xdotool", "getdisplaygeometry"], env=desktop_env(), timeout=5, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
        return {"version": "wren-computer-v1", "inputProtocol": 2, "workspace": "/workspace", "desktop": desktop, "browser": bool(shutil.which("google-chrome") or shutil.which("chromium")), "persistentFiles": ROOT.is_dir(), "durableJobs": True, "workflowProtocol": 1}
    if name == "start":
        return start_job(data)
    if name == "job":
        return inspect_job(data.get("id"))
    if name == "stop":
        return stop_job(data.get("id"))
    if name == "job_logs":
        return job_logs(data)
    if name == "begin_upload":
        return begin_upload(data)
    if name == "upload_chunk":
        return upload_chunk(data)
    if name == "complete_upload":
        return complete_upload(data)
    if name == "abort_upload":
        return abort_upload(data)
    if name == "metrics":
        return resource_metrics()
    if name == "write":
        path = (user_workspace_path if data.get("userFiles") else workspace_path)(data.get("path", ""), False)
        try:
            raw = base64.b64decode(data.get("data", ""), validate=True)
        except (ValueError, TypeError):
            raise Failure("invalid_file", "Invalid file data.")
        if len(raw) > MAX_FILE:
            raise Failure("too_large", "A computer file upload is limited to 8 MiB.")
        path.parent.mkdir(parents=True, exist_ok=True)
        fd, temporary = tempfile.mkstemp(dir=path.parent)
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(raw)
            os.replace(temporary, path)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
        return {"path": "/workspace/" + str(path.relative_to(ROOT)), "size": len(raw), "data": "", "offset": 0, "nextOffset": len(raw), "eof": True}
    if name == "read":
        path = (user_workspace_path if data.get("userFiles") else workspace_path)(data.get("path", ""), False)
        offset, limit = data.get("offset", 0), data.get("limit", MAX_FILE) or MAX_FILE
        if isinstance(offset, bool) or not isinstance(offset, int) or offset < 0 or isinstance(limit, bool) or not isinstance(limit, int) or limit < 1 or limit > MAX_FILE:
            raise Failure("invalid_file", "Invalid file range.")
        fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as stream:
            before = os.fstat(stream.fileno())
            if not stat_is_regular(before.st_mode):
                raise Failure("invalid_file", "Choose a regular file inside /workspace.")
            version = file_version(before)
            expected = data.get("version")
            if expected is not None and expected != version:
                raise Failure("conflict", "The file changed while it was being read.")
            stream.seek(offset)
            raw = stream.read(limit)
            after = os.fstat(stream.fileno())
            if file_version(after) != version:
                raise Failure("conflict", "The file changed while it was being read.")
            size = after.st_size
        return {"path": "/workspace/" + str(path.relative_to(ROOT)), "data": base64.b64encode(raw).decode(), "size": size, "offset": offset, "nextOffset": offset + len(raw), "eof": offset + len(raw) >= size, "version": version}
    if name == "list":
        user_files = bool(data.get("userFiles"))
        directory = (user_workspace_path if user_files else workspace_path)(data.get("path", ""))
        entries = []
        for path in sorted(directory.iterdir(), key=lambda item: item.name):
            if user_files and directory == ROOT and path.name in PRIVATE_DIRS:
                continue
            if len(entries) >= 500:
                raise Failure("too_many_files", "Choose a smaller folder; this directory has over 500 entries.")
            if path.is_symlink():
                entries.append({"name": path.name, "directory": False, "size": 0, "symlink": True})
                continue
            resolved = workspace_path("/workspace/" + str(path.relative_to(ROOT)))
            entries.append({"name": path.name, "directory": resolved.is_dir(), "size": resolved.stat().st_size})
        return entries
    if name == "screenshot":
        with tempfile.TemporaryDirectory() as directory:
            target = str(Path(directory) / "screen.png")
            subprocess.run(["scrot", "--overwrite", target], env=desktop_env(), check=True, timeout=10, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            raw = Path(target).read_bytes()
            if len(raw) > MAX_FILE:
                raise Failure("too_large", "The screenshot exceeds 8 MiB.")
            return {"data": base64.b64encode(raw).decode()}
    if name == "input":
        return desktop_input(data)
    raise Failure("invalid_operation", "Unknown computer operation.")

def sandbox_gate():
    if not SANDBOX_MARKER.exists():
        raise Failure("forbidden", "This operation is only available on sandboxes.")

def run_pty(header):
    sandbox_gate()
    import pty as ptymod
    import struct
    import termios
    size = header.get("input") or {}
    cols = min(max(int(size.get("cols") or 80), 10), 500)
    rows = min(max(int(size.get("rows") or 24), 10), 500)
    master, slave = os.openpty()
    winsize = struct.pack("HHHH", rows, cols, 0, 0)
    fcntl.ioctl(slave, termios.TIOCSWINSZ, winsize)
    env = dict(os.environ, TERM="xterm-256color", HOME=str(Path.home()), COLUMNS=str(cols), LINES=str(rows))
    cwd = "/workspace" if Path("/workspace").is_dir() else str(Path.home())
    process = subprocess.Popen(["/bin/bash", "-l"], cwd=cwd, env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True, close_fds=True)
    os.close(slave)
    selector = selectors.DefaultSelector()
    selector.register(master, selectors.EVENT_READ, "pty")
    selector.register(sys.stdin.buffer, selectors.EVENT_READ, "stdin")
    stdin_buffer = b""
    try:
        while process.poll() is None:
            for key, _ in selector.select(0.25):
                if key.data == "pty":
                    try:
                        chunk = os.read(master, 65536)
                    except OSError:
                        chunk = b""
                    if not chunk:
                        return
                    sys.stdout.buffer.write(chunk)
                    sys.stdout.buffer.flush()
                else:
                    chunk = os.read(sys.stdin.fileno(), 65536)
                    if not chunk:
                        return
                    stdin_buffer += chunk
                    while b"\n" in stdin_buffer:
                        line, stdin_buffer = stdin_buffer.split(b"\n", 1)
                        if not line.strip():
                            continue
                        try:
                            frame = json.loads(line)
                        except Exception:
                            continue
                        if frame.get("t") == "i":
                            try:
                                os.write(master, base64.b64decode(frame.get("d") or ""))
                            except Exception:
                                pass
                        elif frame.get("t") == "r":
                            cols = min(max(int(frame.get("cols") or cols), 10), 500)
                            rows = min(max(int(frame.get("rows") or rows), 10), 500)
                            fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    finally:
        selector.close()
        try:
            os.close(master)
        except OSError:
            pass
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except Exception:
            pass

def run_tcp_relay(header):
    sandbox_gate()
    import socket
    size = header.get("input") or {}
    port = int(size.get("port") or 0)
    if port < 1024 or port > 65535:
        raise Failure("invalid_request", "Preview ports must be between 1024 and 65535.")
    connection = socket.create_connection(("127.0.0.1", port), timeout=10)
    selector = selectors.DefaultSelector()
    selector.register(connection, selectors.EVENT_READ, "socket")
    selector.register(sys.stdin.buffer, selectors.EVENT_READ, "stdin")
    try:
        while True:
            events = selector.select(60)
            if not events:
                continue
            for key, _ in events:
                if key.data == "socket":
                    chunk = connection.recv(65536)
                    if not chunk:
                        return
                    sys.stdout.buffer.write(chunk)
                    sys.stdout.buffer.flush()
                else:
                    chunk = os.read(sys.stdin.fileno(), 65536)
                    if not chunk:
                        try:
                            connection.shutdown(socket.SHUT_WR)
                        except OSError:
                            pass
                        return
                    connection.sendall(chunk)
    finally:
        selector.close()
        connection.close()

def main():
    try:
        # Streaming ops take a single JSON header line, then raw/stdin frames.
        # Read the header byte-wise from the fd so no subsequent frame bytes are
        # ever held in a buffered reader the streaming loop cannot see. The scan
        # stops at the first newline or a small bound: streaming headers are
        # compact single-line JSON, while ordinary multi-MB requests without a
        # newline must not pay a byte-per-syscall cost for the whole body.
        chunks = []
        while True:
            byte = os.read(sys.stdin.fileno(), 1)
            if not byte or byte == b"\n":
                break
            chunks.append(byte)
            # Bounded header probe: newline-free requests exit early with every
            # scanned byte still in head, so the bulk read below reassembles the
            # full request byte-for-byte.
            if len(chunks) >= 65536:
                break
        head = b"".join(chunks)
        header = None
        if head.endswith(b"\n") or len(head) <= 1 << 20:
            try:
                header = json.loads(head)
            except Exception:
                header = None
        if isinstance(header, dict) and header.get("operation") == "sandbox_snapshot":
            try:
                if byte != b"\n" or len(head) > 16384 or header.get("userFiles", False):
                    raise ValueError("invalid private snapshot request")
                request = header.get("input")
                if not isinstance(request, dict):
                    raise ValueError("invalid snapshot input")
                helper = runpy.run_path("/opt/canter/sandbox-snapshot.py", run_name="canter_sandbox_snapshot")
                helper["run_request"](request, sys.stdin.buffer, sys.stdout.buffer)
            except Exception:
                print("Sandbox snapshot failed", file=sys.stderr)
                sys.exit(1)
            return
        if header and header.get("operation") in ("pty", "tcp_relay"):
            try:
                if header.get("operation") == "pty":
                    run_pty(header)
                else:
                    run_tcp_relay(header)
            except Failure as error:
                sys.stdout.buffer.write(("STREAM_ERROR " + error.message + "\n").encode())
            except Exception:
                sys.exit(1)
            return
        raw = head + sys.stdin.buffer.read((12 << 20) + 1 - len(head))
        if len(raw) > 12 << 20:
            raise Failure("too_large", "Computer request exceeds its size limit.")
        request = json.loads(raw)
        if request.get("operation") == "screen_stream":
            # After the stream header, errors terminate the stream. Never append
            # a JSON success/error envelope that could be mistaken for pixels.
            try:
                stream_screen()
            except Exception:
                sys.exit(1)
            return
        result = operation(request.get("operation"), request.get("input"))
        response = {"ok": True, "result": result}
    except Failure as error:
        response = {"ok": False, "error": {"code": error.code, "message": error.message}}
    except FileNotFoundError:
        response = {"ok": False, "error": {"code": "not_found", "message": "The requested computer file, job or application was not found."}}
    except PermissionError:
        response = {"ok": False, "error": {"code": "forbidden", "message": "The computer cannot access this resource."}}
    except (ValueError, TypeError, json.JSONDecodeError, KeyError):
        response = {"ok": False, "error": {"code": "invalid_request", "message": "Invalid computer request."}}
    except Exception:
        response = {"ok": False, "error": {"code": "operation_failed", "message": "The computer operation failed. Inspect its state before retrying."}}
    print(json.dumps(response))

if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--worker":
        worker(sys.argv[2])
    else:
        main()

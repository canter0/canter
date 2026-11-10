"""Canter sandbox REST client.

Mirrors the public sandbox REST surface: listing, creation with a required
caller idempotency key, async jobs with a resumable SSE log feed, chunked
uploads, streamed downloads, lifecycle, saved environments and templates.

The access token may be a string or a zero-argument callable returning a fresh
token — it is resolved per request and never persisted or echoed into errors.
Only the standard library is used (urllib, json, base64, hashlib).
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import http.client
import math
import io
import ipaddress
import json
import posixpath
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable, Generator
from typing import Any, BinaryIO, Optional

__all__ = ["SandboxClient", "CanterAPIError", "SandboxProtocolError"]

_ID = re.compile(r"^[A-Za-z0-9_-]{1,128}$")
_IDEMPOTENCY_KEY = re.compile(r"^[\x20-\x7e]{8,128}$")
_SHA256_HEX = re.compile(r"^[0-9a-f]{64}$")
_MAX_JSON = 4 << 20
_MAX_LOG_CHUNK = 262144
_MAX_FRAME = 1 << 20
_UPLOAD_CHUNK = 1 << 20
_WAIT_PHASES = {"ready", "asleep", "deleted"}
_JOB_STATUSES = {"starting", "running", "completed", "failed", "cancelled", "timed_out", "interrupted"}
_JOB_ACTIVE = {"starting", "running"}


class CanterAPIError(Exception):
    """Non-2xx API response: status and server message, never the token."""

    def __init__(self, status: int, message: str) -> None:
        super().__init__(f"Canter API returned {status}: {message}")
        self.status = status


class SandboxProtocolError(Exception):
    """Fatal protocol/receipt error — never retried by stream_logs."""


def _check_id(label: str, value: Any) -> str:
    if not isinstance(value, str) or not _ID.fullmatch(value):
        raise TypeError(f"invalid {label} for Canter sandbox path")
    return urllib.parse.quote(value, safe="")


def _check_offset(label: str, value: Any) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise TypeError(f"{label} must be a nonnegative integer")
    return value


def _loopback(host: str) -> bool:
    name = host.rstrip(".").lower()
    if name == "localhost" or name.endswith(".localhost"):
        return True
    try:
        return ipaddress.ip_address(name).is_loopback
    except ValueError:
        return False


def _normalize_path(raw: str) -> str:
    target = raw.strip() or "/workspace"
    if not target.startswith("/"):
        target = "/workspace/" + target
    normalized = posixpath.normpath(target)
    return normalized if normalized.startswith("/") else "/" + normalized


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: D102
        return None


class SandboxClient:
    """Workspace-scoped Canter sandbox client."""

    def __init__(
        self,
        base_url: str,
        workspace_id: str,
        access_token: str | Callable[[], str],
        timeout: float = 30.0,
    ) -> None:
        if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or timeout <= 0:
            raise TypeError("timeout must be a finite positive number")
        raw = base_url.strip().rstrip("/")
        parsed = urllib.parse.urlsplit(raw)
        if (
            parsed.scheme not in ("http", "https")
            or not parsed.netloc
            or parsed.username is not None
            or parsed.password is not None
            or parsed.query
            or parsed.fragment
        ):
            raise ValueError(
                "Canter API URL must be an absolute http or https URL "
                "without credentials, query, or fragment"
            )
        if parsed.scheme == "http" and not _loopback(parsed.hostname or ""):
            raise ValueError("Canter API URL must use https outside loopback development")
        self._base_url = raw
        self._workspace = _check_id("workspace ID", workspace_id)
        self._token = access_token if callable(access_token) else (lambda: access_token)
        self._timeout = timeout
        self._opener = urllib.request.build_opener(_NoRedirect)

    def _access_token(self) -> str:
        token = self._token()
        if not isinstance(token, str) or re.fullmatch(r"[\x21-\x7e]+", token) is None:
            raise TypeError("the Canter access token provider must return a nonempty printable token without whitespace")
        return token

    def _base(self) -> str:
        return f"/v1/workspaces/{self._workspace}"

    def _sandbox_path(self, sandbox_id: str, suffix: str) -> str:
        return f"{self._base()}/sandboxes/{_check_id('sandbox ID', sandbox_id)}{suffix}"

    def _open(self, request: urllib.request.Request, timeout: Optional[float] = None) -> Any:
        return self._opener.open(request, timeout=timeout if timeout is not None else self._timeout)

    @staticmethod
    def _bounded_read(response: Any, limit: int = _MAX_JSON) -> bytes:
        try:
            raw = response.read(limit + 1)
        finally:
            response.close()
        if len(raw) > limit:
            raise SandboxProtocolError(f"Canter API response exceeds {limit} bytes")
        return raw

    @staticmethod
    def _raise(status: int, raw: bytes) -> None:
        message = ""
        try:
            parsed = json.loads(raw[:_MAX_JSON])
            if isinstance(parsed, dict):
                detail = parsed.get("error")
                if isinstance(detail, str):
                    message = detail
                elif isinstance(detail, dict) and isinstance(detail.get("message"), str):
                    message = detail["message"]
        except (json.JSONDecodeError, UnicodeDecodeError):
            pass
        raise CanterAPIError(status, message or f"HTTP {status}")

    def _request(self, method: str, path: str, body: Optional[dict] = None) -> Any:
        # Serialize BEFORE resolving the token so a frozen body cannot be
        # mutated while the token provider resolves.
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(
            self._base_url + path,
            data=data,
            method=method,
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self._access_token()}",
                **({"Content-Type": "application/json"} if data else {}),
            },
        )
        try:
            response = self._open(request)
        except urllib.error.HTTPError as err:
            self._raise(err.code, self._bounded_read(err))
        raw = self._bounded_read(response)
        if not raw:
            return None
        try:
            return json.loads(raw)
        except json.JSONDecodeError as err:
            raise SandboxProtocolError(f"Canter API returned malformed JSON: {err}") from err

    def _request_raw(self, method: str, path: str, data: Optional[bytes], content_type: str = "") -> bytes:
        request = urllib.request.Request(
            self._base_url + path,
            data=data,
            method=method,
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self._access_token()}",
                **({"Content-Type": content_type} if content_type else {}),
            },
        )
        try:
            response = self._open(request)
        except urllib.error.HTTPError as err:
            self._raise(err.code, self._bounded_read(err))
        return self._bounded_read(response)

    # -- listing and lifecycle -------------------------------------------------

    def list(self) -> dict:
        result = self._request("GET", f"{self._base()}/sandboxes")
        if not isinstance(result, dict) or not isinstance(result.get("sandboxes"), list):
            raise SandboxProtocolError("invalid sandbox list receipt")
        for item in result["sandboxes"]:
            if not item.get("id") or not item.get("phase"):
                raise SandboxProtocolError("invalid sandbox list receipt")
        return result

    def create(self, input: dict) -> dict:
        key = input.get("idempotencyKey")
        if not isinstance(key, str) or not _IDEMPOTENCY_KEY.fullmatch(key):
            raise TypeError(
                "sandbox create requires an idempotency key of 8-128 printable ASCII characters"
            )
        result = self._request("POST", f"{self._base()}/sandboxes", input)
        if not isinstance(result, dict) or not result.get("id") or not result.get("phase"):
            raise SandboxProtocolError("invalid sandbox create receipt")
        return result

    def get(self, sandbox_id: str) -> dict:
        result = self._request("GET", self._sandbox_path(sandbox_id, ""))
        if not isinstance(result, dict) or result.get("id") != sandbox_id:
            raise SandboxProtocolError("sandbox receipt ID does not match request")
        return result

    def wait(
        self,
        sandbox_id: str,
        phase: str,
        timeout: float = 900.0,
        stop_event: Any = None,
    ) -> dict:
        if phase not in _WAIT_PHASES:
            raise TypeError(f"invalid wait phase {phase!r}; expected ready, asleep, or deleted")
        if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or timeout <= 0:
            raise TypeError("timeout must be a finite positive number")
        if stop_event is not None and stop_event.is_set():
            raise RuntimeError("wait aborted")
        deadline = time.monotonic() + timeout
        last_phase, last_setup, last_job = "unknown", "none", ""
        def wait_context() -> str:
            return f"waiting for sandbox {sandbox_id} phase {phase!r} (last phase {last_phase!r}, setup {last_setup!r}, job {last_job!r})"
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError(f"{wait_context()} timed out")
            # Cap the per-request timeout at the remaining wait budget.
            request = urllib.request.Request(
                self._base_url + self._sandbox_path(sandbox_id, ""),
                method="GET",
                headers={
                    "Accept": "application/json",
                    "Authorization": f"Bearer {self._access_token()}",
                },
            )
            try:
                response = self._open(request, timeout=min(remaining, self._timeout))
            except urllib.error.HTTPError as err:
                self._raise(err.code, self._bounded_read(err))
            except (OSError, urllib.error.URLError) as err:
                raise RuntimeError(f"{wait_context()}: {err}") from err
            try:
                v = json.loads(self._bounded_read(response))
            except json.JSONDecodeError as err:
                raise SandboxProtocolError(f"Canter API returned malformed JSON: {err}") from err
            except (OSError, urllib.error.URLError, http.client.IncompleteRead) as err:
                raise RuntimeError(f"{wait_context()}: {err}") from err
            if not isinstance(v, dict):
                raise SandboxProtocolError("invalid sandbox receipt")
            current = v.get("phase")
            if not isinstance(current, str) or not current:
                raise SandboxProtocolError("sandbox receipt missing phase")
            if v.get("id") != sandbox_id:
                raise SandboxProtocolError("sandbox receipt ID does not match request")
            setup = v.get("setup")
            if setup is None:
                setup = {}
            if not isinstance(setup, dict):
                raise SandboxProtocolError("invalid sandbox setup receipt")
            setup_status = setup.get("status")
            last_phase, last_setup, last_job = current, setup_status or "none", setup.get("jobId") or ""
            if stop_event is not None and stop_event.is_set():
                raise RuntimeError(f"{wait_context()} aborted")
            if setup_status is not None and setup_status not in ("", "pending", "running", "succeeded", "failed"):
                raise SandboxProtocolError(f"sandbox {sandbox_id} reported unrecognized setup status {setup_status!r}")
            if setup_status == "failed" and phase == "ready":
                raise RuntimeError(
                    f"sandbox {sandbox_id} setup failed (job {setup.get('jobId') or 'unknown'}): {setup.get('error', '')}"
                )
            if current == phase and not (phase == "ready" and setup_status in ("pending", "running")):
                return v
            if current in ("failed", "save_failed"):
                raise RuntimeError(
                    f"sandbox {sandbox_id} reached terminal phase {current!r} "
                    f"while waiting for {phase!r}: {v.get('failure', '')}"
                )
            if current in ("deleting", "deleted") and phase != "deleted":
                raise RuntimeError(
                    f"sandbox {sandbox_id} reached terminal phase {current!r} while waiting for {phase!r}"
                )
            if current not in ("queued", "provisioning", "waking", "ready", "sleeping", "asleep", "deleting", "deleted"):
                raise SandboxProtocolError(f"sandbox {sandbox_id} reported unrecognized phase {current!r}")
            if stop_event is not None:
                if stop_event.wait(min(2.0, max(0.0, deadline - time.monotonic()))):
                    raise RuntimeError(f"{wait_context()} aborted")
            else:
                time.sleep(min(2.0, max(0.0, deadline - time.monotonic())))

    # -- jobs ------------------------------------------------------------------

    def start_job(self, sandbox_id: str, input: dict) -> dict:
        result = self._request("POST", self._sandbox_path(sandbox_id, "/jobs"), input)
        if not isinstance(result, dict) or not result.get("jobId") or result.get("status") not in _JOB_STATUSES:
            raise SandboxProtocolError("invalid sandbox job receipt")
        return result

    def execute(self, sandbox_id: str, input: dict) -> dict:
        """Start a job synchronously: the server waits up to 30 seconds and
        returns a possibly-running job. Uses a 35-second HTTP budget; no
        retries — job start is a mutation."""
        data = json.dumps(input).encode()
        request = urllib.request.Request(
            self._base_url + self._sandbox_path(sandbox_id, "/exec"),
            data=data,
            method="POST",
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {self._access_token()}",
                "Content-Type": "application/json",
            },
        )
        try:
            response = self._open(request, timeout=35)
        except urllib.error.HTTPError as err:
            self._raise(err.code, self._bounded_read(err))
        try:
            result = json.loads(self._bounded_read(response))
        except (json.JSONDecodeError, UnicodeDecodeError) as err:
            raise SandboxProtocolError("invalid sandbox exec receipt") from err
        if not isinstance(result, dict) or not result.get("jobId") or result.get("status") not in _JOB_STATUSES:
            raise SandboxProtocolError("invalid sandbox exec receipt")
        _check_id("job ID", result["jobId"])
        return result

    def job(self, sandbox_id: str, job_id: str) -> dict:
        result = self._request(
            "GET", self._sandbox_path(sandbox_id, f"/jobs/{_check_id('job ID', job_id)}")
        )
        if result.get("jobId") != job_id or result.get("status") not in _JOB_STATUSES:
            raise SandboxProtocolError("invalid sandbox job receipt")
        return result

    def stop_job(self, sandbox_id: str, job_id: str) -> dict:
        result = self._request(
            "POST",
            self._sandbox_path(sandbox_id, f"/jobs/{_check_id('job ID', job_id)}/stop"),
            {},
        )
        if result.get("jobId") != job_id or result.get("status") not in _JOB_STATUSES:
            raise SandboxProtocolError("invalid sandbox job receipt")
        return result

    def stream_logs(
        self,
        sandbox_id: str,
        job_id: str,
        stream: str = "combined",
        offset: int = 0,
        stop_event: Any = None,
    ) -> Generator[dict, None, None]:
        """Yield job log events; `data` is decoded bytes.

        Reconnects at the exact delivered offset after a normal feed end, a
        transport failure, or 429/5xx — at most three consecutive attempts
        without progress. Ends when the job reaches a terminal status after
        all delivered output. Protocol errors are fatal; the token is
        resolved per connection.
        """
        if stream not in ("combined", "stdout", "stderr"):
            raise TypeError(f"invalid sandbox log stream {stream!r}")
        delivered = _check_offset("sandbox log offset", offset)
        segment = _check_id("job ID", job_id)
        path = self._sandbox_path(sandbox_id, f"/jobs/{segment}/events")
        no_progress = 0
        last_status = ""
        while True:
            if stop_event is not None and stop_event.is_set():
                raise RuntimeError("stream aborted")
            token = self._access_token()
            query = urllib.parse.urlencode({"stream": stream, "offset": delivered})
            request = urllib.request.Request(
                f"{self._base_url}{path}?{query}",
                method="GET",
                headers={"Accept": "text/event-stream", "Authorization": f"Bearer {token}"},
            )
            feed_start = time.monotonic()
            try:
                response = self._open(request, timeout=35)
            except urllib.error.HTTPError as err:
                body = self._bounded_read(err)
                if err.code != 429 and err.code < 500:
                    self._raise(err.code, body)
                no_progress += 1
                if no_progress > 3:
                    self._raise(err.code, body)
                self._sleep_backoff(no_progress, stop_event)
                continue
            except OSError as err:
                no_progress += 1
                if no_progress > 3:
                    raise RuntimeError(f"sandbox log stream transport failed: {err}") from err
                self._sleep_backoff(no_progress, stop_event)
                continue

            before = delivered
            status_before = last_status
            terminal = False
            saw_heartbeat = False
            transport_error: Optional[BaseException] = None
            try:
                holder = [delivered]
                for event_ in self._consume_events(response, job_id, stream, holder):
                    if stop_event is not None and stop_event.is_set():
                        raise RuntimeError("stream aborted")
                    if event_["event"] == "heartbeat":
                        saw_heartbeat = True
                        continue
                    yield event_
                    delivered = holder[0]
                    if event_["event"] == "status":
                        if event_["status"] != last_status:
                            last_status = event_["status"]
                        if event_["status"] not in _JOB_ACTIVE:
                            terminal = True
                            break
            except SandboxProtocolError:
                raise
            except (OSError, urllib.error.URLError, http.client.IncompleteRead) as err:
                transport_error = err
            finally:
                response.close()
            if terminal:
                return
            if stop_event is not None and stop_event.is_set():
                raise RuntimeError("stream aborted") from transport_error
            progressed = delivered != before or last_status != status_before or (
                saw_heartbeat and time.monotonic() - feed_start >= 1.0
            )
            if progressed:
                no_progress = 0
            else:
                no_progress += 1
                if no_progress > 3:
                    raise RuntimeError(
                        "sandbox log stream made no progress after repeated reconnects"
                    ) from transport_error
            self._sleep_backoff(no_progress, stop_event)

    @staticmethod
    def _sleep_backoff(no_progress: int, stop_event: Any) -> None:
        delay = min(0.2 * max(no_progress, 1), 0.6)
        if stop_event is not None:
            if stop_event.wait(delay):
                raise RuntimeError("stream aborted")
        else:
            time.sleep(delay)

    def _consume_events(
        self, response: Any, job_id: str, stream: str, delivered: list
    ) -> Generator[dict, None, None]:
        buffer = b""
        event = ""
        data_lines: list = []
        heartbeat = False
        frame_size = 0

        def flush() -> Optional[dict]:
            nonlocal event, data_lines
            if not data_lines and not event:
                return None
            frame_event, frame_data = event, "\n".join(data_lines)
            event = ""
            data_lines = []
            if frame_event == "output":
                try:
                    chunk = json.loads(frame_data)
                except json.JSONDecodeError as err:
                    raise SandboxProtocolError(f"invalid sandbox log output frame: {err}") from err
                if not isinstance(chunk, dict):
                    raise SandboxProtocolError("invalid sandbox log output receipt")
                raw_data = chunk.get("dataBase64")
                if not isinstance(raw_data, str) or not raw_data:
                    raise SandboxProtocolError("invalid sandbox log output receipt: empty data")
                try:
                    data = base64.b64decode(raw_data, validate=True)
                except (binascii.Error, ValueError) as err:
                    raise SandboxProtocolError(f"invalid sandbox log output encoding: {err}") from err
                offset, next_offset = chunk.get("offset"), chunk.get("nextOffset")
                if (
                    len(data) > _MAX_LOG_CHUNK
                    or chunk.get("jobId") != job_id
                    or chunk.get("stream") != stream
                    or not isinstance(offset, int) or isinstance(offset, bool)
                    or not isinstance(next_offset, int) or isinstance(next_offset, bool)
                    or offset != delivered[0]
                    or next_offset < offset
                    or next_offset - offset != len(data)
                    or chunk.get("status") not in _JOB_STATUSES
                    or ("exitCode" in chunk and (isinstance(chunk["exitCode"], bool) or not isinstance(chunk["exitCode"], int)))
                ):
                    raise SandboxProtocolError("invalid sandbox log output receipt")
                delivered[0] = next_offset
                return {
                    "event": "output", "jobId": job_id, "stream": stream,
                    "status": chunk["status"], "offset": offset, "nextOffset": next_offset,
                    "data": data, "exitCode": chunk.get("exitCode"),
                    "logTruncated": chunk.get("logTruncated") is True,
                }
            if frame_event == "status":
                try:
                    view = json.loads(frame_data)
                except json.JSONDecodeError as err:
                    raise SandboxProtocolError(f"invalid sandbox log status frame: {err}") from err
                if (
                    not isinstance(view, dict)
                    or view.get("jobId") != job_id
                    or view.get("status") not in _JOB_STATUSES
                    or ("exitCode" in view and (isinstance(view["exitCode"], bool) or not isinstance(view["exitCode"], int)))
                ):
                    raise SandboxProtocolError("invalid sandbox log status receipt")
                return {
                    "event": "status", "jobId": job_id, "stream": "",
                    "status": view["status"], "offset": 0, "nextOffset": 0,
                    "data": b"", "exitCode": view.get("exitCode"),
                    "logTruncated": view.get("logTruncated") is True,
                }
            return None

        while True:
            block = response.read1(65536) if hasattr(response, "read1") else response.read(65536)
            if not block:
                return  # unterminated partial frame is dropped at EOF
            buffer += block
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                frame_size += len(line) + 1
                if frame_size > _MAX_FRAME:
                    raise SandboxProtocolError(
                        f"sandbox job event frame exceeds {_MAX_FRAME} bytes"
                    )
                text = line.decode("utf-8").rstrip("\r")
                if text == "":
                    if heartbeat and not data_lines and not event:
                        yield {"event": "heartbeat"}
                    elif (emitted := flush()) is not None:
                        yield emitted
                    heartbeat = False
                    frame_size = 0
                    event = ""
                    data_lines = []
                elif text.startswith(":"):
                    heartbeat = True
                elif text.startswith("event:"):
                    event = text[6:].strip()
                elif text.startswith("data:"):
                    data_lines.append(text[6:] if text.startswith("data: ") else text[5:])
            if frame_size + len(buffer) > _MAX_FRAME:
                raise SandboxProtocolError(
                    f"sandbox job event frame exceeds {_MAX_FRAME} bytes"
                )

    # -- files -----------------------------------------------------------------

    def upload(self, sandbox_id: str, path: str, source: BinaryIO) -> dict:
        """Upload through begin/chunk/complete; aborts on any failure.

        Hashes and uploads the source from its current position through EOF,
        restoring the original position on every outcome.
        """
        if source is None or not hasattr(source, "seek") or not hasattr(source, "read"):
            raise TypeError("a seekable sandbox upload source is required")
        normalized = _normalize_path(path)
        position = source.tell()
        try:
            source.seek(0, 2)
            size = source.tell() - position
            if size < 0:
                raise ValueError("invalid sandbox upload source bounds")
            source.seek(position)
            hasher = hashlib.sha256()
            remaining = size
            while remaining > 0:
                block = source.read(min(1 << 20, remaining))
                if not block:
                    raise ValueError("sandbox upload source ended early during hashing")
                hasher.update(block)
                remaining -= len(block)
            sha256 = hasher.hexdigest()
            source.seek(position)
            base = self._sandbox_path(sandbox_id, "/uploads")
            begun = self._request(
                "POST", base, {"path": normalized, "size": size, "sha256": sha256}
            )
            # Only a validated server upload ID is ever sent on the abort route.
            upload_id = _check_id("upload ID", (begun or {}).get("id"))

            def abort() -> None:
                try:
                    request = urllib.request.Request(
                        f"{self._base_url}{base}/{upload_id}",
                        method="DELETE",
                        headers={"Authorization": f"Bearer {self._access_token()}"},
                    )
                    with self._open(request, timeout=15):
                        pass
                except Exception:
                    pass

            if (
                begun.get("path") != normalized
                or begun.get("size") != size
                or begun.get("offset") != 0
                or begun.get("complete") is not False
            ):
                abort()
                raise SandboxProtocolError("invalid sandbox upload begin receipt")
            try:
                sent = 0
                while sent < size:
                    piece = source.read(min(_UPLOAD_CHUNK, size - sent))
                    if not piece:
                        raise ValueError("sandbox upload source ended early")
                    receipt = json.loads(
                        self._request_raw(
                            "PATCH",
                            f"{base}/{upload_id}?offset={sent}",
                            piece,
                            "application/octet-stream",
                        )
                    )
                    if (
                        receipt.get("id") != begun["id"]
                        or receipt.get("path") != normalized
                        or receipt.get("size") != size
                        or receipt.get("offset") != sent + len(piece)
                        or receipt.get("complete") is not False
                    ):
                        raise SandboxProtocolError("invalid sandbox upload chunk receipt")
                    sent += len(piece)
                completed = self._request("POST", f"{base}/{upload_id}/complete", {})
                if (
                    completed.get("complete") is not True
                    or completed.get("id") != begun["id"]
                    or completed.get("path") != normalized
                    or completed.get("size") != size
                    or completed.get("offset") != size
                ):
                    raise SandboxProtocolError("invalid sandbox upload completion receipt")
                if completed.get("sha256") and completed["sha256"] != sha256:
                    raise SandboxProtocolError("sandbox upload checksum mismatch")
                return completed
            except Exception:
                abort()
                raise
        finally:
            source.seek(position)

    def download(self, sandbox_id: str, path: str, destination: BinaryIO) -> None:
        """Stream a sandbox file into a writable binary destination; short
        writes on the destination surface as errors, never silent truncation."""
        if destination is None or not hasattr(destination, "write"):
            raise TypeError("a writable sandbox download destination is required")
        query = urllib.parse.urlencode({"path": path})
        request = urllib.request.Request(
            f"{self._base_url}{self._sandbox_path(sandbox_id, '/files/content')}?{query}",
            method="GET",
            headers={"Authorization": f"Bearer {self._access_token()}"},
        )
        try:
            response = self._open(request)
        except urllib.error.HTTPError as err:
            self._raise(err.code, self._bounded_read(err))
        try:
            declared = int(response.headers.get("Content-Length") or -1)
            received = 0
            while True:
                block = response.read(65536)
                if not block:
                    break
                written = 0
                while written < len(block):
                    n = destination.write(block[written:])
                    if isinstance(n, bool) or not isinstance(n, int) or not 0 < n <= len(block) - written:
                        raise OSError("sandbox download destination made no progress")
                    written += n
                received += len(block)
            if declared >= 0 and received != declared:
                raise ValueError(
                    f"sandbox download truncated: received {received} of {declared} bytes"
                )
        finally:
            response.close()

    # -- save / snapshots / templates ------------------------------------------

    def sleep(self, sandbox_id: str) -> None:
        self._request("POST", self._sandbox_path(sandbox_id, "/sleep"), {})

    def wake(self, sandbox_id: str) -> None:
        self._request("POST", self._sandbox_path(sandbox_id, "/wake"), {})

    def delete(self, sandbox_id: str) -> None:
        self._request("DELETE", self._sandbox_path(sandbox_id, ""))

    def save(self, sandbox_id: str, name: str = "", keep: bool = False) -> dict:
        self.sleep(sandbox_id)
        asleep = self.wait(sandbox_id, "asleep")
        snapshot_id = asleep.get("snapshotId")
        if not snapshot_id:
            raise RuntimeError(f"sandbox {sandbox_id} slept without producing a snapshot")
        path = f"{self._base()}/sandbox-snapshots/{_check_id('snapshot ID', snapshot_id)}"
        if not name and not keep:
            return self._request("GET", path)
        body: dict = {}
        if name:
            body["name"] = name
        if keep:
            body["retained"] = True
        return self._request("PATCH", path, body)

    def snapshots(self, source: str = "", before: str = "") -> dict:
        query = {}
        if source:
            query["sourceSandboxId"] = _check_id("source sandbox ID", source)
        if before:
            query["before"] = _check_id("snapshot cursor", before)
        suffix = f"?{urllib.parse.urlencode(query)}" if query else ""
        return self._request("GET", f"{self._base()}/sandbox-snapshots{suffix}")

    def templates(self) -> dict:
        return self._request("GET", f"{self._base()}/sandbox-templates")

    def create_template(self, name: str, snapshot_id: str) -> dict:
        _check_id("snapshot ID", snapshot_id)
        return self._request(
            "POST", f"{self._base()}/sandbox-templates", {"name": name, "snapshotId": snapshot_id}
        )

    def restore(self, sandbox_id: str, snapshot_id: str, generation: int) -> dict:
        _check_id("snapshot ID", snapshot_id)
        if isinstance(generation, bool) or not isinstance(generation, int) or generation < 1:
            raise TypeError("restore generation must be a positive integer")
        result = self._request(
            "POST",
            self._sandbox_path(sandbox_id, "/restore"),
            {"snapshotId": snapshot_id, "expectedGeneration": generation},
        )
        if result.get("id") != sandbox_id:
            raise SandboxProtocolError("sandbox receipt ID does not match request")
        return result

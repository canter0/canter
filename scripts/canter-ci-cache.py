#!/usr/bin/env python3
"""Canter CI runner cache client: bounded transport plus save/restore CLI."""

import datetime
import hashlib
import http.client
import ipaddress
import json
import os
import re
import socket
import ssl
import stat
import sys
import tempfile
import threading
import time
import urllib.parse


class CacheProtocolError(ValueError):
    pass


def _ascii_url(raw):
    return isinstance(raw, str) and bool(raw) and all(0x20 < ord(c) < 0x7F for c in raw)


def _dns_host(host):
    return (
        0 < len(host) <= 253
        and all(
            0 < len(label) <= 63
            and re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", label, re.ASCII)
            for label in host.split(".")
        )
    )


def cache_origin(raw, *, allow_loopback_http=False):
    """Derive the realm locally, before asking GitHub for an ID token."""
    try:
        if not _ascii_url(raw) or "?" in raw or "#" in raw:
            raise ValueError
        u = urllib.parse.urlsplit(raw)
        if not u.netloc or u.username is not None or u.password is not None:
            raise ValueError
        if u.path not in ("", "/") or u.netloc.endswith(":"):
            raise ValueError
        host = u.hostname or ""
        if "%" in host:
            raise ValueError
        try:
            ip = ipaddress.ip_address(host)
        except ValueError:
            ip = None
        if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped is not None:
            ip = ip.ipv4_mapped
        if ip is not None:
            host = str(ip)
        elif not _dns_host(host):
            raise ValueError
        scheme = u.scheme.lower()
        if scheme != "https" and not (
            scheme == "http" and allow_loopback_http and ip is not None and ip.is_loopback
        ):
            raise ValueError
        port = u.port
        if port == 0:
            raise ValueError
        if (scheme == "https" and port == 443) or (scheme == "http" and port == 80):
            port = None
        authority = "[" + host + "]" if ":" in host else host
        if port is not None:
            authority += ":" + str(port)
        origin = scheme + "://" + authority
        digest = hashlib.sha256(
            ("canter-ci-cache-audience-v1\n" + origin + "\n").encode("ascii")
        ).hexdigest()
        return origin, "urn:canter:ci:cache:v1:" + digest
    except (ValueError, UnicodeError):
        raise CacheProtocolError("invalid cache origin") from None


def github_id_token_url(raw, audience):
    """Send the runtime bearer only to GitHub's fixed Actions domain."""
    try:
        if not _ascii_url(raw) or len(raw) > 8192 or "#" in raw:
            raise ValueError
        if not isinstance(audience, str) or re.fullmatch(
            r"urn:canter:ci:cache:v1:[0-9a-f]{64}", audience, re.ASCII
        ) is None:
            raise ValueError
        u = urllib.parse.urlsplit(raw)
        host = u.hostname or ""
        if (
            u.scheme != "https"
            or u.username is not None
            or u.password is not None
            or not _dns_host(host)
            or not host.endswith(".actions.githubusercontent.com")
            or u.port not in (None, 443)
            or u.netloc.endswith(":")
            or not u.path.startswith("/")
        ):
            raise ValueError
        pairs = urllib.parse.parse_qsl(
            u.query, keep_blank_values=True, strict_parsing=True,
            encoding="utf-8", errors="strict",
        )
        if any(key.lower() == "audience" for key, _ in pairs):
            raise ValueError
        pairs.append(("audience", audience))
        return urllib.parse.urlunsplit(
            ("https", host, u.path, urllib.parse.urlencode(pairs), "")
        )
    except (ValueError, UnicodeError):
        raise CacheProtocolError("invalid GitHub identity endpoint") from None


# ---------------------------------------------------------------------------
# Transport: one monotonic operation budget, bounded DNS, verified TLS.

MAX_OPERATION_SECONDS = 120.0
STEP_TIMEOUT_SECONDS = 15.0
MAX_DNS_ADDRESSES = 64
MAX_INFO_BYTES = 16384
MAX_ID_TOKEN_BYTES = 65536
MAX_JWT_BYTES = 16384
MAX_BEARER_BYTES = 16384
CHUNK_BYTES = 65536
MAX_BLOB_BYTES = 536870912
MAX_KEY_BYTES = 512
MAX_VERSION_BYTES = 128
RUNNER_RE = re.compile(r"[A-Za-z0-9_-]{1,64}\Z", re.ASCII)
DEADLINE_RE = re.compile(
    r"(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?Z\Z",
    re.ASCII,
)
HEX64_RE = re.compile(r"[0-9a-f]{64}\Z", re.ASCII)


class _Deadline(Exception):
    pass


class _TransportError(Exception):
    pass


class _Skip(Exception):
    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


class _LocalIO(Exception):
    pass


class _Budget:
    def __init__(self, seconds):
        self.end = time.monotonic() + max(0.001, float(seconds))

    def remaining(self):
        return self.end - time.monotonic()

    def check(self):
        if self.remaining() <= 0:
            raise _Deadline()
        return self.remaining()

    def step(self):
        self.check()
        return min(STEP_TIMEOUT_SECONDS, self.remaining())


class _Watchdog:
    """One daemon thread per operation; kills every registered socket at the
    operation deadline so blocked connect/TLS/read/write cannot outlive it."""

    def __init__(self, budget):
        self._budget = budget
        self._socks = set()
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._dead = False
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    @staticmethod
    def _kill(sock):
        try:
            sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        try:
            sock.close()
        except OSError:
            pass

    def _run(self):
        wait = max(0.0, self._budget.remaining())
        self._stop.wait(wait)
        if self._stop.is_set():
            return
        with self._lock:
            self._dead = True
            socks = list(self._socks)
        for sock in socks:
            self._kill(sock)

    def register(self, sock):
        """Atomically fences expiry: a socket registered at/after the deadline
        is killed immediately and never becomes a live connection."""
        with self._lock:
            if self._dead or self._stop.is_set() or self._budget.remaining() <= 0:
                expired = True
            else:
                expired = False
                self._socks.add(sock)
        if expired:
            self._kill(sock)
            raise _Deadline()

    def unregister(self, sock):
        with self._lock:
            self._socks.discard(sock)

    def close(self):
        self._stop.set()
        self._thread.join()


def _resolve(host, port, budget):
    """Bounded getaddrinfo in a daemon worker; the late worker only resolves
    and never connects or sends credentials."""
    box = {}

    def work():
        try:
            box["addrs"] = socket.getaddrinfo(
                host, port, socket.AF_UNSPEC, socket.SOCK_STREAM
            )
        except OSError as exc:
            box["err"] = exc

    thread = threading.Thread(target=work, daemon=True)
    thread.start()
    thread.join(min(STEP_TIMEOUT_SECONDS, max(0.001, budget.remaining())))
    if thread.is_alive():
        raise _TransportError("dns")
    if "err" in box:
        raise _TransportError("dns")
    seen = set()
    addrs = []
    for fam, _, _, _, sa in box.get("addrs") or ():
        key = (fam, sa)
        if key in seen:
            continue
        seen.add(key)
        addrs.append((fam, sa))
        if len(addrs) >= MAX_DNS_ADDRESSES:
            break
    if not addrs:
        raise _TransportError("dns")
    return addrs


def _tcp_connect(host, port, budget, watchdog):
    for fam, sa in _resolve(host, port, budget):
        budget.check()
        sock = socket.socket(fam, socket.SOCK_STREAM, 0)
        try:
            sock.settimeout(budget.step())
            watchdog.register(sock)
            sock.connect(sa)
            return sock
        except OSError:
            # Connection-level failure: try the next resolved address.
            watchdog.unregister(sock)
            sock.close()
            continue
        except Exception:
            # Budget/deadline or watchdog fence: never retry, never leak.
            watchdog.unregister(sock)
            sock.close()
            raise
    raise _TransportError("connect")


class _Transport:
    """Per-operation transport: shared monotonic budget + socket watchdog."""

    def __init__(self, seconds=MAX_OPERATION_SECONDS, ssl_context=None):
        self.budget = _Budget(seconds)
        self.watchdog = _Watchdog(self.budget)
        self.ssl_context = ssl_context or ssl.create_default_context()

    def close(self):
        self.watchdog.close()

    def request(self, method, url, headers=None, body=None):
        parts = urllib.parse.urlsplit(url)
        host = parts.hostname or ""
        port = parts.port or (443 if parts.scheme == "https" else 80)
        path = parts.path or "/"
        if parts.query:
            path += "?" + parts.query
        timeout = self.budget.step()
        if parts.scheme == "https":
            conn = _HTTPSConnection(host, port, self, timeout)
        elif parts.scheme == "http":
            conn = _HTTPConnection(host, port, self, timeout)
        else:
            raise _TransportError("scheme")
        hdrs = {"Accept-Encoding": "identity"}
        if headers:
            hdrs.update(headers)
        try:
            conn.connect()
            self.budget.check()
            conn.request(method, path, body=body, headers=hdrs)
            # getresponse() auto-closes the connection on will_close before
            # returning; suspending keeps the wire socket registered with the
            # watchdog until _Reply.close finishes reading the body.
            conn._suspend_close = True
            try:
                resp = conn.getresponse()
            finally:
                conn._suspend_close = False
            return _Reply(resp, conn)
        except OSError as exc:
            conn.close()
            raise _TransportError("io") from exc
        except Exception:
            conn.close()
            raise


class _HTTPSConnection(http.client.HTTPSConnection):
    def __init__(self, host, port, transport, timeout):
        super().__init__(
            host, port, timeout=timeout, context=transport.ssl_context
        )
        self._transport = transport
        self._suspend_close = False

    def connect(self):
        raw = _tcp_connect(self.host, self.port, self._transport.budget,
                           self._transport.watchdog)
        try:
            wrapped = self._context.wrap_socket(
                raw, server_hostname=self.host, do_handshake_on_connect=False
            )
        except Exception:
            self._transport.watchdog.unregister(raw)
            raw.close()
            raise
        self._transport.watchdog.unregister(raw)
        try:
            self._transport.watchdog.register(wrapped)
        except Exception:
            wrapped.close()
            raise
        try:
            wrapped.settimeout(self._transport.budget.step())
            wrapped.do_handshake()
        except Exception:
            self._transport.watchdog.unregister(wrapped)
            wrapped.close()
            raise
        self.sock = wrapped

    def close(self):
        if getattr(self, "_suspend_close", False):
            return
        sock = getattr(self, "sock", None)
        if sock is not None:
            self._transport.watchdog.unregister(sock)
        super().close()


class _HTTPConnection(http.client.HTTPConnection):
    """Plain-HTTP sibling used only for literal-IP loopback origins already
    restricted by cache_origin; shares the same bounded DNS/connect path."""

    def __init__(self, host, port, transport, timeout):
        super().__init__(host, port, timeout=timeout)
        self._transport = transport
        self._suspend_close = False

    def connect(self):
        self.sock = _tcp_connect(
            self.host, self.port, self._transport.budget,
            self._transport.watchdog,
        )

    def close(self):
        if getattr(self, "_suspend_close", False):
            return
        sock = getattr(self, "sock", None)
        if sock is not None:
            self._transport.watchdog.unregister(sock)
        super().close()


class _Reply:
    __slots__ = ("status", "_resp", "_conn")

    def __init__(self, resp, conn):
        self.status = resp.status
        self._resp = resp
        self._conn = conn

    @property
    def headers(self):
        return self._resp.getheaders() or []

    def header(self, name):
        values = [v for k, v in self.headers if k.lower() == name.lower()]
        return values[0] if len(values) == 1 else None

    def header_count(self, name):
        return sum(1 for k, _ in self.headers if k.lower() == name.lower())

    def read(self, n):
        try:
            return self._resp.read(n)
        except OSError as exc:
            raise _TransportError("io") from exc

    def close(self):
        try:
            self._resp.close()
        finally:
            self._conn.close()


def _reject_constant(_):
    raise ValueError("non-finite")


def _strict_json(raw, cap):
    if not isinstance(raw, (bytes, bytearray)) or len(raw) > cap:
        raise CacheProtocolError("invalid response")
    try:
        text = bytes(raw).decode("utf-8", "strict")
    except UnicodeDecodeError:
        raise CacheProtocolError("invalid response") from None

    def pairs(items):
        out = {}
        for key, value in items:
            if key in out:
                raise ValueError("duplicate key")
            out[key] = value
        return out

    try:
        value = json.loads(
            text,
            object_pairs_hook=pairs,
            parse_constant=_reject_constant,
            parse_float=_reject_constant,
        )
    except (ValueError, TypeError):
        raise CacheProtocolError("invalid response") from None
    if not isinstance(value, dict):
        raise CacheProtocolError("invalid response")
    return value


def _is_int(value):
    return type(value) is int


def _is_bool(value):
    return type(value) is bool


def _is_text(value, cap):
    if type(value) is not str:
        return False
    try:
        return len(value.encode("utf-8", "strict")) <= cap
    except UnicodeEncodeError:
        return False


def _check_framing(reply):
    """Reject unexpected content encodings and hop-by-hop framing on every
    JSON and binary response; duplicate headers are already counted."""
    if reply.header_count("content-encoding") > 1:
        raise CacheProtocolError("invalid response")
    encoding = reply.header("content-encoding")
    if encoding is not None and encoding.strip().lower() != "identity":
        raise CacheProtocolError("invalid response")
    # TE+CL together is a request-smuggling conflict; TE alone is legal
    # chunked framing that http.client already de-chunks for bounded reads.
    if (
        reply.header_count("transfer-encoding") > 0
        and reply.header_count("content-length") > 0
    ):
        raise CacheProtocolError("invalid response")


def _json_body(reply, cap, budget):
    _check_framing(reply)
    return _strict_json(_read_bounded(reply, cap, budget), cap)


def _read_bounded(reply, cap, budget):
    chunks = []
    total = 0
    while True:
        budget.check()
        want = min(CHUNK_BYTES, cap + 1 - total)
        if want <= 0:
            break
        block = reply.read(want)
        if not block:
            break
        total += len(block)
        chunks.append(block)
        if total > cap:
            raise CacheProtocolError("invalid response")
    return b"".join(chunks)


def _cache_request(transport, method, url, token, body=None):
    headers = {"Authorization": "Bearer " + token}
    if body is not None:
        headers["Content-Type"] = "application/json"
    reply = transport.request(method, url, headers=headers, body=body)
    return reply


def _get_json(transport, method, url, token, body, cap):
    reply = _cache_request(transport, method, url, token, body)
    try:
        if reply.status != 200:
            raise _http_status_error(reply.status)
        return _json_body(reply, cap, transport.budget)
    finally:
        reply.close()


def _require_keys(data, keys):
    if set(data.keys()) != set(keys):
        raise CacheProtocolError("invalid response")


def _check_info(data, expected_audience):
    _require_keys(data, ("schemaVersion", "enabled", "audience", "maxEntryBytes"))
    if (
        not _is_int(data["schemaVersion"])
        or data["schemaVersion"] != 1
        or not _is_bool(data["enabled"])
        or not _is_text(data["audience"], 256)
        or not _is_int(data["maxEntryBytes"])
        or data["maxEntryBytes"] < 0
        or data["maxEntryBytes"] > MAX_BLOB_BYTES
        or (data["enabled"] is True and data["maxEntryBytes"] <= 0)
    ):
        raise CacheProtocolError("invalid response")
    if data["audience"] != expected_audience:
        raise CacheProtocolError("invalid response")
    return data


def _mint_id_token(env, audience, transport):
    raw_url = env.get("ACTIONS_ID_TOKEN_REQUEST_URL")
    bearer = env.get("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
    if (
        type(raw_url) is not str
        or type(bearer) is not str
        or not raw_url
        or not bearer
        or len(bearer.encode("utf-8", "replace")) > MAX_BEARER_BYTES
    ):
        raise _Skip("no_credentials")
    try:
        url = github_id_token_url(raw_url, audience)
    except CacheProtocolError:
        raise _Skip("no_credentials") from None
    reply = transport.request(
        "GET", url, headers={"Authorization": "Bearer " + bearer}
    )
    try:
        if reply.status != 200:
            raise _TransportError("token")
        data = _json_body(reply, MAX_ID_TOKEN_BYTES, transport.budget)
    finally:
        reply.close()
    value = data.get("value")
    if "value" not in data or type(value) is not str:
        raise CacheProtocolError("invalid response")
    try:
        encoded = value.encode("utf-8", "strict")
    except UnicodeEncodeError:
        raise CacheProtocolError("invalid response") from None
    if not encoded or len(encoded) > MAX_JWT_BYTES:
        raise CacheProtocolError("invalid response")
    return value


def _parse_deadline(text):
    """Validate the UTC RFC3339Nano deadline and return its epoch seconds.
    Callers clip it against their own budget; it never extends 120s."""
    match = DEADLINE_RE.match(text) if type(text) is str else None
    if match is None:
        raise CacheProtocolError("invalid response")
    year, month, day, hour, minute, second = (
        int(match.group(i)) for i in range(1, 7)
    )
    frac = match.group(7) or ""
    try:
        base = datetime.datetime(
            year, month, day, hour, minute, second,
            tzinfo=datetime.timezone.utc,
        ).timestamp()
    except (ValueError, OverflowError, OSError):
        raise CacheProtocolError("invalid response") from None
    nanos = int(frac.ljust(9, "0")[:9]) if frac else 0
    return base + nanos / 1e9


def _http_status_error(status):
    if status in (401, 403):
        return _Skip("denied")
    if status == 404:
        return _Skip("miss")
    if status == 409:
        return _Skip("conflict")
    if status == 413:
        return _Skip("too_large")
    if status == 503:
        return _Skip("unavailable")
    return _Skip("unavailable")


def _cache_base(origin):
    return origin + "/v1/ci/cache"


def _info(transport, origin, audience):
    reply = transport.request("GET", _cache_base(origin) + "/info")
    try:
        if reply.status != 200:
            raise _http_status_error(reply.status)
        data = _json_body(reply, MAX_INFO_BYTES, transport.budget)
    finally:
        reply.close()
    return _check_info(data, audience)


def _prepare(env, allow_loopback_http, url):
    """Returns (origin, audience, runner) or raises _Skip/CacheProtocolError."""
    raw = url if url is not None else env.get("CANTER_CI_CACHE_URL")
    if raw is None or raw == "":
        raise _Skip("unconfigured")
    try:
        origin, audience = cache_origin(
            raw, allow_loopback_http=allow_loopback_http
        )
    except CacheProtocolError:
        raise _Skip("invalid_config") from None
    runner = env.get("RUNNER_NAME", "")
    if type(runner) is not str or RUNNER_RE.match(runner) is None:
        raise _Skip("unconfigured")
    return origin, audience, runner


def _lookup(transport, origin, token, runner, key, version):
    body = json.dumps(
        {
            "schemaVersion": 1,
            "runnerName": runner,
            "key": key,
            "cacheVersion": version,
        },
        separators=(",", ":"),
    ).encode("ascii")
    data = _get_json(
        transport, "POST", _cache_base(origin) + "/lookup", token, body,
        MAX_INFO_BYTES,
    )
    if data.get("found") is False and set(data.keys()) == {
        "schemaVersion", "found"
    } and _is_int(data.get("schemaVersion")) and data["schemaVersion"] == 1:
        return None
    _require_keys(data, ("schemaVersion", "found", "objectId", "sizeBytes", "sha256"))
    if (
        not _is_int(data["schemaVersion"])
        or data["schemaVersion"] != 1
        or data["found"] is not True
        or type(data["objectId"]) is not str
        or HEX64_RE.match(data["objectId"]) is None
        or not _is_int(data["sizeBytes"])
        or data["sizeBytes"] <= 0
        or data["sizeBytes"] > MAX_BLOB_BYTES
        or type(data["sha256"]) is not str
        or HEX64_RE.match(data["sha256"]) is None
    ):
        raise CacheProtocolError("invalid response")
    return data


def _reserve(transport, origin, token, runner, key, version, size, sha):
    body = json.dumps(
        {
            "schemaVersion": 1,
            "runnerName": runner,
            "key": key,
            "cacheVersion": version,
            "sizeBytes": size,
            "sha256": sha,
        },
        separators=(",", ":"),
    ).encode("ascii")
    reply = _cache_request(
        transport, "POST", _cache_base(origin) + "/reservations", token, body
    )
    try:
        status = reply.status
        if status not in (200, 201):
            raise _http_status_error(status)
        data = _json_body(reply, MAX_INFO_BYTES, transport.budget)
    finally:
        reply.close()
    if status == 200:
        _require_keys(data, ("schemaVersion", "created"))
        if (
            _is_int(data["schemaVersion"])
            and data["schemaVersion"] == 1
            and data["created"] is False
        ):
            return None
        raise CacheProtocolError("invalid response")
    _require_keys(data, ("schemaVersion", "created", "uploadId", "deadline"))
    if (
        not _is_int(data["schemaVersion"])
        or data["schemaVersion"] != 1
        or data["created"] is not True
        or type(data["uploadId"]) is not str
        or HEX64_RE.match(data["uploadId"]) is None
        or type(data["deadline"]) is not str
    ):
        raise CacheProtocolError("invalid response")
    deadline = _parse_deadline(data["deadline"])
    if deadline <= time.time():
        raise _Skip("expired")
    return data["uploadId"], deadline


def _object_url(origin, object_id, runner):
    query = urllib.parse.quote(runner, safe="")
    return (
        _cache_base(origin)
        + "/objects/"
        + object_id
        + "?runnerName="
        + query
    )


def _put_object(transport, origin, token, runner, object_id, fd_file, size):
    headers = {
        "Content-Type": "application/octet-stream",
        "Content-Length": str(size),
    }
    reply = transport.request(
        "PUT",
        _object_url(origin, object_id, runner),
        headers={"Authorization": "Bearer " + token, **headers},
        body=fd_file,
    )
    try:
        status = reply.status
        if status != 200:
            raise _http_status_error(status)
        data = _json_body(reply, MAX_INFO_BYTES, transport.budget)
    finally:
        reply.close()
    _require_keys(data, ("schemaVersion", "saved"))
    if (
        _is_int(data["schemaVersion"])
        and data["schemaVersion"] == 1
        and data["saved"] is True
    ):
        return True
    if _is_bool(data["saved"]) and data["saved"] is False:
        raise _Skip("conflict")
    raise CacheProtocolError("invalid response")


class _BoundedFile:
    """Read adapter over the SAME already-open file descriptor: never reads
    past the authorized size even if the underlying file grew after hashing,
    and fails if the file ends before the declared size."""

    def __init__(self, fd, size, budget, wall_deadline=None):
        self._fd = fd
        self._remaining = size
        self._budget = budget
        self._wall = wall_deadline

    def read(self, n=-1):
        if self._remaining <= 0:
            return b""
        self._budget.check()
        if self._wall is not None and time.time() >= self._wall:
            raise _Deadline()
        want = self._remaining if n is None or n < 0 else min(n, self._remaining)
        block = os.read(self._fd, want)
        if not block:
            raise _LocalIO()
        self._remaining -= len(block)
        return block


def _hash_and_stat(fd, size, budget):
    digest = hashlib.sha256()
    os.lseek(fd, 0, os.SEEK_SET)
    remaining = size
    while remaining > 0:
        budget.check()
        block = os.read(fd, min(CHUNK_BYTES, remaining))
        if not block:
            raise _LocalIO()
        digest.update(block)
        remaining -= len(block)
    return digest.hexdigest()


def _save_file(path, cap):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError:
        raise _LocalIO() from None
    try:
        st = os.fstat(fd)
        if not stat_isreg(st) or st.st_size < 1:
            raise _LocalIO()
        if st.st_size > cap:
            raise _Skip("too_large")
        return fd, st.st_size
    except Exception:
        os.close(fd)
        raise


def stat_isreg(st):
    return stat.S_ISREG(st.st_mode)


def _run_save(parsed, env, transport, token_provider):
    origin, audience, runner = _prepare(
        env, parsed["allow_loopback_http"], parsed["url"]
    )
    info = _info(transport, origin, audience)
    if info["enabled"] is not True:
        raise _Skip("disabled")
    cap = min(info["maxEntryBytes"], MAX_BLOB_BYTES)
    fd, size = _save_file(parsed["file"], cap)
    try:
        budget = transport.budget
        st_before = os.fstat(fd)
        sha = _hash_and_stat(fd, size, budget)
        st_after = os.fstat(fd)
        if (st_after.st_ino, st_after.st_size, st_after.st_mtime_ns) != (
            st_before.st_ino, st_before.st_size, st_before.st_mtime_ns
        ):
            raise _Skip("local_io")
        token = token_provider(env, audience, transport)
        reserved = _reserve(
            transport, origin, token, runner, parsed["key"],
            parsed["cache_version"], size, sha,
        )
        if reserved is None:
            raise _Skip("duplicate")
        upload_id, deadline = reserved
        # Clip the upload to the earlier of the reservation deadline and the
        # operation budget; a remote deadline never extends the 120s budget.
        wall = min(deadline, time.time() + transport.budget.remaining())
        if time.time() >= wall:
            raise _Skip("expired")
        os.lseek(fd, 0, os.SEEK_SET)
        source = _BoundedFile(fd, size, transport.budget, wall)
        _put_object(
            transport, origin, token, runner, upload_id, source, size,
        )
        return {"saved": True, "sizeBytes": size}
    finally:
        os.close(fd)


def _run_restore(parsed, env, transport, token_provider):
    origin, audience, runner = _prepare(
        env, parsed["allow_loopback_http"], parsed["url"]
    )
    info = _info(transport, origin, audience)
    if info["enabled"] is not True:
        raise _Skip("disabled")
    dest = parsed["output"]
    if os.path.lexists(dest):
        raise _Skip("exists")
    parent = os.path.dirname(os.path.abspath(dest))
    try:
        if not os.path.isdir(parent):
            raise _LocalIO()
    except OSError:
        raise _LocalIO() from None
    token = token_provider(env, audience, transport)
    hit = _lookup(
        transport, origin, token, runner, parsed["key"], parsed["cache_version"]
    )
    if hit is None:
        raise _Skip("miss")
    size = hit["sizeBytes"]
    sha = hit["sha256"]
    object_id = hit["objectId"]
    tmp_fd, tmp_path = -1, None
    published = False
    try:
        tmp_fd, tmp_path = _make_temp(parent)
        reply = transport.request(
            "GET",
            _object_url(origin, object_id, runner),
            headers={"Authorization": "Bearer " + token},
        )
        try:
            _validate_get_headers(reply, size, sha)
            digest = hashlib.sha256()
            written = 0
            while written < size:
                transport.budget.check()
                block = reply.read(min(CHUNK_BYTES, size - written))
                if not block:
                    raise CacheProtocolError("invalid response")
                written += len(block)
                if written > size:
                    raise CacheProtocolError("invalid response")
                digest.update(block)
                _write_all(tmp_fd, block)
            if reply.read(1):
                raise CacheProtocolError("invalid response")
            if digest.hexdigest() != sha:
                raise CacheProtocolError("invalid response")
        finally:
            reply.close()
        transport.budget.check()
        os.fsync(tmp_fd)
        os.close(tmp_fd)
        tmp_fd = -1
        transport.budget.check()
        try:
            os.link(tmp_path, dest)
        except FileExistsError:
            raise _Skip("exists") from None
        except OSError:
            raise _LocalIO() from None
        os.unlink(tmp_path)
        tmp_path = None
        _fsync_dir(parent)
        return {"hit": True, "sizeBytes": size}
    finally:
        if tmp_fd >= 0:
            os.close(tmp_fd)
        if tmp_path is not None:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass


def _make_temp(parent):
    fd, path = tempfile.mkstemp(prefix=".canter-ci-cache-", dir=parent)
    try:
        os.fchmod(fd, 0o600)
    except OSError:
        os.close(fd)
        os.unlink(path)
        raise _LocalIO() from None
    return fd, path


def _write_all(fd, block):
    view = memoryview(block)
    while view:
        try:
            n = os.write(fd, view)
        except OSError:
            raise _LocalIO() from None
        view = view[n:]


def _fsync_dir(path):
    try:
        dfd = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(dfd)
    except OSError:
        pass
    finally:
        os.close(dfd)


def _validate_get_headers(reply, size, sha):
    if reply.status != 200:
        raise _http_status_error(reply.status)
    _check_framing(reply)
    if reply.header_count("content-length") != 1:
        raise CacheProtocolError("invalid response")
    try:
        length = int(reply.header("content-length"))
    except (TypeError, ValueError):
        raise CacheProtocolError("invalid response") from None
    if length != size:
        raise CacheProtocolError("invalid response")
    ctype = reply.header("content-type")
    if ctype is None or ctype.split(";")[0].strip().lower() != "application/octet-stream":
        raise CacheProtocolError("invalid response")
    etag = reply.header("etag")
    if etag != '"' + sha + '"':
        raise CacheProtocolError("invalid response")
    if reply.header("x-canter-cache-sha256") != sha:
        raise CacheProtocolError("invalid response")
    if reply.header_count("etag") != 1 or reply.header_count("x-canter-cache-sha256") != 1:
        raise CacheProtocolError("invalid response")


# ---------------------------------------------------------------------------
# CLI

USAGE = (
    "usage: canter-ci-cache save|restore [--url ORIGIN] "
    "--key KEY --cache-version VERSION "
    "(save: --file PATH | restore: --output PATH) "
    "[--allow-loopback-http]\n"
)


def _parse_args(argv):
    if not argv or argv[0] not in ("save", "restore"):
        return None
    op = argv[0]
    args = {"op": op, "url": None, "key": None, "cache_version": None,
            "file": None, "output": None, "allow_loopback_http": False}
    need = {"save": "--file", "restore": "--output"}[op]
    i = 1
    while i < len(argv):
        arg = argv[i]
        if arg == "--allow-loopback-http":
            args["allow_loopback_http"] = True
            i += 1
            continue
        if arg in ("--url", "--key", "--cache-version", "--file", "--output"):
            if i + 1 >= len(argv) or argv[i + 1].startswith("--"):
                return None
            field = arg[2:].replace("-", "_")
            if field == "cache_version":
                field = "cache_version"
            if field in args and args[field] is not None and field != "op":
                if field == "allow_loopback_http":
                    return None
            key = {"url": "url", "key": "key", "cache_version": "cache_version",
                   "file": "file", "output": "output"}[field]
            if args[key] is not None:
                return None
            args[key] = argv[i + 1]
            i += 2
            continue
        return None
    for required in ("key", "cache_version"):
        if args[required] is None:
            return None
    file_field = "file" if op == "save" else "output"
    other = "output" if op == "save" else "file"
    if args[file_field] is None or args[other] is not None:
        return None
    return args


def _emit(result):
    sys.stdout.write(json.dumps(result, separators=(",", ":")) + "\n")


def _miss_result(op, reason):
    if op == "save":
        return {"schemaVersion": 1, "operation": "save", "saved": False,
                "reason": reason}
    return {"schemaVersion": 1, "operation": "restore", "hit": False,
            "reason": reason}


def _success_result(op, size):
    if op == "save":
        return {"schemaVersion": 1, "operation": "save", "saved": True,
                "sizeBytes": size}
    return {"schemaVersion": 1, "operation": "restore", "hit": True,
            "sizeBytes": size}


def _cli(argv, env, transport_factory, token_provider, seconds):
    parsed = _parse_args(argv)
    if parsed is None:
        sys.stderr.write(USAGE)
        return 2
    op = parsed["op"]
    for field, cap in (("key", MAX_KEY_BYTES), ("cache_version", MAX_VERSION_BYTES)):
        value = parsed[field]
        try:
            raw = value.encode("utf-8", "strict")
        except (UnicodeEncodeError, AttributeError):
            raw = b""
        if (
            not raw
            or len(raw) > cap
            or value != value.strip()
            or any(
                ord(ch) < 0x20 or 0x7F <= ord(ch) <= 0x9F for ch in value
            )
        ):
            sys.stderr.write(USAGE)
            return 2
    if parsed["url"] is not None:
        try:
            cache_origin(
                parsed["url"],
                allow_loopback_http=parsed["allow_loopback_http"],
            )
        except CacheProtocolError:
            sys.stderr.write("canter-ci-cache: invalid configuration\n")
            return 2
    try:
        transport = transport_factory(seconds)
    except Exception:
        transport = None
    try:
        try:
            if transport is None:
                raise _Skip("unavailable")
            runner_fn = _run_save if op == "save" else _run_restore
            result = runner_fn(parsed, env, transport, token_provider)
            _emit(_success_result(op, result.get("sizeBytes", 0)))
            return 0
        except _Skip as skip:
            _emit(_miss_result(op, skip.reason))
            return 0
        except _Deadline:
            _emit(_miss_result(op, "unavailable"))
            return 0
        except _TransportError:
            _emit(_miss_result(op, "unavailable"))
            return 0
        except _LocalIO:
            _emit(_miss_result(op, "local_io"))
            return 0
        except CacheProtocolError:
            _emit(_miss_result(op, "invalid_response"))
            return 0
        except OSError:
            _emit(_miss_result(op, "local_io"))
            return 0
        except Exception:
            _emit(_miss_result(op, "unavailable"))
            return 0
    finally:
        if transport is not None:
            try:
                transport.close()
            except Exception:
                pass


def main(argv=None, env=None, transport_factory=None, token_provider=None,
         operation_seconds=MAX_OPERATION_SECONDS):
    argv = list(sys.argv[1:] if argv is None else argv)
    env = os.environ if env is None else env
    if transport_factory is None:
        transport_factory = lambda seconds: _Transport(seconds)
    if token_provider is None:
        token_provider = _mint_id_token
    if argv == ["--help"] or argv == ["-h"] or argv == ["help"]:
        sys.stdout.write(USAGE)
        return 0
    return _cli(argv, env, transport_factory, token_provider,
                operation_seconds)


if __name__ == "__main__":
    raise SystemExit(main())

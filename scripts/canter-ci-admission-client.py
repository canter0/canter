#!/usr/bin/env python3
"""Authenticated guest bridge: POSTs one private admission request to the
control plane and publishes the validated receipt, exclusively and atomically.

The capability token arrives only through an inherited file descriptor; it is
never read from argv, the environment, or any protocol file, and is never
logged. Stdlib only; reuses the JIT runner's private protocol validators.

POSIX-only: the whole run is bounded by an absolute monotonic deadline
enforced with SIGALRM/ITIMER_REAL, so a slow peer, a dripping response, or a
blocked descriptor cannot outlive --wait-seconds.
"""

import argparse
import http.client
import importlib.util
import json
import os
from pathlib import Path
import re
import select
import stat
import signal
import socket
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from time import monotonic, sleep

_spec = importlib.util.spec_from_file_location(
    'canter_ci_jit_runner',
    Path(__file__).with_name('canter-ci-jit-runner.py'))
jit = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(jit)

TOKEN_MAX_BYTES = 4096
BODY_MAX_BYTES = 16384
RETRY_SECONDS = 0.25
ATTEMPT_TIMEOUT_SECONDS = 5.0
LOOPBACK_HOSTS = frozenset(('localhost', '127.0.0.1', '::1'))


class BridgeError(Exception):
    pass


class BridgeDeadlineError(BridgeError):
    """The absolute overall deadline expired; nothing may be published."""


def _deadline_expired(signum, frame):
    raise BridgeDeadlineError('overall admission deadline exceeded')


def _install_deadline_guard(deadline):
    """Arm SIGALRM for the remaining budget. Fails closed rather than risk a
    missing deadline or clobbering another timer."""
    if threading.current_thread() is not threading.main_thread():
        raise BridgeError('deadline guard requires the main thread')
    if signal.getitimer(signal.ITIMER_REAL) != (0.0, 0.0):
        raise BridgeError('a real-time timer is already active')
    previous = signal.signal(signal.SIGALRM, _deadline_expired)
    remaining = deadline - monotonic()
    signal.setitimer(signal.ITIMER_REAL, remaining if remaining > 0 else 0.001)

    def restore():
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)

    return restore


class _Parser(argparse.ArgumentParser):
    def error(self, message):
        # Never echo raw values, tokens, or URLs back to any log.
        self.print_usage(sys.stderr)
        self.exit(2, 'canter-ci-admission-client: error: invalid arguments\n')


def parse_args(argv):
    parser = _Parser(
        prog='canter-ci-admission-client',
        allow_abbrev=False,
        description='Submit one private CI admission request and publish its receipt.')
    parser.add_argument('--endpoint', required=True,
                        help='control-plane admission URL (https; http only with --allow-loopback-http on loopback)')
    parser.add_argument('--admission-dir', required=True,
                        help='private admission directory containing config.json and request.json')
    parser.add_argument('--token-fd', required=True, type=int,
                        help='inherited file descriptor (>=3) carrying the bearer capability')
    parser.add_argument('--evidence-fd', type=int, default=None,
                        help='optional inherited pipe (>=3) receiving one anonymous evidence frame')
    parser.add_argument('--wait-seconds', type=int, default=30,
                        help='overall monotonic budget in seconds (1-30, default 30)')
    parser.add_argument('--allow-loopback-http', action='store_true',
                        help='permit plain http only to a loopback endpoint (local tests)')
    args = parser.parse_args(argv)
    if args.token_fd < 3:
        parser.error('--token-fd must be >= 3')
    if args.evidence_fd is not None and args.evidence_fd < 3:
        parser.error('--evidence-fd must be >= 3')
    if args.evidence_fd is not None and args.evidence_fd == args.token_fd:
        parser.error('--evidence-fd must differ from --token-fd')
    if not 1 <= args.wait_seconds <= 30:
        parser.error('--wait-seconds must be between 1 and 30')
    return args


def _endpoint_parts(endpoint, allow_loopback):
    try:
        parts = urllib.parse.urlsplit(endpoint)
    except ValueError:
        raise BridgeError('endpoint is invalid') from None
    if parts.username is not None or parts.password is not None:
        raise BridgeError('endpoint must not carry userinfo')
    if parts.query or parts.fragment:
        raise BridgeError('endpoint must not carry query or fragment')
    host = parts.hostname
    if not host:
        raise BridgeError('endpoint host is missing')
    scheme = parts.scheme.lower()
    loopback = host.lower() in LOOPBACK_HOSTS
    if scheme == 'https':
        pass
    elif scheme == 'http' and allow_loopback and loopback:
        pass
    else:
        raise BridgeError('endpoint scheme is not permitted')
    return parts


def _read_token(fd, deadline):
    try:
        os.set_inheritable(fd, False)
    except OSError:
        raise BridgeError('token fd cannot be secured') from None
    chunks = []
    total = 0
    try:
        while True:
            remaining = deadline - monotonic()
            if remaining <= 0:
                raise BridgeError('token read timed out')
            ready, _, _ = select.select([fd], [], [], remaining)
            if not ready:
                raise BridgeError('token read timed out')
            block = os.read(fd, 65536)
            if not block:
                break
            total += len(block)
            if total > TOKEN_MAX_BYTES:
                raise BridgeError('token exceeds size bound')
            chunks.append(block)
    finally:
        try:
            os.close(fd)
        except OSError:
            pass
    raw = b''.join(chunks)
    if not raw:
        raise BridgeError('token is empty')
    try:
        token = raw.decode('utf-8')
    except UnicodeDecodeError:
        raise BridgeError('token is not UTF-8') from None
    if not token or len(token) > TOKEN_MAX_BYTES or any(c.isspace() for c in token):
        raise BridgeError('token is invalid')
    return token


def _strict_response_json(raw):
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        raise BridgeError('response is not UTF-8') from None

    def pairs_hook(pairs):
        out = {}
        for key, value in pairs:
            if key in out:
                raise ValueError('duplicate key')
            out[key] = value
        return out

    def bad_constant(value):
        raise ValueError('invalid constant ' + value)

    try:
        return json.loads(text, object_pairs_hook=pairs_hook,
                          parse_constant=bad_constant)
    except ValueError:
        raise BridgeError('response is not strict JSON') from None


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _opener():
    # No environment proxies and no redirect following: credentials never
    # leave the exact validated endpoint.
    return urllib.request.build_opener(
        urllib.request.ProxyHandler({}), _NoRedirect())


def _post_impl(endpoint, body, token, timeout):
    req = urllib.request.Request(
        endpoint, data=body, method='POST',
        headers={'Content-Type': 'application/json',
                 'Accept': 'application/json',
                 'Authorization': 'Bearer ' + token})
    evidence = []
    try:
        with _opener().open(req, timeout=timeout) as resp:
            status = resp.status
            raw = resp.read(BODY_MAX_BYTES + 1)
            evidence = list(resp.headers.get_all('X-Canter-CI-Evidence') or [])
    except urllib.error.HTTPError as exc:
        status = exc.code
        try:
            raw = exc.read(BODY_MAX_BYTES + 1)
            evidence = list(exc.headers.get_all('X-Canter-CI-Evidence') or [])
        except Exception:
            raw = b''
        finally:
            exc.close()
    except (urllib.error.URLError, OSError, socket.timeout, http.client.HTTPException):
        raise BridgeError('request failed') from None
    if len(raw) > BODY_MAX_BYTES:
        raise BridgeError('response too large')
    return status, raw, evidence


def _post(endpoint, body, token, timeout):
    status, raw, _ = _post_impl(endpoint, body, token, timeout)
    return status, raw


def _post_evidence(endpoint, body, token, timeout):
    return _post_impl(endpoint, body, token, timeout)


EVIDENCE_HEADER = 'X-Canter-CI-Evidence'
EVIDENCE_CAPABILITY_RE = re.compile(
    r'ce1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+')


def _evidence_header_token(values):
    """Exactly one well-formed evidence header or None; never an error."""
    if not isinstance(values, (list, tuple)) or len(values) != 1:
        return None
    value = values[0]
    if (not isinstance(value, str) or not 1 <= len(value) <= 4096
            or not value.isascii() or any(c.isspace() for c in value)
            or not EVIDENCE_CAPABILITY_RE.fullmatch(value)):
        return None
    return value


def _deliver_evidence_frame(fd, token, receipt, deadline):
    """Best-effort: one bounded anonymous pipe frame before the receipt is
    published. Every failure is ignored; a valid job is never denied."""
    if token is None:
        return
    try:
        spec = importlib.util.spec_from_file_location(
            'canter_ci_evidence',
            Path(__file__).resolve().with_name('canter-ci-evidence.py'))
        evidence = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(evidence)
        frame = {
            'token': token,
            'deadlineUnixMs': receipt['executionDeadlineUnixMs'],
            'baseline': evidence.read_vm_counters(),
            'baselineAt': evidence.utc_timestamp(),
        }
        raw = json.dumps(frame, separators=(',', ':')).encode()
        if not raw or len(raw) > evidence.FRAME_MAX_BYTES:
            return
        try:
            os.set_blocking(fd, False)
        except OSError:
            return
        view = memoryview(raw)
        try:
            while view:
                try:
                    view = view[os.write(fd, view):]
                except BlockingIOError:
                    # A saturated pipe drops the frame immediately; optional
                    # telemetry never waits on or consumes the setup budget.
                    return
        except OSError:
            pass
    except BridgeDeadlineError:
        raise
    except Exception:
        pass


def _wait_request(directory, deadline):
    request_path = os.path.join(directory, jit.FILE_REQUEST)
    while True:
        if monotonic() >= deadline:
            raise BridgeDeadlineError('request wait timed out')
        if os.path.lexists(request_path):
            obj = jit._read_private_json(request_path)
            jit._check_request(obj)
            return obj
        sleep(jit.HOOK_POLL_SECONDS)


def _publish_receipt(directory, obj):
    payload = json.dumps(obj, separators=(',', ':')).encode() + b'\n'
    jit._publish_exclusive(directory, jit.FILE_RECEIPT, payload, 0o600)


def _publish_cached_deny(directory, request):
    receipt = dict(request)
    receipt['decision'] = 'deny'
    _publish_receipt(directory, receipt)


def _check_deadline(deadline):
    if monotonic() >= deadline:
        raise BridgeDeadlineError('overall admission deadline exceeded')


def _run_once(args, deadline, sink):
    """One bounded attempt sequence. ``sink`` caches the validated directory
    and request so a terminal failure can publish a matching deny without any
    new file reads."""
    _endpoint_parts(args.endpoint, args.allow_loopback_http)
    config_path = os.path.join(os.path.abspath(args.admission_dir), jit.FILE_CONFIG)
    try:
        config, directory = jit._load_config(config_path)
    except Exception:
        raise BridgeError('admission config is invalid') from None
    jit._check_private_dir(directory)
    sink['directory'] = directory

    token = _read_token(args.token_fd, deadline)

    request = _wait_request(directory, deadline)
    jit._check_request_binding(request, config)
    sink['request'] = request

    body = json.dumps(request, separators=(',', ':')).encode() + b'\n'
    evidence_fd = getattr(args, 'evidence_fd', None)
    if evidence_fd is not None:
        try:
            if not stat.S_ISFIFO(os.fstat(evidence_fd).st_mode):
                raise BridgeError('evidence fd is not a pipe')
        except OSError:
            raise BridgeError('evidence fd is invalid') from None
    try:
        while True:
            _check_deadline(deadline)
            remaining = deadline - monotonic()
            evidence_headers = []
            if evidence_fd is not None:
                status, raw, evidence_headers = _post_evidence(
                    args.endpoint, body, token,
                    min(remaining, ATTEMPT_TIMEOUT_SECONDS))
            else:
                status, raw = _post(args.endpoint, body, token,
                                    min(remaining, ATTEMPT_TIMEOUT_SECONDS))
            _check_deadline(deadline)
            if status in (202, 503):
                if monotonic() + RETRY_SECONDS >= deadline:
                    raise BridgeDeadlineError('admission timed out')
                sleep(RETRY_SECONDS)
                continue
            if status == 200:
                obj = _strict_response_json(raw)
                decision, _ = jit._validate_receipt(obj, request, int(time.time() * 1000))
                if decision != 'allow':
                    raise BridgeError('allow receipt is invalid')
                _check_deadline(deadline)
                if evidence_fd is not None:
                    _deliver_evidence_frame(
                        evidence_fd, _evidence_header_token(evidence_headers),
                        obj, deadline)
                # Optional baseline work must not carry the approval past
                # the original setup deadline.
                _check_deadline(deadline)
                _publish_receipt(directory, obj)
                return 0
            if status == 403:
                obj = _strict_response_json(raw)
                decision, _ = jit._validate_receipt(obj, request, int(time.time() * 1000))
                if decision != 'deny':
                    raise BridgeError('deny receipt is invalid')
                _check_deadline(deadline)
                _publish_receipt(directory, obj)
                return 0
            raise BridgeError('unexpected admission response')
    finally:
        if evidence_fd is not None:
            try:
                os.close(evidence_fd)
            except OSError:
                pass


def run(argv):
    """Public entry for callers/tests: raises on any rejection and never
    publishes a receipt itself beyond a validated allow/deny."""
    args = parse_args(argv)
    deadline = monotonic() + args.wait_seconds
    restore = _install_deadline_guard(deadline)
    try:
        return _run_once(args, deadline, {})
    finally:
        restore()


def main(argv=None):
    try:
        args = parse_args(sys.argv[1:] if argv is None else argv)
    except SystemExit:
        return 2
    deadline = monotonic() + args.wait_seconds
    sink = {}
    try:
        restore = _install_deadline_guard(deadline)
    except BridgeError:
        return 1
    try:
        try:
            return _run_once(args, deadline, sink)
        except Exception:
            # Raw transport or protocol exceptions must never reach stderr.
            # Terminal failure may publish a matching deny only from the
            # already-validated cached request, only before the deadline,
            # and without any new file reads. After the deadline there is no
            # fallback I/O at all; the supervisor fails closed on timeout.
            if monotonic() < deadline and 'request' in sink:
                try:
                    _publish_cached_deny(sink['directory'], sink['request'])
                except Exception:
                    pass
            return 1
    finally:
        restore()


if __name__ == '__main__':
    signal.signal(signal.SIGPIPE, signal.SIG_DFL)
    sys.exit(main())

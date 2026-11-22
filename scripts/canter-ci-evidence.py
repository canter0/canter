#!/usr/bin/env python3
"""Guest-reported CI measurements and workflow-reported JUnit data.

These observations never authorize execution or determine billing. Missing
observations stay missing; CPU is VM-wide execution utilization, normalized
across all allocated CPUs. Network/OOM counters start at the admission baseline.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
from decimal import Decimal, InvalidOperation, ROUND_HALF_UP
from datetime import datetime, timezone
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import select
import signal
import socket
import stat
import struct
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from time import monotonic
import xml.etree.ElementTree as ET

MAX_COUNTER = (1 << 63) - 1
PROC_MAX_BYTES = 65536
JUNIT_MAX_BYTES = 4 << 20
MAX_TESTS = 1000
MAX_XML_DEPTH = 64
MAX_TEST_DURATION_MS = 21600000
METRIC_INTERVAL_SECONDS = 10
REQUEST_MAX_BYTES = 256 << 10
FRAME_MAX_BYTES = 8192
POST_TIMEOUT_SECONDS = 5
LOCAL_READ_TIMEOUT_SECONDS = 5
LOCAL_BACKLOG = 4
LOCAL_WORKERS = 4
LOCAL_REPLY_TIMEOUT_SECONDS = 1
LOCAL_SERVER_POLL_SECONDS = 0.2
CLI_WAIT_SECONDS = 30
REPORT_MAX_FILES = 64
RETRY_SECONDS = 1
SOCKET_NAME = 'evidence.sock'
ACCEPTED = b'\x00'
REJECTED = b'\x01'
RETRY = b'\x02'
_INTERFACE = re.compile(r'^[A-Za-z0-9_.:-]{1,15}$')
_SECONDS = re.compile(r'^[+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$')


class EvidenceError(Exception):
    pass


def _counter(value):
    return type(value) is int and 0 <= value <= MAX_COUNTER


def _read_proc(root, name):
    try:
        with (Path(root) / name).open('rb') as source:
            raw = source.read(PROC_MAX_BYTES + 1)
        if len(raw) <= PROC_MAX_BYTES:
            return raw.decode('ascii')
    except (OSError, UnicodeError):
        pass
    return ''


def read_vm_counters(proc_root='/proc'):
    """Read only host procfs, with independent missing-data handling."""
    out = {}
    try:
        row = next(line.split() for line in _read_proc(proc_root, 'stat').splitlines()
                   if line.split() and line.split()[0] == 'cpu')
        ticks = [int(value) for value in row[1:9]]
        if len(ticks) == 8 and all(_counter(value) for value in ticks):
            # Linux already includes guest/guest_nice in user/nice. Ignore
            # those extra columns; idle, iowait and steal are not execution.
            total = sum(ticks)
            busy = sum(ticks[index] for index in (0, 1, 2, 5, 6))
            if _counter(total) and _counter(busy):
                out.update(cpu_total_ticks=total, cpu_busy_ticks=busy)
    except (StopIteration, ValueError):
        pass
    try:
        memory = {}
        for line in _read_proc(proc_root, 'meminfo').splitlines():
            parts = line.split()
            if len(parts) == 3 and parts[0] in ('MemTotal:', 'MemAvailable:') and parts[2] == 'kB':
                memory[parts[0]] = int(parts[1]) * 1024
        total, available = memory['MemTotal:'], memory['MemAvailable:']
        if _counter(total) and total > 0 and _counter(available) and available <= total:
            out.update(memory_total_bytes=total, memory_available_bytes=available)
    except (KeyError, ValueError):
        pass
    try:
        routes = []
        for line in _read_proc(proc_root, 'net/route').splitlines()[1:]:
            row = line.split()
            if (len(row) >= 8 and row[1] == '00000000' and row[7] == '00000000'
                    and _INTERFACE.fullmatch(row[0]) and int(row[3], 16) & 1):
                metric = int(row[6])
                if metric >= 0:
                    routes.append((metric, row[0]))
        interface = min(routes)[1]
        for line in _read_proc(proc_root, 'net/dev').splitlines():
            name, separator, data = line.partition(':')
            if separator and name.strip() == interface:
                counters = data.split()
                rx, tx = int(counters[0]), int(counters[8])
                if _counter(rx) and _counter(tx):
                    out.update(network_interface=interface,
                               network_rx_bytes=rx, network_tx_bytes=tx)
                break
    except (ValueError, IndexError):
        pass
    try:
        for line in _read_proc(proc_root, 'vmstat').splitlines():
            row = line.split()
            if len(row) == 2 and row[0] == 'oom_kill':
                value = int(row[1])
                if _counter(value):
                    out['oom_kills'] = value
                break
    except ValueError:
        pass
    return out


def make_vm_metric(current, previous, baseline, at):
    """CPU averages the interval; network/OOM are admission-relative totals."""
    out = {'at': at}
    if previous:
        values = [frame.get(key) for frame in (current, previous)
                  for key in ('cpu_total_ticks', 'cpu_busy_ticks')]
        if all(_counter(value) for value in values):
            total, busy, old_total, old_busy = values
            delta_total, delta_busy = total - old_total, busy - old_busy
            if delta_total > 0 and 0 <= delta_busy <= delta_total:
                out['cpuPercent'] = 100.0 * delta_busy / delta_total
    total, available = current.get('memory_total_bytes'), current.get('memory_available_bytes')
    if _counter(total) and total > 0 and _counter(available) and available <= total:
        out.update(memoryBytes=total-available, memoryLimitBytes=total)
    interface = current.get('network_interface')
    if (baseline and isinstance(interface, str) and _INTERFACE.fullmatch(interface)
            and interface == baseline.get('network_interface')):
        for source, target in (('network_rx_bytes', 'networkRxBytes'),
                               ('network_tx_bytes', 'networkTxBytes')):
            now, start = current.get(source), baseline.get(source)
            if _counter(now) and _counter(start) and now >= start:
                out[target] = now - start
    if baseline:
        now, start = current.get('oom_kills'), baseline.get('oom_kills')
        if _counter(now) and _counter(start) and now >= start:
            out['oomKills'] = now - start
    return out


def _duration_ms(value):
    if not isinstance(value, str) or len(value) > 64 or not _SECONDS.fullmatch(value):
        raise EvidenceError('invalid reported test duration')
    try:
        seconds = Decimal(value)
        if not seconds.is_finite() or seconds < 0 or seconds > Decimal(MAX_TEST_DURATION_MS) / 1000:
            raise EvidenceError('invalid reported test duration')
        return int((seconds * 1000).quantize(Decimal('1'), rounding=ROUND_HALF_UP))
    except InvalidOperation:
        raise EvidenceError('invalid reported test duration') from None


def _tag(element):
    return element.tag.rsplit('}', 1)[-1] if isinstance(element.tag, str) else ''


def _reported_text(value, limit, required=False):
    if not isinstance(value, str) or (required and not value) or len(value.encode('utf-8')) > limit:
        raise EvidenceError('invalid reported test identity')
    return value


def parse_junit(raw):
    """Parse explicit workflow output; no filesystem scan or entity loading.

    A missing duration is omitted, not fabricated as zero. IDs include the
    report digest and occurrence, preserving repeated testcase names.
    """
    if not isinstance(raw, bytes) or not raw or len(raw) > JUNIT_MAX_BYTES:
        raise EvidenceError('invalid test report')
    try:
        text = raw.decode('utf-8')
        if '\x00' in text or '<!DOCTYPE' in text.upper() or '<!ENTITY' in text.upper():
            raise EvidenceError('unsupported test report')
        root = ET.fromstring(text)
    except (UnicodeError, ET.ParseError):
        raise EvidenceError('invalid test report') from None
    if _tag(root) not in ('testsuites', 'testsuite', 'testcase'):
        raise EvidenceError('unsupported test report')
    report = hashlib.sha256(raw).hexdigest()
    results = []
    stack = [(root, '', 0)]
    while stack:
        element, suite, depth = stack.pop()
        if depth > MAX_XML_DEPTH:
            raise EvidenceError('test report nesting exceeds limit')
        tag = _tag(element)
        if tag == 'testsuite':
            name = element.get('name', '')
            suite = '/'.join(part for part in (suite, name) if part)
            _reported_text(suite, 1024)
        if tag == 'testcase':
            if len(results) >= MAX_TESTS:
                raise EvidenceError('too many reported tests')
            name = _reported_text(element.get('name'), 4096, required=True)
            classname = element.get('classname', '')
            case_suite = _reported_text('/'.join(part for part in (suite, classname) if part), 1024)
            declared = {}
            for child in element:
                kind = _tag(child)
                if kind in ('error', 'failure', 'skipped') and kind not in declared:
                    declared[kind] = child
            status, detail = 'passed', None
            for kind, label in (('error', 'error'), ('failure', 'failed'), ('skipped', 'skipped')):
                if kind in declared:
                    status, detail = label, declared[kind]
                    break
            if not declared:
                attr = element.get('status', '').lower()
                result = element.get('result', '').lower()
                if attr in ('error',) or result == 'error':
                    status = 'error'
                elif attr in ('failure', 'failed') or result in ('failure', 'failed'):
                    status = 'failed'
                elif attr in ('notrun', 'skipped', 'disabled') or result in ('suppressed', 'skipped'):
                    status = 'skipped'
                elif attr not in ('', 'run', 'passed', 'success', 'completed') or result not in ('', 'completed', 'passed', 'success'):
                    raise EvidenceError('unsupported reported test state')
            identity = json.dumps([report, case_suite, name, len(results)], separators=(',', ':'), ensure_ascii=True)
            message = ''
            if detail is not None:
                message = '\n'.join(part for part in (detail.get('message', ''), ''.join(detail.itertext())) if part)
                if len(message.encode('utf-8')) > 8192:
                    message = message.encode('utf-8')[:8179].decode('utf-8', errors='ignore') + '\n[truncated]'
            row = {'id': hashlib.sha256(identity.encode('ascii')).hexdigest(),
                   'name': name, 'suite': case_suite, 'status': status, 'message': message}
            if 'time' in element.attrib:
                row['durationMs'] = _duration_ms(element.attrib['time'])
            results.append(row)
        elif tag in ('testsuite', 'testsuites'):
            stack.extend((child, suite, depth+1) for child in reversed(list(element)))
    return results


def read_junit_reports(paths):
    """Read explicit regular files; cap aggregate bytes and unique cases."""
    if not isinstance(paths, (list, tuple)) or not 1 <= len(paths) <= REPORT_MAX_FILES:
        raise EvidenceError('invalid test report selection')
    total = 0
    results = {}
    for path in paths:
        fd = None
        try:
            fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
            if not stat.S_ISREG(os.fstat(fd).st_mode):
                raise EvidenceError('test report is not a regular file')
            with os.fdopen(fd, 'rb') as source:
                fd = None
                raw = source.read(JUNIT_MAX_BYTES-total+1)
        except OSError:
            raise EvidenceError('test report cannot be read') from None
        finally:
            if fd is not None:
                os.close(fd)
        total += len(raw)
        if total > JUNIT_MAX_BYTES:
            raise EvidenceError('test reports exceed size limit')
        for row in parse_junit(raw):
            results.setdefault(row['id'], row)
            if len(results) > MAX_TESTS:
                raise EvidenceError('too many reported tests')
    return list(results.values())


def encode_evidence_batch(metrics, tests, batch_id=None):
    """Encode once; all retries must retain these exact bytes and batch ID."""
    if batch_id is None:
        batch_id = secrets.token_hex(32)
    if (not re.fullmatch(r'[0-9a-f]{64}', batch_id)
            or not isinstance(metrics, list) or not isinstance(tests, list)
            or not metrics and not tests):
        raise EvidenceError('invalid evidence batch')
    try:
        raw = json.dumps({'schemaVersion': 1, 'batchId': batch_id,
                          'metrics': metrics, 'tests': tests},
                         ensure_ascii=False, allow_nan=False,
                         separators=(',', ':')).encode('utf-8')
    except (ValueError, TypeError, UnicodeError):
        raise EvidenceError('invalid evidence batch') from None
    if len(raw) > REQUEST_MAX_BYTES:
        raise EvidenceError('evidence batch exceeds size limit')
    return raw


def make_test_batches(tests):
    """Partition without truncating cases or exceeding the exact wire limit."""
    if not isinstance(tests, list) or len(tests) > MAX_TESTS:
        raise EvidenceError('too many reported tests')
    packets = []
    rows = []
    size = 0
    # Prefix length is independent of the random ID's contents.
    def prefix():
        return ('{"schemaVersion":1,"batchId":"' + secrets.token_hex(32)
                + '","metrics":[],"tests":[').encode('ascii')

    head = prefix()
    for test in tests:
        try:
            row = json.dumps(test, ensure_ascii=False, allow_nan=False,
                             separators=(',', ':')).encode('utf-8')
        except (ValueError, TypeError, UnicodeError):
            raise EvidenceError('invalid evidence batch') from None
        extra = len(row) + bool(rows)
        if len(head) + size + extra + 2 > REQUEST_MAX_BYTES and rows:
            packets.append(head + b','.join(rows) + b']}')
            head, rows, size = prefix(), [], 0
            extra = len(row)
        if len(head) + size + extra + 2 > REQUEST_MAX_BYTES:
            raise EvidenceError('reported test exceeds batch size limit')
        rows.append(row)
        size += extra
    if rows:
        packets.append(head + b','.join(rows) + b']}')
    return packets


def utc_timestamp():
    return datetime.now(timezone.utc).isoformat(timespec='milliseconds').replace('+00:00', 'Z')


def iter_vm_metrics(baseline, baseline_at, stop, monotonic_deadline):
    """Advance observed counters even when a downstream best-effort post fails."""
    if stop.is_set() or monotonic() >= monotonic_deadline:
        return
    previous = baseline
    due = monotonic() + METRIC_INTERVAL_SECONDS
    initial = make_vm_metric(baseline, None, baseline, baseline_at)
    if len(initial) > 1:
        yield initial
    while not stop.is_set():
        delay = min(due, monotonic_deadline) - monotonic()
        if delay > 0 and stop.wait(delay):
            return
        if stop.is_set() or monotonic() >= monotonic_deadline:
            return
        current = read_vm_counters()
        metric = make_vm_metric(current, previous, baseline, utc_timestamp())
        previous = current
        if len(metric) > 1:
            yield metric
        due += METRIC_INTERVAL_SECONDS
        # Do not emit a burst of fabricated catch-up points after a slow post.
        if due < monotonic():
            due = monotonic() + METRIC_INTERVAL_SECONDS


EVIDENCE_PATH = '/v1/ci/runner-evidence'
EVIDENCE_CAPABILITY_RE = re.compile(r'ce1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+')
FIXED_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
LOOPBACK_HOSTS = frozenset(('localhost', '127.0.0.1', '::1'))
FRAME_PREFIX_BYTES = 4

_BASELINE_KEYS = frozenset((
    'cpu_total_ticks', 'cpu_busy_ticks', 'memory_total_bytes',
    'memory_available_bytes', 'network_interface', 'network_rx_bytes',
    'network_tx_bytes', 'oom_kills'))
_BASELINE_COUNTER_KEYS = _BASELINE_KEYS - {'network_interface'}
_FRAME_KEYS = frozenset(('token', 'deadlineUnixMs', 'baseline', 'baselineAt'))
_UTC_RE = re.compile(
    r'^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'
    r'(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:[0-9]{2})$')


def _sibling(name):
    spec = importlib.util.spec_from_file_location(
        name, Path(__file__).resolve().with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _bridge():
    return _sibling('canter-ci-admission-client')


def _jit():
    return _bridge().jit


def _child_env():
    env = {key: os.environ[key]
           for key in ('HOME', 'LANG', 'LC_ALL', 'TMPDIR') if key in os.environ}
    env['PATH'] = FIXED_PATH
    return env


def _evidence_url(endpoint):
    parts = urllib.parse.urlsplit(endpoint)
    if not parts.hostname:
        raise EvidenceError('endpoint is invalid')
    return (parts.scheme + '://' + parts.netloc + EVIDENCE_PATH)


def _read_bounded_fd(fd, limit, deadline):
    chunks = []
    total = 0
    while True:
        remaining = deadline - monotonic()
        if remaining <= 0:
            raise EvidenceError('read timed out')
        ready, _, _ = select.select([fd], [], [], remaining)
        if not ready:
            raise EvidenceError('read timed out')
        block = os.read(fd, 65536)
        if not block:
            break
        total += len(block)
        if total > limit:
            raise EvidenceError('input exceeds size bound')
        chunks.append(block)
    return b''.join(chunks)


def _http_post(url, token, raw, timeout):
    req = urllib.request.Request(
        url, data=raw, method='POST',
        headers={'Content-Type': 'application/json',
                 'Accept': 'application/json',
                 'Authorization': 'Bearer ' + token})
    try:
        with _bridge()._opener().open(req, timeout=timeout) as resp:
            status = resp.status
            resp.read(4096)
    except urllib.error.HTTPError as exc:
        status = exc.code
        try:
            exc.read(4096)
        except Exception:
            pass
        finally:
            exc.close()
    except (urllib.error.URLError, OSError, socket.timeout,
            http.client.HTTPException):
        raise EvidenceError('request failed') from None
    return status


def _post_exit(status):
    if status == 204:
        return 0
    if status in (400, 401, 403, 409, 413, 429) or status < 500:
        return 65
    return 75


def _cmd_post(argv):
    parser = argparse.ArgumentParser(
        prog='canter-ci-evidence post', allow_abbrev=False)
    parser.add_argument('--endpoint', required=True)
    parser.add_argument('--token-fd', required=True, type=int)
    parser.add_argument('--allow-loopback-http', action='store_true')
    args = parser.parse_args(argv)
    if args.token_fd < 3:
        return 65
    bridge = _bridge()
    deadline = monotonic() + POST_TIMEOUT_SECONDS
    try:
        restore = bridge._install_deadline_guard(deadline)
    except Exception:
        return 75
    try:
        try:
            parts = bridge._endpoint_parts(args.endpoint,
                                           args.allow_loopback_http)
            if parts.path != EVIDENCE_PATH:
                return 65
            url = (parts.scheme + '://' + parts.netloc + parts.path)
            token = bridge._read_token(args.token_fd, deadline)
            raw = _read_bounded_fd(0, REQUEST_MAX_BYTES + 1, deadline)
        except bridge.BridgeDeadlineError:
            return 75
        except Exception:
            return 65
        if not raw or len(raw) > REQUEST_MAX_BYTES:
            return 65
        try:
            status = _http_post(url, token, raw,
                                max(0.001, deadline - monotonic()))
        except EvidenceError:
            return 75
        except bridge.BridgeDeadlineError:
            return 75
        return _post_exit(status)
    finally:
        restore()


def _valid_frame(frame):
    if not isinstance(frame, dict) or set(frame.keys()) != _FRAME_KEYS:
        return None
    token = frame['token']
    if (not isinstance(token, str) or not 1 <= len(token) <= 4096
            or not token.isascii() or any(c.isspace() for c in token)
            or not EVIDENCE_CAPABILITY_RE.fullmatch(token)):
        return None
    deadline = frame['deadlineUnixMs']
    if type(deadline) is not int or deadline <= 0:
        return None
    baseline = frame['baseline']
    if not isinstance(baseline, dict) or not set(baseline) <= _BASELINE_KEYS:
        return None
    for key, value in baseline.items():
        if key == 'network_interface':
            if not isinstance(value, str) or not _INTERFACE.fullmatch(value):
                return None
        elif (type(value) is not int or not 0 <= value <= MAX_COUNTER):
            return None
    at = frame['baselineAt']
    if not isinstance(at, str) or not _UTC_RE.fullmatch(at):
        return None
    try:
        parsed = datetime.fromisoformat(at.replace('Z', '+00:00'))
    except ValueError:
        return None
    if parsed.tzinfo is None or parsed.utcoffset() != timezone.utc.utcoffset(parsed):
        return None
    return frame


def _decode_frame(raw):
    if not raw or len(raw) > FRAME_MAX_BYTES:
        return None
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        return None

    def pairs_hook(pairs):
        out = {}
        for key, value in pairs:
            if key in out:
                raise ValueError('duplicate key')
            out[key] = value
        return out

    try:
        obj = json.loads(text, object_pairs_hook=pairs_hook,
                         parse_constant=lambda v: (_ for _ in ()).throw(ValueError()))
    except ValueError:
        return None
    return _valid_frame(obj)


class _CombinedStop:
    """Stop view for the root generator: parent stop OR reporter closing."""

    def __init__(self, *events):
        self._events = events

    def is_set(self):
        return any(event.is_set() for event in self._events)

    def wait(self, delay):
        end = monotonic() + delay
        while True:
            if self.is_set():
                return True
            remaining = end - monotonic()
            if remaining <= 0:
                return self.is_set()
            time.sleep(min(0.05, remaining))


class CIReporter:
    """Parent-owned evidence publisher: one private UDS listener plus the
    root sampling generator. The token lives only in memory and anonymous
    pipes; workflow reporters reach it only through the job-private socket."""

    def __init__(self, endpoint, frame, admission_dir, stop,
                 hard_deadline_unix_ms, allow_loopback_http=False):
        self._url = _evidence_url(endpoint)
        self._frame = frame
        self._dir = admission_dir
        self._stop = stop
        self._allow_loopback = allow_loopback_http
        now_wall = time.time()
        now_mono = monotonic()
        wall_ms = min(int(frame['deadlineUnixMs']),
                      int(hard_deadline_unix_ms))
        self._mono_deadline = now_mono + (wall_ms / 1000.0 - now_wall)
        self._post_mutex = threading.Lock()
        self._registry = threading.Lock()
        self._publishers = set()
        self._closing = threading.Event()
        self._listener = None
        self._sock_path = os.path.join(admission_dir, SOCKET_NAME)
        self._sock_dev = None
        self._sock_ino = None
        self._server = None
        self._conns = set()
        self._conns_lock = threading.Lock()

    def _open_listener(self):
        jit = _jit()
        jit._check_private_dir(self._dir)
        if os.path.lexists(self._sock_path):
            raise EvidenceError('evidence socket path exists')
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            sock.bind(self._sock_path)
            os.chmod(self._sock_path, 0o600)
            sock.listen(LOCAL_BACKLOG)
        except Exception:
            sock.close()
            raise
        info = os.lstat(self._sock_path)
        self._sock_dev = info.st_dev
        self._sock_ino = info.st_ino
        self._listener = sock

    def _spawn_post(self, raw):
        token = self._frame['token']
        read_fd, write_fd = os.pipe()
        argv = [sys.executable, '-I', str(Path(__file__).resolve()),
                'post', '--endpoint', self._url,
                '--token-fd', str(read_fd)]
        if self._allow_loopback:
            argv.append('--allow-loopback-http')
        with self._registry:
            if self._closing.is_set() or self._stop.is_set():
                os.close(read_fd)
                os.close(write_fd)
                return None
            try:
                proc = subprocess.Popen(
                    argv, env=_child_env(), stdin=subprocess.PIPE,
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                    pass_fds=(read_fd,), close_fds=True,
                    start_new_session=True)
            except Exception:
                os.close(read_fd)
                os.close(write_fd)
                return None
            os.close(read_fd)
            data = token.encode('ascii')
            try:
                while data:
                    data = data[os.write(write_fd, data):]
            except OSError:
                pass
            try:
                os.close(write_fd)
            except OSError:
                pass
            self._publishers.add(proc)
        return proc

    def _await_post(self, proc, raw):
        try:
            proc.communicate(input=raw, timeout=POST_TIMEOUT_SECONDS)
            code = proc.returncode
        except Exception:
            code = 75
        finally:
            # Every exit path kills and reaps our own child group before the
            # registry slot is released; a live publisher is never orphaned.
            if proc.poll() is None:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError, OSError):
                    pass
            try:
                proc.wait(timeout=2)
            except Exception:
                try:
                    proc.kill()
                    proc.wait(timeout=2)
                except Exception:
                    pass
            with self._registry:
                self._publishers.discard(proc)
        if code == 0:
            return ACCEPTED
        if code == 65:
            return REJECTED
        return RETRY

    def _post_raw(self, raw):
        if not self._post_mutex.acquire(timeout=LOCAL_READ_TIMEOUT_SECONDS):
            return RETRY
        try:
            if self._closing.is_set() or self._stop.is_set():
                return RETRY
            proc = self._spawn_post(raw)
            if proc is None:
                return RETRY
            return self._await_post(proc, raw)
        finally:
            self._post_mutex.release()

    def _post_metric(self, raw):
        if not self._post_mutex.acquire(blocking=False):
            return
        try:
            if self._closing.is_set() or self._stop.is_set():
                return
            proc = self._spawn_post(raw)
            if proc is None:
                return
            self._await_post(proc, raw)
        finally:
            self._post_mutex.release()

    def _recv_exact(self, conn, count, deadline):
        chunks = []
        total = 0
        while total < count:
            remaining = deadline - monotonic()
            if remaining <= 0:
                raise EvidenceError('frame read timed out')
            ready, _, _ = select.select([conn], [], [], remaining)
            if not ready:
                raise EvidenceError('frame read timed out')
            block = conn.recv(count - total)
            if not block:
                raise EvidenceError('frame ended early')
            total += len(block)
            chunks.append(block)
        return b''.join(chunks)

    def _handle_conn(self, conn):
        try:
            conn.settimeout(None)
            deadline = monotonic() + LOCAL_READ_TIMEOUT_SECONDS
            prefix = self._recv_exact(conn, FRAME_PREFIX_BYTES, deadline)
            length = struct.unpack('>I', prefix)[0]
            if not 1 <= length <= REQUEST_MAX_BYTES:
                return
            raw = self._recv_exact(conn, length, deadline)
            reply = self._post_raw(raw)
            try:
                conn.settimeout(LOCAL_REPLY_TIMEOUT_SECONDS)
                conn.sendall(reply)
            except OSError:
                pass
        except (OSError, EvidenceError):
            pass
        finally:
            with self._conns_lock:
                self._conns.discard(conn)
            try:
                conn.close()
            except OSError:
                pass

    def _serve(self):
        listener = self._listener
        slots = threading.BoundedSemaphore(LOCAL_WORKERS)

        def handle(conn):
            try:
                self._handle_conn(conn)
            finally:
                slots.release()

        with ThreadPoolExecutor(max_workers=LOCAL_WORKERS) as workers:
            while not (self._closing.is_set() or self._stop.is_set()):
                try:
                    ready, _, _ = select.select(
                        [listener], [], [], LOCAL_SERVER_POLL_SECONDS)
                except (OSError, ValueError):
                    return
                if not ready:
                    continue
                try:
                    conn, _ = listener.accept()
                except OSError:
                    if self._closing.is_set() or self._stop.is_set():
                        return
                    continue
                if not slots.acquire(blocking=False):
                    conn.close()
                    continue
                with self._conns_lock:
                    if self._closing.is_set() or self._stop.is_set():
                        conn.close()
                        slots.release()
                        return
                    self._conns.add(conn)
                workers.submit(handle, conn)

    def _remove_socket(self):
        if self._sock_dev is None:
            return
        try:
            info = os.lstat(self._sock_path)
        except OSError:
            return
        if (stat.S_ISSOCK(info.st_mode)
                and info.st_dev == self._sock_dev
                and info.st_ino == self._sock_ino):
            try:
                os.unlink(self._sock_path)
            except OSError:
                pass

    def close(self):
        self._closing.set()
        listener = self._listener
        if listener is not None:
            try:
                listener.close()
            except OSError:
                pass
        with self._conns_lock:
            conns = list(self._conns)
        for conn in conns:
            try:
                conn.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                conn.close()
            except OSError:
                pass
        with self._registry:
            procs = list(self._publishers)
        for proc in procs:
            if proc.poll() is None:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError, OSError):
                    pass
        for proc in procs:
            try:
                proc.wait(timeout=2)
            except Exception:
                pass
        self._remove_socket()

    def run(self):
        try:
            try:
                self._open_listener()
            except Exception:
                self._listener = None
            if self._listener is not None:
                self._server = threading.Thread(
                    target=self._serve, daemon=True)
                self._server.start()
            baseline = self._frame['baseline']
            metric_stop = _CombinedStop(self._stop, self._closing)
            for metric in iter_vm_metrics(
                    baseline, self._frame['baselineAt'],
                    metric_stop, self._mono_deadline):
                try:
                    raw = encode_evidence_batch([metric], [])
                except EvidenceError:
                    continue
                self._post_metric(raw)
        finally:
            self.close()
            if self._server is not None:
                self._server.join(timeout=POST_TIMEOUT_SECONDS + 2)


def _connect_socket(path, deadline):
    jit = _jit()
    jit._check_private_dir(os.path.dirname(os.path.abspath(path)))
    info = os.lstat(path)
    if (not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid()
            or info.st_mode & 0o077):
        raise EvidenceError('report socket is not private')
    remaining = deadline - monotonic()
    if remaining <= 0:
        raise EvidenceError('report deadline exceeded')
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    # The server may legitimately hold a packet for mutex+post time before
    # answering, so every blocking phase gets the whole remaining budget;
    # the absolute guard, not a per-op cap, bounds the command.
    conn.settimeout(remaining)
    try:
        conn.connect(path)
        return conn
    except Exception:
        conn.close()
        raise


def _send_packet(conn, raw, deadline):
    conn.settimeout(max(0.001, deadline - monotonic()))
    conn.sendall(struct.pack('>I', len(raw)) + raw)
    conn.settimeout(max(0.001, deadline - monotonic()))
    reply = conn.recv(1)
    if not reply:
        raise EvidenceError('report reply missing')
    return reply


def _junit_run(args, deadline):
    try:
        rows = read_junit_reports(args.files)
    except EvidenceError:
        print('Test report was not accepted.', file=sys.stderr)
        return 1
    if not rows:
        print('No test cases reported.')
        return 0
    try:
        packets = make_test_batches(rows)
    except EvidenceError:
        print('Test report was not accepted.', file=sys.stderr)
        return 1
    path = os.path.join(os.path.abspath(args.admission_dir), SOCKET_NAME)
    for raw in packets:
        while True:
            if monotonic() >= deadline:
                print('Test report was not accepted.', file=sys.stderr)
                return 1
            try:
                conn = _connect_socket(path, deadline)
            except Exception:
                sleep_left = min(RETRY_SECONDS, deadline - monotonic())
                if sleep_left <= 0:
                    print('Test report was not accepted.', file=sys.stderr)
                    return 1
                time.sleep(sleep_left)
                continue
            try:
                reply = _send_packet(conn, raw, deadline)
            except Exception:
                reply = None
            finally:
                conn.close()
            if reply == ACCEPTED:
                break
            if reply == RETRY or reply is None:
                sleep_left = min(RETRY_SECONDS, deadline - monotonic())
                if sleep_left <= 0:
                    print('Test report was not accepted.', file=sys.stderr)
                    return 1
                time.sleep(sleep_left)
                continue
            print('Test report was not accepted.', file=sys.stderr)
            return 1
    print('Test report accepted.')
    return 0


def _cmd_junit(argv):
    parser = argparse.ArgumentParser(
        prog='canter-ci-evidence junit', allow_abbrev=False)
    parser.add_argument('files', nargs='+')
    parser.add_argument('--admission-dir',
                        default='/home/canter-ci/admission')
    args = parser.parse_args(argv)
    bridge = _bridge()
    deadline = monotonic() + CLI_WAIT_SECONDS
    try:
        restore = bridge._install_deadline_guard(deadline)
    except Exception:
        print('Test report was not accepted.', file=sys.stderr)
        return 1
    try:
        return _junit_run(args, deadline)
    except bridge.BridgeDeadlineError:
        print('Test report was not accepted.', file=sys.stderr)
        return 1
    finally:
        restore()


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if argv and argv[0] == 'post':
        return _cmd_post(argv[1:])
    if argv and argv[0] == 'junit':
        return _cmd_junit(argv[1:])
    sys.stderr.write('usage: canter-ci-evidence {post|junit} ...\n')
    return 2


if __name__ == '__main__':
    signal.signal(signal.SIGPIPE, signal.SIG_DFL)
    sys.exit(main())

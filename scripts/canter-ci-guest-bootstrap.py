#!/usr/bin/env python3
"""Guest bootstrap: claim one runner bootstrap grant, then supervise the
JIT-configured job under admission control.

Reads an opaque bootstrap capability only from an inherited descriptor,
exchanges it for the official JIT config plus a scoped admission
capability, and hands both to the existing JIT supervisor in-process. The
bootstrap token is never written to disk, argv, the environment, or logs;
the admission capability reaches the admission bridge only through a
private pipe. Stdlib only; reuses the admission client and JIT runner
modules so their tested protocol validators stay authoritative.
"""
import argparse
import http.client
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import select
import signal
import socket
import stat
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from time import monotonic, sleep

_spec = importlib.util.spec_from_file_location(
    'canter_ci_admission_client',
    Path(__file__).with_name('canter-ci-admission-client.py'))
bridge = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(bridge)
jit = bridge.jit
legacy = jit.legacy

BOOTSTRAP_PATH = '/v1/ci/runner-bootstrap'
ADMISSION_PATH = '/v1/ci/runner-admissions'
BRIDGE_PATH = Path(__file__).resolve().with_name('canter-ci-admission-client.py')
RESPONSE_MAX_BYTES = 128 * 1024
PENDING_RETRY_SECONDS = 1.0
MAXIMUM_SECONDS_MAX = 21600
WAIT_SECONDS_MAX = 120
CAPABILITY_MAX_BYTES = 4096
DENIAL_REASON = 'bootstrap_bridge_failed'

OPERATION_ID_RE = re.compile(r'[A-Za-z0-9_-]{1,128}')
CAPABILITY_RE = re.compile(r'v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+')

RESPONSE_FIELDS = frozenset((
    'version', 'operationId', 'admissionId', 'runnerName', 'githubRunnerId',
    'githubRepositoryId', 'maximumSeconds', 'setupDeadlineUnixMs',
    'hardDeadlineUnixMs', 'encodedJitConfig', 'admissionCapability'))


class BootstrapError(Exception):
    pass


class _Parser(argparse.ArgumentParser):
    def error(self, message):
        # Never echo raw values, tokens, or URLs back to any log.
        self.print_usage(sys.stderr)
        self.exit(2, 'canter-ci-guest-bootstrap: error: invalid arguments\n')


def _positive_int64(text):
    if not isinstance(text, str):
        raise argparse.ArgumentTypeError('expected a decimal integer')
    return jit.int64_arg(text)


def _bounded_seconds(lo, hi):
    def check(text):
        if not isinstance(text, str) or not jit.DECIMAL_RE.fullmatch(text):
            raise argparse.ArgumentTypeError('expected a decimal integer')
        value = int(text)
        if not lo <= value <= hi:
            raise argparse.ArgumentTypeError('value out of range')
        return value
    return check


def _operation_id(text):
    if not isinstance(text, str) or not OPERATION_ID_RE.fullmatch(text):
        raise argparse.ArgumentTypeError('invalid operation id')
    return text


def parse_args(argv):
    parser = _Parser(
        prog='canter-ci-guest-bootstrap',
        allow_abbrev=False,
        description='Claim one CI runner bootstrap grant and supervise its JIT job.')
    parser.add_argument('--endpoint', required=True,
                        help='control-plane bootstrap URL (https; http only with '
                             '--allow-loopback-http on loopback)')
    parser.add_argument('--runner-dir', required=True,
                        help='directory containing an already extracted official runner')
    parser.add_argument('--admission-dir', required=True,
                        help='path for a fresh private admission directory that must not exist')
    parser.add_argument('--token-fd', required=True, type=int,
                        help='inherited file descriptor (>=3) carrying the opaque bootstrap capability')
    parser.add_argument('--operation-id', required=True, type=_operation_id,
                        help='operation id echoed by the bootstrap response')
    parser.add_argument('--admission-id', required=True, type=jit.admission_id_arg,
                        help='64 lowercase hex admission id')
    parser.add_argument('--name', required=True, type=legacy.runner_name_arg,
                        help='runner name: 1-64 characters from [a-zA-Z0-9_-]')
    parser.add_argument('--github-repository-id', required=True, type=_positive_int64,
                        help='decimal GitHub repository id')
    parser.add_argument('--maximum-seconds', required=True,
                        type=_bounded_seconds(1, MAXIMUM_SECONDS_MAX),
                        help='maximum job seconds echoed by the bootstrap response')
    parser.add_argument('--setup-deadline-unix-ms', required=True, type=_positive_int64,
                        help='setup deadline in unix milliseconds echoed by the response')
    parser.add_argument('--hard-deadline-unix-ms', required=True, type=_positive_int64,
                        help='hard deadline in unix milliseconds echoed by the response')
    parser.add_argument('--bootstrap-expires-at', required=True, type=_positive_int64,
                        help='bootstrap capability expiry in unix seconds')
    parser.add_argument('--wait-seconds', type=_bounded_seconds(1, WAIT_SECONDS_MAX),
                        default=120,
                        help='absolute bootstrap budget in seconds (1-120, default 120)')
    parser.add_argument('--allow-loopback-http', action='store_true',
                        help='permit plain http only to a loopback endpoint (local tests)')
    args = parser.parse_args(argv)
    if args.token_fd < 3:
        parser.error('--token-fd must be >= 3')
    return args


def _endpoint_parts(endpoint, allow_loopback):
    parts = bridge._endpoint_parts(endpoint, allow_loopback)
    # urlsplit drops an empty query/fragment, so reject the delimiters
    # themselves to also refuse a bare '?' or '#'.
    if '?' in endpoint or '#' in endpoint:
        raise BootstrapError('endpoint must not carry query or fragment')
    if parts.path != BOOTSTRAP_PATH:
        raise BootstrapError('endpoint path is not the bootstrap route')
    try:
        port = parts.port
    except ValueError:
        raise BootstrapError('endpoint port is invalid') from None
    if port is not None and not 1 <= port <= 65535:
        raise BootstrapError('endpoint port is invalid')
    return parts


def _check_admission_parent(path):
    candidate = os.path.normpath(os.path.abspath(os.path.expanduser(path)))
    parent = os.path.dirname(candidate)
    name = os.path.basename(candidate)
    if not name or name in ('.', '..'):
        raise BootstrapError('admission directory path is invalid')
    if os.path.islink(parent):
        raise BootstrapError('admission parent must be a real directory')
    if not os.path.isdir(parent):
        raise BootstrapError('admission parent does not exist')
    info = os.lstat(os.path.realpath(parent))
    if not stat.S_ISDIR(info.st_mode):
        raise BootstrapError('admission parent is not a real directory')
    if info.st_uid != os.geteuid():
        raise BootstrapError('admission parent has wrong owner')
    if info.st_mode & 0o022:
        raise BootstrapError('admission parent is group/world writable')
    admission = os.path.join(os.path.realpath(parent), name)
    if os.path.lexists(admission):
        raise BootstrapError('admission directory already exists')
    return admission


def _validate_times(args):
    now_s = int(time.time())
    now_ms = now_s * 1000
    if not (args.hard_deadline_unix_ms > args.setup_deadline_unix_ms > now_ms):
        raise BootstrapError('deadlines are inconsistent')
    setup_s = args.setup_deadline_unix_ms // 1000
    if not (args.bootstrap_expires_at > now_s
            and args.bootstrap_expires_at <= setup_s
            and args.bootstrap_expires_at <= now_s + 900):
        raise BootstrapError('bootstrap expiry is outside the allowed window')


def _bootstrap_deadline(args):
    now_wall = time.time()
    mono = monotonic()
    return min(mono + args.wait_seconds,
               mono + (args.bootstrap_expires_at - now_wall),
               mono + (args.setup_deadline_unix_ms / 1000.0 - now_wall))


def _post(endpoint, token, timeout):
    body = b'{"version":1}'
    req = urllib.request.Request(
        endpoint, data=body, method='POST',
        headers={'Content-Type': 'application/json',
                 'Accept': 'application/json',
                 'Authorization': 'Bearer ' + token})
    try:
        with bridge._opener().open(req, timeout=timeout) as resp:
            status = resp.status
            raw = resp.read(RESPONSE_MAX_BYTES + 1)
    except urllib.error.HTTPError as exc:
        status = exc.code
        try:
            raw = exc.read(RESPONSE_MAX_BYTES + 1)
        except Exception:
            raw = b''
        finally:
            exc.close()
    except (urllib.error.URLError, OSError, socket.timeout, http.client.HTTPException):
        raise BootstrapError('request failed') from None
    if len(raw) > RESPONSE_MAX_BYTES:
        raise BootstrapError('response too large')
    return status, raw


def _eq_str(obj, key, expected):
    value = obj[key]
    if type(value) is not str or value != expected:
        raise BootstrapError('bootstrap response field is invalid')


def _eq_int(obj, key, expected):
    value = obj[key]
    if type(value) is not int or value != expected:
        raise BootstrapError('bootstrap response field is invalid')


def _validate_response(obj, args):
    if not isinstance(obj, dict) or set(obj.keys()) != RESPONSE_FIELDS:
        raise BootstrapError('bootstrap response keys are not exact')
    if type(obj['version']) is not int or obj['version'] != 1:
        raise BootstrapError('bootstrap response version is invalid')
    _eq_str(obj, 'operationId', args.operation_id)
    _eq_str(obj, 'admissionId', args.admission_id)
    _eq_str(obj, 'runnerName', args.name)
    rid = obj['githubRunnerId']
    if type(rid) is not int or not 1 <= rid <= jit.INT64_MAX:
        raise BootstrapError('bootstrap response runner id is invalid')
    _eq_int(obj, 'githubRepositoryId', args.github_repository_id)
    _eq_int(obj, 'maximumSeconds', args.maximum_seconds)
    _eq_int(obj, 'setupDeadlineUnixMs', args.setup_deadline_unix_ms)
    _eq_int(obj, 'hardDeadlineUnixMs', args.hard_deadline_unix_ms)
    jit_raw = obj['encodedJitConfig']
    if type(jit_raw) is not str or not jit_raw:
        raise BootstrapError('bootstrap response JIT config is invalid')
    try:
        jit_bytes = jit_raw.encode('ascii')
    except UnicodeEncodeError:
        raise BootstrapError('bootstrap response JIT config is invalid') from None
    try:
        jit_text = jit._read_jit(io.BytesIO(jit_bytes))
    except Exception:
        raise BootstrapError('bootstrap response JIT config is invalid') from None
    cap = obj['admissionCapability']
    if type(cap) is not str:
        raise BootstrapError('bootstrap response capability is invalid')
    try:
        cap_bytes = cap.encode('ascii')
    except UnicodeEncodeError:
        raise BootstrapError('bootstrap response capability is invalid') from None
    if (not cap_bytes or len(cap_bytes) > CAPABILITY_MAX_BYTES
            or any(chr(b).isspace() for b in cap_bytes)
            or not CAPABILITY_RE.fullmatch(cap)):
        raise BootstrapError('bootstrap response capability is invalid')
    return {'runnerId': rid, 'jit': jit_text, 'capability': cap}


def _bootstrap_exchange(args, deadline, token):
    while True:
        remaining = deadline - monotonic()
        if remaining <= 0:
            raise bridge.BridgeDeadlineError('bootstrap deadline exceeded')
        status, raw = _post(args.endpoint, token, remaining)
        if status == 200:
            return _validate_response(bridge._strict_response_json(raw), args)
        if status == 202:
            obj = bridge._strict_response_json(raw)
            if (isinstance(obj, dict) and set(obj.keys()) == {'pending'}
                    and obj['pending'] is True):
                if monotonic() + PENDING_RETRY_SECONDS >= deadline:
                    raise bridge.BridgeDeadlineError('bootstrap deadline exceeded')
                sleep(PENDING_RETRY_SECONDS)
                continue
        raise BootstrapError('unexpected bootstrap response')


def _bridge_env():
    env = {key: os.environ[key]
           for key in ('HOME', 'LANG', 'LC_ALL', 'TMPDIR') if key in os.environ}
    env['PATH'] = jit.FIXED_PATH
    return env


_evidence_module = None


def _evidence():
    global _evidence_module
    if _evidence_module is None:
        spec = importlib.util.spec_from_file_location(
            'canter_ci_evidence',
            Path(__file__).resolve().with_name('canter-ci-evidence.py'))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        _evidence_module = module
    return _evidence_module


def _drain_evidence(fd, buf):
    """Nonblocking drain of the anonymous evidence pipe, capped at one
    frame. Optional telemetry is fully isolated: any loader or read failure
    returns the buffer unchanged and never disturbs the bridge lifecycle."""
    try:
        evidence = _evidence()
        limit = evidence.FRAME_MAX_BYTES
    except Exception:
        return buf
    while len(buf) < limit:
        try:
            ready, _, _ = select.select([fd], [], [], 0)
        except (OSError, ValueError):
            break
        if not ready:
            break
        try:
            chunk = os.read(fd, limit - len(buf))
        except OSError:
            break
        if not chunk:
            break
        buf += chunk
    return buf


def _maybe_enable_evidence(shared, directory, request, raw):
    """Enable the reporter only for a valid allow receipt whose execution
    deadline matches a well-formed anonymous evidence frame. Evidence
    problems never deny or alter an approved job."""
    try:
        frame = _evidence()._decode_frame(raw)
    except Exception:
        return
    if frame is None:
        return
    try:
        obj = jit._read_private_json(
            os.path.join(directory, jit.FILE_RECEIPT))
        decision, deadline = jit._validate_receipt(
            obj, request, int(time.time() * 1000))
    except Exception:
        return
    if decision != 'allow' or deadline != frame['deadlineUnixMs']:
        return
    shared.setdefault('evidence_frame', None)
    shared['evidence_frame'] = frame
    shared.setdefault('evidence_ready', threading.Event())
    shared['evidence_ready'].set()


def _terminate_bridge(proc):
    try:
        proc.terminate()
    except Exception:
        pass
    try:
        proc.wait(timeout=2)
    except Exception:
        try:
            proc.kill()
            proc.wait(timeout=2)
        except Exception:
            pass


def _publish_bridge_denial(directory):
    try:
        jit._check_private_dir(directory)
    except Exception:
        return False
    try:
        jit._publish_exclusive(
            directory, jit.FILE_DENIED,
            json.dumps({'reason': DENIAL_REASON}).encode() + b'\n', 0o600)
        return True
    except Exception:
        return False


def _deny_bridge(directory, stop):
    # Publish the fixed non-secret denial once the private directory exists;
    # before it does, wait only on the stop event.
    while True:
        if _publish_bridge_denial(directory):
            return
        if stop.is_set():
            return
        sleep(jit.HOOK_POLL_SECONDS)


def _receipt_satisfies(directory, request):
    """True when a private receipt already satisfies the published request:
    the bridge's authority work is done even if its process is still alive,
    so a slow or preempted bridge must not outlive the setup window and
    wrongly deny an already-admitted job."""
    path = os.path.join(directory, jit.FILE_RECEIPT)
    if not os.path.lexists(path):
        return False
    try:
        obj = jit._read_private_json(path)
        jit._validate_receipt(obj, request, int(time.time() * 1000))
    except Exception:
        return False
    return True


def _bridge_worker(args, response, admission_url, shared):
    stop = shared['stop']
    # Cutoffs are derived once at worker start from the original wall-clock
    # setup and capability expiry; a monotonic reading cannot be extended by
    # clock rollback, and the wall reading catches monotonic drift.
    now_wall = time.time()
    setup_mono = monotonic() + (args.setup_deadline_unix_ms / 1000.0 - now_wall)
    cap_mono = monotonic() + (args.bootstrap_expires_at - now_wall)
    wait_mono = min(setup_mono, cap_mono)
    wall_limit = min(args.setup_deadline_unix_ms / 1000.0,
                     float(args.bootstrap_expires_at))
    request_path = os.path.join(args.admission_dir, jit.FILE_REQUEST)
    config_path = os.path.join(os.path.realpath(args.admission_dir),
                               jit.FILE_CONFIG)
    directory = None
    proc = None
    evidence_read = -1
    try:
        while True:
            if stop.is_set():
                return
            if monotonic() >= wait_mono or time.time() >= wall_limit:
                _deny_bridge(args.admission_dir, stop)
                return
            if os.path.lexists(config_path) and os.path.lexists(request_path):
                break
            sleep(jit.HOOK_POLL_SECONDS)
        try:
            config, directory = jit._load_config(config_path)
            jit._check_private_dir(directory)
            request = jit._read_private_json(request_path)
            jit._check_request(request)
            jit._check_request_binding(request, config)
            if (request['admissionId'] != args.admission_id
                    or request['githubRepositoryId'] != args.github_repository_id
                    or request['runnerName'] != args.name
                    or request['githubRunnerId'] != response['runnerId']):
                raise jit.ProtocolError('request does not match bootstrap response')
        except Exception:
            _deny_bridge(directory or args.admission_dir, stop)
            return
        remaining = min(wait_mono - monotonic(), wall_limit - time.time())
        if remaining < 1.0:
            _deny_bridge(directory, stop)
            return
        wait_seconds = max(1, min(30, int(remaining)))
        read_fd = write_fd = evidence_read = evidence_write = -1
        try:
            read_fd, write_fd = os.pipe()
            evidence_read, evidence_write = os.pipe()
        except Exception:
            for fd in (read_fd, write_fd, evidence_read, evidence_write):
                if fd >= 0:
                    try:
                        os.close(fd)
                    except OSError:
                        pass
            _deny_bridge(directory, stop)
            return
        try:
            proc = subprocess.Popen(
                [sys.executable, '-I', str(BRIDGE_PATH),
                 '--endpoint', admission_url,
                 '--admission-dir', directory,
                 '--token-fd', str(read_fd),
                 '--evidence-fd', str(evidence_write),
                 '--wait-seconds', str(wait_seconds)]
                + (['--allow-loopback-http'] if args.allow_loopback_http else []),
                env=_bridge_env(), stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                pass_fds=(read_fd, evidence_write), close_fds=True)
        except Exception:
            for fd in (read_fd, write_fd, evidence_read, evidence_write):
                try:
                    os.close(fd)
                except OSError:
                    pass
            evidence_read = -1
            _deny_bridge(directory, stop)
            return
        with shared['lock']:
            shared['proc'] = proc
        os.close(read_fd)
        os.close(evidence_write)
        try:
            data = response['capability'].encode('ascii')
            while data:
                written = os.write(write_fd, data)
                data = data[written:]
        except OSError:
            pass
        finally:
            try:
                os.close(write_fd)
            except OSError:
                pass
        evidence_buf = b''
        while True:
            evidence_buf = _drain_evidence(evidence_read, evidence_buf)
            if stop.is_set():
                _terminate_bridge(proc)
                return
            if _receipt_satisfies(directory, request):
                # The bridge writes the frame before publishing the
                # receipt; drain once more so a just-written frame is not
                # lost to the terminate that follows.
                evidence_buf = _drain_evidence(evidence_read, evidence_buf)
                _maybe_enable_evidence(
                    shared, directory, request, evidence_buf)
                _terminate_bridge(proc)
                return
            rc = proc.poll()
            if rc is not None:
                if rc != 0:
                    _deny_bridge(directory, stop)
                else:
                    evidence_buf = _drain_evidence(
                        evidence_read, evidence_buf)
                    _maybe_enable_evidence(
                        shared, directory, request, evidence_buf)
                return
            if monotonic() >= wait_mono or time.time() >= wall_limit:
                _terminate_bridge(proc)
                _deny_bridge(directory, stop)
                return
            sleep(0.05)
    except Exception:
        try:
            _deny_bridge(directory or args.admission_dir, stop)
        except Exception:
            pass
    finally:
        if evidence_read >= 0:
            try:
                os.close(evidence_read)
            except OSError:
                pass
        if proc is not None and proc.poll() is None:
            _terminate_bridge(proc)


def _reporter_worker(args, admission_url, shared):
    """Starts the evidence reporter once the bridge has validated an allow
    receipt and a matching anonymous evidence frame. Runs for the job's
    remaining clamped lifetime; every failure is dropped, never fatal."""
    ready = shared.get('evidence_ready')
    if ready is None:
        return
    while not ready.wait(0.2):
        if shared['stop'].is_set():
            return
    if shared['stop'].is_set():
        return
    frame = shared.get('evidence_frame')
    if frame is None:
        return
    try:
        reporter = _evidence().CIReporter(
            admission_url, frame, args.admission_dir, shared['stop'],
            args.hard_deadline_unix_ms, args.allow_loopback_http)
        with shared['lock']:
            shared['reporter'] = reporter
        reporter.run()
    except Exception:
        pass


def _deliver(args, runner_dir, response, admission_url):
    now = time.time()
    if (args.bootstrap_expires_at <= now
            or args.setup_deadline_unix_ms <= now * 1000):
        raise BootstrapError('bootstrap window has expired')
    outer = min(jit.MAX_TIMEOUT_SECONDS,
                int(args.hard_deadline_unix_ms / 1000.0 - now))
    if outer < jit.MIN_TIMEOUT_SECONDS:
        raise BootstrapError('insufficient remaining execution budget')
    argv = ['run', '--runner-dir', runner_dir, '--name', args.name,
            '--github-runner-id', str(response['runnerId']),
            '--github-repository-id', str(args.github_repository_id),
            '--admission-dir', args.admission_dir,
            '--admission-id', args.admission_id,
            '--timeout-seconds', str(outer)]
    shared = {'stop': threading.Event(), 'proc': None,
              'lock': threading.Lock(),
              'evidence_ready': threading.Event(),
              'evidence_frame': None, 'reporter': None}
    worker = threading.Thread(
        target=_bridge_worker,
        args=(args, response, admission_url, shared), daemon=True)
    worker.start()
    reporter_thread = threading.Thread(
        target=_reporter_worker,
        args=(args, admission_url, shared), daemon=True)
    reporter_thread.start()
    try:
        return jit.main(argv, stdin=io.BytesIO(response['jit'].encode('ascii')))
    finally:
        shared['stop'].set()
        with shared['lock']:
            proc = shared['proc']
            reporter = shared['reporter']
        if proc is not None and proc.poll() is None:
            _terminate_bridge(proc)
        if reporter is not None:
            try:
                reporter.close()
            except Exception:
                pass
        worker.join(timeout=5)
        reporter_thread.join(timeout=15)


def _run_bootstrap(args):
    fd_done = False
    try:
        _validate_times(args)
        legacy.check_host(os.environ)
        runner_dir = legacy.validate_runner_dir(args.runner_dir)
        args.admission_dir = _check_admission_parent(args.admission_dir)
        parts = _endpoint_parts(args.endpoint, args.allow_loopback_http)
        admission_url = parts.scheme + '://' + parts.netloc + ADMISSION_PATH
        deadline = _bootstrap_deadline(args)
        restore = bridge._install_deadline_guard(deadline)
        try:
            try:
                token = bridge._read_token(args.token_fd, deadline)
            finally:
                # _read_token closes the descriptor on every path it owns.
                fd_done = True
            response = _bootstrap_exchange(args, deadline, token)
        finally:
            restore()
        return _deliver(args, runner_dir, response, admission_url)
    finally:
        if not fd_done:
            try:
                os.close(args.token_fd)
            except OSError:
                pass


def run(argv):
    """Public entry for callers/tests: returns the JIT supervisor result or
    raises on any bootstrap rejection."""
    return _run_bootstrap(parse_args(argv))


def main(argv=None):
    try:
        args = parse_args(sys.argv[1:] if argv is None else argv)
    except SystemExit:
        return 2
    except KeyboardInterrupt:
        return 130
    try:
        return _run_bootstrap(args)
    except bridge.BridgeDeadlineError:
        return 124
    except KeyboardInterrupt:
        return 130
    except Exception:
        return 1


if __name__ == '__main__':
    signal.signal(signal.SIGPIPE, signal.SIG_DFL)
    sys.exit(main())

#!/usr/bin/env python3
"""Supervise one ephemeral JIT-configured GitHub Actions runner job.

Offline admission prototype: the parent supplies one base64 JIT config on
stdin, this supervisor spawns the official run.sh once with
ACTIONS_RUNNER_INPUT_JITCONFIG, and a generated JOB_STARTED hook admits at
most one job through a private request/receipt exchange inside a disposable
admission directory. It is not a runner backend and does not prove billing,
source authority, or registration state.
"""
import argparse
import importlib.util
import json
import os
import re
import shlex
import signal
import stat
import subprocess
import sys
import time
from pathlib import Path
from time import monotonic, sleep

_spec = importlib.util.spec_from_file_location(
    'canter_ci_runner_legacy',
    Path(__file__).resolve().with_name('canter-ci-runner.py'))
legacy = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(legacy)

FIXED_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
ADMISSION_SECONDS = 900
REQUEST_WAIT_SECONDS = 30
HOOK_TIMEOUT_SECONDS = 30
POLL_SECONDS = 0.1
HOOK_POLL_SECONDS = 0.05
MAX_JIT_BYTES = 65536
MAX_STATE_BYTES = 16384
MAX_EXECUTION_DEADLINE_MS = 21600000
INT64_MAX = 9223372036854775807
MIN_TIMEOUT_SECONDS = 300
MAX_TIMEOUT_SECONDS = 22500
DEFAULT_TIMEOUT_SECONDS = 22500
TIMEOUT_EXIT = 124
DENIED_EXIT = 125

ADMISSION_ID_RE = re.compile(r'[0-9a-f]{64}')
JIT_B64_RE = re.compile(r'[A-Za-z0-9+/]+={0,2}')
DECIMAL_RE = re.compile(r'[0-9]+')
JOB_KEY_RE = re.compile(r'[A-Za-z_][A-Za-z0-9_-]{0,255}')

REQUEST_FIELDS = ('schemaVersion', 'admissionId', 'githubRunnerId',
                  'githubRepositoryId', 'repository', 'githubRunId',
                  'runAttempt', 'jobKey', 'runnerName')
CONFIG_FIELDS = frozenset(('schemaVersion', 'admissionId', 'githubRunnerId',
                           'githubRepositoryId', 'runnerName'))
FILE_CONFIG = 'config.json'
FILE_HOOK = 'job-started.sh'
FILE_REQUEST = 'request.json'
FILE_RECEIPT = 'receipt.json'
FILE_DENIED = 'denied.json'

DENY_REASONS = frozenset(('deny', 'invalid-context', 'duplicate-request',
                          'malformed-receipt', 'timeout', 'internal-error',
                          'signal'))


class ContextError(Exception):
    pass


class ProtocolError(Exception):
    pass


def int64_arg(text):
    if not DECIMAL_RE.fullmatch(text):
        raise argparse.ArgumentTypeError('expected a decimal integer')
    value = int(text)
    if not 1 <= value <= INT64_MAX:
        raise argparse.ArgumentTypeError('expected an integer in 1..9223372036854775807')
    return value


def admission_id_arg(text):
    if not ADMISSION_ID_RE.fullmatch(text):
        raise argparse.ArgumentTypeError('expected exactly 64 lowercase hexadecimal characters')
    return text


def timeout_arg(text):
    if not DECIMAL_RE.fullmatch(text):
        raise argparse.ArgumentTypeError('expected an integer number of seconds')
    value = int(text)
    if not MIN_TIMEOUT_SECONDS <= value <= MAX_TIMEOUT_SECONDS:
        raise argparse.ArgumentTypeError(
            f'expected between {MIN_TIMEOUT_SECONDS} and {MAX_TIMEOUT_SECONDS} seconds')
    return value


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog='canter-ci-jit-runner',
        description='Supervise one ephemeral JIT-configured runner job in a disposable admission directory.')
    sub = parser.add_subparsers(dest='command', required=True)
    run = sub.add_parser('run', help='spawn the official runner once under admission supervision')
    run.add_argument('--runner-dir', required=True,
                     help='directory containing an already extracted official runner')
    run.add_argument('--name', required=True, type=legacy.runner_name_arg,
                     help='runner name: 1-64 characters from [a-zA-Z0-9_-]')
    run.add_argument('--github-runner-id', required=True, type=int64_arg,
                     help='decimal GitHub runner id')
    run.add_argument('--github-repository-id', required=True, type=int64_arg,
                     help='decimal GitHub repository id')
    run.add_argument('--admission-dir', required=True,
                     help='path for a fresh private admission directory that must not exist')
    run.add_argument('--admission-id', required=True, type=admission_id_arg,
                     help='64 lowercase hex characters supplied by the parent')
    run.add_argument('--toolchain-dir', default=None,
                     help='optional prepared toolchain prefix produced by canter-ci-prepare')
    run.add_argument('--timeout-seconds', type=timeout_arg, default=DEFAULT_TIMEOUT_SECONDS,
                     help=f'outer wall-clock budget in seconds ({MIN_TIMEOUT_SECONDS}-{MAX_TIMEOUT_SECONDS}, '
                          f'default {DEFAULT_TIMEOUT_SECONDS})')
    hook = sub.add_parser('hook', help=argparse.SUPPRESS)
    hook.add_argument('--config', required=True,
                      help='absolute path to the generated admission config.json')
    return parser.parse_args(argv)


def _open_private(path, flags):
    try:
        fd = os.open(path, flags | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError:
        raise ProtocolError('protocol file cannot be opened safely') from None
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise ProtocolError('protocol file is not a regular file')
        if info.st_uid != os.geteuid():
            raise ProtocolError('protocol file has wrong owner')
        if info.st_mode & 0o077:
            raise ProtocolError('protocol file is group/world accessible')
    except Exception:
        os.close(fd)
        raise
    return fd


def _read_private_json(path, limit=MAX_STATE_BYTES):
    fd = _open_private(path, os.O_RDONLY)
    try:
        chunks = []
        total = 0
        while True:
            block = os.read(fd, 65536)
            if not block:
                break
            total += len(block)
            if total > limit:
                raise ProtocolError('protocol file exceeds size bound')
            chunks.append(block)
    finally:
        os.close(fd)
    raw = b''.join(chunks)
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        raise ProtocolError('protocol file is not UTF-8') from None

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
        raise ProtocolError('protocol file is not strict JSON') from None


def _check_private_dir(path):
    info = os.lstat(path)
    if not stat.S_ISDIR(info.st_mode):
        raise ProtocolError('admission directory is not a real directory')
    if info.st_uid != os.geteuid():
        raise ProtocolError('admission directory has wrong owner')
    if info.st_mode & 0o077:
        raise ProtocolError('admission directory is group/world accessible')


def _is_int(value):
    return type(value) is int


def _is_int64(value):
    return _is_int(value) and 1 <= value <= INT64_MAX


def _check_request(obj):
    if not isinstance(obj, dict) or set(obj.keys()) != set(REQUEST_FIELDS):
        raise ProtocolError('request keys are not exact')
    if not _is_int(obj['schemaVersion']) or obj['schemaVersion'] != 1:
        raise ProtocolError('request schemaVersion is invalid')
    if not isinstance(obj['admissionId'], str) or not ADMISSION_ID_RE.fullmatch(obj['admissionId']):
        raise ProtocolError('request admissionId is invalid')
    for key in ('githubRunnerId', 'githubRepositoryId', 'githubRunId', 'runAttempt'):
        if not _is_int64(obj[key]):
            raise ProtocolError('request ' + key + ' is invalid')
    try:
        legacy.repository_arg(obj['repository'])
    except Exception:
        raise ProtocolError('request repository is invalid') from None
    if not isinstance(obj['jobKey'], str) or not JOB_KEY_RE.fullmatch(obj['jobKey']):
        raise ProtocolError('request jobKey is invalid')
    if not isinstance(obj['runnerName'], str) or not legacy.RUNNER_NAME_RE.fullmatch(obj['runnerName']):
        raise ProtocolError('request runnerName is invalid')


def _check_echoes(receipt, request):
    for key in REQUEST_FIELDS:
        value = receipt.get(key)
        if type(value) is not type(request[key]) or value != request[key]:
            raise ProtocolError('receipt echo mismatch for ' + key)


def _check_request_binding(request, config):
    for key in ('admissionId', 'githubRunnerId', 'githubRepositoryId', 'runnerName'):
        if type(request[key]) is not type(config[key]) or request[key] != config[key]:
            raise ProtocolError('request does not match bootstrap config')


def _validate_receipt(obj, request, now_ms):
    if not isinstance(obj, dict):
        raise ProtocolError('receipt is not an object')
    keys = set(obj.keys())
    base = set(REQUEST_FIELDS)
    if keys == base | {'decision'}:
        if obj['decision'] != 'deny':
            raise ProtocolError('receipt decision is invalid')
        _check_echoes(obj, request)
        return 'deny', None
    if keys == base | {'decision', 'executionDeadlineUnixMs'}:
        if obj['decision'] != 'allow':
            raise ProtocolError('receipt decision is invalid')
        _check_echoes(obj, request)
        deadline = obj['executionDeadlineUnixMs']
        if not _is_int(deadline):
            raise ProtocolError('receipt deadline is not an integer')
        if deadline <= now_ms or deadline - now_ms > MAX_EXECUTION_DEADLINE_MS:
            raise ProtocolError('receipt deadline is expired or too far ahead')
        return 'allow', deadline
    raise ProtocolError('receipt keys are not exact')


def _publish_exclusive(directory, name, data, mode):
    pending = os.path.join(directory, name + '.pending')
    final = os.path.join(directory, name)
    fd = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.link(pending, final, follow_symlinks=False)
    finally:
        try:
            os.remove(pending)
        except OSError:
            pass


def _load_config(path):
    if not os.path.isabs(path):
        raise ContextError('config path must be absolute')
    canonical = os.path.normpath(path)
    if os.path.basename(canonical) != FILE_CONFIG:
        raise ContextError('config path must name config.json')
    if os.path.islink(canonical) or os.path.realpath(canonical) != canonical:
        raise ContextError('config path must be canonical')
    try:
        obj = _read_private_json(canonical)
    except ProtocolError:
        raise ContextError('config.json failed private validation') from None
    if not isinstance(obj, dict) or set(obj.keys()) != CONFIG_FIELDS:
        raise ContextError('config.json keys are not exact')
    if not _is_int(obj['schemaVersion']) or obj['schemaVersion'] != 1:
        raise ContextError('config.json schemaVersion is invalid')
    if not isinstance(obj['admissionId'], str) or not ADMISSION_ID_RE.fullmatch(obj['admissionId']):
        raise ContextError('config.json admissionId is invalid')
    for key in ('githubRunnerId', 'githubRepositoryId'):
        if not _is_int64(obj[key]):
            raise ContextError('config.json ' + key + ' is invalid')
    if not isinstance(obj['runnerName'], str) or not legacy.RUNNER_NAME_RE.fullmatch(obj['runnerName']):
        raise ContextError('config.json runnerName is invalid')
    return obj, os.path.dirname(canonical)


def _env_int64(env, key):
    text = env.get(key, '')
    if not DECIMAL_RE.fullmatch(text):
        raise ContextError(key + ' is missing or invalid')
    value = int(text)
    if not 1 <= value <= INT64_MAX:
        raise ContextError(key + ' is out of range')
    return value


def _context_request(config, env):
    repository = env.get('GITHUB_REPOSITORY', '')
    try:
        legacy.repository_arg(repository)
    except Exception:
        raise ContextError('GITHUB_REPOSITORY is missing or invalid') from None
    if _env_int64(env, 'GITHUB_REPOSITORY_ID') != config['githubRepositoryId']:
        raise ContextError('GITHUB_REPOSITORY_ID does not match admission')
    if env.get('RUNNER_NAME', '') != config['runnerName']:
        raise ContextError('RUNNER_NAME does not match admission')
    request = {
        'schemaVersion': 1,
        'admissionId': config['admissionId'],
        'githubRunnerId': config['githubRunnerId'],
        'githubRepositoryId': config['githubRepositoryId'],
        'repository': repository,
        'githubRunId': _env_int64(env, 'GITHUB_RUN_ID'),
        'runAttempt': _env_int64(env, 'GITHUB_RUN_ATTEMPT'),
        'jobKey': env.get('GITHUB_JOB', ''),
        'runnerName': config['runnerName'],
    }
    if not JOB_KEY_RE.fullmatch(request['jobKey']):
        raise ContextError('GITHUB_JOB is missing or invalid')
    if env.get('GITHUB_SERVER_URL', '') != 'https://github.com':
        raise ContextError('GITHUB_SERVER_URL is unsupported')
    if env.get('GITHUB_API_URL', '') != 'https://api.github.com':
        raise ContextError('GITHUB_API_URL is unsupported')
    return request


def _record_denial(directory, reason):
    if reason not in DENY_REASONS or directory is None:
        return
    try:
        _publish_exclusive(directory, FILE_DENIED,
                           json.dumps({'reason': reason}).encode() + b'\n', 0o600)
    except OSError:
        pass


def _block_forever():
    while True:
        signal.pause()


def _deny(directory, reason):
    _record_denial(directory, reason)
    _block_forever()


def _wait_receipt(directory, request, deadline, state):
    receipt_path = os.path.join(directory, FILE_RECEIPT)
    while True:
        if state.signum is not None:
            raise InterruptedError('hook interrupted')
        if monotonic() >= deadline:
            raise TimeoutError('receipt wait timed out')
        if os.path.lexists(receipt_path):
            obj = _read_private_json(receipt_path)
            decision, _ = _validate_receipt(obj, request, int(time.time() * 1000))
            return decision
        sleep(HOOK_POLL_SECONDS)


def hook_main(args):
    entry = monotonic()
    state = legacy.InterruptState()
    previous = {sig: signal.signal(sig, state.handler)
                for sig in (signal.SIGINT, signal.SIGTERM)}
    directory = None
    try:
        config, directory = _load_config(args.config)
        _check_private_dir(directory)
        request = _context_request(config, os.environ)
        try:
            payload = json.dumps(request, separators=(',', ':')).encode() + b'\n'
            _publish_exclusive(directory, FILE_REQUEST, payload, 0o600)
        except FileExistsError:
            _deny(directory, 'duplicate-request')
        except OSError:
            _deny(directory, 'internal-error')
        try:
            decision = _wait_receipt(directory, request,
                                     entry + HOOK_TIMEOUT_SECONDS, state)
        except TimeoutError:
            _deny(directory, 'timeout')
        except InterruptedError:
            _deny(directory, 'signal')
        except (ProtocolError, OSError, ValueError):
            _deny(directory, 'malformed-receipt')
        if decision == 'deny':
            _deny(directory, 'deny')
        return 0
    except ContextError:
        _deny(directory, 'invalid-context')
    except Exception:
        _deny(directory, 'internal-error')
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)


def _read_jit(stream):
    if stream.isatty():
        raise RuntimeError('JIT config must be piped on stdin')
    data = stream.read(MAX_JIT_BYTES + 3) or b''
    if isinstance(data, str):
        data = data.encode('utf-8')
    if data.endswith(b'\r\n'):
        data = data[:-2]
    elif data.endswith(b'\n'):
        data = data[:-1]
    if not data:
        raise RuntimeError('JIT config is empty')
    if len(data) > MAX_JIT_BYTES:
        raise RuntimeError('JIT config exceeds 65536 encoded bytes')
    if b'\n' in data or b'\r' in data:
        raise RuntimeError('JIT config must be a single line')
    try:
        text = data.decode('ascii')
    except UnicodeDecodeError:
        raise RuntimeError('JIT config is not valid base64') from None
    if not JIT_B64_RE.fullmatch(text) or len(data) % 4 != 0:
        raise RuntimeError('JIT config is not valid base64')
    return text


def _prepare_admission(path):
    candidate = os.path.normpath(os.path.abspath(os.path.expanduser(path)))
    parent = os.path.dirname(candidate)
    name = os.path.basename(candidate)
    if not name or name in ('.', '..'):
        raise RuntimeError('admission directory path is invalid')
    if os.path.islink(parent):
        raise RuntimeError('admission parent must be a real directory')
    if not os.path.isdir(parent):
        raise RuntimeError('admission parent does not exist')
    info = os.lstat(os.path.realpath(parent))
    if not stat.S_ISDIR(info.st_mode):
        raise RuntimeError('admission parent is not a real directory')
    if info.st_uid != os.geteuid():
        raise RuntimeError('admission parent has wrong owner')
    if info.st_mode & 0o022:
        raise RuntimeError('admission parent is group/world writable')
    admission = os.path.join(os.path.realpath(parent), name)
    if os.path.lexists(admission):
        raise RuntimeError('admission directory already exists')
    os.mkdir(admission, 0o700)
    os.chmod(admission, 0o700)
    return admission


def _hook_script(directory):
    python_exe = shlex.quote(os.path.realpath(sys.executable))
    script = shlex.quote(os.path.realpath(__file__))
    config = shlex.quote(os.path.join(directory, FILE_CONFIG))
    return (f'{python_exe} -I {script} hook --config {config} '
            '|| { while :; do sleep 3600; done; }\n').encode()


def _write_admission_files(directory, args):
    config = {
        'schemaVersion': 1,
        'admissionId': args.admission_id,
        'githubRunnerId': args.github_runner_id,
        'githubRepositoryId': args.github_repository_id,
        'runnerName': args.name,
    }
    payload = json.dumps(config, separators=(',', ':')).encode() + b'\n'
    _publish_exclusive(directory, FILE_CONFIG, payload, 0o600)
    _publish_exclusive(directory, FILE_HOOK, _hook_script(directory), 0o700)
    return config


def _child_env(environ, jit, toolchain_dir):
    env = {key: environ[key] for key in ('HOME', 'LANG', 'LC_ALL', 'TMPDIR')
           if key in environ}
    prefix = ''
    if toolchain_dir is not None:
        root = os.path.realpath(toolchain_dir)
        prefix = os.path.join(root, 'bin') + os.pathsep
        env['RUNNER_TOOL_CACHE'] = os.path.join(root, 'toolcache')
    env['PATH'] = prefix + FIXED_PATH
    env['ACTIONS_RUNNER_INPUT_JITCONFIG'] = jit
    return env


def _supervise(proc, directory, timeout_seconds, interrupted, config):
    spawn_mono = monotonic()
    overall_mono = spawn_mono + timeout_seconds
    admission_mono = spawn_mono + ADMISSION_SECONDS
    request_path = os.path.join(directory, FILE_REQUEST)
    receipt_path = os.path.join(directory, FILE_RECEIPT)
    denied_path = os.path.join(directory, FILE_DENIED)
    state = {'request': None, 'request_at': None, 'allow': None}

    def admission_expired(now_mono):
        if now_mono >= admission_mono:
            return True
        return (state['request'] is not None
                and now_mono >= state['request_at'] + REQUEST_WAIT_SECONDS)

    def execution_expired(now_mono):
        return (now_mono >= state['allow'][0]
                or time.time() >= state['allow'][1])

    def observe():
        # Returns 'denied', 'protocol', or None; may freeze a valid allow.
        if os.path.lexists(denied_path):
            return 'denied'
        if state['request'] is None and os.path.lexists(request_path):
            try:
                candidate = _read_private_json(request_path)
                _check_request(candidate)
                _check_request_binding(candidate, config)
            except Exception:
                return 'protocol'
            state['request'] = candidate
            state['request_at'] = monotonic()
        if state['request'] is None:
            if os.path.lexists(receipt_path):
                return 'protocol'
        elif state['allow'] is None and os.path.lexists(receipt_path):
            if admission_expired(monotonic()):
                return None
            try:
                receipt = _read_private_json(receipt_path)
                decision, deadline = _validate_receipt(
                    receipt, state['request'], int(time.time() * 1000))
            except Exception:
                return 'protocol'
            if decision == 'deny':
                return 'denied'
            now_ms = int(time.time() * 1000)
            state['allow'] = (monotonic() + (deadline - now_ms) / 1000.0,
                              deadline / 1000.0)
        return None

    def reap():
        legacy.kill_group(proc, signal.SIGKILL)
        try:
            proc.wait(timeout=10)
        except Exception:
            pass

    while True:
        if proc.poll() is not None:
            break
        now_mono = monotonic()
        verdict = observe()
        if verdict is not None:
            reap()
            return DENIED_EXIT
        if state['allow'] is None:
            expired = admission_expired(now_mono)
        else:
            expired = execution_expired(now_mono)
        if expired or now_mono >= overall_mono:
            reap()
            return TIMEOUT_EXIT
        signum = interrupted()
        if signum is not None:
            reap()
            return 128 + signum
        sleep(POLL_SECONDS)
    verdict = observe()
    if verdict is not None:
        reap()
        return DENIED_EXIT
    now_mono = monotonic()
    if state['allow'] is None:
        timed_out = admission_expired(now_mono)
    else:
        timed_out = execution_expired(now_mono)
    if timed_out or now_mono >= overall_mono:
        reap()
        return TIMEOUT_EXIT
    signum = interrupted()
    if signum is not None:
        reap()
        return 128 + signum
    reap()
    if os.path.lexists(denied_path):
        return DENIED_EXIT
    if state['allow'] is None:
        return DENIED_EXIT
    rc = proc.returncode
    if rc is None:
        rc = 1
    if rc < 0:
        return 128 + (-rc)
    return rc


def run_main(args, stdin=None):
    legacy.check_host(os.environ)
    runner_dir = legacy.validate_runner_dir(args.runner_dir)
    toolchain_dir = (legacy.validate_toolchain_dir(args.toolchain_dir)
                     if args.toolchain_dir else None)
    if stdin is None:
        stream = getattr(sys.stdin, 'buffer', sys.stdin)
    else:
        stream = stdin
    jit = _read_jit(stream)
    legacy.scrub_parent_env()
    directory = _prepare_admission(args.admission_dir)
    config = _write_admission_files(directory, args)
    env = _child_env(os.environ, jit, toolchain_dir)
    env['ACTIONS_RUNNER_HOOK_JOB_STARTED'] = os.path.join(directory, FILE_HOOK)
    state = legacy.InterruptState()
    previous = legacy.install_handlers(state)
    proc = None
    try:
        proc = subprocess.Popen([os.path.join(runner_dir, 'run.sh')],
                                cwd=runner_dir, env=env,
                                stdin=subprocess.DEVNULL, start_new_session=True)
        return _supervise(proc, directory, args.timeout_seconds, state, config)
    finally:
        if proc is not None:
            legacy.kill_group(proc, signal.SIGKILL)
            try:
                proc.wait(timeout=10)
            except Exception:
                pass
        legacy.restore_handlers(previous)


def main(argv=None, stdin=None):
    args = parse_args(argv)
    if args.command == 'hook':
        return hook_main(args)
    return run_main(args, stdin=stdin)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print('interrupted', file=sys.stderr)
        sys.exit(130)
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as exc:
        print(f'JIT runner launch failed: {exc}', file=sys.stderr)
        sys.exit(1)



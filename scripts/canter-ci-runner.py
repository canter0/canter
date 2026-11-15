#!/usr/bin/env python3
"""Run one ephemeral GitHub Actions runner job inside a disposable runner directory."""
import argparse
import hashlib
import json
import os
import platform
import re
import signal
import stat
import subprocess
import sys
from time import monotonic, sleep

MIN_TIMEOUT_SECONDS = 300
MAX_TIMEOUT_SECONDS = 7200
DEFAULT_TIMEOUT_SECONDS = 3600
MAX_TOKEN_BYTES = 4096
POLL_SECONDS = 0.1
GRACE_SECONDS = 10.0
TIMEOUT_EXIT = 124

BASE_ENV_KEYS = ('PATH', 'HOME', 'LANG', 'LC_ALL', 'TMPDIR')
RESERVED_NAMES = ('.runner', '.credentials', '.credentials_rsaparams',
                  '.env', '.path', '.service', '_work')
REPOSITORY_RE = re.compile(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}')
RUNNER_NAME_RE = re.compile(r'[A-Za-z0-9_-]{1,64}')
VERSION_RE = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+')
STATUS_MESSAGE = ('ephemeral runner ended; destroy disposable VM; '
                  'runner registration may need removal after failure')


class InterruptState:
    def __init__(self):
        self.signum = None

    def handler(self, signum, frame):
        self.signum = signum

    def __call__(self):
        return self.signum


def repository_arg(text):
    if not REPOSITORY_RE.fullmatch(text):
        raise argparse.ArgumentTypeError('expected a GitHub repository in OWNER/REPO form')
    if text.rsplit('/', 1)[1] in ('.', '..'):
        raise argparse.ArgumentTypeError('repository name cannot be . or ..')
    return text


def runner_name_arg(text):
    if not RUNNER_NAME_RE.fullmatch(text):
        raise argparse.ArgumentTypeError('expected 1-64 characters from [a-zA-Z0-9_-]')
    return text


def timeout_arg(text):
    if not re.fullmatch(r'[0-9]+', text):
        raise argparse.ArgumentTypeError('expected an integer number of seconds')
    value = int(text)
    if not MIN_TIMEOUT_SECONDS <= value <= MAX_TIMEOUT_SECONDS:
        raise argparse.ArgumentTypeError(
            f'expected between {MIN_TIMEOUT_SECONDS} and {MAX_TIMEOUT_SECONDS} seconds')
    return value


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog='canter-ci-runner',
        description='Configure and run one ephemeral GitHub Actions runner job in a disposable directory.')
    parser.add_argument('--repository', required=True, type=repository_arg,
                        help='GitHub repository in OWNER/REPO form')
    parser.add_argument('--runner-dir', required=True,
                        help='directory containing an already extracted official runner')
    parser.add_argument('--name', required=True, type=runner_name_arg,
                        help='runner name: 1-64 characters from [a-zA-Z0-9_-]')
    parser.add_argument('--timeout-seconds', type=timeout_arg, default=DEFAULT_TIMEOUT_SECONDS,
                        help=f'wall-clock budget in seconds for runner configuration and the job, '
                             f'starting once the registration token is read from stdin '
                             f'({MIN_TIMEOUT_SECONDS}-{MAX_TIMEOUT_SECONDS}, default {DEFAULT_TIMEOUT_SECONDS})')
    parser.add_argument('--toolchain-dir', default=None,
                        help='optional prepared toolchain prefix produced by canter-ci-prepare; '
                             'its bin directory is prepended to PATH and toolcache is exported '
                             'as RUNNER_TOOL_CACHE for both runner phases')
    return parser.parse_args(argv)


def check_host(environ):
    if platform.system() != 'Linux':
        raise RuntimeError('canter-ci-runner only supports Linux runners')
    if os.geteuid() == 0:
        raise RuntimeError('refusing to run the GitHub runner as root')
    if environ.get('RUNNER_ALLOW_RUNASROOT'):
        raise RuntimeError('RUNNER_ALLOW_RUNASROOT is set; refusing to continue')


def validate_runner_dir(path):
    path = os.path.expanduser(path)
    if not os.path.isdir(path):
        raise RuntimeError(f'runner directory does not exist: {path}')
    root = os.path.realpath(path)
    for name in RESERVED_NAMES:
        if os.path.lexists(os.path.join(path, name)):
            raise RuntimeError(
                f'runner directory already contains {name}; use a fresh extracted official runner directory')
    for name in ('config.sh', 'run.sh'):
        script = os.path.join(path, name)
        if not os.path.isfile(script) or not os.access(script, os.X_OK):
            raise RuntimeError(f'{name} is missing or not executable in the runner directory')
        if os.path.commonpath((root, os.path.realpath(script))) != root:
            raise RuntimeError(f'{name} resolves outside the runner directory')
    return root


def validate_toolchain_dir(path):
    root = os.path.realpath(os.path.expanduser(path))
    if not os.path.isdir(root):
        raise RuntimeError(f'toolchain directory does not exist: {path}')
    manifest_path = os.path.join(root, 'manifest.json')
    if not os.path.isfile(manifest_path):
        raise RuntimeError('toolchain manifest.json is missing')
    try:
        with open(manifest_path, 'r', encoding='utf-8') as stream:
            manifest = json.load(stream)
    except (OSError, ValueError):
        raise RuntimeError('toolchain manifest.json is not valid JSON') from None
    if not isinstance(manifest, dict) or manifest.get('schemaVersion') != 1 \
            or manifest.get('platform') != 'linux-x64':
        raise RuntimeError('toolchain manifest schema or platform is unsupported')
    versions = {}
    for tool in ('go', 'node', 'pnpm', 'runner'):
        spec = manifest.get(tool)
        version = spec.get('version') if isinstance(spec, dict) else None
        if not isinstance(version, str) or not VERSION_RE.fullmatch(version):
            raise RuntimeError(f'toolchain manifest has an invalid {tool} version')
        versions[tool] = version
    required = []
    for tool in ('go', 'node'):
        version_root = os.path.join('toolcache', tool, versions[tool])
        required.append((os.path.join(version_root, 'x64.complete'), False))
        required.append((os.path.join(version_root, 'x64', 'bin',
                                      'go' if tool == 'go' else 'node'), True))
    required.append((os.path.join('pnpm', versions['pnpm'], 'bin', 'pnpm.cjs'), False))
    for rel, executable in required:
        candidate = os.path.join(root, rel)
        resolved = os.path.realpath(candidate)
        if os.path.commonpath((root, resolved)) != root:
            raise RuntimeError(f'toolchain path escapes the prefix: {rel}')
        if not os.path.isfile(resolved):
            raise RuntimeError(f'toolchain is missing {rel}')
        if executable and not os.access(resolved, os.X_OK):
            raise RuntimeError(f'toolchain file is not executable: {rel}')
    for name in ('go', 'node', 'npm', 'npx', 'pnpm'):
        link = os.path.join(root, 'bin', name)
        resolved = os.path.realpath(link)
        if os.path.commonpath((root, resolved)) != root:
            raise RuntimeError(f'toolchain bin link escapes the prefix: {name}')
        if not os.path.isfile(resolved):
            raise RuntimeError(f'toolchain bin link is broken: {name}')
        if not os.access(resolved, os.X_OK):
            raise RuntimeError(f'toolchain bin link is not executable: {name}')
    return root


PAYLOAD_UID = 0
PAYLOAD_GID = 0
PAYLOAD_IMAGE_KEYS = frozenset((
    'schemaVersion', 'platform', 'baseOS', 'runnerUrl', 'runnerSha256',
    'toolchainManifestSha256', 'dependenciesInstalled'))
PAYLOAD_ROOT_CHILDREN = frozenset(('image.json', 'runner.tar.gz', 'toolchains'))
PAYLOAD_TOOLCHAIN_CHILDREN = frozenset(
    ('bin', 'manifest.json', 'pnpm', 'runner', 'toolcache'))
PAYLOAD_STATE_NAMES = RESERVED_NAMES + ('_diag',)
PAYLOAD_SHA256_RE = re.compile(r'[0-9a-f]{64}')
MAX_IMAGE_JSON_BYTES = 8192
MAX_TOOLCHAIN_JSON_BYTES = 65536
MAX_RUNNER_ARCHIVE_BYTES = 512 * 1024 * 1024
MAX_PAYLOAD_ENTRIES = 200000


def _read_payload_file(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise RuntimeError('payload metadata is not a regular file')
        if info.st_uid != PAYLOAD_UID or info.st_gid != PAYLOAD_GID \
                or stat.S_IMODE(info.st_mode) != 0o444:
            raise RuntimeError('payload metadata is not a root-owned read-only file')
        if info.st_size > limit:
            raise RuntimeError('payload metadata exceeds size limit')
        chunks = []
        total = 0
        while True:
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if total > limit:
                raise RuntimeError('payload metadata exceeds size limit')
            chunks.append(chunk)
        return b''.join(chunks)
    finally:
        os.close(fd)


def _load_payload_json(data, what):
    try:
        text = data.decode('utf-8')
    except UnicodeDecodeError:
        raise RuntimeError(what + ' is not valid UTF-8') from None

    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError('duplicate key')
            result[key] = value
        return result

    try:
        value = json.loads(text, object_pairs_hook=unique)
    except ValueError:
        raise RuntimeError(what + ' is not valid JSON') from None
    if not isinstance(value, dict):
        raise RuntimeError(what + ' is not a JSON object')
    return value


def _payload_manifest_digest(manifest):
    raw = (json.dumps(manifest, sort_keys=True, separators=(',', ':'),
                      ensure_ascii=True) + '\n').encode('ascii')
    return hashlib.sha256(raw).hexdigest()


def _payload_link_ok(root_real, path):
    target = os.readlink(path)
    if not target or os.path.isabs(target):
        raise RuntimeError('payload link must be a relative target')
    resolved = os.path.realpath(path)
    if os.path.commonpath((root_real, resolved)) != root_real:
        raise RuntimeError('payload link escapes the prefix')
    if not os.path.exists(resolved):
        raise RuntimeError('payload link is dangling or cyclic')


def _validate_payload_tree(root):
    root_real = os.path.realpath(root)
    runner_root = os.path.realpath(os.path.join(root, 'toolchains', 'runner'))
    count = 0

    def walk_error(_error):
        raise RuntimeError('payload tree is unreadable')

    for dirpath, dirnames, filenames in os.walk(
            root, followlinks=False, onerror=walk_error):
        dir_real = os.path.realpath(dirpath)
        under_runner = os.path.commonpath((runner_root, dir_real)) == runner_root
        for name in dirnames + filenames:
            count += 1
            if count > MAX_PAYLOAD_ENTRIES:
                raise RuntimeError('payload tree exceeds entry limit')
            if under_runner and name in PAYLOAD_STATE_NAMES:
                raise RuntimeError('payload contains reserved runner state name')
            path = os.path.join(dirpath, name)
            info = os.lstat(path)
            if info.st_uid != PAYLOAD_UID or info.st_gid != PAYLOAD_GID:
                raise RuntimeError('payload entry is not root-owned')
            if stat.S_ISLNK(info.st_mode):
                _payload_link_ok(root_real, path)
                continue
            if stat.S_ISDIR(info.st_mode):
                if stat.S_IMODE(info.st_mode) != 0o555:
                    raise RuntimeError('payload directory mode is not 0555')
                continue
            if stat.S_ISREG(info.st_mode):
                if stat.S_IMODE(info.st_mode) not in (0o444, 0o555):
                    raise RuntimeError('payload file mode is not 0444 or 0555')
                continue
            raise RuntimeError('payload entry is not a file or directory')


def validate_prepared_image(path, runner_url, runner_sha256):
    if not isinstance(runner_url, str) or not runner_url:
        raise RuntimeError('prepared image requires the approved runner URL')
    if not isinstance(runner_sha256, str) \
            or not PAYLOAD_SHA256_RE.fullmatch(runner_sha256):
        raise RuntimeError('prepared image requires the approved runner SHA-256')
    if not os.path.isabs(path):
        raise RuntimeError('prepared image prefix must be absolute')
    if os.path.realpath(path) != path:
        raise RuntimeError('prepared image prefix is not canonical')
    info = os.lstat(path)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != PAYLOAD_UID \
            or info.st_gid != PAYLOAD_GID or stat.S_IMODE(info.st_mode) != 0o555:
        raise RuntimeError(
            'prepared image prefix is not a root-owned read-only directory')
    parent = os.path.dirname(path)
    if os.path.islink(parent):
        raise RuntimeError('prepared image parent must not be a symlink')
    parent_info = os.lstat(parent)
    if not stat.S_ISDIR(parent_info.st_mode) \
            or parent_info.st_uid != PAYLOAD_UID \
            or parent_info.st_gid != PAYLOAD_GID \
            or parent_info.st_mode & 0o022:
        raise RuntimeError('prepared image parent is not a secure directory')
    if frozenset(os.listdir(path)) != PAYLOAD_ROOT_CHILDREN:
        raise RuntimeError('prepared image prefix children do not match')
    toolchains = os.path.join(path, 'toolchains')
    if frozenset(os.listdir(toolchains)) != PAYLOAD_TOOLCHAIN_CHILDREN:
        raise RuntimeError('prepared toolchain children do not match')
    _validate_payload_tree(path)
    image = _load_payload_json(
        _read_payload_file(os.path.join(path, 'image.json'), MAX_IMAGE_JSON_BYTES),
        'image.json')
    if frozenset(image) != PAYLOAD_IMAGE_KEYS:
        raise RuntimeError('image.json keys do not match')
    if type(image['schemaVersion']) is not int or image['schemaVersion'] != 1:
        raise RuntimeError('image.json schemaVersion must be integer 1')
    if image['platform'] != 'linux-x64' or image['baseOS'] != 'ubuntu-24.04':
        raise RuntimeError('image.json platform or baseOS is unsupported')
    if image['dependenciesInstalled'] is not True:
        raise RuntimeError('image.json does not record installed dependencies')
    if image['runnerUrl'] != runner_url \
            or image['runnerSha256'] != runner_sha256:
        raise RuntimeError('image.json runner pin does not match the runtime')
    if not isinstance(image['toolchainManifestSha256'], str) \
            or not PAYLOAD_SHA256_RE.fullmatch(image['toolchainManifestSha256']):
        raise RuntimeError('image.json toolchain digest is invalid')
    manifest = _load_payload_json(
        _read_payload_file(os.path.join(toolchains, 'manifest.json'),
                           MAX_TOOLCHAIN_JSON_BYTES),
        'toolchain manifest.json')
    if type(manifest.get('schemaVersion')) is not int \
            or manifest.get('schemaVersion') != 1 \
            or manifest.get('platform') != 'linux-x64':
        raise RuntimeError('toolchain manifest schema or platform is unsupported')
    runner_spec = manifest.get('runner')
    if not isinstance(runner_spec, dict) \
            or runner_spec.get('url') != runner_url \
            or not isinstance(runner_spec.get('digest'), str) \
            or runner_spec['digest'].lower() != runner_sha256:
        raise RuntimeError('toolchain manifest runner pin does not match')
    fd = os.open(os.path.join(path, 'runner.tar.gz'),
                 os.O_RDONLY | os.O_NOFOLLOW)
    try:
        archive_info = os.fstat(fd)
        if not stat.S_ISREG(archive_info.st_mode) \
                or archive_info.st_uid != PAYLOAD_UID \
                or archive_info.st_gid != PAYLOAD_GID \
                or stat.S_IMODE(archive_info.st_mode) != 0o444 \
                or archive_info.st_size <= 0 \
                or archive_info.st_size > MAX_RUNNER_ARCHIVE_BYTES:
            raise RuntimeError('runner archive is not a bounded root-owned file')
        hasher = hashlib.sha256()
        total = 0
        while True:
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if total > MAX_RUNNER_ARCHIVE_BYTES:
                raise RuntimeError('runner archive exceeds size limit')
            hasher.update(chunk)
    finally:
        os.close(fd)
    if hasher.hexdigest() != runner_sha256:
        raise RuntimeError('runner archive hash mismatch')
    if _payload_manifest_digest(manifest) != image['toolchainManifestSha256']:
        raise RuntimeError('toolchain manifest digest mismatch')
    return validate_toolchain_dir(toolchains)


def read_token(stream):
    if stream.isatty():
        raise RuntimeError('registration token must be piped on stdin')
    data = stream.read(MAX_TOKEN_BYTES + 4) or b''
    if isinstance(data, str):
        data = data.encode('utf-8')
    try:
        text = data.decode('utf-8')
    except UnicodeDecodeError:
        raise RuntimeError('registration token is not valid UTF-8') from None
    if not text.strip():
        raise RuntimeError('registration token is empty')
    lines = text.splitlines()
    if len(lines) != 1:
        raise RuntimeError('registration token must be a single line')
    token = lines[0].strip()
    if len(token.encode('utf-8')) > MAX_TOKEN_BYTES:
        raise RuntimeError('registration token exceeds 4096 bytes')
    return token


def scrub_parent_env():
    for key in [key for key in os.environ
                if key.startswith('ACTIONS_RUNNER_INPUT_') or key == 'RUNNER_TOOL_CACHE']:
        del os.environ[key]


def child_env(environ, token=None, toolchain_dir=None):
    env = {key: environ[key] for key in BASE_ENV_KEYS if key in environ}
    if toolchain_dir is not None:
        root = os.path.realpath(toolchain_dir)
        bindir = os.path.join(root, 'bin')
        path = environ.get('PATH', '')
        env['PATH'] = bindir + (os.pathsep + path if path else '')
        env['RUNNER_TOOL_CACHE'] = os.path.join(root, 'toolcache')
    if token is not None:
        env['ACTIONS_RUNNER_INPUT_TOKEN'] = token
    return env


def kill_group(proc, signum):
    try:
        os.killpg(proc.pid, signum)
    except OSError:
        pass


def stop_group(proc, signum):
    kill_group(proc, signum)
    grace_end = monotonic() + GRACE_SECONDS
    while proc.poll() is None and monotonic() < grace_end:
        sleep(POLL_SECONDS)
    kill_group(proc, signal.SIGKILL)
    if proc.poll() is None:
        try:
            proc.wait(timeout=GRACE_SECONDS)
        except subprocess.TimeoutExpired:
            pass
    return proc.returncode


def abort_reason(interrupted, deadline):
    signum = interrupted()
    if signum is not None:
        return signum
    if monotonic() >= deadline:
        return 'timeout'
    return None


def wait_phase(proc, deadline, interrupted):
    while proc.poll() is None:
        reason = abort_reason(interrupted, deadline)
        if reason == 'timeout':
            stop_group(proc, signal.SIGTERM)
            print('timed out; terminated runner process group', file=sys.stderr)
            return TIMEOUT_EXIT
        if reason is not None:
            stop_group(proc, reason)
            print(f'interrupted by {signal.Signals(reason).name}; terminated runner process group',
                  file=sys.stderr)
            return 128 + reason
        sleep(POLL_SECONDS)
    kill_group(proc, signal.SIGKILL)
    return proc.returncode if proc.returncode is not None else 1


def run_phase(argv, cwd, env, deadline, interrupted):
    reason = abort_reason(interrupted, deadline)
    if reason == 'timeout':
        print('timed out before starting runner step', file=sys.stderr)
        return TIMEOUT_EXIT
    if reason is not None:
        print(f'interrupted by {signal.Signals(reason).name} before starting runner step',
              file=sys.stderr)
        return 128 + reason
    proc = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                            start_new_session=True)
    return wait_phase(proc, deadline, interrupted)


def execute(args, runner_dir, token, deadline, interrupted, toolchain_dir=None):
    config_argv = [
        os.path.join(runner_dir, 'config.sh'),
        '--unattended', '--ephemeral', '--disableupdate',
        '--url', f'https://github.com/{args.repository}',
        '--name', args.name,
        '--labels', 'canter',
        '--work', '_work',
    ]
    print('configuring ephemeral runner', file=sys.stderr)
    rc = run_phase(config_argv, runner_dir,
                   child_env(os.environ, token, toolchain_dir), deadline, interrupted)
    if rc != 0:
        print(f'runner configuration exited with status {rc}', file=sys.stderr)
        return rc
    print('starting ephemeral runner', file=sys.stderr)
    return run_phase([os.path.join(runner_dir, 'run.sh')], runner_dir,
                     child_env(os.environ, None, toolchain_dir), deadline, interrupted)


def install_handlers(state):
    return {sig: signal.signal(sig, state.handler) for sig in (signal.SIGINT, signal.SIGTERM)}


def restore_handlers(previous):
    for sig, handler in previous.items():
        signal.signal(sig, handler)


def main(argv=None, stdin=None):
    args = parse_args(argv)
    check_host(os.environ)
    runner_dir = validate_runner_dir(args.runner_dir)
    toolchain_dir = validate_toolchain_dir(args.toolchain_dir) if args.toolchain_dir else None
    if stdin is None:
        stream = getattr(sys.stdin, 'buffer', sys.stdin)
    else:
        stream = stdin
    token = read_token(stream)
    scrub_parent_env()
    deadline = monotonic() + args.timeout_seconds
    state = InterruptState()
    previous = install_handlers(state)
    try:
        return execute(args, runner_dir, token, deadline, state, toolchain_dir)
    finally:
        restore_handlers(previous)
        print(STATUS_MESSAGE)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print('interrupted', file=sys.stderr)
        sys.exit(130)
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as exc:
        print(f'Runner launch failed: {exc}', file=sys.stderr)
        sys.exit(1)

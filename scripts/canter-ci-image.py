#!/usr/bin/env python3
"""Plan, build, and verify the immutable prepared Canter CI payload."""
import argparse
import hashlib
import importlib.util
import json
import os
import platform
import pwd
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

_spec = importlib.util.spec_from_file_location(
    'canter_ci_prepare',
    Path(__file__).resolve().with_name('canter-ci-prepare.py'))
prepare = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(prepare)

_spec = importlib.util.spec_from_file_location(
    'canter_ci_runner_payload',
    Path(__file__).resolve().with_name('canter-ci-runner.py'))
runner = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(runner)

PREPARED_IMAGE_ROOT = '/opt/canter-ci-image'
PAYLOAD_UID = 0
PAYLOAD_GID = 0
PLATFORM = 'linux-x64'
BASE_OS = 'ubuntu-24.04'
MAX_BUILD_SECONDS = 2700
DEPS_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
RUNNER_VERSION_RE = re.compile(
    r'[1-9][0-9]{0,3}\.(?:0|[1-9][0-9]{0,3})\.(?:0|[1-9][0-9]{0,3})')
NOTE = 'prepared CI payload only; not a sanitized bootable image'
MANIFEST_LIMIT = 65536
DEPS_CLEANUP_SECONDS = 5


def _canonical_bytes(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'),
                      ensure_ascii=True) + '\n').encode('ascii')


def _manifest_digest(manifest):
    return hashlib.sha256(_canonical_bytes(manifest)).hexdigest()


def _expected_pin(tool, version):
    if tool == 'go':
        return ('https://dl.google.com/go/go' + version + '.linux-amd64.tar.gz',
                'sha256', 'go')
    if tool == 'node':
        return ('https://nodejs.org/dist/v' + version + '/node-v' + version +
                '-linux-x64.tar.gz', 'sha256',
                'node-v' + version + '-linux-x64')
    if tool == 'pnpm':
        return ('https://registry.npmjs.org/pnpm/-/pnpm-' + version + '.tgz',
                'sha512', 'package')
    return ('https://github.com/actions/runner/releases/download/v' + version +
            '/actions-runner-linux-x64-' + version + '.tar.gz',
            'sha256', '')


def _strict_manifest(path):
    try:
        with open(path, 'rb') as src:
            raw = src.read(MANIFEST_LIMIT + 1)
    except OSError:
        raise RuntimeError('cannot read toolchain manifest') from None
    if len(raw) > MANIFEST_LIMIT:
        raise RuntimeError('toolchain manifest is too large')
    manifest = runner._load_payload_json(raw, 'toolchain manifest')
    if type(manifest.get('schemaVersion')) is not int:
        raise ValueError('manifest schemaVersion must be integer 1')
    normalized = prepare.validate_manifest(manifest)
    for tool in prepare.EXPECTED_TOOLS:
        spec = normalized[tool]
        version = spec['version']
        if tool == 'runner' and not RUNNER_VERSION_RE.fullmatch(version):
            raise ValueError('runner version is not an official release')
        url, algorithm, strip = _expected_pin(tool, version)
        if spec['url'] != url or spec['algorithm'] != algorithm \
                or spec['stripPrefix'] != strip:
            raise ValueError(tool + ' does not match the pinned official source')
        spec['digest'] = spec['digest'].lower()
    return normalized


def _plan_data(manifest_path):
    normalized = _strict_manifest(manifest_path)
    runner_spec = normalized['runner']
    return {
        'schemaVersion': 1,
        'payloadPrefix': PREPARED_IMAGE_ROOT,
        'runnerUrl': runner_spec['url'],
        'runnerSha256': runner_spec['digest'],
        'toolchainManifestSha256': _manifest_digest(normalized),
        'requiresFreshDisposableVm': True,
        'createsProviderResources': False,
        'sanitizesWholeMachine': False,
    }


def _os_release(path='/etc/os-release'):
    values = {}
    with open(path, 'r', encoding='utf-8') as src:
        for line in src:
            line = line.strip()
            if not line or line.startswith('#') or '=' not in line:
                continue
            key, _, value = line.partition('=')
            value = value.strip()
            if len(value) >= 2 and value[0] == '"' and value[-1] == '"':
                value = value[1:-1]
            values[key.strip()] = value
    return values


def _require_build_host():
    if platform.system() != 'Linux' \
            or platform.machine().lower() not in ('x86_64', 'amd64'):
        raise RuntimeError('payload build requires Linux x86_64')
    if os.geteuid() != 0:
        raise RuntimeError('payload build requires root')
    release = _os_release()
    if release.get('ID') != 'ubuntu' or release.get('VERSION_ID') != '24.04':
        raise RuntimeError('payload build requires Ubuntu 24.04')


def _require_builder_account(getpwnam):
    try:
        account = getpwnam('canter-ci')
    except KeyError:
        raise RuntimeError('dedicated canter-ci account is required') from None
    if account.pw_uid < 1000 or account.pw_gid == 0:
        raise RuntimeError('invalid guest account')
    return account


def _deadline_seconds(deadline_unix_ms):
    if type(deadline_unix_ms) is not int:
        raise ValueError('deadline must be integer unix milliseconds')
    remaining = deadline_unix_ms / 1000.0 - time.time()
    if remaining <= 0:
        raise RuntimeError('build deadline already passed')
    if remaining > MAX_BUILD_SECONDS:
        raise RuntimeError('build deadline exceeds 45 minutes')
    return remaining


def _require_fresh_prefix(prefix):
    prefix = os.path.abspath(prefix)
    parent = os.path.dirname(prefix)
    if os.path.islink(parent) or not os.path.isdir(parent):
        raise RuntimeError('payload parent directory is unavailable')
    info = os.lstat(parent)
    if (info.st_uid != PAYLOAD_UID or info.st_gid != PAYLOAD_GID or
            info.st_mode & 0o022):
        raise RuntimeError('payload parent directory is not secure')
    if os.path.lexists(prefix):
        raise RuntimeError('payload prefix already exists')
    return prefix


def _no_proxy_opener():
    return urllib.request.build_opener(
        urllib.request.ProxyHandler({}), prepare._HttpsOnlyRedirect)


class _CacheOpener:
    def __init__(self, archive_path, runner_url, opener):
        self._archive_path = archive_path
        self._runner_url = runner_url
        self._opener = opener

    def open(self, request, timeout=None):
        if request.full_url == self._runner_url:
            return open(self._archive_path, 'rb')
        return self._opener.open(request, timeout=timeout)


def _reap_process_group(proc):
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    except OSError:
        raise RuntimeError('dependency process cleanup failed')
    try:
        proc.wait(timeout=DEPS_CLEANUP_SECONDS)
    except subprocess.TimeoutExpired:
        raise RuntimeError('dependency process cleanup timed out')


def _run_dependency_installer(toolchains, deadline_mono, popen):
    script = os.path.join(toolchains, 'runner', 'bin', 'installdependencies.sh')
    env = {'PATH': DEPS_PATH, 'HOME': '/root', 'LANG': 'C.UTF-8',
           'DEBIAN_FRONTEND': 'noninteractive'}
    if deadline_mono - time.monotonic() <= 0:
        raise TimeoutError('dependency installation deadline exceeded')
    proc = None
    recorded = []

    def record(signum, _frame):
        recorded.append(signum)

    previous = {}
    try:
        for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGALRM):
            previous[sig] = signal.signal(sig, record)
        try:
            proc = popen([script], cwd=os.path.join(toolchains, 'runner'),
                         env=env, stdin=subprocess.DEVNULL,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         start_new_session=True, close_fds=True)
        finally:
            for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGALRM):
                signal.signal(sig, previous[sig])
        if recorded:
            if signal.SIGALRM in recorded:
                raise TimeoutError('payload build deadline expired')
            raise InterruptedError('payload build interrupted')
        if deadline_mono - time.monotonic() <= 0:
            raise TimeoutError('dependency installation deadline exceeded')
        code = proc.wait(timeout=deadline_mono - time.monotonic())
    except subprocess.TimeoutExpired:
        raise RuntimeError('dependency installation timed out') from None
    finally:
        if proc is not None:
            _reap_process_group(proc)
    if code != 0:
        raise RuntimeError('dependency installation failed')


def _write_image_json(stage, normalized):
    runner_spec = normalized['runner']
    image = {
        'schemaVersion': 1,
        'platform': PLATFORM,
        'baseOS': BASE_OS,
        'runnerUrl': runner_spec['url'],
        'runnerSha256': runner_spec['digest'],
        'toolchainManifestSha256': _manifest_digest(normalized),
        'dependenciesInstalled': True,
    }
    with open(os.path.join(stage, 'image.json'), 'x', encoding='ascii') as out:
        out.write(_canonical_bytes(image).decode('ascii'))


def _seal_payload(stage, chown, chmod):
    root_real = os.path.realpath(stage)
    for dirpath, dirnames, filenames in os.walk(stage, followlinks=False):
        for name in dirnames + filenames:
            path = os.path.join(dirpath, name)
            info = os.lstat(path)
            chown(path, 0, 0, follow_symlinks=False)
            if stat.S_ISLNK(info.st_mode):
                target = os.readlink(path)
                if not target or os.path.isabs(target):
                    raise RuntimeError('payload link must be relative')
                resolved = os.path.realpath(path)
                if os.path.commonpath((root_real, resolved)) != root_real:
                    raise RuntimeError('payload link escapes the prefix')
                continue
            if stat.S_ISDIR(info.st_mode):
                chmod(path, 0o555)
            elif stat.S_ISREG(info.st_mode):
                chmod(path, 0o555 if info.st_mode & 0o111 else 0o444)
            else:
                raise RuntimeError('unexpected payload entry type')
    chown(stage, 0, 0)
    chmod(stage, 0o555)


def _build(manifest_path, deadline_unix_ms, ack, getpwnam=pwd.getpwnam,
           chown=os.chown, chmod=os.chmod, opener=None, popen=subprocess.Popen):
    if not ack:
        raise RuntimeError('--ack-disposable-vm is required')
    _require_build_host()
    _require_builder_account(getpwnam)
    seconds = _deadline_seconds(deadline_unix_ms)
    normalized = _strict_manifest(manifest_path)
    prefix = _require_fresh_prefix(PREPARED_IMAGE_ROOT)
    deadline_mono = time.monotonic() + seconds
    staging = tempfile.TemporaryDirectory(
        prefix='.canter-ci-image-', dir=os.path.dirname(prefix))
    stage = os.path.realpath(staging.name)
    try:
        archive = os.path.join(stage, 'runner.tar.gz')
        spec = normalized['runner']
        network = opener or _no_proxy_opener()
        prepare.download_tool('runner', spec, archive, network,
                              time.monotonic() + seconds)
        prepare.verify_digest(archive, spec['algorithm'], spec['digest'])
        cache = _CacheOpener(archive, spec['url'], network)
        toolchains = os.path.join(stage, 'toolchains')
        prepare.prepare(normalized, toolchains, 'canter-ci', opener=cache,
                        chown=chown, getpwnam=getpwnam)
        _run_dependency_installer(toolchains, deadline_mono, popen)
        _write_image_json(stage, normalized)
        _seal_payload(stage, chown, chmod)
        runner.validate_prepared_image(stage, spec['url'], spec['digest'])
        prepare._publish_no_replace(stage, prefix)
    finally:
        if os.path.lexists(stage):
            try:
                for dirpath, _dirnames, _filenames in os.walk(stage):
                    os.chmod(dirpath, 0o700)
            except OSError:
                pass
            try:
                staging.cleanup()
            except OSError:
                pass
        else:
            finalizer = getattr(staging, '_finalizer', None)
            if finalizer is not None:
                finalizer.detach()
    return {
        'schemaVersion': 1,
        'payloadPrefix': prefix,
        'runnerUrl': spec['url'],
        'runnerSha256': spec['digest'],
        'toolchainManifestSha256': _manifest_digest(normalized),
        'dependenciesInstalled': True,
        'note': NOTE,
    }


def _build_signal(signum, _frame):
    if signum == signal.SIGALRM:
        raise TimeoutError('payload build deadline expired')
    raise InterruptedError('payload build interrupted')


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog='canter-ci-image',
        description='Plan, build, and verify the immutable prepared Canter CI payload.')
    commands = parser.add_subparsers(dest='command', required=True)
    for name in ('plan', 'verify'):
        sub = commands.add_parser(name)
        sub.add_argument('--manifest', default=str(prepare.DEFAULT_MANIFEST))
    build = commands.add_parser('build')
    build.add_argument('--manifest', default=str(prepare.DEFAULT_MANIFEST))
    build.add_argument('--deadline-unix-ms', type=int, required=True)
    build.add_argument('--ack-disposable-vm', action='store_true')
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(sys.argv[1:] if argv is None else argv)
    try:
        if args.command == 'plan':
            print(json.dumps(_plan_data(args.manifest), indent=2, sort_keys=True))
            return 0
        if args.command == 'verify':
            data = _plan_data(args.manifest)
            toolchain = runner.validate_prepared_image(
                PREPARED_IMAGE_ROOT, data['runnerUrl'], data['runnerSha256'])
            actual = runner._load_payload_json(
                runner._read_payload_file(
                    os.path.join(toolchain, 'manifest.json'),
                    runner.MAX_TOOLCHAIN_JSON_BYTES),
                'toolchain manifest.json')
            if _manifest_digest(actual) != data['toolchainManifestSha256']:
                raise RuntimeError(
                    'prepared payload does not match requested toolchain manifest')
            print(json.dumps({
                'schemaVersion': 1,
                'payloadPrefix': PREPARED_IMAGE_ROOT,
                'toolchainPrefix': toolchain,
                'runnerUrl': data['runnerUrl'],
                'runnerSha256': data['runnerSha256'],
                'toolchainManifestSha256': data['toolchainManifestSha256'],
                'dependenciesInstalled': True,
                'note': NOTE,
            }, indent=2, sort_keys=True))
            return 0
        seconds = _deadline_seconds(args.deadline_unix_ms)
        if signal.getitimer(signal.ITIMER_REAL) != (0.0, 0.0):
            raise RuntimeError('build timer already active')
        previous = {}
        for sig in (signal.SIGALRM, signal.SIGINT, signal.SIGTERM):
            previous[sig] = signal.signal(sig, _build_signal)
        try:
            signal.setitimer(signal.ITIMER_REAL, seconds)
            result = _build(args.manifest, args.deadline_unix_ms,
                            args.ack_disposable_vm)
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for sig in (signal.SIGALRM, signal.SIGINT, signal.SIGTERM):
                signal.signal(sig, previous[sig])
        print(json.dumps(result, indent=2, sort_keys=True))
        return 0
    except (RuntimeError, OSError, ValueError):
        sys.stderr.write('canter-ci-image: operation failed\n')
        return 1


if __name__ == '__main__':
    sys.exit(main())

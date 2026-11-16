#!/usr/bin/env python3
"""Root-only, one-shot cloud-init template for a dedicated Canter CI VM.

The control plane replaces DATA_JSON with a quoted JSON string in memory.
The unexpanded template is also included in the pinned guest bundle.
"""

import base64
import hashlib
import importlib.util
import io
import json
import math
import os
from pathlib import Path
import pwd
import re
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.parse
import urllib.request


DATA_JSON = "__CANTER_CI_DATA_JSON__"
HOME = Path('/home/canter-ci')
RUNNER = HOME / 'actions-runner'
TOOLS = Path('/opt/canter-ci')
MACHINE = Path('/opt/canter-ci-machine.py')
UNIT = Path('/etc/systemd/system/canter-ci-runner.service')
STATE = Path('/var/lib/canter-ci')
CACHE_COMMAND = Path('/usr/local/bin/canter-ci-cache')
CACHE_COMMAND_BYTES = (
    b'#!/bin/sh\n'
    b'exec /usr/bin/python3 -I /opt/canter-ci/canter-ci-cache.py "$@"\n'
)
REPORT_COMMAND = Path('/usr/local/bin/canter-ci-report')
REPORT_COMMAND_BYTES = (
    b'#!/bin/sh\n'
    b'exec /usr/bin/python3 -I /opt/canter-ci/canter-ci-evidence.py "$@"\n'
)
FIXED_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
PREPARED = Path('/opt/canter-ci-image')
MAX_RUNNER_BYTES = 512 * 1024 * 1024
MAX_BUNDLE_FILE_BYTES = 256 * 1024
MAX_BUNDLE_TOTAL_BYTES = 1024 * 1024
BUNDLE_NAMES = frozenset((
    'canter-ci-runner.py', 'canter-ci-jit-runner.py',
    'canter-ci-admission-client.py', 'canter-ci-guest-bootstrap.py',
    'canter-ci-machine-init.py', 'canter-ci-cache.py',
    'canter-ci-evidence.py',
))
CONFIG_KEYS = frozenset((
    'version', 'bundleBase64', 'bundleSha256', 'runnerUrl', 'runnerSha256',
    'guestArguments', 'bootstrapToken', 'setupDeadlineUnixMs',
    'hardDeadlineUnixMs',
))
ARCHIVE_URL = re.compile(
    r'https://github\.com/actions/runner/releases/download/v'
    r'([1-9][0-9]{0,3}\.(?:0|[1-9][0-9]{0,3})\.(?:0|[1-9][0-9]{0,3}))'
    r'/actions-runner-linux-x64-\1\.tar\.gz')


def configuration():
    data = json.loads(DATA_JSON)
    if not isinstance(data, dict) or set(data) != CONFIG_KEYS:
        raise ValueError('invalid machine configuration')
    if type(data['version']) is not int or data['version'] != 1:
        raise ValueError('invalid machine configuration')
    for key in ('runnerSha256', 'bundleSha256'):
        if not isinstance(data[key], str) or not re.fullmatch(r'[0-9a-f]{64}', data[key]):
            raise ValueError('invalid machine configuration')
    if not isinstance(data['runnerUrl'], str) or not ARCHIVE_URL.fullmatch(data['runnerUrl']):
        raise ValueError('invalid machine configuration')
    for key in ('setupDeadlineUnixMs', 'hardDeadlineUnixMs'):
        if type(data[key]) is not int or not 1 <= data[key] <= 2**63 - 1:
            raise ValueError('invalid machine configuration')
    if data['hardDeadlineUnixMs'] <= data['setupDeadlineUnixMs']:
        raise ValueError('invalid machine configuration')
    token = data['bootstrapToken']
    if (not isinstance(token, str) or not 1 <= len(token) <= 4096 or
            not token.isascii() or any(c.isspace() for c in token)):
        raise ValueError('invalid machine configuration')
    args = data['guestArguments']
    if not isinstance(args, list) or not args or any(not isinstance(v, str) for v in args):
        raise ValueError('invalid machine configuration')
    if '--token-fd' in args:
        raise ValueError('invalid machine configuration')
    return data


def remaining(data, key):
    value = data[key] / 1000.0 - time.time()
    if value <= 0:
        raise TimeoutError('machine deadline expired')
    return value


def child_environment(toolchain_dir=None):
    env = {'PATH': FIXED_PATH, 'HOME': str(HOME), 'LANG': 'C.UTF-8',
           'USER': 'canter-ci', 'LOGNAME': 'canter-ci'}
    if toolchain_dir is not None:
        env['PATH'] = os.path.join(toolchain_dir, 'bin') + os.pathsep + FIXED_PATH
    return env


def _prepared_toolchain(data):
    if not os.path.lexists(PREPARED):
        return None
    spec = importlib.util.spec_from_file_location(
        'canter_ci_runner_payload', TOOLS / 'canter-ci-runner.py')
    legacy = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(legacy)
    return legacy.validate_prepared_image(
        str(PREPARED), data['runnerUrl'], data['runnerSha256'])


def install_bundle(data):
    blob = base64.b64decode(data['bundleBase64'], validate=True)
    if hashlib.sha256(blob).hexdigest() != data['bundleSha256']:
        raise ValueError('guest bundle hash mismatch')
    TOOLS.mkdir(mode=0o755)
    seen, total = set(), 0
    with tarfile.open(fileobj=io.BytesIO(blob), mode='r:gz') as archive:
        for member in archive:
            if (member.name not in BUNDLE_NAMES or member.name in seen or
                    not member.isfile() or member.size < 0 or
                    member.size > MAX_BUNDLE_FILE_BYTES):
                raise ValueError('invalid guest bundle')
            total += member.size
            if total > MAX_BUNDLE_TOTAL_BYTES:
                raise ValueError('guest bundle too large')
            seen.add(member.name)
            stream = archive.extractfile(member)
            if stream is None:
                raise ValueError('invalid guest bundle')
            with stream:
                content = stream.read(MAX_BUNDLE_FILE_BYTES + 1)
            if len(content) != member.size:
                raise ValueError('invalid guest bundle')
            fd = os.open(TOOLS / member.name,
                         os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o600)
            with os.fdopen(fd, 'wb') as target:
                target.write(content)
                target.flush()
                os.fchmod(target.fileno(), 0o555)
    if seen != BUNDLE_NAMES:
        raise ValueError('guest bundle is incomplete')
    TOOLS.chmod(0o555)


def _install_wrapper(path, data):
    # Root-owned fixed wrapper; refuses to overwrite anything already there.
    fd = os.open(path,
                 os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                 0o600)
    with os.fdopen(fd, 'wb') as target:
        target.write(data)
        target.flush()
        os.fchmod(target.fileno(), 0o555)


def install_cache_command():
    _install_wrapper(CACHE_COMMAND, CACHE_COMMAND_BYTES)


def install_report_command():
    _install_wrapper(REPORT_COMMAND, REPORT_COMMAND_BYTES)


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if urllib.parse.urlsplit(newurl).scheme.lower() != 'https':
            raise ValueError('runner download redirect refused')
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def install_runner(data):
    toolchain = _prepared_toolchain(data)
    RUNNER.mkdir(mode=0o700)
    if toolchain is not None:
        remaining(data, 'setupDeadlineUnixMs')
        fd = os.open(PREPARED / 'runner.tar.gz',
                     os.O_RDONLY | os.O_NOFOLLOW)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or
                    info.st_gid != 0 or info.st_size <= 0 or
                    info.st_size > MAX_RUNNER_BYTES or
                    stat.S_IMODE(info.st_mode) != 0o444):
                raise ValueError('invalid prepared runner archive')
            source = os.fdopen(fd, 'rb')
        except Exception:
            os.close(fd)
            raise
        with source:
            with tarfile.open(fileobj=source, mode='r:gz') as bundle:
                bundle.extractall(path=RUNNER, filter='data')
        remaining(data, 'setupDeadlineUnixMs')
        return
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), HTTPSRedirect())
    digest, total = hashlib.sha256(), 0
    end = time.monotonic() + remaining(data, 'setupDeadlineUnixMs')
    with tempfile.TemporaryFile(dir=STATE) as archive:
        with opener.open(data['runnerUrl'], timeout=min(10, remaining(data, 'setupDeadlineUnixMs'))) as response:
            while True:
                if time.monotonic() >= end:
                    raise TimeoutError('runner download timed out')
                block = response.read(1024 * 1024)
                if not block:
                    break
                total += len(block)
                if total > MAX_RUNNER_BYTES:
                    raise ValueError('runner archive too large')
                digest.update(block)
                archive.write(block)
        if digest.hexdigest() != data['runnerSha256']:
            raise ValueError('runner archive hash mismatch')
        archive.seek(0)
        with tarfile.open(fileobj=archive, mode='r:gz') as source:
            source.extractall(path=RUNNER, filter='data')
    proc = subprocess.Popen(
        [str(RUNNER / 'bin' / 'installdependencies.sh')], cwd=RUNNER,
        env=dict(child_environment(), DEBIAN_FRONTEND='noninteractive'),
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL, start_new_session=True, close_fds=True)
    try:
        if proc.wait(timeout=remaining(data, 'setupDeadlineUnixMs')) != 0:
            raise RuntimeError('runner dependencies failed')
    finally:
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.wait()


def service_unit(runtime_seconds):
    return f"""[Unit]
Description=Canter CI ephemeral guest supervisor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
ExecStart=/usr/bin/python3 -I /opt/canter-ci-machine.py launch
Restart=no
KillMode=control-group
TimeoutStopSec=5
RuntimeMaxSec={runtime_seconds}
UMask=0077
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
"""


def install(data):
    remaining(data, 'setupDeadlineUnixMs')
    account = pwd.getpwnam('canter-ci')
    if account.pw_uid < 1000 or account.pw_gid == 0:
        raise ValueError('invalid guest account')
    HOME.chmod(0o700)
    STATE.mkdir(mode=0o700)
    install_bundle(data)
    install_cache_command()
    install_report_command()
    install_runner(data)
    for parent, dirs, files in os.walk(RUNNER, followlinks=False):
        os.chown(parent, account.pw_uid, account.pw_gid, follow_symlinks=False)
        for name in dirs + files:
            os.chown(os.path.join(parent, name), account.pw_uid,
                     account.pw_gid, follow_symlinks=False)
    os.chown(HOME, account.pw_uid, account.pw_gid, follow_symlinks=False)
    runtime = min(22500, math.floor(remaining(data, 'hardDeadlineUnixMs')))
    if runtime < 1:
        raise TimeoutError('machine deadline expired')
    with UNIT.open('x', encoding='ascii') as target:
        target.write(service_unit(runtime))
    subprocess.run(['/usr/bin/systemctl', 'daemon-reload'], check=True,
                   timeout=min(10, remaining(data, 'setupDeadlineUnixMs')),
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(['/usr/bin/systemctl', 'enable', '--now', UNIT.name], check=True,
                   timeout=min(10, remaining(data, 'setupDeadlineUnixMs')),
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def launch(data):
    account = pwd.getpwnam('canter-ci')
    if account.pw_uid < 1000 or account.pw_gid == 0:
        raise ValueError('invalid guest account')
    remaining(data, 'setupDeadlineUnixMs')
    budget = min(22500, remaining(data, 'hardDeadlineUnixMs'))
    hard_mono = time.monotonic() + budget
    toolchain = _prepared_toolchain(data)
    remaining(data, 'setupDeadlineUnixMs')
    # Remove only the private launch file created for this VM, never a repo
    # source path or a symlink supplied by another process.
    info = MACHINE.lstat()
    if (Path(__file__).resolve() != MACHINE or not stat.S_ISREG(info.st_mode) or
            info.st_uid != 0 or info.st_mode & 0o077):
        raise ValueError('invalid private launch file')
    MACHINE.unlink()
    read_fd, write_fd = os.pipe()
    proc = None
    try:
        argv = ['/usr/bin/python3', '-I', str(TOOLS / 'canter-ci-guest-bootstrap.py'),
                *data['guestArguments'], '--token-fd', str(read_fd)]
        proc = subprocess.Popen(
            argv, user=account.pw_uid, group=account.pw_gid, extra_groups=[],
            cwd=HOME, env=child_environment(toolchain), pass_fds=(read_fd,),
            close_fds=True, stdin=subprocess.DEVNULL)
        os.close(read_fd)
        read_fd = -1
        payload = data['bootstrapToken'].encode('ascii')
        while payload:
            payload = payload[os.write(write_fd, payload):]
        os.close(write_fd)
        write_fd = -1
        wait_budget = min(hard_mono - time.monotonic(),
                          remaining(data, 'hardDeadlineUnixMs'))
        if wait_budget <= 0:
            raise TimeoutError('machine deadline expired')
        return proc.wait(timeout=wait_budget)
    finally:
        for fd in (read_fd, write_fd):
            if fd >= 0:
                os.close(fd)
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=2)


def main(argv=None):
    args = sys.argv[1:] if argv is None else argv
    try:
        if os.geteuid() != 0 or args not in (['install'], ['launch']):
            return 1
        data = configuration()
        if args == ['install']:
            if signal.getitimer(signal.ITIMER_REAL) != (0.0, 0.0):
                raise RuntimeError('machine timer already active')
            previous = signal.signal(signal.SIGALRM, install_deadline)
            try:
                signal.setitimer(signal.ITIMER_REAL, remaining(data, 'setupDeadlineUnixMs'))
                install(data)
            finally:
                signal.setitimer(signal.ITIMER_REAL, 0)
                signal.signal(signal.SIGALRM, previous)
            return 0
        return launch(data)
    except Exception:
        sys.stderr.write('canter-ci-machine: bootstrap failed\n')
        return 1


def install_deadline(signum, frame):
    raise TimeoutError('machine setup deadline expired')


if __name__ == '__main__':
    sys.exit(main())

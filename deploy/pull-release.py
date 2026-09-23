#!/usr/bin/env python3
"""Fetch CI-published production releases without inbound deployment access."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
import time
import urllib.request

ROOT = Path('/opt/canter')
STATE = Path('/var/lib/canter-deploy')
REPO = 'canter0/canter'
HARNESS = Path('/opt/canter-harness')
SYSTEMD = Path('/etc/systemd/system')
HARNESS_UNITS = ('canter-harness.socket', 'canter-harness@.service')

def run(*args):
    return subprocess.run(args, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

def get(url):
    request = urllib.request.Request(url, headers={'User-Agent': 'canter-production-deploy'})
    with urllib.request.urlopen(request, timeout=60) as response:
        return response.read()

def healthy(sha=None):
    try:
        if json.loads(get('http://127.0.0.1:8081/readyz'))['status'] != 'ready':
            return False
        get('http://127.0.0.1:3000/sign-in')
        return sha is None or json.loads(get('http://127.0.0.1:3000/release.json'))['commit'] == sha
    except Exception:
        return False

def install_binary(source):
    target = ROOT / 'bin/canter-controlplane'
    temporary = target.with_suffix('.pending')
    shutil.copy2(source, temporary)
    os.chmod(temporary, 0o755)
    temporary.replace(target)

def unit_state(unit, state):
    return subprocess.run(['systemctl', state, '--quiet', unit],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def install_file(source, target, mode=0o755):
    temporary = target.with_suffix('.pending')
    shutil.copy2(source, temporary)
    temporary.chmod(mode)
    temporary.replace(target)


def runtime_snapshot(release):
    current = HARNESS / 'current'
    if current.exists() and not current.is_symlink():
        raise RuntimeError('Harness current path must be a managed symlink')
    previous_static = release / 'previous-static'
    static = ROOT / 'bin/canter-static-linux'
    if static.exists():
        shutil.copy2(static, previous_static)
    return {
        'target': os.readlink(current) if current.is_symlink() else None,
        'units': {name: (SYSTEMD / name).read_bytes() if (SYSTEMD / name).exists() else None
                  for name in HARNESS_UNITS},
        'enabled': unit_state('canter-harness.socket', 'is-enabled'),
        'active': unit_state('canter-harness.socket', 'is-active'),
        'static': previous_static if previous_static.exists() else None,
    }


def point_harness(target):
    temporary = HARNESS / 'current.pending'
    temporary.unlink(missing_ok=True)
    temporary.symlink_to(target)
    temporary.replace(HARNESS / 'current')


def stop_runtime():
    # A first deployment has no socket unit yet. Existing accepted connections
    # can outlive a stopped listener, so also stop all running instances.
    if unit_state('canter-harness.socket', 'is-active'):
        run('systemctl', 'stop', 'canter-harness.socket')
    run('systemctl', 'stop', 'canter-harness@*.service')


def install_runtime(release, sha):
    # Dependencies are installed from the lockfile on the CI Linux runner;
    # activation never runs npm or package lifecycle scripts as root.
    for path in [release / 'canter-static-linux', release / 'harness/runner.mjs',
                 release / 'harness/node_modules/just-bash/package.json',
                 *(release / 'deploy' / name for name in HARNESS_UNITS)]:
        if not path.is_file():
            raise RuntimeError('Release is missing a required runtime artifact: ' + path.name)
    HARNESS.mkdir(mode=0o755, exist_ok=True)
    (HARNESS / 'releases').mkdir(mode=0o755, exist_ok=True)
    target = HARNESS / 'releases' / sha
    if target.exists():
        raise RuntimeError('Harness release already exists; inspect before retrying')
    shutil.copytree(release / 'harness', target, symlinks=True)
    # DynamicUser can read only this public, immutable runtime, not /opt/canter.
    for path in [HARNESS, HARNESS / 'releases', target, *target.rglob('*')]:
        if not path.is_symlink():
            path.chmod(path.stat().st_mode | (0o055 if path.is_dir() else 0o044))
    stop_runtime()
    point_harness(target)
    for name in HARNESS_UNITS:
        install_file(release / 'deploy' / name, SYSTEMD / name, 0o644)
    install_file(release / 'canter-static-linux', ROOT / 'bin/canter-static-linux')
    run('systemctl', 'daemon-reload')
    run('systemctl', 'enable', '--now', 'canter-harness.socket')


def restore_runtime(previous):
    stop_runtime()
    if previous['target'] is None:
        (HARNESS / 'current').unlink(missing_ok=True)
    else:
        point_harness(previous['target'])
    for name, content in previous['units'].items():
        target = SYSTEMD / name
        if content is None:
            target.unlink(missing_ok=True)
        else:
            target.write_bytes(content)
            target.chmod(0o644)
    static = ROOT / 'bin/canter-static-linux'
    if previous['static'] is None:
        static.unlink(missing_ok=True)
    else:
        install_file(previous['static'], static)
    # A deleted first-install unit can no longer be disabled with systemctl.
    if not previous['enabled']:
        (SYSTEMD / 'sockets.target.wants/canter-harness.socket').unlink(missing_ok=True)
    run('systemctl', 'daemon-reload')
    if previous['active']:
        run('systemctl', 'start', 'canter-harness.socket')


def activate(release, sha):
    previous_web = release / 'previous-web'
    previous_binary = release / 'previous-controlplane'
    shutil.copy2(ROOT / 'bin/canter-controlplane', previous_binary)
    # Keep a verified database backup before any new executable can migrate it.
    backup = release / 'database.dump'
    with backup.open('wb') as output:
        subprocess.run(['sudo', '-u', 'postgres', 'pg_dump', '-Fc', 'canter'], stdout=output, check=True)
    run('pg_restore', '--list', str(backup))
    database = 'canter_deploy_verify_' + sha[:12]
    run('sudo', '-u', 'postgres', 'createdb', database)
    try:
        with backup.open('rb') as source:
            subprocess.run(['sudo', '-u', 'postgres', 'pg_restore', '--exit-on-error', '-d', database], stdin=source, check=True)
    finally:
        run('sudo', '-u', 'postgres', 'dropdb', database)
    runtime = runtime_snapshot(release)
    run('chown', '-R', 'canter:canter', str(release / 'web'))
    switched = False
    runtime_changed = False
    try:
        run('systemctl', 'stop', 'canter-web', 'canter-controlplane')
        runtime_changed = True
        install_runtime(release, sha)
        (ROOT / 'web').rename(previous_web)
        switched = True
        (release / 'web').rename(ROOT / 'web')
        install_binary(release / 'canter-controlplane')
        run('systemctl', 'restart', 'canter-controlplane', 'canter-web')
        for _ in range(60):
            if healthy(sha):
                (STATE / 'current').write_text(sha)
                print('Activated', sha, flush=True)
                return
            time.sleep(2)
        raise RuntimeError('Release failed readiness checks')
    except Exception:
        run('systemctl', 'stop', 'canter-web', 'canter-controlplane')
        if switched:
            if (ROOT / 'web').exists():
                (ROOT / 'web').rename(release / 'failed-web')
            previous_web.rename(ROOT / 'web')
        install_binary(previous_binary)
        if runtime_changed:
            restore_runtime(runtime)
        run('systemctl', 'start', 'canter-controlplane', 'canter-web')
        (STATE / 'failed').write_text(sha)
        # Additive migrations remain; restoring a live DB would discard new writes.
        raise

def main():
    STATE.mkdir(mode=0o700, exist_ok=True)
    with (STATE / 'lock').open('w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        release = json.loads(get(f'https://api.github.com/repos/{REPO}/releases/latest'))
        match = re.fullmatch(r'production-([0-9a-f]{40})', release['tag_name'])
        if not match or release['draft'] or release['prerelease']:
            raise RuntimeError('Latest release is not a production release')
        sha = match[1]
        if any(p.exists() and p.read_text().strip() == sha for p in [STATE / 'current', STATE / 'failed']):
            return
        assets = {a['name']: a['browser_download_url'] for a in release['assets']}
        base = f'https://github.com/{REPO}/releases/download/{release["tag_name"]}/'
        for name in ['canter-release.tar.gz', 'SHA256SUMS']:
            if assets.get(name) != base + name:
                raise RuntimeError('Missing or unexpected release asset URL')
        expected = get(assets['SHA256SUMS']).decode().strip()
        if not re.fullmatch(r'[0-9a-f]{64}  canter-release.tar.gz', expected):
            raise RuntimeError('Invalid checksum manifest')
        data = get(assets['canter-release.tar.gz'])
        if hashlib.sha256(data).hexdigest() != expected.split()[0]:
            raise RuntimeError('Release checksum mismatch')
        releases = ROOT / 'releases'
        releases.mkdir(exist_ok=True)
        path = releases / ('actions-' + sha)
        if path.exists():
            raise RuntimeError('Incomplete release directory exists; inspect before retrying')
        path.mkdir(mode=0o700)
        with tempfile.TemporaryFile() as archive:
            archive.write(data)
            archive.seek(0)
            with tarfile.open(fileobj=archive) as tar:
                tar.extractall(path, filter='data')
        if json.loads((path / 'web/public/release.json').read_text())['commit'] != sha:
            raise RuntimeError('Artifact commit does not match tag')
        activate(path, sha)

if __name__ == '__main__':
    main()

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
    run('chown', '-R', 'canter:canter', str(release / 'web'))
    run('systemctl', 'stop', 'canter-web')
    switched = False
    try:
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

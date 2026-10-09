#!/usr/bin/env python3
"""Fetch CI-published production releases without inbound deployment access."""
import fcntl
import gzip
import grp
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
import time
import urllib.request
import zlib

ROOT = Path('/opt/canter')
STATE = Path('/var/lib/canter-deploy')
REPO = 'canter0/canter'
HARNESS = Path('/opt/canter-harness')
SYSTEMD = Path('/etc/systemd/system')
WEB_USER = 'canter-web'
WEB_GROUP = 'canter-web'
HARNESS_UNITS = ('canter-harness.socket', 'canter-harness@.service')
RUNTIME_UNITS = (*HARNESS_UNITS, 'canter-web.service')
DOWNLOAD_CHUNK_SIZE = 1024 * 1024
MAX_ARCHIVE_BYTES = 1024 * 1024 * 1024
MAX_ARCHIVE_MEMBERS = 100_000
MAX_EXPANDED_BYTES = 2 * 1024 * 1024 * 1024
MAX_TAR_METADATA_BYTES = 1024 * 1024
TAR_READ_SIZE = 10 * 1024

def run(*args):
    return subprocess.run(args, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

def get(url):
    request = urllib.request.Request(url, headers={'User-Agent': 'canter-production-deploy'})
    with urllib.request.urlopen(request, timeout=60) as response:
        return response.read()

def download(url, output):
    """Stream a release asset to disk while hashing it with bounded memory."""
    request = urllib.request.Request(url, headers={'User-Agent': 'canter-production-deploy'})
    digest = hashlib.sha256()
    total = 0
    with urllib.request.urlopen(request, timeout=60) as response:
        while chunk := response.read(DOWNLOAD_CHUNK_SIZE):
            total += len(chunk)
            if total > MAX_ARCHIVE_BYTES:
                raise RuntimeError('Release archive exceeds the compressed size limit')
            output.write(chunk)
            digest.update(chunk)
    return digest.hexdigest()


class BoundedTarInfo(tarfile.TarInfo):
    """Stop oversized extension records before tarfile reads their bodies."""
    def _proc_pax(self, tarfile):
        if self.size > MAX_TAR_METADATA_BYTES:
            raise RuntimeError('Release archive metadata record is too large')
        return super()._proc_pax(tarfile)

    def _proc_gnulong(self, tarfile):
        if self.size > MAX_TAR_METADATA_BYTES:
            raise RuntimeError('Release archive metadata record is too large')
        return super()._proc_gnulong(tarfile)


class BoundedTarReader:
    """Limit bytes produced by gzip, including headers and block padding."""
    def __init__(self, source):
        self.source = source
        self.total = 0

    def read(self, size=-1):
        size = TAR_READ_SIZE if size is None or size < 0 else min(size, TAR_READ_SIZE)
        remaining = MAX_EXPANDED_BYTES - self.total
        data = self.source.read(min(size, remaining + 1))
        if len(data) > remaining:
            raise RuntimeError('Release archive exceeds the expanded size limit')
        self.total += len(data)
        return data


class BoundedTar:
    """Context helper for a streaming gzip tar reader with parser limits."""
    def __init__(self, archive):
        self.archive = archive

    def __enter__(self):
        self.archive.seek(0)
        self.gzip = gzip.GzipFile(fileobj=self.archive, mode='rb')
        self.reader = BoundedTarReader(self.gzip)
        try:
            self.tar = tarfile.open(fileobj=self.reader, mode='r|', tarinfo=BoundedTarInfo)
        except Exception:
            self.gzip.close()
            raise
        return self.tar

    def __exit__(self, exc_type, exc_value, traceback):
        self.tar.close()
        self.gzip.close()
        if exc_type is None:
            verify_gzip_member(self.archive)
        return False


def verify_gzip_member(archive):
    """Consume and validate exactly one gzip member with bounded output."""
    archive.seek(0)
    decoder = zlib.decompressobj(16 + zlib.MAX_WBITS)
    expanded = 0
    while True:
        chunk = archive.read(64 * 1024)
        if not chunk:
            if not decoder.eof:
                raise RuntimeError('Release archive has a truncated gzip stream')
            return
        pending = chunk
        while pending:
            remaining = MAX_EXPANDED_BYTES - expanded
            output = decoder.decompress(pending, min(TAR_READ_SIZE, remaining + 1))
            expanded += len(output)
            if expanded > MAX_EXPANDED_BYTES:
                raise RuntimeError('Release archive exceeds the expanded size limit')
            if decoder.unused_data:
                raise RuntimeError('Release archive contains trailing compressed data or multiple gzip members')
            pending = decoder.unconsumed_tail
            if decoder.eof:
                if pending or archive.read(1):
                    raise RuntimeError('Release archive contains trailing compressed data or multiple gzip members')
                return


def validate_archive(tar):
    """Reject archives that could exhaust disk or memory during extraction."""
    expanded = 0
    for count, member in enumerate(tar, start=1):
        if count > MAX_ARCHIVE_MEMBERS:
            raise RuntimeError('Release archive contains too many entries')
        expanded += member.size
        if expanded > MAX_EXPANDED_BYTES:
            raise RuntimeError('Release archive exceeds the expanded size limit')

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


def ensure_web_account():
    """Create or verify a dedicated web identity before switching services."""
    try:
        group = grp.getgrnam(WEB_GROUP)
    except KeyError:
        run('groupadd', '--system', WEB_GROUP)
        group = grp.getgrnam(WEB_GROUP)

    try:
        user = pwd.getpwnam(WEB_USER)
    except KeyError:
        run('useradd', '--system', '--no-create-home', '--home-dir', '/nonexistent',
            '--shell', '/usr/sbin/nologin', '--gid', WEB_GROUP, WEB_USER)
        user = pwd.getpwnam(WEB_USER)

    try:
        memberships = set(os.getgrouplist(WEB_USER, user.pw_gid))
        canter_uid = pwd.getpwnam('canter').pw_uid
        canter_gid = grp.getgrnam('canter').gr_gid
    except KeyError as error:
        raise RuntimeError('Could not verify the canter-web account against the canter account') from error
    except OSError as error:
        raise RuntimeError('Could not verify the canter-web group membership') from error
    if (user.pw_uid == 0 or user.pw_uid == canter_uid or user.pw_gid != group.gr_gid
            or group.gr_gid == canter_gid or memberships != {group.gr_gid}
            or user.pw_dir != '/nonexistent'
            or user.pw_shell not in ('/usr/sbin/nologin', '/sbin/nologin', '/bin/false')):
        raise RuntimeError('canter-web must be a dedicated nologin account in only the canter-web group')


def prepare_web_tree(path):
    """Make the web release readable and its Next cache writable."""
    # Repository-root tracing nests the Next server under web/. A standalone
    # build traced from the app directory instead puts it at the release root.
    app_dir = path / 'web' if (path / 'web/server.js').is_file() else path
    if app_dir.is_symlink():
        raise RuntimeError('Web application path must not be a symlink')
    next_dir = app_dir / '.next'
    cache_dir = next_dir / 'cache'
    if next_dir.is_symlink() or cache_dir.is_symlink():
        raise RuntimeError('Web Next.js cache path must not be a symlink')
    if not next_dir.is_dir():
        raise RuntimeError('Web release is missing its .next directory')
    cache_dir.mkdir(mode=0o750, exist_ok=True)
    if not cache_dir.is_dir():
        raise RuntimeError('Web Next.js cache path must be a directory')
    run('chown', '-R', f'{WEB_USER}:{WEB_GROUP}', str(path))
    cache_dir.chmod(stat.S_IMODE(cache_dir.stat().st_mode) | stat.S_IRUSR | stat.S_IWUSR | stat.S_IXUSR)


def allow_web_path_traversal():
    """Let canter-web traverse /opt/canter without listing its contents."""
    os.chmod(ROOT, stat.S_IMODE(ROOT.stat().st_mode) | stat.S_IXOTH)


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
                  for name in RUNTIME_UNITS},
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
                 *(release / 'deploy' / name for name in RUNTIME_UNITS)]:
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
    for name in RUNTIME_UNITS:
        install_file(release / 'deploy' / name, SYSTEMD / name, 0o644)
    install_file(release / 'canter-static-linux', ROOT / 'bin/canter-static-linux')
    run('systemctl', 'daemon-reload')
    run('systemctl', 'enable', '--now', 'canter-harness.socket')


def restore_runtime(previous, keep_web_unit=False):
    stop_runtime()
    if previous['target'] is None:
        (HARNESS / 'current').unlink(missing_ok=True)
    else:
        point_harness(previous['target'])
    for name, content in previous['units'].items():
        # Keep the hardened identity even when the application release rolls back.
        if name == 'canter-web.service' and keep_web_unit:
            continue
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
    ensure_web_account()
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
    prepare_web_tree(release / 'web')
    allow_web_path_traversal()
    switched = False
    runtime_changed = False
    web_unit_installed = False
    try:
        run('systemctl', 'stop', 'canter-web', 'canter-controlplane')
        runtime_changed = True
        install_runtime(release, sha)
        web_unit_installed = True
        prepare_web_tree(ROOT / 'web')
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
            restore_runtime(runtime, keep_web_unit=web_unit_installed)
        if web_unit_installed:
            prepare_web_tree(ROOT / 'web')
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
        releases = ROOT / 'releases'
        releases.mkdir(exist_ok=True)
        path = releases / ('actions-' + sha)
        if path.exists():
            raise RuntimeError('Incomplete release directory exists; inspect before retrying')
        with tempfile.TemporaryFile() as archive:
            if download(assets['canter-release.tar.gz'], archive) != expected.split()[0]:
                raise RuntimeError('Release checksum mismatch')
            archive.seek(0)
            with BoundedTar(archive) as tar:
                validate_archive(tar)
            path.mkdir(mode=0o700)
            with BoundedTar(archive) as tar:
                for member in tar:
                    tar.extract(member, path, filter='data')
        if json.loads((path / 'web/public/release.json').read_text())['commit'] != sha:
            raise RuntimeError('Artifact commit does not match tag')
        activate(path, sha)

if __name__ == '__main__':
    main()

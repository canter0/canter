#!/usr/bin/env python3
"""Prepare an isolated Canter CI toolchain prefix from a pinned manifest."""
import argparse
import ctypes
import errno
import hashlib
import json
import os
import platform
import posixpath
import pwd
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse
import urllib.request
from time import monotonic
from pathlib import Path

MAX_DOWNLOAD_BYTES = 512 * 1024 * 1024
SOCKET_TIMEOUT_SECONDS = 30
TOTAL_DEADLINE_SECONDS = 20 * 60
EXPECTED_TOOLS = ('go', 'node', 'pnpm', 'runner')
MANIFEST_KEYS = {'schemaVersion', 'platform'} | set(EXPECTED_TOOLS)
TOOL_KEYS = {'version', 'url', 'algorithm', 'digest', 'stripPrefix'}
DIGEST_HEX_LENGTH = {'sha256': 64, 'sha512': 128}
VERSION_RE = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+')
RESERVED_NAMES = ('.runner', '.credentials', '.credentials_rsaparams',
                  '.env', '.path', '.service', '_work')
NOTE = 'prepared tools only; not a sanitized bootable image'
DEFAULT_MANIFEST = (Path(__file__).resolve().parent.parent
                    / 'deploy' / 'canter-ci-toolchains.json')


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog='canter-ci-prepare',
        description='Download, verify, and install the pinned Canter CI toolchain '
                    'into a fresh prefix directory.')
    parser.add_argument('--manifest', default=str(DEFAULT_MANIFEST),
                        help='pinned toolchain manifest JSON')
    parser.add_argument('--prefix', default='/opt/canter-ci',
                        help='installation prefix (default /opt/canter-ci)')
    parser.add_argument('--owner', default=None,
                        help='dedicated non-root guest user that owns the runner tree '
                             'and toolcache directories (required unless --verify-only)')
    parser.add_argument('--verify-only', action='store_true',
                        help='validate an installed prefix without network access or writes')
    return parser.parse_args(argv)


def check_platform():
    if platform.system() != 'Linux':
        raise RuntimeError('canter-ci-prepare only supports Linux')
    if platform.machine().lower() not in ('x86_64', 'amd64'):
        raise RuntimeError('canter-ci-prepare only supports x86_64/amd64')


def _validate_url(url, tool):
    if not isinstance(url, str) or not url:
        raise ValueError(f'{tool}: url must be a nonempty string')
    try:
        parts = urllib.parse.urlsplit(url)
    except ValueError:
        raise ValueError(f'{tool}: url is not parseable') from None
    if parts.scheme != 'https':
        raise ValueError(f'{tool}: url must use https')
    if not parts.hostname:
        raise ValueError(f'{tool}: url must include a host')
    if parts.username is not None or parts.password is not None:
        raise ValueError(f'{tool}: url must not contain credentials')
    if parts.query or parts.fragment:
        raise ValueError(f'{tool}: url must not contain a query or fragment')


def _validate_strip_prefix(value, tool):
    if not isinstance(value, str):
        raise ValueError(f'{tool}: stripPrefix must be a string')
    if value == '':
        return ''
    stripped = value.rstrip('/')
    if not stripped or stripped.startswith('/') or '\\' in stripped:
        raise ValueError(f'{tool}: stripPrefix must be a safe relative path')
    if posixpath.normpath(stripped) != stripped or stripped in ('.', '..') \
            or stripped.startswith('../'):
        raise ValueError(f'{tool}: stripPrefix must be a safe relative path')
    return stripped


def validate_manifest(manifest):
    if not isinstance(manifest, dict):
        raise ValueError('manifest must be a JSON object')
    if manifest.get('schemaVersion') != 1:
        raise ValueError('manifest schemaVersion must be 1')
    if manifest.get('platform') != 'linux-x64':
        raise ValueError('manifest platform must be linux-x64')
    tools = {key for key in manifest if key not in MANIFEST_KEYS}
    if tools:
        raise ValueError('manifest contains unknown keys: ' + ','.join(sorted(tools)))
    normalized = dict(manifest)
    for tool in EXPECTED_TOOLS:
        spec = manifest.get(tool)
        if not isinstance(spec, dict):
            raise ValueError(f'manifest is missing tool {tool}')
        unknown = set(spec) - TOOL_KEYS
        if unknown:
            raise ValueError(f'{tool}: unknown keys ' + ','.join(sorted(unknown)))
        missing = TOOL_KEYS - set(spec)
        if missing:
            raise ValueError(f'{tool}: missing keys ' + ','.join(sorted(missing)))
        version = spec['version']
        if not isinstance(version, str) or not VERSION_RE.fullmatch(version):
            raise ValueError(f'{tool}: version must be an exact X.Y.Z release')
        _validate_url(spec['url'], tool)
        algorithm = spec['algorithm']
        if algorithm not in DIGEST_HEX_LENGTH:
            raise ValueError(f'{tool}: unsupported digest algorithm')
        digest = spec['digest']
        hexdigits = DIGEST_HEX_LENGTH[algorithm]
        if not isinstance(digest, str) or len(digest) != hexdigits \
                or any(c not in '0123456789abcdefABCDEF' for c in digest):
            raise ValueError(f'{tool}: digest must be {hexdigits} hex characters')
        spec = dict(manifest[tool])
        spec['stripPrefix'] = _validate_strip_prefix(spec['stripPrefix'], tool)
        normalized[tool] = spec
    return normalized


class _HttpsOnlyRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if urllib.parse.urlsplit(newurl).scheme != 'https':
            return None
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def build_opener():
    return urllib.request.build_opener(_HttpsOnlyRedirect)


def download_tool(tool, spec, dest, opener, deadline):
    request = urllib.request.Request(
        spec['url'], headers={'User-Agent': 'canter-ci-prepare'})
    try:
        with opener.open(request, timeout=SOCKET_TIMEOUT_SECONDS) as response:
            written = 0
            with open(dest, 'wb') as out:
                while True:
                    if monotonic() > deadline:
                        raise RuntimeError('download deadline exceeded')
                    chunk = response.read(1024 * 1024)
                    if not chunk:
                        break
                    written += len(chunk)
                    if written > MAX_DOWNLOAD_BYTES:
                        raise RuntimeError('download exceeds size limit')
                    out.write(chunk)
    except RuntimeError:
        raise
    except Exception:
        raise RuntimeError(f'download failed for {tool}') from None


def verify_digest(path, algorithm, digest):
    hasher = hashlib.new(algorithm)
    with open(path, 'rb') as src:
        for chunk in iter(lambda: src.read(1024 * 1024), b''):
            hasher.update(chunk)
    if hasher.hexdigest().lower() != digest.lower():
        raise RuntimeError('checksum mismatch')


def _member_relpath(name, strip):
    if not isinstance(name, str) or not name or '\x00' in name:
        raise RuntimeError('archive member has an unsafe name')
    if '..' in name.split('/'):
        raise RuntimeError('archive member name contains a .. component')
    norm = posixpath.normpath(name)
    if norm.startswith('/') or norm == '..' or norm.startswith('../'):
        raise RuntimeError('archive member escapes the extraction root')
    if strip:
        if norm == strip:
            return None
        if not norm.startswith(strip + '/'):
            return None
        rel = norm[len(strip) + 1:]
    else:
        rel = norm
    if rel in ('', '.', '..'):
        return None
    if rel.startswith('/') or rel.startswith('../') or '/../' in rel + '/':
        raise RuntimeError('archive member escapes the extraction root')
    return rel


def _link_target_inside(member_rel, linkname):
    if not isinstance(linkname, str) or not linkname or '\x00' in linkname \
            or posixpath.isabs(linkname):
        raise RuntimeError('archive link target is unsafe')
    base = posixpath.dirname(member_rel)
    target = posixpath.normpath(posixpath.join(base, linkname)) if base \
        else posixpath.normpath(linkname)
    if target == '..' or target.startswith('../') or posixpath.isabs(target):
        raise RuntimeError('archive link target escapes the extraction root')


def _validated_members(tar, strip):
    members = []
    for member in tar.getmembers():
        rel = _member_relpath(member.name, strip)
        if rel is None:
            continue
        kind = member.type
        if kind in (tarfile.REGTYPE, tarfile.AREGTYPE):
            link = None
        elif kind == tarfile.DIRTYPE:
            link = None
        elif kind == tarfile.SYMTYPE:
            _link_target_inside(rel, member.linkname)
            link = ('symlink', member.linkname)
        elif kind == tarfile.LNKTYPE:
            target = _member_relpath(member.linkname, strip)
            if target is None:
                raise RuntimeError('archive hardlink target is outside the strip prefix')
            link = ('hardlink', target)
        else:
            raise RuntimeError('archive contains an unsupported member type')
        members.append((member, rel, link))
    return members


def _check_parent_inside(dest_real, target):
    parent = os.path.dirname(target) or dest_real
    resolved = os.path.realpath(parent)
    if resolved != dest_real and not resolved.startswith(dest_real + os.sep):
        raise RuntimeError('archive write would escape through a symlink ancestor')


def _check_target_inside(dest_real, target):
    resolved = os.path.realpath(target)
    if resolved != dest_real and not resolved.startswith(dest_real + os.sep):
        raise RuntimeError('archive member would escape through a symlink')


def _check_link_inside(dest_real, target):
    if not os.path.islink(target):
        return
    resolved = os.path.realpath(target)
    if resolved != dest_real and not resolved.startswith(dest_real + os.sep):
        raise RuntimeError('archive symlink escapes the extraction root')


def _remove_existing(target):
    if os.path.islink(target) or os.path.exists(target):
        if os.path.isdir(target) and not os.path.islink(target):
            raise RuntimeError('archive member conflicts with an existing directory')
        os.remove(target)


def extract_archive(archive_path, dest, strip):
    dest = os.path.abspath(dest)
    os.makedirs(dest, exist_ok=True)
    dest_real = os.path.realpath(dest)
    links = []
    with tarfile.open(archive_path, 'r:*') as tar:
        members = _validated_members(tar, strip)
        deferred = [entry for entry in members
                    if entry[2] is not None and entry[2][0] == 'hardlink']
        for member, rel, link in members:
            if link is not None and link[0] == 'hardlink':
                continue
            target = os.path.join(dest, *rel.split('/'))
            _check_parent_inside(dest_real, target)
            if member.type == tarfile.DIRTYPE:
                _check_target_inside(dest_real, target)
                os.makedirs(target, exist_ok=True)
                os.chmod(target, (member.mode & 0o777) | 0o700)
                continue
            parent = os.path.dirname(target)
            _check_parent_inside(dest_real, target)
            os.makedirs(parent, exist_ok=True)
            _check_parent_inside(dest_real, target)
            _remove_existing(target)
            if member.type in (tarfile.REGTYPE, tarfile.AREGTYPE):
                source = tar.extractfile(member)
                if source is None:
                    raise RuntimeError('archive member is not readable')
                with source, open(target, 'wb') as out:
                    shutil.copyfileobj(source, out, 1024 * 1024)
                os.chmod(target, (member.mode & 0o777) or 0o644)
            elif link is not None and link[0] == 'symlink':
                os.symlink(link[1], target)
                try:
                    _check_link_inside(dest_real, target)
                except RuntimeError:
                    os.remove(target)
                    raise
                links.append(target)
        for member, rel, link in deferred:
            target = os.path.join(dest, *rel.split('/'))
            _check_parent_inside(dest_real, target)
            parent = os.path.dirname(target)
            os.makedirs(parent, exist_ok=True)
            _check_parent_inside(dest_real, target)
            _remove_existing(target)
            src = os.path.join(dest, *link[1].split('/'))
            if not os.path.lexists(src):
                raise RuntimeError('archive hardlink target is missing')
            src_real = os.path.realpath(src)
            if src_real != dest_real and not src_real.startswith(dest_real + os.sep):
                raise RuntimeError('archive hardlink source escapes the extraction root')
            if not stat.S_ISREG(os.lstat(src).st_mode):
                raise RuntimeError('archive hardlink source is not a regular file')
            os.link(src, target, follow_symlinks=False)
            os.chmod(target, (member.mode & 0o777) or 0o644)
    for target in links:
        _check_link_inside(dest_real, target)


def _layout_paths(prefix, manifest):
    return {
        'bin': os.path.join(prefix, 'bin'),
        'go': os.path.join(prefix, 'toolcache', 'go',
                           manifest['go']['version'], 'x64'),
        'node': os.path.join(prefix, 'toolcache', 'node',
                             manifest['node']['version'], 'x64'),
        'pnpm': os.path.join(prefix, 'pnpm', manifest['pnpm']['version']),
        'runner': os.path.join(prefix, 'runner'),
        'manifest': os.path.join(prefix, 'manifest.json'),
    }


def _result(prefix, manifest):
    return {
        'prefix': prefix,
        'paths': _layout_paths(prefix, manifest),
        'versions': {tool: manifest[tool]['version'] for tool in EXPECTED_TOOLS},
        'note': NOTE,
    }


def _stage_extract(archive, dest, strip):
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    extract_archive(archive, dest, strip)


def _write_marker(path):
    with open(path, 'wb'):
        pass


def _link_bin(staging, manifest):
    bindir = os.path.join(staging, 'bin')
    os.makedirs(bindir, exist_ok=True)
    links = {
        'go': os.path.join('..', 'toolcache', 'go', manifest['go']['version'],
                           'x64', 'bin', 'go'),
        'node': os.path.join('..', 'toolcache', 'node', manifest['node']['version'],
                             'x64', 'bin', 'node'),
        'npm': os.path.join('..', 'toolcache', 'node', manifest['node']['version'],
                            'x64', 'bin', 'npm'),
        'npx': os.path.join('..', 'toolcache', 'node', manifest['node']['version'],
                            'x64', 'bin', 'npx'),
        'pnpm': os.path.join('..', 'pnpm', manifest['pnpm']['version'],
                             'bin', 'pnpm.cjs'),
    }
    for name, target in links.items():
        os.symlink(target, os.path.join(bindir, name))


def _copy_manifest(staging, manifest):
    with open(os.path.join(staging, 'manifest.json'), 'w', encoding='utf-8') as out:
        json.dump(manifest, out, indent=2, sort_keys=True)
        out.write('\n')


def _chown_one(chown, path, uid, gid, follow_symlinks=True):
    try:
        chown(path, uid, gid, follow_symlinks=follow_symlinks)
    except TypeError:
        chown(path, uid, gid)


def _chown_tree(chown, root, uid, gid):
    for dirpath, dirnames, filenames in os.walk(root):
        for name in dirnames + filenames:
            path = os.path.join(dirpath, name)
            follow = not os.path.islink(path)
            _chown_one(chown, path, uid, gid, follow_symlinks=follow)
    _chown_one(chown, root, uid, gid)


def _chown_toolcache_dirs(chown, root, uid, gid):
    for dirpath, dirnames, _filenames in os.walk(root):
        _chown_one(chown, dirpath, uid, gid)
        dirnames.sort()


def apply_ownership(staging, uid, gid, chown=os.chown):
    _chown_tree(chown, os.path.join(staging, 'runner'), uid, gid)
    _chown_toolcache_dirs(chown, os.path.join(staging, 'toolcache'), uid, gid)


def _publish_no_replace(src, dst):
    if platform.system() != 'Linux' \
            or platform.machine().lower() not in ('x86_64', 'amd64'):
        raise RuntimeError('atomic publish requires Linux x86_64')
    libc = ctypes.CDLL(None, use_errno=True)
    libc.syscall.restype = ctypes.c_long
    result = libc.syscall(316, -100, os.fsencode(src), -100, os.fsencode(dst), 1)
    if result != 0:
        error = ctypes.get_errno()
        if error == errno.EEXIST:
            raise RuntimeError('prefix appeared during preparation; refusing to overwrite')
        raise OSError(error, os.strerror(error))


def prepare(manifest, prefix, owner, opener=None,
            chown=os.chown, getpwnam=pwd.getpwnam):
    manifest = validate_manifest(manifest)
    prefix = os.path.abspath(os.path.expanduser(prefix))
    parent = os.path.dirname(prefix)
    os.makedirs(parent, exist_ok=True)
    if os.path.lexists(prefix):
        raise RuntimeError('prefix already exists; refusing to overwrite')
    try:
        account = getpwnam(owner)
    except KeyError:
        raise RuntimeError('unknown owner user') from None
    if account.pw_uid == 0:
        raise RuntimeError('owner must be a dedicated non-root user')
    opener = opener or build_opener()
    download_dir = tempfile.mkdtemp(prefix='.canter-ci-dl-', dir=parent)
    staging = None
    try:
        deadline = monotonic() + TOTAL_DEADLINE_SECONDS
        archives = {}
        for tool in EXPECTED_TOOLS:
            spec = manifest[tool]
            archive = os.path.join(download_dir, tool + '.tar')
            download_tool(tool, spec, archive, opener, deadline)
            verify_digest(archive, spec['algorithm'], spec['digest'])
            archives[tool] = archive
        staging = tempfile.mkdtemp(prefix='.canter-ci-stage-', dir=parent)
        _stage_extract(archives['go'],
                       os.path.join(staging, 'toolcache', 'go',
                                    manifest['go']['version'], 'x64'),
                       manifest['go']['stripPrefix'])
        _stage_extract(archives['node'],
                       os.path.join(staging, 'toolcache', 'node',
                                    manifest['node']['version'], 'x64'),
                       manifest['node']['stripPrefix'])
        _stage_extract(archives['pnpm'],
                       os.path.join(staging, 'pnpm', manifest['pnpm']['version']),
                       manifest['pnpm']['stripPrefix'])
        _stage_extract(archives['runner'],
                       os.path.join(staging, 'runner'),
                       manifest['runner']['stripPrefix'])
        for tool in ('go', 'node'):
            _write_marker(os.path.join(
                staging, 'toolcache', tool, manifest[tool]['version'], 'x64.complete'))
        _link_bin(staging, manifest)
        _copy_manifest(staging, manifest)
        pnpm_cjs = os.path.join(staging, 'pnpm', manifest['pnpm']['version'],
                                'bin', 'pnpm.cjs')
        os.chmod(pnpm_cjs, os.stat(pnpm_cjs).st_mode | 0o111)
        apply_ownership(staging, account.pw_uid, account.pw_gid, chown)
        versions, problems = _inspect_prefix(staging, manifest)
        if problems:
            raise RuntimeError('staged toolchain verification failed: '
                               + '; '.join(problems))
        if os.path.lexists(prefix):
            raise RuntimeError('prefix appeared during preparation; refusing to overwrite')
        os.chmod(staging, 0o755)
        _publish_no_replace(staging, prefix)
        staging = None
    finally:
        if staging is not None:
            shutil.rmtree(staging, ignore_errors=True)
        shutil.rmtree(download_dir, ignore_errors=True)
    result = _result(prefix, manifest)
    result['versions'] = versions
    return result


def _inside(root, path):
    root_real = os.path.realpath(root)
    resolved = os.path.realpath(path)
    return resolved == root_real or resolved.startswith(root_real + os.sep)


def _require_file(root, rel, executable=False):
    path = os.path.join(root, *rel.split('/'))
    if not os.path.isfile(path):
        return 'missing ' + rel
    if not _inside(root, path):
        return 'escapes prefix ' + rel
    if executable and not os.access(path, os.X_OK):
        return 'not executable ' + rel
    return None


VERSION_ENV = {
    'PATH': '/usr/bin:/bin',
    'LANG': 'C',
    'LC_ALL': 'C',
    'GOTOOLCHAIN': 'local',
    'GOTELEMETRY': 'off',
    'GOPROXY': 'off',
    'GOSUMDB': 'off',
}


def _version_output(argv, timeout=30):
    try:
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=timeout,
                              env=dict(VERSION_ENV), cwd='/')
    except (OSError, subprocess.SubprocessError):
        return None, 'unavailable'
    if proc.returncode != 0:
        return None, 'failed'
    return (proc.stdout or '').strip(), 'ok'


def _forbidden_runner_state(root):
    problems = []
    runner_root = os.path.join(root, 'runner')
    for dirpath, dirnames, filenames in os.walk(runner_root):
        for name in dirnames + filenames:
            if name in RESERVED_NAMES:
                problems.append(os.path.relpath(os.path.join(dirpath, name), root))
    return problems


def _inspect_prefix(prefix, manifest):
    manifest = validate_manifest(manifest)
    errors = []
    manifest_path = os.path.join(prefix, 'manifest.json')
    if not os.path.isfile(manifest_path):
        errors.append('missing manifest.json')
    else:
        try:
            with open(manifest_path, 'r', encoding='utf-8') as src:
                installed = validate_manifest(json.load(src))
        except (OSError, ValueError):
            installed = None
        if installed != manifest:
            errors.append('installed manifest.json does not match')
    checks = []
    for tool in ('go', 'node'):
        version = manifest[tool]['version']
        base = f'toolcache/{tool}/{version}'
        checks.append((f'{base}/x64.complete', False))
        binary = 'go' if tool == 'go' else 'node'
        checks.append((f'{base}/x64/bin/{binary}', True))
    pnpm_version = manifest['pnpm']['version']
    checks.append((f'pnpm/{pnpm_version}/bin/pnpm.cjs', False))
    for name in ('go', 'node', 'npm', 'npx', 'pnpm'):
        checks.append((f'bin/{name}', True))
    checks.append(('runner/config.sh', True))
    checks.append(('runner/run.sh', True))
    checks.append(('runner/bin/Runner.Listener', True))
    for rel, executable in checks:
        problem = _require_file(prefix, rel, executable)
        if problem:
            errors.append(problem)
    for name in ('go', 'node', 'npm', 'npx', 'pnpm'):
        link = os.path.join(prefix, 'bin', name)
        if os.path.lexists(link) and not _inside(prefix, link):
            errors.append('bin link escapes prefix: ' + name)
    for problem in _forbidden_runner_state(prefix):
        errors.append('forbidden runner state: ' + problem)
    if errors:
        return {}, errors
    versions = {}
    go_bin = os.path.join(prefix, 'toolcache', 'go',
                          manifest['go']['version'], 'x64', 'bin', 'go')
    expected = f'go version go{manifest["go"]["version"]} linux/amd64'
    out, status = _version_output([go_bin, 'version'])
    if status != 'ok' or ' '.join((out or '').split()) != expected:
        errors.append('go version check failed')
    else:
        versions['go'] = manifest['go']['version']
    node_bin = os.path.join(prefix, 'toolcache', 'node',
                            manifest['node']['version'], 'x64', 'bin', 'node')
    out, status = _version_output([node_bin, '--version'])
    if status != 'ok' or out != 'v' + manifest['node']['version']:
        errors.append('node version check failed')
    else:
        versions['node'] = manifest['node']['version']
    pnpm_cjs = os.path.join(prefix, 'pnpm', pnpm_version, 'bin', 'pnpm.cjs')
    out, status = _version_output([node_bin, pnpm_cjs, '--version'])
    if status != 'ok' or out != manifest['pnpm']['version']:
        errors.append('pnpm version check failed')
    else:
        versions['pnpm'] = manifest['pnpm']['version']
    versions['runner'] = manifest['runner']['version'] + ' (presence only)'
    return versions, errors


def verify_prefix(prefix, manifest):
    prefix = os.path.abspath(os.path.expanduser(prefix))
    if not os.path.isdir(prefix):
        raise RuntimeError('prefix does not exist: ' + prefix)
    versions, errors = _inspect_prefix(prefix, manifest)
    if errors:
        raise RuntimeError('toolchain verification failed: ' + '; '.join(errors))
    result = _result(prefix, manifest)
    result['versions'] = versions
    return result


def main(argv=None):
    args = parse_args(argv)
    check_platform()
    try:
        with open(args.manifest, 'rb') as src:
            manifest = json.load(src)
    except (OSError, ValueError):
        raise RuntimeError('cannot read toolchain manifest') from None
    manifest = validate_manifest(manifest)
    if args.verify_only:
        result = verify_prefix(args.prefix, manifest)
    else:
        if not args.owner:
            raise RuntimeError('--owner is required')
        if os.geteuid() != 0:
            raise RuntimeError('toolchain preparation requires root')
        result = prepare(manifest, args.prefix, args.owner)
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (RuntimeError, OSError, ValueError) as exc:
        print(f'canter-ci-prepare failed: {exc}', file=sys.stderr)
        sys.exit(1)

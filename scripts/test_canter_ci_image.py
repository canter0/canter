import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import tempfile
import time
import unittest
import urllib.request
from types import SimpleNamespace
from unittest import mock

spec = importlib.util.spec_from_file_location(
    'canter_ci_image', Path(__file__).with_name('canter-ci-image.py'))
image = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image)

ARCHIVE = b'fake-runner-archive-bytes'
ARCHIVE_SHA = hashlib.sha256(ARCHIVE).hexdigest()
RUNNER_URL = ('https://github.com/actions/runner/releases/download/'
              'v2.3.4/actions-runner-linux-x64-2.3.4.tar.gz')


def manifest_dict(archive_sha=ARCHIVE_SHA):
    return {
        'schemaVersion': 1, 'platform': 'linux-x64',
        'go': {'version': '1.2.3',
               'url': 'https://dl.google.com/go/go1.2.3.linux-amd64.tar.gz',
               'algorithm': 'sha256', 'digest': 'a' * 64, 'stripPrefix': 'go/'},
        'node': {'version': '3.4.5',
                 'url': 'https://nodejs.org/dist/v3.4.5/node-v3.4.5-linux-x64.tar.gz',
                 'algorithm': 'sha256', 'digest': 'b' * 64,
                 'stripPrefix': 'node-v3.4.5-linux-x64/'},
        'pnpm': {'version': '6.7.8',
                 'url': 'https://registry.npmjs.org/pnpm/-/pnpm-6.7.8.tgz',
                 'algorithm': 'sha512', 'digest': 'c' * 128, 'stripPrefix': 'package/'},
        'runner': {'version': '2.3.4', 'url': RUNNER_URL, 'algorithm': 'sha256',
                   'digest': archive_sha, 'stripPrefix': ''},
    }


def write_toolchains(prefix, normalized):
    tc = Path(prefix)
    (tc / 'bin').mkdir(parents=True)
    (tc / 'runner').mkdir()
    (tc / 'runner' / 'README').write_text('runner\n')
    (tc / 'manifest.json').write_text(
        json.dumps(normalized, indent=2, sort_keys=True) + '\n')
    go_v = normalized['go']['version']
    node_v = normalized['node']['version']
    pnpm_v = normalized['pnpm']['version']
    for tool, version, names in (
            ('go', go_v, ('go',)),
            ('node', node_v, ('node', 'npm', 'npx'))):
        version_root = tc / 'toolcache' / tool / version
        bindir = version_root / 'x64' / 'bin'
        bindir.mkdir(parents=True)
        (version_root / 'x64.complete').write_text('')
        for name in names:
            target = bindir / name
            target.write_text('#!/bin/sh\nexit 0\n')
            target.chmod(0o755)
    pnpm_bin = tc / 'pnpm' / pnpm_v / 'bin'
    pnpm_bin.mkdir(parents=True)
    cjs = pnpm_bin / 'pnpm.cjs'
    cjs.write_text('// pnpm\n')
    cjs.chmod(0o755)
    for name, target in {
            'go': '../toolcache/go/' + go_v + '/x64/bin/go',
            'node': '../toolcache/node/' + node_v + '/x64/bin/node',
            'npm': '../toolcache/node/' + node_v + '/x64/bin/npm',
            'npx': '../toolcache/node/' + node_v + '/x64/bin/npx',
            'pnpm': '../pnpm/' + pnpm_v + '/bin/pnpm.cjs'}.items():
        os.symlink(target, tc / 'bin' / name)


def seal_modes(root):
    for dirpath, _dirnames, filenames in os.walk(str(root)):
        os.chmod(dirpath, 0o555)
        for name in filenames:
            path = os.path.join(dirpath, name)
            if not os.path.islink(path):
                os.chmod(path, 0o444)
    for rel in ('toolcache/go/1.2.3/x64/bin/go',
                'toolcache/node/3.4.5/x64/bin/node',
                'toolcache/node/3.4.5/x64/bin/npm',
                'toolcache/node/3.4.5/x64/bin/npx',
                'pnpm/6.7.8/bin/pnpm.cjs'):
        os.chmod(Path(root) / 'toolchains' / rel, 0o555)


def write_payload(root, normalized, archive_sha):
    write_toolchains(Path(root) / 'toolchains', normalized)
    (Path(root) / 'runner.tar.gz').write_bytes(ARCHIVE)
    marker = {
        'schemaVersion': 1, 'platform': 'linux-x64', 'baseOS': 'ubuntu-24.04',
        'runnerUrl': normalized['runner']['url'], 'runnerSha256': archive_sha,
        'toolchainManifestSha256': image._manifest_digest(normalized),
        'dependenciesInstalled': True,
    }
    (Path(root) / 'image.json').write_text(json.dumps(marker) + '\n')
    seal_modes(root)


class FakeResponse:
    def __init__(self, data):
        self._data = data
        self._pos = 0

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def read(self, size=-1):
        if self._pos >= len(self._data):
            return b''
        if size is None or size < 0:
            chunk = self._data[self._pos:]
        else:
            chunk = self._data[self._pos:self._pos + size]
        self._pos += len(chunk)
        return chunk


class FakeOpener:
    def __init__(self, data=ARCHIVE):
        self.data = data
        self.requests = []

    def open(self, request, timeout=None):
        self.requests.append(request.full_url)
        return FakeResponse(self.data)


class ImageCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.addCleanup(self._restore_perms)
        self.base = Path(self.tmp.name).resolve()

    def _restore_perms(self):
        dirs = [self.base]
        for dirpath, dirnames, _filenames in os.walk(self.base):
            dirs.extend(os.path.join(dirpath, name) for name in dirnames)
        for path in dirs:
            try:
                os.chmod(path, 0o700)
            except OSError:
                pass

    def _manifest_file(self, manifest=None):
        path = self.base / 'manifest.json'
        path.write_text(json.dumps(manifest or manifest_dict()))
        return str(path)

    def _deadline(self, seconds=600):
        return int((time.time() + seconds) * 1000)


class PlanCase(ImageCase):
    def test_real_manifest_plan_fields_and_canonical_digest(self):
        path = str(image.prepare.DEFAULT_MANIFEST)
        data = image._plan_data(path)
        self.assertEqual(set(data), {
            'schemaVersion', 'payloadPrefix', 'runnerUrl', 'runnerSha256',
            'toolchainManifestSha256', 'requiresFreshDisposableVm',
            'createsProviderResources', 'sanitizesWholeMachine'})
        self.assertEqual(data['schemaVersion'], 1)
        self.assertEqual(data['payloadPrefix'], '/opt/canter-ci-image')
        self.assertTrue(data['requiresFreshDisposableVm'])
        self.assertFalse(data['createsProviderResources'])
        self.assertFalse(data['sanitizesWholeMachine'])
        normalized = image._strict_manifest(path)
        raw = (json.dumps(normalized, sort_keys=True, separators=(',', ':'),
                          ensure_ascii=True) + '\n').encode('ascii')
        self.assertEqual(data['toolchainManifestSha256'],
                         hashlib.sha256(raw).hexdigest())
        self.assertEqual(data['runnerUrl'], normalized['runner']['url'])
        self.assertEqual(data['runnerSha256'], normalized['runner']['digest'])

    def test_plan_cli_prints_public_descriptor(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            rc = image.main(['plan', '--manifest', self._manifest_file()])
        self.assertEqual(rc, 0)
        data = json.loads(out.getvalue())
        self.assertEqual(data['payloadPrefix'], '/opt/canter-ci-image')
        self.assertEqual(data['runnerSha256'], ARCHIVE_SHA)

    def test_manifest_pin_violations_rejected(self):
        mutations = [
            lambda m: m['go'].update(url='https://evil.example/go.tar.gz'),
            lambda m: m['go'].update(algorithm='md5'),
            lambda m: m['node'].update(stripPrefix='wrong/'),
            lambda m: m['pnpm'].update(url='https://registry.npmjs.org/evil.tgz'),
            lambda m: m['runner'].update(version='2.338'),
            lambda m: m['runner'].update(url='https://example.test/x.tar.gz'),
            lambda m: m.update(schemaVersion=True),
            lambda m: m.update(schemaVersion=2),
            lambda m: m.update(platform='linux-arm64'),
            lambda m: m.update(unknown={'version': '1.0.0'}),
        ]
        for index, mutate in enumerate(mutations):
            manifest = manifest_dict()
            mutate(manifest)
            with self.assertRaises((RuntimeError, ValueError), msg=str(index)):
                image._strict_manifest(self._manifest_file(manifest))
            (self.base / 'manifest.json').unlink()

    def test_manifest_duplicate_keys_and_size_rejected(self):
        dup = self.base / 'dup.json'
        dup.write_text('{"schemaVersion": 1, "schemaVersion": 1}')
        with self.assertRaises((RuntimeError, ValueError)):
            image._strict_manifest(str(dup))
        big = self.base / 'big.json'
        big.write_text(json.dumps(manifest_dict()) + ' ' * 70000)
        with self.assertRaises((RuntimeError, ValueError)):
            image._strict_manifest(str(big))


class VerifyCase(ImageCase):
    def setUp(self):
        super().setUp()
        self.manifest_path = self._manifest_file()
        normalized = image._strict_manifest(self.manifest_path)
        self.payload = self.base / 'payload'
        write_payload(self.payload, normalized, ARCHIVE_SHA)
        self.stack = contextlib.ExitStack()
        self.stack.enter_context(mock.patch.object(
            image, 'PREPARED_IMAGE_ROOT', str(self.payload)))
        self.stack.enter_context(mock.patch.object(
            image.runner, 'PAYLOAD_UID', os.getuid()))
        self.stack.enter_context(mock.patch.object(
            image.runner, 'PAYLOAD_GID', os.getgid()))
        self.stack.enter_context(mock.patch.object(
            image, 'PAYLOAD_UID', os.getuid()))
        self.stack.enter_context(mock.patch.object(
            image, 'PAYLOAD_GID', os.getgid()))
        self.addCleanup(self.stack.close)

    def test_verify_accepts_valid_payload(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            rc = image.main(['verify', '--manifest', self.manifest_path])
        self.assertEqual(rc, 0)
        data = json.loads(out.getvalue())
        self.assertEqual(data['payloadPrefix'], str(self.payload))
        self.assertEqual(data['toolchainPrefix'],
                         str(self.payload / 'toolchains'))
        self.assertEqual(data['note'], image.NOTE)

    def test_verify_rejects_corrupt_payload(self):
        path = self.payload / 'runner.tar.gz'
        path.chmod(0o644)
        path.write_bytes(b'corrupted')
        path.chmod(0o444)
        with contextlib.redirect_stderr(io.StringIO()):
            rc = image.main(['verify', '--manifest', self.manifest_path])
        self.assertEqual(rc, 1)

    def test_verify_rejects_self_consistent_foreign_toolchain(self):
        manifest_path = self.payload / 'toolchains' / 'manifest.json'
        manifest_path.chmod(0o644)
        foreign = json.loads(manifest_path.read_text())
        foreign['go']['digest'] = 'f' * 64
        manifest_path.write_text(
            json.dumps(foreign, indent=2, sort_keys=True) + '\n')
        manifest_path.chmod(0o444)
        marker_path = self.payload / 'image.json'
        marker_path.chmod(0o644)
        marker = json.loads(marker_path.read_text())
        marker['toolchainManifestSha256'] = image._manifest_digest(foreign)
        marker_path.write_text(json.dumps(marker) + '\n')
        marker_path.chmod(0o444)
        self.assertEqual(
            image.runner.validate_prepared_image(
                str(self.payload), RUNNER_URL, ARCHIVE_SHA),
            str(self.payload / 'toolchains'))
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), \
                contextlib.redirect_stderr(err):
            rc = image.main(['verify', '--manifest', self.manifest_path])
        self.assertEqual(rc, 1)
        self.assertEqual(out.getvalue(), '')
        self.assertEqual(err.getvalue(), 'canter-ci-image: operation failed\n')


class BuilderCase(ImageCase):
    def setUp(self):
        super().setUp()
        self.manifest_path = self._manifest_file()
        self.payload_path = self.base / 'payload'
        self.stack = contextlib.ExitStack()
        self.stack.enter_context(mock.patch.object(
            image.platform, 'system', return_value='Linux'))
        self.stack.enter_context(mock.patch.object(
            image.platform, 'machine', return_value='x86_64'))
        self.stack.enter_context(mock.patch.object(
            image.os, 'geteuid', return_value=0))
        self.stack.enter_context(mock.patch.object(
            image, '_os_release',
            return_value={'ID': 'ubuntu', 'VERSION_ID': '24.04'}))
        self.stack.enter_context(mock.patch.object(
            image, 'PAYLOAD_UID', os.getuid()))
        self.stack.enter_context(mock.patch.object(
            image, 'PAYLOAD_GID', os.getgid()))
        self.stack.enter_context(mock.patch.object(
            image.runner, 'PAYLOAD_UID', os.getuid()))
        self.stack.enter_context(mock.patch.object(
            image.runner, 'PAYLOAD_GID', os.getgid()))
        self.stack.enter_context(mock.patch.object(
            image, 'PREPARED_IMAGE_ROOT', str(self.payload_path)))
        self.addCleanup(self.stack.close)

    def _account(self, uid=1000, gid=1000):
        return lambda name: SimpleNamespace(pw_uid=uid, pw_gid=gid)

    def _staging_dirs(self):
        return [p for p in self.base.iterdir()
                if p.name.startswith('.canter-ci-image-')]

    def test_gate_rejections_happen_before_any_write_or_network(self):
        opener = mock.Mock()
        popen = mock.Mock()
        cases = [
            ('no-ack', {}, False),
            ('deadline-past', {}, True),
            ('deadline-far', {}, 'far'),
        ]
        for name, _kw, deadline in cases:
            if deadline is False:
                ms = self._deadline()
            elif deadline is True:
                ms = int((time.time() - 1) * 1000)
            else:
                ms = self._deadline(2701)
            with self.assertRaises(RuntimeError, msg=name):
                image._build(self.manifest_path, ms, name != 'no-ack',
                             getpwnam=self._account(), opener=opener,
                             popen=popen)
        opener.open.assert_not_called()
        popen.assert_not_called()
        self.assertEqual(self._staging_dirs(), [])

        extra = [
            ('nonlinux', mock.patch.object(image.platform, 'system',
                                           return_value='Darwin')),
            ('nonroot', mock.patch.object(image.os, 'geteuid',
                                          return_value=1000)),
            ('wrongos', mock.patch.object(
                image, '_os_release',
                return_value={'ID': 'debian', 'VERSION_ID': '12'})),
            ('badaccount', None),
            ('prefix-exists', None),
            ('foreign-parent', None),
            ('bad-manifest', None),
        ]
        for name, patch in extra:
            if name == 'badaccount':
                with self.assertRaises(RuntimeError, msg=name):
                    image._build(self.manifest_path, self._deadline(), True,
                                 getpwnam=self._account(uid=500), opener=opener,
                                 popen=popen)
            elif name == 'prefix-exists':
                self.payload_path.mkdir()
                with self.assertRaises(RuntimeError, msg=name):
                    image._build(self.manifest_path, self._deadline(), True,
                                 getpwnam=self._account(), opener=opener,
                                 popen=popen)
                self.payload_path.rmdir()
            elif name == 'foreign-parent':
                os.chmod(self.base, 0o777)
                try:
                    with self.assertRaises(RuntimeError, msg=name):
                        image._build(self.manifest_path, self._deadline(), True,
                                     getpwnam=self._account(), opener=opener,
                                     popen=popen)
                finally:
                    os.chmod(self.base, 0o700)
            elif name == 'bad-manifest':
                bad = manifest_dict()
                bad['go']['url'] = 'https://evil.example/go.tar.gz'
                badpath = self.base / 'bad.json'
                badpath.write_text(json.dumps(bad))
                with self.assertRaises((RuntimeError, ValueError), msg=name):
                    image._build(str(badpath), self._deadline(), True,
                                 getpwnam=self._account(), opener=opener,
                                 popen=popen)
            else:
                with patch:
                    with self.assertRaises(RuntimeError, msg=name):
                        image._build(self.manifest_path, self._deadline(), True,
                                     getpwnam=self._account(), opener=opener,
                                     popen=popen)
        opener.open.assert_not_called()
        popen.assert_not_called()
        self.assertEqual(self._staging_dirs(), [])

    def test_cache_opener_serves_verified_runner_once(self):
        archive = self.base / 'runner.tar.gz'
        archive.write_bytes(ARCHIVE)
        inner = FakeOpener(b'other')
        cache = image._CacheOpener(str(archive), RUNNER_URL, inner)
        request = urllib.request.Request(RUNNER_URL)
        with cache.open(request, timeout=5) as response:
            self.assertEqual(response.read(), ARCHIVE)
        self.assertEqual(inner.requests, [])
        other = urllib.request.Request('https://example.test/tool.tar.gz')
        with cache.open(other, timeout=5) as response:
            self.assertEqual(response.read(), b'other')
        self.assertEqual(inner.requests, ['https://example.test/tool.tar.gz'])

    def test_successful_build_publishes_verified_payload(self):
        normalized = image._strict_manifest(self.manifest_path)
        prepared = []
        chowns = []
        spawns = []

        def fake_prepare(manifest, prefix, owner, opener=None, chown=None,
                         getpwnam=None):
            prepared.append((prefix, owner))
            write_toolchains(prefix, manifest)
            return {}

        def fake_chown(path, uid, gid, follow_symlinks=True):
            chowns.append((os.fspath(path), uid, gid, follow_symlinks))

        def fake_popen(argv, **kwargs):
            spawns.append((argv, kwargs))
            proc = mock.Mock()
            proc.pid = 4321
            proc.wait.return_value = 0
            proc.poll.return_value = 0
            return proc

        published = []

        def fake_publish(src, dst):
            published.append((src, dst))
            if os.path.lexists(dst):
                raise RuntimeError('prefix appeared; refusing to overwrite')
            os.rename(src, dst)

        opener = FakeOpener()
        with mock.patch.object(image.prepare, 'prepare',
                               side_effect=fake_prepare), \
                mock.patch.object(image.prepare, '_publish_no_replace',
                                  side_effect=fake_publish), \
                mock.patch.object(image.os, 'killpg') as killpg:
            result = image._build(
                self.manifest_path, self._deadline(), True,
                getpwnam=self._account(), chown=fake_chown, opener=opener,
                popen=fake_popen)
        killpg.assert_called_once_with(4321, signal.SIGKILL)

        self.assertEqual(opener.requests, [RUNNER_URL])
        self.assertEqual(len(prepared), 1)
        self.assertEqual(prepared[0][1], 'canter-ci')
        self.assertEqual(len(spawns), 1)
        argv, kwargs = spawns[0]
        self.assertTrue(argv[0].endswith('installdependencies.sh'))
        self.assertEqual(kwargs['env'], {
            'PATH': image.DEPS_PATH, 'HOME': '/root', 'LANG': 'C.UTF-8',
            'DEBIAN_FRONTEND': 'noninteractive'})
        self.assertTrue(kwargs['start_new_session'])
        self.assertIs(kwargs['stdin'], subprocess.DEVNULL)
        self.assertIs(kwargs['stdout'], subprocess.DEVNULL)
        self.assertIs(kwargs['stderr'], subprocess.DEVNULL)
        self.assertTrue(chowns)
        self.assertTrue(all(uid == 0 and gid == 0 for _p, uid, gid, _f in chowns))
        self.assertEqual(len(published), 1)
        self.assertEqual(result['payloadPrefix'], str(self.payload_path))
        self.assertEqual(result['runnerUrl'], RUNNER_URL)
        self.assertEqual(result['runnerSha256'], ARCHIVE_SHA)
        self.assertEqual(result['toolchainManifestSha256'],
                         image._manifest_digest(normalized))
        self.assertTrue(result['dependenciesInstalled'])
        self.assertEqual(result['note'], image.NOTE)

        payload = self.payload_path
        self.assertTrue((payload / 'image.json').is_file())
        self.assertTrue((payload / 'runner.tar.gz').is_file())
        self.assertEqual(
            hashlib.sha256((payload / 'runner.tar.gz').read_bytes()).hexdigest(),
            ARCHIVE_SHA)
        marker = json.loads((payload / 'image.json').read_text())
        self.assertEqual(set(marker), {
            'schemaVersion', 'platform', 'baseOS', 'runnerUrl', 'runnerSha256',
            'toolchainManifestSha256', 'dependenciesInstalled'})
        self.assertEqual(marker['dependenciesInstalled'], True)
        self.assertEqual(stat.S_IMODE((payload / 'image.json').stat().st_mode),
                         0o444)
        self.assertEqual(stat.S_IMODE((payload / 'toolchains' / 'bin').stat().st_mode),
                         0o555)
        go_bin = payload / 'toolchains' / 'toolcache' / 'go' / '1.2.3' / 'x64' / 'bin' / 'go'
        self.assertEqual(stat.S_IMODE(go_bin.stat().st_mode), 0o555)
        readme = payload / 'toolchains' / 'runner' / 'README'
        self.assertEqual(stat.S_IMODE(readme.stat().st_mode), 0o444)
        self.assertEqual(
            image.runner.validate_prepared_image(
                str(payload), RUNNER_URL, ARCHIVE_SHA),
            str(payload / 'toolchains'))

    def test_dependency_failure_and_timeout_never_publish(self):
        normalized = image._strict_manifest(self.manifest_path)

        def fake_prepare(manifest, prefix, owner, opener=None, chown=None,
                         getpwnam=None):
            write_toolchains(prefix, manifest)
            return {}

        published = []

        def fake_publish(src, dst):
            published.append((src, dst))
            os.rename(src, dst)

        for name, wait in (
                ('rc1', mock.Mock(return_value=1)),
                ('timeout', mock.Mock(side_effect=[
                    subprocess.TimeoutExpired('deps', 1), 0]))):
            proc = mock.Mock()
            proc.pid = 4321
            proc.poll.return_value = None if name == 'timeout' else 0
            proc.wait = wait
            with mock.patch.object(image.prepare, 'prepare',
                                   side_effect=fake_prepare), \
                    mock.patch.object(image.prepare, '_publish_no_replace',
                                      side_effect=fake_publish), \
                    mock.patch.object(image.os, 'killpg') as killpg:
                with self.assertRaises(RuntimeError, msg=name):
                    image._build(
                        self.manifest_path, self._deadline(), True,
                        getpwnam=self._account(), chown=lambda *a, **k: None,
                        opener=FakeOpener(), popen=mock.Mock(return_value=proc))
            killpg.assert_called_once_with(4321, image.signal.SIGKILL)
            self.assertEqual(proc.wait.call_count, 2)
            self.assertEqual(published, [])
            self.assertFalse(self.payload_path.exists())
            self.assertEqual(self._staging_dirs(), [])

    def test_publish_collision_leaves_existing_prefix_untouched(self):
        normalized = image._strict_manifest(self.manifest_path)

        def fake_prepare(manifest, prefix, owner, opener=None, chown=None,
                         getpwnam=None):
            write_toolchains(prefix, manifest)
            return {}

        def colliding_publish(src, dst):
            os.mkdir(dst)
            (Path(dst) / 'existing').write_text('untouched\n')
            raise RuntimeError('prefix appeared; refusing to overwrite')

        with mock.patch.object(image.prepare, 'prepare',
                               side_effect=fake_prepare), \
                mock.patch.object(image.prepare, '_publish_no_replace',
                                  side_effect=colliding_publish), \
                mock.patch.object(image.os, 'killpg') as killpg:
            with self.assertRaises(RuntimeError):
                image._build(
                    self.manifest_path, self._deadline(), True,
                    getpwnam=self._account(), chown=lambda *a, **k: None,
                    opener=FakeOpener(),
                    popen=mock.Mock(return_value=mock.Mock(
                        pid=4321, wait=mock.Mock(return_value=0),
                        poll=mock.Mock(return_value=0))))
        killpg.assert_called_once_with(4321, signal.SIGKILL)
        self.assertTrue((self.payload_path / 'existing').is_file())
        self.assertEqual((self.payload_path / 'existing').read_text(),
                         'untouched\n')
        self.assertEqual(self._staging_dirs(), [])

    def test_dependency_deadline_before_spawn_never_calls_popen(self):
        popen = mock.Mock()
        with self.assertRaises(TimeoutError):
            image._run_dependency_installer(
                '/toolchains', time.monotonic() - 1, popen)
        popen.assert_not_called()

    def test_dependency_group_reaped_on_leader_exit_and_kill_errors(self):
        for code in (0, 1):
            proc = mock.Mock()
            proc.pid = 4321
            proc.wait.return_value = code
            proc.poll.return_value = code
            popen = mock.Mock(return_value=proc)
            with mock.patch.object(image.os, 'killpg') as killpg:
                if code == 0:
                    image._run_dependency_installer(
                        '/toolchains', time.monotonic() + 60, popen)
                else:
                    with self.assertRaises(RuntimeError):
                        image._run_dependency_installer(
                            '/toolchains', time.monotonic() + 60, popen)
            killpg.assert_called_once_with(4321, signal.SIGKILL)
            self.assertEqual(proc.wait.call_count, 2)

        proc = mock.Mock()
        proc.pid = 4321
        proc.wait.return_value = 0
        proc.poll.return_value = 0
        with mock.patch.object(image.os, 'killpg',
                               side_effect=OSError('denied')):
            with self.assertRaises(RuntimeError):
                image._run_dependency_installer(
                    '/toolchains', time.monotonic() + 60,
                    mock.Mock(return_value=proc))

        proc = mock.Mock()
        proc.pid = 4321
        proc.wait.return_value = 0
        proc.poll.return_value = 0
        with mock.patch.object(image.os, 'killpg',
                               side_effect=ProcessLookupError):
            image._run_dependency_installer(
                '/toolchains', time.monotonic() + 60,
                mock.Mock(return_value=proc))

    def test_signal_during_dependency_spawn_retires_group(self):
        for sig, expected in (
                (signal.SIGINT, InterruptedError),
                (signal.SIGTERM, InterruptedError),
                (signal.SIGALRM, TimeoutError)):
            proc = mock.Mock()
            proc.pid = 4321
            proc.wait.return_value = 0
            proc.poll.return_value = 0

            def signally(argv, _sig=sig, **kwargs):
                signal.getsignal(_sig)(_sig, None)
                return proc

            with mock.patch.object(image.os, 'killpg') as killpg:
                with self.assertRaises(expected, msg=str(sig)):
                    image._run_dependency_installer(
                        '/toolchains', time.monotonic() + 60, signally)
            killpg.assert_called_once_with(4321, signal.SIGKILL)

    def test_build_cli_restores_handlers_and_timer(self):
        previous = {sig: signal.getsignal(sig) for sig in
                    (signal.SIGALRM, signal.SIGINT, signal.SIGTERM)}
        with mock.patch.object(image, '_build',
                               return_value={'ok': True}), \
                contextlib.redirect_stdout(io.StringIO()):
            rc = image.main(['build', '--manifest', self.manifest_path,
                             '--deadline-unix-ms', str(self._deadline()),
                             '--ack-disposable-vm'])
        self.assertEqual(rc, 0)
        self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))
        for sig, handler in previous.items():
            self.assertIs(signal.getsignal(sig), handler)

    def test_build_cli_rejects_active_timer_without_touching_it(self):
        signal.setitimer(signal.ITIMER_REAL, 30)
        try:
            with mock.patch.object(image, '_build') as build, \
                    contextlib.redirect_stderr(io.StringIO()):
                rc = image.main(['build', '--manifest', self.manifest_path,
                                 '--deadline-unix-ms', str(self._deadline()),
                                 '--ack-disposable-vm'])
            self.assertEqual(rc, 1)
            build.assert_not_called()
            self.assertGreater(signal.getitimer(signal.ITIMER_REAL)[0], 0)
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)


if __name__ == '__main__':
    unittest.main()

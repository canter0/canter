import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import tarfile
import tempfile
import types
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location(
    'canter_ci_prepare', Path(__file__).with_name('canter-ci-prepare.py'))
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


def tarball(entries):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode='w:gz') as tf:
        for entry in entries:
            info = tarfile.TarInfo(entry[0])
            info.mode = entry[2].get('mode', 0o644) if len(entry) > 2 else 0o644
            kind = entry[1]
            if kind == 'dir':
                info.type = tarfile.DIRTYPE
                info.mode = 0o755
                tf.addfile(info)
            elif kind == 'symlink':
                info.type = tarfile.SYMTYPE
                info.linkname = entry[2]['link']
                tf.addfile(info)
            elif kind == 'hardlink':
                info.type = tarfile.LNKTYPE
                info.linkname = entry[2]['link']
                tf.addfile(info)
            elif kind == 'char' or kind == 'fifo':
                info.type = tarfile.CHRTYPE if kind == 'char' else tarfile.FIFOTYPE
                tf.addfile(info)
            else:
                data = entry[2].get('data', b'x') if len(entry) > 2 else b'x'
                info.size = len(data)
                tf.addfile(info, io.BytesIO(data))
    return buf.getvalue()


GO_TAR = tarball([
    ('go', 'dir', {}),
    ('go/bin', 'dir', {}),
    ('go/bin/go', 'file',
     {'data': b'#!/bin/sh\nif [ "$1" = version ]; then echo "go version go1.2.3 linux/amd64"; fi\n',
      'mode': 0o755}),
    ('go/VERSION', 'file', {'data': b'go1.2.3\n'}),
])
NODE_TAR = tarball([
    ('node-x', 'dir', {}),
    ('node-x/bin', 'dir', {}),
    ('node-x/lib', 'dir', {}),
    ('node-x/bin/node', 'file',
     {'data': b'#!/bin/sh\nif [ "$1" = "--version" ]; then echo v4.5.6; else exec "$1" "$2"; fi\n',
      'mode': 0o755}),
    ('node-x/lib/node_modules', 'dir', {}),
    ('node-x/lib/node_modules/npm', 'dir', {}),
    ('node-x/lib/node_modules/npm/bin', 'dir', {}),
    ('node-x/lib/node_modules/npm/bin/npm-cli.js', 'file',
     {'data': b'#!/bin/sh\necho npm\n', 'mode': 0o755}),
    ('node-x/lib/node_modules/npm/bin/npx-cli.js', 'file',
     {'data': b'#!/bin/sh\necho npx\n', 'mode': 0o755}),
    ('node-x/bin/npm', 'symlink', {'link': '../lib/node_modules/npm/bin/npm-cli.js'}),
    ('node-x/bin/npx', 'symlink', {'link': '../lib/node_modules/npm/bin/npx-cli.js'}),
])
PNPM_TAR = tarball([
    ('package', 'dir', {}),
    ('package/bin', 'dir', {}),
    ('package/bin/pnpm.cjs', 'file',
     {'data': b'#!/bin/sh\necho 7.8.9\n', 'mode': 0o755}),
    ('package/package.json', 'file', {'data': b'{}'}),
])
RUNNER_TAR = tarball([
    ('bin', 'dir', {}),
    ('bin/Runner.Listener', 'file',
     {'data': b'#!/bin/sh\necho 1.0.0\n', 'mode': 0o755}),
    ('config.sh', 'file', {'data': b'#!/bin/sh\n', 'mode': 0o755}),
    ('run.sh', 'file', {'data': b'#!/bin/sh\n', 'mode': 0o755}),
    ('env.sh', 'file', {'data': b''}),
])

TOOL_URLS = {
    'go': 'https://dl.example.test/go.tar.gz',
    'node': 'https://node.example.test/node.tar.gz',
    'pnpm': 'https://npm.example.test/pnpm.tgz',
    'runner': 'https://gh.example.test/runner.tar.gz',
}
TOOL_FILES = {'go': GO_TAR, 'node': NODE_TAR, 'pnpm': PNPM_TAR, 'runner': RUNNER_TAR}
URL_FILES = {TOOL_URLS[tool]: TOOL_FILES[tool] for tool in TOOL_FILES}
TOOL_META = {
    'go': ('1.2.3', 'sha256', 'go/'),
    'node': ('4.5.6', 'sha256', 'node-x/'),
    'pnpm': ('7.8.9', 'sha512', 'package/'),
    'runner': ('1.0.0', 'sha256', ''),
}


def make_manifest(overrides=None, file_map=None):
    file_map = file_map or TOOL_FILES
    manifest = {'schemaVersion': 1, 'platform': 'linux-x64'}
    for tool, (version, algorithm, strip) in TOOL_META.items():
        data = file_map[tool]
        manifest[tool] = {
            'version': version,
            'url': TOOL_URLS[tool],
            'algorithm': algorithm,
            'digest': hashlib.new(algorithm, data).hexdigest(),
            'stripPrefix': strip,
        }
    if overrides:
        for tool, patch in overrides.items():
            if tool == '__top__':
                manifest.update(patch)
            else:
                manifest[tool].update(patch)
    return manifest


class FakeResponse:
    def __init__(self, data):
        self._stream = io.BytesIO(data)

    def read(self, size=-1):
        return self._stream.read(size)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class FakeOpener:
    def __init__(self, files=None, fail_urls=None):
        self.files = files if files is not None else dict(URL_FILES)
        self.fail_urls = fail_urls or set()
        self.requests = []

    def open(self, request, timeout=None):
        url = getattr(request, 'full_url', None) or str(request)
        self.requests.append(url)
        if url in self.fail_urls:
            raise OSError('network down')
        return FakeResponse(self.files[url])


class PrepareCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.prefix = self.base / 'prefix'
        self.account = types.SimpleNamespace(pw_name='canter', pw_uid=1000, pw_gid=1000)
        self.chown_calls = []
        self.published = []
        patcher = mock.patch.object(prepare, '_publish_no_replace', self.fake_publish)
        patcher.start()
        self.addCleanup(patcher.stop)

    def fake_publish(self, src, dst):
        self.published.append((src, dst))
        os.rename(src, dst)

    def chown(self, path, uid, gid, follow_symlinks=True):
        self.chown_calls.append((str(path), uid, gid, follow_symlinks))

    def getpwnam(self, name):
        if name != 'canter':
            raise KeyError(name)
        return self.account

    def toolset(self, **overrides):
        file_map = dict(TOOL_FILES)
        file_map.update(overrides)
        files = {TOOL_URLS[tool]: file_map[tool] for tool in file_map}
        return files, make_manifest(file_map=file_map)

    def run_prepare(self, manifest=None, files=None, prefix=None, owner='canter',
                    opener=None):
        manifest = manifest if manifest is not None else make_manifest()
        return prepare.prepare(
            manifest, str(prefix or self.prefix), owner,
            opener=opener or FakeOpener(files),
            chown=self.chown, getpwnam=self.getpwnam)

    def leftovers(self):
        return [name for name in os.listdir(self.base)
                if name.startswith('.canter-ci-')]

    def test_prepare_installs_expected_layout(self):
        result = self.run_prepare()
        prefix = Path(result['prefix'])
        self.assertEqual(prefix, self.prefix)
        self.assertEqual(set(result['versions']),
                         {'go', 'node', 'pnpm', 'runner'})
        self.assertIn('not a sanitized bootable image', result['note'])
        go = prefix / 'toolcache' / 'go' / '1.2.3'
        self.assertTrue((go / 'x64' / 'bin' / 'go').is_file())
        self.assertTrue((go / 'x64' / 'VERSION').is_file())
        self.assertTrue((go / 'x64.complete').is_file())
        node = prefix / 'toolcache' / 'node' / '4.5.6'
        self.assertTrue((node / 'x64' / 'bin' / 'node').is_file())
        self.assertTrue((node / 'x64.complete').is_file())
        self.assertTrue((prefix / 'pnpm' / '7.8.9' / 'bin' / 'pnpm.cjs').is_file())
        self.assertTrue((prefix / 'runner' / 'config.sh').is_file())
        self.assertTrue((prefix / 'runner' / 'run.sh').is_file())
        self.assertEqual(json.loads((prefix / 'manifest.json').read_text()),
                         prepare.validate_manifest(make_manifest()))
        self.assertEqual(stat.S_IMODE(os.stat(prefix).st_mode), 0o755)
        for name in ('go', 'node', 'npm', 'npx', 'pnpm'):
            link = prefix / 'bin' / name
            self.assertTrue(link.is_symlink(), name)
            self.assertTrue(os.path.exists(link), name + ' resolves')
        npm_target = os.path.realpath(prefix / 'bin' / 'npm')
        self.assertIn(str(prefix), npm_target)
        self.assertEqual(self.leftovers(), [])

    def test_prepare_chowns_runner_and_toolcache_dirs_only(self):
        self.run_prepare()
        paths = [call[0] for call in self.chown_calls]
        self.assertTrue(any(p.endswith(os.path.join('runner', 'config.sh'))
                            for p in paths))
        self.assertTrue(any(os.path.join('toolcache') in p for p in paths))
        self.assertFalse(any(p.endswith(os.path.join('x64', 'bin', 'go'))
                             for p in paths))
        self.assertFalse(any(p.endswith('pnpm.cjs') for p in paths))
        for call in self.chown_calls:
            self.assertEqual((call[1], call[2]), (1000, 1000))

    def test_prepare_rejects_root_owner(self):
        self.account.pw_uid = 0
        with self.assertRaisesRegex(RuntimeError, 'non-root'):
            self.run_prepare()
        self.assertFalse(self.prefix.exists())

    def test_prepare_rejects_unknown_owner(self):
        with self.assertRaisesRegex(RuntimeError, 'unknown owner'):
            self.run_prepare(owner='ghost')

    def test_prepare_never_overwrites_existing_prefix(self):
        self.prefix.mkdir()
        sentinel = self.prefix / 'keep.txt'
        sentinel.write_text('precious')
        opener = FakeOpener()
        with self.assertRaisesRegex(RuntimeError, 'already exists'):
            self.run_prepare(opener=opener)
        self.assertEqual(sentinel.read_text(), 'precious')
        self.assertEqual(opener.requests, [])

    def test_checksum_mismatch_fails_before_extraction(self):
        files = dict(URL_FILES)
        files[TOOL_URLS['go']] = files[TOOL_URLS['go']][:-8] + b'CORRUPT!'
        opener = FakeOpener(files)
        with self.assertRaisesRegex(RuntimeError, 'checksum'):
            self.run_prepare(opener=opener)
        self.assertFalse(self.prefix.exists())
        self.assertEqual(self.leftovers(), [])

    def test_download_failure_is_sanitized(self):
        opener = FakeOpener(fail_urls={TOOL_URLS['go']})
        with self.assertRaisesRegex(RuntimeError, 'download failed for go') as ctx:
            self.run_prepare(opener=opener)
        self.assertNotIn(TOOL_URLS['go'], str(ctx.exception))
        self.assertFalse(self.prefix.exists())
        self.assertEqual(self.leftovers(), [])

    def test_manifest_validation_rejects_bad_values(self):
        cases = []

        def add(label, mutate):
            manifest = make_manifest()
            mutate(manifest)
            cases.append((label, manifest))

        add('schema', lambda m: m.update(schemaVersion=2))
        add('platform', lambda m: m.update(platform='linux-arm64'))
        add('extra tool', lambda m: m.update(cargo={}))
        add('missing tool', lambda m: m.pop('node'))
        add('empty version', lambda m: m['go'].update(version=''))
        add('padded version', lambda m: m['go'].update(version=' 1.2.3 '))
        add('traversal version', lambda m: m['go'].update(version='../1.2.3'))
        add('absolute version', lambda m: m['go'].update(version='/1.2.3'))
        add('nonnumeric version', lambda m: m['go'].update(version='1.2'))
        add('prerelease version', lambda m: m['go'].update(version='1.2.3-rc.1'))
        add('build version', lambda m: m['go'].update(version='1.2.3+build'))
        add('integer version', lambda m: m['go'].update(version=123))
        add('list version', lambda m: m['go'].update(version=['1.2.3']))
        add('string spec', lambda m: m.update(go='not-a-dict'))
        add('list spec', lambda m: m.update(node=['version']))
        add('http url', lambda m: m['go'].update(url='http://dl.example.test/x'))
        add('userinfo url', lambda m: m['go'].update(url='https://u:p@dl.example.test/x'))
        add('query url', lambda m: m['go'].update(url='https://dl.example.test/x?a=1'))
        add('fragment url', lambda m: m['go'].update(url='https://dl.example.test/x#f'))
        add('bad algorithm', lambda m: m['go'].update(algorithm='md5'))
        add('short digest', lambda m: m['go'].update(digest='ab' * 10))
        add('nonhex digest', lambda m: m['go'].update(digest='z' * 64))
        add('sha512 short', lambda m: m['pnpm'].update(digest='ab' * 32))
        add('absolute strip', lambda m: m['go'].update(stripPrefix='/abs/'))
        add('dotdot strip', lambda m: m['go'].update(stripPrefix='../x/'))
        add('inner dotdot strip', lambda m: m['go'].update(stripPrefix='a/../b/'))
        add('double slash strip', lambda m: m['go'].update(stripPrefix='a//b/'))
        add('unknown tool key', lambda m: m['go'].update(extra='x'))
        add('missing tool key', lambda m: m['go'].pop('digest'))
        for label, manifest in cases:
            with self.subTest(label=label):
                opener = FakeOpener()
                with self.assertRaises((ValueError, RuntimeError)):
                    self.run_prepare(manifest=manifest, opener=opener)
                self.assertEqual(opener.requests, [])
                self.assertFalse(self.prefix.exists())

    def extract(self, entries, strip=''):
        archive = self.base / 'in.tar.gz'
        archive.write_bytes(tarball(entries))
        dest = self.base / 'out'
        prepare.extract_archive(str(archive), str(dest), strip)
        return dest

    def test_extraction_rejects_traversal_and_bad_members(self):
        cases = [
            ([('pkg', 'dir', {}), ('../evil.txt', 'file', {})], ''),
            ([('pkg', 'dir', {}), ('/abs.txt', 'file', {})], ''),
            ([('pkg', 'dir', {}), ('a/../../evil.txt', 'file', {})], ''),
            ([('pkg', 'dir', {}), ('pkg/../sneaky.txt', 'file', {})], ''),
            ([('pkg', 'dir', {}), ('go/../sneaky.txt', 'file', {})], 'go'),
            ([('pkg', 'dir', {}), ('pkg/link', 'symlink', {'link': '/etc/passwd'})], ''),
            ([('pkg', 'dir', {}), ('pkg/link', 'symlink', {'link': '../../../etc/x'})], ''),
            ([('pkg', 'dir', {}), ('pkg/up', 'symlink', {'link': '..'}),
              ('evil', 'symlink', {'link': 'pkg/up/../../sneaky'})], ''),
            ([('pkg', 'dir', {}), ('pkg/a.txt', 'file', {}),
              ('pkg/b.txt', 'hardlink', {'link': '../outside.txt'})], ''),
            ([('pkg', 'dir', {}), ('pkg/dev', 'char', {})], ''),
            ([('pkg', 'dir', {}), ('pkg/pipe', 'fifo', {})], ''),
        ]
        for index, (entries, strip) in enumerate(cases):
            with self.subTest(case=index):
                with self.assertRaises(RuntimeError):
                    self.extract(entries, strip)

    def test_extraction_skips_members_outside_strip_prefix(self):
        dest = self.extract([
            ('go', 'dir', {}),
            ('go/bin', 'dir', {}),
            ('go/bin/go', 'file', {'data': b'bin'}),
            ('other/evil.txt', 'file', {'data': b'no'}),
        ], strip='go')
        self.assertTrue((dest / 'bin' / 'go').is_file())
        self.assertFalse((dest / 'evil.txt').exists())
        self.assertFalse((dest / 'other').exists())

    def test_extraction_allows_contained_symlink_and_hardlink(self):
        dest = self.extract([
            ('pkg', 'dir', {}),
            ('pkg/real.txt', 'file', {'data': b'data'}),
            ('pkg/link.txt', 'symlink', {'link': 'real.txt'}),
            ('pkg/sub', 'dir', {}),
            ('pkg/sub/alias.txt', 'symlink', {'link': '../real.txt'}),
            ('pkg/copy.txt', 'hardlink', {'link': 'pkg/real.txt'}),
        ], strip='')
        self.assertEqual((dest / 'pkg' / 'link.txt').read_bytes(), b'data')
        self.assertEqual((dest / 'pkg' / 'sub' / 'alias.txt').read_bytes(), b'data')
        self.assertEqual((dest / 'pkg' / 'copy.txt').read_bytes(), b'data')
        self.assertTrue(os.path.islink(dest / 'pkg' / 'link.txt'))

    def test_extraction_allows_writes_through_contained_symlink_ancestor(self):
        dest = self.extract([
            ('pkg', 'dir', {}),
            ('pkg/subdir', 'dir', {}),
            ('pkg/subdir/real.txt', 'file', {'data': b'data'}),
            ('pkg/link', 'symlink', {'link': 'subdir'}),
            ('pkg/link/alias.txt', 'file', {'data': b'via-link'}),
        ], strip='')
        self.assertEqual((dest / 'pkg' / 'subdir' / 'alias.txt').read_bytes(),
                         b'via-link')

    def test_extraction_allows_forward_contained_symlink(self):
        dest = self.extract([
            ('pkg', 'dir', {}),
            ('pkg/fwd', 'symlink', {'link': 'later/file.txt'}),
            ('pkg/later', 'dir', {}),
            ('pkg/later/file.txt', 'file', {'data': b'data'}),
        ], strip='')
        self.assertEqual((dest / 'pkg' / 'fwd').read_bytes(), b'data')

    def test_extraction_rejects_hardlink_to_symlink(self):
        with self.assertRaisesRegex(RuntimeError, 'regular file'):
            self.extract([
                ('pkg', 'dir', {}),
                ('pkg/real.txt', 'file', {'data': b'data'}),
                ('pkg/lnk', 'symlink', {'link': 'real.txt'}),
                ('pkg/copy.txt', 'hardlink', {'link': 'pkg/lnk'}),
            ], strip='')

    def test_extraction_rejects_dir_target_symlink_escape(self):
        sentinel = self.base / 'sentinel'
        sentinel.mkdir()
        sentinel.chmod(0o700)
        dest = self.base / 'out'
        dest.mkdir()
        os.symlink(str(sentinel), dest / 'link')
        archive = self.base / 'dir-escape.tar.gz'
        archive.write_bytes(tarball([('link', 'dir', {})]))
        with self.assertRaisesRegex(RuntimeError, 'escape'):
            prepare.extract_archive(str(archive), str(dest), '')
        self.assertEqual(stat.S_IMODE(os.stat(sentinel).st_mode), 0o700)
        self.assertEqual(list(sentinel.iterdir()), [])

    def test_extraction_rejects_file_write_through_symlink_escape(self):
        sentinel = self.base / 'sentinel'
        sentinel.mkdir()
        dest = self.base / 'out'
        dest.mkdir()
        os.symlink(str(sentinel), dest / 'link')
        archive = self.base / 'file-escape.tar.gz'
        archive.write_bytes(tarball([
            ('link/evil.txt', 'file', {'data': b'no'}),
        ]))
        with self.assertRaises(RuntimeError):
            prepare.extract_archive(str(archive), str(dest), '')
        self.assertEqual(list(sentinel.iterdir()), [])

    def runner_tar(self, extra=(), drop=(), listener=b'#!/bin/sh\necho 1.0.0\n'):
        entries = [
            ('bin', 'dir', {}),
            ('bin/Runner.Listener', 'file', {'data': listener, 'mode': 0o755}),
            ('config.sh', 'file', {'data': b'#!/bin/sh\n', 'mode': 0o755}),
            ('run.sh', 'file', {'data': b'#!/bin/sh\n', 'mode': 0o755}),
        ]
        entries = [entry for entry in entries if entry[0] not in drop]
        return tarball(entries + list(extra))

    def test_prepare_rejects_forbidden_runner_state(self):
        cases = [(name, [(name, 'file', {'data': b'x'})])
                 for name in prepare.RESERVED_NAMES]
        cases.append(('nested _work', [('sub', 'dir', {}),
                                       ('sub/_work', 'dir', {}),
                                       ('sub/_work/job', 'file', {})]))
        for label, extra in cases:
            with self.subTest(label=label):
                files, manifest = self.toolset(runner=self.runner_tar(extra=extra))
                with self.assertRaisesRegex(RuntimeError, 'forbidden runner state'):
                    self.run_prepare(manifest=manifest, opener=FakeOpener(files))
                self.assertFalse(self.prefix.exists())
                self.assertEqual(self.leftovers(), [])

    def test_prepare_rejects_wrong_tool_version(self):
        bad_go = tarball([
            ('go', 'dir', {}),
            ('go/bin', 'dir', {}),
            ('go/bin/go', 'file',
             {'data': b'#!/bin/sh\nif [ "$1" = version ]; then echo "go version go9.9.9 linux/amd64"; fi\n',
              'mode': 0o755}),
        ])
        files, manifest = self.toolset(go=bad_go)
        with self.assertRaisesRegex(RuntimeError, 'go version check failed'):
            self.run_prepare(manifest=manifest, opener=FakeOpener(files))
        self.assertFalse(self.prefix.exists())
        self.assertEqual(self.leftovers(), [])

    def test_prepare_rejects_runner_missing_scripts(self):
        for name in ('config.sh', 'run.sh'):
            with self.subTest(name=name):
                files, manifest = self.toolset(
                    runner=self.runner_tar(drop=(name,)))
                with self.assertRaisesRegex(RuntimeError, 'missing runner/' + name):
                    self.run_prepare(manifest=manifest, opener=FakeOpener(files))
                self.assertFalse(self.prefix.exists())
                self.assertEqual(self.leftovers(), [])

    def test_prepare_reports_runner_presence_only(self):
        files, manifest = self.toolset(
            runner=self.runner_tar(listener=b'#!/bin/sh\nexit 1\n'))
        result = self.run_prepare(manifest=manifest, opener=FakeOpener(files))
        self.assertIn('presence only', result['versions']['runner'])
        self.assertTrue(self.prefix.exists())

    def test_publish_race_preserves_existing_prefix(self):
        def concurrent(src, dst):
            os.mkdir(dst)
            raise RuntimeError('prefix appeared during preparation; refusing to overwrite')
        files, manifest = self.toolset()
        with mock.patch.object(prepare, '_publish_no_replace',
                               side_effect=concurrent):
            with self.assertRaisesRegex(RuntimeError, 'refusing to overwrite'):
                self.run_prepare(manifest=manifest, opener=FakeOpener(files))
        self.assertTrue(self.prefix.is_dir())
        self.assertEqual(list(self.prefix.iterdir()), [])
        self.assertEqual(self.leftovers(), [])

    def fake_prefix(self, root, listener_script='#!/bin/sh\necho 1.0.0\n'):
        root = Path(root)
        root.mkdir(parents=True, exist_ok=True)
        manifest = make_manifest()
        (root / 'manifest.json').write_text(json.dumps(manifest))
        for tool in ('go', 'node'):
            version = manifest[tool]['version']
            version_root = root / 'toolcache' / tool / version
            base = version_root / 'x64'
            (base / 'bin').mkdir(parents=True)
            (version_root / 'x64.complete').write_text('')
            binary = base / 'bin' / ('go' if tool == 'go' else 'node')
            if tool == 'go':
                binary.write_text('#!/bin/sh\nif [ "$1" = version ]; then echo "go version go1.2.3 linux/amd64"; fi\n')
            else:
                binary.write_text('#!/bin/sh\nif [ "$1" = "--version" ]; then echo v4.5.6; else exec "$1" "$2"; fi\n')
            binary.chmod(0o755)
        pnpm = root / 'pnpm' / '7.8.9' / 'bin' / 'pnpm.cjs'
        pnpm.parent.mkdir(parents=True)
        pnpm.write_text('#!/bin/sh\necho 7.8.9\n')
        pnpm.chmod(0o755)
        runner_bin = root / 'runner' / 'bin'
        runner_bin.mkdir(parents=True)
        listener = runner_bin / 'Runner.Listener'
        listener.write_text(listener_script)
        listener.chmod(0o755)
        for name in ('config.sh', 'run.sh'):
            script = root / 'runner' / name
            script.write_text('#!/bin/sh\nexit 0\n')
            script.chmod(0o755)
        bindir = root / 'bin'
        bindir.mkdir()
        for name, target in (
                ('go', '../toolcache/go/1.2.3/x64/bin/go'),
                ('node', '../toolcache/node/4.5.6/x64/bin/node'),
                ('npm', '../toolcache/node/4.5.6/x64/bin/npm'),
                ('npx', '../toolcache/node/4.5.6/x64/bin/npx'),
                ('pnpm', '../pnpm/7.8.9/bin/pnpm.cjs')):
            os.symlink(target, bindir / name)
        for name in ('npm', 'npx'):
            cli = root / 'toolcache' / 'node' / '4.5.6' / 'x64' / 'lib' / 'node_modules' / 'npm' / 'bin' / (name + '-cli.js')
            cli.parent.mkdir(parents=True, exist_ok=True)
            cli.write_text('#!/bin/sh\necho ok\n')
            cli.chmod(0o755)
            os.symlink('../lib/node_modules/npm/bin/' + name + '-cli.js',
                        root / 'toolcache' / 'node' / '4.5.6' / 'x64' / 'bin' / name)
        return manifest

    def test_verify_only_validates_without_writes(self):
        root = self.base / 'installed'
        manifest = self.fake_prefix(root)
        before = sorted(str(p) for p in root.rglob('*'))
        result = prepare.verify_prefix(str(root), manifest)
        after = sorted(str(p) for p in root.rglob('*'))
        self.assertEqual(before, after)
        self.assertEqual(result['versions']['go'], '1.2.3')
        self.assertEqual(result['versions']['node'], '4.5.6')
        self.assertEqual(result['versions']['pnpm'], '7.8.9')
        self.assertEqual(result['versions']['runner'], '1.0.0 (presence only)')
        self.assertIn('not a sanitized bootable image', result['note'])

    def test_verify_only_runner_falls_back_to_presence(self):
        root = self.base / 'installed2'
        manifest = self.fake_prefix(root, listener_script='#!/bin/sh\nexit 1\n')
        result = prepare.verify_prefix(str(root), manifest)
        self.assertIn('presence only', result['versions']['runner'])

    def test_verify_only_rejects_broken_prefix(self):
        root = self.base / 'broken'
        manifest = self.fake_prefix(root)
        os.unlink(root / 'toolcache' / 'go' / '1.2.3' / 'x64.complete')
        with self.assertRaisesRegex(RuntimeError, 'x64.complete'):
            prepare.verify_prefix(str(root), manifest)
        os.unlink(root / 'runner' / 'config.sh')
        with self.assertRaises(RuntimeError):
            prepare.verify_prefix(str(root), manifest)

    def test_verify_only_uses_scrubbed_version_env(self):
        root = self.base / 'installed-env'
        manifest = self.fake_prefix(root)
        calls = []

        def fake_run(argv, **kwargs):
            calls.append((argv, kwargs))
            name = os.path.basename(argv[0])
            if name == 'go':
                out = 'go version go1.2.3 linux/amd64'
            elif name == 'node':
                out = '7.8.9' if len(argv) > 2 else 'v4.5.6'
            else:
                out = '1.0.0'
            return types.SimpleNamespace(returncode=0, stdout=out + '\n', stderr='')

        with mock.patch.dict(os.environ, {'AWS_SECRET_ACCESS_KEY': 'sekrit',
                                          'GOPROXY': 'https://evil.example'}), \
                mock.patch.object(prepare.subprocess, 'run', side_effect=fake_run):
            prepare.verify_prefix(str(root), manifest)
        self.assertEqual(len(calls), 3)
        for argv, kwargs in calls:
            self.assertNotEqual(os.path.basename(argv[0]), 'Runner.Listener')
            self.assertEqual(kwargs['cwd'], '/')
            self.assertEqual(set(kwargs['env']), set(prepare.VERSION_ENV))
            self.assertEqual(kwargs['env']['GOTOOLCHAIN'], 'local')
            self.assertEqual(kwargs['env']['GOPROXY'], 'off')
            self.assertEqual(kwargs['env']['GOSUMDB'], 'off')
            self.assertEqual(kwargs['env']['GOTELEMETRY'], 'off')
            self.assertNotIn('AWS_SECRET_ACCESS_KEY', kwargs['env'])

    def test_verify_only_rejects_version_output_drift(self):
        cases = [
            ('node', 'v4.5.6-extra', 'node version check failed'),
            ('go', 'go version go1.2.3 linux/amd64 extra', 'go version check failed'),
            ('pnpm', '7.8.9-extra', 'pnpm version check failed'),
        ]
        for label, drift, message in cases:
            with self.subTest(label=label):
                root = self.base / ('drift-' + label)
                manifest = self.fake_prefix(root)

                def fake_run(argv, drift=drift, label=label, **kwargs):
                    name = os.path.basename(argv[0])
                    if name == 'node' and len(argv) > 2:
                        key = 'pnpm'
                    elif name == 'node':
                        key = 'node'
                    elif name == 'go':
                        key = 'go'
                    else:
                        key = 'runner'
                    if key == label:
                        out = drift
                    elif key == 'go':
                        out = 'go version go1.2.3 linux/amd64'
                    elif key == 'node':
                        out = 'v4.5.6'
                    elif key == 'pnpm':
                        out = '7.8.9'
                    else:
                        out = '1.0.0'
                    return types.SimpleNamespace(returncode=0, stdout=out, stderr='')

                with mock.patch.object(prepare.subprocess, 'run',
                                       side_effect=fake_run):
                    with self.assertRaisesRegex(RuntimeError, message):
                        prepare.verify_prefix(str(root), manifest)

    def test_verify_only_never_runs_binaries_after_structural_errors(self):
        root = self.base / 'escaping'
        manifest = self.fake_prefix(root)
        os.unlink(root / 'bin' / 'go')
        os.symlink('/etc/passwd', root / 'bin' / 'go')
        with mock.patch.object(prepare.subprocess, 'run') as run:
            with self.assertRaises(RuntimeError):
                prepare.verify_prefix(str(root), manifest)
        run.assert_not_called()

    def test_main_verify_only_needs_no_root_or_network(self):
        root = self.base / 'installed3'
        manifest = self.fake_prefix(root)
        manifest_path = self.base / 'manifest.json'
        manifest_path.write_text(json.dumps(manifest))
        out = io.StringIO()
        with mock.patch.object(prepare.platform, 'system', return_value='Linux'), \
                mock.patch.object(prepare.platform, 'machine', return_value='x86_64'), \
                mock.patch.object(prepare.os, 'geteuid', return_value=1000), \
                contextlib.redirect_stdout(out):
            rc = prepare.main(['--manifest', str(manifest_path),
                               '--prefix', str(root), '--verify-only'])
        self.assertEqual(rc, 0)
        payload = json.loads(out.getvalue())
        self.assertEqual(payload['prefix'], str(root))
        self.assertIn('note', payload)

    def test_main_rejects_non_linux_and_nonroot_prepare(self):
        manifest_path = self.base / 'manifest.json'
        manifest_path.write_text(json.dumps(make_manifest()))
        with mock.patch.object(prepare.platform, 'system', return_value='Darwin'):
            with self.assertRaisesRegex(RuntimeError, 'Linux'):
                prepare.main(['--manifest', str(manifest_path),
                              '--prefix', str(self.prefix), '--owner', 'canter'])
        with mock.patch.object(prepare.platform, 'system', return_value='Linux'), \
                mock.patch.object(prepare.platform, 'machine', return_value='x86_64'), \
                mock.patch.object(prepare.os, 'geteuid', return_value=1000):
            with self.assertRaisesRegex(RuntimeError, 'root'):
                prepare.main(['--manifest', str(manifest_path),
                              '--prefix', str(self.prefix), '--owner', 'canter'])
        with mock.patch.object(prepare.platform, 'system', return_value='Linux'), \
                mock.patch.object(prepare.platform, 'machine', return_value='x86_64'), \
                mock.patch.object(prepare.os, 'geteuid', return_value=0):
            with self.assertRaisesRegex(RuntimeError, 'owner'):
                prepare.main(['--manifest', str(manifest_path),
                              '--prefix', str(self.prefix)])


if __name__ == '__main__':
    unittest.main()

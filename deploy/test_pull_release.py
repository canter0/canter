import gzip
import grp
import hashlib
import importlib.util
import io
import json
import pwd
from pathlib import Path
import tempfile
import tarfile
import unittest
from unittest.mock import call, patch

spec = importlib.util.spec_from_file_location('release', Path(__file__).with_name('pull-release.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class DownloadTests(unittest.TestCase):
    def test_download_streams_bounded_chunks_and_hashes_content(self):
        content = b'x' * (release.DOWNLOAD_CHUNK_SIZE * 2 + 7)

        class Response:
            def __init__(self):
                self.position = 0
                self.read_sizes = []

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self, size):
                self.read_sizes.append(size)
                chunk = content[self.position:self.position + size]
                self.position += len(chunk)
                return chunk

        response = Response()
        with tempfile.TemporaryFile() as output, patch.object(release.urllib.request, 'urlopen', return_value=response):
            digest = release.download('https://example.invalid/release.tar.gz', output)
            output.seek(0)
            self.assertEqual(output.read(), content)

        self.assertEqual(digest, hashlib.sha256(content).hexdigest())
        self.assertEqual(response.read_sizes, [release.DOWNLOAD_CHUNK_SIZE] * 4)

    def test_download_rejects_oversized_archive(self):
        class Response:
            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self, size):
                return b'abcd'

        with tempfile.TemporaryFile() as output, \
                patch.object(release, 'MAX_ARCHIVE_BYTES', 3), \
                patch.object(release.urllib.request, 'urlopen', return_value=Response()):
            with self.assertRaisesRegex(RuntimeError, 'compressed size limit'):
                release.download('https://example.invalid/release.tar.gz', output)
            self.assertEqual(output.tell(), 0)


class ArchiveLimitTests(unittest.TestCase):
    def test_rejects_too_many_members(self):
        class Archive:
            def __iter__(self):
                for _ in range(3):
                    yield type('Member', (), {'size': 0})()

        with patch.object(release, 'MAX_ARCHIVE_MEMBERS', 2):
            with self.assertRaisesRegex(RuntimeError, 'too many entries'):
                release.validate_archive(Archive())

    def test_rejects_excessive_expanded_size(self):
        class Archive:
            def __iter__(self):
                yield type('Member', (), {'size': 4})()
                yield type('Member', (), {'size': 1})()

        with patch.object(release, 'MAX_EXPANDED_BYTES', 4):
            with self.assertRaisesRegex(RuntimeError, 'expanded size limit'):
                release.validate_archive(Archive())

    @staticmethod
    def header(name, size, kind):
        header = bytearray(512)
        header[:len(name)] = name
        header[100:108] = b'0000644\0'
        header[108:116] = b'0000000\0'
        header[116:124] = b'0000000\0'
        header[124:136] = f'{size:011o}\0'.encode()
        header[136:148] = b'00000000000\0'
        header[148:156] = b'        '
        header[156:157] = kind
        header[257:263] = b'ustar\0'
        header[263:265] = b'00'
        checksum = sum(header)
        header[148:156] = f'{checksum:06o}\0 '.encode()
        return bytes(header)

    def test_rejects_oversized_pax_and_gnu_longname_before_body_read(self):
        # These tiny gzip fixtures advertise a large metadata body but contain
        # only an end marker. The parser must reject from the header itself.
        for kind in (b'x', b'L'):
            with self.subTest(kind=kind):
                archive = io.BytesIO(gzip.compress(
                    self.header(b'metadata', 4096, kind) + bytes(1024)))
                with patch.object(release, 'MAX_TAR_METADATA_BYTES', 32):
                    with self.assertRaisesRegex(RuntimeError, 'metadata record'):
                        with release.BoundedTar(archive) as tar:
                            release.validate_archive(tar)

    def test_raw_expanded_limit_counts_tar_padding_and_headers(self):
        raw = self.header(b'file', 1, b'0') + b'x' + bytes(511) + bytes(1024)
        archive = io.BytesIO(gzip.compress(raw))
        # The file payload is one byte, but its padded tar stream plus headers
        # exceeds this limit.
        with patch.object(release, 'MAX_EXPANDED_BYTES', 1024):
            with self.assertRaisesRegex(RuntimeError, 'expanded size limit'):
                with release.BoundedTar(archive) as tar:
                    release.validate_archive(tar)

    def test_valid_gnu_and_pax_archives_remain_readable(self):
        long_name = 'nested/' + ('n' * 120)
        for archive_format in (tarfile.GNU_FORMAT, tarfile.PAX_FORMAT):
            with self.subTest(format=archive_format):
                raw = io.BytesIO()
                with tarfile.open(fileobj=raw, mode='w', format=archive_format) as tar:
                    info = tarfile.TarInfo(long_name)
                    info.size = 1
                    tar.addfile(info, io.BytesIO(b'x'))
                compressed = io.BytesIO(gzip.compress(raw.getvalue()))
                with release.BoundedTar(compressed) as tar:
                    release.validate_archive(tar)
                with tempfile.TemporaryDirectory() as directory:
                    with release.BoundedTar(compressed) as tar:
                        for member in tar:
                            tar.extract(member, directory, filter='data')
                    self.assertEqual((Path(directory) / long_name).read_bytes(), b'x')

    def test_rejects_unverified_or_concatenated_gzip_data(self):
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode='w') as tar:
            info = tarfile.TarInfo('file')
            info.size = 1
            tar.addfile(info, io.BytesIO(b'x'))
        valid = gzip.compress(raw.getvalue())
        bad_crc = valid[:-8] + bytes([valid[-8] ^ 1]) + valid[-7:]
        fixtures = {
            'bad CRC': bad_crc,
            'trailing byte': valid + b'x',
            'second gzip member': valid + gzip.compress(b'extra'),
            'truncated trailer': valid[:-4],
        }
        for label, data in fixtures.items():
            with self.subTest(label=label):
                with self.assertRaises((RuntimeError, EOFError, release.zlib.error)):
                    with release.BoundedTar(io.BytesIO(data)) as tar:
                        release.validate_archive(tar)


class ActivationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.root = root / 'opt'
        self.state = root / 'state'
        self.state.mkdir()
        (self.root / 'bin').mkdir(parents=True)
        self.root.chmod(0o750)
        (self.root / 'bin/canter-controlplane').write_text('old-binary')
        (self.root / 'web').mkdir()
        (self.root / 'web').chmod(0o750)
        (self.root / 'web/page').write_text('old-web')
        (self.root / 'web/.next/cache').mkdir(parents=True)
        self.next = root / 'release'
        (self.next / 'web').mkdir(parents=True)
        (self.next / 'web/page').write_text('new-web')
        (self.next / 'web/.next').mkdir()
        (self.next / 'canter-controlplane').write_text('new-binary')
        self.sha = 'a' * 40
        self.harness = root / 'harness'
        self.systemd = root / 'systemd'
        self.systemd.mkdir()
        (self.systemd / 'canter-web.service').write_text('User=canter\nGroup=canter\n')
        (self.next / 'harness/node_modules/just-bash').mkdir(parents=True)
        (self.next / 'harness/runner.mjs').write_text('new-runner')
        (self.next / 'harness/node_modules/just-bash/package.json').write_text('{}')
        (self.next / 'deploy').mkdir()
        for name in release.RUNTIME_UNITS:
            content = 'User=canter-web\nGroup=canter-web\n' if name == 'canter-web.service' else 'new-unit'
            (self.next / 'deploy' / name).write_text(content)
        (self.next / 'canter-static-linux').write_text('new-static')
        p = patch.object(release, 'unit_state', return_value=False)
        p.start()
        self.addCleanup(p.stop)
        for name, value in [('ROOT', self.root), ('STATE', self.state), ('HARNESS', self.harness), ('SYSTEMD', self.systemd)]:
            p = patch.object(release, name, value)
            p.start()
            self.addCleanup(p.stop)
        p = patch.object(release, 'ensure_web_account')
        self.ensure_web_account = p.start()
        self.addCleanup(p.stop)

    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    @patch.object(release, 'healthy', return_value=True)
    def test_repository_traced_web_cache_matches_packaged_layout(self, health, run, subprocess):
        for tree in (self.root / 'web', self.next / 'web'):
            (tree / 'web').mkdir()
            (tree / 'web/server.js').write_text('next-server')
            (tree / '.next').rename(tree / 'web/.next')
        release.activate(self.next, self.sha)
        self.assertTrue((self.root / 'web/web/.next/cache').is_dir())
        self.assertFalse((self.root / 'web/.next').exists())
        self.assertEqual((self.systemd / 'canter-web.service').read_text(), 'User=canter-web\nGroup=canter-web\n')

    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    @patch.object(release, 'healthy', return_value=True)
    def test_success_retains_backup_and_records_commit(self, health, run, subprocess):
        self.assertEqual((self.systemd / 'canter-web.service').read_text(), 'User=canter\nGroup=canter\n')
        self.assertEqual(self.root.stat().st_mode & 0o777, 0o750)
        release.activate(self.next, self.sha)
        self.assertEqual((self.root / 'bin/canter-controlplane').read_text(), 'new-binary')
        self.assertEqual((self.root / 'web/page').read_text(), 'new-web')
        self.assertEqual((self.next / 'previous-web/page').read_text(), 'old-web')
        self.assertEqual((self.state / 'current').read_text(), self.sha)
        self.assertEqual((self.root / 'bin/canter-static-linux').read_text(), 'new-static')
        self.assertEqual((self.harness / 'current/runner.mjs').read_text(), 'new-runner')
        self.assertTrue((self.harness / 'current').is_symlink())
        run.assert_any_call('systemctl', 'enable', '--now', 'canter-harness.socket')
        self.assertNotIn(('systemctl', 'stop', 'canter-harness.socket'), [c.args for c in run.call_args_list])
        self.assertTrue(any('pg_restore' in call.args[0] for call in subprocess.call_args_list))
        self.ensure_web_account.assert_called_once_with()
        self.assertEqual((self.root / 'web/.next/cache').stat().st_mode & 0o700, 0o700)
        self.assertEqual(sum(c.args[:3] == ('chown', '-R', 'canter-web:canter-web') for c in run.call_args_list), 2)
        self.assertEqual((self.systemd / 'canter-web.service').read_text(), 'User=canter-web\nGroup=canter-web\n')
        self.assertFalse((self.systemd / 'canter-web.pending').exists())
        self.assertEqual(self.root.stat().st_mode & 0o751, 0o751)
        calls = [item.args for item in run.call_args_list]
        self.assertLess(calls.index(('systemctl', 'daemon-reload')),
                        calls.index(('systemctl', 'restart', 'canter-controlplane', 'canter-web')))

    @patch.object(release.time, 'sleep')
    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    @patch.object(release, 'healthy', return_value=False)
    def test_failed_health_restores_previous_release(self, health, run, subprocess, sleep):
        self.assertEqual((self.systemd / 'canter-web.service').read_text(), 'User=canter\nGroup=canter\n')
        self.assertEqual(self.root.stat().st_mode & 0o777, 0o750)
        with self.assertRaisesRegex(RuntimeError, 'readiness'):
            release.activate(self.next, self.sha)
        self.assertEqual((self.root / 'bin/canter-controlplane').read_text(), 'old-binary')
        self.assertEqual((self.root / 'web/page').read_text(), 'old-web')
        self.assertEqual((self.state / 'failed').read_text(), self.sha)
        self.assertEqual((self.systemd / 'canter-web.service').read_text(), 'User=canter-web\nGroup=canter-web\n')
        self.assertEqual(self.root.stat().st_mode & 0o751, 0o751)
        self.assertGreaterEqual(sum(c.args[:3] == ('chown', '-R', 'canter-web:canter-web') for c in run.call_args_list), 3)
        self.assertFalse((self.state / 'current').exists())
        self.assertFalse((self.root / 'bin/canter-static-linux').exists())
        self.assertFalse((self.harness / 'current').is_symlink())
        for name in release.HARNESS_UNITS:
            self.assertFalse((self.systemd / name).exists())

    @patch.object(release.subprocess, 'run', side_effect=RuntimeError('backup failed'))
    @patch.object(release, 'run')
    def test_backup_failure_does_not_stop_production(self, run, subprocess):
        with self.assertRaisesRegex(RuntimeError, 'backup failed'):
            release.activate(self.next, self.sha)
        run.assert_not_called()
        self.assertEqual((self.root / 'web/page').read_text(), 'old-web')

    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    @patch.object(release, 'ensure_web_account', side_effect=RuntimeError('conflicting web account'))
    def test_conflicting_web_account_stops_before_backup_or_activation(self, ensure_account, run, subprocess):
        with self.assertRaisesRegex(RuntimeError, 'conflicting web account'):
            release.activate(self.next, self.sha)
        ensure_account.assert_called_once_with()
        run.assert_not_called()
        subprocess.assert_not_called()
        self.assertEqual((self.root / 'web/page').read_text(), 'old-web')

    @patch.object(release.time, 'sleep')
    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    @patch.object(release, 'healthy', return_value=False)
    def test_failed_upgrade_restores_existing_runtime(self, health, run, subprocess, sleep):
        old = self.harness / 'releases/old'
        old.mkdir(parents=True)
        (old / 'runner.mjs').write_text('old-runner')
        (self.harness / 'current').symlink_to(old)
        (self.root / 'bin/canter-static-linux').write_text('old-static')
        for name in release.HARNESS_UNITS:
            (self.systemd / name).write_text('old-' + name)
        with patch.object(release, 'unit_state', return_value=True):
            with self.assertRaisesRegex(RuntimeError, 'readiness'):
                release.activate(self.next, self.sha)
        self.assertEqual((self.harness / 'current').resolve(), old.resolve())
        self.assertEqual((self.root / 'bin/canter-static-linux').read_text(), 'old-static')
        for name in release.HARNESS_UNITS:
            self.assertEqual((self.systemd / name).read_text(), 'old-' + name)
        run.assert_any_call('systemctl', 'start', 'canter-harness.socket')

    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    def test_missing_runtime_artifact_restores_application(self, run, subprocess):
        (self.next / 'canter-static-linux').unlink()
        with self.assertRaisesRegex(RuntimeError, 'required runtime artifact'):
            release.activate(self.next, self.sha)
        self.assertEqual((self.root / 'web/page').read_text(), 'old-web')
        self.assertEqual((self.root / 'bin/canter-controlplane').read_text(), 'old-binary')
        self.assertEqual((self.systemd / 'canter-web.service').read_text(), 'User=canter\nGroup=canter\n')
        run.assert_any_call('systemctl', 'start', 'canter-controlplane', 'canter-web')


class WebAccountTests(unittest.TestCase):
    def test_creates_missing_group_and_user_with_mocked_account_tools(self):
        web_group = grp.struct_group(('canter-web', 'x', 321, []))
        canter_group = grp.struct_group(('canter', 'x', 320, []))
        web_user = pwd.struct_passwd(('canter-web', 'x', 654, 321, '', '/nonexistent', '/usr/sbin/nologin'))
        canter_user = pwd.struct_passwd(('canter', 'x', 653, 320, '', '/var/lib/canter', '/usr/sbin/nologin'))

        def get_group(name):
            if name == 'canter-web':
                if get_group.web_missing:
                    get_group.web_missing = False
                    raise KeyError(name)
                return web_group
            return canter_group
        get_group.web_missing = True

        def get_user(name):
            if name == 'canter-web':
                if get_user.web_missing:
                    get_user.web_missing = False
                    raise KeyError(name)
                return web_user
            return canter_user
        get_user.web_missing = True

        with patch.object(release.grp, 'getgrnam', side_effect=get_group), \
             patch.object(release.pwd, 'getpwnam', side_effect=get_user), \
             patch.object(release.os, 'getgrouplist', return_value=[321]), \
             patch.object(release, 'run') as run:
            release.ensure_web_account()

        self.assertEqual(run.call_args_list, [
            call('groupadd', '--system', 'canter-web'),
            call('useradd', '--system', '--no-create-home', '--home-dir', '/nonexistent',
                 '--shell', '/usr/sbin/nologin', '--gid', 'canter-web', 'canter-web'),
        ])

    def test_rejects_existing_account_with_secret_reading_group(self):
        web_group = grp.struct_group(('canter-web', 'x', 321, []))
        canter_group = grp.struct_group(('canter', 'x', 320, []))
        web_user = pwd.struct_passwd(('canter-web', 'x', 654, 321, '', '/nonexistent', '/usr/sbin/nologin'))
        canter_user = pwd.struct_passwd(('canter', 'x', 653, 320, '', '/var/lib/canter', '/usr/sbin/nologin'))

        def get_group(name):
            return web_group if name == 'canter-web' else canter_group

        def get_user(name):
            return web_user if name == 'canter-web' else canter_user

        with patch.object(release.grp, 'getgrnam', side_effect=get_group), \
             patch.object(release.pwd, 'getpwnam', side_effect=get_user), \
             patch.object(release.os, 'getgrouplist', return_value=[321, 320]):
            with self.assertRaisesRegex(RuntimeError, 'dedicated nologin account'):
                release.ensure_web_account()

if __name__ == '__main__':
    unittest.main()

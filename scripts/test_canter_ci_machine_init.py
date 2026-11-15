import base64
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import stat
import tarfile
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    'canter_ci_machine_init',
    Path(__file__).with_name('canter-ci-machine-init.py'))
machine = importlib.util.module_from_spec(spec)
spec.loader.exec_module(machine)

runner_spec = importlib.util.spec_from_file_location(
    'canter_ci_runner_tests',
    Path(__file__).with_name('test_canter_ci_runner.py'))
runner_tests = importlib.util.module_from_spec(runner_spec)
runner_spec.loader.exec_module(runner_tests)


def runner_archive():
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz') as archive:
        for name in ('bin/run.sh', 'bin/installdependencies.sh', 'config.sh'):
            content = b'#!/bin/sh\nexit 0\n'
            member = tarfile.TarInfo(name)
            member.size = len(content)
            member.mode = 0o755
            archive.addfile(member, io.BytesIO(content))
    return buffer.getvalue()


def prepared_payload(root, archive):
    sha = hashlib.sha256(archive).hexdigest()
    manifest = runner_tests.prepared_manifest(sha)
    tc = root / 'toolchains'
    (tc / 'bin').mkdir(parents=True)
    (tc / 'runner').mkdir()
    (tc / 'runner' / 'README').write_text('runner\n')
    (tc / 'manifest.json').write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + '\n')
    for tool, version, names in (
            ('go', '1.2.3', ('go',)),
            ('node', '3.4.5', ('node', 'npm', 'npx'))):
        version_root = tc / 'toolcache' / tool / version
        bindir = version_root / 'x64' / 'bin'
        bindir.mkdir(parents=True)
        (version_root / 'x64.complete').write_text('')
        for name in names:
            target = bindir / name
            target.write_text('#!/bin/sh\nexit 0\n')
            target.chmod(0o755)
    pnpm_bin = tc / 'pnpm' / '6.7.8' / 'bin'
    pnpm_bin.mkdir(parents=True)
    cjs = pnpm_bin / 'pnpm.cjs'
    cjs.write_text('// pnpm\n')
    cjs.chmod(0o755)
    for name, target in {
            'go': '../toolcache/go/1.2.3/x64/bin/go',
            'node': '../toolcache/node/3.4.5/x64/bin/node',
            'npm': '../toolcache/node/3.4.5/x64/bin/npm',
            'npx': '../toolcache/node/3.4.5/x64/bin/npx',
            'pnpm': '../pnpm/6.7.8/bin/pnpm.cjs'}.items():
        os.symlink(target, tc / 'bin' / name)
    (root / 'runner.tar.gz').write_bytes(archive)
    marker = {
        'schemaVersion': 1, 'platform': 'linux-x64', 'baseOS': 'ubuntu-24.04',
        'runnerUrl': runner_tests.PREPARED_RUNNER_URL,
        'runnerSha256': sha,
        'toolchainManifestSha256':
            runner_tests.runner._payload_manifest_digest(manifest),
        'dependenciesInstalled': True,
    }
    (root / 'image.json').write_text(json.dumps(marker) + '\n')
    for dirpath, _dirnames, filenames in os.walk(root):
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
        os.chmod(root / 'toolchains' / rel, 0o555)
    return sha


def bundle(entries=None):
    if entries is None:
        entries = [(name, b'# trusted fixture\n', tarfile.REGTYPE)
                   for name in sorted(machine.BUNDLE_NAMES)]
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz') as archive:
        for name, content, kind in entries:
            member = tarfile.TarInfo(name)
            member.type = kind
            member.size = len(content)
            if kind == tarfile.SYMTYPE:
                member.linkname = '../../escape'
                member.size = 0
            archive.addfile(member, io.BytesIO(content))
    return buffer.getvalue()


def configuration(blob=None):
    blob = bundle() if blob is None else blob
    now = int(time.time() * 1000)
    return {
        'version': 1,
        'bundleBase64': base64.b64encode(blob).decode('ascii'),
        'bundleSha256': hashlib.sha256(blob).hexdigest(),
        'runnerUrl': ('https://github.com/actions/runner/releases/download/v2.330.0/'
                      'actions-runner-linux-x64-2.330.0.tar.gz'),
        'runnerSha256': 'a' * 64,
        'guestArguments': ['--endpoint', 'https://ci.example.test/v1/ci/runner-bootstrap'],
        'bootstrapToken': 'v1.fixture-bootstrap.scoped-token',
        'setupDeadlineUnixMs': now + 600000,
        'hardDeadlineUnixMs': now + 1000000,
    }


class RootOwnedPath(type(Path())):
    def lstat(self):
        return SimpleNamespace(st_mode=stat.S_IFREG | 0o600, st_uid=0)


class MachineInitCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()

    def test_configuration_excludes_argument_and_url_injection(self):
        data = configuration()
        with mock.patch.object(machine, 'DATA_JSON', json.dumps(data)):
            self.assertEqual(machine.configuration(), data)
        mutations = (
            lambda d: d.update(version=True),
            lambda d: d.update(runnerUrl='https://other.example.test/runner.tar.gz'),
            lambda d: d.update(runnerUrl=d['runnerUrl'] + '?token=x'),
            lambda d: d.update(runnerSha256=''),
            lambda d: d.update(guestArguments=['--token-fd', '7']),
            lambda d: d.update(bootstrapToken='token with whitespace'),
            lambda d: d.update(bootstrapToken='token-\u00e9'),
            lambda d: d.update(hardDeadlineUnixMs=d['setupDeadlineUnixMs']),
            lambda d: d.update(extra='not allowed'),
        )
        for mutate in mutations:
            bad = dict(data)
            mutate(bad)
            with mock.patch.object(machine, 'DATA_JSON', json.dumps(bad)):
                with self.assertRaises(ValueError):
                    machine.configuration()

    def test_only_pinned_regular_bundle_files_are_installed(self):
        for index, mask in enumerate((0o022, 0o077)):
            tools = self.root / ('tools-' + str(index))
            previous = os.umask(mask)
            try:
                with mock.patch.object(machine, 'TOOLS', tools):
                    machine.install_bundle(configuration())
            finally:
                os.umask(previous)
            self.assertEqual({p.name for p in tools.iterdir()}, machine.BUNDLE_NAMES)
            self.assertEqual(stat.S_IMODE(tools.stat().st_mode), 0o555)
            for path in tools.iterdir():
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o555)

    def test_bundle_hash_and_ownership_boundaries_fail_closed(self):
        with mock.patch.object(machine, 'TOOLS', self.root / 'unverified'):
            bad = configuration()
            bad['bundleSha256'] = '0' * 64
            with self.assertRaises(ValueError):
                machine.install_bundle(bad)
            self.assertFalse(machine.TOOLS.exists())
        for index, entries in enumerate((
            [('../../escape', b'data', tarfile.REGTYPE)],
            [('canter-ci-runner.py', b'', tarfile.SYMTYPE)],
            [('canter-ci-runner.py', b'a', tarfile.REGTYPE)] * 2,
            [('canter-ci-runner.py', b'a', tarfile.REGTYPE)],
        )):
            with mock.patch.object(machine, 'TOOLS', self.root / ('bad-' + str(index))):
                with self.assertRaises(ValueError):
                    machine.install_bundle(configuration(bundle(entries)))
        self.assertFalse((self.root.parent / 'escape').exists())

    def test_cache_command_wrapper_exact_and_protected(self):
        for index, mask in enumerate((0o022, 0o077)):
            bindir = self.root / ('bin-' + str(index))
            bindir.mkdir()
            target = bindir / 'canter-ci-cache'
            previous = os.umask(mask)
            try:
                with mock.patch.object(machine, 'CACHE_COMMAND', target):
                    machine.install_cache_command()
            finally:
                os.umask(previous)
            self.assertEqual(target.read_bytes(), machine.CACHE_COMMAND_BYTES)
            self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o555)
            # No overwrite: a second install must fail closed.
            with mock.patch.object(machine, 'CACHE_COMMAND', target):
                with self.assertRaises(FileExistsError):
                    machine.install_cache_command()
            self.assertEqual(target.read_bytes(), machine.CACHE_COMMAND_BYTES)

    def test_report_command_wrapper_exact_and_protected(self):
        for index, mask in enumerate((0o022, 0o077)):
            bindir = self.root / ('rbin-' + str(index))
            bindir.mkdir()
            target = bindir / 'canter-ci-report'
            previous = os.umask(mask)
            try:
                with mock.patch.object(machine, 'REPORT_COMMAND', target):
                    machine.install_report_command()
            finally:
                os.umask(previous)
            self.assertEqual(target.read_bytes(), machine.REPORT_COMMAND_BYTES)
            self.assertEqual(target.read_bytes(),
                b'#!/bin/sh\n'
                b'exec /usr/bin/python3 -I /opt/canter-ci/canter-ci-evidence.py "$@"\n')
            self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o555)
            with mock.patch.object(machine, 'REPORT_COMMAND', target):
                with self.assertRaises(FileExistsError):
                    machine.install_report_command()
            self.assertEqual(target.read_bytes(), machine.REPORT_COMMAND_BYTES)

    def test_report_command_refuses_symlink(self):
        bindir = self.root / 'rbin-link'
        bindir.mkdir()
        victim = self.root / 'rvictim'
        victim.write_bytes(b'keep')
        link = bindir / 'canter-ci-report'
        link.symlink_to(victim)
        with mock.patch.object(machine, 'REPORT_COMMAND', link):
            with self.assertRaises(OSError):
                machine.install_report_command()
        self.assertEqual(victim.read_bytes(), b'keep')
        self.assertTrue(link.is_symlink())

    def test_cache_command_refuses_symlink(self):
        bindir = self.root / 'bin-link'
        bindir.mkdir()
        victim = self.root / 'victim'
        victim.write_bytes(b'keep')
        link = bindir / 'canter-ci-cache'
        link.symlink_to(victim)
        with mock.patch.object(machine, 'CACHE_COMMAND', link):
            with self.assertRaises(OSError):
                machine.install_cache_command()
        self.assertEqual(victim.read_bytes(), b'keep')
        self.assertTrue(link.is_symlink())

    def test_install_orders_bundle_command_runner(self):
        calls = []
        data = configuration()
        with mock.patch.object(machine, 'install_bundle',
                               lambda d: calls.append('bundle')), \
                mock.patch.object(machine, 'install_cache_command',
                                  lambda: calls.append('command')), \
                mock.patch.object(machine, 'install_report_command',
                                  lambda: calls.append('report')), \
                mock.patch.object(machine, 'install_runner',
                                  lambda d: calls.append('runner')), \
                mock.patch.object(machine.pwd, 'getpwnam',
                                  return_value=SimpleNamespace(
                                      pw_uid=1001, pw_gid=1001)), \
                mock.patch.object(machine, 'HOME', self.root / 'home'), \
                mock.patch.object(machine, 'STATE', self.root / 'state'), \
                mock.patch.object(machine, 'UNIT', self.root / 'unit'), \
                mock.patch.object(machine.subprocess, 'run'), \
                mock.patch.object(machine.os, 'chown'):
            (self.root / 'home').mkdir()
            machine.install(data)
        self.assertEqual(calls[:4], ['bundle', 'command', 'report', 'runner'])

    def test_runner_hash_precedes_extraction_and_root_execution(self):
        data = configuration()
        opener = mock.Mock()
        opener.open.return_value = io.BytesIO(b'not the pinned archive')
        state = self.root / 'state'
        state.mkdir()
        with mock.patch.object(machine, 'RUNNER', self.root / 'runner'), \
                mock.patch.object(machine, 'STATE', state), \
                mock.patch.object(machine.urllib.request, 'build_opener', return_value=opener), \
                mock.patch.object(machine.subprocess, 'Popen') as spawn:
            with self.assertRaises(ValueError):
                machine.install_runner(data)
            spawn.assert_not_called()
            self.assertEqual(list(machine.RUNNER.iterdir()), [])

    def test_root_setup_absolute_alarm_interrupts_a_slow_operation(self):
        data = configuration()
        data['setupDeadlineUnixMs'] = int(time.time() * 1000) + 200
        stderr = io.StringIO()
        start = time.monotonic()
        with mock.patch.object(machine.os, 'geteuid', return_value=0), \
                mock.patch.object(machine, 'configuration', return_value=data), \
                mock.patch.object(machine, 'install', side_effect=lambda _: time.sleep(5)), \
                contextlib.redirect_stderr(stderr):
            self.assertEqual(machine.main(['install']), 1)
        self.assertLess(time.monotonic() - start, 2)
        self.assertEqual(stderr.getvalue(), 'canter-ci-machine: bootstrap failed\n')

    def test_launch_drops_all_privileges_and_transports_only_a_pipe(self):
        source = RootOwnedPath(self.root / 'machine.py')
        source.write_text('private runtime configuration\n')
        source.chmod(0o600)
        data = configuration()
        account = SimpleNamespace(pw_uid=1000, pw_gid=1000)
        calls = []
        captured = []

        def spawn(argv, **kwargs):
            calls.append((argv, kwargs))
            inherited = os.dup(kwargs['pass_fds'][0])

            def wait(timeout=None):
                try:
                    captured.append(os.read(inherited, 4097))
                finally:
                    os.close(inherited)
                return 0

            return SimpleNamespace(wait=wait, poll=lambda: 0)

        with mock.patch.object(machine, 'MACHINE', source), \
                mock.patch.object(machine, '__file__', str(source)), \
                mock.patch.object(machine, 'HOME', self.root), \
                mock.patch.object(machine.pwd, 'getpwnam', return_value=account), \
                mock.patch.object(machine.subprocess, 'Popen', side_effect=spawn):
            self.assertEqual(machine.launch(data), 0)
        self.assertFalse(source.exists())
        self.assertEqual(captured, [data['bootstrapToken'].encode('ascii')])
        argv, kwargs = calls[0]
        self.assertEqual((kwargs['user'], kwargs['group'], kwargs['extra_groups']),
                         (1000, 1000, []))
        self.assertTrue(kwargs['close_fds'])
        self.assertEqual(len(kwargs['pass_fds']), 1)
        self.assertNotIn(data['bootstrapToken'], repr(argv))
        self.assertNotIn(data['bootstrapToken'], repr(kwargs['env']))
        with self.assertRaises(OSError):
            os.fstat(kwargs['pass_fds'][0])

    def test_prepared_toolchain_absent_or_validated_through_bundle(self):
        data = configuration()
        with mock.patch.object(machine, 'PREPARED', self.root / 'missing'):
            self.assertIsNone(machine._prepared_toolchain(data))
        tools = self.root / 'tools'
        tools.mkdir()
        (tools / 'canter-ci-runner.py').write_text(
            'def validate_prepared_image(path, url, sha):\n'
            '    return (path, url, sha)\n')
        prepared = self.root / 'prepared'
        prepared.mkdir()
        with mock.patch.object(machine, 'PREPARED', prepared), \
                mock.patch.object(machine, 'TOOLS', tools):
            result = machine._prepared_toolchain(data)
        self.assertEqual(result, (str(prepared), data['runnerUrl'],
                                  data['runnerSha256']))

    def _payload_case(self, corrupt=False):
        archive = runner_archive()
        sha = prepared_payload(self.root / 'prepared-src', archive)
        prepared = self.root / 'prepared'
        shutil.copytree(self.root / 'prepared-src', prepared, symlinks=True)
        for dirpath, _dirnames, _filenames in os.walk(prepared):
            os.chmod(dirpath, 0o555)
        if corrupt:
            target = prepared / 'runner.tar.gz'
            target.chmod(0o644)
            target.write_bytes(b'corrupted-archive')
            target.chmod(0o444)
        data = configuration()
        data['runnerUrl'] = runner_tests.PREPARED_RUNNER_URL
        data['runnerSha256'] = sha
        tools = self.root / 'tools'
        tools.mkdir()
        (tools / 'canter-ci-runner.py').write_bytes(
            Path(__file__).with_name('canter-ci-runner.py').read_bytes())
        runner_dir = self.root / 'runner'
        state = self.root / 'state'
        state.mkdir()
        return data, prepared, tools, runner_dir, state

    def _owner_proxies(self):
        real_lstat, real_fstat = os.lstat, os.fstat

        def zeroed(info):
            return os.stat_result((
                info.st_mode, info.st_ino, info.st_dev, info.st_nlink,
                0, 0, info.st_size, info.st_atime, info.st_mtime,
                info.st_ctime))

        return (mock.patch.object(
                    machine.os, 'lstat',
                    side_effect=lambda p, *a, **k: zeroed(real_lstat(p, *a, **k))),
                mock.patch.object(
                    machine.os, 'fstat',
                    side_effect=lambda fd, *a, **k: zeroed(real_fstat(fd, *a, **k))))

    def test_prepared_install_extracts_verified_archive_without_network_or_deps(self):
        data, prepared, tools, runner_dir, state = self._payload_case()
        lstat_patch, fstat_patch = self._owner_proxies()
        opener = mock.Mock(side_effect=AssertionError('network'))
        spawn = mock.Mock(side_effect=AssertionError('deps'))
        with lstat_patch, fstat_patch, \
                mock.patch.object(machine, 'PREPARED', prepared), \
                mock.patch.object(machine, 'TOOLS', tools), \
                mock.patch.object(machine, 'RUNNER', runner_dir), \
                mock.patch.object(machine, 'STATE', state), \
                mock.patch.object(machine.urllib.request, 'build_opener',
                                  return_value=opener), \
                mock.patch.object(machine.subprocess, 'Popen', spawn):
            machine.install_runner(data)
        self.assertEqual(stat.S_IMODE(runner_dir.stat().st_mode), 0o700)
        self.assertTrue((runner_dir / 'bin' / 'run.sh').exists())
        self.assertEqual((runner_dir / 'bin' / 'run.sh').read_bytes(),
                         b'#!/bin/sh\nexit 0\n')
        opener.open.assert_not_called()
        spawn.assert_not_called()

    def test_corrupt_prepared_archive_fails_before_runner_dir(self):
        data, prepared, tools, runner_dir, state = self._payload_case(
            corrupt=True)
        lstat_patch, fstat_patch = self._owner_proxies()
        opener = mock.Mock(side_effect=AssertionError('network'))
        spawn = mock.Mock(side_effect=AssertionError('deps'))
        with lstat_patch, fstat_patch, \
                mock.patch.object(machine, 'PREPARED', prepared), \
                mock.patch.object(machine, 'TOOLS', tools), \
                mock.patch.object(machine, 'RUNNER', runner_dir), \
                mock.patch.object(machine, 'STATE', state), \
                mock.patch.object(machine.urllib.request, 'build_opener',
                                  return_value=opener), \
                mock.patch.object(machine.subprocess, 'Popen', spawn):
            with self.assertRaises(RuntimeError):
                machine.install_runner(data)
        self.assertFalse(runner_dir.exists())
        opener.open.assert_not_called()
        spawn.assert_not_called()

    def test_invalid_prepared_payload_fails_before_runner_dir(self):
        data = configuration()
        prepared = self.root / 'prepared'
        prepared.mkdir()
        runner_dir = self.root / 'runner'
        state = self.root / 'state'
        state.mkdir()
        with mock.patch.object(machine, '_prepared_toolchain',
                               side_effect=RuntimeError('invalid payload')), \
                mock.patch.object(machine, 'PREPARED', prepared), \
                mock.patch.object(machine, 'RUNNER', runner_dir), \
                mock.patch.object(machine, 'STATE', state), \
                mock.patch.object(machine.urllib.request, 'build_opener',
                                  side_effect=AssertionError('network')):
            with self.assertRaises(RuntimeError):
                machine.install_runner(data)
        self.assertFalse(runner_dir.exists())

    def test_launch_prepends_validated_toolchain_to_fixed_path(self):
        source = RootOwnedPath(self.root / 'machine.py')
        source.write_text('private runtime configuration\n')
        source.chmod(0o600)
        data = configuration()
        account = SimpleNamespace(pw_uid=1000, pw_gid=1000)
        envs = []

        def spawn(argv, **kwargs):
            envs.append(kwargs['env'])
            inherited = os.dup(kwargs['pass_fds'][0])

            def wait(timeout=None):
                try:
                    os.read(inherited, 4097)
                finally:
                    os.close(inherited)
                return 0

            return SimpleNamespace(wait=wait, poll=lambda: 0)

        with mock.patch.object(machine, 'MACHINE', source), \
                mock.patch.object(machine, '__file__', str(source)), \
                mock.patch.object(machine, 'HOME', self.root), \
                mock.patch.object(machine, '_prepared_toolchain',
                                  return_value='/tc'), \
                mock.patch.object(machine.pwd, 'getpwnam', return_value=account), \
                mock.patch.object(machine.subprocess, 'Popen', side_effect=spawn):
            self.assertEqual(machine.launch(data), 0)
        env = envs[0]
        self.assertEqual(env['PATH'], '/tc/bin:' + machine.FIXED_PATH)
        self.assertNotIn('RUNNER_TOOL_CACHE', env)

    def test_launch_wait_budget_never_exceeds_original_hard_deadline(self):
        source = RootOwnedPath(self.root / 'machine.py')
        source.write_text('private runtime configuration\n')
        source.chmod(0o600)
        data = configuration()
        now = 1000000.0
        data['setupDeadlineUnixMs'] = int((now + 600) * 1000)
        data['hardDeadlineUnixMs'] = int((now + 1000) * 1000)
        clock = [now]
        waits = []
        account = SimpleNamespace(pw_uid=1000, pw_gid=1000)

        def spawn(argv, **kwargs):
            inherited = os.dup(kwargs['pass_fds'][0])

            def wait(timeout=None):
                waits.append(timeout)
                try:
                    os.read(inherited, 4097)
                finally:
                    os.close(inherited)
                return 0

            return SimpleNamespace(wait=wait, poll=lambda: 0)

        def slow_toolchain(_data):
            clock[0] += 60
            return '/tc'

        with mock.patch.object(machine.time, 'time',
                               side_effect=lambda: clock[0]), \
                mock.patch.object(machine.time, 'monotonic',
                                  side_effect=lambda: clock[0]), \
                mock.patch.object(machine, 'MACHINE', source), \
                mock.patch.object(machine, '__file__', str(source)), \
                mock.patch.object(machine, 'HOME', self.root), \
                mock.patch.object(machine, '_prepared_toolchain',
                                  side_effect=slow_toolchain), \
                mock.patch.object(machine.pwd, 'getpwnam',
                                  return_value=account), \
                mock.patch.object(machine.subprocess, 'Popen',
                                  side_effect=spawn):
            self.assertEqual(machine.launch(data), 0)
        self.assertEqual(len(waits), 1)
        self.assertLessEqual(waits[0], 940.0)
        self.assertAlmostEqual(waits[0], 940.0, places=3)

    def test_service_owns_the_entire_process_tree_without_restart(self):
        unit = machine.service_unit(300)
        self.assertIn('KillMode=control-group\n', unit)
        self.assertIn('Restart=no\n', unit)
        self.assertIn('RuntimeMaxSec=300\n', unit)
        self.assertIn('NoNewPrivileges=yes\n', unit)
        self.assertNotIn('Environment=', unit)
        self.assertNotIn('token', unit.lower())


if __name__ == '__main__':
    unittest.main()

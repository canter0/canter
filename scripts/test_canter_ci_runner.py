import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import signal
import stat
import tempfile
import threading
import time
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('canter_ci_runner',
                                              Path(__file__).with_name('canter-ci-runner.py'))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

TOKEN = 'REGISTRATION-TOKEN-5ecret'


class RunnerCase(unittest.TestCase):
    def setUp(self):
        platform_patch = mock.patch.object(runner.platform, 'system', return_value='Linux')
        euid_patch = mock.patch.object(runner.os, 'geteuid', return_value=1000)
        platform_patch.start()
        euid_patch.start()
        self.addCleanup(platform_patch.stop)
        self.addCleanup(euid_patch.stop)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.runner_dir = self.tmp.name
        self.write_script('config.sh', 'exit 0')
        self.write_script('run.sh', 'exit 0')

    def write_script(self, name, body, mode=0o755):
        path = Path(self.runner_dir) / name
        path.write_text('#!/bin/sh\n' + body + '\n')
        path.chmod(mode)
        return path

    def recording_scripts(self, config_rc=0, run_lines=None):
        d = self.runner_dir
        self.write_script('config.sh',
                          "printf '%s\\n' \"$@\" > '" + d + "/config.args'\n"
                          "env | sort > '" + d + "/config.env'\n"
                          'exit ' + str(config_rc))
        lines = run_lines if run_lines is not None else [
            "printf '%s' \"$@\" > '" + d + "/run.args'",
            "env | sort > '" + d + "/run.env'",
            "echo x >> '" + d + "/run.count'",
            'exit 0',
        ]
        self.write_script('run.sh', '\n'.join(lines))

    def argv(self, *extra, repository='octo-org/widgets', name='ci-runner-1'):
        return ['--repository', repository, '--runner-dir', self.runner_dir,
                '--name', name, *extra]

    def token_stream(self, data=None):
        if data is None:
            data = TOKEN.encode() + b'\n'
        return io.BytesIO(data)

    def run_main(self, argv=None, stdin=None):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = runner.main(argv if argv is not None else self.argv(),
                             stdin=stdin if stdin is not None else self.token_stream())
        return rc, out.getvalue(), err.getvalue()

    def wait_dead(self, pid, timeout=5.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                return True
            time.sleep(0.05)
        return False

    def signal_when_ready(self, path, signum):
        def watch():
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if os.path.exists(path):
                    os.kill(os.getpid(), signum)
                    return
                time.sleep(0.02)
        thread = threading.Thread(target=watch, daemon=True)
        thread.start()
        return thread

    def test_successful_config_and_single_run(self):
        self.recording_scripts()
        rc, out, _ = self.run_main()
        self.assertEqual(rc, 0)
        config_args = Path(self.runner_dir, 'config.args').read_text().splitlines()
        self.assertEqual(config_args, [
            '--unattended', '--ephemeral', '--disableupdate',
            '--url', 'https://github.com/octo-org/widgets',
            '--name', 'ci-runner-1', '--labels', 'canter', '--work', '_work'])
        self.assertNotIn(TOKEN, Path(self.runner_dir, 'config.args').read_text())
        self.assertEqual(Path(self.runner_dir, 'run.args').read_text(), '')
        self.assertEqual(len(Path(self.runner_dir, 'run.count').read_text().splitlines()), 1)
        for name in ('config.env', 'run.env'):
            content = Path(self.runner_dir, name).read_text()
            self.assertRegex(content, r'(?m)^PATH=')
            self.assertRegex(content, r'(?m)^HOME=')
        config_env = Path(self.runner_dir, 'config.env').read_text()
        run_env = Path(self.runner_dir, 'run.env').read_text()
        self.assertIn('ACTIONS_RUNNER_INPUT_TOKEN=' + TOKEN, config_env)
        self.assertNotIn('ACTIONS_RUNNER_INPUT', run_env)
        self.assertNotIn(TOKEN, run_env)
        self.assertIn('destroy disposable VM', out)

    def test_child_env_excludes_ambient_secrets(self):
        self.recording_scripts()
        leaked = {
            'AWS_SECRET_ACCESS_KEY': 'ambient-aws-secret',
            'GITHUB_TOKEN': 'ghp_ambient_token',
            'GITHUB_ACTIONS_TOKEN': 'ghs_ambient',
            'CANTER_API_TOKEN': 'ambient-canter-secret',
            'OPENAI_API_KEY': 'ambient-openai-secret',
            'ACTIONS_RUNNER_INPUT_TOKEN': 'stale-parent-token',
        }
        with mock.patch.dict(os.environ, leaked):
            rc, _, _ = self.run_main()
            self.assertEqual(rc, 0)
            self.assertNotIn('ACTIONS_RUNNER_INPUT_TOKEN', os.environ)
            for stale in leaked.values():
                self.assertNotIn(stale, Path(self.runner_dir, 'config.env').read_text())
                self.assertNotIn(stale, Path(self.runner_dir, 'run.env').read_text())
            config_env = Path(self.runner_dir, 'config.env').read_text()
            self.assertIn('ACTIONS_RUNNER_INPUT_TOKEN=' + TOKEN, config_env)
            self.assertNotIn('stale-parent-token', config_env)

    def test_config_failure_skips_run(self):
        self.recording_scripts(config_rc=3)
        rc, out, err = self.run_main()
        self.assertEqual(rc, 3)
        self.assertFalse(Path(self.runner_dir, 'run.count').exists())
        self.assertIn('status 3', err)
        self.assertIn('destroy disposable VM', out)

    def test_normal_exit_kills_stray_descendants(self):
        d = self.runner_dir
        self.write_script('config.sh', 'exit 0')
        self.write_script('run.sh',
                          'sleep 300 &\n'
                          'echo $! > \'' + d + '/stray.pid\'\n'
                          'exit 0\n')
        rc, _, _ = self.run_main()
        self.assertEqual(rc, 0)
        pid = int(Path(d, 'stray.pid').read_text().strip())
        self.assertTrue(self.wait_dead(pid), 'stray descendant survived runner exit')

    def test_run_failure_returns_nonzero(self):
        self.write_script('config.sh', 'exit 0')
        self.write_script('run.sh', 'exit 7')
        rc, _, _ = self.run_main()
        self.assertEqual(rc, 7)

    def test_timeout_kills_process_group(self):
        d = self.runner_dir
        self.write_script('config.sh', 'exit 0')
        self.write_script('run.sh',
                          'echo $$ > \'' + d + '/run.pid\'\n'
                          'sleep 300 &\n'
                          'echo $! > \'' + d + '/sleep.pid\'\n'
                          'wait\n')
        with mock.patch.object(runner, 'MIN_TIMEOUT_SECONDS', 1):
            rc, _, err = self.run_main(self.argv('--timeout-seconds', '2'))
        self.assertEqual(rc, runner.TIMEOUT_EXIT)
        self.assertIn('timed out', err)
        for name in ('run.pid', 'sleep.pid'):
            pid = int(Path(d, name).read_text().strip())
            self.assertTrue(self.wait_dead(pid), name + ' survived the timeout')

    def interruption_case(self, signum, expected_rc, label):
        d = self.runner_dir
        self.write_script('config.sh', 'exit 0')
        self.write_script('run.sh',
                          'trap \'echo ' + label + ' > "' + d + '/signal"; exit 0\' ' + label + '\n'
                          'echo ready > \'' + d + '/ready\'\n'
                          'sleep 300\n')
        thread = self.signal_when_ready(Path(d, 'ready'), signum)
        try:
            rc, out, _ = self.run_main()
        finally:
            thread.join(timeout=5)
        self.assertEqual(rc, expected_rc)
        self.assertEqual(Path(d, 'signal').read_text().strip(), label)
        self.assertIn('destroy disposable VM', out)

    def test_sigterm_is_forwarded_to_runner_group(self):
        self.interruption_case(signal.SIGTERM, 128 + signal.SIGTERM, 'TERM')

    def test_sigint_is_forwarded_to_runner_group(self):
        self.interruption_case(signal.SIGINT, 128 + signal.SIGINT, 'INT')

    def stubborn_script(self, d):
        return ('( trap \'\' TERM INT; exec sleep 300 ) &\n'
                'echo $! > \'' + d + '/grandchild.pid\'\n'
                'trap \'exit 0\' TERM INT\n'
                'wait\n')

    def test_timeout_kills_term_ignoring_grandchild(self):
        d = self.runner_dir
        self.write_script('config.sh', 'exit 0')
        self.write_script('run.sh', self.stubborn_script(d))
        with mock.patch.object(runner, 'MIN_TIMEOUT_SECONDS', 1):
            rc, _, _ = self.run_main(self.argv('--timeout-seconds', '2'))
        self.assertEqual(rc, runner.TIMEOUT_EXIT)
        pid = int(Path(d, 'grandchild.pid').read_text().strip())
        self.assertTrue(self.wait_dead(pid), 'term-ignoring grandchild survived timeout')

    def test_interrupt_kills_term_ignoring_grandchild(self):
        d = self.runner_dir
        self.write_script('config.sh', 'exit 0')
        self.write_script('run.sh', self.stubborn_script(d))
        thread = self.signal_when_ready(Path(d, 'grandchild.pid'), signal.SIGINT)
        try:
            rc, _, _ = self.run_main()
        finally:
            thread.join(timeout=5)
        self.assertEqual(rc, 128 + signal.SIGINT)
        pid = int(Path(d, 'grandchild.pid').read_text().strip())
        self.assertTrue(self.wait_dead(pid), 'term-ignoring grandchild survived interrupt')

    def test_run_phase_does_not_spawn_after_deadline(self):
        state = runner.InterruptState()
        with mock.patch.object(runner.subprocess, 'Popen') as popen, \
                contextlib.redirect_stderr(io.StringIO()):
            rc = runner.run_phase(['run.sh'], self.runner_dir, {},
                                  time.monotonic() - 1, state)
        self.assertEqual(rc, runner.TIMEOUT_EXIT)
        popen.assert_not_called()

    def test_run_phase_does_not_spawn_when_interrupted(self):
        state = runner.InterruptState()
        state.signum = signal.SIGTERM
        with mock.patch.object(runner.subprocess, 'Popen') as popen, \
                contextlib.redirect_stderr(io.StringIO()):
            rc = runner.run_phase(['run.sh'], self.runner_dir, {},
                                  time.monotonic() + runner.DEFAULT_TIMEOUT_SECONDS, state)
        self.assertEqual(rc, 128 + signal.SIGTERM)
        popen.assert_not_called()

    def test_execute_skips_config_when_already_interrupted(self):
        state = runner.InterruptState()
        state.signum = signal.SIGINT
        args = runner.parse_args(self.argv())
        with mock.patch.object(runner.subprocess, 'Popen') as popen, \
                contextlib.redirect_stderr(io.StringIO()):
            rc = runner.execute(args, self.runner_dir, TOKEN,
                                time.monotonic() + runner.DEFAULT_TIMEOUT_SECONDS, state)
        self.assertEqual(rc, 128 + signal.SIGINT)
        popen.assert_not_called()

    def test_rejects_invalid_repository(self):
        for repository in ('noslash', 'a/b/c', 'bad repo/x', '../x', 'owner/',
                           'owner/name/extra', 'owner/.', 'owner/..', ''):
            with self.subTest(repository=repository):
                with contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit):
                        runner.main(self.argv(repository=repository), stdin=self.token_stream())

    def test_rejects_invalid_runner_name(self):
        for name in ('bad name', 'x' * 65, 'na/me', 'na:me', ''):
            with self.subTest(name=name):
                with contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit):
                        runner.main(self.argv(name=name), stdin=self.token_stream())

    def test_rejects_invalid_timeout(self):
        for value in ('100', '299', '7201', 'abc', '10.5', '3_00', '-5'):
            with self.subTest(value=value):
                with contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit):
                        runner.main(self.argv('--timeout-seconds', value), stdin=self.token_stream())
        rc, _, _ = self.run_main(self.argv('--timeout-seconds', '7200'))
        self.assertEqual(rc, 0)

    def test_rejects_non_linux(self):
        with mock.patch.object(runner.platform, 'system', return_value='Darwin'):
            with self.assertRaisesRegex(RuntimeError, 'Linux'):
                runner.main(self.argv(), stdin=self.token_stream())

    def test_rejects_root(self):
        with mock.patch.object(runner.os, 'geteuid', return_value=0):
            with self.assertRaisesRegex(RuntimeError, 'root'):
                runner.main(self.argv(), stdin=self.token_stream())

    def test_rejects_runasroot_env(self):
        with mock.patch.dict(os.environ, {'RUNNER_ALLOW_RUNASROOT': '1'}):
            with self.assertRaisesRegex(RuntimeError, 'RUNNER_ALLOW_RUNASROOT'):
                runner.main(self.argv(), stdin=self.token_stream())

    def test_rejects_missing_runner_dir(self):
        argv = ['--repository', 'octo-org/widgets',
                '--runner-dir', '/nonexistent-canter-runner-dir-xyz', '--name', 'ci-runner-1']
        with self.assertRaisesRegex(RuntimeError, 'does not exist'):
            runner.main(argv, stdin=self.token_stream())

    def test_rejects_preconfigured_runner_dir(self):
        for name in runner.RESERVED_NAMES:
            with self.subTest(name=name):
                marker = Path(self.runner_dir, name)
                marker.write_text('x')
                with mock.patch.object(runner.subprocess, 'Popen') as popen:
                    with self.assertRaises(RuntimeError):
                        runner.main(self.argv(), stdin=self.token_stream())
                popen.assert_not_called()
                marker.unlink()

    def test_rejects_missing_or_nonexecutable_scripts(self):
        os.unlink(Path(self.runner_dir, 'run.sh'))
        with self.assertRaisesRegex(RuntimeError, 'run.sh'):
            runner.main(self.argv(), stdin=self.token_stream())
        self.write_script('run.sh', 'exit 0')
        self.write_script('config.sh', 'exit 0', mode=0o644)
        with self.assertRaisesRegex(RuntimeError, 'config.sh'):
            runner.main(self.argv(), stdin=self.token_stream())

    def test_rejects_script_symlink_escaping_runner_dir(self):
        with tempfile.TemporaryDirectory() as outside:
            external = Path(outside) / 'config.sh'
            external.write_text('#!/bin/sh\nexit 0\n')
            external.chmod(0o755)
            os.unlink(Path(self.runner_dir, 'config.sh'))
            os.symlink(str(external), Path(self.runner_dir, 'config.sh'))
            with self.assertRaisesRegex(RuntimeError, 'outside'):
                runner.main(self.argv(), stdin=self.token_stream())

    def test_allows_script_symlink_inside_runner_dir(self):
        d = Path(self.runner_dir)
        impl = d / 'config-impl.sh'
        impl.write_text('#!/bin/sh\nexit 0\n')
        impl.chmod(0o755)
        os.unlink(d / 'config.sh')
        os.symlink('config-impl.sh', d / 'config.sh')
        rc, _, _ = self.run_main()
        self.assertEqual(rc, 0)

    def test_token_must_come_from_pipe(self):
        class TtyStream(io.BytesIO):
            def isatty(self):
                return True
        with self.assertRaisesRegex(RuntimeError, 'stdin'):
            runner.read_token(TtyStream(b'token'))

    def test_token_validation(self):
        for data, pattern in ((b'', 'empty'), (b'\n\n', 'empty'),
                              (b'aaa\nbbb\n', 'single line'),
                              (b'aaa\r\nbbb', 'single line'),
                              (b'\xff\xfe', 'UTF-8'),
                              (b'A' * 4097, '4096')):
            with self.subTest(data=data[:16]), self.assertRaisesRegex(RuntimeError, pattern):
                runner.read_token(io.BytesIO(data))
        self.assertEqual(runner.read_token(io.BytesIO(b'  tok-123  \n')), 'tok-123')
        self.assertEqual(runner.read_token(io.BytesIO(b'A' * 4096 + b'\n')), 'A' * 4096)

    def test_multiline_token_rejected_before_launch(self):
        with mock.patch.object(runner.subprocess, 'Popen') as popen:
            with self.assertRaisesRegex(RuntimeError, 'single line'):
                runner.main(self.argv(), stdin=self.token_stream(b'bad\n' + TOKEN.encode() + b'\n'))
        popen.assert_not_called()

    def make_toolchain(self, root):
        root = Path(root)
        manifest = {
            'schemaVersion': 1,
            'platform': 'linux-x64',
            'go': {'version': '1.2.3'},
            'node': {'version': '4.5.6'},
            'pnpm': {'version': '7.8.9'},
            'runner': {'version': '1.0.0'},
        }
        (root / 'manifest.json').write_text(json.dumps(manifest))
        for tool, version, binary in (('go', '1.2.3', 'go'),
                                      ('node', '4.5.6', 'node')):
            base = root / 'toolcache' / tool / version
            (base / 'x64' / 'bin').mkdir(parents=True)
            (base / 'x64.complete').write_text('')
            exe = base / 'x64' / 'bin' / binary
            exe.write_text('#!/bin/sh\necho ok\n')
            exe.chmod(0o755)
        pnpm_bin = root / 'pnpm' / '7.8.9' / 'bin'
        pnpm_bin.mkdir(parents=True)
        pnpm_cjs = pnpm_bin / 'pnpm.cjs'
        pnpm_cjs.write_text('#!/bin/sh\necho ok\n')
        pnpm_cjs.chmod(0o755)
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
            link = root / 'toolcache' / 'node' / '4.5.6' / 'x64' / 'bin' / name
            os.symlink('../lib/node_modules/npm/bin/' + name + '-cli.js', link)
            cli = root / 'toolcache' / 'node' / '4.5.6' / 'x64' / 'lib' / 'node_modules' / 'npm' / 'bin' / (name + '-cli.js')
            cli.parent.mkdir(parents=True, exist_ok=True)
            cli.write_text('#!/bin/sh\necho ok\n')
            cli.chmod(0o755)
        return str(root)

    def env_map(self, name):
        content = Path(self.runner_dir, name).read_text()
        return dict(line.split('=', 1) for line in content.splitlines() if '=' in line)

    def test_toolchain_dir_sets_path_and_tool_cache(self):
        self.recording_scripts()
        with tempfile.TemporaryDirectory() as toolchain:
            root = self.make_toolchain(toolchain)
            original_path = os.environ.get('PATH', '/usr/bin')
            with mock.patch.dict(os.environ, {'PATH': original_path}):
                rc, _, _ = self.run_main(self.argv('--toolchain-dir', root))
            self.assertEqual(rc, 0)
            for name in ('config.env', 'run.env'):
                env = self.env_map(name)
                self.assertEqual(env.get('RUNNER_TOOL_CACHE'),
                                 os.path.join(os.path.realpath(root), 'toolcache'))
                self.assertTrue(env.get('PATH', '').startswith(
                    os.path.join(os.path.realpath(root), 'bin') + os.pathsep))
                self.assertTrue(env['PATH'].endswith(original_path))
            self.assertIn('ACTIONS_RUNNER_INPUT_TOKEN=' + TOKEN,
                          Path(self.runner_dir, 'config.env').read_text())

    def test_toolchain_dir_scrubs_ambient_tool_cache(self):
        self.recording_scripts()
        with tempfile.TemporaryDirectory() as toolchain:
            root = self.make_toolchain(toolchain)
            with mock.patch.dict(os.environ, {'RUNNER_TOOL_CACHE': '/evil/cache'}):
                rc, _, _ = self.run_main(self.argv('--toolchain-dir', root))
                self.assertEqual(rc, 0)
                for name in ('config.env', 'run.env'):
                    env = self.env_map(name)
                    self.assertEqual(env['RUNNER_TOOL_CACHE'],
                                     os.path.join(os.path.realpath(root), 'toolcache'))
                    self.assertNotIn('/evil/cache', env['RUNNER_TOOL_CACHE'])
                rc2, _, _ = self.run_main()
                self.assertEqual(rc2, 0)
                for name in ('config.env', 'run.env'):
                    self.assertNotIn('RUNNER_TOOL_CACHE', self.env_map(name))

    def test_toolchain_dir_rejects_bad_trees_before_launch(self):
        def write_manifest(root, **patch):
            manifest = {
                'schemaVersion': 1,
                'platform': 'linux-x64',
                'go': {'version': '1.2.3'},
                'node': {'version': '4.5.6'},
                'pnpm': {'version': '7.8.9'},
                'runner': {'version': '1.0.0'},
            }
            for tool, changes in patch.items():
                if tool == '__top__':
                    manifest.update(changes)
                elif isinstance(changes, dict):
                    manifest[tool].update(changes)
                else:
                    manifest[tool] = changes
            (root / 'manifest.json').write_text(json.dumps(manifest))

        cases = [
            ('missing manifest', lambda root: os.unlink(root / 'manifest.json')),
            ('bad schema', lambda root: write_manifest(root, __top__={'schemaVersion': 2})),
            ('bad platform', lambda root: write_manifest(root, __top__={'platform': 'linux-arm64'})),
            ('traversal go version', lambda root: write_manifest(root, go={'version': '../1.2.3'})),
            ('absolute node version', lambda root: write_manifest(root, node={'version': '/4.5.6'})),
            ('nonnumeric pnpm version', lambda root: write_manifest(root, pnpm={'version': '7.8'})),
            ('bad runner version', lambda root: write_manifest(root, runner={'version': '1.0.0-rc1'})),
            ('string go spec', lambda root: write_manifest(root, go='not-a-dict')),
            ('list node spec', lambda root: write_manifest(root, node=['4.5.6'])),
            ('missing marker', lambda root: os.unlink(
                root / 'toolcache' / 'go' / '1.2.3' / 'x64.complete')),
            ('missing go binary', lambda root: os.unlink(
                root / 'toolcache' / 'go' / '1.2.3' / 'x64' / 'bin' / 'go')),
            ('missing bin link', lambda root: os.unlink(root / 'bin' / 'go')),
            ('escaping bin link', lambda root: (
                os.unlink(root / 'bin' / 'go'),
                os.symlink('/etc/passwd', root / 'bin' / 'go'))),
            ('nonexec pnpm link', lambda root: os.chmod(
                root / 'pnpm' / '7.8.9' / 'bin' / 'pnpm.cjs', 0o644)),
            ('missing npm link', lambda root: os.unlink(root / 'bin' / 'npm')),
        ]
        for label, mutate in cases:
            with self.subTest(label=label):
                with tempfile.TemporaryDirectory() as toolchain:
                    root = Path(self.make_toolchain(toolchain))
                    mutate(root)
                    with mock.patch.object(runner.subprocess, 'Popen') as popen:
                        with self.assertRaises(RuntimeError):
                            runner.main(self.argv('--toolchain-dir', str(root)),
                                        stdin=self.token_stream())
                    popen.assert_not_called()

    def test_toolchain_dir_rejects_missing_dir(self):
        with mock.patch.object(runner.subprocess, 'Popen') as popen:
            with self.assertRaisesRegex(RuntimeError, 'does not exist'):
                runner.main(self.argv('--toolchain-dir', '/nonexistent-tc-xyz'),
                            stdin=self.token_stream())
        popen.assert_not_called()

    def test_errors_do_not_disclose_token(self):
        secret = 'XYZZY-5ECRET-777'
        self.recording_scripts(config_rc=1)
        rc, out, err = self.run_main(stdin=self.token_stream((secret + '\n').encode()))
        self.assertNotEqual(rc, 0)
        self.assertNotIn(secret, out)
        self.assertNotIn(secret, err)
        with self.assertRaises(RuntimeError) as ctx:
            runner.main(self.argv(), stdin=self.token_stream(b'bad\n' + secret.encode() + b'\n'))
        self.assertNotIn(secret, str(ctx.exception))
        with mock.patch.object(runner.subprocess, 'Popen',
                               side_effect=OSError('spawn failed')):
            with contextlib.redirect_stdout(io.StringIO()), \
                    contextlib.redirect_stderr(io.StringIO()), \
                    self.assertRaises(OSError):
                runner.main(self.argv(), stdin=self.token_stream((secret + '\n').encode()))


PREPARED_RUNNER_URL = ('https://github.com/actions/runner/releases/download/'
                       'v2.3.4/actions-runner-linux-x64-2.3.4.tar.gz')
PREPARED_ARCHIVE = b'fake-runner-archive-bytes'


def prepared_manifest(archive_sha):
    return {
        'schemaVersion': 1,
        'platform': 'linux-x64',
        'go': {'version': '1.2.3',
               'url': 'https://dl.google.com/go/go1.2.3.linux-amd64.tar.gz',
               'algorithm': 'sha256', 'digest': 'a' * 64, 'stripPrefix': 'go'},
        'node': {'version': '3.4.5',
                 'url': 'https://nodejs.org/dist/v3.4.5/node-v3.4.5-linux-x64.tar.gz',
                 'algorithm': 'sha256', 'digest': 'b' * 64,
                 'stripPrefix': 'node-v3.4.5-linux-x64'},
        'pnpm': {'version': '6.7.8',
                 'url': 'https://registry.npmjs.org/pnpm/-/pnpm-6.7.8.tgz',
                 'algorithm': 'sha512', 'digest': 'c' * 128, 'stripPrefix': 'package'},
        'runner': {'version': '2.3.4', 'url': PREPARED_RUNNER_URL,
                   'algorithm': 'sha256', 'digest': archive_sha, 'stripPrefix': ''},
    }


class PreparedImageCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.addCleanup(self._restore_perms)
        self.base = Path(self.tmp.name).resolve()
        self.archive_sha = hashlib.sha256(PREPARED_ARCHIVE).hexdigest()
        self.manifest = prepared_manifest(self.archive_sha)
        self.payload = self.base / 'payload'
        self._build_payload(self.payload)
        uid_patch = mock.patch.object(runner, 'PAYLOAD_UID', os.getuid())
        gid_patch = mock.patch.object(runner, 'PAYLOAD_GID', os.getgid())
        uid_patch.start()
        gid_patch.start()
        self.addCleanup(uid_patch.stop)
        self.addCleanup(gid_patch.stop)

    def _restore_perms(self):
        dirs = [self.base]
        for dirpath, dirnames, _filenames in os.walk(self.base):
            dirs.extend(os.path.join(dirpath, name) for name in dirnames)
        for path in dirs:
            try:
                os.chmod(path, 0o700)
            except OSError:
                pass

    def _build_payload(self, root):
        tc = root / 'toolchains'
        (tc / 'bin').mkdir(parents=True)
        (tc / 'runner').mkdir()
        (tc / 'runner' / 'README').write_text('runner\n')
        (tc / 'manifest.json').write_text(
            json.dumps(self.manifest, indent=2, sort_keys=True) + '\n')
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
        (root / 'runner.tar.gz').write_bytes(PREPARED_ARCHIVE)
        image = {
            'schemaVersion': 1, 'platform': 'linux-x64', 'baseOS': 'ubuntu-24.04',
            'runnerUrl': PREPARED_RUNNER_URL, 'runnerSha256': self.archive_sha,
            'toolchainManifestSha256': runner._payload_manifest_digest(self.manifest),
            'dependenciesInstalled': True,
        }
        (root / 'image.json').write_text(json.dumps(image) + '\n')
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

    def _rewrite(self, root, rel, content, mode=0o444):
        path = root / rel
        path.chmod(0o644)
        path.write_text(content)
        path.chmod(mode)

    def _bad(self, name, mutate):
        root = self._fresh(name)
        mutate(root)
        with self.assertRaises(RuntimeError):
            runner.validate_prepared_image(
                str(root), PREPARED_RUNNER_URL, self.archive_sha)

    def _fresh(self, name):
        root = self.base / name
        self._build_payload(root)
        return root

    def _image(self, root):
        return json.loads((root / 'image.json').read_text())

    def test_valid_payload_returns_canonical_toolchain_prefix(self):
        self.assertEqual(
            runner.validate_prepared_image(
                str(self.payload), PREPARED_RUNNER_URL, self.archive_sha),
            str(self.payload / 'toolchains'))

    def test_metadata_and_pin_mismatches_fail(self):
        cases = [
            ('url', {'runnerUrl': 'https://evil.example/x.tar.gz'}),
            ('sha', {'runnerSha256': '0' * 64}),
            ('boolschema', {'schemaVersion': True}),
            ('nodeps', {'dependenciesInstalled': False}),
            ('digest', {'toolchainManifestSha256': '0' * 64}),
            ('baseos', {'baseOS': 'debian-12'}),
            ('platform', {'platform': 'linux-arm64'}),
        ]
        for name, change in cases:
            def mutate(root, change=change):
                bad = dict(self._image(root))
                bad.update(change)
                self._rewrite(root, 'image.json', json.dumps(bad) + '\n')
            self._bad('meta-' + name, mutate)

        def extra(root):
            bad = dict(self._image(root))
            bad['unexpected'] = 1
            self._rewrite(root, 'image.json', json.dumps(bad) + '\n')
        self._bad('meta-extra', extra)

    def test_duplicate_and_invalid_json_fail(self):
        def dup(root):
            self._rewrite(root, 'image.json',
                          '{"schemaVersion": 1, "schemaVersion": 1}')
        self._bad('dup', dup)

        def trailing(root):
            path = root / 'image.json'
            path.chmod(0o644)
            with path.open('a') as out:
                out.write('\n{"schemaVersion": 1}')
            path.chmod(0o444)
        self._bad('trailing', trailing)

        def notjson(root):
            self._rewrite(root, 'image.json', 'not json\n')
        self._bad('notjson', notjson)

    def test_archive_and_tree_violations_fail(self):
        def corrupt(root):
            path = root / 'runner.tar.gz'
            path.chmod(0o644)
            path.write_bytes(b'corrupted')
            path.chmod(0o444)
        self._bad('corrupt', corrupt)

        def writable_archive(root):
            (root / 'runner.tar.gz').chmod(0o644)
        self._bad('writable-archive', writable_archive)

        def writable_dir(root):
            (root / 'toolchains' / 'bin').chmod(0o755)
        self._bad('writable-dir', writable_dir)

        def writable_file(root):
            (root / 'toolchains' / 'manifest.json').chmod(0o644)
        self._bad('writable-file', writable_file)

        def extra_child(root):
            target = root / 'toolchains'
            target.chmod(0o755)
            (target / 'extra').write_text('x')
            (target / 'extra').chmod(0o444)
            target.chmod(0o555)
        self._bad('extra-child', extra_child)

        def missing_child(root):
            target = root / 'toolchains' / 'pnpm'
            for dirpath, dirnames, filenames in os.walk(str(target), topdown=False):
                for name in filenames:
                    os.chmod(os.path.join(dirpath, name), 0o644)
                os.chmod(dirpath, 0o755)
            (root / 'toolchains').chmod(0o755)
            shutil.rmtree(target)
        self._bad('missing-child', missing_child)

        def escape_link(root):
            bindir = root / 'toolchains' / 'bin'
            bindir.chmod(0o755)
            os.symlink('../../../outside', bindir / 'evil')
            bindir.chmod(0o555)
        self._bad('escape-link', escape_link)

        def absolute_link(root):
            bindir = root / 'toolchains' / 'bin'
            bindir.chmod(0o755)
            os.symlink('/etc/passwd', bindir / 'abs')
            bindir.chmod(0o555)
        self._bad('absolute-link', absolute_link)

        def credential_state(root):
            state = root / 'toolchains' / 'runner' / '.credentials'
            target = state.parent
            target.chmod(0o755)
            state.write_text('secret\n')
            state.chmod(0o444)
            target.chmod(0o555)
        self._bad('credential-state', credential_state)

        def diag_state(root):
            state = root / 'toolchains' / 'runner' / '_diag'
            target = state.parent
            target.chmod(0o755)
            state.mkdir()
            os.chmod(state, 0o555)
            target.chmod(0o555)
        self._bad('diag-state', diag_state)

    def test_prefix_parent_and_manifest_violations_fail(self):
        def bool_manifest(root):
            path = root / 'toolchains' / 'manifest.json'
            path.chmod(0o644)
            manifest = dict(self.manifest, schemaVersion=True)
            path.write_text(json.dumps(manifest) + '\n')
            path.chmod(0o444)
        self._bad('bool-manifest', bool_manifest)

        def runner_pin(root):
            path = root / 'toolchains' / 'manifest.json'
            path.chmod(0o644)
            manifest = json.loads(json.dumps(self.manifest))
            manifest['runner'] = dict(manifest['runner'],
                                      url='https://evil.example/x.tar.gz')
            path.write_text(json.dumps(manifest) + '\n')
            path.chmod(0o444)
        self._bad('runner-pin', runner_pin)

        def partial(root):
            root.chmod(0o755)
            (root / 'image.json').chmod(0o644)
            (root / 'image.json').unlink()
            root.chmod(0o555)
        self._bad('partial', partial)

        def writable_prefix(root):
            root.chmod(0o755)
        self._bad('writable-prefix', writable_prefix)

        def foreign_parent(root):
            os.chmod(root.parent, 0o777)
            try:
                with self.assertRaises(RuntimeError):
                    runner.validate_prepared_image(
                        str(root), PREPARED_RUNNER_URL, self.archive_sha)
            finally:
                os.chmod(root.parent, 0o700)
        foreign_parent(self._fresh('foreign-parent'))

        self._build_payload(self.base / 'linkcase-real')
        os.symlink('linkcase-real', self.base / 'linkcase')
        with self.assertRaises(RuntimeError):
            runner.validate_prepared_image(
                str(self.base / 'linkcase'), PREPARED_RUNNER_URL, self.archive_sha)

        with mock.patch.object(runner, 'PAYLOAD_UID', os.getuid() + 1):
            with self.assertRaises(RuntimeError):
                runner.validate_prepared_image(
                    str(self.payload), PREPARED_RUNNER_URL, self.archive_sha)

        with mock.patch.object(runner, 'MAX_RUNNER_ARCHIVE_BYTES', 4):
            with self.assertRaises(RuntimeError):
                runner.validate_prepared_image(
                    str(self.payload), PREPARED_RUNNER_URL, self.archive_sha)

        with mock.patch.object(runner, 'MAX_IMAGE_JSON_BYTES', 8):
            with self.assertRaises(RuntimeError):
                runner.validate_prepared_image(
                    str(self.payload), PREPARED_RUNNER_URL, self.archive_sha)

    def test_exact_modes_owners_and_tree_edges_fail(self):
        def mode0400(root):
            (root / 'toolchains' / 'manifest.json').chmod(0o400)
        self._bad('mode-0400', mode0400)

        def mode0500dir(root):
            (root / 'toolchains' / 'bin').chmod(0o500)
        self._bad('mode-0500-dir', mode0500dir)

        def mode0500file(root):
            go_bin = (root / 'toolchains' / 'toolcache' / 'go' / '1.2.3'
                      / 'x64' / 'bin' / 'go')
            go_bin.chmod(0o500)
        self._bad('mode-0500-file', mode0500file)

        def suid_file(root):
            cjs = root / 'toolchains' / 'pnpm' / '6.7.8' / 'bin' / 'pnpm.cjs'
            cjs.chmod(0o4555)
        self._bad('suid-file', suid_file)

        def fifo_entry(root):
            target = root / 'toolchains' / 'runner'
            target.chmod(0o755)
            os.mkfifo(target / 'pipe')
            target.chmod(0o555)
        self._bad('fifo-entry', fifo_entry)

        def link_cycle(root):
            bindir = root / 'toolchains' / 'bin'
            bindir.chmod(0o755)
            os.symlink('cycle-b', bindir / 'cycle-a')
            os.symlink('cycle-a', bindir / 'cycle-b')
            bindir.chmod(0o555)
        self._bad('link-cycle', link_cycle)

        def dangling_link(root):
            bindir = root / 'toolchains' / 'bin'
            bindir.chmod(0o755)
            os.symlink('missing-target', bindir / 'dangling')
            bindir.chmod(0o555)
        self._bad('dangling-link', dangling_link)

        def env_in_runner(root):
            target = root / 'toolchains' / 'runner'
            target.chmod(0o755)
            (target / '.env').write_text('SECRET=1\n')
            (target / '.env').chmod(0o444)
            target.chmod(0o555)
        self._bad('env-in-runner', env_in_runner)

        with mock.patch.object(runner, 'PAYLOAD_GID', os.getgid() + 1):
            with self.assertRaises(RuntimeError):
                runner.validate_prepared_image(
                    str(self.payload), PREPARED_RUNNER_URL, self.archive_sha)

        with mock.patch.object(runner, 'MAX_PAYLOAD_ENTRIES', 3):
            with self.assertRaises(RuntimeError):
                runner.validate_prepared_image(
                    str(self.payload), PREPARED_RUNNER_URL, self.archive_sha)

        real_lstat = os.lstat

        def owner_proxy(field, delta):
            def fake_lstat(path, *args, **kwargs):
                info = real_lstat(path, *args, **kwargs)
                if os.fspath(path).endswith('manifest.json'):
                    values = [info.st_mode, info.st_ino, info.st_dev,
                              info.st_nlink, info.st_uid, info.st_gid,
                              info.st_size, info.st_atime, info.st_mtime,
                              info.st_ctime]
                    values[4 if field == 'uid' else 5] += delta
                    return os.stat_result(tuple(values))
                return info
            return fake_lstat

        for field in ('uid', 'gid'):
            root = self._fresh('wrong-' + field)
            with mock.patch.object(runner.os, 'lstat',
                                   side_effect=owner_proxy(field, 1)):
                with self.assertRaises(RuntimeError):
                    runner.validate_prepared_image(
                        str(root), PREPARED_RUNNER_URL, self.archive_sha)

        def fake_walk(*_args, **kwargs):
            kwargs['onerror'](OSError('denied'))
            return iter(())
        with mock.patch.object(runner.os, 'walk', side_effect=fake_walk):
            with self.assertRaises(RuntimeError):
                runner.validate_prepared_image(
                    str(self.payload), PREPARED_RUNNER_URL, self.archive_sha)

    def test_public_tool_files_may_carry_runner_state_names(self):
        root = self._fresh('public-state-names')
        x64 = root / 'toolchains' / 'toolcache' / 'go' / '1.2.3' / 'x64'
        x64.chmod(0o755)
        testdata = x64 / 'src' / 'testdata'
        testdata.mkdir(parents=True)
        env_file = testdata / '.env'
        env_file.write_text('PUBLIC=1\n')
        env_file.chmod(0o444)
        os.chmod(testdata, 0o555)
        os.chmod(testdata.parent, 0o555)
        x64.chmod(0o555)
        self.assertEqual(
            runner.validate_prepared_image(
                str(root), PREPARED_RUNNER_URL, self.archive_sha),
            str(root / 'toolchains'))


if __name__ == '__main__':
    unittest.main()

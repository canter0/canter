import base64
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location(
    'canter_ci_jit_runner', Path(__file__).with_name('canter-ci-jit-runner.py'))
jit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(jit)
legacy = jit.legacy
JIT_PATH = Path(__file__).with_name('canter-ci-jit-runner.py').resolve()

JIT_B64 = base64.b64encode(b'{"runnerCredentials":"opaque-jit-payload"}').decode()
ADMISSION_ID = 'ab' * 32
RUNNER_ID = '4242'
REPO_ID = '987'
RUNNER_NAME = 'ci-jit-1'

HOOK_ENV = ('GITHUB_REPOSITORY=octo-org/widgets GITHUB_REPOSITORY_ID=' + REPO_ID +
            ' RUNNER_NAME=' + RUNNER_NAME + ' GITHUB_RUN_ID=777 GITHUB_RUN_ATTEMPT=2 ' +
            'GITHUB_JOB=build GITHUB_SERVER_URL=https://github.com ' +
            'GITHUB_API_URL=https://api.github.com ')
HOOK_ENV_DICT = {'GITHUB_REPOSITORY': 'octo-org/widgets',
                'GITHUB_REPOSITORY_ID': REPO_ID, 'RUNNER_NAME': RUNNER_NAME,
                'GITHUB_RUN_ID': '777', 'GITHUB_RUN_ATTEMPT': '2',
                'GITHUB_JOB': 'build',
                'GITHUB_SERVER_URL': 'https://github.com',
                'GITHUB_API_URL': 'https://api.github.com'}


class JitCase(unittest.TestCase):
    def setUp(self):
        patcher = mock.patch.object(legacy, 'check_host')
        patcher.start()
        self.addCleanup(patcher.stop)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.runner_dir = os.path.join(self.tmp.name, 'runner')
        os.mkdir(self.runner_dir, 0o755)
        self.admission_dir = os.path.join(self.tmp.name, 'admission')
        self.write_script('config.sh',
                          'touch "' + self.runner_dir + '/config.called"')

    def write_script(self, name, body, mode=0o755):
        path = Path(self.runner_dir) / name
        path.write_text('#!/bin/sh\n' + body + '\n')
        path.chmod(mode)
        return path

    def admit_script(self, tail=''):
        d = self.runner_dir
        return ('printf \'%s\' "$@" > "' + d + '/run.args"\n'
                'env | sort > "' + d + '/child.env"\n'
                'if [ -n "${ACTIONS_RUNNER_HOOK_JOB_STARTED:-}" ]; then\n'
                '    ' + HOOK_ENV +
                '/bin/sh "$ACTIONS_RUNNER_HOOK_JOB_STARTED"\n'
                'fi\n'
                'echo ran > "' + d + '/marker"\n' + tail + '\nexit 0\n')

    def argv(self, *extra):
        return ['run', '--runner-dir', self.runner_dir, '--name', RUNNER_NAME,
                '--github-runner-id', RUNNER_ID, '--github-repository-id', REPO_ID,
                '--admission-dir', self.admission_dir, '--admission-id', ADMISSION_ID,
                *extra]

    def stdin(self, data=None):
        if data is None:
            data = JIT_B64.encode() + b'\n'
        return io.BytesIO(data)

    def run_main(self, argv=None, stdin=None):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = jit.main(argv if argv is not None else self.argv(),
                          stdin=stdin if stdin is not None else self.stdin())
        return rc, out.getvalue(), err.getvalue()

    def allow_writer(self, deadline_ms=None, mutate=None, delay=0.0):
        def work():
            if delay:
                time.sleep(delay)
            deadline = time.monotonic() + 10
            request_path = os.path.join(self.admission_dir, 'request.json')
            while time.monotonic() < deadline:
                if os.path.exists(request_path):
                    break
                time.sleep(0.02)
            else:
                return
            req = json.loads(Path(request_path).read_text())
            rec = dict(req)
            rec['decision'] = 'allow'
            rec['executionDeadlineUnixMs'] = (
                deadline_ms if deadline_ms is not None
                else int(time.time() * 1000) + 60000)
            if mutate:
                mutate(rec)
            tmp = os.path.join(self.admission_dir, 'receipt.tmp')
            with open(tmp, 'wb') as stream:
                stream.write(json.dumps(rec).encode())
            os.chmod(tmp, 0o600)
            os.replace(tmp, os.path.join(self.admission_dir, 'receipt.json'))
        thread = threading.Thread(target=work, daemon=True)
        thread.start()
        return thread

    def raw_writer(self, payload, delay=0.0):
        def work():
            if delay:
                time.sleep(delay)
            deadline = time.monotonic() + 10
            request_path = os.path.join(self.admission_dir, 'request.json')
            while time.monotonic() < deadline:
                if os.path.exists(request_path):
                    break
                time.sleep(0.02)
            tmp = os.path.join(self.admission_dir, 'receipt.tmp')
            with open(tmp, 'wb') as stream:
                stream.write(payload)
            os.chmod(tmp, 0o600)
            os.replace(tmp, os.path.join(self.admission_dir, 'receipt.json'))
        thread = threading.Thread(target=work, daemon=True)
        thread.start()
        return thread

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

    def child_pid_file(self, extra=''):
        return ('echo $$ > "' + self.runner_dir + '/run.pid"\n'
                + 'sleep 300 &\n'
                'echo $! > "' + self.runner_dir + '/sleep.pid"\n' + extra)

    def fresh_dirs(self, tag):
        self.runner_dir = os.path.join(self.tmp.name, 'runner-' + tag)
        os.mkdir(self.runner_dir, 0o755)
        self.admission_dir = os.path.join(self.tmp.name, 'adm-' + tag)
        self.write_script('config.sh',
                          'touch "' + self.runner_dir + '/config.called"')

    def fn_writer(self, fn):
        def work():
            deadline = time.monotonic() + 10
            request_path = os.path.join(self.admission_dir, 'request.json')
            while time.monotonic() < deadline:
                if os.path.exists(request_path):
                    break
                time.sleep(0.02)
            else:
                return
            req = json.loads(Path(request_path).read_text())
            payload = fn(req)
            if isinstance(payload, dict):
                payload = json.dumps(payload).encode()
            elif isinstance(payload, str):
                payload = payload.encode()
            tmp = os.path.join(self.admission_dir, 'receipt.tmp')
            with open(tmp, 'wb') as stream:
                stream.write(payload)
            os.chmod(tmp, 0o600)
            os.replace(tmp, os.path.join(self.admission_dir, 'receipt.json'))
        thread = threading.Thread(target=work, daemon=True)
        thread.start()
        return thread

    def wait_file(self, path, timeout=10.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if os.path.exists(path):
                return True
            time.sleep(0.02)
        return False

    def hook_setup(self, tag='hook'):
        directory = os.path.join(os.path.realpath(self.tmp.name), tag)
        os.mkdir(directory, 0o700)
        config = {'schemaVersion': 1, 'admissionId': ADMISSION_ID,
                  'githubRunnerId': int(RUNNER_ID),
                  'githubRepositoryId': int(REPO_ID),
                  'runnerName': RUNNER_NAME}
        cfg = os.path.join(directory, 'config.json')
        fd = os.open(cfg, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            os.write(fd, json.dumps(config).encode())
        finally:
            os.close(fd)
        return directory, cfg

    def hook_env(self, **overrides):
        env = {'PATH': '/usr/bin:/bin', 'HOME': self.tmp.name}
        env.update(HOOK_ENV_DICT)
        for key, value in overrides.items():
            if value is None:
                env.pop(key, None)
            else:
                env[key] = str(value)
        return env

    def spawn_hook(self, cfg, env):
        return subprocess.Popen(
            [sys.executable, str(JIT_PATH), 'hook', '--config', cfg],
            env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    def test_valid_allow_runs_once_no_config(self):
        self.write_script('run.sh', self.admit_script())
        writer = self.allow_writer()
        rc, _, _ = self.run_main()
        writer.join(timeout=5)
        self.assertEqual(rc, 0)
        marker = Path(self.runner_dir, 'marker')
        self.assertEqual(marker.read_text().strip(), 'ran')
        self.assertFalse(Path(self.runner_dir, 'config.called').exists())
        request = json.loads(Path(self.admission_dir, 'request.json').read_text())
        self.assertEqual(request, {
            'schemaVersion': 1, 'admissionId': ADMISSION_ID,
            'githubRunnerId': int(RUNNER_ID), 'githubRepositoryId': int(REPO_ID),
            'repository': 'octo-org/widgets', 'githubRunId': 777,
            'runAttempt': 2, 'jobKey': 'build', 'runnerName': RUNNER_NAME})
        hook = Path(self.admission_dir, 'job-started.sh').read_text()
        self.assertIn('-I', hook)
        self.assertIn('hook --config', hook)

    def test_child_env_is_allowlisted(self):
        self.write_script('run.sh', self.admit_script())
        hostile = {'AWS_SECRET_ACCESS_KEY': 'ambient-aws',
                   'GITHUB_TOKEN': 'ghp_ambient',
                   'CANTER_API_TOKEN': 'ambient-canter',
                   'PYTHONPATH': '/tmp/evil', 'BASH_ENV': '/tmp/evil.sh',
                   'LD_PRELOAD': '/tmp/evil.so',
                   'ACTIONS_RUNNER_INPUT_TOKEN': 'stale-token',
                   'ACTIONS_RUNNER_INPUT_JITCONFIG': 'stale-jit',
                   'ACTIONS_RUNNER_HOOK_JOB_STARTED': '/tmp/evil-hook',
                   'ACTIONS_RUNNER_HOOK_JOB_COMPLETED': '/tmp/evil-completed',
                   'RUNNER_TOOL_CACHE': '/tmp/stale-cache',
                   'PATH': '/tmp/evil-path'}
        with mock.patch.dict(os.environ, hostile):
            writer = self.allow_writer()
            rc, _, _ = self.run_main()
            writer.join(timeout=5)
        self.assertEqual(rc, 0)
        env = {}
        for line in Path(self.runner_dir, 'child.env').read_text().splitlines():
            key, _, value = line.partition('=')
            env[key] = value
        self.assertEqual(env['PATH'], jit.FIXED_PATH)
        self.assertEqual(env['ACTIONS_RUNNER_INPUT_JITCONFIG'], JIT_B64)
        self.assertEqual(
            env['ACTIONS_RUNNER_HOOK_JOB_STARTED'],
            os.path.join(os.path.realpath(self.admission_dir),
                         'job-started.sh'))
        self.assertNotIn('ACTIONS_RUNNER_INPUT_TOKEN', env)
        self.assertNotIn('ACTIONS_RUNNER_HOOK_JOB_COMPLETED', env)
        for leaked in ('ambient-aws', 'ghp_ambient', 'ambient-canter',
                       'stale-token', 'stale-jit', '/tmp/evil-path',
                       '/tmp/stale-cache', '/tmp/evil',
                       '/tmp/evil-hook', '/tmp/evil-completed'):
            self.assertNotIn(leaked, Path(self.runner_dir, 'child.env').read_text())
        allowed = {'PATH', 'HOME', 'LANG', 'LC_ALL', 'TMPDIR',
                   'ACTIONS_RUNNER_INPUT_JITCONFIG',
                   'ACTIONS_RUNNER_HOOK_JOB_STARTED',
                   'SHLVL', 'PWD', '_'}
        self.assertLessEqual(set(env), allowed | {'RUNNER_TOOL_CACHE'})

    def test_toolchain_prefixes_path_and_tool_cache(self):
        root = os.path.join(self.tmp.name, 'toolchain')
        manifest = {'schemaVersion': 1, 'platform': 'linux-x64',
                    'go': {'version': '1.2.3'}, 'node': {'version': '4.5.6'},
                    'pnpm': {'version': '7.8.9'}, 'runner': {'version': '1.0.0'}}
        os.makedirs(os.path.join(root, 'toolcache', 'go', '1.2.3', 'x64', 'bin'))
        os.makedirs(os.path.join(root, 'toolcache', 'node', '4.5.6', 'x64', 'bin'))
        os.makedirs(os.path.join(root, 'pnpm', '7.8.9', 'bin'))
        os.makedirs(os.path.join(root, 'bin'))
        Path(root, 'manifest.json').write_text(json.dumps(manifest))
        for rel in ('toolcache/go/1.2.3/x64.complete',
                    'toolcache/node/4.5.6/x64.complete'):
            Path(root, rel).write_text('')
        for name, target in (('go', 'toolcache/go/1.2.3/x64/bin/go'),
                             ('node', 'toolcache/node/4.5.6/x64/bin/node'),
                             ('npm', 'toolcache/node/4.5.6/x64/bin/npm'),
                             ('npx', 'toolcache/node/4.5.6/x64/bin/npx'),
                             ('pnpm', 'pnpm/7.8.9/bin/pnpm.cjs')):
            Path(root, 'bin', name).symlink_to(os.path.join(root, target))
            Path(root, target).write_text('#!/bin/sh\n')
            os.chmod(Path(root, target), 0o755)
        Path(root, 'pnpm/7.8.9/bin/pnpm.cjs').write_text('// pnpm\n')
        for name in ('go', 'node', 'npm', 'npx', 'pnpm'):
            os.chmod(Path(root, 'bin', name), 0o755)
        self.write_script('run.sh', self.admit_script())
        writer = self.allow_writer()
        rc, _, _ = self.run_main(self.argv('--toolchain-dir', root))
        writer.join(timeout=5)
        self.assertEqual(rc, 0)
        env = dict(line.split('=', 1) for line in
                   Path(self.runner_dir, 'child.env').read_text().splitlines())
        self.assertTrue(env['PATH'].startswith(
            os.path.join(os.path.realpath(root), 'bin') + ':'))
        self.assertTrue(env['PATH'].endswith(jit.FIXED_PATH))
        self.assertEqual(env['RUNNER_TOOL_CACHE'],
                         os.path.join(os.path.realpath(root), 'toolcache'))

    def test_stdin_rejected(self):
        self.write_script('run.sh', 'exit 0')
        class Tty(io.BytesIO):
            def isatty(self):
                return True
        oversized = base64.b64encode(b'x' * 50000).decode().encode()
        oversized = (oversized[:65537] + b'\n')
        cases = {
            'tty': Tty(JIT_B64.encode()),
            'empty': io.BytesIO(b''),
            'blank': io.BytesIO(b'\n'),
            'multiline': io.BytesIO(JIT_B64.encode() + b'\n' + JIT_B64.encode() + b'\n'),
            'oversized': io.BytesIO(oversized),
            'invalid-base64': io.BytesIO(b'!!!!not-base64!!!!\n'),
        }
        for label, stream in cases.items():
            with self.subTest(case=label):
                with self.assertRaises(RuntimeError) as ctx:
                    jit.main(self.argv(), stdin=stream)
                self.assertNotIn(JIT_B64, str(ctx.exception))
                self.assertFalse(os.path.exists(self.admission_dir))

    def test_receipt_denials_abort_without_marker(self):
        def base(req):
            return dict(req, decision='allow',
                        executionDeadlineUnixMs=int(time.time() * 1000) + 60000)
        cases = {
            'deny': lambda req: dict(req, decision='deny'),
            'missing-runner-name': lambda req: {k: v for k, v in base(req).items()
                                                 if k != 'runnerName'},
            'extra-key': lambda req: dict(base(req), surprise=1),
            'mismatched-run-id': lambda req: dict(base(req), githubRunId=888),
            'mismatched-repo': lambda req: dict(base(req), repository='octo-org/other'),
            'bool-deadline': lambda req: dict(base(req), executionDeadlineUnixMs=True),
            'expired': lambda req: dict(base(req),
                                        executionDeadlineUnixMs=int(time.time() * 1000) - 1000),
            'overlong': lambda req: dict(base(req),
                                         executionDeadlineUnixMs=int(time.time() * 1000)
                                         + jit.MAX_EXECUTION_DEADLINE_MS + 60000),
            'bad-decision': lambda req: dict(base(req), decision='maybe'),
            'bool-echo': lambda req: dict(base(req), githubRunId=True),
            'string-deadline': lambda req: dict(base(req), executionDeadlineUnixMs='123'),
        }
        for label, fn in cases.items():
            with self.subTest(case=label):
                self.fresh_dirs(label)
                self.write_script('run.sh', self.admit_script())
                writer = self.fn_writer(fn)
                rc, _, _ = self.run_main()
                writer.join(timeout=5)
                self.assertEqual(rc, jit.DENIED_EXIT, label)
                self.assertFalse(Path(self.runner_dir, 'marker').exists(), label)

    def test_malformed_receipts_abort_without_marker(self):
        def base(req):
            return dict(req, decision='allow',
                        executionDeadlineUnixMs=int(time.time() * 1000) + 60000)
        cases = {
            'not-json': lambda req: b'this is not json',
            'empty': lambda req: b'',
            'list': lambda req: b'[1, 2, 3]',
            'trailing': lambda req: json.dumps(base(req)).encode() + b' {}',
            'duplicate-keys': lambda req: (json.dumps(base(req))[:-1].encode()
                                           + b', "runnerName": "ci-jit-1"}'),
            'nan': lambda req: (json.dumps(base(req))[:-1].encode()
                                + b', "x": NaN}'),
        }
        for label, fn in cases.items():
            with self.subTest(case=label):
                self.fresh_dirs('raw-' + label)
                self.write_script('run.sh', self.admit_script())
                writer = self.fn_writer(fn)
                rc, _, _ = self.run_main()
                writer.join(timeout=5)
                self.assertEqual(rc, jit.DENIED_EXIT, label)
                self.assertFalse(Path(self.runner_dir, 'marker').exists(), label)

    def test_missing_allow_times_out(self):
        self.write_script('run.sh', self.admit_script())
        with mock.patch.object(jit, 'REQUEST_WAIT_SECONDS', 1):
            rc, _, _ = self.run_main()
        self.assertEqual(rc, jit.TIMEOUT_EXIT)
        self.assertFalse(Path(self.runner_dir, 'marker').exists())

    def test_setup_timeout_without_request_kills_group(self):
        d = self.runner_dir
        self.write_script('run.sh',
                          self.child_pid_file() + 'trap \'\' TERM\nwait\n')
        with mock.patch.object(jit, 'ADMISSION_SECONDS', 1):
            rc, _, _ = self.run_main()
        self.assertEqual(rc, jit.TIMEOUT_EXIT)
        for name in ('run.pid', 'sleep.pid'):
            pid = int(Path(d, name).read_text().strip())
            self.assertTrue(self.wait_dead(pid), name + ' survived setup timeout')
        self.assertFalse(Path(d, 'marker').exists())

    def test_execution_deadline_stops_group(self):
        self.write_script('run.sh', self.admit_script('sleep 300'))
        writer = self.allow_writer(
            deadline_ms=int(time.time() * 1000) + 1500)
        started = time.monotonic()
        rc, _, _ = self.run_main()
        writer.join(timeout=5)
        self.assertEqual(rc, jit.TIMEOUT_EXIT)
        self.assertLess(time.monotonic() - started, 15)
        self.assertEqual(Path(self.runner_dir, 'marker').read_text().strip(), 'ran')

    def test_sigterm_kills_term_ignoring_grandchild(self):
        d = self.runner_dir
        self.write_script('run.sh',
                          'echo $$ > "' + d + '/run.pid"\n'
                          '( trap \'\' TERM INT; exec sleep 300 ) &\n'
                          'echo $! > "' + d + '/grandchild.pid"\n'
                          'trap \'exit 0\' TERM INT\nwait\n')
        thread = self.signal_when_ready(Path(d, 'grandchild.pid'), signal.SIGTERM)
        try:
            rc, _, _ = self.run_main()
        finally:
            thread.join(timeout=5)
        self.assertEqual(rc, 128 + signal.SIGTERM)
        pid = int(Path(d, 'grandchild.pid').read_text().strip())
        self.assertTrue(self.wait_dead(pid), 'term-ignoring grandchild survived')
        run_pid = int(Path(d, 'run.pid').read_text().strip())
        self.assertTrue(self.wait_dead(run_pid))

    def test_child_exit_zero_without_allow_is_denied(self):
        self.write_script('run.sh', 'exit 0')
        rc, _, _ = self.run_main()
        self.assertEqual(rc, jit.DENIED_EXIT)

    def test_approved_listener_failure_preserved(self):
        self.write_script('run.sh', self.admit_script('exit 3'))
        writer = self.allow_writer()
        rc, _, _ = self.run_main()
        writer.join(timeout=5)
        self.assertEqual(rc, 3)

    def test_approved_listener_signal_normalized(self):
        self.write_script('run.sh', self.admit_script('kill -TERM $$'))
        writer = self.allow_writer()
        rc, _, _ = self.run_main()
        writer.join(timeout=5)
        self.assertEqual(rc, 128 + signal.SIGTERM)

    def test_no_jit_in_argv_diagnostics_or_protocol_files(self):
        self.write_script('run.sh', self.admit_script())
        writer = self.allow_writer()
        rc, _, err = self.run_main()
        writer.join(timeout=5)
        self.assertEqual(rc, 0)
        self.assertNotIn(JIT_B64, err)
        self.assertEqual(Path(self.runner_dir, 'run.args').read_text(), '')
        for name in ('request.json', 'receipt.json', 'config.json', 'job-started.sh'):
            content = Path(self.admission_dir, name).read_text()
            self.assertNotIn(JIT_B64, content, name)
        self.assertIn(JIT_B64,
                      Path(self.runner_dir, 'child.env').read_text())

    def test_admission_dir_must_be_fresh(self):
        self.write_script('run.sh', 'exit 0')
        os.mkdir(self.admission_dir, 0o700)
        with self.assertRaises(RuntimeError):
            jit.main(self.argv(), stdin=self.stdin())
        self.fresh_dirs('symlink')
        os.symlink(self.tmp.name, self.admission_dir)
        with self.assertRaises(RuntimeError):
            jit.main(self.argv(), stdin=self.stdin())

    def test_admission_parent_must_be_private(self):
        self.write_script('run.sh', 'exit 0')
        bad = os.path.join(self.tmp.name, 'badparent')
        os.mkdir(bad, 0o755)
        os.chmod(bad, 0o777)
        argv = ['run', '--runner-dir', self.runner_dir, '--name', RUNNER_NAME,
                '--github-runner-id', RUNNER_ID, '--github-repository-id', REPO_ID,
                '--admission-dir', os.path.join(bad, 'adm'),
                '--admission-id', ADMISSION_ID]
        with self.assertRaises(RuntimeError):
            jit.main(argv, stdin=self.stdin())
        missing = os.path.join(self.tmp.name, 'no-such-parent', 'adm')
        argv[argv.index('--admission-dir') + 1] = missing
        with self.assertRaises(RuntimeError):
            jit.main(argv, stdin=self.stdin())

    def test_strict_arguments_rejected(self):
        self.write_script('run.sh', 'exit 0')
        for value in ('0', '-1', 'abc', '1.5', '9223372036854775808', ' 5', ''):
            with self.subTest(field='runner-id', value=value):
                argv = self.argv()
                argv[argv.index('--github-runner-id') + 1] = value
                with self.assertRaises(SystemExit):
                    jit.main(argv, stdin=self.stdin())
        for value in ('a' * 63, 'A' * 64, 'g' * 64, 'ab' * 31 + 'zz'):
            with self.subTest(field='admission-id', value=value[:20]):
                argv = self.argv()
                argv[argv.index('--admission-id') + 1] = value
                with self.assertRaises(SystemExit):
                    jit.main(argv, stdin=self.stdin())
        for value in ('bad name', 'x' * 65, 'na/me', ''):
            with self.subTest(field='name', value=value):
                argv = self.argv()
                argv[argv.index('--name') + 1] = value
                with self.assertRaises(SystemExit):
                    jit.main(argv, stdin=self.stdin())
        for value in ('299', '22501', 'abc', '10.5', '-5'):
            with self.subTest(field='timeout', value=value):
                with self.assertRaises(SystemExit):
                    jit.main(self.argv('--timeout-seconds', value), stdin=self.stdin())

    def test_runner_dir_and_host_checks(self):
        self.write_script('config.sh', 'exit 0')
        Path(self.runner_dir, '.runner').write_text('mimic')
        with self.assertRaises(RuntimeError):
            jit.main(self.argv(), stdin=self.stdin())
        fresh = os.path.join(self.tmp.name, 'runner2')
        os.mkdir(fresh, 0o755)
        Path(fresh, 'config.sh').write_text('#!/bin/sh\nexit 0\n')
        os.chmod(Path(fresh, 'config.sh'), 0o755)
        Path(fresh, 'run.sh').write_text('#!/bin/sh\nexit 0\n')
        os.chmod(Path(fresh, 'run.sh'), 0o755)
        argv = self.argv()
        argv[argv.index('--runner-dir') + 1] = fresh
        with mock.patch.object(legacy, 'check_host',
                               side_effect=RuntimeError('not linux')):
            with self.assertRaisesRegex(RuntimeError, 'not linux'):
                jit.main(argv, stdin=self.stdin())

    def test_context_validation_unit(self):
        config = {'schemaVersion': 1, 'admissionId': ADMISSION_ID,
                  'githubRunnerId': int(RUNNER_ID),
                  'githubRepositoryId': int(REPO_ID), 'runnerName': RUNNER_NAME}
        request = jit._context_request(config, dict(HOOK_ENV_DICT))
        self.assertEqual(request['repository'], 'octo-org/widgets')
        self.assertEqual(request['githubRunId'], 777)
        self.assertEqual(request['jobKey'], 'build')
        bad = [
            {'GITHUB_REPOSITORY': None},
            {'GITHUB_REPOSITORY': 'bad repo/x'},
            {'GITHUB_REPOSITORY_ID': '1'},
            {'GITHUB_REPOSITORY_ID': 'abc'},
            {'RUNNER_NAME': 'other-runner'},
            {'GITHUB_RUN_ID': '0'},
            {'GITHUB_RUN_ID': 'abc'},
            {'GITHUB_RUN_ATTEMPT': '-2'},
            {'GITHUB_JOB': 'bad key!'},
            {'GITHUB_JOB': ''},
            {'GITHUB_SERVER_URL': 'http://github.com'},
            {'GITHUB_SERVER_URL': 'https://ghe.example.com'},
            {'GITHUB_API_URL': 'http://api.github.com'},
        ]
        for override in bad:
            with self.subTest(override=override):
                env = dict(HOOK_ENV_DICT)
                env.update(override)
                for key, value in list(env.items()):
                    if value is None:
                        del env[key]
                with self.assertRaises(jit.ContextError):
                    jit._context_request(config, env)

    def test_private_protocol_files_rejected(self):
        directory = os.path.join(self.tmp.name, 'priv')
        os.mkdir(directory, 0o700)
        good = os.path.join(directory, 'good.json')
        fd = os.open(good, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.write(fd, b'{"a": 1}')
        os.close(fd)
        self.assertEqual(jit._read_private_json(good), {'a': 1})
        link = os.path.join(directory, 'link.json')
        os.symlink(good, link)
        with self.assertRaises(jit.ProtocolError):
            jit._read_private_json(link)
        fifo = os.path.join(directory, 'fifo.json')
        os.mkfifo(fifo, 0o600)
        with self.assertRaises(jit.ProtocolError):
            jit._read_private_json(fifo)
        wide = os.path.join(directory, 'wide.json')
        fd = os.open(wide, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
        os.write(fd, b'{"a": 1}')
        os.close(fd)
        os.chmod(wide, 0o644)
        with self.assertRaises(jit.ProtocolError):
            jit._read_private_json(wide)
        big = os.path.join(directory, 'big.json')
        with open(big, 'wb') as stream:
            stream.write(b'{"a": "' + b'x' * 20000 + b'"}')
        os.chmod(big, 0o600)
        with self.assertRaises(jit.ProtocolError):
            jit._read_private_json(big)
        for label, raw in (('dup', b'{"a": 1, "a": 2}'),
                           ('trailing', b'{"a": 1} {}'),
                           ('nan', b'{"a": NaN}')):
            path = os.path.join(directory, label + '.json')
            with open(path, 'wb') as stream:
                stream.write(raw)
            os.chmod(path, 0o600)
            with self.assertRaises(jit.ProtocolError, msg=label):
                jit._read_private_json(path)
        list_path = os.path.join(directory, 'list.json')
        with open(list_path, 'wb') as stream:
            stream.write(b'[1]')
        os.chmod(list_path, 0o600)
        parsed = jit._read_private_json(list_path)
        with self.assertRaises(jit.ProtocolError):
            jit._validate_receipt(parsed, {}, 0)
        open_dir = os.path.join(directory, 'open')
        os.mkdir(open_dir, 0o755)
        os.chmod(open_dir, 0o755)
        with self.assertRaises(jit.ProtocolError):
            jit._check_private_dir(open_dir)
        jit._check_private_dir(directory)

    def test_hook_allow_exits_zero(self):
        directory, cfg = self.hook_setup('hook-allow')
        proc = self.spawn_hook(cfg, self.hook_env())
        try:
            self.assertTrue(self.wait_file(os.path.join(directory, 'request.json')))
            req = json.loads(Path(directory, 'request.json').read_text())
            rec = dict(req, decision='allow',
                       executionDeadlineUnixMs=int(time.time() * 1000) + 60000)
            tmp = os.path.join(directory, 'receipt.tmp')
            with open(tmp, 'w') as stream:
                json.dump(rec, stream)
            os.chmod(tmp, 0o600)
            os.replace(tmp, os.path.join(directory, 'receipt.json'))
            self.assertEqual(proc.wait(timeout=15), 0)
            self.assertFalse(os.path.exists(os.path.join(directory, 'denied.json')))
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

    def test_hook_deny_blocks_forever(self):
        directory, cfg = self.hook_setup('hook-deny')
        proc = self.spawn_hook(cfg, self.hook_env())
        try:
            self.assertTrue(self.wait_file(os.path.join(directory, 'request.json')))
            req = json.loads(Path(directory, 'request.json').read_text())
            tmp = os.path.join(directory, 'receipt.tmp')
            with open(tmp, 'w') as stream:
                json.dump(dict(req, decision='deny'), stream)
            os.chmod(tmp, 0o600)
            os.replace(tmp, os.path.join(directory, 'receipt.json'))
            denied = os.path.join(directory, 'denied.json')
            self.assertTrue(self.wait_file(denied, timeout=15))
            self.assertEqual(json.loads(Path(denied).read_text())['reason'], 'deny')
            time.sleep(0.3)
            self.assertIsNone(proc.poll(), 'denied hook must keep blocking')
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

    def test_hook_invalid_context_blocks(self):
        directory, cfg = self.hook_setup('hook-badctx')
        proc = self.spawn_hook(cfg, self.hook_env(GITHUB_REPOSITORY=None))
        try:
            denied = os.path.join(directory, 'denied.json')
            self.assertTrue(self.wait_file(denied, timeout=15))
            self.assertEqual(json.loads(Path(denied).read_text())['reason'],
                             'invalid-context')
            self.assertFalse(os.path.exists(os.path.join(directory, 'request.json')))
            time.sleep(0.3)
            self.assertIsNone(proc.poll())
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

    def test_hook_duplicate_request_blocks(self):
        directory, cfg = self.hook_setup('hook-dup')
        req_path = os.path.join(directory, 'request.json')
        with open(req_path, 'w') as stream:
            stream.write('{}')
        os.chmod(req_path, 0o600)
        proc = self.spawn_hook(cfg, self.hook_env())
        try:
            denied = os.path.join(directory, 'denied.json')
            self.assertTrue(self.wait_file(denied, timeout=15))
            self.assertEqual(json.loads(Path(denied).read_text())['reason'],
                             'duplicate-request')
            time.sleep(0.3)
            self.assertIsNone(proc.poll())
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

    def test_hook_signal_records_denial_and_blocks(self):
        directory, cfg = self.hook_setup('hook-sig')
        proc = self.spawn_hook(cfg, self.hook_env())
        try:
            self.assertTrue(self.wait_file(os.path.join(directory, 'request.json')))
            os.kill(proc.pid, signal.SIGTERM)
            denied = os.path.join(directory, 'denied.json')
            self.assertTrue(self.wait_file(denied, timeout=15))
            self.assertEqual(json.loads(Path(denied).read_text())['reason'], 'signal')
            time.sleep(0.3)
            self.assertIsNone(proc.poll(), 'interrupted hook must keep blocking')
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

    def test_wait_receipt_unit_timeout(self):
        directory = os.path.join(self.tmp.name, 'waitunit')
        os.mkdir(directory, 0o700)
        state = legacy.InterruptState()
        with self.assertRaises(TimeoutError):
            jit._wait_receipt(directory, {}, time.monotonic() + 0.3, state)

    def test_publish_atomic_barrier(self):
        directory = os.path.join(os.path.realpath(self.tmp.name), 'pub')
        os.mkdir(directory, 0o700)
        entered = threading.Event()
        release = threading.Event()
        real_fsync = os.fsync

        def barrier(fd):
            entered.set()
            release.wait(10)
            real_fsync(fd)

        payload = b'{"a": 1}\n'
        with mock.patch.object(jit.os, 'fsync', side_effect=barrier):
            thread = threading.Thread(
                target=jit._publish_exclusive,
                args=(directory, 'request.json', payload, 0o600))
            thread.start()
            self.assertTrue(entered.wait(10))
            final = os.path.join(directory, 'request.json')
            pending = final + '.pending'
            self.assertFalse(os.path.exists(final),
                             'final name must stay absent until publish completes')
            self.assertTrue(os.path.exists(pending))
            self.assertEqual(Path(pending).read_bytes(), payload)
            release.set()
            thread.join(10)
        self.assertEqual(json.loads(Path(final).read_text()), {'a': 1})
        self.assertFalse(os.path.exists(pending))
        with self.assertRaises(FileExistsError):
            jit._publish_exclusive(directory, 'request.json', b'{"b": 2}\n', 0o600)
        self.assertEqual(json.loads(Path(final).read_text()), {'a': 1})
        self.assertFalse(os.path.exists(pending))

    def test_load_config_rejects_symlinked_paths(self):
        directory, cfg = self.hook_setup('cfgcanon')
        config, admission = jit._load_config(cfg)
        self.assertEqual(config['admissionId'], ADMISSION_ID)
        self.assertEqual(admission, directory)
        base = os.path.realpath(self.tmp.name)
        alias = os.path.join(base, 'aliasdir')
        os.mkdir(alias, 0o700)
        os.symlink(cfg, os.path.join(alias, 'config.json'))
        with self.assertRaises(jit.ContextError):
            jit._load_config(os.path.join(alias, 'config.json'))
        ancestor = os.path.join(base, 'linkdir')
        os.symlink(directory, ancestor)
        with self.assertRaises(jit.ContextError):
            jit._load_config(os.path.join(ancestor, 'config.json'))

    def test_foreign_request_binding_rejected(self):
        foreign = {'schemaVersion': 1, 'admissionId': 'cd' * 32,
                   'githubRunnerId': 999, 'githubRepositoryId': 555,
                   'repository': 'octo-org/widgets', 'githubRunId': 777,
                   'runAttempt': 2, 'jobKey': 'build', 'runnerName': RUNNER_NAME}
        payload = json.dumps(foreign, separators=(',', ':'))
        d = self.runner_dir
        a = self.admission_dir
        self.write_script('run.sh',
                          'umask 077\n'
                          'printf %s \'' + payload + '\' > "' + a + '/request.json"\n'
                          'sleep 300\n')
        writer = self.fn_writer(
            lambda req: dict(req, decision='allow',
                             executionDeadlineUnixMs=int(time.time() * 1000) + 60000))
        rc, _, _ = self.run_main()
        writer.join(timeout=5)
        self.assertEqual(rc, jit.DENIED_EXIT)
        self.assertFalse(Path(d, 'marker').exists())

    def test_handlers_installed_before_spawn_and_restored(self):
        self.write_script('run.sh', 'exit 0')
        previous = {sig: signal.getsignal(sig)
                    for sig in (signal.SIGINT, signal.SIGTERM)}
        seen = {}
        real_popen = subprocess.Popen

        def spy(*args, **kwargs):
            seen['sigint'] = signal.getsignal(signal.SIGINT)
            seen['sigterm'] = signal.getsignal(signal.SIGTERM)
            return real_popen(*args, **kwargs)

        with mock.patch.object(jit.subprocess, 'Popen', side_effect=spy):
            rc, _, _ = self.run_main()
        self.assertEqual(rc, jit.DENIED_EXIT)
        for key in ('sigint', 'sigterm'):
            handler = seen[key]
            self.assertTrue(callable(handler), key)
            self.assertNotEqual(handler, signal.SIG_DFL, key)
            self.assertEqual(getattr(handler, '__self__', None).__class__.__name__,
                             'InterruptState', key)
        self.assertEqual(signal.getsignal(signal.SIGINT), previous[signal.SIGINT])
        self.assertEqual(signal.getsignal(signal.SIGTERM), previous[signal.SIGTERM])

    def test_installed_handler_interrupt_stops_group(self):
        d = self.runner_dir
        self.write_script('run.sh',
                          'echo $$ > "' + d + '/run.pid"\n'
                          'echo ready > "' + d + '/ready"\n'
                          'sleep 300\n')
        seen = {}
        real_popen = subprocess.Popen

        def spy(*args, **kwargs):
            seen['handler'] = signal.getsignal(signal.SIGINT)
            return real_popen(*args, **kwargs)

        def fire():
            self.wait_file(Path(d, 'ready'))
            seen['handler'](signal.SIGINT, None)

        thread = threading.Thread(target=fire, daemon=True)
        thread.start()
        with mock.patch.object(jit.subprocess, 'Popen', side_effect=spy):
            rc, _, _ = self.run_main()
        thread.join(timeout=5)
        self.assertEqual(rc, 128 + signal.SIGINT)
        pid = int(Path(d, 'run.pid').read_text().strip())
        self.assertTrue(self.wait_dead(pid))

    def test_supervise_exception_cleans_group_and_handlers(self):
        d = self.runner_dir
        self.write_script('run.sh',
                          'echo $$ > "' + d + '/run.pid"\nsleep 300\n')
        before = signal.getsignal(signal.SIGINT)

        def boom(*args, **kwargs):
            deadline = time.time() + 10
            while time.time() < deadline:
                if os.path.exists(os.path.join(d, 'run.pid')):
                    break
                time.sleep(0.01)
            raise RuntimeError('boom')

        with mock.patch.object(jit, '_supervise', side_effect=boom):
            with self.assertRaises(RuntimeError):
                self.run_main()
        pid = int(Path(d, 'run.pid').read_text().strip())
        self.assertTrue(self.wait_dead(pid), 'child survived supervisor exception')
        self.assertEqual(signal.getsignal(signal.SIGINT), before)

    def test_sigint_kills_term_ignoring_grandchild(self):
        d = self.runner_dir
        self.write_script('run.sh',
                          'echo $$ > "' + d + '/run.pid"\n'
                          '( trap \'\' TERM INT; exec sleep 300 ) &\n'
                          'echo $! > "' + d + '/grandchild.pid"\n'
                          'trap \'exit 0\' TERM INT\nwait\n')
        thread = self.signal_when_ready(Path(d, 'grandchild.pid'), signal.SIGINT)
        try:
            rc, _, _ = self.run_main()
        finally:
            thread.join(timeout=5)
        self.assertEqual(rc, 128 + signal.SIGINT)
        pid = int(Path(d, 'grandchild.pid').read_text().strip())
        self.assertTrue(self.wait_dead(pid), 'term-ignoring grandchild survived')


class Clock:
    def __init__(self):
        self.t = 0.0

    def __call__(self):
        return self.t


class FakeProc:
    def __init__(self, clock, step, rc=0, exit_at=None, on_poll=None):
        self.pid = 4194304
        self.returncode = None
        self._rc = rc
        self._clock = clock
        self._step = step
        self._exit_at = exit_at
        self._on_poll = on_poll

    def poll(self):
        self._clock.t += self._step
        if self._on_poll is not None:
            self._on_poll(self._clock.t)
        if self._exit_at is not None and self._clock.t >= self._exit_at:
            self.returncode = self._rc
            return self._rc
        return None

    def wait(self, timeout=None):
        if self.returncode is None:
            self.returncode = self._rc
        return self.returncode


SUPERVISE_CONFIG = {'schemaVersion': 1, 'admissionId': ADMISSION_ID,
                    'githubRunnerId': int(RUNNER_ID),
                    'githubRepositoryId': int(REPO_ID),
                    'runnerName': RUNNER_NAME}
SUPERVISE_REQUEST = dict(SUPERVISE_CONFIG, repository='octo-org/widgets',
                         githubRunId=777, runAttempt=2, jobKey='build')


def write_private_json(directory, name, obj):
    data = json.dumps(obj, separators=(',', ':')).encode()
    fd = os.open(os.path.join(directory, name),
                 os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        os.write(fd, data)
    finally:
        os.close(fd)


def allow_receipt(now_ms=None):
    rec = dict(SUPERVISE_REQUEST, decision='allow',
               executionDeadlineUnixMs=(now_ms if now_ms is not None
                                         else int(time.time() * 1000) + 60000))
    return rec


class SuperviseCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def private_dir(self, tag):
        directory = os.path.join(os.path.realpath(self.tmp.name), tag)
        os.mkdir(directory, 0o700)
        return directory

    def run_supervise(self, proc, directory, interrupted=None):
        clock = getattr(proc, '_clock', None)
        patches = [mock.patch.object(jit, 'POLL_SECONDS', 0),
                   mock.patch.object(legacy, 'kill_group')]
        if clock is not None:
            patches.append(mock.patch.object(jit, 'monotonic', clock))
        with contextlib.ExitStack() as stack:
            for patch in patches:
                stack.enter_context(patch)
            kill = legacy.kill_group
            rc = jit._supervise(proc, directory, 22500,
                                interrupted or (lambda: None),
                                SUPERVISE_CONFIG)
        kill.assert_called_once_with(proc, signal.SIGKILL)
        return rc

    def test_request_and_allow_after_admission_cap_times_out(self):
        directory = self.private_dir('sup-late')
        clock = Clock()
        written = set()

        def on_poll(t):
            if t >= 899 and 'request' not in written:
                write_private_json(directory, 'request.json', SUPERVISE_REQUEST)
                written.add('request')
            if t >= 905 and 'receipt' not in written:
                write_private_json(directory, 'receipt.json', allow_receipt())
                written.add('receipt')

        proc = FakeProc(clock, 1, exit_at=905, on_poll=on_poll)
        rc = self.run_supervise(proc, directory)
        self.assertEqual(rc, jit.TIMEOUT_EXIT)
        self.assertEqual(clock.t, 900)
        self.assertIn('request', written)
        self.assertNotIn('receipt', written)
        self.assertFalse(os.path.exists(
            os.path.join(directory, 'receipt.json')))

    def test_request_wait_cannot_extend_admission_cap(self):
        directory = self.private_dir('sup-wait')
        clock = Clock()
        written = set()

        def on_poll(t):
            if t >= 899 and 'request' not in written:
                write_private_json(directory, 'request.json', SUPERVISE_REQUEST)
                written.add('request')

        proc = FakeProc(clock, 1, on_poll=on_poll)
        rc = self.run_supervise(proc, directory)
        self.assertEqual(rc, jit.TIMEOUT_EXIT)
        self.assertEqual(clock.t, 900)
        self.assertIn('request', written)
        self.assertTrue(os.path.exists(
            os.path.join(directory, 'request.json')))

    def test_late_receipt_past_request_wait_times_out(self):
        directory = self.private_dir('sup-late-req')
        clock = Clock()
        written = set()

        def on_poll(t):
            if t >= 25 and 'request' not in written:
                write_private_json(directory, 'request.json', SUPERVISE_REQUEST)
                written.add('request')
            if t >= 75 and 'receipt' not in written:
                write_private_json(directory, 'receipt.json', allow_receipt())
                written.add('receipt')

        proc = FakeProc(clock, 25, exit_at=75, on_poll=on_poll)
        rc = self.run_supervise(proc, directory)
        self.assertEqual(rc, jit.TIMEOUT_EXIT)
        self.assertEqual(clock.t, 75)
        self.assertEqual(written, {'request', 'receipt'})
        self.assertTrue(os.path.exists(
            os.path.join(directory, 'receipt.json')))

    def test_clean_allow_then_exit_returns_zero(self):
        directory = self.private_dir('sup-ok')
        clock = Clock()
        written = set()

        def on_poll(t):
            if t >= 100 and 'request' not in written:
                write_private_json(directory, 'request.json', SUPERVISE_REQUEST)
                written.add('request')
            if t >= 110 and 'receipt' not in written:
                write_private_json(directory, 'receipt.json', allow_receipt())
                written.add('receipt')

        proc = FakeProc(clock, 10, exit_at=120, on_poll=on_poll)
        rc = self.run_supervise(proc, directory)
        self.assertEqual(rc, 0)

    def test_terminal_denial_observation_honored(self):
        directory = self.private_dir('sup-deny')
        clock = Clock()

        def on_poll(t):
            if t >= 50 and not os.path.exists(
                    os.path.join(directory, 'denied.json')):
                write_private_json(directory, 'denied.json',
                                   {'reason': 'deny'})

        proc = FakeProc(clock, 50, exit_at=50, on_poll=on_poll)
        rc = self.run_supervise(proc, directory)
        self.assertEqual(rc, jit.DENIED_EXIT)
        self.assertTrue(os.path.exists(
            os.path.join(directory, 'denied.json')))

    def test_terminal_timeout_when_exit_first_seen_at_cutoff(self):
        for exit_at, expected in ((800, jit.DENIED_EXIT),
                                  (900, jit.TIMEOUT_EXIT)):
            with self.subTest(exit_at=exit_at):
                directory = self.private_dir('sup-cut-%d' % exit_at)
                clock = Clock()
                proc = FakeProc(clock, 100, exit_at=exit_at)
                rc = self.run_supervise(proc, directory)
                self.assertEqual(rc, expected)

    def test_terminal_interrupt_returns_signal(self):
        directory = self.private_dir('sup-int')
        clock = Clock()
        proc = FakeProc(clock, 100, exit_at=100)
        rc = self.run_supervise(proc, directory,
                                interrupted=lambda: signal.SIGTERM)
        self.assertEqual(rc, 128 + signal.SIGTERM)


if __name__ == '__main__':
    unittest.main()

import base64
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import sys
import tempfile
import threading
import time
import unittest
from types import SimpleNamespace
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

spec = importlib.util.spec_from_file_location(
    'canter_ci_guest_bootstrap',
    Path(__file__).with_name('canter-ci-guest-bootstrap.py'))
gb = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gb)
jit = gb.jit
legacy = gb.legacy

OP_ID = 'op-guest-1_Xy'
ADMISSION_ID = 'cd' * 32
RUNNER_ID = 4242
REPO_ID = 987
RUNNER_NAME = 'ci-jit-1'
MAX_SECONDS = 3600


def _wire(payload, tag):
    enc = lambda data: base64.urlsafe_b64encode(data).rstrip(b'=').decode()
    return 'v1.' + enc(payload) + '.' + enc(tag)


BOOTSTRAP_TOKEN = _wire(b'{"bootstrap":true}', b'bootstrap-tag-marker-0123456789')
ADMISSION_TOKEN = _wire(b'{"admission":true}', b'admission-tag-marker-0123456789abcdef')
JIT_B64 = base64.b64encode(
    b'{"runnerCredentials":"' + b'j' * 20000 + b'"}').decode()

HOOK_ENV = ('GITHUB_REPOSITORY=octo-org/widgets GITHUB_REPOSITORY_ID=' + str(REPO_ID) +
            ' RUNNER_NAME=' + RUNNER_NAME + ' GITHUB_RUN_ID=777 GITHUB_RUN_ATTEMPT=2 ' +
            'GITHUB_JOB=build GITHUB_SERVER_URL=https://github.com ' +
            'GITHUB_API_URL=https://api.github.com ')


class Fixture(unittest.TestCase):
    def setUp(self):
        patcher = mock.patch.object(legacy, 'check_host')
        patcher.start()
        self.addCleanup(patcher.stop)
        self.check_host_mock = legacy.check_host
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.fresh_dirs('main')
        self.now = time.time()
        self.setup_ms = int((self.now + 600) * 1000)
        self.hard_ms = int((self.now + 3600) * 1000)
        self.expires_at = int(self.now) + 500
        self.receipt_ttl_ms = 3600000
        self.hits = {'bootstrap': [], 'admission': []}
        self.bootstrap_queue = []
        self.bootstrap_raw = None
        self.drip = False
        self.bootstrap_gate = None
        self.admission_gate = None
        self.admission_seen = None
        self.admission_handler = None
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                try:
                    length = int(self.headers.get('Content-Length') or 0)
                except ValueError:
                    length = 0
                body = self.rfile.read(min(length, 1 << 20))
                entry = {'path': self.path,
                         'authorization': self.headers.get('Authorization'),
                         'body': body}
                if self.path == '/v1/ci/runner-bootstrap':
                    fixture.hits['bootstrap'].append(entry)
                    fixture._respond_bootstrap(self, body)
                elif self.path == '/v1/ci/runner-admissions':
                    fixture.hits['admission'].append(entry)
                    fixture._respond_admission(self, body)
                else:
                    self.send_response(404)
                    self.send_header('Content-Length', '2')
                    self.end_headers()
                    try:
                        self.wfile.write(b'{}')
                    except (BrokenPipeError, ConnectionResetError):
                        pass

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever,
                                       daemon=True)
        self.thread.start()
        self.addCleanup(self.server.shutdown)
        self.addCleanup(self.server.server_close)
        self.endpoint = ('http://127.0.0.1:%d/v1/ci/runner-bootstrap'
                         % self.server.server_port)

    def fresh_dirs(self, tag):
        base = os.path.realpath(self.tmp.name)
        self.runner_dir = os.path.join(base, 'runner-' + tag)
        os.mkdir(self.runner_dir, 0o755)
        self.admission_dir = os.path.join(base, 'adm-' + tag)
        self.write_script('config.sh',
                          'touch "' + self.runner_dir + '/config.called"')
        self.write_script('run.sh', self.admit_script())
        self.bootstrap_raw = None
        self.bootstrap_queue = []
        self.bootstrap_gate = None
        self.admission_gate = None
        self.admission_seen = None
        self.admission_handler = None
        self.drip = False
        self.hits = {'bootstrap': [], 'admission': []}

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

    def hook_then_wait(self):
        d = self.runner_dir
        return ('echo $$ > "' + d + '/run.pid"\n'
                'sleep 300 &\n'
                'echo $! > "' + d + '/sleep.pid"\n'
                'echo ready > "' + d + '/ready"\n'
                'if [ -n "${ACTIONS_RUNNER_HOOK_JOB_STARTED:-}" ]; then\n'
                '    ' + HOOK_ENV +
                '/bin/sh "$ACTIONS_RUNNER_HOOK_JOB_STARTED"\n'
                'fi\n'
                'wait\n')

    def response_obj(self, mutate=None):
        obj = {'version': 1, 'operationId': OP_ID, 'admissionId': ADMISSION_ID,
               'runnerName': RUNNER_NAME, 'githubRunnerId': RUNNER_ID,
               'githubRepositoryId': REPO_ID, 'maximumSeconds': MAX_SECONDS,
               'setupDeadlineUnixMs': self.setup_ms,
               'hardDeadlineUnixMs': self.hard_ms,
               'encodedJitConfig': JIT_B64,
               'admissionCapability': ADMISSION_TOKEN}
        if mutate is not None:
            mutate(obj)
        return obj

    def response_bytes(self, mutate=None):
        return json.dumps(self.response_obj(mutate),
                          separators=(',', ':')).encode()

    def default_admission(self, body):
        req = json.loads(body)
        rec = dict(req)
        rec['decision'] = 'allow'
        rec['executionDeadlineUnixMs'] = (int(time.time() * 1000)
                                          + self.receipt_ttl_ms)
        return 200, json.dumps(rec, separators=(',', ':')).encode()

    def _respond_bootstrap(self, handler, body):
        if self.bootstrap_gate is not None:
            self.bootstrap_gate.wait(20)
        if self.bootstrap_raw is not None:
            raw = self.bootstrap_raw
            handler.send_response(200)
            handler.send_header('Content-Type', 'application/json')
            handler.send_header('Content-Length', str(len(raw)))
            handler.end_headers()
            if self.drip:
                for i in range(0, len(raw), 8):
                    try:
                        handler.wfile.write(raw[i:i + 8])
                        handler.wfile.flush()
                    except (BrokenPipeError, ConnectionResetError):
                        return
                    time.sleep(0.1)
                return
            try:
                handler.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        if self.bootstrap_queue:
            status, payload, extra = self.bootstrap_queue.pop(0)
        else:
            status, payload, extra = 200, self.response_bytes(), {}
        handler.send_response(status)
        handler.send_header('Content-Type', 'application/json')
        handler.send_header('Content-Length', str(len(payload)))
        for key, value in extra.items():
            handler.send_header(key, value)
        handler.end_headers()
        try:
            handler.wfile.write(payload)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _respond_admission(self, handler, body):
        if self.admission_seen is not None:
            self.admission_seen.set()
        if self.admission_gate is not None:
            self.admission_gate.wait(20)
        if self.admission_handler is not None:
            status, payload = self.admission_handler(body)
        else:
            status, payload = self.default_admission(body)
        handler.send_response(status)
        handler.send_header('Content-Type', 'application/json')
        handler.send_header('Content-Length', str(len(payload)))
        handler.end_headers()
        try:
            handler.wfile.write(payload)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def token_fd(self, data=BOOTSTRAP_TOKEN):
        read_fd, write_fd = os.pipe()
        os.write(write_fd, data.encode())
        os.close(write_fd)
        self.addCleanup(lambda: self._close_quiet(read_fd))
        return read_fd

    @staticmethod
    def _close_quiet(fd):
        try:
            os.close(fd)
        except OSError:
            pass

    def argv(self, fd, wait=None, endpoint=None, setup_ms=None, hard_ms=None,
             expires=None, allow_http=True):
        argv = ['--endpoint', endpoint or self.endpoint,
                '--runner-dir', self.runner_dir,
                '--admission-dir', self.admission_dir,
                '--token-fd', str(fd),
                '--operation-id', OP_ID,
                '--admission-id', ADMISSION_ID,
                '--name', RUNNER_NAME,
                '--github-repository-id', str(REPO_ID),
                '--maximum-seconds', str(MAX_SECONDS),
                '--setup-deadline-unix-ms',
                str(self.setup_ms if setup_ms is None else setup_ms),
                '--hard-deadline-unix-ms',
                str(self.hard_ms if hard_ms is None else hard_ms),
                '--bootstrap-expires-at',
                str(self.expires_at if expires is None else expires),
                '--wait-seconds', str(120 if wait is None else wait)]
        if allow_http:
            argv.append('--allow-loopback-http')
        return argv

    def run_gb(self, fd, **kw):
        return gb.run(self.argv(fd, **kw))

    def main_gb(self, fd, **kw):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = gb.main(self.argv(fd, **kw))
        return rc, out.getvalue(), err.getvalue()

    def wait_file(self, path, timeout=10.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if os.path.exists(path):
                return True
            time.sleep(0.02)
        return False

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
            if not self.wait_file(path, timeout=15):
                return
            os.kill(os.getpid(), signum)
        thread = threading.Thread(target=watch, daemon=True)
        thread.start()
        return thread

    def admission_dir_real(self):
        return os.path.realpath(self.admission_dir)


class BootstrapExchange(Fixture):
    def test_pending_then_success_runs_admitted_job(self):
        self.assertGreater(len(JIT_B64), 16 * 1024)
        self.assertLess(len(JIT_B64), 64 * 1024)
        self.bootstrap_queue = [
            (202, b'{"pending":true}', {}),
            (200, self.response_bytes(), {}),
        ]
        self.write_script('run.sh', self.admit_script())
        fd = self.token_fd()
        start = time.monotonic()
        rc = self.run_gb(fd)
        elapsed = time.monotonic() - start
        self.assertEqual(rc, 0)
        self.assertGreaterEqual(elapsed, 0.9)  # one bounded 1s pending retry
        self.assertEqual(len(self.hits['bootstrap']), 2)
        for hit in self.hits['bootstrap']:
            self.assertEqual(hit['body'], b'{"version":1}')
            self.assertEqual(hit['authorization'], 'Bearer ' + BOOTSTRAP_TOKEN)
        self.assertEqual(len(self.hits['admission']), 1)
        ahit = self.hits['admission'][0]
        self.assertEqual(ahit['path'], '/v1/ci/runner-admissions')
        self.assertEqual(ahit['authorization'], 'Bearer ' + ADMISSION_TOKEN)
        self.assertNotIn(BOOTSTRAP_TOKEN, ahit['body'].decode())
        self.assertNotIn(ADMISSION_TOKEN, ahit['body'].decode())
        req = json.loads(ahit['body'])
        self.assertEqual(req['admissionId'], ADMISSION_ID)
        self.assertEqual(req['githubRunnerId'], RUNNER_ID)
        self.assertEqual(req['githubRepositoryId'], REPO_ID)
        self.assertEqual(req['runnerName'], RUNNER_NAME)
        self.assertEqual(Path(self.runner_dir, 'marker').read_text().strip(),
                         'ran')
        directory = self.admission_dir_real()
        for name in ('config.json', 'request.json', 'receipt.json',
                     'job-started.sh'):
            path = os.path.join(directory, name)
            if os.path.exists(path):
                content = Path(path).read_bytes()
                self.assertNotIn(BOOTSTRAP_TOKEN.encode(), content, name)
                self.assertNotIn(ADMISSION_TOKEN.encode(), content, name)
        self.assertFalse(os.path.exists(os.path.join(directory, 'denied.json')))
        env_text = Path(self.runner_dir, 'child.env').read_text()
        self.assertNotIn(BOOTSTRAP_TOKEN, env_text)
        self.assertNotIn(ADMISSION_TOKEN, env_text)
        self.assertIn(JIT_B64, env_text)  # JIT env is the intended carrier
        with self.assertRaises(OSError):
            os.fstat(fd)

    def test_slow_legitimate_bootstrap_response_is_tolerated(self):
        # The control plane performs a real one-shot GitHub register/JIT
        # exchange before its 200; a healthy slow response inside the
        # absolute wait budget must not be cut by a short socket timeout.
        self.bootstrap_gate = threading.Event()
        release = threading.Timer(6.0, self.bootstrap_gate.set)
        release.start()
        self.addCleanup(release.cancel)
        self.write_script('run.sh', self.admit_script())
        fd = self.token_fd()
        now = time.time()
        self.setup_ms = int((now + 600) * 1000)
        self.hard_ms = int((now + 3600) * 1000)
        start = time.monotonic()
        rc = self.run_gb(fd, wait=10, expires=int(now) + 500)
        elapsed = time.monotonic() - start
        self.assertEqual(rc, 0)
        self.assertGreaterEqual(elapsed, 5.9)
        self.assertEqual(len(self.hits['bootstrap']), 1)
        self.assertEqual(len(self.hits['admission']), 1)
        self.assertTrue(Path(self.runner_dir, 'marker').exists())

    def test_response_over_limit_fails_before_spawn(self):
        mutate = lambda obj: obj.update(encodedJitConfig='A' * 140000)
        self.bootstrap_queue = [(200, self.response_bytes(mutate), {})]
        self.write_script('run.sh', self.admit_script())
        fd = self.token_fd()
        with self.assertRaises(gb.BootstrapError):
            self.run_gb(fd)
        self.assertEqual(len(self.hits['bootstrap']), 1)
        self.assertEqual(len(self.hits['admission']), 0)
        self.assertFalse(os.path.exists(self.admission_dir))
        self.assertFalse(Path(self.runner_dir, 'marker').exists())
        with self.assertRaises(OSError):
            os.fstat(fd)

    def test_invalid_200_fields_fail_before_runner(self):
        cases = {
            'operationId': lambda o: o.update(operationId='other-op'),
            'admissionId': lambda o: o.update(admissionId='ef' * 32),
            'runnerName': lambda o: o.update(runnerName='other-name'),
            'repoId': lambda o: o.update(githubRepositoryId=555),
            'maxSeconds': lambda o: o.update(maximumSeconds=MAX_SECONDS + 1),
            'setupMs': lambda o: o.update(setupDeadlineUnixMs=self.setup_ms + 1),
            'hardMs': lambda o: o.update(hardDeadlineUnixMs=self.hard_ms + 1),
            'rid_negative': lambda o: o.update(githubRunnerId=-1),
            'rid_bool': lambda o: o.update(githubRunnerId=True),
            'rid_zero': lambda o: o.update(githubRunnerId=0),
            'rid_overflow': lambda o: o.update(githubRunnerId=2 ** 63),
            'version_true': lambda o: o.update(version=True),
            'version_2': lambda o: o.update(version=2),
            'missing_field': lambda o: o.pop('runnerName'),
            'extra_field': lambda o: o.update(extra=1),
            'null_field': lambda o: o.update(operationId=None),
            'bad_jit': lambda o: o.update(encodedJitConfig='!!!not-base64!!!'),
            'jit_too_long': lambda o: o.update(encodedJitConfig='A' * 70000),
            'jit_nonascii': lambda o: o.update(encodedJitConfig='\u00e9' * 100),
            'bad_cap': lambda o: o.update(admissionCapability='not-a-capability'),
            'cap_space': lambda o: o.update(admissionCapability=ADMISSION_TOKEN + ' '),
            'cap_long': lambda o: o.update(
                admissionCapability='v1.' + 'a' * 5000 + '.' + 'b' * 40),
            'cap_no_prefix': lambda o: o.update(admissionCapability='x' * 100),
        }
        raw_cases = {
            'dup_key': None,   # built below
            'nan': None,
            'trailing': None,
            'not_json': b'this is not json',
            'list': b'[1, 2, 3]',
        }
        good = self.response_bytes()
        raw_cases['dup_key'] = good.replace(
            b'"version":1', b'"version":1,"version":1', 1)
        raw_cases['nan'] = good.replace(b'"version":1', b'"version":NaN', 1)
        raw_cases['trailing'] = good + b' {}'
        for label, mutate in cases.items():
            with self.subTest(case=label):
                self.fresh_dirs('bad-' + label)
                self.write_script('run.sh', self.admit_script())
                self.bootstrap_queue = [(200, self.response_bytes(mutate), {})]
                fd = self.token_fd()
                rc, out, err = self.main_gb(fd)
                self.assertEqual(rc, 1, label)
                self.assertEqual(len(self.hits['admission']), 0, label)
                self.assertFalse(Path(self.runner_dir, 'marker').exists(), label)
                self.assertFalse(os.path.exists(self.admission_dir), label)
                blob = out + err
                self.assertNotIn(BOOTSTRAP_TOKEN, blob)
                self.assertNotIn(ADMISSION_TOKEN, blob)
                with self.assertRaises(OSError):
                    os.fstat(fd)
        for label, raw in raw_cases.items():
            with self.subTest(case='raw_' + label):
                self.fresh_dirs('raw-' + label)
                self.write_script('run.sh', self.admit_script())
                self.bootstrap_raw = raw
                fd = self.token_fd()
                rc, out, err = self.main_gb(fd)
                self.assertEqual(rc, 1, label)
                self.assertEqual(len(self.hits['admission']), 0, label)
                self.assertFalse(Path(self.runner_dir, 'marker').exists(), label)
                self.assertFalse(os.path.exists(self.admission_dir), label)

    def test_terminal_failures_never_retry(self):
        redirect_target = []

        class Target(BaseHTTPRequestHandler):
            def do_POST(self):
                redirect_target.append(
                    self.headers.get('Authorization'))
                self.send_response(200)
                self.send_header('Content-Length', '2')
                self.end_headers()
                try:
                    self.wfile.write(b'{}')
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *args):
                pass

        target = ThreadingHTTPServer(('127.0.0.1', 0), Target)
        threading.Thread(target=target.serve_forever, daemon=True).start()
        self.addCleanup(target.shutdown)
        self.addCleanup(target.server_close)
        target_url = 'http://127.0.0.1:%d/elsewhere' % target.server_port
        cases = [
            ('status_503', [(503, b'{"error":"x"}', {})]),
            ('status_401', [(401, b'{}', {})]),
            ('status_409', [(409, b'{}', {})]),
            ('status_500', [(500, b'{}', {})]),
            ('redirect_302', [(302, b'', {'Location': target_url})]),
            ('invalid_202_false', [(202, b'{"pending":false}', {})]),
            ('invalid_202_extra', [(202, b'{"pending":true,"x":1}', {})]),
            ('invalid_202_dup',
             [(202, b'{"pending":true,"pending":true}', {})]),
            ('invalid_202_nonjson', [(202, b'not json', {})]),
        ]
        for label, queue in cases:
            with self.subTest(case=label):
                self.fresh_dirs('term-' + label)
                self.write_script('run.sh', self.admit_script())
                self.bootstrap_queue = list(queue)
                fd = self.token_fd()
                with self.assertRaises(
                        (gb.BootstrapError, gb.bridge.BridgeError)):
                    self.run_gb(fd)
                self.assertEqual(len(self.hits['bootstrap']), 1, label)
                self.assertEqual(len(self.hits['admission']), 0, label)
                with self.assertRaises(OSError):
                    os.fstat(fd)
        self.assertEqual(redirect_target, [])

    def test_network_error_and_proxies(self):
        fd = self.token_fd()
        dead = 'http://127.0.0.1:1/v1/ci/runner-bootstrap'
        with self.assertRaises(gb.BootstrapError):
            self.run_gb(fd, endpoint=dead)
        self.assertEqual(len(self.hits['bootstrap']), 0)
        # Environment proxies must never reroute the credential.
        self.write_script('run.sh', self.admit_script())
        env = {'http_proxy': 'http://127.0.0.1:9',
               'https_proxy': 'http://127.0.0.1:9',
               'HTTP_PROXY': 'http://127.0.0.1:9',
               'HTTPS_PROXY': 'http://127.0.0.1:9',
               'no_proxy': '', 'NO_PROXY': ''}
        with mock.patch.dict(os.environ, env):
            rc = self.run_gb(self.token_fd())
        self.assertEqual(rc, 0)
        self.assertEqual(len(self.hits['bootstrap']), 1)
        self.assertEqual(len(self.hits['admission']), 1)

    def test_endpoint_validation_never_connects(self):
        port = self.server.server_port
        bad = [
            'http://example.com/v1/ci/runner-bootstrap',
            'https://example.com/v1/ci/other',
            'http://127.0.0.1:%d/v1/ci/other' % port,
            'http://127.0.0.1:%d/v1/ci/runner-bootstrap/' % port,
            self.endpoint + '?',
            self.endpoint + '?x=1',
            self.endpoint + '#',
            self.endpoint + '#frag',
            'http://user@127.0.0.1:%d/v1/ci/runner-bootstrap' % port,
            'http://user:pw@127.0.0.1:%d/v1/ci/runner-bootstrap' % port,
            'ftp://127.0.0.1:%d/v1/ci/runner-bootstrap' % port,
            'http://127.0.0.1:%d:0/v1/ci/runner-bootstrap' % port,
        ]
        for endpoint in bad:
            with self.subTest(endpoint=endpoint):
                fd = self.token_fd()
                with self.assertRaises(
                        (gb.BootstrapError, gb.bridge.BridgeError)):
                    self.run_gb(fd, endpoint=endpoint)
                with self.assertRaises(OSError):
                    os.fstat(fd)
        # Plain http to loopback still requires the explicit flag.
        fd = self.token_fd()
        with self.assertRaises(gb.bridge.BridgeError):
            self.run_gb(fd, allow_http=False)
        with self.assertRaises(OSError):
            os.fstat(fd)
        self.assertEqual(len(self.hits['bootstrap']), 0)
        self.assertEqual(len(self.hits['admission']), 0)

    def test_host_and_directory_validation(self):
        with mock.patch.object(legacy, 'check_host',
                               side_effect=RuntimeError('host denied')):
            fd = self.token_fd()
            with self.assertRaises(RuntimeError):
                self.run_gb(fd)
            self.assertEqual(len(self.hits['bootstrap']), 0)
            with self.assertRaises(OSError):
                os.fstat(fd)

        def env_check(env):
            if env.get('RUNNER_ALLOW_RUNASROOT'):
                raise RuntimeError('root runner refused')
        with mock.patch.object(legacy, 'check_host', side_effect=env_check):
            with mock.patch.dict(os.environ, {'RUNNER_ALLOW_RUNASROOT': '1'}):
                fd = self.token_fd()
                with self.assertRaises(RuntimeError):
                    self.run_gb(fd)
                self.assertEqual(len(self.hits['bootstrap']), 0)

        self.fresh_dirs('hostok')
        Path(self.runner_dir, '.runner').write_text('mimic')
        fd = self.token_fd()
        with self.assertRaises(RuntimeError):
            self.run_gb(fd)
        self.assertEqual(len(self.hits['bootstrap']), 0)
        with self.assertRaises(OSError):
            os.fstat(fd)

        self.fresh_dirs('dirs')
        os.mkdir(self.admission_dir, 0o700)  # must not already exist
        fd = self.token_fd()
        with self.assertRaises(gb.BootstrapError):
            self.run_gb(fd)
        self.assertEqual(len(self.hits['bootstrap']), 0)
        with self.assertRaises(OSError):
            os.fstat(fd)

        self.fresh_dirs('badparent')
        bad_parent = os.path.join(os.path.realpath(self.tmp.name), 'open-parent')
        os.mkdir(bad_parent, 0o755)
        os.chmod(bad_parent, 0o777)
        self.admission_dir = os.path.join(bad_parent, 'adm')
        fd = self.token_fd()
        with self.assertRaises(gb.BootstrapError):
            self.run_gb(fd)
        self.assertEqual(len(self.hits['bootstrap']), 0)
        with self.assertRaises(OSError):
            os.fstat(fd)

    def test_argument_validation(self):
        fd = self.token_fd()
        base = self.argv(fd)
        replacements = [
            ('--operation-id', 'bad op!'),
            ('--operation-id', 'x' * 129),
            ('--admission-id', 'AB' * 32),
            ('--admission-id', 'ab' * 31),
            ('--name', 'bad name'),
            ('--github-repository-id', '0'),
            ('--github-repository-id', '-5'),
            ('--github-repository-id', 'abc'),
            ('--github-repository-id', '9223372036854775808'),
            ('--maximum-seconds', '0'),
            ('--maximum-seconds', '21601'),
            ('--setup-deadline-unix-ms', '0'),
            ('--hard-deadline-unix-ms', '-9'),
            ('--bootstrap-expires-at', '0'),
            ('--wait-seconds', '0'),
            ('--wait-seconds', '121'),
        ]
        for flag, value in replacements:
            with self.subTest(flag=flag, value=value[:20]):
                argv = list(base)
                argv[argv.index(flag) + 1] = value
                self.assertEqual(gb.main(argv), 2)
        with self.assertRaises(SystemExit):
            gb.parse_args(self.argv(2))
        # Secret-looking values are never echoed by the parser.
        marker = 'SECRETMARKER-9f8e7d'
        buf = io.StringIO()
        argv = list(base)
        argv[argv.index('--operation-id') + 1] = 'bad ' + marker
        with mock.patch.object(sys, 'stderr', buf):
            with self.assertRaises(SystemExit):
                gb.parse_args(argv)
        self.assertNotIn(marker, buf.getvalue())
        # Inconsistent deadlines fail before any network use.
        inconsistent = [
            {'hard_ms': self.setup_ms},
            {'setup_ms': int(time.time() * 1000) - 1000},
            {'expires': int(time.time()) + 901},
            {'expires': self.setup_ms // 1000 + 1},
            {'expires': int(time.time()) - 5},
        ]
        for kw in inconsistent:
            with self.subTest(**{k: str(v)[:20] for k, v in kw.items()}):
                fdf = self.token_fd()
                rc, _, _ = self.main_gb(fdf, **kw)
                self.assertEqual(rc, 1)
                self.assertEqual(len(self.hits['bootstrap']), 0)
                with self.assertRaises(OSError):
                    os.fstat(fdf)

    def test_token_fd_no_eof_is_bounded(self):
        read_fd, write_fd = os.pipe()  # no data, writer stays open
        self.addCleanup(self._close_quiet, read_fd)
        self.addCleanup(self._close_quiet, write_fd)
        start = time.monotonic()
        rc, _, _ = self.main_gb(read_fd, wait=1)
        self.assertLess(time.monotonic() - start, 2.5)
        self.assertIn(rc, (1, 124))
        self.assertEqual(len(self.hits['bootstrap']), 0)
        with self.assertRaises(OSError):
            os.fstat(read_fd)
        self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))

    def test_drip_response_bounded_by_deadline(self):
        self.bootstrap_raw = self.response_bytes()
        self.drip = True
        fd = self.token_fd()
        start = time.monotonic()
        rc, _, _ = self.main_gb(fd, wait=1)
        self.assertLess(time.monotonic() - start, 2.5)
        self.assertIn(rc, (1, 124))
        self.assertEqual(len(self.hits['bootstrap']), 1)
        self.assertEqual(len(self.hits['admission']), 0)
        with self.assertRaises(OSError):
            os.fstat(fd)


class BootstrapSupervision(Fixture):
    def test_no_user_code_until_approval(self):
        self.write_script('run.sh', self.admit_script())
        self.admission_seen = threading.Event()
        self.admission_gate = threading.Event()
        checks = {}
        marker = os.path.join(self.runner_dir, 'marker')
        receipt = os.path.join(self.admission_dir_real(), 'receipt.json')

        def watcher():
            checks['seen'] = self.admission_seen.wait(15)
            checks['request'] = self.wait_file(
                os.path.join(self.admission_dir_real(), 'request.json'), 5)
            checks['marker_absent'] = not os.path.exists(marker)
            checks['receipt_absent'] = not os.path.exists(receipt)
            time.sleep(0.5)
            self.admission_gate.set()

        thread = threading.Thread(target=watcher, daemon=True)
        fd = self.token_fd()
        thread.start()
        rc = self.run_gb(fd)
        thread.join(20)
        self.assertTrue(checks.get('seen'))
        self.assertTrue(checks.get('request'))
        self.assertTrue(checks.get('marker_absent'))
        self.assertTrue(checks.get('receipt_absent'))
        self.assertEqual(rc, 0)
        self.assertTrue(os.path.exists(marker))

    def test_deny_receipt_blocks_job_and_reaps_group(self):
        self.write_script('run.sh', self.hook_then_wait())

        def deny(body):
            req = json.loads(body)
            return 403, json.dumps(dict(req, decision='deny'),
                                   separators=(',', ':')).encode()

        self.admission_handler = deny
        fd = self.token_fd()
        start = time.monotonic()
        rc = self.run_gb(fd)
        self.assertLess(time.monotonic() - start, 20)
        self.assertEqual(rc, jit.DENIED_EXIT)
        self.assertFalse(Path(self.runner_dir, 'marker').exists())
        # The supervisor can observe the deny receipt and reap before the
        # hook writes its own denied.json marker, so the published deny
        # receipt is the deterministic evidence.
        receipt = os.path.join(self.admission_dir_real(), 'receipt.json')
        self.assertTrue(os.path.exists(receipt))
        self.assertEqual(json.loads(Path(receipt).read_text())['decision'],
                         'deny')
        pid = int(Path(self.runner_dir, 'sleep.pid').read_text().strip())
        self.assertTrue(self.wait_dead(pid))

    def test_wrong_id_allow_never_runs_marker(self):
        self.write_script('run.sh', self.admit_script())

        def wrong(body):
            req = json.loads(body)
            rec = dict(req, decision='allow', githubRunnerId=RUNNER_ID + 1,
                       executionDeadlineUnixMs=int(time.time() * 1000) + 60000)
            return 200, json.dumps(rec, separators=(',', ':')).encode()

        self.admission_handler = wrong
        fd = self.token_fd()
        rc = self.run_gb(fd)
        self.assertEqual(rc, jit.DENIED_EXIT)
        self.assertFalse(Path(self.runner_dir, 'marker').exists())
        receipt = os.path.join(self.admission_dir_real(), 'receipt.json')
        self.assertTrue(os.path.exists(receipt))
        self.assertEqual(json.loads(Path(receipt).read_text())['decision'],
                         'deny')

    def test_setup_expiry_before_request_denies_fast(self):
        self.write_script('run.sh', 'sleep 300\n')  # never calls the hook
        now = time.time()
        self.setup_ms = int((now + 4) * 1000)  # response must echo argv times
        fd = self.token_fd()
        start = time.monotonic()
        rc = self.run_gb(fd, setup_ms=self.setup_ms,
                         expires=int(now) + 3)
        elapsed = time.monotonic() - start
        self.assertEqual(rc, jit.DENIED_EXIT)
        self.assertLess(elapsed, 20)
        denied = os.path.join(self.admission_dir_real(), 'denied.json')
        self.assertTrue(os.path.exists(denied))
        self.assertEqual(json.loads(Path(denied).read_text())['reason'],
                         'bootstrap_bridge_failed')
        self.assertFalse(Path(self.runner_dir, 'marker').exists())

    def test_job_continues_past_setup_after_allow(self):
        self.receipt_ttl_ms = 10000
        self.write_script('run.sh', self.admit_script('sleep 5'))
        now = time.time()
        self.setup_ms = int((now + 4) * 1000)
        fd = self.token_fd()
        start = time.monotonic()
        rc = self.run_gb(fd, setup_ms=self.setup_ms,
                         expires=int(now) + 3)
        elapsed = time.monotonic() - start
        self.assertEqual(rc, 0)
        self.assertGreaterEqual(elapsed, 4.5)  # outlived the 4s setup window
        self.assertTrue(Path(self.runner_dir, 'marker').exists())

    def test_execution_deadline_stops_group(self):
        self.receipt_ttl_ms = 1000
        self.write_script('run.sh', self.hook_then_wait())
        fd = self.token_fd()
        start = time.monotonic()
        rc = self.run_gb(fd)
        self.assertLess(time.monotonic() - start, 20)
        self.assertEqual(rc, jit.TIMEOUT_EXIT)
        for name in ('run.pid', 'sleep.pid'):
            pid = int(Path(self.runner_dir, name).read_text().strip())
            self.assertTrue(self.wait_dead(pid), name + ' survived deadline')
        self.assertFalse(Path(self.runner_dir, 'marker').exists())

    def test_sigterm_returns_143_and_reaps(self):
        self.write_script('run.sh', self.hook_then_wait())
        thread = self.signal_when_ready(
            os.path.join(self.runner_dir, 'ready'), signal.SIGTERM)
        fd = self.token_fd()
        try:
            rc = self.run_gb(fd)
        finally:
            thread.join(timeout=10)
        self.assertEqual(rc, 128 + signal.SIGTERM)
        for name in ('run.pid', 'sleep.pid'):
            pid = int(Path(self.runner_dir, name).read_text().strip())
            self.assertTrue(self.wait_dead(pid), name + ' survived SIGTERM')
        self.assertLessEqual(len(self.hits['admission']), 1)

    def test_tilde_admission_dir_resolves_canonical(self):
        home = os.path.join(os.path.realpath(self.tmp.name), 'home')
        os.mkdir(home, 0o700)
        self.write_script('run.sh', self.admit_script())
        fd = self.token_fd()
        argv = self.argv(fd)
        argv[argv.index('--admission-dir') + 1] = '~/adm-tilde'
        with mock.patch.dict(os.environ, {'HOME': home}):
            rc = gb.run(argv)
        self.assertEqual(rc, 0)
        created = os.path.join(home, 'adm-tilde')
        self.assertTrue(os.path.isdir(created))
        self.assertTrue(os.path.exists(
            os.path.join(created, 'request.json')))
        self.assertTrue(os.path.exists(
            os.path.join(created, 'receipt.json')))
        self.assertTrue(Path(self.runner_dir, 'marker').exists())

    def test_published_receipt_retires_live_bridge_after_setup_expiry(self):
        # A bridge whose valid allow receipt is already committed has
        # finished its authority work. When the worker only observes that
        # receipt after both the setup and capability deadlines have
        # passed, it must retire the process without publishing a denial
        # for an already-admitted job.
        directory = jit._prepare_admission(self.admission_dir)
        base = time.time()
        fakewall = [base]
        args = SimpleNamespace(
            admission_dir=directory,
            setup_deadline_unix_ms=int((base + 3) * 1000),
            bootstrap_expires_at=int(base) + 2,
            admission_id=ADMISSION_ID,
            github_runner_id=RUNNER_ID,
            github_repository_id=REPO_ID,
            name=RUNNER_NAME,
            allow_loopback_http=True)
        jit._write_admission_files(directory, args)
        config, _ = jit._load_config(os.path.join(directory, jit.FILE_CONFIG))
        env = dict(part.split('=', 1) for part in HOOK_ENV.split())
        request = jit._context_request(config, env)
        payload = json.dumps(request, separators=(',', ':')).encode() + b'\n'
        jit._publish_exclusive(directory, jit.FILE_REQUEST, payload, 0o600)
        shared = {'stop': threading.Event(), 'proc': None, 'lock': threading.Lock()}
        response = {'runnerId': RUNNER_ID, 'capability': ADMISSION_TOKEN}
        created = []
        real_popen = gb.subprocess.Popen

        def spy(*spy_args, **spy_kwargs):
            proc = real_popen([sys.executable, '-I', '-c',
                               'import time; time.sleep(30)'],
                              stdin=gb.subprocess.DEVNULL,
                              stdout=gb.subprocess.DEVNULL,
                              stderr=gb.subprocess.DEVNULL,
                              close_fds=True)
            created.append(proc)
            # The receipt commits while the wall clock is still before both
            # deadlines; the worker then wakes to a clock already past setup
            # and capability expiry, the late-preemption window itself.
            receipt = dict(request, decision='allow',
                           executionDeadlineUnixMs=int((base + 10) * 1000))
            raw = json.dumps(receipt, separators=(',', ':')).encode() + b'\n'
            jit._publish_exclusive(directory, jit.FILE_RECEIPT, raw, 0o600)
            fakewall[0] = base + 4
            return proc

        admission_url = self.endpoint.replace('/runner-bootstrap',
                                              '/runner-admissions')
        try:
            with mock.patch.object(gb.time, 'time',
                                   side_effect=lambda: fakewall[0]), \
                    mock.patch.object(gb.subprocess, 'Popen',
                                      side_effect=spy), \
                    mock.patch.object(gb, '_publish_bridge_denial',
                                      wraps=gb._publish_bridge_denial) as deny:
                gb._bridge_worker(args, response, admission_url, shared)
            self.assertEqual(len(created), 1)
            self.assertGreater(fakewall[0], base + 3)
            self.assertGreater(fakewall[0], int(base) + 2)
            self.assertTrue(gb._receipt_satisfies(directory, request))
            self.assertTrue(os.path.exists(
                os.path.join(directory, jit.FILE_RECEIPT)))
            self.assertFalse(os.path.exists(
                os.path.join(directory, jit.FILE_DENIED)))
            deny.assert_not_called()
            self.assertFalse(shared['stop'].is_set())
            proc = created[0]
            self.assertIsNotNone(proc.poll())
            self.assertTrue(self.wait_dead(proc.pid),
                            'bridge process survived receipt retirement')
        finally:
            for proc in created:
                if proc.poll() is None:
                    proc.kill()
                proc.wait()


if __name__ == '__main__':
    unittest.main()

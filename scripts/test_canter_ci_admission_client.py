import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

spec = importlib.util.spec_from_file_location(
    'canter_ci_admission_client',
    Path(__file__).with_name('canter-ci-admission-client.py'))
client = importlib.util.module_from_spec(spec)
spec.loader.exec_module(client)
jit = client.jit
CLIENT_PATH = Path(__file__).with_name('canter-ci-admission-client.py').resolve()

ADMISSION_ID = 'ab' * 32
RUNNER_ID = 4242
REPO_ID = 987
RUNNER_NAME = 'ci-jit-1'
TOKEN = 'capability-token-0123456789abcdef'

CONFIG = {'schemaVersion': 1, 'admissionId': ADMISSION_ID,
          'githubRunnerId': RUNNER_ID, 'githubRepositoryId': REPO_ID,
          'runnerName': RUNNER_NAME}
REQUEST = dict(CONFIG, repository='octo-org/widgets', githubRunId=777,
               runAttempt=2, jobKey='build')


def write_private_json(directory, name, obj):
    data = json.dumps(obj, separators=(',', ':')).encode()
    fd = os.open(os.path.join(directory, name),
                 os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        os.write(fd, data)
    finally:
        os.close(fd)


def allow_receipt(now_ms=None, request=REQUEST):
    rec = dict(request, decision='allow',
               executionDeadlineUnixMs=(now_ms if now_ms is not None
                                         else int(time.time() * 1000) + 3600000))
    return rec


def deny_receipt(request=REQUEST):
    return dict(request, decision='deny')


class Fixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = os.path.join(os.path.realpath(self.tmp.name), 'admission')
        os.mkdir(self.directory, 0o700)
        write_private_json(self.directory, jit.FILE_CONFIG, CONFIG)
        write_private_json(self.directory, jit.FILE_REQUEST, REQUEST)
        self.hits = []
        self.responses = []
        self.drip = False
        self.raw_response = None
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get('Content-Length') or 0)
                body = self.rfile.read(min(length, 65536))
                fixture.hits.append({
                    'path': self.path,
                    'authorization': self.headers.get('Authorization'),
                    'body': body,
                })
                if fixture.responses:
                    spec = fixture.responses.pop(0)
                else:
                    spec = (200, json.dumps(
                        allow_receipt(), separators=(',', ':')).encode(), {})
                status, payload = spec[0], spec[1]
                extra = spec[2] if len(spec) > 2 else {}
                if fixture.raw_response is not None:
                    # Malformed status line: http.client raises BadStatusLine.
                    try:
                        self.wfile.write(fixture.raw_response)
                        self.wfile.flush()
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    return
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(payload)))
                for key, value in extra.items():
                    self.send_header(key, value)
                self.end_headers()
                if fixture.drip:
                    # Drip the body well below any per-read timeout so only
                    # the absolute deadline can bound the exchange.
                    for i in range(0, len(payload), 8):
                        try:
                            self.wfile.write(payload[i:i + 8])
                            self.wfile.flush()
                        except (BrokenPipeError, ConnectionResetError):
                            return
                        time.sleep(0.1)
                    return
                try:
                    self.wfile.write(payload)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.server.shutdown)
        self.addCleanup(self.server.server_close)
        self.endpoint = 'http://127.0.0.1:%d/v1/ci/runner-admissions' % self.server.server_port

    def token_fd(self, data=TOKEN):
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

    def argv(self, fd, wait='10', endpoint=None):
        return ['--endpoint', endpoint or self.endpoint,
                '--admission-dir', self.directory,
                '--token-fd', str(fd),
                '--wait-seconds', wait,
                '--allow-loopback-http']

    def run_client(self, fd, wait='10', endpoint=None):
        return client.run(self.argv(fd, wait, endpoint))

    def receipt_path(self):
        return os.path.join(self.directory, jit.FILE_RECEIPT)

    def read_receipt(self):
        with open(self.receipt_path(), 'rb') as stream:
            return json.loads(stream.read().decode())


class AllowCase(Fixture):
    def test_allow_publishes_receipt(self):
        fd = self.token_fd()
        self.assertEqual(self.run_client(fd), 0)
        receipt = self.read_receipt()
        self.assertEqual(receipt['decision'], 'allow')
        for key in jit.REQUEST_FIELDS:
            self.assertEqual(receipt[key], REQUEST[key])
        self.assertGreater(receipt['executionDeadlineUnixMs'],
                           int(time.time() * 1000))
        info = os.lstat(self.receipt_path())
        self.assertEqual(stat_mode(info), 0o600)
        # The credential traveled only in the Authorization header.
        self.assertEqual(len(self.hits), 1)
        self.assertEqual(self.hits[0]['authorization'], 'Bearer ' + TOKEN)
        self.assertNotIn(TOKEN, self.hits[0]['body'].decode())
        # The fd was closed and marked non-inheritable by the client.
        with self.assertRaises(OSError):
            os.fstat(fd)

    def test_pending_and_unavailable_retry_within_deadline(self):
        self.responses = [(202, b'{"pending":true}'),
                          (503, b'{"error":"unavailable"}'),
                          (200, json.dumps(allow_receipt(),
                                           separators=(',', ':')).encode())]
        fd = self.token_fd()
        self.assertEqual(self.run_client(fd), 0)
        self.assertEqual(len(self.hits), 3)
        for hit in self.hits:
            self.assertEqual(hit['authorization'], 'Bearer ' + TOKEN)
            self.assertEqual(json.loads(hit['body'].decode()), REQUEST)
        self.assertEqual(self.read_receipt()['decision'], 'allow')

    def test_deny_publishes_deny_without_deadline(self):
        self.responses = [(403, json.dumps(deny_receipt(),
                                           separators=(',', ':')).encode())]
        fd = self.token_fd()
        self.assertEqual(self.run_client(fd), 0)
        receipt = self.read_receipt()
        self.assertEqual(receipt['decision'], 'deny')
        self.assertNotIn('executionDeadlineUnixMs', receipt)
        self.assertEqual(set(receipt.keys()),
                         set(jit.REQUEST_FIELDS) | {'decision'})

    def test_env_proxies_are_ignored(self):
        env = {'http_proxy': 'http://127.0.0.1:9',
               'https_proxy': 'http://127.0.0.1:9',
               'HTTP_PROXY': 'http://127.0.0.1:9',
               'HTTPS_PROXY': 'http://127.0.0.1:9',
               'no_proxy': '', 'NO_PROXY': ''}
        fd = self.token_fd()
        with mock.patch.dict(os.environ, env):
            self.assertEqual(self.run_client(fd), 0)
        self.assertEqual(len(self.hits), 1)

    def test_subprocess_inherited_fd(self):
        fd = self.token_fd()
        proc = subprocess.run(
            [sys.executable, str(CLIENT_PATH)] + self.argv(fd),
            pass_fds=(fd,), capture_output=True, timeout=30)
        self.assertEqual(proc.returncode, 0, proc.stderr.decode())
        self.assertEqual(self.read_receipt()['decision'], 'allow')
        self.assertNotIn(TOKEN.encode(), proc.stdout + proc.stderr)


def stat_mode(info):
    return info.st_mode & 0o777


class RejectCase(Fixture):
    def assert_failed(self, fd, wait='10', endpoint=None):
        with self.assertRaises((client.BridgeError, jit.ProtocolError,
                                jit.ContextError, OSError, ValueError)):
            self.run_client(fd, wait, endpoint)
        self.assertFalse(os.path.exists(self.receipt_path()),
                         'a receipt must never be published on rejection')

    def test_wrong_identity_receipt_never_allows(self):
        bad = allow_receipt()
        bad['admissionId'] = 'cd' * 32
        self.responses = [(200, json.dumps(bad, separators=(',', ':')).encode())]
        self.assert_failed(self.token_fd())

    def test_wrong_deadline_receipt_never_allows(self):
        past = allow_receipt(now_ms=int(time.time() * 1000) - 60000)
        self.responses = [(200, json.dumps(past, separators=(',', ':')).encode())]
        self.assert_failed(self.token_fd())
        far = allow_receipt(now_ms=int(time.time() * 1000) + 86400000)
        self.responses = [(200, json.dumps(far, separators=(',', ':')).encode())]
        self.assert_failed(self.token_fd())

    def test_malformed_responses_never_allow(self):
        good = json.dumps(allow_receipt(), separators=(',', ':')).encode()
        dup = good.replace(b'"decision":"allow"',
                           b'"decision":"allow","decision":"allow"')
        cases = [
            b'{"decision":"allow",',
            dup,
            good + b' {}',
            json.dumps(allow_receipt() | {'extra': 1},
                       separators=(',', ':')).encode(),
            json.dumps(dict(REQUEST, decision='deny'),
                       separators=(',', ':')).encode(),  # 200 with deny
            b' ' * 20000,
            json.dumps({'decision': 'allow'}).encode(),
        ]
        for payload in cases:
            with self.subTest(payload=payload[:30]):
                self.responses = [(200, payload)]
                self.assert_failed(self.token_fd())
                # reset dir state between subtests
                try:
                    os.remove(self.receipt_path())
                except OSError:
                    pass

    def test_403_with_allow_shaped_receipt_never_allows(self):
        self.responses = [(403, json.dumps(allow_receipt(),
                                           separators=(',', ':')).encode())]
        self.assert_failed(self.token_fd())

    def test_unexpected_status_never_allows(self):
        self.responses = [(500, b'{"error":"x"}'), (302, b'')]
        self.assert_failed(self.token_fd())

    def test_redirect_target_never_sees_credential(self):
        hits = []

        class Target(BaseHTTPRequestHandler):
            def _record(self):
                hits.append((self.command,
                             self.headers.get('Authorization')))
                self.send_response(200)
                self.send_header('Content-Length', '2')
                self.end_headers()
                try:
                    self.wfile.write(b'{}')
                except (BrokenPipeError, ConnectionResetError):
                    pass

            do_GET = _record
            do_POST = _record

            def log_message(self, *args):
                pass

        target = ThreadingHTTPServer(('127.0.0.1', 0), Target)
        threading.Thread(target=target.serve_forever, daemon=True).start()
        self.addCleanup(target.shutdown)
        self.addCleanup(target.server_close)
        target_url = 'http://127.0.0.1:%d/elsewhere' % target.server_port
        # A real redirect with a Location header: without _NoRedirect the
        # credential would be forwarded to the target.
        self.responses = [(302, b'', {'Location': target_url})]
        self.assert_failed(self.token_fd())
        self.assertEqual(hits, [])

    def test_endpoint_validation(self):
        fd = self.token_fd()
        for endpoint in ('http://example.com/x',
                         'http://user@127.0.0.1:%d/x' % self.server.server_port,
                         self.endpoint + '?x=1',
                         self.endpoint + '#frag',
                         'ftp://127.0.0.1/x'):
            with self.subTest(endpoint=endpoint):
                with self.assertRaises(client.BridgeError):
                    self.run_client(fd, endpoint=endpoint)
        # Plain http to loopback still requires the explicit flag.
        argv = self.argv(fd)
        argv.remove('--allow-loopback-http')
        with self.assertRaises(client.BridgeError):
            client.run(argv)

    def test_request_wait_timeout_is_bounded(self):
        os.remove(os.path.join(self.directory, jit.FILE_REQUEST))
        start = time.monotonic()
        with self.assertRaises(client.BridgeError):
            self.run_client(self.token_fd(), wait='1')
        self.assertLess(time.monotonic() - start, 5)

    def test_token_read_timeout_is_bounded(self):
        read_fd, write_fd = os.pipe()  # no data, writer stays open
        self.addCleanup(self._close_quiet, read_fd)
        self.addCleanup(self._close_quiet, write_fd)
        start = time.monotonic()
        with self.assertRaises(client.BridgeError):
            self.run_client(read_fd, wait='1')
        self.assertLess(time.monotonic() - start, 5)

    def test_bad_token_fd_fails_closed(self):
        fd = os.open(os.devnull, os.O_RDONLY)
        os.close(fd)
        self.assertEqual(client.main(self.argv(fd)), 1)
        # If a deny was proven from the still-valid request it must be deny.
        if os.path.exists(self.receipt_path()):
            self.assertEqual(self.read_receipt()['decision'], 'deny')

    def test_missing_or_malformed_request_denies(self):
        # Valid request but terminal transport failure publishes matching deny.
        self.responses = [(500, b'{}')]
        fd = self.token_fd()
        self.assertEqual(client.main(self.argv(fd)), 1)
        receipt = self.read_receipt()
        self.assertEqual(receipt['decision'], 'deny')
        self.assertEqual(set(receipt.keys()),
                         set(jit.REQUEST_FIELDS) | {'decision'})
        # Missing request.json: nothing can be proven, still fails closed.
        os.remove(self.receipt_path())
        os.remove(os.path.join(self.directory, jit.FILE_REQUEST))
        fd2 = self.token_fd()
        self.assertEqual(client.main(self.argv(fd2, wait='1')), 1)

    def test_existing_receipt_is_never_replaced(self):
        sentinel = b'{"decision":"allow","sentinel":true}\n'
        fd0 = os.open(self.receipt_path(),
                      os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            os.write(fd0, sentinel)
        finally:
            os.close(fd0)
        self.assertEqual(client.main(self.argv(self.token_fd())), 1)
        with open(self.receipt_path(), 'rb') as stream:
            self.assertEqual(stream.read(), sentinel)

    def test_binding_mismatch_fails(self):
        os.remove(os.path.join(self.directory, jit.FILE_REQUEST))
        bad = dict(REQUEST, admissionId='ef' * 32)
        write_private_json(self.directory, jit.FILE_REQUEST, bad)
        self.assertEqual(client.main(self.argv(self.token_fd())), 1)

    def test_config_and_request_validation(self):
        os.remove(os.path.join(self.directory, jit.FILE_CONFIG))
        write_private_json(self.directory, jit.FILE_CONFIG,
                           dict(CONFIG, extra=1))
        self.assertEqual(client.main(self.argv(self.token_fd())), 1)
        # request.json with duplicate keys is not valid strict JSON.
        os.remove(os.path.join(self.directory, jit.FILE_REQUEST))
        raw = json.dumps(REQUEST, separators=(',', ':')).encode()
        dup = raw.replace(b'"jobKey":"build"',
                          b'"jobKey":"build","jobKey":"build"')
        fd0 = os.open(os.path.join(self.directory, jit.FILE_REQUEST),
                      os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            os.write(fd0, dup)
        finally:
            os.close(fd0)
        self.assertEqual(client.main(self.argv(self.token_fd())), 1)

    def test_slow_drip_body_hits_absolute_deadline(self):
        self.drip = True
        self.responses = [(200, json.dumps(
            allow_receipt(), separators=(',', ':')).encode(), {})]
        fd = self.token_fd()
        start = time.monotonic()
        with self.assertRaises(client.BridgeError):
            self.run_client(fd, wait='1')
        self.assertLess(time.monotonic() - start, 3)
        self.assertFalse(os.path.exists(self.receipt_path()))

    def test_subprocess_deadline_guard_bounds_drip(self):
        self.drip = True
        self.responses = [(200, json.dumps(
            allow_receipt(), separators=(',', ':')).encode(), {})]
        fd = self.token_fd()
        start = time.monotonic()
        proc = subprocess.run(
            [sys.executable, str(CLIENT_PATH)] + self.argv(fd, wait='1'),
            pass_fds=(fd,), capture_output=True, timeout=15)
        self.assertLess(time.monotonic() - start, 5)
        self.assertNotEqual(proc.returncode, 0)
        self.assertFalse(os.path.exists(self.receipt_path()))
        self.assertNotIn(TOKEN.encode(), proc.stdout + proc.stderr)

    def test_fifo_protocol_file_never_hangs(self):
        request_path = os.path.join(self.directory, jit.FILE_REQUEST)
        os.remove(request_path)
        os.mkfifo(request_path, 0o600)
        start = time.monotonic()
        self.assertEqual(client.main(self.argv(self.token_fd(), wait='5')), 1)
        self.assertLess(time.monotonic() - start, 3)

    def test_token_whitespace_is_rejected_not_stripped(self):
        fd = self.token_fd(data=TOKEN + '\n')
        with self.assertRaises(client.BridgeError):
            self.run_client(fd)

    def test_arguments_never_echo_secret_values(self):
        for extra in (['--token', TOKEN], ['--token-fd', TOKEN]):
            buf = io.StringIO()
            argv = ['--endpoint', self.endpoint,
                    '--admission-dir', self.directory,
                    '--token-fd', '3'] + extra
            with mock.patch.object(sys, 'stderr', buf):
                with self.assertRaises(SystemExit):
                    client.parse_args(argv)
            self.assertNotIn(TOKEN, buf.getvalue())
        # An endpoint carrying a secret-looking query is rejected with a
        # fixed message, never echoing the value.
        with self.assertRaises(client.BridgeError) as ctx:
            self.run_client(self.token_fd(),
                            endpoint=self.endpoint + '?q=' + TOKEN)
        self.assertNotIn(TOKEN, str(ctx.exception))

    def test_raw_invalid_status_line_never_leaks_token(self):
        marker = TOKEN
        self.raw_response = (marker + ' this is not a valid status line\r\n\r\n').encode()
        fd = self.token_fd()
        proc = subprocess.run(
            [sys.executable, str(CLIENT_PATH)] + self.argv(fd, wait='5'),
            pass_fds=(fd,), capture_output=True, timeout=15)
        self.assertEqual(proc.returncode, 1)
        self.assertNotIn(marker.encode(), proc.stdout + proc.stderr)
        if os.path.exists(self.receipt_path()):
            self.assertEqual(self.read_receipt()['decision'], 'deny')

    def test_deny_fallback_exception_never_leaks_or_raises(self):
        self.responses = [(500, b'{"error":"x"}')] * 40
        marker = 'FALLBACK-TOKEN-leakcheck'
        fd = self.token_fd()

        def boom(directory, request):
            raise client.BridgeDeadlineError(marker)

        out, err = io.StringIO(), io.StringIO()
        with mock.patch.object(client, '_publish_cached_deny', boom), \
                mock.patch.object(sys, 'stdout', out), \
                mock.patch.object(sys, 'stderr', err):
            rc = client.main(self.argv(fd))
        self.assertEqual(rc, 1)
        self.assertNotIn(marker, out.getvalue() + err.getvalue())
        self.assertFalse(os.path.exists(self.receipt_path()))

    def test_argument_validation(self):
        with self.assertRaises(SystemExit):
            client.parse_args(['--endpoint', self.endpoint,
                               '--admission-dir', self.directory,
                               '--token-fd', '2'])
        with self.assertRaises(SystemExit):
            client.parse_args(['--endpoint', self.endpoint,
                               '--admission-dir', self.directory,
                               '--token-fd', '3', '--wait-seconds', '0'])
        with self.assertRaises(SystemExit):
            client.parse_args(['--endpoint', self.endpoint,
                               '--admission-dir', self.directory,
                               '--token-fd', '3', '--wait-seconds', '31'])
        with self.assertRaises(SystemExit):
            client.parse_args(['--endpoint', self.endpoint,
                               '--admission-dir', self.directory,
                               '--token-fd', '3', '--token', TOKEN])


if __name__ == '__main__':
    unittest.main()

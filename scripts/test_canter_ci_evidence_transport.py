import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from contextlib import redirect_stdout
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

spec = importlib.util.spec_from_file_location(
    'canter_ci_evidence', Path(__file__).with_name('canter-ci-evidence.py'))
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)

ADMISSION_ID = 'cd' * 32
EVIDENCE_TOKEN = 'ce1.' + 'a' * 24 + '.' + 'b' * 43
FRAME = {'token': EVIDENCE_TOKEN, 'deadlineUnixMs': 4000000000000,
         'baseline': {'cpu_total_ticks': 100, 'cpu_busy_ticks': 20,
                      'memory_total_bytes': 1024,
                      'memory_available_bytes': 512,
                      'network_interface': 'ens3',
                      'network_rx_bytes': 5, 'network_tx_bytes': 7,
                      'oom_kills': 0},
         'baselineAt': '2026-12-01T00:00:00.000Z'}
EVIDENCE_PATH = '/v1/ci/runner-evidence'


def private_dir(root):
    path = os.path.join(os.path.realpath(root), 'admission')
    os.mkdir(path, 0o700)
    return path


def junit_file(root, name='report.xml', body=None):
    if body is None:
        body = ('<testsuite name="s"><testcase name="one" time="0.5"/>'
                '<testcase name="two"><failure>bad</failure></testcase>'
                '</testsuite>')
    path = Path(root) / name
    path.write_bytes(body.encode())
    return str(path)


class HttpFixture(unittest.TestCase):
    """Loopback-only HTTP server; records hits, replays scripted replies."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.hits = []
        self.queue = []
        self.default = (204, b'', None)
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get('Content-Length') or 0)
                body = self.rfile.read(min(length, 300000))
                fixture.hits.append({
                    'path': self.path,
                    'authorization': self.headers.get('Authorization'),
                    'body': body,
                })
                status, raw, headers = fixture.queue.pop(0) if fixture.queue \
                    else fixture.default
                self.send_response(status)
                for key, value in (headers or {}).items():
                    self.send_header(key, value)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                if raw:
                    self.wfile.write(raw)

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(
            target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.server.shutdown)
        self.addCleanup(self.server.server_close)

    def url(self, path=EVIDENCE_PATH):
        return 'http://127.0.0.1:%d%s' % (self.port, path)

    def queue_reply(self, status, raw=b'', headers=None):
        self.queue.append((status, raw, headers))


class PostCommandTests(HttpFixture):
    def _spawn_post(self, url, token=EVIDENCE_TOKEN, body=b'{"x":1}',
                    extra_fd=None):
        read_fd, write_fd = os.pipe()
        os.write(write_fd, token.encode())
        os.close(write_fd)
        argv = [sys.executable, '-I',
                str(Path(__file__).with_name('canter-ci-evidence.py')),
                'post', '--endpoint', url, '--token-fd', str(read_fd),
                '--allow-loopback-http']
        proc = subprocess.Popen(
            argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, pass_fds=(read_fd,), close_fds=True)
        os.close(read_fd)
        out, err = proc.communicate(input=body, timeout=30)
        return proc.returncode, out, err, self.hits

    def test_204_accepted_and_wire_shape(self):
        code, out, err, hits = self._spawn_post(self.url())
        self.assertEqual(code, 0)
        self.assertEqual(len(hits), 1)
        self.assertEqual(hits[0]['path'], EVIDENCE_PATH)
        self.assertEqual(hits[0]['authorization'], 'Bearer ' + EVIDENCE_TOKEN)
        self.assertEqual(hits[0]['body'], b'{"x":1}')
        self.assertNotIn(EVIDENCE_TOKEN.encode(), out + err)

    def test_terminal_statuses_exit_65(self):
        for status in (200, 202, 400, 401, 403, 409, 413, 429):
            self.queue_reply(status, b'{"error":"x"}')
            code, _, _, _ = self._spawn_post(self.url())
            self.assertEqual(code, 65, status)

    def test_retryable_statuses_exit_75(self):
        for status in (500, 503):
            self.queue_reply(status, b'{}')
            code, _, _, _ = self._spawn_post(self.url())
            self.assertEqual(code, 75, status)

    def test_network_failure_exit_75(self):
        code, _, _, _ = self._spawn_post('http://127.0.0.1:1' + EVIDENCE_PATH)
        self.assertEqual(code, 75)

    def test_wrong_path_exit_65_no_request(self):
        code, _, _, hits = self._spawn_post(self.url('/v1/other'))
        self.assertEqual(code, 65)
        self.assertEqual(hits, [])

    def test_redirect_never_forwards_token(self):
        self.queue_reply(302, b'', {'Location': 'http://127.0.0.1:9/evil'})
        code, _, _, hits = self._spawn_post(self.url())
        self.assertEqual(code, 65)
        self.assertEqual(len(hits), 1)
        self.assertNotIn(b'evil', hits[0]['path'].encode())

    def test_oversize_body_exit_65_no_request(self):
        code, _, _, hits = self._spawn_post(
            self.url(), body=b'x' * (evidence.REQUEST_MAX_BYTES + 1))
        self.assertEqual(code, 65)
        self.assertEqual(hits, [])

    def test_stalled_response_bounded_by_deadline(self):
        release = threading.Event()

        class Stall(BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get('Content-Length') or 0)
                self.rfile.read(length)
                release.wait(30)
                try:
                    self.send_response(204)
                    self.send_header('Content-Length', '0')
                    self.end_headers()
                except Exception:
                    pass

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(('127.0.0.1', 0), Stall)
        port = server.server_address[1]
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        self.addCleanup(server.server_close)
        self.addCleanup(release.set)
        start = time.monotonic()
        code, _, _, _ = self._spawn_post(
            'http://127.0.0.1:%d%s' % (port, EVIDENCE_PATH))
        elapsed = time.monotonic() - start
        self.assertEqual(code, 75)
        self.assertLess(elapsed, 12)
        self.assertGreaterEqual(elapsed, 4)

    def test_token_fd_closed_and_no_secret_in_output(self):
        code, out, err, _ = self._spawn_post(self.url())
        self.assertEqual(code, 0)
        self.assertNotIn(EVIDENCE_TOKEN.encode(), out + err)


class ReporterFixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = private_dir(self.tmp.name)
        self.stop = threading.Event()
        self.posts = []
        self.statuses = []
        self.addCleanup(self.stop.set)

    def make_reporter(self, frame=None, deadline_ms=None):
        frame = dict(frame or FRAME)
        if deadline_ms is not None:
            frame['deadlineUnixMs'] = deadline_ms
        return evidence.CIReporter(
            'http://127.0.0.1:9' + '/v1/ci/runner-admissions',
            frame, self.dir, self.stop,
            frame['deadlineUnixMs'] + 60000,
            allow_loopback_http=True)

    def sock_path(self):
        return os.path.join(self.dir, evidence.SOCKET_NAME)

    def send_frame(self, raw, timeout=10):
        path = self.sock_path()
        deadline = time.monotonic() + timeout
        while True:
            try:
                conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                conn.settimeout(5)
                conn.connect(path)
                break
            except OSError:
                conn.close()
                if time.monotonic() >= deadline or self.stop.is_set():
                    raise
                time.sleep(0.05)
        try:
            conn.sendall(struct.pack('>I', len(raw)) + raw)
            conn.settimeout(15)
            return conn.recv(1)
        finally:
            conn.close()

    def spawn_server(self, reporter):
        thread = threading.Thread(target=reporter.run, daemon=True)
        thread.start()
        deadline = time.monotonic() + 10
        while not os.path.lexists(self.sock_path()):
            if time.monotonic() >= deadline:
                self.fail('reporter socket did not appear')
            time.sleep(0.02)
        return thread


class FakeProc:
    def __init__(self, code=0):
        self.returncode = code
        self.pid = os.getpid()
        self.killed = False

    def communicate(self, input=None, timeout=None):
        if input is not None:
            self.sent = input
        return b'', b''

    def poll(self):
        return self.returncode

    def wait(self, timeout=None):
        return self.returncode


class ReporterUnitTests(ReporterFixture):
    def test_frame_decode_validation(self):
        good = json.dumps(FRAME).encode()
        self.assertEqual(evidence._decode_frame(good)['token'], EVIDENCE_TOKEN)
        for bad in (b'', b'x' * (evidence.FRAME_MAX_BYTES + 1),
                    b'not json', b'{"a":1}',
                    json.dumps(dict(FRAME, extra=1)).encode(),
                    json.dumps(dict(FRAME, deadlineUnixMs=True)).encode(),
                    json.dumps(dict(FRAME, deadlineUnixMs=-1)).encode(),
                    json.dumps(dict(FRAME, deadlineUnixMs='5')).encode(),
                    json.dumps(dict(FRAME, token='v1.a.b')).encode(),
                    json.dumps(dict(FRAME, token='ce1.a')).encode(),
                    json.dumps(dict(FRAME, baseline={'cpu_total_ticks': True})).encode(),
                    json.dumps(dict(FRAME, baseline={'cpu_total_ticks': -1})).encode(),
                    json.dumps(dict(FRAME, baseline={'cpu_total_ticks': evidence.MAX_COUNTER + 1})).encode(),
                    json.dumps(dict(FRAME, baseline={'network_interface': 'bad iface!'})).encode(),
                    json.dumps(dict(FRAME, baselineAt='not a time')).encode(),
                    json.dumps(dict(FRAME, baselineAt='2026-12-01 00:00:00')).encode()):
            with self.subTest(bad=bad[:60]):
                self.assertIsNone(evidence._decode_frame(bad))

    def test_socket_created_private_and_removed_on_close(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        info = os.lstat(self.sock_path())
        self.assertTrue(stat.S_ISSOCK(info.st_mode))
        self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
        self.assertEqual(info.st_uid, os.geteuid())
        reporter.close()
        thread.join(timeout=10)
        self.assertFalse(os.path.lexists(self.sock_path()))

    def test_preexisting_socket_path_fails_closed(self):
        victim = Path(self.sock_path())
        victim.write_bytes(b'keep')
        reporter = self.make_reporter()
        with mock.patch.object(evidence, 'iter_vm_metrics', return_value=iter(())):
            reporter.run()  # listener fails; metrics loop exits immediately
        self.assertTrue(victim.exists())
        self.assertEqual(victim.read_bytes(), b'keep')

    def test_symlink_socket_path_rejected(self):
        target = Path(self.dir) / 'target'
        target.write_bytes(b'x')
        os.symlink(target, self.sock_path())
        reporter = self.make_reporter()
        with mock.patch.object(evidence, 'iter_vm_metrics', return_value=iter(())):
            reporter.run()
        self.assertTrue(os.path.islink(self.sock_path()))
        self.assertEqual(target.read_bytes(), b'x')

    def test_uds_frame_roundtrip_204(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        raw = json.dumps({'schemaVersion': 1, 'batchId': 'a' * 64,
                          'metrics': [], 'tests': []}).encode()
        # Empty metrics+tests is a valid wire packet for transport tests.
        with mock.patch.object(reporter, '_spawn_post',
                               return_value=FakeProc(0)) as spawn:
            reply = self.send_frame(raw)
        self.assertEqual(reply, evidence.ACCEPTED)
        reporter.close()
        thread.join(timeout=10)

    def test_uds_status_mapping(self):
        for code, expected in ((0, evidence.ACCEPTED),
                               (65, evidence.REJECTED),
                               (75, evidence.RETRY),
                               (1, evidence.RETRY)):
            reporter = self.make_reporter()
            thread = self.spawn_server(reporter)
            raw = b'{"schemaVersion":1}'
            with mock.patch.object(reporter, '_spawn_post',
                                   return_value=FakeProc(code)):
                reply = self.send_frame(raw)
            self.assertEqual(reply, expected, code)
            reporter.close()
            thread.join(timeout=10)

    def test_uds_length_prefix_bounds(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        path = self.sock_path()
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(10)
        conn.connect(path)
        conn.sendall(struct.pack('>I', 0))
        conn.close()
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(10)
        conn.connect(path)
        conn.sendall(struct.pack('>I', evidence.REQUEST_MAX_BYTES + 1))
        conn.close()
        reporter.close()
        thread.join(timeout=10)

    def test_partial_and_dribbled_frame(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        raw = json.dumps({'schemaVersion': 1, 'batchId': 'b' * 64,
                          'metrics': [], 'tests': []}).encode()
        path = self.sock_path()
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(10)
        conn.connect(path)
        frame = struct.pack('>I', len(raw)) + raw
        conn.sendall(frame[:5])
        time.sleep(0.1)
        conn.sendall(frame[5:])
        with mock.patch.object(reporter, '_spawn_post',
                               return_value=FakeProc(0)):
            reply = conn.recv(1)
        self.assertEqual(reply, evidence.ACCEPTED)
        conn.close()
        reporter.close()
        thread.join(timeout=10)

    def test_post_mutex_busy_replies_retry(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        reporter._post_mutex.acquire()
        try:
            raw = b'{"schemaVersion":1}'
            conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            conn.settimeout(12)
            conn.connect(self.sock_path())
            conn.sendall(struct.pack('>I', len(raw)) + raw)
            reply = conn.recv(1)
            conn.close()
            self.assertEqual(reply, evidence.RETRY)
        finally:
            reporter._post_mutex.release()
        reporter.close()
        thread.join(timeout=10)

    def test_close_unblocks_dribbling_connection(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(10)
        conn.connect(self.sock_path())
        conn.sendall(struct.pack('>I', 100))
        conn.sendall(b'x' * 10)  # dribble, never completes
        time.sleep(0.1)
        start = time.monotonic()
        reporter.close()
        thread.join(timeout=10)
        self.assertLess(time.monotonic() - start, 8)
        conn.close()
        self.assertFalse(thread.is_alive())

    def test_stalled_reader_does_not_block_other_clients(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        release = threading.Event()
        self.addCleanup(release.set)
        first_read = threading.Event()
        seen = []
        original = reporter._recv_exact

        def observed(conn, count, deadline):
            if not seen:
                seen.append(conn)
                first_read.set()
                release.wait(20)
                raise evidence.EvidenceError('fixture reader released')
            return original(conn, count, deadline)

        stalled = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        stalled.settimeout(10)
        try:
            with mock.patch.object(reporter, '_recv_exact', observed), \
                    mock.patch.object(reporter, '_post_raw',
                                      return_value=evidence.ACCEPTED):
                stalled.connect(self.sock_path())
                self.assertTrue(first_read.wait(10))
                reply = self.send_frame(b'{"schemaVersion":1}')
                self.assertEqual(reply, evidence.ACCEPTED)
                self.assertFalse(release.is_set())
                with reporter._conns_lock:
                    self.assertIn(seen[0], reporter._conns)
        finally:
            release.set()
            stalled.close()
            reporter.close()
            thread.join(timeout=10)
        self.assertFalse(thread.is_alive())

    def test_worker_capacity_rejects_extra_clients_immediately(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        release = threading.Event()
        self.addCleanup(release.set)
        entered = threading.Event()
        active = [0]
        lock = threading.Lock()

        def observed(conn, count, deadline):
            with lock:
                active[0] += 1
                if active[0] == evidence.LOCAL_WORKERS:
                    entered.set()
            release.wait(20)
            raise evidence.EvidenceError('fixture reader released')

        stalled = []
        try:
            with mock.patch.object(reporter, '_recv_exact', observed):
                for _ in range(evidence.LOCAL_WORKERS):
                    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                    conn.settimeout(10)
                    conn.connect(self.sock_path())
                    stalled.append(conn)
                self.assertTrue(entered.wait(10))
                overflow = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                overflow.settimeout(10)
                overflow.connect(self.sock_path())
                self.assertEqual(overflow.recv(1), b'')
                overflow.close()
        finally:
            release.set()
            for conn in stalled:
                conn.close()
            reporter.close()
            thread.join(timeout=10)
        self.assertFalse(thread.is_alive())

    def test_stop_race_spawn_registration(self):
        reporter = self.make_reporter()
        reporter._closing.set()
        reporter._stop.set()
        proc = reporter._spawn_post(b'{}')
        self.assertIsNone(proc)

    def test_metric_wire_uses_root_generator_output(self):
        reporter = self.make_reporter()
        captured = []

        def fake_metrics(baseline, at, stop, deadline):
            yield {'at': 't1', 'cpuPercent': 42.0}
            yield {'at': 't2', 'memoryBytes': 7}

        def fake_await(proc, raw):
            captured.append(raw)
            return evidence.ACCEPTED

        with mock.patch.object(evidence, 'iter_vm_metrics', fake_metrics), \
                mock.patch.object(reporter, '_spawn_post',
                                  return_value=FakeProc(0)), \
                mock.patch.object(reporter, '_await_post', fake_await), \
                mock.patch.object(evidence, '_jit',
                                  side_effect=Exception('no listener')):
            reporter.run()
        self.assertEqual(len(captured), 2)
        first = json.loads(captured[0])
        self.assertEqual(first['metrics'], [{'at': 't1', 'cpuPercent': 42.0}])
        self.assertEqual(first['tests'], [])
        self.assertRegex(first['batchId'], r'^[0-9a-f]{64}$')
        second = json.loads(captured[1])
        self.assertEqual(second['metrics'], [{'at': 't2', 'memoryBytes': 7}])

    def test_metric_skips_when_mutex_busy(self):
        reporter = self.make_reporter()
        spawned = []
        reporter._post_mutex.acquire()
        try:
            with mock.patch.object(reporter, '_spawn_post',
                                   side_effect=lambda r: spawned.append(r)):
                reporter._post_metric(b'{}')
        finally:
            reporter._post_mutex.release()
        self.assertEqual(spawned, [])


class JUnitCliTests(ReporterFixture, HttpFixture):
    def setUp(self):
        HttpFixture.setUp(self)
        ReporterFixture.setUp(self)

    def test_empty_reports_message_and_no_http(self):
        empty = junit_file(self.tmp.name, 'empty.xml', '<testsuite/>')
        buf = io.StringIO()
        with redirect_stdout(buf):
            code = evidence._cmd_junit(
                [empty, '--admission-dir', self.dir])
        self.assertEqual(code, 0)
        self.assertEqual(buf.getvalue().strip(), 'No test cases reported.')
        self.assertEqual(self.hits, [])
        rows = evidence.read_junit_reports([empty])
        self.assertEqual(rows, [])

    def test_junit_end_to_end_accepted(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        report = junit_file(self.tmp.name, 'tests.xml')
        with mock.patch.object(reporter, '_spawn_post',
                               return_value=FakeProc(0)) as spawn:
            code = evidence._cmd_junit(
                [report, '--admission-dir', self.dir])
        self.assertEqual(code, 0)
        self.assertTrue(spawn.called)
        raw = spawn.call_args[0][0]
        batch = json.loads(raw)
        self.assertEqual(batch['schemaVersion'], 1)
        self.assertEqual(len(batch['tests']), 2)
        self.assertEqual(batch['tests'][0]['name'], 'one')
        self.assertEqual(batch['tests'][0]['durationMs'], 500)
        self.assertNotIn('durationMs', batch['tests'][1])
        reporter.close()
        thread.join(timeout=10)

    def test_junit_rejected_exits_1(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        report = junit_file(self.tmp.name, 'tests.xml')
        with mock.patch.object(reporter, '_spawn_post',
                               return_value=FakeProc(65)):
            code = evidence._cmd_junit(
                [report, '--admission-dir', self.dir])
        self.assertEqual(code, 1)
        reporter.close()
        thread.join(timeout=10)

    def test_junit_retry_preserves_batch_ids(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        report = junit_file(self.tmp.name, 'tests.xml')
        replies = [FakeProc(75), FakeProc(0)]
        seen = []

        def spawn_side(raw):
            seen.append(raw)
            return replies.pop(0) if replies else FakeProc(0)

        with mock.patch.object(reporter, '_spawn_post',
                               side_effect=spawn_side), \
                mock.patch.object(evidence, 'RETRY_SECONDS', 0.05):
            code = evidence._cmd_junit(
                [report, '--admission-dir', self.dir])
        self.assertEqual(code, 0)
        self.assertEqual(len(seen), 2)
        self.assertEqual(seen[0], seen[1])
        reporter.close()
        thread.join(timeout=10)

    def test_junit_packet_split_over_256k(self):
        big = '<testsuite>' + ''.join(
            '<testcase name="c%d" time="1"><failure>%s</failure></testcase>'
            % (i, 'x' * 4000) for i in range(70)) + '</testsuite>'
        report = junit_file(self.tmp.name, 'big.xml', big)
        rows = evidence.read_junit_reports([report])
        packets = evidence.make_test_batches(rows)
        self.assertGreater(len(packets), 1)
        for packet in packets:
            self.assertLessEqual(len(packet), evidence.REQUEST_MAX_BYTES)

    def test_junit_bad_file_fixed_error(self):
        bad = junit_file(self.tmp.name, 'bad.xml', 'not xml at all')
        code = evidence._cmd_junit([bad, '--admission-dir', self.dir])
        self.assertEqual(code, 1)

    def test_report_socket_metadata_rejected(self):
        # World-readable socket path must be refused by the CLI.
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        os.chmod(self.sock_path(), 0o666)
        report = junit_file(self.tmp.name, 'tests.xml')
        code = evidence._cmd_junit(
            [report, '--admission-dir', self.dir])
        self.assertEqual(code, 1)
        reporter.close()
        thread.join(timeout=10)




spec2 = importlib.util.spec_from_file_location(
    'canter_ci_admission_client',
    Path(__file__).with_name('canter-ci-admission-client.py'))
client = importlib.util.module_from_spec(spec2)
spec2.loader.exec_module(client)
jit = client.jit

AC_CONFIG = {'schemaVersion': 1, 'admissionId': ADMISSION_ID,
             'githubRunnerId': 4242, 'githubRepositoryId': 987,
             'runnerName': 'ci-jit-1'}
AC_REQUEST = dict(AC_CONFIG, repository='octo-org/widgets',
                  githubRunId=777, runAttempt=2, jobKey='build')
AC_TOKEN = 'capability-token-0123456789abcdef'


def write_json(directory, name, obj):
    data = json.dumps(obj, separators=(',', ':')).encode()
    fd = os.open(os.path.join(directory, name),
                 os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        os.write(fd, data)
    finally:
        os.close(fd)


def allow_receipt(deadline_ms):
    return dict(AC_REQUEST, decision='allow',
                executionDeadlineUnixMs=deadline_ms)


class AdmissionEvidenceTests(HttpFixture):
    def setUp(self):
        super().setUp()
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = private_dir(self.tmp.name)
        write_json(self.directory, jit.FILE_CONFIG, AC_CONFIG)
        write_json(self.directory, jit.FILE_REQUEST, AC_REQUEST)
        self.deadline_ms = int(time.time() * 1000) + 3600000

    def _pipes(self):
        token_r, token_w = os.pipe()
        ev_r, ev_w = os.pipe()
        os.write(token_w, AC_TOKEN.encode())
        os.close(token_w)
        return token_r, ev_r, ev_w

    def _invoke(self, with_evidence=True, responses=None):
        token_r, ev_r, ev_w = self._pipes()
        if responses:
            for item in responses:
                self.queue_reply(*item)
        deadline_ms = self.deadline_ms
        receipt = allow_receipt(deadline_ms)
        if responses is None:
            self.default = (200, json.dumps(receipt).encode(),
                            {'X-Canter-CI-Evidence': EVIDENCE_TOKEN})
        argv = ['--endpoint', self.url('/v1/ci/runner-admissions'),
                '--admission-dir', self.directory,
                '--token-fd', str(token_r), '--wait-seconds', '10',
                '--allow-loopback-http']
        if with_evidence:
            argv += ['--evidence-fd', str(ev_w)]
        code = client.run(argv)
        frame_raw = b''
        if with_evidence:
            os.set_blocking(ev_r, False)
            try:
                while True:
                    chunk = os.read(ev_r, 8192)
                    if not chunk:
                        break
                    frame_raw += chunk
            except (BlockingIOError, OSError):
                pass
            os.close(ev_r)
        receipt_path = os.path.join(self.directory, jit.FILE_RECEIPT)
        published = None
        if os.path.lexists(receipt_path):
            published = jit._read_private_json(receipt_path)
        return code, frame_raw, published

    def test_allow_with_header_writes_frame_before_receipt(self):
        seen = {}
        real_publish = client._publish_receipt

        def spy(directory, obj):
            # Drain the evidence pipe: the frame must already be there.
            found = b''
            for fd in getattr(self, '_ev_fds', []):
                try:
                    os.set_blocking(fd, False)
                    while True:
                        chunk = os.read(fd, 8192)
                        if not chunk:
                            break
                        found += chunk
                except (BlockingIOError, OSError):
                    pass
            seen['frame_at_publish'] = found
            return real_publish(directory, obj)

        token_r, ev_r, ev_w = self._pipes()
        self._ev_fds = [ev_r]
        self.default = (200, json.dumps(allow_receipt(self.deadline_ms)).encode(),
                        {'X-Canter-CI-Evidence': EVIDENCE_TOKEN})
        argv = ['--endpoint', self.url('/v1/ci/runner-admissions'),
                '--admission-dir', self.directory,
                '--token-fd', str(token_r), '--wait-seconds', '10',
                '--allow-loopback-http', '--evidence-fd', str(ev_w)]
        with mock.patch.object(client, '_publish_receipt', spy):
            code = client.run(argv)
        self.assertEqual(code, 0)
        self.assertTrue(seen['frame_at_publish'])
        frame = evidence._decode_frame(seen['frame_at_publish'])
        self.assertIsNotNone(frame)
        self.assertEqual(frame['token'], EVIDENCE_TOKEN)
        self.assertEqual(frame['deadlineUnixMs'], self.deadline_ms)
        os.close(ev_r)

    def test_missing_header_allows_without_frame(self):
        code, frame_raw, published = self._invoke(
            responses=[(200, json.dumps(allow_receipt(self.deadline_ms)).encode(), None)])
        self.assertEqual(code, 0)
        self.assertEqual(frame_raw, b'')
        self.assertEqual(published['decision'], 'allow')

    def test_multiple_headers_no_frame(self):
        # Two header values: send via raw handler is complex; the fixture
        # cannot emit duplicate headers, so verify the validator directly.
        self.assertIsNone(client._evidence_header_token(
            [EVIDENCE_TOKEN, EVIDENCE_TOKEN]))
        self.assertIsNone(client._evidence_header_token([]))
        self.assertIsNone(client._evidence_header_token(['v1.a.b']))
        self.assertIsNone(client._evidence_header_token(['ce1.a b.c']))
        self.assertEqual(client._evidence_header_token([EVIDENCE_TOKEN]),
                         EVIDENCE_TOKEN)

    def test_bad_header_shape_allows_without_frame(self):
        for bad in ('v1.aaa.bbb', 'ce1.onlyone', 'ce1.a b.c', '', 'x' * 5000):
            code, frame_raw, published = self._invoke(
                responses=[(200, json.dumps(allow_receipt(self.deadline_ms)).encode(),
                            {'X-Canter-CI-Evidence': bad})])
            self.assertEqual(code, 0, bad)
            self.assertEqual(frame_raw, b'', bad)
            self.assertEqual(published['decision'], 'allow', bad)
            # reset dir for next iteration
            os.unlink(os.path.join(self.directory, jit.FILE_RECEIPT))

    def test_deny_publishes_no_frame(self):
        deny = dict(AC_REQUEST, decision='deny')
        code, frame_raw, published = self._invoke(
            responses=[(403, json.dumps(deny).encode(),
                        {'X-Canter-CI-Evidence': EVIDENCE_TOKEN})])
        self.assertEqual(code, 0)
        self.assertEqual(frame_raw, b'')
        self.assertEqual(published['decision'], 'deny')

    def test_pending_then_allow_frame_once(self):
        self.queue_reply(202, b'{"pending":true}')
        code, frame_raw, published = self._invoke()
        self.assertEqual(code, 0)
        self.assertTrue(frame_raw)
        frame = evidence._decode_frame(frame_raw)
        self.assertIsNotNone(frame)

    def test_evidence_fd_closed_on_deny(self):
        deny = dict(AC_REQUEST, decision='deny')
        token_r, ev_r, ev_w = self._pipes()
        self.queue_reply(403, json.dumps(deny).encode(), None)
        argv = ['--endpoint', self.url('/v1/ci/runner-admissions'),
                '--admission-dir', self.directory,
                '--token-fd', str(token_r), '--wait-seconds', '10',
                '--allow-loopback-http', '--evidence-fd', str(ev_w)]
        code = client.run(argv)
        self.assertEqual(code, 0)
        # fd was closed by client: the write end is gone, read gives EOF.
        os.set_blocking(ev_r, False)
        self.assertEqual(os.read(ev_r, 1), b'')
        os.close(ev_r)

    def test_legacy_no_fd_unchanged(self):
        code, _, published = self._invoke(with_evidence=False)
        self.assertEqual(code, 0)
        self.assertEqual(published['decision'], 'allow')

    def test_evidence_fd_must_differ_from_token_fd(self):
        with self.assertRaises(SystemExit):
            client.parse_args([
                '--endpoint', 'https://x', '--admission-dir', '/tmp',
                '--token-fd', '3', '--evidence-fd', '3'])
        with self.assertRaises(SystemExit):
            client.parse_args([
                '--endpoint', 'https://x', '--admission-dir', '/tmp',
                '--token-fd', '3', '--evidence-fd', '2'])


class BootstrapEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = private_dir(self.tmp.name)
        self.request = dict(AC_REQUEST)

    def _receipt(self, decision='allow', deadline_ms=None):
        obj = dict(self.request)
        if decision == 'allow':
            obj['decision'] = 'allow'
            obj['executionDeadlineUnixMs'] = (
                deadline_ms or int(time.time() * 1000) + 3600000)
        else:
            obj['decision'] = 'deny'
        write_json(self.directory, jit.FILE_RECEIPT, obj)
        return obj

    def test_maybe_enable_requires_allow_and_matching_deadline(self):
        spec3 = importlib.util.spec_from_file_location(
            'canter_ci_guest_bootstrap',
            Path(__file__).with_name('canter-ci-guest-bootstrap.py'))
        boot = importlib.util.module_from_spec(spec3)
        spec3.loader.exec_module(boot)
        shared = {'stop': threading.Event(), 'proc': None,
                  'lock': threading.Lock()}
        deadline_ms = int(time.time() * 1000) + 3600000
        frame = dict(FRAME, deadlineUnixMs=deadline_ms)
        frame_raw = json.dumps(frame).encode()
        receipt = self._receipt(deadline_ms=deadline_ms)
        boot._maybe_enable_evidence(shared, self.directory, self.request,
                                    frame_raw)
        self.assertTrue(shared['evidence_ready'].is_set())
        self.assertEqual(shared['evidence_frame']['token'], EVIDENCE_TOKEN)

    def test_maybe_enable_rejects_deny_mismatch_and_garbage(self):
        spec3 = importlib.util.spec_from_file_location(
            'canter_ci_guest_bootstrap',
            Path(__file__).with_name('canter-ci-guest-bootstrap.py'))
        boot = importlib.util.module_from_spec(spec3)
        spec3.loader.exec_module(boot)
        good_ms = int(time.time() * 1000) + 3600000
        good_raw = json.dumps(dict(FRAME, deadlineUnixMs=good_ms)).encode()
        for decision, deadline, raw in (
                ('deny', None, good_raw),
                ('allow', good_ms + 1, good_raw),
                ('allow', good_ms, b'garbage'),
                ('allow', good_ms, b'')):
            shared = {'stop': threading.Event(), 'proc': None,
                      'lock': threading.Lock()}
            try:
                os.unlink(os.path.join(self.directory, jit.FILE_RECEIPT))
            except OSError:
                pass
            self._receipt(decision, deadline)
            boot._maybe_enable_evidence(shared, self.directory,
                                        self.request, raw)
            self.assertFalse(
                shared.get('evidence_ready') and
                shared['evidence_ready'].is_set(), (decision, deadline, raw[:20]))

    def test_drain_evidence_nonblocking(self):
        spec3 = importlib.util.spec_from_file_location(
            'canter_ci_guest_bootstrap',
            Path(__file__).with_name('canter-ci-guest-bootstrap.py'))
        boot = importlib.util.module_from_spec(spec3)
        spec3.loader.exec_module(boot)
        r, w = os.pipe()
        payload = json.dumps(FRAME).encode()
        os.write(w, payload)
        buf = boot._drain_evidence(r, b'')
        self.assertEqual(buf, payload)
        self.assertEqual(boot._drain_evidence(r, buf), buf)
        os.close(r)
        os.close(w)




class ReporterLifecycleTests(ReporterFixture):
    def test_await_post_reaps_live_child_on_communicate_failure(self):
        reporter = self.make_reporter()
        proc = subprocess.Popen(
            [sys.executable, '-c', 'import time; time.sleep(30)'],
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL, start_new_session=True)
        reporter._publishers.add(proc)
        try:
            with mock.patch.object(proc, 'communicate',
                                   side_effect=OSError('boom')):
                reply = reporter._await_post(proc, b'{}')
            self.assertEqual(reply, evidence.RETRY)
            self.assertIsNotNone(proc.poll())
            self.assertNotIn(proc, reporter._publishers)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

    def test_await_post_timeout_kills_and_reaps(self):
        reporter = self.make_reporter()
        proc = subprocess.Popen(
            [sys.executable, '-c', 'import time; time.sleep(30)'],
            stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL, start_new_session=True)
        reporter._publishers.add(proc)
        try:
            with mock.patch.object(evidence, 'POST_TIMEOUT_SECONDS', 0.3):
                reply = reporter._await_post(proc, b'{}')
            self.assertEqual(reply, evidence.RETRY)
            self.assertIsNotNone(proc.poll())
            self.assertNotIn(proc, reporter._publishers)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

    def test_spawn_registration_races_close(self):
        reporter = self.make_reporter()
        entered = threading.Event()
        release = threading.Event()
        real_popen = subprocess.Popen
        children = []

        def gated(argv, **kwargs):
            # Spawn an independent long-lived child in its own session so the
            # registration window stays open until close() is already waiting
            # on the registry; the real poster argv would self-exit first.
            proc = real_popen(
                [sys.executable, '-c', 'import time; time.sleep(30)'],
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL, close_fds=True,
                start_new_session=True)
            children.append(proc)
            entered.set()
            release.wait(10)
            return proc

        worker = threading.Thread(
            target=lambda: reporter._post_raw(b'{"schemaVersion":1}'),
            daemon=True)
        closer = threading.Thread(target=reporter.close, daemon=True)
        try:
            with mock.patch.object(evidence.subprocess, 'Popen', gated):
                worker.start()
                self.assertTrue(entered.wait(10))
                self.assertEqual(len(children), 1)
                # The child is alive but not yet registered: close() must
                # still find and reap it once registration completes.
                self.assertIsNone(children[0].poll())
                closer.start()
                # close() sets _closing before blocking on the registry.
                self.assertTrue(reporter._closing.wait(1))
                release.set()
                worker.join(timeout=10)
                closer.join(timeout=10)
            self.assertFalse(worker.is_alive())
            self.assertFalse(closer.is_alive())
            for proc in children:
                self.assertIsNotNone(proc.poll())
            self.assertEqual(reporter._publishers, set())
        finally:
            release.set()
            reporter.close()
            worker.join(timeout=10)
            closer.join(timeout=10)
            for proc in children:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait()


class JUnitDeadlineTests(ReporterFixture):
    def test_deadline_during_file_load_restores_timer(self):
        report = junit_file(self.tmp.name, 'tests.xml')
        real_read = evidence.read_junit_reports

        def slow_read(files):
            time.sleep(0.6)
            return real_read(files)

        with mock.patch.object(evidence, 'CLI_WAIT_SECONDS', 0.3), \
                mock.patch.object(evidence, 'read_junit_reports', slow_read):
            code = evidence._cmd_junit([report, '--admission-dir', self.dir])
        self.assertEqual(code, 1)
        self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))

    def test_deadline_during_recv_restores_timer(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        report = junit_file(self.tmp.name, 'tests.xml')
        # Server accepts but the post never completes: hold the mutex so the
        # packet waits past the shrunken CLI budget.
        reporter._post_mutex.acquire()
        try:
            with mock.patch.object(evidence, 'CLI_WAIT_SECONDS', 0.5), \
                    mock.patch.object(evidence, 'RETRY_SECONDS', 0.05):
                code = evidence._cmd_junit(
                    [report, '--admission-dir', self.dir])
            self.assertEqual(code, 1)
        finally:
            reporter._post_mutex.release()
            reporter.close()
            thread.join(timeout=10)
        self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))

    def test_slow_valid_ack_within_global_budget_succeeds_once(self):
        reporter = self.make_reporter()
        thread = self.spawn_server(reporter)
        report = junit_file(self.tmp.name, 'tests.xml')
        calls = []

        def slow_await(proc, raw):
            calls.append(raw)
            time.sleep(7)
            return evidence.ACCEPTED

        try:
            with mock.patch.object(reporter, '_spawn_post',
                                   return_value=FakeProc(0)), \
                    mock.patch.object(reporter, '_await_post', slow_await), \
                    mock.patch.object(evidence, 'CLI_WAIT_SECONDS', 12):
                code = evidence._cmd_junit(
                    [report, '--admission-dir', self.dir])
            self.assertEqual(code, 0)
            self.assertEqual(len(calls), 1)
        finally:
            reporter.close()
            thread.join(timeout=10)


class AdmissionFrameEdgeTests(AdmissionEvidenceTests):
    def test_saturated_pipe_still_publishes_receipt_fast(self):
        token_r, ev_r, ev_w = self._pipes()
        # Fill the pipe so the frame write hits BlockingIOError and drops.
        filler = b'x' * 70000
        os.set_blocking(ev_w, False)
        try:
            os.write(ev_w, filler)
        except BlockingIOError:
            pass
        self.default = (200, json.dumps(allow_receipt(self.deadline_ms)).encode(),
                        {'X-Canter-CI-Evidence': EVIDENCE_TOKEN})
        argv = ['--endpoint', self.url('/v1/ci/runner-admissions'),
                '--admission-dir', self.directory,
                '--token-fd', str(token_r), '--wait-seconds', '10',
                '--allow-loopback-http', '--evidence-fd', str(ev_w)]
        start = time.monotonic()
        code = client.run(argv)
        self.assertLess(time.monotonic() - start, 8)
        self.assertEqual(code, 0)
        receipt_path = os.path.join(self.directory, jit.FILE_RECEIPT)
        self.assertTrue(os.path.lexists(receipt_path))
        published = jit._read_private_json(receipt_path)
        self.assertEqual(published['decision'], 'allow')
        os.close(ev_r)

    def test_deadline_cross_during_baseline_never_publishes(self):
        token_r, ev_r, ev_w = self._pipes()
        self.default = (200, json.dumps(allow_receipt(self.deadline_ms)).encode(),
                        {'X-Canter-CI-Evidence': EVIDENCE_TOKEN})
        jumped = {'on': False}
        real_mono = client.monotonic
        real_deliver = client._deliver_evidence_frame

        def fake_mono():
            if jumped['on']:
                return real_mono() + 100
            return real_mono()

        def deliver_then_jump(fd, token, receipt, deadline):
            real_deliver(fd, token, receipt, deadline)
            jumped['on'] = True

        argv = ['--endpoint', self.url('/v1/ci/runner-admissions'),
                '--admission-dir', self.directory,
                '--token-fd', str(token_r), '--wait-seconds', '10',
                '--allow-loopback-http', '--evidence-fd', str(ev_w)]
        with mock.patch.object(client, 'monotonic', fake_mono), \
                mock.patch.object(client, '_deliver_evidence_frame',
                                  deliver_then_jump):
            code = client.main(argv)
        self.assertNotEqual(code, 0)
        self.assertFalse(os.path.lexists(
            os.path.join(self.directory, jit.FILE_RECEIPT)))
        os.close(ev_r)


class BridgePipeFailureTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        spec3 = importlib.util.spec_from_file_location(
            'canter_ci_guest_bootstrap',
            Path(__file__).with_name('canter-ci-guest-bootstrap.py'))
        self.boot = importlib.util.module_from_spec(spec3)
        spec3.loader.exec_module(self.boot)
        self.directory = private_dir(self.tmp.name)
        write_json(self.directory, jit.FILE_CONFIG, AC_CONFIG)
        write_json(self.directory, jit.FILE_REQUEST, AC_REQUEST)
        now_ms = int(time.time() * 1000)
        self.args = type('A', (), {
            'admission_dir': self.directory,
            'admission_id': ADMISSION_ID,
            'github_repository_id': 987,
            'name': 'ci-jit-1',
            'setup_deadline_unix_ms': now_ms + 60000,
            'bootstrap_expires_at': int(time.time()) + 60,
            'allow_loopback_http': True,
        })()
        self.response = {'capability': 'v1.' + 'a' * 20 + '.' + 'b' * 20,
                         'runnerId': 4242}
        self.shared = {'stop': threading.Event(), 'proc': None,
                       'lock': threading.Lock()}

    def test_second_pipe_failure_leaks_no_fds(self):
        before = len(os.listdir('/dev/fd'))
        real_pipe = os.pipe
        calls = {'n': 0}

        def flaky_pipe():
            calls['n'] += 1
            if calls['n'] == 2:
                raise OSError('pipe exhausted')
            return real_pipe()

        with mock.patch.object(self.boot.os, 'pipe', flaky_pipe):
            self.boot._bridge_worker(
                self.args, self.response,
                'http://127.0.0.1:1/v1/ci/runner-admissions', self.shared)
        after = len(os.listdir('/dev/fd'))
        self.assertLessEqual(after, before + 1)
        self.assertTrue(os.path.lexists(
            os.path.join(self.directory, jit.FILE_DENIED)))

    def test_drain_evidence_isolated_from_loader_failure(self):
        r, w = os.pipe()
        os.write(w, b'{"partial":')
        with mock.patch.object(self.boot, '_evidence',
                               side_effect=ImportError('no module')):
            out = self.boot._drain_evidence(r, b'keep')
        self.assertEqual(out, b'keep')
        os.close(r)
        os.close(w)


if __name__ == '__main__':
    unittest.main()

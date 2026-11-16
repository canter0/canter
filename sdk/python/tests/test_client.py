import base64
import io
import json
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import canter_sandboxes
from canter_sandboxes import CanterAPIError, SandboxClient


def make_server(handler):
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def make_client(server):
    return SandboxClient(f"http://127.0.0.1:{server.server_port}", "wrk_1", "token_test")


class ClientTest(unittest.TestCase):
    def test_create_idempotency_and_errors(self):
        calls = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                calls.append(self.path)
                body = self.rfile.read(int(self.headers["Content-Length"]))
                parsed = json.loads(body)
                if parsed["idempotencyKey"] == "conflict-key":
                    self.send_response(409)
                    self.end_headers()
                    self.wfile.write(json.dumps({"error": "conflict"}).encode())
                    return
                assert self.headers["Authorization"] == "Bearer token_test"
                self.send_response(202)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"id": "sbx_1", "phase": "provisioning"}).encode())

            def log_message(self, *args):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(TypeError):
                client.create({"name": "x", "size": "s-1x2"})
            self.assertEqual(len(calls), 0)
            created = client.create({"name": "build", "size": "s-2x4", "idempotencyKey": "key-1234"})
            self.assertEqual(created["id"], "sbx_1")
            with self.assertRaises(CanterAPIError) as ctx:
                client.create({"name": "x", "size": "s-1x2", "idempotencyKey": "conflict-key"})
            self.assertEqual(ctx.exception.status, 409)
        finally:
            server.shutdown()

    def test_unsafe_url_and_ids(self):
        for bad in ("https://u:p@canter.test", "https://canter.test/?q=1", "http://canter.test"):
            with self.assertRaises(ValueError):
                SandboxClient(bad, "w", "t")
        client = SandboxClient("http://127.0.0.1:1", "wrk_1", "t")
        for bad in ("", "a/b", "..", "s x"):
            with self.assertRaises(TypeError):
                client.get(bad)

    def test_redirect_never_followed(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(302)
                self.send_header("Location", "http://attacker.invalid/steal")
                self.end_headers()

            def log_message(self, *args):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(CanterAPIError):
                client.list()
        finally:
            server.shutdown()

    def test_stream_logs_reconnect_exact_offset(self):
        offsets = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                parsed = urllib.parse.urlsplit(self.path)
                offsets.append(urllib.parse.parse_qs(parsed.query)["offset"][0])
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                if len(offsets) == 1:
                    frame = {
                        "jobId": "job_1", "stream": "combined",
                        "dataBase64": base64.b64encode(b"hello").decode(),
                        "offset": 0, "nextOffset": 5, "eof": False, "status": "running",
                    }
                    self.wfile.write(f"event: output\ndata: {json.dumps(frame)}\n\n".encode())
                else:
                    frame = {
                        "jobId": "job_1", "stream": "combined",
                        "dataBase64": base64.b64encode(b" world").decode(),
                        "offset": 5, "nextOffset": 11, "eof": False, "status": "running",
                    }
                    status = {"jobId": "job_1", "status": "completed", "exitCode": 0}
                    self.wfile.write(f"event: output\ndata: {json.dumps(frame)}\n\n".encode())
                    self.wfile.write(f"event: status\ndata: {json.dumps(status)}\n\n".encode())

            def log_message(self, *args):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            data, exit_code = b"", None
            for event in client.stream_logs("sbx_1", "job_1"):
                if event["event"] == "output":
                    data += event["data"]
                if event["event"] == "status":
                    exit_code = event["exitCode"]
            self.assertEqual(data, b"hello world")
            self.assertEqual(exit_code, 0)
            self.assertEqual(offsets, ["0", "5"])
        finally:
            server.shutdown()

    def test_stream_logs_rejects_bad_receipt(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                frame = {
                    "jobId": "job_other", "stream": "combined",
                    "dataBase64": base64.b64encode(b"x").decode(),
                    "offset": 0, "nextOffset": 1, "eof": False, "status": "running",
                }
                self.wfile.write(f"event: output\ndata: {json.dumps(frame)}\n\n".encode())

            def log_message(self, *args):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(canter_sandboxes.SandboxProtocolError):
                list(client.stream_logs("sbx_1", "job_1"))
        finally:
            server.shutdown()

    def test_upload_round_trip_and_abort(self):
        size = (1 << 20) + 37
        payload = (bytes(range(256)) * (size // 256 + 1))[:size]
        received = bytearray()
        state = {"aborted": False, "completed": False, "bad_offset": False}

        class Handler(BaseHTTPRequestHandler):
            def _json(self, status, body):
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(body).encode())

            def do_POST(self):
                raw = self.rfile.read(int(self.headers["Content-Length"]))
                if self.path.endswith("/uploads"):
                    body = json.loads(raw)
                    self._json(201, {"id": "upl_1", "path": body["path"], "size": body["size"], "offset": 0, "complete": False})
                elif self.path.endswith("/complete"):
                    state["completed"] = True
                    self._json(200, {"id": "upl_1", "path": "/workspace/f", "size": len(payload), "offset": len(payload), "complete": True})
                else:
                    self.send_response(404)
                    self.end_headers()

            def do_PATCH(self):
                data = self.rfile.read(int(self.headers["Content-Length"]))
                received.extend(data)
                offset = len(received) if not state["bad_offset"] else len(received) * 2
                self._json(200, {"id": "upl_1", "path": "/workspace/f", "size": len(payload), "offset": offset, "complete": False})

            def do_DELETE(self):
                state["aborted"] = True
                self.send_response(204)
                self.end_headers()

            def log_message(self, *args):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            result = client.upload("sbx_1", "/workspace/f", io.BytesIO(payload))
            self.assertTrue(result["complete"])
            self.assertEqual(bytes(received), payload)
            self.assertFalse(state["aborted"])

            state["bad_offset"] = True
            state["completed"] = False
            with self.assertRaises(canter_sandboxes.SandboxProtocolError):
                client.upload("sbx_1", "/workspace/f", io.BytesIO(payload))
            self.assertTrue(state["aborted"])
            self.assertFalse(state["completed"])
        finally:
            server.shutdown()

    def test_download_and_truncation(self):
        payload = b"x" * 5000

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                parsed = urllib.parse.urlsplit(self.path)
                path = urllib.parse.parse_qs(parsed.query)["path"][0]
                body = payload[:10] if path == "/short" else payload
                self.send_response(200)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            dest = io.BytesIO()
            client.download("sbx_1", "/workspace/f", dest)
            self.assertEqual(dest.getvalue(), payload)
            with self.assertRaises(ValueError):
                client.download("sbx_1", "/short", io.BytesIO())
        finally:
            server.shutdown()

    def test_wait_terminal(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"id": "sbx_1", "phase": "failed", "failure": "boom"}).encode())

            def log_message(self, *args):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(RuntimeError):
                client.wait("sbx_1", "ready", timeout=10)
        finally:
            server.shutdown()


class StreamRetryTest(unittest.TestCase):
    def test_progress_survives_five_truncated_chunked_responses(self):
        offsets = []

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_GET(self):
                offset = int(urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)["offset"][0])
                offsets.append(offset)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                if offset == 5:
                    frame = b'event: status\ndata: {"jobId":"job_1","status":"completed","exitCode":0}\n\n'
                else:
                    payload = {
                        "jobId": "job_1", "stream": "combined", "dataBase64": "eA==",
                        "offset": offset, "nextOffset": offset + 1, "eof": False, "status": "running",
                    }
                    frame = f"event: output\ndata: {json.dumps(payload)}\n\n".encode()
                self.wfile.write(f"{len(frame):x}\r\n".encode() + frame + b"\r\n")
                if offset == 5:
                    self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
                # Omitting the terminating chunk raises IncompleteRead after
                # the complete output frame has been delivered.
                self.close_connection = True

            def log_message(self, *args):
                pass

        server = make_server(Handler)
        try:
            output = b"".join(event["data"] for event in make_client(server).stream_logs("sbx_1", "job_1"))
            self.assertEqual(output, b"xxxxx")
            self.assertEqual(offsets, [0, 1, 2, 3, 4, 5])
        finally:
            server.shutdown()
            server.server_close()

    def test_503_retry_then_valid(self):
        offsets = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                offsets.append(urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)["offset"][0])
                if len(offsets) == 1:
                    self.send_response(503)
                    self.end_headers()
                    self.wfile.write(b'{"error":"down"}')
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(b'event: status\ndata: {"jobId":"job_1","status":"completed","exitCode":0}\n\n')

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            events = list(client.stream_logs("sbx_1", "job_1"))
            self.assertEqual(events[-1]["status"], "completed")
            self.assertEqual(offsets, ["0", "0"])
        finally:
            server.shutdown()
            server.server_close()

    def test_midbody_cut_resumes_exact_offset(self):
        attempts = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                attempts.append(urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)["offset"][0])
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                if len(attempts) == 1:
                    self.wfile.write(b'event: output\ndata: {"jobId":"job_1","stream":"combined","dataBase64":"aGk=","offset":0,"nextOffset":2,"eof":false,"status":"running"}\n\n')
                    self.close_connection = True  # drop mid-body
                else:
                    self.wfile.write(b'event: status\ndata: {"jobId":"job_1","status":"completed","exitCode":0}\n\n')

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            data = b""
            for event in client.stream_logs("sbx_1", "job_1"):
                data += event["data"]
            self.assertEqual(data, b"hi")
            self.assertEqual(attempts, ["0", "2"])
        finally:
            server.shutdown()
            server.server_close()

    def test_token_resolved_per_reconnect(self):
        tokens = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                tokens.append(self.headers.get("Authorization"))
                if len(tokens) == 1:
                    self.send_response(503)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(b'event: status\ndata: {"jobId":"job_1","status":"completed","exitCode":0}\n\n')

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            n = [0]
            def provider():
                n[0] += 1
                return f"tok_{n[0]}"
            client = SandboxClient(f"http://127.0.0.1:{server.server_port}", "wrk_1", provider)
            list(client.stream_logs("sbx_1", "job_1"))
            self.assertEqual(tokens, ["Bearer tok_1", "Bearer tok_2"])
        finally:
            server.shutdown()
            server.server_close()

    def test_oversize_and_malformed_no_retry(self):
        calls = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(1)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(b"data: " + b"x" * ((1 << 20) + 10))

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(canter_sandboxes.SandboxProtocolError):
                list(client.stream_logs("sbx_1", "job_1"))
            self.assertEqual(len(calls), 1)
        finally:
            server.shutdown()
            server.server_close()

    def test_no_progress_stops(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                # Immediate clean EOF, no events — a stalled feed.

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(RuntimeError):
                list(client.stream_logs("sbx_1", "job_1"))
        finally:
            server.shutdown()
            server.server_close()


class UploadContractTest(unittest.TestCase):
    def test_cursor_and_relative_path(self):
        payload = b"p" * 300
        state = {"begin_path": None, "sent": 0}

        class Handler(BaseHTTPRequestHandler):
            def _json(self, status, body):
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(body).encode())

            def do_POST(self):
                body = self.rfile.read(int(self.headers["Content-Length"]))
                if self.path.endswith("/uploads"):
                    body = json.loads(body)
                    state["begin_path"] = body["path"]
                    self._json(201, {"id": "upl_1", "path": body["path"], "size": body["size"], "offset": 0, "complete": False})
                elif self.path.endswith("/complete"):
                    self._json(200, {"id": "upl_1", "path": "/workspace/rel/f", "size": len(payload), "offset": len(payload), "complete": True})
                else:
                    self.send_response(404)
                    self.end_headers()

            def do_PATCH(self):
                data = self.rfile.read(int(self.headers["Content-Length"]))
                state["sent"] += len(data)
                self._json(200, {"id": "upl_1", "path": "/workspace/rel/f", "size": len(payload), "offset": state["sent"], "complete": False})

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            source = io.BytesIO(b"SKIP" + payload)
            source.seek(4)
            result = client.upload("sbx_1", "rel/f", source)
            self.assertTrue(result["complete"])
            self.assertEqual(state["begin_path"], "/workspace/rel/f")
            self.assertEqual(state["sent"], len(payload))
            self.assertEqual(source.tell(), 4)
        finally:
            server.shutdown()
            server.server_close()

    def test_bad_begin_id_no_abort(self):
        calls = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                calls.append(self.command)
                self.send_response(201)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"id": "../evil", "path": "/workspace/f", "size": 1, "offset": 0, "complete": False}).encode())

            def do_DELETE(self):
                calls.append("DELETE")
                self.send_response(204)
                self.end_headers()

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(TypeError):
                client.upload("sbx_1", "/workspace/f", io.BytesIO(b"x"))
            self.assertNotIn("DELETE", calls)
        finally:
            server.shutdown()
            server.server_close()

    def test_bad_final_offset_rejected(self):
        class Handler(BaseHTTPRequestHandler):
            def _json(self, status, body):
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(body).encode())

            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                if self.path.endswith("/uploads"):
                    self._json(201, {"id": "upl_1", "path": "/workspace/f", "size": 4, "offset": 0, "complete": False})
                else:
                    self._json(200, {"id": "upl_1", "path": "/workspace/f", "size": 4, "offset": 999, "complete": True})

            def do_PATCH(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                self._json(200, {"id": "upl_1", "path": "/workspace/f", "size": 4, "offset": 4, "complete": False})

            def do_DELETE(self):
                self.send_response(204)
                self.end_headers()

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(canter_sandboxes.SandboxProtocolError):
                client.upload("sbx_1", "/workspace/f", io.BytesIO(b"data"))
        finally:
            server.shutdown()
            server.server_close()


class DownloadWriteTest(unittest.TestCase):
    def test_partial_and_none_write(self):
        payload = b"x" * 1000

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            # None-returning writer must error, not claim success.
            class NoneWriter:
                def write(self, data):
                    return None
            with self.assertRaises(OSError):
                client.download("sbx_1", "/workspace/f", NoneWriter())
            # Short-write writer errors on no progress.
            class ShortWriter:
                def write(self, data):
                    return 0
            with self.assertRaises(OSError):
                client.download("sbx_1", "/workspace/f", ShortWriter())
            # Partial writer that makes progress works.
            class PartialWriter:
                def __init__(self):
                    self.buf = io.BytesIO()
                def write(self, data):
                    return self.buf.write(data[: len(data) // 2 or 1])
            dest = PartialWriter()
            client.download("sbx_1", "/workspace/f", dest)
            self.assertEqual(dest.buf.getvalue(), payload)
        finally:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    unittest.main()

class SetupAwareWaitTest(unittest.TestCase):
    def test_setup_gates_ready_and_failed_reports_job(self):
        calls = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(1)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                setup = {"status": "pending"} if len(calls) == 1 else {"status": "running", "jobId": "job_setup_1"} if len(calls) == 2 else {"status": "succeeded"}
                self.wfile.write(json.dumps({"id": "sbx_1", "phase": "ready", "setup": setup}).encode())

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            v = client.wait("sbx_1", "ready", timeout=10)
            self.assertEqual(v["setup"]["status"], "succeeded")
        finally:
            server.shutdown()
            server.server_close()

        class FailHandler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"id": "sbx_1", "phase": "ready", "setup": {"status": "failed", "jobId": "job_setup_1", "error": "boom"}}).encode())

            def log_message(self, *a):
                pass

        server = make_server(FailHandler)
        try:
            client = make_client(server)
            with self.assertRaises(RuntimeError) as ctx:
                client.wait("sbx_1", "ready", timeout=10)
            self.assertIn("job_setup_1", str(ctx.exception))
        finally:
            server.shutdown()
            server.server_close()

    def test_execute_validates_receipt(self):
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(202)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"jobId": "job_1", "status": "running"}).encode())

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            job = client.execute("sbx_1", {"command": "echo hi"})
            self.assertEqual(job["jobId"], "job_1")
        finally:
            server.shutdown()
            server.server_close()

    def test_nested_error_message(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(409)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"error":{"message":"quota exceeded"}}')

            def log_message(self, *a):
                pass

        server = make_server(Handler)
        try:
            client = make_client(server)
            with self.assertRaises(CanterAPIError) as ctx:
                client.list()
            self.assertIn("quota exceeded", str(ctx.exception))
        finally:
            server.shutdown()
            server.server_close()

    def test_newline_token_rejected(self):
        client = SandboxClient("http://127.0.0.1:1", "wrk_1", "tok\nbad")
        with self.assertRaises(TypeError):
            client.list()

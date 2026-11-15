#!/usr/bin/env python3
"""Narrow stdlib tests for the Canter CI cache client transport/CLI."""

import hashlib
import io
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.parse
from unittest import mock
from http.server import BaseHTTPRequestHandler, HTTPServer

import importlib.util
from pathlib import Path

SPEC = importlib.util.spec_from_file_location(
    "canter_ci_cache", Path(__file__).with_name("canter-ci-cache.py")
)
cache = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cache)

JWT = "test.id.token"
RUNNER = "test-runner-01"


def _deadline(offset=60):
    now = time.time() + offset
    whole = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(now))
    frac = ("%09d" % int((now % 1) * 1e9)).rstrip("0") or "0"
    return whole + "." + frac + "Z"


class _App:
    """In-memory Canter cache wire fixture."""

    def __init__(self, info=None):
        self.info = info or {
            "schemaVersion": 1, "enabled": True, "audience": "",
            "maxEntryBytes": 1 << 20,
        }
        self.objects = {}       # (key, version) -> dict
        self.pending = {}       # uploadId -> dict
        self.requests = []      # (method, path, auth)
        self.put_count = 0
        self.trickle = False
        self.bad_json = False
        self.redirect_to = None
        self.expired_deadline = False
        self.object_put_bodies = []
        self.extra = {}
        self.get_extra = {}
        self.lookup_status = 0
        self.slow_object = 0.0
        self.dribble = 0.0
        self.object_gets = 0

    def reset(self):
        self.objects.clear()
        self.pending.clear()
        self.requests.clear()
        self.put_count = 0
        self.trickle = False
        self.bad_json = False
        self.redirect_to = None
        self.expired_deadline = False
        self.object_put_bodies = []
        self.extra = {}
        self.get_extra = {}
        self.lookup_status = 0
        self.slow_object = 0.0
        self.dribble = 0.0
        self.object_gets = 0
        self.info.update({"enabled": True, "maxEntryBytes": 1 << 20})


class _Handler(BaseHTTPRequestHandler):
    app = None
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _send(self, status, body, headers=None, ctype="application/json"):
        if self.app.trickle and len(body) > 8:
            self.send_response(status)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            half = len(body) // 2
            self.wfile.write(body[:half])
            self.wfile.flush()
            time.sleep(0.4)
            self.wfile.write(body[half:])
            self.wfile.flush()
            return
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def _json(self, status, obj):
        if self.app.bad_json:
            self._send(status, b"{not json", ctype="application/json")
            return
        self._send(status, json.dumps(obj).encode(), ctype="application/json",
                   headers=self.app.extra)

    def _auth(self):
        return self.headers.get("Authorization", "")

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        app = self.app
        app.requests.append(("GET", path, self._auth()))
        if app.redirect_to is not None:
            self.send_response(302)
            self.send_header("Location", app.redirect_to)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if path == "/v1/ci/cache/info":
            info = dict(app.info)
            self._json(200, info)
            return
        if path.startswith("/v1/ci/cache/objects/"):
            oid = path.rsplit("/", 1)[-1]
            for meta in app.objects.values():
                if meta["id"] == oid:
                    data = meta["data"]
                    app.object_gets += 1
                    hdrs = {
                        "ETag": '"%s"' % meta["sha"],
                        "X-Canter-Cache-SHA256": meta["sha"],
                    }
                    hdrs.update(app.get_extra)
                    if app.dribble:
                        self.send_response(200)
                        self.send_header("Content-Type", "application/octet-stream")
                        self.send_header("Content-Length", str(len(data)))
                        self.send_header("Connection", "close")
                        for k, v in hdrs.items():
                            self.send_header(k, v)
                        self.end_headers()
                        self.close_connection = True
                        self.wfile.flush()
                        chunks = (len(data) + 511) // 512
                        step = app.dribble / max(1, chunks)
                        for off in range(0, len(data), 512):
                            self.wfile.write(data[off:off + 512])
                            self.wfile.flush()
                            time.sleep(step)
                        return
                    if app.slow_object:
                        self.send_response(200)
                        self.send_header("Content-Type", "application/octet-stream")
                        self.send_header("Content-Length", str(len(data)))
                        for k, v in hdrs.items():
                            self.send_header(k, v)
                        self.end_headers()
                        self.wfile.flush()
                        time.sleep(app.slow_object)
                        self.wfile.write(data)
                        self.wfile.flush()
                        return
                    self._send(200, data, ctype="application/octet-stream",
                               headers=hdrs)
                    return
            self._json(404, {"error": "not_found"})
            return
        self._json(404, {"error": "not_found"})

    def do_POST(self):
        path = urllib.parse.urlsplit(self.path).path
        app = self.app
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        app.requests.append(("POST", path, self._auth()))
        if path == "/v1/ci/cache/lookup":
            body = json.loads(raw.decode())
            meta = app.objects.get((body["key"], body["cacheVersion"]))
            status = app.lookup_status or 200
            if meta is None:
                self._json(status, {"schemaVersion": 1, "found": False})
            else:
                self._json(status, {
                    "schemaVersion": 1, "found": True,
                    "objectId": meta["id"], "sizeBytes": meta["size"],
                    "sha256": meta["sha"],
                })
            return
        if path == "/v1/ci/cache/reservations":
            body = json.loads(raw.decode())
            k = (body["key"], body["cacheVersion"])
            if k in app.objects:
                self._json(200, {"schemaVersion": 1, "created": False})
                return
            oid = hashlib.sha256(
                (k[0] + k[1] + str(time.time())).encode()
            ).hexdigest()
            app.pending[oid] = {
                "id": oid, "key": k[0], "version": k[1],
                "size": body["sizeBytes"], "sha": body["sha256"],
            }
            self._json(201, {
                "schemaVersion": 1, "created": True, "uploadId": oid,
                "deadline": _deadline(-60 if app.expired_deadline else 60),
            })
            return
        self._json(404, {"error": "not_found"})

    def do_PUT(self):
        parts = urllib.parse.urlsplit(self.path)
        app = self.app
        oid = parts.path.rsplit("/", 1)[-1]
        length = int(self.headers.get("Content-Length") or 0)
        app.requests.append(("PUT", parts.path, self._auth()))
        app.put_count += 1
        meta = app.pending.get(oid)
        if meta is None:
            self._json(404, {"error": "not_found"})
            return
        data = self.rfile.read(length)
        app.object_put_bodies.append(len(data))
        sha = hashlib.sha256(data).hexdigest()
        if sha != meta["sha"] or len(data) != meta["size"]:
            self._json(409, {"error": "conflict"})
            return
        meta["data"] = data
        app.objects[(meta["key"], meta["version"])] = meta
        del app.pending[oid]
        self._json(200, {"schemaVersion": 1, "saved": True})


class _ClientCase(unittest.TestCase):
    server = None
    origin = ""
    client_ctx = None
    app = None
    cert_dir = ""
    fd_before = 0

    @classmethod
    def setUpClass(cls):
        cls.cert_dir = tempfile.mkdtemp(prefix="cci-cert-")
        key = os.path.join(cls.cert_dir, "k.pem")
        cert = os.path.join(cls.cert_dir, "c.pem")
        subprocess.run(
            ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
             "-keyout", key, "-out", cert, "-days", "1",
             "-subj", "/CN=localhost",
             "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost"],
            check=True, capture_output=True,
        )
        cls.client_ctx = ssl.create_default_context(cafile=cert)
        server_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        server_ctx.load_cert_chain(cert, key)
        _Handler.app = _App()
        cls.app = _Handler.app
        httpd = HTTPServer(("127.0.0.1", 0), _Handler)
        httpd.socket = server_ctx.wrap_socket(httpd.socket, server_side=True)
        cls.server = httpd
        cls.origin = "https://127.0.0.1:%d" % httpd.server_address[1]
        cls.thread = threading.Thread(target=httpd.serve_forever, daemon=True)
        cls.thread.start()
        plain = HTTPServer(("127.0.0.1", 0), _Handler)
        cls.plain_server = plain
        cls.http_origin = "http://127.0.0.1:%d" % plain.server_address[1]
        pthread = threading.Thread(target=plain.serve_forever, daemon=True)
        pthread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.plain_server.shutdown()
        cls.plain_server.server_close()
        shutil.rmtree(cls.cert_dir, ignore_errors=True)

    def setUp(self):
        self.app.reset()
        self.app.info["audience"] = cache.cache_origin(self.origin)[1]
        self.tmp = tempfile.mkdtemp(prefix="cci-test-")
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.token_calls = []
        self.last_transport = []

    def env(self, **extra):
        env = {"RUNNER_NAME": RUNNER, "CANTER_CI_CACHE_URL": self.origin}
        env.update(extra)
        return env

    def provider(self, env, audience, transport):
        self.token_calls.append(audience)
        return JWT

    def factory(self, seconds_holder=None):
        def make(seconds):
            t = cache._Transport(seconds, ssl_context=self.client_ctx)
            self.last_transport.append(t)
            return t
        return make

    def run_cli(self, argv, env=None, seconds=120, provider=None):
        out = io.StringIO()
        err = io.StringIO()
        oldout, olderr = sys.stdout, sys.stderr
        sys.stdout, sys.stderr = out, err
        try:
            rc = cache.main(
                argv, env if env is not None else self.env(),
                transport_factory=self.factory(),
                token_provider=provider or self.provider,
                operation_seconds=seconds,
            )
        finally:
            sys.stdout, sys.stderr = oldout, olderr
        text = out.getvalue().strip()
        result = json.loads(text) if text else None
        return rc, result, err.getvalue()

    def payload_file(self, name="blob.bin", data=None):
        data = data if data is not None else os.urandom(8192)
        path = os.path.join(self.tmp, name)
        with open(path, "wb") as fh:
            fh.write(data)
        return path, data

    # -- core wire ---------------------------------------------------------

    def test_save_restore_roundtrip(self):
        src, data = self.payload_file()
        rc, res, _ = self.run_cli([
            "save", "--key", "k1", "--cache-version", "v1", "--file", src])
        self.assertEqual(rc, 0)
        self.assertEqual(res, {"schemaVersion": 1, "operation": "save",
                               "saved": True, "sizeBytes": len(data)})
        dst = os.path.join(self.tmp, "out.bin")
        rc, res, _ = self.run_cli([
            "restore", "--key", "k1", "--cache-version", "v1",
            "--output", dst])
        self.assertEqual(rc, 0)
        self.assertEqual(res["hit"], True)
        self.assertEqual(res["sizeBytes"], len(data))
        with open(dst, "rb") as fh:
            self.assertEqual(fh.read(), data)
        # bearer went only to cache routes, never /info
        auths = [a for _, p, a in self.app.requests if p == "/v1/ci/cache/info"]
        self.assertTrue(all(a == "" for a in auths))
        bearer = [a for _, p, a in self.app.requests
                  if p != "/v1/ci/cache/info"]
        self.assertTrue(bearer)
        self.assertTrue(all(a == "Bearer " + JWT for a in bearer))

    def test_lookup_miss_restore(self):
        dst = os.path.join(self.tmp, "out.bin")
        rc, res, _ = self.run_cli([
            "restore", "--key", "nope", "--cache-version", "v1",
            "--output", dst])
        self.assertEqual(rc, 0)
        self.assertEqual(res["hit"], False)
        self.assertEqual(res["reason"], "miss")
        self.assertFalse(os.path.exists(dst))

    def test_duplicate_save_no_put(self):
        src, data = self.payload_file()
        argv = ["save", "--key", "dup", "--cache-version", "v1", "--file", src]
        rc, res, _ = self.run_cli(argv)
        self.assertTrue(res["saved"])
        puts = self.app.put_count
        rc, res, _ = self.run_cli(argv)
        self.assertEqual(rc, 0)
        self.assertEqual(res["saved"], False)
        self.assertEqual(self.app.put_count, puts)

    def test_disabled_pool_miss(self):
        self.app.info["enabled"] = False
        self.app.info["maxEntryBytes"] = 0
        src, _ = self.payload_file()
        rc, res, _ = self.run_cli([
            "save", "--key", "k", "--cache-version", "v", "--file", src])
        self.assertEqual(res["reason"], "disabled")
        self.assertEqual(self.token_calls, [])

    def test_audience_mismatch_zero_token(self):
        self.app.info["audience"] = "urn:canter:ci:cache:v1:" + "0" * 64
        src, _ = self.payload_file()
        rc, res, _ = self.run_cli([
            "save", "--key", "k", "--cache-version", "v", "--file", src])
        self.assertEqual(res["reason"], "invalid_response")
        self.assertEqual(self.token_calls, [])
        self.assertEqual(self.app.put_count, 0)

    def test_missing_credentials_miss(self):
        def no_creds(env, audience, transport):
            raise cache._Skip("no_credentials")
        src, _ = self.payload_file()
        rc, res, _ = self.run_cli(
            ["save", "--key", "k", "--cache-version", "v", "--file", src],
            provider=no_creds)
        self.assertEqual(res["reason"], "no_credentials")
        self.assertEqual(self.app.put_count, 0)

    def test_unconfigured_miss(self):
        rc, res, _ = self.run_cli(
            ["save", "--key", "k", "--cache-version", "v",
             "--file", "/tmp/x"],
            env={"RUNNER_NAME": RUNNER})
        self.assertEqual(res["reason"], "unconfigured")

    def test_explicit_bad_url_exit2(self):
        rc, res, err = self.run_cli(
            ["save", "--url", "http://example.com", "--key", "k",
             "--cache-version", "v", "--file", "/tmp/x"])
        self.assertEqual(rc, 2)
        self.assertIsNone(res)
        self.assertNotIn("example.com", err)

    def test_cli_syntax_exit2(self):
        rc, res, err = self.run_cli(["bogus"])
        self.assertEqual(rc, 2)
        rc, res, err = self.run_cli(
            ["save", "--key", "k"], env=self.env())
        self.assertEqual(rc, 2)
        self.assertIn("usage", err.lower())

    def test_proxy_env_ignored(self):
        src, data = self.payload_file()
        proxies = {
            "HTTP_PROXY": "http://127.0.0.1:1",
            "HTTPS_PROXY": "http://127.0.0.1:1",
            "http_proxy": "http://127.0.0.1:1",
            "https_proxy": "http://127.0.0.1:1",
            "ALL_PROXY": "http://127.0.0.1:1",
            "all_proxy": "http://127.0.0.1:1",
        }
        with mock.patch.dict(os.environ, proxies):
            rc, res, _ = self.run_cli(
                ["save", "--key", "px", "--cache-version", "v",
                 "--file", src])
        self.assertTrue(res["saved"])

    def test_redirect_not_followed(self):
        self.app.redirect_to = "https://evil.example/x"
        src, _ = self.payload_file()
        rc, res, _ = self.run_cli([
            "save", "--key", "r", "--cache-version", "v", "--file", src])
        self.assertEqual(res["saved"], False)
        self.assertNotIn("evil.example", str(self.app.requests))
        paths = [p for _, p, _ in self.app.requests]
        self.assertFalse(any("evil" in p for p in paths))

    # -- file edge cases ----------------------------------------------------

    def test_oversize_file_miss(self):
        self.app.info["maxEntryBytes"] = 64
        src, _ = self.payload_file(data=b"x" * 128)
        rc, res, _ = self.run_cli([
            "save", "--key", "big", "--cache-version", "v", "--file", src])
        self.assertEqual(res["reason"], "too_large")
        self.assertEqual(self.token_calls, [])

    def test_symlink_source_rejected(self):
        src, data = self.payload_file()
        link = os.path.join(self.tmp, "link.bin")
        os.symlink(src, link)
        rc, res, _ = self.run_cli([
            "save", "--key", "s", "--cache-version", "v", "--file", link])
        self.assertEqual(res["saved"], False)
        self.assertEqual(res["reason"], "local_io")

    def test_fifo_source_rejected_bounded(self):
        fifo = os.path.join(self.tmp, "fifo")
        os.mkfifo(fifo)
        start = time.time()
        rc, res, _ = self.run_cli([
            "save", "--key", "f", "--cache-version", "v", "--file", fifo],
            seconds=5)
        self.assertLess(time.time() - start, 5)
        self.assertEqual(res["saved"], False)

    def test_path_swap_uses_same_fd(self):
        src, data = self.payload_file()
        replacement = os.path.join(self.tmp, "other.bin")
        with open(replacement, "wb") as fh:
            fh.write(b"Z" * len(data))

        def swap_provider(env, audience, transport):
            os.replace(replacement, src)  # path now points elsewhere
            return JWT

        rc, res, _ = self.run_cli(
            ["save", "--key", "swap", "--cache-version", "v", "--file", src],
            provider=swap_provider)
        # The already-open FD still holds the original bytes: hash and
        # upload agree, so the save succeeds with the ORIGINAL content.
        self.assertTrue(res["saved"])
        dst = os.path.join(self.tmp, "back.bin")
        self.run_cli(["restore", "--key", "swap", "--cache-version", "v",
                      "--output", dst])
        with open(dst, "rb") as fh:
            self.assertEqual(fh.read(), data)

    def test_post_hash_mutation_rejected_by_server_sha(self):
        src, data = self.payload_file()

        def mutate_provider(env, audience, transport):
            # Runs AFTER the hash/fstat checks: same-size in-place rewrite.
            # The reserved SHA no longer matches the bytes on the wire, so
            # the server must reject the PUT rather than publish corrupt data.
            with open(src, "r+b") as fh:
                fh.write(b"M" * len(data))
            return JWT

        rc, res, _ = self.run_cli(
            ["save", "--key", "mut", "--cache-version", "v", "--file", src],
            provider=mutate_provider)
        self.assertEqual(res["saved"], False)
        self.assertEqual(res["reason"], "conflict")
        self.assertEqual(len(self.app.objects), 0)

    # -- restore edge cases -------------------------------------------------

    def test_restore_existing_dest_untouched(self):
        dst = os.path.join(self.tmp, "exists.bin")
        with open(dst, "wb") as fh:
            fh.write(b"keep-me")
        rc, res, _ = self.run_cli([
            "restore", "--key", "none", "--cache-version", "v",
            "--output", dst])
        self.assertEqual(res["reason"], "exists")
        with open(dst, "rb") as fh:
            self.assertEqual(fh.read(), b"keep-me")

    def _publish(self, key="rk", data=None):
        src, data = self.payload_file("src-%s" % key,
                                      data if data is not None else b"R" * 4096)
        self.run_cli(["save", "--key", key, "--cache-version", "v1",
                      "--file", src])
        return data

    def test_restore_truncated_never_publishes(self):
        data = self._publish("trunc")
        orig_read = _Handler.wfile_write if hasattr(_Handler, "wfile_write") else None
        # shrink object server-side: corrupt stored blob length via direct edit
        for meta in self.app.objects.values():
            meta["data"] = meta["data"][:-10]
            meta["size"] = len(meta["data"])
        dst = os.path.join(self.tmp, "out.bin")
        rc, res, _ = self.run_cli([
            "restore", "--key", "trunc", "--cache-version", "v1",
            "--output", dst])
        self.assertEqual(res["hit"], False)
        self.assertFalse(os.path.exists(dst))
        leftovers = [f for f in os.listdir(self.tmp)
                     if f.startswith(".canter-ci-cache-")]
        self.assertEqual(leftovers, [])

    def test_restore_wrong_sha_never_publishes(self):
        self._publish("badsha")
        for meta in self.app.objects.values():
            meta["sha"] = "0" * 64
        dst = os.path.join(self.tmp, "out.bin")
        rc, res, _ = self.run_cli([
            "restore", "--key", "badsha", "--cache-version", "v1",
            "--output", dst])
        self.assertEqual(res["hit"], False)
        self.assertFalse(os.path.exists(dst))

    def test_restore_content_length_mismatch(self):
        self._publish("clen")
        dst = os.path.join(self.tmp, "out.bin")
        # server now reports a different length than stored bytes
        for meta in self.app.objects.values():
            meta["size"] = meta["size"] + 1
        rc, res, _ = self.run_cli([
            "restore", "--key", "clen", "--cache-version", "v1",
            "--output", dst])
        self.assertEqual(res["hit"], False)
        self.assertFalse(os.path.exists(dst))

    def test_bad_json_invalid_response(self):
        self.app.bad_json = True
        src, _ = self.payload_file()
        rc, res, _ = self.run_cli([
            "save", "--key", "j", "--cache-version", "v", "--file", src])
        self.assertEqual(res["reason"], "invalid_response")

    def test_trickle_bounded_by_budget(self):
        self.app.trickle = True
        src, _ = self.payload_file()
        start = time.time()
        rc, res, _ = self.run_cli(
            ["save", "--key", "t", "--cache-version", "v", "--file", src],
            seconds=1)
        elapsed = time.time() - start
        self.assertLess(elapsed, 1.5)
        self.assertEqual(res["saved"], False)

    def test_no_fd_or_timer_leaks(self):
        src, _ = self.payload_file()
        before = len(os.listdir("/dev/fd"))
        for i in range(3):
            self.run_cli(["save", "--key", "leak%d" % i,
                          "--cache-version", "v", "--file", src])
        after = len(os.listdir("/dev/fd"))
        self.assertLessEqual(after, before + 2)
        for t in self.last_transport:
            self.assertFalse(t.watchdog._thread.is_alive())


    # -- v2 contract batch ---------------------------------------------------

    def _fake_reply(self, status, body, headers=None):
        class R:
            def __init__(self):
                self.status = status
                self._body = body
                self._headers = headers or []
                self.closed = False

            @property
            def headers(self):
                return self._headers

            def header(self, name):
                vals = [v for k, v in self._headers
                        if k.lower() == name.lower()]
                return vals[0] if len(vals) == 1 else None

            def header_count(self, name):
                return sum(1 for k, _ in self._headers
                           if k.lower() == name.lower())

            def read(self, n):
                out, self._body = self._body[:n], self._body[n:]
                return out

            def close(self):
                self.closed = True

        return R()

    def _fake_transport(self, reply):
        calls = []

        class T:
            budget = cache._Budget(30)

            def request(self, method, url, headers=None, body=None,
                        content_length=None):
                calls.append((method, url, headers))
                return reply

            def close(self):
                pass

        return T(), calls

    def test_mint_id_token_gh_only(self):
        audience = cache.cache_origin(self.origin)[1]
        body = json.dumps({"value": JWT, "extra": "allowed"}).encode()
        reply = self._fake_reply(200, body,
                                 [("Content-Type", "application/json")])
        transport, calls = self._fake_transport(reply)
        env = {
            "ACTIONS_ID_TOKEN_REQUEST_URL":
                "https://token.actions.githubusercontent.com/?api-version=2.0",
            "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "runtime-bearer-1",
        }
        token = cache._mint_id_token(env, audience, transport)
        self.assertEqual(token, JWT)
        self.assertEqual(len(calls), 1)
        method, url, headers = calls[0]
        self.assertEqual(method, "GET")
        parts = urllib.parse.urlsplit(url)
        self.assertEqual(parts.scheme, "https")
        self.assertTrue(parts.hostname.endswith(".actions.githubusercontent.com"))
        self.assertNotIn("127.0.0.1", url)
        self.assertNotIn(self.origin, url)
        query = urllib.parse.parse_qs(parts.query)
        self.assertEqual(query.get("audience"), [audience])
        self.assertEqual(headers.get("Authorization"),
                         "Bearer runtime-bearer-1")
        # The minted JWT is then the only Bearer on Canter requests.
        src, data = self.payload_file()
        rc, res, _ = self.run_cli(
            ["save", "--key", "mint", "--cache-version", "v", "--file", src],
            provider=lambda e, a, t: token)
        self.assertTrue(res["saved"])
        bearer = [a for _, p, a in self.app.requests
                  if p != "/v1/ci/cache/info"]
        self.assertTrue(all(a == "Bearer " + JWT for a in bearer))

    def test_mint_id_token_failures(self):
        audience = cache.cache_origin(self.origin)[1]
        env = {
            "ACTIONS_ID_TOKEN_REQUEST_URL":
                "https://token.actions.githubusercontent.com/?api-version=2.0",
            "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "rt",
        }
        # 302 is never followed
        t, _ = self._fake_transport(self._fake_reply(302, b""))
        with self.assertRaises(cache._TransportError):
            cache._mint_id_token(env, audience, t)
        # malformed JSON
        t, _ = self._fake_transport(self._fake_reply(200, b"{bad"))
        with self.assertRaises(cache.CacheProtocolError):
            cache._mint_id_token(env, audience, t)
        # duplicate value keys rejected
        t, _ = self._fake_transport(
            self._fake_reply(200, b'{"value":"a","value":"b"}'))
        with self.assertRaises(cache.CacheProtocolError):
            cache._mint_id_token(env, audience, t)
        # oversized value rejected
        t, _ = self._fake_transport(
            self._fake_reply(200, json.dumps(
                {"value": "x" * (cache.MAX_JWT_BYTES + 1)}).encode()))
        with self.assertRaises(cache.CacheProtocolError):
            cache._mint_id_token(env, audience, t)
        # missing env -> optional miss
        with self.assertRaises(cache._Skip):
            cache._mint_id_token({}, audience, t)

    def test_default_tls_rejects_untrusted(self):
        # No injected CA: the self-signed fixture must fail verification.
        def factory(seconds):
            return cache._Transport(seconds)
        src, _ = self.payload_file()
        out = io.StringIO()
        old = sys.stdout
        sys.stdout = out
        try:
            rc = cache.main(
                ["save", "--key", "tls", "--cache-version", "v",
                 "--file", src], self.env(), factory, self.provider, 10)
        finally:
            sys.stdout = old
        res = json.loads(out.getvalue())
        self.assertEqual(res["saved"], False)
        self.assertEqual(res["reason"], "unavailable")

    def test_source_growth_sends_exact_size(self):
        src, data = self.payload_file()

        def grow_provider(env, audience, transport):
            with open(src, "ab") as fh:
                fh.write(b"G" * 100)
            return JWT

        rc, res, _ = self.run_cli(
            ["save", "--key", "grow", "--cache-version", "v", "--file", src],
            provider=grow_provider)
        self.assertTrue(res["saved"])
        # Exactly the authorized size went on the wire — never the growth.
        self.assertEqual(self.app.object_put_bodies, [len(data)])

    def test_expired_reservation_no_put(self):
        self.app.expired_deadline = True
        src, _ = self.payload_file()
        rc, res, _ = self.run_cli([
            "save", "--key", "exp", "--cache-version", "v", "--file", src])
        self.assertEqual(res["saved"], False)
        self.assertEqual(res["reason"], "expired")
        self.assertEqual(self.app.put_count, 0)
        self.assertEqual(len(self.app.objects), 0)

    def test_restore_expired_budget_no_dest(self):
        data = self._publish("slowpub")
        self.app.slow_object = 0.6
        dst = os.path.join(self.tmp, "out.bin")
        start = time.time()
        rc, res, _ = self.run_cli(
            ["restore", "--key", "slowpub", "--cache-version", "v1",
             "--output", dst], seconds=0.4)
        elapsed = time.time() - start
        self.assertLess(elapsed, 3)
        self.assertEqual(res["hit"], False)
        self.assertFalse(os.path.exists(dst))
        self.app.slow_object = 0.0

    def test_content_encoding_rejected(self):
        self.app.extra = {"Content-Encoding": "gzip"}
        src, _ = self.payload_file()
        rc, res, _ = self.run_cli([
            "save", "--key", "enc", "--cache-version", "v", "--file", src])
        self.assertEqual(res["reason"], "invalid_response")

    def test_duplicate_content_length_rejected(self):
        self._publish("duplcl")
        self.app.get_extra["Content-Length"] = "1"
        dst = os.path.join(self.tmp, "out.bin")
        rc, res, _ = self.run_cli([
            "restore", "--key", "duplcl", "--cache-version", "v1",
            "--output", dst])
        self.assertEqual(res["hit"], False)
        self.assertFalse(os.path.exists(dst))

    def test_lookup_401_hit_shaped_not_accepted(self):
        self._publish("authz")
        self.app.lookup_status = 401
        dst = os.path.join(self.tmp, "out.bin")
        rc, res, _ = self.run_cli([
            "restore", "--key", "authz", "--cache-version", "v1",
            "--output", dst])
        self.assertEqual(res["hit"], False)
        self.assertEqual(res["reason"], "denied")
        self.assertFalse(os.path.exists(dst))

    def test_http_loopback_bounded_budget(self):
        # Plain-HTTP literal-IP loopback shares the same bounded transport.
        self.app.info["audience"] = cache.cache_origin(
            self.http_origin, allow_loopback_http=True)[1]
        self.app.trickle = True
        src, _ = self.payload_file()
        env = {"RUNNER_NAME": RUNNER, "CANTER_CI_CACHE_URL": self.http_origin}
        start = time.time()
        rc, res, _ = self.run_cli(
            ["save", "--url", self.http_origin, "--allow-loopback-http",
             "--key", "hb", "--cache-version", "v", "--file", src],
            env=env, seconds=1)
        elapsed = time.time() - start
        self.assertLess(elapsed, 1.5)
        self.assertEqual(res["saved"], False)
        self.app.trickle = False

    def test_late_dns_worker_never_connects(self):
        gate = threading.Event()
        calls = []
        real_gai = socket.getaddrinfo

        def slow_gai(*args, **kwargs):
            calls.append(args)
            gate.wait(5)
            return real_gai(*args, **kwargs)

        import socket as _socket
        budget = cache._Budget(0.1)
        watchdog = cache._Watchdog(budget)
        try:
            with mock.patch.object(_socket, "getaddrinfo", slow_gai):
                start = time.time()
                with self.assertRaises(cache._TransportError):
                    cache._tcp_connect("127.0.0.1", 443, budget, watchdog)
                self.assertLess(time.time() - start, 1.0)
                self.assertEqual(len(calls), 1)
            # Release the late worker: it only resolves, never connects.
            gate.set()
            time.sleep(0.2)
        finally:
            watchdog.close()

    def test_connection_close_dribble_bounded(self):
        # will_close responses keep the wire socket watchdog-registered
        # until the body finishes; a >1s single body with <1s gaps must
        # still die at the private 1s budget on BOTH schemes.
        data = self._publish("drib")
        # 3s of trickle with <1s gaps: only the watchdog can stop the
        # in-flight read(); per-read idle timeouts and budget.check() at
        # chunk boundaries cannot discriminate the bug.
        self.app.dribble = 3.0
        dst = os.path.join(self.tmp, "out.bin")
        fd_before = len(os.listdir("/dev/fd"))
        for label, argv_extra, env in (
            ("https", [], self.env()),
            ("http", ["--allow-loopback-http"],
             {"RUNNER_NAME": RUNNER,
              "CANTER_CI_CACHE_URL": self.http_origin}),
        ):
            if label == "http":
                self.app.info["audience"] = cache.cache_origin(
                    self.http_origin, allow_loopback_http=True)[1]
                argv = ["restore", "--url", self.http_origin,
                        "--allow-loopback-http", "--key", "drib",
                        "--cache-version", "v1", "--output", dst]
            else:
                argv = ["restore", "--key", "drib", "--cache-version", "v1",
                        "--output", dst]
            self.app.object_gets = 0
            start = time.time()
            rc, res, _ = self.run_cli(argv, env=env, seconds=1)
            elapsed = time.time() - start
            self.assertLess(elapsed, 1.5, label)
            self.assertEqual(res["hit"], False, label)
            self.assertEqual(res["reason"], "unavailable", label)
            self.assertGreaterEqual(self.app.object_gets, 1, label)
            self.assertFalse(os.path.exists(dst), label)
        fd_after = len(os.listdir("/dev/fd"))
        self.assertLessEqual(fd_after, fd_before + 2)
        for t in self.last_transport:
            self.assertFalse(t.watchdog._thread.is_alive())
        self.app.dribble = 0.0
        self.app.info["audience"] = cache.cache_origin(self.origin)[1]

    def test_tcp_connect_deadline_leaks_no_socket(self):
        # budget.check passes but step() raises: the fresh socket must be
        # closed without ever connecting.
        class DeadBudget:
            def remaining(self):
                return 5.0

            def check(self):
                return 5.0

            def step(self):
                raise cache._Deadline()

        fake = mock.Mock()
        fake.connect = mock.Mock()
        watchdog = cache._Watchdog(DeadBudget())
        try:
            with mock.patch.object(cache.socket, "socket",
                                   lambda *a, **k: fake):
                with self.assertRaises(cache._Deadline):
                    cache._tcp_connect("127.0.0.1", 443, DeadBudget(),
                                       watchdog)
            fake.close.assert_called_once()
            fake.connect.assert_not_called()
        finally:
            watchdog.close()

    def test_tcp_connect_falls_back(self):
        # First resolved address refuses, second connects: fallback works.
        addrs = [(socket.AF_INET, ("127.0.0.1", 1)),
                 (socket.AF_INET, ("127.0.0.1", 2))]
        socks = []

        class FakeSock:
            def __init__(self, fail):
                self.fail = fail
                self.closed = False
                self.timeouts = []

            def settimeout(self, t):
                self.timeouts.append(t)

            def connect(self, sa):
                if self.fail:
                    raise OSError("refused")

            def shutdown(self, how):
                pass

            def close(self):
                self.closed = True

        def fake_socket(*a, **k):
            sock = FakeSock(len(socks) == 0)
            socks.append(sock)
            return sock

        watchdog = cache._Watchdog(cache._Budget(10))
        try:
            with mock.patch.object(cache.socket, "getaddrinfo",
                                   lambda *a, **k: [
                                       (f, 0, 0, "", sa) for f, sa in addrs]):
                with mock.patch.object(cache.socket, "socket", fake_socket):
                    out = cache._tcp_connect("127.0.0.1", 443,
                                             cache._Budget(10), watchdog)
            self.assertIs(out, socks[1])
            self.assertTrue(socks[0].closed)
            self.assertFalse(socks[1].closed)
        finally:
            watchdog.close()

    def test_c1_control_char_rejected(self):
        rc, res, err = self.run_cli(
            ["save", "--key", "a\x85b", "--cache-version", "v",
             "--file", "/tmp/x"])
        self.assertEqual(rc, 2)
        self.assertIsNone(res)

    def test_help_output(self):
        out = io.StringIO()
        old = sys.stdout
        sys.stdout = out
        try:
            rc = cache.main(["--help"], {}, None, None, 1)
        finally:
            sys.stdout = old
        self.assertEqual(rc, 0)
        text = out.getvalue()
        self.assertIn("save", text)
        self.assertIn("restore", text)
        self.assertNotIn(self.origin, text)
        self.assertNotIn(JWT, text)


if __name__ == "__main__":
    unittest.main()

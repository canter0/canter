import test from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { SandboxClient, CanterAPIError, SandboxProtocolError } from "../dist/index.js";

function makeClient(handler, opts = {}) {
  const calls = [];
  const fakeFetch = async (url, init = {}) => {
    calls.push({ url, init });
    return handler(url, init);
  };
  const client = new SandboxClient({
    baseUrl: "http://127.0.0.1:9999",
    workspaceId: "wrk_1",
    accessToken: "token_test",
    fetch: fakeFetch,
    ...opts,
  });
  return { client, calls };
}

function json(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function sse(frames) {
  return new Response(frames.join(""), {
    headers: { "Content-Type": "text/event-stream" },
  });
}

function frame(event, data) {
  return `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
}

test("create requires idempotency key and sends frozen body", async () => {
  const { client, calls } = makeClient((url, init) => {
    assert.equal(new URL(url).pathname, "/v1/workspaces/wrk_1/sandboxes");
    assert.equal(init.headers.Authorization, "Bearer token_test");
    return json({ id: "sbx_1", phase: "provisioning" }, 202);
  });
  await assert.rejects(
    client.create({ name: "x", size: "s-1x2" }),
    /idempotency/,
  );
  assert.equal(calls.length, 0);
  const input = { name: "build", size: "s-2x4", idempotencyKey: "key-1234" };
  const promise = client.create(input);
  input.name = "mutated"; // body already serialized — mutation is inert
  const created = await promise;
  assert.equal(created.id, "sbx_1");
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.name, "build");
  assert.equal(sent.idempotencyKey, "key-1234");
});

test("403 and 409 surface as CanterAPIError without token leakage", async () => {
  const { client: c403 } = makeClient(() => json({ error: "not permitted" }, 403));
  const e403 = await c403.list().catch((e) => e);
  assert.ok(e403 instanceof CanterAPIError);
  assert.equal(e403.status, 403);
  assert.ok(!e403.message.includes("token_test"));
  const { client: c409 } = makeClient(() => json({ error: "conflict" }, 409));
  const e409 = await c409.create({ name: "x", size: "s-1x2", idempotencyKey: "key-1234" }).catch((e) => e);
  assert.ok(e409 instanceof CanterAPIError);
  assert.equal(e409.status, 409);
});

test("unsafe base URL and IDs rejected with strict types", async () => {
  for (const bad of ["https://u:p@canter.test", "https://canter.test/?q=1", "http://canter.test"]) {
    assert.throws(() => new SandboxClient({ baseUrl: bad, workspaceId: "w", accessToken: "t" }));
  }
  const { client } = makeClient(() => json({}));
  for (const id of ["", "a/b", "..", "s x", 5, null, { toString: () => "sbx_1" }]) {
    await assert.rejects(client.get(id), TypeError);
  }
});

test("redirect never followed on a real local server", async () => {
  let redirected = 0;
  const target = http.createServer((_req, res) => {
    redirected++;
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end('{"sandboxes":[]}');
  });
  await new Promise((resolve) => target.listen(0, "127.0.0.1", resolve));
  const server = http.createServer((_req, res) => {
    res.writeHead(302, { Location: `http://127.0.0.1:${target.address().port}/steal` });
    res.end();
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  try {
    const client = new SandboxClient({
      baseUrl: `http://127.0.0.1:${server.address().port}`,
      workspaceId: "wrk_1",
      accessToken: "token_test",
    });
    await assert.rejects(client.list());
    assert.equal(redirected, 0);
  } finally {
    await Promise.all([server, target].map((listener) => new Promise((resolve, reject) => {
      listener.close((error) => error ? reject(error) : resolve());
      listener.closeAllConnections();
    })));
  }
});

test("streamLogs: 503 then valid feed resumes at exact offset", async () => {
  let n = 0;
  const { client } = makeClient(() => {
    n++;
    if (n === 1) return json({ error: "down" }, 503);
    return sse([
      frame("output", { jobId: "job_1", stream: "combined", dataBase64: "aGk=", offset: 0, nextOffset: 2, eof: false, status: "running" }),
      frame("status", { jobId: "job_1", status: "completed", exitCode: 0 }),
    ]);
  });
  const bytes = [];
  for await (const e of client.streamLogs("sbx_1", "job_1")) bytes.push(...e.data);
  assert.equal(new TextDecoder().decode(new Uint8Array(bytes)), "hi");
  assert.equal(n, 2);
});

test("streamLogs: 403 stops without retry", async () => {
  let n = 0;
  const { client } = makeClient(() => { n++; return json({ error: "denied" }, 403); });
  const err = await (async () => { for await (const _ of client.streamLogs("sbx_1", "job_1")) { /* */ } })().catch((e) => e);
  assert.ok(err instanceof CanterAPIError && err.status === 403);
  assert.equal(n, 1);
});

test("streamLogs: valid output then malformed frame in the SAME feed delivers bytes then fails", async () => {
  const { client } = makeClient(() => sse([
    frame("output", { jobId: "job_1", stream: "combined", dataBase64: "b2s=", offset: 0, nextOffset: 2, eof: false, status: "running" }),
    frame("output", { jobId: "job_other", stream: "combined", dataBase64: "eA==", offset: 2, nextOffset: 3, eof: false, status: "running" }),
    frame("status", { jobId: "job_1", status: "completed", exitCode: 0 }),
  ]));
  const bytes = [];
  const err = await (async () => { for await (const e of client.streamLogs("sbx_1", "job_1")) bytes.push(...e.data); })().catch((e) => e);
  assert.ok(err instanceof SandboxProtocolError);
  assert.equal(new TextDecoder().decode(new Uint8Array(bytes)), "ok");
});

test("streamLogs: mismatched terminal job ID is fatal", async () => {
  const { client } = makeClient(() => sse([frame("status", { jobId: "job_other", status: "completed", exitCode: 0 })]));
  await assert.rejects(async () => { for await (const _ of client.streamLogs("sbx_1", "job_1")) { /* */ } }, SandboxProtocolError);
});

test("streamLogs: partial frame at EOF is dropped; reconnect resumes same offset", async () => {
  const offsets = [];
  const { client } = makeClient((url) => {
    offsets.push(new URL(url).searchParams.get("offset"));
    if (offsets.length === 1) {
      return sse(["event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"aGk=\",\"offset\":0,\"nextOff"]);
    }
    return sse([
      frame("output", { jobId: "job_1", stream: "combined", dataBase64: "aGk=", offset: 0, nextOffset: 2, eof: false, status: "running" }),
      frame("status", { jobId: "job_1", status: "completed", exitCode: 0 }),
    ]);
  });
  const bytes = [];
  for await (const e of client.streamLogs("sbx_1", "job_1")) bytes.push(...e.data);
  assert.equal(new TextDecoder().decode(new Uint8Array(bytes)), "hi");
  assert.deepEqual(offsets, ["0", "0"]);
});

test("streamLogs: >1MiB frame across many reads fails", async () => {
  const big = Buffer.alloc((1 << 20) + 10, 0x20);
  const { client } = makeClient(() => new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode("data: "));
      for (let i = 0; i < big.length; i += 4096) controller.enqueue(big.subarray(i, i + 4096));
      controller.close();
    },
  }), { headers: { "Content-Type": "text/event-stream" } }));
  await assert.rejects(async () => { for await (const _ of client.streamLogs("sbx_1", "job_1")) { /* */ } }, SandboxProtocolError);
});

test("streamLogs: split CRLF + UTF-8 + multiline data + extra field", async () => {
  const raw = 'event: output\r\ndata: {"jobId":"job_1","stream":"combined","dataBase64":"w6k=","offset":0,"nextOffset":2,"eof":false,"status":"run' +
    'ning","extra":true}\r\n\r\n';
  const { client } = makeClient(() => new Response(new ReadableStream({
    async start(controller) {
      const bytes = new TextEncoder().encode(raw);
      controller.enqueue(bytes.subarray(0, 40));
      controller.enqueue(bytes.subarray(40));
      controller.enqueue(new TextEncoder().encode(frame("status", { jobId: "job_1", status: "completed", exitCode: 0 })));
      controller.close();
    },
  }), { headers: { "Content-Type": "text/event-stream" } }));
  const bytes = [];
  for await (const e of client.streamLogs("sbx_1", "job_1")) bytes.push(...e.data);
  assert.equal(new TextDecoder().decode(new Uint8Array(bytes)), "é");
});

test("streamLogs: token callback resolved per reconnect", async () => {
  const tokens = [];
  let n = 0;
  const { client } = makeClient((url, init) => {
    tokens.push(init.headers.Authorization);
    n++;
    if (n === 1) return json({ error: "down" }, 503);
    return sse([frame("status", { jobId: "job_1", status: "completed", exitCode: 0 })]);
  }, { accessToken: () => `tok_${tokens.length}` });
  for await (const _ of client.streamLogs("sbx_1", "job_1")) { /* */ }
  assert.deepEqual(tokens, ["Bearer tok_0", "Bearer tok_1"]);
});

test("bounded JSON reader aborts >4MiB", async () => {
  const { client } = makeClient(() => new Response(new ReadableStream({
    start(controller) {
      for (let i = 0; i < 70; i++) controller.enqueue(new Uint8Array(65536).fill(65));
      controller.close();
    },
  }), { headers: { "Content-Type": "application/json" } }));
  await assert.rejects(client.list(), /exceeds/);
});

test("wait: deleting→deleted succeeds; cancelled wait makes no request", async () => {
  let n = 0;
  const { client } = makeClient(() => { n++; return json({ id: "sbx_1", phase: n >= 2 ? "deleted" : "deleting" }); });
  const v = await client.wait("sbx_1", "deleted", { timeoutMs: 10_000 });
  assert.equal(v.phase, "deleted");
  const before = n;
  const aborted = AbortSignal.abort();
  await assert.rejects(client.wait("sbx_1", "ready", { signal: aborted }), /aborted/);
  assert.equal(n, before); // preflight abort issues no request
  await assert.rejects(client.wait("sbx_1", "bogus"), TypeError);
});

test("upload: relative path normalized; bad offsets abort without complete", async () => {
  const payload = new Uint8Array((1 << 20) + 37).fill(7);
  const sha256 = "ab".repeat(32);
  let received = 0, aborted = false, completed = false, badOffset = false, beginPath;
  const { client } = makeClient(async (url, init) => {
    const path = new URL(url).pathname;
    if (init.method === "POST" && path.endsWith("/uploads")) {
      beginPath = JSON.parse(await new Response(init.body).text()).path;
      return json({ id: "upl_1", path: beginPath, size: payload.length, offset: 0, complete: false }, 201);
    }
    if (init.method === "PATCH") {
      const body = new Uint8Array(await new Response(init.body).arrayBuffer());
      received += body.length;
      const offset = badOffset ? received * 2 : received;
      return json({ id: "upl_1", path: beginPath, size: payload.length, offset, complete: false });
    }
    if (path.endsWith("/complete")) {
      completed = true;
      return json({ id: "upl_1", path: beginPath, size: payload.length, offset: payload.length, sha256, complete: true });
    }
    if (init.method === "DELETE") { aborted = true; return new Response("", { status: 204 }); }
    return new Response("", { status: 404 });
  });
  async function* chunks() { yield payload; }
  const result = await client.upload("sbx_1", "rel/f", { size: payload.length, sha256, chunks: chunks() });
  assert.equal(result.complete, true);
  assert.equal(beginPath, "/workspace/rel/f");
  assert.equal(received, payload.length);
  assert.equal(completed, true);
  assert.equal(aborted, false);
  // Bad chunk offset → abort, never complete.
  badOffset = true; received = 0; completed = false; aborted = false;
  async function* chunks2() { yield payload; }
  await assert.rejects(client.upload("sbx_1", "rel/f", { size: payload.length, sha256, chunks: chunks2() }), SandboxProtocolError);
  assert.equal(aborted, true);
  assert.equal(completed, false);
});

test("upload: unsafe begin ID never reaches abort route", async () => {
  const { client, calls } = makeClient(() =>
    json({ id: "../evil", path: "/workspace/f", size: 1, offset: 0, complete: false }, 201));
  async function* chunks() { yield new Uint8Array(1); }
  await assert.rejects(client.upload("sbx_1", "/workspace/f", { size: 1, sha256: "ab".repeat(32), chunks: chunks() }));
  assert.equal(calls.filter((c) => c.init.method === "DELETE").length, 0);
});

test("upload: bad final offset rejected", async () => {
  const payload = new Uint8Array(4);
  const { client } = makeClient(async (url, init) => {
    const path = new URL(url).pathname;
    if (init.method === "POST" && path.endsWith("/uploads")) return json({ id: "upl_1", path: "/workspace/f", size: 4, offset: 0, complete: false }, 201);
    if (init.method === "PATCH") return json({ id: "upl_1", path: "/workspace/f", size: 4, offset: 4, complete: false });
    if (path.endsWith("/complete")) return json({ id: "upl_1", path: "/workspace/f", size: 4, offset: 999, complete: true });
    return new Response("", { status: 204 });
  });
  async function* chunks() { yield payload; }
  await assert.rejects(client.upload("sbx_1", "/workspace/f", { size: 4, sha256: "ab".repeat(32), chunks: chunks() }), SandboxProtocolError);
});

test("download yields bytes and surfaces truncation", async () => {
  const payload = new Uint8Array(5000).fill(9);
  const { client } = makeClient((url) => {
    if (new URL(url).searchParams.get("path") === "/short") {
      return new Response(payload.slice(0, 10), { headers: { "Content-Length": String(payload.length) } });
    }
    return new Response(payload, { headers: { "Content-Length": String(payload.length) } });
  });
  const parts = [];
  for await (const chunk of client.download("sbx_1", "/workspace/f")) parts.push(chunk);
  assert.equal(parts.reduce((n, c) => n + c.length, 0), payload.length);
  await assert.rejects(async () => {
    for await (const _ of client.download("sbx_1", "/short")) { /* drain */ }
  }, /truncat/);
});

test("native redirect across two real servers never forwards credentials", async () => {
  let targetRequests = 0;
  const target = http.createServer((req, res) => { targetRequests++; res.end(); });
  await new Promise((r) => target.listen(0, "127.0.0.1", r));
  const origin = http.createServer((req, res) => {
    res.writeHead(302, { Location: `http://127.0.0.1:${target.address().port}/steal` });
    res.end();
  });
  await new Promise((r) => origin.listen(0, "127.0.0.1", r));
  try {
    const client = new SandboxClient({
      baseUrl: `http://127.0.0.1:${origin.address().port}`,
      workspaceId: "wrk_1", accessToken: "token_test",
    });
    await assert.rejects(client.list());
    assert.equal(targetRequests, 0);
  } finally {
    origin.close(); target.close();
  }
});

test("malformed base64 and null frame fail once, no retry", async () => {
  let n = 0;
  const { client } = makeClient(() => {
    n++;
    return sse([frame("output", { jobId: "job_1", stream: "combined", dataBase64: "!!!bad!!!", offset: 0, nextOffset: 1, eof: false, status: "running" })]);
  });
  await assert.rejects(async () => { for await (const _ of client.streamLogs("sbx_1", "job_1")) { /* */ } }, SandboxProtocolError);
  n = 0;
  const { client: c2 } = makeClient(() => {
    n++;
    return sse(["event: status\ndata: null\n\n"]);
  });
  await assert.rejects(async () => { for await (const _ of c2.streamLogs("sbx_1", "job_1")) { /* */ } }, SandboxProtocolError);
  assert.equal(n, 1);
});

test("empty and HTML 403 responses preserve status as CanterAPIError", async () => {
  const { client: empty } = makeClient(() => new Response("", { status: 403 }));
  const e1 = await empty.list().catch((e) => e);
  assert.ok(e1 instanceof CanterAPIError && e1.status === 403);
  const { client: html } = makeClient(() => new Response("<html>denied</html>", { status: 403 }));
  const e2 = await html.list().catch((e) => e);
  assert.ok(e2 instanceof CanterAPIError && e2.status === 403);
});

test("UTF-8 extra field split between bytes + multiline data", async () => {
  const encoder = new TextEncoder();
  const payload = frame("output", {
    jobId: "job_1", stream: "combined", dataBase64: "w6k=", offset: 0, nextOffset: 2,
    eof: false, status: "running", extra: "é",
  }) + frame("status", { jobId: "job_1", status: "completed", exitCode: 0 });
  const bytes = encoder.encode(payload);
  const { client } = makeClient(() => new Response(new ReadableStream({
    start(controller) {
      // Split inside the é's two-byte UTF-8 sequence.
      const splitAt = bytes.indexOf(0xc3);
      controller.enqueue(bytes.subarray(0, splitAt));
      controller.enqueue(bytes.subarray(splitAt, splitAt + 1));
      controller.enqueue(bytes.subarray(splitAt + 1));
      controller.close();
    },
  }), { headers: { "Content-Type": "text/event-stream" } }));
  const got = [];
  for await (const e of client.streamLogs("sbx_1", "job_1")) got.push(...e.data);
  assert.equal(new TextDecoder().decode(new Uint8Array(got)), "é");
});

test("early generator close cancels the source reader", async () => {
  let cancelled = false;
  const { client } = makeClient(() => new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(
        frame("output", { jobId: "job_1", stream: "combined", dataBase64: "eA==", offset: 0, nextOffset: 1, eof: false, status: "running" }),
      ));
      // Stream stays open — generator must cancel it on early return.
    },
    cancel() { cancelled = true; },
  }), { headers: { "Content-Type": "text/event-stream" } }));
  const gen = client.streamLogs("sbx_1", "job_1");
  const first = await gen.next();
  assert.equal(first.value.event, "output");
  await gen.return(undefined);
  assert.equal(cancelled, true);
});

test("progress survives five broken response bodies at exact delivered offsets", async () => {
  const offsets = [];
  const { client } = makeClient((url) => {
    const offset = Number(new URL(url).searchParams.get("offset"));
    offsets.push(offset);
    if (offset === 5) return sse([frame("status", { jobId: "job_1", status: "completed", exitCode: 0 })]);
    let sent = false;
    return new Response(new ReadableStream({
      pull(controller) {
        if (sent) {
          controller.error(new TypeError("connection reset"));
          return;
        }
        sent = true;
        controller.enqueue(new TextEncoder().encode(frame("output", {
          jobId: "job_1", stream: "combined", dataBase64: "eA==",
          offset, nextOffset: offset + 1, eof: false, status: "running",
        })));
      },
    }, { highWaterMark: 0 }), { headers: { "Content-Type": "text/event-stream" } });
  });
  const bytes = [];
  for await (const event of client.streamLogs("sbx_1", "job_1")) bytes.push(...event.data);
  assert.equal(new TextDecoder().decode(new Uint8Array(bytes)), "xxxxx");
  assert.deepEqual(offsets, [0, 1, 2, 3, 4, 5]);
});

test("idempotency keys with a trailing newline fail before a request", async () => {
  const { client, calls } = makeClient(() => json({}));
  await assert.rejects(client.create({ name: "build", size: "s-2x4", idempotencyKey: "valid-key\n" }), TypeError);
  assert.equal(calls.length, 0);
});

import zlib from "node:zlib";

test("wait: pending→running→succeeded setup gates ready", async () => {
  let n = 0;
  const { client } = makeClient(() => {
    n++;
    const setup = n === 1 ? { status: "pending" } : n === 2 ? { status: "running", jobId: "job_setup_1" } : { status: "succeeded" };
    return json({ id: "sbx_1", phase: "ready", setup });
  });
  const v = await client.wait("sbx_1", "ready", { timeoutMs: 10_000 });
  assert.equal(v.setup.status, "succeeded");
  const { client: failed } = makeClient(() => json({ id: "sbx_1", phase: "ready", setup: { status: "failed", jobId: "job_setup_1", error: "boom" } }));
  await assert.rejects(failed.wait("sbx_1", "ready"), /job_setup_1/);
  const { client: unknown } = makeClient(() => json({ id: "sbx_1", phase: "ready", setup: { status: "bogus" } }));
  await assert.rejects(unknown.wait("sbx_1", "ready"), SandboxProtocolError);
});

test("exec: validated receipt over the longer budget", async () => {
  const { client, calls } = makeClient((url, init) => {
    assert.equal(new URL(url).pathname, "/v1/workspaces/wrk_1/sandboxes/sbx_1/exec");
    return json({ jobId: "job_1", status: "running" }, 202);
  });
  const job = await client.exec("sbx_1", { command: "echo hi" });
  assert.equal(job.jobId, "job_1");
  const { client: bad } = makeClient(() => json({ jobId: "", status: "nope" }, 202));
  await assert.rejects(bad.exec("sbx_1", { command: "x" }), SandboxProtocolError);
});

test("gzip download: decoded bytes, encoded Content-Length", async () => {
  const payload = new Uint8Array(5000).fill(7);
  const compressed = zlib.gzipSync(Buffer.from(payload));
  const server = http.createServer((req, res) => {
    res.writeHead(200, { "Content-Type": "application/octet-stream", "Content-Encoding": "gzip", "Content-Length": String(compressed.length) });
    res.end(compressed);
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  try {
    const client = new SandboxClient({ baseUrl: `http://127.0.0.1:${server.address().port}`, workspaceId: "wrk_1", accessToken: "t" });
    const parts = [];
    for await (const chunk of client.download("sbx_1", "/workspace/f")) parts.push(chunk);
    const got = Buffer.concat(parts.map(p => Buffer.from(p)));
    assert.deepEqual(new Uint8Array(got), payload);
  } finally { server.close(); }
});

test("nested API error yields the real message", async () => {
  const { client } = makeClient(() => json({ error: { message: "quota exceeded" } }, 409));
  const err = await client.create({ name: "x", size: "s-1x2", idempotencyKey: "key-1234" }).catch(e => e);
  assert.ok(err instanceof CanterAPIError);
  assert.match(err.message, /quota exceeded/);
});

test("newline token produces a fixed error before any request", async () => {
  const { client, calls } = makeClient(() => json({}), { accessToken: "tok\nbad" });
  await assert.rejects(client.list(), TypeError);
  assert.equal(calls.length, 0);
});

import test from "node:test";
import assert from "node:assert/strict";

// Import the REAL production utility the pages call — not a reimplemented parser.
const client = await import("../src/lib/sandbox-client.ts");

const sse = (frames) => new Response(new ReadableStream({
  start(c) { c.enqueue(new TextEncoder().encode(frames)); c.close(); }
}), { status: 200 });
const frame = (event, data) => `event: ${event}\ndata: ${data}\n\n`;

test("sandboxAPIURL prefixes raw fetches with /api/canter", () => {
  assert.equal(client.sandboxAPIURL("/w/s/files/content?path=/x"), "/api/canter/w/s/files/content?path=/x");
  assert.equal(client.sandboxDownloadURL("/w/s", "/workspace/a.txt"), "/api/canter/w/s/files/content?path=%2Fworkspace%2Fa.txt");
});

test("followJob reconnects at the previous offset after a non-terminal EOF", async () => {
  const requests = [];
  const fakeFetch = async (url) => {
    requests.push(url);
    if (requests.length === 1) {
      return sse(frame("output", JSON.stringify({ dataBase64: btoa("hello"), nextOffset: 5, status: "running" })));
    }
    return sse(frame("status", JSON.stringify({ status: "completed", exitCode: 0 })));
  };
  const original = globalThis.fetch;
  globalThis.fetch = fakeFetch;
  try {
    const states = [];
    client.followJob("/w/s", "job_x", s => states.push({ ...s }));
    await new Promise(r => setTimeout(r, 900));
    assert.equal(states.at(-1).status, "completed");
    // second request resumed at the recorded offset, not 0
    assert.match(requests[1], /offset=5/);
  } finally { globalThis.fetch = original; }
});

test("followJob decodes UTF-8 split across SEPARATE requests", async () => {
  const bytes = new TextEncoder().encode("héllo");
  const p1 = btoa(String.fromCharCode(...bytes.slice(0, 2)));
  const p2 = btoa(String.fromCharCode(...bytes.slice(2)));
  const responses = [
    sse(frame("output", JSON.stringify({ dataBase64: p1, nextOffset: 2, status: "running" }))),
    sse(frame("output", JSON.stringify({ dataBase64: p2, nextOffset: bytes.length, status: "running" })) + frame("status", JSON.stringify({ status: "completed", exitCode: 0 }))),
  ];
  let i = 0;
  const fakeFetch = async () => responses[Math.min(i++, responses.length - 1)];
  const original = globalThis.fetch;
  globalThis.fetch = fakeFetch;
  try {
    const states = [];
    client.followJob("/w/s", "job_x", s => states.push({ ...s }));
    await new Promise(r => setTimeout(r, 900));
    assert.equal(states.at(-1).text, "héllo");
    assert.equal(states.at(-1).status, "completed");
  } finally { globalThis.fetch = original; }
});

test("followJob preserves the truncated flag through the terminal event", async () => {
  const responses = [
    sse(frame("output", JSON.stringify({ dataBase64: btoa("x"), nextOffset: 1, logTruncated: true, status: "running" }))),
    sse(frame("status", JSON.stringify({ status: "completed", exitCode: 0 }))),
  ];
  let i = 0;
  const fakeFetch = async () => responses[Math.min(i++, responses.length - 1)];
  const original = globalThis.fetch;
  globalThis.fetch = fakeFetch;
  try {
    const states = [];
    client.followJob("/w/s", "job_x", s => states.push({ ...s }));
    await new Promise(r => setTimeout(r, 900));
    assert.equal(states.at(-1).status, "completed");
    assert.equal(states.at(-1).truncated, true);
  } finally { globalThis.fetch = original; }
});

test("followJob stops on nonretryable 403 without looping", async () => {
  let calls = 0;
  const fakeFetch = async () => { calls += 1; return new Response(JSON.stringify({ error: { message: "forbidden" } }), { status: 403 }); };
  const original = globalThis.fetch;
  globalThis.fetch = fakeFetch;
  try {
    const states = [];
    client.followJob("/w/s", "job_x", s => states.push({ ...s }));
    await new Promise(r => setTimeout(r, 300));
    assert.equal(states.at(-1).status, "running"); // a log error does not stop the job
    assert.equal(states.at(-1).error, "forbidden");
    assert.equal(calls, 1);
  } finally { globalThis.fetch = original; }
});

test("followJob aborts cleanly during backoff without further requests", async () => {
  let calls = 0;
  const fakeFetch = async () => { calls += 1; return new Response("", { status: 500 }); };
  const original = globalThis.fetch;
  globalThis.fetch = fakeFetch;
  try {
    const stop = client.followJob("/w/s", "job_x", () => {});
    await new Promise(r => setTimeout(r, 50));
    stop();
    const after = calls;
    await new Promise(r => setTimeout(r, 1000));
    assert.equal(calls, after); // no requests after abort during backoff
  } finally { globalThis.fetch = original; }
});

test("runUpload uses API limits and cleans up on abort", async () => {
  const calls = [];
  const abort = new AbortController();
  const fakeFetch = async (url, init) => {
    calls.push({ url, method: init.method, signal: init.signal, size: init.body?.size });
    if (init.method === "POST" && url.endsWith("/uploads")) {
      return new Response(JSON.stringify({ id: "upl_test0001", path: "/w/a", size: 8, offset: 0, complete: false }), { status: 201 });
    }
    if (init.method === "PATCH") {
      // Second chunk hangs until aborted — abort must fire cleanup mid-upload.
      if (calls.filter(c => c.method === "PATCH").length > 1) {
        return new Promise((_, reject) => {
          const signal = init.signal;
          if (signal) signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
          queueMicrotask(() => abort.abort());
        });
      }
      return new Response(JSON.stringify({ id: "upl_test0001", path: "/w/a", size: 8, offset: 4, complete: false }));
    }
    if (init.method === "DELETE") return new Response(null, { status: 204 });
    return new Response("{}", { status: 200 });
  };
  const original = globalThis.fetch;
  globalThis.fetch = fakeFetch;
  try {
    const progress = [];
    // 8-byte file with a 4-byte API chunk limit → first chunk ok, second hangs.
    const file = new Blob(["abcdefgh"]);
    await assert.rejects(client.runUpload("/w/s", "/w/a", file, { maxFileBytes: 1 << 20, uploadChunkBytes: 4 }, p => progress.push(p), abort.signal));
    assert.deepEqual(progress, [{ done: 4, size: 8 }]);
    assert.deepEqual(calls.filter(c => c.method === "PATCH").map(c => c.size), [4, 4]);
    assert.ok(calls.some(c => c.method === "DELETE"), "cleanup DELETE sent after abort");
    assert.equal(calls.find(c => c.method === "DELETE").signal.aborted, false);
  } finally { globalThis.fetch = original; }
});

test("JSON labels preserve spaces and reject non-string values", () => {
  assert.deepEqual(client.parseSandboxLabels('{"team":"platform team","empty":""}'), { team: "platform team", empty: "" });
  assert.deepEqual(client.parseSandboxLabels(""), {});
  for (const raw of ['team=x', '["x"]', '{"x":1}', '{"x":"line\\nbreak"}', 'null']) {
    assert.throws(() => client.parseSandboxLabels(raw));
  }
});

test("followJob rejects a mismatched cursor without accepting its bytes", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = async () => sse(frame("output", JSON.stringify({ dataBase64: btoa("hello"), nextOffset: 99 })));
  let stop = () => {};
  try {
    const result = await new Promise(resolve => {
      stop = client.followJob("/w/s", "job_cursor", state => { if (state.error) resolve(state); });
    });
    assert.match(result.error, /offset/);
    assert.equal(result.offset, 0);
    assert.equal(result.text, "");
  } finally { stop(); globalThis.fetch = original; }
});

test("empty successful log responses exhaust a bounded retry budget", async () => {
  const originalFetch = globalThis.fetch;
  const originalTimeout = globalThis.setTimeout;
  let requests = 0;
  globalThis.fetch = async () => { requests++; return sse(""); };
  globalThis.setTimeout = (callback, milliseconds, ...args) => originalTimeout(callback, Math.min(milliseconds, 1), ...args);
  let stop = () => {};
  try {
    const result = await new Promise((resolve, reject) => {
      const watchdog = originalTimeout(() => reject(new Error("retry loop did not stop")), 5000);
      stop = client.followJob("/w/s", "job_empty", state => {
        if (state.error) { clearTimeout(watchdog); resolve(state); }
      });
    });
    assert.equal(requests, 6);
    assert.match(result.error, /disconnected/);
  } finally { stop(); globalThis.fetch = originalFetch; globalThis.setTimeout = originalTimeout; }
});

test("rendered Unicode logs stay within the byte budget", async () => {
  const data = Buffer.from("é".repeat(65536));
  let events = "";
  for (let i = 1; i <= 9; i++) events += frame("output", JSON.stringify({ dataBase64: data.toString("base64"), nextOffset: data.length*i }));
  events += frame("status", JSON.stringify({ status: "completed" }));
  const original = globalThis.fetch;
  globalThis.fetch = async () => sse(events);
  let stop = () => {};
  try {
    const result = await new Promise(resolve => {
      stop = client.followJob("/w/s", "job_budget", state => { if (state.status === "completed") resolve(state); });
    });
    assert.ok(Buffer.byteLength(result.text) <= 1 << 20);
    assert.equal(result.truncated, true);
    assert.ok(!result.text.includes("�"));
  } finally { stop(); globalThis.fetch = original; }
});

test("an upload cancelled during begin cleans up the late receipt", async () => {
  const original = globalThis.fetch;
  let finishBegin;
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, ...init });
    if (init.method === "POST") return new Promise(resolve => { finishBegin = resolve; });
    return new Response(null, { status: 204 });
  };
  try {
    const abort = new AbortController();
    const work = client.runUpload("/w/s", "/workspace/a", new Blob(["x"]), { maxFileBytes: 10, uploadChunkBytes: 4 }, () => {}, abort.signal);
    abort.abort();
    assert.equal(calls[0].signal.aborted, false);
    finishBegin(new Response(JSON.stringify({ id: "upl_late0001", path: "/workspace/a", size: 1, offset: 0, complete: false }), { status: 201 }));
    await assert.rejects(work);
    assert.equal(calls.filter(c => c.method === "DELETE").length, 1);
    assert.equal(calls.find(c => c.method === "DELETE").signal.aborted, false);
    assert.equal(calls.filter(c => c.method === "PATCH").length, 0);
  } finally { globalThis.fetch = original; }
});

test("a failed chunk removes staging without completing the upload", async () => {
  const original = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, method: init.method });
    if (init.method === "POST") return new Response(JSON.stringify({ id: "upl_error001", path: "/workspace/a", size: 1, offset: 0, complete: false }), { status: 201 });
    if (init.method === "PATCH") return new Response(JSON.stringify({ error: "disk full" }), { status: 409 });
    return new Response(null, { status: 204 });
  };
  try {
    await assert.rejects(client.runUpload("/w/s", "/workspace/a", new Blob(["x"]), { maxFileBytes: 10, uploadChunkBytes: 4 }, () => {}), /disk full/);
    assert.equal(calls.filter(c => c.method === "DELETE").length, 1);
    assert.ok(!calls.some(c => c.url.endsWith("/complete")));
  } finally { globalThis.fetch = original; }
});

// Helper eligibility for the saved-environments library.
const sandboxesLib = await import("../src/lib/sandboxes.ts");

test("compatibleSandboxSizes only accepts disks that fit the save", () => {
  const sizes = [
    { id: "free-1x1", vcpus: 1, memoryMiB: 1024, diskGiB: 20, free: true, monthlyCents: 0 },
    { id: "s-2x4", vcpus: 2, memoryMiB: 4096, diskGiB: 40, free: false, monthlyCents: 2400 },
    { id: "s-4x8", vcpus: 4, memoryMiB: 8192, diskGiB: 80, free: false, monthlyCents: 4800 },
  ];
  assert.deepEqual(sandboxesLib.compatibleSandboxSizes(sizes, 40).map(s => s.id), ["s-2x4", "s-4x8"]);
  assert.deepEqual(sandboxesLib.compatibleSandboxSizes(sizes, 20).map(s => s.id), ["free-1x1", "s-2x4", "s-4x8"]);
  assert.deepEqual(sandboxesLib.compatibleSandboxSizes(sizes, 81), []);
});

test("canCopySandboxSnapshot allows free current saves without billing", () => {
  const free = [{ id: "sbx_1", snapshotId: "snap_a", tier: "free", phase: "ready" }];
  const deleting = [{ id: "sbx_1", snapshotId: "snap_a", tier: "free", phase: "deleting" }];
  const paid = [{ id: "sbx_1", snapshotId: "snap_a", tier: "paid", phase: "ready" }];
  // Free current save is copyable with or without billing.
  assert.equal(sandboxesLib.canCopySandboxSnapshot("snap_a", false, free), true);
  assert.equal(sandboxesLib.canCopySandboxSnapshot("snap_a", true, free), true);
  // Retained/extra history still needs active billing.
  assert.equal(sandboxesLib.canCopySandboxSnapshot("snap_b", false, free), false);
  assert.equal(sandboxesLib.canCopySandboxSnapshot("snap_b", true, free), true);
  // A paid machine's reference doesn't make it free-included.
  assert.equal(sandboxesLib.canCopySandboxSnapshot("snap_a", false, paid), false);
  // A deleting machine's reference doesn't count either.
  assert.equal(sandboxesLib.canCopySandboxSnapshot("snap_a", false, deleting), false);
});

// Desktop eligibility: >=2 vCPU AND >=4 GiB RAM.
test("sandboxDesktopEligible requires 2 vCPU and 4 GiB", async () => {
	const { sandboxDesktopEligible } = await import("../src/lib/sandboxes.ts");
	assert.equal(sandboxDesktopEligible({ vcpus: 1, memoryMiB: 4096 }), false);
	assert.equal(sandboxDesktopEligible({ vcpus: 2, memoryMiB: 2048 }), false);
	assert.equal(sandboxDesktopEligible({ vcpus: 2, memoryMiB: 4096 }), true);
	assert.equal(sandboxDesktopEligible(undefined), false);
});

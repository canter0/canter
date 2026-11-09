// Production sandbox client utilities: every raw fetch the UI makes to the
// sandbox REST surface goes through these so tests exercise the real paths.

export function sandboxAPIURL(path: string) {
  return `/api/canter${path}`;
}

export type JobLogChunk = {
  jobId: string; stream: string; dataBase64: string;
  offset: number; nextOffset: number; eof: boolean;
  status: string; exitCode?: number; logTruncated?: boolean;
};
export type JobView = { jobId: string; status: string; exitCode?: number; error?: string; logTruncated?: boolean };
export type FollowState = { text: string; offset: number; status: string; exitCode?: number; truncated: boolean; error?: string };
type FollowUpdate = (state: FollowState) => void;

const RENDER_BUDGET = 1 << 20; // keep at most the last 1MiB of rendered log text.
const MAX_ATTEMPTS = 5;

class SandboxProtocolError extends Error {}

function delay(ms: number, signal: AbortSignal) {
  return new Promise<void>(resolve => {
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const timer = setTimeout(finish, ms);
    signal.addEventListener("abort", finish, { once: true });
    if (signal.aborted) finish();
  });
}

// followJob consumes the resumable SSE feed for a sandbox job. It decodes
// inner base64 bytes through a streaming TextDecoder kept across reconnects,
// reconnects with bounded backoff (reset only on progress), waits out a
// non-terminal EOF before reconnecting, and surfaces nonretryable errors.
// The truncated flag survives the terminal event; the backend emits the final
// status only after recorded output is fully drained.
export function followJob(baseURL: string, jobId: string, onUpdate: FollowUpdate, signal?: AbortSignal): () => void {
  const abort = new AbortController();
  const inner = new TextDecoder();   // job output bytes, kept across reconnects
  let offset = 0;
  let rendered = "";
  let truncated = false;
  let attempts = 0;
  let alive = true;
  let lastStatus = "running";
  const stop = () => { alive = false; abort.abort(); };
  signal?.addEventListener("abort", stop, { once: true });
  if (signal?.aborted) stop();
  const emit = (patch: Partial<FollowState>) => {
    if (!alive) return;
    const base: FollowState = { text: rendered, offset, status: lastStatus, truncated };
    onUpdate({ ...base, ...patch });
  };
  const append = (text: string) => {
    rendered += text;
    const bytes = new TextEncoder().encode(rendered);
    if (bytes.length > RENDER_BUDGET) {
      let start = bytes.length - RENDER_BUDGET;
      while ((bytes[start] & 0xc0) === 0x80) start++;
      rendered = new TextDecoder().decode(bytes.subarray(start));
      truncated = true;
    }
  };

  async function read() {
    while (alive) {
      let madeProgress = false;
      let keepalive = false;
      const started = Date.now();
      let failure = "Log stream disconnected.";
      try {
        const response = await fetch(sandboxAPIURL(`${baseURL}/jobs/${encodeURIComponent(jobId)}/events?stream=combined&offset=${offset}`), {
          signal: abort.signal, credentials: "include", cache: "no-store",
        });
        if (!response.ok || !response.body) {
          const message = await errorText(response, "Log stream failed.");
          const retryable = response.status === 0 || response.status >= 500 || response.status === 429;
          if (!retryable) {
            emit({ error: message });
            stop();
            return;
          }
          throw new Error(message);
        }
        const reader = response.body.getReader();
        const outer = new TextDecoder(); // partial SSE frames are not resumed
        let buffer = "";
        try {
          while (alive) {
            const chunk = await reader.read();
            if (chunk.done) break;
            buffer += outer.decode(chunk.value, { stream: true });
            buffer = buffer.replace(/\r\n/g, "\n");
            const blocks = buffer.split("\n\n");
            buffer = blocks.pop() ?? "";
            if (buffer.length > 1 << 20) throw new SandboxProtocolError("Oversized log stream frame.");
            for (const block of blocks) {
              let event = "", data = "";
              for (const line of block.split("\n")) {
                if (line.startsWith(":")) keepalive = true;
                if (line.startsWith("event: ")) event = line.slice(7);
                else if (line.startsWith("data: ")) data += line.slice(6);
              }
              if (!data) continue;
              let payload: { dataBase64?: string; nextOffset?: number; logTruncated?: boolean; status?: string; exitCode?: number };
              try { payload = JSON.parse(data); } catch { throw new SandboxProtocolError("Invalid log stream data."); }
              if (event === "output" && typeof payload.dataBase64 === "string") {
                const bytes = Uint8Array.from(atob(payload.dataBase64), c => c.charCodeAt(0));
                const expected = offset + bytes.length;
                if (!Number.isSafeInteger(payload.nextOffset) || payload.nextOffset !== expected || bytes.length > 262144) {
                  throw new SandboxProtocolError("Invalid log offset from sandbox.");
                }
                offset = expected;
                madeProgress ||= bytes.length > 0;
                append(inner.decode(bytes, { stream: true }));
                if (payload.status) lastStatus = payload.status;
                if (payload.logTruncated) truncated = true;
                emit({});
              } else if (event === "status" && payload.status) {
                madeProgress ||= lastStatus !== payload.status;
                keepalive = true;
                lastStatus = payload.status;
                truncated ||= Boolean(payload.logTruncated);
                if (payload.status !== "starting" && payload.status !== "running") {
                  append(inner.decode());
                  emit({ status: payload.status, exitCode: payload.exitCode });
                  stop();
                  return;
                }
                emit({});
              }
            }
          }
        } finally {
          await reader.cancel().catch(() => {});
          try { reader.releaseLock(); } catch { /* already released */ }
        }
      } catch (cause) {
        if (!alive || abort.signal.aborted) return;
        failure = cause instanceof Error ? cause.message : failure;
        if (cause instanceof SandboxProtocolError) {
          emit({ error: failure });
          stop();
          return;
        }
      }
      if (!alive) return;
      // A quiet, healthy 25s feed may contain only heartbeats. Empty or
      // immediately closed responses must not cause an endless retry loop.
      attempts = madeProgress || (keepalive && Date.now() - started >= 1000) ? 0 : attempts + 1;
      if (attempts > MAX_ATTEMPTS) {
        emit({ error: failure });
        stop();
        return;
      }
      await delay(Math.max(500, Math.min(500 * attempts, 3000)), abort.signal);
    }
  }
  void read().finally(() => signal?.removeEventListener("abort", stop));
  return stop;
}

export type UploadReceipt = { id: string; path: string; size: number; offset: number; complete: boolean };

// uploadFile coordinates a chunked upload with local per-operation state — the
// caller's abort only cancels this upload, and cleanup uses a fresh request.
export type UploadLimits = { maxFileBytes: number; uploadChunkBytes: number };
export type UploadProgress = { done: number; size: number };

export async function beginUpload(baseURL: string, path: string, size: number, sha256?: string, signal?: AbortSignal): Promise<UploadReceipt> {
  const response = await fetch(sandboxAPIURL(`${baseURL}/uploads`), {
    method: "POST", credentials: "include", cache: "no-store", signal,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path, size, sha256 }),
  });
  if (!response.ok) throw new Error(await errorText(response, "Upload couldn't start."));
  return response.json();
}

export async function uploadChunk(baseURL: string, id: string, offset: number, data: Blob, signal?: AbortSignal): Promise<UploadReceipt> {
  const response = await fetch(sandboxAPIURL(`${baseURL}/uploads/${encodeURIComponent(id)}?offset=${offset}`), {
    method: "PATCH", credentials: "include", cache: "no-store", signal,
    headers: { "Content-Type": "application/octet-stream" }, body: data,
  });
  if (!response.ok) throw new Error(await errorText(response, "Upload failed."));
  return response.json();
}

export async function completeUpload(baseURL: string, id: string, signal?: AbortSignal): Promise<UploadReceipt> {
  const response = await fetch(sandboxAPIURL(`${baseURL}/uploads/${encodeURIComponent(id)}/complete`), {
    method: "POST", credentials: "include", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(await errorText(response, "Upload couldn't finish."));
  return response.json();
}

// abortUpload uses a fresh request, never the cancelled signal, so cleanup
// still reaches the server after the UI aborts.
export async function abortUpload(baseURL: string, id: string): Promise<void> {
  try {
    await fetch(sandboxAPIURL(`${baseURL}/uploads/${encodeURIComponent(id)}`), {
      method: "DELETE", credentials: "include", cache: "no-store", keepalive: true,
      signal: AbortSignal.timeout(5000),
    });
  } catch { /* best-effort cleanup */ }
}

// runUpload is the testable upload coordinator: per-operation local state,
// API-provided chunk/file limits, fresh-signal cleanup on any failure path.
export async function runUpload(
  baseURL: string, path: string, file: Blob, limits: UploadLimits,
  onProgress: (p: UploadProgress) => void, signal?: AbortSignal,
): Promise<UploadReceipt> {
  if (!Number.isSafeInteger(limits.maxFileBytes) || limits.maxFileBytes < 0 ||
      !Number.isSafeInteger(limits.uploadChunkBytes) || limits.uploadChunkBytes <= 0) {
    throw new Error("Upload limits are unavailable. Refresh and try again.");
  }
  if (file.size > limits.maxFileBytes) {
    throw new Error(`File exceeds the ${formatSize(limits.maxFileBytes)} upload limit.`);
  }
  const chunkSize = limits.uploadChunkBytes;
  const local = { base: baseURL, id: "", abort: new AbortController() };
  const stop = () => local.abort.abort();
  signal?.addEventListener("abort", stop, { once: true });
  if (signal?.aborted) stop();
  try {
    local.abort.signal.throwIfAborted();
    // Let a bounded begin finish after UI cancellation, so its returned ID
    // can still be cleaned up instead of occupying a guest upload slot.
    const begin = await beginUpload(local.base, path, file.size, undefined, AbortSignal.timeout(120000));
    local.id = begin.id;
    if (!/^upl_[A-Za-z0-9_-]{8,32}$/.test(local.id) || begin.size !== file.size || begin.offset !== 0 || begin.complete) {
      throw new Error("Invalid upload receipt.");
    }
    local.abort.signal.throwIfAborted();
    for (let offset = 0; offset < file.size; offset += chunkSize) {
      local.abort.signal.throwIfAborted();
      const next = Math.min(offset + chunkSize, file.size);
      const receipt = await uploadChunk(local.base, local.id, offset, file.slice(offset, next), local.abort.signal);
      if (receipt.id !== local.id || receipt.offset !== next || receipt.size !== file.size || receipt.complete) {
        throw new Error("Invalid upload chunk receipt.");
      }
      local.abort.signal.throwIfAborted();
      onProgress({ done: next, size: file.size });
    }
    const done = await completeUpload(local.base, local.id, local.abort.signal);
    if (done.id !== local.id || !done.complete || done.offset !== file.size || done.size !== file.size) {
      throw new Error("Invalid completed upload receipt.");
    }
    return done;
  } catch (cause) {
    if (local.id) void abortUpload(local.base, local.id);
    throw cause;
  } finally {
    signal?.removeEventListener("abort", stop);
  }
}

export function parseSandboxLabels(raw: string): Record<string, string> {
  if (!raw.trim()) return {};
  let value: unknown;
  try { value = JSON.parse(raw); } catch { throw new Error("Labels must be a JSON object of string values."); }
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Labels must be a JSON object of string values.");
  const entries = Object.entries(value);
  if (entries.length > 16) throw new Error("Use at most 16 labels.");
  for (const [key, item] of entries) {
    if (!/^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,62}$/.test(key) || typeof item !== "string" ||
        /[\0\r\n]/.test(item) || new TextEncoder().encode(item).length > 128) {
      throw new Error("Use valid label names and string values of at most 128 bytes, without line breaks.");
    }
  }
  return value as Record<string, string>;
}

export function sandboxDownloadURL(baseURL: string, path: string): string {
  return sandboxAPIURL(`${baseURL}/files/content?path=${encodeURIComponent(path)}`);
}

function formatSize(bytes: number): string {
  if (bytes >= 1 << 30) return `${(bytes / (1 << 30)).toFixed(1)} GiB`;
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MiB`;
  return `${bytes} B`;
}

async function errorText(response: Response, fallback: string): Promise<string> {
  try {
    const body = await response.json();
    if (body && typeof body.error === "string") return body.error;
    if (body && body.error && typeof body.error.message === "string") return body.error.message;
  } catch { /* not json */ }
  return `${fallback} (${response.status})`;
}

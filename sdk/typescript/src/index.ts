/**
 * @canter/sandboxes — Canter sandbox REST client.
 *
 * Mirrors the public sandbox REST surface: listing, creation with a required
 * caller idempotency key, async jobs with a resumable SSE log feed, chunked
 * uploads, streamed downloads, lifecycle, saved environments and templates.
 * The access token is resolved per request through the supplied callback and
 * is never persisted or echoed into errors. Redirects are never followed —
 * a bearer token is only ever sent to the configured origin.
 */

export class CanterAPIError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(`Canter API returned ${status}: ${message}`);
    this.name = "CanterAPIError";
    this.status = status;
  }
}

/** Fatal protocol/receipt error — never retried by streamLogs. */
export class SandboxProtocolError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "SandboxProtocolError";
  }
}

export interface Sandbox {
  id: string;
  workspaceId: string;
  name: string;
  size: string;
  tier: string;
  phase: string;
  vcpus: number;
  memoryMiB: number;
  diskGiB: number;
  monthlyCents: number;
  failure?: string;
  deleteRequested: boolean;
  createdByKind: string;
  createdById?: string;
  createdByName?: string;
  lastUsedAt?: string;
  awakeSince?: string;
  hasCheckpoint: boolean;
  createdAt: string;
  updatedAt: string;
  generation: number;
  notice?: string;
  labels: Record<string, string>;
  idleTimeoutSeconds: number;
  snapshotId?: string;
  desktop: boolean;
  environment?: SandboxEnvironment;
  setup: SandboxSetup;
}

export interface SandboxRepository {
  url: string;
  ref?: string;
}

export interface SandboxEnvironment {
  repository?: SandboxRepository;
  setupCommand?: string;
  setupTimeoutSeconds?: number;
  env?: Record<string, string>;
  secretRefs?: Record<string, string>;
}

export interface SandboxSetup {
  status?: string;
  jobId?: string;
  startedAt?: string;
  exitCode?: number;
  error?: string;
}

export interface SandboxCreateInput {
  name: string;
  size: string;
  labels?: Record<string, string>;
  idleTimeoutSeconds?: number;
  idempotencyKey: string;
  snapshotId?: string;
  templateId?: string;
  templateVersion?: number;
  desktop?: boolean;
  environment?: SandboxEnvironment;
}

export interface SandboxExecInput {
  command: string;
  directory?: string;
  env?: Record<string, string>;
  secretRefs?: Record<string, string>;
  timeoutSeconds?: number;
}

export interface SandboxJob {
  jobId: string;
  status: string;
  updatedAt: string;
  exitCode?: number;
  error?: string;
  logTruncated?: boolean;
}

export interface SandboxLogEvent {
  event: string;
  jobId: string;
  stream: string;
  status: string;
  offset: number;
  nextOffset: number;
  data: Uint8Array;
  exitCode?: number;
  logTruncated: boolean;
}

export interface SandboxUpload {
  id: string;
  path: string;
  size: number;
  offset: number;
  sha256?: string;
  complete: boolean;
}

export interface SandboxSnapshotStats {
  format: string;
  includeBrowser: boolean;
  rawBytes: number;
  entries: number;
  compressedBytes: number;
  archiveSha256: string;
  runtimeSkipped: number;
}

export interface SandboxSnapshot {
  id: string;
  workspaceId: string;
  sourceSandboxId: string;
  sourceGeneration: number;
  sourceSize: string;
  sourceDiskGiB: number;
  name: string;
  state: string;
  retained: boolean;
  stats: SandboxSnapshotStats;
  storedBytes: number;
  failure?: string;
  createdAt: string;
  readyAt?: string;
}

export interface SandboxSnapshotPage {
  snapshots: SandboxSnapshot[];
  nextCursor?: string;
  historyEnabled: boolean;
}

export interface SandboxTemplateVersion {
  version: number;
  snapshotId: string;
  sourceSize: string;
  sourceDiskGiB: number;
  storedBytes: number;
  createdAt: string;
}

export interface SandboxTemplate {
  id: string;
  workspaceId: string;
  name: string;
  createdAt: string;
  updatedAt: string;
  latest?: SandboxTemplateVersion;
  versions?: SandboxTemplateVersion[];
}

export interface SandboxTemplateList {
  templates: SandboxTemplate[];
  historyEnabled: boolean;
}

export interface SandboxFreeUsage {
  usedSeconds: number;
  limitSeconds: number;
}

export interface SandboxSize {
  id: string;
  vcpus: number;
  memoryMiB: number;
  diskGiB: number;
  free: boolean;
  monthlyCents: number;
}

export interface SandboxLimits {
  maxCommandSeconds: number;
  maxFileBytes: number;
  uploadChunkBytes: number;
  defaultIdleSeconds: number;
  maxJobLogBytes: number;
}

export interface SandboxList {
  sandboxes: Sandbox[];
  enabled: boolean;
  previewsEnabled: boolean;
  freeUsage: SandboxFreeUsage;
  sizes: SandboxSize[];
  limits: SandboxLimits;
}

export interface SandboxClientOptions {
  baseUrl: string;
  workspaceId: string;
  accessToken: string | (() => string | Promise<string>);
  requestTimeoutMs?: number;
  fetch?: typeof fetch;
}

export interface RequestOptions {
  signal?: AbortSignal;
}

export interface WaitOptions extends RequestOptions {
  timeoutMs?: number;
}

export interface StreamLogsOptions extends RequestOptions {
  stream?: "combined" | "stdout" | "stderr";
  offset?: number;
}

export interface UploadOptions extends RequestOptions {
  size: number;
  sha256: string;
  chunks: AsyncIterable<Uint8Array>;
}

export interface SnapshotsOptions extends RequestOptions {
  source?: string;
  before?: string;
}

const ID_PATTERN = /^[A-Za-z0-9_-]{1,128}$/;
const IDEMPOTENCY_KEY = /^[\x20-\x7e]{8,128}$/;
const FULL_ID = /^[A-Za-z0-9_-]{1,128}$/;
const SHA256_HEX = /^[0-9a-f]{64}$/;
const MAX_JSON = 4 << 20;
const MAX_LOG_CHUNK = 262144;
const MAX_FRAME = 1 << 20;
const UPLOAD_CHUNK = 1 << 20;
const WAIT_PHASES = new Set(["ready", "asleep", "deleted"]);
const JOB_STATUSES = new Set(["starting", "running", "completed", "failed", "cancelled", "timed_out", "interrupted"]);
const JOB_ACTIVE = new Set(["starting", "running"]);

function checkId(label: string, id: unknown): string {
  // JS $ semantics allow a trailing newline — require an exact full match.
  if (typeof id !== "string" || ID_PATTERN.exec(id)?.[0] !== id) {
    throw new TypeError(`invalid ${label} for Canter sandbox path`);
  }
  return encodeURIComponent(id);
}

function checkTimeout(label: string, value: unknown, fallback: number): number {
  if (value === undefined) return fallback;
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    throw new TypeError(`${label} must be a finite positive number`);
  }
  return value;
}

function isLoopback(host: string): boolean {
  const name = host.toLowerCase().replace(/\.$/, "");
  if (name === "localhost" || name.endsWith(".localhost") || name === "[::1]") return true;
  const parts = name.split(".");
  return parts.length === 4 && parts[0] === "127" && parts.every((p) => /^\d{1,3}$/.test(p));
}

function decodeBase64(text: string): Uint8Array {
  if (typeof text !== "string" || text === "" || !/^[A-Za-z0-9+/]*={0,2}$/.test(text)) {
    throw new SandboxProtocolError("invalid sandbox log output encoding");
  }
  try {
    if (typeof atob === "function") {
      const binary = atob(text);
      const out = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
      return out;
    }
    return new Uint8Array(Buffer.from(text, "base64"));
  } catch {
    throw new SandboxProtocolError("invalid sandbox log output encoding");
  }
}

/** Reads a response body with a hard byte bound; cancels on overflow. */
async function boundedBody(response: Response, limit: number): Promise<Uint8Array> {
  if (!response.body) return new Uint8Array(0);
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      total += value.length;
      if (total > limit) {
        throw new SandboxProtocolError(`Canter API response exceeds ${limit} bytes`);
      }
      chunks.push(value);
    }
  } finally {
    try { await reader.cancel(); } catch { /* ignore */ }
    reader.releaseLock();
  }
  const out = new Uint8Array(total);
  let at = 0;
  for (const chunk of chunks) { out.set(chunk, at); at += chunk.length; }
  return out;
}

function parseJson<T>(raw: Uint8Array): T {
  try {
    return JSON.parse(new TextDecoder().decode(raw)) as T;
  } catch (err) {
    throw new SandboxProtocolError(`Canter API returned malformed JSON: ${(err as Error).message}`);
  }
}

function apiErrorMessage(raw: Uint8Array, fallback: string): string {
  try {
    const body = parseJson<{error?: unknown}>(raw);
    const detail = body?.error;
    if (typeof detail === "string") return detail;
    if (detail && typeof detail === "object" && "message" in detail && typeof detail.message === "string") return detail.message;
  } catch { /* non-JSON error responses retain their HTTP status */ }
  return fallback;
}

function abortable(delay: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(new Error("aborted")); return; }
    const timer = setTimeout(() => { cleanup(); resolve(); }, delay);
    const onAbort = () => { cleanup(); reject(new Error("aborted")); };
    const cleanup = () => { clearTimeout(timer); signal.removeEventListener("abort", onAbort); };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

function mergeSignals(signals: Array<AbortSignal | undefined>): AbortSignal {
  const live = signals.filter((s): s is AbortSignal => s !== undefined);
  if (live.length === 0) return AbortSignal.timeout(20_000);
  if (live.length === 1) return live[0];
  return AbortSignal.any(live);
}

export class SandboxClient {
  private readonly baseUrl: string;
  private readonly workspace: string;
  private readonly tokenSource: () => string | Promise<string>;
  private readonly timeoutMs: number;
  private readonly fetchFn: typeof fetch;

  constructor(options: SandboxClientOptions) {
    const raw = options.baseUrl.trim().replace(/\/+$/, "");
    const parsed = new URL(raw);
    if ((parsed.protocol !== "https:" && parsed.protocol !== "http:") || !parsed.host ||
        parsed.username !== "" || parsed.password !== "" || parsed.search !== "" || parsed.hash !== "") {
      throw new TypeError("Canter API URL must be an absolute http or https URL without credentials, query, or fragment");
    }
    if (parsed.protocol === "http:" && !isLoopback(parsed.hostname)) {
      throw new TypeError("Canter API URL must use https outside loopback development");
    }
    this.baseUrl = raw;
    this.workspace = checkId("workspace ID", options.workspaceId);
    this.tokenSource = typeof options.accessToken === "function" ? options.accessToken : () => options.accessToken as string;
    this.timeoutMs = checkTimeout("requestTimeoutMs", options.requestTimeoutMs, 20_000);
    this.fetchFn = options.fetch ?? fetch;
  }

  private async token(): Promise<string> {
    const token = await this.tokenSource();
    if (typeof token !== "string" || /^[\x21-\x7e]+$/.exec(token)?.[0] !== token) {
      throw new TypeError("the Canter access token provider must return a nonempty printable token without whitespace");
    }
    return token;
  }

  private base(): string {
    return `/v1/workspaces/${this.workspace}`;
  }

  private sandboxPath(id: string, suffix: string): string {
    return `${this.base()}/sandboxes/${checkId("sandbox ID", id)}${suffix}`;
  }

  private signal(options: RequestOptions | undefined, timeoutMs: number): AbortSignal {
    return mergeSignals([options?.signal, AbortSignal.timeout(timeoutMs)]);
  }

  private async request<T>(method: string, path: string, body?: unknown, options?: RequestOptions): Promise<T> {
    // Serialize BEFORE awaiting the token so a frozen create body cannot be
    // mutated by the caller while the token provider resolves.
    const payload = body !== undefined ? JSON.stringify(body) : null;
    const token = await this.token();
    const response = await this.fetchFn(this.baseUrl + path, {
      method,
      redirect: "manual",
      credentials: "omit",
      cache: "no-store",
      signal: this.signal(options, this.timeoutMs),
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${token}`,
        ...(payload !== null ? { "Content-Type": "application/json" } : {}),
      },
      body: payload,
    });
    const raw = await boundedBody(response, MAX_JSON);
    if (!response.ok) {
      let message = response.statusText;
      message = apiErrorMessage(raw, message);
      throw new CanterAPIError(response.status, message);
    }
    if (raw.length === 0) return undefined as T;
    return parseJson<T>(raw);
  }

  private async requestRaw(method: string, path: string, body: BodyInit | null, contentType: string, options?: RequestOptions): Promise<Uint8Array> {
    const token = await this.token();
    const response = await this.fetchFn(this.baseUrl + path, {
      method,
      redirect: "manual",
      credentials: "omit",
      cache: "no-store",
      signal: this.signal(options, this.timeoutMs),
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${token}`,
        ...(contentType ? { "Content-Type": contentType } : {}),
      },
      body,
    });
    const raw = await boundedBody(response, MAX_JSON);
    if (!response.ok) {
      let message = response.statusText;
      message = apiErrorMessage(raw, message);
      throw new CanterAPIError(response.status, message);
    }
    return raw;
  }

  private async requestJson<T>(method: string, path: string, body: BodyInit | null, contentType: string, options?: RequestOptions): Promise<T> {
    return parseJson<T>(await this.requestRaw(method, path, body, contentType, options));
  }

  async list(options?: RequestOptions): Promise<SandboxList> {
    const result = await this.request<SandboxList>("GET", `${this.base()}/sandboxes`, undefined, options);
    if (!Array.isArray(result.sandboxes)) throw new SandboxProtocolError("invalid sandbox list receipt");
    for (const item of result.sandboxes) {
      if (typeof item.id !== "string" || !item.id || typeof item.phase !== "string" || !item.phase) {
        throw new SandboxProtocolError("invalid sandbox list receipt");
      }
    }
    return result;
  }

  async create(input: SandboxCreateInput, options?: RequestOptions): Promise<Sandbox> {
    if (typeof input.idempotencyKey !== "string" || IDEMPOTENCY_KEY.exec(input.idempotencyKey)?.[0] !== input.idempotencyKey) {
      throw new TypeError("sandbox create requires an idempotency key of 8-128 printable ASCII characters");
    }
    const result = await this.request<Sandbox>("POST", `${this.base()}/sandboxes`, input, options);
    if (typeof result.id !== "string" || !result.id || typeof result.phase !== "string" || !result.phase) {
      throw new SandboxProtocolError("invalid sandbox create receipt");
    }
    return result;
  }

  async get(id: string, options?: RequestOptions): Promise<Sandbox> {
    const result = await this.request<Sandbox>("GET", this.sandboxPath(id, ""), undefined, options);
    if (result.id !== id) throw new SandboxProtocolError("sandbox receipt ID does not match request");
    return result;
  }

  async wait(id: string, phase: string, options?: WaitOptions): Promise<Sandbox> {
    if (!WAIT_PHASES.has(phase)) {
      throw new TypeError(`invalid wait phase "${phase}"; expected ready, asleep, or deleted`);
    }
    const timeoutMs = checkTimeout("timeoutMs", options?.timeoutMs, 15 * 60_000);
    const signal = this.signal(options, timeoutMs);
    if (signal.aborted) throw new Error("wait aborted");
    let lastPhase = "unknown", lastSetup = "none", lastJob = "";
    try {
    for (;;) {
      const v = await this.request<Sandbox>("GET", this.sandboxPath(id, ""), undefined, { signal });
      if (!v || typeof v !== "object" || typeof v.phase !== "string" || !v.phase) {
        throw new SandboxProtocolError("sandbox receipt missing phase");
      }
      if (v.id !== id) throw new SandboxProtocolError("sandbox receipt ID does not match request");
      if (v.setup != null && (typeof v.setup !== "object" || Array.isArray(v.setup))) throw new SandboxProtocolError("invalid sandbox setup receipt");
      lastPhase = v.phase; lastSetup = v.setup?.status ?? "none"; lastJob = v.setup?.jobId ?? "";
      if (v.phase === "ready" && phase === "ready" && (v.setup?.status === "pending" || v.setup?.status === "running")) {
        // Setup still running — not ready yet.
      } else if (v.setup?.status === "failed" && phase === "ready") {
        throw new Error(`sandbox ${id} setup failed (job ${v.setup.jobId ?? "unknown"}): ${v.setup.error ?? ""}`);
      } else if (v.setup?.status !== undefined && !["pending", "running", "succeeded", "failed", ""].includes(v.setup.status)) {
        throw new SandboxProtocolError(`sandbox ${id} reported unrecognized setup status "${v.setup.status}"`);
      }
      if (v.phase === phase && !(v.phase === "ready" && (v.setup?.status === "pending" || v.setup?.status === "running"))) return v;
      if (v.phase === "failed" || v.phase === "save_failed") {
        throw new Error(`sandbox ${id} reached terminal phase "${v.phase}" while waiting for "${phase}": ${v.failure ?? ""}`);
      }
      if ((v.phase === "deleting" || v.phase === "deleted") && phase !== "deleted") {
        throw new Error(`sandbox ${id} reached terminal phase "${v.phase}" while waiting for "${phase}"`);
      }
      if (!["queued", "provisioning", "waking", "ready", "sleeping", "asleep", "deleting", "deleted"].includes(v.phase)) {
        throw new SandboxProtocolError(`sandbox ${id} reported unrecognized phase "${v.phase}"`);
      }
      await abortable(2000, signal);
    }
    } catch (error) {
      if (signal.aborted) throw new Error(`waiting for sandbox ${id} phase "${phase}" aborted (last phase "${lastPhase}", setup "${lastSetup}", job "${lastJob}")`, {cause: error});
      throw error;
    }
  }

  /**
   * Starts a job synchronously: the server waits up to 30 seconds before
   * returning a possibly-running job. Uses the 35-second streamed HTTP
   * budget; the caller signal still wins. No retries.
   */
  async exec(id: string, input: SandboxExecInput, options?: RequestOptions): Promise<SandboxJob> {
    const path = this.sandboxPath(id, "/exec");
    const payload = JSON.stringify(input);
    const token = await this.token();
    const response = await this.fetchFn(this.baseUrl + path, {
      method: "POST",
      redirect: "manual",
      credentials: "omit",
      cache: "no-store",
      signal: this.signal(options, 35_000),
      headers: { Accept: "application/json", Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      body: payload,
    });
    const raw = await boundedBody(response, MAX_JSON);
    if (!response.ok) {
      let message = response.statusText;
      message = apiErrorMessage(raw, message);
      throw new CanterAPIError(response.status, message);
    }
    const job = parseJson<SandboxJob>(raw);
    if (!job || typeof job !== "object" || typeof job.jobId !== "string" || ID_PATTERN.exec(job.jobId)?.[0] !== job.jobId || !JOB_STATUSES.has(job.status)) {
      throw new SandboxProtocolError("invalid sandbox exec receipt");
    }
    return job;
  }

  async startJob(id: string, input: SandboxExecInput, options?: RequestOptions): Promise<SandboxJob> {
    const result = await this.request<SandboxJob>("POST", this.sandboxPath(id, "/jobs"), input, options);
    if (typeof result.jobId !== "string" || !result.jobId || typeof result.status !== "string" || !JOB_STATUSES.has(result.status)) {
      throw new SandboxProtocolError("invalid sandbox job receipt");
    }
    return result;
  }

  async job(id: string, jobId: string, options?: RequestOptions): Promise<SandboxJob> {
    const result = await this.request<SandboxJob>("GET", this.sandboxPath(id, `/jobs/${checkId("job ID", jobId)}`), undefined, options);
    if (result.jobId !== jobId || !JOB_STATUSES.has(result.status)) {
      throw new SandboxProtocolError("invalid sandbox job receipt");
    }
    return result;
  }

  async stopJob(id: string, jobId: string, options?: RequestOptions): Promise<SandboxJob> {
    const result = await this.request<SandboxJob>("POST", this.sandboxPath(id, `/jobs/${checkId("job ID", jobId)}/stop`), {}, options);
    if (result.jobId !== jobId || !JOB_STATUSES.has(result.status)) {
      throw new SandboxProtocolError("invalid sandbox job receipt");
    }
    return result;
  }

  /**
   * Streams job log events, reconnecting at the exact delivered offset after
   * a normal feed end, a transport failure, or 429/5xx — at most three
   * consecutive attempts without progress. Ends when the job reaches a
   * terminal status after all delivered output. Protocol errors are fatal.
   */
  async *streamLogs(id: string, jobId: string, options?: StreamLogsOptions): AsyncGenerator<SandboxLogEvent> {
    const stream = options?.stream ?? "combined";
    if (!["combined", "stdout", "stderr"].includes(stream)) {
      throw new TypeError(`invalid sandbox log stream "${stream}"`);
    }
    let delivered = options?.offset ?? 0;
    if (typeof delivered !== "number" || !Number.isSafeInteger(delivered) || delivered < 0) {
      throw new TypeError(`invalid sandbox log offset ${delivered}`);
    }
    const path = this.sandboxPath(id, `/jobs/${checkId("job ID", jobId)}/events`);
    const callerSignal = options?.signal;
    let noProgress = 0;
    let lastStatus = "";
    const backoff = async () => {
      await abortable(Math.min(200 * Math.max(noProgress, 1), 600), mergeSignals([callerSignal, AbortSignal.timeout(35_000)]));
    };
    for (;;) {
      // Resolve the token per connection so a refresh provider takes effect.
      const token = await this.token();
      const query = `stream=${stream}&offset=${delivered}`;
      const feedStart = Date.now();
      let response: Response;
      try {
        response = await this.fetchFn(`${this.baseUrl}${path}?${query}`, {
          method: "GET",
          redirect: "manual",
          credentials: "omit",
          cache: "no-store",
          signal: mergeSignals([callerSignal, AbortSignal.timeout(35_000)]),
          headers: { Accept: "text/event-stream", Authorization: `Bearer ${token}` },
        });
      } catch (err) {
        if (callerSignal?.aborted) throw err;
        noProgress++;
        if (noProgress > 3) throw new Error(`sandbox log stream transport failed: ${(err as Error).message}`);
        await backoff();
        continue;
      }
      if (!response.ok || !response.body) {
        const raw = await boundedBody(response, MAX_JSON).catch(() => new Uint8Array(0));
        let message = response.statusText;
        message = apiErrorMessage(raw, message);
        const retryable = response.status === 429 || response.status >= 500;
        if (!retryable) throw new CanterAPIError(response.status, message);
        noProgress++;
        if (noProgress > 3) throw new CanterAPIError(response.status, message);
        await backoff();
        continue;
      }
      // Parse and yield one complete frame at a time — delivered advances only
      // when an output event is handed to the caller, so a later malformed
      // frame can never silently drop already-valid bytes.
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      let event = "";
      let data = "";
      let haveData = false;
      let heartbeat = false;
      let frameSize = 0;
      let terminal = false;
      let madeProgress = false;
      let sawHeartbeat = false;
      let transportError: Error | undefined;
      const emit = (frame: { event: string; data: string }): SandboxLogEvent | undefined => {
        if (frame.event === "output") {
          let chunk: { jobId?: unknown; stream?: unknown; dataBase64?: unknown; offset?: unknown; nextOffset?: unknown; status?: unknown; exitCode?: unknown; logTruncated?: unknown } | null;
          try { chunk = JSON.parse(frame.data); } catch {
            throw new SandboxProtocolError("invalid sandbox log output frame");
          }
          if (chunk === null || typeof chunk !== "object") {
            throw new SandboxProtocolError("invalid sandbox log output receipt");
          }
          const bytes = decodeBase64(typeof chunk.dataBase64 === "string" ? chunk.dataBase64 : "");
          if (
            bytes.length > MAX_LOG_CHUNK ||
            chunk.jobId !== jobId || chunk.stream !== stream ||
            typeof chunk.offset !== "number" || !Number.isSafeInteger(chunk.offset) || chunk.offset !== delivered ||
            typeof chunk.nextOffset !== "number" || !Number.isSafeInteger(chunk.nextOffset) ||
            chunk.nextOffset < chunk.offset ||
            chunk.nextOffset - chunk.offset !== bytes.length ||
            typeof chunk.status !== "string" || !JOB_STATUSES.has(chunk.status) ||
            (chunk.exitCode !== undefined && !Number.isInteger(chunk.exitCode))
          ) {
            throw new SandboxProtocolError("invalid sandbox log output receipt");
          }
          return {
            event: "output", jobId: jobId, stream, status: chunk.status as string,
            offset: chunk.offset as number, nextOffset: chunk.nextOffset as number, data: bytes,
            ...(Number.isInteger(chunk.exitCode) ? { exitCode: chunk.exitCode as number } : {}),
            logTruncated: chunk.logTruncated === true,
          };
        }
        if (frame.event === "status") {
          let view: SandboxJob | null;
          try { view = JSON.parse(frame.data) as SandboxJob; } catch {
            throw new SandboxProtocolError("invalid sandbox log status frame");
          }
          if (view === null || typeof view !== "object") {
            throw new SandboxProtocolError("invalid sandbox log status receipt");
          }
          if (view.jobId !== jobId || !JOB_STATUSES.has(view.status) ||
              (view.exitCode !== undefined && !Number.isInteger(view.exitCode))) {
            throw new SandboxProtocolError("invalid sandbox log status receipt");
          }
          return {
            event: "status", jobId, stream: "", status: view.status,
            offset: 0, nextOffset: 0, data: new Uint8Array(0),
            ...(Number.isInteger(view.exitCode) ? { exitCode: view.exitCode } : {}),
            logTruncated: view.logTruncated === true,
          };
        }
        return undefined;
      };
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let idx: number;
          while ((idx = buffer.indexOf("\n")) >= 0) {
            if (frameSize > MAX_FRAME) {
              throw new SandboxProtocolError(`sandbox job event frame exceeds ${MAX_FRAME} bytes`);
            }
            let line = buffer.slice(0, idx);
            buffer = buffer.slice(idx + 1);
            frameSize += new TextEncoder().encode(line + "\n").byteLength;
            if (line.endsWith("\r")) line = line.slice(0, -1);
            if (line === "") {
              if (heartbeat && !haveData && event === "") {
                sawHeartbeat = true;
              } else if (haveData || event !== "") {
                const emitted = emit({ event, data });
                event = ""; data = ""; haveData = false;
                if (emitted) {
                  if (emitted.event === "output") {
                    // Yield first, then advance delivered.
                    yield emitted;
                    delivered = emitted.nextOffset;
                    if (emitted.data.length > 0) madeProgress = true;
                  } else {
                    yield emitted;
                    if (emitted.status !== lastStatus) { lastStatus = emitted.status; madeProgress = true; }
                    if (!JOB_ACTIVE.has(emitted.status)) { terminal = true; }
                  }
                }
                if (terminal) break;
              }
              heartbeat = false;
              frameSize = 0;
              event = ""; data = ""; haveData = false;
            } else if (line.startsWith(":")) {
              heartbeat = true;
            } else if (line.startsWith("event:")) {
              event = line.slice(6).trim();
            } else if (line.startsWith("data:")) {
              if (haveData) data += "\n";
              data += line.startsWith("data: ") ? line.slice(6) : line.slice(5);
              haveData = true;
            }
            if (terminal) break;
          }
          if (terminal) break;
          // Bound completed lines plus the in-progress partial line, in
          // UTF-8 bytes — not UTF-16 code units.
          if (frameSize + new TextEncoder().encode(buffer).byteLength > MAX_FRAME) {
            throw new SandboxProtocolError(`sandbox job event frame exceeds ${MAX_FRAME} bytes`);
          }
        }
      } catch (err) {
        if (err instanceof SandboxProtocolError) {
          throw err;
        }
        transportError = err as Error;
      } finally {
        try { await reader.cancel(); } catch { /* ignore */ }
        reader.releaseLock();
      }
      if (terminal) return;
      if (callerSignal?.aborted) throw new Error("stream aborted");
      if (madeProgress || (sawHeartbeat && Date.now() - feedStart >= 1000)) {
        noProgress = 0;
      } else {
        noProgress++;
        if (noProgress > 3) {
          throw new Error("sandbox log stream made no progress after repeated reconnects", { cause: transportError });
        }
      }
      await backoff();
    }
  }

  private static normalizePath(raw: string): string {
    let target = raw.trim();
    if (!target) target = "/workspace";
    if (!target.startsWith("/")) target = `/workspace/${target}`;
    const parts: string[] = [];
    for (const seg of target.split("/")) {
      if (seg === "" || seg === ".") continue;
      if (seg === "..") { parts.pop(); continue; }
      parts.push(seg);
    }
    return `/${parts.join("/")}`;
  }

  /**
   * Uploads a file through begin/chunk/complete. Chunks go as raw octet-stream
   * PATCHes of at most 1MiB; each receipt is validated for the exact consumed
   * offset and path. On any failure the upload is aborted best-effort — an
   * incomplete file is never completed.
   */
  async upload(id: string, path: string, source: UploadOptions): Promise<SandboxUpload> {
    const normalized = SandboxClient.normalizePath(path);
    if (typeof source.size !== "number" || !Number.isSafeInteger(source.size) || source.size < 0) {
      throw new TypeError("sandbox upload size must be a nonnegative safe integer");
    }
    if (typeof source.sha256 !== "string" || !SHA256_HEX.test(source.sha256)) {
      throw new TypeError("sandbox upload sha256 must be a lowercase 64-hex digest");
    }
    const base = this.sandboxPath(id, "/uploads");
    const begun = await this.request<SandboxUpload>("POST", base, { path: normalized, size: source.size, sha256: source.sha256 }, source);
    // Only a validated server upload ID is ever sent on the abort route.
    const uploadId = checkId("upload ID", begun?.id);
    const abort = async () => {
      try { await this.requestRaw("DELETE", `${base}/${uploadId}`, null, ""); } catch { /* best effort */ }
    };
    if (begun.path !== normalized || begun.size !== source.size || begun.offset !== 0 || begun.complete !== false) {
      await abort();
      throw new SandboxProtocolError("invalid sandbox upload begin receipt");
    }
    let sent = 0;
    try {
      for await (const chunk of source.chunks) {
        if (!(chunk instanceof Uint8Array)) {
          throw new TypeError("sandbox upload chunks must be Uint8Array");
        }
        let at = 0;
        while (at < chunk.length) {
          if (sent >= source.size) {
            throw new Error(`sandbox upload source exceeded declared size ${source.size}`);
          }
          const end = Math.min(at + UPLOAD_CHUNK, chunk.length, at + (source.size - sent));
          const piece = chunk.subarray(at, end);
          const receipt = await this.requestJson<SandboxUpload>(
            "PATCH", `${base}/${uploadId}?offset=${sent}`, piece.slice().buffer as ArrayBuffer, "application/octet-stream", source);
          if (receipt.id !== begun.id || receipt.path !== normalized || receipt.size !== source.size ||
              receipt.offset !== sent + piece.length || receipt.complete !== false) {
            throw new SandboxProtocolError("invalid sandbox upload chunk receipt");
          }
          sent += piece.length;
          at = end;
        }
      }
      if (sent !== source.size) {
        throw new Error(`sandbox upload source ended at ${sent} of ${source.size} bytes`);
      }
      const completed = await this.request<SandboxUpload>("POST", `${base}/${uploadId}/complete`, {}, source);
      if (!completed.complete || completed.id !== begun.id || completed.path !== normalized ||
          completed.size !== source.size || completed.offset !== source.size) {
        throw new SandboxProtocolError("invalid sandbox upload completion receipt");
      }
      if (completed.sha256 && completed.sha256 !== source.sha256) {
        throw new SandboxProtocolError("sandbox upload checksum mismatch");
      }
      return completed;
    } catch (err) {
      await abort();
      throw err;
    }
  }

  /** Convenience bytes upload with a native SHA-256 digest. */
  async uploadBytes(id: string, path: string, bytes: Uint8Array, options?: RequestOptions): Promise<SandboxUpload> {
    if (!(bytes instanceof Uint8Array)) {
      throw new TypeError("uploadBytes requires a Uint8Array");
    }
    const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes.slice().buffer as ArrayBuffer));
    const sha256 = Array.from(digest, (b) => b.toString(16).padStart(2, "0")).join("");
    async function* chunks() { yield bytes; }
    return this.upload(id, path, {
      size: bytes.length,
      sha256,
      chunks: chunks(),
      ...(options?.signal ? { signal: options.signal } : {}),
    });
  }

  /** Streams a downloaded file, always releasing the response body. */
  async *download(id: string, path: string, options?: RequestOptions): AsyncGenerator<Uint8Array> {
    const query = new URLSearchParams({ path });
    const token = await this.token();
    const response = await this.fetchFn(`${this.baseUrl}${this.sandboxPath(id, "/files/content")}?${query}`, {
      method: "GET",
      redirect: "manual",
      credentials: "omit",
      cache: "no-store",
      signal: this.signal(options, this.timeoutMs),
      headers: { Authorization: `Bearer ${token}` },
    });
    if (!response.ok || !response.body) {
      const raw = await boundedBody(response, MAX_JSON).catch(() => new Uint8Array(0));
      let message = response.statusText;
      message = apiErrorMessage(raw, message);
      throw new CanterAPIError(response.status, message);
    }
    // Fetch decodes compressed bodies, while Content-Length still describes
    // their encoded representation. Its decoder detects truncated encodings.
    const encoding = response.headers.get("content-encoding");
    const declared = encoding && encoding.toLowerCase() !== "identity"
      ? -1 : Number(response.headers.get("content-length") ?? -1);
    let received = 0;
    const reader = response.body.getReader();
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        received += value.length;
        yield value;
      }
      if (declared >= 0 && received !== declared) {
        throw new Error(`sandbox download truncated: received ${received} of ${declared} bytes`);
      }
    } finally {
      try { await reader.cancel(); } catch { /* ignore */ }
      reader.releaseLock();
    }
  }

  async sleep(id: string, options?: RequestOptions): Promise<void> {
    await this.request("POST", this.sandboxPath(id, "/sleep"), {}, options);
  }

  async wake(id: string, options?: RequestOptions): Promise<void> {
    await this.request("POST", this.sandboxPath(id, "/wake"), {}, options);
  }

  async delete(id: string, options?: RequestOptions): Promise<void> {
    await this.request("DELETE", this.sandboxPath(id, ""), undefined, options);
  }

  async save(id: string, name: string, keep: boolean, options?: RequestOptions): Promise<SandboxSnapshot> {
    await this.sleep(id, options);
    const asleep = await this.wait(id, "asleep", options);
    if (!asleep.snapshotId) {
      throw new Error(`sandbox ${id} slept without producing a snapshot`);
    }
    const path = `${this.base()}/sandbox-snapshots/${checkId("snapshot ID", asleep.snapshotId)}`;
    if (!name && !keep) {
      return this.request<SandboxSnapshot>("GET", path, undefined, options);
    }
    return this.request<SandboxSnapshot>("PATCH", path, {
      ...(name ? { name } : {}),
      ...(keep ? { retained: true } : {}),
    }, options);
  }

  async snapshots(options?: SnapshotsOptions): Promise<SandboxSnapshotPage> {
    const query = new URLSearchParams();
    if (options?.source) query.set("sourceSandboxId", checkId("source sandbox ID", options.source));
    if (options?.before) query.set("before", checkId("snapshot cursor", options.before));
    const suffix = query.size > 0 ? `?${query}` : "";
    return this.request<SandboxSnapshotPage>("GET", `${this.base()}/sandbox-snapshots${suffix}`, undefined, options);
  }

  async templates(options?: RequestOptions): Promise<SandboxTemplateList> {
    return this.request<SandboxTemplateList>("GET", `${this.base()}/sandbox-templates`, undefined, options);
  }

  async createTemplate(name: string, snapshotId: string, options?: RequestOptions): Promise<SandboxTemplate> {
    checkId("snapshot ID", snapshotId);
    return this.request<SandboxTemplate>("POST", `${this.base()}/sandbox-templates`, { name, snapshotId }, options);
  }

  async restore(id: string, snapshotId: string, generation: number, options?: RequestOptions): Promise<Sandbox> {
    checkId("snapshot ID", snapshotId);
    if (!Number.isInteger(generation) || generation < 1) {
      throw new TypeError("restore generation must be a positive integer");
    }
    const result = await this.request<Sandbox>("POST", this.sandboxPath(id, "/restore"), {
      snapshotId,
      expectedGeneration: generation,
    }, options);
    if (result.id !== id) throw new SandboxProtocolError("sandbox receipt ID does not match request");
    return result;
  }
}

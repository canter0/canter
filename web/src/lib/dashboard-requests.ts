import type { OperatorSurface } from "./operator-api";

// A dashboard request is a one-shot handoff into a conversation: it carries an
// unsent prompt the user reviews and sends themselves. Nothing here executes
// infrastructure changes.
export type DashboardRequest = {
  nonce: string;
  accountId: string;
  workspaceId: string;
  conversationId: string | null;
  prompt: string;
  surface?: OperatorSurface;
  composerOrigin?: DashboardRequestComposerOrigin;
  createdAt: number;
};

// Where the request composer sat when the user confirmed — pure presentation
// metadata so the destination composer can morph in from it. Never part of
// any model or message payload.
export type DashboardRequestComposerOrigin = { x: number; y: number; width: number; height: number; viewportWidth: number; viewportHeight: number };

// Invalid geometry is dropped, never a reason to reject a valid request.
export function dashboardRequestComposerOrigin(value: unknown): DashboardRequestComposerOrigin | null {
  if (!value || typeof value !== "object") return null;
  const o = value as Record<string, unknown>;
  const { x, y, width, height, viewportWidth, viewportHeight } = o;
  if (![x, y, width, height, viewportWidth, viewportHeight].every(v => typeof v === "number" && Number.isFinite(v))) return null;
  const origin = { x: x as number, y: y as number, width: width as number, height: height as number, viewportWidth: viewportWidth as number, viewportHeight: viewportHeight as number };
  if (origin.viewportWidth <= 0 || origin.viewportHeight <= 0 || origin.viewportWidth > 16384 || origin.viewportHeight > 16384) return null;
  if (origin.x < 0 || origin.y < 0 || origin.width <= 0 || origin.height <= 0) return null;
  if (origin.x + origin.width > origin.viewportWidth + 1 || origin.y + origin.height > origin.viewportHeight + 1) return null;
  return origin;
}

export function composerOriginMatchesViewport(origin: DashboardRequestComposerOrigin, viewportWidth: number, viewportHeight: number): boolean {
  return Math.abs(viewportWidth - origin.viewportWidth) <= 1 && Math.abs(viewportHeight - origin.viewportHeight) <= 1;
}

export const dashboardRequestTTLMs = 30 * 60 * 1000;
export const dashboardRequestMaxPrompt = 8000;
export const dashboardRequestMaxBytes = 16 * 1024;

const keyPrefix = "canter:dashboard-request:";
export function dashboardRequestKey(nonce: string) {
  return `${keyPrefix}${nonce}`;
}

export type DashboardRequestStore = Pick<Storage, "getItem" | "setItem" | "removeItem">;

const bounded = (value: unknown, max: number): value is string => typeof value === "string" && value.length > 0 && value.length <= max;

// Only the surfaces a dashboard handoff can legitimately carry.
const allowedSurfaceKinds = new Set(["apps", "sandboxes", "sandbox", "super-sandboxes", "super-sandbox"]);
const sandboxSurfaceID = /^sbx_[^\s\x00-\x1f\x7f]+$/;
const superSandboxSurfaceID = /^ssb_[A-Za-z0-9_-]+$/;

export function dashboardRequestSurface(value: unknown): OperatorSurface | null {
  if (!value || typeof value !== "object") return null;
  const surface = value as { kind?: unknown; id?: unknown };
  if (typeof surface.kind !== "string" || !allowedSurfaceKinds.has(surface.kind)) return null;
  if (surface.kind === "sandbox" || surface.kind === "super-sandbox") {
    const pattern = surface.kind === "sandbox" ? sandboxSurfaceID : superSandboxSurfaceID;
    if (typeof surface.id !== "string" || surface.id.length > 160 || !pattern.test(surface.id)) return null;
    return { kind: surface.kind, id: surface.id };
  }
  if (surface.id !== undefined) return null;
  return { kind: surface.kind as OperatorSurface["kind"] };
}

export function createDashboardRequest(
  input: { accountId: string; workspaceId: string; conversationId: string | null; prompt: string; surface?: OperatorSurface; composerOrigin?: DashboardRequestComposerOrigin },
  now = Date.now(),
  nonce = crypto.randomUUID(),
): DashboardRequest | null {
  if (!bounded(input.accountId, 160) || !bounded(input.workspaceId, 160)) return null;
  if (!bounded(nonce, 80) || !Number.isSafeInteger(now) || now <= 0) return null;
  if (input.conversationId !== null && !bounded(input.conversationId, 160)) return null;
  if (!bounded(input.prompt, dashboardRequestMaxPrompt) || !input.prompt.trim()) return null;
  const request: DashboardRequest = { nonce, accountId: input.accountId, workspaceId: input.workspaceId, conversationId: input.conversationId, prompt: input.prompt, createdAt: now };
  const surface = input.surface === undefined ? null : dashboardRequestSurface(input.surface);
  if (input.surface !== undefined && !surface) return null;
  if (surface) request.surface = surface;
  const origin = input.composerOrigin === undefined ? null : dashboardRequestComposerOrigin(input.composerOrigin);
  if (origin) request.composerOrigin = origin;
  return request;
}

export function serializeDashboardRequest(request: DashboardRequest): string | null {
  try {
    const raw = JSON.stringify(request);
    return new TextEncoder().encode(raw).length <= dashboardRequestMaxBytes ? raw : null;
  } catch {
    return null;
  }
}

export function parseDashboardRequest(raw: unknown, now = Date.now()): DashboardRequest | null {
  if (!raw || typeof raw !== "object") return null;
  const value = raw as Record<string, unknown>;
  if (!bounded(value.nonce, 80) || !bounded(value.accountId, 160) || !bounded(value.workspaceId, 160)) return null;
  if (value.conversationId !== null && !bounded(value.conversationId, 160)) return null;
  if (!bounded(value.prompt, dashboardRequestMaxPrompt) || !(value.prompt as string).trim()) return null;
  if (!Number.isSafeInteger(value.createdAt)) return null;
  const createdAt = value.createdAt as number;
  if (createdAt <= 0 || createdAt > now) return null;
  const request: DashboardRequest = { nonce: value.nonce, accountId: value.accountId, workspaceId: value.workspaceId, conversationId: value.conversationId, prompt: value.prompt, createdAt };
  if (value.surface !== undefined) {
    const surface = dashboardRequestSurface(value.surface);
    if (!surface) return null;
    request.surface = surface;
  }
  if (value.composerOrigin !== undefined) {
    const origin = dashboardRequestComposerOrigin(value.composerOrigin);
    if (origin) request.composerOrigin = origin;
  }
  return request;
}

export type DashboardRequestLookup = { request: DashboardRequest; error?: never } | { request?: never; error: "missing" | "invalid" | "expired" };

export function readDashboardRequest(nonce: string, store: DashboardRequestStore, now = Date.now()): DashboardRequestLookup {
  if (!bounded(nonce, 80)) return { error: "invalid" };
  let raw: string | null = null;
  try {
    raw = store.getItem(dashboardRequestKey(nonce));
  } catch {
    return { error: "missing" };
  }
  if (raw === null) return { error: "missing" };
  if (new TextEncoder().encode(raw).length > dashboardRequestMaxBytes) return { error: "invalid" };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { error: "invalid" };
  }
  const request = parseDashboardRequest(parsed, now);
  if (!request || request.nonce !== nonce) return { error: "invalid" };
  if (now - request.createdAt > dashboardRequestTTLMs) return { error: "expired" };
  return { request };
}

export function storeDashboardRequest(request: DashboardRequest, store: DashboardRequestStore): void {
  const raw = serializeDashboardRequest(request);
  if (!raw) throw new Error("This request is too large to hand off.");
  store.setItem(dashboardRequestKey(request.nonce), raw);
}

// Consumption must provably remove the envelope before the request is applied;
// a storage layer that refuses removal leaves the request usable again.
export function consumeDashboardRequest(nonce: string, store: DashboardRequestStore): boolean {
  try {
    store.removeItem(dashboardRequestKey(nonce));
    return store.getItem(dashboardRequestKey(nonce)) === null;
  } catch {
    return false;
  }
}

// Commits the appended draft and retires the envelope atomically from the
// caller's view: the draft write must verifiably land before the envelope is
// consumed, and a failed consume restores the previous draft so a request can
// never be applied twice or lost silently. Storage exceptions are contained.
export function commitDashboardRequestToDraft(nonce: string, store: DashboardRequestStore, draftKey: string, nextDraft: string): boolean {
  let previous: string | null;
  try {
    previous = store.getItem(draftKey);
  } catch {
    return false;
  }
  try {
    store.setItem(draftKey, nextDraft);
    if (store.getItem(draftKey) !== nextDraft) return false;
  } catch {
    return false;
  }
  let consumed = false;
  try {
    store.removeItem(dashboardRequestKey(nonce));
    consumed = store.getItem(dashboardRequestKey(nonce)) === null;
  } catch {
    consumed = false;
  }
  if (!consumed) {
    try {
      store.setItem(draftKey, previous ?? "");
    } catch {
      // The storage failure is unrecoverable here; the caller reports it.
    }
    return false;
  }
  return true;
}

export function dashboardRequestMatches(request: DashboardRequest, scope: { accountId: string; workspaceId: string; conversationId: string | null }): boolean {
  return request.accountId === scope.accountId && request.workspaceId === scope.workspaceId && request.conversationId === scope.conversationId;
}

export function appendDashboardRequestText(draft: string, prompt: string): string {
  return draft.trim() ? `${draft}\n\n${prompt}` : prompt;
}

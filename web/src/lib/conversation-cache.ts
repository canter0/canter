import type { ConversationDetail, OperatorEvent } from "./operator-api";

type Snapshot = { detail: ConversationDetail; events: OperatorEvent[]; fetchedAt: number; bytes: number };

// Owned by one authenticated WorkspaceProvider. Nothing persists across accounts
// or logout, and large attachment histories cannot grow the cache without a bound.
export function createConversationCache(workspace: string, fetcher: (id: string) => Promise<ConversationDetail>, now: () => number = Date.now) {
  const entries = new Map<string, Snapshot>();
  const pending = new Map<string, Promise<ConversationDetail>>();
  const maxBytes = 16 * 1024 * 1024;
  const maxEntries = 8;
  const lifetime = 5 * 60_000;
  let bytes = 0;

  function remove(id: string) {
    const previous = entries.get(id);
    if (previous) bytes -= previous.bytes;
    entries.delete(id);
  }
  function peek(id: string) {
    const value = entries.get(id);
    if (value && now() - value.fetchedAt > lifetime) { remove(id); return undefined; }
    return value;
  }
  function save(id: string, detail: ConversationDetail, events: OperatorEvent[] = [], fetchedAt?: number) {
    if (detail.conversation.id !== id || detail.conversation.workspaceId !== workspace) return;
    const previous = entries.get(id);
    const fetched = fetchedAt ?? (previous?.detail === detail ? previous.fetchedAt : now());
    const size = JSON.stringify({ detail, events }).length * 2;
    remove(id);
    if (size > maxBytes) return;
    entries.set(id, { detail, events, fetchedAt: fetched, bytes: size });
    bytes += size;
    while (entries.size > maxEntries || bytes > maxBytes) remove(entries.keys().next().value!);
  }
  function invalidate(id: string) {
    remove(id);
    // An older request may finish, but must not repopulate an invalidated entry.
    pending.delete(id);
  }
  function load(id: string, refresh = false): Promise<ConversationDetail> {
    const existing = pending.get(id);
    if (existing) return existing;
    const cached = peek(id);
    if (!refresh && cached && now() - cached.fetchedAt < 15_000) return Promise.resolve(cached.detail);
    const request = Promise.resolve().then(() => fetcher(id)).then(detail => {
      if (detail.conversation.id !== id || detail.conversation.workspaceId !== workspace) throw new Error("The conversation does not belong to this workspace.");
      if (pending.get(id) === request) save(id, detail, peek(id)?.events ?? []);
      return detail;
    }).finally(() => { if (pending.get(id) === request) pending.delete(id); });
    pending.set(id, request);
    return request;
  }
  return { peek, save, invalidate, load };
}

export type ConversationCache = ReturnType<typeof createConversationCache>;

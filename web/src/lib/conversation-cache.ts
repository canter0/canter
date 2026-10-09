import type { ConversationDetail, OperatorEvent } from "./operator-api";

type Snapshot = { detail: ConversationDetail; events: OperatorEvent[]; fetchedAt: number; bytes: number; eventBytes: number };

// Owned by one authenticated WorkspaceProvider. Nothing persists across accounts
// or logout, and large attachment histories cannot grow the cache without a bound.
export function createConversationCache(workspace: string, fetcher: (id: string) => Promise<ConversationDetail>, now: () => number = Date.now) {
  const entries = new Map<string, Snapshot>();
  const pending = new Map<string, Promise<ConversationDetail>>();
  const maxBytes = 16 * 1024 * 1024;
  const maxEntries = 8;
  const lifetime = 5 * 60_000;
  const detailLengths = new WeakMap<object, number>();
  const eventLengths = new WeakMap<object, number>();
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
  function serializedLength(value: object, lengths: WeakMap<object, number>) {
    const cached = lengths.get(value);
    if (cached !== undefined) return cached;
    const length = JSON.stringify(value).length;
    lengths.set(value, length);
    return length;
  }
  function sizeOf(detail: ConversationDetail, events: OperatorEvent[], previous?: Snapshot) {
    const detailLength = serializedLength(detail, detailLengths);
    let eventLength = 0;
    let prefix = 0;
    if (previous) {
      const shared = Math.min(previous.events.length, events.length);
      while (prefix < shared && previous.events[prefix] === events[prefix]) prefix++;
    }
    // Event objects are immutable snapshots. The WeakMap avoids serializing the
    // unchanged history again; only events after the shared prefix need sizing.
    if (previous && prefix === previous.events.length) eventLength = previous.eventBytes;
    else for (let index = 0; index < prefix; index++) eventLength += serializedLength(events[index], eventLengths);
    for (let index = prefix; index < events.length; index++) eventLength += serializedLength(events[index], eventLengths);
    const jsonLength = '{"detail":'.length + detailLength + ',"events":['.length + eventLength + Math.max(0, events.length - 1) + ']}'.length;
    return { bytes: jsonLength * 2, eventBytes: eventLength };
  }
  function save(id: string, detail: ConversationDetail, events: OperatorEvent[] = [], fetchedAt?: number) {
    if (detail.conversation.id !== id || detail.conversation.workspaceId !== workspace) return;
    const previous = entries.get(id);
    const fetched = fetchedAt ?? (previous?.detail === detail ? previous.fetchedAt : now());
    const measured = sizeOf(detail, events, previous);
    remove(id);
    if (measured.bytes > maxBytes) return;
    entries.set(id, { detail, events, fetchedAt: fetched, ...measured });
    bytes += measured.bytes;
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

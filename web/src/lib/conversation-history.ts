// API pages arrive newest first; transcript components render oldest first.
export const normalizeConversationDetail = <T extends { messages: unknown[] }>(detail: T): T => ({ ...detail, messages: [...detail.messages].reverse() });

type ConversationHistory = {
  conversation: { id: string };
  messages: Array<{ id: string; createdAt: string }>;
  hasMore?: boolean;
  nextCursor?: string;
};

function compareFractions(left: string, right: string) {
  const length = Math.max(left.length, right.length);
  const a = left.padEnd(length, "0");
  const b = right.padEnd(length, "0");
  return a < b ? -1 : a > b ? 1 : 0;
}

function compareTimestamps(left: string, right: string) {
  const fraction = (value: string) => value.match(/\.(\d+)(?=(?:Z|[+-]\d{2}:\d{2})$)/i)?.[1] ?? "";
  const withoutFraction = (value: string) => value.replace(/\.\d+(?=(?:Z|[+-]\d{2}:\d{2})$)/i, "");
  const leftTime = Date.parse(withoutFraction(left));
  const rightTime = Date.parse(withoutFraction(right));
  if (Number.isFinite(leftTime) && Number.isFinite(rightTime)) {
    if (leftTime !== rightTime) return leftTime < rightTime ? -1 : 1;
    return compareFractions(fraction(left), fraction(right));
  }
  return left < right ? -1 : left > right ? 1 : 0;
}

/** Merge a recent refresh into loaded history while keeping the cursor at the oldest reachable edge. */
export function mergeConversationHistory<T extends ConversationHistory>(current: T | null, incoming: T): T {
  if (!current || current.conversation.id !== incoming.conversation.id) return incoming;
  const currentIds = new Set(current.messages.map(message => message.id));
  const overlaps = incoming.messages.some(message => currentIds.has(message.id));
  const messages = [...new Map([...current.messages, ...incoming.messages].map(message => [message.id, message])).values()]
    .sort((a, b) => compareTimestamps(a.createdAt, b.createdAt) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  // A complete cached history remains complete after a refresh. If both
  // snapshots are partial, an overlap means the cached cursor already points
  // past the older history that is still missing; otherwise the fresh page's
  // cursor is the boundary from which to fill the gap.
  const hasMore = !!incoming.hasMore && (!!current.hasMore || !overlaps);
  const keepCurrentCursor = hasMore && !!current.hasMore && overlaps;
  return {
    ...incoming,
    messages,
    hasMore,
    nextCursor: hasMore ? (keepCurrentCursor ? current.nextCursor : incoming.nextCursor) : undefined,
  };
}

/** Add an older API page and advance the cursor from that page's boundary. */
export function mergeEarlierConversationPage<T extends ConversationHistory>(current: T | null, conversationId: string, page: { messages: T["messages"]; hasMore: boolean; nextCursor?: string }): T | null {
  if (!current || current.conversation.id !== conversationId) return current;
  const messages = [...new Map([...page.messages, ...current.messages].map(message => [message.id, message])).values()]
    .sort((a, b) => compareTimestamps(a.createdAt, b.createdAt) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  return { ...current, messages, hasMore: page.hasMore, nextCursor: page.nextCursor };
}

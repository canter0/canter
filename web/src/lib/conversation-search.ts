import type { Conversation } from "./operator-api";

const normalize = (value: string) => value.normalize("NFD").replace(/\p{M}/gu, "").toLowerCase();

function matchScore(title: string, query: string): number | null {
  if (title === query) return 0;
  if (title.startsWith(query)) return 1;
  const words = title.split(/[^\p{L}\p{N}]+/u).filter(Boolean);
  const terms = query.split(/\s+/);
  if (terms.every(term => words.some(word => word.startsWith(term)))) return 2;
  if (title.includes(query)) return 3;
  // Small abbreviations such as "co" → "clock" work without a network request.
  if (query.length < 2) return null;
  let cursor = 0;
  let first = -1;
  for (const character of query) {
    const index = title.indexOf(character, cursor);
    if (index < 0) return null;
    if (first < 0) first = index;
    cursor = index + 1;
  }
  return 4 + (cursor - first - query.length) / Math.max(title.length, 1);
}

/** Prefer exact/prefix matches, then words, substrings and abbreviations; newest wins ties. */
export function searchConversations(conversations: readonly Conversation[], input: string): Conversation[] {
  const query = normalize(input.trim());
  return conversations.flatMap(conversation => {
    const score = query ? matchScore(normalize(conversation.title), query) : 0;
    return score === null ? [] : [{ conversation, score }];
  }).sort((a, b) => a.score - b.score || b.conversation.updatedAt.localeCompare(a.conversation.updatedAt))
    .map(({ conversation }) => conversation);
}

export function conversationCompletion(title: string, input: string): string {
  return input && title.toLowerCase().startsWith(input.toLowerCase()) ? title.slice(input.length) : "";
}

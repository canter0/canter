const operatorDraftPrefix = "canter:conversation-draft:";

// New and saved conversations share the same account-bound namespace. Missing
// identity never falls back to an ownerless legacy draft or attachment record.
export function operatorDraftKey(accountId: string | null | undefined, workspaceId: string | null | undefined, conversationId?: string | null): string | null {
  if (!accountId?.trim() || !workspaceId?.trim()) return null;
  const conversation = conversationId ?? "new";
  if (!conversation.trim()) return null;
  return operatorDraftPrefix + [accountId, workspaceId, conversation].map(encodeURIComponent).join(":");
}

export function clearOperatorTextDrafts(storage: Pick<Storage, "length" | "key" | "removeItem">) {
  for (let index = storage.length - 1; index >= 0; index--) {
    const key = storage.key(index);
    if (key?.startsWith(operatorDraftPrefix)) storage.removeItem(key);
  }
}

export type OperatorDraftReplyAnchor = { version: 1; conversationId: string; messageId: string };
export function operatorReplyDraftKey(accountId: string | null | undefined, workspaceId: string | null | undefined, conversationId: string | null | undefined): string | null {
  if (!conversationId?.trim()) return null;
  const draft = operatorDraftKey(accountId, workspaceId, conversationId);
  return draft ? `${draft}:reply` : null;
}
export function serializeOperatorReplyAnchor(anchor: { conversationId: string; messageId: string }): string {
  return JSON.stringify({ version: 1, conversationId: anchor.conversationId, messageId: anchor.messageId });
}
export function parseOperatorReplyAnchor(raw: string | null | undefined, conversationId: string | null | undefined): OperatorDraftReplyAnchor | null {
  if (!raw || raw.length > 2048 || !conversationId) return null;
  try {
    const value = JSON.parse(raw);
    if (!value || value.version !== 1 || value.conversationId !== conversationId || typeof value.messageId !== "string" || !value.messageId.trim() || value.messageId.length > 200) return null;
    return { version: 1, conversationId, messageId: value.messageId };
  } catch { return null; }
}
export function isReplyTargetForAnchor(value: unknown, anchor: OperatorDraftReplyAnchor): value is import("./operator-api").OperatorReplyTarget {
  if (!value || typeof value !== "object") return false;
  const target = value as Record<string, unknown>;
  return target.messageId === anchor.messageId && target.conversationId === anchor.conversationId && (target.role === "user" || target.role === "assistant") && typeof target.senderName === "string" && typeof target.excerpt === "string" && typeof target.canWake === "boolean" && (target.wrenId === undefined || typeof target.wrenId === "string");
}

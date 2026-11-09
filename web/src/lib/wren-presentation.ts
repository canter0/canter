import type { Wren, WrenRoom } from "./wrens";

type ConversationIdentity = Pick<Wren, "status" | "lastMessage" | "purpose"> & { sharedAvailable?: boolean };

export function conversationState(value: ConversationIdentity) {
  if (value.sharedAvailable === false) return { label: "Unavailable", tone: "attention" };
  if (value.status === "failed") return { label: "Needs attention", tone: "attention" };
  if (value.status === "queued") return { label: "Queued", tone: "queued" };
  if (value.status === "running") return { label: "Working", tone: "working" };
  if (value.status === "cancelled") return { label: "Stopped", tone: "quiet" };
  return { label: value.lastMessage ? "Last conversation" : "No messages yet", tone: "quiet" };
}

export function conversationPreview(value: ConversationIdentity) {
  const state = conversationState(value);
  if (state.tone !== "quiet") return state.label;
  return plainPreview(value.lastMessage) || plainPreview(value.purpose) || "No messages yet";
}

export function plainPreview(value?: string) {
  return value
    // The server bounds previews, sometimes halfway through a link URL.
    // Keep its readable label instead of showing Markdown or a partial URL.
    ?.replace(/!?\[([^\]\n]+)\]\((?:\\.|[^)\\])*(?:\)|$)/g, "$1")
    .replace(/(^|\n)\s{0,3}(?:#{1,6}\s+|[-*+]\s+|\d+[.)]\s+)/g, "$1")
    .replace(/(\*\*|__|`)/g, "")
    .replace(/\s+/g, " ").trim() ?? "";
}

export function conversationHref(value: Wren | WrenRoom) {
  return "kind" in value ? `/app/wrens/rooms/${encodeURIComponent(value.id)}` : `/app/wrens/${encodeURIComponent(value.id)}`;
}

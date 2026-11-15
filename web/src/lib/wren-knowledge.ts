import type { Wren, WrenRoom } from "./wrens";
import type { OperatorEvent } from "./operator-api";
export type KnowledgeScope = { conversationId: string; scope: "private" | "room"; wrenId?: string };
export type KnowledgeTarget = { scope: KnowledgeScope; wren?: Wren; room?: WrenRoom; label: string };
export type MemorySources = { runId?: string; artifactId?: string; artifactRevision?: number; urls?: string[] };
export type WrenMemoryRecord = KnowledgeScope & { id: string; workspaceId: string; ownerId?: string; authorId: string; revision: number; latestRevision: number; title: string; content: string; sources: MemorySources; promotedFromId?: string; promotedFromRevision?: number; deleted: boolean; createdAt: string; updatedAt: string };
export type WrenSkillRecord = { id: string; workspaceId: string; ownerId: string; authorId: string; version: number; latestVersion: number; name: string; description: string; instructions: string; requiredCapabilities: string[]; disabled: boolean; createdAt: string; updatedAt: string };
export type WrenSkillInstallation = KnowledgeScope & { id: string; workspaceId: string; ownerId?: string; authorId: string; skillId: string; version: number; revision: number; enabled: boolean; skill: WrenSkillRecord; createdAt: string; updatedAt: string };
export type WrenSkillCapability = { name: string; available: boolean; reason?: string };
export const memoryForgetEvent = "canter-wren-memory-forgotten";
export function knowledgeTarget(conversationId: string | undefined, wrens: Wren[], rooms: WrenRoom[]): KnowledgeTarget | null {
  if (!conversationId) return null;
  const room = rooms.find(item => item.conversationId === conversationId);
  if (room) return { scope: { conversationId, scope: "room" }, room, label: `Shared · ${room.name}` };
  const wren = wrens.find(item => item.conversationId === conversationId);
  return wren ? { scope: { conversationId, scope: "private", wrenId: wren.id }, wren, label: `Private · ${wren.name}` } : null;
}
export function knowledgeQuery(scope: KnowledgeScope) {
  const query = new URLSearchParams({ conversationId: scope.conversationId, scope: scope.scope });
  if (scope.scope === "private" && scope.wrenId) query.set("wrenId", scope.wrenId);
  return query.toString();
}
export function visibleMemory(memory: WrenMemoryRecord, forgotten: ReadonlySet<string>): WrenMemoryRecord {
  return memory.deleted || forgotten.has(memory.id) ? { ...memory, title: "Forgotten memory", content: "", sources: {}, deleted: true } : memory;
}
export function memoryForgetNotices(events: OperatorEvent[]) { return events.filter(event => event.kind === "memory" && event.data.deleted === true && typeof event.data.memoryId === "string").map(event => ({ sequence: event.sequence, id: String(event.data.memoryId) })); }
export function notifyMemoryForgotten(workspaceId: string, conversationId: string, memoryId: string) { window.dispatchEvent(new CustomEvent(memoryForgetEvent, { detail: { workspaceId, conversationId, memoryId } })); }
export function parseSkillCapabilities(value: string) {
  const capabilities = [...new Set(value.split(/[\s,]+/).filter(Boolean))];
  if (capabilities.length > 32 || capabilities.some(name => name.length > 100 || !/^[a-zA-Z0-9_.:-]+$/.test(name))) throw new Error("Use up to 32 valid tool names, separated by lines or commas.");
  return capabilities;
}
export function skillInstallationLabel(installation: WrenSkillInstallation) { return installation.skill.disabled ? "Disabled by author" : installation.enabled ? "Enabled here" : "Disabled here"; }
export function roomMemoryAudience(room: WrenRoom, workspaceName: string) { return room.kind === "channel" ? `Everyone in ${workspaceName} can read this copy.` : `The invited people and Wrens in ${room.name} can read this copy.`; }

// Resolve only a confirmed room copy against the viewer's current room list.
export function sharedMemoryDestination(memory: WrenMemoryRecord, rooms: WrenRoom[]) {
  if (memory.deleted || memory.scope !== "room" || !memory.id.startsWith("memory_")) return null;
  const room = rooms.find(item => item.conversationId === memory.conversationId);
  if (!room) return null;
  const query = new URLSearchParams({ work: "wren-memory", workId: memory.id });
  return { name: room.name, href: `/app/wrens/rooms/${encodeURIComponent(room.id)}?${query}` };
}

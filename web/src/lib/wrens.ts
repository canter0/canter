export type Wren = { shared?: boolean; sharedAvailable?: boolean; publicationRevision?: number; canManageProfile?: boolean; unread?: number; role?: string; section?: string; lastMessage?: string; lastMessageAt?: string; id: string; conversationId: string; name: string; purpose: string; shape: string; palette: number; updatedAt: string; status: string };
export const wrenShapes = ["pebble", "circle", "square", "flower", "spark", "triangle", "capsule", "clover"] as const;
export const wrenPalettes = [["#31302f", "#ed9557"], ["#e68b50", "#efc679"], ["#448e8c", "#a1cca9"], ["#8973b8", "#bea7df"], ["#b49541", "#e4c96f"], ["#ab617b", "#dea2a9"], ["#587fb4", "#94c4d7"], ["#647f59", "#b5c28b"]] as const;
export function wrenState(status: string) { return status === "running" ? "working" : status === "queued" ? "queued" : status === "failed" ? "blocked" : "idle"; }

export type WrenRoom = { unread?: number; id: string; conversationId: string; canRecoverOwnership?:boolean; ownerId: string; kind: "channel" | "group"; name: string; purpose: string; section: string; wrenIds: string[]; memberIds: string[]; wrens: Wren[]; updatedAt: string; lastMessage: string; lastMessageAt?: string; status: string };
export type RoomPerson = { id: string; name: string };

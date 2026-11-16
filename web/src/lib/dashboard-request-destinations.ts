import type { Conversation } from "./operator-api";
import type { Wren, WrenRoom } from "./wrens";

// Destinations an "Ask Canter" handoff can land in. Canter is the default
// general conversation; personal Wrens and rooms stay optional extras. Only
// Continue navigates — picking a destination here never does.
export type DashboardRequestDestination = {
  key: string;
  label: string;
  detail: string;
  conversationId: string | null;
  href: string;
  group: "canter" | "wren" | "room";
  wren?: Wren;
};

export type DashboardRequestDestinationGroups = {
  canter: DashboardRequestDestination[];
  wrens: DashboardRequestDestination[];
  rooms: DashboardRequestDestination[];
};

const unavailable = (value: { status?: string; sharedAvailable?: boolean }) => value.status === "archived" || value.status === "deleted" || value.sharedAvailable === false;

export const newCanterDestinationKey = "canter:new";

export function dashboardRequestDestinations(input: { conversations: Conversation[]; wrens: Wren[]; rooms: WrenRoom[] }, hrefFor: (value: Wren | WrenRoom) => string): DashboardRequestDestinationGroups {
  const groups: DashboardRequestDestinationGroups = {
    canter: [{ key: newCanterDestinationKey, label: "New Canter conversation", detail: "Start a fresh conversation", conversationId: null, href: "/app?compose=1", group: "canter" }],
    wrens: [],
    rooms: [],
  };
  // Conversations backing a Wren or room keep their canonical route — never
  // send them through the generic conversation page.
  const claimed = new Set<string>();
  for (const room of input.rooms) {
    if (unavailable(room)) continue;
    claimed.add(room.conversationId);
    groups.rooms.push({ key: `room:${room.id}`, label: room.kind === "channel" ? `# ${room.name.replace(/^\s*#\s*/, "") || room.name}` : room.name, detail: "Room", conversationId: room.conversationId, href: hrefFor(room), group: "room" });
  }
  for (const wren of input.wrens) {
    if (unavailable(wren)) continue;
    claimed.add(wren.conversationId);
    groups.wrens.push({ key: `wren:${wren.id}`, label: wren.name, detail: "Personal Wren", conversationId: wren.conversationId, href: hrefFor(wren), group: "wren", wren });
  }
  const seen = new Set<string>();
  for (const conversation of input.conversations) {
    // Wren and room conversations must never take the generic route — if their
    // lists failed to load they are excluded rather than misrouted.
    if (claimed.has(conversation.id) || seen.has(conversation.id) || conversation.id.startsWith("conv_wren_") || conversation.id.startsWith("conv_room_")) continue;
    seen.add(conversation.id);
    groups.canter.push({ key: `canter:${conversation.id}`, label: conversation.title || "Untitled conversation", detail: "Conversation", conversationId: conversation.id, href: `/app/conversations/${encodeURIComponent(conversation.id)}`, group: "canter" });
  }
  return groups;
}

export function flattenDashboardRequestDestinations(groups: DashboardRequestDestinationGroups): DashboardRequestDestination[] {
  return [...groups.canter, ...groups.wrens, ...groups.rooms];
}

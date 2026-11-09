import type { OperatorEvent } from "./operator-api";

const labels: Record<string, string> = {
  available: "Access enabled", paused: "Paused", removed: "Removed",
  publisher_disabled: "Disabled by publisher", needs_sign_in: "Sign-in needed", unavailable: "Unavailable",
};
export type AppStatusSnapshot = { id: string; name: string; label: string };
export type AppSourceSnapshot = { slug?: string; name: string; description: string };
export type AppDiscoverySnapshot = { state: "checked" | "unavailable"; connections: AppStatusSnapshot[]; sources: AppSourceSnapshot[] };

/** A checked empty list is evidence; a missing or failed result is not. */
export function appDiscoverySnapshot(events: OperatorEvent[]): AppDiscoverySnapshot | null {
  const event = events.findLast(item => item.kind === "tool" && item.data.name === "canter_show_connections");
  if (!event || event.data.status === "running") return null;
  const result = event.data.result;
  if (event.data.status !== "completed" || !result || typeof result !== "object" || !("conversationConnections" in result) || !Array.isArray(result.conversationConnections)) {
    return { state: "unavailable", connections: [], sources: [] };
  }
  const connections = result.conversationConnections.flatMap(item => {
    if (!item || typeof item !== "object" || typeof item.id !== "string" || typeof item.name !== "string" || typeof item.state !== "string" || !Object.hasOwn(labels, item.state)) return [];
    return [{ id: item.id, name: item.name, label: labels[item.state] }];
  });
  // Do not turn malformed records into a confident "no apps" claim.
  if (connections.length !== result.conversationConnections.length) return { state: "unavailable", connections: [], sources: [] };
  const seen = new Set<string>();
  const sources: AppSourceSnapshot[] = "publicSources" in result && Array.isArray(result.publicSources) ? result.publicSources.flatMap(item => {
    if (!item || typeof item !== "object" || typeof item.name !== "string" || typeof item.description !== "string" || seen.has(item.name)) return [];
    seen.add(item.name);
    return [{ name: item.name, description: item.description, ...(typeof item.slug === "string" && /^[a-z0-9-]{1,64}$/.test(item.slug) ? { slug: item.slug } : {}) }];
  }).slice(0, 12) : [];
  return { state: "checked", connections, sources };
}

/** Historical access evidence, never current viewer credentials or live health. */
export function appStatusSnapshot(events: OperatorEvent[]): AppStatusSnapshot[] {
  return appDiscoverySnapshot(events)?.connections ?? [];
}

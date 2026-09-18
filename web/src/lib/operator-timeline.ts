import type { OperatorEvent } from "./operator-api";

// Text events are cumulative snapshots per model step, not incremental tokens.
// Retain first sequence positions while replacing content with the latest snapshot.
export function turnTimeline(events: OperatorEvent[]) {
  const rows = new Map<string, OperatorEvent>();
  for (const event of events) {
    if (event.kind !== "text" && event.kind !== "tool") continue;
    const key = event.kind === "text" ? `text:${event.data.step}` : `tool:${event.data.callId}`;
    const previous = rows.get(key);
    rows.set(key, { ...event, sequence: previous?.sequence ?? event.sequence });
  }
  const ordered = [...rows.values()].sort((a, b) => a.sequence - b.sequence);
  const firstTool = ordered.findIndex(event => event.kind === "tool");
  if (firstTool < 0) return { preamble: null, work: [], tail: ordered.filter(event => event.kind === "text").at(-1) ?? null };
  const lastTool = ordered.findLastIndex(event => event.kind === "tool");
  return {
    preamble: firstTool > 0 ? ordered[firstTool - 1] : null,
    work: ordered.slice(firstTool, lastTool + 1),
    tail: ordered.slice(lastTool + 1).filter(event => event.kind === "text").at(-1) ?? null,
  };
}
export function elapsedLabel(start: string, end: string | number) {
  const seconds = Math.max(0, Math.floor(((typeof end === "number" ? end : Date.parse(end)) - Date.parse(start)) / 1000));
  if (!Number.isFinite(seconds)) return "";
  return seconds >= 60 ? `${Math.floor(seconds / 60)}m ${seconds % 60}s` : `${seconds}s`;
}

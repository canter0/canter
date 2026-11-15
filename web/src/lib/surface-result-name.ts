import type { OperatorEvent, OperatorSurface } from "./operator-api";

export function surfaceResultName(surface: OperatorSurface, events: OperatorEvent[]): string | undefined {
  if (!surface.id) return;
  const event = events.findLast(event => surface.kind === "wren-artifact"
    ? event.kind === "artifact" && event.data.artifactId === surface.id
    : surface.kind === "wren-memory"
      ? event.kind === "memory" && event.data.memoryId === surface.id
      : surface.kind === "wren-job" && event.data.jobId === surface.id);
  const name = event?.data.name ?? event?.data.title;
  return typeof name === "string" && name.trim() ? name : undefined;
}

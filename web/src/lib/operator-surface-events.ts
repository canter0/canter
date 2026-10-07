import type { OperatorEvent, OperatorSurface } from "./operator-api";

// Reading a view never requests attention. Only newly prepared proposals do.
export function takeReviewSurface(events: OperatorEvent[], seen: Set<string>): OperatorSurface | null {
  let review: OperatorSurface | null = null;
  for (const event of events) {
    const data = event.data;
    if (event.kind !== "surface" || data.attention !== "review" || !["deployment", "change", "vps"].includes(String(data.kind)) || typeof data.id !== "string" || !data.id) continue;
    if (data.kind === "change" && (typeof data.system !== "string" || !data.system)) continue;
    const key = `${data.kind}:${data.system ?? ""}:${data.id}`;
    if (seen.has(key)) continue;
    seen.add(key);
    review = data as OperatorSurface;
  }
  return review;
}

export function pendingRepositoryPicker(events: OperatorEvent[]): OperatorEvent | undefined {
  const request = events.findLast(event => event.kind === "surface" && event.data.kind === "github" && event.data.attention === "input");
  if (!request) return;
  const resolved = events.some(event => {
    if (event.sequence <= request.sequence) return false;
    if (event.kind === "surface") return ["repository", "file", "repository-changes", "deployment"].includes(String(event.data.kind));
    // Selection is already complete when the follow-up is accepted, even if
    // repository inspection is still queued or the conversation is reopened.
    if (event.kind !== "queued") return false;
    const message = event.data.message as { surface?: OperatorSurface } | undefined;
    return !!message?.surface?.repository && ["repository", "file", "repository-changes"].includes(message.surface.kind);
  });
  return resolved ? undefined : request;
}

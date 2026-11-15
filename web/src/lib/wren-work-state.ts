import type { WrenJobRecord } from "./wren-work";

export function activeJob(value: WrenJobRecord) { return ["queued", "running", "stop_requested"].includes(value.status) || !value.publishedAt && !value.returnSuppressed; }
export function jobLabel(value: WrenJobRecord) { return value.status === "stop_requested" ? "Stopping…" : value.status === "running" ? "Working" : value.status === "queued" ? "Queued" : value.status === "completed" ? "Finished" : value.status === "cancelled" ? "Stopped" : value.status === "failed" ? "Failed" : value.status; }
export function computerLabel(phase: string) { return ({ ready: "Ready", queued: "Queued", planning: "Preparing", provisioning: "Preparing", starting: "Starting", configuring: "Setting up", bootstrapping: "Setting up", deleting: "Removing", deleted: "Removed", failed: "Needs attention" } as Record<string, string>)[phase] ?? phase.replaceAll("_", " "); }
export function fileSize(bytes: number) { return bytes < 1024 ? `${bytes} B` : bytes < 1048576 ? `${(bytes / 1024).toFixed(0)} KB` : `${(bytes / 1048576).toFixed(1)} MB`; }
export function displayMoney(cents: number) { return new Intl.NumberFormat(undefined, { style: "currency", currency: "USD", maximumFractionDigits: 2 }).format(cents / 100); }
export function desktopPoint(bounds: { left: number; top: number; width: number; height: number; naturalWidth: number; naturalHeight: number }, clientX: number, clientY: number) {
  if (![bounds.left,bounds.top,bounds.width,bounds.height,bounds.naturalWidth,bounds.naturalHeight,clientX,clientY].every(Number.isFinite) || bounds.width <= 0 || bounds.height <= 0 || bounds.naturalWidth <= 0 || bounds.naturalHeight <= 0) return null;
  return { x: Math.max(0, Math.min(bounds.naturalWidth - 1, Math.floor((clientX - bounds.left) * bounds.naturalWidth / bounds.width))), y: Math.max(0, Math.min(bounds.naturalHeight - 1, Math.floor((clientY - bounds.top) * bounds.naturalHeight / bounds.height))) };
}

// PostgreSQL timestamps retain sub-millisecond precision. A stale GET and a
// newer control response can otherwise appear equal after Date.parse().
export function compareWorkTimestamps(left: string, right: string) {
  const a = Date.parse(left), b = Date.parse(right);
  if (!Number.isFinite(a) || !Number.isFinite(b)) return 0;
  if (a !== b) return a < b ? -1 : 1;
  const fraction = (value: string) => (value.match(/\.(\d+)(?=Z|[+-]\d{2}:\d{2}$)/i)?.[1] ?? "").padEnd(9, "0");
  const x = fraction(left), y = fraction(right);
  return x < y ? -1 : x > y ? 1 : 0;
}

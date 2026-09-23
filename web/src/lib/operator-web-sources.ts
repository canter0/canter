import type { OperatorEvent } from "./operator-api";

type WebSource = { sourceId: string; url: string; title: string; retrievedAt: string };

// Only source records from successful retrievals, never URLs scraped from
// generated prose or untrusted page text. Search previews are not read pages.
export function operatorWebSources(events: OperatorEvent[]): WebSource[] {
  const sources = new Map<string, WebSource>();
  for (const event of events) {
    if (event.kind !== "tool" || event.data.status !== "completed" || !["canter_open_web", "canter_read_web"].includes(String(event.data.name))) continue;
    const result = event.data.result;
    if (!result || typeof result !== "object" || !("sources" in result) || !Array.isArray(result.sources)) continue;
    for (const source of result.sources) {
      if (!source || typeof source !== "object" || source.kind !== "document" || typeof source.sourceId !== "string" || typeof source.url !== "string" || typeof source.title !== "string" || typeof source.retrievedAt !== "string") continue;
      try {
        const url = new URL(source.url);
        if (!["https:", "http:"].includes(url.protocol) || url.username || url.password) continue;
        sources.set(url.href, { sourceId: source.sourceId, url: url.href, title: source.title || url.hostname, retrievedAt: source.retrievedAt });
      } catch { /* Ignore malformed historical events. */ }
    }
  }
  return [...sources.values()];
}

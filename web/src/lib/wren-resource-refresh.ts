export function workUpdateMatchesPath(path: string, detail: unknown): boolean {
  if (!detail || typeof detail !== "object" || !("workspaceId" in detail) || typeof detail.workspaceId !== "string") return false;
  const encodedWorkspace = /^\/workspaces\/([^/?]+)(?:\/|\?|$)/.exec(path)?.[1];
  if (!encodedWorkspace) return false;
  try { return decodeURIComponent(encodedWorkspace) === detail.workspaceId; } catch { return false; }
}

/** Serialize refreshes, retaining an invalidation that arrives during a request.
 * Superseded responses must not commit; a single follow-up reads current state. */
export function coalescedResourceRefresh(load: (isCurrent: () => boolean) => Promise<void>) {
  let version = 0, running = false, disposed = false;
  async function drain() {
    running = true;
    try {
      let started: number;
      do {
        started = version;
        await load(() => !disposed && version === started);
      } while (!disposed && version !== started);
    } finally { running = false; }
  }
  return {
    request() { if (disposed) return; version++; if (!running) void drain(); },
    dispose() { disposed = true; },
  };
}

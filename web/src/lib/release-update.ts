export const releaseCheckInterval = 120_000;
const minimumCheckInterval = 30_000;

export function releaseCommit(value: unknown): string | null {
  if (!value || typeof value !== "object" || !("commit" in value)) return null;
  return typeof value.commit === "string" && /^[a-f0-9]{40}$/.test(value.commit) ? value.commit : null;
}

export function createReleaseChecker(loadedCommit: string, options: {
  read: (signal: AbortSignal) => Promise<unknown>;
  onChange: (commit: string | null) => void;
  now?: () => number;
}) {
  const now = options.now ?? Date.now;
  let lastCheck = -Infinity;
  let controller: AbortController | null = null;
  let timeout: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  async function check() {
    if (disposed || controller || !releaseCommit({ commit: loadedCommit }) || now() - lastCheck < minimumCheckInterval) return;
    lastCheck = now();
    controller = new AbortController();
    const request = controller;
    timeout = setTimeout(() => request.abort(), 10_000);
    try {
      const commit = releaseCommit(await options.read(request.signal));
      if (!disposed && !request.signal.aborted && commit) options.onChange(commit === loadedCommit ? null : commit);
    } catch { /* Offline or interrupted checks never interrupt the workspace. */ }
    finally { clearTimeout(timeout); controller = null; }
  }
  return {
    check,
    dispose() { disposed = true; clearTimeout(timeout); controller?.abort(); },
  };
}

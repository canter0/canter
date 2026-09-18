export type RepositoryInspection = { repository: string; description: string; defaultBranch: string; language: string; private: boolean; commit: string; parent?: string; files: string[]; truncated: boolean };

// A repository tree is shared by all source tabs at the same immutable commit.
// Each consumer ignores its own stale result; none can abort another tab's read.
export function createRepositoryInspectionCache(fetcher: (path: string) => Promise<RepositoryInspection>, now: () => number = Date.now) {
  const resolved = new Map<string, { value: RepositoryInspection; expires: number }>();
  const pending = new Map<string, Promise<RepositoryInspection>>();
  const pinned = (commit: string) => /^[a-f0-9]{40}$/i.test(commit);
  const keyFor = (workspace: string, repository: string, commit: string) => JSON.stringify([workspace, repository.toLowerCase(), pinned(commit) ? commit.toLowerCase() : commit]);
  return function inspect(workspace: string, repository: string, commit: string, refresh = false): Promise<RepositoryInspection> {
    const key = keyFor(workspace, repository, commit);
    const time = now();
    for (const [cachedKey, item] of resolved) if (item.expires <= time) resolved.delete(cachedKey);
    if (refresh) resolved.delete(key);
    const existing = pending.get(key);
    if (existing) return existing;
    const cached = pinned(commit) ? resolved.get(key) : undefined;
    if (cached) {
      resolved.delete(key); resolved.set(key, cached);
      return Promise.resolve(cached.value);
    }
    const query = new URLSearchParams({ repository, commit });
    const request = Promise.resolve().then(() => fetcher(`/workspaces/${encodeURIComponent(workspace)}/github/inspect?${query}`)).then(value => {
      // Unpinned lookups are only deduplicated while running. Seed their exact
      // resolved commit so opening the first source tab needs no second read.
      if (pinned(value.commit)) {
        const resolvedKey = keyFor(workspace, repository, value.commit);
        resolved.delete(resolvedKey);
        resolved.set(resolvedKey, { value, expires: now() + 5 * 60_000 });
        while (resolved.size > 16) resolved.delete(resolved.keys().next().value!);
      }
      return value;
    }).finally(() => { pending.delete(key); });
    pending.set(key, request);
    return request;
  };
}

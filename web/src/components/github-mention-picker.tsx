"use client";
import { useEffect, useRef, useState, type RefObject } from "react";
import { canterFetch } from "@/lib/canter-api";
import { ProviderIcon } from "./provider-icon";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./github-mention-picker.module.css";

import { githubConnectURL, type GitHubConnection } from "@/lib/github-connection";

type Result = { connection: GitHubConnection; repositories: { full_name: string; description?: string; private: boolean }[]; page: number; hasMore: boolean };
export function GitHubMentionPicker({ workspaceId, query, inputRef, onPick, onClose }: { workspaceId?: string; query: string; inputRef: RefObject<HTMLTextAreaElement | null>; onPick: (repository?: string) => void; onClose: () => void }) {
  const [data, setData] = useState<Result | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [moreLoading, setMoreLoading] = useState(false);
  const panel = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!workspaceId) return;
    const controller = new AbortController();
    canterFetch<Result>(`/workspaces/${encodeURIComponent(workspaceId)}/github/repositories`, { signal: controller.signal }).then(setData).catch(() => { if (!controller.signal.aborted) setError("Could not load repositories."); });
    return () => controller.abort();
  }, [workspaceId, attempt]);
  useEffect(() => {
    const input = inputRef.current;
    const keyboard = (event: KeyboardEvent) => {
      if (event.isComposing) return;
      if (event.key === "Escape") { event.preventDefault(); onClose(); return; }
      if (event.key === "ArrowDown") { event.preventDefault(); panel.current?.querySelector<HTMLElement>("button, a")?.focus(); }
      if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); panel.current?.querySelector<HTMLElement>("button, a")?.click(); }
    };
    const outside = (event: PointerEvent) => { if (event.target instanceof Node && !panel.current?.contains(event.target) && event.target !== input) onClose(); };
    input?.addEventListener("keydown", keyboard); document.addEventListener("pointerdown", outside);
    return () => { input?.removeEventListener("keydown", keyboard); document.removeEventListener("pointerdown", outside); };
  }, [inputRef, onClose]);
  const repositories = data?.repositories.filter(repo => `${repo.full_name} ${repo.description ?? ""}`.toLowerCase().includes(query.toLowerCase())) ?? [];
  async function loadMore() {
    if (!data || moreLoading) return;
    setMoreLoading(true); setError("");
    try {
      const next = await canterFetch<Result>(`/workspaces/${encodeURIComponent(workspaceId!)}/github/repositories?page=${data.page + 1}`);
      setData({ ...next, repositories: next.connection.connected ? [...new Map([...data.repositories, ...next.repositories].map(repo => [repo.full_name, repo])).values()] : [] });
    } catch { setError("Could not load more repositories."); }
    finally { setMoreLoading(false); }
  }
  return <div ref={panel} id="github-mention-picker" className={styles.panel} role="dialog" aria-label="GitHub context" onKeyDown={event => {
    if (event.key === "Escape") { event.preventDefault(); onClose(); inputRef.current?.focus(); }
    if (["ArrowDown", "ArrowUp"].includes(event.key)) {
      event.preventDefault(); const items = Array.from(panel.current?.querySelectorAll<HTMLElement>("button:not(:disabled), a") ?? []);
      const index = items.indexOf(document.activeElement as HTMLElement); items[(index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length]?.focus();
    }
  }}>
    <button type="button" className={styles.heading} onClick={() => onPick()}><ProviderIcon provider="github" /><strong>GitHub</strong><span>Repositories and source code</span><small>↵</small></button>
    {!data && !error ? <div className={styles.skeleton} role="status" aria-label="Loading repositories"><i /><i /><i /></div> : null}
    {data && !data.connection.connected ? <div className={styles.connect}><strong>Connect GitHub</strong><p>Connect your account to find and attach repositories.</p>{data.connection.enabled && workspaceId ? <a href={githubConnectURL(data.connection, workspaceId, typeof window === "undefined" ? "/app" : window.location.pathname)}>Connect GitHub ↗</a> : <p>GitHub access is not configured on this server.</p>}<small>Choose repositories for Canter to read and deploy. GitHub will show the permissions before you connect.</small></div> : null}
    {data?.connection.connected ? <div className={styles.results}>
      {repositories.map(repo => <button type="button" key={repo.full_name} onClick={() => onPick(repo.full_name)}><WorkspaceIcon name="folder" /><span>{repo.full_name}</span><small>{repo.description || (repo.private ? "Private repository" : "Public repository")}</small></button>)}
      {!repositories.length ? <p>No matching repositories{data.hasMore ? " in this page" : ""}.</p> : null}
      {data.connection.installUrl ? <a href={data.connection.installUrl} target="_blank" rel="noopener noreferrer">Select repositories ↗</a> : null}
      <button type="button" onClick={() => setAttempt(value => value + 1)}>Refresh repositories</button>
      {data.hasMore ? <button type="button" disabled={moreLoading} onClick={() => void loadMore()}>{moreLoading ? "Loading…" : "Load more repositories"}</button> : null}
    </div> : null}
    {error ? <div className={styles.connect} role="alert"><p>{error}</p><button type="button" onClick={() => { setError(""); if (data) void loadMore(); else setAttempt(value => value + 1); }}>Try again</button></div> : null}
  </div>;
}

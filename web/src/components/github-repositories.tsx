"use client";

import { useEffect, useState, type FormEvent } from "react";
import { canterFetch } from "@/lib/canter-api";
import { ProviderIcon } from "./provider-icon";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./github-repositories.module.css";

type Connection = { enabled: boolean; connected: boolean; reconnect?: boolean; login?: string };
type Repository = { full_name: string; description: string; private: boolean; default_branch: string };
type Result = { connection: Connection; repositories: Repository[]; page: number; hasMore: boolean };
const connectionErrors: Record<string, string> = {
  access_denied: "GitHub connection was cancelled. You can try again or use a public repository link.",
  session_expired: "Your connection request expired. Please try again.",
  access_restricted: "You no longer have access to this workspace.",
  repository_access_required: "GitHub repository access is needed to list your repositories. Please reconnect and grant access.",
  provider_unavailable: "GitHub connection is not configured on this server.",
  connection_failed: "GitHub could not be connected. Please try again.",
};

export function GitHubRepositories({ workspaceId, conversationId, result, busy, onDeploy, inline = false }: { inline?: boolean; workspaceId: string; conversationId?: string; result?: string; busy: boolean; onDeploy: (repository: string) => Promise<void> }) {
  const [data, setData] = useState<Result | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [retry, setRetry] = useState(0);
  const [filter, setFilter] = useState("");
  const [repository, setRepository] = useState("");
  const base = `/workspaces/${encodeURIComponent(workspaceId)}/github`;
  const next = conversationId ? `/app/conversations/${encodeURIComponent(conversationId)}` : "/app";
  const connectURL = `/api/canter/auth/oauth/github?${new URLSearchParams({ mode: "repository", workspace: workspaceId, next })}`;

  useEffect(() => {
    const controller = new AbortController();
    canterFetch<Result>(`${base}/repositories`, { signal: controller.signal }).then(value => {
      if (!controller.signal.aborted) { setData(value); setError(""); }
    }).catch(cause => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not load repositories."); });
    return () => controller.abort();
  }, [base, retry]);

  async function more() {
    if (!data || loading) return;
    setLoading(true);
    try {
      const value = await canterFetch<Result>(`${base}/repositories?page=${data.page + 1}`);
      setData({ ...value, repositories: value.connection.connected ? [...new Map([...data.repositories, ...value.repositories].map(repo => [repo.full_name, repo])).values()] : [] });
      setError("");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not load more repositories."); }
    finally { setLoading(false); }
  }
  async function disconnect() {
    if (loading || busy) return;
    setLoading(true);
    try {
      const connection = await canterFetch<Connection>(base, { method: "DELETE" });
      setData({ connection, repositories: [], hasMore: false, page: 1 }); setError("");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not disconnect GitHub."); }
    finally { setLoading(false); }
  }
  async function submit(event: FormEvent) {
    event.preventDefault();
    let value = repository.trim().replace(/^@/, "").replace(/^https:\/\/github\.com\//, "").replace(/\/$/, "").replace(/\.git$/, "");
    if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(value) || value.includes("..")) { setError("Enter a GitHub link or owner/repository."); return; }
    value = value.slice(0, 200);
    setError(""); await onDeploy(value);
  }
  const connection = data?.connection;
  const repos = data?.repositories.filter(repo => repo.full_name.toLowerCase().includes(filter.toLowerCase())) ?? [];
  return <section className={styles.panel} data-inline={inline} aria-label="GitHub repositories">
    <div className={styles.icon}><ProviderIcon provider="github" /></div>
    <h2>{connection?.connected ? "Choose a repository" : "Start with your repository"}</h2>
    <p className={styles.description}>{connection?.connected ? `Connected as ${connection.login}. Choose what you’d like to deploy.` : "Connect GitHub and choose the repository you want Canter to deploy."}</p>
    {result && connectionErrors[result] ? <p className={styles.error} role="alert">{connectionErrors[result]}</p> : null}
    {!data && !error ? <p className={styles.muted} role="status">Checking GitHub connection…</p> : null}
    {connection && !connection.connected ? <div className={styles.connect}>
      {connection.enabled ? <a className={styles.primary} href={connectURL}><ProviderIcon provider="github" />{connection.reconnect ? "Reconnect GitHub" : "Connect GitHub"}<WorkspaceIcon name="external" width="15" height="15" /></a> : <p className={styles.error}>GitHub connection is not configured on this server. You can still use a public repository below.</p>}
      {connection.reconnect ? <p className={styles.muted}>Your saved connection needs to be renewed.</p> : null}
      {connection.enabled ? <p className={styles.permission}>GitHub requests repository access, including write permission. Canter uses this connection to read your source for deployment.</p> : null}
    </div> : null}
    {connection?.connected ? <>
      <label className={styles.search}><WorkspaceIcon name="search" width="16" height="16" /><input aria-label="Filter repositories" placeholder="Find a repository…" value={filter} onChange={event => setFilter(event.target.value)} /></label>
      <div className={styles.repositories}>{repos.map(repo => <button disabled={busy || loading} key={repo.full_name} className={styles.repository} onClick={() => void onDeploy(repo.full_name)}><span><strong>{repo.full_name}</strong><small>{repo.private ? "Private" : "Public"} · {repo.default_branch}</small>{repo.description ? <p>{repo.description}</p> : null}</span><WorkspaceIcon name="chevron" width="16" height="16" /></button>)}</div>
      {!repos.length ? <p className={styles.muted}>{filter ? "No matching repositories in this list." : "No repositories are available to this GitHub connection."}</p> : null}
      {data?.hasMore ? <button disabled={loading} className={styles.secondary} onClick={() => void more()}>{loading ? "Loading…" : "Load more repositories"}</button> : null}
      <div className={styles.accountActions}><a href={connectURL}>Reconnect</a><button disabled={loading || busy} onClick={() => void disconnect()}>Disconnect</button></div>
    </> : null}
    {error ? <p className={styles.error} role="alert">{error} <button onClick={() => { setRetry(value => value + 1); setError(""); }}>Try again</button></p> : null}
    <form className={styles.linkForm} onSubmit={event => void submit(event)}><label htmlFor="github-repository">{connection?.connected ? "Or use a repository link" : "Or paste a public repository link"}</label><div><input id="github-repository" placeholder="github.com/owner/repository" autoComplete="off" autoCapitalize="none" spellCheck={false} value={repository} onChange={event => setRepository(event.target.value)} maxLength={240} disabled={busy} /><button type="submit" aria-label="Deploy repository" disabled={busy || !repository.trim()}><WorkspaceIcon name="arrow" width="18" height="18" /></button></div></form>
    {busy ? <p className={styles.muted} role="status">Canter is working. You can choose a repository when it finishes.</p> : null}
  </section>;
}

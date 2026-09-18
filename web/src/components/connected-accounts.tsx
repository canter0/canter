"use client";

import { useEffect, useState } from "react";
import { useWorkspace } from "./workspace-context";
import { canterFetch } from "@/lib/canter-api";
import { authError, type AuthProviders } from "@/lib/auth";

export function ConnectedAccounts() {
  const [error, setError] = useState("");
  const { data: workspace } = useWorkspace();
  const [repositoryConnection, setRepositoryConnection] = useState<{ enabled: boolean; connected: boolean; login?: string } | null>(null);
  useEffect(() => {
    if (!workspace?.workspace.id) return;
    const controller = new AbortController();
    canterFetch<{ enabled: boolean; connected: boolean; login?: string }>(`/workspaces/${encodeURIComponent(workspace.workspace.id)}/github`, { signal: controller.signal }).then(setRepositoryConnection).catch(() => { if (!controller.signal.aborted) setError("Could not load repository connection."); });
    return () => controller.abort();
  }, [workspace?.workspace.id]);
  const [data, setData] = useState<AuthProviders | null>(null);
  useEffect(() => {
    let cancelled = false;
    const callbackError = authError(new URLSearchParams(window.location.search).get("error") ?? "");
    canterFetch<AuthProviders>("/auth/providers").then(result => { if (!cancelled) { setData(result); setError(callbackError); } }).catch(() => { if (!cancelled) setError("Could not load connected accounts."); });
    return () => { cancelled = true; };
  }, []);
  return <div>
    <p className="text-sm text-[var(--muted)]">Sign-in accounts</p>
    {!data && !error ? <p role="status" className="py-4 text-sm text-[var(--muted)]">Loading connections…</p> : null}
    {data?.providers.map(provider => <div key={provider.id} className="flex h-14 items-center justify-between border-b border-[var(--rule)]"><span>{provider.id === "google" ? "Google" : "GitHub"}</span>{provider.connected ? <span className="text-[var(--muted)]">Connected</span> : provider.enabled ? <a href={`/api/canter/auth/oauth/${provider.id}?mode=link&next=%2Fapp%2Faccount%2Fconnections`} className="rule-link">Connect ↗</a> : <span className="text-[var(--muted)]">Unavailable</span>}</div>)}
    <p className="mt-6 text-sm text-[var(--muted)]">Repository access</p>
    <div className="flex min-h-16 items-center justify-between gap-4 border-b border-[var(--rule)] py-3"><div>GitHub OAuth<p className="text-xs text-[var(--muted)]">{repositoryConnection?.connected ? `Connected as ${repositoryConnection.login}` : "Read repositories using your GitHub account."}</p></div>{repositoryConnection?.enabled && workspace ? <a className="rule-link" href={`/api/canter/auth/oauth/github?${new URLSearchParams({ mode: "repository", workspace: workspace.workspace.id, next: "/app/account/connections" })}`}>{repositoryConnection.connected ? "Reconnect ↗" : "Connect ↗"}</a> : <span className="text-xs text-[var(--muted)]">{repositoryConnection ? "Unavailable" : "Loading…"}</span>}</div>
    <div className="flex min-h-16 items-center justify-between gap-4 py-3"><div>GitHub App<p className="text-xs text-[var(--muted)]">Installations are separate from GitHub sign-in.</p></div><span className="text-xs text-[var(--muted)]">Not yet supported</span></div>
    {error ? <p role="alert" className="mt-4 leading-5">{error}</p> : null}
  </div>;
}

"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useWorkspace } from "./workspace-context";
import { canterFetch } from "@/lib/canter-api";
import { githubConnectURL, type GitHubConnection } from "@/lib/github-connection";
import { authError, type AuthProviders } from "@/lib/auth";
import { WrenAppAccounts } from "./wren-app-accounts";
import {useWrenResource,workBase} from "@/lib/wren-work";

function useAuthProviders() {
  const [data, setData] = useState<AuthProviders | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let cancelled = false;
    const callbackError = authError(new URLSearchParams(window.location.search).get("error") ?? "");
    canterFetch<AuthProviders>("/auth/providers").then(result => { if (!cancelled) { setData(result); setError(callbackError); } }).catch(() => { if (!cancelled) setError("Could not load sign-in connections."); });
    return () => { cancelled = true; };
  }, [attempt]);
  return { data, error, retry: () => setAttempt(value => value + 1) };
}

function useRepositoryConnection() {
  const { data: workspace } = useWorkspace();
  const workspaceId = workspace?.workspace.id;
  const resource=useWrenResource<GitHubConnection>(workspaceId?`${workBase(workspaceId)}/github`:"");
  return { workspace, connection:resource.data, error:resource.error?"Could not load repository connection.":"", retry:resource.refresh };
}

function ConnectionError({ error, onRetry }: { error: string; onRetry: () => void }) {
  if (!error) return null;
  return <div role="alert" className="mt-4 rounded-lg border border-[#624040] bg-[#291f1f] p-4 leading-5 text-[#e5b3b3]"><p>{error}</p><button type="button" className="mt-2 min-h-8 underline underline-offset-4" onClick={onRetry}>Retry</button></div>;
}

export function SignInAccounts() {
  const { data, error, retry } = useAuthProviders();
  return <div>
    {!data && !error ? <p role="status" className="py-4 text-sm text-[var(--muted)]">Loading sign-in connections…</p> : null}
    {data?.providers.map(provider => <div key={provider.id} className="flex h-14 items-center justify-between border-b border-[var(--rule)]"><span>{provider.id === "google" ? "Google" : "GitHub"}</span>{provider.connected ? <span className="text-[var(--muted)]">Connected</span> : provider.enabled ? <a href={`/api/canter/auth/oauth/${provider.id}?mode=link&next=%2Fapp%2Faccount%2Fconnections`} className="rule-link">Connect ↗</a> : <span className="text-[var(--muted)]">Unavailable</span>}</div>)}
    <ConnectionError error={error} onRetry={retry} />
  </div>;
}

export function RepositoryAccess() {
  const { workspace, connection, error, retry } = useRepositoryConnection();
  return <div>
    <div className="flex min-h-16 items-center justify-between gap-4 border-b border-[var(--rule)] py-3"><div>{connection?.appEnabled ? "GitHub App" : "GitHub repositories"}<p className="text-xs text-[var(--muted)]">{connection?.connected ? `Connected as ${connection.login}` : "Read repositories using your GitHub account."}</p></div>{connection?.enabled && workspace ? <a className="rule-link" href={githubConnectURL(connection, workspace.workspace.id, "/app/account/connections")}>{connection.connected ? "Reconnect ↗" : "Connect ↗"}</a> : <span className="text-xs text-[var(--muted)]">{connection ? "Unavailable" : "Loading…"}</span>}</div>
    {connection?.installUrl ? <div className="py-3 text-sm"><a className="rule-link" href={connection.installUrl} target="_blank" rel="noopener noreferrer">Select repositories for Canter ↗</a><p className="mt-2 text-xs text-[var(--muted)]">Grant read-only access to selected private repositories. Public repositories are also available.</p></div> : null}
    <ConnectionError error={error} onRetry={retry} />
  </div>;
}

export function AppAccountConnections() {
  const { data: workspace } = useWorkspace();
  const query = useSearchParams();
  const origin = query.get("conversation") ?? "";
  const accountId = workspace?.account.id ?? "";
  const workspaceId = workspace?.workspace.id ?? "";
  if (!workspace) return <p role="status" className="py-4 text-sm text-[var(--muted)]">Loading app connections…</p>;
  return <div>
    {/* Keyed by person and workspace so saved form state never leaks across accounts. */}
    <WrenAppAccounts key={`${accountId}:${workspaceId}`} bare workspaceId={workspaceId} target={null} />
    {origin ? <p className="mt-3"><Link className="rule-link" href={`/app/conversations/${encodeURIComponent(origin)}`}>Return to the conversation you came from</Link></p> : null}
  </div>;
}

export function ConnectedAccounts() {
  return <div>
    <p className="text-sm text-[var(--muted)]">Sign-in accounts</p>
    <SignInAccounts />
    <p className="mt-6 text-sm text-[var(--muted)]">Repository access</p>
    <RepositoryAccess />
  </div>;
}

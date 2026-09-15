"use client";

import { useEffect, useState } from "react";
import { canterFetch } from "@/lib/canter-api";
import { authError, type AuthProviders } from "@/lib/auth";

export function ConnectedAccounts() {
  const [data, setData] = useState<AuthProviders | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let cancelled = false;
    const callbackError = authError(new URLSearchParams(window.location.search).get("error") ?? "");
    canterFetch<AuthProviders>("/auth/providers").then(result => { if (!cancelled) { setData(result); setError(callbackError); } }).catch(() => { if (!cancelled) setError("Could not load connected accounts."); });
    return () => { cancelled = true; };
  }, []);
  return <div>
    {data?.providers.map(provider => <div key={provider.id} className="flex h-14 items-center justify-between border-b border-[var(--rule)]"><span>{provider.id === "google" ? "Google" : "GitHub"}</span>{provider.connected ? <span className="text-[var(--muted)]">Connected</span> : provider.enabled ? <a href={`/api/canter/auth/oauth/${provider.id}?mode=link&next=%2Fapp%2Faccount`} className="rule-link">Connect ↗</a> : <span className="text-[var(--muted)]">Unavailable</span>}</div>)}
    {error ? <p role="alert" className="mt-4 leading-5">{error}</p> : null}
  </div>;
}

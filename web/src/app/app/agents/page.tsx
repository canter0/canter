"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { AppShell, Metric, SectionHeader } from "@/components/app-shell";
import { authorityLabel, canterFetch, relativeTime, type Installation, type Me } from "@/lib/canter-api";

export default function AgentsPage() {
  const [installations, setInstallations] = useState<Installation[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const me = await canterFetch<Me>("/me");
        const workspace = me.workspaces[0];
        if (!workspace) return;
        const result = await canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspace.id)}`);
        if (!cancelled) setInstallations(result.installations);
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load agents.");
      }
    })();
    return () => { cancelled = true; };
  }, []);

  const authorized = installations.filter((item) => !item.revokedAt);
  const active = authorized.filter((item) => item.activeSessions && item.activeSessions > 0);

  return (
    <AppShell active="Agents">
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="grid min-w-[700px] items-end border-b border-[var(--ink)] pb-7 lg:grid-cols-[1fr_160px_160px]">
          <div><div className="meta">Authorized installations</div><h1 className="display mt-3 text-[32px]">Agents</h1></div>
          <div className="mt-8 grid grid-cols-2 lg:contents"><Metric label="Authorized" value={String(authorized.length)} /><Metric label="Active" value={String(active.length)} /></div>
        </div>
        <div className="mt-10 min-w-[700px]">
          <div className="meta grid grid-cols-[2fr_1fr_1.4fr_1fr] border-b border-[var(--rule)] pb-3"><span>Installation</span><span>Authorization</span><span>Authority</span><span>Last seen</span></div>
          {error ? <div className="flex h-20 items-center border-b border-[var(--rule)]">{error}</div> : null}
          {installations.map((installation) => <Link key={installation.id} href={`/app/agents/${installation.id}`} className="grid h-20 grid-cols-[2fr_1fr_1.4fr_1fr] items-center border-b border-[var(--rule)]"><span className="display flex items-center gap-3 text-[18px]"><span className={`signal ${installation.revokedAt ? "opacity-20" : ""}`} />{installation.name}</span><span>{installation.revokedAt ? "revoked" : "authorized"}</span><span>{authorityLabel(installation.authority)}</span><span>{relativeTime(installation.lastSeenAt)} ↗</span></Link>)}
          {!error && installations.length === 0 ? <div className="flex h-28 items-center gap-4 border-b border-[var(--rule)]"><span className="signal opacity-20" /><div><div>No agent installations</div><div className="mt-2 text-[var(--muted)]">A conversation is not a connection. Authorize an installation to give an agent durable access.</div></div></div> : null}
        </div>
        <div className="mt-12 grid min-w-[700px] gap-12 lg:grid-cols-2">
          <div><SectionHeader left="CONNECTION" right="REMOTE MCP" /><div className="flex h-20 items-center justify-between border-b border-[var(--rule)]"><span>Installation authorization</span><span className="text-[var(--muted)]">durable + revocable</span></div></div>
          <div><SectionHeader left="HUMAN BOUNDARY" /><div className="flex h-20 items-center justify-between border-b border-[var(--rule)]"><span>Apply Changes</span><span className="text-[var(--muted)]">approval required</span></div></div>
        </div>
        <div className="mt-auto flex min-h-20 min-w-[700px] items-center justify-between border-t border-[var(--ink)]"><span className="text-[var(--muted)]">To connect another agent, ask it to open Canter.</span><Link href="/onboarding/agent" className="rule-link">Connection guide ↗</Link></div>
      </section>
    </AppShell>
  );
}

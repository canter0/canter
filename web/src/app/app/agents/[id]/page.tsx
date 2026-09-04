"use client";

import { useParams, useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { AppShell, SectionHeader } from "@/components/app-shell";
import { authorityLabel, canterFetch, relativeTime, type Installation, type Me } from "@/lib/canter-api";

export default function AgentInstallationPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const [installation, setInstallation] = useState<Installation | null>(null);
  const [workspaceId, setWorkspaceId] = useState("");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const me = await canterFetch<Me>("/me");
        const workspace = me.workspaces[0];
        if (!workspace) throw new Error("No workspace is available.");
        const result = await canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspace.id)}`);
        const found = result.installations.find((item) => item.id === id);
        if (!found) throw new Error("Agent installation not found.");
        if (!cancelled) {
          setInstallation(found);
          setWorkspaceId(workspace.id);
        }
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load this installation.");
      }
    })();
    return () => { cancelled = true; };
  }, [id]);

  async function revoke() {
    setPending(true);
    setError("");
    try {
      if (!workspaceId) throw new Error("The installation workspace is unavailable.");
      await canterFetch(`/installations/${encodeURIComponent(id)}?workspaceId=${encodeURIComponent(workspaceId)}`, { method: "DELETE" });
      router.push("/app/agents");
      router.refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not revoke this installation.");
      setPending(false);
    }
  }

  const authority = installation ? [
    ["inspect systems", installation.authority.inspect ? "allowed" : "not allowed"],
    ["draft Changes", installation.authority.draft ? "allowed" : "not allowed"],
    ["apply Changes", installation.authority.applyMode],
    ["read provider credentials", "never"],
  ] : [];
  const connection = installation ? [
    ["transport", "remote MCP / API"],
    ["installed", relativeTime(installation.createdAt)],
    ["last seen", relativeTime(installation.lastSeenAt)],
    ["active sessions", String(installation.activeSessions ?? 0)],
    ["authorization", installation.revokedAt ? "revoked" : "authorized"],
  ] : [];

  return (
    <AppShell active="Agents" context={`canter / default / ${installation?.name ?? "agent"}`}>
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="grid min-w-[720px] grid-cols-[1fr_220px] items-end border-b border-[var(--ink)] pb-7"><div><div className="meta">Agent installation</div><div className="flex items-center gap-4"><h1 className="display mt-3 text-[32px]">{installation?.name ?? "Loading"}</h1><span className="mt-3 flex items-center gap-2 text-[10px]"><span className={`signal ${installation?.revokedAt ? "opacity-20" : ""}`} />{installation?.revokedAt ? "REVOKED" : "AUTHORIZED"}</span></div></div><div className="pb-1 text-right text-[var(--muted)]">{installation ? `principal ${installation.id}` : null}</div></div>
        {error ? <div className="mt-8 border-l-2 border-[var(--ink)] pl-4">{error}</div> : null}
        <div className="mt-10 grid min-w-[720px] gap-12 lg:grid-cols-[1.25fr_1fr]">
          <div><SectionHeader left="AUTHORITY" right={authorityLabel(installation?.authority ?? { inspect: false, draft: false, applyMode: "never" })} />{authority.map(([key, value]) => <div key={key} className="flex h-12 items-center justify-between border-b border-[var(--rule)]"><span>{key}</span><span className="text-[var(--muted)]">{value}</span></div>)}</div>
          <div><SectionHeader left="CONNECTION" />{connection.map(([key, value]) => <div key={key} className="flex h-12 items-center justify-between border-b border-[var(--rule)]"><span>{key}</span><span className="text-[var(--muted)]">{value}</span></div>)}</div>
        </div>
        <div className="mt-12 min-w-[720px]"><SectionHeader left="CONTINUITY" right="CANTER IS THE RECORD" /><div className="border-b border-[var(--rule)] py-6 leading-5 text-[var(--muted)]">Sessions may end. This installation retains authority until revoked, while Systems, Changes, executions, and evidence remain durable in Canter.</div></div>
        <div className="mt-auto flex min-h-20 min-w-[720px] items-center justify-end border-t border-[var(--ink)]"><button onClick={() => void revoke()} disabled={pending || !installation || !workspaceId || Boolean(installation.revokedAt)} className="flex h-10 w-[154px] items-center justify-center border border-[var(--ink)] disabled:opacity-40">{pending ? "Revoking…" : "Revoke access"}</button></div>
      </section>
    </AppShell>
  );
}

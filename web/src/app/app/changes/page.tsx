"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { AppShell, Metric } from "@/components/app-shell";
import { CopyInstruction } from "@/components/copy-instruction";
import { canterFetch, type ChangeSummary, type InitialDeploymentSummary, type Me } from "@/lib/canter-api";

export default function ChangesPage() {
  const [changes, setChanges] = useState<ChangeSummary[]>([]);
  const [initialDeployments, setInitialDeployments] = useState<InitialDeploymentSummary[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const me = await canterFetch<Me>("/me");
        const workspace = me.workspaces[0];
        if (!workspace) throw new Error("No workspace is available.");
        const [changeResult, initialDeploymentResult] = await Promise.all([
          canterFetch<{ changes: ChangeSummary[] }>(`/workspaces/${workspace.id}/changes`),
          canterFetch<{ initialDeployments: InitialDeploymentSummary[] }>(`/workspaces/${workspace.id}/initial-deployments`),
        ]);
        if (!cancelled) {
          setChanges(changeResult.changes ?? []);
          setInitialDeployments(initialDeploymentResult.initialDeployments ?? []);
        }
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load Changes.");
      }
    })();
    return () => { cancelled = true; };
  }, []);

  const pending = changes.filter((change) => ["drafted", "authorized"].includes(change.phase)).length + initialDeployments.filter((deployment) => ["drafted", "authorized"].includes(deployment.phase)).length;
  const running = changes.filter((change) => ["applying", "verifying", "compensating"].includes(change.phase)).length + initialDeployments.filter((deployment) => ["queued", "running"].includes(deployment.phase)).length;
  const verified = changes.filter((change) => change.phase === "committed").length + initialDeployments.filter((deployment) => deployment.phase === "succeeded").length;

  return (
    <AppShell active="Changes">
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="grid min-w-[740px] items-end border-b border-[var(--ink)] pb-7 lg:grid-cols-[1fr_160px_160px_160px]">
          <div><div className="meta">Governed execution</div><h1 className="display mt-3 text-[32px]">Changes</h1></div>
          <div className="mt-8 grid grid-cols-3 lg:contents"><Metric label="Pending" value={String(pending)} /><Metric label="Running" value={String(running)} /><Metric label="Verified" value={String(verified)} /></div>
        </div>
        <div className="mt-10 min-w-[740px]">
          <div className="meta grid grid-cols-[110px_2.4fr_1fr_1fr_1fr] border-b border-[var(--rule)] pb-3"><span>Change</span><span>Intent</span><span>State</span><span>System</span><span>Digest</span></div>
          {initialDeployments.map((deployment) => <Link key={deployment.id} href={`/app/changes/initial/${encodeURIComponent(deployment.id)}`} className="grid h-[74px] grid-cols-[110px_2.4fr_1fr_1fr_1fr] items-center border-b border-[var(--rule)]"><span className="flex items-center gap-3">{["drafted", "authorized"].includes(deployment.phase) ? <span className="signal" /> : null}{deployment.id.replace(/^dep_/, "")}</span><span><span className="meta mr-3 text-[9px]">INITIAL</span>{deployment.summary}</span><span>{deployment.phase}</span><span>{deployment.system}</span><span>{deployment.digest.slice(0, 10)} ↗</span></Link>)}
          {changes.map((change) => <Link key={change.id} href={`/app/changes/${encodeURIComponent(change.id)}?system=${encodeURIComponent(change.system)}`} className="grid h-[74px] grid-cols-[110px_2.4fr_1fr_1fr_1fr] items-center border-b border-[var(--rule)]"><span className="flex items-center gap-3">{["drafted", "authorized"].includes(change.phase) ? <span className="signal" /> : null}{change.id.replace(/^change-/, "")}</span><span>{change.summary}</span><span>{change.phase}</span><span>{change.system}</span><span>{change.digest.slice(0, 10)} ↗</span></Link>)}
          {error ? <div className="flex h-20 items-center border-b border-[var(--rule)]">{error}</div> : null}
          {!error && changes.length === 0 && initialDeployments.length === 0 ? <div className="flex h-28 items-center gap-4 border-b border-[var(--rule)]"><span className="signal opacity-20" /><div><div>No Changes</div><div className="mt-2 text-[var(--muted)]">Drafting is durable and does not mutate production.</div></div></div> : null}
        </div>
        <div className="mt-auto grid min-h-20 min-w-[740px] grid-cols-[190px_1fr_70px] items-center border-t border-[var(--ink)]"><span className="meta">Agent instruction</span><span>Show me what changed since yesterday.</span><CopyInstruction text="Show me what changed since yesterday." /></div>
      </section>
    </AppShell>
  );
}

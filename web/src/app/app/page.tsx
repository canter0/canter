"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { AppShell, Metric, SectionHeader } from "@/components/app-shell";
import { CopyInstruction } from "@/components/copy-instruction";
import { authorityLabel, canterFetch, type ChangeSummary, type InitialDeploymentSummary, type Installation, type Me, type SystemRecord } from "@/lib/canter-api";

export default function Dashboard() {
  const [workspaceName, setWorkspaceName] = useState("Default workspace");
  const [systems, setSystems] = useState<SystemRecord[]>([]);
  const [changes, setChanges] = useState<ChangeSummary[]>([]);
  const [initialDeployments, setInitialDeployments] = useState<InitialDeploymentSummary[]>([]);
  const [installation, setInstallation] = useState<Installation | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const me = await canterFetch<Me>("/me");
        const workspace = me.workspaces[0];
        if (!workspace) throw new Error("No workspace is available.");
        const [systemResult, changeResult, initialDeploymentResult, installationResult] = await Promise.all([
          canterFetch<{ systems: SystemRecord[] }>(`/workspaces/${workspace.id}/systems`),
          canterFetch<{ changes: ChangeSummary[] }>(`/workspaces/${workspace.id}/changes`),
          canterFetch<{ initialDeployments: InitialDeploymentSummary[] }>(`/workspaces/${workspace.id}/initial-deployments`),
          canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspace.id)}`),
        ]);
        if (cancelled) return;
        setWorkspaceName(workspace.name);
        setSystems(systemResult.systems);
        setChanges(changeResult.changes);
        setInitialDeployments(initialDeploymentResult.initialDeployments);
        setInstallation(installationResult.installations.find((item) => !item.revokedAt) ?? null);
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load the workspace.");
      }
    })();
    return () => { cancelled = true; };
  }, []);

  const pendingChanges = changes.filter((change) => !["committed", "rejected", "reverted", "escalated"].includes(change.phase));
  const pendingInitialDeployments = initialDeployments.filter((deployment) => !["succeeded", "failed"].includes(deployment.phase));
  const pendingCount = pendingChanges.length + pendingInitialDeployments.length;
  const nextInitialDeployment = pendingInitialDeployments[0];
  const nextChange = pendingChanges[0];

  return (
    <AppShell active="System">
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="grid min-w-[690px] items-end border-b border-[var(--ink)] pb-7 lg:grid-cols-[1fr_160px_160px_160px]">
          <div><div className="meta">System overview</div><h1 className="display mt-3 text-[32px] tracking-[-0.025em]">{workspaceName}</h1></div>
          <div className="mt-8 grid grid-cols-3 lg:contents"><Metric label="Systems" value={String(systems.length)} /><Metric label="Pending" value={String(pendingCount)} /><Metric label="Incidents" value="0" /></div>
        </div>
        <div className="mt-10 min-w-[690px]">
          <div className="meta grid grid-cols-[2.1fr_1fr_1fr_1fr_.8fr] border-b border-[var(--rule)] pb-3"><span>System</span><span>State</span><span>Shape</span><span>Location</span><span>Revision</span></div>
          {systems.map((system) => { const host = system.contract.spec.constraints?.host; const name = system.contract.metadata.name; return <Link key={name} href={`/app/system/${encodeURIComponent(name)}`} className="grid h-[74px] grid-cols-[2.1fr_1fr_1fr_1fr_.8fr] items-center border-b border-[var(--rule)]"><span className="flex items-center gap-3"><span className="signal" />{name}</span><span>declared</span><span>{host ? `${host.class ?? "compute"} × ${host.count ?? 1}` : "managed"}</span><span>default</span><span>{system.revision} ↗</span></Link>; })}
          {error ? <div className="flex h-28 items-center border-b border-[var(--rule)]">{error}</div> : null}
          {!error && systems.length === 0 ? <div className="flex h-36 items-center gap-4 border-b border-[var(--rule)]"><span className="signal" /><div><div>No systems</div><div className="display mt-2 text-[15px] text-[var(--muted)]">Ask an authorized agent to prepare the first deployment.</div></div></div> : null}
        </div>
        <div className="mt-12 grid min-w-[690px] gap-12 lg:grid-cols-2">
          <div>
            <SectionHeader left="CHANGES" right={`${pendingCount} PENDING`} />
            <div className="flex h-20 items-center justify-between gap-8 border-b border-[var(--rule)]">
              <span>{nextInitialDeployment?.summary ?? nextChange?.summary ?? "No proposal awaiting approval"}</span>
              <Link href={nextInitialDeployment ? `/app/changes/initial/${encodeURIComponent(nextInitialDeployment.id)}` : nextChange ? `/app/changes/${encodeURIComponent(nextChange.id)}?system=${encodeURIComponent(nextChange.system)}` : "/app/changes"} className="shrink-0 text-[var(--muted)]">{nextInitialDeployment || nextChange ? "Review ↗" : "View history ↗"}</Link>
            </div>
          </div>
          <div><SectionHeader left="AGENT AUTHORITY" right={installation?.name.toUpperCase() ?? "NONE"} /><div className="flex h-12 items-center justify-between border-b border-[var(--rule)]"><span>{installation ? authorityLabel(installation.authority) : "no installation"}</span><span className="text-[var(--muted)]">{installation ? "allowed" : "authorize agent"}</span></div><div className="flex h-12 items-center justify-between border-b border-[var(--rule)]"><span>apply</span><span className="text-[var(--muted)]">{installation?.authority.applyMode ?? "not granted"}</span></div></div>
        </div>
        <div className="mt-auto grid min-h-20 min-w-[690px] grid-cols-[190px_1fr_70px] items-center border-t border-[var(--ink)]"><span className="meta">Suggested instruction</span><span>Run this repository on Canter.</span><CopyInstruction text="Run this repository on Canter." /></div>
      </section>
    </AppShell>
  );
}

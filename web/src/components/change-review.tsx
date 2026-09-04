"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { AppShell, SectionHeader } from "@/components/app-shell";
import { canterFetch, relativeTime, type ChangeDetail, type Me } from "@/lib/canter-api";

export function ChangeReview({ id, system }: { id: string; system: string }) {
  const [workspaceId, setWorkspaceId] = useState("");
  const [change, setChange] = useState<ChangeDetail | null>(null);
  const [execution, setExecution] = useState<Record<string, unknown> | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");

  async function load(cancelled?: () => boolean) {
    const me = await canterFetch<Me>("/me");
    const workspace = me.workspaces[0];
    if (!workspace) throw new Error("No workspace is available.");
    const result = await canterFetch<ChangeDetail>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(id)}`);
    if (cancelled?.()) return;
    setWorkspaceId(workspace.id);
    setChange(result);
  }

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const me = await canterFetch<Me>("/me");
        const workspace = me.workspaces[0];
        if (!workspace) throw new Error("No workspace is available.");
        const result = await canterFetch<ChangeDetail>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(id)}`);
        if (cancelled) return;
        setWorkspaceId(workspace.id);
        setChange(result);
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load this Change.");
      }
    })();
    return () => { cancelled = true; };
  }, [id, system]);

  async function authorize() {
    if (!change || !workspaceId) return;
    setPending(true);
    setError("");
    try {
      const result = await canterFetch<ChangeDetail>(`/workspaces/${workspaceId}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(id)}/authorize`, { method: "POST", body: JSON.stringify({ digest: change.digest }) });
      setChange(result);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not authorize this Change.");
    } finally {
      setPending(false);
    }
  }

  async function apply() {
    if (!change || !workspaceId) return;
    setPending(true);
    setError("");
    try {
      const result = await canterFetch<Record<string, unknown>>(`/workspaces/${workspaceId}/systems/${encodeURIComponent(system)}/changes/${encodeURIComponent(id)}/apply`, { method: "POST" });
      setExecution(result);
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not start this execution.");
    } finally {
      setPending(false);
    }
  }

  const impact = change?.plan?.impact;
  const assessment = change ? [
    ["digest", change.digest],
    ["base release", change.plan?.baseVersion ?? "none"],
	...(change.plan?.scale ? [["replica transition", `${change.plan.scale.service}: ${change.plan.scale.fromReplicas} → ${change.plan.scale.toReplicas}`]] : []),
	...(change.plan?.scale?.restoreAt ? [["automatic restore", `${change.plan.scale.restoreToReplicas} replicas at ${new Date(change.plan.scale.restoreAt).toLocaleString()}`]] : []),
    ["availability", impact?.availability ?? "not assessed"],
    ["data", impact?.data ?? "not assessed"],
    ["monthly cost", impact?.monthlyCostDeltaCents == null ? "not assessed" : `${impact.monthlyCostDeltaCents >= 0 ? "+" : "−"}$${Math.abs(impact.monthlyCostDeltaCents / 100).toFixed(2)}`],
  ] : [];

  return (
    <AppShell active="Changes" context={`canter / default / ${id}`}>
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="grid min-w-[740px] grid-cols-[1fr_210px] items-end border-b border-[var(--ink)] pb-7"><div><div className="meta">{id} · drafted by {change?.draftedBy?.displayName ?? change?.draftedBy?.id ?? "agent"}</div><h1 className="display mt-3 text-[32px]">{change?.summary ?? "Loading Change"}</h1></div><div className="flex items-center justify-end gap-2 pb-1 text-[10px]"><span className="signal" />{change?.phase.toUpperCase() ?? "LOADING"}</div></div>
        {error ? <div className="mt-8 border-l-2 border-[var(--ink)] pl-4">{error}</div> : null}
        <div className="mt-10 grid min-w-[740px] gap-12 lg:grid-cols-[1.4fr_1fr]">
          <div><SectionHeader left="EXECUTION PLAN" right={`${change?.operations?.length ?? 0} STEPS`} />{change?.operations?.map((operation, index) => <div key={operation.id} className="grid min-h-14 grid-cols-[56px_1fr_120px] items-center border-b border-[var(--rule)] py-3"><span className="text-[var(--muted)]">{String(index + 1).padStart(2, "0")}</span><div><div>{operation.description}</div><div className="mt-1 text-[var(--muted)]">{operation.kind}</div></div><span className="text-right text-[var(--muted)]">{operation.phase}</span></div>)}</div>
          <div><SectionHeader left="EXACT ASSESSMENT" />{assessment.map(([key, value]) => <div key={key} className="flex min-h-11 items-center justify-between gap-8 border-b border-[var(--rule)] py-3"><span>{key}</span><span className={`max-w-[240px] break-all text-right text-[var(--muted)] ${key === "digest" ? "text-[9px]" : ""}`}>{value}</span></div>)}</div>
        </div>
        <div className="mt-12 min-w-[740px]"><SectionHeader left="EVIDENCE" right={`${change?.evidence?.length ?? 0} RECORDS`} />{change?.evidence?.map((record) => <div key={`${record.operationId}-${record.observedAt}`} className="grid min-h-12 grid-cols-[130px_1fr_140px] items-center border-b border-[var(--rule)] py-3"><span className="text-[var(--muted)]">{record.operationId}</span><span>{record.statement}</span><span className="text-right text-[var(--muted)]">{relativeTime(record.observedAt)}</span></div>)}{change && !change.evidence?.length ? <div className="flex h-16 items-center border-b border-[var(--rule)] text-[var(--muted)]">No execution evidence yet.</div> : null}</div>
        {execution ? <div className="mt-8 border-l-2 border-[var(--signal)] pl-4">Execution accepted: {String(execution.id ?? execution.executionId ?? "durable record created")}</div> : null}
        <div className="mt-auto flex min-h-20 min-w-[740px] items-center justify-between gap-3 border-t border-[var(--ink)]">
          {change ? <Link href={`/app/system/${encodeURIComponent(system)}/policies?from=${encodeURIComponent(change.id)}`} className="rule-link">Create standing policy from this envelope ↗</Link> : <span />}
          <div className="flex gap-3">{change?.phase === "drafted" ? <button onClick={() => void authorize()} disabled={pending} className="flex h-10 min-w-[176px] items-center justify-center bg-[var(--ink)] px-6 text-[var(--paper)] disabled:opacity-50">{pending ? "Authorizing…" : "Authorize exact digest"}</button> : null}
          {change?.phase === "authorized" ? <button onClick={() => void apply()} disabled={pending} className="flex h-10 min-w-[176px] items-center justify-center bg-[var(--ink)] px-6 text-[var(--paper)] disabled:opacity-50">{pending ? "Starting…" : "Apply approved Change"}</button> : null}</div>
        </div>
      </section>
    </AppShell>
  );
}

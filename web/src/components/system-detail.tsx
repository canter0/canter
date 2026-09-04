"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { AppShell, Metric, SectionHeader } from "@/components/app-shell";
import { canterFetch, type Me, type SystemView } from "@/lib/canter-api";

export function SystemDetail({ name }: { name: string }) {
  const [view, setView] = useState<SystemView | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const me = await canterFetch<Me>("/me");
        const workspace = me.workspaces[0];
        if (!workspace) throw new Error("No workspace is available.");
        const result = await canterFetch<SystemView>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(name)}`);
        if (!cancelled) setView(result);
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not inspect this System.");
      }
    })();
    return () => { cancelled = true; };
  }, [name]);

  const contract = view?.contract;
  const host = contract?.spec.constraints?.host;
  const services = contract?.spec.services ?? [];
  const phase = view?.host?.phase ?? (view ? "declared" : "loading");
	const capacity = view?.applicationCapacity;

  return (
    <AppShell active="System" context={`canter / default / ${name}`}>
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="grid min-w-[740px] items-end border-b border-[var(--ink)] pb-7 lg:grid-cols-[1fr_170px_170px_170px]">
          <div><div className="meta">System / {name}</div><div className="flex items-center gap-4"><h1 className="display mt-3 text-[32px]">{name}</h1><span className="mt-3 flex items-center gap-2 text-[10px]"><span className={`signal ${error ? "opacity-20" : ""}`} />{phase.toUpperCase()}</span></div></div>
		  <div className="mt-8 grid grid-cols-3 lg:contents"><Metric label="Ready replicas" value={String(capacity?.readyReplicas ?? 0)} /><Metric label="Maximum" value={String(capacity?.maximumReplicas ?? 0)} /><Metric label="Issues" value={String(view?.issues?.length ?? 0)} /></div>
        </div>
        {error ? <div className="mt-8 border-l-2 border-[var(--ink)] pl-4">{error}</div> : null}
        <div className="mt-10 grid min-w-[740px] gap-12 lg:grid-cols-[2fr_1fr]">
          <div><SectionHeader left="LOGICAL SERVICES" right={`${services.length} DECLARED`} /><div className="meta grid grid-cols-[1.5fr_1fr_1fr_1fr] border-b border-[var(--rule)] py-3"><span>Service</span><span>Kind</span><span>Isolation</span><span>Instances</span></div>{services.map((service) => <div key={service.name} className="grid h-[62px] grid-cols-[1.5fr_1fr_1fr_1fr] items-center border-b border-[var(--rule)]"><span className="flex items-center gap-3"><span className="signal" />{service.name}</span><span>{service.kind}</span><span>{service.isolation}</span><span>{service.instances ?? 1}</span></div>)}</div>
          <div><SectionHeader left="CONTRACT" />{[["intent", contract?.spec.intent ?? "—"], ["host class", host?.class ?? "—"], ["host count", String(host?.count ?? "—")], ["host memory", host?.memoryMiB ? `${host.memoryMiB} MiB` : "—"], ["m1", contract?.spec.m1?.prefix ?? "—"]].map(([key, value]) => <div key={key} className="flex min-h-12 items-center justify-between gap-6 border-b border-[var(--rule)] py-3"><span>{key}</span><span className="max-w-[230px] text-right text-[var(--muted)]">{value}</span></div>)}</div>
        </div>
        <div className="mt-12 min-w-[740px]"><SectionHeader left="CAPABILITY BINDINGS" right={`${view?.bindings.length ?? 0}`} />{view?.bindings.map((binding) => <div key={binding.service} className="grid min-h-12 grid-cols-[150px_1fr_1fr] items-center border-b border-[var(--rule)] py-3"><span>{binding.service}</span><span>{binding.environment}</span><span className="text-[var(--muted)]">consumed by {binding.consumers.join(", ") || "none"}</span></div>)}{view && view.bindings.length === 0 ? <div className="flex h-16 items-center border-b border-[var(--rule)] text-[var(--muted)]">No private service bindings.</div> : null}</div>
		{capacity ? <div className="mt-12 min-w-[740px]"><SectionHeader left="APPLICATION CAPACITY" right={capacity.mode.toUpperCase()} />{[["service", capacity.service], ["declared baseline", String(capacity.declaredBaseline)], ["desired replicas", String(capacity.desiredReplicas)], ["ready replicas", String(capacity.readyReplicas)], ["maximum on current hosts", String(capacity.maximumReplicas)]].map(([key, value]) => <div key={key} className="flex min-h-12 items-center justify-between border-b border-[var(--rule)]"><span>{key}</span><span className="text-[var(--muted)]">{value}</span></div>)}</div> : null}
        {view?.issues?.length ? <div className="mt-12 min-w-[740px]"><SectionHeader left="OBSERVATION ISSUES" right={String(view.issues.length)} />{view.issues.map((issue) => <div key={issue} className="border-b border-[var(--rule)] py-4">{issue}</div>)}</div> : null}
        <div className="mt-auto flex min-h-20 min-w-[740px] items-center justify-end border-t border-[var(--ink)]"><Link href={`/app/system/${encodeURIComponent(name)}/policies`} className="rule-link">Standing policies ↗</Link></div>
      </section>
    </AppShell>
  );
}

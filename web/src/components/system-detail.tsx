"use client";
import {getSelectedWorkspace} from "@/lib/workspace-selection";

import { useEffect, useState } from "react";
import { useSurfaceWorkspace } from "./embedded-app-surface";
import Link from "next/link";
import { AppShell } from "@/components/app-shell";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./detail-page.module.css";
import { phaseTone } from "@/lib/workspace-overview";
import { canterFetch, type SystemView } from "@/lib/canter-api";

export function SystemDetail({ name }: { name: string }) {
  const scopedWorkspace = useSurfaceWorkspace();
  const [view, setView] = useState<SystemView | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const workspace = scopedWorkspace ? { id: scopedWorkspace } : (await getSelectedWorkspace()).workspace;
        if (!workspace) throw new Error("No workspace is available.");
        const result = await canterFetch<SystemView>(`/workspaces/${workspace.id}/systems/${encodeURIComponent(name)}`);
        if (!cancelled) { setView(result); setError(""); }
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not inspect this System.");
      }
    })();
    return () => { cancelled = true; };
  }, [name, scopedWorkspace, attempt]);

  const contract = view?.contract;
  const host = contract?.spec.constraints?.host;
  const services = contract?.spec.services ?? [];
  const phase = view?.host?.phase ?? (view ? "declared" : "loading");
	const capacity = view?.applicationCapacity;

  const tone = error ? "bad" : phaseTone(phase === "running" ? "ready" : phase);
  const contractRows: Array<[string, string]> = [["Intent", contract?.spec.intent ?? "—"], ["Host class", host?.class ?? "—"], ["Host count", String(host?.count ?? "—")], ["Host memory", host?.memoryMiB ? `${host.memoryMiB} MiB` : "—"], ["M1 prefix", contract?.spec.m1?.prefix ?? "—"]];

  return (
    <AppShell active="System" context={`canter / default / ${name}`} pageTitle={name}>
      <section className={styles.page}><div className={styles.inner}>
        <header className={styles.header}>
          <div><div className={styles.eyebrow}>App</div><h1>{name}</h1></div>
          <span className={styles.status} data-tone={tone}><span />{phase.charAt(0).toUpperCase() + phase.slice(1)}</span>
        </header>
        {error ? <div role="alert" className={styles.error}>{error} <button onClick={() => setAttempt(value => value + 1)}>Try again</button></div> : null}
        <div className={styles.metrics}>
          <div><span>Ready replicas</span><strong>{capacity?.readyReplicas ?? "—"}</strong></div>
          <div><span>Maximum</span><strong>{capacity?.maximumReplicas ?? "—"}</strong></div>
          <div><span>Issues</span><strong>{view?.issues?.length ?? 0}</strong></div>
        </div>
        <div className={styles.grid}>
          <section className={styles.section}><h2>Services<small>{services.length}</small></h2>
            {services.length ? <div role="region" aria-label="Services" tabIndex={0} className={styles.scroll}><table className={styles.table}><thead><tr>{["Service", "Kind", "Isolation", "Instances"].map(label => <th scope="col" key={label}>{label}</th>)}</tr></thead><tbody>{services.map(service => <tr key={service.name}>{[service.name, service.kind ?? "—", service.isolation ?? "—", service.instances ?? 1].map((value, index) => <td key={index}>{value}</td>)}</tr>)}</tbody></table></div> : <p className={styles.empty}>{view ? "No services declared." : "Loading…"}</p>}
          </section>
          <section className={styles.section}><h2>Contract</h2>
            <dl className={styles.rows}>{contractRows.map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{value}</dd></div>)}</dl>
          </section>
        </div>
        <div className={styles.stack}>
          <section className={styles.section}><h2>Private bindings<small>{view?.bindings.length ?? 0}</small></h2>
            {view?.bindings.length ? <ul className={styles.list}>{view.bindings.map((binding) => <li key={binding.service}><div>{binding.service}<small>{binding.environment}</small></div><small>Used by {binding.consumers.join(", ") || "no services"}</small></li>)}</ul> : <p className={styles.empty}>No private service bindings.</p>}
          </section>
          {capacity ? <section className={styles.section}><h2>Capacity<small>{capacity.mode}</small></h2>
            <dl className={styles.rows}>{[["Service", capacity.service], ["Declared baseline", String(capacity.declaredBaseline)], ["Desired replicas", String(capacity.desiredReplicas)], ["Ready replicas", String(capacity.readyReplicas)], ["Maximum on current hosts", String(capacity.maximumReplicas)]].map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{value}</dd></div>)}</dl>
          </section> : null}
          {view?.issues?.length ? <section className={styles.section}><h2>Issues<small>{view.issues.length}</small></h2><ul className={styles.list}>{view.issues.map((issue) => <li key={issue}>{issue}</li>)}</ul></section> : null}
        </div>
        <div className={styles.actions}><Link href={`/app/system/${encodeURIComponent(name)}/policies`} className={styles.link}>Standing policies<WorkspaceIcon name="right" width="14" height="14" /></Link></div>
      </div></section>
    </AppShell>
  );
}
